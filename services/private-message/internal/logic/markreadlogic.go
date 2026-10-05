package logic

import (
	"context"

	"go-video/services/private-message/internal/svc"
	"go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MarkReadLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewMarkReadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MarkReadLogic {
	return &MarkReadLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 前移已读游标（幂等，只前进不回退）。
//
// 授权：Members.Find(conversation_id, mid) 必须命中本人才有资格动这一行游标——
// 越权前移别人的游标，实际危害是「对方的未读角标被别人抹掉」，因此先判成员再写。
//
// 幂等与上限：
//   - read_seq 超过会话 last_seq 时按 last_seq 截断（客户端伪造未来游标不能把未读清零）；
//   - 目标值不大于当前游标时不写库，直接回当前值且 changed=false（重复请求与回退请求同一处理）；
//   - MarkRead 在单条 UPDATE 内同时前移游标并清零未读，天然无跨表事务需求；
//     applied=false（并发抢先或本次是回退）按幂等成功处理。
//
// 语义边界：已读是「每会话每成员一行游标」，不是逐条回执表（容量取舍见 README），
// 因此本方法不产生消息级事件，也不向对方推送「已读」。
func (l *MarkReadLogic) MarkRead(in *rpc.MarkReadReq) (*rpc.MarkReadReply, error) {
	ctx := l.ctx
	s := l.svcCtx

	mid := in.GetMid()
	ms, err := requireMembership(ctx, s, in.GetConversationId(), mid)
	if err != nil {
		return nil, err
	}
	target := in.GetReadSeq()
	if target < 0 {
		target = 0
	}
	if target > ms.member.LastSeq {
		target = ms.member.LastSeq
	}
	if target <= ms.member.ReadSeq {
		// 回退/重复：不写库，也不失效缓存（投影未变）。
		return &rpc.MarkReadReply{
			ReadSeq:     ms.member.ReadSeq,
			Changed:     false,
			UnreadCount: ms.member.Unread(),
		}, nil
	}

	applied, current, err := s.Members.MarkRead(ctx, ms.conversationID(), mid, target)
	if err != nil {
		return nil, err
	}
	if current == nil {
		// MarkRead 已写成功但回查不到成员行：并发删除/脏数据，按未生效返回，不伪造游标。
		return nil, notMember(ms.conversationID(), mid)
	}
	if applied {
		invalidateUnreadCache(ctx, s, l.Logger, mid)
	}
	effective := target
	if !applied {
		effective = current.ReadSeq
	}
	return &rpc.MarkReadReply{
		ReadSeq:     effective,
		Changed:     applied,
		UnreadCount: current.Unread(),
	}, nil
}
