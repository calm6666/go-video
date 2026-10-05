package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/audit/internal/svc"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateAuditExportLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateAuditExportLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateAuditExportLogic {
	return &CreateAuditExportLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 提交导出任务（request_id 幂等；导出不走同步大查询）
//
// 实现要点（规格 1~5）：
//  1. request_id、reason 必填；本方法只落一行 pending 任务，绝不生成文件，
//     因此它不存在「伪装成功」的空间；
//  2. 查询约束与 ListAuditEntries 同源（同一 checkQueryWindow），
//     format 只接受 csv/json；
//  3. filter_json 落库前逐值扫 PII：按手机号筛审计这个查询本身就是第二条泄露通道；
//  4. 幂等以 uniq_request_id 为事实来源：先回查、再插入，插入撞唯一键时回查回报 reused；
//  5. 任务停在 pending：本服务不内置 worker，由 services/cron 调 RunAuditExportTask 推进。
func (l *CreateAuditExportLogic) CreateAuditExport(in *rpc.CreateAuditExportReq) (*rpc.CreateAuditExportReply, error) {
	if in == nil {
		return nil, model.ErrRequestRequired
	}
	cc := in.GetCtx()
	if err := validateCallContext(cc, true); err != nil {
		return nil, err
	}
	format, err := exportFormat(in.GetFormat())
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(in.GetReason())
	if reason == "" {
		return nil, fmt.Errorf("%w: 批量带走存证必须有可追动机", model.ErrReasonRequired)
	}

	at, err := knownActorType(in.GetActorType())
	if err != nil {
		return nil, err
	}
	f := exportFilterSpec{
		StartAt:      in.GetStartAt(),
		EndAt:        in.GetEndAt(),
		ActorType:    at,
		ActorID:      in.GetActorId(),
		Action:       strings.TrimSpace(in.GetAction()),
		ActionDomain: strings.TrimSpace(in.GetActionDomain()),
		TargetType:   strings.TrimSpace(in.GetTargetType()),
		TargetID:     strings.TrimSpace(in.GetTargetId()),
		Reason:       reason,
	}
	if f.ActorID < 0 {
		return nil, fmt.Errorf("%w: actor_id=%d", model.ErrActorIDInvalid, f.ActorID)
	}
	for _, lim := range []struct {
		field string
		value string
		max   int
	}{
		{"action", f.Action, model.MaxActionBytes},
		{"action_domain", f.ActionDomain, model.MaxActionDomainBytes},
		{"target_type", f.TargetType, model.MaxTargetTypeBytes},
		{"target_id", f.TargetID, model.MaxTargetIDBytes},
	} {
		if err := checkLen(lim.field, lim.value, lim.max); err != nil {
			return nil, err
		}
	}
	if f.ActionDomain != "" && !model.ValidActionDomain(f.ActionDomain) {
		return nil, fmt.Errorf("%w: action_domain=%q", model.ErrActionDomainInvalid, f.ActionDomain)
	}
	if f.Action != "" && !model.ValidAction(f.Action) {
		return nil, fmt.Errorf("%w: action=%q", model.ErrActionInvalid, f.Action)
	}
	if err := scanPIIValues("filter", f.piiFields()...); err != nil {
		l.Errorf("CreateAuditExport 导出条件命中明文敏感形态 caller=%s request_id=%s err=%v",
			cc.GetCallerService(), cc.GetRequestId(), err)
		return nil, err
	}

	d := buildDeps(l.svcCtx)
	// reason 同时进 filter_json 与自审计条目的 reason 列，
	// 因此上限取「列宽与配置取小」的那一个，两处才不会一处过一处不过。
	if err := checkLen("reason", reason, d.reasonLimit()); err != nil {
		return nil, err
	}
	if err := d.checkQueryWindow(f.StartAt, f.EndAt, f.narrowed()); err != nil {
		l.Errorf("CreateAuditExport 查询约束未通过 caller=%s request_id=%s err=%v",
			cc.GetCallerService(), cc.GetRequestId(), err)
		return nil, err
	}
	filterJSON, err := f.encode()
	if err != nil {
		return nil, err
	}
	if err := checkLen("filter_json", filterJSON, model.MaxFilterJSONBytes); err != nil {
		return nil, err
	}

	requestID := strings.TrimSpace(cc.GetRequestId())
	// 先按幂等键回查：重复提交不该因为「Insert 撞唯一键」而变成一次失败。
	existing, err := d.exports.FindByRequestID(l.ctx, requestID)
	if err != nil {
		l.Errorf("CreateAuditExport 回查已有任务失败 request_id=%s err=%v", requestID, err)
		return nil, err
	}
	if existing != nil {
		return &rpc.CreateAuditExportReply{Task: exportTaskView(existing), Reused: true}, nil
	}

	task := &model.ExportTask{
		RequestID:     requestID,
		OperatorID:    cc.GetOperatorId(),
		CallerService: strings.TrimSpace(cc.GetCallerService()),
		FilterJSON:    filterJSON,
		Format:        format,
		State:         model.ExportStatePending,
		TraceID:       strings.TrimSpace(cc.GetTraceId()),
	}
	if _, err := d.exports.Insert(l.ctx, task); err != nil {
		if !errors.Is(err, model.ErrTaskExists) {
			l.Errorf("CreateAuditExport 建任务失败 request_id=%s err=%v", requestID, err)
			return nil, err
		}
		// 并发重复提交：两个请求同时过了回查，唯一键分出胜负，败者按幂等回放返回。
		again, ferr := d.exports.FindByRequestID(l.ctx, requestID)
		if ferr != nil {
			return nil, errors.Join(err, ferr)
		}
		if again == nil {
			return nil, err
		}
		return &rpc.CreateAuditExportReply{Task: exportTaskView(again), Reused: true}, nil
	}

	selfAuditLogged(l.ctx, l.Logger, cc, d, selfAuditSpec{
		Action:     actionExportSubmit,
		Domain:     selfDomainDataAccess,
		TargetType: targetExportTask,
		TargetID:   strconv.FormatInt(task.TaskID, 10),
		Reason:     reason,
		Before:     filterDims(f.entryFilter(0)),
		After: map[string]string{
			"task_id": strconv.FormatInt(task.TaskID, 10),
			"format":  format,
			"state":   task.State,
		},
	})
	return &rpc.CreateAuditExportReply{Task: exportTaskView(task), Reused: false}, nil
}
