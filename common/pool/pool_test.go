package pool

import (
	"context"
	"io"
	"time"
)

// closer 是测试用的可关闭对象。
type closer struct{}

func (c *closer) Close() error { return nil }

// connection 包装一个池对象与其所属池，模拟使用方持有连接。
type connection struct {
	c    io.Closer
	pool Pool
}

func (c *connection) HandleQuick() {
	// time.Sleep(1 * time.Millisecond)
}

func (c *connection) HandleNormal() {
	time.Sleep(20 * time.Millisecond)
}

func (c *connection) HandleSlow() {
	time.Sleep(500 * time.Millisecond)
}

func (c *connection) Close() {
	c.pool.Put(context.Background(), c.c, false)
}

// nowFunc 的测试替换辅助：在测试后恢复默认时间。
func resetNowFunc() func() {
	orig := nowFunc
	nowFunc = time.Now
	return func() { nowFunc = orig }
}
