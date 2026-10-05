// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/proc"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"

	"go-video/services/live-media/internal/config"
	"go-video/services/live-media/internal/publisher"
	"go-video/services/live-media/model"

	assetrpc "go-video/services/asset/rpc"
	moderationrpc "go-video/services/moderation-orchestrator/rpc"
	videorpc "go-video/services/video/rpc"
)

// ServiceContext 是 live-media 的运行时上下文，承载跨请求共享的依赖。
//
// 装配原则（AGENTS.md §3、§5）：
//   - 只拥有 go_video_live_media 库的 8 张 live_* 表；媒资（asset）、稿件（video）、
//     审核结论（moderation-orchestrator）都只以主键引用形式存在，
//     绝不 import 其它服务的 internal/model，也不直连它们的库表或 Redis key；
//   - 跨域读只走下面的 zrpc client，且 client 全部可选：Target/Etcd.Hosts/Endpoints 皆空时
//     字段保持 nil，调用点必须显式报错（见 logic 层的 ErrDownstream*），不得「当作检查通过」；
//   - CacheRedis 只放任务详情与房间档位的易失副本，真值恒在 MySQL；
//     缓存缺失或 TTL=0 时一律回源，不影响正确性；
//   - MySQL 是硬依赖（状态机 CAS、幂等唯一键、Outbox 都要落库），连不上就启动失败；
//   - Kafka 未开启是允许的（事件只在 live_media_outbox 累积），但「开了却投不出去」不允许：
//     Enabled=true 而二进制没链接发送端、或发布参数不完整时，构造本上下文直接失败（AGENTS.md §6）。
type ServiceContext struct {
	Config config.Config

	// DB 本服务独占的 go_video_live_media 连接：写路径需要「业务行 + Outbox 行」同事务时用。
	DB sqlx.SqlConn

	// Cache 业务缓存；nil 表示未配置，调用方必须回落到 DB 而不是返回空结果。
	Cache *redis.Redis

	// --- 本服务自有表的数据访问 ---

	// TranscodeTasks live_transcode_task：直播转码任务状态机。
	TranscodeTasks model.LiveTranscodeTaskModel
	// StreamOutputs live_stream_output：分发档位（直播实时链路，与回放发布状态无关）。
	StreamOutputs model.LiveStreamOutputModel
	// RecordTasks live_record_task：录制任务与断点续录水位。
	RecordTasks model.LiveRecordTaskModel
	// Segments live_record_segment：切片明细（回放拼接的最小单位，含缺口行）。
	Segments model.LiveRecordSegmentModel
	// ReplayTasks live_replay_task：回放拼接任务。
	ReplayTasks model.LiveReplayTaskModel
	// ReplayRefs live_replay_asset_ref：回放产物与 asset/稿件的引用与只读投影。
	ReplayRefs model.LiveReplayAssetRefModel
	// RetentionTasks live_retention_task：回收意图与计数证据。
	RetentionTasks model.LiveRetentionTaskModel
	// Outbox live_media_outbox：与业务写同事务提交的事件台账。
	Outbox model.LiveMediaOutboxModel

	// --- 下游领域服务客户端（nil 表示本环境未接入）---

	// Asset 回放产物登记（RegisterAsset）；调用方是 Worker，不是本服务的 logic。
	Asset assetrpc.AssetClient
	// Video 回放建稿（CreateSubmission）与稿件状态读取（投影刷新）。
	Video videorpc.VideoClient
	// Moderation 回放送审（SubmitForReview）：回放走普通视频的审核链路。
	Moderation moderationrpc.ModerationOrchestratorClient

	// Publisher live_media_outbox → 9 个 livemedia.*.v1 topic 的事件发布器。
	// Kafka.Enabled=false 时为 nil，此时本进程不投递事件，只在表里累积。
	// 非 nil 只说明二进制链接了 kq、发送通道建立成功、循环已在跑：
	// 本仓库从未与真实 broker 联调，「事件已送达」不在这个结论范围内。
	Publisher *publisher.Publisher

	// workerCtx 后台 worker 的根上下文；cancel 由 stopWorkers 触发。
	workerCtx context.Context
	cancel    context.CancelFunc
}

// NewServiceContext 构造上下文。
//
// 这里刻意不做「下游连不上就退出」的强校验：领域服务必须能在灰度/降级环境里独立启动
// （etc 里三个 client 都带 NonBlock: true），缺依赖的后果推迟到调用点显式报错。
// 配置本身的完整性由 internal/config/config_load_test.go 在 CI 阶段拦住。
//
// 例外是事件发布器：Kafka.Enabled=true 表示运维已经假定「事件在投递」，
// 这时建不出发送端必须启动即失败，而不是带着一个投不出东西的对象对外服务
// （否则回放拼接、档位下线这些下游触发点永远不会发生，而且没有任何地方报错）。
func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	workerCtx, cancel := context.WithCancel(context.Background())
	ctx := &ServiceContext{
		Config:         c,
		DB:             conn,
		Cache:          newCache(c),
		TranscodeTasks: model.NewLiveTranscodeTaskModel(conn),
		StreamOutputs:  model.NewLiveStreamOutputModel(conn),
		RecordTasks:    model.NewLiveRecordTaskModel(conn),
		Segments:       model.NewLiveRecordSegmentModel(conn),
		ReplayTasks:    model.NewLiveReplayTaskModel(conn),
		ReplayRefs:     model.NewLiveReplayAssetRefModel(conn),
		RetentionTasks: model.NewLiveRetentionTaskModel(conn),
		Outbox:         model.NewLiveMediaOutboxModel(conn),
		Asset:          newAssetClient(c.AssetRPC),
		Video:          newVideoClient(c.VideoRPC),
		Moderation:     newModerationClient(c.ModerationRPC),
		workerCtx:      workerCtx,
		cancel:         cancel,
	}
	for _, note := range ctx.Notes() {
		logx.Infof("livemedia/svc: %s", note)
	}
	// go-zero 在 SIGTERM/SIGINT 时先触发 wrap-up 监听器，再调用 gRPC 的 shutdown 监听器，
	// 因此这里取消上下文能给发布循环留出收尾时间（在途批次处理完才关连接）。
	proc.AddWrapUpListener(func() { ctx.stopWorkers() })
	logx.Must(ctx.startPublisher())
	return ctx
}

// newCache 构造业务缓存；Host 为空时返回 nil，由调用方回落到 MySQL。
func newCache(c config.Config) *redis.Redis {
	if c.CacheRedis.Host == "" {
		return nil
	}
	return redis.MustNewRedis(c.CacheRedis)
}

// rpcConfigured 与 internal/config/config_load_test.go 共用同一判定，
// 保证「测试里认为可留空」与「svc 里是否真的构造客户端」不会各说各话。
func rpcConfigured(oc zrpc.RpcClientConf) bool {
	return oc.Target != "" || len(oc.Etcd.Hosts) > 0 || len(oc.Endpoints) > 0
}

func newAssetClient(oc zrpc.RpcClientConf) assetrpc.AssetClient {
	if !rpcConfigured(oc) {
		return nil
	}
	return assetrpc.NewAssetClient(zrpc.MustNewClient(oc).Conn())
}

func newVideoClient(oc zrpc.RpcClientConf) videorpc.VideoClient {
	if !rpcConfigured(oc) {
		return nil
	}
	return videorpc.NewVideoClient(zrpc.MustNewClient(oc).Conn())
}

func newModerationClient(oc zrpc.RpcClientConf) moderationrpc.ModerationOrchestratorClient {
	if !rpcConfigured(oc) {
		return nil
	}
	return moderationrpc.NewModerationOrchestratorClient(zrpc.MustNewClient(oc).Conn())
}

// Transact 在单个事务里执行 fn：logic 的所有状态推进写都走这里，
// 使「业务行 + Outbox 事件」要么一起提交、要么一起回滚（AGENTS.md §5）。
// 单测把 DB 换成 fake 即可覆盖「中途失败不留半成品状态」。
func (s *ServiceContext) Transact(ctx context.Context, fn func(ctx context.Context, sess sqlx.Session) error) error {
	return s.DB.TransactCtx(ctx, fn)
}

// Notes 输出装配期诊断信息，便于运维确认「哪些下游在这个环境必然失败」。
func (s *ServiceContext) Notes() []string {
	type entry struct {
		name  string
		built bool
		apis  string
	}
	entries := []entry{
		{"AssetRPC", s.Asset != nil, "Worker 侧 asset.RegisterAsset（回放产物登记）"},
		{"VideoRPC", s.Video != nil, "Worker 侧 video.CreateSubmission（回放建稿）与投影刷新"},
		{"ModerationRPC", s.Moderation != nil, "Worker 侧 moderation.SubmitForReview（回放送审）"},
	}
	notes := make([]string, 0, len(entries)+2)
	for _, e := range entries {
		if e.built {
			notes = append(notes, fmt.Sprintf("%s 已接入（供 %s 使用）", e.name, e.apis))
			continue
		}
		notes = append(notes, fmt.Sprintf("%s 未配置：%s 一旦调用即以显式错误失败，绝不直连下游库表",
			e.name, e.apis))
	}
	if s.Cache == nil {
		notes = append(notes, "CacheRedis 未配置：任务详情与房间档位全部回源 MySQL")
	}
	if s.Config.LiveMedia.TaskTimeoutSweepEnabled {
		notes = append(notes, "TaskTimeoutSweepEnabled=true，但清扫循环由 services/cron 或本服务 consumer 承担，"+
			"本进程不启动调度")
	}
	return notes
}

// startPublisher 三种结果都有明确日志：
//  1. Kafka.Enabled=false：不建发送通道，live_media_outbox 只累积；
//  2. Enabled=true 且二进制链接了运行时（-tags livemedia_kafka）且参数完整：启动发布循环；
//  3. Enabled=true 但运行时未链接、或 Kafka.* 参数不完整：返回错误，进程启动即失败。
func (s *ServiceContext) startPublisher() error {
	for _, note := range publisher.RuntimeNotes(s.Config.Kafka) {
		logx.WithContext(s.workerCtx).Infof("livemedia/svc: %s", note)
	}
	if !s.Config.Kafka.Enabled {
		return nil
	}
	sender, err := publisher.NewSender(publisher.SenderSettingsFrom(s.Config.Kafka))
	if err != nil {
		return err
	}
	pub, err := publisher.NewPublisher(s.Config, s.Outbox, sender)
	if err != nil {
		// 发送通道已建立但参数不合格：必须关掉，否则留下一条没关的连接（9 个 pusher）。
		_ = sender.Close()
		return err
	}
	if err := pub.Start(); err != nil {
		_ = sender.Close()
		return err
	}
	s.Publisher = pub
	logx.WithContext(s.workerCtx).Infof("livemedia/svc: 事件发布器已启动 topics=%v max_attempts=%d batch=%d",
		publisher.RequiredTopics(), s.Config.Kafka.MaxRetries, s.Config.Kafka.BatchLimit)
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
