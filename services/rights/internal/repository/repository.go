// Package repository 是 rights 服务的数据访问层。
// 组合 rights_contract 与 rights_window 两个 model，
// 为 logic 层提供合同/窗口的 DB + Redis 缓存访问入口。
// CheckPlayable 优先查 Redis 短缓存，miss 时查 DB；
// ExpireWindow 在更新窗口状态后强制失效对应 (content_id, content_type, region) 缓存。
package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/rights/model"
)

const (
	defaultCheckCacheTTL = 120

	prefixCheckPlayable = "rights:chk:%d:%d:%s" // content_id:content_type:region → CheckPlayable 缓存
)

func keyCheckPlayable(contentID int64, contentType int32, region string) string {
	return fmt.Sprintf(prefixCheckPlayable, contentID, contentType, region)
}

// Cache 封装 rights 的 Redis 缓存操作。
type Cache struct {
	rds *redis.Redis
	ttl int // CheckPlayable 缓存 TTL（秒）
}

// NewCache 构造 Cache。ttl <= 0 时使用默认值。
func NewCache(rds *redis.Redis, ttl int) *Cache {
	if ttl <= 0 {
		ttl = defaultCheckCacheTTL
	}
	return &Cache{rds: rds, ttl: ttl}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds.Ping() {
		return nil
	}
	return errors.New("rights/cache: redis ping failed")
}

// GetCheckPlayable 读取 CheckPlayable 缓存。
// 返回 (playable, windowID, endTime, hit, err)：hit=false 表示缓存 miss。
func (c *Cache) GetCheckPlayable(ctx context.Context, contentID int64, contentType int32, region string) (bool, int64, int64, bool, error) {
	raw, err := c.rds.GetCtx(ctx, keyCheckPlayable(contentID, contentType, region))
	if err != nil {
		if err == redis.Nil {
			return false, 0, 0, false, nil
		}
		return false, 0, 0, false, err
	}
	playable, wid, et, ok, perr := parseCheckCache(raw)
	if perr != nil {
		return false, 0, 0, false, perr
	}
	return playable, wid, et, ok, nil
}

// SetCheckPlayable 写入 CheckPlayable 缓存。
func (c *Cache) SetCheckPlayable(ctx context.Context, contentID int64, contentType int32, region string, playable bool, windowID, endTime int64) error {
	return c.rds.SetexCtx(ctx, keyCheckPlayable(contentID, contentType, region), formatCheckCache(playable, windowID, endTime), c.ttl)
}

// DelCheckPlayable 删除 CheckPlayable 缓存（窗口过期/撤权时强制失效）。
func (c *Cache) DelCheckPlayable(ctx context.Context, contentID int64, contentType int32, region string) error {
	_, err := c.rds.DelCtx(ctx, keyCheckPlayable(contentID, contentType, region))
	return err
}

// Cacher 是 Repository 对缓存的依赖面。生产实现是上面的 *Cache（Redis），
// logic/repository 的单元测试注入内存替身；跨包实现本接口不算破坏封装，
// 因为 CheckPlayable 的缓存语义本身就是本服务的对外承诺。
type Cacher interface {
	Ping(ctx context.Context) error
	// GetCheckPlayable 返回 (playable, windowID, endTime, hit, err)；hit=false 表示 miss。
	GetCheckPlayable(ctx context.Context, contentID int64, contentType int32, region string) (bool, int64, int64, bool, error)
	SetCheckPlayable(ctx context.Context, contentID int64, contentType int32, region string, playable bool, windowID, endTime int64) error
	DelCheckPlayable(ctx context.Context, contentID int64, contentType int32, region string) error
}

var _ Cacher = (*Cache)(nil)

// Repository 是 rights 服务的数据访问入口。
type Repository struct {
	cache      Cacher
	contractMd model.RightsContractModel
	windowMd   model.RightsWindowModel
}

// New 构造 Repository（生产路径）。checkTTL <= 0 时使用默认值。
func New(rds *redis.Redis, conn sqlx.SqlConn, checkTTL int) *Repository {
	return NewWithDeps(NewCache(rds, checkTTL),
		model.NewRightsContractModel(conn), model.NewRightsWindowModel(conn))
}

// NewWithDeps 用显式依赖构造 Repository；只服务于测试注入，生产代码一律走 New。
func NewWithDeps(cache Cacher, contractMd model.RightsContractModel, windowMd model.RightsWindowModel) *Repository {
	return &Repository{cache: cache, contractMd: contractMd, windowMd: windowMd}
}

// Ping 检查 Redis 连通性。
func (r *Repository) Ping(ctx context.Context) error {
	return r.cache.Ping(ctx)
}

// --- 合同 ---

// CreateContract 新建合同。state 默认为 active。
func (r *Repository) CreateContract(ctx context.Context, c *model.RightsContract) (int64, error) {
	if c.State == 0 {
		c.State = model.ContractStateActive
	}
	c.RegionsCSV = model.RegionsToCSV(model.RegionsFromCSV(c.RegionsCSV))
	return r.contractMd.Insert(ctx, c)
}

// GetContract 查询单个合同；不存在返回 (nil, nil)。
func (r *Repository) GetContract(ctx context.Context, contractID int64) (*model.RightsContract, error) {
	return r.contractMd.FindOne(ctx, contractID)
}

// ListContracts 分页查询合同。ownerID=0/state=0 表示不筛选。
func (r *Repository) ListContracts(ctx context.Context, ownerID int64, state int32, pn, ps int32) ([]*model.RightsContract, int32, error) {
	return r.contractMd.List(ctx, ownerID, state, pn, ps)
}

// GetContractActive 校验合同是否存在且生效中。返回合同实体。
func (r *Repository) GetContractActive(ctx context.Context, contractID int64) (*model.RightsContract, error) {
	c, err := r.contractMd.FindOne(ctx, contractID)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, model.ErrContractNotFound
	}
	if c.State != model.ContractStateActive {
		return nil, model.ErrContractNotActive
	}
	return c, nil
}

// --- 窗口 ---

// CreateWindow 新建窗口。state 默认为 active。
func (r *Repository) CreateWindow(ctx context.Context, w *model.RightsWindow) (int64, error) {
	if w.State == 0 {
		w.State = model.WindowStateActive
	}
	windowID, err := r.windowMd.Insert(ctx, w)
	if err != nil {
		return 0, err
	}
	// 新窗口创建后失效对应内容的 CheckPlayable 缓存，避免旧缓存误判。
	_ = r.cache.DelCheckPlayable(ctx, w.ContentID, w.ContentType, w.Region)
	return windowID, nil
}

// GetWindow 查询单个窗口；不存在返回 (nil, nil)。
func (r *Repository) GetWindow(ctx context.Context, windowID int64) (*model.RightsWindow, error) {
	return r.windowMd.FindOne(ctx, windowID)
}

// ListWindows 分页查询窗口。contentID/contractID/contentType/state 为 0 表示不筛选。
func (r *Repository) ListWindows(ctx context.Context, contentID, contractID int64, contentType, state int32, pn, ps int32) ([]*model.RightsWindow, int32, error) {
	return r.windowMd.List(ctx, contentID, contractID, contentType, state, pn, ps)
}

// CheckPlayable 校验某 content_id 在某 region 是否可播放。
// 优先查 Redis 短缓存；miss 时查 DB，命中有效窗口后回填缓存。
// 校验条件：state=active AND start_time <= now AND end_time > now AND region 命中。
// 返回 (playable, windowID, endTime, err)。
func (r *Repository) CheckPlayable(ctx context.Context, contentID int64, contentType int32, region string) (bool, int64, int64, error) {
	// 1. 查缓存
	playable, wid, et, hit, err := r.cache.GetCheckPlayable(ctx, contentID, contentType, region)
	if err != nil {
		// 缓存出错不阻塞业务，落库查
		playable, wid, et, hit = false, 0, 0, false
	}
	if hit {
		return playable, wid, et, nil
	}

	// 2. miss 查 DB
	w, err := r.windowMd.FindActiveByContentRegion(ctx, contentID, contentType, region)
	if err != nil {
		return false, 0, 0, err
	}
	if w == nil {
		// 无窗口：回填负缓存
		_ = r.cache.SetCheckPlayable(ctx, contentID, contentType, region, false, 0, 0)
		return false, 0, 0, nil
	}

	// 3. 时间窗口校验
	now := model.NowUnix()
	if w.StartTime <= now && w.EndTime > now {
		// 可播放：回填正缓存。缓存 TTL 不超过窗口剩余时间，避免窗口过期后仍命中缓存。
		_ = r.cache.SetCheckPlayable(ctx, contentID, contentType, region, true, w.WindowID, w.EndTime)
		return true, w.WindowID, w.EndTime, nil
	}

	// 4. 窗口存在但时间未生效/已过期：回填负缓存并触发过期推进。
	_ = r.cache.SetCheckPlayable(ctx, contentID, contentType, region, false, 0, 0)
	// 若窗口已过期（end_time <= now），后台推进状态为 expired，避免长期命中 DB。
	if w.EndTime <= now {
		_ = r.ExpireWindowByID(ctx, w.WindowID, w.ContentID, w.ContentType, w.Region)
	}
	return false, 0, 0, nil
}

// ExpireWindowByID 把指定 window_id 推进到 expired 并失效缓存。
// 由 CheckPlayable 命中已过期窗口或 ExpireWindow RPC 调用。
func (r *Repository) ExpireWindowByID(ctx context.Context, windowID, contentID int64, contentType int32, region string) error {
	aff, err := r.windowMd.UpdateState(ctx, windowID, model.WindowStateExpired)
	if err != nil {
		return err
	}
	_ = aff
	_ = r.cache.DelCheckPlayable(ctx, contentID, contentType, region)
	return nil
}

// ExpireWindow 手动过期窗口（运营/cron）。
// 返回更新后的窗口实体；窗口不存在或已非 active 时返回对应错误。
func (r *Repository) ExpireWindow(ctx context.Context, windowID int64) (*model.RightsWindow, error) {
	w, err := r.windowMd.FindOne(ctx, windowID)
	if err != nil {
		return nil, err
	}
	if w == nil {
		return nil, model.ErrWindowNotFound
	}
	if w.State == model.WindowStateExpired {
		return nil, model.ErrWindowExpired
	}
	if w.State != model.WindowStateActive {
		return nil, model.ErrWindowNotActive
	}
	if err := r.ExpireWindowByID(ctx, w.WindowID, w.ContentID, w.ContentType, w.Region); err != nil {
		return nil, err
	}
	w.State = model.WindowStateExpired
	return w, nil
}

// ListExpiring 查询即将过期的窗口（state=active AND end_time <= now+within）。cron 用。
func (r *Repository) ListExpiring(ctx context.Context, withinSeconds int32, pn, ps int32) ([]*model.RightsWindow, int32, error) {
	return r.windowMd.ListExpiring(ctx, withinSeconds, pn, ps)
}
