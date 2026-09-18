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

type RealnameStatusLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// NewRealnameStatusLogic 查询实名认证状态
func NewRealnameStatusLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RealnameStatusLogic {
	return &RealnameStatusLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// RealnameStatus 查询实名认证状态：聚合 user-profile RealnameStatus RPC。
func (l *RealnameStatusLogic) RealnameStatus(req *types.ParamMid) (resp *types.RealnameStatusResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	reply, err := l.svcCtx.UserProfile.RealnameStatus(l.ctx, &userprofilerc.MemberMidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/realnameStatus: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.RealnameStatusResponse{
		Code:    0,
		Message: "ok",
		Data:    types.RealnameStatusData{Status: int8(reply.GetRealnameStatus())},
		TTL:     0,
	}, nil
}
