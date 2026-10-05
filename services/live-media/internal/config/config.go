// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 live-media 服务的配置结构。
// 领域微服务只暴露 gRPC（AGENTS.md §3/§4），对外 HTTP 由 gateway/app 聚合。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 承载任务详情缓存、房间档位列表缓存与 Worker 领取任务的互斥标记。
	// 不能命名为 Redis：zrpc.RpcServerConf 已内嵌同名 RedisKeyConf 字段，
	// 同名会让 conf.Load 报 "conflict key redis"，代码可编译但启动即失败。
	CacheRedis redis.RedisConf

	// DataSource 是 go_video_live_media 库的 MySQL DSN。
	// 只允许访问本服务的 live_* 表（AGENTS.md §5），禁止直连 asset/video/transcode 库表。
	DataSource string

	// AssetRPC 是 asset 服务的 zrpc client 配置（回放产物登记 RegisterAsset）。
	// 本轮 ServiceContext 不构造客户端：调用点在 internal/repository 的显式 stub 中，
	// 接线步骤见 services/live-media/README.md「已知缺口」。
	AssetRPC zrpc.RpcClientConf `json:",optional"`

	// VideoRPC 是 video 服务的 zrpc client 配置（回放建稿 CreateSubmission 与投影读取）。
	VideoRPC zrpc.RpcClientConf `json:",optional"`

	// ModerationRPC 是 moderation-orchestrator 的 zrpc client 配置（回放提交审核）。
	ModerationRPC zrpc.RpcClientConf `json:",optional"`

	// LiveMedia 是直播媒体领域参数（超时、分片、分页与缓存上限）。
	LiveMedia LiveMediaConf

	// Kafka 是事件链路配置。发布侧已接线（live_media_outbox → 9 个 livemedia.* topic，
	// 见 internal/publisher）：Enabled=true 而二进制没链接队列运行时
	// （`-tags livemedia_kafka`）时构造 ServiceContext 直接失败并写日志，不会「安静地不发事件」；
	// 参数校验见 internal/publisher.ValidatePublishKafka。
	// 消费侧同样已接线（live.state.v1 → 整场档位下线，见 internal/consumer），
	// 与发布侧共用下面的 Enabled 与同一个构建标签；参数校验见 internal/consumer.ValidateKafka。
	Kafka KafkaConf
}

// LiveMediaConf 直播媒体业务参数。全部来自 etc yaml 或配置中心，不写死在代码里。
type LiveMediaConf struct {
	// DefaultTranscodeTimeoutSeconds 转码任务无心跳判超时的默认秒数（请求未指定时使用）。
	DefaultTranscodeTimeoutSeconds int32 `json:",default=60"`
	// DefaultRecordTimeoutSeconds 录制任务无心跳判超时的默认秒数。
	DefaultRecordTimeoutSeconds int32 `json:",default=90"`
	// DefaultMaxAttempts 转码任务默认最大重试次数（RetryLiveTranscode 的上限）。
	DefaultMaxAttempts int32 `json:",default=3"`
	// DefaultRecordSegmentSeconds 录制分片默认时长（秒）。
	DefaultRecordSegmentSeconds int32 `json:",default=10"`
	// MaxRecordSegmentSeconds 录制分片时长上限（秒），超过直接拒绝登记。
	MaxRecordSegmentSeconds int32 `json:",default=60"`
	// MaxSegmentPageSize ListRecordSegments 单次返回上限（keyset 分页）。
	MaxSegmentPageSize int32 `json:",default=500"`
	// DefaultSegmentPageSize ListRecordSegments 默认页大小。
	DefaultSegmentPageSize int32 `json:",default=200"`
	// MaxReplayGapSegments 回放区间内允许的缺口（MISSING/CORRUPT）切片数。
	// 默认 0：allow_gaps=false 时任何空洞都拒绝提交，避免产出时间轴断裂的回放。
	MaxReplayGapSegments int32 `json:",default=0"`
	// ReplayTitleMaxLength 回放标题 rune 上限（透传给 video 前本地夹取）。
	ReplayTitleMaxLength int `json:",default=80"`
	// DefaultRetentionBatchLimit 回收任务默认批量上限。
	DefaultRetentionBatchLimit int32 `json:",default=100"`
	// MaxRetentionBatchLimit 回收任务批量上限（超过则夹取，不报错）。
	MaxRetentionBatchLimit int32 `json:",default=500"`
	// MaxListPageSize pn/ps 分页的每页上限。
	MaxListPageSize int32 `json:",default=50"`
	// TaskCacheTTLSeconds 任务详情缓存秒数，0 表示关闭任务缓存。
	TaskCacheTTLSeconds int `json:",default=60"`
	// StreamOutputCacheTTLSeconds 房间档位列表缓存秒数，0 表示关闭。
	StreamOutputCacheTTLSeconds int `json:",default=15"`
	// TaskTimeoutSweepIntervalSeconds 超时清扫 Worker 轮询间隔（秒）。
	TaskTimeoutSweepIntervalSeconds int `json:",default=15"`
	// TaskTimeoutSweepEnabled 是否在本进程启动超时清扫 Worker。
	// 本轮显式 false：清扫依赖 logic 实现，开启前必须先把状态机与上报打通。
	TaskTimeoutSweepEnabled bool `json:",default=false"`
}

// KafkaConf 事件链路参数。**一个 Enabled 管两条链路**：
// 置 true 同时启动 Outbox 发布器（internal/publisher）与 live.state.v1 消费者（internal/consumer），
// 任一侧的键不完整进程即以错误终止启动，不会出现「只起了发布器、消费者安静地不存在」。
// 「链接了 Kafka 运行时」是构建期事实（同一个 -tags livemedia_kafka），
// 拆成两个开关只会让运维以为能单独关掉一半，因此刻意不提供 ConsumeEnabled 这类键。
//
// 发布侧键：PublishTopics/RetryBackoffSec/RetryMaxBackoffSec/PollIntervalSec/BatchLimit/SendTimeoutSec，
// 逐键校验见 internal/publisher.ValidatePublishKafka；
// 消费侧键：SubscribeTopics/Offset/Conns/Consumers/Processors/ForceCommit/Username/Password/CaFile，
// 逐键校验见 internal/consumer.ValidateKafka；
// 共用键：Brokers/Group/Enabled/MaxRetries（MaxRetries 既是发布器的判死上限，也是消费者的
// 进程内尝试上限，两者刻意同步收紧，不出现「发布器还在退避、消费者已放弃」）。
type KafkaConf struct {
	// Brokers Kafka/Redpanda 地址列表。
	// 注意：go-queue v1.2.2 的 kq.NewPusher 不暴露 SASL/TLS 注入口，因此下面三个鉴权键
	// **只对消费侧生效**，发布侧读到它们也不会有任何作用（带鉴权的集群发布链路对接不了）。
	Brokers []string `json:",optional"`
	// Group 消费组名。
	Group string `json:",default=live-media.v1"`
	// SubscribeTopics 订阅的领域事件 topic，只能是 live.state.v1（本包唯一有映射的事件）。
	SubscribeTopics []string `json:",optional"`
	// PublishTopics Outbox 发布的目标 topic，必须是 model 里 9 个 EventType* 常量派生的
	// topic 全集：多写一个本服务不产出的 topic、或少写一个（漏掉的那条事件会在
	// 每一轮投递时撞「没有发送通道」直到判死）都会在启动时被点名拒绝。
	PublishTopics []string `json:",optional"`
	// Enabled 是否随进程启动事件链路（发布器 + 消费者，见类型注释）。
	// 默认 false，示例配置钉住该值。置 true 需要先 `-tags livemedia_kafka` 构建，
	// 否则 NewSender/NewKqFactory 返回 ErrKafkaRuntimeNotBuilt
	// （本仓库从未与真实 broker 联调，见 README「已知缺口」）。
	Enabled bool `json:",default=false"`
	// MaxRetries 单事件累计尝试上限（含首次）：发布侧达到即置 state=失败，
	// 消费侧达到即记 given_up 日志并提交位点。
	MaxRetries int `json:",default=5"`
	// RetryBackoffSec 退避基数（秒）：第 n 次失败后等 RetryBackoffSec * 2^(n-1)。
	RetryBackoffSec int64 `json:",default=5"`
	// RetryMaxBackoffSec 退避上限（秒），必须不小于 RetryBackoffSec。
	RetryMaxBackoffSec int64 `json:",default=1800"`
	// PollIntervalSec 发布循环的轮询间隔（秒）。进程启动时会立刻先扫一轮，不等这个间隔。
	PollIntervalSec int64 `json:",default=2"`
	// BatchLimit 单轮取到的到期事件数上限。
	BatchLimit int32 `json:",default=100"`
	// SendTimeoutSec 单条投递的上下文超时（秒）。超时按一次失败计入退避，不算已投递。
	SendTimeoutSec int64 `json:",default=5"`

	// --- 以下八键只被消费侧读取（口径与 live-room、live-gateway 的同名键一致） ---

	// Offset 消费组没有位点时的起点。first 会重放整个保留窗口，
	// 对断流下线是安全的（CAS 幂等），但会把保留期内的旧停播事件再执行一遍，只在补账时用。
	Offset string `json:",options=first|last,default=last"`
	// Conns 每个 topic 建立的 reader 连接数。
	Conns int `json:",default=1"`
	// Consumers 每条连接的拉取协程数。
	Consumers int `json:",default=2"`
	// Processors 每条连接的并发处理协程数。
	// 同一房间的两条断流事件并发处理是安全的（下线是 CAS 条件下线），因此不需要按 key 串行化。
	Processors int `json:",default=4"`
	// ForceCommit 处理失败时是否仍提交位点。默认 false：让 broker 重投，
	// 由 MaxRetries 在本进程内收敛尝试次数。
	// 置 true 等于把「档位没摘下来」变成静默丢事件，只在排障时短期开启。
	ForceCommit bool `json:",default=false"`
	// Username/Password/CaFile SASL 与 TLS 参数（仅消费侧，见 Brokers 注释）。
	// 生产值进 Secret/Vault，绝不写进 etc 示例（AGENTS.md §4）。
	Username string `json:",optional"`
	Password string `json:",optional"`
	CaFile   string `json:",optional"`
}
