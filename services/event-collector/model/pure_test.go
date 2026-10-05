package model

import (
	"strings"
	"testing"
)

// 本文件只覆盖不连库的纯函数：隐私哈希、IP 段脱敏、采样判定、schema 兼容、
// 退避序列与状态枚举互斥。任何需要 MySQL 的行为都不在这里断言（迁移未在实例执行）。

func TestSaltedHashStableAndNonLeaking(t *testing.T) {
	const device = "00000000-1111-2222-3333-444444444444"
	s1, err := SaltedHash("salt-A", 1, device)
	if err != nil {
		t.Fatalf("SaltedHash 失败: %v", err)
	}
	s2, err := SaltedHash("salt-A", 1, device)
	if err != nil {
		t.Fatalf("SaltedHash 失败: %v", err)
	}
	if s1 != s2 {
		t.Fatalf("同盐同版本必须稳定： %q != %q", s1, s2)
	}
	if !strings.HasPrefix(s1, hashPrefix) {
		t.Fatalf("缺少算法前缀: %q", s1)
	}
	// 不泄漏原值：摘要里不得出现设备号或其分段片段。
	for _, frag := range []string{"00000000", "1111", device} {
		if strings.Contains(s1, frag) {
			t.Fatalf("哈希结果泄漏原值片段 %q: %q", frag, s1)
		}
	}
	// 盐或版本不同 → 结果不同（轮换后旧数据不可逆推）。
	otherSalt, _ := SaltedHash("salt-B", 1, device)
	if otherSalt == s1 {
		t.Fatal("换盐后摘要必须变化")
	}
	otherVer, _ := SaltedHash("salt-A", 2, device)
	if otherVer == s1 {
		t.Fatal("换盐版本后摘要必须变化")
	}
	// 空值与缺盐：前者返回空串（不哈希空字符串占位），后者必须报错而不是无盐退化。
	if got, err := SaltedHash("salt-A", 1, "  "); err != nil || got != "" {
		t.Fatalf("空白值应返回空串: got=%q err=%v", got, err)
	}
	if _, err := SaltedHash("", 1, device); err != ErrSaltMissing {
		t.Fatalf("缺盐必须 ErrSaltMissing，实得 %v", err)
	}
	if _, err := SaltedHash("salt-A", 0, device); err != ErrSaltMissing {
		t.Fatalf("缺盐版本必须 ErrSaltMissing，实得 %v", err)
	}
	if !SaltedHashEqual(s1, s2) || SaltedHashEqual(s1, otherSalt) {
		t.Fatal("SaltedHashEqual 判定错误")
	}
}

func TestIPSegmentMasksHostBits(t *testing.T) {
	if got := IPSegment("203.0.113.77", 24); got != "203.0.113.0/24" {
		t.Fatalf("IPv4 /24 脱敏错误: %q", got)
	}
	if got := IPSegment("203.0.113.77", 0); got != "203.0.113.0/24" {
		t.Fatalf("默认前缀应为 /24: %q", got)
	}
	if got := IPSegment("203.0.113.77", 99); got != "203.0.113.0/24" {
		t.Fatalf("越界前缀必须回落到 /24（/32 等于存明文 IP）: %q", got)
	}
	if got := IPSegment("203.0.113.77", 32); got != "203.0.113.0/24" {
		t.Fatalf("/32 必须回落到 /24: %q", got)
	}
	// 结果不得包含主机位原值（77），也不得回显非法输入。
	if strings.Contains(IPSegment("203.0.113.77", 24), "77") {
		t.Fatal("IP 段泄漏主机位")
	}
	for _, bad := range []string{"", "   ", "not-an-ip", "203.0.113.999"} {
		if got := IPSegment(bad, 24); got != "" {
			t.Fatalf("非法 IP %q 必须返回空串，实得 %q", bad, got)
		}
	}
	if got := IPSegment("2001:db8:1234:5678::1", 0); got != "2001:db8:1234:5678::/64" {
		t.Fatalf("IPv6 默认应取 /64: %q", got)
	}
}

func TestPayloadAndKeywordDigestDoNotRevealContent(t *testing.T) {
	digest, size := PayloadDigest(`{"k":"v"}`)
	if size != 9 {
		t.Fatalf("payload_bytes 应为 9，实得 %d", size)
	}
	if !strings.HasPrefix(digest, DigestPrefix) || strings.Contains(digest, `"v"`) {
		t.Fatalf("摘要格式或泄漏异常: %q", digest)
	}
	if got, n := PayloadDigest(""); got != "" || n != 0 {
		t.Fatalf("空正文应返回空摘要: %q/%d", got, n)
	}

	kd, runes, truncated := KeywordDigest("一二三四五六七八九十", 5)
	if runes != 5 || !truncated {
		t.Fatalf("截断结果错误: runes=%d truncated=%v", runes, truncated)
	}
	if strings.Contains(kd, "一二三") {
		t.Fatal("搜索词摘要泄漏原文")
	}
	// 截断后的搜索词与未截断的短词必须得到同一摘要（幂等口径）。
	kd2, runes2, tr2 := KeywordDigest("一二三四五", 5)
	if kd2 != kd || runes2 != runes || tr2 {
		t.Fatalf("同词摘要不一致: %q/%d/%v vs %q/%d/%v", kd, runes, truncated, kd2, runes2, tr2)
	}
	if got, n, tr := KeywordDigest("", 5); got != "" || n != 0 || tr {
		t.Fatalf("空搜索词应为空: %q/%d/%v", got, n, tr)
	}
}

func TestSampleHitBoundaries(t *testing.T) {
	const eventID = "01HZY0K4S7N8PQQRSTUVWXYZ123"
	if !SampleHit(eventID, SampleBase) {
		t.Fatal("bps=10000 必须全量保留")
	}
	if SampleHit(eventID, 0) || SampleHit("", 5000) {
		t.Fatal("bps=0 或空 event_id 必须不命中")
	}
	if SampleHit(eventID, -1) {
		t.Fatal("负 bps 必须不命中")
	}
	// 确定性：同一 event_id 重复判定结论一致（幂等回放不自相矛盾）。
	first := SampleHit(eventID, 3000)
	for i := 0; i < 64; i++ {
		if SampleHit(eventID, 3000) != first {
			t.Fatal("采样结论不确定，重放会产生矛盾台账")
		}
	}
	// 边界两侧：bps=1 与 bps=9999 都在 (0,10000) 内参与判定，不得恒真/恒假。
	lo, hi := 0, 0
	for i := 0; i < 2000; i++ {
		id := "evt-" + string(rune('a'+i%26)) + itoa(i)
		if SampleHit(id, 1) {
			lo++
		}
		if SampleHit(id, 9999) {
			hi++
		}
	}
	if hi <= lo {
		t.Fatalf("采样率单调性异常：bps=1 命中 %d，bps=9999 命中 %d", lo, hi)
	}
	if hi < 1900 {
		t.Fatalf("bps=9999 应几乎全量命中，实得 %d/2000", hi)
	}
}

func TestEffectiveSampleBpsRuleSelection(t *testing.T) {
	rules := []SampleRule{
		{EventType: "behavior.play", SampleBps: 5000},
		{EventType: "behavior.quality", SampleBps: 10000, QualityEvents: true},
		{EventType: SampleWildcard, SampleBps: 2000},
	}
	if got := EffectiveSampleBps(rules, "behavior.play", false); got != 5000 {
		t.Fatalf("精确匹配失败: %d", got)
	}
	if got := EffectiveSampleBps(rules, "behavior.quality", true); got != 10000 {
		t.Fatalf("质量事件规则失败: %d", got)
	}
	// quality_events=true 的规则不得作用于非质量事件。
	if got := EffectiveSampleBps(rules, "behavior.quality", false); got != 2000 {
		t.Fatalf("质量规则越界生效: %d", got)
	}
	if got := EffectiveSampleBps(rules, "behavior.click", false); got != 2000 {
		t.Fatalf("通配兜底失败: %d", got)
	}
	if got := EffectiveSampleBps(nil, "behavior.click", false); got != -1 {
		t.Fatalf("无规则必须返回 -1（调用方按全量保守处理），实得 %d", got)
	}
	// 越界 bps 收敛进 [0,10000]。
	if got := EffectiveSampleBps([]SampleRule{{EventType: SampleWildcard, SampleBps: 99999}}, "x", false); got != SampleBase {
		t.Fatalf("bps 未封顶: %d", got)
	}
}

func TestValidateSampleRules(t *testing.T) {
	if !ValidateSampleRules(nil) {
		t.Fatal("空规则合法（表示全量兜底）")
	}
	cases := []struct {
		name  string
		rules []SampleRule
		ok    bool
	}{
		{"空 event_type", []SampleRule{{EventType: " ", SampleBps: 100}}, false},
		{"bps 越界", []SampleRule{{EventType: "a", SampleBps: -1}}, false},
		{"两条通配", []SampleRule{{EventType: SampleWildcard}, {EventType: SampleWildcard}}, false},
		{"通配带 quality", []SampleRule{{EventType: SampleWildcard, QualityEvents: true}}, false},
		{"正常", []SampleRule{{EventType: SampleWildcard, SampleBps: 100}, {EventType: "a", SampleBps: 0}}, true},
	}
	for _, tc := range cases {
		if got := ValidateSampleRules(tc.rules); got != tc.ok {
			t.Errorf("%s: 期望 %v 实得 %v", tc.name, tc.ok, got)
		}
	}
}

func TestSchemaSupported(t *testing.T) {
	if SchemaSupported(0, 3) || SchemaSupported(-1, 3) {
		t.Fatal("schema_version < 1 必须拒绝")
	}
	if SchemaSupported(4, 3) {
		t.Fatal("高于服务端支持版本必须拒绝（不做猜测性兼容）")
	}
	if !SchemaSupported(1, 3) || !SchemaSupported(3, 3) {
		t.Fatal("区间内版本必须接受")
	}
}

func TestNormalizeEventType(t *testing.T) {
	if got, ok := NormalizeEventType(" behavior.custom ", ""); got != "behavior.custom" || !ok {
		t.Fatalf("显式 event_type 应被 trim 后采用: %q %v", got, ok)
	}
	if got, ok := NormalizeEventType("", "play"); got != "behavior.play" || !ok {
		t.Fatalf("由 category 推导失败: %q %v", got, ok)
	}
	if _, ok := NormalizeEventType("", ""); ok {
		t.Fatal("既无 event_type 也无 category 必须判定缺失（REJECT_MISSING_EVENT_TYPE）")
	}
}

func TestNextRetryAtMonotonicAndCapped(t *testing.T) {
	const now, base, maxS = int64(1_000_000), int64(5), int64(600)
	prev := now
	for attempts := int32(0); attempts <= 20; attempts++ {
		next := NextRetryAt(now, attempts, base, maxS)
		if next < now {
			t.Fatalf("退避时间不能落在过去：attempts=%d next=%d", attempts, next)
		}
		if next < prev {
			t.Fatalf("退避序列必须单调不减：attempts=%d %d -> %d", attempts, prev, next)
		}
		if next-now > maxS {
			t.Fatalf("退避未封顶：attempts=%d delay=%d", attempts, next-now)
		}
		prev = next
	}
	if got := NextRetryAt(now, 1, base, maxS); got != now+base {
		t.Fatalf("第 1 次失败应为 now+base，实得 %d", got)
	}
	if got := NextRetryAt(now, 3, base, maxS); got != now+base*4 {
		t.Fatalf("第 3 次失败应为 now+base*4，实得 %d", got)
	}
	// 参数退化时必须仍可推进（至少 now），否则扫描任务会空转。
	if got := NextRetryAt(now, 5, 0, 0); got < now+1 {
		t.Fatalf("base<=0 必须收敛到最小退避: %d", got)
	}
	if got := NextRetryAt(now, 2, 100, 10); got != now+100 {
		t.Fatalf("max<base 时应取 base: %d", got)
	}
}

func TestShouldDeadLetter(t *testing.T) {
	if !ShouldDeadLetter(8, 8) || !ShouldDeadLetter(9, 8) {
		t.Fatal("达到上限必须转死信")
	}
	if ShouldDeadLetter(7, 8) {
		t.Fatal("未达上限不得转死信")
	}
	if !ShouldDeadLetter(1, 0) {
		t.Fatal("maxAttempts<=0 视为不重试，失败即死信（比无限重试安全）")
	}
}

func TestStateEnumsAreExclusive(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   func(int32) bool
		in   []int32
		out  []int32
	}{
		{"delivery", ValidDeliveryState, []int32{1, 2, 3, 4, 5}, []int32{0, 6, -1}},
		{"batch", ValidBatchState, []int32{1, 2, 3, 4, 5, 6}, []int32{0, 7, -1}},
		{"decision", ValidDecision, []int32{1, 2, 3, 4, 5}, []int32{0, 6, -1}},
		{"policy", ValidPolicyState, []int32{1, 2, 3}, []int32{0, 4, -1}},
		{"source", ValidSource, []int32{1, 2}, []int32{0, 3, -1}},
		{"reason", ValidReason, []int32{1, 18, 24}, []int32{0, 25, -1}},
	} {
		for _, v := range tc.in {
			if !tc.ok(v) {
				t.Errorf("%s: %d 应为合法取值", tc.name, v)
			}
		}
		for _, v := range tc.out {
			if tc.ok(v) {
				t.Errorf("%s: %d 应落在枚举之外", tc.name, v)
			}
		}
	}
	// 只有非开放态才是终态。
	if !DeliveryStateOpen(DeliveryStatePending) || !DeliveryStateOpen(DeliveryStateRetrying) {
		t.Fatal("PENDING/RETRYING 必须可被 ClaimDue 捞起")
	}
	for _, s := range []int32{DeliveryStateNone, DeliveryStateSent, DeliveryStateDead} {
		if DeliveryStateOpen(s) {
			t.Fatalf("状态 %d 已是终态，不得再被投递捞起（否则重复发送）", s)
		}
	}
	for _, s := range []int32{BatchStateDispatched, BatchStateRejected} {
		if !BatchStateTerminal(s) {
			t.Fatalf("批次状态 %d 应为终态", s)
		}
	}
	// DEFERRED 整批未落库，因此不写事件台账；其余结论都要留痕。
	if DecisionStored(DecisionDeferred) {
		t.Fatal("DEFERRED 不得落 ec_event_record")
	}
	for _, d := range []int32{DecisionAccepted, DecisionDuplicated, DecisionRejected, DecisionSampledOut} {
		if !DecisionStored(d) {
			t.Fatalf("结论 %d 必须留痕，否则无法解释「客户端说报了、库里没有」", d)
		}
	}
}

func TestPolicyStateTransition(t *testing.T) {
	if !CanTransitPolicyState(PolicyStateDraft, PolicyStateActive) ||
		!CanTransitPolicyState(PolicyStateActive, PolicyStateArchived) ||
		!CanTransitPolicyState(PolicyStateDraft, PolicyStateArchived) {
		t.Fatal("合法迁移被拒绝")
	}
	// 互斥/单向：ARCHIVED 不可复活，ACTIVE 不可退回 DRAFT，同态不算迁移。
	for _, tc := range [][2]int32{
		{PolicyStateArchived, PolicyStateActive},
		{PolicyStateArchived, PolicyStateDraft},
		{PolicyStateActive, PolicyStateDraft},
		{PolicyStateDraft, PolicyStateDraft},
		{PolicyStateActive, PolicyStateActive},
		{PolicyStateActive, PolicyStateDraft},
		{0, PolicyStateActive},
		{PolicyStateDraft, 9},
	} {
		if CanTransitPolicyState(tc[0], tc[1]) {
			t.Errorf("非法迁移被接受: %d -> %d", tc[0], tc[1])
		}
	}
}

func TestActiveFlagOnlyForActivePolicy(t *testing.T) {
	if v := ActiveFlag(PolicyStateActive); v == nil || v.(int) != 1 {
		t.Fatal("ACTIVE 必须写 active_flag=1 占用唯一键")
	}
	for _, s := range []int32{PolicyStateDraft, PolicyStateArchived, 0, 9} {
		if ActiveFlag(s) != nil {
			t.Fatalf("非 ACTIVE(%d) 必须写 NULL，否则 uniq_active 会互相冲突", s)
		}
	}
}

func TestDeadLetterStateStrings(t *testing.T) {
	for _, s := range []string{DeadLetterOpen, DeadLetterReplayed, DeadLetterDiscarded} {
		if !ValidDeadLetterState(s) {
			t.Errorf("%s 应为合法死信状态", s)
		}
	}
	for _, s := range []string{"", "OPEN", "open ", "replay", "unknown"} {
		if ValidDeadLetterState(s) {
			t.Errorf("死信状态 %q 不在白名单，必须拒绝入库", s)
		}
	}
	if !DeadLetterTerminal(DeadLetterReplayed) || DeadLetterTerminal(DeadLetterOpen) {
		t.Fatal("open 不是终态：重放必须只对 open 生效")
	}
}

func TestCursorAndPageSize(t *testing.T) {
	if got := EncodeCursor(0, 0); got != "" {
		t.Fatalf("零值游标应为空串: %q", got)
	}
	enc := EncodeCursor(1700000000, 42)
	cur, err := ParseCursor(enc)
	if err != nil || cur.Ctime != 1700000000 || cur.ID != 42 {
		t.Fatalf("游标往返失败: %q %+v %v", enc, cur, err)
	}
	if cur, err := ParseCursor(""); err != nil || cur.Ctime != 0 || cur.ID != 0 {
		t.Fatalf("空游标应为第一页: %+v %v", cur, err)
	}
	for _, bad := range []string{"abc", "1", "1_", "_1", "-1_2", "2_3_4", "x_y"} {
		if _, err := ParseCursor(bad); err != ErrInvalidCursor {
			t.Errorf("非法游标 %q 应返回 ErrInvalidCursor，实得 %v", bad, err)
		}
	}
	// ps 收敛：0 取默认，越界返回 0（显式报错），配置自相矛盾返回 0。
	if got := ClampPageSize(0, 50, 500); got != 50 {
		t.Fatalf("默认值错误: %d", got)
	}
	if got := ClampPageSize(10, 50, 500); got != 10 {
		t.Fatalf("合法 ps 应原样返回: %d", got)
	}
	for _, ps := range []int32{-1, 501} {
		if got := ClampPageSize(ps, 50, 500); got != 0 {
			t.Errorf("ps=%d 必须返回 0 让 logic 报错，实得 %d", ps, got)
		}
	}
	if got := ClampPageSize(10, 50, 20); got != 0 {
		t.Fatalf("max<def 属配置错误，应返回 0: %d", got)
	}
}

func TestLeaseKeyNeverEmpty(t *testing.T) {
	if got := LeaseKey("", 7); !strings.HasPrefix(got, "unknown:7") {
		t.Fatalf("空 worker 必须留可辨识标记: %q", got)
	}
	if got := LeaseKey("pod-a", 7); got != "pod-a:7" {
		t.Fatalf("LeaseKey 错误: %q", got)
	}
}

func TestIsNotFoundAndIsDuplicate(t *testing.T) {
	for _, err := range []error{ErrBatchNotFound, ErrRecordNotFound, ErrPolicyNotFound, ErrNoActivePolicy,
		ErrPendingNotFound, ErrDeadLetterNotFound} {
		if !IsNotFound(err) {
			t.Errorf("%v 应被 IsNotFound 识别", err)
		}
	}
	if IsNotFound(ErrEventIDRequired) {
		t.Error("入参错误不得被当成「不存在」")
	}
	if !IsDuplicate(errDuplicateWrap) || IsDuplicate(ErrEventIDRequired) || IsDuplicate(nil) {
		t.Fatal("IsDuplicate 判定错误")
	}
}

func TestCheckPolicyRejectsUnusablePolicy(t *testing.T) {
	good := &DispatchPolicy{
		Version: "2026.09.20-1", State: PolicyStateDraft, SampleRulesJSON: "[]",
		SaltVersion: 1, SaltRef: "EVENT_COLLECTOR_SALT_V1",
		MaxEventsPerBatch: 200, MaxRequestBytes: 1 << 20, MaxEventPayloadBytes: 8192,
		MaxClockSkewSeconds: 300, MaxBackfillSeconds: 86400, KeywordMaxRunes: 64,
		RetentionDays: 30, DeliverMaxAttempts: 8, RetryBaseSeconds: 5, RetryMaxSeconds: 3600,
	}
	if err := checkPolicy(good); err != nil {
		t.Fatalf("基准策略应通过: %v", err)
	}
	bad := *good
	bad.SaltRef = ""
	if err := checkPolicy(&bad); err != ErrSaltMissing {
		t.Fatalf("缺 salt_ref 必须 ErrSaltMissing（禁止把盐值写进库），实得 %v", err)
	}
	bad = *good
	bad.SampleRulesJSON = "{not-json"
	if err := checkPolicy(&bad); err == nil {
		t.Fatal("无法解析的采样规则必须拒绝入库")
	}
	bad = *good
	bad.SampleRulesJSON = `[{"event_type":"","sample_bps":100}]`
	if err := checkPolicy(&bad); err == nil {
		t.Fatal("空 event_type 的规则必须拒绝")
	}
	bad = *good
	bad.RetryMaxSeconds = 1
	if err := checkPolicy(&bad); err == nil {
		t.Fatal("retry_max < retry_base 必须拒绝")
	}
	bad = *good
	bad.Version = ""
	if err := checkPolicy(&bad); err == nil {
		t.Fatal("无版本号的策略无法归因，必须拒绝")
	}
}

func TestPlaceholdersAndTruncate(t *testing.T) {
	if got := placeholders(1); got != "?" {
		t.Fatalf("placeholders(1)=%q", got)
	}
	if got := placeholders(3); got != "?,?,?" {
		t.Fatalf("placeholders(3)=%q", got)
	}
	if got := truncate("abcdef", 3); got != "abc" {
		t.Fatalf("truncate 错误: %q", got)
	}
	if got := truncate("abc", 0); got != "abc" {
		t.Fatalf("n<=0 应原样返回: %q", got)
	}
}

// errDuplicateWrap 模拟驱动返回的 1062 错误包装。
var errDuplicateWrap = &wrappedErr{msg: "insert failed: Error 1062 (23000): Duplicate entry 'x' for key 'uniq_event_id'"}

type wrappedErr struct{ msg string }

func (e *wrappedErr) Error() string { return e.msg }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
