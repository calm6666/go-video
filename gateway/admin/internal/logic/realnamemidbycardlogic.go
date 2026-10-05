// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	userprofilerc "go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RealnameMidByCardLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 按证件号批量查询 mid
func NewRealnameMidByCardLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RealnameMidByCardLogic {
	return &RealnameMidByCardLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 按证件号批量查询 mid：调用 user-profile MidByRealnameCard RPC。
func (l *RealnameMidByCardLogic) RealnameMidByCard(req *types.ParamMidByCard) (resp *types.MidByCardResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	if err := adminSessionGate(l.ctx, "realnameMidByCard"); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.UserProfile.MidByRealnameCard(l.ctx, &userprofilerc.MidByRealnameCardsReq{
		CardCode: req.CardCode,
		Country:  req.Country,
		CardType: req.CardType,
	})
	if err != nil {
		l.Errorf("gateway/admin/realnameMidByCard: err=%v", err)
		return nil, err
	}
	return &types.MidByCardResponse{
		Code:    0,
		Message: "ok",
		Data:    types.MidByCardData{CodeToMid: reply.GetCodeToMid()},
		TTL:     0,
	}, nil
}
