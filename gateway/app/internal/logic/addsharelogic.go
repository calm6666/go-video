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

type AddShareLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 上报分享并返回最新分享数
func NewAddShareLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddShareLogic {
	return &AddShareLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// AddShare 上报分享：聚合 engagement AddShare RPC。
// 分享数累加与去抖/风控口径由 engagement 负责，网关只透传 real_ip 并回显服务端计数。
func (l *AddShareLogic) AddShare(req *types.ParamAddShare) (resp *types.EngagementFolderOpResponse, err error) {
	if l.svcCtx.Engagement == nil {
		return nil, errors.New("engagement service not configured")
	}
	reply, err := l.svcCtx.Engagement.AddShare(l.ctx, &engagementrpc.AddShareReq{
		Oid:  req.Oid,
		Mid:  req.Mid,
		Type: req.Type,
		Ip:   req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/addShare: oid=%d mid=%d type=%d err=%v", req.Oid, req.Mid, req.Type, err)
		return nil, err
	}
	// 本路由只回 shares；fid 字段由 folder 路由使用，这里保持零值。
	return &types.EngagementFolderOpResponse{
		Code:    0,
		Message: "ok",
		Data:    types.EngagementFolderOpData{Shares: reply.GetShares()},
		TTL:     0,
	}, nil
}
