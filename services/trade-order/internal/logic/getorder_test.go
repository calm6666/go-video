package logic

// GetOrder 的用例级测试（订单详情 + 归属校验）。
//
// 这个方法的契约要点（rpc/tradeorder.proto 的 GetOrderReq.mid 注释、README §7）：
//   - order_no 必填，且裁剪发生在触库之前；
//   - 带 mid 时校验归属，「不存在」与「不是你的」必须是**同一个应答形状**，
//     否则调用方可以用应答差异枚举出哪些订单号真实存在（AGENTS.md §5 的越权读）；
//   - mid=0 是服务内部（运营面/履约链路）口径，不做归属过滤；
//   - 库存故障绝不能被降级成 found=false（那会把「查不到」伪装成「没有这笔钱」）。
//
// 本文件只换数据访问替身（fakes_test.go），投影走真实的 toOrderInfo。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

func TestGetOrderRejectsBlankOrderNoBeforeTouchingDB(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	l := NewGetOrderLogic(context.Background(), svcCtx)

	for _, in := range []string{"", "   ", "\t\n", "\u3000"} {
		got, err := l.GetOrder(&rpc.GetOrderReq{OrderNo: in})
		if !errors.Is(err, model.ErrOrderNoRequired) {
			t.Errorf("order_no=%q 应回 ErrOrderNoRequired，得到 %v", in, err)
		}
		if got != nil {
			t.Errorf("order_no=%q 被拒时仍返回了应答：%s", in, got.String())
		}
	}
	// 关键：四个空白入参一次库都没碰（守卫在 FindByOrderNo 之前）。
	assertCalls(t, db)
}

// TestGetOrderTrimsBeforeQuery 钉住「裁剪后的订单号才是查询键」：
// 真实订单号由 idgen 生成、不含空白，带空白的入参只可能是调用方没 trim。
// 若不裁剪，带空白的合法订单号会查不到而伪装成 not-found。
func TestGetOrderTrimsBeforeQuery(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedOrder(t, db, &model.Order{OrderNo: "to_trim", Mid: 1001})

	got, err := NewGetOrderLogic(context.Background(), svcCtx).
		GetOrder(&rpc.GetOrderReq{OrderNo: "  to_trim \t"})
	if err != nil {
		t.Fatalf("裁剪后应能命中: %v", err)
	}
	if !got.GetFound() || got.GetOrder().GetOrderNo() != "to_trim" {
		t.Errorf("应答不是裁剪后的那一行: %s", got.String())
	}
	assertCalls(t, db, "FindByOrderNo")
}

func TestGetOrderMissingOrderAnswersNotFound(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedOrder(t, db, &model.Order{OrderNo: "to_other", Mid: 1001})

	got, err := NewGetOrderLogic(context.Background(), svcCtx).
		GetOrder(&rpc.GetOrderReq{OrderNo: "to_absent", Mid: 1001})
	if err != nil {
		t.Fatalf("查不到不是错误: %v", err)
	}
	if got.GetFound() {
		t.Error("found=true 但库里没有这一行")
	}
	if got.GetOrder() != nil {
		t.Errorf("found=false 时 order 必须留空，得到 %s", got.GetOrder().String())
	}
	assertCalls(t, db, "FindByOrderNo")
	assertReadOnly(t, db)
}

// TestGetOrderForeignOwnerIsIndistinguishableFromMissing 是越权探测的护栏：
// 「不是你的单」和「没有这笔单」两个应答必须逐字节相同（都是 found=false、order=nil），
// 一旦其中一条分支多带信息（错误码、订单号回显、部分字段），订单号就成了可枚举资源。
func TestGetOrderForeignOwnerIsIndistinguishableFromMissing(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedOrder(t, db, &model.Order{OrderNo: "to_secret", Mid: 1001, AmountMinor: 8800})
	l := NewGetOrderLogic(context.Background(), svcCtx)

	missing, err := l.GetOrder(&rpc.GetOrderReq{OrderNo: "to_absent", Mid: 2002})
	if err != nil {
		t.Fatalf("不存在分支报错: %v", err)
	}
	foreign, err := l.GetOrder(&rpc.GetOrderReq{OrderNo: "to_secret", Mid: 2002})
	if err != nil {
		t.Fatalf("越权分支必须回 found=false 而不是报错，得到 %v", err)
	}
	if foreign.GetFound() {
		t.Fatal("越权读到了别人的订单")
	}
	if foreign.GetOrder() != nil {
		t.Error("越权应答里仍带出了订单对象")
	}
	if missing.String() != foreign.String() {
		t.Errorf("两条 not-found 分支应答形状不同，可用于枚举订单号：\n missing=%s\n foreign=%s",
			missing.String(), foreign.String())
	}
	// 判定只用同一次读：不能为了区分两种情况去补第二次查询（补读本身就是泄露口子）。
	assertCalls(t, db, "FindByOrderNo", "FindByOrderNo")
	assertReadOnly(t, db)
}

// TestGetOrderOwnerSeesEveryColumn 逐字段核对应答投影：
// 每个 OrderInfo 字段都必须来自被读到的那一行的对应列，
// 漏映射的表现是客户端拿到 0 值而不是报错，所以只能用显式列表钉。
func TestGetOrderOwnerSeesEveryColumn(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	seedOrder(t, db, &model.Order{
		ID: 77, OrderNo: "to_full", RequestID: "req-full", Mid: 1001, BizType: model.BizCoinPack,
		PlanID: 7, PlanCode: "coin_6", Title: "60 硬币", Quantity: 2, DurationDays: 0,
		CoinAmount: 120, UnitPriceMinor: 300, AmountMinor: 600, RefundedMinor: 100,
		Currency: "CNY", PayMethod: model.PaySandbox, State: model.StateFulfilled,
		FulfillState: model.FulfillDone, FulfillAttempts: 3, FulfillDetail: "",
		PaymentNo: "pay_1", GrantRef: "coin_flow:5", ExpireAt: now + 900, Platform: model.PlatformDesktop,
		ClientTraceID: "trace-1", Version: 6, CreatedAt: now - 120, UpdatedAt: now - 30,
		PaidAt: now - 90, FulfilledAt: now - 60, ClosedAt: 0,
	})

	got, err := NewGetOrderLogic(context.Background(), svcCtx).
		GetOrder(&rpc.GetOrderReq{OrderNo: "to_full", Mid: 1001})
	if err != nil {
		t.Fatalf("本人读取失败: %v", err)
	}
	if !got.GetFound() {
		t.Fatal("found=false，本人读不到自己的单")
	}
	want := &rpc.OrderInfo{
		OrderNo: "to_full", Mid: 1001, BizType: rpc.OrderBizType_ORDER_BIZ_TYPE_COIN_PACK, PlanId: 7,
		PlanCode: "coin_6", Title: "60 硬币", Quantity: 2, DurationDays: 0, CoinAmount: 120,
		UnitPriceMinor: 300, AmountMinor: 600, RefundedMinor: 100, Currency: "CNY",
		PayMethod: rpc.PayMethod_PAY_METHOD_SANDBOX_CHANNEL, State: rpc.OrderState_ORDER_STATE_FULFILLED,
		FulfillState: rpc.FulfillState_FULFILL_STATE_SUCCEEDED, FulfillAttempts: 3, FulfillDetail: "",
		PaymentNo: "pay_1", GrantRef: "coin_flow:5", ExpireAt: now + 900,
		Platform: rpc.Platform_PLATFORM_DESKTOP, ClientTraceId: "trace-1", RequestId: "req-full",
		Version: 6, CreatedAt: now - 120, UpdatedAt: now - 30, PaidAt: now - 90, FulfilledAt: now - 60,
	}
	if got.GetOrder().String() != want.String() {
		// want 是逐字段手写的字面量：相等同时证明「每列都映射了」和
		// 「没有多映射出契约外的字段」（内部自增主键就是被这条拦住的）。
		t.Errorf("应答投影与订单行不一致：\n got=%s\nwant=%s", got.GetOrder().String(), want.String())
	}
	assertCalls(t, db, "FindByOrderNo")
	assertReadOnly(t, db)
}

// TestGetOrderZeroMidIsInternalRead 钉住 mid=0 的口径：不带归属过滤，
// 这是运营面/履约链路（CancelOrder、ApproveRefund 内部回读）用的读法。
// 谁能用这个读法由网关挡：/admin/order/get 属只读组，routes.go:1903-1929 那组
// **不挂 AdminPermission 权限点**（只查管理员会话，见 gateway/admin/internal/logic/orderlistlogic.go:33
// 的「只读路由，不挂 AdminPermission」口径），所以 mid=0 的全量读在本服务侧确实无闸门 ——
// 本用例只钉「mid=0 就是内部口径」这一事实，不美化它的权限强度。
func TestGetOrderZeroMidIsInternalRead(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedOrder(t, db, &model.Order{OrderNo: "to_inner", Mid: 1001, State: model.StatePaying})

	got, err := NewGetOrderLogic(context.Background(), svcCtx).GetOrder(&rpc.GetOrderReq{OrderNo: "to_inner"})
	if err != nil {
		t.Fatalf("内部读失败: %v", err)
	}
	if !got.GetFound() || got.GetOrder().GetMid() != 1001 {
		t.Errorf("mid=0 应回订单本体，得到 %s", got.String())
	}
}

// TestGetOrderNegativeMidCurrentlySkipsOwnershipCheck 钉住**当前行为**（不美化）：
// 缺陷：services/trade-order/internal/logic/getorderlogic.go:44 —— 归属判定写成
// `if mid := in.GetMid(); mid > 0 && order.Mid != mid`，于是 mid<0 落进「不校验归属」分支，
// 与 mid=0（服务内部口径）混为一路 —— 而 getorderlogic.go:30 的自述与契约注释
// services/trade-order/rpc/tradeorder.proto:141（「非 0 时校验归属」）说的都是 **非 0**。
// 后果（越权读 + 审计）：终端把 mid 传成 -1 就能读到任意用户的订单本体（金额、支付单号、
// 发放引用、追踪号全在 OrderInfo 里），而且这条分支不会走到 getorderlogic.go:45 那行
// 「belongs to another mid, answered not-found」的越权告警日志 —— 审计里连痕迹都不留。
// 同一个包里 ListMyOrders 对 mid<=0 是直接 model.ErrInvalidMid（listmyorderslogic.go:34-36），
// 两个入口口径不一致。
// 当前爆炸半径（如实记录，别夸大）：两个网关恰好各挡了一道 ——
// gateway/app/internal/logic/conv_commerce.go:38-43 的 requireMid 拒 mid<=0，
// gateway/admin/internal/logic/ordergetlogic.go:53 的 orderNonNeg 拒负数；
// 所以这条目前不是终端可利用的越权，而是**服务侧闸门缺失**：任何直连 gRPC 的调用方
// （cron、将来的 BFF、内部脚本）都绕得过，网关闸门一旦改动就立刻成真。
// 修法方向：`mid == 0` 才走内部读，`mid < 0` 返回 model.ErrInvalidMid（与 ListMyOrders 对齐），
// 或统一成 `mid != 0` 判定；两者都要同步改 README §7 与 getorderlogic.go:30 的口径描述。
// 本用例按现状断言，改动生产代码时必须同步翻转这里的期望值。
func TestGetOrderNegativeMidCurrentlySkipsOwnershipCheck(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedOrder(t, db, &model.Order{OrderNo: "to_leak", Mid: 1001, AmountMinor: 9900})

	got, err := NewGetOrderLogic(context.Background(), svcCtx).
		GetOrder(&rpc.GetOrderReq{OrderNo: "to_leak", Mid: -1})
	if err != nil {
		t.Fatalf("当前实现不拒绝负 mid，得到 %v", err)
	}
	if !got.GetFound() || got.GetOrder().GetMid() != 1001 {
		t.Fatalf("钉住了，但行为已变（生产代码可能已修）：得到 %s", got.String())
	}
	if got.GetOrder().GetAmountMinor() != 9900 {
		t.Errorf("金额字段应与行一致，得到 %d", got.GetOrder().GetAmountMinor())
	}
	assertReadOnly(t, db)
}

// TestGetOrderPropagatesStoreError 保证「库故障」不会被降级成 found=false：
// 后者会让调用方把一次数据库故障读成「这笔订单不存在」，在对账场景里等于抹掉一笔资金事实。
func TestGetOrderPropagatesStoreError(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	storeErr := errors.New("trade-order: dial tcp: connection refused")
	db.findOrderErr = storeErr

	got, err := NewGetOrderLogic(context.Background(), svcCtx).
		GetOrder(&rpc.GetOrderReq{OrderNo: "to_x", Mid: 1001})
	if !errors.Is(err, storeErr) {
		t.Errorf("依赖错误必须原样上抛，得到 %v", err)
	}
	if got != nil {
		t.Errorf("报错时不该带回应答（found=false 会被读成订单不存在）：%s", got.String())
	}
	assertCalls(t, db, "FindByOrderNo")
}
