package logic

import (
	"context"

	"go-video/services/private-message/internal/svc"
	"go-video/services/private-message/model"
	"go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListConversationsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListConversationsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListConversationsLogic {
	return &ListConversationsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 会话列表（cursor 分页，黑名单/风控/隐藏会话在查询层过滤）。
//
// 实现要点：
//  1. 越权边界：只按登录者自己的 mid 扫成员表（idx_mid_list），不 JOIN 也不读别人的成员行；
//  2. 页大小取自服务端配置，超过 MaxPageSize 直接拒绝，禁止无界扫描与 offset 深翻页；
//  3. 游标是上一页末条的 (last_msg_time, id) 双列位点，同秒多条也能稳定续翻；
//  4. 查询侧过滤与发送侧同一套门禁口径（黑名单真值在 social-graph）：
//     social-graph 未配置返回 ErrSocialGraphNotConfigured 而不是放行整页；
//     下游抖动时该行按「命中」剔除（宁可少给一条，不越权给别人的会话）；
//  5. 会话主体行缺失的成员行直接丢弃（脏投影不外露）；
//  6. last_preview 只取成员投影列的脱敏摘要，任何情况下都不回表读正文（隐私级别 P4）；
//  7. 本方法只读不写，未读投影漂移由 RebuildProjection（运营/cron 入口）修复。
func (l *ListConversationsLogic) ListConversations(in *rpc.ListConversationsReq) (*rpc.ListConversationsReply, error) {
	ctx := l.ctx
	s := l.svcCtx
	cfg := s.Config.PrivateMessage

	mid := in.GetMid()
	if err := checkMid(mid); err != nil {
		return nil, err
	}
	ps, err := clampPageSize(in.GetPs(), cfg.PageSize, cfg.MaxPageSize)
	if err != nil {
		return nil, err
	}
	cursorTime, cursorID, err := decodeTimeIDCursor(in.GetCursor())
	if err != nil {
		return nil, err
	}

	// 多取一条判 has_more：省掉一次 COUNT(*)，也不要把整页拉进内存再截。
	rows, err := s.Members.ListByMid(ctx, model.ListMembersOptions{
		Mid:           mid,
		CursorTime:    cursorTime,
		CursorID:      cursorID,
		PageSize:      ps + 1,
		OnlyUnread:    in.GetOnlyUnread(),
		IncludeHidden: in.GetIncludeHidden(),
	})
	if err != nil {
		return nil, err
	}
	hasMore := len(rows) > int(ps)
	if hasMore {
		rows = rows[:ps]
	}

	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ConversationID)
	}
	convs, err := s.Conversations.FindByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}

	list := make([]*rpc.ConversationInfo, 0, len(rows))
	for _, r := range rows {
		conv, ok := convs[r.ConversationID]
		if !ok || conv == nil {
			l.Errorf("private-message/logic: 成员行缺会话主体，丢弃该行 conversation_id=%d mid=%d", r.ConversationID, mid)
			continue
		}
		peer := peerOf(conv, mid, r.PeerMid)
		blocked, err := peerBlockedForRead(ctx, s, mid, peer)
		if err != nil {
			// 未配置：整页拒绝（把「拿不到判定」当成「全部可见」等于绕过反骚扰门禁）。
			return nil, err
		}
		if blocked {
			continue
		}
		info := conversationInfo(r, conv)
		if info == nil {
			continue
		}
		list = append(list, info)
	}

	unreadTotal, _, err := s.Members.SumUnread(ctx, mid, in.GetIncludeHidden())
	if err != nil {
		return nil, err
	}

	nextCursor := ""
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		nextCursor = encodeTimeIDCursor(last.LastMsgTime, last.ID)
		if nextCursor == "" {
			// 投影时间/主键异常时宁可停在「没有下一页」，也不给一个会重复或漏翻的游标。
			hasMore = false
		}
	}
	return &rpc.ListConversationsReply{
		List:        list,
		NextCursor:  nextCursor,
		HasMore:     hasMore,
		UnreadTotal: unreadTotal,
	}, nil
}
