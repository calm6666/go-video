// handler.go 决定一条投递的处理结论，以及队列能不能推进位点。
//
// 三条判定线（缺一不可，都会改变故障表现）：
//  1. 本包能判定的非法（类型、版本、缺 room_id、未知流状态、Stopped 却没有 session_id、
//     标识超长）不送进 logic：判不了的契约违反重投一万次也不会变好，
//     但必须写错误日志留痕，否则「档位为什么还挂在线」查不出原因；
//  2. logic 的 Affected=0 是成功结论（这场已经没有在线档位：重复事件、迟到事件、
//     从没开过档位都会落在这里），位点照常前进；
//  3. 依赖故障（MySQL 抖动、事务失败）不能提交位点，但必须在本进程内封顶尝试次数：
//     本服务没有持久化消费位点表，不封顶就会让一条注定失败的事件在分区里无限热循环，
//     把后面的断流事件全堵住（后果是整房间的档位都迟迟不摘）。
package consumer

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/live-media/internal/config"
)

// defaultMaxTrackedEvents 是重试台账的容量兜底。
// 台账只用于封顶进程内尝试次数，容量打满时随机逐出一条：
// 被逐出的事件会重新获得尝试额度，代价是多做几次无用功，
// 正确性仍由 MarkOfflineTx 的 state=在线 CAS 条件保证（不会重复下线、不会补第二条事件）。
const defaultMaxTrackedEvents = 4096

// ErrNilResult 执行器给出 (nil, nil)：没有结果也没有错误，无法判定，按可重试处理。
var ErrNilResult = errors.New("livemedia/consumer: OfflineSessionOutputs 返回空结果且没有错误")

// Applicator 是本消费者需要的唯一业务能力，由 logic.OfflineSessionOutputsLogic 满足（见 wiring.go）。
// 收敛成一个接口、并用本包自己的命令/结果类型，是为了让翻译、重试与结果分类
// 能在无 MySQL/Redis 的环境里做确定性单测，也不把 logic 的入参结构变成消费侧的隐式契约。
type Applicator interface {
	OfflineSessionOutputs(ctx context.Context, cmd *OfflineCommand) (*OfflineResult, error)
}

// ApplicatorFunc 让闭包满足 Applicator。
type ApplicatorFunc func(ctx context.Context, cmd *OfflineCommand) (*OfflineResult, error)

// OfflineSessionOutputs 实现 Applicator。
func (f ApplicatorFunc) OfflineSessionOutputs(ctx context.Context, cmd *OfflineCommand) (*OfflineResult, error) {
	return f(ctx, cmd)
}

// OfflineResult 执行结论。Affected 是实际被这次调用下线的档位数。
type OfflineResult struct {
	Affected int32
	Scanned  int32
}

// Options 消费参数。
type Options struct {
	// MaxAttempts 同一 event_id 在本进程内的累计尝试上限（含首次），达到即放弃并确认位点。
	MaxAttempts int
	// MaxTrackedEvents 重试台账容量，纯内存上限，防止长时间故障让 map 无界增长。
	MaxTrackedEvents int
}

// OptionsFrom 由服务配置推导消费参数；缺省值集中在 normalize，避免两处漂移。
//
// MaxRetries 在本服务里同时被发布循环（判死上限）和消费循环（进程内尝试上限）使用：
// 一个键、两个环，运维调它就同时调两侧的容忍度，不会出现「发布器还在退避、消费者已放弃」。
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
	// Applied 断流事件已让档位下线（Affected>0）的次数，按档位整场计一次。
	Applied int64
	// Noop 事件已执行但无可下线档位（重复/迟到/该场次没开档位）。
	Noop    int64
	Skipped int64
	Retried int64
	GivenUp int64
	// LastError 最近一次失败原因（含被跳过的非法信封原因），用于「有事件被丢掉」的可查证性。
	LastError string
}

// Outcome 一条投递的处理结论。Commit 决定队列是否提交位点。
type Outcome int

const (
	// OutcomeApplied 档位已按事件整场下线（Affected>0）。
	OutcomeApplied Outcome = iota
	// OutcomeNoop 事件合法且已执行，但这场次没有可下线的在线档位（重复/迟到/未开档位）。
	OutcomeNoop
	// OutcomeSkipped 不是本服务的事件、或状态无需本服务动作、或契约非法：不执行，确认位点。
	OutcomeSkipped
	// OutcomeRetry 依赖故障且未达尝试上限：返回错误，不提交位点。
	OutcomeRetry
	// OutcomeGivenUp 依赖故障且已达尝试上限：放弃重投，写错误日志后确认位点。
	OutcomeGivenUp
)

// Commit 表示本条投递是否可以推进消费位点。
// 只有 OutcomeRetry 不提交（其余终态与永久错误都提交，避免毒消息阻塞分区）。
func (o Outcome) Commit() bool { return o != OutcomeRetry }

func (o Outcome) String() string {
	switch o {
	case OutcomeApplied:
		return "applied"
	case OutcomeNoop:
		return "noop"
	case OutcomeSkipped:
		return "skipped"
	case OutcomeRetry:
		return "retry"
	case OutcomeGivenUp:
		return "given_up"
	default:
		return "outcome_" + strconv.Itoa(int(o))
	}
}

// Handler 单 topic 的事件处理器，直接满足 kq.ConsumeHandler。
type Handler struct {
	app  Applicator
	opts Options

	mu       sync.Mutex
	attempts map[string]int
	stats    Stats
}

// NewHandler 构造处理器。app 必须是可用的执行器：
// 这里不做 nil 检查，nil app 在第一条需要下线的事件上会直接在调用处 panic。
// 生产装配必须经 NewSupervisor（它对 nil applicator 直接报错），
// 本函数只给单测与包内装配用。
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
// 关于「重投能不能救回下线」的诚实边界：本服务没有事件占用表，
// 所以任何一次「事务失败」都不留痕迹，重投必然重新扫一遍在线档位，这正是需要的语义
// （上一次失败时什么都没提交）。唯一救不回来的是「达到尝试上限后跨过位点」，
// 此时在线档位会一直挂着，直到 live_stream_output.online_expire_at 到期被 MarkExpiredOffline
// 收掉（登记时不带有效期的档位则只能靠运营用 OfflineStreamOutput 逐档位摘），
// 日志里的 given_up 就是这条边界的唯一证据。
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
	cmd, decision, err := Interpret(env)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnsupportedEventType), errors.Is(err, ErrUnsupportedSchemaVersion):
			// 订阅面比消费能力宽（整 topic 订阅、上游升版本）时跳过而不是报错：
			// 不是本服务能翻译的事件，重投一万次也不会变好。
			logx.WithContext(ctx).Infof("livemedia/consumer: 事件不属于本服务的消费契约，跳过 event_id=%s err=%v",
				env.EventID, err)
			h.record(OutcomeSkipped, err)
			return nil
		default:
			// 类型和版本都对，却缺了房间、场次或状态非法：上游违反了 live.state.v1 契约，
			// 必须写错误日志留痕（静默跳过会让「断流后档位还在线」查不出原因）。
			return h.skipPermanent(ctx, env.EventID, "事件无法翻译成下线命令", err)
		}
	}
	if decision == DecisionNone {
		// 正常噪声：同一 topic 上还发着 Publishing/Interrupted/Idle，本服务对它们没有写入权。
		h.record(OutcomeSkipped, nil)
		logx.WithContext(ctx).Infof("livemedia/consumer: 事件状态 %s 不需要本服务动作，跳过 event_id=%s "+
			"room_id=%d session_id=%d seq=%d", stateLabel(cmd.Payload.StreamState), env.EventID,
			cmd.Payload.RoomID, cmd.Payload.SessionID, cmd.Payload.StreamSeq)
		return nil
	}
	offline := cmd.Command

	attempts := h.enter(offline.EventID)
	res, cause := h.app.OfflineSessionOutputs(ctx, offline)
	if cause == nil && res == nil {
		cause = ErrNilResult
	}
	if cause != nil {
		if attempts >= h.opts.MaxAttempts {
			h.forget(offline.EventID)
			h.record(OutcomeGivenUp, cause)
			logx.WithContext(ctx).Errorf("livemedia/consumer: 事件放弃重投 event_id=%s room_id=%d session_id=%d "+
				"attempts=%d err=%v；位点将被提交，本场档位停在当前在线态，"+
				"需要运营用 ListStreamOutputs + OfflineStreamOutput 逐档位下线",
				offline.EventID, offline.RoomID, offline.SessionID, attempts, cause)
			return nil
		}
		h.record(OutcomeRetry, cause)
		logx.WithContext(ctx).Errorf("livemedia/consumer: 处理失败等待重投 event_id=%s room_id=%d session_id=%d "+
			"attempts=%d/%d err=%v", offline.EventID, offline.RoomID, offline.SessionID, attempts,
			h.opts.MaxAttempts, cause)
		return cause
	}

	h.forget(offline.EventID)
	if res.Affected <= 0 {
		h.record(OutcomeNoop, nil)
		logx.WithContext(ctx).Infof("livemedia/consumer: 事件已收敛但无可下线档位 event_id=%s room_id=%d "+
			"session_id=%d scanned=%d（重复/迟到事件，或该场次从未登记在线档位）",
			offline.EventID, offline.RoomID, offline.SessionID, res.Scanned)
		return nil
	}
	h.record(OutcomeApplied, nil)
	logx.WithContext(ctx).Infof("livemedia/consumer: 断流已下线 %d 个档位 event_id=%s room_id=%d session_id=%d "+
		"scanned=%d producer_reason=%q", res.Affected, offline.EventID, offline.RoomID, offline.SessionID,
		res.Scanned, strings.TrimSpace(cmd.Payload.Reason))
	return nil
}

// skipPermanent 记录并确认一条永久失败投递。
func (h *Handler) skipPermanent(ctx context.Context, eventID, action string, cause error) error {
	h.record(OutcomeSkipped, cause)
	logx.WithContext(ctx).Errorf("livemedia/consumer: %s，事件被丢弃且不推进任何投影 event_id=%q err=%v",
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
	case OutcomeNoop:
		h.stats.Noop++
	case OutcomeSkipped:
		h.stats.Skipped++
	case OutcomeRetry:
		h.stats.Retried++
	case OutcomeGivenUp:
		h.stats.GivenUp++
	}
	if cause != nil {
		h.stats.LastError = cause.Error()
	}
}
