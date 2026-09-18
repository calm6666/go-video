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

type MemberBaseLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询单个用户基础资料
func NewMemberBaseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MemberBaseLogic {
	return &MemberBaseLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询单个用户基础资料：聚合 user-profile Base RPC。
func (l *MemberBaseLogic) MemberBase(req *types.ParamMid) (resp *types.MemberBaseResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	reply, err := l.svcCtx.UserProfile.Base(l.ctx, &userprofilerc.MemberMidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/memberBase: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.MemberBaseResponse{
		Code:    0,
		Message: "ok",
		Data:    types.MemberBaseData{BaseInfo: toMemberBase(reply)},
		TTL:     0,
	}, nil
}
