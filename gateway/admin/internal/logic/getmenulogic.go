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

type GetMenuLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 按管理员角色并集返回可见菜单（后台 Web 专用）
func NewGetMenuLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetMenuLogic {
	return &GetMenuLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 拉取菜单：admin_id 传 0 时由 operation 回落到 op.operator_id（本服务不做隐式替换，
// 以免网关的会话主体与被查询的账号被混为一谈）。
// GetMenuReply.ttl 是前端可缓存秒数，放进统一信封的 ttl 字段。
func (l *GetMenuLogic) GetMenu(req *types.ParamGetMenu) (resp *types.OperationMenusResponse, err error) {
	if l.svcCtx.Operation == nil {
		return nil, errors.New("operation service not configured")
	}
	opCtx, err := operationOpContext(l.ctx, req.Op, false)
	if err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.Operation.GetMenu(l.ctx, &operationrpc.GetMenuReq{
		Ctx:     opCtx,
		AdminId: req.AdminId,
	})
	if err != nil {
		l.Errorf("gateway/admin/getMenu: operator=%d admin_id=%d err=%v", opCtx.GetOperatorId(), req.AdminId, err)
		return nil, err
	}
	return &types.OperationMenusResponse{
		Code:    0,
		Message: "ok",
		Data:    types.OperationMenusData{Items: menusToAPI(reply.GetItems())},
		TTL:     normalizeTTL(reply.GetTtl()),
	}, nil
}
