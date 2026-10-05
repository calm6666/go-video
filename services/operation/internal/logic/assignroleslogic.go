package logic

import (
	"context"

	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AssignRolesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAssignRolesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AssignRolesLogic {
	return &AssignRolesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 全量覆盖管理员角色（并集生效）
func (l *AssignRolesLogic) AssignRoles(in *rpc.AssignRolesReq) (*rpc.AssignRolesReply, error) {
	actor, err := actorFrom(in.Ctx)
	if err != nil {
		return nil, err
	}
	roleIDs, err := l.svcCtx.Repository.AssignRoles(l.ctx, actor, in.AdminId, in.RoleIds)
	if err != nil {
		l.Errorf("operation/AssignRoles: operator=%d admin=%d err=%v", actor.AdminID, in.AdminId, err)
		return nil, err
	}
	return &rpc.AssignRolesReply{RoleIds: roleIDs}, nil
}
