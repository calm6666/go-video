// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	accountrpc "go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CacheClearLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 接收资料变更通知，失效缓存
func NewCacheClearLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CacheClearLogic {
	return &CacheClearLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 接收资料变更通知失效缓存：解析 JSON 消息
// {"mid":123,"action":"updateVip","new":{"mid":123}}（mid 为 0 时回退 new.mid），
// 兼容纯数字 mid，调用 account DelCache RPC。
func (l *CacheClearLogic) CacheClear(req *types.ParamMsg) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	if err := adminSessionGate(l.ctx, "cacheClear"); err != nil {
		return nil, err
	}
	var msg struct {
		New struct {
			Mid int64 `json:"mid"`
		} `json:"new,omitempty"`
		Mid    int64  `json:"mid"`
		Action string `json:"action"`
	}
	if err = json.Unmarshal([]byte(req.Msg), &msg); err != nil {
		if mid, perr := strconv.ParseInt(req.Msg, 10, 64); perr == nil {
			msg.Mid = mid
		} else {
			l.Errorf("gateway/admin/cacheClear: parse msg=%q err=%v", req.Msg, err)
			return nil, err
		}
	}
	mid := msg.Mid
	if mid == 0 {
		mid = msg.New.Mid
	}
	if mid == 0 {
		return nil, errors.New("no mid in msg")
	}
	if _, err = l.svcCtx.Account.DelCache(l.ctx, &accountrpc.DelCacheReq{Mid: mid, Action: msg.Action}); err != nil {
		l.Errorf("gateway/admin/cacheClear: mid=%d err=%v", mid, err)
		return nil, err
	}
	return &types.EmptyResponse{Code: 0, Message: "ok", Data: types.EmptyData{}, TTL: 0}, nil
}
