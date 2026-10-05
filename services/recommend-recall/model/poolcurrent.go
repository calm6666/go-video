package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RecallPoolCurrent 池的当前生效版本指针（recall_pool_current 表）。
//
// 每个 (source, pool_key) 一行，主键即唯一约束：这是"一个池只有一个 CURRENT"的
// 结构性保证，不依赖 recall_pool_version.state 的软标记。切换与回滚都是本行的
// 条件 UPDATE（见 Switch 的 expect_version），因此在线读的可见性判定只查这一张表，
// 与批次写入互不干扰。
// 本表是控制位投影（可从 recall_pool_version 的 state=CURRENT 行重建），不是业务事实源；
// 但它是唯一决定"在线出哪一批候选"的地方，所以它的变更必须走 CAS + 审计。
type RecallPoolCurrent struct {
	Source          int32  `db:"source"`           // 召回路（联合主键之一）
	PoolKey         string `db:"pool_key"`         // 池键（联合主键之一）
	Version         int64  `db:"version"`          // 当前生效版本，0 表示该池从未上线
	BatchID         string `db:"batch_id"`         // 生效批次
	PreviousVersion int64  `db:"previous_version"` // 上一次生效版本（回滚排障）
	SwitchCount     int64  `db:"switch_count"`     // 累计切换次数
	Operator        string `db:"operator"`         // 最近一次切换操作者
	Note            string `db:"note"`             // 最近一次切换原因
	PublishedAt     int64  `db:"published_at"`     // 最近一次切换时间（Unix 秒）
	Ctime           int64  `db:"ctime"`            // 创建时间（Unix 秒）
	Mtime           int64  `db:"mtime"`            // 修改时间（Unix 秒）
}

// PoolSwitchOutcome 是一次指针切换的结果。
//
// 单独定义结构体而不是返回 bool，是因为 rpc.PublishPoolVersionReply 必须同时回带
// previous_version 与 current_version：调用方（cron 作业、运营回滚开关）要靠这两个值
// 判断"我看到的旧版本有没有被别人先切走"。
type PoolSwitchOutcome struct {
	// Switched 本次调用是否真的移动了指针（false 表示指针已在目标版本，属幂等重放）。
	Switched bool
	// Previous 本次切换前指针所在的版本；Switched=false 时等于 Current。
	Previous int64
	// Current 本次之后指针所在的版本。
	Current int64
	// SwitchCount 切换后的累计次数（0 表示未切换）。
	SwitchCount int64
}

// RecallPoolCurrentModel recall_pool_current 表读写接口。
type RecallPoolCurrentModel interface {
	// FindOne 查询某池的当前指针；不存在返回 ErrPoolNotFound（该池从未登记）。
	// 指针存在但 Version==0 表示"建了行还没上线"，调用方按 pool_not_ready 降级处理。
	FindOne(ctx context.Context, source int32, poolKey string) (*RecallPoolCurrent, error)
	// FindOneForUpdate 在调用方事务内以行锁读取指针（SELECT ... FOR UPDATE）。
	//
	// 为什么发布/回滚要用它而不是 FindOne：FindOne 是普通一致性读，拿到的是本事务快照，
	// 并发下会读到一个已经被别人切走的版本，导致 Switch 的 CAS 必然落空、白报一次
	// ErrSwitchConflict。FOR UPDATE 读的是最新已提交值并且把行锁握到事务提交，
	// 于是同一池的两次上线被串行化：后者要么读到新指针并正常冲突，要么排队到前者提交。
	// session 为 nil 时直接报错 —— 没有事务的 FOR UPDATE 会长期持有行锁，比冲突更危险。
	FindOneForUpdate(ctx context.Context, session sqlx.Session, source int32, poolKey string) (*RecallPoolCurrent, error)
	// EnsureRow 幂等建指针行（version=0），供 Switch 的 CAS 有行可锁。
	// session 非 nil 时在调用方事务内执行（发布流程需要与版本状态推进同事务）。
	EnsureRow(ctx context.Context, session sqlx.Session, source int32, poolKey string) error
	// Switch 原子条件切换版本指针：UPDATE ... WHERE source=? AND pool_key=? AND version=expectVersion。
	//
	// 语义（AGENTS.md §5「写接口要设计幂等键、状态版本或唯一约束」里的状态版本）：
	//   - CAS 命中（RowsAffected==1）：Switched=true，Previous=expectVersion；
	//   - CAS 未命中但重读发现指针已经等于 in.Version：Switched=false（并发对手切到了同一目标，
	//     属幂等重放，不重复计数）；
	//   - CAS 未命中且指针指向别的版本：返回 ErrSwitchConflict，绝不静默覆盖。
	//
	// expectVersion 传当前读到的 version；首次上线传 0（配合 EnsureRow 建的 version=0 行）。
	// 必须在调用方事务内使用（session != nil）才能保证与 recall_pool_version 状态一致。
	Switch(ctx context.Context, session sqlx.Session, in *RecallPoolCurrent, expectVersion int64) (*PoolSwitchOutcome, error)
	// ListBySources 批量读取多个池的指针（在线召回一次拿齐版本，避免逐池 round trip）。
	// pools 必须等长于召回路 x 池键组合，数量超过 MaxPoolRefsPerQuery 返回 ErrTooManyPools。
	ListBySources(ctx context.Context, pools []PoolKey) ([]*RecallPoolCurrent, error)
	// ListCurrent 列出已上线（version>0）的指针，按 published_at DESC 取前 limit 条
	// （GetRecallConfig 的 ready_pools 摘要）。limit 必须落在 (0, MaxPoolRefsPerQuery]。
	ListCurrent(ctx context.Context, limit int) ([]*RecallPoolCurrent, error)
}

// 本模型有意不提供 RetireStale / Delete 之类的批量改指针方法：
// 指针是"在线出哪批候选"的唯一开关，批量改一次就能让整路召回掉线；
// 历史版本条目的回收走 RecallPoolVersionModel + RecallPoolModel.DeleteByVersions，
// 与指针解耦。

// PoolKey 是一个池的寻址二元组（source + pool_key）。
type PoolKey struct {
	Source  int32
	PoolKey string
}

type defaultRecallPoolCurrentModel struct {
	conn sqlx.SqlConn
}

// NewRecallPoolCurrentModel 创建 RecallPoolCurrentModel 实现。
func NewRecallPoolCurrentModel(conn sqlx.SqlConn) RecallPoolCurrentModel {
	return &defaultRecallPoolCurrentModel{conn: conn}
}

const poolCurrentSelect = "SELECT source, pool_key, version, batch_id, previous_version, switch_count, " +
	"operator, note, published_at, ctime, mtime FROM recall_pool_current"

// exec 在事务 session 与全局连接之间选择执行器（与 audit 模型同一约定）。
// session==nil 时退回 conn：只读/无并发保证的路径才允许这样调用。
func (m *defaultRecallPoolCurrentModel) exec(session sqlx.Session) sqlx.Session {
	if session != nil {
		return session
	}
	return m.conn
}

func (m *defaultRecallPoolCurrentModel) FindOne(ctx context.Context, source int32, poolKey string) (*RecallPoolCurrent, error) {
	if !ValidSource(source) {
		return nil, ErrInvalidSource
	}
	if err := ValidatePoolKey(source, poolKey); err != nil {
		return nil, err
	}
	var row RecallPoolCurrent
	query := poolCurrentSelect + " WHERE source = ? AND pool_key = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, source, poolKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPoolNotFound
		}
		return nil, fmt.Errorf("recall_pool_current FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultRecallPoolCurrentModel) FindOneForUpdate(ctx context.Context, session sqlx.Session,
	source int32, poolKey string) (*RecallPoolCurrent, error) {
	if session == nil {
		return nil, fmt.Errorf("recall_pool_current FindOneForUpdate: %w",
			ErrSwitchConflict) // 与 Switch 同一口径：脱离事务的指针操作一律拒绝
	}
	if !ValidSource(source) {
		return nil, ErrInvalidSource
	}
	if err := ValidatePoolKey(source, poolKey); err != nil {
		return nil, err
	}
	var row RecallPoolCurrent
	query := poolCurrentSelect + " WHERE source = ? AND pool_key = ? LIMIT 1 FOR UPDATE"
	if err := session.QueryRowCtx(ctx, &row, query, source, poolKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPoolNotFound
		}
		return nil, fmt.Errorf("recall_pool_current FindOneForUpdate: %w", err)
	}
	return &row, nil
}

func (m *defaultRecallPoolCurrentModel) EnsureRow(ctx context.Context, session sqlx.Session, source int32, poolKey string) error {
	if !ValidSource(source) {
		return ErrInvalidSource
	}
	if err := ValidatePoolKey(source, poolKey); err != nil {
		return err
	}
	now := nowUnix()
	// ON DUPLICATE KEY UPDATE pool_key = pool_key 是显式 no-op：
	// 目的只是让并发首次上线变成"谁插入都行，结果同一行"。
	_, err := m.exec(session).ExecCtx(ctx,
		"INSERT INTO recall_pool_current (source, pool_key, version, batch_id, previous_version, switch_count, "+
			"operator, note, published_at, ctime, mtime) VALUES (?, ?, 0, '', 0, 0, '', '', 0, ?, ?) "+
			"ON DUPLICATE KEY UPDATE pool_key = pool_key",
		source, poolKey, now, now)
	if err != nil {
		return fmt.Errorf("recall_pool_current EnsureRow: %w", err)
	}
	return nil
}

func (m *defaultRecallPoolCurrentModel) Switch(ctx context.Context, session sqlx.Session,
	in *RecallPoolCurrent, expectVersion int64) (*PoolSwitchOutcome, error) {
	if !ValidSource(in.Source) {
		return nil, ErrInvalidSource
	}
	if err := ValidatePoolKey(in.Source, in.PoolKey); err != nil {
		return nil, err
	}
	if in.Version <= 0 {
		return nil, ErrInvalidVersion
	}
	if expectVersion < 0 {
		return nil, ErrInvalidVersion
	}
	if session == nil {
		// 脱离事务的 CAS 仍然能挡住"读后写"窗口，但挡不住与 recall_pool_version 状态推进的
		// 撕裂（版本已 CURRENT、指针还是旧的）。发布/回滚必须同事务，这里宁可报错。
		return nil, fmt.Errorf("recall_pool_current Switch: %w", ErrSwitchConflict)
	}
	if strings.TrimSpace(in.Operator) == "" {
		return nil, ErrOperatorRequired
	}
	if strings.TrimSpace(in.Note) == "" {
		return nil, ErrReasonRequired
	}
	now := nowUnix()
	if in.PublishedAt == 0 {
		in.PublishedAt = now
	}
	// previous_version 与 switch_count 在 SQL 侧取旧值累加，不在 Go 里读改写。
	res, err := session.ExecCtx(ctx,
		"UPDATE recall_pool_current SET previous_version = version, version = ?, batch_id = ?, "+
			"switch_count = switch_count + 1, operator = ?, note = ?, published_at = ?, mtime = ? "+
			"WHERE source = ? AND pool_key = ? AND version = ?",
		in.Version, in.BatchID, in.Operator, in.Note, in.PublishedAt, now, in.Source, in.PoolKey, expectVersion)
	if err != nil {
		return nil, fmt.Errorf("recall_pool_current Switch: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("recall_pool_current Switch RowsAffected: %w", err)
	}
	if affected == 1 {
		return &PoolSwitchOutcome{Switched: true, Previous: expectVersion, Current: in.Version}, nil
	}
	// CAS 未命中：只有"指针已经在目标版本"才算幂等重放，其余一律冲突。
	// 这里必须用 session 读（同一事务快照），换 conn 读会看到事务外的中间态。
	cur, err := m.findBySession(ctx, session, in.Source, in.PoolKey)
	if err != nil {
		return nil, err
	}
	if cur.Version == in.Version {
		return &PoolSwitchOutcome{Switched: false, Previous: cur.Version, Current: cur.Version,
			SwitchCount: cur.SwitchCount}, nil
	}
	return nil, ErrSwitchConflict
}

func (m *defaultRecallPoolCurrentModel) ListBySources(ctx context.Context, pools []PoolKey) ([]*RecallPoolCurrent, error) {
	if len(pools) == 0 {
		return nil, nil
	}
	if len(pools) > MaxPoolRefsPerQuery {
		return nil, ErrTooManyPools
	}
	// 元组 IN 一次取回多个池的指针；池数量有限（召回路 x 分区），不构造动态 SQL 拼接值。
	tuples := make([]string, 0, len(pools))
	args := make([]interface{}, 0, len(pools)*2)
	for _, p := range pools {
		if !ValidSource(p.Source) {
			return nil, ErrInvalidSource
		}
		if err := ValidatePoolKey(p.Source, p.PoolKey); err != nil {
			return nil, err
		}
		tuples = append(tuples, "(?,?)")
		args = append(args, p.Source, p.PoolKey)
	}
	query := poolCurrentSelect + " WHERE (source, pool_key) IN (" + strings.Join(tuples, ",") + ") LIMIT ?"
	args = append(args, len(pools))
	var rows []*RecallPoolCurrent
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("recall_pool_current ListBySources: %w", err)
	}
	return rows, nil
}

func (m *defaultRecallPoolCurrentModel) ListCurrent(ctx context.Context, limit int) ([]*RecallPoolCurrent, error) {
	if err := CheckLimit(limit, MaxPoolRefsPerQuery); err != nil {
		return nil, err
	}
	// 只统计已上线池：version=0 的行是 EnsureRow 建的空壳，不算"可用"。
	query := poolCurrentSelect + " WHERE version > 0 ORDER BY published_at DESC, source ASC LIMIT ?"
	var rows []*RecallPoolCurrent
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("recall_pool_current ListCurrent: %w", err)
	}
	return rows, nil
}

// findBySession 在指定事务内读取指针（供 Switch 冲突判定使用）。
func (m *defaultRecallPoolCurrentModel) findBySession(ctx context.Context, session sqlx.Session,
	source int32, poolKey string) (*RecallPoolCurrent, error) {
	var row RecallPoolCurrent
	query := poolCurrentSelect + " WHERE source = ? AND pool_key = ? LIMIT 1"
	if err := session.QueryRowCtx(ctx, &row, query, source, poolKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPoolNotFound
		}
		return nil, fmt.Errorf("recall_pool_current findBySession: %w", err)
	}
	return &row, nil
}
