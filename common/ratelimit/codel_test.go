package ratelimit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCoDelPushPop 验证报文在队列停留时间低于 Target 时被允许通过。
func TestCoDelPushPop(t *testing.T) {
	q := NewCoDelQueue(nil)
	// 在另一个 goroutine 中调用 Pop，唤醒等待中的 Push。
	go q.Pop()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := q.Push(ctx); err != nil {
		t.Fatalf("push 期望成功，得到 %v", err)
	}
}

// TestCoDelDeadline 验证无人 Pop 时 Push 在 ctx 超时后返回 ErrDeadline。
func TestCoDelDeadline(t *testing.T) {
	q := NewCoDelQueue(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := q.Push(ctx)
	if !errors.Is(err, ErrDeadline) {
		t.Fatalf("期望 ErrDeadline，得到 %v", err)
	}
}

// TestCoDelQueueFull 通过白盒方式预先填满 packets 通道，验证 Push 立即返回 ErrLimitExceed。
func TestCoDelQueueFull(t *testing.T) {
	q := NewCoDelQueue(nil)
	// 直接填满队列通道，绕过 Pop 的消费。
	now := time.Now().UnixNano() / int64(time.Millisecond)
	for i := 0; i < cap(q.packets); i++ {
		q.packets <- coDelPacket{ch: make(chan bool), ts: now}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := q.Push(ctx); !errors.Is(err, ErrLimitExceed) {
		t.Fatalf("期望 ErrLimitExceed，得到 %v", err)
	}
}

// TestCoDelDrop 验证停留时间持续超过 Target 时 CoDel 进入丢包状态。
func TestCoDelDrop(t *testing.T) {
	// Target=5ms，Internal=50ms，便于在测试中触发丢包。
	q := NewCoDelQueue(&CoDelConfig{Target: 5, Internal: 50})
	now := func() int64 { return time.Now().UnixNano() / int64(time.Millisecond) }

	// 第一次 judge：停留时间超过 Target 且 faTime==0，设置 faTime=now+Internal，不丢包。
	oldTS := now() - 100
	if drop := q.judge(coDelPacket{ch: make(chan bool), ts: oldTS}); drop {
		t.Fatal("首次 judge 不应丢包")
	}

	// 进入丢包状态需满足 now-faTime >= Internal（即持续超过一个 Internal 窗口）。
	// faTime 在第一次调用时设为 T0+Internal，因此需等待到 T0+2*Internal 之后。
	// 这里等待 150ms（> 2*Internal=100ms）以留出抖动余量。
	time.Sleep(150 * time.Millisecond)
	oldTS2 := now() - 100
	if drop := q.judge(coDelPacket{ch: make(chan bool), ts: oldTS2}); !drop {
		t.Fatal("越过 faTime 后应丢包")
	}
	stat := q.Stat()
	if !stat.Dropping {
		t.Fatal("统计应处于丢包状态")
	}
}

// TestCoDelReload 验证 Reload 更新配置且忽略非法值。
func TestCoDelReload(t *testing.T) {
	q := NewCoDelQueue(nil)
	origTarget := q.conf.Target
	q.Reload(&CoDelConfig{Target: 100, Internal: 200})
	if q.conf.Target != 100 || q.conf.Internal != 200 {
		t.Fatalf("Reload 后配置未更新: %+v", q.conf)
	}
	// 非法配置应被忽略。
	q.Reload(nil)
	q.Reload(&CoDelConfig{Target: 0, Internal: 0})
	if q.conf.Target != 100 || q.conf.Internal != 200 {
		t.Fatalf("非法 Reload 不应覆盖配置: %+v", q.conf)
	}
	_ = origTarget
}

// TestCoDelConcurrent 简单并发压测，确保无死锁与 panic。
func TestCoDelConcurrent(t *testing.T) {
	q := NewCoDelQueue(&CoDelConfig{Target: 100, Internal: 200})
	var wg sync.WaitGroup
	var pushed, allowed, dropped int64
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := q.Push(ctx)
			atomic.AddInt64(&pushed, 1)
			if err == nil {
				atomic.AddInt64(&allowed, 1)
				// 模拟处理延迟后唤醒下一个。
				time.Sleep(time.Millisecond)
				q.Pop()
			} else if errors.Is(err, ErrLimitExceed) {
				atomic.AddInt64(&dropped, 1)
			}
		}()
	}
	wg.Wait()
	if pushed != 200 {
		t.Errorf("push 计数异常: %d", pushed)
	}
	t.Logf("coDel 并发: pushed=%d allowed=%d exceed=%d", pushed, allowed, dropped)
}
