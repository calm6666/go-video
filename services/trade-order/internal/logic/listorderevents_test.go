package logic

// ListOrderEvents 的用例级测试（订单状态流转台账）。
//
// 台账是「谁在什么时候把订单从 A 推到 B、理由是什么」的正史（README §3），
// 因此契约口径是：
//   - 按 ctime ASC（同秒按自增主键）正序返回，读起来就是状态轨迹本身；
//   - order_no 必填，且守卫发生在分页换算与触库之前；
//   - 不校验归属：ListOrderEventsReq 里没有 mid 位，本方法是运营/排障入口，
//     终端面由 gateway/app 先用 GetOrder(order_no, mid) 预检归属
//     （gateway/app/internal/logic/ordereventslogic.go），本服务不重复判；
//   - request_id 是幂等键，只在服务内部用，不出现在应答里（OrderEventInfo 没这一位）。
//
// 排序本身由 model 的 `ORDER BY ctime ASC, event_id ASC` 决定
// （model/to_order_event.go 的 ListByOrderNo），fakes_test.go 的替身复刻同一口径；
// 本文件因此能钉的是「logic 不改变这个正序、不反转、不把游标页错位」。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

func TestListOrderEventsRequiresOrderNoBeforeAnythingElse(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedLedger(t, db, &model.OrderEvent{OrderNo: "to_x", ToState: model.StatePaid, Ctime: fakeNow() - 5})
	l := NewListOrderEventsLogic(context.Background(), svcCtx)

	// 连 size=999 这种一定报错的分页参数都不能把 ErrOrderNoRequired 挤掉：
	// 台账方法的第一道闸门是「查哪张单」。
	for _, orderNo := range []string{"", "  ", "\n\t"} {
		got, err := l.ListOrderEvents(&rpc.ListOrderEventsReq{OrderNo: orderNo, Page: 1, Size: 999})
		if !errors.Is(err, model.ErrOrderNoRequired) {
			t.Errorf("order_no=%q 应回 ErrOrderNoRequired，得到 %v", orderNo, err)
		}
		if got != nil {
			t.Errorf("order_no=%q 被拒时仍回应答：%s", orderNo, got.String())
		}
	}
	assertCalls(t, db)
}

func TestListOrderEventsPaginatesBeforeTouchingDB(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	l := NewListOrderEventsLogic(context.Background(), svcCtx)

	cases := []struct {
		name     string
		req      *rpc.ListOrderEventsReq
		wantOff  int64
		wantLim  int64
		wantPage int64
	}{
		{"默认第 1 页 20 条", &rpc.ListOrderEventsReq{OrderNo: "to_x"}, 0, 20, 1},
		{"第 3 页每页 10 条", &rpc.ListOrderEventsReq{OrderNo: "to_x", Page: 3, Size: 10}, 20, 10, 3},
		{"第 1 页 100 条（正好等于上限）", &rpc.ListOrderEventsReq{OrderNo: "to_x", Size: 100}, 0, 100, 1},
	}
	for _, c := range cases {
		got, err := l.ListOrderEvents(c.req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if db.lastEventsOff != c.wantOff || db.lastEventsLimit != c.wantLim {
			t.Errorf("%s：下传的分页实参 (offset=%d,limit=%d)，期望 (%d,%d)",
				c.name, db.lastEventsOff, db.lastEventsLimit, c.wantOff, c.wantLim)
		}
		if got.GetSize() != c.wantLim || got.GetPage() != c.wantPage {
			t.Errorf("%s：回显分页位 page=%d size=%d，期望 page=%d size=%d",
				c.name, got.GetPage(), got.GetSize(), c.wantPage, c.wantLim)
		}
	}
	// 越上限拒绝（裁剪会让运营页以为台账就这么多行），且不触库。
	for _, req := range []*rpc.ListOrderEventsReq{
		{OrderNo: "to_x", Size: 101},
		{OrderNo: "to_x", Page: -1, Size: 10},
		{OrderNo: "to_x", Size: -1},
	} {
		got, err := l.ListOrderEvents(req)
		if !errors.Is(err, model.ErrInvalidPage) {
			t.Errorf("%+v 应回 ErrInvalidPage，得到 %v", req, err)
		}
		if got != nil {
			t.Errorf("被拒时仍回应答 %s", got.String())
		}
	}
	assertCalls(t, db, "ListOrderEvents", "ListOrderEvents", "ListOrderEvents")
	assertReadOnly(t, db)
}

// TestListOrderEventsAnswersInChronologicalOrder 钉住正序：
// 台账按插入顺序（=写入顺序）落库，但用例故意倒着、乱着给 ctime，
// 期望应答按时间正序 —— 状态轨迹读成倒序，运营会把「驳回」看成「申请」。
func TestListOrderEventsAnswersInChronologicalOrder(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	seedLedger(t, db, &model.OrderEvent{OrderNo: "to_seq", FromState: model.StateCreated,
		ToState: model.StatePaying, Operator: "user", Reason: "created", Ctime: now - 10})
	seedLedger(t, db, &model.OrderEvent{OrderNo: "to_seq", FromState: model.StatePaying,
		ToState: model.StatePaid, Operator: "user", Reason: "paid", Ctime: now - 40})
	// 同秒的两行必须按自增主键判定（否则顺序不可复现）。
	seedLedger(t, db, &model.OrderEvent{OrderNo: "to_seq", FromState: model.StatePaid,
		ToState: model.StateFulfilling, Operator: "cron", Reason: "attempt-1", Ctime: now - 40})
	seedLedger(t, db, &model.OrderEvent{OrderNo: "to_seq", FromState: model.StateFulfilling,
		ToState: model.StateFulfilled, Operator: "system", Reason: "granted", Ctime: now - 70})
	mark := db.markWrites()

	reply, err := NewListOrderEventsLogic(context.Background(), svcCtx).
		ListOrderEvents(&rpc.ListOrderEventsReq{OrderNo: "to_seq"})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	wantReasons := []string{"granted", "paid", "attempt-1", "created"}
	// 自增主键顺序与插入顺序一致（4,2,3,1 是按 ctime 排完的结果），
	// 用它做第二排序键的见证：同秒的 paid(2) 必须在 attempt-1(3) 之前。
	wantIDs := []int64{4, 2, 3, 1}
	if len(reply.GetEvents()) != len(wantReasons) {
		t.Fatalf("条数 %d，期望 %d", len(reply.GetEvents()), len(wantReasons))
	}
	for i, e := range reply.GetEvents() {
		if e.GetReason() != wantReasons[i] {
			t.Errorf("第 %d 行应为 %s，得到 %s（ctime=%d）", i, wantReasons[i], e.GetReason(), e.GetCtime())
		}
		if e.GetEventId() != wantIDs[i] {
			t.Errorf("第 %d 行 event_id=%d，期望 %d（ctime 相同的行按主键正序）", i, e.GetEventId(), wantIDs[i])
		}
	}
	if reply.GetTotal() != 4 {
		t.Errorf("total=%d，应为台账全量行数 4（不是页内条数）", reply.GetTotal())
	}
	// 正序 + 翻页：第 2 页 2 条应是时间上较新的后两行。
	page2, err := NewListOrderEventsLogic(context.Background(), svcCtx).
		ListOrderEvents(&rpc.ListOrderEventsReq{OrderNo: "to_seq", Page: 2, Size: 2})
	if err != nil {
		t.Fatalf("翻页失败: %v", err)
	}
	if len(page2.GetEvents()) != 2 || page2.GetEvents()[0].GetReason() != "attempt-1" ||
		page2.GetEvents()[1].GetReason() != "created" {
		t.Fatalf("第 2 页内容与正序口径不符: %s", page2.String())
	}
	if page2.GetTotal() != 4 || page2.GetPage() != 2 || page2.GetSize() != 2 {
		t.Errorf("翻页回显不符: total=%d page=%d size=%d", page2.GetTotal(), page2.GetPage(), page2.GetSize())
	}
	// 读整段轨迹一个字节都不写：台账只允许被写入口追加（README §3「只 INSERT」）。
	assertNoWritesAfter(t, db, mark)
}

// TestListOrderEventsProjectsEveryLedgerColumn 逐字段核对投影：
// 台账对外只暴露 7 位（event_id/order_no/from_state/to_state/operator/reason/ctime），
// 幂等键 request_id 留在服务内部 —— 它参与唯一性判定，透出给运营页之外的人
// 等于把「同一笔动作的重复提交标识」交给调用方伪造。
func TestListOrderEventsProjectsEveryLedgerColumn(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	seedLedger(t, db, &model.OrderEvent{OrderNo: "to_col", FromState: model.StatePaid,
		ToState: model.StateRefundRequested, Operator: "operator:8001",
		Reason: "用户申请退款，沙箱全额", RequestID: "req-secret-8899", Ctime: now - 20})
	// from_state=0 是「建单」行的合法取值，必须原样投影而不是被翻译成 UNKNOWN。
	seedLedger(t, db, &model.OrderEvent{OrderNo: "to_col", FromState: 0,
		ToState: model.StateCreated, Operator: "user", Reason: "created", Ctime: now - 30})

	reply, err := NewListOrderEventsLogic(context.Background(), svcCtx).
		ListOrderEvents(&rpc.ListOrderEventsReq{OrderNo: "to_col"})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	want := []*rpc.OrderEventInfo{
		{EventId: 2, OrderNo: "to_col", FromState: rpc.OrderState_ORDER_STATE_UNSPECIFIED,
			ToState: rpc.OrderState_ORDER_STATE_CREATED, Operator: "user", Reason: "created", Ctime: now - 30},
		{EventId: 1, OrderNo: "to_col", FromState: rpc.OrderState_ORDER_STATE_PAID,
			ToState: rpc.OrderState_ORDER_STATE_REFUND_REQUESTED, Operator: "operator:8001",
			Reason: "用户申请退款，沙箱全额", Ctime: now - 20},
	}
	if len(reply.GetEvents()) != len(want) {
		t.Fatalf("条数 %d，期望 %d", len(reply.GetEvents()), len(want))
	}
	for i := range want {
		if reply.GetEvents()[i].String() != want[i].String() {
			t.Errorf("第 %d 行投影不符：\n got=%s\nwant=%s",
				i, reply.GetEvents()[i].String(), want[i].String())
		}
	}
	if strings.Contains(reply.String(), "req-secret-8899") {
		t.Error("应答泄漏了内部幂等键 request_id")
	}
}

// TestListOrderEventsIsScopedToTheRequestedOrder 钉住台账按单隔离：
// 查 A 单不能顺带出 B 单的行，total 也只数 A 的行。
func TestListOrderEventsIsScopedToTheRequestedOrder(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	seedLedger(t, db, &model.OrderEvent{OrderNo: "to_a", ToState: model.StatePaid, Reason: "a1", Ctime: now - 1})
	seedLedger(t, db, &model.OrderEvent{OrderNo: "to_b", ToState: model.StatePaid, Reason: "b1", Ctime: now - 2})
	seedLedger(t, db, &model.OrderEvent{OrderNo: "to_b", ToState: model.StateRefunded, Reason: "b2", Ctime: now - 3})

	reply, err := NewListOrderEventsLogic(context.Background(), svcCtx).
		ListOrderEvents(&rpc.ListOrderEventsReq{OrderNo: "to_a"})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if reply.GetTotal() != 1 || len(reply.GetEvents()) != 1 {
		t.Fatalf("total=%d 条数=%d，应只有 to_a 的一行", reply.GetTotal(), len(reply.GetEvents()))
	}
	if reply.GetEvents()[0].GetOrderNo() != "to_a" || reply.GetEvents()[0].GetReason() != "a1" {
		t.Errorf("读到了别的订单的台账: %s", reply.GetEvents()[0].String())
	}

	// 没有任何台账行的订单：空列表 + total=0，不报错（订单可能刚建好还没推过状态）。
	empty, err := NewListOrderEventsLogic(context.Background(), svcCtx).
		ListOrderEvents(&rpc.ListOrderEventsReq{OrderNo: "to_no_ledger"})
	if err != nil {
		t.Fatalf("空台账不该报错: %v", err)
	}
	if empty.GetEvents() == nil {
		t.Error("events 为 nil 切片")
	}
	if len(empty.GetEvents()) != 0 || empty.GetTotal() != 0 {
		t.Errorf("应为空结果，得到 %s", empty.String())
	}
}

// TestListOrderEventsDoesNotCheckOwnership 钉住「本方法不判归属」这一事实。
// 这不是缺陷也不是放行，而是契约缺口：ListOrderEventsReq 里没有 mid 位
// （services/trade-order/rpc/tradeorder.proto 的 ListOrderEventsReq，只有 order_no/page/size），
// 所以服务侧物理上无法判归属，见 listordereventslogic.go:32 的自述；
// 终端面的归属预检由 gateway/app 用 GetOrder(order_no, mid) 完成
// （gateway/app/internal/logic/ordereventslogic.go:55-60，非本人一律 not-found 后再决定读不读台账）。
// 需要盯住的两点（已记 README §10）：
//   - listordereventslogic.go:33 说「台账行里没有 PII（operator/reason 都是写入侧脱敏过的摘要）」，
//     但写入侧只做 truncate（helpers.go:398 `truncate(reason, maxReasonLen)`）与
//     折叠空白的 sanitize（helpers.go:75），没有任何脱敏；用户自己在 RequestRefund 的 reason
//     里写了什么，台账就原样存什么 —— 所以「不判归属」的前提比注释里写的弱；
//   - 一旦有人给 OrderEventInfo 或请求加了 mid 却忘了在这里判定，
//     或者反过来在本方法里加「静默裁剪成自己的单」（那会让运营面永远查不到别人的单），这里会先响。
func TestListOrderEventsDoesNotCheckOwnership(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedOrder(t, db, &model.Order{OrderNo: "to_foreign", Mid: 1001, State: model.StatePaid})
	seedLedger(t, db, &model.OrderEvent{OrderNo: "to_foreign", FromState: model.StateCreated,
		ToState: model.StatePaying, Reason: "created", Ctime: fakeNow() - 9})
	mark := db.markWrites()

	reply, err := NewListOrderEventsLogic(context.Background(), svcCtx).
		ListOrderEvents(&rpc.ListOrderEventsReq{OrderNo: "to_foreign"})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if len(reply.GetEvents()) != 1 {
		t.Fatalf("台账行数 %d，期望 1（本方法按 order_no 直取，不看归属）", len(reply.GetEvents()))
	}
	// 关键：整条路径只读台账，没有为了「补判归属」去多读主表或写任何东西。
	assertCalls(t, db, "ListOrderEvents")
	assertNoWritesAfter(t, db, mark)
}

func TestListOrderEventsPropagatesStoreError(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	storeErr := errors.New("trade-order: to_order_event list: invalid connection")
	db.listEventsErr = storeErr

	reply, err := NewListOrderEventsLogic(context.Background(), svcCtx).
		ListOrderEvents(&rpc.ListOrderEventsReq{OrderNo: "to_x", Page: 2, Size: 5})
	if !errors.Is(err, storeErr) {
		t.Errorf("依赖错误必须原样上抛，得到 %v", err)
	}
	if reply != nil {
		t.Errorf("报错时不该回应答（空台账会被读成「没人动过这笔订单」）：%s", reply.String())
	}
	// 分页换算必须在报错前就完成并被下传（offset=5,limit=5），否则错误路径会掩盖分页口径。
	if db.lastEventsOff != 5 || db.lastEventsLimit != 5 {
		t.Errorf("下传分页实参不符: offset=%d limit=%d", db.lastEventsOff, db.lastEventsLimit)
	}
	assertCalls(t, db, "ListOrderEvents")
}
