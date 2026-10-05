package policy

import "testing"

// TestSegNo 校验时间轴毫秒到分段号的映射：分段号必须从 0 开始、
// 按 SegmentSeconds 左闭右开切分，且非法入参有确定性回退。
func TestSegNo(t *testing.T) {
	cases := []struct {
		name   string
		ms     int64
		sec    int32
		expect int32
	}{
		{"零位置", 0, DefaultSegmentSeconds, 0},
		{"段内末尾", 5999, DefaultSegmentSeconds, 0},
		{"段边界进位", 6000, DefaultSegmentSeconds, 1},
		{"跨段", 14500, DefaultSegmentSeconds, 2},
		{"自定义段宽", 20000, 10, 2},
		{"非法秒数回退默认", 13000, 0, 2},
		{"负秒数回退默认", 13000, -5, 2},
		{"超大秒数截断", 13000, MaxSegmentSeconds + 1, 0},
		{"负时间轴按零处理", -100, DefaultSegmentSeconds, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SegNo(c.ms, c.sec); got != c.expect {
				t.Fatalf("SegNo(%d, %d) = %d, want %d", c.ms, c.sec, got, c.expect)
			}
		})
	}
}

// TestSegNoMonotonic 保证分段号随时间轴单调不减，
// 否则客户端按分段拉取会漏弹幕。
func TestSegNoMonotonic(t *testing.T) {
	prev := int32(0)
	for ms := int64(0); ms <= 120000; ms += 37 {
		cur := SegNo(ms, DefaultSegmentSeconds)
		if cur < prev {
			t.Fatalf("SegNo 非单调：ms=%d got %d < prev %d", ms, cur, prev)
		}
		prev = cur
	}
}

func TestSegRangeFromMs(t *testing.T) {
	cases := []struct {
		name               string
		start, end         int64
		sec                int32
		wantStart, wantEnd int32
	}{
		{"同段窗口", 1000, 5000, DefaultSegmentSeconds, 0, 0},
		{"跨段窗口", 1000, 13000, DefaultSegmentSeconds, 0, 2},
		{"右端小于左端按左端处理", 7000, 100, DefaultSegmentSeconds, 1, 1},
		{"负起点按零处理", -1, 6000, DefaultSegmentSeconds, 0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, e := SegRangeFromMs(c.start, c.end, c.sec)
			if s != c.wantStart || e != c.wantEnd {
				t.Fatalf("SegRangeFromMs(%d, %d, %d) = (%d, %d), want (%d, %d)",
					c.start, c.end, c.sec, s, e, c.wantStart, c.wantEnd)
			}
		})
	}
}

// TestSegments 校验窗口展开与上限截断：超出窗口上限只取前 maxSegs 段，
// 不返回错误，由 logic 层先行用 SegRangeValid 判定非法区间。
func TestSegments(t *testing.T) {
	cases := []struct {
		name       string
		start, end int32
		maxSegs    int
		want       []int32
	}{
		{"单段", 3, 3, 10, []int32{3}},
		{"连续段", 2, 4, 10, []int32{2, 3, 4}},
		{"超上限截断", 0, 100, 5, []int32{0, 1, 2, 3, 4}},
		{"逆序按单段处理", 5, 1, 10, []int32{5}},
		{"负起始归零", -2, 1, 10, []int32{0, 1}},
		{"非正上限按一段处理", 7, 9, 0, []int32{7}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Segments(c.start, c.end, c.maxSegs)
			if len(got) != len(c.want) {
				t.Fatalf("Segments(%d, %d, %d) = %v, want %v", c.start, c.end, c.maxSegs, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("Segments(%d, %d, %d) = %v, want %v", c.start, c.end, c.maxSegs, got, c.want)
				}
			}
		})
	}
}

func TestSegRangeValid(t *testing.T) {
	if !SegRangeValid(0, 0) {
		t.Fatal("单段窗口应合法")
	}
	if !SegRangeValid(1, 5) {
		t.Fatal("正序窗口应合法")
	}
	if SegRangeValid(5, 1) {
		t.Fatal("起始大于结束的窗口应判非法")
	}
	if SegRangeValid(-1, 3) {
		t.Fatal("负分段号应判非法")
	}
}

func TestSegmentSeconds(t *testing.T) {
	cases := map[int32]int32{
		0:                     DefaultSegmentSeconds,
		-1:                    DefaultSegmentSeconds,
		1:                     1,
		DefaultSegmentSeconds: DefaultSegmentSeconds,
		MaxSegmentSeconds:     MaxSegmentSeconds,
		MaxSegmentSeconds + 1: DefaultSegmentSeconds,
	}
	for in, want := range cases {
		if got := SegmentSeconds(in); got != want {
			t.Fatalf("SegmentSeconds(%d) = %d, want %d", in, got, want)
		}
	}
}
