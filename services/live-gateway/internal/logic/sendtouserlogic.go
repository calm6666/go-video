package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SendToUserLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSendToUserLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SendToUserLogic {
	return &SendToUserLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 定向投递给某个用户在本房间的在线连接（审核回执、私信提示）
func (l *SendToUserLogic) SendToUser(in *rpc.SendToUserReq) (*rpc.SendToUserReply, error) {
	// 已实现行为（编号对应本方法原桩注释）：
	// 1. 参数：room_id>0、target_mid>0（单播不允许 target 是游客 0，那等于给全房间游客发）、
	//    message_id 非空、ValidBroadcastKind。
	// 2. 鉴权顺序与 BroadcastToRoom **完全一致**（同一个 g.runBroadcast）：NormalizeRole →
	//    KindRequiresTrustedSender → RoleAllowedToSend → 发送者凭据三元组。单播不是矩阵后门：
	//    VIEWER 用本入口给主播定向发 MODERATION 消息一律 PERMISSION_DENIED + state=DENIED 审计。
	// 3. 目标定位走 Redis（Leases.ListUserLeases，不查 MySQL）：该用户在本房间无有效租约 →
	//    result=NO_LEASE（正常业务结论，不是 error）；多端同时在线时 delivered_leases 回全部命中数，
	//    数字来自接入层回报，本服务不猜（猜出来的「已送达 N 人」在排障时比 0 更有害）。
	// 4. 幂等：核心链按 (room_id, message_id) 去重（Redis 窗口 + uniq_room_message 兜底）。
	//    proto 注释的 (room, target, message_id) 三元组里 target 不在键里，
	//    因此**调用方必须为每个 target 生成不同 message_id**，否则同房间多目标单播会被误判重复；
	//    live_gw_broadcast_log 也没有 target_mid 列（要「给某人发过哪些定向消息」需同步改
	//    proto + model + 迁移，见服务 README 疑点与本轮交付报告缺口）。
	// 5. 载荷上限与 expire_at：与 BroadcastToRoom 同一套（本请求无 expire_at 字段，按不限处理）。
	// 6. 限流：target 维度走 USER 链（USER→GLOBAL，不含 ROOM/NODE），定向消息不该被房间大配额
	//    淹掉个体降配 —— 由 broadcastIntent.UserScoped 打开。
	// 7. 通道未接线 → result=TRANSPORT_UNAVAILABLE + deny_reason 同因；
	//    require_reliable=true 时返回 error（审核处置不得静默丢失）。
	// 8. 私信正文不进本服务落库：审计只有 payload_digest；私信事实源是 private-message（AGENTS.md §5）。
	// 9. 错误映射：NO_LEASE / DENIED 走 result 返回（无 error），依赖故障返回 error。
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	roomID, target := in.GetRoomId(), in.GetTargetMid()
	if err := g.requireRoomID(roomID); err != nil {
		return nil, err
	}
	if target <= 0 {
		return nil, fmt.Errorf("%w: target_mid=%d, unicast requires a real user (0 would fan out to all guests)",
			model.ErrInvalidMid, target)
	}
	messageID := strings.TrimSpace(in.GetMessageId())
	if err := g.requireMessageID(messageID); err != nil {
		return nil, err
	}
	kind := int32(in.GetKind())
	if !model.ValidBroadcastKind(kind) {
		return nil, fmt.Errorf("%w: kind=%d", model.ErrInvalidBroadcastKind, kind)
	}

	// --- 3. 目标定位（Redis 在线态；无连接是正常结论）---
	leaseIDs, err := g.svcCtx.Leases.ListUserLeases(l.ctx, roomID, target, lgwScanLimit(g))
	if err != nil {
		return nil, err
	}
	targets := make([]string, 0, len(leaseIDs))
	for _, rec := range leaseIDs {
		if rec == nil {
			continue
		}
		if rec.EffectiveState(model.NowUnix()) != model.LeaseStateActive {
			continue // 过期/被踢的连接不算命中：打给死连接会让 delivered_leases 虚高
		}
		targets = append(targets, rec.LeaseID)
	}
	if len(targets) == 0 {
		// 依然要走完整鉴权链吗？不 —— 鉴权是「能不能发」的结论，与目标在不在线无关，
		// 但 NO_LEASE 是终局结论：这里返回就不再落审计，否则「对方不在线」会灌满审计表。
		// 越权尝试仍会在下面那条路径上留下 DENIED 行。
		g.infof("live-gateway: SendToUser 目标无在线连接 room_id=%d target_mid=%d kind=%d message_id=%s",
			roomID, target, kind, safeIdent(messageID))
		return &rpc.SendToUserReply{
			Result:    deliveryResult(model.DeliveryNoLease),
			MessageId: messageID,
		}, nil
	}

	out := g.runBroadcast(l.ctx, broadcastIntent{
		RoomID:      roomID,
		Kind:        kind,
		MessageID:   messageID,
		SenderMid:   in.GetSenderMid(),
		ClaimedRole: int32(in.GetSenderRole()),
		LeaseID:     strings.TrimSpace(in.GetSenderLeaseId()),
		Ticket:      strings.TrimSpace(in.GetSenderTicket()),
		Payload:     in.GetPayload(),
		// 本请求没有 priority 字段：单播按普通优先级走限流（proto 只给了 require_reliable，
		// 而「可靠」是关于失败要不要上抛，不是关于能不能绕过配额）。
		Reliable:   in.GetRequireReliable(),
		TraceID:    strings.TrimSpace(in.GetTraceId()),
		UserScoped: true,
		LeaseIDs:   targets,
	})
	if out.Err != nil {
		l.Errorf("gateway/live-gateway/SendToUser: room_id=%d target_mid=%d kind=%d message_id=%s err=%v",
			roomID, target, kind, safeIdent(messageID), out.Err)
		return nil, out.Err
	}
	return &rpc.SendToUserReply{
		Result:          deliveryResult(lgwUnicastResult(out)),
		DenyReason:      dropReason(out.Drop),
		DeliveredLeases: out.Targeted,
		MessageId:       messageID,
	}, nil
}

// lgwUnicastResult 把下发结论翻译成单播语义。
//
// 只翻译「调用方需要区别对待」的三类：送达、被拒（鉴权/凭据）、通道不可用；
// 其余（限流、载荷过大、无路由、无人在线）统一留 UNSPECIFIED + 可读的 deny_reason，
// 不把「被限流」谎报成「被拒绝」—— 前者该重试，后者永远不该重试。
func lgwUnicastResult(out broadcastOutcome) int32 {
	switch {
	case out.Accepted:
		return model.DeliverySent
	case out.Drop == model.DropPermissionDenied || out.Drop == model.DropBadTicket:
		return model.DeliveryDenied
	case lgwIsTransportErr(out.Err) || out.Drop == model.DropTransportUnavailable:
		return model.DeliveryTransportUnavailable
	default:
		return model.DeliveryUnspecified
	}
}
