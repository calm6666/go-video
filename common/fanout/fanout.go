// Package fanout 提供异步任务执行器，用固定数量的 worker 消费带缓冲 channel 中的任务。
//
// 适用于解耦主链路与旁路处理（如缓存写入、日志上报、指标记录）：
// 调用方通过 Do 异步投递任务，worker 在后台执行；
// channel 满时返回 ErrFull，避免 OOM；worker 执行任务时做 panic recover，不崩溃。
package fanout

import (
	"context"
	"errors"
	"runtime"
	"sync"

	"github.com/zeromicro/go-zero/core/logx"
)

// ErrFull 表示 channel 已满，任务投递失败。
var ErrFull = errors.New("fanout: chan full")

// options 是 fanout 的可配置项集合。
type options struct {
	worker int
	buffer int
}

// Option 是 fanout 配置函数。
type Option func(*options)

// Worker 设置 worker 数量。n 必须 > 0，否则 panic。
func Worker(n int) Option {
	if n <= 0 {
		panic("fanout: worker should > 0")
	}
	return func(o *options) {
		o.worker = n
	}
}

// Buffer 设置 channel 缓冲长度。n 必须 > 0，否则 panic。
func Buffer(n int) Option {
	if n <= 0 {
		panic("fanout: buffer should > 0")
	}
	return func(o *options) {
		o.buffer = n
	}
}

// item 是投递到 channel 的任务单元，携带任务函数与其上下文。
type item struct {
	f   func(c context.Context)
	ctx context.Context
}

// Fanout 异步任务执行器，从 channel 消费任务并在 worker 中执行。
type Fanout struct {
	name    string
	ch      chan item
	options *options
	waiter  sync.WaitGroup

	ctx    context.Context
	cancel func()
}

// New 创建一个 fanout 并启动后台 worker。
//
// name 用于日志标识，为空时回退为 "fanout"。
// 默认 worker=1、buffer=1024。
func New(name string, opts ...Option) *Fanout {
	if name == "" {
		name = "fanout"
	}
	o := &options{
		worker: 1,
		buffer: 1024,
	}
	for _, op := range opts {
		op(o)
	}
	c := &Fanout{
		ch:      make(chan item, o.buffer),
		name:    name,
		options: o,
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.waiter.Add(o.worker)
	for i := 0; i < o.worker; i++ {
		go c.proc()
	}
	return c
}

// proc 是 worker 主循环，从 channel 取任务执行，直到内部 Context 被取消。
func (c *Fanout) proc() {
	defer c.waiter.Done()
	for {
		select {
		case t := <-c.ch:
			wrapFunc(t.f)(t.ctx)
		case <-c.ctx.Done():
			return
		}
	}
}

// wrapFunc 包装任务函数，增加 panic recover，panic 通过 logx.Error 记录。
func wrapFunc(f func(c context.Context)) func(context.Context) {
	return func(ctx context.Context) {
		defer func() {
			if r := recover(); r != nil {
				buf := make([]byte, 64*1024)
				buf = buf[:runtime.Stack(buf, false)]
				logx.Errorf("fanout: panic in proc, err: %s, stack: %s", r, buf)
			}
		}()
		f(ctx)
	}
}

// Do 异步投递任务到 channel。
//
// f 为 nil 或 fanout 已关闭时返回错误；channel 满时返回 ErrFull。
// 传入的 ctx 直接透传给任务函数，不在 worker 侧派生新 Context。
func (c *Fanout) Do(ctx context.Context, f func(ctx context.Context)) (err error) {
	if f == nil || c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	select {
	case c.ch <- item{f: f, ctx: ctx}:
	default:
		err = ErrFull
	}
	return
}

// Close 关闭 fanout，等待所有 worker 退出。
//
// 重复调用返回内部 Context 已取消的错误。
func (c *Fanout) Close() error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	c.cancel()
	c.waiter.Wait()
	return nil
}
