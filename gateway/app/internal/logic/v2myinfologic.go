// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	accountrpc "go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type V2MyInfoLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// v2 查询我的资料
func NewV2MyInfoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *V2MyInfoLogic {
	return &V2MyInfoLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// v2 查询我的资料（ProfileWithStat3 → V2MyInfo 字段转换）。
func (l *V2MyInfoLogic) V2MyInfo(req *types.ParamMid) (resp *types.V2MyInfoResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.ProfileWithStat3(l.ctx, &accountrpc.MidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/v2MyInfo: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.V2MyInfoResponse{
		Code:    0,
		Message: "ok",
		Data:    toV2MyInfo(reply),
		TTL:     0,
	}, nil
}
