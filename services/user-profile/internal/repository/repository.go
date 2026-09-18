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
	cache *Cache

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

	async *fanout.Fanout

	// outbox 事件发布器。
	outbox *OutboxPublisher
}

// New 构造 Repository。
func New(rds *redis.Redis, conn sqlx.SqlConn, c config.Config) *Repository {
	r := &Repository{
		cache:            NewCache(rds),
		conn:             conn,
		baseModel:        model.NewUserBaseModel(conn),
		expModel:         model.NewUserExpModel(conn),
		flagModel:        model.NewUserFlagModel(conn),
		moralModel:       model.NewUserMoralModel(conn),
		officialModel:    model.NewUserOfficialModel(conn),
		officialDocModel: model.NewOfficialDocModel(conn),
		additModel:       model.NewOfficialDocAdditModel(conn),
		monitorModel:     model.NewUserMonitorModel(conn),
		reviewModel:      model.NewUserPropertyReviewModel(conn),
		realnameModel:    model.NewRealnameInfoModel(conn),
		applyModel:       model.NewRealnameApplyModel(conn),
		applyImgModel:    model.NewRealnameApplyImageModel(conn),
		logModel:         model.NewMemberLogModel(conn),
		outboxModel:      model.NewMemberOutboxModel(conn),
		cryptor:          NewCardCryptor(c.Realname.PublicKey, c.Realname.PrivateKey),
		imgURLTemplate:   c.Realname.IMGURLTemplate,
		officials:        map[int64]*model.OfficialInfo{},
		async:            fanout.New("userProfileRepository", fanout.Worker(1), fanout.Buffer(10240)),
	}
	r.outbox = NewOutboxPublisher(r.outboxModel, c.Outbox, NewAccountCacheClient(c.AccountRPC))
	go r.loadOfficialProc()
	r.outbox.Start()
	return r
}

// Ping 检查底层 Redis 连通性，供健康检查使用。
func (r *Repository) Ping(ctx context.Context) error {
	return r.cache.Ping(ctx)
}

// Close 关闭 Outbox 发布器与异步任务执行器。
func (r *Repository) Close() error {
	r.outbox.Close()
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
