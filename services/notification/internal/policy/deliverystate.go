package policy

import (
	"errors"
	"fmt"
	"sort"

	"go-video/services/notification/model"
)

// 投递任务状态机（AGENTS.md §8 同源思路：回调/重试只能推进合法状态）。
//
//	pending  -> sent | retry | failed | dead_letter | suppressed
//	retry    -> sent | retry | failed | dead_letter
//	dead_letter -> pending            （仅运营 RetryDeadLetter 触发）
//	sent / failed / suppressed          终态
var deliveryTransitions = map[int32][]int32{
	model.DeliveryStatePending:    {model.DeliveryStateSent, model.DeliveryStateRetry, model.DeliveryStateFailed, model.DeliveryStateDeadLetter, model.DeliveryStateSuppressed},
	model.DeliveryStateRetry:      {model.DeliveryStateSent, model.DeliveryStateRetry, model.DeliveryStateFailed, model.DeliveryStateDeadLetter},
	model.DeliveryStateDeadLetter: {model.DeliveryStatePending},
	model.DeliveryStateSent:       {},
	model.DeliveryStateFailed:     {},
	model.DeliveryStateSuppressed: {},
}

// ErrIllegalTransition 状态机非法迁移。
var ErrIllegalTransition = errors.New("notification/policy: illegal delivery state transition")

// CanTransitDelivery 判断 from -> to 是否为合法迁移。
func CanTransitDelivery(from, to int32) bool {
	if from == to {
		// 同状态写回视为幂等重放（例如重试任务再次进入 retry）。
		return from == model.DeliveryStatePending || from == model.DeliveryStateRetry
	}
	for _, next := range deliveryTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// DeliverySourceStates 返回允许迁移到 to 的所有源状态，供带守卫的 UPDATE 使用
// （`WHERE delivery_id = ? AND state IN (...)`），确保并发下不会覆盖终态。
func DeliverySourceStates(to int32) []int32 {
	from := make([]int32, 0, 4)
	for src, targets := range deliveryTransitions {
		for _, t := range targets {
			if t == to {
				from = append(from, src)
				break
			}
		}
	}
	if to == model.DeliveryStateRetry {
		from = append(from, model.DeliveryStateRetry)
	}
	sort.Slice(from, func(i, j int) bool { return from[i] < from[j] })
	return from
}

// MustTransit 迁移不合法时返回带上下文的错误。
func MustTransit(from, to int32, deliveryID string) error {
	if CanTransitDelivery(from, to) {
		return nil
	}
	return fmt.Errorf("%w: delivery=%s %d->%d", ErrIllegalTransition, deliveryID, from, to)
}

// 事件消费状态机（docs/api-and-events.md §6：received/processing/succeeded/retry/dead_letter）。
var eventTransitions = map[int32][]int32{
	model.EventStateReceived:   {model.EventStateProcessing, model.EventStateSucceeded, model.EventStateRetry, model.EventStateDeadLetter},
	model.EventStateProcessing: {model.EventStateSucceeded, model.EventStateRetry, model.EventStateDeadLetter},
	model.EventStateRetry:      {model.EventStateProcessing, model.EventStateSucceeded, model.EventStateRetry, model.EventStateDeadLetter},
	model.EventStateSucceeded:  {},
	model.EventStateDeadLetter: {model.EventStateProcessing}, // 运营重投死信时重新进入处理
}

// CanTransitEvent 判断事件状态迁移是否合法。
func CanTransitEvent(from, to int32) bool {
	if from == to {
		return from == model.EventStateRetry || from == model.EventStateReceived
	}
	for _, next := range eventTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// EventSourceStates 返回允许迁移到 to 的源状态集合。
func EventSourceStates(to int32) []int32 {
	from := make([]int32, 0, 4)
	for src, targets := range eventTransitions {
		for _, t := range targets {
			if t == to {
				from = append(from, src)
				break
			}
		}
	}
	sort.Slice(from, func(i, j int) bool { return from[i] < from[j] })
	return from
}
