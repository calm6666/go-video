// Package idgen 生成全局唯一且时间有序的标识符，
// 用于 go-video 中的 event_id、idempotency_key、request_id 等字段。
//
// 实现基于 github.com/oklog/ulid/v2，
// 该库符合 ULID 规范、支持单调性、提供无锁熵源。
// 所有标识符均为 26 字符的 Crockford Base32，前 48 位为 Unix 毫秒时间戳。
package idgen

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/oklog/ulid/v2"
)

// Generator 生成 ID 的接口。
// 测试可通过该接口替换为确定性实现。
type Generator interface {
	// ULID 返回 26 字符的 Crockford Base32 标识符。
	ULID() (string, error)
	// Short 返回 n 字节随机数据，编码为去填充的 Base64URL。
	// 输出长度约为 ceil(n*4/3) 字符。
	Short(n int) (string, error)
	// Prefixed 返回 "<prefix>_<ULID>"。
	Prefixed(prefix string) (string, error)
}

// defaultGen 是包级默认生成器，支撑包级函数。
var defaultGen atomic.Pointer[Generator]

func init() {
	g := Generator(NewGenerator(nil))
	defaultGen.Store(&g)
}

// NewGenerator 使用给定熵源构建 Generator。
// 若 r 为 nil，则使用 crypto/rand.Reader。
// NewGenerator(nil) 是构造默认生成器的推荐方式。
//
// 返回的 Generator 可并发安全使用；
// ulid.Monotonic 内部处理同毫秒内的递增。
func NewGenerator(r io.Reader) Generator {
	if r == nil {
		r = rand.Reader
	}
	return &ulidGenerator{entropy: ulid.Monotonic(r, 0)}
}

// SetDefault 替换包级生成器。传入 nil 恢复 crypto/rand 默认实现。
// 仅供测试使用；生产代码不应在运行时调用。
func SetDefault(g Generator) {
	if g == nil {
		g = NewGenerator(nil)
	}
	defaultGen.Store(&g)
}

// ULID 通过默认生成器生成 26 字符标识符。
func ULID() (string, error) {
	g := defaultGen.Load()
	if g == nil {
		return "", errors.New("idgen: default generator not initialized")
	}
	return (*g).ULID()
}

// MustULID 与 ULID 行为一致，但在出错时 panic。
// 仅在初始化阶段或熵源耗尽不可恢复的代码路径中使用。
func MustULID() string {
	id, err := ULID()
	if err != nil {
		panic(fmt.Sprintf("idgen: %v", err))
	}
	return id
}

// Short 通过默认生成器生成 n 字节随机数据，
// 编码为去填充的 Base64URL。
func Short(n int) (string, error) {
	g := defaultGen.Load()
	if g == nil {
		return "", errors.New("idgen: default generator not initialized")
	}
	return (*g).Short(n)
}

// Prefixed 通过默认生成器返回 "<prefix>_<ULID>"。
func Prefixed(prefix string) (string, error) {
	g := defaultGen.Load()
	if g == nil {
		return "", errors.New("idgen: default generator not initialized")
	}
	return (*g).Prefixed(prefix)
}

// ulidGenerator 基于 oklog/ulid/v2 实现 Generator。
type ulidGenerator struct {
	entropy *ulid.MonotonicEntropy
}

// ULID 生成 26 字符标识符。
func (g *ulidGenerator) ULID() (string, error) {
	id, err := ulid.New(ulid.Now(), g.entropy)
	if err != nil {
		return "", fmt.Errorf("idgen: generate ulid: %w", err)
	}
	return id.String(), nil
}

// Short 返回 n 字节随机数据，编码为去填充的 Base64URL。
func (g *ulidGenerator) Short(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("idgen: Short requires n > 0, got %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", fmt.Errorf("idgen: read short entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Prefixed 返回 "<prefix>_<ULID>"。
func (g *ulidGenerator) Prefixed(prefix string) (string, error) {
	id, err := g.ULID()
	if err != nil {
		return "", err
	}
	return prefix + "_" + id, nil
}
