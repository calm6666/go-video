// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 upload 服务的配置结构。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 业务缓存会话状态短缓存。
	CacheRedis redis.RedisConf // 不能命名为 Redis：与 zrpc.RpcServerConf 内嵌字段同名会让配置加载失败

	// MySQL 主库连接 DSN（生产从配置中心/Secret 注入）。
	DataSource string

	// OSS 对象存储配置。生产环境 AccessKey/SecretKey 必须从 Secret/Vault 注入，
	// 不得提交到仓库；本期为占位字段，签名 URL 由 repository 占位实现。
	OSS OSSConfig

	// Kafka 是事件发布链路配置（upload_outbox → media.task.v1）。
	// Enabled=false（示例配置的默认值）时本进程不投递，事件只在表里累积；
	// 置 true 而二进制没链接队列运行时（`-tags upload_kafka`）时，
	// 构造 ServiceContext 直接失败并写日志，不会「安静地不发事件」。
	// 参数校验见 internal/publisher.ValidatePublishKafka。
	// 刻意没有 Group 键：本服务只做生产，没有任何代码读它。
	Kafka KafkaConf
}

// KafkaConf 是 Outbox 发布循环的参数（与 playback/live-media 的同名结构逐键一致，
// 便于运维用同一份 yaml 模板；消费侧的 Offset/Conns/Consumers 这里没有）。
type KafkaConf struct {
	// Enabled 是否随进程启动 Outbox 发布器。默认 false，示例配置钉住该值。
	// 置 true 需要先 `-tags upload_kafka` 构建，否则 NewSender 返回
	// ErrKafkaRuntimeNotBuilt（本仓库从未与真实 broker 联调，见 README「已知缺口」）。
	Enabled bool `json:",default=false"`
	// Brokers Kafka/Redpanda 地址列表。本地 compose 的 redpanda 为 127.0.0.1:9092。
	// 注意：go-queue v1.2.2 的 kq.NewPusher 不暴露 SASL/TLS 注入口，
	// 因此这里没有 Username/Password/CaFile 三个键，带鉴权的集群不可用。
	Brokers []string `json:",optional"`
	// PublishTopics Outbox 发布的目标 topic。本服务只产出 media.task.v1
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
	BatchLimit int32 `json:",default=100"`
	// SendTimeoutSec 单条投递的上下文超时（秒）。超时按一次失败计入退避，不算已投递。
	SendTimeoutSec int64 `json:",default=5"`
}

// OSSConfig 是 OSS/MinIO 连接与签名配置。
type OSSConfig struct {
	Bucket   string // bucket 名
	Endpoint string // OSS endpoint（如 https://oss-cn-hangzhou.aliyuncs.com）
	// AccessKey、SecretKey 必须从 Secret 注入，禁止明文提交。
	AccessKey string
	SecretKey string
	// PresignTTLSeconds 预签名 URL 有效期（秒），默认 900（15 分钟）。
	PresignTTLSeconds int64
}
