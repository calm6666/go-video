// Package policy 汇集 danmaku 服务的纯领域策略：时间分段、状态机、
// 屏蔽词匹配与防刷屏窗口判定。
//
// 本包不依赖数据库、Redis 或 RPC，便于单元测试全覆盖；
// 所有函数必须对非法入参给出确定性结果，不得静默吞掉异常输入。
package policy

// DefaultSegmentSeconds 是默认分段秒数（与配置缺省值一致）。
const DefaultSegmentSeconds int32 = 6

// MaxSegmentSeconds 是允许的最大分段秒数，避免配置异常导致分段号退化。
const MaxSegmentSeconds int32 = 600

// segmentMs 返回分段毫秒宽度；非法分段秒数回退到默认值。
func segmentMs(segmentSeconds int32) int64 {
	if segmentSeconds <= 0 {
		segmentSeconds = DefaultSegmentSeconds
	}
	if segmentSeconds > MaxSegmentSeconds {
		segmentSeconds = MaxSegmentSeconds
	}
	return int64(segmentSeconds) * 1000
}

// SegNo 把时间轴毫秒位置映射为分段号（从 0 开始）。
// 弹幕量级大，读取侧一律按 (oid, seg_no) 拉取，禁止按 progress_ms 逐条查询。
func SegNo(progressMs int64, segmentSeconds int32) int32 {
	if progressMs < 0 {
		progressMs = 0
	}
	return int32(progressMs / segmentMs(segmentSeconds))
}

// SegRangeFromMs 把时间轴窗口 [startMs, endMs] 映射为分段号闭区间。
// endMs 小于 startMs 时按 startMs 处理，返回的单侧区间仍是合法窗口。
func SegRangeFromMs(startMs, endMs int64, segmentSeconds int32) (int32, int32) {
	if startMs < 0 {
		startMs = 0
	}
	if endMs < startMs {
		endMs = startMs
	}
	return SegNo(startMs, segmentSeconds), SegNo(endMs, segmentSeconds)
}

// Segments 展开 [startSeg, endSeg] 闭区间为分段号列表。
// maxSegs 是服务端窗口上限；超出时截断到上限而不是返回错误，
// 让客户端一次拉取有界数据，错误判定由调用方用 SegRangeValid 先行校验。
func Segments(startSeg, endSeg int32, maxSegs int) []int32 {
	if startSeg < 0 {
		startSeg = 0
	}
	if endSeg < startSeg {
		endSeg = startSeg
	}
	if maxSegs <= 0 {
		maxSegs = 1
	}
	// 覆盖段数超过上限时只取前 maxSegs 段。
	if int64(endSeg)-int64(startSeg)+1 > int64(maxSegs) {
		endSeg = startSeg + int32(maxSegs) - 1
	}
	out := make([]int32, 0, int(endSeg-startSeg)+1)
	for s := startSeg; s <= endSeg; s++ {
		out = append(out, s)
	}
	return out
}

// SegRangeValid 校验分段窗口是否合法（起始非负且不大与结束）。
func SegRangeValid(startSeg, endSeg int32) bool {
	return startSeg >= 0 && endSeg >= startSeg
}

// SegmentSeconds 规整配置里的分段秒数，越界回退默认值。
func SegmentSeconds(v int32) int32 {
	if v <= 0 || v > MaxSegmentSeconds {
		return DefaultSegmentSeconds
	}
	return v
}
