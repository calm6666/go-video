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

type LikeLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 点赞/取消点赞/点踩（幂等）
func NewLikeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LikeLogic {
	return &LikeLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 点赞/取消点赞/点踩：聚合 engagement Like RPC。
// 幂等由 engagement 服务保证（同一 mid + business + message_id 重复请求不重复计数），
// LikeReq 无独立幂等键字段，网关只透传业务标识，不重复计数。
func (l *LikeLogic) Like(req *types.ParamLike) (resp *types.EngagementLikeResponse, err error) {
	if l.svcCtx.Engagement == nil {
		return nil, errors.New("engagement service not configured")
	}
	reply, err := l.svcCtx.Engagement.Like(l.ctx, &engagementrpc.LikeReq{
		Business:  req.Business,
		Mid:       req.Mid,
		UpMid:     req.UpMid,
		OriginId:  req.OriginId,
		MessageId: req.MessageId,
		Action:    engagementrpc.Action(req.Action),
		Ip:        req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/like: business=%s mid=%d origin_id=%d message_id=%d action=%d err=%v",
			req.Business, req.Mid, req.OriginId, req.MessageId, req.Action, err)
		return nil, err
	}
	return &types.EngagementLikeResponse{
		Code:    0,
		Message: "ok",
		Data: types.EngagementLikeData{
			OriginId:      reply.GetOriginId(),
			MessageId:     reply.GetMessageId(),
			LikeNumber:    reply.GetLikeNumber(),
			DislikeNumber: reply.GetDislikeNumber(),
		},
		TTL: 0,
	}, nil
}
