package model

import (
	"strings"
	"sync/atomic"
	"time"
)

// clock 是可注入时钟。生产保持 nil 走 time.Now；测试用 SetClockForTest 换成固定时钟，
// 用来断言取消窗口边界与跨日重置，不需要 sleep（AGENTS.md §9 不许造假实现）。
var clock atomic.Pointer[func() time.Time]

// SetClockForTest 注入测试时钟，返回 restore 必须 defer 还原。仅供测试使用。
func SetClockForTest(f func() time.Time) (restore func()) {
	prev := clock.Swap(&f)
	return func() { clock.Store(prev) }
}

// Now 返回当前时间（本地时区）：日桶口径与 DSN 的 loc=Local 必须同源，
// 否则「今日」在服务端和 DB 里不是同一天。
func Now() time.Time {
	if f := clock.Load(); f != nil {
		return (*f)()
	}
	return time.Now()
}

// NowUnix 返回当前 Unix 秒时间戳，供 logic 跨包使用。
func NowUnix() int64 { return Now().Unix() }

// nowUnix 是包内统一取时间入口：同一次写入的 ctime/mtime 必须来自同一时刻。
func nowUnix() int64 { return Now().Unix() }

// DayNo 把时刻折算成日桶编号 YYYYMMDD（本地时区自然日）。
//
// 为什么用「(mid, date) 一行一天」而不是账户上的 today_xxx 列：
// 自增计数列要靠「读到 date != today 就清零」的写逻辑自己判断跨日，
// 那个判断在并发下会双写清零、且时区一变就整体错乱；
// 换成一天一行后跨日是天然发生的（新日期没有行 = 0），条件累加依旧无竞态。
func DayNo(t time.Time) int32 {
	return int32(t.Year()*10000 + int(t.Month())*100 + t.Day())
}

// TodayDayNo 返回「现在」的日桶编号。
func TodayDayNo() int32 { return DayNo(Now()) }

// placeholders 生成 n 个以逗号连接的 "?"，供 IN (...) 批量查询使用。
// 调用方必须保证 n > 0 并按顺序传入等量参数，禁止把外部输入拼进 SQL。
func placeholders(n int) string {
	if n <= 0 {
		return "?"
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
