// 本文件把事件消费所需的状态机读写收敛在 repository 上：
// internal/consumer 只依赖这里的小接口，不直接碰 model 层与 SQL，
// 保证消费者可脱离真实 MySQL/Kafka 单测。
package repository

import (
	"context"
	"errors"

	"go-video/services/inbox/model"
)

// ErrEventDeferred 表示事件仍在退避窗口内或正被其它处理器执行。
// 消费者据此返回错误、让 Kafka 不提交位点并在稍后重投。
var ErrEventDeferred = errors.New("inbox: event deferred for retry window")

// ClaimEvent 按 event_id 领取处理权。
// 返回 ClaimAcquired 时调用方必须最终写入 succeeded/retry/dead_letter 之一，
// 否则 processing 会在 staleSeconds 后被其它处理器回收。
func (r *Repository) ClaimEvent(
	ctx context.Context, ev *model.ConsumerOffset, staleSeconds int64,
) (model.ClaimOutcome, *model.ConsumerOffset, error) {
	outcome, current, err := r.offsetMd.Claim(ctx, ev, staleSeconds)
	if err != nil {
		return outcome, current, err
	}
	if outcome == model.ClaimDeferred {
		return outcome, current, ErrEventDeferred
	}
	return outcome, current, nil
}

// MarkEventSucceeded 标记事件处理完成。
func (r *Repository) MarkEventSucceeded(ctx context.Context, eventID string) error {
	return r.offsetMd.MarkSucceeded(ctx, eventID)
}

// MarkEventRetry 记录一次失败并写入退避到期时间与重投所需的原始 payload。
func (r *Repository) MarkEventRetry(
	ctx context.Context, eventID string, nextRetryAt int64, reason, payload string,
) error {
	return r.offsetMd.MarkRetry(ctx, eventID, nextRetryAt, reason, payload)
}

// MarkEventDeadLetter 把事件状态置为 dead_letter，保留 payload 供人工重放。
func (r *Repository) MarkEventDeadLetter(ctx context.Context, eventID, reason, payload string) error {
	return r.offsetMd.MarkDeadLetter(ctx, eventID, reason, payload)
}

// SaveDeadLetter 死信留档，按 (topic, payload_digest) 幂等。
func (r *Repository) SaveDeadLetter(ctx context.Context, dl *model.DeadLetter) (bool, error) {
	return r.dlqMd.InsertIdempotent(ctx, dl)
}

// DueRetryEvents 列出退避到期的事件（含 payload），由 consumer 的清扫循环重投。
// 这是「失败一定收敛」的关键：不依赖 Kafka 是否重投同一条消息。
func (r *Repository) DueRetryEvents(ctx context.Context, now int64, limit int32) ([]*model.ConsumerOffset, error) {
	return r.offsetMd.ListOverdueRetry(ctx, now, limit)
}

// EventState 查询事件当前状态，供排障与单测断言。
func (r *Repository) EventState(ctx context.Context, eventID string) (*model.ConsumerOffset, error) {
	return r.offsetMd.FindOne(ctx, eventID)
}
