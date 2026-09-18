package ratelimit

import (
	"context"

	"golang.org/x/time/rate"
)

// 编译期断言 TokenBucket 实现 Limiter。
var _ Limiter = (*TokenBucket)(nil)

// TokenBucket 是基于标准令牌桶算法的限流器。
//
// 底层使用 golang.org/x/time/rate 提供的高性能实现，适合固定 QPS 场景。
// 与自适应限流器不同，令牌桶不依据 RTT 调节，参数在构造时固定。
type TokenBucket struct {
	limiter *rate.Limiter
}

// NewTokenBucket 创建一个令牌桶限流器。
//
// ratePerSec 是每秒填充的令牌数；burst 是桶容量（最大突发请求数）。
// 当 burst <= 0 或 ratePerSec <= 0 时，rate.Limiter 会按其语义处理
// （burst 为 0 表示无突发容量）。
func NewTokenBucket(ratePerSec, burst int) *TokenBucket {
	return &TokenBucket{
		limiter: rate.NewLimiter(rate.Limit(ratePerSec), burst),
	}
}

// Allow 实现 Limiter 接口。
//
// 与自适应限流器不同，令牌桶不阻塞等待：无可用令牌时立即返回 ErrLimitExceed。
// 若 ctx 已取消，返回 ErrDeadline。
// 返回的 done 为空操作，令牌已在 Allow 阶段消费，无需额外反馈。
func (t *TokenBucket) Allow(ctx context.Context) (func(Op), error) {
	if err := ctx.Err(); err != nil {
		return func(Op) {}, ErrDeadline
	}
	if !t.limiter.Allow() {
		return func(Op) {}, ErrLimitExceed
	}
	return func(Op) {}, nil
}
