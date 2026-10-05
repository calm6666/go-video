// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/proc"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/notification/internal/config"
	"go-video/services/notification/internal/consumer"
	"go-video/services/notification/internal/provider"
	"go-video/services/notification/internal/repository"
	"go-video/services/notification/internal/send"
)

// ServiceContext 是 notification 的运行时上下文与装配根。
//
// 装配原则（AGENTS.md §4、§5）：
//   - 入口文件 notification.v1.go 由 goctl 生成、不可手改，所以投递调度器与事件消费者的
//     生命周期统一在这里管理：go-zero 收到 SIGINT/SIGTERM 时先跑 wrap-up 监听器，
//     等在途投递落库再退出；
//   - 任何“配置要求跑但跑不起来”的情况都以错误终止进程，绝不静默降级成“看起来在服务、
//     实际一条都不发”（见 startWorkers 的三个分支与 services/notification/README.md）；
//   - Redis 只承担每日配额计数：不可用时 repository 返回 ErrQuotaUnavailable，
//     投递入口 fail-closed 拒绝，不会绕过频控把提醒打爆用户。
type ServiceContext struct {
	Config config.Config

	// Repository 本服务的数据访问入口（MySQL 五张自有表 + Redis 配额计数）。
	Repository *repository.Repository
	// Providers 已启用的通道适配器注册表；未启用的通道不在其中，投递时显式报错。
	Providers *provider.Registry
	// Enqueuer 投递用例（校验 + 模板解析 + 配额/偏好拦截 + biz_key 幂等落库），
	// gRPC 入口与事件入口共用。
	Enqueuer *send.Enqueuer
	// Dispatcher notification_delivery 的退避扫描与投递执行器；
	// Notification.DispatcherEnabled=false 时为 nil。
	Dispatcher *consumer.Dispatcher
	// Events notification.request.v1 的消费处理器；始终存在（RetryDeadLetter 需要它重放事件），
	// 但只有 Kafka.Enabled=true 时才会收到队列消息。
	Events *consumer.EventHandler

	kafka consumer.KafkaRuntime

	mu        sync.Mutex
	started   bool
	stopOnce  sync.Once
	workerCtx context.Context
	cancel    context.CancelFunc
}

// NewServiceContext 构造上下文并按配置启动后台链路。
// 装配失败直接 logx.Must 终止进程：这是启动期错误，不该留到第一个用户请求才暴露。
func NewServiceContext(c config.Config) *ServiceContext {
	// CacheRedis 是启动期硬依赖：连不上就立刻失败，而不是等到第一个请求才发现配额校验不可用。
	repo := repository.New(sqlx.NewMysql(c.DataSource), redis.MustNewRedis(c.CacheRedis))

	providers, notes, err := buildProviders(c.Providers)
	logx.Must(err)
	for _, n := range notes {
		logx.Errorf("notification/svc: %s", n)
	}

	enqueuer, err := send.New(repo, send.OptionsFrom(c.Notification))
	logx.Must(err)

	workerCtx, cancel := context.WithCancel(context.Background())
	s := &ServiceContext{
		Config:     c,
		Repository: repo,
		Providers:  providers,
		Enqueuer:   enqueuer,
		workerCtx:  workerCtx,
		cancel:     cancel,
	}
	logx.Must(s.startWorkers())
	proc.AddWrapUpListener(func() { s.Stop() })
	return s
}

// startWorkers 按配置决定后台链路的启动方式，三种结果都有明确日志：
//
//  1. Notification.DispatcherEnabled=true：启动投递调度器（只依赖 MySQL，不依赖 MQ）；
//     为 false 时任务会一直停在 pending，这里用 Error 级日志点明，并拒绝
//     “SyncSend=true 且 DispatcherEnabled=false” 这种永远不会发送的组合。
//  2. Kafka.Enabled=true：启动事件退避循环 + kq 消费者；二进制未带
//     `-tags notification_kafka` 构建时 StartKafkaRuntime 返回 ErrKafkaRuntimeNotBuilt，
//     进程随即退出——静默不消费等于丢通知。
//  3. Kafka.Enabled=false：只装配 EventHandler 对象（供 RetryDeadLetter 重放），
//     不连 Kafka，并在启动日志里显式声明本进程不消费。
func (s *ServiceContext) startWorkers() error {
	c := s.Config
	if c.Notification.DispatcherEnabled {
		d, err := consumer.NewDispatcher(s.Repository, s.Providers, provider.NewPassthroughResolver(),
			consumer.NewDispatchPolicy(c.Notification))
		if err != nil {
			return fmt.Errorf("notification/svc: 装配投递调度器失败: %w", err)
		}
		if err := d.Start(); err != nil {
			return fmt.Errorf("notification/svc: 启动投递调度器失败: %w", err)
		}
		s.Dispatcher = d
		logx.Infow("notification/svc: 投递调度器已启动",
			logx.Field("channels", fmt.Sprint(s.Providers.Channels())),
			logx.Field("max_retries", c.Notification.MaxDeliveryRetries))
	} else {
		if c.Notification.SyncSend {
			return errors.New("notification/svc: Notification.SyncSend=true 需要 DispatcherEnabled=true，否则没有任何通道会被投递")
		}
		logx.Errorf("notification/svc: Notification.DispatcherEnabled=false —— notification_delivery 中的任务不会被投递，" +
			"任务会一直停在 pending（确认这是有意的，例如本实例只做模板管理）")
	}

	h, err := consumer.NewEventHandler(s.Repository, s.Enqueuer, consumer.NewEventPolicy(c.Kafka, c.Notification))
	if err != nil {
		return fmt.Errorf("notification/svc: 装配事件处理器失败: %w", err)
	}
	s.Events = h

	if !c.Kafka.Enabled {
		logx.Errorf("notification/svc: Kafka.Enabled=false —— 本进程不消费 %s，也不重投 notification_consumer_offset 里 "+
			"retry 状态的事件；外部通道投递仍可通过 SendNotification RPC 发起", c.Kafka.RequestTopic)
		return nil
	}
	if len(c.Kafka.Brokers) == 0 {
		return errors.New("notification/svc: Kafka.Enabled=true 但 Kafka.Brokers 为空")
	}
	if c.Kafka.RequestTopic == "" {
		return errors.New("notification/svc: Kafka.Enabled=true 但 Kafka.RequestTopic 为空")
	}
	runtime, err := consumer.StartKafkaRuntime(c, h)
	if err != nil {
		return fmt.Errorf("notification/svc: 启动 Kafka 消费者失败: %w", err)
	}
	s.kafka = runtime
	if err := h.Start(); err != nil {
		return fmt.Errorf("notification/svc: 启动事件退避循环失败: %w", err)
	}
	logx.Infof("notification/svc: Kafka 消费者已启动 topic=%s group=%s brokers=%v",
		c.Kafka.RequestTopic, c.Kafka.Group, c.Kafka.Brokers)
	return nil
}

// Stop 停止后台链路（幂等，由 go-zero wrap-up 监听器在进程退出前调用）。
func (s *ServiceContext) Stop() {
	s.stopOnce.Do(func() {
		if s.Events != nil {
			s.Events.Stop()
		}
		if s.Dispatcher != nil {
			s.Dispatcher.Stop()
		}
		if s.kafka != nil {
			s.kafka.Stop()
		}
		s.mu.Lock()
		if s.cancel != nil {
			s.cancel()
			s.cancel = nil
		}
		s.mu.Unlock()
		logx.Info("notification/svc: 后台链路已停止")
	})
}

// WorkerCtx 返回后台链路的根上下文（单测与运维重放入口可用）。
func (s *ServiceContext) WorkerCtx() context.Context { return s.workerCtx }
