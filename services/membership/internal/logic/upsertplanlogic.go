package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/membership/internal/config"
	"go-video/services/membership/internal/svc"
	"go-video/services/membership/model"
	"go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// upsertPlanLedgerReason 是 UpsertPlan 台账 reason 的缺省值：请求没填理由时用它兜底。
// 台账不能留空串，否则审计页分不清「没有理由」与「忘了写」。
const upsertPlanLedgerReason = "plan draft upsert (no reason provided)"

// planLedgerReason 运营填了理由就原样入台账，留空才回落到缺省摘要。
func planLedgerReason(reason string) string {
	if r := strings.TrimSpace(reason); r != "" {
		return r
	}
	return upsertPlanLedgerReason
}

type UpsertPlanLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertPlanLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertPlanLogic {
	return &UpsertPlanLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// UpsertPlan 运营面新建/修改套餐草稿。plan_code 已存在即更新，否则新建。
//
// 口径：
//  1. 新建一律落 DRAFT（state 只能由 SetPlanState 推进，这里不给「顺手上架」的通道）；
//  2. 已离开 DRAFT 的套餐不得改档位/时长/价格/币种（ErrPlanSpecImmutable）：
//     改价必须新建 DRAFT 版本再切换，否则会对已下单用户追溯生效；
//  3. expected_version 是 CAS 位，不匹配返回 ErrConcurrentUpdate 而不是覆盖别人的写入；
//  4. request_id 幂等：套餐写入与变更台账同一事务提交，命中重放回查首次结果返回；
//     同 request_id 换关键参数（指纹不一致）返回 ErrRequestIdReused，绝不静默改口径。
//     reason 不参与指纹：补一次理由不该被判定成换了套餐规格。
func (l *UpsertPlanLogic) UpsertPlan(in *rpc.UpsertPlanReq) (*rpc.PlanInfo, error) {
	cfg := l.svcCtx.Config.Membership
	if err := requireOperator(in.Operator); err != nil {
		return nil, err
	}
	if err := requireRequestId(in.RequestId); err != nil {
		return nil, err
	}
	if err := optionalReason(in.Reason); err != nil {
		return nil, err
	}

	code := strings.TrimSpace(in.PlanCode)
	if code == "" {
		return nil, model.ErrPlanCodeRequired
	}
	if err := checkLen("plan_code", code, model.MaxPlanCodeLength); err != nil {
		return nil, err
	}
	mask, draft, err := planDraftFromReq(in, cfg, code)
	if err != nil {
		return nil, err
	}

	existing, err := l.svcCtx.Plan.FindByCode(l.ctx, code)
	if err != nil {
		l.Errorf("membership/UpsertPlan: read plan_code=%s err=%v", code, err)
		return nil, err
	}
	if existing != nil && in.PlanId > 0 && in.PlanId != existing.PlanID {
		return nil, model.ErrPlanIdentifierMismatch
	}

	operator := strings.TrimSpace(in.Operator)
	fp := planRequestFingerprint(code, draft, in.ExpectedVersion)

	// 幂等重放优先于 CAS 预检（与 SetPlanState 同口径）：超时重试带着「当时那份参数」回来时，
	// 库里版本可能已经被首建推进过，先按 request_id 回到首次结果，
	// 否则调用方分不清「我的写失败了」和「别人也在写」。
	cl, err := l.svcCtx.PlanLog.FindByRequestID(l.ctx, in.RequestId)
	if err != nil {
		l.Errorf("membership/UpsertPlan: idempotency lookup request_id=%s err=%v", in.RequestId, err)
		return nil, err
	}
	if cl != nil {
		return l.replayFromLedger(cl, in.RequestId, fp)
	}

	if existing == nil {
		if in.PlanId > 0 {
			// 新建路径带了 plan_id：先分清「那行存在但 code 是别的」和「那行根本不存在」。
			// 前者是两个标识分家（调用方拿错码或拿错 ID），后者才是不存在「按 ID 改码」的语义。
			rival, rerr := l.svcCtx.Plan.FindOne(l.ctx, in.PlanId)
			if rerr != nil {
				l.Errorf("membership/UpsertPlan: read plan_id=%d err=%v", in.PlanId, rerr)
				return nil, rerr
			}
			if rival != nil {
				return nil, model.ErrPlanIdentifierMismatch
			}
			return nil, model.ErrPlanNotFound
		}
		return l.createPlan(code, draft, mask, operator, in, fp)
	}
	return l.updatePlan(existing, draft, mask, operator, in, fp)
}

// createPlan 新建草稿套餐：套餐行 + 变更台账同事务，任一处失败整体回滚。
func (l *UpsertPlanLogic) createPlan(code string, draft *model.Plan, mask uint32, operator string,
	in *rpc.UpsertPlanReq, fp string) (*rpc.PlanInfo, error) {

	plan := *draft
	plan.PlanCode = code
	plan.PlatformMask = mask
	plan.State = model.PlanStateDraft
	plan.CreatedBy = operator
	plan.UpdatedBy = operator

	err := l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, session sqlx.Session) error {
		id, err := l.svcCtx.Plan.InsertTx(ctx, session, &plan)
		if err != nil {
			return err
		}
		_, err = l.svcCtx.PlanLog.InsertTx(ctx, session, &model.PlanChangeLog{
			PlanID:            id,
			ChangeType:        model.PlanChangeUpsert,
			FromState:         0, // 0 = 尚无此行
			ToState:           plan.State,
			ToPriceMinor:      plan.PriceMinor,
			ToPromPriceMinor:  plan.PromPriceMinor,
			Operator:          operator,
			Reason:            planLedgerReason(in.Reason),
			RequestID:         in.RequestId,
			ParamsFingerprint: fp,
		})
		return err
	})
	if err != nil {
		if l.svcCtx.PlanLog.IsDuplicate(err) {
			return l.replay(code, in.RequestId, fp, err)
		}
		l.Errorf("membership/UpsertPlan: create plan_code=%s request_id=%s err=%v", code, in.RequestId, err)
		return nil, err
	}
	return planToRPC(&plan), nil
}

// updatePlan 修改既有套餐：只覆盖可改字段（展示与平台可见性），state 不由本接口动。
func (l *UpsertPlanLogic) updatePlan(existing *model.Plan, draft *model.Plan, mask uint32, operator string,
	in *rpc.UpsertPlanReq, fp string) (*rpc.PlanInfo, error) {

	if existing.State != model.PlanStateDraft && specChanged(existing, in) {
		return nil, model.ErrPlanSpecImmutable
	}

	next := *existing
	next.Name = draft.Name
	next.Description = draft.Description
	next.PlatformMask = mask
	next.AutoRenewSupported = draft.AutoRenewSupported
	next.UpdatedBy = operator
	if existing.State == model.PlanStateDraft {
		// 草稿阶段允许改档位/时长/价格；一旦上架就冻结（见 ErrPlanSpecImmutable）。
		next.VipType = draft.VipType
		next.DurationDays = draft.DurationDays
		next.UnitCount = draft.UnitCount
		next.PriceMinor = draft.PriceMinor
		next.PromPriceMinor = draft.PromPriceMinor
		next.Currency = draft.Currency
	}

	var applied bool
	err := l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, session sqlx.Session) error {
		ok, err := l.svcCtx.Plan.UpdateTx(ctx, session, &next, in.ExpectedVersion)
		if err != nil {
			return err
		}
		if !ok {
			return model.ErrConcurrentUpdate
		}
		if _, err := l.svcCtx.PlanLog.InsertTx(ctx, session, &model.PlanChangeLog{
			PlanID:             existing.PlanID,
			ChangeType:         model.PlanChangeUpsert,
			FromState:          existing.State,
			ToState:            next.State,
			FromPriceMinor:     existing.PriceMinor,
			ToPriceMinor:       next.PriceMinor,
			FromPromPriceMinor: existing.PromPriceMinor,
			ToPromPriceMinor:   next.PromPriceMinor,
			Operator:           operator,
			Reason:             planLedgerReason(in.Reason),
			RequestID:          in.RequestId,
			ParamsFingerprint:  fp,
		}); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		if l.svcCtx.PlanLog.IsDuplicate(err) {
			return l.replay(existing.PlanCode, in.RequestId, fp, err)
		}
		l.Errorf("membership/UpsertPlan: update plan_code=%s request_id=%s err=%v", existing.PlanCode, in.RequestId, err)
		return nil, err
	}
	if !applied {
		return nil, model.ErrConcurrentUpdate
	}

	// 回读而不是本地 +1：version/mtime 以库里的真实值为准，避免与并发写撞车后回显假版本。
	fresh, err := l.svcCtx.Plan.FindOne(l.ctx, existing.PlanID)
	if err != nil {
		l.Errorf("membership/UpsertPlan: reread plan_id=%d err=%v", existing.PlanID, err)
		return nil, err
	}
	return planToRPC(fresh), nil
}

// replay 处理 request_id 命中唯一索引的两种情况：
//  1. 台账里确实有这条 request_id → 比对指纹，一致则回查首次结果，不一致报冲突；
//  2. 台账里没有 → 说明 1062 来自 plan_code 唯一索引（并发新建），回查那行给调用方，
//     让它按「已存在」处理而不是收到一个含糊的写失败。
func (l *UpsertPlanLogic) replay(code, requestID, fp string, cause error) (*rpc.PlanInfo, error) {
	cl, err := l.svcCtx.PlanLog.FindByRequestID(l.ctx, requestID)
	if err != nil {
		l.Errorf("membership/UpsertPlan: replay lookup request_id=%s err=%v", requestID, err)
		return nil, err
	}
	if cl != nil {
		return l.replayFromLedger(cl, requestID, fp)
	}

	if code == "" {
		return nil, cause
	}
	p, err := l.svcCtx.Plan.FindByCode(l.ctx, code)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, cause
	}
	l.Infof("membership/UpsertPlan: plan_code=%s created concurrently, request_id=%s returns existing row", code, requestID)
	return planToRPC(p), nil
}

// replayFromLedger 台账里确实有这条 request_id：指纹一致才回放首次结果。
func (l *UpsertPlanLogic) replayFromLedger(cl *model.PlanChangeLog, requestID, fp string) (*rpc.PlanInfo, error) {
	if cl.ParamsFingerprint != fp {
		l.Errorf("membership/UpsertPlan: request_id=%s reused with different parameters (plan_id=%d)", requestID, cl.PlanID)
		return nil, model.ErrRequestIdReused
	}
	p, err := l.svcCtx.Plan.FindOne(l.ctx, cl.PlanID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		// 台账在、套餐行没了：数据被人工删过，不能伪造成功。
		return nil, model.ErrPlanNotFound
	}
	return planToRPC(p), nil
}

// planDraftFromReq 把请求体规整成可落库的草稿行（不含 code/state/操作者）。
// 返回 platform_mask 供上层写入。
func planDraftFromReq(in *rpc.UpsertPlanReq, cfg config.MembershipConf, code string) (uint32, *model.Plan, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return 0, nil, model.ErrPlanNameRequired
	}
	if err := checkLen("name", name, model.MaxPlanNameLength); err != nil {
		return 0, nil, err
	}
	desc := strings.TrimSpace(in.Description)
	if err := checkLen("description", desc, model.MaxPlanDescLength); err != nil {
		return 0, nil, err
	}
	if err := requireVipType(in.VipType); err != nil {
		return 0, nil, err
	}
	if in.DurationDays <= 0 {
		return 0, nil, fmt.Errorf("%w: duration_days must be positive", model.ErrInvalidPlanDuration)
	}
	unit := in.UnitCount
	if unit == 0 {
		// proto3 不给 0 与「未设置」的区分，月卡最常见的写法就是留空，按 1 个售卖单位处理。
		unit = 1
	}
	if unit < 0 {
		return 0, nil, fmt.Errorf("%w: unit_count=%d", model.ErrInvalidPlanDuration, in.UnitCount)
	}
	total := int64(in.DurationDays) * int64(unit)
	maxDays := int64(cfg.MaxGrantDeltaDays)
	if maxDays <= 0 {
		maxDays = 3660
	}
	if total > maxDays {
		return 0, nil, fmt.Errorf("%w: duration_days(%d)*unit_count(%d)=%d exceeds %d",
			model.ErrInvalidPlanDuration, in.DurationDays, unit, total, maxDays)
	}
	if in.PriceMinor < 0 || in.PromPriceMinor < 0 {
		return 0, nil, fmt.Errorf("%w: price_minor=%d prom_price_minor=%d", model.ErrInvalidPlanPrice, in.PriceMinor, in.PromPriceMinor)
	}
	if in.PromPriceMinor > 0 && in.PromPriceMinor >= in.PriceMinor {
		return 0, nil, fmt.Errorf("%w: prom_price_minor(%d) must be lower than price_minor(%d)",
			model.ErrInvalidPlanPrice, in.PromPriceMinor, in.PriceMinor)
	}
	currency := strings.TrimSpace(in.Currency)
	if currency == "" {
		currency = cfg.DefaultCurrency
	}
	if currency == "" {
		currency = "CNY"
	}
	// 价格以「分」计且币种显式落库：本轮只放开 CNY，其余币种没有兑换与结算口径，
	// 接受它们等于写进一条永远算不清的标价（AGENTS.md §1 资金语义）。
	if currency != "CNY" {
		return 0, nil, fmt.Errorf("%w: %s", model.ErrUnsupportedCurrency, currency)
	}
	if len(in.Platforms) == 0 {
		return 0, nil, model.ErrPlanPlatformsRequired
	}
	mask, err := planPlatformsFromRPC(in.Platforms)
	if err != nil {
		return 0, nil, err
	}
	if mask == 0 {
		return 0, nil, model.ErrPlanPlatformsRequired
	}
	return mask, &model.Plan{
		Name:               name,
		Description:        desc,
		VipType:            int32(in.VipType),
		DurationDays:       in.DurationDays,
		UnitCount:          unit,
		PriceMinor:         in.PriceMinor,
		PromPriceMinor:     in.PromPriceMinor,
		Currency:           currency,
		AutoRenewSupported: int32FromBool(in.AutoRenewSupported),
	}, nil
}

// specChanged 判定「离开 DRAFT 后是否试图改动档位/时长/价格/币种」。
func specChanged(existing *model.Plan, in *rpc.UpsertPlanReq) bool {
	unit := in.UnitCount
	if unit == 0 {
		unit = 1
	}
	currency := strings.TrimSpace(in.Currency)
	if currency == "" {
		currency = existing.Currency
	}
	return int32(in.VipType) != existing.VipType ||
		in.DurationDays != existing.DurationDays ||
		unit != existing.UnitCount ||
		in.PriceMinor != existing.PriceMinor ||
		in.PromPriceMinor != existing.PromPriceMinor ||
		currency != existing.Currency
}

// planRequestFingerprint 关键参数指纹：只覆盖「会改变结论」的字段，
// reason 不进指纹——同一请求补一次理由不该被判成换了口径。
func planRequestFingerprint(code string, p *model.Plan, expectedVersion int64) string {
	return model.Fingerprint(
		"UpsertPlan", code,
		p.Name, p.Description,
		fmt.Sprintf("%d", p.VipType), fmt.Sprintf("%d", p.DurationDays), fmt.Sprintf("%d", p.UnitCount),
		fmt.Sprintf("%d", p.PriceMinor), fmt.Sprintf("%d", p.PromPriceMinor), p.Currency,
		fmt.Sprintf("%d", p.AutoRenewSupported), fmt.Sprintf("%d", expectedVersion),
	)
}
