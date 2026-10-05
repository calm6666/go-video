package logic

import (
	"context"

	"go-video/services/moderation-orchestrator/internal/svc"
	"go-video/services/moderation-orchestrator/model"
	"go-video/services/moderation-orchestrator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ProcessAppealLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewProcessAppealLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ProcessAppealLogic {
	return &ProcessAppealLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 处理申诉（运营）。
// 依据 AGENTS.md §8：申诉处理只能推进 APPEALED → APPEAL_DONE；
// 不能直接置 APPROVED/PUBLISHED，由内容所有者消费 moderation.result.v1 推进。
func (l *ProcessAppealLogic) ProcessAppeal(in *rpc.ProcessAppealReq) (*rpc.AppealReply, error) {
	if in.AppealId <= 0 {
		return nil, model.ErrInvalidAppealID
	}
	if in.Handler <= 0 {
		return nil, model.ErrInvalidHandler
	}
	if in.FinalVerdict == rpc.Verdict_VERDICT_UNSPECIFIED {
		return nil, model.ErrInvalidVerdict
	}
	if in.FinalReason == "" {
		return nil, model.ErrInvalidReason
	}

	if err := l.svcCtx.Repository.ProcessAppeal(l.ctx,
		in.AppealId, in.Handler, int32(in.FinalVerdict), in.FinalReason); err != nil {
		l.Errorf("moderation/ProcessAppeal: appeal=%d handler=%d err=%v",
			in.AppealId, in.Handler, err)
		return nil, err
	}
	// 回查最新申诉状态用于返回。
	a, err := l.svcCtx.Repository.GetAppeal(l.ctx, in.AppealId)
	if err != nil {
		return nil, err
	}
	return &rpc.AppealReply{Appeal: appealToRPC(a)}, nil
}
