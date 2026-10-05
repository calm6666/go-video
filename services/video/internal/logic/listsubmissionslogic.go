package logic

import (
	"context"

	"go-video/services/video/internal/svc"
	"go-video/services/video/model"
	"go-video/services/video/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListSubmissionsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListSubmissionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListSubmissionsLogic {
	return &ListSubmissionsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListSubmissions 分页查询稿件（按 mid 或 typeid 过滤）。
func (l *ListSubmissionsLogic) ListSubmissions(in *rpc.ListReq) (*rpc.SubmissionsReply, error) {
	if in.Ps < 1 || in.Ps > 50 {
		return nil, model.ErrPsTooLarge
	}
	rows, total, err := l.svcCtx.Repository.ListSubmissions(l.ctx, in.Mid, in.Typeid, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("video/ListSubmissions: mid=%d typeid=%d err=%v", in.Mid, in.Typeid, err)
		return nil, err
	}
	out := make([]*rpc.Submission, 0, len(rows))
	for _, r := range rows {
		out = append(out, submissionModelToRPC(r))
	}
	return &rpc.SubmissionsReply{Total: total, Submissions: out}, nil
}
