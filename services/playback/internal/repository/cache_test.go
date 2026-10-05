package repository

import (
	"fmt"
	"testing"
)

// TestSessionTTLBound 会话缓存 TTL 必须落在 [min,max] 且永不为 0：
// TTL=0 会让 Redis SETEX 直接报错，而超长缓存会让过期授权继续被放行。
func TestSessionTTLBound(t *testing.T) {
	cases := []struct {
		name     string
		expireAt int64
		now      int64
		want     int
	}{
		{"剩余充足按上限截断", 10_000, 1000, cacheTTLSessionMax},
		{"刚好等于上限", 1300, 1000, cacheTTLSessionMax},
		{"剩余 60 秒按实际值", 1060, 1000, 60},
		{"剩余不足最小值抬到最小值", 1005, 1000, cacheTTLSessionMin},
		{"已过期仍给最小 TTL（由逻辑层判定拒绝，缓存不得传 0）", 999, 1000, cacheTTLSessionMin},
		{"时间戳倒挂", 1000, 1000, cacheTTLSessionMin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionTTL(tc.expireAt, tc.now)
			if got != tc.want {
				t.Errorf("sessionTTL(%d,%d) = %d, want %d", tc.expireAt, tc.now, got, tc.want)
			}
			if got < cacheTTLSessionMin || got > cacheTTLSessionMax {
				t.Errorf("sessionTTL(%d,%d) = %d 越界 [%d,%d]",
					tc.expireAt, tc.now, got, cacheTTLSessionMin, cacheTTLSessionMax)
			}
		})
	}
}

// TestRedisKeyNamespacing key 必须带本服务前缀 pb:，禁止与其它服务共用键空间（AGENTS.md §5）。
func TestRedisKeyNamespacing(t *testing.T) {
	cases := map[string]string{
		"session":   fmt.Sprintf(keySession, "01HZ"),
		"verified":  fmt.Sprintf(keyVerified, "01HZ"),
		"playCount": fmt.Sprintf(keyPlayCount, 1, 100),
	}
	for name, key := range cases {
		if len(key) <= 3 || key[:3] != "pb:" {
			t.Errorf("%s key = %q, want 前缀 pb:", name, key)
		}
	}
	// 播放计数按内容维度分片，不同 content_type 不得串到同一个 key
	if fmt.Sprintf(keyPlayCount, 1, 100) == fmt.Sprintf(keyPlayCount, 2, 100) {
		t.Errorf("playCount key 未按 content_type 区分")
	}
}
