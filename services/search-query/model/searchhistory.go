package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// nowUnix 当前 Unix 秒。
func nowUnix() int64 { return time.Now().Unix() }

// SearchHistory 用户搜索历史（search_history 表）。
//
// 幂等：唯一索引 (mid, keyword) + INSERT ... ON DUPLICATE KEY UPDATE，
// 同一用户重复搜索同一词只更新时间与端信息，不产生多行。
// 隐私：用户删除/清空为物理 DELETE（见 DeleteKeyword/DeleteAll），
// state 只用于运营标记待清理行；原始 IP、设备号不入本表。
type SearchHistory struct {
	Id          int64  `db:"id"`           // 自增主键
	Mid         int64  `db:"mid"`          // 用户 ID
	Keyword     string `db:"keyword"`      // 关键词（已规范化）
	KeywordHash string `db:"keyword_hash"` // 关键词 sha256（等值定位与聚合用）
	Platform    string `db:"platform"`     // 最近一次来源端
	State       int32  `db:"state"`        // 状态：见 HistoryState* 常量
	Ctime       int64  `db:"ctime"`        // 首次搜索时间（Unix 秒）
	Mtime       int64  `db:"mtime"`        // 最近一次搜索时间（Unix 秒）
}

// SearchHistoryModel search_history 表访问接口。
type SearchHistoryModel interface {
	// Upsert 幂等写入一条历史；已存在则更新 platform/mtime。
	Upsert(ctx context.Context, h *SearchHistory) error
	// ListByKeyset 按 (mtime, id) 倒序取一页；beforeMtime/beforeID 为 0 表示首页。
	// 只返回 state=HistoryStateNormal 的行。
	ListByKeyset(ctx context.Context, mid int64, beforeMtime, beforeID int64, limit int32) ([]*SearchHistory, error)
	// ListByPrefix 取该用户历史中匹配前缀的词（联想合并用），按 mtime 倒序。
	ListByPrefix(ctx context.Context, mid int64, prefix string, limit int32) ([]*SearchHistory, error)
	// FindByKeyword 精确查询一条历史；不存在返回 (nil, nil)。
	FindByKeyword(ctx context.Context, mid int64, keyword string) (*SearchHistory, error)
	// DeleteKeyword 物理删除单个词；返回受影响行数（0 表示无匹配，天然幂等）。
	DeleteKeyword(ctx context.Context, mid int64, keyword string) (int64, error)
	// DeleteAll 物理清空该用户历史；返回受影响行数。
	DeleteAll(ctx context.Context, mid int64) (int64, error)
	// Prune 只保留最近 keep 条，多余行物理删除（限制单用户历史规模）。
	Prune(ctx context.Context, mid int64, keep int64) (int64, error)
}

type defaultSearchHistoryModel struct {
	conn sqlx.SqlConn
}

// NewSearchHistoryModel 创建 SearchHistoryModel 实现。
func NewSearchHistoryModel(conn sqlx.SqlConn) SearchHistoryModel {
	return &defaultSearchHistoryModel{conn: conn}
}

const historyRows = "id, mid, keyword, keyword_hash, platform, state, ctime, mtime"

func (m *defaultSearchHistoryModel) Upsert(ctx context.Context, h *SearchHistory) error {
	if h.Mid <= 0 {
		return ErrInvalidMid
	}
	if h.KeywordHash == "" {
		h.KeywordHash = KeywordHash(h.Keyword)
	}
	now := nowUnix()
	if h.Ctime == 0 {
		h.Ctime = now
	}
	h.Mtime = now
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO search_history (mid, keyword, keyword_hash, platform, state, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE platform = VALUES(platform), mtime = VALUES(mtime), state = VALUES(state)",
		h.Mid, h.Keyword, h.KeywordHash, h.Platform, h.State, h.Ctime, h.Mtime)
	if err != nil {
		return fmt.Errorf("search_history Upsert: %w", err)
	}
	return nil
}

func (m *defaultSearchHistoryModel) ListByKeyset(ctx context.Context, mid int64, beforeMtime, beforeID int64, limit int32) ([]*SearchHistory, error) {
	if limit <= 0 {
		return nil, ErrInvalidPage
	}
	var rows []*SearchHistory
	query := fmt.Sprintf("SELECT %s FROM search_history WHERE mid = ? AND state = ? "+
		"AND (? = 0 OR mtime < ? OR (mtime = ? AND id < ?)) ORDER BY mtime DESC, id DESC LIMIT ?",
		historyRows)
	args := []any{mid, HistoryStateNormal, beforeMtime, beforeMtime, beforeMtime, beforeID, limit}
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("search_history ListByKeyset: %w", err)
	}
	return rows, nil
}

func (m *defaultSearchHistoryModel) ListByPrefix(ctx context.Context, mid int64, prefix string, limit int32) ([]*SearchHistory, error) {
	if prefix == "" || limit <= 0 {
		return nil, nil
	}
	var rows []*SearchHistory
	query := fmt.Sprintf("SELECT %s FROM search_history WHERE mid = ? AND state = ? "+
		"AND keyword LIKE ? ESCAPE '\\\\' ORDER BY mtime DESC, id DESC LIMIT ?", historyRows)
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mid, HistoryStateNormal, EscapeLikePrefix(prefix)+"%", limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("search_history ListByPrefix: %w", err)
	}
	return rows, nil
}

func (m *defaultSearchHistoryModel) FindByKeyword(ctx context.Context, mid int64, keyword string) (*SearchHistory, error) {
	var h SearchHistory
	query := fmt.Sprintf("SELECT %s FROM search_history WHERE mid = ? AND keyword_hash = ? LIMIT 1", historyRows)
	if err := m.conn.QueryRowCtx(ctx, &h, query, mid, KeywordHash(keyword)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("search_history FindByKeyword: %w", err)
	}
	return &h, nil
}

func (m *defaultSearchHistoryModel) DeleteKeyword(ctx context.Context, mid int64, keyword string) (int64, error) {
	res, err := m.conn.ExecCtx(ctx, "DELETE FROM search_history WHERE mid = ? AND keyword_hash = ?", mid, KeywordHash(keyword))
	if err != nil {
		return 0, fmt.Errorf("search_history DeleteKeyword: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("search_history DeleteKeyword RowsAffected: %w", err)
	}
	return n, nil
}

func (m *defaultSearchHistoryModel) DeleteAll(ctx context.Context, mid int64) (int64, error) {
	res, err := m.conn.ExecCtx(ctx, "DELETE FROM search_history WHERE mid = ?", mid)
	if err != nil {
		return 0, fmt.Errorf("search_history DeleteAll: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("search_history DeleteAll RowsAffected: %w", err)
	}
	return n, nil
}

func (m *defaultSearchHistoryModel) Prune(ctx context.Context, mid int64, keep int64) (int64, error) {
	if keep <= 0 {
		return 0, nil
	}
	// MySQL 不允许在 DELETE 中直接子查询同表，这里先取阈值再删。
	var threshold int64
	q := "SELECT COALESCE(MIN(mtime), 0) FROM (SELECT mtime FROM search_history WHERE mid = ? ORDER BY mtime DESC, id DESC LIMIT ?) t"
	if err := m.conn.QueryRowCtx(ctx, &threshold, q, mid, keep); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("search_history Prune threshold: %w", err)
	}
	if threshold == 0 {
		return 0, nil
	}
	res, err := m.conn.ExecCtx(ctx, "DELETE FROM search_history WHERE mid = ? AND mtime < ?", mid, threshold)
	if err != nil {
		return 0, fmt.Errorf("search_history Prune delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("search_history Prune RowsAffected: %w", err)
	}
	return n, nil
}
