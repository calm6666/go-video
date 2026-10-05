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

type DeleteInboxMessageLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 用户侧软删除站内信（只影响本人收件箱）
func NewDeleteInboxMessageLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteInboxMessageLogic {
	return &DeleteInboxMessageLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// DeleteInboxMessage 只改本人的收件行：同一消息的其它收件人不受影响，
// 消息主体也不删除（AGENTS.md §5 数据所有权）。
func (l *DeleteInboxMessageLogic) DeleteInboxMessage(req *types.ParamInboxDelete) (resp *types.InboxOpResponse, err error) {
	if l.svcCtx.Inbox == nil {
		return nil, errors.New("inbox service not configured")
	}
	if len(req.MsgIds) == 0 {
		return nil, errors.New("gateway/app: msg_ids is empty")
	}
	reply, err := l.svcCtx.Inbox.DeleteMessage(l.ctx, &inboxrpc.DeleteMessageReq{Mid: req.Mid, MsgIds: req.MsgIds})
	if err != nil {
		l.Errorf("gateway/app/deleteInboxMessage: mid=%d n=%d err=%v", req.Mid, len(req.MsgIds), err)
		return nil, err
	}
	return &types.InboxOpResponse{
		Code:    0,
		Message: "ok",
		Data:    types.InboxOpData{Changed: reply.GetChanged(), UnreadTotal: reply.GetUnreadTotal()},
		TTL:     0,
	}, nil
}
