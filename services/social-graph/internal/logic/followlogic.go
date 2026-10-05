package logic

import (
	"context"

	"go-video/services/social-graph/internal/svc"
	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type FollowLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewFollowLogic(ctx context.Context, svcCtx *svc.ServiceContext) *FollowLogic {
	return &FollowLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 关注（幂等：重复不重复计数）。
// 黑名单约束：mid 已拉黑 follower_mid 时拒绝关注，需先取消拉黑。
func (l *FollowLogic) Follow(in *rpc.FollowReq) (*rpc.EmptyReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.FollowerMid <= 0 {
		return nil, model.ErrInvalidFollowerMid
	}
	if in.Mid == in.FollowerMid {
		return nil, model.ErrSelfAction
	}
	blacked, err := l.svcCtx.Repository.IsBlacked(l.ctx, in.Mid, in.FollowerMid)
	if err != nil {
		l.Errorf("social-graph/Follow IsBlacked: mid=%d follower=%d err=%v", in.Mid, in.FollowerMid, err)
		return nil, err
	}
	if blacked {
		return nil, model.ErrBlackNeedCancelFollow
	}
	if _, err := l.svcCtx.Repository.Follow(l.ctx, in.Mid, in.FollowerMid); err != nil {
		l.Errorf("social-graph/Follow: mid=%d follower=%d err=%v", in.Mid, in.FollowerMid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
