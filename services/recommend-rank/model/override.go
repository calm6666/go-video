package model

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// 实验参数覆盖（rank_experiment.overrides）的受控 key 登记处。
//
// 为什么需要一个白名单而不是自由 JSON：
// overrides 是「实验能改什么」的唯一入口。一旦允许任意 key，
// 它就变成一条绕过评审的上线通道 —— 例如塞进 {"boost_aids":[123]} 或
// {"ad_slot":1}，等价于手工修改推荐结果并引入商业化逻辑，两者都是 AGENTS.md §7 的禁区。
// 因此这里只登记「怎么算分、怎么打散、怎么限频」这三类参数，
// 新增 key 必须先在此登记（代码评审）再被 rpc UpsertExperiment 接受，
// 未登记的 key 由 ValidateOverrideKeys 在写库前拒绝。
//
// 值本身仍是 JSON 文本原样落库（人工审计要能读到原值），
// 结构校验（权重区间、配额之和）属打分逻辑，归第二轮。
const (
	// OverrideScoreWeights 多目标权重的缩放表，内层 key 必须是 SupportedObjectives() 之一。
	OverrideScoreWeights = "score_weights"
	// OverrideDiversityGap 打散间隔（同作者/同标签相邻最小位差），整数。
	OverrideDiversityGap = "diversity_gap"
	// OverrideFrequencyCap 频控上限（同作者/同标签在窗口内最多出现次数），整数。
	OverrideFrequencyCap = "frequency_cap"
	// OverrideSourceQuota 各召回路的出参配额比例，内层 key 是 RankSource 编号的十进制串。
	OverrideSourceQuota = "source_quota"
	// OverrideColdStartBoost 冷启动加分系数，浮点数。
	OverrideColdStartBoost = "cold_start_boost"
)

// MaxOverridesBytes 是 overrides 落库的硬字节上限。
// 与 rank_experiment.overrides 的 VARCHAR(4096) 以及 config.Rank.MaxOverridesBytes 默认值一致；
// 配置只能在此基础上收紧（第二轮 logic 会再按配置值校验一次）。
const MaxOverridesBytes = 4096

// SupportedOverrides 返回已登记的实验参数覆盖 key 集合。
func SupportedOverrides() map[string]struct{} {
	return map[string]struct{}{
		OverrideScoreWeights:   {},
		OverrideDiversityGap:   {},
		OverrideFrequencyCap:   {},
		OverrideSourceQuota:    {},
		OverrideColdStartBoost: {},
	}
}

// ValidOverrideKey 判定覆盖参数名是否已登记。
func ValidOverrideKey(key string) bool {
	_, ok := SupportedOverrides()[key]
	return ok
}

// ValidateOverrideKeys 校验 overrides 是「顶层为对象、且所有 key 均已登记」的 JSON。
//
// 空串与 "{}" 合法（表示不覆盖任何参数）；maxBytes <= 0 表示只查 key 不查长度。
// 拒绝时错误里带上未登记的 key（排序后拼接），否则运维只看到一句 invalid 无从下手。
func ValidateOverrideKeys(overrides string, maxBytes int) error {
	trimmed := strings.TrimSpace(overrides)
	if trimmed == "" || trimmed == "{}" {
		return nil
	}
	if maxBytes > 0 && len(trimmed) > maxBytes {
		return fmt.Errorf("%w: %d bytes > %d", ErrOverridesTooLarge, len(trimmed), maxBytes)
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return fmt.Errorf("%w: overrides must be a JSON object: %v", ErrInvalidOverrideKey, err)
	}
	var rejected []string
	for key := range parsed {
		if !ValidOverrideKey(key) {
			rejected = append(rejected, key)
		}
	}
	if len(rejected) > 0 {
		sort.Strings(rejected)
		return fmt.Errorf("%w: %s", ErrInvalidOverrideKey, strings.Join(rejected, ","))
	}
	return nil
}
