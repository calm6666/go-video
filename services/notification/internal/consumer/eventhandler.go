// eventhandler.go 实现 notification.request.v1 的消费流程：
// 解析信封 -> 按 event_id 去重并领取处理权 -> 转成 SendNotification 请求 ->
// 落投递任务 -> 回写 succeeded / retry / dead_letter。
//
// 幂等与「至少一次」的组合方式（docs/api-and-events.md §6）：
//   - notification_consumer_offset 以 event_id 为主键，重复/迟到消息只能读到既有状态，
//     不会把 succeeded 改回 processing；
//   - 原始信封暂存在 payload_json，重试由本包的退避扫描循环驱动，
//     不依赖 Kafka 是否再次投递同一条消息；
//   - 只有真正完成处理或已 durable 地写入 retry/dead_letter 才返回 nil（允许位点提交）；
//     状态写库失败时返回错误，让上游知道这条消息没有落定。
package consumer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/common/eventenvelope"
	"go-video/services/notification/internal/config"
	"go-video/services/notification/internal/policy"
	"go-video/services/notification/internal/repository"
	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

// maxEventPayloadBytes 暂存信封的字节上限：超过则只留摘要不留原文，
// 防止异常大的报文把状态表撑爆（MySQL TEXT 上限 65535）。
const maxEventPayloadBytes = 60000

// Sender 是事件处理需要的发送能力，由 internal/logic 的 SendNotification 适配实现。
// 抽象成接口是为了让消费状态机可以用 fake 单测，同时避免 consumer -> logic -> consumer 的包环。
type Sender interface {
	// Send 执行一次投递请求落库；返回错误代表本批未成功，需要按退避重试。
	Send(ctx context.Context, req *rpc.SendNotificationReq) (*rpc.SendNotificationReply, error)
}

// EventPolicy 是事件消费的退避参数。
type EventPolicy struct {
	// Topic 消费的 topic，写入 notification_consumer_offset.topic。
	Topic string
	// MaxRetries 事件级重试上限，超过转死信留档。
	MaxRetries int32
	// BackoffSeconds 退避阶梯（秒）。
	BackoffSeconds []int64
	// Batch 单次退避扫描的事件数。
	Batch int32
	// Interval 退避扫描间隔。
	Interval time.Duration
}

// NewEventPolicy 从服务配置派生事件消费策略。
func NewEventPolicy(c config.KafkaConf, n config.NotificationConf) EventPolicy {
	interval := time.Duration(n.DispatchIntervalMs) * time.Millisecond
	if interval <= 0 {
		interval = time.Second
	}
	batch := n.DispatchBatch
	if batch <= 0 {
		batch = 32
	}
	return EventPolicy{
		Topic:          c.RequestTopic,
		MaxRetries:     c.MaxEventRetries,
		BackoffSeconds: policy.NormalizeBackoff(n.BackoffSeconds),
		Batch:          batch,
		Interval:       interval,
	}
}

// EventHandler notification.request.v1 的消费者处理器。
type EventHandler struct {
	repo   *repository.Repository
	sender Sender
	policy EventPolicy
	now    func() time.Time

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	running bool
}

// NewEventHandler 构造事件处理器；repo 与 sender 必填（缺少就是接线错误，直接拒绝启动）。
func NewEventHandler(repo *repository.Repository, sender Sender, ep EventPolicy) (*EventHandler, error) {
	if repo == nil {
		return nil, errors.New("notification/consumer: repository is required")
	}
	if sender == nil {
		return nil, errors.New("notification/consumer: sender is required")
	}
	ep.BackoffSeconds = policy.NormalizeBackoff(ep.BackoffSeconds)
	if ep.Batch <= 0 {
		ep.Batch = 32
	}
	if ep.Interval <= 0 {
		ep.Interval = time.Second
	}
	return &EventHandler{repo: repo, sender: sender, policy: ep, now: time.Now}, nil
}

// Start 启动“退避到期事件”重投循环。
func (h *EventHandler) Start() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	h.running = true
	go func() {
		defer close(h.done)
		ticker := time.NewTicker(h.policy.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := h.ProcessDueRetries(ctx); err != nil {
					logx.Errorw("notification event retry sweep failed", logx.Field("error", err.Error()))
				}
			}
		}
	}()
	logx.Infow("notification event handler started", logx.Field("topic", h.policy.Topic))
	return nil
}

// Stop 停止重投循环。
func (h *EventHandler) Stop() {
	h.mu.Lock()
	cancel, done, running := h.cancel, h.done, h.running
	h.running = false
	h.cancel = nil
	h.mu.Unlock()
	if !running {
		return
	}
	cancel()
	<-done
	logx.Info("notification event handler stopped")
}

// Consume 处理一条队列消息，签名与 kq.ConsumeHandler 完全一致，
// 因此 *EventHandler 可直接交给 kq.NewQueue（见 kafkaruntime_kafka.go）。
func (h *EventHandler) Consume(ctx context.Context, _, value string) error {
	_, err := h.Handle(ctx, value)
	return err
}

// Handle 处理一条原始信封报文，返回 event_id（未知时为空）。
// 返回值语义见包注释：只有状态落定才返回 nil。
func (h *EventHandler) Handle(ctx context.Context, raw string) (string, error) {
	env, req, perr := policy.ParseRequestEnvelope([]byte(raw))
	eventID := ""
	if env != nil {
		eventID = env.EventID
	}
	if perr != nil {
		// 报文不合法：无法进入状态机，直接留档死信（不静默丢弃，也不无限重投）。
		return eventID, h.archiveUnparsable(ctx, eventID, raw, perr)
	}

	occurred := parseOccurredAt(env.OccurredAt)
	row := &model.NotificationConsumerOffset{
		EventId:     eventID,
		EventType:   env.EventType,
		Topic:       eventenvelope.Topic(env.EventType, env.SchemaVersion),
		State:       model.EventStateReceived,
		OccurredAt:  occurred,
		BodyDigest:  digestString(raw),
		PayloadJson: storePayload(raw),
		TraceId:     env.TraceID,
	}
	created, err := h.repo.InsertEventIfAbsent(ctx, row)
	if err != nil {
		return eventID, err
	}
	if !created {
		// 重复投递：读取既有状态，只有仍停在 received 的才继续，其余直接确认。
		existing, ferr := h.repo.FindEvent(ctx, eventID)
		if ferr != nil {
			return eventID, ferr
		}
		if existing == nil {
			return eventID, fmt.Errorf("notification/consumer: %w event=%s", model.ErrNotFound, eventID)
		}
		if existing.State != model.EventStateReceived {
			logx.Infow("notification event duplicate ignored",
				logx.Field("event_id", eventID), logx.Field("state", existing.State))
			return eventID, nil
		}
		row = existing
	}

	// 领取处理权：received/retry -> processing，未命中说明别的协程已在处理。
	ok, cerr := h.repo.MarkEventState(ctx, eventID, model.EventStateProcessing, row.RetryCount, 0, "",
		eventFromStates(model.EventStateProcessing))
	if cerr != nil {
		return eventID, cerr
	}
	if !ok {
		return eventID, nil
	}

	reply, serr := h.sender.Send(policy.WithEventID(ctx, eventID), req)
	if serr == nil {
		if ok, merr := h.repo.MarkEventState(ctx, eventID, model.EventStateSucceeded, row.RetryCount, 0, "",
			eventFromStates(model.EventStateSucceeded)); merr != nil {
			return eventID, merr
		} else if !ok {
			logx.Errorw("notification event succeeded mark lost", logx.Field("event_id", eventID))
		}
		suppressed := int32(0)
		if reply != nil {
			suppressed = reply.Suppressed
		}
		logx.Infow("notification event handled",
			logx.Field("event_id", eventID), logx.Field("suppressed", suppressed))
		return eventID, nil
	}
	return eventID, h.recordFailure(ctx, row, serr)
}

// recordFailure 记录一次事件处理失败：写 retry（带退避）或耗尽后写 dead_letter + 死信留档。
func (h *EventHandler) recordFailure(ctx context.Context, row *model.NotificationConsumerOffset, cause error) error {
	nextCount := row.RetryCount + 1
	reason := cause.Error()
	if policy.RetryExhausted(nextCount, h.policy.MaxRetries) {
		if _, err := h.repo.MarkEventState(ctx, row.EventId, model.EventStateDeadLetter, nextCount, 0, reason,
			eventFromStates(model.EventStateDeadLetter)); err != nil {
			return errors.Join(cause, err)
		}
		if _, err := h.repo.ArchiveDeadLetter(ctx, &model.NotificationDeadLetter{
			EventId:       row.EventId,
			EventType:     row.EventType,
			Topic:         h.topicOr(row.Topic),
			Source:        model.DeadLetterSourceEvent,
			PayloadDigest: row.BodyDigest,
			Reason:        reason,
			State:         model.DeadLetterStatePending,
		}); err != nil {
			return errors.Join(cause, err)
		}
		logx.Errorw("notification event dead lettered",
			logx.Field("event_id", row.EventId), logx.Field("error", reason))
		// 死信已 durable 落库：允许位点提交，后续由 RetryDeadLetter 依 payload_json 重放。
		return nil
	}
	next := policy.NextRetryAt(h.now(), nextCount, h.policy.BackoffSeconds)
	if _, err := h.repo.MarkEventState(ctx, row.EventId, model.EventStateRetry, nextCount, next, reason,
		eventFromStates(model.EventStateRetry)); err != nil {
		// 状态没写成功 -> 返回错误，让上游按“未落定”处理（至少一次语义）。
		return errors.Join(cause, err)
	}
	logx.Infow("notification event scheduled for retry",
		logx.Field("event_id", row.EventId), logx.Field("retry_count", nextCount))
	return nil
}

// ProcessDueRetries 重投到期的 retry 事件，返回处理条数。
// Kafka 位点由消费组管理、kq 不透传分区/位点给 handler，因此进程内退避靠这张表。
func (h *EventHandler) ProcessDueRetries(ctx context.Context) (int, error) {
	rows, err := h.repo.ListDueEvents(ctx, h.now().Unix(), h.policy.Batch)
	if err != nil {
		return 0, fmt.Errorf("notification/consumer: list due events: %w", err)
	}
	handled := 0
	for _, row := range rows {
		if row == nil {
			continue
		}
		if err := h.ReplayEvent(ctx, row.EventId); err != nil {
			logx.Errorw("notification event replay failed",
				logx.Field("event_id", row.EventId), logx.Field("error", err.Error()))
		}
		handled++
	}
	return handled, nil
}

// ReplayEvent 按 event_id 用暂存的 payload_json 重放一条事件（退避重试与死信重投共用）。
func (h *EventHandler) ReplayEvent(ctx context.Context, eventID string) error {
	row, err := h.repo.FindEvent(ctx, eventID)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("notification/consumer: %w event=%s", model.ErrNotFound, eventID)
	}
	if row.PayloadJson == "" {
		// 没有原文就无法重放：显式报错而不是假装成功，运营需回溯源事件。
		return fmt.Errorf("notification/consumer: %w: event=%s payload_json is empty",
			model.ErrNotFound, eventID)
	}
	if row.State == model.EventStateSucceeded {
		return nil
	}
	_, herr := h.Handle(ctx, row.PayloadJson)
	return herr
}

// archiveUnparsable 把无法解析的报文留档为死信。
// event_id 缺失时用报文摘要派生稳定键，保证同一条坏报文重复到达也只登记一次。
func (h *EventHandler) archiveUnparsable(ctx context.Context, eventID, raw string, cause error) error {
	key := eventID
	if key == "" {
		key = "unparsable-" + digestString(raw)[:32]
	}
	if _, err := h.repo.ArchiveDeadLetter(ctx, &model.NotificationDeadLetter{
		EventId:       key,
		EventType:     policy.EventTypeNotificationRequest,
		Topic:         h.topicOr(""),
		Source:        model.DeadLetterSourceEvent,
		PayloadDigest: digestString(raw),
		Reason:        cause.Error(),
		State:         model.DeadLetterStatePending,
	}); err != nil {
		return err
	}
	logx.Errorw("notification event unparsable, archived as dead letter",
		logx.Field("event_id", key), logx.Field("error", cause.Error()))
	return nil
}

func (h *EventHandler) topicOr(fallback string) string {
	if h.policy.Topic != "" {
		return h.policy.Topic
	}
	return fallback
}

// eventFromStates 返回事件状态机里允许迁移到 to 的源状态。
func eventFromStates(to int32) []int32 {
	return policy.EventSourceStates(to)
}

// parseOccurredAt 把信封里的 RFC3339 时间转成 Unix 秒；无法解析时返回 0（留档字段，不影响判定）。
func parseOccurredAt(v string) int64 {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(v)); err == nil {
		return t.Unix()
	}
	return 0
}

// digestString 返回报文的 sha256 hex（死信与状态表只存摘要，不留明文）。
func digestString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// storePayload 截断超长报文：只保留能放下的部分长度上限内的原文，超限留空只保摘要。
func storePayload(raw string) string {
	if len(raw) > maxEventPayloadBytes {
		return ""
	}
	return raw
}
