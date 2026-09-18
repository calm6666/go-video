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

type MemberMoralLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询节操值
func NewMemberMoralLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MemberMoralLogic {
	return &MemberMoralLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询节操值：聚合 user-profile Moral RPC。
func (l *MemberMoralLogic) MemberMoral(req *types.ParamMid) (resp *types.MoralResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	reply, err := l.svcCtx.UserProfile.Moral(l.ctx, &userprofilerc.MemberMidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/memberMoral: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.MoralResponse{
		Code:    0,
		Message: "ok",
		Data:    types.MoralData{Moral: toMoral(reply)},
		TTL:     0,
	}, nil
}
