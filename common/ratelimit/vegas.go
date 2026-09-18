package ratelimit

import (
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// Vegas 限流上下限与采样窗口边界。
const (
	vegasMinWindowTime = int64(time.Millisecond * 500)  // 最小采样窗口
	vegasMaxWindowTime = int64(time.Millisecond * 2000) // 最大采样窗口
	vegasMinLimit      = 8                              // 最小并发上限
	vegasMaxLimit      = 2048                           // 最大并发上限
)

// VegasStat 是 Vegas 限流器的统计快照。
type VegasStat struct {
	// Limit 是当前并发上限。
	Limit int64
	// InFlight 是当前在途请求数。
	InFlight int64
	// MinRTT 是历史观测到的最小 RTT（纳秒）。
	MinRTT time.Duration
	// LastRTT 是最近一次采样窗口的平均 RTT（纳秒）。
	LastRTT time.Duration
}

// Vegas 是基于 TCP Vegas 拥塞控制算法的自适应限流器。
//
// 通过采样窗口内的 RTT 与并发数，动态调整并发上限：
//   - 当队列堆积（RTT 升高）时降低并发上限。
//   - 当 RTT 接近最小值时按梯度提升并发上限。
//   - 出现 Drop 时按阈值收敛。
//
// 采样窗口在样本数达到阈值或时间窗口到期时切换，避免长尾样本污染统计。
type Vegas struct {
	limit      int64 // 当前并发上限
	inFlight   int64 // 当前在途请求数
	updateTime int64 // 下一次允许更新上限的时间（纳秒）
	minRTT     int64 // 历史最小 RTT（纳秒）

	// sample 保存当前采样窗口，通过 atomic.Value 实现无锁读取与切换。
	sample atomic.Value // *vegasSample
	mu     sync.Mutex
	probes int64 // 剩余探测轮次，用于周期性重置 minRTT
}

// NewVegas 创建一个 Vegas 限流器，初始并发上限为 vegasMinLimit。
func NewVegas() *Vegas {
	v := &Vegas{
		probes: 100,
		limit:  vegasMinLimit,
	}
	v.sample.Store(&vegasSample{})
	return v
}

// Stat 返回当前统计快照，便于外部监控采集。
func (v *Vegas) Stat() VegasStat {
	s, _ := v.sample.Load().(*vegasSample)
	var lastRTT int64
	if s != nil {
		lastRTT = s.RTT()
	}
	return VegasStat{
		Limit:    atomic.LoadInt64(&v.limit),
		InFlight: atomic.LoadInt64(&v.inFlight),
		MinRTT:   time.Duration(atomic.LoadInt64(&v.minRTT)),
		LastRTT:  time.Duration(lastRTT),
	}
}

// Reset 将限流器恢复到初始状态，清空采样窗口与统计。
func (v *Vegas) Reset() {
	v.mu.Lock()
	defer v.mu.Unlock()
	atomic.StoreInt64(&v.limit, vegasMinLimit)
	atomic.StoreInt64(&v.inFlight, 0)
	atomic.StoreInt64(&v.updateTime, 0)
	atomic.StoreInt64(&v.minRTT, 0)
	v.probes = 100
	v.sample.Store(&vegasSample{})
}

// Acquire 尝试获取一个执行槽位。
//
// 返回值：
//   - done：必须在请求结束时调用，传入请求开始时间与处理结果 Op。
//   - success：true 表示在当前并发上限内直接通过；false 表示超过上限，
//     调用方应将其转入排队（如 CoDel 队列）。
//
// 无论 success 为何，done 都必须被调用，否则 inflight 计数会泄漏。
func (v *Vegas) Acquire() (done func(time.Time, Op), success bool) {
	inFlight := atomic.AddInt64(&v.inFlight, 1)
	if inFlight <= atomic.LoadInt64(&v.limit) {
		success = true
	}

	return func(start time.Time, op Op) {
		atomic.AddInt64(&v.inFlight, -1)
		// Ignore 仅归还 inflight，不参与 RTT 采样。
		if op == Ignore {
			return
		}
		end := time.Now().UnixNano()
		rtt := end - start.UnixNano()

		s, _ := v.sample.Load().(*vegasSample)
		if s == nil {
			return
		}
		if op == Drop {
			s.Add(rtt, inFlight, true)
		} else if op == Success {
			s.Add(rtt, inFlight, false)
		}
		// 仅在采样数足够且到达更新窗口时调节上限，避免噪声干扰。
		if end > atomic.LoadInt64(&v.updateTime) && s.Count() >= 16 {
			v.mu.Lock()
			defer v.mu.Unlock()
			// 双重检查：在加锁期间 sample 可能已被其他 goroutine 替换，
			// 若已被替换则放弃本次更新。
			if cur, _ := v.sample.Load().(*vegasSample); cur != s {
				return
			}
			v.sample.Store(&vegasSample{})

			lastRTT := s.RTT()
			if lastRTT <= 0 {
				return
			}
			// 更新窗口为最近 RTT 的 5 倍，并夹取到 [min, max] 范围。
			updateTime := end + lastRTT*5
			if lastRTT*5 < vegasMinWindowTime {
				updateTime = end + vegasMinWindowTime
			} else if lastRTT*5 > vegasMaxWindowTime {
				updateTime = end + vegasMaxWindowTime
			}
			atomic.StoreInt64(&v.updateTime, updateTime)
			limit := atomic.LoadInt64(&v.limit)
			// queue 估算当前排队量，用于决定提升幅度。
			queue := float64(limit) * (1 - float64(v.minRTT)/float64(lastRTT))
			v.probes--
			// 探测轮次耗尽时，若并发显著回落则重置 minRTT 以重新校准基线。
			if v.probes <= 0 {
				maxFlight := s.MaxInFlight()
				if maxFlight*2 < v.limit || maxFlight <= vegasMinLimit {
					v.probes = 3*limit + rand.Int63n(3*limit)
					v.minRTT = lastRTT
				}
			}
			if v.minRTT == 0 || lastRTT < v.minRTT {
				v.minRTT = lastRTT
			}
			var newLimit float64
			threshold := math.Sqrt(float64(limit)) / 2
			if s.Drop() {
				// 出现丢弃，按阈值收敛。
				newLimit = float64(limit) - threshold
			} else if s.MaxInFlight()*2 < v.limit {
				// 并发未充分使用，保持不变。
				return
			} else {
				// 按 queue 与 threshold 的关系分档调整上限。
				if queue < threshold {
					newLimit = float64(limit) + 6*threshold
				} else if queue < 2*threshold {
					newLimit = float64(limit) + 3*threshold
				} else if queue < 3*threshold {
					newLimit = float64(limit) + threshold
				} else if queue > 6*threshold {
					newLimit = float64(limit) - threshold
				} else {
					return
				}
			}
			newLimit = math.Max(vegasMinLimit, math.Min(vegasMaxLimit, newLimit))
			atomic.StoreInt64(&v.limit, int64(newLimit))
		}
	}, success
}

// vegasSample 是 Vegas 限流器的 RTT 采样窗口，使用原子操作保证并发安全。
type vegasSample struct {
	count       int64 // 样本数
	maxInFlight int64 // 窗口内观测到的最大并发
	drop        int64 // 是否出现丢弃（0/1）
	totalRTT    int64 // 累计 RTT（纳秒）
}

// Add 累加一次 RTT 采样，并更新最大并发与丢弃标记。
func (s *vegasSample) Add(rtt int64, inFlight int64, drop bool) {
	if drop {
		atomic.StoreInt64(&s.drop, 1)
	}
	// 自旋 CAS 更新最大并发，直到成功。
	for max := atomic.LoadInt64(&s.maxInFlight); max < inFlight; max = atomic.LoadInt64(&s.maxInFlight) {
		if atomic.CompareAndSwapInt64(&s.maxInFlight, max, inFlight) {
			break
		}
	}
	atomic.AddInt64(&s.totalRTT, rtt)
	atomic.AddInt64(&s.count, 1)
}

// RTT 返回窗口内平均 RTT（纳秒）；样本为 0 时返回 0。
func (s *vegasSample) RTT() int64 {
	count := atomic.LoadInt64(&s.count)
	if count == 0 {
		return 0
	}
	return atomic.LoadInt64(&s.totalRTT) / count
}

// MaxInFlight 返回窗口内观测到的最大并发数。
func (s *vegasSample) MaxInFlight() int64 {
	return atomic.LoadInt64(&s.maxInFlight)
}

// Count 返回窗口内样本数。
func (s *vegasSample) Count() int64 {
	return atomic.LoadInt64(&s.count)
}

// Drop 返回窗口内是否出现过丢弃。
func (s *vegasSample) Drop() bool {
	return atomic.LoadInt64(&s.drop) == 1
}
