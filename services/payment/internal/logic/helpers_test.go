package logic

// helpers.go 的纯函数口径。这些判定直接决定资金契约的边界，必须逐条钉住：
//   - 单据号只认 common/idgen（ULID），取不到熵必须失败，不得退化成时间戳拼接；
//   - 币种单一，不做隐式换汇；
//   - 文本长度按 rune 计，中文理由不能被静默截断；
//   - 列表有界性（page/size/offset/时间窗）超限是拒绝，不是悄悄改小。

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"go-video/common/idgen"
	"go-video/services/payment/internal/config"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// failingGen 模拟熵源耗尽（/dev/urandom 不可读、宿主时钟异常等）。
type failingGen struct{}

func (failingGen) ULID() (string, error)           { return "", errors.New("entropy exhausted") }
func (failingGen) Short(int) (string, error)       { return "", errors.New("entropy exhausted") }
func (failingGen) Prefixed(string) (string, error) { return "", errors.New("entropy exhausted") }

func withFailingIDGen(t *testing.T) {
	t.Helper()
	idgen.SetDefault(failingGen{})
	t.Cleanup(func() { idgen.SetDefault(nil) })
}

// spyGen 记录 logic 传进来的前缀，并给出可预测的 26 字符 ULID 体。
type spyGen struct{ gotPrefixes []string }

func (s *spyGen) ULID() (string, error) { return strings.Repeat("A", 26), nil }
func (s *spyGen) Short(int) (string, error) {
	return "", errors.New("idgen: Short not used here")
}
func (s *spyGen) Prefixed(prefix string) (string, error) {
	s.gotPrefixes = append(s.gotPrefixes, prefix)
	return prefix + "_" + strings.Repeat("A", 26), nil
}

func withSpyIDGen(t *testing.T) *spyGen {
	t.Helper()
	gen := &spyGen{}
	idgen.SetDefault(gen)
	t.Cleanup(func() { idgen.SetDefault(nil) })
	return gen
}

func TestNewDocumentNoPrefixes(t *testing.T) {
	spy := withSpyIDGen(t)
	for _, tc := range []struct {
		name, prefix string
		gen          func() (string, error)
	}{
		{"充值单", "RC", func() (string, error) { return newDocumentNo(prefixRecharge) }},
		{"支付单", "PM", func() (string, error) { return newDocumentNo(prefixPayment) }},
		{"退款单", "RF", func() (string, error) { return newDocumentNo(prefixRefund) }},
		{"调整单", "AJ", func() (string, error) { return newDocumentNo(prefixAdjust) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			no, err := tc.gen()
			if err != nil {
				t.Fatalf("生成单据号失败: %v", err)
			}
			if !strings.HasPrefix(no, tc.prefix+"_") {
				t.Errorf("单据号 %q 应以 %q 前缀开头", no, tc.prefix)
			}
			// *_no 列宽 VARCHAR(40)：RC_ + 26 字符 ULID = 29，必须在列宽内。
			if utf8.RuneCountInString(no) > 40 {
				t.Errorf("单据号 %q 长度 %d 超过 *_no 列宽 40", no, utf8.RuneCountInString(no))
			}
			if len(spy.gotPrefixes) == 0 || spy.gotPrefixes[len(spy.gotPrefixes)-1] != tc.prefix {
				t.Errorf("传给 idgen 的前缀 = %v，期望结尾为 %q", spy.gotPrefixes, tc.prefix)
			}
		})
	}
}

func TestNewDocumentNoUsesULIDCharset(t *testing.T) {
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		no, err := newDocumentNo(prefixPayment)
		if err != nil {
			t.Fatalf("生成单据号失败: %v", err)
		}
		body := strings.TrimPrefix(no, "PM_")
		if len(body) != 26 {
			t.Fatalf("ULID 体长度 = %d，期望 26（单据号 %q）", len(body), no)
		}
		for _, r := range body {
			if !strings.ContainsRune(crockford, upperASCII(r)) {
				t.Fatalf("单据号 %q 含非 Crockford Base32 字符 %q —— 说明号不是 idgen 产的", no, r)
			}
		}
		if seen[no] {
			t.Fatalf("单据号重复：两次生成都是 %q", no)
		}
		seen[no] = true
	}
}

func upperASCII(r rune) rune {
	if r >= 'a' && r <= 'z' {
		return r - 32
	}
	return r
}

// TestNewDocumentNoFailsWithoutEntropy 钉住「取不到熵就失败」：
// 退化成时间戳拼接会造成撞号，并留下重复入账的口子，所以这条路径必须是 Internal 错误。
func TestNewDocumentNoFailsWithoutEntropy(t *testing.T) {
	withFailingIDGen(t)
	for name, gen := range map[string]func() (string, error){
		"recharge": func() (string, error) { return newDocumentNo(prefixRecharge) },
		"payment":  func() (string, error) { return newDocumentNo(prefixPayment) },
		"refund":   func() (string, error) { return newDocumentNo(prefixRefund) },
		"adjust":   func() (string, error) { return newDocumentNo(prefixAdjust) },
	} {
		t.Run(name, func(t *testing.T) {
			no, err := gen()
			requireSentinel(t, err, model.ErrDocumentNoUnavailable, codes.Internal)
			if no != "" {
				t.Errorf("失败时仍返回单据号 %q —— 不得给出时间戳拼装的替代值", no)
			}
		})
	}
}

func TestResolveCurrency(t *testing.T) {
	cfg := defaultPaymentConf()
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "CNY", false},
		{"  cny  ", "CNY", false},
		{"CNY", "CNY", false},
		{"USD", "", true},  // 不做隐式换汇
		{"cny1", "", true}, // 归一后不等于默认币种也要拒
		{"人民币", "", true},
	}
	for _, tc := range cases {
		t.Run("in="+tc.in, func(t *testing.T) {
			got, err := resolveCurrency(cfg, tc.in)
			if tc.wantErr {
				requireSentinel(t, err, model.ErrUnsupportedCurrency, codes.InvalidArgument)
				return
			}
			if err != nil {
				t.Fatalf("resolveCurrency(%q) 失败: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("resolveCurrency(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestResolveCurrencyFollowsConfiguredDefault 币种单一性来自配置而不是写死：
// 配置成 USD 的实例必须接受 USD、拒绝 CNY。
func TestResolveCurrencyFollowsConfiguredDefault(t *testing.T) {
	cfg := defaultPaymentConf()
	cfg.DefaultCurrency = "usd "
	if got, err := resolveCurrency(cfg, ""); err != nil || got != "USD" {
		t.Fatalf("默认币种应取配置值：got %q err %v", got, err)
	}
	if _, err := resolveCurrency(cfg, "CNY"); err == nil {
		t.Fatal("配置为 USD 的实例不得接受 CNY —— 否则等于在服务侧偷偷换汇")
	}
}

func TestRequireRequestID(t *testing.T) {
	for _, empty := range []string{"", "   ", "\t\n"} {
		if err := requireRequestID(empty); err == nil {
			t.Errorf("request_id=%q 必须拒绝：写接口没有幂等键就无法区分重试与重复下单", empty)
		} else {
			requireSentinel(t, err, model.ErrRequestIDRequired, codes.InvalidArgument)
		}
	}
	if err := requireRequestID(strings.Repeat("r", 64)); err != nil {
		t.Errorf("64 字符 request_id 应放过（列宽 64）: %v", err)
	}
	err := requireRequestID(strings.Repeat("r", 65))
	requireStatus(t, err, codes.InvalidArgument, "request_id too long")
}

func TestRequireOperator(t *testing.T) {
	requireSentinel(t, requireOperator(""), model.ErrOperatorRequired, codes.InvalidArgument)
	requireSentinel(t, requireOperator("  "), model.ErrOperatorRequired, codes.InvalidArgument)
	requireStatus(t, requireOperator(strings.Repeat("o", 65)), codes.InvalidArgument, "operator too long")
	if err := requireOperator("运营工号 A01"); err != nil {
		t.Errorf("正常 operator 应放过: %v", err)
	}
}

// TestRequireReasonCountsRunes 中文理由必须按 rune 判定：
// 按字节算会把 34 个汉字误判成超限，而静默截断又会让审计理由失真。
func TestRequireReasonCountsRunes(t *testing.T) {
	if err := requireReason(""); err == nil {
		t.Fatal("reason 必填（Cancel/Refund/Close/Adjust 的审计口径要求可追溯）")
	} else {
		requireSentinel(t, err, model.ErrReasonRequired, codes.InvalidArgument)
	}
	if err := requireReason(strings.Repeat("补", 100)); err != nil {
		t.Errorf("100 个汉字（%d 字节）应放过，实际被拒: %v", utf8.RuneCountInString(strings.Repeat("补", 100))*3, err)
	}
	err := requireReason(strings.Repeat("补", 101))
	requireStatus(t, err, codes.InvalidArgument, "reason too long")
	if utf8.RuneCountInString(strings.Repeat("补", 101)) <= maxTextRunes {
		t.Fatal("maxTextRunes 常量口径变了，本用例的前提需同步")
	}
}

func TestRequireMaxLengthRejectsInsteadOfTruncating(t *testing.T) {
	if err := requireMaxLength("subject", "会员月卡", 100); err != nil {
		t.Fatalf("正常摘要不应被拒: %v", err)
	}
	long := strings.Repeat("a", 101)
	err := requireMaxLength("subject", long, 100)
	requireStatus(t, err, codes.InvalidArgument, "subject too long")
	if err := requireMaxLength("client_trace_id", strings.Repeat("t", 64), 64); err != nil {
		t.Fatalf("64 字符 trace_id 应放过: %v", err)
	}
}

func TestNormalizePage(t *testing.T) {
	cfg := defaultPaymentConf() // MaxPageSize=100 MaxListOffset=10000
	cases := []struct {
		name             string
		page, size       int64
		wantPage, wantSz int64
		wantErr          codes.Code
	}{
		{"默认页", 0, 0, 1, defaultListSize, 0},
		{"负页码归一", -3, 20, 1, 20, 0},
		{"负条数取默认", 2, -1, 2, defaultListSize, 0},
		{"上限内", 1, 100, 1, 100, 0},
		{"超上限直接拒绝而不是改小", 1, 101, 0, 0, codes.InvalidArgument},
		{"深翻页拒绝（offset 10020 > 10000）", 502, 20, 0, 0, codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, size, err := normalizePage(cfg, tc.page, tc.size)
			if tc.wantErr != 0 {
				if err == nil {
					t.Fatalf("normalizePage(%d,%d) 必须报错，实际返回 (%d,%d)", tc.page, tc.size, page, size)
				}
				if got := status.Code(err); got != tc.wantErr {
					t.Fatalf("code = %s，期望 %s（err=%v）", got, tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizePage(%d,%d) 失败: %v", tc.page, tc.size, err)
			}
			if page != tc.wantPage || size != tc.wantSz {
				t.Fatalf("normalizePage(%d,%d) = (%d,%d)，期望 (%d,%d)",
					tc.page, tc.size, page, size, tc.wantPage, tc.wantSz)
			}
		})
	}
}

func TestNormalizePageRejectsOversizeInsteadOfClamping(t *testing.T) {
	cfg := defaultPaymentConf()
	_, _, err := normalizePage(cfg, 1, 101)
	requireSentinel(t, err, model.ErrPageSizeTooLarge, codes.InvalidArgument)
	_, _, err = normalizePage(cfg, 10000001, 1)
	requireSentinel(t, err, model.ErrListOffsetTooDeep, codes.InvalidArgument)
}

// TestNormalizePageFallbacks 配置缺失（0/负数）时的兜底口径必须是可用值，
// 否则 MaxPageSize=0 会把所有列表接口判成超限。
func TestNormalizePageFallbacks(t *testing.T) {
	_, size, err := normalizePage(config.PaymentConf{}, 1, 500)
	requireSentinel(t, err, model.ErrPageSizeTooLarge, codes.InvalidArgument)
	if size != 500 {
		t.Errorf("报错时也要回显调用方给的原值，得到 %d", size)
	}
	if page, sz, err := normalizePage(config.PaymentConf{}, 1, 0); err != nil || page != 1 || sz != defaultListSize {
		t.Fatalf("零值配置应回落到默认页大小 %d，实际 (%d,%d,err=%v)", defaultListSize, page, sz, err)
	}
	if _, _, err := normalizePage(config.PaymentConf{MaxPageSize: 50, MaxListOffset: 100}, 6, 20); err != nil {
		t.Fatalf("offset=100 在 MaxListOffset=100 内应放过: %v", err)
	}
	if _, _, err := normalizePage(config.PaymentConf{MaxPageSize: 50, MaxListOffset: 100}, 7, 20); err == nil {
		t.Fatal("offset=120 超 MaxListOffset=100 必须拒绝")
	}
}

func TestListPageOffset(t *testing.T) {
	if got := listPage(3, 20); got.Offset != 40 || got.Limit != 20 {
		t.Fatalf("listPage(3,20) = %+v，期望 offset=40 limit=20", got)
	}
	if got := listPage(1, 20); got.Offset != 0 {
		t.Fatalf("首页偏移应为 0，得到 %+v", got)
	}
}

func TestRequireListBounds(t *testing.T) {
	cfg := defaultPaymentConf() // MaxListWindowSeconds=2592000（30 天）
	now := fakeNow()

	t.Run("mid 查询不要求时间窗", func(t *testing.T) {
		if err := requireListBounds(cfg, 42, 0, 0); err != nil {
			t.Fatalf("mid>0 无时间窗应放过: %v", err)
		}
		// mid<0 不在本函数裁决：各 List logic 先以 ErrInvalidMid 拦掉（见 list_logic_test.go）。
	})
	t.Run("跨用户必须给完整窗口", func(t *testing.T) {
		requireSentinel(t, requireListBounds(cfg, 0, 0, 0), model.ErrListWindowRequired, codes.InvalidArgument)
		requireSentinel(t, requireListBounds(cfg, 0, now-10, 0), model.ErrListWindowRequired, codes.InvalidArgument)
		requireSentinel(t, requireListBounds(cfg, 0, 0, now), model.ErrListWindowRequired, codes.InvalidArgument)
	})
	t.Run("窗口超上限拒绝而不是截半", func(t *testing.T) {
		err := requireListBounds(cfg, 0, now-2592001, now)
		requireSentinel(t, err, model.ErrListWindowTooLarge, codes.InvalidArgument)
		if err := requireListBounds(cfg, 0, now-2592000, now); err != nil {
			t.Fatalf("恰好等于上限的窗口应放过: %v", err)
		}
	})
	t.Run("from>to 一律非法", func(t *testing.T) {
		requireSentinel(t, requireListBounds(cfg, 42, now, now-1), model.ErrInvalidTimeRange, codes.InvalidArgument)
		requireSentinel(t, requireListBounds(cfg, 0, now, now-1), model.ErrInvalidTimeRange, codes.InvalidArgument)
	})
	t.Run("零值配置回落 30 天窗口", func(t *testing.T) {
		if err := requireListBounds(config.PaymentConf{}, 0, now-2592001, now); err == nil {
			t.Fatal("MaxListWindowSeconds 缺省应为 2592000，超窗必须拒绝")
		}
	})
}

func TestChannelName(t *testing.T) {
	if got := channelName(rpc.PayChannel_PAY_CHANNEL_SANDBOX); got != config.ChannelSandbox {
		t.Fatalf("channelName(SANDBOX) = %q，期望 %q", got, config.ChannelSandbox)
	}
	if got := channelName(rpc.PayChannel(0)); got != "UNSPECIFIED" {
		t.Fatalf("未指定渠道应映射成 UNSPECIFIED（配置里不存在即不放行），得到 %q", got)
	}
	if got := channelName(rpc.PayChannel(99)); got != "99" {
		t.Fatalf("越界枚举 protobuf String() 给出数字串 %q，得到 %q", "99", got)
	}
	// 关键是越界/未指定的名字都不在渠道白名单里，绝不被放行：
	cfg := defaultPaymentConf()
	for _, name := range []string{channelName(rpc.PayChannel(0)), channelName(rpc.PayChannel(99)), "ALIPAY", ""} {
		if cfg.AllowsChannel(name) {
			t.Errorf("渠道名 %q 不得被放行", name)
		}
	}
}

// TestWalletInfoNilMeansZeroBalance 账户不存在时不得返回 nil：
// 契约要求「无账户＝0 余额」，而 frozen 恒为 0，读侧不能拿它当可用余额。
func TestWalletInfoNilMeansZeroBalance(t *testing.T) {
	got := walletInfo(nil, 7, "CNY")
	if got == nil {
		t.Fatal("账户缺失也要给出 0 余额快照")
	}
	if got.Mid != 7 || got.BalanceMinor != 0 || got.FrozenMinor != 0 || got.Currency != "CNY" {
		t.Fatalf("0 余额快照不符: %+v", got)
	}
	if got.Version != 0 || got.Ctime != 0 || got.Mtime != 0 {
		t.Fatalf("账户不存在时不得凭空造版本号: %+v", got)
	}
	w := &model.Wallet{Mid: 7, BalanceMinor: 100, FrozenMinor: 0, Currency: "CNY", Version: 3}
	if got := walletInfo(w, 7, "CNY"); got.BalanceMinor != 100 || got.Version != 3 {
		t.Fatalf("有账户时应回显台账值: %+v", got)
	}
}

func TestProjectionsNilSafeAndEmptyNotNil(t *testing.T) {
	if rechargeInfo(nil) != nil || paymentInfo(nil) != nil || refundInfo(nil) != nil || flowInfo(nil) != nil {
		t.Fatal("行缺失应投影为 nil，由上层按未找到处理")
	}
	if got := rechargeInfos(nil); got == nil || len(got) != 0 {
		t.Errorf("rechargeInfos(nil) = %v，期望非 nil 空切片（契约里列表字段不得为 null）", got)
	}
	if got := paymentInfos(nil); got == nil || len(got) != 0 {
		t.Errorf("paymentInfos(nil) = %v，期望非 nil 空切片", got)
	}
	if got := refundInfos(nil); got == nil || len(got) != 0 {
		t.Errorf("refundInfos(nil) = %v，期望非 nil 空切片", got)
	}
	if got := flowInfos(nil); got == nil || len(got) != 0 {
		t.Errorf("flowInfos(nil) = %v，期望非 nil 空切片", got)
	}
}

// TestProjectionKeepsLedgerFields 投影必须把台账的审计字段带出去（运营面要能看到经办人），
// 同时不得引入契约外的伪造字段。逐字段比对，防止以后加列漏投影。
func TestProjectionKeepsLedgerFields(t *testing.T) {
	rc := &model.Recharge{RechargeNo: "RC_1", Mid: 7, AmountMinor: 100, Currency: "CNY",
		Channel: model.ChannelSandbox, State: model.RechargeStatePending, Operator: "cron",
		RequestId: "req-1", Reason: "r", SettledAt: 123, Ctime: 11, Mtime: 12}
	rcInfo := rechargeInfo(rc)
	if rcInfo.RechargeNo != rc.RechargeNo || rcInfo.State != rpc.RechargeState(rc.State) ||
		rcInfo.Channel != rpc.PayChannel(rc.Channel) || rcInfo.RequestId != rc.RequestId ||
		rcInfo.Operator != rc.Operator || rcInfo.Reason != rc.Reason || rcInfo.SettledAt != rc.SettledAt ||
		rcInfo.Ctime != rc.Ctime || rcInfo.Mtime != rc.Mtime || rcInfo.AmountMinor != rc.AmountMinor {
		t.Fatalf("RechargeInfo 投影缺字段: %+v", rcInfo)
	}

	pm := &model.Payment{PaymentNo: "PM_1", BizOrderNo: "O1", Mid: 7, AmountMinor: 400,
		RefundedMinor: 100, Currency: "CNY", Method: model.MethodBalance, State: model.PaymentStatePaid,
		Subject: "会员", PaidAt: 5, ExpireAt: 6, RequestId: "req-2", Ctime: 1, Mtime: 2,
		Operator: "user", Remark: "close reason"}
	pmInfo := paymentInfo(pm)
	if pmInfo.RefundedMinor != 100 || pmInfo.Method != rpc.PayMethod_PAY_METHOD_BALANCE ||
		pmInfo.State != rpc.PaymentState_PAYMENT_STATE_PAID || pmInfo.Operator != "user" ||
		pmInfo.Remark != "close reason" || pmInfo.BizOrderNo != "O1" || pmInfo.PaidAt != 5 || pmInfo.ExpireAt != 6 {
		t.Fatalf("PaymentInfo 投影缺字段: %+v", pmInfo)
	}

	rf := &model.Refund{RefundNo: "RF_1", PaymentNo: "PM_1", BizOrderNo: "O1", Mid: 7,
		AmountMinor: 100, Currency: "CNY", State: model.RefundStateSucceeded,
		Destination: model.DestinationBalance, Operator: "op", RequestId: "req-3", Reason: "why", Ctime: 9}
	rfInfo := refundInfo(rf)
	if rfInfo.Destination != model.DestinationBalance || rfInfo.State != rpc.RefundState_REFUND_STATE_SUCCEEDED ||
		rfInfo.PaymentNo != "PM_1" || rfInfo.BizOrderNo != "O1" || rfInfo.Ctime != 9 {
		t.Fatalf("RefundInfo 投影缺字段: %+v", rfInfo)
	}

	fl := &model.Flow{FlowId: 3, Mid: 7, BizType: model.FlowBizAdminAdjust, BizNo: "AJ_1",
		DeltaMinor: -50, BalanceAfterMinor: 600, Currency: "CNY", Remark: "r", Operator: "op",
		RequestId: "req-4", Ctime: 8}
	flInfo := flowInfo(fl)
	if flInfo.BizType != rpc.FlowBizType_FLOW_BIZ_TYPE_ADMIN_ADJUST || flInfo.BalanceAfterMinor != 600 ||
		flInfo.DeltaMinor != -50 || flInfo.RequestId != "req-4" || flInfo.FlowId != 3 {
		t.Fatalf("FlowInfo 投影缺字段: %+v", flInfo)
	}
}
