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

type ListModerationTasksLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询审核任务
func NewListModerationTasksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListModerationTasksLogic {
	return &ListModerationTasksLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 分页查询审核任务：聚合 moderation-orchestrator ListTasks RPC。
// mid/content_type/state 传 0 表示不过滤，过滤与分页由下游服务完成。
func (l *ListModerationTasksLogic) ListModerationTasks(req *types.ParamListModerationTasks) (resp *types.ModerationTasksResponse, err error) {
	if l.svcCtx.Moderation == nil {
		return nil, errors.New("moderation service not configured")
	}
	reply, err := l.svcCtx.Moderation.ListTasks(l.ctx, &moderationrpc.ListTasksReq{
		Mid:         req.Mid,
		ContentType: moderationrpc.ContentType(req.ContentType),
		State:       moderationrpc.TaskState(req.State),
		Pn:          req.Pn,
		Ps:          req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listModerationTasks: mid=%d content_type=%d state=%d pn=%d ps=%d err=%v",
			req.Mid, req.ContentType, req.State, req.Pn, req.Ps, err)
		return nil, err
	}
	tasks := make([]types.ModerationTaskItem, 0, len(reply.GetTasks()))
	for _, t := range reply.GetTasks() {
		tasks = append(tasks, types.ModerationTaskItem{
			TaskId:       t.GetTaskId(),
			SubmissionId: t.GetSubmissionId(),
			ContentType:  int32(t.GetContentType()),
			Mid:          t.GetMid(),
			UpMid:        t.GetUpMid(),
			Business:     t.GetBusiness(),
			Reason:       t.GetReason(),
			State:        int32(t.GetState()),
			Ctime:        t.GetCtime(),
			Mtime:        t.GetMtime(),
			Operator:     t.GetOperator(),
		})
	}
	return &types.ModerationTasksResponse{
		Code:    0,
		Message: "ok",
		Data: types.ModerationTasksData{
			Total: int64(reply.GetTotal()),
			Tasks: tasks,
		},
		TTL: 0,
	}, nil
}
