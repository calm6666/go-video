package logic

import (
	"context"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListTasksLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListTasksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListTasksLogic {
	return &ListTasksLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页列出任务定义。
//
// 游标语义：cursor 是上一页最后一行的 id（任务定义是小表，按 id 升序稳定遍历，
// 与 docs/api-and-events.md §2 的「游标优先于页码」一致）；
// page_size 越界直接报错，不静默放大（AGENTS.md §6）。
func (l *ListTasksLogic) ListTasks(in *rpc.ListTasksReq) (*rpc.ListTasksReply, error) {
	if in == nil {
		in = &rpc.ListTasksReq{}
	}
	limit, err := l.svcCtx.PageSize(in.PageSize)
	if err != nil {
		return nil, err
	}
	cursorID, err := decodeIDCursor(in.Cursor)
	if err != nil {
		return nil, err
	}

	state := int32(in.State) // UNSPECIFIED(0) 在查询语境表示「全部状态」
	rows, next, err := l.svcCtx.TaskDefinitions.ListByCursor(l.ctx, state, in.TaskGroup, in.Handler, cursorID, limit)
	if err != nil {
		l.Errorf("ListTasks failed, state=%d group=%s handler=%s", state, in.TaskGroup, in.Handler)
		return nil, err
	}
	total, err := l.svcCtx.TaskDefinitions.CountByFilter(l.ctx, state, in.TaskGroup, in.Handler)
	if err != nil {
		l.Errorf("ListTasks count failed, state=%d group=%s handler=%s", state, in.TaskGroup, in.Handler)
		return nil, err
	}
	return &rpc.ListTasksReply{
		List:       taskDefinitionList(rows),
		NextCursor: encodeIDCursor(next),
		HasMore:    next > 0,
		Total:      total,
	}, nil
}
