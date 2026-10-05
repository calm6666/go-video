package model

import (
	"strings"
	"testing"
)

// 本文件覆盖不依赖 Redis/MySQL 的纯逻辑：阈值评估、裁决强度、
// 处罚/名单的时间边界、标签合并、命中序列化。全部用固定时间戳，不读系统时钟。

func TestEvaluateThresholdComparison(t *testing.T) {
	cases := []struct {
		name      string
		op        int32
		observed  int64
		threshold int64
		want      bool
	}{
		{"gt_hit", OpGT, 11, 10, true},
		{"gt_boundary_equal_not_hit", OpGT, 10, 10, false},
		{"gte_boundary_equal_hit", OpGTE, 10, 10, true},
		{"lt_hit", OpLT, 9, 10, true},
		{"lt_boundary_equal_not_hit", OpLT, 10, 10, false},
		{"lte_boundary_equal_hit", OpLTE, 10, 10, true},
		{"eq_hit", OpEQ, 7, 7, true},
		{"eq_miss", OpEQ, 8, 7, false},
		{"zero_threshold_gt", OpGT, 1, 0, true},
		// 非法比较符必须不命中：宁可不判也不误判（见 Evaluate 注释）。
		{"unknown_op_zero_never_hits", 0, 999, 1, false},
		{"unknown_op_out_of_range_never_hits", 9, 999, 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Evaluate(c.op, c.observed, c.threshold); got != c.want {
				t.Fatalf("Evaluate(op=%d, %d, %d)=%v, want %v", c.op, c.observed, c.threshold, got, c.want)
			}
		})
	}
}

func TestValidHelpers(t *testing.T) {
	if ValidOp(0) || !ValidOp(OpGT) || !ValidOp(OpEQ) || ValidOp(6) {
		t.Fatal("ValidOp 边界与枚举不一致")
	}
	if !ValidAction(ActionSubmitVideo) || !ValidAction(ActionLiveStart) || ValidAction(ActionAll) || ValidAction(8) {
		t.Fatal("ValidAction 边界错误（0 只允许出现在规则里）")
	}
	if !ValidRuleAction(ActionAll) || ValidRuleAction(8) {
		t.Fatal("ValidRuleAction 应允许 0 表示全动作")
	}
	if !ValidPunishmentState(PunishmentStateActive) || !ValidPunishmentState(PunishmentStateExpired) ||
		ValidPunishmentState(0) || ValidPunishmentState(4) {
		t.Fatal("ValidPunishmentState 边界错误")
	}
	if !ValidListType(ListTypeBlack) || !ValidListType(ListTypeWhite) || ValidListType(3) {
		t.Fatal("ValidListType 边界错误")
	}
	if !ValidTargetType(TargetTypeMid) || !ValidTargetType(TargetTypeIpHash) || ValidTargetType(0) || ValidTargetType(4) {
		t.Fatal("ValidTargetType 边界错误")
	}
	for name := range SupportedMetrics() {
		if !ValidMetric(name) {
			t.Fatalf("SupportedMetrics 里的 %q 被判为非法", name)
		}
	}
	if ValidMetric("ad_revenue") {
		t.Fatal("未注册的指标必须非法（会被当作不可观测跳过）")
	}
}

func TestSeverityOrdersBlockHighest(t *testing.T) {
	order := []int32{DecisionAllow, DecisionChallenge, DecisionReview, DecisionBlock}
	for i := 1; i < len(order); i++ {
		if Severity(order[i-1]) >= Severity(order[i]) {
			t.Fatalf("裁决强度应为 ALLOW<CHALLENGE<REVIEW<BLOCK，实际 %d 不低于 %d", Severity(order[i-1]), Severity(order[i]))
		}
	}
	if Severity(99) != Severity(DecisionAllow) {
		t.Fatal("未知裁决应按最弱强度处理，不得冒充 BLOCK")
	}
}

func TestPunishmentEffectiveTimeBoundaries(t *testing.T) {
	const now = int64(1000)
	cases := []struct {
		name string
		p    RiskPunishment
		want bool
	}{
		{"future_start_not_effective", RiskPunishment{State: PunishmentStateActive, StartAt: now + 1, EndAt: now + 100}, false},
		{"start_equal_now_effective", RiskPunishment{State: PunishmentStateActive, StartAt: now, EndAt: now + 100}, true},
		{"end_equal_now_expired", RiskPunishment{State: PunishmentStateActive, StartAt: now - 100, EndAt: now}, false},
		{"end_one_second_left_effective", RiskPunishment{State: PunishmentStateActive, StartAt: now - 100, EndAt: now + 1}, true},
		{"permanent_effective", RiskPunishment{State: PunishmentStateActive, StartAt: now - 100, EndAt: 0}, true},
		{"lifted_not_effective", RiskPunishment{State: PunishmentStateLifted, StartAt: now - 100, EndAt: 0}, false},
		{"expired_state_not_effective", RiskPunishment{State: PunishmentStateExpired, StartAt: now - 100, EndAt: now + 100}, false},
		{"zero_state_not_effective", RiskPunishment{State: StateDisabled, StartAt: now - 100, EndAt: 0}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := c.p
			if got := p.Effective(now); got != c.want {
				t.Fatalf("Effective(%d)=%v, want %v", now, got, c.want)
			}
		})
	}

	var nilP *RiskPunishment
	if nilP.Effective(now) {
		t.Fatal("nil 处罚必须不生效")
	}
}

func TestPunishmentRemainingAndScope(t *testing.T) {
	const now = int64(1000)
	p := &RiskPunishment{State: PunishmentStateActive, StartAt: now - 10, EndAt: now + 60, Scope: ActionComment}
	if got := p.RemainingSeconds(now); got != 60 {
		t.Fatalf("RemainingSeconds=%d, want 60", got)
	}
	permanent := &RiskPunishment{State: PunishmentStateActive, EndAt: 0}
	if got := permanent.RemainingSeconds(now); got != 0 {
		t.Fatalf("永久处罚剩余应为 0，实际 %d", got)
	}
	finished := &RiskPunishment{State: PunishmentStateLifted, EndAt: now + 60}
	if got := finished.RemainingSeconds(now); got != 0 {
		t.Fatalf("已解除处罚剩余应为 0，实际 %d", got)
	}

	if p.Covers(ActionComment) != true || p.Covers(ActionDanmaku) != false {
		t.Fatal("动作级处罚只应覆盖自己的动作")
	}
	global := &RiskPunishment{Mid: 42, Scope: ActionAll}
	if !global.Covers(ActionLiveStart) {
		t.Fatal("全域处罚应覆盖所有动作")
	}
	var nilP *RiskPunishment
	if nilP.Covers(ActionComment) {
		t.Fatal("nil 处罚不得覆盖任何动作")
	}
	// 解除必须按 scope 精确匹配：全域处罚不能被动作级解除顺带消掉。
	if global.MatchesScope(42, ActionComment) {
		t.Fatal("MatchesScope 不得把全域处罚算作动作级处罚")
	}
	if !global.MatchesScope(42, ActionAll) || global.MatchesScope(43, ActionAll) {
		t.Fatal("MatchesScope 应按 (mid, scope) 精确匹配")
	}
	if nilP.MatchesScope(42, ActionAll) {
		t.Fatal("nil 处罚不得匹配")
	}
}

func TestListEntryActiveBoundaries(t *testing.T) {
	const now = int64(1000)
	cases := []struct {
		name string
		l    RiskList
		want bool
	}{
		{"permanent", RiskList{State: StateEnabled, ExpireAt: 0}, true},
		{"one_second_left", RiskList{State: StateEnabled, ExpireAt: now + 1}, true},
		{"expire_equal_now_inactive", RiskList{State: StateEnabled, ExpireAt: now}, false},
		{"already_expired", RiskList{State: StateEnabled, ExpireAt: now - 1}, false},
		{"disabled", RiskList{State: StateDisabled, ExpireAt: 0}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := c.l
			if got := l.Active(now); got != c.want {
				t.Fatalf("Active(%d)=%v, want %v", now, got, c.want)
			}
		})
	}
	var nilL *RiskList
	if nilL.Active(now) {
		t.Fatal("nil 名单条目必须不生效")
	}
}

func TestRiskRuleValidateRejectsUnusableRules(t *testing.T) {
	base := func() RiskRule {
		return RiskRule{
			Name: "login_bruteforce", ActionType: ActionLogin, Metric: MetricActionCount,
			Op: OpGTE, Threshold: 10, WindowSeconds: 600, Decision: DecisionBlock,
			State: StateEnabled, Version: 1, Operator: 7,
		}
	}
	if r := base(); r.Validate() != nil {
		t.Fatalf("合法规则被拒: %v", r.Validate())
	}

	cases := []struct {
		name   string
		mutate func(*RiskRule)
		want   error
	}{
		{"empty_name", func(r *RiskRule) { r.Name = "" }, ErrInvalidRule},
		{"long_name", func(r *RiskRule) { r.Name = strings.Repeat("x", 129) }, ErrInvalidRule},
		{"metric_not_implemented", func(r *RiskRule) { r.Metric = "sms_send_count" }, ErrInvalidRule},
		{"action_out_of_range", func(r *RiskRule) { r.ActionType = 9 }, ErrInvalidRule},
		{"bad_op", func(r *RiskRule) { r.Op = 6 }, ErrInvalidRule},
		{"window_zero", func(r *RiskRule) { r.WindowSeconds = 0 }, ErrInvalidRule},
		{"window_over_one_day", func(r *RiskRule) { r.WindowSeconds = 86401 }, ErrInvalidRule},
		{"window_max_allowed", func(r *RiskRule) { r.WindowSeconds = 86400 }, nil},
		{"negative_threshold", func(r *RiskRule) { r.Threshold = -1 }, ErrInvalidRule},
		{"lt_needs_positive_threshold", func(r *RiskRule) { r.Op = OpLT; r.Threshold = 0 }, ErrInvalidRule},
		// 规则裁决为 ALLOW 等于没处罚，必须拒绝（否则会产出无法解释的放行命中）。
		{"decision_allow_rejected", func(r *RiskRule) { r.Decision = DecisionAllow }, ErrInvalidRule},
		{"decision_challenge_ok", func(r *RiskRule) { r.Decision = DecisionChallenge }, nil},
		{"decision_review_ok", func(r *RiskRule) { r.Decision = DecisionReview }, nil},
		{"bad_state", func(r *RiskRule) { r.State = 3 }, ErrInvalidRule},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := base()
			c.mutate(&r)
			err := r.Validate()
			if c.want == nil {
				if err != nil {
					t.Fatalf("应通过校验，实际 %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want.Error()) {
				t.Fatalf("应返回 %v，实际 %v", c.want, err)
			}
		})
	}
}

func TestRiskRuleHitUsesItsOwnOp(t *testing.T) {
	r := &RiskRule{Op: OpGTE, Threshold: 5}
	if r.Hit(4) || !r.Hit(5) || !r.Hit(6) {
		t.Fatal("Hit 必须与 Evaluate 同语义")
	}
}

func TestMergeLabelsDedupesAndSorts(t *testing.T) {
	got := MergeLabels("emulator, proxy ,emulator", []string{" root ", "", "proxy", strings.Repeat("l", 33)})
	if got != "emulator,proxy,root" {
		t.Fatalf("MergeLabels=%q, want emulator,proxy,root（去重+字典序+丢弃空与超长）", got)
	}
	// 同一集合、不同写入顺序必须得到同一串，否则画像行会被无意义地反复更新。
	if a, b := MergeLabels("", []string{"z", "a"}), MergeLabels("a", []string{"z"}); a != b {
		t.Fatalf("标签串不稳定: %q vs %q", a, b)
	}
	if got := splitLabels("  ,  ,"); len(got) != 0 {
		t.Fatalf("全空标签应解析为空，实际 %v", got)
	}
	labels := (&RiskDeviceProfile{Labels: "b,a,b"}).LabelList()
	if len(labels) != 2 || labels[0] != "a" || labels[1] != "b" {
		t.Fatalf("LabelList 应去重并保持字典序，实际 %v", labels)
	}
}

func TestFormatHitsKeepsOrderAndVersion(t *testing.T) {
	s := FormatHits([]int64{9, 4, 7}, []int32{2, 5})
	if s != "9@2,4@5,7@0" {
		t.Fatalf("FormatHits=%q, want 9@2,4@5,7@0（缺失版本补 0，顺序不变）", s)
	}
	ids, versions := ParseHits(s)
	if len(ids) != 3 || ids[0] != 9 || ids[1] != 4 || ids[2] != 7 {
		t.Fatalf("ParseHits 丢失了命中顺序: %v", ids)
	}
	if versions[2] != 0 {
		t.Fatalf("缺失版本应解析为 0，实际 %v", versions)
	}
	if FormatHits(nil, nil) != "" {
		t.Fatal("无命中应存空串")
	}
	// 落库条数上限（VARCHAR(500) 保护）：超出部分被丢弃，但前缀顺序仍可解析。
	many := make([]int64, maxHitsInLog+5)
	for i := range many {
		many[i] = int64(i + 1)
	}
	ids2, _ := ParseHits(FormatHits(many, nil))
	if len(ids2) != maxHitsInLog {
		t.Fatalf("落库命中数应为 %d，实际 %d", maxHitsInLog, len(ids2))
	}
	if ids2[0] != 1 || ids2[len(ids2)-1] != int64(maxHitsInLog) {
		t.Fatalf("截断后应保持前缀顺序: %v", ids2)
	}
	if got, _ := ParseHits("12@3,broken,,7@"); len(got) != 2 || got[0] != 12 || got[1] != 7 {
		t.Fatalf("ParseHits 应跳过脏片段而不是中断，实际 %v", got)
	}
	if got, _ := ParseHits("   "); got != nil {
		t.Fatal("空白串应解析为 nil")
	}
}

func TestIdentityNormalizationRejectsRawPII(t *testing.T) {
	if h := DeviceHash("  Device-ABC "); len(h) != 64 || h != DeviceHash("device-abc") {
		t.Fatalf("DeviceHash 应归一化大小写与空格: %q", h)
	}
	if DeviceHash("   ") != "" {
		t.Fatal("空设备号应返回空串")
	}
	for _, raw := range []string{"1.2.3.4", "1.2.3.4:8080", "2001:db8::1", "1.2.3.4, 5.6.7.8"} {
		if !LooksLikeRawIP(raw) {
			t.Fatalf("%q 应被识别为裸 IP", raw)
		}
	}
	if LooksLikeRawIP("aabbccdd") {
		t.Fatal("十六进制摘要不应被误判为裸 IP")
	}
	if NormalizeIPHash("AABBCCDD") != "aabbccdd" {
		t.Fatal("ip_hash 应小写归一化")
	}
	for _, bad := range []string{"", "tooshort", "aabbccddeeffgg", strings.Repeat("a", 65), "1.2.3.4"} {
		if NormalizeIPHash(bad) != "" {
			t.Fatalf("%q 应被拒绝", bad)
		}
	}
	if NormalizeTargetValue(TargetTypeMid, " 12345 ") != "12345" {
		t.Fatal("mid 目标应为十进制串")
	}
	for _, bad := range []string{"12ab", "-1", "1.2.3.4"} {
		if NormalizeTargetValue(TargetTypeMid, bad) != "" {
			t.Fatalf("mid 目标 %q 应被拒绝", bad)
		}
	}
	if NormalizeTargetValue(TargetTypeDevice, strings.Repeat("ab", 32)) != strings.Repeat("ab", 32) {
		t.Fatal("设备摘要目标应通过")
	}
	if NormalizeTargetValue(TargetTypeIpHash, "1.2.3.4") != "" {
		t.Fatal("名单里禁止出现明文 IP")
	}
	if NormalizeTargetValue(9, "123") != "" {
		t.Fatal("未知目标类型应被拒绝")
	}
	if TruncateHash("abcdef", 3) != "abc" || TruncateHash("ab", 0) != "ab" || TruncateHash("", 5) != "" {
		t.Fatal("TruncateHash 边界错误")
	}
}
