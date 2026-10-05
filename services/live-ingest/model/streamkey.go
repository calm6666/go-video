package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// streamKeyColumns 是 live_stream_key 的列清单，必须与
// deploy/migrations/live-ingest/000001_create_live_ingest_key_tables.sql 完全一致。
// 注意清单里没有明文密钥列：本表只有 key_hash / key_ref / key_tail（README 约束）。
const streamKeyColumns = "key_id, stream_name, key_hash, key_ref, key_tail, room_id, session_id, anchor_mid, " +
	"protocol_mask, state, version, prev_key_id, rotate_to_key_id, current_stream_id, max_streams, " +
	"expire_at, grace_until, reason, request_id, trace_id, ctime, mtime"

// StreamKey 推流密钥行（live_stream_key 表投影）。
//
// 安全约束（AGENTS.md §5/§6，README「不保存长期明文推流密钥」）：
//   - KeyHash 是明文密钥的 SHA-256 hex，鉴权只比对哈希；
//   - KeyRef 是 Secret/Vault 引用（明文若需回填由 Vault 侧管理，本服务不写不读明文）；
//   - KeyTail 只有末 4 位，仅供主播在多个密钥之间辨认，绝不参与鉴权；
//   - 任何字段都不得出现在日志或列表响应里（KeyHash 同样不回显）。
type StreamKey struct {
	KeyID           int64  `db:"key_id"`            // 自增主键
	StreamName      string `db:"stream_name"`       // 流标识（推流 URL 的 name 段，可下发客户端）
	KeyHash         string `db:"key_hash"`          // 明文密钥 SHA-256 hex（64 字符）
	KeyRef          string `db:"key_ref"`           // Secret/Vault 引用，明文永不入库
	KeyTail         string `db:"key_tail"`          // 明文末 4 位，仅供辨认
	RoomID          int64  `db:"room_id"`           // 房间引用（live-room 主键）
	SessionID       int64  `db:"session_id"`        // 场次引用，0 表示未绑定
	AnchorMid       int64  `db:"anchor_mid"`        // 主播 ID
	ProtocolMask    uint32 `db:"protocol_mask"`     // 允许协议位图，见 ProtocolMask*
	State           int32  `db:"state"`             // 见 KeyState*
	Version         int32  `db:"version"`           // 轮转代次，从 1 递增
	PrevKeyID       int64  `db:"prev_key_id"`       // 轮转来源，0 表示首发
	RotateToKeyID   int64  `db:"rotate_to_key_id"`  // 轮转后继，0 表示无
	CurrentStreamID string `db:"current_stream_id"` // 当前非终态流 ID，空串表示无活跃流
	MaxStreams      int32  `db:"max_streams"`       // 并发非终态流上限
	ExpireAt        int64  `db:"expire_at"`         // 过期时间（Unix 秒）
	GraceUntil      int64  `db:"grace_until"`       // 轮转宽限截止（Unix 秒），0 表示不适用
	Reason          string `db:"reason"`            // 最近一次状态变更原因
	RequestID       string `db:"request_id"`        // 签发/轮转幂等键（唯一索引）
	TraceID         string `db:"trace_id"`          // 链路追踪 ID
	Ctime           int64  `db:"ctime"`             // 创建时间（Unix 秒）
	Mtime           int64  `db:"mtime"`             // 修改时间（Unix 秒）
}

// StreamKeyFilter 是密钥列表查询条件；零值表示该维度不过滤。
type StreamKeyFilter struct {
	RoomID    int64
	AnchorMid int64
	State     int32
	Offset    int32
	Limit     int32
}

// StreamKeyModel live_stream_key 表查询与写入接口。
type StreamKeyModel interface {
	// Insert 写入密钥行并返回自增 key_id。uniq_request_id 与 uniq_key_hash 是
	// 幂等与唯一性的最终防线：并发重复签发会返回错误，调用方回查后按重放处理。
	// tx 非空时在业务事务内执行（轮转要与旧密钥状态变更同事务）。
	Insert(ctx context.Context, tx sqlx.Session, k *StreamKey) (int64, error)
	// FindOne 按 key_id 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, keyID int64) (*StreamKey, error)
	// LockByID 在事务内对该密钥行加写锁（SELECT ... FOR UPDATE），供轮转/吊销
	// 串行化「读状态 → 迁移 → 写后继」；tx 为空时直接拒绝，不给逻辑层
	// 一个「看起来能跑但会丢更新」的假象（与 StreamModel.LockByID 同一纪律）。
	LockByID(ctx context.Context, tx sqlx.Session, keyID int64) (*StreamKey, error)
	// FindByKeyHash 按明文密钥的 SHA-256 hex 查询（接入鉴权主路径）。
	FindByKeyHash(ctx context.Context, hash string) (*StreamKey, error)
	// FindByIdempotencyRequest 按签发/轮转幂等键查询；不存在返回 (nil, nil)。
	FindByIdempotencyRequest(ctx context.Context, requestID string) (*StreamKey, error)
	// FindCurrentByStreamName 返回该流标识当前可用的密钥（ACTIVE 或宽限期内的 ROTATING），
	// 按 key_id 倒序取一条；不存在返回 (nil, nil)。
	FindCurrentByStreamName(ctx context.Context, streamName string) (*StreamKey, error)
	// TransitionState 以 fromStates 为条件推进到 toState（CAS 语义）。
	// 返回 false 表示状态被并发修改或前置状态不符，调用方需重读。
	TransitionState(ctx context.Context, tx sqlx.Session, keyID int64, toState int32, reason string, fromStates ...int32) (bool, error)
	// LinkRotation 把旧密钥标记为 ROTATING 并回填后继与宽限截止时间；
	// 仅当旧密钥当前是 ACTIVE 时生效（防止并发轮转分叉）。
	LinkRotation(ctx context.Context, tx sqlx.Session, oldKeyID, newKeyID, graceUntil int64) (bool, error)
	// ClaimActiveStream 占用密钥的活跃流指针：仅当 current_stream_id 为空时写入，
	// 用于「同一密钥至多一条非终态流」的强约束（配合 max_streams 做放宽）。
	ClaimActiveStream(ctx context.Context, tx sqlx.Session, keyID int64, streamID string) (bool, error)
	// ReleaseActiveStream 释放活跃流指针：仅当指针正好指向 streamID 时清空，
	// 避免误清后继流。
	ReleaseActiveStream(ctx context.Context, tx sqlx.Session, keyID int64, streamID string) (bool, error)
	// CountActiveStreams 统计该密钥当前非终态流数量（配额判定）。
	CountActiveStreams(ctx context.Context, keyID int64) (int64, error)
	// ListByFilter 按条件分页查询，按 key_id 倒序，必须带 LIMIT。
	ListByFilter(ctx context.Context, f StreamKeyFilter) ([]*StreamKey, error)
	// CountByFilter 统计条件命中行数（分页 total）。
	CountByFilter(ctx context.Context, f StreamKeyFilter) (int64, error)
	// MarkExpired 把已过期但仍 ACTIVE/ROTATING 的密钥批量置 EXPIRED，返回影响行数。
	// 由 cron/后台清扫任务调用，limit 强制生效避免长事务。
	MarkExpired(ctx context.Context, now, limit int32) (int64, error)
}

type defaultStreamKeyModel struct {
	conn sqlx.SqlConn
}

// NewStreamKeyModel 创建 StreamKeyModel 实现。
func NewStreamKeyModel(conn sqlx.SqlConn) StreamKeyModel {
	return &defaultStreamKeyModel{conn: conn}
}

func (m *defaultStreamKeyModel) Insert(ctx context.Context, tx sqlx.Session, k *StreamKey) (int64, error) {
	session := pickSession(m.conn, tx)
	if k.Ctime == 0 {
		k.Ctime = nowUnix()
	}
	k.Mtime = k.Ctime
	res, err := session.ExecCtx(ctx,
		"INSERT INTO live_stream_key (stream_name, key_hash, key_ref, key_tail, room_id, session_id, anchor_mid, "+
			"protocol_mask, state, version, prev_key_id, rotate_to_key_id, current_stream_id, max_streams, "+
			"expire_at, grace_until, reason, request_id, trace_id, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		k.StreamName, k.KeyHash, k.KeyRef, k.KeyTail, k.RoomID, k.SessionID, k.AnchorMid,
		k.ProtocolMask, k.State, k.Version, k.PrevKeyID, k.RotateToKeyID, k.CurrentStreamID, k.MaxStreams,
		k.ExpireAt, k.GraceUntil, k.Reason, k.RequestID, k.TraceID, k.Ctime, k.Mtime)
	if err != nil {
		return 0, fmt.Errorf("live_stream_key Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_stream_key Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultStreamKeyModel) FindOne(ctx context.Context, keyID int64) (*StreamKey, error) {
	var k StreamKey
	query := "SELECT " + streamKeyColumns + " FROM live_stream_key WHERE key_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &k, query, keyID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_key FindOne: %w", err)
	}
	return &k, nil
}

func (m *defaultStreamKeyModel) LockByID(ctx context.Context, tx sqlx.Session, keyID int64) (*StreamKey, error) {
	if tx == nil {
		return nil, errors.New("live_stream_key LockByID requires a transaction session")
	}
	if keyID <= 0 {
		return nil, ErrInvalidKeyId
	}
	var k StreamKey
	query := "SELECT " + streamKeyColumns + " FROM live_stream_key WHERE key_id = ? FOR UPDATE"
	if err := tx.QueryRowCtx(ctx, &k, query, keyID); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_key LockByID: %w", err)
	}
	return &k, nil
}

func (m *defaultStreamKeyModel) FindByKeyHash(ctx context.Context, hash string) (*StreamKey, error) {
	var k StreamKey
	query := "SELECT " + streamKeyColumns + " FROM live_stream_key WHERE key_hash = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &k, query, hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_key FindByKeyHash: %w", err)
	}
	return &k, nil
}

func (m *defaultStreamKeyModel) FindByIdempotencyRequest(ctx context.Context, requestID string) (*StreamKey, error) {
	var k StreamKey
	query := "SELECT " + streamKeyColumns + " FROM live_stream_key WHERE request_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &k, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_key FindByIdempotencyRequest: %w", err)
	}
	return &k, nil
}

func (m *defaultStreamKeyModel) FindCurrentByStreamName(ctx context.Context, streamName string) (*StreamKey, error) {
	var k StreamKey
	query := "SELECT " + streamKeyColumns + " FROM live_stream_key " +
		"WHERE stream_name = ? AND state IN (?, ?) ORDER BY key_id DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &k, query, streamName, KeyStateActive, KeyStateRotating); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_key FindCurrentByStreamName: %w", err)
	}
	return &k, nil
}

func (m *defaultStreamKeyModel) TransitionState(
	ctx context.Context,
	tx sqlx.Session,
	keyID int64,
	toState int32,
	reason string,
	fromStates ...int32,
) (bool, error) {
	if keyID <= 0 || !validKeyState(toState) {
		return false, ErrInvalidKeyId
	}
	session := pickSession(m.conn, tx)

	placeholders := strings.Repeat("?,", len(fromStates))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]interface{}, 0, len(fromStates)+4)
	args = append(args, toState, truncate(reason, 255), nowUnix(), keyID)
	for _, s := range fromStates {
		args = append(args, s)
	}
	query := fmt.Sprintf(
		"UPDATE live_stream_key SET state = ?, reason = ?, mtime = ? WHERE key_id = ? AND state IN (%s)",
		placeholders)
	res, err := session.ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("live_stream_key TransitionState: %w", err)
	}
	return rowsAffected(res, "live_stream_key TransitionState")
}

func (m *defaultStreamKeyModel) LinkRotation(
	ctx context.Context,
	tx sqlx.Session,
	oldKeyID, newKeyID, graceUntil int64,
) (bool, error) {
	session := pickSession(m.conn, tx)
	res, err := session.ExecCtx(ctx,
		"UPDATE live_stream_key SET state = ?, rotate_to_key_id = ?, grace_until = ?, mtime = ? "+
			"WHERE key_id = ? AND state = ?",
		KeyStateRotating, newKeyID, graceUntil, nowUnix(), oldKeyID, KeyStateActive)
	if err != nil {
		return false, fmt.Errorf("live_stream_key LinkRotation: %w", err)
	}
	return rowsAffected(res, "live_stream_key LinkRotation")
}

func (m *defaultStreamKeyModel) ClaimActiveStream(ctx context.Context, tx sqlx.Session, keyID int64, streamID string) (bool, error) {
	session := pickSession(m.conn, tx)
	res, err := session.ExecCtx(ctx,
		"UPDATE live_stream_key SET current_stream_id = ?, mtime = ? WHERE key_id = ? AND current_stream_id = ''",
		streamID, nowUnix(), keyID)
	if err != nil {
		return false, fmt.Errorf("live_stream_key ClaimActiveStream: %w", err)
	}
	return rowsAffected(res, "live_stream_key ClaimActiveStream")
}

func (m *defaultStreamKeyModel) ReleaseActiveStream(ctx context.Context, tx sqlx.Session, keyID int64, streamID string) (bool, error) {
	session := pickSession(m.conn, tx)
	res, err := session.ExecCtx(ctx,
		"UPDATE live_stream_key SET current_stream_id = '', mtime = ? WHERE key_id = ? AND current_stream_id = ?",
		nowUnix(), keyID, streamID)
	if err != nil {
		return false, fmt.Errorf("live_stream_key ReleaseActiveStream: %w", err)
	}
	return rowsAffected(res, "live_stream_key ReleaseActiveStream")
}

func (m *defaultStreamKeyModel) CountActiveStreams(ctx context.Context, keyID int64) (int64, error) {
	var cnt int64
	in, args := statePlaceholders()
	query := "SELECT COUNT(*) FROM live_stream WHERE key_id = ? AND state IN (" + in + ")"
	args = append([]interface{}{keyID}, args...)
	if err := m.conn.QueryRowCtx(ctx, &cnt, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_stream_key CountActiveStreams: %w", err)
	}
	return cnt, nil
}

func (m *defaultStreamKeyModel) ListByFilter(ctx context.Context, f StreamKeyFilter) ([]*StreamKey, error) {
	where, args := f.build()
	tail, tailArgs := pageArgs(f.Limit, 100, f.Offset)
	query := "SELECT " + streamKeyColumns + " FROM live_stream_key WHERE " + where +
		" ORDER BY key_id DESC" + tail
	args = append(args, tailArgs...)

	var rows []*StreamKey
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_key ListByFilter: %w", err)
	}
	return rows, nil
}

func (m *defaultStreamKeyModel) CountByFilter(ctx context.Context, f StreamKeyFilter) (int64, error) {
	where, args := f.build()
	var cnt int64
	if err := m.conn.QueryRowCtx(ctx, &cnt, "SELECT COUNT(*) FROM live_stream_key WHERE "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_stream_key CountByFilter: %w", err)
	}
	return cnt, nil
}

func (m *defaultStreamKeyModel) MarkExpired(ctx context.Context, now, limit int32) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE live_stream_key SET state = ?, reason = 'expired', mtime = ? "+
			"WHERE state IN (?, ?) AND expire_at > 0 AND expire_at <= ? LIMIT ?",
		KeyStateExpired, nowUnix(), KeyStateActive, KeyStateRotating, now, clampLimit(limit, 200))
	if err != nil {
		return 0, fmt.Errorf("live_stream_key MarkExpired: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_stream_key MarkExpired RowsAffected: %w", err)
	}
	return affected, nil
}

// build 组装 WHERE 片段与参数；恒真条件用 state <> 0 占位（state 列非 0）。
func (f StreamKeyFilter) build() (string, []interface{}) {
	conds := []string{"state <> 0"}
	args := make([]interface{}, 0, 4)
	if f.RoomID > 0 {
		conds = append(conds, "room_id = ?")
		args = append(args, f.RoomID)
	}
	if f.AnchorMid > 0 {
		conds = append(conds, "anchor_mid = ?")
		args = append(args, f.AnchorMid)
	}
	if validKeyState(f.State) {
		conds = append(conds, "state = ?")
		args = append(args, f.State)
	}
	return strings.Join(conds, " AND "), args
}

func validKeyState(state int32) bool {
	switch state {
	case KeyStateActive, KeyStateRotating, KeyStateRetired, KeyStateExpired, KeyStateRevoked:
		return true
	default:
		return false
	}
}
