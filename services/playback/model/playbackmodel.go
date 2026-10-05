package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// sessionColumns 是 playback_session 的列清单，必须与
// deploy/migrations/playback/000001_create_playback_tables.sql 完全一致。
const sessionColumns = "session_id, content_type, content_id, vid, mid, platform, app_version, " +
	"region, object_key, uri, request_id, expire_at, state, trace_id, ctime, mtime"

// PlaybackSession 播放会话（playback_session 表）。
// 一次签发 = 一行；request_id 唯一索引保证同一请求重放只产生一个会话。
// 会话只记录"谁在什么端对哪个可播放版本拿到了到 expire_at 的授权"，
// 不复制稿件/媒资/版权主数据（AGENTS.md §5）。
type PlaybackSession struct {
	SessionId   string `db:"session_id"`   // 会话 ID（ULID，主键）
	ContentType int32  `db:"content_type"` // 内容类型：1 UGC、2 PGC
	ContentId   int64  `db:"content_id"`   // 内容 ID：UGC=aid、PGC=episode_id
	Vid         string `db:"vid"`          // UGC bvid（排障用，可为空）
	Mid         int64  `db:"mid"`          // 观看者用户 ID，0 表示游客
	Platform    int32  `db:"platform"`     // 客户端平台：1 android、2 ios、3 harmony、4 desktop
	AppVersion  string `db:"app_version"`  // 客户端版本号
	Region      string `db:"region"`       // 地区代码
	ObjectKey   string `db:"object_key"`   // 媒资对象 key（调用方解析，本服务不校验其存在性）
	Uri         string `db:"uri"`          // 签名使用的 URI 路径（auth_key 与其绑定）
	RequestId   string `db:"request_id"`   // 幂等键（唯一索引）
	ExpireAt    int64  `db:"expire_at"`    // 授权过期时间（Unix 秒）
	State       int32  `db:"state"`        // 会话状态：见 SessionState* 常量
	TraceId     string `db:"trace_id"`     // 签发时的 trace_id
	Ctime       int64  `db:"ctime"`        // 创建时间（Unix 秒）
	Mtime       int64  `db:"mtime"`        // 修改时间（Unix 秒）
}

// PlaybackSessionModel playback_session 表查询与写入接口。
type PlaybackSessionModel interface {
	// Insert 新建播放会话；request_id 或 session_id 冲突时返回 ErrDuplicateRequest。
	Insert(ctx context.Context, s *PlaybackSession) error
	// FindOne 按 session_id 查询会话；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, sessionId string) (*PlaybackSession, error)
	// FindByRequest 按幂等键查询会话；不存在返回 (nil, nil)。
	FindByRequest(ctx context.Context, requestId string) (*PlaybackSession, error)
	// MarkExpired 把确实已过期的会话状态推进为 SessionStateExpired（幂等，0 行受影响不算错误）。
	MarkExpired(ctx context.Context, sessionId string, now int64) error
	// RevokeByContent 按内容撤销全部有效会话（版权撤回/下架时的授权收敛，由运营或 cron 触发）。
	RevokeByContent(ctx context.Context, contentType int32, contentId int64) (int64, error)
}

// ErrDuplicateRequest 表示 request_id 已存在（唯一索引冲突）。
// repository 捕获该错误后改为读取既有会话，实现幂等重放。
var ErrDuplicateRequest = errors.New("playback: duplicate request_id")

type defaultPlaybackSessionModel struct {
	conn sqlx.SqlConn
}

// NewPlaybackSessionModel 创建 PlaybackSessionModel 实现。
func NewPlaybackSessionModel(conn sqlx.SqlConn) PlaybackSessionModel {
	return &defaultPlaybackSessionModel{conn: conn}
}

func (m *defaultPlaybackSessionModel) Insert(ctx context.Context, s *PlaybackSession) error {
	if s.Ctime == 0 {
		s.Ctime = nowUnix()
	}
	s.Mtime = s.Ctime
	if s.State == 0 {
		s.State = SessionStateActive
	}
	// ON DUPLICATE KEY UPDATE mtime = mtime 是刻意的空更新：
	// 命中 uniq_request_id / PRIMARY 时 MySQL 返回 0 行受影响，据此识别重复请求，
	// 从而不依赖 driver 专有错误类型即可完成幂等判定。
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO playback_session ("+sessionColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE mtime = mtime",
		s.SessionId, s.ContentType, s.ContentId, s.Vid, s.Mid, s.Platform, s.AppVersion,
		s.Region, s.ObjectKey, s.Uri, s.RequestId, s.ExpireAt, s.State, s.TraceId, s.Ctime, s.Mtime)
	if err != nil {
		return fmt.Errorf("playback_session Insert: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("playback_session Insert RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrDuplicateRequest
	}
	return nil
}

func (m *defaultPlaybackSessionModel) FindOne(ctx context.Context, sessionId string) (*PlaybackSession, error) {
	var s PlaybackSession
	query := "SELECT " + sessionColumns + " FROM playback_session WHERE session_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &s, query, sessionId); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("playback_session FindOne: %w", err)
	}
	return &s, nil
}

func (m *defaultPlaybackSessionModel) FindByRequest(ctx context.Context, requestId string) (*PlaybackSession, error) {
	var s PlaybackSession
	query := "SELECT " + sessionColumns + " FROM playback_session WHERE request_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &s, query, requestId); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("playback_session FindByRequest: %w", err)
	}
	return &s, nil
}

func (m *defaultPlaybackSessionModel) MarkExpired(ctx context.Context, sessionId string, now int64) error {
	// 只推进确实过期且仍有效的会话；重复调用影响 0 行属于正常情况。
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE playback_session SET state = ?, mtime = ? WHERE session_id = ? AND state = ? AND expire_at <= ?",
		SessionStateExpired, nowUnix(), sessionId, SessionStateActive, now)
	if err != nil {
		return fmt.Errorf("playback_session MarkExpired: %w", err)
	}
	return nil
}

func (m *defaultPlaybackSessionModel) RevokeByContent(ctx context.Context, contentType int32, contentId int64) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE playback_session SET state = ?, mtime = ? WHERE content_type = ? AND content_id = ? AND state = ?",
		SessionStateRevoked, nowUnix(), contentType, contentId, SessionStateActive)
	if err != nil {
		return 0, fmt.Errorf("playback_session RevokeByContent: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("playback_session RevokeByContent RowsAffected: %w", err)
	}
	return aff, nil
}
