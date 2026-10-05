// Package repository 是 recommend-rank 的数据访问层。
//
// 它组合 5 张表（模型版本 / 特征配置 / 实验变体 / 实验分桶 / 决策摘要）与只读快照缓存，
// 并为下游能力（特征、行为统计、内容安全、运营干预位）提供接口 + 显式 stub
// （见 downstream.go），使 logic 层既不直接碰 sqlx，也不 import 任何兄弟服务的 rpc 包。
//
// 依赖故障语义（AGENTS.md §9：必须显式定义，不允许伪造成功）：
//   - 读模型版本/实验配置失败 → logic 按 model.DegradeReason* 标注降级并回退召回原序；
//   - 决策摘要写入失败 → 只记错误日志，不翻转已产出的排序结果（审计缺行由告警暴露）；
//   - 下游 stub 一律返回 model.ErrNotImplemented，绝不返回「看起来正常」的空特征。
package repository

import (
	"context"
	"errors"
	"fmt"

	"go-video/services/recommend-rank/model"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// Options 是仓库层运行参数，由 svc 从 config.RankConf 映射，
// 使本包不依赖 internal/config（便于离线单测）。
type Options struct {
	// DefaultModelKey 是请求未指定 model_key 时使用的逻辑模型名。
	DefaultModelKey string
	// BucketCount 是分桶空间大小（落库口径，默认 1000）。
	BucketCount int32
	// RunningExperimentScanLimit 一次读取 RUNNING 变体的条数上限。
	RunningExperimentScanLimit int
	// LayerVariantScanLimit 同层重叠校验读取的变体条数上限。
	LayerVariantScanLimit int
	// AssignmentScanLimit 分桶主体核对单页条数上限。
	AssignmentScanLimit int
	// MaxDigestAids 决策摘要 top_aids 保留条数。
	MaxDigestAids int
	// ArchiveBatchSize 单批归档/删除行数上限。
	ArchiveBatchSize int
	// DecisionRetentionDays 决策摘要保留天数。
	DecisionRetentionDays int
	// AssignmentRetentionDays 分桶记录保留天数。
	AssignmentRetentionDays int
}

// Repository 是 recommend-rank 的数据访问入口。
type Repository struct {
	conn  sqlx.SqlConn
	cache *Cache
	opt   Options

	// downstream 是外部读依赖（特征/行为/安全/运营参数），当前全部为 stub。
	downstream Downstream

	modelMd   model.RankModelVersionModel
	featureMd model.RankFeatureConfigModel
	expMd     model.RankExperimentModel
	assignMd  model.RankExperimentAssignmentModel
	decision  model.RankDecisionLogModel
}

// New 构造 Repository。
//
// 这里只做对象构造，不建立数据库/Redis 连接（go-zero 是惰性连接），
// 因此服务可以在依赖未就绪时启动，并由每个用例显式暴露存储错误。
// 下游依赖固定先用 stub（NewStubDownstream），第二轮由 SetDownstream 替换成真实 client。
func New(rds *redis.Redis, conn sqlx.SqlConn, opt Options) *Repository {
	return &Repository{
		conn:       conn,
		cache:      NewCache(rds),
		opt:        opt,
		downstream: NewStubDownstream(),
		modelMd:    model.NewRankModelVersionModel(conn),
		featureMd:  model.NewRankFeatureConfigModel(conn),
		expMd:      model.NewRankExperimentModel(conn),
		assignMd:   model.NewRankExperimentAssignmentModel(conn),
		decision:   model.NewRankDecisionLogModel(conn),
	}
}

// Downstream 返回外部读依赖集合（logic 只经由此访问下游）。
func (r *Repository) Downstream() Downstream { return r.downstream }

// SetDownstream 替换外部读依赖，仅在 svc 装配期调用。
// nil 字段保持原值，避免误把某条依赖关掉后静默生效（必须显式传 stub 才关闭）。
func (r *Repository) SetDownstream(d Downstream) {
	if d.Features != nil {
		r.downstream.Features = d.Features
	}
	if d.Behaviors != nil {
		r.downstream.Behaviors = d.Behaviors
	}
	if d.Safety != nil {
		r.downstream.Safety = d.Safety
	}
	if d.OpsConfigs != nil {
		r.downstream.OpsConfigs = d.OpsConfigs
	}
}

// Ping 检查 Redis 与 MySQL 连通性（健康探针与启动自检用）。
func (r *Repository) Ping(ctx context.Context) error {
	if err := r.cache.Ping(ctx); err != nil {
		return err
	}
	var ok int
	if err := r.conn.QueryRowCtx(ctx, &ok, "SELECT 1"); err != nil {
		return fmt.Errorf("recommend-rank/repository: mysql ping failed: %w", err)
	}
	return nil
}

// Conn 暴露事务入口：模型激活需要「旧 ACTIVE 置 RETIRED + 新版本置 ACTIVE」原子完成。
func (r *Repository) Conn() sqlx.SqlConn { return r.conn }

// Cache 暴露只读快照缓存（logic 只读不写事实）。
func (r *Repository) Cache() *Cache { return r.cache }

// Options 返回运行参数快照（logic 需要上限，而不该自己复制一份配置结构）。
func (r *Repository) Options() Options { return r.opt }

// --- 模型版本 ---

// ModelVersions 返回模型版本表访问对象。
func (r *Repository) ModelVersions() model.RankModelVersionModel { return r.modelMd }

// ResolveModelKey 把空 model_key 归一到服务配置的默认模型名。
func (r *Repository) ResolveModelKey(modelKey string) string {
	if modelKey == "" {
		return r.opt.DefaultModelKey
	}
	return modelKey
}

// ActiveModel 返回某 model_key 当前 ACTIVE 版本；modelKey 为空时取默认模型名。
// 没有 ACTIVE 版本时返回 model.ErrNoActiveModel —— 调用方必须据此降级，
// 不允许「没有模型就用默认分」这种伪成功路径。
func (r *Repository) ActiveModel(ctx context.Context, modelKey string) (*model.RankModelVersion, error) {
	return r.modelMd.FindActive(ctx, r.ResolveModelKey(modelKey))
}

// ActivateModel 在同一事务里把旧 ACTIVE 置 RETIRED 并激活目标版本，
// 返回切换前的 ACTIVE 版本号与是否真正切换。
//
// 保护点：ActivateTx 的 SQL 自带「当前 state=READY」条件，并发下两个实例
// 同时激活同一版本只有一个拿到 changed=true，另一个拿到 ErrModelStateTransition，
// 不会出现「两个 ACTIVE 版本」或「重复激活被当成成功」。
func (r *Repository) ActivateModel(ctx context.Context, id int64, modelKey, operator, reason string) (string, bool, error) {
	if id == 0 || modelKey == "" {
		return "", false, model.ErrModelNotFound
	}
	if operator == "" {
		return "", false, model.ErrOperatorRequired
	}
	if reason == "" {
		return "", false, model.ErrReasonRequired
	}
	var previous string
	changed := false
	err := r.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		active, err := r.modelMd.FindActiveTx(ctx, session, modelKey)
		switch {
		case errors.Is(err, model.ErrNoActiveModel):
		case err != nil:
			return err
		case active.ID == id:
			previous = active.Version // 已是 ACTIVE：幂等命中，不重复推进
			return nil
		default:
			previous = active.Version
			if _, err := r.modelMd.DeactivateTx(ctx, session, modelKey, id, model.NowUnix()); err != nil {
				return err
			}
		}
		ok, err := r.modelMd.ActivateTx(ctx, session, id, previous, operator, model.NowUnix())
		if err != nil {
			return err
		}
		if !ok {
			return model.ErrModelStateTransition
		}
		changed = true
		return nil
	})
	if err != nil {
		return previous, false, err
	}
	if changed {
		r.cache.InvalidateModelConfig(ctx, modelKey)
	}
	return previous, changed, nil
}

// --- 特征配置 ---

// FeatureConfigs 返回特征配置表访问对象。
func (r *Repository) FeatureConfigs() model.RankFeatureConfigModel { return r.featureMd }

// DisableFeatureConfig 停用特征配置版本，先确认没有 ACTIVE/READY 模型在引用它。
// 被引用时返回 model.ErrFeatureConfigInUse：停用会让在线排序无特征可读，
// 这类「配置层自伤」必须在写入前拦住，而不是等线上降级才发现。
func (r *Repository) DisableFeatureConfig(ctx context.Context, configVersion, operator, reason string) (bool, error) {
	if operator == "" {
		return false, model.ErrOperatorRequired
	}
	if reason == "" {
		return false, model.ErrReasonRequired
	}
	referenced, err := r.modelMd.CountByFeatureConfig(ctx, configVersion,
		[]int32{model.ModelStateActive, model.ModelStateReady})
	if err != nil {
		return false, err
	}
	if referenced > 0 {
		return false, model.ErrFeatureConfigInUse
	}
	return r.featureMd.UpdateState(ctx, configVersion, model.FeatureStateEnabled, model.FeatureStateDisabled, operator, reason)
}

// --- 实验与分桶 ---

// Experiments 返回实验变体表访问对象。
func (r *Repository) Experiments() model.RankExperimentModel { return r.expMd }

// Assignments 返回实验分桶表访问对象。
func (r *Repository) Assignments() model.RankExperimentAssignmentModel { return r.assignMd }

// BucketCount 返回落库口径的分桶空间大小。
func (r *Repository) BucketCount() int32 { return r.opt.BucketCount }

// RunningExperiments 返回当前生效的 RUNNING 变体（条数受 RunningExperimentScanLimit 约束，
// 超限的部分不返回，因为「变体数量超过扫描上限」本身就是需要人工介入的配置异常）。
func (r *Repository) RunningExperiments(ctx context.Context, at int64) ([]*model.RankExperiment, error) {
	return r.expMd.ListRunning(ctx, at, r.opt.RunningExperimentScanLimit)
}

// BucketOverlapConflict 检查目标桶区间是否与同层其它变体重叠。
// 同层互斥是实验结论可信的前提：区间重叠会让同一主体命中两个变体，
// 所以必须在 Upsert 前证明不重叠，而不是靠「大概率不重叠」的约定。
func (r *Repository) BucketOverlapConflict(ctx context.Context, layerKey string, start, end int32,
	excludeVariant string) (*model.RankExperiment, bool, error) {
	rows, err := r.expMd.ListOverlappingBuckets(ctx, layerKey, start, end, excludeVariant, r.opt.LayerVariantScanLimit)
	if err != nil {
		return nil, false, err
	}
	if len(rows) == 0 {
		return nil, false, nil
	}
	return rows[0], true, nil
}

// ListAssignments 按变体分页核对已分桶主体，limit 超过 AssignmentScanLimit 时按上限截断
// （核对类读路径截断是可接受的，因为它总是带 offset 重查；写路径才坚持「超限报错」）。
func (r *Repository) ListAssignments(ctx context.Context, expKey, variantKey string, offset, limit int) ([]*model.RankExperimentAssignment, error) {
	if limit > r.opt.AssignmentScanLimit {
		limit = r.opt.AssignmentScanLimit
	}
	return r.assignMd.ListByVariant(ctx, expKey, variantKey, offset, limit)
}

// --- 决策摘要 ---

// DecisionLogs 返回排序决策摘要表访问对象。
func (r *Repository) DecisionLogs() model.RankDecisionLogModel { return r.decision }

// MaxDigestAids 返回 top_aids 保留条数。
func (r *Repository) MaxDigestAids() int { return r.opt.MaxDigestAids }

// ListDecisions 分页查决策摘要，返回 (rows, hasMore)。
func (r *Repository) ListDecisions(ctx context.Context, q model.DecisionQuery) ([]*model.RankDecisionLog, bool, error) {
	return r.decision.List(ctx, q)
}

// PurgeExpiredDecisionLogs 按保留期挑出待归档行的主键（单次最多 ArchiveBatchSize 行）。
//
// 调用约束：必须先成功归档到对象存储、再拿这些 id 调 DeleteArchivedDecisionLogs。
// 本方法只负责「挑」不负责「删」，避免出现没留证据就丢审计的行径。
// 本期契约没有 prune RPC，只能由运维巡检触发（见 README「已知缺口」）。
func (r *Repository) PurgeExpiredDecisionLogs(ctx context.Context) ([]int64, error) {
	if r.opt.DecisionRetentionDays <= 0 {
		return nil, nil
	}
	cutoff := model.NowUnix() - int64(r.opt.DecisionRetentionDays)*86400
	return r.decision.SelectExpiredBefore(ctx, cutoff, r.opt.ArchiveBatchSize)
}

// DeleteArchivedDecisionLogs 删除已归档的决策摘要行（批量上限受 ArchiveBatchSize 约束）。
func (r *Repository) DeleteArchivedDecisionLogs(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil // 空批次直接返回，不发一次无意义的 DELETE
	}
	if len(ids) > r.opt.ArchiveBatchSize {
		return 0, fmt.Errorf("recommend-rank/repository: archive batch exceeded %d", r.opt.ArchiveBatchSize)
	}
	return r.decision.DeleteExpiredBefore(ctx, ids)
}

// PurgeExpiredAssignments 按保留期分批清理历史分桶记录，返回本次删除行数。
func (r *Repository) PurgeExpiredAssignments(ctx context.Context) (int64, error) {
	if r.opt.AssignmentRetentionDays <= 0 {
		return 0, nil
	}
	cutoff := model.NowUnix() - int64(r.opt.AssignmentRetentionDays)*86400
	return r.assignMd.DeleteOlderThan(ctx, cutoff, r.opt.ArchiveBatchSize)
}
