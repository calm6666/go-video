package logic

import (
	"context"

	"go-video/services/engagement/internal/svc"
	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RawStatLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRawStatLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RawStatLogic {
	return &RawStatLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// RawStat 查询单个对象的原始点赞计数。
func (l *RawStatLogic) RawStat(in *rpc.RawStatReq) (*rpc.RawStatReply, error) {
	if in.Business == "" {
		return nil, model.ErrInvalidBusiness
	}
	if in.MessageId <= 0 {
		return nil, model.ErrInvalidMessage
	}
	stat, err := l.svcCtx.Repository.RawStat(l.ctx, in.Business, in.OriginId, in.MessageId)
	if err != nil {
		l.Errorf("engagement/RawStat: business=%s msg=%d err=%v", in.Business, in.MessageId, err)
		return nil, err
	}
	if stat == nil {
		return &rpc.RawStatReply{OriginId: in.OriginId, MessageId: in.MessageId}, nil
	}
	return &rpc.RawStatReply{
		OriginId:      in.OriginId,
		MessageId:     in.MessageId,
		LikeNumber:    stat.LikeNumber,
		DislikeNumber: stat.DislikeNumber,
		LikeChange:    stat.LikeChange,
		DislikeChange: stat.DislikeChange,
	}, nil
}
