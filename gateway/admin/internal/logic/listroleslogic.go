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

type ListRolesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询角色
func NewListRolesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRolesLogic {
	return &ListRolesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 分页查询角色：member_count 一并返回，后台据此判断能否删除（规则在 operation）。
func (l *ListRolesLogic) ListRoles(req *types.ParamListRoles) (resp *types.OperationRolesResponse, err error) {
	if l.svcCtx.Operation == nil {
		return nil, errors.New("operation service not configured")
	}
	opCtx, err := operationOpContext(l.ctx, req.Op, false)
	if err != nil {
		return nil, err
	}
	pn, ps := normalizeOperationPage(req.Pn, req.Ps)

	reply, err := l.svcCtx.Operation.ListRoles(l.ctx, &operationrpc.ListRolesReq{
		Ctx:     opCtx,
		State:   req.State,
		Keyword: req.Keyword,
		Pn:      pn,
		Ps:      ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listRoles: operator=%d state=%d keyword=%q pn=%d ps=%d err=%v",
			opCtx.GetOperatorId(), req.State, req.Keyword, pn, ps, err)
		return nil, err
	}
	return &types.OperationRolesResponse{
		Code:    0,
		Message: "ok",
		Data:    types.OperationRolesData{Total: reply.GetTotal(), Items: rolesToAPI(reply.GetItems())},
		TTL:     0,
	}, nil
}
