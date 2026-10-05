// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"context"
	"errors"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/proc"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/core/threading"

	"go-video/services/inbox/internal/config"
	"go-video/services/inbox/internal/consumer"
	"go-video/services/inbox/internal/repository"
)

// ServiceContext 是 inbox 的运行时上下文。
//
// 装配原则（AGENTS.md §4、§5）：
//   - 入口文件 inbox.v1.go 由 goctl 生成、不可手改，所以 MQ 消费者与后台清扫器
//     的生命周期统一放在这里管理，go-zero 收到 SIGTERM/SIGINT 时先跑 wrap-up 监听器
//     再关 gRPC，给在途事件留出收尾时间；
//   - 构造函数不伪造成功：Kafka.Enabled=true 而运行时未链接、或消费配置不完整，
//     一律返回错误并让进程启动即失败（见 startConsumer 的三种分支）；
//   - Redis 只是未读计数的加速副本，真值在 MySQL。
type ServiceContext struct {
	Config config.Config

	// Repository 本服务的数据访问入口：MySQL 五张自有表 + Redis 未读加速层。
	Repository *repository.Repository
	// Consumer 事件消费编排器（每个 topic 一个消费者 + 退避清扫器）。
	// Kafka.Enabled=false 时为 nil，此时本进程不消费事件。
	Consumer *consumer.Supervisor

	// processor 与 Consumer 内部同源，用于未启动消费者时单独推进 retry 行。
	processor *consumer.Processor

	workerCtx context.Context
	cancel    context.CancelFunc
}

// NewServiceContext 构造上下文并按配置启动后台链路。
func NewServiceContext(c config.Config) *ServiceContext {
	rds := redis.MustNewRedis(c.CacheRedis)
	repo := repository.New(rds, sqlx.NewMysql(c.DataSource), c)

	workerCtx, cancel := context.WithCancel(context.Background())
	svcCtx := &ServiceContext{
		Config:     c,
		Repository: repo,
		processor:  consumer.NewProcessor(repo, consumer.OptionsFrom(c.Kafka)),
		workerCtx:  workerCtx,
		cancel:     cancel,
	}

	// 任何消费侧的装配错误都在这里终止进程：配置写错却「安静地不消费」是站内信丢失的最坏形态。
	logx.Must(svcCtx.startConsumer())
	proc.AddWrapUpListener(func() { svcCtx.stopWorkers() })

	return svcCtx
}

// startConsumer 按配置决定消费侧的启动方式，三种结果都有明确日志：
//  1. Kafka.Enabled=false：不连 Kafka，只启动退避重投清扫器（它只需要 MySQL）；
//  2. Enabled=true 且二进制链接了 Kafka 运行时（-tags inbox_kafka）：启动 Supervisor，
//     清扫器由 Supervisor 自带；
//  3. Enabled=true 但运行时未链接、或 Kafka.* 配置不完整：返回错误，调用方终止启动。
//
// 理由与运维口径见 services/inbox/README.md「消费者启动方式」。
func (s *ServiceContext) startConsumer() error {
	k := s.Config.Kafka
	for _, note := range consumer.RuntimeNotes(k) {
		logx.Infof("inbox/svc: %s", note)
	}
	if !k.Enabled {
		s.startRetrySweeper()
		return nil
	}

	factory, err := consumer.NewKqFactory()
	if err != nil {
		return err
	}
	sup, err := consumer.NewSupervisor(s.Config, s.Repository, factory)
	if err != nil {
		return err
	}
	if err := sup.Start(s.workerCtx); err != nil {
		return err
	}
	s.Consumer = sup
	logx.Infof("inbox/svc: 事件消费者已启动 topics=%v group=%s", sup.Topics(), k.Group)
	return nil
}

// startRetrySweeper 在没有消费者时单独推进 inbox_consumer_offset 里到期的 retry 事件。
// 这些行是上一轮（可能消费者已关闭）留下的，不依赖 MQ 也必须继续向前收敛。
func (s *ServiceContext) startRetrySweeper() {
	if !s.Config.Kafka.RetrySweeperEnabled {
		logx.Errorf("inbox/svc: Kafka.RetrySweeperEnabled=false，inbox_consumer_offset 中 retry 状态的事件不会被重投")
		return
	}
	threading.GoSafeCtx(s.workerCtx, func() {
		if err := s.processor.RunRetrySweeper(s.workerCtx); err != nil && !errors.Is(err, context.Canceled) {
			logx.WithContext(s.workerCtx).Errorf("inbox/svc: 重试清扫器退出 err=%v", err)
		}
	})
}

// stopWorkers 停止消费者与清扫器（幂等）。
func (s *ServiceContext) stopWorkers() {
	if s.Consumer != nil {
		s.Consumer.Stop()
		s.Consumer = nil
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

// Processor 暴露事件处理器，供单测与运维重放入口复用（不触发任何网络）。
func (s *ServiceContext) Processor() *consumer.Processor { return s.processor }

// WorkerCtx 返回后台链路的根上下文。
func (s *ServiceContext) WorkerCtx() context.Context { return s.workerCtx }

// Stop 手动收尾入口（正常退出由 go-zero 的 wrap-up 监听器调用）。
func (s *ServiceContext) Stop() { s.stopWorkers() }
