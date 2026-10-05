package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"go-video/common/idempotency"
	"go-video/services/notification/model"
)

// 退避与幂等策略。

// defaultBackoffSeconds 是配置缺失时的默认退避阶梯（秒）：
// 1 分钟、5 分钟、30 分钟、1 小时、6 小时，之后固定 6 小时。
var defaultBackoffSeconds = []int64{60, 300, 1800, 3600, 21600}

// NormalizeBackoff 清洗配置里的退避阶梯：去掉非正值，全部非法时回落默认阶梯。
func NormalizeBackoff(steps []int64) []int64 {
	out := make([]int64, 0, len(steps))
	for _, s := range steps {
		if s > 0 {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return append([]int64{}, defaultBackoffSeconds...)
	}
	return out
}

// BackoffDelay 返回第 retryCount 次失败后的等待时长。
// retryCount 从 1 开始；超出阶梯长度后复用最后一个值（不再无限指数增长）。
func BackoffDelay(retryCount int32, steps []int64) time.Duration {
	s := NormalizeBackoff(steps)
	if retryCount < 1 {
		retryCount = 1
	}
	idx := int(retryCount) - 1
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return time.Duration(s[idx]) * time.Second
}

// NextRetryAt 计算下次重试的 Unix 秒。
func NextRetryAt(now time.Time, retryCount int32, steps []int64) int64 {
	return now.Add(BackoffDelay(retryCount, steps)).Unix()
}

// RetryExhausted 判断是否已达重试上限（max<=0 表示不限制）。
func RetryExhausted(retryCount, max int32) bool {
	if max <= 0 {
		return false
	}
	return retryCount >= max
}

// GroupBizKey 返回请求级业务键：优先使用调用方的 biz_key，其次 idempotency_key。
// 两者都为空时拒绝请求 —— 没有幂等键的写接口不允许存在（AGENTS.md §5）。
func GroupBizKey(bizKey, idempotencyKey string) (string, error) {
	if k := strings.TrimSpace(bizKey); k != "" {
		return k, nil
	}
	if k := strings.TrimSpace(idempotencyKey); k != "" {
		return k, nil
	}
	return "", errors.New("notification/policy: biz_key or idempotency_key is required")
}

// RowBizKey 派生行级幂等键：同一请求里每个接收人（每个投递目标）一行，行级键由
// sha256(请求级键 : channel : 收件人标识 : 受控投递引用) 得到，长度固定 64 字符。
// 该函数是纯函数：相同输入必定命中 notification_delivery.uk_biz_key，从而实现跨实例去重。
// mid 与 target_ref 都参与派生：同一 mid 的多台设备（push）必须是不同任务行，
// 只按 mid 派生会让第二台设备被唯一索引挡掉、永远收不到通知。
func RowBizKey(groupKey string, channel int32, mid int64, targetRef string) (string, error) {
	if strings.TrimSpace(groupKey) == "" {
		return "", errors.New("notification/policy: group biz_key is empty")
	}
	if !model.IsValidChannel(channel) {
		return "", fmt.Errorf("notification/policy: %w channel=%d", model.ErrInvalidChannel, channel)
	}
	ref := strings.TrimSpace(targetRef)
	return idempotency.NewKey(groupKey, model.ChannelName(channel), RecipientKey(mid, ref), ref).String(), nil
}

// RecipientKey 返回接收人的稳定字符串标识（优先 mid，其次受控引用）。
func RecipientKey(mid int64, targetRef string) string {
	if mid > 0 {
		return fmt.Sprintf("mid:%d", mid)
	}
	ref := strings.TrimSpace(targetRef)
	if ref == "" {
		return "anonymous"
	}
	if len(ref) > 128 {
		ref = ref[:128]
	}
	return "ref:" + ref
}

// Digest 返回渲染结果的 sha256 hex 摘要（落 payload_digest，不落明文正文）。
// 标题长度做前缀，避免 ("ab","c") 与 ("a","bc") 得到同一摘要。
func Digest(title, body string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%d\n%s\n%s", len(title), title, body)
	return hex.EncodeToString(h.Sum(nil))
}
