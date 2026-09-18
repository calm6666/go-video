package repository

// 本文件移植自参考仓库 openbilibili-go-common/app/service/main/account/model/queue 的
// PriorityQueue 行为（Workiva go-datastructures，Apache 2.0）：按时间排序的优先级队列，
// 以 mid 为去重键（HashCode），弹出时释放去重键。参考实现依赖阻塞式 sema 唤醒，
// 本项目消费侧使用定时器批量弹出，因此仅移植堆与去重语义，不移植阻塞等待部分。

import (
	"container/heap"
	"sync"
	"time"
)

// delayItem 是一条延迟缓存失效任务：mid 到期后需要再次失效缓存并回温。
type delayItem struct {
	// mid 用户 ID
	mid int64
	// at 到期时间，队列按 at 升序排列（最小堆）
	at time.Time
}

// delayHeap 是 delayItem 的最小堆，堆顶为最早到期的任务。
type delayHeap []*delayItem

func (h delayHeap) Len() int { return len(h) }

// Less 比较两个任务的到期时间，实现最小堆。
func (h delayHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }

func (h delayHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *delayHeap) Push(x any) { *h = append(*h, x.(*delayItem)) }

func (h *delayHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil // 释放引用，避免内存泄漏
	*h = old[:n-1]
	return item
}

// delayQueue 是缓存延迟失效队列。
// 与参考实现一致：同一 mid 在队列中只保留一条（保留最早入队的一条），
// 弹出后允许再次入队。
type delayQueue struct {
	mu      sync.Mutex
	h       delayHeap
	pending map[int64]struct{} // 已在队列中的 mid 集合，用于去重
	closed  bool
}

// newDelayQueue 构造延迟队列。
func newDelayQueue() *delayQueue {
	return &delayQueue{
		pending: make(map[int64]struct{}),
	}
}

// Put 入队一条任务。若 mid 已在队列中则忽略（参考 PriorityQueue.Put 的
// allowDuplicates=false 语义，保留最早入队时间）。
func (q *delayQueue) Put(mid int64, at time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if _, ok := q.pending[mid]; ok {
		return
	}
	q.pending[mid] = struct{}{}
	heap.Push(&q.h, &delayItem{mid: mid, at: at})
}

// Peek 查看最早到期的任务，不弹出。
func (q *delayQueue) Peek() (mid int64, at time.Time, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.h) == 0 {
		return 0, time.Time{}, false
	}
	top := q.h[0]
	return top.mid, top.at, true
}

// PopExpired 弹出所有到期时间不晚于 now 的任务，返回 mid 列表（按到期时间升序）。
func (q *delayQueue) PopExpired(now time.Time) []int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	var mids []int64
	for q.h.Len() > 0 && !q.h[0].at.After(now) {
		item := heap.Pop(&q.h).(*delayItem)
		delete(q.pending, item.mid)
		mids = append(mids, item.mid)
	}
	return mids
}

// Len 返回队列中未到期的任务数。
func (q *delayQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.h.Len()
}

// Close 关闭队列：拒绝后续入队并清空任务。
func (q *delayQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.h = nil
	q.pending = nil
}
