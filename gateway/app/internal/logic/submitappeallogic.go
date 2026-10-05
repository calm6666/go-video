// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	moderationrpc "go-video/services/moderation-orchestrator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SubmitAppealLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 对驳回结论提交申诉（仅作者本人）
func NewSubmitAppealLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitAppealLogic {
	return &SubmitAppealLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SubmitAppeal 聚合 moderation-orchestrator SubmitAppeal RPC：申诉仅限作者本人，
// 且只有处于驳回结论的任务可申诉，两者都由 moderation 校验（AGENTS.md §8）。
func (l *SubmitAppealLogic) SubmitAppeal(req *types.ParamSubmitAppeal) (resp *types.ModerationAppealResponse, err error) {
	if l.svcCtx.Moderation == nil {
		return nil, errors.New("moderation service not configured")
	}
	reply, err := l.svcCtx.Moderation.SubmitAppeal(l.ctx, &moderationrpc.AppealReq{
		TaskId:  req.TaskId,
		Mid:     req.Mid,
		Content: req.Content,
		Ip:      req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/submitAppeal: task_id=%d mid=%d err=%v", req.TaskId, req.Mid, err)
		return nil, err
	}
	data := types.ModerationAppealData{}
	if reply.GetAppeal() != nil {
		data.Appeal = appealToAPI(reply.GetAppeal())
	}
	return &types.ModerationAppealResponse{
		Code:    0,
		Message: "ok",
		Data:    data,
		TTL:     0,
	}, nil
}
