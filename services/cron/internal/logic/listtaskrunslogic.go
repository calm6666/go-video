package logic

import (
	"context"
	"fmt"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListTaskRunsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListTaskRunsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListTaskRunsLogic {
	return &ListTaskRunsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询执行记录。
//
// 游标是上一页最后一行的 run_id（model 侧按 planned_at DESC, id DESC 走 idx_planned），
// 倒序遍历保证「同秒多条执行」也不会漏读。
func (l *ListTaskRunsLogic) ListTaskRuns(in *rpc.ListTaskRunsReq) (*rpc.ListTaskRunsReply, error) {
	if in == nil {
		in = &rpc.ListTaskRunsReq{}
	}
	limit, err := l.svcCtx.PageSize(in.PageSize)
	if err != nil {
		return nil, err
	}
	cursorID, err := decodeIDCursor(in.Cursor)
	if err != nil {
		return nil, err
	}
	if in.PlannedFrom > 0 && in.PlannedTo > 0 && in.PlannedFrom > in.PlannedTo {
		return nil, fmt.Errorf("%w: planned_from=%d planned_to=%d",
			model.ErrInvalidTimeRange, in.PlannedFrom, in.PlannedTo)
	}

	state := int32(in.State)
	rows, next, err := l.svcCtx.Runs.ListByCursor(l.ctx, in.TaskKey, state,
		in.PlannedFrom, in.PlannedTo, cursorID, limit)
	if err != nil {
		l.Errorf("ListTaskRuns failed, task_key=%s state=%d", in.TaskKey, state)
		return nil, err
	}
	total, err := l.svcCtx.Runs.CountByFilter(l.ctx, in.TaskKey, state, in.PlannedFrom, in.PlannedTo)
	if err != nil {
		l.Errorf("ListTaskRuns count failed, task_key=%s state=%d", in.TaskKey, state)
		return nil, err
	}
	return &rpc.ListTaskRunsReply{
		List:       runRecordList(rows),
		NextCursor: encodeIDCursor(next),
		HasMore:    next > 0,
		Total:      total,
	}, nil
}
