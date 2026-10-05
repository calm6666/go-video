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

type WithdrawMessageLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewWithdrawMessageLogic(ctx context.Context, svcCtx *svc.ServiceContext) *WithdrawMessageLogic {
	return &WithdrawMessageLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 撤回消息（只改可见性标记 + 写审计，绝不物理删行）。
//
// 授权与时间窗按 source 分支，缺一不可：
//   - SENDER：operator 必须是消息发送者本人，且落在 WithdrawWindowSeconds 内；
//   - RECEIVER：operator 必须是这条的接收方（本域只有全局状态位，见 README 缺口：
//     「只对接收方隐藏」的用户侧可见性由 hide_state/会话隐藏承担，逐条单边隐藏需要新表）；
//   - SENDER/RECEIVER 两条用户侧路径都再走一次成员证明（requireMembership），
//     消息行的 sender/receiver 快照不能替代成员表（快照会随数据修复而滞后）；
//   - MODERATION：必须带 audit_task_id（与 moderation 侧对账的凭据），缺失即 ErrWithdrawForbidden，
//     且这一步在 Messages.FindByID 之前完成（权限/凭据判定不得先取数据）；
//   - ADMIN：不受自助撤回窗口限制，但 operator_mid 必须是 gateway/admin 鉴权后的真实管理员，
//     且每一条都落 pm_withdraw_log —— 本服务不提供「无主撤回」，也不提供读正文的能力。
//
// 事务：MarkState → WithdrawLogs.Insert → DecrementUnreadIfUnread → RefreshPreview 同事务提交，
// 「改了状态必须有流水、投影必须同步」由这一步保证（AGENTS.md §8）。
// reason 只允许脱敏描述（命中规则、举报单号），禁止写入私信正文；日志同样只带主键。
func (l *WithdrawMessageLogic) WithdrawMessage(in *rpc.WithdrawMessageReq) (*rpc.WithdrawMessageReply, error) {
	ctx := l.ctx
	s := l.svcCtx

	msgID := in.GetMsgId()
	operator := in.GetOperatorMid()
	source := int32(in.GetSource())
	if err := checkMsgID(msgID); err != nil {
		return nil, err
	}
	if err := checkOperator(operator); err != nil {
		return nil, err
	}
	if !model.ValidWithdrawSource(source) {
		return nil, fmt.Errorf("%w: withdraw source=%d", model.ErrInvalidStateTransition, source)
	}
	reason, err := checkReasonText("reason", in.GetReason(), false)
	if err != nil {
		return nil, err
	}
	auditTaskID := in.GetAuditTaskId()
	// MODERATION 的对账凭据在取消息行之前就要成立：缺 audit_task_id 是「处置无证据」，
	// 与这条消息存不存在无关。放在 FindByID 之后等于「先取数据再判权限」——
	// 被拒的调用仍然把别人的 P4 行读进了内存。
	if source == model.WithdrawSourceModeration && auditTaskID <= 0 {
		return nil, fmt.Errorf("%w: source=MODERATION 必须带 audit_task_id", model.ErrWithdrawForbidden)
	}

	msg, err := s.Messages.FindByID(ctx, msgID)
	if err != nil {
		return nil, err
	}
	if msg == nil {
		return nil, model.ErrMessageNotFound
	}

	switch source {
	case model.WithdrawSourceSender:
		if msg.SenderMid != operator {
			return nil, model.ErrWithdrawForbidden
		}
		if _, err := requireMembership(ctx, s, msg.ConversationID, operator); err != nil {
			return nil, err
		}
		window := s.Config.PrivateMessage.WithdrawWindowSeconds
		if elapsed := timeNowUnix() - msg.Ctime; window <= 0 || elapsed > window {
			return nil, fmt.Errorf("%w: 已过 %d 秒（窗口 %d 秒）", model.ErrWithdrawWindowClosed, elapsed, window)
		}
	case model.WithdrawSourceReceiver:
		if msg.ReceiverMid != operator {
			return nil, model.ErrWithdrawForbidden
		}
		if _, err := requireMembership(ctx, s, msg.ConversationID, operator); err != nil {
			return nil, err
		}
		if reason == "" {
			reason = "receiver withdrew received message"
		}
	case model.WithdrawSourceModeration:
		// audit_task_id 已在取消息行之前校验过（见上），这里不再重复判断：
		// 同一件事在两个位置判断，就会出现「改了前面忘了后面」。
	case model.WithdrawSourceAdmin:
		// 运营处置：不受自助窗口限制，但必须留证（下面的流水）。
	default:
		return nil, fmt.Errorf("%w: withdraw source=%d", model.ErrInvalidStateTransition, source)
	}

	var (
		withdrawn bool
		state     = msg.State
		at        int64
	)
	err = s.DB.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		w, st, effectiveAt, err := withdrawInTx(tctx, s, session, withdrawRequest{
			msg:         msg,
			source:      source,
			operator:    operator,
			reason:      reason,
			auditTaskID: auditTaskID,
		})
		withdrawn, state, at = w, st, effectiveAt
		return err
	})
	if err != nil {
		return nil, err
	}
	if withdrawn {
		invalidateUnreadCache(ctx, s, l.Logger, msg.SenderMid, msg.ReceiverMid)
	}
	return &rpc.WithdrawMessageReply{
		MsgId:        msgID,
		State:        state,
		Withdrawn:    withdrawn,
		WithdrawTime: at,
	}, nil
}
