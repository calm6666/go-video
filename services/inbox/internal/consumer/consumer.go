// consumer.go 实现事件消费主流程：解析信封 -> 按 event_id 领取处理权 ->
// 构造站内信 -> 事务投递 -> 回写 succeeded / retry / dead_letter。
//
// 幂等与「至少一次」的组合方式（AGENTS.md §5、docs/api-and-events.md §6）：
//   - inbox_consumer_offset 以 event_id 唯一键做状态机，重复/乱序/迟到消息只能
//     读到既有状态，不会把 succeeded 改回 processing；
//   - inbox_message.idempotency_key = evt:<event_id> 是第二层去重，即使状态表被清空
//     也不会重复投递站内信；
//   - 处理失败时在状态表写入退避到期时间并把原始信封暂存在 payload 列，
//     由本包的清扫循环（retry.go）重投，不依赖 Kafka 是否重投同一条消息。
package consumer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/common/eventenvelope"
	"go-video/services/inbox/internal/config"
	"go-video/services/inbox/internal/repository"
	"go-video/services/inbox/model"
)

// maxStoredPayload 是暂存到 inbox_consumer_offset.payload 的字节上限。
// 该列是 MySQL TEXT（65535 字节），超限只留摘要不留原文，
// 否则退避写会因 "Data too long" 失败并让消息反复重投。
const maxStoredPayload = 60000

// maxEventIDLen 对齐 inbox_consumer_offset.event_id VARCHAR(64)（deploy/migrations/inbox/000002）。
const maxEventIDLen = 64

// Store 是消费流程需要的持久化能力，由 *repository.Repository 实现。
// 收敛成接口是为了让状态机、退避与死信可以用假实现做确定性单测（错误必须真实返回）。
type Store interface {
	// ClaimEvent 按 event_id 领取处理权。
	ClaimEvent(ctx context.Context, ev *model.ConsumerOffset, staleSeconds int64) (model.ClaimOutcome, *model.ConsumerOffset, error)
	// MarkEventSucceeded 标记事件完成。
	MarkEventSucceeded(ctx context.Context, eventID string) error
	// MarkEventRetry 记录失败并写入退避到期时间与可重投的原始 payload。
	MarkEventRetry(ctx context.Context, eventID string, nextRetryAt int64, reason, payload string) error
	// MarkEventDeadLetter 标记事件判死。
	MarkEventDeadLetter(ctx context.Context, eventID, reason, payload string) error
	// SaveDeadLetter 死信留档（按 topic+摘要幂等）。
	SaveDeadLetter(ctx context.Context, dl *model.DeadLetter) (bool, error)
	// DueRetryEvents 取退避到期的事件。
	DueRetryEvents(ctx context.Context, now int64, limit int32) ([]*model.ConsumerOffset, error)
	// Deliver 事务内投递站内信。
	Deliver(ctx context.Context, msg *model.InboxMessage, mids []int64) (*repository.DeliverResult, error)
}

// Options 退避与死信参数。
type Options struct {
	// MaxAttempts 单个事件的最大处理次数（含首次），超过即转死信。
	MaxAttempts int32
	// BaseBackoff 退避基数：第 n 次失败后等待 BaseBackoff * 2^(n-1)。
	BaseBackoff time.Duration
	// MaxBackoff 退避上限。
	MaxBackoff time.Duration
	// StaleProcessing 处于 processing 超过该时长的事件视为进程崩溃遗留，可重新领取。
	StaleProcessing time.Duration
	// RetryBatchLimit 单轮清扫的重投条数。
	RetryBatchLimit int32
	// SweepIdleWait 无到期事件时的轮询间隔。
	SweepIdleWait time.Duration
}

// OptionsFrom 把服务配置映射成消费参数，缺省值集中在这里，避免两处漂移。
func OptionsFrom(k config.KafkaConf) Options {
	base := time.Duration(orDefault(k.RetryBaseSeconds, 10)) * time.Second
	max := time.Duration(orDefault(k.RetryMaxSeconds, 1800)) * time.Second
	if max < base {
		max = base
	}
	return Options{
		MaxAttempts:     k.MaxAttempts,
		BaseBackoff:     base,
		MaxBackoff:      max,
		StaleProcessing: time.Duration(orDefault(k.StaleProcessingSeconds, 300)) * time.Second,
		RetryBatchLimit: 100,
		SweepIdleWait:   10 * time.Second,
	}
}

func orDefault(v, def int64) int64 {
	if v <= 0 {
		return def
	}
	return v
}

// normalize 兜住直接构造 Options 的调用方（含单测）。
func (o *Options) normalize() {
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 8
	}
	if o.BaseBackoff <= 0 {
		o.BaseBackoff = 10 * time.Second
	}
	if o.MaxBackoff < o.BaseBackoff {
		o.MaxBackoff = o.BaseBackoff
	}
	if o.StaleProcessing <= 0 {
		o.StaleProcessing = 300 * time.Second
	}
	if o.RetryBatchLimit <= 0 {
		o.RetryBatchLimit = 100
	}
	if o.SweepIdleWait <= 0 {
		o.SweepIdleWait = 10 * time.Second
	}
}

// Processor 事件处理器。
type Processor struct {
	store Store
	opts  Options
	// now 便于单测固定时间；生产用 time.Now。
	now func() time.Time
}

// NewProcessor 构造 Processor。
func NewProcessor(store Store, opts Options) *Processor {
	opts.normalize()
	return &Processor{store: store, opts: opts, now: time.Now}
}

// Options 暴露当前参数，供启动日志与单测断言。
func (p *Processor) Options() Options { return p.opts }

// permanentCodes 是「重试也不会变好」的错误集合：契约、长度、分类问题。
// 命中即直接判死，不消耗重试配额。
var permanentCodes = []error{
	model.ErrEmptyRecipients,
	model.ErrTooManyRecipients,
	model.ErrEmptyContent,
	model.ErrInvalidCategory,
	model.ErrInvalidIdempotencyKey,
	model.ErrEventIDEmpty,
	ErrEmptyPayload,
	ErrNilEvent,
	ErrUnsupportedEventType,
}

// Process 处理一条队列投递。返回值语义（配合 KafkaConf.ForceCommit=false）：
//   - nil：可以提交位点（成功、按契约跳过、重复事件、已留档的死信）；
//   - error：不要提交位点，稍后重投（依赖不可用、退避窗口未到）。
func (p *Processor) Process(ctx context.Context, topic, value string) error {
	if strings.TrimSpace(value) == "" {
		// 空投递：没有可追溯内容，确认掉即可，不写死信（否则会被空消息刷满表）。
		return nil
	}

	env, err := ParseEnvelope([]byte(value))
	if err != nil {
		// 信封不合法：拿不到 event_id，只能按 (topic, 摘要) 留档，返回 nil 提交位点，
		// 避免同一条毒消息在分区里热循环；留档失败则返回错误让 Kafka 重投。
		return p.discardMalformed(ctx, topic, value, err)
	}

	if !SupportedEventType(env.EventType) {
		// 订阅面比消费能力宽（例如整 topic 订阅）时跳过而不是死信：
		// 不是本服务的事件不算故障，全刷死信会淹没真实问题。
		logx.WithContext(ctx).Infof("inbox/consumer: 忽略不支持的事件类型 %s event_id=%s",
			env.EventType, env.EventID)
		return nil
	}

	st := derivedTopic(env)
	if topic != "" && st != "" && topic != st {
		// topic 与信封版本推导不一致只告警：event_id 全局唯一，按 event_type 路由
		// 比按 topic 字符串路由更稳，不会因为生产者换 topic 命名就丢消息。
		logx.WithContext(ctx).Errorf("inbox/consumer: topic 与信封不一致投递 msg_topic=%s event_topic=%s event_id=%s",
			topic, st, env.EventID)
	}
	if topic == "" {
		topic = st
	}

	if len(env.EventID) > maxEventIDLen {
		// event_id 要落 inbox_consumer_offset.event_id VARCHAR(64)：超长的信封根本拿不到处理权，
		// 只会让 Claim 反复报 "Data too long" 空转到重试上限。按毒消息直接留档判死，不占退避配额。
		return p.discardMalformed(ctx, topic, value,
			fmt.Errorf("event_id 长度 %d 超过 %d 字节上限", len(env.EventID), maxEventIDLen))
	}

	outcome, current, err := p.Claim(ctx, env, topic, value)
	if err != nil {
		if errors.Is(err, repository.ErrEventDeferred) {
			logx.WithContext(ctx).Infof("inbox/consumer: 事件在退避窗口内，等待重投 event_id=%s", env.EventID)
		} else {
			logx.WithContext(ctx).Errorf("inbox/consumer: 领取事件失败 event_id=%s err=%v", env.EventID, err)
		}
		return err
	}
	if outcome == model.ClaimDuplicate {
		logx.WithContext(ctx).Infof("inbox/consumer: 重复事件已跳过 event_id=%s state=%s", env.EventID, current.State)
		return nil
	}

	attempts := attemptOf(current)
	return p.finish(ctx, env, topic, value, attempts, p.apply(ctx, env))
}

// Claim 按 event_id 领取处理权。单独成方法，供清扫循环复用同一套参数。
// raw 是本次投递的原文：首次插入时随状态一起暂存，进程崩溃后仍可重放。
func (p *Processor) Claim(
	ctx context.Context, env *eventenvelope.Envelope, topic, raw string,
) (model.ClaimOutcome, *model.ConsumerOffset, error) {
	return p.store.ClaimEvent(ctx, &model.ConsumerOffset{
		EventID:    env.EventID,
		EventType:  env.EventType,
		Topic:      topic,
		State:      model.ConsumerStateProcessing,
		Payload:    storedPayload(raw),
		OccurredAt: occurredAtSeconds(env.OccurredAt),
	}, int64(p.opts.StaleProcessing.Seconds()))
}

// attemptOf 从状态行推导「这是第几次处理」。首次领取时 retry_count=0。
func attemptOf(current *model.ConsumerOffset) int32 {
	if current == nil || current.RetryCount <= 0 {
		return 1
	}
	if current.RetryCount >= 1<<30 {
		return current.RetryCount
	}
	return current.RetryCount + 1
}

// finish 按 apply 的结果推进状态机，并决定 Kafka 是否提交位点。
func (p *Processor) finish(
	ctx context.Context, env *eventenvelope.Envelope, topic, raw string, attempts int32, cause error,
) error {
	switch {
	case cause == nil:
		if err := p.store.MarkEventSucceeded(ctx, env.EventID); err != nil {
			// 站内信已投递成功，只是状态没落库：不提交位点，重投时 Deliver 的幂等键兜住。
			logx.WithContext(ctx).Errorf("inbox/consumer: 标记 succeeded 失败 event_id=%s err=%v", env.EventID, err)
			return err
		}
		return nil

	case isPermanent(cause):
		return p.deadLetter(ctx, env, topic, raw, attempts, cause)

	default:
		if attempts >= p.opts.MaxAttempts {
			return p.deadLetter(ctx, env, topic, raw, attempts, cause)
		}
		next := p.now().Add(BackoffDelay(attempts, p.opts.BaseBackoff, p.opts.MaxBackoff)).Unix()
		if err := p.store.MarkEventRetry(ctx, env.EventID, next,
			fmt.Sprintf("第 %d 次处理失败: %v", attempts, cause), storedPayload(raw)); err != nil {
			logx.WithContext(ctx).Errorf("inbox/consumer: 标记 retry 失败 event_id=%s err=%v", env.EventID, err)
			return cause
		}
		logx.WithContext(ctx).Errorf("inbox/consumer: 事件转入退避 event_id=%s attempts=%d next_retry_at=%d err=%v",
			env.EventID, attempts, next, cause)
		// 返回错误：本条位点不提交，同时 DB 里的退避窗口保证提前重投也不会重复执行。
		return cause
	}
}

// apply 构造并投递站内信。返回的错误已区分「永久」与「可重试」。
func (p *Processor) apply(ctx context.Context, env *eventenvelope.Envelope) error {
	res, err := BuildMessage(env)
	if err != nil {
		return permanent(err)
	}
	if res.Skip {
		logx.WithContext(ctx).Infof("inbox/consumer: 事件按契约跳过 event_id=%s type=%s reason=%s",
			env.EventID, env.EventType, res.Reason)
		return nil
	}

	out, err := p.store.Deliver(ctx, res.Message, res.Recipients)
	if err != nil {
		if IsPermanentCode(err) {
			return permanent(err)
		}
		return err // MySQL/Redis 抖动交给退避
	}
	logx.WithContext(ctx).Infof("inbox/consumer: 站内信投递完成 event_id=%s type=%s msg_id=%d delivered=%d dedup=%v",
		env.EventID, env.EventType, out.MsgID, out.Delivered, out.Deduplicated)
	return nil
}

// deadLetter 先留档再改状态：
// 留档失败返回错误 -> 不提交位点 -> 重投后仍会尝试留档，绝不静默丢消息；
// 反之若先改状态再留档失败，重投会被判成重复，审计记录就永久丢了。
func (p *Processor) deadLetter(
	ctx context.Context, env *eventenvelope.Envelope, topic, raw string, attempts int32, cause error,
) error {
	reason := fmt.Sprintf("第 %d 次处理判死: %v", attempts, cause)
	dl := &model.DeadLetter{
		EventID:        env.EventID,
		EventType:      env.EventType,
		Topic:          topic,
		PayloadDigest:  model.DigestPayload([]byte(raw)),
		PayloadPreview: model.RedactPreview([]byte(raw)),
		Reason:         reason,
		ConsumedAt:     p.now().Unix(),
		State:          model.DeadLetterStateOpen,
	}
	if _, err := p.store.SaveDeadLetter(ctx, dl); err != nil {
		logx.WithContext(ctx).Errorf("inbox/consumer: 死信留档失败 event_id=%s err=%v", env.EventID, err)
		return err
	}
	if err := p.store.MarkEventDeadLetter(ctx, env.EventID, reason, storedPayload(raw)); err != nil {
		logx.WithContext(ctx).Errorf("inbox/consumer: 标记 dead_letter 失败 event_id=%s err=%v", env.EventID, err)
		return err
	}
	logx.WithContext(ctx).Errorf("inbox/consumer: 事件转入死信 event_id=%s type=%s attempts=%d err=%v",
		env.EventID, env.EventType, attempts, cause)
	// 已留档：提交位点，避免毒消息阻塞分区；后续重放由 inbox_dead_letter/inbox_consumer_offset 支撑。
	return nil
}

// discardMalformed 处理无法解析的信封：只留摘要与脱敏预览，不写 inbox_message。
func (p *Processor) discardMalformed(ctx context.Context, topic, raw string, cause error) error {
	dl := &model.DeadLetter{
		Topic:          topic,
		PayloadDigest:  model.DigestPayload([]byte(raw)),
		PayloadPreview: model.RedactPreview([]byte(raw)),
		Reason:         fmt.Sprintf("信封校验失败: %v", cause),
		ConsumedAt:     p.now().Unix(),
		State:          model.DeadLetterStateOpen,
	}
	if _, err := p.store.SaveDeadLetter(ctx, dl); err != nil {
		logx.WithContext(ctx).Errorf("inbox/consumer: 登记格式错误事件失败 topic=%s err=%v", topic, err)
		return err
	}
	logx.WithContext(ctx).Errorf("inbox/consumer: 格式错误事件已留档 topic=%s digest=%s err=%v",
		topic, dl.PayloadDigest, cause)
	return nil
}

// BackoffDelay 指数退避：base * 2^(attempts-1)，不超过 max。
// attempts 从 1 开始，因此首次失败也要等待 base。
func BackoffDelay(attempts int32, base, max time.Duration) time.Duration {
	if attempts <= 1 {
		return base
	}
	delay := base
	for i := int32(1); i < attempts; i++ {
		if delay >= max/2 {
			return max
		}
		delay *= 2
		if delay > max {
			return max
		}
	}
	return delay
}

// IsPermanentCode 判断错误是否属于「重试不会变好」的契约类错误。
// repository/model 用哨兵错误表达这些结论，因此按 errors.Is 判定而不是猜文案。
func IsPermanentCode(err error) bool {
	for _, code := range permanentCodes {
		if errors.Is(err, code) {
			return true
		}
	}
	return false
}

// permanent 把错误标记为永久失败。
func permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

// permanentError 只加一层标记，保留原错误信息可 unwrap。
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// isPermanent 判定错误是否已被标记为永久。
func isPermanent(err error) bool {
	var pe permanentError
	return errors.As(err, &pe)
}

// storedPayload 决定写入状态表的暂存原文：超长的只留摘要，避免 TEXT 列写入失败。
func storedPayload(raw string) string {
	if len(raw) > maxStoredPayload {
		return ""
	}
	return raw
}

// Handler 是绑定 topic 的消费处理器，直接满足 kq.ConsumeHandler。
type Handler struct {
	topic string
	p     *Processor
}

// NewHandler 构造 Handler。
func NewHandler(topic string, p *Processor) *Handler {
	return &Handler{topic: topic, p: p}
}

// Topic 返回绑定的 topic。
func (h *Handler) Topic() string { return h.topic }

// Consume 是消息队列回调入口。key 当前未使用（事件幂等只看 event_id）。
func (h *Handler) Consume(ctx context.Context, _, value string) error {
	return h.p.Process(ctx, h.topic, value)
}
