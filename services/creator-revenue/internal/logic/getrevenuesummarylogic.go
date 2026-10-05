package logic

import (
	"context"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetRevenueSummaryLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetRevenueSummaryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRevenueSummaryLogic {
	return &GetRevenueSummaryLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// payoutNoteText 是创作者端的固定文案位：本项目没有出金通道，
// 前端据此显示「暂不可提现」，不能让客户端自己猜（AGENTS.md §6 不在服务端写死客户端 UI，
// 但结论必须由服务端给出，否则不同端会各自解读 payout_available=false）。
const payoutNoteText = "当前版本未接入提现与打款通道，此处金额均为应计金额（分），不代表已到账"

// 创作者端收益概览
//
// 判定口径：
//   - mid 必填且为正（分成人必须是真实账号）；
//   - 参与状态：从未参加 → 投影 state=UNSPECIFIED（proto 注释：found=false 的语义由
//     enrollment.state 表达），不当错误、也不回一个空壳当作「已参加」；
//   - current_estimate_minor = 当前周期 cr_metric 的 capped_amount_minor 合计，
//     口径是「未结算的预估」，会随台账更正变动，且**未做月度封顶以外的任何再折算**；
//     周期未收官，所以这个数天然偏低，客户端不能显示成「本月已赚」；
//   - total_confirmed_minor = 已 CONFIRMED 结算单的应计合计，窗口由
//     CreatorRevenue.SummaryRecentPeriods 决定（含当前周期，默认回看 12 个月；
//     0 表示全历史）。契约里这个字段没有暴露窗口的位，属已知缺口（见 README），
//     要精确全历史请把该配置项设为 0；
//   - last_settled_period = 最近一张**在效**单（DRAFT 或 CONFIRMED）的周期数值化，
//     0 表示从未出单；已作废的单不算数，否则会把「被推翻的重算」显示成「已结算」；
//   - payout_available 恒 false：出金不在本期范围，任何情况下都不回 true；
//   - 读侧不做缓存回写：CacheRedis 只是加速位，台账真值恒在 MySQL，
//     缓存不可用一律回源（见 ServiceContext 注释）。
func (l *GetRevenueSummaryLogic) GetRevenueSummary(
	in *rpc.GetRevenueSummaryReq,
) (*rpc.GetRevenueSummaryReply, error) {
	if in == nil {
		in = &rpc.GetRevenueSummaryReq{}
	}
	if err := l.svcCtx.Ready(); err != nil {
		return nil, err
	}
	mid, err := normalizeMid(in.Mid)
	if err != nil {
		return nil, err
	}
	current := model.CurrentPeriod()

	enr, err := l.svcCtx.Enrollments.FindOne(l.ctx, mid)
	if err != nil {
		l.Errorf("GetRevenueSummary 读取参与关系失败 mid=%d: %v", mid, err)
		return nil, err
	}
	enrollment := enrollmentInfo(enr)
	if enrollment == nil {
		// 从未参加：显式投影一个只有 mid + UNSPECIFIED 的壳，而不是 nil，
		// 让客户端能区分「这个人没参加计划」与「查询没带回来」。
		enrollment = &rpc.EnrollmentInfo{
			Mid:   mid,
			State: rpc.EnrollmentState_ENROLLMENT_STATE_UNSPECIFIED,
		}
	}

	estimate, err := l.svcCtx.Metrics.SumCappedByPeriod(l.ctx, current, mid)
	if err != nil {
		l.Errorf("GetRevenueSummary 本月预估聚合失败 mid=%d period=%s: %v", mid, current, err)
		return nil, err
	}

	cutoff, err := model.PeriodCutoff(current, l.svcCtx.Config.CreatorRevenue.SummaryRecentPeriods)
	if err != nil {
		l.Errorf("GetRevenueSummary 统计窗口计算失败 mid=%d period=%s: %v", mid, current, err)
		return nil, err
	}
	totalConfirmed, err := l.svcCtx.Settlements.SumConfirmed(l.ctx, mid, cutoff)
	if err != nil {
		l.Errorf("GetRevenueSummary 已确认合计失败 mid=%d cutoff=%s: %v", mid, cutoff, err)
		return nil, err
	}

	lastPeriod, err := l.svcCtx.Settlements.LatestActivePeriod(l.ctx, mid)
	if err != nil {
		l.Errorf("GetRevenueSummary 最近出单周期读取失败 mid=%d: %v", mid, err)
		return nil, err
	}

	return &rpc.GetRevenueSummaryReply{
		Enrollment:           enrollment,
		CurrentPeriod:        current,
		CurrentEstimateMinor: estimate,
		TotalConfirmedMinor:  totalConfirmed,
		LastSettledPeriod:    periodToNumber(lastPeriod),
		PayoutAvailable:      false,
		PayoutNote:           payoutNoteText,
	}, nil
}
