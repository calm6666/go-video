package repository

import (
	"context"
	"time"

	"go-video/services/recommend-recall/model"
)

// 本文件声明召回在线/离线路径需要、但**本服务不能自己实现**的四类外部读取。
//
// 为什么用接口 + stub 而不是直接 import 下游 rpc 包（AGENTS.md §5）：
//   - spm、feature-store 的 rpc 契约正由同批次同事实现，import 它们会把两个未冻结的
//     契约焊死在一起，任何一侧改字段都要跨服务同时发版；
//   - 本服务对下游**只读**，接口形态（要什么、给多少条、失败了怎么降级）由召回侧需求决定，
//     下游契约冻结后在 svc 里换一个实现即可，logic 不用改。
//
// 红线：stub 一律返回 ErrSourceNotConfigured，绝不返回空切片冒充"这个用户没有兴趣标签"。
// 空结果与"读不到"在降级矩阵里是两个不同分支（前者正常出数，后者 DEGRADE_REASON_FEATURE_UNAVAILABLE）。

// InterestTag 是用户兴趣标签权重（标签池选路用）。
type InterestTag struct {
	TagID  int64
	Weight float64
}

// BehaviorSignals 是用户近期行为信号（种子生成与已看排除用）。
type BehaviorSignals struct {
	// RecentAids 近期观看过的稿件（进 exclude_aids，避免重复下发）。
	RecentAids []int64
	// PositiveAids 近期点赞/完播的稿件（i2i 协同召回的种子）。
	PositiveAids []int64
	// FollowUps 关注UP的 mid（关注池寻址用；本服务不写关系，只读）。
	FollowUps []int64
}

// HotSubject 是热度榜条目（热门池重算输入）。
type HotSubject struct {
	Aid   int64
	Score float64
}

// FeatureSource 提供行为与内容特征读取。
//
// 契约缺口（下游契约冻结后按下列映射接线，勿在 logic 里绕过接口）：
//   - GetUserInterest      -> services/spm rpc `Spm.GetUserInterest`（兴趣标签 Top-N）
//   - GetBehaviorSignals   -> services/spm `Spm.BatchGetMetrics`（按 aid/mid 维度取完播与互动窗口）
//   - services/social-graph `SocialGraph.ListFollowing`（关注列表属关系事实，不从 spm 取）
//   - ListHotSubjects      -> services/spm `Spm.ListHotSubjects`（热度榜，供 cron 重算热门池）
//   - GetVectorCandidates  -> services/feature-store `FeatureStore.BatchGetFeatures`
//     （向量近邻候选需要 embedding 特征；本服务不引入向量检索库，见 README 已知缺口）
//
// 所有方法都必须带 limit 且实现侧保证 limit 有上界：在线召回的总预算是各路之和。
type FeatureSource interface {
	// GetUserInterest 取用户兴趣标签，最多 limit 个（按权重降序）。
	GetUserInterest(ctx context.Context, mid int64, limit int) ([]InterestTag, error)
	// GetBehaviorSignals 取用户近期行为信号，各列表长度不超过 limit。
	GetBehaviorSignals(ctx context.Context, mid int64, within time.Duration, limit int) (*BehaviorSignals, error)
	// ListHotSubjects 取全局/分区热度榜前 limit 条（热门池离线重算输入）。
	ListHotSubjects(ctx context.Context, zoneTypeID int64, within time.Duration, limit int) ([]HotSubject, error)
	// GetVectorCandidates 取用户向量近邻候选（最多 limit 条，返回 aid + 相似度）。
	GetVectorCandidates(ctx context.Context, mid int64, limit int) ([]model.RecallPool, error)
}

// VisibilitySource 提供候选过滤所需的事实读取。
//
// 契约缺口（对应实现待接的服务与方法）：
//   - FilterVisible   -> services/video `Video.ListSubmissions`（按 aid 批量取稿件状态，
//     只保留 PUBLISHED 且未在删除/下架流程中的；稿件状态归 video，本服务不得自建可见性标记）
//   - IsFollowing     -> services/social-graph `SocialGraph.IsFollowedBatch`
//     （关注池要求"只能下发已关注UP的稿件"，关系事实归 social-graph）
//   - BlacklistedUps  -> services/social-graph `SocialGraph.ListBlacks`
//     （拉黑UP的稿件必须整批过滤，属合规底线）
//
// video 与 social-graph 的 rpc 已存在，第二轮在 svc 里构造 zrpc client 实现本接口即可；
// 本轮保持 stub，避免出现"配置里有 Endpoints 但没人调用"的死配置。
type VisibilitySource interface {
	// FilterVisible 返回 aids 中当前可下发的子集（保持入参顺序）。
	FilterVisible(ctx context.Context, aids []int64) ([]int64, error)
	// FollowingUps 返回 mid 关注的 UP 列表（关注池 pool_key 生成用），最多 limit 个。
	FollowingUps(ctx context.Context, mid int64, limit int) ([]int64, error)
	// BlockedUps 返回 mid 拉黑的 UP 列表，最多 limit 个。
	BlockedUps(ctx context.Context, mid int64, limit int) ([]int64, error)
}

// UnconfiguredFeatureSource 是 FeatureSource 的显式未接线实现。
type UnconfiguredFeatureSource struct{}

// GetUserInterest 返回 ErrSourceNotConfigured（契约缺口：待接 spm.GetUserInterest）。
func (UnconfiguredFeatureSource) GetUserInterest(context.Context, int64, int) ([]InterestTag, error) {
	return nil, ErrSourceNotConfigured
}

// GetBehaviorSignals 返回 ErrSourceNotConfigured（契约缺口：待接 spm.BatchGetMetrics + social-graph）。
func (UnconfiguredFeatureSource) GetBehaviorSignals(context.Context, int64, time.Duration, int) (*BehaviorSignals, error) {
	return nil, ErrSourceNotConfigured
}

// ListHotSubjects 返回 ErrSourceNotConfigured（契约缺口：待接 spm.ListHotSubjects）。
func (UnconfiguredFeatureSource) ListHotSubjects(context.Context, int64, time.Duration, int) ([]HotSubject, error) {
	return nil, ErrSourceNotConfigured
}

// GetVectorCandidates 返回 ErrSourceNotConfigured（契约缺口：待接 feature-store.BatchGetFeatures）。
func (UnconfiguredFeatureSource) GetVectorCandidates(context.Context, int64, int) ([]model.RecallPool, error) {
	return nil, ErrSourceNotConfigured
}

// UnconfiguredVisibilitySource 是 VisibilitySource 的显式未接线实现。
type UnconfiguredVisibilitySource struct{}

// FilterVisible 返回 ErrSourceNotConfigured（契约缺口：待接 video.ListSubmissions）。
func (UnconfiguredVisibilitySource) FilterVisible(context.Context, []int64) ([]int64, error) {
	return nil, ErrSourceNotConfigured
}

// FollowingUps 返回 ErrSourceNotConfigured（契约缺口：待接 social-graph.ListFollowing）。
func (UnconfiguredVisibilitySource) FollowingUps(context.Context, int64, int) ([]int64, error) {
	return nil, ErrSourceNotConfigured
}

// BlockedUps 返回 ErrSourceNotConfigured（契约缺口：待接 social-graph.ListBlacks）。
func (UnconfiguredVisibilitySource) BlockedUps(context.Context, int64, int) ([]int64, error) {
	return nil, ErrSourceNotConfigured
}

// 编译期确认 stub 满足接口，也确认接口签名没在改动中漂移。
var (
	_ FeatureSource    = UnconfiguredFeatureSource{}
	_ VisibilitySource = UnconfiguredVisibilitySource{}
)
