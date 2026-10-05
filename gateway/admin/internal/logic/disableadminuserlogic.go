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

type DisableAdminUserLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 禁用管理员账号（同时吊销全部会话）
func NewDisableAdminUserLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DisableAdminUserLogic {
	return &DisableAdminUserLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 禁用管理员账号：禁用即吊销全部后台会话，操作者不能禁用自己的账号——
// 这条自我保护规则由 operation 判定（ErrAdminSelfDisable），网关不做重复推断。
func (l *DisableAdminUserLogic) DisableAdminUser(req *types.ParamDisableAdminUser) (resp *types.OperationAdminUserResponse, err error) {
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

	reply, err := l.svcCtx.Operation.DisableAdminUser(l.ctx, &operationrpc.DisableAdminUserReq{
		Ctx:     opCtx,
		AdminId: req.AdminId,
		Reason:  req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/disableAdminUser: operator=%d admin_id=%d err=%v",
			opCtx.GetOperatorId(), req.AdminId, err)
		return nil, err
	}
	l.Infof("gateway/admin/disableAdminUser: operator=%d admin_id=%d state=%d request_id=%s",
		opCtx.GetOperatorId(), req.AdminId, reply.GetUser().GetState(), opCtx.GetRequestId())
	return &types.OperationAdminUserResponse{
		Code:    0,
		Message: "ok",
		Data:    types.OperationAdminUserData{User: adminUserToAPI(reply.GetUser())},
		TTL:     0,
	}, nil
}
