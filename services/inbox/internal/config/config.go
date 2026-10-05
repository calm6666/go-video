// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// DefaultTopics 是 inbox 默认订阅的事件 topic（docs/api-and-events.md §5）。
// 与 deploy/docker-compose 起的 Redpanda 中的 topic 名保持一致。
var DefaultTopics = []string{
	"engagement.action.v1",
	"content.published.v1",
	"live.state.v1",
}

// Config 是 inbox 服务的配置结构。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 未读计数加速层；真值在 MySQL，缺失时按快照与明细重算。
	// 不能命名为 Redis：与 zrpc.RpcServerConf 内嵌字段同名会让配置加载报
	// "conflict key redis"，代码可编译但服务启动即失败（见 config_load_test.go）。
	CacheRedis redis.RedisConf

	// DataSource 是 go_video_inbox 库的 DSN。
	DataSource string

	// Kafka 是领域事件消费配置（Enabled=false 时不启动消费者）。
	Kafka KafkaConf

	// Inbox 是站内信业务参数。
	Inbox InboxConf
}

// KafkaConf 消费者配置。
// ForceCommit 默认 false：处理失败时不提交位点，由 Kafka 重投，
// 配合 inbox_consumer_offset 的退避状态实现至少一次 + 幂等。
type KafkaConf struct {
	// Enabled 控制进程是否随 RPC 启动消费者。Enabled=true 而消费配置不完整时不会静默跳过，
	// 而是由 svc.NewServiceContext 明确报错终止启动（见 consumer.ValidateKafka）。
	Enabled bool `json:",default=false"`
	// RetrySweeperEnabled 控制是否周期扫描 inbox_consumer_offset 中到期的 retry 事件并重投。
	// 这条链路只依赖 MySQL，不依赖 MQ：即使 Enabled=false（或未链接 Kafka 运行时），
	// 历史失败事件也要继续向前收敛，而不是永远停在 retry 状态。
	RetrySweeperEnabled bool `json:",default=true"`
	// Brokers Kafka broker 列表，本地 Redpanda 为 ["localhost:9092"]。
	Brokers []string `json:",optional"`
	// Group 消费组名。
	Group string `json:",default=inbox.v1.consumer"`
	// Topics 订阅 topic 列表，空表示使用 DefaultTopics。
	Topics []string `json:",optional"`
	// Offset 首次启动时的消费起点。
	Offset string `json:",options=first|last,default=last"`
	// Conns 每个 topic 建立的 reader 连接数。
	Conns int `json:",default=1"`
	// Consumers 每条连接的拉取协程数。
	Consumers int `json:",default=2"`
	// Processors 每条连接的并发处理协程数。
	Processors int `json:",default=4"`
	// ForceCommit 处理失败时是否仍提交位点（默认 false，保留重投）。
	ForceCommit bool `json:",default=false"`
	// MaxAttempts 单事件最大处理次数，超过后转入 inbox_dead_letter。
	MaxAttempts int32 `json:",default=8"`
	// RetryBaseSeconds 退避基数：第 n 次失败后等待 base * 2^(n-1) 秒。
	RetryBaseSeconds int64 `json:",default=10"`
	// RetryMaxSeconds 退避上限。
	RetryMaxSeconds int64 `json:",default=1800"`
	// StaleProcessingSeconds processing 状态超过该秒数视为进程崩溃遗留，可被重新抢占。
	StaleProcessingSeconds int64 `json:",default=300"`
	// Username/SASL 用户名。生产环境从环境变量/Secret 注入，示例配置留空。
	Username string `json:",optional"`
	// Password SASL 口令，与 Username 成对出现（见 consumer.ValidateKafka）。
	Password string `json:",optional"`
	// CaFile TLS 根证书路径；为空表示不启用 TLS。证书内容不进仓库。
	CaFile string `json:",optional"`
}

// EffectiveTopics 返回实际订阅的 topic 列表。
func (k KafkaConf) EffectiveTopics() []string {
	if len(k.Topics) == 0 {
		return DefaultTopics
	}
	return k.Topics
}

// InboxConf 站内信业务参数。
type InboxConf struct {
	// PageSize 请求未指定 ps 时的默认每页条数。
	PageSize int32 `json:",default=20"`
	// MaxPageSize 服务端允许的每页上限，超过返回 ErrPsTooLarge。
	MaxPageSize int32 `json:",default=50"`
	// MaxRecipients 单次 SendSystemMessage 的接收人上限（BroadcastMaxRecipients 语义）。
	MaxRecipients int32 `json:",default=500"`
	// UnreadCacheSeconds 未读计数在 Redis 中的存活秒数，到期后回源 DB 快照。
	UnreadCacheSeconds int `json:",default=1800"`
	// MaxContentBytes 标题与正文的字节上限，防止超长内容打爆下游展示。
	MaxContentBytes int `json:",default=4096"`
}
