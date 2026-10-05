package logic

// 目录读侧四个接口（ListPlans / GetPlan / ListPlansAdmin / ListEntitlements）的用例级测试。
//
// 这四个都是纯读，但「读错」的代价并不比写错小：
//   - ListPlans 泄露一行 DRAFT 就等于把没定价的草稿挂上收银台（README §5）；
//   - GetPlan 替 trade-order 把 DRAFT/OFF_SALE 过滤成「不存在」，下单前置校验就失去依据，
//     所以本接口必须如实回真实 state，并把「没有这行」与「读不动」分家；
//   - ListPlansAdmin 的 page/size 是回显而不是要求值，运营看到「总数 3 但列表空」会当故障；
//   - ListEntitlements 默认要带出 enabled=0 的码，判定侧才能区分 CODE_UNKNOWN 与 CODE_DISABLED。
//
// 断言手法沿用本包既有风格：
//   - 投影逐字段与来源行比对（assertPlanEchoed / assertEntitlementEchoed），漏映射一个字段就发红；
//   - 守卫是否发生在触库之前，用假实现的调用计数证明（planListOnSaleCalls 等），不看日志；
//   - 过滤条件的「折算形态」用入参快照证明（枚举序数必须变成位掩码，而不是被当成掩码）。
//
// 本轮这四个接口都不读写缓存（svcCtx.Cache 无任何引用，README §7-1），
// 因此「写库成功后才失效缓存」的顺序在这条路径上不适用，用例改为钉住「零事务、零写入」。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/membership/model"
	"go-video/services/membership/rpc"
)

// --- 夹具 ---

// catalogPlan 造一行套餐。价格用「分」的整数、平台用位掩码，
// ctime/mtime 用相对 now 的偏移，便于逐字段比对而不依赖挂钟读数。
func catalogPlan(id int64, code string, state, vipType int32, mask uint32) *model.Plan {
	now := nowSec()
	return &model.Plan{
		PlanID:             id,
		PlanCode:           code,
		Name:               "大会员月卡-" + code,
		Description:        "1080P 高码率 + 去广告",
		VipType:            vipType,
		DurationDays:       31,
		UnitCount:          1,
		PriceMinor:         2500,
		PromPriceMinor:     1990,
		Currency:           "CNY",
		PlatformMask:       mask,
		AutoRenewSupported: 1,
		State:              state,
		Version:            4,
		CreatedBy:          planOp,
		UpdatedBy:          "ops-2",
		Ctime:              now - 50*testDay,
		Mtime:              now - testDay,
	}
}

// assertPlanEchoed 逐个字段比对 reply 与库里的行：任何字段漏映射、串台或被「好心」改写都会发红。
func assertPlanEchoed(t *testing.T, got *rpc.PlanInfo, row *model.Plan, where string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s：reply 里没有套餐投影，期望来自 plan_id=%d 的行", where, row.PlanID)
	}
	checks := []struct {
		field       string
		gotV, wantV interface{}
	}{
		{"plan_id", got.GetPlanId(), row.PlanID},
		{"plan_code", got.GetPlanCode(), row.PlanCode},
		{"name", got.GetName(), row.Name},
		{"description", got.GetDescription(), row.Description},
		{"vip_type", got.GetVipType(), rpc.VipType(row.VipType)},
		{"duration_days", got.GetDurationDays(), row.DurationDays},
		{"unit_count", got.GetUnitCount(), row.UnitCount},
		{"price_minor", got.GetPriceMinor(), row.PriceMinor},
		{"prom_price_minor", got.GetPromPriceMinor(), row.PromPriceMinor},
		{"currency", got.GetCurrency(), row.Currency},
		{"auto_renew_supported", got.GetAutoRenewSupported(), row.AutoRenewSupported == 1},
		{"state", got.GetState(), rpc.PlanSaleState(row.State)},
		{"version", got.GetVersion(), row.Version},
		{"ctime", got.GetCtime(), row.Ctime},
		{"mtime", got.GetMtime(), row.Mtime},
		{"created_by", got.GetCreatedBy(), row.CreatedBy},
		{"updated_by", got.GetUpdatedBy(), row.UpdatedBy},
	}
	for _, c := range checks {
		if c.gotV != c.wantV {
			t.Errorf("%s：%s = %v，期望来自库里的 %v", where, c.field, c.gotV, c.wantV)
		}
	}
	// platforms 是位掩码的还原结果：必须与库里的掩码逐项等价，而不是「全部平台」或空。
	wantBits := model.PlatformsOfMask(row.PlatformMask)
	if len(got.GetPlatforms()) != len(wantBits) {
		t.Fatalf("%s：platforms 数量 = %d，期望按掩码 %d 还原成 %d 项",
			where, len(got.GetPlatforms()), row.PlatformMask, len(wantBits))
	}
	for i, p := range got.GetPlatforms() {
		if int32(p) != wantBits[i] {
			t.Errorf("%s：platforms[%d] = %v，期望 %d", where, i, p, wantBits[i])
		}
	}
}

// assertEntitlementEchoed 同上，比对权益码目录行的每个投影字段。
func assertEntitlementEchoed(t *testing.T, got *rpc.EntitlementInfo, row *model.Entitlement, where string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s：缺少 %s 的投影", where, row.Code)
	}
	if got.GetCode() != row.Code {
		t.Errorf("%s：code = %s，期望 %s", where, got.GetCode(), row.Code)
	}
	if got.GetName() != row.Name {
		t.Errorf("%s：name = %s，期望 %s", where, got.GetName(), row.Name)
	}
	if got.GetDescription() != row.Description {
		t.Errorf("%s：description = %s，期望 %s", where, got.GetDescription(), row.Description)
	}
	if got.GetMinVipType() != rpc.VipType(row.MinVipType) {
		t.Errorf("%s：min_vip_type = %v，期望 %v", where, got.GetMinVipType(), row.MinVipType)
	}
	if got.GetEnabled() != (row.Enabled == 1) {
		t.Errorf("%s：enabled = %v，期望库里的 %d", where, got.GetEnabled(), row.Enabled)
	}
	if got.GetVersion() != row.Version {
		t.Errorf("%s：version = %d，期望 %d", where, got.GetVersion(), row.Version)
	}
	if got.GetCtime() != row.Ctime || got.GetMtime() != row.Mtime {
		t.Errorf("%s：ctime/mtime = %d/%d，期望 %d/%d",
			where, got.GetCtime(), got.GetMtime(), row.Ctime, row.Mtime)
	}
}

// --- ListPlans（终端面）---

// TestListPlansOnlyOnSaleRegardlessOfFlag 钉住「终端面恒定只出在售」：
// on_sale_only=false 不得放宽过滤（README §5：不能因为运营误勾一个参数把草稿挂上收银台）。
func TestListPlansOnlyOnSaleRegardlessOfFlag(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	onSale := seedPlan(db, catalogPlan(11, "monthly_on_sale", model.PlanStateOnSale,
		model.VipTypePremium, model.PlatformBitAndroid))
	seedPlan(db, catalogPlan(12, "monthly_draft", model.PlanStateDraft,
		model.VipTypePremium, model.PlatformBitAndroid))
	seedPlan(db, catalogPlan(13, "monthly_off", model.PlanStateOffSale,
		model.VipTypePremium, model.PlatformBitAndroid))

	for _, only := range []bool{true, false} {
		name := fmt.Sprintf("on_sale_only=%v", only)
		t.Run(name, func(t *testing.T) {
			got, err := NewListPlansLogic(context.Background(), svcCtx).ListPlans(&rpc.ListPlansReq{OnSaleOnly: only})
			mustNoError(t, err, "ListPlans "+name)
			if len(got.GetPlans()) != 1 {
				t.Fatalf("终端面出了 %d 行，期望只有那 1 行在售（草稿/下架一律不泄露）", len(got.GetPlans()))
			}
			assertPlanEchoed(t, got.GetPlans()[0], onSale, "ListPlans "+name)
			if got.GetPlans()[0].GetPlanId() != 11 {
				t.Errorf("返回的是 plan_id=%d，期望 11", got.GetPlans()[0].GetPlanId())
			}
			// 纯读：不开事务、不消耗幂等键、不留台账。
			if db.txRuns != 0 {
				t.Errorf("读接口开了 %d 次事务", db.txRuns)
			}
			if len(db.grants) != 0 || len(db.planLogs) != 0 || len(db.bizRequest) != 0 {
				t.Errorf("读接口写出了残留：grant=%d planLog=%d bizRequest=%d",
					len(db.grants), len(db.planLogs), len(db.bizRequest))
			}
		})
	}
}

// TestListPlansPlatformOrdinalIsConvertedToBitMask 锁「平台过滤按位与，不是按序数比大小」。
// 库里刻意放一行掩码=Android|IOS(0b011) 与一行掩码=Harmony(0b100)：
// 若把枚举序数 HARMONY(3) 当掩码传下去，位与会命中前一行、漏掉后一行，本用例立刻发红。
func TestListPlansPlatformOrdinalIsConvertedToBitMask(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedPlan(db, catalogPlan(21, "and_ios", model.PlanStateOnSale,
		model.VipTypePremium, model.PlatformBitAndroid|model.PlatformBitIOS))
	seedPlan(db, catalogPlan(22, "harmony_only", model.PlanStateOnSale,
		model.VipTypePremium, model.PlatformBitHarmony))

	cases := []struct {
		name      string
		platform  rpc.PlanPlatform
		wantBit   uint32
		wantCodes []string
	}{
		{"鸿蒙端只出鸿蒙行", rpc.PlanPlatform_PLAN_PLATFORM_HARMONY, model.PlatformBitHarmony, []string{"harmony_only"}},
		{"iOS 端只出含 iOS 位的行", rpc.PlanPlatform_PLAN_PLATFORM_IOS, model.PlatformBitIOS, []string{"and_ios"}},
		{"全平台不按位过滤", rpc.PlanPlatform_PLAN_PLATFORM_UNSPECIFIED, 0, []string{"and_ios", "harmony_only"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db.planListOnSaleCalls = 0
			got, err := NewListPlansLogic(context.Background(), svcCtx).ListPlans(&rpc.ListPlansReq{Platform: tc.platform})
			mustNoError(t, err, "ListPlans "+tc.name)
			if db.lastOnSaleScan.platformBit != tc.wantBit {
				t.Fatalf("传给 model 的平台过滤 = %d，期望折算成位掩码 %d（枚举序数不能当掩码用）",
					db.lastOnSaleScan.platformBit, tc.wantBit)
			}
			assertCodesInOrder(t, planCodesOf(got.GetPlans()), tc.wantCodes)
		})
	}
}

// TestListPlansVipTypeFilterAndInvalidEnumGuardsBeforeDB：
// 档位过滤要落到查询里；非法枚举必须在触库之前被拒（调用计数为 0 就是证据）。
func TestListPlansVipTypeFilterAndInvalidEnumGuardsBeforeDB(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedPlan(db, catalogPlan(31, "p_plus", model.PlanStateOnSale,
		model.VipTypePremiumPlus, model.PlatformBitAndroid))
	seedPlan(db, catalogPlan(32, "p_premium", model.PlanStateOnSale,
		model.VipTypePremium, model.PlatformBitAndroid))

	got, err := NewListPlansLogic(context.Background(), svcCtx).ListPlans(
		&rpc.ListPlansReq{VipType: rpc.VipType_VIP_TYPE_PREMIUM_PLUS})
	mustNoError(t, err, "ListPlans 按档位过滤")
	if db.lastOnSaleScan.vipType != model.VipTypePremiumPlus {
		t.Fatalf("传给 model 的 vip_type = %d，期望 %d", db.lastOnSaleScan.vipType, model.VipTypePremiumPlus)
	}
	assertCodesInOrder(t, planCodesOf(got.GetPlans()), []string{"p_plus"})

	cases := []struct {
		name string
		in   *rpc.ListPlansReq
		want error
	}{
		{"档位越界", &rpc.ListPlansReq{VipType: rpc.VipType(9)}, model.ErrInvalidVipType},
		{"档位负数", &rpc.ListPlansReq{VipType: rpc.VipType(-1)}, model.ErrInvalidVipType},
		{"平台越界", &rpc.ListPlansReq{Platform: rpc.PlanPlatform(40)}, model.ErrInvalidPlatform},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := db.planListOnSaleCalls
			reply, err := NewListPlansLogic(context.Background(), svcCtx).ListPlans(tc.in)
			mustErrIs(t, err, tc.want, "ListPlans "+tc.name)
			if reply != nil {
				t.Errorf("守卫未过时不得返回任何列表结构，实得 %+v", reply)
			}
			if db.planListOnSaleCalls != before {
				t.Fatalf("非法枚举仍在 %d 次触库之后才被拒，期望守卫发生在读库之前", db.planListOnSaleCalls-before)
			}
		})
	}
}

// TestListPlansReadFailurePropagates：mb_plan 读失败必须是错误，
// 不能塌成「今天没有套餐可卖」的空列表（收银台会因此整页变空且没有任何错误码）。
func TestListPlansReadFailurePropagates(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedPlan(db, catalogPlan(41, "monthly", model.PlanStateOnSale, model.VipTypePremium, model.PlatformBitAndroid))
	db.planErr = errPlanDown

	got, err := NewListPlansLogic(context.Background(), svcCtx).ListPlans(&rpc.ListPlansReq{})
	if !errors.Is(err, errPlanDown) {
		t.Fatalf("读失败必须原样上抛，实得 %v", err)
	}
	if got != nil {
		t.Errorf("读失败时不得返回空列表冒充「无可卖套餐」，实得 %+v", got)
	}
}

// --- GetPlan ---

// TestGetPlanPrefersPlanIdOverCode：两个标识都给时以 plan_id 为准（契约注释钉死的优先级），
// 并且只能走主键读——按码读会把「两个标识指向两行」这件事藏起来。
func TestGetPlanPrefersPlanIdOverCode(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	byID := seedPlan(db, catalogPlan(51, "code_a", model.PlanStateOnSale,
		model.VipTypePremium, model.PlatformBitAndroid))
	seedPlan(db, catalogPlan(52, "code_b", model.PlanStateOnSale,
		model.VipTypePremium, model.PlatformBitAndroid))

	got, err := NewGetPlanLogic(context.Background(), svcCtx).GetPlan(&rpc.GetPlanReq{PlanId: 51, PlanCode: "code_b"})
	mustNoError(t, err, "GetPlan 按 plan_id")
	if !got.GetFound() {
		t.Fatalf("found=false，期望按主键命中 51")
	}
	assertPlanEchoed(t, got.GetPlan(), byID, "GetPlan")
	if db.planFindOneCalls != 1 || db.planFindByCodeCalls != 0 {
		t.Fatalf("读路径 = FindOne %d 次 / FindByCode %d 次，期望 1/0（plan_id 优先）",
			db.planFindOneCalls, db.planFindByCodeCalls)
	}
}

// TestGetPlanByCodeOnlyHitsUniqueIndexPath：只给 plan_code 时必须走唯一码读，
// 且首尾空白要裁掉——否则「" monthly "」会被当成另一个码回 found=false。
func TestGetPlanByCodeOnlyHitsUniqueIndexPath(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	row := seedPlan(db, catalogPlan(61, "monthly", model.PlanStateOnSale,
		model.VipTypePremium, model.PlatformBitAndroid))

	got, err := NewGetPlanLogic(context.Background(), svcCtx).GetPlan(&rpc.GetPlanReq{PlanCode: "  monthly  "})
	mustNoError(t, err, "GetPlan 按 plan_code")
	assertPlanEchoed(t, got.GetPlan(), row, "GetPlan by code")
	if db.planFindByCodeCalls != 1 || db.planFindOneCalls != 0 {
		t.Fatalf("读路径 = FindByCode %d / FindOne %d，期望 1/0", db.planFindByCodeCalls, db.planFindOneCalls)
	}
}

// TestGetPlanDoesNotHideDraftOrOffSale：本接口是下单前置校验的读源，
// 必须如实带回 state，把 DRAFT/OFF_SALE 过滤成 found=false 会让调用方以为「套餐被删了」。
func TestGetPlanDoesNotHideDraftOrOffSale(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	draft := seedPlan(db, catalogPlan(71, "draft_plan", model.PlanStateDraft,
		model.VipTypePremium, model.PlatformBitAndroid))
	off := seedPlan(db, catalogPlan(72, "off_plan", model.PlanStateOffSale,
		model.VipTypePremium, model.PlatformBitAndroid))

	for _, tc := range []struct {
		name string
		id   int64
		want *model.Plan
	}{
		{"草稿照样可见", 71, draft},
		{"已下架照样可见", 72, off},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewGetPlanLogic(context.Background(), svcCtx).GetPlan(&rpc.GetPlanReq{PlanId: tc.id})
			mustNoError(t, err, "GetPlan "+tc.name)
			if !got.GetFound() {
				t.Fatalf("found=false，%s 不该被隐藏", tc.name)
			}
			assertPlanEchoed(t, got.GetPlan(), tc.want, "GetPlan "+tc.name)
		})
	}
}

// TestGetPlanIdentifierRulesLockBeforeDB：两个标识都没给（或码只有空白）必须报
// ErrPlanIdentifierRequired，且发生在任何读之前；真的没有这行才是 found=false。
func TestGetPlanIdentifierRulesLockBeforeDB(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedPlan(db, catalogPlan(81, "monthly", model.PlanStateOnSale, model.VipTypePremium, model.PlatformBitAndroid))

	cases := []struct {
		name string
		in   *rpc.GetPlanReq
	}{
		{"什么都不给", &rpc.GetPlanReq{}},
		{"码是空白", &rpc.GetPlanReq{PlanCode: "   "}},
		{"负的 plan_id 加空码", &rpc.GetPlanReq{PlanId: -7}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := db.planFindOneCalls + db.planFindByCodeCalls
			got, err := NewGetPlanLogic(context.Background(), svcCtx).GetPlan(tc.in)
			mustErrIs(t, err, model.ErrPlanIdentifierRequired, "GetPlan "+tc.name)
			if got != nil {
				t.Errorf("标识缺失时不得返回应答结构，实得 %+v", got)
			}
			if db.planFindOneCalls+db.planFindByCodeCalls != before {
				t.Fatalf("%s 仍在缺标识时触库，期望守卫先于读", tc.name)
			}
		})
	}

	// 给了标识但确实没有这行：found=false + plan=nil，不是错误。
	got, err := NewGetPlanLogic(context.Background(), svcCtx).GetPlan(&rpc.GetPlanReq{PlanCode: "never_created"})
	mustNoError(t, err, "GetPlan 不存在的码应是结论不是错误")
	if got.GetFound() || got.GetPlan() != nil {
		t.Errorf("没有这行却回了 found=%v plan=%+v", got.GetFound(), got.GetPlan())
	}
}

// TestGetPlanReadFailureIsNotNotFound：读不动不能伪装成「套餐不存在」，
// 否则 trade-order 会把一次 DB 故障判成「这单不能下」并永久拒掉一个合法套餐。
func TestGetPlanReadFailureIsNotNotFound(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedPlan(db, catalogPlan(91, "monthly", model.PlanStateOnSale, model.VipTypePremium, model.PlatformBitAndroid))
	db.planErr = errPlanDown

	for _, tc := range []struct {
		name string
		in   *rpc.GetPlanReq
	}{
		{"按 id 读失败", &rpc.GetPlanReq{PlanId: 91}},
		{"按 code 读失败", &rpc.GetPlanReq{PlanCode: "monthly"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewGetPlanLogic(context.Background(), svcCtx).GetPlan(tc.in)
			if !errors.Is(err, errPlanDown) {
				t.Fatalf("错误必须原样上抛，实得 %v", err)
			}
			if got != nil {
				t.Errorf("读失败不得返回 found=false 冒充不存在，实得 %+v", got)
			}
		})
	}
}

// --- ListPlansAdmin ---

// TestListPlansAdminIncludesDraftAndOffSale：运营面是草稿/下架的唯一可见通道，
// 并且按 plan_id 倒序（新建在前），total 是过滤后的总数而不是本页条数。
func TestListPlansAdminIncludesDraftAndOffSale(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedPlan(db, catalogPlan(101, "a_draft", model.PlanStateDraft, model.VipTypePremium, model.PlatformBitAndroid))
	seedPlan(db, catalogPlan(102, "b_onsale", model.PlanStateOnSale, model.VipTypePremiumPlus, model.PlatformBitIOS))
	off := seedPlan(db, catalogPlan(103, "c_off", model.PlanStateOffSale, model.VipTypePremium, model.PlatformBitWeb))

	got, err := NewListPlansAdminLogic(context.Background(), svcCtx).ListPlansAdmin(&rpc.ListPlansAdminReq{})
	mustNoError(t, err, "ListPlansAdmin 全量")
	if got.GetTotal() != 3 {
		t.Errorf("total = %d，期望 3（含草稿与已下架）", got.GetTotal())
	}
	if len(got.GetPlans()) != 3 {
		t.Fatalf("本页 %d 行，期望 3", len(got.GetPlans()))
	}
	// 倒序：plan_id 最大的在前，逐行核对来源。
	if got.GetPlans()[0].GetPlanCode() != "c_off" {
		t.Errorf("首行 = %s，期望 plan_id 倒序的 c_off", got.GetPlans()[0].GetPlanCode())
	}
	assertPlanEchoed(t, got.GetPlans()[0], off, "ListPlansAdmin 首行")
	if db.txRuns != 0 || len(db.planLogs) != 0 {
		t.Errorf("运营读接口有写入残留：tx=%d planLog=%d", db.txRuns, len(db.planLogs))
	}
}

// TestListPlansAdminFiltersReachModel：状态/档位/关键字必须原样进查询条件，
// 关键字是前缀匹配（真 SQL 的 LIKE 'kw%'，通配符被转义），不是子串匹配。
func TestListPlansAdminFiltersReachModel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedPlan(db, catalogPlan(111, "monthly_a", model.PlanStateOnSale, model.VipTypePremium, model.PlatformBitAndroid))
	yearly := seedPlan(db, catalogPlan(112, "yearly_b", model.PlanStateOnSale, model.VipTypePremiumPlus, model.PlatformBitAndroid))
	// 名字前缀也能命中（name 是表级 unicode_ci，大小写折叠）：这行 plan_code 不含关键字。
	byName := catalogPlan(113, "sku_c", model.PlanStateDraft, model.VipTypePremium, model.PlatformBitAndroid)
	byName.Name = "季度包"
	seedPlan(db, byName)

	cases := []struct {
		name string
		in   *rpc.ListPlansAdminReq
		// 过滤条件落到 model 查询里的形态
		wantState, wantVip int32
		wantKeyword        string
		wantCodes          []string
		wantTotal          int64
	}{
		{
			name:        "只看草稿",
			in:          &rpc.ListPlansAdminReq{State: rpc.PlanSaleState_PLAN_SALE_STATE_DRAFT},
			wantState:   model.PlanStateDraft,
			wantCodes:   []string{"sku_c"},
			wantTotal:   1,
			wantKeyword: "",
		},
		{
			name:        "只看超级大会员",
			in:          &rpc.ListPlansAdminReq{VipType: rpc.VipType_VIP_TYPE_PREMIUM_PLUS},
			wantVip:     model.VipTypePremiumPlus,
			wantCodes:   []string{"yearly_b"},
			wantTotal:   1,
			wantKeyword: "",
		},
		{
			name:        "关键字按 plan_code 前缀",
			in:          &rpc.ListPlansAdminReq{Keyword: "mon"},
			wantCodes:   []string{"monthly_a"},
			wantTotal:   1,
			wantKeyword: "mon",
		},
		{
			name:        "关键字按 name 前缀且不区分大小写",
			in:          &rpc.ListPlansAdminReq{Keyword: "季度"},
			wantCodes:   []string{"sku_c"},
			wantTotal:   1,
			wantKeyword: "季度",
		},
		{
			name:        "关键字是子串时不命中（前缀匹配）",
			in:          &rpc.ListPlansAdminReq{Keyword: "ly_b"},
			wantCodes:   nil,
			wantTotal:   0,
			wantKeyword: "ly_b",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewListPlansAdminLogic(context.Background(), svcCtx).ListPlansAdmin(tc.in)
			mustNoError(t, err, "ListPlansAdmin "+tc.name)
			q := db.lastPlanQuery
			if q.State != tc.wantState || q.VipType != tc.wantVip || strings.TrimSpace(q.Keyword) != tc.wantKeyword {
				t.Fatalf("查询条件 = %+v，期望 state=%d vip=%d keyword=%q",
					q, tc.wantState, tc.wantVip, tc.wantKeyword)
			}
			if got.GetTotal() != tc.wantTotal {
				t.Errorf("total = %d，期望 %d", got.GetTotal(), tc.wantTotal)
			}
			assertCodesInOrder(t, planCodesOf(got.GetPlans()), tc.wantCodes)
			if tc.wantCodes == nil && got.GetPlans() == nil {
				t.Errorf("空结果应是空列表而不是 nil（客户端要能区分「没查到」与「没返回字段」）")
			}
		})
	}
	// 顺带确认夹具本身没错：yearly_b 的投影逐字段可比。
	got, err := NewListPlansAdminLogic(context.Background(), svcCtx).ListPlansAdmin(
		&rpc.ListPlansAdminReq{VipType: rpc.VipType_VIP_TYPE_PREMIUM_PLUS})
	mustNoError(t, err, "ListPlansAdmin 档位过滤复查")
	assertPlanEchoed(t, got.GetPlans()[0], yearly, "ListPlansAdmin 档位过滤")
}

// TestListPlansAdminPagingIsClampedAndEchoed：超限的 page/size 裁剪后回显实际值，
// offset 必须按裁剪后的 size 算，否则运营点第 3 页会拿到重叠或空洞的一页。
func TestListPlansAdminPagingIsClampedAndEchoed(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	for i := int64(1); i <= 5; i++ {
		seedPlan(db, catalogPlan(120+i, fmt.Sprintf("sku_%d", i), model.PlanStateOnSale,
			model.VipTypePremium, model.PlatformBitAndroid))
	}

	cases := []struct {
		name          string
		page, size    int64
		wantPage      int64
		wantSize      int64
		wantOffset    int64
		wantRows      int
		wantTotalEcho int64
	}{
		{"size 超上限夹到 100", 1, 9999, 1, 100, 0, 5, 5},
		{"page/size 双零用缺省", 0, 0, 1, 20, 0, 5, 5},
		{"第二页按裁剪后的 size 偏移", 2, 2, 2, 2, 2, 2, 5},
		{"offset 越界是空页不是错误", 9, 2, 9, 2, 16, 0, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewListPlansAdminLogic(context.Background(), svcCtx).
				ListPlansAdmin(&rpc.ListPlansAdminReq{Page: tc.page, Size: tc.size})
			mustNoError(t, err, "ListPlansAdmin "+tc.name)
			if got.GetPage() != tc.wantPage || got.GetSize() != tc.wantSize {
				t.Errorf("回显 page/size = %d/%d，期望 %d/%d",
					got.GetPage(), got.GetSize(), tc.wantPage, tc.wantSize)
			}
			if db.lastPlanQuery.Offset != tc.wantOffset || db.lastPlanQuery.Limit != tc.wantSize {
				t.Fatalf("落到 SQL 的 offset/limit = %d/%d，期望 %d/%d",
					db.lastPlanQuery.Offset, db.lastPlanQuery.Limit, tc.wantOffset, tc.wantSize)
			}
			if len(got.GetPlans()) != tc.wantRows {
				t.Errorf("本页 %d 行，期望 %d 行", len(got.GetPlans()), tc.wantRows)
			}
			if got.GetTotal() != tc.wantTotalEcho {
				t.Errorf("total = %d，期望 %d（total 是过滤后的总数，不是本页条数）", got.GetTotal(), tc.wantTotalEcho)
			}
		})
	}
}

// TestListPlansAdminRejectsIllegalEnumBeforeQuery：非法状态/档位枚举必须在读库之前拒掉，
// 否则等于允许「用一个不存在的状态过滤出空列表」冒充「运营就是没有这种套餐」。
func TestListPlansAdminRejectsIllegalEnumBeforeQuery(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedPlan(db, catalogPlan(131, "monthly", model.PlanStateOnSale, model.VipTypePremium, model.PlatformBitAndroid))

	cases := []struct {
		name string
		in   *rpc.ListPlansAdminReq
		want error
	}{
		{"状态越界", &rpc.ListPlansAdminReq{State: rpc.PlanSaleState(40)}, model.ErrInvalidPlanState},
		{"档位越界", &rpc.ListPlansAdminReq{VipType: rpc.VipType(8)}, model.ErrInvalidVipType},
		{"档位负数", &rpc.ListPlansAdminReq{VipType: rpc.VipType(-2)}, model.ErrInvalidVipType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := db.planListAdminCalls
			got, err := NewListPlansAdminLogic(context.Background(), svcCtx).ListPlansAdmin(tc.in)
			mustErrIs(t, err, tc.want, "ListPlansAdmin "+tc.name)
			if got != nil {
				t.Errorf("守卫未过不得返回应答，实得 %+v", got)
			}
			if db.planListAdminCalls != before {
				t.Fatalf("非法枚举前已有 %d 次查询触库，守卫必须更早", db.planListAdminCalls-before)
			}
		})
	}
}

// TestListPlansAdminReadFailurePropagates：COUNT/列表读失败必须上抛，
// 不能退成 total=0 的空列表（那会让运营以为套餐被删光并重建，造成真实事故）。
func TestListPlansAdminReadFailurePropagates(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedPlan(db, catalogPlan(141, "monthly", model.PlanStateOnSale, model.VipTypePremium, model.PlatformBitAndroid))
	db.planErr = errPlanDown

	got, err := NewListPlansAdminLogic(context.Background(), svcCtx).ListPlansAdmin(&rpc.ListPlansAdminReq{})
	if !errors.Is(err, errPlanDown) {
		t.Fatalf("读失败必须原样上抛，实得 %v", err)
	}
	if got != nil {
		t.Errorf("读失败不得返回 total=0 冒充空目录，实得 %+v", got)
	}
}

// --- ListEntitlements ---

// catalogEnt 造一行权益码。
func catalogEnt(id int64, code string, minVip int32, enabled int32) *model.Entitlement {
	now := nowSec()
	return &model.Entitlement{
		EntitlementID: id, Code: code, Name: "能力-" + code, Description: "说明-" + code,
		MinVipType: minVip, Enabled: enabled, Version: 2, UpdatedBy: entOp,
		Ctime: now - 30*testDay, Mtime: now - testDay,
	}
}

// TestListEntitlementsKeepsDisabledRows：默认必须带出 enabled=0 的历史码，
// 否则判定侧/运营无法区分「传错码」(CODE_UNKNOWN) 与「运营关掉了」(CODE_DISABLED)。
func TestListEntitlementsKeepsDisabledRows(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	premium := seedEntitlement(db, catalogEnt(151, "vip.high_bitrate", model.VipTypePremium, 1))
	off := seedEntitlement(db, catalogEnt(152, "vip.legacy_offline", model.VipTypePremium, 0))
	plus := seedEntitlement(db, catalogEnt(153, "vip.ultra_4k", model.VipTypePremiumPlus, 1))

	got, err := NewListEntitlementsLogic(context.Background(), svcCtx).
		ListEntitlements(&rpc.ListEntitlementsReq{EnabledOnly: false})
	mustNoError(t, err, "ListEntitlements 全量")
	if len(got.GetEntitlements()) != 3 {
		t.Fatalf("返回 %d 个码，期望 3 个（含停用码）", len(got.GetEntitlements()))
	}
	// 真 SQL 按 (min_vip_type, entitlement_id) 升序：投影顺序必须与来源行一致。
	want := []*model.Entitlement{premium, off, plus}
	for i, e := range got.GetEntitlements() {
		assertEntitlementEchoed(t, e, want[i], fmt.Sprintf("ListEntitlements[%d]", i))
	}
	if db.entListCalls != 1 || db.lastEntListEnabled {
		t.Errorf("目录读 = %d 次（enabledOnly=%v），期望 1 次不过滤", db.entListCalls, db.lastEntListEnabled)
	}
	if db.txRuns != 0 {
		t.Errorf("读接口开了 %d 次事务", db.txRuns)
	}
}

// TestListEntitlementsEnabledOnlyFlagReachesQuery：enabled_only=true 只能靠过滤实现，
// 不得靠「读全量再在 logic 里丢掉停用行」——那样目录里有多少停用码就白读多少。
func TestListEntitlementsEnabledOnlyFlagReachesQuery(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedEntitlement(db, catalogEnt(161, "vip.a", model.VipTypePremium, 1))
	seedEntitlement(db, catalogEnt(162, "vip.b", model.VipTypePremium, 0))
	seedEntitlement(db, catalogEnt(163, "vip.c", model.VipTypePremiumPlus, 0))

	got, err := NewListEntitlementsLogic(context.Background(), svcCtx).
		ListEntitlements(&rpc.ListEntitlementsReq{EnabledOnly: true})
	mustNoError(t, err, "ListEntitlements 只看在用")
	if !db.lastEntListEnabled {
		t.Fatalf("enabled_only 没传进查询（在 logic 里过滤），实得 false")
	}
	if len(got.GetEntitlements()) != 1 || got.GetEntitlements()[0].GetCode() != "vip.a" {
		t.Fatalf("只看在用得 %d 行 %+v，期望仅 vip.a", len(got.GetEntitlements()), got.GetEntitlements())
	}
	for _, e := range got.GetEntitlements() {
		if !e.GetEnabled() {
			t.Errorf("%s 在 enabled_only 结果里却是停用状态", e.GetCode())
		}
	}
}

// TestListEntitlementsEmptyCatalogAndReadFailure：空目录是正常结论，读失败是错误。
func TestListEntitlementsEmptyCatalogAndReadFailure(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	got, err := NewListEntitlementsLogic(context.Background(), svcCtx).ListEntitlements(&rpc.ListEntitlementsReq{})
	mustNoError(t, err, "ListEntitlements 空目录")
	if got.GetEntitlements() == nil || len(got.GetEntitlements()) != 0 {
		t.Errorf("空目录应是空列表，实得 %+v", got.GetEntitlements())
	}

	db.entErr = errCatalogDown
	reply, err := NewListEntitlementsLogic(context.Background(), svcCtx).ListEntitlements(&rpc.ListEntitlementsReq{})
	if !errors.Is(err, errCatalogDown) {
		t.Fatalf("目录读失败必须上抛，实得 %v", err)
	}
	if reply != nil {
		t.Errorf("目录读失败不得返回空列表冒充「目录就是空的」，实得 %+v", reply)
	}
}

// 缺陷：model/entitlement.go:176 —— List 写死 `ORDER BY min_vip_type ASC, entitlement_id ASC
// LIMIT 500`，而 rpc/membership.proto:378-380 的 ListEntitlementsReply 只有 entitlements 一个字段
// （无 total/page/size），listentitlementslogic.go:31,36 又把行数原样转出去 ——
// 目录超过 500 个码时，运营面和 GetMembership 的权益投影都会静默少给排序尾部的码，
// 调用方无从察觉，判定上表现为「这个码不存在」(CODE_UNKNOWN) 而不是「目录被截断」。
// 修法方向：给契约加分页 + total（与 ListPlansAdminReply 同口径），或在 reply 回显 truncated。
// 本用例钉住的是当前（错误）行为：第 501 个码在应答里消失，且没有任何截断信号。
func TestListEntitlementsSilentlyTruncatesCodeAtSQLRowLimit(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	const total = entListSQLRowLimit + 1
	for i := 1; i <= total; i++ {
		seedEntitlement(db, catalogEnt(int64(i), fmt.Sprintf("vip.code_%03d", i), model.VipTypePremium, 1))
	}

	got, err := NewListEntitlementsLogic(context.Background(), svcCtx).
		ListEntitlements(&rpc.ListEntitlementsReq{EnabledOnly: true})
	mustNoError(t, err, "ListEntitlements 超上限目录")
	if len(got.GetEntitlements()) != entListSQLRowLimit {
		t.Fatalf("返回 %d 个码，期望钉住被截断成 %d 的现状", len(got.GetEntitlements()), entListSQLRowLimit)
	}
	// 截断掉的是排序尾部那一个（而不是中间随机一个）：首行与末行都是可观察证据。
	first, last := got.GetEntitlements()[0].GetCode(), got.GetEntitlements()[entListSQLRowLimit-1].GetCode()
	if first != "vip.code_001" || last != fmt.Sprintf("vip.code_%03d", entListSQLRowLimit) {
		t.Fatalf("保留的区间是 %s..%s，期望 vip.code_001..vip.code_%03d",
			first, last, entListSQLRowLimit)
	}
	if _, dup := db.ents[fmt.Sprintf("vip.code_%03d", total)]; !dup {
		t.Fatalf("库里第 %d 个码没 seed 上，本用例就失去意义了", total)
	}
	for _, e := range got.GetEntitlements() {
		if e.GetCode() == fmt.Sprintf("vip.code_%03d", total) {
			t.Fatalf("第 %d 个码竟然返回了：说明 model 的 LIMIT 500 已被移除，本用例应随之删除", total)
		}
	}
}

// --- 小工具 ---

// planCodesOf 把应答里的 plan_code 抽成列表，便于按顺序断言。
func planCodesOf(plans []*rpc.PlanInfo) []string {
	out := make([]string, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.GetPlanCode())
	}
	return out
}

// assertCodesInOrder 按顺序比对标识列表（nil 期望 = 空结果）。
func assertCodesInOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("返回 %d 行 %v，期望 %d 行 %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 行 = %s，期望 %s（顺序也是口径的一部分）", i, got[i], want[i])
		}
	}
}
