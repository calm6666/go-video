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

type AuditListExportsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询导出任务（operator/state/创建时间过滤）
func NewAuditListExportsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AuditListExportsLogic {
	return &AuditListExportsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 导出任务列表：operator_id/state 是过滤条件而不是本次的操作者，
// 操作者仍取 CallContext 里的会话身份，供 audit 记 data_access 自审计。
func (l *AuditListExportsLogic) AuditListExports(req *types.ParamAuditListExports) (resp *types.AuditExportsResponse, err error) {
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

	reply, err := l.svcCtx.Audit.ListAuditExports(l.ctx, &auditrpc.ListAuditExportsReq{
		Ctx:        callCtx,
		OperatorId: req.OperatorId,
		State:      req.State,
		StartAt:    req.StartAt,
		EndAt:      req.EndAt,
		Pn:         pn,
		Ps:         ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/auditListExports: operator=%d target_operator=%d state=%s err=%v",
			callCtx.GetOperatorId(), req.OperatorId, req.State, err)
		return nil, err
	}
	return &types.AuditExportsResponse{
		Code:    0,
		Message: "ok",
		Data:    types.AuditExportsData{Items: auditExportsToAPI(reply.GetItems()), Total: reply.GetTotal()},
		TTL:     0,
	}, nil
}
