package model

import (
	"sync/atomic"
	"time"
)

// 本包所有时间都通过注入式时钟取值：租约到期、票据有效期、心跳新鲜度、禁止重连窗口
// 都必须同源，否则单测里无法构造「租约刚好过期」「票据刚好到点」这类边界场景。
// 默认实现是系统时间；SetClock 只在测试里注入假时钟，禁止业务代码调用。

// clockFunc 是一个取时间的函数指针类型，便于 atomic 存取。
type clockFunc func() time.Time

var nowFuncPtr atomic.Pointer[clockFunc]

func init() {
	sysClock := clockFunc(time.Now)
	nowFuncPtr.Store(&sysClock)
}

// SetClock 注入时钟，返回恢复函数（配合 defer restore() 使用）。
// 传 nil 等价于恢复系统时间。并发安全：内部用 atomic 指针，测试与运行期不会数据竞争。
func SetClock(f func() time.Time) (restore func()) {
	prev := nowFuncPtr.Load()
	var next clockFunc
	if f == nil {
		next = time.Now
	} else {
		next = clockFunc(f)
	}
	nowFuncPtr.Store(&next)
	return func() { nowFuncPtr.Store(prev) }
}

// nowUnix 本包统一取当前 Unix 秒（DB 时间列与 Redis TTL 判定都以它为准）。
func nowUnix() int64 { return (*nowFuncPtr.Load())().Unix() }

// NowUnix 返回当前 Unix 秒时间戳，供 repository/logic 跨包使用。
// 契约：所有落库时间字段都是 Unix 秒（见 rpc/livegateway.proto 头部说明）。
func NowUnix() int64 { return nowUnix() }

// NowMilli 返回当前 Unix 毫秒时间戳：心跳序号、RTT 观测与广播去重窗口需要亚秒精度。
func NowMilli() int64 { return (*nowFuncPtr.Load())().UnixNano() / int64(time.Millisecond) }

// Expired 判断一个 Unix 秒截止时刻是否已到期（0 视为「未设置」= 未过期，
// 与各 *_at 字段「0 表示未设置」的契约一致，避免把空字段判成过期）。
func Expired(at, now int64) bool { return at > 0 && at <= now }
