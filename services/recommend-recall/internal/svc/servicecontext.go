// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package svc

import (
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/proc"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/recommend-recall/internal/config"
	"go-video/services/recommend-recall/internal/publisher"
	"go-video/services/recommend-recall/internal/repository"
)

// ServiceContext 是 recommend-recall 服务的运行时上下文。
//
// 装配范围严格限定在"已经存在的东西"：本服务自有表的 model、业务缓存 MySQL 连接、
// 以及两个显式未接线的下游数据源 stub。
// 本轮没有装配任何 zrpc client —— spm、feature-store 的契约正在同批次实现，
// 提前 import 它们的 rpc 包等于把未冻结契约焊死；video、social-graph 虽有 rpc，
// 但在第二轮 logic 之前无人调用，配置里也不留 Endpoints，避免产生死配置。
// 接线点在 internal/repository 的 FeatureSource / VisibilitySource 接口，
// 逐条对应关系见 services/recommend-recall/README.md「待接线接口」。
type ServiceContext struct {
	Config config.Config
	// Repository 聚合自有表 model、事务入口与下游数据源。
	Repository *repository.Repository

	// Publisher recall_outbox → recall.pool.published.v1 的事件发布器。
	// Kafka.Enabled=false 时为 nil，此时本进程不投递事件，只在表里累积。
	// 非 nil 只说明二进制链接了 kq、写入通道建立成功、循环已在跑：
	// 本仓库从未与真实 broker 联调，「事件已送达」不在这个结论范围内。
	Publisher *publisher.Publisher
}

// NewServiceContext 构造 ServiceContext。
//
// 启动期即失败的两类配置错误（panic 而不是带病上线）：
//  1. Recall 参数超出 model 层硬上限，说明配置把 LIMIT 写没了；
//  2. 召回路开关自相矛盾（兜底路没开、默认组合含未启用的路），这种配置下
//     降级链路会在最该出数的时候报 pool_not_ready。
//
// 事件发布器是第三类：Kafka.Enabled=true 表示运维已经假定「事件在投递」，
// 这时建不出发送端或参数不合格必须启动即失败（logx.Must），
// 而不是带着一个投不出东西的对象对外服务。
func NewServiceContext(c config.Config) *ServiceContext {
	if err := c.Recall.Validate(); err != nil {
		logx.Errorf("recommend-recall: invalid Recall config: %v", err)
		panic(err)
	}

	conn := sqlx.NewMysql(c.DataSource)
	cache := redis.MustNewRedis(c.CacheRedis)

	repo, err := repository.New(conn, cache, repository.Options{
		MaxCandidates:               c.Recall.MaxCandidates,
		DefaultLimit:                c.Recall.DefaultLimit,
		PerSourceMax:                c.Recall.PerSourceMax,
		MaxSeedAids:                 c.Recall.MaxSeedAids,
		MaxSeedTags:                 c.Recall.MaxSeedTags,
		MaxExcludeAids:              c.Recall.MaxExcludeAids,
		MaxBatchItems:               c.Recall.MaxBatchItems,
		MaxVersionList:              c.Recall.MaxVersionList,
		MaxPoolSnapshotPage:         c.Recall.MaxPoolSnapshotPage,
		MaxRequestLogPage:           c.Recall.MaxRequestLogPage,
		MinKeepVersions:             c.Recall.MinKeepVersions,
		PoolStaleSeconds:            c.Recall.PoolStaleSeconds,
		IdempotencyLeaseSeconds:     c.Recall.IdempotencyLeaseSeconds,
		IdempotencyRetentionSeconds: c.Recall.IdempotencyRetentionSeconds,
	})
	if err != nil {
		logx.Errorf("recommend-recall: build repository: %v", err)
		panic(err)
	}

	ctx := &ServiceContext{Config: c, Repository: repo}
	ctx.logStartup()
	// go-zero 在 SIGTERM/SIGINT 时先触发 wrap-up 监听器，再调用 gRPC 的 shutdown 监听器，
	// 因此这里的收尾监听器能给发布循环留出把在途批次处理完的时间。
	proc.AddWrapUpListener(func() { ctx.Stop() })
	logx.Must(ctx.startPublisher())
	return ctx
}

// startPublisher 三种结果都有明确日志：
//  1. Kafka.Enabled=false：不建写入通道，recall_outbox 只累积；
//  2. Enabled=true 且二进制链接了运行时（-tags recommendrecall_kafka）且参数完整：启动发布循环；
//  3. Enabled=true 但运行时未链接、或 Kafka.* 参数不完整：返回错误，进程启动即失败。
func (s *ServiceContext) startPublisher() error {
	for _, note := range publisher.RuntimeNotes(s.Config.Kafka) {
		logx.Infof("recommendrecall/svc: %s", note)
	}
	if !s.Config.Kafka.Enabled {
		return nil
	}
	sender, err := publisher.NewSender(publisher.SenderSettingsFrom(s.Config.Kafka))
	if err != nil {
		return err
	}
	pub, err := publisher.NewPublisher(s.Config, s.Repository.Outbox, sender)
	if err != nil {
		// 写入通道已建立但参数不合格：必须关掉，否则留下一条没关的连接。
		_ = sender.Close()
		return err
	}
	if err := pub.Start(); err != nil {
		_ = sender.Close()
		return err
	}
	s.Publisher = pub
	logx.Infof("recommendrecall/svc: 事件发布器已启动 topics=%v max_attempts=%d batch=%d",
		publisher.RequiredTopics(), s.Config.Kafka.MaxRetries, s.Config.Kafka.BatchLimit)
	return nil
}

// Stop 停止发布循环（单测与集成方可显式调用；SIGTERM 由 wrap-up 监听器调用）。
//
// 置 nil 是刻意的：Publisher.Stop() 会关掉 Sender 的写入通道，
// 留着这个字段只会让下一次 Stop() 去关已经关闭的通道。
func (s *ServiceContext) Stop() {
	if s.Publisher == nil {
		return
	}
	s.Publisher.Stop()
	s.Publisher = nil
}

// logStartup 打印启动期自检信息，便于验证配置是否按预期生效（AGENTS.md §4 可验证性）。
func (s *ServiceContext) logStartup() {
	logx.Infof("recommend-recall: enabled_sources=%v default_sources=%v fallback=%d cold_start=%d "+
		"max_candidates=%d budget_ms=%d degrade_enabled=%t pool_stale_seconds=%d",
		s.Config.Recall.EnabledSources, s.Config.Recall.DefaultSources,
		s.Config.Recall.FallbackSource, s.Config.Recall.ColdStartSource,
		s.Config.Recall.MaxCandidates, s.Config.Recall.BudgetMillis,
		s.Config.Recall.DegradeEnabled, s.Config.Recall.PoolStaleSeconds)
	// 下游未接线是"能用但少两路"的状态，必须在日志里显式声明，
	// 避免运维以为协同/向量路已经可用。
	logx.Error("recommend-recall: FeatureSource/VisibilitySource are stubs; sources 4(COLLAB)/5(VECTOR) " +
		"will degrade with feature_unavailable until spm & feature-store rpc are wired (see README 待接线接口)")
}
