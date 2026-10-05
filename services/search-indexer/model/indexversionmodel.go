package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// SearchIndexVersion 索引版本登记表（search_index_version 表）。
// 一个别名在同一时刻只允许一行 state=active（由 uniq_index_name + 应用层 CAS 保证）。
// 本表是「哪个物理索引承接写入/查询」的唯一登记处，避免依赖 OpenSearch 别名做隐式推断。
type SearchIndexVersion struct {
	ID            int64  `db:"id"`             // 自增主键
	Alias         string `db:"alias"`          // 查询别名（search-query 读取）
	IndexName     string `db:"index_name"`     // 物理索引名（全局唯一）
	SchemaVersion string `db:"schema_version"` // 文档结构版本，如 v1
	DocCount      int64  `db:"doc_count"`      // doc 数快照（健康检查/切换时刷新）
	State         string `db:"state"`          // active/retiring/history
	CreatedBy     string `db:"created_by"`     // 创建者：bootstrap/rebuild task_id/运维
	Ctime         int64  `db:"ctime"`          // 创建时间（Unix 秒）
	Mtime         int64  `db:"mtime"`          // 修改时间（Unix 秒）
}

// SearchIndexVersionModel search_index_version 表查询与写入接口。
type SearchIndexVersionModel interface {
	// Insert 登记新索引；命中 uniq_index_name 时返回 existed=false（不覆盖既有登记）。
	Insert(ctx context.Context, v *SearchIndexVersion) (bool, error)
	// FindActive 查询别名当前 active 索引；无登记返回 (nil, nil)。
	FindActive(ctx context.Context, alias string) (*SearchIndexVersion, error)
	// FindByIndexName 按物理索引名查询；不存在返回 (nil, nil)。
	FindByIndexName(ctx context.Context, indexName string) (*SearchIndexVersion, error)
	// ListByAlias 列出别名下的全部版本（含 history），按 id 倒序。
	ListByAlias(ctx context.Context, alias string) ([]*SearchIndexVersion, error)
	// ListAll 列出所有登记（健康检查用）。
	ListAll(ctx context.Context) ([]*SearchIndexVersion, error)
	// SwitchActive 原子切换别名指向：把 alias 下当前 active 行降为 retireState，
	// 并把 indexName 置为 active。indexName 必须已登记，否则返回 ErrVersionNotFound。
	// expectedActive 非空时做乐观校验（与当前 active 索引不一致返回 ErrAliasMismatch）。
	SwitchActive(ctx context.Context, alias, indexName, expectedActive, retireState string, now int64) error
	// UpdateState 更新单行状态（retiring → history 等运维动作）。
	UpdateState(ctx context.Context, indexName, state string, now int64) error
	// UpdateDocCount 刷新 doc 数快照。
	UpdateDocCount(ctx context.Context, indexName string, docCount int64, now int64) error
}

type defaultVersionModel struct {
	conn sqlx.SqlConn
}

// NewSearchIndexVersionModel 创建 SearchIndexVersionModel 实现。
func NewSearchIndexVersionModel(conn sqlx.SqlConn) SearchIndexVersionModel {
	return &defaultVersionModel{conn: conn}
}

const versionColumns = "id, alias, index_name, schema_version, doc_count, state, created_by, ctime, mtime"

func (m *defaultVersionModel) Insert(ctx context.Context, v *SearchIndexVersion) (bool, error) {
	res, err := m.conn.ExecCtx(ctx,
		"INSERT IGNORE INTO search_index_version (alias, index_name, schema_version, doc_count, state, created_by, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		v.Alias, v.IndexName, v.SchemaVersion, v.DocCount, v.State, v.CreatedBy, v.Ctime, v.Mtime)
	if err != nil {
		return false, fmt.Errorf("search_index_version Insert: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("search_index_version Insert RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultVersionModel) FindActive(ctx context.Context, alias string) (*SearchIndexVersion, error) {
	var v SearchIndexVersion
	query := "SELECT " + versionColumns + " FROM search_index_version WHERE alias = ? AND state = ? ORDER BY id DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &v, query, alias, VersionStateActive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("search_index_version FindActive: %w", err)
	}
	return &v, nil
}

func (m *defaultVersionModel) FindByIndexName(ctx context.Context, indexName string) (*SearchIndexVersion, error) {
	var v SearchIndexVersion
	query := "SELECT " + versionColumns + " FROM search_index_version WHERE index_name = ?"
	if err := m.conn.QueryRowCtx(ctx, &v, query, indexName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("search_index_version FindByIndexName: %w", err)
	}
	return &v, nil
}

func (m *defaultVersionModel) ListByAlias(ctx context.Context, alias string) ([]*SearchIndexVersion, error) {
	var rows []*SearchIndexVersion
	query := "SELECT " + versionColumns + " FROM search_index_version WHERE alias = ? ORDER BY id DESC"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, alias); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("search_index_version ListByAlias: %w", err)
	}
	return rows, nil
}

func (m *defaultVersionModel) ListAll(ctx context.Context) ([]*SearchIndexVersion, error) {
	var rows []*SearchIndexVersion
	query := "SELECT " + versionColumns + " FROM search_index_version ORDER BY alias ASC, id DESC"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("search_index_version ListAll: %w", err)
	}
	return rows, nil
}

func (m *defaultVersionModel) SwitchActive(ctx context.Context, alias, indexName, expectedActive, retireState string, now int64) error {
	return m.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		var cur SearchIndexVersion
		query := "SELECT " + versionColumns + " FROM search_index_version WHERE alias = ? AND state = ? LIMIT 1 FOR UPDATE"
		err := session.QueryRowCtx(ctx, &cur, query, alias, VersionStateActive)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if expectedActive != "" {
				return fmt.Errorf("%w: expected %s, alias has no active index", ErrAliasMismatch, expectedActive)
			}
		case err != nil:
			return fmt.Errorf("search_index_version SwitchActive select: %w", err)
		default:
			if expectedActive != "" && cur.IndexName != expectedActive {
				return fmt.Errorf("%w: current=%s expected=%s", ErrAliasMismatch, cur.IndexName, expectedActive)
			}
			if cur.IndexName != indexName {
				if _, err := session.ExecCtx(ctx,
					"UPDATE search_index_version SET state = ?, mtime = ? WHERE index_name = ?",
					retireState, now, cur.IndexName); err != nil {
					return fmt.Errorf("search_index_version SwitchActive retire: %w", err)
				}
			}
		}

		res, err := session.ExecCtx(ctx,
			"UPDATE search_index_version SET state = ?, mtime = ? WHERE index_name = ?",
			VersionStateActive, now, indexName)
		if err != nil {
			return fmt.Errorf("search_index_version SwitchActive activate: %w", err)
		}
		aff, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("search_index_version SwitchActive RowsAffected: %w", err)
		}
		if aff == 0 {
			return ErrVersionNotFound
		}
		return nil
	})
}

func (m *defaultVersionModel) UpdateState(ctx context.Context, indexName, state string, now int64) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE search_index_version SET state = ?, mtime = ? WHERE index_name = ?", state, now, indexName)
	if err != nil {
		return fmt.Errorf("search_index_version UpdateState: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("search_index_version UpdateState RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrVersionNotFound
	}
	return nil
}

func (m *defaultVersionModel) UpdateDocCount(ctx context.Context, indexName string, docCount int64, now int64) error {
	if _, err := m.conn.ExecCtx(ctx,
		"UPDATE search_index_version SET doc_count = ?, mtime = ? WHERE index_name = ?", docCount, now, indexName); err != nil {
		return fmt.Errorf("search_index_version UpdateDocCount: %w", err)
	}
	return nil
}
