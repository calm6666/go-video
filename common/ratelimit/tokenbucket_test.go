package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestTokenBucketAllow 验证桶容量内的请求被允许。
func TestTokenBucketAllow(t *testing.T) {
	tb := NewTokenBucket(1, 5) // 1 token/sec, burst 5
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := tb.Allow(ctx); err != nil {
			t.Fatalf("第 %d 次允许失败: %v", i, err)
		}
	}
}

// TestTokenBucketDeny 验证令牌耗尽后立即拒绝。
func TestTokenBucketDeny(t *testing.T) {
	tb := NewTokenBucket(1, 2) // burst 2
	ctx := context.Background()
	_, _ = tb.Allow(ctx)
	_, _ = tb.Allow(ctx)
	_, err := tb.Allow(ctx)
	if !errors.Is(err, ErrLimitExceed) {
		t.Fatalf("期望 ErrLimitExceed，得到 %v", err)
	}
}

// TestTokenBucketRefill 验证等待令牌恢复后允许通过。
func TestTokenBucketRefill(t *testing.T) {
	tb := NewTokenBucket(100, 1) // 100 token/sec = 1 token/10ms
	ctx := context.Background()
	_, _ = tb.Allow(ctx)
	// 等待 ~2 个令牌恢复。
	time.Sleep(30 * time.Millisecond)
	if _, err := tb.Allow(ctx); err != nil {
		t.Fatalf("恢复后应允许，得到 %v", err)
	}
}

// TestTokenBucketContextCancelled 验证 ctx 已取消时返回 ErrDeadline。
func TestTokenBucketContextCancelled(t *testing.T) {
	tb := NewTokenBucket(1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := tb.Allow(ctx)
	if !errors.Is(err, ErrDeadline) {
		t.Fatalf("期望 ErrDeadline，得到 %v", err)
	}
}

// TestTokenBucketImplementsLimiter 编译期验证实现 Limiter 接口。
func TestTokenBucketImplementsLimiter(t *testing.T) {
	var _ Limiter = (*TokenBucket)(nil)
}
