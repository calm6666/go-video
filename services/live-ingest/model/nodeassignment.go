package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// nodeAssignmentColumns 是 live_node_assignment 的列清单，必须与
// deploy/migrations/live-ingest/000001_create_live_ingest_key_tables.sql 完全一致。
const nodeAssignmentColumns = "assignment_id, request_id, stream_id, room_id, key_id, node_id, protocol, state, " +
	"score, prev_node_id, reason, release_reason, assigned_at, released_at, trace_id, ctime, mtime"

// NodeAssignment 接入节点分配记录（live_node_assignment 表投影）。
//
// 一行 = 一次「某流被分配到某节点」的生命周期：state=ACTIVE 时占用节点配额，
// 释放/迁移时同事务把 live_ingest_node.active_streams 减一。
// uniq_request_id 让 AssignIngestNode 可安全重放；迁移产生的旧记录置 MIGRATED，
// 保留行做容量对账与排障（AGENTS.md §8 要求状态变更可审计）。
type NodeAssignment struct {
	AssignmentID  int64  `db:"assignment_id"`  // 自增主键
	RequestID     string `db:"request_id"`     // 分配幂等键（唯一索引）
	StreamID      string `db:"stream_id"`      // 流 ID
	RoomID        int64  `db:"room_id"`        // 房间引用
	KeyID         int64  `db:"key_id"`         // 密钥 ID（引用，不含密钥内容）
	NodeID        string `db:"node_id"`        // 分配到的节点
	Protocol      int32  `db:"protocol"`       // 接入协议枚举值
	State         int32  `db:"state"`          // 见 AssignmentState*
	Score         int32  `db:"score"`          // 分配打分（观测）
	PrevNodeID    string `db:"prev_node_id"`   // 迁移来源节点，空串表示首次分配
	Reason        string `db:"reason"`         // 分配原因
	ReleaseReason string `db:"release_reason"` // 释放/迁移原因
	AssignedAt    int64  `db:"assigned_at"`    // 分配时间（Unix 秒）
	ReleasedAt    int64  `db:"released_at"`    // 释放时间（Unix 秒），0 表示未释放
	TraceID       string `db:"trace_id"`       // 链路追踪 ID
	Ctime         int64  `db:"ctime"`          // 创建时间（Unix 秒）
	Mtime         int64  `db:"mtime"`          // 修改时间（Unix 秒）
}

// NodeAssignmentFilter 分配记录查询条件；零值表示不过滤。
type NodeAssignmentFilter struct {
	StreamID string
	NodeID   string
	RoomID   int64
	State    int32
	Offset   int32
	Limit    int32
}

// NodeAssignmentModel live_node_assignment 表查询与写入接口。
type NodeAssignmentModel interface {
	// Insert 写入分配记录并返回 ID；uniq_request_id 冲突表示重放。
	Insert(ctx context.Context, tx sqlx.Session, a *NodeAssignment) (int64, error)
	// FindOne 按记录 ID 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, assignmentID int64) (*NodeAssignment, error)
	// FindByRequest 按幂等键查询；不存在返回 (nil, nil)。
	FindByRequest(ctx context.Context, requestID string) (*NodeAssignment, error)
	// FindActiveByStream 返回该流当前生效的分配（state=ACTIVE，最多一条）；不存在返回 (nil, nil)。
	FindActiveByStream(ctx context.Context, tx sqlx.Session, streamID string) (*NodeAssignment, error)
	// Release 结束分配：仅当记录仍 ACTIVE（且 nodeID 非空时要求节点匹配）才生效，
	// 返回 false 表示已被并发释放或本就不存在。
	Release(ctx context.Context, tx sqlx.Session, assignmentID int64, toState int32, at int64, reason, nodeID string) (bool, error)
	// CountActiveByNode 统计节点当前生效分配数（配额对账，允许与 active_streams 有短暂偏差）。
	CountActiveByNode(ctx context.Context, nodeID string) (int64, error)
	// ListByFilter 分页查询分配记录，按 assignment_id 倒序，必须带 LIMIT。
	ListByFilter(ctx context.Context, f NodeAssignmentFilter) ([]*NodeAssignment, error)
	// CountByFilter 统计条件命中行数（分页 total）。
	CountByFilter(ctx context.Context, f NodeAssignmentFilter) (int64, error)
}

type defaultNodeAssignmentModel struct {
	conn sqlx.SqlConn
}

// NewNodeAssignmentModel 创建 NodeAssignmentModel 实现。
func NewNodeAssignmentModel(conn sqlx.SqlConn) NodeAssignmentModel {
	return &defaultNodeAssignmentModel{conn: conn}
}

func (m *defaultNodeAssignmentModel) Insert(ctx context.Context, tx sqlx.Session, a *NodeAssignment) (int64, error) {
	session := pickSession(m.conn, tx)
	if a.Ctime == 0 {
		a.Ctime = nowUnix()
	}
	a.Mtime = a.Ctime
	if a.AssignedAt == 0 {
		a.AssignedAt = a.Ctime
	}
	if a.State == 0 {
		a.State = AssignmentStateActive
	}
	res, err := session.ExecCtx(ctx,
		"INSERT INTO live_node_assignment (request_id, stream_id, room_id, key_id, node_id, protocol, state, score, "+
			"prev_node_id, reason, release_reason, assigned_at, released_at, trace_id, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, 0, ?, ?, ?)",
		a.RequestID, a.StreamID, a.RoomID, a.KeyID, a.NodeID, a.Protocol, a.State, a.Score,
		a.PrevNodeID, truncate(a.Reason, 255), a.AssignedAt, a.TraceID, a.Ctime, a.Mtime)
	if err != nil {
		return 0, fmt.Errorf("live_node_assignment Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_node_assignment Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultNodeAssignmentModel) FindOne(ctx context.Context, assignmentID int64) (*NodeAssignment, error) {
	var a NodeAssignment
	query := "SELECT " + nodeAssignmentColumns + " FROM live_node_assignment WHERE assignment_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &a, query, assignmentID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_node_assignment FindOne: %w", err)
	}
	return &a, nil
}

func (m *defaultNodeAssignmentModel) FindByRequest(ctx context.Context, requestID string) (*NodeAssignment, error) {
	if requestID == "" {
		return nil, ErrIdempotencyKeyRequired
	}
	var a NodeAssignment
	query := "SELECT " + nodeAssignmentColumns + " FROM live_node_assignment WHERE request_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &a, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_node_assignment FindByRequest: %w", err)
	}
	return &a, nil
}

func (m *defaultNodeAssignmentModel) FindActiveByStream(ctx context.Context, tx sqlx.Session, streamID string) (*NodeAssignment, error) {
	session := pickSession(m.conn, tx)
	var a NodeAssignment
	query := "SELECT " + nodeAssignmentColumns + " FROM live_node_assignment " +
		"WHERE stream_id = ? AND state = ? ORDER BY assignment_id DESC LIMIT 1"
	if err := session.QueryRowCtx(ctx, &a, query, streamID, AssignmentStateActive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_node_assignment FindActiveByStream: %w", err)
	}
	return &a, nil
}

func (m *defaultNodeAssignmentModel) Release(
	ctx context.Context,
	tx sqlx.Session,
	assignmentID int64,
	toState int32,
	at int64,
	reason string,
	nodeID string,
) (bool, error) {
	if !validAssignmentState(toState) || toState == AssignmentStateActive {
		return false, fmt.Errorf("live_node_assignment Release: invalid target state %d", toState)
	}
	session := pickSession(m.conn, tx)

	// 条件 UPDATE：assignment_id 是主键，再叠 state=ACTIVE 与可选 node_id 条件，
	// 保证并发释放/迁移只有一个人成功（RowsAffected=0 即表示本次没改到行）。
	conds := []string{"assignment_id = ?", "state = ?"}
	args := []interface{}{toState, truncate(reason, 255), at, nowUnix(), assignmentID, AssignmentStateActive}
	if nodeID != "" {
		conds = append(conds, "node_id = ?")
		args = append(args, nodeID)
	}
	query := "UPDATE live_node_assignment SET state = ?, release_reason = ?, released_at = ?, mtime = ? WHERE " +
		strings.Join(conds, " AND ")
	res, err := session.ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("live_node_assignment Release: %w", err)
	}
	return rowsAffected(res, "live_node_assignment Release")
}

func (m *defaultNodeAssignmentModel) CountActiveByNode(ctx context.Context, nodeID string) (int64, error) {
	var cnt int64
	err := m.conn.QueryRowCtx(ctx, &cnt,
		"SELECT COUNT(*) FROM live_node_assignment WHERE node_id = ? AND state = ? LIMIT 1",
		nodeID, AssignmentStateActive)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_node_assignment CountActiveByNode: %w", err)
	}
	return cnt, nil
}

func (m *defaultNodeAssignmentModel) ListByFilter(ctx context.Context, f NodeAssignmentFilter) ([]*NodeAssignment, error) {
	where, args := f.build()
	tail, tailArgs := pageArgs(f.Limit, 100, f.Offset)
	query := "SELECT " + nodeAssignmentColumns + " FROM live_node_assignment WHERE " + where +
		" ORDER BY assignment_id DESC" + tail
	args = append(args, tailArgs...)

	var rows []*NodeAssignment
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_node_assignment ListByFilter: %w", err)
	}
	return rows, nil
}

func (m *defaultNodeAssignmentModel) CountByFilter(ctx context.Context, f NodeAssignmentFilter) (int64, error) {
	where, args := f.build()
	var cnt int64
	if err := m.conn.QueryRowCtx(ctx, &cnt, "SELECT COUNT(*) FROM live_node_assignment WHERE "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_node_assignment CountByFilter: %w", err)
	}
	return cnt, nil
}

// build 组装 WHERE 片段与参数；恒真条件用 state <> 0 占位（state 列不会取 0）。
func (f NodeAssignmentFilter) build() (string, []interface{}) {
	conds := []string{"state <> 0"}
	args := make([]interface{}, 0, 4)
	if f.StreamID != "" {
		conds = append(conds, "stream_id = ?")
		args = append(args, f.StreamID)
	}
	if f.NodeID != "" {
		conds = append(conds, "node_id = ?")
		args = append(args, f.NodeID)
	}
	if f.RoomID > 0 {
		conds = append(conds, "room_id = ?")
		args = append(args, f.RoomID)
	}
	if validAssignmentState(f.State) {
		conds = append(conds, "state = ?")
		args = append(args, f.State)
	}
	return strings.Join(conds, " AND "), args
}

func validAssignmentState(state int32) bool {
	switch state {
	case AssignmentStateActive, AssignmentStateReleased, AssignmentStateMigrated:
		return true
	default:
		return false
	}
}
