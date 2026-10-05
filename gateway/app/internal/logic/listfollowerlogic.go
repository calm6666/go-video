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

type ListFollowerLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// mid 的粉丝列表（分页）
func NewListFollowerLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListFollowerLogic {
	return &ListFollowerLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// mid 的粉丝列表：聚合 social-graph ListFollower RPC。
// 分页上限（ps 最大 50）由 social-graph 校验，网关不改写调用方请求。
func (l *ListFollowerLogic) ListFollower(req *types.ParamSocialList) (resp *types.SocialFollowerResponse, err error) {
	if l.svcCtx.SocialGraph == nil {
		return nil, errors.New("social-graph service not configured")
	}
	reply, err := l.svcCtx.SocialGraph.ListFollower(l.ctx, &socialgraphrpc.ListReq{
		Mid:    req.Mid,
		Pn:     req.Pn,
		Ps:     req.Ps,
		RealIp: req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/follower: mid=%d pn=%d ps=%d err=%v", req.Mid, req.Pn, req.Ps, err)
		return nil, err
	}
	return &types.SocialFollowerResponse{
		Code:    0,
		Message: "ok",
		Data: types.SocialFollowerData{
			Total: reply.GetTotal(),
			Items: toSocialRelationItems(reply.GetItems()),
		},
		TTL: 0,
	}, nil
}
