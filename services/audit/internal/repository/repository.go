// Package repository 是 audit 服务的数据访问层。
// 它组合自有表 model、幂等缓存与哈希盐，为 logic 层提供统一入口。
//
// 本轮范围说明：契约、model、迁移与配置装配已落地，用例级编排
// （链上追加的事务重试、导出分批写对象存储、归档清单校验）由后续逻辑轮实现，
// logic 层当前一律返回 model.ErrNotImplemented，不给假成功（AGENTS.md §9）。
// 本文件里已实现的部分只有「不可能写错也必须统一」的两类东西：
//  1. 依赖装配与默认值归一（避免各 logic 各写一套兜底常量）；
//  2. 查询硬约束的判定（放开它等于放行千万级大表的全表扫描）。
package repository

import (
	"context"
	"errors"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/audit/model"
)

// 默认值：配置未给或给 0 时生效。集中在这里，是为了让「配置缺省」与
// 「代码兜底」只有一处定义，改默认值不会漏改。
const (
	defaultMaxRangeDays      = 92
	defaultMaxPageSize       = 100
	defaultMaxBatchSize      = 200
	defaultChainRetry        = 3
	defaultMaxReasonLen      = 500
	defaultMaxUserAgentLen   = 255
	defaultMaxVerifyEntries  = 50000
	defaultExportBatchRows   = 2000
	defaultExportMaxRows     = int64(2000000)
	defaultObjectTTLSeconds  = int64(604800)
	defaultPresignTTLSeconds = int64(300)
	defaultArchiveMaxEntries = 100000
)

// StorageConf 对象存储配置（凭据以环境变量名形式传入，不传值本身）。
type StorageConf struct {
	Enabled      bool
	Endpoint     string
	Region       string
	Bucket       string
	AccessKeyRef string
	SecretKeyRef string
	UseSSL       bool
	PathStyle    bool
}

// Options 归一化后的运行参数。
type Options struct {
	// IpHashSalt 来源 IP / 设备标识的哈希盐。缺失时写入路径返回
	// model.ErrHashSaltMissing，而不是退化成裸 SHA-256。
	IpHashSalt []byte

	MaxRangeDays      int
	MaxPageSize       int
	MaxBatchSize      int
	ChainRetry        int
	MaxReasonLen      int
	MaxUserAgentLen   int
	MaxVerifyEntries  int
	ExportBatchRows   int
	ExportMaxRows     int64
	ObjectTTLSeconds  int64
	PresignTTLSeconds int64
	ArchiveMaxEntries int

	// VerifyBeforePurge 恒按 true 执行：即使配置写 false，
	// PurgeMark 也会拒绝（见 PurgeMark 的注释）。字段保留只为让
	// 「谁在配置里关掉了校验」在部署产物里可见、可追溯。
	VerifyBeforePurge bool

	Storage StorageConf
}

// Repository 聚合自有表 model 与运行时参数。
type Repository struct {
	cache *redis.Redis
	conn  sqlx.SqlConn
	opts  Options

	Entries  model.AuditEntryModel
	Chains   model.AuditChainHeadModel
	Exports  model.ExportTaskModel
	Policies model.RetentionPolicyModel
	Archives model.ArchiveBatchModel
}

// New 构造 Repository。
func New(cache *redis.Redis, conn sqlx.SqlConn, opts Options) *Repository {
	opts = withDefaults(opts)
	return &Repository{
		cache:    cache,
		conn:     conn,
		opts:     opts,
		Entries:  model.NewAuditEntryModel(conn),
		Chains:   model.NewAuditChainHeadModel(conn),
		Exports:  model.NewExportTaskModel(conn),
		Policies: model.NewRetentionPolicyModel(conn),
		Archives: model.NewArchiveBatchModel(conn),
	}
}

// withDefaults 把 0 值配置替换成默认值，并夹到安全上界。
// 上界夹取（而不是报错）是刻意的：MaxPageSize 配成 10 万应该是被压回 100，
// 而不是让服务在第一次查询时才失败。
func withDefaults(o Options) Options {
	if o.MaxRangeDays <= 0 {
		o.MaxRangeDays = defaultMaxRangeDays
	}
	if o.MaxPageSize <= 0 || o.MaxPageSize > defaultMaxPageSize {
		o.MaxPageSize = defaultMaxPageSize
	}
	if o.MaxBatchSize <= 0 || o.MaxBatchSize > defaultMaxBatchSize {
		o.MaxBatchSize = defaultMaxBatchSize
	}
	if o.ChainRetry <= 0 {
		o.ChainRetry = defaultChainRetry
	}
	if o.MaxReasonLen <= 0 {
		o.MaxReasonLen = defaultMaxReasonLen
	}
	if o.MaxUserAgentLen <= 0 {
		o.MaxUserAgentLen = defaultMaxUserAgentLen
	}
	if o.MaxVerifyEntries <= 0 {
		o.MaxVerifyEntries = defaultMaxVerifyEntries
	}
	if o.ExportBatchRows <= 0 {
		o.ExportBatchRows = defaultExportBatchRows
	}
	if o.ExportMaxRows <= 0 {
		o.ExportMaxRows = defaultExportMaxRows
	}
	if o.ObjectTTLSeconds <= 0 {
		o.ObjectTTLSeconds = defaultObjectTTLSeconds
	}
	if o.PresignTTLSeconds <= 0 {
		o.PresignTTLSeconds = defaultPresignTTLSeconds
	}
	if o.ArchiveMaxEntries <= 0 {
		o.ArchiveMaxEntries = defaultArchiveMaxEntries
	}
	return o
}

// Options 返回生效参数（logic 层据此填 ttl / max_range_seconds 等回参）。
func (r *Repository) Options() Options { return r.opts }

// Conn 暴露事务入口：链上追加必须在同一事务里「锁定链头 + 写条目 + 推进链头」，
// 因此 logic 层需要能开启事务，但不该直接持有 driver。
func (r *Repository) Conn() sqlx.SqlConn { return r.conn }

// Salt 返回哈希盐；空表示未注入，写入路径必须先判空。
func (r *Repository) Salt() []byte { return r.opts.IpHashSalt }

// HashSource 计算来源标识的加盐短哈希（IP / 设备号共用一把盐）。
// 空值返回空串，让「未知来源」在库里与「已知来源」可区分。
func (r *Repository) HashSource(value string) string {
	if len(r.opts.IpHashSalt) == 0 {
		return ""
	}
	return model.ShortHash(string(r.opts.IpHashSalt), value)
}

// ClampPage 归一化分页参数：pn 至少 1，ps 落到 [1, MaxPageSize]。
// 返回 ps 供 model 拼 LIMIT，非法到无法解释的输入由 caller 判 ErrInvalidPage。
func (r *Repository) ClampPage(pn, ps int32) (int32, int32, error) {
	if pn < 0 || ps < 0 {
		return 0, 0, model.ErrInvalidPage
	}
	if pn == 0 {
		pn = 1
	}
	if ps == 0 || ps > int32(r.opts.MaxPageSize) {
		ps = int32(r.opts.MaxPageSize)
	}
	return pn, ps, nil
}

// CheckQueryWindow 校验审计查询/导出的硬约束：
//  1. 必须给时间范围，且 start_at < end_at；
//  2. 跨度不超过 MaxRangeDays；
//  3. 至少给一个收窄维度（actor / action / action_domain / target / trace）。
//
// 这三条是接口约束而不是建议：audit_entry 是按年亿级的只增表，
// 无界扫描会直接把库拖垮，而「谁在什么时候查了什么」本身也是合规风险点。
func (r *Repository) CheckQueryWindow(startAt, endAt int64, narrowed bool) error {
	if startAt <= 0 || endAt <= 0 {
		return model.ErrQueryRangeRequired
	}
	if startAt >= endAt {
		return model.ErrQueryRangeRequired
	}
	maxSeconds := int64(r.opts.MaxRangeDays) * 86400
	if endAt-startAt > maxSeconds {
		return model.ErrQueryRangeTooWide
	}
	if !narrowed {
		return model.ErrQueryTooBroad
	}
	return nil
}

// MaxRangeSeconds 返回当前生效的查询跨度上限，供回参告诉调用方自我修正。
func (r *Repository) MaxRangeSeconds() int64 {
	return int64(r.opts.MaxRangeDays) * 86400
}

// ErrPurgeWithoutVerify 跳过清单校验就清热表的显式失败。
// 不复用 model 的哨兵：这条防线属于 repository 的编排规则，不是数据层语义。
var ErrPurgeWithoutVerify = errors.New("audit/repository: refusing to purge hot rows before the archive manifest is verified")

// PurgeMark 把一批已归档条目标记为 archived_at。
// 这里写死了「必须先有 verified 批次」的前置检查，且不受 VerifyBeforePurge 配置影响：
// 销毁/隐藏证据的路径上不允许存在可配置的旁路（AGENTS.md §9）。
func (r *Repository) PurgeMark(ctx context.Context, batch *model.ArchiveBatch, ts int64) (int64, error) {
	if batch == nil {
		return 0, model.ErrBatchNotFound
	}
	if batch.State != model.BatchStateVerified {
		return 0, model.ErrBatchUnverified
	}
	if batch.ManifestHash == "" {
		return 0, ErrPurgeWithoutVerify
	}
	return r.Entries.MarkArchived(ctx, batch.ChainKey, batch.FromSeq, batch.ToSeq, ts)
}
