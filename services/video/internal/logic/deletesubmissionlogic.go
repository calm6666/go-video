package logic

import (
	"context"

	"go-video/services/video/internal/svc"
	"go-video/services/video/model"
	"go-video/services/video/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteSubmissionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeleteSubmissionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteSubmissionLogic {
	return &DeleteSubmissionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// DeleteSubmission 删除稿件（状态流转到 DELETED，保留审计）。
// 依据 AGENTS.md §8，删除是状态流转，不是物理删除。
func (l *DeleteSubmissionLogic) DeleteSubmission(in *rpc.SubmissionReq) (*rpc.EmptyReply, error) {
	if in.Aid <= 0 {
		return nil, model.ErrInvalidAid
	}
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	sub, err := l.svcCtx.Repository.GetSubmission(l.ctx, in.Aid)
	if err != nil {
		l.Errorf("video/DeleteSubmission Get: aid=%d err=%v", in.Aid, err)
		return nil, err
	}
	if sub == nil {
		return nil, model.ErrSubmissionNotFound
	}
	if sub.Mid != in.Mid {
		return nil, model.ErrNotOwner
	}
	if sub.State == model.StateDeleted {
		return &rpc.EmptyReply{}, nil
	}
	if !canTransition(sub.State, model.StateDeleted) {
		return nil, model.ErrInvalidStateTransition
	}
	if err := l.svcCtx.Repository.TransitionState(l.ctx, in.Aid, sub.State, model.StateDeleted, "owner:"+itoa(in.Mid), "user delete submission"); err != nil {
		l.Errorf("video/DeleteSubmission Transition: aid=%d err=%v", in.Aid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
