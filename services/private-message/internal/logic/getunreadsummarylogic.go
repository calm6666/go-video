package logic

import (
	"context"

	"go-video/services/private-message/internal/svc"
	"go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetUnreadSummaryLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetUnreadSummaryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetUnreadSummaryLogic {
	return &GetUnreadSummaryLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// unreadSummaryCache 是角标的秒级缓存载荷（真值恒在 MySQL）。
// computed_at 随缓存一起回放，客户端据此判断角标新鲜度，不用「本地时间」误判。
type unreadSummaryCache struct {
	UnreadTotal         int64 `json:"unread_total"`
	UnreadConversations int64 `json:"unread_conversations"`
	ComputedAt          int64 `json:"computed_at"`
}

// 未读汇总（投影，可重算）。
//
// 实现要点：
//  1. 只按登录者自己的 mid 聚合（Members.SumUnread），本方法不接受「查别人的角标」；
//  2. 响应不含任何内容字段（只有计数），因此不需要关系判定，也不调用下游；
//  3. force=false 时先读缓存，缓存不可用/未命中一律回源，缓存不是事实源；
//  4. hide_state 口径固定为 false（不含隐藏会话），与 ListConversations 默认过滤保持一致，
//     否则角标与会话列表会对不上；
//  5. 只读不写：投影漂移由 CountVisibleAfterSeq + RebuildProjection（运营/cron 入口）修复。
func (l *GetUnreadSummaryLogic) GetUnreadSummary(in *rpc.GetUnreadSummaryReq) (*rpc.GetUnreadSummaryReply, error) {
	mid := in.GetMid()
	if err := checkMid(mid); err != nil {
		return nil, err
	}
	key := unreadCacheKey(mid)
	if !in.GetForce() {
		var cached unreadSummaryCache
		if cacheGetJSON(l.ctx, l.svcCtx, key, &cached) && cached.ComputedAt > 0 {
			return &rpc.GetUnreadSummaryReply{
				UnreadTotal:         cached.UnreadTotal,
				UnreadConversations: cached.UnreadConversations,
				ComputedAt:          cached.ComputedAt,
			}, nil
		}
	}

	total, conversations, err := l.svcCtx.Members.SumUnread(l.ctx, mid, false)
	if err != nil {
		return nil, err
	}
	at := timeNowUnix()
	cacheSetJSON(l.ctx, l.svcCtx, key, unreadSummaryCache{
		UnreadTotal:         total,
		UnreadConversations: conversations,
		ComputedAt:          at,
	}, ttlUnreadSummary)
	return &rpc.GetUnreadSummaryReply{
		UnreadTotal:         total,
		UnreadConversations: conversations,
		ComputedAt:          at,
	}, nil
}
