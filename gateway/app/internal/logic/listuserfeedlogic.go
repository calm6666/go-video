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

type ListUserFeedLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询某用户主页动态
func NewListUserFeedLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListUserFeedLogic {
	return &ListUserFeedLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 查询某用户主页动态：聚合 feed ListUserFeed RPC。
// vmid 来自路径（被查看用户），mid 来自查询串（当前用户，0 表示未登录）；
// 动态可见性由 feed 服务判断，网关不做业务规则。
func (l *ListUserFeedLogic) ListUserFeed(req *types.ParamUserFeed) (resp *types.FeedResponse, err error) {
	if l.svcCtx.Feed == nil {
		return nil, errors.New("feed service not configured")
	}
	reply, err := l.svcCtx.Feed.ListUserFeed(l.ctx, &feedrpc.ListUserFeedReq{
		Vmid:   req.Vmid,
		Mid:    req.Mid,
		Cursor: req.Cursor,
		Ps:     req.Ps,
		RealIp: req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/userFeed: vmid=%d mid=%d cursor=%d ps=%d err=%v",
			req.Vmid, req.Mid, req.Cursor, req.Ps, err)
		return nil, err
	}
	return &types.FeedResponse{
		Code:    0,
		Message: "ok",
		Data:    toFeedData(reply),
		TTL:     0,
	}, nil
}
