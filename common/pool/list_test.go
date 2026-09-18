package pool

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"go-video/common/timeutil"
)

// TestListGetPut 验证 List 的 Get/Put 往返。
func TestListGetPut(t *testing.T) {
	config := &Config{
		Active:      1,
		Idle:        1,
		IdleTimeout: timeutil.Duration(90 * time.Second),
		WaitTimeout: timeutil.Duration(10 * time.Millisecond),
		Wait:        false,
	}
	pool := NewList(config)
	pool.New = func(ctx context.Context) (io.Closer, error) {
		return &closer{}, nil
	}

	conn, err := pool.Get(context.TODO())
	assertNil(t, err, "Get")
	c1 := connection{pool: pool, c: conn}
	c1.HandleNormal()
	c1.Close()
}

// TestListPut 验证 Put(forceClose=true) 丢弃对象。
func TestListPut(t *testing.T) {
	var id = 0
	type connID struct {
		io.Closer
		id int
	}
	config := &Config{
		Active:      1,
		Idle:        1,
		IdleTimeout: timeutil.Duration(1 * time.Second),
		Wait:        false,
	}
	pool := NewList(config)
	pool.New = func(ctx context.Context) (io.Closer, error) {
		id = id + 1
		return &connID{id: id, Closer: &closer{}}, nil
	}
	conn, err := pool.Get(context.TODO())
	assertNil(t, err, "Get")
	conn1 := conn.(*connID)
	pool.Put(context.TODO(), conn, true)
	conn, err = pool.Get(context.TODO())
	assertNil(t, err, "Get2")
	conn2 := conn.(*connID)
	if conn1.id == conn2.id {
		t.Fatalf("expect new conn id, got same %d", conn1.id)
	}
}

// TestListIdleTimeout 验证空闲超时后 Get 得到新对象。
func TestListIdleTimeout(t *testing.T) {
	var id = 0
	type connID struct {
		io.Closer
		id int
	}
	config := &Config{
		Active: 1,
		Idle:   1,
		// 对象超时
		IdleTimeout: timeutil.Duration(1 * time.Millisecond),
	}
	pool := NewList(config)
	pool.New = func(ctx context.Context) (io.Closer, error) {
		id = id + 1
		return &connID{id: id, Closer: &closer{}}, nil
	}
	conn, err := pool.Get(context.TODO())
	assertNil(t, err, "Get")
	conn1 := conn.(*connID)
	pool.Put(context.TODO(), conn, false)
	time.Sleep(5 * time.Millisecond)
	// 空闲超时，得到新对象
	conn, err = pool.Get(context.TODO())
	assertNil(t, err, "Get2")
	conn2 := conn.(*connID)
	if conn1.id == conn2.id {
		t.Fatalf("expect new conn id after idle timeout, got same %d", conn1.id)
	}
}

// TestListContextTimeout 验证活跃耗尽时 ctx 超时返回错误。
func TestListContextTimeout(t *testing.T) {
	config := &Config{
		Active:      1,
		Idle:        1,
		IdleTimeout: timeutil.Duration(90 * time.Second),
		WaitTimeout: timeutil.Duration(10 * time.Millisecond),
		Wait:        false,
	}
	pool := NewList(config)
	pool.New = func(ctx context.Context) (io.Closer, error) {
		return &closer{}, nil
	}
	ctx, cancel := context.WithTimeout(context.TODO(), 100*time.Millisecond)
	defer cancel()
	conn, err := pool.Get(ctx)
	assertNil(t, err, "Get")
	_, err = pool.Get(ctx)
	assertErr(t, err, "Get2")
	pool.Put(context.TODO(), conn, false)
	_, err = pool.Get(ctx)
	assertNil(t, err, "Get3")
}

// TestListPoolExhausted 验证活跃耗尽且不可等待时返回 ErrPoolExhausted。
func TestListPoolExhausted(t *testing.T) {
	config := &Config{
		Active:      1,
		Idle:        1,
		IdleTimeout: timeutil.Duration(90 * time.Second),
		Wait:        false,
	}
	pool := NewList(config)
	pool.New = func(ctx context.Context) (io.Closer, error) {
		return &closer{}, nil
	}

	ctx, cancel := context.WithTimeout(context.TODO(), 100*time.Millisecond)
	defer cancel()
	conn, err := pool.Get(context.TODO())
	assertNil(t, err, "Get")
	_, err = pool.Get(ctx)
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expect ErrPoolExhausted, got %v", err)
	}
	pool.Put(context.TODO(), conn, false)
	_, err = pool.Get(ctx)
	assertNil(t, err, "Get3")
}

// TestListStaleClean 验证后台 staleCleaner goroutine 清理过期空闲对象。
func TestListStaleClean(t *testing.T) {
	var id = 0
	type connID struct {
		io.Closer
		id int
	}
	config := &Config{
		Active:      1,
		Idle:        1,
		IdleTimeout: timeutil.Duration(1 * time.Second),
		Wait:        false,
	}
	pool := NewList(config)
	pool.New = func(ctx context.Context) (io.Closer, error) {
		id = id + 1
		return &connID{id: id, Closer: &closer{}}, nil
	}
	conn, err := pool.Get(context.TODO())
	assertNil(t, err, "Get")
	conn1 := conn.(*connID)
	pool.Put(context.TODO(), conn, false)
	conn, err = pool.Get(context.TODO())
	assertNil(t, err, "Get2")
	conn2 := conn.(*connID)
	if conn1.id != conn2.id {
		t.Fatalf("expect same conn id (cached), got %d != %d", conn2.id, conn1.id)
	}
	pool.Put(context.TODO(), conn, false)
	// 睡眠超过 IdleTimeout，等待 staleCleaner 清理
	time.Sleep(2 * time.Second)
	conn, err = pool.Get(context.TODO())
	assertNil(t, err, "Get3")
	conn3 := conn.(*connID)
	if conn1.id == conn3.id {
		t.Fatalf("expect new conn id after stale clean, got same %d", conn1.id)
	}
}

// TestListClose 验证池关闭后 Get 返回 ErrPoolClosed。
func TestListClose(t *testing.T) {
	config := &Config{
		Active:      2,
		Idle:        2,
		IdleTimeout: timeutil.Duration(90 * time.Second),
		Wait:        false,
	}
	pool := NewList(config)
	pool.New = func(ctx context.Context) (io.Closer, error) {
		return &closer{}, nil
	}
	conn, err := pool.Get(context.TODO())
	assertNil(t, err, "Get")
	pool.Put(context.TODO(), conn, false)
	assertNil(t, pool.Close(), "Close")
	_, err = pool.Get(context.TODO())
	if !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("expect ErrPoolClosed, got %v", err)
	}
}
