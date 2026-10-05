// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 audit 服务（追加式审计）的配置结构。
//
// 依赖面（docs/service-catalog.md）：audit 的上游是 operation 与各写入领域服务，
// 自身不调用任何下游 RPC —— 因此本配置里没有 zrpc.RpcClientConf 字段。
// 鉴权也不在这里做：管理员能不能查审计由 operation.VerifyAdminPermission 判定，
// audit 只负责「查询必须受限」和「读了什么必须留痕」（AGENTS.md §5 数据所有权）。
//
// 商业化范围外（AGENTS.md §1）：不提供广告投放、订单、支付、分成相关的动作维度。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 幂等快路径缓存：event_id 已存在提示、链尾序号短缓存。
	// 不能命名为 Redis：与 zrpc.RpcServerConf 内嵌的同名 RedisKeyConf 字段冲突，
	// 会让 conf.Load 报 conflict key redis 而启动失败（全仓统一约定）。
	//
	// 关键语义：缓存**不是**幂等性的依据。audit_entry.uniq_event_id 唯一索引才是，
	// 缓存只用于让重复投递少打一次库；缓存不可用时服务退化为直连 MySQL，
	// 判定结果不变（AGENTS.md §5：跨服务禁止把 Redis 当业务事实源）。
	CacheRedis redis.RedisConf

	// DataSource 本服务自有库 DSN（库名 go_video_audit）。
	// 严禁指向其它服务的库（AGENTS.md §5）。
	DataSource string

	// Security 脱敏哈希参数。密钥只写环境变量名，不落配置文件明文。
	Security SecurityConf

	// Query 查询硬约束。审计表是千万级大表，放宽约束等于放行全表扫描。
	Query QueryConf

	// Write 写入约束。
	Write WriteConf

	// Verify 哈希链校验的单次上限。
	Verify VerifyConf

	// Export 导出任务参数。
	Export ExportConf

	// Archive 归档作业参数。
	Archive ArchiveConf

	// Storage 归档/导出对象存储。凭据只写环境变量引用名（AGENTS.md §4）。
	Storage StorageConf
}

// SecurityConf 脱敏与哈希配置。
type SecurityConf struct {
	// IpHashSaltRef 存放「来源标识哈希盐」的**环境变量名**。
	// 该变量未注入时 AppendAudit/BatchAppendAudit 直接失败（model.ErrHashSaltMissing）：
	// 裸 SHA-256(IP) 可被 2^32 枚举反查，只有加盐才谈得上不可逆，
	// 因此宁可拒写也不静默降级成弱哈希（AGENTS.md §7、§9）。
	IpHashSaltRef string

	// MaxUserAgentLen UA 入库截断长度（按 rune 截，不产生半个字）。
	// <=0 时服务端按 255。UA 是展示字段、不参与哈希，所以允许截断。
	MaxUserAgentLen int `json:",optional"`
}

// QueryConf 查询硬约束。
type QueryConf struct {
	// MaxRangeDays 单次查询/导出的最大时间跨度（天），<=0 时按 92。
	// 审计查询必须给时间范围：这是接口约束，不是建议。
	MaxRangeDays int `json:",optional"`

	// MaxPageSize 单页最大条数，<=0 时按 100。
	MaxPageSize int `json:",optional"`

	// CountTotal 是否在列表查询里回 COUNT(*)。
	// 大区间上 COUNT 会拖慢查询，默认 true 保证契约里 total 可信；
	// 网关做「滚动加载」时可以按调用方显式要求走无 COUNT 的路径（本期未开放该参数）。
	CountTotal bool `json:",default=true"`
}

// WriteConf 写入约束。
type WriteConf struct {
	// MaxBatchSize BatchAppendAudit 单次条数上限，<=0 时按 200。
	// 上限存在的理由：一批在一个事务里，过大事务会长时间持有链头行锁。
	MaxBatchSize int `json:",optional"`

	// ChainRetry 链头乐观锁冲突后的重试次数，<=0 时按 3。
	// 高并发同域写入会撞同一 chain_key 行，重试比排队等待更省连接。
	ChainRetry int `json:",optional"`

	// MaxReasonLen 自由文本 reason 的入库上限（字符），<=0 时按 500。
	// reason 参与哈希，因此超长**不截断**而是拒绝，避免哈希与库值不一致。
	MaxReasonLen int `json:",optional"`
}

// VerifyConf 哈希链校验配置。
type VerifyConf struct {
	// MaxEntriesPerCall 单次校验的最大条数，<=0 时按 50000。
	// 校验按 seq 升序重放，条数上限即内存上限。
	MaxEntriesPerCall int `json:",optional"`
}

// ExportConf 导出配置。
type ExportConf struct {
	// BatchRows 单次 RunAuditExportTask 推进的行数，<=0 时按 2000。
	BatchRows int `json:",optional"`

	// MaxRowsPerTask 单任务累计最大行数，<=0 时按 2000000。
	// 超过即 failed（而不是无限跑），逼迫调用方按时间分片提交多个任务。
	MaxRowsPerTask int64 `json:",optional"`

	// ObjectTTLSeconds 导出对象保留秒数，<=0 时按 604800（7 天）。
	// 审计导出文件是高敏数据，必须有自动过期时间。
	ObjectTTLSeconds int64 `json:",optional"`

	// PresignTTLSeconds 短期签名下载地址有效秒数，<=0 时按 300。
	PresignTTLSeconds int64 `json:",optional"`
}

// ArchiveConf 归档作业配置。
type ArchiveConf struct {
	// MaxEntriesPerBatch 单归档批次的最大条数，<=0 时按 100000。
	// 批次上限同时决定清单文件大小与一次归档失败的爆炸半径。
	MaxEntriesPerBatch int `json:",optional"`

	// VerifyBeforePurge 是否在标记 archived_at 之前强制重算清单哈希。
	// 默认 true，且不提供 false 的合法路径语义：即使配置写成 false，
	// repository 也会拒绝执行 purged（AGENTS.md §9：不得靠关闭校验让测试通过）。
	VerifyBeforePurge bool `json:",default=true"`
}

// StorageConf 对象存储配置。
type StorageConf struct {
	// Enabled false 时导出/归档直接返回 model.ErrObjectStorageMissing，
	// 而不是「库里记一条成功、实际没有文件」。
	Enabled bool `json:",optional"`

	// Endpoint 例如 127.0.0.1:9000（本地 MinIO）。
	Endpoint string `json:",optional"`

	// Region 例如 us-east-1。
	Region string `json:",optional"`

	// Bucket 审计归档桶名。
	Bucket string `json:",optional"`

	// AccessKeyRef / SecretKeyRef 凭据所在的**环境变量名**，不是凭据本身。
	AccessKeyRef string `json:",optional"`
	SecretKeyRef string `json:",optional"`

	// UseSSL 本地 MinIO 默认关闭，生产必须为 true。
	UseSSL bool `json:",optional"`

	// PathStyle 自建 MinIO 通常需要 path-style 寻址。
	PathStyle bool `json:",optional"`
}
