package pool

import (
	"context"
	"io"
	"sync"
	"time"
)

var _ Pool = &Slice{}

// Slice 是基于切片的对象池实现，支持异步 opener 与等待请求队列。
type Slice struct {
	// New 是应用提供的对象构造函数。
	//
	// 返回的对象不应处于特殊状态（如已订阅频道、已开启事务等）。
	New  func(ctx context.Context) (io.Closer, error)
	stop func() // stop 取消对象 opener 协程。

	// mu 保护以下字段。
	mu           sync.Mutex
	freeItem     []*item
	itemRequests map[uint64]chan item
	nextRequest  uint64 // itemRequests 中下一个使用的 key。
	active       int    // 已打开和待打开的对象数。
	// openerCh 用于通知需要新建对象。
	// 一个运行 itemOpener() 的协程从该 chan 读取，maybeOpenNewItems 向其发送（每次需要一个对象发一次）。
	// 在 db.Close() 时关闭。关闭通知 itemOpener 协程退出。
	openerCh  chan struct{}
	closed    bool
	cleanerCh chan struct{}

	// Config 池配置
	conf *Config
}

// NewSlice 创建一个基于切片的对象池。
func NewSlice(c *Config) *Slice {
	// 检查配置
	if c == nil || c.Active < c.Idle {
		panic("config nil or Idle Must <= Active")
	}
	ctx, cancel := context.WithCancel(context.Background())
	// 新建池
	p := &Slice{
		conf:         c,
		stop:         cancel,
		itemRequests: make(map[uint64]chan item),
		openerCh:     make(chan struct{}, 1000000),
	}
	p.startCleanerLocked(time.Duration(c.IdleTimeout))

	go p.itemOpener(ctx)
	return p
}

// Reload 热更新配置。
func (p *Slice) Reload(c *Config) error {
	p.mu.Lock()
	p.startCleanerLocked(time.Duration(c.IdleTimeout))
	p.setActive(c.Active)
	p.setIdle(c.Idle)
	p.conf = c
	p.mu.Unlock()
	return nil
}

// Get 返回一个新打开或缓存的 *item。
func (p *Slice) Get(ctx context.Context) (io.Closer, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}
	idleTimeout := time.Duration(p.conf.IdleTimeout)
	// 优先取空闲对象。
	numFree := len(p.freeItem)
	for numFree > 0 {
		i := p.freeItem[0]
		copy(p.freeItem, p.freeItem[1:])
		p.freeItem = p.freeItem[:numFree-1]
		p.mu.Unlock()
		if i.expired(idleTimeout) {
			i.close()
			p.mu.Lock()
			p.release()
		} else {
			return i.c, nil
		}
		numFree = len(p.freeItem)
	}

	// 没有空闲对象或不可用。若不允许新建更多对象，则发起请求并等待。
	if p.conf.Active > 0 && p.active >= p.conf.Active {
		// 检查 WaitTimeout，不允许等待则直接返回
		if p.conf.WaitTimeout == 0 && !p.conf.Wait {
			p.mu.Unlock()
			return nil, ErrPoolExhausted
		}
		// 创建对象请求 channel，带缓冲以免 itemOpener 写入时阻塞。
		req := make(chan item, 1)
		reqKey := p.nextRequestKeyLocked()
		p.itemRequests[reqKey] = req
		wt := p.conf.WaitTimeout
		p.mu.Unlock()

		// 重置 context 超时
		if wt > 0 {
			var cancel func()
			_, ctx, cancel = wt.Shrink(ctx)
			defer cancel()
		}
		// 用 context 超时控制对象请求。
		select {
		case <-ctx.Done():
			// 移除对象请求并确保没有值被发送到已移除的请求上。
			p.mu.Lock()
			delete(p.itemRequests, reqKey)
			p.mu.Unlock()
			return nil, ctx.Err()
		case ret, ok := <-req:
			if !ok {
				return nil, ErrPoolClosed
			}
			if ret.expired(idleTimeout) {
				ret.close()
				p.mu.Lock()
				p.release()
			} else {
				return ret.c, nil
			}
		}
	}

	p.active++ // 乐观地增加
	p.mu.Unlock()
	c, err := p.New(ctx)
	if err != nil {
		p.mu.Lock()
		p.release()
		p.mu.Unlock()
		return nil, err
	}
	return c, nil
}

// Put 将对象归还到空闲池。
// forceClose 为 true 时强制关闭对象而不归还。
func (p *Slice) Put(ctx context.Context, c io.Closer, forceClose bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if forceClose {
		p.release()
		return c.Close()
	}
	added := p.putItemLocked(c)
	if !added {
		p.active--
		return c.Close()
	}
	return nil
}

// putItemLocked 满足一个对象请求或将对象放入空闲池，返回 true；
// 否则返回 false。
//
// 若存在等待请求则满足之；否则在未超空闲上限时放入 freeItem。
// 池已关闭或活跃数超限时返回 false。
func (p *Slice) putItemLocked(c io.Closer) bool {
	if p.closed {
		return false
	}
	if p.conf.Active > 0 && p.active > p.conf.Active {
		return false
	}
	i := item{
		c:         c,
		createdAt: nowFunc(),
	}
	if l := len(p.itemRequests); l > 0 {
		var req chan item
		var reqKey uint64
		for reqKey, req = range p.itemRequests {
			break
		}
		delete(p.itemRequests, reqKey) // 从待处理请求中移除。
		req <- i
		return true
	} else if !p.closed && p.maxIdleItemsLocked() > len(p.freeItem) {
		p.freeItem = append(p.freeItem, &i)
		return true
	}
	return false
}

// itemOpener 在独立协程中运行，按需打开新对象。
func (p *Slice) itemOpener(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.openerCh:
			p.openNewItem(ctx)
		}
	}
}

// maybeOpenNewItems 按等待请求数与活跃上限打开新对象。
func (p *Slice) maybeOpenNewItems() {
	numRequests := len(p.itemRequests)
	if p.conf.Active > 0 {
		numCanOpen := p.conf.Active - p.active
		if numRequests > numCanOpen {
			numRequests = numCanOpen
		}
	}
	for numRequests > 0 {
		p.active++ // 乐观地增加
		numRequests--
		if p.closed {
			return
		}
		p.openerCh <- struct{}{}
	}
}

// openNewItem 打开一个新对象。
func (p *Slice) openNewItem(ctx context.Context) {
	// maybeOpenNewConnctions 已在发送到 p.openerCh 前执行了 p.active++。
	// 本函数在对象打开失败或关闭前必须执行 p.active--。
	c, err := p.New(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.release()
		return
	}
	if !p.putItemLocked(c) {
		p.active--
		c.Close()
	}
}

// setIdle 设置空闲对象上限。
//
// 若 Active 大于 0 但小于新的 Idle，则 Idle 被缩减到 Active 上限。
// n <= 0 时不保留任何空闲对象。
func (p *Slice) setIdle(n int) {
	p.mu.Lock()
	if n > 0 {
		p.conf.Idle = n
	} else {
		// 不保留空闲对象。
		p.conf.Idle = -1
	}
	// 确保 maxIdle 不超过 maxOpen
	if p.conf.Active > 0 && p.maxIdleItemsLocked() > p.conf.Active {
		p.conf.Idle = p.conf.Active
	}
	var closing []*item
	idleCount := len(p.freeItem)
	maxIdle := p.maxIdleItemsLocked()
	if idleCount > maxIdle {
		closing = p.freeItem[maxIdle:]
		p.freeItem = p.freeItem[:maxIdle]
	}
	p.mu.Unlock()
	for _, c := range closing {
		c.close()
	}
}

// setActive 设置活跃对象上限。
//
// 若 Idle 大于 0 且新的 Active 小于 Idle，则 Idle 被缩减到新的 Active 上限。
// n <= 0 表示不限制。默认为 0（不限）。
func (p *Slice) setActive(n int) {
	p.mu.Lock()
	p.conf.Active = n
	if n < 0 {
		p.conf.Active = 0
	}
	syncIdle := p.conf.Active > 0 && p.maxIdleItemsLocked() > p.conf.Active
	p.mu.Unlock()
	if syncIdle {
		p.setIdle(n)
	}
}

// startCleanerLocked 在需要时启动对象清理协程。
func (p *Slice) startCleanerLocked(d time.Duration) {
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
		go p.staleCleaner(time.Duration(p.conf.IdleTimeout))
	}
}

// staleCleaner 周期性清理过期空闲对象。
func (p *Slice) staleCleaner(d time.Duration) {
	const minInterval = 100 * time.Millisecond

	if d < minInterval {
		d = minInterval
	}
	t := time.NewTimer(d)

	for {
		select {
		case <-t.C:
		case <-p.cleanerCh: // IdleTimeout 被修改或池被关闭。
		}
		p.mu.Lock()
		d = time.Duration(p.conf.IdleTimeout)
		if p.closed || d <= 0 {
			p.mu.Unlock()
			return
		}

		expiredSince := nowFunc().Add(-d)
		var closing []*item
		for i := 0; i < len(p.freeItem); i++ {
			c := p.freeItem[i]
			if c.createdAt.Before(expiredSince) {
				closing = append(closing, c)
				p.active--
				last := len(p.freeItem) - 1
				p.freeItem[i] = p.freeItem[last]
				p.freeItem[last] = nil
				p.freeItem = p.freeItem[:last]
				i--
			}
		}
		p.mu.Unlock()

		for _, c := range closing {
			c.close()
		}

		if d < minInterval {
			d = minInterval
		}
		t.Reset(d)
	}
}

// nextRequestKeyLocked 返回下一个对象请求 key。
// 假定 nextRequest 不会溢出。
func (p *Slice) nextRequestKeyLocked() uint64 {
	next := p.nextRequest
	p.nextRequest++
	return next
}

// defaultIdleItems 是 Idle 配置为 0 时的默认空闲上限。
const defaultIdleItems = 2

// maxIdleItemsLocked 返回当前空闲对象上限。
func (p *Slice) maxIdleItemsLocked() int {
	n := p.conf.Idle
	switch {
	case n == 0:
		return defaultIdleItems
	case n < 0:
		return 0
	default:
		return n
	}
}

// release 递减活跃计数并尝试打开新对象以满足等待请求。
// 调用方必须持有 p.mu。
func (p *Slice) release() {
	p.active--
	p.maybeOpenNewItems()
}

// Close 关闭池。
func (p *Slice) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	if p.cleanerCh != nil {
		close(p.cleanerCh)
	}
	var err error
	for _, i := range p.freeItem {
		i.close()
	}
	p.freeItem = nil
	p.closed = true
	for _, req := range p.itemRequests {
		close(req)
	}
	p.mu.Unlock()
	p.stop()
	return err
}
