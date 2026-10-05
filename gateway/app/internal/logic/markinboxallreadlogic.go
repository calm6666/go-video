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

type MarkInboxAllReadLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 按分类全部标记已读
func NewMarkInboxAllReadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MarkInboxAllReadLogic {
	return &MarkInboxAllReadLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MarkInboxAllRead category=0 表示全部分类。
func (l *MarkInboxAllReadLogic) MarkInboxAllRead(req *types.ParamInboxMarkRead) (resp *types.InboxOpResponse, err error) {
	if l.svcCtx.Inbox == nil {
		return nil, errors.New("inbox service not configured")
	}
	reply, err := l.svcCtx.Inbox.MarkAllRead(l.ctx, &inboxrpc.MarkAllReadReq{
		Mid:      req.Mid,
		Category: inboxrpc.Category(req.Category),
	})
	if err != nil {
		l.Errorf("gateway/app/markInboxAllRead: mid=%d category=%d err=%v", req.Mid, req.Category, err)
		return nil, err
	}
	return &types.InboxOpResponse{
		Code:    0,
		Message: "ok",
		Data:    types.InboxOpData{Changed: reply.GetChanged(), UnreadTotal: reply.GetUnreadTotal()},
		TTL:     0,
	}, nil
}
