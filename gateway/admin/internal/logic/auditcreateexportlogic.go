// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	auditrpc "go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AuditCreateExportLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 提交审计导出任务（request_id 幂等，导出不走同步大查询）
func NewAuditCreateExportLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AuditCreateExportLogic {
	return &AuditCreateExportLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 申请导出：导出的行集合可能含敏感行为证据，因此 reason（动机）必填，
// request_id 必填以让「重复点击」不产生第二个任务（audit 用唯一索引命中并回 reused）。
// 时间跨度与收窄维度的合法性、format 白名单（csv/json）全部由 audit 判定。
func (l *AuditCreateExportLogic) AuditCreateExport(req *types.ParamAuditCreateExport) (resp *types.AuditExportTaskResponse, err error) {
	if l.svcCtx.Audit == nil {
		return nil, errAuditServiceNotConfigured
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	callCtx, err := auditCallContext(l.ctx, req.Ctx, true)
	if err != nil {
		return nil, err
	}
	if err := auditTimeRange(req.StartAt, req.EndAt); err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.Audit.CreateAuditExport(l.ctx, &auditrpc.CreateAuditExportReq{
		Ctx:          callCtx,
		StartAt:      req.StartAt,
		EndAt:        req.EndAt,
		ActorType:    auditrpc.ActorType(req.ActorType),
		ActorId:      req.ActorId,
		Action:       req.Action,
		ActionDomain: req.ActionDomain,
		TargetType:   req.TargetType,
		TargetId:     req.TargetId,
		Format:       req.Format,
		Reason:       req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/auditCreateExport: operator=%d request_id=%s domain=%s err=%v",
			callCtx.GetOperatorId(), callCtx.GetRequestId(), req.ActionDomain, err)
		return nil, err
	}
	return &types.AuditExportTaskResponse{
		Code:    0,
		Message: "ok",
		Data:    types.AuditExportTaskData{Task: auditExportToAPI(reply.GetTask()), Reused: reply.GetReused()},
		TTL:     0,
	}, nil
}
