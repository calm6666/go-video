package counter

import (
	"sync"
	"time"
)

// bucket 是滑动窗口的一个时间桶，通过 next 组成环形链表。
type bucket struct {
	val  int64
	next *bucket
}

// Add 累加桶值。调用方需自行持锁。
func (b *bucket) Add(val int64) {
	b.val += val
}

// Value 返回桶当前值。调用方需自行持锁。
func (b *bucket) Value() int64 {
	return b.val
}

// Reset 清零桶值。调用方需自行持锁。
func (b *bucket) Reset() {
	b.val = 0
}

// 编译期断言 rollingCounter 实现 Counter。
var _ Counter = (*rollingCounter)(nil)

// rollingCounter 是滑动窗口计数器。
//
// 窗口按时间划分为 winBuckets 个桶，每个桶覆盖 window/winBuckets 时长。
// Add 时按当前时间推进游标并重置过期桶；Value 跳过已过期桶仅累计有效桶。
// 桶通过 next 指针组成环形链表，避免切片扩容与索引计算开销。
type rollingCounter struct {
	mu         sync.RWMutex
	buckets    []bucket
	bucketTime int64 // 每桶时长（纳秒）
	lastAccess int64 // 上次推进游标的时间（纳秒）
	cur        *bucket
}

// NewRolling 创建滑动窗口计数器。
//
// window 是整个窗口覆盖的时间跨度；winBuckets 是窗口被划分的桶数。
// 例如 window=1s、winBuckets=10 表示每桶 100ms，窗口累计最近 1 秒的值。
func NewRolling(window time.Duration, winBuckets int) Counter {
	buckets := make([]bucket, winBuckets)
	b := &buckets[0]
	// 构造环形链表。
	for i := 1; i < winBuckets; i++ {
		b.next = &buckets[i]
		b = b.next
	}
	b.next = &buckets[0]
	bucketTime := time.Duration(window.Nanoseconds() / int64(winBuckets))
	return &rollingCounter{
		cur:        &buckets[0],
		buckets:    buckets,
		bucketTime: int64(bucketTime),
		lastAccess: time.Now().UnixNano(),
	}
}

// Add 推进游标到当前时间桶并累加值。
func (r *rollingCounter) Add(val int64) {
	r.mu.Lock()
	r.lastBucket().Add(val)
	r.mu.Unlock()
}

// Value 返回窗口内所有有效桶的累计值。
//
// 已过期的桶会被 elapsed 计数跳过；这些桶将在下一次 Add 时被 lastBucket 重置。
func (r *rollingCounter) Value() (sum int64) {
	now := time.Now().UnixNano()
	r.mu.RLock()
	b := r.cur
	i := r.elapsed(now)
	for j := 0; j < len(r.buckets); j++ {
		// 跳过已过期桶（位于当前桶之前的若干个）。
		if i > 0 {
			i--
		} else {
			sum += b.Value()
		}
		b = b.next
	}
	r.mu.RUnlock()
	return
}

// Reset 重置所有桶并清零当前游标位置。
func (r *rollingCounter) Reset() {
	r.mu.Lock()
	for i := range r.buckets {
		r.buckets[i].Reset()
	}
	r.mu.Unlock()
}

// elapsed 返回自上次访问以来经过的桶数，超过桶总数则截断为桶总数。
func (r *rollingCounter) elapsed(now int64) (i int) {
	var e int64
	if e = now - r.lastAccess; e <= r.bucketTime {
		return
	}
	if i = int(e / r.bucketTime); i > len(r.buckets) {
		i = len(r.buckets)
	}
	return
}

// lastBucket 推进游标到当前时间对应的桶，并重置途经的过期桶。
// 调用方需持有写锁。
func (r *rollingCounter) lastBucket() (b *bucket) {
	now := time.Now().UnixNano()
	b = r.cur
	if i := r.elapsed(now); i > 0 {
		r.lastAccess = now
		// 向前推进 i 个桶，每个途经的桶被重置后用于新数据。
		for ; i > 0; i-- {
			b = b.next
			b.Reset()
		}
	}
	r.cur = b
	return
}
