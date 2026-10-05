// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"context"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/proc"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/live-ingest/internal/config"
	"go-video/services/live-ingest/internal/publisher"
	"go-video/services/live-ingest/internal/repository"
)

// ServiceContext 是 live-ingest 的运行时上下文，承载跨请求共享的依赖。
// logic 通过它访问本服务的表、缓存和外部媒体入口适配器。
type ServiceContext struct {
	Config config.Config

	// Repository 是本服务数据访问入口（go_video_live_ingest 库 + CacheRedis）。
	Repository *repository.Repository

	// Gateway 是外部 CDN/媒体入口适配器。本轮是显式 stub：
	// 所有调用返回 repository.ErrCdnNotConfigured，不伪造「已踢流/已探测」。
	Gateway repository.IngestGateway

	// Publisher live_ingest_outbox → live.state.v1 的事件发布器。
	// Kafka.Enabled=false 时为 nil，此时本进程不投递事件。
	// 非 nil 只说明二进制链接了 kq、发送通道建立成功、循环已在跑：
	// 本仓库从未与真实 broker 联调，「事件已送达」不在这个结论范围内。
	Publisher *publisher.Publisher

	// workerCtx 后台 worker 的根上下文。
	workerCtx context.Context
	cancel    context.CancelFunc
}

// NewServiceContext 构造 ServiceContext，并按配置启动本服务的后台 worker。
//
// 装配策略：
//   - MySQL 与 CacheRedis 是本服务的硬依赖（状态机与幂等都落在库里），沿用
//     MustNewRedis / NewMysql：连不上就启动失败，不带着半残依赖对外服务；
//   - 队列发送端不在这里无条件构造：Kafka.Enabled=true 但二进制没链接 kq、
//     或发布参数不完整时，logx.Must 直接终止启动。「事件安静地不出去」
//     会让 live-room 的房间投影与 inbox 给主播的断流/停播通知永久落后且无人发现
//     （inbox 对 IDLE/PUBLISHING 不发信，见其 consumer/liveActionFromStreamState）；
//   - 下游 RPC 客户端本轮不构造：live-room 的 goctl client 还在并行开发中，
//     这里 import 它会让本服务无法独立编译，配置位 LiveRoomRPC 只保留在 Config 中，
//     接线步骤写在 services/live-ingest/README.md「已知缺口」。
func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	rds := redis.MustNewRedis(c.CacheRedis)

	if !c.Cdn.Enabled {
		logx.Infof("live-ingest/svc: CDN/媒体入口未启用，外部调用走显式 stub（ErrCdnNotConfigured）")
	}
	if c.LiveIngest.SweeperEnabled {
		logx.Errorf("live-ingest/svc: LiveIngest.SweeperEnabled=true 但状态扫描器仍未接线，" +
			"心跳超时/宽限期耗尽不会自动停流（见 README「已知缺口」）")
	}

	svcCtx := &ServiceContext{
		Config:     c,
		Repository: repository.New(conn, rds, c),
		Gateway:    repository.NewIngestGateway(c.Cdn),
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
//  1. Kafka.Enabled=false：不建发送通道，outbox 只累积（GetEventPublishCheckpoint 能看到滞后）；
//  2. Enabled=true 且二进制链接了运行时（-tags liveingest_kafka）且参数完整：启动发布循环；
//  3. Enabled=true 但运行时未链接、或 Kafka.* 参数不完整：返回错误，进程启动即失败。
func (s *ServiceContext) startPublisher() error {
	for _, note := range publisher.RuntimeNotes(s.Config.Kafka) {
		logx.WithContext(s.workerCtx).Infof("live-ingest/svc: %s", note)
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
		// 发送通道已经建立但参数不合格：必须关掉，否则退出时留下一条没关的连接。
		_ = sender.Close()
		return err
	}
	if err := pub.Start(); err != nil {
		_ = sender.Close()
		return err
	}
	s.Publisher = pub
	logx.WithContext(s.workerCtx).Infof("live-ingest/svc: 事件发布器已启动 topic=%s brokers=%v max_attempts=%d",
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

// Stop 实现 go-zero service.Service 之外的手动收尾入口。
func (s *ServiceContext) Stop() { s.stopWorkers() }

// WorkerCtx 返回后台 worker 的上下文（供适配器把发布循环挂到同一生命周期上）。
func (s *ServiceContext) WorkerCtx() context.Context { return s.workerCtx }
