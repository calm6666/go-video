// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 catalog 服务的配置结构。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 是本服务业务缓存（Work/Season/Episode 详情、分区树）的连接。
	// 字段名不能叫 Redis：zrpc.RpcServerConf 已内嵌一个同名的 RedisKeyConf 字段
	// （供 zrpc 内部鉴权/订阅用），同名会让 go-zero 在加载配置时报
	// "conflict key redis"，服务无法启动。
	CacheRedis redis.RedisConf

	// MySQL 主库连接 DSN。
	DataSource string

	// RightsRPC 版权服务客户端配置（上架前校验版权窗口，AGENTS.md §5/§8）。
	// 配置留空时 ServiceContext 不构造客户端；此时 PublishEpisode 按严格策略
	// 返回 ErrRightsCheckerUnavailable 拒绝上架，除非显式设置 DisableRightsCheck。
	RightsRPC zrpc.RpcClientConf `json:",optional"`

	// AssetRPC 媒资服务客户端配置（建集/上架前校验 asset_id 存在且媒资就绪）。
	// 配置留空时 ServiceContext 不构造客户端；CreateEpisode/PublishEpisode 按严格
	// 策略返回 ErrAssetCheckerUnavailable，除非显式设置 DisableAssetCheck。
	AssetRPC zrpc.RpcClientConf `json:",optional"`

	// DefaultRegion 是 PublishEpisode 请求未携带 region 时使用的兜底地区代码。
	// 留空表示请求必须显式传 region，否则返回 ErrMissingRegion。
	// 单地区部署可以设为 CN；多地区部署不要依赖该兜底值，应由运营入口逐次传入 region。
	DefaultRegion string `json:",optional"`

	// DisableRightsCheck 是版权窗口校验的灰度/回滚开关。
	// 零值 false = 严格校验（默认）：rights 客户端缺失或调用失败时拒绝上架。
	// 置 true 仅用于 rights 故障演练或灰度回滚，会打印 Warn 级日志，
	// 因为版权内容误上架的影响远大于误拒绝（AGENTS.md §8）。
	DisableRightsCheck bool `json:",optional"`

	// DisableAssetCheck 是媒资就绪校验的灰度/回滚开关，语义同上。
	DisableAssetCheck bool `json:",optional"`
}
