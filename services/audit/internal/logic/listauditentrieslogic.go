package logic

import (
	"context"
	"strconv"

	"go-video/services/audit/internal/svc"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListAuditEntriesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListAuditEntriesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListAuditEntriesLogic {
	return &ListAuditEntriesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 强约束分页查询（时间范围 + 收窄维度必填）。
//
//  1. Repository.CheckQueryWindow：时间范围必填、跨度 <= Query.MaxRangeDays、
//     且至少一个收窄维度；
//  2. Repository.ClampPage 归一化 pn/ps；
//  3. model.List 固定 occurred_at DESC, entry_id DESC，不接受调用方指定排序键
//     （千万级只增表上的任意排序会退化成 filesort + 临时表）；
//  4. 回参带 max_range_seconds，让被拒的调用方能自我修正而不是靠读文档；
//  5. 写一条 action=audit.entry.list、action_domain=data_access 的自审计条目，
//     摘要维度是过滤条件本身（原文条件不进库）。
func (l *ListAuditEntriesLogic) ListAuditEntries(in *rpc.ListAuditEntriesReq) (*rpc.ListAuditEntriesReply, error) {
	if in == nil {
		return nil, model.ErrRequestRequired
	}
	cc := in.GetCtx()
	if err := validateCallContext(cc, false); err != nil {
		return nil, err
	}
	d := buildDeps(l.svcCtx)

	pn, ps, err := d.clampPage(in.GetPn(), in.GetPs())
	if err != nil {
		return nil, err
	}
	filter, narrowed, err := listFilterOf(in, pn, ps)
	if err != nil {
		return nil, err
	}
	if err := d.checkQueryWindow(in.GetStartAt(), in.GetEndAt(), narrowed); err != nil {
		// 被拒也要留痕：反复试探查询边界的 behaviour 只有靠这条日志与自审计才能回看。
		// 未通过约束时不查库，因此自审计只记条件、不记命中数。
		l.Errorf("ListAuditEntries 查询约束未通过 caller=%s request_id=%s err=%v",
			cc.GetCallerService(), cc.GetRequestId(), err)
		return nil, err
	}

	rows, total, err := d.entries.List(l.ctx, filter)
	if err != nil {
		l.Errorf("ListAuditEntries 查询失败 caller=%s start_at=%d end_at=%d err=%v",
			cc.GetCallerService(), filter.StartAt, filter.EndAt, err)
		return nil, err
	}
	selfAuditLogged(l.ctx, l.Logger, cc, d, selfAuditSpec{
		Action:     actionEntryList,
		Domain:     selfDomainDataAccess,
		TargetType: targetAuditEntry,
		TargetID:   firstNonEmpty(filter.TargetID, filter.ActionDomain, filter.Action),
		Reason:     "分页查询存证条目",
		Before:     filterDims(filter),
		After:      map[string]string{"total": strconv.FormatInt(total, 10), "returned": strconv.Itoa(len(rows))},
	})
	return &rpc.ListAuditEntriesReply{
		Entries:         entryViews(rows),
		Total:           total,
		Pn:              pn,
		Ps:              ps,
		MaxRangeSeconds: d.maxRangeSeconds(),
	}, nil
}
