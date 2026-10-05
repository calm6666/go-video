// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 cron 服务的配置结构。
//
// 加载方式与约束（AGENTS.md §4）：由 conf.Load 从 etc/*.yaml 加载，密钥/口令只从
// 环境变量或配置中心注入，示例配置里不得出现真实凭据；internal/config/config_load_test.go
// 会对 etc 下每个 yaml 做真实加载校验。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 只用于到期索引缓存与健康度计数缓存，真值恒在 MySQL。
	// 不能命名为 Redis：与 zrpc.RpcServerConf 内嵌的 RedisKeyConf 同名会触发
	// "conflict key redis"，代码可编译但服务启动即失败（见 config_load_test.go）。
	CacheRedis redis.RedisConf

	// DataSource go_video_cron 库 DSN。
	DataSource string

	// Scheduler 调度循环（worker tick）参数。
	Scheduler SchedulerConf

	// Lease 租约与心跳的服务端硬约束。
	Lease LeaseConf

	// Task 任务定义与执行记录的业务参数（分页上限、留存天数等）。
	Task TaskConf

	// --- 下游领域服务 RPC（cron 只能通过它们推进业务状态，AGENTS.md §5）---
	// Target 与 Etcd.Hosts 都为空的客户端不构造，对应任务在运行时以
	// ErrDownstreamNotConfigured 失败并进入退避，绝不退化成「绕过 RPC 直接写别人的库」。

	// RightsRPC 版权窗口到期扫描与失效（rights.ListExpiring / ExpireWindow）。
	RightsRPC zrpc.RpcClientConf `json:",optional"`
	// CatalogRPC 版权集下架/上架（catalog.OfflineEpisode / PublishEpisode）。
	CatalogRPC zrpc.RpcClientConf `json:",optional"`
	// VideoRPC 稿件状态机推进（video.TransitionState / ListByState）。
	VideoRPC zrpc.RpcClientConf `json:",optional"`
	// SearchIndexerRPC 索引重建与别名巡检（search-indexer.SubmitRebuildTask / GetRebuildTask）。
	SearchIndexerRPC zrpc.RpcClientConf `json:",optional"`
	// NotificationRPC 投递死信重放与投递状态报表。
	NotificationRPC zrpc.RpcClientConf `json:",optional"`
	// EngagementRPC 计数修复与互动报表任务。
	EngagementRPC zrpc.RpcClientConf `json:",optional"`
	// InboxRPC 站内信未读计数重算（inbox.RecomputeUnread）。
	InboxRPC zrpc.RpcClientConf `json:",optional"`
}

// SchedulerConf 本进程是否随服务启动调度循环，以及 tick 与批量参数。
type SchedulerConf struct {
	// Enabled 控制进程内调度循环。多副本部署时每个副本都会 tick，
	// 正确的并发由 cron_task_lease + cron_task_run 唯一键裁决（不需要单实例 leader）。
	Enabled bool `json:",default=true"`
	// TickSeconds 到期扫描间隔；过小会空转扫库，过大则错过分钟级任务。
	TickSeconds int `json:",default=5"`
	// BatchSize 单轮最多拉取多少个到期任务（ListDueTasks.limit 的服务端默认值）。
	BatchSize int32 `json:",default=64"`
	// LookaheadSeconds 允许预取的提前量，0 表示只处理已到期项。
	LookaheadSeconds int64 `json:",default=0"`
	// MaxConcurrentRuns 单实例同时执行的跑批数量上限，防止把下游打爆。
	MaxConcurrentRuns int `json:",default=8"`
	// ReclaimIntervalSeconds 孤儿执行回收扫描间隔（RUNNING 且租约已过期的行）。
	ReclaimIntervalSeconds int `json:",default=60"`
	// WorkerIDPrefix 实例标识前缀；留空时使用 hostname-pid-random。
	WorkerIDPrefix string `json:",optional"`
	// GracefulStopSeconds 收到退出信号后等待在途执行收尾的秒数，
	// 到期只停止领取新任务，不强制中断正在跑的执行（租约自然过期由其它实例接管）。
	GracefulStopSeconds int `json:",default=30"`
}

// LeaseConf 租约参数的服务端上下界与心跳节奏。
type LeaseConf struct {
	// HeartbeatSeconds 执行中任务的续租间隔，必须显著小于 DefaultTTLSeconds。
	HeartbeatSeconds int `json:",default=20"`
	// DefaultTTLSeconds 任务定义未给出 lease_ttl_seconds 时使用的兜底 TTL。
	DefaultTTLSeconds int64 `json:",default=300"`
	// MinTTLSeconds / MaxTTLSeconds 允许的最小/最大租约 TTL。
	MinTTLSeconds int64 `json:",default=30"`
	// MaxTTLSeconds 上限：超过一天说明任务该拆分成多段。
	MaxTTLSeconds int64 `json:",default=86400"`
	// PreemptionEnabled false 时禁止抢占过期租约（只用于排障窗口：
	// 需要冻结所有执行、人工确认副作用时临时关闭）。
	PreemptionEnabled bool `json:",default=true"`
	// AbortOnLeaseLost true（默认）时，续租发现租约丢失立即取消本次执行；
	// 任何情况下都不允许在丢失租约后继续写下游，配置只影响「是否主动取消」。
	AbortOnLeaseLost bool `json:",default=true"`
}

// TaskConf 任务定义与执行记录的业务参数。
type TaskConf struct {
	// DefaultPageSize / MaxPageSize 列表接口分页默认值与上限。
	DefaultPageSize int32 `json:",default=20"`
	// MaxPageSize 上限，防止深分页拖垮数据库（docs/api-and-events.md §2）。
	MaxPageSize int32 `json:",default=100"`
	// MaxParamsBytes 任务 params 文本上限。
	MaxParamsBytes int `json:",default=4096"`
	// MaxResultSummaryBytes 执行结果摘要入库前的截断长度。
	MaxResultSummaryBytes int `json:",default=1024"`
	// RunRetentionDays 执行记录保留天数，超期由本服务清理任务分批删除。
	RunRetentionDays int `json:",default=30"`
	// AuditRetentionDays 审计保留天数（比执行记录更久，满足下架/版权撤回回溯，AGENTS.md §8）。
	AuditRetentionDays int `json:",default=180"`
	// DeleteBatchSize 清理任务单轮删除行数上限，避免长事务与主从延迟。
	DeleteBatchSize int64 `json:",default=1000"`
	// DefaultTimezone 任务未声明 timezone 时使用的时区。
	DefaultTimezone string `json:",default=Asia/Shanghai"`
}
