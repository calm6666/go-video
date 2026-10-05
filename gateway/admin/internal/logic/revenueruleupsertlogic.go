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

type RevenueRuleUpsertLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新建/修改分成规则草稿（无 state 位，改完不会自动生效；reason 必填）
func NewRevenueRuleUpsertLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevenueRuleUpsertLogic {
	return &RevenueRuleUpsertLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevenueRuleUpsert 转发 creator-revenue UpsertRevenueRule（写/改 DRAFT 规则草稿）。
//
// 门槛分三类，其余一个都不接管（§5 分成规则只属于 creator-revenue）：
//  1. 主体：operator 只能由会话渲染成 gateway/admin:<admin_id>；拿不到会话或 AdminID<=0
//     时在下传前 fail-closed，一次 RPC 都不发（表单自称的 operator 只作日志线索）。
//     created_by/updated_by 与 cr_rule_change 台账都落这一位，改单价必须可追责。
//  2. 幂等：idempotency_key → request_id 原值，不生成、不改写（改一个字符就丢幂等语义）。
//     命中首键时服务回**首次结论**（resolveRuleReplay），那是成功而不是错误；
//     本契约的 UpsertRevenueRule 响应就是 RevenueRuleInfo，**没有 duplicated 位**
//     （已作为契约缺口上报），网关也不会自己造一位。
//     同一 request_id 参数不同 → ErrRequestIDConflict / ErrVersionConflict 原样上抛，
//     绝不换个号或补 expected_version 再打一次（那等于把一次冲突变成两条规则变更）。
//  3. 不可能形状：rule_id 为负、source_type 未填（revenueRuleSourceType）、
//     min_quantity/monthly_cap_minor/effective_from/expected_version 为负、
//     rule_code/name/unit/reason/idempotency_key 缺空。
//
// 刻意**不下判断**的（都归服务，网关自己做一遍就会和服务口径漂移）：
//   - currency 允许留空：服务按 DefaultCurrency 补齐（rulereplay.go），网关若预先拒空
//     就把这条默认路径堵死了（admin.api 把它标成必填但没写这一层，见契约缺口）；
//   - 「单价不能为负」由服务判（ErrNegativeUnitPrice），.api 也明写「负数由服务拒」，
//     因此这一位**不挡**、原样下传；护栏上限 MaxRuleUnitPricePer1000Minor、
//     MaxMonthlyCapMinor 只有服务知道配置值；
//   - rule_code 唯一性（ErrRuleCodeConflict）、「只有 DRAFT 能改」（ErrRuleNotDraft）、
//     expected_version 的 CAS 结论、生效起点与历史周期的关系，全在服务侧；
//   - 表单没有 state 位是**契约设计**（改草稿与让规则生效是两个权限点），网关不补默认值、
//     也不「顺手」调 SetRevenueRuleState 让它生效——那会把一次调价变成一次生效。
func (l *RevenueRuleUpsertLogic) RevenueRuleUpsert(req *types.ParamRevenueRuleUpsert) (resp *types.RevenueRuleUpsertResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errRevenueServiceNotConfigured
	}
	if req == nil {
		return nil, errRevenueRequestMissing
	}
	operator, err := revenueOperator(l.ctx, "revenueRuleUpsert", req.Operator)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("rule_code", req.RuleCode); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("name", req.Name); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("unit", req.Unit); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := revenueNonNeg("rule_id", req.RuleId); err != nil {
		return nil, err
	}
	if err := revenueRuleSourceType(req.SourceType); err != nil {
		return nil, err
	}
	if err := revenueNonNeg("min_quantity", req.MinQuantity); err != nil {
		return nil, err
	}
	if err := revenueNonNeg("monthly_cap_minor", req.MonthlyCapMinor); err != nil {
		return nil, err
	}
	if err := revenueNonNeg("effective_from", req.EffectiveFrom); err != nil {
		return nil, err
	}
	if err := revenueNonNeg("expected_version", req.ExpectedVersion); err != nil {
		return nil, err
	}
	rule, err := l.svcCtx.CreatorRevenue.UpsertRevenueRule(l.ctx, &creatorrevenuerpc.UpsertRevenueRuleReq{
		RuleId:                 req.RuleId,
		RuleCode:               req.RuleCode,
		SourceType:             creatorrevenuerpc.RevenueSourceType(req.SourceType),
		Name:                   req.Name,
		Description:            req.Description,
		UnitPricePer_1000Minor: req.UnitPricePer1000Minor,
		Currency:               req.Currency,
		Unit:                   req.Unit,
		MinQuantity:            req.MinQuantity,
		MonthlyCapMinor:        req.MonthlyCapMinor,
		EffectiveFrom:          req.EffectiveFrom,
		ExpectedVersion:        req.ExpectedVersion,
		Operator:               operator,
		RequestId:              req.IdempotencyKey,
		Reason:                 req.Reason,
	})
	if err != nil {
		// trace_id 只进日志（UpsertRevenueRuleReq 没有该字段可下传）；
		// 单价与档位原样记进日志，reason 正文不进日志（§7）。
		l.Errorf("gateway/admin/revenueRuleUpsert: rule_id=%d rule_code=%q source_type=%d unit_price_per_1000_minor=%d operator=%s trace_id=%s err=%v",
			req.RuleId, req.RuleCode, req.SourceType, req.UnitPricePer1000Minor, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/revenueRuleUpsert: rule_code=%q source_type=%d state=%d version=%d operator=%s",
		rule.GetRuleCode(), rule.GetSourceType(), rule.GetState(), rule.GetVersion(), operator)
	return &types.RevenueRuleUpsertResponse{
		Code:    0,
		Message: "ok",
		Data: types.RevenueRuleUpsertData{
			Rule: revenueRuleToAPI(rule),
		},
		TTL: 0,
	}, nil
}
