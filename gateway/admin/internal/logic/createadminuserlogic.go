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

type CreateAdminUserLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 创建管理员账号（初始口令只进不出，响应永不返回散列）
func NewCreateAdminUserLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateAdminUserLogic {
	return &CreateAdminUserLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 创建管理员账号：账号名唯一性、口令强度、角色是否存在全部由 operation 判定；
// 网关只补审计主体（op.operator_id）、幂等键与必填字段。
func (l *CreateAdminUserLogic) CreateAdminUser(req *types.ParamCreateAdminUser) (resp *types.OperationAdminUserResponse, err error) {
	if l.svcCtx.Operation == nil {
		return nil, errors.New("operation service not configured")
	}
	opCtx, err := operationOpContext(l.ctx, req.Op, true)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("username", req.Username); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("password", req.Password); err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.Operation.CreateAdminUser(l.ctx, &operationrpc.CreateAdminUserReq{
		Ctx:                opCtx,
		Username:           req.Username,
		Password:           req.Password,
		Remark:             req.Remark,
		RoleIds:            req.RoleIds,
		SecondFactorTarget: req.SecondFactorTarget,
	})
	if err != nil {
		// 口令与二次校验目标（手机号）都不进日志：日志与审计索引同样受脱敏约束。
		l.Errorf("gateway/admin/createAdminUser: operator=%d username=%q role_ids=%d err=%v",
			opCtx.GetOperatorId(), req.Username, len(req.RoleIds), err)
		return nil, err
	}
	l.Infof("gateway/admin/createAdminUser: operator=%d admin_id=%d request_id=%s",
		opCtx.GetOperatorId(), reply.GetUser().GetAdminId(), opCtx.GetRequestId())
	return &types.OperationAdminUserResponse{
		Code:    0,
		Message: "ok",
		Data:    types.OperationAdminUserData{User: adminUserToAPI(reply.GetUser())},
		TTL:     0,
	}, nil
}
