package consumer

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-video/services/search-indexer/model"
)

func TestBackoffDelay_ExponentialWithCap(t *testing.T) {
	base := time.Second
	max := 30 * time.Minute
	want := []time.Duration{
		time.Second,      // attempt 1
		2 * time.Second,  // attempt 2
		4 * time.Second,  // attempt 3
		8 * time.Second,  // attempt 4
		16 * time.Second, // attempt 5
		32 * time.Second, // attempt 6
	}
	for i, w := range want {
		if got := BackoffDelay(i+1, base, max); got != w {
			t.Errorf("BackoffDelay(%d) = %v, want %v", i+1, got, w)
		}
	}
}

func TestBackoffDelay_ClampsAndGuardsOverflow(t *testing.T) {
	base := 5 * time.Second
	max := 30 * time.Minute

	// 首次与非法 attempt 都按基数等待，不会因为 0/负数变成「立刻疯狂重试」。
	for _, a := range []int{0, -3, 1} {
		if got := BackoffDelay(a, base, max); got != base {
			t.Errorf("BackoffDelay(%d) = %v, want %v", a, got, base)
		}
	}
	// 大位移必须按上限收敛，不能溢出成负数或 0。
	for _, a := range []int{14, 21, 40, 1000} {
		got := BackoffDelay(a, base, max)
		if got != max {
			t.Errorf("BackoffDelay(%d) = %v, want 上限 %v", a, got, max)
		}
	}
	// 任何输入都不能给出负值（负值会让 sleepCtx 立刻返回并空转刷 CPU）。
	for _, a := range []int{1, 2, 5, 19, 20, 21, 63} {
		if got := BackoffDelay(a, time.Hour, max); got < 0 {
			t.Fatalf("BackoffDelay(%d) = %v < 0", a, got)
		}
	}
}

func TestNextStateAndRetryDeadline(t *testing.T) {
	cases := []struct {
		attempts, maxRetries int
		want                 string
	}{
		{1, 5, StateRetry},
		{4, 5, StateRetry},
		{5, 5, StateDeadLetter}, // 达到上限即转死信，不再占用配额
		{6, 5, StateDeadLetter},
		{1, 1, StateDeadLetter}, // maxRetries<=1：首次失败即死信，但仍落库可查
		{0, 0, StateDeadLetter},
	}
	for _, c := range cases {
		if got := NextState(c.attempts, c.maxRetries); got != c.want {
			t.Errorf("NextState(%d,%d) = %s, want %s", c.attempts, c.maxRetries, got, c.want)
		}
		if got := RetryDeadlineReached(c.attempts, c.maxRetries); got != (c.want == StateDeadLetter) {
			t.Errorf("RetryDeadlineReached(%d,%d) = %v", c.attempts, c.maxRetries, got)
		}
	}
	// 状态字符串必须与 model 常量同源，否则落库状态与查询条件对不上。
	if StateRetry != model.OffsetStateRetry || StateDeadLetter != model.OffsetStateDeadLetter {
		t.Fatalf("状态常量与 model 不一致: %s/%s", StateRetry, StateDeadLetter)
	}
}

func TestPermanentErrorClassification(t *testing.T) {
	if permanent(nil) != nil {
		t.Fatal("permanent(nil) 必须是 nil")
	}
	base := errors.New("payload 违规")
	wrapped := permanent(base)
	if !isPermanent(wrapped) {
		t.Fatal("permanent 包装后必须识别为永久错误")
	}
	if !errors.Is(wrapped, base) {
		t.Fatal("必须保留 Unwrap 链，否则上层无法按域错误判定")
	}
	// 继续 %w 包装后仍需识别为永久（走 repository 的路径会再包一层）。
	if !isPermanent(fmt.Errorf("apply: %w", wrapped)) {
		t.Fatal("多层包装后仍须识别为永久错误")
	}
	// 网络抖动不是永久错误：必须走退避重试，否则一次超时就把事件打进死信。
	if isPermanent(fmt.Errorf("esclient: dial tcp: timeout")) {
		t.Fatal("临时错误不得判为永久")
	}
	if permanent(base).Error() != base.Error() {
		t.Fatal("包装不能改写错误文本")
	}
}

func TestFormatAttemptError(t *testing.T) {
	if got := formatAttemptError(1, nil); got != "" {
		t.Fatalf("成功时不应写失败原因，实际 %q", got)
	}
	got := formatAttemptError(3, errors.New("open search 失败"))
	if !strings.HasPrefix(got, "attempt=3 ") {
		t.Fatalf("失败摘要缺少尝试次数: %q", got)
	}
	long := strings.Repeat("y", 2000)
	truncated := formatAttemptError(2, errors.New(long))
	if len(truncated) > 600 || !strings.HasSuffix(truncated, "...") {
		t.Fatalf("失败摘要未截断（会撑爆 last_error 列），长度=%d", len(truncated))
	}
	// 落库摘要不能带堆栈式换行（list_error 列按单行文本设计）。
	if strings.Contains(truncated, "\n") {
		t.Fatal("摘要不应包含换行")
	}
}

func TestOptionsNormalize(t *testing.T) {
	opts := Options{MaxRetries: -1, InProcessAttempts: 0, BaseBackoff: 0, MaxBackoff: 0, RetryBatchLimit: 0, SweepIdleWait: 0}
	opts.normalize()
	d := DefaultOptions()
	if opts.MaxRetries != d.MaxRetries || opts.InProcessAttempts != d.InProcessAttempts {
		t.Fatalf("默认值未补齐: %+v", opts)
	}
	if opts.BaseBackoff <= 0 || opts.MaxBackoff <= 0 ||
		opts.RetryBatchLimit <= 0 || opts.SweepIdleWait <= 0 {
		t.Fatalf("归一化后仍存在非正参数，退避会变成忙等: %+v", opts)
	}
	if opts.MaxBackoff < opts.BaseBackoff {
		t.Fatalf("退避上限小于基数: %+v", opts)
	}

	// 进程内即时重试不得超过累计配额，否则会一次性耗尽重试次数。
	strict := Options{MaxRetries: 2, InProcessAttempts: 50, BaseBackoff: time.Millisecond, MaxBackoff: time.Second,
		RetryBatchLimit: 1, SweepIdleWait: time.Millisecond}
	strict.normalize()
	if strict.InProcessAttempts > strict.MaxRetries {
		t.Fatalf("InProcessAttempts=%d 应被夹到 MaxRetries=%d", strict.InProcessAttempts, strict.MaxRetries)
	}
}
