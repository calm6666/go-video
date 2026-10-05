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

type IsBlackedLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 是否已拉黑 owner
func NewIsBlackedLogic(ctx context.Context, svcCtx *svc.ServiceContext) *IsBlackedLogic {
	return &IsBlackedLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// IsBlacked 查询 mid 是否拉黑 owner：聚合 social-graph IsBlacked RPC。
// 复用 SocialIsFollowingData 承载 bool 结果（following 字段即"是/否"），
// 拉黑关系对第三方的可见性规则由 social-graph 判定。
func (l *IsBlackedLogic) IsBlacked(req *types.ParamRelation) (resp *types.SocialIsFollowingResponse, err error) {
	if l.svcCtx.SocialGraph == nil {
		return nil, errors.New("social-graph service not configured")
	}
	reply, err := l.svcCtx.SocialGraph.IsBlacked(l.ctx, &socialgraphrpc.RelationReq{
		Mid:    req.Mid,
		Owner:  req.Owner,
		RealIp: req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/isBlacked: mid=%d owner=%d err=%v", req.Mid, req.Owner, err)
		return nil, err
	}
	return &types.SocialIsFollowingResponse{
		Code:    0,
		Message: "ok",
		Data:    types.SocialIsFollowingData{Following: reply.GetFollowing()},
		TTL:     0,
	}, nil
}
