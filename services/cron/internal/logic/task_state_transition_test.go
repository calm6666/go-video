package logic

// 状态迁移写侧单测：PauseTask / ResumeTask / DisableTask。
//
// 三个入口共用 helpers.go 的 stateTransition 骨架，差异只在三处：
//   1. 入参门槛（Pause/Disable 必须带 reason，Resume 没有这个字段）；
//   2. 允许的来源状态集合 allowedFrom（Disable 两个来源，Pause/Resume 各一个）；
//   3. 调度指针怎么落（Pause 交给 SetState 的 SQL CASE；Resume 按 MisfirePolicy 重算；
//      Disable 显式归零）。
//
// 所以本文件的断言集中在四件事：拒绝发生在开事务之前、幂等重入一行都不改也不写审计、
// 指针落点与 MisfirePolicy 一一对应、审计写失败必须连状态带指针整体回滚。
//
// MisfirePolicy 的三条期望值（now / now-3600 / now+3600）互不相同：
// 任何一条算错都会撞上别的分支，这是本文件唯一的正确性来源，
// 不在测试里复刻 cron 表达式解析（具名时区/夏令时的判定以 model 的产出为准，
// 测试只核对「指针落在 03:00:00Z 且晚于现在」这类性质）。
//
// 已知缺陷按 README 编号钉在本文件末尾的 *Sentinel* 用例里（C/E/I）。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"
)

// --- 本文件共用的小工具 ---

func pauseReq(taskKey, reason string, version int64) *rpc.PauseTaskReq {
	return &rpc.PauseTaskReq{
		TaskKey: taskKey, Reason: reason, ExpectedVersion: version,
		IdempotencyKey: "req-pause-" + taskKey, Operator: "ops.carol", TraceId: "trace-pause",
	}
}

func resumeReq(taskKey string, version int64) *rpc.ResumeTaskReq {
	return &rpc.ResumeTaskReq{
		TaskKey: taskKey, ExpectedVersion: version,
		IdempotencyKey: "req-resume-" + taskKey, Operator: "ops.dave", TraceId: "trace-resume",
	}
}

func disableReq(taskKey, reason string, version int64) *rpc.DisableTaskReq {
	return &rpc.DisableTaskReq{
		TaskKey: taskKey, Reason: reason, ExpectedVersion: version,
		IdempotencyKey: "req-disable-" + taskKey, Operator: "ops.erin", TraceId: "trace-disable",
	}
}

// transitionRow 布一条「可被状态迁移」的任务定义：INTERVAL 1 小时、启用中、
// 有一个未来计划点。用例只改自己关心的那几个字段，其余与注册后的形状一致。
// 走 seedDefinition（静默写入），因此不会污染 txRuns/stateCalls 这类调用轨迹。
func transitionRow(db *fakeDB, mutate func(*model.TaskDefinition)) *model.TaskDefinition {
	row := &model.TaskDefinition{
		TaskKey: testTaskKey, Name: "日报扫描", Handler: "report.daily", TaskGroup: "report",
		ScheduleType: model.ScheduleTypeInterval, IntervalSeconds: 3600, Timezone: "UTC",
		State: model.TaskStateEnabled, TimeoutSeconds: 600, MaxAttempts: 1, ConcurrencyLimit: 1,
		LeaseTTLSeconds: 120, MisfirePolicy: model.MisfirePolicyFireOnceNow,
		Version: 1, NextFireAt: fakeNow() + 3600, Operator: "ops.alice",
	}
	if mutate != nil {
		mutate(row)
	}
	return seedDefinition(db, row)
}

// pausedSince 把行改成「暂停了整整 gapSeconds 秒」：last_fire_at 落后 gapSeconds，
// 且指针被 SetState 的 CASE 归零，正是线上恢复动作面对的形状。
func pausedSince(gapSeconds int64) func(*model.TaskDefinition) {
	return func(d *model.TaskDefinition) {
		d.State = model.TaskStatePaused
		d.LastFireAt = fakeNow() - gapSeconds
		d.NextFireAt = 0
	}
}

// runStateOp 按名字调用一个状态迁移入口。
func runStateOp(t *testing.T, svcCtx *svc.ServiceContext, op, taskKey, reason string, version int64,
) (*rpc.TaskOperationReply, error) {
	t.Helper()
	switch op {
	case "pause":
		return NewPauseTaskLogic(context.Background(), svcCtx).PauseTask(pauseReq(taskKey, reason, version))
	case "resume":
		return NewResumeTaskLogic(context.Background(), svcCtx).ResumeTask(resumeReq(taskKey, version))
	case "disable":
		return NewDisableTaskLogic(context.Background(), svcCtx).DisableTask(disableReq(taskKey, reason, version))
	}
	t.Fatalf("未知操作 %q", op)
	return nil, nil
}

// assertStateCalls 核对 SetStateTx 的实参轨迹（含被回滚的调用，见 fakes_test.go 的说明）。
func assertStateCalls(t *testing.T, db *fakeDB, want []stateCall, label string) {
	t.Helper()
	if len(db.stateCalls) != len(want) {
		t.Fatalf("%s：SetState 调用 %d 次，期望 %d：%+v", label, len(db.stateCalls), len(want), db.stateCalls)
	}
	for i, w := range want {
		g := db.stateCalls[i]
		if g != w {
			t.Errorf("%s：第 %d 次 SetState = %+v，期望 %+v", label, i+1, g, w)
		}
	}
}

// detailInt64 取审计 detail 里的一个数值字段（JSON 数字一律解成 float64）。
func detailInt64(t *testing.T, a *model.TaskAudit, key string) (int64, bool) {
	t.Helper()
	v, ok := detailFields(t, a)[key]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("审计 detail[%q] 不是数值：%v", key, v)
	}
	return int64(f), true
}

// detailReason 取审计 detail 里的 reason 文本（cron_task_audit 没有 reason 列，
// 原因只存在于 detail JSON 里，见 model/taskaudit.go 的列清单）。
func detailReason(t *testing.T, a *model.TaskAudit) string {
	t.Helper()
	v, ok := detailFields(t, a)["reason"]
	if !ok {
		t.Fatalf("审计 detail 丢了 reason，答不出「谁在什么时候为什么」：%+v", a.Detail)
	}
	s, _ := v.(string)
	return s
}

// assertRowFrozen 断言库里行逐字段没动。
func assertRowFrozen(t *testing.T, db *fakeDB, before model.TaskDefinition) {
	t.Helper()
	if after := *storedDef(t, db, testTaskKey); after != before {
		t.Errorf("库里行被改动了：\n前 %+v\n后 %+v", before, after)
	}
}

// --- 入参门槛：拒绝必须发生在开事务之前 ---

func TestPauseTaskRejectsBeforeOpeningTransaction(t *testing.T) {
	cases := []struct {
		name    string
		nilReq  bool
		mutate  func(*rpc.PauseTaskReq)
		wantErr error
	}{
		{name: "nil 请求", nilReq: true, wantErr: model.ErrTaskKeyEmpty},
		// 判定顺序：幂等键最先，reason 其次，task_key 最后（三个入口必须同一顺序）。
		{name: "空请求体", mutate: func(r *rpc.PauseTaskReq) { *r = rpc.PauseTaskReq{} },
			wantErr: model.ErrIdempotencyKeyEmpty},
		{name: "缺幂等键", mutate: func(r *rpc.PauseTaskReq) { r.IdempotencyKey = "  " },
			wantErr: model.ErrIdempotencyKeyEmpty},
		{name: "缺 reason", mutate: func(r *rpc.PauseTaskReq) { r.Reason = "" },
			wantErr: model.ErrReasonRequired},
		{name: "reason 只有空白", mutate: func(r *rpc.PauseTaskReq) { r.Reason = " \t " },
			wantErr: model.ErrReasonRequired},
		{name: "task_key 空白", mutate: func(r *rpc.PauseTaskReq) { r.TaskKey = "   " },
			wantErr: model.ErrTaskKeyEmpty},
		// reason 先于 task_key：运营同时漏掉两项时，先补的那条错误信息才有用。
		{name: "同时缺 reason 与 task_key", mutate: func(r *rpc.PauseTaskReq) {
			r.Reason, r.TaskKey = "", ""
		}, wantErr: model.ErrReasonRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			transitionRow(db, nil)
			before := *storedDef(t, db, testTaskKey)
			var in *rpc.PauseTaskReq
			if !tc.nilReq {
				in = pauseReq(testTaskKey, "上游依赖故障，先停", 1)
				if tc.mutate != nil {
					tc.mutate(in)
				}
			}
			reply, err := NewPauseTaskLogic(context.Background(), svcCtx).PauseTask(in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if reply != nil {
				t.Errorf("拒绝时不得回定义：%+v", reply)
			}
			assertRowFrozen(t, db, before)
			if db.txRuns != 0 || len(db.audits) != 0 || len(db.stateCalls) != 0 {
				t.Errorf("拒绝不该留下痕迹：txRuns=%d audits=%d stateCalls=%d",
					db.txRuns, len(db.audits), len(db.stateCalls))
			}
		})
	}
}

func TestDisableTaskRejectsBeforeOpeningTransaction(t *testing.T) {
	cases := []struct {
		name    string
		nilReq  bool
		mutate  func(*rpc.DisableTaskReq)
		wantErr error
	}{
		{name: "nil 请求", nilReq: true, wantErr: model.ErrTaskKeyEmpty},
		// 判定顺序：幂等键最先（与 Pause/Resume 同一顺序，三个入口不能各说各话）。
		{name: "空请求体", mutate: func(r *rpc.DisableTaskReq) { *r = rpc.DisableTaskReq{} },
			wantErr: model.ErrIdempotencyKeyEmpty},
		{name: "缺幂等键", mutate: func(r *rpc.DisableTaskReq) { r.IdempotencyKey = "" },
			wantErr: model.ErrIdempotencyKeyEmpty},
		{name: "缺 reason", mutate: func(r *rpc.DisableTaskReq) { r.Reason = "" },
			wantErr: model.ErrReasonRequired},
		{name: "task_key 空白", mutate: func(r *rpc.DisableTaskReq) { r.TaskKey = "\n" },
			wantErr: model.ErrTaskKeyEmpty},
		{name: "同时缺 reason 与 task_key", mutate: func(r *rpc.DisableTaskReq) {
			r.Reason, r.TaskKey = "", " "
		}, wantErr: model.ErrReasonRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			transitionRow(db, nil)
			before := *storedDef(t, db, testTaskKey)
			var in *rpc.DisableTaskReq
			if !tc.nilReq {
				in = disableReq(testTaskKey, "该任务已废弃", 1)
				if tc.mutate != nil {
					tc.mutate(in)
				}
			}
			reply, err := NewDisableTaskLogic(context.Background(), svcCtx).DisableTask(in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if reply != nil {
				t.Errorf("拒绝时不得回定义：%+v", reply)
			}
			assertRowFrozen(t, db, before)
			if db.txRuns != 0 || len(db.audits) != 0 {
				t.Errorf("拒绝不该留下痕迹：txRuns=%d audits=%d", db.txRuns, len(db.audits))
			}
		})
	}
}

func TestResumeTaskRejectsBeforeOpeningTransaction(t *testing.T) {
	cases := []struct {
		name    string
		nilReq  bool
		mutate  func(*rpc.ResumeTaskReq)
		wantErr error
	}{
		{name: "nil 请求", nilReq: true, wantErr: model.ErrTaskKeyEmpty},
		{name: "缺幂等键", mutate: func(r *rpc.ResumeTaskReq) { r.IdempotencyKey = " " },
			wantErr: model.ErrIdempotencyKeyEmpty},
		{name: "task_key 空白", mutate: func(r *rpc.ResumeTaskReq) { r.TaskKey = "  " },
			wantErr: model.ErrTaskKeyEmpty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			transitionRow(db, pausedSince(3600))
			before := *storedDef(t, db, testTaskKey)
			var in *rpc.ResumeTaskReq
			if !tc.nilReq {
				in = resumeReq(testTaskKey, 1)
				if tc.mutate != nil {
					tc.mutate(in)
				}
			}
			reply, err := NewResumeTaskLogic(context.Background(), svcCtx).ResumeTask(in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if reply != nil {
				t.Errorf("拒绝时不得回定义：%+v", reply)
			}
			assertRowFrozen(t, db, before)
			if db.txRuns != 0 || len(db.audits) != 0 {
				t.Errorf("拒绝不该留下痕迹：txRuns=%d audits=%d", db.txRuns, len(db.audits))
			}
		})
	}
}

// TestResumeTaskDoesNotRequireReason 三个动作里只有 Resume 不要求 reason。
// 判别性对照：同一份缺 reason 的入参，Pause/Disable 当场拒，Resume 正常恢复。
func TestResumeTaskDoesNotRequireReason(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	transitionRow(db, pausedSince(3600))
	p := pauseReq(testTaskKey, "", 1)
	if _, err := NewPauseTaskLogic(context.Background(), svcCtx).PauseTask(p); !errors.Is(err, model.ErrReasonRequired) {
		t.Fatalf("err = %v，期望 ErrReasonRequired", err)
	}
	if _, err := NewResumeTaskLogic(context.Background(), svcCtx).
		ResumeTask(resumeReq(testTaskKey, 1)); err != nil {
		t.Errorf("ResumeTask 不该要求 reason：%v", err)
	}
	if got := storedDef(t, db, testTaskKey); got.State != model.TaskStateEnabled {
		t.Errorf("恢复没生效：%s", model.TaskStateName(got.State))
	}
}

// TestStateOperationNotFoundRunsInsideTransaction 任务不存在时的三个入口。
//
// 这里刻意断言 txRuns==1：状态判定必须在事务里读库里最新行（恢复时尤其重要，
// 拿请求前的旧快照算计划点会算出一个已经过去的未来点），
// 所以「不存在」这个结论也是事务里得到的，不是事务前的预检。
func TestStateOperationNotFoundRunsInsideTransaction(t *testing.T) {
	for _, op := range []string{"pause", "resume", "disable"} {
		t.Run(op, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			reply, err := runStateOp(t, svcCtx, op, "report.never", "没这条任务", 1)
			if !errors.Is(err, model.ErrTaskNotFound) {
				t.Fatalf("err = %v，期望 ErrTaskNotFound", err)
			}
			if reply != nil {
				t.Errorf("任务不存在时不得回应答：%+v", reply)
			}
			if db.txRuns != 1 {
				t.Errorf("状态判定应在事务内读最新行，txRuns=%d", db.txRuns)
			}
			if len(db.defs) != 0 || len(db.audits) != 0 || len(db.stateCalls) != 0 {
				t.Errorf("不该留下任何写痕迹：defs=%d audits=%d stateCalls=%d",
					len(db.defs), len(db.audits), len(db.stateCalls))
			}
		})
	}
}

// --- PauseTask ---

func TestPauseTaskTakesTaskOutOfDueScan(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	transitionRow(db, nil)
	oldPointer := storedDef(t, db, testTaskKey).NextFireAt

	reply, err := NewPauseTaskLogic(context.Background(), svcCtx).
		PauseTask(pauseReq(testTaskKey, "下游 rights 服务变更，暂停两小时", 1))
	if err != nil {
		t.Fatal(err)
	}
	row := storedDef(t, db, testTaskKey)
	if row.State != model.TaskStatePaused {
		t.Errorf("state = %s，期望 paused", model.TaskStateName(row.State))
	}
	// 暂停的全部效果就是这一条：指针归零 => 退出到期扫描。
	if row.NextFireAt != 0 {
		t.Errorf("next_fire_at = %d，期望 0（暂停必须退出到期扫描）", row.NextFireAt)
	}
	if row.Version != 2 {
		t.Errorf("version = %d，期望 2", row.Version)
	}
	// SetState 的 SET 列表带 operator = ?，所以定义行上的「最近变更人」也跟着走
	// （对比 UpdateTask：那条 SQL 也写 operator，logic 却没把新操作人赋进行里，见缺陷 G）。
	if row.Operator != "ops.carol" {
		t.Errorf("定义行 operator = %q，期望 ops.carol", row.Operator)
	}
	if row.Handler != "report.daily" || row.IntervalSeconds != 3600 || row.ScheduleType != model.ScheduleTypeInterval {
		t.Errorf("暂停不该改动调度事实：%+v", row)
	}

	// 回读而非快照：事务里读到的 cur 是 version=1 的旧行，应答必须是提交后的新行。
	if !reply.Changed {
		t.Error("changed = false，期望 true（状态确实变了）")
	}
	if reply.AuditId != 1 {
		t.Errorf("audit_id = %d，期望指向本次写入的审计行", reply.AuditId)
	}
	if got := reply.GetDefinition(); got.GetVersion() != 2 ||
		got.GetState() != rpc.TaskState_TASK_STATE_PAUSED || got.GetNextFireAt() != 0 {
		t.Errorf("应答回的是改动前的快照：%+v", got)
	}

	assertStateCalls(t, db, []stateCall{{
		from: model.TaskStateUnspecified, to: model.TaskStatePaused, expectedVersion: 1, operator: "ops.carol",
	}}, "暂停")
	// from=Unspecified 是既定分工：allowedFrom 可以有多值（Disable 就是两个），
	// 单值守卫会误伤合法迁移，来源判定交给 version。

	audit := onlyAudit(t, db, model.AuditActionPause)
	if audit.FromState != "enabled" || audit.ToState != "paused" {
		t.Errorf("审计状态文本应是可读的 enabled/paused，得到 %s/%s", audit.FromState, audit.ToState)
	}
	// reason 没有对应的列（cron_task_audit 只有 detail），它必须原样活在 detail 里。
	if got := detailReason(t, audit); got != "下游 rights 服务变更，暂停两小时" {
		t.Errorf("审计 detail.reason = %q", got)
	}
	if audit.Operator != "ops.carol" || audit.TraceID != "trace-pause" {
		t.Errorf("审计丢了操作人或 trace_id：%+v", audit)
	}
	if v, ok := detailInt64(t, audit, "expected_version"); !ok || v != 1 {
		t.Errorf("审计 detail.expected_version = %v，期望 1", v)
	}
	if v, ok := detailInt64(t, audit, "prev_next_fire"); !ok || v != oldPointer {
		t.Errorf("审计 detail.prev_next_fire = %v，期望改动前的指针 %d", v, oldPointer)
	}
}

func TestPauseTaskTrimsTaskKey(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	transitionRow(db, nil)
	if _, err := NewPauseTaskLogic(context.Background(), svcCtx).
		PauseTask(pauseReq("  "+testTaskKey+"  ", "窗口期暂停", 1)); err != nil {
		t.Fatal(err)
	}
	if got := storedDef(t, db, testTaskKey); got.State != model.TaskStatePaused {
		t.Errorf("带空白的 task_key 没命中既有任务：%s", model.TaskStateName(got.State))
	}
	if len(db.defs) != 1 {
		t.Errorf("库里多了任务定义（现有 %d 行）", len(db.defs))
	}
}

func TestPauseTaskIsIdempotentWhenAlreadyPaused(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	transitionRow(db, func(d *model.TaskDefinition) {
		d.State = model.TaskStatePaused
		d.Version = 3
		d.NextFireAt = 0
	})
	before := *storedDef(t, db, testTaskKey)

	reply, err := NewPauseTaskLogic(context.Background(), svcCtx).
		PauseTask(pauseReq(testTaskKey, "重复点击", 3))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Changed {
		t.Error("changed = true，目标态已达成时应回 false")
	}
	if reply.AuditId != 0 {
		t.Errorf("audit_id = %d，幂等重入不该写审计（写了反而造出「反复暂停」的假痕迹）", reply.AuditId)
	}
	if reply.GetDefinition().GetState() != rpc.TaskState_TASK_STATE_PAUSED {
		t.Errorf("应答状态：%+v", reply.GetDefinition())
	}
	assertRowFrozen(t, db, before)
	if len(db.audits) != 0 {
		t.Errorf("幂等重入写了审计：%+v", db.audits)
	}
	if len(db.stateCalls) != 0 {
		t.Errorf("幂等重入不该发出 SET：%+v", db.stateCalls)
	}
	if db.txRuns != 1 {
		t.Errorf("幂等判定要在事务里读最新行，txRuns=%d", db.txRuns)
	}
}

// --- 来源状态矩阵 ---

// TestStateTransitionSourceStateMatrix 三个动作 × 三个来源状态的全组合。
// 期望值全部由「业务语义」给出，而不是从实现反推：
//   - 暂停只对启用中的任务有意义；
//   - 恢复只接受 PAUSED，DISABLED 代表「不再被支持」，要重新注册而不是走捷径；
//   - 停用允许 ENABLED/PAUSED 两个来源（运营发现问题时不必先恢复再停用）。
func TestStateTransitionSourceStateMatrix(t *testing.T) {
	cases := []struct {
		op      string
		from    int32
		wantErr error // nil 表示放行
		changed bool  // 仅 wantErr==nil 有意义
	}{
		{"pause", model.TaskStateEnabled, nil, true},
		{"pause", model.TaskStatePaused, nil, false},
		{"pause", model.TaskStateDisabled, model.ErrStateTransition, false},
		{"resume", model.TaskStatePaused, nil, true},
		{"resume", model.TaskStateEnabled, nil, false},
		{"resume", model.TaskStateDisabled, model.ErrStateTransition, false},
		{"disable", model.TaskStateEnabled, nil, true},
		{"disable", model.TaskStatePaused, nil, true},
		{"disable", model.TaskStateDisabled, nil, false},
	}
	for _, tc := range cases {
		name := model.TaskStateName(tc.from) + "->" + tc.op
		t.Run(name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			transitionRow(db, func(d *model.TaskDefinition) {
				d.State = tc.from
				d.Version = 5
				d.NextFireAt = 1_700_000_000
			})
			before := *storedDef(t, db, testTaskKey)

			reply, err := runStateOp(t, svcCtx, tc.op, testTaskKey, "矩阵用例", 5)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if reply != nil || len(db.audits) != 0 || len(db.stateCalls) != 0 {
					t.Errorf("非法迁移被放行了：%+v audits=%d sets=%+v", reply, len(db.audits), db.stateCalls)
				}
				assertRowFrozen(t, db, before)
				return
			}
			if reply.GetChanged() != tc.changed {
				t.Errorf("changed = %v，期望 %v", reply.GetChanged(), tc.changed)
			}
			if !tc.changed {
				// 幂等分支：不改版本、不写审计、也不动指针。
				assertRowFrozen(t, db, before)
				if len(db.audits) != 0 || len(db.stateCalls) != 0 {
					t.Errorf("幂等重入留下痕迹：audits=%d sets=%+v", len(db.audits), db.stateCalls)
				}
				return
			}
			if len(db.audits) != 1 || len(db.stateCalls) != 1 {
				t.Fatalf("真实迁移应恰好一条审计 + 一次 SET：audits=%d sets=%+v",
					len(db.audits), db.stateCalls)
			}
			if db.audits[0].FromState != model.TaskStateName(tc.from) {
				t.Errorf("审计 from_state = %q，期望 %q", db.audits[0].FromState, model.TaskStateName(tc.from))
			}
		})
	}
}

// --- DisableTask ---

func TestDisableTaskClearsPointerFromBothSources(t *testing.T) {
	for _, from := range []int32{model.TaskStateEnabled, model.TaskStatePaused} {
		t.Run(model.TaskStateName(from)+"->disable", func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			transitionRow(db, func(d *model.TaskDefinition) {
				d.State = from
				d.Version = 2
				d.NextFireAt = 1_700_000_000
			})

			reply, err := NewDisableTaskLogic(context.Background(), svcCtx).
				DisableTask(disableReq(testTaskKey, "功能下线，永久停用", 2))
			if err != nil {
				t.Fatal(err)
			}
			row := storedDef(t, db, testTaskKey)
			if row.State != model.TaskStateDisabled {
				t.Errorf("state = %s", model.TaskStateName(row.State))
			}
			// 停用比暂停更彻底：SetState 的 CASE 只管 PAUSED，所以这里必须由 logic 显式归零。
			if row.NextFireAt != 0 {
				t.Errorf("next_fire_at = %d，期望 0", row.NextFireAt)
			}
			if row.Version != 3 || row.Operator != "ops.erin" {
				t.Errorf("库里行没推进：%+v", row)
			}
			if reply.GetDefinition().GetVersion() != 3 {
				t.Errorf("应答回的是旧快照：version=%d", reply.GetDefinition().GetVersion())
			}
			assertStateCalls(t, db, []stateCall{{
				from: model.TaskStateUnspecified, to: model.TaskStateDisabled,
				expectedVersion: 2, operator: "ops.erin",
			}}, "停用")
			audit := onlyAudit(t, db, model.AuditActionDisable)
			if audit.ToState != "disabled" || audit.FromState != model.TaskStateName(from) {
				t.Errorf("审计状态文本：%+v", audit)
			}
			if got := detailReason(t, audit); got != "功能下线，永久停用" {
				t.Errorf("reason 没进审计 detail：%q", got)
			}
			if v, ok := detailInt64(t, audit, "next_fire_at"); !ok || v != 0 {
				t.Errorf("审计 detail.next_fire_at = %v，期望 0（停用最彻底）", v)
			}
			if v, ok := detailInt64(t, audit, "prev_next_fire"); !ok || v != 1_700_000_000 {
				t.Errorf("审计 detail.prev_next_fire = %v，期望 1700000000", v)
			}
		})
	}
}

// --- ResumeTask：指针按 MisfirePolicy 重算 ---

func TestResumeTaskRecomputesPointerPerMisfirePolicy(t *testing.T) {
	cases := []struct {
		name    string
		policy  int32
		wantOff int64 // 期望指针 = now + wantOff
	}{
		// 暂停期跨过了 02:00 这个计划点（interval=3600，last_fire_at = now-7200）。
		{"FIRE_ONCE_NOW 合并补跑一次", model.MisfirePolicyFireOnceNow, 0},
		{"SKIP_TO_NEXT 丢弃过期点", model.MisfirePolicySkipToNext, 3600},
		{"FIRE_ALL 从最早未跑点逐个补齐", model.MisfirePolicyFireAll, -3600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			transitionRow(db, func(d *model.TaskDefinition) {
				d.State = model.TaskStatePaused
				d.LastFireAt = fakeNow() - 7200
				d.NextFireAt = 0
				d.MisfirePolicy = tc.policy
				d.Version = 4
			})

			reply, err := NewResumeTaskLogic(context.Background(), svcCtx).
				ResumeTask(resumeReq(testTaskKey, 4))
			if err != nil {
				t.Fatal(err)
			}
			row := storedDef(t, db, testTaskKey)
			if row.State != model.TaskStateEnabled {
				t.Errorf("state = %s，期望 enabled", model.TaskStateName(row.State))
			}
			assertDelta(t, row.NextFireAt, tc.wantOff, "恢复后的指针")
			if row.Version != 5 {
				t.Errorf("version = %d，期望 5", row.Version)
			}
			if reply.GetDefinition().GetNextFireAt() != row.NextFireAt {
				t.Errorf("应答指针与库里不一致：%d vs %d", reply.GetDefinition().GetNextFireAt(), row.NextFireAt)
			}

			audit := onlyAudit(t, db, model.AuditActionResume)
			if audit.FromState != "paused" || audit.ToState != "enabled" {
				t.Errorf("审计状态文本：%+v", audit)
			}
			// ResumeTaskReq 没有 reason 字段（契约缺口，README 已登记），
			// 于是审计原因只能是固定文本；可断言的底线是「detail 里永远有原因」。
			if reason := strings.TrimSpace(detailReason(t, audit)); reason == "" {
				t.Error("恢复动作的审计 reason 为空，答不出「为什么恢复」")
			} else if !strings.Contains(reason, "misfire_policy") {
				t.Errorf("恢复审计没说明按什么策略恢复：%q", reason)
			}
			got := detailFields(t, audit)
			if got["resumed_by"] != "ops.dave" {
				t.Errorf("审计 detail.resumed_by = %v，期望 ops.dave", got["resumed_by"])
			}
			if v, ok := detailInt64(t, audit, "prev_next_fire"); !ok || v != 0 {
				t.Errorf("审计 detail.prev_next_fire = %v，期望暂停期间的 0", v)
			}
			if v, ok := detailInt64(t, audit, "next_fire_at"); !ok || v != row.NextFireAt {
				t.Errorf("审计 detail.next_fire_at = %v，期望库里指针 %d", v, row.NextFireAt)
			}
		})
	}
}

// TestResumeTaskKeepsFuturePlanWhenNoPointWasSpanned FIRE_ONCE_NOW 的另一半分支：
// 暂停期没跨过任何计划点时按原计划继续，绝不提前补跑。
func TestResumeTaskKeepsFuturePlanWhenNoPointWasSpanned(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	lastFire := fakeNow() - 60
	transitionRow(db, func(d *model.TaskDefinition) {
		d.State = model.TaskStatePaused
		d.LastFireAt = lastFire
		d.NextFireAt = 0
		d.MisfirePolicy = model.MisfirePolicyFireOnceNow
	})
	if _, err := NewResumeTaskLogic(context.Background(), svcCtx).
		ResumeTask(resumeReq(testTaskKey, 1)); err != nil {
		t.Fatal(err)
	}
	got := storedDef(t, db, testTaskKey).NextFireAt
	// interval=3600 且 last_fire_at 只过去 60 秒 => 原计划点仍在未来。
	if got != lastFire+3600 {
		t.Errorf("指针 = %d，期望 %d（原计划点，不该合并成 now）", got, lastFire+3600)
	}
	if got <= fakeNow() {
		t.Errorf("指针 %d 不该落在现在或过去", got)
	}
}

// TestResumeTaskCronSkipsToNextPlanPoint 用 cron 任务验证 SKIP_TO_NEXT 走的是
// 「now 之后第一个命中点」。这里只断言性质（时刻形如 03:00:00Z 且晚于现在），
// 不在测试里复刻 cron 表达式解析。
func TestResumeTaskCronSkipsToNextPlanPoint(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	transitionRow(db, func(d *model.TaskDefinition) {
		d.State = model.TaskStatePaused
		d.ScheduleType = model.ScheduleTypeCron
		d.CronExpr = "0 3 * * *"
		d.IntervalSeconds = 0
		d.LastFireAt = fakeNow() - 86400
		d.NextFireAt = 0
		d.MisfirePolicy = model.MisfirePolicySkipToNext
	})
	if _, err := NewResumeTaskLogic(context.Background(), svcCtx).
		ResumeTask(resumeReq(testTaskKey, 1)); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(storedDef(t, db, testTaskKey).NextFireAt, 0).UTC()
	if at.Hour() != 3 || at.Minute() != 0 || at.Second() != 0 {
		t.Errorf("指针应落在某个 03:00:00Z，得到 %s", at)
	}
	if !at.After(time.Now()) {
		t.Errorf("SKIP_TO_NEXT 给的是未来的点，得到 %s", at)
	}
}

func TestResumeTaskManualTaskExitsDueScan(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	transitionRow(db, func(d *model.TaskDefinition) {
		d.State = model.TaskStatePaused
		d.ScheduleType = model.ScheduleTypeManual
		d.IntervalSeconds = 0
		d.NextFireAt = 1_700_000_000 // 停用前遗留的旧计划点
	})
	if _, err := NewResumeTaskLogic(context.Background(), svcCtx).
		ResumeTask(resumeReq(testTaskKey, 1)); err != nil {
		t.Fatal(err)
	}
	row := storedDef(t, db, testTaskKey)
	if row.State != model.TaskStateEnabled {
		t.Errorf("state = %s", model.TaskStateName(row.State))
	}
	// 手动任务恢复后仍不该被到期扫到：指针必须归零。
	if row.NextFireAt != 0 {
		t.Errorf("手动任务指针 = %d，期望 0", row.NextFireAt)
	}
	assertStateCalls(t, db, []stateCall{{
		from: model.TaskStateUnspecified, to: model.TaskStateEnabled, expectedVersion: 1, operator: "ops.dave",
	}}, "恢复手动任务")
}

// --- 原子性：审计写失败必须连状态带指针一起回滚 ---

func TestStateTransitionRollsBackWhenAuditFails(t *testing.T) {
	cases := []struct {
		op     string
		mutate func(*model.TaskDefinition)
	}{
		{"pause", nil},
		// 恢复会连带动指针，回滚必须把指针也退回暂停期间的 0。
		{"resume", pausedSince(7200)},
		{"disable", nil},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			transitionRow(db, tc.mutate)
			before := *storedDef(t, db, testTaskKey)
			db.auditErr = errors.New("audit insert rejected")

			reply, err := runStateOp(t, svcCtx, tc.op, testTaskKey, "要回滚的原因", before.Version)
			if err == nil || !strings.Contains(err.Error(), "audit insert rejected") {
				t.Fatalf("err = %v，期望原样冒出的审计错误", err)
			}
			if reply != nil {
				t.Errorf("失败时不得回应答：%+v", reply)
			}
			after := *storedDef(t, db, testTaskKey)
			if after != before {
				t.Errorf("状态/指针改动了、审计却没落地（半条结果）：\n前 %+v\n后 %+v", before, after)
			}
			if db.txRuns != 1 || db.txRollups != 1 {
				t.Errorf("事务应回滚一次：runs=%d rollups=%d", db.txRuns, db.txRollups)
			}
			if len(db.audits) != 0 {
				t.Errorf("回滚后不该有审计：%+v", db.audits)
			}
		})
	}
}

// --- 连续迁移：乐观锁版本号必须跟着上一次的结果走 ---

func TestConsecutiveStateTransitionsReadLatestRow(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	transitionRow(db, nil)

	if _, err := NewPauseTaskLogic(context.Background(), svcCtx).
		PauseTask(pauseReq(testTaskKey, "先暂停", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDisableTaskLogic(context.Background(), svcCtx).
		DisableTask(disableReq(testTaskKey, "暂停确认无副作用，直接停用", 2)); err != nil {
		t.Fatal(err)
	}
	// 轨迹 [1,2]：第二次迁移用的是第一次提交后的版本，而不是请求里的数字。
	assertStateCalls(t, db, []stateCall{
		{from: model.TaskStateUnspecified, to: model.TaskStatePaused, expectedVersion: 1, operator: "ops.carol"},
		{from: model.TaskStateUnspecified, to: model.TaskStateDisabled, expectedVersion: 2, operator: "ops.erin"},
	}, "暂停后停用")
	row := storedDef(t, db, testTaskKey)
	if row.State != model.TaskStateDisabled || row.Version != 3 || row.Operator != "ops.erin" {
		t.Errorf("库里行：%+v", row)
	}
	if len(db.audits) != 2 || db.audits[1].Action != model.AuditActionDisable {
		t.Errorf("两次迁移应留下两条审计：%+v", db.audits)
	}
	// 读侧必须能直接看到同样的结论（回读的是落库行，不是内存快照）。
	getReply, err := NewGetTaskLogic(context.Background(), svcCtx).
		GetTask(&rpc.GetTaskReq{TaskKey: testTaskKey})
	if err != nil {
		t.Fatal(err)
	}
	if getReply.GetDefinition().GetState() != rpc.TaskState_TASK_STATE_DISABLED ||
		getReply.GetDefinition().GetVersion() != 3 {
		t.Errorf("读侧与库里不一致：%+v", getReply.GetDefinition())
	}
}

// --- 哨兵用例（钉住现状，改生产代码后必须删除并同步 README） ---

// TestStateTransitionIgnoresExpectedVersionSentinel 哨兵用例。
//
// TODO(缺陷 E)：PauseTaskReq/ResumeTaskReq/DisableTaskReq 都带 expected_version，
// 但三个入口都没读它——stateTransition 用的是事务内刚读到的 cur.Version。
// 后果：调用方无法表达「我以为它还是 v1」，两个后台/两次重试会互相覆盖，
// 只有真库里行版本被第三方推进时 ErrVersionConflict 才可能触发。
// 判别性证据：应答与审计里的版本号都来自服务端自己读的行，与声称值无关。
//
// 说明：真正的丢更新竞态需要「事务内读完之后、写之前版本被别人改动」，
// 现有 fake 只在 UpdateMutableTx 上留了 casMissOnUpdate 注入缝，
// SetStateTx 没有对应开关，所以这一半只能靠真库并发验，已登记 README 缺口。
func TestStateTransitionIgnoresExpectedVersionSentinel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	transitionRow(db, nil) // 库里 version=1

	reply, err := NewPauseTaskLogic(context.Background(), svcCtx).
		PauseTask(pauseReq(testTaskKey, "声称的版本号根本不存在", 999))
	if errors.Is(err, model.ErrVersionConflict) {
		t.Errorf("现状已改变（expected_version=999 被拒）：请删除本哨兵并同步 README")
		return
	}
	if err != nil {
		t.Fatalf("意外错误：%v", err)
	}
	assertStateCalls(t, db, []stateCall{{
		from: model.TaskStateUnspecified, to: model.TaskStatePaused, expectedVersion: 1, operator: "ops.carol",
	}}, "被忽略的 expected_version")
	if reply.GetDefinition().GetVersion() != 2 {
		t.Errorf("version = %d，期望 2", reply.GetDefinition().GetVersion())
	}
	audit := onlyAudit(t, db, model.AuditActionPause)
	if v, ok := detailInt64(t, audit, "expected_version"); !ok || v != 1 {
		t.Errorf("审计 detail.expected_version = %v，期望服务端读到的 1（999 被丢弃）", v)
	}
	t.Log("缺陷 E 仍在：三个状态迁移入口都不读请求里的 expected_version")
}

// TestPauseTaskAuditKeepsPrePausePointerSentinel 哨兵用例。
//
// TODO(缺陷 I)：stateTransition 的审计 detail 里 next_fire_at 落的是
// 「planNextFireAt 的返回值」，而 Pause 没有这个回调，于是它等于改动前的旧值；
// 真正清指针的是 model.SetState 的 SQL CASE。结果暂停审计里
// next_fire_at == prev_next_fire（一个仍在未来的时刻），与库里真实的 0 相反，
// 按审计排障的人会以为暂停后还会跑一次。
func TestPauseTaskAuditKeepsPrePausePointerSentinel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	transitionRow(db, nil)
	oldPointer := storedDef(t, db, testTaskKey).NextFireAt
	if oldPointer <= fakeNow() {
		t.Fatalf("布景就要一个未来的指针，得到 %d", oldPointer)
	}
	if _, err := NewPauseTaskLogic(context.Background(), svcCtx).
		PauseTask(pauseReq(testTaskKey, "钉住审计保真度缺口", 1)); err != nil {
		t.Fatal(err)
	}
	row := storedDef(t, db, testTaskKey)
	if row.NextFireAt != 0 {
		t.Fatalf("库里指针 = %d，期望 0（真已暂停）", row.NextFireAt)
	}
	audit := onlyAudit(t, db, model.AuditActionPause)
	next, hasNext := detailInt64(t, audit, "next_fire_at")
	prev, hasPrev := detailInt64(t, audit, "prev_next_fire")
	if !hasNext || !hasPrev {
		t.Fatalf("审计 detail 缺 next_fire_at/prev_next_fire：%+v", detailFields(t, audit))
	}
	if next != prev {
		t.Errorf("现状已改变（暂停审计区分了新旧指针 next=%d prev=%d）：请删除本哨兵并同步 README", next, prev)
		return
	}
	t.Log("缺陷 I 仍在：暂停审计写的 next_fire_at 就是旧指针，与库里的 0 相反")
}

// TestResumeTaskRejectsUnsetMisfirePolicySentinel 哨兵用例（缺陷 C 的完整链条）。
//
// TODO(缺陷 C)：RegisterTask 用 definitionFromProto(..., full=false)（registertasklogic.go:44），
// 于是 helpers.go 里「未声明 misfire_policy 即默认 FIRE_ONCE_NOW」这条注册期默认值不生效，
// 库里落 MisfirePolicyUnspecified(0)；model.ValidateTaskDefinition 又不检查这一列，
// 于是注册成功、暂停成功，等到恢复才在 planResumeFireAt 的 default 分支炸出
// ErrInvalidSchedule —— 一个「暂停后再也起不来」的任务被登记进了线上。
func TestResumeTaskRejectsUnsetMisfirePolicySentinel(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	registerOK(t, svcCtx, "report.daily")

	// 1) 注册时刻意不给 misfire_policy（契约默认值 FIRE_ONCE_NOW）。
	def := intervalProto(testTaskKey)
	def.MisfirePolicy = rpc.MisfirePolicy_MISFIRE_POLICY_UNSPECIFIED
	if _, err := NewRegisterTaskLogic(context.Background(), svcCtx).RegisterTask(registerReq(def)); err != nil {
		t.Fatalf("注册应当成功：%v", err)
	}
	row := storedDef(t, db, testTaskKey)
	if row.MisfirePolicy != model.MisfirePolicyUnspecified {
		t.Errorf("现状已改变（注册期补了默认策略 misfire=%d）：请删除本哨兵并同步 README", row.MisfirePolicy)
		return
	}
	// 2) 暂停照常成功。
	if _, err := NewPauseTaskLogic(context.Background(), svcCtx).
		PauseTask(pauseReq(testTaskKey, "例行暂停", 1)); err != nil {
		t.Fatalf("暂停应当成功：%v", err)
	}
	if got := storedDef(t, db, testTaskKey); got.State != model.TaskStatePaused || got.Version != 2 {
		t.Fatalf("暂停后的行：%+v", got)
	}

	// 3) 恢复：任务自己把自己锁死在 PAUSED。
	_, err := NewResumeTaskLogic(context.Background(), svcCtx).ResumeTask(resumeReq(testTaskKey, 2))
	if !errors.Is(err, model.ErrInvalidSchedule) {
		t.Errorf("err = %v，期望 ErrInvalidSchedule（misfire_policy=0 无人兜底）", err)
	}
	after := storedDef(t, db, testTaskKey)
	if after.State != model.TaskStatePaused || after.Version != 2 || after.NextFireAt != 0 {
		t.Errorf("失败的恢复必须一行都不改：%+v", after)
	}
	if len(db.audits) != 2 || db.audits[1].Action != model.AuditActionPause {
		t.Errorf("恢复失败不该有 resume 审计：%+v", db.audits)
	}
	if db.txRuns != 3 || db.txRollups != 1 {
		t.Errorf("事务计数：runs=%d rollups=%d，期望 3/1", db.txRuns, db.txRollups)
	}
	t.Log("缺陷 C 仍在：未声明 misfire_policy 的任务注册后一旦暂停就无法恢复")
}
