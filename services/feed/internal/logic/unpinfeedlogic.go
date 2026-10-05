package logic

import (
	"context"

	"go-video/services/feed/internal/svc"
	"go-video/services/feed/model"
	"go-video/services/feed/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UnpinFeedLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUnpinFeedLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UnpinFeedLogic {
	return &UnpinFeedLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// UnpinFeed 取消置顶动态。
// 幂等：未置顶返回 ErrPinNotFound。
func (l *UnpinFeedLogic) UnpinFeed(in *rpc.UnpinFeedReq) (*rpc.EmptyReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.FeedId <= 0 {
		return nil, model.ErrInvalidFeedID
	}
	if err := l.svcCtx.Repository.UnpinFeed(l.ctx, in.Mid, in.FeedId); err != nil {
		l.Errorf("feed/UnpinFeed: mid=%d feed_id=%d err=%v", in.Mid, in.FeedId, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
