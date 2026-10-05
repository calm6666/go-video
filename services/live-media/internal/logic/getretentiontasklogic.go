package logic

import (
	"context"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetRetentionTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetRetentionTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRetentionTaskLogic {
	return &GetRetentionTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询单个回收任务
//
// 只读方法：不加锁、不推进状态、不发事件。retention_id<=0 直接判不存在且不查库。
// 本方法刻意不走缓存（与三类任务详情读不同）：回收是低频、强审计的读，
// state/scanned/deleted 必须实时——缓存会把「已登记未执行」和「已执行未清理」答成同一个值，
// 而这两者的运维含义相反。Worker 领取待执行队列也不走本方法（那会退化成轮询全表，
// 队列入口是 model.ListByState，未在 rpc 暴露）。
// 投影：target_kind/room_id/target_id/expire_before/purge/batch_limit 是登记时的意图快照，
// 终态后不回改；reason/operator 原样返回（审计归因）；fail_reason/errno/err_msg 写入时已脱敏。
func (l *GetRetentionTaskLogic) GetRetentionTask(in *rpc.RetentionTaskReq) (*rpc.LiveRetentionTaskInfo, error) {
	retentionID := in.GetRetentionId()
	if err := checkPositive("retention_id", retentionID, model.ErrRetentionTaskNotFound); err != nil {
		return nil, err
	}
	task, err := l.svcCtx.RetentionTasks.FindOne(l.ctx, retentionID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, model.ErrRetentionTaskNotFound
	}
	return retentionInfo(task), nil
}
