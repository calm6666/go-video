package logic

import (
	"context"

	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetMenuLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetMenuLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetMenuLogic {
	return &GetMenuLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 按管理员角色并集返回可见菜单（后台 Web 专用）
func (l *GetMenuLogic) GetMenu(in *rpc.GetMenuReq) (*rpc.GetMenuReply, error) {
	actor, err := actorFrom(in.Ctx)
	if err != nil {
		return nil, err
	}
	// admin_id 允许代查（例如为同事预览可见范围）；0 时退化为操作者本人。
	adminID := in.AdminId
	if adminID <= 0 {
		adminID = actor.AdminID
	}
	views, ttl, err := l.svcCtx.Repository.GetMenu(l.ctx, adminID)
	if err != nil {
		l.Errorf("operation/GetMenu: admin=%d err=%v", adminID, err)
		return nil, err
	}
	return &rpc.GetMenuReply{Items: menuItems(views), Ttl: int64(ttl)}, nil
}
