// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	engagementrpc "go-video/services/engagement/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UserLikesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 当前用户的点赞历史
func NewUserLikesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UserLikesLogic {
	return &UserLikesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// UserLikes 当前用户点赞历史：聚合 engagement UserLikes RPC。
// 分页上限（ps 最大 50）与记录可见性由 engagement 校验，网关不改写调用方请求。
func (l *UserLikesLogic) UserLikes(req *types.ParamUserLikes) (resp *types.EngagementUserLikesResponse, err error) {
	if l.svcCtx.Engagement == nil {
		return nil, errors.New("engagement service not configured")
	}
	reply, err := l.svcCtx.Engagement.UserLikes(l.ctx, &engagementrpc.UserLikesReq{
		Business: req.Business,
		Mid:      req.Mid,
		Pn:       req.Pn,
		Ps:       req.Ps,
		Ip:       req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/userLikes: business=%s mid=%d pn=%d ps=%d err=%v",
			req.Business, req.Mid, req.Pn, req.Ps, err)
		return nil, err
	}
	return &types.EngagementUserLikesResponse{
		Code:    0,
		Message: "ok",
		Data: types.EngagementUserLikesData{
			Total: reply.GetTotal(),
			Items: toEngagementLikeItems(reply.GetItems()),
		},
		TTL: 0,
	}, nil
}
