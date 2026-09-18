// Package errgroup 提供一组子任务并行执行时的同步、错误传播与 Context 取消能力。
//
// 在标准库 errgroup 思路基础上增强：支持 GOMAXPROCS 限制并发 worker 数量，
// 并在 worker 执行任务时做 panic recover，将 panic 包装为 error 返回，避免进程崩溃。
package errgroup

import (
	"context"
	"fmt"
	"runtime"
	"sync"
)

// Group 是一组共同任务的子任务集合。
//
// 零值 Group 可用且不会在出错时取消。
type Group struct {
	err     error
	wg      sync.WaitGroup
	errOnce sync.Once

	workerOnce sync.Once
	ch         chan func() error
	chs        []func() error

	cancel func()
}

// WithContext 返回一个新 Group 和由 ctx 派生的 Context。
//
// 派生 Context 在首次有函数返回非 nil 错误或 Wait 返回时（以先到者为准）被取消。
func WithContext(ctx context.Context) (*Group, context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	return &Group{cancel: cancel}, ctx
}

// do 执行单个任务，做 panic recover 并处理错误传播与 WaitGroup 计数。
func (g *Group) do(f func() error) {
	var err error
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 64<<10)
			buf = buf[:runtime.Stack(buf, false)]
			err = fmt.Errorf("errgroup: panic recovered: %s\n%s", r, buf)
		}
		if err != nil {
			g.errOnce.Do(func() {
				g.err = err
				if g.cancel != nil {
					g.cancel()
				}
			})
		}
		g.wg.Done()
	}()
	err = f()
}

// GOMAXPROCS 设置最大并发 worker 数。
//
// n 必须 > 0，否则 panic。只能在首次 Go 之前调用一次，重复调用会被忽略。
func (g *Group) GOMAXPROCS(n int) {
	if n <= 0 {
		panic("errgroup: GOMAXPROCS must great than 0")
	}
	g.workerOnce.Do(func() {
		g.ch = make(chan func() error, n)
		for i := 0; i < n; i++ {
			go func() {
				for f := range g.ch {
					g.do(f)
				}
			}()
		}
	})
}

// Go 在新 goroutine（或 worker）中执行给定函数。
//
// 首个返回非 nil 错误的函数会取消 Group，其错误由 Wait 返回。
func (g *Group) Go(f func() error) {
	g.wg.Add(1)
	if g.ch != nil {
		select {
		case g.ch <- f:
		default:
			// worker channel 暂时满，暂存到本地切片，由 Wait 补投。
			g.chs = append(g.chs, f)
		}
		return
	}
	go g.do(f)
}

// Wait 阻塞直到所有通过 Go 提交的函数返回，然后返回其中的首个非 nil 错误。
func (g *Group) Wait() error {
	if g.ch != nil {
		// 补投暂存的任务，确保不丢失。
		for _, f := range g.chs {
			g.ch <- f
		}
	}

	g.wg.Wait()
	if g.ch != nil {
		// 关闭 worker channel，让所有 worker 退出。
		close(g.ch)
	}
	if g.cancel != nil {
		g.cancel()
	}
	return g.err
}
