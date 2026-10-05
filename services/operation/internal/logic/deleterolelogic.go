package logic

import (
	"context"

	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteRoleLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeleteRoleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteRoleLogic {
	return &DeleteRoleLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 删除角色（仍有成员时拒绝）
func (l *DeleteRoleLogic) DeleteRole(in *rpc.DeleteRoleReq) (*rpc.EmptyReply, error) {
	actor, err := actorFrom(in.Ctx)
	if err != nil {
		return nil, err
	}
	if err := l.svcCtx.Repository.DeleteRole(l.ctx, actor, in.RoleId); err != nil {
		// 角色仍有成员时返回 model.ErrRoleHasMembers，由后台提示先迁移成员。
		l.Errorf("operation/DeleteRole: operator=%d role=%d err=%v", actor.AdminID, in.RoleId, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
