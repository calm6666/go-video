// Code scaffolded by goctl. Safe to edit.

package config

import (
	"errors"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 event-collector 服务的配置结构。
//
// 本服务只提供 gRPC（AGENTS.md §4）：对端 SDK 的 HTTP 入口、统一鉴权与响应信封在
// gateway/app，本服务负责接收台账、逐条校验、采样与脱敏、投递推进与死信处置。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 承载批次幂等短路透查、限流计数窗口与生效策略的进程外缓存。
	// 不能命名为 Redis：zrpc.RpcServerConf 已内嵌同名 RedisKeyConf 字段，
	// 同名会让 conf.Load 报 "conflict key redis"，全仓统一用 CacheRedis。
	CacheRedis redis.RedisConf

	// DataSource 是 go_video_event_collector 库的 MySQL DSN。
	// 该库只存接收台账与投递状态，绝不存事件明文正文（AGENTS.md §7）。
	DataSource string

	// Collector 是采集与投递的运行时参数（批量上限、限流、租约与退避、留存）。
	// 注意：这些是「服务端硬上限的默认值」，每条批次实际以 ACTIVE 策略
	// （ec_dispatch_policy）里的同名列裁决；策略缺失时才回落到这里的值。
	Collector CollectorConf

	// Privacy 是脱敏参数：盐值本身只能从 Secret/环境变量注入，示例配置必须留空。
	Privacy PrivacyConf

	// SPMRPC 是 spm 的 zrpc client 配置（事件 schema 真值与登记，见 README 疑点）。
	// 未配置时 svc 不构造客户端，依赖它的 logic 返回 model.ErrSpmRPCNotConfigured。
	SPMRPC zrpc.RpcClientConf `json:",optional"`

	// RiskControlRPC 是 risk-control 的 zrpc client 配置（限流/黑名单真值判定）。
	// 未配置时相关判定返回 model.ErrRiskControlNotConfigured，绝不伪造「风控已通过」。
	RiskControlRPC zrpc.RpcClientConf `json:",optional"`

	// Dispatch 是 MQ 投递参数（common/eventenvelope + kq）。
	// Endpoints 为空表示不启用 dispatcher：事件仍会落 ec_pending_delivery，
	// 由 services/cron 的 RetryPendingDelivery 兜底推进，不允许「发不出去就当已发送」。
	Dispatch DispatchConf
}

// CollectorConf 采集链路运行参数，全部来自 etc yaml 或配置中心。
type CollectorConf struct {
	// PageSize 台账翻页默认条数。
	PageSize int32 `json:",default=50"`
	// MaxPageSize 服务端允许的每页上限，超过返回 model.ErrInvalidPage。
	MaxPageSize int32 `json:",default=500"`
	// MaxEventsPerBatch 单请求事件条数上限（ACTIVE 策略缺省时的兜底值）。
	MaxEventsPerBatch int32 `json:",default=200"`
	// MaxRequestBytes 单请求字节上限（防超大 body 打爆内存与 MySQL 包）。
	MaxRequestBytes int64 `json:",default=1048576"`
	// MaxEventPayloadBytes 单事件 payload 字节上限；超限即 REJECT_PAYLOAD_TOO_LARGE。
	MaxEventPayloadBytes int32 `json:",default=8192"`
	// MaxClockSkewSeconds 允许的 occurred_at 偏差绝对值（REJECT_CLOCK_SKEW）。
	MaxClockSkewSeconds int32 `json:",default=300"`
	// MaxBackfillSeconds 允许的回补窗口（离线补报最大年龄，REJECT_EVENT_TOO_OLD）。
	MaxBackfillSeconds int32 `json:",default=86400"`
	// KeywordMaxRunes 搜索词截断长度（超出只保留前 N 个 rune，且只存摘要）。
	KeywordMaxRunes int32 `json:",default=64"`
	// DefaultSampleBps 无匹配采样规则时的采样比例（默认全量，宁可多存不可丢数据）。
	DefaultSampleBps int32 `json:",default=10000"`
	// SupportedSchemaVersion 本服务支持的最大事件 schema_version。
	SupportedSchemaVersion int32 `json:",default=1"`
	// IPSegmentBits 出口 IP 脱敏前缀长度（默认 /24）。
	IPSegmentBits int `json:",default=24"`
	// MidQps / DeviceQps / IPSegmentQps 三个维度的进程内限流阈值（真值判定仍以
	// risk-control 为准，这里是保护自身的下限，触发即 REJECT_RATE_LIMITED）。
	MidQps       int32 `json:",default=200"`
	DeviceQps    int32 `json:",default=200"`
	IPSegmentQps int32 `json:",default=2000"`
	// GlobalQps / GlobalBurst 进程级写接口令牌桶（复用 common/ratelimit）。
	GlobalQps   int32 `json:",default=2000"`
	GlobalBurst int32 `json:",default=800"`
	// DegradedRejectRatioBps 降级期间的丢弃比例（基点）：依赖故障时按此比例 SAMPLED_OUT，
	// 而不是整批拒绝，保证客户端不会无限重试放大流量。0 表示不降级丢数据。
	DegradedRejectRatioBps int32 `json:",default=0"`
	// ReportIntervalSeconds 正常期建议客户端上报间隔（CollectEventsReply 回带）。
	ReportIntervalSeconds int32 `json:",default=10"`
	// RetryHintMs decision=DEFERRED / REJECT_RATE_LIMITED 时回带给客户端的退避提示。
	RetryHintMs int32 `json:",default=1000"`
	// RetentionDays 接收台账（批次/事件/投递行）保留天数，超期由 services/cron 清理。
	RetentionDays int32 `json:",default=30"`
	// DeadLetterRetentionDays 已处置死信的保留天数。open 行永不清理（审计证据）。
	DeadLetterRetentionDays int32 `json:",default=90"`
	// BatchLimit 单次操作（重放/清理）的行数上限。
	BatchLimit int32 `json:",default=200"`
	// MaxReplayPerRequest 单次 ReplayDeadLetter 允许的 ID 数（proto MaxReplayPerRequest）。
	MaxReplayPerRequest int32 `json:",default=100"`
}

// PrivacyConf 脱敏参数。
//
// 铁律：盐值不入库、不进仓库、不打日志。SaltRef 指向的环境变量缺失时，
// 采集路径必须拒绝写入并返回 model.ErrSaltMissing，
// 绝不允许退化成「无盐 sha256 设备号」（可被枚举还原 = 等于明文入库）。
type PrivacyConf struct {
	// SaltRef 当前盐值所在的环境变量名（如 EVENT_COLLECTOR_SALT_V1），只存名字。
	SaltRef string `json:",default=EVENT_COLLECTOR_SALT_V1"`
	// SaltVersion 与盐值配对的版本号；轮换盐必须同时升版本并新建策略版本。
	SaltVersion int32 `json:",default=1"`
	// SaltValue 仅供本地/测试环境显式注入；生产必须留空并从 Secret 读取。
	// 一旦非空，示例配置自检会失败（见 config_load_test）。
	SaltValue string `json:",optional"`
}

// DispatchConf MQ 投递参数。
type DispatchConf struct {
	// Endpoints Kafka 地址列表；为空表示 dispatcher 未启用（事件留在 Outbox 由 cron 推进）。
	Endpoints []string `json:",optional"`
	// BatchSize RetryPendingDelivery 单轮默认扫描条数（proto limit=0 时使用）。
	BatchSize int32 `json:",default=200"`
	// DeliverMaxAttempts 投递重试上限，超过转 ec_dead_letter。
	DeliverMaxAttempts int32 `json:",default=8"`
	// RetryBaseSeconds 退避基数（delay = base * 2^(attempts-1)）。
	RetryBaseSeconds int64 `json:",default=5"`
	// RetryMaxSeconds 退避上限。
	RetryMaxSeconds int64 `json:",default=3600"`
	// LeaseSeconds 投递租约时长：worker 崩溃后其他实例接管的最小等待。
	LeaseSeconds int64 `json:",default=60"`
	// RetryScanLookaheadSeconds next_retry_at 查询窗口，避免全表扫描。
	RetryScanLookaheadSeconds int64 `json:",default=86400"`
}

// Validate 启动期自检：把「配置能加载但语义危险」的组合在启动阶段就暴露。
// 返回 error 时 svc.NewServiceContext 直接 Severe 终止启动。
//
// 刻意不 import model：config 只依赖 go-zero，保持配置层不反向依赖领域层。
func (c Config) Validate() error {
	col := c.Collector
	if col.MaxEventsPerBatch <= 0 || col.MaxEventsPerBatch > 5000 {
		return errors.New("Collector.MaxEventsPerBatch 必须落在 (0,5000]：过大等于给上游 DoS 开口子")
	}
	if col.MaxRequestBytes <= 0 || col.MaxRequestBytes > 8<<20 {
		return errors.New("Collector.MaxRequestBytes 必须落在 (0,8MiB]")
	}
	if col.MaxEventPayloadBytes < 256 || int64(col.MaxEventPayloadBytes) > col.MaxRequestBytes {
		return errors.New("Collector.MaxEventPayloadBytes 必须 >= 256 且不超过 MaxRequestBytes，" +
			"否则单事件上限形同虚设")
	}
	if col.MaxPageSize < col.PageSize || col.PageSize <= 0 {
		return errors.New("Collector.MaxPageSize 不能小于 PageSize，且 PageSize 必须 > 0")
	}
	if col.MaxClockSkewSeconds <= 0 {
		return errors.New("Collector.MaxClockSkewSeconds 必须 > 0，否则任何客户端时钟偏差都会整批拒绝")
	}
	if col.MaxBackfillSeconds <= col.MaxClockSkewSeconds {
		return errors.New("Collector.MaxBackfillSeconds 必须大于 MaxClockSkewSeconds，否则离线回补永远被拒")
	}
	if col.DefaultSampleBps < 0 || col.DefaultSampleBps > 10000 {
		return errors.New("Collector.DefaultSampleBps 必须落在 [0,10000]（基点，10000 = 全量）")
	}
	if col.SupportedSchemaVersion < 1 {
		return errors.New("Collector.SupportedSchemaVersion 必须 >= 1，否则所有事件都会被 REJECT_UNSUPPORTED_SCHEMA_VERSION")
	}
	if col.KeywordMaxRunes <= 0 {
		return errors.New("Collector.KeywordMaxRunes 必须 > 0")
	}
	if col.IPSegmentBits <= 0 || col.IPSegmentBits >= 32 {
		return errors.New("Collector.IPSegmentBits 必须落在 (0,32)：/32 等于把出口 IP 原样入库（AGENTS.md §7）")
	}
	if col.GlobalQps <= 0 || col.GlobalBurst <= 0 {
		return errors.New("Collector.GlobalQps/GlobalBurst 必须 > 0，否则限流器会把所有请求拒掉")
	}
	if col.DegradedRejectRatioBps < 0 || col.DegradedRejectRatioBps > 10000 {
		return errors.New("Collector.DegradedRejectRatioBps 必须落在 [0,10000]")
	}
	if col.RetentionDays <= 0 || col.DeadLetterRetentionDays <= 0 {
		return errors.New("台账与死信必须有保留上限（RetentionDays/DeadLetterRetentionDays > 0）")
	}
	if col.BatchLimit <= 0 || col.MaxReplayPerRequest <= 0 || col.MaxReplayPerRequest > col.BatchLimit {
		return errors.New("Collector.MaxReplayPerRequest 必须 > 0 且不超过 BatchLimit")
	}
	if c.Privacy.SaltVersion <= 0 || c.Privacy.SaltRef == "" {
		return errors.New("Privacy.SaltRef/SaltVersion 必须配置：脱敏哈希没有盐版本就无法轮换，也无法解释历史数据")
	}
	d := c.Dispatch
	if d.BatchSize <= 0 || d.BatchSize > 5000 {
		return errors.New("Dispatch.BatchSize 必须落在 (0,5000]")
	}
	if d.DeliverMaxAttempts <= 0 {
		return errors.New("Dispatch.DeliverMaxAttempts 必须 > 0，否则失败事件立即变死信")
	}
	if d.RetryBaseSeconds <= 0 || d.RetryMaxSeconds < d.RetryBaseSeconds {
		return errors.New("Dispatch 的退避参数不合法：RetryBaseSeconds>0 且 RetryMaxSeconds>=RetryBaseSeconds")
	}
	if d.LeaseSeconds <= 0 {
		return errors.New("Dispatch.LeaseSeconds 必须 > 0，否则投递中状态无法回收")
	}
	if d.LeaseSeconds >= d.RetryMaxSeconds {
		return errors.New("Dispatch.LeaseSeconds 必须小于 RetryMaxSeconds，否则退避等待会被误判为崩溃接管")
	}
	if d.RetryScanLookaheadSeconds <= 0 {
		return errors.New("Dispatch.RetryScanLookaheadSeconds 必须 > 0")
	}
	return nil
}

// SaltConfigured 判断盐是否可用（SaltValue 只允许本地/测试环境显式注入）。
// 未注入时 logic 的哈希路径返回 model.ErrSaltMissing，不静默降级。
func (c Config) SaltConfigured() bool {
	return c.Privacy.SaltValue != ""
}

// DispatcherEnabled 判断是否配置了 MQ 地址；false 时 svc 不构造 producer，
// 事件只落 Outbox 由 RetryPendingDelivery 推进（README「已知缺口」）。
func (c Config) DispatcherEnabled() bool {
	return len(c.Dispatch.Endpoints) > 0
}
