// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 playback 服务的配置结构。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 业务缓存连接：播放计数、会话回源校验标记等热数据。
	CacheRedis redis.RedisConf // 不能命名为 Redis：与 zrpc.RpcServerConf 内嵌字段同名会让配置加载失败

	// MySQL 主库 DSN（仅本地示例，生产从配置中心/Secret 注入）。
	DataSource string

	// RightsRPC 是 rights 服务的 zrpc client 配置，用于校验 PGC 版权窗口。
	// 留空时不构造客户端，PGC 签发会返回明确错误而不是伪造成功（AGENTS.md §5）。
	RightsRPC zrpc.RpcClientConf `json:",optional"`

	// Sign 是 CDN 防盗链签名配置。
	Sign SignConfig

	// Kafka 是事件发布链路配置（playback_outbox → playback.heartbeat.v1）。
	// Enabled=false（示例配置的默认值）时本进程不投递，outbox 只累积；
	// 置 true 而二进制没链接队列运行时（`-tags playback_kafka`）时，
	// 构造 ServiceContext 直接失败并写日志，不会「安静地不发事件」。
	// 参数校验见 internal/publisher.ValidatePublishKafka。
	// 刻意没有 Group 键：本服务只做生产，没有任何代码读它。
	Kafka KafkaConf
}

// KafkaConf 是 Outbox 发布循环的参数（与 live-ingest 的同名结构逐键一致，
// 便于运维用同一份 yaml 模板；消费侧的 Offset/Conns/Consumers 这里没有）。
type KafkaConf struct {
	// Enabled 是否随进程启动 Outbox 发布器。默认 false，示例配置钉住该值。
	// 置 true 需要先 `-tags playback_kafka` 构建，否则 NewSender 返回
	// ErrKafkaRuntimeNotBuilt（本仓库从未与真实 broker 联调，见 README「已知缺口」）。
	Enabled bool `json:",default=false"`
	// Brokers Kafka/Redpanda 地址列表。本地 compose 的 redpanda 为 127.0.0.1:9092。
	// 注意：go-queue v1.2.2 的 kq.NewPusher 不暴露 SASL/TLS 注入口，
	// 因此这里没有 Username/Password/CaFile 三个键，带鉴权的集群不可用。
	Brokers []string `json:",optional"`
	// PublishTopics Outbox 发布的目标 topic。本服务只产出 playback.heartbeat.v1
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

// SignConfig 是 CDN A 型防盗链签名配置。
// PrivateKey 生产环境必须从 Secret/Vault 注入，禁止提交仓库；
// 留空时 GetPlaybackToken 返回 playback: cdn private key is not configured，
// 绝不签发无签名地址（AGENTS.md §6）。
type SignConfig struct {
	// BaseURL CDN 访问域名，如 https://play.example.com。
	BaseURL string
	// PrivateKey 防盗链私钥，参与 md5hash 计算，永不下发给客户端。
	PrivateKey string
	// KeyID 当前签名密钥标识，用于密钥轮换排障。
	KeyID string
	// TokenTTL 签名地址有效期（秒），默认 1800。
	TokenTTL int64 `json:",optional"`
	// EnableAuthKey 是否启用 CDN 鉴权串。生产必须为 true；
	// 仅 dev/test 模式允许 false（本地直连 MinIO 冒烟）。
	EnableAuthKey bool `json:",optional"`
}
