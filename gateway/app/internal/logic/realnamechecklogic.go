// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"
	"strings"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	userprofilerc "go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RealnameCheckLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 校验实名证件号
func NewRealnameCheckLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RealnameCheckLogic {
	return &RealnameCheckLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 校验实名证件号：聚合 user-profile RealnameDetail RPC 比对证件号。
func (l *RealnameCheckLogic) RealnameCheck(req *types.ParamRealnameCheck) (resp *types.RealnameCheckResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	if strings.TrimSpace(req.CardCode) == "" {
		return nil, errors.New("card_code is required")
	}
	detail, err := l.svcCtx.UserProfile.RealnameDetail(l.ctx, &userprofilerc.MemberMidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/realnameCheck: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	result := false
	if detail != nil && detail.GetStatus() == 1 {
		if req.CardType < 0 || detail.GetCardType() == int32(req.CardType) {
			result = strings.EqualFold(detail.GetCard(), strings.ToUpper(strings.TrimSpace(req.CardCode)))
		}
	}
	return &types.RealnameCheckResponse{
		Code:    0,
		Message: "ok",
		Data:    types.RealnameCheckData{Result: result},
		TTL:     0,
	}, nil
}
