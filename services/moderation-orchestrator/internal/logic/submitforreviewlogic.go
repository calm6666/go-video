package logic

import (
	"context"

	"go-video/services/moderation-orchestrator/internal/svc"
	"go-video/services/moderation-orchestrator/model"
	"go-video/services/moderation-orchestrator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SubmitForReviewLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSubmitForReviewLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitForReviewLogic {
	return &SubmitForReviewLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 领域服务提交审核任务（创建 task 并入 MQ 待处理）。
// 依据 AGENTS.md §8：SubmitForReview 只接受未审核任务，
// 不能直接置 APPROVED/PUBLISHED。
func (l *SubmitForReviewLogic) SubmitForReview(in *rpc.SubmitReq) (*rpc.TaskReply, error) {
	if in.SubmissionId <= 0 {
		return nil, model.ErrInvalidSubmissionID
	}
	if in.ContentType == rpc.ContentType_CONTENT_TYPE_UNSPECIFIED {
		return nil, model.ErrInvalidContentType
	}
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.Business == "" {
		return nil, model.ErrInvalidBusiness
	}

	t := &model.ModerationTask{
		SubmissionID: in.SubmissionId,
		ContentType:  int32(in.ContentType),
		Mid:          in.Mid,
		UpMid:        in.UpMid,
		Business:     in.Business,
		Reason:       in.Reason,
	}
	taskID, err := l.svcCtx.Repository.SubmitForReview(l.ctx, t)
	if err != nil {
		l.Errorf("moderation/SubmitForReview: business=%s submission=%d mid=%d err=%v",
			in.Business, in.SubmissionId, in.Mid, err)
		return nil, err
	}
	// 当前实现占位：不实际调用 moderation-worker。
	// 后续接入 MQ 后由消费者将 task 派发给 worker，worker 完成后回调 SubmitWorkerResult。
	l.Infof("moderation/SubmitForReview: created task=%d business=%s submission=%d",
		taskID, in.Business, in.SubmissionId)
	return &rpc.TaskReply{Task: taskToRPC(t)}, nil
}
