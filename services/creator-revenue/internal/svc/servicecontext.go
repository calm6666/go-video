// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"context"

	"go-video/services/creator-revenue/internal/config"
	"go-video/services/creator-revenue/model"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// Page 是折好界限的分页参数：调用方给什么值不重要，落库查询只看这里。
type Page struct {
	// RequestedPage / RequestedSize 是回显给调用方的实际生效页码与页宽。
	RequestedPage int64
	RequestedSize int64
	// Offset / Limit 是查询用的物理分页位。
	Offset int64
	Limit  int64
}

// NewPage 把入参 page/size 折成有界分页参数。
func NewPage(page, size int64, maxSize int64) Page {
	if maxSize <= 0 {
		maxSize = 100
	}
	if page < 1 {
		page = 1
	}
	if size <= 0 || size > maxSize {
		size = maxSize
	}
	return Page{RequestedPage: page, RequestedSize: size, Offset: (page - 1) * size, Limit: size}
}

// ServiceContext 是 creator-revenue 的运行时上下文：MySQL 连接、缓存与各表 model。
//
// 装配原则（AGENTS.md §5）：
//   - 本服务只拥有 go_video_creator_revenue 库的 7 张 cr_* 表；计量原始事实
//     （会员有效观看、投币、互动）归 spm / coin / membership，本服务只存折算后台账，
//     因此**没有任何下游 RPC 依赖**，也不 import 其它服务的 internal/model；
//   - 跨表写（规则 + 变更台账、计量更正 + 留痕、作废 + 新单 + 分项）一律走 Transact，
//     SQL 仍只在 model 里；logic 需要事务会话时用
//     `model.NewXxxModel(sqlx.NewSqlConnFromSession(tx))` 绑定；
//   - CacheRedis 只是读路径加速位，可为 nil；台账真值恒在 MySQL，
//     任何「是否出单」「金额多少」的判定都不允许依赖缓存结果。
type ServiceContext struct {
	Config config.Config

	// Conn 本服务库连接；logic 只用它开事务（Transact），不拼 SQL。
	Conn sqlx.SqlConn

	// Cache 只读加速层；nil 表示未配置，调用方必须回源 MySQL。
	Cache *redis.Redis

	// Rules 分成规则（rule_code 唯一 + version 乐观锁）。
	Rules model.RevenueRuleModel
	// RuleChanges 规则变更台账（单价/状态历史值的唯一复核来源）。
	RuleChanges model.RuleChangeLogModel
	// Enrollments 参与关系（一人一行）。
	Enrollments model.EnrollmentModel
	// Metrics 计量台账（(period,mid,aid,source_type) 唯一）。
	Metrics model.RevenueMetricModel
	// MetricChanges 计量更正留痕。
	MetricChanges model.MetricChangeLogModel
	// Settlements 结算单（(period,mid) 在效唯一）。
	Settlements model.SettlementModel
	// SettlementItems 结算分项。
	SettlementItems model.SettlementItemModel
}

// NewServiceContext 构造 ServiceContext。
//
// DataSource 为空时不 panic：读接口会以 ErrDBNotConfigured 显式失败，
// 而不是把「查不到」折叠成空台账冒充成功（AGENTS.md §9 禁止伪成功）。
func NewServiceContext(c config.Config) *ServiceContext {
	ctx := &ServiceContext{Config: c}
	if c.DataSource != "" {
		conn := sqlx.NewMysql(c.DataSource)
		ctx.Conn = conn
		ctx.Rules = model.NewRevenueRuleModel(conn)
		ctx.RuleChanges = model.NewRuleChangeLogModel(conn)
		ctx.Enrollments = model.NewEnrollmentModel(conn)
		ctx.Metrics = model.NewRevenueMetricModel(conn)
		ctx.MetricChanges = model.NewMetricChangeLogModel(conn)
		ctx.Settlements = model.NewSettlementModel(conn)
		ctx.SettlementItems = model.NewSettlementItemModel(conn)
	}
	if c.CacheRedis.Host != "" {
		ctx.Cache = redis.MustNewRedis(c.CacheRedis)
	}
	return ctx
}

// Transact 在 go_video_creator_revenue 库上执行一个事务。
//
// 传出的 session 只能交给 `model.NewXxxModel(sqlx.NewSqlConnFromSession(tx))` 使用；
// 未配置 DataSource 时直接回 ErrDBNotConfigured，绝不「跳过写库回成功」。
func (s *ServiceContext) Transact(
	ctx context.Context, fn func(ctx context.Context, tx sqlx.Session) error,
) error {
	if s.Conn == nil {
		return model.ErrDBNotConfigured
	}
	return s.Conn.TransactCtx(ctx, fn)
}

// Ready 判定数据访问是否可用（DataSource 未配置时各 model 为 nil）。
//
// logic 每个入口先调它：配置缺失必须回一个可诊断的错误，
// 而不是 nil pointer panic，更不是「查不到 = 空台账」的伪成功（AGENTS.md §9）。
func (s *ServiceContext) Ready() error {
	if s.Conn == nil || s.Rules == nil || s.Enrollments == nil || s.Metrics == nil || s.Settlements == nil {
		return model.ErrDBNotConfigured
	}
	return nil
}

// PageSize 把调用方给的 page/size 折成有界分页参数（见 NewPage）。
func (s *ServiceContext) PageSize(page, size int64) Page {
	return NewPage(page, size, s.Config.CreatorRevenue.MaxPageSize)
}
