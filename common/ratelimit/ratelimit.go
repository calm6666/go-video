// Package ratelimit 提供自适应限流与令牌桶限流工具，统一 Limiter 接口。
//
// 包内提供两种实现：
//   - AdaptiveLimiter：组合 TCP Vegas 自适应并发调节与 CoDel 受控延迟队列，
//     依据 RTT 采样动态调整窗口，适合保护下游服务。
//   - TokenBucket：基于 golang.org/x/time/rate 的标准令牌桶，适合固定 QPS 场景。
//
// 所有实现的 Allow 方法返回 (done, error)：返回 nil error 时调用方必须在请求
// 结束时调用 done 反馈结果（Success/Ignore/Drop），自适应限流器据此更新统计。
package ratelimit

import (
	"context"
	"errors"
)

// Op 表示一次允许的请求结束时调用方反馈给限流器的结果。
type Op int

const (
	// Success 表示请求成功完成，参与 RTT 采样与限流调节。
	Success Op = iota
	// Ignore 表示本次请求不计入统计（如客户端取消、上游错误），仅归还 inflight 计数。
	Ignore
	// Drop 表示请求被显式丢弃，触发限流器降低窗口。
	Drop
)

// Limiter 是所有限流器的统一接口。
//
// 调用方在收到 nil error 后，必须在请求结束时调用返回的 done 反馈结果；
// 若返回非 nil error，无需调用 done。
type Limiter interface {
	// Allow 尝试获取一个执行许可。
	// 返回的 done 在 nil error 时必须被调用，传入请求处理结果 Op。
	Allow(ctx context.Context) (func(Op), error)
}

// 限流器返回的哨兵错误，使用标准库 errors 定义，便于 errors.Is 比较。
var (
	// ErrLimitExceed 表示请求超过当前限流窗口或队列已满，应立即拒绝。
	ErrLimitExceed = errors.New("ratelimit: limit exceed")
	// ErrDeadline 表示请求在等待被允许前已超过截止时间。
	ErrDeadline = errors.New("ratelimit: deadline")
)
