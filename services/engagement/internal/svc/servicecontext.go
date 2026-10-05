// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"context"

	"go-video/services/engagement/internal/config"
	"go-video/services/engagement/internal/publisher"
	"go-video/services/engagement/internal/repository"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/proc"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ServiceContext 是 engagement 服务的运行时上下文。
type ServiceContext struct {
	Config     config.Config
	Repository *repository.Repository
	// Publisher engagement_outbox → engagement.action.v1 的事件发布器。
	// Kafka.Enabled=false 时为 nil，此时本进程不投递事件，点赞/收藏/分享的事件只在表里累积。
	// 非 nil 只说明二进制链接了 kq、发送通道建立成功、循环已在跑：
	// 本仓库从未与真实 broker 联调，「事件已送达」不在这个结论范围内。
	Publisher *publisher.Publisher

	// workerCtx 后台 worker 的根上下文。
	workerCtx context.Context
	cancel    context.CancelFunc
}

// NewServiceContext 构造 ServiceContext，并按配置启动 Outbox 发布循环。
//
// 装配策略与 video/playback/upload/live-media 一致：
//   - MySQL 与 CacheRedis 是硬依赖（互动关系、计数、Outbox 都在库里），连不上就启动失败；
//   - Kafka 参数不完整，或 Enabled=true 但运行时没链接（默认构建）时启动即失败：
//     「engagement.action.v1 安静地不出去」等于索引里的互动计数永远是 0，
//     而下游 search-indexer 与 inbox 的消费者实现都已经在等这个 topic。
func NewServiceContext(c config.Config) *ServiceContext {
	rds := redis.MustNewRedis(c.CacheRedis)
	conn := sqlx.NewMysql(c.DataSource)

	svcCtx := &ServiceContext{
		Config:     c,
		Repository: repository.New(rds, conn),
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	svcCtx.workerCtx = workerCtx
	svcCtx.cancel = cancel

	// go-zero 在 SIGTERM/SIGINT 时先触发 wrap-up 监听器，再调用 gRPC 的 shutdown 监听器，
	// 因此这里取消上下文能给发布循环留出收尾时间（在途批次处理完才关连接）。
	proc.AddWrapUpListener(func() { svcCtx.stopWorkers() })
	logx.Must(svcCtx.startPublisher())

	return svcCtx
}

// startPublisher 三种结果都有明确日志：
//  1. Kafka.Enabled=false：不建发送通道，engagement_outbox 只累积；
//  2. Enabled=true 且二进制链接了运行时（-tags engagement_kafka）且参数完整：启动发布循环；
//  3. Enabled=true 但运行时未链接、或 Kafka.* 参数不完整：返回错误，进程启动即失败。
func (s *ServiceContext) startPublisher() error {
	for _, note := range publisher.RuntimeNotes(s.Config.Kafka) {
		logx.WithContext(s.workerCtx).Infof("engagement/svc: %s", note)
	}
	if !s.Config.Kafka.Enabled {
		return nil
	}
	sender, err := publisher.NewSender(publisher.SenderSettingsFrom(s.Config.Kafka))
	if err != nil {
		return err
	}
	pub, err := publisher.NewPublisher(s.Config, s.Repository.OutboxModel(), sender)
	if err != nil {
		// 发送通道已建立但参数不合格：必须关掉，否则留下一条没关的连接。
		_ = sender.Close()
		return err
	}
	if err := pub.Start(); err != nil {
		_ = sender.Close()
		return err
	}
	s.Publisher = pub
	logx.WithContext(s.workerCtx).Infof("engagement/svc: 事件发布器已启动 topic=%s brokers=%v max_attempts=%d",
		publisher.RequiredTopic(), s.Config.Kafka.Brokers, s.Config.Kafka.MaxRetries)
	return nil
}

// stopWorkers 停止后台 worker（单测与集成方也可显式调用）。
func (s *ServiceContext) stopWorkers() {
	if s.Publisher != nil {
		s.Publisher.Stop()
		s.Publisher = nil
	}
	if s.cancel != nil {
		s.cancel()
	}
}

// Stop 手动收尾入口（SIGTERM 由 wrap-up 监听器自动调用）。
func (s *ServiceContext) Stop() { s.stopWorkers() }

// WorkerCtx 返回后台 worker 的上下文。
func (s *ServiceContext) WorkerCtx() context.Context { return s.workerCtx }
