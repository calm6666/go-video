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

type RealnameAdultLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询实名成年状态
func NewRealnameAdultLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RealnameAdultLogic {
	return &RealnameAdultLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询实名成年状态：聚合 user-profile RealnameStrippedInfo RPC（含 adult_type）。
func (l *RealnameAdultLogic) RealnameAdult(req *types.ParamMid) (resp *types.RealnameAdultResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	reply, err := l.svcCtx.UserProfile.RealnameStrippedInfo(l.ctx, &userprofilerc.MemberMidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/realnameAdult: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.RealnameAdultResponse{
		Code:    0,
		Message: "ok",
		Data:    types.RealnameAdultData{Type: int8(reply.GetAdultType())},
		TTL:     0,
	}, nil
}
