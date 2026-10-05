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

type AddFavLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 添加收藏
func NewAddFavLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddFavLogic {
	return &AddFavLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 添加收藏：聚合 engagement AddFav RPC。
// 幂等：AddFavReq 无幂等键字段，engagement 以唯一索引 (mid, oid, tp) +
// INSERT ... ON DUPLICATE KEY UPDATE 保证重复请求不产生重复收藏记录。
func (l *AddFavLogic) AddFav(req *types.ParamAddFav) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Engagement == nil {
		return nil, errors.New("engagement service not configured")
	}
	if _, err = l.svcCtx.Engagement.AddFav(l.ctx, &engagementrpc.AddFavReq{
		Tp:    req.Tp,
		Mid:   req.Mid,
		Fid:   req.Fid,
		Oid:   req.Oid,
		Otype: req.Otype,
	}); err != nil {
		l.Errorf("gateway/app/fav: tp=%d mid=%d fid=%d oid=%d otype=%d err=%v",
			req.Tp, req.Mid, req.Fid, req.Oid, req.Otype, err)
		return nil, err
	}
	return emptyResponse(), nil
}
