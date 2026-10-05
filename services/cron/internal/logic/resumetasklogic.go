package logic

import (
	"context"
	"strings"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ResumeTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewResumeTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ResumeTaskLogic {
	return &ResumeTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 恢复任务，按 MisfirePolicy 处理暂停期间的过期计划点。
//
// 只有 PAUSED 可恢复：DISABLED 表示「这个任务不再被支持」，恢复它是一次重新注册
// （代码可能已经变了），不提供 ResumeTask 捷径。
// 指针重算在事务内基于库里的最新行做，避免拿请求前的旧快照算出未来计划点。
func (l *ResumeTaskLogic) ResumeTask(in *rpc.ResumeTaskReq) (*rpc.TaskOperationReply, error) {
	if in == nil {
		return nil, model.ErrTaskKeyEmpty
	}
	if err := requireIdempotencyKey(in.IdempotencyKey); err != nil {
		return nil, err
	}
	taskKey := strings.TrimSpace(in.TaskKey)
	if taskKey == "" {
		return nil, model.ErrTaskKeyEmpty
	}
	defaults := defaultsOf(l.svcCtx)
	operator := operatorOf(in.Operator, l.svcCtx)
	now := l.svcCtx.ServerTime()

	changed, auditID, _, err := (&stateTransition{
		taskKey:     taskKey,
		action:      model.AuditActionResume,
		operator:    operator,
		reason:      resumeReasonText,
		traceID:     in.TraceId,
		toState:     model.TaskStateEnabled,
		allowedFrom: []int32{model.TaskStatePaused},
		planNextFireAt: func(cur *model.TaskDefinition) (int64, error) {
			next, err := planResumeFireAt(cur, defaults, now)
			if err != nil {
				return 0, err
			}
			if next == cur.NextFireAt {
				return next, nil
			}
			l.Infof("cron/resume: task_key=%s 指针 %d -> %d（misfire_policy=%d）",
				cur.TaskKey, cur.NextFireAt, next, cur.MisfirePolicy)
			return next, nil
		},
		extra: map[string]any{"resumed_by": operator},
	}).apply(l.ctx, l.svcCtx)
	if err != nil {
		l.Errorf("ResumeTask failed, task_key=%s", taskKey)
		return nil, err
	}
	return operationReply(l.ctx, l.svcCtx, taskKey, changed, auditID)
}

// resumeReasonText 恢复动作的审计原因文本。
//
// 契约缺口：ResumeTaskReq 没有 reason 字段（PauseTaskReq/DisableTaskReq 都有），
// 所以这里只能落固定文本。已在 README「契约缺口」登记，等契约补字段后改为透传。
const resumeReasonText = "ResumeTask：按任务的 misfire_policy 恢复调度（契约无 reason 字段）"
