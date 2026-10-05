// Package consumer 承载 notification 的两条后台执行链路：
//
//   - Dispatcher：从 notification_delivery 扫描到期任务，按锁定的模板版本重渲染、
//     校验免打扰、调用通道适配器、写回执，失败按退避阶梯重试，耗尽转死信；
//   - EventHandler：消费 notification.request.v1（docs/api-and-events.md §5），
//     以 event_id 去重，走完 received/processing/succeeded/retry/dead_letter 状态机。
//
// 本包不直接依赖 Kafka 客户端：go-queue 已是 go.mod 的直接 require，但默认构建刻意不链接 kq
// （没有 broker 可验证消费语义，不能让「编译得过」被读成「已在消费」），因此队列侧只暴露
// KafkaHandler/KafkaRuntime 两个接口（kafka.go），真正的 kq 接线放在
// kafkaruntime_kafka.go（`-tags notification_kafka`）里，默认构建会显式返回
// ErrKafkaRuntimeNotBuilt，绝不伪造“已在消费”。
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/notification/internal/config"
	"go-video/services/notification/internal/policy"
	"go-video/services/notification/internal/provider"
	"go-video/services/notification/internal/repository"
	"go-video/services/notification/model"
)

// DispatchPolicy 是调度器的运行参数（由 config.NotificationConf 派生）。
type DispatchPolicy struct {
	// BackoffSeconds 退避阶梯（秒）。
	BackoffSeconds []int64
	// MaxRetries 投递任务最大重试次数，超过转死信。
	MaxRetries int32
	// DndEnabled 是否在发送前二次校验免打扰时段。
	DndEnabled bool
	// DefaultTimezone 用户未设偏好时的时区（IANA 名）。
	DefaultTimezone string
	// Batch 单次扫描任务数。
	Batch int32
	// Interval 扫描间隔。
	Interval time.Duration
	// ClaimGrace 领取任务后预留的发送时间窗：窗口内其他实例不会重复取到该任务。
	ClaimGrace time.Duration
	// SendTimeout 单条投递的上下文超时。
	SendTimeout time.Duration
}

// NewDispatchPolicy 从服务配置生成调度策略，并清洗非法值。
func NewDispatchPolicy(c config.NotificationConf) DispatchPolicy {
	interval := time.Duration(c.DispatchIntervalMs) * time.Millisecond
	if interval <= 0 {
		interval = time.Second
	}
	return DispatchPolicy{
		BackoffSeconds:  policy.NormalizeBackoff(c.BackoffSeconds),
		MaxRetries:      c.MaxDeliveryRetries,
		DndEnabled:      c.DndEnabled,
		DefaultTimezone: c.DefaultTimezone,
		Batch:           c.DispatchBatch,
		Interval:        interval,
		ClaimGrace:      2 * time.Minute,
		SendTimeout:     30 * time.Second,
	}
}

// 免打扰时段内的“顺延”固定步长：不消耗重试次数，只推迟 next_retry_at。
const quietHoursHold = 5 * time.Minute

// 投递结果动作。
const (
	actionSent       = "sent"
	actionRetry      = "retry"
	actionDeadLetter = "dead_letter"
	actionSuppressed = "suppressed"
	actionHeld       = "held"
	actionSkipped    = "skipped"
)

// Dispatcher 是 notification_delivery 的投递调度器。
type Dispatcher struct {
	repo      *repository.Repository
	providers *provider.Registry
	contact   provider.ContactResolver
	policy    DispatchPolicy
	loc       *time.Location
	now       func() time.Time

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	running bool
}

// NewDispatcher 构造调度器。时区非法时返回错误：
// 服务启动阶段就要暴露配置问题，不能退化成 UTC 导致免打扰窗口整体偏移。
// 参数 dp 不得命名为 policy：会与策略包同名而遮蔽标识符。
func NewDispatcher(repo *repository.Repository, providers *provider.Registry, contact provider.ContactResolver,
	dp DispatchPolicy) (*Dispatcher, error) {
	if repo == nil {
		return nil, errors.New("notification/consumer: repository is required")
	}
	if providers == nil {
		providers = provider.NewRegistry()
	}
	if contact == nil {
		contact = provider.NewPassthroughResolver()
	}
	loc, err := policy.LoadLocation(dp.DefaultTimezone, "UTC")
	if err != nil {
		return nil, fmt.Errorf("notification/consumer: %w", err)
	}
	dp.BackoffSeconds = policy.NormalizeBackoff(dp.BackoffSeconds)
	if dp.Batch <= 0 {
		dp.Batch = 64
	}
	if dp.Interval <= 0 {
		dp.Interval = time.Second
	}
	if dp.ClaimGrace <= 0 {
		dp.ClaimGrace = 2 * time.Minute
	}
	if dp.SendTimeout <= 0 {
		dp.SendTimeout = 30 * time.Second
	}
	return &Dispatcher{repo: repo, providers: providers, contact: contact, policy: dp, loc: loc, now: time.Now}, nil
}

// Repository 暴露底层数据访问（供 logic 的同步发送路径复用）。
func (d *Dispatcher) Repository() *repository.Repository { return d.repo }

// NowFunc 暴露时间注入点，供单测替换。
func (d *Dispatcher) NowFunc() time.Time { return d.now() }

// Start 启动后台扫描循环；重复调用是幂等的。
func (d *Dispatcher) Start() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.done = make(chan struct{})
	d.running = true
	go func() {
		defer close(d.done)
		d.loop(ctx)
	}()
	logx.Infow("notification dispatcher started",
		logx.Field("interval", d.policy.Interval.String()), logx.Field("batch", d.policy.Batch))
	return nil
}

// Stop 停止后台循环并等待当前批次结束。
func (d *Dispatcher) Stop() {
	d.mu.Lock()
	cancel, done, running := d.cancel, d.done, d.running
	d.running = false
	d.cancel = nil
	d.mu.Unlock()
	if !running {
		return
	}
	cancel()
	<-done
	logx.Info("notification dispatcher stopped")
}

// Running 报告后台循环是否在跑。
func (d *Dispatcher) Running() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running
}

func (d *Dispatcher) loop(ctx context.Context) {
	ticker := time.NewTicker(d.policy.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := d.ProcessOnce(ctx); err != nil {
				logx.Errorw("notification dispatcher sweep failed", logx.Field("error", err.Error()))
			}
		}
	}
}

// ProcessOnce 扫描一批到期任务并逐条投递，返回实际处理条数。
// 扫描本身出错（DB 不可用）时返回错误，由上层记录；单条错误只影响该条状态。
func (d *Dispatcher) ProcessOnce(ctx context.Context) (int, error) {
	now := d.now()
	rows, err := d.repo.ListDueDeliveries(ctx, now.Unix(), d.policy.Batch)
	if err != nil {
		return 0, fmt.Errorf("notification/consumer: list due deliveries: %w", err)
	}
	handled := 0
	for _, row := range rows {
		if row == nil {
			continue
		}
		if err := d.Dispatch(ctx, row.DeliveryId); err != nil {
			logx.Errorw("notification dispatch failed",
				logx.Field("delivery_id", row.DeliveryId), logx.Field("error", err.Error()))
		}
		handled++
	}
	return handled, nil
}

// Dispatch 投递一条任务（按 delivery_id 重新读库），是同步发送与调度器共用的唯一入口。
// 返回错误只代表“本次处理过程出错”，任务状态已按状态机落库，调用方不应据此判定发送成功。
func (d *Dispatcher) Dispatch(ctx context.Context, deliveryID string) error {
	if deliveryID == "" {
		return errors.New("notification/consumer: empty delivery_id")
	}
	row, err := d.repo.FindDelivery(ctx, deliveryID)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("notification/consumer: %w delivery=%s", model.ErrNotFound, deliveryID)
	}
	if row.State != model.DeliveryStatePending && row.State != model.DeliveryStateRetry {
		// 终态或已被其他 worker 接管：直接跳过，绝不重复调用供应商。
		return nil
	}
	// 领取任务：把 next_retry_at 推到领取窗口之外，多实例下只有一个 worker 会真正发送。
	claimAt := d.now().Add(d.policy.ClaimGrace).Unix()
	ok, err := d.repo.MarkDeliveryRetry(ctx, row.DeliveryId, row.RetryCount, claimAt, "claimed by dispatcher",
		policy.DeliverySourceStates(model.DeliveryStateRetry))
	if err != nil {
		return err
	}
	if !ok {
		return nil // 并发下已被别的 worker 领取
	}
	callCtx, cancel := context.WithTimeout(ctx, d.policy.SendTimeout)
	defer cancel()
	action, detail, dErr := d.send(callCtx, row)
	fields := []logx.LogField{
		logx.Field("delivery_id", row.DeliveryId),
		logx.Field("action", action),
		logx.Field("channel", model.ChannelName(row.Channel)),
	}
	if dErr != nil {
		logx.Errorw("notification dispatch error", append(fields, logx.Field("error", dErr.Error()))...)
		return dErr
	}
	logx.Infow("notification dispatch outcome", append(fields, logx.Field("detail", detail))...)
	return nil
}

// send 执行一次投递判定并落库状态。
// 返回 (action, detail, err)：err 只用于日志，任何情况下都不会把失败写成成功。
func (d *Dispatcher) send(ctx context.Context, row *model.NotificationDelivery) (string, string, error) {
	now := d.now()
	from := policy.DeliverySourceStates(model.DeliveryStateSent)

	// 1) 过期：不再打扰用户。
	if row.ExpireAt > 0 && row.ExpireAt <= now.Unix() {
		return d.finishSuppressed(ctx, row, "suppressed: expired before dispatch", from)
	}
	// 2) 通道合法性（枚举漂移时宁可拦截）。
	channel := model.ChannelName(row.Channel)
	if channel == "" {
		return d.finishDeadLetter(ctx, row, fmt.Sprintf("%v: channel=%d", model.ErrInvalidChannel, row.Channel), from)
	}
	// 3) 接收人标识解析。本服务不存明文号码，解析不了就显式失败。
	target, err := d.contact.Resolve(ctx, channel, row.Mid, row.TargetRef)
	if err != nil {
		switch {
		case errors.Is(err, provider.ErrContactNotWired):
			// 契约缺口：等 account 域补齐取联系方式的 RPC 后自动恢复，不判死。
			return d.finishRetry(ctx, row, now, err.Error(), from)
		case errors.Is(err, provider.ErrInvalidConfig):
			return d.finishSuppressed(ctx, row, "suppressed: unparsable recipient: "+err.Error(), from)
		default:
			return d.finishRetry(ctx, row, now, err.Error(), from)
		}
	}
	// 4) 免打扰：用户显式关闭该通道 -> 拦截；处于静默时段 -> 顺延（不消耗重试次数）。
	if blocked, reason := d.preferenceBlocked(ctx, row, now); blocked {
		if reason == reasonQuietHours {
			return d.finishHold(ctx, row, now, from)
		}
		return d.finishSuppressed(ctx, row, "suppressed: "+reason, from)
	}
	// 5) 按锁定版本重渲染（内容不落库，见 README“隐私与内容”）。
	title, body, err := d.renderRow(ctx, row)
	if err != nil {
		if permanentRenderError(err) {
			return d.finishDeadLetter(ctx, row, err.Error(), from)
		}
		return d.finishRetry(ctx, row, now, err.Error(), from)
	}
	// 6) 通道适配器：未配置时绝不伪造成功。
	p, err := d.providers.Get(channel)
	if err != nil {
		return d.finishRetry(ctx, row, now, err.Error(), from)
	}
	res, err := p.Send(ctx, &provider.SendRequest{
		DeliveryID:     row.DeliveryId,
		Channel:        channel,
		TargetRef:      target,
		Title:          title,
		Body:           body,
		Lang:           row.Lang,
		TemplateCode:   row.TemplateCode,
		IdempotencyKey: row.BizKey,
		TraceID:        row.TraceId,
	})
	if err != nil {
		if permanentSendError(err) {
			return d.finishDeadLetter(ctx, row, err.Error(), from)
		}
		return d.finishRetry(ctx, row, now, err.Error(), from)
	}
	if res == nil || !res.Accepted {
		// 适配器没有明确受理，一律按失败处理，避免“未知结果写成已发送”。
		return d.finishRetry(ctx, row, now, "provider returned unaccepted result", from)
	}
	digest := policy.Digest(title, body)
	ok, err := d.repo.MarkDeliverySent(ctx, row.DeliveryId, res.Provider, res.ProviderMsgID, digest, now.Unix(), from)
	if err != nil {
		return actionRetry, "mark sent error", err
	}
	if !ok {
		return actionSkipped, "state changed concurrently", nil
	}
	return actionSent, res.ProviderMsgID, nil
}

// renderRow 用落库时锁定的模板版本重渲染标题与正文。
func (d *Dispatcher) renderRow(ctx context.Context, row *model.NotificationDelivery) (string, string, error) {
	var params map[string]string
	if row.ParamsJson != "" {
		if err := json.Unmarshal([]byte(row.ParamsJson), &params); err != nil {
			return "", "", fmt.Errorf("%w: decode params_json: %v", policy.ErrInvalidTemplate, err)
		}
	}
	tpl, err := d.repo.FindTemplateVersion(ctx, row.TemplateCode, row.Channel, row.Lang, row.TemplateVersion)
	if err != nil {
		return "", "", err
	}
	if tpl == nil {
		return "", "", fmt.Errorf("%w: code=%s channel=%d lang=%s version=%d",
			model.ErrTemplateNotFound, row.TemplateCode, row.Channel, row.Lang, row.TemplateVersion)
	}
	out, err := policy.Render(tpl.TitleTpl, tpl.BodyTpl, params)
	if err != nil {
		return "", "", err
	}
	return out.Title, out.Body, nil
}

// preferenceBlocked 判断该任务此刻是否不该发出。
// reasonQuietHours 表示只是处于静默时段（可顺延），其余原因按拦截处理。
func (d *Dispatcher) preferenceBlocked(ctx context.Context, row *model.NotificationDelivery, now time.Time) (bool, string) {
	if row.Mid <= 0 {
		return false, ""
	}
	pref, err := d.repo.DndPref(ctx, row.Mid)
	if err != nil {
		// 偏好读不到时 fail-closed：宁可不发，也不能打扰已关闭通道的用户。
		logx.Errorw("notification dispatcher read pref failed, fail-closed", logx.Field("mid", row.Mid), logx.Field("error", err.Error()))
		return true, "preference unavailable: " + err.Error()
	}
	if pref == nil {
		return false, ""
	}
	if model.IsChannelMuted(pref.MutedChannels, row.Channel) {
		return true, "channel muted by user preference"
	}
	if !d.policy.DndEnabled || pref.State != model.DndStateOn || row.Priority == policy.PriorityHigh {
		return false, ""
	}
	loc := d.loc
	if tz := pref.Timezone; tz != "" {
		if l, lerr := policy.LoadLocation(tz, ""); lerr == nil {
			loc = l
		}
	}
	in, ierr := policy.InQuietHours(now, loc, pref.QuietStart, pref.QuietEnd)
	if ierr != nil {
		return true, "invalid quiet hours: " + ierr.Error()
	}
	if in {
		return true, reasonQuietHours
	}
	return false, ""
}

// reasonQuietHours 是免打扰时段的内部标记（区别于“用户关闭通道”）。
const reasonQuietHours = "within quiet hours"

func (d *Dispatcher) finishSuppressed(ctx context.Context, row *model.NotificationDelivery, reason string, from []int32) (string, string, error) {
	ok, err := d.repo.MarkDeliverySuppressed(ctx, row.DeliveryId, reason, from)
	return finishResult(actionSuppressed, reason, ok, err)
}

func (d *Dispatcher) finishHold(ctx context.Context, row *model.NotificationDelivery, now time.Time, from []int32) (string, string, error) {
	next := now.Add(quietHoursHold).Unix()
	ok, err := d.repo.MarkDeliveryRetry(ctx, row.DeliveryId, row.RetryCount, next, "held: "+reasonQuietHours,
		policy.DeliverySourceStates(model.DeliveryStateRetry))
	return finishResult(actionHeld, reasonQuietHours, ok, err)
}

func (d *Dispatcher) finishRetry(ctx context.Context, row *model.NotificationDelivery, now time.Time, reason string, from []int32) (string, string, error) {
	nextCount := row.RetryCount + 1
	if policy.RetryExhausted(nextCount, d.policy.MaxRetries) {
		return d.finishDeadLetter(ctx, row, fmt.Sprintf("retries exhausted (%d): %s", row.RetryCount, reason), from)
	}
	next := policy.NextRetryAt(now, nextCount, d.policy.BackoffSeconds)
	ok, err := d.repo.MarkDeliveryRetry(ctx, row.DeliveryId, nextCount, next, reason,
		policy.DeliverySourceStates(model.DeliveryStateRetry))
	return finishResult(actionRetry, reason, ok, err)
}

func (d *Dispatcher) finishDeadLetter(ctx context.Context, row *model.NotificationDelivery, reason string, from []int32) (string, string, error) {
	ok, err := d.repo.MarkDeliveryDeadLetter(ctx, row.DeliveryId, reason, from)
	if err != nil {
		return actionDeadLetter, reason, err
	}
	if ok {
		archived, aErr := d.repo.ArchiveDeadLetter(ctx, &model.NotificationDeadLetter{
			EventId:       row.SourceEventId,
			Source:        model.DeadLetterSourceDelivery,
			DeliveryId:    row.DeliveryId,
			Topic:         "delivery:" + row.TemplateCode,
			PayloadDigest: row.PayloadDigest,
			Reason:        reason,
			State:         model.DeadLetterStatePending,
		})
		if aErr != nil {
			return actionDeadLetter, reason, aErr
		}
		_ = archived
	}
	return actionDeadLetter, reason, nil
}

// finishResult 统一处理“守卫未命中/写库出错”的返回。
func finishResult(action, detail string, ok bool, err error) (string, string, error) {
	if err != nil {
		return action, detail, err
	}
	if !ok {
		return actionSkipped, "state changed concurrently", nil
	}
	return action, detail, nil
}

// permanentRenderError 判定“重试也不会变好”的渲染错误（模板缺版本/内容非法）。
func permanentRenderError(err error) bool {
	return errors.Is(err, model.ErrTemplateNotFound) ||
		errors.Is(err, policy.ErrInvalidTemplate) ||
		errors.Is(err, policy.ErrRenderMissingVar) ||
		errors.Is(err, policy.ErrRenderUnknownVar) ||
		errors.Is(err, policy.ErrRenderTooLong)
}

// permanentSendError 判定“配置层面不可用”的发送错误。
// 注意：ErrProviderNotConfigured 与 ErrContactNotWired 不在其中 —— 它们可能因运维补配
// 或契约补齐而恢复，必须走退避重试，最终由死信留档交人工处理。
func permanentSendError(err error) bool {
	return errors.Is(err, provider.ErrUnsupportedChannel) || errors.Is(err, provider.ErrInvalidConfig)
}
