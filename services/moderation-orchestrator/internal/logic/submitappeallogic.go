package logic

import (
	"context"

	"go-video/services/moderation-orchestrator/internal/svc"
	"go-video/services/moderation-orchestrator/model"
	"go-video/services/moderation-orchestrator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SubmitAppealLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSubmitAppealLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitAppealLogic {
	return &SubmitAppealLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 提交申诉。
// 依据 AGENTS.md §8 状态机：申诉只能在任务 DONE 后发起；
// repository 会校验任务 DONE → APPEALED 的合法迁移。
func (l *SubmitAppealLogic) SubmitAppeal(in *rpc.AppealReq) (*rpc.AppealReply, error) {
	if in.TaskId <= 0 {
		return nil, model.ErrInvalidTaskID
	}
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.Content == "" {
		return nil, model.ErrInvalidContent
	}

	a := &model.ModerationAppeal{
		TaskID:  in.TaskId,
		Mid:     in.Mid,
		Content: in.Content,
	}
	appealID, err := l.svcCtx.Repository.SubmitAppeal(l.ctx, a)
	if err != nil {
		l.Errorf("moderation/SubmitAppeal: task=%d mid=%d err=%v", in.TaskId, in.Mid, err)
		return nil, err
	}
	a.ID = appealID
	return &rpc.AppealReply{Appeal: appealToRPC(a)}, nil
}
