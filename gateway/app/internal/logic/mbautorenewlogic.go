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

type MbAutoRenewLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 自动续费签约/解约（沙箱：只记录意愿，不建立真实代扣协议）
func NewMbAutoRenewLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MbAutoRenewLogic {
	return &MbAutoRenewLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MbAutoRenew 翻转自动续费签约位：沙箱语义下只记录意愿，不建立任何代扣协议，
// 也不产生扣款。request_id 判空后**原样透传**（TrimSpace 后的值不回写、不代造 UUID），
// 因为幂等键被改动就等于再签一次。channel 是否只有 SANDBOX 由 membership 判定，
// 网关不建渠道白名单。duplicated=true 是幂等重放命中，属正常结论。
// operator 由网关渲染成自助身份 "user"，不接受客户端自报。签约位是可变结论，TTL 0。
func (l *MbAutoRenewLogic) MbAutoRenew(req *types.ParamMbAutoRenew) (resp *types.MbAutoRenewResponse, err error) {
	if l.svcCtx.Membership == nil {
		return nil, errors.New("membership service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	if err := requireText("request_id", req.RequestId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Membership.SetAutoRenew(l.ctx, &membershiprpc.SetAutoRenewReq{
		Mid:       req.Mid,
		VipType:   membershiprpc.VipType(req.VipType),
		On:        req.On,
		Channel:   req.Channel,
		Operator:  commerceSelfOperator,
		RequestId: req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/app/mbAutoRenew: mid=%d on=%t err=%v", req.Mid, req.On, err)
		return nil, err
	}
	return &types.MbAutoRenewResponse{
		Code:    0,
		Message: "ok",
		Data: types.MbAutoRenewData{
			Duplicated: reply.GetDuplicated(),
			Membership: mbMembershipToAPI(reply.GetMembership()),
		},
		TTL: 0,
	}, nil
}
