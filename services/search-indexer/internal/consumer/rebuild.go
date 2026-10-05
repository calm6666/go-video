// rebuild.go 执行索引重建任务（RebuildRunner）。
//
// 关键点：
//   - 重建数据来源是「本服务自己写的投影」（OpenSearch 内部 _reindex），
//     绝不直连 video/catalog 的库表（AGENTS.md §5）；
//   - 按 content_id 半开区间 [from, to) 切片，游标持久化在 search_index_task.cursor_value，
//     进程重启后从断点续跑，同一区间重复执行是幂等覆盖（目标 _id 与源一致）；
//   - 连续 N 个空切片判定结束，避免依赖「最大 content_id」这种需要跨服务查询的信息；
//   - 任务成功不会自动切别名：必须由运维/后台显式调用 SwitchAlias 并带 expected_current，
//     避免重建结果未经校验就影响线上查询。
package consumer

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/search-indexer/internal/esclient"
	"go-video/services/search-indexer/internal/repository"
	"go-video/services/search-indexer/model"
)

// RebuildStore 是重建执行需要的持久化能力，由 *repository.Repository 实现。
type RebuildStore interface {
	ClaimRebuildTask(ctx context.Context) (*model.SearchIndexTask, error)
	UpdateRebuildProgress(ctx context.Context, taskID string, cursor, processed, failed, total int64) error
	FinishRebuildTask(ctx context.Context, taskID, state, errText string) error
	EnsureIndex(ctx context.Context, alias, indexName, createdBy string) error
	ResolveWriteIndex(ctx context.Context, alias string) (string, error)
	ReindexSlice(ctx context.Context, req esclient.ReindexSliceReq) (*esclient.ReindexResult, error)
	RefreshIndex(ctx context.Context, index string) error
	CountIndex(ctx context.Context, index string) (int64, bool, error)
}

// RebuildOptions 重建执行参数。
type RebuildOptions struct {
	SliceSpan            int64
	SliceSize            int
	StopAfterEmptySlices int
	PollInterval         time.Duration
	ErrorWait            time.Duration
}

func (o *RebuildOptions) normalize() {
	if o.SliceSpan <= 0 {
		o.SliceSpan = 100000
	}
	if o.SliceSize <= 0 {
		o.SliceSize = 2000
	}
	if o.StopAfterEmptySlices <= 0 {
		o.StopAfterEmptySlices = 3
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 15 * time.Second
	}
	if o.ErrorWait <= 0 {
		o.ErrorWait = 5 * time.Second
	}
}

// RebuildRunner 重建任务执行器。
type RebuildRunner struct {
	store RebuildStore
	opts  RebuildOptions
}

// NewRebuildRunner 构造执行器。
func NewRebuildRunner(store RebuildStore, opts RebuildOptions) *RebuildRunner {
	opts.normalize()
	return &RebuildRunner{store: store, opts: opts}
}

// ParseCursor 解析游标字符串，非法或 <=0 时回退到 1（content_id 起点）。
func ParseCursor(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || v <= 0 {
		return 1
	}
	return v
}

// SliceWindow 计算一个重建切片区间。
func SliceWindow(cursor, span int64) (from, to int64) {
	if span <= 0 {
		span = 100000
	}
	if cursor <= 0 {
		cursor = 1
	}
	return cursor, cursor + span
}

// ShouldStop 判断连续空切片是否已达到停止阈值。
func ShouldStop(emptySlices, stopAfter int) bool {
	return stopAfter > 0 && emptySlices >= stopAfter
}

// ScopeFilter 把重建范围转成 OpenSearch 查询过滤条件。
// full → nil；content_type → term；partition → typeid 区间。
// 返回错误时任务直接判失败，不允许「以为是全量其实只重建了一部分」。
func ScopeFilter(scope, scopeValue string) (map[string]interface{}, error) {
	switch scope {
	case model.ScopeFull, "":
		return nil, nil
	case model.ScopeContentType:
		n, err := strconv.Atoi(strings.TrimSpace(scopeValue))
		if err != nil || n < 1 || n > 3 {
			return nil, fmt.Errorf("%w: content_type=%q", model.ErrInvalidScope, scopeValue)
		}
		return map[string]interface{}{"term": map[string]interface{}{"content_type": n}}, nil
	case model.ScopePartition:
		parts := strings.Split(scopeValue, "-")
		if len(parts) != 2 {
			return nil, fmt.Errorf("%w: partition=%q", model.ErrInvalidScope, scopeValue)
		}
		lo, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		hi, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil || hi < lo {
			return nil, fmt.Errorf("%w: partition=%q", model.ErrInvalidScope, scopeValue)
		}
		return map[string]interface{}{
			"range": map[string]interface{}{
				"typeid": map[string]interface{}{"gte": lo, "lte": hi},
			},
		}, nil
	default:
		return nil, fmt.Errorf("%w: %q", model.ErrInvalidScope, scope)
	}
}

// RebuildStats 单次任务执行统计。
type RebuildStats struct {
	Processed int64
	Failed    int64
	Total     int64
	Slices    int
	Source    string
	Target    string
}

// finishCtx 返回一个不会被任务 ctx 取消的上下文。
// 进程退出时 ctx 已取消，若仍用它写终态，状态会永远停在 running，
// 因此收尾写入必须脱离取消信号（仍保留 trace 等值）。
func finishCtx(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// fail 把任务判失败并返回原始错误，集中一处避免漏写终态。
func (rr *RebuildRunner) fail(ctx context.Context, taskID string, cause error, stats *RebuildStats) (*RebuildStats, error) {
	if err := rr.store.FinishRebuildTask(finishCtx(ctx), taskID, model.TaskStateFailed, cause.Error()); err != nil {
		logx.WithContext(ctx).Errorf("search-indexer/rebuild: 写失败终态出错 task_id=%s err=%v", taskID, err)
	}
	return stats, cause
}

// ExecuteTask 执行一个已处于 running 状态的任务。
// 返回的 error 表示任务被判失败，已把原因写入 last_error。
func (rr *RebuildRunner) ExecuteTask(ctx context.Context, task *model.SearchIndexTask) (*RebuildStats, error) {
	if task == nil {
		return nil, errors.New("consumer: nil rebuild task")
	}
	logger := logx.WithContext(ctx)
	stats := &RebuildStats{Target: task.TargetIndex}

	filter, err := ScopeFilter(task.Scope, task.ScopeValue)
	if err != nil {
		return rr.fail(ctx, task.TaskID, err, stats)
	}

	// 重建来源固定是「当前别名指向的写索引」：本服务只拥有自己的投影，
	// 不允许指定上游库表作为来源（AGENTS.md §5）。
	// 版本表为空说明从未写过投影（首次部署），此时按空跑成功处理。
	source, err := rr.store.ResolveWriteIndex(ctx, task.Alias)
	if errors.Is(err, model.ErrVersionNotFound) {
		source, err = "", nil
	}
	if err != nil {
		return rr.fail(ctx, task.TaskID, err, stats)
	}
	stats.Source = source

	if err := rr.store.EnsureIndex(ctx, task.Alias, task.TargetIndex, task.TaskID); err != nil {
		return rr.fail(ctx, task.TaskID, err, stats)
	}

	if source == "" || source == task.TargetIndex {
		// 没有任何可重建的投影（首次部署）或目标即来源：任务视为空跑成功，
		// 但不切别名，后续由事件流自然补齐。
		if err := rr.store.RefreshIndex(ctx, task.TargetIndex); err != nil {
			return rr.fail(ctx, task.TaskID, err, stats)
		}
		if err := rr.store.FinishRebuildTask(finishCtx(ctx), task.TaskID, model.TaskStateSucceeded, ""); err != nil {
			return stats, err
		}
		logger.Infof("search-indexer/rebuild: 无来源索引，任务空跑成功 task_id=%s target=%s", task.TaskID, task.TargetIndex)
		return stats, nil
	}

	cursor := ParseCursor(task.CursorValue)
	emptySlices := 0
	for {
		if ctx.Err() != nil {
			// 进程退出：保持 running 会卡住任务，这里标 failed 让运维重提任务；
			// cursor_value 已推进，重新提交可从断点继续。
			_ = rr.store.FinishRebuildTask(finishCtx(ctx), task.TaskID, model.TaskStateFailed, "任务被取消（进程退出），cursor_value 可续跑")
			return stats, ctx.Err()
		}

		from, to := SliceWindow(cursor, rr.opts.SliceSpan)
		res, err := rr.store.ReindexSlice(ctx, esclient.ReindexSliceReq{
			Source: source,
			Dest:   task.TargetIndex,
			From:   from,
			To:     to,
			Size:   rr.opts.SliceSize,
			Filter: filter,
		})
		if err != nil {
			// 失败即停：cursor 已落库，重提任务从断点续跑，不跳过任何区间。
			return rr.fail(ctx, task.TaskID, fmt.Errorf("consumer: reindex slice %d-%d: %w", from, to, err), stats)
		}
		stats.Slices++
		if res != nil {
			stats.Processed += res.Total
			stats.Failed += res.VersionConflicts
			stats.Total = stats.Processed
		}

		hit := int64(0)
		if res != nil {
			hit = res.Total
		}
		if hit == 0 {
			emptySlices++
		} else {
			emptySlices = 0
		}

		cursor = to
		if perr := rr.store.UpdateRebuildProgress(ctx, task.TaskID, cursor, stats.Processed, stats.Failed, stats.Total); perr != nil {
			logger.Errorf("search-indexer/rebuild: 更新进度失败 task_id=%s err=%v", task.TaskID, perr)
		}
		if ShouldStop(emptySlices, rr.opts.StopAfterEmptySlices) {
			break
		}
	}

	if err := rr.store.RefreshIndex(ctx, task.TargetIndex); err != nil {
		return rr.fail(ctx, task.TaskID, err, stats)
	}
	cnt, exists, err := rr.store.CountIndex(ctx, task.TargetIndex)
	if err != nil {
		return rr.fail(ctx, task.TaskID, err, stats)
	}
	if !exists {
		return rr.fail(ctx, task.TaskID, fmt.Errorf("consumer: 目标索引 %s 重建后不可见", task.TargetIndex), stats)
	}
	if stats.Processed == 0 && cnt == 0 {
		logger.Infof("search-indexer/rebuild: 目标索引无文档 task_id=%s target=%s", task.TaskID, task.TargetIndex)
	}
	if err := rr.store.FinishRebuildTask(finishCtx(ctx), task.TaskID, model.TaskStateSucceeded, ""); err != nil {
		return stats, err
	}
	logger.Infof("search-indexer/rebuild: 任务完成 task_id=%s source=%s target=%s processed=%d docs=%d",
		task.TaskID, source, task.TargetIndex, stats.Processed, cnt)
	return stats, nil
}

// Run 周期性抢占并执行任务，直到 ctx 取消。
func (rr *RebuildRunner) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		task, err := rr.store.ClaimRebuildTask(ctx)
		if err != nil {
			logx.WithContext(ctx).Errorf("search-indexer/rebuild: 抢占任务失败 err=%v", err)
			if !sleepCtx(ctx, rr.opts.ErrorWait) {
				return ctx.Err()
			}
			continue
		}
		if task == nil {
			if !sleepCtx(ctx, rr.opts.PollInterval) {
				return ctx.Err()
			}
			continue
		}
		if _, err := rr.ExecuteTask(ctx, task); err != nil && !errors.Is(err, context.Canceled) {
			logx.WithContext(ctx).Errorf("search-indexer/rebuild: 任务失败 task_id=%s err=%v", task.TaskID, err)
		}
	}
}

// repository 接口聚合检查（编译期保证 Repository 满足两个 Store 接口）。
var (
	_ Store        = (*repository.Repository)(nil)
	_ RebuildStore = (*repository.Repository)(nil)
)
