// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	commentrpc "go-video/services/comment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ReportCommentLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 举报评论（进入 moderation 待审队列）
func NewReportCommentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportCommentLogic {
	return &ReportCommentLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ReportComment 聚合 comment ReportComment RPC：举报只写入待审队列，结论由 moderation 判定。
// 网关不判断举报理由是否成立，reason 与补充说明原样透传，trace_id 供审核链路关联。
func (l *ReportCommentLogic) ReportComment(req *types.ParamReportComment) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Comment == nil {
		return nil, errors.New("comment service not configured")
	}
	if _, err = l.svcCtx.Comment.ReportComment(l.ctx, &commentrpc.ReportCommentReq{
		Rpid:        req.Rpid,
		ReporterMid: req.ReporterMid,
		Reason:      req.Reason,
		Content:     req.Content,
		TraceId:     req.TraceId,
	}); err != nil {
		l.Errorf("gateway/app/reportComment: rpid=%d reporter_mid=%d reason=%d err=%v",
			req.Rpid, req.ReporterMid, req.Reason, err)
		return nil, err
	}
	return emptyResponse(), nil
}
