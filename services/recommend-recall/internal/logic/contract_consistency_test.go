package logic

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"
)

// 本文件锁定"model 常量与生成枚举同源、实现文件与契约方法同数"这两类必然漂移点：
//  1. model 里的枚举常量编号与 rpc/*.pb.go 生成的枚举完全一致（漂移即失败）；
//  2. 降级原因的稳定 key 与 rpc.DegradeReason 一一对应，落库文本与出参枚举不会两套；
//  3. 每个 RPC 方法都有同名实现文件，且文件里不再留"未实现"哨兵（见 notImplementedSentinel）。
//
// 新增 RPC 时先改 proto -> 生成 -> 补 logic 文件，第 3 项的"方法数对齐"会挡住漏做。

func TestModelSourceMatchesRpcEnum(t *testing.T) {
	cases := []struct {
		name     string
		modelID  int32
		rpcValue int32
	}{
		{name: "SourceHot", modelID: model.SourceHot, rpcValue: int32(rpc.Source_SOURCE_HOT)},
		{name: "SourceFollow", modelID: model.SourceFollow, rpcValue: int32(rpc.Source_SOURCE_FOLLOW)},
		{name: "SourceTag", modelID: model.SourceTag, rpcValue: int32(rpc.Source_SOURCE_TAG)},
		{name: "SourceCollab", modelID: model.SourceCollab, rpcValue: int32(rpc.Source_SOURCE_COLLAB)},
		{name: "SourceVector", modelID: model.SourceVector, rpcValue: int32(rpc.Source_SOURCE_VECTOR)},
		{name: "SourceCold", modelID: model.SourceCold, rpcValue: int32(rpc.Source_SOURCE_COLD)},
		{name: "VersionStateBuilding", modelID: model.VersionStateBuilding,
			rpcValue: int32(rpc.PoolVersionState_POOL_VERSION_STATE_BUILDING)},
		{name: "VersionStateReady", modelID: model.VersionStateReady,
			rpcValue: int32(rpc.PoolVersionState_POOL_VERSION_STATE_READY)},
		{name: "VersionStateCurrent", modelID: model.VersionStateCurrent,
			rpcValue: int32(rpc.PoolVersionState_POOL_VERSION_STATE_CURRENT)},
		{name: "VersionStateRetired", modelID: model.VersionStateRetired,
			rpcValue: int32(rpc.PoolVersionState_POOL_VERSION_STATE_RETIRED)},
		{name: "VersionStateFailed", modelID: model.VersionStateFailed,
			rpcValue: int32(rpc.PoolVersionState_POOL_VERSION_STATE_FAILED)},
		{name: "PlatformAndroid", modelID: model.PlatformAndroid, rpcValue: int32(rpc.Platform_PLATFORM_ANDROID)},
		{name: "PlatformIOS", modelID: model.PlatformIOS, rpcValue: int32(rpc.Platform_PLATFORM_IOS)},
		{name: "PlatformHarmony", modelID: model.PlatformHarmony, rpcValue: int32(rpc.Platform_PLATFORM_HARMONY)},
		{name: "PlatformDesktop", modelID: model.PlatformDesktop, rpcValue: int32(rpc.Platform_PLATFORM_DESKTOP)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.modelID != tc.rpcValue {
				t.Fatalf("%s: model=%d rpc=%d，枚举编号漂移", tc.name, tc.modelID, tc.rpcValue)
			}
		})
	}
	// 枚举 0 必须留给 UNSPECIFIED：模型层的合法区间不得覆盖 0。
	if model.ValidSource(0) || model.ValidVersionState(0) || model.ValidPlatform(0) {
		t.Fatal("模型层把 0 判成合法值，无法区分\"未指定\"与\"第一个枚举\"")
	}
	// 覆盖率：每个非零 rpc 枚举值都要在模型区间内（新增枚举值时强制补模型常量）。
	for num, name := range rpc.Source_name {
		if num == 0 || name == "SOURCE_UNSPECIFIED" {
			continue
		}
		if !model.ValidSource(num) {
			t.Errorf("rpc.Source %s=%d 落在 model.ValidSource 之外", name, num)
		}
	}
	for num, name := range rpc.PoolVersionState_name {
		if num == 0 || name == "POOL_VERSION_STATE_UNSPECIFIED" {
			continue
		}
		if !model.ValidVersionState(num) {
			t.Errorf("rpc.PoolVersionState %s=%d 落在 model.ValidVersionState 之外", name, num)
		}
	}
	for name, val := range rpc.Platform_value {
		if val == 0 || name == "PLATFORM_UNSPECIFIED" {
			continue
		}
		if !model.ValidPlatform(val) {
			t.Errorf("rpc.Platform %s=%d 落在 model.ValidPlatform 之外（本服务不支持小程序）", name, val)
		}
	}
}

// TestDegradeReasonKeyMatchesRpcEnum 保证"落库/日志的稳定 key"与出参枚举同源。
// 降级矩阵（README）就是按这一组 key 表达的，任何一侧单独加值都会在这里失败。
func TestDegradeReasonKeyMatchesRpcEnum(t *testing.T) {
	pairs := []struct {
		key      string
		reason   rpc.DegradeReason
		rpcValue int32
	}{
		{key: model.DegradeReasonNone, reason: rpc.DegradeReason_DEGRADE_REASON_UNSPECIFIED, rpcValue: 0},
		{key: model.DegradeReasonPoolNotReady, reason: rpc.DegradeReason_DEGRADE_REASON_POOL_NOT_READY, rpcValue: 1},
		{key: model.DegradeReasonFeatureUnavailable, reason: rpc.DegradeReason_DEGRADE_REASON_FEATURE_UNAVAILABLE, rpcValue: 2},
		{key: model.DegradeReasonDownstreamTimeout, reason: rpc.DegradeReason_DEGRADE_REASON_DOWNSTREAM_TIMEOUT, rpcValue: 3},
		{key: model.DegradeReasonStoreUnavailable, reason: rpc.DegradeReason_DEGRADE_REASON_STORE_UNAVAILABLE, rpcValue: 4},
		{key: model.DegradeReasonBudgetExhausted, reason: rpc.DegradeReason_DEGRADE_REASON_BUDGET_EXHAUSTED, rpcValue: 5},
		{key: model.DegradeReasonColdStart, reason: rpc.DegradeReason_DEGRADE_REASON_COLD_START, rpcValue: 6},
		{key: model.DegradeReasonAllSourcesEmpty, reason: rpc.DegradeReason_DEGRADE_REASON_ALL_SOURCES_EMPTY, rpcValue: 7},
	}
	for _, p := range pairs {
		p := p
		t.Run(p.key+"/"+p.reason.String(), func(t *testing.T) {
			if int32(p.reason) != p.rpcValue {
				t.Fatalf("rpc %s=%d 与期望编号 %d 不符", p.reason, p.reason, p.rpcValue)
			}
			if !model.ValidDegradeReason(p.key) {
				t.Fatalf("降级原因 key %q 不在 model 受控集合内", p.key)
			}
		})
	}
	// 反向：rpc 里每个非零枚举都必须有对应 key，否则新降级原因无法落库。
	for num, name := range rpc.DegradeReason_name {
		if num == 0 || name == "DEGRADE_REASON_UNSPECIFIED" {
			continue
		}
		if !hasDegradeKeyFor(num) {
			t.Errorf("rpc.DegradeReason %s=%d 在 model 侧没有对应 key", name, num)
		}
	}
	if model.ValidDegradeReason("made_up_reason") {
		t.Error("任意字符串被当成合法降级原因")
	}
}

func hasDegradeKeyFor(val int32) bool {
	for _, key := range []string{
		model.DegradeReasonPoolNotReady, model.DegradeReasonFeatureUnavailable,
		model.DegradeReasonDownstreamTimeout, model.DegradeReasonStoreUnavailable,
		model.DegradeReasonBudgetExhausted, model.DegradeReasonColdStart,
		model.DegradeReasonAllSourcesEmpty,
	} {
		if reasonKeyNumber(key) == val {
			return true
		}
	}
	return false
}

// reasonKeyNumber 用 rpc 的 name->value 表把 key 文本转成编号（DEGRADE_REASON_X -> X）。
func reasonKeyNumber(key string) int32 {
	if key == model.DegradeReasonNone {
		return 0
	}
	return rpc.DegradeReason_value["DEGRADE_REASON_"+upper(key)]
}

func upper(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= 'a' && s[i] <= 'z' {
			out[i] = s[i] - 'a' + 'A'
		} else {
			out[i] = s[i]
		}
	}
	return string(out)
}

// notImplementedSentinel 是"未实现"哨兵的完整引用文本（model 包里那个 var 的限定名）。
//
// 拆成两段拼接是有原因的：仓库门禁会按这个哨兵的标识符全文扫 internal/logic，
// 数"还留着空桩的文件"，本测试若把原文写成一个连续字面量就会自己把自己判成失败。
// stubMarker 只取标识符名而不是"model."限定的整串：那样 `m "…/model"` 之类的别名写法
// 也能被抓到，扫描指纹不该依赖 import 形态。
var (
	notImplementedSentinel = "model.Err" + "NotImplemented"
	stubMarker             = "Not" + "Implemented"
)

// TestNoLogicIsStillStubbed 是"不许有假实现"的源码级门禁，取代契约轮那份
// "逐个方法调用后必须返回未实现哨兵"的用例：实现落地后那条断言本身已经成了错的断言，
// 而它在零值 ServiceContext 上调用 logic 会直接 panic（Repository 为 nil）。
//
// 这里刻意不再调用 logic：本包每个方法都要真实的 Repository（MySQL）才能走通，
// 用零值依赖去调它测不出任何业务语义，只会测出 panic 的位置。
// 反空桩改为读源码判定，仍然满足 AGENTS.md §9（不删用例、不 Skip、不写永真断言）：
//  1. internal/logic 下任何非测试 .go 文件都不得再返回"未实现"哨兵；
//  2. 契约方法与 <小写方法名>logic.go 双向对齐（新增 RPC 忘了建文件、或留下无主文件，都失败）；
//  3. 每个方法文件里必须真的写着对应签名，防止"文件在、方法没了"的空壳。
func TestNoLogicIsStillStubbed(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read logic dir: %v", err)
	}
	logicFiles := make(map[string]string)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(name)
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		body := string(src)
		if strings.Contains(body, stubMarker) {
			t.Errorf("%s 仍返回 %s：未实现的方法必须实现，不能把空桩留在生产代码里", name, notImplementedSentinel)
		}
		if strings.HasSuffix(name, "logic.go") {
			logicFiles[name] = body
		}
	}
	if len(logicFiles) == 0 {
		t.Fatal("本目录下一个 logic 文件都没读到：门禁失效（测试被永真化）")
	}

	serverType := reflect.TypeOf(rpc.UnimplementedRecallServer{})
	expected := make(map[string]string, serverType.NumMethod())
	for i := 0; i < serverType.NumMethod(); i++ {
		method := serverType.Method(i).Name
		expected[strings.ToLower(method)+"logic.go"] = method
	}
	if len(logicFiles) != len(expected) {
		t.Errorf("契约有 %d 个方法，本包有 %d 个 *logic.go 文件：新增/删除 RPC 后必须同步实现文件",
			len(expected), len(logicFiles))
	}
	for file, method := range expected {
		body, ok := logicFiles[file]
		if !ok {
			t.Errorf("RPC %s 没有对应实现文件 %s", method, file)
			continue
		}
		if !strings.Contains(body, ") "+method+"(in *rpc.") {
			t.Errorf("%s 里没有 %s 方法的实现签名，可能是个空壳文件", file, method)
		}
	}
	for file := range logicFiles {
		if _, ok := expected[file]; !ok {
			t.Errorf("%s 不对应任何契约方法（命名须为 <小写方法名>logic.go，或改名成非 logic 文件）", file)
		}
	}
}

// 说明：internal/server 与入口是 goctl 生成物（server.RecallServer 嵌入
// rpc.UnimplementedRecallServer 并逐方法转发到本包，注册时在编译期满足 rpc.RecallServer），
// 本轮不在测试里再手写一遍 facade；"契约方法数 == 实现文件数"由上面的反射断言保证。
