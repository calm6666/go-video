// Package repository 是 account 服务的数据访问层。
// 它组合本地 MySQL 模型、Redis 缓存和下游服务 RPC，为 logic 层提供统一的数据访问入口。
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/common/fanout"
	"go-video/services/account/internal/config"
	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

// ErrNotImplemented 表示下游服务尚未接入或对应业务能力未落地。
// 调用方应捕获该错误并降级返回零值字段。
var ErrNotImplemented = errors.New("account/repository: not implemented")

// UserProfileClient 抽象 user-profile 服务的 RPC client。
// account 服务只读不写 user-profile 的数据（AddExp/AddMoral 委托调用除外）。
// 当 ServiceContext 未注入实现时，repository 降级返回零值字段，不阻塞主流程。
type UserProfileClient interface {
	// Base 查询用户基础资料（昵称、头像、签名、性别、排名）。
	Base(ctx context.Context, mid int64) (*UserProfileBase, error)
	// Bases 批量查询用户基础资料。
	Bases(ctx context.Context, mids []int64) (map[int64]*UserProfileBase, error)
	// Member 查询用户完整资料（含等级、生日、官方认证等）。
	Member(ctx context.Context, mid int64) (*UserProfileMember, error)
	// Members 批量查询用户完整资料。
	Members(ctx context.Context, mids []int64) (map[int64]*UserProfileMember, error)
	// LevelExp 查询等级经验信息。
	LevelExp(ctx context.Context, mid int64) (*rpc.LevelInfo, error)
	// RealnameStatus 查询实名认证状态：0 未认证、1 已认证。
	RealnameStatus(ctx context.Context, mid int64) (int32, error)
	// AddExp 增加经验值，委托 user-profile 持久化。
	AddExp(ctx context.Context, mid int64, exp float64, operater, operate, reason string) error
	// AddMoral 增加道德值，委托 user-profile 持久化。
	AddMoral(ctx context.Context, mid int64, moral float64, oper, reason, remark string) error
}

// UserProfileBase 对应 user-profile 服务暴露的基础资料字段。
// account 聚合时只关心这些字段，避免依赖 user-profile 的内部 model。
type UserProfileBase struct {
	Mid  int64
	Name string
	Sex  string
	Face string
	Sign string
	Rank int32
}

// UserProfileMember 对应 user-profile 服务暴露的完整资料字段。
type UserProfileMember struct {
	UserProfileBase
	Level    int32
	Birthday int64
	Moral    int32
	Official *rpc.OfficialInfo
}

// SocialGraphClient 抽象 social-graph 服务的 RPC client。
type SocialGraphClient interface {
	// Relation 查询 mid 是否关注 owner。
	Relation(ctx context.Context, mid, owner int64) (bool, error)
	// Relations 批量查询 mid 与 owners 的关注关系。
	Relations(ctx context.Context, mid int64, owners []int64) (map[int64]bool, error)
	// Stat 查询关注数和粉丝数。
	Stat(ctx context.Context, mid int64) (*RelationStat, error)
	// Attentions 查询关注列表（含特别关注）。
	Attentions(ctx context.Context, mid int64) ([]int64, error)
	// Blacks 查询黑名单 mid 集合。
	Blacks(ctx context.Context, mid int64) (map[int64]bool, error)
	// RichRelations 查询 owner 与 mids 的关系属性值。
	RichRelations(ctx context.Context, owner int64, mids []int64) (map[int64]int32, error)
}

// RelationStat 是 social-graph 返回的统计字段。
type RelationStat struct {
	Following int64
	Follower  int64
}

// Repository 是 account 服务的数据访问入口，组合本地模型、缓存和下游 RPC。
type Repository struct {
	cache Cacher
	conn  sqlx.SqlConn
	// 下面 6 个 model 与 2 个下游 client 都是接口，生产由 New 注入真实实现，
	// 测试由 NewWithDeps 注入内存替身（见 internal/logic/fakes_test.go）。
	accountModel    model.AccountModel
	credentialModel model.AccountCredentialModel
	secretModel     model.AccountSecretModel
	sessionModel    model.AccountSessionModel
	loginLogModel   model.AccountLoginLogModel
	captureLogModel model.AccountCaptureLogModel
	userProfile     UserProfileClient
	socialGraph     SocialGraphClient
	async           asyncRunner
	// delayQ 缓存延迟失效队列（updateVip 二次失效，见 cache_delay.go）。
	delayQ delaySink
	// delayStop 通知缓存延迟消费协程退出。
	delayStop chan struct{}
	// passportRSA 登录密码 RSA 加解密器（未配置密钥时为 nil，按明文密码处理）。
	passportRSA *PassportRSA
	// tokenTTLDays / refreshTTLDays 会话有效期（天）。
	tokenTTLDays   int64
	refreshTTLDays int64
}

// asyncRunner 抽象缓存回填的异步执行器（与 user-profile 同口径）。
// 生产用 fanout.Fanout（入队后由 worker 协程执行）；测试装配用同步执行器（Do 内联执行），
// 这样「写路径第几步回填了哪个 key」才是可断言的事实，而不是竞态。
type asyncRunner interface {
	Do(ctx context.Context, f func(ctx context.Context)) error
	Close() error
}

// syncRunner 是 NewWithDeps 使用的同步执行器：Do 立即内联执行任务。
type syncRunner struct{}

func (syncRunner) Do(ctx context.Context, f func(ctx context.Context)) error {
	f(ctx)
	return nil
}

func (syncRunner) Close() error { return nil }

// delaySink 抽象缓存延迟二次失效队列。*delayQueue 本身就是纯内存、无后台协程的结构，
// 抽成接口只是为了让测试装配能替换它（确定驱动见 Repository.DrainDelayedCache）。
type delaySink interface {
	Put(mid int64, at time.Time)
	PopExpired(now time.Time) []int64
	Close()
}

// Options 是 NewWithDeps 的 non-model 依赖。零值即可用于测试：
// PassportRSA 为 nil 时按明文密码处理（与未配置密钥的生产环境一致），
// Async 为 nil 时同步执行回填，TokenTTLDays/RefreshTTLDays 沿用 New 的默认规则。
type Options struct {
	// PassportRSA 登录密码加解密器；nil 表示未配置密钥（开发模式，密码按明文处理）。
	PassportRSA *PassportRSA
	// TokenTTLDays access token 有效期（天），<=0 时取 TokenTTLDays 常量。
	TokenTTLDays int64
	// RefreshTTLDays refresh token 有效期（天），<=0 时取 RefreshTTLDays 常量。
	RefreshTTLDays int64
	// Async 异步回填执行器；nil 表示同步执行（测试口径），生产由 New 注入 fanout。
	Async asyncRunner
	// DelayQ 延迟二次失效队列；nil 表示用真实内存队列 newDelayQueue()。
	DelayQ delaySink
}

// New 构造 Repository。生产路径唯一入口，全部依赖为真实实现。
// userProfile 和 socialGraph 可为 nil，表示对应服务尚未接入，
// repository 会降级返回零值字段。
func New(rds *redis.Redis, conn sqlx.SqlConn, c config.Config, userProfile UserProfileClient, socialGraph SocialGraphClient) *Repository {
	r := NewWithDeps(NewCache(rds), conn,
		model.NewAccountModel(conn),
		model.NewAccountCredentialModel(conn),
		model.NewAccountSecretModel(conn),
		model.NewAccountSessionModel(conn),
		model.NewAccountLoginLogModel(conn),
		model.NewAccountCaptureLogModel(conn),
		userProfile, socialGraph,
		Options{
			PassportRSA:    NewPassportRSA(c.PassportRSA.PublicKey, c.PassportRSA.PrivateKey),
			TokenTTLDays:   c.TokenTTLDays,
			RefreshTTLDays: c.RefreshTTLDays,
			Async:          fanout.New("accountRepository", fanout.Worker(1), fanout.Buffer(1024)),
		})
	go r.cacheDelayProc()
	return r
}

// NewWithDeps 是注入缝：显式给出缓存、事务连接、6 个 model 与 2 个下游 client，
// 供 internal/logic 的单测用内存依赖组装**真实 Repository**（见 cache.go 的 Cacher 注释）。
//
// 与 New 的差别只有两处，且不改变任何查询与判定语义：
//   - 不启动 cacheDelayProc 后台协程（否则用例只能用 sleep 等 5 秒竞态；
//     需要驱动延迟二次失效时改调 DrainDelayedCache，走的是同一段代码）；
//   - Async 未给出时用同步执行器，使缓存回填进入可断言的调用序列
//     （生产是入队异步执行，时序不同但内容一致）。
//
// 生产代码不得调用本函数，一律走 New。
func NewWithDeps(
	cache Cacher,
	conn sqlx.SqlConn,
	accountMd model.AccountModel,
	credentialMd model.AccountCredentialModel,
	secretMd model.AccountSecretModel,
	sessionMd model.AccountSessionModel,
	loginLogMd model.AccountLoginLogModel,
	captureLogMd model.AccountCaptureLogModel,
	userProfile UserProfileClient,
	socialGraph SocialGraphClient,
	opt Options,
) *Repository {
	tokenTTL := opt.TokenTTLDays
	if tokenTTL <= 0 {
		tokenTTL = TokenTTLDays
	}
	refreshTTL := opt.RefreshTTLDays
	if refreshTTL <= 0 {
		refreshTTL = RefreshTTLDays
	}
	async := opt.Async
	if async == nil {
		async = syncRunner{}
	}
	delayQ := opt.DelayQ
	if delayQ == nil {
		delayQ = newDelayQueue()
	}
	return &Repository{
		cache:           cache,
		conn:            conn,
		accountModel:    accountMd,
		credentialModel: credentialMd,
		secretModel:     secretMd,
		sessionModel:    sessionMd,
		loginLogModel:   loginLogMd,
		captureLogModel: captureLogMd,
		userProfile:     userProfile,
		socialGraph:     socialGraph,
		async:           async,
		delayQ:          delayQ,
		delayStop:       make(chan struct{}),
		passportRSA:     opt.PassportRSA,
		tokenTTLDays:    tokenTTL,
		refreshTTLDays:  refreshTTL,
	}
}

// Ping 检查底层 Redis 连通性，供健康检查使用。
// MySQL 和下游 RPC 的连通性不在此检查，避免单一依赖故障导致健康检查失败。
func (r *Repository) Ping(ctx context.Context) error {
	return r.cache.Ping(ctx)
}

// Close 停止缓存延迟消费协程，关闭延迟队列，并关闭异步任务执行器。
func (r *Repository) Close() error {
	close(r.delayStop)
	r.delayQ.Close()
	return r.async.Close()
}

// Account 返回本地 account 表的查询接口，供 logic 直接调用。
func (r *Repository) Account() model.AccountModel {
	return r.accountModel
}

// Credentials 返回本地 account_credential 表的查询接口。
func (r *Repository) Credentials() model.AccountCredentialModel {
	return r.credentialModel
}
