package logic

import (
	"context"

	"go-video/services/social-graph/internal/svc"
	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type IsFollowingLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewIsFollowingLogic(ctx context.Context, svcCtx *svc.ServiceContext) *IsFollowingLogic {
	return &IsFollowingLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询 mid 是否关注 owner。
func (l *IsFollowingLogic) IsFollowing(in *rpc.RelationReq) (*rpc.RelationReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.Owner <= 0 {
		return nil, model.ErrInvalidOwnerMid
	}
	ok, err := l.svcCtx.Repository.IsFollowing(l.ctx, in.Mid, in.Owner)
	if err != nil {
		l.Errorf("social-graph/IsFollowing: mid=%d owner=%d err=%v", in.Mid, in.Owner, err)
		return nil, err
	}
	return &rpc.RelationReply{Following: ok}, nil
}
