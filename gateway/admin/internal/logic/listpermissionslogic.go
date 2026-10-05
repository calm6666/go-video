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

type ListPermissionsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询权限点（domain 为空表示全部域）
func NewListPermissionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListPermissionsLogic {
	return &ListPermissionsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 分页查询权限点：resource/action 可能是通配模式（"*"、"video:*"、"*:read"），
// 网关原样透出，不做展开或合法性判定（通配语义归 operation）。
func (l *ListPermissionsLogic) ListPermissions(req *types.ParamListPermissions) (resp *types.OperationPermissionsResponse, err error) {
	if l.svcCtx.Operation == nil {
		return nil, errors.New("operation service not configured")
	}
	opCtx, err := operationOpContext(l.ctx, req.Op, false)
	if err != nil {
		return nil, err
	}
	pn, ps := normalizeOperationPage(req.Pn, req.Ps)

	reply, err := l.svcCtx.Operation.ListPermissions(l.ctx, &operationrpc.ListPermissionsReq{
		Ctx:    opCtx,
		Domain: req.Domain,
		Pn:     pn,
		Ps:     ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listPermissions: operator=%d domain=%q pn=%d ps=%d err=%v",
			opCtx.GetOperatorId(), req.Domain, pn, ps, err)
		return nil, err
	}
	return &types.OperationPermissionsResponse{
		Code:    0,
		Message: "ok",
		Data:    types.OperationPermissionsData{Total: reply.GetTotal(), Items: permissionsToAPI(reply.GetItems())},
		TTL:     0,
	}, nil
}
