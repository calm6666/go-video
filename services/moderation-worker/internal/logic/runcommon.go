package logic

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/oklog/ulid/v2"

	"go-video/services/moderation-worker/internal/svc"
	"go-video/services/moderation-worker/model"
	"go-video/services/moderation-worker/rpc"
)

// runParams 是 Run* 方法共享的执行参数。
type runParams struct {
	workerTaskID string
	taskID       string
	capability   int32
}

// prepareRun 校验 RunTaskReq 并生成 worker_task_id（若为空），返回标准化后的执行参数。
// 同时把 PENDING 任务记录落库，便于后续 GetTaskResult 反查。
func prepareRun(svcCtx *svc.ServiceContext, in *rpc.RunTaskReq, expectedCap rpc.CapabilityType) (*runParams, error) {
	if in.GetTaskId() == "" {
		return nil, model.ErrInvalidTaskID
	}
	if in.GetCapability() != expectedCap {
		return nil, model.ErrCapabilityMismatch
	}
	if in.GetMediaUri() == "" {
		return nil, model.ErrInvalidMediaURI
	}

	workerTaskID := in.GetWorkerTaskId()
	if workerTaskID == "" {
		// 本期使用 ULID 作为 worker_task_id；后续接入可改用业务可读 ID
		workerTaskID = ulid.Make().String()
	}

	paramsJSON := ""
	if len(in.GetParams()) > 0 {
		if bs, err := json.Marshal(in.GetParams()); err == nil {
			paramsJSON = string(bs)
		}
	}

	timeoutMs := in.GetTimeoutMs()
	if timeoutMs <= 0 {
		timeoutMs = svcCtx.Config.DefaultTimeoutMs
	}

	now := model.NowUnix()
	task := &model.WorkerTask{
		WorkerTaskID: workerTaskID,
		TaskID:       in.GetTaskId(),
		Capability:   int32(in.GetCapability()),
		MediaURI:     in.GetMediaUri(),
		DurationMs:   in.GetDurationMs(),
		ParamsJSON:   paramsJSON,
		TimeoutMs:    timeoutMs,
		TraceID:      in.GetTraceId(),
		State:        model.TaskStatePending,
		Ctime:        now,
		Mtime:        now,
	}
	if err := svcCtx.Repository.CreateTask(context.Background(), task); err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}

	return &runParams{
		workerTaskID: workerTaskID,
		taskID:       in.GetTaskId(),
		capability:   int32(in.GetCapability()),
	}, nil
}

// finishRun 写入执行结果并返回 TaskResultReply。
// 本期 Run* 方法不调用真实算法，segments 为空，elapsed_ms 为 0。
// TODO: 接入 OCR/ASR/图像/音频算法后替换此处实现。
func finishRun(svcCtx *svc.ServiceContext, ctx context.Context, p *runParams, cap rpc.CapabilityType) (*rpc.TaskResultReply, error) {
	emptyResult, _ := json.Marshal([]*rpc.ResultSegment{})
	if err := svcCtx.Repository.FinishTask(ctx, p.workerTaskID, model.TaskStateSucceeded, "", 0, string(emptyResult), ""); err != nil {
		return nil, fmt.Errorf("finish task: %w", err)
	}

	return &rpc.TaskResultReply{
		WorkerTaskId:     p.workerTaskID,
		TaskId:           p.taskID,
		Capability:       cap,
		State:            rpc.TaskState_TASK_STATE_SUCCEEDED,
		AlgorithmVersion: "",
		ElapsedMs:        0,
		Segments:         nil,
		ErrorMessage:     "",
	}, nil
}
