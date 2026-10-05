package model

// 本文件覆盖 model 包里「不需要数据库就能证明」的判定：枚举值域、存储形态往返、
// 灰度命中矩阵、规则形态校验、版本指针单调性、分页与限额。
//
// 为什么这些值得单独钉住（AGENTS.md §5/§8/§9）：
//   - 灰度判定与形态校验是写侧与读侧共用的**唯一实现**，一旦两侧口径分叉，
//     控制台展示的放量范围与真实命中人群就会不是同一批人；
//   - ",1,2," 这类存储形态的正确性没有编译期保障，只能靠用例锁住
//     （漏了首尾逗号的实现不会报错，只会让「按端筛规则」静默漏行）；
//   - 版本指针只能前进（回滚也是追加新版本号），是审计可解释的前提。
//
// 约定：对「校验先于 SQL」的写入函数，本文件用 nil session 只走拒绝路径 ——
// 一旦实现被改成先执行 SQL 后校验，用例会当场 panic 而不是静默通过。

import (
	"context"
	"errors"
	"hash/crc32"
	"strconv"
	"strings"
	"testing"
)

// --- 枚举值域 ---

func TestEnumPredicatesRejectZeroAndUnknown(t *testing.T) {
	// 0 值一律不是合法枚举：它是 proto 的「未填」哨兵，把 0 当合法值等于
	// 让漏填变成一种业务语义（本项目里最危险的就是「不限端」）。
	if ValidPlatform(0) || ValidRolloutMode(0) || ValidValueType(0) || ValidState(0) {
		t.Error("0 值不得是任何枚举的合法取值")
	}
	if !ValidPlatform(PlatformAndroid) || !ValidPlatform(PlatformDesktop) {
		t.Error("四端首尾取值必须合法")
	}
	// 本项目只有 Android/iOS/HarmonyOS/桌面四端，没有小程序（AGENTS.md §1、§6）：
	// 第五个取值必须被判定为非法，绝不能被当成「不限端」放过。
	for _, p := range []int32{-1, 5, 6, 42, 100} {
		if ValidPlatform(p) {
			t.Errorf("ValidPlatform(%d) 必须为 false（不存在第五端/小程序端）", p)
		}
		if ScopeOfPlatform(p) != "" {
			t.Errorf("ScopeOfPlatform(%d) 必须回空串，由调用方报 ErrPlatformUnknown", p)
		}
	}
	if ValidState(StateOn) == false || ValidState(StateOff) == false {
		t.Error("1/2 必须是合法启停状态")
	}
	for _, s := range []int32{3, -1} {
		if ValidState(s) {
			t.Errorf("ValidState(%d) 必须为 false", s)
		}
	}
	for _, v := range []int32{0, 5, -1} {
		if ValidValueType(v) {
			t.Errorf("ValidValueType(%d) 必须为 false", v)
		}
	}
	for _, m := range []int32{0, 7, -1} {
		if ValidRolloutMode(m) {
			t.Errorf("ValidRolloutMode(%d) 必须为 false", m)
		}
	}
}

func TestValidScopeExcludesMiniProgram(t *testing.T) {
	for _, s := range []string{ScopeGlobal, ScopeAndroid, ScopeIOS, ScopeHarmony, ScopeDesktop} {
		if !ValidScope(s) {
			t.Errorf("scope %q 应合法", s)
		}
	}
	// 空串由调用方归一为 global 后再判定；weixin/mp/miniprogram 都不是本项目的端。
	for _, s := range []string{"", "weixin", "mp", "miniprogram", "web", "h5", "ANDROID"} {
		if ValidScope(s) {
			t.Errorf("scope %q 必须被拒绝", s)
		}
	}
}

func TestValidItemTypeTopicOnlyInSlot(t *testing.T) {
	for _, it := range []string{ItemTypeUGCVideo, ItemTypePGCSeason, ItemTypePGCEpisode} {
		if !ValidItemType(it, false) || !ValidItemType(it, true) {
			t.Errorf("item_type %q 在专题与坑位里都必须合法", it)
		}
	}
	// 专题内挂专题会形成自引用环（网关递归展开会死），只有坑位允许 topic。
	if ValidItemType(ItemTypeTopic, false) {
		t.Error("专题内不允许 topic")
	}
	if !ValidItemType(ItemTypeTopic, true) {
		t.Error("坑位内允许 topic")
	}
	for _, it := range []string{"", "live_room", "article", "ad", "UGC_VIDEO", "mini_program"} {
		if ValidItemType(it, true) {
			t.Errorf("item_type %q 必须被拒绝", it)
		}
	}
}

func TestModeNameIsTotalAndNeverEmpty(t *testing.T) {
	cases := map[int32]string{
		ModeFull: "full", ModePercentage: "percentage", ModeAppVersion: "app_version",
		ModePlatform: "platform", ModeMidSuffix: "mid_suffix", ModeWhitelist: "whitelist",
		ModeUnspecified: "unspecified", 99: "unspecified",
	}
	for mode, want := range cases {
		if got := ModeName(mode); got != want {
			t.Errorf("ModeName(%d)=%q，期望 %q", mode, got, want)
		}
	}
}

func TestScopeOfPlatformMapsFourPlatforms(t *testing.T) {
	want := map[int32]string{
		PlatformAndroid: ScopeAndroid, PlatformIOS: ScopeIOS,
		PlatformHarmony: ScopeHarmony, PlatformDesktop: ScopeDesktop,
	}
	for p, s := range want {
		if got := ScopeOfPlatform(p); got != s {
			t.Errorf("ScopeOfPlatform(%d)=%q，期望 %q", p, got, s)
		}
		// scope 标识必须落在 ValidScope 里，否则发布出去的键永远查不到。
		if !ValidScope(s) {
			t.Errorf("scope %q 不在允许集合内", s)
		}
	}
}

// --- 端列表 / ID 列表的存储形态 ---

func TestNormalizePlatformListFormAndOrder(t *testing.T) {
	cases := []struct {
		name  string
		in    []int32
		want  string
		wantE error
	}{
		{"空列表=不限端", nil, "", nil},
		{"单端", []int32{PlatformIOS}, ",2,", nil},
		{"乱序去重后升序", []int32{4, 1, 4, 1}, ",1,4,", nil},
		{"四端全集", []int32{2, 1, 4, 3}, ",1,2,3,4,", nil},
		{"未声明端混进列表", []int32{0}, "", ErrPlatformUnknown},
		{"第五端/小程序", []int32{5}, "", ErrPlatformUnknown},
		{"负数", []int32{1, -2}, "", ErrPlatformUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NormalizePlatformList(c.in)
			if !errors.Is(err, c.wantE) {
				t.Fatalf("err=%v，期望 %v", err, c.wantE)
			}
			if got != c.want {
				t.Errorf("存储形态=%q，期望 %q", got, c.want)
			}
			if c.wantE == nil && got != "" {
				// 首尾逗号是 LIKE 精确匹配的前提：缺了就等于把 1 匹配到 12/21。
				if !strings.HasPrefix(got, ",") || !strings.HasSuffix(got, ",") {
					t.Errorf("存储形态 %q 必须首尾各带逗号", got)
				}
			}
		})
	}
}

func TestValidatePlatformListRejectsUnknownNotDropsIt(t *testing.T) {
	// 校验路径上「未知端」必须报错而不是静默丢弃：
	// 丢弃等于把一条脏数据变成「不限端」，那会把某端的放量推给四端。
	if _, err := ValidatePlatformList(",1,9,", 4); !errors.Is(err, ErrPlatformUnknown) {
		t.Errorf("脏端值应报 ErrPlatformUnknown，得到 %v", err)
	}
	if _, err := ValidatePlatformList(",0,", 4); !errors.Is(err, ErrPlatformUnknown) {
		t.Errorf("0 端应报 ErrPlatformUnknown，得到 %v", err)
	}
	// 超过四端的列表说明数据被写坏。
	if _, err := ValidatePlatformList(",1,2,3,4,", 4); err != nil {
		t.Errorf("四端全集应通过，得到 %v", err)
	}
	if _, err := ValidatePlatformList(",1,2,3,4,", 3); !errors.Is(err, ErrPlatformUnknown) {
		t.Errorf("超过 maxCount 应被拒绝，得到 %v", err)
	}
	if got, err := ValidatePlatformList("", 4); err != nil || len(got) != 0 {
		t.Errorf("空列表=不限端，期望 (0,nil)，得到 (%v,%v)", got, err)
	}
}

func TestPlatformContainmentAndVisibilitySemantics(t *testing.T) {
	const stored = ",1,4,"
	if !PlatformListContains(stored, PlatformAndroid) || !PlatformListContains(stored, PlatformDesktop) {
		t.Error("列表内的端必须命中")
	}
	if PlatformListContains(stored, PlatformIOS) {
		t.Error("列表外的端不得命中")
	}
	// 未声明端（0）不等于全命中：漏传一次就把配置推给所有端是最贵的误判。
	if PlatformListContains(stored, 0) || PlatformListContains("", PlatformAndroid) {
		t.Error("platform=0 与空列表在「含端」判定里都必须是 false")
	}
	if PlatformListContains(",12,", PlatformAndroid) {
		t.Error("脏数据 ,12, 不得命中端 1（首尾逗号的意义所在）")
	}
	// 坑位可见性：空列表=不限端，对任何端可见；但请求未声明端时只回「不限端」的行。
	if !PlatformMatchesAny("", PlatformIOS) || !PlatformMatchesAny("", 0) {
		t.Error("不限端的坑位必须对四端与未声明端都可见")
	}
	if PlatformMatchesAny(stored, 0) {
		t.Error("请求未声明端时不得命中「限端」的坑位")
	}
	if !PlatformMatchesAny(stored, PlatformAndroid) || PlatformMatchesAny(stored, PlatformIOS) {
		t.Error("限端坑位的可见性判定必须与 PlatformListContains 一致")
	}
	// 内存判定与 SQL LIKE 参数必须同口径，否则「后台筛出来的端」≠「运行时命中的端」。
	if got := PlatformLikeArg(PlatformHarmony); got != "%,3,%" {
		t.Errorf("PlatformLikeArg(3)=%q，期望 %q", got, "%,3,%")
	}
	if got := IDListLike(42); got != "%,42,%" {
		t.Errorf("IDListLike(42)=%q", got)
	}
}

func TestIDListRoundTripAndDedup(t *testing.T) {
	stored, err := ValidateIDList([]int64{7, 3, 7, 0, -5, 3}, 0)
	if err != nil {
		t.Fatalf("ValidateIDList: %v", err)
	}
	if len(stored) != 2 || stored[0] != 7 || stored[1] != 3 {
		t.Fatalf("ValidateIDList 应去零、去负、去重且保序，得到 %v", stored)
	}
	s := IDListString(stored)
	if s != ",7,3," {
		t.Errorf("通用 ID 列表保序不排序，应为 ,7,3,，得到 %q", s)
	}
	back := ParseIDList(s)
	if len(back) != 2 || back[0] != 7 || back[1] != 3 {
		t.Errorf("往返失败: %v", back)
	}
	if IDListString(nil) != "" || IDListString([]int64{0}) != "" {
		t.Error("空列表必须存成空串（空串=不限/无引用），不能存 \",,\"")
	}
	if IDListContains(s, 3) != true || IDListContains(s, 7) == false {
		t.Error("IDListContains 判定错误")
	}
	// 前缀包含陷阱：3 不得被 37 命中。
	if IDListContains(",37,", 3) {
		t.Error(",37, 不得命中 3")
	}
	if IDListContains(s, 0) || IDListContains(s, -1) {
		t.Error("非正 ID 不得命中")
	}
	// 脏片段被丢弃而不是报错（读侧要能对历史脏数据稳健）。
	if got := ParseIDList(",1,abc,,4,"); len(got) != 2 {
		t.Errorf("ParseIDList 应丢掉脏片段: %v", got)
	}
	// 上限：数量与入库长度双重限制，且数量先判。
	if _, err := ValidateIDList(make([]int64, 0), 0); err != nil {
		t.Errorf("空列表应通过: %v", err)
	}
	many := make([]int64, MaxWhitelistMids+1)
	for i := range many {
		many[i] = int64(i + 1)
	}
	if _, err := ValidateIDList(many, MaxWhitelistMids); !errors.Is(err, ErrBatchTooLarge) {
		t.Errorf("超过 maxCount 应报 ErrBatchTooLarge，得到 %v", err)
	}
}

func TestNormalizeMidSuffixes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"  ", ""},
		{"7", "7"},
		{"9,0,3", "0,3,9"},
		{" 3 , 3 ,1 ", "1,3"},
		{"0,1,2,3,4,5,6,7,8,9", "0,1,2,3,4,5,6,7,8,9"},
	}
	for _, c := range cases {
		got, err := NormalizeMidSuffixes(c.in)
		if err != nil {
			t.Fatalf("NormalizeMidSuffixes(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("NormalizeMidSuffixes(%q)=%q，期望 %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"10", "a", "1,,2", "1;", "-1", "0,", "1 2"} {
		if _, err := NormalizeMidSuffixes(bad); !errors.Is(err, ErrMidSuffixInvalid) {
			t.Errorf("NormalizeMidSuffixes(%q) 应报 ErrMidSuffixInvalid，得到 %v", bad, err)
		}
	}
}

func TestMidSuffixHitUsesDecimalLastDigit(t *testing.T) {
	if !MidSuffixHit(10, "0") || !MidSuffixHit(107, "7") || MidSuffixHit(107, "0") {
		t.Error("尾号命中必须按十进制末位判定")
	}
	// mid<=0（未登录）不参与任何按人抽样。
	for _, mid := range []int64{0, -1, -100} {
		if MidSuffixHit(mid, "0,1,2,3,4,5,6,7,8,9") {
			t.Errorf("mid=%d 不得命中尾号", mid)
		}
	}
	if MidSuffixHit(10, "") {
		t.Error("空尾号列表不得命中")
	}
	if !WhitelistHit(42, ",41,42,43,") || WhitelistHit(4, ",41,42,") {
		t.Error("白名单命中错误（含前缀包含陷阱）")
	}
	if WhitelistHit(0, ",0,") || WhitelistHit(-1, ",-1,") {
		t.Error("未登录不得命中白名单")
	}
}

// --- App 版本比较 ---

func TestCompareAppVersionIsNumericNotLexicographic(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"7.10.0", "7.2.0", 1},  // 字符串序会给出 -1，那会把新版本判成旧版本
		{"7.2.0", "7.10.0", -1}, //
		{"7.2", "7.2.0", 0},     // 缺段按 0 补
		{"7.2.1", "7.2", 1},
		{"1.0.0.1", "1.0.0", 1},
		{"4", "4", 0},
	}
	for _, c := range cases {
		got, ok := CompareAppVersion(c.a, c.b)
		if !ok {
			t.Fatalf("CompareAppVersion(%q,%q) 应为合法输入", c.a, c.b)
		}
		if got != c.want {
			t.Errorf("CompareAppVersion(%q,%q)=%d，期望 %d", c.a, c.b, got, c.want)
		}
	}
	for _, bad := range []struct{ a, b string }{
		{"7.2-beta", "7.0"}, {"", "1.0"}, {"1..2", "1.0"}, {"v1.0", "1.0"},
		{"1.2.3.4.5", "1.0"}, {"1.x", "1.0"}, {"1.0-", "1.0"}, {".1.0", "1.0"},
	} {
		if _, ok := CompareAppVersion(bad.a, bad.b); ok {
			t.Errorf("CompareAppVersion(%q,%q) 必须判定为非法（不能按字符串序兜底）", bad.a, bad.b)
		}
	}
	// 首尾空白是被容忍的（端上偶尔带上），但必须仍按数值比较而不是字符串比较。
	if got, ok := CompareAppVersion(" 1.0 ", "1.0"); !ok || got != 0 {
		t.Errorf("CompareAppVersion(\" 1.0 \",\"1.0\")=(%d,%v)，期望 (0,true)", got, ok)
	}
	if got, ok := CompareAppVersion("1.10", "1.9"); !ok || got <= 0 {
		t.Errorf("1.10 必须大于 1.9（数值段比较，不是字典序），实得 (%d,%v)", got, ok)
	}
}

func TestAppVersionInRange(t *testing.T) {
	if !AppVersionInRange("7.0.0", "", "") {
		t.Error("两侧都不设限时任何版本都命中")
	}
	// 版本未知/非法时宁可不放量。
	if AppVersionInRange("", "7.0.0", "") || AppVersionInRange("bad", "", "7.0.0") {
		t.Error("未知版本在设了边界时必须不命中")
	}
	// 闭区间：边界本身命中。
	if !AppVersionInRange("7.0.0", "7.0.0", "7.9.9") || !AppVersionInRange("7.9.9", "7.0.0", "7.9.9") {
		t.Error("区间必须是闭区间")
	}
	if AppVersionInRange("6.9.9", "7.0.0", "7.9.9") || AppVersionInRange("7.10.0", "7.0.0", "7.9.9") {
		t.Error("区间外的版本必须不命中")
	}
	if !AppVersionInRange("7.2.0", "7.1.0", "") || AppVersionInRange("7.0.0", "7.1.0", "") {
		t.Error("只设下界时的判定错误")
	}
}

// --- 百分比分桶 ---

// TestPercentageBucketMatchesDocumentedFormula 把分桶算法钉死在文档承诺的公式上：
// bucket = crc32(IEEE)(cfg_key + ":" + mid) % 100。
// 用 hash/crc32 在测试里独立重算，是为了防止有人把实现换成 fnv/murmur ——
// 那会让「排障脚本按 Python 复算同一批人」当场失真。
func TestPercentageBucketMatchesDocumentedFormula(t *testing.T) {
	const cfgKey = "home.topic.enabled"
	for _, mid := range []int64{1, 42, 1000003, 9876543210} {
		want := int(crc32.ChecksumIEEE([]byte(cfgKey+":"+strconv.FormatInt(mid, 10)))) % BucketCount
		if got := PercentageBucket(cfgKey, mid); got != want {
			t.Errorf("mid=%d 分桶=%d，公式期望 %d", mid, got, want)
		}
	}
	if BucketCount != 100 {
		t.Errorf("桶数固定 100（percentage 就是百分比），得到 %d", BucketCount)
	}
	// 未登录不参与分桶：匿名流量一起被放量会让灰度失去意义。
	for _, mid := range []int64{0, -1, -99} {
		if got := PercentageBucket(cfgKey, mid); got != -1 {
			t.Errorf("mid=%d 必须回 -1，得到 %d", mid, got)
		}
	}
}

func TestPercentageBucketIsDeterministicAndMixedByKey(t *testing.T) {
	const cfgA, cfgB = "cfg.a", "cfg.b"
	for _, mid := range []int64{1, 7, 99, 123456} {
		first := PercentageBucket(cfgA, mid)
		for i := 0; i < 32; i++ {
			if got := PercentageBucket(cfgA, mid); got != first {
				t.Fatalf("同一输入必须同一结果：mid=%d 第 %d 次得到 %d，首次 %d", mid, i, got, first)
			}
		}
		if first < 0 || first >= BucketCount {
			t.Errorf("mid=%d 的桶号 %d 越界", mid, first)
		}
	}
	// 混入 cfg_key 的理由：所有键都灰度同一批人时，灰度就失去了交叉验证。
	diff := 0
	total := 0
	for mid := int64(1); mid <= 200; mid++ {
		if PercentageBucket(cfgA, mid) != PercentageBucket(cfgB, mid) {
			diff++
		}
		total++
	}
	if diff*2 < total {
		t.Errorf("不同 cfg_key 下至少一半用户的桶号应不同，实际 %d/%d", diff, total)
	}
}

// --- 灰度命中矩阵 ---

func onRule(mod int32, mut func(*RolloutRule)) *RolloutRule {
	r := &RolloutRule{RuleID: 1, ConfigID: 7, Version: 2, Name: "r", Mode: mod, State: StateOn}
	if mut != nil {
		mut(r)
	}
	return r
}

func TestPickRolloutHitMatrix(t *testing.T) {
	const key = "home.enabled"
	// 固定时刻，避免用例依赖挂钟。
	const ts = int64(1_800_000_000)
	bucketOf := func(mid int64) int { return PercentageBucket(key, mid) }

	cases := []struct {
		name  string
		rules []*RolloutRule
		in    RolloutInput
		ts    int64
		want  string // 命中的规则名，"" 表示无人命中
	}{
		{
			name:  "空规则集回落正式版本",
			rules: nil,
			in:    RolloutInput{Platform: PlatformAndroid, Mid: 42, AppVersion: "7.0.0"},
			ts:    ts, want: "",
		},
		{
			name:  "全量命中",
			rules: []*RolloutRule{onRule(ModeFull, func(r *RolloutRule) { r.Name = "full" })},
			in:    RolloutInput{Platform: PlatformAndroid, Mid: 42},
			ts:    ts, want: "full",
		},
		{
			name:  "全量对未登录也命中",
			rules: []*RolloutRule{onRule(ModeFull, func(r *RolloutRule) { r.Name = "full" })},
			in:    RolloutInput{},
			ts:    ts, want: "full",
		},
		{
			name:  "全部不命中（端不符）",
			rules: []*RolloutRule{onRule(ModePlatform, func(r *RolloutRule) { r.Name = "ios"; r.Platforms = ",2," })},
			in:    RolloutInput{Platform: PlatformAndroid, Mid: 42},
			ts:    ts, want: "",
		},
		{
			name:  "端规则在未声明端时不命中",
			rules: []*RolloutRule{onRule(ModePlatform, func(r *RolloutRule) { r.Name = "any"; r.Platforms = ",1,2,3,4," })},
			in:    RolloutInput{Mid: 42},
			ts:    ts, want: "",
		},
		{
			name: "百分比：桶号小于 percentage 才命中（边界取等不命中）",
			rules: []*RolloutRule{onRule(ModePercentage, func(r *RolloutRule) {
				r.Name = "p50"
				r.Percentage = 50
			})},
			in: RolloutInput{Mid: func() int64 {
				for mid := int64(1); ; mid++ {
					if bucketOf(mid) == 50 {
						return mid
					}
				}
			}()},
			ts: ts, want: "",
		},
		{
			name:  "百分比 0 等于不放量（未登录也不命中）",
			rules: []*RolloutRule{onRule(ModePercentage, func(r *RolloutRule) { r.Name = "p0"; r.Percentage = 0 })},
			in:    RolloutInput{Mid: 42},
			ts:    ts, want: "",
		},
		{
			name:  "百分比 100 命中所有已登录",
			rules: []*RolloutRule{onRule(ModePercentage, func(r *RolloutRule) { r.Name = "p100"; r.Percentage = 100 })},
			in:    RolloutInput{Mid: 42},
			ts:    ts, want: "p100",
		},
		{
			name:  "百分比规则对未登录永不命中",
			rules: []*RolloutRule{onRule(ModePercentage, func(r *RolloutRule) { r.Name = "p100"; r.Percentage = 100 })},
			in:    RolloutInput{Mid: 0},
			ts:    ts, want: "",
		},
		{
			name: "多条规则百分比之和不等于 100 也各自独立判定",
			// 语义是「首个命中即用」，不是把人群切分：两条 60% 规则会重叠，
			// 因此 priority 次序决定谁拿到人，用例把这条口径钉住。
			rules: []*RolloutRule{
				onRule(ModePercentage, func(r *RolloutRule) { r.Name = "a60"; r.Percentage = 60; r.Priority = 1 }),
				onRule(ModePercentage, func(r *RolloutRule) { r.Name = "b60"; r.Percentage = 60; r.Priority = 2; r.RuleID = 2 }),
			},
			in: RolloutInput{Mid: func() int64 {
				for mid := int64(1); ; mid++ {
					if bucketOf(mid) < 60 {
						return mid
					}
				}
			}()},
			ts: ts, want: "a60",
		},
		{
			name:  "尾号分桶命中",
			rules: []*RolloutRule{onRule(ModeMidSuffix, func(r *RolloutRule) { r.Name = "s7"; r.MidSuffixes = "7" })},
			in:    RolloutInput{Mid: 1007},
			ts:    ts, want: "s7",
		},
		{
			name:  "尾号分桶不命中",
			rules: []*RolloutRule{onRule(ModeMidSuffix, func(r *RolloutRule) { r.Name = "s7"; r.MidSuffixes = "7" })},
			in:    RolloutInput{Mid: 1008},
			ts:    ts, want: "",
		},
		{
			name:  "白名单命中",
			rules: []*RolloutRule{onRule(ModeWhitelist, func(r *RolloutRule) { r.Name = "wl"; r.WhitelistMids = ",41,42," })},
			in:    RolloutInput{Mid: 42},
			ts:    ts, want: "wl",
		},
		{
			name:  "版本区间命中",
			rules: []*RolloutRule{onRule(ModeAppVersion, func(r *RolloutRule) { r.Name = "v"; r.AppVersionMin = "7.0"; r.AppVersionMax = "7.9" })},
			in:    RolloutInput{AppVersion: "7.10.0"},
			ts:    ts, want: "", // 7.10.0 > 7.9，逐段数值比较（字符串序会误判为命中）
		},
		{
			name:  "版本规则在版本未知时不命中",
			rules: []*RolloutRule{onRule(ModeAppVersion, func(r *RolloutRule) { r.Name = "v"; r.AppVersionMin = "7.0" })},
			in:    RolloutInput{AppVersion: ""},
			ts:    ts, want: "",
		},
		{
			name: "附加维度是 AND：百分比 + 端 + 版本都要满足",
			rules: []*RolloutRule{onRule(ModePercentage, func(r *RolloutRule) {
				r.Name = "and"
				r.Percentage = 100
				r.Platforms = ",1,"
				r.AppVersionMin = "7.0.0"
			})},
			in: RolloutInput{Mid: 42, Platform: PlatformAndroid, AppVersion: "7.1.0"},
			ts: ts, want: "and",
		},
		{
			name: "附加维度 AND：端不符则整条不命中",
			rules: []*RolloutRule{onRule(ModePercentage, func(r *RolloutRule) {
				r.Name = "and"
				r.Percentage = 100
				r.Platforms = ",1,"
			})},
			in: RolloutInput{Mid: 42, Platform: PlatformIOS},
			ts: ts, want: "",
		},
		{
			name: "附加维度 AND：尾号不符则整条不命中",
			rules: []*RolloutRule{onRule(ModeFull, func(r *RolloutRule) {
				r.Name = "full+sfx"
				r.MidSuffixes = "7"
			})},
			in: RolloutInput{Mid: 42},
			ts: ts, want: "",
		},
		{
			name:  "停用规则永不命中",
			rules: []*RolloutRule{onRule(ModeFull, func(r *RolloutRule) { r.Name = "off"; r.State = StateOff })},
			in:    RolloutInput{Mid: 42},
			ts:    ts, want: "",
		},
		{
			name:  "未到 start_at 不命中",
			rules: []*RolloutRule{onRule(ModeFull, func(r *RolloutRule) { r.Name = "future"; r.StartAt = ts + 1 })},
			in:    RolloutInput{Mid: 42},
			ts:    ts, want: "",
		},
		{
			name:  "end_at 是开区间",
			rules: []*RolloutRule{onRule(ModeFull, func(r *RolloutRule) { r.Name = "win"; r.StartAt = ts - 10; r.EndAt = ts })},
			in:    RolloutInput{Mid: 42},
			ts:    ts, want: "",
		},
		{
			name:  "窗口内命中（start_at 闭、end_at 开）",
			rules: []*RolloutRule{onRule(ModeFull, func(r *RolloutRule) { r.Name = "win"; r.StartAt = ts; r.EndAt = ts + 10 })},
			in:    RolloutInput{Mid: 42},
			ts:    ts, want: "win",
		},
		{
			name:  "ignore_rollout 跳过所有规则（后台预览正式值）",
			rules: []*RolloutRule{onRule(ModeFull, func(r *RolloutRule) { r.Name = "full" })},
			in:    RolloutInput{Mid: 42, IgnoreRollout: true},
			ts:    ts, want: "",
		},
		{
			// PickRollout 刻意不再排序：排序只在 ListCandidates 的
			// `ORDER BY priority ASC, rule_id ASC` 做一次。若在 Go 侧再排一次，
			// 两处顺序一旦分叉（例如 priority 相等时），「后台看到的优先级」和
			// 「端上命中的规则」就会不是同一批，所以这里锁住「数组序即优先级序」。
			name: "数组序即优先级：priority 大的排在前就先命中（顺序由 SQL 保证）",
			rules: []*RolloutRule{
				onRule(ModeFull, func(r *RolloutRule) { r.Name = "arr_first"; r.Priority = 9 }),
				onRule(ModeFull, func(r *RolloutRule) { r.Name = "arr_second"; r.Priority = 1; r.RuleID = 2 }),
			},
			in: RolloutInput{Mid: 42},
			ts: ts, want: "arr_first",
		},
		{
			name: "同 priority 时按数组次序（即 SQL 的 rule_id 升序）取首个",
			rules: []*RolloutRule{
				onRule(ModeFull, func(r *RolloutRule) { r.Name = "low_id"; r.Priority = 5; r.RuleID = 1 }),
				onRule(ModeFull, func(r *RolloutRule) { r.Name = "high_id"; r.Priority = 5; r.RuleID = 8 }),
			},
			in: RolloutInput{Mid: 42},
			ts: ts, want: "low_id",
		},
		{
			name: "首个命中即停：后面更高优先级的停用规则不影响结果",
			rules: []*RolloutRule{
				onRule(ModeFull, func(r *RolloutRule) { r.Name = "on" }),
				onRule(ModePercentage, func(r *RolloutRule) { r.Name = "ignored"; r.Percentage = 100; r.State = StateOff }),
			},
			in: RolloutInput{Mid: 42},
			ts: ts, want: "on",
		},
		{
			name:  "未知 mode 不命中（脏数据不得变成全量）",
			rules: []*RolloutRule{onRule(99, func(r *RolloutRule) { r.Name = "junk" })},
			in:    RolloutInput{Mid: 42, Platform: PlatformAndroid},
			ts:    ts, want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for i := 0; i < 3; i++ { // 重复调用结果必须一致（纯函数）
				got := PickRollout(c.rules, key, c.in, c.ts)
				if c.want == "" {
					if got != nil {
						t.Fatalf("期望不命中，实际命中 %q", got.Name)
					}
					continue
				}
				if got == nil {
					t.Fatalf("期望命中 %q，实际不命中", c.want)
				}
				if got.Name != c.want {
					t.Fatalf("期望命中 %q，实际 %q", c.want, got.Name)
				}
			}
		})
	}
}

func TestPickRolloutPercentageCoversExactlyBucketRange(t *testing.T) {
	// percentage=p 命中的集合必须恰好是 {mid | bucket(mid) < p}：
	// 边界写反（<=）会让实际放量比声明的多一个桶。
	const key = "boundary.check"
	for _, p := range []int32{1, 25, 50, 99} {
		rule := onRule(ModePercentage, func(r *RolloutRule) { r.Percentage = p })
		hit, miss := 0, 0
		for mid := int64(1); mid <= 5000; mid++ {
			if PickRollout([]*RolloutRule{rule}, key, RolloutInput{Mid: mid}, 0) != nil {
				hit++
				if bucket := PercentageBucket(key, mid); bucket >= int(p) {
					t.Fatalf("percentage=%d 命中了桶号 %d（应 < %d）", p, bucket, p)
				}
			} else if PercentageBucket(key, mid) < int(p) {
				miss++
				t.Fatalf("percentage=%d 漏掉了桶号 %d", p, PercentageBucket(key, mid))
			}
		}
		if hit == 0 || miss != 0 {
			t.Errorf("percentage=%d 的命中集合异常: hit=%d", p, hit)
		}
	}
}

func TestInTimeWindowAndEffectiveAtNilSafe(t *testing.T) {
	var nilRule *RolloutRule
	if nilRule.EffectiveAt(100) || nilRule.Matched("k", RolloutInput{}) {
		t.Error("nil 规则必须判为不命中，而不是 panic")
	}
	r := onRule(ModeFull, nil)
	r.StartAt, r.EndAt = 100, 200
	if !r.InTimeWindow(100) || !r.InTimeWindow(199) || r.InTimeWindow(200) || r.InTimeWindow(99) {
		t.Error("时间窗口边界错误（start 闭、end 开）")
	}
	open := onRule(ModeFull, nil) // start_at=0 立即生效，end_at=0 不截止
	if !open.InTimeWindow(0) || !open.InTimeWindow(1<<40) {
		t.Error("零值窗口必须表示不设限")
	}
	r.State = StateOff
	if r.EffectiveAt(150) {
		t.Error("停用规则不得算生效")
	}
}

// --- 规则形态校验 ---

func TestValidateRuleShapeMatrix(t *testing.T) {
	base := func(mut func(*RolloutRule)) *RolloutRule {
		r := &RolloutRule{ConfigID: 7, Version: 2, Name: "r", Mode: ModeFull}
		if mut != nil {
			mut(r)
		}
		return r
	}
	cases := []struct {
		name string
		rule *RolloutRule
		want error
	}{
		{"全量：合法", base(nil), nil},
		{"版本号缺失", base(func(r *RolloutRule) { r.Version = 0 }), ErrVersionNotFound},
		{"mode 未声明", base(func(r *RolloutRule) { r.Mode = ModeUnspecified }), ErrRuleModeRequired},
		{"mode 未知", base(func(r *RolloutRule) { r.Mode = 77 }), ErrRuleModeRequired},
		{"声明全量却暗藏百分比", base(func(r *RolloutRule) { r.Percentage = 10 }), ErrRuleModeMismatch},
		{"声明全量却暗藏端", base(func(r *RolloutRule) { r.Platforms = ",1," }), ErrRuleModeMismatch},
		{"声明全量却暗藏白名单", base(func(r *RolloutRule) { r.WhitelistMids = ",1," }), ErrRuleModeMismatch},
		{"声明全量却暗藏版本区间", base(func(r *RolloutRule) { r.AppVersionMin = "7.0" }), ErrRuleModeMismatch},
		{"声明全量却暗藏尾号", base(func(r *RolloutRule) { r.MidSuffixes = "3" }), ErrRuleModeMismatch},
		{"百分比但没填比例", base(func(r *RolloutRule) { r.Mode = ModePercentage }), ErrRuleModeMismatch},
		{"百分比越界上", base(func(r *RolloutRule) { r.Mode = ModePercentage; r.Percentage = 101 }), ErrPercentageOutOfRange},
		{"百分比越界下", base(func(r *RolloutRule) { r.Mode = ModePercentage; r.Percentage = -1 }), ErrPercentageOutOfRange},
		{"版本规则但没填区间", base(func(r *RolloutRule) { r.Mode = ModeAppVersion }), ErrRuleModeMismatch},
		{"端规则但没填端", base(func(r *RolloutRule) { r.Mode = ModePlatform }), ErrRuleModeMismatch},
		{"尾号规则但没填尾号", base(func(r *RolloutRule) { r.Mode = ModeMidSuffix }), ErrRuleModeMismatch},
		{"白名单规则但名单为空", base(func(r *RolloutRule) { r.Mode = ModeWhitelist }), ErrRuleModeMismatch},
		{"区间倒挂", base(func(r *RolloutRule) {
			r.Mode = ModeAppVersion
			r.AppVersionMin = "7.10.0"
			r.AppVersionMax = "7.2.0"
		}), ErrAppVersionRangeInvalid},
		{"区间下界格式非法", base(func(r *RolloutRule) {
			r.Mode = ModeAppVersion
			r.AppVersionMin = "v7.1"
		}), ErrAppVersionRangeInvalid},
		{"end_at 不晚于 start_at", base(func(r *RolloutRule) { r.StartAt = 200; r.EndAt = 100 }), ErrRuleTimeRangeInvalid},
		{"负时间戳", base(func(r *RolloutRule) { r.StartAt = -1 }), ErrRuleTimeRangeInvalid},
		{"脏尾号", base(func(r *RolloutRule) { r.Mode = ModeMidSuffix; r.MidSuffixes = "10" }), ErrMidSuffixInvalid},
		{"存储形态里的未知端", base(func(r *RolloutRule) { r.Mode = ModePlatform; r.Platforms = ",5," }), ErrPlatformUnknown},
		{"端列表超过四端", base(func(r *RolloutRule) { r.Mode = ModePlatform; r.Platforms = ",1,2,3,4," }), nil},
		{"白名单超上限", base(func(r *RolloutRule) {
			r.Mode = ModeWhitelist
			ids := make([]int64, MaxWhitelistMids+1)
			for i := range ids {
				ids[i] = int64(i + 1)
			}
			r.WhitelistMids = IDListString(ids)
		}), ErrWhitelistTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateRuleShape(c.rule, MaxWhitelistMids)
			if c.want == nil {
				if err != nil {
					t.Fatalf("应通过，得到 %v", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("err=%v，期望 %v", err, c.want)
			}
		})
	}
}

// TestShapeValidatorsRunBeforeSQL 钉住「校验先于执行」的次序：
// 传 nil session 时，非法入参必须在碰到 SQL 之前就被拒；
// 若有人把校验挪到 SQL 之后，这里会当场 panic 而不是静默放过。
func TestShapeValidatorsRunBeforeSQL(t *testing.T) {
	ctx := context.Background()
	if _, _, err := upsertRule(ctx, nil, &RolloutRule{}); !errors.Is(err, ErrRuleNameRequired) {
		t.Errorf("upsertRule 空规则应报 ErrRuleNameRequired，得到 %v", err)
	}
	if _, _, err := upsertRule(ctx, nil, &RolloutRule{
		Name: "r", Version: 1, Mode: ModePercentage,
	}); !errors.Is(err, ErrRuleModeMismatch) {
		t.Errorf("upsertRule 应先跑形态校验，得到 %v", err)
	}
	// 版本号单调：回滚也必须前进，否则同一版本号有两种内容，审计无法解释。
	if ok, err := advanceRelease(ctx, nil, 7, 3, 3, 1, 1); ok || !errors.Is(err, ErrVersionConflict) {
		t.Errorf("latest_version 倒退/原地不动必须被拒，得到 (%v,%v)", ok, err)
	}
	if ok, err := advanceRelease(ctx, nil, 7, 3, 2, 1, 1); ok || !errors.Is(err, ErrVersionConflict) {
		t.Errorf("latest_version 倒退必须被拒，得到 (%v,%v)", ok, err)
	}
}

func TestInsertVersionRejectsUnexplainableRowsBeforeSQL(t *testing.T) {
	ctx := context.Background()
	valid := func(mut func(*ConfigVersion)) *ConfigVersion {
		v := &ConfigVersion{
			ConfigID: 7, Version: 1, Value: "1", ValueType: ValueTypeInt,
			ChangeType: ChangeTypePublish, Reason: "为什么改", RequestID: "req-1",
		}
		if mut != nil {
			mut(v)
		}
		return v
	}
	cases := []struct {
		name string
		in   *ConfigVersion
		want error
	}{
		{"缺配置项", valid(func(v *ConfigVersion) { v.ConfigID = 0 }), ErrConfigNotFound},
		{"版本号非正", valid(func(v *ConfigVersion) { v.Version = 0 }), ErrVersionNotFound},
		{"缺幂等键", valid(func(v *ConfigVersion) { v.RequestID = "" }), ErrRequestIDRequired},
		{"缺变更原因", valid(func(v *ConfigVersion) { v.Reason = "" }), ErrReasonRequired},
		{"非法变更类型", valid(func(v *ConfigVersion) { v.ChangeType = "hotfix" }), ErrChangeTypeInvalid},
		{"空变更类型", valid(func(v *ConfigVersion) { v.ChangeType = "" }), ErrChangeTypeInvalid},
		{"值类型不可解释", valid(func(v *ConfigVersion) { v.ValueType = 0 }), ErrValueTypeUnsupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := insertVersion(ctx, nil, c.in)
			if !errors.Is(err, c.want) {
				t.Fatalf("err=%v，期望 %v", err, c.want)
			}
		})
	}
	// 三种合法变更类型必须**越过**校验（在 nil session 上才失败），
	// 否则说明有人把 create/rollback 也挡掉了 —— 那会让首发与回滚永远写不进去。
	for _, ct := range []string{ChangeTypeCreate, ChangeTypePublish, ChangeTypeRollback} {
		t.Run("合法类型 "+ct, func(t *testing.T) {
			reached := runsUntilSession(ctx, valid(func(v *ConfigVersion) { v.ChangeType = ct }))
			if !reached {
				t.Errorf("change_type=%s 应越过全部校验、直到碰到 session 才失败", ct)
			}
		})
	}
}

// runsUntilSession 断言「校验全部通过、直到碰到 session 才失败」。
// nil session 会让真正的 SQL 路径 panic 或报错，两种都说明校验没挡住。
func runsUntilSession(ctx context.Context, v *ConfigVersion) (reachedSQL bool) {
	defer func() {
		if r := recover(); r != nil {
			reachedSQL = true
		}
	}()
	_, err := insertVersion(ctx, nil, v)
	return err == nil || !errors.Is(err, ErrRequestIDRequired) &&
		!errors.Is(err, ErrReasonRequired) && !errors.Is(err, ErrChangeTypeInvalid) &&
		!errors.Is(err, ErrValueTypeUnsupported) && !errors.Is(err, ErrVersionNotFound) &&
		!errors.Is(err, ErrConfigNotFound)
}

func TestRowInsertValidatorsRunBeforeSQL(t *testing.T) {
	ctx := context.Background()
	item := NewConfigItemModel(nil)
	if _, err := item.Insert(ctx, &ConfigItem{CfgKey: "Bad Key", Scope: ScopeGlobal, ValueType: ValueTypeString}); !errors.Is(err, ErrConfigKeyInvalid) {
		t.Errorf("大写/空格键必须被拒，得到 %v", err)
	}
	if _, err := item.Insert(ctx, &ConfigItem{CfgKey: "a.b", Scope: "weixin", ValueType: ValueTypeString}); !errors.Is(err, ErrScopeUnknown) {
		t.Errorf("未知 scope 必须被拒，得到 %v", err)
	}
	if _, err := item.Insert(ctx, &ConfigItem{CfgKey: "a.b", Scope: ScopeGlobal, ValueType: 0}); !errors.Is(err, ErrValueTypeUnsupported) {
		t.Errorf("值类型 0 必须被拒（不猜类型），得到 %v", err)
	}
	// cfg_key 格式：最长 64、只允许小写字母/数字/下划线/点。
	for _, bad := range []string{"", "A", "a b", "a/b", "a-b", strings.Repeat("a", 65), "aé"} {
		if ValidCfgKey(bad) {
			t.Errorf("cfg_key %q 必须不合法", bad)
		}
	}
	for _, good := range []string{"a", "home.topic.enabled", "a_b.9", strings.Repeat("a", 64)} {
		if !ValidCfgKey(good) {
			t.Errorf("cfg_key %q 必须合法", good)
		}
	}
	if _, err := NewTopicModel(nil).Insert(ctx, &Topic{Slug: "Bad Slug", Title: "t"}); !errors.Is(err, ErrTopicSlugInvalid) {
		t.Errorf("slug 格式必须校验，得到 %v", err)
	}
	if _, err := NewTopicModel(nil).Insert(ctx, &Topic{Slug: "ok-slug", Title: ""}); !errors.Is(err, ErrTopicTitleRequired) {
		t.Errorf("空标题必须被拒，得到 %v", err)
	}
	// 只含空白的标题由 logic 层的 TrimSpace 兜住（见 TestSaveTopicRejectsBlankTitle），
	// model 层只判 ""：它是第二道闸，不是唯一一道。
	if _, err := NewTopicModel(nil).Insert(ctx, &Topic{Slug: "ok-slug", Title: "t", StartAt: 200, EndAt: 100}); !errors.Is(err, ErrTopicTimeRangeInvalid) {
		t.Errorf("专题窗口倒挂必须被拒，得到 %v", err)
	}
	if _, err := NewRecommendSlotModel(nil).Insert(ctx, &RecommendSlot{Code: ""}); !errors.Is(err, ErrSlotCodeRequired) {
		t.Errorf("空 code 必须被拒，得到 %v", err)
	}
}

func TestSlugAndCodeFormats(t *testing.T) {
	for _, bad := range []string{"", "-x", "_x", ".x", "a b", "A-b", strings.Repeat("a", 65), "中文"} {
		if ValidTopicSlug(bad) {
			t.Errorf("slug %q 必须不合法", bad)
		}
		if ValidSlotCode(bad) {
			t.Errorf("slot code %q 必须不合法", bad)
		}
	}
	if !ValidTopicSlug("ab") || !ValidTopicSlug("a_1-b") {
		t.Error("合法 slug 被误拒")
	}
	if !ValidSlotCode("ab") || !ValidSlotCode("home.banner_x") {
		t.Error("合法 slot code 被误拒")
	}
	// 专题允许 - 而坑位允许 .（两者寻址语义不同），交叉验证一下别写反。
	if ValidSlotCode("a-b") {
		t.Error("坑位 code 不允许连字符")
	}
	if ValidTopicSlug("a.b") {
		t.Error("专题 slug 不允许点号")
	}
}

func TestSlotCapacityRange(t *testing.T) {
	if ValidSlotCapacity(0) || ValidSlotCapacity(-1) || ValidSlotCapacity(int32(MaxSlotCapacityHard)+1) {
		t.Error("容量边界 [1,MaxSlotCapacityHard] 判定错误")
	}
	if !ValidSlotCapacity(1) || !ValidSlotCapacity(int32(MaxSlotCapacityHard)) {
		t.Error("容量边界值必须合法")
	}
	if !ValidSlotCapacity(DefaultSlotCapacity) {
		t.Error("默认容量必须落在合法区间内")
	}
}

func TestSlotItemWindow(t *testing.T) {
	cases := []struct {
		start, end, at int64
		want           bool
	}{
		{0, 0, 0, true},
		{100, 0, 99, false},
		{100, 0, 100, true},
		{0, 200, 199, true},
		{0, 200, 200, false},
		{100, 200, 150, true},
	}
	for _, c := range cases {
		it := &SlotItem{StartAt: c.start, EndAt: c.end}
		if got := it.InWindow(c.at); got != c.want {
			t.Errorf("InWindow(%d/%d @%d)=%v，期望 %v", c.start, c.end, c.at, got, c.want)
		}
	}
}

func TestValidateTopicItemsAndSlotItems(t *testing.T) {
	item := func(pos int32, typ, id string) *TopicItem {
		return &TopicItem{TopicID: 9, Position: pos, ItemType: typ, ItemID: id, State: StateOn}
	}
	ok := []*TopicItem{item(1, ItemTypeUGCVideo, "111"), item(2, ItemTypePGCSeason, "222")}
	if err := ValidateTopicItems(9, ok, 0); err != nil {
		t.Fatalf("合法条目应通过: %v", err)
	}
	if err := ValidateTopicItems(0, ok, 0); !errors.Is(err, ErrTopicNotFound) {
		t.Errorf("topic_id<=0 必须被拒，得到 %v", err)
	}
	// 空集合的语义：报错而不是清空。
	if err := ValidateTopicItems(9, nil, 0); !errors.Is(err, ErrBatchEmpty) {
		t.Errorf("空条目数组必须报 ErrBatchEmpty（不得当成清空），得到 %v", err)
	}
	if err := ValidateTopicItems(9, []*TopicItem{item(1, ItemTypeUGCVideo, "1"), item(3, ItemTypePGCSeason, "2")}, 0); !errors.Is(err, ErrTopicPositionNotSequential) {
		t.Errorf("position 有洞必须被拒，得到 %v", err)
	}
	if err := ValidateTopicItems(9, []*TopicItem{item(2, ItemTypeUGCVideo, "1")}, 0); !errors.Is(err, ErrTopicPositionNotSequential) {
		t.Errorf("position 不从 1 开始必须被拒，得到 %v", err)
	}
	if err := ValidateTopicItems(9, []*TopicItem{item(1, ItemTypeUGCVideo, "")}, 0); !errors.Is(err, ErrItemRefRequired) {
		t.Errorf("空 item_id 必须被拒，得到 %v", err)
	}
	// 专题里挂专题 = 自引用环，必须拒；坑位里挂专题是正常编排。
	if err := ValidateTopicItems(9, []*TopicItem{item(1, ItemTypeTopic, "9")}, 0); !errors.Is(err, ErrItemTypeUnsupported) {
		t.Errorf("专题内挂 topic 必须被拒，得到 %v", err)
	}
	if err := ValidateTopicItems(9, ok, 1); !errors.Is(err, ErrTopicItemLimit) {
		t.Errorf("超过 maxItems 必须被拒，得到 %v", err)
	}

	si := func(pos int32, typ, id string) *SlotItem {
		return &SlotItem{SlotID: 3, Position: pos, ItemType: typ, ItemID: id, State: StateOn}
	}
	if err := ValidateSlotItems(3, 6, []*SlotItem{si(2, ItemTypeUGCVideo, "1"), si(5, ItemTypeTopic, "9")}, 0); err != nil {
		t.Errorf("坑位允许 position 有空洞（1..capacity 内即可），得到 %v", err)
	}
	if err := ValidateSlotItems(0, 6, []*SlotItem{si(1, ItemTypeUGCVideo, "1")}, 0); !errors.Is(err, ErrSlotNotFound) {
		t.Errorf("slot_id<=0 必须被拒，得到 %v", err)
	}
	if err := ValidateSlotItems(3, 6, nil, 0); !errors.Is(err, ErrBatchEmpty) {
		t.Errorf("空坑位条目必须报 ErrBatchEmpty，得到 %v", err)
	}
	if err := ValidateSlotItems(3, 0, []*SlotItem{si(1, ItemTypeUGCVideo, "1")}, 0); !errors.Is(err, ErrSlotCapacityInvalid) {
		t.Errorf("非法容量必须被拒，得到 %v", err)
	}
	if err := ValidateSlotItems(3, 2, []*SlotItem{si(3, ItemTypeUGCVideo, "1")}, 0); !errors.Is(err, ErrSlotPositionOutOfRange) {
		t.Errorf("position 越界必须被拒（入库也永远展示不出来），得到 %v", err)
	}
	if err := ValidateSlotItems(3, 4, []*SlotItem{si(1, ItemTypeUGCVideo, "1"), si(1, ItemTypePGCEpisode, "2")}, 0); !errors.Is(err, ErrSlotPositionDuplicated) {
		t.Errorf("position 冲突必须被拒，得到 %v", err)
	}
	// 同一坑位重复挂同一内容：排障时无法解释「为什么这一格出的是哪条」。
	if err := ValidateSlotItems(3, 4, []*SlotItem{si(1, ItemTypeUGCVideo, "7"), si(2, ItemTypeUGCVideo, "7")}, 0); !errors.Is(err, ErrSlotItemDuplicated) {
		t.Errorf("重复挂同一内容必须被拒，得到 %v", err)
	}
	// 不同 item_type 下的同 ID 不是重复引用（aid 与 season_id 是两个域的主键）。
	if err := ValidateSlotItems(3, 4, []*SlotItem{si(1, ItemTypeUGCVideo, "7"), si(2, ItemTypePGCSeason, "7")}, 0); err != nil {
		t.Errorf("跨类型同 ID 不是重复，得到 %v", err)
	}
	// 条目数受 capacity 收紧：capacity 才是硬边界。
	if err := ValidateSlotItems(3, 2, []*SlotItem{si(1, ItemTypeUGCVideo, "1"), si(2, ItemTypeUGCVideo, "2"), si(3, ItemTypeUGCVideo, "3")}, 100); !errors.Is(err, ErrSlotItemLimit) {
		t.Errorf("条目数超过 capacity 必须被拒，得到 %v", err)
	}
	if err := ValidateSlotItems(3, 4, []*SlotItem{si(1, ItemTypeTopic, "9")}, 0); err != nil {
		t.Errorf("坑位允许挂专题，得到 %v", err)
	}
	if err := ValidateSlotItems(3, 4, []*SlotItem{{SlotID: 3, Position: 1, ItemType: ItemTypeUGCVideo, ItemID: "1", StartAt: 200, EndAt: 100}}, 0); !errors.Is(err, ErrRuleTimeRangeInvalid) {
		t.Errorf("排期窗口倒挂必须被拒，得到 %v", err)
	}
}

// --- 分页与限额 ---

func TestPageOrDefault(t *testing.T) {
	cases := []struct{ pn, ps, wantPn, wantPs int32 }{
		{0, 0, 1, 100},    // 未填 → 第 1 页 + 上限页长（不是「0 条」）
		{-5, 0, 1, 100},   // 负页码在 model 层兜到 1（logic 层更早拒）
		{2, 20, 2, 20},    //
		{1, 1000, 1, 100}, // 越界夹到上限而不是报错：控制台白屏不是想要的结果
		{1, -1, 1, 100},
	}
	for _, c := range cases {
		pn, ps := PageOrDefault(c.pn, c.ps, 100)
		if pn != c.wantPn || ps != c.wantPs {
			t.Errorf("PageOrDefault(%d,%d) = (%d,%d)，期望 (%d,%d)", c.pn, c.ps, pn, ps, c.wantPn, c.wantPs)
		}
	}
	if _, ps := PageOrDefault(1, 999, 20); ps != 20 {
		t.Error("页长必须被配置上限夹住")
	}
}

func TestHardLimitsAreInternallyConsistent(t *testing.T) {
	// 代码硬上限与「配置只能收紧」的前提：硬上限必须为正，且默认值不超过它。
	for name, v := range map[string]int{
		"MaxCfgValueBytes": MaxCfgValueBytes, "MaxReasonChars": MaxReasonChars,
		"MaxResolveKeys": MaxResolveKeys, "MaxPageSizeHard": MaxPageSizeHard,
		"MaxOperatorNameChars": MaxOperatorNameChars, "MaxTTLSecondsHard": MaxTTLSecondsHard,
		"MaxWhitelistMids": MaxWhitelistMids, "MaxRolloutCandidates": MaxRolloutCandidates,
		"MaxTopicItems": MaxTopicItems, "MaxSlotItems": MaxSlotItems,
		"MaxSlotCapacityHard": MaxSlotCapacityHard, "MaxTopicRefIDs": MaxTopicRefIDs,
		"IDListMaxLen": IDListMaxLen, "MaxMidSuffixes": MaxMidSuffixes,
	} {
		if v <= 0 {
			t.Errorf("%s=%d 必须为正", name, v)
		}
	}
	// 批量解析上限必须等于契约注释里的 50（网关按它拼首页请求）。
	if MaxResolveKeys != 50 || MaxPageSizeHard != 100 {
		t.Errorf("契约上限漂移: MaxResolveKeys=%d MaxPageSizeHard=%d", MaxResolveKeys, MaxPageSizeHard)
	}
	// IDListMaxLen 是所有 ",a,b," 列的统一上限；端列表列宽只有 VARCHAR(32)，
	// 但端最多 4 个（",1,2,3,4," = 9 字节），因此必须显式限数量而不是靠列宽。
	if got := len(IDListString([]int64{1, 2, 3, 4})); got > 32 {
		t.Errorf("四端全集的存储串长度 %d 超过 platforms 列宽", got)
	}
	if MaxMidSuffixes != 10 {
		t.Errorf("十进制尾号最多 10 个，得到 %d", MaxMidSuffixes)
	}
}
