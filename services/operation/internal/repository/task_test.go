package repository

// 管理任务状态机与幂等提交用例（内存 model，不连数据库、不调下游 RPC）。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/operation/model"
)

func newTaskRepository(task *model.AdminTask, steps []*model.AdminTaskStep) (*Repository, *fakeTaskModel, *fakeStepModel, *fakeAuditModel) {
	r := newTestRepository()
	taskMd := &fakeTaskModel{task: task}
	stepMd := &fakeStepModel{steps: steps}
	auditMd := &fakeAuditModel{}
	r.taskMd = taskMd
	r.stepMd = stepMd
	r.auditMd = auditMd
	return r, taskMd, stepMd, auditMd
}

func TestNormalizeTaskParams(t *testing.T) {
	if got, err := normalizeTaskParams("  "); err != nil || got != "{}" {
		t.Fatalf("empty params = %q/%v, want {}", got, err)
	}
	if got, err := normalizeTaskParams(`{"reason":"版权到期"}`); err != nil || !strings.Contains(got, "reason") {
		t.Fatalf("params = %q/%v", got, err)
	}
	if _, err := normalizeTaskParams("not-json"); !errors.Is(err, model.ErrTaskParamsNotJSON) {
		t.Fatalf("err = %v, want ErrTaskParamsNotJSON", err)
	}
	// JSON 数组不是对象：params 必须是键值对象，下游按字段取值。
	if _, err := normalizeTaskParams("[1,2]"); !errors.Is(err, model.ErrTaskParamsNotJSON) {
		t.Fatalf("err = %v, want ErrTaskParamsNotJSON", err)
	}
	p := parseTaskParams(`{"reason":"到期下架","unknown":1}`)
	if p.Reason != "到期下架" {
		t.Fatalf("parsed reason = %q", p.Reason)
	}
	if got := parseTaskParams("broken"); got.Reason != "" {
		t.Fatalf("broken params must degrade to empty, got %+v", got)
	}
}

func TestSubmitAdminTaskValidation(t *testing.T) {
	ctx := context.Background()
	actor := Actor{AdminID: 1, RequestID: "req-1"}

	cases := []struct {
		name string
		in   SubmitTaskInput
		want error
	}{
		{"unknown type", SubmitTaskInput{TaskType: "pay_refund"}, model.ErrTaskTypeUnknown},
		{"no steps", SubmitTaskInput{TaskType: model.TaskTypeBatchOfflineSubmission}, model.ErrTaskEmptySteps},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _, _, auditMd := newTaskRepository(nil, nil)
			_, _, err := r.SubmitAdminTask(ctx, actor, c.in)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if len(auditMd.rows) != 0 {
				t.Fatal("rejected submit must not write an ok audit row")
			}
		})
	}

	// 缺 request_id / 缺操作者：幂等与审计主体都不能少。
	r, _, _, _ := newTaskRepository(nil, nil)
	if _, _, err := r.SubmitAdminTask(ctx, Actor{AdminID: 1}, SubmitTaskInput{
		TaskType: model.TaskTypeBatchOfflineSubmission,
		Steps:    []TaskStepInput{{TargetID: "10"}},
	}); err == nil || !strings.Contains(err.Error(), "request_id") {
		t.Fatalf("err = %v, want request_id required", err)
	}
	if _, _, err := r.SubmitAdminTask(ctx, Actor{RequestID: "req"}, SubmitTaskInput{
		TaskType: model.TaskTypeBatchOfflineSubmission,
		Steps:    []TaskStepInput{{TargetID: "10"}},
	}); !errors.Is(err, model.ErrInvalidOperator) {
		t.Fatalf("err = %v, want ErrInvalidOperator", err)
	}
}

func TestSubmitAdminTaskStepsDedupAndRenumber(t *testing.T) {
	ctx := context.Background()
	r, taskMd, stepMd, auditMd := newTaskRepository(nil, nil)

	// 步骤由调用方给出乱序与重复目标；服务端必须按 target_type#target_id 去重并连续编号，
	// 否则一次误操作会把同一稿件下架两次。
	got, reused, err := r.SubmitAdminTask(ctx, Actor{AdminID: 1, RequestID: "req-9"}, SubmitTaskInput{
		TaskType: model.TaskTypeBatchOfflineSubmission,
		Params:   `{"reason":"违规"}`,
		Steps: []TaskStepInput{
			{TargetID: "20"},
			{TargetID: "10"},
			{TargetID: "10"},
			{TargetType: "submission", TargetID: " 30 "},
		},
	})
	if err != nil {
		t.Fatalf("SubmitAdminTask: %v", err)
	}
	if reused {
		t.Fatal("fresh request must not be reported as reused")
	}
	if got.Total != 3 {
		t.Fatalf("total = %d, want 3 after dedup", got.Total)
	}
	if got.State != model.TaskStatePending || got.RequestID != "req-9" {
		t.Fatalf("task = %+v", got)
	}
	if taskMd.task != nil && taskMd.task.Total != 3 {
		t.Fatalf("inserted total = %d", taskMd.task.Total)
	}
	// 落库步骤：去重后保留首次出现顺序、step_no 连续、状态 pending、已回填 task_id。
	if len(stepMd.steps) != 3 {
		t.Fatalf("inserted steps = %d, want 3", len(stepMd.steps))
	}
	wantIDs := []string{"20", "10", "30"}
	for i, s := range stepMd.steps {
		if s.StepNo != int32(i+1) || s.TargetID != wantIDs[i] {
			t.Fatalf("step %d = %d/%s, want %d/%s", i, s.StepNo, s.TargetID, i+1, wantIDs[i])
		}
		if s.State != model.StepStatePending || s.TaskID != got.TaskID {
			t.Fatalf("step %d = %+v, want pending with task_id %d", i, s, got.TaskID)
		}
	}
	if len(auditMd.rows) != 1 || auditMd.rows[0].Action != actionTaskSubmit {
		t.Fatalf("audit = %+v", auditMd.rows)
	}

	// 目标类型与任务类型不符 / 非正整数 ID：整体拒绝，不落半份任务。
	r2, taskMd2, _, _ := newTaskRepository(nil, nil)
	if _, _, err := r2.SubmitAdminTask(ctx, Actor{AdminID: 1, RequestID: "req-10"}, SubmitTaskInput{
		TaskType: model.TaskTypeBatchExpireWindow,
		Steps:    []TaskStepInput{{TargetType: "submission", TargetID: "5"}},
	}); err == nil || !strings.Contains(err.Error(), "target_type") {
		t.Fatalf("err = %v, want target_type mismatch", err)
	}
	if _, _, err := r2.SubmitAdminTask(ctx, Actor{AdminID: 1, RequestID: "req-11"}, SubmitTaskInput{
		TaskType: model.TaskTypeBatchExpireWindow,
		Steps:    []TaskStepInput{{TargetID: "abc"}},
	}); err == nil || !strings.Contains(err.Error(), "positive id") {
		t.Fatalf("err = %v, want positive id required", err)
	}
	if taskMd2.task != nil {
		t.Fatal("rejected submits must not persist a task")
	}
}

func TestSubmitAdminTaskIsIdempotentOnRequestID(t *testing.T) {
	ctx := context.Background()
	existing := &model.AdminTask{TaskID: 55, TaskType: model.TaskTypeBatchOfflineSubmission, State: model.TaskStateRunning, RequestID: "req-1", Total: 2}
	r, _, stepMd, auditMd := newTaskRepository(existing, nil)
	// 唯一键命中：model 返回 ErrTaskExists，repository 回查并复用既有任务。
	r.taskMd = &idempotentTaskModel{fakeTaskModel: &fakeTaskModel{task: existing}}

	got, reused, err := r.SubmitAdminTask(ctx, Actor{AdminID: 1, RequestID: "req-1"}, SubmitTaskInput{
		TaskType: model.TaskTypeBatchOfflineSubmission,
		Steps:    []TaskStepInput{{TargetID: "1"}, {TargetID: "2"}},
	})
	if err != nil {
		t.Fatalf("SubmitAdminTask: %v", err)
	}
	if !reused || got.TaskID != 55 {
		t.Fatalf("got %d reused=%v, want task 55 reused", got.TaskID, reused)
	}
	// 复用路径不得再写步骤，也不得再写一条 ok 审计。
	if len(stepMd.steps) != 0 {
		t.Fatalf("reused submit inserted steps: %+v", stepMd.steps)
	}
	if len(auditMd.rows) != 0 {
		t.Fatalf("reused submit wrote audit: %+v", auditMd.rows)
	}
}

// idempotentTaskModel 在 Insert 上模拟 uniq_request_id 冲突，其余沿用 fakeTaskModel。
type idempotentTaskModel struct {
	*fakeTaskModel
}

func (m *idempotentTaskModel) Insert(context.Context, *model.AdminTask) (int64, error) {
	return 0, model.ErrTaskExists
}

func TestCancelAdminTaskRejectsIllegalStates(t *testing.T) {
	ctx := context.Background()
	actor := Actor{AdminID: 1}

	for _, state := range []string{model.TaskStateSucceeded, model.TaskStateFailed, model.TaskStatePartial, model.TaskStateCanceled} {
		t.Run(state, func(t *testing.T) {
			r, taskMd, stepMd, _ := newTaskRepository(
				&model.AdminTask{TaskID: 9, State: state}, nil)
			_, err := r.CancelAdminTask(ctx, actor, 9, "误操作")
			if !errors.Is(err, model.ErrTaskBadTransition) {
				t.Fatalf("cancel from %s: err = %v, want ErrTaskBadTransition", state, err)
			}
			if taskMd.transitionCall != 0 || stepMd.cancelCalled != 0 {
				t.Fatalf("illegal cancel must not write: transitions=%d cancels=%d", taskMd.transitionCall, stepMd.cancelCalled)
			}
		})
	}

	// 未知状态（脏数据）同样拒绝，不能被当成 pending 推进。
	r, taskMd, _, _ := newTaskRepository(&model.AdminTask{TaskID: 9, State: "weird"}, nil)
	if _, err := r.CancelAdminTask(ctx, actor, 9, ""); !errors.Is(err, model.ErrTaskBadTransition) {
		t.Fatalf("err = %v, want ErrTaskBadTransition", err)
	}
	if taskMd.transitionCall != 0 {
		t.Fatal("must not transition from an unknown state")
	}

	// 任务不存在 → ErrTaskNotFound，而不是空指针。
	r2, _, _, _ := newTaskRepository(nil, nil)
	if _, err := r2.CancelAdminTask(ctx, actor, 404, ""); !errors.Is(err, model.ErrTaskNotFound) {
		t.Fatalf("err = %v, want ErrTaskNotFound", err)
	}
}

func TestCancelAdminTaskConcurrentAdvanceLoses(t *testing.T) {
	ctx := context.Background()
	// 库中已被另一个推进者改成 running：带 state 条件的 UPDATE 不命中，
	// 必须回报冲突而不是假装取消成功。
	r, taskMd, _, _ := newTaskRepository(
		&model.AdminTask{TaskID: 9, State: model.TaskStatePending}, nil)
	taskMd.stateInDB = model.TaskStateRunning

	if _, err := r.CancelAdminTask(ctx, Actor{AdminID: 1}, 9, ""); !errors.Is(err, model.ErrTaskBadTransition) {
		t.Fatalf("err = %v, want ErrTaskBadTransition", err)
	}
	if taskMd.transitionCall != 1 || taskMd.lastFrom != model.TaskStatePending {
		t.Fatalf("transition calls = %d (%s->%s), want one pending-conditioned write", taskMd.transitionCall, taskMd.lastFrom, taskMd.lastTo)
	}
}

func TestCancelAdminTaskHappyPath(t *testing.T) {
	ctx := context.Background()
	steps := []*model.AdminTaskStep{
		{ID: 1, TaskID: 9, StepNo: 1, State: model.StepStatePending},
		{ID: 2, TaskID: 9, StepNo: 2, State: model.StepStateRunning},
	}
	r, taskMd, stepMd, auditMd := newTaskRepository(
		&model.AdminTask{TaskID: 9, State: model.TaskStatePending, Total: 2}, steps)

	got, err := r.CancelAdminTask(ctx, Actor{AdminID: 1}, 9, "重复提交")
	if err != nil {
		t.Fatalf("CancelAdminTask: %v", err)
	}
	if got.State != model.TaskStateCanceled {
		t.Fatalf("state = %s, want canceled", got.State)
	}
	if stepMd.cancelCalled != 1 {
		t.Fatalf("CancelPending calls = %d, want 1", stepMd.cancelCalled)
	}
	if taskMd.lastTo != model.TaskStateCanceled {
		t.Fatalf("last transition = %s", taskMd.lastTo)
	}
	if len(auditMd.rows) != 1 || !strings.Contains(auditMd.rows[0].ResourceID, "重复提交") {
		t.Fatalf("audit must carry the reason: %+v", auditMd.rows)
	}
	// 审计里的操作原因是运营文本，必须按列宽截断且不含分隔符注入。
	if n := len([]rune(auditMd.rows[0].ResourceID)); n > 80 {
		t.Fatalf("resource_id too long: %d", n)
	}
}

func TestRunAdminTaskFinalStateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	steps := []*model.AdminTaskStep{{ID: 1, TaskID: 9, StepNo: 1, State: model.StepStateSucceeded}}
	r, taskMd, stepMd, auditMd := newTaskRepository(
		&model.AdminTask{TaskID: 9, State: model.TaskStateSucceeded, Total: 1, Succeeded: 1, Progress: 1}, steps)

	view, executed, err := r.RunAdminTask(ctx, Actor{AdminID: 1}, 9, 10)
	if err != nil {
		t.Fatalf("RunAdminTask: %v", err)
	}
	if executed != 0 || view.Task.State != model.TaskStateSucceeded {
		t.Fatalf("executed=%d state=%s, want 0/succeeded", executed, view.Task.State)
	}
	// 重复触发终态任务：零写入（无状态迁移、无步骤标记、无审计）。
	if taskMd.transitionCall != 0 || len(stepMd.marked) != 0 || len(auditMd.rows) != 0 {
		t.Fatalf("re-running a final task must be side-effect free: %d/%v/%+v",
			taskMd.transitionCall, stepMd.marked, auditMd.rows)
	}
}

func TestRunAdminTaskRejectsUnknownTask(t *testing.T) {
	ctx := context.Background()
	r, _, _, _ := newTaskRepository(nil, nil)
	if _, _, err := r.RunAdminTask(ctx, Actor{AdminID: 1}, 404, 5); !errors.Is(err, model.ErrTaskNotFound) {
		t.Fatalf("err = %v, want ErrTaskNotFound", err)
	}
	if _, _, err := r.RunAdminTask(ctx, Actor{AdminID: 1}, 0, 5); err == nil {
		t.Fatal("task_id <= 0 must be refused")
	}
	if _, _, err := r.RunAdminTask(ctx, Actor{}, 9, 5); !errors.Is(err, model.ErrInvalidOperator) {
		t.Fatalf("err = %v, want ErrInvalidOperator", err)
	}
}

func TestStepMarkRespectsStateMachine(t *testing.T) {
	// MarkStep 的并发保护语义：from 与库中状态不符时不命中，推进者必须放弃该步骤。
	steps := []*model.AdminTaskStep{{ID: 1, TaskID: 9, StepNo: 1, State: model.StepStateRunning}}
	f := &fakeStepModel{steps: steps}
	ok, err := f.MarkStep(context.Background(), 1, model.StepStatePending, model.StepStateSucceeded, "", "")
	if err != nil || ok {
		t.Fatalf("claim of an already-running step = %v/%v, want false/nil", ok, err)
	}
	if len(f.marked) != 1 {
		t.Fatalf("marked = %v", f.marked)
	}
	if steps[0].State != model.StepStateRunning {
		t.Fatalf("failed claim altered the row: %s", steps[0].State)
	}
}
