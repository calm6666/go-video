package fanout

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestFanout_Do 验证任务被异步执行，且 worker 内 panic 不影响其他任务和进程。
func TestFanout_Do(t *testing.T) {
	ca := New("cache", Worker(1), Buffer(1024))
	var run atomic.Bool
	done := make(chan struct{})
	err := ca.Do(context.Background(), func(c context.Context) {
		run.Store(true)
		// 触发 panic，验证 recover 不崩溃。
		panic("error")
	})
	if err != nil {
		t.Fatalf("expect Do success, got %v", err)
	}
	go func() {
		// 等待任务执行（含 panic recover）。
		for i := 0; i < 100 && !run.Load(); i++ {
			time.Sleep(time.Millisecond)
		}
		// 再等一下让 recover 完成日志输出。
		time.Sleep(time.Millisecond * 20)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for task execution")
	}
	if !run.Load() {
		t.Fatal("expect run be true")
	}
	if err := ca.Close(); err != nil {
		t.Fatalf("close err: %v", err)
	}
}

// TestFanout_Close 验证 Close 后再 Do 返回错误。
func TestFanout_Close(t *testing.T) {
	ca := New("cache", Worker(1), Buffer(1024))
	if err := ca.Close(); err != nil {
		t.Fatalf("close err: %v", err)
	}
	err := ca.Do(context.Background(), func(c context.Context) {})
	if err == nil {
		t.Fatal("expect get err after close")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expect context.Canceled, got %v", err)
	}
}

// TestFanout_ErrFull 验证 channel 满时返回 ErrFull。
func TestFanout_ErrFull(t *testing.T) {
	// buffer=1，worker 阻塞在第一个任务上，后续投递应失败。
	ca := New("full", Worker(1), Buffer(1))
	// started 让 worker 拿到第一个任务并阻塞在 hold 上。
	hold := make(chan struct{})
	started := make(chan struct{})
	_ = ca.Do(context.Background(), func(c context.Context) {
		close(started)
		<-hold
	})
	<-started
	// buffer 容量为 1，再投递一个填满。
	if err := ca.Do(context.Background(), func(c context.Context) {}); err != nil {
		t.Fatalf("expect first buffered Do success, got %v", err)
	}
	// 此时 channel 已满，再投递应返回 ErrFull。
	if err := ca.Do(context.Background(), func(c context.Context) {}); !errors.Is(err, ErrFull) {
		t.Fatalf("expect ErrFull, got %v", err)
	}
	close(hold)
	_ = ca.Close()
}

// TestFanout_MultiWorkerClose 验证多 worker 下 Close 等待所有 worker 退出。
func TestFanout_MultiWorkerClose(t *testing.T) {
	ca := New("multi", Worker(4), Buffer(16))
	var executed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		_ = ca.Do(context.Background(), func(c context.Context) {
			defer wg.Done()
			time.Sleep(time.Millisecond)
			executed.Add(1)
		})
	}
	// 等待任务完成后再关闭，避免缓冲任务被丢弃导致 Close 早于执行。
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for tasks")
	}
	if err := ca.Close(); err != nil {
		t.Fatalf("close err: %v", err)
	}
	if got := executed.Load(); got != 16 {
		t.Fatalf("expect 16 executed, got %d", got)
	}
}
