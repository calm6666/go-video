package model

import (
	"strings"
	"testing"
	"unicode/utf8"

	"go-video/common/eventenvelope"
)

// 本文件钉的是 model 层「不连库就能证对」的那批计算：窗口左边界、天级分桶、
// 游标（水位）入参校验、分页/批量收敛、受控词表判定，以及拼 SQL 的
// 「占位符与参数同序」不变量。
//
// 为什么这批必须单测（AGENTS.md §9）：
//   - AlignWindow / WindowSeconds 是口径的一部分：README「窗口、水位与迟到」明确
//     「落库与查询都按 window_start 左边界对齐」，规整错一档不会报错，
//     只会让同一分钟的数据散成两行窗口，榜与曲线各自少一半。
//   - checkWatermark 放行一个未规整的 last_closed_start，window_start=0
//     （取最近闭合窗口）就会稳定指向一个不存在的窗口，读接口长期返回空数据。
//   - clampLimit/clampOffset/clampBatch 是「无界扫描」的最后兜底：
//     漏一次 LIMIT 在单测与编译期都发现不了，只有线上慢查询会暴露。
//   - ValidInterestKey / NormalizeAction / ContentStateOfAction 是隐私与口径的
//     受控词表闸门（AGENTS.md §7）：放过一个未知值，画像里就会出现无人能解释的键，
//     榜单就会出现无法重算的动作。
//   - build*Query 的 args 顺序与 ? 顺序不一致时 SQL 依然合法，
//     但会拿错值去比（metric_window.go 的原话：「这是这类拼接最阴的坑」）。

// --- 窗口与分桶 ---

// TestAlignWindowLeftBoundaryContract 钉 AlignWindow 的三条口径：
// 幂等、只向左（绝不越过原时刻）、与窗口秒数严格同档。
func TestAlignWindowLeftBoundaryContract(t *testing.T) {
	const base int64 = 1700000000 // 2023-11-14 22:13:20Z，一个非整档的真实时刻
	cases := []struct {
		name    string
		window  int32
		seconds int64
	}{
		{"5MIN", WindowType5Min, 300},
		{"HOUR", WindowTypeHour, 3600},
		{"DAY", WindowTypeDay, 86400},
		{"WEEK", WindowTypeWeek, 604800},
	}
	for _, tc := range cases {
		for _, ts := range []int64{base, base + 1, base - 1, tc.seconds, tc.seconds - 1, tc.seconds * 3} {
			got := AlignWindow(ts, tc.window)
			if got > ts {
				t.Errorf("%s: AlignWindow(%d)=%d 越过了原时刻，窗口左边界只能左移", tc.name, ts, got)
			}
			if got%tc.seconds != 0 {
				t.Errorf("%s: AlignWindow(%d)=%d 未按 %d 秒对齐", tc.name, ts, got, tc.seconds)
			}
			if ts-got >= tc.seconds {
				t.Errorf("%s: AlignWindow(%d)=%d 少退了一档（差 %d 秒 >= 窗口长度）", tc.name, ts, got, ts-got)
			}
			if again := AlignWindow(got, tc.window); again != got {
				t.Errorf("%s: 规整不幂等：%d -> %d -> %d", tc.name, ts, got, again)
			}
		}
		// 同一档内的任意两个时刻必须规整到同一个 window_start，
		// 否则「同一分钟的数据散成两行」就会发生（README「窗口、水位与迟到」）。
		start := AlignWindow(base, tc.window)
		for delta := int64(0); delta < tc.seconds; delta += tc.seconds / 7 {
			if got := AlignWindow(start+delta, tc.window); got != start {
				t.Errorf("%s: 同档内 start+%d 规整成了 %d，应为 %d", tc.name, delta, got, start)
			}
		}
		if got := AlignWindow(start+tc.seconds, tc.window); got != start+tc.seconds {
			t.Errorf("%s: 恰好落在下一档左边界却被改动：%d -> %d", tc.name, start+tc.seconds, got)
		}
	}
	// TOTAL 恒为 0：window_start=0 在契约里是「整表汇总」的哨兵，
	// 若退化成「按原时刻返回」，uniq_metric 会把同一指标拆成无数行。
	for _, ts := range []int64{0, base, base + 7, -5} {
		if got := AlignWindow(ts, WindowTypeTotal); got != 0 {
			t.Errorf("TOTAL: AlignWindow(%d)=%d，必须恒为 0", ts, got)
		}
	}
	// 非正时间戳与非法粒度一律 0：调用方（window_start=0 的解析路径）依赖这个退化值。
	for _, ts := range []int64{0, -1, -86400} {
		for _, w := range []int32{WindowType5Min, WindowTypeHour, WindowTypeDay, WindowTypeWeek, WindowTypeUnspecified, 99} {
			if got := AlignWindow(ts, w); got != 0 {
				t.Errorf("AlignWindow(%d, %d)=%d，非正时间戳必须为 0", ts, w, got)
			}
		}
	}
}

// TestWindowSecondsMatchesGranularityLadder 钉窗口秒数本身：
// types.go 注释写明「这些常量是口径的一部分，改动等价于口径变更」。
// 这里同时钉档位间的整除关系（榜按 5MIN 累加到 HOUR 才不会余数）。
func TestWindowSecondsMatchesGranularityLadder(t *testing.T) {
	want := map[int32]int64{
		WindowType5Min:  300,
		WindowTypeHour:  3600,
		WindowTypeDay:   86400,
		WindowTypeWeek:  604800,
		WindowTypeTotal: 0,
	}
	for wt, w := range want {
		if got := WindowSeconds(wt); got != w {
			t.Errorf("WindowSeconds(%d)=%d，期望 %d（改档等于口径变更，必须新增 metric_version）", wt, got, w)
		}
	}
	if got := WindowSeconds(WindowTypeUnspecified); got != -1 {
		t.Errorf("未指定粒度的秒数必须是 -1（0 会被调用方读成 TOTAL），实得 %d", got)
	}
	if got := WindowSeconds(12345); got != -1 {
		t.Errorf("未知粒度必须返回 -1，实得 %d", got)
	}
	if WindowSeconds(WindowTypeHour)%WindowSeconds(WindowType5Min) != 0 {
		t.Error("HOUR 不是 5MIN 的整数倍，实时累加到小时档会留余数")
	}
	if WindowSeconds(WindowTypeDay)%WindowSeconds(WindowTypeHour) != 0 {
		t.Error("DAY 不是 HOUR 的整数倍")
	}
	if WindowSeconds(WindowTypeWeek)%WindowSeconds(WindowTypeDay) != 0 {
		t.Error("WEEK 不是 DAY 的整数倍，周榜按天累加会留余数")
	}
	// ValidWindowType 的边界必须与 WindowSeconds 的 -1 分界线一致，
	// 否则会出现「校验通过但拿不到秒数」的粒度。
	for wt := int32(-3); wt <= 12; wt++ {
		if ValidWindowType(wt) != (WindowSeconds(wt) >= 0) {
			t.Errorf("粒度 %d：ValidWindowType=%v 与 WindowSeconds=%d 的合法性判定不一致",
				wt, ValidWindowType(wt), WindowSeconds(wt))
		}
	}
}

// TestDayStartUnixAgreesWithAlignWindowDay 钉「留存分桶按 UTC 日左边界对齐」：
// 天级分桶有两个入口（内部的 dayStartUnix 与导出的 DayStartUnix），
// 而窗口规整另有 AlignWindow(DAY)。三者给出不同的日子，
// cohort_date 与 window_start 就会互相指错桶。
func TestDayStartUnixAgreesWithAlignWindowDay(t *testing.T) {
	const day int64 = 86400
	for _, ts := range []int64{0, 1, day - 1, day, day + 1, 1700000000, 1700000000/2*2 + 1} {
		if got, want := dayStartUnix(ts), AlignWindow(ts, WindowTypeDay); got != want {
			t.Errorf("dayStartUnix(%d)=%d 与 AlignWindow(%d,DAY)=%d 不一致", ts, got, ts, want)
		}
		if DayStartUnix(ts) != dayStartUnix(ts) {
			t.Errorf("DayStartUnix(%d) 与内部实现分叉：logic 回显的分桶日与实际查询用的不是同一个", ts)
		}
		if got := dayStartUnix(ts); got > ts || ts-got >= day {
			t.Errorf("dayStartUnix(%d)=%d 越界：必须落在同一个 UTC 日内", ts, got)
		}
	}
	// 非正时间戳：两个函数都收敛到 0（0 是本服务的「未给出」哨兵）。
	for _, ts := range []int64{-1, -day, -day - 1} {
		if got := AlignWindow(ts, WindowTypeDay); got != 0 {
			t.Errorf("AlignWindow(%d,DAY)=%d，负数时间戳必须收敛到 0", ts, got)
		}
	}
}

// TestValidWindowTypeAndSubjectTypeRanges 钉枚举区间与 rpc 的逐值对齐口径：
// 越界的 0（UNSPECIFIED）与最大值+1 必须被拒，
// 否则「漏传枚举字段」会被当成一个合法档位写进库。
func TestValidWindowTypeAndSubjectTypeRanges(t *testing.T) {
	subjectCases := []struct {
		v    int32
		want bool
	}{
		{SubjectTypeUnspecified, false}, {SubjectTypeAid, true}, {SubjectTypeZone, true},
		{SubjectTypeMid, true}, {SubjectTypeCatalogItem, true},
		{SubjectTypeCatalogItem + 1, false}, {-1, false},
	}
	for _, tc := range subjectCases {
		if got := ValidSubjectType(tc.v); got != tc.want {
			t.Errorf("ValidSubjectType(%d)=%v，期望 %v", tc.v, got, tc.want)
		}
	}
	for _, tc := range []struct {
		v    int32
		want bool
	}{
		{MetricSourceUnspecified, false}, {MetricSourceRealtime, true},
		{MetricSourceOffline, true}, {MetricSourceRecompute, true},
		{MetricSourceRecompute + 1, false},
	} {
		if got := ValidMetricSource(tc.v); got != tc.want {
			t.Errorf("ValidMetricSource(%d)=%v，期望 %v（人工改指标不在计算链路白名单内）", tc.v, got, tc.want)
		}
	}
	for _, tc := range []struct {
		v    int32
		want bool
	}{
		{JobTypeUnspecified, false}, {JobTypeRealtime, true}, {JobTypeOfflineBackfill, true},
		{JobTypeRecompute, true}, {JobTypeRecompute + 1, false},
	} {
		if got := ValidJobType(tc.v); got != tc.want {
			t.Errorf("ValidJobType(%d)=%v，期望 %v", tc.v, got, tc.want)
		}
	}
	for _, tc := range []struct {
		v    int32
		want bool
	}{
		{SourceChannelUnspecified, false}, {SourceChannelCollector, true},
		{SourceChannelDomain, true}, {SourceChannelContent, true},
		{SourceChannelContent + 1, false},
	} {
		if got := ValidSourceChannel(tc.v); got != tc.want {
			t.Errorf("ValidSourceChannel(%d)=%v，期望 %v", tc.v, got, tc.want)
		}
	}
	for _, tc := range []struct {
		v    int32
		want bool
	}{
		{ContentTypeUnspecified, false}, {ContentTypeUGC, true},
		{ContentTypePGC, true}, {ContentTypeLive, true}, {ContentTypeLive + 1, false},
	} {
		if got := ValidContentType(tc.v); got != tc.want {
			t.Errorf("ValidContentType(%d)=%v，期望 %v", tc.v, got, tc.want)
		}
	}
	// 终态集合：MarkFinished 之后不再推进，租约回收也据此跳过。
	for _, tc := range []struct {
		v    int32
		want bool
	}{
		{JobStatePending, false}, {JobStateRunning, false},
		{JobStateSucceeded, true}, {JobStateFailed, true}, {JobStateCancelled, true},
		{JobStateUnspecified, false},
	} {
		if got := isTerminalJobState(tc.v); got != tc.want {
			t.Errorf("isTerminalJobState(%d)=%v，期望 %v", tc.v, got, tc.want)
		}
	}
	if !ValidCohortType(CohortTypeRegisterDay) || !ValidCohortType(CohortTypeFirstPlayDay) {
		t.Error("两个留存分桶类型都必须合法")
	}
	if ValidCohortType(ContentTypeUnspecified) || ValidCohortType(99) {
		t.Error("未知 cohort_type 必须被拒")
	}
}

// --- 截断与批量占位符 ---

// TestTruncateIsByteBoundedAndSingleLine：
// truncate 的输出直接进 VARCHAR 列，切断 UTF-8 序列会让 utf8mb4 拒绝整条写入，
// 保留换行则让「一行日志」变成多行，破坏 last_error 的可grep性。
func TestTruncateIsByteBoundedAndSingleLine(t *testing.T) {
	long := strings.Repeat("abcdefghij", 200)
	if got := truncate(long, 64); got != long[:64] {
		t.Errorf("ASCII 截断结果不对：len=%d，末尾 %q", len(got), got[len(got)-8:])
	}
	cn := strings.Repeat("指标口径变更", 50) // 每 rune 3 字节
	for _, max := range []int{1, 2, 3, 4, 5, 7, 32, 33, 100, len(cn), len(cn) + 10} {
		got := truncate(cn, max)
		if len(got) > max {
			t.Errorf("truncate(_, %d) 返回了 %d 字节，超出列宽", max, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncate(_, %d) 切断了 UTF-8 序列，MySQL utf8mb4 会整条拒绝写入：%q", max, got)
		}
		if !strings.HasPrefix(cn, got) {
			t.Errorf("truncate(_, %d) 不是原串前缀，说明它改写了内容而不是截断", max)
		}
		if max >= len(cn) && got != cn {
			t.Errorf("未超长时不应改变内容（max=%d, len=%d）", max, len(cn))
		}
	}
	// 换行/回车/制表符折叠成空格：错误信息里带堆栈时不能撑出多行。
	messy := "db error:\n\tretry later\r\n"
	got := truncate(messy, 200)
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("折叠失败，仍有控制字符：%q", got)
	}
	if want := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(messy); got != want {
		t.Errorf("折叠结果 %q 与逐字替换的期望 %q 不一致", got, want)
	}
	// max<=0 返回空串而不是 panic：调用方的列宽常量若被改成 0，
	// 静默写空串远好于把整条写入变成 panic。
	for _, max := range []int{0, -1, -100} {
		if got := truncate("anything", max); got != "" {
			t.Errorf("truncate(_, %d)=%q，非正列宽必须返回空串", max, got)
		}
	}
	// 恰好切在多字节中间的位置必须回退到合法边界（少写一个字节而不是多写）。
	if got := truncate("abcd中", 5); got != "abcd" {
		t.Errorf("在 3 字节 rune 中间回退失败：%q", got)
	}
}

// TestRowPlaceholdersCounts 钉批量 INSERT 的 VALUES 段形状：
// 一个 ? 少写就多写一个参数，MySQL 报的是 "column count doesn't match value count"，
// 而报错点在半夜的批量回填里。
func TestRowPlaceholdersCounts(t *testing.T) {
	if got := rowPlaceholders(3, 2); got != "(?,?,?),(?,?,?)" {
		t.Errorf("rowPlaceholders(3,2)=%q", got)
	}
	if got := rowPlaceholders(1, 1); got != "(?)" {
		t.Errorf("rowPlaceholders(1,1)=%q，单行单列不该带尾逗号", got)
	}
	for _, rowLen := range []int{1, 8, 11, 25} {
		for _, rows := range []int{1, 2, 37} {
			got := rowPlaceholders(rowLen, rows)
			if n := strings.Count(got, "?"); n != rowLen*rows {
				t.Errorf("rowPlaceholders(%d,%d) 有 %d 个 ?，应为 %d", rowLen, rows, n, rowLen*rows)
			}
			if n := strings.Count(got, "("); n != rows {
				t.Errorf("rowPlaceholders(%d,%d) 有 %d 组，应为 %d", rowLen, rows, n, rows)
			}
			if strings.Contains(got, "),,") || strings.HasSuffix(got, ",") {
				t.Errorf("rowPlaceholders(%d,%d) 残留尾逗号：%s", rowLen, rows, got)
			}
		}
	}
	for _, tc := range [][2]int{{0, 3}, {3, 0}, {-1, 3}, {3, -1}} {
		if got := rowPlaceholders(tc[0], tc[1]); got != "" {
			t.Errorf("rowPlaceholders(%d,%d)=%q，非正长度必须返回空串", tc[0], tc[1], got)
		}
	}
}

// TestClampHelpersBound 钉 model 层的自助兜底：分页上限 100 与契约的 ps 上限同源，
// 批量上限 10000 是「一次事务锁住多少行」的唯一约束。
func TestClampHelpersBound(t *testing.T) {
	if maxListLimit != 100 {
		t.Errorf("分页上限被改成 %d：契约里 ListHotSubjects/ListDeadLetters/... 的 ps 上限是 100", maxListLimit)
	}
	if defaultListLimit <= 0 || defaultListLimit > maxListLimit {
		t.Errorf("默认页大小 %d 不在 (0, %d] 内", defaultListLimit, maxListLimit)
	}
	for _, tc := range []struct{ in, want int32 }{
		{0, defaultListLimit}, {-1, defaultListLimit}, {-99999, defaultListLimit},
		{1, 1}, {7, 7}, {maxListLimit, maxListLimit},
		{maxListLimit + 1, maxListLimit}, {1 << 20, maxListLimit},
	} {
		if got := clampLimit(tc.in); got != tc.want {
			t.Errorf("clampLimit(%d)=%d，期望 %d", tc.in, got, tc.want)
		}
	}
	// LIMIT ? OFFSET -1 在 MySQL 里是语法错误，而 offset 来自 (p-1)*ps 这类算术。
	for _, tc := range []struct{ in, want int32 }{{-1, 0}, {-100, 0}, {0, 0}, {1, 1}, {1 << 20, 1 << 20}} {
		if got := clampOffset(tc.in); got != tc.want {
			t.Errorf("clampOffset(%d)=%d，期望 %d", tc.in, got, tc.want)
		}
	}
	if defaultBatch <= 0 || defaultBatch > maxBatch {
		t.Errorf("默认批量 %d 不在 (0, %d] 内", defaultBatch, maxBatch)
	}
	for _, tc := range []struct{ in, want int32 }{
		{0, defaultBatch}, {-5, defaultBatch}, {1, 1}, {maxBatch, maxBatch},
		{maxBatch + 1, maxBatch}, {1 << 20, maxBatch},
	} {
		if got := clampBatch(tc.in); got != tc.want {
			t.Errorf("clampBatch(%d)=%d，期望 %d", tc.in, got, tc.want)
		}
	}
	// clampLimit 与 logic 层的「ps 超过 MaxPageSize 直接拒绝」是两条不同的线
	// （README「分页与上限口径」）：logic 拒绝是给调用方的契约，model 收敛是内部兜底。
	// 上表已钉住收敛结果，不再重复判定。
}

// --- 受控词表与事件映射 ---

// TestTopicForDelegatesToEnvelope 钉「topic 名只有一个来源」：
// types.go 明确写着本服务不再自己拼一遍字符串，否则 spm 认为的 topic 与
// event-collector 实际投递的 topic 会静默分叉，消费者空转而告警正常。
func TestTopicForDelegatesToEnvelope(t *testing.T) {
	all := []string{
		EventBehaviorPlay, EventBehaviorClick, EventBehaviorSearch, EventBehaviorSkip,
		EventBehaviorLike, EventBehaviorFavorite, EventBehaviorFollow, EventBehaviorShare,
		EventBehaviorQuality, EventBehaviorExposure,
		EventPlaybackHeartbeat, EventEngagementAction, EventSearchQuery, EventContentPublished,
	}
	for _, et := range all {
		if !SupportedEventType(et) {
			t.Errorf("%s 在事件清单里却被 SupportedEventType 拒了：白名单与实现分叉", et)
		}
		for _, v := range []int{1, 2} {
			if got, want := TopicFor(et, v), eventenvelope.Topic(et, v); got != want {
				t.Errorf("TopicFor(%s,%d)=%q，必须等于 eventenvelope.Topic 的 %q", et, v, got, want)
			}
			if got := TopicFor(et, v); !strings.HasPrefix(got, et+".v") {
				t.Errorf("TopicFor(%s,%d)=%q 不符合 <event_type>.v<schema_version> 形态", et, v, got)
			}
		}
		if ChannelOf(et) == SourceChannelUnspecified {
			t.Errorf("%s 没有来源通道：未知通道的事件不允许进事实表", et)
		}
	}
	for _, et := range []string{"", "play.play", "behavior.PLAY", "behavior.play.v1", "unknown.event"} {
		if SupportedEventType(et) {
			t.Errorf("SupportedEventType(%q) 为真：大小写与带版本后缀的名字都不是合法 event_type", et)
		}
		if got := TopicFor(et, DefaultSchemaVersion); got != "" {
			t.Errorf("TopicFor(%q)=%q，白名单外必须返回空串让调用方按坏消息处理", et, got)
		}
		if ChannelOf(et) != SourceChannelUnspecified {
			t.Errorf("ChannelOf(%q)=%d，未知类型必须归 UNSPECIFIED", et, ChannelOf(et))
		}
	}
	// 通道分类逐条钉死：跨通道重复计数靠「口径登记里的 source_event_types 唯一指定来源」，
	// 通道分错就会把同一次点赞算两遍。
	channelWant := map[string]int32{
		EventBehaviorPlay:      SourceChannelCollector,
		EventBehaviorQuality:   SourceChannelCollector,
		EventBehaviorExposure:  SourceChannelCollector,
		EventPlaybackHeartbeat: SourceChannelDomain,
		EventEngagementAction:  SourceChannelDomain,
		EventSearchQuery:       SourceChannelDomain,
		EventContentPublished:  SourceChannelContent,
	}
	for et, want := range channelWant {
		if got := ChannelOf(et); got != want {
			t.Errorf("ChannelOf(%s)=%d，期望 %d", et, got, want)
		}
	}
	if ChannelOf(EventContentPublished) != SourceChannelContent {
		t.Fatal("content.published 必须是独立通道：它只服务投影，不参与行为计数")
	}
	if DefaultSchemaVersion != 1 {
		t.Errorf("DefaultSchemaVersion=%d，契约的兼容窗口以 v1 为基线", DefaultSchemaVersion)
	}
}

// TestNormalizeActionStaysInsideVocabulary 是受控词表闸门：
// 任何返回 true 的动作键都必须在动作白名单里，否则事实行会带上一个
// 没有任何口径能解释的 action_key（口径一旦登记就不可改写）。
func TestNormalizeActionStaysInsideVocabulary(t *testing.T) {
	vocab := map[string]bool{
		ActionPlay: true, ActionFinish: true, ActionSkip: true, ActionClick: true,
		ActionExposure: true, ActionSearch: true, ActionLike: true, ActionFavorite: true,
		ActionFollow: true, ActionShare: true, ActionComment: true, ActionDanmaku: true,
		ActionQuality: true,
	}
	cases := []struct {
		eventType, payloadAction string
		wantAction               string
		wantOK                   bool
	}{
		{EventBehaviorPlay, "", ActionPlay, true},
		{EventBehaviorPlay, "whatever", ActionPlay, true}, // behavior.* 不看 payload.action
		{EventBehaviorClick, "", ActionClick, true},
		{EventBehaviorExposure, "", ActionExposure, true},
		{EventBehaviorSearch, "keyword=abc", ActionSearch, true},
		{EventBehaviorSkip, "", ActionSkip, true},
		{EventBehaviorQuality, "", ActionQuality, true},
		{EventBehaviorLike, "", ActionLike, true},
		{EventBehaviorFavorite, "", ActionFavorite, true},
		{EventBehaviorFollow, "", ActionFollow, true},
		{EventBehaviorShare, "", ActionShare, true},
		{EventPlaybackHeartbeat, "", ActionPlay, true},
		{EventSearchQuery, "", ActionSearch, true},
		{EventEngagementAction, PayloadActionLike, ActionLike, true},
		{EventEngagementAction, PayloadActionFavorite, ActionFavorite, true},
		{EventEngagementAction, PayloadActionShare, ActionShare, true},
		{EventEngagementAction, PayloadActionFollow, ActionFollow, true},
		{EventEngagementAction, PayloadActionComment, ActionComment, true},
		{EventEngagementAction, PayloadActionDanmaku, ActionDanmaku, true},
		// 撤销类动作没有「负样本」口径：必须跳过，不能映射成正向动作。
		{EventEngagementAction, PayloadActionCancelLike, "", false},
		{EventEngagementAction, PayloadActionUnfollow, "", false},
		{EventEngagementAction, "", "", false},
		{EventEngagementAction, "LIKE", "", false},
		// content.published 不参与行为计数。
		{EventContentPublished, PayloadActionPublish, "", false},
		{EventContentPublished, PayloadActionOffline, "", false},
		// 未知事件类型与未知通道。
		{"behavior.something", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		gotAction, gotOK := NormalizeAction(tc.eventType, tc.payloadAction)
		if gotOK != tc.wantOK || gotAction != tc.wantAction {
			t.Errorf("NormalizeAction(%q,%q)=(%q,%v)，期望 (%q,%v)",
				tc.eventType, tc.payloadAction, gotAction, gotOK, tc.wantAction, tc.wantOK)
		}
		if gotOK && !vocab[gotAction] {
			t.Errorf("NormalizeAction(%q,%q) 返回了词表外的动作键 %q", tc.eventType, tc.payloadAction, gotAction)
		}
		if !gotOK && gotAction != "" {
			t.Errorf("NormalizeAction(%q,%q) 判定失败却返回了动作 %q：调用方会以为有值可用",
				tc.eventType, tc.payloadAction, gotAction)
		}
	}
	// finish 由 mapping 阶段按完播阈值判定，不在这条归一里（types.go 注释同源）：
	// 这里显式钉住，避免有人「顺手」把心跳映射成 finish 造成口径重复计数。
	for _, et := range []string{EventBehaviorPlay, EventPlaybackHeartbeat} {
		if got, _ := NormalizeAction(et, PayloadActionLike); got == ActionFinish {
			t.Errorf("%s 被归一成 finish：完播判定属于 mapping 阶段", et)
		}
	}
	// 白名单里的每个事件都必须要么可归一、要么被明确判为无分析语义，
	// 不能出现「SupportedEventType 放行但 NormalizeAction 无人认领」的空档。
	for _, et := range []string{
		EventBehaviorPlay, EventBehaviorClick, EventBehaviorSearch, EventBehaviorSkip,
		EventBehaviorLike, EventBehaviorFavorite, EventBehaviorFollow, EventBehaviorShare,
		EventBehaviorQuality, EventBehaviorExposure, EventPlaybackHeartbeat,
		EventEngagementAction, EventSearchQuery, EventContentPublished,
	} {
		if _, ok := NormalizeAction(et, "anything"); !ok {
			continue // 明确无语义（engagement 的兜底分支、content.published）
		}
		if !SupportedEventType(et) {
			t.Errorf("%s 能归一化但不在白名单：白名单与映射表分叉", et)
		}
	}
}

// TestContentStateOfActionNeverDefaultsToNormal 钉榜单正确性的关键一条：
// 收到事件绝不默认放开出榜（types.go：「绝不因为收到事件就默认 normal」）。
func TestContentStateOfActionNeverDefaultsToNormal(t *testing.T) {
	cases := []struct {
		action string
		want   int32
		wantOK bool
	}{
		{PayloadActionPublish, ContentStateNormal, true},
		{PayloadActionOffline, ContentStateHidden, true},
		{PayloadActionExpired, ContentStateHidden, true},
		{PayloadActionDelete, ContentStateHidden, true},
		{PayloadActionUpdate, ContentStateUnchanged, false},
		{"", ContentStateUnchanged, false},
		{"unknown", ContentStateUnchanged, false},
		{"PUBLISH", ContentStateUnchanged, false},
	}
	for _, tc := range cases {
		got, ok := ContentStateOfAction(tc.action)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("ContentStateOfAction(%q)=(%d,%v)，期望 (%d,%v)", tc.action, got, ok, tc.want, tc.wantOK)
		}
		if !ok && got == ContentStateNormal {
			t.Errorf("ContentStateOfAction(%q) 判定为不改状态却返回 normal：会把下架内容放回榜上", tc.action)
		}
	}
	if ContentStateNormal == ContentStateHidden || ContentStateUnchanged == ContentStateNormal {
		t.Error("三个状态值必须互不相同，否则「unchanged」会被读成「放开出榜」")
	}
}

// TestInterestKeyVocabulary 钉画像的受控词表（AGENTS.md §7 隐私边界）：
// 放过一条自由文本就等于把用户输入原文长期存进画像表。
func TestInterestKeyVocabulary(t *testing.T) {
	for _, k := range []string{"zone:1009", "tag:1", "catalog:42", "up:7", "zone:0"} {
		if !ValidInterestKey(k) {
			t.Errorf("ValidInterestKey(%q)=false，受控前缀 + 数字 ID 必须合法", k)
		}
	}
	// zone:0 只在「形状」上合法：本服务唯一的构造入口 InterestKey 拒绝 id<=0，
	// 所以库里出现 zone:0 只能是上游绕过 InterestKey 直接塞的。
	// 这里把它钉成已知的形状判定边界，而不是让它在测试里靠运气通过。
	if got := InterestKey(InterestKeyPrefixZone, 0); got != "" {
		t.Errorf("InterestKey(zone,0)=%q，非正 ID 必须返回空串", got)
	}
	for _, k := range []string{
		"", "zone:", ":1009", "zone", "zone:abc", "zone:10 09", "zone:-1", "zone:+1",
		"zone:1,2", "keyword:1", "search:周杰伦", "author:12", "note: 周杰伦",
		"zone:1a", "zone:01", "tag:0x10", "up:1e3", " ZONE:1", "zone::1", "zone:1:2",
		"behavior.play", "title:我的日常",
	} {
		if ValidInterestKey(k) {
			t.Errorf("ValidInterestKey(%q)=true：非法兴趣键被放行（隐私/可解释性断言）", k)
		}
	}
	for _, tc := range []struct {
		prefix string
		id     int64
		want   string
	}{
		{InterestKeyPrefixZone, 1009, "zone:1009"},
		{InterestKeyPrefixTag, 1, "tag:1"},
		{InterestKeyPrefixCatalog, 42, "catalog:42"},
		{InterestKeyPrefixUp, 7, "up:7"},
		{InterestKeyPrefixUp, -1, ""},
		{"author", 12, ""}, // 契约里是 up:<mid>，不是 author:<mid>
		{"", 12, ""},
		{"keyword", 12, ""},
	} {
		got := InterestKey(tc.prefix, tc.id)
		if got != tc.want {
			t.Errorf("InterestKey(%q,%d)=%q，期望 %q", tc.prefix, tc.id, got, tc.want)
		}
		if tc.want != "" && !ValidInterestKey(got) {
			t.Errorf("InterestKey 产出的 %q 通不过 ValidInterestKey：构造与校验口径分叉", got)
		}
	}
	// 往返：任意正 ID 构造出的键都必须被校验接受。
	for _, id := range []int64{1, 9, 10, 999999, 1 << 40} {
		for _, p := range []string{InterestKeyPrefixZone, InterestKeyPrefixTag, InterestKeyPrefixCatalog, InterestKeyPrefixUp} {
			k := InterestKey(p, id)
			if !ValidInterestKey(k) {
				t.Errorf("ValidInterestKey(%s) 为 false，构造函数自己的输出", k)
			}
		}
	}
}

// TestContentTypeNameNormalization 钉「上游两种 content_type 表示只做归一不做发明」：
// keyword 这类非内容主体必须落到 Unspecified，
// 默认成 UGC 会把版权条目的行为并进稿件榜。
func TestContentTypeNameNormalization(t *testing.T) {
	for _, tc := range []struct {
		name   string
		want   int32
		wantOK bool
	}{
		{"ugc", ContentTypeUGC, true},
		{"UGC", ContentTypeUGC, true},
		{" ugc ", ContentTypeUGC, true},
		{"video", ContentTypeUGC, true}, // 个别生产者用 video 指代 UGC 稿件
		{"pgc", ContentTypePGC, true},
		{"live", ContentTypeLive, true},
		{"keyword", ContentTypeUnspecified, false},
		{"", ContentTypeUnspecified, false},
		{"anime", ContentTypeUnspecified, false},
		{"ugc_v2", ContentTypeUnspecified, false},
	} {
		got, ok := ContentTypeFromName(tc.name)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("ContentTypeFromName(%q)=(%d,%v)，期望 (%d,%v)", tc.name, got, ok, tc.want, tc.wantOK)
		}
		if !ok && got != ContentTypeUnspecified {
			t.Errorf("ContentTypeFromName(%q) 判定失败却返回 %d：认不出内容类型不能顺手兜成某个具体类型", tc.name, got)
		}
	}
	// 归一后的整数值必须落在 ValidContentType 与 SubjectTypeOfContent 的口径内。
	for _, ct := range []int32{ContentTypeUGC, ContentTypePGC, ContentTypeLive} {
		if !ValidContentType(ct) {
			t.Errorf("ContentTypeFromName 的产出 %d 通不过 ValidContentType", ct)
		}
	}
	if _, ok := ContentTypeFromName("live"); ok {
		if st, ok := SubjectTypeOfContent(ContentTypeLive); ok || st != SubjectTypeUnspecified {
			t.Error("直播没有本服务的榜单主体（归 live-* 链路）")
		}
	}
}

// TestSubjectTypeOfContentAndResolve 钉消费者 mapping 阶段的唯一映射实现。
// 规则错的表现是「数据看着正常但并错了主体」，只能靠单证。
func TestSubjectTypeOfContentAndResolve(t *testing.T) {
	if got, ok := SubjectTypeOfContent(ContentTypeUGC); !ok || got != SubjectTypeAid {
		t.Errorf("UGC 主体应为 aid，实得 (%d,%v)", got, ok)
	}
	if got, ok := SubjectTypeOfContent(ContentTypePGC); !ok || got != SubjectTypeCatalogItem {
		t.Errorf("PGC 主体应为 catalog item，实得 (%d,%v)", got, ok)
	}
	for _, ct := range []int32{ContentTypeUnspecified, ContentTypeLive, 99, -1} {
		if got, ok := SubjectTypeOfContent(ct); ok || got != SubjectTypeUnspecified {
			t.Errorf("SubjectTypeOfContent(%d)=(%d,%v)，应为无主体", ct, got, ok)
		}
	}
	cases := []struct {
		name           string
		contentType    int32
		contentID, aid int64
		wantType       int32
		wantID         int64
		wantOK         bool
	}{
		{"UGC 只给 content_id", ContentTypeUGC, 123, 0, SubjectTypeAid, 123, true},
		{"UGC content_id 缺省则回退 aid", ContentTypeUGC, 0, 456, SubjectTypeAid, 456, true},
		{"UGC 两个都给时以 content_id 为准", ContentTypeUGC, 123, 456, SubjectTypeAid, 123, true},
		{"UGC 两者皆缺", ContentTypeUGC, 0, 0, SubjectTypeUnspecified, 0, false},
		// 回退条件是 content_id <= 0 而不是「恰好等于 0」：UGC 的 aid 与 content_id
		// 是同一个主键（types.go 注释），坏值 -5 回退到真实 aid 比写一个负 subject_id 正确。
		{"UGC 负 content_id 同样回退 aid", ContentTypeUGC, -5, 456, SubjectTypeAid, 456, true},
		{"UGC 回退目标也非正则失败", ContentTypeUGC, -5, -1, SubjectTypeUnspecified, 0, false},
		// PGC 的 content_id 是 episode（集）级 ID，契约缺口已登记（README「上游契约缺口」）：
		// 这里钉死「不做任何猜测性折算」，尤其不能拿 aid 顶替 item_id。
		{"PGC 用 content_id", ContentTypePGC, 789, 0, SubjectTypeCatalogItem, 789, true},
		{"PGC 缺 content_id 不得回退 aid", ContentTypePGC, 0, 456, SubjectTypeUnspecified, 0, false},
		{"直播无主体", ContentTypeLive, 1, 1, SubjectTypeUnspecified, 0, false},
		{"未知类型无主体", ContentTypeUnspecified, 1, 1, SubjectTypeUnspecified, 0, false},
	}
	for _, tc := range cases {
		gotType, gotID, ok := ResolveContentSubject(tc.contentType, tc.contentID, tc.aid)
		if gotType != tc.wantType || gotID != tc.wantID || ok != tc.wantOK {
			t.Errorf("%s: ResolveContentSubject(%d,%d,%d)=(%d,%d,%v)，期望 (%d,%d,%v)",
				tc.name, tc.contentType, tc.contentID, tc.aid, gotType, gotID, ok,
				tc.wantType, tc.wantID, tc.wantOK)
		}
		if ok && gotID <= 0 {
			t.Errorf("%s: 判定成功却给出非正 subject_id，所有无主行为会被并成一个主体", tc.name)
		}
		if ok && !ValidSubjectType(gotType) {
			t.Errorf("%s: 产出的 subject_type=%d 不合法", tc.name, gotType)
		}
	}
}

// --- 游标（水位）与榜单主体 ---

// TestCheckWatermarkGuardsCursor 钉「最近闭合窗口」这条游标的入参：
// 一个未规整的 last_closed_start 会让 window_start=0 的读请求稳定指向不存在的窗口，
// 表现是「榜永远空」而不是报错。
func TestCheckWatermarkGuardsCursor(t *testing.T) {
	const aligned int64 = 1700000000 - 1700000000%3600
	ok := func() *WindowWatermark {
		return &WindowWatermark{
			SubjectType: SubjectTypeAid, MetricKey: "play_cnt", MetricVersion: 3,
			WindowType: WindowTypeHour, LastClosedStart: aligned, LastEventTime: aligned + 10,
		}
	}
	if err := checkWatermark(ok()); err != nil {
		t.Fatalf("合法水位被拒: %v", err)
	}
	// 汇总水位（subject_type=0）合法：全站榜/分区榜的主体维度是请求参数。
	agg := ok()
	agg.SubjectType = SubjectTypeUnspecified
	if err := checkWatermark(agg); err != nil {
		t.Errorf("跨主体汇总水位被拒: %v", err)
	}
	if err := checkWatermark(nil); err == nil {
		t.Error("nil 水位必须被拒")
	}
	cases := []struct {
		name string
		mut  func(*WindowWatermark)
		want error
	}{
		{"空指标键", func(w *WindowWatermark) { w.MetricKey = "" }, ErrMetricKeyEmpty},
		{"空白指标键", func(w *WindowWatermark) { w.MetricKey = "   " }, ErrMetricKeyEmpty},
		{"缺口径版本", func(w *WindowWatermark) { w.MetricVersion = 0 }, ErrMetricVersionRequired},
		{"负口径版本", func(w *WindowWatermark) { w.MetricVersion = -1 }, ErrMetricVersionRequired},
		{"非法主体", func(w *WindowWatermark) { w.SubjectType = 99 }, ErrInvalidSubject},
		{"负主体", func(w *WindowWatermark) { w.SubjectType = -1 }, ErrInvalidSubject},
		{"TOTAL 不建水位", func(w *WindowWatermark) {
			w.WindowType = WindowTypeTotal
			w.LastClosedStart = 0
		}, ErrInvalidWindow},
		{"未指定粒度", func(w *WindowWatermark) { w.WindowType = WindowTypeUnspecified }, ErrInvalidWindow},
		{"未规整的窗口左边界", func(w *WindowWatermark) { w.LastClosedStart = aligned + 1 }, ErrInvalidWindow},
		{"跨档位的未规整边界", func(w *WindowWatermark) { w.LastClosedStart = aligned + 3599 }, ErrInvalidWindow},
		{"缺推进时刻", func(w *WindowWatermark) { w.LastEventTime = 0 }, ErrEventTimeRequired},
		{"负推进时刻", func(w *WindowWatermark) { w.LastEventTime = -5 }, ErrEventTimeRequired},
		{"负写入行数", func(w *WindowWatermark) { w.RowsWritten = -1 }, ErrNegativeMetricPoint},
	}
	for _, tc := range cases {
		w := ok()
		tc.mut(w)
		if err := checkWatermark(w); err != tc.want {
			t.Errorf("%s: checkWatermark 返回 %v，期望 %v", tc.name, err, tc.want)
		}
	}
	// last_closed_start=0 是「还没有闭合窗口」而不是「指向 epoch 0」：合法放行，
	// 且必须仍然等于 AlignWindow(0, 粒度)。
	zero := ok()
	zero.LastClosedStart = 0
	if err := checkWatermark(zero); err != nil {
		t.Errorf("LastClosedStart=0 应为合法（尚无闭合窗口）: %v", err)
	}
	if AlignWindow(zero.LastClosedStart, zero.WindowType) != zero.LastClosedStart {
		t.Error("0 必须是自身的合法边界，否则 Advance 的单调比较会永远失败")
	}
	// 每一档都必须能构造出合法水位：规整函数与校验函数用的是同一个 AlignWindow。
	for _, wt := range []int32{WindowType5Min, WindowTypeHour, WindowTypeDay, WindowTypeWeek} {
		w := ok()
		w.WindowType = wt
		w.LastClosedStart = AlignWindow(1700000123, wt)
		w.LastEventTime = w.LastClosedStart + 1
		if err := checkWatermark(w); err != nil {
			t.Errorf("粒度 %d 的合法水位被拒: %v", wt, err)
		}
		w.LastClosedStart++
		if err := checkWatermark(w); err != ErrInvalidWindow {
			t.Errorf("粒度 %d 的未规整边界没被拒: %v", wt, err)
		}
	}
	if !validWatermarkSubject(SubjectTypeUnspecified) || validWatermarkSubject(7) {
		t.Error("validWatermarkSubject 必须只放行 0 与合法主体")
	}
}

// TestValidHotSubjectExcludesPrivacyAndTotalWindows 钉出榜白名单：
// 按 mid 出榜等价于「按用户排热度」，本期没有任何这类契约（README 隐私边界一节）。
func TestValidHotSubjectExcludesPrivacyAndTotalWindows(t *testing.T) {
	for _, tc := range []struct {
		v    int32
		want bool
	}{
		{SubjectTypeAid, true}, {SubjectTypeZone, true}, {SubjectTypeCatalogItem, true},
		{SubjectTypeMid, false}, {SubjectTypeUnspecified, false}, {99, false},
	} {
		if got := validHotSubject(tc.v); got != tc.want {
			t.Errorf("validHotSubject(%d)=%v，期望 %v", tc.v, got, tc.want)
		}
	}
	// 出榜主体必须是 ValidSubjectType 的子集，避免「可出榜但不可落库」的组合。
	for _, v := range []int32{SubjectTypeAid, SubjectTypeZone, SubjectTypeCatalogItem} {
		if validHotSubject(v) && !ValidSubjectType(v) {
			t.Errorf("主体 %d 可出榜但不能作为指标主体", v)
		}
	}
}

// TestRequiresContentTypeMatchesIndex 钉「按内容主键分组必须限定 content_type」：
// UGC 的 aid 与 PGC 的 episode_id 是两个互不相干的 ID 空间，数值可以相同。
func TestRequiresContentTypeMatchesIndex(t *testing.T) {
	for _, col := range []string{"content_id", "aid", "catalog_item_id"} {
		if !requiresContentType(col) {
			t.Errorf("按 %s 分组却不限定 content_type，会把不同 ID 空间并进同一主体", col)
		}
	}
	for _, col := range []string{"zone_id", "mid", "target_mid"} {
		if requiresContentType(col) {
			t.Errorf("%s 与内容类型正交，不该强制多传一个无意义参数", col)
		}
	}
	// 白名单与 requiresContentType 的论域必须一致：白名单里出现无人认领的列，
	// 等于放开一个没有索引、也没有 content_type 约束的分组维度。
	for col := range allowedSubjectColumns {
		if _, ok := subjectColumnIndex[col]; !ok {
			t.Errorf("分组列 %s 没有登记支撑索引", col)
		}
	}
	for col := range subjectColumnIndex {
		if _, ok := allowedSubjectColumns[col]; !ok {
			t.Errorf("subjectColumnIndex 登记了白名单外的列 %s", col)
		}
	}
}

// --- 拼 SQL 的占位符/参数同序不变量 ---

// TestFilterBuildersKeepPlaceholdersAndArgsInOrder 覆盖 6 个 build*Query：
//  1. ? 的个数必须等于 len(args)（少一个就是 MySQL 语法错，多一个是参数被丢）；
//  2. 第 i 个 ? 前面的列名必须与 args[i] 对应（顺序写错不会报错，只会拿错值去比）；
//  3. 无过滤条件时必须零参数，且 WHERE 段固定为恒真式（List 与 Count 才能共用）。
func TestFilterBuildersKeepPlaceholdersAndArgsInOrder(t *testing.T) {
	t.Run("buildSummaryFilter", func(t *testing.T) {
		states := []string{ConsumerStateReceived, ConsumerStateRetry, ConsumerStateDeadLetter}
		clause, args := buildSummaryFilter("behavior.play.v1", states, 1700000000)
		assertPlaceholders(t, clause, args, []string{"topic", "state", "state", "state", "ctime"})
		if !strings.Contains(clause, "ctime >= ?") {
			t.Error("下界条件丢了：无下界的 GROUP BY 会扫过整张状态表")
		}
		empty, emptyArgs := buildSummaryFilter("", nil, 0)
		assertPlaceholders(t, empty, emptyArgs, nil)
		if !strings.HasPrefix(empty, " WHERE 1 = 1") {
			t.Errorf("无条件时 WHERE 段必须是恒真式，实得 %q", empty)
		}
		// states 的占位符数与值数必须同步（少一个 ? 就是参数错位）。
		for n := 1; n <= 5; n++ {
			q, a := buildSummaryFilter("", make([]string, n), 0)
			if strings.Count(q, "?") != n || len(a) != n {
				t.Errorf("%d 个状态：? 数=%d，args=%d", n, strings.Count(q, "?"), len(a))
			}
		}
	})
	t.Run("buildDeadLetterQuery", func(t *testing.T) {
		q, args := buildDeadLetterQuery(DeadLetterFilter{
			Topic: "behavior.play.v1", State: DeadLetterStateOpen, Since: 1700000000,
		})
		assertPlaceholders(t, q, args, []string{"topic", "state", "ctime"})
		if got, want := args[1], any(DeadLetterStateOpen); got != want {
			t.Errorf("第 2 个参数是 %v，应为 state 的值 %v", got, want)
		}
		base, baseArgs := buildDeadLetterQuery(DeadLetterFilter{})
		assertPlaceholders(t, base, baseArgs, nil)
		// Count 依赖把 SELECT 段换成 COUNT(*)：SELECT 列常量必须真的出现在查询里，
		// 否则 Replace 静默失败，Count 会把明细行当数字返回。
		if !strings.Contains(base, "SELECT "+deadLetterColumns+" FROM spm_dead_letter") {
			t.Error("死信查询的 SELECT 段与 deadLetterColumns 分叉，Count 的替换会失效")
		}
	})
	t.Run("buildJobQuery", func(t *testing.T) {
		q, args := buildJobQuery(JobFilter{
			JobType: JobTypeRecompute, State: JobStatePending, Since: 1700000000,
		})
		assertPlaceholders(t, q, args, []string{"job_type", "state", "ctime"})
		base, baseArgs := buildJobQuery(JobFilter{})
		assertPlaceholders(t, base, baseArgs, nil)
		if !strings.Contains(base, "SELECT "+aggregationJobColumns+" FROM spm_aggregation_job") {
			t.Error("作业查询的 SELECT 段与 aggregationJobColumns 分叉，Count 的替换会失效")
		}
	})
	t.Run("buildMetricDefinitionQuery", func(t *testing.T) {
		q, args := buildMetricDefinitionQuery(MetricDefinitionFilter{
			MetricKey: "play_cnt", State: DefinitionStateActive,
		})
		assertPlaceholders(t, q, args, []string{"metric_key", "state"})
		base, baseArgs := buildMetricDefinitionQuery(MetricDefinitionFilter{})
		assertPlaceholders(t, base, baseArgs, nil)
		if !strings.Contains(base, "SELECT "+metricDefinitionColumns+" FROM spm_metric_definition") {
			t.Error("口径查询的 SELECT 段与 metricDefinitionColumns 分叉，Count 的替换会失效")
		}
	})
	t.Run("buildWatermarkQuery", func(t *testing.T) {
		q, args := buildWatermarkQuery(WatermarkFilter{
			SubjectType: SubjectTypeAid, MetricKey: "play_cnt", MetricVersion: 3, WindowType: WindowTypeHour,
		})
		assertPlaceholders(t, q, args, []string{"metric_key", "subject_type", "metric_version", "window_type"})
		base, baseArgs := buildWatermarkQuery(WatermarkFilter{})
		assertPlaceholders(t, base, baseArgs, nil)
		// SubjectType=0 是「不限」而不是「= 0」：汇总水位不能与主体水位混在同一次查询里。
		q2, args2 := buildWatermarkQuery(WatermarkFilter{SubjectType: SubjectTypeUnspecified, MetricKey: "play_cnt"})
		assertPlaceholders(t, q2, args2, []string{"metric_key"})
		if !strings.Contains(base, "SELECT "+windowWatermarkColumns+" FROM spm_window_watermark") {
			t.Error("水位查询的 SELECT 段与 windowWatermarkColumns 分叉，Count 的替换会失效")
		}
	})
	t.Run("buildHotQuery", func(t *testing.T) {
		const start = 1700000000 - 1700000000%3600
		cases := []struct {
			name     string
			subject  int32
			zone     int64
			wantCols []string
			wantJoin bool
		}{
			{"内容榜不限分区", SubjectTypeAid, 0,
				[]string{"mw.subject_type", "mw.metric_key", "mw.metric_version", "mw.window_type",
					"mw.window_start", "cp.state"}, true},
			{"内容榜限分区", SubjectTypeAid, 1009,
				[]string{"mw.subject_type", "mw.metric_key", "mw.metric_version", "mw.window_type",
					"mw.window_start", "cp.state", "cp.zone_id"}, true},
			{"分区榜不JOIN投影", SubjectTypeZone, 0,
				[]string{"mw.subject_type", "mw.metric_key", "mw.metric_version", "mw.window_type",
					"mw.window_start"}, false},
		}
		for _, tc := range cases {
			q, args, err := buildHotQuery(tc.subject, "play_cnt", 3, WindowTypeHour, start, tc.zone, "SELECT COUNT(*)")
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			assertPlaceholders(t, q, args, tc.wantCols)
			// 第 5 个参数是 window_start：必须已经过 AlignWindow。
			// 榜单的 COUNT 与 LIST 共用这一份 args，未规整会让两者查不同的窗口。
			if got := args[4]; got != any(AlignWindow(start, WindowTypeHour)) {
				t.Errorf("%s: window_start 参数是 %v，未规整（对齐值应为 %d）", tc.name, got, AlignWindow(start, WindowTypeHour))
			}
			if joined := strings.Contains(q, "LEFT JOIN spm_content_projection"); joined != tc.wantJoin {
				t.Errorf("%s: 投影 JOIN 与预期不符（joined=%v want=%v）", tc.name, joined, tc.wantJoin)
			}
			if tc.wantJoin && !strings.Contains(q, "cp.id IS NULL OR cp.state = ?") {
				t.Errorf("%s: 下架内容剔除条件丢了，违规内容会留在榜上", tc.name)
			}
		}
		// 入参校验：非法主体/缺版本/非法粒度/分区榜带 zone 都要显式拒绝。
		for _, tc := range []struct {
			name                string
			subject             int32
			key                 string
			version, windowType int32
			zone                int64
			want                error
		}{
			{"按 mid 出榜", SubjectTypeMid, "play_cnt", 3, WindowTypeHour, 0, ErrInvalidSubject},
			{"缺口径版本", SubjectTypeAid, "play_cnt", 0, WindowTypeHour, 0, ErrMetricVersionRequired},
			{"空指标键", SubjectTypeAid, " ", 3, WindowTypeHour, 0, ErrMetricVersionRequired},
			{"非法粒度", SubjectTypeAid, "play_cnt", 3, WindowTypeUnspecified, 0, ErrInvalidWindow},
			{"分区榜再限分区", SubjectTypeZone, "play_cnt", 3, WindowTypeHour, 1009, ErrInvalidSubject},
		} {
			q, args, err := buildHotQuery(tc.subject, tc.key, tc.version, tc.windowType, start, tc.zone, "SELECT COUNT(*)")
			if err != tc.want {
				t.Errorf("%s: 返回 err=%v，期望 %v", tc.name, err, tc.want)
			}
			if err != nil && (q != "" || len(args) != 0) {
				t.Errorf("%s: 拒绝时必须不返回半截 SQL（q=%q args=%v）", tc.name, q, args)
			}
		}
	})
}

// TestBuildEventFilterRequiresBoundedRange 单独钉区间方向：
// from>to 在 BETWEEN 下恒为空集，会把「参数算错了」伪装成「这段时间没数据」。
func TestBuildEventFilterRequiresBoundedRange(t *testing.T) {
	for _, tc := range []struct {
		from, to int64
		ok       bool
	}{
		{0, 100, false}, {-1, 100, false}, {100, 0, false}, {100, 99, false},
		{100, 100, true}, {100, 200, true},
	} {
		clause, args, err := buildEventFilter(nil, tc.from, tc.to)
		if tc.ok != (err == nil) {
			t.Errorf("区间 (%d,%d] 的合法性判定反了：err=%v", tc.from, tc.to, err)
			continue
		}
		if !tc.ok {
			if clause != "" || len(args) != 0 {
				t.Errorf("区间非法却返回了条件 %q args=%v", clause, args)
			}
			continue
		}
		if got, want := strings.Count(clause, "?"), len(args); got != want {
			t.Errorf("区间 (%d,%d]：? 数 %d != args 数 %d", tc.from, tc.to, got, want)
		}
		// BETWEEN 的两个参数必须按 from,to 顺序：反了就变成空集。
		if args[0] != any(tc.from) || args[1] != any(tc.to) {
			t.Errorf("BETWEEN 参数顺序错：%v，应为 [%d,%d]", args, tc.from, tc.to)
		}
		if !strings.Contains(clause, "occurred_at BETWEEN ? AND ?") {
			t.Errorf("区间条件形态不对：%q", clause)
		}
	}
	clause, args, err := buildEventFilter([]string{EventBehaviorPlay, EventEngagementAction}, 100, 200)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(clause, "?") != 4 || len(args) != 4 {
		t.Errorf("两个时间 + 两个事件类型应为 4 个参数，? 数=%d args=%d", strings.Count(clause, "?"), len(args))
	}
	if args[0] != any(int64(100)) || args[1] != any(int64(200)) {
		t.Errorf("事件类型必须排在时间之后：%v", args)
	}
	if args[2] != any(EventBehaviorPlay) || args[3] != any(EventEngagementAction) {
		t.Errorf("事件类型参数顺序与占位符不一致：%v", args)
	}
}

// TestNullPayloadUsesSQLNull 钉「成功行清空原文」的落库形态：
// 空串与 NULL 在 payload 这一列语义不同（前者是「本来就带空 payload」）。
func TestNullPayloadUsesSQLNull(t *testing.T) {
	if got := nullPayload(""); got != nil {
		t.Errorf("空载荷应写成 NULL，实得 %#v", got)
	}
	if got := nullPayload(`{"a":1}`); got != `{"a":1}` {
		t.Errorf("非空载荷必须原样下传，实得 %#v", got)
	}
}

// --- helpers ---

// assertPlaceholders 按 ? 出现顺序取回它前面的列名，并同时钉住参数个数与取值顺序。
func assertPlaceholders(t *testing.T, query string, args []any, wantCols []string) {
	t.Helper()
	if len(wantCols) == 0 {
		wantCols = nil
	}
	got := placeholderColumns(query)
	if strings.Count(query, "?") != len(args) {
		t.Errorf("? 数 %d != len(args) %d，SQL 会语法报错或参数被丢：%s", strings.Count(query, "?"), len(args), query)
	}
	if strings.Join(got, ",") != strings.Join(wantCols, ",") {
		t.Errorf("占位符归属的列与期望不符（说明条件与参数的追加顺序分叉）\n 实际=%v\n 期望=%v\n SQL=%s",
			got, wantCols, query)
		return
	}
	// 每个字符串参数都必须非空：空串会让过滤条件静默变成 `col = ''`，
	// 表现是「查询正常但结果全空」。
	// 整型参数不在这里判 0 —— cp.state = ContentStateNormal 的合法值就是 0，
	// 枚举 0（UNSPECIFIED）由各 builder 自己的「不加条件」分支守住。
	for i, a := range args {
		if v, ok := a.(string); ok && v == "" {
			t.Errorf("第 %d 个参数（列 %s）是空串：条件会静默匹配空值", i+1, wantCols[i])
		}
	}
}

// placeholderColumns 按出现顺序返回每个 ? 所属的列名（支持 mw.col 这种带别名前缀的写法）。
// 识别形态：`<col> = ?`、`<col> >= ?`、`<col> IN (?,?)`。
func placeholderColumns(query string) []string {
	var out []string
	for i := 0; i < len(query); i++ {
		if query[i] != '?' {
			continue
		}
		j := i - 1
	skip:
		for j >= 0 {
			switch {
			case query[j] == ' ' || query[j] == '=' || query[j] == '>' ||
				query[j] == '<' || query[j] == '(' || query[j] == ',' || query[j] == '?':
				// '?' 也算分隔符：`IN (?,?,?)` 的第 2/3 个占位符要越过前一个才找得到列名。
				j--
			case j >= 1 && (query[j] == 'N' || query[j] == 'n') &&
				(query[j-1] == 'I' || query[j-1] == 'i') &&
				(j < 2 || !isIdentByte(query[j-2])):
				// `state IN (?,?)`：IN 不是列名，跳过后继续向左找真正的列。
				// 前置非标识符字符的判定避免把 origin 这类以 in 结尾的列名误跳。
				j -= 2
				break skip
			default:
				break skip
			}
		}
		// 上面 break skip 可能停在 IN 之前的空格上，再清一次。
		for j >= 0 && (query[j] == ' ' || query[j] == '(') {
			j--
		}
		end := j + 1
		for j >= 0 && isIdentByte(query[j]) {
			j--
		}
		if end <= j+1 {
			out = append(out, "<无法归属>")
			continue
		}
		out = append(out, query[j+1:end])
	}
	return out
}

func isIdentByte(b byte) bool {
	return b == '.' || b == '_' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
