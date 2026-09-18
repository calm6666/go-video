// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	accountrpc "go-video/services/account/rpc"
	userprofilerc "go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PrivacyLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询用户隐私信息（需白名单 appkey）
func NewPrivacyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PrivacyLogic {
	return &PrivacyLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询用户隐私信息：隐私数据由 user-profile 实名与 account 凭证域持有，
// 网关按查看者权限裁剪后返回（参考 /x/internal/v3/account/privacy 语义）。
// 当前 user-profile 未暴露实名详情 RPC（RealnameDetail 存在），此处聚合：
//   - Realname/IdentityCard/IdentitySex/HandIMG ← user-profile RealnameDetail；
//   - Tel/RegIP/RegTS ← account Profile（tel_status/email_status 已在 Profile 内）。
func (l *PrivacyLogic) Privacy(req *types.ParamMid) (resp *types.PrivacyResponse, err error) {
	if l.svcCtx.Account == nil || l.svcCtx.UserProfile == nil {
		return nil, errors.New("identity services not configured")
	}
	out := types.Privacy{}
	detail, err := l.svcCtx.UserProfile.RealnameDetail(l.ctx, &userprofilerc.MemberMidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/privacy: realname detail mid=%d err=%v", req.Mid, err)
	} else if detail != nil {
		out.Realname = detail.Realname
		out.IdentityCard = detail.Card
		out.IdentitySex = detail.Gender
		out.HandIMG = detail.HandImg
	}
	profile, err := l.svcCtx.Account.Profile3(l.ctx, &accountrpc.MidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/privacy: profile mid=%d err=%v", req.Mid, err)
	} else if p := profile.GetProfile(); p != nil {
		if p.TelStatus == 1 {
			out.Tel = "已绑定手机号"
		}
		out.RegTS = int64(p.JoinTime)
	}
	return &types.PrivacyResponse{
		Code:    0,
		Message: "ok",
		Data:    types.PrivacyData{Privacy: out},
		TTL:     0,
	}, nil
}
