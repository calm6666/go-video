package logic

import (
	"context"

	"go-video/services/private-message/internal/svc"
	"go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type HideConversationLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewHideConversationLogic(ctx context.Context, svcCtx *svc.ServiceContext) *HideConversationLogic {
	return &HideConversationLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 本方隐藏/恢复会话。
//
// 授权：Members.SetHidden 的 WHERE 就是 (conversation_id, mid) —— 只能改自己这一行的
// hide_state，命中 0 行即 ErrNotConversationMember（不是成员改不了本方投影），
// 因此越权在这一条 UPDATE 内被证明并拒绝，不需要额外的读请求。
//
// 语义边界（软删除口径）：隐藏是「用户侧列表投影」——
// 不删除消息、不改会话主体状态、不影响对方可见性、不清空未读真值（恢复后未读仍在），
// 也不触发风控或审核事件。重复隐藏/恢复结果相同（单行状态位写入天然幂等）。
// 不做级联：对方再次发消息时 Members.ApplyIncoming 会把本方 hide_state 复位
// （新消息必须能被看到），这是模型层既有语义。
func (l *HideConversationLogic) HideConversation(in *rpc.HideConversationReq) (*rpc.EmptyReply, error) {
	mid := in.GetMid()
	convID := in.GetConversationId()
	if err := checkConversationID(convID); err != nil {
		return nil, err
	}
	if err := checkMid(mid); err != nil {
		return nil, err
	}
	if err := l.svcCtx.Members.SetHidden(l.ctx, convID, mid, in.GetHide()); err != nil {
		return nil, err
	}
	// 隐藏/恢复改变未读汇总口径（SumUnread 默认排除隐藏行），因此失效角标缓存。
	invalidateUnreadCache(l.ctx, l.svcCtx, l.Logger, mid)
	return &rpc.EmptyReply{}, nil
}
