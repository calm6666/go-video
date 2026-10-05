// docstore.go 是索引投影的写入路径：整篇写入、热度部分更新、删除/降级、批量写。
//
// 迟到与乱序处理（README「一致性策略」）：
//   - 每篇文档带 doc_revision（上游事实的 Unix 毫秒版本，来自 mtime 或版本号）；
//   - 写入前读取索引中的现值，只有 revision >= 现值才允许覆盖（last-write-wins）；
//   - 热度字段独立用 heat_revision 守卫，engagement 事件不重建整篇文档；
//   - 运维回填可用 force 跳过守卫，但必须在日志与任务里留痕。
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go-video/services/search-indexer/internal/esclient"
	"go-video/services/search-indexer/model"
)

// 写入结果标记。
const (
	OutcomeWritten      = "written"       // 已覆盖写入
	OutcomeCreated      = "created"       // 首次写入（索引中原本没有该文档）
	OutcomeSkippedStale = "skipped_stale" // 版本过旧，拒绝覆盖（迟到事件）
	OutcomeMissing      = "missing"       // 正文投影还不存在（热度先到）
	OutcomeDeleted      = "deleted"       // 已物理移除
	OutcomeMarked       = "marked"        // 仅降级为不可检索
	OutcomeNoop         = "noop"          // 无需变更
)

// UpsertResult 单篇写入结果。
type UpsertResult struct {
	Index       string
	Outcome     string
	DocRevision int64
	TookMs      int64
}

// existingProbe 只读取守卫所需的版本字段，避免把整篇文档拉进内存。
type existingProbe struct {
	DocRevision int64 `json:"doc_revision"`
	Heat        struct {
		HeatRevision int64 `json:"heat_revision"`
	} `json:"heat"`
}

// probe 读取索引中的现值版本；found=false 表示文档不存在。
func (r *Repository) probe(ctx context.Context, index, id string) (*existingProbe, bool, error) {
	raw, found, err := r.es.GetSource(ctx, index, id)
	if err != nil {
		return nil, false, fmt.Errorf("search-indexer: probe %s/%s: %w", index, id, err)
	}
	if !found {
		return nil, false, nil
	}
	var p existingProbe
	if err := json.Unmarshal(raw, &p); err != nil {
		// mapping 漂移或脏数据：不能当作「不存在」放过去，必须报错由人工判断。
		return nil, false, fmt.Errorf("search-indexer: unmarshal probe %s/%s: %w", index, id, err)
	}
	return &p, true, nil
}

// UpsertDoc 写入或覆盖一条内容投影。
// force=true 时跳过 revision 守卫（仅运维回填使用）。
func (r *Repository) UpsertDoc(ctx context.Context, doc *esclient.ContentDoc, force bool) (*UpsertResult, error) {
	if doc == nil {
		return nil, model.ErrContentSnapshotRequired
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}

	start := time.Now()
	alias := r.opts.DefaultAlias()
	index, err := r.EnsureActiveIndex(ctx, alias, "bootstrap")
	if err != nil {
		return nil, err
	}

	outcome := OutcomeWritten
	if !force {
		p, found, err := r.probe(ctx, index, doc.ID())
		if err != nil {
			return nil, err
		}
		switch {
		case !found:
			outcome = OutcomeCreated
		case !esclient.ShouldOverwrite(&esclient.ContentDoc{DocRevision: p.DocRevision}, doc):
			// 迟到事件：这是预期路径，返回成功语义但标记 skipped_stale，
			// 让消费侧把 event 标记为 succeeded（重放它只会再次被拒）。
			return &UpsertResult{
				Index:       index,
				Outcome:     OutcomeSkippedStale,
				DocRevision: p.DocRevision,
				TookMs:      time.Since(start).Milliseconds(),
			}, nil
		}
	}

	if err := r.es.IndexDoc(ctx, index, doc.ID(), doc); err != nil {
		return nil, err
	}
	return &UpsertResult{
		Index:       index,
		Outcome:     outcome,
		DocRevision: doc.DocRevision,
		TookMs:      time.Since(start).Milliseconds(),
	}, nil
}

// PatchHeat 只更新热度字段（engagement.action.v1 走这条路，不重建整篇文档）。
// 正文尚未入库时返回 OutcomeMissing，由消费侧退避重试，等 content.published 到齐。
func (r *Repository) PatchHeat(ctx context.Context, contentID int64, contentType int32, heat esclient.Heat, force bool) (*UpsertResult, error) {
	start := time.Now()
	alias := r.opts.DefaultAlias()
	index, err := r.ResolveWriteIndex(ctx, alias)
	if err != nil {
		if errors.Is(err, model.ErrVersionNotFound) {
			return &UpsertResult{Outcome: OutcomeMissing, TookMs: time.Since(start).Milliseconds()}, nil
		}
		return nil, err
	}

	id := esclient.DocID(contentType, contentID)
	if !force {
		p, found, err := r.probe(ctx, index, id)
		if err != nil {
			return nil, err
		}
		if !found {
			return &UpsertResult{Index: index, Outcome: OutcomeMissing, TookMs: time.Since(start).Milliseconds()}, nil
		}
		if !esclient.ShouldPatchHeat(p.Heat.HeatRevision, heat.HeatRevision) {
			return &UpsertResult{
				Index:       index,
				Outcome:     OutcomeSkippedStale,
				DocRevision: p.DocRevision,
				TookMs:      time.Since(start).Milliseconds(),
			}, nil
		}
	}

	applied, err := r.es.UpdatePartial(ctx, index, id, map[string]interface{}{"heat": heat}, r.opts.RetryOnConflict)
	if err != nil {
		return nil, err
	}
	if !applied {
		return &UpsertResult{Index: index, Outcome: OutcomeMissing, TookMs: time.Since(start).Milliseconds()}, nil
	}
	return &UpsertResult{Index: index, Outcome: OutcomeWritten, TookMs: time.Since(start).Milliseconds()}, nil
}

// DeleteOutcome 删除/降级结果。
type DeleteOutcome struct {
	Index    string
	Deleted  bool
	Outcome  string
	DocCount int64
}

// 下架原因 → 索引状态。
func reasonToState(reason string) int32 {
	switch reason {
	case "expired", "rights_expired":
		return esclient.StateExpired
	case "offline":
		return esclient.StateOffline
	default: // deleted / copyright_takedown / 其它
		return esclient.StateDeleted
	}
}

// DeleteContent 移除或降级一条投影。
//
// purge=false 时只做状态降级（保留投影用于审计与恢复），revision 取
// max(现值+1, 当前毫秒) 保证降级不会被随后到达的旧版本事件覆盖。
// CDN/搜索投影一致性：本方法只负责搜索投影；缓存与 CDN 失效由拥有事实的
// 服务（video/catalog/playback）按各自职责处理，见 README「下架链路」。
func (r *Repository) DeleteContent(ctx context.Context, contentID int64, contentType int32, purge bool, reason string) (*DeleteOutcome, error) {
	if contentID <= 0 {
		return nil, model.ErrInvalidContentID
	}
	alias := r.opts.DefaultAlias()
	index, err := r.ResolveWriteIndex(ctx, alias)
	if err != nil {
		if errors.Is(err, model.ErrVersionNotFound) {
			// 从未建过索引 → 没有投影可删，视为幂等成功。
			return &DeleteOutcome{Outcome: OutcomeNoop}, nil
		}
		return nil, err
	}

	id := esclient.DocID(contentType, contentID)
	if purge {
		deleted, err := r.es.DeleteDoc(ctx, index, id)
		if err != nil {
			return nil, err
		}
		outcome := OutcomeNoop
		if deleted {
			outcome = OutcomeDeleted
		}
		return &DeleteOutcome{Index: index, Deleted: deleted, Outcome: outcome}, nil
	}

	p, found, err := r.probe(ctx, index, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return &DeleteOutcome{Index: index, Outcome: OutcomeMissing}, nil
	}
	revision := maxInt64(p.DocRevision+1, model.NowMilli())
	applied, err := r.es.UpdatePartial(ctx, index, id,
		esclient.StatePatchBody(reasonToState(reason), revision), r.opts.RetryOnConflict)
	if err != nil {
		return nil, err
	}
	if !applied {
		return &DeleteOutcome{Index: index, Outcome: OutcomeMissing}, nil
	}
	return &DeleteOutcome{Index: index, Outcome: OutcomeMarked}, nil
}

// BulkApply 批量写入（消费侧按批投递事件时使用）。
// 按 esclient.MaxBulkActions() 自动切分，返回聚合结果，逐项失败不掩盖。
func (r *Repository) BulkApply(ctx context.Context, alias string, ops []esclient.BulkOp) (*esclient.BulkResult, error) {
	if len(ops) == 0 {
		return &esclient.BulkResult{}, nil
	}
	index, err := r.ResolveWriteIndex(ctx, alias)
	if err != nil {
		return nil, err
	}
	maxChunk := r.es.MaxBulkActions()
	if maxChunk <= 0 {
		maxChunk = 500
	}

	agg := &esclient.BulkResult{Items: make([]esclient.BulkItem, 0, len(ops))}
	for start := 0; start < len(ops); start += maxChunk {
		end := start + maxChunk
		if end > len(ops) {
			end = len(ops)
		}
		res, err := r.es.Bulk(ctx, index, ops[start:end])
		if err != nil {
			// 已经成功的分片不回滚：整体返回错误，让上层按事件重试，
			// doc_revision 守卫保证重复投递安全。
			return agg, err
		}
		agg.Took += res.Took
		agg.Errors = agg.Errors || res.Errors
		agg.Items = append(agg.Items, res.Items...)
	}
	return agg, nil
}

// RefreshIndex 显式 refresh（重建收尾、本地验证时使用）。
func (r *Repository) RefreshIndex(ctx context.Context, index string) error {
	return r.es.Refresh(ctx, index)
}

// ReindexSlice 转调 OpenSearch 切片重建（重建 runner 使用）。
func (r *Repository) ReindexSlice(ctx context.Context, req esclient.ReindexSliceReq) (*esclient.ReindexResult, error) {
	return r.es.ReindexSlice(ctx, req)
}

// IndexExists 判断索引是否存在。
func (r *Repository) IndexExists(ctx context.Context, index string) (bool, error) {
	return r.es.IndexExists(ctx, index)
}

// CountIndex 返回索引文档数；索引不存在时返回 (0, false, nil)。
func (r *Repository) CountIndex(ctx context.Context, index string) (int64, bool, error) {
	cnt, err := r.es.Count(ctx, index)
	if err != nil {
		if errors.Is(err, esclient.ErrNotFound) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return cnt, true, nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
