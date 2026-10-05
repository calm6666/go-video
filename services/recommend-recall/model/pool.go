package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RecallPool 池条目（recall_pool 表）。
//
// 定位：(source, pool_key, version, aid) 唯一，是一批离线/近线作业的**投影**，
// 不是事实源——随时可以从生成批次重算，回滚只需切 recall_pool_current 指针。
// 在线读只允许读 CURRENT 版本指向的那一批（见 RecallPoolCurrent + TopCurrent）。
type RecallPool struct {
	ID      int64   `db:"id"`       // 自增主键
	Source  int32   `db:"source"`   // 召回路，参见 Source* 常量
	PoolKey string  `db:"pool_key"` // 池键，语法见 ValidatePoolKey
	Version int64   `db:"version"`  // 池版本号
	Aid     int64   `db:"aid"`      // 稿件 ID（video 服务主键，不复制稿件数据）
	Score   float64 `db:"score"`    // 池内分数（同路可比，跨路不可比）
	Ctime   int64   `db:"ctime"`    // 写入时间（Unix 秒）
	Mtime   int64   `db:"mtime"`    // 修改时间（Unix 秒）
}

// PoolItemInput 批量写入的单条候选。
type PoolItemInput struct {
	Aid   int64
	Score float64
}

// RecallPoolModel recall_pool 表读写接口。
type RecallPoolModel interface {
	// BatchUpsert 幂等写入某个版本的条目（依赖 uniq_pool_item + ON DUPLICATE KEY UPDATE），
	// 返回受影响行数。同一条目重复写入只更新分数与 mtime，不产生第二行。
	// 条数超过 MaxPoolItemBatch 直接返回 ErrTooManyItems，不拆语句偷偷写完。
	BatchUpsert(ctx context.Context, source int32, poolKey string, version int64, items []PoolItemInput) (int64, error)
	// BatchUpsertInTx 与 BatchUpsert 同语义，但在调用方事务内执行。
	// UpsertPoolItems 必须走它：条目写入要与"版本仍是 BUILDING"的行锁核对同事务提交，
	// 否则会出现"版本已封版/已上线，条目还在往里追加"的原地改写（已发布版本不可变）。
	BatchUpsertInTx(ctx context.Context, session sqlx.Session, source int32, poolKey string,
		version int64, items []PoolItemInput) (int64, error)
	// TopByVersion 读取指定版本的前 N 条（按 score DESC, aid ASC 稳定排序）。
	// version 由调用方从 recall_pool_current 取得后传入，模型层不做指针解析，
	// 避免读到半成品批次（BUILDING 版本不可见是 repository 的职责）。
	TopByVersion(ctx context.Context, source int32, poolKey string, version int64, limit int) ([]*RecallPool, error)
	// ListByVersion 分页读取某版本条目（offset 由调用方按 pn 换算，limit 必须落在 (0, MaxPoolQueryLimit]）。
	ListByVersion(ctx context.Context, source int32, poolKey string, version int64, offset, limit int) ([]*RecallPool, error)
	// CountByVersion 统计某版本条目数（发布前校验 item_count 是否与登记一致）。
	CountByVersion(ctx context.Context, source int32, poolKey string, version int64) (int64, error)
	// CountByVersionInTx 在调用方事务内统计条目数（能看到本事务尚未提交的写入）。
	// 封版时用它算 item_count：用事务外连接读会漏掉本批条目，
	// 于是"声明 1000 条、登记 900 条"这类假事实进版本行。
	CountByVersionInTx(ctx context.Context, session sqlx.Session, source int32, poolKey string, version int64) (int64, error)
	// DeleteByVersions 按版本批量删除条目，maxRows 限制单次删除行数（锁与主从延迟保护）。
	// 返回 (删除行数, 是否还有剩余)。非事务调用请传 nil session。
	DeleteByVersions(ctx context.Context, source int32, poolKey string, versions []int64, maxRows int64) (int64, bool, error)
	// DeleteByVersionsInTx 与 DeleteByVersions 同语义，但在调用方事务内执行。
	// PrunePoolVersions 必须走它：条目行与版本登记行要一起提交，
	// 否则会出现"条目已清空、版本行还在"的半清理状态（下一轮清理看不到差异，孤儿条目永久留在库里）。
	DeleteByVersionsInTx(ctx context.Context, session sqlx.Session, source int32, poolKey string,
		versions []int64, maxRows int64) (int64, bool, error)
	// 本模型刻意不提供"按 aid 摘除单条候选"的方法：池条目是整批投影，
	// 手工删某一条等于绕过生成作业改写候选集（AGENTS.md §7 无运营干预入口）。
	// 要下架某稿件，由生成作业在下一个版本里不再产出该 aid，再经 PublishPoolVersion 切指针。
}

type defaultRecallPoolModel struct {
	conn sqlx.SqlConn
}

// NewRecallPoolModel 创建 RecallPoolModel 实现。
func NewRecallPoolModel(conn sqlx.SqlConn) RecallPoolModel {
	return &defaultRecallPoolModel{conn: conn}
}

// exec 在事务 session 与全局连接之间选择执行器（与 poolversion.go 同一约定）。
func (m *defaultRecallPoolModel) exec(session sqlx.Session) sqlx.Session {
	if session != nil {
		return session
	}
	return m.conn
}

// poolSelect 是 recall_pool 的列清单，与迁移文件列顺序保持一致。
const poolSelect = "SELECT id, source, pool_key, version, aid, score, ctime, mtime FROM recall_pool"

func (m *defaultRecallPoolModel) BatchUpsert(ctx context.Context, source int32, poolKey string, version int64, items []PoolItemInput) (int64, error) {
	return m.batchUpsert(ctx, nil, source, poolKey, version, items)
}

func (m *defaultRecallPoolModel) BatchUpsertInTx(ctx context.Context, session sqlx.Session,
	source int32, poolKey string, version int64, items []PoolItemInput) (int64, error) {
	return m.batchUpsert(ctx, session, source, poolKey, version, items)
}

// batchUpsert 是两种执行器共用的写入体。
//
// 注意 RowsAffected 的 MySQL ODKU 语义：新插入=1、值完全未变=0、更新=2，
// 因此返回值是"影响行数"而不是"新增行数"，重放同一批会得到 0 或 2*条数，
// 这是正常结果，调用方不得据此判定写入失败。
func (m *defaultRecallPoolModel) batchUpsert(ctx context.Context, session sqlx.Session,
	source int32, poolKey string, version int64, items []PoolItemInput) (int64, error) {
	if !ValidSource(source) {
		return 0, ErrInvalidSource
	}
	if err := ValidatePoolKey(source, poolKey); err != nil {
		return 0, err
	}
	if version <= 0 {
		return 0, ErrInvalidVersion
	}
	if len(items) == 0 {
		return 0, nil
	}
	if len(items) > MaxPoolItemBatch {
		return 0, ErrTooManyItems
	}
	now := nowUnix()
	var sb strings.Builder
	sb.WriteString("INSERT INTO recall_pool (source, pool_key, version, aid, score, ctime, mtime) VALUES ")
	args := make([]interface{}, 0, len(items)*7)
	for i, it := range items {
		if it.Aid <= 0 {
			return 0, ErrInvalidAid
		}
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?,?,?,?,?,?,?)")
		args = append(args, source, poolKey, version, it.Aid, it.Score, now, now)
	}
	// 幂等：重复写入同一条目只刷新分数与 mtime，不新增行。
	sb.WriteString(" ON DUPLICATE KEY UPDATE score = VALUES(score), mtime = VALUES(mtime)")
	res, err := m.exec(session).ExecCtx(ctx, sb.String(), args...)
	if err != nil {
		return 0, fmt.Errorf("recall_pool BatchUpsert: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("recall_pool BatchUpsert RowsAffected: %w", err)
	}
	return affected, nil
}

func (m *defaultRecallPoolModel) TopByVersion(ctx context.Context, source int32, poolKey string, version int64, limit int) ([]*RecallPool, error) {
	if err := checkPoolQueryArgs(source, poolKey, version, limit); err != nil {
		return nil, err
	}
	query := poolSelect + " WHERE source = ? AND pool_key = ? AND version = ? ORDER BY score DESC, aid ASC LIMIT ?"
	var rows []*RecallPool
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, source, poolKey, version, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("recall_pool TopByVersion: %w", err)
	}
	return rows, nil
}

func (m *defaultRecallPoolModel) ListByVersion(ctx context.Context, source int32, poolKey string, version int64, offset, limit int) ([]*RecallPool, error) {
	if err := checkPoolQueryArgs(source, poolKey, version, limit); err != nil {
		return nil, err
	}
	if offset < 0 {
		offset = 0
	}
	if offset > MaxPoolSnapshotOffset {
		return nil, ErrPageTooDeep
	}
	query := poolSelect + " WHERE source = ? AND pool_key = ? AND version = ? ORDER BY score DESC, aid ASC LIMIT ? OFFSET ?"
	var rows []*RecallPool
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, source, poolKey, version, limit, offset); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("recall_pool ListByVersion: %w", err)
	}
	return rows, nil
}

func (m *defaultRecallPoolModel) CountByVersion(ctx context.Context, source int32, poolKey string, version int64) (int64, error) {
	return m.countByVersion(ctx, nil, source, poolKey, version)
}

func (m *defaultRecallPoolModel) CountByVersionInTx(ctx context.Context, session sqlx.Session,
	source int32, poolKey string, version int64) (int64, error) {
	return m.countByVersion(ctx, session, source, poolKey, version)
}

// countByVersion 是两种执行器共用的统计体。
func (m *defaultRecallPoolModel) countByVersion(ctx context.Context, session sqlx.Session,
	source int32, poolKey string, version int64) (int64, error) {
	if !ValidSource(source) {
		return 0, ErrInvalidSource
	}
	if err := ValidatePoolKey(source, poolKey); err != nil {
		return 0, err
	}
	if version <= 0 {
		return 0, ErrInvalidVersion
	}
	var count int64
	query := "SELECT COUNT(1) FROM recall_pool WHERE source = ? AND pool_key = ? AND version = ?"
	if err := m.exec(session).QueryRowCtx(ctx, &count, query, source, poolKey, version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("recall_pool CountByVersion: %w", err)
	}
	return count, nil
}

func (m *defaultRecallPoolModel) DeleteByVersions(ctx context.Context, source int32, poolKey string, versions []int64, maxRows int64) (int64, bool, error) {
	return m.deleteByVersions(ctx, nil, source, poolKey, versions, maxRows)
}

func (m *defaultRecallPoolModel) DeleteByVersionsInTx(ctx context.Context, session sqlx.Session,
	source int32, poolKey string, versions []int64, maxRows int64) (int64, bool, error) {
	return m.deleteByVersions(ctx, session, source, poolKey, versions, maxRows)
}

// deleteByVersions 是两种执行器共用的删除体。
func (m *defaultRecallPoolModel) deleteByVersions(ctx context.Context, session sqlx.Session,
	source int32, poolKey string, versions []int64, maxRows int64) (int64, bool, error) {
	if len(versions) == 0 {
		return 0, false, nil
	}
	if !ValidSource(source) {
		return 0, false, ErrInvalidSource
	}
	if err := ValidatePoolKey(source, poolKey); err != nil {
		return 0, false, err
	}
	// 版本列表显式展开占位符，不依赖驱动的 slice 展开行为。
	args := []interface{}{source, poolKey}
	for _, v := range versions {
		if v <= 0 {
			return 0, false, ErrInvalidVersion
		}
		args = append(args, v)
	}
	// maxRows 必须显式给出且有硬上限：无 LIMIT 的 DELETE 会锁住整段池条目。
	if err := CheckInt64Limit(maxRows, MaxDeleteRows); err != nil {
		return 0, false, err
	}
	args = append(args, maxRows)
	// 分批删除：LIMIT 限制单次锁住的行数，调用方按 hasMore 继续下一批。
	query := "DELETE FROM recall_pool WHERE source = ? AND pool_key = ? AND version IN (" +
		inPlaceholders(len(versions)) + ") LIMIT ?"
	res, err := m.exec(session).ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, false, fmt.Errorf("recall_pool DeleteByVersions: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("recall_pool DeleteByVersions RowsAffected: %w", err)
	}
	return deleted, deleted >= maxRows, nil
}

// checkPoolQueryArgs 是所有 recall_pool 读路径共用的前置校验。
//
// 关键点：version<=0 一律拒绝，而不是查一个"版本 0"返回空集。
// 空集在本服务里有两种含义（池真的没条目 / 池从未上线），
// 让模型层吞掉参数错误会把后者伪装成前者，进而在 DegradationInfo 里误报为
// ALL_SOURCES_EMPTY 而不是 POOL_NOT_READY。
func checkPoolQueryArgs(source int32, poolKey string, version int64, limit int) error {
	if !ValidSource(source) {
		return ErrInvalidSource
	}
	if err := ValidatePoolKey(source, poolKey); err != nil {
		return err
	}
	if version <= 0 {
		return ErrInvalidVersion
	}
	return CheckLimit(limit, MaxPoolQueryLimit)
}
