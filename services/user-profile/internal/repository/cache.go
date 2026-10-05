package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// 缓存 key 约定，沿用参考仓库 dao/memcache.go 与 dao/redis.go。
const (
	prefixBase        = "bs_%d"                     // 基础资料
	prefixExp         = "exp_%d"                    // 经验值
	prefixMoral       = "moral_%d"                  // 节操值
	prefixRealname    = "realname_info_%d"          // 实名信息
	prefixCaptureCode = "realname_cap_code_%d"      // 实名验证码
	prefixCaptureTime = "realname_cap_times_%d"     // 验证码发送次数
	prefixCaptureErr  = "realname_cap_err_times_%d" // 验证码错误次数

	expShard       = 10000 // 经验奖励位图分片
	expAddedPrefix = "ea_%s_%d_%d"
	expCoinPrefix  = "ecoin_%d_%d"

	// 经验奖励类型
	statShare = "shareClick"
	statView  = "watch"
	statLogin = "login"

	// 缓存过期时间
	cacheTTLBase     = 3600  // 基础资料 1 小时
	cacheTTLExp      = 86400 // 经验 24 小时
	cacheTTLMoral    = 3600  // 节操 1 小时
	cacheTTLRealname = 3600  // 实名信息 1 小时
	captureCodeTTL   = 600   // 验证码 10 分钟
	captureTimesTTL  = 86400 // 发送次数统计 24 小时
	captureErrTTL    = 86400 // 错误次数统计 24 小时
)

func keyBase(mid int64) string         { return fmt.Sprintf(prefixBase, mid) }
func keyExp(mid int64) string          { return fmt.Sprintf(prefixExp, mid) }
func keyMoral(mid int64) string        { return fmt.Sprintf(prefixMoral, mid) }
func keyRealname(mid int64) string     { return fmt.Sprintf(prefixRealname, mid) }
func keyCaptureCode(mid int64) string  { return fmt.Sprintf(prefixCaptureCode, mid) }
func keyCaptureTimes(mid int64) string { return fmt.Sprintf(prefixCaptureTime, mid) }
func keyCaptureErr(mid int64) string   { return fmt.Sprintf(prefixCaptureErr, mid) }

func expAddedKey(tp string, mid, day int64) string {
	return fmt.Sprintf(expAddedPrefix, tp, day, mid/expShard)
}

func expCoinKey(mid, day int64) string {
	return fmt.Sprintf(expCoinPrefix, day, mid)
}

// Cacher 是 Repository 对 Redis 缓存的最小依赖面（与 rights/playback/catalog 各轮的
// Cacher 同构）。抽取本接口的唯一目的是给 internal/logic 的单测留注入缝：
// 测试用内存替身组装**真实 Repository**，让「缓存 → DB → 回填」整条判定链留在被测路径上，
// 而不是把 Repository 一起 mock 掉。生产路径仍然只走 New。
//
// 方法全部使用**未加工的 key**（bs_123 / exp_123 / moral_123 …），即 key 派生、TTL 选择、
// 「miss 记 -1」这类策略仍留在 Repository 侧——它们正是业务口径，必须可被断言；
// Cacher 只负责 Redis 读写原语。原 *Cache 上的组合方法（delBaseCache/statCache/…）
// 因此迁移成了 *Repository 的方法，语义逐字保持不变。
//
// 注意 *Cache 对读错误的处理是「吞成 miss 并记日志」（见 GetJSON/GetInt），
// 而 GetBit 会把错误如实上抛——这两套口径都由用例分别钉住，替身不得自行放宽。
type Cacher interface {
	// Ping 健康探测。
	Ping(ctx context.Context) error
	// GetJSON 读取并反序列化 JSON；miss 返回 nil（v 保持零值）。
	GetJSON(ctx context.Context, key string, v any) error
	// SetJSON 写入 JSON（带过期时间）。
	SetJSON(ctx context.Context, key string, v any, ttlSeconds int)
	// GetInt 读取整数；miss 返回 (0, false)。
	GetInt(ctx context.Context, key string) (int64, bool)
	// SetInt 写入整数（带过期时间）。
	SetInt(ctx context.Context, key string, v int64, ttlSeconds int)
	// Del 删除 key。
	Del(ctx context.Context, key string) error
	// GetBit 读取位图某一位。
	GetBit(ctx context.Context, key string, offset int64) (bool, error)
	// Incr 计数 +1 并返回新值。
	Incr(ctx context.Context, key string) (int64, error)
	// Expire 设置过期时间。
	Expire(ctx context.Context, key string, seconds int) error
}

// Cache 封装 user-profile 的 Redis 缓存操作（go-zero redis，JSON 值）。
type Cache struct {
	rds *redis.Redis
}

// 编译期确认 *Cache 满足注入缝接口。
var _ Cacher = (*Cache)(nil)

// NewCache 构造 Cache。
func NewCache(rds *redis.Redis) *Cache {
	return &Cache{rds: rds}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds.Ping() {
		return nil
	}
	return errors.New("user-profile/cache: redis ping failed")
}

// GetJSON 读取并反序列化 JSON 缓存；miss 与读故障都返回 nil（故障仅记日志）。
func (c *Cache) GetJSON(ctx context.Context, key string, v any) error {
	bs, err := c.rds.GetCtx(ctx, key)
	if err != nil {
		if err == redis.Nil {
			return nil
		}
		logx.Errorf("user-profile/cache: get %s err=%v", key, err)
		return nil // 缓存故障降级为 miss
	}
	if len(bs) == 0 {
		return nil
	}
	if err := json.Unmarshal([]byte(bs), v); err != nil {
		logx.Errorf("user-profile/cache: unmarshal %s err=%v", key, err)
		return nil
	}
	return nil
}

// SetJSON 写入 JSON 缓存（带过期时间，失败仅记日志）。
func (c *Cache) SetJSON(ctx context.Context, key string, v any, ttlSeconds int) {
	bs, err := json.Marshal(v)
	if err != nil {
		logx.Errorf("user-profile/cache: marshal %s err=%v", key, err)
		return
	}
	if err := c.rds.SetexCtx(ctx, key, string(bs), ttlSeconds); err != nil {
		logx.Errorf("user-profile/cache: set %s err=%v", key, err)
	}
}

// Del 删除缓存 key（miss 不视为错误）。
func (c *Cache) Del(ctx context.Context, key string) error {
	_, err := c.rds.DelCtx(ctx, key)
	if err != nil && err != redis.Nil {
		return err
	}
	return nil
}

// GetBit 读取位图某一位（redis.Nil 视为 0，其余故障上抛）。
func (c *Cache) GetBit(ctx context.Context, key string, offset int64) (bool, error) {
	v, err := c.rds.GetBitCtx(ctx, key, offset)
	if err != nil && err != redis.Nil {
		return false, err
	}
	return v == 1, nil
}

// Incr 计数 +1。
func (c *Cache) Incr(ctx context.Context, key string) (int64, error) {
	return c.rds.IncrCtx(ctx, key)
}

// Expire 设置 key 过期时间。
func (c *Cache) Expire(ctx context.Context, key string, seconds int) error {
	return c.rds.ExpireCtx(ctx, key, seconds)
}

// GetInt 读取整数缓存；miss 返回 (0, false)。
func (c *Cache) GetInt(ctx context.Context, key string) (int64, bool) {
	bs, err := c.rds.GetCtx(ctx, key)
	if err != nil || len(bs) == 0 {
		return 0, false
	}
	v, err := strconv.ParseInt(string(bs), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// SetInt 写入整数缓存（带过期时间）。
func (c *Cache) SetInt(ctx context.Context, key string, v int64, ttlSeconds int) {
	if err := c.rds.SetexCtx(ctx, key, strconv.FormatInt(v, 10), ttlSeconds); err != nil {
		logx.Errorf("user-profile/cache: setint %s err=%v", key, err)
	}
}

// --- 以下是 key 派生与 miss 策略（原 *Cache 的组合方法，逐字保持语义） ---

// delBaseCache 失效基础资料缓存（参考 DelBaseInfoCache）。
func (r *Repository) delBaseCache(ctx context.Context, mid int64) error {
	return r.cache.Del(ctx, keyBase(mid))
}

// delExpCache 失效经验缓存。
func (r *Repository) delExpCache(ctx context.Context, mid int64) error {
	return r.cache.Del(ctx, keyExp(mid))
}

// delMoralCache 失效节操缓存（参考 DelMoralCache）。
func (r *Repository) delMoralCache(ctx context.Context, mid int64) error {
	return r.cache.Del(ctx, keyMoral(mid))
}

// delRealnameCache 失效实名信息缓存（参考 DeleteRealnameInfo）。
func (r *Repository) delRealnameCache(ctx context.Context, mid int64) error {
	return r.cache.Del(ctx, keyRealname(mid))
}

// delCaptureCode 删除实名验证码（参考 DeleteRealnameCaptureCode）。
func (r *Repository) delCaptureCode(ctx context.Context, mid int64) error {
	return r.cache.Del(ctx, keyCaptureCode(mid))
}

// captureCode 读取实名验证码；miss 返回 -1。
func (r *Repository) captureCode(ctx context.Context, mid int64) (int, error) {
	if v, ok := r.cache.GetInt(ctx, keyCaptureCode(mid)); ok {
		return int(v), nil
	}
	return -1, nil
}

// setCaptureCode 写入实名验证码（10 分钟过期）。
func (r *Repository) setCaptureCode(ctx context.Context, mid int64, code int) error {
	r.cache.SetInt(ctx, keyCaptureCode(mid), int64(code), captureCodeTTL)
	return nil
}

// captureTimes 读取验证码发送次数；miss 返回 -1（参考 RealnameCaptureTimesCache）。
func (r *Repository) captureTimes(ctx context.Context, mid int64) (int, error) {
	if v, ok := r.cache.GetInt(ctx, keyCaptureTimes(mid)); ok {
		return int(v), nil
	}
	return -1, nil
}

// setCaptureTimes 写入验证码发送次数。
func (r *Repository) setCaptureTimes(ctx context.Context, mid int64, times int) error {
	r.cache.SetInt(ctx, keyCaptureTimes(mid), int64(times), captureTimesTTL)
	return nil
}

// incrCaptureTimes 发送次数 +1（参考 IncreaseRealnameCaptureTimes）。
func (r *Repository) incrCaptureTimes(ctx context.Context, mid int64) error {
	key := keyCaptureTimes(mid)
	if _, err := r.cache.Incr(ctx, key); err != nil {
		return err
	}
	// 首次写入补 TTL
	if v, ok := r.cache.GetInt(ctx, key); ok && v <= 1 {
		_ = r.cache.Expire(ctx, key, captureTimesTTL)
	}
	return nil
}

// captureErrTimes 读取验证码错误次数；miss 返回 -1。
func (r *Repository) captureErrTimes(ctx context.Context, mid int64) (int, error) {
	if v, ok := r.cache.GetInt(ctx, keyCaptureErr(mid)); ok {
		return int(v), nil
	}
	return -1, nil
}

// setCaptureErrTimes 写入验证码错误次数。
func (r *Repository) setCaptureErrTimes(ctx context.Context, mid int64, times int) error {
	r.cache.SetInt(ctx, keyCaptureErr(mid), int64(times), captureErrTTL)
	return nil
}

// incrCaptureErrTimes 错误次数 +1。
func (r *Repository) incrCaptureErrTimes(ctx context.Context, mid int64) error {
	key := keyCaptureErr(mid)
	if _, err := r.cache.Incr(ctx, key); err != nil {
		return err
	}
	if v, ok := r.cache.GetInt(ctx, key); ok && v <= 1 {
		_ = r.cache.Expire(ctx, key, captureErrTTL)
	}
	return nil
}

// delCaptureErrTimes 清空验证码错误次数（发送新验证码后重置）。
func (r *Repository) delCaptureErrTimes(ctx context.Context, mid int64) error {
	return r.cache.Del(ctx, keyCaptureErr(mid))
}

// statCache 读取当日经验奖励统计（参考 dao/redis.go StatCache 的 GETBIT 组合）。
// 位图读取失败会如实上抛（与 JSON/整数缓存「吞成 miss」的口径不同，见 GetBit 注释）。
func (r *Repository) statCache(ctx context.Context, mid, day int64) (login, watch, share bool, coin int64, err error) {
	if login, err = r.cache.GetBit(ctx, expAddedKey(statLogin, mid, day), mid%expShard); err != nil {
		return
	}
	if watch, err = r.cache.GetBit(ctx, expAddedKey(statView, mid, day), mid%expShard); err != nil {
		return
	}
	if share, err = r.cache.GetBit(ctx, expAddedKey(statShare, mid, day), mid%expShard); err != nil {
		return
	}
	if v, ok := r.cache.GetInt(ctx, expCoinKey(mid, day)); ok {
		coin = v
	}
	return
}
