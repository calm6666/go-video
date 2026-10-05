package logic

// 本文件锁两件事：
//  1. helpers.go 里那批纯函数（截断、脱敏、分页、投影、金额乘法）的口径；
//  2. 「列宽常量与迁移 SQL 对齐」这条跨文件不变量。
//
// 这些函数决定了落到 to_order / to_order_event 里的文本形状，一旦放松，
// 表现不是报错而是静默写坏数据（半个中文、超长被 MySQL 裁、凭据落库）。

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/logx"

	memberrpc "go-video/services/membership/rpc"
	paymentrpc "go-video/services/payment/rpc"
	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

func TestTruncateIsRuneSafe(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"短于上限原样返回", "会员月卡", 10, "会员月卡"},
		{"正好等于上限", "abcd", 4, "abcd"},
		{"按 rune 截断不切半个字", "会员会员会员会", 6, "会员会..."},
		{"超长补省略号", strings.Repeat("a", 10), 5, "aa..."},
		{"上限过小直接硬切", "abcdef", 2, "ab"},
		{"空串", "", 5, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncate(c.s, c.n)
			if got != c.want {
				t.Errorf("truncate(%q,%d)=%q，期望 %q", c.s, c.n, got, c.want)
			}
			if n := len([]rune(got)); n > c.n {
				t.Errorf("truncate 结果 %q 长 %d rune，超过上限 %d", got, n, c.n)
			}
		})
	}
}

// TestTruncateNeverProducesInvalidUTF8 钉住「中文商品名不能被切成半个字」：
// 半个 rune 落库后是乱码，而乱码会一路显示到运营页（AGENTS.md §10 的 UTF-8 要求）。
func TestTruncateNeverProducesInvalidUTF8(t *testing.T) {
	long := strings.Repeat("超级大会员年度套餐", 60) // 与 ASCII 混排最易出错
	for n := 1; n <= 60; n++ {
		got := truncate(long, n)
		if !isValidUTF8(got) {
			t.Fatalf("截断到 %d 时产生了非法 UTF-8: %q", n, got)
		}
		if len([]rune(got)) > n {
			t.Fatalf("截断到 %d 时长度仍为 %d", n, len([]rune(got)))
		}
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

// TestColumnWidthConstantsMatchMigration 是 logic 侧的列宽门禁。
// 字面量与 deploy/migrations/trade-order/000001_create_trade_order_tables.sql 逐列对齐，
// model 侧另有 migration_parity_test.go 把同一批宽度钉在 SQL 上，两处共同拦住漂移。
func TestColumnWidthConstantsMatchMigration(t *testing.T) {
	cases := []struct {
		name  string
		got   int
		width int // 迁移里的 VARCHAR(n)
	}{
		{"maxOrderNoLen", maxOrderNoLen, 64},
		{"maxRequestIDLen", maxRequestIDLen, 64},
		{"maxDetailLen", maxDetailLen, 500},
		{"maxReasonLen", maxReasonLen, 500},
		{"maxTitleLen", maxTitleLen, 200},
		{"maxOperatorLen", maxOperatorLen, 64},
		{"maxPaymentNoLen", maxPaymentNoLen, 64},
		{"maxGrantRefLen", maxGrantRefLen, 64},
		{"maxClientTraceLen", maxClientTraceLen, 64},
	}
	for _, c := range cases {
		if c.got > c.width {
			t.Errorf("%s=%d 超过列宽 %d —— 校验放过而写入报错/被静默截断", c.name, c.got, c.width)
		}
		if c.got != c.width {
			t.Errorf("%s=%d 与列宽 %d 不相等 —— 入口校验应精确按列宽收敛，避免半途截断的摘要", c.name, c.got, c.width)
		}
	}
}

func TestDeriveKeyIsDeterministicAndBounded(t *testing.T) {
	k1 := deriveKey("grant", "to_01ABC")
	k2 := deriveKey("grant", "to_01ABC")
	if k1 != k2 {
		t.Errorf("同一订单同一动作的幂等键必须相同，得到 %q / %q", k1, k2)
	}
	if k1 != "grant_to_01ABC" {
		t.Errorf("deriveKey 结果 %q，期望 grant_to_01ABC", k1)
	}
	if deriveKey("revoke", "to_01ABC") == k1 {
		t.Error("grant 与 revoke 撞了同一个键，下游会把回收当重放")
	}
	// 下游 request_id 列宽同样 64：超长必须被裁而不是原样透传。
	long := deriveKey("fulfill", strings.Repeat("x", 200))
	if len(long) > maxRequestIDLen {
		t.Errorf("派生键长 %d，超过 %d", len(long), maxRequestIDLen)
	}
}

func TestSanitizeRedactsCredentialsAndCollapseWhitespace(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		want     string
		contains string
	}{
		{"DSN 必须整条替换", `dial tcp: user:pass@tcp(10.0.0.1:3306)/db`, "downstream error message redacted", ""},
		{"token 泄漏", "upstream said token=abc123", "downstream error message redacted", ""},
		{"密钥字样", "check secret value", "downstream error message redacted", ""},
		{"bearer", "Authorization: Bearer xyz", "downstream error message redacted", ""},
		{"私钥头", "-----BEGIN RSA PRIVATE KEY-----", "downstream error message redacted", ""},
		{"多行压成一行", "line1\n line2\r\tline3", "line1 line2 line3", ""},
		{"普通错误原样保留", "balance not enough", "balance not enough", ""},
		{"空串", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitize(c.in)
			if got != c.want {
				t.Errorf("sanitize(%q)=%q，期望 %q", c.in, got, c.want)
			}
			if strings.ContainsAny(got, "\n\r\t") {
				t.Errorf("sanitize 结果仍含换行/制表，落库会变多行: %q", got)
			}
		})
	}
	// 超长摘要必须收敛到 fulfill_detail 列宽（500），不能把整段下游堆栈落库。
	got := sanitize(strings.Repeat("err ", 500))
	if len([]rune(got)) > maxDetailLen {
		t.Errorf("sanitize 未截断长文本：%d rune", len([]rune(got)))
	}
}

func TestPaginate(t *testing.T) {
	cases := []struct {
		name            string
		page, size, max int64
		wantOffset      int64
		wantLimit       int64
		wantErr         bool
	}{
		{"默认第 1 页 20 条", 0, 0, 100, 0, 20, false},
		{"第 3 页", 3, 10, 100, 20, 10, false},
		{"size 越上限拒绝而不是裁剪", 1, 101, 100, 0, 0, true},
		{"负页码", -1, 10, 100, 0, 0, true},
		{"负 size", 1, -5, 100, 0, 0, true},
		{"maxSize 非正按 100", 1, 50, 0, 0, 50, false},
		{"maxSize 非正时越界", 1, 150, -1, 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			offset, limit, err := paginate(c.page, c.size, c.max)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错，却得到 offset=%d limit=%d", offset, limit)
				}
				if !errors.Is(err, model.ErrInvalidPage) {
					t.Errorf("错误口径应为 ErrInvalidPage，得到 %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if offset != c.wantOffset || limit != c.wantLimit {
				t.Errorf("paginate(%d,%d,%d)=(%d,%d)，期望 (%d,%d)",
					c.page, c.size, c.max, offset, limit, c.wantOffset, c.wantLimit)
			}
		})
	}
}

func TestCheckedMul(t *testing.T) {
	if v, err := checkedMul(31, 12); err != nil || v != 372 {
		t.Errorf("31*12=%d, err=%v", v, err)
	}
	// unit_count 未填（0）视为 1 份，而不是把总时长算成 0。
	if v, err := checkedMul(31, 0); err != nil || v != 31 {
		t.Errorf("unit_count=0 应按 1 处理，得到 %d, err=%v", v, err)
	}
	if v, err := checkedMul(31, -3); err != nil || v != 31 {
		t.Errorf("unit_count<0 应按 1 处理，得到 %d, err=%v", v, err)
	}
	if _, err := checkedMul(math.MaxInt32, 4); !errors.Is(err, model.ErrPlanTierMismatch) {
		t.Errorf("int32 溢出必须报档位不匹配，得到 %v", err)
	}
	if _, err := checkedMul(-1, 5); !errors.Is(err, model.ErrPlanTierMismatch) {
		t.Errorf("负结果也必须被拦住（不能让负时长蒙混过关），得到 %v", err)
	}
}

func TestPlanVisibleOnPlatformFailsClosed(t *testing.T) {
	platforms := []memberrpc.PlanPlatform{memberrpc.PlanPlatform_PLAN_PLATFORM_ANDROID}
	if !planVisibleOnPlatform(platforms, model.PlatformAndroid) {
		t.Error("Android 单应能看到 Android 套餐")
	}
	if planVisibleOnPlatform(platforms, model.PlatformIOS) {
		t.Error("iOS 不该看到只投 Android 的套餐")
	}
	// 资金入口 fail closed：套餐没配可见平台等于谁都买不到。
	if planVisibleOnPlatform(nil, model.PlatformAndroid) {
		t.Error("platforms 为空必须判不可见")
	}
	if planVisibleOnPlatform([]memberrpc.PlanPlatform{}, model.PlatformWeb) {
		t.Error("空切片同样必须判不可见")
	}
	if planVisibleOnPlatform(platforms, 0) {
		t.Error("platform=0（未指定）不该命中任何端")
	}
}

func TestMapPayMethod(t *testing.T) {
	if m, err := mapPayMethod(model.PayBalance); err != nil || m != paymentrpc.PayMethod_PAY_METHOD_BALANCE {
		t.Errorf("PayBalance 应映射到 BALANCE，得到 %v, err=%v", m, err)
	}
	if m, err := mapPayMethod(model.PaySandbox); err != nil || m != paymentrpc.PayMethod_PAY_METHOD_SANDBOX_CHANNEL {
		t.Errorf("PaySandbox 应映射到 SANDBOX_CHANNEL，得到 %v, err=%v", m, err)
	}
	for _, v := range []int32{0, 3, 99, -1} {
		if _, err := mapPayMethod(v); !errors.Is(err, model.ErrInvalidPayMethod) {
			t.Errorf("pay_method=%d 必须显式拒绝（不能静默当沙箱处理），得到 %v", v, err)
		}
	}
}

func TestPaymentRejectNoteNeverLeaksDownstreamText(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"余额不足可自助修复", errors.New("payment: balance not enough"), "balance not enough, recharge first"},
		{"insufficient 同义", errors.New("INSUFFICIENT_FUNDS"), "balance not enough, recharge first"},
		{"渠道未配置", errors.New("refund channel not configured"), "payment channel not configured"},
		{"未知错误给通用文案", errors.New("payment: wallet pay_123 debit failed"), "payment rejected, please retry or contact support"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := paymentRejectNote(c.err); got != c.want {
				t.Errorf("paymentRejectNote=%q，期望 %q", got, c.want)
			}
		})
	}
	// 关键：下游原文里的账号/流水号不能原样吐给终端。
	note := paymentRejectNote(errors.New("wallet w_88371 token=abc debit failed"))
	if strings.Contains(note, "w_88371") || strings.Contains(note, "token") {
		t.Errorf("给客户端的结论里泄漏了下游标识: %q", note)
	}
}

func TestFulfillmentNote(t *testing.T) {
	if got := fulfillmentNote(nil); got != "" {
		t.Errorf("nil 订单应回空串，得到 %q", got)
	}
	for _, s := range []int32{model.StateFulfilling, model.StateFailed} {
		got := fulfillmentNote(&model.Order{State: s, FulfillDetail: "membership rpc down"})
		if !strings.Contains(got, "membership rpc down") {
			t.Errorf("state=%s 时应带出失败摘要，得到 %q", model.StateName(s), got)
		}
	}
	if got := fulfillmentNote(&model.Order{State: model.StateFulfilled, FulfillDetail: "x"}); got != "" {
		t.Errorf("已履约单不该有 pending 结论，得到 %q", got)
	}
}

func TestDupMark(t *testing.T) {
	if dupMark(true) != " (downstream duplicated)" {
		t.Error("duplicated 标记文本变了，台账会看不出幂等重放")
	}
	if dupMark(false) != "" {
		t.Error("非重放不该带标记")
	}
}

func TestNewOrderNoIsUniqueAndBounded(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		no, err := newOrderNo()
		if err != nil {
			t.Fatalf("newOrderNo 报错: %v", err)
		}
		if !strings.HasPrefix(no, orderNoPrefix+"_") {
			t.Fatalf("订单号应带可读前缀，得到 %q", no)
		}
		if len(no) > maxOrderNoLen {
			t.Fatalf("订单号长 %d 超过列宽 %d", len(no), maxOrderNoLen)
		}
		if seen[no] {
			t.Fatalf("同一进程内订单号重复: %s", no)
		}
		seen[no] = true
	}
}

// TestToOrderInfoMapsEveryColumn 逐字段核对投影。
// 这里不用「反射比数量」这种弱断言：漏一个字段的表现是客户端拿到 0 值而不是报错，
// 只有显式列表能拦住。
func TestToOrderInfoMapsEveryColumn(t *testing.T) {
	if toOrderInfo(nil) != nil {
		t.Error("nil 行必须投影成 nil，不能让客户端拿到假订单")
	}
	o := &model.Order{
		ID: 9, OrderNo: "to_01", RequestID: "req-1", Mid: 42, BizType: model.BizCoinPack,
		PlanID: 7, PlanCode: "coin_6", Title: "60 硬币", Quantity: 2, DurationDays: 0,
		CoinAmount: 120, UnitPriceMinor: 300, AmountMinor: 600, RefundedMinor: 100,
		Currency: "CNY", PayMethod: model.PaySandbox, State: model.StateFulfilled,
		FulfillState: model.FulfillDone, FulfillAttempts: 3, FulfillDetail: "",
		PaymentNo: "pay_1", GrantRef: "coin_flow:5", ExpireAt: 1000, Platform: model.PlatformHarmony,
		ClientTraceID: "trace-1", Version: 6, CreatedAt: 10, UpdatedAt: 20,
		PaidAt: 15, FulfilledAt: 18, ClosedAt: 19,
	}
	got := toOrderInfo(o)
	want := &rpc.OrderInfo{
		OrderNo: "to_01", Mid: 42, BizType: rpc.OrderBizType_ORDER_BIZ_TYPE_COIN_PACK, PlanId: 7,
		PlanCode: "coin_6", Title: "60 硬币", Quantity: 2, DurationDays: 0, CoinAmount: 120,
		UnitPriceMinor: 300, AmountMinor: 600, RefundedMinor: 100, Currency: "CNY",
		PayMethod: rpc.PayMethod_PAY_METHOD_SANDBOX_CHANNEL, State: rpc.OrderState_ORDER_STATE_FULFILLED,
		FulfillState: rpc.FulfillState_FULFILL_STATE_SUCCEEDED, FulfillAttempts: 3,
		PaymentNo: "pay_1", GrantRef: "coin_flow:5", ExpireAt: 1000,
		Platform: rpc.Platform_PLATFORM_HARMONY, ClientTraceId: "trace-1", RequestId: "req-1",
		Version: 6, CreatedAt: 10, UpdatedAt: 20, PaidAt: 15, FulfilledAt: 18, ClosedAt: 19,
	}
	if got.String() != want.String() {
		t.Errorf("投影不一致：\n got=%s\nwant=%s", got.String(), want.String())
	}
}

func TestOrderInfosAlwaysNonNil(t *testing.T) {
	// 空列表回非 nil 切片是客户端契约（避免为 null 数组写分支）。
	if got := orderInfos(nil); got == nil {
		t.Error("orderInfos(nil) 返回 nil 切片")
	} else if len(got) != 0 {
		t.Errorf("期望空切片，得到 %d 条", len(got))
	}
	if got := eventInfos(nil); got == nil {
		t.Error("eventInfos(nil) 返回 nil 切片")
	}
	if rows := orderInfos([]*model.Order{{OrderNo: "a"}, nil, {OrderNo: "b"}}); len(rows) != 3 {
		t.Errorf("行数应随行数一致，得到 %d", len(rows))
	} else if rows[1] != nil {
		t.Error("nil 行应投影成 nil 而不是被丢弃")
	}
}

func TestToEventInfo(t *testing.T) {
	if toEventInfo(nil) != nil {
		t.Error("nil 台账行必须投影成 nil")
	}
	e := &model.OrderEvent{
		EventID: 3, OrderNo: "to_01", FromState: model.StatePaid, ToState: model.StateFulfilling,
		Operator: "cron", Reason: "attempt", RequestID: "r1", Ctime: 1234,
	}
	got := toEventInfo(e)
	if got.GetEventId() != 3 || got.GetOrderNo() != "to_01" ||
		got.GetFromState() != rpc.OrderState_ORDER_STATE_PAID ||
		got.GetToState() != rpc.OrderState_ORDER_STATE_FULFILLING ||
		got.GetOperator() != "cron" || got.GetReason() != "attempt" || got.GetCtime() != 1234 {
		t.Errorf("台账投影字段不符: %s", got.String())
	}
}

// TestRequireOrderUsesNotFoundSemanticsForForeignOrder 钉住越权口径：
// 「查不到」与「不是你的」必须是同一个哨兵，否则错误码差异能枚举订单号是否存在。
func TestRequireOrderUsesNotFoundSemanticsForForeignOrder(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedOrder(t, db, &model.Order{OrderNo: "to_own", Mid: 1001, State: model.StatePaid})
	h := newHelper(context.Background(), svcCtx, quietLogger())

	if _, err := h.requireOrder("   ", 0); !errors.Is(err, model.ErrOrderNoRequired) {
		t.Errorf("空白订单号应回 ErrOrderNoRequired，得到 %v", err)
	}
	if _, err := h.requireOrder("to_missing", 0); !errors.Is(err, model.ErrOrderNotFound) {
		t.Errorf("不存在的订单应回 ErrOrderNotFound，得到 %v", err)
	}
	if _, err := h.requireOrder("to_own", 9999); !errors.Is(err, model.ErrOrderNotFound) {
		t.Errorf("别人的订单必须回同一个 not-found 哨兵，得到 %v", err)
	}
	got, err := h.requireOrder("to_own", 1001)
	if err != nil || got == nil || got.Mid != 1001 {
		t.Errorf("本人读取应成功，得到 %+v, err=%v", got, err)
	}
	// mid<=0 是服务内部（cron/履约）入口的口径：不带归属过滤。
	if _, err = h.requireOrder("to_own", 0); err != nil {
		t.Errorf("内部读取不该做归属校验: %v", err)
	}
	if ErrOrderNotFoundSilent != model.ErrOrderNotFound {
		t.Error("两个 not-found 出口不再是同一个错误，越权探测口子被打开")
	}
}

// TestRequireOrderPropagatesModelError 保证「数据库报错」不会被降级成「订单不存在」。
func TestRequireOrderPropagatesModelError(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	svcCtx.Orders = &failingOrderModel{fakeOrderModel: fakeOrderModel{db: db}, err: errors.New("dead")}
	h := newHelper(context.Background(), svcCtx, logx.WithContext(context.Background()))
	if _, err := h.requireOrder("to_x", 0); err == nil || strings.Contains(err.Error(), "not found") {
		t.Errorf("底层错误必须原样上抛，不能伪装成 not-found，得到 %v", err)
	}
}

type failingOrderModel struct {
	fakeOrderModel
	err error
}

func (f *failingOrderModel) FindByOrderNo(context.Context, string) (*model.Order, error) {
	return nil, f.err
}

func TestServiceHelperCarriesContext(t *testing.T) {
	svcCtx, _ := newTestSvc(t)
	logger := logx.WithContext(context.Background())
	h := newHelper(context.Background(), svcCtx, logger)
	if h.svcCtx != svcCtx {
		t.Error("helper 没带上 svcCtx")
	}
	if h.logger == nil {
		t.Error("helper 必须有 logger：runFulfill 的失败日志走它，nil 会在最坏时刻 panic")
	}
	if h.ctx == nil {
		t.Error("helper 必须带上 ctx（超时与 trace_id 全靠它传递）")
	}
}
