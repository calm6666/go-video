package logic

import (
	"go-video/services/video/model"
)

// legalTransitions 是稿件状态机的合法转换表。
// key: from-state；value: 允许到达的目标状态列表。
// 依据 AGENTS.md §8，TransitionState 严格校验，禁止直接写 PUBLISHED。
var legalTransitions = map[int32][]int32{
	model.StateDraft:          {model.StateUploading, model.StateDeleted},
	model.StateUploading:      {model.StateUploaded, model.StateDeleted},
	model.StateUploaded:       {model.StateScanning, model.StateDeleted},
	model.StateScanning:       {model.StateTranscoding, model.StateRejected},
	model.StateTranscoding:    {model.StateReadyForReview, model.StateRejected},
	model.StateReadyForReview: {model.StateApproved, model.StateRejected},
	model.StateRejected:       {model.StateAppeal, model.StateDeleted},
	model.StateAppeal:         {model.StateReadyForReview, model.StateRejected},
	model.StateApproved:       {model.StateScheduled},
	model.StateScheduled:      {model.StatePublished},
	model.StatePublished:      {model.StateOffline, model.StateExpired, model.StateDeleted},
	model.StateOffline:        {model.StatePublished, model.StateDeleted},
}

// canTransition 校验 from → to 是否为合法转换。
func canTransition(from, to int32) bool {
	targets, ok := legalTransitions[from]
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
