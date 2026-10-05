// Package outbox 是「业务事务内写好的 outbox 行 → 消息队列」的通用发布循环。
//
// 它只承担与业务无关的四件事：按 store 给的顺序逐条同步投递、失败按指数退避落库、
// 尝试耗尽或行本身不可发布则判死、库读写失败则中断本批并把错误冒泡给调用方。
// 「哪张表、哪些 topic、列怎么映射」全部留在各服务的 Store 适配里，因此本包不出现
// MySQL、表名、事件类型，也不出现任何队列客户端（对齐 AGENTS.md §3：common 只放
// 稳定基础库与跨服务契约工具，不放业务实体）。
//
// 决策表来自 services/live-ingest/internal/publisher：那是本仓库第一个、也是当前
// 唯一一个跑过完整单测的发布器。本包把它抽出来给其余会写事件 outbox 的服务复用，
// 免得五份各自演化的重试语义。live-ingest 本轮不迁移（它的 publisher 与 21 条用例
// 已经验证过，改动它是另一轮的事），登记的收口缺口见 common/outbox/README.md。
//
// 证据边界（AGENTS.md §9）：本仓库从未连接过任何 broker。这里能给的结论只到
// 「可编译 / 可静态检查 / 本包单测通过」；「事件真的送达」必须先在 Redpanda 上跑通
// 才算数。
package outbox

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

// defaultLabel 是 Options.Name 为空时的日志/错误前缀回落值。
const defaultLabel = "common/outbox"

// maxBackoffShift 夹住指数左移的位数：2^30 秒已远超任何合理上限，
// 再移就溢出成负数，而负的 next_retry_at 会被 ListPending 当成「立即到期」，
// 退避直接失效并且把队列打满重试。
const maxBackoffShift = 30

// Row 是发布器需要的最小行视图，由各服务的 Store 适配从本服务的 outbox 表映射而来。
//
// 只带发布决策用到的列。服务自己的对账口径（例如 live-ingest 的
// GetEventPublishCheckpoint 要读 stream_id/seq/occurred_at）由 logic 层直接查 model，
// 不经过本结构，因此这里不需要为某个服务开专有字段。
type Row struct {
	// ID 是 outbox 自增主键：发布顺序与重试状态推进都按它。
	ID int64
	// EventID 是幂等锚点。broker 重投或 MarkPublished 写失败导致的重复投递，
	// 由消费方按它去重（inbox 的 inbox_consumer_offset、live-room 的 idempotency 表）。
	EventID string
	// Topic 由行上的 event_type + schema_version 拼出，而不是从配置读字符串，
	// 这样改 schema 版本不会出现「列写 v2、topic 还写 v1」的漂移。
	Topic string
	// Key 是分区键，取聚合根 ID（stream_id/content_id/user_id 等）：
	// 同一聚合根的事件因此落在同一分区，顺序才有依据。
	Key string
	// Payload 是事件信封完整 JSON，原样投递，不做二次编码。
	Payload string
	// RetryCount 是已重试次数，退避指数由它推出来。
	RetryCount int32
	// Defect 非空表示这一行根本不该发出去（topic 不属于本服务、列与 payload 不同源等）。
	// 发布器直接判死，不占重试次数：重试不会让一行列错的 payload 变对。
	Defect string
}

// Store 是发布器的持久化能力，由各服务用自己的 model 实现（适配层做列映射与一致性反查）。
// 收敛成接口是为了让顺序、退避与判死这三件事能用假实现做确定性单测，不需要 DB 也不需要网络。
//
// 四个方法的语义约定（各服务适配必须遵守，否则本包的判死/退避结论不成立）：
//   - ListPending 只返回「待发布且 next_retry_at 已到」的行，并且必须按 id 升序；
//     发布器不重排，忘写 ORDER BY 的后果是同聚合根事件乱序，下游 seq 守卫会判成回退。
//   - MarkPublished 置为已发布并记录发布时间。
//   - MarkRetry 保持待发布，写入新的 retry_count 与 next_retry_at。
//   - MarkFailed 判死，之后只能由人工放行接口（各服务的 RetryFailedEvents 类 RPC）解冻。
type Store interface {
	ListPending(ctx context.Context, now int64, limit int32) ([]*Row, error)
	MarkPublished(ctx context.Context, id, publishedAt int64) error
	MarkRetry(ctx context.Context, id int64, retryCount int32, nextRetryAt int64, lastError string) error
	MarkFailed(ctx context.Context, id int64, lastError string) error
}

// Sender 是一次事件投递。真实实现必须放在各服务的 `*_kafka` 构建标签文件里，
// 默认构建则显式拒绝：让默认二进制带上生产者，会把「编译得过」读成「事件在发」。
type Sender interface {
	// Send 把一条信封同步投递到 topic。返回 nil 必须代表队列侧已受理，
	// 绝不能是「已进本地缓冲」：调用方据此决定 MarkPublished。
	Send(ctx context.Context, topic, key, payload string) error
	// Close 释放底层连接。
	Close() error
}

// Options 是发布循环参数。零值不放行，见 validate。
type Options struct {
	// Name 是日志与错误信息里的组件标签，建议写成 "<service>/publisher"，
	// 让运维一眼看出是哪份循环在报错；空串回落到 common/outbox。
	Name string
	// Interval 是轮询间隔。
	Interval time.Duration
	// Batch 是单轮取到的到期事件数上限。
	Batch int32
	// MaxAttempts 是单事件累计尝试上限（含首次），达到即判死。
	MaxAttempts int32
	// BaseBackoff 是退避基数：第 n 次失败后等待 BaseBackoff * 2^(n-1)。
	BaseBackoff time.Duration
	// MaxBackoff 是退避上限。
	MaxBackoff time.Duration
	// SendTimeout 是单条投递的上下文超时。
	SendTimeout time.Duration
}

// label 返回日志/错误前缀。
func (o Options) label() string {
	if strings.TrimSpace(o.Name) == "" {
		return defaultLabel
	}
	return o.Name
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
		return fmt.Errorf("%s: 发布参数非法: %s", o.label(), strings.Join(bad, "; "))
	}
	return nil
}

// Publisher 是一张 outbox 表的发布循环。
//
// 单实例单协程：一轮内按 store 给的顺序串行投递，因此「同聚合根的事件顺序」
// 由 outbox 的 id 顺序加分区键共同保证。
// 多副本会各自轮询同一张表：同一条事件可能被投两次，这不破坏正确性
// （消费方按 event_id 去重），但会白烧算力。本仓库的 outbox 表没有租约列，
// 因此这里不做跨实例抢占，代价与各服务的现状写在服务 README「已知缺口」。
type Publisher struct {
	store  Store
	sender Sender
	opts   Options
	// now 是时间注入点：退避与位点都按它算，单测用固定时钟做确定性断言。
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
		return nil, fmt.Errorf("%s: store is required", opts.label())
	}
	if sender == nil {
		return nil, fmt.Errorf("%s: sender is required", opts.label())
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
		return fmt.Errorf("%s: already started", p.opts.label())
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
	logx.Infof("%s: 已启动 interval=%s batch=%d max_attempts=%d base_backoff=%s max_backoff=%s send_timeout=%s",
		p.opts.label(), p.opts.Interval, p.opts.Batch, p.opts.MaxAttempts,
		p.opts.BaseBackoff, p.opts.MaxBackoff, p.opts.SendTimeout)
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
		logx.Errorf("%s: 关闭发送端出错 err=%v", p.opts.label(), err)
	}
	logx.Infof("%s: 已停止", p.opts.label())
}

func (p *Publisher) loop(ctx context.Context) {
	ticker := time.NewTicker(p.opts.Interval)
	defer ticker.Stop()
	// 启动即扫一次：进程重启后积压的事件要立刻得到处理，不能白等一个 Interval。
	for {
		if _, err := p.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			p.setLastBatchErr(err.Error())
			logx.Errorf("%s: 本轮发布失败 err=%v", p.opts.label(), err)
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
// 因为「行还停在待发布」这件事必须让上层看到；投递失败不返回错误，
// 它已经按退避或判死落进该行的状态里。
func (p *Publisher) RunOnce(ctx context.Context) (int, error) {
	now := p.now()
	rows, err := p.store.ListPending(ctx, now.Unix(), p.opts.Batch)
	if err != nil {
		return 0, fmt.Errorf("%s: 读取待发布事件: %w", p.opts.label(), err)
	}
	handled := 0
	for _, row := range rows {
		if row == nil {
			continue
		}
		if err := p.publish(ctx, row, now); err != nil {
			return handled, err
		}
		handled++
	}
	return handled, nil
}

// publish 处理一行。返回错误只表示「这一行的状态没能落库」，不表示投递失败。
func (p *Publisher) publish(ctx context.Context, row *Row, now time.Time) error {
	label := p.opts.label()
	// attempt 用 int64：列被写坏成 int32 上限时，int32 加一会把日志打成负数，
	// 而同一条日志的 reason 又说「2147483648 attempts」，两个数互相矛盾就没法查。
	fields := []logx.LogField{
		logx.Field("outbox_id", row.ID),
		logx.Field("event_id", row.EventID),
		logx.Field("topic", row.Topic),
		logx.Field("attempt", int64(row.RetryCount)+1),
	}
	if row.Defect != "" {
		// 行本身不可发布：判死并留原因，不发送、不占重试次数。
		reason := "unpublishable: " + row.Defect
		if err := p.store.MarkFailed(ctx, row.ID, reason); err != nil {
			return fmt.Errorf("%s: outbox id=%d 判死写库: %w", label, row.ID, err)
		}
		p.bump(&p.failed)
		logx.Errorw(label+": 事件不可发布，已判死", append(fields, logx.Field("reason", row.Defect))...)
		return nil
	}

	callCtx, cancel := context.WithTimeout(ctx, p.opts.SendTimeout)
	defer cancel()
	sendErr := p.sender.Send(callCtx, row.Topic, row.Key, row.Payload)
	if sendErr == nil {
		if err := p.store.MarkPublished(ctx, row.ID, now.Unix()); err != nil {
			return fmt.Errorf("%s: outbox id=%d 标记已发布: %w", label, row.ID, err)
		}
		p.bump(&p.published)
		logx.Infow(label+": 事件已投递", fields...)
		return nil
	}

	// 用 int64 累加：retry_count 是从库里读出来的列，被写坏成 int32 上限附近时
	// int32 加一会绕成负数，判死边界随之失效，这一行就永远退避、永远不出去。
	nextCount := int64(row.RetryCount) + 1
	if nextCount < 1 {
		// 列被写坏成负数时也要单调增长，否则永远凑不满 MaxAttempts。
		nextCount = 1
	}
	if nextCount >= int64(p.opts.MaxAttempts) {
		reason := fmt.Sprintf("retries exhausted after %d attempts: %v", nextCount, sendErr)
		if err := p.store.MarkFailed(ctx, row.ID, reason); err != nil {
			return fmt.Errorf("%s: outbox id=%d 判死写库: %w", label, row.ID, err)
		}
		p.bump(&p.failed)
		logx.Errorw(label+": 投递尝试耗尽，已判死", append(fields, logx.Field("reason", reason))...)
		return nil
	}
	// 走到这里 nextCount < MaxAttempts（int32），回落成 int32 不会截断。
	nextRetryAt := p.nextRetryAt(now, nextCount)
	if err := p.store.MarkRetry(ctx, row.ID, int32(nextCount), nextRetryAt, sendErr.Error()); err != nil {
		return fmt.Errorf("%s: outbox id=%d 记录重试: %w", label, row.ID, err)
	}
	p.bump(&p.retried)
	logx.Errorw(label+": 投递失败，已安排退避重试",
		append(fields, logx.Field("next_retry_at", nextRetryAt), logx.Field("error", sendErr.Error()))...)
	return nil
}

// nextRetryAt 是指数退避：第 n 次失败后等 BaseBackoff * 2^(n-1)，上限 MaxBackoff。
// n 用 int64 传入，见 publish 里的溢出说明。
func (p *Publisher) nextRetryAt(now time.Time, nextCount int64) int64 {
	shift := nextCount - 1
	if shift < 0 {
		shift = 0
	}
	if shift > maxBackoffShift {
		shift = maxBackoffShift
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
