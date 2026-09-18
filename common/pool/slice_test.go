package pool

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"go-video/common/timeutil"
)

// assertNil 在 err 非 nil 时失败。
func assertNil(t *testing.T, err error, msg string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", msg, err)
	}
}

// assertErr 在 err 为 nil 时失败。
func assertErr(t *testing.T, err error, msg string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expect error, got nil", msg)
	}
}

// TestSliceGetPut 验证 Slice 的 Get/Put 往返。
func TestSliceGetPut(t *testing.T) {
	config := &Config{
		Active:      1,
		Idle:        1,
		IdleTimeout: timeutil.Duration(90 * time.Second),
		WaitTimeout: timeutil.Duration(10 * time.Millisecond),
		Wait:        false,
	}
	pool := NewSlice(config)
	pool.New = func(ctx context.Context) (io.Closer, error) {
		return &closer{}, nil
	}

	conn, err := pool.Get(context.TODO())
	assertNil(t, err, "Get")
	c1 := connection{pool: pool, c: conn}
	c1.HandleNormal()
	c1.Close()
}

// TestSlicePut 验证 Put(forceClose=true) 丢弃对象，下次 Get 得到新对象。
func TestSlicePut(t *testing.T) {
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
	pool := NewSlice(config)
	pool.New = func(ctx context.Context) (io.Closer, error) {
		id = id + 1
		return &connID{id: id, Closer: &closer{}}, nil
	}
	conn, err := pool.Get(context.TODO())
	assertNil(t, err, "Get")
	conn1 := conn.(*connID)
	// Put(forceClose=true) 丢弃该对象。
	pool.Put(context.TODO(), conn, true)
	conn, err = pool.Get(context.TODO())
	assertNil(t, err, "Get2")
	conn2 := conn.(*connID)
	if conn1.id == conn2.id {
		t.Fatalf("expect new conn id, got same %d", conn1.id)
	}
}

// TestSliceIdleTimeout 验证空闲超时后 Get 得到新对象。
func TestSliceIdleTimeout(t *testing.T) {
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
	pool := NewSlice(config)
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

// TestSliceContextTimeout 验证活跃耗尽时 ctx 超时返回错误。
func TestSliceContextTimeout(t *testing.T) {
	config := &Config{
		Active:      1,
		Idle:        1,
		IdleTimeout: timeutil.Duration(90 * time.Second),
		WaitTimeout: timeutil.Duration(10 * time.Millisecond),
		Wait:        false,
	}
	pool := NewSlice(config)
	pool.New = func(ctx context.Context) (io.Closer, error) {
		return &closer{}, nil
	}
	ctx, cancel := context.WithTimeout(context.TODO(), 100*time.Millisecond)
	defer cancel()
	conn, err := pool.Get(ctx)
	assertNil(t, err, "Get")
	_, err = pool.Get(ctx)
	// ctx 超时错误
	assertErr(t, err, "Get2")
	pool.Put(context.TODO(), conn, false)
	_, err = pool.Get(ctx)
	assertNil(t, err, "Get3")
}

// TestSlicePoolExhausted 验证活跃耗尽且不可等待时返回 ErrPoolExhausted。
func TestSlicePoolExhausted(t *testing.T) {
	config := &Config{
		Active:      1,
		Idle:        1,
		IdleTimeout: timeutil.Duration(90 * time.Second),
		Wait:        false,
	}
	pool := NewSlice(config)
	pool.New = func(ctx context.Context) (io.Closer, error) {
		return &closer{}, nil
	}

	ctx, cancel := context.WithTimeout(context.TODO(), 100*time.Millisecond)
	defer cancel()
	conn, err := pool.Get(context.TODO())
	assertNil(t, err, "Get")
	_, err = pool.Get(ctx)
	// active == 1，无可用对象导致耗尽
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expect ErrPoolExhausted, got %v", err)
	}
	pool.Put(context.TODO(), conn, false)
	_, err = pool.Get(ctx)
	assertNil(t, err, "Get3")
}

// TestSliceClose 验证池关闭后 Get 返回 ErrPoolClosed。
func TestSliceClose(t *testing.T) {
	config := &Config{
		Active:      2,
		Idle:        2,
		IdleTimeout: timeutil.Duration(90 * time.Second),
		Wait:        false,
	}
	pool := NewSlice(config)
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
