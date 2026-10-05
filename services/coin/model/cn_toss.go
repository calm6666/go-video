package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 投币状态常量，与 cn_toss.state 列和 rpc.TossState 取值严格一致，禁止重排。
const (
	// TossStateActive 生效中：这些硬币正占用着 target 的投币数与用户的日额度。
	TossStateActive int32 = 1
	// TossStateCancelled 已取消：硬币已退回余额，保留行作为审计证据（AGENTS.md §8），
	// 不做物理删除；重新投币会在同一行上复活（ReactivateTx），(mid, target_aid) 唯一键不变。
	TossStateCancelled int32 = 2
)

// tossColumns 与 000001_create_coin_tables.sql 的 cn_toss 逐列对应。
// 注意 count 是 MySQL 函数名，SQL 里凡出现本列一律写反引号 `count`。
const tossColumns = "id, mid, target_aid, `count`, state, first_tossed_at, last_tossed_at," +
	" cancelled_at, last_request_id, platform, trace_id, last_toss_date, ctime, mtime"

// Toss 单用户对单条内容的投币聚合行（cn_toss 投影，对应 rpc.TossInfo）。
//
// 一行 = 「这个人对这条稿子当前投了几枚」，唯一键 (mid, target_aid)。
// 这不是投影表而是事实表：coin_count（某片收到多少币）由本表 state=ACTIVE 的
// `count` 求和得出，不再另建汇总表，避免多一份会漂移的事实源。
type Toss struct {
	ID            int64  `db:"id"`              // 投币记录 ID（rpc.TossInfo.toss_id）
	Mid           int64  `db:"mid"`             // 投币人
	TargetAid     int64  `db:"target_aid"`      // 被投稿件 aid（只存引用，不校验存在性，见 README 缺口）
	Count         int32  `db:"count"`           // 当前生效投币枚数；state=2 时保留历史值作为退款依据
	State         int32  `db:"state"`           // 见 TossStateActive/TossStateCancelled
	FirstTossedAt int64  `db:"first_tossed_at"` // 该行首次投币时间（Unix 秒，取消后重投不回拨）
	LastTossedAt  int64  `db:"last_tossed_at"`  // 最后一次投币时间（取消窗口的起算点）
	CancelledAt   int64  `db:"cancelled_at"`    // 取消时间，0 表示未取消
	LastRequestID string `db:"last_request_id"` // 最后一次「投币」请求的幂等键（取消不改本列）
	Platform      int32  `db:"platform"`        // 客户端平台，rpc.Platform 原值
	TraceID       string `db:"trace_id"`        // 链路追踪 ID，不含 PII
	LastTossDate  int32  `db:"last_toss_date"`  // 最后一次投币落的日桶，取消时按它回退当日计数
	Ctime         int64  `db:"ctime"`           // 创建时间（Unix 秒）
	Mtime         int64  `db:"mtime"`           // 修改时间（Unix 秒）
}

// TargetAgg 单内容投币聚合结果（rpc.TargetCoinSummary 的数据源）。
type TargetAgg struct {
	Aid           int64 `db:"aid"`
	CoinCount     int64 `db:"coin_count"`      // SUM(`count`) WHERE state=ACTIVE
	CoinUserCount int64 `db:"coin_user_count"` // COUNT(*)：(mid,target_aid) 唯一，一行即一人
}

// TossModel cn_toss 表读写接口。
type TossModel interface {
	// FindOne 按 (mid, target_aid) 读；无记录返回 (nil, nil)。
	FindOne(ctx context.Context, mid, targetAid int64) (*Toss, error)
	// LockByTargetTx 事务内按唯一键加行锁读取；无记录返回 (nil, nil)。
	LockByTargetTx(ctx context.Context, session sqlx.Session, mid, targetAid int64) (*Toss, error)
	// InsertTx 新建投币记录（首次投某内容），返回 toss_id。
	// (mid, target_aid) 唯一键冲突会直接上抛——同一 mid 的写已被 cn_account 行锁串行化，
	// 真撞上了说明有绕过账户锁的写入路径，属于必须暴露的 bug，不可以「当作已存在」吞掉。
	InsertTx(ctx context.Context, session sqlx.Session, t *Toss) (int64, error)
	// AccumulateTx 在 ACTIVE 行上累加，条件 state=ACTIVE AND `count` + delta <= perTargetLimit。
	// 返回 false 即「单片累计超上限」（转 reason=TARGET_LIMIT）。
	AccumulateTx(ctx context.Context, session sqlx.Session, id int64, delta, perTargetLimit int32, now int64, date int32, requestID string, platform int32, traceID string) (bool, error)
	// ReactivateTx 取消过的行重新投币：state 回到 ACTIVE，`count` 被本次枚数覆盖（不是累加，
	// 因为上一次的枚数已经全额退回，把退回过的币再叠加进去会凭空放大 coin_count）。
	// 条件 state=CANCELLED，返回 false 表示并发下状态已变。
	ReactivateTx(ctx context.Context, session sqlx.Session, id int64, count int32, now int64, date int32, requestID string, platform int32, traceID string) (bool, error)
	// CancelTx 置为 CANCELLED 并记 cancelled_at，条件 state=ACTIVE 保证只生效一次；
	// 不改 last_request_id（那列记录的是「投币」请求，取消的幂等锚点在 cn_flow）。
	CancelTx(ctx context.Context, session sqlx.Session, id, now int64) (bool, error)
	// ListByMid 我的投币记录；state=0 表示不限状态，按最后投币时间倒序。
	ListByMid(ctx context.Context, mid int64, state int32, offset int64, limit int) ([]*Toss, error)
	// CountByMid 与 ListByMid 同一过滤条件的总数。
	CountByMid(ctx context.Context, mid int64, state int32) (int64, error)
	// ListActiveByTarget 某内容当前投币人，按最后投币时间倒序（只列 ACTIVE）。
	ListActiveByTarget(ctx context.Context, targetAid int64, offset int64, limit int) ([]*Toss, error)
	// CountActiveByTarget 某内容当前投币人数。
	CountActiveByTarget(ctx context.Context, targetAid int64) (int64, error)
	// SummarizeTargets 批量聚合若干内容的投币数与投币人数；
	// 没有 ACTIVE 投币记录的 aid 不会出现在结果里，由调用方补零行（不能靠缺键猜 0）。
	SummarizeTargets(ctx context.Context, aids []int64) (map[int64]TargetAgg, error)
}

type defaultTossModel struct {
	conn sqlx.SqlConn
}

// NewTossModel 构造 cn_toss 的 sqlx 实现。
func NewTossModel(conn sqlx.SqlConn) TossModel {
	return &defaultTossModel{conn: conn}
}

const tossSelect = "SELECT " + tossColumns + " FROM cn_toss"

func (m *defaultTossModel) FindOne(ctx context.Context, mid, targetAid int64) (*Toss, error) {
	var row Toss
	query := tossSelect + " WHERE mid = ? AND target_aid = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, mid, targetAid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cn_toss FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultTossModel) LockByTargetTx(ctx context.Context, session sqlx.Session, mid, targetAid int64) (*Toss, error) {
	if session == nil {
		return nil, errors.New("coin: LockByTargetTx requires a transaction session")
	}
	var row Toss
	query := tossSelect + " WHERE mid = ? AND target_aid = ? LIMIT 1 FOR UPDATE"
	if err := session.QueryRowCtx(ctx, &row, query, mid, targetAid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cn_toss LockByTargetTx: %w", err)
	}
	return &row, nil
}

func (m *defaultTossModel) InsertTx(ctx context.Context, session sqlx.Session, t *Toss) (int64, error) {
	if t.Mid <= 0 {
		return 0, ErrInvalidMid
	}
	if t.TargetAid <= 0 {
		return 0, ErrInvalidTargetAid
	}
	if t.Count <= 0 {
		return 0, ErrInvalidTossCount
	}
	if t.State == 0 {
		t.State = TossStateActive
	}
	if t.Ctime == 0 {
		t.Ctime = nowUnix()
	}
	if t.Mtime == 0 {
		t.Mtime = t.Ctime
	}
	if t.FirstTossedAt == 0 {
		t.FirstTossedAt = t.Ctime
	}
	if t.LastTossedAt == 0 {
		t.LastTossedAt = t.FirstTossedAt
	}
	res, err := executor(session, m.conn).ExecCtx(ctx,
		"INSERT INTO cn_toss (mid, target_aid, `count`, state, first_tossed_at, last_tossed_at,"+
			" cancelled_at, last_request_id, platform, trace_id, last_toss_date, ctime, mtime)"+
			" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		t.Mid, t.TargetAid, t.Count, t.State, t.FirstTossedAt, t.LastTossedAt,
		t.CancelledAt, t.LastRequestID, t.Platform, t.TraceID, t.LastTossDate, t.Ctime, t.Mtime)
	if err != nil {
		return 0, fmt.Errorf("cn_toss InsertTx: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("cn_toss InsertTx LastInsertId: %w", err)
	}
	t.ID = id
	return id, nil
}

func (m *defaultTossModel) AccumulateTx(ctx context.Context, session sqlx.Session, id int64, delta, perTargetLimit int32, now int64, date int32, requestID string, platform int32, traceID string) (bool, error) {
	if delta <= 0 {
		return false, ErrInvalidTossCount
	}
	res, err := executor(session, m.conn).ExecCtx(ctx,
		"UPDATE cn_toss SET `count` = `count` + ?, last_tossed_at = ?, last_toss_date = ?,"+
			" last_request_id = ?, platform = ?, trace_id = ?, mtime = ?"+
			" WHERE id = ? AND state = ? AND `count` + ? <= ?",
		delta, now, date, requestID, platform, traceID, now, id, TossStateActive, delta, perTargetLimit)
	if err != nil {
		return false, fmt.Errorf("cn_toss AccumulateTx: %w", err)
	}
	return rowsAffectedOne(res, "cn_toss AccumulateTx")
}

func (m *defaultTossModel) ReactivateTx(ctx context.Context, session sqlx.Session, id int64, count int32, now int64, date int32, requestID string, platform int32, traceID string) (bool, error) {
	if count <= 0 {
		return false, ErrInvalidTossCount
	}
	res, err := executor(session, m.conn).ExecCtx(ctx,
		"UPDATE cn_toss SET state = ?, `count` = ?, last_tossed_at = ?, last_toss_date = ?,"+
			" cancelled_at = 0, last_request_id = ?, platform = ?, trace_id = ?, mtime = ?"+
			" WHERE id = ? AND state = ?",
		TossStateActive, count, now, date, requestID, platform, traceID, now, id, TossStateCancelled)
	if err != nil {
		return false, fmt.Errorf("cn_toss ReactivateTx: %w", err)
	}
	return rowsAffectedOne(res, "cn_toss ReactivateTx")
}

func (m *defaultTossModel) CancelTx(ctx context.Context, session sqlx.Session, id, now int64) (bool, error) {
	res, err := executor(session, m.conn).ExecCtx(ctx,
		"UPDATE cn_toss SET state = ?, cancelled_at = ?, mtime = ? WHERE id = ? AND state = ?",
		TossStateCancelled, now, now, id, TossStateActive)
	if err != nil {
		return false, fmt.Errorf("cn_toss CancelTx: %w", err)
	}
	return rowsAffectedOne(res, "cn_toss CancelTx")
}

func (m *defaultTossModel) ListByMid(ctx context.Context, mid int64, state int32, offset int64, limit int) ([]*Toss, error) {
	query := tossSelect + " WHERE mid = ?"
	args := []any{mid}
	if state != 0 {
		query += " AND state = ?"
		args = append(args, state)
	}
	query += " ORDER BY last_tossed_at DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	return queryTosses(ctx, m.conn, "cn_toss ListByMid", query, args...)
}

func (m *defaultTossModel) CountByMid(ctx context.Context, mid int64, state int32) (int64, error) {
	query := "SELECT COUNT(*) FROM cn_toss WHERE mid = ?"
	args := []any{mid}
	if state != 0 {
		query += " AND state = ?"
		args = append(args, state)
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, query, args...); err != nil {
		return 0, fmt.Errorf("cn_toss CountByMid: %w", err)
	}
	return total, nil
}

func (m *defaultTossModel) ListActiveByTarget(ctx context.Context, targetAid int64, offset int64, limit int) ([]*Toss, error) {
	query := tossSelect + " WHERE target_aid = ? AND state = ? ORDER BY last_tossed_at DESC, id DESC LIMIT ? OFFSET ?"
	return queryTosses(ctx, m.conn, "cn_toss ListActiveByTarget", query, targetAid, TossStateActive, limit, offset)
}

func (m *defaultTossModel) CountActiveByTarget(ctx context.Context, targetAid int64) (int64, error) {
	var total int64
	err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM cn_toss WHERE target_aid = ? AND state = ?", targetAid, TossStateActive)
	if err != nil {
		return 0, fmt.Errorf("cn_toss CountActiveByTarget: %w", err)
	}
	return total, nil
}

func (m *defaultTossModel) SummarizeTargets(ctx context.Context, aids []int64) (map[int64]TargetAgg, error) {
	out := make(map[int64]TargetAgg, len(aids))
	if len(aids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(aids)+1)
	args = append(args, TossStateActive)
	query := "SELECT target_aid AS aid, SUM(`count`) AS coin_count, COUNT(*) AS coin_user_count" +
		" FROM cn_toss WHERE state = ? AND target_aid IN (" + placeholders(len(aids)) + ") GROUP BY target_aid"
	for _, aid := range aids {
		args = append(args, aid)
	}
	var rows []TargetAgg
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return nil, fmt.Errorf("cn_toss SummarizeTargets: %w", err)
	}
	for _, r := range rows {
		out[r.Aid] = r
	}
	return out, nil
}

// queryTosses 统一处理「列表查询 + 空结果归一」：
// 无行时必须返回长度 0 的非 nil 切片，logic 才能给出 `[]` 而不是 null，
// 客户端列表页不必再判空指针。查询失败原样上抛，绝不折叠成空列表。
func queryTosses(ctx context.Context, conn sqlx.SqlConn, op, query string, args ...any) ([]*Toss, error) {
	var rows []*Toss
	if err := conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return []*Toss{}, nil
		}
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if rows == nil {
		rows = []*Toss{}
	}
	return rows, nil
}
