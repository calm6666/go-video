// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	paymentrpc "go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type WalletRechargeCancelLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 取消未结算充值单
func NewWalletRechargeCancelLogic(ctx context.Context, svcCtx *svc.ServiceContext) *WalletRechargeCancelLogic {
	return &WalletRechargeCancelLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// WalletRechargeCancel 取消一张未结算的充值单。契约（payment.proto CancelRechargeReq.reason）
// 把 reason 定为必填，所以空串在网关直接拒绝，不把无理由的取消动作推给台账；
// 但「已入账/已取消的单不能取消」是 payment 的状态机判定，网关不预判也不代推进。
// 与 settle 同样：CancelRechargeReply 不带钱包快照，Data.Wallet 保持零值（见 conv_commerce.go 注释）。
func (l *WalletRechargeCancelLogic) WalletRechargeCancel(req *types.ParamWalletRechargeNo) (resp *types.WalletRechargeResponse, err error) {
	if l.svcCtx.Payment == nil {
		return nil, errors.New("payment service not configured")
	}
	if err := requireText("recharge_no", req.RechargeNo); err != nil {
		return nil, err
	}
	if err := requireText("request_id", req.RequestId); err != nil {
		return nil, err
	}
	if err := requireText("reason", req.Reason); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Payment.CancelRecharge(l.ctx, &paymentrpc.CancelRechargeReq{
		RechargeNo: req.RechargeNo,
		Operator:   commerceSelfOperator,
		RequestId:  req.RequestId,
		Reason:     req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/app/walletRechargeCancel: recharge_no=%s err=%v", req.RechargeNo, err)
		return nil, err
	}
	return &types.WalletRechargeResponse{
		Code:    0,
		Message: "ok",
		Data: types.WalletRechargeData{
			Duplicated: reply.GetDuplicated(),
			Recharge:   walletRechargeToAPI(reply.GetRecharge()),
		},
		TTL: 0,
	}, nil
}
