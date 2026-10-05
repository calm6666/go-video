// handler.go 决定一条投递的处理结论，以及队列能不能推进位点。
//
// 三条判定线（缺一不可，都会改变故障表现）：
//  1. 本包能判定的非法（类型、版本、缺 room_id、未知流状态、seq<=0、标识超长）不送进 logic：
//     logic 的第一次写就是占用 event_id 去重键（reportstreamstatelogic.go:67），
//     一旦占用，同 event_id 的后续投递永远只能得到 result=2，投影再也不会前进；
//  2. logic 返回的 result 1..5 全是终态，位点可以前进；重复/乱序/非法迁移按各自级别记日志；
//  3. 依赖故障（MySQL/Redis 抖动、读失败）与不认识的 result 不能提交位点，
//     但必须在本进程内封顶尝试次数：本服务没有持久化消费位点表，
//     不封顶就会让一条注定失败的事件在分区里无限热循环，把后面的事件全堵住。
package consumer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/live-room/internal/config"
	"go-video/services/live-room/rpc"
)

// defaultMaxTrackedEvents 是重试台账的容量兜底。
// 台账只用于封顶进程内尝试次数，容量打满时随机逐出一条：
// 被逐出的事件会重新获得尝试额度，代价是多做几次无用功，
// 正确性仍由 live_room_idempotency 的 event_id 唯一键保证（不会重复推进状态）。
const defaultMaxTrackedEvents = 4096

// ErrNilReply logic 给出 (nil, nil)：没有结果也没有错误，无法判定，按可重试处理。
var ErrNilReply = errors.New("live-room/consumer: ReportStreamState 返回空应答且没有错误")

// Applicator 是本消费者需要的唯一业务能力，由 logic.ReportStreamStateLogic 满足（见 wiring.go）。
// 收敛成一个接口，是为了让翻译、重试与结果分类能在无 MySQL/Redis 的环境里做确定性单测。
type Applicator interface {
	ReportStreamState(ctx context.Context, in *rpc.ReportStreamStateReq) (*rpc.ReportStreamStateReply, error)
}

// ApplicatorFunc 让闭包满足 Applicator。
type ApplicatorFunc func(ctx context.Context, in *rpc.ReportStreamStateReq) (*rpc.ReportStreamStateReply, error)

// ReportStreamState 实现 Applicator。
func (f ApplicatorFunc) ReportStreamState(ctx context.Context, in *rpc.ReportStreamStateReq) (*rpc.ReportStreamStateReply, error) {
	return f(ctx, in)
}

// Options 消费参数。
type Options struct {
	// MaxAttempts 同一 event_id 在本进程内的累计尝试上限（含首次），达到即放弃并确认位点。
	MaxAttempts int
	// MaxTrackedEvents 重试台账容量，纯内存上限，防止长时间故障让 map 无界增长。
	MaxTrackedEvents int
}

// OptionsFrom 由服务配置推导消费参数；缺省值集中在 normalize，避免两处漂移。
func OptionsFrom(k config.KafkaConf) Options {
	o := Options{MaxAttempts: k.MaxRetries}
	o.normalize()
	return o
}

func (o *Options) normalize() {
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 5
	}
	if o.MaxTrackedEvents <= 0 {
		o.MaxTrackedEvents = defaultMaxTrackedEvents
	}
}

// Stats 处理结论计数，供启动日志、单测断言与运维排查使用。
type Stats struct {
	Applied    int64
	Duplicate  int64
	Stale      int64
	Illegal    int64
	Mismatch   int64
	Skipped    int64
	Retried    int64
	GivenUp    int64
	Unexpected int64
	// LastError 最近一次失败原因（含被跳过的非法信封原因），用于「有事件被丢掉」的可查证性。
	LastError string
}

// Handler 单 topic 的事件处理器，直接满足 kq.ConsumeHandler。
type Handler struct {
	app  Applicator
	opts Options

	mu       sync.Mutex
	attempts map[string]int
	stats    Stats
}

// NewHandler 构造处理器。app 为 nil 时返回的处理器会把每条事件判成永久失败，
// 生产构造必须经 NewSupervisor（它对 nil 直接报错）。
func NewHandler(app Applicator, opts Options) *Handler {
	opts.normalize()
	return &Handler{app: app, opts: opts, attempts: make(map[string]int)}
}

// Options 返回当前参数，供启动日志与单测断言。
func (h *Handler) Options() Options { return h.opts }

// Stats 返回计数的快照。
func (h *Handler) Stats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stats
}

// trackedAttempts 返回台账里当前的尝试次数（测试与排查用）。
func (h *Handler) trackedAttempts(eventID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.attempts[eventID]
}

// Consume 处理一条队列投递。返回 nil 表示可以提交位点。
//
// 关于「重投能不能救回投影」的诚实边界：logic 在第一次读之前就占用了 event_id 去重键
// （见 logic 侧的 TestReportStreamStateEventKeyIsBurnedBeforeReads），所以重投只对
// 「抢键之前」就失败的投递有意义（去重表写入失败、连接不可用）。
// 抢键之后的读/事务失败会把键留下，重投只会得到 result=2 重复，投影停在失败前那一刻，
// 此时唯一正确的处置是照实打出 given_up 错误日志并由运维用新 event_id 重放
// （ReportStreamState RPC 是入站口子），本包不伪造「重试已成功」。
func (h *Handler) Consume(ctx context.Context, _, value string) error {
	if strings.TrimSpace(value) == "" {
		// 空投递没有 event_id 可记账，也没有内容可追溯，确认掉即可（写死信会被空消息刷满）。
		h.record(OutcomeSkipped, nil)
		return nil
	}

	env, err := ParseEnvelope([]byte(value))
	if err != nil {
		return h.skipPermanent(ctx, "", "信封解析失败", err)
	}
	req, err := Translate(env)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnsupportedEventType), errors.Is(err, ErrUnsupportedSchemaVersion):
			// 订阅面比消费能力宽（整 topic 订阅、上游升版本）时跳过而不是报错：
			// 不是本服务能翻译的事件，重投一万次也不会变好。
			logx.WithContext(ctx).Infof("live-room/consumer: 事件不属于本服务的消费契约，跳过 event_id=%s err=%v",
				env.EventID, err)
			h.record(OutcomeSkipped, err)
			return nil
		default:
			// 类型和版本都对，却缺了房间或流状态：上游违反了 live.state.v1 契约，
			// 必须写错误日志留痕（静默跳过会让「房间投影不前进」查不出原因）。
			return h.skipPermanent(ctx, env.EventID, "事件无法翻译成 RPC 入参", err)
		}
	}

	attempts := h.enter(env.EventID)
	reply, cause := h.app.ReportStreamState(ctx, req)
	if cause == nil && reply == nil {
		cause = ErrNilReply
	}
	outcome := OutcomeUnexpected
	if cause == nil {
		outcome = Classify(reply.GetResult())
	}

	if cause != nil || outcome == OutcomeUnexpected {
		if cause == nil {
			cause = fmt.Errorf("live-room/consumer: logic 返回了本包不认识的 result=%d", reply.GetResult())
		}
		if attempts >= h.opts.MaxAttempts {
			h.forget(env.EventID)
			h.record(OutcomeGivenUp, cause)
			logx.WithContext(ctx).Errorf("live-room/consumer: 事件放弃重投 event_id=%s room_id=%d seq=%d attempts=%d err=%v；"+
				"位点将被提交，房间投影停在失败前，需要运维用新 event_id 走 ReportStreamState 重放",
				env.EventID, req.GetRoomId(), req.GetStreamSeq(), attempts, cause)
			return nil
		}
		h.record(OutcomeRetry, cause)
		logx.WithContext(ctx).Errorf("live-room/consumer: 处理失败等待重投 event_id=%s room_id=%d seq=%d attempts=%d/%d err=%v",
			env.EventID, req.GetRoomId(), req.GetStreamSeq(), attempts, h.opts.MaxAttempts, cause)
		return cause
	}

	h.forget(env.EventID)
	h.record(outcome, nil)
	switch outcome {
	case OutcomeApplied:
		logx.WithContext(ctx).Infof("live-room/consumer: 投影已推进 event_id=%s room_id=%d seq=%d state=%d",
			env.EventID, req.GetRoomId(), req.GetStreamSeq(), req.GetStreamState())
	case OutcomeMismatch, OutcomeIllegal:
		// 这两个终态说明 ingest 与房间侧对不上（房间没有进行中场次、引用不一致、迁移非法）。
		// 重投不可能自愈，但必须能让值班看到：按错误级留痕，位点照常前进。
		logx.WithContext(ctx).Errorf("live-room/consumer: 事件被判定为 %s，未推进投影 event_id=%s room_id=%d seq=%d msg=%s",
			outcome, env.EventID, req.GetRoomId(), req.GetStreamSeq(), reply.GetMessage())
	default:
		logx.WithContext(ctx).Infof("live-room/consumer: 事件按 %s 收敛 event_id=%s room_id=%d msg=%s",
			outcome, env.EventID, req.GetRoomId(), reply.GetMessage())
	}
	return nil
}

// skipPermanent 记录并确认一条永久失败投递。
func (h *Handler) skipPermanent(ctx context.Context, eventID, action string, cause error) error {
	h.record(OutcomeSkipped, cause)
	logx.WithContext(ctx).Errorf("live-room/consumer: %s，事件被丢弃且不推进任何投影 event_id=%q err=%v",
		action, eventID, cause)
	return nil
}

// enter 记一次尝试并返回「这是第几次」。最后一次不落台账：马上要 forget。
func (h *Handler) enter(eventID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.attempts[eventID] + 1
	if n < h.opts.MaxAttempts {
		if _, known := h.attempts[eventID]; !known && len(h.attempts) >= h.opts.MaxTrackedEvents {
			h.evictLocked()
		}
		h.attempts[eventID] = n
	}
	return n
}

// forget 事件收敛后释放台账额度，避免长时间运行后 map 里堆满已完成的事件。
func (h *Handler) forget(eventID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.attempts, eventID)
}

// evictLocked 容量打满时随机逐出一条（Go 的 map 迭代顺序本身就是随机的）。
func (h *Handler) evictLocked() {
	for k := range h.attempts {
		delete(h.attempts, k)
		return
	}
}

// record 累计计数。cause 为最近一次失败原因，成功的分支不覆盖它。
func (h *Handler) record(outcome Outcome, cause error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch outcome {
	case OutcomeApplied:
		h.stats.Applied++
	case OutcomeDuplicate:
		h.stats.Duplicate++
	case OutcomeStale:
		h.stats.Stale++
	case OutcomeIllegal:
		h.stats.Illegal++
	case OutcomeMismatch:
		h.stats.Mismatch++
	case OutcomeSkipped:
		h.stats.Skipped++
	case OutcomeRetry:
		h.stats.Retried++
	case OutcomeGivenUp:
		h.stats.GivenUp++
	case OutcomeUnexpected:
		h.stats.Unexpected++
	}
	if cause != nil {
		h.stats.LastError = cause.Error()
	}
}
