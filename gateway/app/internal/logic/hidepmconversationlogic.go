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

type HidePmConversationLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 隐藏/恢复本方会话（不删除对方数据）
func NewHidePmConversationLogic(ctx context.Context, svcCtx *svc.ServiceContext) *HidePmConversationLogic {
	return &HidePmConversationLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// HidePmConversation 是用户侧可见性开关：只改本人成员行的 hide_state，
// 不删除消息、不影响对方（AGENTS.md §5：删除/下架要保留审计证据，隐藏不是删除）。
// EmptyReply 无业务字段，成功后返回统一空信封。
func (l *HidePmConversationLogic) HidePmConversation(req *types.ParamPmHideConversation) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.PrivateMessage == nil {
		return nil, errors.New("private-message service not configured")
	}
	if _, err := l.svcCtx.PrivateMessage.HideConversation(l.ctx, &privatemessagerpc.HideConversationReq{
		Mid:            req.Mid,
		ConversationId: req.ConversationId,
		Hide:           req.Hide,
		TraceId:        req.TraceId,
	}); err != nil {
		l.Errorf("gateway/app/hidePmConversation: mid=%d conversation_id=%d hide=%v err=%v",
			req.Mid, req.ConversationId, req.Hide, err)
		return nil, err
	}
	return emptyResponse(), nil
}
