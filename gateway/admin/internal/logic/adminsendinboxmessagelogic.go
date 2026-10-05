// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	inboxrpc "go-video/services/inbox/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminSendInboxMessageLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 下发系统/运营站内信（同事务写主体与收件行，idempotency_key 幂等）
func NewAdminSendInboxMessageLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminSendInboxMessageLogic {
	return &AdminSendInboxMessageLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *AdminSendInboxMessageLogic) AdminSendInboxMessage(req *types.ParamAdminSendInboxMessage) (resp *types.AdminSendInboxMessageResponse, err error) {
	if l.svcCtx.Inbox == nil {
		return nil, errors.New("inbox service not configured")
	}
	// 只校验主体存在 + 会话已鉴权；接收人去重、人数上限、分类/载体默认值全部由 inbox 服务判定，网关不复算。
	if err := adminSubjectGate(l.ctx, "adminSendInboxMessage", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	if len(req.Mids) == 0 {
		return nil, errors.New("gateway/admin: mids required")
	}
	if err := requireNonEmpty("title", req.Title); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("content", req.Content); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.Inbox.SendSystemMessage(l.ctx, &inboxrpc.SendSystemMessageReq{
		Mids:           req.Mids,
		Title:          req.Title,
		Content:        req.Content,
		Category:       inboxrpc.Category(req.Category),
		MsgType:        inboxrpc.MsgType(req.MsgType),
		SenderMid:      req.SenderMid,
		BizType:        req.BizType,
		BizId:          req.BizId,
		Extra:          req.Extra,
		IdempotencyKey: req.IdempotencyKey,
		Operator:       req.OperatorMid,
		TraceId:        req.TraceId,
	})
	if err != nil {
		// 正文属用户内容，不进日志；只记可定位投递的身份与幂等键。
		l.Errorf("gateway/admin/adminSendInboxMessage: operator=%d recipients=%d key=%s err=%v",
			req.OperatorMid, len(req.Mids), req.IdempotencyKey, err)
		return nil, err
	}
	return &types.AdminSendInboxMessageResponse{
		Code:    0,
		Message: "ok",
		Data: types.AdminSendInboxMessageData{
			MsgId:        reply.GetMsgId(),
			Delivered:    reply.GetDelivered(),
			Deduplicated: reply.GetDeduplicated(),
			Ctime:        reply.GetCtime(),
		},
		TTL: 0,
	}, nil
}
