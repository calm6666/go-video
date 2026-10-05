package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// progressColumns 是 playback_progress 的查询列清单，progressInsertColumns 是写入列清单
// （不含自增主键），二者必须与
// deploy/migrations/playback/000001_create_playback_tables.sql 完全一致。
const progressColumns = "id, session_id, content_type, content_id, vid, mid, position_ms, " +
	"duration_ms, buffer_count, avg_bitrate, last_error, ctime, mtime"

const progressInsertColumns = "session_id, content_type, content_id, vid, mid, position_ms, " +
	"duration_ms, buffer_count, avg_bitrate, last_error, ctime, mtime"

// PlaybackProgress 播放进度（playback_progress 表）。
// 一个会话一行（uniq_session_id），心跳按 session_id 幂等 upsert；
// position_ms 只前进不回退，客户端乱序/重复上报不会破坏断点。
type PlaybackProgress struct {
	ID          int64  `db:"id"`           // 自增主键
	SessionId   string `db:"session_id"`   // 播放会话 ID（唯一索引）
	ContentType int32  `db:"content_type"` // 内容类型：1 UGC、2 PGC
	ContentId   int64  `db:"content_id"`   // 内容 ID
	Vid         string `db:"vid"`          // UGC bvid（可为空）
	Mid         int64  `db:"mid"`          // 观看者用户 ID
	PositionMs  int64  `db:"position_ms"`  // 服务端记录的最大播放位置（毫秒）
	DurationMs  int64  `db:"duration_ms"`  // 内容总时长（毫秒）
	BufferCount int32  `db:"buffer_count"` // 累计卡顿次数
	AvgBitrate  int64  `db:"avg_bitrate"`  // 累计平均码率（bps）
	LastError   int32  `db:"last_error"`   // 最近一次播放错误码，0 表示无错误
	Ctime       int64  `db:"ctime"`        // 创建时间（Unix 秒）
	Mtime       int64  `db:"mtime"`        // 修改时间（Unix 秒）
}

// PlaybackProgressModel playback_progress 表查询与写入接口。
type PlaybackProgressModel interface {
	// Upsert 按 session_id 幂等写入心跳；返回落库后的进度行。
	Upsert(ctx context.Context, tx sqlx.Session, p *PlaybackProgress) (*PlaybackProgress, error)
	// FindOne 按 session_id 查询进度；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, sessionId string) (*PlaybackProgress, error)
	// FindLatestByMid 查询某用户对某内容的最新进度（断点续播）；不存在返回 (nil, nil)。
	FindLatestByMid(ctx context.Context, mid int64, contentType int32, contentId int64) (*PlaybackProgress, error)
}

type defaultPlaybackProgressModel struct {
	conn sqlx.SqlConn
}

// NewPlaybackProgressModel 创建 PlaybackProgressModel 实现。
func NewPlaybackProgressModel(conn sqlx.SqlConn) PlaybackProgressModel {
	return &defaultPlaybackProgressModel{conn: conn}
}

// execer 让 Upsert 既能在事务内（sqlx.Session）执行，也能在自动提交下执行。
type execer interface {
	ExecCtx(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowCtx(ctx context.Context, v any, query string, args ...any) error
}

func (m *defaultPlaybackProgressModel) Upsert(ctx context.Context, tx sqlx.Session, p *PlaybackProgress) (*PlaybackProgress, error) {
	var session sqlx.Session = tx
	if session == nil {
		session = m.conn
	}
	if p.Ctime == 0 {
		p.Ctime = nowUnix()
	}
	p.Mtime = p.Ctime
	// 位置取最大值，避免弱网重试的旧心跳把断点回退；buffer_count 累加，
	// avg_bitrate/last_error 以最新心跳为准（客户端上报的是累计值）。
	_, err := session.ExecCtx(ctx,
		"INSERT INTO playback_progress ("+progressInsertColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE position_ms = GREATEST(position_ms, VALUES(position_ms)), "+
			"duration_ms = VALUES(duration_ms), buffer_count = buffer_count + VALUES(buffer_count), "+
			"avg_bitrate = VALUES(avg_bitrate), last_error = VALUES(last_error), mtime = VALUES(mtime)",
		p.SessionId, p.ContentType, p.ContentId, p.Vid, p.Mid, p.PositionMs,
		p.DurationMs, p.BufferCount, p.AvgBitrate, p.LastError, p.Ctime, p.Mtime)
	if err != nil {
		return nil, fmt.Errorf("playback_progress Upsert: %w", err)
	}
	return m.findWith(ctx, session, p.SessionId)
}

func (m *defaultPlaybackProgressModel) FindOne(ctx context.Context, sessionId string) (*PlaybackProgress, error) {
	return m.findWith(ctx, m.conn, sessionId)
}

func (m *defaultPlaybackProgressModel) findWith(ctx context.Context, ex execer, sessionId string) (*PlaybackProgress, error) {
	var p PlaybackProgress
	query := "SELECT " + progressColumns + " FROM playback_progress WHERE session_id = ?"
	if err := ex.QueryRowCtx(ctx, &p, query, sessionId); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("playback_progress FindOne: %w", err)
	}
	return &p, nil
}

func (m *defaultPlaybackProgressModel) FindLatestByMid(ctx context.Context, mid int64, contentType int32, contentId int64) (*PlaybackProgress, error) {
	var p PlaybackProgress
	query := "SELECT " + progressColumns + " FROM playback_progress " +
		"WHERE mid = ? AND content_type = ? AND content_id = ? ORDER BY mtime DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &p, query, mid, contentType, contentId); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("playback_progress FindLatestByMid: %w", err)
	}
	return &p, nil
}
