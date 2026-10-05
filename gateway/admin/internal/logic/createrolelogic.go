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

type CreateRoleLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 创建角色并绑定权限点
func NewCreateRoleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateRoleLogic {
	return &CreateRoleLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 创建角色：name 唯一性与 permission_ids 是否存在由 operation 判定。
// 网关不校验「能否创建超级管理员」这类提权规则——权限点目录只有服务侧知道（AGENTS.md §5）。
func (l *CreateRoleLogic) CreateRole(req *types.ParamCreateRole) (resp *types.OperationRoleResponse, err error) {
	if l.svcCtx.Operation == nil {
		return nil, errors.New("operation service not configured")
	}
	opCtx, err := operationOpContext(l.ctx, req.Op, true)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("name", req.Name); err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.Operation.CreateRole(l.ctx, &operationrpc.CreateRoleReq{
		Ctx:           opCtx,
		Name:          req.Name,
		Title:         req.Title,
		PermissionIds: req.PermissionIds,
	})
	if err != nil {
		l.Errorf("gateway/admin/createRole: operator=%d name=%q permission_ids=%d err=%v",
			opCtx.GetOperatorId(), req.Name, len(req.PermissionIds), err)
		return nil, err
	}
	l.Infof("gateway/admin/createRole: operator=%d role_id=%d name=%s request_id=%s",
		opCtx.GetOperatorId(), reply.GetRole().GetRoleId(), reply.GetRole().GetName(), opCtx.GetRequestId())
	return &types.OperationRoleResponse{
		Code:    0,
		Message: "ok",
		Data:    types.OperationRoleData{Role: roleToAPI(reply.GetRole())},
		TTL:     0,
	}, nil
}
