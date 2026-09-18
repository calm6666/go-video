package ratelimit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAdaptiveAllow 验证单个请求在并发上限内被允许通过。
func TestAdaptiveAllow(t *testing.T) {
	l := NewAdaptiveLimiter(nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done, err := l.Allow(ctx)
	if err != nil {
		t.Fatalf("Allow 期望成功，得到 %v", err)
	}
	done(Success)
	if s := l.Stat(); s.Vegas.InFlight != 0 {
		t.Errorf("InFlight 期望 0，得到 %d", s.Vegas.InFlight)
	}
}

// TestAdaptiveDeadline 验证并发上限外且无人唤醒队列时返回 ErrDeadline。
func TestAdaptiveDeadline(t *testing.T) {
	l := NewAdaptiveLimiter(nil)
	// 不调用 done 占用 inflight 槽位，迫使后续请求进入 CoDel 队列等待超时。
	var wg sync.WaitGroup
	var deadlines int64
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			_, err := l.Allow(ctx)
			if errors.Is(err, ErrDeadline) {
				atomic.AddInt64(&deadlines, 1)
			}
		}()
	}
	wg.Wait()
	if deadlines == 0 {
		t.Errorf("期望部分请求因超时被拒，得到 %d", deadlines)
	}
}

// TestAdaptiveStat 验证 Stat 返回合并统计。
func TestAdaptiveStat(t *testing.T) {
	l := NewAdaptiveLimiter(nil)
	_ = l.Stat() // 不应 panic
	l.rate.Reset()
	if s := l.Stat(); s.Vegas.Limit != vegasMinLimit {
		t.Errorf("Vegas.Limit 期望 %d，得到 %d", vegasMinLimit, s.Vegas.Limit)
	}
}

// TestAdaptiveConcurrent 并发压测，确保不 panic 且 InFlight 最终归零。
func TestAdaptiveConcurrent(t *testing.T) {
	l := NewAdaptiveLimiter(nil)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			done, err := l.Allow(ctx)
			if err != nil {
				return
			}
			time.Sleep(time.Millisecond)
			done(Success)
		}()
	}
	wg.Wait()
	// 等待 inflight 全部归还。
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if l.Stat().Vegas.InFlight == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if s := l.Stat(); s.Vegas.InFlight != 0 {
		t.Errorf("并发结束后 InFlight 期望 0，得到 %d", s.Vegas.InFlight)
	}
}
