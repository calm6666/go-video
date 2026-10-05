package logic

import (
	"context"
	"strings"

	"go-video/services/inbox/internal/svc"
	"go-video/services/inbox/model"
	"go-video/services/inbox/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SendSystemMessageLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSendSystemMessageLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SendSystemMessageLogic {
	return &SendSystemMessageLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// SendSystemMessage 系统/运营向单个或多个用户投递站内信。
//
// 约束：
//   - idempotency_key 必填。服务端不代造幂等键，否则调用方重试会重复投递（AGENTS.md §5）。
//   - 接收人在一次调用内去重，并受 Inbox.MaxRecipients 限制；
//     需要更大规模的运营群发应走 notification 服务的多渠道模板投递，
//     而不是把本接口当广播通道（详见 services/inbox/README.md）。
//   - 消息主体与全部收件行、未读快照在同一事务提交，不会出现“有主体无收件人”。
func (l *SendSystemMessageLogic) SendSystemMessage(in *rpc.SendSystemMessageReq) (*rpc.SendSystemMessageReply, error) {
	if in == nil {
		return nil, model.ErrEmptyRecipients
	}
	key := strings.TrimSpace(in.IdempotencyKey)
	if key == "" {
		return nil, model.ErrInvalidIdempotencyKey
	}
	if len(key) > 128 {
		return nil, model.ErrInvalidIdempotencyKey
	}

	msg := &model.InboxMessage{
		Category:       convertCategory(in.Category),
		MsgType:        convertMsgType(in.MsgType),
		Title:          in.Title,
		Content:        in.Content,
		SenderMid:      in.SenderMid,
		BizType:        in.BizType,
		BizID:          in.BizId,
		Extra:          in.Extra,
		IdempotencyKey: key,
		Operator:       in.Operator,
	}
	res, err := l.svcCtx.Repository.Deliver(l.ctx, msg, in.Mids)
	if err != nil {
		l.Errorf("inbox/SendSystemMessage: key=%s biz=%s/%s recipients=%d err=%v",
			key, in.BizType, in.BizId, len(in.Mids), err)
		return nil, err
	}
	if res.Deduplicated {
		l.Infof("inbox/SendSystemMessage: 幂等命中 key=%s msg_id=%d，未重复投递", key, res.MsgID)
	}
	return &rpc.SendSystemMessageReply{
		MsgId:        res.MsgID,
		Delivered:    res.Delivered,
		Deduplicated: res.Deduplicated,
		Ctime:        res.Ctime,
	}, nil
}
