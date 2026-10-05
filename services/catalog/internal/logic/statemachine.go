package logic

import (
	"go-video/services/catalog/model"
)

// legalEpisodeTransitions 是集状态机的合法转换表（参考 services/video 的 TransitionState 写法）。
// key: from-state；value: 允许到达的目标状态列表。
//
// 依据 AGENTS.md §8：PGC 集的上架必须通过合法状态机推进，并且
//   - →PUBLISHED（EpStateOnline）必须同时通过 rights 版权窗口与 asset 媒资就绪校验
//     （见 guard.go），回调或运营接口都不能绕过校验直接写入上架态；
//   - 已上架幂等返回、已下架幂等返回，由 logic 分支处理，不进转换表。
var legalEpisodeTransitions = map[int32][]int32{
	model.EpStateDraft:   {model.EpStateOnline},  // 草稿 → 上架
	model.EpStateOnline:  {model.EpStateOffline}, // 上架 → 下架（版权撤回/运营下架）
	model.EpStateOffline: {model.EpStateOnline},  // 下架 → 重新上架（需重新校验）
}

// canTransitionEpisode 校验 from → to 是否为合法转换。
func canTransitionEpisode(from, to int32) bool {
	targets, ok := legalEpisodeTransitions[from]
	if !ok {
		return false
	}
	for _, t := range targets {
		if t == to {
			return true
		}
	}
	return false
}
