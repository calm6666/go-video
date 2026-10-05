package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// Webhook 事件类型，与 op_webhook_endpoint.event_type、rpc.WebhookEventType 一致。
// 只有内容/授权/配额类事件，不存在会员、订单、支付、投币、分成或广告事件（AGENTS.md §1）。
const (
	// WebhookEventContentPublishResult 第三方投稿转码/审核结果。
	WebhookEventContentPublishResult int32 = 1
	// WebhookEventContentOffline 内容下架、版权撤回。
	WebhookEventContentOffline int32 = 2
	// WebhookEventGrantRevoked 用户或平台撤销授权。
	WebhookEventGrantRevoked int32 = 3
	// WebhookEventQuotaWarning 配额接近上限告警。
	WebhookEventQuotaWarning int32 = 4
)

// ValidWebhookEventType 判断事件类型是否已定义（0 是 UNSPECIFIED，一律拒绝）。
func ValidWebhookEventType(v int32) bool {
	return v >= WebhookEventContentPublishResult && v <= WebhookEventQuotaWarning
}

// WebhookEndpoint 回调端点（op_webhook_endpoint 表）。
//
// 签名密钥不入库：投递签名用 HMAC-SHA256(masterPepper, app_id || key_version || delivery_id || body)，
// 表里只留 sign_key_version。因此 masterPepper 只存在于 Secret/Vault，
// 数据库泄露时攻击者既拿不到历史签名密钥、也无法伪造合法回调；
// 轮换版本只需改 sign_key_version，应用侧按版本号取对应密钥。
//
// 未验证不投递：verified_at=0 的端点会被 ListMatching 过滤掉，
// 防止注册任意 URL 就变成 SSRF 跳板（URL 校验在 logic：https only、禁止内网/本机地址）。
type WebhookEndpoint struct {
	// EndpointID 端点 ID（主键）
	EndpointID int64 `db:"endpoint_id"`
	// AppID 应用 ID
	AppID int64 `db:"app_id"`
	// EventType 订阅的事件类型，见 WebhookEvent*
	EventType int32 `db:"event_type"`
	// URL 回调地址（https only，禁止内网地址）
	URL string `db:"callback_url"`
	// SignKeyVersion 当前签名密钥版本（不含密钥材料）
	SignKeyVersion int32 `db:"sign_key_version"`
	// Enabled 是否启用（0 表示暂停投递，保留配置）
	Enabled int8 `db:"enabled"`
	// Description 备注
	Description string `db:"description"`
	// VerifiedAt 验证通过时间（Unix 秒，0 表示未验证，不投递）
	VerifiedAt int64 `db:"verified_at"`
	// ChallengeHash 验证挑战的哈希（明文挑战只在注册响应里出现一次，不入库）
	ChallengeHash string `db:"challenge_hash"`
	// ChallengeExpiresAt 挑战有效期（Unix 秒）
	ChallengeExpiresAt int64 `db:"challenge_expires_at"`
	// DeletedAt 删除时间（Unix 秒，0 表示未删除；软删以保留投递归属）
	DeletedAt int64 `db:"deleted_at"`
	// DeleteReason 删除原因（审计）
	DeleteReason string `db:"delete_reason"`
	// Ctime 创建时间（Unix 秒）
	Ctime int64 `db:"ctime"`
	// Mtime 最近更新时间（Unix 秒）
	Mtime int64 `db:"mtime"`
}

// Deliverable 判断该端点当前是否可投递。
func (e *WebhookEndpoint) Deliverable() bool {
	return e != nil && e.Enabled == 1 && e.VerifiedAt > 0 && e.DeletedAt == 0
}

// WebhookEndpointModel op_webhook_endpoint 表读写接口。
type WebhookEndpointModel interface {
	// Insert 注册端点；命中 uniq (app_id, event_type, callback_url) 时返回既有 ID 且 created=false（幂等）。
	Insert(ctx context.Context, e *WebhookEndpoint) (endpointID int64, created bool, err error)
	// FindByID 主键查询（含已删除行，便于投递记录解释）；不存在返回 (nil, nil)。
	FindByID(ctx context.Context, endpointID int64) (*WebhookEndpoint, error)
	// ListByApp 列出该应用端点；includeDisabled=false 时只返回未删除的启用端点。
	ListByApp(ctx context.Context, appID int64, includeDisabled bool) ([]*WebhookEndpoint, error)
	// ListMatching 入队投递用：返回订阅该事件且可投递的端点。
	// appID>0 只查该应用；appID=0 查全平台订阅该事件的端点（平台级事件广播）。
	ListMatching(ctx context.Context, appID int64, eventType int32) ([]*WebhookEndpoint, error)
	// MarkVerified 验证通过（challenge 比对成功后调用），条件为尚未验证，幂等。
	MarkVerified(ctx context.Context, endpointID, now int64) (bool, error)
	// SetEnabled 启停端点。
	SetEnabled(ctx context.Context, endpointID int64, enabled int8, operator int64) (bool, error)
	// SoftDelete 删除端点（保留行以便投递记录归属可查）。
	// session 非空走事务：删除必须与「抑制在途任务」原子生效，否则中间失败会留下
	// 「地址已下线但仍排着待投任务」的窗口（投递 worker 只是跳过，任务永远挂着）。
	SoftDelete(ctx context.Context, session sqlx.Session, endpointID int64, operator int64,
		reason string, now int64) (bool, error)
	// CountByApp 统计该应用未删除端点数（配置上限校验）。
	CountByApp(ctx context.Context, appID int64) (int64, error)
}

type defaultWebhookEndpointModel struct {
	conn sqlx.SqlConn
}

// NewWebhookEndpointModel 创建 WebhookEndpointModel 实现。
func NewWebhookEndpointModel(conn sqlx.SqlConn) WebhookEndpointModel {
	return &defaultWebhookEndpointModel{conn: conn}
}

const webhookEndpointColumns = `endpoint_id, app_id, event_type, callback_url, sign_key_version, enabled, description,
	verified_at, challenge_hash, challenge_expires_at, deleted_at, delete_reason, ctime, mtime`

func (m *defaultWebhookEndpointModel) Insert(ctx context.Context, e *WebhookEndpoint) (int64, bool, error) {
	if e.AppID <= 0 {
		return 0, false, ErrInvalidAppID
	}
	if !ValidWebhookEventType(e.EventType) {
		return 0, false, ErrInvalidEventType
	}
	if strings.TrimSpace(e.URL) == "" {
		return 0, false, ErrInvalidWebhookURL
	}
	if e.SignKeyVersion <= 0 {
		e.SignKeyVersion = 1
	}
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO op_webhook_endpoint (app_id, event_type, callback_url, sign_key_version, enabled, description, "+
			"verified_at, challenge_hash, challenge_expires_at, deleted_at, delete_reason, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, 1, ?, 0, ?, ?, 0, '', ?, ?)",
		e.AppID, e.EventType, e.URL, e.SignKeyVersion, e.Description, e.ChallengeHash,
		e.ChallengeExpiresAt, now, now)
	if err != nil {
		// 同一 (应用, 事件, 地址) 重复注册按幂等处理：回查既有行，不产生第二个端点。
		if isDuplicateKeyErr(err) {
			if old, qerr := m.findExact(ctx, e.AppID, e.EventType, e.URL); qerr == nil && old != nil {
				return old.EndpointID, false, nil
			}
		}
		return 0, false, fmt.Errorf("op_webhook_endpoint Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("op_webhook_endpoint Insert LastInsertId: %w", err)
	}
	return id, true, nil
}

func (m *defaultWebhookEndpointModel) findExact(ctx context.Context, appID int64, eventType int32,
	callbackURL string) (*WebhookEndpoint, error) {
	var e WebhookEndpoint
	err := m.conn.QueryRowCtx(ctx, &e,
		"SELECT "+webhookEndpointColumns+" FROM op_webhook_endpoint WHERE app_id = ? AND event_type = ? "+
			"AND callback_url = ? LIMIT 1", appID, eventType, callbackURL)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_webhook_endpoint findExact: %w", err)
	}
	return &e, nil
}

func (m *defaultWebhookEndpointModel) FindByID(ctx context.Context, endpointID int64) (*WebhookEndpoint, error) {
	var e WebhookEndpoint
	err := m.conn.QueryRowCtx(ctx, &e,
		"SELECT "+webhookEndpointColumns+" FROM op_webhook_endpoint WHERE endpoint_id = ? LIMIT 1", endpointID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_webhook_endpoint FindByID: %w", err)
	}
	return &e, nil
}

func (m *defaultWebhookEndpointModel) ListByApp(ctx context.Context, appID int64,
	includeDisabled bool) ([]*WebhookEndpoint, error) {
	if appID <= 0 {
		return nil, ErrInvalidAppID
	}
	conds := []string{"app_id = ?", "deleted_at = 0"}
	if !includeDisabled {
		conds = append(conds, "enabled = 1")
	}
	query := "SELECT " + webhookEndpointColumns + " FROM op_webhook_endpoint WHERE " +
		strings.Join(conds, " AND ") + " ORDER BY event_type ASC, endpoint_id ASC"
	var rows []*WebhookEndpoint
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, appID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_webhook_endpoint ListByApp: %w", err)
	}
	return rows, nil
}

func (m *defaultWebhookEndpointModel) ListMatching(ctx context.Context, appID int64,
	eventType int32) ([]*WebhookEndpoint, error) {
	if !ValidWebhookEventType(eventType) {
		return nil, ErrInvalidEventType
	}
	conds := []string{"event_type = ?", "enabled = 1", "verified_at > 0", "deleted_at = 0"}
	args := []any{eventType}
	if appID > 0 {
		conds = append(conds, "app_id = ?")
		args = append(args, appID)
	}
	query := "SELECT " + webhookEndpointColumns + " FROM op_webhook_endpoint WHERE " +
		strings.Join(conds, " AND ") + " ORDER BY endpoint_id ASC"
	var rows []*WebhookEndpoint
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_webhook_endpoint ListMatching: %w", err)
	}
	return rows, nil
}

func (m *defaultWebhookEndpointModel) MarkVerified(ctx context.Context, endpointID, now int64) (bool, error) {
	if endpointID <= 0 {
		return false, ErrWebhookNotFound
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE op_webhook_endpoint SET verified_at = ?, challenge_hash = '', mtime = ? "+
			"WHERE endpoint_id = ? AND verified_at = 0 AND deleted_at = 0",
		now, now, endpointID)
	if err != nil {
		return false, fmt.Errorf("op_webhook_endpoint MarkVerified: %w", err)
	}
	return rowsPositive(res, "op_webhook_endpoint MarkVerified")
}

func (m *defaultWebhookEndpointModel) SetEnabled(ctx context.Context, endpointID int64, enabled int8,
	operator int64) (bool, error) {
	if endpointID <= 0 {
		return false, ErrWebhookNotFound
	}
	if operator <= 0 {
		return false, ErrOperatorRequired
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE op_webhook_endpoint SET enabled = ?, mtime = ? WHERE endpoint_id = ? AND deleted_at = 0",
		enabled, nowUnix(), endpointID)
	if err != nil {
		return false, fmt.Errorf("op_webhook_endpoint SetEnabled: %w", err)
	}
	return rowsPositive(res, "op_webhook_endpoint SetEnabled")
}

func (m *defaultWebhookEndpointModel) SoftDelete(ctx context.Context, session sqlx.Session, endpointID int64,
	operator int64, reason string, now int64) (bool, error) {
	if endpointID <= 0 {
		return false, ErrWebhookNotFound
	}
	if operator <= 0 {
		return false, ErrOperatorRequired
	}
	res, err := pick(session, m.conn).ExecCtx(ctx,
		"UPDATE op_webhook_endpoint SET enabled = 0, deleted_at = ?, delete_reason = ?, mtime = ? "+
			"WHERE endpoint_id = ? AND deleted_at = 0",
		now, reason, now, endpointID)
	if err != nil {
		return false, fmt.Errorf("op_webhook_endpoint SoftDelete: %w", err)
	}
	return rowsPositive(res, "op_webhook_endpoint SoftDelete")
}

func (m *defaultWebhookEndpointModel) CountByApp(ctx context.Context, appID int64) (int64, error) {
	var cnt int64
	err := m.conn.QueryRowCtx(ctx, &cnt,
		"SELECT COUNT(*) FROM op_webhook_endpoint WHERE app_id = ? AND deleted_at = 0", appID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("op_webhook_endpoint CountByApp: %w", err)
	}
	return cnt, nil
}
