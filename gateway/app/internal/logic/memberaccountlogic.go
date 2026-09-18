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

type MemberAccountLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询我的全量资料（基础+等级+官方认证）
func NewMemberAccountLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MemberAccountLogic {
	return &MemberAccountLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询我的全量资料：聚合 user-profile Member RPC。
func (l *MemberAccountLogic) MemberAccount(req *types.ParamMid) (resp *types.MemberFullResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	reply, err := l.svcCtx.UserProfile.Member(l.ctx, &userprofilerc.MemberMidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/memberAccount: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.MemberFullResponse{
		Code:    0,
		Message: "ok",
		Data:    types.MemberFullData{MemberInfo: toMemberFull(reply)},
		TTL:     0,
	}, nil
}
