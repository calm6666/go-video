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

type FollowLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 关注（幂等：重复不重复计数）
func NewFollowLogic(ctx context.Context, svcCtx *svc.ServiceContext) *FollowLogic {
	return &FollowLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 关注：聚合 social-graph Follow RPC。
// 幂等：FollowReq 无幂等键字段，social-graph 以关系唯一索引保证重复关注不重复计数；
// 黑名单与自关注校验同样由 social-graph 负责，网关只透传 real_ip 供审计。
func (l *FollowLogic) Follow(req *types.ParamFollow) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.SocialGraph == nil {
		return nil, errors.New("social-graph service not configured")
	}
	if _, err = l.svcCtx.SocialGraph.Follow(l.ctx, &socialgraphrpc.FollowReq{
		Mid:         req.Mid,
		FollowerMid: req.FollowerMid,
		RealIp:      req.IP,
	}); err != nil {
		l.Errorf("gateway/app/follow: mid=%d follower_mid=%d err=%v", req.Mid, req.FollowerMid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
