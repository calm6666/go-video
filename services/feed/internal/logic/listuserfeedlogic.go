package logic

import (
	"context"

	"go-video/services/feed/internal/svc"
	"go-video/services/feed/model"
	"go-video/services/feed/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListUserFeedLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListUserFeedLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListUserFeedLogic {
	return &ListUserFeedLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListUserFeed 查询某用户主页动态（cursor 翻页）。
// 置顶动态不在此接口返回，由前端单独拉取置顶列表展示。
func (l *ListUserFeedLogic) ListUserFeed(in *rpc.ListUserFeedReq) (*rpc.FeedReply, error) {
	if in.Vmid <= 0 {
		return nil, model.ErrInvalidVmid
	}
	if in.Cursor < 0 {
		return nil, model.ErrInvalidCursor
	}
	ps := normalizePs(in.Ps)
	items, nextCursor, hasMore, err := l.svcCtx.Repository.ListUserFeed(l.ctx, in.Vmid, in.Cursor, ps)
	if err != nil {
		l.Errorf("feed/ListUserFeed: vmid=%d cursor=%d err=%v", in.Vmid, in.Cursor, err)
		return nil, err
	}
	return &rpc.FeedReply{
		Items:      toFeedItems(items),
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}
