// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	accountrpc "go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CacheDelLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 失效指定用户的缓存
func NewCacheDelLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CacheDelLogic {
	return &CacheDelLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 失效指定用户的缓存：调用 account DelCache RPC。
// modifiedAttr=updateVip 时 account 会触发延迟二次失效。
func (l *CacheDelLogic) CacheDel(req *types.ParamModify) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	if err := adminSessionGate(l.ctx, "cacheDel"); err != nil {
		return nil, err
	}
	if _, err = l.svcCtx.Account.DelCache(l.ctx, &accountrpc.DelCacheReq{
		Mid:    req.Mid,
		Action: req.ModifiedAttr,
	}); err != nil {
		l.Errorf("gateway/admin/cacheDel: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.EmptyResponse{Code: 0, Message: "ok", Data: types.EmptyData{}, TTL: 0}, nil
}
