package logic

import (
	"context"

	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DisableAdminUserLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDisableAdminUserLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DisableAdminUserLogic {
	return &DisableAdminUserLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 禁用管理员账号（同时吊销全部会话）
func (l *DisableAdminUserLogic) DisableAdminUser(in *rpc.DisableAdminUserReq) (*rpc.DisableAdminUserReply, error) {
	actor, err := actorFrom(in.Ctx)
	if err != nil {
		return nil, err
	}
	view, err := l.svcCtx.Repository.DisableAdminUser(l.ctx, actor, in.AdminId, in.Reason)
	if err != nil {
		l.Errorf("operation/DisableAdminUser: operator=%d admin=%d err=%v", actor.AdminID, in.AdminId, err)
		return nil, err
	}
	return &rpc.DisableAdminUserReply{User: adminUserItem(view)}, nil
}
