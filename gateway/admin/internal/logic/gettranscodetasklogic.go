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

type GetTranscodeTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询转码任务详情
func NewGetTranscodeTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetTranscodeTaskLogic {
	return &GetTranscodeTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 查询转码任务详情：聚合 transcode GetTask RPC。
// 任务状态取值与 transcode.v1.TaskState 枚举一致，网关不解释状态机语义（AGENTS.md §5）。
func (l *GetTranscodeTaskLogic) GetTranscodeTask(req *types.ParamTranscodeTaskId) (resp *types.TranscodeTaskResponse, err error) {
	if l.svcCtx.Transcode == nil {
		return nil, errors.New("transcode service not configured")
	}
	reply, err := l.svcCtx.Transcode.GetTask(l.ctx, &transcoderpc.TaskReq{
		TaskId: req.TaskId,
	})
	if err != nil {
		l.Errorf("gateway/admin/getTranscodeTask: task_id=%d err=%v", req.TaskId, err)
		return nil, err
	}
	// pb getter 对 nil reply 安全，返回零值条目而不是报错。
	task := types.TranscodeTaskItem{
		TaskId:       reply.GetTaskId(),
		AssetId:      reply.GetAssetId(),
		TemplateId:   reply.GetTemplateId(),
		InputBucket:  reply.GetInputBucket(),
		InputKey:     reply.GetInputKey(),
		OutputBucket: reply.GetOutputBucket(),
		OutputKey:    reply.GetOutputKey(),
		State:        int32(reply.GetState()),
		Progress:     reply.GetProgress(),
		Errno:        reply.GetErrno(),
		ErrMsg:       reply.GetErrMsg(),
		Ctime:        reply.GetCtime(),
		Mtime:        reply.GetMtime(),
	}
	return &types.TranscodeTaskResponse{
		Code:    0,
		Message: "ok",
		Data:    types.TranscodeTaskData{Task: task},
		TTL:     0,
	}, nil
}
