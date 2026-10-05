// consumer.go 实现事件消费主流程：解析信封 → 按 event_id 去重 → 路由 → 写索引
// → 成功/退避重试/死信。
//
// 为什么状态机与 MQ 解耦：退避重试与死信全部持久化在 search_consumer_offset，
// 不依赖 broker 重投也能收敛，因此这套逻辑可以在无网络环境下单测。队列侧只保留一个
// 窄接口（见 queue.go 的 QueueFactory / Handler），Kafka 客户端只出现在
// kafkaruntime_kafka.go（`-tags searchindexer_kafka`），默认构建由
// kafkaruntime_disabled.go 显式声明运行时未链接；接入步骤与证据边界见 README「缺口」章节。
package consumer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/common/eventenvelope"
	"go-video/common/timeutil"
	"go-video/services/search-indexer/internal/esclient"
	"go-video/services/search-indexer/internal/repository"
	"go-video/services/search-indexer/model"
)

// Message 一条队列投递。Topic/Key/Value 与 kq 的回调参数一一对应；
// Partition/Offset 只在能拿到位点的读取端（或死信回放任务）才有值，
// kq 推送路径拿不到，保持 0 而不伪造（幂等只看 event_id）。
type Message struct {
	Topic     string
	Partition int32
	Offset    int64
	Key       []byte
	Value     []byte
}

// Store 是消费流程需要的持久化能力，由 *repository.Repository 实现。
// 独立成接口是为了让重试/死信状态机可以用假实现做确定性单测（错误必须真实返回）。
type Store interface {
	MarkEventReceived(ctx context.Context, rec *model.SearchConsumerOffset) (bool, error)
	MarkEventProcessing(ctx context.Context, eventID string) error
	MarkEventSucceeded(ctx context.Context, eventID string) error
	MarkEventRetry(ctx context.Context, eventID string, retryCount int32, nextRetryAt int64, lastError string) error
	MarkEventDeadLetter(ctx context.Context, eventID, lastError string) error
	RecordDeadLetter(ctx context.Context, eventID, eventType, topic string, payload []byte, reason string) (bool, error)
	DueRetryEvents(ctx context.Context, limit int) ([]*model.SearchConsumerOffset, error)
	UpsertDoc(ctx context.Context, doc *esclient.ContentDoc, force bool) (*repository.UpsertResult, error)
	PatchHeat(ctx context.Context, contentID int64, contentType int32, heat esclient.Heat, force bool) (*repository.UpsertResult, error)
	DeleteContent(ctx context.Context, contentID int64, contentType int32, purge bool, reason string) (*repository.DeleteOutcome, error)
}

// Options 消费与重试参数。
type Options struct {
	// MaxRetries 累计尝试次数上限（含首次），达到即转死信。
	MaxRetries int
	// InProcessAttempts 单条消息在进程内立即重试的次数（应对瞬时抖动）。
	InProcessAttempts int
	// BaseBackoff 退避基数。
	BaseBackoff time.Duration
	// MaxBackoff 退避上限。
	MaxBackoff time.Duration
	// RetryBatchLimit 单轮 sweeper 处理的重试事件数。
	RetryBatchLimit int
	// SweepIdleWait 无到期重试时的轮询间隔。
	SweepIdleWait time.Duration
}

// DefaultOptions 返回带默认值的配置。
func DefaultOptions() Options {
	return Options{
		MaxRetries:        5,
		InProcessAttempts: 2,
		BaseBackoff:       5 * time.Second,
		MaxBackoff:        30 * time.Minute,
		RetryBatchLimit:   50,
		SweepIdleWait:     10 * time.Second,
	}
}

func (o *Options) normalize() {
	d := DefaultOptions()
	if o.MaxRetries <= 0 {
		o.MaxRetries = d.MaxRetries
	}
	if o.InProcessAttempts <= 0 {
		o.InProcessAttempts = d.InProcessAttempts
	}
	if o.InProcessAttempts > o.MaxRetries {
		o.InProcessAttempts = o.MaxRetries
	}
	if o.BaseBackoff <= 0 {
		o.BaseBackoff = d.BaseBackoff
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = d.MaxBackoff
	}
	if o.RetryBatchLimit <= 0 {
		o.RetryBatchLimit = d.RetryBatchLimit
	}
	if o.SweepIdleWait <= 0 {
		o.SweepIdleWait = d.SweepIdleWait
	}
}

// Consumer 事件消费器。
type Consumer struct {
	store Store
	opts  Options
}

// New 构造 Consumer。
func New(store Store, opts Options) *Consumer {
	opts.normalize()
	return &Consumer{store: store, opts: opts}
}

// ProcessMessage 处理单条投递。
// 返回值只用于日志与指标：本服务把重试状态持久化在 search_consumer_offset，
// 不依赖 MQ 的 redelivery，因此返回值不阻塞位点前进。
func (c *Consumer) ProcessMessage(ctx context.Context, m *Message) error {
	if m == nil || len(m.Value) == 0 {
		return nil
	}
	env, err := ParseEnvelope(m.Value)
	if err != nil {
		// 信封解析失败：拿不到 event_id，用 payload 摘要合成主键落死信，
		// 保证「坏消息」可被追溯，而不是被静默丢弃。
		synth := malformedID(m.Value)
		if _, derr := c.store.RecordDeadLetter(ctx, synth, "", m.Topic, m.Value, "malformed envelope: "+err.Error()); derr != nil {
			logx.WithContext(ctx).Errorf("search-indexer/consumer: 登记格式错误事件失败 topic=%s err=%v", m.Topic, derr)
		}
		return permanent(err)
	}

	if !SupportedEventType(env.EventType) {
		// 订阅配置比消费能力宽时（例如整 topic 订阅），跳过而不是死信：
		// 未消费不是错误，重复死信会淹没真实故障。
		logx.WithContext(ctx).Infof("search-indexer/consumer: 忽略不支持的事件类型 %s event_id=%s",
			env.EventType, env.EventID)
		return nil
	}

	rec := &model.SearchConsumerOffset{
		EventID:     env.EventID,
		EventType:   env.EventType,
		Topic:       topicOf(env, m.Topic),
		PartitionNo: m.Partition,
		OffsetNo:    m.Offset,
		State:       model.OffsetStateReceived,
		OccurredAt:  occurredAtSeconds(env.OccurredAt),
		PayloadJSON: string(m.Value),
	}
	duplicate, err := c.store.MarkEventReceived(ctx, rec)
	if err != nil {
		// 去重表不可用时不做写入：宁可重复消费也不允许跳过幂等检查。
		return fmt.Errorf("consumer: mark event received: %w", err)
	}
	if duplicate {
		logx.WithContext(ctx).Infof("search-indexer/consumer: 重复事件已跳过 event_id=%s", env.EventID)
		return nil
	}
	if err := c.store.MarkEventProcessing(ctx, env.EventID); err != nil {
		return fmt.Errorf("consumer: mark processing: %w", err)
	}

	attempts := 0
	var lastErr error
	for attempts < c.opts.InProcessAttempts {
		attempts++
		lastErr = c.apply(ctx, env)
		if lastErr == nil || isPermanent(lastErr) {
			break
		}
		logx.WithContext(ctx).Errorf("search-indexer/consumer: 应用事件失败（第 %d 次）event_id=%s type=%s err=%v",
			attempts, env.EventID, env.EventType, lastErr)
		if attempts < c.opts.InProcessAttempts {
			if !sleepCtx(ctx, BackoffDelay(attempts, c.opts.BaseBackoff, c.opts.MaxBackoff)) {
				break // ctx 已取消，剩余尝试交给 sweeper
			}
		}
	}

	switch {
	case lastErr == nil:
		if err := c.store.MarkEventSucceeded(ctx, env.EventID); err != nil {
			return fmt.Errorf("consumer: mark succeeded: %w", err)
		}
		return nil
	case isPermanent(lastErr):
		return c.deadLetter(ctx, env.EventID, env.EventType, topicOf(env, m.Topic), m.Value, lastErr)
	default:
		return c.retryLater(ctx, env, m, attempts, lastErr)
	}
}

// retryLater 持久化退避信息：达到上限即转死信。
func (c *Consumer) retryLater(ctx context.Context, env *eventenvelope.Envelope, m *Message, attempts int, cause error) error {
	if RetryDeadlineReached(attempts, c.opts.MaxRetries) {
		return c.deadLetter(ctx, env.EventID, env.EventType, topicOf(env, m.Topic), m.Value, cause)
	}
	next := time.Now().Add(BackoffDelay(attempts, c.opts.BaseBackoff, c.opts.MaxBackoff)).Unix()
	if err := c.store.MarkEventRetry(ctx, env.EventID, int32(attempts), next, formatAttemptError(attempts, cause)); err != nil {
		return fmt.Errorf("consumer: mark retry: %w", err)
	}
	logx.WithContext(ctx).Errorf("search-indexer/consumer: 事件转入退避重试 event_id=%s attempts=%d next_retry_at=%d err=%v",
		env.EventID, attempts, next, cause)
	return cause
}

// deadLetter 转死信：更新流水状态并登记死信行。
//
// eventID 由调用方显式传入而不是从信封再取一次：清扫器定位的是流水行本身，
// 若 payload 内的 event_id 与行主键不一致（合成主键、上游换号重投等），
// 用 payload 的值会更新到不存在的行上，真实行会永远停在 retry 反复空转。
func (c *Consumer) deadLetter(ctx context.Context, eventID, eventType, topic string, payload []byte, cause error) error {
	reason := formatAttemptError(1, cause)
	if err := c.store.MarkEventDeadLetter(ctx, eventID, reason); err != nil {
		logx.WithContext(ctx).Errorf("search-indexer/consumer: 标记死信失败 event_id=%s err=%v", eventID, err)
	}
	if _, err := c.store.RecordDeadLetter(ctx, eventID, eventType, topic, payload, reason); err != nil {
		logx.WithContext(ctx).Errorf("search-indexer/consumer: 登记死信失败 event_id=%s err=%v", eventID, err)
	}
	logx.WithContext(ctx).Errorf("search-indexer/consumer: 事件转入死信 event_id=%s type=%s err=%v",
		eventID, eventType, cause)
	return cause
}

// apply 按事件类型路由到具体写路径。返回错误区分「可重试」与「永久」。
func (c *Consumer) apply(ctx context.Context, env *eventenvelope.Envelope) error {
	switch env.EventType {
	case EventTypeContentPublished:
		var p ContentPublishedPayload
		if err := unmarshalPayload(env.Payload, &p); err != nil {
			return permanent(err)
		}
		contentID := p.ContentID
		if contentID <= 0 {
			v, err := aggregateIDToInt64(env.AggregateID)
			if err != nil {
				return permanent(err)
			}
			contentID = v
		}
		kind, purge, err := ClassifyAction(p.Action)
		if err != nil {
			return permanent(err)
		}
		if kind == ActionKindUpsert {
			doc, err := DocFromContentEvent(env, &p)
			if err != nil {
				return permanent(err)
			}
			res, err := c.store.UpsertDoc(ctx, doc, false)
			if err != nil {
				return err // OpenSearch/MySQL 抖动：交给退避重试
			}
			logx.WithContext(ctx).Infof("search-indexer/consumer: content.published event_id=%s content_id=%d outcome=%s index=%s",
				env.EventID, contentID, res.Outcome, res.Index)
			return nil
		}
		if p.ContentType <= 0 {
			return permanent(fmt.Errorf("consumer: %s 事件缺少 content_type，无法定位投影 content_id=%d", env.EventType, contentID))
		}
		res, err := c.store.DeleteContent(ctx, contentID, p.ContentType, purge, p.Action)
		if err != nil {
			return err
		}
		logx.WithContext(ctx).Infof("search-indexer/consumer: content.published 下架处理 event_id=%s content_id=%d outcome=%s",
			env.EventID, contentID, res.Outcome)
		return nil

	case EventTypeEngagementAction:
		var p EngagementActionPayload
		if err := unmarshalPayload(env.Payload, &p); err != nil {
			return permanent(err)
		}
		contentID, contentType, heat, err := HeatFromEngagementEvent(env, &p)
		if err != nil {
			return permanent(err)
		}
		res, err := c.store.PatchHeat(ctx, contentID, contentType, heat, false)
		if err != nil {
			return err
		}
		switch res.Outcome {
		case repository.OutcomeMissing:
			// 正文投影还没到（互动比发布更快到达的乱序场景）：交给退避重试。
			return fmt.Errorf("consumer: content doc not ready for heat update, content_id=%d", contentID)
		case repository.OutcomeSkippedStale:
			logx.WithContext(ctx).Infof("search-indexer/consumer: 热度快照过旧已忽略 event_id=%s content_id=%d",
				env.EventID, contentID)
		}
		return nil
	}
	return permanent(fmt.Errorf("consumer: unsupported event type %s", env.EventType))
}

// SweepOnce 处理一轮到期的重试事件，返回处理条数。
// 这是重启后仍能继续退避重试的关键：payload 存在 search_consumer_offset，
// 不依赖 MQ 重投。
func (c *Consumer) SweepOnce(ctx context.Context) (int, error) {
	rows, err := c.store.DueRetryEvents(ctx, c.opts.RetryBatchLimit)
	if err != nil {
		return 0, fmt.Errorf("consumer: load due retries: %w", err)
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		c.retryOne(ctx, row)
	}
	return len(rows), nil
}

// retryOne 重投单条到期事件，并推进状态机。
func (c *Consumer) retryOne(ctx context.Context, row *model.SearchConsumerOffset) {
	env, err := ParseEnvelope([]byte(row.PayloadJSON))
	if err != nil {
		// 原文已损坏，无法再解析：直接死信，不占用重试配额。
		_ = c.store.MarkEventDeadLetter(ctx, row.EventID, "stored payload unparsable: "+err.Error())
		if _, derr := c.store.RecordDeadLetter(ctx, row.EventID, row.EventType, row.Topic, []byte(row.PayloadJSON),
			"stored payload unparsable"); derr != nil {
			logx.WithContext(ctx).Errorf("search-indexer/consumer: 登记死信失败 event_id=%s err=%v", row.EventID, derr)
		}
		return
	}

	attempts := int(row.RetryCount) + 1
	if err := c.store.MarkEventProcessing(ctx, row.EventID); err != nil {
		logx.WithContext(ctx).Errorf("search-indexer/consumer: 标记 processing 失败 event_id=%s err=%v", row.EventID, err)
		return
	}
	lastErr := c.apply(ctx, env)
	switch {
	case lastErr == nil:
		if err := c.store.MarkEventSucceeded(ctx, row.EventID); err != nil {
			logx.WithContext(ctx).Errorf("search-indexer/consumer: 标记 succeeded 失败 event_id=%s err=%v", row.EventID, err)
		}
		logx.WithContext(ctx).Infof("search-indexer/consumer: 重试成功 event_id=%s attempts=%d", row.EventID, attempts)
	case isPermanent(lastErr):
		_ = c.deadLetter(ctx, row.EventID, row.EventType, row.Topic, []byte(row.PayloadJSON), lastErr)
	default:
		if RetryDeadlineReached(attempts, c.opts.MaxRetries) {
			_ = c.deadLetter(ctx, row.EventID, row.EventType, row.Topic, []byte(row.PayloadJSON), lastErr)
			return
		}
		next := time.Now().Add(BackoffDelay(attempts, c.opts.BaseBackoff, c.opts.MaxBackoff)).Unix()
		if err := c.store.MarkEventRetry(ctx, row.EventID, int32(attempts), next, formatAttemptError(attempts, lastErr)); err != nil {
			logx.WithContext(ctx).Errorf("search-indexer/consumer: 更新重试信息失败 event_id=%s err=%v", row.EventID, err)
		}
	}
}

// RunRetrySweeper 周期性推进退避重试队列，直到 ctx 取消。
func (c *Consumer) RunRetrySweeper(ctx context.Context) error {
	for {
		n, err := c.SweepOnce(ctx)
		if err != nil {
			logx.WithContext(ctx).Errorf("search-indexer/consumer: 重试清扫失败 err=%v", err)
		}
		wait := c.opts.SweepIdleWait
		if n > 0 {
			// 还有积压时立刻再来一轮，缩短追平时间。
			wait = time.Second
		}
		if !sleepCtx(ctx, wait) {
			return ctx.Err()
		}
	}
}

// malformedID 为无法解析的事件合成死信主键。
func malformedID(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "malformed_" + hex.EncodeToString(sum[:])[:24]
}

// topicOf 优先使用信封版本化 topic，回退到投递元数据。
func topicOf(env *eventenvelope.Envelope, fallback string) string {
	if t := eventenvelope.Topic(env.EventType, env.SchemaVersion); t != "" {
		return t
	}
	return fallback
}

// occurredAtSeconds 解析 RFC3339 事件时间为 Unix 秒。
func occurredAtSeconds(s string) int64 {
	if t, err := timeutil.ParseRFC3339(s); err == nil {
		return t.Unix()
	}
	return model.NowUnix()
}

// sleepCtx 等待 d，返回 false 表示 ctx 已取消（用于优雅退出）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
