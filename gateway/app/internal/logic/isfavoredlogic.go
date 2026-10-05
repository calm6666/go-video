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

type IsFavoredLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单对象收藏状态
func NewIsFavoredLogic(ctx context.Context, svcCtx *svc.ServiceContext) *IsFavoredLogic {
	return &IsFavoredLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// IsFavored 单对象收藏状态：聚合 engagement IsFavored RPC。
// 收藏夹可见性（私密夹、他人夹）由 engagement 判定，网关不推断 tp/otype 语义。
func (l *IsFavoredLogic) IsFavored(req *types.ParamIsFavored) (resp *types.EngagementFavStateResponse, err error) {
	if l.svcCtx.Engagement == nil {
		return nil, errors.New("engagement service not configured")
	}
	reply, err := l.svcCtx.Engagement.IsFavored(l.ctx, &engagementrpc.IsFavoredReq{
		Tp:  req.Tp,
		Mid: req.Mid,
		Oid: req.Oid,
	})
	if err != nil {
		l.Errorf("gateway/app/isFavored: tp=%d mid=%d oid=%d err=%v", req.Tp, req.Mid, req.Oid, err)
		return nil, err
	}
	return &types.EngagementFavStateResponse{
		Code:    0,
		Message: "ok",
		Data:    types.EngagementFavStateData{Faved: reply.GetFaved()},
		TTL:     0,
	}, nil
}
