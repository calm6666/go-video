package logic

import (
	"context"
	"time"

	"go-video/services/transcode/internal/svc"
	"go-video/services/transcode/model"
	"go-video/services/transcode/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SubmitTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSubmitTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitTaskLogic {
	return &SubmitTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// SubmitTask 创建转码任务（PENDING 状态）。
// 本期占位：仅写库创建 PENDING 任务，不调用 FFmpeg；实际转码由独立 Worker 消费 MQ 实现。
func (l *SubmitTaskLogic) SubmitTask(in *rpc.SubmitTaskReq) (*rpc.TaskReply, error) {
	if in.AssetId <= 0 {
		return nil, model.ErrInvalidAssetID
	}
	if in.TemplateId <= 0 {
		return nil, model.ErrInvalidTemplateID
	}
	if in.InputBucket == "" || in.InputKey == "" || in.OutputBucket == "" || in.OutputKey == "" {
		return nil, model.ErrInvalidState
	}
	now := time.Now().Unix()
	task := &model.TranscodeTask{
		AssetId:      in.AssetId,
		TemplateId:   in.TemplateId,
		InputBucket:  in.InputBucket,
		InputKey:     in.InputKey,
		OutputBucket: in.OutputBucket,
		OutputKey:    in.OutputKey,
		State:        model.TaskStatePending,
		Progress:     0,
		Errno:        0,
		ErrMsg:       "",
		Ctime:        now,
		Mtime:        now,
	}
	taskID, err := l.svcCtx.Repository.SubmitTask(l.ctx, task)
	if err != nil {
		l.Errorf("transcode/SubmitTask: asset=%d tpl=%d err=%v", in.AssetId, in.TemplateId, err)
		return nil, err
	}
	task.TaskId = taskID
	l.Infof("transcode/SubmitTask: task_id=%d asset=%d tpl=%d (placeholder, no FFmpeg)",
		taskID, in.AssetId, in.TemplateId)
	return toTaskReply(task), nil
}
