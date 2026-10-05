// Package repository 是 recommend-recall 的数据访问与下游依赖装配层。
//
// 职责边界（AGENTS.md §4/§5）：
//   - 组合本服务自有表的 model（recall_pool / recall_pool_version / recall_pool_current /
//     recall_request_log / recall_outbox / recall_idempotency）与缓存；
//   - 对外声明召回需要但**契约还没落地**的下游读接口（特征、行为、可见性、关系），
//     并给出显式 stub —— 本文件所在包里没有任何"假装能取到特征"的实现；
//   - 不做用例编排：状态机推进、降级判定、去重合并都在 internal/logic 里做。
package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/common/idempotency"
	"go-video/services/recommend-recall/model"
)

// ErrSourceNotConfigured 表示某个下游数据源尚未接线。
//
// 它与 model.ErrNotImplemented 的分工：后者说"本服务的业务能力没写"，
// 前者说"能力写了但依赖没接上"。logic 捕获它并映射为
// rpc.DEGRADE_REASON_FEATURE_UNAVAILABLE，绝不返回空特征冒充"该用户没有兴趣"。
var ErrSourceNotConfigured = errors.New("recommend-recall: feature/visibility source not configured")

// Options 是 repository 的运行参数，全部来自 etc yaml（config.Recall），不在代码里写死。
//
// New 会逐个校验并拒绝超出 model 硬上限的配置值：配置写错必须在启动期暴露，
// 而不是等在线召回把 limit=0 当成"空池"。
type Options struct {
	// MaxCandidates 单次召回总候选硬上限（对应 rpc max_candidates）。
	MaxCandidates int
	// DefaultLimit 未指定 limit 时的默认条数。
	DefaultLimit int
	// PerSourceMax 单路最大条数。
	PerSourceMax int
	// MaxSeedAids 种子稿件条数上限。
	MaxSeedAids int
	// MaxSeedTags 种子标签条数上限。
	MaxSeedTags int
	// MaxExcludeAids 排除 aid 条数上限。
	MaxExcludeAids int
	// MaxBatchItems 单次池条目写入条数上限。
	MaxBatchItems int
	// MaxVersionList 版本列取上限。
	MaxVersionList int
	// MaxPoolSnapshotPage 池快照分页大小上限。
	MaxPoolSnapshotPage int
	// MaxRequestLogPage 召回日志分页大小上限。
	MaxRequestLogPage int
	// MinKeepVersions 清理时每个池至少保留的版本数。
	MinKeepVersions int
	// PoolStaleSeconds 池超过该秒数未更新即标记 stale。
	PoolStaleSeconds int64
	// IdempotencyLeaseSeconds 幂等键 PENDING 租约秒数。
	IdempotencyLeaseSeconds int64
	// IdempotencyRetentionSeconds 幂等记录保留秒数。
	IdempotencyRetentionSeconds int64
}

// Repository 聚合自有表模型、缓存与下游数据源。
type Repository struct {
	conn sqlx.SqlConn
	opts Options
	// Cache 是业务缓存（池快照与版本指针）。允许为 nil：logic 按 store_unavailable 降级，
	// 不回源打爆 MySQL。
	Cache *redis.Redis

	Pool        model.RecallPoolModel
	PoolVersion model.RecallPoolVersionModel
	Current     model.RecallPoolCurrentModel
	RequestLog  model.RecallRequestLogModel
	Outbox      model.RecallOutboxModel
	Idempotency model.RecallIdempotencyModel

	// Features 提供行为/兴趣/热度特征读取（未接线时返回 ErrSourceNotConfigured）。
	Features FeatureSource
	// Visibility 提供稿件可见性与关系读取（未接线时返回 ErrSourceNotConfigured）。
	Visibility VisibilitySource
}

// New 构造 Repository。
//
// conn 为 nil 会直接 panic：本服务所有方法都依赖 MySQL，
// 没有"没有库也能起"的降级路径，启动期失败比在线每个请求都报 store_unavailable 更好。
func New(conn sqlx.SqlConn, cache *redis.Redis, opts Options) (*Repository, error) {
	if conn == nil {
		return nil, errors.New("recommend-recall: repository requires a MySQL connection")
	}
	if err := opts.validate(); err != nil {
		return nil, err
	}
	return &Repository{
		conn:        conn,
		Cache:       cache,
		opts:        opts,
		Pool:        model.NewRecallPoolModel(conn),
		PoolVersion: model.NewRecallPoolVersionModel(conn),
		Current:     model.NewRecallPoolCurrentModel(conn),
		RequestLog:  model.NewRecallRequestLogModel(conn),
		Outbox:      model.NewRecallOutboxModel(conn),
		Idempotency: model.NewRecallIdempotencyModel(conn),
		// 下游数据源在本轮保持显式 stub（见 featuresource.go）。
		Features:   UnconfiguredFeatureSource{},
		Visibility: UnconfiguredVisibilitySource{},
	}, nil
}

// Deps 是 Repository 的逐槽依赖集合，配 NewWithDeps 使用。
//
// 为什么需要它而不是让测试去改 New：Repository 的 conn/opts 是包外不可见的私有字段，
// logic 包无法在构造后替换任何 model；把六张表的 model 与两个下游数据源做成显式入参，
// 测试才能装进内存替身，生产路径仍然只走 New。
//
// 留空的槽位由 NewWithDeps 按 New 的同一口径补齐（model 走 conn，下游走显式 stub），
// 所以「只替换某一层」的装配不会漏掉别的层而得到一个生产里不存在的形状。
type Deps struct {
	// Conn 是 MySQL 连接：Transact 与所有未显式提供的 model 都依赖它，必填。
	Conn sqlx.SqlConn
	// Cache 业务缓存，允许 nil（logic 按 store_unavailable 降级）。
	Cache *redis.Redis

	Pool        model.RecallPoolModel
	PoolVersion model.RecallPoolVersionModel
	Current     model.RecallPoolCurrentModel
	RequestLog  model.RecallRequestLogModel
	Outbox      model.RecallOutboxModel
	Idempotency model.RecallIdempotencyModel

	Features   FeatureSource
	Visibility VisibilitySource
}

// NewWithDeps 用显式依赖装配 Repository：接线自定义实现（如真实下游 rpc 适配）与测试注入。
//
// 它与 New 的差别只是「model/下游由调用方给出」，配置校验、nil conn 拒绝、
// 私有 conn 字段（Transact 唯一入口）三件事逐字相同；New 不经过它，
// 因此生产构造路径的行为不受影响。
func NewWithDeps(opts Options, d Deps) (*Repository, error) {
	if d.Conn == nil {
		return nil, errors.New("recommend-recall: repository requires a MySQL connection")
	}
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if d.Pool == nil {
		d.Pool = model.NewRecallPoolModel(d.Conn)
	}
	if d.PoolVersion == nil {
		d.PoolVersion = model.NewRecallPoolVersionModel(d.Conn)
	}
	if d.Current == nil {
		d.Current = model.NewRecallPoolCurrentModel(d.Conn)
	}
	if d.RequestLog == nil {
		d.RequestLog = model.NewRecallRequestLogModel(d.Conn)
	}
	if d.Outbox == nil {
		d.Outbox = model.NewRecallOutboxModel(d.Conn)
	}
	if d.Idempotency == nil {
		d.Idempotency = model.NewRecallIdempotencyModel(d.Conn)
	}
	if d.Features == nil {
		d.Features = UnconfiguredFeatureSource{}
	}
	if d.Visibility == nil {
		d.Visibility = UnconfiguredVisibilitySource{}
	}
	return &Repository{
		conn:        d.Conn,
		Cache:       d.Cache,
		opts:        opts,
		Pool:        d.Pool,
		PoolVersion: d.PoolVersion,
		Current:     d.Current,
		RequestLog:  d.RequestLog,
		Outbox:      d.Outbox,
		Idempotency: d.Idempotency,
		Features:    d.Features,
		Visibility:  d.Visibility,
	}, nil
}

// Options 返回生效的运行参数（logic 与 GetRecallConfig 共用同一份，避免两处上限漂移）。
func (r *Repository) Options() Options { return r.opts }

// Transact 在同一个 MySQL 事务内执行 fn。
//
// 发布/回滚必须经它：版本状态推进（recall_pool_version）、指针切换（recall_pool_current）、
// 事件登记（recall_outbox）与幂等标记（recall_idempotency）要一起提交或一起回滚。
func (r *Repository) Transact(ctx context.Context, fn func(session sqlx.Session) error) error {
	if fn == nil {
		return errors.New("recommend-recall: Transact requires a callback")
	}
	return r.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		return fn(session)
	})
}

// RequestFingerprint 计算写请求的幂等指纹（sha256 hex，定长 model.RequestHashLen）。
//
// 只取影响语义的字段：池、版本、批次、目标版本、条目数与条数和。
// 条目原文不进指纹也不落库（幂等表是控制位，不是候选备份）。
func RequestFingerprint(parts ...string) string {
	key := idempotency.NewKey(parts...)
	// idempotency.Key.String() 已是 sha256 hex；这里再校验一次长度，
	// 防止 common 层实现变更导致指纹列宽度不匹配。
	sum := key.String()
	if len(sum) != model.RequestHashLen {
		// 退化路径：用标准库自己算，保证列宽不变量不依赖上游实现细节。
		h := sha256.Sum256([]byte(strings.Join(parts, "|")))
		sum = hex.EncodeToString(h[:])
	}
	return sum
}

// validate 校验配置值落在模型硬上限内。
func (o Options) validate() error {
	rules := []struct {
		name string
		val  int
		max  int
	}{
		{name: "MaxCandidates", val: o.MaxCandidates, max: model.MaxPoolQueryLimit},
		{name: "DefaultLimit", val: o.DefaultLimit, max: model.MaxPoolQueryLimit},
		{name: "PerSourceMax", val: o.PerSourceMax, max: model.MaxPoolQueryLimit},
		{name: "MaxBatchItems", val: o.MaxBatchItems, max: model.MaxPoolItemBatch},
		{name: "MaxVersionList", val: o.MaxVersionList, max: model.MaxVersionListLimit},
		{name: "MaxPoolSnapshotPage", val: o.MaxPoolSnapshotPage, max: model.MaxPoolQueryLimit},
		{name: "MaxRequestLogPage", val: o.MaxRequestLogPage, max: model.MaxRequestLogPageSize},
	}
	for _, rule := range rules {
		if rule.val <= 0 {
			return fmt.Errorf("recommend-recall: config %s must be positive, got %d", rule.name, rule.val)
		}
		if rule.val > rule.max {
			return fmt.Errorf("recommend-recall: config %s=%d exceeds model cap %d", rule.name, rule.val, rule.max)
		}
	}
	if o.MinKeepVersions < 1 {
		return fmt.Errorf("recommend-recall: config MinKeepVersions must be >= 1, got %d", o.MinKeepVersions)
	}
	for name, val := range map[string]int{
		"MaxSeedAids": o.MaxSeedAids, "MaxSeedTags": o.MaxSeedTags, "MaxExcludeAids": o.MaxExcludeAids,
	} {
		if val <= 0 {
			return fmt.Errorf("recommend-recall: config %s must be positive, got %d", name, val)
		}
	}
	if o.PoolStaleSeconds <= 0 {
		return fmt.Errorf("recommend-recall: config PoolStaleSeconds must be positive, got %d", o.PoolStaleSeconds)
	}
	if o.IdempotencyLeaseSeconds <= 0 || o.IdempotencyRetentionSeconds <= o.IdempotencyLeaseSeconds {
		return fmt.Errorf("recommend-recall: idempotency retention(%d) must exceed lease(%d)",
			o.IdempotencyRetentionSeconds, o.IdempotencyLeaseSeconds)
	}
	return nil
}
