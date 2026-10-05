package logic

import (
	"context"

	"go-video/services/operation/internal/repository"
	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateAdminUserLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateAdminUserLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateAdminUserLogic {
	return &CreateAdminUserLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 创建管理员账号（口令只在入参出现，响应永不返回散列）
func (l *CreateAdminUserLogic) CreateAdminUser(in *rpc.CreateAdminUserReq) (*rpc.CreateAdminUserReply, error) {
	actor, err := actorFrom(in.Ctx)
	if err != nil {
		return nil, err
	}
	view, err := l.svcCtx.Repository.CreateAdminUser(l.ctx, actor, repository.CreateAdminUserInput{
		Username:           in.Username,
		Password:           in.Password,
		Remark:             in.Remark,
		RoleIDs:            in.RoleIds,
		SecondFactorTarget: in.SecondFactorTarget,
	})
	if err != nil {
		// 日志只记操作者与账号名：口令与手机号（二次校验目标）一律不落日志。
		l.Errorf("operation/CreateAdminUser: operator=%d username=%q err=%v", actor.AdminID, in.Username, err)
		return nil, err
	}
	return &rpc.CreateAdminUserReply{User: adminUserItem(view)}, nil
}
