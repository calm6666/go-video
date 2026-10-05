// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	inboxrpc "go-video/services/inbox/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type InboxUnreadLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 未读总数与分类未读
func NewInboxUnreadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *InboxUnreadLogic {
	return &InboxUnreadLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// InboxUnread 读侧走 Redis 加速；force_recompute 仅供客户端排障，
// 计数漂移的常规修复入口是 inbox 服务的 RecomputeUnread（cron/运营）。
func (l *InboxUnreadLogic) InboxUnread(req *types.ParamInboxUnread) (resp *types.InboxUnreadResponse, err error) {
	if l.svcCtx.Inbox == nil {
		return nil, errors.New("inbox service not configured")
	}
	reply, err := l.svcCtx.Inbox.GetUnreadCount(l.ctx, &inboxrpc.GetUnreadCountReq{
		Mid:            req.Mid,
		ForceRecompute: req.ForceRecompute,
	})
	if err != nil {
		l.Errorf("gateway/app/inboxUnread: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.InboxUnreadResponse{
		Code:    0,
		Message: "ok",
		Data: types.InboxUnreadData{
			Total:      reply.GetTotal(),
			ByCategory: inboxUnreadByCategoryToAPI(reply.GetByCategory()),
			Mtime:      reply.GetMtime(),
		},
		TTL: 0,
	}, nil
}
