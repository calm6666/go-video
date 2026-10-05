package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RecallPoolVersion 池版本登记（recall_pool_version 表）。
//
// 这是"候选可追溯到生成批次"的载体：一行 = 一个 (source, pool_key, version) 快照，
// 记录产出方（generator）、批次号（batch_id）、条数与校验状态。
// 在线读只允许读 CURRENT 版本；BUILDING 版本即使已写入部分条目也不可见（与 AGENTS.md §8
// 同一思路：写入完成不代表可用）。
type RecallPoolVersion struct {
	ID            int64  `db:"id"`             // 自增主键
	Source        int32  `db:"source"`         // 召回路
	PoolKey       string `db:"pool_key"`       // 池键
	Version       int64  `db:"version"`        // 版本号（同池单调递增）
	BatchID       string `db:"batch_id"`       // 生成批次 ID
	Generator     string `db:"generator"`      // 产出方（cron job / 离线作业标识）
	SchemaVersion int32  `db:"schema_version"` // 条目结构版本
	ItemCount     int64  `db:"item_count"`     // 该版本条目数（发布前与 COUNT(*) 核对）
	State         int32  `db:"state"`          // 参见 VersionState* 常量
	PublishedAt   int64  `db:"published_at"`   // 生效时间（Unix 秒，0 表示未上线）
	Operator      string `db:"operator"`       // 最后操作者（作业/运营标识）
	Note          string `db:"note"`           // 变更说明（审计）
	Ctime         int64  `db:"ctime"`          // 创建时间（Unix 秒）
	Mtime         int64  `db:"mtime"`          // 修改时间（Unix 秒）
}

// RecallPoolVersionModel recall_pool_version 表读写接口。
//
// 变更类方法带 session 参数（nil 表示自成一个语句执行）：发布/回滚必须在同一事务里
// 推进版本状态并切 recall_pool_current 指针，否则会出现"版本已 CURRENT、指针还是旧的"
// 这类在线不可见但审计已污染的撕裂状态（AGENTS.md §5/§8）。
type RecallPoolVersionModel interface {
	// Register 幂等登记版本行（uniq_pool_version + ON DUPLICATE KEY UPDATE）。
	// 已被别的 batch_id 占用的版本不允许复用，返回 ErrVersionReuseBlocked，防止两个作业写同一版本。
	Register(ctx context.Context, session sqlx.Session, v *RecallPoolVersion) error
	// FindOne 按 (source, pool_key, version) 查询；不存在返回 ErrVersionNotFound。
	FindOne(ctx context.Context, source int32, poolKey string, version int64) (*RecallPoolVersion, error)
	// FindForUpdate 在调用方事务内以行锁读取版本行（SELECT ... FOR UPDATE）。
	// 发布/回滚读取目标版本时必须用它：Register/UpdateItemCount 等写入与本事务并发时，
	// 普通一致性读会给出过期的 state/item_count，导致"把 BUILDING 版本当成 READY 上线"。
	// session 为 nil 直接返回 ErrSwitchConflict（与 RecallPoolCurrentModel.Switch 同一口径）。
	FindForUpdate(ctx context.Context, session sqlx.Session, source int32, poolKey string,
		version int64) (*RecallPoolVersion, error)
	// FindCurrent 按 (source, pool_key) 读取处于 CURRENT 状态的版本行；
	// 没有则返回 ErrPoolNotFound（该池从未上线，在线侧按 pool_not_ready 降级）。
	// 用于发布时把旧 CURRENT 置为 RETIRED。
	FindCurrent(ctx context.Context, source int32, poolKey string) (*RecallPoolVersion, error)
	// FindCurrentForUpdate 事务内行锁读取 state=CURRENT 的版本行，供发布流程退役旧版本。
	// 与 FindCurrent 的差别：本方法在"没有 CURRENT 行"时返回 (nil, nil) 而不是 ErrPoolNotFound ——
	// 首次上线时没有旧版本可退役是正常分支，不该借一个"池不存在"的错误码表达它。
	FindCurrentForUpdate(ctx context.Context, session sqlx.Session, source int32,
		poolKey string) (*RecallPoolVersion, error)
	// FindByID 按自增 id 读取版本行（审计/回放路径，指针表只存 version，回溯登记行要按 id）。
	FindByID(ctx context.Context, id int64) (*RecallPoolVersion, error)
	// FindByBatch 按批次号查询该批次产出的版本（回放/排障入口）。
	// 调用方拿 (version, batch_id) 组合核对时，若命中的是另一版本需按 ErrBatchMismatch 处理。
	FindByBatch(ctx context.Context, source int32, poolKey, batchID string) (*RecallPoolVersion, error)
	// ListByPool 列出某池的版本（按 version DESC，limit 必须落在 (0, MaxVersionListLimit]）。
	// includeRetired=false 时过滤掉 RETIRED，用于"还能回滚到哪些版本"的判定。
	ListByPool(ctx context.Context, source int32, poolKey string, includeRetired bool, limit int) ([]*RecallPoolVersion, error)
	// ListByRefs 批量读取多个 (source, pool_key, version) 三元组（GetRecallConfig 给 ready_pools
	// 补 item_count 用，避免逐池 N+1）。refs 数量上限 MaxPoolRefsPerQuery。
	ListByRefs(ctx context.Context, refs []PoolVersionRef, limit int) ([]*RecallPoolVersion, error)
	// MaxVersion 返回某池已登记的最大版本号（供作业生成下一个版本；无登记返回 0）。
	MaxVersion(ctx context.Context, source int32, poolKey string) (int64, error)
	// UpdateState 条件更新状态：只有当前状态在 fromStates 内才生效，返回是否更新成功（RowsAffected 判定）。
	// 用于 BUILDING -> READY -> CURRENT、旧 CURRENT -> RETIRED 的推进，避免并发切换互相覆盖。
	// 返回 false 表示状态已被别人推进，调用方必须重读后决策，不能当成成功。
	UpdateState(ctx context.Context, session sqlx.Session, id int64, toState int32, fromStates []int32, operator, note string) (bool, error)
	// SetPublishedAt 记录版本生效时间并置 CURRENT 状态：只有当前状态是 READY/RETIRED 才生效，
	// 返回是否更新成功（与 RecallPoolCurrent.Switch 同事务调用）。
	SetPublishedAt(ctx context.Context, session sqlx.Session, id, publishedAt int64, operator string) (bool, error)
	// UpdateItemCount 刷新版本条数（每批写入后由调用方以 COUNT(*) 回填）。
	UpdateItemCount(ctx context.Context, session sqlx.Session, id, itemCount int64) error
	// PrunableBefore 返回清理水位线：按 version DESC 排序后第 keepVersions 个版本号；
	// 已登记版本数不足 keepVersions 时返回 0（表示没有可清理版本）。
	// 水位线以下且状态为 RETIRED/FAILED 的版本才可删（CURRENT/READY/BUILDING 永不进入）。
	PrunableBefore(ctx context.Context, source int32, poolKey string, keepVersions int32) (int64, error)
	// ListPrunable 列出 version 低于水位线且已退役/失败的版本行（分批，limit 限幅）。
	ListPrunable(ctx context.Context, source int32, poolKey string, belowVersion int64, limit int) ([]*RecallPoolVersion, error)
	// Delete 按 ID 删除版本登记行（前置条件：状态是 RETIRED/FAILED；条目行由 DeleteByVersions 分批删除）。
	Delete(ctx context.Context, session sqlx.Session, ids []int64) (int64, error)
}

// PoolVersionRef 是一个版本行的寻址三元组（供 ListByRefs 批量取数）。
type PoolVersionRef struct {
	Source  int32
	PoolKey string
	Version int64
}

type defaultRecallPoolVersionModel struct {
	conn sqlx.SqlConn
}

// NewRecallPoolVersionModel 创建 RecallPoolVersionModel 实现。
func NewRecallPoolVersionModel(conn sqlx.SqlConn) RecallPoolVersionModel {
	return &defaultRecallPoolVersionModel{conn: conn}
}

// exec 在事务 session 与全局连接之间选择执行器（与 audit 模型同一约定）。
func (m *defaultRecallPoolVersionModel) exec(session sqlx.Session) sqlx.Session {
	if session != nil {
		return session
	}
	return m.conn
}

// poolVersionSelect 是 recall_pool_version 的列清单，与迁移文件列定义保持一致。
const poolVersionSelect = "SELECT id, source, pool_key, version, batch_id, generator, schema_version, " +
	"item_count, state, published_at, operator, note, ctime, mtime FROM recall_pool_version"

func (m *defaultRecallPoolVersionModel) Register(ctx context.Context, session sqlx.Session, v *RecallPoolVersion) error {
	if !ValidSource(v.Source) {
		return ErrInvalidSource
	}
	if err := ValidatePoolKey(v.Source, v.PoolKey); err != nil {
		return err
	}
	if v.Version <= 0 {
		return ErrInvalidVersion
	}
	if strings.TrimSpace(v.BatchID) == "" {
		return ErrBatchIDRequired
	}
	if v.State == 0 {
		v.State = VersionStateBuilding
	}
	if !ValidVersionState(v.State) {
		return ErrInvalidVersionState
	}
	if v.SchemaVersion == 0 {
		v.SchemaVersion = PoolVersionSchemaVersion
	}
	now := nowUnix()
	// 幂等登记：同 (source, pool_key, version) 重复注册只刷新可变元数据，
	// 不覆盖已登记的 batch_id 与 state（批次归属是本表的核心不变量）。
	if _, err := m.exec(session).ExecCtx(ctx,
		"INSERT INTO recall_pool_version (source, pool_key, version, batch_id, generator, schema_version, "+
			"item_count, state, published_at, operator, note, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE generator = VALUES(generator), schema_version = VALUES(schema_version), "+
			"operator = VALUES(operator), note = VALUES(note), mtime = VALUES(mtime)",
		v.Source, v.PoolKey, v.Version, v.BatchID, v.Generator, v.SchemaVersion,
		v.ItemCount, v.State, v.PublishedAt, v.Operator, v.Note, now, now); err != nil {
		return fmt.Errorf("recall_pool_version Register: %w", err)
	}
	exist, err := m.findOne(ctx, session, v.Source, v.PoolKey, v.Version)
	if err != nil {
		return err
	}
	if exist.BatchID != v.BatchID {
		// 版本号已被另一个批次占用：这是"两个作业写同一版本"的串写，必须硬失败。
		return ErrVersionReuseBlocked
	}
	return nil
}

func (m *defaultRecallPoolVersionModel) FindOne(ctx context.Context, source int32, poolKey string, version int64) (*RecallPoolVersion, error) {
	return m.findOne(ctx, nil, source, poolKey, version)
}

// findOne 是共用查询体：session!=nil 时在调用方事务内读，保证与同事务的写看到同一快照。
func (m *defaultRecallPoolVersionModel) findOne(ctx context.Context, session sqlx.Session,
	source int32, poolKey string, version int64) (*RecallPoolVersion, error) {
	if !ValidSource(source) {
		return nil, ErrInvalidSource
	}
	if err := ValidatePoolKey(source, poolKey); err != nil {
		return nil, err
	}
	if version <= 0 {
		return nil, ErrInvalidVersion
	}
	var row RecallPoolVersion
	query := poolVersionSelect + " WHERE source = ? AND pool_key = ? AND version = ? LIMIT 1"
	err := m.exec(session).QueryRowCtx(ctx, &row, query, source, poolKey, version)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrVersionNotFound
		}
		return nil, fmt.Errorf("recall_pool_version FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultRecallPoolVersionModel) FindCurrent(ctx context.Context, source int32, poolKey string) (*RecallPoolVersion, error) {
	if !ValidSource(source) {
		return nil, ErrInvalidSource
	}
	if err := ValidatePoolKey(source, poolKey); err != nil {
		return nil, err
	}
	var row RecallPoolVersion
	query := poolVersionSelect + " WHERE source = ? AND pool_key = ? AND state = ? ORDER BY version DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, source, poolKey, VersionStateCurrent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPoolNotFound
		}
		return nil, fmt.Errorf("recall_pool_version FindCurrent: %w", err)
	}
	return &row, nil
}

func (m *defaultRecallPoolVersionModel) FindForUpdate(ctx context.Context, session sqlx.Session,
	source int32, poolKey string, version int64) (*RecallPoolVersion, error) {
	if session == nil {
		return nil, fmt.Errorf("recall_pool_version FindForUpdate: %w", ErrSwitchConflict)
	}
	if !ValidSource(source) {
		return nil, ErrInvalidSource
	}
	if err := ValidatePoolKey(source, poolKey); err != nil {
		return nil, err
	}
	if version <= 0 {
		return nil, ErrInvalidVersion
	}
	var row RecallPoolVersion
	query := poolVersionSelect + " WHERE source = ? AND pool_key = ? AND version = ? LIMIT 1 FOR UPDATE"
	if err := session.QueryRowCtx(ctx, &row, query, source, poolKey, version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrVersionNotFound
		}
		return nil, fmt.Errorf("recall_pool_version FindForUpdate: %w", err)
	}
	return &row, nil
}

func (m *defaultRecallPoolVersionModel) FindCurrentForUpdate(ctx context.Context, session sqlx.Session,
	source int32, poolKey string) (*RecallPoolVersion, error) {
	if session == nil {
		return nil, fmt.Errorf("recall_pool_version FindCurrentForUpdate: %w", ErrSwitchConflict)
	}
	if !ValidSource(source) {
		return nil, ErrInvalidSource
	}
	if err := ValidatePoolKey(source, poolKey); err != nil {
		return nil, err
	}
	var row RecallPoolVersion
	query := poolVersionSelect + " WHERE source = ? AND pool_key = ? AND state = ? ORDER BY version DESC LIMIT 1 FOR UPDATE"
	err := session.QueryRowCtx(ctx, &row, query, source, poolKey, VersionStateCurrent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 首次上线：没有旧 CURRENT 行需要退役，是正常分支。
			return nil, nil
		}
		return nil, fmt.Errorf("recall_pool_version FindCurrentForUpdate: %w", err)
	}
	return &row, nil
}

func (m *defaultRecallPoolVersionModel) FindByID(ctx context.Context, id int64) (*RecallPoolVersion, error) {
	if id <= 0 {
		return nil, ErrInvalidVersion
	}
	var row RecallPoolVersion
	query := poolVersionSelect + " WHERE id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrVersionNotFound
		}
		return nil, fmt.Errorf("recall_pool_version FindByID: %w", err)
	}
	return &row, nil
}

func (m *defaultRecallPoolVersionModel) FindByBatch(ctx context.Context, source int32, poolKey, batchID string) (*RecallPoolVersion, error) {
	if !ValidSource(source) {
		return nil, ErrInvalidSource
	}
	if err := ValidatePoolKey(source, poolKey); err != nil {
		return nil, err
	}
	if strings.TrimSpace(batchID) == "" {
		return nil, ErrBatchIDRequired
	}
	var row RecallPoolVersion
	// uniq_pool_version_batch 保证同一批次在同一池内至多一行，LIMIT 1 只是防御性下界。
	query := poolVersionSelect + " WHERE source = ? AND pool_key = ? AND batch_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, source, poolKey, batchID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrVersionNotFound
		}
		return nil, fmt.Errorf("recall_pool_version FindByBatch: %w", err)
	}
	return &row, nil
}

func (m *defaultRecallPoolVersionModel) ListByPool(ctx context.Context, source int32, poolKey string, includeRetired bool, limit int) ([]*RecallPoolVersion, error) {
	if !ValidSource(source) {
		return nil, ErrInvalidSource
	}
	if err := ValidatePoolKey(source, poolKey); err != nil {
		return nil, err
	}
	if err := CheckLimit(limit, MaxVersionListLimit); err != nil {
		return nil, err
	}
	query := poolVersionSelect + " WHERE source = ? AND pool_key = ?"
	args := []interface{}{source, poolKey}
	if !includeRetired {
		query += " AND state <> ?"
		args = append(args, VersionStateRetired)
	}
	query += " ORDER BY version DESC LIMIT ?"
	args = append(args, limit)
	var rows []*RecallPoolVersion
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("recall_pool_version ListByPool: %w", err)
	}
	return rows, nil
}

func (m *defaultRecallPoolVersionModel) ListByRefs(ctx context.Context, refs []PoolVersionRef, limit int) ([]*RecallPoolVersion, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if len(refs) > MaxPoolRefsPerQuery {
		return nil, ErrTooManyPools
	}
	if err := CheckLimit(limit, MaxPoolRefsPerQuery); err != nil {
		return nil, err
	}
	tuples := make([]string, 0, len(refs))
	args := make([]interface{}, 0, len(refs)*3)
	for _, r := range refs {
		if !ValidSource(r.Source) {
			return nil, ErrInvalidSource
		}
		if err := ValidatePoolKey(r.Source, r.PoolKey); err != nil {
			return nil, err
		}
		if r.Version <= 0 {
			return nil, ErrInvalidVersion
		}
		tuples = append(tuples, "(?,?,?)")
		args = append(args, r.Source, r.PoolKey, r.Version)
	}
	query := poolVersionSelect + " WHERE (source, pool_key, version) IN (" + strings.Join(tuples, ",") + ") LIMIT ?"
	args = append(args, limit)
	var rows []*RecallPoolVersion
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("recall_pool_version ListByRefs: %w", err)
	}
	return rows, nil
}

func (m *defaultRecallPoolVersionModel) MaxVersion(ctx context.Context, source int32, poolKey string) (int64, error) {
	if !ValidSource(source) {
		return 0, ErrInvalidSource
	}
	if err := ValidatePoolKey(source, poolKey); err != nil {
		return 0, err
	}
	var maxVersion sql.NullInt64
	query := "SELECT MAX(version) FROM recall_pool_version WHERE source = ? AND pool_key = ?"
	if err := m.conn.QueryRowCtx(ctx, &maxVersion, query, source, poolKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("recall_pool_version MaxVersion: %w", err)
	}
	return maxVersion.Int64, nil
}

func (m *defaultRecallPoolVersionModel) UpdateState(ctx context.Context, session sqlx.Session, id int64, toState int32,
	fromStates []int32, operator, note string) (bool, error) {
	if id <= 0 {
		return false, ErrInvalidVersion
	}
	if !ValidVersionState(toState) {
		return false, ErrInvalidVersionState
	}
	if len(fromStates) == 0 {
		return false, ErrInvalidVersionState
	}
	for _, s := range fromStates {
		if !ValidVersionState(s) {
			return false, ErrInvalidVersionState
		}
	}
	args := []interface{}{toState, nowUnix(), operator, note, id}
	query := "UPDATE recall_pool_version SET state = ?, mtime = ?, operator = ?, note = ? WHERE id = ? AND state IN (" +
		inPlaceholders(len(fromStates)) + ")"
	for _, s := range fromStates {
		args = append(args, s)
	}
	res, err := m.exec(session).ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("recall_pool_version UpdateState: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("recall_pool_version UpdateState RowsAffected: %w", err)
	}
	return affected > 0, nil
}

func (m *defaultRecallPoolVersionModel) SetPublishedAt(ctx context.Context, session sqlx.Session, id, publishedAt int64, operator string) (bool, error) {
	if id <= 0 {
		return false, ErrInvalidVersion
	}
	if publishedAt <= 0 {
		return false, ErrInvalidVersion
	}
	if strings.TrimSpace(operator) == "" {
		return false, ErrOperatorRequired
	}
	query := "UPDATE recall_pool_version SET state = ?, published_at = ?, operator = ?, mtime = ? WHERE id = ? AND state IN (" +
		inPlaceholders(2) + ")"
	args := []interface{}{VersionStateCurrent, publishedAt, operator, publishedAt, id, VersionStateReady, VersionStateRetired}
	res, err := m.exec(session).ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("recall_pool_version SetPublishedAt: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("recall_pool_version SetPublishedAt RowsAffected: %w", err)
	}
	return affected > 0, nil
}

func (m *defaultRecallPoolVersionModel) UpdateItemCount(ctx context.Context, session sqlx.Session, id, itemCount int64) error {
	if id <= 0 || itemCount < 0 {
		return ErrInvalidVersion
	}
	if _, err := m.exec(session).ExecCtx(ctx,
		"UPDATE recall_pool_version SET item_count = ?, mtime = ? WHERE id = ?", itemCount, nowUnix(), id); err != nil {
		return fmt.Errorf("recall_pool_version UpdateItemCount: %w", err)
	}
	return nil
}

func (m *defaultRecallPoolVersionModel) PrunableBefore(ctx context.Context, source int32, poolKey string, keepVersions int32) (int64, error) {
	if keepVersions < 1 {
		return 0, ErrKeepVersionsTooSmall
	}
	if !ValidSource(source) {
		return 0, ErrInvalidSource
	}
	if err := ValidatePoolKey(source, poolKey); err != nil {
		return 0, err
	}
	var version int64
	query := "SELECT version FROM recall_pool_version WHERE source = ? AND pool_key = ? ORDER BY version DESC LIMIT 1 OFFSET ?"
	err := m.conn.QueryRowCtx(ctx, &version, query, source, poolKey, keepVersions-1)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 登记版本数不足保留窗口，没有可清理内容。
			return 0, nil
		}
		return 0, fmt.Errorf("recall_pool_version PrunableBefore: %w", err)
	}
	return version, nil
}

func (m *defaultRecallPoolVersionModel) ListPrunable(ctx context.Context, source int32, poolKey string, belowVersion int64, limit int) ([]*RecallPoolVersion, error) {
	if !ValidSource(source) {
		return nil, ErrInvalidSource
	}
	if err := ValidatePoolKey(source, poolKey); err != nil {
		return nil, err
	}
	if belowVersion <= 0 {
		// 水位线为 0 表示"版本数还在保留窗口内"，没有可清理内容；
		// 绝不能把 0 当成"无下界"而列出全表。
		return nil, nil
	}
	if err := CheckLimit(limit, MaxVersionListLimit); err != nil {
		return nil, err
	}
	query := poolVersionSelect + " WHERE source = ? AND pool_key = ? AND version < ? AND state IN (?, ?) ORDER BY version ASC LIMIT ?"
	args := []interface{}{source, poolKey, belowVersion, VersionStateRetired, VersionStateFailed, limit}
	var rows []*RecallPoolVersion
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("recall_pool_version ListPrunable: %w", err)
	}
	return rows, nil
}

func (m *defaultRecallPoolVersionModel) Delete(ctx context.Context, session sqlx.Session, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if len(ids) > MaxVersionListLimit {
		return 0, ErrTooManyItems
	}
	// 只删已退役/失败版本：CURRENT/READY/BUILDING 行受状态条件保护，误传 ID 也不会被删。
	args := []interface{}{VersionStateRetired, VersionStateFailed}
	query := "DELETE FROM recall_pool_version WHERE state IN (?, ?) AND id IN (" + inPlaceholders(len(ids)) + ")"
	for _, id := range ids {
		args = append(args, id)
	}
	res, err := m.exec(session).ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("recall_pool_version Delete: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("recall_pool_version Delete RowsAffected: %w", err)
	}
	return deleted, nil
}

// inPlaceholders 生成 n 个逗号分隔的 "?"，用于显式展开 IN 列表。
func inPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
