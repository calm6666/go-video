package repository

import (
	"testing"
	"time"
)

// TestDelayQueuePutDedupe 验证同一 mid 在队列中只保留最早入队的一条（参考
// PriorityQueue allowDuplicates=false 的 HashCode 去重语义）。
func TestDelayQueuePutDedupe(t *testing.T) {
	q := newDelayQueue()
	now := time.Now()

	q.Put(1, now.Add(5*time.Second))
	q.Put(1, now.Add(1*time.Second)) // 第二次入队应被忽略，保留 5s 的那条
	q.Put(2, now.Add(2*time.Second))

	if got := q.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2 (dedupe should keep one item per mid)", got)
	}
	mid, at, ok := q.Peek()
	if !ok {
		t.Fatal("Peek() = !ok, want ok")
	}
	if mid != 2 {
		t.Errorf("Peek() mid = %d, want 2 (earliest item first)", mid)
	}
	if want := now.Add(2 * time.Second); !at.Equal(want) {
		t.Errorf("Peek() at = %v, want %v", at, want)
	}
}

// TestDelayQueuePopExpired 验证按到期时间升序弹出到期任务，未到期任务保留。
func TestDelayQueuePopExpired(t *testing.T) {
	q := newDelayQueue()
	now := time.Now()

	q.Put(10, now.Add(4*time.Second))
	q.Put(20, now.Add(1*time.Second))
	q.Put(30, now.Add(6*time.Second)) // 未到期

	got := q.PopExpired(now.Add(5 * time.Second))
	want := []int64{20, 10}
	if len(got) != len(want) {
		t.Fatalf("PopExpired() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("PopExpired()[%d] = %d, want %d", i, got[i], want[i])
		}
	}
	if q.Len() != 1 {
		t.Errorf("Len() = %d, want 1 (only unexpired item left)", q.Len())
	}

	// 弹出后允许同一 mid 再次入队
	q.Put(20, now.Add(2*time.Second))
	if got := q.PopExpired(now.Add(3 * time.Second)); len(got) != 1 || got[0] != 20 {
		t.Errorf("PopExpired() after re-enqueue = %v, want [20]", got)
	}
	if got := q.PopExpired(now.Add(7 * time.Second)); len(got) != 1 || got[0] != 30 {
		t.Errorf("PopExpired() at +7s = %v, want [30]", got)
	}
}

// TestDelayQueueClose 验证关闭后拒绝入队。
func TestDelayQueueClose(t *testing.T) {
	q := newDelayQueue()
	q.Close()
	q.Put(1, time.Now())
	if q.Len() != 0 {
		t.Errorf("Len() = %d, want 0 after Close", q.Len())
	}
	if got := q.PopExpired(time.Now().Add(time.Hour)); len(got) != 0 {
		t.Errorf("PopExpired() = %v, want empty after Close", got)
	}
}
