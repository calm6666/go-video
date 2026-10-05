// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 recommend-rank 服务的配置结构。
// 领域微服务只暴露 gRPC（AGENTS.md §3/§4），面向端的响应由 gateway/app 聚合。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 承载运行时配置、ACTIVE 模型版本、RUNNING 实验集合与幂等回放缓存。
	// 不能命名为 Redis：zrpc.RpcServerConf 已内嵌同名 RedisKeyConf 字段（鉴权/限流用），
	// 同名会让 conf.Load 报 "conflict key redis"，全仓统一用 CacheRedis。
	CacheRedis redis.RedisConf

	// DataSource 是 go_video_recommend_rank 库的 MySQL DSN。
	DataSource string

	// Rank 是排序领域参数：候选与出参上限、分桶口径、降级与保留期。
	// 契约里的 MaxCandidates / MaxReturn / BucketCount / MaxDecisionPage / MaxDigestAids /
	// MaxFeatureKeys 全部来自这里，不写死在代码常量上（灰度可调）。
	Rank RankConf
}

// RankConf 是排序领域参数，全部来自 etc yaml 或配置中心。
//
// 注意：这里没有任何「运营手工置顶/加权某个 aid」的开关（AGENTS.md §7 禁止手工修改推荐结果）。
// 运营可影响的只有模型版本、特征配置与实验状态三类登记，且都必须带 operator/reason 落库。
type RankConf struct {
	// DefaultModelKey 是请求未指定 model_key 时使用的逻辑模型名。
	DefaultModelKey string `json:",default=home_feed_multi_gate"`

	// --- 入参/出参硬上限（超限直接报错，不静默裁剪，见 model.ErrTooManyCandidates） ---

	// MaxCandidates 单次 RankCandidates 接受的候选条数上限。
	MaxCandidates int32 `json:",default=600"`
	// MaxReturn 单次 RankCandidates 返回的条数上限（rpc limit 的天花板）。
	MaxReturn int32 `json:",default=100"`
	// MaxDecisionPage ListRankDecisions 单页最大条数。
	MaxDecisionPage int32 `json:",default=100"`
	// MaxDigestAids 决策摘要里 top_aids 保留的 aid 条数（控制行宽）。
	MaxDigestAids int32 `json:",default=20"`
	// MaxFeatureKeys 单个特征配置版本允许登记的特征条数。
	MaxFeatureKeys int32 `json:",default=512"`
	// MaxWeightSum 多目标权重之和的上限（rpc 注释承诺 <=10 的校验口径）。
	MaxWeightSum float64 `json:",default=10"`
	// MaxOverridesBytes 实验 overrides JSON 的字节上限。
	// 只能等于或收紧 model.MaxOverridesBytes（写库前的硬上限，见 model.ValidateOverrideKeys）。
	MaxOverridesBytes int `json:",default=4096"`
	// MaxArtifactRefBytes 模型工件引用的字符上限（只存对象存储 key，不存本体）。
	MaxArtifactRefBytes int `json:",default=512"`

	// --- 分桶与实验 ---

	// BucketCount 分桶空间大小（千分位），落库口径固定；请求带自定义 bucket_count 时只做回显换算。
	BucketCount int32 `json:",default=1000"`
	// RunningExperimentScanLimit ListRunning 单次扫描条数上限。
	RunningExperimentScanLimit int `json:",default=200"`
	// LayerVariantScanLimit 同层重叠校验单次读取的变体条数上限。
	LayerVariantScanLimit int `json:",default=200"`
	// AssignmentScanLimit 分桶主体核对单页条数上限。
	AssignmentScanLimit int `json:",default=500"`

	// --- 超时与降级（README 降级矩阵的实现口径） ---

	// DegradeEnabled 总开关：false 时任何依赖故障都直接报错（压测/回放环境用）。
	DegradeEnabled bool `json:",default=true"`
	// DefaultFallback 默认兜底策略：recall_order / previous_model / safety_only。
	DefaultFallback string `json:",default=recall_order,options=recall_order|previous_model|safety_only"`
	// ScoreBudgetMs 打分时间预算（毫秒），超预算按 BUDGET_EXHAUSTED 降级并裁剪未打分候选。
	ScoreBudgetMs int64 `json:",default=80"`
	// TtlSeconds 正常结果建议网关缓存秒数；降级结果固定返回 0。
	TtlSeconds int64 `json:",default=30"`
	// DownstreamTimeoutMs 访问下游（feature-store/spm/moderation/ops-config）的单次超时上限（毫秒）。
	// 必须显著小于 rpc 超时，否则「降级」永远来不及发生。
	DownstreamTimeoutMs int64 `json:",default=30"`

	// --- 下游能力开关（false 时走显式降级，不伪造数据） ---

	// FeatureFetchEnabled 是否向 feature-store 拉取实时特征；false 时按特征缺失策略降级。
	FeatureFetchEnabled bool `json:",default=false"`
	// BehaviorFetchEnabled 是否向 spm 拉取内容热度/用户兴趣特征；false 同上。
	BehaviorFetchEnabled bool `json:",default=false"`
	// SafetyCheckEnabled 是否向 moderation-orchestrator 复核候选可见性；
	// 关闭时不写「已通过安全过滤」的摘要，degrade_reason 记 safety_unavailable。
	SafetyCheckEnabled bool `json:",default=false"`
	// OpsConfigEnabled 是否读取 ops-config 的运营干预位（唯一的运营影响入口，必须可审计）。
	OpsConfigEnabled bool `json:",default=false"`

	// --- 特征与候选批量口径 ---

	// FeatureBatchSize 单次向下游取特征的候选条数上限（超出分批，防止单请求打爆下游）。
	FeatureBatchSize int `json:",default=128"`
	// FeatureCacheTTLSeconds 特征快照的进程内/Redis 缓存秒数，0 表示不缓存。
	FeatureCacheTTLSeconds int64 `json:",default=5"`

	// --- 配置缓存（减少在线面 MySQL 读放大） ---

	// RuntimeConfigCacheTTLSeconds GetRankRuntimeConfig 结果缓存秒数，0 表示不缓存。
	RuntimeConfigCacheTTLSeconds int64 `json:",default=5"`
	// DecisionReplayCacheTTLSeconds 幂等回放（同 request_id）读缓存秒数，0 表示只读 DB。
	DecisionReplayCacheTTLSeconds int64 `json:",default=60"`

	// --- 保留期与归档（rank_decision_log 是本库增长最快的表） ---

	// DecisionRetentionDays 决策摘要保留天数，超期先归档再分批删除。
	DecisionRetentionDays int `json:",default=14"`
	// ArchiveBatchSize 单批归档/删除的行数上限。
	ArchiveBatchSize int `json:",default=1000"`
	// AssignmentRetentionDays 分桶记录保留天数（实验结束且超过该天数后可清理）。
	AssignmentRetentionDays int `json:",default=180"`
}
