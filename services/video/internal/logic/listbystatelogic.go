package logic

import (
	"context"

	"go-video/services/video/internal/svc"
	"go-video/services/video/model"
	"go-video/services/video/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListByStateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListByStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListByStateLogic {
	return &ListByStateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListByState 按状态查询稿件（运营/系统用）。
func (l *ListByStateLogic) ListByState(in *rpc.ListByStateReq) (*rpc.SubmissionsReply, error) {
	if in.Ps < 1 || in.Ps > 50 {
		return nil, model.ErrPsTooLarge
	}
	state := stateFromRPC(in.State)
	if state == 0 {
		return nil, model.ErrInvalidTargetState
	}
	rows, total, err := l.svcCtx.Repository.ListByState(l.ctx, state, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("video/ListByState: state=%d err=%v", state, err)
		return nil, err
	}
	out := make([]*rpc.Submission, 0, len(rows))
	for _, r := range rows {
		out = append(out, submissionModelToRPC(r))
	}
	return &rpc.SubmissionsReply{Total: total, Submissions: out}, nil
}
