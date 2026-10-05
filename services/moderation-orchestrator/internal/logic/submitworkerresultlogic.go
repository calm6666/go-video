package logic

import (
	"context"

	"go-video/services/moderation-orchestrator/internal/svc"
	"go-video/services/moderation-orchestrator/model"
	"go-video/services/moderation-orchestrator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SubmitWorkerResultLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSubmitWorkerResultLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitWorkerResultLogic {
	return &SubmitWorkerResultLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// worker 调用，回写识别结果（由 moderation-worker 调用）。
// 依据 AGENTS.md §8：worker 回调只能推进 PENDING/PROCESSING → DONE，
// 不能直接置 APPROVED/PUBLISHED；由内容所有者消费 moderation.result.v1 推进合法状态。
func (l *SubmitWorkerResultLogic) SubmitWorkerResult(in *rpc.WorkerResultReq) (*rpc.EmptyReply, error) {
	if in.TaskId <= 0 {
		return nil, model.ErrInvalidTaskID
	}
	if in.Verdict == rpc.Verdict_VERDICT_UNSPECIFIED {
		return nil, model.ErrInvalidVerdict
	}

	res := &model.ModerationResult{
		TaskID:   in.TaskId,
		Verdict:  int32(in.Verdict),
		Reason:   in.Reason,
		WorkerID: in.WorkerId,
	}
	if err := l.svcCtx.Repository.SubmitWorkerResult(l.ctx, res); err != nil {
		l.Errorf("moderation/SubmitWorkerResult: task=%d worker=%d verdict=%v err=%v",
			in.TaskId, in.WorkerId, in.Verdict, err)
		return nil, err
	}
	// TODO(event): 发布 moderation.result.v1 事件，由 video/catalog/comment/danmaku 消费推进状态机。
	l.Infof("moderation/SubmitWorkerResult: task=%d verdict=%v", in.TaskId, in.Verdict)
	return &rpc.EmptyReply{}, nil
}
