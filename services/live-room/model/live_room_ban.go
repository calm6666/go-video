package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// liveRoomBanColumns 与 000005_create_live_room_ban.sql 逐列对应。
const liveRoomBanColumns = "ban_id, room_id, mid, ban_type, reason, start_at, end_at, state, " +
	"operator_mid, lift_operator_mid, lift_reason, lifted_at, trace_id, ctime"

// LiveRoomBan 禁播记录行（live_room_ban 表投影，对应 rpc.RoomBanInfo）。
//
// 留存口径：解除与到期都是软状态（state=2/3），行永不物理删除，
// 这样「谁在什么时候以什么理由禁了哪个房间、又是谁解除的」可长期审计（AGENTS.md §8）。
// end_at=0 表示永久禁播，只能由 LiftBan 解除（到期扫描按 end_at>0 过滤）。
type LiveRoomBan struct {
	BanID           int64  `db:"ban_id"`            // 禁播记录 ID（主键）
	RoomID          int64  `db:"room_id"`           // 房间 ID
	Mid             int64  `db:"mid"`               // 被禁主播 ID（下单时的生效房主快照）
	BanType         int32  `db:"ban_type"`          // 1 临时、2 永久
	Reason          string `db:"reason"`            // 禁播原因（运营内部说明，不下发终端）
	StartAt         int64  `db:"start_at"`          // 生效时间（Unix 秒）
	EndAt           int64  `db:"end_at"`            // 结束时间（Unix 秒），0 表示永久
	State           int32  `db:"state"`             // 1 生效、2 已解除、3 已过期
	OperatorMid     int64  `db:"operator_mid"`      // 下发运营/系统 ID
	LiftOperatorMid int64  `db:"lift_operator_mid"` // 解除运营 ID，0 表示未解除
	LiftReason      string `db:"lift_reason"`       // 解除原因
	LiftedAt        int64  `db:"lifted_at"`         // 解除时间（Unix 秒）
	TraceID         string `db:"trace_id"`          // 链路追踪 ID
	Ctime           int64  `db:"ctime"`             // 创建时间（Unix 秒）
}

// BanListQuery 是 ListRoomBans 的过滤条件（运营侧审计列表）。
type BanListQuery struct {
	RoomID int64
	Mid    int64
	State  int32 // BanStateActive/Lifted/Expired，0 表示不过滤
	Offset int32
	Limit  int32
}

// LiveRoomBanModel live_room_ban 表读写接口。
type LiveRoomBanModel interface {
	// Insert 新建禁播记录并返回 ban_id。临时禁播必须 end_at > start_at > 0，
	// 永久禁播必须 end_at = 0，否则返回 ErrBanTypeInvalid / ErrBanDurationRequired。
	Insert(ctx context.Context, b *LiveRoomBan) (int64, error)
	// InsertTx 在事务内新建禁播记录，供「禁播落库 + 房间状态迁移 + 场次终止 + 审计日志」
	// 同事务提交（AGENTS.md §5：处置结论与其派生状态不能分裂）。session 为 nil 时退化为 Insert。
	InsertTx(ctx context.Context, session sqlx.Session, b *LiveRoomBan) (int64, error)
	// FindOne 按 ban_id 读记录；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, banID int64) (*LiveRoomBan, error)
	// FindActiveByRoom 读房间当前生效禁播（state=1 且未到期）；无则 (nil, nil)。
	// now 由调用方传入，保证与 live_room.ban_until 的判定用同一时刻。
	FindActiveByRoom(ctx context.Context, roomID, now int64) (*LiveRoomBan, error)
	// HasActiveByMid 判断某主播是否命中任一房间的生效禁播（PrepareLive 的 not_banned 检查项）。
	HasActiveByMid(ctx context.Context, mid, now int64) (bool, error)
	// List 分页查询禁播记录，ban_id 倒序（最新处置在前）。
	List(ctx context.Context, q BanListQuery) ([]*LiveRoomBan, error)
	// Count 返回 List 同条件下的总数。
	Count(ctx context.Context, q BanListQuery) (int64, error)
	// Lift 解除生效禁播：WHERE ban_id=? AND state=1，写入解除人与原因。
	// 返回 false 表示记录已被别人解除/过期（并发），调用方按幂等重放处理。
	Lift(ctx context.Context, banID, liftOperatorMid int64, reason string, liftedAt int64) (bool, error)
	// LiftTx 在事务内解除禁播，供「解除记录 + 房间状态迁移 + 审计日志」同事务提交。
	LiftTx(ctx context.Context, session sqlx.Session, banID, liftOperatorMid int64, reason string,
		liftedAt int64) (bool, error)
	// ExpireDue 把到期的临时禁播批量置为已过期（cron 补偿入口，limit 截断）。
	// 单条 UPDATE ... LIMIT：MySQL 侧按 ban_id 升序取批，避免一次锁全表。
	ExpireDue(ctx context.Context, now int64, limit int32) (int64, error)
	// ListDue 列出到期但未标记的记录（cron 逐条推进房间状态前的候选，limit 截断）。
	ListDue(ctx context.Context, now int64, limit int32) ([]*LiveRoomBan, error)
}

type defaultLiveRoomBanModel struct {
	conn sqlx.SqlConn
}

// NewLiveRoomBanModel 创建 LiveRoomBanModel 实现。
func NewLiveRoomBanModel(conn sqlx.SqlConn) LiveRoomBanModel {
	return &defaultLiveRoomBanModel{conn: conn}
}

func (m *defaultLiveRoomBanModel) Insert(ctx context.Context, b *LiveRoomBan) (int64, error) {
	return m.insert(ctx, m.conn, b)
}

func (m *defaultLiveRoomBanModel) InsertTx(ctx context.Context, session sqlx.Session, b *LiveRoomBan) (int64, error) {
	if session == nil {
		return m.Insert(ctx, b)
	}
	return m.insert(ctx, session, b)
}

// insert 是禁播记录的唯一次写入实现，execer 可以是连接或事务句柄。
func (m *defaultLiveRoomBanModel) insert(ctx context.Context, execer sqlx.Session, b *LiveRoomBan) (int64, error) {
	if b.RoomID <= 0 {
		return 0, ErrInvalidRoomID
	}
	if b.OperatorMid <= 0 {
		return 0, ErrOperatorRequired
	}
	switch b.BanType {
	case BanTypePermanent:
		if b.EndAt != 0 {
			return 0, ErrBanTypeInvalid
		}
	case BanTypeTemporary:
		if b.StartAt <= 0 {
			b.StartAt = nowUnix()
		}
		if b.EndAt <= b.StartAt {
			return 0, ErrBanDurationRequired
		}
	default:
		return 0, ErrBanTypeInvalid
	}
	const query = "INSERT INTO live_room_ban (room_id, mid, ban_type, reason, start_at, end_at, state, " +
		"operator_mid, lift_operator_mid, lift_reason, lifted_at, trace_id, ctime) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 0, ?, ?)"
	if b.State == 0 {
		b.State = BanStateActive
	}
	if b.Ctime == 0 {
		b.Ctime = nowUnix()
	}
	res, err := execer.ExecCtx(ctx, query,
		b.RoomID, b.Mid, b.BanType, b.Reason, b.StartAt, b.EndAt, b.State,
		b.OperatorMid, "", b.TraceID, b.Ctime)
	if err != nil {
		return 0, fmt.Errorf("live_room_ban Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_room_ban Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultLiveRoomBanModel) FindOne(ctx context.Context, banID int64) (*LiveRoomBan, error) {
	var b LiveRoomBan
	query := "SELECT " + liveRoomBanColumns + " FROM live_room_ban WHERE ban_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &b, query, banID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_room_ban FindOne: %w", err)
	}
	return &b, nil
}

func (m *defaultLiveRoomBanModel) FindActiveByRoom(ctx context.Context, roomID, now int64) (*LiveRoomBan, error) {
	var b LiveRoomBan
	query := "SELECT " + liveRoomBanColumns + " FROM live_room_ban " +
		"WHERE room_id = ? AND state = ? AND (end_at = 0 OR end_at > ?) ORDER BY ban_id DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &b, query, roomID, BanStateActive, now); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_room_ban FindActiveByRoom: %w", err)
	}
	return &b, nil
}

func (m *defaultLiveRoomBanModel) HasActiveByMid(ctx context.Context, mid, now int64) (bool, error) {
	if mid <= 0 {
		return false, ErrInvalidMid
	}
	var hit int64
	query := "SELECT COUNT(*) FROM (SELECT ban_id FROM live_room_ban " +
		"WHERE mid = ? AND state = ? AND (end_at = 0 OR end_at > ?) LIMIT 1) AS t"
	if err := m.conn.QueryRowCtx(ctx, &hit, query, mid, BanStateActive, now); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("live_room_ban HasActiveByMid: %w", err)
	}
	return hit > 0, nil
}

func (m *defaultLiveRoomBanModel) List(ctx context.Context, q BanListQuery) ([]*LiveRoomBan, error) {
	if q.Limit <= 0 {
		q.Limit = defaultListLimit
	}
	where, args := banWhere(q)
	query := "SELECT " + liveRoomBanColumns + " FROM live_room_ban WHERE " + where +
		" ORDER BY ban_id DESC LIMIT ? OFFSET ?"
	args = append(args, q.Limit, q.Offset)

	var rows []*LiveRoomBan
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_room_ban List: %w", err)
	}
	return rows, nil
}

func (m *defaultLiveRoomBanModel) Count(ctx context.Context, q BanListQuery) (int64, error) {
	where, args := banWhere(q)
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM live_room_ban WHERE "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_room_ban Count: %w", err)
	}
	return total, nil
}

func (m *defaultLiveRoomBanModel) Lift(ctx context.Context, banID, liftOperatorMid int64, reason string, liftedAt int64) (bool, error) {
	return m.lift(ctx, m.conn, banID, liftOperatorMid, reason, liftedAt)
}

func (m *defaultLiveRoomBanModel) LiftTx(ctx context.Context, session sqlx.Session,
	banID, liftOperatorMid int64, reason string, liftedAt int64) (bool, error) {
	if session == nil {
		return m.Lift(ctx, banID, liftOperatorMid, reason, liftedAt)
	}
	return m.lift(ctx, session, banID, liftOperatorMid, reason, liftedAt)
}

// lift 是解除禁播的唯一 SQL 实现，execer 可以是连接或事务句柄。
func (m *defaultLiveRoomBanModel) lift(ctx context.Context, execer sqlx.Session,
	banID, liftOperatorMid int64, reason string, liftedAt int64) (bool, error) {
	if banID <= 0 {
		return false, ErrBanNotFound
	}
	if liftOperatorMid <= 0 {
		return false, ErrOperatorRequired
	}
	if liftedAt <= 0 {
		liftedAt = nowUnix()
	}
	res, err := execer.ExecCtx(ctx,
		"UPDATE live_room_ban SET state = ?, lift_operator_mid = ?, lift_reason = ?, lifted_at = ? "+
			"WHERE ban_id = ? AND state = ?",
		BanStateLifted, liftOperatorMid, reason, liftedAt, banID, BanStateActive)
	if err != nil {
		return false, fmt.Errorf("live_room_ban Lift: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("live_room_ban Lift RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultLiveRoomBanModel) ExpireDue(ctx context.Context, now int64, limit int32) (int64, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE live_room_ban SET state = ? WHERE state = ? AND end_at > 0 AND end_at <= ? ORDER BY ban_id ASC LIMIT ?",
		BanStateExpired, BanStateActive, now, limit)
	if err != nil {
		return 0, fmt.Errorf("live_room_ban ExpireDue: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_room_ban ExpireDue RowsAffected: %w", err)
	}
	return aff, nil
}

func (m *defaultLiveRoomBanModel) ListDue(ctx context.Context, now int64, limit int32) ([]*LiveRoomBan, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	query := "SELECT " + liveRoomBanColumns + " FROM live_room_ban " +
		"WHERE state = ? AND end_at > 0 AND end_at <= ? ORDER BY end_at ASC LIMIT ?"
	var rows []*LiveRoomBan
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, BanStateActive, now, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_room_ban ListDue: %w", err)
	}
	return rows, nil
}

// banWhere 构造 List/Count 共用的 WHERE 片段。
func banWhere(q BanListQuery) (string, []interface{}) {
	var (
		sb   strings.Builder
		args []interface{}
	)
	sb.WriteString("ban_id > 0") // 恒真占位，让下面的 AND 分支不用判首项
	if q.RoomID > 0 {
		sb.WriteString(" AND room_id = ?")
		args = append(args, q.RoomID)
	}
	if q.Mid > 0 {
		sb.WriteString(" AND mid = ?")
		args = append(args, q.Mid)
	}
	if q.State != 0 {
		sb.WriteString(" AND state = ?")
		args = append(args, q.State)
	}
	return sb.String(), args
}
