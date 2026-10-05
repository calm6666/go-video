// Package repository 是 notification 服务的数据访问层。
// 只访问本服务的 5 张表（AGENTS.md §5）；跨服务信息一律通过 RPC/事件，不直连他人库表。
// Redis 只承担“每日配额计数”，不作为投递事实源；Redis 不可用时按 fail-closed 处理，
// 宁可拒绝发送，也不能绕过频控把提醒打爆用户。
package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/notification/model"
)

// QuotaCounter 是频控需要的最小 Redis 能力集，*redis.Redis 天然满足；
// 抽象成接口是为了单测可注入 fake，不依赖真实 Redis。
type QuotaCounter interface {
	IncrCtx(ctx context.Context, key string) (int64, error)
	ExpireCtx(ctx context.Context, key string, seconds int) error
}

// ErrQuotaUnavailable Redis 配额计数不可用：调用方必须 fail-closed 拒绝发送。
var ErrQuotaUnavailable = errors.New("notification/repository: quota counter unavailable")

// Repository notification 数据访问入口。
type Repository struct {
	conn   sqlx.SqlConn
	quota  QuotaCounter
	tmpl   model.NotificationTemplateModel
	deliv  model.NotificationDeliveryModel
	offset model.NotificationConsumerOffsetModel
	dead   model.NotificationDeadLetterModel
	dnd    model.NotificationDndPrefModel
}

// New 构造生产用的 Repository。
func New(conn sqlx.SqlConn, quota QuotaCounter) *Repository {
	return &Repository{
		conn:   conn,
		quota:  quota,
		tmpl:   model.NewNotificationTemplateModel(conn),
		deliv:  model.NewNotificationDeliveryModel(conn),
		offset: model.NewNotificationConsumerOffsetModel(conn),
		dead:   model.NewNotificationDeadLetterModel(conn),
		dnd:    model.NewNotificationDndPrefModel(conn),
	}
}

// NewWithModels 用显式 model 实现构造 Repository（单测注入 fake）。
func NewWithModels(quota QuotaCounter, tmpl model.NotificationTemplateModel, deliv model.NotificationDeliveryModel,
	offset model.NotificationConsumerOffsetModel, dead model.NotificationDeadLetterModel, dnd model.NotificationDndPrefModel) *Repository {
	return &Repository{quota: quota, tmpl: tmpl, deliv: deliv, offset: offset, dead: dead, dnd: dnd}
}

// Ping 校验数据库连通性（健康检查用，不写数据）。
func (r *Repository) Ping(ctx context.Context) error {
	if r.conn == nil {
		return errors.New("notification/repository: db not configured")
	}
	var ok int
	if err := r.conn.QueryRowCtx(ctx, &ok, "SELECT 1"); err != nil {
		return fmt.Errorf("notification/repository ping: %w", err)
	}
	return nil
}

// --- 模板 ---

// Template 返回底层模板模型（供 logic 直接调用查询方法）。
func (r *Repository) Template() model.NotificationTemplateModel { return r.tmpl }

// DeliveryModel 返回投递任务模型。
func (r *Repository) DeliveryModel() model.NotificationDeliveryModel { return r.deliv }

// DeadLetterModel 返回死信模型。
func (r *Repository) DeadLetterModel() model.NotificationDeadLetterModel { return r.dead }

// OffsetModel 返回事件消费状态模型。
func (r *Repository) OffsetModel() model.NotificationConsumerOffsetModel { return r.offset }

// UpsertTemplate 写入模板：
//   - 同 (code, channel, lang) 已有草稿时覆盖草稿内容；
//   - 否则以 max(version)+1 新建版本；
//   - publish=true 时立即发布，并把同键旧的已发布版本置为下线（单事务，见 model.PublishDraft）。
func (r *Repository) UpsertTemplate(ctx context.Context, in *model.NotificationTemplate, publish bool) (*model.NotificationTemplate, error) {
	if !model.IsValidChannel(in.Channel) {
		return nil, fmt.Errorf("%w: channel=%d", model.ErrInvalidChannel, in.Channel)
	}
	if !model.IsValidLang(in.Lang) {
		return nil, fmt.Errorf("%w: lang=%q", model.ErrInvalidLang, in.Lang)
	}
	existing, err := r.tmpl.FindByState(ctx, in.TemplateCode, in.Channel, in.Lang, model.TemplateStateDraft)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if err := r.tmpl.UpdateDraftContent(ctx, existing.Id, in.TitleTpl, in.BodyTpl, in.Operator); err != nil {
			return nil, err
		}
		existing.TitleTpl, existing.BodyTpl, existing.Operator = in.TitleTpl, in.BodyTpl, in.Operator
		if publish {
			return r.tmpl.PublishDraft(ctx, existing.Id, in.Operator)
		}
		return existing, nil
	}
	maxVer, err := r.tmpl.MaxVersion(ctx, in.TemplateCode, in.Channel, in.Lang)
	if err != nil {
		return nil, err
	}
	now := model.NowUnix()
	if in.Ctime == 0 {
		in.Ctime = now
	}
	in.Mtime = now
	in.Version = maxVer + 1
	in.State = model.TemplateStateDraft
	id, err := r.tmpl.Insert(ctx, in)
	if err != nil {
		return nil, err
	}
	in.Id = id
	if publish {
		return r.tmpl.PublishDraft(ctx, id, in.Operator)
	}
	return in, nil
}

// FindPublished 按语言优先级查找已发布模板；返回模板与命中的语言。
// langs 为空或全部未命中时返回 ErrTemplateNotFound。
func (r *Repository) FindPublished(ctx context.Context, code string, channel int32, langs []string) (*model.NotificationTemplate, string, error) {
	tried := make(map[string]bool, len(langs))
	for _, lang := range langs {
		lang = strings.TrimSpace(lang)
		if lang == "" || tried[lang] {
			continue
		}
		tried[lang] = true
		t, err := r.tmpl.FindByState(ctx, code, channel, lang, model.TemplateStatePublished)
		if err != nil {
			return nil, "", err
		}
		if t != nil {
			return t, lang, nil
		}
	}
	return nil, "", fmt.Errorf("%w: code=%s channel=%d langs=%v", model.ErrTemplateNotFound, code, channel, langs)
}

// --- 投递任务 ---

// CreateDelivery 幂等创建投递任务：命中 biz_key 唯一索引时返回已存在的行且 created=false。
func (r *Repository) CreateDelivery(ctx context.Context, d *model.NotificationDelivery) (*model.NotificationDelivery, bool, error) {
	if d.BizKey == "" {
		return nil, false, errors.New("notification/repository: delivery biz_key is required")
	}
	if d.DeliveryId == "" {
		return nil, false, errors.New("notification/repository: delivery_id is required")
	}
	now := model.NowUnix()
	if d.Ctime == 0 {
		d.Ctime = now
	}
	d.Mtime = now
	if err := r.deliv.Insert(ctx, d); err != nil {
		if errors.Is(err, model.ErrDuplicateBizKey) {
			existed, findErr := r.deliv.FindByBizKey(ctx, d.BizKey)
			if findErr != nil {
				return nil, false, findErr
			}
			if existed == nil {
				return nil, false, err
			}
			return existed, false, nil
		}
		return nil, false, err
	}
	return d, true, nil
}

// --- 偏好 ---

// DndPref 查询用户偏好；未设置过返回 (nil, nil)。
func (r *Repository) DndPref(ctx context.Context, mid int64) (*model.NotificationDndPref, error) {
	return r.dnd.FindOne(ctx, mid)
}

// SaveDndPref 全量覆盖用户偏好（mid 主键 + Upsert，重复提交结果一致）。
func (r *Repository) SaveDndPref(ctx context.Context, p *model.NotificationDndPref) error {
	if p.Mid <= 0 {
		return errors.New("notification/repository: dnd mid is required")
	}
	if err := validateClock(p.QuietStart); err != nil {
		return err
	}
	if err := validateClock(p.QuietEnd); err != nil {
		return err
	}
	if _, err := time.LoadLocation(strings.TrimSpace(p.Timezone)); err != nil && strings.TrimSpace(p.Timezone) != "" {
		return fmt.Errorf("notification/repository: invalid timezone %q: %w", p.Timezone, err)
	}
	now := model.NowUnix()
	if p.Ctime == 0 {
		p.Ctime = now
	}
	p.Mtime = now
	return r.dnd.Upsert(ctx, p)
}

// validateClock 校验 "HH:MM"；空串表示不设时段。
// 规则与 policy.ParseClock 一致；数据访问层不依赖策略包，故独立实现。
func validateClock(s string) error {
	v := strings.TrimSpace(s)
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ":")
	if len(parts) != 2 {
		return fmt.Errorf("notification/repository: quiet time must be HH:MM, got %q", s)
	}
	h, m := 0, 0
	if _, err := fmt.Sscanf(v, "%d:%d", &h, &m); err != nil {
		return fmt.Errorf("notification/repository: quiet time must be HH:MM, got %q", s)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return fmt.Errorf("notification/repository: quiet time out of range, got %q", s)
	}
	return nil
}

// --- 频控 ---

// quotaKey 以“用户本地日期”为粒度，避免时区差异导致配额在半夜被重置或提前。
func quotaKey(mid int64, channel int32, day string) string {
	return fmt.Sprintf("notify:quota:%d:%s:%s", mid, model.ChannelName(channel), day)
}

// AcquireQuota 消耗一次每日配额。limit<=0 表示不限制。
// Redis 不可用时返回 ErrQuotaUnavailable，调用方必须拒绝发送而不是跳过校验。
func (r *Repository) AcquireQuota(ctx context.Context, mid int64, channel int32, now time.Time, loc *time.Location, limit int32) (used int64, allowed bool, err error) {
	if limit <= 0 {
		return 0, true, nil
	}
	if mid <= 0 {
		// 没有 mid 的接收人无法按用户配额，交由通道级限流处理（本期不做营销推送，风险可控）。
		return 0, true, nil
	}
	if r.quota == nil {
		return 0, false, ErrQuotaUnavailable
	}
	if loc == nil {
		loc = time.Local
	}
	day := now.In(loc).Format("20060102")
	used, err = r.quota.IncrCtx(ctx, quotaKey(mid, channel, day))
	if err != nil {
		return 0, false, fmt.Errorf("%w: %w", ErrQuotaUnavailable, err)
	}
	if used == 1 {
		// 48 小时 TTL：覆盖时区切换导致的跨天写入，之后自动回收。
		if err := r.quota.ExpireCtx(ctx, quotaKey(mid, channel, day), 172800); err != nil {
			return used, false, fmt.Errorf("%w: %w", ErrQuotaUnavailable, err)
		}
	}
	if used > int64(limit) {
		return used, false, nil
	}
	return used, true, nil
}

// --- 投递任务读写（供 logic 与投递调度器使用）---
//
// 所有状态写入都要求调用方给出合法源状态集合 from（由 internal/policy 的状态机推导），
// 底层是 `UPDATE ... WHERE delivery_id = ? AND state IN (from...)`：
// 多实例并发扫描同一批任务时，只有一个 worker 能推进状态，其余返回 false 后跳过。

// ListDueDeliveries 扫描到期待投递任务。
func (r *Repository) ListDueDeliveries(ctx context.Context, now int64, limit int32) ([]*model.NotificationDelivery, error) {
	return r.deliv.ListDue(ctx, now, limit)
}

// FindDelivery 按 delivery_id 查询。
func (r *Repository) FindDelivery(ctx context.Context, deliveryId string) (*model.NotificationDelivery, error) {
	return r.deliv.FindOne(ctx, deliveryId)
}

// ListDeliveries 分页查询投递记录。
func (r *Repository) ListDeliveries(ctx context.Context, f model.DeliveryFilter, pn, ps int32) ([]*model.NotificationDelivery, int64, error) {
	return r.deliv.List(ctx, f, pn, ps)
}

// ListDeliveriesBySourceEvent 查询事件派生的投递任务。
func (r *Repository) ListDeliveriesBySourceEvent(ctx context.Context, eventId string, states []int32) ([]*model.NotificationDelivery, error) {
	return r.deliv.ListBySourceEvent(ctx, eventId, states)
}

// MarkDeliverySent 置为已发送。
func (r *Repository) MarkDeliverySent(ctx context.Context, deliveryId, providerName, providerMsgId, payloadDigest string, sentAt int64, from []int32) (bool, error) {
	return r.deliv.MarkSent(ctx, deliveryId, providerName, providerMsgId, payloadDigest, sentAt, from)
}

// MarkDeliveryRetry 置为退避重试。
func (r *Repository) MarkDeliveryRetry(ctx context.Context, deliveryId string, retryCount int32, nextRetryAt int64, reason string, from []int32) (bool, error) {
	return r.deliv.MarkRetry(ctx, deliveryId, retryCount, nextRetryAt, reason, from)
}

// MarkDeliveryFailed 置为不可重试失败。
func (r *Repository) MarkDeliveryFailed(ctx context.Context, deliveryId, reason string, from []int32) (bool, error) {
	return r.deliv.MarkFailed(ctx, deliveryId, reason, from)
}

// MarkDeliveryDeadLetter 置为死信。
func (r *Repository) MarkDeliveryDeadLetter(ctx context.Context, deliveryId, reason string, from []int32) (bool, error) {
	return r.deliv.MarkDeadLetter(ctx, deliveryId, reason, from)
}

// MarkDeliverySuppressed 置为已拦截。
func (r *Repository) MarkDeliverySuppressed(ctx context.Context, deliveryId, reason string, from []int32) (bool, error) {
	return r.deliv.MarkSuppressed(ctx, deliveryId, reason, from)
}

// ResetDeliveryForRetry 把死信任务重置为待投递（运营重投）。
func (r *Repository) ResetDeliveryForRetry(ctx context.Context, deliveryId string) (bool, error) {
	return r.deliv.ResetForDeadLetterRetry(ctx, deliveryId)
}

// --- 模板版本（异步投递时按锁定版本重渲染）---

// FindTemplateVersion 精确查询模板版本；不存在返回 (nil, nil)。
func (r *Repository) FindTemplateVersion(ctx context.Context, code string, channel int32, lang string, version int32) (*model.NotificationTemplate, error) {
	return r.tmpl.Find(ctx, code, channel, lang, version)
}

// ListTemplates 分页查询模板。
func (r *Repository) ListTemplates(ctx context.Context, f model.TemplateFilter, pn, ps int32) ([]*model.NotificationTemplate, int64, error) {
	return r.tmpl.List(ctx, f, pn, ps)
}

// FindTemplateByID 按主键查询模板。
func (r *Repository) FindTemplateByID(ctx context.Context, id int64) (*model.NotificationTemplate, error) {
	return r.tmpl.FindByID(ctx, id)
}

// PublishTemplate 发布草稿并把同键旧已发布版本下线（单事务）。
func (r *Repository) PublishTemplate(ctx context.Context, id int64, operator string) (*model.NotificationTemplate, error) {
	return r.tmpl.PublishDraft(ctx, id, operator)
}

// --- 死信 ---

// ArchiveDeadLetter 幂等登记死信；返回 false 表示已登记过。
func (r *Repository) ArchiveDeadLetter(ctx context.Context, d *model.NotificationDeadLetter) (bool, error) {
	if d.Ctime == 0 {
		d.Ctime = model.NowUnix()
	}
	d.Mtime = model.NowUnix()
	if d.State == 0 {
		d.State = model.DeadLetterStatePending
	}
	return r.dead.InsertIfAbsent(ctx, d)
}

// FindDeadLetter 按主键查询死信。
func (r *Repository) FindDeadLetter(ctx context.Context, id int64) (*model.NotificationDeadLetter, error) {
	return r.dead.FindOne(ctx, id)
}

// ListDeadLetters 分页查询死信。
func (r *Repository) ListDeadLetters(ctx context.Context, f model.DeadLetterFilter, pn, ps int32) ([]*model.NotificationDeadLetter, int64, error) {
	return r.dead.List(ctx, f, pn, ps)
}

// MarkDeadLetterState 流转死信处置状态。
func (r *Repository) MarkDeadLetterState(ctx context.Context, id int64, to int32, operator string, from []int32) (bool, error) {
	return r.dead.MarkState(ctx, id, to, operator, from)
}

// --- 事件消费状态 ---

// InsertEventIfAbsent 幂等登记事件；返回 false 表示重复投递。
func (r *Repository) InsertEventIfAbsent(ctx context.Context, o *model.NotificationConsumerOffset) (bool, error) {
	if o.Ctime == 0 {
		o.Ctime = model.NowUnix()
	}
	o.Mtime = model.NowUnix()
	if o.State == 0 {
		o.State = model.EventStateReceived
	}
	return r.offset.InsertIfAbsent(ctx, o)
}

// FindEvent 按 event_id 查询消费状态。
func (r *Repository) FindEvent(ctx context.Context, eventId string) (*model.NotificationConsumerOffset, error) {
	return r.offset.FindOne(ctx, eventId)
}

// MarkEventState 带源状态守卫地流转事件状态。
func (r *Repository) MarkEventState(ctx context.Context, eventId string, to int32, retryCount int32, nextRetryAt int64, reason string, from []int32) (bool, error) {
	return r.offset.MarkState(ctx, eventId, to, retryCount, nextRetryAt, reason, from)
}

// ListDueEvents 扫描到期待重试事件。
func (r *Repository) ListDueEvents(ctx context.Context, now int64, limit int32) ([]*model.NotificationConsumerOffset, error) {
	return r.offset.ListDue(ctx, now, limit)
}

// 说明：配额不做“写库失败回滚”。go-zero 的 Redis 接口没有 Decr，
// 删除整键会把用户当天已用配额清零（偏松，可能超发）；
// 因此这里刻意保留计数，宁可少发也不超发，语义记录在 README“频控”一节。
