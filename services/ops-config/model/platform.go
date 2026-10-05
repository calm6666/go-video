package model

import (
	"sort"
	"strconv"
	"strings"
)

// 本文件是「端标识」的类型与存储口径的唯一定义处。
//
// 平台枚举取值来自哪里（唯一事实源）
//   - rpc/opsconfig.proto 的 `enum ClientPlatform`：ANDROID=1、IOS=2、HARMONY=3、
//     DESKTOP=4，UNSPECIFIED=0。项目不支持小程序（AGENTS.md §1、§6），
//     因此这里不存在第五个端；新增端必须先改 proto 再改本文件，两侧数值必须一致。
//   - errors.go 的 PlatformAndroid/PlatformIOS/PlatformHarmony/PlatformDesktop 与
//     ValidPlatform 是这份枚举在 Go 侧的镜像常量与值域判定，本文件不再另立数值。
//
// 整型口径（本轮修复的编译错误就来自两侧混用，改动必须整体一致）
//   1. Go 侧端标识一律 int32：protobuf 枚举的 Go 类型就是 int32，
//      ValidPlatform、RolloutInput.Platform、NormalizePlatformList、ValidatePlatformList
//      全部收 int32，不在各调用点做窄化转换（窄化点才是截断风险点）。
//   2. 通用 ID 列表（zone_ids / tag_ids / whitelist_mids）一律 int64，
//      因为它们的取值域是别表主键（BIGINT）。
//   3. 端列表**复用** ID 列表的存储形态 ",1,4,"（首尾带逗号），于是
//      「按端过滤」与「按分区过滤」共用同一套 LIKE 语义。本文件的
//      NormalizePlatformList / ValidatePlatformList / PlatformListContains /
//      PlatformLikeArg 是 int32 与 int64 之间唯一的四个转换点，
//      每一次转换都先经 ValidPlatform 限定值域 1..4，因此不可能溢出。

// NormalizePlatformList 校验并归一化端列表为存储形态 ",1,4,"（升序、去重）。
// 空列表返回空串，语义是「不限端」——这与「列表非空但不含当前端」不同，
// 后者在 RolloutRule.Matched / 坑位过滤里是明确不命中。
// 未知端（含 UNSPECIFIED=0 与任何 >4 的值）一律拒绝：写进库的端标识必须能被
// ValidPlatform 复验，否则读取侧无法区分「配错了」和「协议新加了端」。
func NormalizePlatformList(platforms []int32) (string, error) {
	if len(platforms) == 0 {
		return "", nil
	}
	seen := make(map[int32]struct{}, len(platforms))
	uniq := make([]int32, 0, len(platforms))
	for _, p := range platforms {
		if !ValidPlatform(p) {
			return "", ErrPlatformUnknown
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		uniq = append(uniq, p)
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i] < uniq[j] })
	ids := make([]int64, 0, len(uniq))
	for _, p := range uniq {
		ids = append(ids, int64(p))
	}
	return IDListString(ids), nil
}

// ValidatePlatformList 解析并复验库里的端列表（存储形态 → 升序去重 int32 列表）。
// 与 NormalizePlatformList 成对：写入前归一化、校验时按同一口径重解析，
// 因此「这条规则会给哪些端放量」的判定与真实存储严格一致。
// 命中值域外的元素返回 ErrPlatformUnknown（不静默丢弃）：
// 校验路径上宁可报错，也不能把一个未知的端当成「不限端」放行。
func ValidatePlatformList(stored string, maxCount int) ([]int32, error) {
	// 这里刻意不复用 ParseIDList：ParseIDList 丢掉无法解析与非正的片段（读路径要能
	// 扛住历史脏数据），而本函数的语义是「这条规则到底发给哪些端」。
	// 复用就会把 ",0," 解析成空列表 = 不限端，正好把一端的放量推给四端。
	trimmed := strings.Trim(stored, ",")
	if trimmed == "" {
		return nil, nil
	}
	parts := strings.Split(trimmed, ",")
	if maxCount > 0 && len(parts) > maxCount {
		return nil, ErrPlatformUnknown
	}
	out := make([]int32, 0, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseInt(part, 10, 32)
		if err != nil || !ValidPlatform(int32(id)) {
			return nil, ErrPlatformUnknown
		}
		out = append(out, int32(id))
	}
	return out, nil
}

// PlatformListContains 判定存储形态的端列表是否含某端（内存侧等价于 SQL 的
// `platforms LIKE '%,2,%'`）。platform=0（调用方未声明端）时返回 false：
// 未声明不等于全命中，否则一次漏传就把配置推给了所有端。
func PlatformListContains(stored string, platform int32) bool {
	if !ValidPlatform(platform) {
		return false
	}
	return IDListContains(stored, int64(platform))
}

// PlatformLikeArg 生成与 PlatformListContains 同口径的 LIKE 参数（列表查询用）。
// 两者必须保持一致，否则「后台筛出来的端」和「运行时命中的端」会不是同一批规则。
// 参数以 ? 绑定，端标识先经 ValidPlatform 校验，无注入面。
func PlatformLikeArg(platform int32) string {
	return "%," + strconv.FormatInt(int64(platform), 10) + ",%"
}

// PlatformMatchesAny 判定「不限端（空列表）或含该端」，用于坑位/开关的可见性过滤。
// 与灰度规则的 platforms 判定区别：坑位是「挂着给谁看」，规则是「放量给谁」。
func PlatformMatchesAny(stored string, platform int32) bool {
	if stored == "" {
		return true
	}
	if platform == 0 {
		// 请求未声明端：只回「不限端」的坑位，不猜它是哪一端。
		return false
	}
	return PlatformListContains(stored, platform)
}
