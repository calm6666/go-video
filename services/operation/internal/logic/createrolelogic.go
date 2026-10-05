package logic

import (
	"context"

	"go-video/services/operation/internal/repository"
	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateRoleLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateRoleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateRoleLogic {
	return &CreateRoleLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 创建角色并绑定权限点
func (l *CreateRoleLogic) CreateRole(in *rpc.CreateRoleReq) (*rpc.CreateRoleReply, error) {
	actor, err := actorFrom(in.Ctx)
	if err != nil {
		return nil, err
	}
	view, err := l.svcCtx.Repository.CreateRole(l.ctx, actor, repository.CreateRoleInput{
		Name:          in.Name,
		Title:         in.Title,
		PermissionIDs: in.PermissionIds,
	})
	if err != nil {
		l.Errorf("operation/CreateRole: operator=%d name=%q err=%v", actor.AdminID, in.Name, err)
		return nil, err
	}
	return &rpc.CreateRoleReply{Role: roleItem(view)}, nil
}
