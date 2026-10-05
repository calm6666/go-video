package model

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// DispatchPolicy 采样与脱敏配置版本（ec_dispatch_policy 表）。
//
// 版本语义（proto 注释同义）：
//   - DRAFT 可反复改；ACTIVE 只读，要改必须新建版本再切换；ARCHIVED 永久保留，
//     是历史批次归因依据（每条 ec_event_record 都写了自己用的 policy_version）。
//   - 「同一时刻只有一版生效」由 uniq_active(active_flag) 在数据库层保证：
//     ACTIVE 行写 active_flag=1，其余写 NULL（MySQL 唯一索引允许多个 NULL）。
//     并发切换的第二笔会撞唯一键 → IsDuplicate → ErrConcurrentUpdate，调用方重试。
//
// 隐私：只存 salt_ref（环境变量名），绝不存盐值本身；白名单/禁止字段以 JSON 数组存 TEXT 列。
type DispatchPolicy struct {
	ID int64 `db:"id"`
	// Version 语义化版本，如 2026.09.20-1（uniq_version）
	Version string `db:"version"`
	// State 见 PolicyState*；state=ACTIVE 与 active_flag=1 同义（列不参与查询投影，
	// 避免 NULL→int32 扫描歧义，读写一律用 state 判定）
	State int32 `db:"state"`
	// SampleRulesJSON 采样规则（JSON 数组文本，logic 用 DecodeSampleRules 解析）
	SampleRulesJSON string `db:"sample_rules"`
	// SaltVersion 脱敏哈希盐版本（轮换后旧数据不可逆推）
	SaltVersion int32 `db:"salt_version"`
	// SaltRef 取盐的环境变量名，如 EVENT_COLLECTOR_SALT_V2（不含值）
	SaltRef string `db:"salt_ref"`
	// FieldWhitelistJSON payload 允许保留的字段名（JSON 数组文本）
	FieldWhitelistJSON string `db:"field_whitelist"`
	// DropFieldsJSON 明确禁止入库的字段名（明文 ip/imei/phone/token 等）
	DropFieldsJSON string `db:"drop_fields"`
	// MaxEventsPerBatch 单请求条数上限
	MaxEventsPerBatch int32 `db:"max_events_per_batch"`
	// MaxRequestBytes 单请求字节上限
	MaxRequestBytes int64 `db:"max_request_bytes"`
	// MaxEventPayloadBytes 单事件 payload 字节上限
	MaxEventPayloadBytes int32 `db:"max_event_payload_bytes"`
	// MaxClockSkewSeconds 允许的时钟偏差绝对值
	MaxClockSkewSeconds int32 `db:"max_clock_skew_seconds"`
	// MaxBackfillSeconds 允许的回补窗口（occurred_at 过旧即拒绝）
	MaxBackfillSeconds int32 `db:"max_backfill_seconds"`
	// KeywordMaxRunes 搜索词截断长度
	KeywordMaxRunes int32 `db:"keyword_max_runes"`
	// RetentionDays 接收台账保留天数（cron 清理）
	RetentionDays int32 `db:"retention_days"`
	// DeliverMaxAttempts 投递重试上限，超过转死信
	DeliverMaxAttempts int32 `db:"deliver_max_attempts"`
	// RetryBaseSeconds 退避基数
	RetryBaseSeconds int64 `db:"retry_base_seconds"`
	// RetryMaxSeconds 退避上限
	RetryMaxSeconds int64 `db:"retry_max_seconds"`
	// Note 变更说明（为什么改这版，审计用）
	Note string `db:"note"`
	// Operator 最后修改人（服务账号或运营 ID）
	Operator string `db:"operator"`
	Ctime    int64  `db:"ctime"`
	Mtime    int64  `db:"mtime"`
}

// DispatchPolicyModel ec_dispatch_policy 读写接口。
type DispatchPolicyModel interface {
	// InsertDraft 新建草稿版本；version 已存在返回 (既有 ID, false, nil)。
	InsertDraft(ctx context.Context, session sqlx.Session, p *DispatchPolicy) (id int64, created bool, err error)
	// UpdateDraft 原地更新草稿；只作用于 state=DRAFT 的行，
	// applied=false 表示目标版本已 ACTIVE/ARCHIVED（必须新建版本，见 ErrActivePolicyImmutable）。
	UpdateDraft(ctx context.Context, session sqlx.Session, p *DispatchPolicy) (applied bool, err error)
	// FindByVersion 查询指定版本；不存在返回 ErrPolicyNotFound。
	FindByVersion(ctx context.Context, version string) (*DispatchPolicy, error)
	// FindActive 查询当前生效版本；无生效版本返回 ErrNoActivePolicy。
	FindActive(ctx context.Context) (*DispatchPolicy, error)
	// Activate 原子切换生效版本：在同一事务里锁定当前 ACTIVE、归档、激活新版本。
	// expectedCurrent 非空时做乐观校验，不匹配返回 ErrConcurrentUpdate；
	// 返回被归档的旧版本号（无旧版本时为空串）。
	Activate(ctx context.Context, session sqlx.Session, version, expectedCurrent, operator string) (previous string, err error)
	// Archive 废弃一个草稿版本（DRAFT → ARCHIVED），不允许归档 ACTIVE。
	Archive(ctx context.Context, session sqlx.Session, version, operator string) (applied bool, err error)
	// List 按 id 倒序游标翻页（state=0 表示全部状态）。
	List(ctx context.Context, state int32, afterID int64, ps int32) ([]*DispatchPolicy, error)
	// Count 按状态计数。
	Count(ctx context.Context, state int32) (int64, error)
	// WithSession 绑定事务句柄。
	WithSession(session sqlx.Session) DispatchPolicyModel
}

type defaultDispatchPolicyModel struct {
	conn sqlx.SqlConn
}

// NewDispatchPolicyModel 创建 DispatchPolicyModel 实现。
func NewDispatchPolicyModel(conn sqlx.SqlConn) DispatchPolicyModel {
	return &defaultDispatchPolicyModel{conn: conn}
}

func (m *defaultDispatchPolicyModel) WithSession(session sqlx.Session) DispatchPolicyModel {
	return &defaultDispatchPolicyModel{conn: pick(session, m.conn)}
}

const dispatchPolicyColumns = `id, version, state, sample_rules, salt_version, salt_ref,
	field_whitelist, drop_fields, max_events_per_batch, max_request_bytes, max_event_payload_bytes,
	max_clock_skew_seconds, max_backfill_seconds, keyword_max_runes, retention_days, deliver_max_attempts,
	retry_base_seconds, retry_max_seconds, note, operator, ctime, mtime`

// dispatchPolicyInsertValues 是 InsertDraft 的 VALUES 占位符个数（列清单同函数）。
const dispatchPolicyInsertValues = 22

func checkPolicy(p *DispatchPolicy) error {
	if p == nil {
		return ErrPolicyNotFound
	}
	if strings.TrimSpace(p.Version) == "" || len(p.Version) > 64 {
		return fmt.Errorf("event-collector: policy version required (<=64 chars)")
	}
	if p.SaltVersion <= 0 {
		return ErrSaltMissing
	}
	if strings.TrimSpace(p.SaltRef) == "" {
		return ErrSaltMissing
	}
	if p.MaxEventsPerBatch <= 0 || p.MaxRequestBytes <= 0 || p.MaxEventPayloadBytes <= 0 {
		return ErrBatchTooLarge
	}
	if p.MaxClockSkewSeconds <= 0 || p.MaxBackfillSeconds <= 0 {
		return ErrInvalidStateTransition
	}
	if p.DeliverMaxAttempts < 0 || p.RetryBaseSeconds <= 0 || p.RetryMaxSeconds < p.RetryBaseSeconds {
		return ErrInvalidStateTransition
	}
	rules, err := DecodeSampleRules(p.SampleRulesJSON)
	if err != nil {
		return fmt.Errorf("event-collector: invalid sample_rules json: %w", err)
	}
	if !ValidateSampleRules(rules) {
		return ErrInvalidStateTransition
	}
	return nil
}

func (m *defaultDispatchPolicyModel) InsertDraft(ctx context.Context, session sqlx.Session,
	p *DispatchPolicy) (int64, bool, error) {
	if err := checkPolicy(p); err != nil {
		return 0, false, err
	}
	db := pick(session, m.conn)
	now := nowUnix()
	res, err := db.ExecCtx(ctx,
		"INSERT IGNORE INTO ec_dispatch_policy (version, state, active_flag, sample_rules, salt_version, salt_ref, "+
			"field_whitelist, drop_fields, max_events_per_batch, max_request_bytes, max_event_payload_bytes, "+
			"max_clock_skew_seconds, max_backfill_seconds, keyword_max_runes, retention_days, deliver_max_attempts, "+
			"retry_base_seconds, retry_max_seconds, note, operator, ctime, mtime) "+
			"VALUES ("+placeholders(dispatchPolicyInsertValues)+")",
		p.Version, PolicyStateDraft, nil, p.SampleRulesJSON, p.SaltVersion, p.SaltRef, p.FieldWhitelistJSON,
		p.DropFieldsJSON, p.MaxEventsPerBatch, p.MaxRequestBytes, p.MaxEventPayloadBytes, p.MaxClockSkewSeconds,
		p.MaxBackfillSeconds, p.KeywordMaxRunes, p.RetentionDays, p.DeliverMaxAttempts, p.RetryBaseSeconds,
		p.RetryMaxSeconds, truncate(p.Note, 512), truncate(p.Operator, 64), now, now)
	if err != nil {
		if IsDuplicate(err) {
			exist, qerr := m.FindByVersion(ctx, p.Version)
			if qerr != nil {
				return 0, false, qerr
			}
			return exist.ID, false, nil
		}
		return 0, false, fmt.Errorf("ec_dispatch_policy InsertDraft: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("ec_dispatch_policy InsertDraft RowsAffected: %w", err)
	}
	if affected == 0 {
		exist, qerr := m.FindByVersion(ctx, p.Version)
		if qerr != nil {
			return 0, false, qerr
		}
		return exist.ID, false, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("ec_dispatch_policy InsertDraft LastInsertId: %w", err)
	}
	return id, true, nil
}

func (m *defaultDispatchPolicyModel) UpdateDraft(ctx context.Context, session sqlx.Session,
	p *DispatchPolicy) (bool, error) {
	if err := checkPolicy(p); err != nil {
		return false, err
	}
	db := pick(session, m.conn)
	res, err := db.ExecCtx(ctx,
		"UPDATE ec_dispatch_policy SET sample_rules = ?, salt_version = ?, salt_ref = ?, field_whitelist = ?, "+
			"drop_fields = ?, max_events_per_batch = ?, max_request_bytes = ?, max_event_payload_bytes = ?, "+
			"max_clock_skew_seconds = ?, max_backfill_seconds = ?, keyword_max_runes = ?, retention_days = ?, "+
			"deliver_max_attempts = ?, retry_base_seconds = ?, retry_max_seconds = ?, note = ?, operator = ?, mtime = ? "+
			"WHERE version = ? AND state = ?",
		p.SampleRulesJSON, p.SaltVersion, p.SaltRef, p.FieldWhitelistJSON, p.DropFieldsJSON, p.MaxEventsPerBatch,
		p.MaxRequestBytes, p.MaxEventPayloadBytes, p.MaxClockSkewSeconds, p.MaxBackfillSeconds, p.KeywordMaxRunes,
		p.RetentionDays, p.DeliverMaxAttempts, p.RetryBaseSeconds, p.RetryMaxSeconds, truncate(p.Note, 512),
		truncate(p.Operator, 64), nowUnix(), p.Version, PolicyStateDraft)
	if err != nil {
		return false, fmt.Errorf("ec_dispatch_policy UpdateDraft: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ec_dispatch_policy UpdateDraft RowsAffected: %w", err)
	}
	return n > 0, nil
}

func (m *defaultDispatchPolicyModel) FindByVersion(ctx context.Context, version string) (*DispatchPolicy, error) {
	if strings.TrimSpace(version) == "" {
		return nil, ErrPolicyNotFound
	}
	var row DispatchPolicy
	err := m.conn.QueryRowCtx(ctx, &row,
		"SELECT "+dispatchPolicyColumns+" FROM ec_dispatch_policy WHERE version = ? LIMIT 1", version)
	if err != nil {
		if errors.Is(err, sqlx.ErrNotFound) {
			return nil, ErrPolicyNotFound
		}
		return nil, fmt.Errorf("ec_dispatch_policy FindByVersion: %w", err)
	}
	return &row, nil
}

func (m *defaultDispatchPolicyModel) FindActive(ctx context.Context) (*DispatchPolicy, error) {
	var row DispatchPolicy
	err := m.conn.QueryRowCtx(ctx, &row,
		"SELECT "+dispatchPolicyColumns+" FROM ec_dispatch_policy WHERE active_flag = 1 LIMIT 1")
	if err != nil {
		if errors.Is(err, sqlx.ErrNotFound) {
			return nil, ErrNoActivePolicy
		}
		return nil, fmt.Errorf("ec_dispatch_policy FindActive: %w", err)
	}
	return &row, nil
}

func (m *defaultDispatchPolicyModel) Activate(ctx context.Context, session sqlx.Session,
	version, expectedCurrent, operator string) (string, error) {
	if strings.TrimSpace(version) == "" {
		return "", ErrPolicyNotFound
	}
	if strings.TrimSpace(operator) == "" {
		return "", ErrOperatorRequired
	}
	if session == nil {
		// 切换必须跑在调用方事务里：归档与激活分两个自动连接提交，
		// 中途失败会留下「零个或两个 ACTIVE」，比切换失败严重得多。
		return "", ErrInvalidStateTransition
	}
	db := pick(session, m.conn)

	// 1) 锁住当前 ACTIVE（uniq_active 至多一行），做乐观校验。
	var current string
	err := db.QueryRowCtx(ctx, &current,
		"SELECT version FROM ec_dispatch_policy WHERE active_flag = 1 LIMIT 1 FOR UPDATE")
	switch {
	case err == nil:
	case errors.Is(err, sqlx.ErrNotFound):
		current = ""
	default:
		return "", fmt.Errorf("ec_dispatch_policy Activate lock: %w", err)
	}
	if expectedCurrent != "" && current != expectedCurrent {
		return current, ErrConcurrentUpdate
	}
	if current == version {
		// 重复切换同一版本：幂等成功，不改写时间戳，避免刷掉 mtime 审计。
		return current, nil
	}

	// 2) 归档旧 ACTIVE（active_flag 置 NULL，为新版本让出唯一键）。
	if current != "" {
		if _, err := db.ExecCtx(ctx,
			"UPDATE ec_dispatch_policy SET state = ?, active_flag = NULL, mtime = ?, operator = ? "+
				"WHERE version = ? AND active_flag = 1", PolicyStateArchived, nowUnix(),
			truncate(operator, 64), current); err != nil {
			return current, fmt.Errorf("ec_dispatch_policy Activate archive: %w", err)
		}
	}

	// 3) 激活新版本：只接受 DRAFT，撞 uniq_active 即并发切换失败。
	res, err := db.ExecCtx(ctx,
		"UPDATE ec_dispatch_policy SET state = ?, active_flag = 1, mtime = ?, operator = ? "+
			"WHERE version = ? AND state = ?", PolicyStateActive, nowUnix(), truncate(operator, 64),
		version, PolicyStateDraft)
	if err != nil {
		if IsDuplicate(err) {
			return current, ErrConcurrentUpdate
		}
		return current, fmt.Errorf("ec_dispatch_policy Activate: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return current, fmt.Errorf("ec_dispatch_policy Activate RowsAffected: %w", err)
	}
	if n == 0 {
		target, qerr := m.FindByVersion(ctx, version)
		if qerr != nil {
			return current, qerr
		}
		if target.State == PolicyStateActive {
			return current, nil
		}
		return current, ErrPolicyNotDraft
	}
	return current, nil
}

func (m *defaultDispatchPolicyModel) Archive(ctx context.Context, session sqlx.Session,
	version, operator string) (bool, error) {
	if strings.TrimSpace(operator) == "" {
		return false, ErrOperatorRequired
	}
	db := pick(session, m.conn)
	res, err := db.ExecCtx(ctx,
		"UPDATE ec_dispatch_policy SET state = ?, active_flag = NULL, mtime = ?, operator = ? "+
			"WHERE version = ? AND state = ?", PolicyStateArchived, nowUnix(), truncate(operator, 64),
		version, PolicyStateDraft)
	if err != nil {
		return false, fmt.Errorf("ec_dispatch_policy Archive: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ec_dispatch_policy Archive RowsAffected: %w", err)
	}
	return n > 0, nil
}

func (m *defaultDispatchPolicyModel) List(ctx context.Context, state int32, afterID int64,
	ps int32) ([]*DispatchPolicy, error) {
	if ps <= 0 {
		return nil, ErrInvalidPage
	}
	conds := make([]string, 0, 2)
	args := make([]any, 0, 2)
	if state != 0 {
		if !ValidPolicyState(state) {
			return nil, ErrInvalidStateTransition
		}
		conds = append(conds, "state = ?")
		args = append(args, state)
	}
	if afterID > 0 {
		conds = append(conds, "id < ?")
		args = append(args, afterID)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	var rows []*DispatchPolicy
	query := "SELECT " + dispatchPolicyColumns + " FROM ec_dispatch_policy" + where + " ORDER BY id DESC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, append(args, ps)...); err != nil {
		if errors.Is(err, sqlx.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("ec_dispatch_policy List: %w", err)
	}
	return rows, nil
}

func (m *defaultDispatchPolicyModel) Count(ctx context.Context, state int32) (int64, error) {
	var total int64
	if state == 0 {
		if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(1) FROM ec_dispatch_policy"); err != nil {
			return 0, fmt.Errorf("ec_dispatch_policy Count: %w", err)
		}
		return total, nil
	}
	if !ValidPolicyState(state) {
		return 0, ErrInvalidStateTransition
	}
	err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(1) FROM ec_dispatch_policy WHERE state = ?", state)
	if err != nil {
		return 0, fmt.Errorf("ec_dispatch_policy Count: %w", err)
	}
	return total, nil
}
