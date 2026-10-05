// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"go-video/common/ratelimit"
	"go-video/services/danmaku/internal/config"
	"go-video/services/danmaku/internal/policy"
	"go-video/services/danmaku/internal/repository"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"
)

// ServiceContext 是 danmaku 服务的运行时上下文，承载跨请求共享的依赖。
// logic 通过它访问 repository、审核客户端和进程级限流器。
type ServiceContext struct {
	Config config.Config

	// Repository 是数据访问入口（MySQL + Redis）。
	Repository *repository.Repository

	// Moderation 是 moderation-orchestrator 客户端；未配置或初始化失败时为 nil。
	Moderation repository.ModerationClient

	// PostLimiter 是进程级令牌桶，复用 common/ratelimit，
	// 用于在突发流量下保护 MySQL 与审核下游（不重写限流算法）。
	PostLimiter ratelimit.Limiter

	// SegmentSeconds 是规整后的时间分段秒数。
	SegmentSeconds int32
}

// NewServiceContext 构造 ServiceContext。
//
// 下游依赖缺失时的策略：只记日志、不 panic，让服务可独立启动；
// 但机审开关打开而审核客户端不可用时，PostDanmaku 会返回明确错误，
// 绝不把未送审的弹幕标成可见（AGENTS.md §8、§9 禁止伪造成功）。
func NewServiceContext(c config.Config) *ServiceContext {
	rds := redis.MustNewRedis(c.CacheRedis)
	conn := sqlx.NewMysql(c.DataSource)

	repo := repository.New(rds, conn, repository.Options{
		Cache: repository.CacheOptions{
			SegmentTTLSeconds:      c.Danmaku.SegmentCacheTTLSeconds,
			SegmentCountTTLSeconds: c.Danmaku.SegmentCountTTLSeconds,
			BlockWordTTLSeconds:    c.Danmaku.BlockWordCacheTTLSeconds,
			UserBlockTTLSeconds:    c.Danmaku.UserBlockCacheTTLSeconds,
		},
		MaxSegWindow:           c.Danmaku.MaxSegWindow,
		MaxListLimit:           c.Danmaku.MaxListLimit,
		BlockWordCacheMaxWords: c.Danmaku.BlockWordCacheMaxWords,
	})

	var moderation repository.ModerationClient
	if moderationConfigured(c.ModerationRPC) {
		cli, err := repository.NewModerationClient(c.ModerationRPC)
		if err != nil {
			logx.Errorf("danmaku/svc: init moderation rpc client failed: %v", err)
		} else {
			moderation = cli
		}
	} else if c.Danmaku.MachineReviewEnabled {
		logx.Errorf("danmaku/svc: ModerationRPC 未配置但机审门禁已开启，PostDanmaku 将返回 danmaku: moderation rpc not configured")
	}

	qps := c.Danmaku.PostQps
	burst := c.Danmaku.PostBurst
	if burst < qps {
		burst = qps
	}

	return &ServiceContext{
		Config:         c,
		Repository:     repo,
		Moderation:     moderation,
		PostLimiter:    ratelimit.NewTokenBucket(int(qps), int(burst)),
		SegmentSeconds: policy.SegmentSeconds(c.Danmaku.SegmentSeconds),
	}
}

// moderationConfigured 判断审核 RPC 是否给出可用的发现配置。
func moderationConfigured(c zrpc.RpcClientConf) bool {
	return len(c.Endpoints) > 0 || c.Target != "" || len(c.Etcd.Hosts) > 0
}
