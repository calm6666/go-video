package logic

import (
	"context"
	"errors"
	"time"

	"go-video/services/comment/internal/svc"
	"go-video/services/comment/model"
	"go-video/services/comment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ReportCommentLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReportCommentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportCommentLogic {
	return &ReportCommentLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ReportComment 举报评论。
// 仅记录举报事实，不直接修改评论状态（由 moderation-orchestrator 审核结论回调推进）。
func (l *ReportCommentLogic) ReportComment(in *rpc.ReportCommentReq) (*rpc.EmptyReply, error) {
	if in.Rpid <= 0 {
		return nil, errors.New("comment: invalid rpid")
	}
	if in.ReporterMid <= 0 {
		return nil, model.ErrInvalidMid
	}
	rpt := &model.CommentReport{
		Rpid:        in.Rpid,
		ReporterMid: in.ReporterMid,
		Reason:      in.Reason,
		Content:     in.Content,
		TraceID:     in.TraceId,
		Ctime:       time.Now().Unix(),
	}
	if _, err := l.svcCtx.Repository.ReportComment(l.ctx, rpt); err != nil {
		l.Errorf("comment/ReportComment: rpid=%d reporter=%d err=%v", in.Rpid, in.ReporterMid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
