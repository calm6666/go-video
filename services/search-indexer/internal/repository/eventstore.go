// eventstore.go 是事件消费流水（search_consumer_offset）与死信（search_dead_letter）
// 的访问入口，供 internal/consumer 使用。
//
// 语义（docs/api-and-events.md §6）：
//   - event_id 唯一，重复投递只生效一次（MarkReceived 返回 duplicate）；
//   - 状态机 received → processing → succeeded / retry → dead_letter；
//   - retry 记录保留 payload_json，进程重启后 sweeper 可继续退避重试；
//   - 进入终态后清空 payload_json，死信表只留摘要，不长期堆积业务原文。
package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"go-video/services/search-indexer/model"
)

// PayloadDigest 返回 payload 的 sha256 前 32 个 hex 字符，用于死信登记与排障比对。
func PayloadDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])[:32]
}

// MarkEventReceived 登记事件；duplicate=true 表示该 event_id 已处理过，调用方应直接跳过。
func (r *Repository) MarkEventReceived(ctx context.Context, rec *model.SearchConsumerOffset) (bool, error) {
	if rec.EventID == "" {
		return false, fmt.Errorf("search-indexer: event_id is required")
	}
	if rec.State == "" {
		rec.State = model.OffsetStateReceived
	}
	now := model.NowUnix()
	if rec.Ctime == 0 {
		rec.Ctime = now
	}
	rec.Mtime = now
	return r.offsetMd.MarkReceived(ctx, rec)
}

// EventByCursor 按 event_id 查询流水；不存在返回 (nil, nil)。
func (r *Repository) EventByCursor(ctx context.Context, eventID string) (*model.SearchConsumerOffset, error) {
	return r.offsetMd.FindByEventID(ctx, eventID)
}

// MarkEventProcessing 标记开始处理。
func (r *Repository) MarkEventProcessing(ctx context.Context, eventID string) error {
	return r.offsetMd.MarkProcessing(ctx, eventID, model.NowUnix())
}

// MarkEventSucceeded 标记成功并清空 payload。
func (r *Repository) MarkEventSucceeded(ctx context.Context, eventID string) error {
	return r.offsetMd.MarkSucceeded(ctx, eventID, model.NowUnix())
}

// MarkEventRetry 记录一次退避重试。
func (r *Repository) MarkEventRetry(ctx context.Context, eventID string, retryCount int32, nextRetryAt int64, lastError string) error {
	return r.offsetMd.MarkRetry(ctx, eventID, retryCount, nextRetryAt, sanitizeError(lastError), model.NowUnix())
}

// MarkEventDeadLetter 标记死信并清空 payload_json。
func (r *Repository) MarkEventDeadLetter(ctx context.Context, eventID, lastError string) error {
	return r.offsetMd.MarkDeadLetter(ctx, eventID, sanitizeError(lastError), model.NowUnix())
}

// DueRetryEvents 查询到期待重试的事件（含 payload，可重投）。
func (r *Repository) DueRetryEvents(ctx context.Context, limit int) ([]*model.SearchConsumerOffset, error) {
	return r.offsetMd.ListDueForRetry(ctx, model.NowUnix(), limit)
}

// CountEventsByState 统计某状态事件数（积压观测）。
func (r *Repository) CountEventsByState(ctx context.Context, state string) (int64, error) {
	return r.offsetMd.CountByState(ctx, state)
}

// RecordDeadLetter 登记死信（含 payload 摘要）。返回 existed=true 表示重复登记。
func (r *Repository) RecordDeadLetter(ctx context.Context, eventID, eventType, topic string, payload []byte, reason string) (bool, error) {
	if eventID == "" {
		return false, fmt.Errorf("search-indexer: dead letter requires event_id")
	}
	now := model.NowUnix()
	existed, err := r.dlqMd.Insert(ctx, &model.SearchDeadLetter{
		EventID:       eventID,
		EventType:     eventType,
		Topic:         topic,
		PayloadDigest: PayloadDigest(payload),
		Reason:        sanitizeError(reason),
		State:         model.DLQStateOpen,
		Ctime:         now,
		Mtime:         now,
	})
	if err != nil {
		return false, err
	}
	// INSERT IGNORE 命中 uniq_event_id 时 existed=true，重复死信不会覆盖首次登记原因。
	return existed, nil
}

// DeadLetterCount 统计死信数；state 为空表示全部。
func (r *Repository) DeadLetterCount(ctx context.Context, state string) (int64, error) {
	return r.dlqMd.Count(ctx, state)
}

// ListOpenDeadLetters 列出待处理死信（运维重放入口）。
func (r *Repository) ListOpenDeadLetters(ctx context.Context, limit int) ([]*model.SearchDeadLetter, error) {
	return r.dlqMd.ListOpen(ctx, limit)
}

// MarkDeadLetterState 更新死信处理状态（replayed/discarded）。
func (r *Repository) MarkDeadLetterState(ctx context.Context, eventID, state string) error {
	switch state {
	case model.DLQStateReplayed, model.DLQStateDiscarded, model.DLQStateOpen:
	default:
		return fmt.Errorf("search-indexer: invalid dead letter state %q", state)
	}
	return r.dlqMd.UpdateState(ctx, eventID, state, model.NowUnix())
}
