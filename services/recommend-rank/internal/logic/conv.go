// 本文件是 logic 包的手写投影层（model 行 -> rpc 消息），不是 goctl 生成产物。
//
// 为什么单独成文件：投影是「契约回显」的唯一出口，实验/决策摘要两处共用同一份口径，
// 复制两份迟早会让 GetRankRuntimeConfig 与 ListRankDecisions 对同一行给出不同答案。
// 这里只做字段搬运与枚举映射，不含业务判定（判定在各 logic 文件与 scoring.go）。
package logic

import (
	"fmt"
	"sort"

	"go-video/services/recommend-rank/model"
	"go-video/services/recommend-rank/rpc"
)

// experimentInfo 把变体行投影成 rpc.ExperimentInfo。
// reason 回显 note 列（rpc 用 reason 表达「为什么改」，落库列名是 note）。
func experimentInfo(e *model.RankExperiment) *rpc.ExperimentInfo {
	if e == nil {
		return nil
	}
	return &rpc.ExperimentInfo{
		ExpKey:               e.ExpKey,
		VariantKey:           e.VariantKey,
		LayerKey:             e.LayerKey,
		BucketStart:          e.BucketStart,
		BucketEnd:            e.BucketEnd,
		ModelKey:             e.ModelKey,
		ModelVersion:         e.ModelVersion,
		FeatureConfigVersion: e.FeatureConfigVersion,
		Overrides:            e.Overrides,
		State:                toRPCExpState(e.State),
		Revision:             e.Revision,
		StartAt:              e.StartAt,
		EndAt:                e.EndAt,
		Operator:             e.Operator,
		Reason:               e.Note,
		Ctime:                e.Ctime,
		Mtime:                e.Mtime,
	}
}

func experimentInfos(rows []*model.RankExperiment) []*rpc.ExperimentInfo {
	out := make([]*rpc.ExperimentInfo, 0, len(rows))
	for _, row := range rows {
		if info := experimentInfo(row); info != nil {
			out = append(out, info)
		}
	}
	return out
}

// decisionInfo 把决策摘要行投影成 rpc.RankDecisionInfo（审计读的唯一出口）。
// 契约要求「不存在时为 null，不返回空对象冒充命中」，所以调用方拿到 nil 就返回 nil。
func decisionInfo(row *model.RankDecisionLog) *rpc.RankDecisionInfo {
	if row == nil {
		return nil
	}
	return &rpc.RankDecisionInfo{
		DecisionId:           row.DecisionID,
		RequestId:            row.RequestID,
		TraceId:              row.TraceID,
		SnapshotId:           row.SnapshotID,
		Mid:                  row.Mid,
		SubjectType:          toRPCSubject(row.SubjectType),
		SubjectId:            row.SubjectID,
		Scene:                row.Scene,
		Platform:             toRPCPlatform(row.Platform),
		AppVersion:           row.AppVersion,
		ExpKey:               row.ExpKey,
		VariantKey:           row.VariantKey,
		BucketNo:             row.BucketNo,
		ModelKey:             row.ModelKey,
		ModelVersion:         row.ModelVersion,
		FeatureConfigVersion: row.FeatureConfigVersion,
		InputCount:           row.InputCount,
		ReturnedCount:        row.ReturnedCount,
		ResultDigest:         row.ResultDigest,
		TopAids:              model.SplitAids(row.TopAids),
		Degraded:             row.Degraded != 0,
		Reason:               degradeReasonFromKey(row.DegradeReason),
		Fallback:             fallbackFromKey(row.FallbackStrategy),
		Filters:              filterStat(row),
		CostMs:               row.CostMs,
		Ctime:                row.Ctime,
	}
}

func decisionInfos(rows []*model.RankDecisionLog) []*rpc.RankDecisionInfo {
	out := make([]*rpc.RankDecisionInfo, 0, len(rows))
	for _, row := range rows {
		if info := decisionInfo(row); info != nil {
			out = append(out, info)
		}
	}
	return out
}

// filterStat 回显五个过滤计数（丢弃必须留痕，不允许只回条数）。
func filterStat(row *model.RankDecisionLog) *rpc.FilterStat {
	if row == nil {
		return nil
	}
	return &rpc.FilterStat{
		SafetyFiltered:    row.SafetyFiltered,
		FrequencyFiltered: row.FrequencyFiltered,
		DedupFiltered:     row.DedupFiltered,
		DiversifiedMoved:  row.DiversifiedMoved,
		Truncated:         row.Truncated,
	}
}

// rankedItems 把打分结果投影成 rpc.RankedItem。
func rankedItems(scored []rankedCandidate) []*rpc.RankedItem {
	out := make([]*rpc.RankedItem, 0, len(scored))
	for _, c := range scored {
		out = append(out, c.toRPC())
	}
	return out
}

// aidList 抽取有序 aid 列表（input_digest / result_digest 的输入，顺序敏感）。
func aidList(in []rankedCandidate) []int64 {
	out := make([]int64, 0, len(in))
	for _, c := range in {
		out = append(out, c.aid)
	}
	return out
}

// sourceSummary 渲染候选来源分布 "1:12,2:8"（来源 key 升序，落在 VARCHAR(255) 内）。
// 它是「结果由哪几路候选产生」的证据，必须来自实际参与本次排序的集合。
func sourceSummary(in []rankedCandidate) string {
	counts := make(map[int32]int, 6)
	for _, c := range in {
		counts[c.source]++
	}
	keys := make([]int, 0, len(counts))
	for k := range counts {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d:%d", k, counts[int32(k)]))
	}
	return clip(parts, colSourceSummary)
}
