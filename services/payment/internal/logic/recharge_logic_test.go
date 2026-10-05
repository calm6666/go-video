package logic

// 充值链路的三条写路径：OpenRecharge（开单）→ SettleSandboxRecharge（入账）/ CancelRecharge（作废）。
//
// 本文件要证明的口径（各 logic 头部「口径」注释在这里落成断言）：
//  1. 开单只落 PENDING，绝不动余额、绝不写流水 —— 入账只有 SettleSandboxRecharge 一个入口；
//  2. 结算是「钱到账」的唯一路径：改单据 + 加余额 + 写流水必须在同一个事务里，
//     任何一步失败整体回滚，不允许出现「已入账无流水」或「已加余额无单据」；
//  3. 重复受理按 request_id / 单据号幂等：重放返回首单并置 duplicated=true，台账不二次变动；
//  4. 状态只能从合法前态推进（CAS 0 行即并发冲突），已入账的单不能靠取消抹掉。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/payment/internal/config"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"

	"google.golang.org/grpc/codes"
)

func openReq(mid, amount int64, requestID string) *rpc.OpenRechargeReq {
	return &rpc.OpenRechargeReq{
		Mid: mid, AmountMinor: amount, Channel: rpc.PayChannel_PAY_CHANNEL_SANDBOX,
		RequestId: requestID, ClientTraceId: "trace-1",
	}
}

// mustFail 执行并返回错误；无错误即视为用例失败（禁止假成功）。
func mustFail(t *testing.T, fn func() error) error {
	t.Helper()
	err := fn()
	if err == nil {
		t.Fatal("期望返回错误，实际返回成功")
	}
	return err
}

// --- OpenRecharge ---

func TestOpenRechargeCreatesPendingOnly(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 5000, "CNY")

	reply, err := NewOpenRechargeLogic(context.Background(), svcCtx).OpenRecharge(openReq(7, 20000, "req-open-1"))
	if err != nil {
		t.Fatalf("OpenRecharge 失败: %v", err)
	}
	if reply.Duplicated {
		t.Fatal("首建不得置 duplicated")
	}
	rc := reply.Recharge
	if !strings.HasPrefix(rc.RechargeNo, "RC_") {
		t.Errorf("充值单号 %q 应以 RC_ 前缀开头", rc.RechargeNo)
	}
	if rc.State != rpc.RechargeState_RECHARGE_STATE_PENDING {
		t.Errorf("开单状态 = %s，期望 PENDING（开单不代表到账）", rc.State)
	}
	if rc.Channel != rpc.PayChannel_PAY_CHANNEL_SANDBOX || rc.Currency != "CNY" ||
		rc.Mid != 7 || rc.AmountMinor != 20000 || rc.RequestId != "req-open-1" {
		t.Errorf("单据字段不符: %+v", rc)
	}
	if rc.Ctime == 0 || rc.Mtime != rc.Ctime {
		t.Errorf("台账时间列未回填: %+v", rc)
	}
	// 关键：这一步不动余额、不写流水、不开事务。
	requireBalance(t, db, 7, 5000)
	requireFlows(t, db, 0)
	if len(db.wallets) != 1 {
		t.Errorf("开单不得新建或改动账户行，现有 %d 只账户", len(db.wallets))
	}
	if db.txRuns != 0 {
		t.Errorf("开单是单表写入，不该开事务，实际开了 %d 次", db.txRuns)
	}
	stored := db.recharges[rc.RechargeNo]
	if stored == nil || stored.State != model.RechargeStatePending || stored.ClientTraceId != "trace-1" {
		t.Fatalf("库内行与响应不符: %+v", stored)
	}
}

func TestOpenRechargeAmountBounds(t *testing.T) {
	cfg := defaultPaymentConf() // [1, 200000]
	for _, tc := range []struct {
		name   string
		amount int64
		ok     bool
	}{
		{"下限内", 1, true},
		{"上限内", 200000, true},
		{"零元", 0, false},
		{"负数", -100, false},
		{"超上限", 200001, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t, cfg)
			_, err := NewOpenRechargeLogic(context.Background(), svcCtx).OpenRecharge(openReq(7, tc.amount, "req-amt"))
			if !tc.ok {
				requireSentinel(t, err, model.ErrRechargeAmountOutOfRange, codes.InvalidArgument)
				if len(db.recharges) != 0 {
					t.Fatalf("金额非法却建了单：%d 行", len(db.recharges))
				}
				return
			}
			if err != nil {
				t.Fatalf("金额 %d 应受理: %v", tc.amount, err)
			}
		})
	}
}

func TestOpenRechargeValidatesIdentityAndChannel(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	logic := NewOpenRechargeLogic(context.Background(), svcCtx)

	t.Run("mid", func(t *testing.T) {
		for _, mid := range []int64{0, -7} {
			req := openReq(mid, 100, "req-mid")
			requireSentinel(t, mustFail(t, func() error { _, e := logic.OpenRecharge(req); return e }),
				model.ErrInvalidMid, codes.InvalidArgument)
		}
	})
	t.Run("币种", func(t *testing.T) {
		req := openReq(7, 100, "req-cur")
		req.Currency = "USD"
		requireSentinel(t, mustFail(t, func() error { _, e := logic.OpenRecharge(req); return e }),
			model.ErrUnsupportedCurrency, codes.InvalidArgument)
	})
	t.Run("幂等键", func(t *testing.T) {
		req := openReq(7, 100, "   ")
		requireSentinel(t, mustFail(t, func() error { _, e := logic.OpenRecharge(req); return e }),
			model.ErrRequestIDRequired, codes.InvalidArgument)
	})
	t.Run("trace_id 超长", func(t *testing.T) {
		req := openReq(7, 100, "req-trace")
		req.ClientTraceId = strings.Repeat("t", 65)
		requireStatus(t, mustFail(t, func() error { _, e := logic.OpenRecharge(req); return e }),
			codes.InvalidArgument, "client_trace_id too long")
	})
	if len(db.recharges) != 0 {
		t.Fatalf("入参非法阶段不得落任何单据，已落 %d 行", len(db.recharges))
	}
}

// TestOpenRechargeRejectsNonSandboxChannel 钉住「看不见就当沙箱处理」这条禁令：
// 任何非 SANDBOX 渠道（含 UNSPECIFIED 与越界值）都是非法入参，而不是被悄悄改写。
func TestOpenRechargeRejectsNonSandboxChannel(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	logic := NewOpenRechargeLogic(context.Background(), svcCtx)
	for _, ch := range []rpc.PayChannel{
		rpc.PayChannel_PAY_CHANNEL_UNSPECIFIED,
		rpc.PayChannel(9), // 未来有人加档也不许被默认受理
	} {
		req := openReq(7, 100, "req-ch")
		req.Channel = ch
		err := mustFail(t, func() error { _, e := logic.OpenRecharge(req); return e })
		requireStatus(t, err, codes.InvalidArgument, "unsupported pay channel")
		if !strings.Contains(err.Error(), "channel="+ch.String()) {
			t.Errorf("错误里应回显被拒的渠道，实际 %v", err)
		}
	}
	if len(db.recharges) != 0 {
		t.Fatalf("非沙箱渠道不得建单，已建 %d 行", len(db.recharges))
	}
}

// TestOpenRechargeSandboxDisabledIsNotConfigured 渠道门禁：配置关掉沙箱时，
// 结论必须是 FailedPrecondition「not configured」，而不是「建单成功但永远不结算」。
func TestOpenRechargeSandboxDisabledIsNotConfigured(t *testing.T) {
	cfg := defaultPaymentConf()
	cfg.AllowedChannels = []string{"ALIPAY"} // 运营把沙箱换成了没接入的渠道
	svcCtx, db := newTestSvc(t, cfg)
	_, err := NewOpenRechargeLogic(context.Background(), svcCtx).OpenRecharge(openReq(7, 100, "req-off"))
	requireSentinel(t, err, model.ErrChannelNotConfigured, codes.FailedPrecondition)
	if len(db.recharges) != 0 {
		t.Fatalf("渠道被关掉却建了单：%d 行", len(db.recharges))
	}
}

func TestOpenRechargeIdempotentByRequestID(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	logic := NewOpenRechargeLogic(context.Background(), svcCtx)

	first, err := logic.OpenRecharge(openReq(7, 5000, "req-replay"))
	if err != nil {
		t.Fatalf("首建失败: %v", err)
	}
	second, err := logic.OpenRecharge(openReq(7, 5000, "req-replay"))
	if err != nil {
		t.Fatalf("重放失败: %v", err)
	}
	if !second.Duplicated {
		t.Error("同一 request_id 重放必须置 duplicated=true")
	}
	if second.Recharge.RechargeNo != first.Recharge.RechargeNo {
		t.Errorf("重放回到另一张单：%q != %q", second.Recharge.RechargeNo, first.Recharge.RechargeNo)
	}
	if len(db.recharges) != 1 {
		t.Fatalf("重放产生了第二张充值单：%d 行", len(db.recharges))
	}
	// 换个金额复用同一 request_id：仍回到首单，绝不按新金额建单。
	third, err := logic.OpenRecharge(openReq(7, 9000, "req-replay"))
	if err != nil {
		t.Fatalf("复用 request_id 的第二次调用失败: %v", err)
	}
	if third.Recharge.AmountMinor != 5000 || !third.Duplicated {
		t.Errorf("request_id 命中后必须回到首单，实际 %+v", third.Recharge)
	}
}

// TestOpenRechargeConcurrentInsertFallsBackToReplay 并发下 uniq_request_id 兜住重复建单：
// 插入报唯一键冲突后必须回查并按重放返回，而不是把 1062 抛给调用方。
func TestOpenRechargeConcurrentInsertFallsBackToReplay(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedRecharge(&model.Recharge{RechargeNo: "RC_existing", RequestId: "req-race", Mid: 7,
		AmountMinor: 3000, Currency: "CNY", Channel: model.ChannelSandbox, State: model.RechargeStatePending})
	db.rechargeDup = true

	reply, err := NewOpenRechargeLogic(context.Background(), svcCtx).OpenRecharge(openReq(7, 3000, "req-race"))
	if err != nil {
		t.Fatalf("并发重复应按重放返回，实际报错: %v", err)
	}
	if !reply.Duplicated || reply.Recharge.RechargeNo != "RC_existing" {
		t.Fatalf("重放结论不符: duplicated=%v recharge=%+v", reply.Duplicated, reply.Recharge)
	}
	if len(db.recharges) != 1 {
		t.Fatalf("并发下多出了单据：%d 行", len(db.recharges))
	}
}

// TestOpenRechargeDuplicateWithoutRowSurfacesError 回查不到就别装成功：
// 唯一键冲突又查不到首单时，宁可上抛底层错误（未包装成 status，code 即 Unknown），
// 也不能返回一张凭空的单据。
func TestOpenRechargeDuplicateWithoutRowSurfacesError(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.rechargeDup = true
	_, err := NewOpenRechargeLogic(context.Background(), svcCtx).OpenRecharge(openReq(7, 3000, "req-ghost"))
	if err == nil {
		t.Fatal("唯一键冲突且回查无单据时必须报错")
	}
	requireStatus(t, err, codes.Unknown, "Duplicate entry")
	if len(db.recharges) != 0 {
		t.Fatalf("失败路径落了单据：%d 行", len(db.recharges))
	}
}

func TestOpenRechargeDocumentNoFailureAborts(t *testing.T) {
	withFailingIDGen(t)
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	_, err := NewOpenRechargeLogic(context.Background(), svcCtx).OpenRecharge(openReq(7, 100, "req-noid"))
	requireSentinel(t, err, model.ErrDocumentNoUnavailable, codes.Internal)
	if len(db.recharges) != 0 {
		t.Fatalf("拿不到单据号却建了单：%d 行", len(db.recharges))
	}
}

// --- SettleSandboxRecharge ---

// settleReq 结算请求（reason 用中文，顺带覆盖 rune 口径）。
func settleReq(rechargeNo, requestID string) *rpc.SettleSandboxRechargeReq {
	return &rpc.SettleSandboxRechargeReq{
		RechargeNo: rechargeNo, Operator: "运营工号 A01", RequestId: requestID,
		Reason: "沙箱自动结算：渠道回调未配置",
	}
}

func TestSettleSandboxRechargeCreditsOnceInOneTransaction(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	ctx := context.Background()
	opened, err := NewOpenRechargeLogic(ctx, svcCtx).OpenRecharge(openReq(7, 20000, "req-open-1"))
	if err != nil {
		t.Fatalf("开单失败: %v", err)
	}
	no := opened.Recharge.RechargeNo

	reply, err := NewSettleSandboxRechargeLogic(ctx, svcCtx).SettleSandboxRecharge(settleReq(no, "req-settle-1"))
	if err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if reply.Duplicated {
		t.Error("首次结算不得置 duplicated")
	}
	if reply.Recharge.State != rpc.RechargeState_RECHARGE_STATE_SUCCESS {
		t.Errorf("结算后状态 = %s", reply.Recharge.State)
	}
	if reply.Recharge.SettledAt == 0 {
		t.Error("SUCCESS 单的 settled_at 必须非 0，否则台账自相矛盾")
	}
	if reply.Recharge.Operator != "运营工号 A01" || reply.Recharge.Reason == "" {
		t.Errorf("结算主体与理由必须回写台账: %+v", reply.Recharge)
	}
	requireBalance(t, db, 7, 20000)
	if reply.Wallet.BalanceMinor != 20000 || reply.Wallet.FrozenMinor != 0 || reply.Wallet.Version != 1 {
		t.Errorf("余额快照不符: %+v", reply.Wallet)
	}
	if reply.FlowId == 0 {
		t.Error("结算必须回带流水号")
	}
	requireFlows(t, db, 1)
	fl := db.flows[0]
	if fl.BizType != model.FlowBizRecharge || fl.BizNo != no || fl.DeltaMinor != 20000 ||
		fl.BalanceAfterMinor != 20000 || fl.RequestId != "req-settle-1" || fl.Operator != "运营工号 A01" {
		t.Errorf("入账流水不符: %+v", fl)
	}
	// 「改单据 + 加余额 + 写流水」只允许一个事务。
	if db.txRuns != 1 || db.rollbacks != 0 {
		t.Errorf("事务次数 = %d、回滚 = %d，期望 (1, 0)", db.txRuns, db.rollbacks)
	}
}

// TestSettleSandboxRechargeReplayNoDoubleCredit 重复受理（换了 request_id 也要幂等）：
// 已 SUCCESS 的单只能按重放返回，余额与流水都不许二次变动。
func TestSettleSandboxRechargeReplayNoDoubleCredit(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	ctx := context.Background()
	opened, err := NewOpenRechargeLogic(ctx, svcCtx).OpenRecharge(openReq(7, 15000, "req-o"))
	if err != nil {
		t.Fatalf("开单失败: %v", err)
	}
	no := opened.Recharge.RechargeNo
	first, err := NewSettleSandboxRechargeLogic(ctx, svcCtx).SettleSandboxRecharge(settleReq(no, "req-s1"))
	if err != nil {
		t.Fatalf("首次结算失败: %v", err)
	}

	second, err := NewSettleSandboxRechargeLogic(ctx, svcCtx).SettleSandboxRecharge(settleReq(no, "req-s2"))
	if err != nil {
		t.Fatalf("重放结算失败: %v", err)
	}
	if !second.Duplicated {
		t.Error("已入账单据重复结算必须置 duplicated=true")
	}
	if second.FlowId != first.FlowId {
		t.Errorf("重放回了另一条流水：%d vs %d", second.FlowId, first.FlowId)
	}
	if second.Recharge.RechargeNo != no || second.Recharge.AmountMinor != 15000 {
		t.Errorf("重放必须返回原单据: %+v", second.Recharge)
	}
	requireBalance(t, db, 7, 15000)
	requireFlows(t, db, 1)
	if db.txRuns != 1 || db.rollbacks != 0 {
		t.Errorf("重放走状态判定即可，不得再进事务（txRuns=%d rollbacks=%d）", db.txRuns, db.rollbacks)
	}
}

func TestSettleSandboxRechargeRejectsIllegalPriorState(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		state int32
	}{
		{"已取消", model.RechargeStateCancelled},
		{"已失败", model.RechargeStateFailed},
		{"未指定", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t, defaultPaymentConf())
			db.seedRecharge(&model.Recharge{RechargeNo: "RC_x", RequestId: "req-x", Mid: 7,
				AmountMinor: 1000, Currency: "CNY", Channel: model.ChannelSandbox, State: tc.state})
			db.seedWallet(7, 0, "CNY")

			_, err := NewSettleSandboxRechargeLogic(ctx, svcCtx).SettleSandboxRecharge(settleReq("RC_x", "req-s"))
			requireStatus(t, err, codes.FailedPrecondition, "payment: invalid state transition")
			if !strings.Contains(err.Error(), "recharge_state=") {
				t.Errorf("错误应说明当前状态，实际 %v", err)
			}
			requireBalance(t, db, 7, 0)
			requireFlows(t, db, 0)
			if db.recharges["RC_x"].State != tc.state {
				t.Errorf("非法前态被改写成 %d", db.recharges["RC_x"].State)
			}
			if db.txRuns != 0 {
				t.Errorf("前态非法时不该进入事务（txRuns=%d）", db.txRuns)
			}
		})
	}
}

func TestSettleSandboxRechargeNotFound(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	_, err := NewSettleSandboxRechargeLogic(context.Background(), svcCtx).
		SettleSandboxRecharge(settleReq("RC_missing", "req-s"))
	requireSentinel(t, err, model.ErrRechargeNotFound, codes.NotFound)
	requireFlows(t, db, 0)
}

func TestSettleSandboxRechargeValidates(t *testing.T) {
	ctx := context.Background()
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedRecharge(&model.Recharge{RechargeNo: "RC_v", RequestId: "req-v", Mid: 7,
		AmountMinor: 1000, Currency: "CNY", Channel: model.ChannelSandbox, State: model.RechargeStatePending})
	logic := NewSettleSandboxRechargeLogic(ctx, svcCtx)

	blank := settleReq("  ", "req-1")
	requireSentinel(t, mustFail(t, func() error { _, e := logic.SettleSandboxRecharge(blank); return e }),
		model.ErrRechargeNoRequired, codes.InvalidArgument)

	noReq := settleReq("RC_v", "")
	requireSentinel(t, mustFail(t, func() error { _, e := logic.SettleSandboxRecharge(noReq); return e }),
		model.ErrRequestIDRequired, codes.InvalidArgument)

	noOp := settleReq("RC_v", "req-2")
	noOp.Operator = " "
	requireSentinel(t, mustFail(t, func() error { _, e := logic.SettleSandboxRecharge(noOp); return e }),
		model.ErrOperatorRequired, codes.InvalidArgument)

	long := settleReq("RC_v", "req-3")
	long.Reason = strings.Repeat("结", 101)
	requireStatus(t, mustFail(t, func() error { _, e := logic.SettleSandboxRecharge(long); return e }),
		codes.InvalidArgument, "reason too long")

	requireBalance(t, db, 7, 0)
	requireFlows(t, db, 0)
	if db.recharges["RC_v"].State != model.RechargeStatePending {
		t.Error("校验失败的调用推进了单据")
	}
}

func TestSettleSandboxRechargeSandboxDisabled(t *testing.T) {
	cfg := defaultPaymentConf()
	cfg.AllowedChannels = []string{"ALIPAY"}
	svcCtx, db := newTestSvc(t, cfg)
	db.seedRecharge(&model.Recharge{RechargeNo: "RC_off", RequestId: "req-off", Mid: 7,
		AmountMinor: 1000, Currency: "CNY", Channel: model.ChannelSandbox, State: model.RechargeStatePending})

	_, err := NewSettleSandboxRechargeLogic(context.Background(), svcCtx).
		SettleSandboxRecharge(settleReq("RC_off", "req-s"))
	requireSentinel(t, err, model.ErrChannelNotConfigured, codes.FailedPrecondition)
	requireFlows(t, db, 0)
	requireBalance(t, db, 7, 0)
	if db.recharges["RC_off"].State != model.RechargeStatePending {
		t.Errorf("沙箱被关掉却推进了状态: %d", db.recharges["RC_off"].State)
	}
}

// TestSettleSandboxRechargeFlowFailureRollsBackCredit 流水写不进去时，
// 已加的余额与已推进的状态必须一起回滚 —— 这是「不允许已入账无流水」的可执行版本。
func TestSettleSandboxRechargeFlowFailureRollsBackCredit(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedRecharge(&model.Recharge{RechargeNo: "RC_ef", RequestId: "req-ef", Mid: 7,
		AmountMinor: 7000, Currency: "CNY", Channel: model.ChannelSandbox, State: model.RechargeStatePending})
	db.seedWallet(7, 0, "CNY")
	db.flowErr = errors.New("pm_flow InsertTx: dial tcp 10.0.0.9:3306: connectex failed")

	_, err := NewSettleSandboxRechargeLogic(context.Background(), svcCtx).
		SettleSandboxRecharge(settleReq("RC_ef", "req-s"))
	if err == nil || !strings.Contains(err.Error(), "pm_flow InsertTx") {
		t.Fatalf("底层写流水失败必须上抛，实际 %v", err)
	}
	requireBalance(t, db, 7, 0)
	requireFlows(t, db, 0)
	if db.recharges["RC_ef"].State != model.RechargeStatePending {
		t.Error("事务回滚后单据仍是 SUCCESS —— 状态推进没跟着回滚")
	}
	if db.rollbacks != 1 {
		t.Errorf("应回滚 1 次，实际 %d", db.rollbacks)
	}
}

// TestSettleSandboxRechargeRequestIDReuseRejected 同一 request_id 用在别的资金动作上：
// 流水唯一键撞了，整笔回滚并明确要求换号，不能把别人的入账当自己的重放。
func TestSettleSandboxRechargeRequestIDReuseRejected(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedRecharge(&model.Recharge{RechargeNo: "RC_reuse", RequestId: "req-a", Mid: 7,
		AmountMinor: 4000, Currency: "CNY", Channel: model.ChannelSandbox, State: model.RechargeStatePending})
	db.seedWallet(7, 0, "CNY")
	db.seedFlow(&model.Flow{Mid: 7, BizType: model.FlowBizPayment, BizNo: "PM_other",
		DeltaMinor: -100, BalanceAfterMinor: 100, Currency: "CNY", RequestId: "shared-request"})
	db.flowDup = true

	_, err := NewSettleSandboxRechargeLogic(context.Background(), svcCtx).
		SettleSandboxRecharge(settleReq("RC_reuse", "shared-request"))
	requireSentinel(t, err, model.ErrRequestIDReused, codes.AlreadyExists)
	if !strings.Contains(err.Error(), "nothing was charged or credited") {
		t.Errorf("错误必须说明台账未变动，实际 %v", err)
	}
	requireBalance(t, db, 7, 0)
	requireFlows(t, db, 1) // 只有那条既有的 PAYMENT 流水
	if db.recharges["RC_reuse"].State != model.RechargeStatePending {
		t.Error("request_id 冲突回滚后单据被推进")
	}
}

// TestSettleSandboxRechargeCasMissRollsBackEverything CAS 0 行（并发对手把单据推进了）
// 必须整体回滚：余额不加、流水不留，并按 Aborted 让调用方安全重试。
func TestSettleSandboxRechargeCasMissRollsBackEverything(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedRecharge(&model.Recharge{RechargeNo: "RC_cas", RequestId: "req-cas", Mid: 7,
		AmountMinor: 8000, Currency: "CNY", Channel: model.ChannelSandbox, State: model.RechargeStatePending})
	db.settleMiss = true

	_, err := NewSettleSandboxRechargeLogic(context.Background(), svcCtx).
		SettleSandboxRecharge(settleReq("RC_cas", "req-s"))
	requireSentinel(t, err, model.ErrConcurrentUpdate, codes.Aborted)
	requireFlows(t, db, 0)
	if len(db.wallets) != 0 {
		t.Fatalf("CAS 未命中却动/建了账户：%+v", db.wallets)
	}
	if db.recharges["RC_cas"].State != model.RechargeStatePending {
		t.Error("CAS 未命中不得改单据状态")
	}
	if db.rollbacks != 1 {
		t.Errorf("事务应回滚 1 次，实际 %d 次", db.rollbacks)
	}
}

// TestSettleSandboxRechargeLoserReplaysWinner 并发对手先结算并提交：
// 本事务 CAS 未命中回滚，回读看到 SUCCESS，必须按重放返回对手的结果，
// 且余额只加一次、流水只有一条（本方那条已随事务消失）。
func TestSettleSandboxRechargeLoserReplaysWinner(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	const amount = int64(8000)
	db.seedRecharge(&model.Recharge{RechargeNo: "RC_race", RequestId: "req-race", Mid: 7,
		AmountMinor: amount, Currency: "CNY", Channel: model.ChannelSandbox, State: model.RechargeStatePending})
	db.seedWallet(7, 0, "CNY")
	db.settleSteal = func(d *fakeDB) {
		now := fakeNow()
		r := d.recharges["RC_race"]
		r.State, r.SettledAt, r.Operator, r.Reason, r.Mtime =
			model.RechargeStateSuccess, now, "cron", "并发先结算", now
		d.wallets[7].BalanceMinor += amount
		d.wallets[7].Version++
		d.seedFlow(&model.Flow{Mid: 7, BizType: model.FlowBizRecharge, BizNo: "RC_race",
			DeltaMinor: amount, BalanceAfterMinor: d.wallets[7].BalanceMinor, Currency: "CNY",
			Operator: "cron", RequestId: "req-other-instance"})
	}

	reply, err := NewSettleSandboxRechargeLogic(context.Background(), svcCtx).
		SettleSandboxRecharge(settleReq("RC_race", "req-mine"))
	if err != nil {
		t.Fatalf("败者应按胜者结果重放，实际报错: %v", err)
	}
	if !reply.Duplicated {
		t.Error("并发重放必须置 duplicated=true")
	}
	if reply.Wallet.BalanceMinor != amount || reply.Wallet.Version != 1 {
		t.Errorf("余额被二次入账或版本不符: %+v", reply.Wallet)
	}
	requireBalance(t, db, 7, amount)
	requireFlows(t, db, 1)
	if db.flows[0].RequestId != "req-other-instance" {
		t.Errorf("本事务的流水没有被回滚，留下的是: %+v", db.flows[0])
	}
	if db.rollbacks != 1 {
		t.Errorf("本事务应回滚 1 次，实际 %d", db.rollbacks)
	}
}

// --- CancelRecharge ---

func cancelReq(rechargeNo, requestID string) *rpc.CancelRechargeReq {
	return &rpc.CancelRechargeReq{RechargeNo: rechargeNo, Operator: "运营工号 B02",
		RequestId: requestID, Reason: "用户主动作废"}
}

func TestCancelRechargePendingTouchesNoMoney(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedRecharge(&model.Recharge{RechargeNo: "RC_c", RequestId: "req-c", Mid: 7,
		AmountMinor: 5000, Currency: "CNY", Channel: model.ChannelSandbox, State: model.RechargeStatePending})

	reply, err := NewCancelRechargeLogic(context.Background(), svcCtx).CancelRecharge(cancelReq("RC_c", "req-cancel"))
	if err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	if reply.Duplicated {
		t.Error("首次取消不得置 duplicated")
	}
	if reply.Recharge.State != rpc.RechargeState_RECHARGE_STATE_CANCELLED {
		t.Errorf("状态 = %s，期望 CANCELLED", reply.Recharge.State)
	}
	if reply.Recharge.Operator != "运营工号 B02" || reply.Recharge.Reason != "用户主动作废" {
		t.Errorf("作废的经办人与理由必须留痕: %+v", reply.Recharge)
	}
	// 钱从未进账：取消不得动余额、不得写流水。
	requireFlows(t, db, 0)
	if len(db.wallets) != 0 {
		t.Errorf("取消却建/动了账户：%+v", db.wallets)
	}
}

func TestCancelRechargeIsNaturallyIdempotent(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedRecharge(&model.Recharge{RechargeNo: "RC_c2", RequestId: "req-c2", Mid: 7,
		AmountMinor: 5000, Currency: "CNY", Channel: model.ChannelSandbox, State: model.RechargeStateCancelled,
		Operator: "运营工号 B02", Reason: "用户主动作废"})

	reply, err := NewCancelRechargeLogic(context.Background(), svcCtx).CancelRecharge(cancelReq("RC_c2", "req-again"))
	if err != nil {
		t.Fatalf("重复取消应成功: %v", err)
	}
	if !reply.Duplicated || reply.Recharge.RechargeNo != "RC_c2" {
		t.Fatalf("重复取消结论不符: %+v", reply)
	}
	requireFlows(t, db, 0)
}

// TestCancelRechargeRefusesToRollBackSettledMoney 已入账的单不能靠取消抹掉，
// 错误消息还要指出正确的反向路径（退款 / 运营调整）。
func TestCancelRechargeRefusesToRollBackSettledMoney(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedRecharge(&model.Recharge{RechargeNo: "RC_paid", RequestId: "req-paid", Mid: 7,
		AmountMinor: 5000, Currency: "CNY", Channel: model.ChannelSandbox,
		State: model.RechargeStateSuccess, SettledAt: fakeNow()})
	db.seedWallet(7, 5000, "CNY")

	_, err := NewCancelRechargeLogic(context.Background(), svcCtx).CancelRecharge(cancelReq("RC_paid", "req-x"))
	requireSentinel(t, err, model.ErrRechargeAlreadySettled, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "refund") || !strings.Contains(err.Error(), "AdjustBalance") {
		t.Errorf("错误应说明改走退款/调整，实际 %v", err)
	}
	requireBalance(t, db, 7, 5000)
	requireFlows(t, db, 0)
	if db.recharges["RC_paid"].State != model.RechargeStateSuccess {
		t.Error("已入账单据被取消改态")
	}
}

func TestCancelRechargeIllegalPriorStateAndNotFound(t *testing.T) {
	ctx := context.Background()
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedRecharge(&model.Recharge{RechargeNo: "RC_failed", RequestId: "req-f", Mid: 7,
		AmountMinor: 5000, Currency: "CNY", Channel: model.ChannelSandbox, State: model.RechargeStateFailed})
	_, err := NewCancelRechargeLogic(ctx, svcCtx).CancelRecharge(cancelReq("RC_failed", "req-1"))
	requireStatus(t, err, codes.FailedPrecondition, "only a pending recharge can be cancelled")

	logic := NewCancelRechargeLogic(ctx, svcCtx)
	_, err = logic.CancelRecharge(cancelReq("RC_none", "req-2"))
	requireSentinel(t, err, model.ErrRechargeNotFound, codes.NotFound)

	blank := cancelReq("", "req-3")
	requireSentinel(t, mustFail(t, func() error { _, e := logic.CancelRecharge(blank); return e }),
		model.ErrRechargeNoRequired, codes.InvalidArgument)
	noReason := cancelReq("RC_failed", "req-4")
	noReason.Reason = "  "
	requireSentinel(t, mustFail(t, func() error { _, e := logic.CancelRecharge(noReason); return e }),
		model.ErrReasonRequired, codes.InvalidArgument)
	noOperator := cancelReq("RC_failed", "req-5")
	noOperator.Operator = ""
	requireSentinel(t, mustFail(t, func() error { _, e := logic.CancelRecharge(noOperator); return e }),
		model.ErrOperatorRequired, codes.InvalidArgument)
}

// TestCancelRechargeConcurrentSettle 取消与结算并发：CAS 未命中且对手已入账时，
// 结论必须是「已入账不可取消」，绝不能返回 duplicated 假装取消成功。
func TestCancelRechargeConcurrentSettle(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedRecharge(&model.Recharge{RechargeNo: "RC_race2", RequestId: "req-race2", Mid: 7,
		AmountMinor: 6000, Currency: "CNY", Channel: model.ChannelSandbox, State: model.RechargeStatePending})
	db.cancelSteal = func(d *fakeDB) {
		r := d.recharges["RC_race2"]
		r.State, r.SettledAt, r.Operator = model.RechargeStateSuccess, fakeNow(), "cron"
		d.seedWallet(7, 6000, "CNY")
		d.seedFlow(&model.Flow{Mid: 7, BizType: model.FlowBizRecharge, BizNo: "RC_race2",
			DeltaMinor: 6000, BalanceAfterMinor: 6000, Currency: "CNY", Operator: "cron",
			RequestId: "req-other"})
	}

	_, err := NewCancelRechargeLogic(context.Background(), svcCtx).CancelRecharge(cancelReq("RC_race2", "req-cancel"))
	requireSentinel(t, err, model.ErrRechargeAlreadySettled, codes.FailedPrecondition)
	if db.recharges["RC_race2"].State != model.RechargeStateSuccess {
		t.Error("取消失败方把对手已入账的单据改成了 CANCELLED")
	}
	requireBalance(t, db, 7, 6000)
	requireFlows(t, db, 1)
}

func TestCancelRechargeCasMissStillPending(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedRecharge(&model.Recharge{RechargeNo: "RC_miss", RequestId: "req-miss", Mid: 7,
		AmountMinor: 1000, Currency: "CNY", Channel: model.ChannelSandbox, State: model.RechargeStatePending})
	db.cancelMiss = true

	_, err := NewCancelRechargeLogic(context.Background(), svcCtx).CancelRecharge(cancelReq("RC_miss", "req-cancel"))
	requireStatus(t, err, codes.Aborted, "concurrent ledger update")
	if db.recharges["RC_miss"].State != model.RechargeStatePending {
		t.Error("CAS 未命中却改了状态")
	}
}

// TestRechargeChainKeepsConfigChannelGate 渠道门禁的默认口径（AllowedChannels 缺省）
// 必须仍然是「只放行 SANDBOX」，整条链路不能因为漏配而放行任何真实渠道。
func TestRechargeChainKeepsConfigChannelGate(t *testing.T) {
	cfg := defaultPaymentConf()
	cfg.AllowedChannels = nil
	svcCtx, db := newTestSvc(t, cfg)
	ctx := context.Background()
	opened, err := NewOpenRechargeLogic(ctx, svcCtx).OpenRecharge(openReq(7, 1000, "req-default-gate"))
	if err != nil {
		t.Fatalf("空渠道白名单按默认口径应放行沙箱: %v", err)
	}
	if _, err := NewSettleSandboxRechargeLogic(ctx, svcCtx).
		SettleSandboxRecharge(settleReq(opened.Recharge.RechargeNo, "req-settle")); err != nil {
		t.Fatalf("默认口径下结算应可用: %v", err)
	}
	requireBalance(t, db, 7, 1000)
	if got := config.ChannelSandbox; got != "SANDBOX" {
		t.Fatalf("渠道名常量漂移: %q", got)
	}
}
