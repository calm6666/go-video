// 本文件是 logic 包的手写打分与排序策略（候选归一、去重、合成分、兜底顺序、配额与不变量校验），
// 不是 goctl 生成产物。
//
// 全部是纯函数：不碰 SQL、不碰下游，输入决定输出，可直接离线单测。
// 三条硬约束（rpc/rank.proto 的契约红线）在这里把守：
//  1. 出参必须是入参候选的子集（assertSubset），一旦破坏就放弃本次结果回退召回原序；
//  2. 排序不可用时要的是「声明过的降级 + 配置里的兜底顺序」，不是空列表冒充成功；
//  3. 同一输入两次排序必须得到同一顺序，所以每个比较器都带 aid 兜底 tie-break。

package logic

import (
	"sort"
	"strings"

	"go-video/services/recommend-rank/internal/repository"
	"go-video/services/recommend-rank/model"
	"go-video/services/recommend-rank/rpc"
)

// rankedCandidate 是排序过程中的一条候选。
// index 永远是在入参 candidates 中的下标（rpc.RankedItem.original_index 的口径），
// 去重后仍保留首次出现位置，这样「结果里第 i 条来自入参第几行」可以逐条核对。
type rankedCandidate struct {
	aid          int64
	source       int32
	recallScore  float64
	rankInSource int32
	index        int32
	// poolVersion / batchID 是召回侧的审计回指字段（rpc CandidateInput 原样带过来）：
	// 它们不参与打分，只进决策摘要，用于「这批结果出自哪一版召回池」。
	poolVersion int64
	batchID     string
	score       float64
	objectives  []*rpc.ObjectiveScore
	reasonCode  string
	scored      bool
}

// toRPC 投影成契约出参。score 未打分时为 0，同时 reason_code 会标明是兜底位。
func (c rankedCandidate) toRPC() *rpc.RankedItem {
	return &rpc.RankedItem{
		Aid:           c.aid,
		Score:         c.score,
		Objectives:    c.objectives,
		Source:        toRPCSource(c.source),
		RecallRank:    c.rankInSource,
		ReasonCode:    c.reasonCode,
		OriginalIndex: c.index,
	}
}

// normalizeCandidates 校验并归一入参候选（保留入参顺序与下标）。
// 条数与 limit 的上限检查在调用方做（那是「请求规模」问题）；
// 这里只保证每一条都可解释：aid 为正、来源在枚举内。
// 重复 aid 不在此处剔除，因为 input_digest 必须按「原始入参序列」计算（幂等回放靠它比对）。
func normalizeCandidates(in []*rpc.CandidateInput, maxCount int) ([]rankedCandidate, error) {
	if len(in) > maxCount {
		return nil, model.ErrTooManyCandidates
	}
	out := make([]rankedCandidate, 0, len(in))
	for i, c := range in {
		if c == nil {
			return nil, model.ErrInvalidAid
		}
		if c.GetAid() <= 0 {
			return nil, model.ErrInvalidAid
		}
		source := int32(c.GetSource())
		if !model.ValidSource(source) {
			return nil, model.ErrInvalidSource
		}
		out = append(out, rankedCandidate{
			aid:          c.GetAid(),
			source:       source,
			recallScore:  c.GetRecallScore(),
			rankInSource: c.GetRankInSource(),
			index:        int32(i),
			// 召回侧审计回指：负数视为「未透传」（0 已经是契约里的「未知」值），
			// 不能让它进到 pool_version 列里变成「第 -5 版召回池」这种无法解释的事实。
			poolVersion: maxZero(c.GetPoolVersion()),
			batchID:     clipIdent(c.GetBatchId()),
		})
	}
	return out, nil
}

// maxZero 把负数归零（pool_version=0 的语义是「召回侧未透传」）。
func maxZero(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// clipIdent 裁剪召回批次号：它只进 degrade_detail 文本（表里没有对应列），
// 不裁剪会让一条长 batch_id 把整列明细挤掉（clip 的 clipped 标记会掩盖真正的故障说明）。
func clipIdent(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > colIdent {
		return v[:colIdent]
	}
	return v
}

// poolProvenance 汇总本次候选的召回池版本与批次号。
//
// 一次排序的候选理应来自同一个池：混池（多版本/多批次）会让「这批结果出自哪一版召回」
// 这个问题没有答案，所以把不一致写进降级明细（表里只有 pool_version 一列，只能落单值）。
// 返回 (可代表的池版本, 是否混池, 批次说明)。
func poolProvenance(in []rankedCandidate) (int64, bool, string) {
	var version int64
	seenVersion := false
	mixedVersion := false
	batches := make(map[string]struct{}, 4)
	for _, c := range in {
		if c.poolVersion != 0 {
			if !seenVersion {
				version, seenVersion = c.poolVersion, true
			} else if version != c.poolVersion {
				mixedVersion = true
			}
		}
		if c.batchID != "" {
			batches[c.batchID] = struct{}{}
		}
	}
	if len(batches) == 0 {
		return version, mixedVersion, ""
	}
	ids := make([]string, 0, len(batches))
	for b := range batches {
		ids = append(ids, b)
	}
	sort.Strings(ids)
	return version, mixedVersion, "recall batch_id=" + clip(ids, colIdent)
}

// dedupe 按 aid 去重（保留首次出现），返回去重后的候选与被剔除条数。
// 重复 aid 若都参与打分，同一内容会在结果里出现两次并挤掉两个位置，
// 所以必须在打分前收敛；计数进 FilterStat.dedup_filtered，不允许静默吞。
func dedupe(in []rankedCandidate) ([]rankedCandidate, int32) {
	seen := make(map[int64]struct{}, len(in))
	out := make([]rankedCandidate, 0, len(in))
	for _, c := range in {
		if _, ok := seen[c.aid]; ok {
			continue
		}
		seen[c.aid] = struct{}{}
		out = append(out, c)
	}
	return out, int32(len(in) - len(out))
}

// keepVisible 按内容安全结论过滤；false（含未给出结论）表示不可见。
// 「读不到结论」按不通过处理是 safetyGate 的契约，不允许默认放行。
func keepVisible(in []rankedCandidate, visible map[int64]bool) ([]rankedCandidate, int32) {
	out := make([]rankedCandidate, 0, len(in))
	for _, c := range in {
		if !visible[c.aid] {
			continue
		}
		out = append(out, c)
	}
	return out, int32(len(in) - len(out))
}

// aidSequence 返回入参原始顺序的 aid 序列（input_digest 的输入）。
func aidSequence(in []rankedCandidate) []int64 {
	ordered := make([]rankedCandidate, len(in))
	copy(ordered, in)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })
	return aidList(ordered)
}

// --- 合成分 ---

// scoreOutcome 是一次打分的产出。
// 三个列表互不相交：scored 参与最终排序，unscored 是预算耗尽未打分的，
// dropped 是被 missing_policy=drop_source 整路剔除的。
type scoreOutcome struct {
	scored         []rankedCandidate
	unscored       []rankedCandidate
	dropped        []rankedCandidate
	droppedSources []int32
	// rejected 为 true 表示 missing_policy=reject 命中缺失特征：
	// 本次不可排序，调用方必须走降级而不是拿半截结果当正常。
	rejected bool
}

// scoreRequest 是打分入参（特征由调用方取好后传入，保持本函数无 IO）。
type scoreRequest struct {
	cands     []rankedCandidate
	weights   map[string]float64
	itemFeats map[int64]repository.FeatureVector
	// priors 是 spm 侧的内容质量指标（按 aid 分组），仅在模型预估缺失时作为先验兜底。
	priors          map[int64]repository.FeatureVector
	missingPolicy   string
	overrides       *overrideSet
	budgetExhausted func() bool
}

// scoreCandidates 按「模型登记的多目标权重 × 特征取值」合成排序分。
//
// 取值口径（README「待接线接口」同源）：
//   - feature-store 以目标同名特征提供预估分（pred_click / pred_finish / pred_interact / pred_negative）；
//   - 内容侧特征优先，缺失时用 spm 的质量指标先验（objectivePriorKey 映射）；
//   - pred_negative 参与扣分，其余加分；合成分 = Σ w·v(正项) − Σ w·v(负项)；
//   - cold_start_boost 只放大冷启动路候选的合成分，不改变其它路。
//
// 这里不做任何「人工置顶/加权某个 aid」的事：可调的只有权重乘子与配额（AGENTS.md §7）。
func scoreCandidates(req scoreRequest) scoreOutcome {
	weights := req.overrides.effectiveWeights(req.weights)
	policy := req.missingPolicy
	if policy == "" {
		policy = model.MissingPolicyDefault
	}
	out := scoreOutcome{
		scored:   make([]rankedCandidate, 0, len(req.cands)),
		dropped:  make([]rankedCandidate, 0, 8),
		unscored: make([]rankedCandidate, 0, 8),
	}
	droppedSource := make(map[int32]struct{}, 6)

	for idx, c := range req.cands {
		if _, ok := droppedSource[c.source]; ok {
			out.dropped = append(out.dropped, c)
			continue
		}
		if req.budgetExhausted != nil && req.budgetExhausted() {
			// 预算耗尽：剩余候选整体不打分，由调用方裁剪（不拿「0 分」冒充结果参与排序）。
			out.unscored = append(out.unscored, req.cands[idx:]...)
			return out
		}
		item := req.itemFeats[c.aid]
		prior := req.priors[c.aid]
		value := scoreValue{item: item, prior: prior}
		entries := make([]string, 0, len(weights))
		for k := range weights {
			entries = append(entries, k)
		}
		sort.Strings(entries)

		var (
			total   float64
			missing []string
			objs    []*rpc.ObjectiveScore
		)
		for _, key := range entries {
			v, ok := value.lookup(key)
			if !ok {
				missing = append(missing, key)
				continue
			}
			w := weights[key]
			if key == model.ObjectiveNegative {
				total -= w * v
			} else {
				total += w * v
			}
			objs = append(objs, &rpc.ObjectiveScore{Objective: key, Value: roundScore(v)})
		}
		if len(missing) > 0 {
			switch policy {
			case model.MissingPolicyReject:
				out.unscored = append(out.unscored, c)
				out.rejected = true
				continue
			case model.MissingPolicyDropSource:
				droppedSource[c.source] = struct{}{}
				out.droppedSources = append(out.droppedSources, c.source)
				out.dropped = append(out.dropped, c)
				continue
			default: // MissingPolicyDefault：缺失目标不参与合成分（等价 0 贡献），
				// 已在 objectives 里缺席，审计读能看到「这一位少了哪个目标」。
			}
		}
		if req.overrides != nil && req.overrides.coldStartBoost > 0 && c.source == model.SourceCold {
			total *= 1 + req.overrides.coldStartBoost
		}
		c.score = roundScore(total)
		c.objectives = objs
		c.scored = true
		out.scored = append(out.scored, c)
	}
	return out
}

// scoreValue 是一条候选的特征取值来源。
type scoreValue struct {
	item  repository.FeatureVector
	prior repository.FeatureVector
}

// lookup 先取模型预估（特征 key 与目标同名），缺失时取 spm 质量指标先验。
func (s scoreValue) lookup(objective string) (float64, bool) {
	if v, ok := s.item[objective]; ok {
		return v, true
	}
	if key, ok := objectivePriorKey(objective); ok {
		if v, ok2 := s.prior[key]; ok2 {
			return v, true
		}
	}
	return 0, false
}

// objectivePriorKey 把优化目标映射到 spm 内容质量指标的约定 key。
// 约定（不是 spm 契约强制）：接 spm 时必须按这四个名字提供，否则先验永远取不到，
// 排序会持续按「特征缺失」降级 —— 这条已写进 README 待接线清单。
func objectivePriorKey(objective string) (string, bool) {
	switch objective {
	case model.ObjectiveClick:
		return "click_rate", true
	case model.ObjectiveFinish:
		return "finish_rate", true
	case model.ObjectiveInteract:
		return "interact_rate", true
	case model.ObjectiveNegative:
		return "negative_rate", true
	default:
		return "", false
	}
}

// --- 排序与兜底顺序 ---

// orderScored 按合成分降序；并列时按召回路优先级、路内序号、原始下标、aid 升序，
// 保证「同一输入两次排序」逐位一致（可重放的前提）。
func orderScored(in []rankedCandidate) []rankedCandidate {
	out := make([]rankedCandidate, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return recallOrderLess(out[i], out[j])
	})
	return out
}

// orderRecallOrder 是 FALLBACK_STRATEGY_RECALL_ORDER 的口径：
// 召回路优先级（与 recommend-recall 的 SourcePriority 同序）+ 路内序号 + aid。
// 只用入参自带的 source / rank_in_source，不引入对召回服务的运行时依赖。
func orderRecallOrder(in []rankedCandidate) []rankedCandidate {
	out := make([]rankedCandidate, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool { return recallOrderLess(out[i], out[j]) })
	return out
}

// orderInputOrder 是 FALLBACK_STRATEGY_SAFETY_ONLY 的口径：
// 只做安全过滤与去重，不打分，因此保持调用方给来的顺序（即召回原序的入参形态）。
func orderInputOrder(in []rankedCandidate) []rankedCandidate {
	out := make([]rankedCandidate, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool { return out[i].index < out[j].index })
	return out
}

func recallOrderLess(a, b rankedCandidate) bool {
	pa, pb := model.SourcePriority(a.source), model.SourcePriority(b.source)
	if pa != pb {
		return pa < pb
	}
	if a.rankInSource != b.rankInSource {
		return a.rankInSource < b.rankInSource
	}
	if a.index != b.index {
		return a.index < b.index
	}
	return a.aid < b.aid
}

// truncate 取前 limit 条，返回结果与被截断条数（含配额剔除，见 applyQuota 注释）。
func truncate(in []rankedCandidate, limit int) ([]rankedCandidate, int32) {
	if limit < 0 || len(in) <= limit {
		return in, 0
	}
	return in[:limit], int32(len(in) - limit)
}

// applyQuota 按变体的 source_quota 限制各召回路的出参条数。
//
// 超额条目计入 truncated：契约的 FilterStat 没有「配额丢弃」这一位，
// 它与 limit 截断同属「已排序但未下发」，共用计数并在 degrade_detail 里分开说明（README 已记）。
// 比例为 0 时该路一条都不出（登记 0 就是「这一轮不要这路」的显式表达）。
func applyQuota(in []rankedCandidate, quota map[int32]float64, limit int) ([]rankedCandidate, int32) {
	if len(quota) == 0 || limit <= 0 {
		return in, 0
	}
	budget := make(map[int32]int, len(quota))
	for source, ratio := range quota {
		if ratio <= 0 {
			budget[source] = 0
			continue
		}
		n := int(float64(limit) * ratio)
		if ratio < 1 && n < 1 {
			n = 1 // 给了正比例就至少留一条，否则小 limit 下该路会整体消失
		}
		budget[source] = n
	}
	out := make([]rankedCandidate, 0, len(in))
	dropped := int32(0)
	for _, c := range in {
		allow, ok := budget[c.source]
		if !ok {
			out = append(out, c) // 未登记配额的路不受限
			continue
		}
		if allow <= 0 {
			dropped++
			continue
		}
		budget[c.source] = allow - 1
		out = append(out, c)
	}
	return out, dropped
}

// assertSubset 校验出参 aid 都在入参集合内且互不重复。
// 返回 false 时调用方必须放弃本次结果回退召回原序（model.ErrCandidateSubsetBroken）。
func assertSubset(out []rankedCandidate, allowed map[int64]struct{}) bool {
	seen := make(map[int64]struct{}, len(out))
	for _, c := range out {
		if _, ok := allowed[c.aid]; !ok {
			return false
		}
		if _, dup := seen[c.aid]; dup {
			return false
		}
		seen[c.aid] = struct{}{}
	}
	return true
}

// markReasonCode 给最终出参打回显 reason_code（兜底顺序与模型顺序要能区分）。
func markReasonCode(in []rankedCandidate, fallback string) []rankedCandidate {
	out := make([]rankedCandidate, len(in))
	copy(out, in)
	for i := range out {
		out[i].reasonCode = reasonCodeFor(out[i].source, fallback)
	}
	return out
}
