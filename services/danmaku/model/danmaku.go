package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 弹幕状态常量，与 danmaku.state 列和 rpc.DanmakuState 取值严格一致。
// 变更取值会破坏已落库数据和 moderation.result.v1 契约，禁止重排。
const (
	// StateNormal 审核通过后对所有人可见。
	StateNormal int32 = 0
	// StatePending 待审核，仅发送者本人可见。
	StatePending int32 = 1
	// StateFolded 折叠（命中屏蔽词或被降权），仅发送者本人可见。
	StateFolded int32 = 2
	// StateDeleted 软删除，保留行作为审计证据。
	StateDeleted int32 = 3
	// StateRejected 审核驳回。
	StateRejected int32 = 4
)

// 弹幕池常量，与 danmaku.pool 列和 rpc.DanmakuPool 取值一致。
const (
	// PoolNormal 普通池，参与时间轴下发。
	PoolNormal int32 = 1
	// PoolReview 审核池，等待机审/人审结论。
	PoolReview int32 = 2
	// PoolBlock 屏蔽池，只对发送者自己回显。
	PoolBlock int32 = 4
)

// danmakuColumns 是 danmaku 表的完整列清单，
// 与 deploy/migrations/danmaku/000001_create_danmaku_tables.sql 一一对应。
const danmakuColumns = "dmid, oid, aid, mid, progress_ms, mode, fontsize, color, content, state, pool, seg_no, idempotency_key, moderation_task_id, trace_id, ctime, mtime"

// Danmaku 弹幕主体行（DB 投影）。
// 时间轴定位由 (oid, seg_no) 索引承载，seg_no = progress_ms / segment_ms，
// 客户端按分段窗口批量拉取，禁止按 progress_ms 逐条查询。
type Danmaku struct {
	Dmid             int64  `db:"dmid"`               // 弹幕 ID
	Oid              int64  `db:"oid"`                // 内容主键（视频 aid / 直播 room_id）
	Aid              int64  `db:"aid"`                // 稿件 ID（归档投影用）
	Mid              int64  `db:"mid"`                // 发送者用户 ID
	ProgressMs       int64  `db:"progress_ms"`        // 时间轴位置（毫秒）
	Mode             int32  `db:"mode"`               // 展示模式
	Fontsize         int32  `db:"fontsize"`           // 字号
	Color            int32  `db:"color"`              // RGB 颜色整数值
	Content          string `db:"content"`            // 弹幕正文
	State            int32  `db:"state"`              // 状态，见 State* 常量
	Pool             int32  `db:"pool"`               // 弹幕池，见 Pool* 常量
	SegNo            int32  `db:"seg_no"`             // 时间分段号
	IdempotencyKey   string `db:"idempotency_key"`    // 幂等键（服务端派生哈希）
	ModerationTaskId int64  `db:"moderation_task_id"` // 机审任务 ID，0 表示未提交
	TraceId          string `db:"trace_id"`           // 链路追踪 ID
	Ctime            int64  `db:"ctime"`              // 创建时间（Unix 秒）
	Mtime            int64  `db:"mtime"`              // 修改时间（Unix 秒）
}

// DanmakuModel danmaku 表查询与写入接口。
type DanmakuModel interface {
	// Insert 写入弹幕，返回新 dmid。idempotency_key 上有唯一索引，
	// 并发重复写入会返回错误，调用方需回查后按重放处理。
	Insert(ctx context.Context, d *Danmaku) (int64, error)
	// FindOne 按 dmid 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, dmid int64) (*Danmaku, error)
	// FindByIdempotencyKey 按幂等键查询已落库弹幕；不存在返回 (nil, nil)。
	FindByIdempotencyKey(ctx context.Context, key string) (*Danmaku, error)
	// ListVisibleBySegs 拉取某内容若干分段内可下发弹幕
	// （state=NORMAL 且 pool=NORMAL），按 progress_ms 升序，limit 截断。
	ListVisibleBySegs(ctx context.Context, oid int64, segs []int32, limit int32) ([]*Danmaku, error)
	// ListMineBySegs 拉取本人指定分段内的非可见态弹幕（待审/折叠/驳回），
	// 用于发送后回显，不走缓存。
	ListMineBySegs(ctx context.Context, oid, mid int64, segs []int32) ([]*Danmaku, error)
	// TransitionState 以 fromState 为条件推进到 toState+pool（CAS 语义）。
	// 返回 false 表示状态已被并发修改，调用方需重新读取。
	TransitionState(ctx context.Context, dmid int64, fromState, toState, toPool int32) (bool, error)
	// TransitionStateTx 在事务内做同样的 CAS 迁移，供与 op_log 同事务提交。
	TransitionStateTx(ctx context.Context, session sqlx.Session, dmid int64, fromState, toState, toPool int32) (bool, error)
	// SetModerationTaskID 回填机审任务 ID（仅当当前为 0 时写入）。
	SetModerationTaskID(ctx context.Context, dmid, taskID int64) error
	// SetModerationTaskIDTx 在事务内回填机审任务 ID。
	SetModerationTaskIDTx(ctx context.Context, session sqlx.Session, dmid, taskID int64) error
	// CountVisibleByOid 统计某内容可见弹幕总数（观测与举报页展示用）。
	CountVisibleByOid(ctx context.Context, oid int64) (int64, error)
}

type defaultDanmakuModel struct {
	conn sqlx.SqlConn
}

// NewDanmakuModel 创建 DanmakuModel 实现。
func NewDanmakuModel(conn sqlx.SqlConn) DanmakuModel {
	return &defaultDanmakuModel{conn: conn}
}

func (m *defaultDanmakuModel) Insert(ctx context.Context, d *Danmaku) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO danmaku (oid, aid, mid, progress_ms, mode, fontsize, color, content, state, pool, seg_no, idempotency_key, moderation_task_id, trace_id, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		d.Oid, d.Aid, d.Mid, d.ProgressMs, d.Mode, d.Fontsize, d.Color, d.Content,
		d.State, d.Pool, d.SegNo, d.IdempotencyKey, d.ModerationTaskId, d.TraceId, d.Ctime, d.Mtime)
	if err != nil {
		return 0, fmt.Errorf("danmaku Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("danmaku Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultDanmakuModel) FindOne(ctx context.Context, dmid int64) (*Danmaku, error) {
	var d Danmaku
	query := "SELECT " + danmakuColumns + " FROM danmaku WHERE dmid = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &d, query, dmid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("danmaku FindOne: %w", err)
	}
	return &d, nil
}

func (m *defaultDanmakuModel) FindByIdempotencyKey(ctx context.Context, key string) (*Danmaku, error) {
	var d Danmaku
	query := "SELECT " + danmakuColumns + " FROM danmaku WHERE idempotency_key = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &d, query, key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("danmaku FindByIdempotencyKey: %w", err)
	}
	return &d, nil
}

func (m *defaultDanmakuModel) ListVisibleBySegs(ctx context.Context, oid int64, segs []int32, limit int32) ([]*Danmaku, error) {
	if len(segs) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 1000
	}
	in, args := segInClause(oid, segs)
	query := "SELECT " + danmakuColumns + " FROM danmaku WHERE " + in +
		" AND state = ? AND pool = ? ORDER BY progress_ms ASC LIMIT ?"
	args = append(args, StateNormal, PoolNormal, limit)

	var rows []*Danmaku
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("danmaku ListVisibleBySegs: %w", err)
	}
	return rows, nil
}

func (m *defaultDanmakuModel) ListMineBySegs(ctx context.Context, oid, mid int64, segs []int32) ([]*Danmaku, error) {
	if len(segs) == 0 || mid <= 0 {
		return nil, nil
	}
	in, args := segInClause(oid, segs)
	// 只回显尚未通过审核的行；正常池由 ListVisibleBySegs 覆盖。
	query := "SELECT " + danmakuColumns + " FROM danmaku WHERE " + in +
		" AND mid = ? AND state IN (?, ?, ?) ORDER BY progress_ms ASC"
	args = append(args, mid, StatePending, StateFolded, StateRejected)

	var rows []*Danmaku
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("danmaku ListMineBySegs: %w", err)
	}
	return rows, nil
}

func (m *defaultDanmakuModel) TransitionState(ctx context.Context, dmid int64, fromState, toState, toPool int32) (bool, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE danmaku SET state = ?, pool = ?, mtime = ? WHERE dmid = ? AND state = ?",
		toState, toPool, nowUnix(), dmid, fromState)
	if err != nil {
		return false, fmt.Errorf("danmaku TransitionState: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("danmaku TransitionState RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultDanmakuModel) TransitionStateTx(ctx context.Context, session sqlx.Session, dmid int64, fromState, toState, toPool int32) (bool, error) {
	const query = "UPDATE danmaku SET state = ?, pool = ?, mtime = ? WHERE dmid = ? AND state = ?"
	var (
		res sql.Result
		err error
	)
	if session != nil {
		res, err = session.ExecCtx(ctx, query, toState, toPool, nowUnix(), dmid, fromState)
	} else {
		res, err = m.conn.ExecCtx(ctx, query, toState, toPool, nowUnix(), dmid, fromState)
	}
	if err != nil {
		return false, fmt.Errorf("danmaku TransitionStateTx: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("danmaku TransitionStateTx RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultDanmakuModel) SetModerationTaskID(ctx context.Context, dmid, taskID int64) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE danmaku SET moderation_task_id = ?, mtime = ? WHERE dmid = ? AND moderation_task_id = 0",
		taskID, nowUnix(), dmid)
	if err != nil {
		return fmt.Errorf("danmaku SetModerationTaskID: %w", err)
	}
	return nil
}

func (m *defaultDanmakuModel) SetModerationTaskIDTx(ctx context.Context, session sqlx.Session, dmid, taskID int64) error {
	if session == nil {
		return m.SetModerationTaskID(ctx, dmid, taskID)
	}
	_, err := session.ExecCtx(ctx,
		"UPDATE danmaku SET moderation_task_id = ?, mtime = ? WHERE dmid = ? AND moderation_task_id = 0",
		taskID, nowUnix(), dmid)
	if err != nil {
		return fmt.Errorf("danmaku SetModerationTaskIDTx: %w", err)
	}
	return nil
}

func (m *defaultDanmakuModel) CountVisibleByOid(ctx context.Context, oid int64) (int64, error) {
	var cnt int64
	err := m.conn.QueryRowCtx(ctx, &cnt,
		"SELECT COUNT(*) FROM danmaku WHERE oid = ? AND state = ? AND pool = ?",
		oid, StateNormal, PoolNormal)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("danmaku CountVisibleByOid: %w", err)
	}
	return cnt, nil
}

// segInClause 构造 `oid = ? AND seg_no IN (?,?,...)` 片段与参数，
// 避免拼接分段号造成 SQL 注入。
func segInClause(oid int64, segs []int32) (string, []interface{}) {
	build := strings.Builder{}
	build.WriteString("oid = ? AND seg_no IN (")
	args := make([]interface{}, 0, len(segs)+1)
	args = append(args, oid)
	for i, s := range segs {
		if i > 0 {
			build.WriteString(",")
		}
		build.WriteString("?")
		args = append(args, s)
	}
	build.WriteString(")")
	return build.String(), args
}
