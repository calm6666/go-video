package logic

import (
	"context"
	"fmt"

	"go-video/services/private-message/internal/svc"
	"go-video/services/private-message/model"
	"go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type ApplyModerationVerdictLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewApplyModerationVerdictLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ApplyModerationVerdictLogic {
	return &ApplyModerationVerdictLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 审核结论回写（唯一写结论入口，按 event_id 去重）。
//
// 本方法是 moderation 侧的写入通道，不做参与者授权：它不读也不返回任何正文
// （迁移只依赖状态位与主键，审核链路因此拿不到 P4 明文），
// 业务方（含本服务）都没有把消息标成「已审核通过」的其它入口（AGENTS.md §8）。
//
// 但「不做参与者授权」不等于「不校验主体」：operator 必须是机审（proto 里的 0）
// 或 gateway/admin 鉴权过的真实管理员，负数与无主写请求都在取消息行之前就被拒。
//
// 去重优先于状态机：AuditEventID 与本次 event_id 相同即判重复投递，不产生第二次迁移。
// 合法迁移表（终态不可再改，命中不了返回 applied=false 而不是 error，投递重投天然幂等）：
//   - VERDICT_PASS  ：PENDING_REVIEW → NORMAL；
//   - VERDICT_REVIEW：保持 PENDING_REVIEW，只刷新 audit_task_id/audit_event_id（转人审是等待不是结论）；
//   - VERDICT_REJECT：{NORMAL, PENDING_REVIEW} → REJECTED，写 withdraw_time 与审计流水，
//     并同步修正未读投影与摘要占位。
//
// ErrConcurrentUpdate（CAS 竞争）不吞错：调用方按同一 event_id 重试仍然幂等。
func (l *ApplyModerationVerdictLogic) ApplyModerationVerdict(in *rpc.ApplyModerationVerdictReq) (*rpc.ApplyModerationVerdictReply, error) {
	ctx := l.ctx
	s := l.svcCtx

	msgID := in.GetMsgId()
	taskID := in.GetTaskId()
	verdict := in.GetVerdict()
	operator := in.GetOperator()
	if err := checkMsgID(msgID); err != nil {
		return nil, err
	}
	// 主体先于任何数据访问：0 是 proto 里机审的取值（ApplyModerationVerdictReq.operator
	// 「处理人（0 表示机审）」），负数与其余非正值一律按缺主体拒（checkSubjectOrOperator）。
	if err := checkSubjectOrOperator(operator); err != nil {
		return nil, err
	}
	if taskID <= 0 {
		return nil, fmt.Errorf("%w: task_id 必填", model.ErrInvalidVerdict)
	}
	eventID, err := checkEventID(in.GetEventId())
	if err != nil {
		return nil, err
	}
	// reason 只允许脱敏描述（命中词/模型分），审计列宽上限直接拒超长，不做截断。
	reason, err := checkReasonText("reason", in.GetReason(), false)
	if err != nil {
		return nil, err
	}
	switch verdict {
	case rpc.ModerationVerdict_VERDICT_PASS,
		rpc.ModerationVerdict_VERDICT_REVIEW,
		rpc.ModerationVerdict_VERDICT_REJECT:
	default:
		return nil, fmt.Errorf("%w: verdict=%d", model.ErrInvalidVerdict, verdict)
	}

	msg, err := s.Messages.FindByID(ctx, msgID)
	if err != nil {
		return nil, err
	}
	if msg == nil {
		return nil, model.ErrMessageNotFound
	}
	if msg.AuditEventID != "" && msg.AuditEventID == eventID {
		return &rpc.ApplyModerationVerdictReply{
			MsgId: msgID, State: msg.State, Applied: false,
			Message: "重复投递：该 event_id 的结论已应用",
		}, nil
	}

	switch verdict {
	case rpc.ModerationVerdict_VERDICT_PASS:
		return l.applyPass(ctx, msg, taskID, eventID)
	case rpc.ModerationVerdict_VERDICT_REVIEW:
		return l.applyReview(ctx, msg, taskID, eventID)
	default:
		return l.applyReject(ctx, msg, operator, taskID, eventID, reason)
	}
}

func (l *ApplyModerationVerdictLogic) applyPass(ctx context.Context, msg *model.Message, taskID int64,
	eventID string) (*rpc.ApplyModerationVerdictReply, error) {
	s := l.svcCtx
	if msg.State != model.MsgStatePendingReview {
		return &rpc.ApplyModerationVerdictReply{
			MsgId: msg.MsgID, State: msg.State, Applied: false,
			Message: fmt.Sprintf("当前态 %d 不是待审核，通过结论不生效", msg.State),
		}, nil
	}
	var applied bool
	if err := s.DB.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		var err error
		applied, err = s.Messages.MarkState(tctx, session, msg.MsgID,
			[]int32{model.MsgStatePendingReview}, model.MsgStateNormal, 0, eventID)
		return err
	}); err != nil {
		return nil, err
	}
	if !applied {
		return &rpc.ApplyModerationVerdictReply{
			MsgId: msg.MsgID, State: msg.State, Applied: false,
			Message: "并发下状态已推进，本次结论未生效",
		}, nil
	}
	// 通过后可见性变化：接收方角标与会话列表缓存需失效（真值仍在 MySQL）。
	invalidateUnreadCache(ctx, s, l.Logger, msg.SenderMid, msg.ReceiverMid)
	return &rpc.ApplyModerationVerdictReply{
		MsgId: msg.MsgID, State: model.MsgStateNormal, Applied: true, Message: "通过",
	}, nil
}

// applyReview 转人审：等待而非结论，因此只刷新送审登记，绝不改 state。
func (l *ApplyModerationVerdictLogic) applyReview(ctx context.Context, msg *model.Message, taskID int64,
	eventID string) (*rpc.ApplyModerationVerdictReply, error) {
	applied, err := l.svcCtx.Messages.BindAuditEvent(ctx, nil, msg.MsgID, taskID, eventID)
	if err != nil {
		return nil, err
	}
	if applied {
		return &rpc.ApplyModerationVerdictReply{
			MsgId: msg.MsgID, State: model.MsgStatePendingReview, Applied: true,
			Message: "转人审，保持待审核",
		}, nil
	}
	return &rpc.ApplyModerationVerdictReply{
		MsgId: msg.MsgID, State: msg.State, Applied: false,
		Message: fmt.Sprintf("当前态 %d 已落定，转人审登记未生效", msg.State),
	}, nil
}

// applyReject 驳回：改状态 + 写审计 + 修未读与摘要，四件事同一事务。
func (l *ApplyModerationVerdictLogic) applyReject(ctx context.Context, msg *model.Message, operator, taskID int64,
	eventID, reason string) (*rpc.ApplyModerationVerdictReply, error) {
	s := l.svcCtx
	if isTerminalMsgState(msg.State) {
		return &rpc.ApplyModerationVerdictReply{
			MsgId: msg.MsgID, State: msg.State, Applied: false,
			Message: fmt.Sprintf("%d 是终态，驳回结论不再改写", msg.State),
		}, nil
	}

	at := timeNowUnix()
	if reason == "" {
		reason = fmt.Sprintf("moderation task %d rejected", taskID)
	}
	var applied bool
	err := s.DB.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		ok, err := s.Messages.MarkState(tctx, session, msg.MsgID,
			[]int32{model.MsgStateNormal, model.MsgStatePendingReview}, model.MsgStateRejected, at, eventID)
		if err != nil || !ok {
			return err
		}
		applied = true
		// 驳回同样是处置动作，必须留证：source=MODERATION，operator=0 表示机审。
		if err := s.WithdrawLogs.Insert(tctx, session, &model.WithdrawLog{
			MsgID:          msg.MsgID,
			ConversationID: msg.ConversationID,
			Seq:            msg.Seq,
			SenderMid:      msg.SenderMid,
			OperatorMid:    operator,
			Source:         model.WithdrawSourceModeration,
			Reason:         reason,
			AuditTaskID:    taskID,
			Ctime:          at,
		}); err != nil {
			return err
		}
		if err := s.Members.DecrementUnreadIfUnread(tctx, session, msg.ConversationID, msg.Seq); err != nil {
			return err
		}
		return s.Members.RefreshPreview(tctx, session, msg.ConversationID, msg.MsgID, previewRejected)
	})
	if err != nil {
		return nil, err
	}
	if !applied {
		return &rpc.ApplyModerationVerdictReply{
			MsgId: msg.MsgID, State: msg.State, Applied: false,
			Message: "并发下状态已推进，驳回未生效",
		}, nil
	}
	invalidateUnreadCache(ctx, s, l.Logger, msg.SenderMid, msg.ReceiverMid)
	return &rpc.ApplyModerationVerdictReply{
		MsgId: msg.MsgID, State: model.MsgStateRejected, Applied: true, Message: "驳回",
	}, nil
}
