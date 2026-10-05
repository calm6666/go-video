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

// lgwSystemEventTypes 系统事件语义白名单（proto ForwardSystemEventReq.event_type 注释列出的四类）。
//
// 未知 event_type 一律拒绝而不是「当普通系统消息放过」：下游接入层是按这个串决定要不要改本地房间
// 状态的，放过一个拼错的名字等于让对方静默收到一条不会被处理的事件（比报错难查得多）。
var lgwSystemEventTypes = map[string]bool{
	"room.open":       true,
	"room.disconnect": true,
	"room.close":      true,
	"moderation.mute": true,
}

// lgwRoomCloseEvent room.close 是唯一带路由副作用的事件。
const lgwRoomCloseEvent = "room.close"

type ForwardSystemEventLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewForwardSystemEventLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ForwardSystemEventLogic {
	return &ForwardSystemEventLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 转发系统/房间状态/审核处置事件（内部服务与运营专用入口）
func (l *ForwardSystemEventLogic) ForwardSystemEvent(in *rpc.ForwardSystemEventReq) (*rpc.ForwardSystemEventReply, error) {
	// 已实现行为（编号对应本方法原桩注释）：
	// 1. 参数：room_id>0、event_id 非空（ErrEmptyEventID：系统事件的幂等键，缺了上游重试会重复下发
	//    「开播」这类关键状态）、kind ∈ {ROOM_STATE, MODERATION, SYSTEM}（弹幕类不走本入口，
	//    避免两条入口都能发同类消息造成权限口径分裂）、event_type 过白名单。
	//    本方法没有 message_id 字段，审计行的定位串用 "evt:"+event_id 渲染（uniq_room_message 仍生效）。
	// 2. 来源鉴权：g.requireInternalService(source_service) —— 只接受可信内部主体/运营，
	//    且 source_service 必须与 gRPC 归因里的 service 一致（自报来源不参与判定，只作审计）；
	//    客户端态凭据调本入口是明确攻击面 → PERMISSION_DENIED + DENIED 审计。
	// 3. 主播归属：anchor_mid>0 时问 live-room 它是否确实是该房间归属主播；不一致时**以 live-room 为准**
	//    （审计与下发都用真值），并把差异记进日志；未接线时不因此拒发可信内部事件，但记一次
	//    「未校验」告警（这里降级放行的是可信内部调用方，与 Acquire 的「客户端角色降级」方向不同）。
	// 4. 幂等：按 (room_id, event_id) 去重（Redis 事件窗口 + 审计表 FindByEvent 兜底），
	//    命中 → duplicated=true + accepted=true，不再二次扇出。
	// 5. 可靠性：require_reliable=true（审核处置、开播/下播）时任何下发失败都返回 error
	//    （无路由 ErrNoRoute、通道未接线 ErrTransportUnavailable）——「处置未送达」必须让上游感知；
	//    false 时允许丢弃并回 drop_reason。
	// 6. 优先级：系统事件恒按 priority=1 进核心链 → 豁免限流，但鉴权一步都没豁免（AGENTS.md §5/§8）。
	// 7. 审计：SENT/DROPPED/DENIED 三态之一，drop_reason 必须可解释；载荷只存 payload_digest
	//    （事件载荷可能含处置理由文本，不入库）。
	// 8. room.close 的副作用：事件送达后把 live_gw_room_route 收敛到 OFFLINE，且必须走
	//    SERVING → DRAINING → OFFLINE 两步条件更新（model.IsValidRouteTransition 没给绕过口子；
	//    想允许房间关闭直接 OFFLINE 是先改状态机的契约变更，需评审，本逻辑不自行放宽）。
	//    room.open 不预建路由（路由由首个真实连接的 JoinRoom 登记，避免空房间占表）。
	// 9. 错误映射：duplicated 与 accepted 的组合永远自洽（accepted=false 时 duplicated=false）。
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	roomID := in.GetRoomId()
	if err := g.requireRoomID(roomID); err != nil {
		return nil, err
	}
	eventID := strings.TrimSpace(in.GetEventId())
	if err := g.requireEventID(eventID); err != nil {
		return nil, err
	}
	kind := int32(in.GetKind())
	if kind != model.KindRoomState && kind != model.KindModeration && kind != model.KindSystem {
		return nil, fmt.Errorf("%w: kind=%d, ForwardSystemEvent accepts ROOM_STATE/MODERATION/SYSTEM only",
			model.ErrInvalidBroadcastKind, kind)
	}
	eventType := strings.TrimSpace(in.GetEventType())
	if eventType == "" {
		return nil, fmt.Errorf("%w: room_id=%d event_id=%s", model.ErrEmptyEventType, roomID, safeIdent(eventID))
	}
	if !lgwSystemEventTypes[eventType] {
		return nil, fmt.Errorf("%w %q", model.ErrUnknownEventType, safeIdent(eventType))
	}

	// --- 2. 来源鉴权（先于任何扇出与审计）---
	c, err := g.requireInternalService(in.GetSourceService())
	if err != nil {
		g.auditDrop(l.ctx, broadcastIntent{
			RoomID: roomID, Kind: kind, EventID: eventID, AuditMessage: "evt:" + eventID,
			Source: in.GetSourceService(), TraceID: strings.TrimSpace(in.GetTraceId()),
		}, model.BroadcastLogDenied, lgwDrop(model.DropPermissionDenied, "caller is not an attested internal service"))
		return nil, err
	}
	// 审计来源只认归因：SERVICE 主体的 source_service 已在 requireInternalService 里与
	// x-gw-caller-service 比对过（不一致直接拒），OPERATOR 没有服务名可比，
	// 于是自报来源一律不进审计——否则归因运营能把一条开播事件标成任意服务名，
	// 「这条事件是谁发的」从此不可信（caller.go 对 OPERATOR 分支的注释就是这个口径）。
	source := c.service
	if claimed := strings.TrimSpace(in.GetSourceService()); claimed != "" && !strings.EqualFold(claimed, source) {
		g.errorf("live-gateway: ForwardSystemEvent 自报 source_service=%q 与归因主体 %s 不符，审计按归因记账 room_id=%d event_id=%s",
			safeIdent(claimed), safeIdent(c.String()), roomID, safeIdent(eventID))
	}
	if source == "" {
		source = c.String()
	}
	// --- 3. 主播归属（以 live-room 为准，本服务不复判）---
	anchorMid := in.GetAnchorMid()
	if anchorMid > 0 {
		if !g.svcCtx.Rooms.Available() {
			g.errorf("live-gateway: live-room 未接线，anchor_mid=%d 的归属未校验就下发了 room_id=%d event_id=%s",
				anchorMid, roomID, safeIdent(eventID))
		} else {
			owner, oerr := g.svcCtx.Rooms.IsOwner(l.ctx, roomID, anchorMid)
			if oerr != nil {
				g.errorf("live-gateway: 主播归属查询失败（记为未校验后继续，可信内部调用方不因依赖抖动被拒）"+
					" room_id=%d anchor_mid=%d: %v", roomID, anchorMid, oerr)
			} else if !owner {
				g.errorf("live-gateway: event.anchor_mid=%d 不是 room_id=%d 的归属主播（以 live-room 为准），"+
					"本次按 anchor_mid=0 下发 event_id=%s", anchorMid, roomID, safeIdent(eventID))
				anchorMid = 0
			}
		}
	}

	out := g.runBroadcast(l.ctx, broadcastIntent{
		RoomID:       roomID,
		Kind:         kind,
		EventID:      eventID,
		AuditMessage: "evt:" + eventID,
		DedupEvent:   true,
		SenderMid:    anchorMid,
		ClaimedRole:  c.role,
		Payload:      in.GetPayload(),
		Priority:     1,
		Reliable:     in.GetRequireReliable(),
		Source:       source,
		TraceID:      strings.TrimSpace(in.GetTraceId()),
	})
	if out.Err != nil {
		l.Errorf("gateway/live-gateway/ForwardSystemEvent: room_id=%d kind=%d event_id=%s event_type=%s err=%v",
			roomID, kind, safeIdent(eventID), safeIdent(eventType), out.Err)
		return nil, out.Err
	}

	// --- 8. room.close 的路由收敛（在事件送达之后，否则连接收不到「房间已关」）---
	if eventType == lgwRoomCloseEvent && out.Accepted {
		if cerr := lgwConvergeRoomClosed(l.ctx, g, roomID, source, strings.TrimSpace(in.GetTraceId())); cerr != nil {
			if in.GetRequireReliable() {
				return nil, cerr
			}
			g.errorf("live-gateway: room.close 已送达但路由未收敛 room_id=%d: %v", roomID, cerr)
		}
	}
	return &rpc.ForwardSystemEventReply{
		Accepted:            out.Accepted,
		EventId:             eventID,
		DropReason:          dropReason(out.Drop),
		FanoutNodes:         out.Fanout,
		TargetedConnections: out.Targeted,
		Duplicated:          out.Duplicated,
	}, nil
}

// lgwConvergeRoomClosed 把房间路由按 SERVING → DRAINING → OFFLINE 推进到终态。
//
// 每一步都是带 version 的条件更新：另一个节点/另一次重发同一事件时最多只有一步能改到行，
// 0 行由回读判定（已经是 OFFLINE 就是幂等成功，不再报错）。中间态 DRAINING 的意义是
// 「不再接新连接、存量连接还能收到最后一条消息」，跳过它会让关播瞬间的在途消息丢失。
func lgwConvergeRoomClosed(ctx context.Context, g gate, roomID int64, source, traceID string) error {
	store, err := g.svcCtx.RequireStore()
	if err != nil {
		return err
	}
	reason := "room closed by system event"
	patch := model.RoutePatch{DrainReason: &reason, Operator: &source, TraceID: &traceID}
	for _, to := range []int32{model.RouteStateDraining, model.RouteStateOffline} {
		row, ferr := store.RoomRoutes.FindOne(ctx, roomID)
		if ferr != nil {
			return ferr
		}
		if row == nil {
			return fmt.Errorf("%w: room_id=%d 没有路由行，关播事件只完成下发", model.ErrRouteNotFound, roomID)
		}
		if row.State >= to {
			continue // 已在目标态或更后：幂等跳过
		}
		if !model.IsValidRouteTransition(row.State, to) {
			return fmt.Errorf("%w: room route %d state=%d cannot go to %d",
				model.ErrInvalidTransition, roomID, row.State, to)
		}
		aff, uerr := store.RoomRoutes.UpdateState(ctx, roomID, "", []int32{row.State}, row.Version, to, patch)
		if uerr != nil {
			return uerr
		}
		if aff == 0 {
			// 并发对手正在推进同一间房：重读一次，已到位就算成功，没到位就交回上游重试。
			after, aerr := store.RoomRoutes.FindOne(ctx, roomID)
			if aerr != nil {
				return aerr
			}
			if after != nil && after.State >= to {
				continue
			}
			return fmt.Errorf("%w: room_id=%d 路由状态被并发改动，关播收敛需重试", model.ErrVersionConflict, roomID)
		}
		g.invalidateRouteCache(ctx, roomID)
	}
	g.infof("live-gateway: room.close 已把路由收敛到 OFFLINE room_id=%d source=%s", roomID, safeIdent(source))
	return nil
}
