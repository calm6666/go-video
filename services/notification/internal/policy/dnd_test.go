package policy

import (
	"errors"
	"testing"
	"time"

	"go-video/services/notification/model"
)

// 免打扰时段判定：跨天窗口与时区边界（AGENTS.md §1 不允许打扰已设置免打扰的用户）。

func mustLOC(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("本机缺少 tzdata: %v", err)
	}
	return loc
}

func TestParseClock(t *testing.T) {
	ok := map[string]int{"00:00": 0, "23:59": 1439, "22:00": 1320, " 08:30 ": 510, "9:05": 545}
	for in, want := range ok {
		got, err := ParseClock(in)
		if err != nil || got != want {
			t.Errorf("ParseClock(%q) = %d,%v want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "  ", "24:00", "23:60", "-1:00", "8", "0830", "08:30:00", "ab:cd", "08:3a"} {
		if _, err := ParseClock(bad); !errors.Is(err, ErrInvalidClock) {
			t.Errorf("ParseClock(%q) 应报 ErrInvalidClock, got %v", bad, err)
		}
	}
}

func TestLoadLocation(t *testing.T) {
	sh, err := LoadLocation("Asia/Shanghai", "UTC")
	if err != nil || sh.String() != "Asia/Shanghai" {
		t.Fatalf("首选时区应生效: %v %v", sh, err)
	}
	// name 为空时用 fallback。
	utc, err := LoadLocation("   ", "UTC")
	if err != nil || utc != time.UTC {
		t.Fatalf("应回落到 fallback: %v %v", utc, err)
	}
	// 两者都不可用时显式报错，绝不静默退化成 UTC（免打扰窗口会整体偏移 8 小时）。
	if _, err := LoadLocation("Mars/Olympus", "Not-A-Zone"); !errors.Is(err, ErrInvalidTimezone) {
		t.Fatalf("want ErrInvalidTimezone, got %v", err)
	}
	// 关键：配置显式给了时区但写错时，即使 fallback 合法也不能悄悄退化 ——
	// 退化到 UTC 会让全部用户的免打扰窗口偏移，属于静默错误。
	if _, err := LoadLocation("Asia/Shangha", "UTC"); !errors.Is(err, ErrInvalidTimezone) {
		t.Fatalf("拼错的配置时区必须报错，不能退化成 UTC: got %v", err)
	}
}

// TestInQuietHoursSameDay：命中区间含开始不含结束。
func TestInQuietHoursSameDay(t *testing.T) {
	utc := time.UTC
	at := func(h, m int) time.Time { return time.Date(2026, 3, 14, h, m, 0, 0, utc) }
	cases := []struct {
		t    time.Time
		want bool
	}{
		{at(9, 0), false},
		{at(12, 29), false},
		{at(12, 30), true}, // 边界：含开始
		{at(13, 59), true},
		{at(14, 0), false}, // 边界：不含结束
		{at(23, 59), false},
	}
	for _, c := range cases {
		got, err := InQuietHours(c.t, utc, "12:30", "14:00")
		if err != nil {
			t.Fatalf("InQuietHours(%v): %v", c.t, err)
		}
		if got != c.want {
			t.Errorf("%s UTC: 12:30-14:00 命中=%v want %v", c.t.Format("15:04"), got, c.want)
		}
	}
}

// TestInQuietHoursCrossMidnight：22:00-08:00 是跨天窗口，夜间与凌晨都要命中。
func TestInQuietHoursCrossMidnight(t *testing.T) {
	utc := time.UTC
	at := func(day int, h, m int) time.Time { return time.Date(2026, 3, day, h, m, 0, 0, utc) }
	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"入窗前一分钟", at(14, 21, 59), false},
		{"入窗", at(14, 22, 0), true},
		{"深夜", at(14, 23, 30), true},
		{"跨零点到午夜后", at(15, 0, 0), true},
		{"凌晨窗内", at(15, 7, 59), true},
		{"出窗（不含结束）", at(15, 8, 0), false},
		{"白天", at(15, 13, 0), false},
	}
	for _, c := range cases {
		got, err := InQuietHours(c.t, utc, "22:00", "08:00")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s (%s UTC): 命中=%v want %v", c.name, c.t.Format("01-02 15:04"), got, c.want)
		}
	}
}

// TestInQuietHoursTimezoneBoundary：判定必须以用户本地时间为准。
// 同一个 UTC 时刻在北京是早上 8 点（出窗），在 UTC 是午夜（窗内）。
func TestInQuietHoursTimezoneBoundary(t *testing.T) {
	sh := mustLOC(t, "Asia/Shanghai")
	instant := time.Date(2026, 3, 14, 0, 30, 0, 0, time.UTC) // 北京 08:30
	if got, _ := InQuietHours(instant, sh, "22:00", "08:00"); got {
		t.Error("北京时间 08:30 不该落在 22:00-08:00 免打扰窗口内")
	}
	if got, _ := InQuietHours(instant, time.UTC, "22:00", "08:00"); !got {
		t.Error("UTC 时间 00:30 必须落在免打扰窗口内")
	}
	// 北京 21:59 -> 未入窗；22:00 -> 入窗（含开始）。
	before := time.Date(2026, 3, 14, 13, 59, 0, 0, time.UTC)
	after := time.Date(2026, 3, 14, 14, 0, 0, 0, time.UTC)
	if got, _ := InQuietHours(before, sh, "22:00", "08:00"); got {
		t.Error("北京 21:59 不该命中")
	}
	if got, _ := InQuietHours(after, sh, "22:00", "08:00"); !got {
		t.Error("北京 22:00 必须命中")
	}
	// 同一时刻在纽约：冬季 EST(UTC+8/-5) 与夏季 EDT(UTC+4/-4) 判定不同 —— 偏移必须由时区库给出。
	ny := mustLOC(t, "America/New_York")
	winter := time.Date(2026, 1, 15, 12, 30, 0, 0, time.UTC) // 07:30 EST，窗内
	summer := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC) // 08:30 EDT，窗外
	if got, _ := InQuietHours(winter, ny, "22:00", "08:00"); !got {
		t.Error("纽约冬季 07:30 EST 应在免打扰窗口内")
	}
	if got, _ := InQuietHours(summer, ny, "22:00", "08:00"); got {
		t.Error("纽约夏季 08:30 EDT 不应在免打扰窗口内")
	}
}

func TestInQuietHoursDegenerateConfig(t *testing.T) {
	now := time.Date(2026, 3, 14, 23, 0, 0, 0, time.UTC)
	// 未设置时段：不拦。
	for _, pair := range [][2]string{{"", ""}, {"", "08:00"}, {"22:00", ""}, {"  ", "  "}} {
		got, err := InQuietHours(now, time.UTC, pair[0], pair[1])
		if err != nil || got {
			t.Errorf("空时段 %#v 应不拦且不报错: %v %v", pair, got, err)
		}
	}
	// start == end 视为笔误，不能把用户全天静音。
	got, err := InQuietHours(now, time.UTC, "22:00", "22:00")
	if err != nil || got {
		t.Errorf("start==end 应不拦: %v %v", got, err)
	}
	// 非法格式必须显式报错，由上层按 fail-closed 处理。
	if _, err := InQuietHours(now, time.UTC, "25:00", "08:00"); !errors.Is(err, ErrInvalidClock) {
		t.Errorf("want ErrInvalidClock, got %v", err)
	}
	if _, err := InQuietHours(now, time.UTC, "22:00", "08:xx"); !errors.Is(err, ErrInvalidClock) {
		t.Errorf("want ErrInvalidClock, got %v", err)
	}
}

// TestCheckDnd：通道静音是硬偏好（高优先也不能越过），静默时段可被高优先越过。
func TestCheckDnd(t *testing.T) {
	sh := mustLOC(t, "Asia/Shanghai")
	at := func(h, m int) time.Time { return time.Date(2026, 3, 14, h, m, 0, 0, sh) }

	blocked, reason, err := CheckDnd(nil, sh, at(23, 0), model.ChannelPush, PriorityNormal)
	if err != nil || blocked {
		t.Errorf("无偏好记录不应拦截: %v %q %v", blocked, reason, err)
	}

	on := &model.NotificationDndPref{
		Mid: 1, State: model.DndStateOn, QuietStart: "22:00", QuietEnd: "08:00",
		Timezone: "Asia/Shanghai", MutedChannels: 0,
	}
	if blocked, reason, err = CheckDnd(on, sh, at(23, 0), model.ChannelSMS, PriorityNormal); err != nil || !blocked {
		t.Errorf("静默时段内普通短信应被拦: %v %q %v", blocked, reason, err)
	}
	if blocked, _, err = CheckDnd(on, sh, at(9, 0), model.ChannelSMS, PriorityNormal); err != nil || blocked {
		t.Errorf("窗口外不应拦截: %v", blocked)
	}
	// 高优先（验证码/安全提醒）越过时段。
	if blocked, _, err = CheckDnd(on, sh, at(23, 0), model.ChannelSMS, PriorityHigh); err != nil || blocked {
		t.Errorf("高优先应越过时段: %v %v", blocked, err)
	}
	// 但用户显式关闭该通道时任何优先级都不发。
	muted := &model.NotificationDndPref{
		Mid: 1, State: model.DndStateOn, QuietStart: "22:00", QuietEnd: "08:00",
		Timezone: "Asia/Shanghai", MutedChannels: model.MaskOfChannels([]int32{model.ChannelPush}),
	}
	for _, p := range []int32{PriorityLow, PriorityNormal, PriorityHigh} {
		blocked, reason, err := CheckDnd(muted, sh, at(9, 0), model.ChannelPush, p)
		if err != nil || !blocked {
			t.Errorf("优先级 %d 下关闭推送通道仍被发送", p)
		}
		if reason == "" {
			t.Error("拦截必须给出原因")
		}
	}
	// 关闭总开关后时段失效，但静音通道仍然生效。
	off := &model.NotificationDndPref{Mid: 1, State: model.DndStateOff, QuietStart: "22:00", QuietEnd: "08:00"}
	if blocked, _, err := CheckDnd(off, sh, at(23, 0), model.ChannelPush, PriorityNormal); err != nil || blocked {
		t.Errorf("总开关关闭后不应按时段拦截: %v", blocked)
	}
	blocked, _, err = CheckDnd(&model.NotificationDndPref{
		Mid: 1, State: model.DndStateOff, MutedChannels: model.MaskOfChannels([]int32{model.ChannelEmail}),
	}, sh, at(23, 0), model.ChannelEmail, PriorityNormal)
	if err != nil || !blocked {
		t.Errorf("总开关关闭也要尊重通道静音: %v %v", blocked, err)
	}
	// 偏好里的时区优先于调用方默认时区：窗口 09:00-10:00。
	prefTZ := &model.NotificationDndPref{Mid: 1, State: model.DndStateOn, QuietStart: "09:00", QuietEnd: "10:00"}
	// ① UTC 01:30 = 北京 09:30：按偏好时区判定命中，按调用方 loc（UTC）判定不命中。
	prefTZ.Timezone = "Asia/Shanghai"
	blocked, _, err = CheckDnd(prefTZ, time.UTC, time.Date(2026, 3, 14, 1, 30, 0, 0, time.UTC), model.ChannelPush, PriorityNormal)
	if err != nil || !blocked {
		t.Errorf("应按偏好里的时区判定（UTC 01:30 = 北京 09:30 命中 09:00-10:00）: %v %v", blocked, err)
	}
	// ② UTC 09:30 = 北京 17:30：按偏好时区不命中，按调用方 loc 才会命中。
	blocked, _, err = CheckDnd(prefTZ, time.UTC, time.Date(2026, 3, 14, 9, 30, 0, 0, time.UTC), model.ChannelPush, PriorityNormal)
	if err != nil || blocked {
		t.Errorf("北京 17:30 不该被拦（说明误用了调用方时区）: %v %v", blocked, err)
	}
	// 偏好里时区写错时保持调用方 loc（UTC），不静默换成别的时区。
	prefTZ.Timezone = "Mars/Olympus"
	if blocked, _, err = CheckDnd(prefTZ, time.UTC, time.Date(2026, 3, 14, 9, 30, 0, 0, time.UTC),
		model.ChannelPush, PriorityNormal); err != nil || !blocked {
		t.Errorf("非法偏好时区应回落调用方 loc: %v %v", blocked, err)
	}
}
