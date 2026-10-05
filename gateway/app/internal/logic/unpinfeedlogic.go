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

type UnpinFeedLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 取消置顶动态
func NewUnpinFeedLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UnpinFeedLogic {
	return &UnpinFeedLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// UnpinFeed 取消置顶动态：聚合 feed UnpinFeed RPC。
// 幂等（未置顶时取消不报错）与归属校验由 feed 负责，网关不预判当前置顶状态。
func (l *UnpinFeedLogic) UnpinFeed(req *types.ParamPinFeed) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Feed == nil {
		return nil, errors.New("feed service not configured")
	}
	if _, err = l.svcCtx.Feed.UnpinFeed(l.ctx, &feedrpc.UnpinFeedReq{
		Mid:      req.Mid,
		FeedId:   req.FeedId,
		Operator: req.Operator,
		RealIp:   req.IP,
	}); err != nil {
		l.Errorf("gateway/app/unpinFeed: mid=%d feed_id=%d operator=%s err=%v",
			req.Mid, req.FeedId, req.Operator, err)
		return nil, err
	}
	return emptyResponse(), nil
}
