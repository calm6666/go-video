// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	creatorrevenuerpc "go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RevenueSettlementConfirmLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 确认结算单（金额冻结；只是认账，**不是钱已付出**，payout_state 恒 NOT_PAYABLE）
func NewRevenueSettlementConfirmLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevenueSettlementConfirmLogic {
	return &RevenueSettlementConfirmLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevenueSettlementConfirm 转发 creator-revenue ConfirmSettlement（批量确认结算单，金额冻结）。
//
// 语义必须钉住（AGENTS.md §1）：confirm 是「这份账认了」，**不是「钱已付出」**。
// 本期没有出金通道，每张单的 payout_state 恒 NOT_PAYABLE，本响应里也没有任何
// 打款/提现/到账位；网关不会去调其它服务凑出一句「已出账」。
//
// 网关挡的三类（余下归服务，§5 结算单状态机只属于 creator-revenue）：
//  1. 主体：operator 只能由会话渲染成 gateway/admin:<admin_id>，无会话 fail-closed ——
//     冻结金额是对外口径动作，confirmed_by 必须能追到人。
//  2. 幂等：idempotency_key → request_id 原值，不生成不改写；服务侧的拒绝
//     （ErrRequestReplayed / ErrRequestIDConflict）逐字上抛，网关绝不换号重打
//     （那等于把一次冲突变成两次冻结尝试）。
//     ConfirmSettlementReply **没有 duplicated 位**（只有 confirmed/failed_nos，缺口已上报）。
//  3. 不可能形状：reason 与 idempotency_key 缺空。
//
// 刻意**不下判断**的：
//   - settlement_nos 逐位原样下传：不去重、不排序、不 TrimSpace、不裁剪。
//     空列表由服务回 ErrSettlementNosRequired（admin.api 明写「空列表由服务拒绝」），
//     单号格式与列宽由 normalizeSettlementNos 判，超过 MaxConfirmBatch(200) 由服务
//     **拒绝而不是截断**（悄悄裁会让运营以为剩下的都冻上了）—— 网关一律不代做；
//   - 「这一张现在能不能确认」（非 DRAFT / 已作废 → 进 failed_nos 或直接拒）是服务的结论。
//
// confirmed 与 failed_nos 原样回，**不合并成「全部成功」**：部分确认是本路由的常态结果，
// 少了 failed_nos 后台就不知道该去复核哪几张单。
func (l *RevenueSettlementConfirmLogic) RevenueSettlementConfirm(req *types.ParamRevenueSettlementConfirm) (resp *types.RevenueSettlementConfirmResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errRevenueServiceNotConfigured
	}
	if req == nil {
		return nil, errRevenueRequestMissing
	}
	operator, err := revenueOperator(l.ctx, "revenueSettlementConfirm", req.Operator)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.CreatorRevenue.ConfirmSettlement(l.ctx, &creatorrevenuerpc.ConfirmSettlementReq{
		SettlementNos: req.SettlementNos,
		Operator:      operator,
		RequestId:     req.IdempotencyKey,
		Reason:        req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/revenueSettlementConfirm: requested=%d operator=%s trace_id=%s err=%v",
			len(req.SettlementNos), operator, req.TraceId, err)
		return nil, err
	}
	// 只记数量与单号数，不记 reason 正文（§7）；payout 一位都不出现，因为契约里就没有。
	l.Infof("gateway/admin/revenueSettlementConfirm: requested=%d confirmed=%d failed=%d operator=%s",
		len(req.SettlementNos), reply.GetConfirmed(), len(reply.GetFailedNos()), operator)
	return &types.RevenueSettlementConfirmResponse{
		Code:    0,
		Message: "ok",
		Data: types.RevenueSettlementConfirmData{
			Confirmed: reply.GetConfirmed(),
			FailedNos: reply.GetFailedNos(),
		},
		TTL: 0,
	}, nil
}
