package pool

import (
	"container/list"
	"context"
	"io"
	"sync"
	"time"
)

var _ Pool = &List{}

// List 是基于双向链表的对象池实现，支持 cond 信号唤醒等待者。
type List struct {
	// New 是应用提供的对象构造函数。
	//
	// 返回的对象不应处于特殊状态（如已订阅频道、已开启事务等）。
	New func(ctx context.Context) (io.Closer, error)

	// mu 保护以下字段。
	mu     sync.Mutex
	cond   chan struct{}
	closed bool
	active int
	// 清理过期对象
	cleanerCh chan struct{}

	// 对象栈，最近使用的在头部。
	idles list.List

	// Config 池配置
	conf *Config
}

// NewList 创建一个基于链表的对象池。
func NewList(c *Config) *List {
	// 检查配置
	if c == nil || c.Active < c.Idle {
		panic("config nil or Idle Must <= Active")
	}
	// 新建池
	p := &List{conf: c}
	p.cond = make(chan struct{})
	p.startCleanerLocked(time.Duration(c.IdleTimeout))
	return p
}

// Reload 热更新配置。
func (p *List) Reload(c *Config) error {
	p.mu.Lock()
	p.startCleanerLocked(time.Duration(c.IdleTimeout))
	p.conf = c
	p.mu.Unlock()
	return nil
}

// startCleanerLocked 在需要时启动对象清理协程。
func (p *List) startCleanerLocked(d time.Duration) {
	if d <= 0 {
		// 为 0 时 staleCleaner() 直接返回
		return
	}
	if d < time.Duration(p.conf.IdleTimeout) && p.cleanerCh != nil {
		select {
		case p.cleanerCh <- struct{}{}:
		default:
		}
	}
	// 只运行一个，清理过期对象。
	if p.cleanerCh == nil {
		p.cleanerCh = make(chan struct{}, 1)
		go p.staleCleaner()
	}
}

// staleCleaner 周期性清理过期空闲对象。
func (p *List) staleCleaner() {
	ticker := time.NewTicker(100 * time.Millisecond)
	for {
		select {
		case <-ticker.C:
		case <-p.cleanerCh: // IdleTimeout 被修改或池被关闭。
		}
		p.mu.Lock()
		if p.closed || p.conf.IdleTimeout <= 0 {
			p.mu.Unlock()
			return
		}
		for i, n := 0, p.idles.Len(); i < n; i++ {
			e := p.idles.Back()
			if e == nil {
				// 不可能发生
				break
			}
			ic := e.Value.(item)
			if !ic.expired(time.Duration(p.conf.IdleTimeout)) {
				// 无需继续
				break
			}
			p.idles.Remove(e)
			p.release()
			p.mu.Unlock()
			ic.c.Close()
			p.mu.Lock()
		}
		p.mu.Unlock()
	}
}

// Get 从空闲链表返回对象，或新建一个对象。
func (p *List) Get(ctx context.Context) (io.Closer, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}
	for {
		// 取空闲对象。
		for i, n := 0, p.idles.Len(); i < n; i++ {
			e := p.idles.Front()
			if e == nil {
				break
			}
			ic := e.Value.(item)
			p.idles.Remove(e)
			p.mu.Unlock()
			if !ic.expired(time.Duration(p.conf.IdleTimeout)) {
				return ic.c, nil
			}
			ic.c.Close()
			p.mu.Lock()
			p.release()
		}
		// 拨号新对象前检查池是否已关闭。
		if p.closed {
			p.mu.Unlock()
			return nil, ErrPoolClosed
		}
		// 未达上限时新建对象。
		if p.conf.Active == 0 || p.active < p.conf.Active {
			newItem := p.New
			p.active++
			p.mu.Unlock()
			c, err := newItem(ctx)
			if err != nil {
				p.mu.Lock()
				p.release()
				p.mu.Unlock()
				c = nil
			}
			return c, err
		}
		if p.conf.WaitTimeout == 0 && !p.conf.Wait {
			p.mu.Unlock()
			return nil, ErrPoolExhausted
		}
		wt := p.conf.WaitTimeout
		p.mu.Unlock()

		// 慢路径：重置 context 超时
		nctx := ctx
		cancel := func() {}
		if wt > 0 {
			_, nctx, cancel = wt.Shrink(ctx)
		}
		select {
		case <-nctx.Done():
			cancel()
			return nil, nctx.Err()
		case <-p.cond:
		}
		cancel()
		p.mu.Lock()
	}
}

// Put 将对象归还到空闲池。
func (p *List) Put(ctx context.Context, c io.Closer, forceClose bool) error {
	p.mu.Lock()
	if !p.closed && !forceClose {
		p.idles.PushFront(item{createdAt: nowFunc(), c: c})
		if p.idles.Len() > p.conf.Idle {
			c = p.idles.Remove(p.idles.Back()).(item).c
		} else {
			c = nil
		}
	}
	if c == nil {
		p.signal()
		p.mu.Unlock()
		return nil
	}
	p.release()
	p.mu.Unlock()
	return c.Close()
}

// Close 释放池使用的资源。
func (p *List) Close() error {
	p.mu.Lock()
	idles := p.idles
	p.idles.Init()
	p.closed = true
	p.active -= idles.Len()
	p.mu.Unlock()
	for e := idles.Front(); e != nil; e = e.Next() {
		e.Value.(item).c.Close()
	}
	return nil
}

// release 递减活跃计数并唤醒等待者。调用方必须持有 p.mu。
func (p *List) release() {
	p.active--
	p.signal()
}

// signal 非阻塞地向 cond 发送信号。
func (p *List) signal() {
	select {
	default:
	case p.cond <- struct{}{}:
	}
}
