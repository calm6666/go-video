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

type AuditListEntriesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页检索审计条目（时间范围 + 至少一个收窄维度，由 audit 强制）
func NewAuditListEntriesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AuditListEntriesLogic {
	return &AuditListEntriesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 检索审计条目：网关只补主体、非负时间戳与分页归一，
// 「必须带收窄维度」与跨度上限由 audit 判定并回传 max_range_seconds（AGENTS.md §5）。
// 每次读取会在 audit 侧留下一条 action_domain=data_access 的自审计，网关不再复写一份。
func (l *AuditListEntriesLogic) AuditListEntries(req *types.ParamAuditListEntries) (resp *types.AuditEntriesResponse, err error) {
	if l.svcCtx.Audit == nil {
		return nil, errAuditServiceNotConfigured
	}
	callCtx, err := auditCallContext(l.ctx, req.Ctx, false)
	if err != nil {
		return nil, err
	}
	if err := auditTimeRange(req.StartAt, req.EndAt); err != nil {
		return nil, err
	}
	pn, ps := normalizeAuditPage(req.Pn, req.Ps)

	reply, err := l.svcCtx.Audit.ListAuditEntries(l.ctx, &auditrpc.ListAuditEntriesReq{
		Ctx:          callCtx,
		StartAt:      req.StartAt,
		EndAt:        req.EndAt,
		ActorType:    auditrpc.ActorType(req.ActorType),
		ActorId:      req.ActorId,
		Action:       req.Action,
		ActionDomain: req.ActionDomain,
		TargetType:   req.TargetType,
		TargetId:     req.TargetId,
		TraceId:      req.TraceId,
		Result:       auditrpc.AuditResult(req.Result),
		SourceApp:    auditrpc.SourceApp(req.SourceApp),
		Pn:           pn,
		Ps:           ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/auditListEntries: operator=%d domain=%s action=%s pn=%d ps=%d err=%v",
			callCtx.GetOperatorId(), req.ActionDomain, req.Action, pn, ps, err)
		return nil, err
	}
	return &types.AuditEntriesResponse{
		Code:    0,
		Message: "ok",
		Data: types.AuditEntriesData{
			Entries:         auditEntriesToAPI(reply.GetEntries()),
			Total:           reply.GetTotal(),
			Pn:              reply.GetPn(),
			Ps:              reply.GetPs(),
			MaxRangeSeconds: reply.GetMaxRangeSeconds(),
		},
		TTL: 0,
	}, nil
}
