// 本文件钉住 ResolveSlot —— 终端读侧热路径（每次首页/详情页渲染都会打它）。
//
// 它最需要被证明的不是「能返回内容」，而是这几条会伤到线上的判定：
//  1. 「这个位置没配」与「这个位置配了但当前不该给这一端看」是两种答案
//     （前者 found=false + 短 TTL 让它快点自愈；后者 found=false + ttl=0 让止血立即生效）；
//  2. 停投与排期到期必须由服务端定时，端上时钟不能决定内容；
//  3. capacity 是硬边界，库里的条目比容量多时也必须截断（否则端上位置错乱）；
//  4. Redis 只是投影：投影坏了一个键都不能改变判定，也不能把请求变成 gRPC 失败；
//  5. MySQL 失败必须以原样错误上抛，绝不允许退化成 found=false 这种「看起来像没配」。
package logic

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/ops-config/internal/config"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"
)

// errSlotStoreDown 是注入的依赖故障。用「真实故障的样子」（连接被拒）而不是 sentinel 常量：
// logic 对它的正确反应只有一种——原样上抛，所以断言的是错误链完整，不是错误相等。
var errSlotStoreDown = errors.New("dial tcp 10.0.0.9:3306: connect: connection refused")

// --- 本文件私有的小工具 ---

func (e *testEnv) resolveSlot(t *testing.T, req *rpc.ResolveSlotReq) (*rpc.ResolveSlotReply, error) {
	t.Helper()
	if req.Ctx == nil {
		req.Ctx = &rpc.CallContext{}
	}
	return NewResolveSlotLogic(bg(), e.svc).ResolveSlot(req)
}

// slotItemIDs 把回参条目压成「位置:内容」串：断言截断与排序时，
// 比对字符串数组比逐字段比对少一半噪声，也不会把「顺序对了但字段错了」放过去。
func slotItemIDs(items []*rpc.SlotItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, fmt.Sprintf("%d:%s", it.GetPosition(), it.GetItemId()))
	}
	return out
}

// logAllText 连 debug 级一起回：本服务有一类契约（app_version 不参与过滤）
// 只有 Debug 日志这条线索，只读 errors() 就证不到它。
func logAllText(w *fakeLogWriter) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.text, "\n")
}

func TestResolveSlotRejectsIllegalInputsBeforeTouchingStorage(t *testing.T) {
	e := newTestEnv(t)
	// 先造一个真实存在的坑位：证明「非法入参」不是因为库里没东西才成功的。
	e.seedSlot(t, &model.RecommendSlot{Code: "rs.home", Page: "home", Title: "首页",
		Capacity: 3, State: model.StateOn})
	before := e.effects()

	cases := []struct {
		name string
		req  *rpc.ResolveSlotReq
		want error
	}{
		{"code 为空", &rpc.ResolveSlotReq{Code: ""}, model.ErrSlotCodeRequired},
		{"code 是空白", &rpc.ResolveSlotReq{Code: "   "}, model.ErrSlotCodeRequired},
		// 形态非法必须被拒而不是「查不到」：拼错编码与没配坑位是两件事，
		// 混成后者会让运营以为已经配上了，端上却永远拿不到内容。
		{"code 太短（1 字符）", &rpc.ResolveSlotReq{Code: "a"}, model.ErrSlotCodeInvalid},
		{"code 含大写", &rpc.ResolveSlotReq{Code: "Rs.Home"}, model.ErrSlotCodeInvalid},
		{"code 含连字符", &rpc.ResolveSlotReq{Code: "rs-home"}, model.ErrSlotCodeInvalid},
		{"code 以下划线开头", &rpc.ResolveSlotReq{Code: "_rs"}, model.ErrSlotCodeInvalid},
		{"code 以点开头", &rpc.ResolveSlotReq{Code: ".rs"}, model.ErrSlotCodeInvalid},
		{"code 超长 65", &rpc.ResolveSlotReq{Code: strings.Repeat("a", 65)}, model.ErrSlotCodeInvalid},
		{"target 端越界", &rpc.ResolveSlotReq{Code: "rs.home", Target: rolloutCtx(7, rpc.ClientPlatform(9), "7.2.0")},
			model.ErrPlatformUnknown},
		// 本项目只有四端，没有小程序（AGENTS.md §1、§6）：
		// 第五个取值绝不能被当成「不做端过滤」放过，那会让一条 Android 专属排期全端可见。
		{"target 是第五端", &rpc.ResolveSlotReq{Code: "rs.home", Target: rolloutCtx(7, rpc.ClientPlatform(5), "7.2.0")},
			model.ErrPlatformUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := e.resolveSlot(t, tc.req)
			wantFail(t, err, tc.want, tc.name)
			if reply != nil {
				t.Fatalf("%s：出错还回了响应体 %+v", tc.name, reply)
			}
			if got := e.call("Slot.FindByCode"); got != 0 {
				t.Fatalf("%s：入参校验阶段就读了坑位表 %d 次", tc.name, got)
			}
			if got := len(e.cache.gets); got != 0 {
				t.Fatalf("%s：入参校验阶段就碰了投影缓存 %d 次", tc.name, got)
			}
		})
	}
	e.requireSameEffects(t, before, "非法 ResolveSlot 入参零副作用")
}

func TestResolveSlotUnknownCodeIsAMissThatIsNotCached(t *testing.T) {
	e := newTestEnv(t)
	key := e.limits.slotKey("code:rs.none")

	reply, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.none"})
	got := wantOK(t, reply, err, "查不存在的坑位")
	if got.GetFound() {
		t.Fatal("坑位不存在却回了 found=true")
	}
	if got.GetSlot() != nil || len(got.GetItems()) != 0 {
		t.Fatalf("不存在却回了内容：%+v items=%v", got.GetSlot(), slotItemIDs(got.GetItems()))
	}
	// 「还没配」要较快自愈，所以回一个短 TTL；但它必须小于任何配置项 TTL，
	// 否则新建坑位要等一个才生效——负缓存是「保存没生效」这类反馈的头号成因。
	if got.GetTtl() != int32(itemPointerTTLSeconds) {
		t.Fatalf("未命中应回 %d 秒建议 TTL：got=%d", itemPointerTTLSeconds, got.GetTtl())
	}
	if _, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.none"}); err != nil {
		t.Fatalf("第二次读取：%v", err)
	}
	if n := e.call("Slot.FindByCode"); n != 2 {
		t.Fatalf("未命中被缓存了（回源 %d 次，期望 2 次）", n)
	}
	if len(e.cache.sets) != 0 {
		t.Fatalf("未命中回填了投影键：%v", e.cache.sets)
	}
	requireOwnKeys(t, e.cache.gets, testKeyPrefix, "ResolveSlot 读键")
	if e.cache.data[key] != "" {
		t.Fatalf("缓存里出现了未命中坑位的值：%s", e.cache.data[key])
	}
}

func TestResolveSlotConfiguredButEmptyIsFoundWithSlotTTL(t *testing.T) {
	e := newTestEnv(t)
	e.seedSlot(t, &model.RecommendSlot{Code: "rs.empty", Page: "home", Title: "空的",
		Capacity: 3, State: model.StateOn})

	reply, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.empty"})
	got := wantOK(t, reply, err, "已配置但没排期的坑位")
	// 与「没配」的区分点：位置存在，端上应渲染骨架并按 slot TTL 缓存；
	// 混成 found=false 会让网关把一个正常位置按「未命中」短 TTL 反复回源。
	if !got.GetFound() {
		t.Fatalf("坑位存在却回了 found=false：%+v", got)
	}
	if len(got.GetItems()) != 0 {
		t.Fatalf("空坑位回了条目：%v", slotItemIDs(got.GetItems()))
	}
	if got.GetSlot().GetCapacity() != 3 || got.GetSlot().GetCode() != "rs.empty" {
		t.Fatalf("坑位定义投影不符：%+v", got.GetSlot())
	}
	if got.GetTtl() != 30 { // OpsSlot.DefaultTTLSeconds=30
		t.Fatalf("TTL 应取配置里的坑位默认值：got=%d", got.GetTtl())
	}
}

func TestResolveSlotStoppedOrInvisibleEndsInMissWithoutReadingItems(t *testing.T) {
	e := newTestEnv(t)
	off := e.seedSlot(t, &model.RecommendSlot{Code: "rs.off", Page: "home", Title: "停投",
		Capacity: 3, State: model.StateOff})
	androidOnly := e.seedSlot(t, &model.RecommendSlot{Code: "rs.android", Page: "home", Title: "仅安卓",
		Platforms: ",1,", Capacity: 3, State: model.StateOn})
	all := e.seedSlot(t, &model.RecommendSlot{Code: "rs.all", Page: "home", Title: "不限端",
		Capacity: 3, State: model.StateOn})
	// 三个坑位都排上内容：如果「停投/不可见」路径漏了短路，条目查询就会留下刻度。
	for _, id := range []int64{off.SlotID, androidOnly.SlotID, all.SlotID} {
		e.seedSlotItems(t, id, 3, []*model.SlotItem{
			{Position: 1, ItemType: model.ItemTypeUGCVideo, ItemID: fmt.Sprintf("i%d", id), State: model.StateOn},
		})
	}
	itemsBefore := e.call("SlotItem.ListEffective")

	cases := []struct {
		name string
		code string
		tgt  *rpc.TargetContext
	}{
		{"停投坑位", "rs.off", rolloutCtx(7, rpc.ClientPlatform(model.PlatformAndroid), "7.2.0")},
		{"仅安卓却问 iOS", "rs.android", rolloutCtx(7, rpc.ClientPlatform(model.PlatformIOS), "7.2.0")},
		{"仅安卓却问鸿蒙", "rs.android", rolloutCtx(7, rpc.ClientPlatform(model.PlatformHarmony), "7.2.0")},
		// 端未声明时只回「不限端」的坑位：猜它是哪一端会把某端专属位置推到全端。
		{"仅安卓却没声明端", "rs.android", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: tc.code, Target: tc.tgt})
			got := wantOK(t, reply, err, tc.name)
			if got.GetFound() || len(got.GetItems()) != 0 || got.GetSlot() != nil {
				t.Fatalf("%s：应整块不可见，实得 %+v items=%v", tc.name, got, slotItemIDs(got.GetItems()))
			}
			// ttl=0 是「立即止血」的兑现：刚停投的内容被继续缓存一个 TTL，
			// 等于运营按了开关却没关掉，而且没有任何日志会指向这条缓存。
			if got.GetTtl() != 0 {
				t.Fatalf("%s：不可见必须禁止缓存，实得 ttl=%d", tc.name, got.GetTtl())
			}
			if e.call("SlotItem.ListEffective") != itemsBefore {
				t.Fatalf("%s：位置对本端不可见却仍去查了条目（多一次无谓回源）", tc.name)
			}
		})
	}

	// 反例：不限端坑位在「没声明端」时也必须可见，否则上面的规则会被写成「一律拒绝」。
	reply, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.all"})
	empty := wantOK(t, reply, err, "不限端坑位不带 target")
	if !empty.GetFound() || len(empty.GetItems()) != 1 {
		t.Fatalf("不限端坑位应全端可见（含未声明端）：%+v", empty)
	}
	if e.call("SlotItem.ListEffective") == itemsBefore {
		t.Fatal("命中路径没查条目：断言失效")
	}

	// ttl=0 约束的是**调用方**，不约束本服务自己的投影：停投坑位的定义行照样按 slotTTL 缓存。
	// 这不是漏洞（SaveSlot 改状态时会先删键，缓存里那行确实是当前真相，
	// 且它只决定「这块位置给不给看」，内容清单每轮都回源），
	// 但口径必须写清：看到 Redis 里有 rs.off 的定义行不等于停投没生效。
	if raw := e.cache.data[e.limits.slotKey("code:rs.off")]; raw == "" {
		t.Fatalf("钉住当前行为：停投坑位的定义行本服务仍会缓存，实得无该键（说明缓存策略变了）")
	} else {
		var row model.RecommendSlot
		if err := json.Unmarshal([]byte(raw), &row); err != nil || row.State != model.StateOff {
			t.Fatalf("缓存里的停投行不对：state 应是 2，raw=%s err=%v", raw, err)
		}
	}
}

func TestResolveSlotCapacityIsAHardBoundaryAtReadTime(t *testing.T) {
	e := newTestEnv(t)
	// 现场：坑位容量被改小到 2，但条目还按 5 个排着没清（seedSlotItems 的 capacity 入参
	// 是 model 侧的校验基准，故意给 5，等价于「条目是按旧容量排进去的」）。
	// SaveSlot 的缩容检查（failclosed_test 的「缩容到装不下现有条目」）本应挡住这种库，
	// 但 DBA 手工改表 / 先改容量再清条目都会留下它 —— 读侧必须自己兜住，
	// 否则端上会拿到一个装不下的结果集，位置 3..5 直接错乱。
	slot := e.seedSlot(t, &model.RecommendSlot{Code: "rs.cap", Page: "home", Title: "容量 2",
		Capacity: 2, State: model.StateOn})
	var items []*model.SlotItem
	for pos := int32(1); pos <= 5; pos++ {
		items = append(items, &model.SlotItem{Position: pos, ItemType: model.ItemTypeUGCVideo,
			ItemID: fmt.Sprintf("c%d", pos), State: model.StateOn})
	}
	e.seedSlotItems(t, slot.SlotID, 5, items)

	cases := []struct {
		name  string
		limit int32
		want  []string
	}{
		{"不给 limit 按容量截断", 0, []string{"1:c1", "2:c2"}},
		{"limit 大于容量仍按容量", 99, []string{"1:c1", "2:c2"}},
		{"limit 小于容量按 limit", 1, []string{"1:c1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.cap", Limit: tc.limit})
			got := wantOK(t, reply, err, tc.name)
			if ids := slotItemIDs(got.GetItems()); strings.Join(ids, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("%s：条目不符\n  got =%v\n want=%v", tc.name, ids, tc.want)
			}
			// 无论截断到几条，回参里的容量都是坑位定义的那个：
			// 端上按它决定占位高度，回 99 会让端上留出一排空位。
			if got.GetSlot().GetCapacity() != 2 {
				t.Fatalf("%s：容量被 limit 改写了：%d", tc.name, got.GetSlot().GetCapacity())
			}
		})
	}

	// 缺陷：resolveslotlogic.go:72 —— `if in.GetLimit() > 0` 把负数 limit 当成「没给」，
	// 而同包对负数 pn/ps 是显式拒绝（helpers.go:556 pageOf → ErrInvalidPage）。
	// 口径不一致的代价：调用方把 limit 拼成 -1 时得到 2 条而不是错误，
	// 它会以为自己传的 limit 生效了。修法方向：limit<0 直接 ErrInvalidPage 同款哨兵。
	neg, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.cap", Limit: -1})
	wantOK(t, neg, err, "负数 limit 当前被当成缺省")
	if len(neg.GetItems()) != 2 {
		t.Fatalf("负数 limit 的当前行为不是「按容量」：%v", slotItemIDs(neg.GetItems()))
	}
}

func TestResolveSlotScheduleWindowIsHalfOpenAndServerClocked(t *testing.T) {
	e := newTestEnv(t)
	now := model.NowUnix()
	slot := e.seedSlot(t, &model.RecommendSlot{Code: "rs.window", Page: "home", Title: "排期",
		Capacity: 6, State: model.StateOn})
	e.seedSlotItems(t, slot.SlotID, 6, []*model.SlotItem{
		{Position: 1, ItemType: model.ItemTypeUGCVideo, ItemID: "always"}, // 双 0 = 不设窗口
		{Position: 2, ItemType: model.ItemTypeUGCVideo, ItemID: "live", StartAt: now - 100, EndAt: now + 100},
		{Position: 3, ItemType: model.ItemTypeUGCVideo, ItemID: "future", StartAt: now + 3600, EndAt: now + 7200},
		{Position: 4, ItemType: model.ItemTypeUGCVideo, ItemID: "past", StartAt: now - 7200, EndAt: now - 3600},
		{Position: 5, ItemType: model.ItemTypeUGCVideo, ItemID: "off", StartAt: now - 100, EndAt: now + 100,
			State: model.StateOff}, // 停用不删行，保留排期证据
		{Position: 6, ItemType: model.ItemTypeUGCVideo, ItemID: "edge", StartAt: now, EndAt: now + 300},
	})

	// 服务端定时：不给 at 就按此刻，future/past/停用都不该出现。
	nowReply, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.window"})
	got := wantOK(t, nowReply, err, "不给 at")
	if ids := slotItemIDs(got.GetItems()); strings.Join(ids, ",") != "1:always,2:live,6:edge" {
		t.Fatalf("此刻的生效集合不符（停用/过期/未到期都不许出现）：got=%v", ids)
	}
	// at=0 与 at<0 同义：负数时间戳不是「无限早」，那样会把 past 也捞回来。
	for _, at := range []int64{0, -1} {
		reply, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.window", At: at})
		neg := wantOK(t, reply, err, fmt.Sprintf("at=%d", at))
		if len(neg.GetItems()) != 3 {
			t.Fatalf("at=%d 应按服务端此刻处理，实得 %v", at, slotItemIDs(neg.GetItems()))
		}
	}
	// 半开区间 [start_at, end_at)：start 含、end 不含（与 Topic.InWindow / RolloutRule 同语义，
	// model 是唯一实现）。写成闭区间会让一条 00:00 结束的排期多播一秒，
	// 而那一秒正好是跨天排期交接最常出事的时刻。
	for _, tc := range []struct {
		name string
		at   int64
		want string
	}{
		{"start_at 当刻即生效", now - 100, "1:always,2:live"},
		{"end_at 当刻即失效（同刻窗口更宽的 edge 仍在）", now + 100, "1:always,6:edge"},
		{"过期排期的最后一秒", now - 3601, "1:always,4:past"},
		{"过期排期的截止当刻", now - 3600, "1:always"},
		{"未到期排期的起始当刻", now + 3600, "1:always,3:future"},
	} {
		reply, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.window", At: tc.at})
		want := wantOK(t, reply, err, tc.name)
		if ids := slotItemIDs(want.GetItems()); strings.Join(ids, ",") != tc.want {
			t.Fatalf("%s（at=%d）：窗口不符\n  got =%v\n want=%s", tc.name, tc.at, ids, tc.want)
		}
	}
	// 到点的内容靠时间自己浮现：全程没有任何写、没有刷新。
	if len(e.audit.reqs) != 0 {
		t.Fatalf("只读接口写了审计：%v", e.audit.reqs)
	}
	if len(e.cache.dels) != 0 {
		t.Fatalf("只读接口删了投影键：%v", e.cache.dels)
	}
}

func TestResolveSlotProjectionHoldsOnlyTheDefinitionRow(t *testing.T) {
	e := newTestEnv(t)
	slot := e.seedSlot(t, &model.RecommendSlot{Code: "rs.cache", Page: "home", Title: "首页金刚位",
		Platforms: ",1,2,", Capacity: 3, State: model.StateOn, Version: 7})
	e.seedSlotItems(t, slot.SlotID, 3, []*model.SlotItem{
		{Position: 1, ItemType: model.ItemTypeUGCVideo, ItemID: "content-9527", State: model.StateOn},
	})
	key := e.limits.slotKey("code:rs.cache")
	requireOwnKeys(t, []string{key}, testKeyPrefix, "ResolveSlot 投影键")

	first, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.cache",
		Target: rolloutCtx(7, rpc.ClientPlatform(model.PlatformAndroid), "")})
	got := wantOK(t, first, err, "首轮")
	if len(got.GetItems()) != 1 || got.GetItems()[0].GetItemId() != "content-9527" {
		t.Fatalf("首轮内容不符：%v", slotItemIDs(got.GetItems()))
	}
	// 缓存值 = 定义行，且只有定义行：内容引用一个字节都不许进缓存，
	// 否则本服务就成了内容清单的第二份真相（AGENTS.md §5）。
	raw := e.cache.data[key]
	if raw == "" {
		t.Fatal("首轮没回填坑位定义投影")
	}
	if strings.Contains(raw, "content-9527") {
		t.Fatalf("缓存值里混进了内容引用：%s", raw)
	}
	var row model.RecommendSlot
	if err := json.Unmarshal([]byte(raw), &row); err != nil {
		t.Fatalf("缓存值不是坑位行的 JSON：%v raw=%s", err, raw)
	}
	if row.SlotID != slot.SlotID || row.Capacity != 3 || row.State != model.StateOn || row.Version != 7 {
		t.Fatalf("缓存里的定义行不符：%+v", row)
	}
	if n := e.cache.setTTLs[key]; n != e.limits.slotTTL {
		t.Fatalf("投影 TTL 应取 OpsSlot.DefaultTTLSeconds(%d)：got=%d", e.limits.slotTTL, n)
	}
	if got.GetSlot().GetVersion() != 7 {
		t.Fatalf("回参丢了乐观锁版本：%+v", got.GetSlot())
	}

	// 第二轮：定义行走缓存，条目必须仍回源 —— 换内容要立刻可见。
	// 键只有 code、没有端：投影存的是「与端无关的定义行」，
	// 换成按 code+platform 派生键就会在 SaveSlotItems 之后删不干净（本服务不用 SCAN/KEYS）。
	setsBefore := len(e.cache.sets)
	second, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.cache",
		Target: rolloutCtx(8, rpc.ClientPlatform(model.PlatformIOS), "7.2.0")})
	wantOK(t, second, err, "次轮")
	if n := e.call("Slot.FindByCode"); n != 1 {
		t.Fatalf("第二轮仍回源查坑位定义：%d 次", n)
	}
	if n := e.call("SlotItem.ListEffective"); n != 2 {
		t.Fatalf("条目被缓存了（期望每次回源，实得 %d 次）", n)
	}
	if len(e.cache.sets) != setsBefore {
		t.Fatalf("命中后又回填了一遍：%v", e.cache.sets)
	}
	if slotItemIDs(second.GetItems())[0] != "1:content-9527" {
		t.Fatalf("命中缓存时内容丢了：%v", slotItemIDs(second.GetItems()))
	}
}

func TestResolveSlotCacheDamageNeverChangesTheVerdict(t *testing.T) {
	e := newTestEnv(t)
	slot := e.seedSlot(t, &model.RecommendSlot{Code: "rs.broken", Page: "home", Title: "坏投影",
		Capacity: 2, State: model.StateOn})
	e.seedSlotItems(t, slot.SlotID, 2, []*model.SlotItem{
		{Position: 1, ItemType: model.ItemTypeUGCVideo, ItemID: "b1", State: model.StateOn},
	})
	key := e.limits.slotKey("code:rs.broken")

	// 基准：一切都正常时答案长什么样，后面三种故障都必须与它逐字段一致。
	base, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.broken"})
	want := wantOK(t, base, err, "基准读取")

	logs := captureLogs(t)
	cases := []struct {
		name   string
		setup  func()
		logSub string
	}{
		{"Redis 读故障", func() { e.cache.getErr = errSlotStoreDown }, "读 " + key + " 失败"},
		{"投影值是半截 JSON", func() { e.cache.seed(key, "{\"slot_id\":") }, "解析 " + key + " 失败"},
		{"回填失败", func() { e.cache.setErr = errSlotStoreDown }, "回填 " + key + " 失败"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup()
			readsBefore := e.call("Slot.FindByCode")
			reply, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.broken"})
			got := wantOK(t, reply, err, tc.name)
			// 判定以 MySQL 为准：投影坏了顶多多回源一次，绝不能变成 gRPC 失败或 found=false。
			if got.GetFound() != want.GetFound() || got.GetTtl() != want.GetTtl() ||
				strings.Join(slotItemIDs(got.GetItems()), ",") != strings.Join(slotItemIDs(want.GetItems()), ",") {
				t.Fatalf("%s：投影故障改变了判定\n  got =%v ttl=%d\n want=%v ttl=%d",
					tc.name, slotItemIDs(got.GetItems()), got.GetTtl(),
					slotItemIDs(want.GetItems()), want.GetTtl())
			}
			// 但也不能安静：只有 Error 日志能证明「这次是投影坏了，不是内容没了」，
			// 排查时没有这条日志就会去查错的地方。
			if !strings.Contains(logs.joined(), tc.logSub) {
				t.Fatalf("%s：故障没有留下 Error 日志（缺 %q）\n%s", tc.name, tc.logSub, logs.joined())
			}
			// 这一次调用确实回源读了库：少了这一步，上面的「判定不变」可能是命中了坏缓存。
			if e.call("Slot.FindByCode") != readsBefore+1 {
				t.Fatalf("%s：投影故障时没有真的回源（%d → %d）", tc.name, readsBefore, e.call("Slot.FindByCode"))
			}
			e.cache.getErr, e.cache.setErr = nil, nil
			delete(e.cache.data, key)
		})
	}
}

func TestResolveSlotPropagatesStorageErrorsVerbatim(t *testing.T) {
	e := newTestEnv(t)
	slot := e.seedSlot(t, &model.RecommendSlot{Code: "rs.db", Page: "home", Title: "库故障",
		Capacity: 2, State: model.StateOn})
	e.seedSlotItems(t, slot.SlotID, 2, []*model.SlotItem{
		{Position: 1, ItemType: model.ItemTypeUGCVideo, ItemID: "d1", State: model.StateOn},
	})

	// 坑位定义读失败：必须是错误，绝不能退成 found=false。
	// 后者的代价是端上「首页这块位置凭空消失」且没有任何错误码，比 500 更难查。
	setsBefore := len(e.cache.sets)
	e.db.readErrs["Slot.FindByCode"] = errSlotStoreDown
	reply, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.db"})
	wantFail(t, err, errSlotStoreDown, "Slot.FindByCode 故障")
	if reply != nil {
		t.Fatalf("依赖故障却回了响应体（等于把故障伪装成结果）：%+v", reply)
	}
	if e.call("SlotItem.ListEffective") != 0 {
		t.Fatal("定义行都没读到还去查条目")
	}
	if len(e.cache.sets) != setsBefore {
		t.Fatalf("读失败却回填了投影（把故障写进缓存）：%v", e.cache.sets)
	}
	delete(e.db.readErrs, "Slot.FindByCode")

	// 条目读失败：同理。此时定义行可以已经缓存好了，判定仍必须是错误。
	first, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.db"})
	wantOK(t, first, err, "先正常读一次")
	itemsBefore := e.call("SlotItem.ListEffective")
	e.db.readErrs["SlotItem.ListEffective"] = errSlotStoreDown
	second, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.db"})
	wantFail(t, err, errSlotStoreDown, "SlotItem.ListEffective 故障")
	if second != nil {
		t.Fatalf("条目读失败却回了 found=%v 的响应", second.GetFound())
	}
	if e.call("SlotItem.ListEffective") != itemsBefore+1 {
		t.Fatal("没有真的去查条目，上面的断言是空的")
	}
	// 错误必须是原样的那一个：不退化成 ErrSlotNotFound（那会被当成「坑位没配」）、
	// 也不退化成 ErrSlotCodeInvalid（那会被当成调用方传错了参数）。
	if errors.Is(err, model.ErrSlotNotFound) || errors.Is(err, model.ErrSlotCodeInvalid) {
		t.Fatalf("依赖错误被改写成了业务语义：%v", err)
	}
}

func TestResolveSlotAppVersionCannotFilterSlotItems(t *testing.T) {
	e := newTestEnv(t)
	slot := e.seedSlot(t, &model.RecommendSlot{Code: "rs.ver", Page: "home", Title: "版本",
		Capacity: 2, State: model.StateOn})
	e.seedSlotItems(t, slot.SlotID, 2, []*model.SlotItem{
		{Position: 1, ItemType: model.ItemTypeUGCVideo, ItemID: "v1", State: model.StateOn},
		{Position: 2, ItemType: model.ItemTypeUGCVideo, ItemID: "v2", State: model.StateOn},
	})
	logs := captureLogs(t)

	// 缺口：ops_recommend_slot_item 没有 app_version 区间列（对照 ops_client_switch
	// 的 min/max_version 与灰度规则的 app_version_min/max），因此 target.app_version
	// 目前无法参与条目过滤。resolveslotlogic.go:94-99 刻意不假装过滤过。
	// 这里钉的是「当前行为」：新端老端拿到的是同一份清单，并且日志里留着痕。
	// 修法方向：给 ops_recommend_slot_item 加版本区间列并在 model.ValidateSlotItems
	// 与 ListEffective 两侧同语义落地，而不是在 logic 里另拼一份判定。
	vars := []string{"", "1.0.0", "99.9.9"}
	var first string
	for _, v := range vars {
		reply, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.ver",
			Target: rolloutCtx(7, rpc.ClientPlatform(model.PlatformAndroid), v)})
		got := wantOK(t, reply, err, "app_version="+v)
		ids := strings.Join(slotItemIDs(got.GetItems()), ",")
		if ids != "1:v1,2:v2" {
			t.Fatalf("app_version=%q 时条目被改动过：%s", v, ids)
		}
		if v == "" {
			first = ids
			continue
		}
		if ids != first {
			t.Fatalf("同一坑位在不同版本下清单不同（说明有隐藏过滤）：%s vs %s", first, ids)
		}
		if !strings.Contains(logAllText(logs), "未按版本过滤") {
			t.Fatalf("app_version=%q 没有留下「未按版本过滤」的日志线索：\n%s", v, logAllText(logs))
		}
	}
	// 空 app_version 不该打这条日志：那是给真实故障留的信道，不能被噪声占满。
	if strings.Count(logAllText(logs), "未按版本过滤") != 2 {
		t.Fatalf("版本过滤提示的条数不符（应只在真的带了版本号时出现）：\n%s", logAllText(logs))
	}
}

func TestResolveSlotSuggestedTTLIsClampedButTheProjectionTTLIsNot(t *testing.T) {
	// 配置：全局上限 7 秒，坑位投影自己 900 秒。
	e := newTestEnvWith(t, func(c *config.Config) {
		c.Cache.MaxTTLSeconds = 7
		c.OpsSlot.DefaultTTLSeconds = 900
	})
	slot := e.seedSlot(t, &model.RecommendSlot{Code: "rs.ttl", Page: "home", Title: "TTL",
		Capacity: 1, State: model.StateOn})
	e.seedSlotItems(t, slot.SlotID, 1, []*model.SlotItem{
		{Position: 1, ItemType: model.ItemTypeUGCVideo, ItemID: "t1", State: model.StateOn},
	})
	key := e.limits.slotKey("code:rs.ttl")

	reply, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.ttl"})
	got := wantOK(t, reply, err, "TTL 夹取")
	if got.GetTtl() != 7 {
		t.Fatalf("建议 TTL 必须被 Cache.MaxTTLSeconds 夹住：got=%d", got.GetTtl())
	}
	// 缺陷：resolveslotlogic.go:135 —— 回填用的是裸 lim.slotTTL，没有过 clampTTL，
	// 而同一份配置的「建议 TTL」在 resolveslotlogic.go:105 被夹住了。
	// 后果：Cache.MaxTTLSeconds 的注释（helpers.go:140「一次错误放量最长要等一个 TTL 才被踢掉」）
	// 对网关/端上生效、对本服务自己的投影不生效 —— 运维把上限调到 7 秒，
	// 停投的坑位定义仍会在 Redis 里留 900 秒，看起来像「开关失效」。
	// 同一形状还在 gettopiclogic.go:119（对照 :69）与 conv_resolve.go:142 各出现一次。
	// 修法方向：Setex/cacheLoad 统一传 lim.clampTTL(...)，或让 slot/topic TTL 配置项本身受 MaxTTLSeconds 约束。
	if n := e.cache.setTTLs[key]; n != 900 {
		t.Fatalf("钉住当前行为：投影 TTL 未被 MaxTTLSeconds 夹住，实得 %d", n)
	}
	// 未命中路径的建议 TTL 走的是另一个常量，同样受上限约束（这里是 5 > 7? 否 → 5）。
	miss, err := e.resolveSlot(t, &rpc.ResolveSlotReq{Code: "rs.none"})
	wantOK(t, miss, err, "未命中路径")
	if miss.GetTtl() != itemPointerTTLSeconds || miss.GetFound() {
		t.Fatalf("未命中的 TTL 语义不符：%+v", miss)
	}
}
