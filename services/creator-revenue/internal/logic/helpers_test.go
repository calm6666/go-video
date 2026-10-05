package logic

// 本文件锁 helpers.go 的纯函数：入参闸门（宽度/必填/幂等键留白）、
// 行→RPC 投影的逐字段一致（投影漏字段是「接口没报错但运营看不到」那一类最贵的 bug），
// 以及裁尾函数必须落在字符边界上。

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"

	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------- 身份与文本闸门

func TestIsSystemOperator(t *testing.T) {
	for _, op := range []string{"cron", " spm ", "creatorrevenue"} {
		if !IsSystemOperator(op) {
			t.Errorf("%q 应被判成系统回填身份", op)
		}
	}
	// 运营工号必须留原因：它是「人为判断」，不是事实搬运。
	for _, op := range []string{"ops-77", "user", "", "  ", "CRON", "cronx"} {
		if IsSystemOperator(op) {
			t.Errorf("%q 不该被判成系统身份", op)
		}
	}
}

func TestCheckTextOnlyJudgesWidth(t *testing.T) {
	if err := checkText("name", strings.Repeat("a", 64), 64); err != nil {
		t.Fatalf("刚好等于列宽必须放行：%v", err)
	}
	err := checkText("name", strings.Repeat("a", 65), 64)
	mustErrIs(t, err, model.ErrTextTooLong)
	if !strings.Contains(err.Error(), "name 长度 65 超过列宽 64") {
		t.Fatalf("错误要指出是哪个字段与两个数字：%v", err)
	}
	// 空白串只管长度，必填由各自判定负责（否则 requireOperator 的提示会被它抢走）。
	if err := checkText("name", "   ", 2); err != nil {
		t.Fatalf("空白串不该在这里被拒：%v", err)
	}
}

func TestRequireOperatorReasonRequestID(t *testing.T) {
	op, err := requireOperator("  ops-77  ")
	mustNoErr(t, err)
	if op != "ops-77" {
		t.Fatalf("身份必须去掉首尾空格后返回：%q", op)
	}
	if _, err := requireOperator(""); !errors.Is(err, model.ErrOperatorRequired) {
		t.Fatalf("空身份必须拒：%v", err)
	}
	if _, err := requireOperator(strings.Repeat("o", model.MaxOperatorBytes+1)); !errors.Is(err, model.ErrTextTooLong) {
		t.Fatalf("超列宽身份必须拒：%v", err)
	}

	r, err := requireReason(" 台账已复核 ")
	mustNoErr(t, err)
	if r != "台账已复核" {
		t.Fatalf("原因未归一：%q", r)
	}
	if _, err := requireReason("   "); !errors.Is(err, model.ErrReasonRequired) {
		t.Fatalf("空原因必须拒：%v", err)
	}
	// 超长与缺失必须是两个不同哨兵：调用方「写了但太长」不能被翻译成「没写」。
	longErr, err := requireReason(strings.Repeat("理", model.MaxReasonBytes+1))
	if longErr != "" || !errors.Is(err, model.ErrTextTooLong) || errors.Is(err, model.ErrReasonRequired) {
		t.Fatalf("超长原因必须是 ErrTextTooLong，实际 %v / %q", err, longErr)
	}

	if _, err := requireRequestID("  ", model.MaxRequestIDBytes); !errors.Is(err, model.ErrRequestIDRequired) {
		t.Fatalf("空幂等键必须拒：%v", err)
	}
	if _, err := requireRequestID(strings.Repeat("r", 65), 64); !errors.Is(err, model.ErrTextTooLong) {
		t.Fatalf("超长幂等键必须拒：%v", err)
	}
}

func TestRequireScopedRequestIDReservesSuffixWidth(t *testing.T) {
	const suffix = 27
	// settleKeySuffixBytes 的算术：'#'1 + period6 + '#'1 + int64 最坏 19 位 = 27。
	if v := 1 + model.MaxPeriodBytes + 1 + 19; v != suffix {
		t.Fatalf("后缀预算与常量不符：%d", v)
	}
	ok := strings.Repeat("r", model.MaxRequestIDBytes-suffix)
	if _, err := requireScopedRequestID(ok, suffix); err != nil {
		t.Fatalf("边界宽度被误拒：%v", err)
	}
	if _, err := requireScopedRequestID(ok+"r", suffix); !errors.Is(err, model.ErrTextTooLong) {
		t.Fatalf("多 1 字节必须拒（否则 INSERT 才在 VARCHAR(64) 上撞截断）：%v", err)
	}
	// 后缀预算吃掉了几乎所有列宽时，也要给父键留 8 字节可用空间（否则入口永远过不去）。
	if _, err := requireScopedRequestID("12345678", 60); err != nil {
		t.Fatalf("下限 8 字节未生效：%v", err)
	}
	if _, err := requireScopedRequestID("123456789", 60); !errors.Is(err, model.ErrTextTooLong) {
		t.Fatalf("超过下限仍要拒：%v", err)
	}
}

func TestEnumAndIDValidators(t *testing.T) {
	for _, v := range []int32{
		model.SettlementStateUnspecified, model.SettlementStateDraft,
		model.SettlementStateConfirmed, model.SettlementStateVoided,
	} {
		if err := validSettlementStateFilter(v); err != nil {
			t.Errorf("状态 %d 应在过滤范围内：%v", v, err)
		}
	}
	// 越界状态回错误而不是空名单：运营拿「查不到」去催确认会催错方向。
	for _, v := range []int32{-1, model.SettlementStateVoided + 1} {
		if err := validSettlementStateFilter(v); !errors.Is(err, model.ErrInvalidRuleState) {
			t.Errorf("状态 %d 必须拒，实际 %v", v, err)
		}
	}
	for _, v := range []int32{model.SourceTypeVipWatch, model.SourceTypeCoin,
		model.SourceTypeInteraction, model.SourceTypeActivity} {
		if err := validSourceType(v); err != nil {
			t.Errorf("来源 %d 合法却回错：%v", v, err)
		}
	}
	for _, v := range []int32{model.SourceTypeUnspecified, model.SourceTypeActivity + 1, -2} {
		if err := validSourceType(v); !errors.Is(err, model.ErrInvalidSourceType) {
			t.Errorf("来源 %d 必须拒，实际 %v", v, err)
		}
	}

	mid, err := normalizeMid(42)
	mustNoErr(t, err)
	if mid != 42 {
		t.Fatalf("mid 未透传：%d", mid)
	}
	// 分成人必须是真实账号：0 在全量模式里表示「不限作者」，不能混进写路径。
	for _, v := range []int64{0, -1} {
		if _, err := normalizeMid(v); !errors.Is(err, model.ErrInvalidMid) {
			t.Errorf("mid=%d 必须拒，实际 %v", v, err)
		}
	}
	// aid=0 合法（活动激励不挂具体内容），负数非法。
	for _, v := range []int64{0, 7} {
		got, err := normalizeAid(v)
		mustNoErr(t, err)
		if got != v {
			t.Fatalf("aid=%d 未透传：%d", v, got)
		}
	}
	if _, err := normalizeAid(-1); !errors.Is(err, model.ErrInvalidAid) {
		t.Fatalf("负 aid 必须拒，实际 %v", err)
	}
}

func TestPeriodToNumber(t *testing.T) {
	if got := periodToNumber("202601"); got != 202601 {
		t.Fatalf("YYYYMM 应折成 202601，实际 %d", got)
	}
	if got := periodToNumber("2026-01"); got != 202601 {
		t.Fatalf("带分隔符的周期码 ValidatePeriod 认，数值化也要认，实际 %d", got)
	}
	// 0 的语义是「从没出过单」，所以非法周期必须折成 0 而不是脏数字。
	for _, p := range []string{"", "000000", "202613", "abcdef", "20260"} {
		if got := periodToNumber(p); got != 0 {
			t.Errorf("period=%q 应折成 0，实际 %d", p, got)
		}
	}
}

// ---------------------------------------------------------------- 裁尾

func TestTruncFitsWidth(t *testing.T) {
	if got := trunc("abc", 8); got != "abc" {
		t.Fatalf("未超宽不该被改：%q", got)
	}
	if got := trunc("abcdef", 3); got != "abc" {
		t.Fatalf("ASCII 裁尾错误：%q", got)
	}
	if got := trunc("abcdef", 0); got != "" {
		t.Fatalf("max=0 应回空串：%q", got)
	}
}

// TestTruncNeverSplitsARune 钉住「裁尾只能落在字符边界」。
// 被裁的都是中文说明文本（作废原因、计算依据摘要、参与备注）：
// 半截 UTF-8 序列写进 utf8mb4 列会被驱动判成 Incorrect string value，
// 整条 INSERT 失败，而报错点离「一句备注太长」这个根因非常远。
func TestTruncNeverSplitsARune(t *testing.T) {
	src := strings.Repeat("分成口径说明", 100) // 每字 3 字节
	if len(src)%3 != 0 {
		t.Fatal("种子文本假设被破坏")
	}
	for max := 1; max <= 20; max++ {
		got := trunc(src, max)
		if len(got) > max {
			t.Fatalf("max=%d 裁出来仍超宽：%d 字节", max, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("max=%d 裁进了字符内部，产出非法 UTF-8：%q", max, got)
		}
		if utf8.RuneCountInString(got) != max/3 {
			t.Fatalf("max=%d 应保留 %d 个完整汉字，实际 %d：%q",
				max, max/3, utf8.RuneCountInString(got), got)
		}
	}
	// 混合文本：ASCII 前缀 + 中文，裁点落在汉字中间同样要退到边界。
	mixed := "reason:" + strings.Repeat("理", 10)
	for max := 8; max < 15; max++ {
		if got := trunc(mixed, max); !utf8.ValidString(got) {
			t.Fatalf("max=%d 产出非法 UTF-8：%q", max, got)
		}
	}
}

// TestMetricSourceDetailFitsColumn 是 trunc 的另一条真实调用路径：
// 调用方给的摘要 + 「被门槛拦掉」后缀一定超过 512 字节，
// 而这段文本要落进 cr_metric.source_detail（utf8mb4）参与复核。
func TestMetricSourceDetailFitsColumn(t *testing.T) {
	d := &metricDraft{SourceDetail: strings.Repeat("观", model.MaxSourceDetailBytes/2)}
	rule := ruleWithCurrency("R1", model.SourceTypeVipWatch, "CNY")
	rule.MinQuantity = 100

	got := metricSourceDetail(d, rule, 0, true)
	if len(got) > model.MaxSourceDetailBytes {
		t.Fatalf("摘要超出 VARCHAR(%d)：%d 字节", model.MaxSourceDetailBytes, len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("摘要被裁成非法 UTF-8：%q", got)
	}
	// 没给摘要时按公式渲染一句人话，保证复核能反推算法。
	auto := metricSourceDetail(&metricDraft{Quantity: 9000}, rule, 10800, false)
	if !strings.Contains(auto, "quantity=9000") || !strings.Contains(auto, "= 10800 分") {
		t.Fatalf("公式摘要缺信息：%q", auto)
	}
}

// ---------------------------------------------------------------- 行→RPC 投影

func TestRuleInfoProjectsEveryColumn(t *testing.T) {
	if ruleInfo(nil) != nil || enrollmentInfo(nil) != nil || metricInfo(nil) != nil ||
		settlementInfo(nil) != nil {
		t.Fatal("nil 行必须投影成 nil，不能回一个零值结构体冒充数据")
	}
	row := &model.RevenueRule{
		RuleId: 7, RuleCode: "R7", SourceType: model.SourceTypeCoin, Name: "投币分成",
		Description: "有效投币", UnitPricePer1000: 1200, Currency: "CNY", Unit: "coin",
		MinQuantity: 10, MonthlyCapMinor: 500000, State: model.RuleStateActive,
		EffectiveFrom: 1767225600, Version: 4, Ctime: 111, Mtime: 222,
		CreatedBy: "ops-01", UpdatedBy: "ops-02",
	}
	got := ruleInfo(row)
	want := &rpc.RevenueRuleInfo{
		RuleId: 7, RuleCode: "R7", SourceType: rpc.RevenueSourceType_REVENUE_SOURCE_COIN, Name: "投币分成",
		Description: "有效投币", UnitPricePer_1000Minor: 1200, Currency: "CNY", Unit: "coin",
		MinQuantity: 10, MonthlyCapMinor: 500000, State: rpc.RuleState_RULE_STATE_ACTIVE,
		EffectiveFrom: 1767225600, Version: 4, Ctime: 111, Mtime: 222,
		CreatedBy: "ops-01", UpdatedBy: "ops-02",
	}
	if !proto.Equal(got, want) {
		t.Fatalf("规则投影字段不符：\n got %+v\nwant %+v", got, want)
	}
}

func TestEnrollmentAndMetricProjections(t *testing.T) {
	enr := enrollmentInfo(&model.Enrollment{
		Mid: 100, State: model.EnrollmentStateLeft, AgreedRuleVersion: 3,
		EnrolledAt: 111, LeftAt: 222, Mtime: 333, Operator: "ops-77", Remark: "本人申请退出",
	})
	if enr.Mid != 100 || enr.State != rpc.EnrollmentState_ENROLLMENT_STATE_LEFT || enr.AgreedRuleVersion != 3 ||
		enr.EnrolledAt != 111 || enr.LeftAt != 222 || enr.UpdatedAt != 333 ||
		enr.Operator != "ops-77" || enr.Remark != "本人申请退出" {
		t.Fatalf("参与关系投影错误：%+v", enr)
	}
	// corrected 只存在于 DB 侧（契约未暴露该位），投影不得凭空造字段。

	m := metricInfo(&model.RevenueMetric{
		MetricId: 9, Period: "202601", Mid: 100, Aid: 11, SourceType: model.SourceTypeVipWatch,
		RuleCode: "R1", RuleVersion: 4, Quantity: 9000, Unit: "minute",
		AmountMinor: 1000, CappedAmountMinor: 800, SourceDetail: "公式", Ctime: 1, Mtime: 2,
	})
	if m.MetricId != 9 || m.Period != "202601" || m.Mid != 100 || m.Aid != 11 ||
		m.SourceType != rpc.RevenueSourceType_REVENUE_SOURCE_VIP_WATCH || m.RuleCode != "R1" || m.RuleVersion != 4 ||
		m.Quantity != 9000 || m.Unit != "minute" || m.AmountMinor != 1000 ||
		m.CappedAmountMinor != 800 || m.SourceDetail != "公式" || m.Ctime != 1 || m.Mtime != 2 {
		t.Fatalf("台账投影错误：%+v", m)
	}
}

func TestSettlementProjectionsKeepAmountSemantics(t *testing.T) {
	s := settlementInfo(&model.Settlement{
		SettlementNo: "CRS202601-100-1", Period: "202601", Mid: 100,
		AmountMinor: 1300, CapAppliedMinor: 200, Currency: "CNY", MetricCount: 3,
		State: model.SettlementStateConfirmed, PayoutState: model.PayoutStateNotPayable,
		ConfirmedAt: 555, ConfirmedBy: "ops-77", VoidReason: "", Ctime: 1, Mtime: 2,
	})
	if s.SettlementNo != "CRS202601-100-1" || s.AmountMinor != 1300 || s.CapAppliedMinor != 200 ||
		s.MetricCount != 3 || s.State != rpc.SettlementState_SETTLEMENT_STATE_CONFIRMED ||
		s.PayoutState != rpc.PayoutState_PAYOUT_STATE_NOT_PAYABLE ||
		s.ConfirmedAt != 555 || s.ConfirmedBy != "ops-77" {
		t.Fatalf("结算单投影错误：%+v", s)
	}
	items := settlementItemInfos([]*model.SettlementItem{
		{SourceType: model.SourceTypeVipWatch, RuleCode: "R1", Quantity: 9000, AmountMinor: 800},
	})
	if len(items) != 1 || items[0].RuleCode != "R1" || items[0].AmountMinor != 800 ||
		items[0].SourceType != rpc.RevenueSourceType_REVENUE_SOURCE_VIP_WATCH {
		t.Fatalf("分项投影错误：%+v", items)
	}
}

// TestListProjectionsNeverReturnNil 锁读接口的共用口径：
// 空结果投影成非 nil 空数组，网关才能区分「库里没有」与「上游没查」。
func TestListProjectionsNeverReturnNil(t *testing.T) {
	if got := ruleInfos(nil); got == nil || len(got) != 0 {
		t.Fatalf("ruleInfos 空结果必须是空数组：%v", got)
	}
	if got := enrollmentInfos(nil); got == nil || len(got) != 0 {
		t.Fatalf("enrollmentInfos 空结果必须是空数组：%v", got)
	}
	if got := metricInfos(nil); got == nil || len(got) != 0 {
		t.Fatalf("metricInfos 空结果必须是空数组：%v", got)
	}
	if got := settlementInfos(nil); got == nil || len(got) != 0 {
		t.Fatalf("settlementInfos 空结果必须是空数组：%v", got)
	}
	if got := settlementItemInfos(nil); got == nil || len(got) != 0 {
		t.Fatalf("settlementItemInfos 空结果必须是空数组：%v", got)
	}
	// 列表里混进 nil 行（model 读不到）也不能投影出带空洞的数组。
	if got := metricInfos([]*model.RevenueMetric{nil, {MetricId: 1}}); len(got) != 2 || got[0] != nil {
		t.Fatalf("投影保持与输入同行数的口径变了：%+v", got)
	}
}

// ---------------------------------------------------------------- 分页护栏

func TestPageSizeFolding(t *testing.T) {
	ctx, _ := newTestSvc(t)
	p := ctx.PageSize(1, 10)
	if p.RequestedPage != 1 || p.RequestedSize != 10 || p.Offset != 0 || p.Limit != 10 {
		t.Fatalf("正常分页被改了：%+v", p)
	}
	p = ctx.PageSize(3, 10)
	if p.Offset != 20 || p.Limit != 10 {
		t.Fatalf("物理分页位错误：%+v", p)
	}
	// 页宽越界折到上限而不是拒：列表页翻过头是导航问题，写坏 SQL 才是事故。
	p = ctx.PageSize(0, 10_000)
	if p.RequestedPage != 1 || p.RequestedSize != 100 || p.Limit != 100 {
		t.Fatalf("页宽未被 MaxPageSize 夹住：%+v", p)
	}
	p = ctx.PageSize(-5, 0)
	if p.RequestedPage != 1 || p.RequestedSize != 100 {
		t.Fatalf("非正页码/页宽未兜底：%+v", p)
	}
	// MaxPageSize 漏配也不能变成无界查询。
	ctx.Config.CreatorRevenue.MaxPageSize = 0
	if got := ctx.PageSize(1, 5000).RequestedSize; got != 100 {
		t.Fatalf("漏配时默认页宽应为 100，实际 %d", got)
	}
	if got := (svc.Page{}); got.Limit != 0 {
		t.Fatalf("零值 Page 不该自带 limit：%+v", got)
	}
	if got := svc.NewPage(2, 20, 30); got.Offset != 20 || got.Limit != 20 {
		t.Fatalf("NewPage 纯函数口径变了：%+v", got)
	}
}
