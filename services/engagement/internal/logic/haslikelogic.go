package logic

import (
	"context"

	"go-video/services/engagement/internal/svc"
	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type HasLikeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewHasLikeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *HasLikeLogic {
	return &HasLikeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// HasLike 批量查询用户是否点赞。
func (l *HasLikeLogic) HasLike(in *rpc.HasLikeReq) (*rpc.HasLikeReply, error) {
	if in.Business == "" {
		return nil, model.ErrInvalidBusiness
	}
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if len(in.MessageIds) == 0 {
		return &rpc.HasLikeReply{States: map[int64]*rpc.UserLikeState{}}, nil
	}
	if len(in.MessageIds) > 100 {
		return nil, model.ErrTooManyMessageIDs
	}
	states, err := l.svcCtx.Repository.HasLike(l.ctx, in.Business, in.Mid, in.MessageIds)
	if err != nil {
		l.Errorf("engagement/HasLike: business=%s mid=%d err=%v", in.Business, in.Mid, err)
		return nil, err
	}
	out := make(map[int64]*rpc.UserLikeState, len(states))
	for id, s := range states {
		item := &rpc.UserLikeState{Mid: in.Mid}
		if s != nil {
			item.Time = s.Ctime
			item.State = stateToRPC(s.State)
		}
		out[id] = item
	}
	return &rpc.HasLikeReply{States: out}, nil
}
