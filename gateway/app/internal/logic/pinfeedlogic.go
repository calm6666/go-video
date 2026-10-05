// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	feedrpc "go-video/services/feed/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PinFeedLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 置顶本人动态
func NewPinFeedLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PinFeedLogic {
	return &PinFeedLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PinFeed 置顶本人动态：聚合 feed PinFeed RPC。
// 动态归属校验（mid 是否有权置顶该 feed_id）与置顶位数量限制由 feed 负责，
// 网关只透传 operator/real_ip 供审计。
func (l *PinFeedLogic) PinFeed(req *types.ParamPinFeed) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Feed == nil {
		return nil, errors.New("feed service not configured")
	}
	if _, err = l.svcCtx.Feed.PinFeed(l.ctx, &feedrpc.PinFeedReq{
		Mid:      req.Mid,
		FeedId:   req.FeedId,
		Operator: req.Operator,
		RealIp:   req.IP,
	}); err != nil {
		l.Errorf("gateway/app/pinFeed: mid=%d feed_id=%d operator=%s err=%v",
			req.Mid, req.FeedId, req.Operator, err)
		return nil, err
	}
	return emptyResponse(), nil
}
