package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type GenerateSettlementLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGenerateSettlementLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GenerateSettlementLogic {
	return &GenerateSettlementLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// settleKeySuffixBytes 是行级幂等键后缀 `#<period>#<mid>` 的最大宽度预算
// （`#` 1 + period 6 + `#` 1 + int64 最坏 19 位 = 27）。
// 父 request_id 必须按它留出空间，否则落库时会在 VARCHAR(64) 上撞截断，
// 让「同一 request_id 重放」判不出来（见 requireScopedRequestID 的说明）。
const settleKeySuffixBytes = 27

// settlementEchoLimit 是全量出单时回显给调用方的单数上限。
// proto 注释即为口径：「全量时只回前若干条，完整结果查台账」——一批 500 张单
// 全量回传给网关会白烧几十倍带宽，而运营复核走 ListSettlements。
const settlementEchoLimit = 50

// defaultGenerateMaxBatch 在 GenerateMaxBatch 被配成 0/负数时兜底。
// 配置护栏不能让「漏配」变成「无界出单」，那会把整月台账锁在一个超长事务里。
const defaultGenerateMaxBatch int64 = 500

// 生成/重算周期结算单
//
// 判定口径（事务与锁顺序见 settlecompute.go）：
//   - period 必填、必须合法且不得晚于当前周期（ErrFuturePeriod）；未收官周期允许出单，
//     但只算预估口径（会打 Infof，便于事后解释「为什么这个月还有单」）；
//   - 只对 ENROLLED 出单：LEFT/SUSPENDED/从未参加一律不出（ErrNotEnrolled /
//     ErrEnrollmentSuspended / ErrEnrollmentNotFound）；
//   - 金额只从 cr_metric 的 capped_amount_minor 聚合，出单阶段不再折算一次；
//     没有任何台账 → ErrNoMetricsToSettle，绝不出 0 元单冒充「已结清」；
//   - DRAFT 在效单：就地重算覆盖（单号不变）；
//     CONFIRMED 在效单：不带 force_void_confirmed 时零写入、duplicated=true；
//     带该危险位时旧单置 VOIDED（原因落 void_reason）并另起新单号——已确认金额
//     永不就地改写，改必留痕；
//   - 幂等：落库键 <request_id>#<period>#<mid> 押在 cr_settlement.uniq_request_id 上，
//     事务内先抢键回读、事务外撞键也回读首次结论（settleReplayed），
//     一次 request_id 不会出两张单；
//   - mid=0 为全量模式：候选来自「本周期有台账且已 ENROLLED」的升序扫描，
//     单批上限 CreatorRevenue.GenerateMaxBatch，超出只处理前 N 条并回 truncated=true；
//     每个作者一个事务，单作者失败不牵连整批（可跳过的合法结论见 ignorableSettleSkip）。
func (l *GenerateSettlementLogic) GenerateSettlement(
	in *rpc.GenerateSettlementReq,
) (*rpc.GenerateSettlementReply, error) {
	if in == nil {
		in = &rpc.GenerateSettlementReq{}
	}
	if err := l.svcCtx.Ready(); err != nil {
		return nil, err
	}
	period, err := requirePeriodNotFuture(in.Period)
	if err != nil {
		return nil, err
	}
	operator, err := requireOperator(in.Operator)
	if err != nil {
		return nil, err
	}
	requestID, err := requireScopedRequestID(in.RequestId, settleKeySuffixBytes)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(in.Reason)
	if err := checkText("reason", reason, model.MaxReasonBytes); err != nil {
		return nil, err
	}
	// 强制作废已确认单是「推翻运营对外口径」的动作，没有原因就不给做（危险位不能空手按）。
	if in.ForceVoidConfirmed && reason == "" {
		return nil, model.ErrForceVoidReasonRequired
	}
	if in.Mid < 0 {
		return nil, fmt.Errorf("%w: mid=%d（0 表示该周期全量出单）", model.ErrInvalidMid, in.Mid)
	}
	defaultCurrency := strings.TrimSpace(l.svcCtx.Config.CreatorRevenue.DefaultCurrency)

	var (
		mids      []int64
		truncated bool
	)
	if in.Mid > 0 {
		mid, err := normalizeMid(in.Mid)
		if err != nil {
			return nil, err
		}
		mids = []int64{mid}
	} else {
		limit := l.svcCtx.Config.CreatorRevenue.GenerateMaxBatch
		if limit <= 0 {
			limit = defaultGenerateMaxBatch
		}
		// 多取一条只为判定「还有没有」，不为了多写一张单。
		cands, err := l.svcCtx.Metrics.ListSettleableMids(l.ctx, period, limit+1)
		if err != nil {
			l.Errorf("GenerateSettlement 扫描可出单作者失败 period=%s: %v", period, err)
			return nil, err
		}
		if int64(len(cands)) > limit {
			truncated = true
			cands = cands[:limit]
		}
		mids = cands
		if len(mids) == 0 {
			// 全量模式扫到空候选是合法结论（本月还没回填台账），回 generated=0；
			// 单作者模式（in.Mid>0）走 ErrNoMetricsToSettle，见 settlecompute.go。
			l.Infof("GenerateSettlement period=%s 无可出单作者（台账为空或均已非 ENROLLED）", period)
		}
	}
	if before, berr := model.PeriodBeforeCurrent(period); berr == nil && !before {
		l.Infof("GenerateSettlement period=%s 尚未收官，本次金额为预估口径 operator=%s", period, operator)
	}

	reply := &rpc.GenerateSettlementReply{Settlements: make([]*rpc.SettlementInfo, 0, len(mids))}
	for _, mid := range mids {
		row, action, serr := l.settleOne(period, mid, in.ForceVoidConfirmed, operator, requestID, reason, defaultCurrency)
		if serr != nil {
			if in.Mid == 0 && ignorableSettleSkip(serr) {
				// 扫描快照与本行状态之间的并发变化：跳过并留错误日志，整批继续。
				l.Errorf("GenerateSettlement 跳过 mid=%d period=%s request_id=%s: %v", mid, period, requestID, serr)
				reply.Duplicated = true
				continue
			}
			l.Errorf("GenerateSettlement 失败 period=%s mid=%d operator=%s request_id=%s: %v",
				period, mid, operator, requestID, serr)
			return nil, serr
		}
		if action.countsAsGenerated() {
			reply.Generated++
		} else {
			// 已有在效单未强制重算，或本次是同一 request_id 的重放。
			reply.Duplicated = true
		}
		if int64(len(reply.Settlements)) < settlementEchoLimit {
			reply.Settlements = append(reply.Settlements, settlementInfo(row))
		}
	}
	reply.Truncated = truncated
	l.Infof("GenerateSettlement 完成 period=%s operator=%s request_id=%s generated=%d duplicated=%v truncated=%v",
		period, operator, requestID, reply.Generated, reply.Duplicated, reply.Truncated)
	return reply, nil
}

// settleOne 处理一个 (period, mid)：一个事务内完成闸门判定、聚合与落库。
//
// 事务失败后（此刻已回滚）只做只读回读，把「撞幂等键」折算成首次结论，
// 与规则写侧 resolveRuleReplay 同一范式；无法回读到的唯一键冲突一律回
// ErrConcurrentUpdate 让调用方重试，绝不回零值冒充出单成功。
func (l *GenerateSettlementLogic) settleOne(
	period string, mid int64, force bool, operator, requestID, reason, defaultCurrency string,
) (*model.Settlement, settleAction, error) {
	q := &settleQuery{
		Period:          period,
		Mid:             mid,
		Force:           force,
		RequestKey:      model.BuildSettlementRequestKey(requestID, period, mid),
		Operator:        operator,
		Reason:          reason,
		DefaultCurrency: defaultCurrency,
	}

	var (
		row     *model.Settlement
		action  settleAction
		created bool
	)
	err := l.svcCtx.Transact(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		r, a, err := applySettlementInTx(ctx, tx, q)
		if err != nil {
			return err
		}
		row, action, created = r, a, true
		return nil
	})
	if err != nil {
		created = false
		if !model.IsDuplicateErr(err) {
			return nil, 0, err
		}
		prev, rerr := l.svcCtx.Settlements.FindByRequest(l.ctx, q.RequestKey)
		if rerr != nil {
			return nil, 0, rerr
		}
		if prev != nil {
			return prev, settleReplayed, nil
		}
		// 撞的是 uniq_active_period_mid 或 uniq_settlement_no：并发已有人出单，
		// 本次一个字都没写，让调用方重载后重试，不能悄悄出第二张单。
		return nil, 0, fmt.Errorf("%w: period=%s mid=%d 已被并发出单（%v），请重试",
			model.ErrConcurrentUpdate, period, mid, err)
	}
	if !created || row == nil {
		return nil, 0, fmt.Errorf("%w: period=%s mid=%d 事务提交但未产出结算单",
			model.ErrSettlementNotFound, period, mid)
	}
	return row, action, nil
}

// ignorableSettleSkip 判定全量批内错误是否属于「这个人本就不该出单」的合法结论。
//
// 只放行这几类：它们都发生在「扫描快照之后有人改了参与状态」的窗口里，
// 跳过是正确处置，且已 Errorf 留痕。其余错误（DB 故障、金额溢出、跨币种台账、
// 脏数据）必须整体失败——把失败折叠成「少出一个人」的静默跳过，等于让运营
// 以为这个月已经出完单了。
func ignorableSettleSkip(err error) bool {
	return errors.Is(err, model.ErrEnrollmentNotFound) ||
		errors.Is(err, model.ErrEnrollmentSuspended) ||
		errors.Is(err, model.ErrNotEnrolled) ||
		errors.Is(err, model.ErrNoMetricsToSettle)
}
