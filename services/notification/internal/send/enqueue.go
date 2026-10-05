// Package send 是“把一次投递请求展开成投递任务行”的领域用例，被两条入口共用：
//
//   - internal/logic 的 SendNotification（gRPC 同步入口）；
//   - internal/consumer 的 EventHandler（notification.request.v1 事件入口，
//     通过 consumer.Sender 接口注入，见 Enqueuer.Send）。
//
// 之所以不放 internal/logic：logic 依赖 internal/svc，而 svc 要装配消费者与投递调度器，
// 反过来 import logic 会形成包环；用例下沉到本层后两条链路必然走同一套校验与幂等口径，
// 不会出现“RPC 直投被拦截、事件投递却放行”的分裂行为（AGENTS.md §5 幂等要求）。
//
// 本包只负责“落任务”，真正调用供应商的动作由 internal/consumer.Dispatcher 按
// next_retry_at 扫描执行；SyncSend 模式下由 logic 在落库后立即触发一次 Dispatch。
package send

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go-video/common/idgen"
	"go-video/services/notification/internal/config"
	"go-video/services/notification/internal/policy"
	"go-video/services/notification/internal/repository"
	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

// 显式错误：调用方（gateway / operation）按这些语义决定给终端返回什么提示。
var (
	// ErrNoRecipients 请求未携带任何接收人。
	ErrNoRecipients = errors.New("notification/send: recipients is empty")
	// ErrTooManyRecipients 接收人数超过单次上限（避免一次请求放大投递量）。
	ErrTooManyRecipients = errors.New("notification/send: too many recipients")
	// ErrExpiredRequest expire_at 已是过去时间：任务永远不会被投递，显式拒绝而不是静默丢弃。
	ErrExpiredRequest = errors.New("notification/send: expire_at is already past")
	// ErrNoRecipientTarget 接收人既没有 mid 也没有 target_ref，无法定位投递对象。
	ErrNoRecipientTarget = errors.New("notification/send: recipient requires mid or target_ref")
	// ErrRecipientNil 接收人为空指针。
	ErrRecipientNil = errors.New("notification/send: nil recipient")
)

// Options 是用例运行参数（由 config.NotificationConf 派生）。
type Options struct {
	// DailyQuotaPerMid 单用户单通道每日上限，<=0 表示不限制。
	DailyQuotaPerMid int32
	// DndEnabled 是否启用免打扰时段判定（关闭时仍尊重“用户关闭通道”的硬偏好）。
	DndEnabled bool
	// DefaultLanguage 请求与接收人都未指定语言时的回落语言编码。
	DefaultLanguage string
	// MaxRecipients 单次请求接收人上限，<=0 时回落 200。
	MaxRecipients int32
	// DefaultTimezone 配额与免打扰判定使用的默认 IANA 时区。
	DefaultTimezone string
}

// OptionsFrom 从服务配置派生用例参数。
func OptionsFrom(c config.NotificationConf) Options {
	return Options{
		DailyQuotaPerMid: c.DailyQuotaPerMid,
		DndEnabled:       c.DndEnabled,
		DefaultLanguage:  c.DefaultLanguage,
		MaxRecipients:    c.MaxRecipients,
		DefaultTimezone:  c.DefaultTimezone,
	}
}

const defaultMaxRecipients = 200

// Result 入队结果。
type Result struct {
	// Reply 返回给调用方的投递记录集合（每个接收人一条）。
	Reply *rpc.SendNotificationReply
	// NewDeliveryIDs 本次真正新建、且处于待投递状态的任务 ID。
	// SyncSend 只对这批任务立即投递：幂等回放与已拦截任务都不会再次外发。
	NewDeliveryIDs []string
}

// Enqueuer 把投递请求展开成 notification_delivery 行。
// 它是纯数据编排：不持有 gRPC/HTTP 客户端，不调用任何供应商接口。
type Enqueuer struct {
	repo *repository.Repository
	opt  Options
	loc  *time.Location
	now  func() time.Time
}

// New 构造入队器。时区或默认语言非法时返回错误：
// 这属于配置错误，必须启动期暴露，不能运行期把免打扰窗口算偏。
func New(repo *repository.Repository, opt Options) (*Enqueuer, error) {
	if repo == nil {
		return nil, errors.New("notification/send: repository is required")
	}
	loc, err := policy.LoadLocation(opt.DefaultTimezone, "UTC")
	if err != nil {
		return nil, fmt.Errorf("notification/send: %w", err)
	}
	if opt.MaxRecipients <= 0 {
		opt.MaxRecipients = defaultMaxRecipients
	}
	if opt.DefaultLanguage == "" {
		opt.DefaultLanguage = model.LangZhCN
	}
	if !model.IsValidLang(opt.DefaultLanguage) {
		return nil, fmt.Errorf("%w: default language %q", model.ErrInvalidLang, opt.DefaultLanguage)
	}
	return &Enqueuer{repo: repo, opt: opt, loc: loc, now: time.Now}, nil
}

// WithClock 注入时间源（单测用），返回浅拷贝以免影响生产实例。
func (e *Enqueuer) WithClock(now func() time.Time) *Enqueuer {
	if now == nil {
		return e
	}
	cp := *e
	cp.now = now
	return &cp
}

// WithLocation 注入时区（单测用）。
func (e *Enqueuer) WithLocation(loc *time.Location) *Enqueuer {
	if loc == nil {
		return e
	}
	cp := *e
	cp.loc = loc
	return &cp
}

// DndEnabled 报告当前是否启用免打扰时段判定，供 logic/README 与运维核对配置生效情况。
func (e *Enqueuer) DndEnabled() bool { return e.opt.DndEnabled }

// Send 实现 consumer.Sender：事件入口与 gRPC 入口共用同一套校验与幂等逻辑。
func (e *Enqueuer) Send(ctx context.Context, in *rpc.SendNotificationReq) (*rpc.SendNotificationReply, error) {
	res, err := e.Enqueue(ctx, in)
	if err != nil {
		return nil, err
	}
	return res.Reply, nil
}

// Enqueue 校验请求 -> 解析已发布模板 -> 试渲染校验变量 -> 按用户通道偏好与每日配额拦截 ->
// 以行级 biz_key 幂等落库。整个过程不会调用任何供应商接口，也不会把失败写成成功。
func (e *Enqueuer) Enqueue(ctx context.Context, in *rpc.SendNotificationReq) (*Result, error) {
	if in == nil {
		return nil, errors.New("notification/send: nil request")
	}
	channelName, err := policy.ChannelOfEnum(in.GetChannel())
	if err != nil {
		return nil, err
	}
	channel := int32(in.GetChannel())
	code := strings.TrimSpace(in.GetTemplateCode())
	if code == "" {
		return nil, errors.New("notification/send: template_code is required")
	}
	params := in.GetTemplateParams()
	if err := policy.ValidateParams(params); err != nil {
		return nil, err
	}
	groupKey, err := policy.GroupBizKey(in.GetBizKey(), in.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	recips := in.GetRecipients()
	if len(recips) == 0 {
		return nil, ErrNoRecipients
	}
	if int32(len(recips)) > e.opt.MaxRecipients {
		return nil, fmt.Errorf("%w: %d > %d", ErrTooManyRecipients, len(recips), e.opt.MaxRecipients)
	}
	now := e.now()
	if expire := in.GetExpireAt(); expire > 0 && expire <= now.Unix() {
		return nil, fmt.Errorf("%w: expire_at=%d now=%d", ErrExpiredRequest, expire, now.Unix())
	}
	priority := policy.PriorityCodeOf(in.GetPriority())
	paramsJSON, err := encodeParams(params)
	if err != nil {
		return nil, err
	}
	eventID := policy.EventIDFromCtx(ctx)

	reply := &rpc.SendNotificationReply{Deliveries: make([]*rpc.DeliveryInfo, 0, len(recips))}
	pending := make([]string, 0, len(recips))
	seenRowKey := make(map[string]bool, len(recips))
	tplCache := make(map[string]*model.NotificationTemplate, 4)
	prefCache := make(map[int64]*model.NotificationDndPref, len(recips))

	for _, r := range recips {
		if r == nil {
			return nil, ErrRecipientNil
		}
		if r.GetMid() <= 0 && strings.TrimSpace(r.GetTargetRef()) == "" {
			return nil, fmt.Errorf("%w: mid=0 且 target_ref 为空", ErrNoRecipientTarget)
		}
		tpl, lang, err := e.resolveTemplate(ctx, code, channel, e.langCandidates(r, in.GetDefaultLanguage()), tplCache)
		if err != nil {
			return nil, err
		}
		// 落库前先试渲染一次：变量缺失属于调用方错误，必须在入口拒绝，
		// 而不是写出一条注定进死信的任务（错误里带缺失变量清单，见 policy.Render）。
		if _, err := policy.Render(tpl.TitleTpl, tpl.BodyTpl, params); err != nil {
			return nil, fmt.Errorf("notification/send: template %s/%s/%s v%d: %w",
				code, channelName, lang, tpl.Version, err)
		}
		rowKey, err := e.rowKey(groupKey, channel, r)
		if err != nil {
			return nil, err
		}
		if seenRowKey[rowKey] {
			// 同一请求里重复的接收人只投一次：行级 biz_key 唯一索引会拒绝第二次插入。
			reply.Duplicate = true
			continue
		}
		seenRowKey[rowKey] = true

		state, reason, err := e.screen(ctx, r.GetMid(), channel, now, prefCache)
		if err != nil {
			return nil, err
		}
		saved, created, err := e.repo.CreateDelivery(ctx, &model.NotificationDelivery{
			DeliveryId:      idgen.MustULID(),
			BizKey:          rowKey,
			BizGroupKey:     groupKey,
			Mid:             r.GetMid(),
			Channel:         channel,
			TemplateCode:    code,
			TemplateVersion: tpl.Version,
			Lang:            lang,
			TargetRef:       strings.TrimSpace(r.GetTargetRef()),
			ParamsJson:      paramsJSON,
			State:           state,
			Priority:        priority,
			ExpireAt:        in.GetExpireAt(),
			SourceEventId:   eventID,
			TraceId:         in.GetTraceId(),
			LastError:       reason,
			Ctime:           now.Unix(),
			Mtime:           now.Unix(),
		})
		if err != nil {
			return nil, err
		}
		reply.Deliveries = append(reply.Deliveries, policy.ToDeliveryInfo(saved))
		if !created {
			// 跨实例/跨请求幂等命中：返回既有任务，绝不重复投递。
			reply.Duplicate = true
			continue
		}
		if saved.State == model.DeliveryStateSuppressed {
			reply.Suppressed++
			continue
		}
		pending = append(pending, saved.DeliveryId)
	}
	return &Result{Reply: reply, NewDeliveryIDs: pending}, nil
}

// screen 决定任务落库时的初始状态：待投递，还是直接落终态 suppressed。
//
// 这里只拦两类“重试也不会变好”的情况：
//   - 用户显式关闭该通道（硬偏好，任何优先级都不发，也不受 DndEnabled 开关影响）；
//   - 超过每日配额（再发就是打扰用户）。
//
// 免打扰时段不在这里拦：它只是“现在不该发”，由 Dispatcher 走 held + retry 顺延，
// 落库成 suppressed 会把该发的提醒永久丢掉。
// 返回 error 代表无法安全判定（配额存储或偏好表不可用），调用方必须拒绝整批请求，
// 让上游按同一 biz_key 重试，而不是在数据源抖动时把通知写成成功或永久拦截。
func (e *Enqueuer) screen(ctx context.Context, mid int64, channel int32, now time.Time,
	prefCache map[int64]*model.NotificationDndPref) (int32, string, error) {
	if mid > 0 {
		pref, cached := prefCache[mid]
		if !cached {
			p, err := e.repo.DndPref(ctx, mid)
			if err != nil {
				return 0, "", fmt.Errorf("notification/send: 读取用户偏好失败，拒绝投递: %w", err)
			}
			pref = p
			prefCache[mid] = p
		}
		if pref != nil && model.IsChannelMuted(pref.MutedChannels, channel) {
			return model.DeliveryStateSuppressed, "suppressed: channel muted by user preference", nil
		}
	}
	if e.opt.DailyQuotaPerMid > 0 {
		_, allowed, err := e.repo.AcquireQuota(ctx, mid, channel, now, e.loc, e.opt.DailyQuotaPerMid)
		if err != nil {
			return 0, "", err
		}
		if !allowed {
			return model.DeliveryStateSuppressed,
				fmt.Sprintf("suppressed: over daily quota %d per mid per channel", e.opt.DailyQuotaPerMid), nil
		}
	}
	return model.DeliveryStatePending, "", nil
}

// resolveTemplate 按语言优先级取已发布模板，并按 (code,channel,langs) 缓存避免重复查询。
func (e *Enqueuer) resolveTemplate(ctx context.Context, code string, channel int32, langs []string,
	cache map[string]*model.NotificationTemplate) (*model.NotificationTemplate, string, error) {
	key := strings.Join([]string{code, fmt.Sprint(channel), strings.Join(langs, ",")}, "|")
	if t, ok := cache[key]; ok {
		return t, t.Lang, nil
	}
	tpl, lang, err := e.repo.FindPublished(ctx, code, channel, langs)
	if err != nil {
		return nil, "", err
	}
	cache[key] = tpl
	return tpl, lang, nil
}

// langCandidates 给出该接收人的语言回落顺序：本人指定 -> 请求默认 -> 全局默认（去重保序）。
func (e *Enqueuer) langCandidates(r *rpc.Recipient, requestDefault rpc.Language) []string {
	global, _ := policy.LangCodeToEnum(e.opt.DefaultLanguage)
	langs := make([]string, 0, 3)
	seen := make(map[string]bool, 3)
	for _, l := range []rpc.Language{r.GetLanguage(), requestDefault, global} {
		code := policy.LangCodeOfEnum(l)
		if code == "" || seen[code] {
			continue
		}
		seen[code] = true
		langs = append(langs, code)
	}
	return langs
}

// rowKey 派生行级幂等键：收件人身份由 mid（若有）+ 受控投递引用 + 设备标识共同决定。
// 三者都参与派生，否则同一 mid 的多台设备/多个 target_ref 会被唯一索引当成同一行，
// 第二台设备永远收不到通知。相同输入必得同一键值，这是跨实例去重的前提。
func (e *Enqueuer) rowKey(groupKey string, channel int32, r *rpc.Recipient) (string, error) {
	parts := make([]string, 0, 3)
	if id := policy.RecipientKey(r.GetMid(), r.GetTargetRef()); id != "anonymous" {
		parts = append(parts, id)
	}
	if ref := strings.TrimSpace(r.GetTargetRef()); ref != "" {
		parts = append(parts, "ref:"+ref)
	}
	if dev := strings.TrimSpace(r.GetDeviceId()); dev != "" {
		parts = append(parts, "device:"+dev)
	}
	return policy.RowBizKey(groupKey, channel, r.GetMid(), strings.Join(parts, "|"))
}

// encodeParams 把渲染变量序列化成落库快照（encoding/json 对 map key 排序，结果稳定）。
func encodeParams(params map[string]string) (string, error) {
	if len(params) == 0 {
		return "{}", nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return "", fmt.Errorf("notification/send: encode template params: %w", err)
	}
	return string(raw), nil
}
