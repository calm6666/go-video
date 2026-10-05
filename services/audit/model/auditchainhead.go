package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// chainHeadColumns 是 audit_chain_head 的列清单。
const chainHeadColumns = "chain_key, last_entry_id, last_hash, seq, entry_count, first_at, mtime"

// ChainHead 对应 audit_chain_head 表：一条哈希链的当前尾部状态。
//
// 为什么需要它：追加一条审计要先拿到「上一条的 entry_hash」和「下一个 seq」，
// 这两个值必须在同一个事务里被同一条 SELECT ... FOR UPDATE 锁定，
// 否则并发追加会算出相同 seq/prev_hash，链会出现分叉。
// 锁的粒度是 chain_key（域 + 日），所以并发度等于「活跃域数 × 1 天」，
// 而不是全局单行锁。
type ChainHead struct {
	// ChainKey 链标识："<action_domain>/<UTC 日>"，主键。
	ChainKey string `db:"chain_key"`
	// LastEntryID 链尾条目的 entry_id。
	LastEntryID int64 `db:"last_entry_id"`
	// LastHash 链尾条目的 entry_hash；新条目的 prev_hash 取此值。
	LastHash string `db:"last_hash"`
	// Seq 已分配的最大序号（下一条是 Seq + 1）。
	Seq int64 `db:"seq"`
	// EntryCount 累计条数（等长于 Seq，单独存一列是为了让「序号空洞」
	// 能被 COUNT 与 MAX(seq) 的差值直接暴露出来）。
	EntryCount int64 `db:"entry_count"`
	// FirstAt 链上第一条的业务时间（Unix 秒），用于归档时快速判断整链是否已封口。
	FirstAt int64 `db:"first_at"`
	// Mtime 最后推进时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
}

// AuditChainHeadModel 抽象 audit_chain_head 表。
// 本表只有推进没有回退：不提供 Decrease/Rollback。
type AuditChainHeadModel interface {
	// Ensure 幂等建链头（seq=0、last_hash=''）。并发建链靠唯一键 + ON DUPLICATE KEY UPDATE
	// 自身主键，保证不会因两个请求同时插入而报错。
	Ensure(ctx context.Context, session sqlx.Session, chainKey string) error
	// LockForUpdate 在事务内锁定链头行，返回当前尾部状态。
	// 必须在写入 audit_entry 之前调用；链头不存在时返回 ErrChainHeadMissing。
	LockForUpdate(ctx context.Context, session sqlx.Session, chainKey string) (*ChainHead, error)
	// Advance 把链头推进到新尾部（seq/last_entry_id/last_hash/entry_count/mtime）。
	// WHERE 带 seq = expectSeq 条件（乐观锁）：并发推进时只有一人成功，
	// 失败方返回 updated=false，由 repository 转成 ErrChainConflict 让调用方重试。
	Advance(ctx context.Context, session sqlx.Session, chainKey string, expectSeq int64, head *ChainHead) (bool, error)
	// FindOne 读取链头（不加锁，供 VerifyAuditChain 判断链尾）。
	FindOne(ctx context.Context, chainKey string) (*ChainHead, error)
	// List 分页列出链头（供运维巡检：哪条链在动、动了多少条）。
	List(ctx context.Context, actionDomain string, pn, ps int32) ([]*ChainHead, int64, error)
}

type defaultAuditChainHeadModel struct {
	conn sqlx.SqlConn
}

// NewAuditChainHeadModel 构造 audit_chain_head 的 sqlx 实现。
func NewAuditChainHeadModel(conn sqlx.SqlConn) AuditChainHeadModel {
	return &defaultAuditChainHeadModel{conn: conn}
}

func (m *defaultAuditChainHeadModel) exec(session sqlx.Session) sqlx.Session {
	if session != nil {
		return session
	}
	return m.conn
}

func (m *defaultAuditChainHeadModel) Ensure(ctx context.Context, session sqlx.Session, chainKey string) error {
	if chainKey == "" {
		return ErrChainKeyRequired
	}
	_, err := m.exec(session).ExecCtx(ctx,
		"INSERT INTO audit_chain_head (chain_key, last_entry_id, last_hash, seq, entry_count, first_at, mtime)"+
			" VALUES (?, 0, '', 0, 0, 0, ?)"+
			" ON DUPLICATE KEY UPDATE chain_key = chain_key",
		chainKey, nowUnix())
	if err != nil {
		return fmt.Errorf("audit_chain_head Ensure: %w", err)
	}
	return nil
}

func (m *defaultAuditChainHeadModel) LockForUpdate(ctx context.Context, session sqlx.Session, chainKey string) (*ChainHead, error) {
	if chainKey == "" {
		return nil, ErrChainKeyRequired
	}
	if session == nil {
		// 没有事务就没有行锁：脱离事务的 FOR UPDATE 会立刻释放，
		// 调用方以为锁住了其实没有。宁可报错也不能给出假的串行化保证。
		return nil, fmt.Errorf("audit_chain_head LockForUpdate: %w", ErrChainConflict)
	}
	var row ChainHead
	query := "SELECT " + chainHeadColumns + " FROM audit_chain_head WHERE chain_key = ? FOR UPDATE"
	if err := session.QueryRowCtx(ctx, &row, query, chainKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrChainHeadMissing
		}
		return nil, fmt.Errorf("audit_chain_head LockForUpdate: %w", err)
	}
	return &row, nil
}

func (m *defaultAuditChainHeadModel) Advance(ctx context.Context, session sqlx.Session, chainKey string,
	expectSeq int64, head *ChainHead) (bool, error) {
	if chainKey == "" {
		return false, ErrChainKeyRequired
	}
	res, err := m.exec(session).ExecCtx(ctx,
		"UPDATE audit_chain_head SET last_entry_id = ?, last_hash = ?, seq = ?, entry_count = ?,"+
			" first_at = IF(first_at = 0, ?, first_at), mtime = ?"+
			" WHERE chain_key = ? AND seq = ?",
		head.LastEntryID, head.LastHash, head.Seq, head.EntryCount, head.FirstAt, nowUnix(), chainKey, expectSeq)
	if err != nil {
		return false, fmt.Errorf("audit_chain_head Advance: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("audit_chain_head Advance RowsAffected: %w", err)
	}
	return n == 1, nil
}

func (m *defaultAuditChainHeadModel) FindOne(ctx context.Context, chainKey string) (*ChainHead, error) {
	var row ChainHead
	query := "SELECT " + chainHeadColumns + " FROM audit_chain_head WHERE chain_key = ? LIMIT 1"
	err := m.conn.QueryRowCtx(ctx, &row, query, chainKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrChainHeadMissing
		}
		return nil, fmt.Errorf("audit_chain_head FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultAuditChainHeadModel) List(ctx context.Context, actionDomain string, pn, ps int32) ([]*ChainHead, int64, error) {
	where := "WHERE 1 = 1"
	args := make([]any, 0, 2)
	if actionDomain != "" {
		// chain_key 形如 "<domain>/<date>"，前缀匹配可用 idx 走范围扫描。
		where += " AND chain_key LIKE ?"
		args = append(args, actionDomain+"/%")
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM audit_chain_head "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("audit_chain_head List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), ps, (pn-1)*ps)
	var rows []*ChainHead
	query := "SELECT " + chainHeadColumns + " FROM audit_chain_head " + where +
		" ORDER BY chain_key DESC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("audit_chain_head List: %w", err)
	}
	return rows, total, nil
}
