package repository

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/search-query/model"
)

// historyKeepRows 单个用户保留的历史条数上限（超出部分物理删除，隐私最小化）。
const historyKeepRows = 300

// HistoryPage 一页搜索历史。
type HistoryPage struct {
	Rows       []*model.SearchHistory
	NextCursor string
	HasMore    bool
}

// RecordHistory 幂等记录一条搜索历史，并把该用户历史规模裁剪到上限。
//
// 只在登录态（mid>0）下写入；游客不建历史行（不采集可关联的设备标识）。
// 调用方应以“尽力而为”处理错误：搜索已经成功，记录失败不应变成用户可见错误，
// 但必须落日志（见 logic 层）。
func (r *Repository) RecordHistory(ctx context.Context, mid int64, keyword, platform string) error {
	if mid <= 0 {
		return model.ErrInvalidMid
	}
	if keyword == "" {
		return model.ErrInvalidKeyword
	}
	if err := r.historyMd.Upsert(ctx, &model.SearchHistory{
		Mid:         mid,
		Keyword:     keyword,
		KeywordHash: model.KeywordHash(keyword),
		Platform:    platform,
		State:       model.HistoryStateNormal,
	}); err != nil {
		return err
	}
	if _, err := r.historyMd.Prune(ctx, mid, historyKeepRows); err != nil {
		// 裁剪失败不影响本次搜索，但会让历史无限增长：显式记录。
		logx.Errorf("search-query/history: prune mid=%d err=%v", mid, err)
	}
	return nil
}

// ListHistory 按 (mtime, id) 倒序取一页历史；cursor 为空表示首页。
// 多取一条用于判定 hasMore，避免 COUNT 查询。
func (r *Repository) ListHistory(ctx context.Context, mid int64, cursor string, limit int32) (*HistoryPage, error) {
	if mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if limit <= 0 {
		return nil, model.ErrInvalidPage
	}
	beforeMtime, beforeID, err := DecodeKeysetCursor(cursor)
	if err != nil {
		return nil, err
	}
	rows, err := r.historyMd.ListByKeyset(ctx, mid, beforeMtime, beforeID, limit+1)
	if err != nil {
		return nil, err
	}
	page := &HistoryPage{}
	if int32(len(rows)) > limit {
		rows = rows[:limit]
		page.HasMore = true
	}
	page.Rows = rows
	if n := len(rows); n > 0 {
		last := rows[n-1]
		next, err := EncodeKeysetCursor(last.Mtime, last.Id)
		if err != nil {
			return nil, fmt.Errorf("search-query/history: encode cursor: %w", err)
		}
		page.NextCursor = next
	}
	return page, nil
}

// DeleteHistory 物理删除用户某个词的搜索历史，返回删除行数。
// 0 行表示不存在（重复调用天然幂等）。历史不参与结果缓存，无需额外失效动作。
func (r *Repository) DeleteHistory(ctx context.Context, mid int64, keyword string) (int64, error) {
	if mid <= 0 {
		return 0, model.ErrInvalidMid
	}
	if keyword == "" {
		return 0, model.ErrInvalidKeyword
	}
	return r.historyMd.DeleteKeyword(ctx, mid, keyword)
}

// ClearHistory 物理清空用户全部搜索历史，返回删除行数。
func (r *Repository) ClearHistory(ctx context.Context, mid int64) (int64, error) {
	if mid <= 0 {
		return 0, model.ErrInvalidMid
	}
	return r.historyMd.DeleteAll(ctx, mid)
}
