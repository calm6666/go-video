// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 live-ingest 服务的配置结构。
// 领域微服务只暴露 gRPC（AGENTS.md §3/§4），对外 HTTP 由 gateway/app 聚合。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 承载密钥鉴权结果短缓存、CDN 回调 nonce 防重放、节点打分快照与
	// 流状态读缓存。
	// 不能命名为 Redis：zrpc.RpcServerConf 已内嵌同名 RedisKeyConf 字段，
	// 同名会让 conf.Load 报 "conflict key redis"，代码可编译但启动即失败。
	// 全仓统一用 CacheRedis。
	CacheRedis redis.RedisConf

	// DataSource 是 go_video_live_ingest 库的 MySQL DSN。
	// 只允许访问本服务的 live_* 表（AGENTS.md §5），禁止直连 live-room/live-media 库表。
	DataSource string

	// LiveIngest 是推流接入领域参数（有效期、宽限期、健康阈值、分页上限）。
	LiveIngest LiveIngestConf

	// Cdn 是 CDN/媒体入口配置位：回调域名白名单与签名密钥的 Secret/Vault 引用。
	// 真实厂商适配未落地，调用点在 internal/repository 的显式 stub 中（见服务 README）。
	Cdn CdnConf

	// Kafka 是事件发布链路配置（live_ingest_outbox → live.state.v1）。
	// Enabled=false（示例配置的默认值）时本进程不投递，outbox 只堆在表里；
	// 置 true 而二进制没链接队列运行时（`-tags liveingest_kafka`）时，
	// 构造 ServiceContext 直接失败并写日志，不会「安静地不发事件」。
	// 参数校验见 internal/publisher.ValidatePublishKafka。
	Kafka KafkaConf

	// LiveRoomRPC 是 live-room 的 zrpc client 配置位（房间门禁/场次回查的将来依赖）。
	// 刻意不构造客户端：两服务是「事件生产者 → 消费者」关系，不需要编译期依赖
	// （生成包 go-video/services/live-room/rpc 已存在，随时可 import）。
	// 接线步骤写在 services/live-ingest/README.md「已知缺口」第 6 条。
	LiveRoomRPC zrpc.RpcClientConf `json:",optional"`
}

// LiveIngestConf 推流接入业务参数。全部来自 etc yaml 或配置中心，不写死在代码里。
type LiveIngestConf struct {
	// IssueTtlSeconds 签发密钥的默认有效期（秒），请求未指定时使用。
	IssueTtlSeconds int64 `json:",default=604800"`
	// MaxIssueTtlSeconds 密钥有效期上限（秒），超过则拒绝：长期密钥违反 README 约束。
	MaxIssueTtlSeconds int64 `json:",default=2592000"`
	// KeyRandomBytes 明文密钥的随机字节数（Base62/Hex 展开后决定密钥长度）。
	KeyRandomBytes int `json:",default=18"`
	// RotateGraceSeconds 轮转后旧密钥的默认宽限秒数（OBS 不重启切换窗口）。
	RotateGraceSeconds int64 `json:",default=300"`
	// MaxStreamsPerKey 单密钥默认并发非终态流上限。
	MaxStreamsPerKey int32 `json:",default=1"`
	// InterruptGraceSeconds 断流宽限期（秒）：INTERRUPTED 超过该时长未重连即判 STOPPED。
	InterruptGraceSeconds int64 `json:",default=60"`
	// IdleTimeoutSeconds IDLE 流（鉴权通过但从未推流）回收秒数。
	IdleTimeoutSeconds int64 `json:",default=120"`
	// HeartbeatTimeoutSeconds 非终态流心跳超时秒数，超时进入 INTERRUPTED。
	HeartbeatTimeoutSeconds int64 `json:",default=30"`
	// HealthSampleWindowSeconds 健康采样默认聚合窗口（秒）。
	HealthSampleWindowSeconds int32 `json:",default=10"`
	// HealthNoDataSeconds 超过该秒数无采样即判 NO_DATA（应大于一个采样窗口）。
	HealthNoDataSeconds int64 `json:",default=30"`
	// DegradedMinVideoBitrateBps 视频码率低于该值判 DEGRADED（bps）。
	DegradedMinVideoBitrateBps int64 `json:",default=800000"`
	// CriticalMinVideoBitrateBps 视频码率低于该值判 CRITICAL（bps）。
	CriticalMinVideoBitrateBps int64 `json:",default=200000"`
	// CriticalMaxPacketLossPpm 丢包率超过该值（百万分比）判 CRITICAL。
	CriticalMaxPacketLossPpm int32 `json:",default=50000"`
	// CriticalMinFpsX100 帧率（×100）低于该值判 CRITICAL。
	CriticalMinFpsX100 int32 `json:",default=500"`
	// MaxListPageSize 所有 pn/ps 分页的每页上限。
	MaxListPageSize int32 `json:",default=50"`
	// MaxEventPageSize ListStreamEvents / 位点取样的单次上限。
	MaxEventPageSize int32 `json:",default=200"`
	// MaxEventRetryBatch RetryFailedEvents 单次批量上限。
	MaxEventRetryBatch int32 `json:",default=200"`
	// MaxSamplePoints GetStreamHealth 返回的采样点上限。
	MaxSamplePoints int32 `json:",default=120"`
	// CountHardLimit 断流记录 total 的统计上限，超出返回 -1（避免大表 COUNT）。
	CountHardLimit int64 `json:",default=10000"`
	// CallbackSkewSeconds CDN 回调时间戳允许偏移（秒），超出判为重放风险。
	CallbackSkewSeconds int64 `json:",default=300"`
	// CallbackNonceTTLSec 回调 nonce 在 CacheRedis 中的防重放保留秒数。
	CallbackNonceTTLSec int `json:",default=600"`
	// SweeperEnabled 是否在本进程启动状态扫描器（心跳超时→INTERRUPTED、宽限期耗尽→STOPPED）。
	// 扫描器本身仍未接线（见 svc 的显式告警与 README「已知缺口」），因此这里保持默认 false。
	// 刻意不与 Kafka.Enabled 合并成一个开关：扫描器只需要 MySQL + Redis，
	// 发布器只需要 outbox 表 + MQ，失败模式不同，运维要能单独开或单独关。
	SweeperEnabled bool `json:",default=false"`
	// SweepIntervalSeconds 扫描器轮询间隔（秒）。
	SweepIntervalSeconds int64 `json:",default=10"`
	// SweepBatchLimit 单次扫描的流数量上限。
	SweepBatchLimit int32 `json:",default=200"`
}

// CdnConf CDN/媒体入口参数。密钥只放 Secret/Vault 引用，配置里不出现任何真实密钥。
type CdnConf struct {
	// Enabled 是否启用外部入口适配（本轮 false：所有外部调用走显式 stub）。
	Enabled bool `json:",default=false"`
	// PublishDomains 允许接入与回调的推流域名白名单。
	PublishDomains []string `json:",optional"`
	// CallbackSecretRef CDN 回调签名密钥的 Secret/Vault 引用（例如
	// vault:secret/data/live-ingest/cdn#callback_key）。绝不在日志或响应中回显。
	CallbackSecretRef string `json:",optional"`
	// RequestTimeoutSeconds 调用外部入口/CDN 管理的超时（秒）。
	RequestTimeoutSeconds int64 `json:",default=3"`
	// DefaultAssignRegionLimit 节点分配时是否强制同区域就近。
	DefaultAssignSameRegion bool `json:",default=false"`
}

// KafkaConf 事件发布链路参数（live_ingest_outbox → live.state.v1）。
//
// 这组键是 Outbox 投递的唯一参数来源：早期版本在 LiveIngestConf 里还另有
// OutboxMaxRetries / OutboxBackoffBaseSeconds 两个同义键（无任何读取者），已合并到这里，
// 不要再加第三个「重试次数」。
//
// 逐键校验在 internal/publisher.ValidatePublishKafka：Enabled=true 但参数不完整时
// 服务启动即失败，不留「配置写错了于是事件安静地不出去」这种状态。
type KafkaConf struct {
	// Enabled 是否随进程启动 Outbox 发布器。默认 false，且示例配置钉住该值。
	// 置 true 需要先 `-tags liveingest_kafka` 构建，否则 NewSender 返回
	// ErrKafkaRuntimeNotBuilt（本仓库从未与真实 broker 联调，见 README「已知缺口」）。
	Enabled bool `json:",default=false"`
	// Brokers Kafka/Redpanda 地址列表。本地 compose 的 redpanda 为 127.0.0.1:9092。
	// 注意：go-queue v1.2.2 的 kq.NewPusher 不暴露 SASL/TLS 注入口，
	// 因此这里没有 Username/Password/CaFile 三个键（消费侧有），带鉴权的集群不可用。
	Brokers []string `json:",optional"`
	// Group 消费组名。本服务只做生产，不消费事件，该键只保留给将来的死信重投工具，
	// 因此刻意不参与 ValidatePublishKafka 的必填判定（校验一个没人读的键是假严格）。
	Group string `json:",default=live-ingest.v1"`
	// PublishTopics Outbox 发布的目标 topic。本服务只产出 live.state.v1
	// （由 model 的 event_type + schema_version 拼出），配置里多写或少写都会被点名拒绝。
	PublishTopics []string `json:",optional"`
	// MaxRetries 单事件累计尝试上限（含首次），达到即置 state=FAILED。
	MaxRetries int `json:",default=5"`
	// RetryBackoffSec 退避基数（秒）：第 n 次失败后等 RetryBackoffSec * 2^(n-1)。
	RetryBackoffSec int64 `json:",default=2"`
	// RetryMaxBackoffSec 退避上限（秒），必须不小于 RetryBackoffSec。
	RetryMaxBackoffSec int64 `json:",default=1800"`
	// PollIntervalSec 发布循环的轮询间隔（秒）。进程启动时会立刻先扫一轮，不等这个间隔。
	PollIntervalSec int64 `json:",default=2"`
	// BatchLimit 单轮取到的到期事件数上限（受 model 的 clampLimit 再夹一次）。
	BatchLimit int32 `json:",default=100"`
	// SendTimeoutSec 单条投递的上下文超时（秒）。超时按一次失败计入退避，不算已投递。
	SendTimeoutSec int64 `json:",default=5"`
}
