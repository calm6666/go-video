// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	membershiprpc "go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MbEntitlementsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 批量权益判定（播放详情页一次问多项）
func NewMbEntitlementsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MbEntitlementsLogic {
	return &MbEntitlementsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MbEntitlements 是全站唯一的会员权益判定出口（网关不复制档位比较逻辑，见 app.api 段落注释）。
// codes 为空时在网关直接拒绝：空集合必然得到空 decisions，端上会把「没传码」读成「都没权益」。
// 每个 code 的 reason（未开通/已过期/码被下线/码不存在）都是结论，逐项原样投影成 Code:0；
// 判定结论随时可变，TTL 固定 0，否则等于给过期权益续命。
func (l *MbEntitlementsLogic) MbEntitlements(req *types.ParamMbEntitlements) (resp *types.MbEntitlementsResponse, err error) {
	if l.svcCtx.Membership == nil {
		return nil, errors.New("membership service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	if err := requireCodes(req.Codes); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Membership.CheckEntitlements(l.ctx, &membershiprpc.CheckEntitlementsReq{
		Mid:   req.Mid,
		Codes: req.Codes,
	})
	if err != nil {
		l.Errorf("gateway/app/mbEntitlements: mid=%d codes=%d err=%v", req.Mid, len(req.Codes), err)
		return nil, err
	}
	return &types.MbEntitlementsResponse{
		Code:    0,
		Message: "ok",
		Data: types.MbEntitlementsData{
			Decisions: mbEntitlementDecisionsToAPI(reply.GetDecisions()),
			ExpireAt:  reply.GetExpireAt(),
			VipType:   int32(reply.GetVipType()),
		},
		TTL: 0,
	}, nil
}
