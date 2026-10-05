// 本文件是 logic 包的手写扩展（出单聚合、结算单落库与幂等重放），不是 goctl 生成产物。
// SQL 与库表读写一律留在 model（AGENTS.md §4）。

package logic

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"go-video/services/creator-revenue/model"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// settleAction 是一次 (period, mid) 出单的处置结论。
type settleAction int

const (
	// settleCreated 新建在效单（该周期该作者此前没有在效单）。
	settleCreated settleAction = iota
	// settleRecomputed 已有 DRAFT 单，就地重算覆盖（单号不变）。
	settleRecomputed
	// settleVoidedAndCreated force_void_confirmed 生效：旧 CONFIRMED 单置 VOIDED 后另起新单号。
	settleVoidedAndCreated
	// settleDuplicated 已有 CONFIRMED 单且本次未带强制位：什么都不改（proto duplicated 语义）。
	settleDuplicated
	// settleReplayed 行级幂等键 <request_id>#<period>#<mid> 命中：回首次产出的那张单。
	settleReplayed
)

// countsAsGenerated 判定该结论是否真的改动了结算台账。
// duplicated/replayed 只回读不改写，绝不能计入 reply.generated，
// 否则运营面板会把「重复点击」显示成「又出了一批单」。
func (a settleAction) countsAsGenerated() bool {
	return a == settleCreated || a == settleRecomputed || a == settleVoidedAndCreated
}

// settleQuery 是校验完的出单意图（一个 (period, mid) 一行）。
type settleQuery struct {
	Period string
	Mid    int64
	// Force 危险位：true 才允许把已 CONFIRMED 的单置 VOIDED 重算。
	Force bool
	// RequestKey 是落库的行级幂等键（BuildSettlementRequestKey 生成）。
	RequestKey string
	Operator   string
	// Reason 只在 Force 时必填（作废已确认单是有后果的动作，必须有据可查）。
	Reason string
	// DefaultCurrency 台账未给出可判定币种时的兜底记账币种（来自配置，不写死）。
	DefaultCurrency string
}

// settlementAgg 是一次出单的聚合结果。
type settlementAgg struct {
	amountMinor     int64
	capAppliedMinor int64
	metricCount     int64
	currency        string
	items           []*model.SettlementItem
}

// addAmount 做「溢出即报错」的金额累加。
// 结算金额一旦回绕成负数，等于凭空造出一笔倒扣的应计，比算错更危险，
// 所以这里宁可让整次出单失败（ErrAmountOverflow 自带 InvalidArgument 语义）。
func addAmount(sum, v int64, what string) (int64, error) {
	if v < 0 {
		return 0, fmt.Errorf("%w: %s 出现负金额 %d，台账被绕过写入口改了", model.ErrAmountOverflow, what, v)
	}
	if sum > math.MaxInt64-v {
		return 0, fmt.Errorf("%w: %s 累计 %d + %d 超出 int64", model.ErrAmountOverflow, what, sum, v)
	}
	return sum + v, nil
}

// applySettlementInTx 在一个事务里完成「闸门 + 聚合 + 幂等判定 + 主体与分项写入」。
//
// 事务边界为什么必须覆盖到分项：只有主体没有分项的单，运营看得到总额却看不到钱从哪来；
// 只有分项没有主体的单更糟——分项无主，复核时直接变成孤儿数据（AGENTS.md §5、§8）。
//
// 锁顺序（全服务统一，跨方法不要改）：
//  1. cr_settlement 在效行（LockActive，同时是 (period,mid) 出单的槽位锁）；
//  2. cr_metric 组行（ListGroupForUpdate，按 source_type 升序，锁与遍历同序）；
//     该锁同时挡住并发的 RecordRevenueMetric（它也会锁同组台账做封顶重分配），
//     所以「出单读到的金额」与「事务结束后台账里的金额」必然一致。
//
// 出单只读 cr_enrollment、不锁它：计量与出单的串行点押在 cr_metric 组锁上，
// 出单期间即使有人把作者置为 SUSPENDED，本单也已按「出单瞬间的 ENROLLED」算完，
// 下一次出单/重算才反映新状态——这是刻意选择：暂停不能把已经算完的应计悄悄抹掉。
func applySettlementInTx(
	ctx context.Context, tx sqlx.Session, q *settleQuery,
) (*model.Settlement, settleAction, error) {
	conn := sqlx.NewSqlConnFromSession(tx)
	settlements := model.NewSettlementModel(conn)
	enrollments := model.NewEnrollmentModel(conn)
	items := model.NewSettlementItemModel(conn)

	// 行锁先行：没有这把锁，两个并发请求可以各自数出同一个 rev 并同时插进 (period,mid,0) 槽位，
	// 其中一个只能以唯一键冲突失败——冲突本身无害，但结论会从「重算」退化成「报错」。
	active, err := settlements.LockActive(ctx, q.Period, q.Mid)
	if err != nil {
		return nil, 0, err
	}

	// 幂等键命中 → 回首次结论（含「首单后来被别的请求作废」的情况：
	// 重放看到的就应该是当时那张单，而不是重新出一张，否则一次 request_id 出两次应计）。
	prev, err := settlements.FindByRequest(ctx, q.RequestKey)
	if err != nil {
		return nil, 0, err
	}
	if prev != nil {
		return prev, settleReplayed, nil
	}

	enr, err := enrollments.FindOne(ctx, q.Mid)
	if err != nil {
		return nil, 0, err
	}
	if enr == nil {
		return nil, 0, fmt.Errorf("%w: mid=%d 从未参加计划，不出单", model.ErrEnrollmentNotFound, q.Mid)
	}
	if enr.State != model.EnrollmentStateEnrolled {
		if enr.State == model.EnrollmentStateSuspended {
			return nil, 0, fmt.Errorf("%w: mid=%d state=%d，暂停期间不出单",
				model.ErrEnrollmentSuspended, q.Mid, enr.State)
		}
		return nil, 0, fmt.Errorf("%w: mid=%d state=%d，只有 ENROLLED 出单",
			model.ErrNotEnrolled, q.Mid, enr.State)
	}

	agg, err := aggregateLedger(ctx, tx, q)
	if err != nil {
		return nil, 0, err
	}
	if strings.TrimSpace(agg.currency) == "" {
		// 规则未声明币种且 CreatorRevenue.DefaultCurrency 漏配：不能凭空挑一个币种写单号。
		return nil, 0, fmt.Errorf("%w: period=%s mid=%d 无法确定记账币种，"+
			"请检查规则 currency 与 CreatorRevenue.DefaultCurrency 配置",
			model.ErrRuleCurrencyMismatch, q.Period, q.Mid)
	}

	switch {
	case active == nil:
		rev, err := settlements.CountByPeriodMid(ctx, q.Period, q.Mid)
		if err != nil {
			return nil, 0, err
		}
		no := model.BuildSettlementNo(q.Period, q.Mid, rev+1)
		if err := fillItemNo(agg.items, no); err != nil {
			return nil, 0, err
		}
		if _, err := settlements.Insert(ctx, settlementRow(no, q, agg)); err != nil {
			return nil, 0, err
		}
		if err := items.InsertBatch(ctx, agg.items); err != nil {
			return nil, 0, err
		}
		row, err := settlements.FindOneByNo(ctx, no)
		if err != nil {
			return nil, 0, err
		}
		if row == nil {
			return nil, 0, fmt.Errorf("%w: %s 写入后读不到", model.ErrSettlementNotFound, no)
		}
		return row, settleCreated, nil

	case active.State == model.SettlementStateConfirmed:
		if !q.Force {
			// proto 口径：已有未作废单且本次未强制重算 → duplicated=true，零写入。
			// 已确认单金额冻结是「运营对外承诺过」的口径，不能被一次普通重算改掉。
			return active, settleDuplicated, nil
		}
		ok, err := settlements.Void(ctx, active.SettlementNo, model.SettlementStateConfirmed,
			trunc(q.Reason, model.MaxVoidReasonBytes))
		if err != nil {
			return nil, 0, err
		}
		if !ok {
			return nil, 0, fmt.Errorf("%w: 作废已确认单 %s 未命中（状态已变），请重试",
				model.ErrConcurrentUpdate, active.SettlementNo)
		}
		rev, err := settlements.CountByPeriodMid(ctx, q.Period, q.Mid)
		if err != nil {
			return nil, 0, err
		}
		no := model.BuildSettlementNo(q.Period, q.Mid, rev+1)
		if err := fillItemNo(agg.items, no); err != nil {
			return nil, 0, err
		}
		if _, err := settlements.Insert(ctx, settlementRow(no, q, agg)); err != nil {
			return nil, 0, err
		}
		if err := items.InsertBatch(ctx, agg.items); err != nil {
			return nil, 0, err
		}
		row, err := settlements.FindOneByNo(ctx, no)
		if err != nil {
			return nil, 0, err
		}
		if row == nil {
			return nil, 0, fmt.Errorf("%w: %s 写入后读不到", model.ErrSettlementNotFound, no)
		}
		return row, settleVoidedAndCreated, nil

	case active.State == model.SettlementStateDraft:
		ok, err := settlements.UpdateDraftAmounts(ctx, active.SettlementNo, agg.amountMinor,
			agg.capAppliedMinor, agg.metricCount, agg.currency, q.RequestKey)
		if err != nil {
			return nil, 0, err
		}
		if !ok {
			return nil, 0, fmt.Errorf("%w: 重算 DRAFT 单 %s 未命中（已被确认或作废），请重试",
				model.ErrSettlementNotDraft, active.SettlementNo)
		}
		// 分项先清后写：重算后某来源可能整组归零（例如台账被更正为 0），
		// 只改金额不删分项会让运营看到一笔「已经不存在的钱」。
		if err := items.DeleteByNo(ctx, active.SettlementNo); err != nil {
			return nil, 0, err
		}
		if err := fillItemNo(agg.items, active.SettlementNo); err != nil {
			return nil, 0, err
		}
		if err := items.InsertBatch(ctx, agg.items); err != nil {
			return nil, 0, err
		}
		row, err := settlements.FindOneByNo(ctx, active.SettlementNo)
		if err != nil {
			return nil, 0, err
		}
		if row == nil {
			return nil, 0, fmt.Errorf("%w: %s 重算后读不到", model.ErrSettlementNotFound, active.SettlementNo)
		}
		return row, settleRecomputed, nil

	default:
		// void_seq=0 的行只可能是 DRAFT/CONFIRMED；走到这里说明数据被绕过接口改坏。
		return nil, 0, fmt.Errorf("%w: 在效单 %s 的 state=%d 非法，禁止出单",
			model.ErrSettlementVoided, active.SettlementNo, active.State)
	}
}

// aggregateLedger 按来源锁读并聚合本周期该作者的台账。
//
// 金额一律取 capped_amount_minor（门槛与封顶已在计量写入时定案），
// 聚合阶段不再做任何二次折算——否则同一笔钱会有两套算法。
// capAppliedMinor 只累计「封顶扣掉的额度」，门槛拦掉的额度单独算，两者不能混：
// 运营看到 cap_applied_minor 时的判断是「要不要抬封顶」，不是「要不要降门槛」。
func aggregateLedger(
	ctx context.Context, tx sqlx.Session, q *settleQuery,
) (*settlementAgg, error) {
	metrics := model.NewRevenueMetricModel(sqlx.NewSqlConnFromSession(tx))
	rules := model.NewRevenueRuleModel(sqlx.NewSqlConnFromSession(tx))

	agg := &settlementAgg{currency: strings.TrimSpace(q.DefaultCurrency)}
	sourceTypes := make([]int32, 0, 4)
	for st := model.SourceTypeVipWatch; st <= model.SourceTypeActivity; st++ {
		sourceTypes = append(sourceTypes, st)
	}

	var (
		ruleCodes   []string
		seenRule    = map[string]bool{}
		currencies  = map[string]bool{}
		totalDeduct int64
	)
	for _, st := range sourceTypes {
		// ListGroupForUpdate 已经按 aid、metric_id 升序锁行：锁与读同一语句完成，
		// 不会出现「读完被别人改了再写」的窗口。
		rows, err := metrics.ListGroupForUpdate(ctx, q.Period, q.Mid, st)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			continue
		}
		var (
			quantity  int64
			capped    int64
			deduct    int64
			mainCode  string
			mainValue int64
		)
		for _, r := range rows {
			if r.RuleCode != "" && !seenRule[r.RuleCode] {
				seenRule[r.RuleCode] = true
				ruleCodes = append(ruleCodes, r.RuleCode)
			}
			quantity, err = addAmount(quantity, r.Quantity, "quantity 聚合")
			if err != nil {
				return nil, err
			}
			capped, err = addAmount(capped, r.CappedAmountMinor, "capped_amount_minor 聚合")
			if err != nil {
				return nil, err
			}
			agg.metricCount++
			if r.ThresholdBlocked != thresholdBlockedValue {
				// 封顶扣减 = 封顶前应计 − 封顶后应计；门槛拦掉的行不参与，
				// 它的差额在 source_detail 与 threshold_blocked 里自证。
				if r.AmountMinor >= r.CappedAmountMinor {
					deduct, err = addAmount(deduct, r.AmountMinor-r.CappedAmountMinor, "cap 扣减聚合")
					if err != nil {
						return nil, err
					}
				} else {
					// capped 大于 amount 只能是台账被绕过写入口改坏，绝不能让它进结算。
					return nil, fmt.Errorf("%w: metric_id=%d capped=%d 大于 amount=%d",
						model.ErrAmountOverflow, r.MetricId, r.CappedAmountMinor, r.AmountMinor)
				}
			}
			// 主规则取「本来源封顶后应计最高」的那条；并列时取 rule_code 字典序小者，
			// 保证同一份台账重算两次得到同一个分项说明。
			if r.CappedAmountMinor > mainValue ||
				(r.CappedAmountMinor == mainValue && mainCode != "" && r.RuleCode < mainCode) {
				mainValue = r.CappedAmountMinor
				mainCode = r.RuleCode
			}
		}
		totalDeduct, err = addAmount(totalDeduct, deduct, "cap 扣减合计")
		if err != nil {
			return nil, err
		}
		agg.amountMinor, err = addAmount(agg.amountMinor, capped, "amount_minor 合计")
		if err != nil {
			return nil, err
		}
		agg.items = append(agg.items, &model.SettlementItem{
			SourceType:  st,
			RuleCode:    mainCode,
			Quantity:    quantity,
			AmountMinor: capped,
		})
	}

	if agg.metricCount == 0 {
		// 没有台账就不出单：出一张 0 元单会把「这个月没数据」伪装成「这个月已结清」。
		return nil, fmt.Errorf("%w: period=%s mid=%d", model.ErrNoMetricsToSettle, q.Period, q.Mid)
	}
	agg.capAppliedMinor = totalDeduct

	sort.Strings(ruleCodes)
	for _, code := range ruleCodes {
		r, err := rules.FindByCode(ctx, code)
		if err != nil {
			return nil, err
		}
		if r == nil {
			// 规则行永不删除（就地更新 + ARCHIVED 终态），读不到说明数据被人为清理过。
			// 此时必须报错而不是按配置币种出单：单号一旦发出去就得能解释它的币种。
			return nil, fmt.Errorf("%w: 台账引用规则 %s，但规则行不存在，无法确定记账币种",
				model.ErrRuleNotFound, code)
		}
		if cur := strings.TrimSpace(r.Currency); cur != "" {
			currencies[cur] = true
		}
	}
	switch len(currencies) {
	case 0:
		// 台账引用的规则都没写币种（配置默认值兜底）：q.DefaultCurrency 已在
		// GenerateSettlement 入口校验非空，这里不再往代码里写死币种。
	case 1:
		for cur := range currencies {
			agg.currency = cur
		}
	default:
		list := make([]string, 0, len(currencies))
		for cur := range currencies {
			list = append(list, cur)
		}
		sort.Strings(list)
		return nil, fmt.Errorf("%w: period=%s mid=%d 的台账跨币种 %v，禁止合并成一单",
			model.ErrRuleCurrencyMismatch, q.Period, q.Mid, list)
	}
	return agg, nil
}

// settlementRow 组装结算单主体。
//
// payout_state 写入即 NOT_PAYABLE，且本服务没有任何代码路径改它：
// 出金（提现/打款/发票/税务/对账）不在范围内，「已确认」也只表示应计口径冻结，
// 不等于钱已出账（AGENTS.md §1 范围外能力不得假成功）。
func settlementRow(no string, q *settleQuery, agg *settlementAgg) *model.Settlement {
	return &model.Settlement{
		SettlementNo:    no,
		Period:          q.Period,
		Mid:             q.Mid,
		AmountMinor:     agg.amountMinor,
		CapAppliedMinor: agg.capAppliedMinor,
		Currency:        agg.currency,
		MetricCount:     agg.metricCount,
		State:           model.SettlementStateDraft,
		PayoutState:     model.PayoutStateNotPayable,
		RequestId:       q.RequestKey,
		VoidSeq:         0,
	}
}

// fillItemNo 把聚合出来的分项挂到具体单号上（分项在单号确定后才可写）。
func fillItemNo(rows []*model.SettlementItem, no string) error {
	for _, it := range rows {
		if strings.TrimSpace(it.RuleCode) == "" {
			return fmt.Errorf("%w: 单号 %s 来源 %d 的分项缺少主规则编码，台账脏数据",
				model.ErrMetricNotFound, no, it.SourceType)
		}
		it.SettlementNo = no
	}
	return nil
}
