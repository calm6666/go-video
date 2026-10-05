package logic

// admintask_logic_test.go 覆盖 SubmitAdminTask / GetAdminTask / ListAdminTasks。
//
// 这三条是任务编排的「入口与读面」，要紧的结论依次是：
//   - 提交必须**先全部校验再落库**（守卫拒绝时零依赖调用，一条 SQL 都不发）；
//   - request_id 幂等是「重放返回既有任务、不重复建步骤、不重复留痕」三件事一起成立，
//     少一件就会让运营重试时看到两个任务或两份审计；
//   - 目标去重必须**同时**保证 step_no 连续且 total 与步骤数一致，否则推进时会出现空洞；
//   - 读接口不鉴权只校验「有无可信操作者」（logic/helpers.go:20-23），
//     但分页/筛选的兜底口径（pn<=0→1、ps<=0→20、ps>100→100）要在进 model 之前定死。
//
// 本轮已核实并钉住的生产现状（不改生产代码，AGENTS.md §10）：
//   - 步骤写入失败时任务行**已经落库**且状态 pending、无步骤、无审计
//     （repository/task.go:163-167 的注释就是这个设计）。这不是缺陷，但后果必须可见：
//     见 TestSubmitAdminTask步骤写入失败任务留在pending且无步骤。
//   - repository/task.go:133-135 的第二次 len(steps)==0 判定不可达
//     （上面第 98 行已经拒过空步骤，而循环里每个首次出现的目标都会 append），
//     属于死代码，登记在 README 的「不可离线覆盖分支」。
//   - repository/task.go:153-155 的 existing == nil 分支只在 Insert 与 FindByRequestID
//     **两条语句之间**该行被并发删除时才可达，离线没有任何时序缝能插进去，
//     同样登记 README。

import (
	"errors"
	"fmt"
	"testing"

	"go-video/services/operation/model"
	"go-video/services/operation/rpc"
)

// errSubmitProbe 是注入到替身里的探针错误（报文里带得出来，便于断言外传口径）。
var errSubmitProbe = errors.New("probe: admin_task write failed")

// actorWith 在 opCtx 基础上**总是**改写幂等键（request_id 是被测对象，
// 不能沿用 opCtx 的默认值——「漏传 request_id」这条用例就是这么触发的），
// 操作者名只在显式给出时才覆写（要测退化口径请直接构造 OpContext）。
func actorWith(operator int64, requestID, name string) *rpc.OpContext {
	c := opCtx(operator)
	c.RequestId = requestID
	if name != "" {
		c.OperatorName = name
	}
	return c
}

// spec 是一条步骤声明。
func spec(targetType, targetID string) *rpc.TaskStepSpec {
	return &rpc.TaskStepSpec{TargetType: targetType, TargetId: targetID}
}

// submitReq 组一个提交请求。
func submitReq(operator int64, requestID, taskType, params string, steps ...*rpc.TaskStepSpec) *rpc.SubmitAdminTaskReq {
	return &rpc.SubmitAdminTaskReq{
		Ctx: actorWith(operator, requestID, ""), TaskType: taskType, Params: params, Steps: steps,
	}
}

// stepFields 把步骤行摘要成断言用的字符串（一次比对覆盖步号/类型/ID/状态四列）。
func stepFields(steps []*model.AdminTaskStep) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, fmt.Sprintf("%d/%s/%s/%s", s.StepNo, s.TargetType, s.TargetID, s.State))
	}
	return out
}

func TestSubmitAdminTask提交任务落库并留痕(t *testing.T) {
	e := newEnv(t)
	started := nowUnix()
	reply, err := submitTaskCall(t, e, submitReq(9001, "req-submit-1",
		model.TaskTypeBatchOfflineSubmission, `{"reason":"版权到期"}`,
		spec("submission", "101"), spec("submission", "102")))
	wantOK(t, reply, err, "提交批量下架任务")
	to := nowUnix()

	taskID := reply.Task.TaskId
	if taskID == 0 {
		t.Fatalf("应答未带回 task_id：%+v", reply.Task)
	}
	// 顺序本身就是结论：任务行必须先于步骤（步骤要带 task_id），审计必须最后一步
	// （前面任何一步失败都还不该留下「已提交」的留痕）。
	wantOps(t, "提交轨迹", e.ops(0), []string{
		"admin_task.Insert:req-submit-1",
		"admin_task_step.InsertBatch:2",
		"audit_index.Insert:admin_task.submit/ok",
	})
	wantEQ(t, "提交应答", "reused", reply.Reused, false)
	wantEQ(t, "提交应答", "task_type", reply.Task.TaskType, model.TaskTypeBatchOfflineSubmission)
	wantEQ(t, "提交应答", "state", reply.Task.State, model.TaskStatePending)
	wantEQ(t, "提交应答", "request_id", reply.Task.RequestId, "req-submit-1")
	wantEQ(t, "提交应答", "total", reply.Task.Total, int32(2))
	wantEQ(t, "提交应答", "succeeded", reply.Task.Succeeded, int32(0))
	wantEQ(t, "提交应答", "failed", reply.Task.Failed, int32(0))
	wantEQ(t, "提交应答", "progress", reply.Task.Progress, int32(0))
	wantEQ(t, "提交应答", "operator_id", reply.Task.OperatorId, int64(9001))
	wantEQ(t, "提交应答", "trace_id", reply.Task.TraceId, "trace-op-1")
	wantEQ(t, "提交应答", "params", reply.Task.Params, `{"reason":"版权到期"}`)
	wantEQ(t, "提交应答", "started_at（未推进必须为 0）", reply.Task.StartedAt, int64(0))
	wantEQ(t, "提交应答", "finished_at（未结束必须为 0）", reply.Task.FinishedAt, int64(0))
	wantTSWindow(t, "提交应答", "ctime", reply.Task.Ctime, started, to)

	// 落库口径独立回读：应答是内存投影，替身里的行才是「真的写了什么」。
	row := e.st.task.get(taskID)
	if row == nil {
		t.Fatalf("op_admin_task 里没有任务行 %d", taskID)
	}
	wantEQ(t, "落库", "state", row.State, model.TaskStatePending)
	wantEQ(t, "落库", "request_id", row.RequestID, "req-submit-1")
	wantEQ(t, "落库", "operator", row.Operator, int64(9001))
	wantEQ(t, "落库", "total", row.Total, int32(2))

	steps := e.st.step.stepsOf(taskID)
	wantOps(t, "落库步骤（步号/目标类型/目标ID/状态）", stepFields(steps), []string{
		"1/submission/101/pending",
		"2/submission/102/pending",
	})

	audit := e.st.audit.only(t)
	wantEQ(t, "提交审计", "action", audit.Action, "admin_task.submit")
	wantEQ(t, "提交审计", "resource_type", audit.ResourceType, "admin_task")
	wantEQ(t, "提交审计", "resource_id", audit.ResourceID, itoa(taskID))
	wantEQ(t, "提交审计", "result", audit.Result, model.AuditResultOK)
	wantEQ(t, "提交审计", "admin_id", audit.AdminID, int64(9001))
	wantHex32(t, "提交审计", "ip_hash", audit.IPHash)
	wantNotContains(t, "提交审计（IP 不得明文入库）", audit.UserAgent, "203.0.113.9")
}

func TestSubmitAdminTask守卫拒绝后零依赖调用(t *testing.T) {
	base := model.TaskTypeBatchOfflineSubmission
	cases := []struct {
		name     string
		in       *rpc.SubmitAdminTaskReq
		wantSent error // nil 表示只断言「确实报错」
		needle   string
	}{
		{
			name:     "无操作者上下文",
			in:       &rpc.SubmitAdminTaskReq{TaskType: base, Steps: []*rpc.TaskStepSpec{spec("submission", "1")}},
			wantSent: ErrInvalidOperator,
		},
		{
			name:     "operator_id 为 0",
			in:       &rpc.SubmitAdminTaskReq{Ctx: &rpc.OpContext{OperatorId: 0, RequestId: "r"}, TaskType: base, Steps: []*rpc.TaskStepSpec{spec("submission", "1")}},
			wantSent: ErrInvalidOperator,
		},
		{
			name:     "operator_id 为负",
			in:       &rpc.SubmitAdminTaskReq{Ctx: actorWith(-1, "r", ""), TaskType: base, Steps: []*rpc.TaskStepSpec{spec("submission", "1")}},
			wantSent: ErrInvalidOperator,
		},
		{
			name:   "缺 request_id",
			in:     submitReq(9001, "", base, "", spec("submission", "1")),
			needle: "request_id is required for idempotent task submit",
		},
		{
			name:   "request_id 全空白",
			in:     submitReq(9001, "   ", base, "", spec("submission", "1")),
			needle: "request_id is required",
		},
		{
			name:     "未知任务类型",
			in:       submitReq(9001, "req-1", "batch_send_push", "", spec("submission", "1")),
			wantSent: ErrTaskTypeUnknown,
		},
		{
			name:     "params 不是 JSON",
			in:       submitReq(9001, "req-1", base, "reason=版权到期", spec("submission", "1")),
			wantSent: ErrTaskParamsNotJSON,
		},
		{
			name:     "params 是 JSON 数组",
			in:       submitReq(9001, "req-1", base, `[{"reason":"x"}]`, spec("submission", "1")),
			wantSent: ErrTaskParamsNotJSON,
		},
		{
			name:     "空步骤列表",
			in:       submitReq(9001, "req-1", base, "{}"),
			wantSent: ErrTaskEmptySteps,
		},
		{
			name:   "步骤目标类型不匹配任务类型",
			in:     submitReq(9001, "req-1", base, "{}", spec("episode", "1")),
			needle: `only accepts target_type "submission", got "episode"`,
		},
		{
			name:   "目标 ID 非数字",
			in:     submitReq(9001, "req-1", base, "{}", spec("submission", "aid-101")),
			needle: `step 1 target_id "aid-101" is not a positive id`,
		},
		{
			name:   "目标 ID 为 0",
			in:     submitReq(9001, "req-1", base, "{}", spec("submission", "0")),
			needle: `step 1 target_id "0" is not a positive id`,
		},
		{
			name:   "目标 ID 为负",
			in:     submitReq(9001, "req-1", base, "{}", spec("submission", "-7")),
			needle: `step 1 target_id "-7" is not a positive id`,
		},
		{
			name:   "目标 ID 为空",
			in:     submitReq(9001, "req-1", base, "{}", spec("submission", "")),
			needle: `step 1 target_id "" is not a positive id`,
		},
		{
			name:   "第二条步骤非法时报错指向步号",
			in:     submitReq(9001, "req-1", base, "{}", spec("submission", "1"), spec("submission", "bad")),
			needle: `step 2 target_id "bad"`,
		},
		{
			name:   "目标 ID 超出 int64",
			in:     submitReq(9001, "req-1", base, "{}", spec("submission", "99999999999999999999")),
			needle: "is not a positive id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			reply, err := submitTaskCall(t, e, tc.in)
			if reply != nil {
				t.Errorf("应答 = %+v, want nil", reply)
			}
			wantErr(t, tc.name, err)
			if tc.wantSent != nil {
				wantErrIs(t, tc.name, err, tc.wantSent)
			}
			if tc.needle != "" {
				wantContains(t, tc.name+" 的错误报文", errText(err), tc.needle)
			}
			// 全部校验都在发第一条 SQL 之前完成：守卫拒绝后不得有任何依赖调用。
			wantZeroOps(t, tc.name, e.ops(0))
		})
	}
}

func TestSubmitAdminTask步骤数超上限整批拒绝(t *testing.T) {
	e := newEnv(t)
	steps := make([]*rpc.TaskStepSpec, 0, 1001)
	for i := range 1001 {
		steps = append(steps, spec("submission", itoa(int64(i+1))))
	}
	_, err := submitTaskCall(t, e, submitReq(9001, "req-big",
		model.TaskTypeBatchOfflineSubmission, "{}", steps...))
	wantErrIs(t, "步骤数超上限", err, ErrTaskTooManySteps)
	wantContains(t, "步骤数上限报文", errText(err), "1001 > 1000")
	wantZeroOps(t, "超上限拒绝后不得触达依赖", e.ops(0))
}

func TestSubmitAdminTask同目标去重且步号连续(t *testing.T) {
	e := newEnv(t)
	// 三条声明里 101 重复两次（其中一次带大小写/空白差异），落库必须只剩两条且步号 1、2 连续；
	// 步号若有空洞，推进时 ListExecutable/CountByTask 的计数就会和 total 对不上。
	reply, err := submitTaskCall(t, e, submitReq(9001, "req-dedup",
		model.TaskTypeBatchOfflineSubmission, "{}",
		spec("submission", "101"), spec("SUBMISSION", " 101 "), spec("submission", "101"), spec("submission", "102")))
	wantOK(t, reply, err, "去重提交")
	wantOps(t, "去重提交轨迹", e.ops(0), []string{
		"admin_task.Insert:req-dedup",
		"admin_task_step.InsertBatch:2",
		"audit_index.Insert:admin_task.submit/ok",
	})
	wantEQ(t, "去重提交", "total（按去重后计数）", reply.Task.Total, int32(2))
	wantOps(t, "落库步骤", stepFields(e.st.step.stepsOf(reply.Task.TaskId)), []string{
		"1/submission/101/pending",
		"2/submission/102/pending",
	})
}

func TestSubmitAdminTask目标类型可省略并小写归一(t *testing.T) {
	e := newEnv(t)
	reply, err := submitTaskCall(t, e, submitReq(9001, "req-implicit",
		model.TaskTypeBatchExpireWindow, "{}", spec("", "301"), spec("  RIGHTS_WINDOW ", "302")))
	wantOK(t, reply, err, "省略 target_type 的提交")
	wantEQ(t, "省略 target_type", "total", reply.Task.Total, int32(2))
	wantOps(t, "落库步骤", stepFields(e.st.step.stepsOf(reply.Task.TaskId)), []string{
		"1/rights_window/301/pending",
		"2/rights_window/302/pending",
	})
}

func TestSubmitAdminTask忽略应答里的空步骤指针(t *testing.T) {
	e := newEnv(t)
	// logic 层丢掉 nil 指针（submitadmintasklogic.go:35-37），repository 只看到 1 条；
	// 若这层过滤漏掉，nil 解引用会直接 panic 掉整个 RPC。
	reply, err := submitTaskCall(t, e, submitReq(9001, "req-nil",
		model.TaskTypeBatchOfflineEpisode, "{}", nil, spec("episode", "201"), nil))
	wantOK(t, reply, err, "带空指针的提交")
	wantOps(t, "空指针过滤后的轨迹", e.ops(0), []string{
		"admin_task.Insert:req-nil",
		"admin_task_step.InsertBatch:1",
		"audit_index.Insert:admin_task.submit/ok",
	})
	wantEQ(t, "空指针过滤", "total", reply.Task.Total, int32(1))
}

func TestSubmitAdminTask全为空步骤指针按空步骤拒绝(t *testing.T) {
	e := newEnv(t)
	_, err := submitTaskCall(t, e, submitReq(9001, "req-allnil",
		model.TaskTypeBatchOfflineSubmission, "{}", nil, nil))
	wantErrIs(t, "全空指针步骤", err, ErrTaskEmptySteps)
	wantZeroOps(t, "全空指针步骤", e.ops(0))
}

func TestSubmitAdminTask空params归一化为空对象(t *testing.T) {
	e := newEnv(t)
	reply, err := submitTaskCall(t, e, submitReq(9001, "req-params",
		model.TaskTypeBatchRejectAppeal, "   ", spec("moderation_appeal", "401")))
	wantOK(t, reply, err, "空 params 提交")
	wantEQ(t, "空 params 归一化", "params", reply.Task.Params, "{}")
	wantEQ(t, "落库 params", "params（空串归一化为 {}）", e.st.task.get(reply.Task.TaskId).Params, "{}")
}

func TestSubmitAdminTask幂等重放返回既有任务且不重复建步骤(t *testing.T) {
	e := newEnv(t)
	existing := seedTask(t, e.st, model.TaskTypeBatchOfflineSubmission, "req-replay", func(row *model.AdminTask) {
		row.Total = 3
		row.Params = `{"reason":"原批"}` // 故意留一条与重放入参不同的 params，用于证明回传的是库存行
		row.Operator = 7788
	})
	for i := range 3 {
		e.st.step.put(&model.AdminTaskStep{
			TaskID: existing, StepNo: int32(i + 1), TargetType: "submission",
			TargetID: itoa(int64(500 + i)), State: model.StepStatePending, Ctime: nowUnix(),
		})
	}

	reply, err := submitTaskCall(t, e, submitReq(9001, "req-replay",
		model.TaskTypeBatchOfflineSubmission, `{"reason":"重试"}`, spec("submission", "900")))
	wantOK(t, reply, err, "幂等重放")
	// 只有两条：唯一键冲突 + 回读既有行。**没有** InsertBatch，也**没有**第二条审计。
	wantOps(t, "幂等重放轨迹", e.ops(0), []string{
		"admin_task.Insert:req-replay",
		"admin_task.FindByRequestID:req-replay",
	})
	wantEQ(t, "幂等重放", "reused", reply.Reused, true)
	wantEQ(t, "幂等重放", "task_id", reply.Task.TaskId, existing)
	wantEQ(t, "幂等重放", "total（回传库存行而非重放入参）", reply.Task.Total, int32(3))
	wantEQ(t, "幂等重放", "operator（回传提交人而非重试人）", reply.Task.OperatorId, int64(7788))
	wantEQ(t, "幂等重放", "params（回传库存行）", reply.Task.Params, `{"reason":"原批"}`)
	wantCount(t, "幂等重放", e.ops(0), "admin_task_step.InsertBatch", 0)
	wantCount(t, "幂等重放", e.ops(0), "audit_index.Insert", 0)
	wantEQ(t, "幂等重放后", "任务行数", int64(len(e.st.task.order)), int64(1))
}

func TestSubmitAdminTask幂等键按大小写不敏感命中(t *testing.T) {
	e := newEnv(t)
	existing := seedTask(t, e.st, model.TaskTypeBatchOfflineSubmission, "Req-Mixed", nil)
	// uniq_request_id 是 utf8mb4_unicode_ci：换大小写的重试不得变成第二个任务。
	reply, err := submitTaskCall(t, e, submitReq(9001, "req-mixed",
		model.TaskTypeBatchOfflineSubmission, "{}", spec("submission", "101")))
	wantOK(t, reply, err, "大小写变体重放")
	wantOps(t, "大小写变体重放轨迹", e.ops(0), []string{
		"admin_task.Insert:req-mixed",
		"admin_task.FindByRequestID:req-mixed",
	})
	wantEQ(t, "大小写变体重放", "reused", reply.Reused, true)
	wantEQ(t, "大小写变体重放", "task_id", reply.Task.TaskId, existing)
	wantEQ(t, "大小写变体重放", "库存 request_id（不得被改写）", e.st.task.get(existing).RequestID, "Req-Mixed")
}

func TestSubmitAdminTask步骤写入失败任务留在pending且无步骤(t *testing.T) {
	e := newEnv(t)
	e.st.step.failWith("InsertBatch", errSubmitProbe)
	reply, err := submitTaskCall(t, e, submitReq(9001, "req-stepfail",
		model.TaskTypeBatchOfflineSubmission, "{}", spec("submission", "101")))
	if reply != nil {
		t.Errorf("应答 = %+v, want nil", reply)
	}
	wantErrIs(t, "步骤写入失败", err, errSubmitProbe)
	wantContains(t, "步骤写入失败的包装文案", errText(err), "insert task steps")
	// 已成事实的一半：任务行落了、步骤没落、审计没写。
	// 这正是 repository/task.go:164-166 注释描述的状态（推进时会因无步骤直接收敛），
	// 用例把它钉住，避免有人改成「补偿删除任务行」却不同时补审计。
	wantOps(t, "步骤失败轨迹", e.ops(0), []string{
		"admin_task.Insert:req-stepfail",
		"admin_task_step.InsertBatch:1",
	})
	tasks := e.st.task.order
	wantEQ(t, "步骤失败后", "任务行数", int64(len(tasks)), int64(1))
	row := e.st.task.get(tasks[0])
	wantEQ(t, "步骤失败后", "任务状态", row.State, model.TaskStatePending)
	wantCount(t, "步骤失败后不得留审计", e.ops(0), "audit_index.Insert", 0)
	wantEQ(t, "步骤失败后", "步骤行数", int64(len(e.st.step.order)), int64(0))
}

func TestSubmitAdminTask回读既有任务失败时外传(t *testing.T) {
	e := newEnv(t)
	seedTask(t, e.st, model.TaskTypeBatchOfflineSubmission, "req-findfail", nil)
	e.st.task.failWith("FindByRequestID", errSubmitProbe)
	_, err := submitTaskCall(t, e, submitReq(9001, "req-findfail",
		model.TaskTypeBatchOfflineSubmission, "{}", spec("submission", "101")))
	wantErrIs(t, "幂等回读失败", err, errSubmitProbe)
	wantOps(t, "回读失败轨迹", e.ops(0), []string{
		"admin_task.Insert:req-findfail",
		"admin_task.FindByRequestID:req-findfail",
	})
}

func TestSubmitAdminTask任务写入失败不留步骤与审计(t *testing.T) {
	e := newEnv(t)
	e.st.task.failWith("Insert", errSubmitProbe)
	_, err := submitTaskCall(t, e, submitReq(9001, "req-insertfail",
		model.TaskTypeBatchOfflineSubmission, "{}", spec("submission", "101")))
	wantErrIs(t, "任务写入失败", err, errSubmitProbe)
	wantOps(t, "任务写入失败轨迹", e.ops(0), []string{"admin_task.Insert:req-insertfail"})
	wantEQ(t, "任务写入失败后", "任务行数", int64(len(e.st.task.order)), int64(0))
}

func TestSubmitAdminTask审计写失败任务与步骤已成立(t *testing.T) {
	e := newEnv(t)
	e.st.audit.failWith("Insert", errSubmitProbe)
	reply, err := submitTaskCall(t, e, submitReq(9001, "req-auditfail",
		model.TaskTypeBatchOfflineSubmission, "{}", spec("submission", "101")))
	if reply != nil {
		t.Errorf("应答 = %+v, want nil（审计写失败必须外传错误）", reply)
	}
	wantErrIs(t, "审计写失败", err, errSubmitProbe)
	wantContains(t, "审计写失败的包装文案", errText(err), "write audit index failed")
	wantOps(t, "审计失败轨迹", e.ops(0), []string{
		"admin_task.Insert:req-auditfail",
		"admin_task_step.InsertBatch:1",
		"audit_index.Insert:admin_task.submit/ok",
	})
	// 已成事实：任务与步骤都在库里，但 op_audit_index 一行都没有 ——
	// 运营看到「提交失败」再提交一次时，幂等键会让它拿到同一个任务，
	// 这条链路的可解释性完全依赖 request_id，不依赖审计。
	wantEQ(t, "审计失败后", "任务仍已落库", int64(len(e.st.task.order)), int64(1))
	wantEQ(t, "审计失败后", "步骤仍已落库", int64(len(e.st.step.order)), int64(1))
	wantEQ(t, "审计失败后", "审计行数", int64(len(e.st.audit.all())), int64(0))
}

func TestGetAdminTask按任务ID读明细(t *testing.T) {
	e := newEnv(t)
	id := seedTaskAndSteps(t, e.st, model.TaskTypeBatchOfflineSubmission, "req-get-1", "101", "102")

	reply, err := getTaskCall(t, e, &rpc.GetAdminTaskReq{Ctx: opCtx(9001), TaskId: id})
	wantOK(t, reply, err, "按 task_id 查询任务")
	wantOps(t, "按 task_id 查询轨迹", e.ops(0), []string{
		"admin_task.FindOne:" + itoa(id),
		"admin_task_step.ListByTask:" + itoa(id),
	})
	wantEQ(t, "查询应答", "task_id", reply.Task.TaskId, id)
	wantEQ(t, "查询应答", "request_id", reply.Task.RequestId, "req-get-1")
	wantOps(t, "查询应答步骤明细", stepFields(toModelSteps(reply.Steps)), []string{
		"1/submission/101/pending",
		"2/submission/102/pending",
	})
}

func TestGetAdminTask按request_id读明细(t *testing.T) {
	e := newEnv(t)
	id := seedTaskAndSteps(t, e.st, model.TaskTypeBatchExpireWindow, "req-get-rid", "301")
	reply, err := getTaskCall(t, e, &rpc.GetAdminTaskReq{Ctx: opCtx(9001), RequestId: "req-get-rid"})
	wantOK(t, reply, err, "按 request_id 查询任务")
	wantOps(t, "按 request_id 查询轨迹", e.ops(0), []string{
		"admin_task.FindByRequestID:req-get-rid",
		"admin_task_step.ListByTask:" + itoa(id),
	})
	wantEQ(t, "查询应答", "task_id", reply.Task.TaskId, id)
	wantEQ(t, "查询应答", "task_type", reply.Task.TaskType, model.TaskTypeBatchExpireWindow)
}

func TestGetAdminTask两个定位参数同时给时以task_id为准(t *testing.T) {
	e := newEnv(t)
	byID := seedTask(t, e.st, model.TaskTypeBatchOfflineSubmission, "req-a", nil)
	byRID := seedTask(t, e.st, model.TaskTypeBatchOfflineEpisode, "req-b", nil)
	reply, err := getTaskCall(t, e, &rpc.GetAdminTaskReq{Ctx: opCtx(9001), TaskId: byID, RequestId: "req-b"})
	wantOK(t, reply, err, "同时给两个定位参数")
	// 走的是 switch 的第一条 case（repository/task.go:182-185）：不得出现 FindByRequestID。
	wantOps(t, "定位优先级轨迹", e.ops(0), []string{
		"admin_task.FindOne:" + itoa(byID),
		"admin_task_step.ListByTask:" + itoa(byID),
	})
	wantEQ(t, "定位结果", "task_id", reply.Task.TaskId, byID)
	if reply.Task.TaskId == byRID {
		t.Errorf("定位到了错误的任务 %+v", reply.Task)
	}
}

func TestGetAdminTask守卫与不存在(t *testing.T) {
	t.Run("缺操作者上下文", func(t *testing.T) {
		e := newEnv(t)
		_, err := getTaskCall(t, e, &rpc.GetAdminTaskReq{TaskId: 1})
		wantErrIs(t, "缺操作者上下文", err, ErrInvalidOperator)
		wantZeroOps(t, "缺操作者上下文", e.ops(0))
	})
	t.Run("operator_id 非法", func(t *testing.T) {
		e := newEnv(t)
		_, err := getTaskCall(t, e, &rpc.GetAdminTaskReq{Ctx: opCtx(0), TaskId: 1})
		wantErrIs(t, "operator_id 非法", err, ErrInvalidOperator)
		wantZeroOps(t, "operator_id 非法", e.ops(0))
	})
	t.Run("两个定位参数都没给", func(t *testing.T) {
		e := newEnv(t)
		_, err := getTaskCall(t, e, &rpc.GetAdminTaskReq{Ctx: opCtx(9001)})
		wantErr(t, "两个定位参数都没给", err)
		wantContains(t, "定位参数报错", errText(err), "task_id or request_id required")
		wantZeroOps(t, "两个定位参数都没给", e.ops(0))
	})
	t.Run("task_id 为负且 request_id 全空白", func(t *testing.T) {
		e := newEnv(t)
		_, err := getTaskCall(t, e, &rpc.GetAdminTaskReq{Ctx: opCtx(9001), TaskId: -5, RequestId: "  "})
		wantErr(t, "task_id 为负且 request_id 全空白", err)
		wantContains(t, "定位参数报错", errText(err), "task_id or request_id required")
		wantZeroOps(t, "task_id 为负且 request_id 全空白", e.ops(0))
	})
	t.Run("任务不存在", func(t *testing.T) {
		e := newEnv(t)
		_, err := getTaskCall(t, e, &rpc.GetAdminTaskReq{Ctx: opCtx(9001), TaskId: 999})
		wantErrIs(t, "任务不存在", err, ErrTaskNotFound)
		// 读不到任务就不得再去读步骤（否则等于给不存在的服务暴露一次查询）。
		wantOps(t, "任务不存在轨迹", e.ops(0), []string{"admin_task.FindOne:999"})
	})
	t.Run("request_id 不存在", func(t *testing.T) {
		e := newEnv(t)
		_, err := getTaskCall(t, e, &rpc.GetAdminTaskReq{Ctx: opCtx(9001), RequestId: "req-none"})
		wantErrIs(t, "request_id 不存在", err, ErrTaskNotFound)
		wantOps(t, "request_id 不存在轨迹", e.ops(0), []string{"admin_task.FindByRequestID:req-none"})
	})
	t.Run("步骤读取失败外传", func(t *testing.T) {
		e := newEnv(t)
		id := seedTaskAndSteps(t, e.st, model.TaskTypeBatchOfflineSubmission, "req-steperr", "101")
		e.st.step.failWith("ListByTask", errSubmitProbe)
		_, err := getTaskCall(t, e, &rpc.GetAdminTaskReq{Ctx: opCtx(9001), TaskId: id})
		wantErrIs(t, "步骤读取失败", err, errSubmitProbe)
		wantOps(t, "步骤读取失败轨迹", e.ops(0), []string{
			"admin_task.FindOne:" + itoa(id),
			"admin_task_step.ListByTask:" + itoa(id),
		})
	})
	t.Run("任务无步骤时明细为空", func(t *testing.T) {
		e := newEnv(t)
		id := seedTask(t, e.st, model.TaskTypeBatchOfflineSubmission, "req-nosteps", nil)
		reply, err := getTaskCall(t, e, &rpc.GetAdminTaskReq{Ctx: opCtx(9001), TaskId: id})
		wantOK(t, reply, err, "无步骤任务查询")
		wantEQ(t, "无步骤任务", "steps 条数", int64(len(reply.Steps)), int64(0))
	})
}

func TestListAdminTasks按条件分页查询(t *testing.T) {
	e := newEnv(t)
	// 三条任务：两种状态、两种类型、两个提交人，覆盖 state/task_type/operator 三个筛选项。
	p1 := seedTask(t, e.st, model.TaskTypeBatchOfflineSubmission, "req-l-1", func(r *model.AdminTask) {
		r.State = model.TaskStatePending
		r.Operator = 9001
		r.Total = 1
	})
	p2 := seedTask(t, e.st, model.TaskTypeBatchOfflineSubmission, "req-l-2", func(r *model.AdminTask) {
		r.State = model.TaskStatePending
		r.Operator = 9002
		r.Total = 2
	})
	s1 := seedTask(t, e.st, model.TaskTypeBatchExpireWindow, "req-l-3", func(r *model.AdminTask) {
		r.State = model.TaskStateSucceeded
		r.Operator = 9001
		r.Total = 5
	})
	if p1 >= p2 || p2 >= s1 {
		t.Fatalf("布景 task_id 不是递增的：%d %d %d（列表倒序断言依赖这一点）", p1, p2, s1)
	}

	cases := []struct {
		name      string
		in        *rpc.ListAdminTasksReq
		wantOp    string
		wantIDs   []string
		wantTotal int64
	}{
		{
			name:      "全部",
			in:        &rpc.ListAdminTasksReq{Ctx: opCtx(9001), Pn: 1, Ps: 20},
			wantOp:    "admin_task.List://0/1/20",
			wantIDs:   []string{itoa(s1), itoa(p2), itoa(p1)}, // ORDER BY task_id DESC
			wantTotal: 3,
		},
		{
			name:      "按状态筛",
			in:        &rpc.ListAdminTasksReq{Ctx: opCtx(9001), State: model.TaskStatePending, Pn: 1, Ps: 20},
			wantOp:    "admin_task.List:pending//0/1/20",
			wantIDs:   []string{itoa(p2), itoa(p1)},
			wantTotal: 2,
		},
		{
			name:      "按类型筛",
			in:        &rpc.ListAdminTasksReq{Ctx: opCtx(9001), TaskType: model.TaskTypeBatchExpireWindow, Pn: 1, Ps: 20},
			wantOp:    "admin_task.List:/batch_expire_window/0/1/20",
			wantIDs:   []string{itoa(s1)},
			wantTotal: 1,
		},
		{
			name:      "按提交人筛",
			in:        &rpc.ListAdminTasksReq{Ctx: opCtx(9001), OperatorId: 9001, Pn: 1, Ps: 20},
			wantOp:    "admin_task.List://9001/1/20",
			wantIDs:   []string{itoa(s1), itoa(p1)},
			wantTotal: 2,
		},
		{
			name:      "状态与类型与提交人三合取",
			in:        &rpc.ListAdminTasksReq{Ctx: opCtx(9001), State: model.TaskStatePending, TaskType: model.TaskTypeBatchOfflineSubmission, OperatorId: 9002, Pn: 1, Ps: 20},
			wantOp:    "admin_task.List:pending/batch_offline_submission/9002/1/20",
			wantIDs:   []string{itoa(p2)},
			wantTotal: 1,
		},
		{
			name:      "第二页只剩一条",
			in:        &rpc.ListAdminTasksReq{Ctx: opCtx(9001), State: model.TaskStatePending, Pn: 2, Ps: 1},
			wantOp:    "admin_task.List:pending//0/2/1",
			wantIDs:   []string{itoa(p1)},
			wantTotal: 2,
		},
		{
			name:      "越界页返回空集但 total 不变",
			in:        &rpc.ListAdminTasksReq{Ctx: opCtx(9001), State: model.TaskStatePending, Pn: 9, Ps: 1},
			wantOp:    "admin_task.List:pending//0/9/1",
			wantIDs:   nil,
			wantTotal: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 布景只做一次（seed 是静默写入、不进轨迹），但轨迹必须逐用例清零：
			// 每个用例断言的都是「恰好一次 admin_task.List、零其它调用」的完整序列，
			// 沿用同一个 env 的累加轨迹会让期望值变成前缀，等于什么都没断。
			e.st.log.reset()
			reply, err := listTasksCall(t, e, tc.in)
			wantOK(t, reply, err, tc.name)
			wantOps(t, tc.name+" 轨迹", e.ops(0), []string{tc.wantOp})
			wantEQ(t, tc.name, "total", reply.Total, tc.wantTotal)
			got := make([]string, 0, len(reply.Items))
			for _, it := range reply.Items {
				got = append(got, itoa(it.TaskId))
			}
			wantOps(t, tc.name+" 的 task_id 顺序", got, tc.wantIDs)
		})
	}
}

func TestListAdminTasks不返回步骤明细(t *testing.T) {
	e := newEnv(t)
	seedTaskAndSteps(t, e.st, model.TaskTypeBatchOfflineSubmission, "req-list-nosteps", "101", "102")
	reply, err := listTasksCall(t, e, &rpc.ListAdminTasksReq{Ctx: opCtx(9001), Pn: 1, Ps: 20})
	wantOK(t, reply, err, "列表查询")
	// 列表只查任务表：一次 List、零次步骤查询（避免批量任务把 N 万行步骤拉回网关）。
	wantOps(t, "列表轨迹", e.ops(0), []string{"admin_task.List://0/1/20"})
	wantEQ(t, "列表", "条目数", int64(len(reply.Items)), int64(1))
}

func TestListAdminTasks分页参数钳制(t *testing.T) {
	e := newEnv(t)
	seedTask(t, e.st, model.TaskTypeBatchOfflineSubmission, "req-page", nil)
	cases := []struct {
		name   string
		pn, ps int32
		wantPn int32
		wantPs int32
	}{
		{"pn 为 0", 0, 20, 1, 20},
		{"pn 为负", -3, 20, 1, 20},
		{"ps 为 0", 1, 0, 1, 20},
		{"ps 为负", 1, -10, 1, 20},
		{"ps 超上限", 2, 500, 2, 100},
		{"ps 恰为上限", 1, 100, 1, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 同上：布景共享，轨迹逐用例清零，否则「恰好一次 List」会被前序用例的调用污染。
			e.st.log.reset()
			_, err := listTasksCall(t, e, &rpc.ListAdminTasksReq{Ctx: opCtx(9001), Pn: tc.pn, Ps: tc.ps})
			wantNoErr(t, tc.name, err)
			wantOps(t, tc.name, e.ops(0), []string{
				fmt.Sprintf("admin_task.List://0/%d/%d", tc.wantPn, tc.wantPs),
			})
		})
	}
}

func TestListAdminTasks未知筛选值在查询前拒绝(t *testing.T) {
	cases := []struct {
		name     string
		in       *rpc.ListAdminTasksReq
		wantSent error
		needle   string
	}{
		{
			name:   "未知状态",
			in:     &rpc.ListAdminTasksReq{Ctx: opCtx(9001), State: "cancelled", Pn: 1, Ps: 20},
			needle: `unknown task state filter "cancelled"`,
		},
		{
			name:     "未知任务类型",
			in:       &rpc.ListAdminTasksReq{Ctx: opCtx(9001), TaskType: "batch_nonsense", Pn: 1, Ps: 20},
			wantSent: ErrTaskTypeUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seedTask(t, e.st, model.TaskTypeBatchOfflineSubmission, "req-x", nil)
			_, err := listTasksCall(t, e, tc.in)
			wantErr(t, tc.name, err)
			if tc.wantSent != nil {
				wantErrIs(t, tc.name, err, tc.wantSent)
			}
			if tc.needle != "" {
				wantContains(t, tc.name, errText(err), tc.needle)
			}
			wantZeroOps(t, tc.name+"（不得把非法筛选值发给 DB）", e.ops(0))
		})
	}
}

func TestListAdminTasks六个合法状态都可筛(t *testing.T) {
	e := newEnv(t)
	// 六个状态各布一行：既验证 isKnownTaskState 的取值域与 model 常量一致，
	// 也保证筛选真的按 state 列生效（而不是恒返回全集）。
	states := []string{
		model.TaskStatePending, model.TaskStateRunning, model.TaskStateSucceeded,
		model.TaskStatePartial, model.TaskStateFailed, model.TaskStateCanceled,
	}
	for i, s := range states {
		rid := fmt.Sprintf("req-state-%d", i)
		seedTask(t, e.st, model.TaskTypeBatchOfflineSubmission, rid, func(r *model.AdminTask) { r.State = s })
	}
	for i, s := range states {
		reply, err := listTasksCall(t, e, &rpc.ListAdminTasksReq{Ctx: opCtx(9001), State: s, Pn: 1, Ps: 20})
		wantOK(t, reply, err, "按状态 "+s+" 筛")
		wantEQ(t, "状态筛选 "+s, "命中数", reply.Total, int64(1))
		if len(reply.Items) != 1 {
			t.Fatalf("状态 %s 的条目数 = %d, want 1（%v）", s, len(reply.Items), reply.Items)
		}
		wantEQ(t, "状态筛选 "+s, "条目状态", reply.Items[0].State, s)
		// 命中的必须正是那一行，而不是「任何一行」：request_id 是每行唯一的锚点。
		wantEQ(t, "状态筛选 "+s, "request_id（命中的必须是那一行）", reply.Items[0].RequestId, fmt.Sprintf("req-state-%d", i))
	}
	wantCount(t, "状态筛选", e.ops(0), "admin_task.List:", 6)
}

func TestListAdminTasks守卫与查询失败(t *testing.T) {
	t.Run("缺操作者上下文", func(t *testing.T) {
		e := newEnv(t)
		_, err := listTasksCall(t, e, &rpc.ListAdminTasksReq{Pn: 1, Ps: 20})
		wantErrIs(t, "缺操作者上下文", err, ErrInvalidOperator)
		wantZeroOps(t, "缺操作者上下文", e.ops(0))
	})
	t.Run("operator_id 非法", func(t *testing.T) {
		e := newEnv(t)
		_, err := listTasksCall(t, e, &rpc.ListAdminTasksReq{Ctx: opCtx(-1), Pn: 1, Ps: 20})
		wantErrIs(t, "operator_id 非法", err, ErrInvalidOperator)
		wantZeroOps(t, "operator_id 非法", e.ops(0))
	})
	t.Run("列表查询失败外传", func(t *testing.T) {
		e := newEnv(t)
		seedTask(t, e.st, model.TaskTypeBatchOfflineSubmission, "req-fail", nil)
		e.st.task.failWith("List", errSubmitProbe)
		reply, err := listTasksCall(t, e, &rpc.ListAdminTasksReq{Ctx: opCtx(9001), Pn: 1, Ps: 20})
		if reply != nil {
			t.Errorf("应答 = %+v, want nil", reply)
		}
		wantErrIs(t, "列表查询失败", err, errSubmitProbe)
		wantOps(t, "列表查询失败轨迹", e.ops(0), []string{"admin_task.List://0/1/20"})
	})
}

// toModelSteps 把应答里的步骤投影还原成 model 形态，复用同一套摘要断言口径。
func toModelSteps(items []*rpc.TaskStepInfo) []*model.AdminTaskStep {
	out := make([]*model.AdminTaskStep, 0, len(items))
	for _, it := range items {
		out = append(out, &model.AdminTaskStep{
			ID: it.Id, TaskID: it.TaskId, StepNo: it.StepNo, TargetType: it.TargetType,
			TargetID: it.TargetId, State: it.State, Result: it.Result, ErrMsg: it.ErrMsg,
			Ctime: it.Ctime, Mtime: it.Mtime,
		})
	}
	return out
}
