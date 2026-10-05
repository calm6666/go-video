package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type BroadcastToRoomLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewBroadcastToRoomLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BroadcastToRoomLogic {
	return &BroadcastToRoomLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 向房间广播一条消息（弹幕/系统消息/互动提示统一入口）
func (l *BroadcastToRoomLogic) BroadcastToRoom(in *rpc.BroadcastToRoomReq) (*rpc.BroadcastToRoomReply, error) {
	// 已实现行为（本服务最核心的下发路径，门禁顺序即契约，不可调换；编号对应原桩注释）：
	// 1. 参数：room_id>0（ErrInvalidRoomID）、message_id 非空（ErrEmptyMessageID：去重与审计都依赖它，
	//    绝不允许服务端补随机值绕过）、ValidBroadcastKind（未知类别不「当普通消息放过」）。
	// 2~9. 载荷上限 → 时效 → 发送者鉴权 → 房间可广播 → 路由 → 去重 → 限流 → 扇出 → 审计
	//    整体走 g.runBroadcast（与 ForwardDanmaku / ForwardSystemEvent / SendToUser 同一条链，
	//    四条入口各写一遍鉴权与限流迟早漂移）。要点：
	//    - 鉴权在去重之前：复用旧 message_id 的越权探测也必须留 DENIED 审计；
	//    - 去重在限流之前：重放不消耗配额；
	//    - 权限矩阵唯一事实来源是 model.RoleAllowedToSend，logic 不放宽；
	//    - 用户态发送必须带 sender_lease_id 或 sender_ticket，三元组不匹配是 BAD_TICKET、
	//      不带凭据是 PERMISSION_DENIED（可信内部主体免凭据，归因来自 gRPC metadata 而不是自报角色）；
	//    - require_reliable=true 时下发失败返回 error（审核处置「静默丢失」等于处置未执行）。
	// 10. 无订阅者是正常可丢弃结果（房间没人时广播本就该静默），不是错误。
	// 11. 错误映射：鉴权/参数/依赖故障返回 error；可丢弃类走 accepted + drop_reason。
	//     BroadcastToRoomReply 没有 deny_detail 字段（契约缺口，本轮不改 proto），
	//     丢弃原因文本只进服务端日志，枚举 drop_reason 是回给调用方的唯一依据。
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	roomID := in.GetRoomId()
	if err := g.requireRoomID(roomID); err != nil {
		return nil, err
	}
	messageID := strings.TrimSpace(in.GetMessageId())
	if err := g.requireMessageID(messageID); err != nil {
		return nil, err
	}
	kind := int32(in.GetKind())
	if !model.ValidBroadcastKind(kind) {
		return nil, fmt.Errorf("%w: kind=%d", model.ErrInvalidBroadcastKind, kind)
	}
	topics, err := lgwBroadcastTopics(g, in.GetTargetTopics())
	if err != nil {
		return nil, err
	}
	roles, err := lgwBroadcastRoles(g, in.GetTargetRoles())
	if err != nil {
		return nil, err
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
		Topics:      topics,
		Roles:       roles,
		ExpireAt:    in.GetExpireAt(),
		Priority:    in.GetPriority(),
		Reliable:    in.GetRequireReliable(),
		TraceID:     strings.TrimSpace(in.GetTraceId()),
		UserScoped:  kind == model.KindDanmaku || kind == model.KindInteraction,
	})
	if out.Err != nil {
		l.Errorf("gateway/live-gateway/BroadcastToRoom: room_id=%d kind=%d message_id=%s err=%v",
			roomID, kind, safeIdent(messageID), out.Err)
		return nil, out.Err
	}
	if !out.Accepted && out.Detail != "" {
		g.errorf("live-gateway: 广播被丢弃 room_id=%d message_id=%s kind=%d drop=%d: %s",
			roomID, safeIdent(messageID), kind, out.Drop, out.Detail)
	}
	return &rpc.BroadcastToRoomReply{
		Accepted:            out.Accepted,
		MessageId:           messageID,
		DropReason:          dropReason(out.Drop),
		FanoutNodes:         out.Fanout,
		TargetedConnections: out.Targeted,
		EnqueuedAt:          out.EnqueuedAt,
		Duplicated:          out.Duplicated,
		RateRemaining:       out.Remaining,
	}, nil
}

// lgwBroadcastTopics 子通道过滤器：空=全体（必须保持空，不能被 NormalizeTopics 展开成默认全集，
// 否则「发给全体」会被写成「发给固定那几路」，两者在新增通道时行为完全不同）。
func lgwBroadcastTopics(g gate, raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// NormalizeTopics 已负责去空白/小写/去重并拒绝未知通道（拼错通道会永久收不到消息，
	// 静默忽略比报错糟糕得多）。
	topics, err := repository.NormalizeTopics(raw, g.cfg().MaxTopicsPerSubscription)
	if err != nil {
		return nil, fmt.Errorf("live-gateway: target_topics: %w", err)
	}
	return topics, nil
}

// defaultMaxTargetRoles 与 config.LiveGatewayConf.MaxTargetRoles 的 default 对齐：
// 配置被写成 0 时不能理解成「一个角色都不许填」，那会让整条角色过滤功能静默失效。
const defaultMaxTargetRoles = 6

// lgwBroadcastRoles 角色过滤器：与子通道同性质 —— 都是调用方可控的扇出放大器参数，
// 所以必须有长度上限（config 注释里写的「越界直接拒」到今天才有执行点）。
// 上限只夹长度，不校验角色名：本服务没有角色字符串词表（角色枚举在 rpc.ConnRole，
// 匹配发生在接入层），凭空造一份词表会让上游的合法写法被误拒 —— 该缺口记在 README。
func lgwBroadcastRoles(g gate, raw []string) ([]string, error) {
	roles := lgwTrimStrings(raw)
	max := g.cfg().MaxTargetRoles
	if max <= 0 {
		max = defaultMaxTargetRoles
	}
	if int32(len(roles)) > max {
		return nil, fmt.Errorf("%w: %d roles, max %d", repository.ErrTooManyTargetRoles, len(roles), max)
	}
	return roles, nil
}
