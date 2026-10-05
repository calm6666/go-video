package logic

import (
	"context"

	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRolesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRolesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRolesLogic {
	return &ListRolesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询角色
func (l *ListRolesLogic) ListRoles(in *rpc.ListRolesReq) (*rpc.ListRolesReply, error) {
	if _, err := actorFrom(in.Ctx); err != nil {
		return nil, err
	}
	views, total, err := l.svcCtx.Repository.ListRoles(l.ctx, in.State, in.Keyword, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("operation/ListRoles: state=%d keyword=%q err=%v", in.State, in.Keyword, err)
		return nil, err
	}
	return &rpc.ListRolesReply{Items: roleItems(views), Total: total}, nil
}
