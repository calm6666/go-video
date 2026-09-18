// Package ecode 提供唯一业务错误码的注册表，附带人类可读消息。
//
// Code 值不可变且可比较，可作为 map key 或哨兵 error 使用。
// 注册是进程级全局的；服务应在初始化阶段（如 ServiceContext 中）一次性注册自己的错误码。
package ecode

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
)

// 哨兵码，与 common/httpresponse 的数值常量保持一致。
var (
	// OK 是成功码，也是 Code 的零值。
	OK = Code(0)

	// ServerErr 是未识别服务端错误的兜底码，
	// 对应 httpresponse.CodeInternalError。
	ServerErr = Code(50000)
)

var (
	// messages 保存已注册的 code 到 message 映射。
	// 通过 atomic.Value 存储，注册完成后的并发读无需加锁。
	messages atomic.Value // map[int]string

	// codes 记录已注册的 code 值，避免 New 重复分配。
	mu    sync.RWMutex
	codes = map[int]struct{}{}
)

func init() {
	// 初始化 messages 为内置哨兵码消息，保证 Message() 在任何 Register 之前都能工作。
	messages.Store(map[int]string{
		0:     "ok",
		50000: "internal error",
	})
}

// Register 替换全局 code 到 message 的映射。该调用不是累加的：
// 后一次调用会覆盖前一次。服务应在启动时传入完整的映射。
func Register(m map[int]string) {
	if m == nil {
		m = map[int]string{}
	}
	merged := make(map[int]string, len(m)+2)
	merged[0] = "ok"
	merged[50000] = "internal error"
	for k, v := range m {
		merged[k] = v
	}
	messages.Store(merged)
}

// New 分配一个唯一的业务码。当 e <= 0 或已被占用时会 panic。
// 服务自定义码应在初始化阶段通过 New 分配；运行时解析外部输入应使用 Int，
// 它不进行唯一性校验。
func New(e int) Code {
	if e <= 0 {
		panic(fmt.Sprintf("ecode: business code must be greater than zero, got %d", e))
	}
	mu.Lock()
	defer mu.Unlock()
	if _, exists := codes[e]; exists {
		panic(fmt.Sprintf("ecode: code %d already registered", e))
	}
	codes[e] = struct{}{}
	return Code(e)
}

// Int 构造 Code 但不做唯一性校验。
// 用于解析外部输入（如上游 RPC 响应中的码），允许重复值。
func Int(i int) Code { return Code(i) }

// String 将错误字符串解析为 Code。
// 空输入返回 OK；非数字字符串返回 ServerErr，
// 保证调用方总能拿到一个 Code。
func String(s string) Code {
	if s == "" {
		return OK
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		return ServerErr
	}
	return Code(i)
}

// Code 是以整数标识的值类型 error，实现了 error 接口。
type Code int

// Error 返回 code 的十进制字符串形式。
// 仅用于日志和监控，不应作为面向用户的消息；用户消息请使用 Message。
func (c Code) Error() string {
	return strconv.FormatInt(int64(c), 10)
}

// Code 返回底层的整数值。
func (c Code) Code() int { return int(c) }

// Message 返回已注册的消息；若未注册则返回 code 的十进制字符串。
func (c Code) Message() string {
	if m, ok := messages.Load().(map[int]string); ok {
		if msg, found := m[int(c)]; found {
			return msg
		}
	}
	return c.Error()
}

// Err 返回携带 c 和可选 details 的 *Error。
// 当需要附带结构化详情（如审核证据引用）供消费方通过 Details 检查时使用。
func (c Code) Err(details ...any) *Error {
	return &Error{code: c, details: details}
}

// Equal 判断两个 code 是否具有相同的整数值。
func Equal(a, b Code) bool { return a.Code() == b.Code() }

// EqualError 判断 err 是否携带指定 code。nil err 视为 OK。
func EqualError(code Code, err error) bool {
	return Cause(err).Code() == code.Code()
}

// Error 是携带 Code 和可选 details 的类型化错误。
// 实现 Unwrap 以便 errors.Is/errors.As 能够遍历错误链。
type Error struct {
	code    Code
	details []any
	cause   error
}

// Error 返回 "<code>: <message>" 格式的字符串。
func (e *Error) Error() string {
	if e == nil {
		return OK.Message()
	}
	return fmt.Sprintf("%d: %s", e.code.Code(), e.code.Message())
}

// Code 返回底层 Code。
func (e *Error) Code() Code {
	if e == nil {
		return OK
	}
	return e.code
}

// Message 返回所包装 Code 已注册的消息。
func (e *Error) Message() string {
	if e == nil {
		return OK.Message()
	}
	return e.code.Message()
}

// Details 返回构造时附带的结构化详情，可能为空。
func (e *Error) Details() []any {
	if e == nil {
		return nil
	}
	return e.details
}

// Unwrap 返回内部 cause（若有）。
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Wrap 返回一个新的 Error，使用接收者的 code 包装 cause。
func (e *Error) Wrap(cause error) *Error {
	if e == nil {
		return nil
	}
	return &Error{code: e.code, details: e.details, cause: cause}
}

// Cause 提取 err 错误链中第一个 Code。nil err 返回 OK。
// 若错误链中不存在 Code，则返回 ServerErr。
func Cause(err error) Code {
	if err == nil {
		return OK
	}
	var ec *Error
	if errors.As(err, &ec) {
		return ec.Code()
	}
	var c Code
	if errors.As(err, &c) {
		return c
	}
	return ServerErr
}
