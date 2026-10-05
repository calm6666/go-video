package logic

import (
	"context"

	"go-video/services/video/internal/svc"
	"go-video/services/video/model"
	"go-video/services/video/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateSubmissionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateSubmissionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateSubmissionLogic {
	return &UpdateSubmissionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// UpdateSubmission 更新稿件元信息（仅 DRAFT 可改）。
func (l *UpdateSubmissionLogic) UpdateSubmission(in *rpc.UpdateSubmissionReq) (*rpc.SubmissionReply, error) {
	if in.Aid <= 0 {
		return nil, model.ErrInvalidAid
	}
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	sub, err := l.svcCtx.Repository.GetSubmission(l.ctx, in.Aid)
	if err != nil {
		l.Errorf("video/UpdateSubmission Get: aid=%d err=%v", in.Aid, err)
		return nil, err
	}
	if sub == nil {
		return nil, model.ErrSubmissionNotFound
	}
	if sub.Mid != in.Mid {
		return nil, model.ErrNotOwner
	}
	if sub.State != model.StateDraft {
		return nil, model.ErrSubmissionNotDraft
	}
	title := in.Title
	if title == "" {
		title = sub.Title
	}
	desc := in.Desc
	if desc == "" {
		desc = sub.Desc
	}
	cover := in.Cover
	if cover == "" {
		cover = sub.Cover
	}
	typeid := in.Typeid
	if typeid == 0 {
		typeid = sub.Typeid
	}
	tag := in.Tag
	if tag == "" {
		tag = sub.Tag
	}
	if err := l.svcCtx.Repository.UpdateSubmissionFields(l.ctx, in.Aid, title, desc, cover, typeid, tag); err != nil {
		l.Errorf("video/UpdateSubmission: aid=%d err=%v", in.Aid, err)
		return nil, err
	}
	updated, err := l.svcCtx.Repository.GetSubmission(l.ctx, in.Aid)
	if err != nil {
		return nil, err
	}
	return &rpc.SubmissionReply{Submission: submissionModelToRPC(updated)}, nil
}
