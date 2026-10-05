package model

import "time"

// nowUnix 统一由 model 层取时间，避免各调用方各自读时钟导致同一次资金变更的
// 单据时间与流水时间不一致。
func nowUnix() int64 { return time.Now().Unix() }

// settledAtOrNow 归一结算时间：调用方给 0（或负数）时取当前时间，
// 避免出现「状态已 SUCCESS 但 settled_at 仍是 0」这种自相矛盾的台账行。
func settledAtOrNow(ts int64) int64 {
	if ts > 0 {
		return ts
	}
	return nowUnix()
}
