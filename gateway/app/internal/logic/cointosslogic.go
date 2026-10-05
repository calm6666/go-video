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

type CoinTossLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 投币（扣币+记录+限额判定，request_id 幂等）
func NewCoinTossLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CoinTossLogic {
	return &CoinTossLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CoinToss 投币是扣减虚拟币的写操作，幂等完全依赖 request_id：判空后**原样透传**，
// 不修剪、不大小写改写、不代造 UUID——改一个字符就是换了一把幂等键，会重复扣币。
// 余额不足 / 超日限 / 超单片上限 / aid 非法都是 coin 服务的**结论**（accepted=false + reason），
// 照常返回 Code:0，端上按 reason 与 reason_text 决定文案；网关不重复判定限额，
// 也不把 reason_text 改写成自己的话。duplicated=true 表示命中重放，未重复扣币。
func (l *CoinTossLogic) CoinToss(req *types.ParamCoinToss) (resp *types.CoinTossResponse, err error) {
	if l.svcCtx.Coin == nil {
		return nil, errors.New("coin service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	if err := requireText("request_id", req.RequestId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Coin.TossCoin(l.ctx, &coinrpc.TossCoinReq{
		Mid:           req.Mid,
		TargetAid:     req.TargetAid,
		Count:         req.Count, // <=0 由服务按 1 处理，网关不补默认值
		RequestId:     req.RequestId,
		Platform:      coinrpc.Platform(req.Platform),
		ClientTraceId: req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/app/coinToss: mid=%d target_aid=%d count=%d err=%v", req.Mid, req.TargetAid, req.Count, err)
		return nil, err
	}
	return &types.CoinTossResponse{
		Code:    0,
		Message: "ok",
		Data: types.CoinTossData{
			Accepted:   reply.GetAccepted(),
			Reason:     int32(reply.GetReason()),
			ReasonText: reply.GetRejectDetail(),
			Duplicated: reply.GetDuplicated(),
			Account:    coinAccountToAPI(reply.GetAccount()),
			Toss:       coinTossToAPI(reply.GetToss()),
			FlowId:     reply.GetFlowId(),
		},
		TTL: 0,
	}, nil
}
