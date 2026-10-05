package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// liveRoomIdempotencyColumns 与 000008_create_live_room_idempotency.sql 逐列对应。
const liveRoomIdempotencyColumns = "id, dedup_key, kind, rpc, room_id, session_id, result_json, trace_id, ctime, mtime"

// LiveRoomIdempotency 幂等/事件去重行（live_room_idempotency 表投影）。
//
// 一张表同时承担两类去重锚点，靠 uniq_dedup_key 保证「同一键只被处理一次」：
//   - kind=IdempotencyKindRequest：客户端 request_id（CreateRoom/StartLive/BanRoom… 重试）；
//   - kind=IdempotencyKindEvent：上游 event_id（live.state.v1 经 ReportStreamState、
//     moderation.result.v1 经 ApplyRoomModerationResult）。
//
// result_json 保存首次执行的响应快照（JSON，已脱敏），重放时原样返回，
// 保证「客户端重试拿到同一个 session_id / ban_id」而不是第二次副作用。
type LiveRoomIdempotency struct {
	ID         int64  `db:"id"`          // 自增主键
	DedupKey   string `db:"dedup_key"`   // 去重键：request_id 或 event_id（uniq_dedup_key）
	Kind       int32  `db:"kind"`        // 1 request_id、2 event_id
	Rpc        string `db:"rpc"`         // 产生该键的 RPC 方法名（如 StartLive）
	RoomID     int64  `db:"room_id"`     // 关联房间 ID（观测与按房间清理）
	SessionID  int64  `db:"session_id"`  // 关联场次 ID，0 表示无
	ResultJSON string `db:"result_json"` // 首次执行响应快照（JSON，脱敏），空表示尚未回填
	TraceID    string `db:"trace_id"`    // 首次执行的链路追踪 ID
	Ctime      int64  `db:"ctime"`       // 首次受理时间（Unix 秒）
	Mtime      int64  `db:"mtime"`       // 结果回填时间（Unix 秒）
}

// LiveRoomIdempotencyModel live_room_idempotency 表读写接口。
type LiveRoomIdempotencyModel interface {
	// Claim 抢占一个去重键：INSERT ... ON DUPLICATE KEY UPDATE id=id。
	// 返回 true 表示本次是首次受理（可继续执行业务）；
	// 返回 false 表示键已存在（重放或重复投递），调用方必须回查结果后返回，
	// 不得再次产生副作用。数据库唯一键是最终防线，Redis 只能作为前置加速。
	Claim(ctx context.Context, rec *LiveRoomIdempotency) (bool, error)
	// Find 按去重键读记录；不存在返回 (nil, nil)。
	Find(ctx context.Context, dedupKey string) (*LiveRoomIdempotency, error)
	// SaveResult 回填首次执行的响应快照，供后续重放返回同一结果。
	// 只在 result_json 为空时写入（先到先得，后到的重放不覆盖既有事实）。
	SaveResult(ctx context.Context, dedupKey, resultJSON string) (bool, error)
	// DeleteExpired 删除早于 before 的记录（按 ctime，limit 截断），返回删除行数。
	// 保留窗口必须大于客户端最大重试间隔与事件最大重投间隔，否则幂等失效。
	DeleteExpired(ctx context.Context, before int64, limit int64) (int64, error)
}

type defaultLiveRoomIdempotencyModel struct {
	conn sqlx.SqlConn
}

// NewLiveRoomIdempotencyModel 创建 LiveRoomIdempotencyModel 实现。
func NewLiveRoomIdempotencyModel(conn sqlx.SqlConn) LiveRoomIdempotencyModel {
	return &defaultLiveRoomIdempotencyModel{conn: conn}
}

func (m *defaultLiveRoomIdempotencyModel) Claim(ctx context.Context, rec *LiveRoomIdempotency) (bool, error) {
	if rec.DedupKey == "" {
		return false, ErrRequestIDRequired
	}
	if rec.Kind != IdempotencyKindRequest && rec.Kind != IdempotencyKindEvent {
		return false, ErrIdempotencyKindInvalid
	}
	const query = "INSERT INTO live_room_idempotency (dedup_key, kind, rpc, room_id, session_id, " +
		"result_json, trace_id, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) " +
		"ON DUPLICATE KEY UPDATE id = id"
	now := nowUnix()
	if rec.Ctime == 0 {
		rec.Ctime = now
	}
	rec.Mtime = now
	res, err := m.conn.ExecCtx(ctx, query,
		rec.DedupKey, rec.Kind, rec.Rpc, rec.RoomID, rec.SessionID, rec.ResultJSON, rec.TraceID, rec.Ctime, rec.Mtime)
	if err != nil {
		return false, fmt.Errorf("live_room_idempotency Claim: %w", err)
	}
	// `ON DUPLICATE KEY UPDATE id = id` 是「无变化更新」：
	// MySQL 对已存在行返回 affected=0，对真正插入的行返回 1，据此区分首次/重放。
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("live_room_idempotency Claim RowsAffected: %w", err)
	}
	return aff == 1, nil
}

func (m *defaultLiveRoomIdempotencyModel) Find(ctx context.Context, dedupKey string) (*LiveRoomIdempotency, error) {
	if dedupKey == "" {
		return nil, ErrRequestIDRequired
	}
	var rec LiveRoomIdempotency
	query := "SELECT " + liveRoomIdempotencyColumns + " FROM live_room_idempotency WHERE dedup_key = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &rec, query, dedupKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_room_idempotency Find: %w", err)
	}
	return &rec, nil
}

func (m *defaultLiveRoomIdempotencyModel) SaveResult(ctx context.Context, dedupKey, resultJSON string) (bool, error) {
	if dedupKey == "" {
		return false, ErrRequestIDRequired
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE live_room_idempotency SET result_json = ?, mtime = ? WHERE dedup_key = ? AND result_json = ''",
		resultJSON, nowUnix(), dedupKey)
	if err != nil {
		return false, fmt.Errorf("live_room_idempotency SaveResult: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("live_room_idempotency SaveResult RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultLiveRoomIdempotencyModel) DeleteExpired(ctx context.Context, before, limit int64) (int64, error) {
	if before <= 0 {
		return 0, ErrQueryRangeRequired
	}
	if limit <= 0 {
		limit = int64(defaultListLimit)
	}
	res, err := m.conn.ExecCtx(ctx,
		"DELETE FROM live_room_idempotency WHERE ctime < ? ORDER BY id ASC LIMIT ?", before, limit)
	if err != nil {
		return 0, fmt.Errorf("live_room_idempotency DeleteExpired: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_room_idempotency DeleteExpired RowsAffected: %w", err)
	}
	return aff, nil
}
