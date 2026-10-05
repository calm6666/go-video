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

type HasLikeLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 批量查询当前用户点赞状态
func NewHasLikeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *HasLikeLogic {
	return &HasLikeLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// HasLike 批量查询点赞状态：聚合 engagement HasLike RPC。
// 返回 message_id → 点赞状态映射（engagement.LikeState 枚举转 int32），
// 缺失 key 表示该对象无状态记录，语义由 engagement 定义。
func (l *HasLikeLogic) HasLike(req *types.ParamHasLike) (resp *types.EngagementHasLikeResponse, err error) {
	if l.svcCtx.Engagement == nil {
		return nil, errors.New("engagement service not configured")
	}
	reply, err := l.svcCtx.Engagement.HasLike(l.ctx, &engagementrpc.HasLikeReq{
		Business:   req.Business,
		MessageIds: req.MessageIds,
		Mid:        req.Mid,
		Ip:         req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/hasLike: business=%s mid=%d n=%d err=%v",
			req.Business, req.Mid, len(req.MessageIds), err)
		return nil, err
	}
	return &types.EngagementHasLikeResponse{
		Code:    0,
		Message: "ok",
		Data:    types.EngagementHasLikeData{States: toEngagementLikeStates(reply.GetStates())},
		TTL:     0,
	}, nil
}
