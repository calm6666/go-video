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

type MemberOfficialLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询生效官方认证信息
func NewMemberOfficialLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MemberOfficialLogic {
	return &MemberOfficialLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询生效官方认证信息：聚合 user-profile OfficialDoc RPC（未认证时降级零值）。
func (l *MemberOfficialLogic) MemberOfficial(req *types.ParamMid) (resp *types.OfficialInfoResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	reply, err := l.svcCtx.UserProfile.OfficialDoc(l.ctx, &userprofilerc.MidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/memberOfficial: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.OfficialInfoResponse{
		Code:    0,
		Message: "ok",
		Data: types.OfficialInfoData{OfficialInfo: types.OfficialInfo{
			Role:  reply.GetRole(),
			Title: reply.GetTitle(),
			Desc:  reply.GetDesc(),
		}},
		TTL: 0,
	}, nil
}
