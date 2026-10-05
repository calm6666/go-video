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

type ProcessModerationAppealLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 处理申诉
func NewProcessModerationAppealLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ProcessModerationAppealLogic {
	return &ProcessModerationAppealLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 处理申诉：聚合 moderation-orchestrator ProcessAppeal RPC。
// 网关只透传处理人（handler）与最终结论，申诉状态机与幂等由下游服务校验（AGENTS.md §5、§8）；
// 审核结论不会在此直接写入稿件 PUBLISHED 状态。
func (l *ProcessModerationAppealLogic) ProcessModerationAppeal(req *types.ParamProcessAppeal) (resp *types.ModerationAppealResponse, err error) {
	if l.svcCtx.Moderation == nil {
		return nil, errors.New("moderation service not configured")
	}
	operatorID, err := adminOperatorID(l.ctx, "processModerationAppeal", req.Handler)
	if err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Moderation.ProcessAppeal(l.ctx, &moderationrpc.ProcessAppealReq{
		AppealId:     req.AppealId,
		Handler:      operatorID,
		FinalVerdict: moderationrpc.Verdict(req.FinalVerdict),
		FinalReason:  req.FinalReason,
	})
	if err != nil {
		l.Errorf("gateway/admin/processModerationAppeal: appeal_id=%d handler=%d err=%v", req.AppealId, operatorID, err)
		return nil, err
	}
	appeal := reply.GetAppeal()
	data := types.ModerationAppealItem{
		AppealId:     appeal.GetAppealId(),
		TaskId:       appeal.GetTaskId(),
		Mid:          appeal.GetMid(),
		Content:      appeal.GetContent(),
		FinalVerdict: int32(appeal.GetFinalVerdict()),
		FinalReason:  appeal.GetFinalReason(),
		Handler:      appeal.GetHandler(),
		Ctime:        appeal.GetCtime(),
		Mtime:        appeal.GetMtime(),
	}
	return &types.ModerationAppealResponse{
		Code:    0,
		Message: "ok",
		Data:    types.ModerationAppealData{Appeal: data},
		TTL:     0,
	}, nil
}
