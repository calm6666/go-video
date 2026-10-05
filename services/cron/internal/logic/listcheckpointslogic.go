package logic

import (
	"context"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListCheckpointsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListCheckpointsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListCheckpointsLogic {
	return &ListCheckpointsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询游标。
//
// cursor 是 task_key + "\x1f" + scope_key 复合键，由 model 解析与生成；
// 非法游标返回 model.ErrInvalidCursor（不退化成「当作第一页」）。
func (l *ListCheckpointsLogic) ListCheckpoints(in *rpc.ListCheckpointsReq) (*rpc.ListCheckpointsReply, error) {
	if in == nil {
		in = &rpc.ListCheckpointsReq{}
	}
	limit, err := l.svcCtx.PageSize(in.PageSize)
	if err != nil {
		return nil, err
	}
	cursor, err := decodeCompositeCursor(in.Cursor)
	if err != nil {
		return nil, err
	}

	rows, next, err := l.svcCtx.Checkpoints.ListByCursor(l.ctx, in.TaskKey, cursor, limit)
	if err != nil {
		l.Errorf("ListCheckpoints failed, task_key=%s", in.TaskKey)
		return nil, err
	}
	total, err := l.svcCtx.Checkpoints.CountByTask(l.ctx, in.TaskKey)
	if err != nil {
		l.Errorf("ListCheckpoints count failed, task_key=%s", in.TaskKey)
		return nil, err
	}
	return &rpc.ListCheckpointsReply{
		List:       checkpointInfoList(rows),
		NextCursor: next,
		HasMore:    next != "",
		Total:      total,
	}, nil
}
