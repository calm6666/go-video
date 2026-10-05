package model

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 投递任务状态：与 rpc/notification.proto 的 DeliveryState 一致。
// 合法迁移见 services/notification/internal/policy/deliverystate.go 与 README。
const (
	// DeliveryStatePending 已落库待投递。
	DeliveryStatePending int32 = 1
	// DeliveryStateSent 供应商已受理（终态）。
	DeliveryStateSent int32 = 2
	// DeliveryStateFailed 不可重试失败（终态）。
	DeliveryStateFailed int32 = 3
	// DeliveryStateRetry 等待退避重试。
	DeliveryStateRetry int32 = 4
	// DeliveryStateDeadLetter 重试耗尽，转死信（终态，可被运营重投回 Pending）。
	DeliveryStateDeadLetter int32 = 5
	// DeliveryStateSuppressed 被免打扰/频控/过期/收件人不可解析拦截，未调用供应商（终态）。
	DeliveryStateSuppressed int32 = 6
)

// NotificationDelivery 投递任务与回执（notification_delivery 表）。
// 隐私约束：不存明文手机号/邮箱与渲染后的正文，只存受控标识 target_ref、
// 渲染变量快照 params_json（由调用方保证脱敏）和渲染结果摘要 payload_digest。
// 异步投递时按 template_code + template_version + lang 重新渲染，保证内容可复现。
type NotificationDelivery struct {
	DeliveryId      string `db:"delivery_id"`      // 投递任务 ID（ULID，主键）
	BizKey          string `db:"biz_key"`          // 行级幂等键：sha256(请求级 biz_key|channel|收件人)，唯一索引
	BizGroupKey     string `db:"biz_group_key"`    // 调用方请求级业务键，用于整批查询
	Mid             int64  `db:"mid"`              // 接收人用户 ID（0 表示只有 target_ref）
	Channel         int32  `db:"channel"`          // 通道
	TemplateCode    string `db:"template_code"`    // 模板码
	TemplateVersion int32  `db:"template_version"` // 模板版本（锁定，避免投递时漂移）
	Lang            string `db:"lang"`             // 语言
	TargetRef       string `db:"target_ref"`       // 受控投递标识（设备 token / 供应商收件人引用 / 哈希）
	ParamsJson      string `db:"params_json"`      // 渲染变量快照（JSON，调用方需脱敏）
	PayloadDigest   string `db:"payload_digest"`   // 渲染结果 sha256 hex
	State           int32  `db:"state"`            // 状态，见 DeliveryState* 常量
	Provider        string `db:"provider"`         // 实际使用的通道适配器名
	ProviderMsgId   string `db:"provider_msg_id"`  // 供应商回执消息 ID
	Priority        int32  `db:"priority"`         // 优先级，1 低 2 普通 3 高
	RetryCount      int32  `db:"retry_count"`      // 已重试次数
	NextRetryAt     int64  `db:"next_retry_at"`    // 下次重试时间（Unix 秒），0 表示不再重试
	LastError       string `db:"last_error"`       // 最近一次错误（脱敏）
	SentAt          int64  `db:"sent_at"`          // 投递成功时间（Unix 秒）
	ExpireAt        int64  `db:"expire_at"`        // 过期时间（Unix 秒），0 表示不过期
	SourceEventId   string `db:"source_event_id"`  // 来源事件 ID（RPC 直投时为空）
	TraceId         string `db:"trace_id"`         // 链路 ID
	Ctime           int64  `db:"ctime"`            // 创建时间（Unix 秒）
	Mtime           int64  `db:"mtime"`            // 修改时间（Unix 秒）
}

// DeliveryFilter 投递记录列表过滤条件，零值字段表示不过滤。
type DeliveryFilter struct {
	Mid           int64
	Channel       int32
	State         int32
	BizKey        string
	BizGroupKey   string
	SourceEventId string
	StartCtime    int64
	EndCtime      int64
}

// NotificationDeliveryModel notification_delivery 表读写接口。
// 所有状态写入都带源状态守卫：UPDATE ... WHERE state IN (from...)，
// 返回 false 表示迁移不合法（未命中任何源状态），调用方必须拒绝而不是静默覆盖。
type NotificationDeliveryModel interface {
	// Insert 新建投递任务；biz_key 冲突返回 ErrDuplicateBizKey。
	Insert(ctx context.Context, d *NotificationDelivery) error
	// FindOne 按 delivery_id 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, deliveryId string) (*NotificationDelivery, error)
	// FindByBizKey 按行级幂等键查询；不存在返回 (nil, nil)。
	FindByBizKey(ctx context.Context, bizKey string) (*NotificationDelivery, error)
	// ListDue 扫描到期待投递任务（state ∈ {pending, retry} 且 next_retry_at <= now，
	// 且未过期），按优先级降序、next_retry_at 升序。
	ListDue(ctx context.Context, now int64, limit int32) ([]*NotificationDelivery, error)
	// MarkSent 置为已发送并记录供应商回执。
	MarkSent(ctx context.Context, deliveryId, provider, providerMsgId, payloadDigest string, sentAt int64, from []int32) (bool, error)
	// MarkRetry 置为退避重试并更新重试计数、下次重试时间与错误。
	MarkRetry(ctx context.Context, deliveryId string, retryCount int32, nextRetryAt int64, lastError string, from []int32) (bool, error)
	// MarkFailed 置为不可重试失败。
	MarkFailed(ctx context.Context, deliveryId, lastError string, from []int32) (bool, error)
	// MarkDeadLetter 置为死信。
	MarkDeadLetter(ctx context.Context, deliveryId, lastError string, from []int32) (bool, error)
	// MarkSuppressed 置为已拦截（免打扰/频控/过期/收件人不可解析）。
	MarkSuppressed(ctx context.Context, deliveryId, reason string, from []int32) (bool, error)
	// ResetForDeadLetterRetry 把死信任务重置为待投递（运营重投）。
	ResetForDeadLetterRetry(ctx context.Context, deliveryId string) (bool, error)
	// List 分页查询投递记录，按 ctime 降序。
	List(ctx context.Context, f DeliveryFilter, pn, ps int32) ([]*NotificationDelivery, int64, error)
	// ListBySourceEvent 查询某事件派生的、处于指定状态的投递任务。
	ListBySourceEvent(ctx context.Context, eventId string, states []int32) ([]*NotificationDelivery, error)
}

type defaultNotificationDeliveryModel struct {
	conn sqlx.SqlConn
}

// NewNotificationDeliveryModel 创建 NotificationDeliveryModel 实现。
func NewNotificationDeliveryModel(conn sqlx.SqlConn) NotificationDeliveryModel {
	return &defaultNotificationDeliveryModel{conn: conn}
}

const deliveryCols = "delivery_id, biz_key, biz_group_key, mid, channel, template_code, template_version, lang, " +
	"target_ref, params_json, payload_digest, state, provider, provider_msg_id, priority, retry_count, " +
	"next_retry_at, last_error, sent_at, expire_at, source_event_id, trace_id, ctime, mtime"

func (m *defaultNotificationDeliveryModel) Insert(ctx context.Context, d *NotificationDelivery) error {
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO notification_delivery (delivery_id, biz_key, biz_group_key, mid, channel, template_code, template_version, lang, "+
			"target_ref, params_json, payload_digest, state, provider, provider_msg_id, priority, retry_count, next_retry_at, "+
			"last_error, sent_at, expire_at, source_event_id, trace_id, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		d.DeliveryId, d.BizKey, d.BizGroupKey, d.Mid, d.Channel, d.TemplateCode, d.TemplateVersion, d.Lang,
		d.TargetRef, d.ParamsJson, d.PayloadDigest, d.State, d.Provider, d.ProviderMsgId, d.Priority, d.RetryCount,
		d.NextRetryAt, d.LastError, d.SentAt, d.ExpireAt, d.SourceEventId, d.TraceId, d.Ctime, d.Mtime)
	if err != nil {
		if isDuplicateErr(err) {
			return ErrDuplicateBizKey
		}
		return fmt.Errorf("notification_delivery Insert: %w", err)
	}
	return nil
}

func (m *defaultNotificationDeliveryModel) scan(ctx context.Context, query string, args ...any) (*NotificationDelivery, error) {
	var d NotificationDelivery
	if err := m.conn.QueryRowCtx(ctx, &d, query, args...); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("notification_delivery query: %w", err)
	}
	return &d, nil
}

func (m *defaultNotificationDeliveryModel) FindOne(ctx context.Context, deliveryId string) (*NotificationDelivery, error) {
	return m.scan(ctx, "SELECT "+deliveryCols+" FROM notification_delivery WHERE delivery_id = ? LIMIT 1", deliveryId)
}

func (m *defaultNotificationDeliveryModel) FindByBizKey(ctx context.Context, bizKey string) (*NotificationDelivery, error) {
	return m.scan(ctx, "SELECT "+deliveryCols+" FROM notification_delivery WHERE biz_key = ? LIMIT 1", bizKey)
}

func (m *defaultNotificationDeliveryModel) ListDue(ctx context.Context, now int64, limit int32) ([]*NotificationDelivery, error) {
	if limit < 1 {
		limit = 64
	}
	var rows []*NotificationDelivery
	q := "SELECT " + deliveryCols + " FROM notification_delivery " +
		"WHERE state IN (?, ?) AND next_retry_at <= ? AND (expire_at = 0 OR expire_at > ?) " +
		"ORDER BY priority DESC, next_retry_at ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, q, DeliveryStatePending, DeliveryStateRetry, now, now, limit); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("notification_delivery ListDue: %w", err)
	}
	return rows, nil
}

// guardedUpdate 执行带源状态守卫的状态更新，返回是否命中。
func (m *defaultNotificationDeliveryModel) guardedUpdate(ctx context.Context, set string, args []any, deliveryId string, from []int32) (bool, error) {
	if len(from) == 0 {
		return false, ErrIllegalStateTransition
	}
	q := "UPDATE notification_delivery SET " + set + ", mtime = ? WHERE delivery_id = ? AND state IN (" + placeholders(len(from)) + ")"
	args = append(args, nowUnix(), deliveryId)
	args = append(args, int32Args(from)...)
	res, err := m.conn.ExecCtx(ctx, q, args...)
	if err != nil {
		return false, fmt.Errorf("notification_delivery %s: %w", set, err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("notification_delivery %s RowsAffected: %w", set, err)
	}
	return aff > 0, nil
}

func (m *defaultNotificationDeliveryModel) MarkSent(ctx context.Context, deliveryId, provider, providerMsgId, payloadDigest string, sentAt int64, from []int32) (bool, error) {
	return m.guardedUpdate(ctx,
		"state = ?, provider = ?, provider_msg_id = ?, payload_digest = ?, sent_at = ?, last_error = '', next_retry_at = 0",
		[]any{DeliveryStateSent, provider, providerMsgId, payloadDigest, sentAt}, deliveryId, from)
}

func (m *defaultNotificationDeliveryModel) MarkRetry(ctx context.Context, deliveryId string, retryCount int32, nextRetryAt int64, lastError string, from []int32) (bool, error) {
	return m.guardedUpdate(ctx,
		"state = ?, retry_count = ?, next_retry_at = ?, last_error = ?",
		[]any{DeliveryStateRetry, retryCount, nextRetryAt, truncate(lastError, 500)}, deliveryId, from)
}

func (m *defaultNotificationDeliveryModel) MarkFailed(ctx context.Context, deliveryId, lastError string, from []int32) (bool, error) {
	return m.guardedUpdate(ctx,
		"state = ?, last_error = ?, next_retry_at = 0",
		[]any{DeliveryStateFailed, truncate(lastError, 500)}, deliveryId, from)
}

func (m *defaultNotificationDeliveryModel) MarkDeadLetter(ctx context.Context, deliveryId, lastError string, from []int32) (bool, error) {
	return m.guardedUpdate(ctx,
		"state = ?, last_error = ?, next_retry_at = 0",
		[]any{DeliveryStateDeadLetter, truncate(lastError, 500)}, deliveryId, from)
}

func (m *defaultNotificationDeliveryModel) MarkSuppressed(ctx context.Context, deliveryId, reason string, from []int32) (bool, error) {
	return m.guardedUpdate(ctx,
		"state = ?, last_error = ?, next_retry_at = 0",
		[]any{DeliveryStateSuppressed, truncate(reason, 500)}, deliveryId, from)
}

func (m *defaultNotificationDeliveryModel) ResetForDeadLetterRetry(ctx context.Context, deliveryId string) (bool, error) {
	return m.guardedUpdate(ctx,
		"state = ?, retry_count = 0, next_retry_at = 0, last_error = ''",
		[]any{DeliveryStatePending}, deliveryId, []int32{DeliveryStateDeadLetter})
}

func (m *defaultNotificationDeliveryModel) List(ctx context.Context, f DeliveryFilter, pn, ps int32) ([]*NotificationDelivery, int64, error) {
	if ps > 100 {
		return nil, 0, ErrPsTooLarge
	}
	if pn < 1 {
		pn = 1
	}
	if ps < 1 {
		ps = 20
	}
	cond, args := f.where()

	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM notification_delivery WHERE "+cond, args...); err != nil {
		return nil, 0, fmt.Errorf("notification_delivery List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), ps, (pn-1)*ps)
	var rows []*NotificationDelivery
	q := "SELECT " + deliveryCols + " FROM notification_delivery WHERE " + cond + " ORDER BY ctime DESC, delivery_id DESC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, q, listArgs...); err != nil {
		if isNoRows(err) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("notification_delivery List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultNotificationDeliveryModel) ListBySourceEvent(ctx context.Context, eventId string, states []int32) ([]*NotificationDelivery, error) {
	if eventId == "" {
		return nil, nil
	}
	var rows []*NotificationDelivery
	q := "SELECT " + deliveryCols + " FROM notification_delivery WHERE source_event_id = ?"
	args := []any{eventId}
	if len(states) > 0 {
		q += " AND state IN (" + placeholders(len(states)) + ")"
		args = append(args, int32Args(states)...)
	}
	q += " ORDER BY ctime ASC"
	if err := m.conn.QueryRowsCtx(ctx, &rows, q, args...); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("notification_delivery ListBySourceEvent: %w", err)
	}
	return rows, nil
}

// where 组装过滤条件。
func (f DeliveryFilter) where() (string, []any) {
	where := []string{"1 = 1"}
	args := make([]any, 0, 8)
	if f.Mid > 0 {
		where = append(where, "mid = ?")
		args = append(args, f.Mid)
	}
	if f.Channel != 0 {
		where = append(where, "channel = ?")
		args = append(args, f.Channel)
	}
	if f.State != 0 {
		where = append(where, "state = ?")
		args = append(args, f.State)
	}
	if f.BizKey != "" {
		where = append(where, "biz_key = ?")
		args = append(args, f.BizKey)
	}
	if f.BizGroupKey != "" {
		where = append(where, "biz_group_key = ?")
		args = append(args, f.BizGroupKey)
	}
	if f.SourceEventId != "" {
		where = append(where, "source_event_id = ?")
		args = append(args, f.SourceEventId)
	}
	if f.StartCtime > 0 {
		where = append(where, "ctime >= ?")
		args = append(args, f.StartCtime)
	}
	if f.EndCtime > 0 {
		where = append(where, "ctime <= ?")
		args = append(args, f.EndCtime)
	}
	return joinAnd(where), args
}

// truncate 按字节截断，避免超长错误信息写坏 last_error 列。
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
