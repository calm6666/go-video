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

type DelFavLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除收藏
func NewDelFavLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DelFavLogic {
	return &DelFavLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 删除收藏：聚合 engagement DelFav RPC。
// 幂等：engagement 用条件更新（state = 0）标记取消，重复取消不会二次生效。
func (l *DelFavLogic) DelFav(req *types.ParamDelFav) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Engagement == nil {
		return nil, errors.New("engagement service not configured")
	}
	if _, err = l.svcCtx.Engagement.DelFav(l.ctx, &engagementrpc.DelFavReq{
		Tp:    req.Tp,
		Mid:   req.Mid,
		Fid:   req.Fid,
		Oid:   req.Oid,
		Otype: req.Otype,
	}); err != nil {
		l.Errorf("gateway/app/unfav: tp=%d mid=%d fid=%d oid=%d otype=%d err=%v",
			req.Tp, req.Mid, req.Fid, req.Oid, req.Otype, err)
		return nil, err
	}
	return emptyResponse(), nil
}
