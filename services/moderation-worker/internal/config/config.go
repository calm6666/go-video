// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 moderation-worker 服务的配置结构。
// 本期不引入 FFmpeg/算法 SDK 依赖；算法接入后扩展 Algorithm 段。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 业务缓存与任务幂等锁连接。
	CacheRedis redis.RedisConf // 不能命名为 Redis：与 zrpc.RpcServerConf 内嵌字段同名会让配置加载失败

	// MySQL 主库连接 DSN（worker_task 表）。
	DataSource string

	// DefaultTimeoutMs 单任务默认超时（毫秒），Run* 请求 timeout_ms=0 时使用。
	DefaultTimeoutMs int64 `json:",default=60000"`
}
