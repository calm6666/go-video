package logic

import (
	"context"

	"go-video/services/search-indexer/internal/svc"
	"go-video/services/search-indexer/model"
	"go-video/services/search-indexer/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRebuildTasksLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRebuildTasksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRebuildTasksLogic {
	return &ListRebuildTasksLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询重建任务（cursor + 状态过滤）。
func (l *ListRebuildTasksLogic) ListRebuildTasks(in *rpc.ListRebuildTasksReq) (*rpc.ListRebuildTasksReply, error) {
	tasks, next, err := l.svcCtx.Repository.ListRebuildTasks(l.ctx, in.State, in.Cursor, int(in.Limit))
	if err != nil {
		l.Errorf("search-indexer/ListRebuildTasks: state=%s cursor=%s limit=%d err=%v",
			in.State, in.Cursor, in.Limit, err)
		return nil, err
	}

	var dlq int64
	if cnt, cerr := l.svcCtx.Repository.DeadLetterCount(l.ctx, model.DLQStateOpen); cerr != nil {
		// 观测值读失败不影响列表返回，但要留下日志（不能静默当成 0）。
		l.Errorf("search-indexer/ListRebuildTasks: 统计死信失败，dlq_count 上报为 0 err=%v", cerr)
	} else {
		dlq = cnt
	}

	out := make([]*rpc.RebuildTask, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, taskToRPC(t, dlq))
	}
	return &rpc.ListRebuildTasksReply{Tasks: out, NextCursor: next}, nil
}
