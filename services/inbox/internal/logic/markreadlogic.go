package logic

import (
	"context"

	"go-video/services/inbox/internal/svc"
	"go-video/services/inbox/model"
	"go-video/services/inbox/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MarkReadLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewMarkReadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MarkReadLogic {
	return &MarkReadLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// MarkRead 幂等标记已读。
// changed 只统计真实从未读变已读的行，重复调用返回 0 且不触碰计数器；
// unread_total 取变更后的未读快照（DB 真值，Redis 仅加速）。
func (l *MarkReadLogic) MarkRead(in *rpc.MarkReadReq) (*rpc.MarkReadReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if len(in.MsgIds) == 0 {
		return nil, model.ErrMessageNotFound
	}
	changed, err := l.svcCtx.Repository.MarkReadBatch(l.ctx, in.Mid, in.MsgIds)
	if err != nil {
		return nil, err
	}
	total, err := unreadTotal(l.ctx, l.svcCtx, in.Mid)
	if err != nil {
		return nil, err
	}
	return &rpc.MarkReadReply{Changed: int32(changed), UnreadTotal: total}, nil
}
