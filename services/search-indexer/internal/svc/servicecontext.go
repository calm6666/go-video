// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"context"
	"errors"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/proc"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/core/threading"

	"go-video/services/search-indexer/internal/config"
	"go-video/services/search-indexer/internal/consumer"
	"go-video/services/search-indexer/internal/esclient"
	"go-video/services/search-indexer/internal/repository"
)

// ServiceContext 是 search-indexer 的运行时上下文。
//
// 装配原则（AGENTS.md §9）：
//   - 外部依赖（OpenSearch / MySQL / Redis）全部以接口或懒连接方式注入，
//     构造过程不发起任何网络握手，因此仓库可在离线环境下编译与单测；
//   - 后台 worker（重试清扫、重建执行）由 context 控制生命周期：进程收到退出信号时
//     先取消 worker（重建任务会把续跑游标与 failed 终态落库），再让 gRPC 优雅退出；
//   - 依赖不可用时返回明确错误，绝不返回「看起来成功」的空结果。
type ServiceContext struct {
	Config config.Config

	// ES 是 OpenSearch 客户端（接口注入，logic 层排障时可直接读索引）。
	ES esclient.Client
	// Repository 本服务的数据访问入口：MySQL 四张自有表 + OpenSearch 投影。
	Repository *repository.Repository
	// Consumer 事件消费器的状态机（去重 / 退避 / 死信），被三条入口共用：
	// Kafka 消费者（Supervisor，仅在链接运行时且 Kafka.Enabled=true 时存在）、
	// 本进程的重试清扫器，以及 services/cron 与 RPC 投影写路径。
	Consumer *consumer.Consumer
	// Supervisor Kafka 消费者编排器（每个 topic 一个消费者）。
	// Kafka.Enabled=false 时为 nil，此时本进程不消费事件。
	// 非 nil 只说明二进制链接了 kq 且队列启动成功，不说明已与 broker 联调过。
	Supervisor *consumer.Supervisor
	// Rebuild 索引重建执行器，在本进程内抢占并执行 search_index_task。
	Rebuild *consumer.RebuildRunner

	// workerCtx 后台 worker 的根上下文。
	workerCtx context.Context
	cancel    context.CancelFunc
}

// NewServiceContext 构造上下文并启动本服务负责的后台 worker。
func NewServiceContext(c config.Config) *ServiceContext {
	es, err := esclient.New(esclient.Options{
		Endpoints:            c.OpenSearch.Endpoints,
		Username:             c.OpenSearch.Username,
		Password:             c.OpenSearch.Password,
		Timeout:              time.Duration(c.OpenSearch.TimeoutMs) * time.Millisecond,
		MaxRetries:           c.OpenSearch.MaxRetries,
		BulkActions:          c.OpenSearch.BulkActions,
		AllowAnonymousWrites: c.OpenSearch.AllowAnonymousWrites,
		Analyzer:             esclient.Analyzer{Kind: esclient.AnalyzerKind(c.OpenSearch.Analyzer.Kind), StopwordsPath: c.OpenSearch.Analyzer.StopwordsPath, SynonymsPath: c.OpenSearch.Analyzer.SynonymsPath},
	})
	// 端点为空时本服务没有任何投影能力，直接快速失败而不是静默降级。
	logx.Must(err)

	var rds *redis.Redis
	if c.CacheRedis.Host != "" {
		rds = redis.MustNewRedis(c.CacheRedis)
	} else {
		// 不 panic：Redis 只承担缓存与别名切换互斥，缺失时退化为单实例语义，
		// 幂等与并发安全仍由 MySQL 唯一索引和条件更新保证。
		logx.Errorf("search-indexer: CacheRedis 未配置，写入索引缓存与别名切换互斥退化为单实例语义")
	}

	repo, err := repository.New(rds, sqlx.NewMysql(c.DataSource), es, repoOptions(c.OpenSearch))
	logx.Must(err)

	workerCtx, cancel := context.WithCancel(context.Background())
	svcCtx := &ServiceContext{
		Config:     c,
		ES:         es,
		Repository: repo,
		Consumer:   consumer.New(repo, consumerOptions(c.Kafka)),
		Rebuild:    consumer.NewRebuildRunner(repo, rebuildOptions(c)),
		workerCtx:  workerCtx,
		cancel:     cancel,
	}

	// go-zero 在 SIGTERM/SIGINT 时先触发 wrap-up 监听器，再调用 gRPC 的 shutdown 监听器，
	// 因此这里取消上下文可以给 worker 留出优雅收尾的时间。
	proc.AddWrapUpListener(func() { svcCtx.stopWorkers() })
	// 装配错误必须终止启动：Kafka.Enabled=true 却没链接运行时、或消费参数不完整时，
	// 「安静地不消费」等于搜索投影静默落后，没人从日志里发现。
	logx.Must(svcCtx.startWorkers())

	return svcCtx
}

// startWorkers 按配置启动后台循环，并在最后决定消费侧的启动方式。
// 入口文件由 goctl 生成、不可手改（AGENTS.md §4），因此 worker 的生命周期在 ServiceContext 内统一管理。
func (s *ServiceContext) startWorkers() error {
	if s.Config.Kafka.RetrySweeperEnabled {
		threading.GoSafeCtx(s.workerCtx, func() {
			if err := s.Consumer.RunRetrySweeper(s.workerCtx); err != nil && !errors.Is(err, context.Canceled) {
				logx.WithContext(s.workerCtx).Errorf("search-indexer: 重试清扫器退出 err=%v", err)
			}
		})
	}
	if s.Config.Kafka.RebuildRunnerEnabled {
		threading.GoSafeCtx(s.workerCtx, func() {
			if err := s.Rebuild.Run(s.workerCtx); err != nil && !errors.Is(err, context.Canceled) {
				logx.WithContext(s.workerCtx).Errorf("search-indexer: 重建执行器退出 err=%v", err)
			}
		})
	}
	return s.startConsumer()
}

// startConsumer 三种结果都有明确日志：
//  1. Kafka.Enabled=false：不连 Kafka，只跑清扫器与重建执行器（两者都只需要 MySQL + OpenSearch）；
//  2. Enabled=true 且二进制链接了运行时（-tags searchindexer_kafka）且消费参数完整：启动 Supervisor；
//  3. Enabled=true 但运行时未链接、或 Kafka.* 参数不完整：返回错误，进程启动即失败。
//
// 理由与运维口径见 services/search-indexer/README.md「启动方式（worker 与消费者在哪个进程）」
// 与「已知缺口」1。
func (s *ServiceContext) startConsumer() error {
	k := s.Config.Kafka
	for _, note := range consumer.RuntimeNotes(k) {
		logx.WithContext(s.workerCtx).Infof("search-indexer/svc: %s", note)
	}
	if !k.Enabled {
		return nil
	}
	factory, err := consumer.NewKqFactory()
	if err != nil {
		return err
	}
	sup, err := consumer.NewSupervisor(s.Config, s.Consumer, factory)
	if err != nil {
		return err
	}
	if err := sup.Start(); err != nil {
		return err
	}
	s.Supervisor = sup
	logx.WithContext(s.workerCtx).Infof("search-indexer/svc: 事件消费者已启动 topics=%v group=%s",
		sup.Topics(), k.Group)
	return nil
}

// stopWorkers 停止后台 worker（单测与集成方也可显式调用）：先停在途消费的收尾，再取消上下文。
func (s *ServiceContext) stopWorkers() {
	if s.Supervisor != nil {
		s.Supervisor.Stop()
		s.Supervisor = nil
	}
	if s.cancel != nil {
		s.cancel()
	}
}

// Stop 实现 go-zero service.Service 之外的手动收尾入口。
func (s *ServiceContext) Stop() { s.stopWorkers() }

// WorkerCtx 返回后台 worker 的上下文（供适配器把消费循环挂到同一生命周期上）。
func (s *ServiceContext) WorkerCtx() context.Context { return s.workerCtx }

// repoOptions 配置 → repository 投影参数。
func repoOptions(c config.OpenSearchConf) repository.Options {
	return repository.Options{
		IndexPrefix:          c.IndexPrefix,
		SchemaVersion:        c.SchemaVersion,
		SliceSpan:            c.ReindexSliceSpan,
		SliceSize:            c.ReindexSliceSize,
		StopAfterEmptySlices: c.StopAfterEmptySlices,
		RetryOnConflict:      c.RetryOnConflict,
	}
}

// consumerOptions 配置 → 消费/退避参数。
func consumerOptions(c config.KafkaConf) consumer.Options {
	return consumer.Options{
		MaxRetries:        c.MaxRetries,
		InProcessAttempts: c.InProcessAttempts,
		BaseBackoff:       time.Duration(c.RetryBackoffSec) * time.Second,
		MaxBackoff:        time.Duration(c.MaxRetryBackoffSec) * time.Second,
		RetryBatchLimit:   c.RetryBatchLimit,
		SweepIdleWait:     time.Duration(c.RetrySweepIntervalSec) * time.Second,
	}
}

// rebuildOptions 配置 → 重建执行参数（切片参数取自 OpenSearch 段，与投影写入同源）。
func rebuildOptions(c config.Config) consumer.RebuildOptions {
	return consumer.RebuildOptions{
		SliceSpan:            c.OpenSearch.ReindexSliceSpan,
		SliceSize:            c.OpenSearch.ReindexSliceSize,
		StopAfterEmptySlices: c.OpenSearch.StopAfterEmptySlices,
		PollInterval:         time.Duration(c.Kafka.RebuildPollIntervalSec) * time.Second,
	}
}
