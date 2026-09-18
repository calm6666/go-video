// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	userprofilerc "go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ExpRewardLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询当日经验奖励统计
func NewExpRewardLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ExpRewardLogic {
	return &ExpRewardLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询当日经验奖励统计：聚合 user-profile ExpStat RPC。
func (l *ExpRewardLogic) ExpReward(req *types.ParamMid) (resp *types.ExpRewardResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	reply, err := l.svcCtx.UserProfile.ExpStat(l.ctx, &userprofilerc.MidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/expReward: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.ExpRewardResponse{
		Code:    0,
		Message: "ok",
		Data:    types.ExpRewardData{ExpStat: toExpStat(reply)},
		TTL:     0,
	}, nil
}
