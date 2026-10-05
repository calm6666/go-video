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

type UpdateAdminUserLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 更新管理员账号（备注/状态/重置口令，重置口令会吊销会话）
func NewUpdateAdminUserLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateAdminUserLogic {
	return &UpdateAdminUserLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 更新管理员账号：state 取值（0 不修改/1 正常/2 禁用）、重置口令是否会吊销会话、
// second_factor_target 传 "-" 关闭二次校验等语义全部由 operation 实现，网关原样转发。
func (l *UpdateAdminUserLogic) UpdateAdminUser(req *types.ParamUpdateAdminUser) (resp *types.OperationAdminUserResponse, err error) {
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

	reply, err := l.svcCtx.Operation.UpdateAdminUser(l.ctx, &operationrpc.UpdateAdminUserReq{
		Ctx:                opCtx,
		AdminId:            req.AdminId,
		Remark:             req.Remark,
		State:              req.State,
		NewPassword:        req.NewPassword,
		SecondFactorTarget: req.SecondFactorTarget,
	})
	if err != nil {
		// new_password 不落日志；只记目标账号与是否触发口令重置这类可定位信息。
		l.Errorf("gateway/admin/updateAdminUser: operator=%d admin_id=%d state=%d password_reset=%v err=%v",
			opCtx.GetOperatorId(), req.AdminId, req.State, req.NewPassword != "", err)
		return nil, err
	}
	l.Infof("gateway/admin/updateAdminUser: operator=%d admin_id=%d state=%d request_id=%s",
		opCtx.GetOperatorId(), req.AdminId, reply.GetUser().GetState(), opCtx.GetRequestId())
	return &types.OperationAdminUserResponse{
		Code:    0,
		Message: "ok",
		Data:    types.OperationAdminUserData{User: adminUserToAPI(reply.GetUser())},
		TTL:     0,
	}, nil
}
