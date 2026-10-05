// Package repository 是 danmaku 服务的数据访问层。
// 组合 danmaku / danmaku_segment / danmaku_blockword / danmaku_user_block /
// danmaku_report / danmaku_op_log 六个 model 与 Redis 缓存，为 logic 层提供
// 统一入口。
//
// 缓存策略对应参考仓库 dm 服务的分段索引：
//   - 段列表 dm:seg:<oid>:<seg> 短 TTL（时间轴窗口重复读极多）；
//   - 段计数 dm:cnt:<oid>:<seg>；
//   - 屏蔽词库 dm:bw:* 长 TTL，运营写操作后主动失效；
//   - 防刷屏计数 dm:rl:* 按分钟分桶，key 自带时间维度，TTL 仅用于回收。
//
// SQL 一律在 model 层，logic 层不接触任何查询语句。
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/zeromicro/go-zero/core/stores/redis"

	"go-video/services/danmaku/model"
)

// Redis key 约定（前缀 dm: 独占，不与 comment/engagement 的 key 空间重叠）。
const (
	keySegList   = "dm:seg:%d:%d"    // oid:seg_no → 段内可见弹幕 JSON 数组
	keySegCount  = "dm:cnt:%d:%d"    // oid:seg_no → 段内可见弹幕计数
	keyOidTotal  = "dm:tot:%d"       // oid → 可见弹幕总数
	keyBlockWord = "dm:bw:%d"        // oid(0=全局) → 生效屏蔽词 JSON 数组
	keyUserBlock = "dm:ub:%d"        // mid → 用户生效屏蔽项 JSON 数组
	keyRateMid   = "dm:rl:mid:%d:%d" // mid:分钟 → 发送计数
	keyRateOid   = "dm:rl:oid:%d:%d" // oid:分钟 → 发送计数
)

// 窗口与缓存参数。
const (
	// rateWindowSeconds 是防刷屏固定窗口长度（秒）。
	rateWindowSeconds = 60
	// rateKeyTTLSeconds 是窗口 key 的保留时长，仅用于回收，
	// 正确性由 key 中的分钟桶保证。
	rateKeyTTLSeconds = 180
)

// Cache 封装 danmaku 的 Redis 操作。
type Cache struct {
	rds *redis.Redis
	// segTTL/blockwordTTL 由配置注入，避免在代码里写死缓存秒数。
	segTTL       int
	blockwordTTL int
	userBlockTTL int
	countTTL     int
}

// CacheOptions 是缓存相关配置。
type CacheOptions struct {
	// SegmentTTLSeconds 段列表缓存秒数。
	SegmentTTLSeconds int
	// SegmentCountTTLSeconds 段计数缓存秒数。
	SegmentCountTTLSeconds int
	// BlockWordTTLSeconds 屏蔽词库缓存秒数。
	BlockWordTTLSeconds int
	// UserBlockTTLSeconds 用户屏蔽列表缓存秒数。
	UserBlockTTLSeconds int
}

// Cacher 是 Repository 对缓存的最小依赖契约，逐字对应本文件 *Cache 上被
// Repository 调用的方法集（DelSegments/IncrRateWindow 这类只服务于 *Cache 自身
// 或批量场景的方法不进契约）。
//
// 为什么要这个接口：ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造走 New（真 Redis + 真 MySQL），logic 单测无处塞替身。有了 Cacher，
// 测试就能用 NewWithDeps(内存缓存, 内存 model…) 组装**真实的 Repository**，
// 让「段缓存读穿/回填、空段防击穿、屏蔽词降级、段计数增减、失效顺序」整条判定链
// 留在被测路径上，而不是把 Repository 整个 mock 掉。生产路径仍然只走 New。
type Cacher interface {
	// Ping 健康探针。
	Ping(ctx context.Context) error

	// IncrMidWindow / IncrOidWindow 是防刷屏的分钟窗口计数。
	IncrMidWindow(ctx context.Context, mid int64, now time.Time) (int32, error)
	IncrOidWindow(ctx context.Context, oid int64, now time.Time) (int32, error)

	// 段列表缓存。
	GetSegment(ctx context.Context, oid int64, segNo int32) ([]*model.Danmaku, bool, error)
	SetSegment(ctx context.Context, oid int64, segNo int32, rows []*model.Danmaku) error
	DelSegment(ctx context.Context, oid int64, segNo int32) error

	// 段计数缓存。
	GetSegmentCounts(ctx context.Context, oid int64, segs []int32) (map[int32]int32, error)
	SetSegmentCount(ctx context.Context, oid int64, segNo, count int32) error
	IncrSegmentCount(ctx context.Context, oid int64, segNo, delta int32) error

	// 屏蔽词库缓存。
	GetBlockWords(ctx context.Context, oid int64) ([]string, bool, error)
	SetBlockWords(ctx context.Context, oid int64, words []string) error
	DelBlockWords(ctx context.Context, oid int64) error

	// 用户屏蔽列表缓存。
	GetUserBlocks(ctx context.Context, mid int64) ([]*model.UserBlock, bool, error)
	SetUserBlocks(ctx context.Context, mid int64, rows []*model.UserBlock) error
	DelUserBlocks(ctx context.Context, mid int64) error
}

// 编译期确认真实缓存实现满足契约；签名漂移在这里立刻炸出，而不是留到 logic 单测。
var _ Cacher = (*Cache)(nil)

// NewCache 构造 Cache，非法 TTL 回退为 0（表示不缓存）。
func NewCache(rds *redis.Redis, opt CacheOptions) *Cache {
	return &Cache{
		rds:          rds,
		segTTL:       opt.SegmentTTLSeconds,
		blockwordTTL: opt.BlockWordTTLSeconds,
		userBlockTTL: opt.UserBlockTTLSeconds,
		countTTL:     opt.SegmentCountTTLSeconds,
	}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds.PingCtx(ctx) {
		return nil
	}
	return errors.New("danmaku/cache: redis ping failed")
}

// --- 分段列表缓存 ---

// GetSegment 读取某段弹幕缓存。
// 返回 (rows, hit)：hit=false 表示未命中，需要回源 MySQL。
// 空数组 [] 是有效的命中结果（防击穿）。
func (c *Cache) GetSegment(ctx context.Context, oid int64, segNo int32) ([]*model.Danmaku, bool, error) {
	if c.segTTL <= 0 {
		return nil, false, nil
	}
	bs, err := c.rds.GetCtx(ctx, fmt.Sprintf(keySegList, oid, segNo))
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("danmaku/cache GetSegment: %w", err)
	}
	if bs == "" {
		return nil, false, nil
	}
	var rows []*model.Danmaku
	if err := json.Unmarshal([]byte(bs), &rows); err != nil {
		// 脏数据视为未命中并删除，避免持续返回错误结果。
		_, _ = c.rds.DelCtx(ctx, fmt.Sprintf(keySegList, oid, segNo))
		return nil, false, nil
	}
	return rows, true, nil
}

// SetSegment 写入某段弹幕缓存。
func (c *Cache) SetSegment(ctx context.Context, oid int64, segNo int32, rows []*model.Danmaku) error {
	if c.segTTL <= 0 {
		return nil
	}
	if rows == nil {
		rows = []*model.Danmaku{}
	}
	bs, err := json.Marshal(rows)
	if err != nil {
		return fmt.Errorf("danmaku/cache SetSegment marshal: %w", err)
	}
	return c.rds.SetexCtx(ctx, fmt.Sprintf(keySegList, oid, segNo), string(bs), c.segTTL)
}

// DelSegment 失效某段缓存（状态推进/删除后调用）。
func (c *Cache) DelSegment(ctx context.Context, oid int64, segNo int32) error {
	_, err := c.rds.DelCtx(ctx, fmt.Sprintf(keySegList, oid, segNo))
	return err
}

// DelSegments 批量失效分段缓存。
func (c *Cache) DelSegments(ctx context.Context, oid int64, segs []int32) error {
	for _, s := range segs {
		if err := c.DelSegment(ctx, oid, s); err != nil {
			return err
		}
	}
	return nil
}

// --- 分段计数 ---

// GetSegmentCounts 批量读取段计数缓存，返回 seg → count（缺失不在 map 中）。
func (c *Cache) GetSegmentCounts(ctx context.Context, oid int64, segs []int32) (map[int32]int32, error) {
	out := make(map[int32]int32, len(segs))
	if c.countTTL <= 0 {
		return out, nil
	}
	for _, s := range segs {
		bs, err := c.rds.GetCtx(ctx, fmt.Sprintf(keySegCount, oid, s))
		if err != nil {
			if errors.Is(err, redis.Nil) {
				continue
			}
			return out, fmt.Errorf("danmaku/cache GetSegmentCounts: %w", err)
		}
		if bs == "" {
			continue
		}
		n, err := strconv.ParseInt(bs, 10, 32)
		if err != nil {
			continue
		}
		out[s] = int32(n)
	}
	return out, nil
}

// SetSegmentCount 写入段计数缓存。
func (c *Cache) SetSegmentCount(ctx context.Context, oid int64, segNo, count int32) error {
	if c.countTTL <= 0 {
		return nil
	}
	return c.rds.SetexCtx(ctx, fmt.Sprintf(keySegCount, oid, segNo), strconv.FormatInt(int64(count), 10), c.countTTL)
}

// IncrSegmentCount 实时增减段计数缓存（写路径同步，读路径回源修正）。
func (c *Cache) IncrSegmentCount(ctx context.Context, oid int64, segNo, delta int32) error {
	if c.countTTL <= 0 {
		return nil
	}
	key := fmt.Sprintf(keySegCount, oid, segNo)
	n, err := c.rds.IncrbyCtx(ctx, key, int64(delta))
	if err != nil {
		return fmt.Errorf("danmaku/cache IncrSegmentCount: %w", err)
	}
	if n <= 0 {
		// 计数被回删到 0 以下时清掉，下一次读回源 DB 修正。
		_, _ = c.rds.DelCtx(ctx, key)
	}
	return nil
}

// --- 防刷屏固定窗口 ---

// currentMinute 返回当前分钟桶。
func currentMinute(now time.Time) int64 { return now.Unix() / rateWindowSeconds }

// IncrRateWindow 对某个维度的分钟窗口计数 +1 并返回窗口内累计值。
// key 内含分钟桶，TTL 只用于回收过期 key。
func (c *Cache) IncrRateWindow(ctx context.Context, keyFmt string, id int64, now time.Time) (int32, error) {
	key := fmt.Sprintf(keyFmt, id, currentMinute(now))
	n, err := c.rds.IncrCtx(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("danmaku/cache IncrRateWindow: %w", err)
	}
	if err := c.rds.ExpireCtx(ctx, key, rateKeyTTLSeconds); err != nil {
		return int32(n), fmt.Errorf("danmaku/cache IncrRateWindow expire: %w", err)
	}
	return int32(n), nil
}

// IncrMidWindow mid 维度分钟窗口。
func (c *Cache) IncrMidWindow(ctx context.Context, mid int64, now time.Time) (int32, error) {
	return c.IncrRateWindow(ctx, keyRateMid, mid, now)
}

// IncrOidWindow oid 维度分钟窗口。
func (c *Cache) IncrOidWindow(ctx context.Context, oid int64, now time.Time) (int32, error) {
	return c.IncrRateWindow(ctx, keyRateOid, oid, now)
}

// --- 屏蔽词库缓存 ---

// GetBlockWords 读取生效屏蔽词缓存（oid=0 表示全局词库）。
func (c *Cache) GetBlockWords(ctx context.Context, oid int64) ([]string, bool, error) {
	if c.blockwordTTL <= 0 {
		return nil, false, nil
	}
	bs, err := c.rds.GetCtx(ctx, fmt.Sprintf(keyBlockWord, oid))
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("danmaku/cache GetBlockWords: %w", err)
	}
	if bs == "" {
		return nil, false, nil
	}
	var words []string
	if err := json.Unmarshal([]byte(bs), &words); err != nil {
		_, _ = c.rds.DelCtx(ctx, fmt.Sprintf(keyBlockWord, oid))
		return nil, false, nil
	}
	return words, true, nil
}

// SetBlockWords 写入生效屏蔽词缓存。
func (c *Cache) SetBlockWords(ctx context.Context, oid int64, words []string) error {
	if c.blockwordTTL <= 0 {
		return nil
	}
	if words == nil {
		words = []string{}
	}
	bs, err := json.Marshal(words)
	if err != nil {
		return fmt.Errorf("danmaku/cache SetBlockWords marshal: %w", err)
	}
	return c.rds.SetexCtx(ctx, fmt.Sprintf(keyBlockWord, oid), string(bs), c.blockwordTTL)
}

// DelBlockWords 失效屏蔽词缓存（全局 + 指定分区）。
func (c *Cache) DelBlockWords(ctx context.Context, oid int64) error {
	keys := []string{fmt.Sprintf(keyBlockWord, 0)}
	if oid > 0 {
		keys = append(keys, fmt.Sprintf(keyBlockWord, oid))
	}
	_, err := c.rds.DelCtx(ctx, keys...)
	return err
}

// --- 用户屏蔽列表缓存 ---

// GetUserBlocks 读取用户生效屏蔽项缓存。
func (c *Cache) GetUserBlocks(ctx context.Context, mid int64) ([]*model.UserBlock, bool, error) {
	if c.userBlockTTL <= 0 || mid <= 0 {
		return nil, false, nil
	}
	bs, err := c.rds.GetCtx(ctx, fmt.Sprintf(keyUserBlock, mid))
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("danmaku/cache GetUserBlocks: %w", err)
	}
	if bs == "" {
		return nil, false, nil
	}
	var rows []*model.UserBlock
	if err := json.Unmarshal([]byte(bs), &rows); err != nil {
		_, _ = c.rds.DelCtx(ctx, fmt.Sprintf(keyUserBlock, mid))
		return nil, false, nil
	}
	return rows, true, nil
}

// SetUserBlocks 写入用户生效屏蔽项缓存。
func (c *Cache) SetUserBlocks(ctx context.Context, mid int64, rows []*model.UserBlock) error {
	if c.userBlockTTL <= 0 || mid <= 0 {
		return nil
	}
	if rows == nil {
		rows = []*model.UserBlock{}
	}
	bs, err := json.Marshal(rows)
	if err != nil {
		return fmt.Errorf("danmaku/cache SetUserBlocks marshal: %w", err)
	}
	return c.rds.SetexCtx(ctx, fmt.Sprintf(keyUserBlock, mid), string(bs), c.userBlockTTL)
}

// DelUserBlocks 失效用户屏蔽列表缓存。
func (c *Cache) DelUserBlocks(ctx context.Context, mid int64) error {
	_, err := c.rds.DelCtx(ctx, fmt.Sprintf(keyUserBlock, mid))
	return err
}
