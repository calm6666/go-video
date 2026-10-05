package logic

import (
	"context"
	"strings"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PauseTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPauseTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PauseTaskLogic {
	return &PauseTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 暂停任务（可恢复）。
//
// 暂停语义 = 「不再产生新的计划点」，不是「掐断在跑的执行」：
// model.SetState 会把 next_fire_at 归零，于是任务退出到期扫描；已在 RUNNING 的执行
// 继续心跳到结束（强行掐断只会留下半成品副作用）。但 AcquireLease 拒绝非 ENABLED
// 任务的领取，所以暂停后不会再有新执行（手动 TriggerTask 除外，见该 logic 说明）。
func (l *PauseTaskLogic) PauseTask(in *rpc.PauseTaskReq) (*rpc.TaskOperationReply, error) {
	if in == nil {
		return nil, model.ErrTaskKeyEmpty
	}
	if err := requireIdempotencyKey(in.IdempotencyKey); err != nil {
		return nil, err
	}
	// 暂停是运营动作，无痕暂停不允许：没有 reason 就答不出「谁在什么时候为什么停的」。
	if err := requireReason(in.Reason); err != nil {
		return nil, err
	}
	taskKey := strings.TrimSpace(in.TaskKey)
	if taskKey == "" {
		return nil, model.ErrTaskKeyEmpty
	}

	changed, auditID, _, err := (&stateTransition{
		taskKey:     taskKey,
		action:      model.AuditActionPause,
		operator:    operatorOf(in.Operator, l.svcCtx),
		reason:      in.Reason,
		traceID:     in.TraceId,
		toState:     model.TaskStatePaused,
		allowedFrom: []int32{model.TaskStateEnabled},
	}).apply(l.ctx, l.svcCtx)
	if err != nil {
		l.Errorf("PauseTask failed, task_key=%s", taskKey)
		return nil, err
	}
	return operationReply(l.ctx, l.svcCtx, taskKey, changed, auditID)
}
