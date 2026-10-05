package logic

import (
	"context"

	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreatePermissionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreatePermissionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreatePermissionLogic {
	return &CreatePermissionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 创建权限点（resource + action 唯一）
func (l *CreatePermissionLogic) CreatePermission(in *rpc.CreatePermissionReq) (*rpc.CreatePermissionReply, error) {
	actor, err := actorFrom(in.Ctx)
	if err != nil {
		return nil, err
	}
	view, err := l.svcCtx.Repository.CreatePermission(l.ctx, actor, in.Resource, in.Action, in.Domain, in.Description)
	if err != nil {
		l.Errorf("operation/CreatePermission: operator=%d resource=%q action=%q err=%v",
			actor.AdminID, in.Resource, in.Action, err)
		return nil, err
	}
	return &rpc.CreatePermissionReply{Permission: permissionItem(view)}, nil
}
