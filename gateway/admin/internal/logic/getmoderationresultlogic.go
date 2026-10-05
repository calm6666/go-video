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

type GetModerationResultLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询审核结论（按任务 ID）
func NewGetModerationResultLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetModerationResultLogic {
	return &GetModerationResultLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 查询审核结论：聚合 moderation-orchestrator GetResult RPC。
// verdict 取值与 moderation.v1.Verdict 枚举一致（1 通过 / 2 转人审 / 3 拒绝）。
func (l *GetModerationResultLogic) GetModerationResult(req *types.ParamModerationTaskId) (resp *types.ModerationResultResponse, err error) {
	if l.svcCtx.Moderation == nil {
		return nil, errors.New("moderation service not configured")
	}
	reply, err := l.svcCtx.Moderation.GetResult(l.ctx, &moderationrpc.ResultReq{
		TaskId: req.TaskId,
	})
	if err != nil {
		l.Errorf("gateway/admin/getModerationResult: task_id=%d err=%v", req.TaskId, err)
		return nil, err
	}
	result := reply.GetResult()
	data := types.ModerationResultItem{
		TaskId:   result.GetTaskId(),
		Verdict:  int32(result.GetVerdict()),
		Reason:   result.GetReason(),
		WorkerId: result.GetWorkerId(),
		Reviewer: result.GetReviewer(),
		Ctime:    result.GetCtime(),
	}
	return &types.ModerationResultResponse{
		Code:    0,
		Message: "ok",
		Data:    types.ModerationResultData{Result: data},
		TTL:     0,
	}, nil
}
