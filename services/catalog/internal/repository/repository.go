// Package repository 是 catalog 服务的数据访问层。
// 组合 catalog_work / catalog_season / catalog_episode / catalog_zone / catalog_tag
// 5 个 model，为 logic 层提供统一数据访问入口。
// 缓存策略：Work/Season/Episode 详情、分区树短 TTL 缓存；写操作失效对应缓存。
// 跨服务禁止直连本服务 MySQL/Redis（见 AGENTS.md §5）。
package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/catalog/model"
)

// Cache 封装 catalog 的 Redis 缓存操作。
// Work/Season/Episode 详情走短 TTL 缓存；分区树短缓存；标签不缓存（流量小）。
type Cache struct {
	rds *redis.Redis
}

// 缓存 key 与 TTL。
const (
	prefixWork    = "cat:w:%d"  // season_id(作品) → Work 详情 JSON
	prefixSeason  = "cat:s:%d"  // season_id(季) → Season 详情 JSON
	prefixEpisode = "cat:e:%d"  // epid → Episode 详情 JSON
	prefixZones   = "cat:zones" // 全量分区列表

	cacheTTLDetail = 300 // 详情缓存 5 分钟
	cacheTTLZones  = 60  // 分区树缓存 60 秒
)

func keyWork(seasonID int64) string   { return fmt.Sprintf(prefixWork, seasonID) }
func keySeason(seasonID int64) string { return fmt.Sprintf(prefixSeason, seasonID) }
func keyEpisode(epid int64) string    { return fmt.Sprintf(prefixEpisode, epid) }
func keyZones() string                { return prefixZones }

// NewCache 构造 Cache。
func NewCache(rds *redis.Redis) *Cache {
	return &Cache{rds: rds}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds.Ping() {
		return nil
	}
	return errors.New("catalog/cache: redis ping failed")
}

// --- Work 缓存 ---

// GetWork 读取作品缓存。payload 为空表示 miss。
func (c *Cache) GetWork(ctx context.Context, seasonID int64) (string, error) {
	bs, err := c.rds.GetCtx(ctx, keyWork(seasonID))
	if err != nil {
		if err == redis.Nil {
			return "", nil
		}
		return "", err
	}
	return bs, nil
}

// SetWork 写入作品缓存。
func (c *Cache) SetWork(ctx context.Context, seasonID int64, payload string) error {
	return c.rds.SetexCtx(ctx, keyWork(seasonID), payload, cacheTTLDetail)
}

// DelWork 删除作品缓存。
func (c *Cache) DelWork(ctx context.Context, seasonID int64) error {
	_, err := c.rds.DelCtx(ctx, keyWork(seasonID))
	return err
}

// --- Season 缓存 ---

// GetSeason 读取季缓存。
func (c *Cache) GetSeason(ctx context.Context, seasonID int64) (string, error) {
	bs, err := c.rds.GetCtx(ctx, keySeason(seasonID))
	if err != nil {
		if err == redis.Nil {
			return "", nil
		}
		return "", err
	}
	return bs, nil
}

// SetSeason 写入季缓存。
func (c *Cache) SetSeason(ctx context.Context, seasonID int64, payload string) error {
	return c.rds.SetexCtx(ctx, keySeason(seasonID), payload, cacheTTLDetail)
}

// DelSeason 删除季缓存。
func (c *Cache) DelSeason(ctx context.Context, seasonID int64) error {
	_, err := c.rds.DelCtx(ctx, keySeason(seasonID))
	return err
}

// --- Episode 缓存 ---

// GetEpisode 读取集缓存。
func (c *Cache) GetEpisode(ctx context.Context, epid int64) (string, error) {
	bs, err := c.rds.GetCtx(ctx, keyEpisode(epid))
	if err != nil {
		if err == redis.Nil {
			return "", nil
		}
		return "", err
	}
	return bs, nil
}

// SetEpisode 写入集缓存。
func (c *Cache) SetEpisode(ctx context.Context, epid int64, payload string) error {
	return c.rds.SetexCtx(ctx, keyEpisode(epid), payload, cacheTTLDetail)
}

// DelEpisode 删除集缓存。
func (c *Cache) DelEpisode(ctx context.Context, epid int64) error {
	_, err := c.rds.DelCtx(ctx, keyEpisode(epid))
	return err
}

// --- Zone 缓存 ---

// GetZones 读取全量分区缓存。
func (c *Cache) GetZones(ctx context.Context) (string, error) {
	bs, err := c.rds.GetCtx(ctx, keyZones())
	if err != nil {
		if err == redis.Nil {
			return "", nil
		}
		return "", err
	}
	return bs, nil
}

// SetZones 写入全量分区缓存。
func (c *Cache) SetZones(ctx context.Context, payload string) error {
	return c.rds.SetexCtx(ctx, keyZones(), payload, cacheTTLZones)
}

// --- Repository ---

// Cacher 是 Repository 用到的缓存能力集合，唯一生产实现是 *Cache。
// 之所以抽成接口：logic 层的按方法单测要在不连 Redis、不连 MySQL 的前提下装配
// Repository（AGENTS.md §9 要求测试能真的失败，不能靠真实依赖的可用性），
// 因此缓存与 5 个 model 都必须可替换。生产装配路径仍然只有 New()。
type Cacher interface {
	Ping(ctx context.Context) error
	GetWork(ctx context.Context, seasonID int64) (string, error)
	SetWork(ctx context.Context, seasonID int64, payload string) error
	DelWork(ctx context.Context, seasonID int64) error
	GetSeason(ctx context.Context, seasonID int64) (string, error)
	SetSeason(ctx context.Context, seasonID int64, payload string) error
	DelSeason(ctx context.Context, seasonID int64) error
	GetEpisode(ctx context.Context, epid int64) (string, error)
	SetEpisode(ctx context.Context, epid int64, payload string) error
	DelEpisode(ctx context.Context, epid int64) error
	GetZones(ctx context.Context) (string, error)
	SetZones(ctx context.Context, payload string) error
}

var _ Cacher = (*Cache)(nil)

// Repository 是 catalog 服务的数据访问入口。
type Repository struct {
	cache     Cacher
	workMd    model.WorkModel
	seasonMd  model.SeasonModel
	episodeMd model.EpisodeModel
	zoneMd    model.ZoneModel
	tagMd     model.TagModel
}

// New 构造生产 Repository：真 Redis 缓存 + 真 model 实现。
func New(rds *redis.Redis, conn sqlx.SqlConn) *Repository {
	return NewWithDeps(NewCache(rds),
		model.NewWorkModel(conn),
		model.NewSeasonModel(conn),
		model.NewEpisodeModel(conn),
		model.NewZoneModel(conn),
		model.NewTagModel(conn))
}

// NewWithDeps 用显式依赖装配 Repository。生产代码不走这里（统一用 New），
// 只有单测用它注入缓存与 model 替身，从而断言 logic 的校验/状态机/分页判定链。
func NewWithDeps(cache Cacher, work model.WorkModel, season model.SeasonModel,
	ep model.EpisodeModel, zone model.ZoneModel, tag model.TagModel) *Repository {
	return &Repository{
		cache:     cache,
		workMd:    work,
		seasonMd:  season,
		episodeMd: ep,
		zoneMd:    zone,
		tagMd:     tag,
	}
}

// Ping 检查 Redis 连通性。
func (r *Repository) Ping(ctx context.Context) error {
	return r.cache.Ping(ctx)
}

// --- Work ---

// CreateWork 新建作品；返回新 season_id。
func (r *Repository) CreateWork(ctx context.Context, w *model.Work) (int64, error) {
	id, err := r.workMd.Insert(ctx, w)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// GetWork 查询作品（先查缓存）。
func (r *Repository) GetWork(ctx context.Context, seasonID int64) (*model.Work, error) {
	payload, err := r.cache.GetWork(ctx, seasonID)
	if err != nil {
		return nil, err
	}
	if payload != "" {
		var w model.Work
		if err := jsonUnmarshal([]byte(payload), &w); err != nil {
			return nil, fmt.Errorf("GetWork unmarshal cache: %w", err)
		}
		return &w, nil
	}
	w, err := r.workMd.FindOne(ctx, seasonID)
	if err != nil {
		return nil, err
	}
	if w != nil {
		_ = r.cache.SetWork(ctx, seasonID, jsonMustMarshal(w))
	}
	return w, nil
}

// ListWorks 分页查询作品。
func (r *Repository) ListWorks(ctx context.Context, typeid, state int32, pn, ps int32) ([]*model.Work, int32, error) {
	return r.workMd.List(ctx, typeid, state, pn, ps)
}

// --- Season ---

// CreateSeason 新建季；返回新 season_id。
func (r *Repository) CreateSeason(ctx context.Context, s *model.Season) (int64, error) {
	id, err := r.seasonMd.Insert(ctx, s)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// ListSeasons 查询某作品的全部季。
func (r *Repository) ListSeasons(ctx context.Context, workID int64) ([]*model.Season, error) {
	return r.seasonMd.ListByWork(ctx, workID)
}

// GetSeason 查询单个季。
func (r *Repository) GetSeason(ctx context.Context, seasonID int64) (*model.Season, error) {
	return r.seasonMd.FindOne(ctx, seasonID)
}

// --- Episode ---

// CreateEpisode 新建集；返回新 epid。
func (r *Repository) CreateEpisode(ctx context.Context, e *model.Episode) (int64, error) {
	id, err := r.episodeMd.Insert(ctx, e)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// ListEpisodes 查询某季的全部集。
func (r *Repository) ListEpisodes(ctx context.Context, seasonID int64) ([]*model.Episode, error) {
	return r.episodeMd.ListBySeason(ctx, seasonID)
}

// GetEpisode 查询集详情（先查缓存）。
func (r *Repository) GetEpisode(ctx context.Context, epid int64) (*model.Episode, error) {
	payload, err := r.cache.GetEpisode(ctx, epid)
	if err != nil {
		return nil, err
	}
	if payload != "" {
		var e model.Episode
		if err := jsonUnmarshal([]byte(payload), &e); err != nil {
			return nil, fmt.Errorf("GetEpisode unmarshal cache: %w", err)
		}
		return &e, nil
	}
	e, err := r.episodeMd.FindOne(ctx, epid)
	if err != nil {
		return nil, err
	}
	if e != nil {
		_ = r.cache.SetEpisode(ctx, epid, jsonMustMarshal(e))
	}
	return e, nil
}

// UpdateEpisodeState 流转集状态（上架/下架）。
// 失效该集缓存，保证后续查询一致。
func (r *Repository) UpdateEpisodeState(ctx context.Context, epid int64, state int32) error {
	if err := r.episodeMd.UpdateState(ctx, epid, state); err != nil {
		return err
	}
	_ = r.cache.DelEpisode(ctx, epid)
	return nil
}

// --- Zone ---

// ListZones 查询全量分区（先查缓存）。
func (r *Repository) ListZones(ctx context.Context) ([]*model.Zone, error) {
	payload, err := r.cache.GetZones(ctx)
	if err != nil {
		return nil, err
	}
	if payload != "" {
		var zones []*model.Zone
		if err := jsonUnmarshal([]byte(payload), &zones); err != nil {
			return nil, fmt.Errorf("ListZones unmarshal cache: %w", err)
		}
		return zones, nil
	}
	zones, err := r.zoneMd.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	_ = r.cache.SetZones(ctx, jsonMustMarshal(zones))
	return zones, nil
}

// --- Tag ---

// FindTagsByIDs 按 ID 列表查询标签。
func (r *Repository) FindTagsByIDs(ctx context.Context, ids []int64) ([]*model.Tag, error) {
	return r.tagMd.FindByIDs(ctx, ids)
}

// SearchTags 按名字模糊查询标签。
func (r *Repository) SearchTags(ctx context.Context, name string, pn, ps int32) ([]*model.Tag, error) {
	return r.tagMd.SearchByName(ctx, name, pn, ps)
}
