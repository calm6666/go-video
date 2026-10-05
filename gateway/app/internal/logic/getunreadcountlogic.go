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

type GetUnreadCountLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询用户未读动态数
func NewGetUnreadCountLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetUnreadCountLogic {
	return &GetUnreadCountLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 查询用户未读动态数：聚合 feed GetUnreadCount RPC。
// 未读是实时计数，故 ttl 返回 0，不建议客户端缓存。
func (l *GetUnreadCountLogic) GetUnreadCount(req *types.ParamFeedMid) (resp *types.FeedUnreadResponse, err error) {
	if l.svcCtx.Feed == nil {
		return nil, errors.New("feed service not configured")
	}
	reply, err := l.svcCtx.Feed.GetUnreadCount(l.ctx, &feedrpc.MidReq{
		Mid:    req.Mid,
		RealIp: req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/unread: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.FeedUnreadResponse{
		Code:    0,
		Message: "ok",
		Data:    types.FeedUnreadData{Unread: reply.GetUnread()},
		TTL:     0,
	}, nil
}
