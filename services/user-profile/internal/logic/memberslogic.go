package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MembersLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewMembersLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MembersLogic {
	return &MembersLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 批量查询用户全量信息。
// 参考 service.Members：最多 100 个，批量聚合基础+等级+官方认证。
func (l *MembersLogic) Members(in *rpc.MemberMidsReq) (*rpc.MemberInfosReply, error) {
	members, err := l.svcCtx.Repository.Members(l.ctx, in.Mids)
	if err != nil {
		l.Errorf("user-profile/Members: err=%v", err)
		return nil, err
	}
	return &rpc.MemberInfosReply{MemberInfos: members}, nil
}
