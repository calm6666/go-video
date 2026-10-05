package logic

import (
	"context"

	"go-video/services/private-message/internal/svc"
	"go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListMessagesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListMessagesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListMessagesLogic {
	return &ListMessagesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 会话内消息分页（conversation_id + seq 游标，禁止 offset 全表扫）。
//
// 授权与隐私（本方法最重要的两条）：
//  1. 参与者授权先于任何解密：Members.Find(conversation_id, mid) 不存在即 ErrNotConversationMember，
//     「不是成员」与「会话不存在」同一口径，避免用错误码枚举会话 ID；
//  2. 明文只在「成员身份 + 状态可见」两道门禁之后出现：PENDING_REVIEW 仅发送者可见，
//     WITHDRAWN/REJECTED/DELETED 与留存到期行返回固定占位文案（且隐去 media_ref），
//     解密失败统一成 ErrDecryptFailed（与「无权限」同口径，不给侧信道探测留缝）。
//
// 其余口径：ps 落配置上下限（超过 MaxPageSize 拒绝，禁无界扫描）；游标 cursor_seq<=0 表示从最新开始，
// 翻页位点取本页最小 seq（到底为 0）；本方法只读，前移已读游标只能走 MarkRead。
func (l *ListMessagesLogic) ListMessages(in *rpc.ListMessagesReq) (*rpc.ListMessagesReply, error) {
	ctx := l.ctx
	s := l.svcCtx
	cfg := s.Config.PrivateMessage

	viewer := in.GetMid()
	ms, err := requireMembership(ctx, s, in.GetConversationId(), viewer)
	if err != nil {
		return nil, err
	}
	ps, err := clampPageSize(in.GetPs(), cfg.PageSize, cfg.MaxPageSize)
	if err != nil {
		return nil, err
	}
	// 游标负值归零而不是报错：翻页抖动（客户端把 0 当游标回传）不该让整个会话页打不开。
	cursorSeq := in.GetCursorSeq()
	if cursorSeq < 0 {
		cursorSeq = 0
	}

	// 反骚扰：拉黑关系命中时这一页按「没有可见消息」处理，而不是留一行行空占位。
	blocked, err := peerBlockedForRead(ctx, s, viewer, ms.peer)
	if err != nil {
		return nil, err
	}
	if blocked {
		l.Infof("private-message/logic: 拉黑关系命中，消息列表按空返回 conversation_id=%d mid=%d",
			ms.conversationID(), viewer)
		return &rpc.ListMessagesReply{
			List:    []*rpc.MessageInfo{},
			ReadSeq: ms.readSeq(),
		}, nil
	}

	rows, err := s.Messages.ListBeforeSeq(ctx, ms.conversationID(), cursorSeq, ps+1)
	if err != nil {
		return nil, err
	}
	hasMore := len(rows) > int(ps)
	if hasMore {
		rows = rows[:ps]
	}

	list := make([]*rpc.MessageInfo, 0, len(rows))
	for _, row := range rows {
		info, err := projectMessage(s, row, viewer)
		if err != nil {
			// 解密失败不降级成「返回空正文」：那会让用户以为对方发了空消息。
			return nil, err
		}
		if info == nil {
			continue
		}
		list = append(list, info)
	}

	// 游标始终按「取回的行」推进，而不是按「留下的行」：
	// 全部被过滤时若游标不前移，客户端会在同一页上无限重试。
	nextCursor := int64(0)
	if hasMore && len(rows) > 0 {
		nextCursor = rows[len(rows)-1].Seq
	}
	return &rpc.ListMessagesReply{
		List:          list,
		NextCursorSeq: nextCursor,
		HasMore:       hasMore,
		ReadSeq:       ms.readSeq(),
	}, nil
}
