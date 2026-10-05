package model

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// 本文件把 model 的「调度与状态机纯规则」钉成可执行门禁（AGENTS.md §8、§9）。
//
// 覆盖的是不连 MySQL 也必须正确的部分：
//   - 执行记录状态机矩阵（终态永不回退，人工重试只能追加 attempt）；
//   - 退避计算的上限收敛（绝不能「失败越多跑得越勤」，也不能算成负数）；
//   - 租约过期判定与 TTL 夹紧（配置矛盾时行为必须确定）；
//   - 任务定义结构自洽校验、分页收敛、枚举合法性、复合游标编解码；
//   - cron 表达式解析与下一个计划时刻计算（含时区回落）。
//
// 约定：断言全部针对「服务端真值」，任何一条放宽都必须先改 README 的语义说明。

// --- 执行状态机 ---

func TestCanTransitionMatrix(t *testing.T) {
	states := []int32{
		RunStateUnspecified, RunStatePending, RunStateRunning, RunStateRetrying,
		RunStateSucceeded, RunStateFailed, RunStateTimeout, RunStateCanceled, RunStateSkipped,
	}
	// 期望矩阵逐格写死：新增状态或改边都必须在这里显式表态，
	// 而不是让 map 少写一行就「静默不合法」。
	allowed := map[int32][]int32{
		RunStatePending:  {RunStateRunning, RunStateSkipped, RunStateCanceled, RunStateTimeout, RunStateFailed},
		RunStateRunning:  {RunStateSucceeded, RunStateRetrying, RunStateFailed, RunStateTimeout, RunStateCanceled, RunStateSkipped},
		RunStateRetrying: {RunStateRunning, RunStateFailed, RunStateTimeout, RunStateCanceled},
		// 终态与未指定：零出边。
		RunStateSucceeded: {}, RunStateFailed: {}, RunStateTimeout: {}, RunStateCanceled: {}, RunStateSkipped: {},
	}
	for _, from := range states {
		if from == RunStateUnspecified {
			// 未指定的来源状态根本不该出现在库里，必须一律拒绝。
			for _, to := range states {
				if CanTransition(from, to) {
					t.Fatalf("CanTransition(UNSPECIFIED, %s) 必须为 false", RunStateName(to))
				}
			}
			continue
		}
		want, ok := allowed[from]
		if !ok {
			t.Fatalf("测试漏了来源状态 %s", RunStateName(from))
		}
		wantSet := map[int32]struct{}{}
		for _, to := range want {
			wantSet[to] = struct{}{}
		}
		for _, to := range states {
			_, legal := wantSet[to]
			if got := CanTransition(from, to); got != legal {
				t.Errorf("CanTransition(%s, %s) = %t，期望 %t",
					RunStateName(from), RunStateName(to), got, legal)
			}
		}
	}
}

func TestTerminalStatesHaveNoOutgoingEdge(t *testing.T) {
	// 终态可追加（新行）但绝不可回改（同状态机迁移）——历史轨迹不可篡改。
	for _, terminal := range []int32{RunStateSucceeded, RunStateFailed, RunStateTimeout,
		RunStateCanceled, RunStateSkipped} {
		if !IsTerminalRunState(terminal) {
			t.Errorf("IsTerminalRunState(%s) 应为 true", RunStateName(terminal))
		}
		for _, to := range []int32{RunStatePending, RunStateRunning, RunStateRetrying, terminal} {
			if CanTransition(terminal, to) {
				t.Errorf("终态 %s 不允许迁移到 %s", RunStateName(terminal), RunStateName(to))
			}
		}
	}
	for _, live := range []int32{RunStateUnspecified, RunStatePending, RunStateRunning, RunStateRetrying} {
		if IsTerminalRunState(live) {
			t.Errorf("IsTerminalRunState(%s) 应为 false", RunStateName(live))
		}
	}
	// 越界数值不得被当成终态。
	for _, odd := range []int32{-1, 9, 100} {
		if IsTerminalRunState(odd) {
			t.Errorf("IsTerminalRunState(%d) 应为 false", odd)
		}
	}
}

// --- 重试与退避 ---

func TestRetryAllowedBoundaries(t *testing.T) {
	cases := []struct {
		attempt, maxAttempts int32
		want                 bool
		why                  string
	}{
		{1, 1, false, "max_attempts 含首次，1 表示不重试"},
		{1, 2, true, "还有第二次可用"},
		{2, 3, true, "退避链路上还能继续"},
		{3, 3, false, "最后一次失败即终态"},
		{4, 3, false, "越界的尝试号不再放行"},
		{0, 3, true, "异常 attempt 也不该卡死重试"},
		{1, 0, false, "max_attempts<1 归一为 1，等于不重试"},
		{1, -5, false, "负数配置同样归一"},
	}
	for _, c := range cases {
		if got := RetryAllowed(c.attempt, c.maxAttempts); got != c.want {
			t.Errorf("RetryAllowed(%d,%d) = %t，期望 %t（%s）", c.attempt, c.maxAttempts, got, c.want, c.why)
		}
	}
}

func TestBackoffSecondsGrowsAndConverges(t *testing.T) {
	// base=30 / max=1800 是迁移文件里的默认配置，逐次翻倍并在上限处收敛。
	cases := []struct {
		attempt, base, max int32
		want               int64
	}{
		{1, 30, 1800, 30},
		{2, 30, 1800, 60},
		{3, 30, 1800, 120},
		{6, 30, 1800, 960},
		{7, 30, 1800, 1800},  // 1920 -> 收敛到上限
		{30, 30, 1800, 1800}, // 指数溢出前就被上限拦住
		{1, 0, 1800, 1},      // base<=0 退化为 1 秒，绝不给 0（0 会变成忙等）
		{0, 30, 1800, 30},    // attempt<1 按首次处理
		{-3, 30, 1800, 30},   // 负尝试号同样按首次
		{1, 30, 0, 30},       // max<=0 表示不设上限
		{1, 30, -1, 30},      // 负上限同样视为不设上限
		{15, 60, 0, 604800},  // 无上限时收敛到 7 天硬天花板
		{40, 60, 0, 604800},  // 再多次也不会溢出成负数
	}
	for _, c := range cases {
		if got := BackoffSeconds(c.attempt, c.base, c.max); got != c.want {
			t.Errorf("BackoffSeconds(attempt=%d, base=%d, max=%d) = %d，期望 %d",
				c.attempt, c.base, c.max, got, c.want)
		}
	}
}

func TestBackoffSecondsNeverNegativeOrZero(t *testing.T) {
	// 这条不变量比逐值断言更硬：任何输入下退避都必须落在 (0, 7天] 内，
	// 否则「下次可执行时间」会早于当前时间，等于退化成忙等。
	for _, attempt := range []int32{-100, 0, 1, 2, 3, 7, 15, 63, 64, 1000} {
		for _, base := range []int32{-7, 0, 1, 30, 3600, 2147483647} {
			for _, max := range []int32{-1, 0, 1, 1800, 2147483647} {
				got := BackoffSeconds(attempt, base, max)
				if got <= 0 {
					t.Fatalf("BackoffSeconds(%d,%d,%d) = %d，必须为正", attempt, base, max, got)
				}
				if got > 86400*7 {
					t.Fatalf("BackoffSeconds(%d,%d,%d) = %d，超过 7 天天花板", attempt, base, max, got)
				}
				if max > 0 && got > int64(max) {
					t.Fatalf("BackoffSeconds(%d,%d,%d) = %d，超过声明的上限", attempt, base, max, got)
				}
			}
		}
	}
}

func TestNextRetryAt(t *testing.T) {
	const now int64 = 1_700_000_000
	if got, want := NextRetryAt(now, 3, 30, 1800), now+int64(BackoffSeconds(3, 30, 1800)); got != want {
		t.Errorf("NextRetryAt 必须等于 now+BackoffSeconds，得到 %d/%d", got, want)
	}
	if got := NextRetryAt(now, 1, 30, 1800); got != now+30 {
		t.Errorf("NextRetryAt(首次) = %d，期望 %d", got, now+30)
	}
	// 上限收敛后仍要晚于 now：否则调用方会以为「立刻可重试」。
	if got := NextRetryAt(now, 50, 30, 1800); got <= now {
		t.Errorf("NextRetryAt(%d) 必须晚于 now=%d", got, now)
	}
}

// --- 租约时间语义 ---

func TestLeaseExpired(t *testing.T) {
	const now int64 = 1_700_000_000
	cases := []struct {
		expireAt int64
		want     bool
		why      string
	}{
		{now, true, "等于 now 即视为已过期（SQL 侧条件也是 expire_at <= now）"},
		{now - 1, true, "已过期"},
		{now + 1, false, "仍在有效期内"},
		{0, true, "0 表示从未持有或已释放，时间维度上可被抢占"},
		{-5, true, "负值不可能仍在持有"},
	}
	for _, c := range cases {
		if got := LeaseExpired(c.expireAt, now); got != c.want {
			t.Errorf("LeaseExpired(%d,%d) = %t，期望 %t（%s）", c.expireAt, now, got, c.want, c.why)
		}
	}
}

func TestClampTTL(t *testing.T) {
	cases := []struct{ ttl, min, max, want int64 }{
		{300, 30, 86400, 300},
		{5, 30, 86400, 30},
		{999999, 30, 86400, 86400},
		{10, 30, 20, 10}, // 配置矛盾（min>max）时保留原值，由调用方的校验报错
		{0, 0, 0, 0},
	}
	for _, c := range cases {
		if got := ClampTTL(c.ttl, c.min, c.max); got != c.want {
			t.Errorf("ClampTTL(%d,%d,%d) = %d，期望 %d", c.ttl, c.min, c.max, got, c.want)
		}
	}
}

// --- 任务定义结构校验 ---

func validDefinition() *TaskDefinition {
	return &TaskDefinition{
		TaskKey: "rights.expire_scan", Handler: "rights.expire_scan",
		ScheduleType: ScheduleTypeCron, CronExpr: "0 3 * * *", Timezone: "UTC",
		TimeoutSeconds: 300, MaxAttempts: 3, RetryBaseSeconds: 30, RetryMaxSeconds: 1800,
		ConcurrencyLimit: 1, LeaseTTLSeconds: 300, MisfirePolicy: MisfirePolicyFireOnceNow,
		MisfireBackfillLim: 5, State: TaskStateEnabled,
	}
}

func TestValidateTaskDefinitionAcceptsBaseline(t *testing.T) {
	if err := ValidateTaskDefinition(validDefinition()); err != nil {
		t.Fatalf("基线定义应当通过校验，得到 %v", err)
	}
	interval := validDefinition()
	interval.ScheduleType = ScheduleTypeInterval
	interval.CronExpr = ""
	interval.IntervalSeconds = 600
	if err := ValidateTaskDefinition(interval); err != nil {
		t.Fatalf("INTERVAL 定义应当通过校验，得到 %v", err)
	}
	manual := validDefinition()
	manual.ScheduleType = ScheduleTypeManual
	manual.CronExpr = ""
	manual.MaxAttempts = 1
	manual.RetryBaseSeconds = 0
	if err := ValidateTaskDefinition(manual); err != nil {
		t.Fatalf("MANUAL（不重试）定义应当通过校验，得到 %v", err)
	}
	// lease_ttl 小于 timeout 但仍在上限内：允许（长任务的 TTL 可以短于超时，靠心跳续租）。
	shortLease := validDefinition()
	shortLease.LeaseTTLSeconds = 60
	if err := ValidateTaskDefinition(shortLease); err != nil {
		t.Fatalf("lease_ttl=60 (<timeout 但 >=下限) 应当通过，得到 %v", err)
	}
}

func TestValidateTaskDefinitionRejects(t *testing.T) {
	cases := []struct {
		name  string
		patch func(*TaskDefinition)
		want  error
	}{
		{"空任务键", func(d *TaskDefinition) { d.TaskKey = "" }, ErrTaskKeyEmpty},
		{"缺 handler", func(d *TaskDefinition) { d.Handler = "" }, nil},
		{"CRON 无表达式", func(d *TaskDefinition) { d.CronExpr = "" }, ErrInvalidSchedule},
		{"INTERVAL 间隔为 0", func(d *TaskDefinition) {
			d.ScheduleType = ScheduleTypeInterval
			d.IntervalSeconds = 0
		}, ErrInvalidSchedule},
		{"INTERVAL 间隔为负", func(d *TaskDefinition) {
			d.ScheduleType = ScheduleTypeInterval
			d.IntervalSeconds = -1
		}, ErrInvalidSchedule},
		{"未知调度类型", func(d *TaskDefinition) { d.ScheduleType = 9 }, ErrInvalidSchedule},
		{"调度类型未指定", func(d *TaskDefinition) { d.ScheduleType = ScheduleTypeUnspecified }, ErrInvalidSchedule},
		{"max_attempts=0", func(d *TaskDefinition) { d.MaxAttempts = 0 }, ErrInvalidRetryPolicy},
		{"开启重试却没有退避基数", func(d *TaskDefinition) { d.RetryBaseSeconds = 0 }, ErrInvalidRetryPolicy},
		{"超时非正", func(d *TaskDefinition) { d.TimeoutSeconds = 0 }, ErrInvalidLeaseTTL},
		{"租约 TTL 非正", func(d *TaskDefinition) { d.LeaseTTLSeconds = 0 }, ErrInvalidLeaseTTL},
		{"租约 TTL 低于下限且短于超时", func(d *TaskDefinition) { d.LeaseTTLSeconds = 10 }, ErrInvalidLeaseTTL},
		{"并发上限为 0", func(d *TaskDefinition) { d.ConcurrencyLimit = 0 }, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := validDefinition()
			c.patch(d)
			err := ValidateTaskDefinition(d)
			if err == nil {
				t.Fatalf("%s 必须被拒绝", c.name)
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("错误类型不符：得到 %v，期望 %v", err, c.want)
			}
		})
	}
	if err := ValidateTaskDefinition(nil); err == nil {
		t.Error("nil 定义必须报错")
	}
}

// --- 分页 ---

func TestNormalizePageSize(t *testing.T) {
	cases := []struct {
		requested int32
		want      int
		wantErr   bool
	}{
		{0, DefaultPageSize, false},
		{-1, DefaultPageSize, false},
		{1, 1, false},
		{MaxPageSize, MaxPageSize, false},
		{MaxPageSize + 1, 0, true},
		{999999, 0, true},
	}
	for _, c := range cases {
		got, err := NormalizePageSize(c.requested)
		if c.wantErr {
			if !errors.Is(err, ErrInvalidPageLimit) {
				t.Errorf("NormalizePageSize(%d) 应返回 ErrInvalidPageLimit，得到 %v", c.requested, err)
			}
			if got != 0 {
				t.Errorf("NormalizePageSize(%d) 出错时必须返回 0，得到 %d", c.requested, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizePageSize(%d) 不应报错：%v", c.requested, err)
		}
		if got != c.want {
			t.Errorf("NormalizePageSize(%d) = %d，期望 %d", c.requested, got, c.want)
		}
	}
}

// --- 枚举与文本映射 ---

func TestStateNamesCoverEveryValidEnum(t *testing.T) {
	for s := RunStatePending; s <= RunStateSkipped; s++ {
		if !ValidRunState(s) {
			t.Errorf("ValidRunState(%d) 应为 true", s)
		}
		if name := RunStateName(s); name == "" || name == "unspecified" {
			t.Errorf("RunStateName(%d) = %q，执行状态必须有可读文本（审计里靠它猜状态）", s, name)
		}
	}
	for _, bad := range []int32{RunStateUnspecified, -1, RunStateSkipped + 1} {
		if ValidRunState(bad) {
			t.Errorf("ValidRunState(%d) 必须为 false：0 值不是合法执行状态", bad)
		}
		if RunStateName(bad) != "unspecified" {
			t.Errorf("RunStateName(%d) 应为 unspecified", bad)
		}
	}
	for s := TaskStateEnabled; s <= TaskStateDisabled; s++ {
		if !ValidTaskState(s) || TaskStateName(s) == "unspecified" {
			t.Errorf("任务状态 %d 必须合法且有名称，得到 %q", s, TaskStateName(s))
		}
	}
	if ValidTaskState(TaskStateUnspecified) {
		t.Error("ValidTaskState(0) 必须为 false")
	}
}

func TestEnumPredicatesRejectZeroAndOverflow(t *testing.T) {
	preds := []struct {
		name  string
		fn    func(int32) bool
		legal []int32
	}{
		{"ValidScheduleType", ValidScheduleType, []int32{ScheduleTypeCron, ScheduleTypeInterval, ScheduleTypeManual}},
		{"ValidMisfirePolicy", ValidMisfirePolicy, []int32{MisfirePolicyFireOnceNow, MisfirePolicySkipToNext, MisfirePolicyFireAll}},
		{"ValidTriggerType", ValidTriggerType, []int32{TriggerTypeScheduled, TriggerTypeManual, TriggerTypeRetry, TriggerTypeReplay}},
	}
	for _, p := range preds {
		for _, ok := range p.legal {
			if !p.fn(ok) {
				t.Errorf("%s(%d) 应为 true", p.name, ok)
			}
		}
		for _, bad := range []int32{0, -1, 99} {
			if p.fn(bad) {
				t.Errorf("%s(%d) 必须为 false", p.name, bad)
			}
		}
	}
	// 未指定值必须以 Unspecified 常量存在且为 0（proto3 的 0 值约定）。
	if ScheduleTypeUnspecified != 0 || MisfirePolicyUnspecified != 0 ||
		TaskStateUnspecified != 0 || RunStateUnspecified != 0 || TriggerTypeUnspecified != 0 {
		t.Fatal("各枚举的未指定值必须为 0")
	}
}

func TestAuditActionSetIsStable(t *testing.T) {
	actions := AuditActions()
	if len(actions) == 0 {
		t.Fatal("AuditActions 不能为空")
	}
	seen := map[string]struct{}{}
	for i, a := range actions {
		if _, dup := seen[a]; dup {
			t.Errorf("AuditActions 出现重复项 %s", a)
		}
		seen[a] = struct{}{}
		if !ValidAuditAction(a) {
			t.Errorf("ValidAuditAction(%s) 应为 true", a)
		}
		if i > 0 && actions[i-1] > a {
			t.Errorf("AuditActions 必须升序，%s 在 %s 之后", actions[i-1], a)
		}
	}
	for _, bad := range []string{"", "delete_everything", "Pause", "drop table"} {
		if ValidAuditAction(bad) {
			t.Errorf("ValidAuditAction(%q) 必须为 false：审计动作是白名单", bad)
		}
	}
}

// --- 复合游标编解码 ---

func TestCompositeCursorRoundTrip(t *testing.T) {
	cases := [][2]string{{"index.rebuild", ""}, {"index.rebuild", "shard=7"}, {"a", "b\x1fc"}}
	for _, c := range cases {
		cursor := CompositeCursor(c[0], c[1])
		taskKey, scopeKey, err := SplitCompositeCursor(cursor)
		if err != nil {
			t.Fatalf("SplitCompositeCursor(%q) 报错：%v", cursor, err)
		}
		if taskKey != c[0] || scopeKey != c[1] {
			t.Errorf("游标往返不一致：%v/%v -> %q -> %q/%q", c[0], c[1], cursor, taskKey, scopeKey)
		}
	}
}

func TestSplitCompositeCursorRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "no-separator", "plain text", strings.Repeat("x", 10)} {
		if _, _, err := SplitCompositeCursor(bad); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("SplitCompositeCursor(%q) 必须返回 ErrInvalidCursor，得到 %v", bad, err)
		}
	}
	// 分隔符在最前是合法结构（task_key 为空由 logic 层再判），不能和「没有分隔符」混为一谈。
	taskKey, scope, err := SplitCompositeCursor("\x1fshard=1")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if taskKey != "" || scope != "shard=1" {
		t.Errorf("解析结果不符：%q / %q", taskKey, scope)
	}
}

// --- 入库截断 ---

func TestTruncateKeepsMarker(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"", 10, ""},
		{"abc", 3, "abc"},
		{"abcd", 3, "abc"},     // max<=3 时不能塞省略号，否则超出长度
		{"abcdef", 5, "ab..."}, // 超长时以 ... 结尾，总长恰为 max
		{"abc", 0, "abc"},      // max<=0 表示不截断
		{"abc", -1, "abc"},
	}
	for _, c := range cases {
		got := truncate(c.in, c.max)
		if got != c.want {
			t.Errorf("truncate(%q,%d) = %q，期望 %q", c.in, c.max, got, c.want)
		}
		if c.max > 0 && len(got) > c.max {
			t.Errorf("truncate(%q,%d) = %q，超出上限", c.in, c.max, got)
		}
	}
	if got := truncate(strings.Repeat("x", MaxResultSummaryBytes*3), MaxResultSummaryBytes); got == "" {
		t.Error("截断结果不能为空")
	} else if len(got) != MaxResultSummaryBytes {
		t.Errorf("截断长度 %d != %d", len(got), MaxResultSummaryBytes)
	} else if !strings.HasSuffix(got, "...") {
		t.Error("截断结果必须带 ... 标记，否则运维无法分辨「就这么长」和「被切了」")
	}
}

// --- cron 表达式解析 ---

func TestParseCronExprAccepts(t *testing.T) {
	for _, expr := range []string{
		"* * * * *", "0 3 * * *", "*/15 9-17 * * 1-5", "0 0 1 jan mon",
		"30 4 * * fri-mon", "0 0 29 2 *", "0,30 * * * *", "0 0 * * 7",
	} {
		if err := ValidateCronExpr(expr); err != nil {
			t.Errorf("ValidateCronExpr(%q) 应通过：%v", expr, err)
		}
	}
}

func TestParseCronExprRejects(t *testing.T) {
	for _, expr := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * 32 * *",
		"* * * 13 *", "* * * * 8", "*/0 * * * *", "*/x * * * *", "/15 * * * *", "* * * *-",
		"a b c d e", "* * * , *", "1-2-3 * * * *",
	} {
		_, err := ParseCronExpr(expr)
		if err == nil {
			t.Errorf("ParseCronExpr(%q) 必须报错", expr)
			continue
		}
		if !errors.Is(err, ErrInvalidSchedule) {
			t.Errorf("ParseCronExpr(%q) 应返回 ErrInvalidSchedule，得到 %v", expr, err)
		}
	}
}

func TestParseCronExprWeekdaySevenIsSunday(t *testing.T) {
	// 7 与 0 都表示周日：两者必须命中同一批日期。
	seven, err := ParseCronExpr("0 12 * * 7")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	zero, err := ParseCronExpr("0 12 * * 0")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	sunday := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC) // 2026-09-13 是周日
	if sunday.Weekday() != time.Sunday {
		t.Fatalf("测试前提被破坏：%s 不是周日", sunday.Format("2006-01-02 Mon"))
	}
	if !seven.dayMatches(sunday) || !zero.dayMatches(sunday) {
		t.Errorf("0 与 7 都应命中周日，得到 %t/%t", zero.dayMatches(sunday), seven.dayMatches(sunday))
	}
	if seven.dayMatches(sunday.AddDate(0, 0, 1)) {
		t.Error("周一不该命中周日表达式")
	}
}

func TestCronDayMatchesVixieOrSemantics(t *testing.T) {
	// dom 与 dow 同时限定 -> 并集（Vixie cron 语义）。
	spec, err := ParseCronExpr("0 0 13 * fri")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	thirteen := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC) // 周日，13 号
	friday := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)   // 周五，11 号
	plain := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)    // 周一，14 号
	if !spec.dayMatches(thirteen) || !spec.dayMatches(friday) {
		t.Errorf("13 号与周五都应命中，得到 %t/%t", spec.dayMatches(thirteen), spec.dayMatches(friday))
	}
	if spec.dayMatches(plain) {
		t.Error("既不是 13 号也不是周五的日子不该命中")
	}
	// 只限定 dom -> 交集语义。
	domOnly, err := ParseCronExpr("0 0 13 * *")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if domOnly.dayMatches(friday) {
		t.Error("只限定 13 号时，周五（非 13 号）不该命中")
	}
}

func TestNextFireAfterCronUsesTimezone(t *testing.T) {
	// 同一个 UTC 时刻，在 Asia/Shanghai 与 UTC 下的「下一个 03:00」必然不同。
	d := &TaskDefinition{ScheduleType: ScheduleTypeCron, CronExpr: "0 3 * * *"}
	anchor := time.Date(2026, 9, 13, 4, 0, 0, 0, time.UTC).Unix() // 12:00 北京时间，已过当天 03:00
	d.Timezone = "UTC"
	utcNext, err := NextFireAfter(d, "UTC", anchor)
	if err != nil {
		t.Fatalf("UTC 计算失败：%v", err)
	}
	d.Timezone = "Asia/Shanghai"
	shNext, err := NextFireAfter(d, "UTC", anchor)
	if err != nil {
		t.Fatalf("Asia/Shanghai 计算失败：%v", err)
	}
	if want := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC).Unix(); utcNext != want {
		t.Errorf("UTC 下一次应为 %d，得到 %d", want, utcNext)
	}
	if want := time.Date(2026, 9, 13, 19, 0, 0, 0, time.UTC).Unix(); shNext != want {
		t.Errorf("北京时间下一次应为 %d，得到 %d", want, shNext)
	}
	if shNext >= utcNext {
		t.Error("UTC+8 的下一次 03:00 必然早于 UTC 的下一次")
	}
}

func TestNextFireAfterStrictlyAfterAnchor(t *testing.T) {
	d := &TaskDefinition{ScheduleType: ScheduleTypeCron, CronExpr: "* * * * *", Timezone: "UTC"}
	anchor := time.Date(2026, 9, 13, 12, 30, 0, 0, time.UTC).Unix()
	got, err := NextFireAfter(d, "UTC", anchor)
	if err != nil {
		t.Fatalf("计算失败：%v", err)
	}
	if got != anchor+60 {
		t.Errorf("每分钟表达式必须严格晚于 anchor，得到 %d（anchor=%d）", got, anchor)
	}
}

func TestNextFireAfterIntervalAndManual(t *testing.T) {
	const anchor int64 = 1_700_000_000
	manual := &TaskDefinition{ScheduleType: ScheduleTypeManual}
	got, err := NextFireAfter(manual, "UTC", anchor)
	if err != nil || got != 0 {
		t.Errorf("手动任务不参与到期扫描，应返回 0，得到 %d/%v", got, err)
	}
	interval := &TaskDefinition{ScheduleType: ScheduleTypeInterval, IntervalSeconds: 90}
	if got, err := NextFireAfter(interval, "UTC", anchor); err != nil || got != anchor+90 {
		t.Errorf("INTERVAL 应为 anchor+间隔，得到 %d/%v", got, err)
	}
	if _, err := NextFireAfter(&TaskDefinition{ScheduleType: ScheduleTypeInterval}, "UTC", anchor); !errors.Is(err, ErrInvalidSchedule) {
		t.Errorf("interval_seconds<=0 必须报 ErrInvalidSchedule，得到 %v", err)
	}
	if _, err := NextFireAfter(&TaskDefinition{ScheduleType: ScheduleTypeCron, CronExpr: "bogus"}, "UTC", anchor); !errors.Is(err, ErrInvalidSchedule) {
		t.Errorf("非法表达式必须报 ErrInvalidSchedule，得到 %v", err)
	}
	if _, err := NextFireAfter(&TaskDefinition{ScheduleType: 42}, "UTC", anchor); !errors.Is(err, ErrInvalidSchedule) {
		t.Errorf("未知调度类型必须报 ErrInvalidSchedule，得到 %v", err)
	}
	if _, err := NextFireAfter(nil, "UTC", anchor); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("nil 定义必须报 ErrTaskNotFound，得到 %v", err)
	}
}

func TestNextFireAfterRejectsUnknownTimezone(t *testing.T) {
	// 悄悄按 UTC 跑会让 03:00 的任务在错误时间叫醒下游，必须显式失败。
	d := &TaskDefinition{ScheduleType: ScheduleTypeCron, CronExpr: "0 3 * * *", Timezone: "Mars/Olympus"}
	if _, err := NextFireAfter(d, "UTC", time.Now().Unix()); !errors.Is(err, ErrInvalidSchedule) {
		t.Errorf("未知时区必须报 ErrInvalidSchedule，得到 %v", err)
	}
	if _, err := FirstFireAt(d, "UTC", time.Now().Unix()); !errors.Is(err, ErrInvalidSchedule) {
		t.Errorf("FirstFireAt 也要在注册期拦住坏时区，得到 %v", err)
	}
}

func TestLoadZoneFallback(t *testing.T) {
	loc, err := LoadZone("", "Asia/Shanghai")
	if err != nil {
		t.Fatalf("回落默认时区失败：%v", err)
	}
	if loc.String() != "Asia/Shanghai" {
		t.Errorf("应回落到默认时区，得到 %s", loc)
	}
	if loc, err = LoadZone("  ", "  "); err != nil || loc.String() != "UTC" {
		t.Errorf("两者都空白时应落到 UTC，得到 %s/%v", loc, err)
	}
	if _, err := LoadZone("Nowhere/Land", ""); !errors.Is(err, ErrInvalidSchedule) {
		t.Errorf("坏时区必须报 ErrInvalidSchedule，得到 %v", err)
	}
}

func TestCronSpecCacheIsConsistent(t *testing.T) {
	// 到期扫描每轮都会重算，缓存必须返回同一份解析结果，且坏表达式不被缓存住。
	first, err := cachedCronSpec("*/5 * * * *")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	second, err := cachedCronSpec("*/5 * * * *")
	if err != nil {
		t.Fatalf("第二次解析失败：%v", err)
	}
	if first != second {
		t.Error("同一表达式必须命中缓存（指针相同）")
	}
	for i := 0; i < 3; i++ {
		if _, err := cachedCronSpec("still bogus"); err == nil {
			t.Fatal("坏表达式必须每次都报错")
		}
	}
}
