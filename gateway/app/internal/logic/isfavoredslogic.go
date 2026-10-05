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

// isFavoredsMaxOids 是 engagement.proto IsFavoredsReq.oids 声明的硬批量上限。
const isFavoredsMaxOids = 100

type IsFavoredsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 批量收藏状态（最多 100）
func NewIsFavoredsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *IsFavoredsLogic {
	return &IsFavoredsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// IsFavoreds 批量收藏状态：聚合 engagement IsFavoreds RPC。
// 网关只做 proto 注释里写明的硬批量上限（oids ≤ 100）预检，避免整个请求被服务端拒绝；
// 返回值只包含已收藏的对象，缺失 key 表示未收藏，语义由 engagement 定义。
func (l *IsFavoredsLogic) IsFavoreds(req *types.ParamIsFavoreds) (resp *types.EngagementFavStatesResponse, err error) {
	if l.svcCtx.Engagement == nil {
		return nil, errors.New("engagement service not configured")
	}
	if len(req.Oids) > isFavoredsMaxOids {
		return nil, errors.New("oids exceeds batch limit")
	}
	reply, err := l.svcCtx.Engagement.IsFavoreds(l.ctx, &engagementrpc.IsFavoredsReq{
		Tp:   req.Tp,
		Mid:  req.Mid,
		Oids: req.Oids,
	})
	if err != nil {
		l.Errorf("gateway/app/isFavoreds: tp=%d mid=%d n=%d err=%v", req.Tp, req.Mid, len(req.Oids), err)
		return nil, err
	}
	return &types.EngagementFavStatesResponse{
		Code:    0,
		Message: "ok",
		Data:    types.EngagementFavStatesData{Faveds: toEngagementFaveds(reply.GetFaveds())},
		TTL:     0,
	}, nil
}
