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

type ExpUpdateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 增加经验值
func NewExpUpdateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ExpUpdateLogic {
	return &ExpUpdateLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 增加经验值：调用 user-profile UpdateExp RPC。
func (l *ExpUpdateLogic) ExpUpdate(req *types.ParamExp) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	if err := adminSessionGate(l.ctx, "expUpdate"); err != nil {
		return nil, err
	}
	if _, err = l.svcCtx.UserProfile.UpdateExp(l.ctx, &userprofilerc.AddExpReq{
		Mid:     req.Mid,
		Count:   req.Count,
		Reason:  req.Reason,
		Operate: req.Operate,
		Ip:      req.IP,
	}); err != nil {
		l.Errorf("gateway/admin/expUpdate: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.EmptyResponse{Code: 0, Message: "ok", Data: types.EmptyData{}, TTL: 0}, nil
}
