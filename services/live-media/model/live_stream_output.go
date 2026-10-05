package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// LiveStreamOutput 直播分发输出（live_stream_output）：一个房间某场次下某协议某档位的产物登记。
// 这是「直播实时链路」的表：断流即下线，与回放（live_replay_*）的发布状态完全无关。
// 大文件不进 MySQL，只存 bucket/object_key 与 CDN 域名（AGENTS.md §5）。
type LiveStreamOutput struct {
	OutputId      int64  `db:"output_id"`
	RoomId        int64  `db:"room_id"`
	LiveSession   int64  `db:"live_session_id"`
	TaskId        int64  `db:"task_id"`       // 产生该输出的转码任务（0 表示源流直出）
	BitrateLevel  int32  `db:"bitrate_level"` // 档位（rpc.BitrateLevel）
	Protocol      int32  `db:"protocol"`      // 协议（rpc.StreamProtocol）
	Bucket        string `db:"bucket"`
	ObjectKey     string `db:"object_key"` // playlist/流路径（相对路径，不含签名）
	CdnDomain     string `db:"cdn_domain"` // CDN 域名（配置项，非密钥）
	Width         int32  `db:"width"`      // 档位参数快照
	Height        int32  `db:"height"`     // 档位参数快照
	BitrateKbps   int32  `db:"bitrate_kbps"`
	Fps           int32  `db:"fps"`
	State         int32  `db:"state"`            // StreamOutputState*
	OfflineReason int32  `db:"offline_reason"`   // 下线原因（rpc.FailureReason）
	OnlineAt      int64  `db:"online_at"`        // Unix 秒
	OfflineAt     int64  `db:"offline_at"`       // Unix 秒，0 表示仍在线
	OnlineExpire  int64  `db:"online_expire_at"` // 在线有效期（Unix 秒），0 表示随断流下线
	RequestId     string `db:"request_id"`       // 最近一次登记的幂等键（非唯一：同一天然键可反复上下线）
	TraceId       string `db:"trace_id"`
	Ctime         int64  `db:"ctime"`
	Mtime         int64  `db:"mtime"`
}

// LiveStreamOutputModel live_stream_output 表接口。
type LiveStreamOutputModel interface {
	// Upsert 按天然键 (room_id, live_session_id, bitrate_level, protocol) 登记或刷新档位并置在线，
	// 返回 output_id。重复登记不产生新行（幂等靠 UNIQUE KEY uniq_output_natural）。
	Upsert(ctx context.Context, o *LiveStreamOutput) (int64, error)
	// UpsertTx 与 Upsert 同语义，但写入跑在调用方事务里（档位行 + Outbox 同事务，AGENTS.md §5）。
	UpsertTx(ctx context.Context, sess sqlx.Session, o *LiveStreamOutput) (int64, error)
	// FindOne 按主键查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, outputID int64) (*LiveStreamOutput, error)
	// FindByNaturalKey 按天然键查询；不存在返回 (nil, nil)。
	FindByNaturalKey(ctx context.Context, roomID, sessionID int64, bitrateLevel, protocol int32) (*LiveStreamOutput, error)
	// FindOnlineByLevel 按 (room, level, protocol) 定位当前在线档位（不含场次维度）：
	// OfflineStreamOutput 只带 room+档位+协议时的定位入口。
	// 同一档位同时有两场在线属数据异常，返回包装 ErrStreamOutputAmbiguous 的错误而不是任选一行：
	// 下线猜错对象会把还在分发的档位摘掉。
	FindOnlineByLevel(ctx context.Context, roomID int64, bitrateLevel, protocol int32) (*LiveStreamOutput, error)
	// ListByRoom 分页查询房间档位；sessionID<=0 且 includeOffline=false 时只返回当前在线档位。
	ListByRoom(ctx context.Context, roomID, sessionID int64, includeOffline bool, pn, ps, maxPS int32) ([]*LiveStreamOutput, int32, error)
	// ListOnlineBySession 取「某房间某场次」下全部仍在线的档位，供断流事件整场下线使用。
	// 两个 ID 都必须为正：session_id<=0 会退化成「按房间下线所有场次」，
	// 一条迟到的旧断流事件就能摘掉刚开播场次的分发，因此直接判错而不是猜。
	// 结果超过 MaxSessionOutputs 视为数据异常，返回 ErrSessionOutputOverflow（不静默截断）。
	ListOnlineBySession(ctx context.Context, roomID, sessionID int64) ([]*LiveStreamOutput, error)
	// MarkOffline 条件下线：只有 state=online 才写入，返回受影响行数（0 表示已下线，幂等）。
	MarkOffline(ctx context.Context, outputID int64, reason int32, traceID string) (int64, error)
	// MarkOfflineTx 与 MarkOffline 同语义，但条件 UPDATE 跑在调用方事务里。
	MarkOfflineTx(ctx context.Context, sess sqlx.Session, outputID int64, reason int32,
		traceID string) (int64, error)
	// MarkExpiredOffline 把超过 online_expire_at 的在线档位批量置下线，返回受影响行数。
	MarkExpiredOffline(ctx context.Context, now int64, limit int32) (int64, error)
}

const liveStreamOutputColumns = "SELECT output_id, room_id, live_session_id, task_id, bitrate_level, protocol, " +
	"bucket, object_key, cdn_domain, width, height, bitrate_kbps, fps, state, offline_reason, online_at, offline_at, " +
	"online_expire_at, request_id, trace_id, ctime, mtime"

type defaultLiveStreamOutputModel struct {
	conn sqlx.SqlConn
}

// NewLiveStreamOutputModel 构造 live_stream_output 的 model。
func NewLiveStreamOutputModel(conn sqlx.SqlConn) LiveStreamOutputModel {
	return &defaultLiveStreamOutputModel{conn: conn}
}

func (m *defaultLiveStreamOutputModel) Upsert(ctx context.Context, o *LiveStreamOutput) (int64, error) {
	return m.UpsertTx(ctx, m.conn, o)
}

func (m *defaultLiveStreamOutputModel) UpsertTx(ctx context.Context, sess sqlx.Session,
	o *LiveStreamOutput) (int64, error) {
	now := nowUnix()
	if o.OnlineAt == 0 {
		o.OnlineAt = now
	}
	// 同一档位重复上报只刷新产物与参数快照，offline_at 复位为 0（重新在线）。
	// 不更新 ctime：这一行的首次登记时间属于审计事实。
	res, err := sess.ExecCtx(ctx,
		"INSERT INTO live_stream_output (room_id, live_session_id, task_id, bitrate_level, protocol, bucket, "+
			"object_key, cdn_domain, width, height, bitrate_kbps, fps, state, online_at, offline_at, "+
			"online_expire_at, request_id, trace_id, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE task_id = VALUES(task_id), bucket = VALUES(bucket), "+
			"object_key = VALUES(object_key), cdn_domain = VALUES(cdn_domain), width = VALUES(width), "+
			"height = VALUES(height), bitrate_kbps = VALUES(bitrate_kbps), fps = VALUES(fps), "+
			"state = VALUES(state), online_expire_at = VALUES(online_expire_at), "+
			"request_id = VALUES(request_id), trace_id = VALUES(trace_id), offline_at = 0, offline_reason = 0, mtime = VALUES(mtime)",
		o.RoomId, o.LiveSession, o.TaskId, o.BitrateLevel, o.Protocol, o.Bucket, o.ObjectKey, o.CdnDomain,
		o.Width, o.Height, o.BitrateKbps, o.Fps, StreamOutputStateOnline, o.OnlineAt, o.OnlineExpire,
		o.RequestId, o.TraceId, now, now)
	if err != nil {
		return 0, fmt.Errorf("live_stream_output Upsert: %w", err)
	}
	// 走 ON DUPLICATE 分支时 LastInsertId 不可信，统一用天然键回读主键。
	id, err := res.LastInsertId()
	if err == nil && id > 0 {
		return id, nil
	}
	row, err := m.FindByNaturalKey(ctx, o.RoomId, o.LiveSession, o.BitrateLevel, o.Protocol)
	if err != nil {
		return 0, err
	}
	if row == nil {
		return 0, fmt.Errorf("live_stream_output Upsert: %w", ErrStreamOutputNotFound)
	}
	return row.OutputId, nil
}

func (m *defaultLiveStreamOutputModel) FindOne(ctx context.Context, outputID int64) (*LiveStreamOutput, error) {
	var o LiveStreamOutput
	if err := m.conn.QueryRowCtx(ctx, &o, liveStreamOutputColumns+" FROM live_stream_output WHERE output_id = ?", outputID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_output FindOne: %w", err)
	}
	return &o, nil
}

func (m *defaultLiveStreamOutputModel) FindByNaturalKey(ctx context.Context, roomID, sessionID int64, bitrateLevel, protocol int32) (*LiveStreamOutput, error) {
	var o LiveStreamOutput
	query := liveStreamOutputColumns + " FROM live_stream_output " +
		"WHERE room_id = ? AND live_session_id = ? AND bitrate_level = ? AND protocol = ?"
	if err := m.conn.QueryRowCtx(ctx, &o, query, roomID, sessionID, bitrateLevel, protocol); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_output FindByNaturalKey: %w", err)
	}
	return &o, nil
}

func (m *defaultLiveStreamOutputModel) FindOnlineByLevel(ctx context.Context, roomID int64,
	bitrateLevel, protocol int32) (*LiveStreamOutput, error) {
	if roomID <= 0 {
		return nil, ErrInvalidRoomID
	}
	// 多取一行用于判定歧义：同档位两场同时在线属数据异常，宁可直接报错也不猜。
	var rows []*LiveStreamOutput
	query := liveStreamOutputColumns + " FROM live_stream_output " +
		"WHERE room_id = ? AND state = ? AND bitrate_level = ? AND protocol = ? ORDER BY output_id DESC LIMIT 2"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, roomID, StreamOutputStateOnline, bitrateLevel, protocol); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_output FindOnlineByLevel: %w", err)
	}
	switch len(rows) {
	case 0:
		return nil, nil
	case 1:
		return rows[0], nil
	default:
		return nil, fmt.Errorf("live-media: room=%d level=%d protocol=%d has %d online outputs, use output_id: %w",
			roomID, bitrateLevel, protocol, len(rows), ErrStreamOutputAmbiguous)
	}
}

func (m *defaultLiveStreamOutputModel) ListByRoom(ctx context.Context, roomID, sessionID int64, includeOffline bool,
	pn, ps, maxPS int32) ([]*LiveStreamOutput, int32, error) {
	if roomID <= 0 {
		return nil, 0, ErrInvalidRoomID
	}
	where, args := buildWhere(
		whereFragment{"room_id = ?", []any{roomID}},
		whereFragment{"live_session_id = ?", []any{}}.when(sessionID > 0, sessionID),
		whereFragment{"state = ?", []any{}}.when(!includeOffline, StreamOutputStateOnline),
	)
	limit, offset := clampPage(pn, ps, maxPS)

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM live_stream_output "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("live_stream_output List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), limit, offset)
	var rows []*LiveStreamOutput
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		liveStreamOutputColumns+" FROM live_stream_output "+where+
			" ORDER BY bitrate_level ASC, protocol ASC LIMIT ? OFFSET ?", listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("live_stream_output List: %w", err)
	}
	return rows, total, nil
}

// MaxSessionOutputs 单场次在线档位数上限：查询多取一行用来判定溢出。
// 正常量级是「档位数 × 协议数」（本服务 rpc 枚举下不超过 5×4=20），
// 超过这个上限只可能是脏数据，断流下线遇到它必须报错而不是挑一部分下线。
const MaxSessionOutputs = 64

func (m *defaultLiveStreamOutputModel) ListOnlineBySession(ctx context.Context, roomID,
	sessionID int64) ([]*LiveStreamOutput, error) {
	if roomID <= 0 {
		return nil, ErrInvalidRoomID
	}
	if sessionID <= 0 {
		return nil, ErrInvalidSessionID
	}
	// 走 idx_session_state (live_session_id, state)：room_id 只作归属校验，
	// 场次 ID 本身全局唯一，带上它能让「事件房间与档位房间不一致」这种脏数据显式失败。
	query := liveStreamOutputColumns + " FROM live_stream_output " +
		"WHERE live_session_id = ? AND state = ? AND room_id = ? ORDER BY bitrate_level ASC, protocol ASC LIMIT ?"
	var rows []*LiveStreamOutput
	if err := m.conn.QueryRowsCtx(ctx, &rows, query,
		sessionID, StreamOutputStateOnline, roomID, MaxSessionOutputs+1); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream_output ListOnlineBySession: %w", err)
	}
	if len(rows) > MaxSessionOutputs {
		return nil, fmt.Errorf("live-media: room=%d session=%d has more than %d online outputs: %w",
			roomID, sessionID, MaxSessionOutputs, ErrSessionOutputOverflow)
	}
	return rows, nil
}

func (m *defaultLiveStreamOutputModel) MarkOffline(ctx context.Context, outputID int64, reason int32,
	traceID string) (int64, error) {
	return m.MarkOfflineTx(ctx, m.conn, outputID, reason, traceID)
}

func (m *defaultLiveStreamOutputModel) MarkOfflineTx(ctx context.Context, sess sqlx.Session, outputID int64,
	reason int32, traceID string) (int64, error) {
	// 档位表没有 version 列：state 本身就是 CAS 条件（只有在线行需要被下线）。
	sets := []columnValue{
		{"state", StreamOutputStateOffline},
		{"offline_at", nowUnix()},
		{"offline_reason", reason},
	}
	if traceID != "" {
		sets = append(sets, columnValue{"trace_id", traceID})
	}
	return conditionalUpdate(ctx, sess, "live_stream_output", sets, false, []whereFragment{
		{"output_id = ?", []any{outputID}},
		{"state = ?", []any{StreamOutputStateOnline}},
	})
}

func (m *defaultLiveStreamOutputModel) MarkExpiredOffline(ctx context.Context, now int64, limit int32) (int64, error) {
	if limit <= 0 {
		limit = 200
	}
	// 带 LIMIT 的条件更新：避免过期档位堆积时一次锁住全表。
	// 到期下线记 ReasonTimeout：与断流（SOURCE_LOST）区分，运营才能判断是配置有效期到点还是源没了。
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE live_stream_output SET state = ?, offline_at = ?, offline_reason = ?, mtime = ? "+
			"WHERE state = ? AND online_expire_at > 0 AND online_expire_at < ? LIMIT ?",
		StreamOutputStateOffline, now, ReasonTimeout, now, StreamOutputStateOnline, now, limit)
	if err != nil {
		return 0, fmt.Errorf("live_stream_output MarkExpiredOffline: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_stream_output MarkExpiredOffline RowsAffected: %w", err)
	}
	return aff, nil
}
