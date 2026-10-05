package logic

import (
	"context"

	"go-video/services/feed/internal/svc"
	"go-video/services/feed/model"
	"go-video/services/feed/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteFeedLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeleteFeedLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteFeedLogic {
	return &DeleteFeedLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// DeleteFeed 删除自己的动态（本人或运营）。
// 软删除 outbox + 清理作者 outbox ZSet + 清理粉丝 inbox ZSet + 软删除 inbox 投影。
// 鉴权（本人/运营）由网关或调用方负责，本接口按 mid 校验动态归属。
func (l *DeleteFeedLogic) DeleteFeed(in *rpc.DeleteFeedReq) (*rpc.EmptyReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.FeedId <= 0 {
		return nil, model.ErrInvalidFeedID
	}
	if err := l.svcCtx.Repository.DeleteFeed(l.ctx, in.Mid, in.FeedId); err != nil {
		l.Errorf("feed/DeleteFeed: mid=%d feed_id=%d err=%v", in.Mid, in.FeedId, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
