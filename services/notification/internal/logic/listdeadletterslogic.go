package logic

import (
	"context"
	"errors"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/notification/internal/policy"
	"go-video/services/notification/internal/svc"
	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

type ListDeadLettersLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListDeadLettersLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListDeadLettersLogic {
	return &ListDeadLettersLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询死信（按创建时间倒序）。
// 隐私：只回传报文摘要与原因，原始内容不落库也不回传（见 model.NotificationDeadLetter）。
func (l *ListDeadLettersLogic) ListDeadLetters(in *rpc.ListDeadLettersReq) (*rpc.ListDeadLettersReply, error) {
	if in == nil {
		return nil, errors.New("notification/logic: nil request")
	}
	pn, ps := normalizePage(in.GetPn(), in.GetPs())
	rows, total, err := l.svcCtx.Repository.ListDeadLetters(l.ctx, model.DeadLetterFilter{
		EventId: in.GetEventId(),
		Topic:   in.GetTopic(),
		State:   int32(in.GetState()),
	}, pn, ps)
	if err != nil {
		return nil, err
	}
	return &rpc.ListDeadLettersReply{DeadLetters: policy.ToDeadLetterInfos(rows), Total: total}, nil
}
