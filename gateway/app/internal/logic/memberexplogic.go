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

type MemberExpLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询经验等级信息（含当前经验）
func NewMemberExpLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MemberExpLogic {
	return &MemberExpLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询经验等级信息：聚合 user-profile Exp RPC。
func (l *MemberExpLogic) MemberExp(req *types.ParamMid) (resp *types.LevelResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	reply, err := l.svcCtx.UserProfile.Exp(l.ctx, &userprofilerc.MidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/memberExp: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.LevelResponse{
		Code:    0,
		Message: "ok",
		Data:    types.LevelData{LevelInfo: toMemberLevelInfo(reply)},
		TTL:     0,
	}, nil
}
