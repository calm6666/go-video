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

type ListInboxMessagesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 收件箱分页（cursor 优先）
func NewListInboxMessagesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListInboxMessagesLogic {
	return &ListInboxMessagesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ListInboxMessages 按收件人视角投影（AGENTS.md §5：inbox 是站内信事实源，
// 与 feed 的动态未读是两个域，网关不合并两者计数）。
func (l *ListInboxMessagesLogic) ListInboxMessages(req *types.ParamInboxMessages) (resp *types.InboxMessagesResponse, err error) {
	if l.svcCtx.Inbox == nil {
		return nil, errors.New("inbox service not configured")
	}
	reply, err := l.svcCtx.Inbox.ListMessages(l.ctx, &inboxrpc.ListMessagesReq{
		Mid:        req.Mid,
		Category:   inboxrpc.Category(req.Category),
		Cursor:     req.Cursor,
		Ps:         req.Ps,
		UnreadOnly: req.UnreadOnly,
	})
	if err != nil {
		l.Errorf("gateway/app/listInboxMessages: mid=%d category=%d err=%v", req.Mid, req.Category, err)
		return nil, err
	}
	return &types.InboxMessagesResponse{
		Code:    0,
		Message: "ok",
		Data: types.InboxMessagesData{
			List:        inboxMessagesToAPI(reply.GetList()),
			NextCursor:  reply.GetNextCursor(),
			HasMore:     reply.GetHasMore(),
			UnreadTotal: reply.GetUnreadTotal(),
		},
		TTL: 0,
	}, nil
}
