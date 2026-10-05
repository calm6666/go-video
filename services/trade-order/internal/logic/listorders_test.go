package logic

// ListOrders 的用例级测试（运营面订单检索，有界窗口）。
//
// 本方法的规则是「拒绝」而不是「裁剪」（listorderslogic.go 注释、README §7）：
// 无界扫描在 to_order 上就是锁风险，所以跨用户（mid<=0）时必须给出
// 点位查询（order_no/payment_no 走索引）或完整时间窗，且窗口不超过
// MaxListWindowSeconds；调用方的 max_window_seconds 只能收紧、不能放宽。
//
// 权限位不在本服务：ListOrdersReq 里没有任何身份位，调用方鉴权由
// gateway/admin 侧完成（只读路由，见 gateway/admin/internal/logic/orderlistlogic.go）。
// 因此本文件能钉的是「过滤条件不能被绕过/不能被子调用方借道夹带」，
// 以及「守卫全部发生在触库之前」—— 一旦某个守卫写成先查后判，锁风险就已经发生了。

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

func TestListOrdersPaginationGuardRunsFirst(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	l := NewListOrdersLogic(context.Background(), svcCtx)

	// 什么都不过滤的跨用户查询本应回 ErrFilterRequired，但 size 越界更靠前：
	// 两个守卫的先后是固定口径，不能被调用方的其它入参影响。
	got, err := l.ListOrders(&rpc.ListOrdersReq{Size: 101})
	if !errors.Is(err, model.ErrInvalidPage) {
		t.Errorf("size=101 应回 ErrInvalidPage，得到 %v", err)
	}
	if got != nil {
		t.Errorf("被拒时仍回应答：%s", got.String())
	}
	if _, err := l.ListOrders(&rpc.ListOrdersReq{Mid: 1001, Page: -2}); !errors.Is(err, model.ErrInvalidPage) {
		t.Errorf("负页码应回 ErrInvalidPage，得到 %v", err)
	}
	assertCalls(t, db)
}

func TestListOrdersValidatesEveryEnumFilterBeforeTouchingDB(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedOrder(t, db, &model.Order{OrderNo: "to_e", Mid: 1001, State: model.StatePaid})
	l := NewListOrdersLogic(context.Background(), svcCtx)

	cases := []struct {
		name string
		req  *rpc.ListOrdersReq
		want error
		text string
	}{
		{"未定义状态", &rpc.ListOrdersReq{Mid: 1001, State: rpc.OrderState(12)},
			model.ErrInvalidStateTransition, "state=12"},
		{"第三种业务类型", &rpc.ListOrdersReq{Mid: 1001, BizType: rpc.OrderBizType(7)},
			model.ErrInvalidBizType, "biz_type=7"},
		{"第三种支付方式（真资金渠道没接入）", &rpc.ListOrdersReq{Mid: 1001, PayMethod: rpc.PayMethod(3)},
			model.ErrInvalidPayMethod, "pay_method=3"},
		{"支付方式负值", &rpc.ListOrdersReq{Mid: 1001, PayMethod: rpc.PayMethod(-1)},
			model.ErrInvalidPayMethod, "pay_method=-1"},
		// 三个枚举都非法时按 state → biz_type → pay_method 依次判，口径固定。
		{"三者同时非法（state 优先）", &rpc.ListOrdersReq{Mid: 1001, State: rpc.OrderState(12),
			BizType: rpc.OrderBizType(7), PayMethod: rpc.PayMethod(3)}, model.ErrInvalidStateTransition, "state=12"},
		{"biz_type 优先于 pay_method", &rpc.ListOrdersReq{Mid: 1001, BizType: rpc.OrderBizType(7),
			PayMethod: rpc.PayMethod(3)}, model.ErrInvalidBizType, "biz_type=7"},
	}
	for _, c := range cases {
		got, err := l.ListOrders(c.req)
		if !errors.Is(err, c.want) {
			t.Errorf("%s：得到 %v，期望 %v", c.name, err, c.want)
		}
		if err != nil && !strings.Contains(err.Error(), c.text) {
			t.Errorf("%s：错误里没有回显实际取值 %s: %v", c.name, c.text, err)
		}
		if got != nil {
			t.Errorf("%s：被拒时仍回应答 %s", c.name, got.String())
		}
	}
	// 合法枚举（BALANCE=1 / SANDBOX=2 / MEMBERSHIP=1 / COIN_PACK=2）必须放行。
	if _, err := l.ListOrders(&rpc.ListOrdersReq{Mid: 1001, PayMethod: rpc.PayMethod_PAY_METHOD_BALANCE,
		BizType: rpc.OrderBizType_ORDER_BIZ_TYPE_MEMBERSHIP, State: rpc.OrderState_ORDER_STATE_CANCELLED}); err != nil {
		t.Errorf("合法枚举组合被拒: %v", err)
	}
	assertCalls(t, db, "ListByFilter")
}

func TestListOrdersRejectsInvertedWindowForAnyCaller(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	l := NewListOrdersLogic(context.Background(), svcCtx)

	// mid>0 也一样要拒：倒着给的时间窗说明调用方算错了边界，
	// 放过去会得到一个「永远为空」的运营页而不是报错。
	cases := []*rpc.ListOrdersReq{
		{Mid: 1001, FromTs: 2000, ToTs: 1000},
		{FromTs: 2000, ToTs: 1000, State: rpc.OrderState_ORDER_STATE_PAID},
		{FromTs: 2000, ToTs: 1999, OrderNo: "to_x"},
	}
	for _, req := range cases {
		got, err := l.ListOrders(req)
		if !errors.Is(err, model.ErrListWindowTooLarge) {
			t.Errorf("%+v 应回 ErrListWindowTooLarge，得到 %v", req, err)
		}
		if err != nil && !strings.Contains(err.Error(), "from_ts=2000 > to_ts=") {
			t.Errorf("错误里要写清是哪个边界: %v", err)
		}
		if got != nil {
			t.Errorf("被拒时仍回应答 %s", got.String())
		}
	}
	assertCalls(t, db)
}

// TestListOrdersCrossUserRequiresWindowOrPointQuery 钉住两条跨用户闸门的分工：
// 什么都没给 → ErrFilterRequired；只给低基数过滤（状态/类型/支付方式）→
// ErrListWindowRequired（状态是低基数列，光它命中不了多少选择性，同样是全表扫）；
// 点位查询（order_no/payment_no）可以不带窗口（走唯一/二级索引）。
// 另外钉住「空白字符串不算点位条件」：不 trim 就等于用 "   " 蒙过闸门。
func TestListOrdersCrossUserRequiresWindowOrPointQuery(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedOrder(t, db, &model.Order{OrderNo: "to_p", Mid: 1001, State: model.StatePaid})
	l := NewListOrdersLogic(context.Background(), svcCtx)

	cases := []struct {
		name string
		req  *rpc.ListOrdersReq
		want error
	}{
		{"完全无过滤", &rpc.ListOrdersReq{}, model.ErrFilterRequired},
		{"mid 为负也算跨用户", &rpc.ListOrdersReq{Mid: -5}, model.ErrFilterRequired},
		{"只有状态", &rpc.ListOrdersReq{State: rpc.OrderState_ORDER_STATE_PAID}, model.ErrListWindowRequired},
		{"只有业务类型", &rpc.ListOrdersReq{BizType: rpc.OrderBizType_ORDER_BIZ_TYPE_COIN_PACK},
			model.ErrListWindowRequired},
		{"只有支付方式", &rpc.ListOrdersReq{PayMethod: rpc.PayMethod_PAY_METHOD_SANDBOX_CHANNEL},
			model.ErrListWindowRequired},
		{"半开时间窗 + 过滤条件：缺完整窗口", &rpc.ListOrdersReq{FromTs: 1000,
			State: rpc.OrderState_ORDER_STATE_PAID}, model.ErrListWindowRequired},
		{"只有 to_ts（另一半也不给）+ 业务类型", &rpc.ListOrdersReq{ToTs: 1000,
			BizType: rpc.OrderBizType_ORDER_BIZ_TYPE_COIN_PACK}, model.ErrListWindowRequired},
		{"半开时间窗但零过滤位：先撞更外层的闸门", &rpc.ListOrdersReq{FromTs: 1000},
			model.ErrFilterRequired},
		{"只有空白 order_no（不能被当成点位条件）", &rpc.ListOrdersReq{OrderNo: "   "}, model.ErrFilterRequired},
		{"只有空白 payment_no", &rpc.ListOrdersReq{PaymentNo: "\t "}, model.ErrFilterRequired},
		{"时间窗倒挂优先于缺窗口的判定", &rpc.ListOrdersReq{FromTs: 900, ToTs: 100,
			State: rpc.OrderState_ORDER_STATE_PAID}, model.ErrListWindowTooLarge},
	}
	for _, c := range cases {
		got, err := l.ListOrders(c.req)
		if !errors.Is(err, c.want) {
			t.Errorf("%s：得到 %v，期望 %v", c.name, err, c.want)
		}
		if got != nil {
			t.Errorf("%s：被拒时仍回应答 %s", c.name, got.String())
		}
	}
	assertCalls(t, db)

	// 反过来：点位查询不需要时间窗（单用户也一样）。
	for _, req := range []*rpc.ListOrdersReq{
		{OrderNo: "to_p"},
		{PaymentNo: "pay_1"},
		{Mid: 1001, State: rpc.OrderState_ORDER_STATE_PAID}, // 单用户不需要窗口
	} {
		if _, err := l.ListOrders(req); err != nil {
			t.Errorf("%+v 应放行: %v", req, err)
		}
	}
	assertCalls(t, db, "ListByFilter", "ListByFilter", "ListByFilter")
}

// TestListOrdersWindowBoundary 钉窗口边界与「只能收紧」这条方向性约束：
// 跨度**等于**上限放行（判定是 toTs-fromTs > maxWindow），上限 +1 秒拒绝；
// max_window_seconds 小于服务上限时按更严的走，大于服务上限时被忽略（不能放宽）。
func TestListOrdersWindowBoundary(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	svcCtx.Config.TradeOrder.MaxListWindowSeconds = 3600
	l := NewListOrdersLogic(context.Background(), svcCtx)

	// 恰好等于上限：放行。
	if _, err := l.ListOrders(&rpc.ListOrdersReq{FromTs: 1000, ToTs: 4600}); err != nil {
		t.Errorf("窗口恰好等于上限 3600s 应放行，得到 %v", err)
	}
	// 上限 +1：拒绝，且错误里给出实际跨度与上限。
	_, err := l.ListOrders(&rpc.ListOrdersReq{FromTs: 1000, ToTs: 4601})
	if !errors.Is(err, model.ErrListWindowTooLarge) {
		t.Fatalf("窗口 3601s 应被拒，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "window=3601s") || !strings.Contains(err.Error(), "max=3600s") {
		t.Errorf("错误文本缺少数值上下文: %v", err)
	}
	// 调用方只能收紧：把上限降到 60s 后，61s 的窗口就该拒。
	if _, err := l.ListOrders(&rpc.ListOrdersReq{FromTs: 1000, ToTs: 1061, MaxWindowSeconds: 60}); !errors.Is(err,
		model.ErrListWindowTooLarge) {
		t.Errorf("max_window_seconds=60 时 61s 窗口应被拒，得到 %v", err)
	}
	if _, err := l.ListOrders(&rpc.ListOrdersReq{FromTs: 1000, ToTs: 1060, MaxWindowSeconds: 60}); err != nil {
		t.Errorf("max_window_seconds=60 时 60s 窗口应放行，得到 %v", err)
	}
	// 不能放宽：传一个比服务上限大的值，越界结论必须不变（错误里 max 仍是 3600）。
	_, err = l.ListOrders(&rpc.ListOrdersReq{FromTs: 1000, ToTs: 4700, MaxWindowSeconds: 999999})
	if !errors.Is(err, model.ErrListWindowTooLarge) {
		t.Fatalf("max_window_seconds 被当成了放宽上限的口子，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "max=3600s") {
		t.Errorf("窗口上限没有被服务侧钳制: %v", err)
	}
	// 单用户（mid>0）不受窗口约束：走 idx_mid_state_created，跨度再大也放行。
	if _, err := l.ListOrders(&rpc.ListOrdersReq{Mid: 1001, FromTs: 1, ToTs: 999999999}); err != nil {
		t.Errorf("限定单用户时不该做窗口上限判定: %v", err)
	}
	// 六次调用里只有「恰好等于上限」「收紧后仍等于上限」「单用户」三次放行。
	assertCalls(t, db, "ListByFilter", "ListByFilter", "ListByFilter")
}

// TestListOrdersMaxWindowSecondsZeroMeansNoTightening 钉住 max_window_seconds=0 的口径：
// 0 是「用服务默认窗口」（proto 注释），不是「不限窗口」。
func TestListOrdersMaxWindowSecondsZeroMeansNoTightening(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	svcCtx.Config.TradeOrder.MaxListWindowSeconds = 3600
	l := NewListOrdersLogic(context.Background(), svcCtx)

	if _, err := l.ListOrders(&rpc.ListOrdersReq{FromTs: 1000, ToTs: 999999, MaxWindowSeconds: 0}); !errors.Is(err,
		model.ErrListWindowTooLarge) {
		t.Errorf("max_window_seconds=0 必须落回服务默认上限而不是放开，得到 %v", err)
	}
	assertCalls(t, db)
}

// TestListOrdersSingleUserRejectsNothingOnMissingWindow 说明闸门只针对跨用户：
// mid>0 时不带任何过滤位也放行（就是「这个用户的全部订单」，索引前缀已收敛）。
func TestListOrdersSingleUserRejectsNothingOnMissingWindow(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedOrder(t, db, &model.Order{OrderNo: "to_s1", Mid: 1001, State: model.StatePaid, CreatedAt: 500})
	seedOrder(t, db, &model.Order{OrderNo: "to_s2", Mid: 2002, State: model.StatePaid, CreatedAt: 600})

	reply, err := NewListOrdersLogic(context.Background(), svcCtx).ListOrders(&rpc.ListOrdersReq{Mid: 1001})
	if err != nil {
		t.Fatalf("单用户查询失败: %v", err)
	}
	if reply.GetTotal() != 1 || len(reply.GetOrders()) != 1 {
		t.Fatalf("限定 mid 后仍读到 %d 条 / total=%d", len(reply.GetOrders()), reply.GetTotal())
	}
	if reply.GetOrders()[0].GetOrderNo() != "to_s1" {
		t.Errorf("读到了别的用户的单: %s", reply.GetOrders()[0].GetOrderNo())
	}
	if db.lastFilter.Mid != 1001 {
		t.Errorf("mid 没下传: %+v", db.lastFilter)
	}
	assertReadOnly(t, db)
}

// TestListOrdersForwardsTrimmedFiltersExactly 逐字段核对下传的 OrderFilter：
// 点位条件必须按 trim 后的值传下去，分页位换算同 ListMyOrders，
// 并且 States(IN) 这一位永远不由本方法填入（它会绕开窗口闸门的语义）。
func TestListOrdersForwardsTrimmedFiltersExactly(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedOrder(t, db, &model.Order{OrderNo: "to_t", Mid: 0, PaymentNo: "pay_t", State: model.StateCancelled})

	l := NewListOrdersLogic(context.Background(), svcCtx)
	if _, err := l.ListOrders(&rpc.ListOrdersReq{
		OrderNo: "  to_t\t", PaymentNo: " pay_t ", State: rpc.OrderState_ORDER_STATE_CANCELLED,
		BizType: rpc.OrderBizType_ORDER_BIZ_TYPE_COIN_PACK, PayMethod: rpc.PayMethod_PAY_METHOD_BALANCE,
		FromTs: 100, ToTs: 200, Page: 2, Size: 3,
	}); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	want := &model.OrderFilter{
		State: model.StateCancelled, BizType: model.BizCoinPack, PayMethod: model.PayBalance,
		OrderNo: "to_t", PaymentNo: "pay_t", FromTs: 100, ToTs: 200, Offset: 3, Limit: 3,
	}
	if !reflect.DeepEqual(db.lastFilter, want) {
		t.Errorf("下传的过滤条件不符：\n got=%+v\nwant=%+v", db.lastFilter, want)
	}
	if db.lastFilter.States != nil {
		t.Errorf("States(IN) 不该由本方法填入: %v", db.lastFilter.States)
	}
}

// TestListOrdersAnswersAcrossUsersByDesign 钉住运营面的本体语义：
// 跨用户查询按 created_at DESC, id DESC 返回多个用户的单，
// total 是过滤后的总数（不是页内条数），每条应答的字段来自自己那一行。
func TestListOrdersAnswersAcrossUsersByDesign(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	seedOrder(t, db, &model.Order{OrderNo: "to_x1", Mid: 1001, State: model.StatePaid, AmountMinor: 100,
		PayMethod: model.PayBalance, BizType: model.BizMembership, CreatedAt: now - 300, UpdatedAt: now - 300})
	seedOrder(t, db, &model.Order{OrderNo: "to_x2", Mid: 2002, State: model.StateFulfilled, AmountMinor: 200,
		PayMethod: model.PaySandbox, BizType: model.BizCoinPack, CreatedAt: now - 200, UpdatedAt: now - 200})
	seedOrder(t, db, &model.Order{OrderNo: "to_x3", Mid: 3003, State: model.StatePaid, AmountMinor: 300,
		PayMethod: model.PayBalance, BizType: model.BizMembership, CreatedAt: now - 100, UpdatedAt: now - 100})
	// 窗口外的行：不能因为「同一状态」被顺带捞出来。
	seedOrder(t, db, &model.Order{OrderNo: "to_out", Mid: 4004, State: model.StatePaid, AmountMinor: 400,
		PayMethod: model.PayBalance, BizType: model.BizMembership, CreatedAt: now - 99999, UpdatedAt: now - 99999})

	l := NewListOrdersLogic(context.Background(), svcCtx)
	reply, err := l.ListOrders(&rpc.ListOrdersReq{
		State: rpc.OrderState_ORDER_STATE_PAID, FromTs: now - 400, ToTs: now, Page: 1, Size: 10,
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if reply.GetTotal() != 2 || len(reply.GetOrders()) != 2 {
		t.Fatalf("状态+窗口过滤后应为 2 条 / total=2，得到 条数=%d total=%d",
			len(reply.GetOrders()), reply.GetTotal())
	}
	wantNo := []string{"to_x3", "to_x1"}
	wantMid := []int64{3003, 1001}
	wantAmount := []int64{300, 100}
	for i, o := range reply.GetOrders() {
		if o.GetOrderNo() != wantNo[i] || o.GetMid() != wantMid[i] || o.GetAmountMinor() != wantAmount[i] {
			t.Errorf("第 %d 条应答与行不符（顺序或字段串行）: %s", i, o.String())
		}
		if o.GetState() != rpc.OrderState_ORDER_STATE_PAID {
			t.Errorf("第 %d 条状态没过滤干净: %s", i, o.GetState())
		}
	}
	if reply.GetPage() != 1 || reply.GetSize() != 10 {
		t.Errorf("分页位回显不符: page=%d size=%d", reply.GetPage(), reply.GetSize())
	}

	// 空结果回非 nil 切片（运营页不必为 null 写分支），total 仍是 SQL 给的 0。
	empty, err := l.ListOrders(&rpc.ListOrdersReq{PaymentNo: "pay_absent"})
	if err != nil {
		t.Fatalf("查不到不是错误: %v", err)
	}
	if empty.GetOrders() == nil {
		t.Error("orders 为 nil 切片")
	}
	if len(empty.GetOrders()) != 0 || empty.GetTotal() != 0 {
		t.Errorf("应为空结果，得到 %s", empty.String())
	}
	assertReadOnly(t, db)
}

func TestListOrdersPropagatesStoreError(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	storeErr := errors.New("trade-order: to_order ListByFilter count: connection reset by peer")
	db.listErr = storeErr

	reply, err := NewListOrdersLogic(context.Background(), svcCtx).
		ListOrders(&rpc.ListOrdersReq{OrderNo: "to_x"})
	if !errors.Is(err, storeErr) {
		t.Errorf("依赖错误必须原样上抛，得到 %v", err)
	}
	if reply != nil {
		t.Errorf("报错时不该回应答（空列表会冒充「没有这笔订单」）：%s", reply.String())
	}
	assertCalls(t, db, "ListByFilter")
}

// TestListOrdersWindowOnlyCrossUserScanIsAccepted 钉住**当前行为**（不美化）：
// 缺陷：services/trade-order/internal/logic/listorderslogic.go:64 —— `ErrFilterRequired` 的判定写成
// `in.GetMid() <= 0 && !filtered && !hasWindow`（过滤位与窗口**两者都没有**才拒），
// 而 listorderslogic.go:34 的函数头与 README §7 都写着「跨用户（mid=0）必须同时满足：
// 至少一个过滤条件 + 完整时间窗」。文档是 AND，实现是 OR，于是
// 「只给一个 90 天窗口、零过滤条件」的跨用户查询被放行 ——
// 在 to_order 上这是一次 created_at 区间扫（回表全用户订单），
// 与本方法「拒绝而不是裁剪」的自述相反，是运营页最容易把库拖慢、也最容易把全量用户订单
// 一次性摊给一个只读后台会话的入口（后台只读组不挂权限点，见 gateway/admin routes.go:1903-1929）。
// 修法方向：跨用户且非点位查询时 `!filtered` 直接 ErrFilterRequired
// （窗口只解决「扫多少」，过滤条件才解决「扫得起来吗」），
// 或把 listorderslogic.go:34 与 README §7 改成实际口径（窗口即可）并按窗口长度另设更低的上限。
// 本用例按现状断言；改动生产代码时必须同步翻转这里的期望值。
func TestListOrdersWindowOnlyCrossUserScanIsAccepted(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	seedOrder(t, db, &model.Order{OrderNo: "to_w1", Mid: 1001, CreatedAt: now - 10, UpdatedAt: now - 10})
	seedOrder(t, db, &model.Order{OrderNo: "to_w2", Mid: 2002, CreatedAt: now - 20, UpdatedAt: now - 20})

	reply, err := NewListOrdersLogic(context.Background(), svcCtx).
		ListOrders(&rpc.ListOrdersReq{FromTs: now - 3600, ToTs: now})
	if err != nil {
		t.Fatalf("当前实现没有拒绝窗口型跨用户扫描，得到 %v", err)
	}
	if len(reply.GetOrders()) != 2 || reply.GetTotal() != 2 {
		t.Fatalf("钉住了，但结果不符（可能生产代码已修）：条数=%d total=%d",
			len(reply.GetOrders()), reply.GetTotal())
	}
	// 顺带钉住这条路径的下传形态：mid=0、零过滤位、只有窗口。
	want := &model.OrderFilter{FromTs: now - 3600, ToTs: now, Offset: 0, Limit: 20}
	if !reflect.DeepEqual(db.lastFilter, want) {
		t.Errorf("窗口型扫描的过滤条件不符：\n got=%+v\nwant=%+v", db.lastFilter, want)
	}
}

// TestListOrdersZeroMaxListWindowUnbindsCrossUserScan 钉住**当前行为**（不美化）：
// 缺陷：services/trade-order/internal/logic/listorderslogic.go:76 —— 窗口上限判定写成
// `if maxWindow > 0 && toTs-fromTs > maxWindow`，而 maxWindow 直接取自配置
// （listorderslogic.go:72），所以配置值 ≤ 0 时整个上限判定被跳过：跨用户查询可以带任意长的时间窗。
// （漏键本身安全 —— config.go:56 的 `default=7776000` 由 conf.Load 补上；出事的是**显式写 0/负数**
// 的 yaml，以及任何不走 conf.Load 构造 Config 的路径。）
// 更糟的是 listorderslogic.go:73 的
// 「调用方只能收紧」写作 `want > 0 && want < maxWindow`，maxWindow=0 时调用方自己传的
// max_window_seconds 也一并失效（0 不比 0 小），等于把唯一的界交给配置文件。
// README §8 只写了「默认 7776000」，此前没说非正值会退化成无界扫描。
// 同一包里 paginate 对 MaxPageSize<=0 兜底成 100（helpers.go:106-108）、
// ListStuckOrders 对 StuckScanMaxLimit<=0 兜底成 200（liststuckorderslogic.go:91-92），只有这里没兜底 —— 口径不一致。
// 修法方向：maxWindow<=0 时回落到常量默认（与 README §8 的 7776000 同源），
// 或直接拒绝并提示配置缺失；同时给 config 加载补一条必填校验。
// 本用例按现状断言，改动生产代码时必须同步翻转这里的期望值。
func TestListOrdersZeroMaxListWindowUnbindsCrossUserScan(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	svcCtx.Config.TradeOrder.MaxListWindowSeconds = 0
	now := fakeNow()

	// 跨度约 10 年：服务上限被配置缺项抹掉后照样放行。
	if _, err := NewListOrdersLogic(context.Background(), svcCtx).
		ListOrders(&rpc.ListOrdersReq{FromTs: now - 315360000, ToTs: now}); err != nil {
		t.Fatalf("当前实现不做上限判定，得到 %v", err)
	}
	if db.lastFilter.ToTs-db.lastFilter.FromTs != 315360000 {
		t.Errorf("窗口没有原样下传: %+v", db.lastFilter)
	}
	// 但倒挂仍然被拒：说明「配置缺项」只抹掉了上限，没有让守卫整体失效。
	if _, err := NewListOrdersLogic(context.Background(), svcCtx).
		ListOrders(&rpc.ListOrdersReq{FromTs: now, ToTs: now - 10}); !errors.Is(err, model.ErrListWindowTooLarge) {
		t.Errorf("倒挂窗口仍应被拒，得到 %v", err)
	}
	assertCalls(t, db, "ListByFilter")
}
