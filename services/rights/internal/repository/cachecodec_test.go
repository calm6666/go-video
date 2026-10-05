package repository

// cachecodec_test.go 钉住 CheckPlayable 缓存的**线格式**。
// 这个编码是 Redis 里跨进程读写的唯一形状（logic 只看见 bool/int64），
// 解析错位会把「有版权」读成「没版权」或反过来，且不会报错——所以必须单独测。

import "testing"

func TestCheckCacheKeySeparatesTypeAndRegion(t *testing.T) {
	// 同一内容不同 contentType / region 必须是不同 key：跨地区授权互不覆盖。
	a := keyCheckPlayable(5, 1, "CN")
	if b := keyCheckPlayable(5, 2, "CN"); a == b {
		t.Errorf("contentType 未进 key：%s", a)
	}
	if b := keyCheckPlayable(5, 1, "TW"); a == b {
		t.Errorf("region 未进 key：%s", a)
	}
	if a != "rights:chk:5:1:CN" {
		t.Errorf("key 格式漂移 = %q, want %q", a, "rights:chk:5:1:CN")
	}
	if got := keyCheckPlayable(5, 1, "CN:HK"); got == a {
		t.Errorf("region 含冒号时未与分隔符区分：%s", got)
	}
}

func TestCheckCacheRoundTrip(t *testing.T) {
	for _, c := range []struct {
		label    string
		playable bool
		windowID int64
		endTime  int64
	}{
		{"可播", true, 42, 1_700_000_000},
		{"不可播（负缓存）", false, 0, 0},
		{"零窗口但带到期时间", false, 0, 1_700_000_000},
		{"极大 ID", true, 1 << 40, 1<<62 - 1},
	} {
		raw := formatCheckCache(c.playable, c.windowID, c.endTime)
		playable, wid, et, ok, err := parseCheckCache(raw)
		if err != nil {
			t.Errorf("%s：解析出错 %v", c.label, err)
		}
		if !ok {
			t.Errorf("%s：刚写出的值被判为 miss，raw=%q", c.label, raw)
		}
		if playable != c.playable || wid != c.windowID || et != c.endTime {
			t.Errorf("%s：往返 = %v/%d/%d, want %v/%d/%d（raw=%q）",
				c.label, playable, wid, et, c.playable, c.windowID, c.endTime, raw)
		}
	}
}

func TestCheckCacheRejectsMalformedAsMiss(t *testing.T) {
	// 非法值一律降级为「miss」（ok=false, err=nil）：让调用方回源查库，
	// 既不能当成「不可播」（误拒真实版权），也不能报错（阻塞播放）。
	for _, c := range []struct {
		label string
		raw   string
	}{
		{"空串", ""},
		{"段数不足", "1:42"},
		{"只有 playable", "1"},
		{"window_id 非数字", "1:abc:1700000000"},
		{"end_time 非数字", "1:42:later"},
		// SplitN(raw, ":", 3) 将 end_time 段留给 ParseInt：多出来的冒号让 "17:99" 解析失败。
		{"段数超了", "1:42:17:99"},
	} {
		playable, wid, et, ok, err := parseCheckCache(c.raw)
		if err != nil {
			t.Errorf("%s：非法值应静默降级，不应报错（%v）", c.label, err)
		}
		if ok {
			t.Errorf("%s：%q 被判为命中缓存", c.label, c.raw)
		}
		if playable || wid != 0 || et != 0 {
			t.Errorf("%s：%q miss 时应返回零值，实际 %v/%d/%d", c.label, c.raw, playable, wid, et)
		}
	}
}

// TestCheckCacheUnknownPlayableByteFailsClosed 钉住一个刻意的方向选择：
// 首段既不是 "0" 也不是 "1" 时（只可能来自外部写入或旧版本格式），
// 解析仍算**命中**且 playable=false —— 即按「不可播」处理，而不是回源放行。
// 这与 AGENTS.md §8「版权判定宁可拒绝」一致；若日后改成回源，请连这条用例与 README 一起改。
func TestCheckCacheUnknownPlayableByteFailsClosed(t *testing.T) {
	playable, wid, et, ok, err := parseCheckCache("2:42:1700000000")
	if err != nil {
		t.Errorf("解析出错 %v", err)
	}
	if !ok {
		t.Errorf("非 0/1 首段应仍算命中（并落回不可播），实际 miss")
	}
	if playable {
		t.Errorf("未知 playable 字节被判为可播")
	}
	if wid != 42 || et != 1_700_000_000 {
		t.Errorf("其余段仍按原值解析 = %d/%d, want 42/1700000000", wid, et)
	}
}
