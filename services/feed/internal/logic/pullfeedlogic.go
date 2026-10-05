package logic

import (
	"context"

	"go-video/services/feed/internal/svc"
	"go-video/services/feed/model"
	"go-video/services/feed/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PullFeedLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPullFeedLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PullFeedLogic {
	return &PullFeedLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// PullFeed 用户拉取关注流（cursor 翻页）。
// cursor=0 表示从头开始；否则只取 ctime < cursor 的条目。
func (l *PullFeedLogic) PullFeed(in *rpc.PullFeedReq) (*rpc.FeedReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.Cursor < 0 {
		return nil, model.ErrInvalidCursor
	}
	ps := normalizePs(in.Ps)
	items, nextCursor, hasMore, err := l.svcCtx.Repository.PullFeed(l.ctx, in.Mid, in.Cursor, ps)
	if err != nil {
		l.Errorf("feed/PullFeed: mid=%d cursor=%d err=%v", in.Mid, in.Cursor, err)
		return nil, err
	}
	return &rpc.FeedReply{
		Items:      toFeedItems(items),
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}
