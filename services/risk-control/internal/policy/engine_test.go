package policy

import (
	"testing"

	"go-video/services/risk-control/model"
)

// 本文件覆盖决策引擎纯函数 Evaluate：名单/处罚优先级、hit_rule_ids 顺序、
// 不可观测规则的跳过、分数与明细裁剪。全部不依赖 Redis/MySQL。

const testNow = int64(1_700_000_000)

func countObservation(ruleID int64, window, value int64, op int32, threshold int64, decision int32, priority int32) Observation {
	return Observation{
		RuleID: ruleID, Version: 1, Name: "rule", Metric: model.MetricActionCount,
		Op: op, Threshold: threshold, WindowSeconds: window, Decision: decision,
		Priority: priority, Value: value, Available: true,
	}
}

func TestEvaluateBlacklistBeatsEverything(t *testing.T) {
	in := Input{RequestID: "req-1", Mid: 42, Action: model.ActionLogin, Now: testNow}
	facts := Facts{
		BlacklistHits:    []string{"black:mid=42"},
		WhitelistHits:    []string{"white:mid=42"},
		Punishment:       &PunishmentView{ID: 9, Decision: model.DecisionReview},
		Observations:     []Observation{countObservation(1, 60, 999, model.OpGTE, 1, model.DecisionBlock, 100)},
		CounterAvailable: true,
	}
	res := Evaluate(in, facts, Config{})
	if res.Decision != model.DecisionBlock || res.Basis != BasisBlacklist {
		t.Fatalf("黑名单必须最优先 BLOCK，实际 decision=%d basis=%s", res.Decision, res.Basis)
	}
	if res.ActionCode != ActionCodeBlocked || res.Score != 100 {
		t.Fatalf("黑名单裁决应带 blocked 文案与满分, code=%q score=%d", res.ActionCode, res.Score)
	}
	if res.Evaluated {
		t.Fatal("黑名单命中时不得再评估规则（hit_rule_ids 会被误解释）")
	}
	if len(res.HitRuleIDs) != 0 {
		t.Fatalf("黑名单命中不应返回规则命中, 实际 %v", res.HitRuleIDs)
	}
}

func TestEvaluatePunishmentBeatsWhitelist(t *testing.T) {
	// 白名单只豁免规则评估，不能替运营解除处罚，否则「已封禁账号因为在白名单里又能动作」。
	in := Input{RequestID: "req-2", Mid: 42, Action: model.ActionComment, Now: testNow}
	facts := Facts{
		WhitelistHits:    []string{"white:mid=42"},
		Punishment:       &PunishmentView{ID: 9, Scope: model.ActionComment, Decision: model.DecisionBlock, StartAt: testNow - 10, EndAt: testNow + 100, ReasonCode: "risk.comment.blocked"},
		CounterAvailable: true,
	}
	res := Evaluate(in, facts, Config{ChallengeTTLSeconds: 300})
	if res.Decision != model.DecisionBlock || res.Basis != BasisPunishment {
		t.Fatalf("处罚应优先于白名单，实际 decision=%d basis=%s", res.Decision, res.Basis)
	}
	if res.Punishment == nil || res.Punishment.ID != 9 {
		t.Fatal("处罚裁决必须回带处罚快照供客户端展示剩余时长")
	}
	if res.ActionCode != ActionCodePunished {
		t.Fatalf("ActionCode=%q, want %q", res.ActionCode, ActionCodePunished)
	}
}

func TestEvaluatePunishmentChallengeTTLIsCappedByRemainingTime(t *testing.T) {
	in := Input{Mid: 1, Action: model.ActionComment, Now: testNow}
	facts := Facts{
		Punishment:       &PunishmentView{ID: 3, Decision: model.DecisionChallenge, StartAt: testNow - 5, EndAt: testNow + 30},
		CounterAvailable: true,
	}
	res := Evaluate(in, facts, Config{ChallengeTTLSeconds: 300})
	if res.ChallengeTTLSeconds != 30 {
		t.Fatalf("CHALLENGE 有效期应取「配置值」与「处罚剩余」的较小者，实际 %d", res.ChallengeTTLSeconds)
	}

	// 永久处罚没有剩余时长可下发，TTL 归 0 表示客户端每次都要重新校验。
	facts.Punishment.EndAt = 0
	if res = Evaluate(in, facts, Config{ChallengeTTLSeconds: 300}); res.ChallengeTTLSeconds != 0 {
		t.Fatalf("永久 CHALLENGE 处罚的 TTL 应为 0，实际 %d", res.ChallengeTTLSeconds)
	}
}

func TestEvaluatePunishmentWithInvalidDecisionFallsBackToBlock(t *testing.T) {
	in := Input{Mid: 1, Action: model.ActionComment, Now: testNow}
	for _, bad := range []int32{model.DecisionAllow, 0, 99} {
		facts := Facts{Punishment: &PunishmentView{ID: 1, Decision: bad}, CounterAvailable: true}
		res := Evaluate(in, facts, Config{})
		if res.Decision != model.DecisionBlock {
			t.Fatalf("处罚裁决 %d 非法时应收敛为 BLOCK，实际 %d", bad, res.Decision)
		}
	}
}

func TestEvaluateWhitelistSkipsRules(t *testing.T) {
	in := Input{Mid: 1, Action: model.ActionComment, Now: testNow}
	facts := Facts{
		WhitelistHits: []string{"white:mid=1"},
		Observations:  []Observation{countObservation(5, 60, 999, model.OpGTE, 1, model.DecisionBlock, 10)},
	}
	res := Evaluate(in, facts, Config{})
	if res.Decision != model.DecisionAllow || res.Basis != BasisWhitelist {
		t.Fatalf("白名单应放行，实际 decision=%d basis=%s", res.Decision, res.Basis)
	}
	if res.Evaluated || len(res.HitRuleIDs) != 0 {
		t.Fatalf("白名单命中应跳过规则评估，Evaluated=%v hits=%v", res.Evaluated, res.HitRuleIDs)
	}
	if res.ActionCode != ActionCodeNone || res.Score != 0 {
		t.Fatalf("放行不应带动作文案与分数, code=%q score=%d", res.ActionCode, res.Score)
	}
}

// hit_rule_ids 的顺序是决策解释的一部分：同一份规则集在任何观测顺序下都必须一致。
func TestEvaluateHitRuleOrderIsDeterministic(t *testing.T) {
	in := Input{Mid: 7, Action: model.ActionDanmaku, Now: testNow}
	build := func(shuffle bool) []Observation {
		a := countObservation(11, 60, 50, model.OpGTE, 10, model.DecisionChallenge, 5)
		b := countObservation(22, 600, 50, model.OpGTE, 10, model.DecisionBlock, 9)
		c := countObservation(33, 60, 50, model.OpGTE, 10, model.DecisionReview, 5)
		d := countObservation(4, 60, 50, model.OpGTE, 10, model.DecisionBlock, 9)
		if shuffle {
			return []Observation{c, d, a, b}
		}
		return []Observation{a, b, c, d}
	}
	for _, shuf := range []bool{false, true} {
		res := Evaluate(in, Facts{Observations: build(shuf), CounterAvailable: true}, Config{})
		got := res.HitRuleIDs
		want := []int64{4, 22, 11, 33} // priority DESC(9,9,5,5) 内再按 rule_id ASC
		if len(got) != len(want) {
			t.Fatalf("命中数=%d, want %d (%v)", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("shuffle=%v 时 hit_rule_ids=%v, want %v", shuf, got, want)
			}
		}
		if res.Decision != model.DecisionBlock {
			t.Fatalf("最严重裁决应为 BLOCK，实际 %d", res.Decision)
		}
		if res.Basis != BasisRules || !res.Evaluated {
			t.Fatalf("Basis=%q Evaluated=%v", res.Basis, res.Evaluated)
		}
		if res.Hits[0].RuleID != 4 || res.HitVersions[0] != 1 {
			t.Fatalf("命中明细与版本必须同序，hits[0]=%d versions[0]=%d", res.Hits[0].RuleID, res.HitVersions[0])
		}
	}
}

func TestEvaluateScoreGrowsWithExtraHitsAndCapsAt100(t *testing.T) {
	in := Input{Mid: 1, Action: model.ActionComment, Now: testNow}
	// 基线：CHALLENGE=25、REVIEW=50、BLOCK=100，额外命中每条 +5。
	cases := []struct {
		name  string
		obs   []Observation
		want  int32
		wDeal int32
	}{
		{"single_challenge", []Observation{countObservation(1, 60, 9, model.OpGTE, 5, model.DecisionChallenge, 1)}, 25, model.DecisionChallenge},
		{"two_challenge", []Observation{
			countObservation(1, 60, 9, model.OpGTE, 5, model.DecisionChallenge, 3),
			countObservation(2, 60, 9, model.OpGTE, 5, model.DecisionChallenge, 2),
		}, 30, model.DecisionChallenge},
		{"review_with_two_extra", []Observation{
			countObservation(1, 60, 9, model.OpGTE, 5, model.DecisionReview, 3),
			countObservation(2, 60, 9, model.OpGTE, 5, model.DecisionChallenge, 2),
		}, 55, model.DecisionReview},
		{"block_capped", []Observation{
			countObservation(1, 60, 9, model.OpGTE, 5, model.DecisionBlock, 3),
			countObservation(2, 60, 9, model.OpGTE, 5, model.DecisionBlock, 2),
			countObservation(3, 60, 9, model.OpGTE, 5, model.DecisionBlock, 1),
			countObservation(4, 60, 9, model.OpGTE, 5, model.DecisionBlock, 0),
			countObservation(5, 60, 9, model.OpGTE, 5, model.DecisionBlock, -1),
			countObservation(6, 60, 9, model.OpGTE, 5, model.DecisionBlock, -2),
			countObservation(7, 60, 9, model.OpGTE, 5, model.DecisionBlock, -3),
			countObservation(8, 60, 9, model.OpGTE, 5, model.DecisionBlock, -4),
			countObservation(9, 60, 9, model.OpGTE, 5, model.DecisionBlock, -5),
			countObservation(10, 60, 9, model.OpGTE, 5, model.DecisionBlock, -6),
			countObservation(11, 60, 9, model.OpGTE, 5, model.DecisionBlock, -7),
			countObservation(12, 60, 9, model.OpGTE, 5, model.DecisionBlock, -8),
			countObservation(13, 60, 9, model.OpGTE, 5, model.DecisionBlock, -9),
			countObservation(14, 60, 9, model.OpGTE, 5, model.DecisionBlock, -10),
			countObservation(15, 60, 9, model.OpGTE, 5, model.DecisionBlock, -11),
		}, 100, model.DecisionBlock},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Evaluate(in, Facts{Observations: c.obs, CounterAvailable: true}, Config{})
			if res.Score != c.want || res.Decision != c.wDeal {
				t.Fatalf("score=%d decision=%d, want %d/%d", res.Score, res.Decision, c.want, c.wDeal)
			}
			if res.ActionCode == ActionCodeNone {
				t.Fatal("非放行裁决必须给出建议动作文案 key")
			}
		})
	}
}

func TestEvaluateUnreachableRuleDoesNotHit(t *testing.T) {
	in := Input{Mid: 1, Action: model.ActionComment, Now: testNow}
	obs := []Observation{
		countObservation(1, 60, 5, model.OpGTE, 10, model.DecisionBlock, 1), // 未达阈值
		countObservation(2, 60, 5, model.OpLT, 0, model.DecisionBlock, 1),   // 非法比较方向 -> 恒不命中
		countObservation(3, 60, 0, model.OpGT, 0, model.DecisionBlock, 1),   // 计数为 0 不算命中
	}
	res := Evaluate(in, Facts{Observations: obs, CounterAvailable: true}, Config{})
	if res.Decision != model.DecisionAllow || res.Basis != BasisNoRule {
		t.Fatalf("无命中应 ALLOW/no_rule，实际 decision=%d basis=%s", res.Decision, res.Basis)
	}
	if res.Evaluated != true {
		t.Fatal("完成规则评估必须标记 Evaluated=true")
	}
}

// 指标不可观测（Redis 档位不够、主体缺失、指标未实现）时进入 skipped_rule_ids，
// 绝不能当成「值为 0」从而误判命中或误判放行理由。
func TestEvaluateUnavailableObservationsAreSkipped(t *testing.T) {
	in := Input{Mid: 1, Action: model.ActionComment, Now: testNow}
	unavailable := Observation{RuleID: 77, Version: 3, Metric: model.MetricIpActionCount, Op: model.OpGTE, Threshold: 0, Decision: model.DecisionBlock, Available: false}
	facts := Facts{
		Observations:     []Observation{unavailable, countObservation(80, 60, 3, model.OpGTE, 1, model.DecisionChallenge, 1)},
		CounterAvailable: false,
	}
	res := Evaluate(in, facts, Config{})
	if len(res.SkippedRuleIDs) != 1 || res.SkippedRuleIDs[0] != 77 {
		t.Fatalf("不可观测规则应进 skipped_rule_ids，实际 %v", res.SkippedRuleIDs)
	}
	if !res.Degraded {
		t.Fatal("计数器不可用时裁决必须标记 Degraded")
	}
	if res.Decision != model.DecisionChallenge {
		t.Fatalf("跳过的规则不得影响其它规则评估，实际 decision=%d", res.Decision)
	}
	if res.ChallengeTTLSeconds != 300 {
		t.Fatalf("ChallengeTTLSeconds=%d, want 默认补齐后的 300", res.ChallengeTTLSeconds)
	}
}

func TestEvaluateLocalFallbackLimitsWhenCounterBlind(t *testing.T) {
	in := Input{Mid: 1, Action: model.ActionSubmitVideo, Now: testNow}
	facts := Facts{
		Observations:     []Observation{countObservation(1, 60, 999, model.OpGTE, 1, model.DecisionBlock, 1)},
		CounterAvailable: false,
		LocalLimited:     true,
	}
	res := Evaluate(in, facts, Config{})
	if res.Decision != model.DecisionBlock || res.Basis != BasisFallbackLocalLimit || !res.Degraded {
		t.Fatalf("Redis 全盲且触发进程内兜底时应 BLOCK/fallback，实际 decision=%d basis=%s degraded=%v", res.Decision, res.Basis, res.Degraded)
	}
	if res.ActionCode != ActionCodeUnavailable {
		t.Fatalf("兜底拒绝应提示稍后重试，实际 %q", res.ActionCode)
	}
	if res.Evaluated || len(res.HitRuleIDs) != 0 {
		t.Fatal("兜底路径不产出规则命中，避免伪造解释")
	}
}

func TestEvaluateMaxHitsTrimsDetailsOnly(t *testing.T) {
	in := Input{Mid: 1, Action: model.ActionComment, Now: testNow}
	obs := make([]Observation, 0, 4)
	for i := int64(1); i <= 4; i++ {
		obs = append(obs, countObservation(i, 60, 99, model.OpGTE, 1, model.DecisionBlock, int32(10-i)))
	}
	res := Evaluate(in, Facts{Observations: obs, CounterAvailable: true}, Config{MaxHitsPerDecision: 2})
	if len(res.Hits) != 2 || len(res.HitRuleIDs) != 2 {
		t.Fatalf("明细应被裁剪到 2 条，实际 %d/%d", len(res.Hits), len(res.HitRuleIDs))
	}
	// 分数与裁决仍按全部 4 条命中计算（100 + 3*5 -> 上限 100）。
	if res.Decision != model.DecisionBlock || res.Score != 100 {
		t.Fatalf("裁剪不得改变裁决，实际 decision=%d score=%d", res.Decision, res.Score)
	}
	if res.Hits[0].RuleID != 1 || res.Hits[1].RuleID != 2 {
		t.Fatalf("被裁剪的应是优先级最低的规则，实际 %v", res.HitRuleIDs)
	}
}

func TestConfigDefaults(t *testing.T) {
	c := Config{}.WithDefaults()
	if c.ChallengeTTLSeconds != 300 || c.MaxHitsPerDecision != 20 {
		t.Fatalf("默认值未按预期补齐: %+v", c)
	}
	if c.Degrade.DecisionFor(model.ActionSubmitVideo) != model.DecisionBlock {
		t.Fatal("默认高危动作应 BLOCK-on-error")
	}
	if c.Degrade.DecisionFor(model.ActionComment) != model.DecisionAllow {
		t.Fatal("默认低危动作应 ALLOW-on-error")
	}
	// 自定义高危集合不能被默认值污染。
	custom := Config{Degrade: DegradePolicy{HighRiskActions: []int32{model.ActionDanmaku}, DefaultDecision: model.DecisionBlock}}.WithDefaults()
	if custom.Degrade.DecisionFor(model.ActionDanmaku) != model.DecisionBlock ||
		custom.Degrade.DecisionFor(model.ActionComment) != model.DecisionBlock {
		t.Fatalf("自定义降级策略被改写: %+v", custom.Degrade)
	}
	if custom.Degrade.HighRiskDecision != model.DecisionBlock {
		t.Fatal("未指定高危裁决时默认 BLOCK")
	}
}
