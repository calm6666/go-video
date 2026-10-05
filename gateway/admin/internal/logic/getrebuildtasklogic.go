// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	searchindexerrpc "go-video/services/search-indexer/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetRebuildTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询单个重建任务进度
func NewGetRebuildTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRebuildTaskLogic {
	return &GetRebuildTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 查询重建任务进度：聚合 search-indexer GetRebuildTask RPC。
// 只读投影，不改任务状态；task 不存在时由服务端返回明确错误，网关不伪造空任务。
func (l *GetRebuildTaskLogic) GetRebuildTask(req *types.ParamRebuildTaskId) (resp *types.SearchRebuildTaskResponse, err error) {
	if l.svcCtx.SearchIndexer == nil {
		return nil, errors.New("search-indexer service not configured")
	}
	if err := requireOperatorID(req.OperatorId); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("task_id", req.TaskId); err != nil {
		return nil, err
	}
	// 契约缺口：searchindexer.v1.GetRebuildTaskReq 没有 operator/operator_id 字段，
	// 读接口的审计主体只能记在网关日志里，任务表看不到「谁查过」。
	reply, err := l.svcCtx.SearchIndexer.GetRebuildTask(l.ctx, &searchindexerrpc.GetRebuildTaskReq{
		TaskId: req.TaskId,
	})
	if err != nil {
		l.Errorf("gateway/admin/getRebuildTask: task_id=%s operator_id=%d err=%v", req.TaskId, req.OperatorId, err)
		return nil, err
	}
	return &types.SearchRebuildTaskResponse{
		Code:    0,
		Message: "ok",
		Data:    types.SearchRebuildTaskData{Task: rebuildTaskToAPI(reply)},
		TTL:     0,
	}, nil
}
