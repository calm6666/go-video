// 本文件钉住 ListSlots（后台分页列坑位定义），以及一条最容易在改动里丢失的不变式：
//
//	同一份配置，「后台看到的坑位」与「端上 ResolveSlot 拿到的坑位」必须是同一批。
//
// 两侧对 0 的语义刻意不同（列表：不过滤；解析：未声明端只给不限端），
// 这一点必须写清并钉住，否则有人会把其中一侧「统一」成另一侧，
// 结果是要么后台筛不出某端专属位，要么端上把某端专属位推给全端。
package logic

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"go-video/services/ops-config/internal/config"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"
)

var errSlotListDown = errors.New("read ops_recommend_slot: syscall error")

func (e *testEnv) listSlots(t *testing.T, req *rpc.ListSlotsReq) (*rpc.ListSlotsReply, error) {
	t.Helper()
	return NewListSlotsLogic(bg(), e.svc).ListSlots(req)
}

func slotCodes(items []*rpc.RecommendSlot) []string {
	out := make([]string, 0, len(items))
	for _, s := range items {
		out = append(out, s.GetCode())
	}
	return out
}

func TestListSlotsRejectsIllegalInputsBeforeTouchingStorage(t *testing.T) {
	e := newTestEnv(t)
	e.seedSlot(t, &model.RecommendSlot{Code: "ls.one", Page: "home", Title: "一号位",
		Capacity: 3, State: model.StateOn})
	before := e.effects()

	cases := []struct {
		name string
		req  *rpc.ListSlotsReq
		want error
	}{
		{"pn 为负", &rpc.ListSlotsReq{Pn: -1}, model.ErrInvalidPage},
		{"ps 为负", &rpc.ListSlotsReq{Ps: -1}, model.ErrInvalidPage},
		// state=0 是「全部」，第四个值才是错误：把 3 当成「不过滤」会让后台一个条件都没筛上。
		{"state 第四个值", &rpc.ListSlotsReq{State: 3}, model.ErrRuleStateInvalid},
		{"state 越界", &rpc.ListSlotsReq{State: -1}, model.ErrRuleStateInvalid},
		{"platform 越界", &rpc.ListSlotsReq{Platform: rpc.ClientPlatform(9)}, model.ErrPlatformUnknown},
		{"platform 是第五端（小程序不存在）", &rpc.ListSlotsReq{Platform: rpc.ClientPlatform(5)}, model.ErrPlatformUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := e.listSlots(t, tc.req)
			wantFail(t, err, tc.want, tc.name)
			if reply != nil {
				t.Fatalf("%s：出错还回了响应体 %+v", tc.name, reply)
			}
			if got := e.call("Slot.List"); got != 0 {
				t.Fatalf("%s：入参校验阶段就查了坑位表 %d 次", tc.name, got)
			}
		})
	}
	// 合法的「不过滤」取值：0 与 1/2 都不该被拒。
	for _, req := range []*rpc.ListSlotsReq{{}, {State: model.StateOn}, {State: model.StateOff},
		{Platform: rpc.ClientPlatform_CLIENT_PLATFORM_UNSPECIFIED}} {
		if _, err := e.listSlots(t, req); err != nil {
			t.Fatalf("合法分页请求 %+v 被拒：%v", req, err)
		}
	}
	e.requireSameEffects(t, before, "非法 ListSlots 入参零副作用")
}

func TestListSlotsPagesAndSortsByCode(t *testing.T) {
	e := newTestEnv(t)
	// 故意用「插入顺序 ≠ 字典序」的编码：没有确定性 ORDER BY 时翻页会重复/漏行。
	for _, code := range []string{"home.zulu", "mz.alpha", "home.bravo", "detail.mike", "aa.first"} {
		page := "home"
		if strings.HasPrefix(code, "mz.") || strings.HasPrefix(code, "aa.") {
			page = "mine"
		}
		e.seedSlot(t, &model.RecommendSlot{Code: code, Page: page, Title: code,
			Capacity: 3, State: model.StateOn})
	}
	// 一个停用的：state 过滤与 total 口径都要对上。
	e.seedSlot(t, &model.RecommendSlot{Code: "off.kilo", Page: "home", Title: "停投位",
		Capacity: 3, State: model.StateOff})

	all, err := e.listSlots(t, &rpc.ListSlotsReq{Ps: 100})
	got := wantOK(t, all, err, "全量一页")
	wantOrder := []string{"aa.first", "detail.mike", "home.bravo", "home.zulu", "mz.alpha", "off.kilo"}
	if strings.Join(slotCodes(got.GetItems()), ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("排序不是 code ASC：%v", slotCodes(got.GetItems()))
	}
	if got.GetTotal() != 6 {
		t.Fatalf("total 应是全量条数而不是本页条数：%d", got.GetTotal())
	}

	// 翻页：两页拼起来必须等于全量，既不重复也不漏。
	p1, err := e.listSlots(t, &rpc.ListSlotsReq{Pn: 1, Ps: 4})
	first := wantOK(t, p1, err, "第一页")
	p2, err := e.listSlots(t, &rpc.ListSlotsReq{Pn: 2, Ps: 4})
	second := wantOK(t, p2, err, "第二页")
	if len(first.GetItems()) != 4 || len(second.GetItems()) != 2 {
		t.Fatalf("页大小没被执行：%d / %d", len(first.GetItems()), len(second.GetItems()))
	}
	if first.GetTotal() != 6 || second.GetTotal() != 6 {
		t.Fatalf("翻页时 total 漂移了：%d / %d", first.GetTotal(), second.GetTotal())
	}
	joined := append(slotCodes(first.GetItems()), slotCodes(second.GetItems())...)
	if strings.Join(joined, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("翻页拼接与全量不一致：%v", joined)
	}
	// 越界页：合法的空结果，而不是错误——后台翻到底部不该 500。
	empty, err := e.listSlots(t, &rpc.ListSlotsReq{Pn: 9, Ps: 4})
	out := wantOK(t, empty, err, "越界页")
	if len(out.GetItems()) != 0 || out.GetTotal() != 6 {
		t.Fatalf("越界页应回空集但保留 total：%v / %d", slotCodes(out.GetItems()), out.GetTotal())
	}

	// ps 越界「夹到上限」而不是报错（proto 注明上限 100）；pn=0 归一到第 1 页。
	clamped, err := e.listSlots(t, &rpc.ListSlotsReq{Ps: 500})
	big := wantOK(t, clamped, err, "ps 越界")
	if len(big.GetItems()) != 6 {
		t.Fatalf("全量只有 6 条，夹取不应改变本页条数：%d", len(big.GetItems()))
	}
	def, err := e.listSlots(t, &rpc.ListSlotsReq{})
	dflt := wantOK(t, def, err, "缺省分页")
	if len(dflt.GetItems()) != 6 {
		t.Fatalf("缺省 ps 应回落到上限：%d", len(dflt.GetItems()))
	}

	// page 是归属筛选，不是布局指令（AGENTS.md §6：服务端不解释 page）。
	mine, err := e.listSlots(t, &rpc.ListSlotsReq{Page: "mine", Ps: 100})
	part := wantOK(t, mine, err, "按 page 筛")
	if strings.Join(slotCodes(part.GetItems()), ",") != "aa.first,mz.alpha" {
		t.Fatalf("page=mine 结果不符：%v", slotCodes(part.GetItems()))
	}
	if part.GetTotal() != 2 {
		t.Fatalf("total 必须是筛选后的条数：%d", part.GetTotal())
	}
	// 不存在的 page：空集 + total=0，而不是错误。
	none, err := e.listSlots(t, &rpc.ListSlotsReq{Page: "nope"})
	nil0 := wantOK(t, none, err, "不存在的 page")
	if len(nil0.GetItems()) != 0 || nil0.GetTotal() != 0 {
		t.Fatalf("不存在的 page 应回空集：%v %d", slotCodes(nil0.GetItems()), nil0.GetTotal())
	}
}

func TestListSlotsStateAndPlatformFiltersCoverEveryEnd(t *testing.T) {
	e := newTestEnv(t)
	e.seedSlot(t, &model.RecommendSlot{Code: "sp.all", Page: "home", Title: "不限端",
		Capacity: 3, State: model.StateOn})
	e.seedSlot(t, &model.RecommendSlot{Code: "sp.android", Page: "home", Title: "仅安卓",
		Platforms: ",1,", Capacity: 3, State: model.StateOn})
	e.seedSlot(t, &model.RecommendSlot{Code: "sp.two", Page: "home", Title: "安卓+鸿蒙",
		Platforms: ",1,3,", Capacity: 3, State: model.StateOn})
	e.seedSlot(t, &model.RecommendSlot{Code: "sp.ios", Page: "home", Title: "仅 iOS",
		Platforms: ",2,", Capacity: 3, State: model.StateOn})
	e.seedSlot(t, &model.RecommendSlot{Code: "sp.android.off", Page: "home", Title: "停投安卓位",
		Platforms: ",1,", Capacity: 3, State: model.StateOff})

	on, err := e.listSlots(t, &rpc.ListSlotsReq{State: model.StateOn, Ps: 100})
	ons := wantOK(t, on, err, "只看启用")
	if len(ons.GetItems()) != 4 || ons.GetTotal() != 4 {
		t.Fatalf("state=ON 应有 4 条：%v total=%d", slotCodes(ons.GetItems()), ons.GetTotal())
	}
	off, err := e.listSlots(t, &rpc.ListSlotsReq{State: model.StateOff, Ps: 100})
	offs := wantOK(t, off, err, "只看停用")
	if strings.Join(slotCodes(offs.GetItems()), ",") != "sp.android.off" {
		t.Fatalf("state=OFF 结果不符：%v", slotCodes(offs.GetItems()))
	}
	// 两个筛选条件相加 must be 划分：启用 + 停用 = 全量，且不重叠。
	if len(ons.GetItems())+len(offs.GetItems()) != 5 {
		t.Fatalf("state 过滤不是划分：%d + %d", len(ons.GetItems()), len(offs.GetItems()))
	}

	// 端筛选必须同时命中「含该端」与「不限端」——少了后一支，
	// 后台按端筛选会漏掉几乎全部公共位（model 与内存侧同口径）。
	cases := []struct {
		name string
		plat rpc.ClientPlatform
		want string
	}{
		{"安卓", rpc.ClientPlatform(model.PlatformAndroid), "sp.all,sp.android,sp.android.off,sp.two"},
		{"iOS", rpc.ClientPlatform(model.PlatformIOS), "sp.all,sp.ios"},
		{"鸿蒙", rpc.ClientPlatform(model.PlatformHarmony), "sp.all,sp.two"},
		{"桌面", rpc.ClientPlatform(model.PlatformDesktop), "sp.all"},
		// 不限端坑位在「不给端」时也必须出现，否则后台默认视图是空的。
		{"不给端＝不过滤", rpc.ClientPlatform_CLIENT_PLATFORM_UNSPECIFIED,
			"sp.all,sp.android,sp.android.off,sp.ios,sp.two"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := e.listSlots(t, &rpc.ListSlotsReq{Platform: tc.plat, Ps: 100})
			got := wantOK(t, reply, err, tc.name)
			if strings.Join(slotCodes(got.GetItems()), ",") != tc.want {
				t.Fatalf("%s：端筛选不符\n  got =%v\n want=%s", tc.name, slotCodes(got.GetItems()), tc.want)
			}
		})
	}
	// 启用 + 端 组合：后台「这一端能投什么」视图的正确答案。
	andOn, err := e.listSlots(t, &rpc.ListSlotsReq{Platform: rpc.ClientPlatform(model.PlatformAndroid),
		State: model.StateOn, Ps: 100})
	combo := wantOK(t, andOn, err, "安卓且启用")
	if strings.Join(slotCodes(combo.GetItems()), ",") != "sp.all,sp.android,sp.two" {
		t.Fatalf("组合筛选不符：%v", slotCodes(combo.GetItems()))
	}
}

func TestListSlotsOnlyReturnsDefinitions(t *testing.T) {
	e := newTestEnv(t)
	slot := e.seedSlot(t, &model.RecommendSlot{Code: "ls.def", Page: "home", Title: "金刚位",
		Platforms: ",1,2,", Capacity: 4, State: model.StateOn, Version: 9, OperatorID: 42, Remark: "备注"})
	e.seedSlotItems(t, slot.SlotID, 4, []*model.SlotItem{
		{Position: 1, ItemType: model.ItemTypeUGCVideo, ItemID: "big-content", State: model.StateOn},
	})

	reply, err := e.listSlots(t, &rpc.ListSlotsReq{Ps: 100})
	got := wantOK(t, reply, err, "定义列表")
	one := got.GetItems()[0]
	if one.GetSlotId() != slot.SlotID || one.GetCode() != "ls.def" || one.GetCapacity() != 4 ||
		one.GetVersion() != 9 || one.GetOperatorId() != 42 || one.GetRemark() != "备注" {
		t.Fatalf("定义字段丢了：%+v", one)
	}
	// 端列表必须从存储串还原成数组：回 ",1,2," 会把存储形态漏给调用方（AGENTS.md §6）。
	if len(one.GetPlatforms()) != 2 || one.GetPlatforms()[0] != rpc.ClientPlatform(model.PlatformAndroid) ||
		one.GetPlatforms()[1] != rpc.ClientPlatform(model.PlatformIOS) {
		t.Fatalf("platforms 未还原成数组：%v", one.GetPlatforms())
	}
	// 条目是排期数据，看内容走 ResolveSlot：混在一起会让每条坑位多一次子查询。
	if e.call("SlotItem.ListEffective") != 0 || e.call("SlotItem.ListAll") != 0 {
		t.Fatalf("列表接口去查了条目（ListEffective=%d ListAll=%d）",
			e.call("SlotItem.ListEffective"), e.call("SlotItem.ListAll"))
	}
	// 后台列表也不许建投影：坑位定义缓存只属于运行时读路径。
	if len(e.cache.sets) != 0 || len(e.cache.gets) != 0 || len(e.cache.dels) != 0 {
		t.Fatalf("ListSlots 动了缓存：sets=%v gets=%v dels=%v", e.cache.sets, e.cache.gets, e.cache.dels)
	}
	if len(e.audit.reqs) != 0 {
		t.Fatalf("只读接口写了审计：%v", e.audit.reqs)
	}
	// 不限端坑位回空数组而不是 [""]：端上据此判「全端可见」，脏元素会变成第 0 端。
	e.seedSlot(t, &model.RecommendSlot{Code: "ls.nop", Page: "home", Title: "不限端",
		Capacity: 1, State: model.StateOn})
	all, err := e.listSlots(t, &rpc.ListSlotsReq{Ps: 100})
	wantOK(t, all, err, "含不限端坑位")
	for _, row := range all.GetItems() {
		if row.GetCode() == "ls.nop" && len(row.GetPlatforms()) != 0 {
			t.Fatalf("不限端坑位回了端列表：%v", row.GetPlatforms())
		}
	}
}

func TestListSlotsPropagatesStorageErrorVerbatim(t *testing.T) {
	e := newTestEnv(t)
	e.seedSlot(t, &model.RecommendSlot{Code: "ls.db", Page: "home", Title: "库故障",
		Capacity: 3, State: model.StateOn})
	before := e.effects()

	e.db.readErrs["Slot.List"] = errSlotListDown
	reply, err := e.listSlots(t, &rpc.ListSlotsReq{Ps: 100})
	wantFail(t, err, errSlotListDown, "Slot.List 故障")
	if reply != nil {
		t.Fatalf("依赖故障却回了响应体（空列表也是一种伪装）：%+v", reply)
	}
	// 不退化成「空列表 + total=0」：那在后台看起来就是「坑位被删光了」。
	if len(e.cache.sets) != 0 || len(e.audit.reqs) != 0 {
		t.Fatalf("读失败还留下了写副作用：sets=%v audit=%d", e.cache.sets, len(e.audit.reqs))
	}
	delete(e.db.readErrs, "Slot.List")
	ok, err := e.listSlots(t, &rpc.ListSlotsReq{Ps: 100})
	recovered := wantOK(t, ok, err, "故障恢复后")
	if len(recovered.GetItems()) != 1 {
		t.Fatalf("恢复后读不到坑位：%v", slotCodes(recovered.GetItems()))
	}
	e.requireSameEffects(t, before, "ListSlots 只读零副作用")
}

// 后台列表的 total 在关掉 Query.CountTotal 时必须回 0（helpers.go:81-88 的口径：
// 「本接口不提供总数」，README 里写明此时 total 不可信）。
// 这一条同时是 ListRolloutRules 的对照面：那个接口回的是裸 total（见 rolloutread_test.go）。
func TestListSlotsHonoursCountTotalSwitch(t *testing.T) {
	on := newTestEnv(t)
	off := newTestEnvWith(t, func(c *config.Config) { c.Query.CountTotal = false })
	for _, e := range []*testEnv{on, off} {
		e.seedSlot(t, &model.RecommendSlot{Code: "ct.one", Page: "home", Title: "一",
			Capacity: 3, State: model.StateOn})
		e.seedSlot(t, &model.RecommendSlot{Code: "ct.two", Page: "home", Title: "二",
			Capacity: 3, State: model.StateOn})
	}
	r1, err := on.listSlots(t, &rpc.ListSlotsReq{Ps: 1})
	got := wantOK(t, r1, err, "CountTotal=true")
	r2, err := off.listSlots(t, &rpc.ListSlotsReq{Ps: 1})
	hidden := wantOK(t, r2, err, "CountTotal=false")
	if got.GetTotal() != 2 || hidden.GetTotal() != 0 {
		t.Fatalf("total 开关没生效：on=%d off=%d", got.GetTotal(), hidden.GetTotal())
	}
	// 关掉总数不影响本页内容：只关「总数」这一个字段，不退化成「不给数据」。
	if len(got.GetItems()) != 1 || len(hidden.GetItems()) != 1 {
		t.Fatalf("关掉 total 连带砍了列表：%d / %d", len(got.GetItems()), len(hidden.GetItems()))
	}
}

// 本服务最值钱的一条跨接口不变式：同一份配置，后台列表视图与终端解析视图必须是同一批坑位。
// 两侧不一致的表现形式是「后台明明配了，端上说没有」——最难复现的一类投诉。
func TestListSlotsAndResolveSlotAgreeOnTheSameConfig(t *testing.T) {
	e := newTestEnv(t)
	seed := []struct {
		code, platforms string
		state           int32
	}{
		{"mx.all", "", model.StateOn},
		{"mx.android", ",1,", model.StateOn},
		{"mx.two", ",1,3,", model.StateOn},
		{"mx.ios", ",2,", model.StateOn},
		{"mx.desk", ",4,", model.StateOn},
		{"mx.android.off", ",1,", model.StateOff},
		{"mx.all.off", "", model.StateOff},
	}
	for _, s := range seed {
		e.seedSlot(t, &model.RecommendSlot{Code: s.code, Page: "home", Title: s.code,
			Platforms: s.platforms, Capacity: 3, State: s.state})
	}
	// 每个坑位都排上内容：这样 found=true 只能由「可见性判定」决定，不会因为空位而假绿。
	for _, s := range seed {
		e.seedSlotItems(t, e.slotByCode(s.code).SlotID, 3, []*model.SlotItem{
			{Position: 1, ItemType: model.ItemTypeUGCVideo, ItemID: "x-" + s.code, State: model.StateOn},
		})
	}

	platforms := []rpc.ClientPlatform{
		rpc.ClientPlatform(model.PlatformAndroid), rpc.ClientPlatform(model.PlatformIOS),
		rpc.ClientPlatform(model.PlatformHarmony), rpc.ClientPlatform(model.PlatformDesktop),
	}
	for _, p := range platforms {
		name := fmt.Sprintf("端 %d", int32(p))
		t.Run(name, func(t *testing.T) {
			listed, err := e.listSlots(t, &rpc.ListSlotsReq{Platform: p, State: model.StateOn, Ps: 100})
			rows := wantOK(t, listed, err, name+"/ListSlots")
			// 后台视图去重成集合，逐条与端上视图比对。
			fromList := map[string]bool{}
			for _, code := range slotCodes(rows.GetItems()) {
				fromList[code] = true
			}
			fromResolve := map[string]bool{}
			for _, s := range seed {
				reply, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: s.code,
					Target: rolloutCtx(7, p, "7.2.0")})
				got := wantOK(t, reply, err, name+"/ResolveSlot "+s.code)
				if got.GetFound() {
					fromResolve[s.code] = true
					// 命中时必须真给出内容，否则「可见」是空的。
					if len(got.GetItems()) != 1 || got.GetItems()[0].GetItemId() != "x-"+s.code {
						t.Fatalf("%s：ResolveSlot 命中却没回条目：%v", s.code, slotItemIDs(got.GetItems()))
					}
					if got.GetSlot().GetState() != model.StateOn {
						t.Fatalf("%s：命中了停用坑位（state=%d）", s.code, got.GetSlot().GetState())
					}
				}
			}
			if len(fromList) != len(fromResolve) {
				t.Fatalf("%s：两侧条数不同\n 列表=%v\n 解析=%v", name, sortedKeys(fromList), sortedKeys(fromResolve))
			}
			for code := range fromList {
				if !fromResolve[code] {
					t.Fatalf("%s：后台列出了 %s，端上却解析不出（配了但看不到）", name, code)
				}
			}
			for code := range fromResolve {
				if !fromList[code] {
					t.Fatalf("%s：端上能看到 %s，后台列表却没有（该位置将成为无主配置）", name, code)
				}
			}
			// 停用坑位在两侧都必须消失。
			for _, code := range []string{"mx.android.off", "mx.all.off"} {
				if fromResolve[code] || fromList[code] {
					t.Fatalf("%s：停用坑位仍可见（列表=%v 解析=%v）", code, fromList[code], fromResolve[code])
				}
			}
		})
	}

	// 刻意不一致的一处，钉住它免得被「统一」掉：
	// 不给端时，ListSlots 是「不过滤」（后台默认视图要全），
	// ResolveSlot 是「未声明端只给不限端坑位」（猜端会把某端专属位推给全端）。
	noTargetList, err := e.listSlots(t, &rpc.ListSlotsReq{State: model.StateOn, Ps: 100})
	all := wantOK(t, noTargetList, err, "不给端＝不过滤")
	if len(all.GetItems()) != 5 {
		t.Fatalf("不给端应列出全部启用坑位：%v", slotCodes(all.GetItems()))
	}
	var resolved []string
	for _, s := range seed {
		if s.state != model.StateOn {
			continue
		}
		reply, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: s.code})
		got := wantOK(t, reply, err, "不给端/ResolveSlot "+s.code)
		if got.GetFound() {
			resolved = append(resolved, s.code)
		}
	}
	if strings.Join(resolved, ",") != "mx.all" {
		t.Fatalf("未声明端时只应看到不限端坑位：%v", resolved)
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
