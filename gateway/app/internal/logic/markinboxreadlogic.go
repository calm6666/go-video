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

type MarkInboxReadLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 幂等标记已读
func NewMarkInboxReadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MarkInboxReadLogic {
	return &MarkInboxReadLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MarkInboxRead 重复提交不产生变更（changed=0），未读总数由 inbox 服务同事务维护。
func (l *MarkInboxReadLogic) MarkInboxRead(req *types.ParamInboxMarkRead) (resp *types.InboxOpResponse, err error) {
	if l.svcCtx.Inbox == nil {
		return nil, errors.New("inbox service not configured")
	}
	if len(req.MsgIds) == 0 {
		return nil, errors.New("gateway/app: msg_ids is empty")
	}
	reply, err := l.svcCtx.Inbox.MarkRead(l.ctx, &inboxrpc.MarkReadReq{Mid: req.Mid, MsgIds: req.MsgIds})
	if err != nil {
		l.Errorf("gateway/app/markInboxRead: mid=%d n=%d err=%v", req.Mid, len(req.MsgIds), err)
		return nil, err
	}
	return &types.InboxOpResponse{
		Code:    0,
		Message: "ok",
		Data:    types.InboxOpData{Changed: reply.GetChanged(), UnreadTotal: reply.GetUnreadTotal()},
		TTL:     0,
	}, nil
}
