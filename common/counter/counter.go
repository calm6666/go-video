// Package counter 提供线程安全的计数器原语。
//
// 提供三种 Counter 实现：
//   - NewAtomic：基于 sync/atomic 的原子计数器。
//   - NewGauge：gauge 计数器，语义上表示可上下波动的瞬时值。
//   - NewRolling：滑动窗口计数器，仅统计窗口内有效桶的累计值。
//
// CounterGroup 支持按 key 分组管理多个计数器。
package counter

import (
	"sync"
	"sync/atomic"
)

// Counter 是所有计数器的统一接口。
type Counter interface {
	// Add 增加计数值，可传负数回退。
	Add(int64)
	// Reset 重置计数器到零。
	Reset()
	// Value 返回当前计数值。
	Value() int64
}

// 编译期断言 atomicCounter 实现 Counter。
var _ Counter = (*atomicCounter)(nil)

// atomicCounter 是基于 sync/atomic 的简单计数器，单调累加（可回退）。
type atomicCounter int64

// NewAtomic 返回一个原子计数器。
func NewAtomic() Counter {
	return new(atomicCounter)
}

// Add 原子地累加值。
func (c *atomicCounter) Add(val int64) {
	atomic.AddInt64((*int64)(c), val)
}

// Value 原子地读取当前值。
func (c *atomicCounter) Value() int64 {
	return atomic.LoadInt64((*int64)(c))
}

// Reset 原子地清零。
func (c *atomicCounter) Reset() {
	atomic.StoreInt64((*int64)(c), 0)
}

// CounterGroup 是按 key 分组的计数器集合。
//
// 调用方需在构造时设置 New 字段以指定计数器构造方式；首次访问未知 key 时
// 会调用 New 创建并加入集合。
type CounterGroup struct {
	mu   sync.RWMutex
	vecs map[string]Counter

	// New 指定计数器构造函数；不应在并发调用其他方法期间变更。
	New func() Counter
}

// Add 按 key 增加计数值；若 key 不存在则通过 g.New 创建。
//
// 读路径使用 RLock，仅当 key 不存在时升为写锁，避免常见路径的锁竞争。
// 升锁后再次检查 map：若已有其他 goroutine 创建了该 key，复用 map 中已存的
// 计数器，避免写入丢失。
func (g *CounterGroup) Add(key string, value int64) {
	g.mu.RLock()
	vec, ok := g.vecs[key]
	g.mu.RUnlock()
	if !ok {
		g.mu.Lock()
		if g.vecs == nil {
			g.vecs = make(map[string]Counter)
		}
		// 双检锁：升锁期间可能有其他 goroutine 已创建该 key，
		// 此时复用 map 中已存的计数器，保证所有写入落到同一实例。
		if vec, ok = g.vecs[key]; !ok {
			vec = g.New()
			g.vecs[key] = vec
		}
		g.mu.Unlock()
	}
	vec.Add(value)
}

// Value 按 key 获取当前值；不存在返回 0。
func (g *CounterGroup) Value(key string) int64 {
	g.mu.RLock()
	vec, ok := g.vecs[key]
	g.mu.RUnlock()
	if ok {
		return vec.Value()
	}
	return 0
}

// Reset 按 key 重置计数器。
func (g *CounterGroup) Reset(key string) {
	g.mu.RLock()
	vec, ok := g.vecs[key]
	g.mu.RUnlock()
	if ok {
		vec.Reset()
	}
}
