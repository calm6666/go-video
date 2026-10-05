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

type WalletRechargeSettleLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 沙箱结算充值单（把钱记到自己台账上；不请求任何第三方支付）
func NewWalletRechargeSettleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *WalletRechargeSettleLogic {
	return &WalletRechargeSettleLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// WalletRechargeSettle 是**沙箱结算**：把充值单置为已入账并写一条充值流水，
// 全程只动 payment 的本地台账，不请求任何第三方支付渠道，也不涉及回调验签。
// 状态判定（是否可结算、已结算/已取消）在 payment，重复结算返回 duplicated=true 的结论。
// recharge_no 与 request_id 都判空后原样透传；本路由入参没有 mid，归属由服务侧按单据判定，
// 网关不自造主体。reply 的 wallet 快照如实投影；flow_id 在 types 里无对应位，不透出。
func (l *WalletRechargeSettleLogic) WalletRechargeSettle(req *types.ParamWalletRechargeNo) (resp *types.WalletRechargeResponse, err error) {
	if l.svcCtx.Payment == nil {
		return nil, errors.New("payment service not configured")
	}
	if err := requireText("recharge_no", req.RechargeNo); err != nil {
		return nil, err
	}
	if err := requireText("request_id", req.RequestId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Payment.SettleSandboxRecharge(l.ctx, &paymentrpc.SettleSandboxRechargeReq{
		RechargeNo: req.RechargeNo,
		Operator:   commerceSelfOperator,
		RequestId:  req.RequestId,
		Reason:     req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/app/walletRechargeSettle: recharge_no=%s err=%v", req.RechargeNo, err)
		return nil, err
	}
	return &types.WalletRechargeResponse{
		Code:    0,
		Message: "ok",
		Data: types.WalletRechargeData{
			Duplicated: reply.GetDuplicated(),
			Recharge:   walletRechargeToAPI(reply.GetRecharge()),
			Wallet:     walletToAPI(reply.GetWallet()),
		},
		TTL: 0,
	}, nil
}
