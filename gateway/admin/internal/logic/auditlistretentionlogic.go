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

type AuditListRetentionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询保留期策略（state=0 表示全部）
func NewAuditListRetentionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AuditListRetentionLogic {
	return &AuditListRetentionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 保留期策略决定审计何时可归档、何时允许在库外清理，是运营必须能看到的证据；
// 网关原样投影 version（乐观锁）与 operator_id（最后修改人），不做任何推算。
func (l *AuditListRetentionLogic) AuditListRetention(req *types.ParamAuditListRetention) (resp *types.AuditRetentionPoliciesResponse, err error) {
	if l.svcCtx.Audit == nil {
		return nil, errAuditServiceNotConfigured
	}
	callCtx, err := auditCallContext(l.ctx, req.Ctx, false)
	if err != nil {
		return nil, err
	}
	pn, ps := normalizeAuditPage(req.Pn, req.Ps)

	reply, err := l.svcCtx.Audit.ListRetentionPolicies(l.ctx, &auditrpc.ListRetentionPoliciesReq{
		Ctx:   callCtx,
		State: req.State,
		Pn:    pn,
		Ps:    ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/auditListRetention: operator=%d state=%d err=%v",
			callCtx.GetOperatorId(), req.State, err)
		return nil, err
	}
	return &types.AuditRetentionPoliciesResponse{
		Code:    0,
		Message: "ok",
		Data:    types.AuditRetentionPoliciesData{Items: auditRetentionPoliciesToAPI(reply.GetItems()), Total: reply.GetTotal()},
		TTL:     0,
	}, nil
}
