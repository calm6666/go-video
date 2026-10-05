package logic

// 写侧「排队 / 追加 attempt / 到期扫描」单测：TriggerTask、RetryRun、ListDueTasks。
//
// 这三个方法共同决定「一次执行到底会不会跑第二遍」，因此本文件钉的是可验证的硬事实：
//   1. 入参非法必须在**第一次读库之前**就被拒（readCalls 轨迹为空即证据，
//      而不是只比错误类型——顺序错了对用户是「一次误点跑出两遍」）；
//   2. 排队与审计必须在同一事务里同生共死（auditErr 注入后库内不留半条执行记录）；
//   3. attempt 只能由服务端取号，且 TriggerTask（事务外取号）与 RetryRun（事务内取号）
//      的差别必须被测出来：maxAttemptStaleBy 这个开关只应打中前者；
//   4. ListDueTasks 的「到期」= 状态 ENABLED 且指针已过期，两个约束缺一不可，
//      且它一行都不能写。
//
// 已知不成立的语义没有被跳过，而是以 *Sentinel* 用例钉成「当前真相」，
// 并在 services/cron/README.md「已知缺口」登记（修复后这些用例会红，逼着人来改断言）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/cron/model"
	"go-video/services/cron/rpc"
)

const (
	triggerTaskKey = "report.daily_aggregation"
	triggerHandler = "report.daily"
)

// seedTriggerable 铺一条「可被手动触发/人工重试」的任务定义，返回入库后的行。
func seedTriggerable(db *fakeDB, taskKey, handler string, state int32) *model.TaskDefinition {
	return seedDefinition(db, &model.TaskDefinition{
		TaskKey: taskKey, Name: taskKey, Handler: handler, TaskGroup: "report",
		ScheduleType: model.ScheduleTypeCron, CronExpr: "0 1 * * *", Timezone: "UTC",
		TimeoutSeconds: 120, MaxAttempts: 3, RetryBaseSeconds: 30, RetryMaxSeconds: 600,
		ConcurrencyLimit: 1, LeaseTTLSeconds: 300, State: state, Version: 1,
		NextFireAt: fakeNow() + 3600,
	})
}

// detailOf 解开审计 detail。用 map 而不是字符串比对：auditDetail 的键序属于实现细节，
// 但「哪些事实被记下来」是契约，必须逐字段断言。
func detailOf(t *testing.T, detail string) map[string]any {
	t.Helper()
	if strings.TrimSpace(detail) == "" {
		t.Fatalf("审计 detail 为空，无法回答「这次改了什么」")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(detail), &m); err != nil {
		t.Fatalf("审计 detail 不是合法 JSON：%q: %v", detail, err)
	}
	return m
}

// num 取 JSON 数字字段（encoding/json 一律解成 float64）。
func num(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("审计 detail 缺字段 %q：%v", key, m)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("审计 detail 字段 %q 不是数字：%#v", key, v)
	}
	return f
}

// str 取 JSON 字符串字段。
func str(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("审计 detail 缺字段 %q：%v", key, m)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("审计 detail 字段 %q 不是字符串：%#v", key, v)
	}
	return s
}

// lastAudit 取最后一条审计（多条时按写入顺序）。
func lastAudit(t *testing.T, db *fakeDB) *model.TaskAudit {
	t.Helper()
	if len(db.audits) == 0 {
		t.Fatal("库里没有任何审计")
	}
	return db.audits[len(db.audits)-1]
}

// runsByAttempt 把库内执行记录按 attempt 索引，便于断言「追加了哪一号」。
func runsByAttempt(db *fakeDB) map[int32]*model.TaskRun {
	out := map[int32]*model.TaskRun{}
	for _, r := range db.runs {
		out[r.Attempt] = r
	}
	return out
}

// --- TriggerTask ---

func TestTriggerTaskRejectsBeforeTouchingDatabase(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, triggerHandler)

	for _, tc := range []struct {
		name string
		in   *rpc.TriggerTaskReq
		want error
	}{
		{"nil 请求", nil, model.ErrTaskKeyEmpty},
		// 幂等键检查排在 task_key 之前：两个都缺时先报幂等键。
		{"缺幂等键", &rpc.TriggerTaskReq{TaskKey: "ghost.task"}, model.ErrIdempotencyKeyEmpty},
		{"幂等键只有空白", &rpc.TriggerTaskReq{TaskKey: "ghost.task", IdempotencyKey: " \t "},
			model.ErrIdempotencyKeyEmpty},
		{"缺 task_key", &rpc.TriggerTaskReq{IdempotencyKey: "ik-1"}, model.ErrTaskKeyEmpty},
		{"params 覆盖位被显式拒绝", &rpc.TriggerTaskReq{
			IdempotencyKey: "ik-1", TaskKey: "ghost.task", Params: `{"a":1}`}, model.ErrRunParamsUnsupported},
		// params 检查排在 planned_at 之前：两个都非法时先报 params。
		{"params 优先于负 planned_at", &rpc.TriggerTaskReq{
			IdempotencyKey: "ik-1", TaskKey: "ghost.task", Params: "x", PlannedAt: -1},
			model.ErrRunParamsUnsupported},
		{"负 planned_at", &rpc.TriggerTaskReq{
			IdempotencyKey: "ik-1", TaskKey: "ghost.task", PlannedAt: -5}, model.ErrPlannedAtRequired},
	} {
		reply, err := NewTriggerTaskLogic(context.Background(), svcCtx).TriggerTask(tc.in)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v，期望 %v", tc.name, err, tc.want)
		}
		if reply != nil {
			t.Errorf("%s: 拒绝时不得回执行记录：%+v", tc.name, reply)
		}
	}
	// 判别性证据：以上每一次拒绝都没读过库，也没开过事务。
	if len(db.readCalls) != 0 {
		t.Errorf("非法请求不得触库，实际读库轨迹：%v", db.readCalls)
	}
	if db.txRuns != 0 || len(db.runs) != 0 || len(db.audits) != 0 {
		t.Errorf("非法请求不得有写副作用：txRuns=%d runs=%d audits=%d",
			db.txRuns, len(db.runs), len(db.audits))
	}

	// 反面对照：同一上下文、同一 handler，合法请求必须真的落库，
	// 否则上面那串「零调用」只是因为整条路都断了。
	seedTriggerable(db, triggerTaskKey, triggerHandler, model.TaskStateEnabled)
	ok, err := NewTriggerTaskLogic(context.Background(), svcCtx).
		TriggerTask(&rpc.TriggerTaskReq{IdempotencyKey: "ik-1", TaskKey: triggerTaskKey})
	if err != nil || !ok.GetCreated() {
		t.Fatalf("对照用例应成功排队，得到 %+v / %v", ok, err)
	}
	if len(db.readCalls) == 0 || db.txRuns == 0 {
		t.Errorf("对照用例应真的读库并开事务：readCalls=%v txRuns=%d", db.readCalls, db.txRuns)
	}
}

func TestTriggerTaskParamsBlankIsTreatedAsUnset(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, triggerHandler)
	seedTriggerable(db, triggerTaskKey, triggerHandler, model.TaskStateEnabled)

	// params 的判定是 TrimSpace 后判空：全空白等于「没传」，不该被 ErrRunParamsUnsupported 挡回。
	reply, err := NewTriggerTaskLogic(context.Background(), svcCtx).
		TriggerTask(&rpc.TriggerTaskReq{IdempotencyKey: "ik-blank", TaskKey: triggerTaskKey, Params: "  "})
	if err != nil {
		t.Fatalf("params 全空白应视为未覆盖，得到 %v", err)
	}
	if reply.GetRun().GetRunId() <= 0 {
		t.Errorf("应回一条带主键的执行记录：%+v", reply)
	}
}

func TestTriggerTaskGateOrderDefinitionStateThenHandlerThenNumbering(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, triggerHandler)
	logic := NewTriggerTaskLogic(context.Background(), svcCtx)

	// 定义不存在 → ErrTaskNotFound，且只发生一次定义读。
	if _, err := logic.TriggerTask(&rpc.TriggerTaskReq{
		IdempotencyKey: "ik-1", TaskKey: "ghost.task"}); !errors.Is(err, model.ErrTaskNotFound) {
		t.Errorf("任务不存在应报 ErrTaskNotFound，得到 %v", err)
	}
	if got, want := fmt.Sprint(db.readCalls), `[definition:ghost.task]`; got != want {
		t.Fatalf("不存在的任务只应有一次定义读，得到 %s（期望 %s）", got, want)
	}

	// DISABLED 是终态：连「取 attempt 号」都不该发生。
	db.readCalls = nil
	seedTriggerable(db, "stopped.task", triggerHandler, model.TaskStateDisabled)
	_, err := logic.TriggerTask(&rpc.TriggerTaskReq{IdempotencyKey: "ik-2", TaskKey: "stopped.task"})
	if !errors.Is(err, model.ErrStateTransition) {
		t.Errorf("已停用任务应报 ErrStateTransition，得到 %v", err)
	}
	if got, want := fmt.Sprint(db.readCalls), `[definition:stopped.task]`; got != want {
		t.Errorf("停用判定必须在取号之前，实际轨迹 %s（期望 %s）", got, want)
	}

	// handler 未注册：同样必须在取号之前拦住，否则会留下无主的 PENDING 行。
	db.readCalls = nil
	seedTriggerable(db, "orphan.task", "handler.not.in.this.process", model.TaskStateEnabled)
	_, err = logic.TriggerTask(&rpc.TriggerTaskReq{IdempotencyKey: "ik-3", TaskKey: "orphan.task"})
	if !errors.Is(err, model.ErrHandlerNotRegistered) {
		t.Errorf("未注册 handler 应报 ErrHandlerNotRegistered，得到 %v", err)
	}
	if got, want := fmt.Sprint(db.readCalls), `[definition:orphan.task]`; got != want {
		t.Errorf("handler 闸门必须在取号之前，实际轨迹 %s（期望 %s）", got, want)
	}
	if len(db.runs) != 0 || db.txRuns != 0 {
		t.Errorf("闸门拦下后不得有排队痕迹：runs=%d txRuns=%d", len(db.runs), db.txRuns)
	}

	// PAUSED 允许手动触发（运营要能「先验证一次再恢复调度」）。
	if _, err := logic.TriggerTask(&rpc.TriggerTaskReq{IdempotencyKey: "ik-4", TaskKey: "stopped.task"}); err == nil {
		t.Errorf("对照：DISABLED 必须始终被拒")
	}
	seedTriggerable(db, "paused.task", triggerHandler, model.TaskStatePaused)
	reply, err := logic.TriggerTask(&rpc.TriggerTaskReq{IdempotencyKey: "ik-5", TaskKey: "paused.task"})
	if err != nil {
		t.Fatalf("PAUSED 任务应允许手动触发，得到 %v", err)
	}
	if !reply.GetCreated() {
		t.Errorf("首次排队应报 created=true：%+v", reply)
	}
	if got := str(t, detailOf(t, onlyAudit(t, db, model.AuditActionTrigger).Detail), "task_state"); got != "paused" {
		t.Errorf("审计要记下触发时的任务状态，得到 %q", got)
	}
}

func TestTriggerTaskQueuesPendingRunAndAuditsAtomically(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, triggerHandler)
	def := seedTriggerable(db, triggerTaskKey, triggerHandler, model.TaskStateEnabled)

	before := fakeNow()
	reply, err := NewTriggerTaskLogic(context.Background(), svcCtx).
		TriggerTask(&rpc.TriggerTaskReq{
			IdempotencyKey: "ik-9527", TaskKey: triggerTaskKey, Operator: "alice", TraceId: "trace-1"})
	after := fakeNow()
	if err != nil {
		t.Fatalf("TriggerTask: %v", err)
	}
	if !reply.GetCreated() {
		t.Errorf("首次触发必须 created=true：%+v", reply)
	}
	if reply.GetRun() == nil {
		t.Fatal("回包必须带执行记录，不能只说「成功」")
	}

	stored := onlyRun(t, db)
	if stored.State != model.RunStatePending {
		t.Errorf("触发只排队：state = %s，期望 pending", model.RunStateName(stored.State))
	}
	if stored.TriggerType != model.TriggerTypeManual {
		t.Errorf("trigger_type = %d，期望 manual(%d)", stored.TriggerType, model.TriggerTypeManual)
	}
	if stored.PlannedAt < before || stored.PlannedAt > after {
		t.Errorf("planned_at=0 必须由服务端时钟决定，得到 %d（窗口 [%d,%d]）", stored.PlannedAt, before, after)
	}
	if stored.Attempt != 1 {
		t.Errorf("首次触发 attempt = %d，期望 1", stored.Attempt)
	}
	if stored.Operator != "alice" || stored.TraceID != "trace-1" {
		t.Errorf("operator/trace_id 未落到行上：%q / %q", stored.Operator, stored.TraceID)
	}
	// 排队不等于领取：租约三件套必须还空着，否则 worker 再也抢不到。
	if stored.LeaseOwner != "" || stored.FenceToken != 0 || stored.LeaseExpireAt != 0 {
		t.Errorf("PENDING 行不得带租约痕迹：%+v", stored)
	}
	if len(db.leases) != 0 {
		t.Errorf("TriggerTask 不得写 cron_task_lease，得到 %d 条", len(db.leases))
	}

	// 回包与库内行必须同源（投影不得搬运到一半）。
	if reply.GetRun().GetRunId() != stored.ID || reply.GetRun().GetPlannedAt() != stored.PlannedAt {
		t.Errorf("回包与落库行不一致：reply=%+v stored=%+v", reply.GetRun(), stored)
	}
	if reply.GetRun().GetState() != rpc.RunState_RUN_STATE_PENDING {
		t.Errorf("回包 state = %v，期望 PENDING", reply.GetRun().GetState())
	}

	// 审计与排队同事务：一次触发 = 一条 trigger 审计。
	if db.txRuns != 1 {
		t.Errorf("排队 + 审计应只开 1 个事务，得到 %d", db.txRuns)
	}
	audit := onlyAudit(t, db, model.AuditActionTrigger)
	if audit.FromState != "" {
		t.Errorf("首次排队的 from_state 应为空（没有前序状态可写），得到 %q", audit.FromState)
	}
	if audit.ToState != model.RunStateName(model.RunStatePending) {
		t.Errorf("to_state = %q，期望 pending", audit.ToState)
	}
	if audit.Operator != "alice" || audit.TaskKey != triggerTaskKey {
		t.Errorf("审计归属不对：operator=%q task_key=%q", audit.Operator, audit.TaskKey)
	}
	if audit.Ctime < before || audit.Ctime > after {
		t.Errorf("审计 ctime 用服务端时钟，得到 %d（窗口 [%d,%d]）", audit.Ctime, before, after)
	}
	d := detailOf(t, audit.Detail)
	if num(t, d, "run_id") != float64(stored.ID) || num(t, d, "planned_at") != float64(stored.PlannedAt) ||
		num(t, d, "attempt") != 1 {
		t.Errorf("审计 detail 的执行身份字段不对：%v", d)
	}
	if str(t, d, "handler") != triggerHandler || str(t, d, "task_state") != "enabled" ||
		str(t, d, "idempotency_key") != "ik-9527" {
		t.Errorf("审计 detail 的上下文字段不对：%v", d)
	}
	if v, ok := d["explicit_planned"]; !ok || v != false {
		t.Errorf("planned_at=0 时 explicit_planned 必须是 false，得到 %#v", v)
	}

	// 只排队不动调度：定义行的指针/版本/状态一个字节都不该变。
	cur := db.defs[triggerTaskKey]
	if cur.NextFireAt != def.NextFireAt || cur.Version != def.Version || cur.State != def.State ||
		cur.LastFireAt != def.LastFireAt {
		t.Errorf("触发不得推进调度指针或版本：before=%+v after=%+v", def, cur)
	}
}

func TestTriggerTaskExplicitPlannedAtAndServerSideNumbering(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, triggerHandler)
	seedTriggerable(db, triggerTaskKey, triggerHandler, model.TaskStateEnabled)
	planned := int64(1700000000)
	for _, a := range []int32{1, 2, 3} {
		seedRun(db, &model.TaskRun{
			TaskKey: triggerTaskKey, PlannedAt: planned, Attempt: a,
			State: model.RunStateSucceeded, TriggerType: model.TriggerTypeScheduled,
		})
	}

	reply, err := NewTriggerTaskLogic(context.Background(), svcCtx).
		TriggerTask(&rpc.TriggerTaskReq{
			IdempotencyKey: "ik-backfill", TaskKey: triggerTaskKey, PlannedAt: planned, TraceId: "t-2"})
	if err != nil {
		t.Fatalf("补跑历史计划时刻： %v", err)
	}
	if reply.GetRun().GetPlannedAt() != planned {
		t.Errorf("显式 planned_at 必须原样使用，得到 %d", reply.GetRun().GetPlannedAt())
	}
	if got := reply.GetRun().GetAttempt(); got != 4 {
		t.Errorf("attempt 由服务端在同一计划时刻上续号，期望 4，得到 %d", got)
	}
	// 取号读的就是即将写入的那个计划时刻。
	if last := db.readCalls[len(db.readCalls)-1]; last != fmt.Sprintf("max_attempt:%s@%d", triggerTaskKey, planned) {
		t.Errorf("取号轨迹不对：%v", db.readCalls)
	}
	if v, ok := detailOf(t, onlyAudit(t, db, model.AuditActionTrigger).Detail)["explicit_planned"]; !ok || v != true {
		t.Errorf("显式补跑必须在审计里标出来，得到 %#v", v)
	}

	// operator 缺省退化为实例标识：审计里必须永远能回答「谁干的」。
	reply2, err := NewTriggerTaskLogic(context.Background(), svcCtx).
		TriggerTask(&rpc.TriggerTaskReq{IdempotencyKey: "ik-no-operator", TaskKey: triggerTaskKey})
	if err != nil {
		t.Fatalf("未带 operator 的触发： %v", err)
	}
	if reply2.GetRun().GetLeaseOwner() != "" {
		t.Errorf("PENDING 行不该有 lease_owner：%+v", reply2.GetRun())
	}
	if got := lastAudit(t, db).Operator; got != svcCtx.WorkerID() {
		t.Errorf("operator 缺省应退化为实例标识，得到 %q 期望 %q", got, svcCtx.WorkerID())
	}
	if len(db.audits) != 2 {
		t.Errorf("两次触发应有两条审计，得到 %d", len(db.audits))
	}
}

func TestTriggerTaskRollsBackWhenAuditFails(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, triggerHandler)
	seedTriggerable(db, triggerTaskKey, triggerHandler, model.TaskStateEnabled)
	boom := errors.New("audit insert exploded")
	db.auditErr = boom

	reply, err := NewTriggerTaskLogic(context.Background(), svcCtx).
		TriggerTask(&rpc.TriggerTaskReq{IdempotencyKey: "ik-1", TaskKey: triggerTaskKey})
	if !errors.Is(err, boom) {
		t.Fatalf("审计写失败必须原样冒出去，得到 %v", err)
	}
	if reply != nil {
		t.Errorf("失败时不得回执行记录：%+v", reply)
	}
	// 「排了队却没痕迹」与「有痕迹却没排队」都不可接受：整个事务必须回滚。
	if len(db.runs) != 0 || len(db.audits) != 0 {
		t.Errorf("事务回滚后库内应干净，得到 runs=%d audits=%d", len(db.runs), len(db.audits))
	}
	if db.txRuns != 1 {
		t.Errorf("应只尝试过 1 个事务，得到 %d", db.txRuns)
	}

	// 故障排除后同一请求要能正常排队（证明上面的空库不是「根本没走到写」。）
	db.auditErr = nil
	if _, err := NewTriggerTaskLogic(context.Background(), svcCtx).
		TriggerTask(&rpc.TriggerTaskReq{IdempotencyKey: "ik-2", TaskKey: triggerTaskKey}); err != nil {
		t.Fatalf("恢复后应能排队：%v", err)
	}
	if len(db.runs) != 1 || len(db.audits) != 1 {
		t.Errorf("恢复后应恰好留下 1 条执行 + 1 条审计，得到 runs=%d audits=%d", len(db.runs), len(db.audits))
	}
}

func TestTriggerTaskReentryReadsBackExistingRunWithoutNewAudit(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, triggerHandler)
	seedTriggerable(db, triggerTaskKey, triggerHandler, model.TaskStateEnabled)
	planned := int64(1700000100)
	origin := seedRun(db, &model.TaskRun{
		TaskKey: triggerTaskKey, PlannedAt: planned, Attempt: 1,
		State: model.RunStateFailed, TriggerType: model.TriggerTypeScheduled, FinishedAt: planned + 5,
	})

	// 竞态复现：事务外取号读到旧快照 → 取到已存在的 1 号 → uniq_fire_attempt 命中。
	db.maxAttemptStaleBy = 1
	reply, err := NewTriggerTaskLogic(context.Background(), svcCtx).
		TriggerTask(&rpc.TriggerTaskReq{IdempotencyKey: "ik-loser", TaskKey: triggerTaskKey, PlannedAt: planned})
	if err != nil {
		t.Fatalf("唯一键命中不是错误：%v", err)
	}
	if reply.GetCreated() {
		t.Errorf("重入必须报 created=false：%+v", reply)
	}
	if reply.GetRun().GetRunId() != origin.ID || reply.GetRun().GetState() != rpc.RunState_RUN_STATE_FAILED {
		t.Errorf("重入应把既有行原样读回给调用方核对：%+v", reply.GetRun())
	}
	if len(db.runs) != 1 || len(db.audits) != 0 {
		t.Errorf("重入不得追加行或审计：runs=%d audits=%d", len(db.runs), len(db.audits))
	}
	// 既有终态行不能被触发「洗白」。
	if db.runs[origin.ID].State != model.RunStateFailed || db.runs[origin.ID].Attempt != 1 {
		t.Errorf("既有行被改写：%+v", db.runs[origin.ID])
	}
}

// TestTriggerTaskIdempotencyKeyDoesNotDedupeSentinel 钉住一条与 README 相反的现实：
// services/cron/README.md 声称「TriggerTask：idempotency_key 必填；同秒重复点击被唯一键挡回」，
// 但 attempt 是在事务**外**用 MAX(attempt)+1 取的，重复点击必然得到新的号，
// uniq_fire_attempt 永不冲突 —— 幂等键只被写进审计 detail，不参与任何去重。
// 修好那天（幂等键落库 + 唯一索引，或取号移入事务）本用例会变红。
func TestTriggerTaskIdempotencyKeyDoesNotDedupeSentinel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, triggerHandler)
	seedTriggerable(db, triggerTaskKey, triggerHandler, model.TaskStateEnabled)
	planned := int64(1700000200)
	in := &rpc.TriggerTaskReq{IdempotencyKey: "ik-same", TaskKey: triggerTaskKey, PlannedAt: planned}
	logic := NewTriggerTaskLogic(context.Background(), svcCtx)

	first, err := logic.TriggerTask(in)
	if err != nil {
		t.Fatalf("首次触发：%v", err)
	}
	second, err := logic.TriggerTask(in)
	if err != nil {
		t.Fatalf("重复触发：%v", err)
	}

	if !first.GetCreated() || !second.GetCreated() {
		t.Fatalf("当前实现两次都报 created=true，用例前提变了：%+v / %+v", first, second)
	}
	if second.GetRun().GetAttempt() != first.GetRun().GetAttempt()+1 {
		t.Errorf("两次触发拿到相邻号（说明完全没去重）：attempt %d vs %d",
			first.GetRun().GetAttempt(), second.GetRun().GetAttempt())
	}
	if len(db.runs) != 2 {
		t.Errorf("当前现实：同一幂等键在同一计划时刻排下 2 条 PENDING，得到 %d 条", len(db.runs))
	}
	if len(db.audits) != 2 {
		t.Errorf("当前现实：两条 trigger 审计都带同一个 idempotency_key，得到 %d 条", len(db.audits))
	}
	for _, a := range db.audits {
		if got := str(t, detailOf(t, a.Detail), "idempotency_key"); got != "ik-same" {
			t.Errorf("审计里的幂等键不对：%q", got)
		}
	}
}

// --- RetryRun ---

func TestRetryRunRejectsBeforeTouchingDatabase(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, triggerHandler)

	for _, tc := range []struct {
		name string
		in   *rpc.RetryRunReq
		want error
	}{
		// run_id 检查排在幂等键之前：nil 与非法 id 都报 ErrRunNotFound。
		{"nil 请求", nil, model.ErrRunNotFound},
		{"run_id 为 0", &rpc.RetryRunReq{}, model.ErrRunNotFound},
		{"run_id 为负", &rpc.RetryRunReq{RunId: -3}, model.ErrRunNotFound},
		{"缺幂等键", &rpc.RetryRunReq{RunId: 42}, model.ErrIdempotencyKeyEmpty},
		{"缺原因", &rpc.RetryRunReq{RunId: 42, IdempotencyKey: "ik-1"}, model.ErrReasonRequired},
		{"原因只有空白", &rpc.RetryRunReq{RunId: 42, IdempotencyKey: "ik-1", Reason: "  "},
			model.ErrReasonRequired},
	} {
		reply, err := NewRetryRunLogic(context.Background(), svcCtx).RetryRun(tc.in)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v，期望 %v", tc.name, err, tc.want)
		}
		if reply != nil {
			t.Errorf("%s: 拒绝时不得回执行记录：%+v", tc.name, reply)
		}
	}
	if len(db.readCalls) != 0 || db.txRuns != 0 || len(db.runs) != 0 || len(db.audits) != 0 {
		t.Errorf("非法请求不得触库或写库：readCalls=%v txRuns=%d runs=%d audits=%d",
			db.readCalls, db.txRuns, len(db.runs), len(db.audits))
	}

	// 对照：合法请求要真的读 run 行。
	origin := seedRun(db, &model.TaskRun{
		TaskKey: triggerTaskKey, PlannedAt: 1700000300, Attempt: 1, State: model.RunStateFailed})
	seedTriggerable(db, triggerTaskKey, triggerHandler, model.TaskStateEnabled)
	if _, err := NewRetryRunLogic(context.Background(), svcCtx).
		RetryRun(&rpc.RetryRunReq{RunId: origin.ID, IdempotencyKey: "ik-1", Reason: "上游补数完成"}); err != nil {
		t.Fatalf("对照用例应成功：%v", err)
	}
	if db.readCalls[0] != fmt.Sprintf("run:%d", origin.ID) {
		t.Errorf("对照用例应先读原执行记录：%v", db.readCalls)
	}
}

func TestRetryRunOnlyFailedOrTimeoutAreRetryable(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, triggerHandler)
	seedTriggerable(db, triggerTaskKey, triggerHandler, model.TaskStateEnabled)

	states := []int32{
		model.RunStatePending, model.RunStateRunning, model.RunStateRetrying,
		model.RunStateSucceeded, model.RunStateFailed, model.RunStateTimeout,
		model.RunStateCanceled, model.RunStateSkipped,
	}
	for _, s := range states {
		db.runs = map[int64]*model.TaskRun{}
		db.audits = nil
		db.readCalls = nil
		row := seedRun(db, &model.TaskRun{
			TaskKey: triggerTaskKey, PlannedAt: 1700000400, Attempt: 1, State: s})
		reply, err := NewRetryRunLogic(context.Background(), svcCtx).
			RetryRun(&rpc.RetryRunReq{RunId: row.ID, IdempotencyKey: "ik-1", Reason: "r"})

		retryable := s == model.RunStateFailed || s == model.RunStateTimeout
		if retryable {
			if err != nil || !reply.GetCreated() {
				t.Fatalf("%s 应允许人工重试，得到 %+v / %v", model.RunStateName(s), reply, err)
			}
			continue
		}
		if !errors.Is(err, model.ErrRunNotRetryable) {
			t.Errorf("%s 不该可重试，err = %v", model.RunStateName(s), err)
		}
		if reply != nil {
			t.Errorf("%s 拒绝时不得回记录：%+v", model.RunStateName(s), reply)
		}
		// 状态闸门排在定义读之前：只应读到原行。
		if got, want := fmt.Sprint(db.readCalls), fmt.Sprintf("[run:%d]", row.ID); got != want {
			t.Errorf("%s: 状态判定应在读定义之前，实际轨迹 %s（期望 %s）", model.RunStateName(s), got, want)
		}
		if len(db.runs) != 1 || len(db.audits) != 0 {
			t.Errorf("%s: 拒绝后不得追加行或审计：runs=%d audits=%d",
				model.RunStateName(s), len(db.runs), len(db.audits))
		}
	}
}

func TestRetryRunAppendsAttemptWithoutTouchingOrigin(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, triggerHandler)
	def := seedTriggerable(db, triggerTaskKey, triggerHandler, model.TaskStateEnabled)
	planned := int64(1700000500)
	seedRun(db, &model.TaskRun{
		TaskKey: triggerTaskKey, PlannedAt: planned, Attempt: 2,
		State: model.RunStateSucceeded, TriggerType: model.TriggerTypeScheduled, FinishedAt: planned + 9})
	origin := seedRun(db, &model.TaskRun{
		TaskKey: triggerTaskKey, PlannedAt: planned, Attempt: 3, State: model.RunStateFailed,
		TriggerType: model.TriggerTypeRetry, LeaseOwner: "worker-old", FenceToken: 77,
		LeaseExpireAt: 0, StartedAt: planned + 20, FinishedAt: planned + 30, LastError: "boom"})
	originSnapshot := *origin

	reply, err := NewRetryRunLogic(context.Background(), svcCtx).
		RetryRun(&rpc.RetryRunReq{
			RunId: origin.ID, IdempotencyKey: "ik-retry", Operator: "bob",
			Reason: "上游数据已补齐", TraceId: "trace-9"})
	if err != nil {
		t.Fatalf("RetryRun: %v", err)
	}
	if !reply.GetCreated() {
		t.Errorf("人工重试首次追加必须 created=true：%+v", reply)
	}

	byAttempt := runsByAttempt(db)
	if len(byAttempt) != 3 {
		t.Fatalf("库内应有 attempt=2/3/4 三条，实际 %v", runIDs(db.runs))
	}
	added, ok := byAttempt[4]
	if !ok {
		t.Fatalf("新行必须是 attempt = 历史最大 + 1 = 4，实际 %v", runIDs(db.runs))
	}
	if added.State != model.RunStatePending {
		t.Errorf("新行 state = %s，期望 pending", model.RunStateName(added.State))
	}
	if added.TriggerType != model.TriggerTypeRetry {
		t.Errorf("新行 trigger_type = %d，期望 retry(%d)", added.TriggerType, model.TriggerTypeRetry)
	}
	if added.PlannedAt != planned {
		t.Errorf("重放的幂等上下文不变：planned_at = %d，期望 %d", added.PlannedAt, planned)
	}
	if added.Operator != "bob" || added.TraceID != "trace-9" {
		t.Errorf("新行未带 operator/trace_id：%q / %q", added.Operator, added.TraceID)
	}
	// 追加新行，绝不原地复活终态行（历史不可篡改，AGENTS.md §8）。
	if *db.runs[origin.ID] != originSnapshot {
		t.Errorf("原终态行被改写：before=%+v after=%+v", originSnapshot, *db.runs[origin.ID])
	}
	if len(db.leases) != 0 {
		t.Errorf("RetryRun 不得写租约，得到 %d 条", len(db.leases))
	}
	if cur := db.defs[triggerTaskKey]; cur.NextFireAt != def.NextFireAt || cur.Version != def.Version {
		t.Errorf("人工重试不得推进调度指针/版本：cur=%+v def=%+v", cur, def)
	}

	audit := onlyAudit(t, db, model.AuditActionRetry)
	if audit.ToState != model.RunStateName(model.RunStatePending) || audit.FromState != "" {
		t.Errorf("retry 审计的 to/from_state 不对：from=%q to=%q", audit.FromState, audit.ToState)
	}
	if audit.Operator != "bob" {
		t.Errorf("审计 operator = %q，期望 bob", audit.Operator)
	}
	d := detailOf(t, audit.Detail)
	if num(t, d, "origin_run_id") != float64(origin.ID) || num(t, d, "new_run_id") != float64(added.ID) {
		t.Errorf("审计要连起原行与新行：%v", d)
	}
	if num(t, d, "origin_attempt") != 3 || num(t, d, "new_attempt") != 4 ||
		num(t, d, "planned_at") != float64(planned) || num(t, d, "max_attempts") != float64(def.MaxAttempts) {
		t.Errorf("审计 detail 的取号事实不对：%v", d)
	}
	if str(t, d, "origin_state") != "failed" || str(t, d, "reason") != "上游数据已补齐" ||
		str(t, d, "idempotency_key") != "ik-retry" {
		t.Errorf("审计 detail 的来龙去脉不对：%v", d)
	}
}

// TestRetryRunNumbersInsideTransactionSentinel 证明取号发生在事务内：
// maxAttemptStaleBy 这个只污染「非事务 MaxAttempt」的开关，对 RetryRun 必须完全无效
// （TriggerTask 的同一开关见 TestTriggerTaskReentryReadsBackExistingRunWithoutNewAudit）。
func TestRetryRunNumbersInsideTransactionSentinel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, triggerHandler)
	seedTriggerable(db, triggerTaskKey, triggerHandler, model.TaskStateEnabled)
	planned := int64(1700000600)
	origin := seedRun(db, &model.TaskRun{
		TaskKey: triggerTaskKey, PlannedAt: planned, Attempt: 1, State: model.RunStateTimeout})
	// 同一计划时刻已有更大的号：人工重试要接在最大号之后，而不是 origin 之后。
	seedRun(db, &model.TaskRun{
		TaskKey: triggerTaskKey, PlannedAt: planned, Attempt: 5, State: model.RunStateSucceeded})
	db.maxAttemptStaleBy = 4 // 若 RetryRun 用非事务取号，会算出 attempt=2 并撞上已有行

	reply, err := NewRetryRunLogic(context.Background(), svcCtx).
		RetryRun(&rpc.RetryRunReq{RunId: origin.ID, IdempotencyKey: "ik-1", Reason: "r"})
	if err != nil {
		t.Fatalf("RetryRun: %v", err)
	}
	if got := reply.GetRun().GetAttempt(); got != 6 {
		t.Errorf("attempt 应为「事务内读到的最大值 + 1」= 6，得到 %d（轨迹 %v）", got, db.readCalls)
	}
	if !reply.GetCreated() {
		t.Errorf("事务内取号不该撞上已有行，却回了 created=false：%+v", reply)
	}
	if len(db.runs) != 3 {
		t.Errorf("应追加恰好 1 条，实际 %v", runIDs(db.runs))
	}
}

func TestRetryRunDefinitionGatesAndRollback(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, triggerHandler)
	planned := int64(1700000700)

	// 定义已被删掉：孤儿执行记录不能被重试（没有 handler 与调度参数可依）。
	orphan := seedRun(db, &model.TaskRun{
		TaskKey: "deleted.task", PlannedAt: planned, Attempt: 1, State: model.RunStateFailed})
	_, err := NewRetryRunLogic(context.Background(), svcCtx).
		RetryRun(&rpc.RetryRunReq{RunId: orphan.ID, IdempotencyKey: "ik-1", Reason: "r"})
	if !errors.Is(err, model.ErrTaskNotFound) {
		t.Errorf("定义缺失应报 ErrTaskNotFound，得到 %v", err)
	}
	if got, want := fmt.Sprint(db.readCalls), fmt.Sprintf("[run:%d definition:deleted.task]", orphan.ID); got != want {
		t.Errorf("读轨迹不对：%s（期望 %s）", got, want)
	}

	// DISABLED 是终态，人工重试也不能跑；PAUSED 仍允许。
	db.readCalls = nil
	seedTriggerable(db, "stopped.task", triggerHandler, model.TaskStateDisabled)
	stopped := seedRun(db, &model.TaskRun{
		TaskKey: "stopped.task", PlannedAt: planned, Attempt: 1, State: model.RunStateFailed})
	if _, err := NewRetryRunLogic(context.Background(), svcCtx).
		RetryRun(&rpc.RetryRunReq{RunId: stopped.ID, IdempotencyKey: "ik-2", Reason: "r"}); !errors.Is(err, model.ErrStateTransition) {
		t.Errorf("停用任务应报 ErrStateTransition，得到 %v", err)
	}
	seedTriggerable(db, "paused.task", triggerHandler, model.TaskStatePaused)
	paused := seedRun(db, &model.TaskRun{
		TaskKey: "paused.task", PlannedAt: planned, Attempt: 1, State: model.RunStateFailed})
	if _, err := NewRetryRunLogic(context.Background(), svcCtx).
		RetryRun(&rpc.RetryRunReq{RunId: paused.ID, IdempotencyKey: "ik-3", Reason: "r"}); err != nil {
		t.Errorf("PAUSED 任务应允许人工重试，得到 %v", err)
	}

	// handler 未注册：闸门必须在开事务之前。
	seedTriggerable(db, "orphan-handler.task", "handler.missing", model.TaskStateEnabled)
	hard := seedRun(db, &model.TaskRun{
		TaskKey: "orphan-handler.task", PlannedAt: planned, Attempt: 1, State: model.RunStateTimeout})
	before := db.txRuns
	if _, err := NewRetryRunLogic(context.Background(), svcCtx).
		RetryRun(&rpc.RetryRunReq{RunId: hard.ID, IdempotencyKey: "ik-4", Reason: "r"}); !errors.Is(err, model.ErrHandlerNotRegistered) {
		t.Errorf("未注册 handler 应报 ErrHandlerNotRegistered，得到 %v", err)
	}
	if db.txRuns != before {
		t.Errorf("handler 闸门应在事务之前，txRuns 从 %d 变成 %d", before, db.txRuns)
	}

	// 审计写失败：新行必须跟着回滚，原行不动。
	boom := errors.New("audit insert exploded")
	runsBefore := runsOfTask(db, "paused.task")
	auditsBefore := len(db.audits)
	db.auditErr = boom
	if _, err := NewRetryRunLogic(context.Background(), svcCtx).
		RetryRun(&rpc.RetryRunReq{RunId: paused.ID, IdempotencyKey: "ik-5", Reason: "r"}); !errors.Is(err, boom) {
		t.Fatalf("审计失败要原样冒出去，得到 %v", err)
	}
	if got := runsOfTask(db, "paused.task"); len(got) != len(runsBefore) {
		t.Errorf("回滚后不该多出 attempt 行：before=%v after=%v", runIDs(runsBefore), runIDs(got))
	}
	if len(db.audits) != auditsBefore {
		t.Errorf("回滚后不该多出审计：%d → %d", auditsBefore, len(db.audits))
	}
	db.auditErr = nil
	if _, err := NewRetryRunLogic(context.Background(), svcCtx).
		RetryRun(&rpc.RetryRunReq{RunId: paused.ID, IdempotencyKey: "ik-6", Reason: "r"}); err != nil {
		t.Fatalf("恢复后应能重试：%v", err)
	}
	if r := db.runs[paused.ID]; r.State != model.RunStateFailed || r.Attempt != 1 {
		t.Errorf("原终态行在回滚与恢复后都必须保持不动：%+v", r)
	}
	if got := runsOfTask(db, "paused.task"); len(got) != len(runsBefore)+1 {
		t.Errorf("恢复后应恰好追加 1 行：%v", runIDs(got))
	}
}

// runsOfTask 取某个任务在库内的全部执行记录（返回的是副本 map，可安全比对）。
func runsOfTask(db *fakeDB, taskKey string) map[int64]*model.TaskRun {
	out := map[int64]*model.TaskRun{}
	for id, r := range db.runs {
		if r.TaskKey == taskKey {
			c := *r
			out[id] = &c
		}
	}
	return out
}

// --- ListDueTasks ---

// seedDue 铺一条参与到期扫描的定义：state 与 next_fire_at 是唯一两个判定维度。
func seedDue(db *fakeDB, taskKey string, state int32, nextFireAt int64, group string) *model.TaskDefinition {
	return seedDefinition(db, &model.TaskDefinition{
		TaskKey: taskKey, Name: taskKey, Handler: "h." + taskKey, TaskGroup: group,
		ScheduleType: model.ScheduleTypeInterval, IntervalSeconds: 60, Timezone: "UTC",
		TimeoutSeconds: 60, MaxAttempts: 2, RetryBaseSeconds: 10, ConcurrencyLimit: 1,
		LeaseTTLSeconds: 120, Params: `{"day":"2026-01-02"}`, MisfirePolicy: model.MisfirePolicyFireOnceNow,
		State: state, Version: 1, NextFireAt: nextFireAt,
	})
}

func dueKeys(reply *rpc.ListDueTasksReply) []string {
	out := make([]string, 0, len(reply.GetList()))
	for _, d := range reply.GetList() {
		out = append(out, d.TaskKey)
	}
	return out
}

func TestListDueTasksRequiresEnabledStateAndExpiredPointer(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := int64(1700000000)
	seedDue(db, "due.enabled", model.TaskStateEnabled, now-30, "report")
	seedDue(db, "due.future", model.TaskStateEnabled, now+600, "report")
	seedDue(db, "due.paused_stale", model.TaskStatePaused, now-60, "report")
	seedDue(db, "due.disabled_stale", model.TaskStateDisabled, now-60, "report")
	seedDue(db, "due.enabled_zero_pointer", model.TaskStateEnabled, 0, "report")
	seedDue(db, "due.enabled_boundary", model.TaskStateEnabled, now, "other")

	reply, err := NewListDueTasksLogic(context.Background(), svcCtx).
		ListDueTasks(&rpc.ListDueTasksReq{Now: now})
	if err != nil {
		t.Fatalf("ListDueTasks: %v", err)
	}
	// 状态与指针缺一不可：暂停/停用即使指针过期也不扫，启用但指针为 0 也不扫。
	if got, want := fmt.Sprint(dueKeys(reply)), "[due.enabled due.enabled_boundary]"; got != want {
		t.Errorf("到期集合 = %s，期望 %s", got, want)
	}
	call := db.dueCalls[0]
	if call.now != now || call.lookahead != 0 || call.group != "" {
		t.Errorf("扫描实参不对：%+v", call)
	}

	// 边界即 next_fire_at == now 算到期（<=，不是 <）。
	if dueKeys(reply)[1] != "due.enabled_boundary" {
		t.Errorf("指针恰等于 now 的行必须在清单里：%v", dueKeys(reply))
	}

	// 提前量把未来的点也纳入；负值夹到 0，等于「只要已到期的」。
	ahead, err := NewListDueTasksLogic(context.Background(), svcCtx).
		ListDueTasks(&rpc.ListDueTasksReq{Now: now, LookaheadSeconds: 900})
	if err != nil {
		t.Fatalf("带提前量的扫描：%v", err)
	}
	if got, want := fmt.Sprint(dueKeys(ahead)),
		"[due.enabled due.enabled_boundary due.future]"; got != want {
		t.Errorf("提前 900 秒的集合 = %s，期望 %s", got, want)
	}
	if db.dueCalls[len(db.dueCalls)-1].lookahead != 900 {
		t.Errorf("提前量应原样传给 model，得到 %+v", db.dueCalls[len(db.dueCalls)-1])
	}
	neg, err := NewListDueTasksLogic(context.Background(), svcCtx).
		ListDueTasks(&rpc.ListDueTasksReq{Now: now, LookaheadSeconds: -120})
	if err != nil {
		t.Fatalf("负提前量应被夹住而不是报错：%v", err)
	}
	last := db.dueCalls[len(db.dueCalls)-1]
	if last.lookahead != 0 {
		t.Errorf("负提前量必须夹到 0，得到 %+v", last)
	}
	if got, want := fmt.Sprint(dueKeys(neg)), "[due.enabled due.enabled_boundary]"; got != want {
		t.Errorf("夹住后的集合 = %s，期望 %s", got, want)
	}

	// 分组过滤在 SQL 里做。
	filtered, err := NewListDueTasksLogic(context.Background(), svcCtx).
		ListDueTasks(&rpc.ListDueTasksReq{Now: now, TaskGroup: "other"})
	if err != nil {
		t.Fatalf("分组过滤扫描：%v", err)
	}
	if got, want := fmt.Sprint(dueKeys(filtered)), "[due.enabled_boundary]"; got != want {
		t.Errorf("分组集合 = %s，期望 %s", got, want)
	}
	if db.dueCalls[len(db.dueCalls)-1].group != "other" {
		t.Errorf("分组条件未下推：%+v", db.dueCalls[len(db.dueCalls)-1])
	}
}

func TestListDueTasksIsReadOnlyAndUsesServerClock(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	def := seedDue(db, "due.readonly", model.TaskStateEnabled, now-5, "report")
	seedRun(db, &model.TaskRun{
		TaskKey: "due.readonly", PlannedAt: now - 60, Attempt: 1, State: model.RunStateSucceeded})
	runsBefore := len(db.runs)

	reply, err := NewListDueTasksLogic(context.Background(), svcCtx).ListDueTasks(nil)
	if err != nil {
		t.Fatalf("nil 请求要按服务端默认跑：%v", err)
	}
	if got, want := fmt.Sprint(dueKeys(reply)), "[due.readonly]"; got != want {
		t.Fatalf("集合 = %s，期望 %s", got, want)
	}
	// now=0 时取服务端时钟；同时 reply.server_time 也是服务端现读，不是回显。
	call := db.dueCalls[len(db.dueCalls)-1]
	if call.now < now || call.now > fakeNow() {
		t.Errorf("now=0 必须落到服务端时钟，得到 %+v", call)
	}
	if reply.GetServerTime() < call.now {
		t.Errorf("server_time 必须是服务端时钟（不早于扫描时刻）：scan=%d reply=%d",
			call.now, reply.GetServerTime())
	}
	// 负 now 与 0 同等对待（调用方时钟不可信）。
	if _, err := NewListDueTasksLogic(context.Background(), svcCtx).
		ListDueTasks(&rpc.ListDueTasksReq{Now: -1}); err != nil {
		t.Fatalf("负 now 应被当作未提供：%v", err)
	}
	echo := db.dueCalls[len(db.dueCalls)-1]
	if echo.now < now {
		t.Errorf("负 now 未落到服务端时钟：%+v", echo)
	}

	// 只读契约：不产生执行记录、不占租约、不写审计、不动指针。
	if db.txRuns != 0 {
		t.Errorf("到期扫描不得开事务，txRuns=%d", db.txRuns)
	}
	if len(db.runs) != runsBefore || len(db.leases) != 0 || len(db.audits) != 0 {
		t.Errorf("到期扫描不得写库：runs=%d leases=%d audits=%d",
			len(db.runs), len(db.leases), len(db.audits))
	}
	cur := db.defs["due.readonly"]
	if cur.NextFireAt != def.NextFireAt || cur.LastFireAt != def.LastFireAt || cur.Version != def.Version {
		t.Errorf("扫描不得推进指针/版本：def=%+v cur=%+v", def, cur)
	}
}

func TestListDueTasksLimitConvergence(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := int64(1700000000)
	seedDue(db, "due.a", model.TaskStateEnabled, now-3, "report")
	seedDue(db, "due.b", model.TaskStateEnabled, now-2, "report")
	logic := NewListDueTasksLogic(context.Background(), svcCtx)

	// limit=0：用 config.Scheduler.BatchSize；未配置时兜到 DefaultPageSize。
	if _, err := logic.ListDueTasks(&rpc.ListDueTasksReq{Now: now}); err != nil {
		t.Fatalf("默认批量：%v", err)
	}
	if got := db.dueCalls[len(db.dueCalls)-1].limit; got != model.DefaultPageSize {
		t.Errorf("BatchSize 未配置时应兜到 DefaultPageSize(%d)，得到 %d", model.DefaultPageSize, got)
	}
	svcCtx.Config.Scheduler.BatchSize = 1
	if _, err := logic.ListDueTasks(&rpc.ListDueTasksReq{Now: now}); err != nil {
		t.Fatalf("BatchSize=1：%v", err)
	}
	call := db.dueCalls[len(db.dueCalls)-1]
	if call.limit != 1 {
		t.Fatalf("BatchSize 必须生效，得到 %+v", call)
	}
	if got, want := fmt.Sprint(dueKeys(mustDue(t, logic, &rpc.ListDueTasksReq{Now: now}))),
		"[due.a]"; got != want {
		t.Errorf("限 1 条时按 next_fire_at 升序取最早的，得到 %s 期望 %s", got, want)
	}
	// 配置里的 BatchSize 再大也夹到 model.MaxPageSize，不做无界扫描。
	svcCtx.Config.Scheduler.BatchSize = 100000
	if _, err := logic.ListDueTasks(&rpc.ListDueTasksReq{Now: now}); err != nil {
		t.Fatalf("超大 BatchSize：%v", err)
	}
	if got := db.dueCalls[len(db.dueCalls)-1].limit; got != model.MaxPageSize {
		t.Errorf("BatchSize 应夹到 MaxPageSize(%d)，得到 %d", model.MaxPageSize, got)
	}
	// 显式越界要报错，不静默放大。
	db.dueCalls = nil
	if _, err := logic.ListDueTasks(&rpc.ListDueTasksReq{Now: now, Limit: model.MaxPageSize + 1}); !errors.Is(err, model.ErrInvalidPageLimit) {
		t.Errorf("limit 越界应报 ErrInvalidPageLimit，得到 %v", err)
	}
	if len(db.dueCalls) != 0 {
		t.Errorf("越界 limit 必须在扫描之前拒绝：%v", db.dueCalls)
	}
	// 负数按未提供处理（<=0 分支）。
	if _, err := logic.ListDueTasks(&rpc.ListDueTasksReq{Now: now, Limit: -7}); err != nil {
		t.Errorf("负 limit 应按默认处理，得到 %v", err)
	}
}

func mustDue(t *testing.T, l *ListDueTasksLogic, in *rpc.ListDueTasksReq) *rpc.ListDueTasksReply {
	t.Helper()
	reply, err := l.ListDueTasks(in)
	if err != nil {
		t.Fatalf("ListDueTasks(%+v): %v", in, err)
	}
	return reply
}

// TestListDueTasksLimitIgnoresConfigMaxPageSentinel 钉住本服务的两套分页天花板：
// 其它列表接口用 svcCtx.PageSize（受 config.Task.MaxPageSize=100 约束），
// 而 ListDueTasks 只认 model.MaxPageSize=200，且不读 config.Task.MaxPageSize。
// 于是同一个 page_size=150 在两个接口里一边报错一边放行。
func TestListDueTasksLimitIgnoresConfigMaxPageSentinel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	svcCtx.Config.Task.MaxPageSize = 100
	now := int64(1700000000)
	seedDue(db, "due.a", model.TaskStateEnabled, now-1, "report")

	reply, err := NewListDueTasksLogic(context.Background(), svcCtx).
		ListDueTasks(&rpc.ListDueTasksReq{Now: now, Limit: 150})
	if err != nil {
		t.Fatalf("当前现实：limit=150 被放行（配置里 max=100），err=%v", err)
	}
	if got := db.dueCalls[len(db.dueCalls)-1].limit; got != 150 {
		t.Fatalf("实参未透传，得到 %d", got)
	}
	if len(reply.GetList()) != 1 {
		t.Fatalf("应扫到 1 条：%v", dueKeys(reply))
	}

	// 判别性对照：同一份配置下，ListTaskRuns 的 page_size=150 必然被拒。
	if _, err := NewListTaskRunsLogic(context.Background(), svcCtx).
		ListTaskRuns(&rpc.ListTaskRunsReq{PageSize: 150}); !errors.Is(err, model.ErrInvalidPageLimit) {
		t.Errorf("对照失败：ListTaskRuns 也应放行 150 了？err=%v", err)
	}
}

func TestListDueTasksRunningCountCountsOnlyUnexpiredRuns(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	seedDue(db, "busy.task", model.TaskStateEnabled, now-10, "report")
	seedDue(db, "idle.task", model.TaskStateEnabled, now-9, "report")
	// busy：一条租约未到期的 RUNNING + 一条崩溃实例的过期 RUNNING + 若干非 RUNNING。
	seedRun(db, &model.TaskRun{TaskKey: "busy.task", PlannedAt: now - 60, Attempt: 1,
		State: model.RunStateRunning, LeaseOwner: "w-1", FenceToken: 3, LeaseExpireAt: now + 300})
	seedRun(db, &model.TaskRun{TaskKey: "busy.task", PlannedAt: now - 120, Attempt: 1,
		State: model.RunStateRunning, LeaseOwner: "w-dead", FenceToken: 4, LeaseExpireAt: now - 1})
	seedRun(db, &model.TaskRun{TaskKey: "busy.task", PlannedAt: now - 180, Attempt: 2,
		State: model.RunStateRetrying, NextRetryAt: now + 60})
	seedRun(db, &model.TaskRun{TaskKey: "idle.task", PlannedAt: now - 60, Attempt: 1,
		State: model.RunStateSucceeded})

	reply, err := NewListDueTasksLogic(context.Background(), svcCtx).
		ListDueTasks(&rpc.ListDueTasksReq{Now: now})
	if err != nil {
		t.Fatalf("ListDueTasks: %v", err)
	}
	counts := map[string]int64{}
	for _, d := range reply.GetList() {
		counts[d.TaskKey] = d.RunningCount
	}
	// 过期 RUNNING 不再占用并发额度，否则崩溃一次就把这个任务永久堵死。
	if counts["busy.task"] != 1 {
		t.Errorf("running_count = %d，期望 1（过期 RUNNING 不计数）：%v", counts["busy.task"], counts)
	}
	if counts["idle.task"] != 0 {
		t.Errorf("非 RUNNING 行不得计数，得到 %d", counts["idle.task"])
	}
	// 每个到期任务读一次计数，顺序与清单一致；未到期的任务一次都不读。
	if got, want := fmt.Sprint(db.countRunningCalls), "[busy.task idle.task]"; got != want {
		t.Errorf("计数读取轨迹 = %s，期望 %s", got, want)
	}
}

func TestListDueTasksFailsWholeScanWhenCounterFails(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	seedDue(db, "due.a", model.TaskStateEnabled, now-1, "report")
	boom := errors.New("count running blew up")
	db.countRunningErr = boom

	reply, err := NewListDueTasksLogic(context.Background(), svcCtx).
		ListDueTasks(&rpc.ListDueTasksReq{Now: now})
	if !errors.Is(err, boom) {
		t.Fatalf("并发数拿不到必须整体失败，得到 %v", err)
	}
	// 关键：绝不返回一份 running_count=0 的「假清单」，那会让调度端超发。
	if reply != nil {
		t.Errorf("失败时不得回半张清单：%+v", reply)
	}

	// 判别性对照：扫不到到期行时根本不会读计数器，因此同一故障也不该让调用失败。
	db.dueCalls = nil
	db.countRunningCalls = nil
	ok, err := NewListDueTasksLogic(context.Background(), svcCtx).
		ListDueTasks(&rpc.ListDueTasksReq{Now: now - 100000})
	if err != nil {
		t.Fatalf("空清单不应触发计数读：%v", err)
	}
	if len(ok.GetList()) != 0 {
		t.Errorf("应为空清单：%v", dueKeys(ok))
	}
	if len(db.countRunningCalls) != 0 {
		t.Errorf("空清单时不得读计数器：%v", db.countRunningCalls)
	}
}

func TestListDueTasksProjectsSchedulingFields(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := fakeNow()
	def := seedDue(db, "due.project", model.TaskStateEnabled, now-7, "media")
	seedRun(db, &model.TaskRun{TaskKey: "due.project", PlannedAt: now - 30, Attempt: 1,
		State: model.RunStateRunning, LeaseExpireAt: now + 90})

	reply, err := NewListDueTasksLogic(context.Background(), svcCtx).
		ListDueTasks(&rpc.ListDueTasksReq{Now: now})
	if err != nil {
		t.Fatalf("ListDueTasks: %v", err)
	}
	if len(reply.GetList()) != 1 {
		t.Fatalf("应恰好 1 条：%v", dueKeys(reply))
	}
	got := reply.GetList()[0]
	if got.TaskKey != def.TaskKey || got.Handler != def.Handler || got.TaskGroup != def.TaskGroup ||
		got.Params != def.Params || got.TimeoutSeconds != def.TimeoutSeconds ||
		got.MaxAttempts != def.MaxAttempts || got.LeaseTtlSeconds != def.LeaseTTLSeconds ||
		got.ConcurrencyLimit != def.ConcurrencyLimit || got.PlannedAt != def.NextFireAt ||
		got.RunningCount != 1 {
		t.Errorf("DueTask 投影与定义不一致：got=%+v def=%+v", got, def)
	}
	// planned_at 取自定义指针：claim 时的幂等身份由它决定。
	if got.PlannedAt != now-7 {
		t.Errorf("planned_at = %d，期望 next_fire_at %d", got.PlannedAt, now-7)
	}
	// 投影完整性守卫：proto 加字段必须在这里补搬运断言。
	if n, want := exportedFieldCount(rpc.DueTask{}), 10; n != want {
		t.Errorf("rpc.DueTask 导出字段数 = %d，本用例覆盖 %d 个；新增字段必须补断言", n, want)
	}
}
