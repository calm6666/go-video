// 本文件钉住运营侧举报台账入口 ListReports（internal/logic/listreportslogic.go），
// 它是本服务最后一个此前只有构造器级用例的 Logic。
//
// 按风险排序，这个方法真正要证的三件事：
//  1. 隐私：运营面拿到的是「谁举报了谁」的台账，不是「他们说了什么」。
//     ReportInfo 的字段集合与逐字段取值都被钉死，并且整条链路一行消息都不会被物化进内存
//     （用 fixtures_test.go 的 loadedMessageRows 记账来证，比「读代码说没读」强）。
//  2. 守卫次序：operator → state → ps → cursor 四层全部先于查库，被拒时一次数据接口都不许碰。
//  3. 游标翻页：report_id 倒序位点「不漏、不重、不多给」；ps+1 探针是否真按 +1 下传，
//     只能从 fake 记的实参看（fakes_test.go 的 listByCursorArg）——响应里只剩投影，看不出来。
//
// 与 self 域（ListConversations / GetUnreadSummary）不同，本方法**不走**读侧黑名单门禁：
// 那条门禁保护的是「用户之间互相看不见」，而举报台账的主体是运营。
// 这个差别由 TestListReportsDoesNotUseReadSideBlacklistGate 钉住，并配了一条 self 域对照，
// 避免「门禁整体坏掉了」被误读成「本方法设计上不接门禁」。

package logic

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go-video/services/private-message/model"
	"go-video/services/private-message/rpc"
)

const (
	// opMain / opOther 是运营 mid：明显假的号段，不与 alice/bob/carol/mallory 等会话主体重合。
	opMain  = int64(77001)
	opOther = int64(77002)
	// targetA / targetB 是过滤矩阵要的第 4、5 个被举报人。
	// 过滤类用例每档必须给唯一种子值，否则「漏掉一条过滤」在条数上完全看不出来。
	targetA = int64(404)
	targetB = int64(505)
)

// --- 本文件专用的脚手架 ---

func callListReports(t *testing.T, e *env, in *rpc.ListReportsReq) (*rpc.ListReportsReply, error) {
	t.Helper()
	return NewListReportsLogic(bg(), e.svc).ListReports(in)
}

// listReports 调一次举报台账并当场要求成功：本文件里「查询失败」永远不是可接受的结果。
func listReports(t *testing.T, e *env, in *rpc.ListReportsReq, label string) *rpc.ListReportsReply {
	t.Helper()
	got, err := callListReports(t, e, in)
	return wantOK(t, got, err, label)
}

func reportIDs(page *rpc.ListReportsReply) []int64 {
	out := make([]int64, 0, len(page.List))
	for _, r := range page.List {
		out = append(out, r.ReportId)
	}
	return out
}

// reasonSet 把一页里的原因码排序取出：过滤类断言只关心「哪些行进了页」，
// 顺序由翻页用例单独钉（两件事混在一起，失败信息就说不清是哪条规则破了）。
func reasonSet(page *rpc.ListReportsReply) []int32 {
	out := make([]int32, 0, len(page.List))
	for _, r := range page.List {
		out = append(out, r.Reason)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sameNums[T int32 | int64](a, b []T) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

func sameNames(a, b []string) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

// reportRow 回查库里那一行：投影断言必须跟着落库值，而不是跟着传进去的 struct。
func (e *env) reportRow(t *testing.T, reportID int64) model.Report {
	t.Helper()
	row, ok := e.st.reports[reportID]
	if !ok {
		t.Fatalf("库里没有 report_id=%d", reportID)
	}
	return row
}

// reportSeed 是一条举报种子的入参。走 Reports.Insert +（可选）MarkHandled 两条真实写路径，
// 不直接塞 map：只有经过真写路径，「库里有这一行」才等价于「生产库里可能出现这一行」。
type reportSeed struct {
	conversationID, msgID, reporterMid, targetMid int64
	reason                                        int32
	description, traceID                          string
	auditTaskID                                   int64
	// handled 非 0 时插入后立刻处置到该终态（Insert 的起点固定是 PENDING，不必登记）。
	handled int32
	handler int64
	note    string
	// key 是处置幂等键：handled != 0 时必填，空串会被 model 层按「无主处置」拒绝。
	key string
}

func (e *env) seedReport(t *testing.T, s reportSeed) model.Report {
	t.Helper()
	row := model.Report{
		ConversationID: s.conversationID, MsgID: s.msgID, ReporterMid: s.reporterMid,
		TargetMid: s.targetMid, Reason: s.reason, Description: s.description,
		AuditTaskID: s.auditTaskID, TraceID: s.traceID,
	}
	id, created, err := e.svc.Reports.Insert(bg(), &row)
	if err != nil || !created {
		t.Fatalf("seed 举报（msg=%d reporter=%d）：%v created=%v", s.msgID, s.reporterMid, err, created)
	}
	if s.handled != 0 {
		if err := e.svc.Reports.MarkHandled(bg(), id, s.handled, s.handler, s.note, s.key); err != nil {
			t.Fatalf("seed 举报处置 report_id=%d → state=%d：%v", id, s.handled, err)
		}
	}
	stored := e.reportRow(t, id)
	// 口径与 fixtures_test.go 的 seedMessage 完全一致：fake 克隆行、只在自己的副本上盖时间戳
	// （fakeReports.Insert 无条件 row.Ctime = now），调用方手里那个 struct 的 Ctime 仍是 0。
	// 种子若跟着 0，「投影逐字段搬运 ctime」就退化成 0==0 的永真断言。
	if row.Ctime != 0 || stored.Ctime <= 0 {
		t.Fatalf("种子时间戳前提不成立：入参 struct ctime=%d（应为 0），落库行 ctime=%d（应 > 0）", row.Ctime, stored.Ctime)
	}
	return stored
}

// forceReportTimes 手工拉开一行的 ctime/mtime。
// fake 用同一个 timeNowUnix() 盖两列（Insert 与 markHandled 都是），同一秒内两列相等，
// 「两列各搬各的」就退化成一条断言；生产里「处置几乎必然晚于举报」，按那个形状改。
func (e *env) forceReportTimes(t *testing.T, reportID, ctime, mtime int64) model.Report {
	t.Helper()
	row := e.reportRow(t, reportID)
	row.Ctime, row.Mtime = ctime, mtime
	e.st.reports[reportID] = row
	out := e.reportRow(t, reportID)
	if out.Ctime == out.Mtime || out.Ctime <= 0 {
		t.Fatalf("时间戳前提不成立：ctime=%d mtime=%d 必须都是正数且互不相等", out.Ctime, out.Mtime)
	}
	return out
}

// seedReportOn 在已有会话里造「对方发了一条消息、reporter 举报它」的完整现场。
// target_mid 取消息发送者快照，与 reportmessagelogic.go 的写入口径一致。
func (e *env) seedReportOn(t *testing.T, p pair, reporter, seq int64, content, description string, reason int32) model.Report {
	t.Helper()
	msg := e.seedMessage(t, p, p.otherOf(reporter), reporter, seq, content, model.MsgStateNormal)
	return e.seedReport(t, reportSeed{
		conversationID: p.convID, msgID: msg.MsgID, reporterMid: reporter,
		targetMid: msg.SenderMid, reason: reason, description: description,
	})
}

// lastListByCursor 取 fakeReports 记下的最近一次 ListByCursor 实参。
// 本服务里 Reports.ListByCursor 只有 ListReports 一个调用方（listreportslogic.go:58），
// 因此记下的那条必然属于被测方法。
func (e *env) lastListByCursor(t *testing.T) listByCursorArg {
	t.Helper()
	fr, ok := e.svc.Reports.(*fakeReports)
	if !ok {
		t.Fatal("Reports 句柄不是 fakeReports，取不到实参台账")
	}
	if len(fr.listByCursorArgs) == 0 {
		t.Fatal("这次调用没有走到 Reports.ListByCursor")
	}
	return fr.listByCursorArgs[len(fr.listByCursorArgs)-1]
}

// requireNoModelTouched 断言标记之后六个数据句柄一次都没被调用。
// 与 requireOnlyHandleRead 的差别：那条用于「已知道要读哪张表」的入口，
// 这条用于「守卫就该在查库之前挡住、一张都不许读」的负向用例。
func requireNoModelTouched(t *testing.T, m callMark, label string) {
	t.Helper()
	for _, h := range modelHandles {
		if n := m.handleCallsOf(h); n != 0 {
			t.Fatalf("%s：守卫被拒后仍碰了 %s.*（%d 次）；本次实际碰过 %v", label, h, n, m.touchedSince())
		}
	}
}

// exportedFieldNames 取结构体的导出字段名（已排序）。
// protobuf 生成的结构体里 state/unknownFields/sizeCache 是非导出运行时字段，不参与契约。
func exportedFieldNames(v any) []string {
	rt := reflect.TypeOf(v).Elem()
	out := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		if f := rt.Field(i); f.IsExported() {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
}

// --- 身份守卫 ---

// TestListReportsOperatorGateBlocksBeforeAnyQuery 钉住「无主查询不可受理」：
// operator_mid <= 0 必须在任何数据访问之前返回 ErrOperatorRequired，
// 且不写行、不开事务、一行消息都不取进内存。
func TestListReportsOperatorGateBlocksBeforeAnyQuery(t *testing.T) {
	e := newEnv(t)
	p := e.seedPair(t, alice, bob)
	e.seedReportOn(t, p, alice, 1, "被举报的一条消息", "辱骂", 11)

	for _, op := range []int64{0, -1, -opMain} {
		label := fmt.Sprintf("operator_mid=%d", op)
		mark := e.markCalls()
		before := e.probe()
		got, err := callListReports(t, e, &rpc.ListReportsReq{OperatorMid: op, Ps: 10})
		wantFail(t, err, model.ErrOperatorRequired, label)
		if got != nil {
			t.Fatalf("%s：被拒却回了一页 %v（伪造成功是最坏的一类通过）", label, reportIDs(got))
		}
		requireNoModelTouched(t, mark, label)
		e.requireRejectedNoSideEffects(t, before, label)
	}

	// 正向对照：合法 operator 过关并确实查到那一行。
	// 没有这条对照，上面的「被拒」可能只是因为整个入口跑不通。
	page := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 10}, "合法 operator")
	if len(page.List) != 1 || page.List[0].ReporterMid != alice || page.List[0].TargetMid != bob {
		t.Fatalf("合法 operator 应看到 1 条 alice→bob 的举报，实得 %v", reportIDs(page))
	}
}

// TestListReportsGuardPrecedence 钉住四层守卫的先后次序（主体 → 状态 → 页大小 → 游标）。
// 每条用例同时把后几层也写成非法值：回的哨兵才是「先挡住的是谁」的可判别证据，
// 否则把状态校验与 clampPageSize 换个调用顺序，测试照样绿。
func TestListReportsGuardPrecedence(t *testing.T) {
	e := newEnv(t)
	p := e.seedPair(t, alice, bob)
	e.seedReportOn(t, p, alice, 1, "有一条举报", "说明", 12)

	for _, tc := range []struct {
		name string
		in   *rpc.ListReportsReq
		want error
	}{
		{"主体先于状态", &rpc.ListReportsReq{OperatorMid: 0, State: rpc.ReportState(9), Ps: -1, Cursor: "x"}, model.ErrOperatorRequired},
		{"主体先于页大小", &rpc.ListReportsReq{OperatorMid: -3, Ps: -1, Cursor: "x"}, model.ErrOperatorRequired},
		{"主体先于游标", &rpc.ListReportsReq{OperatorMid: 0, Cursor: "x"}, model.ErrOperatorRequired},
		{"状态先于页大小", &rpc.ListReportsReq{OperatorMid: opMain, State: rpc.ReportState(9), Ps: -1, Cursor: "x"}, model.ErrInvalidReportAction},
		{"状态先于游标", &rpc.ListReportsReq{OperatorMid: opMain, State: rpc.ReportState(-2), Cursor: "x"}, model.ErrInvalidReportAction},
		{"页大小先于游标", &rpc.ListReportsReq{OperatorMid: opMain, Ps: -1, Cursor: "x"}, model.ErrInvalidPage},
		{"页大小上限先于游标", &rpc.ListReportsReq{OperatorMid: opMain, Ps: 51, Cursor: "x"}, model.ErrPsTooLarge},
		{"最后才轮到游标", &rpc.ListReportsReq{OperatorMid: opMain, Ps: 10, Cursor: "x"}, model.ErrInvalidCursor},
	} {
		mark := e.markCalls()
		before := e.probe()
		got, err := callListReports(t, e, tc.in)
		wantErr(t, err, tc.want, tc.name)
		if got != nil {
			t.Fatalf("%s：入参被拒却回了一页 %v", tc.name, reportIDs(got))
		}
		requireNoModelTouched(t, mark, tc.name)
		e.requireRejectedNoSideEffects(t, before, tc.name)
	}
}

// TestListReportsOperatorMidIsNotAScope 钉住「本服务不按 operator 切分可见范围」这条现状：
// 台账是全站一份，换一个合法 operator、甚至换一个与任何举报都无关的 operator，
// 拿到的都是同一页。这不是缺陷（管理员鉴权与数据范围归 gateway/admin），
// 但它意味着本方法的越权风险全部压在「谁能提交 operator_mid」上，必须显式留痕。
func TestListReportsOperatorMidIsNotAScope(t *testing.T) {
	e := newEnv(t)
	p := e.seedPair(t, alice, bob)
	e.seedReportOn(t, p, alice, 1, "bob 的一条消息", "骚扰", 13)

	first := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 10}, "operator=opMain")
	second := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opOther, Ps: 10}, "operator=opOther")
	third := listReports(t, e, &rpc.ListReportsReq{OperatorMid: targetA, Ps: 10}, "operator 与任何举报都无关")

	if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(first, third) {
		t.Fatalf("同一份台账在三个合法 operator 下回了不同的页：%v / %v / %v",
			reportIDs(first), reportIDs(second), reportIDs(third))
	}
	// 反过来也要成立：operator_mid 不是 target_mid 的隐式过滤，
	// 否则「与举报无关的运营」会看到一张空表，游标语义随之失真。
	if len(first.List) != 1 || first.List[0].TargetMid != bob {
		t.Fatalf("合法 operator 看不到别人的举报（operator 被当成了过滤条件？）：%v", reportIDs(first))
	}
}

// --- 分页 ---

// TestListReportsPageSizeBounds 钉页大小的三档：负数拒绝、超上限拒绝、恰好等于上限放行，
// 以及「ps=0 走服务端默认」——这条只能从下传给 model 的实参看出来（默认 20 → 探针 21）。
func TestListReportsPageSizeBounds(t *testing.T) {
	e := newEnv(t)
	p := e.seedPair(t, alice, bob)
	e.seedReportOn(t, p, alice, 1, "一条被举报的消息", "说明", 14)

	for _, tc := range []struct {
		name string
		ps   int32
		want error
	}{
		{"负数", -1, model.ErrInvalidPage},
		{"极小负数", math.MinInt32, model.ErrInvalidPage},
		{"超上限一档", 51, model.ErrPsTooLarge},
		{"远超上限", 1 << 20, model.ErrPsTooLarge},
	} {
		mark := e.markCalls()
		got, err := callListReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: tc.ps})
		wantErr(t, err, tc.want, fmt.Sprintf("ps=%d（%s）", tc.ps, tc.name))
		if got != nil {
			t.Fatalf("ps=%d 被拒却回了一页", tc.ps)
		}
		requireNoModelTouched(t, mark, fmt.Sprintf("ps=%d", tc.ps))
	}

	// 上限本身合法（是「原样受理」而不是「夹到上限」），默认页也合法。
	for _, ps := range []int32{0, 1, 50} {
		page := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: ps}, fmt.Sprintf("ps=%d", ps))
		if len(page.List) != 1 {
			t.Fatalf("ps=%d 应回 1 条，实得 %d", ps, len(page.List))
		}
		wantPS := ps
		if wantPS == 0 {
			wantPS = 20 // newTestConfig 的 PrivateMessage.PageSize
		}
		if got := e.lastListByCursor(t).ps; got != wantPS+1 {
			t.Fatalf("ps=%d 时下传了 ps=%d，期望探针值 %d（多取一条判 has_more）", ps, got, wantPS+1)
		}
	}
}

// TestListReportsPageSizeFollowsConfigLimits 钉页大小与配置的三条关系（都是现状）：
//  1. 默认页大小配成 0 时，ps=0 的请求被显式拒绝，不会退化成空页假成功；
//  2. PageSize 与 MaxPageSize 同时为 0 能通过 config.Validate（它只比两列大小，
//     config.go:117），此时**上限检查整体失效**（helpers.go:251 的 `maxPS > 0 &&`），
//     显式 ps 不受限；
//  3. 上限配到 int32 边界时，logic 的 ps+1 会溢出成负数，本方法自己不发现，
//     只由 model 的 `ps <= 0` 守卫（pm_report.go:187）原样挡回。
//
// ⚠ 2、3 是现状哨兵，收严方向见 README「举报台账（ListReports）本轮钉住的现状」前两条。
func TestListReportsPageSizeFollowsConfigLimits(t *testing.T) {
	e := newEnv(t)
	p := e.seedPair(t, alice, bob)
	e.seedReportOn(t, p, alice, 1, "一条被举报的消息", "说明", 15)

	// ① 默认页大小非法：ps=0（走默认）必拒，显式 ps 照旧可用。
	e.svc.Config.PrivateMessage.PageSize = 0
	got, err := callListReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain})
	wantFail(t, err, model.ErrInvalidPage, "服务端默认页大小为 0")
	if got != nil {
		t.Fatal("默认页大小非法却仍回了一页")
	}
	if page := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 5}, "默认页坏掉但显式 ps 可用"); len(page.List) != 1 {
		t.Fatalf("显式 ps 应仍然可用，实得 %d 条", len(page.List))
	}

	// ② 双零配置：启动自检放得过去，且上限彻底不设防。
	degenerate := newTestConfig()
	degenerate.PrivateMessage.PageSize = 0
	degenerate.PrivateMessage.MaxPageSize = 0
	if err := degenerate.Validate(); err != nil {
		t.Fatalf("前提不成立：双零页大小配置被启动自检拒绝了（%v），下面那条现状断言就没意义了", err)
	}
	e.svc.Config = degenerate
	page := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 5000}, "上限为 0 = 不设限")
	if len(page.List) != 1 {
		t.Fatalf("期望原样受理 ps=5000（现状：上限未配置即不设限），实得 %d 条", len(page.List))
	}
	if arg := e.lastListByCursor(t); arg.ps != 5001 {
		t.Fatalf("下传的探针值是 %d 而不是 5001：上限检查可能被别处夹掉了", arg.ps)
	}

	// ③ ps = MaxInt32 时 ps+1 溢出：logic 不发现，由 model 的 ps<=0 守卫挡回（原样上抛）。
	e.svc.Config.PrivateMessage.MaxPageSize = math.MaxInt32
	mark := e.markCalls()
	got, err = callListReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: math.MaxInt32})
	wantFail(t, err, model.ErrInvalidPage, "ps=MaxInt32 的探针值溢出")
	if got != nil {
		t.Fatalf("溢出被静默当成了成功（回了 %d 条）", len(got.List))
	}
	if arg := e.lastListByCursor(t); arg.ps != math.MinInt32 {
		t.Fatalf("期望下传的正是溢出值 %d（本用例钉的就是 logic 不发现溢出），实得 %d", math.MinInt32, arg.ps)
	}
	if n := mark.callsOf("Reports.ListByCursor"); n != 1 {
		t.Fatalf("ListByCursor 应恰好一次（由 model 兜住溢出），实得 %d 次", n)
	}
}

// TestListReportsPagesDescCursorWithoutGapsOrRepeats 是游标翻页的正确性定义本身：
// report_id 倒序、不漏一条、不重一条、每页不超发，最后一页既不给游标也不说还有。
func TestListReportsPagesDescCursorWithoutGapsOrRepeats(t *testing.T) {
	e := newEnv(t)
	p := e.seedPair(t, alice, bob)
	const total = 5
	seeded := make([]int64, 0, total)
	for i := 0; i < total; i++ {
		rep := e.seedReportOn(t, p, alice, int64(i+1), fmt.Sprintf("第 %d 条被举报的消息", i+1), "", int32(21+i))
		seeded = append(seeded, rep.ReportID)
	}
	// 前提：主键严格递增，否则「期望 = 种子倒序」是我臆造的而不是数据决定的。
	for i := 1; i < len(seeded); i++ {
		if seeded[i] <= seeded[i-1] {
			t.Fatalf("种子举报主键非递增：%v", seeded)
		}
	}
	want := make([]int64, len(seeded))
	for i, id := range seeded {
		want[len(seeded)-1-i] = id
	}

	var (
		cursor  string
		got     []int64
		pages   int
		perPage = int32(2)
	)
	for {
		if pages >= 10 {
			t.Fatalf("翻页 10 次仍未结束，游标停在 %q：位点不前进", cursor)
		}
		before := e.probe()
		page := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: perPage, Cursor: cursor}, "翻页")
		pages++
		ids := reportIDs(page)
		if int32(len(ids)) > perPage {
			t.Fatalf("第 %d 页回了 %d 条，超过 ps=%d：无界返回", pages, len(ids), perPage)
		}
		got = append(got, ids...)
		if !page.HasMore {
			if page.NextCursor != "" {
				t.Fatalf("最后一页仍给了游标 %q", page.NextCursor)
			}
			break
		}
		if page.NextCursor == "" {
			t.Fatalf("第 %d 页 has_more=true 却没有游标：客户端会停在半路", pages)
		}
		if wantCursor := fmt.Sprint(ids[len(ids)-1]); page.NextCursor != wantCursor {
			t.Fatalf("第 %d 页游标 %q 不等于本页最后一条的 report_id %s：位点会跳过或重复", pages, page.NextCursor, wantCursor)
		}
		if arg := e.lastListByCursor(t); arg.cursorID != 0 && fmt.Sprint(arg.cursorID) != cursor {
			t.Fatalf("第 %d 页把游标 %q 解成了位点 %d", pages, cursor, arg.cursorID)
		}
		cursor = page.NextCursor
		// 只读：不改状态、不写审计、不开事务（处置入口只有 HandleReport）。
		e.requireSameRowsAndWrites(t, before, fmt.Sprintf("第 %d 页", pages))
	}
	if pages != 3 {
		t.Fatalf("%d 条按 ps=2 应恰好 3 页，实得 %d 页", total, pages)
	}
	if !sameNums(got, want) {
		t.Fatalf("翻页结果不符\n  期望（report_id 倒序）：%v\n  实得：%v", want, got)
	}
	seen := map[int64]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("翻页出现重复举报 %d：游标位点用了 >= 而不是 <", id)
		}
		seen[id] = true
	}
}

// TestListReportsHasMoreFollowsProbeNotTotal 钉 has_more 的唯一来源是「多取一条」探针：
// 恰好 ps 条时必须报到底（否则客户端白翻一页），多一条时才报还有。
// 本方法没有 COUNT、也不该有：台账翻页不能为判尾再扫一遍全表。
func TestListReportsHasMoreFollowsProbeNotTotal(t *testing.T) {
	e := newEnv(t)
	p := e.seedPair(t, alice, bob)
	for i := 0; i < 3; i++ {
		e.seedReportOn(t, p, alice, int64(i+1), fmt.Sprintf("第 %d 条", i+1), "", int32(31+i))
	}

	exact := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 3}, "ps 恰好等于条数")
	if exact.HasMore || exact.NextCursor != "" {
		t.Fatalf("三条数据 ps=3 却报 has_more=%v 游标=%q：探针少算了一条", exact.HasMore, exact.NextCursor)
	}
	if len(exact.List) != 3 {
		t.Fatalf("ps=3 应回 3 条，实得 %d", len(exact.List))
	}

	oneShort := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 2}, "ps 比条数少一")
	if !oneShort.HasMore {
		t.Fatal("还有 1 条没给却报 has_more=false：客户端会漏读")
	}
	if oneShort.NextCursor != fmt.Sprint(reportIDs(oneShort)[1]) {
		t.Fatalf("游标 %q 不是本页最后一条的主键 %v", oneShort.NextCursor, reportIDs(oneShort))
	}
	// 探针取到的那「多一条」绝不能出现在响应里。
	if len(oneShort.List) != 2 {
		t.Fatalf("多取的一条被吐进了响应：%v", reportIDs(oneShort))
	}
	if arg := e.lastListByCursor(t); arg.ps != 3 {
		t.Fatalf("ps=2 时下传 %d，探针不是 +1", arg.ps)
	}
}

// TestListReportsEmptyAndOutOfRangeCursor 钉三种「没有数据」的形态都回真空页：
// 库里没行、状态过滤命中 0 行、游标越界。三者都不该报错，也不该报 has_more。
func TestListReportsEmptyAndOutOfRangeCursor(t *testing.T) {
	e := newEnv(t)
	got, err := callListReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 10})
	wantOK(t, got, err, "空表")
	if got.List == nil {
		t.Fatal("空页回了 nil 切片：conv.go:91 的 reportInfoList 恒给非 nil，nil 意味着投影被整段跳过")
	}
	if len(got.List) != 0 || got.HasMore || got.NextCursor != "" {
		t.Fatalf("空表回成了 %v / has_more=%v / cursor=%q", reportIDs(got), got.HasMore, got.NextCursor)
	}

	// 形态二/三：有数据，但被过滤条件与游标挡掉。
	p := e.seedPair(t, alice, bob)
	only := e.seedReportOn(t, p, alice, 1, "唯一的一条", "说明", 41)
	if page := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, State: rpc.ReportState(model.ReportStateDismissed)}, "状态命中 0 行"); len(page.List) != 0 || page.HasMore {
		t.Fatalf("状态过滤回成了 %v / has_more=%v", reportIDs(page), page.HasMore)
	}
	// 越界位点：比所有行都小的 cursor，倒序位点之后没有行（report_id < cursor）。
	if page := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Cursor: "1"}, "游标越界"); len(page.List) != 0 || page.HasMore || page.NextCursor != "" {
		t.Fatalf("越界游标回成了 %v / has_more=%v", reportIDs(page), page.HasMore)
	}
	// 游标落在唯一那条的主键上：位点是**开区间**，这一条不该再出现。
	if page := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Cursor: fmt.Sprint(only.ReportID)}, "游标落在唯一行上"); len(page.List) != 0 {
		t.Fatalf("游标位置的那一条被重复返回：%v", reportIDs(page))
	}
	// 「往后翻到空」与「查询本身坏了」是两件事：撤掉游标后仍能拿到那一条（对照）。
	if page := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain}, "撤掉游标后"); len(page.List) != 1 {
		t.Fatalf("对照失败：无游标时应回 1 条，实得 %d（上面那些空页就成了查询坏掉的证据）", len(page.List))
	}
}

// TestListReportsCursorDecodingMatchesIDCursor 钉举报游标的取值域，以及
// 「不可解析绝不退化成第一页」这条 fail-stop 口径（与会话游标同源，helpers.go:266/:295）。
func TestListReportsCursorDecodingMatchesIDCursor(t *testing.T) {
	e := newEnv(t)
	p := e.seedPair(t, alice, bob)
	first := e.seedReportOn(t, p, alice, 1, "一条被举报的消息", "说明", 42)
	second := e.seedReportOn(t, p, alice, 2, "另一条被举报的消息", "说明", 43)
	if second.ReportID <= first.ReportID {
		t.Fatalf("种子主键顺序不符：%d %d", first.ReportID, second.ReportID)
	}

	for _, tc := range []struct {
		name   string
		cursor string
		want   error
	}{
		{"非数字", "abc", model.ErrInvalidCursor},
		{"串台成会话游标（time:id）", "1700000000:5", model.ErrInvalidCursor},
		{"负数位点", "-1", model.ErrInvalidCursor},
		{"超出 int64", "99999999999999999999", model.ErrInvalidCursor},
		{"含空格的非数字", " a b c ", model.ErrInvalidCursor},
	} {
		mark := e.markCalls()
		before := e.probe()
		got, err := callListReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 10, Cursor: tc.cursor})
		wantErr(t, err, tc.want, tc.name)
		if got != nil {
			t.Fatalf("%s：不可解析的游标却回了 %v（退化成第一页就是重复推送）", tc.name, reportIDs(got))
		}
		requireNoModelTouched(t, mark, tc.name)
		e.requireRejectedNoSideEffects(t, before, tc.name)
	}

	// 合法但语义特殊的位点，按现状钉住：
	//   - 空串、"0"、"00"、带空格的 "0" 都表示「从头开始」（decodeIDCursor 只在 <0 时报错），
	//     客户端回传 "0" 会重复拿到第一页，而这不是错误；
	//   - 首尾空格被 TrimSpace 吃掉（游标由服务端自己产出，容错不改变语义）。
	for _, cursor := range []string{"", "0", " 0 ", "00"} {
		page := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 10, Cursor: cursor}, fmt.Sprintf("位点 %q", cursor))
		if !sameNums(reportIDs(page), []int64{second.ReportID, first.ReportID}) {
			t.Fatalf("游标 %q 应等价于第一页（倒序两条），实得 %v", cursor, reportIDs(page))
		}
		if arg := e.lastListByCursor(t); arg.cursorID != 0 {
			t.Fatalf("游标 %q 解成了位点 %d，应为 0（不过滤）", cursor, arg.cursorID)
		}
	}
	// 真位点必须前进而不是从头给：cursor = 最大主键+1 → 两条；= 最大主键 → 只剩一条。
	above := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 10, Cursor: fmt.Sprint(second.ReportID + 1)}, "位点在全部行之上")
	if !sameNums(reportIDs(above), []int64{second.ReportID, first.ReportID}) {
		t.Fatalf("位点 %d 应回 2 条，实得 %v", second.ReportID+1, reportIDs(above))
	}
	if arg := e.lastListByCursor(t); arg.cursorID != second.ReportID+1 {
		t.Fatalf("位点 %d 没被原样下传（实得 %d）", second.ReportID+1, arg.cursorID)
	}
	atTop := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 10, Cursor: fmt.Sprint(second.ReportID)}, "位点正好在最大行上")
	if !sameNums(reportIDs(atTop), []int64{first.ReportID}) {
		t.Fatalf("位点 %d 之后应只剩 %d，实得 %v", second.ReportID, first.ReportID, reportIDs(atTop))
	}
}

// TestListReportsCursorFailStopsOnUnencodablePrimaryKey 钉一条与
// TestListConversationsPageSizeAndCursorGuards 同族的 fail-stop：
// 游标编不出来时（encodeIDCursor 对 id <= 0 回空串，helpers.go:288），
// logic 把 has_more 一起抹成 false——宁让客户端停在半路，也不给一个续翻不了的位点。
//
// 可达性要说实话：report_id 是自增主键，正常写入永远走不到这条分支。
// 这里手工塞两行主键 <= 0 的脏数据（只有 DBA 改库才会出现，口径同 fixtures_test.go 的 rawMessage），
// 钉的是**降级方向**（静默截断 vs 报错 vs 死循环），不是「数据会长这样」。
// ⚠ 现状哨兵：收严后（例如回明确错误而不是假「到底」）本用例必须变红，见 README。
func TestListReportsCursorFailStopsOnUnencodablePrimaryKey(t *testing.T) {
	e := newEnv(t)
	p := e.seedPair(t, alice, bob)
	for i := 0; i < 2; i++ {
		e.seedReportOn(t, p, alice, int64(i+1), fmt.Sprintf("正常的一条（第 %d 条）", i+1), "", int32(51+i))
	}
	// 两行主键不合法的脏行：让「本页最后一条」正好落在 report_id = 0 上。
	e.st.reports[0] = model.Report{ReportID: 0, MsgID: 9001, ReporterMid: alice, TargetMid: bob,
		Reason: 59, State: model.ReportStatePending, Ctime: 1700000000, Mtime: 1700000000}
	e.st.reports[-1] = model.Report{ReportID: -1, MsgID: 9002, ReporterMid: alice, TargetMid: bob,
		Reason: 60, State: model.ReportStatePending, Ctime: 1700000001, Mtime: 1700000001}

	got, err := callListReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 3})
	wantOK(t, got, err, "脏主键下的游标降级")
	// 前提：探针确实多取到了第 4 行（否则 has_more=false 只是因为表里本来就只有 3 行）。
	if arg := e.lastListByCursor(t); arg.ps != 4 {
		t.Fatalf("探针值不是 ps+1=4，实得 %d", arg.ps)
	}
	if len(got.List) != 3 {
		t.Fatalf("截断后应回 ps=3 条，实得 %v", reportIDs(got))
	}
	if got.NextCursor != "" || got.HasMore {
		t.Fatalf("现状应是「游标编不出 → 连带抹掉 has_more」，实得 cursor=%q has_more=%v", got.NextCursor, got.HasMore)
	}
	if n := len(e.st.reports); n != 4 {
		t.Fatalf("库里共 %d 行（前提不成立：应恰有 1 行被这次翻页静默截断）", n)
	}
}

// --- 过滤 ---

// TestListReportsStateAndTargetFiltersAreAnded 每档给唯一种子值（reason 即行身份）：
// 漏掉任一条过滤、把两条过滤 OR 起来、或把实参搬错位置，都会在条数/集合上立刻可见。
func TestListReportsStateAndTargetFiltersAreAnded(t *testing.T) {
	e := newEnv(t)
	// (reason, 被举报人, 状态)。状态走真实写路径推到位：Insert 固定 PENDING，MarkHandled 做 CAS。
	matrix := []struct {
		reason int32
		target int64
		state  int32
	}{
		{reason: 11, target: bob, state: model.ReportStatePending},
		{reason: 12, target: bob, state: model.ReportStateHandled},
		{reason: 13, target: carol, state: model.ReportStatePending},
		{reason: 14, target: carol, state: model.ReportStateDismissed},
		{reason: 15, target: targetA, state: model.ReportStateHandled},
		{reason: 16, target: targetB, state: model.ReportStateDismissed},
	}
	pairs := map[int64]pair{}
	seqs := map[int64]int64{}
	for i, m := range matrix {
		p, ok := pairs[m.target]
		if !ok {
			p = e.seedPair(t, alice, m.target)
			pairs[m.target] = p
		}
		seqs[m.target]++
		rep := e.seedReportOn(t, p, alice, seqs[m.target], fmt.Sprintf("%d 号被举报消息", m.reason), "", m.reason)
		if m.state == model.ReportStatePending {
			continue
		}
		if err := e.svc.Reports.MarkHandled(bg(), rep.ReportID, m.state, opMain, "处置备注", fmt.Sprintf("seed-handle-%d", i)); err != nil {
			t.Fatalf("把举报 %d 推到 state=%d：%v", rep.ReportID, m.state, err)
		}
	}
	// 前提：六个种子确实各成一行（否则下面的期望可能只是空表巧合）。
	if n := len(e.st.reports); n != len(matrix) {
		t.Fatalf("种子行数 %d 与矩阵 %d 不符", n, len(matrix))
	}

	byReason := func(reasons ...int32) []int32 {
		out := append([]int32(nil), reasons...)
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	for _, tc := range []struct {
		name   string
		state  rpc.ReportState
		target int64
		want   []int32
	}{
		{name: "全部", state: rpc.ReportState_REPORT_STATE_UNSPECIFIED, want: byReason(11, 12, 13, 14, 15, 16)},
		{name: "只看待处理", state: rpc.ReportState_REPORT_STATE_PENDING, want: byReason(11, 13)},
		{name: "只看已处理", state: rpc.ReportState_REPORT_STATE_HANDLED, want: byReason(12, 15)},
		{name: "只看已驳回", state: rpc.ReportState_REPORT_STATE_DISMISSED, want: byReason(14, 16)},
		{name: "按被举报人 bob", state: rpc.ReportState_REPORT_STATE_UNSPECIFIED, target: bob, want: byReason(11, 12)},
		{name: "按被举报人 carol", state: rpc.ReportState_REPORT_STATE_UNSPECIFIED, target: carol, want: byReason(13, 14)},
		{name: "按被举报人 targetB", state: rpc.ReportState_REPORT_STATE_UNSPECIFIED, target: targetB, want: byReason(16)},
		// 交叉：AND 一旦被写成 OR，这一档会多回 12/13/15。
		{name: "状态与人的交集", state: rpc.ReportState_REPORT_STATE_HANDLED, target: bob, want: byReason(12)},
		{name: "交集为空", state: rpc.ReportState_REPORT_STATE_HANDLED, target: carol, want: byReason()},
	} {
		page := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 20, State: tc.state, TargetMid: tc.target}, tc.name)
		if !sameNums(reasonSet(page), tc.want) {
			t.Fatalf("%s：原因码集合不符\n  期望：%v\n  实得：%v（主键 %v）", tc.name, tc.want, reasonSet(page), reportIDs(page))
		}
		// 实参位置也要对上：state 与 target_mid 换个位，投影照样像样，只有实参能证伪。
		arg := e.lastListByCursor(t)
		if arg.state != int64(tc.state) || arg.targetMid != tc.target || arg.cursorID != 0 || arg.ps != 21 {
			t.Fatalf("%s：下传实参不符 %+v（state/target_mid/cursor/ps 四位）", tc.name, arg)
		}
	}

	// 现状：target_mid 为负不校验（不走 checkMid），落到 model 的「> 0 才过滤」语义上
	// 等价于「不过滤」，回整页而不是报错。影响是运营台把一个 mid 误传成负数时，
	// 拿到的是全站台账而不是空页——不会越权，但会让人以为过滤生效了。
	// ⚠ 现状哨兵：收严方向见 README「举报台账（ListReports）本轮钉住的现状」最后一条。
	all := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 20, TargetMid: -bob}, "负 target_mid")
	if len(all.List) != len(matrix) {
		t.Fatalf("负 target_mid 应等价于不过滤（现状），实得 %v", reasonSet(all))
	}
}

// --- 投影与隐私 ---

// wantReportInfoFields 是运营面允许看到的举报字段全集（rpc/privatemessage.proto:322-336）。
// 钉「集合」而不是只钉逐字段取值，是为了让「往 ReportInfo 加一个 content/preview 列」
// 这类扩张先在这里变红：加字段的人必须显式回答「它为什么不携带私信正文」。
// 顺序是导出字段名的字典序（exportedFieldNames 会排序），与结构体声明顺序无关。
var wantReportInfoFields = []string{
	"AuditTaskId", "ConversationId", "Ctime", "Description", "HandleNote", "Handler",
	"MsgId", "Mtime", "Reason", "ReportId", "ReporterMid", "State", "TargetMid",
}

// wantListReportsReqFields 是本入口接受的全部入参：没有任何按 conversation_id / msg_id /
// report_id 收敛的过滤位，也没有 pending_only 之类的开关（「只看未处理」只能用 state=1 表达）。
// 这条钉子同时是「别凭空调一个不存在的守卫」的防幻觉声明。
var wantListReportsReqFields = []string{"Cursor", "OperatorMid", "Ps", "State", "TargetMid", "TraceId"}

// TestListReportsProjectionIsFieldForField 逐字段核对投影，并证明这条链路
// 对私信正文没有任何读取能力（一次消息行物化都没发生）。
func TestListReportsProjectionIsFieldForField(t *testing.T) {
	e := newEnv(t)
	// 下游接上：audit_task_id 是机审真给的任务号，不是种子硬编的。
	e.attachDownstream(t)
	p := e.seedPair(t, alice, bob)
	msg := e.seedMessage(t, p, bob, alice, 1, secretBody, model.MsgStateNormal)
	reply, err := e.report(t, msg.MsgID, alice, 7, "对方在私信里辱骂并索要转账")
	wantOK(t, reply, err, "举报一条真实消息")

	// 把这条举报推到已处理：handler/note/mtime/幂等键才有真值可投影（PENDING 行这些全是零值，
	// 「投影搬运了 handler」在零值上是永真的）。
	if err := e.svc.Reports.MarkHandled(bg(), reply.ReportId, model.ReportStateHandled, opOther, "已核实并转交处罚", "proj-handle-key"); err != nil {
		t.Fatalf("处置种子举报：%v", err)
	}
	stored := e.forceReportTimes(t, reply.ReportId, 1700000000, 1700000900)
	// 前提：处置幂等键真的落了库 —— 它**不该**出现在响应里，靠下面的字段集合钉子保证，
	// 所以先确认库里有值，否则那条钉子会退化成「比对了两个都不存在的空串」。
	if !stored.HandleIdempotencyKey.Valid || stored.HandleIdempotencyKey.String == "" {
		t.Fatalf("处置幂等键没落库（%+v），字段集合钉子会退化成永真", stored.HandleIdempotencyKey)
	}

	mark := e.markCalls()
	before := e.probe()
	page := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 10}, "运营台账")
	requireOnlyHandleRead(t, mark, "Reports", "运营台账")
	if len(page.List) != 1 {
		t.Fatalf("应回 1 条举报，实得 %v", reportIDs(page))
	}
	info := page.List[0]

	// 逐字段：期望值全部来自**回查到的落库行**，而不是传进去的 struct。
	switch {
	case info.ReportId != stored.ReportID:
		t.Fatalf("report_id 投影不符：%d vs 落库 %d", info.ReportId, stored.ReportID)
	case info.ConversationId != stored.ConversationID || info.ConversationId != p.convID:
		t.Fatalf("conversation_id 投影不符：%d vs 落库 %d", info.ConversationId, stored.ConversationID)
	case info.MsgId != stored.MsgID || info.MsgId != msg.MsgID:
		t.Fatalf("msg_id 投影不符：%d vs 落库 %d", info.MsgId, stored.MsgID)
	case info.ReporterMid != stored.ReporterMid || info.ReporterMid != alice:
		t.Fatalf("reporter_mid 投影不符：%d", info.ReporterMid)
	case info.TargetMid != stored.TargetMid || info.TargetMid != bob:
		t.Fatalf("target_mid 投影不符：%d（应取消息发送者快照）", info.TargetMid)
	case info.Reason != stored.Reason || info.Reason != 7:
		t.Fatalf("reason 投影不符：%d", info.Reason)
	case info.State != stored.State || info.State != model.ReportStateHandled:
		t.Fatalf("state 投影不符：%d（举报状态只由 HandleReport/ApplyModerationVerdict 推进）", info.State)
	case info.Handler != stored.Handler || info.Handler != opOther:
		t.Fatalf("handler 投影不符：%d", info.Handler)
	case info.HandleNote != stored.HandleNote:
		t.Fatalf("handle_note 投影不符：%q vs 落库 %q", info.HandleNote, stored.HandleNote)
	case info.AuditTaskId != stored.AuditTaskID || info.AuditTaskId == 0:
		t.Fatalf("audit_task_id 投影不符：%d vs 落库 %d", info.AuditTaskId, stored.AuditTaskID)
	case info.Ctime != stored.Ctime || info.Ctime <= 0:
		t.Fatalf("ctime 投影不符：%d vs 落库 %d", info.Ctime, stored.Ctime)
	case info.Mtime != stored.Mtime:
		t.Fatalf("mtime 投影不符：%d vs 落库 %d", info.Mtime, stored.Mtime)
	case info.Description != "对方在私信里辱骂并索要转账":
		t.Fatalf("正常说明应原样给运营（不含引流形态）：%q", info.Description)
	}

	// 字段集合钉子：不多不少。
	if got := exportedFieldNames(info); !sameNames(got, wantReportInfoFields) {
		t.Fatalf("ReportInfo 字段集合变了\n  期望：%v\n  实得：%v\n新增字段必须先证明它不携带私信正文", wantReportInfoFields, got)
	}
	if got := exportedFieldNames(&rpc.ListReportsReq{}); !sameNames(got, wantListReportsReqFields) {
		t.Fatalf("ListReportsReq 入参集合变了：%v（期望 %v）", got, wantListReportsReqFields)
	}

	// 正文侧证据：整条链路没有一条消息行被物化进内存，因此也不可能有解密与正文出境。
	// 这是本包能给出的最强隐私证明（Cache 是具体类型，见 fakes_test.go 文件头）。
	if got := e.loadedMessageRows() - before.msgRows; got != 0 {
		t.Fatalf("举报台账读出了 %d 条消息行：运营面不该有任何读正文的能力", got)
	}
	dump := fmt.Sprintf("%v", page)
	for _, leak := range []string{secretBody, string(msg.ContentCipher), msg.Preview, msg.ContentHash} {
		if leak != "" && strings.Contains(dump, leak) {
			t.Fatalf("台账响应里出现了正文侧材料 %q", leak)
		}
	}
	// trace_id 与处置幂等键留在库里，不给运营面（它们是链路/幂等句柄，不是台账内容）。
	if stored.TraceID != "" && strings.Contains(dump, stored.TraceID) {
		t.Fatalf("台账响应里出现了 trace_id %q", stored.TraceID)
	}
	if strings.Contains(dump, "proj-handle-key") {
		t.Fatal("台账响应里出现了处置幂等键")
	}
	e.requireSameRowsAndWrites(t, before, "运营台账（只读）")
}

// TestListReportsDescriptionIsMaskedOnlyAtReadTime 钉举报说明的两条口径：
//   - 库里存的是举报人写的原话（reportmessagelogic.go 只截断不改写），这是处置证据；
//   - 出运营面前过 displayUserText（helpers.go:363）去掉引流形态，台账列表不能变成引流位。
//
// 也就是说脱敏发生在**读侧**，本方法一行都不写。若哪天有人把脱敏改成回写，
// 「库里仍是原话」这条会立刻变红。
func TestListReportsDescriptionIsMaskedOnlyAtReadTime(t *testing.T) {
	e := newEnv(t)
	p := e.seedPair(t, alice, bob)
	cases := []struct {
		name        string
		stored      string
		wantDisplay string
	}{
		{name: "手机号/QQ 形态", stored: "加我 13800001234 详聊", wantDisplay: "[含联系方式，已隐去]"},
		{name: "链接形态", stored: "看这里 https://evil.example.com/x", wantDisplay: "[含链接，已隐去]"},
		{name: "正常说明原样给", stored: "他在私信里持续辱骂我", wantDisplay: "他在私信里持续辱骂我"},
	}
	byReason := map[int32]string{}
	for i, tc := range cases {
		reason := int32(61 + i)
		rep := e.seedReportOn(t, p, alice, int64(i+1), fmt.Sprintf("第 %d 条被举报的消息", i+1), tc.stored, reason)
		byReason[reason] = tc.name
		// 前提：库里那一行存的就是原话（写侧不脱敏，否则下面的读侧断言只是复读）。
		if got := e.reportRow(t, rep.ReportID).Description; got != tc.stored {
			t.Fatalf("%s：落库说明已被改写成 %q，读侧脱敏的前提不成立", tc.name, got)
		}
	}

	before := e.probe()
	page := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 20}, "举报说明脱敏")
	if len(page.List) != len(cases) {
		t.Fatalf("应回 %d 条，实得 %v", len(cases), reportIDs(page))
	}
	for i, tc := range cases {
		reason := int32(61 + i)
		var info *rpc.ReportInfo
		for _, row := range page.List {
			if row.Reason == reason {
				info = row
			}
		}
		if info == nil {
			t.Fatalf("%s：这一档没进页", tc.name)
		}
		if info.Description != tc.wantDisplay {
			t.Fatalf("%s：说明投影 %q，期望 %q", tc.name, info.Description, tc.wantDisplay)
		}
	}
	// 只读：脱敏绝不回写主数据。
	e.requireSameRowsAndWrites(t, before, "举报说明脱敏（只读）")
	for _, row := range e.st.reports {
		if strings.Contains(row.Description, "已隐去") {
			t.Fatalf("库里被写成了脱敏文案（report_id=%d）：%q", row.ReportID, row.Description)
		}
	}
}

// --- 门禁与下游故障 ---

// TestListReportsDoesNotUseReadSideBlacklistGate 钉住一条容易被误加的事实：
// 举报台账**不接** self 域那条读侧黑名单门禁（gate.go 的 peerBlockedForRead）。
//
// 为什么这是对的而不是漏了：黑名单保护的是「用户 A 不想再看见用户 B」，
// 而这里的读取主体是运营，读的是举报单，不是任何人的会话。
// 照搬 self 域口径等于让 social-graph 的可用性决定运营能不能看台账。
//
// 因此本用例与其余读侧用例相反：**不接任何下游**必须成功。
// 末尾给一条 self 域对照，证明这不是「环境没配好所以谁都失败」的假通过。
func TestListReportsDoesNotUseReadSideBlacklistGate(t *testing.T) {
	e := newEnv(t)
	if e.svc.SocialGraph != nil || e.svc.RiskControl != nil || e.svc.Moderation != nil {
		t.Fatal("前提不成立：本用例要求三个下游都未配置")
	}
	p := e.seedPair(t, alice, bob)
	e.seedReportOn(t, p, alice, 1, "被举报的消息", "说明", 71)

	mark := e.markCalls()
	page := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 10}, "零下游下的举报台账")
	if len(page.List) != 1 {
		t.Fatalf("举报台账被下游缺失挡掉了：%v（设计上它只依赖 MySQL 的 pm_report）", reportIDs(page))
	}
	// 只依赖 pm_report 一张表：多读任何一张（尤其 Messages）都是新增越权面。
	requireOnlyHandleRead(t, mark, "Reports", "零下游下的举报台账")

	// 对照：同一套零下游环境里，self 域读侧是 fail-closed 的整页拒绝。
	got, err := callListConversations(t, e, &rpc.ListConversationsReq{Mid: bob})
	wantFail(t, err, model.ErrSocialGraphNotConfigured, "对照：会话列表的读侧门禁")
	if got != nil && len(got.List) != 0 {
		t.Fatalf("对照失败：会话列表在下游缺失时仍给了数据（那条 fail-closed 口径已被破坏）")
	}
}

// TestListReportsPropagatesModelFailureVerbatim 钉依赖故障的处理方式（原样上抛）：
// 既不折叠成「空页」这种看着像成功的结果，也不就地吞掉只写日志。
// 用 errInjected 而不是业务哨兵，才测得出「把任意错误都折叠成某个码」这类实现。
func TestListReportsPropagatesModelFailureVerbatim(t *testing.T) {
	e := newEnv(t)
	p := e.seedPair(t, alice, bob)
	e.seedReportOn(t, p, alice, 1, "被举报的消息", "说明", 81)
	e.st.failTx["Reports.ListByCursor"] = errInjected

	logBefore := len(e.logs.all())
	mark := e.markCalls()
	before := e.probe()
	got, err := callListReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 10})
	wantFail(t, err, errInjected, "ListByCursor 故障")
	if got != nil {
		t.Fatalf("下游故障却回了响应体（%v）：故障不能被折叠成空页", reportIDs(got))
	}
	if n := mark.callsOf("Reports.ListByCursor"); n != 1 {
		t.Fatalf("故障路径上 ListByCursor 被调了 %d 次（本方法没有重试，也不该有）", n)
	}
	e.requireSameRowsAndWrites(t, before, "ListByCursor 故障")
	// 现状：本方法完全不写日志（错误直接上抛给 RPC 层）。
	// 「吞掉只写日志」这一类处理在这里不存在，用日志条数钉住，别靠读代码相信。
	if added := len(e.logs.all()) - logBefore; added != 0 {
		t.Fatalf("故障路径上写了 %d 条日志（现状是 0 条、只上抛）", added)
	}

	// 撤销注入后回到真值：注入没有改写数据。
	delete(e.st.failTx, "Reports.ListByCursor")
	page := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 10}, "注入撤销后")
	if len(page.List) != 1 {
		t.Fatalf("撤销注入后应回 1 条，实得 %v", reportIDs(page))
	}
	if added := len(e.logs.all()) - logBefore; added != 0 {
		t.Fatalf("成功路径写了 %d 条日志：台账查询不该产生日志", added)
	}
	// trace_id 被接受但完全不参与行为：既不写日志也不改变结果（见 wantListReportsReqFields 那条钉子）。
	withTrace := listReports(t, e, &rpc.ListReportsReq{OperatorMid: opMain, Ps: 10, TraceId: "trace-pm-list-1"}, "带 trace_id")
	if added := len(e.logs.all()) - logBefore; added != 0 {
		t.Fatalf("带 trace_id 的调用多写了 %d 条日志（现状：trace_id 在本方法里被完全忽略）", added)
	}
	if !reflect.DeepEqual(withTrace, page) {
		t.Fatalf("trace_id 影响了查询结果：%v vs %v", reportIDs(withTrace), reportIDs(page))
	}
}
