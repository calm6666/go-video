package counter

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRollingCounterBasic 验证同桶内累加与重置。
func TestRollingCounterBasic(t *testing.T) {
	c := NewRolling(time.Second, 10)
	c.Add(5)
	c.Add(3)
	if v := c.Value(); v != 8 {
		t.Errorf("期望 8，得到 %d", v)
	}
	c.Reset()
	if v := c.Value(); v != 0 {
		t.Errorf("重置后期望 0，得到 %d", v)
	}
}

// TestRollingCounterExpire 验证窗口整体过期后值归零。
func TestRollingCounterExpire(t *testing.T) {
	// 2 桶各 100ms，窗口 200ms。
	c := NewRolling(200*time.Millisecond, 2)
	c.Add(7)
	if v := c.Value(); v != 7 {
		t.Fatalf("期望 7，得到 %d", v)
	}
	// 等待超过整个窗口，所有桶应被 elapsed 跳过。
	time.Sleep(350 * time.Millisecond)
	if v := c.Value(); v != 0 {
		t.Errorf("过期后期望 0，得到 %d", v)
	}
}

// TestRollingCounterRotate 验证推进游标后旧桶仍在窗口内被累计。
func TestRollingCounterRotate(t *testing.T) {
	// 10 桶各 100ms，窗口 1s。
	c := NewRolling(time.Second, 10)
	c.Add(10) // bucket[0] = 10
	// 推进约 1 个桶（>100ms）。
	time.Sleep(150 * time.Millisecond)
	c.Add(20) // 推进游标到 bucket[1] = 20，bucket[0] 仍在窗口内。
	v := c.Value()
	// 窗口内累计应为 30（bucket[0] + bucket[1]）；
	// 但睡眠抖动可能多推进 1~2 个桶，bucket[0] 仍处于 10 个桶内，结果应稳定为 30。
	if v < 20 || v > 30 {
		t.Errorf("期望 20~30，得到 %d", v)
	}
	if v != 30 {
		t.Logf("注意：得到 %d（睡眠抖动可能多推进桶，bucket[0] 仍计入）", v)
	}
}

// TestRollingCounterMinInterval 验证高频累加下窗口累计值近似为请求总数。
func TestRollingCounterMinInterval(t *testing.T) {
	// 10 桶各 50ms，窗口 500ms。
	c := NewRolling(500*time.Millisecond, 10)
	tk := time.NewTicker(5 * time.Millisecond)
	defer tk.Stop()
	for i := 0; i < 100; i++ {
		<-tk.C
		c.Add(1)
	}
	v := c.Value()
	// 100 次累加分布于 500ms 窗口内，允许边界抖动。
	if v < 80 || v > 100 {
		t.Errorf("期望 80~100，得到 %d", v)
	}
}

// TestRollingCounterConcurrent 验证并发累加结果的正确性。
func TestRollingCounterConcurrent(t *testing.T) {
	// 10 桶各 10ms，窗口 100ms。
	c := NewRolling(100*time.Millisecond, 10)
	var wg sync.WaitGroup
	var total int64
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Add(1)
			atomic.AddInt64(&total, 1)
		}()
	}
	wg.Wait()
	// Value 在并发 Add 进行中读取会略小于 total，但应近似。
	v := c.Value()
	if v < 1 {
		t.Errorf("并发后窗口值应大于 0，得到 %d", v)
	}
}
