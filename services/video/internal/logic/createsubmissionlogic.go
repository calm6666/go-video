package logic

import (
	"context"
	"time"

	"go-video/services/video/internal/svc"
	"go-video/services/video/model"
	"go-video/services/video/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateSubmissionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateSubmissionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateSubmissionLogic {
	return &CreateSubmissionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CreateSubmission 创建稿件（DRAFT 状态），返回新稿件。
func (l *CreateSubmissionLogic) CreateSubmission(in *rpc.CreateSubmissionReq) (*rpc.SubmissionReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.Title == "" {
		return nil, model.ErrInvalidTitle
	}
	if in.Typeid <= 0 {
		return nil, model.ErrInvalidTypeid
	}
	now := time.Now().Unix()
	sub := &model.VideoSubmission{
		Mid:    in.Mid,
		Title:  in.Title,
		Desc:   in.Desc,
		Cover:  in.Cover,
		Typeid: in.Typeid,
		Tag:    in.Tag,
		State:  model.StateDraft,
		Ctime:  now,
		Mtime:  now,
	}
	aid, err := l.svcCtx.Repository.CreateSubmission(l.ctx, sub)
	if err != nil {
		l.Errorf("video/CreateSubmission: mid=%d typeid=%d err=%v", in.Mid, in.Typeid, err)
		return nil, err
	}
	sub.Aid = aid
	return &rpc.SubmissionReply{Submission: submissionModelToRPC(sub)}, nil
}
