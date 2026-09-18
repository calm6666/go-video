package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MemberLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewMemberLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MemberLogic {
	return &MemberLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 查询单个用户全量信息（基础+等级+官方认证）。
// 参考 service.Member：BaseInfo + Exp 聚合 + 官方认证内存快照。
func (l *MemberLogic) Member(in *rpc.MemberMidReq) (*rpc.MemberInfoReply, error) {
	reply, err := l.svcCtx.Repository.Member(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("user-profile/Member: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return reply, nil
}
