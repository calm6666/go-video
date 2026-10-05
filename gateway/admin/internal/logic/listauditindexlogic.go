// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	operationrpc "go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListAuditIndexLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询管理操作审计索引（正文证据在被操作的领域服务）
func NewListAuditIndexLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListAuditIndexLogic {
	return &ListAuditIndexLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 查询审计索引：审计查询本身也必须带可信主体（operation 侧无 operator_id 直接拒绝），
// start_at 含、end_at 不含；这里只搬运索引，完整证据仍在被操作的领域服务。
// 注意 admin_id/resource_id 是查询条件，不是本次请求的操作者。
func (l *ListAuditIndexLogic) ListAuditIndex(req *types.ParamListAuditIndex) (resp *types.OperationAuditsResponse, err error) {
	if l.svcCtx.Operation == nil {
		return nil, errors.New("operation service not configured")
	}
	opCtx, err := operationOpContext(l.ctx, req.Op, false)
	if err != nil {
		return nil, err
	}
	if req.StartAt < 0 || req.EndAt < 0 {
		return nil, errors.New("gateway/admin: start_at/end_at must be >= 0")
	}
	if req.StartAt > 0 && req.EndAt > 0 && req.StartAt >= req.EndAt {
		return nil, errors.New("gateway/admin: start_at must be earlier than end_at")
	}
	pn, ps := normalizeOperationPage(req.Pn, req.Ps)

	reply, err := l.svcCtx.Operation.ListAuditIndex(l.ctx, &operationrpc.ListAuditIndexReq{
		Ctx:          opCtx,
		AdminId:      req.AdminId,
		Action:       req.Action,
		ResourceType: req.ResourceType,
		ResourceId:   req.ResourceId,
		StartAt:      req.StartAt,
		EndAt:        req.EndAt,
		Pn:           pn,
		Ps:           ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listAuditIndex: operator=%d target_admin=%d action=%q resource_type=%q pn=%d ps=%d err=%v",
			opCtx.GetOperatorId(), req.AdminId, req.Action, req.ResourceType, pn, ps, err)
		return nil, err
	}
	return &types.OperationAuditsResponse{
		Code:    0,
		Message: "ok",
		Data:    types.OperationAuditsData{Total: reply.GetTotal(), Items: auditsToAPI(reply.GetItems())},
		TTL:     0,
	}, nil
}
