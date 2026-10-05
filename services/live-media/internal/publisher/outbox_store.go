// Package publisher 把 live_media_outbox 里「已在业务事务内提交」的事件投递到消息队列。
//
// 分层与 playback、live-ingest 的同类包一致，发布循环本身不再重写：
// 顺序、退避、判死与写库失败中断这些与业务无关的决策全在 go-video/common/outbox，
// 本包只回答 live-media 的四个问题：
//  1. 这张表的行怎么映射成 outbox.Row（topic 由 event_type + schema_version 派生）；
//  2. 哪些 topic 是本服务真实产出的（RequiredTopics，锚在 model 的 9 个 EventType* 常量上）；
//  3. 本表的条件 UPDATE 语义（Mark* 返回受影响行数）怎么映射成引擎要的 error 口径；
//  4. 真实队列客户端怎么建（只在 kafkaruntime_kafka.go，`-tags livemedia_kafka`）。
//
// 证据边界（AGENTS.md §9）：本仓库从未连接过任何 broker。这里能给的结论只到
// 「可编译 / 可静态检查 / 本包单测通过」；「事件真的送达」必须先在 Redpanda 上跑通才算数。
package publisher

import (
	"context"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/common/eventenvelope"
	"go-video/common/outbox"
	"go-video/services/live-media/model"
)

// 复用通用引擎：本包不维护第二份状态机。
type (
	// Publisher 是 live_media_outbox 的发布循环（= common/outbox 的引擎）。
	Publisher = outbox.Publisher
	// Sender 是一次事件投递的抽象，真实实现按构建标签提供。
	Sender = outbox.Sender
	// Options 是发布循环参数。
	Options = outbox.Options
)

// label 是日志与错误信息里的组件标签，运维据此分辨是哪个服务的循环。
const label = "live-media/publisher"

// eventTypes 是本服务全部产出事件类型，逐个取自 model 常量，
// 顺序与 docs/api-and-events.md §5 的登记顺序一致。
//
// 之所以要在发布器里再列一遍：topic 集合必须有一个「服务端侧的事实来源」用于配置校验和
// 行归属判定。写在 yaml 里等于让运维决定本服务发什么事件，写在每个 logic 里等于没有地方
// 能一次问全。新增 EventType* 常量时必须同时加进这里，
// TestEventTypesAndRequiredTopicsStayInSync 会把漏加的那一条变成红用例。
var eventTypes = []string{
	model.EventTypeTranscodeStateChanged,
	model.EventTypeRecordStateChanged,
	model.EventTypeRecordStopped,
	model.EventTypeRecordGapDetected,
	model.EventTypeStreamOutputOnline,
	model.EventTypeStreamOutputOffline,
	model.EventTypeReplayReviewSubmitted,
	model.EventTypeReplayContentStateChanged,
	model.EventTypeRetentionFinished,
}

// RequiredTopics 返回本服务真实产出的 topic 全集（当前 9 个，均为 `<event_type>.v1`）。
// 它们由 model 的 event_type + schema_version 常量拼出，配置校验、行归属判定与单测都以它
// 为锚点，不在任何 yaml 或字面量里重复一遍。
func RequiredTopics() []string {
	topics := make([]string, 0, len(eventTypes))
	for _, eventType := range eventTypes {
		topics = append(topics, eventenvelope.Topic(eventType, model.EventSchemaVersion))
	}
	return topics
}

// OutboxStore 是 outbox.Store 的 live-media 实现：列映射、一致性反查，
// 以及把本表「(受影响行数, error)」的写库口径翻译成引擎要的 error 口径。
type OutboxStore struct {
	outboxMd model.LiveMediaOutboxModel
}

// NewOutboxStore 构造 Store。model 为 nil 时直接失败，不返回「扫不到任何行」的空实现。
func NewOutboxStore(outboxMd model.LiveMediaOutboxModel) (*OutboxStore, error) {
	if outboxMd == nil {
		return nil, errors.New(label + ": outbox model is required")
	}
	return &OutboxStore{outboxMd: outboxMd}, nil
}

// ListPending 读取到期事件并映射成 outbox.Row。
// 排序、state 过滤与 next_retry_at 判定都留在 SQL 里（model.ListPending 只取 state=0），
// 这里不重排：id 升序是同聚合根事件顺序的前提。
func (s *OutboxStore) ListPending(ctx context.Context, now int64, limit int32) ([]*outbox.Row, error) {
	rows, err := s.outboxMd.ListPending(ctx, now, limit)
	if err != nil {
		return nil, fmt.Errorf("%s: 读取 live_media_outbox: %w", label, err)
	}
	records := make([]*outbox.Row, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		records = append(records, toRow(row))
	}
	return records, nil
}

// MarkPublished 置为已发布并写入 published_at（本表有独立位点列，不像 playback 复用 mtime）。
func (s *OutboxStore) MarkPublished(ctx context.Context, id, publishedAt int64) error {
	affected, err := s.outboxMd.MarkPublished(ctx, id, publishedAt)
	if err != nil {
		return fmt.Errorf("%s: live_media_outbox id=%d 标记已发布: %w", label, id, err)
	}
	noteConditionalMiss(ctx, "MarkPublished", id, affected)
	return nil
}

// MarkRetry 记录本次失败并安排下次重试（行仍留在待发布态）。
//
// 引擎传入的 retryCount 在这里刻意不使用：本表的 MarkRetry 写的是
// `retry_count = retry_count + 1`（SQL 侧自增），而不是「覆盖成引擎算出来的值」。
// 单实例下两者恒等（引擎的 nextCount 就是行上的 retry_count+1）；
// 多副本下自增不会被两个副本各算一次的计数互相覆盖。
// 代价是判死边界仍按引擎读到的那一份 retry_count 走，
// 因此并发副本可能让某条事件比 Kafka.MaxRetries 多投几轮，本表没有租约列所以无法避免
// （README 已知缺口）。
func (s *OutboxStore) MarkRetry(ctx context.Context, id int64, _ int32, nextRetryAt int64, lastError string) error {
	affected, err := s.outboxMd.MarkRetry(ctx, id, nextRetryAt, lastError)
	if err != nil {
		return fmt.Errorf("%s: live_media_outbox id=%d 记录重试: %w", label, id, err)
	}
	noteConditionalMiss(ctx, "MarkRetry", id, affected)
	return nil
}

// MarkFailed 判死：state=2 后本服务没有人工放行接口（README 已知缺口），只能人工改库或重投。
func (s *OutboxStore) MarkFailed(ctx context.Context, id int64, lastError string) error {
	affected, err := s.outboxMd.MarkFailed(ctx, id, lastError)
	if err != nil {
		return fmt.Errorf("%s: live_media_outbox id=%d 判死: %w", label, id, err)
	}
	noteConditionalMiss(ctx, "MarkFailed", id, affected)
	return nil
}

// noteConditionalMiss 把「条件 UPDATE 命中 0 行」写成日志而不是错误。
//
// 为什么不报错：本表三个 Mark* 都带 state 守卫（MarkPublished/MarkRetry 要 state ∈
// {待发布, 失败}，MarkFailed 要 state = 待发布），而 ListPending 只取待发布态，
// 所以命中 0 行只有一个成因：这一行在读取之后已经被别的副本给出结论（已发布/已判死），
// 或被清理任务删掉了。此时引擎的结论（「这一行投过了」）依然成立，
// 把它冒泡成错误会让 RunOnce 中断整批，把同一个批次里其余正常行也拖回「本轮没结论」。
// 真实错误只有一种：读库或写库本身失败，那已经由上面的 err 分支带表名冒泡。
func noteConditionalMiss(ctx context.Context, op string, id, affected int64) {
	if affected > 0 {
		return
	}
	logx.WithContext(ctx).Infof("%s: live_media_outbox id=%d %s 命中 0 行："+
		"该行的状态守卫未命中（已被其它副本判定，或已被清理），本行不再重复投递也不视为失败",
		label, id, op)
}

// toRow 完成列 → outbox.Row 的映射与一致性反查。
// 发现缺陷不返回错误：那一行仍要落库给出结论（判死），错误留给真正的读库失败。
func toRow(row *model.LiveMediaOutbox) *outbox.Row {
	rec := &outbox.Row{
		ID:         row.Id,
		EventID:    row.EventId,
		Topic:      eventenvelope.Topic(row.EventType, int(row.SchemaVersion)),
		Key:        row.AggregateId,
		Payload:    row.Payload,
		RetryCount: row.RetryCount,
	}
	rec.Defect = outbox.CheckRow(rec, RequiredTopics())
	return rec
}
