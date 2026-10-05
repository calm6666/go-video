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

type ClearUnreadLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 清零未读计数
func NewClearUnreadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ClearUnreadLogic {
	return &ClearUnreadLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 清零未读计数：聚合 feed ClearUnread RPC（未读计数归 feed 服务所有，见 AGENTS.md §5）。
// 该操作天然幂等（重复清零结果相同），故不需要额外幂等键。
func (l *ClearUnreadLogic) ClearUnread(req *types.ParamFeedMid) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Feed == nil {
		return nil, errors.New("feed service not configured")
	}
	if _, err = l.svcCtx.Feed.ClearUnread(l.ctx, &feedrpc.MidReq{
		Mid:    req.Mid,
		RealIp: req.IP,
	}); err != nil {
		l.Errorf("gateway/app/clearUnread: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
