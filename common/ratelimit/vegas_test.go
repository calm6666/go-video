package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestVegasAcquireAndStat 验证 Acquire 与 Success 反馈后 InFlight 归零。
func TestVegasAcquireAndStat(t *testing.T) {
	v := NewVegas()
	done, ok := v.Acquire()
	if !ok {
		t.Fatal("初始并发上限内应直接通过")
	}
	if s := v.Stat(); s.InFlight != 1 {
		t.Fatalf("InFlight 期望 1，得到 %d", s.InFlight)
	}
	// 反馈成功结果（带一个真实 RTT）。
	time.Sleep(time.Millisecond)
	done(time.Now().Add(-time.Millisecond), Success)
	if s := v.Stat(); s.InFlight != 0 {
		t.Fatalf("done 后 InFlight 期望 0，得到 %d", s.InFlight)
	}
}

// TestVegasIgnoreNotSample 验证 Ignore 反馈不污染采样统计。
func TestVegasIgnoreNotSample(t *testing.T) {
	v := NewVegas()
	done, _ := v.Acquire()
	done(time.Now(), Ignore)
	s := v.Stat()
	if s.LastRTT != 0 {
		t.Fatalf("Ignore 不应产生 RTT 样本，得到 %v", s.LastRTT)
	}
}

// TestVegasReset 验证 Reset 清空所有统计。
func TestVegasReset(t *testing.T) {
	v := NewVegas()
	done, _ := v.Acquire()
	time.Sleep(time.Millisecond)
	done(time.Now().Add(-time.Millisecond), Success)
	v.Reset()
	s := v.Stat()
	if s.Limit != vegasMinLimit {
		t.Errorf("Limit 期望 %d，得到 %d", vegasMinLimit, s.Limit)
	}
	if s.InFlight != 0 {
		t.Errorf("InFlight 期望 0，得到 %d", s.InFlight)
	}
	if s.MinRTT != 0 {
		t.Errorf("MinRTT 期望 0，得到 %v", s.MinRTT)
	}
}

// TestVegasConcurrentAcquire 并发压测，确保无 race 与泄漏。
func TestVegasConcurrentAcquire(t *testing.T) {
	v := NewVegas()
	var wg sync.WaitGroup
	var inFlightMax int64
	for i := 0; i < 500; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			done, _ := v.Acquire()
			defer done(start, Success)
			if cur := v.Stat().InFlight; cur > 0 {
				for old := atomic.LoadInt64(&inFlightMax); cur > old; old = atomic.LoadInt64(&inFlightMax) {
					if atomic.CompareAndSwapInt64(&inFlightMax, old, cur) {
						break
					}
				}
			}
			time.Sleep(time.Microsecond * 100)
		}()
	}
	wg.Wait()
	if s := v.Stat(); s.InFlight != 0 {
		t.Errorf("并发结束后 InFlight 期望 0，得到 %d", s.InFlight)
	}
}
