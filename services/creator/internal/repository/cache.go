// Package repository 是 creator 服务的数据访问层。
// 组合本地 MySQL 模型与 Redis 缓存，为 logic 层提供统一数据访问入口。
// 移植自参考仓库 up 服务的 dao + service 聚合（仅保留特殊属性/分组/开关/签约子域）。
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/zeromicro/go-zero/core/stores/redis"
)

// 缓存 key 约定，沿用参考仓库 dao/mc_up.go 与 dao/mc.cache.go 风格。
const (
	prefixSpecial  = "up:spec:%d"     // 单个 mid 的特殊分组 ID 列表
	prefixAttr     = "up:attr:%d:%d"  // mid + from 复合键的 UP 身份
	prefixSwitch   = "up:sw:%d:%d"    // mid + from 复合键的开关状态
	prefixGroups   = "up:groups"      // 全量特殊分组列表
	prefixGroupMem = "up:gm:%d:%d:%d" // group_id + pn + ps 的分组成员列表
	prefixHighAlly = "up:ha:%d"       // 高能联盟签约（按 mid 单条缓存）

	// 缓存过期时间
	cacheTTLSpecial  = 3600  // 特殊属性 1 小时
	cacheTTLAttr     = 3600  // 身份属性 1 小时
	cacheTTLSwitch   = 86400 // 开关 24 小时
	cacheTTLGroups   = 300   // 分组列表 5 分钟（变化少）
	cacheTTLGroupMem = 60    // 分组成员列表 1 分钟（高实时性）
	cacheTTLHighAlly = 3600  // 高能联盟 1 小时

	// 防击穿空标记（参考 user-profile cache 设计）
	emptyMark = "{}"
)

func keySpecial(mid int64) string                { return fmt.Sprintf(prefixSpecial, mid) }
func keyAttr(mid int64, from int32) string       { return fmt.Sprintf(prefixAttr, mid, from) }
func keySwitch(mid int64, from int32) string     { return fmt.Sprintf(prefixSwitch, mid, from) }
func keyGroups() string                          { return prefixGroups }
func keyGroupMem(gid int64, pn, ps int32) string { return fmt.Sprintf(prefixGroupMem, gid, pn, ps) }
func keyHighAlly(mid int64) string               { return fmt.Sprintf(prefixHighAlly, mid) }

// Cache 封装 creator 的 Redis 缓存操作（go-zero redis，JSON 值）。
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
	return errors.New("creator/cache: redis ping failed")
}

// getJSON 读取并反序列化 JSON 缓存；miss 返回 (nil, nil)。
func (c *Cache) getJSON(ctx context.Context, key string, v any) error {
	bs, err := c.rds.GetCtx(ctx, key)
	if err != nil {
		if err == redis.Nil {
			return nil
		}
		return err
	}
	if bs == "" || bs == emptyMark {
		return nil
	}
	return json.Unmarshal([]byte(bs), v)
}

// setJSON 写入 JSON 缓存。
func (c *Cache) setJSON(ctx context.Context, key string, v any, ttlSec int) error {
	bs, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.rds.SetexCtx(ctx, key, string(bs), ttlSec)
}

// setEmpty 写入空标记防击穿。
func (c *Cache) setEmpty(ctx context.Context, key string, ttlSec int) error {
	return c.rds.SetexCtx(ctx, key, emptyMark, ttlSec)
}

// --- 特殊属性缓存 ---

// GetSpecial 读取单个 mid 的特殊分组 ID 列表缓存。
// miss 返回 (nil, nil)；空标记返回空切片。
func (c *Cache) GetSpecial(ctx context.Context, mid int64) ([]int64, error) {
	var ids []int64
	if err := c.getJSON(ctx, keySpecial(mid), &ids); err != nil {
		return nil, err
	}
	if ids == nil {
		// 区分空标记 vs 真 miss：空标记在 getJSON 中返回 nil，无法区分
		// 此处统一视为缓存 miss，调用方按 nil 处理回源 DB
		exist, err := c.rds.ExistsCtx(ctx, keySpecial(mid))
		if err != nil {
			return nil, err
		}
		if exist {
			return []int64{}, nil // 空标记
		}
		return nil, nil // miss
	}
	return ids, nil
}

// SetSpecial 写入单个 mid 的特殊分组 ID 列表缓存。
func (c *Cache) SetSpecial(ctx context.Context, mid int64, ids []int64) error {
	if ids == nil {
		return c.setEmpty(ctx, keySpecial(mid), cacheTTLSpecial)
	}
	return c.setJSON(ctx, keySpecial(mid), ids, cacheTTLSpecial)
}

// DelSpecial 删除特殊属性缓存（写操作后失效）。
func (c *Cache) DelSpecial(ctx context.Context, mid int64) error {
	_, err := c.rds.DelCtx(ctx, keySpecial(mid))
	return err
}

// --- 身份属性缓存 ---

// GetAttr 读取 UP 身份属性缓存。
func (c *Cache) GetAttr(ctx context.Context, mid int64, from int32) (int32, bool, error) {
	bs, err := c.rds.GetCtx(ctx, keyAttr(mid, from))
	if err != nil {
		if err == redis.Nil {
			return 0, false, nil
		}
		return 0, false, err
	}
	if bs == "" || bs == emptyMark {
		return 0, true, nil // 空标记：不存在
	}
	v, err := strconv.Atoi(bs)
	if err != nil {
		return 0, false, err
	}
	return int32(v), true, nil
}

// SetAttr 写入 UP 身份属性缓存。hit=false 表示空标记。
func (c *Cache) SetAttr(ctx context.Context, mid int64, from, state int32) error {
	return c.rds.SetexCtx(ctx, keyAttr(mid, from), strconv.Itoa(int(state)), cacheTTLAttr)
}

// SetAttrEmpty 写入空标记。
func (c *Cache) SetAttrEmpty(ctx context.Context, mid int64, from int32) error {
	return c.setEmpty(ctx, keyAttr(mid, from), cacheTTLAttr)
}

// --- 开关缓存 ---

// GetSwitch 读取开关状态缓存。
// 返回 (state, hit, err)：hit=false 表示缓存 miss。
func (c *Cache) GetSwitch(ctx context.Context, mid int64, from int32) (int32, bool, error) {
	bs, err := c.rds.GetCtx(ctx, keySwitch(mid, from))
	if err != nil {
		if err == redis.Nil {
			return 0, false, nil
		}
		return 0, false, err
	}
	if bs == "" || bs == emptyMark {
		return 0, true, nil // 空标记：按默认关闭
	}
	v, err := strconv.Atoi(bs)
	if err != nil {
		return 0, false, err
	}
	return int32(v), true, nil
}

// SetSwitch 写入开关状态缓存。
func (c *Cache) SetSwitch(ctx context.Context, mid int64, from, state int32) error {
	return c.rds.SetexCtx(ctx, keySwitch(mid, from), strconv.Itoa(int(state)), cacheTTLSwitch)
}

// DelSwitch 删除开关缓存。
func (c *Cache) DelSwitch(ctx context.Context, mid int64, from int32) error {
	_, err := c.rds.DelCtx(ctx, keySwitch(mid, from))
	return err
}

// --- 特殊分组列表缓存 ---

// GetGroups 读取全量特殊分组列表缓存。
// 返回 JSON 字符串（调用方反序列化）；miss 返回 ("", nil)。
func (c *Cache) GetGroups(ctx context.Context) (string, error) {
	bs, err := c.rds.GetCtx(ctx, keyGroups())
	if err != nil {
		if err == redis.Nil {
			return "", nil
		}
		return "", err
	}
	return bs, nil
}

// SetGroups 写入全量特殊分组列表缓存。
func (c *Cache) SetGroups(ctx context.Context, payload string) error {
	return c.rds.SetexCtx(ctx, keyGroups(), payload, cacheTTLGroups)
}

// --- 分组成员列表缓存 ---

// GetGroupMids 读取分组成员列表缓存。
func (c *Cache) GetGroupMids(ctx context.Context, gid int64, pn, ps int32) (string, error) {
	bs, err := c.rds.GetCtx(ctx, keyGroupMem(gid, pn, ps))
	if err != nil {
		if err == redis.Nil {
			return "", nil
		}
		return "", err
	}
	return bs, nil
}

// SetGroupMids 写入分组成员列表缓存。
func (c *Cache) SetGroupMids(ctx context.Context, gid int64, pn, ps int32, payload string) error {
	return c.rds.SetexCtx(ctx, keyGroupMem(gid, pn, ps), payload, cacheTTLGroupMem)
}
