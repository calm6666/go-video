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

type MoralLogLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询节操值变更日志
func NewMoralLogLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MoralLogLogic {
	return &MoralLogLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询节操值变更日志：聚合 user-profile MoralLog RPC。
func (l *MoralLogLogic) MoralLog(req *types.ParamMid) (resp *types.MoralLogResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	reply, err := l.svcCtx.UserProfile.MoralLog(l.ctx, &userprofilerc.MemberMidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/moralLog: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.MoralLogResponse{
		Code:    0,
		Message: "ok",
		Data:    types.MoralLogData{UserLogs: toUserLogs(reply.GetUserLogs())},
		TTL:     0,
	}, nil
}
