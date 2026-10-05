package logic

import (
	"context"

	"go-video/services/operation/internal/repository"
	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateAdminUserLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateAdminUserLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateAdminUserLogic {
	return &UpdateAdminUserLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 更新管理员账号（备注/状态/重置口令，重置口令会吊销会话）
func (l *UpdateAdminUserLogic) UpdateAdminUser(in *rpc.UpdateAdminUserReq) (*rpc.UpdateAdminUserReply, error) {
	actor, err := actorFrom(in.Ctx)
	if err != nil {
		return nil, err
	}
	view, err := l.svcCtx.Repository.UpdateAdminUser(l.ctx, actor, repository.UpdateAdminUserInput{
		AdminID:            in.AdminId,
		Remark:             in.Remark,
		State:              in.State,
		NewPassword:        in.NewPassword,
		SecondFactorTarget: in.SecondFactorTarget,
	})
	if err != nil {
		l.Errorf("operation/UpdateAdminUser: operator=%d admin=%d err=%v", actor.AdminID, in.AdminId, err)
		return nil, err
	}
	return &rpc.UpdateAdminUserReply{User: adminUserItem(view)}, nil
}
