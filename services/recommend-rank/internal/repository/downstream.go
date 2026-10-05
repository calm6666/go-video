package repository

import (
	"context"
	"errors"

	"go-video/services/recommend-rank/model"
)

// 本文件定义排序链路对「兄弟服务」的全部依赖，并提供显式 stub。
//
// 为什么是接口 + stub 而不是直接 import 下游 rpc client：
//   - recommend-recall / spm / feature-store / ops-config 正由同一批次并行落地，
//     直接 import 会让本服务编译依赖别人的未完成代码（AGENTS.md §4 生成纪律、§5 数据所有权）；
//   - stub 返回 model.ErrNotImplemented，logic 据此走「显式降级」，
//     绝不会把「取不到特征」当成「特征为 0」参与打分（那会产出看起来正常、实际错乱的排序）。
//
// 每个接口都注明将来接哪个服务的哪个方法，第二轮按此接线（README「待接线接口」同源）。

// --- 特征 ---

// FeatureVector 是一组特征值。缺失的 key 不出现在 map 里，
// 由 model.RankFeatureConfig.MissingPolicy 决定补默认值、丢该路候选还是整体降级。
type FeatureVector map[string]float64

// FeatureSource 提供候选与用户侧特征。
//
// 契约缺口：待 feature-store 落地后接
// feature-store rpc `FeatureStore.BatchGetFeatures`（候选侧批量特征）与
// `FeatureStore.GetFeature`（用户侧单点特征），按 feature_config_version 下传受控特征清单；
// 现由 StubFeatureSource 占位，返回 model.ErrNotImplemented。
type FeatureSource interface {
	// ItemFeatures 批量取候选内容特征；aids 条数不超过 Options.FeatureBatchSize（调用方分批）。
	ItemFeatures(ctx context.Context, scene string, featureConfigVersion string, aids []int64) (map[int64]FeatureVector, error)
	// UserFeatures 取用户侧特征；mid=0 表示游客，返回空向量由调用方按缺失策略处理。
	UserFeatures(ctx context.Context, mid int64, featureConfigVersion string) (FeatureVector, error)
}

// --- 行为统计（SPM，仅质量目标，不含任何商业化指标） ---

// BehaviorSource 提供内容热度与用户兴趣特征。
//
// 契约缺口：待 spm 落地后接 `Spm.BatchGetMetrics`（内容完播率/互动率等质量指标）与
// `Spm.GetUserInterest`（用户兴趣向量）。AGENTS.md §7：SPM 只作为排序特征输入，
// 本接口不提供广告位、投放或商业化报表字段，也不允许调用方据此「手工改结果」。
type BehaviorSource interface {
	// ContentQuality 批量取内容质量指标（完播率、互动率、负反馈率）。
	ContentQuality(ctx context.Context, aids []int64) (map[int64]FeatureVector, error)
	// UserInterests 取用户兴趣标签权重。
	UserInterests(ctx context.Context, mid int64) (FeatureVector, error)
}

// --- 内容安全 ---

// SafetyGate 复核候选的可分发状态。
//
// 契约缺口：moderation-orchestrator 现有 rpc 只有按单条 content 的 `GetResult`，
// 没有批量可见性查询；第二轮接线时两种做法二选一：
//  1. 在 moderation-orchestrator 增补批量 rpc（需要该服务改 proto 并重新生成）；
//  2. 由 gateway/app 在调用排序前用 video 的发布态做一次性过滤，本服务不再复核。
//
// 在打通之前，SafetyCheckEnabled=false 时本接口返回 ErrNotImplemented，
// logic 必须把 degrade_reason 记为 model.DegradeReasonSafetyUnavailable，
// 而不是在摘要里写「安全过滤已通过」。
type SafetyGate interface {
	// VisibleAids 返回每个 aid 的可见结论；false 表示不可见（已剔除）。
	// 读不到结论时返回 error（按不通过处理并声明降级），不允许默认放行。
	VisibleAids(ctx context.Context, aids []int64) (map[int64]bool, error)
}

// --- 运营干预位（唯一的运营影响入口，且必须可审计） ---

// Intervention 是允许运营调节的**参数**，不是结果。
// 结构体里刻意没有 aid 列表、没有加权系数、没有任何投放/商业化字段：
// 运营能改的只有「频控阈值 / 打散窗口 / 场景开关」这类参数，
// 且每次生效都会把 config_revision 记进 rank_decision_log，事后可查。
type Intervention struct {
	// Enabled false 表示该场景临时关闭个性化排序（一律走召回原序兜底）。
	Enabled bool
	// FrequencyWindowSeconds 频控窗口（秒），0 表示用模型登记的默认值。
	FrequencyWindowSeconds int64
	// DiversityGap 打散窗口内同作者/同标签最大相邻条数，0 表示用默认值。
	DiversityGap int32
	// ConfigRevision 运营配置代次（写进决策摘要，用于「这段时间的结果是谁配的」）。
	ConfigRevision string
}

// OpsConfigReader 读取运营干预参数。
//
// 契约缺口：待 ops-config 落地后接 `OpsConfig.ResolveConfig`（场景级参数 key，
// 例如 recommend.rank.scene.home_feed），BatchResolveConfig 用于一次拉多场景。
// 现由 StubOpsConfigReader 占位，返回 model.ErrNotImplemented。
type OpsConfigReader interface {
	// Resolve 读取场景的排序参数；未配置或不可读时返回 error，调用方保持默认参数并记降级。
	Resolve(ctx context.Context, scene string) (Intervention, error)
}

// --- 下游集合 ---

// Downstream 聚合排序所需的四个外部读依赖。
// svc 装配时先给 stub，第二轮逐个替换为真实 client，logic 不需要改签名。
type Downstream struct {
	Features   FeatureSource
	Behaviors  BehaviorSource
	Safety     SafetyGate
	OpsConfigs OpsConfigReader
}

// NewStubDownstream 返回一组「什么都不接」的下游实现。
// 用于：本轮占位、以及本地/回放环境显式关闭某条下游（配合 config 的 *Enabled 开关）。
func NewStubDownstream() Downstream {
	return Downstream{
		Features:   StubFeatureSource{},
		Behaviors:  StubBehaviorSource{},
		Safety:     StubSafetyGate{},
		OpsConfigs: StubOpsConfigReader{},
	}
}

// --- stub 实现：一律返回 model.ErrNotImplemented，绝不返回零值冒充成功 ---

// IsUnwired 判定一个错误是否表示「这条下游还是契约轮占位实现」。
// 占位哨兵由本包（NewStubDownstream）定义，因此判定口径也留在本包：
// logic 据此区分「配置声明启用但没接线」（部署不一致，必须显式报错）
// 与「接线了但此刻读不到」（可降级）。
func IsUnwired(err error) bool { return errors.Is(err, model.ErrNotImplemented) }

// StubFeatureSource 是 FeatureSource 的占位实现。
type StubFeatureSource struct{}

// ItemFeatures 未接线，返回 ErrNotImplemented。
func (StubFeatureSource) ItemFeatures(context.Context, string, string, []int64) (map[int64]FeatureVector, error) {
	return nil, model.ErrNotImplemented
}

// UserFeatures 未接线，返回 ErrNotImplemented。
func (StubFeatureSource) UserFeatures(context.Context, int64, string) (FeatureVector, error) {
	return nil, model.ErrNotImplemented
}

// StubBehaviorSource 是 BehaviorSource 的占位实现。
type StubBehaviorSource struct{}

// ContentQuality 未接线，返回 ErrNotImplemented。
func (StubBehaviorSource) ContentQuality(context.Context, []int64) (map[int64]FeatureVector, error) {
	return nil, model.ErrNotImplemented
}

// UserInterests 未接线，返回 ErrNotImplemented。
func (StubBehaviorSource) UserInterests(context.Context, int64) (FeatureVector, error) {
	return nil, model.ErrNotImplemented
}

// StubSafetyGate 是 SafetyGate 的占位实现。
type StubSafetyGate struct{}

// VisibleAids 未接线，返回 ErrNotImplemented。
func (StubSafetyGate) VisibleAids(context.Context, []int64) (map[int64]bool, error) {
	return nil, model.ErrNotImplemented
}

// StubOpsConfigReader 是 OpsConfigReader 的占位实现。
type StubOpsConfigReader struct{}

// Resolve 未接线，返回 ErrNotImplemented。
func (StubOpsConfigReader) Resolve(context.Context, string) (Intervention, error) {
	return Intervention{}, model.ErrNotImplemented
}

// 编译期断言：stub 必须完整实现接口，接口签名变化时在编译期就暴露。
var (
	_ FeatureSource   = StubFeatureSource{}
	_ BehaviorSource  = StubBehaviorSource{}
	_ SafetyGate      = StubSafetyGate{}
	_ OpsConfigReader = StubOpsConfigReader{}
)
