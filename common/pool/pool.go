// Package pool 提供通用对象池，统一管理可复用资源（如连接、客户端）的获取、归还、空闲清理与关闭。
//
// 提供 Pool 接口与两种实现：基于切片的 NewSlice 和基于链表的 NewList。
// 支持活跃上限、空闲上限、空闲超时清理与等待超时，避免频繁创建销毁资源。
package pool

import (
	"context"
	"errors"
	"io"
	"time"

	"go-video/common/timeutil"
)

var (
	// ErrPoolExhausted 表示活跃对象已耗尽且不允许等待。
	ErrPoolExhausted = errors.New("container/pool exhausted")
	// ErrPoolClosed 表示池已关闭。
	ErrPoolClosed = errors.New("container/pool closed")

	// nowFunc 返回当前时间，测试中可被替换。
	nowFunc = time.Now
)

// Config 是对象池配置。
type Config struct {
	// Active 是池在给定时刻分配的对象总数上限。
	// 为 0 时表示不限制。
	Active int
	// Idle 是空闲对象数量上限。
	Idle int
	// IdleTimeout 是对象空闲超时时长；为 0 时不做超时清理。
	// 应用应将其设置为小于服务端超时的值。
	IdleTimeout timeutil.Duration
	// WaitTimeout 在池达到 Active 上限时，Get 最多等待该时长等对象归还。
	WaitTimeout timeutil.Duration
	// Wait 在未设置 WaitTimeout 时生效：为 true 时等待 ctx 超时，为 false 时直接返回。
	Wait bool
}

// item 是池中的对象包装，记录创建时间用于空闲超时判断。
type item struct {
	createdAt time.Time
	c         io.Closer
}

// expired 判断对象是否已超过空闲超时。
func (i *item) expired(timeout time.Duration) bool {
	if timeout <= 0 {
		return false
	}
	return i.createdAt.Add(timeout).Before(nowFunc())
}

// close 关闭底层对象。
func (i *item) close() error {
	return i.c.Close()
}

// Pool 是对象池接口。
type Pool interface {
	Get(ctx context.Context) (io.Closer, error)
	Put(ctx context.Context, c io.Closer, forceClose bool) error
	Close() error
}
