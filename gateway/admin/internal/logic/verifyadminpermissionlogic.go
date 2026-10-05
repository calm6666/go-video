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

type VerifyAdminPermissionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 权限判定调试入口（token 或 admin_id + resource + action，返回判定与命中角色）
func NewVerifyAdminPermissionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *VerifyAdminPermissionLogic {
	return &VerifyAdminPermissionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 权限判定：本路由不在 AdminPermission 中间件组内（中间件正是它的调用方），
// 供后台自助排查「这个账号为什么看不到某个按钮」。
// 判定结果与拒绝原因一律以 operation 为准，网关不做任何本地授权。
func (l *VerifyAdminPermissionLogic) VerifyAdminPermission(req *types.ParamVerifyAdminPermission) (resp *types.AdminPermissionResponse, err error) {
	if l.svcCtx.Operation == nil {
		return nil, errors.New("operation service not configured")
	}
	// token 与 admin_id 二选一（operation 侧 token 优先），两者都为空时无法定位主体。
	if req.Token == "" && req.AdminId <= 0 {
		return nil, errors.New("gateway/admin: token or admin_id required")
	}
	if err := requireNonEmpty("resource", req.Resource); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("action", req.Action); err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.Operation.VerifyAdminPermission(l.ctx, &operationrpc.VerifyAdminPermissionReq{
		Token:    req.Token,
		AdminId:  req.AdminId,
		Resource: req.Resource,
		Action:   req.Action,
		TraceId:  req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/verifyAdminPermission: admin_id=%d resource=%s action=%s err=%v",
			req.AdminId, req.Resource, req.Action, err)
		return nil, err
	}
	// 拒绝是正常业务结果（allowed=false），不是 error；token 不回显也不落日志。
	l.Infof("gateway/admin/verifyAdminPermission: admin_id=%d resource=%s action=%s allowed=%v reason=%s",
		reply.GetAdminId(), req.Resource, req.Action, reply.GetAllowed(), reply.GetReason())
	return &types.AdminPermissionResponse{
		Code:    0,
		Message: "ok",
		Data: types.AdminPermissionData{
			Allowed:      reply.GetAllowed(),
			AdminId:      reply.GetAdminId(),
			MatchedRoles: reply.GetMatchedRoles(),
			Reason:       reply.GetReason(),
		},
		TTL: int64TTL(reply.GetTtl()),
	}, nil
}
