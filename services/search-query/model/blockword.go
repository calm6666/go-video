package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// SearchBlockWord 屏蔽词/无结果保护词（search_block_word 表）。
//
// 语义：命中生效词时不查询引擎，返回“无结果 + safe_filtered=true”，
// 避免通过搜索试探敏感内容，也不泄露命中数量。
// 词表维护由运营侧完成（本服务查询链路只读；写入口在后续管理接口评审后增加，
// 不由网关或客户端直接改表，见 AGENTS.md §3：管理逻辑归 services/operation）。
type SearchBlockWord struct {
	Id       int64  `db:"id"`       // 自增主键
	Word     string `db:"word"`     // 屏蔽词（规范化后的关键词，唯一）
	State    int32  `db:"state"`    // 0 生效、1 停用
	Operator string `db:"operator"` // 维护人/来源系统，用于审计
	Ctime    int64  `db:"ctime"`    // 创建时间（Unix 秒）
	Mtime    int64  `db:"mtime"`    // 修改时间（Unix 秒）
}

// SearchBlockWordModel search_block_word 表访问接口。
type SearchBlockWordModel interface {
	// IsBlocked 判断规范化关键词是否为生效屏蔽词。
	IsBlocked(ctx context.Context, keyword string) (bool, error)
	// ListActive 按 id 游标分页拉取生效词（供缓存字典刷新使用）。
	// afterId=0 表示从头开始；返回结果按 id 升序。
	ListActive(ctx context.Context, afterId int64, limit int32) ([]*SearchBlockWord, error)
}

type defaultSearchBlockWordModel struct {
	conn sqlx.SqlConn
}

// NewSearchBlockWordModel 创建 SearchBlockWordModel 实现。
func NewSearchBlockWordModel(conn sqlx.SqlConn) SearchBlockWordModel {
	return &defaultSearchBlockWordModel{conn: conn}
}

func (m *defaultSearchBlockWordModel) IsBlocked(ctx context.Context, keyword string) (bool, error) {
	if keyword == "" {
		return false, ErrInvalidKeyword
	}
	var id int64
	err := m.conn.QueryRowCtx(ctx, &id,
		"SELECT id FROM search_block_word WHERE word = ? AND state = ? LIMIT 1", keyword, BlockWordStateActive)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("search_block_word IsBlocked: %w", err)
	}
	return id > 0, nil
}

func (m *defaultSearchBlockWordModel) ListActive(ctx context.Context, afterId int64, limit int32) ([]*SearchBlockWord, error) {
	if limit <= 0 {
		return nil, nil
	}
	var rows []*SearchBlockWord
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT id, word, state, operator, ctime, mtime FROM search_block_word WHERE state = ? AND id > ? ORDER BY id ASC LIMIT ?",
		BlockWordStateActive, afterId, limit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("search_block_word ListActive: %w", err)
	}
	return rows, nil
}
