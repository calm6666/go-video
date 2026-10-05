package logic

import (
	"context"
	"strings"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DisableTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDisableTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DisableTaskLogic {
	return &DisableTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 停用任务（终态，保留历史）。
//
// 停用与暂停的区别：DISABLED 是「这个任务不再被支持」的声明，只能重新 RegisterTask
// 或由管理员改回 ENABLED，不提供 ResumeTask 捷径。
// 这里刻意不级联删除：历史执行轨迹与游标（cron_task_run / cron_task_checkpoint）是
// 排查依据，体积问题由清理任务按 RunRetentionDays / AuditRetentionDays 分批删。
// 已在 RUNNING 的执行不强杀，但退出到期扫描后不再有新计划点。
func (l *DisableTaskLogic) DisableTask(in *rpc.DisableTaskReq) (*rpc.TaskOperationReply, error) {
	if in == nil {
		return nil, model.ErrTaskKeyEmpty
	}
	if err := requireIdempotencyKey(in.IdempotencyKey); err != nil {
		return nil, err
	}
	// 停用是「不再被支持」的声明，必须能回溯是谁说的（AGENTS.md §8）。
	if err := requireReason(in.Reason); err != nil {
		return nil, err
	}
	taskKey := strings.TrimSpace(in.TaskKey)
	if taskKey == "" {
		return nil, model.ErrTaskKeyEmpty
	}
	operator := operatorOf(in.Operator, l.svcCtx)

	changed, auditID, _, err := (&stateTransition{
		taskKey:  taskKey,
		action:   model.AuditActionDisable,
		operator: operator,
		reason:   in.Reason,
		traceID:  in.TraceId,
		toState:  model.TaskStateDisabled,
		// ENABLED 与 PAUSED 都可直接停用：运营发现任务有问题时不必先恢复再停用。
		allowedFrom: []int32{model.TaskStateEnabled, model.TaskStatePaused},
		// SetState 只在暂停时清零指针，停用最彻底：不保留任何未来计划点。
		planNextFireAt: func(cur *model.TaskDefinition) (int64, error) { return 0, nil },
	}).apply(l.ctx, l.svcCtx)
	if err != nil {
		l.Errorf("DisableTask failed, task_key=%s", taskKey)
		return nil, err
	}
	return operationReply(l.ctx, l.svcCtx, taskKey, changed, auditID)
}
