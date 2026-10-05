// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	coinrpc "go-video/services/coin/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CoinGrantLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 发放/扣回硬币（正负皆可；只接受 ORDER_PACK/ADMIN_GRANT，不产生资金流水）
func NewCoinGrantLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CoinGrantLogic {
	return &CoinGrantLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CoinGrant 转发 coin GrantCoin（发放/扣回硬币，本域唯一写口）。
//
// 门槛分三类，其余一个都不接管（§5 硬币余额与投币记录只属于 coin）：
//  1. 主体：operator 只能由会话渲染成 gateway/admin:<admin_id>；拿不到会话或 AdminID<=0
//     时在下传前 fail-closed，一次 RPC 都不发（表单自称的 operator 只作日志线索）。
//     服务侧 operator 恒必填（ErrGrantOperatorRequired），而网关永远给得出，所以这条
//     拒绝在本域实际落在「没有主体」上——正因如此不能放开。
//  2. 幂等：idempotency_key → request_id 原值，不生成、不改写（改一个字符就丢幂等语义）。
//     重放回 duplicated=true + 首次 flow_id + 当前账户，是**成功结论**；
//     同一 request_id 参数不同 → ErrIdempotencyConflict 原样上抛，绝不换个号再打一次
//     （那等于把一次冲突变成两笔发放）。
//  3. 不可能形状：mid<=0（ErrInvalidMid，硬币没有游客号）、delta=0（无记账意义）、
//     flow_type 不在 {ORDER_PACK, ADMIN_GRANT}（见 coinGrantFlowType：TOSS/CANCEL_TOSS
//     是终端投币链路自己写的，从后台塞进来就是伪造互动信号、污染 spm 特征，§7；
//     EXPIRE 本项目未开启）。
//
// 刻意**不下判断**的（都归服务，网关只做一遍就会和服务口径漂移）：
//   - |delta| 是否超 Coin.MaxGrantDelta、负 delta 会不会把余额扣成负数
//     （ErrGrantDeltaInvalid / ErrGrantBalanceWouldGoNegative，含服务回的人读余额）；
//   - 「ADMIN_GRANT 必须带 reason」「ORDER_PACK 必须带 biz_no」这两条**按 flow_type 分叉的
//     条件必填**由服务判定 —— admin.api 的 reason 位注释明写「ADMIN_GRANT 必填，由服务判定」，
//     且标成 optional，网关若在 ADMIN_GRANT 上替它拒空就是第二处规则源；服务侧
//     ErrGrantReasonRequired/ErrGrantBizNoRequired 会原样透出，调用方照样拿得到明确拒绝；
//   - 日限/单片上限/取消窗口等投币规则与本口无关，网关不代为放宽也不代为收紧。
//
// 硬币不是钱：本路由**不产生任何资金流水**，与 payment 的余额调整是两套账（§1），
// 因此这里不存在「折算」「补齐」之类的字段，网关也不会把 delta 与 *_minor 混算。
func (l *CoinGrantLogic) CoinGrant(req *types.ParamCoinGrant) (resp *types.CoinGrantResponse, err error) {
	if l.svcCtx.Coin == nil {
		return nil, errCoinServiceNotConfigured
	}
	if req == nil {
		return nil, errCoinRequestMissing
	}
	operator, err := coinOperator(l.ctx, "coinGrant", req.Operator)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := coinPositive("mid", req.Mid); err != nil {
		return nil, err
	}
	if err := coinDeltaNonZero(req.Delta); err != nil {
		return nil, err
	}
	if err := coinGrantFlowType(req.FlowType); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Coin.GrantCoin(l.ctx, &coinrpc.GrantCoinReq{
		Mid:       req.Mid,
		Delta:     req.Delta,
		FlowType:  coinrpc.CoinFlowType(req.FlowType),
		BizNo:     req.BizNo,
		Operator:  operator,
		RequestId: req.IdempotencyKey,
		Reason:    req.Reason,
	})
	if err != nil {
		// trace_id 只进日志（GrantCoinReq 没有该字段可下传）。
		l.Errorf("gateway/admin/coinGrant: mid=%d delta=%d flow_type=%d biz_no=%q operator=%s trace_id=%s err=%v",
			req.Mid, req.Delta, req.FlowType, req.BizNo, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/coinGrant: mid=%d delta=%d flow_type=%d duplicated=%t flow_id=%d balance=%d operator=%s",
		req.Mid, req.Delta, req.FlowType, reply.GetDuplicated(), reply.GetFlowId(),
		reply.GetAccount().GetBalance(), operator)
	return &types.CoinGrantResponse{
		Code:    0,
		Message: "ok",
		Data: types.CoinGrantData{
			Duplicated: reply.GetDuplicated(),
			FlowId:     reply.GetFlowId(),
			Account:    coinAccountToAPI(reply.GetAccount()),
		},
		TTL: 0,
	}, nil
}
