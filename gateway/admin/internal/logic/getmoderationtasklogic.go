// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	moderationrpc "go-video/services/moderation-orchestrator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetModerationTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询审核任务详情
func NewGetModerationTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetModerationTaskLogic {
	return &GetModerationTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 查询审核任务详情：聚合 moderation-orchestrator GetTask RPC。
// 审核结论的所有者是 moderation-orchestrator，网关只读展示，不推进任何状态（AGENTS.md §5）。
func (l *GetModerationTaskLogic) GetModerationTask(req *types.ParamModerationTaskId) (resp *types.ModerationTaskResponse, err error) {
	if l.svcCtx.Moderation == nil {
		return nil, errors.New("moderation service not configured")
	}
	reply, err := l.svcCtx.Moderation.GetTask(l.ctx, &moderationrpc.TaskReq{
		TaskId: req.TaskId,
	})
	if err != nil {
		l.Errorf("gateway/admin/getModerationTask: task_id=%d err=%v", req.TaskId, err)
		return nil, err
	}
	task := reply.GetTask()
	data := types.ModerationTaskItem{
		TaskId:       task.GetTaskId(),
		SubmissionId: task.GetSubmissionId(),
		ContentType:  int32(task.GetContentType()),
		Mid:          task.GetMid(),
		UpMid:        task.GetUpMid(),
		Business:     task.GetBusiness(),
		Reason:       task.GetReason(),
		State:        int32(task.GetState()),
		Ctime:        task.GetCtime(),
		Mtime:        task.GetMtime(),
		Operator:     task.GetOperator(),
	}
	return &types.ModerationTaskResponse{
		Code:    0,
		Message: "ok",
		Data:    types.ModerationTaskData{Task: data},
		TTL:     0,
	}, nil
}
