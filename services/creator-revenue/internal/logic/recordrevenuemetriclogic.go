package logic

import (
	"context"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type RecordRevenueMetricLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRecordRevenueMetricLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RecordRevenueMetricLogic {
	return &RecordRevenueMetricLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 写入/更正计量台账（cron、spm 回填或运营手工激励）
//
// 判定口径（完整推导见 metricmutate.go）：
//   - 闸门一：参与关系。只有 ENROLLED 可计量；SUSPENDED/LEFT/从未参加一律拒绝，
//     且判定押在 cr_enrollment 的行锁上（暂停与回填并发时不会有孤儿应计）；
//   - 闸门二：规则。rule_code 必须存在、锁行后仍为 ACTIVE、source_type 与请求一致、
//     周期起点不早于 effective_from；amount = quantity × 单价 / 1000（整数除法，
//     溢出报 ErrAmountOverflow，绝不回绕成负数），低于 min_quantity 则 capped=0 并置
//     threshold_blocked=1；
//   - 闸门三：周期。拒绝未来周期（ErrFuturePeriod）；本周期结算单已 CONFIRMED 时
//     台账冻结，回 ErrSettlementConfirmed，要改必须先作废重算；
//   - 幂等：唯一键 (period, mid, aid, source_type)。同值重放 → created/corrected 均 false
//     且不写变更台账；值不同 → 先写 cr_metric_change_log 留旧值，再就地更正（corrected=true）。
//     本表没有 request_id 列，所以重放判定押在「唯一键 + 同值」上，
//     request_id 只进变更台账供反查（已记入 README 契约缺口）。
//   - 封顶：写完后在同一事务里按 aid、metric_id 升序重分配 (period, mid, source_type)
//     组额度，保证同一批数据在任何写入顺序下得到相同结果。
//
// 本方法的 request_id 不落 cr_metric，因此不做「抢键 + 回滚 + 重放」那套规则台账范式；
// 重复调用的结论由上述同值判定给出，不会产生第二行台账或第二次应计。
func (l *RecordRevenueMetricLogic) RecordRevenueMetric(
	in *rpc.RecordRevenueMetricReq,
) (*rpc.RecordRevenueMetricReply, error) {
	if err := l.svcCtx.Ready(); err != nil {
		return nil, err
	}
	d, err := validateMetricUpsert(in)
	if err != nil {
		return nil, err
	}

	var (
		row     *model.RevenueMetric
		outcome metricOutcome
	)
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		r, o, err := applyMetricInTx(ctx, tx, d)
		if err != nil {
			return err
		}
		row, outcome = r, o
		return nil
	})
	if err != nil {
		l.Errorf("record revenue metric period=%s mid=%d aid=%d source_type=%d rule_code=%s "+
			"operator=%s request_id=%s: %v", d.Period, d.Mid, d.Aid, d.SourceType, d.RuleCode,
			d.Operator, d.RequestId, err)
		return nil, err
	}

	l.Infof("record revenue metric ok period=%s mid=%d aid=%d source_type=%d rule_code=%s "+
		"outcome=%d amount_minor=%d capped_minor=%d operator=%s request_id=%s",
		d.Period, d.Mid, d.Aid, d.SourceType, d.RuleCode, outcome, row.AmountMinor,
		row.CappedAmountMinor, d.Operator, d.RequestId)

	return &rpc.RecordRevenueMetricReply{
		Created:   outcome == metricCreated,
		Corrected: outcome == metricCorrected,
		Metric:    metricInfo(row),
	}, nil
}
