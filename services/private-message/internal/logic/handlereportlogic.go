package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/services/private-message/internal/svc"
	"go-video/services/private-message/model"
	"go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type HandleReportLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewHandleReportLogic(ctx context.Context, svcCtx *svc.ServiceContext) *HandleReportLogic {
	return &HandleReportLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 运营侧举报处置（幂等键防重复处置）。
//
// 实现要点：
//  1. 参数：report_id/handler 必填（handler<=0 即无主处置，不可受理；管理员鉴权在 gateway/admin），
//     action 必须是已开放的 REPORT_ACTION_*，idempotency_key 非空且 <=128；
//     note 是审计文本，禁止粘贴私信正文；
//  2. 幂等：先按 handle_idempotency_key 回放首次结果（replayed=true，不重复处置、不重复撤回）；
//  3. 动作映射：DISMISS→Dismissed；WITHDRAW/PUNISH/ESCALATE→Handled；
//     ESCALATE 必须先拿到 moderation 任务再改状态（未配置返回 ErrModerationNotConfigured，
//     不得把「没送出去」的举报标成已处理）；PUNISH 只记录「已转交 risk-control」，
//     处罚结论不落本库（数据所有者是 risk-control，AGENTS.md §5）；
//  4. 事务：MarkHandledInTx(CAS: 当前态必须为 PENDING) 与连带撤回同事务提交，
//     否则会出现「举报已处理但违规消息仍可见」的空档；
//  5. 撤回复用 withdrawInTx（source=ADMIN、report_id 关联），不受自助撤回时间窗限制但必写流水；
//  6. 被撤回的消息只取举报单里的 msg_id 快照，不接受调用方另传消息 ID（防止「处置 A 却撤掉 B」）；
//  7. CAS 失败回滚后按幂等键回放；回放也拿不到就说明被他人抢先处置，返回 ErrConcurrentUpdate 让调用方重查。
func (l *HandleReportLogic) HandleReport(in *rpc.HandleReportReq) (*rpc.HandleReportReply, error) {
	ctx := l.ctx
	s := l.svcCtx

	reportID := in.GetReportId()
	handler := in.GetHandler()
	action := in.GetAction()
	if reportID <= 0 {
		return nil, model.ErrReportNotFound
	}
	if err := checkOperator(handler); err != nil {
		return nil, err
	}
	key, err := checkIdempotencyKey(in.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	note, err := checkReasonText("note", in.GetNote(), false)
	if err != nil {
		return nil, err
	}

	var targetState int32
	switch action {
	case rpc.ReportAction_REPORT_ACTION_DISMISS:
		targetState = model.ReportStateDismissed
	case rpc.ReportAction_REPORT_ACTION_WITHDRAW,
		rpc.ReportAction_REPORT_ACTION_PUNISH,
		rpc.ReportAction_REPORT_ACTION_ESCALATE:
		targetState = model.ReportStateHandled
	default:
		return nil, fmt.Errorf("%w: action=%d", model.ErrInvalidReportAction, action)
	}

	// --- 幂等回放 ---
	if prev, err := s.Reports.FindByHandleKey(ctx, key); err != nil {
		return nil, err
	} else if prev != nil {
		return l.replayHandled(ctx, prev, "同一 idempotency_key 的重复处置")
	}

	rep, err := s.Reports.FindByID(ctx, reportID)
	if err != nil {
		return nil, err
	}
	if rep == nil {
		return nil, model.ErrReportNotFound
	}
	if rep.State != model.ReportStatePending {
		if rep.HandleIdempotencyKey.Valid && rep.HandleIdempotencyKey.String == key {
			return l.replayHandled(ctx, rep, "本键已处置")
		}
		return nil, fmt.Errorf("%w: 举报单当前态 %d 已由他人处置", model.ErrConcurrentUpdate, rep.State)
	}

	// --- ESCALATE 先送审：拿不到任务就不改状态 ---
	var escalateTaskID int64
	if action == rpc.ReportAction_REPORT_ACTION_ESCALATE {
		taskID, serr := submitForModeration(ctx, s, reportID, rep.TargetMid, "report escalation")
		if serr != nil {
			// 未配置（ErrModerationNotConfigured）与调用失败都原样返回：
			// 升级人审没落地，举报单必须仍是待处理。
			return nil, serr
		}
		escalateTaskID = taskID
	}

	reason := note
	if reason == "" {
		reason = fmt.Sprintf("report %d action %d", reportID, action)
	}
	reason = truncateRunes(reason, maxReasonRunes)

	var (
		withdrawMsgID int64
		committed     int32 = targetState
	)
	err = s.DB.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		if err := s.Reports.MarkHandledInTx(tctx, session, reportID, targetState, handler,
			truncateRunes(note, maxNoteRunes), key); err != nil {
			return err
		}
		if !in.GetWithdrawMessage() || action == rpc.ReportAction_REPORT_ACTION_DISMISS {
			return nil
		}
		msg, err := s.Messages.FindByID(tctx, rep.MsgID)
		if err != nil {
			return err
		}
		if msg == nil {
			// 举报单指向的消息不存在：处置照旧成立，撤回这一步没有对象，
			// 回滚整笔避免「标记已处理但什么都没做」的假象。
			return model.ErrMessageNotFound
		}
		w, _, _, err := withdrawInTx(tctx, s, session, withdrawRequest{
			msg:         msg,
			source:      model.WithdrawSourceAdmin,
			operator:    handler,
			reason:      reason,
			auditTaskID: rep.AuditTaskID,
			reportID:    reportID,
		})
		if err != nil {
			return err
		}
		if w {
			withdrawMsgID = msg.MsgID
		}
		return nil
	})
	if errors.Is(err, model.ErrConcurrentUpdate) {
		if prev, perr := s.Reports.FindByHandleKey(ctx, key); perr != nil {
			return nil, perr
		} else if prev != nil {
			return l.replayHandled(ctx, prev, "并发处置后按幂等键回放")
		}
		return nil, err
	}
	if err != nil {
		return nil, err
	}

	if escalateTaskID > 0 {
		if berr := s.Reports.BindAuditTask(ctx, reportID, escalateTaskID); berr != nil {
			// 状态已改、任务号未回写：举报仍属「已处理」，任务号由对账补齐，
			// 这里只记日志（不回滚已提交的处置，也不谎报成功）。
			l.Errorf("private-message/logic: 回写升级审核任务失败 report_id=%d task_id=%d: %v",
				reportID, escalateTaskID, berr)
		}
	}
	invalidateUnreadCache(ctx, s, l.Logger, rep.ReporterMid, rep.TargetMid)
	return &rpc.HandleReportReply{
		ReportId:      reportID,
		State:         committed,
		Replayed:      false,
		WithdrawMsgId: withdrawMsgID,
	}, nil
}

// replayHandled 用已处置的举报单构造回放响应。
// withdraw_msg_id 从「该行现在是否已撤回」反推：撤回状态是本域事实，重放不需要额外幂等表。
func (l *HandleReportLogic) replayHandled(ctx context.Context, rep *model.Report, why string) (*rpc.HandleReportReply, error) {
	if rep == nil {
		return nil, model.ErrReportNotFound
	}
	var withdrawnMsgID int64
	if rep.MsgID > 0 {
		msg, err := l.svcCtx.Messages.FindByID(ctx, rep.MsgID)
		if err != nil {
			return nil, err
		}
		if msg != nil && msg.State == model.MsgStateWithdrawn {
			withdrawnMsgID = msg.MsgID
		}
	}
	state := int32(model.ReportStateHandled)
	if rep != nil {
		state = rep.State
	}
	l.Infof("private-message/logic: 举报处置幂等回放 report_id=%d（%s）", rep.ReportID, why)
	return &rpc.HandleReportReply{
		ReportId:      rep.ReportID,
		State:         state,
		Replayed:      true,
		WithdrawMsgId: withdrawnMsgID,
	}, nil
}
