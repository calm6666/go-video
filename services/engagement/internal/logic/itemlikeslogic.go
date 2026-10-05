package logic

import (
	"context"

	"go-video/services/engagement/internal/svc"
	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ItemLikesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewItemLikesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ItemLikesLogic {
	return &ItemLikesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ItemLikes 对象的点赞人列表（按点赞时间倒序）。
func (l *ItemLikesLogic) ItemLikes(in *rpc.ItemLikesReq) (*rpc.ItemLikesReply, error) {
	if in.Business == "" {
		return nil, model.ErrInvalidBusiness
	}
	if in.MessageId <= 0 {
		return nil, model.ErrInvalidMessage
	}
	if in.Pn <= 0 {
		in.Pn = 1
	}
	if in.Ps <= 0 || in.Ps > 50 {
		return nil, model.ErrPsTooLarge
	}
	likes, _, err := l.svcCtx.Repository.ItemLikes(l.ctx, in.Business, in.OriginId, in.MessageId, in.LastMid, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("engagement/ItemLikes: business=%s msg=%d err=%v", in.Business, in.MessageId, err)
		return nil, err
	}
	out := make([]*rpc.UserRecord, 0, len(likes))
	for _, lk := range likes {
		out = append(out, &rpc.UserRecord{
			Mid:  lk.Mid,
			Time: lk.Ctime,
		})
	}
	return &rpc.ItemLikesReply{Users: out}, nil
}
