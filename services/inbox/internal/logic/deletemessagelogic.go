package logic

import (
	"context"

	"go-video/services/inbox/internal/svc"
	"go-video/services/inbox/model"
	"go-video/services/inbox/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteMessageLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeleteMessageLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteMessageLogic {
	return &DeleteMessageLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// DeleteMessage 用户侧软删除：只改本人的 inbox_user_message.del_state，
// 消息主体与其它收件人的收件行不受影响；重复删除返回 changed=0。
func (l *DeleteMessageLogic) DeleteMessage(in *rpc.DeleteMessageReq) (*rpc.DeleteMessageReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if len(in.MsgIds) == 0 {
		return nil, model.ErrMessageNotFound
	}
	changed, owned, err := l.svcCtx.Repository.DeleteMessages(l.ctx, in.Mid, in.MsgIds)
	if err != nil {
		return nil, err
	}
	if !owned {
		// 一条都不属于该收件人：可能是越权或消息已被清理，不做静默成功。
		return nil, model.ErrMessageNotFound
	}
	total, err := unreadTotal(l.ctx, l.svcCtx, in.Mid)
	if err != nil {
		return nil, err
	}
	return &rpc.DeleteMessageReply{Changed: int32(changed), UnreadTotal: total}, nil
}
