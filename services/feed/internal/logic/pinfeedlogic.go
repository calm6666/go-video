package logic

import (
	"context"

	"go-video/services/feed/internal/svc"
	"go-video/services/feed/model"
	"go-video/services/feed/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PinFeedLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPinFeedLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PinFeedLogic {
	return &PinFeedLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// PinFeed 置顶动态（运营或本人）。
// 校验动态存在且属于本人空间。
func (l *PinFeedLogic) PinFeed(in *rpc.PinFeedReq) (*rpc.EmptyReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.FeedId <= 0 {
		return nil, model.ErrInvalidFeedID
	}
	if err := l.svcCtx.Repository.PinFeed(l.ctx, in.Mid, in.FeedId); err != nil {
		l.Errorf("feed/PinFeed: mid=%d feed_id=%d err=%v", in.Mid, in.FeedId, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
