// 本文件覆盖「运行时读取」这一组接口（ResolveConfig / BatchResolveConfig / RefreshCache /
// ListConfigs），它们共享 conv_resolve.go 的 resolveOne 内核。
//
// 读取路径真正的风险不是「取不到值」，而是三类静默分歧：
//  1. 灰度结论与写入口径不一致（同一 mid 在批量里和单键里命中不同版本）；
//  2. 投影（Redis）与事实源（MySQL）不一致时被当成故障，或被当成事实；
//  3. 缓存键删错范围：少删 = 改了但端上还是旧的，多删 = 越界到别人的键空间。
// 因此每条断言都落在探针上：读了哪个键、回填的 TTL 是多少、删了哪些键、回了几次源。
//
// 契约出处（断言的依据都写在这，不靠实现反推）：
//   - rpc/opsconfig.proto：ResolveConfigReply.found「false 表示键不存在或已停用（不返回
//     gRPC NotFound）」、ConfigView.ttl/epoch 语义、TargetContext.ignore_rollout、
//     ROLLOUT_MODE_PERCENTAGE 的分桶公式、BatchResolveConfigReq「单次最多 50 个键」；
//   - helpers.go：itemPointerTTLSeconds 与 configKeysOf 的「规则不缓存」注释；
//   - internal/svc 的 CacheKV 注释：缓存是可整域重建的只读投影，不是事实源。

package logic

import (
	"errors"
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"
	"testing"

	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"
)

const (
	modeFull       = rpc.RolloutMode(model.ModeFull)
	modeAppVersion = rpc.RolloutMode(model.ModeAppVersion)
	testKeyPrefix  = "govideo:opsconfig:test"
)

// (e *testEnv) resolveCached 走「带 request_id」的解析，即投影路径；
// 脚手架里的 e.resolve 刻意不带 request_id，走的是纯回源路径。
func (e *testEnv) resolveCached(t *testing.T, requestID, cfgKey string, target *rpc.TargetContext) *rpc.ConfigView {
	t.Helper()
	reply, err := NewResolveConfigLogic(bg(), e.svc).ResolveConfig(&rpc.ResolveConfigReq{
		Ctx: &rpc.CallContext{RequestId: requestID}, CfgKey: cfgKey, Target: target,
	})
	wantOK(t, reply, err, "ResolveConfig(带投影) "+cfgKey)
	if !reply.GetFound() {
		t.Fatalf("ResolveConfig(%s)：期望命中，实得 found=false", cfgKey)
	}
	return reply.GetConfig()
}

// requireSameView 逐字段比对两份视图。不用反射或 String() 快照：
// 批量与单键的分歧恰恰可能只在某一个字段上，报错要能直接指出是哪个。
func requireSameView(t *testing.T, got, want *rpc.ConfigView, label string) {
	t.Helper()
	if got == nil || want == nil {
		t.Fatalf("%s：视图为空 got=%v want=%v", label, got, want)
	}
	for _, c := range []struct {
		field     string
		got, want any
	}{
		{"config_id", got.GetConfigId(), want.GetConfigId()},
		{"cfg_key", got.GetCfgKey(), want.GetCfgKey()},
		{"scope", got.GetScope(), want.GetScope()},
		{"value", got.GetValue(), want.GetValue()},
		{"value_type", got.GetValueType(), want.GetValueType()},
		{"version", got.GetVersion(), want.GetVersion()},
		{"rollout_rule_id", got.GetRolloutRuleId(), want.GetRolloutRuleId()},
		{"rollout_mode", got.GetRolloutMode(), want.GetRolloutMode()},
		{"ttl", got.GetTtl(), want.GetTtl()},
		{"epoch", got.GetEpoch(), want.GetEpoch()},
		{"published_by", got.GetPublishedBy(), want.GetPublishedBy()},
		{"published_at", got.GetPublishedAt(), want.GetPublishedAt()},
	} {
		if c.got != c.want {
			t.Fatalf("%s：%s 不一致\n got=%v\nwant=%v", label, c.field, c.got, c.want)
		}
	}
}

// bucketByProto 按 rpc/opsconfig.proto 里写死的公式重算分桶
// （bucket = crc32(cfg_key + ":" + mid) % 100 < percentage）。
// 刻意不叫 model.PercentageBucket：否则「读路径符合契约」这句话会退化成
// 「读路径调用了它自己的实现」，实现整体漂移时测不出来。
func bucketByProto(cfgKey string, mid int64) int {
	if mid <= 0 {
		return -1
	}
	return int(crc32.ChecksumIEEE([]byte(cfgKey+":"+strconv.FormatInt(mid, 10))) % model.BucketCount)
}

func TestResolveConfigProjectsTheLiveVersionView(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.title", "v1", 0, "req-1")
	e.publish(t, "home.title", "v2", 1, "req-2")

	item, ver := e.itemByName("home.title"), e.versionRow(e.itemByName("home.title").ConfigID, 2)
	delsBefore := len(e.cache.dels)
	view := e.mustResolve(t, "home.title", nil)

	if view.GetConfigId() != item.ConfigID || view.GetCfgKey() != "home.title" ||
		view.GetScope() != model.ScopeGlobal {
		t.Fatalf("身份三列不对：%+v", view)
	}
	if view.GetVersion() != 2 || view.GetValue() != "v2" || view.GetValueType() != vtString {
		t.Fatalf("指针推进后仍读到旧快照：%+v", view)
	}
	if view.GetRolloutRuleId() != 0 || view.GetRolloutMode() != "" {
		t.Fatalf("没有灰度时不该带规则号/方式：%+v", view)
	}
	// ttl 是给调用方的建议缓存秒数，epoch 是缓存代次（proto ConfigView 注释）。
	if view.GetTtl() != 60 {
		t.Fatalf("建议 TTL 应取 Resolve.DefaultTTLSeconds：got=%d", view.GetTtl())
	}
	if view.GetEpoch() != item.Epoch {
		t.Fatalf("epoch 必须是库里那一份：got=%d row=%d", view.GetEpoch(), item.Epoch)
	}
	// published_by/published_at 取自**命中那一版**的快照，而不是配置项的 mtime。
	if view.GetPublishedBy() != ver.OperatorName || view.GetPublishedAt() != ver.PublishedAt {
		t.Fatalf("发布归因取自历史快照而非当前项：got=%q/%d row=%q/%d",
			view.GetPublishedBy(), view.GetPublishedAt(), ver.OperatorName, ver.PublishedAt)
	}
	// 无 request_id 的调用不进投影（resolveconfiglogic：否则探测流量会打穿热键）。
	if n := len(e.cache.gets) + len(e.cache.sets); n != 0 {
		t.Fatalf("回源路径读了 %d 次投影：与「缺 request_id 不缓存」的口径冲突", n)
	}
	if n := len(e.cache.dels) - delsBefore; n != 0 {
		t.Fatalf("只读接口删了 %d 个投影键", n)
	}
}

func TestResolveConfigReportsMissesAsResultsNotErrors(t *testing.T) {
	e := newTestEnv(t)
	offItem, _ := e.seedPublished(t, "home.off", "v1", "seed-off")
	e.forceItemState(offItem.ConfigID, model.StateOff)
	// 有主记录、一版快照，但指针从没推进过（latest_version=0）。
	unpub := e.seedItem(t, "home.unpub", model.ScopeGlobal, model.ValueTypeString, model.StateOn)
	e.seedVersion(t, unpub.ConfigID, 1, "v1", model.ValueTypeString, model.ChangeTypeCreate, "seed-unpub", "种子")

	for _, cfgKey := range []string{"home.nope", "home.off", "home.unpub"} {
		reply, err := e.resolve(t, cfgKey, rolloutCtx(7, platAndroid, "7.2.0"))
		wantOK(t, reply, err, cfgKey+" 应作为正常结果返回")
		if reply.GetFound() {
			t.Fatalf("%s：不该命中", cfgKey)
		}
		if reply.GetConfig() != nil {
			t.Fatalf("%s：found=false 时不能带回视图：%+v", cfgKey, reply.GetConfig())
		}
	}
	// 三种「没配」都不许留痕迹：既没写投影也没删投影。
	if n := len(e.cache.sets) + len(e.cache.dels); n != 0 {
		t.Fatalf("未命中产生了 %d 次投影写入/删除", n)
	}
}

func TestResolveConfigCarriesEveryValueTypeAsText(t *testing.T) {
	e := newTestEnv(t)
	cases := []struct {
		cfgKey, value string
		vt            rpc.ConfigValueType
	}{
		{"app.limit.int", "42", vtInt},
		{"app.flag.bool", "false", vtBool},
		{"app.tpl.json", `{"tabs":["a","b"]}`, vtJSON},
		{"app.copy.str", "首页标题", vtString},
	}
	for i, tc := range cases {
		_, perr := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
			Ctx: callCtx("req-type-" + strconv.Itoa(i)), CfgKey: tc.cfgKey,
			ValueType: tc.vt, Value: tc.value, Reason: "按声明类型发布",
		})
		if perr != nil {
			t.Fatalf("发布 %s：%v", tc.cfgKey, perr)
		}
		view := e.mustResolve(t, tc.cfgKey, nil)
		// proto：string value = 5「命中版本的值（文本承载）」——
		// 服务端不做任何解析，解析器归四端各自所有。
		if view.GetValueType() != tc.vt {
			t.Fatalf("%s：值类型未随快照返回：got=%v want=%v", tc.cfgKey, view.GetValueType(), tc.vt)
		}
		if view.GetValue() != tc.value {
			t.Fatalf("%s：值被服务端改写过：%q != %q", tc.cfgKey, view.GetValue(), tc.value)
		}
	}
}

// TestResolveConfigMatchesEveryRolloutMode 钉住「mode 声明主维度、非空维度取交集」这条
// 写读共用口径（model/rollout.go 的 ValidateRuleShape 注释）。
func TestResolveConfigMatchesEveryRolloutMode(t *testing.T) {
	cases := []struct {
		name    string
		spec    *rpc.RolloutRuleSpec
		target  *rpc.TargetContext
		wantHit bool
		wantMod string
	}{
		{
			name:    "全量规则连未登录请求也命中",
			spec:    &rpc.RolloutRuleSpec{Name: "full", Mode: modeFull, Priority: 10, Remark: "全量"},
			target:  nil,
			wantHit: true, wantMod: "full",
		},
		{
			name:    "白名单命中",
			spec:    &rpc.RolloutRuleSpec{Name: "wl", Mode: modeWhitelist, WhitelistMids: []int64{7, 8}, Priority: 10, Remark: "白名单"},
			target:  rolloutCtx(8, platAndroid, "7.2.0"),
			wantHit: true, wantMod: "whitelist",
		},
		{
			name:    "白名单不命中就不算命中",
			spec:    &rpc.RolloutRuleSpec{Name: "wl", Mode: modeWhitelist, WhitelistMids: []int64{7, 8}, Priority: 10, Remark: "白名单"},
			target:  rolloutCtx(9, platAndroid, "7.2.0"),
			wantHit: false,
		},
		{
			name:    "尾号命中",
			spec:    &rpc.RolloutRuleSpec{Name: "suffix", Mode: modeMidSuffix, MidSuffixes: "0,3", Priority: 10, Remark: "尾号"},
			target:  rolloutCtx(12340, platAndroid, "7.2.0"),
			wantHit: true, wantMod: "mid_suffix",
		},
		{
			name:    "尾号不命中",
			spec:    &rpc.RolloutRuleSpec{Name: "suffix", Mode: modeMidSuffix, MidSuffixes: "0,3", Priority: 10, Remark: "尾号"},
			target:  rolloutCtx(12345, platAndroid, "7.2.0"),
			wantHit: false,
		},
		{
			name:    "按端命中",
			spec:    &rpc.RolloutRuleSpec{Name: "plat", Mode: modePlatform, Platforms: []rpc.ClientPlatform{platAndroid}, Priority: 10, Remark: "按端"},
			target:  rolloutCtx(7, platAndroid, "7.2.0"),
			wantHit: true, wantMod: "platform",
		},
		{
			name:    "端未知时按端规则不命中（未知端不等于不限端）",
			spec:    &rpc.RolloutRuleSpec{Name: "plat", Mode: modePlatform, Platforms: []rpc.ClientPlatform{platAndroid}, Priority: 10, Remark: "按端"},
			target:  rolloutCtx(7, rpc.ClientPlatform_CLIENT_PLATFORM_UNSPECIFIED, "7.2.0"),
			wantHit: false,
		},
		{
			name:    "版本区间内命中",
			spec:    &rpc.RolloutRuleSpec{Name: "ver", Mode: modeAppVersion, AppVersionMin: "7.0.0", AppVersionMax: "7.5.0", Priority: 10, Remark: "版本"},
			target:  rolloutCtx(7, platAndroid, "7.2.10"),
			wantHit: true, wantMod: "app_version",
		},
		{
			name:    "版本按段比较：7.10.0 不算 7.5.0 之后",
			spec:    &rpc.RolloutRuleSpec{Name: "ver", Mode: modeAppVersion, AppVersionMin: "7.0.0", AppVersionMax: "7.5.0", Priority: 10, Remark: "版本"},
			target:  rolloutCtx(7, platAndroid, "7.10.0"),
			wantHit: false,
		},
		{
			name: "版本区间规则的附加端条件仍生效",
			spec: &rpc.RolloutRuleSpec{Name: "ver", Mode: modeAppVersion, AppVersionMin: "7.0.0",
				Platforms: []rpc.ClientPlatform{platAndroid}, Priority: 10, Remark: "版本+端"},
			target:  rolloutCtx(7, rpc.ClientPlatform(model.PlatformIOS), "7.2.0"),
			wantHit: false,
		},
		{
			name:    "未登录不参与百分比分桶",
			spec:    &rpc.RolloutRuleSpec{Name: "pct", Mode: modePercentage, Percentage: 100, Priority: 10, Remark: "百分比"},
			target:  rolloutCtx(0, platAndroid, "7.2.0"),
			wantHit: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.publish(t, "home.title", "v1", 0, "req-1")
			gray := e.publishWithRules(t, "home.title", "v2", 1, "req-2", []*rpc.RolloutRuleSpec{tc.spec})
			// 带规则的发布不推进指针，且规则默认停用：开闸才是放量（proto「写规则/开闸两步」）。
			if got := e.itemByName("home.title").LatestVersion; got != 1 {
				t.Fatalf("前置被破坏：灰度发布把指针推进到了 %d", got)
			}
			ruleID := gray.GetRules()[0].GetRuleId()
			e.openRule(t, ruleID, "req-open")

			view := e.mustResolve(t, "home.title", tc.target)
			if !tc.wantHit {
				if view.GetVersion() != 1 || view.GetRolloutRuleId() != 0 || view.GetRolloutMode() != "" {
					t.Fatalf("规则不该命中，却回了灰度版本：%+v", view)
				}
				return
			}
			if view.GetVersion() != 2 {
				t.Fatalf("命中灰度却读到正式版本：%+v", view)
			}
			if view.GetRolloutRuleId() != ruleID {
				t.Fatalf("回的不是被命中那条规则的 ID：%d != %d", view.GetRolloutRuleId(), ruleID)
			}
			if view.GetRolloutMode() != tc.wantMod {
				t.Fatalf("命中方式标识不符：got=%q want=%q", view.GetRolloutMode(), tc.wantMod)
			}
			// 灰度只改「谁读到哪一版」，不改版本指针本身。
			if view.GetEpoch() != e.itemByName("home.title").Epoch {
				t.Fatal("灰度命中不该改动 epoch")
			}
		})
	}
}

// TestResolveConfigPercentageFollowsTheContractFormula 用 proto 里写死的公式独立重算，
// 并把「同一 mid 永远同一结论」当成放量语义的正确性定义。
func TestResolveConfigPercentageBucketMatchesTheProtoFormula(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.title", "v1", 0, "req-1")
	gray := e.publishWithRules(t, "home.title", "v2", 1, "req-2", []*rpc.RolloutRuleSpec{
		{Name: "pct30", Mode: modePercentage, Percentage: 30, Priority: 10, Remark: "放量 30%"},
	})
	e.openRule(t, gray.GetRules()[0].GetRuleId(), "req-open")
	ruleID := gray.GetRules()[0].GetRuleId()

	for _, mid := range []int64{1, 2, 7, 42, 99, 100, 123456789} {
		wantGray := bucketByProto("home.title", mid) < 30
		first := e.mustResolve(t, "home.title", rolloutCtx(mid, platAndroid, "7.2.0"))
		second := e.mustResolve(t, "home.title", rolloutCtx(mid, platAndroid, "7.2.0"))
		requireSameView(t, first, second, fmt.Sprintf("mid=%d 两次解析", mid))
		if got := first.GetVersion() == 2; got != wantGray {
			t.Fatalf("mid=%d bucket=%d：命中结论与契约公式不符（version=%d rule=%d）",
				mid, bucketByProto("home.title", mid), first.GetVersion(), first.GetRolloutRuleId())
		}
		if wantGray != (first.GetRolloutRuleId() == ruleID) {
			t.Fatalf("mid=%d：rollout_rule_id 与版本结论互相矛盾：%+v", mid, first)
		}
	}

	// 分桶必须掺入 cfg_key：否则所有配置灰度的都是同一批人（model 注释里的理由）。
	var sawDifferent bool
	for _, mid := range []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10} {
		if bucketByProto("home.title", mid) != bucketByProto("detail.title", mid) {
			sawDifferent = true
			break
		}
	}
	if !sawDifferent {
		t.Fatal("分桶未随 cfg_key 变化，所有灰度会命中同一批人")
	}
}

func TestResolveConfigTakesFirstHitByPriorityThenRuleID(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.title", "v1", 0, "req-1")
	item := e.itemByName("home.title")
	e.seedVersion(t, item.ConfigID, 2, "v2", model.ValueTypeString, model.ChangeTypePublish, "s2", "种子")
	e.seedVersion(t, item.ConfigID, 3, "v3", model.ValueTypeString, model.ChangeTypePublish, "s3", "种子")
	e.seedVersion(t, item.ConfigID, 4, "v4", model.ValueTypeString, model.ChangeTypePublish, "s4", "种子")

	ruleP10A := e.seedRule(t, &model.RolloutRule{ConfigID: item.ConfigID, Version: 2, Name: "a-p10",
		Mode: model.ModeWhitelist, WhitelistMids: model.IDListString([]int64{7}), Priority: 10, State: model.StateOn})
	ruleP10B := e.seedRule(t, &model.RolloutRule{ConfigID: item.ConfigID, Version: 4, Name: "b-p10",
		Mode: model.ModeWhitelist, WhitelistMids: model.IDListString([]int64{7}), Priority: 10, State: model.StateOn})
	ruleP20 := e.seedRule(t, &model.RolloutRule{ConfigID: item.ConfigID, Version: 3, Name: "p20",
		Mode: model.ModeWhitelist, WhitelistMids: model.IDListString([]int64{7, 8}), Priority: 20, State: model.StateOn})
	if ruleP10A.RuleID >= ruleP10B.RuleID || ruleP20.Priority <= ruleP10B.Priority {
		t.Fatalf("前置不成立：需要 a-p10 的 rule_id 最小且 p20 名次更靠后，实得 %d/%d",
			ruleP10A.RuleID, ruleP10B.RuleID)
	}

	// 三条规则都命中 mid=7：首个命中 = priority 最小、同优先级 rule_id 最小的 a-p10。
	// 名次与写入次序都由 SQL 的 ORDER BY priority, rule_id 决定，这里把它钉住。
	target := rolloutCtx(7, platAndroid, "7.2.0")
	view := e.mustResolve(t, "home.title", target)
	if view.GetVersion() != 2 || view.GetRolloutRuleId() != ruleP10A.RuleID {
		t.Fatalf("首个命中应为 priority 最小、同优先级 rule_id 最小的那条：got=%d/%d want=%d/%d",
			view.GetVersion(), view.GetRolloutRuleId(), 2, ruleP10A.RuleID)
	}

	// 关掉它：同优先级的 b-p10 接手（而不是名次更靠后的 p20，也不是「回到正式版本」）。
	e.closeRule(t, ruleP10A.RuleID, "req-close-a")
	view = e.mustResolve(t, "home.title", target)
	if view.GetVersion() != 4 || view.GetRolloutRuleId() != ruleP10B.RuleID {
		t.Fatalf("同优先级次条应接手：%+v", view)
	}
	// 名次更靠后的规则接手前必须先把靠前全部关完：这条决定「止血」的可预期性。
	e.closeRule(t, ruleP10B.RuleID, "req-close-b")
	view = e.mustResolve(t, "home.title", target)
	if view.GetVersion() != 3 || view.GetRolloutRuleId() != ruleP20.RuleID {
		t.Fatalf("关完靠前规则后应由 priority 20 接手：%+v", view)
	}

	// 全部规则都不命中（mid=99 不在任何白名单）⇒ 正式版本，rule 字段清零。
	e.closeRule(t, ruleP20.RuleID, "req-close-c")
	view = e.mustResolve(t, "home.title", rolloutCtx(99, platAndroid, "7.2.0"))
	if view.GetVersion() != 1 || view.GetRolloutRuleId() != 0 || view.GetRolloutMode() != "" {
		t.Fatalf("无命中必须回落正式版本：%+v", view)
	}
	// 停用不是删除：规则行仍在库里，仍是放量决策的证据（proto「不物理删除」）。
	if len(e.ruleIDsFor(item.ConfigID, 2, 0)) == 0 {
		t.Fatal("关闸把规则行删了")
	}
}

func TestResolveConfigIgnoresRulesOutsideTheirTimeWindow(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.title", "v1", 0, "req-1")
	item := e.itemByName("home.title")
	e.seedVersion(t, item.ConfigID, 2, "v2", model.ValueTypeString, model.ChangeTypePublish, "s2", "种子")
	e.seedVersion(t, item.ConfigID, 3, "v3", model.ValueTypeString, model.ChangeTypePublish, "s3", "种子")

	now := model.NowUnix()
	wl := model.IDListString([]int64{7})
	// 三条规则都 state=ON 且都命中 mid=7，只有时间窗不同：
	// 于是「读没读到灰度」只可能由窗口决定。
	e.seedRule(t, &model.RolloutRule{ConfigID: item.ConfigID, Version: 2, Name: "future", Mode: model.ModeWhitelist,
		WhitelistMids: wl, Priority: 10, State: model.StateOn, StartAt: now + 3600})
	e.seedRule(t, &model.RolloutRule{ConfigID: item.ConfigID, Version: 3, Name: "expired", Mode: model.ModeWhitelist,
		WhitelistMids: wl, Priority: 20, State: model.StateOn, StartAt: now - 7200, EndAt: now - 60})

	view := e.mustResolve(t, "home.title", rolloutCtx(7, platAndroid, "7.2.0"))
	if view.GetVersion() != 1 || view.GetRolloutRuleId() != 0 {
		t.Fatalf("未到/已过窗口的规则被启用了（定时放量会变成提前放量）：%+v", view)
	}
	if got := e.call("RolloutRule.ListCandidates"); got == 0 {
		t.Fatal("前置不成立：根本没查过候选规则")
	}

	// end_at 是右开：ts == end_at 时刻已不生效（model.InTimeWindow）。
	rightOpen := e.seedRule(t, &model.RolloutRule{ConfigID: item.ConfigID, Version: 2, Name: "open", Mode: model.ModeWhitelist,
		WhitelistMids: wl, Priority: 30, State: model.StateOn, StartAt: now - 60, EndAt: now + 3600})
	view = e.mustResolve(t, "home.title", rolloutCtx(7, platAndroid, "7.2.0"))
	if view.GetVersion() != 2 || view.GetRolloutRuleId() != rightOpen.RuleID {
		t.Fatalf("窗口内规则应生效：%+v", view)
	}
}

func TestResolveConfigIgnoreRolloutPreviewsTheLiveVersion(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.title", "v1", 0, "req-1")
	gray := e.publishWithRules(t, "home.title", "v2", 1, "req-2", []*rpc.RolloutRuleSpec{
		{Name: "full-rollout", Mode: modeFull, Priority: 10, Remark: "全量放量中"},
	})
	e.openRule(t, gray.GetRules()[0].GetRuleId(), "req-open")

	hit := e.mustResolve(t, "home.title", rolloutCtx(7, platAndroid, "7.2.0"))
	if hit.GetVersion() != 2 {
		t.Fatalf("前置不成立：全量规则没命中：%+v", hit)
	}
	preview := e.mustResolve(t, "home.title", &rpc.TargetContext{
		Platform: platAndroid, AppVersion: "7.2.0", Mid: 7, IgnoreRollout: true,
	})
	// proto：ignore_rollout「true 表示强制取正式版本（后台预览与排障用）」。
	if preview.GetVersion() != 1 || preview.GetRolloutRuleId() != 0 || preview.GetRolloutMode() != "" {
		t.Fatalf("ignore_rollout 没预览到正式版本：%+v", preview)
	}
	if preview.GetValue() != "v1" {
		t.Fatalf("预览值不是正式版本的值：%q", preview.GetValue())
	}
	// 预览是只读动作：不改指针、不改规则、不动投影。
	if got := e.itemByName("home.title").LatestVersion; got != 1 {
		t.Fatalf("预览推进了指针：%d", got)
	}
}

// TestResolveConfigFallsBackWhenGrayRulePointsAtAnUnpublishedVersion 覆盖 fail-closed 分支：
// conv_resolve.go 第 4 步「宁可不放量，也不能把一个不存在的值推给线上」，
// 并且必须让那条规则在 Error 日志里现形（否则它会一直静默不生效）。
func TestResolveConfigFallsBackWhenGrayRulePointsAtAnUnpublishedVersion(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.title", "v1", 0, "req-1")
	item := e.itemByName("home.title")
	logs := captureLogs(t)

	ghost := e.seedRule(t, &model.RolloutRule{ConfigID: item.ConfigID, Version: 9, Name: "ghost",
		Mode: model.ModeWhitelist, WhitelistMids: model.IDListString([]int64{7}), Priority: 10, State: model.StateOn})

	view := e.mustResolve(t, "home.title", rolloutCtx(7, platAndroid, "7.2.0"))
	if view.GetVersion() != 1 {
		t.Fatalf("幽灵版本被推给了线上：%+v", view)
	}
	if view.GetRolloutRuleId() != 0 || view.GetRolloutMode() != "" {
		t.Fatalf("回落时不该声称命中了规则：%+v", view)
	}
	joined := logs.joined()
	if !strings.Contains(joined, "已回落正式版本") ||
		!strings.Contains(joined, strconv.FormatInt(ghost.RuleID, 10)) ||
		!strings.Contains(joined, "version=9") {
		t.Fatalf("回落必须在 Error 日志里点出规则与版本：\n%s", joined)
	}
}

// TestResolveConfigReportsBrokenPointerAsDataError 区分「没配」与「数据被改坏」：
// 前者是正常结果（found=false），后者必须是错误，否则坏数据会被当成空配置长期存在。
func TestResolveConfigReportsBrokenPointerAsDataError(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.title", "v1", 0, "req-1")
	item := e.itemByName("home.title")
	logs := captureLogs(t)

	// 指针指向 2，但快照行不存在：手工改坏的现场。
	e.db.items[item.ConfigID].LatestVersion = 2
	_, err := e.resolve(t, "home.title", nil)
	wantFail(t, err, model.ErrVersionNotFound, "指针指向不存在的快照")
	if !strings.Contains(logs.joined(), "没有对应快照行") {
		t.Fatalf("坏指针必须留 Error 日志：\n%s", logs.joined())
	}

	// value_type 被改成非法值：契约里没有「尽力解析」这一档。
	e2 := newTestEnv(t)
	e2.publish(t, "home.title", "v1", 0, "req-x")
	row := e2.versionRow(e2.itemByName("home.title").ConfigID, 1)
	bad := *row
	bad.ValueType = 99
	e2.putRawVersion(&bad)
	view := e2.mustResolve(t, "home.title", nil)
	if int32(view.GetValueType()) != 99 {
		t.Fatalf("读路径按快照原样透传值类型（判定归 model 写侧），实得 %v", view.GetValueType())
	}
}

func TestResolveConfigRejectsIllegalInputsBeforeTouchingStorage(t *testing.T) {
	e := newTestEnv(t)
	before := e.effects()
	cases := []struct {
		name   string
		req    *rpc.ResolveConfigReq
		want   error
		logSub string
	}{
		{"键为空", &rpc.ResolveConfigReq{}, model.ErrConfigKeyRequired, ""},
		{"键是空白", &rpc.ResolveConfigReq{CfgKey: "   "}, model.ErrConfigKeyRequired, ""},
		{"键含大写", &rpc.ResolveConfigReq{CfgKey: "Home.Title"}, model.ErrConfigKeyInvalid, ""},
		{"scope 是小程序", &rpc.ResolveConfigReq{CfgKey: "home.title", Scope: "mini_program"}, model.ErrScopeUnknown, ""},
		{"target 端未知", &rpc.ResolveConfigReq{CfgKey: "home.title",
			Target: rolloutCtx(7, rpc.ClientPlatform(9), "7.2.0")}, model.ErrPlatformUnknown, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewResolveConfigLogic(bg(), e.svc).ResolveConfig(tc.req)
			wantFail(t, err, tc.want, tc.name)
			if got := e.call("ConfigItem.FindOne"); got != 0 {
				t.Fatalf("%s：校验阶段就读了 %d 次库", tc.name, got)
			}
		})
	}
	e.requireSameEffects(t, before, "非法读取零副作用")
}

func TestResolveConfigServesFromProjectionAndRecomputesAfterPublish(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.title", "v1", 0, "req-1")
	item := e.itemByName("home.title")
	itemKey, verKey := e.limits.itemKey(model.ScopeGlobal, "home.title"), e.limits.versionKey(item.ConfigID, 1)
	requireOwnKeys(t, []string{itemKey, verKey}, testKeyPrefix, "投影键空间")

	// 第一次：两个键都 miss ⇒ 各回源一次并回填。
	delsBefore := e.effects()
	first := e.resolveCached(t, "req-r1", "home.title", nil)
	if first.GetVersion() != 1 {
		t.Fatalf("首轮解析版本不符：%+v", first)
	}
	if got := e.cache.setTTLs[itemKey]; got != itemPointerTTLSeconds {
		t.Fatalf("指针键 TTL 必须是 %d（指针一旧 latest_version/epoch 都可能旧）：got=%d", itemPointerTTLSeconds, got)
	}
	if got := e.cache.setTTLs[verKey]; got != 60 {
		t.Fatalf("不可变快照的 TTL 应取 Resolve.DefaultTTLSeconds：got=%d", got)
	}
	if e.cache.hits != 0 || e.cache.misses != 2 {
		t.Fatalf("首轮应为两次 miss：%+v", e.cache)
	}
	requireDeletedKeys(t, e.deletedKeysSince(delsBefore), nil, "解析只读不删")

	// 第二次：完全命中投影，不再读项与快照。
	items, vers := e.call("ConfigItem.FindOne"), e.call("ConfigVersion.FindOne")
	second := e.resolveCached(t, "req-r2", "home.title", nil)
	requireSameView(t, second, first, "投影命中应与回源同结论")
	if got := e.call("ConfigItem.FindOne"); got != items {
		t.Fatalf("投影命中仍回源取项 %d 次", got-items)
	}
	if got := e.call("ConfigVersion.FindOne"); got != vers {
		t.Fatalf("投影命中仍回源取快照 %d 次", got-vers)
	}
	if e.cache.hits != 2 {
		t.Fatalf("应有 2 次投影命中，实得 %d", e.cache.hits)
	}
	// 规则**永不缓存**（helpers.go configKeysOf）：带时间窗的投影会让定时开闸延后一个 TTL。
	if got := e.call("RolloutRule.ListCandidates"); got != 2 {
		t.Fatalf("每次解析都必须现查候选规则：got=%d want=2", got)
	}
	if n := len(e.cache.sets); n != 2 {
		t.Fatalf("投影回填次数应为 2（指针键 + 快照键），实得 %d：%v", n, e.cache.sets)
	}
	if n := len(e.cache.gets); n != 4 {
		t.Fatalf("投影读取次数应为 4（两次解析各读两个键），实得 %d：%v", n, e.cache.gets)
	}
	for _, k := range append(append([]string{}, e.cache.gets...), e.cache.sets...) {
		if k != itemKey && k != verKey {
			t.Fatalf("解析碰了不属于本次读取的键 %q（本服务只按 item/ver 两类键做投影）", k)
		}
	}

	// 发布 v2 ⇒ 只删指针键（快照不可变、无需失效），下一次解析看到新版本。
	delsBeforePublish := e.effects()
	e.publish(t, "home.title", "v2", 1, "req-2")
	requireDeletedKeys(t, e.deletedKeysSince(delsBeforePublish), []string{itemKey}, "发布删键")
	third := e.resolveCached(t, "req-r3", "home.title", nil)
	if third.GetVersion() != 2 {
		t.Fatalf("发布后端上还读到旧版本：%+v", third)
	}
	if third.GetEpoch() != e.itemByName("home.title").Epoch {
		t.Fatal("换代后的 epoch 未随指针一起更新")
	}

	// refresh=true 强制回源（proto「true 强制回源并回填缓存」）。
	before := e.call("ConfigItem.FindOne")
	reply, err := NewResolveConfigLogic(bg(), e.svc).ResolveConfig(&rpc.ResolveConfigReq{
		Ctx: &rpc.CallContext{RequestId: "req-r4"}, CfgKey: "home.title", Refresh: true,
	})
	wantOK(t, reply, err, "refresh 解析")
	if e.call("ConfigItem.FindOne") == before {
		t.Fatal("refresh=true 仍走了投影命中")
	}
}

func TestResolveConfigVerdictIsMySQLsEvenWhenRedisIsDown(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.title", "v1", 0, "req-1")
	logs := captureLogs(t)
	e.cache.getErr = errors.New("redis down")
	e.cache.setErr = errors.New("redis down")
	e.cache.delErr = errors.New("redis down")

	// 读失败按 miss 处理：结果仍然正确，只是没省下一次回源。
	view := e.resolveCached(t, "req-r2", "home.title", nil)
	if view.GetVersion() != 1 || view.GetValue() != "v1" {
		t.Fatalf("缓存故障改变了判定结果：%+v", view)
	}
	if !strings.Contains(logs.joined(), "ops-config/cache") {
		t.Fatalf("缓存故障必须记 Error 日志：\n%s", logs.joined())
	}
	// DEL 失败最多让旧投影多活一个 TTL：epoch 已换代，写接口照常成功。
	reply := e.publish(t, "home.title", "v2", 1, "req-3")
	if got := reply.GetItem().GetLatestVersion(); got != 2 {
		t.Fatalf("投影删不掉不该挡住发布：%d", got)
	}
	if !strings.Contains(logs.joined(), "删除 1 个投影键失败") {
		t.Fatalf("删键失败也要留日志（否则失效缺口不可见）：\n%s", logs.joined())
	}
	// 键没删掉但内存投影里是旧值：读失败再次退化成回源，仍读到新版本。
	e.cache.getErr = nil
	if again := e.resolveCached(t, "req-r4", "home.title", nil); again.GetVersion() != 2 {
		t.Fatalf("Get 恢复后应回源看到新版本：%+v", again)
	}
}

func TestResolveConfigWithoutCacheProjectionStillResolves(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.title", "v1", 0, "req-1")
	// svc.Cache 为 nil 是「未配 Redis」的合法形态（svc 注释：实现内部全部按 miss 处理，
	// 但 logic 也必须能在拿到 nil 时不 panic —— 测试注入的是真 nil）。
	e.svc.Cache = nil
	view := e.resolveCached(t, "req-r2", "home.title", rolloutCtx(7, platAndroid, "7.2.0"))
	if view.GetVersion() != 1 {
		t.Fatalf("无投影时解析失败：%+v", view)
	}
	reply := e.publish(t, "home.title", "v2", 1, "req-3")
	if got := reply.GetItem().GetLatestVersion(); got != 2 {
		t.Fatalf("无投影时发布失败：%d", got)
	}
	refresh, err := NewRefreshCacheLogic(bg(), e.svc).RefreshCache(&rpc.RefreshCacheReq{
		Ctx: callCtx("req-rf"), Target: "config", CfgKey: "home.title", Reason: "无投影刷新",
	})
	wantOK(t, refresh, err, "无投影刷新")
	if refresh.GetAffected() != 0 {
		t.Fatalf("没有缓存可删时 affected 必须是 0：%d", refresh.GetAffected())
	}
	if refresh.GetEpoch() == 0 {
		t.Fatal("换代是权威失效手段，与缓存无关，必须回新代次")
	}
}

func TestBatchResolveConfigAgreesWithSingleResolve(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.a", "va", 0, "req-a")
	e.publish(t, "home.b", "vb", 0, "req-b")
	// home.c 是「放量中」的形态：v1 只挂在白名单灰度规则上，v2 才是正式版本。
	grayRule := e.publishWithRules(t, "home.c", "vc-gray", 0, "req-c1", []*rpc.RolloutRuleSpec{
		{Name: "wl", Mode: modeWhitelist, WhitelistMids: []int64{7}, Priority: 10, Remark: "白名单"},
	})
	e.publish(t, "home.c", "vc-live", 0, "req-c2")
	e.openRule(t, grayRule.GetRules()[0].GetRuleId(), "req-open")
	if got := e.itemByName("home.c").LatestVersion; got != 2 {
		t.Fatalf("前置不成立：home.c 的正式版本应为 2，实得 %d", got)
	}

	off, _ := e.seedPublished(t, "home.off", "v1", "req-off")
	e.forceItemState(off.ConfigID, model.StateOff)

	target := rolloutCtx(7, platAndroid, "7.2.0")
	findBefore := e.call("ConfigItem.FindByKeys")
	reply, err := NewBatchResolveConfigLogic(bg(), e.svc).BatchResolveConfig(&rpc.BatchResolveConfigReq{
		Ctx: &rpc.CallContext{}, CfgKeys: []string{"home.a", "home.a", " home.b ", "home.c", "home.off", "home.nope", ""},
		Target: target,
	})
	wantOK(t, reply, err, "BatchResolveConfig")
	// 一次批量不许退化成 N 次单键项查询（batchresolveconfiglogic：禁止循环 FindOne）。
	if got := e.call("ConfigItem.FindByKeys") - findBefore; got != 1 {
		t.Fatalf("6 个键的批次取了 %d 次项，应为 1", got)
	}

	var gotKeys []string
	for _, v := range reply.GetConfigs() {
		gotKeys = append(gotKeys, v.GetCfgKey())
	}
	if strings.Join(gotKeys, ",") != "home.a,home.b,home.c" {
		t.Fatalf("命中集合/顺序不符（去重去空后按入参序）：%v", gotKeys)
	}
	if strings.Join(reply.GetMissingKeys(), ",") != "home.off,home.nope" {
		t.Fatalf("missing_keys 不符：%v", reply.GetMissingKeys())
	}
	// 与单键解析逐字段一致（灰度结论、值类型、归因），这正是 conv_resolve 复用内核的意义。
	for i, cfgKey := range []string{"home.a", "home.b", "home.c"} {
		single := e.mustResolve(t, cfgKey, target)
		requireSameView(t, reply.GetConfigs()[i], single, "批量第 "+cfgKey+" 项")
	}
	// home.c 命中白名单灰度：读到的必须是挂在规则上的 v1，而不是正式版本 v2。
	if v := reply.GetConfigs()[2]; v.GetVersion() != 1 ||
		v.GetRolloutRuleId() != grayRule.GetRules()[0].GetRuleId() || v.GetValue() != "vc-gray" {
		t.Fatalf("批量里的灰度结论丢了：%+v", v)
	}
	// 不在白名单里的 mid 走正式版本：同一批键、不同 target，只可能差在灰度结论上。
	other := rolloutCtx(99, platAndroid, "7.2.0")
	plain, perr := NewBatchResolveConfigLogic(bg(), e.svc).BatchResolveConfig(&rpc.BatchResolveConfigReq{
		Ctx: &rpc.CallContext{}, CfgKeys: []string{"home.c"}, Target: other,
	})
	wantOK(t, plain, perr, "非白名单 mid 的批量")
	if got := plain.GetConfigs()[0]; got.GetVersion() != 2 || got.GetRolloutRuleId() != 0 || got.GetValue() != "vc-live" {
		t.Fatalf("mid=99 不该被放量：%+v", got)
	}
	if reply.GetTtl() != 60 {
		t.Fatalf("整批建议 TTL 应取本批最小值：got=%d", reply.GetTtl())
	}
}

func TestBatchResolveConfigKeepsTheSmallestTTLAndZeroWrites(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.a", "va", 0, "req-a")
	before := e.effects()

	reply, err := NewBatchResolveConfigLogic(bg(), e.svc).BatchResolveConfig(&rpc.BatchResolveConfigReq{
		Ctx: &rpc.CallContext{}, CfgKeys: []string{"home.a"}, Target: rolloutCtx(1, platAndroid, "1.0.0"),
	})
	wantOK(t, reply, err, "BatchResolveConfig 单键")
	if reply.GetTtl() != reply.GetConfigs()[0].GetTtl() {
		t.Fatalf("单键批次的 TTL 应等于该项 TTL：%d vs %d", reply.GetTtl(), reply.GetConfigs()[0].GetTtl())
	}
	e.requireSameEffects(t, before, "批量读取零副作用")

	// 一个键停用/未发布不许拖垮整批（网关首页其它模块还要渲染）。
	e2 := newTestEnv(t)
	e2.publish(t, "home.a", "va", 0, "req-a")
	e2.publish(t, "home.b", "vb", 0, "req-b")
	unpub := e2.seedItem(t, "home.unpub", model.ScopeGlobal, model.ValueTypeString, model.StateOn)
	e2.seedVersion(t, unpub.ConfigID, 1, "v1", model.ValueTypeString, model.ChangeTypeCreate, "s-u", "种子")
	mixed, err := NewBatchResolveConfigLogic(bg(), e2.svc).BatchResolveConfig(&rpc.BatchResolveConfigReq{
		Ctx: &rpc.CallContext{}, CfgKeys: []string{"home.a", "home.unpub", "home.b"},
	})
	wantOK(t, mixed, err, "混入未发布键的批次")
	if len(mixed.GetConfigs()) != 2 || strings.Join(mixed.GetMissingKeys(), ",") != "home.unpub" {
		t.Fatalf("未发布键应是 missing 而不是整批失败：%+v", mixed)
	}
	// 全部未命中：configs 空、ttl 回 0（调用方据此不缓存）。
	none, err := NewBatchResolveConfigLogic(bg(), e2.svc).BatchResolveConfig(&rpc.BatchResolveConfigReq{
		Ctx: &rpc.CallContext{}, CfgKeys: []string{"home.nope"},
	})
	wantOK(t, none, err, "全未命中批次")
	if len(none.GetConfigs()) != 0 || none.GetTtl() != 0 || len(none.GetMissingKeys()) != 1 {
		t.Fatalf("全未命中批次不该回缓存建议：%+v", none)
	}
}

func TestBatchResolveConfigBoundsAreCheckedOnDedupedKeys(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.a", "va", 0, "req-a")
	before := e.effects()

	dupKeys := make([]string, 0, 51)
	for i := 0; i < 50; i++ {
		dupKeys = append(dupKeys, "home.a")
	}
	dupKeys = append(dupKeys, "home.a")
	// proto「单次最多 50 个键」是对**去重后**的条数判定（normalizeBatchKeys 注释）。
	one, err := NewBatchResolveConfigLogic(bg(), e.svc).BatchResolveConfig(&rpc.BatchResolveConfigReq{
		Ctx: &rpc.CallContext{}, CfgKeys: dupKeys,
	})
	wantOK(t, one, err, "51 个重复键去重后只剩 1 个")
	if len(one.GetConfigs()) != 1 {
		t.Fatalf("去重后只该回一份：%d", len(one.GetConfigs()))
	}

	distinct := make([]string, 0, 51)
	for i := 0; i < 51; i++ {
		distinct = append(distinct, fmt.Sprintf("home.k%d", i))
	}
	_, err = NewBatchResolveConfigLogic(bg(), e.svc).BatchResolveConfig(&rpc.BatchResolveConfigReq{
		Ctx: &rpc.CallContext{}, CfgKeys: distinct,
	})
	wantFail(t, err, model.ErrBatchTooLarge, "51 个不同键")

	_, err = NewBatchResolveConfigLogic(bg(), e.svc).BatchResolveConfig(&rpc.BatchResolveConfigReq{
		Ctx: &rpc.CallContext{}, CfgKeys: []string{"  ", "", "\t"},
	})
	wantFail(t, err, model.ErrBatchEmpty, "全空白键")

	_, err = NewBatchResolveConfigLogic(bg(), e.svc).BatchResolveConfig(&rpc.BatchResolveConfigReq{
		Ctx: &rpc.CallContext{}, CfgKeys: []string{"home.a", "Bad Key"},
	})
	wantFail(t, err, model.ErrConfigKeyInvalid, "非法键必须整批被拒（不是悄悄丢掉那一个）")

	_, err = NewBatchResolveConfigLogic(bg(), e.svc).BatchResolveConfig(&rpc.BatchResolveConfigReq{
		Ctx: &rpc.CallContext{}, CfgKeys: []string{"home.a"}, Scope: "mini_program",
	})
	wantFail(t, err, model.ErrScopeUnknown, "未知 scope")

	_, err = NewBatchResolveConfigLogic(bg(), e.svc).BatchResolveConfig(&rpc.BatchResolveConfigReq{
		Ctx: &rpc.CallContext{}, CfgKeys: []string{"home.a"}, Target: rolloutCtx(7, rpc.ClientPlatform(9), "1.0.0"),
	})
	wantFail(t, err, model.ErrPlatformUnknown, "未知端")

	e.requireSameEffects(t, before, "被拒的批量读取零副作用")
}

func TestRefreshCacheBumpsEpochFirstAndDeletesOnlyItsOwnKeys(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.a", "va", 0, "req-a")
	e.publish(t, "home.b", "vb", 0, "req-b")
	item := e.itemByName("home.a")
	e.resolveCached(t, "req-r1", "home.a", nil)
	e.resolveCached(t, "req-r2", "home.b", nil)

	// 只给 cfg_key：删该键的指针投影，回一份确定代次。
	before := e.itemRow(item.ConfigID).Epoch
	delsBefore := e.effects()
	reply, err := NewRefreshCacheLogic(bg(), e.svc).RefreshCache(&rpc.RefreshCacheReq{
		Ctx: callCtx("req-rf1"), Target: "config", CfgKey: "home.a", Reason: "单键失效",
	})
	wantOK(t, reply, err, "RefreshCache(config)")
	if reply.GetAffected() != 1 {
		t.Fatalf("只该删掉 1 个指针键：%d", reply.GetAffected())
	}
	requireDeletedKeys(t, e.deletedKeysSince(delsBefore),
		[]string{e.limits.itemKey(model.ScopeGlobal, "home.a")}, "定向刷新的删除范围")
	if reply.GetEpoch() != before+1 {
		t.Fatalf("epoch 应回换代后的代次：got=%d before=%d", reply.GetEpoch(), before)
	}
	if got := e.itemByName("home.a").State; got != model.StateOn {
		t.Fatal("刷新不该碰配置项的启停位")
	}
	// 键空间边界：本服务只许删自己前缀下的键（AGENTS.md §5）。
	requireOwnKeys(t, e.cache.dels, testKeyPrefix, "RefreshCache 删键")
	// 另一个键的投影必须还在：误删全域会把一次定向刷新变成全库回源。
	if _, ok := e.cache.data[e.limits.itemKey(model.ScopeGlobal, "home.b")]; !ok {
		t.Fatalf("定向刷新删掉了别的键：%v", e.cache.dels)
	}

	// target=all：全域没有单一代次可回（每行各自 +1），proto 里 epoch 回 0 表示「整体已失效」。
	all, err := NewRefreshCacheLogic(bg(), e.svc).RefreshCache(&rpc.RefreshCacheReq{
		Ctx: callCtx("req-rf2"), Target: "all", Reason: "全域失效",
	})
	wantOK(t, all, err, "RefreshCache(all)")
	if all.GetEpoch() != 0 {
		t.Fatalf("全域失效没有单一代次可回，必须是 0：%d", all.GetEpoch())
	}
	// affected 回的是**实际删除**条数（svc.CacheKV.Del 注释），不是候选键数：
	// home.a 的指针键上一步已经删过，这里再删一次不计数。
	if all.GetAffected() != 1 {
		t.Fatalf("affected 应为实际删除条数 1：got=%d", all.GetAffected())
	}
	if got := e.itemByName("home.a").Epoch; got != before+2 {
		t.Fatalf("target=all 未把每行各自换代：got=%d want=%d", got, before+2)
	}
}

func TestRefreshCacheRejectsTargetsItCannotExplain(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.a", "va", 0, "req-a")
	epoch := e.itemByName("home.a").Epoch
	before := e.effects()

	cases := []struct {
		name string
		req  *rpc.RefreshCacheReq
		want error
	}{
		{"target 为空", &rpc.RefreshCacheReq{Ctx: callCtx("r1"), Reason: "x"}, model.ErrRefreshTargetRequired},
		{"target 未知（不能当成「什么都不做也算成功」）", &rpc.RefreshCacheReq{Ctx: callCtx("r2"), Target: "everywhere", Reason: "x"}, model.ErrRefreshTargetUnsupported},
		{"刷一个不存在的键", &rpc.RefreshCacheReq{Ctx: callCtx("r3"), Target: "config", CfgKey: "home.nope", Reason: "x"}, model.ErrConfigNotFound},
		{"target=all 不带理由", &rpc.RefreshCacheReq{Ctx: callCtx("r4"), Target: "all"}, model.ErrRefreshReasonRequired},
		{"定向刷新不带理由", &rpc.RefreshCacheReq{Ctx: callCtx("r5"), Target: "config", CfgKey: "home.a"}, model.ErrReasonRequired},
		{"topic 没给 ID", &rpc.RefreshCacheReq{Ctx: callCtx("r6"), Target: "topic", Reason: "x"}, model.ErrTopicNotFound},
		{"slot 没给 ID", &rpc.RefreshCacheReq{Ctx: callCtx("r7"), Target: "slot", Reason: "x"}, model.ErrSlotNotFound},
		{"键名非法", &rpc.RefreshCacheReq{Ctx: callCtx("r8"), Target: "config", CfgKey: "Home A", Reason: "x"}, model.ErrConfigKeyInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRefreshCacheLogic(bg(), e.svc).RefreshCache(tc.req)
			wantFail(t, err, tc.want, tc.name)
			e.requireSameEffects(t, before, tc.name+" 零副作用")
		})
	}
	if got := e.itemByName("home.a").Epoch; got != epoch {
		t.Fatalf("被拒的刷新换代了：%d -> %d", epoch, got)
	}
}

func TestListConfigsClampsPageSizeAndReportsTotal(t *testing.T) {
	e := newTestEnv(t)
	const seeded = 105
	for i := 0; i < seeded; i++ {
		e.seedItem(t, fmt.Sprintf("home.k%03d", i), model.ScopeGlobal, model.ValueTypeString, model.StateOn)
	}
	// 一个停用项：state 过滤与 total 口径都要对上。
	e.seedItem(t, "home.off", model.ScopeGlobal, model.ValueTypeString, model.StateOff)

	// proto：ps「上限 100」；model.PageOrDefault 对越界取「夹到上限」而不是报错。
	reply, err := NewListConfigsLogic(bg(), e.svc).ListConfigs(&rpc.ListConfigsReq{Ps: 500})
	wantOK(t, reply, err, "ListConfigs(ps=500)")
	if got := len(reply.GetItems()); got != 100 {
		t.Fatalf("ps 未夹到服务端上限：%d", got)
	}
	if reply.GetTotal() != seeded+1 {
		t.Fatalf("total 应是全量条数而不是本页条数：%d", reply.GetTotal())
	}
	// 排序口径 cfg_key ASC, scope ASC（model.List 的 ORDER BY）。
	if reply.GetItems()[0].GetCfgKey() != "home.k000" ||
		reply.GetItems()[99].GetCfgKey() != "home.k099" {
		t.Fatalf("排序不符：%q..%q", reply.GetItems()[0].GetCfgKey(), reply.GetItems()[99].GetCfgKey())
	}

	page2, err := NewListConfigsLogic(bg(), e.svc).ListConfigs(&rpc.ListConfigsReq{Pn: 2, Ps: 100})
	wantOK(t, page2, err, "ListConfigs 第二页")
	if len(page2.GetItems()) != 6 || page2.GetTotal() != seeded+1 {
		t.Fatalf("第二页应有 6 条：got=%d total=%d", len(page2.GetItems()), page2.GetTotal())
	}
	if page2.GetItems()[0].GetCfgKey() != "home.k100" {
		t.Fatalf("翻页串位：%q", page2.GetItems()[0].GetCfgKey())
	}

	// ps 缺省（0）= 用上限；pn=0 归一到第 1 页，而不是拒绝。
	def, err := NewListConfigsLogic(bg(), e.svc).ListConfigs(&rpc.ListConfigsReq{})
	wantOK(t, def, err, "ListConfigs 缺省分页")
	if len(def.GetItems()) != 100 {
		t.Fatalf("缺省 ps 应回落到上限：got=%d", len(def.GetItems()))
	}

	// 但负数是入参错误：把「pn 拼错」当成「回到第一页」会让调用方以为翻页成功。
	for _, req := range []*rpc.ListConfigsReq{{Pn: -1}, {Ps: -1}, {Pn: -1, Ps: -1}} {
		_, err = NewListConfigsLogic(bg(), e.svc).ListConfigs(req)
		wantFail(t, err, model.ErrInvalidPage, fmt.Sprintf("负分页 %+v", req))
	}
	_, err = NewListConfigsLogic(bg(), e.svc).ListConfigs(&rpc.ListConfigsReq{Scope: "mini_program"})
	wantFail(t, err, model.ErrScopeUnknown, "未知 scope")
	_, err = NewListConfigsLogic(bg(), e.svc).ListConfigs(&rpc.ListConfigsReq{State: 7})
	wantFail(t, err, model.ErrRuleStateInvalid, "非法 state")
	_, err = NewListConfigsLogic(bg(), e.svc).ListConfigs(&rpc.ListConfigsReq{Keyword: strings.Repeat("词", 65)})
	wantFail(t, err, model.ErrBatchTooLarge, "模糊词超长")

	// 投影不回配置值：值只存在于 ops_config_version（listconfigslogic 注释）。
	// 这里能钉住的口径是「列表行回的是指针」：调用方据此再走 ListConfigVersions。
	one := def.GetItems()[0]
	if one.GetCfgKey() != "home.k000" || one.GetLatestVersion() != 0 || one.GetState() != model.StateOn {
		t.Fatalf("列表行应是「指针 + 身份」：%+v", one)
	}
}
