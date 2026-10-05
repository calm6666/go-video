package model

import (
	"sync/atomic"
	"time"
)

// clock 是可注入时钟。生产环境保持 nil，所有取时间走 time.Now；
// 单测通过 SetClockForTest 换成固定/递进时钟，用来断言禁播到期、
// 场次时长簿记与 state_version 递增这类「跟时间有关」的边界，
// 而不需要 sleep 或造假实现（AGENTS.md §9）。
var clock atomic.Pointer[func() time.Time]

// SetClockForTest 注入测试时钟，返回的 restore 必须 defer 调用以还原全局状态。
// 仅供测试使用：并发跑同一包测试时请在用例里串行调用，避免互相污染。
func SetClockForTest(f func() time.Time) (restore func()) {
	prev := clock.Swap(&f)
	return func() { clock.Store(prev) }
}

// NowUnix 返回当前 Unix 秒时间戳，供 repository/logic 跨包使用。
func NowUnix() int64 {
	if f := clock.Load(); f != nil {
		return (*f)().Unix()
	}
	return time.Now().Unix()
}

// nowUnix 是包内统一取时间入口，禁止在 model 各处直接调 time.Now：
// 同一次写入里 ctime/mtime 必须来自同一时刻，否则时长与到期判定会出现自相矛盾的行。
func nowUnix() int64 { return NowUnix() }

// nowMillis 返回当前 Unix 毫秒时间戳（日志与游标排序用）。
func nowMillis() int64 {
	if f := clock.Load(); f != nil {
		return (*f)().UnixMilli()
	}
	return time.Now().UnixMilli()
}

// placeholders 生成 n 个以逗号连接的 "?"，用于 IN (...) 批量查询。
// 调用方必须保证 n > 0 并按顺序传入等量参数，禁止把用户输入拼进 SQL。
func placeholders(n int) string {
	if n <= 0 {
		return "?"
	}
	out := make([]byte, 0, n*3)
	for i := 0; i < n; i++ {
		if i > 0 {
			out = append(out, ',', ' ')
		}
		out = append(out, '?')
	}
	return string(out)
}
