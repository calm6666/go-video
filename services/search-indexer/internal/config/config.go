// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 search-indexer 服务的配置结构（AGENTS.md §4：骨架由 goctl 生成后按需扩展）。
// 约定：密码/密钥类字段只允许写环境变量注入，示例配置里一律留空；
// 留空导致的写失败必须返回明确错误（见 esclient.ErrWriteGuarded），不允许静默成功。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 写入索引解析缓存与别名切换互斥锁。
	// 不能命名为 Redis：与 zrpc.RpcServerConf 内嵌的 RedisKeyConf 同 key 会导致配置加载失败。
	CacheRedis redis.RedisConf `json:",optional"`

	// DataSource 本服务自有库 DSN（go_video_search_indexer）。
	// 只允许访问本服务的四张表，禁止连上游领域服务的库（AGENTS.md §5）。
	DataSource string

	// Kafka 事件消费配置（content.published.v1 / engagement.action.v1）。
	Kafka KafkaConf

	// OpenSearch 索引写入与别名切换配置。
	OpenSearch OpenSearchConf
}

// KafkaConf 消费与后台重试参数。
//
// 队列侧只有一条路径：`Handler`（推送）+ `Supervisor`，Kafka 客户端只出现在
// `internal/consumer/kafkaruntime_kafka.go`（`-tags searchindexer_kafka`）里，默认构建由
// `kafkaruntime_disabled.go` 返回 ErrKafkaRuntimeNotBuilt。接线不需要任何 go.mod/go.sum 变更
// （`go-queue` 已是 go.mod 的直接 require），但**本仓库从未与真实 broker 联调**，
// 因此 Enabled=true 只在链接了运行时的构建里有意义，证据边界见 README「已知缺口」1。
//
// 退避重试与死信状态机不依赖 MQ，直接跑在 search_consumer_offset 上
// （RetrySweepIntervalSec 控制轮询），Enabled=false 时清扫器照常工作。
type KafkaConf struct {
	// Enabled 决定进程是否随 RPC 启动 Kafka 消费者。默认 false。
	// 置 true 而运行时未链接、或下面的消费参数不完整时，svc 构造直接失败并写日志，
	// 不会「安静地不消费」（见 internal/svc 的 startConsumer 与 consumer.ValidateKafka）。
	Enabled bool `json:",default=false"`
	// Brokers Kafka broker 地址列表，本地 compose 的 redpanda 为 127.0.0.1:9092。
	Brokers []string `json:",optional"`
	// Group 消费组名。
	Group string `json:",default=search-indexer.v1"`
	// Topics 订阅的事件 topic（对齐 docs/api-and-events.md §5 的版本化命名）。
	// 本服务只消费 content.published.v1 / engagement.action.v1，其余类型会在消费侧被判为不支持而跳过。
	Topics []string `json:",optional"`
	// Offset 消费组没有位点时的起点。
	Offset string `json:",options=first|last,default=last"`
	// Conns 每个 topic 建立的 reader 连接数。
	Conns int `json:",default=1"`
	// Consumers 每条连接的拉取协程数。
	Consumers int `json:",default=2"`
	// Processors 每条连接的并发处理协程数。
	Processors int `json:",default=4"`
	// ForceCommit 处理失败时是否仍提交位点。默认 false：让 Kafka 重投，
	// 重投会被 search_consumer_offset 的 event_id 唯一索引判成重复而跳过。
	ForceCommit bool `json:",default=false"`
	// Username SASL 用户名。生产只由环境变量/Secret 注入，示例配置留空。
	Username string `json:",optional"`
	// Password SASL 口令，必须与 Username 成对出现（见 consumer.ValidateKafka）。
	Password string `json:",optional"`
	// CaFile TLS 根证书路径；为空表示不启用 TLS。证书内容不进仓库。
	CaFile string `json:",optional"`
	// MaxRetries 单事件累计尝试上限（含首次），达到即转死信。
	MaxRetries int `json:",default=5"`
	// InProcessAttempts 单条消息进程内即时重试次数（应对瞬时抖动）。
	InProcessAttempts int `json:",default=2"`
	// RetryBackoffSec 退避基数（秒）。
	RetryBackoffSec int64 `json:",default=5"`
	// MaxRetryBackoffSec 退避上限（秒）。
	MaxRetryBackoffSec int64 `json:",default=1800"`
	// RetrySweepIntervalSec 重试清扫器空转时的轮询间隔（秒）。
	RetrySweepIntervalSec int64 `json:",default=10"`
	// RetryBatchLimit 单轮清扫处理的到期事件数。
	RetryBatchLimit int `json:",default=50"`
	// RetrySweeperEnabled 是否在本进程启动重试清扫器。
	RetrySweeperEnabled bool `json:",default=true"`
	// RebuildRunnerEnabled 是否在本进程启动重建任务执行器。
	RebuildRunnerEnabled bool `json:",default=true"`
	// RebuildPollIntervalSec 重建执行器无任务时的轮询间隔（秒）。
	RebuildPollIntervalSec int64 `json:",default=15"`
}

// OpenSearchConf 索引客户端与投影参数。
type OpenSearchConf struct {
	// Endpoints OpenSearch 节点地址列表（含 scheme）。留空则写读路径全部拒绝启动。
	Endpoints []string `json:",optional"`
	// IndexPrefix 别名前缀，同时是默认查询别名（search-query 读同一个别名）。
	IndexPrefix string `json:",default=go_video_content"`
	// SchemaVersion 当前 mapping 版本（v1）；变更字段结构时递增并走「新索引 + 别名切换」。
	SchemaVersion string `json:",default=v1"`
	// Username 基础认证用户名。
	Username string `json:",optional"`
	// Password 基础认证密码，只由 Secret/环境变量注入，示例配置留空。
	Password string `json:",optional"`
	// TimeoutMs 单次 HTTP 请求超时（毫秒）。
	TimeoutMs int64 `json:",default=5000"`
	// MaxRetries 幂等请求的额外重试次数（非幂等的 _aliases/_reindex/_update 不重试）。
	MaxRetries int `json:",default=2"`
	// BulkActions 单次 _bulk 的最大动作数，超过自动分片。
	BulkActions int `json:",default=500"`
	// AllowAnonymousWrites 无密码时是否允许写（仅限本地匿名集群；生产必须 false）。
	AllowAnonymousWrites bool `json:",default=false"`
	// ReindexSliceSpan 重建时 content_id 区间宽度。
	ReindexSliceSpan int64 `json:",default=100000"`
	// ReindexSliceSize 单次 _reindex 的服务端文档数上限。
	ReindexSliceSize int `json:",default=2000"`
	// StopAfterEmptySlices 连续多少个空切片后判定重建结束。
	StopAfterEmptySlices int `json:",default=3"`
	// RetryOnConflict 部分更新的服务端冲突重试次数。
	RetryOnConflict int `json:",default=3"`
	// Analyzer 建索引时写入的分词策略；改分词族等于改索引结构，必须递增 SchemaVersion
	// 并走重建 + 切别名流程（见 deploy/opensearch/README.md）。
	Analyzer AnalyzerConf `json:",optional"`
}

// AnalyzerConf 中文分词配置。mapping 只由 esclient.IndexBody 生成，本段仅决定选哪套
// tokenizer/filter；脚本侧用 `go run ./services/search-indexer/cmd/esmapping` 取同一份 JSON。
type AnalyzerConf struct {
	// Kind 分词插件族：
	//   cjk（默认）内置 standard tokenizer + cjk bigram filter，无需集群插件；
	//   ik 需要 analysis-ik 插件（index=ik_max_word / search=ik_smart）；
	//   smartcn 需要 analysis-smartcn 插件；
	//   standard 不做中文切分，只用于纯英文或 ID 调试。
	Kind string `json:",default=cjk,options=cjk|ik|smartcn|standard"`
	// StopwordsPath 停用词文件在集群容器内的绝对路径，挂在写入分析器上；
	// 改内容需要重建索引。compose 已把 deploy/opensearch/analysis 挂载到
	// /usr/share/opensearch/config/analysis。留空表示不过滤停用词。
	StopwordsPath string `json:",optional"`
	// SynonymsPath 同义词文件在集群容器内的绝对路径，只挂查询分析器（updateable），
	// 改内容 reload 即可生效，不必重建索引。留空表示不做同义词扩展。
	SynonymsPath string `json:",optional"`
}
