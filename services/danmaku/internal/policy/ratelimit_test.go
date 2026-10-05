package policy

import "testing"

// TestCheckWindowsBoundary 固定窗口判定的边界：Count 是 Redis INCR 后的值
// （含本次），等于阈值必须放行，阈值加一才拒绝。
func TestCheckWindowsBoundary(t *testing.T) {
	cases := []struct {
		name    string
		windows []Window
		wantOK  bool
		wantDim string
	}{
		{
			name:    "用户窗口未达阈值",
			windows: []Window{{Dimension: "user", Count: 19, Limit: 20}},
			wantOK:  true,
		},
		{
			name:    "用户窗口刚好达到阈值仍放行",
			windows: []Window{{Dimension: "user", Count: 20, Limit: 20}},
			wantOK:  true,
		},
		{
			name:    "用户窗口超阈值拒绝",
			windows: []Window{{Dimension: "user", Count: 21, Limit: 20}},
			wantOK:  false, wantDim: "user",
		},
		{
			name: "两个维度都未超",
			windows: []Window{
				{Dimension: "user", Count: 5, Limit: 20},
				{Dimension: "oid", Count: 100, Limit: 6000},
			},
			wantOK: true,
		},
		{
			name: "返回首个触发维度",
			windows: []Window{
				{Dimension: "user", Count: 21, Limit: 20},
				{Dimension: "oid", Count: 6001, Limit: 6000},
			},
			wantOK: false, wantDim: "user",
		},
		{
			name: "仅内容维度超限",
			windows: []Window{
				{Dimension: "user", Count: 1, Limit: 20},
				{Dimension: "oid", Count: 6001, Limit: 6000},
			},
			wantOK: false, wantDim: "oid",
		},
		{
			name: "阈值为 0 表示该维度不限流",
			windows: []Window{
				{Dimension: "user", Count: 999999, Limit: 0},
				{Dimension: "oid", Count: 1, Limit: 6000},
			},
			wantOK: true,
		},
		{
			name:    "负阈值同样视为不限流",
			windows: []Window{{Dimension: "user", Count: 999999, Limit: -1}},
			wantOK:  true,
		},
		{
			name:    "空窗口列表放行",
			windows: nil,
			wantOK:  true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, dim := CheckWindows(c.windows...)
			if ok != c.wantOK {
				t.Fatalf("CheckWindows(%v) ok = %v, want %v", c.windows, ok, c.wantOK)
			}
			if dim != c.wantDim {
				t.Fatalf("CheckWindows(%v) dim = %q, want %q", c.windows, dim, c.wantDim)
			}
		})
	}
}

// TestCheckWindowsDoesNotMutateInput 判定函数必须是纯函数：
// 计数由 repository 侧的 Redis 维护，这里不得改写窗口值。
func TestCheckWindowsDoesNotMutateInput(t *testing.T) {
	w := []Window{{Dimension: "user", Count: 30, Limit: 20}}
	CheckWindows(w...)
	if w[0].Count != 30 || w[0].Limit != 20 {
		t.Fatalf("CheckWindows 修改了入参： %+v", w)
	}
}
