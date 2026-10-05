package logic

import (
	"context"

	"go-video/services/video/internal/svc"
	"go-video/services/video/model"
	"go-video/services/video/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetSubmissionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetSubmissionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetSubmissionLogic {
	return &GetSubmissionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetSubmission 查询稿件详情。
func (l *GetSubmissionLogic) GetSubmission(in *rpc.SubmissionReq) (*rpc.SubmissionReply, error) {
	if in.Aid <= 0 {
		return nil, model.ErrInvalidAid
	}
	sub, err := l.svcCtx.Repository.GetSubmission(l.ctx, in.Aid)
	if err != nil {
		l.Errorf("video/GetSubmission: aid=%d err=%v", in.Aid, err)
		return nil, err
	}
	if sub == nil {
		return nil, model.ErrSubmissionNotFound
	}
	return &rpc.SubmissionReply{Submission: submissionModelToRPC(sub)}, nil
}
