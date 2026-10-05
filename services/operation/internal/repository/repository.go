package repository

// 本文件定义 Repository 依赖装配：model + 缓存 + 下游 RPC + 口令与会话策略。
// logic 层只调用 Repository 的导出方法，不直接触碰 sqlx/Redis，
// 这样权限判定、任务状态机与审计脱敏的测试都能在无 MySQL/Redis 的环境里跑（AGENTS.md §9）。

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/operation/model"
)

// 业务规模上限（对外错误为 model.Err*，不依赖框架默认值）。
const (
	// maxPageSize 列表接口单页上限。
	maxPageSize = 100
	// defaultPageSize 未传分页时的默认值。
	defaultPageSize = 20
	// maxTaskSteps 单个管理任务的步骤上限，超出直接拒绝（防止一次 RPC 打爆下游）。
	maxTaskSteps = 1000
	// defaultRunSteps RunAdminTask 单次推进的默认步数。
	defaultRunSteps = 100
	// maxUsernameLen 管理员账号名长度上限。
	maxUsernameLen = 32
	// minUsernameLen 管理员账号名长度下限。
	minUsernameLen = 3
	// maxRemarkLen 备注/变更说明长度上限。
	maxRemarkLen = 255

	// defaultPermissionTTL 权限快照缓存秒数：后台权限变更需在分钟级收敛。
	defaultPermissionTTL = 60
	// defaultConfigTTL 运营配置缓存秒数。
	defaultConfigTTL = 60
	// defaultMenuTTL 菜单树缓存秒数。
	defaultMenuTTL = 300
)

// Options 是缓存 TTL 与执行规模的配置载体（由 etc/Cache、etc/Login 映射而来）。
type Options struct {
	// PermissionTTL 权限判定快照缓存秒数，同时作为网关侧缓存建议值。
	PermissionTTL int
	// ConfigTTL 运营配置读取缓存秒数。
	ConfigTTL int
	// MenuTTL 菜单树缓存秒数。
	MenuTTL int
	// RunSteps RunAdminTask 单次推进的默认步数（<=0 时用 defaultRunSteps）。
	RunSteps int32
	// MaxFail 连续登录失败锁定阈值（<=0 时用 defaultMaxFail）。
	MaxFail int32
	// LockMinutes 锁定时长（<=0 时用 defaultLockMinutes）。
	LockMinutes int64
}

// Repository 聚合 operation 自有表模型、缓存、下游客户端与策略配置。
type Repository struct {
	cache *Cache
	conn  sqlx.SqlConn

	adminMd   model.AdminUserModel
	sessionMd model.AdminSessionModel
	roleMd    model.RoleModel
	permMd    model.PermissionModel
	menuMd    model.MenuModel
	configMd  model.OpsConfigModel
	taskMd    model.AdminTaskModel
	stepMd    model.AdminTaskStepModel
	auditMd   model.AuditIndexModel

	downstream *Downstream

	session SessionConf
	login   LoginConf
	cfg     Options
}

// Models 聚合 Repository 依赖的表模型，供 NewWithDeps 注入。
// 字段留 nil 表示「本用例不该触达这张表」：一旦触达就是空接口调用，直接 panic 炸出来，
// 比默默返回零值更容易发现依赖泄漏（与 account 服务 NewWithDeps 同口径）。
type Models struct {
	AdminUser  model.AdminUserModel
	Session    model.AdminSessionModel
	Role       model.RoleModel
	Permission model.PermissionModel
	Menu       model.MenuModel
	Config     model.OpsConfigModel
	Task       model.AdminTaskModel
	Step       model.AdminTaskStepModel
	Audit      model.AuditIndexModel
}

// New 装配 Repository。
// rds 允许为 nil（开发环境降级为“无缓存直连 DB”，判定结果不受影响）；
// conn 必须可用；downstream 中未配置的客户端在调用点返回 ErrDownstreamUnavailable。
func New(rds *redis.Redis, conn sqlx.SqlConn, downstream *Downstream, sess SessionConf, opts Options) *Repository {
	var st Store
	if rds == nil {
		// 无 Redis 时用空缓存兜底，避免每个调用点判 nil。
		st = noopStore{}
	} else {
		st = &redisStore{rds: rds}
	}
	return NewWithDeps(NewCacheWithStore(st), conn, downstream, Models{
		AdminUser:  model.NewAdminUserModel(conn),
		Session:    model.NewAdminSessionModel(conn),
		Role:       model.NewRoleModel(conn),
		Permission: model.NewPermissionModel(conn),
		Menu:       model.NewMenuModel(conn),
		Config:     model.NewOpsConfigModel(conn),
		Task:       model.NewAdminTaskModel(conn),
		Step:       model.NewAdminTaskStepModel(conn),
		Audit:      model.NewAuditIndexModel(conn),
	}, sess, opts)
}

// NewWithDeps 用显式依赖装配 Repository：缓存、连接、9 张表的 model 与下游客户端全部注入。
// 生产路径经 New 调用它（依赖为 sqlx/redis 实现），logic 层单测传入内存 model 与
// 内存 Store，就能把「口令校验、防爆破锁定、权限判定、审计脱敏、会话吊销」整条链路
// 放进被测路径，而不必把 Repository 一起 mock 掉（AGENTS.md §9）。
func NewWithDeps(cache *Cache, conn sqlx.SqlConn, downstream *Downstream, md Models, sess SessionConf, opts Options) *Repository {
	repo := &Repository{
		cache:      cache,
		conn:       conn,
		adminMd:    md.AdminUser,
		sessionMd:  md.Session,
		roleMd:     md.Role,
		permMd:     md.Permission,
		menuMd:     md.Menu,
		configMd:   md.Config,
		taskMd:     md.Task,
		stepMd:     md.Step,
		auditMd:    md.Audit,
		downstream: downstream,
		session:    sess,
		cfg:        normalizeOptions(opts),
	}
	repo.login = LoginConf{MaxFail: repo.cfg.MaxFail, LockMinutes: repo.cfg.LockMinutes}
	return repo
}

// normalizeOptions 补齐配置缺省值并做下限保护。
func normalizeOptions(o Options) Options {
	if o.PermissionTTL <= 0 {
		o.PermissionTTL = defaultPermissionTTL
	}
	if o.ConfigTTL <= 0 {
		o.ConfigTTL = defaultConfigTTL
	}
	if o.MenuTTL <= 0 {
		o.MenuTTL = defaultMenuTTL
	}
	if o.RunSteps <= 0 {
		o.RunSteps = defaultRunSteps
	}
	if o.RunSteps > maxTaskSteps {
		o.RunSteps = maxTaskSteps
	}
	if o.MaxFail <= 0 {
		o.MaxFail = defaultMaxFail
	}
	if o.LockMinutes <= 0 {
		o.LockMinutes = defaultLockMinutes
	}
	return o
}

// Conn 暴露只读的 sqlx 连接给少数需要事务的入口（当前仅测试与运维脚本使用）。
func (r *Repository) Conn() sqlx.SqlConn { return r.conn }

// ConfigTTL 返回运营配置的建议缓存秒数（已含缺省值），供 logic 回传给调用方。
func (r *Repository) ConfigTTL() int { return r.cfg.ConfigTTL }

// pagePair 归一化分页参数，返回 model 层使用的 (pn, ps)。
func pagePair(pn, ps int32) (int32, int32) {
	if pn <= 0 {
		pn = 1
	}
	if ps <= 0 {
		ps = defaultPageSize
	}
	if ps > maxPageSize {
		ps = maxPageSize
	}
	return pn, ps
}
