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

type DeleteFeedLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除本人动态（写扩散撤销由 feed 服务处理）
func NewDeleteFeedLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteFeedLogic {
	return &DeleteFeedLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// DeleteFeed 删除本人动态：聚合 feed DeleteFeed RPC。
// 写扩散撤销（粉丝收件箱清理、未读计数回退）与 outbox 事件由 feed 负责，
// 归属校验（本人或运营 operator）同样在 feed 完成，网关不判定权限。
func (l *DeleteFeedLogic) DeleteFeed(req *types.ParamDeleteFeed) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Feed == nil {
		return nil, errors.New("feed service not configured")
	}
	if _, err = l.svcCtx.Feed.DeleteFeed(l.ctx, &feedrpc.DeleteFeedReq{
		Mid:      req.Mid,
		FeedId:   req.FeedId,
		Operator: req.Operator,
		RealIp:   req.IP,
	}); err != nil {
		l.Errorf("gateway/app/deleteFeed: mid=%d feed_id=%d operator=%s err=%v",
			req.Mid, req.FeedId, req.Operator, err)
		return nil, err
	}
	return emptyResponse(), nil
}
