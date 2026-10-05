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

type ListFollowingLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// mid 的关注列表（分页）
func NewListFollowingLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListFollowingLogic {
	return &ListFollowingLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// mid 的关注列表：聚合 social-graph ListFollowing RPC。
// 分页上限（ps 最大 50）由 social-graph 校验，网关不改写调用方请求。
func (l *ListFollowingLogic) ListFollowing(req *types.ParamSocialList) (resp *types.SocialFollowingResponse, err error) {
	if l.svcCtx.SocialGraph == nil {
		return nil, errors.New("social-graph service not configured")
	}
	reply, err := l.svcCtx.SocialGraph.ListFollowing(l.ctx, &socialgraphrpc.ListReq{
		Mid:    req.Mid,
		Pn:     req.Pn,
		Ps:     req.Ps,
		RealIp: req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/following: mid=%d pn=%d ps=%d err=%v", req.Mid, req.Pn, req.Ps, err)
		return nil, err
	}
	return &types.SocialFollowingResponse{
		Code:    0,
		Message: "ok",
		Data: types.SocialFollowingData{
			Total: reply.GetTotal(),
			Items: toSocialRelationItems(reply.GetItems()),
		},
		TTL: 0,
	}, nil
}
