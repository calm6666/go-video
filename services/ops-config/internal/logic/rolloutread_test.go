// 本文件钉住 ListRolloutRules（后台分页列灰度规则），并把三个「分页读」接口之间
// 刻意不一致的地方一次说清：
//
//  1. 排序事实源：ops_rollout_rule.go:295 是
//     `ORDER BY config_id ASC, priority ASC, rule_id ASC`。config_id 排在第一级，
//     所以「按优先级全局排序」是错的；同 config 同 priority 才轮到 rule_id 兜底。
//  2. 守卫次序与 ListConfigVersions 相反：本接口先分页、再 version、再 state、
//     最后才碰 cfg_key（listrolloutruleslogic.go:31-64）。两个接口都测时，
//     「谁先报错」就是唯一可观察的次序证据。
//  3. 键不存在在这里是 fail-**stop**（第 60-62 行：ErrConfigNotFound），
//     在版本历史那边是 fail-soft：拼错 key 与「这个 key 没有规则」都回空列表的话，
//     前者会被当成后者，运营会以为自己已经清干净了灰度。
//
// 另有一条现状缺陷按原样钉住（见 README「已知缺口」）：
// 本接口回的是**裸 total**（第 78 行 `Total: total`），没有过 lim.totalOf，
// 因此 Query.CountTotal=false 对它不生效——与 ListConfigs/ListTopics/ListSlots/ListClientSwitches 不同。
package logic

import (
	"errors"
	"strings"
	"testing"

	"go-video/services/ops-config/internal/config"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"
)

var errRuleStoreDown = errors.New("read ops_rollout_rule: dead-lock found")

func (e *testEnv) listRules(t *testing.T, req *rpc.ListRolloutRulesReq) (*rpc.ListRolloutRulesReply, error) {
	t.Helper()
	return NewListRolloutRulesLogic(bg(), e.svc).ListRolloutRules(req)
}

// ruleNames 把投影压成规则名序列：次序期望由真实 ORDER BY 推导，
// 而 config_id / rule_id 由替身自增分配，用例不猜它们从几开始。
func ruleNames(items []*rpc.RolloutRule) []string {
	out := make([]string, 0, len(items))
	for _, r := range items {
		out = append(out, r.GetName())
	}
	return out
}

// requireRuleOrderFollowsTheRealOrderBy 按 `config_id ASC, priority ASC, rule_id ASC`
// 复核回读结果自身：这是「派生顺序」的判据，比抄一串硬编码主键更经得起改动。
func requireRuleOrderFollowsTheRealOrderBy(t *testing.T, items []*rpc.RolloutRule, label string) {
	t.Helper()
	for i := 1; i < len(items); i++ {
		prev, cur := items[i-1], items[i]
		if prev.GetConfigId() > cur.GetConfigId() {
			t.Fatalf("%s：第一级不是 config_id ASC：%s(%d) 排在了 %s(%d) 前面",
				label, prev.GetName(), prev.GetConfigId(), cur.GetName(), cur.GetConfigId())
		}
		if prev.GetConfigId() != cur.GetConfigId() {
			continue
		}
		if prev.GetPriority() > cur.GetPriority() {
			t.Fatalf("%s：第二级不是 priority ASC：%s(%d) 排在了 %s(%d) 前面",
				label, prev.GetName(), prev.GetPriority(), cur.GetName(), cur.GetPriority())
		}
		if prev.GetPriority() != cur.GetPriority() {
			continue
		}
		// 第三级 rule_id 兜底：少了它，同 config 同 priority 的两条规则在翻页时会重复或漏行。
		if prev.GetRuleId() >= cur.GetRuleId() {
			t.Fatalf("%s：同 config 同 priority 没按 rule_id ASC 兜底：%s(%d) 排在了 %s(%d) 前面",
				label, prev.GetName(), prev.GetRuleId(), cur.GetName(), cur.GetRuleId())
		}
	}
}

// requireRuleRowMatches 逐字段比对投影与库里的规则行。
func requireRuleRowMatches(t *testing.T, got *rpc.RolloutRule, row *model.RolloutRule, label string) {
	t.Helper()
	if got.GetRuleId() != row.RuleID || got.GetConfigId() != row.ConfigID ||
		got.GetVersion() != row.Version || got.GetName() != row.Name ||
		int32(got.GetMode()) != row.Mode || got.GetPercentage() != row.Percentage ||
		got.GetAppVersionMin() != row.AppVersionMin || got.GetAppVersionMax() != row.AppVersionMax ||
		got.GetMidSuffixes() != row.MidSuffixes || got.GetPriority() != row.Priority ||
		got.GetState() != row.State || got.GetOperatorId() != row.OperatorID ||
		got.GetRemark() != row.Remark || got.GetStartAt() != row.StartAt ||
		got.GetEndAt() != row.EndAt || got.GetCtime() != row.Ctime || got.GetMtime() != row.Mtime {
		t.Fatalf("%s：投影与 ops_rollout_rule 行不一致\n  got=%+v\n row=%+v", label, got, row)
	}
}

// ruleBook 记下种子的业务键与主键，供派生断言使用（不硬编码自增 ID）。
type ruleBook struct {
	itemA, itemAndroid, itemB *model.ConfigItem
}

// seedRuleBook 布三个配置项 + 七条规则，插入顺序刻意与任何一列的序都不同：
// 只有一种情况能碰对期望，就是实现真的按那句 ORDER BY 排过。
func (e *testEnv) seedRuleBook(t *testing.T) ruleBook {
	t.Helper()
	a := e.seedItem(t, "rollout.a", model.ScopeGlobal, model.ValueTypeString, model.StateOn)
	android := e.seedItem(t, "rollout.a", model.ScopeAndroid, model.ValueTypeString, model.StateOn)
	b := e.seedItem(t, "rollout.b", model.ScopeGlobal, model.ValueTypeString, model.StateOn)
	rule := func(item *model.ConfigItem, name string, version int64, priority int32, state int32) {
		e.seedRule(t, &model.RolloutRule{
			ConfigID: item.ConfigID, Version: version, Name: name,
			Mode: model.ModeFull, Priority: priority, State: state, OperatorID: 42,
		})
	}
	// 插入顺序：B 的先插、A 的后插，同 priority 的两条按「rule_id 兜底」的正序插。
	rule(b, "b-v1-p01", 1, 1, model.StateOn)        // 全局优先级最小，但 config_id 最大 → 仍排最后
	rule(a, "a-v1-p100", 1, 0, model.StateOn)       // 不给 priority → model 默认 100，同 config 内排最后
	rule(a, "a-v1-p10-first", 1, 10, model.StateOn) // 同 priority 两条：先插的 rule_id 更小 → 排前
	rule(a, "a-v1-p10-second", 1, 10, model.StateOff)
	rule(a, "a-v2-p05", 2, 5, model.StateOn)
	rule(android, "ab-v1-p07", 1, 7, model.StateOn)
	rule(b, "b-v2-p02", 2, 2, model.StateOff)
	return ruleBook{itemA: a, itemAndroid: android, itemB: b}
}

func TestListRolloutRulesRejectsIllegalInputsBeforeTouchingStorage(t *testing.T) {
	e := newTestEnv(t)
	e.seedRuleBook(t)
	before := e.effects()
	// 查询计数是全局累加的：先取基线，再断言差值（见 fakes_test.go 的 calls 说明）。
	listBase, findOneBase := e.call("RolloutRule.List"), e.call("ConfigItem.FindOne")

	cases := []struct {
		name string
		req  *rpc.ListRolloutRulesReq
		want error
	}{
		{"pn 为负", &rpc.ListRolloutRulesReq{Pn: -1}, model.ErrInvalidPage},
		{"ps 为负", &rpc.ListRolloutRulesReq{Ps: -1}, model.ErrInvalidPage},
		// 负版本号是入参错误，本接口沿用 ErrVersionNotFound 表达（没有更贴切的哨兵）。
		// 现状哨兵：若为它补一个 ErrInvalidVersion，本用例要按新哨兵改写而不是放宽。
		{"version 为负", &rpc.ListRolloutRulesReq{Version: -1}, model.ErrVersionNotFound},
		{"version 为负且 state 非法", &rpc.ListRolloutRulesReq{Version: -3, State: 7}, model.ErrVersionNotFound},
		// state=0 是「全部」，第四个值必须报错：放过去等于后台的筛选条件凭空失效。
		{"state 第四个值", &rpc.ListRolloutRulesReq{State: 3}, model.ErrRuleStateInvalid},
		{"state 为负", &rpc.ListRolloutRulesReq{State: -2}, model.ErrRuleStateInvalid},
		{"cfg_key 含大写", &rpc.ListRolloutRulesReq{CfgKey: "Rollout.A"}, model.ErrConfigKeyInvalid},
		{"cfg_key 含连字符", &rpc.ListRolloutRulesReq{CfgKey: "rollout-a"}, model.ErrConfigKeyInvalid},
		{"cfg_key 合法但 scope 未知",
			&rpc.ListRolloutRulesReq{CfgKey: "rollout.a", Scope: "weixin"}, model.ErrScopeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := e.listRules(t, tc.req)
			wantFail(t, err, tc.want, tc.name)
			if reply != nil {
				t.Fatalf("%s：出错还回了响应体：%+v", tc.name, reply)
			}
			if got := e.call("RolloutRule.List"); got != listBase {
				t.Fatalf("%s：入参校验阶段就查了规则表：%d → %d 次", tc.name, listBase, got)
			}
			if got := e.call("ConfigItem.FindOne"); got != findOneBase {
				t.Fatalf("%s：入参校验阶段就查了配置项：%d → %d 次", tc.name, findOneBase, got)
			}
		})
	}
	// 守卫次序本身（与 ListConfigVersions 相反：那边 cfg_key 在最前）：
	// 这里分页第一、version 第二、state 第三、cfg_key 第四。
	if _, err := e.listRules(t, &rpc.ListRolloutRulesReq{Pn: -1, CfgKey: "BAD KEY", Version: -1}); !errors.Is(err, model.ErrInvalidPage) {
		t.Fatalf("分页守卫没排在最前：%v", err)
	}
	if _, err := e.listRules(t, &rpc.ListRolloutRulesReq{Version: -1, CfgKey: "BAD KEY"}); !errors.Is(err, model.ErrVersionNotFound) {
		t.Fatalf("version 守卫没排在 cfg_key 之前：%v", err)
	}
	if _, err := e.listRules(t, &rpc.ListRolloutRulesReq{State: 9, CfgKey: "BAD KEY"}); !errors.Is(err, model.ErrRuleStateInvalid) {
		t.Fatalf("state 守卫没排在 cfg_key 之前：%v", err)
	}
	// 合法取值必须放行：0 是「不过滤」而不是「筛出一个不存在的端/状态」。
	for _, req := range []*rpc.ListRolloutRulesReq{{}, {State: model.StateOn}, {State: model.StateOff},
		{Version: 0}, {Version: 1}, {Pn: 0, Ps: 0}} {
		if _, err := e.listRules(t, req); err != nil {
			t.Fatalf("合法请求 %+v 被拒：%v", req, err)
		}
	}
	e.requireSameEffects(t, before, "非法 ListRolloutRules 入参零副作用")
}

func TestListRolloutRulesOrdersByConfigThenPriorityThenRuleID(t *testing.T) {
	e := newTestEnv(t)
	book := e.seedRuleBook(t)

	all, err := e.listRules(t, &rpc.ListRolloutRulesReq{Ps: 100})
	rows := wantOK(t, all, err, "全量一页")
	want := []string{"a-v2-p05", "a-v1-p10-first", "a-v1-p10-second", "a-v1-p100",
		"ab-v1-p07", "b-v1-p01", "b-v2-p02"}
	if got := strings.Join(ruleNames(rows.GetItems()), ","); got != strings.Join(want, ",") {
		t.Fatalf("次序不符（期望 config_id ASC 优先，再 priority ASC，再 rule_id ASC）：\n  got =%s\n want=%s",
			got, strings.Join(want, ","))
	}
	// 派生判据：与真实 ORDER BY 的三段逐条复核（主键由替身分配，不抄字面量）。
	requireRuleOrderFollowsTheRealOrderBy(t, rows.GetItems(), "全量一页")
	if rows.GetTotal() != 7 {
		t.Fatalf("total 应是筛选后全量：%d", rows.GetTotal())
	}
	// config_id 是三个不同的项：rollout.a 的 global 与 android 绝不合并。
	ids := map[int64]int{}
	for _, r := range rows.GetItems() {
		ids[r.GetConfigId()]++
	}
	if len(ids) != 3 || ids[book.itemA.ConfigID] != 4 || ids[book.itemAndroid.ConfigID] != 1 ||
		ids[book.itemB.ConfigID] != 2 {
		t.Fatalf("规则没有落在各自的 config_id 上：%v", ids)
	}
	// 不给 priority 的那条由 model 兜底成 100：缺了默认值「首个命中」的次序就会变。
	var defaulted *rpc.RolloutRule
	for _, r := range rows.GetItems() {
		if r.GetName() == "a-v1-p100" {
			defaulted = r
		}
	}
	if defaulted == nil || defaulted.GetPriority() != 100 {
		t.Fatalf("priority 缺省没被 model 兜底成 100：%+v", defaulted)
	}

	// 翻页：两页拼起来必须与全量逐条同序，且 total 不漂移。
	p1, err := e.listRules(t, &rpc.ListRolloutRulesReq{Pn: 1, Ps: 4})
	first := wantOK(t, p1, err, "第一页")
	p2, err := e.listRules(t, &rpc.ListRolloutRulesReq{Pn: 2, Ps: 4})
	second := wantOK(t, p2, err, "第二页")
	if len(first.GetItems()) != 4 || len(second.GetItems()) != 3 {
		t.Fatalf("页大小没被执行：%d / %d", len(first.GetItems()), len(second.GetItems()))
	}
	joined := append(ruleNames(first.GetItems()), ruleNames(second.GetItems())...)
	if strings.Join(joined, ",") != strings.Join(ruleNames(rows.GetItems()), ",") {
		t.Fatalf("翻页拼接与全量不一致：\n  got =%v\n want=%v", joined, ruleNames(rows.GetItems()))
	}
	if first.GetTotal() != 7 || second.GetTotal() != 7 {
		t.Fatalf("翻页时 total 漂移：%d / %d", first.GetTotal(), second.GetTotal())
	}

	// 越界页：空 items + 保留 total（与「筛不到」的 total=0 可区分）。
	far, err := e.listRules(t, &rpc.ListRolloutRulesReq{Pn: 9, Ps: 4})
	out := wantOK(t, far, err, "越界页")
	if len(out.GetItems()) != 0 || out.GetTotal() != 7 {
		t.Fatalf("越界页口径不符：%d 条 total=%d", len(out.GetItems()), out.GetTotal())
	}
	// ps 越界夹到 Query.MaxPageSize(100)、缺省回落到上限，都不改变本页条数。
	for _, req := range []*rpc.ListRolloutRulesReq{{Ps: 900}, {}} {
		reply, err := e.listRules(t, req)
		got := wantOK(t, reply, err, "ps 夹取")
		if len(got.GetItems()) != 7 {
			t.Fatalf("%+v：夹取改变了本页条数：%d", req, len(got.GetItems()))
		}
	}
	// 只读：零写、零缓存（规则带时间窗，永不进投影）、零审计。
	if len(e.cache.sets) != 0 || len(e.cache.gets) != 0 || len(e.cache.dels) != 0 || len(e.audit.reqs) != 0 {
		t.Fatalf("ListRolloutRules 动了缓存或审计：sets=%v gets=%v dels=%v audit=%d",
			e.cache.sets, e.cache.gets, e.cache.dels, len(e.audit.reqs))
	}
}

// 每一段 WHERE 一条唯一种子：config_id / version / state 三个维度分别钉。
// 另钉两处「没有」：
//   - 本接口没有 mode 条件（rpc.ListRolloutRulesReq 无 mode 字段，
//     而 model 的 RolloutRuleFilter.Mode 存在但永远为 0）；
//   - 本接口不做时间窗判定：窗口只属于运行时（ListCandidates），
//     过期规则在后台必须仍列得出来，否则「昨天谁被放量了」无从回答。
func TestListRolloutRulesFiltersAreEachDiscriminable(t *testing.T) {
	e := newTestEnv(t)
	book := e.seedRuleBook(t)
	// 一条已过窗的规则：只进后台列表，不参与运行时判定。
	expired := e.seedRule(t, &model.RolloutRule{
		ConfigID: book.itemB.ConfigID, Version: 1, Name: "b-v1-window", Mode: model.ModeFull,
		Priority: 3, State: model.StateOn, StartAt: 1, EndAt: 2,
	})

	for _, tc := range []struct {
		name string
		req  *rpc.ListRolloutRulesReq
		want string
	}{
		// 三条 b 规则的次序由 priority ASC 决定（1 < 2 < 3），与插入顺序无关。
		{"不给条件＝全部", &rpc.ListRolloutRulesReq{Ps: 100},
			"a-v2-p05,a-v1-p10-first,a-v1-p10-second,a-v1-p100,ab-v1-p07,b-v1-p01,b-v2-p02,b-v1-window"},
		// version = ?：0 不过滤；1/2 各命中一条子集。
		{"version=2", &rpc.ListRolloutRulesReq{Version: 2, Ps: 100}, "a-v2-p05,b-v2-p02"},
		{"version=1", &rpc.ListRolloutRulesReq{Version: 1, Ps: 100},
			"a-v1-p10-first,a-v1-p10-second,a-v1-p100,ab-v1-p07,b-v1-p01,b-v1-window"},
		{"version=99（不存在）", &rpc.ListRolloutRulesReq{Version: 99, Ps: 100}, ""},
		// state = ?：启用/停用是划分，合起来是全量。
		{"state=ON", &rpc.ListRolloutRulesReq{State: model.StateOn, Ps: 100},
			"a-v2-p05,a-v1-p10-first,a-v1-p100,ab-v1-p07,b-v1-p01,b-v1-window"},
		{"state=OFF", &rpc.ListRolloutRulesReq{State: model.StateOff, Ps: 100}, "a-v1-p10-second,b-v2-p02"},
		// config_id = ?（由 cfg_key + scope 解析而来）：同键不同 scope 必须是两个项。
		{"cfg_key=rollout.a/global", &rpc.ListRolloutRulesReq{CfgKey: "rollout.a", Ps: 100},
			"a-v2-p05,a-v1-p10-first,a-v1-p10-second,a-v1-p100"},
		{"cfg_key=rollout.a/android",
			&rpc.ListRolloutRulesReq{CfgKey: "rollout.a", Scope: model.ScopeAndroid, Ps: 100}, "ab-v1-p07"},
		{"cfg_key=rollout.b", &rpc.ListRolloutRulesReq{CfgKey: "rollout.b", Ps: 100},
			"b-v1-p01,b-v2-p02,b-v1-window"},
		{"cfg_key 两端空白被裁掉", &rpc.ListRolloutRulesReq{CfgKey: "  rollout.b  ", Ps: 100},
			"b-v1-p01,b-v2-p02,b-v1-window"},
		// 纯空白＝不过滤（第 46 行的 TrimSpace 判空）：不能当成「一个不存在的键」而报错。
		// 注意 scope 此时也不校验：它没有配对的 cfg_key。
		{"纯空白 cfg_key＝不过滤", &rpc.ListRolloutRulesReq{CfgKey: "   ", Scope: "weixin", Ps: 100},
			"a-v2-p05,a-v1-p10-first,a-v1-p10-second,a-v1-p100,ab-v1-p07,b-v1-p01,b-v2-p02,b-v1-window"},
		// 三个维度相交，而不是「后一个覆盖前一个」。
		{"cfg_key + version", &rpc.ListRolloutRulesReq{CfgKey: "rollout.a", Version: 1, Ps: 100},
			"a-v1-p10-first,a-v1-p10-second,a-v1-p100"},
		{"cfg_key + state", &rpc.ListRolloutRulesReq{CfgKey: "rollout.a", State: model.StateOff, Ps: 100},
			"a-v1-p10-second"},
		{"version + state", &rpc.ListRolloutRulesReq{Version: 2, State: model.StateOn, Ps: 100}, "a-v2-p05"},
		{"三者互斥（空集）",
			&rpc.ListRolloutRulesReq{CfgKey: "rollout.a", Scope: model.ScopeAndroid, Version: 2, Ps: 100}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := e.listRules(t, tc.req)
			got := wantOK(t, reply, err, tc.name)
			if s := strings.Join(ruleNames(got.GetItems()), ","); s != tc.want {
				t.Fatalf("%s：结果不符\n  got =%s\n want=%s", tc.name, s, tc.want)
			}
			// total 与被筛出的条数一致：期望串就是唯一事实源，不再抄一遍数字。
			wantTotal := int64(0)
			if tc.want != "" {
				wantTotal = int64(len(strings.Split(tc.want, ",")))
			}
			if got.GetTotal() != wantTotal {
				t.Fatalf("%s：total=%d 与本筛选下的条数不符（want=%q）", tc.name, got.GetTotal(), tc.want)
			}
			requireRuleOrderFollowsTheRealOrderBy(t, got.GetItems(), tc.name)
		})
	}

	// 空集与越界页在 total 上必须可区分（上面 version=99 已钉 total=0）。
	// 「没有 mode 守卫」钉成事实：同 config 下换一种 mode 也照样一起回。
	e.seedRule(t, &model.RolloutRule{
		ConfigID: book.itemA.ConfigID, Version: 1, Name: "a-v1-pct", Mode: model.ModePercentage,
		Percentage: 20, Priority: 11, State: model.StateOn,
	})
	mixed, err := e.listRules(t, &rpc.ListRolloutRulesReq{CfgKey: "rollout.a", Version: 1, Ps: 100})
	rows := wantOK(t, mixed, err, "两种 mode 混列")
	if strings.Join(ruleNames(rows.GetItems()), ",") != "a-v1-p10-first,a-v1-p10-second,a-v1-pct,a-v1-p100" {
		t.Fatalf("mode 不该参与本接口的筛选：%v", ruleNames(rows.GetItems()))
	}
	var pct *rpc.RolloutRule
	for _, r := range rows.GetItems() {
		if r.GetName() == "a-v1-pct" {
			pct = r
		}
	}
	if pct == nil {
		t.Fatalf("新种下的规则没出现在列表里（上面刚断言过名字序列，这里只能说明 map 取错了）")
	}
	if int32(pct.GetMode()) != model.ModePercentage || pct.GetPercentage() != 20 {
		t.Fatalf("mode/percentage 列没回给后台：%+v", pct)
	}

	// 过期规则仍在后台列表里（见上表 b-v1-window），并且本接口绝不走运行时查询。
	if got := e.call("RolloutRule.ListCandidates"); got != 0 {
		t.Fatalf("后台列表走了运行时候选查询：%d 次", got)
	}
	expiredRow := e.ruleRow(expired.RuleID)
	if expiredRow.EndAt != 2 || expiredRow.StartAt != 1 {
		t.Fatalf("种子失效：这条规则本该已经过窗：%+v", expiredRow)
	}
}

// 键不存在 → fail-stop（与版本历史的 fail-soft 相反）。
// 这一条的价值全在「两件事能被区分」：拼错键与「这个键确实没有规则」不能都回空集。
func TestListRolloutRulesUnknownCfgKeyFailsStop(t *testing.T) {
	e := newTestEnv(t)
	e.seedRuleBook(t)
	before := e.effects()

	// 键不存在：整页被拒，而不是回一份「看起来干净」的空列表。
	ruleCalls := e.call("RolloutRule.List")
	reply, err := e.listRules(t, &rpc.ListRolloutRulesReq{CfgKey: "rollout.typo", Ps: 100})
	wantFail(t, err, model.ErrConfigNotFound, "cfg_key 不存在")
	if reply != nil {
		t.Fatalf("键不存在却回了响应体：%+v", reply)
	}
	if !strings.Contains(err.Error(), "rollout.typo") || !strings.Contains(err.Error(), model.ScopeGlobal) {
		t.Fatalf("错误里没有承载定位信息（排查时猜不出是哪个键）：%v", err)
	}
	// 关键：拒绝前一次规则表都不该查——否则「空集」与「整页被拒」在库里留下同样的痕迹。
	if e.call("RolloutRule.List") != ruleCalls {
		t.Fatalf("键不存在还去查了规则表（筛选条件被偷偷放宽了）")
	}
	// 键存在但那个版本没有规则：这是合法的空集，不是错误。
	empty, err := e.listRules(t, &rpc.ListRolloutRulesReq{CfgKey: "rollout.b", Version: 77, Ps: 100})
	none := wantOK(t, empty, err, "键存在但该版本无规则")
	if len(none.GetItems()) != 0 || none.GetTotal() != 0 {
		t.Fatalf("合法空集口径不符：%v total=%d", ruleNames(none.GetItems()), none.GetTotal())
	}
	// 空白键不过滤：scope 也不会被解析（第 46 行的判空在 scope 校验之前）。
	if _, err := e.listRules(t, &rpc.ListRolloutRulesReq{CfgKey: "  ", Scope: "ANDROID", Ps: 100}); err != nil {
		t.Fatalf("空白 cfg_key 时 scope 不该参与校验：%v", err)
	}
	e.requireSameEffects(t, before, "键不存在/合法空集零副作用")
}

// 现状哨兵：本接口回裸 total，不过 lim.totalOf。
// 因此 Query.CountTotal=false 对 ListRolloutRules 不生效（与四个同类分页读不同），
// 登记在 README「已知缺口」。修它的人应当让这里变红，而不是把断言改成「两种都行」。
func TestListRolloutRulesIgnoresTheCountTotalSwitch(t *testing.T) {
	on := newTestEnv(t)
	off := newTestEnvWith(t, func(c *config.Config) { c.Query.CountTotal = false })
	for _, e := range []*testEnv{on, off} {
		e.seedRuleBook(t)
		// 对照接口也要有数据：total=0 与「开关关掉了 total」是两件事，
		// 空库里的 on/off 相同毫无信息量。
		e.seedSwitches(t)
	}
	r1, err := on.listRules(t, &rpc.ListRolloutRulesReq{Ps: 4})
	first := wantOK(t, r1, err, "CountTotal=true")
	r2, err := off.listRules(t, &rpc.ListRolloutRulesReq{Ps: 4})
	second := wantOK(t, r2, err, "CountTotal=false")
	if len(first.GetItems()) != 4 || len(second.GetItems()) != 4 {
		t.Fatalf("两页都应有 4 条：%d / %d", len(first.GetItems()), len(second.GetItems()))
	}
	if first.GetTotal() != 7 || second.GetTotal() != 7 {
		t.Fatalf("本接口两侧都回裸 total（现状）：on=%d off=%d", first.GetTotal(), second.GetTotal())
	}
	// 同类接口的对照面：同一种配置下 ListClientSwitches 关掉开关就回 0。
	swOn, err := on.listSwitches(t, &rpc.ListClientSwitchesReq{Ps: 4})
	swOnRows := wantOK(t, swOn, err, "对照：开关列表 CountTotal=true")
	swOff, err := off.listSwitches(t, &rpc.ListClientSwitchesReq{Ps: 4})
	swOffRows := wantOK(t, swOff, err, "对照：开关列表 CountTotal=false")
	if swOnRows.GetTotal() == 0 || swOffRows.GetTotal() != 0 {
		t.Fatalf("对照面失效：开关列表的 total 开关不再生效（on=%d off=%d），"+
			"本用例登记的口径差异需要重新确认", swOnRows.GetTotal(), swOffRows.GetTotal())
	}
}

// 存储形态还原 + 读侧对脏数据的容忍度。
func TestListRolloutRulesRestoresStoredShapesIntoArrays(t *testing.T) {
	e := newTestEnv(t)
	item := e.seedItem(t, "rollout.shape", model.ScopeGlobal, model.ValueTypeString, model.StateOn)
	e.seedRule(t, &model.RolloutRule{
		ConfigID: item.ConfigID, Version: 1, Name: "shape-whitelist", Mode: model.ModeWhitelist,
		Platforms: ",1,4,", MidSuffixes: "0,7", WhitelistMids: model.IDListString([]int64{11, 22}),
		AppVersionMin: "7.2.0", Priority: 5, State: model.StateOn, OperatorID: 42,
		Remark: "白名单：预计周五收口", StartAt: 100, EndAt: 200,
	})
	dirty := e.seedRule(t, &model.RolloutRule{
		ConfigID: item.ConfigID, Version: 1, Name: "shape-dirty", Mode: model.ModePlatform,
		Platforms: ",1,2,", Priority: 6, State: model.StateOn,
	})
	// 手工把 platforms 改坏：加一个第五端 9（本项目没有小程序，AGENTS.md §6）。
	// 写路径不可能产出这种行（NormalizePlatformList 会拒），只能是人为改库。
	e.db.rules[dirty.RuleID].Platforms = ",1,2,9,"
	e.seedRule(t, &model.RolloutRule{
		ConfigID: item.ConfigID, Version: 1, Name: "shape-none", Mode: model.ModeFull,
		Priority: 7, State: model.StateOff,
	})

	reply, err := e.listRules(t, &rpc.ListRolloutRulesReq{CfgKey: "rollout.shape", Ps: 100})
	rows := wantOK(t, reply, err, "形态还原")
	if strings.Join(ruleNames(rows.GetItems()), ",") != "shape-whitelist,shape-dirty,shape-none" {
		t.Fatalf("次序不符：%v", ruleNames(rows.GetItems()))
	}
	byName := map[string]*rpc.RolloutRule{}
	for _, r := range rows.GetItems() {
		byName[r.GetName()] = r
	}
	// 三种存储形态各还原成契约类型：数组、数组、字符串（mid 尾号契约里就是串）。
	wl := byName["shape-whitelist"]
	if len(wl.GetPlatforms()) != 2 || int32(wl.GetPlatforms()[0]) != model.PlatformAndroid ||
		int32(wl.GetPlatforms()[1]) != model.PlatformDesktop {
		t.Fatalf("platforms 没从 \",1,4,\" 还原成数组：%v", wl.GetPlatforms())
	}
	if wl.GetMidSuffixes() != "0,7" {
		t.Fatalf("mid_suffixes 不该被改写：%q", wl.GetMidSuffixes())
	}
	if got := wl.GetWhitelistMids(); len(got) != 2 || got[0] != 11 || got[1] != 22 {
		t.Fatalf("whitelist_mids 没从 \",11,22,\" 还原：%v", got)
	}
	if wl.GetAppVersionMin() != "7.2.0" || wl.GetStartAt() != 100 || wl.GetEndAt() != 200 ||
		wl.GetRemark() != "白名单：预计周五收口" {
		t.Fatalf("规则的时间窗与备注丢了：%+v", wl)
	}
	// 脏端标识在还原阶段被丢弃而不是报错（ops_rollout_rule.go:73-85 的注释）：
	// 现状哨兵——读路径宁可少回一个端，也不让一条历史脏数据把整页变成 500。
	// 若将来改成报错（与写侧口径统一），本用例必须变红后改写。
	d := byName["shape-dirty"]
	if got := d.GetPlatforms(); len(got) != 2 || int32(got[0]) != model.PlatformAndroid ||
		int32(got[1]) != model.PlatformIOS {
		t.Fatalf("脏 platforms \",1,2,9,\" 的还原结果不符：%v（应丢掉第五端 9）", got)
	}
	// 不限端 / 无维度的规则回空数组而不是 [0]：一个 0 元素会被调用方当成合法端。
	none := byName["shape-none"]
	if len(none.GetPlatforms()) != 0 || len(none.GetWhitelistMids()) != 0 ||
		none.GetMidSuffixes() != "" || none.GetPercentage() != 0 {
		t.Fatalf("空维度回成了非空占位：%+v", none)
	}
	// 逐字段比对：投影少搬一列在这里红，而不是靠字面量。
	for _, r := range rows.GetItems() {
		requireRuleRowMatches(t, r, e.ruleRow(r.GetRuleId()), "行比对 "+r.GetName())
	}
	// 读侧只是「少回一个端」，绝不顺手把库里的脏值改成它认为干净的样子：
	// 一旦列表接口变成写接口，本行立刻红。
	if got := e.ruleRow(dirty.RuleID).Platforms; got != ",1,2,9," {
		t.Fatalf("只读接口改写了 ops_rollout_rule.platforms：%q", got)
	}
	if rows.GetTotal() != 3 {
		t.Fatalf("total 不符：%d", rows.GetTotal())
	}
}

func TestListRolloutRulesPropagatesStorageErrorsVerbatim(t *testing.T) {
	e := newTestEnv(t)
	e.seedRuleBook(t)
	before := e.effects()

	// 第一类：规则表读失败必须原样上抛，不能退成空列表 + total=0。
	// 那在后台看起来就是「所有灰度都收口了」——最危险的一类伪装。
	e.db.readErrs["RolloutRule.List"] = errRuleStoreDown
	reply, err := e.listRules(t, &rpc.ListRolloutRulesReq{Ps: 100})
	wantFail(t, err, errRuleStoreDown, "RolloutRule.List 故障")
	if reply != nil {
		t.Fatalf("依赖故障却回了响应体：%+v", reply)
	}
	if errors.Is(err, model.ErrRuleNotFound) {
		t.Fatalf("依赖错误被改写成了业务语义（规则不存在）：%v", err)
	}
	if !strings.Contains(err.Error(), "RolloutRule.List") {
		t.Fatalf("错误链里丢了出错的表/方法定位：%v", err)
	}
	delete(e.db.readErrs, "RolloutRule.List")

	// 同一句 findOne 故障绝不能走成「键不存在」或「不筛 config」：
	// 后者更糟——筛子被放宽成全表，回一份别人的规则。
	e.db.readErrs["ConfigItem.FindOne"] = errRuleStoreDown
	findOneCalls, listCalls := e.call("ConfigItem.FindOne"), e.call("RolloutRule.List")
	bad, err := e.listRules(t, &rpc.ListRolloutRulesReq{CfgKey: "rollout.a", Ps: 100})
	wantFail(t, err, errRuleStoreDown, "ConfigItem.FindOne 故障")
	if bad != nil {
		t.Fatalf("解析 cfg_key 失败却回了响应体：%+v", bad)
	}
	if errors.Is(err, model.ErrConfigNotFound) {
		t.Fatalf("依赖错误被改写成了「配置项不存在」：%v", err)
	}
	if e.call("ConfigItem.FindOne") != findOneCalls+1 {
		t.Fatalf("定位配置项的次数不符：%d → %d", findOneCalls, e.call("ConfigItem.FindOne"))
	}
	if e.call("RolloutRule.List") != listCalls {
		t.Fatalf("解析配置项失败还去查了规则（筛选条件被放宽）：%d → %d", listCalls, e.call("RolloutRule.List"))
	}
	delete(e.db.readErrs, "ConfigItem.FindOne")

	ok, err := e.listRules(t, &rpc.ListRolloutRulesReq{Ps: 100})
	back := wantOK(t, ok, err, "故障恢复后")
	if len(back.GetItems()) != 7 {
		t.Fatalf("恢复后读不到规则：%v", ruleNames(back.GetItems()))
	}
	e.requireSameEffects(t, before, "ListRolloutRules 只读零副作用")
}

// 跨接口不变式：后台列出的规则集，必须与 ResolveConfig 在同一点的取用集合同源同序。
// 两侧不一致的表现形式是「后台说开着，端上没命中」——差在谓词而不是差在数据。
func TestListRolloutRulesAndResolveCandidatesShareTheSameOrdering(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "rollout.live", "v1", 0, "rl-p1")
	item := e.itemByName("rollout.live")
	first := e.seedRule(t, &model.RolloutRule{
		ConfigID: item.ConfigID, Version: 1, Name: "live-p10-a", Mode: model.ModeFull,
		Priority: 10, State: model.StateOn,
	})
	second := e.seedRule(t, &model.RolloutRule{
		ConfigID: item.ConfigID, Version: 1, Name: "live-p10-b", Mode: model.ModeFull,
		Priority: 10, State: model.StateOn,
	})
	third := e.seedRule(t, &model.RolloutRule{
		ConfigID: item.ConfigID, Version: 1, Name: "live-p20", Mode: model.ModeFull,
		Priority: 20, State: model.StateOff, // 停用：只出现在后台列表，不进运行时候选
	})

	listed, err := e.listRules(t, &rpc.ListRolloutRulesReq{CfgKey: "rollout.live", Ps: 100})
	rows := wantOK(t, listed, err, "后台列表")
	if strings.Join(ruleNames(rows.GetItems()), ",") != "live-p10-a,live-p10-b,live-p20" {
		t.Fatalf("后台列表次序不符：%v", ruleNames(rows.GetItems()))
	}
	// 同 priority 的两条按 rule_id 兜底，与运行时 ORDER BY priority, rule_id 同序。
	if first.RuleID >= second.RuleID || second.RuleID >= third.RuleID {
		t.Fatalf("种子失效：三条规则的 rule_id 顺序与期望相反：%d/%d/%d",
			first.RuleID, second.RuleID, third.RuleID)
	}
	// 后台列的是全部（含停用），运行时取的是「启用且在窗口内」的子集：
	// 这一条差异是刻意的，必须写清，否则有人会去把其中一侧「统一」掉。
	candidates, err := e.svc.Models.RolloutRule.ListCandidates(bg(), item.ConfigID, model.NowUnix(), 0)
	want := wantOK(t, candidates, err, "运行时候选")
	got := make([]string, 0, len(want))
	for _, r := range want {
		got = append(got, r.Name)
	}
	if strings.Join(got, ",") != "live-p10-a,live-p10-b" {
		t.Fatalf("运行时候选与后台列出的子集不同源：%v（停用的 live-p20 不该在）", got)
	}
	// 同序：候选集必须是后台列表的一个「保序子序列」，否则两侧对同一次放量的说法就会打架。
	requireRuleNamesAreASubsequenceOf(t, got, ruleNames(rows.GetItems()), "运行时候选 vs 后台列表")
	if e.call("RolloutRule.ListCandidates") != 1 {
		t.Fatalf("本用例不该多次走运行时查询：%d", e.call("RolloutRule.ListCandidates"))
	}
	// 后台列表接口自身绝不查候选（上面那一次是本用例直连 model 造成的基线）。
	again, err := e.listRules(t, &rpc.ListRolloutRulesReq{CfgKey: "rollout.live", Ps: 100})
	second2 := wantOK(t, again, err, "第二次后台列表")
	if second2.GetTotal() != 3 {
		t.Fatalf("total 与条数不符：%d", second2.GetTotal())
	}
	if got := e.call("RolloutRule.ListCandidates"); got != 1 {
		t.Fatalf("后台列表接口调到了运行时候选查询：%d 次", got)
	}
}

// requireRuleNamesAreASubsequenceOf 断言 a 保序地出现在 b 中。
func requireRuleNamesAreASubsequenceOf(t *testing.T, a, b []string, label string) {
	t.Helper()
	i := 0
	for _, name := range b {
		if i < len(a) && a[i] == name {
			i++
		}
	}
	if i != len(a) {
		t.Fatalf("%s：%v 不是 %v 的保序子序列", label, a, b)
	}
}
