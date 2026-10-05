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

type DeleteRoleLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除角色（仍有成员时 operation 拒绝）
func NewDeleteRoleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteRoleLogic {
	return &DeleteRoleLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 删除角色：DeleteRoleReply 是 EmptyReply（契约无回传字段），因此 data 返回空对象，
// 是否仍有成员由 operation 以 ErrRoleHasMembers 拒绝，网关不预先查成员数。
func (l *DeleteRoleLogic) DeleteRole(req *types.ParamDeleteRole) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Operation == nil {
		return nil, errors.New("operation service not configured")
	}
	opCtx, err := operationOpContext(l.ctx, req.Op, true)
	if err != nil {
		return nil, err
	}
	if req.RoleId <= 0 {
		return nil, errors.New("gateway/admin: role_id required")
	}

	if _, err := l.svcCtx.Operation.DeleteRole(l.ctx, &operationrpc.DeleteRoleReq{
		Ctx:    opCtx,
		RoleId: req.RoleId,
	}); err != nil {
		l.Errorf("gateway/admin/deleteRole: operator=%d role_id=%d err=%v", opCtx.GetOperatorId(), req.RoleId, err)
		return nil, err
	}
	l.Infof("gateway/admin/deleteRole: operator=%d role_id=%d request_id=%s",
		opCtx.GetOperatorId(), req.RoleId, opCtx.GetRequestId())
	return &types.EmptyResponse{
		Code:    0,
		Message: "ok",
		Data:    types.EmptyData{},
		TTL:     0,
	}, nil
}
