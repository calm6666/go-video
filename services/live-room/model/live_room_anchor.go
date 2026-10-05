package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// liveRoomAnchorColumns 与 000003_create_live_room_anchor.sql 逐列对应。
// owner_room_id 是可空列：只有「生效房主」行写入 room_id，其余行为 NULL，
// 配合 uniq_active_owner 唯一索引，用 MySQL 的「NULL 不参与唯一性」语义
// 强制「一个房间同一时刻只有一个生效房主」（对应 rpc.MutateAnchorReply 校验）。
const liveRoomAnchorColumns = "id, room_id, mid, role, state, owner_room_id, ctime, mtime"

// LiveRoomAnchor 主播绑定行（live_room_anchor 表投影，对应 rpc.AnchorInfo）。
// 解绑是软状态（state=0）：保留行才能回答「某场直播当时谁是房管」这类审计问题。
type LiveRoomAnchor struct {
	ID          int64         `db:"id"`            // 记录 ID（主键）
	RoomID      int64         `db:"room_id"`       // 房间 ID
	Mid         int64         `db:"mid"`           // 主播/房管用户 ID
	Role        int32         `db:"role"`          // 角色，见 AnchorRole* 常量
	State       int32         `db:"state"`         // 1 生效、0 已解绑
	OwnerRoomID sql.NullInt64 `db:"owner_room_id"` // 生效房主占位列，非房主为 NULL
	Ctime       int64         `db:"ctime"`         // 创建时间（Unix 秒）
	Mtime       int64         `db:"mtime"`         // 修改时间（Unix 秒）
}

// AnchorListQuery 是 ListAnchors 的过滤条件。
type AnchorListQuery struct {
	RoomID      int64
	Role        int32 // AnchorRoleUnspecified 表示不按角色过滤
	OnlyEnabled bool
	Offset      int32
	Limit       int32
}

// LiveRoomAnchorModel live_room_anchor 表读写接口。
type LiveRoomAnchorModel interface {
	// Bind 绑定/重新启用一个 (room_id, mid, role)：命中 uniq_room_mid_role 时把该行
	// 置回生效并刷新房主占位列。返回新插入时的自增 id（重绑返回 0）。
	// role=AnchorRoleOwner 时会占用 uniq_active_owner：已有别的生效房主则返回 ErrDuplicateOwner。
	Bind(ctx context.Context, roomID, mid int64, role int32) (int64, error)
	// BindTx 在事务内绑定，供 CreateRoom「房间 + 房主绑定 + 配置」同事务提交使用。
	// session 为 nil 时退化为 Bind（不分裂成两次写入的语义不变）。
	BindTx(ctx context.Context, session sqlx.Session, roomID, mid int64, role int32) (int64, error)
	// FindOwner 读房间的生效房主；没有返回 (nil, nil)。
	FindOwner(ctx context.Context, roomID int64) (*LiveRoomAnchor, error)
	// Find 读指定 (room_id, mid, role) 行；不存在返回 (nil, nil)。
	Find(ctx context.Context, roomID, mid int64, role int32) (*LiveRoomAnchor, error)
	// IsEnabled 判断某 mid 在房间内是否以任一角色生效（开播资格与操作者校验用）。
	// 返回的 role 是命中的首个角色（房主优先）。
	IsEnabled(ctx context.Context, roomID, mid int64) (bool, int32, error)
	// List 分页查询房间绑定，按 role、id 升序（房主在最前，便于客户端直接取首位）。
	List(ctx context.Context, q AnchorListQuery) ([]*LiveRoomAnchor, error)
	// Count 返回 List 同条件下的总数。
	Count(ctx context.Context, q AnchorListQuery) (int64, error)
	// CountActiveRoomsByMid 统计某主播作为「生效房主」的房间数（MutateAnchor 上限校验，
	// 即 rpc.MutateAnchorReply.bound_count）。role 为 AnchorRoleUnspecified 时统计全部角色。
	CountActiveRoomsByMid(ctx context.Context, mid int64, role int32) (int64, error)
	// Unbind 解绑：role 为 AnchorRoleUnspecified 时解绑该 mid 在本房间的全部非房主角色。
	// 房主行永不通过本方法解绑（ErrCannotUnbindOwner）。返回受影响行数。
	Unbind(ctx context.Context, roomID, mid int64, role int32) (int64, error)
	// TransferOwner 换房主：在同一事务内先释放旧房主的占位列、再让 toMid 占位，
	// 避免出现「无房主房间」或两行同时持有占位。
	// fromMid 已不是生效房主时返回 (false, ErrConcurrentUpdate)；
	// 占位被第三方抢到时返回 (false, ErrDuplicateOwner)。两者都不产生写入。
	TransferOwner(ctx context.Context, roomID, fromMid, toMid int64) (bool, error)
	// ListRoomsByMid 按主播读其生效绑定到的房间 ID（room_id 倒序，limit 截断），
	// 供「我的直播间」列表在 live_room 表二次取行。
	ListRoomsByMid(ctx context.Context, mid int64, role int32, limit int32) ([]int64, error)
}

type defaultLiveRoomAnchorModel struct {
	conn sqlx.SqlConn
}

// NewLiveRoomAnchorModel 创建 LiveRoomAnchorModel 实现。
func NewLiveRoomAnchorModel(conn sqlx.SqlConn) LiveRoomAnchorModel {
	return &defaultLiveRoomAnchorModel{conn: conn}
}

func (m *defaultLiveRoomAnchorModel) Bind(ctx context.Context, roomID, mid int64, role int32) (int64, error) {
	return m.bind(ctx, m.conn, roomID, mid, role)
}

func (m *defaultLiveRoomAnchorModel) BindTx(ctx context.Context, session sqlx.Session,
	roomID, mid int64, role int32) (int64, error) {
	if session == nil {
		return m.Bind(ctx, roomID, mid, role)
	}
	return m.bind(ctx, session, roomID, mid, role)
}

// bind 是绑定行的唯一 SQL 实现，execer 可以是连接或事务句柄。
func (m *defaultLiveRoomAnchorModel) bind(ctx context.Context, execer sqlx.Session,
	roomID, mid int64, role int32) (int64, error) {
	if roomID <= 0 {
		return 0, ErrInvalidRoomID
	}
	if mid <= 0 {
		return 0, ErrInvalidMid
	}
	if !ValidAnchorRole(role) {
		return 0, ErrAnchorRoleInvalid
	}
	now := nowUnix()
	// 占位列只在「生效 + 房主」时写值：解绑行必须释放占位，否则房主永远换不掉。
	const query = "INSERT INTO live_room_anchor (room_id, mid, role, state, owner_room_id, ctime, mtime) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?) " +
		"ON DUPLICATE KEY UPDATE state = VALUES(state), owner_room_id = VALUES(owner_room_id), mtime = VALUES(mtime)"
	var owner interface{}
	if role == AnchorRoleOwner {
		owner = roomID
	}
	res, err := execer.ExecCtx(ctx, query, roomID, mid, role, BindStateEnabled, owner, now, now)
	if err != nil {
		// uniq_active_owner 冲突不是故障：房主已被别人持有，翻译成语义错误给 logic。
		if isDuplicateErr(err) {
			return 0, ErrDuplicateOwner
		}
		return 0, fmt.Errorf("live_room_anchor Bind: %w", err)
	}
	// LastInsertId 在 ON DUPLICATE 更新分支上会返回无意义的自增值，
	// 因此只在确实新增行（RowsAffected==1）时才采用。
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_room_anchor Bind RowsAffected: %w", err)
	}
	if aff != 1 {
		return 0, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_room_anchor Bind LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultLiveRoomAnchorModel) FindOwner(ctx context.Context, roomID int64) (*LiveRoomAnchor, error) {
	var a LiveRoomAnchor
	query := "SELECT " + liveRoomAnchorColumns + " FROM live_room_anchor " +
		"WHERE room_id = ? AND role = ? AND state = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &a, query, roomID, AnchorRoleOwner, BindStateEnabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_room_anchor FindOwner: %w", err)
	}
	return &a, nil
}

func (m *defaultLiveRoomAnchorModel) Find(ctx context.Context, roomID, mid int64, role int32) (*LiveRoomAnchor, error) {
	var a LiveRoomAnchor
	query := "SELECT " + liveRoomAnchorColumns + " FROM live_room_anchor WHERE room_id = ? AND mid = ? AND role = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &a, query, roomID, mid, role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_room_anchor Find: %w", err)
	}
	return &a, nil
}

func (m *defaultLiveRoomAnchorModel) IsEnabled(ctx context.Context, roomID, mid int64) (bool, int32, error) {
	if roomID <= 0 || mid <= 0 {
		return false, 0, ErrInvalidMid
	}
	var role int32
	// role 升序命中即房主优先（OWNER=1 < COHOST=2 < MANAGER=3）。
	query := "SELECT role FROM live_room_anchor WHERE room_id = ? AND mid = ? AND state = ? " +
		"ORDER BY role ASC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &role, query, roomID, mid, BindStateEnabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("live_room_anchor IsEnabled: %w", err)
	}
	return true, role, nil
}

func (m *defaultLiveRoomAnchorModel) List(ctx context.Context, q AnchorListQuery) ([]*LiveRoomAnchor, error) {
	if q.RoomID <= 0 {
		return nil, ErrInvalidRoomID
	}
	if q.Limit <= 0 {
		q.Limit = defaultListLimit
	}
	where, args := anchorWhere(q)
	query := "SELECT " + liveRoomAnchorColumns + " FROM live_room_anchor WHERE " + where +
		" ORDER BY role ASC, id ASC LIMIT ?"
	args = append(args, q.Limit)

	var rows []*LiveRoomAnchor
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_room_anchor List: %w", err)
	}
	return rows, nil
}

func (m *defaultLiveRoomAnchorModel) Count(ctx context.Context, q AnchorListQuery) (int64, error) {
	if q.RoomID <= 0 {
		return 0, ErrInvalidRoomID
	}
	where, args := anchorWhere(q)
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM live_room_anchor WHERE "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_room_anchor Count: %w", err)
	}
	return total, nil
}

func (m *defaultLiveRoomAnchorModel) CountActiveRoomsByMid(ctx context.Context, mid int64, role int32) (int64, error) {
	if mid <= 0 {
		return 0, ErrInvalidMid
	}
	query := "SELECT COUNT(DISTINCT room_id) FROM live_room_anchor WHERE mid = ? AND state = ?"
	args := []interface{}{mid, BindStateEnabled}
	if role != AnchorRoleUnspecified {
		query += " AND role = ?"
		args = append(args, role)
	}
	var cnt int64
	if err := m.conn.QueryRowCtx(ctx, &cnt, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_room_anchor CountActiveRoomsByMid: %w", err)
	}
	return cnt, nil
}

func (m *defaultLiveRoomAnchorModel) Unbind(ctx context.Context, roomID, mid int64, role int32) (int64, error) {
	if roomID <= 0 {
		return 0, ErrInvalidRoomID
	}
	if mid <= 0 {
		return 0, ErrInvalidMid
	}
	if role != AnchorRoleUnspecified && !ValidAnchorRole(role) {
		return 0, ErrAnchorRoleInvalid
	}
	// 房主的解绑/换绑必须走 TransferOwner（先立新人再退旧人），
	// 这里无条件排除 OWNER，避免出现「无房主房间」。
	query := "UPDATE live_room_anchor SET state = ?, owner_room_id = NULL, mtime = ? " +
		"WHERE room_id = ? AND mid = ? AND state = ? AND role <> ?"
	args := []interface{}{BindStateDisabled, nowUnix(), roomID, mid, BindStateEnabled, AnchorRoleOwner}
	if role != AnchorRoleUnspecified {
		query += " AND role = ?"
		args = append(args, role)
	}
	res, err := m.conn.ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("live_room_anchor Unbind: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_room_anchor Unbind RowsAffected: %w", err)
	}
	return aff, nil
}

func (m *defaultLiveRoomAnchorModel) TransferOwner(ctx context.Context, roomID, fromMid, toMid int64) (bool, error) {
	if roomID <= 0 || fromMid <= 0 || toMid <= 0 || fromMid == toMid {
		return false, ErrInvalidMid
	}
	now := nowUnix()
	err := m.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		// 顺序很关键：先释放旧房主的占位再让新人占位。反过来会在同一事务里
		// 撞上 uniq_active_owner（旧行还持有 room_id），换房主永远失败。
		res, err := session.ExecCtx(ctx,
			"UPDATE live_room_anchor SET state = ?, owner_room_id = NULL, mtime = ? "+
				"WHERE room_id = ? AND mid = ? AND role = ? AND state = ?",
			BindStateDisabled, now, roomID, fromMid, AnchorRoleOwner, BindStateEnabled)
		if err != nil {
			return fmt.Errorf("live_room_anchor TransferOwner release old: %w", err)
		}
		released, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("live_room_anchor TransferOwner RowsAffected: %w", err)
		}
		if released == 0 {
			// fromMid 已不是生效房主：并发换绑发生过，整事务回滚（不会把 toMid 提成房主）。
			return errOwnerNotHeld
		}
		if _, err := session.ExecCtx(ctx,
			"INSERT INTO live_room_anchor (room_id, mid, role, state, owner_room_id, ctime, mtime) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?) "+
				"ON DUPLICATE KEY UPDATE state = VALUES(state), owner_room_id = VALUES(owner_room_id), mtime = VALUES(mtime)",
			roomID, toMid, AnchorRoleOwner, BindStateEnabled, roomID, now, now); err != nil {
			return fmt.Errorf("live_room_anchor TransferOwner bind new: %w", err)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errOwnerNotHeld) {
			return false, ErrConcurrentUpdate
		}
		// 唯一键冲突不是故障：说明另一笔换房主已抢先占位。
		if isDuplicateErr(err) {
			return false, ErrDuplicateOwner
		}
		return false, err
	}
	return true, nil
}

func (m *defaultLiveRoomAnchorModel) ListRoomsByMid(ctx context.Context, mid int64, role int32, limit int32) ([]int64, error) {
	if mid <= 0 {
		return nil, ErrInvalidMid
	}
	if limit <= 0 {
		limit = defaultListLimit
	}
	query := "SELECT room_id FROM live_room_anchor WHERE mid = ? AND state = ?"
	args := []interface{}{mid, BindStateEnabled}
	if role != AnchorRoleUnspecified {
		query += " AND role = ?"
		args = append(args, role)
	}
	query += " ORDER BY room_id DESC LIMIT ?"
	args = append(args, limit)

	var rooms []int64
	if err := m.conn.QueryRowsCtx(ctx, &rooms, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_room_anchor ListRoomsByMid: %w", err)
	}
	return rooms, nil
}

// anchorWhere 构造 List/Count 共用的 WHERE 片段。
func anchorWhere(q AnchorListQuery) (string, []interface{}) {
	var (
		sb   strings.Builder
		args []interface{}
	)
	sb.WriteString("room_id = ?")
	args = append(args, q.RoomID)
	if q.Role != AnchorRoleUnspecified {
		sb.WriteString(" AND role = ?")
		args = append(args, q.Role)
	}
	if q.OnlyEnabled {
		sb.WriteString(" AND state = ?")
		args = append(args, BindStateEnabled)
	}
	return sb.String(), args
}

// isDuplicateErr 判定在 errors.go（本包统一按错误报文做字符串判定，
// 不导入 go-sql-driver/mysql：该依赖在 go.mod 里是 indirect，直接引用会改依赖分类）。
