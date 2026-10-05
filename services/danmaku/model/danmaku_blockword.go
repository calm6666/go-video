package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 屏蔽词作用域，与 danmaku_blockword.scope 列和 rpc.BlockWordScope 一致。
const (
	// ScopeGlobal 全局屏蔽词。
	ScopeGlobal int32 = 1
	// ScopeOid 单内容/分区屏蔽词。
	ScopeOid int32 = 2
)

// 屏蔽词状态。
const (
	// BlockWordDisabled 停用（保留行便于审计）。
	BlockWordDisabled int32 = 0
	// BlockWordEnabled 生效。
	BlockWordEnabled int32 = 1
)

// BlockWord 屏蔽词条目（danmaku_blockword 表）。
// 本表是发送侧词库的唯一来源，由运营通过 BlockWord RPC 维护；
// 与 moderation-orchestrator 的审核规则不是一回事，后者归审核域。
type BlockWord struct {
	WordID   int64  `db:"word_id"`  // 词 ID
	Word     string `db:"word"`     // 屏蔽词原文
	Scope    int32  `db:"scope"`    // 作用域
	Oid      int64  `db:"oid"`      // scope=ScopeOid 时的内容主键
	State    int32  `db:"state"`    // 1 生效、0 停用
	Operator int64  `db:"operator"` // 最近操作运营 ID
	Ctime    int64  `db:"ctime"`    // 创建时间（Unix 秒）
	Mtime    int64  `db:"mtime"`    // 修改时间（Unix 秒）
}

// BlockWordModel danmaku_blockword 表查询与写入接口。
type BlockWordModel interface {
	// Upsert 按 word 唯一索引新增或重新启用词条，返回 word_id。
	Upsert(ctx context.Context, w *BlockWord) (int64, error)
	// Disable 停用词条；返回 false 表示词条不存在。
	Disable(ctx context.Context, word string, operator int64) (bool, error)
	// Delete 物理删除词条；返回 false 表示词条不存在。
	Delete(ctx context.Context, word string) (bool, error)
	// FindOne 按 word 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, word string) (*BlockWord, error)
	// ListEnabled 列出全部生效词（全局 + 指定 oid 的分区词），供发送侧词库加载。
	// oid<=0 时只返回全局词。
	ListEnabled(ctx context.Context, oid int64) ([]*BlockWord, error)
	// List 运营侧分页查询。scope<=0 表示不过滤；onlyEnabled 只查生效词。
	List(ctx context.Context, scope int32, oid int64, onlyEnabled bool, pn, ps int32) ([]*BlockWord, int32, error)
}

type defaultBlockWordModel struct {
	conn sqlx.SqlConn
}

// NewBlockWordModel 创建 BlockWordModel 实现。
func NewBlockWordModel(conn sqlx.SqlConn) BlockWordModel {
	return &defaultBlockWordModel{conn: conn}
}

func (m *defaultBlockWordModel) Upsert(ctx context.Context, w *BlockWord) (int64, error) {
	now := nowUnix()
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO danmaku_blockword (word, scope, oid, state, operator, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE scope = VALUES(scope), oid = VALUES(oid), state = VALUES(state), operator = VALUES(operator), mtime = VALUES(mtime)",
		w.Word, w.Scope, w.Oid, w.State, w.Operator, now, now)
	if err != nil {
		return 0, fmt.Errorf("danmaku_blockword Upsert: %w", err)
	}
	var id int64
	if err := m.conn.QueryRowCtx(ctx, &id, "SELECT word_id FROM danmaku_blockword WHERE word = ? LIMIT 1", w.Word); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrBlockWordNotFound
		}
		return 0, fmt.Errorf("danmaku_blockword Upsert find: %w", err)
	}
	return id, nil
}

func (m *defaultBlockWordModel) Disable(ctx context.Context, word string, operator int64) (bool, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE danmaku_blockword SET state = ?, operator = ?, mtime = ? WHERE word = ?",
		BlockWordDisabled, operator, nowUnix(), word)
	if err != nil {
		return false, fmt.Errorf("danmaku_blockword Disable: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("danmaku_blockword Disable RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultBlockWordModel) Delete(ctx context.Context, word string) (bool, error) {
	res, err := m.conn.ExecCtx(ctx, "DELETE FROM danmaku_blockword WHERE word = ?", word)
	if err != nil {
		return false, fmt.Errorf("danmaku_blockword Delete: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("danmaku_blockword Delete RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultBlockWordModel) FindOne(ctx context.Context, word string) (*BlockWord, error) {
	var w BlockWord
	query := "SELECT word_id, word, scope, oid, state, operator, ctime, mtime FROM danmaku_blockword WHERE word = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &w, query, word); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("danmaku_blockword FindOne: %w", err)
	}
	return &w, nil
}

func (m *defaultBlockWordModel) ListEnabled(ctx context.Context, oid int64) ([]*BlockWord, error) {
	var rows []*BlockWord
	var err error
	if oid > 0 {
		err = m.conn.QueryRowsCtx(ctx, &rows,
			"SELECT word_id, word, scope, oid, state, operator, ctime, mtime FROM danmaku_blockword WHERE state = ? AND (scope = ? OR (scope = ? AND oid = ?))",
			BlockWordEnabled, ScopeGlobal, ScopeOid, oid)
	} else {
		err = m.conn.QueryRowsCtx(ctx, &rows,
			"SELECT word_id, word, scope, oid, state, operator, ctime, mtime FROM danmaku_blockword WHERE state = ? AND scope = ?",
			BlockWordEnabled, ScopeGlobal)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("danmaku_blockword ListEnabled: %w", err)
	}
	return rows, nil
}

func (m *defaultBlockWordModel) List(ctx context.Context, scope int32, oid int64, onlyEnabled bool, pn, ps int32) ([]*BlockWord, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 100 {
		ps = 20
	}

	where := "WHERE 1 = 1"
	args := make([]interface{}, 0, 4)
	if scope > 0 {
		where += " AND scope = ?"
		args = append(args, scope)
	}
	if oid > 0 {
		where += " AND oid = ?"
		args = append(args, oid)
	}
	if onlyEnabled {
		where += " AND state = ?"
		args = append(args, BlockWordEnabled)
	}

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM danmaku_blockword "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("danmaku_blockword List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	listArgs := append(append([]interface{}{}, args...), ps, (pn-1)*ps)
	var rows []*BlockWord
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT word_id, word, scope, oid, state, operator, ctime, mtime FROM danmaku_blockword "+
			where+" ORDER BY word_id DESC LIMIT ? OFFSET ?", listArgs...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("danmaku_blockword List: %w", err)
	}
	return rows, total, nil
}
