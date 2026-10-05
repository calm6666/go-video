// Package publisher 把 live_ingest_outbox 里「已在业务事务内提交」的事件投递到消息队列。
//
// 为什么单独一个包（而不是塞进 internal/consumer）：本服务只产事件、不消费事件
// （live.state.v1 的消费方是 live-room / live-gateway / live-media / inbox），
// 因此这里没有 consumer 目录，发布器也不该伪装成消费者。
//
// 分层与 search-indexer 的消费侧完全对称：
//   - 本文件与 outbox_store.go 是纯状态机，一个 Kafka 客户端都不出现，
//     单测用假 Store / 假 Sender 驱动，不需要 broker 也不需要网络；
//   - 真实队列客户端只在 kafkaruntime_kafka.go（`-tags liveingest_kafka`）一个文件里，
//     默认构建改由 kafkaruntime_disabled.go 返回 ErrKafkaRuntimeNotBuilt。
//
// 证据边界（AGENTS.md §9）：本仓库从未连接过任何 broker。这里能给的结论只到
// 「可编译 / 可静态检查 / 本包单测通过」；「事件真的送达」必须先在 Redpanda 上跑通
// 才算数，口径见 services/live-ingest/README.md「已知缺口」。
package publisher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/threading"
)

// Record 是发布器需要的最小行视图，由 OutboxStore 从 live_ingest_outbox 行映射而来。
//
// 只带发布决策用到的列：状态机不读 stream_id/seq/occurred_at，那些列属于
// GetEventPublishCheckpoint 的对账口径（logic 层直接查 model）。
type Record struct {
	// ID outbox 自增主键，发布顺序与重试状态推进都按它。
	ID int64
	// EventID 幂等锚点：broker 重投或 MarkPublished 写失败导致的重复投递，
	// 由消费方按它去重（inbox 的 inbox_consumer_offset、live-room 的 idempotency 表）。
	EventID string
	// Topic 目标 topic（live.state.v1），由行的 event_type + schema_version 拼出。
	Topic string
	// Key 分区键，取聚合根 stream_id：同一场流的事件因此落在同一分区。
	Key string
	// Payload 事件信封完整 JSON，原样投递，不做二次编码。
	Payload string
	// RetryCount 已重试次数（退避指数依据）。
	RetryCount int32
	// Defect 非空表示这一行根本不该发出去（信封与列不一致、event_type 不属于本服务等）。
	// 发布器直接判死，不占重试次数：重试不会让一行列错的 payload 变对。
	Defect string
}

// Store 是发布器的持久化能力，由 OutboxStore（model.EventOutboxModel 适配）实现。
// 收敛成接口是为了让顺序、退避与判死这三件事能用假实现做确定性单测。
type Store interface {
	// ListPending 取到期可投递的行（state=待发布 且 next_retry_at 已到），按 id 升序。
	ListPending(ctx context.Context, now int64, limit int32) ([]*Record, error)
	// MarkPublished 标记已发布。
	MarkPublished(ctx context.Context, id, publishedAt int64) error
	// MarkRetry 记录失败并设置下次重试时间。
	MarkRetry(ctx context.Context, id int64, retryCount int32, nextRetryAt int64, lastError string) error
	// MarkFailed 判死（等 RetryFailedEvents RPC 人工放行）。
	MarkFailed(ctx context.Context, id int64, lastError string) error
}

// Sender 是一次事件投递，默认构建由 kafkaruntime_disabled.go 显式拒绝，
// 真实实现见 kafkaruntime_kafka.go（`-tags liveingest_kafka`）。
type Sender interface {
	// Send 把一条信封同步投递到 topic。返回 nil 必须代表队列侧已受理，
	// 绝不能是「已进本地缓冲」：调用方据此决定 MarkPublished。
	Send(ctx context.Context, topic, key, payload string) error
	// Close 释放底层连接。
	Close() error
}

// Options 发布循环参数。零值不放行，见 validate。
type Options struct {
	// Interval 轮询间隔。
	Interval time.Duration
	// Batch 单轮取到的到期事件数上限。
	Batch int32
	// MaxAttempts 单事件累计尝试上限（含首次），达到即判死。
	MaxAttempts int32
	// BaseBackoff 退避基数：第 n 次失败后等待 BaseBackoff * 2^(n-1)。
	BaseBackoff time.Duration
	// MaxBackoff 退避上限。
	MaxBackoff time.Duration
	// SendTimeout 单条投递的上下文超时。
	SendTimeout time.Duration
}

// validate 逐键点名报错：配置没写对时，运维要在日志里看到是哪个键，
// 而不是看到「事件安静地不出去」。
func (o Options) validate() error {
	var bad []string
	if o.Interval <= 0 {
		bad = append(bad, "Options.Interval 必须大于 0（来自 Kafka.PollIntervalSec）")
	}
	if o.Batch <= 0 {
		bad = append(bad, "Options.Batch 必须大于 0（来自 Kafka.BatchLimit）")
	}
	if o.MaxAttempts <= 0 {
		bad = append(bad, "Options.MaxAttempts 必须大于 0（来自 Kafka.MaxRetries）")
	}
	if o.BaseBackoff <= 0 {
		bad = append(bad, "Options.BaseBackoff 必须大于 0（来自 Kafka.RetryBackoffSec）")
	}
	if o.MaxBackoff < o.BaseBackoff {
		bad = append(bad, "Options.MaxBackoff 不得小于 BaseBackoff（来自 Kafka.RetryMaxBackoffSec）")
	}
	if o.SendTimeout <= 0 {
		bad = append(bad, "Options.SendTimeout 必须大于 0（来自 Kafka.SendTimeoutSec）")
	}
	if len(bad) > 0 {
		return fmt.Errorf("live-ingest/publisher: 发布参数非法: %s", strings.Join(bad, "; "))
	}
	return nil
}

// Publisher 是 live_ingest_outbox 的发布循环。
//
// 单实例单协程：一轮内按 id 升序串行投递，因此「同一流的事件顺序」
// 由 outbox 的 id 顺序 + 分区键（stream_id）共同保证。
// 多副本会各自轮询同一张表：同一条事件可能被投两次，这不破坏正确性
// （消费方按 event_id 去重），但会白烧算力。表里没有租约列，本仓库不做
// 跨实例抢占，细节与代价写在 README「已知缺口」。
type Publisher struct {
	store  Store
	sender Sender
	opts   Options
	// now 时间注入点：退避与位点都按它算，单测用固定时钟做确定性断言。
	now func() time.Time

	mu           sync.Mutex
	cancel       context.CancelFunc
	done         chan struct{}
	running      bool
	published    int64
	retried      int64
	failed       int64
	lastBatchErr string
}

// New 用已校验的参数构造发布器。任一依赖为 nil 直接失败，不返回「看起来能用」的空对象。
func New(store Store, sender Sender, opts Options) (*Publisher, error) {
	if store == nil {
		return nil, errors.New("live-ingest/publisher: store is required")
	}
	if sender == nil {
		return nil, errors.New("live-ingest/publisher: sender is required")
	}
	if err := opts.validate(); err != nil {
		return nil, err
	}
	return &Publisher{store: store, sender: sender, opts: opts, now: time.Now}, nil
}

// Options 返回生效参数（供运维接口与单测读取）。
func (p *Publisher) Options() Options { return p.opts }

// Stats 返回累计处理条数与最近一轮的扫描错误，供启动自检与排障对照。
// lastBatchErr 非空只说明「扫描这一步失败过」，不代表事件已丢：行仍是待发布状态。
func (p *Publisher) Stats() (published, retried, failed int64, lastBatchErr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.published, p.retried, p.failed, p.lastBatchErr
}

// Running 报告后台循环是否在跑。
func (p *Publisher) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// Start 启动后台轮询循环。重复启动直接报错：两套循环会把同一批事件各投一遍，
// 而消费侧的去重只能保证不重复生效，不能保证不浪费一倍算力与日志。
func (p *Publisher) Start() error {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return errors.New("live-ingest/publisher: already started")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	p.cancel = cancel
	p.done = done
	p.running = true
	p.mu.Unlock()

	// 闭包只引用局部 done：Stop 会把 p.done 字段置 nil（幂等需要），
	// 若在收尾时读字段就是 close(nil) → panic，循环也就永远等不到确认。
	threading.GoSafe(func() {
		defer close(done)
		p.loop(ctx)
	})
	logx.Infof("live-ingest/publisher: 已启动 interval=%s batch=%d max_attempts=%d base_backoff=%s max_backoff=%s send_timeout=%s",
		p.opts.Interval, p.opts.Batch, p.opts.MaxAttempts, p.opts.BaseBackoff, p.opts.MaxBackoff, p.opts.SendTimeout)
	return nil
}

// Stop 停止循环并关闭 Sender（幂等：未启动时什么都不做）。
// 必须等当前批次收尾再关连接，否则半截批次的写入结果未知，
// 而我们连「哪几行没落库」都说不清。
func (p *Publisher) Stop() {
	p.mu.Lock()
	cancel, done, running := p.cancel, p.done, p.running
	p.running = false
	p.cancel = nil
	p.done = nil
	p.mu.Unlock()
	if !running {
		return
	}
	cancel()
	<-done
	if err := p.sender.Close(); err != nil {
		logx.Errorf("live-ingest/publisher: 关闭发送端出错 err=%v", err)
	}
	logx.Info("live-ingest/publisher: 已停止")
}

func (p *Publisher) loop(ctx context.Context) {
	ticker := time.NewTicker(p.opts.Interval)
	defer ticker.Stop()
	// 启动即扫一次：进程重启后积压的事件要立刻得到处理，
	// 不能白等一个 Interval（推流状态事件对滞后敏感）。
	for {
		if _, err := p.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			p.setLastBatchErr(err.Error())
			logx.Errorf("live-ingest/publisher: 本轮发布失败 err=%v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Publisher) setLastBatchErr(msg string) {
	p.mu.Lock()
	p.lastBatchErr = msg
	p.mu.Unlock()
}

// RunOnce 扫描一批到期事件并逐条投递，返回处理条数。
//
// 错误传播口径：扫描失败（读库）与状态写库失败都会中断本批并返回错误，
// 因为「行还停在 pending」这件事必须让上层看到；投递失败不返回错误，
// 它已经按退避/判死落进该行的状态里。
func (p *Publisher) RunOnce(ctx context.Context) (int, error) {
	now := p.now()
	rows, err := p.store.ListPending(ctx, now.Unix(), p.opts.Batch)
	if err != nil {
		return 0, fmt.Errorf("live-ingest/publisher: 读取待发布事件: %w", err)
	}
	handled := 0
	for _, rec := range rows {
		if rec == nil {
			continue
		}
		if err := p.publish(ctx, rec, now); err != nil {
			return handled, err
		}
		handled++
	}
	return handled, nil
}

// publish 处理一行。返回错误只表示「这一行的状态没能落库」，不表示投递失败。
func (p *Publisher) publish(ctx context.Context, rec *Record, now time.Time) error {
	fields := []logx.LogField{
		logx.Field("outbox_id", rec.ID),
		logx.Field("event_id", rec.EventID),
		logx.Field("topic", rec.Topic),
		logx.Field("attempt", rec.RetryCount+1),
	}
	if rec.Defect != "" {
		// 行本身不可发布：判死并留原因，不发送、不占重试次数。
		reason := "unpublishable: " + rec.Defect
		if err := p.store.MarkFailed(ctx, rec.ID, reason); err != nil {
			return fmt.Errorf("live-ingest/publisher: outbox id=%d 判死写库: %w", rec.ID, err)
		}
		p.bump(&p.failed)
		logx.Errorw("live-ingest/publisher: 事件不可发布，已判死", append(fields, logx.Field("reason", rec.Defect))...)
		return nil
	}

	callCtx, cancel := context.WithTimeout(ctx, p.opts.SendTimeout)
	defer cancel()
	sendErr := p.sender.Send(callCtx, rec.Topic, rec.Key, rec.Payload)
	if sendErr == nil {
		if err := p.store.MarkPublished(ctx, rec.ID, now.Unix()); err != nil {
			return fmt.Errorf("live-ingest/publisher: outbox id=%d 标记已发布: %w", rec.ID, err)
		}
		p.bump(&p.published)
		logx.Infow("live-ingest/publisher: 事件已投递", fields...)
		return nil
	}

	nextCount := rec.RetryCount + 1
	if nextCount >= p.opts.MaxAttempts {
		reason := fmt.Sprintf("retries exhausted after %d attempts: %v", nextCount, sendErr)
		if err := p.store.MarkFailed(ctx, rec.ID, reason); err != nil {
			return fmt.Errorf("live-ingest/publisher: outbox id=%d 判死写库: %w", rec.ID, err)
		}
		p.bump(&p.failed)
		logx.Errorw("live-ingest/publisher: 投递尝试耗尽，已判死",
			append(fields, logx.Field("reason", reason))...)
		return nil
	}
	nextRetryAt := p.nextRetryAt(now, nextCount)
	if err := p.store.MarkRetry(ctx, rec.ID, nextCount, nextRetryAt, sendErr.Error()); err != nil {
		return fmt.Errorf("live-ingest/publisher: outbox id=%d 记录重试: %w", rec.ID, err)
	}
	p.bump(&p.retried)
	logx.Errorw("live-ingest/publisher: 投递失败，已安排退避重试",
		append(fields, logx.Field("next_retry_at", nextRetryAt), logx.Field("error", sendErr.Error()))...)
	return nil
}

// nextRetryAt 指数退避：第 n 次失败后等 BaseBackoff * 2^(n-1)，上限 MaxBackoff。
// 指数夹到 30：2^30 秒已远超任何合理上限，再左移会溢出成负数，
// 而负的 next_retry_at 会被 ListPending 当成「立即到期」，退避直接失效。
func (p *Publisher) nextRetryAt(now time.Time, nextCount int32) int64 {
	shift := nextCount - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 30 {
		shift = 30
	}
	delay := p.opts.BaseBackoff << time.Duration(shift)
	if delay <= 0 || delay > p.opts.MaxBackoff {
		delay = p.opts.MaxBackoff
	}
	return now.Add(delay).Unix()
}

func (p *Publisher) bump(counter *int64) {
	p.mu.Lock()
	*counter++
	p.mu.Unlock()
}
