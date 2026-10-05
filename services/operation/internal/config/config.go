// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 operation 服务（管理后台领域服务）的配置结构。
//
// 与 gateway/admin 的分工（docs/api-and-events.md §1.1）：网关只做入口聚合、
// 统一鉴权与限流；管理员账号、RBAC、运营配置、管理任务与审计索引的业务规则
// 与数据都在本服务，因此本服务需要 MySQL（自有库 go_video_operation）与
// Redis（判定/配置/菜单缓存）两类基础设施，以及下游领域服务的 RPC 客户端配置。
//
// 商业化范围外（AGENTS.md §1）：不提供支付/订单/广告投放相关配置项。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 业务缓存连接：权限快照、运营配置、菜单树、会话。
	// 不能命名为 Redis：与 zrpc.RpcServerConf 内嵌的同名 RedisKeyConf 字段冲突，
	// 会让 conf.MustLoad 报 conflict key redis 而启动失败（与 playback/comment 同约定）。
	CacheRedis redis.RedisConf

	// DataSource 本服务自有库 DSN（库名 go_video_operation）。
	// 严禁指向其它服务的库（AGENTS.md §5 数据所有权）。
	DataSource string

	// AdminSession 后台会话（token）签发参数。
	AdminSession AdminSessionConf

	// Login 登录防爆破策略。
	Login LoginConf

	// Cache 各类缓存 TTL 与任务推进批量。
	Cache CacheConf

	// VideoRPC video 服务客户端（稿件下架）。留空表示未部署，
	// 任务步骤会返回「下游不可用」而不是静默成功。
	VideoRPC zrpc.RpcClientConf `json:",optional"`

	// CatalogRPC catalog 服务客户端（目录集下架）。
	CatalogRPC zrpc.RpcClientConf `json:",optional"`

	// RightsRPC rights 服务客户端（版权窗口提前失效）。
	RightsRPC zrpc.RpcClientConf `json:",optional"`

	// ModerationRPC moderation-orchestrator 服务客户端（申诉处理）。
	ModerationRPC zrpc.RpcClientConf `json:",optional"`

	// AccountRPC account 服务客户端（管理员二次校验码通道）。
	// 只调用其公开 RPC：不读写用户账号/会话表，两端 token 互不通用。
	AccountRPC zrpc.RpcClientConf `json:",optional"`
}

// AdminSessionConf 后台会话配置。
type AdminSessionConf struct {
	// TokenTTL 管理 token 有效秒数，<=0 时服务端按 7200 兜底。
	// 后台会话应显著短于用户端 30 天，降低令牌泄露窗口。
	TokenTTL int64 `json:",optional"`

	// TokenSecretRef 存放 token 摘要密钥的**环境变量名**（不落配置文件明文）。
	// 该变量未注入时本服务拒绝签发后台会话（model.ErrTokenSecretMissing），
	// 登录返回明确错误，不会退化成无摘要段的弱 token；生产由 compose/Secret 注入。
	// 密钥只用于打库前的本地格式快速校验，
	// 授权依据仍是 op_admin_session 表。
	TokenSecretRef string `json:",optional"`

	// Issuer 签发方标识，参与 token 前缀（adm_<issuer>_<random>_<sig>），
	// 便于多环境隔离与排查。留空时按 "op"。
	Issuer string `json:",optional"`
}

// LoginConf 登录防爆破配置。
type LoginConf struct {
	// MaxFail 连续失败次数阈值，<=0 时按 5。
	MaxFail int32 `json:",optional"`

	// LockMinutes 达到阈值后的锁定时长（分钟），<=0 时按 15。
	LockMinutes int64 `json:",optional"`
}

// CacheConf 缓存与执行批量配置。
type CacheConf struct {
	// PermissionTTL 权限判定快照缓存秒数，同时作为 gateway/admin 的建议缓存秒数，
	// <=0 时按 60。授权关系写入时会主动失效，该值只决定收敛上限。
	PermissionTTL int `json:",optional"`

	// ConfigTTL 运营配置读取缓存秒数，<=0 时按 60。
	ConfigTTL int `json:",optional"`

	// MenuTTL 菜单树缓存秒数，<=0 时按 300。
	MenuTTL int `json:",optional"`

	// RunSteps RunAdminTask 单次推进的最大步数，<=0 时按 100，上限 1000。
	RunSteps int32 `json:",optional"`
}
