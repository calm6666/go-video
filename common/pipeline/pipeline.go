// Package pipeline 提供合并写入管道，按 Split 分片聚批，达到 MaxSize 或 Interval 超时后批量回调处理。
//
// 适用于批量写库、批量上报、批量刷缓存等场景：调用方通过 Add/SyncAdd 投递数据，
// worker 按 key 分片聚批，满足阈值或超时后调用 Do 回调一次性处理。
package pipeline

import (
	"context"
	"errors"
	"sync"
	"time"

	"go-video/common/timeutil"
)

// ErrFull 表示分片 channel 已满，异步投递失败。
var ErrFull = errors.New("channel full")

// message 是投递到 channel 的消息单元，携带 key 与 value。
type message struct {
	key   string
	value interface{}
}

// Pipeline 是合并写入管道。
//
// 使用前必须设置 Do 与 Split 字段，然后调用 Start 启动 worker。
type Pipeline struct {
	Do     func(c context.Context, index int, values map[string][]interface{})
	Split  func(key string) int
	chans  []chan *message
	config *Config
	wait   sync.WaitGroup
}

// Config 是 Pipeline 配置。
type Config struct {
	// MaxSize 单批最大条数。
	MaxSize int
	// Interval 聚批超时时长。
	Interval timeutil.Duration
	// Buffer 单个分片 channel 容量。
	Buffer int
	// Worker 分片（worker）数量。
	Worker int
	// Smooth 是否错开各 worker 首次 ticker。
	Smooth bool
}

// fix 填充配置零值为默认值。
func (c *Config) fix() {
	if c.MaxSize <= 0 {
		c.MaxSize = 1000
	}
	if c.Interval <= 0 {
		c.Interval = timeutil.Duration(time.Second)
	}
	if c.Buffer <= 0 {
		c.Buffer = 1000
	}
	if c.Worker <= 0 {
		c.Worker = 10
	}
}

// NewPipeline 创建一个合并写入管道。
func NewPipeline(config *Config) (res *Pipeline) {
	if config == nil {
		config = &Config{}
	}
	config.fix()
	res = &Pipeline{
		chans:  make([]chan *message, config.Worker),
		config: config,
	}
	for i := 0; i < config.Worker; i++ {
		res.chans[i] = make(chan *message, config.Buffer)
	}
	return
}

// Start 启动所有 worker。
//
// Do 或 Split 为 nil 时 panic。
func (p *Pipeline) Start() {
	if p.Do == nil {
		panic("pipeline: do func is nil")
	}
	if p.Split == nil {
		panic("pipeline: split func is nil")
	}
	p.wait.Add(len(p.chans))
	for i, ch := range p.chans {
		go p.mergeproc(i, ch)
	}
}

// SyncAdd 同步阻塞地向分片 channel 投递一个值，分片由 Split 决定。
func (p *Pipeline) SyncAdd(c context.Context, key string, value interface{}) {
	ch, msg := p.add(c, key, value)
	ch <- msg
}

// Add 异步向分片 channel 投递一个值，满时返回 ErrFull。
func (p *Pipeline) Add(c context.Context, key string, value interface{}) (err error) {
	ch, msg := p.add(c, key, value)
	select {
	case ch <- msg:
	default:
		err = ErrFull
	}
	return
}

// add 计算分片并构造消息。
func (p *Pipeline) add(c context.Context, key string, value interface{}) (ch chan *message, m *message) {
	shard := p.Split(key) % p.config.Worker
	ch = p.chans[shard]
	m = &message{key: key, value: value}
	return
}

// Close 关闭所有 worker。
//
// 向每个 channel 发送 nil 哨兵，worker 处理剩余聚批后退出。
func (p *Pipeline) Close() (err error) {
	for _, ch := range p.chans {
		ch <- nil
	}
	p.wait.Wait()
	return
}

// mergeproc 是单个 worker 的聚批主循环。
func (p *Pipeline) mergeproc(index int, ch <-chan *message) {
	defer p.wait.Done()
	var (
		m         *message
		vals      = make(map[string][]interface{}, p.config.MaxSize)
		closed    bool
		count     int
		inteval   = time.Duration(p.config.Interval)
		oldTicker = true
	)
	// Smooth 模式下错开各 worker 的首次 ticker 间隔。
	if p.config.Smooth && index > 0 {
		inteval = time.Duration(int64(index) * (int64(p.config.Interval) / int64(p.config.Worker)))
	}
	ticker := time.NewTicker(inteval)
	for {
		select {
		case m = <-ch:
			if m == nil {
				closed = true
				break
			}
			count++
			vals[m.key] = append(vals[m.key], m.value)
			if count >= p.config.MaxSize {
				break
			}
			continue
		case <-ticker.C:
			// Smooth 模式下首次 ticker 触发后切换为标准 Interval。
			if p.config.Smooth && oldTicker {
				ticker.Stop()
				ticker = time.NewTicker(time.Duration(p.config.Interval))
				oldTicker = false
			}
		}
		if len(vals) > 0 {
			ctx := context.Background()
			p.Do(ctx, index, vals)
			vals = make(map[string][]interface{}, p.config.MaxSize)
			count = 0
		}
		if closed {
			ticker.Stop()
			return
		}
	}
}
