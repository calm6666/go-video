package ratelimit

import (
	"context"
	"time"
)

// 编译期断言 AdaptiveLimiter 实现 Limiter。
var _ Limiter = (*AdaptiveLimiter)(nil)

// AdaptiveLimiter 组合 Vegas 与 CoDel，提供自适应限流。
//
// 工作流程：
//  1. 调用 Vegas.Acquire 获取槽位；若在当前并发上限内直接通过。
//  2. 超过上限的请求进入 CoDel 队列等待，队列满或超时则拒绝。
//  3. 请求结束时调用 done 反馈结果，驱动 Vegas 调节并发上限并唤醒队列下一条。
//
// 与原实现相比，本实现不内置每秒日志 goroutine，统计通过 Stat() 按需采集，
// 由调用方决定输出方式（如写入 go-zero logx 或 Prometheus）。
type AdaptiveLimiter struct {
	rate  *Vegas
	queue *CoDelQueue
}

// NewAdaptiveLimiter 创建自适应限流器。
// nil 配置使用 CoDel 默认配置。
func NewAdaptiveLimiter(conf *CoDelConfig) *AdaptiveLimiter {
	return &AdaptiveLimiter{
		rate:  NewVegas(),
		queue: NewCoDelQueue(conf),
	}
}

// AdaptiveStat 是自适应限流器的合并统计，便于一次性采集。
type AdaptiveStat struct {
	Vegas VegasStat
	CoDel CoDelStat
}

// Stat 返回 Vegas 与 CoDel 的合并统计。
func (l *AdaptiveLimiter) Stat() AdaptiveStat {
	return AdaptiveStat{
		Vegas: l.rate.Stat(),
		CoDel: l.queue.Stat(),
	}
}

// Allow 实现 Limiter 接口。
//
// 超过 Vegas 当前并发上限的请求会进入 CoDel 队列等待；
// 队列满或等待超时则返回错误（ErrLimitExceed / ErrDeadline）。
// 返回 nil error 时，调用方必须在请求结束时调用 done 反馈 Op。
func (l *AdaptiveLimiter) Allow(ctx context.Context) (func(Op), error) {
	done, ok := l.rate.Acquire()
	if !ok {
		// 超过当前 inflight 上限，进入 CoDel 队列等待。
		if err := l.queue.Push(ctx); err != nil {
			// 即使被拒绝也要调用 done 归还 inflight，并以 Ignore 标记不污染统计。
			done(time.Time{}, Ignore)
			return func(Op) {}, err
		}
	}
	start := time.Now()
	return func(op Op) {
		done(start, op)
		// 唤醒队列中的下一个等待方（若有）。
		l.queue.Pop()
	}, nil
}
