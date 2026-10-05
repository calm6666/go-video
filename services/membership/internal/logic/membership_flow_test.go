package logic

// 会员写入三接口（GrantMembership / RevokeMembership / ExpireMembership）的流程口径测试。
//
// 这三条是「资金语义」的正面（AGENTS.md §1）：会员是否生效只由 mb_grant 台账 + 它投影出的
// mb_membership 行决定，因此这里逐条钉住：
//   - 身份行与台账必须同一事务提交；任何一步失败都不许留下「加了时长没台账」或「有台账没加时长」；
//   - request_id 命中即重放（绝不二次加/扣时长），同 request_id 换关键参数（含本轮审计字段
//     plan_id / biz_order_no / payment_no）一律 ErrRequestIdReused；
//   - CAS 不命中返回 ErrConcurrentUpdate 并整体回滚，而不是覆盖并发写入；
//   - 幂等预读、身份读、目录读「失败」必须上抛，不许被折叠成「没开通 / 没台账 / 可跳过」；
//   - 到期是幂等事件：ExpireMembership 只补台账，一个字都不改 mb_membership。
//
// 并发与唯一键冲突用「语义等价」的假实现复现（见 fakes_test.go 顶部说明）：
// 冲突即返回 1062 报文、CAS 条件不命中即 RowsAffected=0，logic 面对这些返回值的裁决才是被测对象。

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go-video/services/membership/model"
	"go-video/services/membership/rpc"
)

const (
	opsReason   = "客服补偿：录制中断"
	revokeTrail = "退款回收 SO20260921001"
	bizOrder    = "SO20260921001"
	paymentRef  = "PAY20260921001"
)

// --- 夹具 ---

func opsGrantReq(mid int64, days int32, requestID string) *rpc.GrantMembershipReq {
	return &rpc.GrantMembershipReq{
		Mid: mid, VipType: rpc.VipType_VIP_TYPE_PREMIUM, DeltaDays: days,
		Source: rpc.GrantSource_GRANT_SOURCE_ADMIN_OPS, Reason: opsReason,
		Operator: "ops-1", RequestId: requestID,
	}
}

func paidGrantReq(mid int64, days int32, requestID string) *rpc.GrantMembershipReq {
	return &rpc.GrantMembershipReq{
		Mid: mid, VipType: rpc.VipType_VIP_TYPE_PREMIUM, DeltaDays: days,
		Source:     rpc.GrantSource_GRANT_SOURCE_SANDBOX_PURCHASE,
		BizOrderNo: bizOrder, PaymentNo: paymentRef,
		Operator: "trade-order", RequestId: requestID,
	}
}

func revokeReq(mid int64, requestID string) *rpc.RevokeMembershipReq {
	return &rpc.RevokeMembershipReq{
		Mid: mid, VipType: rpc.VipType_VIP_TYPE_PREMIUM, ClearRemaining: true,
		Operator: "ops-1", RequestId: requestID, Reason: revokeTrail,
	}
}

func expireReq(mid int64, requestID string) *rpc.ExpireMembershipReq {
	return &rpc.ExpireMembershipReq{
		Mid: mid, VipType: rpc.VipType_VIP_TYPE_PREMIUM,
		Operator: "cron", RequestId: requestID,
	}
}

func mustNoError(t *testing.T, err error, op string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s 不该报错：%v", op, err)
	}
}

func mustErrIs(t *testing.T, got, want error, op string) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("%s 实得 %v，期望 %v", op, got, want)
	}
}

// --- GrantMembership ---

// TestGrantFirstTimeWritesIdentityAndLedgerTogether 首次开通：身份行与台账一起出现，
// 时长从 now 起算（不是从某个历史 expire_at 起算），付费来源才计付费月数。
func TestGrantFirstTimeWritesIdentityAndLedgerTogether(t *testing.T) {
	svcCtx, db := newTestSvc(t)

	got, err := NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(paidGrantReq(2001, 31, "g-1"))
	mustNoError(t, err, "GrantMembership")
	if got.GetDuplicated() {
		t.Error("首次开通却报 duplicated")
	}
	if got.GetMembership().GetStartAt() == 0 {
		t.Error("start_at 未落")
	}
	if span := got.GetMembership().GetExpireAt() - got.GetMembership().GetStartAt(); span != 31*day {
		t.Errorf("首次时长 = %d 秒（%d 天），期望 31 天", span, span/day)
	}
	if got.GetMembership().GetVersion() != 1 {
		t.Errorf("新行 version = %d，期望 1", got.GetMembership().GetVersion())
	}
	if got.GetMembership().GetPaidMonthCount() != model.MonthsForDeltaDays(31) {
		t.Errorf("付费来源 paid_month_count = %d，期望 %d",
			got.GetMembership().GetPaidMonthCount(), model.MonthsForDeltaDays(31))
	}

	if len(db.grants) != 1 || len(db.members) != 1 {
		t.Fatalf("身份行/台账数 = %d/%d，期望 1/1（同一事务）", len(db.members), len(db.grants))
	}
	g := db.grantByRequest("g-1")
	if g == nil {
		t.Fatal("没有台账，判定侧将永远读不到这笔开通")
	}
	if g.Action != model.ActionGrant {
		t.Errorf("首次动作台账 = %s，期望 %s", g.Action, model.ActionGrant)
	}
	if g.BeforeExpireAt != 0 || g.AfterExpireAt != got.GetMembership().GetExpireAt() {
		t.Errorf("台账前后到期 = %d->%d，期望 0->%d", g.BeforeExpireAt, g.AfterExpireAt, got.GetMembership().GetExpireAt())
	}
	if g.DeltaDays != 31 || g.Source != model.GrantSourceSandboxPurchase ||
		g.BizOrderNo != bizOrder || g.PaymentNo != paymentRef || g.Operator != "trade-order" {
		t.Errorf("台账审计位不全： %+v", g)
	}
	if g.Reason != "" {
		t.Errorf("付费来源的 reason 未提供却写了值：%q", g.Reason)
	}
	if got.GetGrantId() != g.GrantID {
		t.Errorf("reply.grant_id = %d，台账 grant_id = %d", got.GetGrantId(), g.GrantID)
	}
}

// TestGrantOnExpiredIdentityRestartsFromNow 是「续费吞掉已享受时间」的反例守卫：
// 过期身份必须从 now 重新起算，而不是在旧 expire_at 上顺延。
func TestGrantOnExpiredIdentityRestartsFromNow(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	old := seedMembership(db, &model.Membership{
		Mid: 2101, VipType: model.VipTypePremium, StartAt: now - 100*day, ExpireAt: now - 10*day,
		Source: model.GrantSourceSandboxPurchase, PaidMonthCount: 3, AutoRenew: 0,
	})

	got, err := NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(opsGrantReq(2101, 31, "g-2"))
	mustNoError(t, err, "GrantMembership")
	if span := got.GetMembership().GetExpireAt() - now; span < 30*day || span > 31*day {
		t.Errorf("过期身份续 31 天后剩余 %d 秒，期望约 31 天（不得从 %d 起算）", span, old.ExpireAt)
	}
	if got.GetMembership().GetStartAt() != old.StartAt {
		t.Error("start_at 被改写：首次开通时间必须只写一次")
	}
	if got.GetMembership().GetPaidMonthCount() != 3 {
		t.Errorf("运营赠送却累加了付费月数：%d", got.GetMembership().GetPaidMonthCount())
	}
	g := db.grantByRequest("g-2")
	if g == nil || g.Action != model.ActionExtend || g.BeforeExpireAt != old.ExpireAt {
		t.Errorf("台账应为 EXTEND 且 before=旧到期，得到 %+v", g)
	}
	if db.members[memberKey(2101, model.VipTypePremium)].Version != old.Version+1 {
		t.Error("身份行未按 CAS 递增 version")
	}
}

// TestGrantOnActiveIdentityExtendsOnTop 未过期则在原到期时间上顺延。
func TestGrantOnActiveIdentityExtendsOnTop(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	old := seedMembership(db, &model.Membership{
		Mid: 2201, VipType: model.VipTypePremium, StartAt: now - 5*day, ExpireAt: now + 5*day, PaidMonthCount: 1,
	})

	got, err := NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(paidGrantReq(2201, 31, "g-3"))
	mustNoError(t, err, "GrantMembership")
	if span := got.GetMembership().GetExpireAt() - old.ExpireAt; span != 31*day {
		t.Errorf("生效中续期增量 = %d 秒，期望 31 天（从旧 expire_at 顺延）", span)
	}
	if got.GetMembership().GetPaidMonthCount() != 2 {
		t.Errorf("付费来源续期后 paid_month_count = %d，期望 2", got.GetMembership().GetPaidMonthCount())
	}
}

// TestGrantPaidSourcesMustCarryPaymentTrace 是 AGENTS.md §1 的直接体现：
// 无凭据的付费开通等于凭空造权益，必须拒掉且不留下任何一行。
func TestGrantPaidSourcesMustCarryPaymentTrace(t *testing.T) {
	cases := []struct {
		name    string
		source  rpc.GrantSource
		wantErr error
	}{
		{"沙箱订单", rpc.GrantSource_GRANT_SOURCE_SANDBOX_PURCHASE, model.ErrGrantSourceNeedsOrder},
		{"沙箱自动续费", rpc.GrantSource_GRANT_SOURCE_SANDBOX_AUTO_RENEW, model.ErrGrantSourceNeedsOrder},
		{"存量迁移", rpc.GrantSource_GRANT_SOURCE_LEGACY_IMPORT, model.ErrGrantSourceNeedsOrder},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			in := opsGrantReq(2301, 31, "g-trace")
			in.Source = tc.source
			in.Reason = "" // 这三个来源不要求 reason，只要求可回溯

			_, err := NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(in)
			mustErrIs(t, err, tc.wantErr, "无凭据开通")
			if len(db.grants) != 0 || len(db.members) != 0 || db.txRuns != 0 {
				t.Errorf("校验未通过却动了库：台账 %d、身份 %d、事务 %d 次", len(db.grants), len(db.members), db.txRuns)
			}

			// 只带 payment_no（不带订单号）同样可回溯，必须放过。
			in.PaymentNo = paymentRef
			if _, err := NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(in); err != nil {
				t.Errorf("带资金流水号仍被拒：%v", err)
			}
		})
	}
}

// TestGrantOpsAndExperienceRequireReason 运营手工与体验发放是本服务唯一两类「无支付流水」授权，
// 无理由等于留下无人能解释的权益变更。
func TestGrantOpsAndExperienceRequireReason(t *testing.T) {
	for _, src := range []rpc.GrantSource{rpc.GrantSource_GRANT_SOURCE_ADMIN_OPS, rpc.GrantSource_GRANT_SOURCE_EXPERIENCE} {
		t.Run(src.String(), func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			in := opsGrantReq(2401, 7, "g-reason")
			in.Source = src
			in.Reason = "   "
			_, err := NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(in)
			mustErrIs(t, err, model.ErrReasonRequired, "无理由授权")
			if len(db.grants) != 0 {
				t.Error("被拒的授权仍留下了台账")
			}

			// 付费来源不强制 reason，但给了就必须守列宽。
			paid := paidGrantReq(2401, 7, "g-longreason")
			paid.Reason = strings.Repeat("凭", model.MaxReasonLength+1)
			_, err = NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(paid)
			mustErrIs(t, err, model.ErrFieldTooLong, "超长 reason")
		})
	}
}

func TestGrantRejectsNonPositiveDeltaAndBadIdentifiers(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	logic := NewGrantMembershipLogic(context.Background(), svcCtx)

	cases := []struct {
		name   string
		mutate func(*rpc.GrantMembershipReq)
		want   error
	}{
		{"零天", func(in *rpc.GrantMembershipReq) { in.DeltaDays = 0 }, model.ErrInvalidGrantDelta},
		{"负天数留给 REVOKE", func(in *rpc.GrantMembershipReq) { in.DeltaDays = -3 }, model.ErrInvalidGrantDelta},
		{"超出单次上限", func(in *rpc.GrantMembershipReq) { in.DeltaDays = 3661 }, model.ErrInvalidGrantDelta},
		{"缺 mid", func(in *rpc.GrantMembershipReq) { in.Mid = 0 }, model.ErrInvalidMid},
		{"缺档位", func(in *rpc.GrantMembershipReq) { in.VipType = rpc.VipType_VIP_TYPE_UNSPECIFIED }, model.ErrInvalidVipType},
		{"超管档位", func(in *rpc.GrantMembershipReq) { in.VipType = rpc.VipType(9) }, model.ErrInvalidVipType},
		{"缺幂等键", func(in *rpc.GrantMembershipReq) { in.RequestId = " " }, model.ErrRequestIdRequired},
		{"幂等键超长", func(in *rpc.GrantMembershipReq) { in.RequestId = strings.Repeat("r", maxRequestIDWidth+1) }, model.ErrFieldTooLong},
		{"缺操作者", func(in *rpc.GrantMembershipReq) { in.Operator = "" }, model.ErrOperatorRequired},
		{"订单号超长", func(in *rpc.GrantMembershipReq) { in.BizOrderNo = strings.Repeat("o", model.MaxBizNoLength+1) }, model.ErrFieldTooLong},
		{"流水号超长", func(in *rpc.GrantMembershipReq) { in.PaymentNo = strings.Repeat("p", model.MaxBizNoLength+1) }, model.ErrFieldTooLong},
		{"来源未指定", func(in *rpc.GrantMembershipReq) { in.Source = rpc.GrantSource_GRANT_SOURCE_UNSPECIFIED }, model.ErrGrantSourceRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := opsGrantReq(2501, 31, "g-bad")
			tc.mutate(in)
			if _, err := logic.GrantMembership(in); !errors.Is(err, tc.want) {
				t.Fatalf("实得 %v，期望 %v", err, tc.want)
			}
		})
	}
	if len(db.grants) != 0 || len(db.members) != 0 {
		t.Errorf("全部非法请求却写了台账/身份：%d/%d", len(db.grants), len(db.members))
	}
}

// TestGrantPlanMustExistMatchTierAndLeaveDraft 挂套餐的授予要与套餐一致：
// 买大会员的订单不能开出超级大会员，DRAFT 不可履约；套餐读不动必须上抛。
func TestGrantPlanMustExistMatchTierAndLeaveDraft(t *testing.T) {
	t.Run("档位不一致", func(t *testing.T) {
		svcCtx, db := newTestSvc(t)
		p := seedPlan(db, &model.Plan{PlanCode: "p1", VipType: model.VipTypePremium, State: model.PlanStateOnSale})
		in := opsGrantReq(2601, 31, "g-plan-tier")
		in.VipType = rpc.VipType_VIP_TYPE_PREMIUM_PLUS
		in.PlanId = p.PlanID
		_, err := NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(in)
		mustErrIs(t, err, model.ErrInvalidVipType, "跨档授予")
		if len(db.grants) != 0 {
			t.Error("被拒的跨档授予留下台账")
		}
	})
	t.Run("DRAFT 不可履约", func(t *testing.T) {
		svcCtx, db := newTestSvc(t)
		p := seedPlan(db, &model.Plan{PlanCode: "p2", VipType: model.VipTypePremium, State: model.PlanStateDraft})
		in := opsGrantReq(2601, 31, "g-plan-draft")
		in.PlanId = p.PlanID
		_, err := NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(in)
		mustErrIs(t, err, model.ErrInvalidPlanStateTransition, "DRAFT 履约")
	})
	t.Run("套餐不存在", func(t *testing.T) {
		svcCtx, _ := newTestSvc(t)
		in := opsGrantReq(2601, 31, "g-plan-missing")
		in.PlanId = 999
		if _, err := NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(in); !errors.Is(err, model.ErrPlanNotFound) {
			t.Fatalf("实得 %v，期望 ErrPlanNotFound", err)
		}
	})
	t.Run("套餐读失败不得当成无套餐", func(t *testing.T) {
		svcCtx, db := newTestSvc(t)
		seedPlan(db, &model.Plan{PlanCode: "p3", VipType: model.VipTypePremium, State: model.PlanStateOnSale})
		db.planErr = errPlanDown
		in := opsGrantReq(2601, 31, "g-plan-read")
		in.PlanId = 1
		if _, err := NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(in); !errors.Is(err, errPlanDown) {
			t.Fatalf("套餐读失败必须上抛，实得 %v", err)
		}
		if len(db.grants) != 0 {
			t.Error("套餐读不动却完成了开通")
		}
	})
}

// TestGrantReplayNeverAppliesDurationTwice 幂等快路径：同 request_id 重放不得二次加时长，
// 也不该再开一个事务。
func TestGrantReplayNeverAppliesDurationTwice(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	held := seedMembership(db, &model.Membership{
		Mid: 2701, VipType: model.VipTypePremium, StartAt: now - day, ExpireAt: now + 30*day,
		Source: model.GrantSourceAdminOps,
	})
	first := seedGrant(db, &model.Grant{
		Mid: 2701, VipType: model.VipTypePremium, Action: model.ActionGrant, DeltaDays: 31,
		Source: model.GrantSourceAdminOps, BeforeExpireAt: 0, AfterExpireAt: held.ExpireAt,
		Operator: "ops-1", RequestID: "g-replay", Reason: opsReason, Ctime: now - day,
	})

	got, err := NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(opsGrantReq(2701, 31, "g-replay"))
	mustNoError(t, err, "重放")
	if !got.GetDuplicated() {
		t.Error("命中同一 request_id 却没报 duplicated")
	}
	if got.GetGrantId() != first.GrantID {
		t.Errorf("重放回查的 grant_id = %d，期望首次 %d", got.GetGrantId(), first.GrantID)
	}
	if got.GetMembership().GetExpireAt() != held.ExpireAt {
		t.Errorf("重放改变了到期时间：%d != %d", got.GetMembership().GetExpireAt(), held.ExpireAt)
	}
	if len(db.grants) != 1 || db.txRuns != 0 {
		t.Errorf("重放又写了 %d 条台账 / 起了 %d 个事务，期望 0/0", len(db.grants)-1, db.txRuns)
	}
}

// TestGrantReplayRejectsChangedParameters 钉住「幂等键只能重放同一请求」。
// 每一项都单独改动：任何一项被放过，都等于允许用旧 request_id 改口径。
func TestGrantReplayRejectsChangedParameters(t *testing.T) {
	// 换一个「合法、在售、同档位」的套餐：这一路要撞的是幂等参数比对，不能被套餐校验先拦下。
	const otherOnSalePlanID int64 = 77
	mutations := []struct {
		name   string
		mutate func(*rpc.GrantMembershipReq)
	}{
		{"换 mid", func(in *rpc.GrantMembershipReq) { in.Mid = 2802 }},
		{"换档位", func(in *rpc.GrantMembershipReq) { in.VipType = rpc.VipType_VIP_TYPE_PREMIUM_PLUS }},
		{"换时长", func(in *rpc.GrantMembershipReq) { in.DeltaDays = 62 }},
		{"换套餐", func(in *rpc.GrantMembershipReq) { in.PlanId = otherOnSalePlanID }},
		{"换来源", func(in *rpc.GrantMembershipReq) { in.Source = rpc.GrantSource_GRANT_SOURCE_EXPERIENCE }},
		{"换订单号", func(in *rpc.GrantMembershipReq) { in.BizOrderNo = "SO-OTHER" }},
		{"换流水号", func(in *rpc.GrantMembershipReq) { in.PaymentNo = "PAY-OTHER" }},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			now := nowSec()
			seedPlan(db, &model.Plan{
				PlanID: otherOnSalePlanID, VipType: model.VipTypePremium, State: model.PlanStateOnSale,
				PriceMinor: 2500, Currency: "CNY", PlanCode: "plan-replay-other",
				Name: "另一个在售套餐", DurationDays: 31, UnitCount: 1,
			})
			held := seedMembership(db, &model.Membership{
				Mid: 2801, VipType: model.VipTypePremium, StartAt: now - day, ExpireAt: now + 30*day,
			})
			seedGrant(db, &model.Grant{
				Mid: 2801, VipType: model.VipTypePremium, Action: model.ActionGrant, DeltaDays: 31,
				Source: model.GrantSourceAdminOps, AfterExpireAt: held.ExpireAt,
				Operator: "ops-1", RequestID: "g-mut", Reason: opsReason, Ctime: now - day,
			})

			in := opsGrantReq(2801, 31, "g-mut")
			m.mutate(in)
			_, err := NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(in)
			mustErrIs(t, err, model.ErrRequestIdReused, "借幂等键改口径")
			if again := db.members[memberKey(2801, model.VipTypePremium)]; again.ExpireAt != held.ExpireAt {
				t.Error("冲突请求居然改变了到期时间")
			}
			if len(db.grants) != 1 {
				t.Errorf("冲突请求留下了台账：%d 条", len(db.grants))
			}
		})
	}
}

// TestGrantReplayWithoutIdentityRowIsError：台账在、身份行没了 = 数据被人工删过，
// 报伪成功会把事故藏起来。
func TestGrantReplayWithoutIdentityRowIsError(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	seedGrant(db, &model.Grant{
		Mid: 2901, VipType: model.VipTypePremium, Action: model.ActionGrant, DeltaDays: 31,
		Source: model.GrantSourceAdminOps, AfterExpireAt: now + 30*day,
		Operator: "ops-1", RequestID: "g-orphan", Reason: opsReason, Ctime: now - day,
	})

	_, err := NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(opsGrantReq(2901, 31, "g-orphan"))
	mustErrIs(t, err, model.ErrMembershipNotFound, "孤儿台账")
}

// TestGrantIdempotencyLookupFailurePropagates 幂等预读失败时既不能重放也不能直接写：
// 否则「查不到台账」会被当成「从没记过账」，一次 DB 抖动就能把时长加两遍。
func TestGrantIdempotencyLookupFailurePropagates(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	db.grantErr = errLedgerDown

	_, err := NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(opsGrantReq(3001, 31, "g-lookup"))
	mustErrIs(t, err, errLedgerDown, "幂等预读失败")
	if db.txRuns != 0 || len(db.grants) != 0 {
		t.Errorf("预读失败仍尝试写入：事务 %d 次、台账 %d 条", db.txRuns, len(db.grants))
	}

	// 身份行读不动同样必须上抛（事务内），且整体不留台账。
	svcCtx2, db2 := newTestSvc(t)
	db2.memberErr = errIdentityDown
	if _, err := NewGrantMembershipLogic(context.Background(), svcCtx2).GrantMembership(opsGrantReq(3002, 31, "g-member-read")); !errors.Is(err, errIdentityDown) {
		t.Fatalf("身份读失败必须上抛，实得 %v", err)
	}
	if len(db2.grants) != 0 || len(db2.members) != 0 {
		t.Errorf("身份读失败留下半条结果：台账 %d、身份 %d", len(db2.grants), len(db2.members))
	}
}

// TestGrantCasMissRollsBackLedger 并发已改动身份行：必须 ErrConcurrentUpdate，
// 且同事务的台账一并回滚（否则「有台账没加时长」，用户白等）。
func TestGrantCasMissRollsBackLedger(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	held := seedMembership(db, &model.Membership{
		Mid: 3101, VipType: model.VipTypePremium, StartAt: now - 40*day, ExpireAt: now + 10*day,
		Version: 4,
	})
	db.membershipConcurrentCommit = true

	_, err := NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(opsGrantReq(3101, 31, "g-cas"))
	mustErrIs(t, err, model.ErrConcurrentUpdate, "CAS 未命中")
	if len(db.grants) != 0 {
		t.Errorf("CAS 冲突后仍留下 %d 条台账（事务未回滚）", len(db.grants))
	}
	// 只断言「本次什么都没写进去」：错误本身即证明 UPDATE 条件按旧 version 没命中，
	// 否则事务会成功、台账会出现。并发方把 version 推到哪一类细节属于真库语义，不在单测里假装。
	after := db.members[memberKey(3101, model.VipTypePremium)]
	if after.ExpireAt != held.ExpireAt || after.Version != held.Version {
		t.Errorf("CAS 冲突却改动了身份行：expire %d->%d version %d->%d",
			held.ExpireAt, after.ExpireAt, held.Version, after.Version)
	}
}

// TestGrantConcurrentFirstCommitFallsBackToReplay 预读没看见、INSERT 时唯一索引才咬人：
// 必须回查首次结果并按重放处理（duplicated=true），而不是把 1062 抛给调用方。
func TestGrantConcurrentFirstCommitFallsBackToReplay(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	held := seedMembership(db, &model.Membership{
		Mid: 3201, VipType: model.VipTypePremium, StartAt: now - day, ExpireAt: now + 30*day,
	})
	rival := seedGrant(db, &model.Grant{
		Mid: 3201, VipType: model.VipTypePremium, Action: model.ActionGrant, DeltaDays: 31,
		Source: model.GrantSourceAdminOps, AfterExpireAt: held.ExpireAt,
		Operator: "ops-1", RequestID: "g-race", Reason: opsReason, Ctime: now,
	})
	db.grantPreReadMiss = true

	got, err := NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(opsGrantReq(3201, 31, "g-race"))
	mustNoError(t, err, "唯一索引兜底")
	if !got.GetDuplicated() || got.GetGrantId() != rival.GrantID {
		t.Errorf("并发首开未回查首次结果：duplicated=%v grant_id=%d，期望 true/%d",
			got.GetDuplicated(), got.GetGrantId(), rival.GrantID)
	}
	if len(db.grants) != 1 {
		t.Errorf("台账数 = %d，期望仍为 1", len(db.grants))
	}
	if db.members[memberKey(3201, model.VipTypePremium)].ExpireAt != held.ExpireAt {
		t.Error("回查重放却二次改了到期时间")
	}
}

// TestGrantIdentityCreatedConcurrentlyIsConflict 身份行的唯一键先咬人、幂等键却没冲突时，
// 只能报 ErrConcurrentUpdate 让调用方重读重试——绝不能凭空写台账。
func TestGrantIdentityCreatedConcurrentlyIsConflict(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	rival := seedMembership(db, &model.Membership{
		Mid: 3301, VipType: model.VipTypePremium, StartAt: now, ExpireAt: now + 31*day,
		Source: model.GrantSourceSandboxPurchase,
	})
	db.membershipAppearsOnInsert = true // 事务内读不到、INSERT 时才撞 uniq_mid_vip_type

	_, err := NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(opsGrantReq(3301, 31, "g-ghost"))
	mustErrIs(t, err, model.ErrConcurrentUpdate, "身份行并发创建")
	if len(db.grants) != 0 {
		t.Errorf("冲突请求留下了 %d 条台账", len(db.grants))
	}
	// 撞唯一键时不得覆盖并发方那一行——否则两个订单的时长会互相吞掉。
	after := db.members[memberKey(3301, model.VipTypePremium)]
	if after.ExpireAt != rival.ExpireAt || after.Version != rival.Version {
		t.Errorf("并发首开行被覆盖：expire %d->%d version %d->%d",
			rival.ExpireAt, after.ExpireAt, rival.Version, after.Version)
	}
}

// --- RevokeMembership ---

// TestRevokeClearRemainingInvalidatesAndUnsigns 立即失效 + 签约位一并清零：
// 「已收回却被自动续费重新延长」是自相矛盾的状态。paid_month_count 不回退。
func TestRevokeClearRemainingInvalidatesAndUnsigns(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	seedMembership(db, &model.Membership{
		Mid: 4001, VipType: model.VipTypePremium, StartAt: now - 40*day, ExpireAt: now + 20*day,
		AutoRenew: 1, AutoRenewChannel: model.AllowedAutoRenewChannel, AutoRenewSignedAt: now - 30*day,
		Source: model.GrantSourceSandboxPurchase, PaidMonthCount: 2,
	})

	in := revokeReq(4001, "r-1")
	in.PlanId = 55
	in.BizOrderNo = bizOrder
	in.PaymentNo = paymentRef
	got, err := NewRevokeMembershipLogic(context.Background(), svcCtx).RevokeMembership(in)
	mustNoError(t, err, "RevokeMembership")
	if got.GetDuplicated() {
		t.Error("首次收回报 duplicated")
	}
	if span := got.GetMembership().GetExpireAt() - now; span < 0 || span > 2 {
		t.Errorf("clear_remaining 后剩余 %d 秒，期望停在 now", span)
	}
	if got.GetMembership().GetAutoRenew() {
		t.Error("已无有效时长却仍留着签约位")
	}
	if got.GetMembership().GetAutoRenewChannel() != "" {
		t.Errorf("解约后渠道未清空：%q", got.GetMembership().GetAutoRenewChannel())
	}
	if got.GetMembership().GetPaidMonthCount() != 2 {
		t.Errorf("paid_month_count 被回退成 %d，期望保持 2（付费事实不可篡改）", got.GetMembership().GetPaidMonthCount())
	}

	g := db.grantByRequest("r-1")
	if g == nil {
		t.Fatal("收回没有台账，审计页将看不到这次权益终止")
	}
	if g.Action != model.ActionRevoke || g.DeltaDays != 0 || g.Source != model.GrantSourceAdminOps {
		t.Errorf("收回台账口径不符：%+v", g)
	}
	if g.BeforeExpireAt <= g.AfterExpireAt {
		t.Errorf("台账前后到期未体现扣减：%d -> %d", g.BeforeExpireAt, g.AfterExpireAt)
	}
	if g.PlanID != 55 || g.BizOrderNo != bizOrder || g.PaymentNo != paymentRef || g.Reason != revokeTrail {
		t.Errorf("追溯位丢失，退款流程无法对账：%+v", g)
	}
	if len(db.grants) != 1 || len(db.members) != 1 {
		t.Errorf("行数异常：台账 %d、身份 %d", len(db.grants), len(db.members))
	}
}

// TestRevokeOverdraftStopsAtNow 扣过头停在 now，不制造早于今天的负余额——
// 那会让下次 NextExpireAt 从错误的过去时刻起算，等于白送时长。
func TestRevokeOverdraftStopsAtNow(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	before := seedMembership(db, &model.Membership{
		Mid: 4101, VipType: model.VipTypePremium, StartAt: now - 10*day, ExpireAt: now + 10*day,
	})

	in := revokeReq(4101, "r-delta")
	in.ClearRemaining = false
	in.DeltaDays = 30
	got, err := NewRevokeMembershipLogic(context.Background(), svcCtx).RevokeMembership(in)
	mustNoError(t, err, "RevokeMembership")
	if span := got.GetMembership().GetExpireAt() - now; span < 0 || span > 2 {
		t.Errorf("扣过头后到期时间落在 now 之前 %d 秒，期望停在 now", span)
	}
	g := db.grantByRequest("r-delta")
	if g.DeltaDays != -30 {
		t.Errorf("台账 delta_days = %d，期望记请求意图 -30（实际影响看 before/after）", g.DeltaDays)
	}
	if g.BeforeExpireAt != before.ExpireAt {
		t.Errorf("台账 before_expire_at = %d，期望 %d", g.BeforeExpireAt, before.ExpireAt)
	}
}

// TestRevokeAlreadyExpiredWritesNoOpLedger 已过期的身份：动作合法但无时长可扣，
// 照实记一条 before==after 的台账并原样返回——退款流程不该被「收回失败」卡住。
func TestRevokeAlreadyExpiredWritesNoOpLedger(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	expired := seedMembership(db, &model.Membership{
		Mid: 4201, VipType: model.VipTypePremium, StartAt: now - 100*day, ExpireAt: now - 5*day,
	})

	got, err := NewRevokeMembershipLogic(context.Background(), svcCtx).RevokeMembership(revokeReq(4201, "r-expired"))
	mustNoError(t, err, "收回已过期身份")
	if got.GetMembership().GetExpireAt() != expired.ExpireAt {
		t.Errorf("已过期身份的 expire_at 被改成 %d，期望保持 %d（不得推进一个已过期的时间戳）",
			got.GetMembership().GetExpireAt(), expired.ExpireAt)
	}
	g := db.grantByRequest("r-expired")
	if g == nil || g.BeforeExpireAt != g.AfterExpireAt {
		t.Errorf("应留下 before==after 的空操作台账，得到 %+v", g)
	}
}

func TestRevokeParameterRules(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*rpc.RevokeMembershipReq)
		want   error
	}{
		{"无理由", func(in *rpc.RevokeMembershipReq) { in.Reason = "" }, model.ErrReasonRequired},
		{"理由超长", func(in *rpc.RevokeMembershipReq) { in.Reason = strings.Repeat("由", model.MaxReasonLength+1) }, model.ErrFieldTooLong},
		{"两个选择器同时给", func(in *rpc.RevokeMembershipReq) { in.DeltaDays = 5 }, model.ErrInvalidGrantDelta},
		{"都不给", func(in *rpc.RevokeMembershipReq) { in.ClearRemaining = false }, model.ErrInvalidGrantDelta},
		{"负 delta", func(in *rpc.RevokeMembershipReq) { in.ClearRemaining = false; in.DeltaDays = -5 }, model.ErrInvalidGrantDelta},
		{"delta 超上限", func(in *rpc.RevokeMembershipReq) {
			in.ClearRemaining = false
			in.DeltaDays = 3661
		}, model.ErrInvalidGrantDelta},
		{"订单号超长", func(in *rpc.RevokeMembershipReq) { in.BizOrderNo = strings.Repeat("o", model.MaxBizNoLength+1) }, model.ErrFieldTooLong},
		{"缺幂等键", func(in *rpc.RevokeMembershipReq) { in.RequestId = "" }, model.ErrRequestIdRequired},
		{"档位非法", func(in *rpc.RevokeMembershipReq) { in.VipType = rpc.VipType_VIP_TYPE_UNSPECIFIED }, model.ErrInvalidVipType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			now := nowSec()
			seedMembership(db, &model.Membership{Mid: 4301, VipType: model.VipTypePremium, ExpireAt: now + 10*day})
			in := revokeReq(4301, "r-rule")
			tc.mutate(in)
			_, err := NewRevokeMembershipLogic(context.Background(), svcCtx).RevokeMembership(in)
			mustErrIs(t, err, tc.want, "收回参数")
			if len(db.grants) != 0 {
				t.Errorf("非法请求留下 %d 条台账", len(db.grants))
			}
		})
	}

	t.Run("从未开通的身份必须报错而不是空操作", func(t *testing.T) {
		svcCtx, db := newTestSvc(t)
		_, err := NewRevokeMembershipLogic(context.Background(), svcCtx).RevokeMembership(revokeReq(4302, "r-none"))
		mustErrIs(t, err, model.ErrMembershipNotFound, "收回不存在的身份")
		if len(db.grants) != 0 {
			t.Error("收回不存在的身份却写了台账")
		}
	})
}

// TestRevokeReadFailurePropagates 幂等预读/身份读失败必须上抛，
// 不能塌成「没有这行 → ErrMembershipNotFound」或「没有台账 → 直接再扣一次」。
func TestRevokeReadFailurePropagates(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	seedMembership(db, &model.Membership{Mid: 4401, VipType: model.VipTypePremium, ExpireAt: now + 10*day})
	db.grantErr = errLedgerDown
	if _, err := NewRevokeMembershipLogic(context.Background(), svcCtx).RevokeMembership(revokeReq(4401, "r-ledgerfail")); !errors.Is(err, errLedgerDown) {
		t.Fatalf("台账读失败实得 %v", err)
	}
	if db.txRuns != 0 {
		t.Error("预读失败仍起了事务（可能二次扣减）")
	}

	svcCtx2, db2 := newTestSvc(t)
	seedMembership(db2, &model.Membership{Mid: 4402, VipType: model.VipTypePremium, ExpireAt: now + 10*day})
	db2.memberErr = errIdentityDown
	if _, err := NewRevokeMembershipLogic(context.Background(), svcCtx2).RevokeMembership(revokeReq(4402, "r-memberfail")); !errors.Is(err, errIdentityDown) {
		t.Fatalf("身份读失败实得 %v", err)
	}
	if len(db2.grants) != 0 {
		t.Error("身份读失败仍写了台账")
	}
}

// TestRevokeReplayAndConflict 重放不再扣一次；换审计字段就是换口径。
func TestRevokeReplayAndConflict(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	held := seedMembership(db, &model.Membership{
		Mid: 4501, VipType: model.VipTypePremium, StartAt: now - 40*day, ExpireAt: now + 5*day,
	})
	first := seedGrant(db, &model.Grant{
		Mid: 4501, VipType: model.VipTypePremium, Action: model.ActionRevoke, DeltaDays: -5,
		Source: model.GrantSourceAdminOps, BizOrderNo: bizOrder,
		BeforeExpireAt: held.ExpireAt, AfterExpireAt: now,
		Operator: "ops-1", RequestID: "r-replay", Reason: revokeTrail, Ctime: now,
	})

	// 首次结果被上游回滚到 now；重放同参数必须原样还回首次结果，不再扣第二次。
	identical := revokeReq(4501, "r-replay")
	identical.ClearRemaining = false
	identical.DeltaDays = 5
	identical.BizOrderNo = bizOrder
	got, err := NewRevokeMembershipLogic(context.Background(), svcCtx).RevokeMembership(identical)
	mustNoError(t, err, "收回重放")
	if !got.GetDuplicated() || got.GetGrantId() != first.GrantID {
		t.Errorf("重放结果不对：duplicated=%v grant_id=%d", got.GetDuplicated(), got.GetGrantId())
	}
	if len(db.grants) != 1 || db.txRuns != 0 {
		t.Errorf("重放又扣了一次：台账 %d、事务 %d", len(db.grants), db.txRuns)
	}

	// 同 request_id 换审计字段（订单号）：本轮新增的追溯位必须参与冲突判定。
	changed := revokeReq(4501, "r-replay")
	changed.ClearRemaining = false
	changed.DeltaDays = 5
	changed.BizOrderNo = "SO-OTHER"
	if _, err := NewRevokeMembershipLogic(context.Background(), svcCtx).RevokeMembership(changed); !errors.Is(err, model.ErrRequestIdReused) {
		t.Fatalf("换订单号实得 %v，期望 ErrRequestIdReused", err)
	}
	// 同 request_id 换套餐追溯位同理。
	changedPlan := revokeReq(4501, "r-replay")
	changedPlan.ClearRemaining = false
	changedPlan.DeltaDays = 5
	changedPlan.BizOrderNo = bizOrder
	changedPlan.PlanId = 12
	if _, err := NewRevokeMembershipLogic(context.Background(), svcCtx).RevokeMembership(changedPlan); !errors.Is(err, model.ErrRequestIdReused) {
		t.Fatalf("换 plan_id 实得 %v，期望 ErrRequestIdReused", err)
	}
	if db.members[memberKey(4501, model.VipTypePremium)].ExpireAt != held.ExpireAt {
		t.Error("被拒的收回改变了到期时间")
	}
}

// TestRevokeNegativePlanIdNormalized plan_id 负数按「未指定」落账，
// 否则对账脚本要自己猜 -7 是什么意思，重放也比不中。
func TestRevokeNegativePlanIdNormalized(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	seedMembership(db, &model.Membership{Mid: 4601, VipType: model.VipTypePremium, ExpireAt: now + 9*day})

	in := revokeReq(4601, "r-negplan")
	in.PlanId = -7
	if _, err := NewRevokeMembershipLogic(context.Background(), svcCtx).RevokeMembership(in); err != nil {
		t.Fatalf("%v", err)
	}
	g := db.grantByRequest("r-negplan")
	if g == nil || g.PlanID != 0 {
		t.Fatalf("负 plan_id 未归一化：%+v", g)
	}

	// 重放时不带 plan_id（归一化后同为 0）必须仍能识别为同一次请求。
	retry := revokeReq(4601, "r-negplan")
	got, err := NewRevokeMembershipLogic(context.Background(), svcCtx).RevokeMembership(retry)
	mustNoError(t, err, "归一化后的重放")
	if !got.GetDuplicated() {
		t.Error("同一次请求的两次写法被判成了不同参数")
	}
}

// TestRevokeCasMissRollsBackLedger 身份行 CAS 未命中：台账一并回滚。
func TestRevokeCasMissRollsBackLedger(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	seedMembership(db, &model.Membership{Mid: 4701, VipType: model.VipTypePremium, ExpireAt: now + 9*day, Version: 6})
	db.membershipConcurrentCommit = true

	_, err := NewRevokeMembershipLogic(context.Background(), svcCtx).RevokeMembership(revokeReq(4701, "r-cas"))
	mustErrIs(t, err, model.ErrConcurrentUpdate, "收回 CAS 未命中")
	if len(db.grants) != 0 {
		t.Errorf("收回 CAS 冲突仍留下 %d 条台账", len(db.grants))
	}
	if got := db.members[memberKey(4701, model.VipTypePremium)]; got.ExpireAt != now+9*day {
		t.Errorf("CAS 冲突却改了到期时间：%d", got.ExpireAt)
	}
}

// --- ExpireMembership ---

// TestExpireNotYetDueSkipsWithoutConsumingRequestId 未到期：skipped=true、不记台账，
// 也不占用 request_id（到期后同一次任务还能重试成功）。
func TestExpireNotYetDueSkipsWithoutConsumingRequestId(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	held := seedMembership(db, &model.Membership{
		Mid: 5001, VipType: model.VipTypePremium, StartAt: now - day, ExpireAt: now + 3*day, Version: 2,
	})

	got, err := NewExpireMembershipLogic(context.Background(), svcCtx).ExpireMembership(expireReq(5001, "e-1"))
	mustNoError(t, err, "ExpireMembership")
	if !got.GetSkipped() {
		t.Error("未到期应 skipped")
	}
	if got.GetGrantId() != 0 || len(db.grants) != 0 {
		t.Errorf("未到期却记了台账：grant_id=%d 台账数=%d", got.GetGrantId(), len(db.grants))
	}
	if m := db.members[memberKey(5001, model.VipTypePremium)]; m.ExpireAt != held.ExpireAt || m.Version != held.Version {
		t.Error("未到期却动了身份行")
	}
	if got.GetMembership().GetExpireAt() != held.ExpireAt {
		t.Error("skipped 也要如实回显当前身份行，供调用方核对")
	}
}

// TestExpireWritesLedgerOnlyAndNeverTouchesIdentity 是「expire_at 即状态」设计的直接体现：
// 到期只补一条 EXPIRE 台账，mb_membership 一个字段都不改（含 version/mtime/auto_renew）。
func TestExpireWritesLedgerOnlyAndNeverTouchesIdentity(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	before := seedMembership(db, &model.Membership{
		Mid: 5101, VipType: model.VipTypePremium, StartAt: now - 90*day, ExpireAt: now - 2*day,
		AutoRenew: 1, AutoRenewChannel: model.AllowedAutoRenewChannel, AutoRenewSignedAt: now - 60*day,
		Source: model.GrantSourceSandboxPurchase, PaidMonthCount: 3, Version: 7,
		Ctime: now - 90*day, Mtime: now - 90*day,
	})
	snapshotRow := *before

	got, err := NewExpireMembershipLogic(context.Background(), svcCtx).ExpireMembership(expireReq(5101, "e-2"))
	mustNoError(t, err, "ExpireMembership")
	if got.GetSkipped() {
		t.Error("已到期却被 skip")
	}
	after := db.members[memberKey(5101, model.VipTypePremium)]
	if *after != snapshotRow {
		t.Errorf("ExpireMembership 改了身份行：%+v，期望保持 %+v（到期时间必须留在原值）", *after, snapshotRow)
	}

	g := db.grantByRequest("e-2")
	if g == nil {
		t.Fatal("到期没有台账，判定侧无从回溯权益终止")
	}
	if g.Action != model.ActionExpire || g.DeltaDays != 0 || g.Source != expireLedgerSource {
		t.Errorf("到期台账口径不符：%+v", g)
	}
	if g.BeforeExpireAt != before.ExpireAt || g.AfterExpireAt != before.ExpireAt {
		t.Errorf("到期台账前后值应相等且等于原 expire_at：%d -> %d vs %d", g.BeforeExpireAt, g.AfterExpireAt, before.ExpireAt)
	}
	if g.Operator != "cron" || !strings.Contains(g.Reason, strconv.FormatInt(before.ExpireAt, 10)) {
		t.Errorf("reason 留空时必须回落成含到期时间的摘要：%q", g.Reason)
	}
	if got.GetGrantId() != g.GrantID || got.GetMembership().GetExpireAt() != before.ExpireAt {
		t.Errorf("reply 未回显台账与身份：%+v", got)
	}
}

func TestExpireReasonPassthroughAndFallback(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	seedMembership(db, &model.Membership{Mid: 5201, VipType: model.VipTypePremium, ExpireAt: now - day})

	in := expireReq(5201, "e-3")
	in.Reason = "任务 expire-sweep-2026-09-21 判定到期"
	if _, err := NewExpireMembershipLogic(context.Background(), svcCtx).ExpireMembership(in); err != nil {
		t.Fatalf("%v", err)
	}
	g := db.grantByRequest("e-3")
	if g == nil || g.Reason != in.Reason {
		t.Errorf("cron 填了理由却被替换：%q", g.Reason)
	}
}

// TestExpireDedupesAcrossRequestIds cron 换 request_id 再扫一次同一到期事件时，
// FindAction(mid,vip,EXPIRE,expire_at) 必须挡住第二条台账，并把首次 grant_id 还回去。
func TestExpireDedupesAcrossRequestIds(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	expired := seedMembership(db, &model.Membership{
		Mid: 5301, VipType: model.VipTypePremium, StartAt: now - 60*day, ExpireAt: now - 6*day,
	})
	first := seedGrant(db, &model.Grant{
		Mid: 5301, VipType: model.VipTypePremium, Action: model.ActionExpire,
		BeforeExpireAt: expired.ExpireAt, AfterExpireAt: expired.ExpireAt,
		Operator: "cron", RequestID: "e-old", Reason: "上次任务", Ctime: now - 5*day,
	})

	got, err := NewExpireMembershipLogic(context.Background(), svcCtx).ExpireMembership(expireReq(5301, "e-new"))
	mustNoError(t, err, "ExpireMembership")
	if !got.GetSkipped() {
		t.Error("同一到期事件被重复受理")
	}
	if got.GetGrantId() != first.GrantID {
		t.Errorf("去重后未回查首次 grant_id：%d != %d", got.GetGrantId(), first.GrantID)
	}
	if len(db.grants) != 1 {
		t.Errorf("同一到期事件记了 %d 条台账，期望 1", len(db.grants))
	}
}

func TestExpireReplayAndConflict(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	expired := seedMembership(db, &model.Membership{
		Mid: 5401, VipType: model.VipTypePremium, StartAt: now - 60*day, ExpireAt: now - day,
	})
	first := seedGrant(db, &model.Grant{
		Mid: 5401, VipType: model.VipTypePremium, Action: model.ActionExpire,
		BeforeExpireAt: expired.ExpireAt, AfterExpireAt: expired.ExpireAt,
		Operator: "cron", RequestID: "e-replay", Reason: "任务摘要", Ctime: now,
	})

	got, err := NewExpireMembershipLogic(context.Background(), svcCtx).ExpireMembership(expireReq(5401, "e-replay"))
	mustNoError(t, err, "到期重放")
	if !got.GetSkipped() || got.GetGrantId() != first.GrantID {
		t.Errorf("重放未回首次结果：%+v", got)
	}
	if len(db.grants) != 1 || db.txRuns != 0 {
		t.Errorf("重放又处理了一遍：台账 %d、事务 %d", len(db.grants), db.txRuns)
	}

	// 同一 request_id 换档位：EXPIRE 台账的 plan_id/引用位固定为空，只有身份能变，必须冲突。
	other := expireReq(5401, "e-replay")
	other.VipType = rpc.VipType_VIP_TYPE_PREMIUM_PLUS
	if _, err := NewExpireMembershipLogic(context.Background(), svcCtx).ExpireMembership(other); !errors.Is(err, model.ErrRequestIdReused) {
		t.Fatalf("换档位实得 %v，期望 ErrRequestIdReused", err)
	}
	// 台账里那一行是别的动作（GRANT），复用同一个 request_id 做 expire 必须冲突。
	svcCtx2, db2 := newTestSvc(t)
	now2 := nowSec()
	seedMembership(db2, &model.Membership{Mid: 5402, VipType: model.VipTypePremium, ExpireAt: now2 - day})
	seedGrant(db2, &model.Grant{
		Mid: 5402, VipType: model.VipTypePremium, Action: model.ActionGrant, DeltaDays: 31,
		Source: model.GrantSourceAdminOps, Operator: "ops-1", RequestID: "e-mixed", Ctime: now2,
	})
	if _, err := NewExpireMembershipLogic(context.Background(), svcCtx2).ExpireMembership(expireReq(5402, "e-mixed")); !errors.Is(err, model.ErrRequestIdReused) {
		t.Fatalf("跨动作复用 request_id 实得 %v，期望 ErrRequestIdReused", err)
	}
}

func TestExpireIdentityRules(t *testing.T) {
	t.Run("身份不存在报错而不是 skip", func(t *testing.T) {
		svcCtx, db := newTestSvc(t)
		_, err := NewExpireMembershipLogic(context.Background(), svcCtx).ExpireMembership(expireReq(5501, "e-none"))
		mustErrIs(t, err, model.ErrMembershipNotFound, "到期不存在的身份")
		if len(db.grants) != 0 {
			t.Error("身份不存在却记了到期台账")
		}
	})
	t.Run("台账读失败上抛", func(t *testing.T) {
		svcCtx, db := newTestSvc(t)
		db.grantErr = errLedgerDown
		if _, err := NewExpireMembershipLogic(context.Background(), svcCtx).ExpireMembership(expireReq(5502, "e-read")); !errors.Is(err, errLedgerDown) {
			t.Fatalf("实得 %v", err)
		}
		if db.txRuns != 0 {
			t.Error("预读失败仍起了事务")
		}
	})
	t.Run("身份读失败上抛", func(t *testing.T) {
		svcCtx, db := newTestSvc(t)
		db.memberErr = errIdentityDown
		if _, err := NewExpireMembershipLogic(context.Background(), svcCtx).ExpireMembership(expireReq(5503, "e-read2")); !errors.Is(err, errIdentityDown) {
			t.Fatalf("实得 %v", err)
		}
		if len(db.grants) != 0 {
			t.Error("读不动身份却写了到期台账")
		}
	})
	t.Run("重放时身份被删要报错", func(t *testing.T) {
		svcCtx, db := newTestSvc(t)
		now := nowSec()
		seedGrant(db, &model.Grant{
			Mid: 5504, VipType: model.VipTypePremium, Action: model.ActionExpire,
			Operator: "cron", RequestID: "e-orphan", Ctime: now,
		})
		if _, err := NewExpireMembershipLogic(context.Background(), svcCtx).ExpireMembership(expireReq(5504, "e-orphan")); !errors.Is(err, model.ErrMembershipNotFound) {
			t.Fatalf("孤儿台账实得 %v，期望 ErrMembershipNotFound", err)
		}
	})
	t.Run("参数校验", func(t *testing.T) {
		svcCtx, _ := newTestSvc(t)
		logic := NewExpireMembershipLogic(context.Background(), svcCtx)
		if _, err := logic.ExpireMembership(&rpc.ExpireMembershipReq{Mid: 1, VipType: rpc.VipType_VIP_TYPE_PREMIUM, Operator: "cron"}); !errors.Is(err, model.ErrRequestIdRequired) {
			t.Errorf("缺幂等键实得 %v", err)
		}
		bad := expireReq(1, "e-len")
		bad.Reason = strings.Repeat("期", model.MaxReasonLength+1)
		if _, err := logic.ExpireMembership(bad); !errors.Is(err, model.ErrFieldTooLong) {
			t.Errorf("超长 reason 实得 %v", err)
		}
		noOp := expireReq(1, "e-op")
		noOp.Operator = ""
		if _, err := logic.ExpireMembership(noOp); !errors.Is(err, model.ErrOperatorRequired) {
			t.Errorf("缺操作者实得 %v", err)
		}
	})
}

// --- 三条写路径共有的口径 ---

// TestWritePathsNeverInventMembership 是本文件的核心命题：
// 判定读的是身份行，而身份行只能由这三条路径按台账写入。
// 这里用「读完再写」的顺序把三条路径各跑一遍，确认到期时间始终单调地由台账解释。
func TestWritePathsNeverInventMembership(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	mid := int64(6001)

	// 1. 从未开通：判定为 NO_MEMBERSHIP，且库里什么都没有。
	if len(db.members) != 0 || len(db.grants) != 0 {
		t.Fatal("夹具不应预置数据")
	}

	// 2. 付费开通 31 天。
	if _, err := NewGrantMembershipLogic(context.Background(), svcCtx).GrantMembership(paidGrantReq(mid, 31, "l-1")); err != nil {
		t.Fatalf("开通失败：%v", err)
	}
	// 3. 收回其中 10 天。
	deduct := revokeReq(mid, "l-2")
	deduct.ClearRemaining = false
	deduct.DeltaDays = 10
	if _, err := NewRevokeMembershipLogic(context.Background(), svcCtx).RevokeMembership(deduct); err != nil {
		t.Fatalf("收回失败：%v", err)
	}
	// 4. 未到期前 cron 只能 skip。
	skipped, err := NewExpireMembershipLogic(context.Background(), svcCtx).ExpireMembership(expireReq(mid, "l-3"))
	mustNoError(t, err, "到期扫描")
	if !skipped.GetSkipped() || len(db.grants) != 2 {
		t.Fatalf("未到期却处理了：%+v 台账 %d 条", skipped, len(db.grants))
	}

	// 每条台账都必须能解释一次到期时间变化：按自增 id 排序后应还原出真实写入顺序，
	// 且 before/after 首尾相接，最后一条的 after 就是身份行现在的到期时间。
	var ledger []*model.Grant
	seen := map[string]bool{}
	for _, g := range db.grants {
		if seen[g.RequestID] {
			t.Errorf("request_id %s 出现两条台账", g.RequestID)
		}
		seen[g.RequestID] = true
		ledger = append(ledger, g)
	}
	if len(ledger) != 2 {
		t.Fatalf("台账 %d 条，期望 2 条（开通 + 收回）", len(ledger))
	}
	sort.Slice(ledger, func(i, j int) bool { return ledger[i].GrantID < ledger[j].GrantID })
	if ledger[0].RequestID != "l-1" || ledger[1].RequestID != "l-2" {
		t.Errorf("自增 id 顺序与写入顺序不符：%s -> %s", ledger[0].RequestID, ledger[1].RequestID)
	}
	if ledger[0].BeforeExpireAt != 0 {
		t.Errorf("首开台账的 before_expire_at = %d，期望 0（此前没有身份行）", ledger[0].BeforeExpireAt)
	}
	if ledger[1].BeforeExpireAt != ledger[0].AfterExpireAt {
		t.Errorf("台账链断了：第 2 条 before=%d，第 1 条 after=%d",
			ledger[1].BeforeExpireAt, ledger[0].AfterExpireAt)
	}
	m := db.members[memberKey(mid, model.VipTypePremium)]
	if got := ledger[1].AfterExpireAt; got != m.ExpireAt {
		t.Errorf("最后一次台账的 after_expire_at (%d) 与身份行 (%d) 不一致：判定与台账已经分家",
			got, m.ExpireAt)
	}
	if m.ExpireAt <= nowSec() {
		t.Errorf("10 天收回后不应已过期：%d", m.ExpireAt)
	}
}
