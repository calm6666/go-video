package model

import (
	"hash/crc32"
	"strconv"
	"strings"
)

// 本文件是灰度判定的**唯一实现**，且是纯函数（不碰数据库、不碰时间）。
// 目的有两个：
//  1. 写入侧用它校验规则「到底会命中谁」，读取侧用它决定「当前请求命中哪条」，
//     两侧共用一套语义，控制台展示与实际放量不可能出现口径分歧；
//  2. 单测不需要 DB 就能覆盖分桶、版本区间、尾号、白名单与时间窗口。

// RolloutInput 一次运行时解析的灰度输入（对应 proto TargetContext 的服务端形态）。
type RolloutInput struct {
	// Platform 端标识，0 表示调用方未声明（此时任何带端条件的规则都不命中）。
	Platform int32
	// AppVersion 客户端版本号，空表示未知。
	AppVersion string
	// Mid 当前登录用户，0 表示未登录。
	Mid int64
	// IgnoreRollout true 表示强制走正式版本（后台预览/排障）。
	IgnoreRollout bool
}

// MaxWhitelistMids 白名单规模上限。白名单是给少量内部账号验证用的排障工具，
// 不是放量手段——需要大面积放量必须走 percentage 或 app_version，
// 否则「谁被放量了」这个问题会变成一次线性扫描。
const MaxWhitelistMids = 200

// MaxMidSuffixes 尾号个数上限（十进制只有 0-9，留 1 个余量给去重前的输入）。
const MaxMidSuffixes = 10

// BucketCount 百分比分桶的桶数。固定 100 让 percentage 字段就是「百分比」，
// 不随实现漂移；改成 10000 会让 5% 与 50% 的写法混淆。
const BucketCount = 100

// PercentageBucket 计算 mid 在指定配置键下的分桶（0..99）。
//
// 混合 cfg_key 的原因：如果只按 mid 分桶，所有配置灰度的都是同一批人，
// 「A 配置先放量 5% 验证过的人」会在 B 配置上再次成为首批受害者，
// 灰度就失去了「不同人群交叉验证」的意义。
// 用 crc32（IEEE）而不是 Go 的 murmur3/xxhash：那些是哈希库实现细节，
// 跨语言重算（排障脚本、Python 复算）会不一致，crc32 在任何语言里都是定死的表。
func PercentageBucket(cfgKey string, mid int64) int {
	if mid <= 0 {
		return -1 // 未登录不参与分桶，见 Matched 的注释
	}
	return int(crc32.ChecksumIEEE([]byte(cfgKey+":"+strconv.FormatInt(mid, 10))) % BucketCount)
}

// CompareAppVersion 按点分十进制逐段比较，缺段按 0 补（"7.2" == "7.2.0"）。
// 只接受数字段；任何非法段返回 (0, false)，调用方必须拒绝而不是按字符串序比较——
// 字符串序下 "7.10.0" < "7.2.0"，那会把新版本判成旧版本。
func CompareAppVersion(a, b string) (int, bool) {
	as, ok1 := versionParts(a)
	bs, ok2 := versionParts(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int64
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

func versionParts(v string) ([]int64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, false
	}
	segs := strings.Split(v, ".")
	if len(segs) > 4 {
		return nil, false
	}
	out := make([]int64, 0, len(segs))
	for _, s := range segs {
		if s == "" {
			return nil, false
		}
		for _, r := range s {
			if r < '0' || r > '9' {
				return nil, false
			}
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// AppVersionInRange 判定版本落在 [min, max] 闭区间；空边界表示该侧不设限。
// 未知版本（输入为空或格式非法）一律 false：宁可不放量，也不把配置推给
// 一个无法判定新旧的客户端。
func AppVersionInRange(version, minVer, maxVer string) bool {
	if minVer == "" && maxVer == "" {
		return true
	}
	if version == "" {
		return false
	}
	if minVer != "" {
		c, ok := CompareAppVersion(version, minVer)
		if !ok || c < 0 {
			return false
		}
	}
	if maxVer != "" {
		c, ok := CompareAppVersion(version, maxVer)
		if !ok || c > 0 {
			return false
		}
	}
	return true
}

// NormalizeMidSuffixes 校验并归一化尾号列表为 "0,3,7"（升序、去重、单个数字）。
// 归一化是必须的：不排序不去重会让同一集合出现多种存储形态，
// 「这条规则和那条是否等价」就无法判定。
func NormalizeMidSuffixes(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	seen := map[byte]bool{}
	digits := make([]byte, 0, 10)
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if len(part) != 1 || part[0] < '0' || part[0] > '9' {
			return "", ErrMidSuffixInvalid
		}
		if seen[part[0]] {
			continue
		}
		seen[part[0]] = true
		digits = append(digits, part[0])
		if len(digits) > MaxMidSuffixes {
			return "", ErrMidSuffixInvalid
		}
	}
	for i := 1; i < len(digits); i++ {
		for j := i; j > 0 && digits[j] < digits[j-1]; j-- {
			digits[j-1], digits[j] = digits[j], digits[j-1]
		}
	}
	return strings.Join(splitDigits(digits), ","), nil
}

func splitDigits(ds []byte) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, string(d))
	}
	return out
}

// MidSuffixHit 判定 mid 的十进制尾号是否落在列表内。mid<=0 一律 false。
func MidSuffixHit(mid int64, suffixes string) bool {
	if mid <= 0 || suffixes == "" {
		return false
	}
	s := strconv.FormatInt(mid, 10)
	last := s[len(s)-1:]
	for _, part := range strings.Split(suffixes, ",") {
		if part == last {
			return true
		}
	}
	return false
}

// WhitelistHit 判定 mid 是否在白名单存储形态 ",11,22," 内。
func WhitelistHit(mid int64, whitelist string) bool {
	if mid <= 0 {
		return false
	}
	return IDListContains(whitelist, mid)
}

// ModeName 返回灰度方式的小写标识（回参 rollout_mode 用它，便于日志与前端排障对齐）。
// 它只是标签，不驱动任何客户端行为。
func ModeName(mode int32) string {
	switch mode {
	case ModeFull:
		return "full"
	case ModePercentage:
		return "percentage"
	case ModeAppVersion:
		return "app_version"
	case ModePlatform:
		return "platform"
	case ModeMidSuffix:
		return "mid_suffix"
	case ModeWhitelist:
		return "whitelist"
	default:
		return "unspecified"
	}
}

// ValidateRuleShape 校验一条规则的形态（写入前调用）。
//
// 判定语义固定为「非空维度取交集」，mode 声明的是主维度：
//   - mode=FULL 时不允许再填任何维度，否则运营看到的「全量」实际藏着百分比条件；
//   - mode=其它时，该 mode 对应的必填维度必须非空，其余已填维度作为附加 AND 条件；
//
// 这样规则的可读性（mode 标签）与真实生效范围（交集）永远一致。
// cfgKey 参与分桶，因此由调用方在 Matched 时传入，这里不校验。
func ValidateRuleShape(r *RolloutRule, maxWhitelist int) error {
	if r.Version <= 0 {
		return ErrVersionNotFound
	}
	// 规则名是 (config_id, version, name) 唯一键的一列：空由 Upsert 侧报
	// ErrRuleNameRequired，超长必须在这里拒，否则整条 upsert 以 MySQL 1406 失败。
	if exceedsColumn(r.Name, MaxRuleNameChars) {
		return ErrRuleNameTooLong
	}
	if !ValidRolloutMode(r.Mode) {
		return ErrRuleModeRequired
	}
	if r.Percentage < 0 || r.Percentage > 100 {
		return ErrPercentageOutOfRange
	}
	if r.StartAt < 0 || r.EndAt < 0 || (r.EndAt > 0 && r.EndAt <= r.StartAt && r.StartAt > 0) {
		return ErrRuleTimeRangeInvalid
	}
	if _, err := NormalizeMidSuffixes(r.MidSuffixes); err != nil {
		return err
	}
	if r.AppVersionMin != "" {
		if exceedsColumn(r.AppVersionMin, MaxAppVersionChars) {
			return ErrAppVersionTooLong
		}
		if _, ok := CompareAppVersion(r.AppVersionMin, r.AppVersionMin); !ok {
			return ErrAppVersionRangeInvalid
		}
	}
	if r.AppVersionMax != "" {
		if exceedsColumn(r.AppVersionMax, MaxAppVersionChars) {
			return ErrAppVersionTooLong
		}
		if _, ok := CompareAppVersion(r.AppVersionMax, r.AppVersionMax); !ok {
			return ErrAppVersionRangeInvalid
		}
	}
	if r.AppVersionMin != "" && r.AppVersionMax != "" {
		// 区间倒挂等于空集：规则永远不命中，属于「配了但没配」的典型事故。
		if c, ok := CompareAppVersion(r.AppVersionMin, r.AppVersionMax); !ok || c > 0 {
			return ErrAppVersionRangeInvalid
		}
	}
	platforms, err := ValidatePlatformList(r.Platforms, maxPlatformCount)
	if err != nil {
		return err
	}
	wl, err := ValidateIDList(r.WhitelistList(), maxWhitelist)
	if err != nil {
		if err == ErrBatchTooLarge {
			return ErrWhitelistTooLarge
		}
		return err
	}

	filled := r.Percentage > 0 || r.AppVersionMin != "" || r.AppVersionMax != "" ||
		len(platforms) > 0 || r.MidSuffixes != "" || len(wl) > 0
	switch r.Mode {
	case ModeFull:
		if filled {
			return ErrRuleModeMismatch
		}
	case ModePercentage:
		if r.Percentage <= 0 {
			return ErrRuleModeMismatch
		}
	case ModeAppVersion:
		if r.AppVersionMin == "" && r.AppVersionMax == "" {
			return ErrRuleModeMismatch
		}
	case ModePlatform:
		if len(platforms) == 0 {
			return ErrRuleModeMismatch
		}
	case ModeMidSuffix:
		if r.MidSuffixes == "" {
			return ErrRuleModeMismatch
		}
	case ModeWhitelist:
		if len(wl) == 0 {
			return ErrRuleModeMismatch
		}
	}
	return nil
}

// maxPlatformCount 平台只有四端，超过即说明列表被写坏了。
const maxPlatformCount = 4

// InTimeWindow 判定规则在 ts 时刻是否处于生效窗口内。
// start_at = 0 表示立即生效；end_at = 0 表示不设截止。
func (r *RolloutRule) InTimeWindow(ts int64) bool {
	if r.StartAt > 0 && ts < r.StartAt {
		return false
	}
	if r.EndAt > 0 && ts >= r.EndAt {
		return false
	}
	return true
}

// EffectiveAt 判定规则是否「启用 + 在时间窗口内」。
func (r *RolloutRule) EffectiveAt(ts int64) bool {
	return r != nil && r.State == StateOn && r.InTimeWindow(ts)
}

// Matched 判定一次解析请求是否命中本规则（不含时间窗口与 state 判定，
// 由 EffectiveAt 先筛，方便调用方分别统计「规则没生效」与「条件不满足」）。
func (r *RolloutRule) Matched(cfgKey string, in RolloutInput) bool {
	if r == nil {
		return false
	}
	// 主维度。
	switch r.Mode {
	case ModeFull:
		// 全量：只看附加维度与时间窗口。
	case ModePercentage:
		// 未登录不参与分桶：mid=0 时把匿名流量整体当成一个桶会让他们一起被放量，
		// 而匿名请求量级又大得多，灰度的意义正好在于控制爆炸半径。
		if in.Mid <= 0 {
			return false
		}
		if PercentageBucket(cfgKey, in.Mid) >= int(r.Percentage) {
			return false
		}
	case ModeAppVersion:
		if !AppVersionInRange(in.AppVersion, r.AppVersionMin, r.AppVersionMax) {
			return false
		}
	case ModePlatform:
		if in.Platform == 0 || !PlatformListContains(r.Platforms, in.Platform) {
			return false
		}
	case ModeMidSuffix:
		if !MidSuffixHit(in.Mid, r.MidSuffixes) {
			return false
		}
	case ModeWhitelist:
		if !WhitelistHit(in.Mid, r.WhitelistMids) {
			return false
		}
	default:
		return false
	}
	// 附加维度：非空即参与 AND（platforms 在 ModePlatform 下已判过，重复判定结果一致）。
	if r.Platforms != "" && !PlatformListContains(r.Platforms, in.Platform) {
		return false
	}
	if r.Mode != ModeAppVersion && (r.AppVersionMin != "" || r.AppVersionMax != "") {
		if !AppVersionInRange(in.AppVersion, r.AppVersionMin, r.AppVersionMax) {
			return false
		}
	}
	if r.Mode != ModeMidSuffix && r.MidSuffixes != "" && !MidSuffixHit(in.Mid, r.MidSuffixes) {
		return false
	}
	return true
}

// PickRollout 从候选规则里挑出命中的那条：按 priority 升序、rule_id 升序取首个匹配。
// 排序在调用方（model.RolloutRuleModel.ListCandidates）用 ORDER BY 保证，这里只按序扫描，
// 因此「谁先命中」的判定与 SQL 顺序严格一致，不会因为 Go 侧重新排序而改变。
func PickRollout(rules []*RolloutRule, cfgKey string, in RolloutInput, ts int64) *RolloutRule {
	if in.IgnoreRollout {
		return nil
	}
	for _, r := range rules {
		if !r.EffectiveAt(ts) {
			continue
		}
		if r.Matched(cfgKey, in) {
			return r
		}
	}
	return nil
}
