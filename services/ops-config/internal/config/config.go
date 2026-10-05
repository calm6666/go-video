// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 ops-config 服务（运营配置发布与灰度）的配置结构。
//
// 依赖面：本服务只有**一个下游** —— audit（写自身配置动作的存证，见 rpc/opsconfig.proto 文件头）。
// 鉴权不在本服务做：管理员身份与 RBAC 归 operation，由 gateway/admin 调
// operation.VerifyAdminPermission 完成，本服务只记录 operator_id（AGENTS.md §5）。
// catalog 也不在本配置里：专题只存分区/标签 ID 引用，展示名由调用方自行解析，
// 因此两侧不会因对方改名而联动。
//
// 商业化范围外（AGENTS.md §1、§7）：本结构不存在广告位、投放、出价、排期购买、
// 计费、分成、会员相关字段；config_load_test.go 有守护用例。
// 终端范围（AGENTS.md §1、§6）：只有 Android/iOS/HarmonyOS/桌面端四端，不含小程序。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 运行时读缓存连接：解析结果、专题视图、坑位视图、按端的能力开关全集。
	// 不能命名为 Redis —— zrpc.RpcServerConf 内嵌了同名的 auth.RedisConf 字段，
	// 会让 conf.Load 报 "conflict key redis" 导致服务启动即失败（全仓统一约定，
	// internal/config/config_load_test.go 的 TestExampleConfigsLoad 会真实加载 etc/*.yaml 兜底）。
	//
	// 关键语义：这里的缓存是**可整域重建的只读投影**，不是事实源。
	// 全部数据都在 ops_* 表里，RefreshCache 的语义就是「丢掉投影」；
	// Redis 不可用时服务退化为直连 MySQL，判定结果不变（AGENTS.md §5）。
	CacheRedis redis.RedisConf

	// DataSource 本服务自有库 DSN（库名 go_video_ops_config）。
	// 严禁指向其它服务的库：分区/标签在 go_video_catalog，管理员在 go_video_operation
	// （AGENTS.md §5）。
	DataSource string

	// AuditRPC audit 服务客户端。留空时不构造客户端：
	// 写接口仍会正常推进配置状态机，但 audit_entry_id 留 0 并打 Error 日志，
	// 由补偿任务重投（proto 文件头约定的「审计缺口可见而不是被静默吞掉」）。
	// 绝不因为审计不可用而伪造一个 entry_id。
	AuditRPC zrpc.RpcClientConf `json:",optional"`

	// OpsValue 配置键、值与版本的长度约束。
	OpsValue ValueConf

	// OpsTopic 专题侧约束（条目数与引用列表长度）。
	OpsTopic TopicConf

	// OpsSlot 坑位侧约束（容量与条目数）。
	OpsSlot SlotConf

	// Rollout 灰度规则侧约束。
	Rollout RolloutConf

	// Resolve 运行时解析侧约束（批量上限与建议 TTL）。
	Resolve ResolveConf

	// Query 后台列表分页约束。
	Query QueryConf

	// Cache 缓存键前缀与各域 TTL 上限。
	Cache CacheConf
}

// ValueConf 配置内容的长度约束。这些值同时是建表时的列宽依据，
// 改大要先改迁移，改小要在 README 里说明「历史长值怎么办」。
type ValueConf struct {
	// MaxBytes 单个配置值的最大字节数，<=0 时按 8192。
	// 超过即 ErrValueTooLong：配置值会进 Redis、进网关响应、进审计摘要，
	// 一个 MB 级的 JSON 会让这三处同时退化，而它本该是一行配置。
	// 必须与 ops_config_version.cfg_value 的列宽一致（迁移 000001）。
	MaxBytes int

	// MaxKeyLen cfg_key 最大长度，<=0 时按 64。与 model.cfgKeyRe 的 {1,64} 一致。
	MaxKeyLen int `json:",optional"`

	// MaxReasonLen 发布/回滚/启停理由的最大长度，<=0 时按 500。
	// 超长是拒绝而不是截断：理由参与审计摘要，截断会让摘要与库值不一致。
	MaxReasonLen int `json:",optional"`

	// MaxOperatorNameLen 操作人展示名快照的最大长度，<=0 时按 64。
	MaxOperatorNameLen int `json:",optional"`
}

// TopicConf 专题侧约束。
type TopicConf struct {
	// MaxItems 单个专题的条目上限，<=0 时按 500，且不得超过 model.MaxTopicItems（硬上限）。
	MaxItems int

	// MaxRefIDs 单个专题可引用的分区/标签个数，<=0 时按 64（model.MaxTopicRefIDs）。
	MaxRefIDs int `json:",optional"`

	// DefaultItemLimit GetTopicReq.item_limit<=0 时的服务端默认，<=0 时按 100。
	DefaultItemLimit int `json:",optional"`

	// ListTTLSeconds 专题视图缓存的建议 TTL 上限，<=0 时按 60。
	ListTTLSeconds int `json:",optional"`
}

// SlotConf 坑位侧约束。
type SlotConf struct {
	// MaxCapacity 坑位容量上限，<=0 时按 200，且不得超过 model.MaxSlotCapacityHard（硬上限）。
	// 它是 ErrSlotCapacityInvalid 的来源：没有上限就等于给网关一个无界结果集。
	MaxCapacity int

	// MaxItems 单坑位条目上限，<=0 时按 200，且不得超过 model.MaxSlotItems。
	MaxItems int `json:",optional"`

	// DefaultTTLSeconds ResolveSlot 的建议缓存秒数上限，<=0 时按 30。
	// 坑位是运营随时改的东西，TTL 长了会出现「后台改了端上没变」的排障黑洞。
	DefaultTTLSeconds int `json:",optional"`
}

// RolloutConf 灰度规则侧约束。
type RolloutConf struct {
	// MaxWhitelistMids 白名单 mid 数量上限，<=0 时按 200。
	// 只能 <= model.MaxWhitelistMids（硬上限）：白名单是排障工具，不是放量手段。
	// config_load_test.go 会拒绝把这里配得比硬上限更松。
	MaxWhitelistMids int

	// MaxRulesPerConfig 单次解析取回的候选规则上限，<=0 时按 50（model.MaxRolloutCandidates）。
	MaxRulesPerConfig int `json:",optional"`
}

// ResolveConf 运行时解析约束。
type ResolveConf struct {
	// MaxBatchKeys BatchResolveConfig 单次键数上限（契约写 50），<=0 时按 50。
	// 这是网关聚合接口的扇出上界：没有它，一次首页请求可以变成一次全表扫描。
	MaxBatchKeys int

	// DefaultTTLSeconds 未显式指定时给调用方的建议缓存秒数，<=0 时按 60。
	DefaultTTLSeconds int `json:",optional"`

	// DefaultTopicTTLSeconds topic 视图的建议 TTL，<=0 时按 60。
	DefaultTopicTTLSeconds int `json:",optional"`
}

// QueryConf 后台列表约束。
type QueryConf struct {
	// MaxPageSize 单页最大条数，<=0 时按 100。
	// 契约里 ps 上限 100，超限按「夹到上限」处理（见 model.PageOrDefault）。
	MaxPageSize int

	// CountTotal 列表是否回 COUNT(*)。本服务是千级配置表，默认 true 保证 total 可信。
	CountTotal bool `json:",default=true"`
}

// CacheConf 缓存键与 TTL 约束。
type CacheConf struct {
	// KeyPrefix 所有缓存键的统一前缀（如 govideo:opsconfig）。
	// 前缀必须可配置：多套环境共用一个 Redis 实例时，这是唯一的隔离手段。
	KeyPrefix string

	// MaxTTLSeconds 任何建议 TTL 的硬上限，<=0 时按 3600。
	// ResolveConfig/BatchResolve/ResolveSlot 回的 ttl 一律被夹到 [0, MaxTTLSeconds]：
	// 一次错误放量最长要等一个 TTL 才能被踢掉，所以宁可让调用方多打几次回源。
	MaxTTLSeconds int

	// DeleteOnBump 发布/RefreshCache 时是否同步删除读缓存键。
	// 默认 true。设为 false 时只递增 epoch，靠调用方比对 epoch 收敛 ——
	// 这只在缓存与库不在同一集群的过渡期才需要，正常部署不要关。
	DeleteOnBump bool `json:",default=true"`
}
