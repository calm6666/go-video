// Package timeutil 提供 MySQL 友好的时间戳包装与 Duration 工具。
package timeutil

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	stdtime "time"
)

// Time 是以 Unix 秒为单位的时间戳，支持从数据库列扫描并进行 driver 值转换。
type Time int64

// Scan 支持 time.Time、string、[]byte 和 int64 输入。
// 其他类型返回错误，以便尽早暴露 schema 不匹配。
func (t *Time) Scan(src any) error {
	if t == nil {
		return errors.New("timeutil: Scan receiver is nil")
	}
	switch v := src.(type) {
	case nil:
		*t = 0
	case stdtime.Time:
		*t = Time(v.Unix())
	case string:
		if v == "" {
			*t = 0
			return nil
		}
		i, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("timeutil: parse %q as int64: %w", v, err)
		}
		*t = Time(i)
	case []byte:
		s := strings.TrimSpace(string(v))
		if s == "" {
			*t = 0
			return nil
		}
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return fmt.Errorf("timeutil: parse %q as int64: %w", s, err)
		}
		*t = Time(i)
	case int64:
		*t = Time(v)
	case uint64:
		*t = Time(int64(v))
	default:
		return fmt.Errorf("timeutil: unsupported Scan source type %T", src)
	}
	return nil
}

// Value 返回 time.Time 类型的 driver 值。零值 Time 映射到零值 time.Time。
func (t Time) Value() (driver.Value, error) {
	return t.Time(), nil
}

// Time 转换为 UTC 时区的标准库 time.Time。
func (t Time) Time() stdtime.Time {
	if t == 0 {
		return stdtime.Time{}
	}
	return stdtime.Unix(int64(t), 0).UTC()
}

// Duration 包装 time.Duration，便于 YAML/TOML 解码器从 "500ms"、"1s" 等字符串解析。
type Duration stdtime.Duration

// UnmarshalText 解析 duration 字符串。空输入解析为零值。
func (d *Duration) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := stdtime.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("timeutil: parse %q as duration: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Shrink 返回 d 与 ctx 截止时间剩余值中较小的一个。
// 若 ctx 没有截止时间，则返回 d 并附带一个携带 d 超时的子 context。
// 调用方必须调用返回的 cancel 函数。
//
// 该方法用于避免下游 RPC 调用超过上游截止时间。
func (d Duration) Shrink(ctx context.Context) (Duration, context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := stdtime.Until(deadline); remaining < stdtime.Duration(d) {
			return Duration(remaining), ctx, func() {}
		}
	}
	sctx, cancel := context.WithTimeout(ctx, stdtime.Duration(d))
	return d, sctx, cancel
}

// nowFuncPtr 保存当前时间提供者。测试可通过 SetNowFunc 替换；
// 生产代码不应修改。
var nowFuncPtr atomic.Pointer[func() stdtime.Time]

func defaultNow() stdtime.Time { return stdtime.Now() }

func init() {
	f := defaultNow
	nowFuncPtr.Store(&f)
}

// Now 返回当前活跃时间提供者的时间。可并发安全调用。
func Now() stdtime.Time {
	f := nowFuncPtr.Load()
	if f == nil {
		return stdtime.Now()
	}
	return (*f)()
}

// NowMilli 返回当前时间的 Unix 毫秒值。
func NowMilli() int64 {
	return Now().UnixMilli()
}

// SetNowFunc 替换 Now 使用的时间提供者。传入 nil 恢复默认实现。
// 仅供测试使用。
func SetNowFunc(f func() stdtime.Time) {
	if f == nil {
		f = defaultNow
	}
	nowFuncPtr.Store(&f)
}

// ParseRFC3339 解析严格 RFC3339 格式的时间字符串。
func ParseRFC3339(s string) (stdtime.Time, error) {
	t, err := stdtime.Parse(stdtime.RFC3339, s)
	if err != nil {
		return stdtime.Time{}, fmt.Errorf("timeutil: parse %q as RFC3339: %w", s, err)
	}
	return t, nil
}

// FormatRFC3339 以 UTC RFC3339 格式化 t。零值时间返回空字符串。
func FormatRFC3339(t stdtime.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(stdtime.RFC3339)
}
