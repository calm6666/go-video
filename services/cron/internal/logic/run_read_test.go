package logic

// 读侧「执行记录 + 审计」单测：GetTaskRun、ListTaskRuns、ListTaskAudits。
//
// 这三个方法是排障时唯一能看到的证据链，钉的知识点：
//   1. 「查无此人」的表达方式在三个方法里并不相同（GetTaskRun 报 ErrRunNotFound，
//      GetCheckpoint/GetLease 回 found=false），这个不对称必须被测出来而不是靠猜；
//   2. 分页三件套（page_size / cursor / 时间窗）的校验顺序决定了一次误请求是
//      「零成本报错」还是「一次大表扫描」，用调用轨迹证明拒绝发生在 model 之前；
//   3. 游标方向必须与 ORDER BY 一致（run/audit 都是倒序，游标是 id < cursor）；
//   4. ListTaskAudits 不给时间窗时必须有兜底下界，否则审计大表的 count 无界；
//   5. 投影逐列搬运，且「库里存了但契约没透出」的列必须以哨兵形式钉住。

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"go-video/services/cron/model"
	"go-video/services/cron/rpc"
)

// fullRun 是一条每一列取值都互不相同的执行记录，用于投影逐列核对：
// 任意两列写串都会当场红。
func fullRun(taskKey string) *model.TaskRun {
	return &model.TaskRun{
		ID: 9001, TaskKey: taskKey, PlannedAt: 1700000001, Attempt: 2,
		TriggerType: model.TriggerTypeRetry, State: model.RunStateRunning,
		LeaseOwner: "worker-9", LeaseExpireAt: 1700000002, FenceToken: 1700000003,
		StartedAt: 1700000004, FinishedAt: 1700000005, DurationMs: 1700000006,
		ResultSummary: "scanned=42", LastError: "boom", NextRetryAt: 1700000007,
		Operator: "carol", TraceID: "trace-77", Ctime: 1700000008, Mtime: 1700000009,
	}
}

// assertRunProjected 把 rpc.RunRecord 的 18 个字段逐一按列位比对。
func assertRunProjected(t *testing.T, got *rpc.RunRecord, src *model.TaskRun) {
	t.Helper()
	if got == nil {
		t.Fatal("投影结果为 nil")
	}
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"run_id", got.GetRunId(), src.ID},
		{"task_key", got.GetTaskKey(), src.TaskKey},
		{"planned_at", got.GetPlannedAt(), src.PlannedAt},
		{"attempt", got.GetAttempt(), src.Attempt},
		{"trigger_type", int32(got.GetTriggerType()), src.TriggerType},
		{"state", int32(got.GetState()), src.State},
		{"lease_owner", got.GetLeaseOwner(), src.LeaseOwner},
		{"lease_expire_at", got.GetLeaseExpireAt(), src.LeaseExpireAt},
		{"fence_token", got.GetFenceToken(), src.FenceToken},
		{"started_at", got.GetStartedAt(), src.StartedAt},
		{"finished_at", got.GetFinishedAt(), src.FinishedAt},
		{"duration_ms", got.GetDurationMs(), src.DurationMs},
		{"result_summary", got.GetResultSummary(), src.ResultSummary},
		{"last_error", got.GetLastError(), src.LastError},
		{"next_retry_at", got.GetNextRetryAt(), src.NextRetryAt},
		{"trace_id", got.GetTraceId(), src.TraceID},
		{"ctime", got.GetCtime(), src.Ctime},
		{"mtime", got.GetMtime(), src.Mtime},
	} {
		if fmt.Sprint(c.got) != fmt.Sprint(c.want) {
			t.Errorf("列 %s：got=%v want=%v", c.name, c.got, c.want)
		}
	}
	if n, want := exportedFieldCount(rpc.RunRecord{}), 18; n != want {
		t.Errorf("rpc.RunRecord 导出字段数 = %d，本用例覆盖 %d 个；proto 加列必须补断言", n, want)
	}
}

// --- GetTaskRun ---

func TestGetTaskRunRejectsBlankIDAndFailsLoudlyOnMissingRow(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedRun(db, fullRun("rights.expire_scan"))

	for _, tc := range []struct {
		name string
		in   *rpc.GetTaskRunReq
	}{
		{"nil 请求", nil},
		{"run_id 为 0", &rpc.GetTaskRunReq{}},
		{"run_id 为负", &rpc.GetTaskRunReq{RunId: -9}},
	} {
		reply, err := NewGetTaskRunLogic(context.Background(), svcCtx).GetTaskRun(tc.in)
		if !errors.Is(err, model.ErrRunNotFound) {
			t.Errorf("%s: err = %v，期望 ErrRunNotFound", tc.name, err)
		}
		if reply != nil {
			t.Errorf("%s: 拒绝时不得回记录：%+v", tc.name, reply)
		}
	}
	if len(db.readCalls) != 0 {
		t.Errorf("非法 run_id 不得触库：%v", db.readCalls)
	}

	// 库里确实没有这一行：报 ErrTaskNotFound 语义的 ErrRunNotFound，且不给空对象。
	missing, err := NewGetTaskRunLogic(context.Background(), svcCtx).GetTaskRun(&rpc.GetTaskRunReq{RunId: 4242})
	if !errors.Is(err, model.ErrRunNotFound) {
		t.Errorf("不存在的 run_id 应报 ErrRunNotFound，得到 %v", err)
	}
	if missing != nil {
		t.Errorf("不存在时不得回空 RunRecord 假装查到了：%+v", missing)
	}
	if got, want := fmt.Sprint(db.readCalls), "[run:4242]"; got != want {
		t.Errorf("读轨迹 = %s，期望 %s", got, want)
	}

	// 对照：合法 id 必须读到，证明上面的失败不是整条路都断。
	ok, err := NewGetTaskRunLogic(context.Background(), svcCtx).GetTaskRun(&rpc.GetTaskRunReq{RunId: 9001})
	if err != nil || ok.GetRun().GetRunId() != 9001 {
		t.Fatalf("对照用例应成功：%+v / %v", ok, err)
	}
	// 不对称事实（登记 README）：执行记录没有 found=false 通道，租约/游标有。
	// 这里用 reflect 钉住「GetTaskRunReply 至今没有 Found 字段」，
	// 一旦契约补上（本用例会红）就说明三个读方法的缺失语义已经统一。
	if _, has := reflect.TypeOf(rpc.GetTaskRunReply{}).FieldByName("Found"); has {
		t.Errorf("GetTaskRunReply 出现了 Found 字段：请同步把缺失语义改成 found=false，并撤销 README 缺口")
	}
}

func TestGetTaskRunProjectsEveryColumn(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	src := fullRun("media.transcode_sweep")
	seedRun(db, src)

	reply, err := NewGetTaskRunLogic(context.Background(), svcCtx).
		GetTaskRun(&rpc.GetTaskRunReq{RunId: src.ID})
	if err != nil {
		t.Fatalf("GetTaskRun: %v", err)
	}
	assertRunProjected(t, reply.GetRun(), src)
	// 读侧零写副作用。
	if db.txRuns != 0 || len(db.audits) != 0 {
		t.Errorf("GetTaskRun 不该有写副作用：txRuns=%d audits=%d", db.txRuns, len(db.audits))
	}
}

// TestGetTaskRunHidesOperatorSentinel 钉住一条契约缺口：
// cron_task_run.operator 明确记了「谁手动触发/谁人工重试」（TriggerTask/RetryRun 都写），
// 但 rpc.RunRecord 没有对应列，于是 GetTaskRun / ListTaskRuns / TriggerTask / RetryRun
// 四个回包都答不出这个问题，只能绕道 ListTaskAudits。
// 修好那天（proto 加 operator 列）本用例会红。
func TestGetTaskRunHidesOperatorSentinel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	src := fullRun("report.daily_aggregation")
	seedRun(db, src)

	reply, err := NewGetTaskRunLogic(context.Background(), svcCtx).
		GetTaskRun(&rpc.GetTaskRunReq{RunId: src.ID})
	if err != nil {
		t.Fatalf("GetTaskRun: %v", err)
	}
	if _, has := reflect.TypeOf(rpc.RunRecord{}).FieldByName("Operator"); has {
		t.Fatal("rpc.RunRecord 已补 operator 列：请把本用例改成断言透出的操作人，并撤销 README 缺口")
	}
	if _, has := reflect.TypeOf(model.TaskRun{}).FieldByName("Operator"); !has {
		t.Fatal("model.TaskRun 的 operator 列不见了：投影缺口的前提已不成立")
	}
	// 库里存着、回包里没有：值必须仍然落在行上（否则缺口比描述的更小）。
	if got := reply.GetRun().GetLeaseOwner(); got != src.LeaseOwner {
		t.Errorf("对照：lease_owner 是透出的，得到 %q", got)
	}
	if stored := db.runs[src.ID]; stored.Operator != "carol" {
		t.Errorf("库内 operator 仍在（=%q），只是没有透出通道", stored.Operator)
	}
	if exportedFieldCount(rpc.RunRecord{}) != exportedFieldCount(model.TaskRun{})-1 {
		t.Errorf("RunRecord 与 TaskRun 的列数差不再是 1，缺口形状变了：%d vs %d",
			exportedFieldCount(rpc.RunRecord{}), exportedFieldCount(model.TaskRun{}))
	}
}

// --- ListTaskRuns ---

func TestListTaskRunsRejectsBadPagingBeforeTouchingModel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedRun(db, fullRun("rights.expire_scan"))
	logic := NewListTaskRunsLogic(context.Background(), svcCtx)

	if _, err := logic.ListTaskRuns(&rpc.ListTaskRunsReq{PageSize: 101}); !errors.Is(err, model.ErrInvalidPageLimit) {
		t.Errorf("page_size 越界应报 ErrInvalidPageLimit，得到 %v", err)
	}
	for _, bad := range []string{"abc", "0", "-7", "12e3"} {
		if _, err := logic.ListTaskRuns(&rpc.ListTaskRunsReq{Cursor: bad}); !errors.Is(err, model.ErrInvalidCursor) {
			t.Errorf("坏游标 %q: err = %v，期望 ErrInvalidCursor", bad, err)
		}
	}
	if _, err := logic.ListTaskRuns(&rpc.ListTaskRunsReq{
		PlannedFrom: 200, PlannedTo: 100}); !errors.Is(err, model.ErrInvalidTimeRange) {
		t.Errorf("区间倒置应报 ErrInvalidTimeRange，得到 %v", err)
	}
	// 校验顺序：page_size → cursor → 时间区间。三个都坏时报最先那个。
	if _, err := logic.ListTaskRuns(&rpc.ListTaskRunsReq{
		PageSize: 999, Cursor: "nope", PlannedFrom: 5, PlannedTo: 1}); !errors.Is(err, model.ErrInvalidPageLimit) {
		t.Errorf("page_size 必须最先被拒，得到 %v", err)
	}
	if _, err := logic.ListTaskRuns(&rpc.ListTaskRunsReq{
		Cursor: "nope", PlannedFrom: 5, PlannedTo: 1}); !errors.Is(err, model.ErrInvalidCursor) {
		t.Errorf("游标必须先于区间被拒，得到 %v", err)
	}
	if len(db.runCalls) != 0 {
		t.Errorf("非法分页参数不得触库：%+v", db.runCalls)
	}

	// 空白游标是「从头开始」，单边区间合法（planned_to=0 表示不设上界）：
	// 这两条必须放行，否则上面的「零调用」只是因为整条路都断了。
	if _, err := logic.ListTaskRuns(&rpc.ListTaskRunsReq{Cursor: "  ", PlannedFrom: 200}); err != nil {
		t.Errorf("空白游标 + 单边区间应放行，得到 %v", err)
	}
	if len(db.runCalls) != 1 {
		t.Errorf("合法请求必须真的下推一次查询：%+v", db.runCalls)
	}
	if got := db.runCalls[0]; got.cursorID != 0 || got.plannedFrom != 200 || got.plannedTo != 0 {
		t.Errorf("实参不对：%+v", got)
	}
}

func TestListTaskRunsPassesFiltersAndOrdersByPlannedDesc(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	base := int64(1700001000)
	// 同一计划时刻两条：id 大的必须排在前面（倒序遍历不能漏同秒行）。
	seedRun(db, &model.TaskRun{ID: 11, TaskKey: "a.task", PlannedAt: base, Attempt: 1,
		State: model.RunStateSucceeded})
	seedRun(db, &model.TaskRun{ID: 12, TaskKey: "a.task", PlannedAt: base, Attempt: 2,
		State: model.RunStateFailed})
	seedRun(db, &model.TaskRun{ID: 13, TaskKey: "a.task", PlannedAt: base + 100, Attempt: 1,
		State: model.RunStateSucceeded})
	seedRun(db, &model.TaskRun{ID: 14, TaskKey: "b.task", PlannedAt: base + 50, Attempt: 1,
		State: model.RunStateRunning})
	seedRun(db, &model.TaskRun{ID: 15, TaskKey: "a.task", PlannedAt: base - 100, Attempt: 1,
		State: model.RunStateSucceeded})

	reply, err := NewListTaskRunsLogic(context.Background(), svcCtx).
		ListTaskRuns(&rpc.ListTaskRunsReq{TaskKey: "a.task", PageSize: 50})
	if err != nil {
		t.Fatalf("ListTaskRuns: %v", err)
	}
	if got, want := fmt.Sprint(runIDsOf(reply)), "[13 12 11 15]"; got != want {
		t.Errorf("按 (planned_at, id) 倒序 = %s，期望 %s", got, want)
	}
	call := db.runCalls[len(db.runCalls)-1]
	if call.taskKey != "a.task" || call.state != model.RunStateUnspecified || call.limit != 50 ||
		call.plannedFrom != 0 || call.plannedTo != 0 || call.cursorID != 0 {
		t.Errorf("过滤实参未原样下推：%+v", call)
	}
	if reply.GetTotal() != 4 {
		t.Errorf("total = %d，期望 4", reply.GetTotal())
	}

	// 状态枚举按值过滤。
	failed, err := NewListTaskRunsLogic(context.Background(), svcCtx).
		ListTaskRuns(&rpc.ListTaskRunsReq{State: rpc.RunState_RUN_STATE_FAILED, PageSize: 50})
	if err != nil {
		t.Fatalf("按状态过滤：%v", err)
	}
	if got, want := fmt.Sprint(runIDsOf(failed)), "[12]"; got != want {
		t.Errorf("FAILED 集合 = %s，期望 %s", got, want)
	}
	// 时间窗两端都是闭区间。
	windowed, err := NewListTaskRunsLogic(context.Background(), svcCtx).
		ListTaskRuns(&rpc.ListTaskRunsReq{TaskKey: "a.task", PlannedFrom: base, PlannedTo: base + 50, PageSize: 50})
	if err != nil {
		t.Fatalf("按时间窗过滤：%v", err)
	}
	if got, want := fmt.Sprint(runIDsOf(windowed)), "[12 11]"; got != want {
		t.Errorf("窗口集合 = %s，期望 %s", got, want)
	}
	if windowed.GetTotal() != 2 {
		t.Errorf("total 必须与同一套过滤条件一致，得到 %d", windowed.GetTotal())
	}
}

func runIDsOf(reply *rpc.ListTaskRunsReply) []int64 {
	out := make([]int64, 0, len(reply.GetList()))
	for _, r := range reply.GetList() {
		out = append(out, r.RunId)
	}
	return out
}

func TestListTaskRunsPagesWithIDCursorAndStopsAtExhaustion(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	base := int64(1700002000)
	for i := int64(1); i <= 3; i++ {
		seedRun(db, &model.TaskRun{ID: i, TaskKey: "paging.task", PlannedAt: base + i,
			Attempt: 1, State: model.RunStateSucceeded})
	}
	logic := NewListTaskRunsLogic(context.Background(), svcCtx)

	first, err := logic.ListTaskRuns(&rpc.ListTaskRunsReq{TaskKey: "paging.task", PageSize: 2})
	if err != nil {
		t.Fatalf("第一页：%v", err)
	}
	if got, want := fmt.Sprint(runIDsOf(first)), "[3 2]"; got != want {
		t.Fatalf("第一页 = %s，期望 %s", got, want)
	}
	if !first.GetHasMore() || first.GetNextCursor() != "2" {
		t.Errorf("满页必须给出下一页游标：cursor=%q has_more=%t", first.GetNextCursor(), first.GetHasMore())
	}
	if first.GetTotal() != 3 {
		t.Errorf("total 是全量命中数而不是当页条数，得到 %d", first.GetTotal())
	}

	second, err := logic.ListTaskRuns(&rpc.ListTaskRunsReq{
		TaskKey: "paging.task", PageSize: 2, Cursor: first.GetNextCursor()})
	if err != nil {
		t.Fatalf("第二页：%v", err)
	}
	if got, want := fmt.Sprint(runIDsOf(second)), "[1]"; got != want {
		t.Fatalf("第二页 = %s，期望 %s", got, want)
	}
	if second.GetHasMore() || second.GetNextCursor() != "" {
		t.Errorf("不满页不得假装还有下一页：cursor=%q has_more=%t",
			second.GetNextCursor(), second.GetHasMore())
	}
	if db.runCalls[len(db.runCalls)-1].cursorID != 2 {
		t.Errorf("游标必须解成 id 下推给 model：%+v", db.runCalls[len(db.runCalls)-1])
	}

	// 走完再翻一次：空清单 + total 不变（不是「越翻越多」也不是报错）。
	third, err := logic.ListTaskRuns(&rpc.ListTaskRunsReq{TaskKey: "paging.task", PageSize: 2, Cursor: "1"})
	if err != nil {
		t.Fatalf("末页之后再翻：%v", err)
	}
	if len(third.GetList()) != 0 || third.GetTotal() != 3 || third.GetHasMore() {
		t.Errorf("耗尽后应回空清单且 total 不变：%+v", third)
	}

	// page_size=0 用配置的 DefaultPageSize(20)，不是「全表」。
	if _, err := logic.ListTaskRuns(&rpc.ListTaskRunsReq{}); err != nil {
		t.Fatalf("默认分页：%v", err)
	}
	if got := db.runCalls[len(db.runCalls)-1].limit; got != 20 {
		t.Errorf("page_size=0 应兜到 config.Task.DefaultPageSize(20)，得到 %d", got)
	}
}

// TestListTaskRunsDoesNotTrimTaskKeySentinel 钉住读写两侧的口径不一致：
// 写侧（TriggerTask/RetryRun/注册/状态迁移）与 GetCheckpoint/GetLease 都会先清
// task_key 两侧空白再查库，而 ListTaskRuns / ListTaskAudits / ListLeases /
// ListCheckpoints 把 in.Task_key 原样塞进 WHERE，于是「带一个空格的过滤条件」
// 静默返回空清单 + total=0，看起来像「这个任务没有执行记录」。
func TestListTaskRunsDoesNotTrimTaskKeySentinel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedRun(db, &model.TaskRun{ID: 5, TaskKey: "trim.check", PlannedAt: 1700003000,
		Attempt: 1, State: model.RunStateSucceeded})

	plain, err := NewListTaskRunsLogic(context.Background(), svcCtx).
		ListTaskRuns(&rpc.ListTaskRunsReq{TaskKey: "trim.check"})
	if err != nil || len(plain.GetList()) != 1 {
		t.Fatalf("精确匹配应有 1 条：%+v / %v", plain, err)
	}
	padded, err := NewListTaskRunsLogic(context.Background(), svcCtx).
		ListTaskRuns(&rpc.ListTaskRunsReq{TaskKey: " trim.check "})
	if err != nil {
		t.Fatalf("带空格的过滤条件不是错误：%v", err)
	}
	if len(padded.GetList()) != 0 || padded.GetTotal() != 0 {
		t.Errorf("当前现实：带空格的 task_key 查不到任何东西（got=%d/%d），修复后请改成断言与精确匹配一致",
			len(padded.GetList()), padded.GetTotal())
	}
	if db.runCalls[len(db.runCalls)-1].taskKey != " trim.check " {
		t.Errorf("现实是本样下推：%q", db.runCalls[len(db.runCalls)-1].taskKey)
	}

	// 判别性对照：同一份数据、同一个 key，GetCheckpoint 侧就会先清空白。
	seedCursor(db, "trim.check", "", 12, "")
	found, err := NewGetCheckpointLogic(context.Background(), svcCtx).
		GetCheckpoint(&rpc.GetCheckpointReq{TaskKey: " trim.check "})
	if err != nil || !found.GetFound() {
		t.Fatalf("GetCheckpoint 必须清理 task_key 后再查库：%+v / %v", found, err)
	}
}

// --- ListTaskAudits ---

// seedAudit 铺一条审计（静默写入）。
func seedAudit(db *fakeDB, id int64, taskKey, action, operator string, ctime int64) *model.TaskAudit {
	return seedAuditRow(db, &model.TaskAudit{
		ID: id, TaskKey: taskKey, Action: action, FromState: "enabled", ToState: "paused",
		Operator: operator, Detail: `{"reason":"x"}`, TraceID: "tr-" + operator, Ctime: ctime,
	})
}

func TestListTaskAuditsRejectsBadPagingBeforeTouchingModel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedAudit(db, 1, "a.task", model.AuditActionPause, "alice", fakeNow())
	logic := NewListTaskAuditsLogic(context.Background(), svcCtx)

	if _, err := logic.ListTaskAudits(&rpc.ListTaskAuditsReq{PageSize: 250}); !errors.Is(err, model.ErrInvalidPageLimit) {
		t.Errorf("page_size 越界应报 ErrInvalidPageLimit，得到 %v", err)
	}
	if _, err := logic.ListTaskAudits(&rpc.ListTaskAuditsReq{Cursor: "12abc"}); !errors.Is(err, model.ErrInvalidCursor) {
		t.Errorf("坏游标应报 ErrInvalidCursor，得到 %v", err)
	}
	if _, err := logic.ListTaskAudits(&rpc.ListTaskAuditsReq{
		CtimeFrom: 900, CtimeTo: 800}); !errors.Is(err, model.ErrInvalidTimeRange) {
		t.Errorf("区间倒置应报 ErrInvalidTimeRange，得到 %v", err)
	}
	// 顺序：page_size → cursor → 区间。
	if _, err := logic.ListTaskAudits(&rpc.ListTaskAuditsReq{
		PageSize: 250, Cursor: "x", CtimeFrom: 9, CtimeTo: 1}); !errors.Is(err, model.ErrInvalidPageLimit) {
		t.Errorf("page_size 必须最先被拒，得到 %v", err)
	}
	if _, err := logic.ListTaskAudits(&rpc.ListTaskAuditsReq{
		Cursor: "x", CtimeFrom: 9, CtimeTo: 1}); !errors.Is(err, model.ErrInvalidCursor) {
		t.Errorf("游标必须先于区间被拒，得到 %v", err)
	}
	if len(db.auditCalls) != 0 {
		t.Errorf("非法分页参数不得触库：%+v", db.auditCalls)
	}
	// 空白游标与单边区间都是合法输入（不是「非法即报错」的对象）。
	if _, err := logic.ListTaskAudits(&rpc.ListTaskAuditsReq{Cursor: " ", CtimeTo: 900}); err != nil {
		t.Errorf("空白游标 + 单边区间应放行，得到 %v", err)
	}
}

func TestListTaskAuditsDefaultsToRetentionWindow(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	const day int64 = 86400
	now := fakeNow()
	seedAudit(db, 1, "a.task", model.AuditActionUpdate, "alice", now-10*day)
	seedAudit(db, 2, "a.task", model.AuditActionPause, "bob", now-200*day)

	// 两端都不给 → 服务端按留存期兜下界（配置未给时兜 180 天）。
	reply, err := NewListTaskAuditsLogic(context.Background(), svcCtx).ListTaskAudits(nil)
	if err != nil {
		t.Fatalf("ListTaskAudits: %v", err)
	}
	call := db.auditCalls[len(db.auditCalls)-1]
	if call.ctimeTo != 0 {
		t.Errorf("默认窗口只补下界，上界仍不设限，得到 %+v", call)
	}
	if call.ctimeFrom < now-180*day-5 || call.ctimeFrom > now-180*day+5 {
		t.Errorf("缺省留存期应为 180 天，得到 from=%d（now=%d）", call.ctimeFrom, now)
	}
	if got, want := fmt.Sprint(auditIDs(reply)), "[1]"; got != want {
		t.Errorf("超出留存期的行不得出现在清单里：%s（期望 %s）", got, want)
	}
	if reply.GetTotal() != 1 {
		t.Errorf("total 必须与同一套窗口条件一致，得到 %d", reply.GetTotal())
	}

	// 下界跟着配置走：改 7 天后，10 天前那条也查不到。
	svcCtx.Config.Task.AuditRetentionDays = 7
	if _, err := NewListTaskAuditsLogic(context.Background(), svcCtx).
		ListTaskAudits(&rpc.ListTaskAuditsReq{}); err != nil {
		t.Fatalf("留存期 7 天：%v", err)
	}
	call = db.auditCalls[len(db.auditCalls)-1]
	if call.ctimeFrom < now-7*day-5 || call.ctimeFrom > now-7*day+5 {
		t.Errorf("from 应随 config.Task.AuditRetentionDays 收敛，得到 %d", call.ctimeFrom)
	}

	// 只要给了任一端就不再兜底：planned 场景里「只对上界设限」是合法诉求。
	svcCtx.Config.Task.AuditRetentionDays = 0
	withTo, err := NewListTaskAuditsLogic(context.Background(), svcCtx).
		ListTaskAudits(&rpc.ListTaskAuditsReq{CtimeTo: now})
	if err != nil {
		t.Fatalf("只给上界：%v", err)
	}
	if got, want := fmt.Sprint(auditIDs(withTo)), "[2 1]"; got != want {
		t.Errorf("给了上界时不得偷偷加下界：%s（期望 %s）", got, want)
	}
	if got := db.auditCalls[len(db.auditCalls)-1].ctimeFrom; got != 0 {
		t.Errorf("上界存在时下界必须保持未设，得到 %d", got)
	}
}

func auditIDs(reply *rpc.ListTaskAuditsReply) []int64 {
	out := make([]int64, 0, len(reply.GetList()))
	for _, a := range reply.GetList() {
		out = append(out, a.Id)
	}
	return out
}

func TestListTaskAuditsPagesWithIDCursorDescAndFilters(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	seedAudit(db, 3, "x.task", model.AuditActionPause, "alice", now-3)
	seedAudit(db, 1, "x.task", model.AuditActionUpdate, "bob", now-1)
	seedAudit(db, 2, "y.task", model.AuditActionPause, "cara", now-2)
	logic := NewListTaskAuditsLogic(context.Background(), svcCtx)

	all, err := logic.ListTaskAudits(&rpc.ListTaskAuditsReq{CtimeFrom: now - 100, PageSize: 2})
	if err != nil {
		t.Fatalf("第一页：%v", err)
	}
	// 审计是只追加轨迹，倒序即「最新在前」，游标条件为 id < cursor。
	if got, want := fmt.Sprint(auditIDs(all)), "[3 2]"; got != want {
		t.Fatalf("最新两条 = %s，期望 %s", got, want)
	}
	if all.GetNextCursor() != "2" || !all.GetHasMore() || all.GetTotal() != 3 {
		t.Errorf("游标/总数不对：cursor=%q has_more=%t total=%d",
			all.GetNextCursor(), all.GetHasMore(), all.GetTotal())
	}
	next, err := logic.ListTaskAudits(&rpc.ListTaskAuditsReq{
		CtimeFrom: now - 100, PageSize: 2, Cursor: all.GetNextCursor()})
	if err != nil {
		t.Fatalf("第二页：%v", err)
	}
	if got, want := fmt.Sprint(auditIDs(next)), "[1]"; got != want {
		t.Errorf("第二页 = %s，期望 %s", got, want)
	}
	if next.GetHasMore() || next.GetNextCursor() != "" {
		t.Errorf("不满页不得给出游标：%q", next.GetNextCursor())
	}
	if db.auditCalls[len(db.auditCalls)-1].cursorID != 2 {
		t.Errorf("游标必须解成 id 下推：%+v", db.auditCalls[len(db.auditCalls)-1])
	}

	// task_key / action 两个过滤维度独立生效，且 total 与当页同源。
	byAction, err := logic.ListTaskAudits(&rpc.ListTaskAuditsReq{
		CtimeFrom: now - 100, Action: model.AuditActionPause})
	if err != nil {
		t.Fatalf("按动作过滤：%v", err)
	}
	if got, want := fmt.Sprint(auditIDs(byAction)), "[3 2]"; got != want {
		t.Errorf("pause 集合 = %s，期望 %s", got, want)
	}
	byKey, err := logic.ListTaskAudits(&rpc.ListTaskAuditsReq{
		CtimeFrom: now - 100, TaskKey: "y.task"})
	if err != nil {
		t.Fatalf("按任务过滤：%v", err)
	}
	if got, want := fmt.Sprint(auditIDs(byKey)), "[2]"; got != want {
		t.Errorf("y.task 集合 = %s，期望 %s", got, want)
	}
	if byKey.GetTotal() != 1 {
		t.Errorf("total 要跟着过滤条件收敛，得到 %d", byKey.GetTotal())
	}
}

func TestListTaskAuditsProjectsEveryColumn(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	src := &model.TaskAudit{
		ID: 61, TaskKey: "proj.task", Action: model.AuditActionReplay,
		FromState: "running", ToState: "canceled", Operator: "dave",
		Detail: `{"reason":"上游重跑"}`, TraceID: "trace-61", Ctime: now - 5,
	}
	seedAuditRow(db, src)

	reply, err := NewListTaskAuditsLogic(context.Background(), svcCtx).
		ListTaskAudits(&rpc.ListTaskAuditsReq{TaskKey: src.TaskKey, CtimeFrom: now - 3600})
	if err != nil {
		t.Fatalf("ListTaskAudits: %v", err)
	}
	if len(reply.GetList()) != 1 {
		t.Fatalf("应恰好 1 条：%+v", reply.GetList())
	}
	got := reply.GetList()[0]
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"id", got.GetId(), src.ID},
		{"task_key", got.GetTaskKey(), src.TaskKey},
		{"action", got.GetAction(), src.Action},
		{"from_state", got.GetFromState(), src.FromState},
		{"to_state", got.GetToState(), src.ToState},
		{"operator", got.GetOperator(), src.Operator},
		{"detail", got.GetDetail(), src.Detail},
		{"trace_id", got.GetTraceId(), src.TraceID},
		{"ctime", got.GetCtime(), src.Ctime},
	} {
		if fmt.Sprint(c.got) != fmt.Sprint(c.want) {
			t.Errorf("列 %s：got=%v want=%v", c.name, c.got, c.want)
		}
	}
	if n, want := exportedFieldCount(rpc.TaskAudit{}), 9; n != want {
		t.Errorf("rpc.TaskAudit 导出字段数 = %d，本用例覆盖 %d 个；proto 加列必须补断言", n, want)
	}
	// 审计是只追加的：读接口一行都不能改、不能删。
	if db.txRuns != 0 || len(db.audits) != 1 || *db.audits[0] != *src {
		t.Errorf("ListTaskAudits 不得改动审计行：txRuns=%d rows=%+v", db.txRuns, db.audits)
	}
}
