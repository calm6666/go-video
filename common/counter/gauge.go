package counter

import "sync/atomic"

// 编译期断言 gaugeCounter 实现 Counter。
var _ Counter = (*gaugeCounter)(nil)

// gaugeCounter 是基于 sync/atomic 的 gauge 计数器。
//
// 语义上表示可上下波动的瞬时值（如当前在线连接数、队列长度），
// 实现与原子计数器一致，区别仅在语义与命名。
type gaugeCounter int64

// NewGauge 返回一个 gauge 计数器。
func NewGauge() Counter {
	return new(gaugeCounter)
}

// Add 原子地调整 gauge 值，可正可负。
func (g *gaugeCounter) Add(val int64) {
	atomic.AddInt64((*int64)(g), val)
}

// Value 原子地读取当前 gauge 值。
func (g *gaugeCounter) Value() int64 {
	return atomic.LoadInt64((*int64)(g))
}

// Reset 原子地清零。
func (g *gaugeCounter) Reset() {
	atomic.StoreInt64((*int64)(g), 0)
}
