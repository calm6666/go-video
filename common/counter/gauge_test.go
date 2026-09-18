package counter

import "testing"

// TestGaugeCounter 验证 gauge 计数器的累加、回退与重置。
func TestGaugeCounter(t *testing.T) {
	g := NewGauge()
	g.Add(10)
	g.Add(5)
	if v := g.Value(); v != 15 {
		t.Errorf("期望 15，得到 %d", v)
	}
	g.Add(-3)
	if v := g.Value(); v != 12 {
		t.Errorf("回退后期望 12，得到 %d", v)
	}
	g.Reset()
	if v := g.Value(); v != 0 {
		t.Errorf("重置后期望 0，得到 %d", v)
	}
}

// TestGaugeInGroup 验证 gauge 在 CounterGroup 中按 key 累加。
func TestGaugeInGroup(t *testing.T) {
	g := &CounterGroup{
		New: func() Counter { return NewGauge() },
	}
	g.Add("online", 1)
	g.Add("online", 2)
	g.Add("online", -1)
	if v := g.Value("online"); v != 2 {
		t.Errorf("期望 2，得到 %d", v)
	}
	g.Reset("online")
	if v := g.Value("online"); v != 0 {
		t.Errorf("重置后期望 0，得到 %d", v)
	}
}
