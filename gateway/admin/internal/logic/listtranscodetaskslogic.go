// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	transcoderpc "go-video/services/transcode/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListTranscodeTasksLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询转码任务（按 asset_id/state 过滤）
func NewListTranscodeTasksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListTranscodeTasksLogic {
	return &ListTranscodeTasksLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 分页查询转码任务：聚合 transcode ListTasks RPC。
// asset_id/state 传 0 表示不过滤，过滤与分页由 transcode 服务完成。
func (l *ListTranscodeTasksLogic) ListTranscodeTasks(req *types.ParamListTranscodeTasks) (resp *types.TranscodeTasksResponse, err error) {
	if l.svcCtx.Transcode == nil {
		return nil, errors.New("transcode service not configured")
	}
	reply, err := l.svcCtx.Transcode.ListTasks(l.ctx, &transcoderpc.ListReq{
		AssetId: req.AssetId,
		State:   transcoderpc.TaskState(req.State),
		Pn:      req.Pn,
		Ps:      req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listTranscodeTasks: asset_id=%d state=%d pn=%d ps=%d err=%v", req.AssetId, req.State, req.Pn, req.Ps, err)
		return nil, err
	}
	tasks := make([]types.TranscodeTaskItem, 0, len(reply.GetTasks()))
	for _, t := range reply.GetTasks() {
		tasks = append(tasks, types.TranscodeTaskItem{
			TaskId:       t.GetTaskId(),
			AssetId:      t.GetAssetId(),
			TemplateId:   t.GetTemplateId(),
			InputBucket:  t.GetInputBucket(),
			InputKey:     t.GetInputKey(),
			OutputBucket: t.GetOutputBucket(),
			OutputKey:    t.GetOutputKey(),
			State:        int32(t.GetState()),
			Progress:     t.GetProgress(),
			Errno:        t.GetErrno(),
			ErrMsg:       t.GetErrMsg(),
			Ctime:        t.GetCtime(),
			Mtime:        t.GetMtime(),
		})
	}
	return &types.TranscodeTasksResponse{
		Code:    0,
		Message: "ok",
		Data: types.TranscodeTasksData{
			Total: int64(reply.GetTotal()),
			Tasks: tasks,
		},
		TTL: 0,
	}, nil
}
