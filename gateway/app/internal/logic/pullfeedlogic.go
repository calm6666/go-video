// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	feedrpc "go-video/services/feed/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PullFeedLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 拉取关注流（cursor 翻页）
func NewPullFeedLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PullFeedLogic {
	return &PullFeedLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 拉取关注流：聚合 feed PullFeed RPC（cursor 翻页，cursor=0 表示从头开始）。
// 未读清零是独立动作（POST /feed/clear_unread），网关不在读路径上隐式改写状态。
func (l *PullFeedLogic) PullFeed(req *types.ParamPullFeed) (resp *types.FeedResponse, err error) {
	if l.svcCtx.Feed == nil {
		return nil, errors.New("feed service not configured")
	}
	reply, err := l.svcCtx.Feed.PullFeed(l.ctx, &feedrpc.PullFeedReq{
		Mid:    req.Mid,
		Cursor: req.Cursor,
		Ps:     req.Ps,
		RealIp: req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/pullFeed: mid=%d cursor=%d ps=%d err=%v", req.Mid, req.Cursor, req.Ps, err)
		return nil, err
	}
	return &types.FeedResponse{
		Code:    0,
		Message: "ok",
		Data:    toFeedData(reply),
		TTL:     0,
	}, nil
}
