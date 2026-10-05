// Package testx 提供 notification 服务单测用的内存假件。
//
// 约束（AGENTS.md §9）：这些假件只被 *_test.go 引用，不进生产二进制；
// 它们复刻 model 层的真实语义（唯一键冲突、源状态守卫、退避扫描排序），
// 而不是“永不失败”的空实现 —— 否则测试通过也证明不了状态机与幂等是对的。
// 全部假件不连 MySQL/Redis/Kafka，也不发起任何网络请求。
package testx

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"go-video/services/notification/internal/provider"
	"go-video/services/notification/model"
)

// --- 配额计数 ---

// QuotaCounter 是内存版频控计数器。
type QuotaCounter struct {
	mu     sync.Mutex
	Counts map[string]int64
	TTLs   map[string]int
	// Fail 非空时 Incr 直接失败，用于验证 Redis 不可用时的 fail-closed 行为。
	Fail error
}

// NewQuotaCounter 构造空计数器。
func NewQuotaCounter() *QuotaCounter {
	return &QuotaCounter{Counts: map[string]int64{}, TTLs: map[string]int{}}
}

// IncrCtx 实现 repository.QuotaCounter。
func (q *QuotaCounter) IncrCtx(_ context.Context, key string) (int64, error) {
	if q.Fail != nil {
		return 0, q.Fail
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.Counts[key]++
	return q.Counts[key], nil
}

// ExpireCtx 实现 repository.QuotaCounter。
func (q *QuotaCounter) ExpireCtx(_ context.Context, key string, seconds int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.TTLs[key] = seconds
	return nil
}

// --- 投递任务 ---

// DeliveryModel 是 notification_delivery 的内存实现。
type DeliveryModel struct {
	mu   sync.Mutex
	rows map[string]*model.NotificationDelivery

	// Duplicates 记录命中 biz_key 唯一索引的次数，供测试断言幂等。
	Duplicates int
	// IllegalTransitions 记录被源状态守卫挡下的次数。
	IllegalTransitions int
}

// NewDeliveryModel 构造空的内存投递表。
func NewDeliveryModel() *DeliveryModel {
	return &DeliveryModel{rows: map[string]*model.NotificationDelivery{}}
}

// Rows 返回按 delivery_id 索引的快照（测试断言用）。
func (m *DeliveryModel) Rows() map[string]*model.NotificationDelivery {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]*model.NotificationDelivery, len(m.rows))
	for k, v := range m.rows {
		cp := *v
		out[k] = &cp
	}
	return out
}

// Seed 预置一行任务（测试准备场景用）。
func (m *DeliveryModel) Seed(d *model.NotificationDelivery) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *d
	m.rows[cp.DeliveryId] = &cp
}

func (m *DeliveryModel) clone(d *model.NotificationDelivery) *model.NotificationDelivery {
	cp := *d
	return &cp
}

// Insert 实现 model.NotificationDeliveryModel。
func (m *DeliveryModel) Insert(_ context.Context, d *model.NotificationDelivery) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.BizKey == d.BizKey {
			m.Duplicates++
			return model.ErrDuplicateBizKey
		}
	}
	cp := *d
	m.rows[cp.DeliveryId] = &cp
	return nil
}

// FindOne 实现 model.NotificationDeliveryModel。
func (m *DeliveryModel) FindOne(_ context.Context, deliveryID string) (*model.NotificationDelivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[deliveryID]
	if !ok {
		return nil, nil
	}
	return m.clone(r), nil
}

// FindByBizKey 实现 model.NotificationDeliveryModel。
func (m *DeliveryModel) FindByBizKey(_ context.Context, bizKey string) (*model.NotificationDelivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.BizKey == bizKey {
			return m.clone(r), nil
		}
	}
	return nil, nil
}

// ListDue 实现 model.NotificationDeliveryModel：
// state ∈ {pending, retry} 且 next_retry_at <= now 且未过期，按优先级降序、next_retry_at 升序。
func (m *DeliveryModel) ListDue(_ context.Context, now int64, limit int32) ([]*model.NotificationDelivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit < 1 {
		limit = 64
	}
	var out []*model.NotificationDelivery
	for _, r := range m.rows {
		if r.State != model.DeliveryStatePending && r.State != model.DeliveryStateRetry {
			continue
		}
		if r.NextRetryAt > now {
			continue
		}
		if r.ExpireAt > 0 && r.ExpireAt <= now {
			continue
		}
		out = append(out, m.clone(r))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		if out[i].NextRetryAt != out[j].NextRetryAt {
			return out[i].NextRetryAt < out[j].NextRetryAt
		}
		return out[i].DeliveryId < out[j].DeliveryId
	})
	if int32(len(out)) > limit {
		out = out[:limit]
	}
	return out, nil
}

// update 应用一次带源状态守卫的状态写入。
func (m *DeliveryModel) update(deliveryID string, from []int32, apply func(*model.NotificationDelivery)) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[deliveryID]
	if !ok {
		return false, fmt.Errorf("%w: delivery=%s", model.ErrNotFound, deliveryID)
	}
	if !containsState(from, r.State) {
		m.IllegalTransitions++
		return false, nil
	}
	apply(r)
	r.Mtime = model.NowUnix()
	return true, nil
}

// MarkSent 实现 model.NotificationDeliveryModel。
func (m *DeliveryModel) MarkSent(_ context.Context, deliveryID, prov, providerMsgID, payloadDigest string, sentAt int64, from []int32) (bool, error) {
	return m.update(deliveryID, from, func(r *model.NotificationDelivery) {
		r.State = model.DeliveryStateSent
		r.Provider = prov
		r.ProviderMsgId = providerMsgID
		r.PayloadDigest = payloadDigest
		r.SentAt = sentAt
		r.LastError = ""
		r.NextRetryAt = 0
	})
}

// MarkRetry 实现 model.NotificationDeliveryModel。
func (m *DeliveryModel) MarkRetry(_ context.Context, deliveryID string, retryCount int32, nextRetryAt int64, lastError string, from []int32) (bool, error) {
	return m.update(deliveryID, from, func(r *model.NotificationDelivery) {
		r.State = model.DeliveryStateRetry
		r.RetryCount = retryCount
		r.NextRetryAt = nextRetryAt
		r.LastError = lastError
	})
}

// MarkFailed 实现 model.NotificationDeliveryModel。
func (m *DeliveryModel) MarkFailed(_ context.Context, deliveryID, lastError string, from []int32) (bool, error) {
	return m.update(deliveryID, from, func(r *model.NotificationDelivery) {
		r.State = model.DeliveryStateFailed
		r.LastError = lastError
		r.NextRetryAt = 0
	})
}

// MarkDeadLetter 实现 model.NotificationDeliveryModel。
func (m *DeliveryModel) MarkDeadLetter(_ context.Context, deliveryID, lastError string, from []int32) (bool, error) {
	return m.update(deliveryID, from, func(r *model.NotificationDelivery) {
		r.State = model.DeliveryStateDeadLetter
		r.LastError = lastError
		r.NextRetryAt = 0
	})
}

// MarkSuppressed 实现 model.NotificationDeliveryModel。
func (m *DeliveryModel) MarkSuppressed(_ context.Context, deliveryID, reason string, from []int32) (bool, error) {
	return m.update(deliveryID, from, func(r *model.NotificationDelivery) {
		r.State = model.DeliveryStateSuppressed
		r.LastError = reason
		r.NextRetryAt = 0
	})
}

// ResetForDeadLetterRetry 实现 model.NotificationDeliveryModel。
func (m *DeliveryModel) ResetForDeadLetterRetry(_ context.Context, deliveryID string) (bool, error) {
	return m.update(deliveryID, []int32{model.DeliveryStateDeadLetter}, func(r *model.NotificationDelivery) {
		r.State = model.DeliveryStatePending
		r.RetryCount = 0
		r.NextRetryAt = 0
		r.LastError = ""
	})
}

// List 实现 model.NotificationDeliveryModel（按 ctime 倒序，测试不覆盖全部过滤组合）。
func (m *DeliveryModel) List(_ context.Context, f model.DeliveryFilter, pn, ps int32) ([]*model.NotificationDelivery, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ps > 100 {
		return nil, 0, model.ErrPsTooLarge
	}
	var out []*model.NotificationDelivery
	for _, r := range m.rows {
		if f.Mid > 0 && r.Mid != f.Mid {
			continue
		}
		if f.Channel != 0 && r.Channel != f.Channel {
			continue
		}
		if f.State != 0 && r.State != f.State {
			continue
		}
		if f.BizKey != "" && r.BizKey != f.BizKey {
			continue
		}
		if f.BizGroupKey != "" && r.BizGroupKey != f.BizGroupKey {
			continue
		}
		if f.SourceEventId != "" && r.SourceEventId != f.SourceEventId {
			continue
		}
		if f.StartCtime > 0 && r.Ctime < f.StartCtime {
			continue
		}
		if f.EndCtime > 0 && r.Ctime > f.EndCtime {
			continue
		}
		out = append(out, m.clone(r))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ctime != out[j].Ctime {
			return out[i].Ctime > out[j].Ctime
		}
		return out[i].DeliveryId > out[j].DeliveryId
	})
	total := int64(len(out))
	if total == 0 {
		return nil, 0, nil
	}
	if pn < 1 {
		pn = 1
	}
	if ps < 1 {
		ps = 20
	}
	start := int(pn-1) * int(ps)
	if start >= len(out) {
		return nil, total, nil
	}
	end := start + int(ps)
	if end > len(out) {
		end = len(out)
	}
	return out[start:end], total, nil
}

// ListBySourceEvent 实现 model.NotificationDeliveryModel。
func (m *DeliveryModel) ListBySourceEvent(_ context.Context, eventID string, states []int32) ([]*model.NotificationDelivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*model.NotificationDelivery
	for _, r := range m.rows {
		if r.SourceEventId != eventID || eventID == "" {
			continue
		}
		if len(states) > 0 && !containsState(states, r.State) {
			continue
		}
		out = append(out, m.clone(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ctime < out[j].Ctime })
	return out, nil
}

// --- 模板 ---

// TemplateModel 是 notification_template 的内存实现（含版本与发布语义）。
type TemplateModel struct {
	mu   sync.Mutex
	rows []*model.NotificationTemplate
	next int64
}

// NewTemplateModel 构造空模板表。
func NewTemplateModel() *TemplateModel { return &TemplateModel{next: 1} }

// Rows 返回全部模板版本的快照（按 id 升序），供用例核对「校验不通过时一行都没落库」。
func (m *TemplateModel) Rows() []*model.NotificationTemplate {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*model.NotificationTemplate, 0, len(m.rows))
	for _, r := range m.rows {
		cp := *r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// Seed 预置一个模板版本。
func (m *TemplateModel) Seed(t *model.NotificationTemplate) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *t
	if cp.Id == 0 {
		cp.Id = m.next
		m.next++
	}
	m.rows = append(m.rows, &cp)
}

// Insert 实现 model.NotificationTemplateModel。
func (m *TemplateModel) Insert(_ context.Context, t *model.NotificationTemplate) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.TemplateCode == t.TemplateCode && r.Channel == t.Channel && r.Lang == t.Lang && r.Version == t.Version {
			return 0, model.ErrDuplicateBizKey
		}
	}
	cp := *t
	cp.Id = m.next
	m.next++
	m.rows = append(m.rows, &cp)
	t.Id = cp.Id
	return cp.Id, nil
}

func (m *TemplateModel) match(t *model.NotificationTemplate, code string, channel int32, lang string, version int32) bool {
	return t.TemplateCode == code && t.Channel == channel && t.Lang == lang && (version <= 0 || t.Version == version)
}

// Find 实现 model.NotificationTemplateModel。
func (m *TemplateModel) Find(_ context.Context, code string, channel int32, lang string, version int32) (*model.NotificationTemplate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *model.NotificationTemplate
	for _, r := range m.rows {
		if !m.match(r, code, channel, lang, version) {
			continue
		}
		if best == nil || r.Version > best.Version {
			best = r
		}
	}
	if best == nil {
		return nil, nil
	}
	cp := *best
	return &cp, nil
}

// FindByState 实现 model.NotificationTemplateModel。
func (m *TemplateModel) FindByState(_ context.Context, code string, channel int32, lang string, state int32) (*model.NotificationTemplate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *model.NotificationTemplate
	for _, r := range m.rows {
		if r.TemplateCode != code || r.Channel != channel || r.Lang != lang || r.State != state {
			continue
		}
		if best == nil || r.Version > best.Version {
			best = r
		}
	}
	if best == nil {
		return nil, nil
	}
	cp := *best
	return &cp, nil
}

// FindByID 实现 model.NotificationTemplateModel。
func (m *TemplateModel) FindByID(_ context.Context, id int64) (*model.NotificationTemplate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.Id == id {
			cp := *r
			return &cp, nil
		}
	}
	return nil, nil
}

// MaxVersion 实现 model.NotificationTemplateModel。
func (m *TemplateModel) MaxVersion(_ context.Context, code string, channel int32, lang string) (int32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var max int32
	for _, r := range m.rows {
		if r.TemplateCode == code && r.Channel == channel && r.Lang == lang && r.Version > max {
			max = r.Version
		}
	}
	return max, nil
}

// UpdateDraftContent 实现 model.NotificationTemplateModel。
func (m *TemplateModel) UpdateDraftContent(_ context.Context, id int64, title, body, operator string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.Id == id && r.State == model.TemplateStateDraft {
			r.TitleTpl, r.BodyTpl = title, body
			if operator != "" {
				r.Operator = operator
			}
			r.Mtime = model.NowUnix()
			return nil
		}
	}
	return model.ErrIllegalStateTransition
}

// PublishDraft 实现 model.NotificationTemplateModel（同键旧已发布版本下线）。
// 与生产 SQL 对齐：目标行的 state、operator、mtime 三列同时更新，
// 少写 operator 会让「发布人留痕」这类断言在假件上永远成立、在真库上失败。
func (m *TemplateModel) PublishDraft(_ context.Context, id int64, operator string) (*model.NotificationTemplate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var target *model.NotificationTemplate
	for _, r := range m.rows {
		if r.Id == id {
			target = r
		}
	}
	if target == nil {
		return nil, model.ErrTemplateNotFound
	}
	if target.State != model.TemplateStateDraft {
		return nil, model.ErrIllegalStateTransition
	}
	for _, r := range m.rows {
		if r.TemplateCode == target.TemplateCode && r.Channel == target.Channel && r.Lang == target.Lang &&
			r.State == model.TemplateStatePublished {
			r.State = model.TemplateStateOffline
			r.Mtime = model.NowUnix()
		}
	}
	target.State = model.TemplateStatePublished
	if operator != "" {
		target.Operator = operator
	}
	target.Mtime = model.NowUnix()
	cp := *target
	return &cp, nil
}

// List 实现 model.NotificationTemplateModel。
func (m *TemplateModel) List(_ context.Context, f model.TemplateFilter, pn, ps int32) ([]*model.NotificationTemplate, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ps > 100 {
		return nil, 0, model.ErrPsTooLarge
	}
	var out []*model.NotificationTemplate
	for _, r := range m.rows {
		if f.TemplateCode != "" && r.TemplateCode != f.TemplateCode {
			continue
		}
		if f.Channel != 0 && r.Channel != f.Channel {
			continue
		}
		if f.Lang != "" && r.Lang != f.Lang {
			continue
		}
		if f.State != 0 && r.State != f.State {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TemplateCode != out[j].TemplateCode {
			return out[i].TemplateCode < out[j].TemplateCode
		}
		return out[i].Version > out[j].Version
	})
	total := int64(len(out))
	if total == 0 {
		return nil, 0, nil
	}
	if pn < 1 {
		pn = 1
	}
	if ps < 1 {
		ps = 20
	}
	start := int(pn-1) * int(ps)
	if start >= len(out) {
		return nil, total, nil
	}
	end := start + int(ps)
	if end > len(out) {
		end = len(out)
	}
	return out[start:end], total, nil
}

// --- 事件消费状态 ---

// OffsetModel 是 notification_consumer_offset 的内存实现（event_id 唯一）。
type OffsetModel struct {
	mu   sync.Mutex
	rows map[string]*model.NotificationConsumerOffset

	// Duplicates 记录按 event_id 去重挡下的重复投递次数。
	Duplicates int
}

// NewOffsetModel 构造空事件状态表。
func NewOffsetModel() *OffsetModel {
	return &OffsetModel{rows: map[string]*model.NotificationConsumerOffset{}}
}

// Rows 返回快照（测试断言用）。
func (m *OffsetModel) Rows() map[string]*model.NotificationConsumerOffset {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]*model.NotificationConsumerOffset, len(m.rows))
	for k, v := range m.rows {
		cp := *v
		out[k] = &cp
	}
	return out
}

// Seed 预置一行事件状态。
func (m *OffsetModel) Seed(o *model.NotificationConsumerOffset) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *o
	m.rows[cp.EventId] = &cp
}

// InsertIfAbsent 实现 model.NotificationConsumerOffsetModel。
func (m *OffsetModel) InsertIfAbsent(_ context.Context, o *model.NotificationConsumerOffset) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[o.EventId]; ok {
		m.Duplicates++
		return false, nil
	}
	cp := *o
	m.rows[cp.EventId] = &cp
	return true, nil
}

// FindOne 实现 model.NotificationConsumerOffsetModel。
func (m *OffsetModel) FindOne(_ context.Context, eventID string) (*model.NotificationConsumerOffset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[eventID]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

// MarkState 实现 model.NotificationConsumerOffsetModel。
func (m *OffsetModel) MarkState(_ context.Context, eventID string, to int32, retryCount int32,
	nextRetryAt int64, lastError string, from []int32) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[eventID]
	if !ok {
		return false, fmt.Errorf("%w: event=%s", model.ErrNotFound, eventID)
	}
	if !containsState(from, r.State) {
		return false, nil
	}
	r.State = to
	r.RetryCount = retryCount
	r.LastError = lastError
	switch to {
	case model.EventStateRetry:
		r.NextRetryAt = nextRetryAt
	case model.EventStateSucceeded:
		r.NextRetryAt = 0
		r.PayloadJson = ""
	default:
		r.NextRetryAt = 0
	}
	r.Mtime = model.NowUnix()
	return true, nil
}

// ListDue 实现 model.NotificationConsumerOffsetModel。
func (m *OffsetModel) ListDue(_ context.Context, now int64, limit int32) ([]*model.NotificationConsumerOffset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit < 1 {
		limit = 64
	}
	var out []*model.NotificationConsumerOffset
	for _, r := range m.rows {
		if r.State != model.EventStateRetry || r.NextRetryAt > now {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NextRetryAt < out[j].NextRetryAt })
	if int32(len(out)) > limit {
		out = out[:limit]
	}
	return out, nil
}

// --- 死信 ---

// DeadLetterModel 是 notification_dead_letter 的内存实现。
type DeadLetterModel struct {
	mu   sync.Mutex
	rows map[int64]*model.NotificationDeadLetter
	next int64

	// Duplicates 记录 (source, event_id, delivery_id) 唯一键挡下的重复登记。
	Duplicates int
}

// NewDeadLetterModel 构造空死信表。
func NewDeadLetterModel() *DeadLetterModel {
	return &DeadLetterModel{rows: map[int64]*model.NotificationDeadLetter{}, next: 1}
}

// Rows 返回全部死信（测试断言用）。
func (m *DeadLetterModel) Rows() []*model.NotificationDeadLetter {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*model.NotificationDeadLetter, 0, len(m.rows))
	for _, r := range m.rows {
		cp := *r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// Seed 预置一条死信并返回其主键，供用例引用「已存在的死信记录」。
// 与 InsertIfAbsent 的区别：Seed 不做 (source, event_id, delivery_id) 唯一键判定，
// 因此可以按测试意图铺出任意状态的死信（例如已被别的运营处置成 discarded）；
// 主键仍像真实自增列一样由假件分配，用例不得手写 id=0。
func (m *DeadLetterModel) Seed(d *model.NotificationDeadLetter) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *d
	if cp.Id == 0 {
		cp.Id = m.next
		m.next++
	} else if cp.Id >= m.next {
		// 用例显式给了主键时抬高自增位，避免下一条 Seed 撞 id。
		m.next = cp.Id + 1
	}
	m.rows[cp.Id] = &cp
	return cp.Id
}

// InsertIfAbsent 实现 model.NotificationDeadLetterModel。
func (m *DeadLetterModel) InsertIfAbsent(_ context.Context, d *model.NotificationDeadLetter) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.Source == d.Source && r.EventId == d.EventId && r.DeliveryId == d.DeliveryId {
			m.Duplicates++
			return false, nil
		}
	}
	cp := *d
	cp.Id = m.next
	m.next++
	m.rows[cp.Id] = &cp
	d.Id = cp.Id
	return true, nil
}

// FindOne 实现 model.NotificationDeadLetterModel。
func (m *DeadLetterModel) FindOne(_ context.Context, id int64) (*model.NotificationDeadLetter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

// FindByDelivery 实现 model.NotificationDeadLetterModel。
func (m *DeadLetterModel) FindByDelivery(_ context.Context, deliveryID string) (*model.NotificationDeadLetter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *model.NotificationDeadLetter
	for _, r := range m.rows {
		if r.DeliveryId == deliveryID && r.Source == model.DeadLetterSourceDelivery {
			if best == nil || r.Id > best.Id {
				best = r
			}
		}
	}
	if best == nil {
		return nil, nil
	}
	cp := *best
	return &cp, nil
}

// MarkState 实现 model.NotificationDeadLetterModel。
func (m *DeadLetterModel) MarkState(_ context.Context, id int64, to int32, operator string, from []int32) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return false, fmt.Errorf("%w: dead letter=%d", model.ErrNotFound, id)
	}
	if !containsState(from, r.State) {
		return false, nil
	}
	r.State = to
	r.Operator = operator
	r.Mtime = model.NowUnix()
	return true, nil
}

// List 实现 model.NotificationDeadLetterModel。
func (m *DeadLetterModel) List(_ context.Context, f model.DeadLetterFilter, pn, ps int32) ([]*model.NotificationDeadLetter, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ps > 100 {
		return nil, 0, model.ErrPsTooLarge
	}
	var out []*model.NotificationDeadLetter
	for _, r := range m.rows {
		if f.EventId != "" && r.EventId != f.EventId {
			continue
		}
		if f.Topic != "" && r.Topic != f.Topic {
			continue
		}
		if f.Source != "" && r.Source != f.Source {
			continue
		}
		if f.State != 0 && r.State != f.State {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ctime != out[j].Ctime {
			return out[i].Ctime > out[j].Ctime
		}
		return out[i].Id > out[j].Id
	})
	total := int64(len(out))
	if total == 0 {
		return nil, 0, nil
	}
	if pn < 1 {
		pn = 1
	}
	if ps < 1 {
		ps = 20
	}
	start := int(pn-1) * int(ps)
	if start >= len(out) {
		return nil, total, nil
	}
	end := start + int(ps)
	if end > len(out) {
		end = len(out)
	}
	return out[start:end], total, nil
}

// --- 用户偏好 ---

// DndPrefModel 是 notification_dnd_pref 的内存实现（mid 主键，全量覆盖）。
type DndPrefModel struct {
	mu   sync.Mutex
	rows map[int64]*model.NotificationDndPref
}

// NewDndPrefModel 构造空偏好表。
func NewDndPrefModel() *DndPrefModel {
	return &DndPrefModel{rows: map[int64]*model.NotificationDndPref{}}
}

// Seed 预置一行偏好。
func (m *DndPrefModel) Seed(p *model.NotificationDndPref) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *p
	m.rows[cp.Mid] = &cp
}

// Upsert 实现 model.NotificationDndPrefModel。
func (m *DndPrefModel) Upsert(_ context.Context, p *model.NotificationDndPref) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *p
	if old, ok := m.rows[cp.Mid]; ok {
		cp.Ctime = old.Ctime
	}
	m.rows[cp.Mid] = &cp
	return nil
}

// FindOne 实现 model.NotificationDndPrefModel。
func (m *DndPrefModel) FindOne(_ context.Context, mid int64) (*model.NotificationDndPref, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[mid]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

// --- 通道适配器 ---

// Provider 是可编排的假通道适配器。
type Provider struct {
	// Chan 该适配器负责的通道。
	Chan string
	// AdapterName 落库的适配器名。
	AdapterName string
	// Err 非空时 Send 返回该错误（模拟供应商拒绝/超时）。
	Err error
	// Accepted 控制回执是否“已受理”（false 用于验证“未知结果不得写成已发送”）。
	Accepted bool
	// MsgID 回执消息 ID。
	MsgID string
	// NilResult 为 true 时返回 (nil, nil)：模拟适配器行为异常、没有任何回执。
	NilResult bool

	mu    sync.Mutex
	Calls int
	Reqs  []*provider.SendRequest
}

// Name 实现 provider.Provider。
func (p *Provider) Name() string {
	if p.AdapterName != "" {
		return p.AdapterName
	}
	return "fake-" + p.Chan
}

// Channel 实现 provider.Provider。
func (p *Provider) Channel() string { return p.Chan }

// Send 实现 provider.Provider。
// Err 非空时返回错误（供应商拒绝/超时）；NilResult 时返回 (nil, nil)（适配器无回执）；
// 否则返回回执，是否“已受理”由 Accepted 决定 —— 用于验证“结果未知绝不能写成已发送”。
func (p *Provider) Send(_ context.Context, req *provider.SendRequest) (*provider.SendResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls++
	p.Reqs = append(p.Reqs, req)
	if p.Err != nil {
		return nil, p.Err
	}
	if p.NilResult {
		return nil, nil
	}
	return &provider.SendResult{
		Provider:      p.Name(),
		ProviderMsgID: p.MsgID,
		Status:        200,
		Accepted:      p.Accepted,
	}, nil
}

// Snapshot 返回调用次数与最后一次请求（测试断言用）。
func (p *Provider) Snapshot() (int, *provider.SendRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.Reqs) == 0 {
		return p.Calls, nil
	}
	return p.Calls, p.Reqs[len(p.Reqs)-1]
}

// containsState 判断源状态集合是否命中当前状态。
func containsState(states []int32, state int32) bool {
	for _, s := range states {
		if s == state {
			return true
		}
	}
	return false
}
