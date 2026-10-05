package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"go-video/common/ratelimit"
	"go-video/services/private-message/internal/svc"
	"go-video/services/private-message/model"
	"go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type SendMessageLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSendMessageLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SendMessageLogic {
	return &SendMessageLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// errSendReplay 只在事务内部用来触发回滚：Insert 撞到 uniq_sender_client_msg
// 说明同一 client_msg_id 已被另一次请求抢先落库，本次分配的 seq 与投影都不该留下。
// 它不会返回给调用方（在 SendMessage 里被翻译成幂等回放响应）。
var errSendReplay = errors.New("private-message: send replay")

// 发送私信（client_msg_id 幂等 + 门禁顺序见 SendMessageReq 注释）。
//
// 门禁顺序是契约的一部分，本方法逐步对应契约蓝图，任何一步都不允许跳过或调换：
//  1. 参数校验（mid 必须登录、载体类型、幂等键、正文长度、media_ref 形态、目标形态：
//     conversation_id 与 peer_mid 二选一，负数 conversation_id 不是「未填」）；
//  2. 幂等回放：命中 (sender_mid, client_msg_id) 直接回首次结果，不再走门禁与频控；
//  3. 本地写侧限流：进程级令牌桶 + 用户级分钟窗口 + 新会话日配额（Redis 计数取不到即拒）；
//  4. 会话定位（成员行证明，README 门禁顺序原文）：conversation_id 或 pair_key 命中既有会话时
//     一律先过 requireMembership —— 只报 peer_mid 不是绕过成员表的第二条发送路径；
//     仅当 pair_key 无行（陌生人首条）时才落到「接收方门槛」放行，成员行由第 7 步的事务建立；
//  5. 反骚扰门禁（黑名单 → 接收范围门槛 → 风控裁决 → 会话冻结）；下游未配置原样返回
//     Err*NotConfigured，绝不把「没接上」当成「门禁通过」；
//  6. 内容处理：无密钥即 ErrCipherKeyMissing，禁止退化明文入库；摘要与指纹都不含原文；
//  7. 事务：建档(+成员行) → 冻结复核 → AllocateSeq → Insert → ApplyIncoming → TouchLastMessage，
//     会话指针、未读计数与消息行一次提交（约束：不允许分事务）；
//  8. 提交后送审：机审开启时消息以 PENDING_REVIEW 落库（仅发送者可见），
//     送审失败保持待审并回 audit_task_id=0，不伪造「已审核通过」；
//  9. 响应只回主键、状态与脱敏摘要；日志只带 mid/会话/消息 ID，禁止带正文或密文。
func (l *SendMessageLogic) SendMessage(in *rpc.SendMessageReq) (*rpc.SendMessageReply, error) {
	ctx := l.ctx
	s := l.svcCtx
	cfg := s.Config.PrivateMessage

	// --- 1. 参数校验 ---
	sender := in.GetMid()
	if err := checkMid(sender); err != nil {
		return nil, err
	}
	msgType := int32(in.GetMsgType())
	if !model.ValidMsgType(msgType) {
		return nil, model.ErrInvalidMsgType
	}
	clientMsgID, err := checkClientMsgID(in.GetClientMsgId())
	if err != nil {
		return nil, err
	}
	content, err := checkContentText(in.GetContent(), cfg.MaxTextLength)
	if err != nil {
		return nil, err
	}
	mediaRef := ""
	if msgType == model.MsgTypeText {
		if content == "" {
			return nil, model.ErrContentEmpty
		}
	} else if mediaRef, err = checkMediaRef(in.GetMediaRef()); err != nil {
		// 非文本载体允许带正文（分享卡片标题），但必须有 asset 引用，且绝不接受 URL。
		return nil, err
	}
	traceID := sanitizeTraceID(in.GetTraceId())

	// 目标形态（rpc/privatemessage.proto:186-187）：conversation_id 与 peer_mid 二选一。
	// 负数 conversation_id 不是「未填」，而是拿不存在的 ID 试探的常见形态：
	// 在这一层就拒掉，绝不能让它退化成「按 peer_mid 建会话」那条分支。
	convID := in.GetConversationId()
	peer := in.GetPeerMid()
	switch {
	case convID < 0:
		return nil, checkConversationID(convID)
	case convID == 0:
		if err := checkMid(peer); err != nil {
			return nil, err
		}
		if peer == sender {
			// 自聊：成员表里永远不可能有这一行（建档只写两个不同 mid 的投影），
			// 与「陌生人没有会话」同口径按非成员拒（见 notMemberForPair 注释）。
			return nil, notMemberForPair(sender, peer)
		}
	}

	// --- 2. 幂等回放（先于限流：客户端重试不该消耗配额、也不该被风控二次评估） ---
	exist, err := s.Messages.FindByClientMsgID(ctx, sender, clientMsgID)
	if err != nil {
		return nil, err
	}
	if exist != nil {
		return &rpc.SendMessageReply{
			MsgId: exist.MsgID, ConversationId: exist.ConversationID, Seq: exist.Seq,
			State: exist.State, Ctime: exist.Ctime, Replayed: true,
			AuditTaskId: exist.AuditTaskID, Preview: exist.Preview,
		}, nil
	}

	// --- 3. 本地写侧限流 ---
	release, err := s.WriteLimiter.Allow(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: 进程级写限流: %v", model.ErrRiskDenied, err)
	}
	op := ratelimit.Ignore
	defer func() { release(op) }()

	if cfg.MaxPerUserPerMinute > 0 {
		// 计数器取不到就等于「没证明没超限」，incrCounter 失败必须拒（fail-closed）。
		n, err := incrCounter(ctx, s, sendRateKey(sender), ttlSendRateWindow)
		if err != nil {
			return nil, fmt.Errorf("%w: 本地发送频控不可用: %v", model.ErrRiskDenied, err)
		}
		if n > int64(cfg.MaxPerUserPerMinute) {
			return nil, fmt.Errorf("%w: 每分钟 %d 条上限（当前 %d）", model.ErrRiskDenied,
				cfg.MaxPerUserPerMinute, n)
		}
	}

	// --- 4. 会话定位：会话与接收方必须来自同一份事实，不能拿 A 的会话往 B 发 ---
	var (
		conv     *model.Conversation
		receiver int64
	)
	if convID > 0 {
		// 已存在的会话：成员行是越权判定的唯一依据，接收方恒由会话主体决定，
		// 不受调用方自带的 peer_mid 影响（README 门禁顺序「会话定位（成员行证明）」）。
		var err error
		if conv, receiver, err = membershipTarget(ctx, s, convID, sender); err != nil {
			return nil, err
		}
	} else {
		var err error
		if conv, err = s.Conversations.FindByPairKey(ctx, model.PairKey(sender, peer)); err != nil {
			return nil, err
		}
		if conv == nil {
			// 新会话（陌生人首条）：此刻成员表里没有行可证，放行与否由「日配额 + 接收方门槛」决定，
			// 成员行由第 7 步事务里的 FindOrCreateInTx + Members.Ensure 建立。
			// 日配额只约束「新建」，配额计数器取不到即拒（fail-closed）。
			if cfg.MaxStrangerConversationsPerDay > quotaUnlimited {
				key := strangerQuotaKey(sender, strconv.FormatInt(epochDay(timeNowUnix()), 10))
				n, err := incrCounter(ctx, s, key, ttlStrangerQuotaDay)
				if err != nil {
					return nil, fmt.Errorf("%w: 陌生人会话配额不可用: %v", model.ErrRiskDenied, err)
				}
				if n > int64(cfg.MaxStrangerConversationsPerDay) {
					return nil, fmt.Errorf("%w: 每日新建会话 %d 上限（当前 %d）", model.ErrRiskDenied,
						cfg.MaxStrangerConversationsPerDay, n)
				}
			}
			receiver = peer
		} else {
			// pair_key 命中既有会话：这一支与显式 conversation_id 完全等价，过同一份成员证明
			// （防 pair 与成员行漂移，也不给「只报 peer_mid」留第二条绕过成员表的路）。
			if conv, receiver, err = membershipTarget(ctx, s, conv.ConversationID, sender); err != nil {
				return nil, err
			}
		}
	}

	// --- 5a/5b. 黑名单与接收范围门槛（关系真值在 social-graph，本库只取判定结果） ---
	if conv != nil {
		// 风控上下文里的会话 ID 用真正解析出来的那一个（请求方可能只给了 peer_mid）。
		convID = conv.ConversationID
	}
	rel, err := loadRelation(ctx, s, sender, receiver)
	if err != nil {
		return nil, err
	}
	if rel.Blocked {
		return nil, model.ErrBlockedByPeer
	}
	setting, err := loadSettingOrDefault(ctx, s, receiver)
	if err != nil {
		return nil, err
	}
	// 陌生人门槛只作用于「这个发送者在本会话内的第一条消息」：
	// 已有历史说明接收方曾经收过对方，不复评（README 门禁 b 的语义边界）。
	firstFromSender := true
	if conv != nil {
		hit, err := s.Messages.HasAnyFromSender(ctx, conv.ConversationID, sender)
		if err != nil {
			return nil, err
		}
		firstFromSender = !hit
	}
	if err := receiverAcceptsSender(setting, rel, firstFromSender); err != nil {
		return nil, err
	}

	// --- 5c. 风控裁决 ---
	if err := riskAllowsSend(ctx, s, sender, convID, receiver, msgType, clientMsgID, traceID); err != nil {
		return nil, err
	}
	// --- 5d. 会话冻结（事务外先判，事务内再复核一次，见事务体注释） ---
	if conv != nil && conv.State == model.ConversationStateFrozen {
		return nil, model.ErrConversationFrozen
	}
	if conv == nil && setting.AllowFrom == model.AllowFromNone {
		return nil, model.ErrBlockedByPeer
	}

	// --- 6. 内容处理：加密、指纹与脱敏摘要（明文只在这三行里存在过） ---
	mc, err := newMessageCipher(s.Config)
	if err != nil {
		return nil, err
	}
	blob, err := mc.encrypt(content)
	if err != nil {
		return nil, err
	}
	preview := buildPreview(msgType, content, cfg.PreviewRunes)
	if preview == "" {
		preview = mediaPreviewLabel(msgType)
	}
	// 机审开启时消息落库即为「待审」（仅发送者可见），结论唯一入口是 ApplyModerationVerdict。
	// 客户端未配置就不可能进入这个状态，因此必须在写入之前判定，避免留下永远等不到结论的行。
	initialState := model.MsgStateNormal
	if cfg.MachineReviewEnabled {
		initialState = model.MsgStatePendingReview
		if _, err := s.ModerationClient(); err != nil {
			return nil, err
		}
	}

	// --- 7. 事务：会话指针 + 未读计数 + 消息行一次提交 ---
	var (
		msgTime  = timeNowUnix()
		stored   *model.Message
		replayID int64
	)
	err = s.DB.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		userA, userB := lowHigh(sender, receiver)
		c, _, err := s.Conversations.FindOrCreateInTx(tctx, session, model.PairKey(sender, receiver), userA, userB)
		if err != nil {
			return err
		}
		// 冻结在事务内复核：门禁判定与写入之间风控可能已把会话冻上，
		// 少这一步就是「冻结后仍有一批消息挤进去」。
		if c.State == model.ConversationStateFrozen {
			return model.ErrConversationFrozen
		}
		// 双方成员行必须存在：缺接收方行会让未读投影静默丢失（ApplyIncoming 是 UPDATE）。
		if err := s.Members.Ensure(tctx, session, []*model.ConversationMember{
			newMemberRow(c.ConversationID, sender, receiver),
			newMemberRow(c.ConversationID, receiver, sender),
		}); err != nil {
			return err
		}
		seq, err := s.Conversations.AllocateSeq(tctx, session, c.ConversationID)
		if err != nil {
			return err
		}
		msg := &model.Message{
			ConversationID: c.ConversationID,
			Seq:            seq,
			SenderMid:      sender,
			ReceiverMid:    receiver,
			MsgType:        msgType,
			ContentCipher:  blob,
			KeyVersion:     mc.version(),
			ContentHash:    mc.hash(content),
			MediaRef:       mediaRef,
			Preview:        preview,
			State:          initialState,
			ClientMsgID:    clientMsgID,
			Ctime:          msgTime,
		}
		msgID, created, err := s.Messages.Insert(tctx, session, msg)
		if err != nil {
			return err
		}
		if !created {
			// 并发重试抢先落库：回滚本事务（含刚分配的 seq），改走幂等回放。
			replayID = msgID
			return errSendReplay
		}
		msg.MsgID = msgID
		if err := s.Members.ApplyIncoming(tctx, session, c.ConversationID, sender, receiver,
			msgID, seq, msgType, preview, msgTime); err != nil {
			return err
		}
		if err := s.Conversations.TouchLastMessage(tctx, session, c.ConversationID, msgID, seq, msgTime); err != nil {
			return err
		}
		stored = msg
		return nil
	})
	if errors.Is(err, errSendReplay) {
		return l.replay(ctx, sender, clientMsgID, replayID)
	}
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, fmt.Errorf("private-message/logic: 发送事务提交但未产生消息 mid=%d", sender)
	}
	op = ratelimit.Success

	// --- 8. 提交后送审（审计失败不回滚已投递的消息，也不标成已审核） ---
	var auditTaskID int64
	if cfg.MachineReviewEnabled {
		taskID, serr := submitForModeration(ctx, s, stored.MsgID, sender, "private message machine review")
		if serr != nil {
			// 消息保持 PENDING_REVIEW（仅发送者可见）：宁可延迟可见，也不放行未检内容。
			l.Errorf("private-message/logic: 送审失败，消息保持待审 msg_id=%d mid=%d: %v", stored.MsgID, sender, serr)
		} else {
			if berr := s.Messages.BindAuditTask(ctx, nil, stored.MsgID, taskID); berr != nil {
				l.Errorf("private-message/logic: 回写审核任务失败 msg_id=%d task_id=%d: %v", stored.MsgID, taskID, berr)
			} else {
				auditTaskID = taskID
			}
		}
	}

	// --- 9. 返回 ---
	invalidateUnreadCache(ctx, s, l.Logger, sender, receiver)
	return &rpc.SendMessageReply{
		MsgId:          stored.MsgID,
		ConversationId: stored.ConversationID,
		Seq:            stored.Seq,
		State:          stored.State,
		Ctime:          stored.Ctime,
		Replayed:       false,
		AuditTaskId:    auditTaskID,
		Preview:        stored.Preview,
	}, nil
}

// replay 回放到 client_msg_id 首次写入的那条消息。
// 回查不到说明抢跑的另一个请求最终回滚了（同一幂等键不可能同时成功），
// 此时返回 ErrConcurrentUpdate 让客户端重试，而不是「当作发送成功」。
func (l *SendMessageLogic) replay(ctx context.Context, sender int64, clientMsgID string, msgID int64) (*rpc.SendMessageReply, error) {
	exist, err := l.svcCtx.Messages.FindByClientMsgID(ctx, sender, clientMsgID)
	if err != nil {
		return nil, err
	}
	if exist == nil {
		return nil, fmt.Errorf("%w: 幂等键冲突但回查不到消息 msg_id=%d", model.ErrConcurrentUpdate, msgID)
	}
	return &rpc.SendMessageReply{
		MsgId: exist.MsgID, ConversationId: exist.ConversationID, Seq: exist.Seq,
		State: exist.State, Ctime: exist.Ctime, Replayed: true,
		AuditTaskId: exist.AuditTaskID, Preview: exist.Preview,
	}, nil
}
