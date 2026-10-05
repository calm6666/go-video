// 测试域 8：版本切换审计列表（ListVersionSwitches）。
//
// 审计是「谁在什么时候把哪个特征切到了哪一版」的唯一事实源，危险有两种：
//  1. 排序或分页错位——审计列表的意义就在「最近几条」，顺序错就等于给出一份
//     看起来完整、实际漏掉最新变更的台账；
//  2. 将存储故障当成「没有切换记录」——total=0 的空页与「库连不上」对运维是
//     完全不同的两件事。
//
// 因此本域钉四件事：
//  1. 守卫（分页 → 键 → since）顺序与「被拒请求零存储调用」；
//  2. 顺序与分页语义：期望顺序只来自 SQL 的 `ORDER BY switch_id DESC LIMIT ? OFFSET ?`
//     （model/featureversionswitch.go:259），种子数据的 ctime 序与 feature_key 序
//     都刻意与该期望不同，于是「按 ctime 排」「按键排」「正着排」都会留下可见差异；
//  3. 上限的来源：分页闸门是 model.ValidatePageSize（types.go:407，越界报错），
//     而不是 model 自己的 VersionSwitchFilter.Normalize（featureversionswitch.go:117，
//     越界夹到 MaxListPageSize）；响应体字节上限（svc.MaxResponseBytes）在本路径不被引用，
//     这一条按观察到的行为钉住；
//  4. 投影：条目内容逐字段等于库里的行，且只等于契约里那 8 个字段。
package logic

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"
)

// --- 本域共用的调用与种子小工具 ---

func listSwitchesReq(key string, since int64, pn, ps int32) *rpc.ListVersionSwitchesReq {
	return &rpc.ListVersionSwitchesReq{FeatureKey: key, Since: since, Pn: pn, Ps: ps}
}

func callListSwitches(f *fixture,
	in *rpc.ListVersionSwitchesReq) (*rpc.ListVersionSwitchesReply, error) {
	return NewListVersionSwitchesLogic(f.ctx, f.ServiceContext).ListVersionSwitches(in)
}

// seedAuditRow 往审计表里静默铺一行。
//
// 为什么不用 f.switches.Append：Append 会把 "switches.Append" 记进调用表，
// 而本域第一条用例判的就是「被拒的请求一次依赖都不碰」（assertTouchedNothing 读的
// 正是那张表）——用 Append 铺数据会让那条断言永远为假，只能把用例改成「先清空调用表」，
// 那等于自废。这里直接写表，但把替身 Append 的三条前置校验照抄一遍（类型合法、
// operator/request_id 非空、uniq (request_id, switch_type)），
// 免得静默播种反而种出真库写不出的行。
func seedAuditRow(t *testing.T, f *fixture, rec *model.VersionSwitch) *model.VersionSwitch {
	t.Helper()
	if strings.TrimSpace(rec.FeatureKey) == "" {
		t.Fatalf("test premise broken: audit row needs a feature_key")
	}
	if !model.ValidSwitchType(rec.SwitchType) {
		t.Fatalf("test premise broken: switch_type %q is not a declared value", rec.SwitchType)
	}
	if strings.TrimSpace(rec.Operator) == "" || strings.TrimSpace(rec.RequestID) == "" {
		t.Fatal("test premise broken: operator and request_id are required by the real Append")
	}
	for _, existing := range f.switches.rows {
		if existing.RequestID == rec.RequestID && existing.SwitchType == rec.SwitchType {
			t.Fatalf("test premise broken: uniq (request_id, switch_type) violated by %s/%s",
				rec.RequestID, rec.SwitchType)
		}
	}
	if rec.Ctime == 0 {
		rec.Ctime = f.nowUnix()
	}
	f.switches.nextID++
	rec.SwitchID = f.switches.nextID
	row := *rec
	f.switches.rows[row.SwitchID] = row
	return &row
}

// auditRow 按 switch_id 取回库里的行（期望值的唯一来源）。
func auditRow(t *testing.T, f *fixture, id int64) *model.VersionSwitch {
	t.Helper()
	row, ok := f.switches.rows[id]
	if !ok {
		t.Fatalf("test premise broken: no audit row with switch_id %d", id)
	}
	out := row
	return &out
}

// auditItemText 是「一条审计条目」的稳定文本形。
// 八个投影字段全进比对，一个都不省：漏投影 ctime、把 to_version 写成 from_version、
// request_id 与 trace_id 互换，都会在这行文本上留下差异。
func auditItemText(id int64, key string, from, to int32, operator, reason, requestID string,
	ctime int64) string {
	return fmt.Sprintf("id=%d|key=%s|%d>%d|op=%s|reason=%s|req=%s|ctime=%d",
		id, key, from, to, operator, reason, requestID, ctime)
}

func wantAuditItem(r *model.VersionSwitch) string {
	return auditItemText(r.SwitchID, r.FeatureKey, r.FromVersion, r.ToVersion,
		r.Operator, r.Reason, r.RequestID, r.Ctime)
}

func gotAuditItem(i *rpc.ListVersionSwitchesReply_SwitchRecord) string {
	return auditItemText(i.GetSwitchId(), i.GetFeatureKey(), i.GetFromVersion(), i.GetToVersion(),
		i.GetOperator(), i.GetReason(), i.GetRequestId(), i.GetCtime())
}

func wantAuditList(rows ...*model.VersionSwitch) string {
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, wantAuditItem(r))
	}
	return strings.Join(parts, " || ")
}

// stripAuditID 去掉条目文本开头的主键段，只留正文。
// 用于「两条不同的库行是否渲染成同一条目」的比对：主键本身不表达变更类型。
func stripAuditID(text string) string {
	if !strings.HasPrefix(text, "id=") {
		return text
	}
	if i := strings.Index(text, "|"); i >= 0 {
		return text[i+1:]
	}
	return text
}

func replyAuditList(reply *rpc.ListVersionSwitchesReply) string {
	parts := make([]string, 0, len(reply.GetItems()))
	for _, i := range reply.GetItems() {
		parts = append(parts, gotAuditItem(i))
	}
	return strings.Join(parts, " || ")
}

// seedSixAuditRows 铺六行，三种键 × 两版，并按下面三张表刻意错开顺序：
//
//	switch_id 倒序（期望）：6 5 4 3 2 1
//	ctime 倒序：            2 4 6 1 5 3
//	feature_key 升序：      1 3 6 | 2 5 | 4
//
// 三个候选排序两两不同，所以「ORDER BY ctime」「ORDER BY feature_key」「ORDER BY switch_id ASC」
// 任何一种走错都会让期望串错位；只比条数则三种都看不出问题。
func seedSixAuditRows(t *testing.T, f *fixture) []*model.VersionSwitch {
	t.Helper()
	keyOf := map[int64]string{1: "a_item_heat_7d", 2: "u_play_finish_7d", 3: "a_item_heat_7d",
		4: "u_zone_ratio_7d", 5: "u_play_finish_7d", 6: "a_item_heat_7d"}
	ctimeOf := map[int64]int64{1: testNow + 30, 2: testNow + 60, 3: testNow + 10,
		4: testNow + 50, 5: testNow + 20, 6: testNow + 40}
	rows := make([]*model.VersionSwitch, 0, 6)
	for id := int64(1); id <= 6; id++ {
		from := int32(id) - 1
		rows = append(rows, seedAuditRow(t, f, &model.VersionSwitch{
			FeatureKey: keyOf[id], SwitchType: model.SwitchTypeVersionSwitch,
			FromVersion: from, ToVersion: int32(id),
			Operator: fmt.Sprintf("admin:ops%d", id), Reason: fmt.Sprintf("reason-%d", id),
			RequestID: fmt.Sprintf("req-audit-%d", id), Ctime: ctimeOf[id],
			TraceID: fmt.Sprintf("trace-%d", id),
		}))
	}
	if len(f.switches.calls) != 0 {
		t.Fatalf("silent seeding recorded calls %v: 守卫用例的零依赖断言会因此空转", f.switches.calls)
	}
	return rows
}

// --- 守卫与守卫顺序：被拒的请求一次依赖都不碰 ---

func TestListVersionSwitchesGuardOrderTouchesNothing(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ListVersionSwitchesReq
		want error
	}{
		{"pn 从 0 起", listSwitchesReq("", 0, 0, 10), model.ErrLimitTooLarge},
		{"pn 为负", listSwitchesReq("", 0, -5, 10), model.ErrLimitTooLarge},
		{"ps 为 0", listSwitchesReq("", 0, 1, 0), model.ErrLimitTooLarge},
		{"ps 为负", listSwitchesReq("", 0, 1, -1), model.ErrLimitTooLarge},
		// 契约写的是 ps 上限 100：越界只能报错。model 侧的 Normalize 会把它夹到 100，
		// 二者只对「越界」给出相反答案，这一条就是区分这两种优先级的地方。
		{"ps 越界则拒绝而非夹取", listSwitchesReq("", 0, 1, model.MaxListPageSize+1),
			model.ErrLimitTooLarge},
		{"键太短", listSwitchesReq("u", 0, 1, 10), model.ErrFeatureKeyRequired},
		{"键含大写", listSwitchesReq("U_play_finish_7d", 0, 1, 10), model.ErrFeatureKeyRequired},
		{"键含非法字符", listSwitchesReq("u_play-finish_7d", 0, 1, 10), model.ErrFeatureKeyRequired},
		// 范围外语义（AGENTS.md §1）：合法形态也不能查出来。
		{"键命中范围外语义", listSwitchesReq("ad_hot_7d", 0, 1, 10), model.ErrFeatureKeyForbidden},
		{"since 为负", listSwitchesReq("", -1, 1, 10), model.ErrBackfillWindowInvalid},
		// 顺序判别①：分页先于键（两者都坏时报分页错）。
		// 若把键校验提到最前，这条会拿到 ErrFeatureKeyRequired 而变红。
		{"分页先于键", listSwitchesReq("u", 0, 1, model.MaxListPageSize+1), model.ErrLimitTooLarge},
		// 顺序判别②：键先于 since（两者都坏时报键错）。
		{"键先于 since", listSwitchesReq("ad_hot_7d", -1, 1, 10), model.ErrFeatureKeyForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			// 库里确有审计行，且键与被测用例的键同族：
			// 只有这样「零 ops」才等于「被守卫挡住」，而不是本来就查不到。
			seedSixAuditRows(t, f)

			reply, err := callListSwitches(f, c.in)
			if !errors.Is(err, c.want) {
				t.Fatalf("err=%v, want %v", err, c.want)
			}
			if reply != nil {
				t.Errorf("reply=%+v, want nil alongside the rejection", reply)
			}
			assertTouchedNothing(t, f)
		})
	}
}

// --- 顺序与分页：期望只来自 ORDER BY switch_id DESC ---

func TestListVersionSwitchesOrdersBySwitchIDDescAcrossPages(t *testing.T) {
	f := newFixture(t)
	seedSixAuditRows(t, f)

	// 整页：顺序必须是 6,5,4,3,2,1（model/featureversionswitch.go:259）。
	// 期望串按这个顺序手写在下面，而不是从实现回读。
	descIDs := []int64{6, 5, 4, 3, 2, 1}
	rows := make([]*model.VersionSwitch, 0, len(descIDs))
	for _, id := range descIDs {
		rows = append(rows, auditRow(t, f, id))
	}
	want := wantAuditList(rows...)

	full, err := callListSwitches(f, listSwitchesReq("", 0, 1, model.MaxListPageSize))
	if err != nil {
		t.Fatalf("list first page: %v", err)
	}
	if got := replyAuditList(full); got != want {
		t.Errorf("items=\n%s\nwant switch_id DESC:\n%s", got, want)
	}
	if full.GetTotal() != 6 {
		t.Errorf("total=%d, want 6", full.GetTotal())
	}

	// 分页拼接：三页等于整页，不重不漏；OFFSET=(pn-1)*ps 走错的实现会在这里错位。
	var paged []string
	for pn := int32(1); pn <= 3; pn++ {
		reply, err := callListSwitches(f, listSwitchesReq("", 0, pn, 2))
		if err != nil {
			t.Fatalf("page %d: %v", pn, err)
		}
		if len(reply.GetItems()) != 2 {
			t.Fatalf("page %d items=%d, want the requested ps=2: %s", pn,
				len(reply.GetItems()), replyAuditList(reply))
		}
		if reply.GetTotal() != 6 {
			t.Errorf("page %d total=%d, want 6: total 是全量计数，与本页条数无关", pn, reply.GetTotal())
		}
		for _, i := range reply.GetItems() {
			paged = append(paged, gotAuditItem(i))
		}
	}
	if strings.Join(paged, " || ") != want {
		t.Errorf("pages concatenated to\n%s\nwant\n%s", strings.Join(paged, " || "), want)
	}

	// 越界页：空页 + 真实 total（不回绕到第一页，也不谎报 total=0）。
	past, err := callListSwitches(f, listSwitchesReq("", 0, 4, 2))
	if err != nil {
		t.Fatalf("page past the end: %v", err)
	}
	if len(past.GetItems()) != 0 {
		t.Errorf("page past the end returned %d items, want 0 (不回绕): %s",
			len(past.GetItems()), replyAuditList(past))
	}
	if past.GetTotal() != 6 {
		t.Errorf("page past the end total=%d, want 6 so the caller can see it is not the end",
			past.GetTotal())
	}

	// 每页参数原样下传：listSeen 按调用顺序记录，逐页核对 pn/ps 而不是只看最后一次。
	seen := f.switches.listSeen
	if len(seen) != 5 {
		t.Fatalf("recorded List calls=%d, want 5 (整页 1 次 + 三页 + 越界页)", len(seen))
	}
	if seen[0].Pn != 1 || seen[0].Ps != model.MaxListPageSize {
		t.Errorf("first call = %+v, want pn/ps 原样下传", seen[0])
	}
	for pn := int32(1); pn <= 3; pn++ {
		if c := seen[int(pn)]; c.Pn != pn || c.Ps != 2 {
			t.Errorf("page %d filter = %+v, want pn=%d ps=2", pn, c, pn)
		}
	}
	if c := seen[4]; c.Pn != 4 || c.Ps != 2 {
		t.Errorf("past-the-end filter = %+v, want pn=4 ps=2", c)
	}
}

// --- 上限的来源：请求值（拒绝）而不是 model 的夹取，也不是配置 ---

func TestListVersionSwitchesPageSizeComesFromTheRequestNotTheModelClamp(t *testing.T) {
	f := newFixture(t)
	seedSixAuditRows(t, f)

	// 请求 ps=3：若分页值被 model 的 Normalize 或配置接管（夹到/改成 MaxListPageSize），
	// 这一页会给出 6 条而不是 3 条，立刻变红。
	small, err := callListSwitches(f, listSwitchesReq("", 0, 1, 3))
	if err != nil {
		t.Fatalf("ps=3: %v", err)
	}
	if len(small.GetItems()) != 3 {
		t.Fatalf("ps=3 returned %d items, want exactly 3 (分页由请求决定): %s",
			len(small.GetItems()), replyAuditList(small))
	}
	if got := replyAuditList(small); got != wantAuditList(
		auditRow(t, f, 6), auditRow(t, f, 5), auditRow(t, f, 4)) {
		t.Errorf("items=\n%s\nwant the three newest rows", got)
	}
	if c := f.switches.listSeen[0]; c.Ps != 3 || c.Pn != 1 {
		t.Errorf("filter = %+v, want ps=3 pn=1 原样下传而不是被夹取后的值", c)
	}

	// 边界对：ps=MaxListPageSize 是契约内的最大值，必须放行（100 条一页拿到全部 6 行）。
	maxPage, err := callListSwitches(f, listSwitchesReq("", 0, 1, model.MaxListPageSize))
	if err != nil {
		t.Fatalf("ps=%d: %v", model.MaxListPageSize, err)
	}
	if len(maxPage.GetItems()) != 6 {
		t.Errorf("ps=%d items=%d, want all 6 rows", model.MaxListPageSize, len(maxPage.GetItems()))
	}

	// 边界对的另一半：ps=MaxListPageSize+1 越界必须报错且一次依赖都不碰。
	// 若优先级换过来（model 的夹取赢），这里会拿到前一页内容 + nil error。
	before := len(f.switches.listSeen)
	_, err = callListSwitches(f, listSwitchesReq("", 0, 1, model.MaxListPageSize+1))
	if !errors.Is(err, model.ErrLimitTooLarge) {
		t.Fatalf("err=%v, want %v for ps just above the contract cap", err, model.ErrLimitTooLarge)
	}
	if len(f.switches.listSeen) != before {
		t.Errorf("List calls after the rejection=%d, want %d: 越界不该查库",
			len(f.switches.listSeen), before)
	}

	// 配置不是本路径的任何闸门：把响应体上限压到 64 字节，审计列表照旧成功。
	// 这是**观察到的行为**（listversionswitcheslogic.go 里没有 svcCtx.MaxResponseBytes()
	// 的比对，而 ListEntityFeatures 有，见 entity_feature_export_test.go 的体积闸门用例），
	// 记下它而不是断言「应该会挡」。见交付说明里的缺口一条。
	verbose := newFixture(t).withMaxResponseBytes(64)
	for i := 0; i < 5; i++ {
		seedAuditRow(t, verbose, &model.VersionSwitch{
			FeatureKey: "u_play_finish_7d", SwitchType: model.SwitchTypeRollback,
			FromVersion: int32(i + 2), ToVersion: int32(i + 1),
			Operator: "admin:ops", Reason: fmt.Sprintf("回滚单 RB-%d", i),
			RequestID: fmt.Sprintf("req-long-%d", i), Ctime: testNow + int64(i),
		})
	}
	reply, err := callListSwitches(verbose, listSwitchesReq("u_play_finish_7d", 0, 1, 100))
	if err != nil {
		t.Fatalf("audit list with a tiny configured byte cap: %v", err)
	}
	if len(reply.GetItems()) != 5 {
		t.Fatalf("items=%d, want 5: 本路径不受配置体积上限影响", len(reply.GetItems()))
	}
	if reply.GetTotal() != 5 {
		t.Errorf("total=%d, want 5 rows counted regardless of their size", reply.GetTotal())
	}
}

// --- key / since 是下传给 SQL 的条件，不是本地再筛一遍 ---

func TestListVersionSwitchesPassesKeyAndSinceDownVerbatim(t *testing.T) {
	t.Run("两侧空白被归一化后才是条件", func(t *testing.T) {
		f := newFixture(t)
		seedSixAuditRows(t, f)
		// a_item_heat_7d 在 id 1/3/6 三行上，期望按 switch_id 倒序给出 6、3、1。
		want := wantAuditList(auditRow(t, f, 6), auditRow(t, f, 3), auditRow(t, f, 1))

		reply, err := callListSwitches(f, listSwitchesReq("  a_item_heat_7d  ", 0, 1, 100))
		if err != nil {
			t.Fatalf("filtered list: %v", err)
		}
		if got := replyAuditList(reply); got != want {
			t.Errorf("items=\n%s\nwant the three a_item_heat_7d rows newest first:\n%s", got, want)
		}
		if reply.GetTotal() != 3 {
			t.Errorf("total=%d, want 3: total 是过滤后的计数，而不是全表 6 行", reply.GetTotal())
		}
		if c := f.switches.listSeen[0]; c.FeatureKey != "a_item_heat_7d" {
			t.Errorf("filter key=%q, want the normalized key (下传的不是带空白的原串)", c.FeatureKey)
		}
	})

	t.Run("空键等于不过滤且不带任何类型条件", func(t *testing.T) {
		f := newFixture(t)
		seedSixAuditRows(t, f)
		reply, err := callListSwitches(f, listSwitchesReq("   ", 0, 1, 100))
		if err != nil {
			t.Fatalf("unfiltered list with a whitespace-only key: %v", err)
		}
		// 观察到的行为：纯空白键在 TrimSpace 之后与空串同义（「查全部」），
		// 而不是「键非法」。listversionswitcheslogic.go:37-42 只在非空时才做键校验。
		if len(reply.GetItems()) != 6 {
			t.Errorf("items=%d, want all 6 rows: %s", len(reply.GetItems()), replyAuditList(reply))
		}
		c := f.switches.listSeen[0]
		if c.FeatureKey != "" || c.SwitchType != "" || c.Until != 0 {
			t.Errorf("filter = %+v, want only key/since/pn/ps 被填：契约里没有 switch_type / until 列", c)
		}
	})

	t.Run("since 是包含边界的条件下推", func(t *testing.T) {
		f := newFixture(t)
		// 三行 ctime 分别是 since-1 / since / since+1：SQL 写的是 `AND ctime >= ?`
		// （model/featureversionswitch.go:238-241），所以边界那一行必须在结果里。
		seedAuditRow(t, f, &model.VersionSwitch{FeatureKey: "u_play_finish_7d",
			SwitchType: model.SwitchTypeActivate, FromVersion: 0, ToVersion: 1,
			Operator: "admin:ops", Reason: "just below the window", RequestID: "req-win-1",
			Ctime: testNow - 1})
		at := seedAuditRow(t, f, &model.VersionSwitch{FeatureKey: "u_play_finish_7d",
			SwitchType: model.SwitchTypeVersionSwitch, FromVersion: 1, ToVersion: 2,
			Operator: "admin:ops", Reason: "right at the window edge", RequestID: "req-win-2",
			Ctime: testNow})
		after := seedAuditRow(t, f, &model.VersionSwitch{FeatureKey: "u_play_finish_7d",
			SwitchType: model.SwitchTypeRollback, FromVersion: 2, ToVersion: 1,
			Operator: "admin:ops", Reason: "inside the window", RequestID: "req-win-3",
			Ctime: testNow + 1})

		reply, err := callListSwitches(f, listSwitchesReq("u_play_finish_7d", testNow, 1, 100))
		if err != nil {
			t.Fatalf("windowed list: %v", err)
		}
		want := wantAuditList(after, at) // switch_id 倒序：3、2；id=1 被 since 条件挡掉
		if got := replyAuditList(reply); got != want {
			t.Errorf("items=\n%s\nwant\n%s", got, want)
		}
		if reply.GetTotal() != 2 {
			t.Errorf("total=%d, want 2 (窗口外那一行既不进条目也不进计数)", reply.GetTotal())
		}
		// since 原样下传：不被换成服务端当前时刻（那会把「查某天之后」变成「查现在之前」）。
		if c := f.switches.listSeen[0]; c.Since != testNow {
			t.Errorf("filter since=%d, want the requested %d verbatim", c.Since, testNow)
		}
	})

	t.Run("窗口在过去终点之外时是空集而不是错误", func(t *testing.T) {
		f := newFixture(t)
		seedSixAuditRows(t, f)
		reply, err := callListSwitches(f, listSwitchesReq("", testNow+1000, 1, 100))
		if err != nil {
			t.Fatalf("window with no rows: %v", err)
		}
		if len(reply.GetItems()) != 0 || reply.GetTotal() != 0 {
			t.Errorf("items=%d total=%d, want 0/0: %s", len(reply.GetItems()), reply.GetTotal(),
				replyAuditList(reply))
		}
	})
}

// --- 存储故障必须是错误，不能读成「没有切换记录」 ---

func TestListVersionSwitchesPropagatesRawStorageError(t *testing.T) {
	boom := errors.New("dial tcp 127.0.0.1:3306: connectex: no connection")
	f := newFixture(t)
	seedSixAuditRows(t, f)
	f.switches.errList = boom

	reply, err := callListSwitches(f, listSwitchesReq("u_play_finish_7d", 0, 1, 20))
	// 「库里没有」与「库查不动」必须能区分：这里拿到的是同一个原始 error，
	// 不是 items=nil / total=0 的空页（后者与真的空台账同形）。
	if err != boom {
		t.Fatalf("err=%v, want the identical raw storage error", err)
	}
	if reply != nil {
		t.Errorf("reply=%+v, want nil alongside the error", reply)
	}
	if n := countCalled(f.switches.calls, "switches.List"); n != 1 {
		t.Errorf("switches.List calls=%d, want 1: 失败后不该重试成一份看起来完整的列表", n)
	}
}

// TestListVersionSwitchesProjectsEveryAuditTypeWithoutTheTypeColumn 钉住审计列表的投影面：
// 六种 switch_type 混在同一份列表里返回（既不过滤也没投影 switch_type），
// 于是「状态变更」与「隐私调整」这类 from_version==to_version 的条目
// 在响应里读起来像一次空切换，而且两条可以完全同形。
//
// 观察到的行为，不是期望：见 listversionswitcheslogic.go:55-67（投影 8 列）与
// model/featureversionswitch.go:79-101（switch_type / from_value / to_value /
// from_digest / to_digest / trace_id / rollback_switch_id 都没进契约条目）。
// 两条断言各自钉住一半：
//   - 同一 request_id 上的 state_change 与 privacy_change 给出逐字相同的条目
//     （真库允许：uniq 索引是 (request_id, switch_type)，见 fakes_test.go:1376-1381）；
//   - 应答里出现的字段集合就是那 8 个，别的一个都不出。
//
// 若日后契约补上 switch_type（或允许按类型过滤），「两条同形」这条必须一起改。
func TestListVersionSwitchesProjectsEveryAuditTypeWithoutTheTypeColumn(t *testing.T) {
	f := newFixture(t)
	types := []string{model.SwitchTypeActivate, model.SwitchTypeVersionSwitch,
		model.SwitchTypeRollback, model.SwitchTypeStateChange, model.SwitchTypePrivacyChange,
		model.SwitchTypeBackfillAutoSwitch}
	ids := make([]int64, 0, len(types))
	for i, st := range types {
		row := seedAuditRow(t, f, &model.VersionSwitch{
			FeatureKey: "u_play_finish_7d", SwitchType: st,
			FromVersion: 2, ToVersion: 3,
			// from_value / to_value 是只有状态与隐私类审计才有的事实，库里都有值，
			// 但审计列表看不见它们。
			FromValue: fmt.Sprintf("from-%s", st), ToValue: fmt.Sprintf("to-%s", st),
			FromDigest: fmt.Sprintf("digest-from-%d", i), ToDigest: fmt.Sprintf("digest-to-%d", i),
			Operator: "admin:ops", Reason: "同一批变更", RequestID: fmt.Sprintf("req-type-%d", i),
			TraceID: fmt.Sprintf("trace-%d", i), RollbackSwitchID: int64(900 + i),
			Ctime: testNow + int64(i),
		})
		ids = append(ids, row.SwitchID)
	}

	// 一对同形行：同一个 request_id 上的一次状态变更与一次隐私调整。
	// 库里的 from_value/to_value 不同（DRAFT>ACTIVE 对比 3>4），条目却一模一样。
	// 真库里这一对是合法的：uniq 索引是 (request_id, switch_type)，
	// 一个工单号既可以落一笔状态变更、也可以落一笔隐私调整。
	sharedRow := func(st, fromValue, toValue string) *model.VersionSwitch {
		return &model.VersionSwitch{FeatureKey: "u_zone_ratio_7d", SwitchType: st,
			FromVersion: 4, ToVersion: 4, // 状态/隐私类审计：to_version 恒等于 from_version
			FromValue: fromValue, ToValue: toValue,
			Operator: "admin:privacy", Reason: "一次工单两笔变更",
			RequestID: "req-shared", Ctime: testNow + 100, TraceID: "trace-shared"}
	}
	stateRow := seedAuditRow(t, f, sharedRow(model.SwitchTypeStateChange, "DRAFT", "ACTIVE"))
	privacyRow := seedAuditRow(t, f, sharedRow(model.SwitchTypePrivacyChange, "3", "4"))
	if stateRow.SwitchID == privacyRow.SwitchID {
		t.Fatal("test premise broken: the two rows must be distinct audit rows")
	}
	if stateRow.FromValue == privacyRow.FromValue || stateRow.ToValue == privacyRow.ToValue {
		t.Fatal("test premise broken: the two rows must record different facts in the DB")
	}

	reply, err := callListSwitches(f, listSwitchesReq("", 0, 1, 100))
	if err != nil {
		t.Fatalf("audit list over every switch type: %v", err)
	}
	if reply.GetTotal() != 8 {
		t.Fatalf("total=%d, want 8 rows (六种类型各一条 + 同形两条)", reply.GetTotal())
	}
	if len(reply.GetItems()) != 8 {
		t.Fatalf("items=%d, want 8: 审计列表不按类型筛，一条都不该少", len(reply.GetItems()))
	}
	// 六条各类型的行 + 两条同形行都在同一份列表里出现（按 switch_id 集合核对）。
	gotIDs := make(map[int64]bool, len(reply.GetItems()))
	for _, i := range reply.GetItems() {
		gotIDs[i.GetSwitchId()] = true
	}
	for _, id := range append(append([]int64{}, ids...), stateRow.SwitchID, privacyRow.SwitchID) {
		if !gotIDs[id] {
			t.Errorf("audit row switch_id=%d missing from the unfiltered list", id)
		}
	}
	newest, second := gotAuditItem(reply.GetItems()[0]), gotAuditItem(reply.GetItems()[1])
	if newest != wantAuditItem(privacyRow) || second != wantAuditItem(stateRow) {
		t.Errorf("two newest items=\n%s\n%s\nwant the state_change and privacy_change rows:\n%s\n%s",
			newest, second, wantAuditItem(stateRow), wantAuditItem(privacyRow))
	}
	// 库里这两行是不同的事实，条目却逐字相同（除了自增主键本身）：
	// 这就是「看不见 switch_type / from_value / to_value」的代价。
	// 主键不携带「这是哪一类变更」的信息，所以比对去掉 id 之后的正文。
	if a, b := stripAuditID(newest), stripAuditID(second); a != b {
		t.Errorf("the two rows rendered differently (%q vs %q): 投影里若出现了区分它们的信息，"+
			"本用例的缺口前提就不再成立，要连同上面的期望一起改", a, b)
	}

	// 投影面：条目文本里只允许出现契约那 8 个字段的值。
	// 用整份应答渲染，避免「漏了一个字段」只能靠人眼看结构体。
	rendered := replyAuditList(reply)
	for _, leak := range []string{"state_change", "privacy_change", "rollback", "activate",
		"version_switch", "backfill_auto_switch", "from-", "to-", "digest-", "trace-", "900"} {
		if strings.Contains(rendered, leak) {
			t.Errorf("audit items contain %q: 投影面是 8 列，越界字段一旦出现在响应里，"+
				"本用例就失去了那条缺口前提", leak)
		}
	}
}

// TestListVersionSwitchesAcceptsUnboundedPageNumbers 钉住一条真实存在的溢出缺口：
// pn 只有下界（pn>=1）没有上界，而 model 的 OFFSET 用 int32 乘出来
// （model/featureversionswitch.go:257 `(f.Pn-1)*f.Ps`）。
// pn=200_000_001、ps=100 时该乘积是 2e10，int32 回绕成负数——
// 真 SQL 会拿到「OFFSET 为负」的错误，而不是空页。
//
// 这里断言的是观察到的行为：logic 照收、照原样下传，替身用 64 位整数算起点所以给出
// 空页 + 真实 total。若日后加了 pn 上界（或 OFFSET 换成 int64 计算），
// 「无错误」与「pn 原样下传」两条必须一起改。
func TestListVersionSwitchesAcceptsUnboundedPageNumbers(t *testing.T) {
	f := newFixture(t)
	seedSixAuditRows(t, f)

	const hugePn = int32(200_000_001) // (hugePn-1)*100 = 2e10 > MaxInt32
	reply, err := callListSwitches(f, listSwitchesReq("", 0, hugePn, 100))
	if err != nil {
		t.Fatalf("pn=%d must not be rejected by the current guards: %v", hugePn, err)
	}
	if len(reply.GetItems()) != 0 {
		t.Errorf("items=%d, want 0: %s", len(reply.GetItems()), replyAuditList(reply))
	}
	if reply.GetTotal() != 6 {
		t.Errorf("total=%d, want the real 6 (空页不该谎报台账是空的)", reply.GetTotal())
	}
	c := f.switches.listSeen[0]
	if c.Pn != hugePn || c.Ps != 100 {
		t.Errorf("filter = %+v, want pn/ps 原样下传（溢出发生在 model 的 OFFSET 乘法里，"+
			"logic 没有做任何收敛）", c)
	}
}
