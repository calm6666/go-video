package logic

import (
	"context"

	"go-video/services/feed/internal/svc"
	"go-video/services/feed/model"
	"go-video/services/feed/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PushFeedLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPushFeedLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PushFeedLogic {
	return &PushFeedLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// PushFeed 领域服务（video/catalog/live-room）推送新动态入收件箱。
// 写扩散：fan-out 到所有粉丝的 Redis ZSet。
// 简化：不实现大 V 限流策略，所有用户都走写扩散。
func (l *PushFeedLogic) PushFeed(in *rpc.PushFeedReq) (*rpc.EmptyReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.Oid <= 0 {
		return nil, model.ErrInvalidOid
	}
	f := &model.FeedOutbox{
		Mid:       in.Mid,
		Oid:       in.Oid,
		Otype:     int32(in.Otype),
		Action:    int32(in.Action),
		Ctime:     in.Ctime,
		Title:     in.Title,
		Cover:     in.Cover,
		Uri:       in.Uri,
		ForwardID: in.ForwardId,
	}
	if _, err := l.svcCtx.Repository.PushFeed(l.ctx, f); err != nil {
		l.Errorf("feed/PushFeed: mid=%d oid=%d otype=%v err=%v",
			in.Mid, in.Oid, in.Otype, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
