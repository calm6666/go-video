package repository

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/search-query/model"
)

// HotKeywordsOutcome 热词查询结果。
type HotKeywordsOutcome struct {
	Rows       []*model.SearchHotKeyword
	SnapshotAt int64 // 本批快照时间，0 表示还没有快照
	FromCache  bool
}

// HotKeywords 读取某 scope 的热词快照（Redis 短缓存 + DB 回源）。
//
// 语义澄清：search_hot_keyword 是“快照表”，其内容由离线/定时聚合任务从
// search_query_log 计算后写入（README 记录写入方与刷新周期）。
// 本方法只读快照，不在查询链路上写热度事实。
// 快照为空时返回空列表 + SnapshotAt=0，由调用方决定是否回退到分区默认榜单，
// 服务端不编造热词。
func (r *Repository) HotKeywords(ctx context.Context, scope string, limit int32) (*HotKeywordsOutcome, error) {
	normalized, err := model.NormalizeScope(scope)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, fmt.Errorf("%w: limit must be positive", model.ErrInvalidPage)
	}

	if r.cache != nil {
		rows, hit, cerr := r.cache.GetHot(ctx, normalized)
		if cerr != nil {
			logx.Errorf("search-query/hot: read cache scope=%s err=%v", normalized, cerr)
		}
		if hit {
			return &HotKeywordsOutcome{
				Rows:       r.applyBlockFilter(ctx, pickRows(rows, limit)),
				SnapshotAt: snapshotAtOf(rows),
				FromCache:  true,
			}, nil
		}
	}

	// 多取一些再屏蔽，保证屏蔽后仍能凑满 limit（词表通常远小于候选量）。
	fetch := limit + blockFilterExtra
	rows, err := r.hotMd.ListByScope(ctx, normalized, fetch)
	if err != nil {
		return nil, err
	}
	snapshotAt := snapshotAtOf(rows)
	if r.cache != nil {
		if err := r.cache.SetHot(ctx, normalized, rows, r.cfg.CacheTTLSeconds); err != nil {
			logx.Errorf("search-query/hot: write cache scope=%s err=%v", normalized, err)
		}
	}
	return &HotKeywordsOutcome{Rows: r.applyBlockFilter(ctx, pickRows(rows, limit)), SnapshotAt: snapshotAt}, nil
}

// blockFilterExtra 屏蔽过滤预留的候选余量。
const blockFilterExtra = 20

// applyBlockFilter 出口过滤：去掉命中屏蔽词的热词/联想词。
func (r *Repository) applyBlockFilter(ctx context.Context, rows []*model.SearchHotKeyword) []*model.SearchHotKeyword {
	if len(rows) == 0 {
		return rows
	}
	set := r.blockedWordSet(ctx)
	if len(set) == 0 {
		return rows
	}
	out := make([]*model.SearchHotKeyword, 0, len(rows))
	for _, row := range rows {
		if _, blocked := set[row.Keyword]; blocked {
			continue
		}
		out = append(out, row)
	}
	return out
}

// pickRows 截断到 limit（limit 大于长度时原样返回）。
func pickRows(rows []*model.SearchHotKeyword, limit int32) []*model.SearchHotKeyword {
	if limit > 0 && int32(len(rows)) > limit {
		return rows[:limit]
	}
	return rows
}

// snapshotAtOf 取快照时间（列表已按分数倒序，快照时间取最大值更稳）。
func snapshotAtOf(rows []*model.SearchHotKeyword) int64 {
	var ts int64
	for _, row := range rows {
		if row.SnapshotAt > ts {
			ts = row.SnapshotAt
		}
	}
	return ts
}
