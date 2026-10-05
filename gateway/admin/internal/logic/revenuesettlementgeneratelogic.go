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

type RevenueSettlementGenerateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 生成/重算周期结算单（mid=0 全量；force_void_confirmed 是危险位且必须带 reason）
func NewRevenueSettlementGenerateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevenueSettlementGenerateLogic {
	return &RevenueSettlementGenerateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevenueSettlementGenerate 转发 creator-revenue GenerateSettlement（出单/重算周期结算单）。
//
// 网关挡的三类（余下一切归服务，§5 结算台账只属于 creator-revenue）：
//  1. 主体：operator 只能由会话渲染，无会话 fail-closed —— 出单是有财务后果的写动作，
//     没有主体就一张都不出。
//  2. 幂等：idempotency_key → request_id 原值。服务会派生行级子键（同理要求父键留空间，
//     过长由 requireScopedRequestID 拒绝），幂等由 (period, mid) 保证；
//     duplicated=true（该周期该用户已有未作废单且本次未强制重算）是**成功结论**，
//     网关照抄并回服务给的这一批单，绝不换个号再打一次。
//  3. 不可能形状：period 缺空（.api 必填，空串连定位都没有）、mid 为负、
//     idempotency_key 缺空。
//
// 刻意**不下判断**的：
//   - period 的 YYYYMM 格式、是不是未来周期（ErrInvalidPeriod / ErrFuturePeriod）、
//     「这一期有没有台账」（ErrNoMetricsToSettle）全在服务，网关不预先拒也不代填上月；
//   - mid=0 是「该周期全量出单」的合法哨兵（服务用 GenerateMaxBatch 界定批量），
//     网关不给它填一个 mid、也不自己在本地循环出单；
//   - **force_void_confirmed 是危险位**：true 才允许把已 CONFIRMED 的单置 VOIDED 重算。
//     网关不默认关、不代为打开、也不因「看起来像误操作」而拒绝，更不在这里附带调
//     ConfirmSettlement「顺手把新单冻上」——「算完了」和「认了」是两个权限点、两件事。
//     它要求带 reason 这条**条件必填**（ErrForceVoidReasonRequired）也由服务判：
//     admin.api 把 reason 标成 optional 并注明这一条件，网关若预先拒空就是第二处规则源。
//
// 响应如实转达：generated / duplicated / truncated / settlements 全部照抄。
// **truncated=true 时不隐瞒也不本地补齐**——完整结果本来就该去 /settlement/list 读；
// 每张单的 payout_state 是服务给的值（本期恒 NOT_PAYABLE），出单不代表任何一笔钱付出。
func (l *RevenueSettlementGenerateLogic) RevenueSettlementGenerate(req *types.ParamRevenueSettlementGenerate) (resp *types.RevenueSettlementGenerateResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errRevenueServiceNotConfigured
	}
	if req == nil {
		return nil, errRevenueRequestMissing
	}
	operator, err := revenueOperator(l.ctx, "revenueSettlementGenerate", req.Operator)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("period", req.Period); err != nil {
		return nil, err
	}
	if err := revenueNonNeg("mid", req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.CreatorRevenue.GenerateSettlement(l.ctx, &creatorrevenuerpc.GenerateSettlementReq{
		Period:             req.Period,
		Mid:                req.Mid,
		ForceVoidConfirmed: req.ForceVoidConfirmed,
		Operator:           operator,
		RequestId:          req.IdempotencyKey,
		Reason:             req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/revenueSettlementGenerate: period=%q mid=%d force_void_confirmed=%t operator=%s trace_id=%s err=%v",
			req.Period, req.Mid, req.ForceVoidConfirmed, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/revenueSettlementGenerate: period=%q mid=%d force_void_confirmed=%t duplicated=%t generated=%d truncated=%t operator=%s",
		req.Period, req.Mid, req.ForceVoidConfirmed, reply.GetDuplicated(), reply.GetGenerated(),
		reply.GetTruncated(), operator)
	return &types.RevenueSettlementGenerateResponse{
		Code:    0,
		Message: "ok",
		Data: types.RevenueSettlementGenerateData{
			Duplicated:  reply.GetDuplicated(),
			Generated:   reply.GetGenerated(),
			Truncated:   reply.GetTruncated(),
			Settlements: revenueSettlementsToAPI(reply.GetSettlements()),
		},
		TTL: 0,
	}, nil
}
