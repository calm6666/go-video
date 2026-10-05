package model

import "time"

// nowUnix 返回当前 Unix 秒时间戳，供各 model 统一取时间。
//
// 会员域所有时间口径（expire_at、start_at、server_now）都是「Unix 秒」，
// 不做时区换算：套餐时长以「天」为最小授予单位，跨夏令时地区若按本地日历日累加
// 会产生 ±1 小时抖动，进而让到期判定随客户端时钟漂移。
func nowUnix() int64 { return time.Now().Unix() }

// NowUnix 供 logic 层取服务端当前秒，保证「判定依据」与「落库时间」同一时钟口径。
func NowUnix() int64 { return nowUnix() }

// SecondsPerDay 是一天的秒数，续期/收回的时长换算只用整数，不出现浮点。
const SecondsPerDay int64 = 86400

// NextExpireAt 计算一次授予后的到期时间（Unix 秒），是会员时长唯一的换算入口。
//
// 口径（会被逐条复查，别改）：
//   - 未过期（current > now）：在旧的 expire_at 之上顺延 current + delta*86400；
//   - 已过期（current <= now）：从 now 重新起算 now + delta*86400，
//     绝不从旧的 expire_at 往回减，否则「续费」会把用户已享受的时间吞掉。
//
// delta 为负即收回：结果不得早于 now，调用方按 max(结果, now) 收敛，
// 本函数不夹取下界，保留「扣回到 0 表示立即失效」的表达力。
func NextExpireAt(current, deltaSeconds, now int64) int64 {
	if current <= now {
		return now + deltaSeconds
	}
	return current + deltaSeconds
}

// MonthsForDeltaDays 把授予天数折算为「累计付费月数」增量（月卡 31 天=1 个月，
// 季卡 93=3、年卡 366=12），向上取整；只用于 paid_month_count 快照，不参与金额计算。
// 非正数返回 0，保证该计数单调不减。
func MonthsForDeltaDays(deltaDays int32) int32 {
	if deltaDays <= 0 {
		return 0
	}
	return (deltaDays + 30) / 31
}
