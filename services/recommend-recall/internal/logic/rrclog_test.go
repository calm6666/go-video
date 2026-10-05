// 本文件覆盖 GetRecallRequestLog / ListRecallRequestLogs（召回审计回放，运营/排障面）。
//
// 守住四条：
//  1. 参数缺失与"未命中"是两件事：两个 ID 都没给必须报错，未命中回 Entry=nil、err=nil；
//  2. 存储故障绝不允许被读成未命中（errLogFind 注入位就是为这条存在的）；
//  3. per_source / requested_sources 是本服务自己写的受控文本，解析失败必须整条/整页失败，
//     静默给"没有逐路统计"的干净响应等于把一次降级读成正常；
//  4. Count 与 List 必须用同一个谓词，否则 total 与页内容不匹配、has_more 会说谎。
package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/recommend-recall/internal/config"
	"go-video/services/recommend-recall/internal/svc"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"
)

// panicked 报告 f 是否以 panic 结束，并带回 recover 值（用来钉住
// "哪些方法有 nil-repository 守卫、哪些没有"这类只能靠行为区分的事实）。
func panicked(f func()) (hit bool, text string) {
	defer func() {
		if r := recover(); r != nil {
			hit, text = true, fmt.Sprint(r)
		}
	}()
	f()
	return false, ""
}

func TestGetRecallRequestLogRequiresOneOfTwoIDs(t *testing.T) {
	cases := []struct {
		name string
		req  *rpc.GetRecallRequestLogReq
		want error
	}{
		{"nil 请求", nil, model.ErrRequestRequired},
		{"两个 ID 都没给", &rpc.GetRecallRequestLogReq{}, model.ErrRequestLogRequired},
		{"两个 ID 全是空白", &rpc.GetRecallRequestLogReq{RequestId: "  ", SnapshotId: "\t"},
			model.ErrRequestLogRequired},
	}
	for _, tc := range cases {
		e := newEnv(t)
		reply, err := NewGetRecallRequestLogLogic(e.ctx(), e.svcCtx).GetRecallRequestLog(tc.req)
		requireErrIs(t, err, tc.want)
		if reply != nil {
			t.Fatalf("%s：拒绝却回了响应 %+v", tc.name, reply)
		}
		// 空白 ID 尤其重要：不 Trim 就会带着 "" 进 SQL，把"没传"读成"查不到"。
		if got := e.calls().total(); got != 0 {
			t.Fatalf("%s：拒绝前已触库 %d 次：%v", tc.name, got, e.calls().ops())
		}
	}
}

// 两个 ID 同时给出时以 request_id 为准（它是幂等键；snapshot_id 同一次请求重试可能变）。
// 布景刻意让两个 ID 指向不同的行，这样"取错行"必然可见。
func TestGetRecallRequestLogPrefersRequestIDSentinel(t *testing.T) {
	e := newEnv(t)
	e.st.seedRequestLog(requestLogRow("req-A", "snap-A"))
	e.st.seedRequestLog(requestLogRow("req-B", "snap-B"))

	reply, err := NewGetRecallRequestLogLogic(e.ctx(), e.svcCtx).GetRecallRequestLog(
		&rpc.GetRecallRequestLogReq{RequestId: "req-A", SnapshotId: "snap-B"})
	if err != nil {
		t.Fatalf("回放失败：%v", err)
	}
	if got := reply.GetEntry().GetRequestId(); got != "req-A" {
		t.Fatalf("entry.request_id=%q，两个 ID 都给时必须以 request_id 为准", got)
	}
	assertOps(t, e.calls(), 0, "RequestLog.FindByRequestID")
}

// 逐列投影：审计行的每个字段都必须原样出现在响应里（回放靠它复现一次请求）。
func TestGetRecallRequestLogProjectsEveryColumn(t *testing.T) {
	e := newEnv(t)
	row := requestLogRow("req-full", "snap-full")
	row.Mid = 900000077
	row.Scene = "video.end"
	row.Platform = model.PlatformIOS
	row.AppVersion = "10.0.1"
	row.Region = "cn-south"
	row.RequestedSources = encodeSourcesCSV([]int32{model.SourceHot, model.SourceFollow, model.SourceCold})
	row.PerSource = statsJSON(t,
		&rpc.SourceStat{Source: rpc.Source_SOURCE_HOT, Planned: 120, Returned: 118,
			PoolVersion: 6, BatchId: "b6", Degraded: false},
		&rpc.SourceStat{Source: rpc.Source_SOURCE_FOLLOW, Planned: 60, Returned: 0,
			PoolVersion: 0, BatchId: "", Degraded: true, ErrorCode: "feature_unavailable"})
	row.CandidateCount = 118
	row.ReturnedCount = 100
	row.Degraded = 1
	row.DegradeReason = model.DegradeReasonFeatureUnavailable
	row.CostMs = 42
	row.TraceID = "trace-abc"
	row.Ctime = 1700000200
	e.st.seedRequestLog(row)

	entry, err := NewGetRecallRequestLogLogic(e.ctx(), e.svcCtx).GetRecallRequestLog(
		&rpc.GetRecallRequestLogReq{RequestId: "req-full"})
	if err != nil {
		t.Fatalf("按 request_id 回放失败：%v", err)
	}
	got := entry.GetEntry()
	if got.GetSnapshotId() != "snap-full" || got.GetMid() != 900000077 || got.GetScene() != "video.end" {
		t.Errorf("标识三列投影错：snapshot=%q mid=%d scene=%q", got.GetSnapshotId(), got.GetMid(), got.GetScene())
	}
	if got.GetPlatform() != rpc.Platform_PLATFORM_IOS || got.GetAppVersion() != "10.0.1" ||
		got.GetRegion() != "cn-south" {
		t.Errorf("客户端三列投影错：platform=%v app=%q region=%q",
			got.GetPlatform(), got.GetAppVersion(), got.GetRegion())
	}
	if s := fmtSources(got.GetRequestedSources()); s != "SOURCE_HOT,SOURCE_FOLLOW,SOURCE_COLD" {
		t.Errorf("requested_sources=%q，csv 解码结果不符", s)
	}
	if len(got.GetPerSource()) != 2 {
		t.Fatalf("per_source 解码出 %d 路，期望 2 路", len(got.GetPerSource()))
	}
	follow := got.GetPerSource()[1]
	if follow.GetSource() != rpc.Source_SOURCE_FOLLOW || !follow.GetDegraded() ||
		follow.GetErrorCode() != "feature_unavailable" || follow.GetReturned() != 0 ||
		follow.GetPoolVersion() != 0 {
		t.Errorf("降级那一路投影错：%+v", follow)
	}
	if got.GetCandidateCount() != 118 || got.GetReturnedCount() != 100 || got.GetCostMs() != 42 {
		t.Errorf("计数三列投影错：cand=%d ret=%d cost=%d",
			got.GetCandidateCount(), got.GetReturnedCount(), got.GetCostMs())
	}
	if !got.GetDegraded() || got.GetReason() != rpc.DegradeReason_DEGRADE_REASON_FEATURE_UNAVAILABLE {
		t.Errorf("降级声明投影错：degraded=%t reason=%v", got.GetDegraded(), got.GetReason())
	}
	if got.GetVersionsDigest() != row.VersionsDigest || got.GetTraceId() != "trace-abc" ||
		got.GetCtime() != 1700000200 {
		t.Errorf("追溯三列投影错：digest=%q trace=%q ctime=%d",
			got.GetVersionsDigest(), got.GetTraceId(), got.GetCtime())
	}

	// 同一行按 snapshot_id 必须读到同一条（两条查询路径的投影口径不得分叉）。
	bySnap, err := NewGetRecallRequestLogLogic(e.ctx(), e.svcCtx).GetRecallRequestLog(
		&rpc.GetRecallRequestLogReq{SnapshotId: "snap-full"})
	if err != nil {
		t.Fatalf("按 snapshot_id 回放失败：%v", err)
	}
	if bySnap.GetEntry().GetRequestId() != "req-full" || !bySnap.GetEntry().GetDegraded() {
		t.Fatalf("snapshot 路径读到的不是同一行或降级丢了：%+v", bySnap.GetEntry())
	}
}

// 未命中 = 空 entry + 无错误；存储故障 = 错误。两者绝不能互换。
func TestGetRecallRequestLogDistinguishesMissFromStoreFailure(t *testing.T) {
	e := newEnv(t)
	e.st.seedRequestLog(requestLogRow("req-exist", "snap-exist"))

	reply, err := NewGetRecallRequestLogLogic(e.ctx(), e.svcCtx).GetRecallRequestLog(
		&rpc.GetRecallRequestLogReq{RequestId: "req-never"})
	if err != nil {
		t.Fatalf("未命中被当成错误：%v", err)
	}
	if reply.GetEntry() != nil {
		t.Fatalf("未命中却回了非空 entry（契约要求 null，不能用空对象冒充命中）：%+v", reply.GetEntry())
	}
	reply2, err := NewGetRecallRequestLogLogic(e.ctx(), e.svcCtx).GetRecallRequestLog(
		&rpc.GetRecallRequestLogReq{SnapshotId: "snap-never"})
	if err != nil || reply2.GetEntry() != nil {
		t.Fatalf("snapshot 未命中口径错：err=%v entry=%+v", err, reply2.GetEntry())
	}

	// 注入一次真实故障：必须原样上抛，不允许落到"未命中"分支。
	e2 := newEnv(t)
	e2.st.seedRequestLog(requestLogRow("req-exist", "snap-exist"))
	e2.st.errLogFind = errors.New("mysql down")
	reply3, err := NewGetRecallRequestLogLogic(e2.ctx(), e2.svcCtx).GetRecallRequestLog(
		&rpc.GetRecallRequestLogReq{RequestId: "req-exist"})
	if err == nil {
		t.Fatalf("存储故障被吞成了一次正常回放：%+v", reply3)
	}
	if errors.Is(err, model.ErrRequestNotFound) {
		t.Fatalf("存储故障被错误地包装成未命中：%v", err)
	}
	if reply3 != nil {
		t.Fatalf("故障时回了响应：%+v", reply3)
	}
}

// 受控文本被改坏 = 数据可信度问题，必须报错而不是回一个"看起来干净"的 entry。
func TestGetRecallRequestLogPropagatesMalformedControlledText(t *testing.T) {
	cases := []struct{ name, perSource, requested string }{
		{"per_source 不是 JSON", "{oops", "1"},
		{"requested_sources 含非数字", "[]", "1,x"},
		{"requested_sources 含非法路号", "[]", "1,99"},
	}
	for _, tc := range cases {
		e := newEnv(t)
		row := requestLogRow("req-bad", "snap-bad")
		row.PerSource = tc.perSource
		row.RequestedSources = tc.requested
		e.st.seedRequestLog(row)
		reply, err := NewGetRecallRequestLogLogic(e.ctx(), e.svcCtx).GetRecallRequestLog(
			&rpc.GetRecallRequestLogReq{RequestId: "req-bad"})
		if err == nil {
			t.Fatalf("%s：脏数据被静默放过，响应看起来完全正常（%d 路统计）",
				tc.name, len(reply.GetEntry().GetPerSource()))
		}
		if reply != nil {
			t.Fatalf("%s：报错的同时还回了响应：%+v", tc.name, reply)
		}
	}
}

// 降级原因只在 degraded=1 时表达；未知 key 的回落行为也必须被钉住（新增 key 时这里会变红，
// 提醒同步 rpc 枚举，而不是让一次降级在响应里读成"未降级"）。
func TestGetRecallRequestLogDegradeFlagAndReason(t *testing.T) {
	e := newEnv(t)
	row := requestLogRow("req-1", "snap-1")
	row.Degraded = 0
	row.DegradeReason = model.DegradeReasonStoreUnavailable
	e.st.seedRequestLog(row)
	entry, err := NewGetRecallRequestLogLogic(e.ctx(), e.svcCtx).GetRecallRequestLog(
		&rpc.GetRecallRequestLogReq{RequestId: "req-1"})
	if err != nil {
		t.Fatalf("未降级行回放失败：%v", err)
	}
	if entry.GetEntry().GetDegraded() {
		t.Errorf("degraded=0 却回了 degraded=true")
	}
	if entry.GetEntry().GetReason() != rpc.DegradeReason_DEGRADE_REASON_UNSPECIFIED {
		t.Errorf("未降级时 reason=%v，期望 UNSPECIFIED（degrade_reason 列有残留也不能冒充降级原因）",
			entry.GetEntry().GetReason())
	}

	e2 := newEnv(t)
	row2 := requestLogRow("req-2", "snap-2")
	row2.Degraded = 1
	row2.DegradeReason = "brand_new_reason"
	e2.st.seedRequestLog(row2)
	entry2, err := NewGetRecallRequestLogLogic(e2.ctx(), e2.svcCtx).GetRecallRequestLog(
		&rpc.GetRecallRequestLogReq{RequestId: "req-2"})
	if err != nil {
		t.Fatalf("已降级行回放失败：%v", err)
	}
	if !entry2.GetEntry().GetDegraded() {
		t.Errorf("degraded=1 却回了 false")
	}
	if entry2.GetEntry().GetReason() != rpc.DegradeReason_DEGRADE_REASON_UNSPECIFIED {
		t.Errorf("未知 key 竟然映射成了 %v，期望 UNSPECIFIED", entry2.GetEntry().GetReason())
	}
}

func TestListRecallRequestLogsRejectsBeforeQuerying(t *testing.T) {
	cases := []struct {
		name string
		req  *rpc.ListRecallRequestLogsReq
		want error
	}{
		{"nil 请求", nil, model.ErrRequestRequired},
		{"scene 超列宽", &rpc.ListRecallRequestLogsReq{Scene: strings.Repeat("s", colScene+1)},
			model.ErrFieldTooLong},
		{"ps 超配置上限", &rpc.ListRecallRequestLogsReq{Ps: 101}, model.ErrLimitTooLarge},
		{"pn 负", &rpc.ListRecallRequestLogsReq{Pn: -1}, model.ErrPageTooDeep},
		{"深翻超硬上限", &rpc.ListRecallRequestLogsReq{Pn: 102, Ps: 100}, model.ErrPageTooDeep},
		{"mid 负", &rpc.ListRecallRequestLogsReq{Mid: -1}, model.ErrRequestLogRequired},
		{"from 负", &rpc.ListRecallRequestLogsReq{FromTime: -1}, model.ErrRequestLogRequired},
		{"to 负", &rpc.ListRecallRequestLogsReq{ToTime: -5}, model.ErrRequestLogRequired},
		{"窗口倒挂", &rpc.ListRecallRequestLogsReq{FromTime: 200, ToTime: 100}, model.ErrPageTooDeep},
	}
	for _, tc := range cases {
		e := newEnv(t)
		reply, err := NewListRecallRequestLogsLogic(e.ctx(), e.svcCtx).ListRecallRequestLogs(tc.req)
		requireErrIs(t, err, tc.want)
		if reply != nil {
			t.Fatalf("%s：拒绝却回了响应", tc.name)
		}
		if got := e.calls().total(); got != 0 {
			t.Fatalf("%s：拒绝前已触库 %d 次：%v", tc.name, got, e.calls().ops())
		}
	}
	// 门禁顺序：分页先于 mid —— 两个都坏时报分页错（调用方先修最省事的那个）。
	e := newEnv(t)
	_, err := NewListRecallRequestLogsLogic(e.ctx(), e.svcCtx).ListRecallRequestLogs(
		&rpc.ListRecallRequestLogsReq{Pn: 102, Ps: 100, Mid: -1})
	requireErrIs(t, err, model.ErrPageTooDeep)
	// ps 恰为配置上限 100 必须放行（model 的 500 不是本接口的口径）。
	e2 := newEnv(t)
	if _, err := NewListRecallRequestLogsLogic(e2.ctx(), e2.svcCtx).ListRecallRequestLogs(
		&rpc.ListRecallRequestLogsReq{Ps: 100}); err != nil {
		t.Fatalf("ps=100（配置上限）被拒：%v", err)
	}
}

// Count 与 List 必须共用同一个谓词、同一页参数：否则 total 与页内容不匹配、has_more 会说谎。
func TestListRecallRequestLogsUsesOnePredicateForCountAndList(t *testing.T) {
	e := newEnv(t)
	base := requestLogRow("req-1", "snap-1")
	base.Mid, base.Scene, base.Ctime = 777, "home.feed", 1700000300
	e.st.seedRequestLog(base)
	other := requestLogRow("req-2", "snap-2")
	other.Mid, other.Scene, other.Ctime = 777, "video.end", 1700000200
	e.st.seedRequestLog(other)
	otherMid := requestLogRow("req-3", "snap-3")
	otherMid.Mid, otherMid.Scene, otherMid.Ctime = 888, "home.feed", 1700000100
	e.st.seedRequestLog(otherMid)

	writesBefore := e.st.writes
	reply, err := NewListRecallRequestLogsLogic(e.ctx(), e.svcCtx).ListRecallRequestLogs(
		&rpc.ListRecallRequestLogsReq{Mid: 777, Scene: "home.feed", FromTime: 1700000000, ToTime: 1700000250, Pn: 1, Ps: 10})
	if err != nil {
		t.Fatalf("列取失败：%v", err)
	}
	assertOps(t, e.calls(), 0, "RequestLog.Count", "RequestLog.List")
	countOp, listOp := entryAt(e.calls(), 0), entryAt(e.calls(), 1)
	for _, kv := range []string{"mid=777", "scene=home.feed", "from=1700000000", "to=1700000250"} {
		if !strings.Contains(countOp, kv) || !strings.Contains(listOp, kv) {
			t.Fatalf("谓词没同时传给 Count 和 List：count=%q list=%q", countOp, listOp)
		}
	}
	if !strings.Contains(listOp, "off=0 lim=10") {
		t.Fatalf("页参数没下去：list=%q", listOp)
	}
	// 窗口只圈住 req-1（ctime 300 > to=250 被排除）：其余两行各自因 scene/mid 落选。
	if len(reply.GetEntries()) != 0 || reply.GetHasMore() {
		t.Fatalf("窗口边界判定错：entries=%d has_more=%t（to_time 是含端点，300 必须落在窗外）",
			len(reply.GetEntries()), reply.GetHasMore())
	}
	// 不限时间窗：两行命中、按 ctime DESC，第三行（mid=888）不出现。
	reply2, err := NewListRecallRequestLogsLogic(e.ctx(), e.svcCtx).ListRecallRequestLogs(
		&rpc.ListRecallRequestLogsReq{Mid: 777})
	if err != nil {
		t.Fatalf("仅按 mid 列取失败：%v", err)
	}
	if len(reply2.GetEntries()) != 2 || reply2.GetEntries()[0].GetRequestId() != "req-1" ||
		reply2.GetEntries()[1].GetRequestId() != "req-2" {
		t.Fatalf("排序或条数不符：%+v", reply2.GetEntries())
	}
	if reply2.GetHasMore() {
		t.Errorf("total=2、offset=0、取了 2 条，has_more 仍为真（会诱导 cron/页面无限翻页）")
	}
	// ps=2 只取一条时必须诚实报 has_more。
	reply3, err := NewListRecallRequestLogsLogic(e.ctx(), e.svcCtx).ListRecallRequestLogs(
		&rpc.ListRecallRequestLogsReq{Mid: 777, Ps: 1})
	if err != nil {
		t.Fatalf("ps=1 列取失败：%v", err)
	}
	if len(reply3.GetEntries()) != 1 || !reply3.GetHasMore() {
		t.Fatalf("has_more 判定错：entries=%d has_more=%t", len(reply3.GetEntries()), reply3.GetHasMore())
	}
	// ps<=0 用配置上限而不是 1（默认页大小取单页上限，见实现注释）。
	reply4, err := NewListRecallRequestLogsLogic(e.ctx(), e.svcCtx).ListRecallRequestLogs(
		&rpc.ListRecallRequestLogsReq{Mid: 777})
	if err != nil {
		t.Fatalf("默认分页失败：%v", err)
	}
	if len(reply4.GetEntries()) != 2 || reply4.GetHasMore() {
		t.Fatalf("默认分页结果错：entries=%d has_more=%t", len(reply4.GetEntries()), reply4.GetHasMore())
	}
	if !strings.Contains(entryAt(e.calls(), e.calls().total()-1), "lim=100") {
		t.Fatalf("ps=0 时下推的 limit=%q，期望配置 MaxRequestLogPage=100",
			entryAt(e.calls(), e.calls().total()-1))
	}
	if e.st.writes != writesBefore {
		t.Fatalf("审计列取写了库：writes %d -> %d", writesBefore, e.st.writes)
	}
}

// 一页里任何一行解析失败都必须整页失败：跳过那行等于抹掉一次降级记录。
func TestListRecallRequestLogsFailsWholePageOnOneBadRow(t *testing.T) {
	e := newEnv(t)
	good := requestLogRow("req-good", "snap-good")
	good.Mid, good.Ctime = 777, 1700000300
	e.st.seedRequestLog(good)
	bad := requestLogRow("req-bad", "snap-bad")
	bad.Mid, bad.Ctime = 777, 1700000200
	bad.PerSource = "not-json"
	e.st.seedRequestLog(bad)

	reply, err := NewListRecallRequestLogsLogic(e.ctx(), e.svcCtx).ListRecallRequestLogs(
		&rpc.ListRecallRequestLogsReq{Mid: 777})
	if err == nil {
		t.Fatalf("脏行被跳过、整页照常返回 %d 条（排障的人会以为那次请求没有降级记录）", len(reply.GetEntries()))
	}
	if reply != nil {
		t.Fatalf("整页失败却回了部分结果：%+v", reply.GetEntries())
	}
	// 注入存储故障同样整页失败。
	e2 := newEnv(t)
	g2 := requestLogRow("req-g", "snap-g")
	g2.Mid = 777
	e2.st.seedRequestLog(g2)
	e2.st.failOn("RequestLog.List", errors.New("deadlock"))
	if _, err := NewListRecallRequestLogsLogic(e2.ctx(), e2.svcCtx).ListRecallRequestLogs(
		&rpc.ListRecallRequestLogsReq{Mid: 777}); err == nil {
		t.Fatalf("List 的存储故障被吞掉了")
	}
	e3 := newEnv(t)
	e3.st.seedRequestLog(func() *model.RecallRequestLog { r := requestLogRow("req-c", "snap-c"); r.Mid = 777; return r }())
	e3.st.failOn("RequestLog.Count", errors.New("count timeout"))
	if _, err := NewListRecallRequestLogsLogic(e3.ctx(), e3.svcCtx).ListRecallRequestLogs(
		&rpc.ListRecallRequestLogsReq{Mid: 777}); err == nil {
		t.Fatalf("Count 的存储故障被吞掉了（total 拿不到却继续回页内容）")
	} else {
		assertOps(t, e3.calls(), 0, "RequestLog.Count")
	}
}

// 钉住本包的 nil-Repository 形态差异：GetRecallConfig / PrunePoolVersions 有守卫并回业务错误，
// 两个审计读方法没有守卫（`repo.RequestLog` 直接解引用 nil）=> panic。
// 这是 README「已知缺口」里那条不对称的真实形态：任何一方被修掉，本用例就该变红并要求同步文档。
func TestRequestLogReadsDivergeFromConfigReadsOnNilRepository(t *testing.T) {
	l := NewGetRecallConfigLogic(context.Background(), &svc.ServiceContext{Config: config.Config{}})
	if _, err := l.GetRecallConfig(&rpc.GetRecallConfigReq{Mid: 1}); err == nil {
		t.Fatalf("GetRecallConfig 的装配守卫消失了（请同步 README 缺口条目）")
	}

	get := NewGetRecallRequestLogLogic(context.Background(), &svc.ServiceContext{Config: config.Config{}})
	didPanic, _ := panicked(func() {
		_, _ = get.GetRecallRequestLog(&rpc.GetRecallRequestLogReq{RequestId: "req-x"})
	})
	if !didPanic {
		t.Fatalf("GetRecallRequestLog 现在有 nil-repository 守卫了：README 里的缺口条目需要删除或改写")
	}

	list := NewListRecallRequestLogsLogic(context.Background(), &svc.ServiceContext{Config: config.Config{}})
	didPanic, _ = panicked(func() {
		_, _ = list.ListRecallRequestLogs(&rpc.ListRecallRequestLogsReq{Mid: 1})
	})
	if !didPanic {
		t.Fatalf("ListRecallRequestLogs 现在有 nil-repository 守卫了：README 里的缺口条目需要删除或改写")
	}
}
