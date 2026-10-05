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

type EngagementStatsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 批量查询对象计数与当前用户状态
func NewEngagementStatsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *EngagementStatsLogic {
	return &EngagementStatsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 批量查询对象计数与当前用户状态：聚合 engagement Stats RPC。
// message_ids 上限（100）由 engagement 服务校验，网关不做业务规则判断。
func (l *EngagementStatsLogic) EngagementStats(req *types.ParamEngagementStats) (resp *types.EngagementStatsResponse, err error) {
	if l.svcCtx.Engagement == nil {
		return nil, errors.New("engagement service not configured")
	}
	reply, err := l.svcCtx.Engagement.Stats(l.ctx, &engagementrpc.StatsReq{
		Business:   req.Business,
		OriginId:   req.OriginId,
		MessageIds: req.MessageIds,
		Mid:        req.Mid,
		Ip:         req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/engagementStats: business=%s origin_id=%d mid=%d n=%d err=%v",
			req.Business, req.OriginId, req.Mid, len(req.MessageIds), err)
		return nil, err
	}
	return &types.EngagementStatsResponse{
		Code:    0,
		Message: "ok",
		Data:    types.EngagementStatsData{Stats: toEngagementStats(reply.GetStats())},
		TTL:     0,
	}, nil
}
