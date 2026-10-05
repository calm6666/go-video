package logic

import (
	"context"

	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListAdminUsersLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListAdminUsersLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListAdminUsersLogic {
	return &ListAdminUsersLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询管理员账号
func (l *ListAdminUsersLogic) ListAdminUsers(in *rpc.ListAdminUsersReq) (*rpc.ListAdminUsersReply, error) {
	if _, err := actorFrom(in.Ctx); err != nil {
		return nil, err
	}
	views, total, err := l.svcCtx.Repository.ListAdminUsers(l.ctx, in.State, in.Keyword, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("operation/ListAdminUsers: state=%d keyword=%q err=%v", in.State, in.Keyword, err)
		return nil, err
	}
	return &rpc.ListAdminUsersReply{Items: adminUserItems(views), Total: total}, nil
}
