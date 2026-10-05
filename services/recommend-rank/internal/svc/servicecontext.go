// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"go-video/services/recommend-rank/internal/config"
	"go-video/services/recommend-rank/internal/repository"
	"go-video/services/recommend-rank/model"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ServiceContext 是 recommend-rank 服务的运行时上下文，承载跨请求共享的依赖。
// logic 通过它访问 repository（5 张自有表 + 只读快照缓存）与下游能力接口。
type ServiceContext struct {
	Config config.Config

	// Repository 是数据访问入口（MySQL + Redis 快照缓存 + 下游依赖集合）。
	Repository *repository.Repository

	// MaxCandidates 是入参候选条数上限（规整后的有效值，logic 直接比较）。
	MaxCandidates int32
	// MaxReturn 是出参条数上限。
	MaxReturn int32
	// MaxDecisionPage 是审计分页单页上限。
	MaxDecisionPage int32
	// BucketCount 是分桶空间大小（落库口径）。
	BucketCount int32
}

// NewServiceContext 构造 ServiceContext。
//
// 只装配**已经存在**的依赖：本服务的 5 张表与 Redis 快照缓存。
// recommend-recall / spm / feature-store / ops-config 正由同批次并行落地，
// 这里**不 import** 它们的 rpc 包（连编译依赖都不建），
// 取而代之的是 repository.Downstream 里的接口 + 显式 stub：
// 未接线的下游一律返回 model.ErrNotImplemented，由 logic 转成「声明过的降级」，
// 绝不返回看起来正常的空特征。接线清单见 services/recommend-rank/README.md「待接线接口」。
//
// 关于 recommend-recall 的降级关系：本服务不调用召回接口。
// 排序不可用时的「回退召回原序」用的是**入参候选自带**的 source + rank_in_source，
// 因此不引入新的运行时依赖（详见 README 降级矩阵）。
func NewServiceContext(c config.Config) *ServiceContext {
	rds := redis.MustNewRedis(c.CacheRedis)
	conn := sqlx.NewMysql(c.DataSource)

	repo := repository.New(rds, conn, repoOptions(c.Rank))

	svcCtx := &ServiceContext{
		Config:          c,
		Repository:      repo,
		MaxCandidates:   positiveOr(c.Rank.MaxCandidates, 600),
		MaxReturn:       positiveOr(c.Rank.MaxReturn, 100),
		MaxDecisionPage: positiveOr(c.Rank.MaxDecisionPage, 100),
		BucketCount:     positiveOr(c.Rank.BucketCount, model.DefaultBucketCount),
	}
	svcCtx.logStartup()
	return svcCtx
}

// logStartup 打印启动期生效的关键参数与下游开关（AGENTS.md §4：配置必须可验证）。
// 尤其重要：下游全是 stub，日志里明确写出来，避免运维以为线上真在拉特征。
func (s *ServiceContext) logStartup() {
	logx.Infof("recommend-rank: model_key=%s bucket_count=%d max_candidates=%d max_return=%d "+
		"degrade_enabled=%t fallback=%s score_budget=%dms ttl=%ds decision_retention=%dd | "+
		"downstream stub: feature=%t behavior=%t safety=%t ops_config=%t（false 表示未接线，将按显式降级处理）",
		s.Config.Rank.DefaultModelKey, s.BucketCount, s.MaxCandidates, s.MaxReturn,
		s.Config.Rank.DegradeEnabled, s.Config.Rank.DefaultFallback, s.Config.Rank.ScoreBudgetMs,
		s.Config.Rank.TtlSeconds, s.Config.Rank.DecisionRetentionDays,
		s.Config.Rank.FeatureFetchEnabled, s.Config.Rank.BehaviorFetchEnabled,
		s.Config.Rank.SafetyCheckEnabled, s.Config.Rank.OpsConfigEnabled)
}

// positiveOr 把非正的上线参数兜底为默认值：配置写错（0 或负数）时
// 不能让服务变成「接受 0 条候选」或「返回 0 条结果」的可疑状态。
func positiveOr[T int32 | int64 | int](v T, fallback T) T {
	if v <= 0 {
		return fallback
	}
	return v
}

// fallbackDefaultModelKey 是 DefaultModelKey 未配置时的兜底值。
// 必须与 etc 示例配置和 rpc 注释里的示例模型名一致，否则「默认 model_key」在不同环境指向不同模型。
const fallbackDefaultModelKey = "home_feed_multi_gate"

// repoOptions 是「配置 → 仓库参数」的唯一映射点。
// 单独成函数是为了能在不连 MySQL/Redis 的前提下测出映射错误
// （字段接错线不会编译失败，只会让线上按错误的上限或保留期运行）。
func repoOptions(r config.RankConf) repository.Options {
	modelKey := r.DefaultModelKey
	if modelKey == "" {
		modelKey = fallbackDefaultModelKey
	}
	return repository.Options{
		DefaultModelKey:            modelKey,
		BucketCount:                positiveOr(r.BucketCount, model.DefaultBucketCount),
		RunningExperimentScanLimit: positiveOr(r.RunningExperimentScanLimit, 200),
		LayerVariantScanLimit:      positiveOr(r.LayerVariantScanLimit, 200),
		AssignmentScanLimit:        positiveOr(r.AssignmentScanLimit, 500),
		MaxDigestAids:              int(positiveOr(r.MaxDigestAids, int32(20))),
		ArchiveBatchSize:           positiveOr(r.ArchiveBatchSize, 1000),
		DecisionRetentionDays:      positiveOr(r.DecisionRetentionDays, 14),
		AssignmentRetentionDays:    positiveOr(r.AssignmentRetentionDays, 180),
	}
}
