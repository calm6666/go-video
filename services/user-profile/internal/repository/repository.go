// Package repository 是 user-profile 服务的数据访问层。
// 组合本地 MySQL 模型、Redis 缓存、实名证件 RSA 加解密与领域事件 Outbox 发布器，
// 为 logic 层提供统一数据访问入口。移植自参考仓库 member 服务的 dao + service 聚合。
package repository

import (
	"context"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/common/fanout"
	"go-video/services/user-profile/internal/config"
	"go-video/services/user-profile/model"
)

// Repository 是 user-profile 服务的数据访问入口。
type Repository struct {
	cache Cacher

	conn sqlx.SqlConn

	baseModel        model.UserBaseModel
	expModel         model.UserExpModel
	flagModel        model.UserFlagModel
	moralModel       model.UserMoralModel
	officialModel    model.UserOfficialModel
	officialDocModel model.OfficialDocModel
	additModel       model.OfficialDocAdditModel
	monitorModel     model.UserMonitorModel
	reviewModel      model.UserPropertyReviewModel
	realnameModel    model.RealnameInfoModel
	applyModel       model.RealnameApplyModel
	applyImgModel    model.RealnameApplyImageModel
	logModel         model.MemberLogModel
	outboxModel      model.MemberOutboxModel

	// cryptor 实名证件 RSA 加解密器。
	cryptor *CardCryptor

	// imgURLTemplate 证件照 CDN URL 模板（如 https://cdn.example.com/idenfiles/%s.txt）。
	imgURLTemplate string

	// officials 已生效官方认证信息的内存快照（参考 loadOfficial）。
	officialMu sync.RWMutex
	officials  map[int64]*model.OfficialInfo

	async asyncRunner

	// outbox 事件发布器。测试装配（NewWithDeps）不启动发布器，故为 nil。
	outbox *OutboxPublisher
}

// asyncRunner 抽象缓存回填的异步执行器。
// 生产用 fanout.Fanout（入队后由 worker 协程执行）；测试装配用同步执行器（Do 内联执行），
// 这样「回源后有没有回填、回填排在第几步」才是可断言的事实，而不是竞态。
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

// Options 是 NewWithDeps 的 non-model 依赖。零值即可用于测试：
// Cryptor 为空串密钥（加解密必然报错，由用例显式注入 PEM），Async 为 nil 时同步执行回填。
type Options struct {
	// IMGURLTemplate 证件照 CDN URL 模板。
	IMGURLTemplate string
	// Cryptor 实名证件加解密器。
	Cryptor *CardCryptor
	// Async 异步回填执行器；nil 表示同步执行（测试口径），生产由 New 注入 fanout。
	Async asyncRunner
}

// New 构造 Repository。生产路径唯一入口，全部依赖为真实实现。
func New(rds *redis.Redis, conn sqlx.SqlConn, c config.Config) *Repository {
	r := NewWithDeps(NewCache(rds), conn,
		model.NewUserBaseModel(conn),
		model.NewUserExpModel(conn),
		model.NewUserFlagModel(conn),
		model.NewUserMoralModel(conn),
		model.NewUserOfficialModel(conn),
		model.NewOfficialDocModel(conn),
		model.NewOfficialDocAdditModel(conn),
		model.NewUserMonitorModel(conn),
		model.NewUserPropertyReviewModel(conn),
		model.NewRealnameInfoModel(conn),
		model.NewRealnameApplyModel(conn),
		model.NewRealnameApplyImageModel(conn),
		model.NewMemberLogModel(conn),
		model.NewMemberOutboxModel(conn),
		Options{
			IMGURLTemplate: c.Realname.IMGURLTemplate,
			Cryptor:        NewCardCryptor(c.Realname.PublicKey, c.Realname.PrivateKey),
			Async:          fanout.New("userProfileRepository", fanout.Worker(1), fanout.Buffer(10240)),
		})
	r.outbox = NewOutboxPublisher(r.outboxModel, c.Outbox, NewAccountCacheClient(c.AccountRPC))
	go r.loadOfficialProc()
	r.outbox.Start()
	return r
}

// NewWithDeps 是注入缝：显式给出缓存、事务连接与 14 个 model，供 internal/logic 的单测
// 用内存依赖组装**真实 Repository**（见 cache.go 的 Cacher 注释）。
//
// 与 New 的差别只有三处，且不改变任何查询语义：
//   - 不创建 OutboxPublisher、不启动发布协程（读侧用例不该看到后台投递）；
//   - Async 未给出时用同步执行器，使缓存回填进入可断言的调用序列；
//   - 官方认证快照在构造时**同步**装载一次（New 是由后台协程立即装载第一次，结论一致）。
//
// 生产代码不得调用本函数，一律走 New。
func NewWithDeps(
	cache Cacher,
	conn sqlx.SqlConn,
	baseMd model.UserBaseModel,
	expMd model.UserExpModel,
	flagMd model.UserFlagModel,
	moralMd model.UserMoralModel,
	officialMd model.UserOfficialModel,
	officialDocMd model.OfficialDocModel,
	additMd model.OfficialDocAdditModel,
	monitorMd model.UserMonitorModel,
	reviewMd model.UserPropertyReviewModel,
	realnameMd model.RealnameInfoModel,
	applyMd model.RealnameApplyModel,
	applyImgMd model.RealnameApplyImageModel,
	logMd model.MemberLogModel,
	outboxMd model.MemberOutboxModel,
	opt Options,
) *Repository {
	async := opt.Async
	if async == nil {
		async = syncRunner{}
	}
	r := &Repository{
		cache:            cache,
		conn:             conn,
		baseModel:        baseMd,
		expModel:         expMd,
		flagModel:        flagMd,
		moralModel:       moralMd,
		officialModel:    officialMd,
		officialDocModel: officialDocMd,
		additModel:       additMd,
		monitorModel:     monitorMd,
		reviewModel:      reviewMd,
		realnameModel:    realnameMd,
		applyModel:       applyMd,
		applyImgModel:    applyImgMd,
		logModel:         logMd,
		outboxModel:      outboxMd,
		cryptor:          opt.Cryptor,
		imgURLTemplate:   opt.IMGURLTemplate,
		officials:        map[int64]*model.OfficialInfo{},
		async:            async,
	}
	if err := r.loadOfficial(); err != nil {
		logx.Errorf("user-profile: load official on init err=%v", err)
	}
	return r
}

// Ping 检查底层 Redis 连通性，供健康检查使用。
func (r *Repository) Ping(ctx context.Context) error {
	return r.cache.Ping(ctx)
}

// Close 关闭 Outbox 发布器与异步任务执行器。
func (r *Repository) Close() error {
	if r.outbox != nil {
		r.outbox.Close()
	}
	return r.async.Close()
}

// Conn 返回底层 SqlConn，供需要事务的业务（节操变更、实名申请等）使用。
func (r *Repository) Conn() sqlx.SqlConn {
	return r.conn
}

// loadOfficial 全量加载生效官方认证信息到内存（参考 service.loadOfficial）。
// 加载完成后对比旧快照，对发生变化的 mid 通知 account 失效缓存。
func (r *Repository) loadOfficial() error {
	ctx := context.Background()
	om, err := r.officialModel.All(ctx)
	if err != nil {
		return err
	}

	r.officialMu.Lock()
	origin := r.officials
	r.officials = om
	r.officialMu.Unlock()

	if origin == nil {
		return nil
	}
	// 变化检测：新增/变更/删除的 mid 都触发缓存失效通知（参考 loadOfficial 的 NotifyPurgeCache）
	for mid, of := range om {
		if oof := origin[mid]; oof == nil || !of.Equal(oof) {
			logx.Infof("user-profile: official changed, notify purge cache mid=%d", mid)
			r.enqueueProfileUpdated(ctx, mid, ActUpdateByAdmin)
		}
	}
	for mid := range origin {
		if om[mid] == nil {
			logx.Infof("user-profile: official removed, notify purge cache mid=%d", mid)
			r.enqueueProfileUpdated(ctx, mid, ActUpdateByAdmin)
		}
	}
	return nil
}

// loadOfficialProc 每 5 分钟刷新一次官方认证内存快照（失败 60 秒后重试）。
func (r *Repository) loadOfficialProc() {
	for {
		if err := r.loadOfficial(); err != nil {
			logx.Errorf("user-profile: load official err=%v", err)
			time.Sleep(60 * time.Second)
			continue
		}
		time.Sleep(5 * time.Minute)
	}
}

// Officials 返回当前内存中的官方认证快照（只读）。
func (r *Repository) Officials() map[int64]*model.OfficialInfo {
	r.officialMu.RLock()
	defer r.officialMu.RUnlock()
	return r.officials
}
