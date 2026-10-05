package logic

import (
	"context"

	"go-video/services/private-message/internal/svc"
	"go-video/services/private-message/model"
	"go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type GetOrCreateConversationLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetOrCreateConversationLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetOrCreateConversationLogic {
	return &GetOrCreateConversationLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 定位或创建单聊会话（pair_key 唯一索引保证幂等）。
//
// 实现要点：
//  1. mid/peer_mid 必须 >0 且不相等（私信不支持游客、不允许自聊）；
//  2. pair_key 只由 model.PairKey 归一，(a,b) 与 (b,a) 必然同一行，logic 不自行拼接；
//  3. 建档 + 两行成员投影在同一事务内提交（成员行是越权判定的依据，缺行的会话谁都读不到）；
//     Ensure 走 uniq_conv_mid 幂等，整事务可安全重放；
//  4. 本方法不判定关系、不扣陌生人配额：会话行不是内容，只读调用被配额误伤会让
//     「先建会话再发消息」的正常客户端无法工作，配额在 SendMessage 上扣（README 门禁 c）；
//  5. 不回传 pair_key 与 user_a/user_b 顺序等内部键，只回会话主键、是否新建、状态与创建时间。
func (l *GetOrCreateConversationLogic) GetOrCreateConversation(in *rpc.GetOrCreateConversationReq) (*rpc.GetOrCreateConversationReply, error) {
	mid, peer := in.GetMid(), in.GetPeerMid()
	if err := checkPair(mid, peer); err != nil {
		return nil, err
	}
	userA, userB := lowHigh(mid, peer)
	pairKey := model.PairKey(mid, peer)

	var (
		conv    *model.Conversation
		created bool
	)
	err := l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, session sqlx.Session) error {
		c, isNew, err := l.svcCtx.Conversations.FindOrCreateInTx(ctx, session, pairKey, userA, userB)
		if err != nil {
			return err
		}
		conv, created = c, isNew
		// 成员行每轮都补写（幂等）：崩溃在「会话已建、成员行未写」窗口时，
		// 只靠 created=true 会永远留下一个无人能读的会话，重进会话页即自愈。
		return l.svcCtx.Members.Ensure(ctx, session, []*model.ConversationMember{
			newMemberRow(c.ConversationID, userA, userB),
			newMemberRow(c.ConversationID, userB, userA),
		})
	})
	if err != nil {
		return nil, err
	}
	if conv == nil {
		return nil, model.ErrConversationNotFound
	}
	return &rpc.GetOrCreateConversationReply{
		ConversationId: conv.ConversationID,
		Created:        created,
		State:          conv.State,
		Ctime:          conv.CreatedAt,
	}, nil
}

// newMemberRow 构造成员投影初值：游标与未读全 0、本方未隐藏。
// 展示列（last_*）留空由首条消息的 ApplyIncoming 填，建档不伪造「会话里有内容」。
func newMemberRow(conversationID, mid, peer int64) *model.ConversationMember {
	return &model.ConversationMember{
		ConversationID: conversationID,
		Mid:            mid,
		PeerMid:        peer,
		HideState:      model.HideStateNormal,
	}
}
