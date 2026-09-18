package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ProfileWithStat3Logic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewProfileWithStat3Logic(ctx context.Context, svcCtx *svc.ServiceContext) *ProfileWithStat3Logic {
	return &ProfileWithStat3Logic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询带统计的资料。
// 参考 service.ProfileWithStat：repository 内部并发聚合 Profile + LevelExp + Stat。
// 依据 AGENTS.md §1 商业化范围外约束，coins 字段固定返回 0，不再调用 coin 服务。
func (l *ProfileWithStat3Logic) ProfileWithStat3(in *rpc.MidReq) (*rpc.ProfileStatReply, error) {
	reply, err := l.svcCtx.Repository.ProfileWithStat(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("account/ProfileWithStat3: repository.ProfileWithStat mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	if reply == nil {
		reply = &rpc.ProfileStatReply{}
	}
	if reply.Profile == nil {
		reply.Profile = &rpc.Profile{Mid: in.Mid}
	}
	if reply.LevelInfo == nil {
		reply.LevelInfo = &rpc.LevelInfo{}
	}
	return reply, nil
}
