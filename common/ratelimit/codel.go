package ratelimit

import (
	"context"
	"math"
	"sync"
	"time"
)

// coDelQueueCapacity 是 CoDel 队列的固定容量。
// 超过该容量时 Push 立即返回 ErrLimitExceed，避免无界排队。
const coDelQueueCapacity = 2048

// CoDelConfig 是 CoDel 受控延迟队列的配置，单位均为毫秒。
type CoDelConfig struct {
	// Target 是期望的队列停留时间，超过该值时进入丢包判定；默认 50ms。
	Target int64
	// Internal 是滑动最小值的时间窗口宽度，决定丢包节奏；默认 500ms。
	Internal int64
}

// CoDelStat 是 CoDel 队列的统计快照，用于外部监控采集。
type CoDelStat struct {
	// Dropping 表示当前是否处于丢包状态。
	Dropping bool
	// FaTime 是进入丢包状态的判定时间（毫秒）。
	FaTime int64
	// DropNext 是下一次预期丢包时间（毫秒）。
	DropNext int64
	// Packets 是当前队列中等待的报文数。
	Packets int
}

// 默认配置：Target 50ms，Internal 500ms。
var defaultCoDelConf = &CoDelConfig{
	Target:   50,
	Internal: 500,
}

// coDelPacket 是 CoDel 队列中的一个等待项。
// ch 用于 Pop 向 Push 通知是否丢弃；ts 是入队时间戳（毫秒）。
type coDelPacket struct {
	ch chan bool
	ts int64
}

// CoDelQueue 是基于 CoDel 算法的受控延迟队列。
//
// 当报文在队列中的停留时间持续超过 Target 时，按 controlLaw 节奏丢弃后续报文，
// 以保护后端免受慢请求堆积。队列容量固定为 coDelQueueCapacity。
type CoDelQueue struct {
	// pool 复用 coDelPacket.ch 对应的 channel，减少分配。
	pool    sync.Pool
	packets chan coDelPacket

	mux      sync.RWMutex
	conf     *CoDelConfig
	count    int64 // 当前丢包周期内已丢包数量，用于 controlLaw 计算节奏
	dropping bool  // 是否处于丢包状态
	faTime   int64 // 进入丢包状态的判定时间（0 表示未触发）
	dropNext int64 // 下一次丢包时间
}

// DefaultCoDelQueue 返回使用默认配置的 CoDel 队列。
func DefaultCoDelQueue() *CoDelQueue {
	return NewCoDelQueue(nil)
}

// NewCoDelQueue 创建 CoDel 队列；nil 配置使用默认值。
func NewCoDelQueue(conf *CoDelConfig) *CoDelQueue {
	if conf == nil {
		conf = defaultCoDelConf
	}
	q := &CoDelQueue{
		packets: make(chan coDelPacket, coDelQueueCapacity),
		conf:    conf,
	}
	q.pool.New = func() interface{} {
		return make(chan bool)
	}
	return q
}

// Reload 热更新队列配置；忽略 nil 或非法值（避免运行时把队列打挂）。
func (q *CoDelQueue) Reload(c *CoDelConfig) {
	if c == nil || c.Internal <= 0 || c.Target <= 0 {
		return
	}
	q.mux.Lock()
	q.conf = c
	q.mux.Unlock()
}

// Stat 返回当前队列统计快照。统计采集与限流调节解耦，便于按需输出。
func (q *CoDelQueue) Stat() CoDelStat {
	q.mux.Lock()
	defer q.mux.Unlock()
	return CoDelStat{
		Dropping: q.dropping,
		FaTime:   q.faTime,
		DropNext: q.dropNext,
		Packets:  len(q.packets),
	}
}

// Push 将请求压入队列并等待被 Pop 唤醒或超时。
// 返回 nil 表示请求被允许通过，调用方必须在处理完后调用 Pop 通知下一个报文。
// 返回 ErrLimitExceed 表示队列已满或被 CoDel 丢弃；ErrDeadline 表示等待超时。
func (q *CoDelQueue) Push(ctx context.Context) (err error) {
	r := coDelPacket{
		ch: q.pool.Get().(chan bool),
		ts: time.Now().UnixNano() / int64(time.Millisecond),
	}
	// 第一步：入队，队列满则立即拒绝。
	select {
	case q.packets <- r:
	default:
		q.pool.Put(r.ch)
		return ErrLimitExceed
	}
	// 第二步：等待 Pop 的判定或 ctx 超时。
	select {
	case drop := <-r.ch:
		q.pool.Put(r.ch)
		if drop {
			return ErrLimitExceed
		}
		return nil
	case <-ctx.Done():
		return ErrDeadline
	}
}

// Pop 从队列中按 CoDel 算法取出一个报文并通知其等待方。
//
// 若报文被判定通过（drop=false），Pop 返回；
// 若被判定丢弃（drop=true），Pop 继续处理下一个报文；
// 若等待方已离开（无法发送），回收 channel 并继续。
func (q *CoDelQueue) Pop() {
	for {
		select {
		case p := <-q.packets:
			drop := q.judge(p)
			select {
			case p.ch <- drop:
				if !drop {
					return
				}
			default:
				// 等待方已离开（如 ctx 超时），回收 channel 并继续。
				q.pool.Put(p.ch)
			}
		default:
			return
		}
	}
}

// controlLaw 按 CoDel 控制律计算下一次丢包时间。
// 丢包节奏随本周期已丢包数 count 的平方根反比递减。
func (q *CoDelQueue) controlLaw(now int64) int64 {
	q.dropNext = now + int64(float64(q.conf.Internal)/math.Sqrt(float64(q.count)))
	return q.dropNext
}

// judge 根据 CoDel 算法判定报文是否应丢弃。
//
// 算法要点：
//   - 停留时间 sojurn < Target：重置 faTime，不丢包。
//   - sojurn >= Target 且 faTime == 0：设置 faTime = now + Internal，本帧不丢包。
//   - sojurn >= Target 且 now >= faTime：标记丢包。
//   - 进入丢包状态后按 controlLaw 节奏持续丢包，直到 sojurn 回落。
func (q *CoDelQueue) judge(p coDelPacket) (drop bool) {
	now := time.Now().UnixNano() / int64(time.Millisecond)
	sojurn := now - p.ts
	q.mux.Lock()
	defer q.mux.Unlock()
	if sojurn < q.conf.Target {
		q.faTime = 0
	} else if q.faTime == 0 {
		q.faTime = now + q.conf.Internal
	} else if now >= q.faTime {
		drop = true
	}
	if q.dropping {
		if !drop {
			// 停留时间回落到 Target 以下，退出丢包状态。
			q.dropping = false
		} else if now > q.dropNext {
			q.count++
			q.dropNext = q.controlLaw(q.dropNext)
			drop = true
			return
		}
	} else if drop && (now-q.dropNext < q.conf.Internal || now-q.faTime >= q.conf.Internal) {
		// 满足进入丢包状态的条件。
		q.dropping = true
		// 复用上一周期控制该队列的丢包率作为本周期起点。
		if now-q.dropNext < q.conf.Internal {
			if q.count > 2 {
				q.count = q.count - 2
			} else {
				q.count = 1
			}
		} else {
			q.count = 1
		}
		q.dropNext = q.controlLaw(now)
		drop = true
		return
	}
	return
}
