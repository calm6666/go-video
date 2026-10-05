// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	socialgraphrpc "go-video/services/social-graph/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UnfollowLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 取关（幂等）
func NewUnfollowLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UnfollowLogic {
	return &UnfollowLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 取关：聚合 social-graph Unfollow RPC（与 Follow 复用同一入参类型 ParamFollow）。
// 幂等：重复取关由 social-graph 保证不重复扣减计数。
func (l *UnfollowLogic) Unfollow(req *types.ParamFollow) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.SocialGraph == nil {
		return nil, errors.New("social-graph service not configured")
	}
	if _, err = l.svcCtx.SocialGraph.Unfollow(l.ctx, &socialgraphrpc.UnfollowReq{
		Mid:         req.Mid,
		FollowerMid: req.FollowerMid,
		RealIp:      req.IP,
	}); err != nil {
		l.Errorf("gateway/app/unfollow: mid=%d follower_mid=%d err=%v", req.Mid, req.FollowerMid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
