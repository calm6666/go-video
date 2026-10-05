package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// LiveGwAccessQuota 接入与广播配额（live_gw_access_quota）。
//
// 一行 = 一个作用域（全局/节点/房间/用户）的接入与广播上限配置。
// 0 值一律解释为「继承上一层」，只有 GLOBAL 行的 0 才落到进程配置的默认值
// （由 QuotaDefaults 传入，见 Resolve）：这样运营只想提一项配额时不必复制整行。
//
// 配额是"判定依据"而不是"计数器"：实时计数（当前连接数、QPS 窗口）在 Redis，
// 本表只存运营配置，改动必须带 version 与 operator 审计。
type LiveGwAccessQuota struct {
	Id               int64  `db:"id"`
	Scope            int32  `db:"scope"`              // QuotaScope*
	ScopeId          int64  `db:"scope_id"`           // GLOBAL=0、NODE=节点哈希、ROOM=room_id、USER=mid
	ScopeKey         string `db:"scope_key"`          // 可读标识（node_id 等），仅展示与排障
	MaxConnections   int32  `db:"max_connections"`    // 0 继承
	BroadcastQps     int32  `db:"broadcast_qps"`      // 0 继承
	DanmakuQps       int32  `db:"danmaku_qps"`        // 0 继承
	LeaseTtlSeconds  int32  `db:"lease_ttl_seconds"`  // 0 用默认
	TicketTtlSeconds int32  `db:"ticket_ttl_seconds"` // 0 用默认
	MaxPayloadBytes  int32  `db:"max_payload_bytes"`  // 0 继承
	AllowGuest       int32  `db:"allow_guest"`        // 0 继承（继承链最终由 QuotaDefaults 决定）
	Version          int64  `db:"version"`
	UpdatedBy        string `db:"updated_by"` // 最后修改者（运营账号，审计）
	RequestId        string `db:"request_id"` // 幂等键（唯一索引）
	TraceId          string `db:"trace_id"`
	Ctime            int64  `db:"ctime"`
	Mtime            int64  `db:"mtime"`
}

// QuotaDefaults 进程配置里的全局兜底值（来自 config.LiveGatewayConf）。
// 它是"配置"，不是"数据"：运营在 DB 里没写 GLOBAL 行时用它，写了则以 DB 为准。
type QuotaDefaults struct {
	MaxConnections   int32
	BroadcastQps     int32
	DanmakuQps       int32
	LeaseTtlSeconds  int32
	TicketTtlSeconds int32
	MaxPayloadBytes  int32
	AllowGuest       bool
}

// EffectiveQuota 继承链解析结果。HitScopes 记录"哪些层真正生效"，
// 让排障时能说清「这条连接被拒用的是哪一层配置」，而不是只给一个数字。
type EffectiveQuota struct {
	MaxConnections   int32
	BroadcastQps     int32
	DanmakuQps       int32
	LeaseTtlSeconds  int32
	TicketTtlSeconds int32
	MaxPayloadBytes  int32
	AllowGuest       bool
	HitScopes        []string
}

// AccessQuotaFilter List 过滤条件。
type AccessQuotaFilter struct {
	Scope       int32
	ScopeId     int64
	Pn          int32
	Ps          int32
	MaxPageSize int32
}

// LiveGwAccessQuotaModel live_gw_access_quota 表接口。
type LiveGwAccessQuotaModel interface {
	// Create 新建配额行（expected_version 语义为 0）：UNIQUE(scope,scope_id) 已存在时
	// 返回 ErrVersionConflict，强制调用方先读后带版本更新，避免运营盲写覆盖别人的配置。
	Create(ctx context.Context, q *LiveGwAccessQuota) (int64, error)
	// Update 条件 UPDATE 覆盖配置：WHERE (scope, scope_id, version=expected) 且 expected>0。
	// 返回受影响行数（0 行由调用方回读区分 ErrQuotaNotFound / ErrVersionConflict）。
	Update(ctx context.Context, scope int32, scopeID, expectedVersion int64, q *LiveGwAccessQuota) (int64, error)
	FindOne(ctx context.Context, scope int32, scopeID int64) (*LiveGwAccessQuota, error)
	FindByRequestID(ctx context.Context, requestID string) (*LiveGwAccessQuota, error)
	List(ctx context.Context, f AccessQuotaFilter) ([]*LiveGwAccessQuota, int32, error)
	// Resolve 按 QuotaScopeChain 自下而上解析生效配额（含继承），并给出命中层级。
	Resolve(ctx context.Context, scope int32, scopeID int64, defaults QuotaDefaults) (*EffectiveQuota, error)
}

const liveGwAccessQuotaColumns = "SELECT id, scope, scope_id, scope_key, max_connections, broadcast_qps, danmaku_qps, " +
	"lease_ttl_seconds, ticket_ttl_seconds, max_payload_bytes, allow_guest, version, updated_by, request_id, trace_id, ctime, mtime"

type defaultLiveGwAccessQuotaModel struct {
	conn sqlx.SqlConn
}

// NewLiveGwAccessQuotaModel 构造 live_gw_access_quota 的 model。
func NewLiveGwAccessQuotaModel(conn sqlx.SqlConn) LiveGwAccessQuotaModel {
	return &defaultLiveGwAccessQuotaModel{conn: conn}
}

// CheckQuotaBounds 校验配额的取值边界。max*<=0 表示该项不加上限（由调用方按配置传入）。
// 只拦「越界会直接打挂接入」的项：TTL 与载荷必须有界，越界会让客户端拿到
// 一个永远续不完 / 永远发不进的配置。0 是合法的（继承语义）。
func CheckQuotaBounds(q *LiveGwAccessQuota, maxLeaseTTL, maxTicketTTL, maxPayload int32) error {
	if !ValidQuotaScope(q.Scope) {
		return fmt.Errorf("scope=%d: %w", q.Scope, ErrInvalidQuotaScope)
	}
	if q.Scope == QuotaScopeGlobal && q.ScopeId != 0 {
		return fmt.Errorf("global scope needs scope_id=0, got %d: %w", q.ScopeId, ErrInvalidQuotaScope)
	}
	if q.Scope != QuotaScopeGlobal && q.ScopeId <= 0 {
		return fmt.Errorf("scope=%d needs positive scope_id: %w", q.Scope, ErrInvalidQuotaScope)
	}
	if q.LeaseTtlSeconds < 0 || (maxLeaseTTL > 0 && q.LeaseTtlSeconds > maxLeaseTTL) {
		return fmt.Errorf("lease_ttl_seconds=%d out of bounds: %w", q.LeaseTtlSeconds, ErrQuotaExceeded)
	}
	if q.TicketTtlSeconds < 0 || (maxTicketTTL > 0 && q.TicketTtlSeconds > maxTicketTTL) {
		return fmt.Errorf("ticket_ttl_seconds=%d out of bounds: %w", q.TicketTtlSeconds, ErrQuotaExceeded)
	}
	if q.MaxPayloadBytes < 0 || (maxPayload > 0 && q.MaxPayloadBytes > maxPayload) {
		return fmt.Errorf("max_payload_bytes=%d out of bounds: %w", q.MaxPayloadBytes, ErrPayloadTooLarge)
	}
	if q.AllowGuest < 0 || q.AllowGuest > 1 {
		return fmt.Errorf("allow_guest=%d must be 0 or 1", q.AllowGuest)
	}
	return nil
}

func (m *defaultLiveGwAccessQuotaModel) Create(ctx context.Context, q *LiveGwAccessQuota) (int64, error) {
	if q.RequestId == "" {
		return 0, ErrEmptyRequestID
	}
	if q.UpdatedBy == "" {
		return 0, ErrEmptyOperator
	}
	if err := CheckQuotaBounds(q, 0, 0, 0); err != nil {
		return 0, fmt.Errorf("live_gw_access_quota Create: %w", err)
	}
	now := nowUnix()
	if q.Ctime == 0 {
		q.Ctime = now
	}
	if q.Mtime == 0 {
		q.Mtime = now
	}
	if q.Version == 0 {
		q.Version = 1
	}
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO live_gw_access_quota (scope, scope_id, scope_key, max_connections, broadcast_qps, danmaku_qps, "+
			"lease_ttl_seconds, ticket_ttl_seconds, max_payload_bytes, allow_guest, version, updated_by, request_id, "+
			"trace_id, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		q.Scope, q.ScopeId, q.ScopeKey, q.MaxConnections, q.BroadcastQps, q.DanmakuQps, q.LeaseTtlSeconds,
		q.TicketTtlSeconds, q.MaxPayloadBytes, q.AllowGuest, q.Version, q.UpdatedBy, q.RequestId, q.TraceId,
		q.Ctime, q.Mtime)
	if err != nil {
		if isDuplicateErr(err) {
			// uniq_scope 命中：配置行已存在，必须走 Update（带 expected_version）而不是盲写覆盖。
			return 0, fmt.Errorf("scope=%d scope_id=%d: %w", q.Scope, q.ScopeId, ErrVersionConflict)
		}
		return 0, fmt.Errorf("live_gw_access_quota Create: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_gw_access_quota Create LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultLiveGwAccessQuotaModel) Update(ctx context.Context, scope int32, scopeID, expectedVersion int64,
	q *LiveGwAccessQuota) (int64, error) {
	if expectedVersion <= 0 {
		return 0, fmt.Errorf("live_gw_access_quota Update: expected_version=%d %w", expectedVersion, ErrVersionConflict)
	}
	if q.UpdatedBy == "" {
		return 0, ErrEmptyOperator
	}
	if err := CheckQuotaBounds(q, 0, 0, 0); err != nil {
		return 0, fmt.Errorf("live_gw_access_quota Update: %w", err)
	}
	// 这里是「整行覆盖写」：0 值继承的合并语义由 logic 先 FindOne 取当前行再算好传进来。
	// 不在 SQL 里玩 IF(col=0, old, new)：那种写法无法表达"这次就是要清零"，也让审计读不懂。
	sets := []columnValue{
		{"scope_key", q.ScopeKey},
		{"max_connections", q.MaxConnections},
		{"broadcast_qps", q.BroadcastQps},
		{"danmaku_qps", q.DanmakuQps},
		{"lease_ttl_seconds", q.LeaseTtlSeconds},
		{"ticket_ttl_seconds", q.TicketTtlSeconds},
		{"max_payload_bytes", q.MaxPayloadBytes},
		{"allow_guest", q.AllowGuest},
		{"updated_by", q.UpdatedBy},
		{"request_id", q.RequestId},
		{"trace_id", q.TraceId},
	}
	return conditionalUpdate(ctx, m.conn, "live_gw_access_quota", sets, true, []whereFragment{
		{"scope = ?", []any{scope}},
		{"scope_id = ?", []any{scopeID}},
		{"version = ?", []any{expectedVersion}},
	})
}

func (m *defaultLiveGwAccessQuotaModel) FindOne(ctx context.Context, scope int32, scopeID int64) (*LiveGwAccessQuota, error) {
	if !ValidQuotaScope(scope) {
		return nil, ErrInvalidQuotaScope
	}
	var q LiveGwAccessQuota
	query := liveGwAccessQuotaColumns + " FROM live_gw_access_quota WHERE scope = ? AND scope_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &q, query, scope, scopeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_gw_access_quota FindOne: %w", err)
	}
	return &q, nil
}

func (m *defaultLiveGwAccessQuotaModel) FindByRequestID(ctx context.Context, requestID string) (*LiveGwAccessQuota, error) {
	var q LiveGwAccessQuota
	query := liveGwAccessQuotaColumns + " FROM live_gw_access_quota WHERE request_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &q, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_gw_access_quota FindByRequestID: %w", err)
	}
	return &q, nil
}

func (m *defaultLiveGwAccessQuotaModel) List(ctx context.Context, f AccessQuotaFilter) ([]*LiveGwAccessQuota, int32, error) {
	where, args := buildWhere(
		whereFragment{"scope = ?", []any{}}.when(f.Scope > 0, f.Scope),
		whereFragment{"scope_id = ?", []any{}}.when(f.ScopeId > 0, f.ScopeId),
	)
	limit, offset := clampPage(f.Pn, f.Ps, f.MaxPageSize)

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM live_gw_access_quota "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("live_gw_access_quota List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), limit, offset)
	var rows []*LiveGwAccessQuota
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		liveGwAccessQuotaColumns+" FROM live_gw_access_quota "+where+
			" ORDER BY scope ASC, scope_id ASC LIMIT ? OFFSET ?", listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("live_gw_access_quota List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultLiveGwAccessQuotaModel) Resolve(ctx context.Context, scope int32, scopeID int64,
	defaults QuotaDefaults) (*EffectiveQuota, error) {
	if !ValidQuotaScope(scope) {
		return nil, ErrInvalidQuotaScope
	}
	eff := &EffectiveQuota{
		MaxConnections:   defaults.MaxConnections,
		BroadcastQps:     defaults.BroadcastQps,
		DanmakuQps:       defaults.DanmakuQps,
		LeaseTtlSeconds:  defaults.LeaseTtlSeconds,
		TicketTtlSeconds: defaults.TicketTtlSeconds,
		MaxPayloadBytes:  defaults.MaxPayloadBytes,
		AllowGuest:       defaults.AllowGuest,
	}
	// 自外向内覆盖：先 GLOBAL，再本层，保证"更具体的层赢"。
	chain := QuotaScopeChain(scope)
	for i := len(chain) - 1; i >= 0; i-- {
		layer := chain[i]
		layerID := int64(0)
		if layer != QuotaScopeGlobal {
			layerID = scopeID
		}
		row, err := m.FindOne(ctx, layer, layerID)
		if err != nil {
			return nil, err
		}
		if row == nil {
			continue
		}
		applyQuotaLayer(eff, row, layer)
	}
	return eff, nil
}

// applyQuotaLayer 把一层的非 0 值覆盖到生效配额上，并记录命中层级。
func applyQuotaLayer(eff *EffectiveQuota, row *LiveGwAccessQuota, layer int32) {
	hit := false
	if row.MaxConnections != 0 {
		eff.MaxConnections = row.MaxConnections
		hit = true
	}
	if row.BroadcastQps != 0 {
		eff.BroadcastQps = row.BroadcastQps
		hit = true
	}
	if row.DanmakuQps != 0 {
		eff.DanmakuQps = row.DanmakuQps
		hit = true
	}
	if row.LeaseTtlSeconds != 0 {
		eff.LeaseTtlSeconds = row.LeaseTtlSeconds
		hit = true
	}
	if row.TicketTtlSeconds != 0 {
		eff.TicketTtlSeconds = row.TicketTtlSeconds
		hit = true
	}
	if row.MaxPayloadBytes != 0 {
		eff.MaxPayloadBytes = row.MaxPayloadBytes
		hit = true
	}
	if row.AllowGuest != 0 {
		eff.AllowGuest = row.AllowGuest == 1
		hit = true
	}
	if hit {
		eff.HitScopes = append(eff.HitScopes, fmt.Sprintf("%d:%d", layer, row.ScopeId))
	}
}
