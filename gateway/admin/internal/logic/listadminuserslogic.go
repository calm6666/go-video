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

type ListAdminUsersLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询管理员账号（state/keyword 过滤，ps 上限 100）
func NewListAdminUsersLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListAdminUsersLogic {
	return &ListAdminUsersLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 分页查询管理员账号：读接口不要求 request_id（重复查询无副作用），
// 但仍要求可信的 op.operator_id——operation 对缺主体的读请求一律拒绝。
// 返回投影永不含口令散列与二次校验目标明文（由 operation 的脱敏视图保证）。
func (l *ListAdminUsersLogic) ListAdminUsers(req *types.ParamListAdminUsers) (resp *types.OperationAdminUsersResponse, err error) {
	if l.svcCtx.Operation == nil {
		return nil, errors.New("operation service not configured")
	}
	opCtx, err := operationOpContext(l.ctx, req.Op, false)
	if err != nil {
		return nil, err
	}
	pn, ps := normalizeOperationPage(req.Pn, req.Ps)

	reply, err := l.svcCtx.Operation.ListAdminUsers(l.ctx, &operationrpc.ListAdminUsersReq{
		Ctx:     opCtx,
		State:   req.State,
		Keyword: req.Keyword,
		Pn:      pn,
		Ps:      ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listAdminUsers: operator=%d state=%d keyword=%q pn=%d ps=%d err=%v",
			opCtx.GetOperatorId(), req.State, req.Keyword, pn, ps, err)
		return nil, err
	}
	return &types.OperationAdminUsersResponse{
		Code:    0,
		Message: "ok",
		Data:    types.OperationAdminUsersData{Total: reply.GetTotal(), Items: adminUsersToAPI(reply.GetItems())},
		TTL:     0,
	}, nil
}
