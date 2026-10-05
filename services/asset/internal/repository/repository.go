// Package repository 是 asset 服务的数据访问层。
// 组合 asset_meta/asset_cover/asset_subtitle/asset_screenshot 4 个 model，
// 为 logic 层提供统一数据访问入口。
// Redis 缓存 Asset 详情，短 TTL；RegisterAsset/UpdateAssetMeta/TransitionState
// 主动失效缓存。
package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/asset/model"
)

// Cache 封装 asset 的 Redis 缓存操作。
// 只缓存 Asset 详情；列表与封面/字幕/截图列表不缓存（流量小，直接 DB）。
type Cache struct {
	rds *redis.Redis
}

// 缓存 key 与 TTL。
const (
	prefixAsset = "asset:a:%d" // asset_id → AssetMeta JSON
	cacheTTL    = 60
)

func keyAsset(assetID int64) string { return fmt.Sprintf(prefixAsset, assetID) }

// NewCache 构造 Cache。
func NewCache(rds *redis.Redis) *Cache {
	return &Cache{rds: rds}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds.Ping() {
		return nil
	}
	return errors.New("asset/cache: redis ping failed")
}

// GetAsset 读取 Asset 详情缓存；hit=false 表示缓存 miss。
func (c *Cache) GetAsset(ctx context.Context, assetID int64) (string, bool, error) {
	bs, err := c.rds.GetCtx(ctx, keyAsset(assetID))
	if err != nil {
		if err == redis.Nil {
			return "", false, nil
		}
		return "", false, err
	}
	return bs, bs != "", nil
}

// SetAsset 写入 Asset 详情缓存。
func (c *Cache) SetAsset(ctx context.Context, assetID int64, payload string) error {
	return c.rds.SetexCtx(ctx, keyAsset(assetID), payload, cacheTTL)
}

// DelAsset 失效 Asset 详情缓存。
func (c *Cache) DelAsset(ctx context.Context, assetID int64) error {
	_, err := c.rds.DelCtx(ctx, keyAsset(assetID))
	return err
}

// Cacher 是 Repository 对缓存的依赖面。生产实现是上面的 *Cache（Redis），
// logic 单元测试注入内存替身，于是「详情读穿回填、探测回写后失效、列表不缓存」
// 这条口径整条留在被测路径上，而不是把 Repository 一起 mock 掉。
//
// 只列 Repository 真正调用的方法：Cache 上没有调用点的方法不进接口，
// 否则替身被迫实现死代码，缓存调用次数的断言也会被污染。
type Cacher interface {
	Ping(ctx context.Context) error
	// GetAsset 读取 Asset 详情缓存 JSON；hit=false 表示缓存 miss。
	GetAsset(ctx context.Context, assetID int64) (string, bool, error)
	SetAsset(ctx context.Context, assetID int64, payload string) error
	DelAsset(ctx context.Context, assetID int64) error
}

var _ Cacher = (*Cache)(nil)

// --- Repository ---

// Repository 是 asset 服务的数据访问入口。
type Repository struct {
	cache   Cacher
	conn    sqlx.SqlConn
	metaMd  model.AssetMetaModel
	coverMd model.AssetCoverModel
	subMd   model.AssetSubtitleModel
	shotMd  model.AssetScreenshotModel
}

// New 构造 Repository（生产路径）。
func New(rds *redis.Redis, conn sqlx.SqlConn) *Repository {
	repo := NewWithDeps(NewCache(rds),
		model.NewAssetMetaModel(conn),
		model.NewAssetCoverModel(conn),
		model.NewAssetSubtitleModel(conn),
		model.NewAssetScreenshotModel(conn))
	// conn 只有这里赋值：Repository 自身现在没有直连 SQL（4 个 model 各持 conn），
	// 但字段保留，生产语义与加 NewWithDeps 之前逐字一致。
	repo.conn = conn
	return repo
}

// NewWithDeps 用显式依赖构造 Repository；只服务于测试注入，生产代码一律走 New。
//
// 不收 conn：本 Repository 的方法全部通过 4 个 model 访问数据库，没有任何
// conn.ExecCtx/QueryRowCtx 调用点（见 README「测试口径」）。将来某天真要直连 SQL，
// 这个构造器必须一起长出一个 conn 参数，否则新读路径会在测试里拿到 nil 而立刻炸。
func NewWithDeps(cache Cacher,
	metaMd model.AssetMetaModel,
	coverMd model.AssetCoverModel,
	subMd model.AssetSubtitleModel,
	shotMd model.AssetScreenshotModel) *Repository {
	return &Repository{
		cache:   cache,
		metaMd:  metaMd,
		coverMd: coverMd,
		subMd:   subMd,
		shotMd:  shotMd,
	}
}

// Ping 检查 Redis 连通性。
func (r *Repository) Ping(ctx context.Context) error {
	return r.cache.Ping(ctx)
}

// --- 媒资登记与查询 ---

// RegisterAsset 创建 asset_meta 记录；返回新 asset_id。
// 不立即触发转码，转码由 video 服务直接调用 transcode 触发。
func (r *Repository) RegisterAsset(ctx context.Context, m *model.AssetMeta) (int64, error) {
	id, err := r.metaMd.Insert(ctx, m)
	if err != nil {
		return 0, err
	}
	m.AssetID = id
	_ = r.cache.SetAsset(ctx, id, jsonMustMarshal(m))
	return id, nil
}

// GetAsset 查询媒资详情；先查缓存，miss 再查 DB 并回填。
func (r *Repository) GetAsset(ctx context.Context, assetID int64) (*model.AssetMeta, error) {
	payload, hit, err := r.cache.GetAsset(ctx, assetID)
	if err != nil {
		return nil, err
	}
	if hit {
		var m model.AssetMeta
		if err := jsonUnmarshal([]byte(payload), &m); err != nil {
			return nil, fmt.Errorf("GetAsset unmarshal cache: %w", err)
		}
		return &m, nil
	}
	m, err := r.metaMd.FindOne(ctx, assetID)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, model.ErrAssetNotFound
	}
	_ = r.cache.SetAsset(ctx, assetID, jsonMustMarshal(m))
	return m, nil
}

// ListAssets 分页查询媒资列表。
func (r *Repository) ListAssets(ctx context.Context, mid int64, state int32, pn, ps int32) ([]*model.AssetMeta, int32, error) {
	return r.metaMd.List(ctx, mid, state, pn, ps)
}

// UpdateAssetMeta 更新 duration/width/height/codec；失效缓存。
func (r *Repository) UpdateAssetMeta(ctx context.Context, assetID int64, duration int64, width, height int32, codec string) (*model.AssetMeta, error) {
	if err := r.metaMd.UpdateMeta(ctx, assetID, duration, width, height, codec); err != nil {
		return nil, err
	}
	_ = r.cache.DelAsset(ctx, assetID)
	return r.metaMd.FindOne(ctx, assetID)
}

// TransitionState 推进 asset 状态；校验合法性后写入；失效缓存。
func (r *Repository) TransitionState(ctx context.Context, assetID int64, fromState, toState int32) (*model.AssetMeta, error) {
	if !model.CanTransition(fromState, toState) {
		return nil, model.ErrInvalidTransition
	}
	if err := r.metaMd.UpdateState(ctx, assetID, toState); err != nil {
		return nil, err
	}
	_ = r.cache.DelAsset(ctx, assetID)
	return r.metaMd.FindOne(ctx, assetID)
}

// --- 封面 ---

// AddCover 添加封面；返回 cover_id。
func (r *Repository) AddCover(ctx context.Context, c *model.AssetCover) (int64, error) {
	return r.coverMd.Insert(ctx, c)
}

// ListCovers 查询某媒资的封面列表。
func (r *Repository) ListCovers(ctx context.Context, assetID int64) ([]*model.AssetCover, error) {
	return r.coverMd.ListByAsset(ctx, assetID)
}

// --- 字幕 ---

// AddSubtitle 添加字幕；返回 sub_id。
func (r *Repository) AddSubtitle(ctx context.Context, s *model.AssetSubtitle) (int64, error) {
	return r.subMd.Insert(ctx, s)
}

// ListSubtitles 查询某媒资的字幕列表。
func (r *Repository) ListSubtitles(ctx context.Context, assetID int64) ([]*model.AssetSubtitle, error) {
	return r.subMd.ListByAsset(ctx, assetID)
}

// --- 截图 ---

// AddScreenshot 添加截图；返回 shot_id。
func (r *Repository) AddScreenshot(ctx context.Context, s *model.AssetScreenshot) (int64, error) {
	return r.shotMd.Insert(ctx, s)
}
