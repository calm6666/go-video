package logic

// 运营面三个写接口（UpsertPlan / SetPlanState / UpsertEntitlement）+ SetAutoRenew 的流程口径测试。
//
// 纯函数校验表在 validation_test.go（planDraftFromReq / specChanged / canMovePlanState /
// planRequestFingerprint / validEntitlementCode / normalizeAutoRenewChannel 都已逐条钉住），
// 这里只测「校验之后发生了什么」：
//   - 套餐只能落 DRAFT，state 由 SetPlanState 单向推进；改价必须先建新草稿再切换；
//   - 已离开 DRAFT 的档位/时长/价格/币种不得被追溯改动（ErrPlanSpecImmutable），且拒绝时零写入；
//   - 套餐行与变更台账同事务：CAS 未命中 / 台账唯一键冲突都整体回滚；
//   - request_id 幂等：指纹一致才重放，换参数一律 ErrRequestIdReused；reason 不入指纹；
//   - 权益码不可改（UPDATE 列表里没有 code），下架不删行；
//   - 目录读/台账读失败必须上抛，不许被折叠成「不存在 / 可重放」。

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"go-video/services/membership/model"
	"go-video/services/membership/rpc"
)

const (
	planOp        = "ops-plan-1"
	planReason    = "双十一前调价"
	entOp         = "ops-catalog-1"
	sandboxString = model.AllowedAutoRenewChannel
)

// --- 夹具 ---

func planReq(code string) *rpc.UpsertPlanReq {
	return &rpc.UpsertPlanReq{
		PlanCode: code, Name: "大会员月卡", Description: "1080P 高码率 + 去广告",
		VipType: rpc.VipType_VIP_TYPE_PREMIUM, DurationDays: 31, UnitCount: 1,
		PriceMinor: 2500, Currency: "CNY",
		Platforms: []rpc.PlanPlatform{rpc.PlanPlatform_PLAN_PLATFORM_ANDROID, rpc.PlanPlatform_PLAN_PLATFORM_IOS},
		Operator:  planOp, RequestId: "p-1", Reason: planReason,
	}
}

func stateReq(planID int64, to rpc.PlanSaleState, version int64, requestID string) *rpc.SetPlanStateReq {
	return &rpc.SetPlanStateReq{
		PlanId: planID, TargetState: to, ExpectedVersion: version,
		Operator: planOp, RequestId: requestID, Reason: "上架给全量用户",
	}
}

func entReq(code string) *rpc.UpsertEntitlementReq {
	return &rpc.UpsertEntitlementReq{
		Code: code, Name: "高码率播放", Description: "1080P 及以上",
		MinVipType: rpc.VipType_VIP_TYPE_PREMIUM, Enabled: true,
		Operator: entOp, RequestId: "e-1",
	}
}

func autoRenewReq(mid int64, on bool, channel, requestID string) *rpc.SetAutoRenewReq {
	return &rpc.SetAutoRenewReq{
		Mid: mid, VipType: rpc.VipType_VIP_TYPE_PREMIUM, On: on, Channel: channel,
		Operator: "user-self", RequestId: requestID,
	}
}

// expectPlanFingerprint 用被测代码自己的指纹函数造「应当一致」的台账行，
// 这样测试钉的是「一致就重放 / 不一致就冲突」，而不是某个哈希值。
func expectPlanFingerprint(t *testing.T, in *rpc.UpsertPlanReq, code string) string {
	t.Helper()
	_, draft, err := planDraftFromReq(in, testConf(), code)
	if err != nil {
		t.Fatalf("夹具请求本身就不过校验：%v", err)
	}
	return planRequestFingerprint(code, draft, in.ExpectedVersion)
}

// onSalePlan 造一行「已离开 DRAFT」的在售套餐，规格与 planReq("monthly_on_sale") 对齐，
// 这样测试里的 in 只要照抄 before 的规格字段就能只改展示项；unit_count 取 2 而不是 1，
// 是为了让「请求留空 unit_count」在归一化成 1 之后真的与库里不符（而不是恰好相等而漏判）。
func onSalePlan(id int64) *model.Plan {
	return &model.Plan{
		PlanID:       id,
		PlanCode:     "monthly_on_sale",
		Name:         "大会员月卡",
		Description:  "1080P 高码率 + 去广告",
		VipType:      model.VipTypePremium,
		DurationDays: 31,
		UnitCount:    2,
		PriceMinor:   2500,
		Currency:     "CNY",
		PlatformMask: model.PlatformBitAndroid | model.PlatformBitIOS,
		State:        model.PlanStateOnSale,
		CreatedBy:    planOp,
		UpdatedBy:    planOp,
	}
}

// --- UpsertPlan ---

// TestUpsertPlanAlwaysCreatesDraft 新建只能落 DRAFT：
// 本接口没有「顺手上架」的通道，上架必须走 SetPlanState 留痕。
func TestUpsertPlanAlwaysCreatesDraft(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	in := planReq("monthly_premium")
	in.ExpectedVersion = 9 // 新建时调用方乱填 CAS 位也必须被忽略（此时还没有行可比）

	got, err := NewUpsertPlanLogic(context.Background(), svcCtx).UpsertPlan(in)
	mustNoError(t, err, "UpsertPlan 新建")
	if got.GetState() != rpc.PlanSaleState_PLAN_SALE_STATE_DRAFT {
		t.Errorf("state = %v，期望 DRAFT", got.GetState())
	}
	if got.GetVersion() != 1 {
		t.Errorf("version = %d，期望首行为 1", got.GetVersion())
	}
	if got.GetPlanId() == 0 || got.GetPlanCode() != "monthly_premium" {
		t.Errorf("回填标识不完整：%+v", got)
	}
	if len(got.GetPlatforms()) != 2 {
		t.Errorf("平台数 = %d，期望 2（掩码回读丢失）", len(got.GetPlatforms()))
	}
	if got.GetCurrency() != "CNY" {
		t.Errorf("currency = %s，期望显式落库 CNY", got.GetCurrency())
	}
	if got.GetUnitCount() != 1 {
		t.Errorf("unit_count = %d，期望留空按 1 处理", got.GetUnitCount())
	}

	row := db.plans[got.GetPlanId()]
	if row.State != model.PlanStateDraft || row.CreatedBy != planOp || row.UpdatedBy != planOp {
		t.Errorf("落库行不符：%+v", row)
	}
	if len(db.planLogs) != 1 {
		t.Fatalf("台账 %d 条，期望与套餐行同时写入 1 条", len(db.planLogs))
	}
	lg := db.planLogs["p-1"]
	if lg.ChangeType != model.PlanChangeUpsert || lg.FromState != 0 || lg.ToState != model.PlanStateDraft {
		t.Errorf("台账状态不符：type=%s %d->%d", lg.ChangeType, lg.FromState, lg.ToState)
	}
	if lg.ToPriceMinor != 2500 || lg.FromPriceMinor != 0 {
		t.Errorf("台账价格不符：from=%d to=%d", lg.FromPriceMinor, lg.ToPriceMinor)
	}
	if lg.PlanID != got.GetPlanId() || lg.Operator != planOp {
		t.Errorf("台账归属不符：plan_id=%d operator=%s", lg.PlanID, lg.Operator)
	}
	if lg.ParamsFingerprint == "" || len(lg.ParamsFingerprint) != 64 {
		t.Errorf("指纹长度 %d（CHAR(64) 列），实值 %q", len(lg.ParamsFingerprint), lg.ParamsFingerprint)
	}
	// 目录写入不得顺手碰会员身份与时长台账。
	if len(db.members) != 0 || len(db.grants) != 0 {
		t.Errorf("建套餐却写了身份/时长台账：%d/%d", len(db.members), len(db.grants))
	}
}

// TestUpsertPlanLedgerReasonFallback 空理由落缺省摘要、填了原样入台账（去首尾空白）：
// 审计页要能分清「没理由」与「忘了写理由」。
func TestUpsertPlanLedgerReasonFallback(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   string
	}{
		{"留空", "", upsertPlanLedgerReason},
		{"全空白", "   \t ", upsertPlanLedgerReason},
		{"填了", "  " + planReason + "  ", planReason},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			in := planReq("fallback_" + string(rune('a'+i)))
			in.Reason = c.reason
			if _, err := NewUpsertPlanLogic(context.Background(), svcCtx).UpsertPlan(in); err != nil {
				t.Fatalf("UpsertPlan：%v", err)
			}
			if got := db.planLogs[in.RequestId].Reason; got != c.want {
				t.Errorf("台账 reason = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestUpsertPlanIdentifierRules 标识一致性：plan_id 与 plan_code 不能指向两行，
// 给了 plan_id 却查无此行也不存在「按 ID 改码」的语义。两种都要在起事务之前拒掉。
func TestUpsertPlanIdentifierRules(t *testing.T) {
	t.Run("两个标识指向不同套餐", func(t *testing.T) {
		svcCtx, db := newTestSvc(t)
		seedPlan(db, &model.Plan{PlanID: 10, PlanCode: "monthly_a", Name: "A", VipType: model.VipTypePremium,
			DurationDays: 31, UnitCount: 1, PriceMinor: 2500, Currency: "CNY", State: model.PlanStateDraft})
		in := planReq("monthly_b")
		in.PlanId = 10
		_, err := NewUpsertPlanLogic(context.Background(), svcCtx).UpsertPlan(in)
		mustErrIs(t, err, model.ErrPlanIdentifierMismatch, "plan_id 与 plan_code 分家")
		if db.txRuns != 0 {
			t.Errorf("标识冲突却起了 %d 个事务", db.txRuns)
		}
	})

	t.Run("plan_id 查无此行", func(t *testing.T) {
		svcCtx, _ := newTestSvc(t)
		in := planReq("brand_new_code")
		in.PlanId = 88
		_, err := NewUpsertPlanLogic(context.Background(), svcCtx).UpsertPlan(in)
		mustErrIs(t, err, model.ErrPlanNotFound, "按 ID 改码")
	})

	t.Run("code 必填", func(t *testing.T) {
		svcCtx, _ := newTestSvc(t)
		in := planReq("   ")
		_, err := NewUpsertPlanLogic(context.Background(), svcCtx).UpsertPlan(in)
		mustErrIs(t, err, model.ErrPlanCodeRequired, "空白 plan_code")
	})
}

// TestUpsertPlanDraftCanReprice 草稿阶段是唯一的定价窗口：改价要留痕、版本要前移、
// 回读的是库里的真版本而不是本地 +1。
func TestUpsertPlanDraftCanReprice(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	plan := seedPlan(db, &model.Plan{PlanID: 20, PlanCode: "monthly_cny", Name: "大会员月卡",
		VipType: model.VipTypePremium, DurationDays: 31, UnitCount: 1, PriceMinor: 2500, Currency: "CNY",
		PlatformMask: model.PlatformBitAndroid, State: model.PlanStateDraft})

	in := planReq("monthly_cny")
	in.PlanId = plan.PlanID
	in.ExpectedVersion = plan.Version
	in.PriceMinor = 1990
	in.Reason = "草稿改价"

	got, err := NewUpsertPlanLogic(context.Background(), svcCtx).UpsertPlan(in)
	mustNoError(t, err, "草稿改价")
	if got.GetPriceMinor() != 1990 || got.GetState() != rpc.PlanSaleState_PLAN_SALE_STATE_DRAFT {
		t.Errorf("回显不符：price=%d state=%v", got.GetPriceMinor(), got.GetState())
	}
	if got.GetVersion() != plan.Version+1 {
		t.Errorf("回显 version = %d，期望回读库里的 %d", got.GetVersion(), plan.Version+1)
	}
	lg := db.planLogs[in.RequestId]
	if lg == nil {
		t.Fatal("改价没留台账")
	}
	if lg.FromPriceMinor != 2500 || lg.ToPriceMinor != 1990 || lg.FromState != model.PlanStateDraft {
		t.Errorf("台账价格/状态不符：from=%d to=%d from_state=%d", lg.FromPriceMinor, lg.ToPriceMinor, lg.FromState)
	}
}

// TestUpsertPlanSpecFrozenAfterLeavingDraft 钉住 ErrPlanSpecImmutable：
// 已上架套餐改价会对「按原价下单的用户」追溯生效，只能新建 DRAFT 版本再切换。
func TestUpsertPlanSpecFrozenAfterLeavingDraft(t *testing.T) {
	frozen := []struct {
		name   string
		mutate func(*rpc.UpsertPlanReq)
	}{
		{"改原价", func(in *rpc.UpsertPlanReq) { in.PriceMinor = 1990 }},
		{"改促销价", func(in *rpc.UpsertPlanReq) { in.PromPriceMinor = 990 }},
		{"改档位", func(in *rpc.UpsertPlanReq) { in.VipType = rpc.VipType_VIP_TYPE_PREMIUM_PLUS }},
		{"改时长", func(in *rpc.UpsertPlanReq) { in.DurationDays = 62 }},
		{"改售卖单位", func(in *rpc.UpsertPlanReq) { in.UnitCount = 3 }},
		{"留空 unit_count", func(in *rpc.UpsertPlanReq) { in.UnitCount = 0 }}, // 归一化成 1，与库里 2 不符
	}
	for _, c := range frozen {
		t.Run(c.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			before := seedPlan(db, onSalePlan(30))
			in := planReq("monthly_on_sale")
			in.PlanId = before.PlanID
			in.ExpectedVersion = before.Version
			in.DurationDays, in.UnitCount, in.PriceMinor = before.DurationDays, before.UnitCount, before.PriceMinor
			in.VipType = rpc.VipType(before.VipType)
			in.PromPriceMinor = before.PromPriceMinor
			c.mutate(in)

			_, err := NewUpsertPlanLogic(context.Background(), svcCtx).UpsertPlan(in)
			mustErrIs(t, err, model.ErrPlanSpecImmutable, "改冻结字段")
			if db.txRuns != 0 || len(db.planLogs) != 0 {
				t.Errorf("拒改冻结字段却写了东西：事务 %d 次、台账 %d 条", db.txRuns, len(db.planLogs))
			}
			after := db.plans[before.PlanID]
			if *after != *before {
				t.Errorf("拒改之后行还变了：%+v -> %+v", before, after)
			}
		})
	}

	// 同一份冻结规格下，只改展示字段必须放行——否则「修个错别字」也要新建套餐。
	t.Run("只改展示字段放行", func(t *testing.T) {
		svcCtx, db := newTestSvc(t)
		before := seedPlan(db, onSalePlan(31))
		in := planReq("monthly_on_sale")
		in.PlanId = before.PlanID
		in.ExpectedVersion = before.Version
		in.DurationDays, in.UnitCount, in.PriceMinor = before.DurationDays, before.UnitCount, before.PriceMinor
		in.VipType = rpc.VipType(before.VipType)
		in.PromPriceMinor = before.PromPriceMinor
		in.Name = "大会员月卡（改名）"
		in.Description = ""
		in.Platforms = []rpc.PlanPlatform{rpc.PlanPlatform_PLAN_PLATFORM_WEB}

		got, err := NewUpsertPlanLogic(context.Background(), svcCtx).UpsertPlan(in)
		mustNoError(t, err, "改展示字段")
		if got.GetState() != rpc.PlanSaleState_PLAN_SALE_STATE_ON_SALE {
			t.Errorf("改展示字段把 state 带跑了：%v", got.GetState())
		}
		row := db.plans[before.PlanID]
		if row.Name != "大会员月卡（改名）" || row.PriceMinor != before.PriceMinor ||
			row.DurationDays != before.DurationDays || row.Currency != before.Currency {
			t.Errorf("展示字段更新连带改了规格：%+v", row)
		}
	})
}

// TestUpsertPlanStateNotChangedHere 本接口不得改 state：即便请求把在售套餐的字段照抄一遍。
func TestUpsertPlanCannotChangeState(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	before := seedPlan(db, onSalePlan(32))
	in := planReq("monthly_on_sale")
	in.PlanId = before.PlanID
	in.ExpectedVersion = before.Version
	in.DurationDays, in.UnitCount, in.PriceMinor = before.DurationDays, before.UnitCount, before.PriceMinor
	in.VipType = rpc.VipType(before.VipType)
	if _, err := NewUpsertPlanLogic(context.Background(), svcCtx).UpsertPlan(in); err != nil {
		t.Fatalf("UpsertPlan：%v", err)
	}
	if db.plans[before.PlanID].State != model.PlanStateOnSale {
		t.Errorf("UpsertPlan 改了 state：%d", db.plans[before.PlanID].State)
	}
	if lg := db.planLogs[in.RequestId]; lg.ToState != model.PlanStateOnSale || lg.ChangeType != model.PlanChangeUpsert {
		t.Errorf("台账没记真实状态：to_state=%d type=%s", lg.ToState, lg.ChangeType)
	}
}

// TestUpsertPlanCasMissRollsBackLedger 版本号过期：套餐行与台账一起回滚，
// 不能留下「有台账没改价」这种审计噪声。
func TestUpsertPlanCasMissRollsBackLedger(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	plan := seedPlan(db, &model.Plan{PlanID: 40, PlanCode: "monthly_cas", Name: "月卡",
		VipType: model.VipTypePremium, DurationDays: 31, UnitCount: 1, PriceMinor: 2500, Currency: "CNY",
		State: model.PlanStateDraft, Version: 7})
	in := planReq("monthly_cas")
	in.PlanId = plan.PlanID
	in.ExpectedVersion = plan.Version - 1 // 别人已经改过一轮

	_, err := NewUpsertPlanLogic(context.Background(), svcCtx).UpsertPlan(in)
	mustErrIs(t, err, model.ErrConcurrentUpdate, "过期版本号")
	if len(db.planLogs) != 0 {
		t.Errorf("CAS 未命中仍留下 %d 条台账", len(db.planLogs))
	}
	after := db.plans[plan.PlanID]
	if after.PriceMinor != 2500 || after.Version != 7 {
		t.Errorf("CAS 未命中却改了行：price=%d version=%d", after.PriceMinor, after.Version)
	}
}

// TestUpsertPlanReplayReturnsFirstResult 幂等键命中且指纹一致：回查首次结果，
// 且这次绝不能二次推进版本（事务已整体回滚）。
func TestUpsertPlanReplayReturnsFirstResult(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	plan := seedPlan(db, &model.Plan{PlanID: 50, PlanCode: "monthly_replay", Name: "月卡",
		VipType: model.VipTypePremium, DurationDays: 31, UnitCount: 1, PriceMinor: 2500, Currency: "CNY",
		State: model.PlanStateDraft, Version: 1})
	in := planReq("monthly_replay")
	in.PlanId = plan.PlanID
	in.ExpectedVersion = plan.Version
	in.Name = "大会员月卡"

	seedPlanLog(db, &model.PlanChangeLog{
		PlanID: plan.PlanID, ChangeType: model.PlanChangeUpsert,
		FromState: model.PlanStateDraft, ToState: model.PlanStateDraft,
		Operator: planOp, RequestID: in.RequestId, ParamsFingerprint: expectPlanFingerprint(t, in, "monthly_replay"),
	})

	got, err := NewUpsertPlanLogic(context.Background(), svcCtx).UpsertPlan(in)
	mustNoError(t, err, "指纹一致的幂等重放")
	if got.GetPlanId() != plan.PlanID {
		t.Errorf("重放回了别的套餐：%d", got.GetPlanId())
	}
	if db.plans[plan.PlanID].Version != 1 {
		t.Errorf("重放把 version 推进到 %d，期望仍是 1", db.plans[plan.PlanID].Version)
	}
	if len(db.planLogs) != 1 {
		t.Errorf("重放又记了台账：现有 %d 条", len(db.planLogs))
	}
}

// TestUpsertPlanReplayRejectsChangedSpec 同一幂等键换关键参数必须报冲突，
// 而且换 reason 不算换参数（reason 不入指纹）。
func TestUpsertPlanReplayRejectsChangedSpec(t *testing.T) {
	t.Run("改规格借旧键", func(t *testing.T) {
		svcCtx, db := newTestSvc(t)
		seedPlan(db, &model.Plan{PlanID: 60, PlanCode: "monthly_conflict", Name: "大会员月卡",
			Description: "1080P 高码率 + 去广告",
			VipType:     model.VipTypePremium, DurationDays: 31, UnitCount: 1, PriceMinor: 2500, Currency: "CNY",
			State:        model.PlanStateDraft,
			PlatformMask: model.PlatformBitAndroid | model.PlatformBitIOS})
		// 首次提交的那份参数（PriceMinor 仍是 2500），指纹按它记账。
		first := planReq("monthly_conflict")
		first.PlanId = 60
		first.ExpectedVersion = 1
		in := planReq("monthly_conflict")
		in.PlanId = 60
		in.ExpectedVersion = 1
		seedPlanLog(db, &model.PlanChangeLog{PlanID: 60, ChangeType: model.PlanChangeUpsert,
			Operator: planOp, RequestID: in.RequestId, ParamsFingerprint: expectPlanFingerprint(t, first, "monthly_conflict")})

		in.PriceMinor = 990 // 借旧 request_id 改价
		_, err := NewUpsertPlanLogic(context.Background(), svcCtx).UpsertPlan(in)
		mustErrIs(t, err, model.ErrRequestIdReused, "借幂等键改规格")
		if db.plans[60].PriceMinor != 2500 {
			t.Errorf("冲突请求改了价格：%d", db.plans[60].PriceMinor)
		}
	})

	t.Run("只补理由不算换参数", func(t *testing.T) {
		svcCtx, db := newTestSvc(t)
		plan := seedPlan(db, &model.Plan{PlanID: 61, PlanCode: "monthly_reason", Name: "月卡",
			VipType: model.VipTypePremium, DurationDays: 31, UnitCount: 1, PriceMinor: 2500, Currency: "CNY",
			State:        model.PlanStateDraft,
			PlatformMask: model.PlatformBitAndroid | model.PlatformBitIOS})
		in := planReq("monthly_reason")
		in.PlanId = plan.PlanID
		in.ExpectedVersion = plan.Version
		in.Reason = ""
		seedPlanLog(db, &model.PlanChangeLog{PlanID: plan.PlanID, ChangeType: model.PlanChangeUpsert,
			Operator: planOp, RequestID: in.RequestId, ParamsFingerprint: expectPlanFingerprint(t, in, "monthly_reason")})

		in.Reason = "补一条理由"
		if _, err := NewUpsertPlanLogic(context.Background(), svcCtx).UpsertPlan(in); err != nil {
			t.Fatalf("补理由被判成换参数：%v", err)
		}
		if db.plans[plan.PlanID].Version != plan.Version {
			t.Errorf("重放却推进了版本：%d -> %d", plan.Version, db.plans[plan.PlanID].Version)
		}
	})
}

// TestUpsertPlanConcurrentCodeCreationReturnsExistingRow plan_code 唯一索引先咬人、
// 台账里又没有这条 request_id：说明是并发新建，回查那行还给调用方，不返回含糊的写失败。
func TestUpsertPlanConcurrentCodeCreationReturnsExistingRow(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	rival := seedPlan(db, &model.Plan{PlanID: 70, PlanCode: "monthly_race", Name: "并发建的月卡",
		VipType: model.VipTypePremium, DurationDays: 31, UnitCount: 1, PriceMinor: 2500, Currency: "CNY",
		State: model.PlanStateDraft})
	db.planCodePreReadMiss = true // 预读时那行还不存在

	got, err := NewUpsertPlanLogic(context.Background(), svcCtx).UpsertPlan(planReq("monthly_race"))
	mustNoError(t, err, "并发新建回查")
	if got.GetPlanId() != rival.PlanID || got.GetName() != rival.Name {
		t.Errorf("没把并发方那行还回去：%+v", got)
	}
	if len(db.planLogs) != 0 {
		t.Errorf("并发新建却记了 %d 条台账", len(db.planLogs))
	}
	if db.plans[rival.PlanID].Version != rival.Version {
		t.Errorf("并发新建覆盖了别人的行：version %d -> %d", rival.Version, db.plans[rival.PlanID].Version)
	}
}

// TestUpsertPlanReadFailurePropagates 目录读失败不得变成「当作新建」或伪成功。
func TestUpsertPlanReadFailurePropagates(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	db.planErr = errPlanDown
	_, err := NewUpsertPlanLogic(context.Background(), svcCtx).UpsertPlan(planReq("monthly_down"))
	if !errors.Is(err, errPlanDown) {
		t.Fatalf("目录读失败必须原样上抛，实得 %v", err)
	}
	if db.txRuns != 0 || len(db.plans) != 0 || len(db.planLogs) != 0 {
		t.Errorf("读失败仍然落笔：事务 %d、套餐 %d、台账 %d", db.txRuns, len(db.plans), len(db.planLogs))
	}

	// 台账回查看不见（幂等键读不动）时，不得凭「大概一致」返回结果。
	svcCtx2, db2 := newTestSvc(t)
	plan := seedPlan(db2, &model.Plan{PlanID: 71, PlanCode: "monthly_logdown", Name: "月卡",
		VipType: model.VipTypePremium, DurationDays: 31, UnitCount: 1, PriceMinor: 2500, Currency: "CNY",
		State: model.PlanStateDraft})
	db2.planLogErr = errLogDown
	in := planReq("monthly_logdown")
	in.PlanId = plan.PlanID
	in.ExpectedVersion = plan.Version
	seedPlanLog(db2, &model.PlanChangeLog{PlanID: plan.PlanID, Operator: planOp, RequestID: in.RequestId})
	if _, err := NewUpsertPlanLogic(context.Background(), svcCtx2).UpsertPlan(in); !errors.Is(err, errLogDown) {
		t.Fatalf("台账读失败必须上抛，实得 %v", err)
	}
}

// --- SetPlanState ---

// TestSetPlanStateLegalEdgesAndLedger 三条合法边各走一遍：状态机推进 + 台账 + 版本前移。
func TestSetPlanStateLegalEdgesAndLedger(t *testing.T) {
	cases := []struct {
		name     string
		from     int32
		to       rpc.PlanSaleState
		wantTo   int32
		planCode string
	}{
		{"草稿上架", model.PlanStateDraft, rpc.PlanSaleState_PLAN_SALE_STATE_ON_SALE, model.PlanStateOnSale, "st_draft"},
		{"在售下架", model.PlanStateOnSale, rpc.PlanSaleState_PLAN_SALE_STATE_OFF_SALE, model.PlanStateOffSale, "st_onsale"},
		{"重新上架", model.PlanStateOffSale, rpc.PlanSaleState_PLAN_SALE_STATE_ON_SALE, model.PlanStateOnSale, "st_offsale"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			plan := seedPlan(db, &model.Plan{PlanID: 80, PlanCode: c.planCode, Name: "月卡",
				VipType: model.VipTypePremium, DurationDays: 31, UnitCount: 1, PriceMinor: 2500, Currency: "CNY",
				State: c.from, Version: 3})
			got, err := NewSetPlanStateLogic(context.Background(), svcCtx).
				SetPlanState(stateReq(plan.PlanID, c.to, plan.Version, "s-1"))
			mustNoError(t, err, "合法迁移")
			if got.GetState() != c.to {
				t.Errorf("回显 state = %v，期望 %v", got.GetState(), c.to)
			}
			row := db.plans[plan.PlanID]
			if row.State != c.wantTo || row.Version != plan.Version+1 || row.UpdatedBy != planOp {
				t.Errorf("落库不符：state=%d version=%d by=%s", row.State, row.Version, row.UpdatedBy)
			}
			lg := db.planLogs["s-1"]
			if lg == nil || lg.ChangeType != model.PlanChangeState ||
				lg.FromState != c.from || lg.ToState != c.wantTo {
				t.Errorf("台账不符：%+v", lg)
			}
			if lg.Reason != "上架给全量用户" {
				t.Errorf("上下架理由必须原样入台账，实得 %q", lg.Reason)
			}
			if lg.FromPriceMinor != lg.ToPriceMinor {
				t.Errorf("上下架不该改价：%d -> %d", lg.FromPriceMinor, lg.ToPriceMinor)
			}
		})
	}
}

// TestSetPlanStateIllegalEdges 其余一律拒：同状态重复上架也不放（重试由幂等重放负责，
// 不靠放松状态机兜），DRAFT 是终态不可回退，未指定/越界枚举直接拒。
func TestSetPlanStateIllegalEdges(t *testing.T) {
	cases := []struct {
		name string
		from int32
		to   rpc.PlanSaleState
	}{
		{"草稿回草稿", model.PlanStateDraft, rpc.PlanSaleState_PLAN_SALE_STATE_DRAFT},
		{"在售重复上架", model.PlanStateOnSale, rpc.PlanSaleState_PLAN_SALE_STATE_ON_SALE},
		{"下架重复下架", model.PlanStateOffSale, rpc.PlanSaleState_PLAN_SALE_STATE_OFF_SALE},
		{"在售回草稿", model.PlanStateOnSale, rpc.PlanSaleState_PLAN_SALE_STATE_DRAFT},
		{"下架回草稿", model.PlanStateOffSale, rpc.PlanSaleState_PLAN_SALE_STATE_DRAFT},
		{"草稿直接下架", model.PlanStateDraft, rpc.PlanSaleState_PLAN_SALE_STATE_OFF_SALE},
		{"未指定目标态", model.PlanStateOnSale, rpc.PlanSaleState_PLAN_SALE_STATE_UNSPECIFIED},
		{"越界枚举", model.PlanStateOnSale, rpc.PlanSaleState(99)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			plan := seedPlan(db, &model.Plan{PlanID: 90, PlanCode: "st_edge", Name: "月卡",
				VipType: model.VipTypePremium, DurationDays: 31, UnitCount: 1, PriceMinor: 2500, Currency: "CNY",
				State: c.from, Version: 2})
			_, err := NewSetPlanStateLogic(context.Background(), svcCtx).
				SetPlanState(stateReq(plan.PlanID, c.to, plan.Version, "s-edge"))
			// 未指定/越界是「目标态本身不合法」，其余是「迁移不合法」，两者都不能写库。
			if err == nil {
				t.Fatal("非法上下架被放行")
			}
			if !errors.Is(err, model.ErrInvalidPlanState) && !errors.Is(err, model.ErrInvalidPlanStateTransition) {
				t.Fatalf("错误分类不对：%v", err)
			}
			if db.txRuns != 0 || len(db.planLogs) != 0 {
				t.Errorf("非法迁移却起了事务/记了台账：%d/%d", db.txRuns, len(db.planLogs))
			}
			if db.plans[plan.PlanID].State != c.from || db.plans[plan.PlanID].Version != 2 {
				t.Errorf("非法迁移改了行：state=%d version=%d", db.plans[plan.PlanID].State, db.plans[plan.PlanID].Version)
			}
		})
	}
}

// TestSetPlanStateRequiresReasonAndIdentifier 无理由的上下架不受理（有终端后果的动作）；
// plan_id 缺失也不允许靠 code 猜。
func TestSetPlanStateRequiresReasonAndIdentifier(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	plan := seedPlan(db, onSalePlan(100))

	noReason := stateReq(plan.PlanID, rpc.PlanSaleState_PLAN_SALE_STATE_OFF_SALE, plan.Version, "s-nr")
	noReason.Reason = "  "
	_, err := NewSetPlanStateLogic(context.Background(), svcCtx).SetPlanState(noReason)
	mustErrIs(t, err, model.ErrReasonRequired, "无理由上下架")

	noID := stateReq(0, rpc.PlanSaleState_PLAN_SALE_STATE_OFF_SALE, plan.Version, "s-ni")
	_, err = NewSetPlanStateLogic(context.Background(), svcCtx).SetPlanState(noID)
	mustErrIs(t, err, model.ErrPlanIdentifierRequired, "缺 plan_id")

	noReq := stateReq(plan.PlanID, rpc.PlanSaleState_PLAN_SALE_STATE_OFF_SALE, plan.Version, "")
	_, err = NewSetPlanStateLogic(context.Background(), svcCtx).SetPlanState(noReq)
	mustErrIs(t, err, model.ErrRequestIdRequired, "缺幂等键")

	if db.txRuns != 0 {
		t.Errorf("参数不全仍起了 %d 个事务", db.txRuns)
	}
}

// TestSetPlanStateCasAndRollback CAS 位缺失/过期都判并发冲突，且状态与台账一起回滚。
func TestSetPlanStateCasAndRollback(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	plan := seedPlan(db, &model.Plan{PlanID: 110, PlanCode: "st_cas", Name: "月卡",
		VipType: model.VipTypePremium, DurationDays: 31, UnitCount: 1, PriceMinor: 2500, Currency: "CNY",
		State: model.PlanStateDraft, Version: 5})

	// expected_version 留空 = 调用方没参与乐观锁，不能让它覆盖别人的写。
	_, err := NewSetPlanStateLogic(context.Background(), svcCtx).
		SetPlanState(stateReq(plan.PlanID, rpc.PlanSaleState_PLAN_SALE_STATE_ON_SALE, 0, "s-zero"))
	mustErrIs(t, err, model.ErrConcurrentUpdate, "expected_version 留空")

	// 版本号过期：CAS 条件不命中 → 冲突，且台账不得留下。
	_, err = NewSetPlanStateLogic(context.Background(), svcCtx).
		SetPlanState(stateReq(plan.PlanID, rpc.PlanSaleState_PLAN_SALE_STATE_ON_SALE, plan.Version-1, "s-stale"))
	mustErrIs(t, err, model.ErrConcurrentUpdate, "过期版本号")
	if len(db.planLogs) != 0 {
		t.Errorf("CAS 未命中仍记了 %d 条台账", len(db.planLogs))
	}
	row := db.plans[plan.PlanID]
	if row.State != model.PlanStateDraft || row.Version != 5 {
		t.Errorf("CAS 未命中却改了行：state=%d version=%d", row.State, row.Version)
	}
}

// TestSetPlanStateReplayBeatsStateMachine 重试到达时当前态已是目标态：
// 幂等重放必须走在状态机校验之前，否则「ON_SALE→ON_SALE」会被误判成非法迁移。
func TestSetPlanStateReplayBeatsStateMachine(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	plan := seedPlan(db, &model.Plan{PlanID: 120, PlanCode: "st_replay", Name: "月卡",
		VipType: model.VipTypePremium, DurationDays: 31, UnitCount: 1, PriceMinor: 2500, Currency: "CNY",
		State: model.PlanStateOnSale, Version: 2}) // 首次上架已生效
	fp := model.Fingerprint("SetPlanState", "120", "2", "1")
	seedPlanLog(db, &model.PlanChangeLog{PlanID: plan.PlanID, ChangeType: model.PlanChangeState,
		FromState: model.PlanStateDraft, ToState: model.PlanStateOnSale,
		Operator: planOp, RequestID: "s-replay", ParamsFingerprint: fp})

	got, err := NewSetPlanStateLogic(context.Background(), svcCtx).
		SetPlanState(stateReq(plan.PlanID, rpc.PlanSaleState_PLAN_SALE_STATE_ON_SALE, 1, "s-replay"))
	mustNoError(t, err, "重放优先于状态机")
	if got.GetState() != rpc.PlanSaleState_PLAN_SALE_STATE_ON_SALE || got.GetVersion() != 2 {
		t.Errorf("重放回显不符：state=%v version=%d", got.GetState(), got.GetVersion())
	}
	if len(db.planLogs) != 1 || db.txRuns != 0 {
		t.Errorf("重放又写了台账/起了事务：%d/%d", len(db.planLogs), db.txRuns)
	}

	// 同键换目标态或换 CAS 位 = 借幂等键改口径。
	for _, mut := range []*rpc.SetPlanStateReq{
		stateReq(plan.PlanID, rpc.PlanSaleState_PLAN_SALE_STATE_OFF_SALE, 1, "s-replay"),
		stateReq(plan.PlanID, rpc.PlanSaleState_PLAN_SALE_STATE_ON_SALE, 2, "s-replay"),
		stateReq(121, rpc.PlanSaleState_PLAN_SALE_STATE_ON_SALE, 1, "s-replay"),
	} {
		_, err := NewSetPlanStateLogic(context.Background(), svcCtx).SetPlanState(mut)
		mustErrIs(t, err, model.ErrRequestIdReused, "借幂等键改口径")
	}
}

// TestSetPlanStateReadFailuresPropagate 目录读与台账读失败都不许降级成「没这行」或「可重放」。
func TestSetPlanStateReadFailuresPropagate(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	db.planErr = errPlanDown
	_, err := NewSetPlanStateLogic(context.Background(), svcCtx).
		SetPlanState(stateReq(130, rpc.PlanSaleState_PLAN_SALE_STATE_ON_SALE, 1, "s-down"))
	if !errors.Is(err, errPlanDown) {
		t.Fatalf("目录读失败必须上抛，实得 %v", err)
	}
	if db.txRuns != 0 {
		t.Errorf("读失败仍起了 %d 个事务", db.txRuns)
	}

	svcCtx2, db2 := newTestSvc(t)
	db2.planLogErr = errLogDown
	if _, err := NewSetPlanStateLogic(context.Background(), svcCtx2).
		SetPlanState(stateReq(131, rpc.PlanSaleState_PLAN_SALE_STATE_ON_SALE, 1, "s-logdown")); !errors.Is(err, errLogDown) {
		t.Fatalf("幂等预读失败必须上抛，实得 %v", err)
	}
	if db2.txRuns != 0 {
		t.Errorf("幂等预读失败仍起了 %d 个事务", db2.txRuns)
	}

	// 套餐不存在 ≠ 读失败：报 ErrPlanNotFound，不写台账。
	svcCtx3, db3 := newTestSvc(t)
	if _, err := NewSetPlanStateLogic(context.Background(), svcCtx3).
		SetPlanState(stateReq(132, rpc.PlanSaleState_PLAN_SALE_STATE_ON_SALE, 1, "s-none")); !errors.Is(err, model.ErrPlanNotFound) {
		t.Fatalf("实得 %v，期望 ErrPlanNotFound", err)
	}
	if len(db3.planLogs) != 0 {
		t.Errorf("不存在的套餐被记了 %d 条台账", len(db3.planLogs))
	}

	// 台账在、套餐行被人工删了：不能伪称成功。
	svcCtx4, db4 := newTestSvc(t)
	seedPlanLog(db4, &model.PlanChangeLog{PlanID: 999, ChangeType: model.PlanChangeState,
		Operator: planOp, RequestID: "s-orphan",
		ParamsFingerprint: model.Fingerprint("SetPlanState", "999", "2", "1")})
	_, err = NewSetPlanStateLogic(context.Background(), svcCtx4).
		SetPlanState(stateReq(999, rpc.PlanSaleState_PLAN_SALE_STATE_ON_SALE, 1, "s-orphan"))
	mustErrIs(t, err, model.ErrPlanNotFound, "孤儿台账")
}

// --- UpsertEntitlement ---

// TestUpsertEntitlementCreateBackfillsIdempotencyResult 新建权益码：
// 幂等键与目录行同事务，且 result_id 必须回填成真正的 entitlement_id。
func TestUpsertEntitlementCreateBackfillsIdempotencyResult(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	got, err := NewUpsertEntitlementLogic(context.Background(), svcCtx).UpsertEntitlement(entReq("vip.high_bitrate"))
	mustNoError(t, err, "新建权益码")
	if !got.GetEnabled() || got.GetVersion() != 1 || got.GetCode() != "vip.high_bitrate" {
		t.Errorf("回显不符：%+v", got)
	}
	row := db.ents["vip.high_bitrate"]
	if row == nil || row.MinVipType != model.VipTypePremium || row.UpdatedBy != entOp {
		t.Fatalf("落库行不符：%+v", row)
	}
	req := db.bizRequest["e-1"]
	if req == nil {
		t.Fatal("幂等键没写")
	}
	if req.Api != model.ApiUpsertEntitlement || req.Subject != "vip.high_bitrate" {
		t.Errorf("幂等键内容不符：api=%s subject=%s", req.Api, req.Subject)
	}
	if req.ResultID != row.EntitlementID {
		t.Errorf("result_id = %d，期望回填 entitlement_id %d", req.ResultID, row.EntitlementID)
	}
	if len(req.ParamsFingerprint) != 64 {
		t.Errorf("指纹长度 %d（CHAR(64) 列）", len(req.ParamsFingerprint))
	}
	// 目录写入不得碰时长台账。
	if len(db.grants) != 0 || len(db.members) != 0 {
		t.Errorf("改目录却写了会员表：%d/%d", len(db.grants), len(db.members))
	}
}

// TestUpsertEntitlementCodeIsImmutable 更新只改展示与门槛：code 是跨服务稳定引用，
// 换口径要新增码并把旧码关掉。
func TestUpsertEntitlementCodeIsImmutable(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedEntitlement(db, &model.Entitlement{Code: "vip.high_bitrate", Name: "高码率播放",
		Description: "1080P", MinVipType: model.VipTypePremium, Enabled: 1, Version: 4})

	in := entReq("vip.high_bitrate")
	in.ExpectedVersion = 4
	in.Name = "高码率播放（改名）"
	in.MinVipType = rpc.VipType_VIP_TYPE_PREMIUM_PLUS
	in.Description = ""
	in.RequestId = "e-up"
	got, err := NewUpsertEntitlementLogic(context.Background(), svcCtx).UpsertEntitlement(in)
	mustNoError(t, err, "更新权益码")
	if got.GetCode() != "vip.high_bitrate" || !got.GetEnabled() {
		t.Errorf("回显不符：%+v", got)
	}
	if got.GetMinVipType() != rpc.VipType_VIP_TYPE_PREMIUM_PLUS {
		t.Errorf("门槛没改：%v", got.GetMinVipType())
	}
	if _, leaked := db.ents["vip.high_bitrate（改名）"]; leaked {
		t.Error("名字被当成码写进了键")
	}
	if db.ents["vip.high_bitrate"].Version != 5 {
		t.Errorf("version = %d，期望 CAS 递增到 5", db.ents["vip.high_bitrate"].Version)
	}
	// 关掉不等于删行：判定要能区分 CODE_DISABLED 与 CODE_UNKNOWN。
	off := entReq("vip.high_bitrate")
	off.ExpectedVersion = 5
	off.Enabled = false
	off.Name = "高码率播放（改名）"
	off.MinVipType = rpc.VipType_VIP_TYPE_PREMIUM_PLUS
	off.RequestId = "e-off"
	if _, err := NewUpsertEntitlementLogic(context.Background(), svcCtx).UpsertEntitlement(off); err != nil {
		t.Fatalf("关掉权益码：%v", err)
	}
	if _, still := db.ents["vip.high_bitrate"]; !still {
		t.Error("enabled=0 把行删了，CODE_DISABLED 会退化成 CODE_UNKNOWN")
	}
}

// TestUpsertEntitlementRejectsBadCatalog 目录码的字符集/宽度与操作者、幂等键都要在起事务前拒掉。
func TestUpsertEntitlementRejectsBadCatalog(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*rpc.UpsertEntitlementReq)
		want   error
	}{
		{"空码", func(in *rpc.UpsertEntitlementReq) { in.Code = "  " }, model.ErrEntitlementCodeRequired},
		{"带空格的码", func(in *rpc.UpsertEntitlementReq) { in.Code = "vip high" }, model.ErrEntitlementCodeRequired},
		{"中文码", func(in *rpc.UpsertEntitlementReq) { in.Code = "vip.高码率" }, model.ErrEntitlementCodeRequired},
		{"超长码", func(in *rpc.UpsertEntitlementReq) {
			in.Code = "vip." + strings.Repeat("x", model.MaxEntitlementCodeLength)
		}, model.ErrFieldTooLong},
		{"空名", func(in *rpc.UpsertEntitlementReq) { in.Name = " " }, model.ErrEntitlementCodeRequired},
		{"超长名", func(in *rpc.UpsertEntitlementReq) {
			in.Name = strings.Repeat("名", model.MaxEntitlementNameLength+1)
		}, model.ErrFieldTooLong},
		{"超长描述", func(in *rpc.UpsertEntitlementReq) {
			in.Description = strings.Repeat("y", model.MaxEntitlementDescLength+1)
		}, model.ErrFieldTooLong},
		{"未指定档位", func(in *rpc.UpsertEntitlementReq) { in.MinVipType = rpc.VipType_VIP_TYPE_UNSPECIFIED },
			model.ErrInvalidVipType},
		{"越界档位", func(in *rpc.UpsertEntitlementReq) { in.MinVipType = rpc.VipType(9) }, model.ErrInvalidVipType},
		{"缺操作者", func(in *rpc.UpsertEntitlementReq) { in.Operator = "" }, model.ErrOperatorRequired},
		{"缺幂等键", func(in *rpc.UpsertEntitlementReq) { in.RequestId = "" }, model.ErrRequestIdRequired},
		{"超长幂等键", func(in *rpc.UpsertEntitlementReq) { in.RequestId = strings.Repeat("r", maxRequestIDWidth+1) },
			model.ErrFieldTooLong},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			in := entReq("vip.ok")
			c.mutate(in)
			_, err := NewUpsertEntitlementLogic(context.Background(), svcCtx).UpsertEntitlement(in)
			mustErrIs(t, err, c.want, "目录入参校验")
			if db.txRuns != 0 || len(db.ents) != 0 || len(db.bizRequest) != 0 {
				t.Errorf("校验未过仍落笔：%d/%d/%d", db.txRuns, len(db.ents), len(db.bizRequest))
			}
		})
	}
}

// TestUpsertEntitlementReplayAndConflict 幂等重放：同 request_id 同参数返回首次结果且不改目录；
// 换参数报冲突；重放路径的目录读失败必须上抛。
func TestUpsertEntitlementReplayAndConflict(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedEntitlement(db, &model.Entitlement{Code: "vip.ultra_4k", Name: "4K 超清", Description: "2160P",
		MinVipType: model.VipTypePremiumPlus, Enabled: 1, Version: 3})
	fp := model.Fingerprint("UpsertEntitlement", "vip.ultra_4k", "4K 超清", "2160P", "2", "1", "3")
	seedRequest(db, &model.BizRequest{RequestID: "e-replay", Api: model.ApiUpsertEntitlement,
		Subject: "vip.ultra_4k", ParamsFingerprint: fp, Operator: entOp})

	// 与台账指纹逐字段对齐的那份请求——否则「重放」根本重放不到首次的参数。
	alreadyApplied := func() *rpc.UpsertEntitlementReq {
		r := entReq("vip.ultra_4k")
		r.Name = "4K 超清"
		r.Description = "2160P"
		r.MinVipType = rpc.VipType_VIP_TYPE_PREMIUM_PLUS
		r.ExpectedVersion = 3
		r.RequestId = "e-replay"
		return r
	}

	got, err := NewUpsertEntitlementLogic(context.Background(), svcCtx).UpsertEntitlement(alreadyApplied())
	mustNoError(t, err, "权益码幂等重放")
	if got.GetName() != "4K 超清" || got.GetVersion() != 3 {
		t.Errorf("重放没返回首次结果：%+v", got)
	}
	if db.ents["vip.ultra_4k"].Version != 3 || len(db.bizRequest) != 1 {
		t.Errorf("重放二次改了目录：version=%d 幂等键 %d 条", db.ents["vip.ultra_4k"].Version, len(db.bizRequest))
	}

	mutations := []struct {
		name   string
		mutate func(*rpc.UpsertEntitlementReq)
	}{
		{"改名", func(in *rpc.UpsertEntitlementReq) { in.Name = "4K 超清（新）" }},
		{"改门槛", func(in *rpc.UpsertEntitlementReq) { in.MinVipType = rpc.VipType_VIP_TYPE_PREMIUM }},
		{"改开关", func(in *rpc.UpsertEntitlementReq) { in.Enabled = false }},
		{"改描述", func(in *rpc.UpsertEntitlementReq) { in.Description = "改了描述" }},
		{"改 CAS 位", func(in *rpc.UpsertEntitlementReq) { in.ExpectedVersion = 2 }},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			mut := alreadyApplied()
			m.mutate(mut)
			_, err := NewUpsertEntitlementLogic(context.Background(), svcCtx).UpsertEntitlement(mut)
			mustErrIs(t, err, model.ErrRequestIdReused, "借幂等键改目录")
			if db.ents["vip.ultra_4k"].Version != 3 {
				t.Error("冲突请求改了目录版本")
			}
		})
	}

	// 幂等键读不动时不得当成「没记过账」直接写。
	svcCtx2, db2 := newTestSvc(t)
	db2.requestErr = errRequestDown
	if _, err := NewUpsertEntitlementLogic(context.Background(), svcCtx2).
		UpsertEntitlement(entReq("vip.down")); !errors.Is(err, errRequestDown) {
		t.Fatalf("幂等预读失败必须上抛，实得 %v", err)
	}
	if len(db2.ents) != 0 || db2.txRuns != 0 {
		t.Errorf("预读失败仍落了目录：%d 行、%d 个事务", len(db2.ents), db2.txRuns)
	}

	// 目录读失败同理。
	svcCtx3, db3 := newTestSvc(t)
	db3.entErr = errCatalogDown
	if _, err := NewUpsertEntitlementLogic(context.Background(), svcCtx3).
		UpsertEntitlement(entReq("vip.down")); !errors.Is(err, errCatalogDown) {
		t.Fatalf("目录读失败必须上抛，实得 %v", err)
	}
	if db3.txRuns != 0 {
		t.Errorf("目录读失败仍起了 %d 个事务", db3.txRuns)
	}
}

// TestUpsertEntitlementCreateRetryIsReplay 首建请求超时后按原样重试（expected_version 仍是 0）：
// 这一路必须走幂等重放，而不是被「目录已存在、版本对不上」拦成并发冲突。
func TestUpsertEntitlementCreateRetryIsReplay(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	first, err := NewUpsertEntitlementLogic(context.Background(), svcCtx).UpsertEntitlement(entReq("vip.retry"))
	mustNoError(t, err, "首建")

	retry, err := NewUpsertEntitlementLogic(context.Background(), svcCtx).UpsertEntitlement(entReq("vip.retry"))
	mustNoError(t, err, "原样重试")
	if retry.GetCode() != first.GetCode() || retry.GetName() != first.GetName() ||
		retry.GetVersion() != first.GetVersion() {
		// 契约里权益以 code 为标识（EntitlementInfo 没有 entitlement_id 字段）。
		t.Errorf("重试没回到首建结果：%+v vs %+v", retry, first)
	}
	if db.ents["vip.retry"].Version != 1 || len(db.bizRequest) != 1 {
		t.Errorf("重试二次改了目录：version=%d 幂等键 %d 条", db.ents["vip.retry"].Version, len(db.bizRequest))
	}
}

// TestUpsertEntitlementCasAndConcurrentCode 过期版本号给明确冲突；
// 并发同瞬间建同码则整体回滚并让调用方重读重试。
func TestUpsertEntitlementCasAndConcurrentCode(t *testing.T) {
	t.Run("过期版本号", func(t *testing.T) {
		svcCtx, db := newTestSvc(t)
		seedEntitlement(db, &model.Entitlement{Code: "vip.cas", Name: "n", MinVipType: model.VipTypePremium,
			Enabled: 1, Version: 9})
		in := entReq("vip.cas")
		in.ExpectedVersion = 8
		_, err := NewUpsertEntitlementLogic(context.Background(), svcCtx).UpsertEntitlement(in)
		mustErrIs(t, err, model.ErrConcurrentUpdate, "过期版本号")
		if db.txRuns != 0 || len(db.bizRequest) != 0 {
			t.Errorf("注定回滚的写还是起了事务/占了幂等键：%d/%d", db.txRuns, len(db.bizRequest))
		}
	})

	t.Run("并发建同码", func(t *testing.T) {
		svcCtx, db := newTestSvc(t)
		// 并发方那行先落库，但读路径暂时看不见它：预读判「不存在」，INSERT 才撞 uniq_code。
		seedEntitlement(db, &model.Entitlement{Code: "vip.race", Name: "并发方",
			MinVipType: model.VipTypePremium, Enabled: 1})
		in := entReq("vip.race")
		db.entAppearsOnInsert = true

		_, err := NewUpsertEntitlementLogic(context.Background(), svcCtx).UpsertEntitlement(in)
		mustErrIs(t, err, model.ErrConcurrentUpdate, "并发建同码")
		if len(db.bizRequest) != 0 {
			t.Errorf("并发冲突后仍占了 %d 个幂等键", len(db.bizRequest))
		}
		row := db.ents["vip.race"]
		if row == nil || row.Name != "并发方" {
			t.Errorf("并发方的行被覆盖或丢失：%+v", row)
		}
	})
}

// --- SetAutoRenew ---

// TestSetAutoRenewSignAndUnsign 签约只认 SANDBOX；解约一律清空渠道，
// 且不动 source/paid_month_count（自助翻签约位不该抹掉付费来源）。
func TestSetAutoRenewSignAndUnsign(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	seedMembership(db, &model.Membership{Mid: 200, VipType: model.VipTypePremium, StartAt: now - 10*day,
		ExpireAt: now + 20*day, Source: model.GrantSourceSandboxPurchase, PaidMonthCount: 3})

	got, err := NewSetAutoRenewLogic(context.Background(), svcCtx).
		SetAutoRenew(autoRenewReq(200, true, sandboxString, "a-on"))
	mustNoError(t, err, "签约")
	if !got.GetMembership().GetAutoRenew() || got.GetMembership().GetAutoRenewChannel() != sandboxString {
		t.Errorf("签约回显不符：%+v", got.GetMembership())
	}
	row := db.members[memberKey(200, model.VipTypePremium)]
	if row.Source != model.GrantSourceSandboxPurchase || row.PaidMonthCount != 3 {
		t.Errorf("翻签约位改动了来源/付费月数：%d/%d", row.Source, row.PaidMonthCount)
	}
	if row.AutoRenewSignedAt < now {
		t.Errorf("signed_at=%d 不在本次请求之后", row.AutoRenewSignedAt)
	}
	req := db.bizRequest["a-on"]
	if req.Api != model.ApiSetAutoRenew || req.Subject != membershipSubject(200, model.VipTypePremium) ||
		req.Mid != 200 || req.VipType != model.VipTypePremium || req.ResultID != row.MembershipID {
		t.Errorf("幂等键内容不符：%+v", req)
	}
	if len(db.grants) != 0 {
		t.Errorf("签约位翻转不该进 mb_grant，实得 %d 条", len(db.grants))
	}

	// 解约时客户端常常原样回显渠道：一律归一化成无渠道。
	off := autoRenewReq(200, false, sandboxString, "a-off")
	got, err = NewSetAutoRenewLogic(context.Background(), svcCtx).SetAutoRenew(off)
	mustNoError(t, err, "解约")
	if got.GetMembership().GetAutoRenew() || got.GetMembership().GetAutoRenewChannel() != "" {
		t.Errorf("解约后仍留着签约痕迹：%+v", got.GetMembership())
	}
}

// TestSetAutoRenewChannelAndStateRules 渠道枚举、过期身份不得签约、无身份行不得凭空造。
func TestSetAutoRenewChannelAndStateRules(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	seedMembership(db, &model.Membership{Mid: 210, VipType: model.VipTypePremium,
		StartAt: now - 60*day, ExpireAt: now - day}) // 已过期

	_, err := NewSetAutoRenewLogic(context.Background(), svcCtx).
		SetAutoRenew(autoRenewReq(210, true, sandboxString, "a-expired"))
	mustErrIs(t, err, model.ErrAutoRenewUnsupported, "过期身份签约")
	if len(db.bizRequest) != 0 {
		t.Errorf("被拒的请求仍占了 %d 个幂等键", len(db.bizRequest))
	}

	_, err = NewSetAutoRenewLogic(context.Background(), svcCtx).
		SetAutoRenew(autoRenewReq(211, true, sandboxString, "a-none"))
	mustErrIs(t, err, model.ErrMembershipNotFound, "无身份行签约")

	for _, ch := range []string{"", "  ", "wechatPapay", "ALIPAY", "sandbox", sandboxString + "x"} {
		in := autoRenewReq(210, true, ch, "a-ch-"+strconv.Itoa(len(ch)))
		_, err := NewSetAutoRenewLogic(context.Background(), svcCtx).SetAutoRenew(in)
		want := model.ErrAutoRenewChannelRejected
		if strings.TrimSpace(ch) == "" {
			want = model.ErrAutoRenewChannelRequired
		}
		mustErrIs(t, err, want, "签约渠道 "+ch)
	}
}

// TestSetAutoRenewReplayConflict 同 request_id 重放不得二次翻位（signed_at 不该被推）；
// 换 on/channel 报冲突。
func TestSetAutoRenewReplayConflict(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	seedMembership(db, &model.Membership{Mid: 220, VipType: model.VipTypePremium, StartAt: now - day,
		ExpireAt: now + 20*day, AutoRenew: 1, AutoRenewChannel: sandboxString, AutoRenewSignedAt: now - 5*day,
		Version: 2})
	fp := model.Fingerprint(model.ApiSetAutoRenew, membershipSubject(220, model.VipTypePremium), "true", sandboxString)
	seedRequest(db, &model.BizRequest{RequestID: "a-replay", Api: model.ApiSetAutoRenew,
		Subject: membershipSubject(220, model.VipTypePremium), Mid: 220, VipType: model.VipTypePremium,
		ParamsFingerprint: fp, Operator: "user-self", Ctime: now - day})

	got, err := NewSetAutoRenewLogic(context.Background(), svcCtx).
		SetAutoRenew(autoRenewReq(220, true, sandboxString, "a-replay"))
	mustNoError(t, err, "重放")
	if !got.GetDuplicated() {
		t.Error("重放没标 duplicated")
	}
	signed := db.members[memberKey(220, model.VipTypePremium)].AutoRenewSignedAt
	if signed != now-5*day {
		t.Errorf("重放把 signed_at 推到了 %d，期望仍是首次的 %d", signed, now-5*day)
	}
	if db.members[memberKey(220, model.VipTypePremium)].Version != 2 {
		t.Error("重放二次改了身份行版本")
	}

	_, err = NewSetAutoRenewLogic(context.Background(), svcCtx).
		SetAutoRenew(autoRenewReq(220, false, sandboxString, "a-replay"))
	mustErrIs(t, err, model.ErrRequestIdReused, "借幂等键改开关")

	// 台账里主体信息缺失 = 数据被人工改过，不能伪称成功。
	svcCtx2, db2 := newTestSvc(t)
	seedMembership(db2, &model.Membership{Mid: 230, VipType: model.VipTypePremium,
		StartAt: now - day, ExpireAt: now + 20*day})
	seedRequest(db2, &model.BizRequest{RequestID: "a-orphan", Api: model.ApiSetAutoRenew,
		Subject: membershipSubject(230, model.VipTypePremium),
		ParamsFingerprint: model.Fingerprint(model.ApiSetAutoRenew, membershipSubject(230, model.VipTypePremium),
			"true", sandboxString), Operator: "user-self"})
	_, err = NewSetAutoRenewLogic(context.Background(), svcCtx2).
		SetAutoRenew(autoRenewReq(230, true, sandboxString, "a-orphan"))
	mustErrIs(t, err, model.ErrMembershipNotFound, "幂等键缺主体信息")
}

// TestSetAutoRenewCasMissRollsBackKey 身份行 CAS 未命中：刚占的幂等键一并回滚，
// 否则重试会被自己的键挡住。
func TestSetAutoRenewCasMissRollsBackKey(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	seedMembership(db, &model.Membership{Mid: 240, VipType: model.VipTypePremium, StartAt: now - day,
		ExpireAt: now + 20*day, Version: 3})
	db.membershipConcurrentCommit = true

	_, err := NewSetAutoRenewLogic(context.Background(), svcCtx).
		SetAutoRenew(autoRenewReq(240, true, sandboxString, "a-cas"))
	mustErrIs(t, err, model.ErrConcurrentUpdate, "签约 CAS 未命中")
	if len(db.bizRequest) != 0 {
		t.Errorf("CAS 冲突后幂等键没回滚：%+v", db.bizRequest)
	}
	row := db.members[memberKey(240, model.VipTypePremium)]
	if row.AutoRenew != 0 || row.AutoRenewSignedAt != 0 {
		t.Errorf("CAS 冲突却改了签约位：%+v", row)
	}
}
