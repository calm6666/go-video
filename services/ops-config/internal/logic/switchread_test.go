// 本文件钉住 ListClientSwitches（后台分页列客户端开关），三条真正的风险：
//
//  1. 这是**后台定义列表**，不是运行时读取路径：端上取「本端生效开关」必须走
//     ClientSwitch.ListForPlatform + Available(app_version)（listclientswitcheslogic.go:28-32）。
//     两条路一旦混用，就会出现「后台列出来的开关端上没生效」这类无法解释的投诉，
//     所以本文件把「列表接口绝不做版本判定」钉成断言。
//  2. 排序事实源只有 model 里那一句 ORDER BY：ops_client_switch.go:292
//     是 `ORDER BY switch_key ASC, platform ASC LIMIT ? OFFSET ?`（没有第三级 tiebreaker，
//     但 uniq_key_platform 保证 (switch_key, platform) 唯一，因此仍然确定）。
//     本文件的次序期望全部由这一句推导，翻页拼接则与全量逐条比对，不抄第二份字面量。
//  3. total / 空页 / 越界页是三件事，必须分开钉：把「越界页」写成 total=0，
//     后台就会在翻页时以为数据被删空了。
//
// 与同目录 ListTopics / ListSlots 的口径差异也在本文件登记：
// 本接口**没有**关键词长度守卫（checkKeywordLen 只作用于专题与配置项列表）。
package logic

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/ops-config/internal/config"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"
)

var errSwitchStoreDown = errors.New("read ops_client_switch: connection reset by peer")

func (e *testEnv) listSwitches(t *testing.T, req *rpc.ListClientSwitchesReq) (*rpc.ListClientSwitchesReply, error) {
	t.Helper()
	return NewListClientSwitchesLogic(bg(), e.svc).ListClientSwitches(req)
}

// switchLabels 把投影行压成「键/端」可比对序列：排序与筛选的期望全部由它派生，
// 这样「取错列」（回成 switch_id 或 platform 单独一列）会立刻显形。
func switchLabels(items []*rpc.ClientSwitch) []string {
	out := make([]string, 0, len(items))
	for _, s := range items {
		out = append(out, fmt.Sprintf("%s/%d", s.GetSwitchKey(), int32(s.GetPlatform())))
	}
	return out
}

// requireSwitchRowMatches 逐字段比对投影与库里的行（读回的行是唯一事实源）：
// switchList 少搬一列，这里就红，而不是靠用例里的一堆字面量。
func requireSwitchRowMatches(t *testing.T, got *rpc.ClientSwitch, row *model.ClientSwitch, label string) {
	t.Helper()
	if got.GetSwitchId() != row.SwitchID || got.GetSwitchKey() != row.SwitchKey ||
		int32(got.GetPlatform()) != row.Platform || got.GetMinVersion() != row.MinVersion ||
		got.GetMaxVersion() != row.MaxVersion || got.GetEnabled() != row.Enabled ||
		got.GetConfigId() != row.ConfigID || got.GetOperatorId() != row.OperatorID ||
		got.GetRemark() != row.Remark || got.GetVersion() != row.Version ||
		got.GetCtime() != row.Ctime || got.GetMtime() != row.Mtime {
		t.Fatalf("%s：投影与 ops_client_switch 行不一致\n  got=%+v\n row=%+v", label, got, row)
	}
}

// seedSwitches 布一批「插入顺序既不按 key 也不按端」的种子：
// 少了这种打乱，没有确定性 ORDER BY 的实现也能靠 map 顺序碰对期望。
// 返回的 map 按 switch_key/platform 索引，供逐字段比对使用。
func (e *testEnv) seedSwitches(t *testing.T) map[string]*model.ClientSwitch {
	t.Helper()
	type spec struct {
		key      string
		platform int32
		enabled  int32
		minVer   string
		maxVer   string
		label    string
		remark   string
	}
	specs := []spec{
		// 同一个 key 三个端：platform ASC 的 tiebreaker 只有这一组能证。
		{key: "alpha.one", platform: model.PlatformDesktop, enabled: model.StateOn, label: "alpha.one/4", remark: "桌面"},
		{key: "alpha.one", platform: model.PlatformIOS, enabled: model.StateOff, label: "alpha.one/2", remark: "iOS 收口"},
		{key: "zeta.nine", platform: model.PlatformAndroid, enabled: model.StateOn, label: "zeta.nine/1", remark: "字典序最后"},
		{key: "alpha.one", platform: model.PlatformAndroid, enabled: model.StateOn, minVer: "7.2.0",
			label: "alpha.one/1", remark: "安卓看版本"},
		{key: "beta.two", platform: model.PlatformIOS, enabled: model.StateOn, label: "beta.two/2", remark: "绑定配置"},
		{key: "alpha.two", platform: model.PlatformHarmony, enabled: model.StateOff, maxVer: "6.0.0",
			label: "alpha.two/3", remark: "区间上界"},
	}
	rows := map[string]*model.ClientSwitch{}
	for i := range specs {
		s := specs[i]
		row := e.seedSwitch(t, &model.ClientSwitch{
			SwitchKey: s.key, Platform: s.platform, Enabled: s.enabled,
			MinVersion: s.minVer, MaxVersion: s.maxVer,
			OperatorID: 42, Remark: s.remark,
		})
		if s.key == "beta.two" {
			// config_id 只是引用：本服务不因它去 ops_config_item 回填 cfg_key。
			bound := e.seedItem(t, "home.switch.bind", model.ScopeGlobal, model.ValueTypeBool, model.StateOn)
			e.db.switches[row.SwitchID].ConfigID = bound.ConfigID
			row = e.switchRow(row.SwitchID)
		}
		rows[s.label] = row
	}
	return rows
}

func TestListClientSwitchesRejectsIllegalInputsBeforeTouchingStorage(t *testing.T) {
	e := newTestEnv(t)
	e.seedSwitches(t)
	before := e.effects()

	cases := []struct {
		name string
		req  *rpc.ListClientSwitchesReq
		want error
	}{
		// 分页守卫排在最前（listclientswitcheslogic.go:34-38）：负数是入参错误，
		// 不是「回到第一页」——放过去调用方会以为翻页成功。
		{"pn 为负", &rpc.ListClientSwitchesReq{Pn: -1}, model.ErrInvalidPage},
		{"ps 为负", &rpc.ListClientSwitchesReq{Ps: -1}, model.ErrInvalidPage},
		{"pn 与 ps 同时为负", &rpc.ListClientSwitchesReq{Pn: -2, Ps: -2}, model.ErrInvalidPage},
		// platform=0 才是「不过滤」；第五端必须报错而不是当成「不限端」：
		// 本项目没有小程序（AGENTS.md §6），放行等于把不存在的端推给四端。
		{"platform 是第五端", &rpc.ListClientSwitchesReq{Platform: rpc.ClientPlatform(5)}, model.ErrPlatformUnknown},
		{"platform 越界", &rpc.ListClientSwitchesReq{Platform: rpc.ClientPlatform(9)}, model.ErrPlatformUnknown},
		{"platform 为负", &rpc.ListClientSwitchesReq{Platform: rpc.ClientPlatform(-1)}, model.ErrPlatformUnknown},
		// enabled 只有 0（不过滤）/1/2 三种合法取值。
		{"enabled 第四个值", &rpc.ListClientSwitchesReq{Enabled: 3}, model.ErrRuleStateInvalid},
		{"enabled 为负", &rpc.ListClientSwitchesReq{Enabled: -2}, model.ErrRuleStateInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := e.listSwitches(t, tc.req)
			wantFail(t, err, tc.want, tc.name)
			if reply != nil {
				t.Fatalf("%s：出错还回了响应体：%+v", tc.name, reply)
			}
			// 守卫先于查库：被拒的请求一次 COUNT 都不该发出。
			if got := e.call("ClientSwitch.List"); got != 0 {
				t.Fatalf("%s：入参校验阶段就查了库：%d 次", tc.name, got)
			}
		})
	}
	// 合法的「不过滤」与合法筛选值都必须放行，否则上面那段拒绝就是误伤。
	for _, req := range []*rpc.ListClientSwitchesReq{{}, {Platform: rpc.ClientPlatform_CLIENT_PLATFORM_UNSPECIFIED},
		{Enabled: model.StateOn}, {Enabled: model.StateOff}, {Pn: 1, Ps: 1}} {
		if _, err := e.listSwitches(t, req); err != nil {
			t.Fatalf("合法请求 %+v 被拒：%v", req, err)
		}
	}
	e.requireSameEffects(t, before, "非法 ListClientSwitches 入参零副作用")
}

// 现状哨兵：本接口**没有**关键词长度守卫。
// checkKeywordLen（helpers.go:564-571，超 64 rune 回 ErrBatchTooLarge）只被
// listtopicslogic / listconfigslogic 调用；listclientswitcheslogic.go:53 把
// strings.TrimSpace(switch_key) 直接交给 model 的 LIKE，长度不设限。
// 判它是「现状」而不是「缺陷」的理由：LIKE 是参数化的、且 keyword 不参与索引扫描之外的
// 开销，最坏是一次空手全表扫（ops_client_switch 是百行量级的表）。
// 将来若给本接口补上同一道守卫，本用例必须以「超长被拒」的形式变红后改写，
// 不许反过来把断言放宽。
func TestListClientSwitchesHasNoKeywordLengthGuard(t *testing.T) {
	e := newTestEnv(t)
	e.seedSwitches(t)
	before := e.effects()

	long := strings.Repeat("x", 300)
	// 同长度的关键词在专题列表里是被拒的（topicread_test.go 的「关键词超长」）：
	// 两侧对照，才能看出本接口确实少一道守卫，而不是用例写漏了。
	if _, err := e.listTopics(t, &rpc.ListTopicsReq{Keyword: strings.Repeat("词", 65)}); !errors.Is(err, model.ErrBatchTooLarge) {
		t.Fatalf("对照面失效：ListTopics 的关键词守卫不再生效（%v），本用例的前提需要重新确认", err)
	}

	calls := e.call("ClientSwitch.List")
	reply, err := e.listSwitches(t, &rpc.ListClientSwitchesReq{SwitchKey: long})
	got := wantOK(t, reply, err, "300 字符的开关键")
	if len(got.GetItems()) != 0 || got.GetTotal() != 0 {
		t.Fatalf("超长关键词不该匹配到任何行：%d 条 total=%d", len(got.GetItems()), got.GetTotal())
	}
	if e.call("ClientSwitch.List") != calls+1 {
		t.Fatalf("超长关键词没真的进 LIKE（%d → %d），那就说明有人在 logic 侧偷偷加了长度分支",
			calls, e.call("ClientSwitch.List"))
	}
	e.requireSameEffects(t, before, "超长开关键零副作用")
}

// 排序事实源：ops_client_switch.go:292 `ORDER BY switch_key ASC, platform ASC`。
// 期望序列由这一句推出：同 key 的多端按端号升序、不同 key 按字典序。
// 「取错列」（按 platform 主序）与「排序方向反了」都会让下面第一句立刻红。
func TestListClientSwitchesOrdersBySwitchKeyThenPlatform(t *testing.T) {
	e := newTestEnv(t)
	rows := e.seedSwitches(t)

	all, err := e.listSwitches(t, &rpc.ListClientSwitchesReq{Ps: 100})
	got := wantOK(t, all, err, "全量一页")
	want := []string{"alpha.one/1", "alpha.one/2", "alpha.one/4", "alpha.two/3", "beta.two/2", "zeta.nine/1"}
	if strings.Join(switchLabels(got.GetItems()), ",") != strings.Join(want, ",") {
		t.Fatalf("排序不是 switch_key ASC, platform ASC：%v（期望 %v）", switchLabels(got.GetItems()), want)
	}
	if got.GetTotal() != 6 {
		t.Fatalf("total 应是筛选后全量条数：%d", got.GetTotal())
	}
	// 逐字段比对回读的行：投影少搬一列在这里红，而不是在某个字面量断言里。
	for i, label := range want {
		requireSwitchRowMatches(t, got.GetItems()[i], rows[label], "行比对 "+label)
	}

	// 翻页：两页拼起来必须与上面那份全量逐条同序（次序已钉过，这里只比对拼接结果）。
	p1, err := e.listSwitches(t, &rpc.ListClientSwitchesReq{Pn: 1, Ps: 4})
	first := wantOK(t, p1, err, "第一页")
	p2, err := e.listSwitches(t, &rpc.ListClientSwitchesReq{Pn: 2, Ps: 4})
	second := wantOK(t, p2, err, "第二页")
	if len(first.GetItems()) != 4 || len(second.GetItems()) != 2 {
		t.Fatalf("页大小没被执行：%d / %d", len(first.GetItems()), len(second.GetItems()))
	}
	joined := append(switchLabels(first.GetItems()), switchLabels(second.GetItems())...)
	if strings.Join(joined, ",") != strings.Join(switchLabels(got.GetItems()), ",") {
		t.Fatalf("翻页拼接与全量不一致：\n  got =%v\n want=%v", joined, switchLabels(got.GetItems()))
	}
	// 翻页时 total 不漂移（否则后台的「共 N 条」会随页码变化）。
	if first.GetTotal() != 6 || second.GetTotal() != 6 {
		t.Fatalf("翻页时 total 漂移：%d / %d", first.GetTotal(), second.GetTotal())
	}
}

// total、空页、越界页三件事分开钉，另加 Query.CountTotal 开关。
// 本接口没有游标与 hasMore 字段（rpc.ListClientSwitchesReply 只有 items + total），
// 「编码游标对 0 值静默回空串从而把 hasMore 降级成 false」这类缺陷在这里不存在，
// 因此钉成「没有」：越界页只能是空 items + 非零 total，别的组合都是编造。
func TestListClientSwitchesPagesTotalsAndEmptyPage(t *testing.T) {
	e := newTestEnv(t)
	e.seedSwitches(t)

	// 越界页：合法的空结果 + 保留 total，而不是错误。
	far, err := e.listSwitches(t, &rpc.ListClientSwitchesReq{Pn: 9, Ps: 4})
	out := wantOK(t, far, err, "越界页")
	if len(out.GetItems()) != 0 {
		t.Fatalf("越界页回了条目：%v", switchLabels(out.GetItems()))
	}
	if out.GetTotal() != 6 {
		t.Fatalf("越界页的 total 必须仍是筛选后全量（否则后台以为数据被删空）：%d", out.GetTotal())
	}

	// 空页（筛选条件打空）：total 也应是 0，与「越界页」可区分。
	none, err := e.listSwitches(t, &rpc.ListClientSwitchesReq{SwitchKey: "不存在的开关键"})
	empty := wantOK(t, none, err, "查不到的关键词")
	if len(empty.GetItems()) != 0 || empty.GetTotal() != 0 {
		t.Fatalf("查不到应回空集 + total=0：%v %d", switchLabels(empty.GetItems()), empty.GetTotal())
	}

	// ps 越界夹到 Query.MaxPageSize(100)、ps=0 回落到上限、pn=0 归一到第 1 页：
	// 三条都不该改变本页条数（全量只有 6 条）。
	for _, tc := range []struct {
		name string
		req  *rpc.ListClientSwitchesReq
	}{
		{"ps 越界夹到上限", &rpc.ListClientSwitchesReq{Ps: 900}},
		{"缺省分页", &rpc.ListClientSwitchesReq{}},
		{"pn=0 归一到第 1 页", &rpc.ListClientSwitchesReq{Pn: 0, Ps: 100}},
	} {
		reply, err := e.listSwitches(t, tc.req)
		got := wantOK(t, reply, err, tc.name)
		if len(got.GetItems()) != 6 {
			t.Fatalf("%s：got=%d", tc.name, len(got.GetItems()))
		}
	}
	// ps=1 时第一页只有一条，total 仍是全量：夹取与总数互不影响。
	one, err := e.listSwitches(t, &rpc.ListClientSwitchesReq{Ps: 1})
	single := wantOK(t, one, err, "ps=1")
	if len(single.GetItems()) != 1 || single.GetTotal() != 6 {
		t.Fatalf("ps=1：%d 条 total=%d", len(single.GetItems()), single.GetTotal())
	}

	// Query.CountTotal=false → total 回 0（helpers.go:81-88 的口径：本接口不提供总数），
	// 但列表内容不受影响。
	off := newTestEnvWith(t, func(c *config.Config) { c.Query.CountTotal = false })
	off.seedSwitches(t)
	hidden, err := off.listSwitches(t, &rpc.ListClientSwitchesReq{Ps: 4})
	got := wantOK(t, hidden, err, "CountTotal=false")
	if got.GetTotal() != 0 {
		t.Fatalf("关掉总数开关后 total 必须是 0，实得 %d", got.GetTotal())
	}
	if len(got.GetItems()) != 4 {
		t.Fatalf("关掉 total 连带砍了列表：%d", len(got.GetItems()))
	}
}

// 每一段 WHERE 都配一条唯一种子，让「漏过滤条件」可判别。
// 另外钉两处「没有的守卫/没有的能力」：
//   - model 侧 ClientSwitchFilter.ConfigID（ops_client_switch.go:276）在本接口不可达：
//     rpc.ListClientSwitchesReq 没有 config_id 字段，logic 也不填（第 51-57 行）。
//   - 本接口不做 Available(app_version) 判定：请求里没有 app_version，
//     区间把全世界都挡在外面的开关照样出现在后台列表里。
func TestListClientSwitchesFiltersEachWhereSegment(t *testing.T) {
	e := newTestEnv(t)
	e.seedSwitches(t)

	for _, tc := range []struct {
		name string
		req  *rpc.ListClientSwitchesReq
		want string
	}{
		// platform = ?：每个端一条唯一种子。
		{"安卓", &rpc.ListClientSwitchesReq{Platform: rpc.ClientPlatform(model.PlatformAndroid), Ps: 100},
			"alpha.one/1,zeta.nine/1"},
		{"iOS", &rpc.ListClientSwitchesReq{Platform: rpc.ClientPlatform(model.PlatformIOS), Ps: 100},
			"alpha.one/2,beta.two/2"},
		{"鸿蒙", &rpc.ListClientSwitchesReq{Platform: rpc.ClientPlatform(model.PlatformHarmony), Ps: 100},
			"alpha.two/3"},
		{"桌面", &rpc.ListClientSwitchesReq{Platform: rpc.ClientPlatform(model.PlatformDesktop), Ps: 100},
			"alpha.one/4"},
		// enabled = ?：开/关两条互补，合起来是全量（划分而非重叠）。
		{"只看生效", &rpc.ListClientSwitchesReq{Enabled: model.StateOn, Ps: 100},
			"alpha.one/1,alpha.one/4,beta.two/2,zeta.nine/1"},
		{"只看关闭", &rpc.ListClientSwitchesReq{Enabled: model.StateOff, Ps: 100}, "alpha.one/2,alpha.two/3"},
		// switch_key LIKE '%kw%'：中缀匹配，不是前缀等值。
		{"关键词中缀命中", &rpc.ListClientSwitchesReq{SwitchKey: "lpha", Ps: 100},
			"alpha.one/1,alpha.one/2,alpha.one/4,alpha.two/3"},
		{"关键词两端空白被裁掉", &rpc.ListClientSwitchesReq{SwitchKey: "  zeta  ", Ps: 100}, "zeta.nine/1"},
		// 通配符按字面量处理：model 侧 escapeLike（ops_client_switch.go:270），
		// 少了它 "%" 会捞出全表。大小写敏感性交给列的 collation，本用例不钉。
		{"通配符不放大结果", &rpc.ListClientSwitchesReq{SwitchKey: "%", Ps: 100}, ""},
		// 组合条件必须相交，而不是「后一个覆盖前一个」。
		{"端 + 生效", &rpc.ListClientSwitchesReq{Platform: rpc.ClientPlatform(model.PlatformAndroid),
			Enabled: model.StateOn, Ps: 100}, "alpha.one/1,zeta.nine/1"},
		{"端 + 关闭（空集）", &rpc.ListClientSwitchesReq{Platform: rpc.ClientPlatform(model.PlatformDesktop),
			Enabled: model.StateOff, Ps: 100}, ""},
		{"key + 端（唯一键定位）", &rpc.ListClientSwitchesReq{SwitchKey: "alpha.one",
			Platform: rpc.ClientPlatform(model.PlatformIOS), Ps: 100}, "alpha.one/2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := e.listSwitches(t, tc.req)
			got := wantOK(t, reply, err, tc.name)
			if s := strings.Join(switchLabels(got.GetItems()), ","); s != tc.want {
				t.Fatalf("%s：结果不符\n  got =%v\n want=%s", tc.name, switchLabels(got.GetItems()), tc.want)
			}
			if got.GetTotal() != int64(len(got.GetItems())) {
				t.Fatalf("%s：total 与本页集不一致：%d vs %d", tc.name, got.GetTotal(), len(got.GetItems()))
			}
		})
	}

	// 区间把任何版本都挡在外面的开关仍出现在后台列表：本接口不是运行时读取路径。
	blocked, err := e.listSwitches(t, &rpc.ListClientSwitchesReq{Ps: 100})
	rows := wantOK(t, blocked, err, "含版本区间行")
	var maxOnly, minOnly *rpc.ClientSwitch
	for _, row := range rows.GetItems() {
		switch row.GetSwitchKey() + "/" + fmt.Sprint(int32(row.GetPlatform())) {
		case "alpha.two/3":
			maxOnly = row
		case "alpha.one/1":
			minOnly = row
		}
	}
	if maxOnly.GetMaxVersion() != "6.0.0" || minOnly.GetMinVersion() != "7.2.0" {
		t.Fatalf("版本区间列没回给后台：%+v / %+v", maxOnly, minOnly)
	}
	// config_id 只回引用（库里绑了配置项的那条），契约里没有承载 cfg_key 的字段。
	var bound *rpc.ClientSwitch
	for _, row := range rows.GetItems() {
		if row.GetSwitchKey() == "beta.two" {
			bound = row
		}
	}
	if bound == nil || bound.GetConfigId() == 0 {
		t.Fatalf("beta.two 的 config_id 引用丢了：%+v", bound)
	}
	// 「没有的能力」钉成「没有」：按 config_id 筛不进本接口的 WHERE，
	// 所以带着绑定的那一行仍会与其它行一起出现（上一条已证），
	// 并且本用例全程只发了 1 次查询/行——没有为 config_id 追加回查。
	if got := e.call("ConfigItem.FindOne"); got != 0 {
		t.Fatalf("ListClientSwitches 为回填 cfg_key 去查了配置项：%d 次", got)
	}
	if got := e.call("ConfigItem.List"); got != 0 {
		t.Fatalf("ListClientSwitches 顺带列了配置项：%d 次", got)
	}
}

// 运行时路径与后台列表必须是两条路：本接口绝不走 ListForPlatform，
// 也不查 ops_config_item（端上取「本端全部生效开关」另有 model 方法）。
func TestListClientSwitchesNeverTakesTheRuntimePath(t *testing.T) {
	e := newTestEnv(t)
	e.seedSwitches(t)
	before := e.effects()

	if _, err := e.listSwitches(t, &rpc.ListClientSwitchesReq{Enabled: model.StateOn, Ps: 100}); err != nil {
		t.Fatalf("列表读取：%v", err)
	}
	if got := e.call("ClientSwitch.ListForPlatform"); got != 0 {
		t.Fatalf("后台列表走了运行时读取路径：%d 次", got)
	}
	if got := e.call("ClientSwitch.List"); got != 1 {
		t.Fatalf("一次列表请求应只发一次查询：%d 次", got)
	}
	// 只读：零写、零缓存、零审计。
	if len(e.cache.sets) != 0 || len(e.cache.gets) != 0 || len(e.cache.dels) != 0 {
		t.Fatalf("ListClientSwitches 动了缓存：sets=%v gets=%v dels=%v", e.cache.sets, e.cache.gets, e.cache.dels)
	}
	if len(e.audit.reqs) != 0 {
		t.Fatalf("只读接口写了审计：%d 条", len(e.audit.reqs))
	}
	e.requireSameEffects(t, before, "ListClientSwitches 只读零副作用")
}

func TestListClientSwitchesPropagatesStorageErrorVerbatim(t *testing.T) {
	e := newTestEnv(t)
	e.seedSwitches(t)
	before := e.effects()

	// 第一类：原始错误上抛。绝不退化成「空列表 + total=0」——
	// 那在后台看起来就是「开关被人删光了」。
	e.db.readErrs["ClientSwitch.List"] = errSwitchStoreDown
	reply, err := e.listSwitches(t, &rpc.ListClientSwitchesReq{Ps: 100})
	wantFail(t, err, errSwitchStoreDown, "ClientSwitch.List 故障")
	if reply != nil {
		t.Fatalf("依赖故障却回了响应体：%+v", reply)
	}
	if errors.Is(err, model.ErrSwitchNotFound) {
		t.Fatalf("依赖错误被改写成了业务语义（开关不存在）：%v", err)
	}
	if !strings.Contains(err.Error(), "ClientSwitch.List") {
		t.Fatalf("错误链里丢了出错的表/方法定位：%v", err)
	}
	delete(e.db.readErrs, "ClientSwitch.List")

	// 恢复后能读到，且整个过程零副作用：本接口没有第二类（不透明哨兵）与
	// 第三类（就地吞掉只写日志）分支——读路径唯一的依赖就是 MySQL。
	ok, err := e.listSwitches(t, &rpc.ListClientSwitchesReq{Ps: 100})
	back := wantOK(t, ok, err, "故障恢复后")
	if len(back.GetItems()) != 6 {
		t.Fatalf("恢复后读不到开关：%v", switchLabels(back.GetItems()))
	}
	e.requireSameEffects(t, before, "读故障不留下任何痕迹")
}
