package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// liveRoomColumns 与 deploy/migrations/live-room/000001_create_live_room.sql 逐列对应，
// 改动任一列必须同时改迁移（新增 0000NN 文件，不改已应用文件）。
const liveRoomColumns = "room_id, owner_mid, title, cover, area_id, state, verify_state, " +
	"active_session_id, active_stream_id, state_version, reject_reason, ban_until, " +
	"platform, app_version, moderation_task_id, ctime, mtime"

// defaultListLimit 是调用方忘记传 limit 时的兜底行数。
// 本服务所有列表查询都必须落到一个有限 LIMIT（AGENTS.md §9：禁止无界扫描），
// 兜底取「够用且最小」的值，而不是不封顶。
const defaultListLimit int32 = 50

// LiveRoom 直播间主体行（live_room 表投影，对应 rpc.RoomInfo）。
//
// 字段所有权说明：
//   - owner_mid 是 live_room_anchor 生效房主的**投影**，真值在绑定表；
//     MutateAnchor 换房主时必须同事务回写本列，否则「按房主查房间」会读到脏房主。
//   - active_session_id / active_stream_id 是当前场次的投影；active_stream_id 只是
//     live-ingest 分配的推流标识引用，本服务不校验其存在性（边界见服务 README §1）。
//   - state_version 每次状态迁移 +1，既做乐观并发控制，也用于事件乱序守卫。
type LiveRoom struct {
	RoomID           int64  `db:"room_id"`            // 房间 ID（主键）
	OwnerMid         int64  `db:"owner_mid"`          // 房主用户 ID（生效房主投影）
	Title            string `db:"title"`              // 房间标题（1~80 字符）
	Cover            string `db:"cover"`              // 封面对象引用（不含签名地址）
	AreaID           int64  `db:"area_id"`            // 直播分区 ID
	State            int32  `db:"state"`              // 房间业务状态，见 RoomState* 常量
	VerifyState      int32  `db:"verify_state"`       // 资料审核状态，见 VerifyState* 常量
	ActiveSessionID  int64  `db:"active_session_id"`  // 当前场次 ID，0 表示无进行中场次
	ActiveStreamID   string `db:"active_stream_id"`   // 当前场次 stream_id 引用
	StateVersion     int32  `db:"state_version"`      // 状态版本号，每次迁移 +1
	RejectReason     string `db:"reject_reason"`      // 资料驳回原因（verify_state=REJECTED 时有值）
	BanUntil         int64  `db:"ban_until"`          // 生效禁播到期时间（Unix 秒），0 表示无禁播/永久
	Platform         int32  `db:"platform"`           // 创建端，见 Platform* 常量
	AppVersion       string `db:"app_version"`        // 创建时客户端版本号
	ModerationTaskID int64  `db:"moderation_task_id"` // 最近一次资料送审任务 ID，0 表示未送审
	Ctime            int64  `db:"ctime"`              // 创建时间（Unix 秒）
	Mtime            int64  `db:"mtime"`              // 修改时间（Unix 秒）
}

// RoomPatch 是状态迁移时可以顺带写入的列；nil 表示该列不动。
// 之所以和状态迁移合并成一条 UPDATE：房间状态与其派生列（当前场次、禁播到期）
// 必须原子变化，拆成两条语句会让并发读者看到「LIVING 但没有 active_session」这种中间态。
type RoomPatch struct {
	VerifyState      *int32
	RejectReason     *string
	BanUntil         *int64
	ActiveSessionID  *int64
	ActiveStreamID   *string
	ModerationTaskID *int64
}

// RoomListQuery 是 ListRooms 的过滤条件。零值表示「不按该维度过滤」。
type RoomListQuery struct {
	OwnerMid int64
	AreaID   int64
	State    int32
	Order    int32 // 见 RoomOrder* 常量
	Offset   int32
	Limit    int32 // 必须由调用方夹到 config.LiveRoom.MaxListPageSize
}

// LiveRoomModel live_room 表读写接口。
type LiveRoomModel interface {
	// Insert 新建房间，返回自增 room_id。调用方必须把 state 置为 RoomStatePending、
	// verify_state 置为初始审核态、state_version 置 1（首版状态）。
	Insert(ctx context.Context, r *LiveRoom) (int64, error)
	// InsertTx 在事务内建档，语义与 Insert 完全一致（session==nil 时退化为 Insert）。
	InsertTx(ctx context.Context, session sqlx.Session, r *LiveRoom) (int64, error)
	// FindOne 按 room_id 读房间；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, roomID int64) (*LiveRoom, error)
	// ListByOwner 按房主读生效中（非 FINISHED）的房间，room_id 倒序，limit 截断。
	// GetRoom(owner_mid) 取第一条；返回多条即该主播持有多个房间，属正常情况。
	ListByOwner(ctx context.Context, ownerMid int64, limit int32) ([]*LiveRoom, error)
	// List 分页浏览房间（发现页/主播主页/运营列表），LIMIT 由 q.Limit 控制。
	List(ctx context.Context, q RoomListQuery) ([]*LiveRoom, error)
	// Count 返回 List 同过滤条件下的总数（ListRoomsReply.total），两者共用一个 WHERE 构造器。
	Count(ctx context.Context, q RoomListQuery) (int64, error)
	// CountByOwner 统计某主播指定状态集合内的房间数（CreateRoom 的上限校验）。
	// states 为空表示不限状态（含已关闭房间，历史留档也计入上限）。
	CountByOwner(ctx context.Context, ownerMid int64, states []int32) (int64, error)
	// CountByArea 统计分区下未关闭的房间数（UpsertArea 停用分区前的占用检查）。
	CountByArea(ctx context.Context, areaID int64, states []int32) (int64, error)
	// Transition 条件迁移房间状态：WHERE room_id=? AND state=from
	// [AND state_version=expectVersion]，命中后 state=to、state_version+1、mtime=now。
	// expectVersion <= 0 表示不校验版本（仅用于本服务内部串行路径，如 CloseRoom）。
	// 返回 false 表示状态/版本已被并发推进，调用方必须重读再决策，不得当作成功。
	Transition(ctx context.Context, roomID int64, from, to int32, expectVersion int32, patch RoomPatch) (bool, error)
	// TransitionTx 在事务内做同样的条件迁移，供与 live_room_state_log、live_session
	// 的写入同事务提交（AGENTS.md §5：状态与审计证据不能分裂）。
	TransitionTx(ctx context.Context, session sqlx.Session, roomID int64, from, to int32, expectVersion int32, patch RoomPatch) (bool, error)
	// UpdateProfile 改标题/封面/分区并把资料审核态重置（allowStates 内的状态才可改）。
	// 空串/0 表示该列不修改。返回 false 表示房间已不在可编辑状态（并发下被关闭/禁播）。
	UpdateProfile(ctx context.Context, roomID int64, title, cover string, areaID int64,
		verifyState int32, moderationTaskID int64, allowStates []int32) (bool, error)
	// ClearActiveSession 场次结束后清理当前场次投影：仅当 active_session_id 仍是该场次时生效，
	// 避免把后继场次的投影误清空（返回 false 表示已被新场次接管，属正常并发结果）。
	ClearActiveSession(ctx context.Context, roomID, sessionID int64) (bool, error)
	// ListBansToExpire 列出到期未清理的禁播房间（cron 的 LiftBan 补偿扫描，limit 截断）。
	ListBansToExpire(ctx context.Context, now int64, limit int32) ([]*LiveRoom, error)
	// SetVerifyResult 只回写资料审核结论（verify_state + reject_reason），不推进房间业务状态。
	// 存在的理由：moderation 结论是「资料」维度的事实，即使房间业务状态此刻不允许降级
	// （例如仍在 LIVING，矩阵里没有 Living->Pending 这条边），结论也必须留证，
	// 但不能因此伪造一次房间状态迁移。allowStates 限定生效状态，命中 0 行返回 false。
	SetVerifyResult(ctx context.Context, roomID int64, verifyState int32, rejectReason string, allowStates []int32) (bool, error)
	// SetVerifyResultTx 事务内版本（session==nil 时退化为连接版本）。
	SetVerifyResultTx(ctx context.Context, session sqlx.Session, roomID int64, verifyState int32,
		rejectReason string, allowStates []int32) (bool, error)
	// SetBanUntil 只回写生效禁播的到期投影（ban_until），不推进房间业务状态。
	// 存在的理由：对已经处于 BANNED 的房间再次下发禁播是「替换处置区间」，
	// 而矩阵里没有 Banned->Banned 这条自边，用两次迁移去凑会凭空多出 READY 中间态；
	// 因此这里只更新投影，allowStates 限定 BANNED，命中 0 行返回 false。
	SetBanUntil(ctx context.Context, roomID int64, banUntil int64, allowStates []int32) (bool, error)
	// SetBanUntilTx 事务内版本（session==nil 时退化为连接版本）。
	SetBanUntilTx(ctx context.Context, session sqlx.Session, roomID int64, banUntil int64,
		allowStates []int32) (bool, error)
	// ListByRoomIDs 按显式 room_id 集合回表（ListRooms 的「按主播可见房间」路径：
	// ids 来自 live_room_anchor 的分页结果，因此本查询天然有界）。
	// 过滤与排序复用 RoomListQuery（OwnerMid 由调用方留空，可见性来自 ids），
	// 保证与 List 同一口径；ids 为空直接返回空页，不当作全表。
	ListByRoomIDs(ctx context.Context, ids []int64, q RoomListQuery) ([]*LiveRoom, error)
}

type defaultLiveRoomModel struct {
	conn sqlx.SqlConn
}

// NewLiveRoomModel 创建 LiveRoomModel 实现。
func NewLiveRoomModel(conn sqlx.SqlConn) LiveRoomModel {
	return &defaultLiveRoomModel{conn: conn}
}

func (m *defaultLiveRoomModel) Insert(ctx context.Context, r *LiveRoom) (int64, error) {
	return m.insert(ctx, m.conn, r)
}

// InsertTx 见接口注释：CreateRoom 要把「房间建档 + 房主绑定 + 配置行 + 审计日志」
// 绑成一个事务，任何一半留在事务外都会留下永远开不了播的脏房间。
func (m *defaultLiveRoomModel) InsertTx(ctx context.Context, session sqlx.Session, r *LiveRoom) (int64, error) {
	if session == nil {
		return m.Insert(ctx, r)
	}
	return m.insert(ctx, session, r)
}

// insert 是房间建档的唯一 SQL 实现，execer 可以是连接或事务句柄。
func (m *defaultLiveRoomModel) insert(ctx context.Context, execer sqlx.Session, r *LiveRoom) (int64, error) {
	const query = "INSERT INTO live_room (owner_mid, title, cover, area_id, state, verify_state, " +
		"active_session_id, active_stream_id, state_version, reject_reason, ban_until, " +
		"platform, app_version, moderation_task_id, ctime, mtime) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
	now := nowUnix()
	if r.Ctime == 0 {
		r.Ctime = now
	}
	r.Mtime = now
	res, err := execer.ExecCtx(ctx, query,
		r.OwnerMid, r.Title, r.Cover, r.AreaID, r.State, r.VerifyState,
		r.ActiveSessionID, r.ActiveStreamID, r.StateVersion, r.RejectReason, r.BanUntil,
		r.Platform, r.AppVersion, r.ModerationTaskID, r.Ctime, r.Mtime)
	if err != nil {
		return 0, fmt.Errorf("live_room Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_room Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultLiveRoomModel) FindOne(ctx context.Context, roomID int64) (*LiveRoom, error) {
	var r LiveRoom
	query := "SELECT " + liveRoomColumns + " FROM live_room WHERE room_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &r, query, roomID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_room FindOne: %w", err)
	}
	return &r, nil
}

func (m *defaultLiveRoomModel) ListByOwner(ctx context.Context, ownerMid int64, limit int32) ([]*LiveRoom, error) {
	if ownerMid <= 0 {
		return nil, ErrInvalidMid
	}
	if limit <= 0 {
		limit = defaultListLimit
	}
	query := "SELECT " + liveRoomColumns + " FROM live_room " +
		"WHERE owner_mid = ? AND state <> ? ORDER BY room_id DESC LIMIT ?"
	var rows []*LiveRoom
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, ownerMid, RoomStateFinished, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_room ListByOwner: %w", err)
	}
	return rows, nil
}

func (m *defaultLiveRoomModel) List(ctx context.Context, q RoomListQuery) ([]*LiveRoom, error) {
	if q.Limit <= 0 {
		q.Limit = defaultListLimit
	}
	where, args := roomListWhere(q)
	query := "SELECT " + liveRoomColumns + " FROM live_room WHERE " + where +
		" ORDER BY " + roomListOrder(q.Order) + " LIMIT ?"
	args = append(args, q.Limit)

	var rows []*LiveRoom
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_room List: %w", err)
	}
	return rows, nil
}

// Count 返回 List 同条件下的总数。与 List 共用 roomListWhere，保证页码与总数口径一致。
func (m *defaultLiveRoomModel) Count(ctx context.Context, q RoomListQuery) (int64, error) {
	where, args := roomListWhere(q)
	var total int64
	query := "SELECT COUNT(*) FROM live_room WHERE " + where
	if err := m.conn.QueryRowCtx(ctx, &total, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_room Count: %w", err)
	}
	return total, nil
}

func (m *defaultLiveRoomModel) CountByOwner(ctx context.Context, ownerMid int64, states []int32) (int64, error) {
	if ownerMid <= 0 {
		return 0, ErrInvalidMid
	}
	return m.countByStates(ctx, "owner_mid", ownerMid, states)
}

func (m *defaultLiveRoomModel) CountByArea(ctx context.Context, areaID int64, states []int32) (int64, error) {
	if areaID <= 0 {
		return 0, ErrInvalidAreaID
	}
	return m.countByStates(ctx, "area_id", areaID, states)
}

// countByStates 按「某一列 = 值 且 state IN (...)」计数，供两类上限校验复用。
func (m *defaultLiveRoomModel) countByStates(ctx context.Context, column string, value int64, states []int32) (int64, error) {
	// column 只取本包内写死的字面量（owner_mid / area_id），不接受外部输入，
	// 因此不存在注入面；这一点由调用点约束，不靠运行时白名单。
	args := []interface{}{value}
	query := "SELECT COUNT(*) FROM live_room WHERE " + column + " = ?"
	if len(states) > 0 {
		query += " AND state IN (" + placeholders(len(states)) + ")"
		for _, s := range states {
			args = append(args, s)
		}
	}
	var cnt int64
	if err := m.conn.QueryRowCtx(ctx, &cnt, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_room countByStates(%s): %w", column, err)
	}
	return cnt, nil
}

func (m *defaultLiveRoomModel) Transition(ctx context.Context, roomID int64, from, to int32, expectVersion int32, patch RoomPatch) (bool, error) {
	return m.transition(ctx, m.conn, roomID, from, to, expectVersion, patch)
}

func (m *defaultLiveRoomModel) TransitionTx(ctx context.Context, session sqlx.Session, roomID int64, from, to int32, expectVersion int32, patch RoomPatch) (bool, error) {
	if session == nil {
		return m.Transition(ctx, roomID, from, to, expectVersion, patch)
	}
	return m.transition(ctx, session, roomID, from, to, expectVersion, patch)
}

// transition 是房间状态迁移的唯一 SQL 实现，execer 允许是连接或事务句柄。
func (m *defaultLiveRoomModel) transition(ctx context.Context, execer sqlx.Session, roomID int64, from, to int32, expectVersion int32, patch RoomPatch) (bool, error) {
	if !CanRoomTransition(from, to) {
		return false, ErrInvalidRoomTransition
	}
	set := []string{"state = ?", "state_version = state_version + 1", "mtime = ?"}
	args := []interface{}{to, nowUnix()}
	set = append(set, patch.setClauses(&args)...)

	query := "UPDATE live_room SET " + strings.Join(set, ", ") + " WHERE room_id = ? AND state = ?"
	args = append(args, roomID, from)
	if expectVersion > 0 {
		query += " AND state_version = ?"
		args = append(args, expectVersion)
	}

	res, err := execer.ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("live_room Transition: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("live_room Transition RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultLiveRoomModel) UpdateProfile(ctx context.Context, roomID int64, title, cover string, areaID int64,
	verifyState int32, moderationTaskID int64, allowStates []int32) (bool, error) {
	if len(allowStates) == 0 {
		return false, ErrRoomStateNotEditable
	}
	set := []string{"mtime = ?"}
	args := []interface{}{nowUnix()}
	if title != "" {
		set = append(set, "title = ?")
		args = append(args, title)
	}
	if cover != "" {
		set = append(set, "cover = ?")
		args = append(args, cover)
	}
	if areaID > 0 {
		set = append(set, "area_id = ?")
		args = append(args, areaID)
	}
	if verifyState != VerifyStateUnspecified {
		set = append(set, "verify_state = ?")
		args = append(args, verifyState)
		// 重新送审时必须清掉上一次的驳回原因，否则客户端会一直显示旧理由。
		set = append(set, "reject_reason = ?")
		args = append(args, "")
	}
	if moderationTaskID > 0 {
		set = append(set, "moderation_task_id = ?")
		args = append(args, moderationTaskID)
	}

	query := "UPDATE live_room SET " + strings.Join(set, ", ") +
		" WHERE room_id = ? AND state IN (" + placeholders(len(allowStates)) + ")"
	args = append(args, roomID)
	for _, s := range allowStates {
		args = append(args, s)
	}

	res, err := m.conn.ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("live_room UpdateProfile: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("live_room UpdateProfile RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultLiveRoomModel) ClearActiveSession(ctx context.Context, roomID, sessionID int64) (bool, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE live_room SET active_session_id = 0, active_stream_id = '', mtime = ? "+
			"WHERE room_id = ? AND active_session_id = ?",
		nowUnix(), roomID, sessionID)
	if err != nil {
		return false, fmt.Errorf("live_room ClearActiveSession: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("live_room ClearActiveSession RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultLiveRoomModel) ListBansToExpire(ctx context.Context, now int64, limit int32) ([]*LiveRoom, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	// 只捞「临时禁播且已到期」的房间：ban_until=0 是永久禁播，必须由 LiftBan 解除。
	query := "SELECT " + liveRoomColumns + " FROM live_room " +
		"WHERE state = ? AND ban_until > 0 AND ban_until <= ? ORDER BY ban_until ASC LIMIT ?"
	var rows []*LiveRoom
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, RoomStateBanned, now, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_room ListBansToExpire: %w", err)
	}
	return rows, nil
}

// SetVerifyResult 见接口注释：资料审核结论与房间业务状态是两条状态机，
// 只有前者可独立回写，后者必须走 Transition。
func (m *defaultLiveRoomModel) SetVerifyResult(ctx context.Context, roomID int64, verifyState int32,
	rejectReason string, allowStates []int32) (bool, error) {
	return m.setVerifyResult(ctx, m.conn, roomID, verifyState, rejectReason, allowStates)
}

// SetVerifyResultTx 事务内版本：ApplyRoomModerationResult 要把「结论回写 + 审计日志」
// 一起提交，用连接版本会跑到事务外（另一条连接），既可能自锁也不原子。
func (m *defaultLiveRoomModel) SetVerifyResultTx(ctx context.Context, session sqlx.Session, roomID int64,
	verifyState int32, rejectReason string, allowStates []int32) (bool, error) {
	if session == nil {
		return m.SetVerifyResult(ctx, roomID, verifyState, rejectReason, allowStates)
	}
	return m.setVerifyResult(ctx, session, roomID, verifyState, rejectReason, allowStates)
}

func (m *defaultLiveRoomModel) setVerifyResult(ctx context.Context, execer sqlx.Session, roomID int64,
	verifyState int32, rejectReason string, allowStates []int32) (bool, error) {
	if roomID <= 0 {
		return false, ErrInvalidRoomID
	}
	if !ValidVerifyState(verifyState) {
		return false, ErrInvalidVerifyTransition
	}
	if len(allowStates) == 0 {
		return false, ErrRoomStateNotEditable
	}
	query := "UPDATE live_room SET verify_state = ?, reject_reason = ?, mtime = ? " +
		"WHERE room_id = ? AND state IN (" + placeholders(len(allowStates)) + ")"
	args := []interface{}{verifyState, rejectReason, nowUnix(), roomID}
	for _, s := range allowStates {
		args = append(args, s)
	}
	res, err := execer.ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("live_room SetVerifyResult: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("live_room SetVerifyResult RowsAffected: %w", err)
	}
	return aff > 0, nil
}

// SetBanUntil 见接口注释：只回写禁播到期投影，不推进房间业务状态。
func (m *defaultLiveRoomModel) SetBanUntil(ctx context.Context, roomID int64, banUntil int64,
	allowStates []int32) (bool, error) {
	return m.setBanUntil(ctx, m.conn, roomID, banUntil, allowStates)
}

// SetBanUntilTx 事务内版本，供 BanRoom 替换生效禁播区间时使用。
func (m *defaultLiveRoomModel) SetBanUntilTx(ctx context.Context, session sqlx.Session, roomID int64,
	banUntil int64, allowStates []int32) (bool, error) {
	if session == nil {
		return m.SetBanUntil(ctx, roomID, banUntil, allowStates)
	}
	return m.setBanUntil(ctx, session, roomID, banUntil, allowStates)
}

func (m *defaultLiveRoomModel) setBanUntil(ctx context.Context, execer sqlx.Session, roomID int64,
	banUntil int64, allowStates []int32) (bool, error) {
	if roomID <= 0 {
		return false, ErrInvalidRoomID
	}
	if banUntil < 0 {
		return false, ErrBanDurationRequired
	}
	if len(allowStates) == 0 {
		return false, ErrRoomStateNotEditable
	}
	for _, s := range allowStates {
		if !ValidRoomState(s) {
			return false, ErrInvalidRoomTransition
		}
	}
	query := "UPDATE live_room SET ban_until = ?, mtime = ? " +
		"WHERE room_id = ? AND state IN (" + placeholders(len(allowStates)) + ")"
	args := []interface{}{banUntil, nowUnix(), roomID}
	for _, s := range allowStates {
		args = append(args, s)
	}
	res, err := execer.ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("live_room SetBanUntil: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("live_room SetBanUntil RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultLiveRoomModel) ListByRoomIDs(ctx context.Context, ids []int64, q RoomListQuery) ([]*LiveRoom, error) {
	if len(ids) == 0 {
		// 空集合不是「全表」：直接返回空页，避免 placeholders(0) 拼出非法 SQL。
		return nil, nil
	}
	if q.Limit <= 0 {
		q.Limit = defaultListLimit
	}
	// 复用 roomListWhere + roomListOrder，保证「按主播可见」与「按房主过滤」两条路径
	// 的过滤与排序口径完全一致（q.OwnerMid 由调用方留空，可见性来自 ids）。
	where, args := roomListWhere(q)
	where += " AND room_id IN (" + placeholders(len(ids)) + ")"
	for _, id := range ids {
		args = append(args, id)
	}
	query := "SELECT " + liveRoomColumns + " FROM live_room WHERE " + where +
		" ORDER BY " + roomListOrder(q.Order) + " LIMIT ?"
	args = append(args, q.Limit)

	var rows []*LiveRoom
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_room ListByRoomIDs: %w", err)
	}
	return rows, nil
}

// setClauses 把 patch 里的非 nil 字段翻译成 SET 片段，并把值按顺序追加进 args。
func (p RoomPatch) setClauses(args *[]interface{}) []string {
	var set []string
	if p.VerifyState != nil {
		set = append(set, "verify_state = ?")
		*args = append(*args, *p.VerifyState)
	}
	if p.RejectReason != nil {
		set = append(set, "reject_reason = ?")
		*args = append(*args, *p.RejectReason)
	}
	if p.BanUntil != nil {
		set = append(set, "ban_until = ?")
		*args = append(*args, *p.BanUntil)
	}
	if p.ActiveSessionID != nil {
		set = append(set, "active_session_id = ?")
		*args = append(*args, *p.ActiveSessionID)
	}
	if p.ActiveStreamID != nil {
		set = append(set, "active_stream_id = ?")
		*args = append(*args, *p.ActiveStreamID)
	}
	if p.ModerationTaskID != nil {
		set = append(set, "moderation_task_id = ?")
		*args = append(*args, *p.ModerationTaskID)
	}
	return set
}

// roomListWhere 构造 ListRooms / Count 共用的 WHERE 片段（不含 LIMIT），
// 保证「列表内容」与「总数」的过滤口径永不分裂。
func roomListWhere(q RoomListQuery) (string, []interface{}) {
	var (
		sb   strings.Builder
		args []interface{}
	)
	sb.WriteString("state > ?")
	args = append(args, RoomStateUnspecified) // 脏数据/未初始化行不参与浏览
	if q.OwnerMid > 0 {
		sb.WriteString(" AND owner_mid = ?")
		args = append(args, q.OwnerMid)
	}
	if q.AreaID > 0 {
		sb.WriteString(" AND area_id = ?")
		args = append(args, q.AreaID)
	}
	if q.State != RoomStateUnspecified {
		sb.WriteString(" AND state = ?")
		args = append(args, q.State)
	}
	return sb.String(), args
}

// roomListOrder 把 rpc.RoomOrder 映射成 ORDER BY 片段。
// 取值全部来自本包常量表，不存在拼接注入面。
func roomListOrder(order int32) string {
	switch order {
	case RoomOrderLivingFirst:
		// 直播中优先：state=3 的行排前，其次 room_id 倒序（新房间优先）。
		return "(state = " + fmt.Sprint(RoomStateLiving) + ") DESC, room_id DESC"
	case RoomOrderCtimeDesc:
		return "ctime DESC, room_id DESC"
	default:
		return "room_id DESC"
	}
}
