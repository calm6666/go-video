package counter

import (
	"sync"
	"testing"
)

// TestAtomicCounter 验证原子计数器的基本累加、回退与重置。
func TestAtomicCounter(t *testing.T) {
	c := NewAtomic()
	c.Add(1)
	c.Add(2)
	if v := c.Value(); v != 3 {
		t.Errorf("期望 3，得到 %d", v)
	}
	c.Add(-1)
	if v := c.Value(); v != 2 {
		t.Errorf("期望 2，得到 %d", v)
	}
	c.Reset()
	if v := c.Value(); v != 0 {
		t.Errorf("重置后期望 0，得到 %d", v)
	}
}

// TestAtomicConcurrentAdd 验证并发累加结果的正确性。
func TestAtomicConcurrentAdd(t *testing.T) {
	c := NewAtomic()
	var wg sync.WaitGroup
	const goroutines = 200
	const perG = 100
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				c.Add(1)
			}
		}()
	}
	wg.Wait()
	if want := int64(goroutines * perG); c.Value() != want {
		t.Errorf("并发后期望 %d，得到 %d", want, c.Value())
	}
}

// TestCounterGroup 验证按 key 分组的增、查、重置。
func TestCounterGroup(t *testing.T) {
	g := &CounterGroup{
		New: func() Counter { return NewAtomic() },
	}
	g.Add("a", 1)
	g.Add("a", 2)
	g.Add("b", 5)
	if v := g.Value("a"); v != 3 {
		t.Errorf("a 期望 3，得到 %d", v)
	}
	if v := g.Value("b"); v != 5 {
		t.Errorf("b 期望 5，得到 %d", v)
	}
	if v := g.Value("missing"); v != 0 {
		t.Errorf("未知 key 期望 0，得到 %d", v)
	}
	g.Reset("a")
	if v := g.Value("a"); v != 0 {
		t.Errorf("a 重置后期望 0，得到 %d", v)
	}
	// b 不受影响。
	if v := g.Value("b"); v != 5 {
		t.Errorf("b 不应受影响，得到 %d", v)
	}
}

// TestCounterGroupConcurrent 验证并发下相同 key 不会重复创建计数器。
func TestCounterGroupConcurrent(t *testing.T) {
	g := &CounterGroup{
		New: func() Counter { return NewAtomic() },
	}
	var wg sync.WaitGroup
	const goroutines = 100
	const perG = 50
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				g.Add("shared", 1)
			}
		}()
	}
	wg.Wait()
	if want := int64(goroutines * perG); g.Value("shared") != want {
		t.Errorf("并发后期望 %d，得到 %d", want, g.Value("shared"))
	}
}

// TestCounterGroupConcurrentDistinctKeys 验证并发下不同 key 的隔离性。
func TestCounterGroupConcurrentDistinctKeys(t *testing.T) {
	g := &CounterGroup{
		New: func() Counter { return NewAtomic() },
	}
	var wg sync.WaitGroup
	keys := []string{"k1", "k2", "k3"}
	for _, k := range keys {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				g.Add(key, 1)
			}
		}(k)
	}
	wg.Wait()
	for _, k := range keys {
		if v := g.Value(k); v != 100 {
			t.Errorf("%s 期望 100，得到 %d", k, v)
		}
	}
}
