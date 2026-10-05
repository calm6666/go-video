package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// LiveGwBroadcastLog 广播审计流水（live_gw_broadcast_log）。
//
// 只存摘要不存正文：payload_digest 是载荷 sha256 的前 N 位，payload_bytes 是长度。
// 弹幕正文、系统事件载荷都只在 Redis/内存里短期存在（README 数据分层约束）。
//
// 这张表的目的是回答"这条消息为什么没到"：
// 每一次受理/丢弃/越权拒绝都落一行，drop_reason 必须是 Drop* 常量之一，
// 绝不允许出现 state=已下发 而 drop_reason 缺失的行，也不允许用"成功"掩盖越权。
//
// 幂等：UNIQUE KEY uniq_room_message(room_id, message_id)——同一 (房间, 消息) 只留首行。
// 重复投递不追加行（否则审计表会被重放灌满），重复计数由 Redis 去重窗口承担，
// 并在 RPC 响应里以 duplicated=true 回显（DropDuplicated）。
type LiveGwBroadcastLog struct {
	Id                  int64  `db:"id"`
	MessageId           string `db:"message_id"` // 幂等键（房间维度唯一）
	RoomId              int64  `db:"room_id"`
	Kind                int32  `db:"kind"`           // Kind*
	SenderMid           int64  `db:"sender_mid"`     // 系统消息为 0
	SenderRole          int32  `db:"sender_role"`    // Role*（服务端判定结果，不是客户端自报值）
	EventId             string `db:"event_id"`       // 系统事件来源 ID（非事件投递为空）
	PayloadDigest       string `db:"payload_digest"` // 载荷摘要（sha256 hex 前缀），不含正文
	PayloadBytes        int32  `db:"payload_bytes"`  // 载荷字节数
	FanoutNodes         int32  `db:"fanout_nodes"`   // 扇出节点数
	TargetedConnections int32  `db:"targeted_connections"`
	State               int32  `db:"state"`          // BroadcastLog*
	DropReason          int32  `db:"drop_reason"`    // Drop*（state=已下发时为 DropOK）
	SourceService       string `db:"source_service"` // 来源服务名（审计）
	TraceId             string `db:"trace_id"`
	Ctime               int64  `db:"ctime"`
}

// BroadcastLogFilter List 过滤条件（room_id 必填：审计按房间查，禁止全表扫）。
type BroadcastLogFilter struct {
	RoomId      int64
	Kind        int32
	SenderMid   int64
	OnlyDropped bool // 只看被丢弃/被拒绝/被去重的行
	Pn          int32
	Ps          int32
	MaxPageSize int32
}

// LiveGwBroadcastLogModel live_gw_broadcast_log 表接口。
type LiveGwBroadcastLogModel interface {
	// Insert 写入审计行；返回 (id, nil) 正常、(0, nil) 表示 (room_id, message_id) 已存在
	// （重复投递，调用方据此回 duplicated=true），其它 error 为真失败。
	Insert(ctx context.Context, l *LiveGwBroadcastLog) (int64, error)
	// FindByMessage 按 (room_id, message_id) 回读首行（Redis 不可用时的兜底判定），不存在返回 (nil, nil)。
	FindByMessage(ctx context.Context, roomID int64, messageID string) (*LiveGwBroadcastLog, error)
	FindByEvent(ctx context.Context, roomID int64, eventID string) (*LiveGwBroadcastLog, error)
	List(ctx context.Context, f BroadcastLogFilter) ([]*LiveGwBroadcastLog, int32, error)
	// CountByRoomSince 房间在 since（Unix 秒）之后的各类计数，用于运营看"这个房间广播被丢了多少"。
	CountByRoomSince(ctx context.Context, roomID int64, since int64) (*BroadcastLogStats, error)
	// PurgeBefore 清理早于 before（Unix 秒）的审计行，分批删除，返回删除行数。
	// 删除是安全的：广播去重的**事实源是 Redis 去重窗口**（分钟级 TTL），本表只做审计；
	// 保留期由配置 LiveGateway.BroadcastLogRetentionDays 决定（默认 30 天）。
	PurgeBefore(ctx context.Context, before int64, limit int32) (int64, error)
}

// BroadcastLogStats 审计流水的聚合视图（只统计，不参与下发判定：实时口径在 Redis）。
type BroadcastLogStats struct {
	Sent       int64 `db:"sent"`
	Dropped    int64 `db:"dropped"`
	Denied     int64 `db:"denied"`
	Duplicated int64 `db:"duplicated"`
}

const liveGwBroadcastLogColumns = "SELECT id, message_id, room_id, kind, sender_mid, sender_role, event_id, " +
	"payload_digest, payload_bytes, fanout_nodes, targeted_connections, state, drop_reason, source_service, " +
	"trace_id, ctime"

type defaultLiveGwBroadcastLogModel struct {
	conn sqlx.SqlConn
}

// NewLiveGwBroadcastLogModel 构造 live_gw_broadcast_log 的 model。
func NewLiveGwBroadcastLogModel(conn sqlx.SqlConn) LiveGwBroadcastLogModel {
	return &defaultLiveGwBroadcastLogModel{conn: conn}
}

func (m *defaultLiveGwBroadcastLogModel) Insert(ctx context.Context, l *LiveGwBroadcastLog) (int64, error) {
	if l.RoomId <= 0 {
		return 0, ErrInvalidRoomID
	}
	if l.MessageId == "" {
		return 0, ErrEmptyMessageID
	}
	if !ValidBroadcastKind(l.Kind) {
		return 0, fmt.Errorf("live_gw_broadcast_log Insert: kind=%d %w", l.Kind, ErrInvalidBroadcastKind)
	}
	if !ValidBroadcastLogState(l.State) {
		return 0, fmt.Errorf("live_gw_broadcast_log Insert: state=%d %w", l.State, ErrInvalidTransition)
	}
	// 一致性约束：判为"已下发"的行必须给出 DropOK；被丢弃/拒绝/去重的行必须给出可解释原因。
	if l.State == BroadcastLogSent {
		l.DropReason = DropOK
	} else if !ValidDropReason(l.DropReason) || l.DropReason == DropOK {
		return 0, fmt.Errorf("live_gw_broadcast_log Insert: state=%d without drop_reason: %w", l.State, ErrPermissionDenied)
	}
	if l.Ctime == 0 {
		l.Ctime = nowUnix()
	}
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO live_gw_broadcast_log (message_id, room_id, kind, sender_mid, sender_role, event_id, "+
			"payload_digest, payload_bytes, fanout_nodes, targeted_connections, state, drop_reason, source_service, "+
			"trace_id, ctime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		l.MessageId, l.RoomId, l.Kind, l.SenderMid, l.SenderRole, l.EventId, l.PayloadDigest, l.PayloadBytes,
		l.FanoutNodes, l.TargetedConnections, l.State, l.DropReason, l.SourceService, l.TraceId, l.Ctime)
	if err != nil {
		if isDuplicateErr(err) {
			// uniq_room_message 命中：同一 (房间, 消息) 已经受理过一次，本次是重复投递。
			return 0, nil
		}
		return 0, fmt.Errorf("live_gw_broadcast_log Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_gw_broadcast_log Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultLiveGwBroadcastLogModel) FindByMessage(ctx context.Context, roomID int64, messageID string) (*LiveGwBroadcastLog, error) {
	if roomID <= 0 || messageID == "" {
		return nil, ErrEmptyMessageID
	}
	var l LiveGwBroadcastLog
	query := liveGwBroadcastLogColumns + " FROM live_gw_broadcast_log WHERE room_id = ? AND message_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &l, query, roomID, messageID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_gw_broadcast_log FindByMessage: %w", err)
	}
	return &l, nil
}

func (m *defaultLiveGwBroadcastLogModel) FindByEvent(ctx context.Context, roomID int64, eventID string) (*LiveGwBroadcastLog, error) {
	if roomID <= 0 || eventID == "" {
		return nil, ErrEmptyEventID
	}
	var l LiveGwBroadcastLog
	query := liveGwBroadcastLogColumns + " FROM live_gw_broadcast_log WHERE room_id = ? AND event_id = ? " +
		"ORDER BY id ASC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &l, query, roomID, eventID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_gw_broadcast_log FindByEvent: %w", err)
	}
	return &l, nil
}

func (m *defaultLiveGwBroadcastLogModel) List(ctx context.Context, f BroadcastLogFilter) ([]*LiveGwBroadcastLog, int32, error) {
	if f.RoomId <= 0 {
		return nil, 0, ErrInvalidRoomID
	}
	frags := []whereFragment{
		{"room_id = ?", []any{f.RoomId}},
		whereFragment{"kind = ?", []any{}}.when(f.Kind > 0, f.Kind),
		whereFragment{"sender_mid = ?", []any{}}.when(f.SenderMid > 0, f.SenderMid),
	}
	if f.OnlyDropped {
		frags = append(frags, whereFragment{"state IN (?, ?, ?)",
			[]any{BroadcastLogDropped, BroadcastLogDenied, BroadcastLogDuplicated}})
	}
	where, args := buildWhere(frags...)
	limit, offset := clampPage(f.Pn, f.Ps, f.MaxPageSize)

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM live_gw_broadcast_log "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("live_gw_broadcast_log List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), limit, offset)
	var rows []*LiveGwBroadcastLog
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		liveGwBroadcastLogColumns+" FROM live_gw_broadcast_log "+where+" ORDER BY id DESC LIMIT ? OFFSET ?",
		listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("live_gw_broadcast_log List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultLiveGwBroadcastLogModel) CountByRoomSince(ctx context.Context, roomID int64, since int64) (*BroadcastLogStats, error) {
	if roomID <= 0 {
		return nil, ErrInvalidRoomID
	}
	var st BroadcastLogStats
	query := "SELECT COALESCE(SUM(state = ?), 0) AS sent, COALESCE(SUM(state = ?), 0) AS dropped, " +
		"COALESCE(SUM(state = ?), 0) AS denied, COALESCE(SUM(state = ?), 0) AS duplicated " +
		"FROM live_gw_broadcast_log WHERE room_id = ? AND ctime >= ?"
	if err := m.conn.QueryRowCtx(ctx, &st, query, BroadcastLogSent, BroadcastLogDropped, BroadcastLogDenied,
		BroadcastLogDuplicated, roomID, since); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &BroadcastLogStats{}, nil
		}
		return nil, fmt.Errorf("live_gw_broadcast_log CountByRoomSince: %w", err)
	}
	return &st, nil
}

func (m *defaultLiveGwBroadcastLogModel) PurgeBefore(ctx context.Context, before int64, limit int32) (int64, error) {
	if before <= 0 {
		return 0, fmt.Errorf("live_gw_broadcast_log PurgeBefore: before=%d must be a positive Unix second", before)
	}
	if limit <= 0 {
		limit = 500
	}
	res, err := m.conn.ExecCtx(ctx,
		"DELETE FROM live_gw_broadcast_log WHERE ctime < ? LIMIT ?", before, limit)
	if err != nil {
		return 0, fmt.Errorf("live_gw_broadcast_log PurgeBefore: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_gw_broadcast_log PurgeBefore RowsAffected: %w", err)
	}
	return aff, nil
}
