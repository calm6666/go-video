// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package config

import (
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"

	"go-video/services/recommend-recall/model"
)

// Config 是 recommend-recall 服务的配置结构。
// 领域微服务只暴露 gRPC（AGENTS.md §3/§4），面向端的响应聚合与信封由 gateway/app 负责。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 承载池快照、版本指针与召回结果的进程外缓存。
	// 不能命名为 Redis：zrpc.RpcServerConf 已内嵌同名 RedisKeyConf 字段（限流用），
	// 同名字段会让 conf.Load 报 "conflict key redis"，全仓统一用 CacheRedis。
	CacheRedis redis.RedisConf

	// DataSource 是 go_video_recommend_recall 库的 MySQL DSN。
	DataSource string

	// Recall 是召回领域参数。所有条数上限都在这里，代码内不写死业务规模。
	Recall RecallConf

	// Kafka 是事件发布链路配置（recall_outbox → recall.pool.published.v1）。
	// Enabled=false（示例配置的默认值）时本进程不投递，outbox 只累积；
	// 置 true 而二进制没链接队列运行时（`-tags recommendrecall_kafka`）时，
	// 构造 ServiceContext 直接失败并写日志，不会「安静地不发事件」。
	// 参数校验见 internal/publisher.ValidatePublishKafka。
	// 刻意没有 Group/SubscribeTopics 键：本服务只产出事件，没有任何代码读队列。
	Kafka KafkaConf
}

// KafkaConf 是 Outbox 发布循环的参数（与 playback、live-media 的同名结构逐键一致，
// 便于运维用同一份 yaml 模板；消费侧的 Offset/Conns/Consumers 这里没有）。
type KafkaConf struct {
	// Enabled 是否随进程启动 Outbox 发布器。默认 false，示例配置钉住该值。
	// 置 true 需要先 `-tags recommendrecall_kafka` 构建，否则 NewSender 返回
	// ErrKafkaRuntimeNotBuilt（本仓库从未与真实 broker 联调，见 README「已知缺口」）。
	Enabled bool `json:",default=false"`
	// Brokers Kafka/Redpanda 地址列表。本地 compose 的 redpanda 为 127.0.0.1:9092。
	// 注意：go-queue v1.2.2 的 kq.NewPusher 不暴露 SASL/TLS 注入口，
	// 因此这里没有 Username/Password/CaFile 三个键，带鉴权的集群不可用。
	Brokers []string `json:",optional"`
	// PublishTopics Outbox 发布的目标 topic。本服务只产出 recall.pool.published.v1
	// （由 model 的 event_type + schema_version 拼出），多写或少写都会被点名拒绝。
	PublishTopics []string `json:",optional"`
	// MaxRetries 单事件累计尝试上限（含首次），达到即置 state=失败。
	MaxRetries int `json:",default=5"`
	// RetryBackoffSec 退避基数（秒）：第 n 次失败后等 RetryBackoffSec * 2^(n-1)。
	RetryBackoffSec int64 `json:",default=2"`
	// RetryMaxBackoffSec 退避上限（秒），必须不小于 RetryBackoffSec。
	RetryMaxBackoffSec int64 `json:",default=1800"`
	// PollIntervalSec 发布循环的轮询间隔（秒）。进程启动时会立刻先扫一轮，不等这个间隔。
	PollIntervalSec int64 `json:",default=2"`
	// BatchLimit 单轮取到的到期事件数上限。
	// 不得超过 model.MaxOutboxBatch：recall_outbox.ListPending 对超限直接报错，
	// 每轮都报错的发布器等于没接。
	BatchLimit int32 `json:",default=100"`
	// SendTimeoutSec 单条投递的上下文超时（秒）。超时按一次失败计入退避，不算已投递。
	SendTimeoutSec int64 `json:",default=5"`
}

// RecallConf 是召回业务参数，全部来自 etc yaml 或配置中心。
//
// 这些值同时决定 rpc GetRecallConfig 下发的内容：客户端/网关据此控制请求规模，
// 因此任何一项被改小时，在线侧的拒绝行为是"明确报错 + 可解释"，不是静默裁剪。
type RecallConf struct {
	// MaxCandidates 单次 RecallCandidates 的总候选硬上限，超过返回 ErrLimitTooLarge。
	MaxCandidates int `json:",default=400"`
	// DefaultLimit 请求未带 limit 时的默认条数。
	DefaultLimit int `json:",default=120"`
	// PerSourceMax 单路召回的最大条数（防止一路吃满预算）。
	PerSourceMax int `json:",default=120"`
	// EnabledSources 服务开启的召回路（rpc Source 编号，1..6）。
	// 默认只开热门(1)、关注(2)、标签(3)、冷启动(6)：协同与向量路依赖
	// spm / feature-store 特征，接线前保持关闭，开了也只会稳定降级。
	// 有意不给 default：召回路开关是部署决策，漏配必须启动失败而不是悄悄用默认组合。
	EnabledSources []int64
	// DefaultSources 请求未指定 sources 时使用的组合（必须是 EnabledSources 的子集）。
	DefaultSources []int64
	// FallbackSource 降级兜底路（推荐链路故障时仍要能出数；必须是 EnabledSources 内的路）。
	FallbackSource int64
	// ColdStartSource 游客/无行为用户走的召回路（必须是 EnabledSources 内的路）。
	ColdStartSource int64
	// DegradeEnabled 是否允许降级出数。false 时依赖故障直接返回错误，
	// 供压测与回放拿到"真实失败"而不是"看起来正常的降级"。
	DegradeEnabled bool `json:",default=true"`
	// MaxSeedAids 相似/协同召回的种子 aid 条数上限。
	MaxSeedAids int `json:",default=20"`
	// MaxSeedTags 标签召回的种子标签条数上限。
	MaxSeedTags int `json:",default=20"`
	// MaxExcludeAids 单次请求可排除的 aid 条数上限。
	MaxExcludeAids int `json:",default=500"`
	// BudgetMillis 单次召回的内部时间预算：超预算的召回路被裁剪并记
	// DEGRADE_REASON_BUDGET_EXHAUSTED，而不是让整个请求超时。
	BudgetMillis int `json:",default=80"`
	// TTLSeconds 正常结果建议网关缓存秒数；降级结果恒为 0。
	TTLSeconds int64 `json:",default=30"`
	// PoolStaleSeconds 池超过该秒数没有新版本即标记 stale（在线仍出数，但要告警）。
	PoolStaleSeconds int64 `json:",default=1800"`
	// MaxReadyPools GetRecallConfig 一次下发的已上线池数量上限。
	MaxReadyPools int `json:",default=50"`

	// ---- 离线写入与运维路径 ----

	// MaxBatchItems UpsertPoolItems 单次写入条数上限。
	MaxBatchItems int `json:",default=1000"`
	// MaxVersionList ListPoolVersions 条数上限。
	MaxVersionList int `json:",default=100"`
	// MaxPoolSnapshotPage GetPoolSnapshot 单页条数上限。
	MaxPoolSnapshotPage int `json:",default=200"`
	// MaxRequestLogPage ListRecallRequestLogs 单页条数上限。
	MaxRequestLogPage int `json:",default=100"`
	// MinKeepVersions PrunePoolVersions 每个池至少保留的版本数（下限保护，
	// 防止一次误参数把可回滚版本全删）。
	MinKeepVersions int `json:",default=2"`
	// PruneMaxRows PrunePoolVersions 单次删除行数上限。
	PruneMaxRows int64 `json:",default=2000"`
	// RequestLogRetentionSeconds 召回审计日志保留秒数（由 services/cron 触发清理）。
	RequestLogRetentionSeconds int64 `json:",default=604800"`
	// IdempotencyLeaseSeconds 写接口幂等键 PENDING 租约秒数。
	IdempotencyLeaseSeconds int64 `json:",default=300"`
	// IdempotencyRetentionSeconds 幂等记录保留秒数（必须大于租约）。
	IdempotencyRetentionSeconds int64 `json:",default=86400"`
}

// Validate 校验召回路开关组合自洽。
//
// 这一层必须存在，因为降级矩阵的正确性依赖"兜底路自己也开着"：
// 若 FallbackSource 指向未启用的路，降级路径会在最该出数的时候报 pool_not_ready。
// 返回错误即启动失败，不允许带着自相矛盾的召回配置上线。
func (r RecallConf) Validate() error {
	enabled := make(map[int32]struct{}, len(r.EnabledSources))
	for _, s := range r.EnabledSources {
		src := int32(s)
		if !model.ValidSource(src) {
			return fmt.Errorf("recommend-recall: EnabledSources contains invalid source %d", s)
		}
		if _, dup := enabled[src]; dup {
			return fmt.Errorf("recommend-recall: EnabledSources contains duplicate source %d", s)
		}
		enabled[src] = struct{}{}
	}
	if len(enabled) == 0 {
		return errors.New("recommend-recall: EnabledSources must not be empty")
	}
	requireEnabled := func(name string, s int64) error {
		if _, ok := enabled[int32(s)]; !ok {
			return fmt.Errorf("recommend-recall: %s=%d is not in EnabledSources", name, s)
		}
		return nil
	}
	for _, s := range r.DefaultSources {
		if !model.ValidSource(int32(s)) {
			return fmt.Errorf("recommend-recall: DefaultSources contains invalid source %d", s)
		}
		if err := requireEnabled("DefaultSources entry", s); err != nil {
			return err
		}
	}
	if len(r.DefaultSources) == 0 {
		return errors.New("recommend-recall: DefaultSources must not be empty")
	}
	if err := requireEnabled("FallbackSource", r.FallbackSource); err != nil {
		return err
	}
	if err := requireEnabled("ColdStartSource", r.ColdStartSource); err != nil {
		return err
	}
	if r.MaxCandidates < r.DefaultLimit {
		return fmt.Errorf("recommend-recall: MaxCandidates(%d) must be >= DefaultLimit(%d)",
			r.MaxCandidates, r.DefaultLimit)
	}
	if r.PerSourceMax > r.MaxCandidates {
		return fmt.Errorf("recommend-recall: PerSourceMax(%d) must be <= MaxCandidates(%d)",
			r.PerSourceMax, r.MaxCandidates)
	}
	if r.BudgetMillis <= 0 {
		return fmt.Errorf("recommend-recall: BudgetMillis must be positive, got %d", r.BudgetMillis)
	}
	if r.MaxReadyPools <= 0 {
		return fmt.Errorf("recommend-recall: MaxReadyPools must be positive, got %d", r.MaxReadyPools)
	}
	if r.PruneMaxRows <= 0 {
		return fmt.Errorf("recommend-recall: PruneMaxRows must be positive, got %d", r.PruneMaxRows)
	}
	if r.RequestLogRetentionSeconds <= 0 {
		return fmt.Errorf("recommend-recall: RequestLogRetentionSeconds must be positive, got %d",
			r.RequestLogRetentionSeconds)
	}
	return nil
}
