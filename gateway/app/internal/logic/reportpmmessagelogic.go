// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	privatemessagerpc "go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ReportPmMessageLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 举报私信（写举报事实并向 moderation 送审）
func NewReportPmMessageLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportPmMessageLogic {
	return &ReportPmMessageLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ReportPmMessage 网关只记录「谁举报了哪条消息、原因码是什么」这一事实，
// 不判断是否违规、不代替 moderation-orchestrator 给结论（AGENTS.md §5：审核结论唯一所有者）。
// reason 是稳定原因码（端与网关约定），description 为补充说明，两者都不写正文。
func (l *ReportPmMessageLogic) ReportPmMessage(req *types.ParamPmReport) (resp *types.PmReportResponse, err error) {
	if l.svcCtx.PrivateMessage == nil {
		return nil, errors.New("private-message service not configured")
	}
	reply, err := l.svcCtx.PrivateMessage.ReportMessage(l.ctx, &privatemessagerpc.ReportMessageReq{
		MsgId:       req.MsgId,
		ReporterMid: req.ReporterMid,
		Reason:      req.Reason,
		Description: req.Description,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/app/reportPmMessage: msg_id=%d reporter_mid=%d reason=%d err=%v",
			req.MsgId, req.ReporterMid, req.Reason, err)
		return nil, err
	}
	return &types.PmReportResponse{
		Code:    0,
		Message: "ok",
		Data: types.PmReportData{
			ReportId:    reply.GetReportId(),
			Duplicated:  reply.GetDuplicated(),
			AuditTaskId: reply.GetAuditTaskId(),
		},
		TTL: 0,
	}, nil
}
