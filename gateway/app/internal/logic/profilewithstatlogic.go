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

type ProfileWithStatLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询带统计的资料
func NewProfileWithStatLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ProfileWithStatLogic {
	return &ProfileWithStatLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询带统计的资料：聚合 account ProfileWithStat3 RPC。
func (l *ProfileWithStatLogic) ProfileWithStat(req *types.ParamMid) (resp *types.ProfileStatResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.ProfileWithStat3(l.ctx, &accountrpc.MidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/profileWithStat: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.ProfileStatResponse{
		Code:    0,
		Message: "ok",
		Data:    types.ProfileStatData{ProfileStat: toProfileStat(reply)},
		TTL:     0,
	}, nil
}
