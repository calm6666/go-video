package model

import (
	"strings"
	"sync/atomic"
	"time"
)

// clock 是可注入时钟。生产环境保持 nil，所有取时间走 time.Now。
// 单测用 SetClockForTest 换成固定/递进时钟，用来断言 TTL 到期、PREVIOUS_VERSION 回退、
// 回填作业租约与「乱序写不覆盖新值」这类跟时间有关的边界，
// 不需要 sleep，也不把依赖换成永不失败的假实现（AGENTS.md §9）。
var clock atomic.Pointer[func() time.Time]

// SetClockForTest 注入测试时钟，返回的 restore 必须 defer 调用以还原全局状态。
// 仅供测试使用：同包并发跑用例时请串行调用，避免互相污染。
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

// nowUnix 是 model 包内唯一取时间入口：禁止在各文件里直接调 time.Now。
// 同一次写入的 ctime/mtime/expire_at 必须来自同一时刻，否则「mtime 比 ctime 早」
// 「expire_at 与 ttl_seconds 不自洽」这类自相矛盾的行会写进库里。
func nowUnix() int64 { return NowUnix() }

// placeholders 生成 n 个以逗号连接的 "?"，用于 IN (...) 批量查询与多列 INSERT。
// 调用方必须保证 n > 0 并按顺序传入等量参数，禁止把用户输入拼进 SQL 串。
func placeholders(n int) string {
	if n <= 0 {
		return "?"
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
