package logic

// 余额调整（AdjustBalance）、退款（RefundPayment）与余额读取（GetWallet）。
//
// 本文件要证明的口径（三个 logic 头部「口径」注释在此落成断言）：
//  1. AdjustBalance 只有 AJ_ 前缀流水、没有单据：调整靠 reason + operator 留痕，
//     充值必须走充值单，两条路径不得混用；
//  2. 结果余额永不为负：负向调整走条件扣减，未命中即拒，且拒绝时一条流水都不写；
//  3. RefundPayment 只支持退回到余额，且只对余额支付成立 —— 沙箱渠道单退成余额
//     等于凭空加钱；要求原路退回渠道就是 not-configured，绝不假装「已退回」；
//  4. 累计退款不得超过 amount_minor，守卫写在 UPDATE 的 WHERE 里（0 行即整笔回滚）；
//  5. 「写退款单 + 更新支付单 + 加余额 + 写流水」同一事务，任何一步失败整体回滚；
//  6. 读余额不给账户建行：账户不存在就是 0 余额。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/payment/internal/config"
	"go-video/services/payment/internal/svc"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"

	"google.golang.org/grpc/codes"
)

func adjustReq(mid, delta int64, requestID string) *rpc.AdjustBalanceReq {
	return &rpc.AdjustBalanceReq{
		Mid: mid, DeltaMinor: delta, Currency: "CNY",
		Operator: "运营工号 A01", RequestId: requestID, Reason: "误扣补偿，需人工调整",
	}
}

func mustAdjust(t *testing.T, svcCtx *svc.ServiceContext, in *rpc.AdjustBalanceReq) *rpc.AdjustBalanceReply {
	t.Helper()
	reply, err := NewAdjustBalanceLogic(context.Background(), svcCtx).AdjustBalance(in)
	if err != nil {
		t.Fatalf("AdjustBalance(mid=%d delta=%d) 失败: %v", in.Mid, in.DeltaMinor, err)
	}
	return reply
}

func refundReq(paymentNo string, amount int64, requestID string) *rpc.RefundPaymentReq {
	return &rpc.RefundPaymentReq{
		PaymentNo: paymentNo, AmountMinor: amount, ToBalance: true,
		Operator: "运营工号 A01", RequestId: requestID, Reason: "用户申请退款",
	}
}

func mustRefund(t *testing.T, svcCtx *svc.ServiceContext, in *rpc.RefundPaymentReq) *rpc.RefundPaymentReply {
	t.Helper()
	reply, err := NewRefundPaymentLogic(context.Background(), svcCtx).RefundPayment(in)
	if err != nil {
		t.Fatalf("RefundPayment(payment_no=%s amount=%d) 失败: %v", in.PaymentNo, in.AmountMinor, err)
	}
	return reply
}

// --- AdjustBalance ---

func TestAdjustBalancePositiveWritesAdjustFlowOnly(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 1000, "CNY")

	reply := mustAdjust(t, svcCtx, adjustReq(7, 500, "req-adj-1"))

	if reply.Duplicated {
		t.Fatal("首次调整不该 duplicated")
	}
	if reply.FlowId == 0 {
		t.Fatal("调整必须返回流水号，0 表示台账没落地")
	}
	if reply.Wallet.BalanceMinor != 1500 {
		t.Fatalf("余额 = %d，期望 1500", reply.Wallet.BalanceMinor)
	}
	requireFlows(t, db, 1)
	fl := db.flows[0]
	if fl.BizType != model.FlowBizAdminAdjust {
		t.Fatalf("biz_type = %d，调整只能记 ADMIN_ADJUST(%d)", fl.BizType, model.FlowBizAdminAdjust)
	}
	if !strings.HasPrefix(fl.BizNo, "AJ_") {
		t.Fatalf("调整流水 biz_no = %q，应为 AJ_<ULID>（与充值/消费单据区分开）", fl.BizNo)
	}
	if fl.DeltaMinor != 500 || fl.BalanceAfterMinor != 1500 {
		t.Fatalf("流水金额不对：delta=%d balance_after=%d", fl.DeltaMinor, fl.BalanceAfterMinor)
	}
	if fl.Remark != "误扣补偿，需人工调整" || fl.Operator != "运营工号 A01" || fl.RequestId != "req-adj-1" {
		t.Fatalf("调整留痕缺失：remark=%q operator=%q request_id=%q", fl.Remark, fl.Operator, fl.RequestId)
	}
	// 调整不留单据：只有流水，不能被误当成充值或消费。
	if len(db.recharges) != 0 || len(db.payments) != 0 || len(db.refunds) != 0 {
		t.Fatalf("调整写出了单据：recharge=%d payment=%d refund=%d",
			len(db.recharges), len(db.payments), len(db.refunds))
	}
	if db.txRuns != 1 {
		t.Fatalf("改余额与写流水应在同一个事务（txRuns=%d）", db.txRuns)
	}
}

func TestAdjustBalanceNegativeWithinBalance(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 1000, "CNY")

	mustAdjust(t, svcCtx, adjustReq(7, -400, "req-adj-2"))

	requireBalance(t, db, 7, 600)
	requireFlows(t, db, 1)
	if db.flows[0].DeltaMinor != -400 || db.flows[0].BalanceAfterMinor != 600 {
		t.Fatalf("负向调整流水不对：%+v", db.flows[0])
	}
}

func TestAdjustBalanceNeverDrivesBalanceNegative(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 100, "CNY")

	_, err := NewAdjustBalanceLogic(context.Background(), svcCtx).AdjustBalance(adjustReq(7, -500, "req-adj-3"))

	requireSentinel(t, err, model.ErrInsufficientBalance, codes.FailedPrecondition)
	requireBalance(t, db, 7, 100)
	requireFlows(t, db, 0) // 拒绝时不能留无意义流水污染对账
	if db.rollbacks != 1 {
		t.Fatalf("失败事务应回滚，实际 rollbacks=%d", db.rollbacks)
	}
}

func TestAdjustBalanceRejectedWithoutAccountRow(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())

	_, err := NewAdjustBalanceLogic(context.Background(), svcCtx).AdjustBalance(adjustReq(7, -1, "req-adj-4"))

	requireSentinel(t, err, model.ErrInsufficientBalance, codes.FailedPrecondition)
	if _, ok := db.wallets[7]; ok {
		t.Fatal("扣减失败后不应留下账户行")
	}
	requireFlows(t, db, 0)
}

func TestAdjustBalanceZeroDeltaAndOverLimitRejected(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 100, "CNY")
	cfg := defaultPaymentConf()
	over := cfg.MaxAdjustMinor + 1

	for _, delta := range []int64{0, over, -over} {
		_, err := NewAdjustBalanceLogic(context.Background(), svcCtx).AdjustBalance(adjustReq(7, delta, "req-adj-5"))
		want := model.ErrAdjustDeltaZero
		if delta != 0 {
			want = model.ErrAdjustAmountOutOfRange
		}
		requireSentinel(t, err, want, codes.InvalidArgument)
	}
	requireBalance(t, db, 7, 100)
	requireFlows(t, db, 0)
}

// TestAdjustBalanceUsesConfiguredLimit 上限来自配置而不是写死的数字。
func TestAdjustBalanceUsesConfiguredLimit(t *testing.T) {
	cfg := defaultPaymentConf()
	cfg.MaxAdjustMinor = 50
	svcCtx, db := newTestSvc(t, cfg)
	db.seedWallet(7, 1000, "CNY")

	_, err := NewAdjustBalanceLogic(context.Background(), svcCtx).AdjustBalance(adjustReq(7, 51, "req-adj-6"))
	requireSentinel(t, err, model.ErrAdjustAmountOutOfRange, codes.InvalidArgument)

	mustAdjust(t, svcCtx, adjustReq(7, 50, "req-adj-7"))
	requireBalance(t, db, 7, 1050)
}

func TestAdjustBalanceMissingOperatorOrReasonRejected(t *testing.T) {
	cases := []struct {
		name     string
		patch    func(*rpc.AdjustBalanceReq)
		sentinel error
		msg      string
	}{
		{"缺操作人", func(r *rpc.AdjustBalanceReq) { r.Operator = "  " }, model.ErrOperatorRequired, ""},
		{"操作人超长", func(r *rpc.AdjustBalanceReq) { r.Operator = strings.Repeat("运", 65) },
			nil, "operator too long"},
		{"缺理由", func(r *rpc.AdjustBalanceReq) { r.Reason = "" }, model.ErrReasonRequired, ""},
		{"理由是空格", func(r *rpc.AdjustBalanceReq) { r.Reason = "   " }, model.ErrReasonRequired, ""},
		{"理由超长", func(r *rpc.AdjustBalanceReq) { r.Reason = strings.Repeat("调", 101) },
			nil, "reason too long"},
		{"缺幂等键", func(r *rpc.AdjustBalanceReq) { r.RequestId = "" }, model.ErrRequestIDRequired, ""},
		{"mid 非法", func(r *rpc.AdjustBalanceReq) { r.Mid = -1 }, model.ErrInvalidMid, ""},
		{"外币", func(r *rpc.AdjustBalanceReq) { r.Currency = "EUR" }, model.ErrUnsupportedCurrency, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t, defaultPaymentConf())
			db.seedWallet(7, 1000, "CNY")
			in := adjustReq(7, 100, "req-adj-8")
			tc.patch(in)

			_, err := NewAdjustBalanceLogic(context.Background(), svcCtx).AdjustBalance(in)

			if tc.sentinel != nil {
				requireSentinel(t, err, tc.sentinel, codes.InvalidArgument)
			} else {
				requireStatus(t, err, codes.InvalidArgument, tc.msg)
			}
			requireBalance(t, db, 7, 1000)
			requireFlows(t, db, 0)
		})
	}
}

// TestAdjustBalanceReasonAccepts100ChineseRunes 长度按 rune 计，
// 中文审计理由不会被「按字节截断」而丢掉真实语义。
func TestAdjustBalanceReasonAccepts100ChineseRunes(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 1000, "CNY")
	reason := strings.Repeat("补偿", 50) // 100 rune / 300 字节

	in := adjustReq(7, 20, "req-adj-11")
	in.Reason = reason
	mustAdjust(t, svcCtx, in)
	if db.flows[len(db.flows)-1].Remark != reason {
		t.Fatal("中文理由被截断或改写了")
	}

	in2 := adjustReq(7, 30, "req-adj-12")
	in2.Reason = reason + "额" // 101 rune：超限就拒，不截断入库
	_, err := NewAdjustBalanceLogic(context.Background(), svcCtx).AdjustBalance(in2)
	requireStatus(t, err, codes.InvalidArgument, "reason too long")
	requireBalance(t, db, 7, 1020)
}

func TestAdjustBalanceWalletCurrencyMismatch(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 1000, "USD")

	_, err := NewAdjustBalanceLogic(context.Background(), svcCtx).AdjustBalance(adjustReq(7, 100, "req-adj-13"))

	requireSentinel(t, err, model.ErrUnsupportedCurrency, codes.InvalidArgument)
	requireBalance(t, db, 7, 1000)
	requireFlows(t, db, 0)
}

func TestAdjustBalanceReplayByRequestID(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 1000, "CNY")
	first := mustAdjust(t, svcCtx, adjustReq(7, 500, "req-adj-14"))

	again := mustAdjust(t, svcCtx, adjustReq(7, 500, "req-adj-14"))

	if !again.Duplicated {
		t.Fatal("同 request_id 重放必须 duplicated=true")
	}
	if again.FlowId != first.FlowId {
		t.Fatalf("重放流水号 = %d，期望首笔 %d", again.FlowId, first.FlowId)
	}
	requireBalance(t, db, 7, 1500) // 只入账一次
	requireFlows(t, db, 1)
	if db.txRuns != 1 {
		t.Fatalf("重放不该再进事务（txRuns=%d）", db.txRuns)
	}
}

// TestAdjustBalanceRequestIDUsedByRechargeRejected 号被别的业务动作用掉了：
// 不能把它当本次调整的重放，否则「一次充值」会被运营读成「一次调整」。
func TestAdjustBalanceRequestIDUsedByRechargeRejected(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 1000, "CNY")
	db.seedFlow(&model.Flow{Mid: 7, BizType: model.FlowBizRecharge, BizNo: "RC_other",
		DeltaMinor: 1000, BalanceAfterMinor: 1000, Currency: "CNY", RequestId: "shared-request"})

	_, err := NewAdjustBalanceLogic(context.Background(), svcCtx).AdjustBalance(adjustReq(7, 500, "shared-request"))

	requireSentinel(t, err, model.ErrRequestIDReused, codes.AlreadyExists)
	requireBalance(t, db, 7, 1000)
	requireFlows(t, db, 1) // 只有原先那条充值流水
}

func TestAdjustBalanceSameRequestIDDifferentMidRejected(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 1000, "CNY")
	db.seedWallet(8, 1000, "CNY")
	mustAdjust(t, svcCtx, adjustReq(7, 500, "req-adj-cross-mid"))

	_, err := NewAdjustBalanceLogic(context.Background(), svcCtx).AdjustBalance(adjustReq(8, 500, "req-adj-cross-mid"))

	requireSentinel(t, err, model.ErrRequestIDReused, codes.AlreadyExists)
	requireBalance(t, db, 7, 1500)
	requireBalance(t, db, 8, 1000)
}

func TestAdjustBalanceFlowFailureRollsBackBalance(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 1000, "CNY")
	db.flowErr = errors.New("pm_flow InsertTx: dial tcp: i/o timeout")

	_, err := NewAdjustBalanceLogic(context.Background(), svcCtx).AdjustBalance(adjustReq(7, 500, "req-adj-15"))

	if err == nil || !strings.Contains(err.Error(), "pm_flow InsertTx") {
		t.Fatalf("底层报错必须上抛，实际 %v", err)
	}
	requireBalance(t, db, 7, 1000) // 不允许「加了钱没流水」
	requireFlows(t, db, 0)
}

func TestAdjustBalanceConcurrentDuplicateReplaysWinner(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 1000, "CNY")
	db.flowSteal = func(db *fakeDB) {
		winner := db.seedFlow(&model.Flow{Mid: 7, BizType: model.FlowBizAdminAdjust, BizNo: "AJ_winner",
			DeltaMinor: 500, BalanceAfterMinor: 1500, Currency: "CNY", Operator: "运营工号 A01",
			RequestId: "req-adj-16"})
		winner.FlowId = 777
	}

	reply, err := NewAdjustBalanceLogic(context.Background(), svcCtx).AdjustBalance(adjustReq(7, 500, "req-adj-16"))
	if err != nil {
		t.Fatalf("唯一键冲突应收敛成重放，实际报错: %v", err)
	}
	if !reply.Duplicated || reply.FlowId != 777 {
		t.Fatalf("重放结论不符：duplicated=%v flow_id=%d", reply.Duplicated, reply.FlowId)
	}
	// 本次失败尝试的加钱已随事务回滚（对手那笔属于对手事务）。
	requireBalance(t, db, 7, 1000)
	if len(db.flows) != 1 || db.flows[0].BizNo != "AJ_winner" {
		t.Fatalf("并发下多出了流水：%+v", db.flows)
	}
}

func TestAdjustBalanceDuplicateWithoutFlowRowIsReuse(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 1000, "CNY")
	db.flowDup = true

	_, err := NewAdjustBalanceLogic(context.Background(), svcCtx).AdjustBalance(adjustReq(7, 500, "req-adj-17"))

	requireSentinel(t, err, model.ErrRequestIDReused, codes.AlreadyExists)
	requireBalance(t, db, 7, 1000)
	requireFlows(t, db, 0)
}

func TestAdjustBalanceDocumentNoFailureWritesNothing(t *testing.T) {
	withFailingIDGen(t)
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 1000, "CNY")

	_, err := NewAdjustBalanceLogic(context.Background(), svcCtx).AdjustBalance(adjustReq(7, 500, "req-adj-18"))

	requireSentinel(t, err, model.ErrDocumentNoUnavailable, codes.Internal)
	requireBalance(t, db, 7, 1000)
	requireFlows(t, db, 0)
	if db.txRuns != 0 {
		t.Fatalf("拿不到单据号却进了事务（txRuns=%d）", db.txRuns)
	}
}

// --- RefundPayment ---

func paidRefundablePayment(db *fakeDB, paymentNo, bizOrderNo string, mid, amount int64) *model.Payment {
	return db.seedPayment(&model.Payment{
		PaymentNo: paymentNo, BizOrderNo: bizOrderNo, RequestId: "req-pay-" + paymentNo, Mid: mid,
		AmountMinor: amount, Currency: "CNY", Method: model.MethodBalance,
		State: model.PaymentStatePaid, Operator: "user",
	})
}

func TestRefundPaymentToBalanceCreditsLedgerInOneTransaction(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	paidRefundablePayment(db, "PM_r1", "order-r1", 7, 5000)
	db.seedWallet(7, 3000, "CNY")

	reply := mustRefund(t, svcCtx, refundReq("PM_r1", 2000, "req-rf-1"))

	if reply.Duplicated {
		t.Fatal("首笔退款不该 duplicated")
	}
	if !strings.HasPrefix(reply.Refund.RefundNo, "RF_") {
		t.Fatalf("refund_no = %q，应为 RF_<ULID>", reply.Refund.RefundNo)
	}
	if reply.Refund.Destination != model.DestinationBalance {
		t.Fatalf("destination = %q，本服务只有 %q", reply.Refund.Destination, model.DestinationBalance)
	}
	if reply.Refund.State != rpc.RefundState_REFUND_STATE_SUCCEEDED {
		t.Fatalf("退款状态 = %s", reply.Refund.State)
	}
	if reply.Wallet.BalanceMinor != 5000 {
		t.Fatalf("余额 = %d，期望 3000+2000", reply.Wallet.BalanceMinor)
	}
	if reply.Payment.State != rpc.PaymentState_PAYMENT_STATE_PARTIALLY_REFUNDED {
		t.Fatalf("部分退款后 state = %s", reply.Payment.State)
	}
	if reply.Payment.RefundedMinor != 2000 {
		t.Fatalf("refunded_minor = %d，期望 2000", reply.Payment.RefundedMinor)
	}

	row := db.refunds[reply.Refund.RefundNo]
	if row == nil {
		t.Fatal("退款单没落库")
	}
	if row.PaymentNo != "PM_r1" || row.BizOrderNo != "order-r1" || row.Mid != 7 || row.Currency != "CNY" {
		t.Fatalf("退款单未绑定原单：%+v", row)
	}
	if row.State != model.RefundStateSucceeded || row.Operator != "运营工号 A01" || row.Reason != "用户申请退款" {
		t.Fatalf("退款单审计列缺失：%+v", row)
	}
	requireFlows(t, db, 1)
	fl := db.flows[0]
	if fl.BizType != model.FlowBizRefund {
		t.Fatalf("biz_type = %d，期望 REFUND(%d)", fl.BizType, model.FlowBizRefund)
	}
	if fl.BizNo != row.RefundNo || fl.DeltaMinor != 2000 || fl.BalanceAfterMinor != 5000 {
		t.Fatalf("退款流水与退款单不匹配：%+v", fl)
	}
	if fl.RequestId != "req-rf-1" {
		t.Fatalf("退款流水未绑定 request_id：%q", fl.RequestId)
	}
	if db.txRuns != 1 {
		t.Fatalf("四步写入应在同一个事务（txRuns=%d）", db.txRuns)
	}
	p := db.payments["PM_r1"]
	if p.LastRequestId != "req-rf-1" || p.Remark != "用户申请退款" {
		t.Fatalf("支付单未记录退款推进的留痕：last_request_id=%q remark=%q", p.LastRequestId, p.Remark)
	}
}

func TestRefundPaymentZeroAmountRefundsAllRemaining(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	paidRefundablePayment(db, "PM_r2", "order-r2", 7, 5000)
	db.seedWallet(7, 3000, "CNY")
	// 先退 1000，再用 amount=0 退「剩余可退」
	mustRefund(t, svcCtx, refundReq("PM_r2", 1000, "req-rf-2"))

	reply := mustRefund(t, svcCtx, refundReq("PM_r2", 0, "req-rf-3"))

	if reply.Refund.AmountMinor != 4000 {
		t.Fatalf("全额剩余 = %d，期望 4000", reply.Refund.AmountMinor)
	}
	if reply.Payment.State != rpc.PaymentState_PAYMENT_STATE_REFUNDED {
		t.Fatalf("退满后 state = %s，期望 REFUNDED", reply.Payment.State)
	}
	if reply.Payment.RefundedMinor != 5000 {
		t.Fatalf("refunded_minor = %d，期望 5000", reply.Payment.RefundedMinor)
	}
	requireBalance(t, db, 7, 8000) // 3000 + 1000 + 4000，累计不超过原始 5000 退款额
	requireFlows(t, db, 2)
}

func TestRefundPaymentSequentialPartialsNeverExceedOriginal(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	paidRefundablePayment(db, "PM_r3", "order-r3", 7, 3000)
	db.seedWallet(7, 0, "CNY")

	mustRefund(t, svcCtx, refundReq("PM_r3", 1200, "req-rf-4"))
	mustRefund(t, svcCtx, refundReq("PM_r3", 1200, "req-rf-5"))
	_, err := NewRefundPaymentLogic(context.Background(), svcCtx).RefundPayment(refundReq("PM_r3", 1000, "req-rf-6"))

	requireStatus(t, err, codes.FailedPrecondition, "refundable_minor=600")
	p := db.payments["PM_r3"]
	if p.State != model.PaymentStatePartiallyRefunded || p.RefundedMinor != 2400 {
		t.Fatalf("超退请求改了台账：state=%d refunded=%d", p.State, p.RefundedMinor)
	}
	requireBalance(t, db, 7, 2400)
	requireFlows(t, db, 2)
	if len(db.refunds) != 2 {
		t.Fatalf("被拒的超退留了退款单：%d 行", len(db.refunds))
	}
}

func TestRefundPaymentOverAmountRejectedWithoutTrace(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	paidRefundablePayment(db, "PM_r4", "order-r4", 7, 5000)
	db.seedWallet(7, 3000, "CNY")

	_, err := NewRefundPaymentLogic(context.Background(), svcCtx).RefundPayment(refundReq("PM_r4", 6000, "req-rf-7"))

	requireStatus(t, err, codes.FailedPrecondition, "refundable_minor=5000")
	requireBalance(t, db, 7, 3000)
	requireFlows(t, db, 0)
	if len(db.refunds) != 0 {
		t.Fatalf("被拒的退款留了单：%d 行", len(db.refunds))
	}
	if db.txRuns != 0 {
		t.Fatalf("前置校验就该拒掉，不该进事务（txRuns=%d）", db.txRuns)
	}
}

func TestRefundPaymentFullyRefundedPaymentRefuses(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedPayment(&model.Payment{PaymentNo: "PM_r5", BizOrderNo: "order-r5", RequestId: "req-r5",
		Mid: 7, AmountMinor: 5000, RefundedMinor: 5000, Currency: "CNY",
		Method: model.MethodBalance, State: model.PaymentStateRefunded})
	db.seedWallet(7, 5000, "CNY")

	_, err := NewRefundPaymentLogic(context.Background(), svcCtx).RefundPayment(refundReq("PM_r5", 0, "req-rf-8"))

	requireStatus(t, err, codes.FailedPrecondition, "payment fully refunded")
	requireBalance(t, db, 7, 5000)
	requireFlows(t, db, 0)
}

func TestRefundPaymentNeverRefundableStates(t *testing.T) {
	cases := []struct {
		state int32
		name  string
	}{
		{model.PaymentStatePending, "PAYMENT_STATE_PENDING"},
		{model.PaymentStateFailed, "PAYMENT_STATE_FAILED"},
		{model.PaymentStateClosed, "PAYMENT_STATE_CLOSED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t, defaultPaymentConf())
			db.seedPayment(&model.Payment{PaymentNo: "PM_ns", BizOrderNo: "order-ns", RequestId: "req-ns",
				Mid: 7, AmountMinor: 5000, Currency: "CNY", Method: model.MethodBalance, State: tc.state})
			db.seedWallet(7, 3000, "CNY")

			_, err := NewRefundPaymentLogic(context.Background(), svcCtx).
				RefundPayment(refundReq("PM_ns", 1000, "req-rf-9"))

			requireStatus(t, err, codes.FailedPrecondition, "payment_state="+tc.name)
			requireBalance(t, db, 7, 3000)
			requireFlows(t, db, 0)
			if len(db.refunds) != 0 {
				t.Fatalf("没收过钱的单被退了款：%d 行", len(db.refunds))
			}
		})
	}
}

// TestRefundPaymentSandboxChannelPaymentRefused 沙箱渠道支付的单从未占用余额，
// 退成余额等于凭空给台账加钱，必须拒绝。
func TestRefundPaymentSandboxChannelPaymentRefused(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedPayment(&model.Payment{PaymentNo: "PM_sc", BizOrderNo: "order-sc", RequestId: "req-sc",
		Mid: 7, AmountMinor: 5000, Currency: "CNY", Method: model.MethodSandboxChannel,
		State: model.PaymentStatePaid})
	db.seedWallet(7, 3000, "CNY")

	_, err := NewRefundPaymentLogic(context.Background(), svcCtx).RefundPayment(refundReq("PM_sc", 5000, "req-rf-10"))

	requireSentinel(t, err, model.ErrRefundRequiresBalancePayment, codes.FailedPrecondition)
	requireBalance(t, db, 7, 3000)
	requireFlows(t, db, 0)
	if len(db.refunds) != 0 {
		t.Fatal("被拒的渠道单退款留了单")
	}
}

// TestRefundPaymentToChannelIsNotConfigured 原路退回真实渠道没有配置：
// 明确拒绝，绝不返回「已退回」。
func TestRefundPaymentToChannelIsNotConfigured(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	paidRefundablePayment(db, "PM_oc", "order-oc", 7, 5000)
	db.seedWallet(7, 3000, "CNY")
	in := refundReq("PM_oc", 1000, "req-rf-11")
	in.ToBalance = false

	_, err := NewRefundPaymentLogic(context.Background(), svcCtx).RefundPayment(in)

	requireStatus(t, err, codes.FailedPrecondition, "refund to original channel not configured")
	requireBalance(t, db, 7, 3000)
	requireFlows(t, db, 0)
	if len(db.refunds) != 0 {
		t.Fatal("未配置的退款路径却落了退款单")
	}
}

// TestRefundPaymentCasMissRollsBackLedger 累计守卫在 UPDATE 的 WHERE 里：
// 0 行（并发挤占或状态被推进）必须整笔回滚，退款单和加钱都不留。
func TestRefundPaymentCasMissRollsBackLedger(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	paidRefundablePayment(db, "PM_cm", "order-cm", 7, 5000)
	db.seedWallet(7, 3000, "CNY")
	db.refundCasMiss = true

	_, err := NewRefundPaymentLogic(context.Background(), svcCtx).RefundPayment(refundReq("PM_cm", 2000, "req-rf-12"))

	requireStatus(t, err, codes.FailedPrecondition, "refundable_minor=5000")
	requireBalance(t, db, 7, 3000)
	requireFlows(t, db, 0)
	if len(db.refunds) != 0 {
		t.Fatalf("CAS 未命中却留了退款单：%d 行", len(db.refunds))
	}
	if db.payments["PM_cm"].State != model.PaymentStatePaid {
		t.Fatal("CAS 未命中却推进了支付单状态")
	}
	if db.rollbacks != 1 {
		t.Fatalf("应回滚 1 次，实际 %d", db.rollbacks)
	}
}

func TestRefundPaymentReplayByRequestID(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	paidRefundablePayment(db, "PM_rp", "order-rp", 7, 5000)
	db.seedWallet(7, 3000, "CNY")
	first := mustRefund(t, svcCtx, refundReq("PM_rp", 2000, "req-rf-13"))

	again := mustRefund(t, svcCtx, refundReq("PM_rp", 2000, "req-rf-13"))

	if !again.Duplicated {
		t.Fatal("同 request_id 重放必须 duplicated=true")
	}
	if again.Refund.RefundNo != first.Refund.RefundNo {
		t.Fatalf("重放返回了另一张退款单：%s vs %s", again.Refund.RefundNo, first.Refund.RefundNo)
	}
	requireBalance(t, db, 7, 5000) // 只入账一次
	requireFlows(t, db, 1)
	if len(db.refunds) != 1 {
		t.Fatalf("重放多出了退款单：%d 行", len(db.refunds))
	}
	if db.txRuns != 1 {
		t.Fatalf("重放不该再进事务（txRuns=%d）", db.txRuns)
	}
}

// TestRefundPaymentReplayAfterFullRefundHitsStateGate 现状口径（不是期望口径）：
// request_id 的重放回读排在「状态与可退额」门禁之后，所以首笔退款把单子退满之后，
// 同 request_id 的迟到重试拿到的是 FailedPrecondition，而不是 duplicated=true。
// 这里把实际行为钉住；若要把幂等提到门禁之前，本用例必须同步改。
func TestRefundPaymentReplayAfterFullRefundHitsStateGate(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	paidRefundablePayment(db, "PM_fr", "order-fr", 7, 1000)
	db.seedWallet(7, 0, "CNY")
	mustRefund(t, svcCtx, refundReq("PM_fr", 0, "req-rf-14"))

	_, err := NewRefundPaymentLogic(context.Background(), svcCtx).RefundPayment(refundReq("PM_fr", 0, "req-rf-14"))

	requireStatus(t, err, codes.FailedPrecondition, "payment fully refunded")
	// 钱没有被二次入账，这是这条路径唯一的好消息。
	requireBalance(t, db, 7, 1000)
	requireFlows(t, db, 1)
}

func TestRefundPaymentNotFound(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())

	_, err := NewRefundPaymentLogic(context.Background(), svcCtx).RefundPayment(refundReq("PM_ghost", 100, "req-rf-15"))

	requireSentinel(t, err, model.ErrPaymentNotFound, codes.NotFound)
	requireFlows(t, db, 0)
}

func TestRefundPaymentValidation(t *testing.T) {
	cases := []struct {
		name     string
		patch    func(*rpc.RefundPaymentReq)
		sentinel error
		msg      string
	}{
		{"缺单号", func(r *rpc.RefundPaymentReq) { r.PaymentNo = " " }, model.ErrPaymentNoRequired, ""},
		{"单号超长", func(r *rpc.RefundPaymentReq) { r.PaymentNo = strings.Repeat("p", 41) },
			nil, "payment_no too long"},
		{"负金额", func(r *rpc.RefundPaymentReq) { r.AmountMinor = -1 },
			model.ErrRefundAmountNotPositive, ""},
		{"缺幂等键", func(r *rpc.RefundPaymentReq) { r.RequestId = "" }, model.ErrRequestIDRequired, ""},
		{"缺操作人", func(r *rpc.RefundPaymentReq) { r.Operator = "" }, model.ErrOperatorRequired, ""},
		{"缺理由", func(r *rpc.RefundPaymentReq) { r.Reason = "" }, model.ErrReasonRequired, ""},
		{"理由超长", func(r *rpc.RefundPaymentReq) { r.Reason = strings.Repeat("退", 101) },
			nil, "reason too long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t, defaultPaymentConf())
			paidRefundablePayment(db, "PM_v", "order-v", 7, 5000)
			db.seedWallet(7, 3000, "CNY")
			in := refundReq("PM_v", 1000, "req-rf-16")
			tc.patch(in)

			_, err := NewRefundPaymentLogic(context.Background(), svcCtx).RefundPayment(in)

			if tc.sentinel != nil {
				requireSentinel(t, err, tc.sentinel, codes.InvalidArgument)
			} else {
				requireStatus(t, err, codes.InvalidArgument, tc.msg)
			}
			requireBalance(t, db, 7, 3000)
			requireFlows(t, db, 0)
			if len(db.refunds) != 0 {
				t.Fatal("入参不合法却落了退款单")
			}
		})
	}
}

func TestRefundPaymentFlowFailureRollsBackAllFourSteps(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	paidRefundablePayment(db, "PM_ff", "order-ff", 7, 5000)
	db.seedWallet(7, 3000, "CNY")
	db.flowErr = errors.New("pm_flow InsertTx: connection reset by peer")

	_, err := NewRefundPaymentLogic(context.Background(), svcCtx).RefundPayment(refundReq("PM_ff", 2000, "req-rf-17"))

	if err == nil || !strings.Contains(err.Error(), "pm_flow InsertTx") {
		t.Fatalf("底层报错必须上抛，实际 %v", err)
	}
	requireBalance(t, db, 7, 3000)
	requireFlows(t, db, 0)
	if len(db.refunds) != 0 {
		t.Fatalf("流水没写成却留了退款单：%d 行", len(db.refunds))
	}
	p := db.payments["PM_ff"]
	if p.RefundedMinor != 0 || p.State != model.PaymentStatePaid {
		t.Fatalf("退款单回滚了但支付单没回滚：refunded=%d state=%d", p.RefundedMinor, p.State)
	}
}

func TestRefundPaymentConcurrentDuplicateReplaysWinner(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	paidRefundablePayment(db, "PM_cd", "order-cd", 7, 5000)
	db.seedWallet(7, 3000, "CNY")
	db.refundSteal = func(db *fakeDB) {
		winner := db.seedRefund(&model.Refund{RefundNo: "RF_winner", RequestId: "req-rf-18",
			PaymentNo: "PM_cd", BizOrderNo: "order-cd", Mid: 7, AmountMinor: 2000, Currency: "CNY",
			State: model.RefundStateSucceeded, Destination: model.DestinationBalance})
		winner.Id = 666
	}

	reply := mustRefund(t, svcCtx, refundReq("PM_cd", 2000, "req-rf-18"))

	if !reply.Duplicated || reply.Refund.RefundNo != "RF_winner" {
		t.Fatalf("并发重复应收敛成重放：duplicated=%v refund_no=%s", reply.Duplicated, reply.Refund.RefundNo)
	}
	requireBalance(t, db, 7, 3000) // 本次失败尝试的加钱已回滚
	requireFlows(t, db, 0)
}

func TestRefundPaymentDuplicateWithoutRowIsReuse(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	paidRefundablePayment(db, "PM_dr", "order-dr", 7, 5000)
	db.seedWallet(7, 3000, "CNY")
	db.refundDup = true

	_, err := NewRefundPaymentLogic(context.Background(), svcCtx).RefundPayment(refundReq("PM_dr", 1000, "req-rf-19"))

	requireSentinel(t, err, model.ErrRequestIDReused, codes.AlreadyExists)
	requireBalance(t, db, 7, 3000)
	requireFlows(t, db, 0)
	if len(db.refunds) != 0 {
		t.Fatalf("回滚后还留着退款单：%d 行", len(db.refunds))
	}
}

func TestRefundPaymentDocumentNoFailureWritesNothing(t *testing.T) {
	withFailingIDGen(t)
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	paidRefundablePayment(db, "PM_dn", "order-dn", 7, 5000)
	db.seedWallet(7, 3000, "CNY")

	_, err := NewRefundPaymentLogic(context.Background(), svcCtx).RefundPayment(refundReq("PM_dn", 1000, "req-rf-20"))

	requireSentinel(t, err, model.ErrDocumentNoUnavailable, codes.Internal)
	requireBalance(t, db, 7, 3000)
	requireFlows(t, db, 0)
	if len(db.refunds) != 0 || db.txRuns != 0 {
		t.Fatalf("拿不到单据号却写了台账：refunds=%d txRuns=%d", len(db.refunds), db.txRuns)
	}
}

// --- GetWallet ---

func TestGetWalletMissingAccountReadsZeroWithoutCreatingRow(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())

	reply, err := NewGetWalletLogic(context.Background(), svcCtx).GetWallet(&rpc.GetWalletReq{Mid: 7})
	if err != nil {
		t.Fatalf("GetWallet: %v", err)
	}
	if reply.Wallet.BalanceMinor != 0 || reply.Wallet.Currency != "CNY" {
		t.Fatalf("无账户应按 0 余额语义返回：%+v", reply.Wallet)
	}
	if _, ok := db.wallets[7]; ok {
		t.Fatal("读路径建了账户行 —— 建行只允许发生在要动钱的写入里")
	}
	requireFlows(t, db, 0)
}

func TestGetWalletReadsLedgerRowAsIs(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	w := db.seedWallet(7, 1234, "CNY")
	w.Version = 9

	reply, err := NewGetWalletLogic(context.Background(), svcCtx).GetWallet(&rpc.GetWalletReq{Mid: 7, Currency: " cny "})
	if err != nil {
		t.Fatalf("GetWallet: %v", err)
	}
	got := reply.Wallet
	if got.BalanceMinor != 1234 || got.Version != 9 || got.FrozenMinor != 0 {
		t.Fatalf("投影与台账不一致：%+v", got)
	}
	if got.Mid != 7 || got.Currency != "CNY" {
		t.Fatalf("投影标识缺失：%+v", got)
	}
}

func TestGetWalletValidation(t *testing.T) {
	svcCtx, _ := newTestSvc(t, defaultPaymentConf())
	logic := NewGetWalletLogic(context.Background(), svcCtx)

	_, err := logic.GetWallet(&rpc.GetWalletReq{Mid: 0})
	requireSentinel(t, err, model.ErrInvalidMid, codes.InvalidArgument)
	_, err = logic.GetWallet(&rpc.GetWalletReq{Mid: 7, Currency: "USD"})
	requireSentinel(t, err, model.ErrUnsupportedCurrency, codes.InvalidArgument)
}

// --- 台账自洽性：充值 → 消费 → 退款 → 调整，流水合计必须等于余额 ---

func TestLedgerChainKeepsFlowsConsistentWithBalance(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	ctx := context.Background()

	opened, err := NewOpenRechargeLogic(ctx, svcCtx).OpenRecharge(openReq(7, 5000, "req-chain-open"))
	if err != nil {
		t.Fatalf("OpenRecharge: %v", err)
	}
	if _, err := NewSettleSandboxRechargeLogic(ctx, svcCtx).
		SettleSandboxRecharge(settleReq(opened.Recharge.RechargeNo, "req-chain-settle")); err != nil {
		t.Fatalf("SettleSandboxRecharge: %v", err)
	}
	paid := mustCreatePayment(t, svcCtx, payReq("order-chain", "req-chain-pay", 7, 2000))
	if _, err := NewRefundPaymentLogic(ctx, svcCtx).RefundPayment(refundReq(paid.Payment.PaymentNo, 500, "req-chain-refund")); err != nil {
		t.Fatalf("RefundPayment: %v", err)
	}
	mustAdjust(t, svcCtx, adjustReq(7, -300, "req-chain-adjust"))

	requireBalance(t, db, 7, 3200)
	requireFlows(t, db, 4)

	var sum int64
	seen := map[int32]bool{}
	for i, fl := range db.flows {
		sum += fl.DeltaMinor
		seen[fl.BizType] = true
		if fl.BalanceAfterMinor != sum {
			t.Fatalf("第 %d 条流水的 balance_after=%d 与累计合计 %d 不吻合：%+v",
				i+1, fl.BalanceAfterMinor, sum, fl)
		}
	}
	if sum != 3200 {
		t.Fatalf("流水合计 = %d，余额 = 3200", sum)
	}
	for _, biz := range []int32{model.FlowBizRecharge, model.FlowBizPayment, model.FlowBizRefund, model.FlowBizAdminAdjust} {
		if !seen[biz] {
			t.Fatalf("四类业务动作应各留一条流水，缺 biz_type=%d", biz)
		}
	}
	// 单据侧同样要闭环：支付单已退 500、状态 PARTIALLY_REFUNDED。
	p := db.payments[paid.Payment.PaymentNo]
	if p.RefundedMinor != 500 || p.State != model.PaymentStatePartiallyRefunded {
		t.Fatalf("支付单退款进度不对：refunded=%d state=%d", p.RefundedMinor, p.State)
	}
}

// TestLedgerWritesNeverUseRealChannelConfig 全链路上没有任何「真实渠道」开关：
// 沙箱之外的渠道名一律不被放行，配置写错也不会接通真实资金。
func TestLedgerWritesNeverUseRealChannelConfig(t *testing.T) {
	cfg := defaultPaymentConf()
	cfg.AllowedChannels = []string{"SANDBOX", "ALIPAY", "WECHAT_PAY", "UNSPECIFIED"}
	svcCtx, db := newTestSvc(t, cfg)
	db.seedWallet(7, 1000, "CNY")

	// 即便白名单被写脏，入账/消费也只认 SANDBOX/BALANCE 两条沙箱路径。
	opened, err := NewOpenRechargeLogic(context.Background(), svcCtx).OpenRecharge(openReq(7, 1000, "req-gate-open"))
	if err != nil {
		t.Fatalf("OpenRecharge: %v", err)
	}
	if _, err := NewSettleSandboxRechargeLogic(context.Background(), svcCtx).
		SettleSandboxRecharge(settleReq(opened.Recharge.RechargeNo, "req-gate-settle")); err != nil {
		t.Fatalf("SettleSandboxRecharge: %v", err)
	}
	requireBalance(t, db, 7, 2000)

	_, err = NewCreatePaymentLogic(context.Background(), svcCtx).
		CreatePayment(payReq("order-gate", "req-gate-pay", 7, 100))
	if err != nil {
		t.Fatalf("BALANCE 消费不该受渠道脏配置影响: %v", err)
	}
	requireBalance(t, db, 7, 1900)
	requireFlows(t, db, 2)

	for _, name := range []string{"ALIPAY", "WECHAT_PAY", "UNSPECIFIED", ""} {
		if (config.PaymentConf{}).AllowsChannel(name) {
			t.Fatalf("缺省配置放行了 %q", name)
		}
	}
}
