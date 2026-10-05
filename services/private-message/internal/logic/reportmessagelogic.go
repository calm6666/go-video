package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/private-message/internal/svc"
	"go-video/services/private-message/model"
	"go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ReportMessageLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReportMessageLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportMessageLogic {
	return &ReportMessageLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 举报私信（写本域举报事实并向 moderation 送审）。
//
// 实现要点：
//  1. 越权防护：举报必须先证明举报者是该消息所属会话的成员（Members.Find），
//     否则任何拿到 msg_id 的人都能刷举报单（ErrNotConversationMember 在任何写入之前返回）；
//     本方法不回读、不外传正文，举报响应里没有任何内容字段；
//  2. target_mid 取消息发送者，conversation_id 作为冗余快照写入（运营侧免 JOIN）；
//  3. 幂等：uniq_msg_reporter(msg_id, reporter_mid) 命中时 created=false，回已有单号且 duplicated=true，
//     同一举报人对同一消息不叠加；
//  4. 送审：新举报通过 ModerationClient() 提交拿 task_id 后 BindAuditTask；
//     未配置或失败时保留举报行、audit_task_id=0 并回 0（禁止伪造「已送审」，
//     也禁止回滚用户已提交的举报——举报是用户主张，不是合规结论）；
//  5. 状态固定 ReportStatePending：本方法不写结论，结论唯一入口是 ApplyModerationVerdict；
//  6. 不改动被举报消息的可见性：举报只是「有人这么认为」，处置由 HandleReport 决定；
//  7. description 按列上限截断（举报说明是补充材料，超长截断而不是丢掉整笔举报），
//     日志只带主键与原因码。
func (l *ReportMessageLogic) ReportMessage(in *rpc.ReportMessageReq) (*rpc.ReportMessageReply, error) {
	ctx := l.ctx
	s := l.svcCtx

	reporter := in.GetReporterMid()
	msgID := in.GetMsgId()
	if err := checkMsgID(msgID); err != nil {
		return nil, err
	}
	if err := checkMid(reporter); err != nil {
		return nil, err
	}
	reason := in.GetReason()
	if reason <= 0 {
		return nil, fmt.Errorf("%w: 举报原因码 reason=%d", model.ErrInvalidReportAction, reason)
	}
	description := truncateRunes(strings.TrimSpace(in.GetDescription()), maxDescriptionRunes)

	msg, err := s.Messages.FindByID(ctx, msgID)
	if err != nil {
		return nil, err
	}
	if msg == nil {
		return nil, model.ErrMessageNotFound
	}
	if _, err := requireMembership(ctx, s, msg.ConversationID, reporter); err != nil {
		return nil, err
	}

	reportID, created, err := s.Reports.Insert(ctx, &model.Report{
		ConversationID: msg.ConversationID,
		MsgID:          msg.MsgID,
		ReporterMid:    reporter,
		TargetMid:      msg.SenderMid,
		Reason:         reason,
		Description:    description,
		State:          model.ReportStatePending,
		TraceID:        sanitizeTraceID(in.GetTraceId()),
	})
	if err != nil {
		return nil, err
	}
	if !created {
		// 重复举报：回首次那条的送审进度，不重复送审（moderation 侧 submission_id 会重复登记）。
		cur, err := s.Reports.FindByID(ctx, reportID)
		if err != nil {
			return nil, err
		}
		var taskID int64
		if cur != nil {
			taskID = cur.AuditTaskID
		}
		return &rpc.ReportMessageReply{ReportId: reportID, Duplicated: true, AuditTaskId: taskID}, nil
	}

	taskID, err := submitForModeration(ctx, s, reportID, reporter, "private message report")
	if err != nil {
		// 举报行保留、audit_task_id=0：由重试/cron 补送，禁止把「没送出去」说成「已送审」。
		l.Errorf("private-message/logic: 举报送审失败，保留举报单待补送 report_id=%d msg_id=%d reporter_mid=%d: %v",
			reportID, msgID, reporter, err)
		return &rpc.ReportMessageReply{ReportId: reportID, Duplicated: false, AuditTaskId: 0}, nil
	}
	if err := s.Reports.BindAuditTask(ctx, reportID, taskID); err != nil {
		l.Errorf("private-message/logic: 回写举报审核任务失败 report_id=%d task_id=%d: %v", reportID, taskID, err)
		return &rpc.ReportMessageReply{ReportId: reportID, Duplicated: false, AuditTaskId: 0}, nil
	}
	return &rpc.ReportMessageReply{ReportId: reportID, Duplicated: false, AuditTaskId: taskID}, nil
}
