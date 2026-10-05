// retry.go 是事件消费的重试/死信状态机（纯函数 + 错误分类），
// 与消息队列实现解耦，可在无网络环境下单测。
package consumer

import (
	"errors"
	"fmt"
	"time"
)

// 状态机常量（与 model.OffsetState* 一致，避免本包反向依赖 model 的字符串）。
const (
	StateRetry      = "retry"
	StateDeadLetter = "dead_letter"
)

// permanentError 表示重试不会成功的错误：payload 违规、未知 action、
// 缺版本号、mapping 不兼容等。这类事件直接进死信，避免占用重试配额。
type permanentError struct {
	err error
}

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// permanent 包装永久错误。
func permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// isPermanent 判断是否永久错误。
func isPermanent(err error) bool {
	var pe *permanentError
	return errors.As(err, &pe)
}

// BackoffDelay 计算第 attempt 次重试前的等待时间：base * 2^(attempt-1)，上限 max。
//
// attempt 从 1 开始；<=0 视为 1。指数退避保证 OpenSearch 抖动时
// 不会把重试风暴打回上游，上限保证事件最终会被处理而不会无限延后。
func BackoffDelay(attempt int, base, max time.Duration) time.Duration {
	if attempt <= 1 {
		return clampDelay(base, max)
	}
	shift := attempt - 1
	if shift > 20 {
		// 2^20 * base 早已超过常规上限，直接按上限返回，避免溢出。
		return clampDelay(max, max)
	}
	d := base << shift
	if d <= 0 || d > max { // 溢出或超界
		return clampDelay(max, max)
	}
	return d
}

func clampDelay(d, max time.Duration) time.Duration {
	if max > 0 && d > max {
		return max
	}
	if d < 0 {
		return 0
	}
	return d
}

// NextState 决定失败后的下一状态：累计尝试次数达到上限即转 dead_letter。
// maxRetries<=0 表示首次失败即死信（不静默丢弃，仍然落库可查）。
func NextState(attempts, maxRetries int) string {
	if attempts >= maxRetries {
		return StateDeadLetter
	}
	return StateRetry
}

// RetryDeadlineReached 便于调用方判断是否应转死信。
func RetryDeadlineReached(attempts, maxRetries int) bool {
	return NextState(attempts, maxRetries) == StateDeadLetter
}

// formatAttemptError 生成落库的失败摘要（脱敏、限长，不含堆栈）。
func formatAttemptError(attempt int, err error) string {
	if err == nil {
		return ""
	}
	msg := fmt.Sprintf("attempt=%d err=%s", attempt, err.Error())
	if len(msg) > 500 {
		msg = msg[:500] + "..."
	}
	return msg
}
