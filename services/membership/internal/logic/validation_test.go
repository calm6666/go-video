package logic

// 校验、边界换算与判定口径的纯函数用例。
//
// 本文件钉住的是「不需要数据库也必须永远成立」的那部分结论：
//  1. 列宽校验按 rune 计数（VARCHAR(n) 是字符数不是字节数），放过/拒绝的分界正好在列宽上；
//  2. 分页与批量上限是「裁剪而不是报错」，并且回显实际生效值；
//  3. 幂等指纹覆盖 plan_id / biz_order_no / payment_no，少一个字段就会把改口径当成重放；
//  4. reason 不参与套餐指纹（补理由≠换规格）；
//  5. decideEntitlement 是 CheckEntitlement/CheckEntitlements 的唯一实现，七种结论互不混淆。

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/membership/internal/config"
	"go-video/services/membership/model"
	"go-video/services/membership/rpc"
)

const day = model.SecondsPerDay

// --- 长度与必填 ---

func TestCheckLenCountsRunesNotBytes(t *testing.T) {
	// 64 个汉字 = 192 字节。按字节实现的校验会在这里当场拒绝一条完全合法的名称，
	// 而按 rune 实现（MySQL VARCHAR(64) 的口径）必须放过。
	cn64 := strings.Repeat("会", 64)
	if len(cn64) != 192 {
		t.Fatalf("夹具异常：%q 的字节数应为 192，实为 %d", cn64, len(cn64))
	}
	if err := checkLen("name", cn64, model.MaxPlanNameLength); err != nil {
		t.Fatalf("64 个汉字应放过（列宽是字符数）：%v", err)
	}
	if err := checkLen("name", cn64+"员", model.MaxPlanNameLength); !errors.Is(err, model.ErrFieldTooLong) {
		t.Fatalf("65 个汉字必须拒绝，实得 %v", err)
	}
	// 边界：正好等于列宽放过，超一字符拒绝。
	for _, tc := range []struct {
		n    int
		want error
	}{
		{model.MaxBizNoLength, nil},
		{model.MaxBizNoLength + 1, model.ErrFieldTooLong},
		{model.MaxReasonLength, nil},
		{model.MaxReasonLength + 1, model.ErrFieldTooLong},
	} {
		got := checkLen("f", strings.Repeat("a", tc.n), tc.n)
		if !errors.Is(got, tc.want) && tc.want == nil {
			t.Fatalf("长度 %d 应放过，实得 %v", tc.n, got)
		}
		if tc.want != nil {
			long := checkLen("f", strings.Repeat("a", tc.n+1), tc.n)
			if !errors.Is(long, tc.want) {
				t.Fatalf("超限应得 %v，实得 %v", tc.want, long)
			}
		}
	}
}

func TestRequireRequestIdHonoursColumnWidth(t *testing.T) {
	// 64 是 mb_grant.request_id / mb_biz_request.request_id / mb_plan_change_log.request_id
	// 的列宽（model/migration_parity_test.go 会拿它与建表语句比对）。
	// 这里写字面量而不是 maxRequestIdLen，是为了让「常量被改动」与「列宽漂移」都能红。
	if err := requireRequestId(strings.Repeat("r", 64)); err != nil {
		t.Fatalf("恰好 64 字符的幂等键应放过：%v", err)
	}
	if err := requireRequestId(strings.Repeat("r", 65)); !errors.Is(err, model.ErrFieldTooLong) {
		t.Fatalf("65 字符必须拒绝，实得 %v", err)
	}
	if maxRequestIDWidth != maxRequestIdLen {
		t.Fatalf("logic 的 maxRequestIdLen=%d 与测试夹具列宽 %d 不一致", maxRequestIdLen, maxRequestIDWidth)
	}
	for _, blank := range []string{"", "   ", "\t\n"} {
		if err := requireRequestId(blank); !errors.Is(err, model.ErrRequestIdRequired) {
			t.Fatalf("空/全空白 request_id %q 应得 ErrRequestIdRequired，实得 %v", blank, err)
		}
	}
}

func TestRequiredGuardsRejectEmptyAndOverlong(t *testing.T) {
	if err := requireMid(0); !errors.Is(err, model.ErrInvalidMid) {
		t.Fatalf("mid=0 应拒绝（游客态由调用方自己判定），实得 %v", err)
	}
	if err := requireMid(-7); !errors.Is(err, model.ErrInvalidMid) {
		t.Fatalf("负 mid 应拒绝，实得 %v", err)
	}
	if err := requireMid(1); err != nil {
		t.Fatalf("正 mid 应放过：%v", err)
	}

	for _, v := range []rpc.VipType{rpc.VipType_VIP_TYPE_UNSPECIFIED, rpc.VipType(0), rpc.VipType(3), rpc.VipType(-1)} {
		if err := requireVipType(v); !errors.Is(err, model.ErrInvalidVipType) {
			t.Fatalf("档位 %d 不可落库，实得 %v", int32(v), err)
		}
	}
	for _, v := range []rpc.VipType{rpc.VipType_VIP_TYPE_PREMIUM, rpc.VipType_VIP_TYPE_PREMIUM_PLUS} {
		if err := requireVipType(v); err != nil {
			t.Fatalf("合法档位 %v 应放过：%v", v, err)
		}
	}
	// UNSPECIFIED 在「取当前生效最高档」的调用方是合法入参。
	if err := requireVipTypeOrUnspecified(rpc.VipType_VIP_TYPE_UNSPECIFIED); err != nil {
		t.Fatalf("UNSPECIFIED 应放过：%v", err)
	}
	if err := requireVipTypeOrUnspecified(rpc.VipType(9)); !errors.Is(err, model.ErrInvalidVipType) {
		t.Fatalf("越界档位仍要拒绝，实得 %v", err)
	}

	if err := requireOperator("  "); !errors.Is(err, model.ErrOperatorRequired) {
		t.Fatalf("空 operator 应拒绝，实得 %v", err)
	}
	if err := requireOperator(strings.Repeat("o", model.MaxOperatorLength+1)); !errors.Is(err, model.ErrFieldTooLong) {
		t.Fatalf("operator 超列宽应拒绝，实得 %v", err)
	}
	if err := requireReason(" "); !errors.Is(err, model.ErrReasonRequired) {
		t.Fatalf("收回/上下架无理由应拒绝，实得 %v", err)
	}
	// optionalReason 允许空，但同样吃列宽约束。
	if err := optionalReason(""); err != nil {
		t.Fatalf("可选理由留空应放过：%v", err)
	}
	if err := optionalReason(strings.Repeat("理", model.MaxReasonLength+1)); !errors.Is(err, model.ErrFieldTooLong) {
		t.Fatalf("可选理由超列宽必须拒绝，实得 %v", err)
	}
}

func TestCheckGrantDeltaRejectsOutOfBoundsInsteadOfClamping(t *testing.T) {
	cfg := config.MembershipConf{MaxGrantDeltaDays: 3660}
	for _, ok := range []int32{1, 3660, -1, -3660} {
		if err := checkGrantDelta(cfg, ok); err != nil {
			t.Fatalf("delta=%d 在 ±上限内应放过：%v", ok, err)
		}
	}
	for _, bad := range []int32{0, 3661, -3661} {
		if err := checkGrantDelta(cfg, bad); !errors.Is(err, model.ErrInvalidGrantDelta) {
			t.Fatalf("delta=%d 必须拒绝而不是静默裁剪，实得 %v", bad, err)
		}
	}
	// 配置为 0（漏配）时收敛到内置 3660，而不是「任何值都越界」或「任何值都放过」。
	zero := checkGrantDelta(config.MembershipConf{}, 3660)
	if zero != nil {
		t.Fatalf("缺配应回落到 3660 上限：%v", zero)
	}
	if err := checkGrantDelta(config.MembershipConf{}, 3661); !errors.Is(err, model.ErrInvalidGrantDelta) {
		t.Fatalf("缺配回落后的上界仍要生效，实得 %v", err)
	}
}

// --- 分页与批量上限：裁剪而不是报错 ---

func TestClampPageTruncatesAndEchoes(t *testing.T) {
	cfg := testConf() // DefaultPageSize=20, MaxPageSize=100
	cases := []struct {
		name       string
		page, size int64
		wantPage   int64
		wantSize   int64
		wantOffset int64
	}{
		{"双零用缺省", 0, 0, 1, 20, 0},
		{"负页码归一", -5, 10, 1, 10, 0},
		{"正常页", 3, 50, 3, 50, 100},
		{"size 超上限裁剪到 100", 2, 99999, 2, 100, 100},
		{"size 负数回落缺省", 4, -1, 4, 20, 60},
		{"第二页偏移按裁剪后的 size 算", 2, 100, 2, 100, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, size, offset := clampPage(cfg, tc.page, tc.size)
			if page != tc.wantPage || size != tc.wantSize || offset != tc.wantOffset {
				t.Fatalf("clampPage(%d,%d) = (%d,%d,%d)，期望 (%d,%d,%d)",
					tc.page, tc.size, page, size, offset, tc.wantPage, tc.wantSize, tc.wantOffset)
			}
		})
	}
	// 坏配置（缺省与上限都是 0）不得给出 size=0 —— 那等于「一次读 0 行」的假成功。
	page, size, offset := clampPage(config.MembershipConf{}, 0, 0)
	if size < 1 || page < 1 || offset < 0 {
		t.Fatalf("缺配下 clampPage 应收敛到安全值，实得 (%d,%d,%d)", page, size, offset)
	}
	// MaxPageSize<=0 表示「不设上限」，此时不得把调用方要求的 size 抹平。
	_, size, _ = clampPage(config.MembershipConf{DefaultPageSize: 50}, 1, 7)
	if size != 7 {
		t.Fatalf("未设上限时应尊重调用方的 size=7，实得 %d", size)
	}
}

func TestClampLimitFallsBackToCeiling(t *testing.T) {
	cfg := config.MembershipConf{ExpireScanMaxLimit: 500}
	for _, tc := range []struct{ in, want int64 }{
		{0, 500}, {-1, 500}, {500, 500}, {501, 500}, {1 << 40, 500}, {7, 7},
	} {
		if got := clampLimit(cfg, tc.in); got != tc.want {
			t.Fatalf("clampLimit(%d) = %d，期望 %d", tc.in, got, tc.want)
		}
	}
	// 上限漏配时回落到内置 500。
	if got := clampLimit(config.MembershipConf{}, 9999); got != 500 {
		t.Fatalf("缺配应回落 500，实得 %d", got)
	}
	// 负上限按缺省 500 处理：既不能把请求值原样放过，也不能把批次夹成负数。
	if got := clampLimit(config.MembershipConf{ExpireScanMaxLimit: -3}, 9999); got != 500 {
		t.Fatalf("负上限应按缺省 500 封顶，实得 %d", got)
	}
	if got := clampLimit(config.MembershipConf{ExpireScanMaxLimit: -3}, 10); got != 10 {
		t.Fatalf("负上限下仍应尊重更小的批次要求，实得 %d", got)
	}
}

// --- 幂等指纹 ---

// TestGrantMatchesRequestCoversAuditRefs 钉住重放比对的字段全集。
// plan_id / biz_order_no / payment_no 是本轮新加的追溯位：漏比一个字段，
// 「同 request_id 换一单」就会被判成重放并把首次结果还回去，等于静默改了台账口径。
func TestGrantMatchesRequestCoversAuditRefs(t *testing.T) {
	row := &model.Grant{
		Mid: 42, VipType: model.VipTypePremium, Action: model.ActionGrant, DeltaDays: 31,
		PlanID: 7, Source: model.GrantSourceSandboxPurchase,
		BizOrderNo: "TO-1", PaymentNo: "PM-1",
	}
	base := func() []any {
		return []any{int64(42), model.VipTypePremium, model.ActionGrant, int32(31),
			int64(7), model.GrantSourceSandboxPurchase, "TO-1", "PM-1"}
	}
	match := func(args []any) bool {
		return grantMatchesRequest(row, args[0].(int64), args[1].(int32), args[2].(string),
			args[3].(int32), args[4].(int64), args[5].(int32), args[6].(string), args[7].(string))
	}
	if !match(base()) {
		t.Fatal("完全一致的参数必须判定为重放")
	}
	// 逐字段改动都必须被识别成「换了口径」。
	for _, tc := range []struct {
		name  string
		index int
		value any
	}{
		{"mid", 0, int64(43)},
		{"vip_type", 1, int32(model.VipTypePremiumPlus)},
		{"action", 2, model.ActionExtend},
		{"delta_days", 3, int32(30)},
		{"plan_id", 4, int64(8)},
		{"source", 5, int32(model.GrantSourceAdminOps)},
		{"biz_order_no", 6, "TO-2"},
		{"payment_no", 7, "PM-2"},
	} {
		args := base()
		args[tc.index] = tc.value
		if match(args) {
			t.Errorf("%s 改变后必须判定为冲突（ErrRequestIdReused 的前提），却被认成重放", tc.name)
		}
	}
	// 引用两侧的空格是传输噪声，比对前应归一化。
	if !grantMatchesRequest(row, 42, model.VipTypePremium, model.ActionGrant, 31, 7,
		model.GrantSourceSandboxPurchase, "  TO-1 ", "\tPM-1") {
		t.Fatal("biz_order_no/payment_no 的首尾空格不得破坏重放判定")
	}
	// 逐字节比较：大小写不同的单号不是同一笔资金流水。
	if grantMatchesRequest(row, 42, model.VipTypePremium, model.ActionGrant, 31, 7,
		model.GrantSourceSandboxPurchase, "to-1", "PM-1") {
		t.Fatal("biz_order_no 大小写不同必须判为不同请求（列是 utf8mb4_bin）")
	}
}

func TestFingerprintSeparatesSegments(t *testing.T) {
	// 不加 0x00 分隔时 ["ab","c"] 与 ["a","bc"] 会折叠成同一个指纹。
	if model.Fingerprint("ab", "c") == model.Fingerprint("a", "bc") {
		t.Fatal("指纹必须区分参数边界")
	}
	if got := len(model.Fingerprint("x")); got != 64 {
		t.Fatalf("指纹是 sha256 hex，长度应为 64，实得 %d", got)
	}
}

// --- reason 与指纹的关系 ---

func TestPlanRequestFingerprintIgnoresReasonOnly(t *testing.T) {
	mk := func(price int64, reason string, version int64) *rpc.UpsertPlanReq {
		return &rpc.UpsertPlanReq{
			PlanCode: "vip-month", Name: "月卡", VipType: rpc.VipType_VIP_TYPE_PREMIUM,
			DurationDays: 31, UnitCount: 1, PriceMinor: price, Currency: "CNY",
			Platforms: []rpc.PlanPlatform{rpc.PlanPlatform_PLAN_PLATFORM_ANDROID},
			Reason:    reason, ExpectedVersion: version,
		}
	}
	sum := func(in *rpc.UpsertPlanReq) string {
		_, draft, err := planDraftFromReq(in, testConf(), in.PlanCode)
		if err != nil {
			t.Fatalf("夹具请求不合法：%v", err)
		}
		return planRequestFingerprint(in.PlanCode, draft, in.ExpectedVersion)
	}
	// 只补一次理由：不该被判定成换了套餐规格。
	if sum(mk(3000, "", 0)) != sum(mk(3000, "运营补记", 0)) {
		t.Fatal("reason 不得进入套餐指纹")
	}
	if sum(mk(3000, "x", 0)) == sum(mk(3100, "x", 0)) {
		t.Error("改价必须改变指纹，否则同 request_id 换价格会被静默重放")
	}
	if sum(mk(3000, "x", 1)) == sum(mk(3000, "x", 2)) {
		t.Error("CAS 位必须进指纹")
	}
	if sum(mk(3000, "x", 1)) == sum(func() *rpc.UpsertPlanReq {
		in := mk(3000, "x", 1)
		in.PlanCode = "vip-year"
		return in
	}()) {
		t.Error("换 plan_code 必须改变指纹")
	}
}

func TestPlanLedgerReasonAndExpireLedgerReasonFallbacks(t *testing.T) {
	// UpsertPlan：留空才回落缺省摘要，填了就原样入台账。
	if got := planLedgerReason(""); got != upsertPlanLedgerReason {
		t.Fatalf("空理由应回落缺省摘要，实得 %q", got)
	}
	if got := planLedgerReason("  \t "); got != upsertPlanLedgerReason {
		t.Fatalf("全空白理由应回落缺省摘要，实得 %q", got)
	}
	if got := planLedgerReason(" 改价上线 "); got != "改价上线" {
		t.Fatalf("填了理由应去掉首尾后原样入台账，实得 %q", got)
	}
	if upsertPlanLedgerReason == "" {
		t.Fatal("缺省摘要不允许是空串：台账要能区分「无理由」与「忘了写」")
	}

	// ExpireMembership：cron 填了就用它的（能说明是哪次任务），留空回落机器可读摘要。
	if got := expireLedgerReason("task-77", 1700000000, "cron"); got != "task-77" {
		t.Fatalf("到期理由应尊重调用方，实得 %q", got)
	}
	fallback := expireLedgerReason("   ", 1700000000, "cron")
	if !strings.Contains(fallback, "1700000000") || !strings.Contains(fallback, "cron") {
		t.Fatalf("兜底理由必须能说明到期时刻与判定方，实得 %q", fallback)
	}
	if len([]rune(fallback)) > model.MaxReasonLength {
		t.Fatalf("兜底理由本身不得超过列宽：%d", len([]rune(fallback)))
	}
}

// --- 状态机与收回换算 ---

func TestCanMovePlanStateOnlyThreeEdges(t *testing.T) {
	all := []int32{model.PlanStateDraft, model.PlanStateOnSale, model.PlanStateOffSale}
	allowed := map[[2]int32]bool{
		{model.PlanStateDraft, model.PlanStateOnSale}:   true,
		{model.PlanStateOnSale, model.PlanStateOffSale}: true,
		{model.PlanStateOffSale, model.PlanStateOnSale}: true,
	}
	for _, from := range append(all, 0, 9) {
		for _, to := range append(all, 0, 9) {
			want := allowed[[2]int32{from, to}]
			if got := canMovePlanState(from, to); got != want {
				t.Errorf("canMovePlanState(%d,%d) = %v，期望 %v", from, to, got, want)
			}
		}
	}
	// 显式重申三条业务结论（上面的表若被改坏，这三条会指出为什么该红）。
	if canMovePlanState(model.PlanStateOnSale, model.PlanStateOnSale) {
		t.Error("同状态「再上一次」不属于合法迁移：幂等重试由 request_id 重放路径负责")
	}
	if canMovePlanState(model.PlanStateOnSale, model.PlanStateDraft) {
		t.Error("已上架不允许退回草稿")
	}
	if canMovePlanState(model.PlanStateOffSale, model.PlanStateDraft) {
		t.Error("已下架不允许退回草稿")
	}
}

func TestRevokeResultNeverCreatesNegativeBalance(t *testing.T) {
	now := int64(1_700_000_000)
	cases := []struct {
		name      string
		before    int64
		clearRem  bool
		deltaDays int32
		want      int64
	}{
		{"未到期扣 10 天", now + 30*day, false, 10, now + 20*day},
		{"扣过头停在 now", now + 5*day, false, 10, now},
		{"立即失效", now + 30*day, true, 0, now},
		{"已过期保持原值（不推进也不回拉）", now - 7*day, false, 10, now - 7*day},
		{"已过期且 clear_remaining 也不改", now - 1, true, 0, now - 1},
		{"恰好等于 now 视为已过期", now, true, 0, now},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := revokeResult(tc.before, tc.clearRem, tc.deltaDays, now)
			if got != tc.want {
				t.Fatalf("revokeResult(%d,%v,%d) = %d，期望 %d", tc.before, tc.clearRem, tc.deltaDays, got, tc.want)
			}
			if got < now && tc.before > now {
				t.Fatalf("未到期身份被扣成了过去的时刻 %d < now=%d（负余额会让下次续期白送时长）", got, now)
			}
		})
	}
}

func TestRevokePlanIDNormalizesNegative(t *testing.T) {
	for _, tc := range []struct{ in, want int64 }{{-5, 0}, {0, 0}, {7, 7}, {1 << 40, 1 << 40}} {
		if got := revokePlanID(tc.in); got != tc.want {
			t.Fatalf("revokePlanID(%d) = %d，期望 %d", tc.in, got, tc.want)
		}
	}
}

func TestDaysRemovedCeilsAndNeverGoesPositive(t *testing.T) {
	for _, tc := range []struct {
		seconds int64
		want    int32
	}{
		{0, 0}, {-1, 0}, {1, -1}, {day, -1}, {day + 1, -2}, {31 * day, -31},
	} {
		if got := daysRemoved(tc.seconds); got != tc.want {
			t.Fatalf("daysRemoved(%d) = %d，期望 %d", tc.seconds, got, tc.want)
		}
	}
	// 极端值不得溢出成正数（int32 截断是台账上最难查的错误）。
	if got := daysRemoved(1 << 62); got >= 0 {
		t.Fatalf("超大秒数折算后被截断成 %d，必须仍是非正数", got)
	}
}

func TestSmallConverters(t *testing.T) {
	if got := membershipSubject(42, model.VipTypePremiumPlus); got != "mid:42:vip:2" {
		t.Fatalf("subject 格式变了会破坏幂等台账的可读性：%q", got)
	}
	if autoRenewOf(0) || !autoRenewOf(1) {
		t.Fatal("autoRenewOf 只认 1")
	}
	if int32FromBool(true) != 1 || int32FromBool(false) != 0 {
		t.Fatal("bool 落 0/1 列值")
	}
}

// --- 权益码字符集与签约渠道 ---

func TestValidEntitlementCodeCharset(t *testing.T) {
	for _, ok := range []string{"vip.high_bitrate", "vip-early-access", "A0._-", "playback.4k"} {
		if !validEntitlementCode(ok) {
			t.Errorf("%q 是合法权益码", ok)
		}
	}
	// 含空格/中文/引号的码会让「看起来相同」的两个码给出两个结论。
	for _, bad := range []string{"vip high", "会员.code", "vip(code)", "vip/high", "café"} {
		if validEntitlementCode(bad) {
			t.Errorf("%q 必须被字符集拒掉", bad)
		}
	}
	// 空串在字符集上是「无一字符非法」，由上游的 code required 拒掉；
	// 这条断言把两道校验的分工钉死，避免有人以为字符集校验会兜住空码。
	if !validEntitlementCode("") {
		t.Error("空码应由 ErrEntitlementCodeRequired 拒，而不是字符集校验")
	}
}

func TestNormalizeAutoRenewChannelAcceptsSandboxOnly(t *testing.T) {
	if got, err := normalizeAutoRenewChannel(true, " SANDBOX "); err != nil || got != model.AllowedAutoRenewChannel {
		t.Fatalf("SANDBOX 应放过并去空白，实得 (%q,%v)", got, err)
	}
	if _, err := normalizeAutoRenewChannel(true, ""); !errors.Is(err, model.ErrAutoRenewChannelRequired) {
		t.Fatalf("签约缺渠道应得 ErrAutoRenewChannelRequired，实得 %v", err)
	}
	// 逐字节比对：落一个小写协议位就等于给续费 cron 留一个假线索。
	if _, err := normalizeAutoRenewChannel(true, "sandbox"); !errors.Is(err, model.ErrAutoRenewChannelRejected) {
		t.Fatalf("小写 sandbox 必须拒绝（列是 utf8mb4_bin），实得 %v", err)
	}
	for _, real := range []string{"ALIPAY", "WECHAT", "APPLE_IAP"} {
		if _, err := normalizeAutoRenewChannel(true, real); !errors.Is(err, model.ErrAutoRenewChannelRejected) {
			t.Errorf("真实代扣渠道 %s 一律不得落库，实得 %v", real, err)
		}
	}
	// 解约：允许不带渠道，也允许客户端原样回显渠道，但归一化后必须为空。
	if got, err := normalizeAutoRenewChannel(false, ""); err != nil || got != "" {
		t.Fatalf("解约空渠道应归一化为空，实得 (%q,%v)", got, err)
	}
	if got, err := normalizeAutoRenewChannel(false, "ALIPAY"); err != nil || got != "" {
		t.Fatalf("解约带回显渠道应归一化为空，实得 (%q,%v)", got, err)
	}
	if _, err := normalizeAutoRenewChannel(true, strings.Repeat("c", model.MaxAutoRenewChannelLength+1)); !errors.Is(err, model.ErrFieldTooLong) {
		t.Fatalf("渠道超列宽应得 ErrFieldTooLong，实得 %v", err)
	}
}

// --- 套餐草稿装配 ---

func validPlanReq() *rpc.UpsertPlanReq {
	return &rpc.UpsertPlanReq{
		PlanCode: "vip-month", Name: "大会员月卡", VipType: rpc.VipType_VIP_TYPE_PREMIUM,
		DurationDays: 31, PriceMinor: 3000, Currency: "CNY",
		Platforms: []rpc.PlanPlatform{rpc.PlanPlatform_PLAN_PLATFORM_ANDROID, rpc.PlanPlatform_PLAN_PLATFORM_IOS},
		Operator:  "ops-1", RequestId: "req-1",
	}
}

func TestPlanDraftFromReqValidationTable(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*rpc.UpsertPlanReq)
		cfg     *config.MembershipConf
		wantErr error
	}{
		{"缺 name", func(in *rpc.UpsertPlanReq) { in.Name = "   " }, nil, model.ErrPlanNameRequired},
		{"name 超列宽", func(in *rpc.UpsertPlanReq) { in.Name = strings.Repeat("卡", model.MaxPlanNameLength+1) }, nil, model.ErrFieldTooLong},
		{"description 超列宽", func(in *rpc.UpsertPlanReq) {
			in.Description = strings.Repeat("述", model.MaxPlanDescLength+1)
		}, nil, model.ErrFieldTooLong},
		{"档位非法", func(in *rpc.UpsertPlanReq) { in.VipType = rpc.VipType(9) }, nil, model.ErrInvalidVipType},
		{"duration_days 非正", func(in *rpc.UpsertPlanReq) { in.DurationDays = 0 }, nil, model.ErrInvalidPlanDuration},
		{"unit_count 负数", func(in *rpc.UpsertPlanReq) { in.UnitCount = -1 }, nil, model.ErrInvalidPlanDuration},
		{"总时长超单次上限", func(in *rpc.UpsertPlanReq) {
			in.DurationDays, in.UnitCount = 3661, 1
		}, nil, model.ErrInvalidPlanDuration},
		{"总时长按配置上限判定越界", func(in *rpc.UpsertPlanReq) {
			in.DurationDays, in.UnitCount = 366, 11 // 4026 天 > MaxGrantDeltaDays 3660
		}, &config.MembershipConf{MaxGrantDeltaDays: 3660}, model.ErrInvalidPlanDuration},
		{"原价为负", func(in *rpc.UpsertPlanReq) { in.PriceMinor = -1 }, nil, model.ErrInvalidPlanPrice},
		{"促销价为负", func(in *rpc.UpsertPlanReq) { in.PromPriceMinor = -1 }, nil, model.ErrInvalidPlanPrice},
		{"促销价不低于原价", func(in *rpc.UpsertPlanReq) {
			in.PriceMinor, in.PromPriceMinor = 3000, 3000
		}, nil, model.ErrInvalidPlanPrice},
		{"不支持的币种", func(in *rpc.UpsertPlanReq) { in.Currency = "USD" }, nil, model.ErrUnsupportedCurrency},
		{"无平台", func(in *rpc.UpsertPlanReq) { in.Platforms = nil }, nil, model.ErrPlanPlatformsRequired},
		{"平台含 UNSPECIFIED", func(in *rpc.UpsertPlanReq) {
			in.Platforms = []rpc.PlanPlatform{rpc.PlanPlatform_PLAN_PLATFORM_UNSPECIFIED}
		}, nil, model.ErrInvalidPlatform},
		{"平台序数越界", func(in *rpc.UpsertPlanReq) { in.Platforms = []rpc.PlanPlatform{rpc.PlanPlatform(9)} }, nil, model.ErrInvalidPlatform},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validPlanReq()
			cfg := testConf()
			if tc.cfg != nil {
				cfg = *tc.cfg
				cfg.DefaultCurrency = "CNY"
			}
			tc.mutate(in)
			_, _, err := planDraftFromReq(in, cfg, in.PlanCode)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
		})
	}
}

func TestPlanDraftFromReqAppliesDocumentedDefaults(t *testing.T) {
	in := validPlanReq()
	in.UnitCount = 0 // proto3 区分不了「未设置」与 0，月卡最常见写法是留空 => 1 个售卖单位
	in.Currency = "" // 留空走配置的缺省币种
	mask, draft, err := planDraftFromReq(in, testConf(), in.PlanCode)
	if err != nil {
		t.Fatalf("合法请求被拒：%v", err)
	}
	if draft.UnitCount != 1 || draft.DurationDays != 31 {
		t.Fatalf("留空的 unit_count 应按 1 处理，实得 %+v", draft)
	}
	if draft.Currency != "CNY" {
		t.Fatalf("币种缺省应为 CNY，实得 %q", draft.Currency)
	}
	if mask != model.PlatformBitAndroid|model.PlatformBitIOS {
		t.Fatalf("平台位掩码应为 Android|iOS，实得 %d", mask)
	}
	if draft.AutoRenewSupported != 0 {
		t.Fatalf("未声明支持签约时不得为 1：%d", draft.AutoRenewSupported)
	}
	// 连 DefaultCurrency 都漏配时仍要落到 CNY（否则会把空币种写进 CHAR(3)）。
	_, draft, err = planDraftFromReq(in, config.MembershipConf{}, in.PlanCode)
	if err != nil || draft.Currency != "CNY" {
		t.Fatalf("缺配兜底失败：currency=%q err=%v", draft.Currency, err)
	}
	// 促销价 0 表示「无促销」，不得被 >= 原价 的规则误杀。
	in.PromPriceMinor = 0
	if _, _, err := planDraftFromReq(in, testConf(), in.PlanCode); err != nil {
		t.Fatalf("prom=0 表示无促销，应放过：%v", err)
	}
	// 总时长正好等于上限（366*10=3660）是合法的年卡叠法，边界不得多裁一天。
	year := validPlanReq()
	year.DurationDays, year.UnitCount = 366, 10
	if _, draft, err := planDraftFromReq(year, testConf(), year.PlanCode); err != nil || draft.TotalDurationDays() != 3660 {
		t.Fatalf("总时长等于上限应放过，实得 (%v,%v)", err != nil, draft)
	}
}

func TestSpecChangedOnlyLooksAtFrozenFields(t *testing.T) {
	existing := &model.Plan{
		VipType: model.VipTypePremium, DurationDays: 31, UnitCount: 1,
		PriceMinor: 3000, PromPriceMinor: 0, Currency: "CNY",
		Name: "旧名", Description: "旧描述", State: model.PlanStateOnSale,
	}
	cases := []struct {
		name   string
		mutate func(*rpc.UpsertPlanReq)
		want   bool
	}{
		{"只改展示名", func(in *rpc.UpsertPlanReq) { in.Name = "新名" }, false},
		{"只改平台可见性", func(in *rpc.UpsertPlanReq) {
			in.Platforms = []rpc.PlanPlatform{rpc.PlanPlatform_PLAN_PLATFORM_WEB}
		}, false},
		{"改价", func(in *rpc.UpsertPlanReq) { in.PriceMinor = 2500 }, true},
		{"改促销价", func(in *rpc.UpsertPlanReq) { in.PromPriceMinor = 100 }, true},
		{"改档位", func(in *rpc.UpsertPlanReq) { in.VipType = rpc.VipType_VIP_TYPE_PREMIUM_PLUS }, true},
		{"改时长", func(in *rpc.UpsertPlanReq) { in.DurationDays = 30 }, true},
		// unit_count 留空 == 1，与已落库的 1 等价，不该被当成改规格。
		{"unit_count 留空等价于 1", func(in *rpc.UpsertPlanReq) { in.UnitCount = 0 }, false},
		{"币种留空沿用原值", func(in *rpc.UpsertPlanReq) { in.Currency = "" }, false},
		{"改币种", func(in *rpc.UpsertPlanReq) { in.Currency = "EUR" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validPlanReq()
			in.VipType = rpc.VipType(existing.VipType)
			in.DurationDays, in.UnitCount, in.PriceMinor = existing.DurationDays, existing.UnitCount, existing.PriceMinor
			in.PromPriceMinor, in.Currency = existing.PromPriceMinor, existing.Currency
			tc.mutate(in)
			if got := specChanged(existing, in); got != tc.want {
				t.Fatalf("specChanged = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// --- 判定口径 ---

func TestDecideEntitlementReasonMatrix(t *testing.T) {
	now := int64(1_700_000_000)
	premium := &model.Entitlement{Code: "vip.skip_ad", MinVipType: model.VipTypePremium, Enabled: 1}
	plus := &model.Entitlement{Code: "vip.4k", MinVipType: model.VipTypePremiumPlus, Enabled: 1}
	off := &model.Entitlement{Code: "vip.legacy", MinVipType: model.VipTypePremium, Enabled: 0}

	activeP := &model.Membership{Mid: 1, VipType: model.VipTypePremium, ExpireAt: now + 10*day}
	activePlus := &model.Membership{Mid: 1, VipType: model.VipTypePremiumPlus, ExpireAt: now + 20*day}
	expiredP := &model.Membership{Mid: 1, VipType: model.VipTypePremium, ExpireAt: now - day}
	boundary := &model.Membership{Mid: 1, VipType: model.VipTypePremium, ExpireAt: now} // 恰好等于 now == 已过期

	cases := []struct {
		name       string
		code       string
		ent        *model.Entitlement
		rows       []*model.Membership
		wantReason rpc.EntitlementReason
		wantGrant  bool
		wantExpire int64
		wantVip    rpc.VipType
	}{
		{
			name: "未知码不放行", code: "vip.nope", ent: nil, rows: []*model.Membership{activePlus},
			wantReason: rpc.EntitlementReason_ENTITLEMENT_CODE_UNKNOWN, wantExpire: now + 20*day, wantVip: rpc.VipType_VIP_TYPE_PREMIUM_PLUS,
		},
		{
			name: "空码等价于未知码", code: "", ent: nil, rows: []*model.Membership{activePlus},
			wantReason: rpc.EntitlementReason_ENTITLEMENT_CODE_UNKNOWN, wantExpire: now + 20*day, wantVip: rpc.VipType_VIP_TYPE_PREMIUM_PLUS,
		},
		{
			name: "下线码与未知码可区分", code: off.Code, ent: off, rows: []*model.Membership{activePlus},
			wantReason: rpc.EntitlementReason_ENTITLEMENT_CODE_DISABLED, wantExpire: now + 20*day, wantVip: rpc.VipType_VIP_TYPE_PREMIUM_PLUS,
		},
		{
			name: "从未开通", code: premium.Code, ent: premium, rows: nil,
			wantReason: rpc.EntitlementReason_ENTITLEMENT_NO_MEMBERSHIP,
		},
		{
			name: "曾开通已过期", code: premium.Code, ent: premium, rows: []*model.Membership{expiredP},
			wantReason: rpc.EntitlementReason_ENTITLEMENT_EXPIRED, wantExpire: now - day, wantVip: rpc.VipType_VIP_TYPE_PREMIUM,
		},
		{
			name: "expire_at == now 视为已过期", code: premium.Code, ent: premium, rows: []*model.Membership{boundary},
			wantReason: rpc.EntitlementReason_ENTITLEMENT_EXPIRED, wantExpire: now, wantVip: rpc.VipType_VIP_TYPE_PREMIUM,
		},
		{
			name: "档位达标即通过", code: premium.Code, ent: premium, rows: []*model.Membership{activeP},
			wantReason: rpc.EntitlementReason_ENTITLEMENT_GRANTED, wantGrant: true, wantExpire: now + 10*day, wantVip: rpc.VipType_VIP_TYPE_PREMIUM,
		},
		{
			// 这是任务口径：生效中但档位不足 => false + TIER_NOT_ENOUGH，并回带实际档位与到期时间。
			name: "生效中但档位不足要回带实际档位", code: plus.Code, ent: plus, rows: []*model.Membership{activeP},
			wantReason: rpc.EntitlementReason_ENTITLEMENT_TIER_NOT_ENOUGH, wantExpire: now + 10*day, wantVip: rpc.VipType_VIP_TYPE_PREMIUM,
		},
		{
			name: "超级大会员拥有大会员全部权益", code: premium.Code, ent: premium, rows: []*model.Membership{activePlus},
			wantReason: rpc.EntitlementReason_ENTITLEMENT_GRANTED, wantGrant: true, wantExpire: now + 20*day, wantVip: rpc.VipType_VIP_TYPE_PREMIUM_PLUS,
		},
		{
			name: "多档取生效中的最高档", code: plus.Code, ent: plus,
			rows:       []*model.Membership{activeP, activePlus},
			wantReason: rpc.EntitlementReason_ENTITLEMENT_GRANTED, wantGrant: true, wantExpire: now + 20*day, wantVip: rpc.VipType_VIP_TYPE_PREMIUM_PLUS,
		},
		{
			name: "低档生效高档过期时按生效档判定", code: plus.Code, ent: plus,
			rows:       []*model.Membership{activeP, expiredP2plus(now)},
			wantReason: rpc.EntitlementReason_ENTITLEMENT_TIER_NOT_ENOUGH, wantExpire: now + 10*day, wantVip: rpc.VipType_VIP_TYPE_PREMIUM,
		},
		{
			name: "nil 行不参与挑选", code: premium.Code, ent: premium, rows: []*model.Membership{nil, activeP},
			wantReason: rpc.EntitlementReason_ENTITLEMENT_GRANTED, wantGrant: true, wantExpire: now + 10*day, wantVip: rpc.VipType_VIP_TYPE_PREMIUM,
		},
		{
			name: "未知码优先于未开通", code: "vip.nope", ent: nil, rows: nil,
			wantReason: rpc.EntitlementReason_ENTITLEMENT_CODE_UNKNOWN,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decideEntitlement(tc.code, tc.ent, tc.rows, now)
			if d.granted != tc.wantGrant {
				t.Fatalf("granted = %v，期望 %v", d.granted, tc.wantGrant)
			}
			if d.reason != tc.wantReason {
				t.Fatalf("reason = %v，期望 %v", d.reason, tc.wantReason)
			}
			if d.expireAt != tc.wantExpire {
				t.Fatalf("expire_at = %d，期望 %d", d.expireAt, tc.wantExpire)
			}
			if d.vipType != tc.wantVip {
				t.Fatalf("vip_type = %v，期望 %v", d.vipType, tc.wantVip)
			}
			if d.code != tc.code {
				t.Fatalf("decision.code = %q，期望 %q", d.code, tc.code)
			}
			// granted=true 与 GRANTED 必须同时成立，不允许出现「通过但无结论码」。
			if d.granted != (d.reason == rpc.EntitlementReason_ENTITLEMENT_GRANTED) {
				t.Fatalf("granted/reason 自相矛盾：%v / %v", d.granted, d.reason)
			}
		})
	}
}

func expiredP2plus(now int64) *model.Membership {
	return &model.Membership{Mid: 1, VipType: model.VipTypePremiumPlus, ExpireAt: now - 2*day}
}

// --- 假实现与生产判定的对照 ---

// TestFakeDuplicatePredicateMatchesModel 防止「假实现自己认一套、生产认另一套」。
// model 的 isDuplicateErr 按报文判定（不 import 驱动专有类型），logic 的幂等分支完全依赖它；
// 假实现若放宽，重放路径会被假通过。
func TestFakeDuplicatePredicateMatchesModel(t *testing.T) {
	cases := []error{
		nil,
		dupErr("uniq_request_id"),
		fmt.Errorf("wrapped: %w", dupErr("uniq_code")),
		errors.New("Duplicate entry 'x' for key 'y'"),
		errors.New("Error 1062: ..."),
		errors.New("connection refused"),
		fmt.Errorf("%w: timeout", model.ErrConcurrentUpdate),
	}
	// 真连接的 IsDuplicate 不碰 conn，可以直接用 nil 构造（不建连、不联网）。
	against := map[string]func(error) bool{
		"Grant":       model.NewGrantModel(nil).IsDuplicate,
		"PlanLog":     model.NewPlanChangeLogModel(nil).IsDuplicate,
		"Entitlement": model.NewEntitlementModel(nil).IsDuplicate,
		"Membership":  model.NewMembershipModel(nil).IsDuplicate,
		"Request":     model.NewBizRequestModel(nil).IsDuplicate,
	}
	for _, err := range cases {
		want := isDuplicateLike(err)
		for name, fn := range against {
			if got := fn(err); got != want {
				t.Errorf("%s.IsDuplicate(%v) = %v，假实现判为 %v", name, err, got, want)
			}
		}
	}
	if !isDuplicateLike(dupErr("PRIMARY")) {
		t.Fatal("mb_biz_request 用主键做幂等键，冲突报文的 key 名是 PRIMARY，必须被认出")
	}
}
