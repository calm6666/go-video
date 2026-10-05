package logic

import (
	"context"

	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListPermissionsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListPermissionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListPermissionsLogic {
	return &ListPermissionsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询权限点
func (l *ListPermissionsLogic) ListPermissions(in *rpc.ListPermissionsReq) (*rpc.ListPermissionsReply, error) {
	if _, err := actorFrom(in.Ctx); err != nil {
		return nil, err
	}
	views, total, err := l.svcCtx.Repository.ListPermissions(l.ctx, in.Domain, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("operation/ListPermissions: domain=%q err=%v", in.Domain, err)
		return nil, err
	}
	return &rpc.ListPermissionsReply{Items: permissionItems(views), Total: total}, nil
}
