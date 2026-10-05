package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// Webhook 投递状态，与 op_webhook_delivery.state、rpc.WebhookDeliveryState 一致。
const (
	// DeliveryStatePending 待投递。
	DeliveryStatePending int32 = 1
	// DeliveryStateDelivering 投递中（lease_until 到期后可被其他 worker 抢占）。
	DeliveryStateDelivering int32 = 2
	// DeliveryStateSuccess 成功（HTTP 2xx）。
	DeliveryStateSuccess int32 = 3
	// DeliveryStateRetryScheduled 已排定退避重试。
	DeliveryStateRetryScheduled int32 = 4
	// DeliveryStateDead 死信（超过最大次数，等待人工重放）。
	DeliveryStateDead int32 = 5
	// DeliveryStateIgnored 人工忽略或端点删除后抑制（终态）。
	DeliveryStateIgnored int32 = 6
)

// ValidDeliveryState 判断投递状态取值合法。
func ValidDeliveryState(v int32) bool {
	return v >= DeliveryStatePending && v <= DeliveryStateIgnored
}

// WebhookDelivery 投递任务（op_webhook_delivery 表）：一个 (事件, 端点) 一行。
//
// 幂等：唯一键 (event_id, endpoint_id)。领域事件可能重复投递（Kafka at-least-once），
// 同一 event_id 重复入队不会产生第二条任务，EnqueueWebhookEvent 返回 deduplicated=true。
//
// payload 与投递记录的可观测性折中：本表必须存 payload（重放时不能要求上游重新生产），
// 但任何 RPC 响应只回显 payload_digest（sha256:<hex>），生产侧写入前也已禁止放
// token/secret/身份证/手机号明文；本表按 WebhookPayloadRetentionDays 定期清理正文列。
type WebhookDelivery struct {
	// DeliveryID 投递任务 ID（主键，参与签名串）
	DeliveryID int64 `db:"delivery_id"`
	// AppID 应用 ID（冗余，便于按应用排障）
	AppID int64 `db:"app_id"`
	// EndpointID 端点 ID
	EndpointID int64 `db:"endpoint_id"`
	// EventType 事件类型
	EventType int32 `db:"event_type"`
	// EventID 领域事件幂等键
	EventID string `db:"event_id"`
	// Payload 投递正文（JSON 文本，禁止含敏感凭证；响应侧不回显）
	Payload string `db:"payload"`
	// PayloadDigest 正文摘要，格式 sha256:<hex>
	PayloadDigest string `db:"payload_digest"`
	// State 投递状态，见 DeliveryState*
	State int32 `db:"state"`
	// Attempt 已尝试次数
	Attempt int32 `db:"attempt"`
	// MaxAttempts 最大尝试次数（入队时从配置快照，改配置不影响历史任务）
	MaxAttempts int32 `db:"max_attempts"`
	// NextRetryAt 下次可投递时间（Unix 秒；DELIVERING 期间复用为租约到期时间）
	NextRetryAt int64 `db:"next_retry_at"`
	// LeaseUntil worker 抢占租约到期时间（Unix 秒，0 表示无租约）
	LeaseUntil int64 `db:"lease_until"`
	// LastStatusCode 最近一次 HTTP 状态码
	LastStatusCode int64 `db:"last_status_code"`
	// LastError 最近一次错误摘要（截断且脱敏，禁止含响应体原文）
	LastError string `db:"last_error"`
	// DeliveredAt 成功时间（Unix 秒）
	DeliveredAt int64 `db:"delivered_at"`
	// Ctime 入队时间（Unix 秒）
	Ctime int64 `db:"ctime"`
	// Mtime 最近更新时间（Unix 秒）
	Mtime int64 `db:"mtime"`
}

// Retryable 判断当前状态是否可（继续）投递。
func (d *WebhookDelivery) Retryable() bool {
	return d != nil && (d.State == DeliveryStatePending || d.State == DeliveryStateRetryScheduled)
}

// ManuallyReplayable 判断是否允许人工重放（只有死信与人工忽略两类终态可重放）。
func (d *WebhookDelivery) ManuallyReplayable() bool {
	return d != nil && (d.State == DeliveryStateDead || d.State == DeliveryStateIgnored)
}

// WebhookDeliveryModel op_webhook_delivery 表读写接口。
type WebhookDeliveryModel interface {
	// Insert 入队一条投递任务；命中 uniq (event_id, endpoint_id) 时返回既有 ID 且 created=false。
	Insert(ctx context.Context, d *WebhookDelivery) (deliveryID int64, created bool, err error)
	// FindByID 主键查询；不存在返回 (nil, nil)。
	FindByID(ctx context.Context, deliveryID int64) (*WebhookDelivery, error)
	// ListByEventID 返回同一事件的全部投递任务（判断 matched_endpoints 与去重）。
	ListByEventID(ctx context.Context, eventID string) ([]*WebhookDelivery, error)
	// ListDue 拉取到期待投递任务 ID（worker 轮询；只取 ID，不预载正文）。
	ListDue(ctx context.Context, now int64, limit int32) ([]int64, error)
	// Claim 抢占任务（CAS）：pending/retry_scheduled 且 next_retry_at<=now 时置为 delivering，
	// attempt+1 并把租约写到 leaseUntil；返回 applied=false 表示被其他 worker 抢走。
	Claim(ctx context.Context, deliveryID, now, leaseUntil int64) (applied bool, err error)
	// MarkSuccess 标记成功（仅 accepting delivering 状态，避免重复计数）。
	MarkSuccess(ctx context.Context, deliveryID, statusCode, now int64) (bool, error)
	// MarkFailed 单次失败：dead=true 置死信，否则置 retry_scheduled 并写入 nextRetryAt。
	// 只接受 delivering 状态，保证重试次数与租约一致。
	MarkFailed(ctx context.Context, deliveryID int64, dead bool, nextRetryAt, statusCode int64,
		lastError string, now int64) (bool, error)
	// ReleaseLease 投递协程异常退出时归还租约（delivering → retry_scheduled，不消耗次数）。
	ReleaseLease(ctx context.Context, deliveryID, nextRetryAt int64) (bool, error)
	// ResetForReplay 人工重放（死信/忽略 → pending，attempt 归零），幂等。
	ResetForReplay(ctx context.Context, deliveryID, operator int64, reason string,
		now int64) (bool, error)
	// SuppressByEndpoint 端点删除后抑制未完成任务（pending/delivering/retry_scheduled → ignored）。
	// session 非空走事务，与 WebhookEndpointModel.SoftDelete 同事务提交。
	SuppressByEndpoint(ctx context.Context, session sqlx.Session, endpointID int64, reason string,
		now int64) (int64, error)
	// ListByCursor 运营/开发者侧投递记录分页：cursor 为 (ctime, delivery_id) 倒序位点。
	ListByCursor(ctx context.Context, appID, endpointID int64, state int32, onlyFailed bool,
		cursorTime, cursorID int64, ps int32) ([]*WebhookDelivery, error)
	// PurgePayloadBefore 清空到期任务的正文列（保留投递结论，隐私最小化）。
	PurgePayloadBefore(ctx context.Context, before int64, limit int32) (int64, error)
}

type defaultWebhookDeliveryModel struct {
	conn sqlx.SqlConn
}

// NewWebhookDeliveryModel 创建 WebhookDeliveryModel 实现。
func NewWebhookDeliveryModel(conn sqlx.SqlConn) WebhookDeliveryModel {
	return &defaultWebhookDeliveryModel{conn: conn}
}

const webhookDeliveryColumns = `delivery_id, app_id, endpoint_id, event_type, event_id, payload,
	payload_digest, state, attempt, max_attempts, next_retry_at, lease_until, last_status_code,
	last_error, delivered_at, ctime, mtime`

func (m *defaultWebhookDeliveryModel) Insert(ctx context.Context, d *WebhookDelivery) (int64, bool, error) {
	if d.AppID <= 0 {
		return 0, false, ErrInvalidAppID
	}
	if d.EndpointID <= 0 {
		return 0, false, ErrWebhookNotFound
	}
	if strings.TrimSpace(d.EventID) == "" {
		return 0, false, ErrEventIDRequired
	}
	if !ValidWebhookEventType(d.EventType) {
		return 0, false, ErrInvalidEventType
	}
	if d.MaxAttempts <= 0 {
		return 0, false, ErrInvalidStateTransition
	}
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO op_webhook_delivery (app_id, endpoint_id, event_type, event_id, payload, payload_digest, "+
			"state, attempt, max_attempts, next_retry_at, lease_until, last_status_code, last_error, "+
			"delivered_at, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, 0, 0, '', 0, ?, ?) "+
			"ON DUPLICATE KEY UPDATE delivery_id = delivery_id",
		d.AppID, d.EndpointID, d.EventType, d.EventID, d.Payload, d.PayloadDigest,
		DeliveryStatePending, d.MaxAttempts, d.NextRetryAt, now, now)
	if err != nil {
		return 0, false, fmt.Errorf("op_webhook_delivery Insert: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("op_webhook_delivery Insert RowsAffected: %w", err)
	}
	if affected == 1 {
		id, err := res.LastInsertId()
		if err != nil {
			return 0, false, fmt.Errorf("op_webhook_delivery Insert LastInsertId: %w", err)
		}
		return id, true, nil
	}
	// 同一 event_id + endpoint 已入队：返回既有任务，调用方标记 deduplicated。
	old, err := m.findExact(ctx, d.EventID, d.EndpointID)
	if err != nil {
		return 0, false, err
	}
	if old == nil {
		return 0, false, ErrEventIDRequired
	}
	return old.DeliveryID, false, nil
}

func (m *defaultWebhookDeliveryModel) findExact(ctx context.Context, eventID string,
	endpointID int64) (*WebhookDelivery, error) {
	var d WebhookDelivery
	err := m.conn.QueryRowCtx(ctx, &d,
		"SELECT "+webhookDeliveryColumns+" FROM op_webhook_delivery WHERE event_id = ? AND endpoint_id = ? "+
			"LIMIT 1", eventID, endpointID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_webhook_delivery findExact: %w", err)
	}
	return &d, nil
}

func (m *defaultWebhookDeliveryModel) FindByID(ctx context.Context, deliveryID int64) (*WebhookDelivery, error) {
	var d WebhookDelivery
	err := m.conn.QueryRowCtx(ctx, &d,
		"SELECT "+webhookDeliveryColumns+" FROM op_webhook_delivery WHERE delivery_id = ? LIMIT 1", deliveryID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_webhook_delivery FindByID: %w", err)
	}
	return &d, nil
}

func (m *defaultWebhookDeliveryModel) ListByEventID(ctx context.Context, eventID string) ([]*WebhookDelivery, error) {
	if strings.TrimSpace(eventID) == "" {
		return nil, ErrEventIDRequired
	}
	var rows []*WebhookDelivery
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT "+webhookDeliveryColumns+" FROM op_webhook_delivery WHERE event_id = ? ORDER BY endpoint_id ASC",
		eventID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_webhook_delivery ListByEventID: %w", err)
	}
	return rows, nil
}

func (m *defaultWebhookDeliveryModel) ListDue(ctx context.Context, now int64, limit int32) ([]int64, error) {
	if limit <= 0 {
		return nil, ErrInvalidPage
	}
	var ids []int64
	// delivering 且租约过期的任务一并回收：worker 崩溃不会把任务永久卡住。
	err := m.conn.QueryRowsCtx(ctx, &ids,
		"SELECT delivery_id FROM op_webhook_delivery "+
			"WHERE ((state IN (?, ?) AND next_retry_at <= ?) OR (state = ? AND lease_until <= ?)) "+
			"ORDER BY next_retry_at ASC, delivery_id ASC LIMIT ?",
		DeliveryStatePending, DeliveryStateRetryScheduled, now, DeliveryStateDelivering, now, limit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_webhook_delivery ListDue: %w", err)
	}
	return ids, nil
}

func (m *defaultWebhookDeliveryModel) Claim(ctx context.Context, deliveryID, now, leaseUntil int64) (bool, error) {
	if deliveryID <= 0 {
		return false, ErrDeliveryNotFound
	}
	if leaseUntil <= now {
		return false, ErrInvalidStateTransition
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE op_webhook_delivery SET state = ?, attempt = attempt + 1, lease_until = ?, mtime = ? "+
			"WHERE delivery_id = ? AND ((state IN (?, ?) AND next_retry_at <= ?) OR (state = ? AND lease_until <= ?))",
		DeliveryStateDelivering, leaseUntil, now, deliveryID,
		DeliveryStatePending, DeliveryStateRetryScheduled, now, DeliveryStateDelivering, now)
	if err != nil {
		return false, fmt.Errorf("op_webhook_delivery Claim: %w", err)
	}
	return rowsPositive(res, "op_webhook_delivery Claim")
}

func (m *defaultWebhookDeliveryModel) MarkSuccess(ctx context.Context, deliveryID, statusCode,
	now int64) (bool, error) {
	if deliveryID <= 0 {
		return false, ErrDeliveryNotFound
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE op_webhook_delivery SET state = ?, delivered_at = ?, last_status_code = ?, last_error = '', "+
			"lease_until = 0, mtime = ? WHERE delivery_id = ? AND state = ?",
		DeliveryStateSuccess, now, statusCode, now, deliveryID, DeliveryStateDelivering)
	if err != nil {
		return false, fmt.Errorf("op_webhook_delivery MarkSuccess: %w", err)
	}
	return rowsPositive(res, "op_webhook_delivery MarkSuccess")
}

func (m *defaultWebhookDeliveryModel) MarkFailed(ctx context.Context, deliveryID int64, dead bool,
	nextRetryAt, statusCode int64, lastError string, now int64) (bool, error) {
	if deliveryID <= 0 {
		return false, ErrDeliveryNotFound
	}
	state := DeliveryStateRetryScheduled
	if dead {
		state = DeliveryStateDead
	}
	if len(lastError) > maxDeliveryErrLen {
		lastError = lastError[:maxDeliveryErrLen]
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE op_webhook_delivery SET state = ?, next_retry_at = ?, last_status_code = ?, last_error = ?, "+
			"lease_until = 0, mtime = ? WHERE delivery_id = ? AND state = ?",
		state, nextRetryAt, statusCode, lastError, now, deliveryID, DeliveryStateDelivering)
	if err != nil {
		return false, fmt.Errorf("op_webhook_delivery MarkFailed: %w", err)
	}
	return rowsPositive(res, "op_webhook_delivery MarkFailed")
}

// maxDeliveryErrLen 错误摘要入库上限（防止把整段响应体塞进数据库，也避免日志放大）。
const maxDeliveryErrLen = 200

func (m *defaultWebhookDeliveryModel) ReleaseLease(ctx context.Context, deliveryID, nextRetryAt int64) (bool, error) {
	if deliveryID <= 0 {
		return false, ErrDeliveryNotFound
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE op_webhook_delivery SET state = ?, next_retry_at = ?, lease_until = 0, mtime = ? "+
			"WHERE delivery_id = ? AND state = ?",
		DeliveryStateRetryScheduled, nextRetryAt, nowUnix(), deliveryID, DeliveryStateDelivering)
	if err != nil {
		return false, fmt.Errorf("op_webhook_delivery ReleaseLease: %w", err)
	}
	return rowsPositive(res, "op_webhook_delivery ReleaseLease")
}

func (m *defaultWebhookDeliveryModel) ResetForReplay(ctx context.Context, deliveryID, operator int64,
	reason string, now int64) (bool, error) {
	if deliveryID <= 0 {
		return false, ErrDeliveryNotFound
	}
	if operator <= 0 {
		return false, ErrOperatorRequired
	}
	summary := strings.TrimSpace(reason)
	if summary == "" {
		return false, ErrDeliveryNotRetryable
	}
	if len(summary) > maxDeliveryErrLen {
		summary = summary[:maxDeliveryErrLen]
	}
	// attempt 归零：人工重放等于重新走一遍完整的退避策略。
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE op_webhook_delivery SET state = ?, attempt = 0, next_retry_at = ?, last_error = ?, "+
			"lease_until = 0, mtime = ? WHERE delivery_id = ? AND state IN (?, ?)",
		DeliveryStatePending, now, summary, now, deliveryID, DeliveryStateDead, DeliveryStateIgnored)
	if err != nil {
		return false, fmt.Errorf("op_webhook_delivery ResetForReplay: %w", err)
	}
	return rowsPositive(res, "op_webhook_delivery ResetForReplay")
}

func (m *defaultWebhookDeliveryModel) SuppressByEndpoint(ctx context.Context, session sqlx.Session,
	endpointID int64, reason string, now int64) (int64, error) {
	if endpointID <= 0 {
		return 0, ErrWebhookNotFound
	}
	res, err := pick(session, m.conn).ExecCtx(ctx,
		"UPDATE op_webhook_delivery SET state = ?, next_retry_at = ?, last_error = ?, lease_until = 0, mtime = ? "+
			"WHERE endpoint_id = ? AND state IN (?, ?, ?)",
		DeliveryStateIgnored, now, reason, now, endpointID,
		DeliveryStatePending, DeliveryStateDelivering, DeliveryStateRetryScheduled)
	if err != nil {
		return 0, fmt.Errorf("op_webhook_delivery SuppressByEndpoint: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("op_webhook_delivery SuppressByEndpoint RowsAffected: %w", err)
	}
	return aff, nil
}

func (m *defaultWebhookDeliveryModel) ListByCursor(ctx context.Context, appID, endpointID int64,
	state int32, onlyFailed bool, cursorTime, cursorID int64, ps int32) ([]*WebhookDelivery, error) {
	if ps <= 0 {
		return nil, ErrInvalidPage
	}
	conds := []string{"1 = 1"}
	var args []any
	if appID > 0 {
		conds = append(conds, "app_id = ?")
		args = append(args, appID)
	}
	if endpointID > 0 {
		conds = append(conds, "endpoint_id = ?")
		args = append(args, endpointID)
	}
	if state > 0 {
		conds = append(conds, "state = ?")
		args = append(args, state)
	}
	if onlyFailed {
		conds = append(conds, "state IN (?, ?)")
		args = append(args, DeliveryStateDead, DeliveryStateRetryScheduled)
	}
	if cursorTime > 0 {
		conds = append(conds, "(ctime < ? OR (ctime = ? AND delivery_id < ?))")
		args = append(args, cursorTime, cursorTime, cursorID)
	}
	// 不选 payload 列：列表接口只需要结论与摘要，禁止把投递正文批量读进内存。
	query := "SELECT delivery_id, app_id, endpoint_id, event_type, event_id, payload_digest, state, attempt, " +
		"max_attempts, next_retry_at, lease_until, last_status_code, last_error, delivered_at, ctime, mtime " +
		"FROM op_webhook_delivery WHERE " + strings.Join(conds, " AND ") +
		" ORDER BY ctime DESC, delivery_id DESC LIMIT ?"
	args = append(args, ps)

	var rows []*WebhookDelivery
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_webhook_delivery ListByCursor: %w", err)
	}
	return rows, nil
}

func (m *defaultWebhookDeliveryModel) PurgePayloadBefore(ctx context.Context, before int64,
	limit int32) (int64, error) {
	if limit <= 0 {
		return 0, ErrInvalidPage
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE op_webhook_delivery SET payload = '', mtime = ? "+
			"WHERE state IN (?, ?) AND ctime < ? AND payload <> '' LIMIT ?",
		nowUnix(), DeliveryStateSuccess, DeliveryStateIgnored, before, limit)
	if err != nil {
		return 0, fmt.Errorf("op_webhook_delivery PurgePayloadBefore: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("op_webhook_delivery PurgePayloadBefore RowsAffected: %w", err)
	}
	return aff, nil
}

// NextRetryAt 计算指数退避后的下次投递时间：base * 2^(attempt-1)，上限 maxSeconds。
// 纯函数便于单测；attempt 从 1 起（Claim 已自增），因此第一次重试间隔就是 base。
func NextRetryAt(now, baseSeconds, maxSeconds int64, attempt int32) int64 {
	if baseSeconds <= 0 {
		baseSeconds = 30
	}
	if maxSeconds <= 0 {
		maxSeconds = 3600
	}
	shift := int64(attempt - 1)
	if shift < 0 {
		shift = 0
	}
	if shift > 20 { // 2^20 * base 早已超过任何合理上限，同时避免位移溢出。
		shift = 20
	}
	delay := baseSeconds << shift
	if delay <= 0 || delay > maxSeconds {
		delay = maxSeconds
	}
	return now + delay
}
