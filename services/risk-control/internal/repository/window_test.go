package repository

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go-video/services/risk-control/internal/policy"
	"go-video/services/risk-control/model"
)

// 本文件覆盖 Redis 滑窗计数的纯计算部分与进程内兜底限流：
// 这些桶数学决定了「规则可观测/不可观测」的边界，出错会让规则误命中或永久不命中，
// 因此必须在没有 Redis 的情况下可验证。

func TestNormalizeTiers(t *testing.T) {
	if got := normalizeTiers(nil); len(got) != len(DefaultCounterTiers) {
		t.Fatalf("空档位应回落到默认: %v", got)
	}
	if got := normalizeTiers([]int64{0, -5, 3600, 60, 60, 600}); fmt.Sprint(got) != "[60 600 3600]" {
		t.Fatalf("应过滤非法值、去重并升序: %v", got)
	}
	if got := normalizeTiers([]int64{-1, 0}); len(got) != len(DefaultCounterTiers) {
		t.Fatalf("全非法时应回落默认档位: %v", got)
	}
}

func TestPickTierNeverDowngradesToApproximateWindow(t *testing.T) {
	tiers := []int64{60, 600, 3600}
	cases := []struct {
		want   int64
		tier   int64
		ok     bool
		reason string
	}{
		{1, 60, true, "小于最小档位时向上取齐"},
		{60, 60, true, "刚好等于档位"},
		{61, 600, true, "超出 1 秒必须跳到下一档，不能用 60 秒窗口冒充"},
		{3600, 3600, true, "最大档位可用"},
		{3601, 0, false, "超出最大档位视为不可观测"},
		{86400, 0, false, "一天窗口超出档位，规则应进 skipped_rule_ids"},
	}
	for _, c := range cases {
		t.Run(c.reason, func(t *testing.T) {
			tier, ok := pickTier(tiers, c.want)
			if ok != c.ok || tier != c.tier {
				t.Fatalf("pickTier(%d)=(%d,%v), want (%d,%v)", c.want, tier, ok, c.tier, c.ok)
			}
		})
	}
}

func TestBucketMath(t *testing.T) {
	if got := bucketSize(60, 6); got != 10 {
		t.Fatalf("bucketSize(60,6)=%d, want 10", got)
	}
	if got := bucketSize(60, 0); got != 60 {
		t.Fatalf("桶数非法时应退化为单桶: %d", got)
	}
	if got := bucketSize(5, 10); got != 1 {
		t.Fatalf("桶长至少 1 秒，实际 %d", got)
	}
	if got := bucketIndex(1000, 10); got != 100 {
		t.Fatalf("bucketIndex=%d, want 100", got)
	}
	if got := bucketIndex(1000, 0); got != 0 {
		t.Fatalf("桶长为 0 时应返回 0 而不是除零: %d", got)
	}
	// 窗口 [now-window, now] 覆盖的桶数：1000 处窗口 60、桶 10 -> 7 个桶（含两端）。
	if got := bucketSpan(1000, 60, 10); got != 7 {
		t.Fatalf("bucketSpan=%d, want 7", got)
	}
	if got := bucketSpan(1000, 0, 10); got != 1 {
		t.Fatalf("窗口为 0 时至少读 1 个桶，实际 %d", got)
	}
	if got := bucketSpan(1000, 60, -1); got != 1 {
		t.Fatalf("桶长非法时至少读 1 个桶，实际 %d", got)
	}
	if got := bucketSpan(0, 1<<40, 1); got != 4096 {
		t.Fatalf("应有读放大上限保护，实际 %d", got)
	}
}

func TestCounterKeysAreBucketAlignedAndOrdered(t *testing.T) {
	const now = int64(1_700_000_000)
	keys, tier, bucket, ok := counterKeys(model.MetricActionCount, subjectMid(42), model.ActionLogin, []int64{60, 600}, 6, 60, now)
	if !ok {
		t.Fatal("窗口 60 应可观测")
	}
	if tier != 60 || bucket != 10 {
		t.Fatalf("tier/bucket=%d/%d, want 60/10", tier, bucket)
	}
	if len(keys) != 7 {
		t.Fatalf("应读 7 个桶，实际 %d", len(keys))
	}
	if keys[0] != counterKey(model.MetricActionCount, "mid:42", model.ActionLogin, 60, bucketIndex(now-60, 10)) {
		t.Fatalf("首个 key 应对应窗口最老的桶: %s", keys[0])
	}
	if !strings.HasPrefix(keys[0], keyPrefixCounter+":") {
		t.Fatalf("计数器 key 必须带本服务前缀 %s，避免与其他服务共用业务 key: %s", keyPrefixCounter, keys[0])
	}
	if _, _, _, ok := counterKeys(model.MetricActionCount, "mid:42", model.ActionLogin, []int64{60}, 6, 3600, now); ok {
		t.Fatal("窗口超出档位上限必须返回不可观测，而不是退化成近似窗口")
	}
}

// 主体为空时 Counter.Sum 必须在触达 Redis 之前就返回不可观测，
// 否则会拿「空主体」去统计全网动作，规则必然误命中。
func TestCounterSumSkipsUnusableSubjectWithoutTouchingRedis(t *testing.T) {
	repo := New(nil, nil, Config{CounterTiers: []int64{60}, WindowBuckets: 6, DefaultWindowSeconds: 60})
	total, observable, err := repo.counter.Sum(context.Background(), model.MetricActionCount, "", model.ActionLogin, 60, 1_700_000_000)
	if observable || err != nil || total != 0 {
		t.Fatalf("空主体应返回不可观测且无错误，实际 total=%d ok=%v err=%v", total, observable, err)
	}
	if _, observable, _ := repo.counter.Sum(context.Background(), model.MetricActionCount, "mid:1", model.ActionLogin, 999999, 1_700_000_000); observable {
		t.Fatal("窗口 999999 超出档位时应标记不可观测，且不得访问 Redis")
	}
}

func TestKeyNamespacesAreIsolated(t *testing.T) {
	if eventKey(" evt-1 ") != keyPrefixEvent+":evt-1" {
		t.Fatalf("eventKey=%s", eventKey(" evt-1 "))
	}
	if eventKey("") != "" {
		t.Fatal("空 event_id 表示不去重，必须返回空 key")
	}
	if checkKey("req") != keyPrefixCheck+":req" || checkKey("  ") != "" {
		t.Fatal("checkKey 前缀/空值语义错误")
	}
	if rulesKey(model.ActionAll) != "rc:rl:0" {
		t.Fatalf("rulesKey=%s", rulesKey(model.ActionAll))
	}
	// 三类 key 不得互相覆盖。
	prefixes := map[string]bool{keyPrefixCheck: true, keyPrefixRules: true, keyPrefixEvent: true, keyPrefixCounter: true}
	if len(prefixes) != 4 {
		t.Fatal("Redis key 前缀重复会导致数据互相覆盖")
	}
}

func TestParseCountsToleratesDirtyValues(t *testing.T) {
	if got := parseCounts([]string{"3", " 4 ", "", "x", "-1", "0", "9223372036854775807x"}); got != 7 {
		t.Fatalf("parseCounts=%d, want 7（脏值按 0 处理而不是中断整条链路）", got)
	}
	if parseCounts(nil) != 0 {
		t.Fatal("空结果应为 0")
	}
}

func TestDedupTTLAndClampInt(t *testing.T) {
	if got := dedupTTLSeconds(60); got != 120 {
		t.Fatalf("dedupTTLSeconds(60)=%d, want 120", got)
	}
	if got := dedupTTLSeconds(5); got != 120 {
		t.Fatalf("下限应为 120 秒，实际 %d", got)
	}
	if got := dedupTTLSeconds(86400); got != 7200 {
		t.Fatalf("上限应为 2 小时，实际 %d", got)
	}
	if clampInt(-1) != 0 || clampInt(5) != 5 || clampInt(1<<40) <= 0 {
		t.Fatal("clampInt 必须把越界配置收敛为合法区间，避免 panic")
	}
}

func TestSubjectKeyRejectsUnusableSubjects(t *testing.T) {
	cases := []struct {
		name string
		s    Subject
		want string
	}{
		{"mid", Subject{Kind: "mid", Value: "42"}, "mid:42"},
		{"mid_zero", Subject{Kind: "mid", Value: "0"}, ""},
		{"mid_empty", Subject{Kind: "mid", Value: ""}, ""},
		{"mid_not_number", Subject{Kind: "mid", Value: "abc"}, ""},
		{"mid_negative", Subject{Kind: "mid", Value: "-3"}, ""},
		{"device", Subject{Kind: "device", Value: "abcd1234"}, "dev:abcd1234"},
		{"device_empty", Subject{Kind: "device", Value: ""}, ""},
		{"ip", Subject{Kind: "ip", Value: "abcd1234"}, "ip:abcd1234"},
		{"unknown_kind", Subject{Kind: "phone", Value: "13800000000"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.SubjectKey(); got != c.want {
				t.Fatalf("SubjectKey=%q, want %q", got, c.want)
			}
		})
	}
	if got := metricForSubject("device"); got != model.MetricDeviceActionCount {
		t.Fatalf("metricForSubject=%q", got)
	}
	if got := metricForSubject("ip"); got != model.MetricIpActionCount {
		t.Fatalf("metricForSubject=%q", got)
	}
	if got := metricForSubject("mid"); got != model.MetricActionCount {
		t.Fatalf("metricForSubject=%q", got)
	}
}

func TestSubjectKeyForMetricRequiresTheRightDimension(t *testing.T) {
	in := policy.Input{Mid: 42, DeviceHash: "deadbeef", IPHash: "cafebabe"}
	if got := subjectKeyForMetric(model.MetricActionCount, in); got != "mid:42" {
		t.Fatalf("action_count 主体=%q", got)
	}
	if got := subjectKeyForMetric(model.MetricDeviceActionCount, in); got != "dev:deadbeef" {
		t.Fatalf("device_action_count 主体=%q", got)
	}
	if got := subjectKeyForMetric(model.MetricIpActionCount, in); got != "ip:cafebabe" {
		t.Fatalf("ip_action_count 主体=%q", got)
	}
	// 未登录/未上报设备或 ip_hash 时必须返回空串 -> 规则不可观测，而不是拿空主体统计全网。
	if got := subjectKeyForMetric(model.MetricActionCount, policy.Input{Mid: 0, DeviceHash: "d", IPHash: "i"}); got != "" {
		t.Fatalf("mid=0 时不应有主体标识，实际 %q", got)
	}
	if got := subjectKeyForMetric(model.MetricIpActionCount, policy.Input{Mid: 1}); got != "" {
		t.Fatalf("缺少 ip_hash 时不应有主体标识，实际 %q", got)
	}
	if got := subjectKeyForMetric("not_a_metric", in); got != "" {
		t.Fatalf("未知指标应返回空，实际 %q", got)
	}
}

// 进程内兜底限流：Redis 全盲时唯一的防线，key 维度只有动作枚举，内存上界可控。
func TestLocalRateGuard(t *testing.T) {
	if g := newLocalRateGuard(0, 6, 10); g.Enabled() {
		t.Fatal("窗口为 0 时应禁用兜底限流")
	}
	if g := newLocalRateGuard(60, 6, 0); g.Enabled() {
		t.Fatal("阈值 <=0 表示关闭")
	}
	if g := newLocalRateGuard(60, 0, 10); !g.Enabled() || g.buckets != 6 {
		t.Fatal("桶数非法应回落到 6 而不是禁用")
	}
	var nilGuard *localRateGuard
	if nilGuard.Enabled() || nilGuard.limited(model.ActionComment) || nilGuard.current(model.ActionComment) != 0 {
		t.Fatal("nil 兜底限流器应为 no-op，不得 panic")
	}
	nilGuard.observe(model.ActionComment)

	g := newLocalRateGuard(3600, 6, 3)
	for i := 0; i < 3; i++ {
		g.observe(model.ActionLogin)
	}
	if g.limited(model.ActionLogin) {
		t.Fatal("未超过阈值时不应触发兜底限流")
	}
	if got := g.current(model.ActionLogin); got != 3 {
		t.Fatalf("current=%d, want 3", got)
	}
	g.observe(model.ActionLogin)
	if !g.limited(model.ActionLogin) {
		t.Fatal("超过阈值后应触发兜底限流")
	}
	// 动作维度隔离：登录被打满不应牵连弹幕（低危动作被误伤会放大故障面）。
	if g.limited(model.ActionDanmaku) {
		t.Fatal("兜底限流必须按动作维度隔离")
	}
	if localKey(model.ActionLogin) != "action:5" {
		t.Fatalf("localKey=%s", localKey(model.ActionLogin))
	}
}

// Repository.New 的配置归一化：错误配置不得让计数器或兜底限流进入未定义状态。
func TestRepositoryConfigNormalization(t *testing.T) {
	repo := New(nil, nil, Config{CounterTiers: []int64{-1}, WindowBuckets: 0, DefaultWindowSeconds: 0, DecisionCacheSeconds: 0, RuleCacheSeconds: 0, LocalFallbackLimit: 0})
	if len(repo.counter.tiers) != len(DefaultCounterTiers) || repo.counter.tiers[0] != 60 {
		t.Fatalf("非法档位应回落默认: %v", repo.counter.tiers)
	}
	if repo.cfg.DefaultWindowSeconds != 60 || repo.cfg.WindowBuckets != 6 || repo.cfg.DecisionCacheSeconds != 60 || repo.cfg.RuleCacheSeconds != 30 {
		t.Fatalf("零值配置未归一化: %+v", repo.cfg)
	}
	if repo.LocalFallbackEnabled() {
		t.Fatal("LocalFallbackLimit=0 应关闭进程内兜底限流")
	}
	if repo.counter.defaultWindow != 60 {
		t.Fatalf("counter.defaultWindow=%d", repo.counter.defaultWindow)
	}
}
