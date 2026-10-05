// 本文件是 model 包的手写规则函数（调度与退避计算），不是 goctl 生成产物。

package model

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	// Windows 开发机与精简容器镜像里没有 /usr/share/zoneinfo，
	// 而任务定义允许 Asia/Shanghai 这类具名时区（cron_expr 的解释依赖它）。
	// 只 import 标准库 time/tzdata，不引入第三方依赖（AGENTS.md §1）。
	_ "time/tzdata"
)

// cron 字段位图：minute/hour/dom/month/dow 都用 uint64 位集合表达，
// 解析一次即可反复判定「某个时刻是否命中」，避免每次到期扫描都重新切字符串。
type cronField struct {
	all  bool
	bits uint64
}

func (f cronField) match(v int) bool { return f.all || f.bits&(1<<uint(v)) != 0 }

// cronSpec 一条 5 字段标准 cron 表达式（分 时 日 月 周）。
// 不支持 @daily 之类的宏，也不支持 6 字段秒级表达式：
// 调度粒度到分钟是本服务的既定边界，写进 README 以免误配。
type cronSpec struct {
	minute cronField
	hour   cronField
	dom    cronField
	month  cronField
	dow    cronField
}

// cronFieldRange 一个字段允许的取值区间与名称别名。
type cronFieldRange struct {
	min, max int
	aliases  map[string]int
}

var (
	minuteRange = cronFieldRange{min: 0, max: 59}
	hourRange   = cronFieldRange{min: 0, max: 23}
	domRange    = cronFieldRange{min: 1, max: 31}
	monthRange  = cronFieldRange{min: 1, max: 12, aliases: alphaMonths}
	dowRange    = cronFieldRange{min: 0, max: 7, aliases: alphaDays}
)

var (
	alphaMonths = map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}
	alphaDays = map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}
)

// cronFieldSpecs 5 个字段的解析顺序与取值边界。
var cronFieldSpecs = []struct {
	name string
	rng  cronFieldRange
	slot int // 0 minute,1 hour,2 dom,3 month,4 dow
}{
	{name: "minute", rng: minuteRange, slot: 0},
	{name: "hour", rng: hourRange, slot: 1},
	{name: "dom", rng: domRange, slot: 2},
	{name: "month", rng: monthRange, slot: 3},
	{name: "dow", rng: dowRange, slot: 4},
}

// maxCronScanDays 向后搜索的天数上限：足够覆盖「2 月 29 日 + 特定星期」这类罕见组合，
// 同时保证非法但可解析的表达式不会让到期扫描陷入死循环。
const maxCronScanDays = 366 * 5

// ParseCronExpr 解析标准 5 字段 cron 表达式。
func ParseCronExpr(expr string) (*cronSpec, error) {
	fields := strings.Fields(expr)
	if len(fields) != len(cronFieldSpecs) {
		return nil, fmt.Errorf("%w: cron_expr %q 需要 5 个字段（分 时 日 月 周）", ErrInvalidSchedule, expr)
	}
	spec := &cronSpec{}
	slots := []*cronField{&spec.minute, &spec.hour, &spec.dom, &spec.month, &spec.dow}
	for i, raw := range fields {
		parsed, err := parseCronField(raw, cronFieldSpecs[i].rng, cronFieldSpecs[i].name)
		if err != nil {
			return nil, err
		}
		*slots[i] = parsed
	}
	return spec, nil
}

// ValidateCronExpr 只做合法性判定，供注册/更新前的结构校验使用。
func ValidateCronExpr(expr string) error {
	_, err := ParseCronExpr(expr)
	return err
}

// parseCronField 解析单个字段：支持 * 、a、a-b、a-b/n、a/n、*/n 以及逗号列表和英文别名。
// 允许回绕区间（如 dow 字段的 "fri-mon" / "5-1"）。
func parseCronField(raw string, rng cronFieldRange, name string) (cronField, error) {
	out := cronField{}
	for _, item := range strings.Split(raw, ",") {
		if item == "" {
			return cronField{}, fmt.Errorf("%w: %s 字段存在空项", ErrInvalidSchedule, name)
		}
		body, step, err := splitCronStep(item, rng, name)
		if err != nil {
			return cronField{}, err
		}
		if body == "*" {
			if step == 1 {
				out.all = true
			}
			out.addSpan(rng.min, rng.max, step, rng)
			continue
		}
		low, high, isRange, err := parseCronBounds(body, rng, name, item)
		if err != nil {
			return cronField{}, err
		}
		switch {
		case !isRange && step == 1:
			out.addSpan(low, high, 1, rng)
		case !isRange:
			// "a/n" 按 Vixie cron 语义从 a 一直走到字段上界。
			out.addSpan(low, rng.max, step, rng)
		case low <= high:
			out.addSpan(low, high, step, rng)
		default:
			// 回绕区间：5-1 => [5,max] + [min,1]。
			out.addSpan(low, rng.max, step, rng)
			out.addSpan(rng.min, high, step, rng)
		}
	}
	return out, nil
}

// splitCronStep 拆出 "*/15" 这类项的 body 与步长。
func splitCronStep(item string, rng cronFieldRange, name string) (string, int, error) {
	slash := strings.Index(item, "/")
	if slash < 0 {
		return item, 1, nil
	}
	body := item[:slash]
	stepText := item[slash+1:]
	if stepText == "" {
		return "", 0, fmt.Errorf("%w: %s 字段 %q 缺少步长", ErrInvalidSchedule, name, item)
	}
	step, err := strconv.Atoi(stepText)
	if err != nil || step <= 0 {
		return "", 0, fmt.Errorf("%w: %s 字段步长 %q 非法", ErrInvalidSchedule, name, stepText)
	}
	if body == "" {
		return "", 0, fmt.Errorf("%w: %s 字段 %q 缺少取值范围", ErrInvalidSchedule, name, item)
	}
	return body, step, nil
}

// parseCronBounds 解析 "*" 之外的 body：a 或 a-b。
func parseCronBounds(body string, rng cronFieldRange, name, item string) (int, int, bool, error) {
	dash := strings.Index(body, "-")
	if dash <= 0 {
		v, err := parseCronNumber(body, rng, name, item)
		return v, v, false, err
	}
	low, err := parseCronNumber(body[:dash], rng, name, item)
	if err != nil {
		return 0, 0, true, err
	}
	high, err := parseCronNumber(body[dash+1:], rng, name, item)
	if err != nil {
		return 0, 0, true, err
	}
	return low, high, true, nil
}

// addSpan 把 [low,high] 内按 step 命中的取值写进位图。
func (f *cronField) addSpan(low, high, step int, rng cronFieldRange) {
	if low > high {
		return
	}
	for v := low; v <= high; v += step {
		f.set(v, rng)
	}
}

// set 置位；星期 7 归一为 0（周日）。
func (f *cronField) set(v int, rng cronFieldRange) {
	if rng.max == 7 && v == 7 {
		v = 0
	}
	f.bits |= 1 << uint(v)
}

func parseCronNumber(text string, rng cronFieldRange, name, item string) (int, error) {
	if v, err := strconv.Atoi(text); err == nil {
		if v < rng.min || v > rng.max {
			return 0, fmt.Errorf("%w: %s 字段 %q 取值 %d 超出 %d-%d",
				ErrInvalidSchedule, name, item, v, rng.min, rng.max)
		}
		return v, nil
	}
	if rng.aliases != nil {
		if v, ok := rng.aliases[strings.ToLower(text)]; ok {
			return v, nil
		}
	}
	return 0, fmt.Errorf("%w: %s 字段 %q 无法解析", ErrInvalidSchedule, name, text)
}

// dayMatches 判定某一天是否需要检查。
// dom 与 dow 同时被限定时按 Vixie cron 语义取并集（OR），只限定其一时取交集。
func (c *cronSpec) dayMatches(day time.Time) bool {
	if !c.month.match(int(day.Month())) {
		return false
	}
	domOK := c.dom.match(day.Day())
	dowOK := c.dow.match(int(day.Weekday()))
	switch {
	case !c.dom.all && !c.dow.all:
		return domOK || dowOK
	case !c.dom.all:
		return domOK
	case !c.dow.all:
		return dowOK
	default:
		return true
	}
}

// nextAfter 返回严格晚于 after 的下一个命中时刻（秒位归零）。
func (c *cronSpec) nextAfter(after time.Time) (time.Time, bool) {
	loc := after.Location()
	day := time.Date(after.Year(), after.Month(), after.Day(), 0, 0, 0, 0, loc)
	for i := 0; i < maxCronScanDays; i++ {
		if c.dayMatches(day) {
			for h := 0; h < 24; h++ {
				if !c.hour.match(h) {
					continue
				}
				for m := 0; m < 60; m++ {
					if !c.minute.match(m) {
						continue
					}
					cand := time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, loc)
					// 夏令时切换会让某些时刻不存在，time.Date 会归一化到别的日/时，
					// 这里以「回读一致」为准丢弃伪候选。
					if cand.Day() != day.Day() || cand.Hour() != h || cand.Minute() != m {
						continue
					}
					if cand.After(after) {
						return cand, true
					}
				}
			}
		}
		day = day.AddDate(0, 0, 1)
	}
	return time.Time{}, false
}

// cronCache 表达式解析结果缓存：到期扫描每轮都会重算 next_fire_at，
// 表达式数量等于任务数（受注册流程约束），因此缓存规模可控，超限直接清空重来，
// 不做 LRU 以免为调度辅助引入额外的数据结构复杂度。
var (
	cronCacheMu  sync.Mutex
	cronCacheMap = map[string]*cronSpec{}
)

const cronCacheLimit = 256

// cachedCronSpec 解析并缓存表达式。
func cachedCronSpec(expr string) (*cronSpec, error) {
	cronCacheMu.Lock()
	if spec, ok := cronCacheMap[expr]; ok {
		cronCacheMu.Unlock()
		return spec, nil
	}
	cronCacheMu.Unlock()

	spec, err := ParseCronExpr(expr)
	if err != nil {
		return nil, err
	}
	cronCacheMu.Lock()
	if len(cronCacheMap) >= cronCacheLimit {
		cronCacheMap = map[string]*cronSpec{}
	}
	cronCacheMap[expr] = spec
	cronCacheMu.Unlock()
	return spec, nil
}

// LoadZone 解析时区名；tz 为空时回落 defaultTZ，两者都不可用时返回 ErrInvalidSchedule，
// 绝不「悄悄按 UTC 跑」——那会让 03:00 的任务在错误的时间点叫醒下游。
func LoadZone(tz, defaultTZ string) (*time.Location, error) {
	name := strings.TrimSpace(tz)
	if name == "" {
		name = strings.TrimSpace(defaultTZ)
	}
	if name == "" {
		name = "UTC"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%w: timezone=%q: %v", ErrInvalidSchedule, name, err)
	}
	return loc, nil
}

// NextFireAfter 计算任务在 anchorUnix 之后的下一个计划时刻（Unix 秒）。
// 返回 0 表示手动任务不参与到期扫描。
func NextFireAfter(d *TaskDefinition, defaultTZ string, anchorUnix int64) (int64, error) {
	if d == nil {
		return 0, ErrTaskNotFound
	}
	switch d.ScheduleType {
	case ScheduleTypeManual:
		return 0, nil
	case ScheduleTypeInterval:
		if d.IntervalSeconds <= 0 {
			return 0, fmt.Errorf("%w: interval_seconds=%d", ErrInvalidSchedule, d.IntervalSeconds)
		}
		return anchorUnix + int64(d.IntervalSeconds), nil
	case ScheduleTypeCron:
		spec, err := cachedCronSpec(d.CronExpr)
		if err != nil {
			return 0, err
		}
		loc, err := LoadZone(d.Timezone, defaultTZ)
		if err != nil {
			return 0, err
		}
		next, ok := spec.nextAfter(time.Unix(anchorUnix, 0).In(loc))
		if !ok {
			return 0, fmt.Errorf("%w: cron_expr %q 在可搜索范围内没有命中时刻",
				ErrInvalidSchedule, d.CronExpr)
		}
		return next.Unix(), nil
	default:
		return 0, fmt.Errorf("%w: schedule_type=%d", ErrInvalidSchedule, d.ScheduleType)
	}
}

// FirstFireAt 给出任务注册后的首个计划时刻。
// 具名时区在注册期就要校验（配置错了要让注册失败，而不是让任务永远不跑）。
func FirstFireAt(d *TaskDefinition, defaultTZ string, now int64) (int64, error) {
	if d == nil {
		return 0, ErrTaskNotFound
	}
	if d.ScheduleType == ScheduleTypeCron {
		if _, err := LoadZone(d.Timezone, defaultTZ); err != nil {
			return 0, err
		}
	}
	return NextFireAfter(d, defaultTZ, now)
}

// BackoffSeconds 计算第 attempt 次尝试失败后的退避秒数：min(base*2^(attempt-1), max)。
// max<=0 表示不设上限；attempt<1 时按首次处理。溢出与负值都收敛到上限，
// 避免退避算成负数导致「失败越多跑得越勤」。
func BackoffSeconds(attempt, baseSeconds, maxSeconds int32) int64 {
	if baseSeconds <= 0 {
		baseSeconds = 1
	}
	if attempt < 1 {
		attempt = 1
	}
	const hardCap int64 = 86400 * 7 // 退避绝对天花板：一周，超过即视为配置错误
	delta := int64(baseSeconds)
	for i := int32(1); i < attempt; i++ {
		if delta > hardCap/2 {
			delta = hardCap
			break
		}
		delta *= 2
	}
	if maxSeconds > 0 && delta > int64(maxSeconds) {
		delta = int64(maxSeconds)
	}
	if delta > hardCap {
		delta = hardCap
	}
	return delta
}

// NextRetryAt 按退避策略给出下一次可执行时间（Unix 秒）。
func NextRetryAt(now int64, attempt, baseSeconds, maxSeconds int32) int64 {
	return now + BackoffSeconds(attempt, baseSeconds, maxSeconds)
}

// RetryAllowed 判定第 attempt 次尝试失败后是否还能自动重试。
// maxAttempts 含首次，因此 attempt < maxAttempts 才有下一次。
func RetryAllowed(attempt, maxAttempts int32) bool {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	return attempt < maxAttempts
}

// LeaseExpired 判定租约是否已过期可被抢占。
// expire_at<=0 表示从未持有或已释放，同样视为「可被抢占」，
// 但调用方必须先区分「无人持有」与「他人持有」再决定 outcome，
// 因此这里只回答时间维度的问题。
func LeaseExpired(expireAt, now int64) bool { return expireAt <= now }

// ClampTTL 把租约 TTL 收敛到 [min,max]（min>max 时视为配置矛盾，返回原值）。
func ClampTTL(ttl, min, max int64) int64 {
	if min > max {
		return ttl
	}
	if ttl < min {
		return min
	}
	if ttl > max {
		return max
	}
	return ttl
}

// TaskStateName 任务状态的稳定文本（审计 with from_state/to_state 用可读文本，
// 避免运维要靠枚举数值表猜「2 是什么」）。
func TaskStateName(state int32) string {
	switch state {
	case TaskStateEnabled:
		return "enabled"
	case TaskStatePaused:
		return "paused"
	case TaskStateDisabled:
		return "disabled"
	default:
		return "unspecified"
	}
}

// RunStateName 执行状态文本。
func RunStateName(state int32) string {
	switch state {
	case RunStatePending:
		return "pending"
	case RunStateRunning:
		return "running"
	case RunStateRetrying:
		return "retrying"
	case RunStateSucceeded:
		return "succeeded"
	case RunStateFailed:
		return "failed"
	case RunStateTimeout:
		return "timeout"
	case RunStateCanceled:
		return "canceled"
	case RunStateSkipped:
		return "skipped"
	default:
		return "unspecified"
	}
}

// ValidAuditAction 判定审计动作是否在本服务约定的集合内。
func ValidAuditAction(action string) bool {
	switch action {
	case AuditActionRegister, AuditActionUpdate, AuditActionPause, AuditActionResume,
		AuditActionDisable, AuditActionTrigger, AuditActionRetry, AuditActionReplay:
		return true
	default:
		return false
	}
}

// ValidRunState / ValidTaskState / ValidScheduleType / ValidMisfirePolicy / ValidTriggerType
// 枚举合法性判定，供 logic 的参数校验与 model 规则测试共用。

func ValidRunState(state int32) bool {
	return state >= RunStatePending && state <= RunStateSkipped
}

func ValidTaskState(state int32) bool {
	return state >= TaskStateEnabled && state <= TaskStateDisabled
}

func ValidScheduleType(t int32) bool {
	return t >= ScheduleTypeCron && t <= ScheduleTypeManual
}

func ValidMisfirePolicy(p int32) bool {
	return p >= MisfirePolicyFireOnceNow && p <= MisfirePolicyFireAll
}

func ValidTriggerType(t int32) bool {
	return t >= TriggerTypeScheduled && t <= TriggerTypeReplay
}

// AuditActions 返回受支持的审计动作（升序），供测试与健康度输出核对。
func AuditActions() []string {
	out := []string{AuditActionRegister, AuditActionUpdate, AuditActionPause, AuditActionResume,
		AuditActionDisable, AuditActionTrigger, AuditActionRetry, AuditActionReplay}
	sort.Strings(out)
	return out
}
