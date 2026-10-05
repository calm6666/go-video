package policy

// Window 是一次固定窗口限流判定的输入。
// Count 是窗口内已发生的次数（含本次，Redis INCR 后的值）。
type Window struct {
	// Dimension 是维度名，用于错误信息与日志（如 user、oid）。
	Dimension string
	// Count 是窗口内计数。
	Count int32
	// Limit 是窗口阈值；<= 0 表示该维度不限流。
	Limit int32
}

// CheckWindows 对多个固定窗口维度做判定，任一维度超阈值即拒绝。
// 返回首个触发的维度名，便于 gateway 映射为可读错误。
//
// 窗口计数由调用方（repository）用 Redis INCR + EXPIRE 维护，
// 本函数只负责阈值判定，保证规则可单测。
// 进程级 QPS 保护复用 common/ratelimit 的 TokenBucket，不在此重复实现。
func CheckWindows(windows ...Window) (bool, string) {
	for _, w := range windows {
		if w.Limit <= 0 {
			continue
		}
		if w.Count > w.Limit {
			return false, w.Dimension
		}
	}
	return true, ""
}
