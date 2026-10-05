// indexstore.go 管理「哪个物理索引承接写入/查询」的登记与别名切换。
//
// 零停机重建流程（README 有完整图示）：
//  1. SubmitRebuildTask 预分配 target_index 并建索引（不影响线上查询）；
//  2. RebuildRunner 按 content_id 区间切片 _reindex，推进 cursor_value；
//  3. GetIndexHealth 校验目标索引 doc 数；
//  4. SwitchAlias 原子切别名（expected_current 乐观校验），旧索引转 retiring；
//  5. 观察期后旧索引转 history，由运维清理。
package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go-video/services/search-indexer/internal/esclient"
	"go-video/services/search-indexer/model"
)

// EnsureActiveIndex 返回别名当前的写入索引；首次使用时创建 v1 索引并登记。
// 并发首次写入靠 uniq_index_name + 重新读取收敛，不依赖分布式锁。
func (r *Repository) EnsureActiveIndex(ctx context.Context, alias, createdBy string) (string, error) {
	alias = r.opts.Alias(alias)
	if v, err := r.versionMd.FindActive(ctx, alias); err != nil {
		return "", err
	} else if v != nil {
		return v.IndexName, nil
	}

	indexName := r.opts.NewIndexName(alias)
	exists, err := r.es.CreateIndex(ctx, indexName, r.opts.SchemaVersion)
	if err != nil {
		return "", fmt.Errorf("search-indexer: create index %s: %w", indexName, err)
	}
	if exists {
		// 极小概率撞名（同秒重建），换个后缀重试一次。
		indexName = fmt.Sprintf("%s_%s", indexName, strconv.FormatInt(time.Now().UnixNano()%1000, 10))
		if _, err := r.es.CreateIndex(ctx, indexName, r.opts.SchemaVersion); err != nil {
			return "", fmt.Errorf("search-indexer: retry create index %s: %w", indexName, err)
		}
	}

	now := model.NowUnix()
	registered, err := r.versionMd.Insert(ctx, &model.SearchIndexVersion{
		Alias:         alias,
		IndexName:     indexName,
		SchemaVersion: r.opts.SchemaVersion,
		DocCount:      0,
		State:         model.VersionStateActive,
		CreatedBy:     createdBy,
		Ctime:         now,
		Mtime:         now,
	})
	if err != nil {
		return "", err
	}
	if !registered {
		// 另一实例已抢先登记，读取它选定的索引，避免两个写入目标并存。
		if v, err := r.versionMd.FindActive(ctx, alias); err != nil {
			return "", err
		} else if v != nil {
			r.cache.DelActiveIndex(ctx, alias)
			return v.IndexName, nil
		}
		return "", fmt.Errorf("search-indexer: index %s registered but not active", indexName)
	}
	r.cache.DelActiveIndex(ctx, alias)
	return indexName, nil
}

// ResolveWriteIndex 只读解析当前写入索引；未登记时返回 model.ErrVersionNotFound。
// 热度更新、删除、批量写用它：没有正文投影时不应该顺手建索引。
func (r *Repository) ResolveWriteIndex(ctx context.Context, alias string) (string, error) {
	alias = r.opts.Alias(alias)
	if idx, ok := r.cache.GetActiveIndex(ctx, alias); ok {
		return idx, nil
	}
	v, err := r.versionMd.FindActive(ctx, alias)
	if err != nil {
		return "", err
	}
	if v == nil {
		return "", fmt.Errorf("%w: alias=%s", model.ErrVersionNotFound, alias)
	}
	r.cache.SetActiveIndex(ctx, alias, v.IndexName)
	return v.IndexName, nil
}

// EnsureIndex 幂等创建物理索引（重建任务预分配目标索引时调用）。
func (r *Repository) EnsureIndex(ctx context.Context, alias, indexName, createdBy string) error {
	if indexName == "" {
		indexName = r.opts.NewIndexName(alias)
	}
	if _, err := r.es.CreateIndex(ctx, indexName, r.opts.SchemaVersion); err != nil {
		return fmt.Errorf("search-indexer: ensure index %s: %w", indexName, err)
	}
	now := model.NowUnix()
	if _, err := r.versionMd.Insert(ctx, &model.SearchIndexVersion{
		Alias:         r.opts.Alias(alias),
		IndexName:     indexName,
		SchemaVersion: r.opts.SchemaVersion,
		DocCount:      0,
		// 重建出来的索引在建好前不能承接写入，一律登记为 retiring（未激活）。
		State:     model.VersionStateRetiring,
		CreatedBy: createdBy,
		Ctime:     now,
		Mtime:     now,
	}); err != nil {
		return err
	}
	return nil
}

// ListIndexVersions 返回别名（空表示全部）下的索引版本登记。
func (r *Repository) ListIndexVersions(ctx context.Context, alias string) ([]*model.SearchIndexVersion, error) {
	if alias == "" {
		return r.versionMd.ListAll(ctx)
	}
	return r.versionMd.ListByAlias(ctx, r.opts.Alias(alias))
}

// SwitchResult 别名切换结果。
type SwitchResult struct {
	Alias         string
	PreviousIndex string
	CurrentIndex  string
	DocCount      int64
	RecordState   string
	Noop          bool
}

// SwitchAlias 把查询别名切到 targetIndex，带 expected_current 乐观校验。
//
// 顺序很关键：先在 OpenSearch 原子切换别名（查询侧生效），再更新本服务登记表。
// 若登记表更新失败，索引切换仍然成立，GetIndexHealth 会暴露不一致，由运维补偿；
// 反向顺序会造成「库里说切了但查询还在老索引」的静默数据丢失。
func (r *Repository) SwitchAlias(ctx context.Context, alias, targetIndex, expectedCurrent string, skipHealthCheck bool) (*SwitchResult, error) {
	alias = r.opts.Alias(alias)
	if targetIndex == "" {
		return nil, model.ErrTargetIndexNotFound
	}
	// 约定：同一别名的所有物理索引都以 "<alias>_" 开头（见 Options.NewIndexName）。
	// 拒绝把别名指到无关索引，避免误操作把查询打到别人的索引上。
	if !strings.HasPrefix(strings.ToLower(targetIndex), alias+"_") {
		return nil, fmt.Errorf("%w: %s 不属于别名 %s", model.ErrIndexNameMismatch, targetIndex, alias)
	}
	if expectedCurrent == targetIndex {
		return nil, fmt.Errorf("search-indexer: %w: expected_current 与 target_index 相同", model.ErrAliasMismatch)
	}

	ok, err := r.cache.AcquireLock(ctx, fmt.Sprintf(keyAliasLock, alias), ttlAliasLock)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("search-indexer: alias %s 正在被其它实例切换，请稍后重试", alias)
	}
	defer r.cache.ReleaseLock(ctx, fmt.Sprintf(keyAliasLock, alias))

	exists, err := r.es.IndexExists(ctx, targetIndex)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", model.ErrTargetIndexNotFound, targetIndex)
	}

	var docCount int64
	if docCount, err = r.es.Count(ctx, targetIndex); err != nil {
		if !errors.Is(err, esclient.ErrNotFound) {
			return nil, err
		}
		docCount = 0
	}
	if docCount == 0 && !skipHealthCheck {
		return nil, fmt.Errorf("%w: %s 文档数为 0，拒绝把空索引上线（确认要切请置 skip_health_check=true）",
			model.ErrTargetIndexNotFound, targetIndex)
	}

	current, err := r.es.AliasTargets(ctx, alias)
	if err != nil {
		if !errors.Is(err, esclient.ErrNotFound) {
			return nil, err
		}
		// 别名尚未建立：首次挂载，要求调用方明确 expected_current 为空。
		current = nil
	}
	plan, err := esclient.PlanAliasSwitch(alias, targetIndex, current, expectedCurrent)
	if err != nil {
		return nil, err
	}

	if !plan.Noop {
		if err := r.es.ApplyAliasActions(ctx, plan.Actions); err != nil {
			return nil, fmt.Errorf("search-indexer: apply alias actions for %s: %w", alias, err)
		}
	}

	// 登记表：确保目标索引已登记，再切换 active，并把被替换的索引转 retiring。
	if err := r.ensureRegistered(ctx, alias, targetIndex, docCount); err != nil {
		return nil, err
	}
	prev := plan.PreviousIndex
	if expectedCurrent != "" {
		prev = expectedCurrent
	}
	if err := r.versionMd.SwitchActive(ctx, alias, targetIndex, expectedCurrent, model.VersionStateRetiring, model.NowUnix()); err != nil {
		return nil, fmt.Errorf("search-indexer: 索引切换已在 OpenSearch 生效，但登记表更新失败（需人工补偿）: %w", err)
	}
	// 之前处于 retiring 但已被本轮回替换的索引保持 retiring，等待观察期后转 history。
	r.cache.DelActiveIndex(ctx, alias)

	return &SwitchResult{
		Alias:         alias,
		PreviousIndex: prev,
		CurrentIndex:  targetIndex,
		DocCount:      docCount,
		RecordState:   model.VersionStateActive,
		Noop:          plan.Noop,
	}, nil
}

// ensureRegistered 保证索引在 search_index_version 有登记行（切换前置条件）。
func (r *Repository) ensureRegistered(ctx context.Context, alias, indexName string, docCount int64) error {
	v, err := r.versionMd.FindByIndexName(ctx, indexName)
	if err != nil {
		return err
	}
	now := model.NowUnix()
	if v == nil {
		if _, err := r.versionMd.Insert(ctx, &model.SearchIndexVersion{
			Alias:         alias,
			IndexName:     indexName,
			SchemaVersion: r.opts.SchemaVersion,
			DocCount:      docCount,
			State:         model.VersionStateRetiring,
			CreatedBy:     "switch",
			Ctime:         now,
			Mtime:         now,
		}); err != nil {
			return err
		}
		return nil
	}
	return r.versionMd.UpdateDocCount(ctx, indexName, docCount, now)
}

// MarkIndexHistory 把 retiring 索引转 history（观察期结束后由运维调用）。
func (r *Repository) MarkIndexHistory(ctx context.Context, indexName string) error {
	return r.versionMd.UpdateState(ctx, indexName, model.VersionStateHistory, model.NowUnix())
}

// AliasHealth 单个别名的健康快照。
type AliasHealth struct {
	Alias         string
	ActiveIndex   string
	SchemaVersion string
	DocCount      int64
	IndexExists   bool
	Health        string
	State         string
	AliasTargets  []string
}

// Health 汇总各别名的登记状态与 OpenSearch 实测状态。
// 单个索引读失败不会中断整体巡检：把 health 标为 unknown 并继续，
// 因为巡检接口本身就是排障入口（AGENTS.md §9 不得吞错，这里把错误写进结果）。
func (r *Repository) Health(ctx context.Context, alias string) ([]AliasHealth, error) {
	var (
		versions []*model.SearchIndexVersion
		err      error
	)
	if alias == "" {
		versions, err = r.versionMd.ListAll(ctx)
	} else {
		versions, err = r.versionMd.ListByAlias(ctx, r.opts.Alias(alias))
	}
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(versions))
	out := make([]AliasHealth, 0, len(versions))
	now := model.NowUnix()
	for _, v := range versions {
		if v.State != model.VersionStateActive {
			continue
		}
		if seen[v.Alias] {
			continue
		}
		seen[v.Alias] = true

		h := AliasHealth{
			Alias:         v.Alias,
			ActiveIndex:   v.IndexName,
			SchemaVersion: v.SchemaVersion,
			State:         v.State,
			DocCount:      -1,
			Health:        "unknown",
		}
		exists, err := r.es.IndexExists(ctx, v.IndexName)
		if err != nil {
			h.Health = "error: " + err.Error()
		} else {
			h.IndexExists = exists
			cnt, err := r.es.Count(ctx, v.IndexName)
			if err != nil {
				if errors.Is(err, esclient.ErrNotFound) {
					h.Health = "missing"
				} else {
					h.Health = "error: " + err.Error()
				}
			} else {
				h.DocCount = cnt
				st, err := r.es.ClusterHealth(ctx, v.IndexName)
				if err != nil {
					h.Health = "error: " + err.Error()
				} else {
					h.Health = st
				}
				// doc 数快照回写登记表，失败只记日志（不影响巡检返回）。
				_ = r.versionMd.UpdateDocCount(ctx, v.IndexName, cnt, now)
			}
		}
		if targets, err := r.es.AliasTargets(ctx, v.Alias); err == nil {
			h.AliasTargets = targets
		}
		out = append(out, h)
	}
	return out, nil
}
