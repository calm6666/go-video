// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"context"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/proc"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/playback/internal/config"
	"go-video/services/playback/internal/publisher"
	"go-video/services/playback/internal/repository"
	"go-video/services/playback/internal/signurl"
)

// ServiceContext 是 playback 服务的运行时上下文，承载跨请求共享的依赖。
type ServiceContext struct {
	Config     config.Config
	Repository *repository.Repository
	// Rights 版权窗口校验器。未配置 RightsRPC 时注入显式失败实现，
	// PGC 签发会返回 playback: rights service unavailable，而不是伪造成功。
	Rights repository.RightsChecker
	// Signer CDN A 型防盗链签名器。
	Signer *signurl.Signer
	// Publisher playback_outbox → playback.heartbeat.v1 的事件发布器。
	// Kafka.Enabled=false 时为 nil，此时本进程不投递事件，心跳事件只在表里累积。
	// 非 nil 只说明二进制链接了 kq、发送通道建立成功、循环已在跑：
	// 本仓库从未与真实 broker 联调，「事件已送达」不在这个结论范围内。
	Publisher *publisher.Publisher

	// workerCtx 后台 worker 的根上下文。
	workerCtx context.Context
	cancel    context.CancelFunc
}

// NewServiceContext 构造 ServiceContext，并按配置启动 Outbox 发布循环。
//
// 装配策略与 live-ingest 一致：
//   - MySQL 与 CacheRedis 是硬依赖（播放会话、进度、幂等都在库里），连不上就启动失败；
//   - 配置错误（生产模式关签名、Kafka 参数不完整）直接 panic/logx.Must 终止启动，
//     不带着半残依赖对外服务（AGENTS.md §6）；
//   - Kafka.Enabled=true 但二进制没链接 kq（默认构建）时同样启动失败：
//     「事件安静地不出去」会让 SPM 的完播率与热度特征永久落后且无人发现。
func NewServiceContext(c config.Config) *ServiceContext {
	rds := redis.MustNewRedis(c.CacheRedis)
	conn := sqlx.NewMysql(c.DataSource)

	var rights repository.RightsChecker = repository.NewUnavailableRights()
	if len(c.RightsRPC.Etcd.Hosts) > 0 || c.RightsRPC.Target != "" {
		rights = repository.NewRightsClient(c.RightsRPC)
	}

	// 仅 dev/test 模式允许关闭 auth_key（本地直连 MinIO 冒烟）；其它模式必须签名。
	allowUnsigned := c.Mode == service.DevMode || c.Mode == service.TestMode
	signer, err := signurl.New(signurl.Config{
		BaseURL:       c.Sign.BaseURL,
		PrivateKey:    c.Sign.PrivateKey,
		KeyID:         c.Sign.KeyID,
		TokenTTL:      c.Sign.TokenTTL,
		EnableAuthKey: c.Sign.EnableAuthKey,
		AllowUnsigned: allowUnsigned,
	})
	logx.Must(err)

	svcCtx := &ServiceContext{
		Config:     c,
		Repository: repository.New(rds, conn),
		Rights:     rights,
		Signer:     signer,
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
//  1. Kafka.Enabled=false：不建发送通道，playback_outbox 只累积；
//  2. Enabled=true 且二进制链接了运行时（-tags playback_kafka）且参数完整：启动发布循环；
//  3. Enabled=true 但运行时未链接、或 Kafka.* 参数不完整：返回错误，进程启动即失败。
func (s *ServiceContext) startPublisher() error {
	for _, note := range publisher.RuntimeNotes(s.Config.Kafka) {
		logx.WithContext(s.workerCtx).Infof("playback/svc: %s", note)
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
	logx.WithContext(s.workerCtx).Infof("playback/svc: 事件发布器已启动 topic=%s brokers=%v max_attempts=%d",
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
