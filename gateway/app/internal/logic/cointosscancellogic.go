// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	coinrpc "go-video/services/coin/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CoinTossCancelLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 取消投币（窗口内全额退回）
func NewCoinTossCancelLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CoinTossCancelLogic {
	return &CoinTossCancelLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CoinTossCancel 取消投币的窗口判定（是否超 cancel_window_seconds）在 coin 服务，
// 超窗返回 cancelled=false + reason 的结论而不是错误，网关原样投影成 Code:0。
// 幂等键同样原样透传。
// 契约缺口：coin.CancelTossReply 只有 cancelled/reason，没有 TossCoinReply 的
// reject_detail 位，因此本路由的 reason_text 只能是空串——网关不编造可读文案，
// 端上按 reason 枚举出文案。
func (l *CoinTossCancelLogic) CoinTossCancel(req *types.ParamCoinTossCancel) (resp *types.CoinTossResponse, err error) {
	if l.svcCtx.Coin == nil {
		return nil, errors.New("coin service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	if err := requireText("request_id", req.RequestId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Coin.CancelToss(l.ctx, &coinrpc.CancelTossReq{
		Mid:       req.Mid,
		TargetAid: req.TargetAid,
		RequestId: req.RequestId,
		Operator:  commerceSelfOperator, // 终端只可能是自助取消；代操作走 gateway/admin
		Reason:    "",                   // 运营代操作才必填，types 里没有该入参
	})
	if err != nil {
		l.Errorf("gateway/app/coinTossCancel: mid=%d target_aid=%d err=%v", req.Mid, req.TargetAid, err)
		return nil, err
	}
	return &types.CoinTossResponse{
		Code:    0,
		Message: "ok",
		Data: types.CoinTossData{
			Accepted:   reply.GetCancelled(),
			Reason:     int32(reply.GetReason()),
			Duplicated: reply.GetDuplicated(),
			Account:    coinAccountToAPI(reply.GetAccount()),
			Toss:       coinTossToAPI(reply.GetToss()),
			FlowId:     reply.GetFlowId(),
		},
		TTL: 0,
	}, nil
}
