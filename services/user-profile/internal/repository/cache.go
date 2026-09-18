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

// Cache 封装 user-profile 的 Redis 缓存操作（go-zero redis，JSON 值）。
type Cache struct {
	rds *redis.Redis
}

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

// getJSON 读取并反序列化 JSON 缓存；miss 返回 (nil, nil)。
func (c *Cache) getJSON(ctx context.Context, key string, v any) error {
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

// setJSON 写入 JSON 缓存（带过期时间）。
func (c *Cache) setJSON(ctx context.Context, key string, v any, ttlSeconds int) {
	bs, err := json.Marshal(v)
	if err != nil {
		logx.Errorf("user-profile/cache: marshal %s err=%v", key, err)
		return
	}
	if err := c.rds.SetexCtx(ctx, key, string(bs), ttlSeconds); err != nil {
		logx.Errorf("user-profile/cache: set %s err=%v", key, err)
	}
}

// del 删除缓存 key（miss 不视为错误）。
func (c *Cache) del(ctx context.Context, key string) error {
	_, err := c.rds.DelCtx(ctx, key)
	if err != nil && err != redis.Nil {
		return err
	}
	return nil
}

// getInt 读取整数缓存；miss 返回 (0, false)。
func (c *Cache) getInt(ctx context.Context, key string) (int64, bool) {
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

// setInt 写入整数缓存（带过期时间）。
func (c *Cache) setInt(ctx context.Context, key string, v int64, ttlSeconds int) {
	if err := c.rds.SetexCtx(ctx, key, strconv.FormatInt(v, 10), ttlSeconds); err != nil {
		logx.Errorf("user-profile/cache: setint %s err=%v", key, err)
	}
}

// delBaseCache 失效基础资料缓存（参考 DelBaseInfoCache）。
func (c *Cache) delBaseCache(ctx context.Context, mid int64) error {
	return c.del(ctx, keyBase(mid))
}

// delExpCache 失效经验缓存。
func (c *Cache) delExpCache(ctx context.Context, mid int64) error {
	return c.del(ctx, keyExp(mid))
}

// delMoralCache 失效节操缓存（参考 DelMoralCache）。
func (c *Cache) delMoralCache(ctx context.Context, mid int64) error {
	return c.del(ctx, keyMoral(mid))
}

// delRealnameCache 失效实名信息缓存（参考 DeleteRealnameInfo）。
func (c *Cache) delRealnameCache(ctx context.Context, mid int64) error {
	return c.del(ctx, keyRealname(mid))
}

// delCaptureCode 删除实名验证码（参考 DeleteRealnameCaptureCode）。
func (c *Cache) delCaptureCode(ctx context.Context, mid int64) error {
	return c.del(ctx, keyCaptureCode(mid))
}

// captureCode 读取实名验证码；miss 返回 -1。
func (c *Cache) captureCode(ctx context.Context, mid int64) (int, error) {
	if v, ok := c.getInt(ctx, keyCaptureCode(mid)); ok {
		return int(v), nil
	}
	return -1, nil
}

// setCaptureCode 写入实名验证码（10 分钟过期）。
func (c *Cache) setCaptureCode(ctx context.Context, mid int64, code int) error {
	c.setInt(ctx, keyCaptureCode(mid), int64(code), captureCodeTTL)
	return nil
}

// captureTimes 读取验证码发送次数；miss 返回 -1（参考 RealnameCaptureTimesCache）。
func (c *Cache) captureTimes(ctx context.Context, mid int64) (int, error) {
	if v, ok := c.getInt(ctx, keyCaptureTimes(mid)); ok {
		return int(v), nil
	}
	return -1, nil
}

// setCaptureTimes 写入验证码发送次数。
func (c *Cache) setCaptureTimes(ctx context.Context, mid int64, times int) error {
	c.setInt(ctx, keyCaptureTimes(mid), int64(times), captureTimesTTL)
	return nil
}

// incrCaptureTimes 发送次数 +1（参考 IncreaseRealnameCaptureTimes）。
func (c *Cache) incrCaptureTimes(ctx context.Context, mid int64) error {
	_, err := c.rds.IncrCtx(ctx, keyCaptureTimes(mid))
	if err != nil {
		return err
	}
	// 首次写入补 TTL
	if v, ok := c.getInt(ctx, keyCaptureTimes(mid)); ok && v <= 1 {
		c.rds.ExpireCtx(ctx, keyCaptureTimes(mid), captureTimesTTL)
	}
	return nil
}

// captureErrTimes 读取验证码错误次数；miss 返回 -1。
func (c *Cache) captureErrTimes(ctx context.Context, mid int64) (int, error) {
	if v, ok := c.getInt(ctx, keyCaptureErr(mid)); ok {
		return int(v), nil
	}
	return -1, nil
}

// setCaptureErrTimes 写入验证码错误次数。
func (c *Cache) setCaptureErrTimes(ctx context.Context, mid int64, times int) error {
	c.setInt(ctx, keyCaptureErr(mid), int64(times), captureErrTTL)
	return nil
}

// incrCaptureErrTimes 错误次数 +1。
func (c *Cache) incrCaptureErrTimes(ctx context.Context, mid int64) error {
	_, err := c.rds.IncrCtx(ctx, keyCaptureErr(mid))
	if err != nil {
		return err
	}
	if v, ok := c.getInt(ctx, keyCaptureErr(mid)); ok && v <= 1 {
		c.rds.ExpireCtx(ctx, keyCaptureErr(mid), captureErrTTL)
	}
	return nil
}

// delCaptureErrTimes 清空验证码错误次数（发送新验证码后重置）。
func (c *Cache) delCaptureErrTimes(ctx context.Context, mid int64) error {
	return c.del(ctx, keyCaptureErr(mid))
}

// statCache 读取当日经验奖励统计（参考 dao/redis.go StatCache 的 GETBIT 组合）。
func (c *Cache) statCache(ctx context.Context, mid, day int64) (login, watch, share bool, coin int64, err error) {
	login, err = c.getBit(ctx, expAddedKey(statLogin, mid, day), mid%expShard)
	if err != nil {
		return
	}
	watch, err = c.getBit(ctx, expAddedKey(statView, mid, day), mid%expShard)
	if err != nil {
		return
	}
	share, err = c.getBit(ctx, expAddedKey(statShare, mid, day), mid%expShard)
	if err != nil {
		return
	}
	if v, ok := c.getInt(ctx, expCoinKey(mid, day)); ok {
		coin = v
	}
	return
}

func (c *Cache) getBit(ctx context.Context, key string, offset int64) (bool, error) {
	v, err := c.rds.GetBitCtx(ctx, key, offset)
	if err != nil && err != redis.Nil {
		return false, err
	}
	return v == 1, nil
}
