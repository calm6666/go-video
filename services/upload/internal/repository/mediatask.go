// mediatask.go 是 media.task.v1 的 payload 与信封装配（本服务唯一产出的事件）。
//
// 为什么单独成文件而不是写进 repository.go：payload 的字段清单是跨服务契约
// （docs/api-and-events.md §5 登记的 media.task.v1），改字段就是改契约，
// 必须和「会话状态推进」这段数据访问代码分开放，才看得清哪一处需要版本递增。
//
// 三处同源约束（common/outbox.CheckRow 会在发布前反查，任一处写歪即判死）：
//  1. outbox.event_id 列 == 信封 event_id；
//  2. outbox.aggregate_id 列（分区键）== 信封 aggregate_id == upload_id；
//  3. outbox.occurred_at 列 == 信封 occurred_at 解析出的 Unix 秒。
package repository

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/zeromicro/go-zero/core/trace"

	"go-video/common/eventenvelope"
	"go-video/common/timeutil"
	"go-video/services/upload/model"
)

// mediaTaskPayload 是 media.task.v1 的 payload（schema_version=1）。
//
// 只放下游接管媒资必需的事实：对象引用（bucket/object_key）、文件规格
// （size/chunk_size/total_chunks/md5）、归属（mid）与 asset 占位 ID。
// 刻意不含预签名 URL 与任何 AK/SK（AGENTS.md §6），也不含调用方 IP。
// 字段只增不改：删除或改名会让已发布的 v1 事件在消费侧解错，必须递增 schema_version。
type mediaTaskPayload struct {
	UploadID    string `json:"upload_id"`
	Mid         int64  `json:"mid"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	TypeID      int32  `json:"typeid"`
	Md5         string `json:"md5,omitempty"`
	Bucket      string `json:"bucket"`
	ObjectKey   string `json:"object_key"`
	AssetID     string `json:"asset_id"`
	ChunkSize   int64  `json:"chunk_size"`
	TotalChunks int32  `json:"total_chunks"`
	CompletedAt int64  `json:"completed_at"`
}

// buildMediaTaskEvent 用「已完成上传的会话行」组装 media.task.v1 信封。
//
// md5/assetID 是本次事务**将要写入**的两个值而不是 sess 上的现值：完成上传前会话行里的
// md5 还是初始化时的旧值、asset_id 还是空串，直接读 sess 会让 payload 停在半完成形态。
//
// completedAt 由调用方传入（与会话行 mtime 同一口径）；信封的 occurred_at 由
// eventenvelope.New 取组事件时刻，两者相差不足一秒，outbox 列只登记后者（见 parseOccurredAt）。
//
// trace_id 取自 ctx 的链路上下文：go-zero 在没有 trace 时返回空串，
// 信封的 trace_id 是 omitempty 字段，因此既不会污染契约也不会丢事件。
func buildMediaTaskEvent(ctx context.Context, sess *model.UploadSession, md5, assetID string,
	completedAt int64) (*eventenvelope.Envelope, error) {
	if sess == nil {
		return nil, fmt.Errorf("upload/repository: media.task.v1 需要会话行")
	}
	body, err := json.Marshal(mediaTaskPayload{
		UploadID:    sess.UploadId,
		Mid:         sess.Mid,
		Filename:    sess.Filename,
		Size:        sess.Size,
		TypeID:      sess.Typeid,
		Md5:         md5,
		Bucket:      sess.Bucket,
		ObjectKey:   sess.ObjectKey,
		AssetID:     assetID,
		ChunkSize:   sess.ChunkSize,
		TotalChunks: sess.TotalChunks,
		CompletedAt: completedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("upload/repository: 序列化 media.task.v1 payload: %w", err)
	}
	env, err := eventenvelope.New(model.Producer, model.EventMediaTask,
		model.AggregateTypeUploadSession, sess.UploadId, model.EventSchemaVersion,
		body, trace.TraceIDFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("upload/repository: 组装 media.task.v1 信封: %w", err)
	}
	return env, nil
}

// marshalEnvelope 序列化信封。Envelope.MarshalJSON 在序列化阶段再跑一次 Validate，
// 因此残缺信封在这里就报错，不会写出一行「六个业务列齐全但 payload 不合法」的 outbox。
func marshalEnvelope(env *eventenvelope.Envelope) (string, error) {
	raw, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("upload/repository: 序列化 media.task.v1 信封: %w", err)
	}
	return string(raw), nil
}

// parseOccurredAt 把信封的 RFC3339 occurred_at 转成 outbox 表存的 Unix 秒，
// 保证「列的 occurred_at」与「信封的 occurred_at」同源（信封是契约真源，列只是检索索引）。
func parseOccurredAt(occurredAt string) (int64, error) {
	t, err := timeutil.ParseRFC3339(occurredAt)
	if err != nil {
		return 0, fmt.Errorf("upload/repository occurred_at %q: %w", occurredAt, err)
	}
	return t.Unix(), nil
}
