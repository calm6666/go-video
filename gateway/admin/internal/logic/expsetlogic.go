// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	userprofilerc "go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ExpSetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 设置经验值（仅运营）
func NewExpSetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ExpSetLogic {
	return &ExpSetLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 设置经验值（仅运营）：调用 user-profile SetExp RPC。
func (l *ExpSetLogic) ExpSet(req *types.ParamExp) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	if err := adminSessionGate(l.ctx, "expSet"); err != nil {
		return nil, err
	}
	if _, err = l.svcCtx.UserProfile.SetExp(l.ctx, &userprofilerc.AddExpReq{
		Mid:     req.Mid,
		Count:   req.Count,
		Reason:  req.Reason,
		Operate: req.Operate,
		Ip:      req.IP,
	}); err != nil {
		l.Errorf("gateway/admin/expSet: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.EmptyResponse{Code: 0, Message: "ok", Data: types.EmptyData{}, TTL: 0}, nil
}
