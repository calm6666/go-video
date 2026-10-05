package logic

import (
	"context"

	"go-video/services/social-graph/internal/svc"
	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UnfollowLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUnfollowLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UnfollowLogic {
	return &UnfollowLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 取关（幂等）。
func (l *UnfollowLogic) Unfollow(in *rpc.UnfollowReq) (*rpc.EmptyReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.FollowerMid <= 0 {
		return nil, model.ErrInvalidFollowerMid
	}
	if in.Mid == in.FollowerMid {
		return nil, model.ErrSelfAction
	}
	if _, err := l.svcCtx.Repository.Unfollow(l.ctx, in.Mid, in.FollowerMid); err != nil {
		l.Errorf("social-graph/Unfollow: mid=%d follower=%d err=%v", in.Mid, in.FollowerMid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
