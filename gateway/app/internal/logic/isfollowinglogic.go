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

type IsFollowingLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询 mid 是否关注 owner
func NewIsFollowingLogic(ctx context.Context, svcCtx *svc.ServiceContext) *IsFollowingLogic {
	return &IsFollowingLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 查询 mid 是否关注 owner：聚合 social-graph IsFollowing RPC。
func (l *IsFollowingLogic) IsFollowing(req *types.ParamRelation) (resp *types.SocialIsFollowingResponse, err error) {
	if l.svcCtx.SocialGraph == nil {
		return nil, errors.New("social-graph service not configured")
	}
	reply, err := l.svcCtx.SocialGraph.IsFollowing(l.ctx, &socialgraphrpc.RelationReq{
		Mid:    req.Mid,
		Owner:  req.Owner,
		RealIp: req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/isFollowing: mid=%d owner=%d err=%v", req.Mid, req.Owner, err)
		return nil, err
	}
	return &types.SocialIsFollowingResponse{
		Code:    0,
		Message: "ok",
		Data:    types.SocialIsFollowingData{Following: reply.GetFollowing()},
		TTL:     0,
	}, nil
}
