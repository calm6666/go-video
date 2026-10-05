package logic

import (
	"context"

	"go-video/services/engagement/internal/svc"
	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UserLikesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUserLikesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UserLikesLogic {
	return &UserLikesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// UserLikes 用户点赞列表（按时间倒序）。
func (l *UserLikesLogic) UserLikes(in *rpc.UserLikesReq) (*rpc.UserLikesReply, error) {
	if in.Business == "" {
		return nil, model.ErrInvalidBusiness
	}
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.Pn <= 0 {
		in.Pn = 1
	}
	if in.Ps <= 0 || in.Ps > 50 {
		return nil, model.ErrPsTooLarge
	}
	likes, total, err := l.svcCtx.Repository.UserLikes(l.ctx, in.Business, in.Mid, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("engagement/UserLikes: business=%s mid=%d err=%v", in.Business, in.Mid, err)
		return nil, err
	}
	out := make([]*rpc.ItemRecord, 0, len(likes))
	for _, lk := range likes {
		out = append(out, &rpc.ItemRecord{
			MessageId: lk.MessageID,
			Time:      lk.Ctime,
		})
	}
	return &rpc.UserLikesReply{Total: total, Items: out}, nil
}
