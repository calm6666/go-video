package repository

// 本文件实现管理任务编排：提交（幂等）、查询、取消与推进。
//
// 职责边界（AGENTS.md §3/§5）：
//   - 本服务只记录“要编排哪些下游动作”并逐步推进，真实写入由
//     video / catalog / rights / moderation-orchestrator 的公开 RPC 完成；
//   - 本服务不内置分发 worker。推进入口是 RunAdminTask RPC，
//     由 services/cron 或人工触发（定时策略归 cron 所有）；
//   - 步骤状态与任务状态都只能通过 model.CanTaskTransition/CanStepTransition
//     允许的迁移推进，且写回带 state 条件（乐观并发），因此多个推进者同时
//     调用 RunAdminTask 也不会重复执行同一步骤。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/operation/model"
)

// taskTargetType 任务类型 → 步骤目标类型（一步一目标）。
var taskTargetType = map[string]string{
	model.TaskTypeBatchOfflineSubmission: "submission",
	model.TaskTypeBatchOfflineEpisode:    "episode",
	model.TaskTypeBatchExpireWindow:      "rights_window",
	model.TaskTypeBatchRejectAppeal:      "moderation_appeal",
}

// TaskStepInput 提交任务的步骤声明。
type TaskStepInput struct {
	TargetType string
	TargetID   string
}

// SubmitTaskInput 提交任务入参（RequestID 来自 OpContext，用作幂等键）。
type SubmitTaskInput struct {
	TaskType string
	Params   string
	Steps    []TaskStepInput
}

// TaskView 任务及其步骤的读取结果。
type TaskView struct {
	Task  *model.AdminTask
	Steps []*model.AdminTaskStep
}

// taskParams 是 op_admin_task.params 的已知字段（其余键透传给下游，不解析）。
type taskParams struct {
	// Reason 运营填写的操作原因，进入下游服务的状态变更理由。
	Reason string `json:"reason"`
}

// normalizeTaskParams 校验 params 是 JSON 对象文本；空串归一化为 "{}"。
func normalizeTaskParams(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "{}", nil
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "", fmt.Errorf("%w: %v", model.ErrTaskParamsNotJSON, err)
	}
	return raw, nil
}

// parseTaskParams 解析已知字段；解析失败按空参数处理（不阻断推进，理由留空）。
func parseTaskParams(raw string) taskParams {
	var p taskParams
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return taskParams{}
	}
	return p
}

// SubmitAdminTask 提交批量运营任务。
// 幂等：request_id 命中已有任务时返回该任务并置 reused=true，不重复创建步骤。
func (r *Repository) SubmitAdminTask(ctx context.Context, actor Actor, in SubmitTaskInput) (*model.AdminTask, bool, error) {
	if err := r.requireOperator(actor); err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(actor.RequestID) == "" {
		return nil, false, fmt.Errorf("operation: request_id is required for idempotent task submit")
	}
	if !model.ValidTaskType(in.TaskType) {
		return nil, false, model.ErrTaskTypeUnknown
	}
	params, err := normalizeTaskParams(in.Params)
	if err != nil {
		return nil, false, err
	}
	if len(in.Steps) == 0 {
		return nil, false, model.ErrTaskEmptySteps
	}
	if len(in.Steps) > maxTaskSteps {
		return nil, false, fmt.Errorf("%w: %d > %d", model.ErrTaskTooManySteps, len(in.Steps), maxTaskSteps)
	}
	targetType := taskTargetType[in.TaskType]
	steps := make([]*model.AdminTaskStep, 0, len(in.Steps))
	seen := make(map[string]struct{}, len(in.Steps))
	for i, s := range in.Steps {
		st := strings.ToLower(strings.TrimSpace(s.TargetType))
		if st == "" {
			st = targetType
		}
		if st != targetType {
			return nil, false, fmt.Errorf("operation: task_type %s only accepts target_type %q, got %q",
				in.TaskType, targetType, st)
		}
		idText := strings.TrimSpace(s.TargetID)
		id, err := strconv.ParseInt(idText, 10, 64)
		if err != nil || id <= 0 {
			return nil, false, fmt.Errorf("operation: step %d target_id %q is not a positive id", i+1, idText)
		}
		dedup := st + "#" + idText
		if _, ok := seen[dedup]; ok {
			continue // 同一目标去重，避免重复下架/重复过期
		}
		seen[dedup] = struct{}{}
		steps = append(steps, &model.AdminTaskStep{
			StepNo:     int32(len(steps) + 1),
			TargetType: st,
			TargetID:   idText,
			State:      model.StepStatePending,
		})
	}
	if len(steps) == 0 {
		return nil, false, model.ErrTaskEmptySteps
	}

	task := &model.AdminTask{
		TaskType:  in.TaskType,
		Params:    params,
		State:     model.TaskStatePending,
		RequestID: strings.TrimSpace(actor.RequestID),
		Total:     int32(len(steps)),
		Operator:  actor.AdminID,
		TraceID:   actor.TraceID,
	}
	taskID, err := r.taskMd.Insert(ctx, task)
	if err != nil {
		if errors.Is(err, model.ErrTaskExists) {
			existing, findErr := r.taskMd.FindByRequestID(ctx, task.RequestID)
			if findErr != nil {
				return nil, false, findErr
			}
			if existing == nil {
				return nil, false, err
			}
			return existing, true, nil
		}
		return nil, false, err
	}
	for _, s := range steps {
		s.TaskID = taskID
	}
	if err := r.stepMd.InsertBatch(ctx, steps); err != nil {
		// 步骤写入失败：任务留在 pending 且无步骤，推进时会直接收敛为 failed，
		// 同时把错误返回给调用方重试（request_id 幂等保证不会重复建任务）。
		return nil, false, fmt.Errorf("operation: insert task steps: %w", err)
	}
	if err := r.writeAudit(ctx, actor, actionTaskSubmit, "admin_task", strconv.FormatInt(taskID, 10), model.AuditResultOK); err != nil {
		return nil, false, err
	}
	task.TaskID = taskID
	return task, false, nil
}

// GetAdminTask 按 task_id 或 request_id 读取任务与步骤明细。
func (r *Repository) GetAdminTask(ctx context.Context, taskID int64, requestID string) (*TaskView, error) {
	var (
		task *model.AdminTask
		err  error
	)
	switch {
	case taskID > 0:
		task, err = r.taskMd.FindOne(ctx, taskID)
	case strings.TrimSpace(requestID) != "":
		task, err = r.taskMd.FindByRequestID(ctx, strings.TrimSpace(requestID))
	default:
		return nil, fmt.Errorf("operation: task_id or request_id required")
	}
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, model.ErrTaskNotFound
	}
	steps, err := r.stepMd.ListByTask(ctx, task.TaskID)
	if err != nil {
		return nil, err
	}
	return &TaskView{Task: task, Steps: steps}, nil
}

// ListAdminTasks 分页查询任务（不返回步骤明细，避免大结果集）。
func (r *Repository) ListAdminTasks(ctx context.Context, state, taskType string, operator int64, pn, ps int32) ([]*model.AdminTask, int64, error) {
	pn, ps = pagePair(pn, ps)
	if state != "" && !isKnownTaskState(state) {
		return nil, 0, fmt.Errorf("operation: unknown task state filter %q", state)
	}
	if taskType != "" && !model.ValidTaskType(taskType) {
		return nil, 0, model.ErrTaskTypeUnknown
	}
	return r.taskMd.List(ctx, state, taskType, operator, pn, ps)
}

// isKnownTaskState 判断状态字符串是否在状态机取值域内。
func isKnownTaskState(s string) bool {
	switch s {
	case model.TaskStatePending, model.TaskStateRunning, model.TaskStateSucceeded,
		model.TaskStatePartial, model.TaskStateFailed, model.TaskStateCanceled:
		return true
	default:
		return false
	}
}

// CancelAdminTask 取消任务：pending/running 才允许，未执行步骤一并置 canceled。
func (r *Repository) CancelAdminTask(ctx context.Context, actor Actor, taskID int64, reason string) (*model.AdminTask, error) {
	if err := r.requireOperator(actor); err != nil {
		return nil, err
	}
	if taskID <= 0 {
		return nil, fmt.Errorf("operation: invalid task_id %d", taskID)
	}
	task, err := r.taskMd.FindOne(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, model.ErrTaskNotFound
	}
	if model.IsTaskFinalState(task.State) {
		return nil, fmt.Errorf("%w: task %d already %s", model.ErrTaskBadTransition, taskID, task.State)
	}
	if !model.CanTaskTransition(task.State, model.TaskStateCanceled) {
		return nil, fmt.Errorf("%w: %s -> %s", model.ErrTaskBadTransition, task.State, model.TaskStateCanceled)
	}
	if _, err := r.stepMd.CancelPending(ctx, taskID); err != nil {
		return nil, err
	}
	ok, err := r.taskMd.TransitionState(ctx, taskID, task.State, model.TaskStateCanceled, 0, 0)
	if err != nil {
		return nil, err
	}
	if !ok {
		// 并发推进已改变状态：让调用方重读，不猜测结果。
		return nil, fmt.Errorf("%w: state changed concurrently, reload task %d", model.ErrTaskBadTransition, taskID)
	}
	resourceID := strconv.FormatInt(taskID, 10) + reasonSuffix(reason)
	if err := r.writeAudit(ctx, actor, actionTaskCancel, "admin_task", resourceID, model.AuditResultOK); err != nil {
		return nil, err
	}
	return r.taskMd.FindOne(ctx, taskID)
}

// RunAdminTask 推进任务至多 maxSteps 步（maxSteps<=0 时用服务端默认）。
// 返回本次执行的步骤与推进后的任务。下游失败只标记该步骤 failed，
// 不中断整批（运营需要看到全量结果），最终状态由步骤计数推导。
func (r *Repository) RunAdminTask(ctx context.Context, actor Actor, taskID int64, maxSteps int32) (*TaskView, int32, error) {
	if err := r.requireOperator(actor); err != nil {
		return nil, 0, err
	}
	if taskID <= 0 {
		return nil, 0, fmt.Errorf("operation: invalid task_id %d", taskID)
	}
	if maxSteps <= 0 || maxSteps > r.cfg.RunSteps {
		maxSteps = r.cfg.RunSteps
	}
	task, err := r.taskMd.FindOne(ctx, taskID)
	if err != nil {
		return nil, 0, err
	}
	if task == nil {
		return nil, 0, model.ErrTaskNotFound
	}
	switch task.State {
	case model.TaskStatePending:
		ok, err := r.taskMd.TransitionState(ctx, taskID, model.TaskStatePending, model.TaskStateRunning, nowUnix(), 0)
		if err != nil {
			return nil, 0, err
		}
		if !ok {
			return nil, 0, fmt.Errorf("%w: task %d state changed concurrently", model.ErrTaskBadTransition, taskID)
		}
		task.State = model.TaskStateRunning
	case model.TaskStateRunning:
		// 继续推进未执行步骤。
	default:
		// 终态任务不再执行，直接回传现状（幂等：重复触发无副作用）。
		view, err := r.GetAdminTask(ctx, taskID, "")
		if err != nil {
			return nil, 0, err
		}
		return view, 0, nil
	}

	pending, err := r.stepMd.ListExecutable(ctx, taskID, maxSteps)
	if err != nil {
		return nil, 0, err
	}
	var executed int32
	for _, step := range pending {
		// 先抢占 pending -> running，抢不到的步骤说明已有推进者在跑，跳过。
		claimed, err := r.stepMd.MarkStep(ctx, step.ID, model.StepStatePending, model.StepStateRunning, "", "")
		if err != nil {
			return nil, executed, err
		}
		if !claimed {
			continue
		}
		executed++
		result, runErr := r.runStep(ctx, actor, task, step)
		from, to := model.StepStateRunning, model.StepStateSucceeded
		errMsg := ""
		if runErr != nil {
			to = model.StepStateFailed
			errMsg = truncate(runErr.Error(), 500)
			logx.Errorf("operation/task: task=%d step=%d type=%s target=%s/%s err=%v",
				task.TaskID, step.StepNo, task.TaskType, step.TargetType, step.TargetID, runErr)
		}
		if _, err := r.stepMd.MarkStep(ctx, step.ID, from, to, truncate(result, 500), errMsg); err != nil {
			return nil, executed, err
		}
		var sd, fd int32
		if to == model.StepStateSucceeded {
			sd = 1
		} else {
			fd = 1
		}
		if err := r.taskMd.AddCounters(ctx, taskID, sd, fd); err != nil {
			return nil, executed, err
		}
	}

	final, err := r.settleTask(ctx, actor, taskID)
	if err != nil {
		return nil, executed, err
	}
	view := &TaskView{Task: final}
	view.Steps, err = r.stepMd.ListByTask(ctx, taskID)
	if err != nil {
		return nil, executed, err
	}
	return view, executed, nil
}

// settleTask 在没有待执行步骤时把任务收敛到终态；仍有 pending 步骤则保持 running。
func (r *Repository) settleTask(ctx context.Context, actor Actor, taskID int64) (*model.AdminTask, error) {
	task, err := r.taskMd.FindOne(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, model.ErrTaskNotFound
	}
	counts, err := r.stepMd.CountByTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if counts[model.StepStatePending] > 0 || counts[model.StepStateRunning] > 0 {
		return task, nil
	}
	if model.IsTaskFinalState(task.State) {
		return task, nil
	}
	final := model.ResolveTaskFinalState(task.Total, task.Succeeded, task.Failed)
	if !model.CanTaskTransition(task.State, final) {
		return nil, fmt.Errorf("%w: %s -> %s", model.ErrTaskBadTransition, task.State, final)
	}
	ok, err := r.taskMd.TransitionState(ctx, taskID, task.State, final, 0, nowUnix())
	if err != nil {
		return nil, err
	}
	if !ok {
		return r.taskMd.FindOne(ctx, taskID)
	}
	if err := r.writeAudit(ctx, actor, actionTaskRun, "admin_task",
		fmt.Sprintf("%d->%s", taskID, final), model.AuditResultOK); err != nil {
		return nil, err
	}
	return r.taskMd.FindOne(ctx, taskID)
}

// runStep 执行单个步骤：只调用下游公开 RPC，不碰其它服务的库。
// 返回下游结果摘要（写入 step.result，便于运营核对）。
func (r *Repository) runStep(ctx context.Context, actor Actor, task *model.AdminTask, step *model.AdminTaskStep) (string, error) {
	targetID, err := strconv.ParseInt(step.TargetID, 10, 64)
	if err != nil || targetID <= 0 {
		return "", fmt.Errorf("operation: step target_id %q invalid", step.TargetID)
	}
	params := parseTaskParams(task.Params)
	reason := truncate(params.Reason, 200)

	switch task.TaskType {
	case model.TaskTypeBatchOfflineSubmission:
		gw, err := r.downstream.requireVideo()
		if err != nil {
			return "", err
		}
		if err := gw.OfflineSubmission(ctx, targetID, stepOperator(actor), reason, actor.IP); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"aid":%d,"state":"offline"}`, targetID), nil

	case model.TaskTypeBatchOfflineEpisode:
		gw, err := r.downstream.requireCatalog()
		if err != nil {
			return "", err
		}
		if err := gw.OfflineEpisode(ctx, targetID); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"epid":%d,"state":"offline"}`, targetID), nil

	case model.TaskTypeBatchExpireWindow:
		gw, err := r.downstream.requireRights()
		if err != nil {
			return "", err
		}
		if err := gw.ExpireWindow(ctx, targetID, actor.IP); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"window_id":%d,"state":"expired"}`, targetID), nil

	case model.TaskTypeBatchRejectAppeal:
		gw, err := r.downstream.requireModeration()
		if err != nil {
			return "", err
		}
		if err := gw.RejectAppeal(ctx, targetID, actor.AdminID, reason, actor.IP); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"appeal_id":%d,"verdict":"reject"}`, targetID), nil

	default:
		return "", model.ErrTaskTypeUnknown
	}
}

// stepOperator 下游状态机要求 operator 是可读标识；优先用户名，退化到 ID。
func stepOperator(actor Actor) string {
	if name := strings.TrimSpace(actor.Username); name != "" {
		return truncate(name, 64)
	}
	return "admin:" + strconv.FormatInt(actor.AdminID, 10)
}
