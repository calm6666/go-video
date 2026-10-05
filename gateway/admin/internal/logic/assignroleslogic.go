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

type AssignRolesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 全量覆盖管理员角色（空数组表示清空）
func NewAssignRolesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AssignRolesLogic {
	return &AssignRolesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 全量覆盖角色：role_ids 是覆盖语义，漏传字段与传空数组都等于「清空该账号角色」，
// 后台表单必须回显当前角色后再提交（operation 侧契约如此，网关不做猜测式合并）。
func (l *AssignRolesLogic) AssignRoles(req *types.ParamAssignRoles) (resp *types.OperationAssignRolesResponse, err error) {
	if l.svcCtx.Operation == nil {
		return nil, errors.New("operation service not configured")
	}
	opCtx, err := operationOpContext(l.ctx, req.Op, true)
	if err != nil {
		return nil, err
	}
	if req.AdminId <= 0 {
		return nil, errors.New("gateway/admin: admin_id required")
	}

	reply, err := l.svcCtx.Operation.AssignRoles(l.ctx, &operationrpc.AssignRolesReq{
		Ctx:     opCtx,
		AdminId: req.AdminId,
		RoleIds: req.RoleIds,
	})
	if err != nil {
		l.Errorf("gateway/admin/assignRoles: operator=%d admin_id=%d role_ids=%d err=%v",
			opCtx.GetOperatorId(), req.AdminId, len(req.RoleIds), err)
		return nil, err
	}
	l.Infof("gateway/admin/assignRoles: operator=%d admin_id=%d assigned=%d request_id=%s",
		opCtx.GetOperatorId(), req.AdminId, len(reply.GetRoleIds()), opCtx.GetRequestId())
	return &types.OperationAssignRolesResponse{
		Code:    0,
		Message: "ok",
		Data:    types.OperationAssignRolesData{RoleIds: reply.GetRoleIds()},
		TTL:     0,
	}, nil
}
