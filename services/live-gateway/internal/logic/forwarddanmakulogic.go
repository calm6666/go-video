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

type ForwardDanmakuLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewForwardDanmakuLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ForwardDanmakuLogic {
	return &ForwardDanmakuLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 转发弹幕到房间（danmaku 服务/网关调用，本服务不改写弹幕事实）
func (l *ForwardDanmakuLogic) ForwardDanmaku(in *rpc.ForwardDanmakuReq) (*rpc.ForwardDanmakuReply, error) {
	// 已实现行为（编号对应本方法原桩注释）：
	// 1. 参数：room_id>0、danmaku_id>0（引用 danmaku 主键，本服务不复制正文入库、不复判审核状态）、
	//    sender_mid>=0、message_id 非空（ErrEmptyMessageID，去重与审计都靠它，服务端不补随机值）、
	//    sent_at 与服务器时刻偏差超过 LiveGateway.HeartbeatMaxSkewSeconds 只观测告警，不拒发
	//    （客户端时钟不可信是常态，拒发等于让弱网用户的弹幕消失）。
	// 2. 归属一致性（弹幕越权最高发的面）交给 g.resolveSender：sender_lease_id 租约上的 mid 与
	//    room_id 必须吻合，带 sender_ticket 时按票据三元组校验；都不带 → PERMISSION_DENIED，
	//    带了但不匹配 → BAD_TICKET。可信内部主体（SERVICE）免凭据，但归因只认 gRPC metadata。
	//    判定后的 role/mid 以服务端读到的登记值为准，不接受 in.sender_role 自报（矩阵里 VIEWER
	//    也能发弹幕，所以自报提权是唯一风险方向）。
	// 3. 权限矩阵：kind 固定 KindDanmaku，model.RoleAllowedToSend 是唯一事实来源；
	//    content_digest 只用于排障比对（正文与摘要都不参与内容判定，本服务无审核权）。
	// 4. 限流是本方法主要职责：房间层 BroadcastQps + USER 层 DanmakuQps（含房间层更严的一侧）
	//    都在 g.runBroadcast 的第 7 步做，超限 → accepted=false + RATE_LIMITED + rate_remaining。
	//    **禁止「丢弃但仍返回 accepted=true」**：danmaku 侧会以为已下发，前后端计数从此不一致。
	// 5. 幂等：(room_id, message_id) 的 Redis 去重窗口 + uniq_room_message 兜底，都在核心链里。
	// 6. 载荷：payload 带正文（下发给观看端），只在内存与下发通道里短期存在；
	//    审计行只存 payload_digest + payload_bytes（README 数据分层）。
	// 7. 路由与扇出：与 BroadcastToRoom 同一条路径（无路由 NO_ROUTE、无人 NO_SUBSCRIBER、
	//    通道未接线 TRANSPORT_UNAVAILABLE，且本入口 require_reliable 恒为 false——
	//    弹幕是可丢弃语义，不该让上游为重发一条弹幕而拿到 error 再放大流量）。
	// 8. 房间可广播性问 live-room；未接线返回显式 error，不默认「房间能发」。
	// 9. 本方法不改写弹幕事实：落库、审核、折叠都归 danmaku 服务（AGENTS.md §5 一个写者）。
	// 10. 错误映射：限流/丢弃走 accepted + drop_reason；参数与依赖故障返回 error。
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	roomID := in.GetRoomId()
	if err := g.requireRoomID(roomID); err != nil {
		return nil, err
	}
	if in.GetDanmakuId() <= 0 {
		return nil, fmt.Errorf("live-gateway: danmaku_id=%d must reference an existing danmaku row "+
			"(本服务不复制弹幕正文，无主键的转发无法与 danmaku 台账对上)", in.GetDanmakuId())
	}
	if err := g.requireMid(in.GetSenderMid()); err != nil {
		return nil, err
	}
	messageID := strings.TrimSpace(in.GetMessageId())
	if err := g.requireMessageID(messageID); err != nil {
		return nil, err
	}
	if sentAt := in.GetSentAt(); sentAt > 0 {
		// 只观测：弹幕的送达判定用服务端时刻，客户端时钟不参与任何权限结论。
		if skew := model.NowUnix() - sentAt; skew > int64(g.cfg().HeartbeatMaxSkewSeconds) ||
			-skew > int64(g.cfg().HeartbeatMaxSkewSeconds) {
			g.errorf("live-gateway: 弹幕 sent_at 偏差过大 room_id=%d danmaku_id=%d skew=%ds（仅观测，不影响下发）",
				roomID, in.GetDanmakuId(), skew)
		}
	}

	out := g.runBroadcast(l.ctx, broadcastIntent{
		RoomID:      roomID,
		Kind:        model.KindDanmaku,
		MessageID:   messageID,
		SenderMid:   in.GetSenderMid(),
		ClaimedRole: model.RoleUnspecified, // 本请求没有角色字段：一律按凭据/归因判定，不给提权入口
		LeaseID:     strings.TrimSpace(in.GetSenderLeaseId()),
		Ticket:      strings.TrimSpace(in.GetSenderTicket()),
		Payload:     in.GetPayload(),
		UserScoped:  true,
		Source:      "danmaku",
		TraceID:     strings.TrimSpace(in.GetTraceId()),
	})
	if out.Err != nil {
		l.Errorf("gateway/live-gateway/ForwardDanmaku: room_id=%d danmaku_id=%d message_id=%s err=%v",
			roomID, in.GetDanmakuId(), safeIdent(messageID), out.Err)
		return nil, out.Err
	}
	if !out.Accepted {
		g.errorf("live-gateway: 弹幕未下发 room_id=%d danmaku_id=%d message_id=%s drop=%d: %s",
			roomID, in.GetDanmakuId(), safeIdent(messageID), out.Drop, out.Detail)
	}
	return &rpc.ForwardDanmakuReply{
		Accepted:            out.Accepted,
		MessageId:           messageID,
		DropReason:          dropReason(out.Drop),
		FanoutNodes:         out.Fanout,
		TargetedConnections: out.Targeted,
		RateRemaining:       out.Remaining,
	}, nil
}
