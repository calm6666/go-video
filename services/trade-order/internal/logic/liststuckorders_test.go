package logic

// ListStuckOrders 的用例级测试（cron / 运营巡检的卡单扫描）。
//
// 这个方法的契约要点（liststuckorderslogic.go 的方法注释、README §3/§7）：
//   - 它是**纯扫描**：扫出来的单由调用方自己投递给 FulfillOrder/BindPayment/ApproveRefund，
//     本方法一步都不许写（扫描与修复分离）。沙箱语义下「顺手补偿一下」等于伪造已补偿，
//     所以每个用例都用触库序列 + 写基线双重钉住；
//   - 「卡住」= 状态 ∈ 扫描集合 **且** updated_at 严格早于 cutoff（model/to_order.go:591-592
//     的 `AND updated_at < ?`），阈值那一秒本身不算卡住；
//   - cutoff 只由 older_than_seconds / OrderExpireSeconds / 硬兜底 1800 这条链决定，
//     older_than<=0 绝不能退化成「全表扫」（liststuckorderslogic.go:66-71）；
//   - 状态守卫在 limit 守卫之前、更在触库之前；越上限是**拒绝**而不是裁剪
//     （裁剪会让 cron 以为「就这些单」，把剩下的漏掉）；
//   - 结果按 updated_at 升序（最老的先修），空结果回非 nil 切片。
//
// 时钟不可注入（model/now.go:14 的 NowUnix 没有替身缝），所以阈值边界用
// 「先记下 model 真收到的 cutoff，再按它现算期望集合」的写法：
// 这样等式两侧都有落点行，`<` 写成 `<=` 一定红，而跨秒抖动不会造成假红。

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

// stuckOrderNos 把应答里的订单号按返回顺序抽出来（顺序本身就是要断言的内容之一）。
func stuckOrderNos(reply *rpc.ListStuckOrdersReply) []string {
	out := make([]string, 0, len(reply.GetOrders()))
	for _, o := range reply.GetOrders() {
		out = append(out, o.GetOrderNo())
	}
	return out
}

// TestListStuckOrdersDefaultStateSetIsExactlyThreeInFlightStates 钉住默认扫描集合：
// states 为空时只扫 PAYING/PAID/FULFILLING（liststuckorderslogic.go:32），
// 连 stuckAllowedStates 里显式允许的 CREATED/FAILED/REFUND_REQUESTED/REFUND_APPROVED
// 都不含（允许 ≠ 默认：默认三档是「超时必然是故障」，FAILED 要人工、REFUND_APPROVED 是差异单）。
// 全部行都远超时间阈值，因此结果差异只能来自状态判定。
func TestListStuckOrdersDefaultStateSetIsExactlyThreeInFlightStates(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	anchor := fakeNow() - 100000 // 每张单都远超阈值：只有状态决定归属
	all := []int32{model.StateCreated, model.StatePaying, model.StatePaid, model.StateFulfilling,
		model.StateFulfilled, model.StateCancelled, model.StateFailed, model.StateRefundRequested,
		model.StateRefundApproved, model.StateRefunded, model.StateRefundRejected}
	for i, s := range all {
		seedOrder(t, db, &model.Order{
			OrderNo: fmt.Sprintf("to_s%02d", s), State: s, Mid: 1001 + int64(i),
			UpdatedAt: anchor + int64(i), CreatedAt: anchor + int64(i),
		})
	}
	mark := db.markWrites()

	got, err := NewListStuckOrdersLogic(context.Background(), svcCtx).
		ListStuckOrders(&rpc.ListStuckOrdersReq{OlderThanSeconds: 3600, Limit: 50})
	if err != nil {
		t.Fatalf("默认扫描失败: %v", err)
	}
	// 传给 model 的状态集合就是契约里的三档，逐位相等（不多不少、不排序不去重成别的形状）。
	if !reflect.DeepEqual(db.lastStuckStates,
		[]int32{model.StatePaying, model.StatePaid, model.StateFulfilling}) {
		t.Errorf("默认 states = %v，期望 [2 3 4]", db.lastStuckStates)
	}
	// 应答：只有那三张，按 updated_at 升序（anchor+1 < anchor+2 < anchor+3）。
	if nos := stuckOrderNos(got); !reflect.DeepEqual(nos, []string{"to_s02", "to_s03", "to_s04"}) {
		t.Errorf("默认扫描结果 = %v，期望 [to_s02 to_s03 to_s04]（终态与需人工态都不该出现）", nos)
	}
	assertCalls(t, db, "ListStuck")
	assertNoWritesAfter(t, db, mark)
}

// TestListStuckOrdersRejectsUndefinedStateBeforeTouchingDB 覆盖「数字根本不是一个状态」：
// UNSPECIFIED(0)、越界(12/-1/99) 都要回 ErrStuckStateNotAllowed 并点名是「不是已定义状态」。
// 拒绝而不是静默剔除（静默裁剪会让 cron 作者以为自己扫的是全集）。
func TestListStuckOrdersRejectsUndefinedStateBeforeTouchingDB(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	l := NewListStuckOrdersLogic(context.Background(), svcCtx)

	for _, s := range []rpc.OrderState{
		rpc.OrderState_ORDER_STATE_UNSPECIFIED, rpc.OrderState(12), rpc.OrderState(-1), rpc.OrderState(99),
	} {
		got, err := l.ListStuckOrders(&rpc.ListStuckOrdersReq{States: []rpc.OrderState{s}})
		if !errors.Is(err, model.ErrStuckStateNotAllowed) {
			t.Errorf("state=%d 应回 ErrStuckStateNotAllowed，得到 %v", int32(s), err)
		}
		if err != nil && !strings.Contains(err.Error(), "不是已定义状态") {
			t.Errorf("state=%d 的拒因文本要走「未定义」分支，得到 %q", int32(s), err.Error())
		}
		if got != nil {
			t.Errorf("state=%d 被拒时仍回应答：%s", int32(s), got.String())
		}
	}
	// 关键：四次非法状态一次库都没碰，也没写任何东西。
	assertCalls(t, db)
	assertReadOnly(t, db)
}

// TestListStuckOrdersRejectsClosedStateWithItsOwnReason 覆盖「是状态但已结案」分支：
// FULFILLED/CANCELLED/REFUNDED/REFUND_REJECTED 都有出边为 0 的终态语义，
// 错误文本必须换成「已结案，无自愈动作」（用 StateName 翻成人话，见 liststuckorderslogic.go:82），
// 与「不是已定义状态」是两条不同判因 —— 合并成一条就会让人分不清自己写错了枚举还是扫了终态。
func TestListStuckOrdersRejectsClosedStateWithItsOwnReason(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	l := NewListStuckOrdersLogic(context.Background(), svcCtx)

	for _, c := range []struct {
		state rpc.OrderState
		name  string
	}{
		{rpc.OrderState_ORDER_STATE_FULFILLED, "FULFILLED"},
		{rpc.OrderState_ORDER_STATE_CANCELLED, "CANCELLED"},
		{rpc.OrderState_ORDER_STATE_REFUNDED, "REFUNDED"},
		{rpc.OrderState_ORDER_STATE_REFUND_REJECTED, "REFUND_REJECTED"},
	} {
		got, err := l.ListStuckOrders(&rpc.ListStuckOrdersReq{States: []rpc.OrderState{c.state}})
		if !errors.Is(err, model.ErrStuckStateNotAllowed) {
			t.Errorf("state=%s 应回 ErrStuckStateNotAllowed，得到 %v", c.name, err)
		}
		if err != nil {
			want := fmt.Sprintf("state=%s 已结案，无自愈动作", c.name)
			if !strings.Contains(err.Error(), want) {
				t.Errorf("错误文本应含 %q，得到 %q", want, err.Error())
			}
			if strings.Contains(err.Error(), "不是已定义状态") {
				t.Errorf("终态被误判成「未定义状态」，运维会去查枚举而不是查调用方: %q", err.Error())
			}
		}
		if got != nil {
			t.Errorf("state=%s 被拒时仍回应答：%s", c.name, got.String())
		}
	}
	assertCalls(t, db)
}

// TestListStuckOrdersChecksStatesBeforeLimitAndBeforeDB 钉守卫次序：
// 非法状态 + 越界 limit 同时出现时必须先报状态（状态是「扫什么」，limit 是「扫多少」），
// 且两者都发生在触库之前 —— 只看返回值看不出「先查再判」，只能靠触库序列。
func TestListStuckOrdersChecksStatesBeforeLimitAndBeforeDB(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	svcCtx.Config.TradeOrder.StuckScanMaxLimit = 5
	l := NewListStuckOrdersLogic(context.Background(), svcCtx)

	got, err := l.ListStuckOrders(&rpc.ListStuckOrdersReq{
		States:           []rpc.OrderState{rpc.OrderState_ORDER_STATE_CANCELLED},
		Limit:            999999,
		OlderThanSeconds: -1,
	})
	if !errors.Is(err, model.ErrStuckStateNotAllowed) {
		t.Errorf("期望 ErrStuckStateNotAllowed（状态优先），得到 %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "limit=") {
		t.Errorf("状态守卫被 limit 守卫抢了，说明次序颠倒: %q", err.Error())
	}
	if got != nil {
		t.Errorf("被拒时仍回应答：%s", got.String())
	}
	assertCalls(t, db)
}

// TestListStuckOrdersAcceptsEveryRecoverableStateAndForwardsThemVerbatim
// 钉住 stuckAllowedStates（liststuckorderslogic.go:39-47）的全部七档都能扫，
// 且显式传入的 states 原样下发：不排序、不去重、不补默认值 ——
// cron 若写了 [FULFILLING, PAYING, FULFILLING]，SQL 里就是这三元，重复由 DB 的 IN 自然吸收。
func TestListStuckOrdersAcceptsEveryRecoverableStateAndForwardsThemVerbatim(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	l := NewListStuckOrdersLogic(context.Background(), svcCtx)

	// 七档逐一放行（每档先单独扫一次，确认没有哪档被误当终态拒掉）。
	allowed := []int32{model.StateCreated, model.StatePaying, model.StatePaid, model.StateFulfilling,
		model.StateFailed, model.StateRefundRequested, model.StateRefundApproved}
	for i, s := range allowed {
		if _, err := l.ListStuckOrders(&rpc.ListStuckOrdersReq{
			States: []rpc.OrderState{rpc.OrderState(s)}, Limit: 10,
		}); err != nil {
			t.Errorf("state=%d(%s) 应可扫描，得到 %v", s, model.StateName(s), err)
		}
		if !reflect.DeepEqual(db.lastStuckStates, []int32{s}) {
			t.Errorf("第 %d 次下发 states = %v，期望 [%d]", i+1, db.lastStuckStates, s)
		}
	}
	// 乱序 + 重复原样下发。
	if _, err := l.ListStuckOrders(&rpc.ListStuckOrdersReq{
		States: []rpc.OrderState{rpc.OrderState(model.StateFulfilling),
			rpc.OrderState(model.StatePaying), rpc.OrderState(model.StateFulfilling)},
		Limit: 10,
	}); err != nil {
		t.Fatalf("乱序重复 states 应放行: %v", err)
	}
	if !reflect.DeepEqual(db.lastStuckStates,
		[]int32{model.StateFulfilling, model.StatePaying, model.StateFulfilling}) {
		t.Errorf("states 被改写了（排序/去重/补默认）：%v", db.lastStuckStates)
	}
	assertCalls(t, db, "ListStuck", "ListStuck", "ListStuck", "ListStuck", "ListStuck", "ListStuck",
		"ListStuck", "ListStuck")
	assertReadOnly(t, db)
}

// TestListStuckOrdersCutoffComesFromTheFallbackChain 钉住 cutoff = NowUnix() - olderThan 的阈值链：
// 显值 > 配置 OrderExpireSeconds > 硬兜底 1800，任何一档都不许退化成 0（=全表扫）。
// model.NowUnix 不可注入，所以断言取「model 真收到的 cutoff 落在 [before-X, after-X]」这个夹逼区间：
// 边界是闭的、不依赖跨秒时机，但把 olderThan 用错（例如拿配置原值直接当 cutoff、或忘了取负）都会红。
func TestListStuckOrdersCutoffComesFromTheFallbackChain(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	l := NewListStuckOrdersLogic(context.Background(), svcCtx)

	cases := []struct {
		name          string
		expireConfig  int64
		olderThan     int64
		wantThreshold int64 // 期望的 olderThan 生效值
	}{
		{"显式值优先于配置", 1800, 600, 600},
		{"零值退化为配置", 1800, 0, 1800},
		{"负值退化为配置", 1800, -30, 1800},
		{"配置改小就跟着变小", 900, 0, 900},
		{"配置被清空时兜底 1800", 0, 0, 1800},
		{"配置非法负值时也兜底 1800", -1, 0, 1800},
		{"显式值压过兜底", 0, 45, 45},
	}
	for i, c := range cases {
		svcCtx.Config.TradeOrder.OrderExpireSeconds = c.expireConfig
		before := model.NowUnix()
		got, err := l.ListStuckOrders(&rpc.ListStuckOrdersReq{OlderThanSeconds: c.olderThan, Limit: 10})
		after := model.NowUnix()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got == nil {
			t.Fatalf("%s: 返回了 nil 应答", c.name)
		}
		cutoff := db.lastStuckBefore
		if cutoff < before-c.wantThreshold || cutoff > after-c.wantThreshold {
			t.Errorf("%s: cutoff=%d 不在 [%d, %d]（olderThan 应为 %d）",
				c.name, cutoff, before-c.wantThreshold, after-c.wantThreshold, c.wantThreshold)
		}
		if countCalls(db, "ListStuck") != i+1 {
			t.Fatalf("%s: 第 %d 个用例后触库 %d 次", c.name, i+1, countCalls(db, "ListStuck"))
		}
	}
	assertReadOnly(t, db)
}

// TestListStuckOrdersStuckPredicateIsUpdatedAtStrictlyOlderThanCutoff 钉「卡住」判定的两条腿：
//  1. 时间列是 updated_at，不是 created_at：ancient-created + 刚更新的单不算卡住，
//     recent-created + 早就没动的单算卡住（一次建单后停在 PAYING 的单是主要目标）；
//  2. 阈值那一秒的归属：SQL 写的是 `updated_at < ?`（model/to_order.go:591-592），
//     所以 updated_at == cutoff 不卡住。本用例把行铺在 cutoff 的 -1/0/+1/+2 秒上，
//     再用 model 真收到的那个 cutoff 现算期望集合，四种跨秒情形里必然有一行正好压在 cutoff 上，
//     把 `<` 改成 `<=` 一定红。
func TestListStuckOrdersStuckPredicateIsUpdatedAtStrictlyOlderThanCutoff(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	const olderThan = 3600
	before := model.NowUnix()
	anchor := before - olderThan

	// 按 updated_at 升序铺行（期望集合直接沿用这个顺序）：
	// 四行压在 cutoff 的 -1/0/+1/+2 秒上，另三行分别测「时间列选错」与「状态判错」。
	seed := []struct {
		no        string
		updatedAt int64
		createdAt int64
	}{
		{"to_old_cancelled", anchor - 3, anchor - 3}, // 终态：再老也不扫
		{"to_stale_update", anchor - 2, before},      // 刚建但早已不动：必须扫到（判据非 created_at）
		{"to_minus1", anchor - 1, anchor - 1},        // 阈值前一秒：卡住
		{"to_zero", anchor, anchor},                  // 正好压在 cutoff 那一侧，见下
		{"to_plus1", anchor + 1, anchor + 1},
		{"to_plus2", anchor + 2, anchor + 2},
		{"to_fresh_touch", before - 1, anchor - 100000}, // 一万小时前建、刚刚才更新：不该扫到
	}
	for _, s := range seed {
		state := int32(model.StatePaying)
		if s.no == "to_old_cancelled" {
			state = model.StateCancelled
		}
		seedOrder(t, db, &model.Order{OrderNo: s.no, State: state, Mid: 1001,
			UpdatedAt: s.updatedAt, CreatedAt: s.createdAt})
	}
	mark := db.markWrites()

	got, err := NewListStuckOrdersLogic(context.Background(), svcCtx).
		ListStuckOrders(&rpc.ListStuckOrdersReq{OlderThanSeconds: olderThan, Limit: 50})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	cutoff := db.lastStuckBefore
	// 夹逼整个用例的时间跨度：跨到 3 秒以上说明机器卡到不可信，直接判失败而不是放过。
	if cutoff < anchor || cutoff > anchor+3 {
		t.Fatalf("cutoff=%d 落在 [%d, %d] 之外，边界判定不可信", cutoff, anchor, anchor+3)
	}

	want := make([]string, 0, len(seed))
	for _, s := range seed {
		// 期望集合同样按生产 SQL 的两条腿现算：状态 ∈ 集合 且 updated_at 严格 < cutoff。
		if s.no != "to_old_cancelled" && s.updatedAt < cutoff {
			want = append(want, s.no)
		}
	}
	// seed 里 updatedAt 已按升序排布（anchor-3 … anchor+2、before-1 在最末），
	// 所以 want 本身就是「最老的先修」的期望顺序。
	if nos := stuckOrderNos(got); !reflect.DeepEqual(nos, want) {
		t.Errorf("卡单集合 = %v，期望 %v（cutoff=%d）", nos, want, cutoff)
	}
	// 单挑最危险的一条：任何一行 updated_at >= cutoff 都不许出现 —— 「等于阈值」这一侧不算卡住。
	for _, o := range got.GetOrders() {
		if o.GetUpdatedAt() >= cutoff {
			t.Errorf("%s 的 updated_at=%d 不早于 cutoff=%d，阈值边界被判成 >= 了",
				o.GetOrderNo(), o.GetUpdatedAt(), cutoff)
		}
	}
	for _, o := range got.GetOrders() {
		if o.GetState() == rpc.OrderState_ORDER_STATE_CANCELLED {
			t.Errorf("终态 %s 被扫出来了", o.GetOrderNo())
		}
	}
	assertCalls(t, db, "ListStuck")
	assertNoWritesAfter(t, db, mark)
}

// TestListStuckOrdersLimitIsRejectedNotTrimmed 钉住上限语义：
// 越上限回 ErrInvalidPage 并带上 limit/max 两个数字（cron 作者要能在日志里看见自己写了多少），
// 且发生在触库之前；limit<=0 用上限而不是报错；上限只来自 StuckScanMaxLimit，
// 配置缺项（<=0）时兜底 200 —— 兜底必须是「有界」而不是「无界」。
func TestListStuckOrdersLimitIsRejectedNotTrimmed(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	l := NewListStuckOrdersLogic(context.Background(), svcCtx)

	svcCtx.Config.TradeOrder.StuckScanMaxLimit = 5
	// 越上限：拒绝，且一次库都不碰。
	for _, limit := range []int64{6, 999999} {
		got, err := l.ListStuckOrders(&rpc.ListStuckOrdersReq{Limit: limit})
		if !errors.Is(err, model.ErrInvalidPage) {
			t.Errorf("limit=%d 应回 ErrInvalidPage，得到 %v", limit, err)
		}
		if err != nil {
			if want := fmt.Sprintf("limit=%d, max=5", limit); !strings.Contains(err.Error(), want) {
				t.Errorf("错误文本应含 %q，得到 %q", want, err.Error())
			}
		}
		if got != nil {
			t.Errorf("limit=%d 被拒时仍回应答：%s", limit, got.String())
		}
	}
	assertCalls(t, db)

	// 正好等于上限放行，且 limit 原样下发（不被偷偷改小）。
	if _, err := l.ListStuckOrders(&rpc.ListStuckOrdersReq{Limit: 5}); err != nil {
		t.Fatalf("limit=max 应放行: %v", err)
	}
	if db.lastStuckLimit != 5 {
		t.Errorf("limit = %d，期望 5", db.lastStuckLimit)
	}
	// limit<=0 走上限：0、-1 都一样（注意 -1 不是报错而是「用全集」，见下方行为说明）。
	for _, limit := range []int64{0, -1, -100} {
		if _, err := l.ListStuckOrders(&rpc.ListStuckOrdersReq{Limit: limit}); err != nil {
			t.Errorf("limit=%d 应退化为上限而不是报错: %v", limit, err)
		}
		if db.lastStuckLimit != 5 {
			t.Errorf("limit=%d 下发成 %d，期望用上限 5", limit, db.lastStuckLimit)
		}
	}

	// 配置上限只收紧不放宽：StuckScanMaxLimit=2 时 limit=5 必须被拒。
	svcCtx.Config.TradeOrder.StuckScanMaxLimit = 2
	if _, err := l.ListStuckOrders(&rpc.ListStuckOrdersReq{Limit: 5}); !errors.Is(err, model.ErrInvalidPage) {
		t.Errorf("配置上限 2 时 limit=5 应被拒，得到 %v", err)
	}
	// 配置缺项时兜底 200（无界扫描会锁表）。
	svcCtx.Config.TradeOrder.StuckScanMaxLimit = 0
	if _, err := l.ListStuckOrders(&rpc.ListStuckOrdersReq{Limit: 201}); !errors.Is(err, model.ErrInvalidPage) {
		t.Errorf("StuckScanMaxLimit=0 时 limit=201 应被兜底上限 200 拒掉，得到 %v", err)
	}
	if _, err := l.ListStuckOrders(&rpc.ListStuckOrdersReq{}); err != nil {
		t.Fatalf("缺省 limit 应退化为兜底上限: %v", err)
	}
	if db.lastStuckLimit != 200 {
		t.Errorf("缺省 limit 下发成 %d，期望兜底 200", db.lastStuckLimit)
	}
	assertCalls(t, db, "ListStuck", "ListStuck", "ListStuck", "ListStuck", "ListStuck")
	assertReadOnly(t, db)
}

// TestListStuckOrdersKeepsTheStoresOldestFirstOrder 钉住投影不重排：
// 排序由 model 的 `ORDER BY updated_at ASC`（model/to_order.go:591-592）给出，
// logic 只做投影。这里刻意把 created_at 排成 updated_at 的反序 ——
// 若 logic 自己按 created_at 重排（或反排），期望顺序立刻翻转。
func TestListStuckOrdersKeepsTheStoresOldestFirstOrder(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	anchor := fakeNow() - 5000
	rows := []struct {
		no        string
		updatedAt int64
	}{
		{"to_u_oldest", anchor},
		{"to_u_middle", anchor + 60},
		{"to_u_newest", anchor + 120},
	}
	for i, r := range rows {
		seedOrder(t, db, &model.Order{OrderNo: r.no, State: model.StateFulfilling, Mid: 1001,
			UpdatedAt: r.updatedAt, CreatedAt: anchor + int64(len(rows)-i)*1000})
	}

	got, err := NewListStuckOrdersLogic(context.Background(), svcCtx).
		ListStuckOrders(&rpc.ListStuckOrdersReq{OlderThanSeconds: 600, Limit: 10})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	want := []string{"to_u_oldest", "to_u_middle", "to_u_newest"}
	if nos := stuckOrderNos(got); !reflect.DeepEqual(nos, want) {
		t.Errorf("返回顺序 = %v，期望按 updated_at 升序 %v", nos, want)
	}
	assertCalls(t, db, "ListStuck")
}

// TestListStuckOrdersProjectsEveryColumnAndAnswersNonNilEmpty 逐列核对投影：
// 卡单列表页要拿 expire_at/fulfill_attempts/version 决定投递哪个修复动作，
// 漏映射不会报错、只会静默给 0 值，所以只能用显式字面量整体比对。
// 同时钉住「没有卡单」回的是空切片而不是 nil（nil 会让上层 JSON 渲染成 null）。
func TestListStuckOrdersProjectsEveryColumnAndAnswersNonNilEmpty(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	anchor := now - 4000
	seedOrder(t, db, &model.Order{
		ID: 88, OrderNo: "to_stuck", RequestID: "req-stuck", Mid: 2002, BizType: model.BizMembership,
		PlanID: 9, PlanCode: "vip_m", Title: "大会员月卡", Quantity: 3, DurationDays: 31,
		CoinAmount: 0, UnitPriceMinor: 3000, AmountMinor: 9000, RefundedMinor: 0,
		Currency: "CNY", PayMethod: model.PayBalance, State: model.StatePaid,
		FulfillState: model.FulfillPending, FulfillAttempts: 2, FulfillDetail: "coin rpc timeout",
		PaymentNo: "pay_9", GrantRef: "", ExpireAt: anchor + 300, Platform: model.PlatformIOS,
		ClientTraceID: "trace-9", Version: 4, CreatedAt: anchor - 10, UpdatedAt: anchor,
		PaidAt: anchor - 5, FulfilledAt: 0, ClosedAt: 0,
	})
	mark := db.markWrites()

	got, err := NewListStuckOrdersLogic(context.Background(), svcCtx).
		ListStuckOrders(&rpc.ListStuckOrdersReq{OlderThanSeconds: 600, Limit: 10})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(got.GetOrders()) != 1 {
		t.Fatalf("命中数 = %d，期望 1: %s", len(got.GetOrders()), got.String())
	}
	want := &rpc.OrderInfo{
		OrderNo: "to_stuck", Mid: 2002, BizType: rpc.OrderBizType_ORDER_BIZ_TYPE_MEMBERSHIP, PlanId: 9,
		PlanCode: "vip_m", Title: "大会员月卡", Quantity: 3, DurationDays: 31, CoinAmount: 0,
		UnitPriceMinor: 3000, AmountMinor: 9000, RefundedMinor: 0, Currency: "CNY",
		PayMethod: rpc.PayMethod_PAY_METHOD_BALANCE, State: rpc.OrderState_ORDER_STATE_PAID,
		FulfillState: rpc.FulfillState_FULFILL_STATE_PENDING, FulfillAttempts: 2,
		FulfillDetail: "coin rpc timeout", PaymentNo: "pay_9", GrantRef: "", ExpireAt: anchor + 300,
		Platform: rpc.Platform_PLATFORM_IOS, ClientTraceId: "trace-9", RequestId: "req-stuck",
		Version: 4, CreatedAt: anchor - 10, UpdatedAt: anchor, PaidAt: anchor - 5,
	}
	if got.GetOrders()[0].String() != want.String() {
		t.Errorf("投影与订单行不一致：\n got=%s\nwant=%s", got.GetOrders()[0].String(), want.String())
	}

	// 没有卡单：非 nil 空切片（换一份干净的库，避免与上一行的「确实卡住」混淆阈值判定）。
	emptyCtx, emptyDB := newTestSvc(t)
	empty, err := NewListStuckOrdersLogic(context.Background(), emptyCtx).
		ListStuckOrders(&rpc.ListStuckOrdersReq{OlderThanSeconds: 600, Limit: 10})
	if err != nil {
		t.Fatalf("空扫描失败: %v", err)
	}
	if empty.Orders == nil {
		t.Error("无卡单时 orders 是 nil，期望非 nil 空切片")
	}
	if len(empty.GetOrders()) != 0 {
		t.Errorf("空库扫描命中 %d 张，期望 0: %s", len(empty.GetOrders()), empty.String())
	}
	assertCalls(t, db, "ListStuck")
	assertNoWritesAfter(t, db, mark)
	assertCalls(t, emptyDB, "ListStuck")
	assertReadOnly(t, emptyDB)
}

// TestListStuckOrdersScansWithoutAnyCompensation 是本方法最要紧的一条：
// 扫描**不许顺手推进状态**（README §5：状态机只由写入口按幂等键推进；
// AGENTS.md §9：不许伪造成功）。沙箱下「扫到 PAYING 就顺手关掉/顺手补发」会被运营
// 读成「已经补偿过了」，真实差异就此被抹平。
// 这里除了看写基线，还逐列比对每一行扫描前后的状态字段，并核对台账没多行。
func TestListStuckOrdersScansWithoutAnyCompensation(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	anchor := fakeNow() - 4000
	nos := []string{"to_a", "to_b", "to_c"}
	states := []int32{model.StatePaying, model.StatePaid, model.StateFulfilling}
	for i, no := range nos {
		seedOrder(t, db, &model.Order{OrderNo: no, State: states[i], Mid: 1001 + int64(i),
			AmountMinor: 6000, RefundedMinor: 0, FulfillState: model.FulfillPending,
			FulfillAttempts: int32(i), Version: int64(2 + i), ExpireAt: anchor + 300,
			PaymentNo: "pay_" + no, CreatedAt: anchor - 100, UpdatedAt: anchor + int64(i)})
		seedLedger(t, db, &model.OrderEvent{OrderNo: no, FromState: model.StateCreated,
			ToState: states[i], Operator: "user", Reason: "created", Ctime: anchor - 100})
	}
	before := make([]*model.Order, len(nos))
	for i, no := range nos {
		before[i] = mustOrder(t, db, no)
	}
	mark := db.markWrites()
	eventsBefore := len(db.events)

	for round := 0; round < 3; round++ { // 反复扫描（cron 每轮都扫）也必须幂等
		got, err := NewListStuckOrdersLogic(context.Background(), svcCtx).
			ListStuckOrders(&rpc.ListStuckOrdersReq{OlderThanSeconds: 600, Limit: 10})
		if err != nil {
			t.Fatalf("第 %d 轮扫描失败: %v", round+1, err)
		}
		if len(got.GetOrders()) != len(nos) {
			t.Fatalf("第 %d 轮命中 %d 张，期望 %d 张", round+1, len(got.GetOrders()), len(nos))
		}
	}

	// 每行逐列不动：状态、履约态、尝试数、版本、金额、时间戳全部保持扫描前的值。
	for i, no := range nos {
		after := mustOrder(t, db, no)
		b := before[i]
		if after.State != b.State || after.FulfillState != b.FulfillState ||
			after.FulfillAttempts != b.FulfillAttempts || after.Version != b.Version ||
			after.AmountMinor != b.AmountMinor || after.RefundedMinor != b.RefundedMinor ||
			after.UpdatedAt != b.UpdatedAt || after.ExpireAt != b.ExpireAt || after.ClosedAt != b.ClosedAt {
			t.Errorf("%s 被扫描改写过：扫描前 %+v，扫描后 %+v", no, *b, *after)
		}
	}
	if len(db.events) != eventsBefore {
		t.Errorf("扫描写了 %d 行台账", len(db.events)-eventsBefore)
	}
	// 只读了一张扫描视图：没有逐行回读、没有 CAS、没有事务。
	assertCalls(t, db, "ListStuck", "ListStuck", "ListStuck")
	assertNoWritesAfter(t, db, mark)
}

// TestListStuckOrdersPropagatesStoreError 保证库存故障回原样错误：
// 扫不出单与扫不了单是两件事，后者如果被降级成空列表，cron 会以为「本轮没有卡单」而停止巡检。
func TestListStuckOrdersPropagatesStoreError(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	storeErr := errors.New("trade-order: read timeout on idx_state_updated")
	db.stuckErr = storeErr
	before := model.NowUnix()

	got, err := NewListStuckOrdersLogic(context.Background(), svcCtx).
		ListStuckOrders(&rpc.ListStuckOrdersReq{OlderThanSeconds: 600, Limit: 10,
			States: []rpc.OrderState{rpc.OrderState_ORDER_STATE_PAID}})
	after := model.NowUnix()
	if !errors.Is(err, storeErr) {
		t.Errorf("依赖错误必须原样上抛，得到 %v", err)
	}
	if got != nil {
		t.Errorf("报错时不该带回应答（空列表会被读成「本轮无卡单」）：%s", got.String())
	}
	// 参数已经换算完才撞库：states/older_than/limit 三条腿都要如实传下去，
	// 否则运维按错误日志复现扫描时会扫出另一批单。
	if !reflect.DeepEqual(db.lastStuckStates, []int32{model.StatePaid}) || db.lastStuckLimit != 10 {
		t.Errorf("实参未正确下发：states=%v limit=%d", db.lastStuckStates, db.lastStuckLimit)
	}
	if db.lastStuckBefore < before-600 || db.lastStuckBefore > after-600 {
		t.Errorf("cutoff=%d 不在 [%d, %d]（older_than=600 没如实换算）",
			db.lastStuckBefore, before-600, after-600)
	}
	assertCalls(t, db, "ListStuck")
	assertReadOnly(t, db)
}

// TestListStuckOrdersForwardsContextToStore 钉住 ctx 一路传到 model：
// 取消/超时信号丢了，一次卡住的扫描会一直占着连接（cron 每轮叠加就是雪崩）。
func TestListStuckOrdersForwardsContextToStore(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	rec := &ctxRecordingOrderModel{fakeOrderModel: fakeOrderModel{db: db}}
	svcCtx.Orders = rec
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "cron-run-1")

	if _, err := NewListStuckOrdersLogic(ctx, svcCtx).
		ListStuckOrders(&rpc.ListStuckOrdersReq{OlderThanSeconds: 600, Limit: 10}); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if got, _ := rec.gotCtx.Value(ctxKey{}).(string); got != "cron-run-1" {
		t.Errorf("model 收到的 ctx 丢了调用方上下文，得到 %q", got)
	}
}

// ListStuck 上包一层，记录 logic 实际传给 model 的 ctx。
func (m *ctxRecordingOrderModel) ListStuck(ctx context.Context, states []int32,
	updatedBefore, limit int64,
) ([]*model.Order, error) {
	m.gotCtx = ctx
	return m.fakeOrderModel.ListStuck(ctx, states, updatedBefore, limit)
}
