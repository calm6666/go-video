// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"

	catalogrpc "go-video/services/catalog/rpc"
	"go-video/services/cron/internal/config"
	"go-video/services/cron/internal/registry"
	"go-video/services/cron/model"
	engagementrpc "go-video/services/engagement/rpc"
	inboxrpc "go-video/services/inbox/rpc"
	notificationrpc "go-video/services/notification/rpc"
	rightsrpc "go-video/services/rights/rpc"
	searchindexerrpc "go-video/services/search-indexer/rpc"
	videorpc "go-video/services/video/rpc"
)

// ServiceContext 是 cron 的运行时上下文。
//
// 装配原则（AGENTS.md §3、§5）：
//   - cron 只拥有 go_video_cron 库的 5 张调度表；任何业务写操作都通过下面的领域
//     客户端完成，绝不 import 其它服务的 internal/model 包，也不直连它们的库表/Redis key；
//   - 下游客户端全部可选：Target/Etcd.Hosts/Endpoints 皆空时字段保持 nil，
//     调用方拿到 ErrDownstreamNotConfigured 显式失败（任务进入退避），
//     而不是「安静地跳过」或降级成裸 SQL；
//   - CacheRedis 只做「到期索引/健康度计数」的加速副本，真值恒在 MySQL；
//   - 调度循环（worker tick）在第二轮由 NewServiceContext 用
//     proc.AddWrapUpListener + threading.GoSafeCtx 启动，因此入口文件 cron.v1.go
//     （goctl 生成、不可手改）无需任何改动，与 inbox 的消费者装配方式一致。
type ServiceContext struct {
	Config config.Config

	// --- 本服务自有表的数据访问（一轮交付，SQL 已定；logic 二轮接线）---
	TaskDefinitions model.TaskDefinitionModel
	Runs            model.TaskRunModel
	Leases          model.TaskLeaseModel
	Checkpoints     model.TaskCheckpointModel
	Audits          model.TaskAuditModel

	// Cache 只读加速层；nil 表示未配置，调用方必须回落到 MySQL。
	Cache *redis.Redis

	// Conn 本服务库（go_video_cron）的连接。logic 只用它开事务
	// （见 Transact），SQL 一律留在 model 的 *Tx 变体里（AGENTS.md §4）。
	Conn sqlx.SqlConn

	// Registry 任务处理器注册表：DB 里的 handler 名必须在此注册，
	// 否则该任务以 ErrHandlerNotRegistered 失败而不是被静默跳过（AGENTS.md §9）。
	Registry *registry.Registry

	// --- 下游领域服务客户端（nil 表示该环境未接入）---
	Rights        rightsrpc.RightsClient
	Catalog       catalogrpc.CatalogClient
	Video         videorpc.VideoClient
	SearchIndexer searchindexerrpc.SearchIndexerClient
	Notification  notificationrpc.NotificationClient
	Engagement    engagementrpc.EngagementClient
	Inbox         inboxrpc.InboxClient

	// workerID 本进程实例标识，用于租约 owner 与执行记录 lease_owner。
	workerID string
	// tickCount 已完成的调度轮次（诊断用；第二轮的 tick 循环里递增）。
	tickCount atomic.Int64
}

// NewServiceContext 构造上下文。
//
// 这里刻意不做「连不上就退出」的强校验：DataSource 由 sqlx.NewMysql 惰性建连，
// 配置缺项由 internal/config/config_load_test.go 在 CI 阶段拦住。
func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	ctx := &ServiceContext{
		Config:          c,
		TaskDefinitions: model.NewTaskDefinitionModel(conn),
		Runs:            model.NewTaskRunModel(conn),
		Leases:          model.NewTaskLeaseModel(conn),
		Checkpoints:     model.NewTaskCheckpointModel(conn),
		Audits:          model.NewTaskAuditModel(conn),
		Conn:            conn,
		Registry:        registry.New(),
		workerID:        buildWorkerID(c),
	}
	if c.CacheRedis.Host != "" {
		ctx.Cache = redis.MustNewRedis(c.CacheRedis)
	}
	ctx.Rights = newRightsClient(c.RightsRPC)
	ctx.Catalog = newCatalogClient(c.CatalogRPC)
	ctx.Video = newVideoClient(c.VideoRPC)
	ctx.SearchIndexer = newSearchIndexerClient(c.SearchIndexerRPC)
	ctx.Notification = newNotificationClient(c.NotificationRPC)
	ctx.Engagement = newEngagementClient(c.EngagementRPC)
	ctx.Inbox = newInboxClient(c.InboxRPC)

	for _, note := range ctx.DownstreamNotes() {
		logx.Infof("cron/svc: %s", note)
	}
	return ctx
}

// isConfigured 与 internal/config/config_load_test.go 共用同一判定，
// 保证「测试里认为可留空」与「svc 里是否真的构造」不会各说各话。
func isConfigured(c zrpc.RpcClientConf) bool {
	return c.Target != "" || len(c.Etcd.Hosts) > 0 || len(c.Endpoints) > 0
}

func newRightsClient(c zrpc.RpcClientConf) rightsrpc.RightsClient {
	if !isConfigured(c) {
		return nil
	}
	return rightsrpc.NewRightsClient(zrpc.MustNewClient(c).Conn())
}

func newCatalogClient(c zrpc.RpcClientConf) catalogrpc.CatalogClient {
	if !isConfigured(c) {
		return nil
	}
	return catalogrpc.NewCatalogClient(zrpc.MustNewClient(c).Conn())
}

func newVideoClient(c zrpc.RpcClientConf) videorpc.VideoClient {
	if !isConfigured(c) {
		return nil
	}
	return videorpc.NewVideoClient(zrpc.MustNewClient(c).Conn())
}

func newSearchIndexerClient(c zrpc.RpcClientConf) searchindexerrpc.SearchIndexerClient {
	if !isConfigured(c) {
		return nil
	}
	return searchindexerrpc.NewSearchIndexerClient(zrpc.MustNewClient(c).Conn())
}

func newNotificationClient(c zrpc.RpcClientConf) notificationrpc.NotificationClient {
	if !isConfigured(c) {
		return nil
	}
	return notificationrpc.NewNotificationClient(zrpc.MustNewClient(c).Conn())
}

func newEngagementClient(c zrpc.RpcClientConf) engagementrpc.EngagementClient {
	if !isConfigured(c) {
		return nil
	}
	return engagementrpc.NewEngagementClient(zrpc.MustNewClient(c).Conn())
}

func newInboxClient(c zrpc.RpcClientConf) inboxrpc.InboxClient {
	if !isConfigured(c) {
		return nil
	}
	return inboxrpc.NewInboxClient(zrpc.MustNewClient(c).Conn())
}

// DownstreamNotes 输出装配期诊断信息，便于运维确认「某个任务在这个环境必然失败」。
func (s *ServiceContext) DownstreamNotes() []string {
	type entry struct {
		name    string
		built   bool
		depends []string
	}
	entries := []entry{
		{"RightsRPC", s.Rights != nil, []string{"rights.expire_scan"}},
		{"CatalogRPC", s.Catalog != nil, []string{"catalog.window_offline"}},
		{"VideoRPC", s.Video != nil, []string{"submission.state_sweep"}},
		{"SearchIndexerRPC", s.SearchIndexer != nil, []string{"index.rebuild_stale", "index.alias_patrol"}},
		{"NotificationRPC", s.Notification != nil, []string{"notification.deadletter_replay"}},
		{"EngagementRPC", s.Engagement != nil, []string{"engagement.count_repair"}},
		{"InboxRPC", s.Inbox != nil, []string{"inbox.unread_recompute"}},
	}
	notes := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.built {
			notes = append(notes, fmt.Sprintf("%s 已接入（供 %v 使用）", e.name, e.depends))
			continue
		}
		notes = append(notes, fmt.Sprintf("%s 未配置：依赖它的任务将以 %v 失败并进入退避，绝不绕过 RPC 直连下游库表",
			e.name, model.ErrDownstreamNotConfigured))
	}
	return notes
}

// WorkerID 返回本进程实例标识，用作租约 owner 与 cron_task_run.lease_owner。
func (s *ServiceContext) WorkerID() string { return s.workerID }

// BuildVersion 返回构建版本，供 GetSchedulerHealth 排障定位。
// 一轮不引入构建注入变量：这里只报告 Go 版本与进程名，二轮由 -ldflags 注入。
func (s *ServiceContext) BuildVersion() string {
	return fmt.Sprintf("cron.v1 (go %s, pid %d)", runtimeGoVersion(), os.Getpid())
}

// ServerTime 统一的服务端时钟（Unix 秒），供调度端校正本地漂移。
func (s *ServiceContext) ServerTime() int64 { return unixNow() }

// TickCount 已完成的调度轮次数（诊断用；一轮恒为 0，因为循环在第二轮接入）。
func (s *ServiceContext) TickCount() int64 { return s.tickCount.Load() }

// ErrNoCache 显式表达「缓存未配置」，调用方必须回落 MySQL 而不是返回空结果。
var ErrNoCache = errors.New("cron: cache redis is not configured")

// LeaseTTL 按配置上下界收敛某个任务实际使用的租约 TTL（秒）。
// 任务未声明时用 Scheduler 默认值；越界则夹紧到 [Min,Max]，
// 并把结果同时用于 cron_task_lease 与 cron_task_run 的过期时间，避免两处不一致。
func (s *ServiceContext) LeaseTTL(taskTTLSeconds int32) (int64, error) {
	ttl := int64(taskTTLSeconds)
	if ttl <= 0 {
		ttl = s.Config.Lease.DefaultTTLSeconds
	}
	if ttl < s.Config.Lease.MinTTLSeconds {
		ttl = s.Config.Lease.MinTTLSeconds
	}
	if ttl > s.Config.Lease.MaxTTLSeconds {
		return 0, fmt.Errorf("%w: ttl=%d, max=%d", model.ErrInvalidLeaseTTL,
			ttl, s.Config.Lease.MaxTTLSeconds)
	}
	if ttl < model.MinLeaseTTLSeconds {
		return 0, fmt.Errorf("%w: ttl=%d, minimum=%d", model.ErrInvalidLeaseTTL, ttl, model.MinLeaseTTLSeconds)
	}
	// 心跳必须显著快于 TTL，否则崩溃实例的租约要等到过期才能被抢占，
	// 这种配置属于「能跑但不安全」，直接拒绝。
	if s.Config.Lease.HeartbeatSeconds > 0 &&
		int64(s.Config.Lease.HeartbeatSeconds)*3 >= ttl {
		return 0, fmt.Errorf("%w: ttl=%d 与 heartbeat=%ds 不匹配（心跳需快于 TTL 的 1/3）",
			model.ErrInvalidLeaseTTL, ttl, s.Config.Lease.HeartbeatSeconds)
	}
	return ttl, nil
}

// PageSize 把请求的分页大小收敛到本服务允许的范围。
func (s *ServiceContext) PageSize(requested int32) (int, error) {
	maxAllowed := s.Config.Task.MaxPageSize
	if maxAllowed <= 0 || maxAllowed > model.MaxPageSize {
		maxAllowed = model.MaxPageSize
	}
	if requested <= 0 {
		return int(s.Config.Task.DefaultPageSize), nil
	}
	if requested > maxAllowed {
		return 0, fmt.Errorf("%w: page_size=%d, max=%d", model.ErrInvalidPageLimit, requested, maxAllowed)
	}
	return int(requested), nil
}

// Transact 在 go_video_cron 库上执行一个事务。
//
// logic 用它把「写执行记录 + 抢租约 + 推进调度指针 + 写审计」绑成一次原子操作
// （AGENTS.md §5）；传出的 session 只能交给 model 的 *Tx 变体使用，
// logic 自身不得拼 SQL（AGENTS.md §4）。
func (s *ServiceContext) Transact(
	ctx context.Context, fn func(ctx context.Context, tx sqlx.Session) error,
) error {
	if s.Conn == nil {
		return errors.New("cron: database connection is not configured")
	}
	return s.Conn.TransactCtx(ctx, fn)
}

// DefaultTimezone 任务未声明 timezone 时使用的时区（配置缺省即 Asia/Shanghai）。
func (s *ServiceContext) DefaultTimezone() string { return s.Config.Task.DefaultTimezone }

// buildWorkerID 生成稳定的实例标识：优先配置前缀，其次 hostname，最后 pid + 随机后缀。
// 租约的可抢占性依赖 owner 唯一，因此这里必须保证同集群内不重复。
func buildWorkerID(c config.Config) string {
	prefix := strings.TrimSpace(c.Scheduler.WorkerIDPrefix)
	if prefix == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "cron"
		}
		prefix = host
	}
	return fmt.Sprintf("%s-%d-%s", prefix, os.Getpid(), randSuffix())
}

// randSuffix 生成 6 位实例后缀，避免同机多进程 pid 复用时的 owner 冲撞。
func randSuffix() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 读失败时退化到时间戳后缀：宁可标识可读性差，也不能让 owner 为空。
		return strconv.FormatInt(time.Now().UnixNano()%1e6, 36)
	}
	for i, b := range buf {
		buf[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(buf)
}

// runtimeGoVersion 供 BuildVersion 输出，便于排障时区分镜像里的工具链。
func runtimeGoVersion() string { return runtime.Version() }

// unixNow 服务端统一时钟（Unix 秒）。
func unixNow() int64 { return time.Now().Unix() }
