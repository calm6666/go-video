// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"os"

	"go-video/common/ratelimit"
	"go-video/services/event-collector/internal/config"
	"go-video/services/event-collector/model"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"
)

// ServiceContext 是 event-collector 的运行时上下文：MySQL 连接、缓存、五张表的 model
// 与可选下游 RPC 客户端。logic 只通过这里取依赖，不在方法内建连接或建 model。
type ServiceContext struct {
	Config config.Config

	// DB 是 go_video_event_collector 库连接。接收路径的事务由 logic 侧
	// DB.TransactCtx 发起，把「批次行 + 事件台账行 + Outbox 行」一次提交（AGENTS.md §5）。
	DB sqlx.SqlConn

	// Cache 是批次幂等短路透查、限流计数窗口与 ACTIVE 策略缓存。
	// 真值始终在 MySQL：缓存不可用时必须回源，不允许因缓存缺失拒绝采集。
	Cache *redis.Redis

	// Batches ec_ingest_batch：批次幂等与校验/投递结论汇总（计数为可重算投影）。
	Batches model.IngestBatchModel
	// Records ec_event_record：逐条 event_id 去重键与校验结论、投递状态台账。
	Records model.EventRecordModel
	// Policies ec_dispatch_policy：采样/脱敏配置版本，uniq_active 保证单生效版本。
	Policies model.DispatchPolicyModel
	// Pending ec_pending_delivery：MQ 投递 Outbox（event_id+topic 唯一、退避与租约）。
	Pending model.PendingDeliveryModel
	// DeadLetters ec_dead_letter：投递死信与处置审计。
	DeadLetters model.DeadLetterModel

	// SPM 是 spm 的 zrpc 客户端（事件 schema 真值/登记）；未配置时为 nil。
	// 未配置时依赖它的 logic 返回 model.ErrSpmRPCNotConfigured，
	// 不伪造「schema 已核对」。归属争议见 services/event-collector/README.md「疑点」。
	SPM zrpc.Client
	// RiskControl 是 risk-control 的 zrpc 客户端（限流与设备/IP 黑名单真值）；
	// 未配置时为 nil，相关判定返回 model.ErrRiskControlNotConfigured 而不是放行。
	RiskControl zrpc.Client

	// GlobalLimiter 进程级令牌桶（复用 common/ratelimit），保护 MySQL 与下游 MQ：
	// 采集入口的第一道容量闸门，触发即整批 DEFERRED + retry_after_ms。
	GlobalLimiter ratelimit.Limiter
}

// NewServiceContext 构造 ServiceContext。
//
// 依赖缺失策略：配置自检失败直接 Severe 终止启动（宁可不启动，也不带着
// 「IP 段前缀 /32 = 存明文 IP」这类危险配置上线）；
// 下游 RPC 与 MQ 未配置只记日志、不 panic，让服务可独立启动，
// 但相关方法一律返回明确的 model.Err* 而不是伪造成功。
func NewServiceContext(c config.Config) *ServiceContext {
	if err := c.Validate(); err != nil {
		logx.Severe("event-collector/svc: 配置自检失败: ", err)
	}

	conn := sqlx.NewMysql(c.DataSource)
	qps := int(c.Collector.GlobalQps)
	burst := int(c.Collector.GlobalBurst)
	if burst < qps {
		burst = qps
	}

	return &ServiceContext{
		Config:        c,
		DB:            conn,
		Cache:         redis.MustNewRedis(c.CacheRedis),
		Batches:       model.NewIngestBatchModel(conn),
		Records:       model.NewEventRecordModel(conn),
		Policies:      model.NewDispatchPolicyModel(conn),
		Pending:       model.NewPendingDeliveryModel(conn),
		DeadLetters:   model.NewDeadLetterModel(conn),
		SPM:           newClientIfConfigured(c.SPMRPC, "SPMRPC"),
		RiskControl:   newClientIfConfigured(c.RiskControlRPC, "RiskControlRPC"),
		GlobalLimiter: ratelimit.NewTokenBucket(qps, burst),
	}
}

// newClientIfConfigured 仅在给出可用发现配置时构造客户端；构造失败只记日志返回 nil，
// 由 logic 在请求路径上返回明确的 model.Err*NotConfigured（禁止把「没接上」当成「已核对」）。
func newClientIfConfigured(conf zrpc.RpcClientConf, name string) zrpc.Client {
	if len(conf.Endpoints) == 0 && conf.Target == "" && len(conf.Etcd.Hosts) == 0 {
		logx.Infof("event-collector/svc: %s 未配置，依赖它的判定将返回 not configured", name)
		return nil
	}
	cli, err := zrpc.NewClient(conf)
	if err != nil {
		logx.Errorf("event-collector/svc: 初始化 %s 客户端失败: %v", name, err)
		return nil
	}
	return cli
}

// Salt 读取当前脱敏盐值：优先配置显式注入（仅本地/测试），否则按 Privacy.SaltRef
// 指向的环境变量取（生产由 Secret 注入为环境变量，盐值本身永不入库、不打日志）。
// 取不到返回 model.ErrSaltMissing，调用方必须拒绝写入而不是退化成无盐哈希。
func (s *ServiceContext) Salt() (string, error) {
	if v := s.Config.Privacy.SaltValue; v != "" {
		return v, nil
	}
	if ref := s.Config.Privacy.SaltRef; ref != "" {
		if v := os.Getenv(ref); v != "" {
			return v, nil
		}
	}
	return "", model.ErrSaltMissing
}

// DispatcherEnabled 报告 MQ 投递通道是否可用（Dispatch.Endpoints 非空）。
// false 时 RetryPendingDelivery/CollectEvents 的投递推进只能推进 Outbox 状态，
// logic 必须返回 model.ErrDispatcherMissing 而不是把台账标成 SENT。
func (s *ServiceContext) DispatcherEnabled() bool {
	return s.Config.DispatcherEnabled()
}

// SPMClient 返回 spm 客户端，未配置时给出明确错误。
func (s *ServiceContext) SPMClient() (zrpc.Client, error) {
	if s.SPM == nil {
		return nil, model.ErrSpmRPCNotConfigured
	}
	return s.SPM, nil
}

// RiskControlClient 返回 risk-control 客户端，未配置时给出明确错误。
func (s *ServiceContext) RiskControlClient() (zrpc.Client, error) {
	if s.RiskControl == nil {
		return nil, model.ErrRiskControlNotConfigured
	}
	return s.RiskControl, nil
}
