package policy

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go-video/services/notification/model"
)

// 免打扰与通道偏好策略（AGENTS.md §1：通知只服务业务提醒，不做营销/广告推送）。

// ErrInvalidClock 时段格式不合法。
var ErrInvalidClock = errors.New("notification/policy: quiet hours must be HH:MM")

// ErrInvalidTimezone 时区名无法加载。
var ErrInvalidTimezone = errors.New("notification/policy: invalid IANA timezone")

// ParseClock 把 "HH:MM" 解析成当天分钟数（0..1439）。
func ParseClock(s string) (int, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return 0, fmt.Errorf("%w: empty", ErrInvalidClock)
	}
	parts := strings.Split(v, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%w: %q", ErrInvalidClock, s)
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("%w: %q", ErrInvalidClock, s)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("%w: %q out of range", ErrInvalidClock, s)
	}
	return h*60 + m, nil
}

// LoadLocation 加载 IANA 时区；只有 name 为空时才使用 fallback。
// name 非空但加载失败时必须报错：服务启动即校验配置时区，
// 绝不能悄悄退化成 fallback（UTC）导致所有用户的免打扰窗口整体偏移。
func LoadLocation(name, fallback string) (*time.Location, error) {
	if n := strings.TrimSpace(name); n != "" {
		loc, err := time.LoadLocation(n)
		if err != nil {
			return nil, fmt.Errorf("%w: %q: %v", ErrInvalidTimezone, name, err)
		}
		return loc, nil
	}
	if f := strings.TrimSpace(fallback); f != "" {
		loc, err := time.LoadLocation(f)
		if err != nil {
			return nil, fmt.Errorf("%w: fallback %q: %v", ErrInvalidTimezone, fallback, err)
		}
		return loc, nil
	}
	return nil, fmt.Errorf("%w: %q/%q", ErrInvalidTimezone, name, fallback)
}

// InQuietHours 判断时刻 t 是否落在用户免打扰时段内。
//
// 规则（均以 loc 本地时间为准，天然覆盖夏令时与跨天）：
//   - start 或 end 为空：不设时段，返回 false；
//   - start == end：视为无效配置，返回 false（不因为笔误把用户全天静音）；
//   - start <  end：命中 [start, end)，边界含开始不含结束；
//   - start >  end：跨天窗口，命中 [start, 24:00) ∪ [00:00, end)。
func InQuietHours(t time.Time, loc *time.Location, start, end string) (bool, error) {
	if strings.TrimSpace(start) == "" || strings.TrimSpace(end) == "" {
		return false, nil
	}
	s, err := ParseClock(start)
	if err != nil {
		return false, err
	}
	e, err := ParseClock(end)
	if err != nil {
		return false, err
	}
	if s == e {
		return false, nil
	}
	if loc == nil {
		loc = time.Local
	}
	cur := t.In(loc)
	mins := cur.Hour()*60 + cur.Minute()
	if s < e {
		return mins >= s && mins < e, nil
	}
	return mins >= s || mins < e, nil
}

// CheckDnd 综合判断某条投递是否被免打扰拦截。
// 返回 blocked=true 时 reason 说明原因；PRIORITY_HIGH（安全提醒/验证码）可越过免打扰时段，
// 但不能越过“用户显式关闭该通道”的偏好，也不能越过频控。
func CheckDnd(pref *model.NotificationDndPref, loc *time.Location, now time.Time, channel, priority int32) (blocked bool, reason string, err error) {
	if pref == nil {
		return false, "", nil
	}
	if model.IsChannelMuted(pref.MutedChannels, channel) {
		return true, "suppressed: channel muted by user preference", nil
	}
	if pref.State != model.DndStateOn {
		return false, "", nil
	}
	if priority == PriorityHigh {
		// 高优先只跳过时段静音，仍然记录原因便于运营核对。
		return false, "", nil
	}
	if tz := strings.TrimSpace(pref.Timezone); tz != "" {
		// 用户偏好里的时区优先；不可加载时保持调用方传入的 loc（配置默认时区），
		// 不静默退化成 UTC，否则免打扰窗口会整体偏移。
		if prefLoc, lerr := LoadLocation(tz, ""); lerr == nil {
			loc = prefLoc
		}
	}
	inWindow, ierr := InQuietHours(now, loc, pref.QuietStart, pref.QuietEnd)
	if ierr != nil {
		return false, "", ierr
	}
	if inWindow {
		return true, "suppressed: within user quiet hours", nil
	}
	return false, "", nil
}

// 优先级常量：与 rpc/notification.proto 的 Priority 一致，
// 单独定义是为了让 repository/consumer 不必依赖 rpc 包。
const (
	// PriorityLow 低优先。
	PriorityLow int32 = 1
	// PriorityNormal 普通。
	PriorityNormal int32 = 2
	// PriorityHigh 高优先（安全提醒、验证码）。
	PriorityHigh int32 = 3
)

// IsValidPriority 判断优先级取值是否合法（0 表示未指定，由调用方回落为普通）。
func IsValidPriority(p int32) bool { return p >= 0 && p <= PriorityHigh }
