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

type SaveMenuLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新建/更新菜单节点（menu_id 为 0 表示新建）
func NewSaveMenuLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SaveMenuLogic {
	return &SaveMenuLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 新建/更新菜单：menu_id=0 新建、父子环（ErrMenuParentSelf）、有子节点不可删等
// 规则由 operation 判定；required_permission 的 "resource#action" 合法性同样归服务侧。
func (l *SaveMenuLogic) SaveMenu(req *types.ParamSaveMenu) (resp *types.OperationMenuResponse, err error) {
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

	reply, err := l.svcCtx.Operation.SaveMenu(l.ctx, &operationrpc.SaveMenuReq{
		Ctx:                opCtx,
		MenuId:             req.MenuId,
		ParentId:           req.ParentId,
		Name:               req.Name,
		Path:               req.Path,
		Icon:               req.Icon,
		Sort:               req.Sort,
		RequiredPermission: req.RequiredPermission,
		State:              req.State,
	})
	if err != nil {
		l.Errorf("gateway/admin/saveMenu: operator=%d menu_id=%d parent_id=%d name=%q err=%v",
			opCtx.GetOperatorId(), req.MenuId, req.ParentId, req.Name, err)
		return nil, err
	}
	l.Infof("gateway/admin/saveMenu: operator=%d menu_id=%d request_id=%s",
		opCtx.GetOperatorId(), reply.GetMenu().GetMenuId(), opCtx.GetRequestId())
	return &types.OperationMenuResponse{
		Code:    0,
		Message: "ok",
		Data:    types.OperationMenuData{Menu: menuToAPI(reply.GetMenu())},
		TTL:     0,
	}, nil
}
