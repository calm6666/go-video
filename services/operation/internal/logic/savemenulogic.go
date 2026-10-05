package logic

import (
	"context"

	"go-video/services/operation/internal/repository"
	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SaveMenuLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSaveMenuLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SaveMenuLogic {
	return &SaveMenuLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 新建/更新菜单节点
func (l *SaveMenuLogic) SaveMenu(in *rpc.SaveMenuReq) (*rpc.SaveMenuReply, error) {
	actor, err := actorFrom(in.Ctx)
	if err != nil {
		return nil, err
	}
	view, err := l.svcCtx.Repository.SaveMenu(l.ctx, actor, repository.MenuInput{
		MenuID:             in.MenuId,
		ParentID:           in.ParentId,
		Name:               in.Name,
		Path:               in.Path,
		Icon:               in.Icon,
		Sort:               in.Sort,
		RequiredPermission: in.RequiredPermission,
		State:              in.State,
	})
	if err != nil {
		l.Errorf("operation/SaveMenu: operator=%d menu=%d err=%v", actor.AdminID, in.MenuId, err)
		return nil, err
	}
	return &rpc.SaveMenuReply{Menu: menuItem(view)}, nil
}
