package policy

import (
	"sort"

	"go-video/services/risk-control/model"
)

// Config 是引擎的业务参数（由 svc 从 config.RiskControlConf 映射而来）。
type Config struct {
	// ChallengeTTLSeconds 是 CHALLENGE 裁决建议的有效期。
	ChallengeTTLSeconds int64
	// MaxHitsPerDecision 限制返回的命中明细条数（决策仍按全部命中计算）。
	MaxHitsPerDecision int
	// Degrade 是依赖故障时的降级策略。
	Degrade DegradePolicy
}

// DefaultConfig 返回带默认值的引擎配置，避免配置缺失导致语义漂移。
func DefaultConfig() Config {
	return Config{
		ChallengeTTLSeconds: 300,
		MaxHitsPerDecision:  20,
		Degrade: DegradePolicy{
			HighRiskActions:    append([]int32{}, DefaultHighRiskActions...),
			DefaultDecision:    model.DecisionAllow,
			HighRiskDecision:   model.DecisionBlock,
			LocalFallbackLimit: 200,
		},
	}
}

// WithDefaults 补齐零值字段（返回副本，不改接收者语义不清的问题）。
func (c Config) WithDefaults() Config {
	out := c
	def := DefaultConfig()
	if out.ChallengeTTLSeconds <= 0 {
		out.ChallengeTTLSeconds = def.ChallengeTTLSeconds
	}
	if out.MaxHitsPerDecision <= 0 {
		out.MaxHitsPerDecision = def.MaxHitsPerDecision
	}
	out.Degrade = out.Degrade.withDefaults()
	return out
}

// 默认高危动作：1 投稿、5 登录、6 改名、7 直播开播。
// 这些动作一旦误放行代价高（垃圾内容上线、账号被撞库、身份被冒用），
// 因此 DB 故障时选择 BLOCK-on-error；评论/弹幕/关注为低危动作，
// 拒绝会放大故障面并诱发用户重试风暴，选择 ALLOW-on-error。
var DefaultHighRiskActions = []int32{
	model.ActionSubmitVideo,
	model.ActionLogin,
	model.ActionRename,
	model.ActionLiveStart,
}

// DegradePolicy 描述依赖故障时按动作分级的保守策略。
type DegradePolicy struct {
	// HighRiskActions 是走 HighRiskDecision 的动作集合。
	HighRiskActions []int32
	// HighRiskDecision 是高危动作降级后的裁决，默认 BLOCK。
	HighRiskDecision int32
	// DefaultDecision 是其余动作降级后的裁决，默认 ALLOW。
	DefaultDecision int32
	// LocalFallbackLimit 是 Redis 不可用时单实例每动作每窗口的兜底动作数上限，
	// <=0 表示关闭进程内兜底限流。
	LocalFallbackLimit int64
}

// withDefaults 补齐零值。
func (p DegradePolicy) withDefaults() DegradePolicy {
	out := p
	if len(out.HighRiskActions) == 0 {
		out.HighRiskActions = append([]int32{}, DefaultHighRiskActions...)
	}
	if out.HighRiskDecision == 0 {
		out.HighRiskDecision = model.DecisionBlock
	}
	if out.DefaultDecision == 0 {
		out.DefaultDecision = model.DecisionAllow
	}
	return out
}

// DecisionFor 返回动作 action 在依赖故障时应给出的裁决。
func (p DegradePolicy) DecisionFor(action int32) int32 {
	for _, a := range p.HighRiskActions {
		if a == action {
			return p.HighRiskDecision
		}
	}
	return p.DefaultDecision
}

// decisionScore 把裁决映射为 0-100 的风险分基线：
// ALLOW 0、CHALLENGE 25、REVIEW 50、BLOCK 100。
// 多条命中时取最大值，再按额外命中条数每条约 +5，上限 100，
// 使 score 可复算、可解释，而不是黑盒模型分。
func decisionScore(decision int32) int32 {
	switch decision {
	case model.DecisionChallenge:
		return 25
	case model.DecisionReview:
		return 50
	case model.DecisionBlock:
		return 100
	default:
		return 0
	}
}

// Evaluate 是纯函数版决策：按「黑名单 → 生效处罚 → 白名单 → 规则」的优先级产出裁决。
//
// 语义说明：
//   - 黑名单优先，命中即 BLOCK，不再评估规则；
//   - 生效处罚优先于白名单：白名单只豁免规则评估，不能替运营解除处罚，
//     否则「已封禁账号因为在白名单里又能动作」无法审计；
//   - 白名单命中时跳过规则评估（Evaluated=false），返回 ALLOW；
//   - 规则评估阶段，观测值不可得的规则计入 skipped_rule_ids，不视为命中。
func Evaluate(in Input, facts Facts, cfg Config) *Result {
	cfg = cfg.WithDefaults()
	res := &Result{RequestID: in.RequestID}

	switch {
	case len(facts.BlacklistHits) > 0:
		res.Decision = model.DecisionBlock
		res.Basis = BasisBlacklist
		res.Score = decisionScore(res.Decision)
		res.ActionCode = ActionCodeBlocked
		return res

	case facts.Punishment != nil:
		p := facts.Punishment
		res.Punishment = p
		res.Decision = clampPunishmentDecision(p.Decision)
		res.Basis = BasisPunishment
		res.Score = decisionScore(res.Decision)
		res.ActionCode = ActionCodePunished
		if res.Decision == model.DecisionChallenge {
			res.ChallengeTTLSeconds = minInt64(cfg.ChallengeTTLSeconds, p.RemainingSeconds(in.Now))
		}
		return res

	case len(facts.WhitelistHits) > 0:
		res.Decision = model.DecisionAllow
		res.Basis = BasisWhitelist
		return res
	}

	// 计数器整体不可用时的进程内兜底：Redis 挂了不代表没有攻击。
	if facts.LocalLimited {
		res.Decision = model.DecisionBlock
		res.Basis = BasisFallbackLocalLimit
		res.Degraded = true
		res.Score = decisionScore(res.Decision)
		res.ActionCode = ActionCodeUnavailable
		return res
	}

	res.Evaluated = true
	res.Degraded = !facts.CounterAvailable

	hits := make([]Hit, 0, len(facts.Observations))
	for _, o := range facts.Observations {
		if !o.Available {
			res.SkippedRuleIDs = append(res.SkippedRuleIDs, o.RuleID)
			continue
		}
		if !model.Evaluate(o.Op, o.Value, o.Threshold) {
			continue
		}
		hits = append(hits, Hit{
			RuleID:        o.RuleID,
			Version:       o.Version,
			Name:          o.Name,
			Metric:        o.Metric,
			Op:            o.Op,
			Threshold:     o.Threshold,
			Observed:      o.Value,
			WindowSeconds: o.WindowSeconds,
			Decision:      o.Decision,
			Priority:      o.Priority,
		})
	}

	// 命中顺序固定为 (priority DESC, rule_id ASC)，与观测值无关，
	// 保证同一份规则集在任何输入顺序下都产出同样的 hit_rule_ids。
	sortHits(hits)

	res.HitRuleIDs = make([]int64, 0, len(hits))
	res.HitVersions = make([]int32, 0, len(hits))
	for _, h := range hits {
		res.HitRuleIDs = append(res.HitRuleIDs, h.RuleID)
		res.HitVersions = append(res.HitVersions, h.Version)
	}

	if len(hits) == 0 {
		res.Decision = model.DecisionAllow
		res.Basis = BasisNoRule
		return res
	}

	severe := hits[0].Decision
	for _, h := range hits[1:] {
		if model.Severity(h.Decision) > model.Severity(severe) {
			severe = h.Decision
		}
	}
	res.Decision = severe
	res.Basis = BasisRules
	// 置信度以最严重裁决为基线，每多命中一条规则加 5 分（上限 100），
	// 便于运营排序：同样 BLOCK，命中 4 条的比命中 1 条的更可信。
	res.Score = clampScore(decisionScore(severe) + int32(5*(len(hits)-1)))
	res.ActionCode = actionCodeFor(severe)
	if severe == model.DecisionChallenge {
		res.ChallengeTTLSeconds = cfg.ChallengeTTLSeconds
	}

	if cfg.MaxHitsPerDecision > 0 && len(hits) > cfg.MaxHitsPerDecision {
		// 只裁剪返回明细，decision/score 仍按全部命中计算；被裁剪的是优先级最低的规则。
		res.Hits = hits[:cfg.MaxHitsPerDecision]
		res.HitRuleIDs = res.HitRuleIDs[:cfg.MaxHitsPerDecision]
		res.HitVersions = res.HitVersions[:cfg.MaxHitsPerDecision]
	} else {
		res.Hits = hits
	}
	return res
}

// actionCodeFor 把裁决映射为建议动作文案 code。
func actionCodeFor(decision int32) string {
	switch decision {
	case model.DecisionChallenge:
		return ActionCodeChallenge
	case model.DecisionReview:
		return ActionCodeReview
	case model.DecisionBlock:
		return ActionCodeBlocked
	default:
		return ActionCodeNone
	}
}

// clampPunishmentDecision 保证处罚裁决不会被配成 ALLOW（ALLOW 等于没处罚）。
func clampPunishmentDecision(decision int32) int32 {
	switch decision {
	case model.DecisionChallenge, model.DecisionReview, model.DecisionBlock:
		return decision
	default:
		return model.DecisionBlock
	}
}

// clampScore 把分数限制在 0-100。
func clampScore(score int32) int32 {
	if score < 0 {
		return 0
	}
	if score > 100 {
		return 100
	}
	return score
}

// sortHits 按 (priority DESC, rule_id ASC) 稳定排序。
func sortHits(hits []Hit) {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Priority != hits[j].Priority {
			return hits[i].Priority > hits[j].Priority
		}
		return hits[i].RuleID < hits[j].RuleID
	})
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
