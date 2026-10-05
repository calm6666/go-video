package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// SearchHotKeyword 热词快照（search_hot_keyword 表）。
//
// 数据来源：由 search_query_log 聚合得到的“快照”，不是事实源。
// 写入方：热度聚合任务（阶段 2 由 services/cron 或运维导入流程负责），
// 本服务查询链路只读该表；上线时若表为空，HotKeywords 返回空列表并说明
// snapshot_at=0（表示尚无快照），不编造热词。
type SearchHotKeyword struct {
	Id         int64   `db:"id"`          // 自增主键
	Scope      string  `db:"scope"`       // 作用域：global 或 zone:<zone_id>
	Keyword    string  `db:"keyword"`     // 热词
	Score      float64 `db:"score"`       // 热度分
	SnapshotAt int64   `db:"snapshot_at"` // 快照生成时间（Unix 秒）
	Ctime      int64   `db:"ctime"`       // 创建时间（Unix 秒）
	Mtime      int64   `db:"mtime"`       // 修改时间（Unix 秒）
}

// SearchHotKeywordModel search_hot_keyword 表访问接口。
type SearchHotKeywordModel interface {
	// ListByScope 按 scope 取热度倒序的前 limit 条。
	ListByScope(ctx context.Context, scope string, limit int32) ([]*SearchHotKeyword, error)
	// LatestSnapshotAt 返回该 scope 最近一次快照时间；无数据返回 (0, nil)。
	LatestSnapshotAt(ctx context.Context, scope string) (int64, error)
	// UpsertSnapshot 幂等写入/更新一个 (scope, keyword) 的热度值，
	// 供聚合任务调用（查询链路不使用）。
	UpsertSnapshot(ctx context.Context, h *SearchHotKeyword) error
	// PruneStale 删除该 scope 下早于 before 的快照行（聚合完成后清理旧词）。
	PruneStale(ctx context.Context, scope string, before int64) (int64, error)
}

type defaultSearchHotKeywordModel struct {
	conn sqlx.SqlConn
}

// NewSearchHotKeywordModel 创建 SearchHotKeywordModel 实现。
func NewSearchHotKeywordModel(conn sqlx.SqlConn) SearchHotKeywordModel {
	return &defaultSearchHotKeywordModel{conn: conn}
}

const hotRows = "id, scope, keyword, score, snapshot_at, ctime, mtime"

func (m *defaultSearchHotKeywordModel) ListByScope(ctx context.Context, scope string, limit int32) ([]*SearchHotKeyword, error) {
	if limit <= 0 {
		return nil, nil
	}
	var rows []*SearchHotKeyword
	query := fmt.Sprintf("SELECT %s FROM search_hot_keyword WHERE scope = ? ORDER BY score DESC, id ASC LIMIT ?", hotRows)
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, scope, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("search_hot_keyword ListByScope: %w", err)
	}
	return rows, nil
}

func (m *defaultSearchHotKeywordModel) LatestSnapshotAt(ctx context.Context, scope string) (int64, error) {
	var ts int64
	err := m.conn.QueryRowCtx(ctx, &ts, "SELECT COALESCE(MAX(snapshot_at), 0) FROM search_hot_keyword WHERE scope = ?", scope)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("search_hot_keyword LatestSnapshotAt: %w", err)
	}
	return ts, nil
}

func (m *defaultSearchHotKeywordModel) UpsertSnapshot(ctx context.Context, h *SearchHotKeyword) error {
	if h.Scope == "" || h.Keyword == "" {
		return fmt.Errorf("search_hot_keyword UpsertSnapshot: empty scope/keyword")
	}
	now := nowUnix()
	if h.SnapshotAt == 0 {
		h.SnapshotAt = now
	}
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO search_hot_keyword (scope, keyword, score, snapshot_at, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE score = VALUES(score), snapshot_at = VALUES(snapshot_at), mtime = VALUES(mtime)",
		h.Scope, h.Keyword, h.Score, h.SnapshotAt, now, now)
	if err != nil {
		return fmt.Errorf("search_hot_keyword UpsertSnapshot: %w", err)
	}
	return nil
}

func (m *defaultSearchHotKeywordModel) PruneStale(ctx context.Context, scope string, before int64) (int64, error) {
	res, err := m.conn.ExecCtx(ctx, "DELETE FROM search_hot_keyword WHERE scope = ? AND snapshot_at < ?", scope, before)
	if err != nil {
		return 0, fmt.Errorf("search_hot_keyword PruneStale: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("search_hot_keyword PruneStale RowsAffected: %w", err)
	}
	return n, nil
}
