// trace.go 给 fakes.go 的内存假件套一层「有序调用轨迹 + 按方法注入存储错误」的透明代理，
// 供 internal/logic 的用例断言两类光看返回值证明不了的不变量：
//
//   - 顺序：RetryDeadLetter 必须「先把任务复位、再把死信标记为已重投」，
//     反过来的话标记成功而复位失败就再也无法通过该接口恢复；
//   - 次数：配额不可用时不得触达 notification_delivery 插入、幂等命中不得二次外发。
//
// 为什么不直接在 fakes.go 的方法里加埋点：那 5 个假件同时被 internal/send、
// internal/consumer 的用例使用，改签名或改行为会让既有测试全量返工。
// Spy 只记录与注入，**不持有状态**（状态仍在 fakes.go 的假件里，保持单一假件源）；
// Seed/Rows 这类测试辅助方法不经过 Spy，所以「预热必须静默」是结构上成立的，
// 不需要每个用例手动清理轨迹。
//
// 轨迹条目格式：<假件短名>.<方法>:<定位键>，定位键只用确定性字段
// （模板码、mid、死信 id、请求级 biz_group_key）；ULID 主键与 sha256 行键不进轨迹，
// 否则断言会退化成「和随机数比较」。
package testx

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"go-video/services/notification/internal/repository"
	"go-video/services/notification/model"
)

// --- 调用轨迹 ---

// Trace 是跨假件共享的有序调用轨迹。nil 轨迹是空操作，
// 因此未挂轨迹的用例可以直接复用假件而不必改代码。
type Trace struct {
	mu  sync.Mutex
	ops []string
}

// NewTrace 构造空轨迹。
func NewTrace() *Trace { return &Trace{} }

// add 追加一条记录。
func (tr *Trace) add(format string, args ...any) {
	if tr == nil {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.ops = append(tr.ops, fmt.Sprintf(format, args...))
}

// Ops 返回轨迹快照（值拷贝，调用方改动不会污染轨迹本身）。
func (tr *Trace) Ops() []string {
	if tr == nil {
		return nil
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	out := make([]string, len(tr.ops))
	copy(out, tr.ops)
	return out
}

// Mark 返回当前轨迹长度，供用例只断言「这次调用之后」发生的部分。
func (tr *Trace) Mark() int {
	if tr == nil {
		return 0
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return len(tr.ops)
}

// OpsFrom 返回第 from 条之后的调用；from 取 Mark() 的返回值。
func (tr *Trace) OpsFrom(from int) []string {
	ops := tr.Ops()
	if from >= len(ops) {
		return nil
	}
	if from < 0 {
		from = 0
	}
	return ops[from:]
}

// Count 统计完全相等的调用条数。
func (tr *Trace) Count(op string) int {
	n := 0
	for _, o := range tr.Ops() {
		if o == op {
			n++
		}
	}
	return n
}

// CountPrefix 统计以 prefix 开头的调用数，用于「这一类操作一次都没发生」的断言。
func (tr *Trace) CountPrefix(prefix string) int {
	n := 0
	for _, o := range tr.Ops() {
		if strings.HasPrefix(o, prefix) {
			n++
		}
	}
	return n
}

// --- Spy 公共部分 ---

// spy 记录轨迹并持有按方法名注入的错误表。
type spy struct {
	tr      *Trace
	faults  map[string]error
	befores map[string]func()
}

func newSpy(tr *Trace) spy { return spy{tr: tr, faults: map[string]error{}} }

// fail 登记某个方法名的注入错误（由各 Spy 的导出 Fail 调用）。
func (s *spy) fail(method string, err error) {
	if s.faults == nil {
		s.faults = map[string]error{}
	}
	s.faults[method] = err
}

// clear 撤掉某个方法名的注入错误（由各 Spy 的导出 Recover 调用）。
// 有了它，「同一条链路先失败后恢复」可以在一个用例里连续断言：
// 失败时的守卫行为 + 恢复后的正确行为，不必为此再造第二个环境。
func (s *spy) clear(method string) {
	if s.faults == nil {
		return
	}
	delete(s.faults, method)
}

// fault 返回注入错误；像真实 model 那样再包一层 %w，
// 这样用例可以证明 logic 传播的是错误链而不只是 nil / 非 nil。
func (s *spy) fault(method string) error {
	err, ok := s.faults[method]
	if !ok || err == nil {
		return nil
	}
	return fmt.Errorf("testx injected failure for %s: %w", method, err)
}

// before 登记「真正落到假件之前」要执行的回调。
// 用于模拟并发：本次写入发生前，另一个操作人/协程已经改掉了同一行，
// 于是带源状态守卫的写入会返回 (false, nil)——「看起来成功了但其实没写」这条分支
// 只能靠这种时机插桩才测得到（Fail 只能造错误，造不出 false+nil）。
func (s *spy) before(method string, fn func()) {
	if s.befores == nil {
		s.befores = map[string]func(){}
	}
	s.befores[method] = fn
}

// runBefore 执行登记的回调；未登记时是空操作。
func (s *spy) runBefore(method string) {
	if fn, ok := s.befores[method]; ok && fn != nil {
		fn()
	}
}

// --- 模板 ---

// TemplateSpy 观测并干扰 notification_template 的读写。
type TemplateSpy struct {
	spy
	inner model.NotificationTemplateModel
}

// SpyTemplate 包住真实假件；tr 为 nil 时只保留错误注入能力。
func SpyTemplate(m model.NotificationTemplateModel, tr *Trace) *TemplateSpy {
	return &TemplateSpy{spy: newSpy(tr), inner: m}
}

// Fail 让方法 method 在下一次调用时返回包装后的 err，返回自身便于链式写法。
func (s *TemplateSpy) Fail(method string, err error) *TemplateSpy {
	s.fail(method, err)
	return s
}

// Insert 实现 model.NotificationTemplateModel。
func (s *TemplateSpy) Insert(ctx context.Context, t *model.NotificationTemplate) (int64, error) {
	s.tr.add("tmpl.Insert:%s/%d/%s/v%d", t.TemplateCode, t.Channel, t.Lang, t.Version)
	if err := s.fault("Insert"); err != nil {
		return 0, err
	}
	return s.inner.Insert(ctx, t)
}

// Find 实现 model.NotificationTemplateModel。
func (s *TemplateSpy) Find(ctx context.Context, code string, channel int32, lang string,
	version int32) (*model.NotificationTemplate, error) {
	s.tr.add("tmpl.Find:%s/%d/%s/v%d", code, channel, lang, version)
	if err := s.fault("Find"); err != nil {
		return nil, err
	}
	return s.inner.Find(ctx, code, channel, lang, version)
}

// FindByState 实现 model.NotificationTemplateModel。
func (s *TemplateSpy) FindByState(ctx context.Context, code string, channel int32, lang string,
	state int32) (*model.NotificationTemplate, error) {
	s.tr.add("tmpl.FindByState:%s/%d/%s/st%d", code, channel, lang, state)
	if err := s.fault("FindByState"); err != nil {
		return nil, err
	}
	return s.inner.FindByState(ctx, code, channel, lang, state)
}

// FindByID 实现 model.NotificationTemplateModel。
func (s *TemplateSpy) FindByID(ctx context.Context, id int64) (*model.NotificationTemplate, error) {
	s.tr.add("tmpl.FindByID:%d", id)
	if err := s.fault("FindByID"); err != nil {
		return nil, err
	}
	return s.inner.FindByID(ctx, id)
}

// MaxVersion 实现 model.NotificationTemplateModel。
func (s *TemplateSpy) MaxVersion(ctx context.Context, code string, channel int32,
	lang string) (int32, error) {
	s.tr.add("tmpl.MaxVersion:%s/%d/%s", code, channel, lang)
	if err := s.fault("MaxVersion"); err != nil {
		return 0, err
	}
	return s.inner.MaxVersion(ctx, code, channel, lang)
}

// UpdateDraftContent 实现 model.NotificationTemplateModel。
func (s *TemplateSpy) UpdateDraftContent(ctx context.Context, id int64, title, body,
	operator string) error {
	s.tr.add("tmpl.UpdateDraft:%d", id)
	if err := s.fault("UpdateDraftContent"); err != nil {
		return err
	}
	return s.inner.UpdateDraftContent(ctx, id, title, body, operator)
}

// PublishDraft 实现 model.NotificationTemplateModel。
func (s *TemplateSpy) PublishDraft(ctx context.Context, id int64, operator string) (*model.NotificationTemplate, error) {
	s.tr.add("tmpl.PublishDraft:%d", id)
	if err := s.fault("PublishDraft"); err != nil {
		return nil, err
	}
	return s.inner.PublishDraft(ctx, id, operator)
}

// List 实现 model.NotificationTemplateModel。
func (s *TemplateSpy) List(ctx context.Context, f model.TemplateFilter, pn,
	ps int32) ([]*model.NotificationTemplate, int64, error) {
	s.tr.add("tmpl.List:code=%s/ch%d/lang=%s/st%d/pn%d/ps%d", f.TemplateCode, f.Channel, f.Lang, f.State, pn, ps)
	if err := s.fault("List"); err != nil {
		return nil, 0, err
	}
	return s.inner.List(ctx, f, pn, ps)
}

// --- 投递任务 ---

// DeliverySpy 观测并干扰 notification_delivery 的读写。
type DeliverySpy struct {
	spy
	inner model.NotificationDeliveryModel
}

// SpyDelivery 包住真实假件。
func SpyDelivery(m model.NotificationDeliveryModel, tr *Trace) *DeliverySpy {
	return &DeliverySpy{spy: newSpy(tr), inner: m}
}

// Fail 让方法 method 下一次调用返回包装后的 err。
func (s *DeliverySpy) Fail(method string, err error) *DeliverySpy {
	s.fail(method, err)
	return s
}

// Insert 实现 model.NotificationDeliveryModel（轨迹用请求级 biz_group_key，行级 biz_key 是 sha256 不可读）。
func (s *DeliverySpy) Insert(ctx context.Context, d *model.NotificationDelivery) error {
	s.tr.add("deliv.Insert:gk=%s/mid%d", d.BizGroupKey, d.Mid)
	if err := s.fault("Insert"); err != nil {
		return err
	}
	return s.inner.Insert(ctx, d)
}

// FindOne 实现 model.NotificationDeliveryModel。
func (s *DeliverySpy) FindOne(ctx context.Context, deliveryID string) (*model.NotificationDelivery, error) {
	s.tr.add("deliv.FindOne:%s", deliveryID)
	if err := s.fault("FindOne"); err != nil {
		return nil, err
	}
	return s.inner.FindOne(ctx, deliveryID)
}

// FindByBizKey 实现 model.NotificationDeliveryModel。
func (s *DeliverySpy) FindByBizKey(ctx context.Context, bizKey string) (*model.NotificationDelivery, error) {
	s.tr.add("deliv.FindByBizKey")
	if err := s.fault("FindByBizKey"); err != nil {
		return nil, err
	}
	return s.inner.FindByBizKey(ctx, bizKey)
}

// ListDue 实现 model.NotificationDeliveryModel。
func (s *DeliverySpy) ListDue(ctx context.Context, now int64, limit int32) ([]*model.NotificationDelivery, error) {
	s.tr.add("deliv.ListDue:%d/%d", now, limit)
	if err := s.fault("ListDue"); err != nil {
		return nil, err
	}
	return s.inner.ListDue(ctx, now, limit)
}

// MarkSent 实现 model.NotificationDeliveryModel。
func (s *DeliverySpy) MarkSent(ctx context.Context, deliveryID, prov, providerMsgID,
	payloadDigest string, sentAt int64, from []int32) (bool, error) {
	s.tr.add("deliv.MarkSent:%s", deliveryID)
	if err := s.fault("MarkSent"); err != nil {
		return false, err
	}
	return s.inner.MarkSent(ctx, deliveryID, prov, providerMsgID, payloadDigest, sentAt, from)
}

// MarkRetry 实现 model.NotificationDeliveryModel。
func (s *DeliverySpy) MarkRetry(ctx context.Context, deliveryID string, retryCount int32,
	nextRetryAt int64, lastError string, from []int32) (bool, error) {
	s.tr.add("deliv.MarkRetry:%s", deliveryID)
	if err := s.fault("MarkRetry"); err != nil {
		return false, err
	}
	return s.inner.MarkRetry(ctx, deliveryID, retryCount, nextRetryAt, lastError, from)
}

// MarkFailed 实现 model.NotificationDeliveryModel。
func (s *DeliverySpy) MarkFailed(ctx context.Context, deliveryID, lastError string, from []int32) (bool, error) {
	s.tr.add("deliv.MarkFailed:%s", deliveryID)
	if err := s.fault("MarkFailed"); err != nil {
		return false, err
	}
	return s.inner.MarkFailed(ctx, deliveryID, lastError, from)
}

// MarkDeadLetter 实现 model.NotificationDeliveryModel。
func (s *DeliverySpy) MarkDeadLetter(ctx context.Context, deliveryID, lastError string, from []int32) (bool, error) {
	s.tr.add("deliv.MarkDeadLetter:%s", deliveryID)
	if err := s.fault("MarkDeadLetter"); err != nil {
		return false, err
	}
	return s.inner.MarkDeadLetter(ctx, deliveryID, lastError, from)
}

// MarkSuppressed 实现 model.NotificationDeliveryModel。
func (s *DeliverySpy) MarkSuppressed(ctx context.Context, deliveryID, reason string, from []int32) (bool, error) {
	s.tr.add("deliv.MarkSuppressed:%s", deliveryID)
	if err := s.fault("MarkSuppressed"); err != nil {
		return false, err
	}
	return s.inner.MarkSuppressed(ctx, deliveryID, reason, from)
}

// ResetForDeadLetterRetry 实现 model.NotificationDeliveryModel。
func (s *DeliverySpy) ResetForDeadLetterRetry(ctx context.Context, deliveryID string) (bool, error) {
	s.tr.add("deliv.ResetForDeadLetterRetry:%s", deliveryID)
	if err := s.fault("ResetForDeadLetterRetry"); err != nil {
		return false, err
	}
	return s.inner.ResetForDeadLetterRetry(ctx, deliveryID)
}

// List 实现 model.NotificationDeliveryModel。
func (s *DeliverySpy) List(ctx context.Context, f model.DeliveryFilter, pn,
	ps int32) ([]*model.NotificationDelivery, int64, error) {
	s.tr.add("deliv.List:mid%d/ch%d/st%d/pn%d/ps%d", f.Mid, f.Channel, f.State, pn, ps)
	if err := s.fault("List"); err != nil {
		return nil, 0, err
	}
	return s.inner.List(ctx, f, pn, ps)
}

// ListBySourceEvent 实现 model.NotificationDeliveryModel。
func (s *DeliverySpy) ListBySourceEvent(ctx context.Context, eventID string,
	states []int32) ([]*model.NotificationDelivery, error) {
	s.tr.add("deliv.ListBySourceEvent:%s", eventID)
	if err := s.fault("ListBySourceEvent"); err != nil {
		return nil, err
	}
	return s.inner.ListBySourceEvent(ctx, eventID, states)
}

// --- 事件消费状态 ---

// OffsetSpy 观测并干扰 notification_consumer_offset。
type OffsetSpy struct {
	spy
	inner model.NotificationConsumerOffsetModel
}

// SpyOffset 包住真实假件。
func SpyOffset(m model.NotificationConsumerOffsetModel, tr *Trace) *OffsetSpy {
	return &OffsetSpy{spy: newSpy(tr), inner: m}
}

// Fail 让方法 method 下一次调用返回包装后的 err。
func (s *OffsetSpy) Fail(method string, err error) *OffsetSpy {
	s.fail(method, err)
	return s
}

// InsertIfAbsent 实现 model.NotificationConsumerOffsetModel。
func (s *OffsetSpy) InsertIfAbsent(ctx context.Context, o *model.NotificationConsumerOffset) (bool, error) {
	s.tr.add("offset.InsertIfAbsent:%s", o.EventId)
	if err := s.fault("InsertIfAbsent"); err != nil {
		return false, err
	}
	return s.inner.InsertIfAbsent(ctx, o)
}

// FindOne 实现 model.NotificationConsumerOffsetModel。
func (s *OffsetSpy) FindOne(ctx context.Context, eventID string) (*model.NotificationConsumerOffset, error) {
	s.tr.add("offset.FindOne:%s", eventID)
	if err := s.fault("FindOne"); err != nil {
		return nil, err
	}
	return s.inner.FindOne(ctx, eventID)
}

// MarkState 实现 model.NotificationConsumerOffsetModel。
func (s *OffsetSpy) MarkState(ctx context.Context, eventID string, to int32, retryCount int32,
	nextRetryAt int64, lastError string, from []int32) (bool, error) {
	s.tr.add("offset.MarkState:%s->%d", eventID, to)
	if err := s.fault("MarkState"); err != nil {
		return false, err
	}
	return s.inner.MarkState(ctx, eventID, to, retryCount, nextRetryAt, lastError, from)
}

// ListDue 实现 model.NotificationConsumerOffsetModel。
func (s *OffsetSpy) ListDue(ctx context.Context, now int64, limit int32) ([]*model.NotificationConsumerOffset, error) {
	s.tr.add("offset.ListDue")
	if err := s.fault("ListDue"); err != nil {
		return nil, err
	}
	return s.inner.ListDue(ctx, now, limit)
}

// --- 死信 ---

// DeadLetterSpy 观测并干扰 notification_dead_letter。
type DeadLetterSpy struct {
	spy
	inner model.NotificationDeadLetterModel
}

// SpyDeadLetter 包住真实假件。
func SpyDeadLetter(m model.NotificationDeadLetterModel, tr *Trace) *DeadLetterSpy {
	return &DeadLetterSpy{spy: newSpy(tr), inner: m}
}

// Fail 让方法 method 下一次调用返回包装后的 err。
func (s *DeadLetterSpy) Fail(method string, err error) *DeadLetterSpy {
	s.fail(method, err)
	return s
}

// Before 让方法 method 在真正落到假件之前执行 fn，用于模拟并发处置
// （例如另一个运营账号在 MarkState 之前已经把死信改成了 discarded）。
func (s *DeadLetterSpy) Before(method string, fn func()) *DeadLetterSpy {
	s.before(method, fn)
	return s
}

// InsertIfAbsent 实现 model.NotificationDeadLetterModel。
func (s *DeadLetterSpy) InsertIfAbsent(ctx context.Context, d *model.NotificationDeadLetter) (bool, error) {
	s.tr.add("dead.InsertIfAbsent:%s/%s", d.Source, d.DeliveryId)
	if err := s.fault("InsertIfAbsent"); err != nil {
		return false, err
	}
	return s.inner.InsertIfAbsent(ctx, d)
}

// FindOne 实现 model.NotificationDeadLetterModel。
func (s *DeadLetterSpy) FindOne(ctx context.Context, id int64) (*model.NotificationDeadLetter, error) {
	s.tr.add("dead.FindOne:%d", id)
	if err := s.fault("FindOne"); err != nil {
		return nil, err
	}
	return s.inner.FindOne(ctx, id)
}

// FindByDelivery 实现 model.NotificationDeadLetterModel。
func (s *DeadLetterSpy) FindByDelivery(ctx context.Context, deliveryID string) (*model.NotificationDeadLetter, error) {
	s.tr.add("dead.FindByDelivery:%s", deliveryID)
	if err := s.fault("FindByDelivery"); err != nil {
		return nil, err
	}
	return s.inner.FindByDelivery(ctx, deliveryID)
}

// MarkState 实现 model.NotificationDeadLetterModel。
// s.runBefore 在写入前执行登记的回调：RetryDeadLetter 的「并发下别的运营已经处置过」
// 分支（守卫未命中 -> false, nil）只有这种时机插桩才造得出来。
func (s *DeadLetterSpy) MarkState(ctx context.Context, id int64, to int32, operator string,
	from []int32) (bool, error) {
	s.tr.add("dead.MarkState:%d->%d", id, to)
	s.runBefore("MarkState")
	if err := s.fault("MarkState"); err != nil {
		return false, err
	}
	return s.inner.MarkState(ctx, id, to, operator, from)
}

// List 实现 model.NotificationDeadLetterModel。
func (s *DeadLetterSpy) List(ctx context.Context, f model.DeadLetterFilter, pn,
	ps int32) ([]*model.NotificationDeadLetter, int64, error) {
	s.tr.add("dead.List:evt=%s/topic=%s/src=%s/st%d/pn%d/ps%d", f.EventId, f.Topic, f.Source, f.State, pn, ps)
	if err := s.fault("List"); err != nil {
		return nil, 0, err
	}
	return s.inner.List(ctx, f, pn, ps)
}

// --- 用户偏好 ---

// DndSpy 观测并干扰 notification_dnd_pref。
type DndSpy struct {
	spy
	inner model.NotificationDndPrefModel
}

// SpyDnd 包住真实假件。
func SpyDnd(m model.NotificationDndPrefModel, tr *Trace) *DndSpy {
	return &DndSpy{spy: newSpy(tr), inner: m}
}

// Fail 让方法 method 下一次调用返回包装后的 err。
func (s *DndSpy) Fail(method string, err error) *DndSpy {
	s.fail(method, err)
	return s
}

// Upsert 实现 model.NotificationDndPrefModel。
func (s *DndSpy) Upsert(ctx context.Context, p *model.NotificationDndPref) error {
	s.tr.add("dnd.Upsert:%d", p.Mid)
	if err := s.fault("Upsert"); err != nil {
		return err
	}
	return s.inner.Upsert(ctx, p)
}

// FindOne 实现 model.NotificationDndPrefModel。
func (s *DndSpy) FindOne(ctx context.Context, mid int64) (*model.NotificationDndPref, error) {
	s.tr.add("dnd.FindOne:%d", mid)
	if err := s.fault("FindOne"); err != nil {
		return nil, err
	}
	return s.inner.FindOne(ctx, mid)
}

// --- 配额计数 ---

// QuotaSpy 观测 Redis 配额调用；失败注入仍用 QuotaCounter.Fail 字段
// （生产代码只依赖它是否为 error，不需要按方法注入）。
type QuotaSpy struct {
	tr    *Trace
	inner repository.QuotaCounter
}

// SpyQuota 包住真实计数器。
func SpyQuota(m repository.QuotaCounter, tr *Trace) *QuotaSpy {
	return &QuotaSpy{tr: tr, inner: m}
}

// IncrCtx 实现 repository.QuotaCounter（key 含用户本地日期，不进轨迹，避免断言依赖当天）。
func (s *QuotaSpy) IncrCtx(ctx context.Context, key string) (int64, error) {
	s.tr.add("quota.Incr")
	return s.inner.IncrCtx(ctx, key)
}

// ExpireCtx 实现 repository.QuotaCounter。
func (s *QuotaSpy) ExpireCtx(ctx context.Context, key string, seconds int) error {
	s.tr.add("quota.Expire:%d", seconds)
	return s.inner.ExpireCtx(ctx, key, seconds)
}

// --- 撤销注入错误 ---
//
// Recover 与 Fail 成对：用例先注入故障断言「失败时不得留下半条数据 / 不得越级推进状态」，
// 再撤销故障、重放同一请求，断言恢复后的正常链路。缺少 Recover 时第二条断言只能另起
// 一个测试环境，反而更容易漏掉「失败后状态是否还能继续推进」这条真实问题。

// Recover 撤掉方法 method 的注入错误，返回自身便于链式写法。
func (s *TemplateSpy) Recover(method string) *TemplateSpy {
	s.clear(method)
	return s
}

// Recover 撤掉方法 method 的注入错误。
func (s *DeliverySpy) Recover(method string) *DeliverySpy {
	s.clear(method)
	return s
}

// Recover 撤掉方法 method 的注入错误。
func (s *OffsetSpy) Recover(method string) *OffsetSpy {
	s.clear(method)
	return s
}

// Recover 撤掉方法 method 的注入错误。
func (s *DeadLetterSpy) Recover(method string) *DeadLetterSpy {
	s.clear(method)
	return s
}

// Recover 撤掉方法 method 的注入错误。
func (s *DndSpy) Recover(method string) *DndSpy {
	s.clear(method)
	return s
}
