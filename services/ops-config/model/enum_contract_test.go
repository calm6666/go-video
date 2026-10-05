package model

// 本文件是「rpc 枚举 ↔ model 枚举镜像」的契约测试，写法是反向覆盖：
// 不去只枚举 model 侧的常量（那样即使 proto 新增一个端也永远绿），而是遍历
// protoc-gen-go 生成的 Xxx_name 映射表，要求每个非零、非 *_UNSPECIFIED 的
// 编号都能在 model 侧被判为合法。于是「proto 加了第五端 / 第七种灰度模式」
// 会立刻让这里的对应用例失败——这正是必须失败的时刻：model 里每个枚举都连着
// 一套校验分支 + 一张表的列注释，漏改一处就是线上脏数据。
//
// 另两个方向一并锁住：model 侧不得比 proto 多认任何值（否则库里的未定义值会被
// 当成合法），以及 0（UNSPECIFIED）永远不是合法业务值——把 0 当合法等于把
// 「调用方漏传」解释成「一种真实的状态」。

import (
	"sort"
	"strings"
	"testing"

	"go-video/services/ops-config/rpc"
)

// enumSpec 描述一次反向覆盖：proto 的 name 表、model 的合法性判定、
// model 侧承认的取值集合，以及人类可读标签。
type enumSpec struct {
	label  string
	names  map[int32]string
	legal  func(int32) bool
	models []int32 // model 侧认为合法的全部取值（不含 0）
}

func (s enumSpec) run(t *testing.T) {
	t.Helper()

	// 1) proto 侧每个非零非 UNSPECIFIED 编号，model 必须认。
	var rpcVals []int32
	for num, name := range s.names {
		if num == 0 || name == "" || strings.HasSuffix(name, "_UNSPECIFIED") {
			continue
		}
		rpcVals = append(rpcVals, num)
		if !s.legal(num) {
			t.Errorf("%s: proto 值 %d(%s) 在 model 侧不合法——镜像常量或值域判定漏改了", s.label, num, name)
		}
	}
	sort.Slice(rpcVals, func(i, j int) bool { return rpcVals[i] < rpcVals[j] })

	// 2) 数量必须一致：这一条就是「新增枚举值必须让测试失败」的来源。
	want := append([]int32(nil), s.models...)
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if len(rpcVals) != len(want) {
		t.Fatalf("%s: proto 认 %v（%d 个），model 认 %v（%d 个）——两侧枚举必须同步增删并同步本测试",
			s.label, rpcVals, len(rpcVals), want, len(want))
	}
	for i := range want {
		if rpcVals[i] != want[i] {
			t.Errorf("%s: 第 %d 个取值不一致 proto=%d model=%d", s.label, i, rpcVals[i], want[i])
		}
	}

	// 3) model 侧不得比 proto 多认任何值（扫到 max+8，覆盖常见的越界脏值）。
	var max int32
	for num := range s.names {
		if num > max {
			max = num
		}
	}
	for v := int32(1); v <= max+8; v++ {
		_, defined := s.names[v]
		if s.legal(v) && !defined {
			t.Errorf("%s: model 承认 proto 里没有的值 %d（会把库里的未定义值当合法放行）", s.label, v)
		}
	}

	// 4) 0 与负数、越界值永远不合法。
	if s.legal(0) {
		t.Errorf("%s: UNSPECIFIED(0) 不得是合法业务值", s.label)
	}
	for _, v := range []int32{-1, max + 1, 1000} {
		if s.legal(v) {
			t.Errorf("%s: %d 必须不合法", s.label, v)
		}
	}
}

func TestRPCEnumsMirrorModelLegality(t *testing.T) {
	specs := []enumSpec{
		{
			label:  "ClientPlatform",
			names:  rpc.ClientPlatform_name,
			legal:  ValidPlatform,
			models: []int32{PlatformAndroid, PlatformIOS, PlatformHarmony, PlatformDesktop},
		},
		{
			label: "RolloutMode",
			names: rpc.RolloutMode_name,
			legal: ValidRolloutMode,
			models: []int32{ModeFull, ModePercentage, ModeAppVersion,
				ModePlatform, ModeMidSuffix, ModeWhitelist},
		},
		{
			label:  "ConfigValueType",
			names:  rpc.ConfigValueType_name,
			legal:  ValidValueType,
			models: []int32{ValueTypeString, ValueTypeInt, ValueTypeBool, ValueTypeJSON},
		},
		{
			label:  "ItemState",
			names:  rpc.ItemState_name,
			legal:  ValidState,
			models: []int32{StateOn, StateOff},
		},
	}
	for _, s := range specs {
		s := s
		t.Run(s.label, func(t *testing.T) { s.run(t) })
	}
}

// rpcNameToModelName 是各枚举在 model 侧的「小写名」表：SQL 列注释、日志与
// ModeName 都用这套名字，proto 改名必须同步这里。
var rpcNameToModelName = map[string]func(int32) string{
	"ClientPlatform":  platformName,
	"RolloutMode":     ModeName,
	"ConfigValueType": valueTypeName,
	"ItemState":       stateName,
}

func platformName(v int32) string {
	switch v {
	case PlatformAndroid:
		return "android"
	case PlatformIOS:
		return "ios"
	case PlatformHarmony:
		return "harmony"
	case PlatformDesktop:
		return "desktop"
	}
	return ""
}

func valueTypeName(v int32) string {
	switch v {
	case ValueTypeString:
		return "string"
	case ValueTypeInt:
		return "int"
	case ValueTypeBool:
		return "bool"
	case ValueTypeJSON:
		return "json"
	}
	return ""
}

func stateName(v int32) string {
	switch v {
	case StateOn:
		return "on"
	case StateOff:
		return "off"
	}
	return ""
}

func TestEnumNumbersAndNamesMatchBetweenRPCAndModel(t *testing.T) {
	// 数值逐一对齐：两侧都按 int32 落库，编号错位=语义错位
	// （把「iOS」写成「harmony」的灰度规则既看不出来也改不回来）。
	cases := []struct {
		label  string
		names  map[int32]string
		prefix string
		legal  func(int32) bool
	}{
		{"ClientPlatform", rpc.ClientPlatform_name, "CLIENT_PLATFORM_", ValidPlatform},
		{"RolloutMode", rpc.RolloutMode_name, "ROLLOUT_MODE_", ValidRolloutMode},
		{"ConfigValueType", rpc.ConfigValueType_name, "CONFIG_VALUE_TYPE_", ValidValueType},
		{"ItemState", rpc.ItemState_name, "ITEM_STATE_", ValidState},
	}
	nameOf := rpcNameToModelName
	for _, c := range cases {
		fn, ok := nameOf[c.label]
		if !ok {
			t.Fatalf("%s 没有 model 侧名字表，新增枚举必须同时补这里", c.label)
		}
		for num, name := range c.names {
			if !strings.HasPrefix(name, c.prefix) {
				t.Errorf("%s: proto 枚举名 %q 不符合生成约定（前缀 %q）", c.label, name, c.prefix)
				continue
			}
			suffix := strings.TrimPrefix(name, c.prefix)
			if suffix == "UNSPECIFIED" {
				if num != 0 {
					t.Errorf("%s: %q 必须占 0，实得 %d", c.label, name, num)
				}
				if got := fn(num); got != "" && got != "unspecified" {
					t.Errorf("%s: 0 号值不该有业务名字，实得 %q", c.label, got)
				}
				continue
			}
			if !c.legal(num) {
				t.Errorf("%s: %q=%d 未被 model 侧承认", c.label, name, num)
				continue
			}
			if got := fn(num); got != strings.ToLower(suffix) {
				t.Errorf("%s: model 名字 %q 与 proto %q 对不上（日志、列注释与端上语义会互相指错）",
					c.label, got, name)
			}
		}
	}
}

func TestNoMiniProgramPlatformAnywhere(t *testing.T) {
	// AGENTS.md §1、§6：项目不做小程序。锁住三个可能的漏出口：
	// proto 枚举、model 端常量/scope 值域、以及任何「第五端」的字符串形态。
	for num, name := range rpc.ClientPlatform_name {
		up := strings.ToUpper(name)
		for _, bad := range []string{"MINI", "WECHAT", "WEIXIN", "MP_", "PROGRAM", "H5", "KUAISHOU", "DOUYIN", "WEB"} {
			if strings.Contains(up, bad) {
				t.Errorf("proto ClientPlatform 出现禁止端 %d=%s（项目不含小程序/H5/第三方分发端）", num, name)
			}
		}
	}
	if len(rpc.ClientPlatform_name) != 5 {
		t.Errorf("ClientPlatform 应为 UNSPECIFIED+四端共 5 项，实得 %d 项: %v", len(rpc.ClientPlatform_name), rpc.ClientPlatform_name)
	}
	if len(rpc.ClientPlatform_value) != 5 {
		t.Errorf("ClientPlatform_value 与 _name 项数不一致: %v", rpc.ClientPlatform_value)
	}
	for v := int32(5); v <= 10; v++ {
		if ValidPlatform(v) {
			t.Errorf("端 %d 必须不合法：四端之外一律拒绝", v)
		}
	}
	// scope 与端同名口径，同样不得出现小程序形态。
	for _, s := range []string{"mini", "miniprogram", "weixin", "wechat", "wx", "h5", "mp", "Mini"} {
		if ValidScope(s) {
			t.Errorf("scope %q 必须不合法（不支持小程序/H5）", s)
		}
	}
	// 四端 scope 必须都在，且与端常量一一对应。
	for _, c := range []struct {
		p int32
		s string
	}{{PlatformAndroid, ScopeAndroid}, {PlatformIOS, ScopeIOS},
		{PlatformHarmony, ScopeHarmony}, {PlatformDesktop, ScopeDesktop}} {
		if ScopeOfPlatform(c.p) != c.s || !ValidScope(c.s) {
			t.Errorf("端 %d 与 scope %q 的映射断了", c.p, c.s)
		}
	}
	if ScopeOfPlatform(0) != "" || ScopeOfPlatform(9) != "" {
		t.Error("未知端的 scope 映射必须回空串，由调用方报 ErrPlatformUnknown")
	}
	if ValidScope("") || ValidScope("  ") || ValidScope("GLOBAL") {
		t.Error("空串/空白/大写都不是合法 scope：空串由调用方归一为 global 后再判定")
	}
}

func TestItemTypeDomainIsClosedAndTopicOnlyInSlot(t *testing.T) {
	// 值域刻意收窄：新增引用类型意味着要引用一个新域的主键形态，必须走评审。
	for _, it := range []string{ItemTypeUGCVideo, ItemTypePGCSeason, ItemTypePGCEpisode} {
		if !ValidItemType(it, false) || !ValidItemType(it, true) {
			t.Errorf("%s 必须在专题与坑位两处都合法", it)
		}
	}
	if ValidItemType(ItemTypeTopic, false) || !ValidItemType(ItemTypeTopic, true) {
		t.Error("topic 只允许出现在坑位里（专题挂专题会形成自引用环）")
	}
	// 任何未在 model 里定义的类型字符串都必须被拒（含商业化与未评审的引用形态）。
	for _, bad := range []string{"", " ", "ad", "ads", "commerce", "vip", "coin", "order",
		"live_room", "article", "Topic", "UGC_VIDEO", "course", "creator"} {
		if ValidItemType(bad, true) {
			t.Errorf("item_type %q 必须被拒", bad)
		}
	}
}

func TestStringEnumConstantsAreExactAndDistinct(t *testing.T) {
	// change_type / refresh target 是字符串枚举：库里存的是字面量，SQL 列注释与
	// 逻辑分支也都按字面量匹配，改名等于换数据域。值域「判定」行为分别由
	// TestInsertVersionRejectsUnexplainableRowsBeforeSQL（model）与
	// logic 侧的 RefreshCache 用例断言，这里锁住的是常量本身。
	groups := [][]string{
		{ChangeTypeCreate, ChangeTypePublish, ChangeTypeRollback},
		{RefreshTargetConfig, RefreshTargetTopic, RefreshTargetSlot, RefreshTargetAll},
		{ScopeGlobal, ScopeAndroid, ScopeIOS, ScopeHarmony, ScopeDesktop},
		{ItemTypeUGCVideo, ItemTypePGCSeason, ItemTypePGCEpisode, ItemTypeTopic},
	}
	want := [][]string{
		{"create", "publish", "rollback"},
		{"config", "topic", "slot", "all"},
		{"global", "android", "ios", "harmony", "desktop"},
		{"ugc_video", "pgc_season", "pgc_episode", "topic"},
	}
	for gi, g := range groups {
		if len(g) != len(want[gi]) {
			t.Fatalf("第 %d 组字面量数量变了: %v", gi, g)
		}
		seen := make(map[string]struct{}, len(g))
		for i, v := range g {
			if v != want[gi][i] {
				t.Errorf("第 %d 组第 %d 个字面量应是 %q，实为 %q（库里历史行会读不出来）", gi, i, want[gi][i], v)
			}
			if v != strings.TrimSpace(strings.ToLower(v)) {
				t.Errorf("%q 必须是小写无空白形态", v)
			}
			if _, dup := seen[v]; dup {
				t.Errorf("%q 在同一组里重复，SQL 注释与分支会指错", v)
			}
			seen[v] = struct{}{}
		}
	}
}
