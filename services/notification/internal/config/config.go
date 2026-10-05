// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 notification 服务的配置结构（AGENTS.md §4：配置骨架由 goctl 生成后按需扩展）。
// 所有供应商密钥只允许通过环境变量名（*Ref 字段）注入，配置文件里不得出现明文密钥。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 用于每日频次配额、模板与偏好的短缓存。
	// 不能命名为 Redis：与 zrpc.RpcServerConf 内嵌的 RedisKeyConf（RedisKey）同名，
	// 会让 conf.Load 报 "conflict key redis" 而使服务无法启动。
	CacheRedis redis.RedisConf

	// DataSource MySQL 主库 DSN（go_video_notification 库）。
	DataSource string

	// Kafka notification.request.v1 消费配置。
	Kafka KafkaConf

	// AccountRPC 预留的 account RPC 客户端配置。
	// 本期不建链：account.v1 契约中没有“按 mid 取手机号/邮箱”的方法（见 README 缺口章节），
	// 因此服务只保留配置位，不构造客户端、不发起连接。
	AccountRPC zrpc.RpcClientConf `json:",optional"`

	// Providers 各通道的外部投递适配器配置。
	Providers ProvidersConf

	// Notification 投递行为的全局策略。
	Notification NotificationConf
}

// KafkaConf Kafka 消费配置。
// Enabled=true 且以 `-tags notification_kafka` 构建时才会真正拉起消费者；
// 默认构建下 consumer.StartKafkaRuntime 返回显式错误，绝不伪造“已在消费”。
type KafkaConf struct {
	// Enabled 是否由本进程接管 notification.request.v1 消费。
	// false（默认）时本进程只按接口注入消费能力、不连 Kafka，并在启动日志里显式声明；
	// true 且二进制未带 `-tags notification_kafka` 构建时启动直接失败，绝不静默“假装在消费”。
	Enabled bool `json:",default=false"`
	// Brokers Kafka broker 地址列表，本地 compose 为 127.0.0.1:9092。
	Brokers []string `json:",optional"`
	// Group 消费组名。
	Group string `json:",default=notification.v1"`
	// RequestTopic 通知请求 topic，对齐 docs/api-and-events.md §5。
	RequestTopic string `json:",default=notification.request.v1"`
	// Offset 首次启动位点策略：first|last。
	Offset string `json:",options=first|last,default=last"`
	// Conns 连接数。
	Conns int `json:",default=1"`
	// Consumers 单连接消费者协程数。
	Consumers int `json:",default=4"`
	// Processors 处理协程数。
	Processors int `json:",default=8"`
	// MaxEventRetries 事件级重试上限，超过后写死信留档。
	MaxEventRetries int32 `json:",default=5"`
}

// ProvidersConf 每个通道一个投递适配器条目。
type ProvidersConf struct {
	Push  ProviderConf `json:",optional"`
	Sms   ProviderConf `json:",optional"`
	Email ProviderConf `json:",optional"`
}

// ProviderConf 单个通道的通用 HTTP 投递适配器配置。
// 仓库不引入任何厂商 SDK（AGENTS.md §2 禁止新增依赖），
// 供应商差异全部由 Endpoint/Method/Headers/BodyTemplate/签名方式表达。
type ProviderConf struct {
	// Enabled 关闭时该通道投递返回“通道未配置”。
	Enabled bool `json:",default=false"`
	// Name 适配器名字，落入 notification_delivery.provider。
	Name string `json:",default=generic-http"`
	// Endpoint 供应商 HTTP 投递地址。
	Endpoint string `json:",optional"`
	// Method HTTP 方法。
	Method string `json:",default=POST"`
	// TimeoutMs 单次请求超时（毫秒）。
	TimeoutMs int64 `json:",default=3000"`
	// MaxRetries 仅针对传输层错误的进程内即时重试次数（不含持久化退避重试）。
	MaxRetries int32 `json:",default=2"`
	// RetryDelayMs 即时重试间隔（毫秒）。
	RetryDelayMs int64 `json:",default=200"`
	// Headers 固定请求头，值支持 ${ENV_NAME} 占位。
	Headers map[string]string `json:",optional"`
	// BodyTemplate JSON 请求体模板，仅支持被双引号包裹的占位符：
	// {{target}} {{title}} {{body}} {{idempotency_key}} {{device_id}} {{delivery_id}} {{trace_id}}
	BodyTemplate string `json:",optional"`
	// SignMode 签名方式：none|bearer|apikey|hmac-sha256。
	SignMode string `json:",options=none|bearer|apikey|hmac-sha256,default=none"`
	// ApiKeyRef 存放 API Key 的环境变量名（不落明文）。
	ApiKeyRef string `json:",optional"`
	// SecretRef 存放签名密钥的环境变量名（不落明文）。
	SecretRef string `json:",optional"`
	// ApiKeyHeader bearer/apikey 模式使用的头名，默认 Authorization / X-Api-Key。
	ApiKeyHeader string `json:",optional"`
	// SuccessField 响应 JSON 中判定成功的字段（点号路径），为空表示只看 HTTP 状态码。
	SuccessField string `json:",optional"`
	// SuccessValue SuccessField 期望值（字符串比较）。
	SuccessValue string `json:",optional"`
	// MessageIDField 响应 JSON 中供应商回执 ID 的字段（点号路径）。
	MessageIDField string `json:",optional"`
}

// NotificationConf 投递策略。
type NotificationConf struct {
	// DailyQuotaPerMid 单用户单通道每日投递上限，0 表示不限制。
	DailyQuotaPerMid int32 `json:",default=50"`
	// DndEnabled 免打扰校验总开关。
	DndEnabled bool `json:",default=true"`
	// DefaultTimezone 用户未设置偏好时使用的时区（IANA 名）。
	DefaultTimezone string `json:",default=Asia/Shanghai"`
	// DefaultLanguage 用户与请求都未指定语言时的回落语言。
	DefaultLanguage string `json:",default=zh-CN"`
	// BackoffSeconds 退避阶梯（秒），索引=已重试次数；末尾值用于后续重试。
	BackoffSeconds []int64 `json:",optional"`
	// MaxDeliveryRetries 投递任务最大重试次数，超过转死信。
	MaxDeliveryRetries int32 `json:",default=5"`
	// SyncSend true 时 SendNotification 在落库后同步尝试一次投递；
	// false（默认）时只落任务，由投递调度器按退避扫描发送。取舍见 README。
	SyncSend bool `json:",default=false"`
	// DispatcherEnabled 是否在本进程启动投递调度器。
	DispatcherEnabled bool `json:",default=true"`
	// DispatchIntervalMs 调度扫描间隔（毫秒）。
	DispatchIntervalMs int64 `json:",default=1000"`
	// DispatchBatch 单次扫描取任务数。
	DispatchBatch int32 `json:",default=64"`
	// MaxRecipients 单次 SendNotification 允许的接收人数上限。
	MaxRecipients int32 `json:",default=200"`
}
