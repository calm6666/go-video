package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// joinRPCName request_id 幂等窗口里本方法的键名（与 proto 的 rpc 名一致，避免与租约族撞键）。
const joinRPCName = "JoinRoom"

// joinRouteWriter live_gw_room_route.updated_by 的取值：路由是接入层登记的，
// 不是运营改的，写死成固定主体而不是把调用方可控字段灌进审计列。
const joinRouteWriter = "live-gateway/join"

type JoinRoomLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewJoinRoomLogic(ctx context.Context, svcCtx *svc.ServiceContext) *JoinRoomLogic {
	return &JoinRoomLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 加入房间（登记订阅关系 + 复用/创建房间路由）
func (l *JoinRoomLogic) JoinRoom(in *rpc.JoinRoomReq) (*rpc.JoinRoomReply, error) {
	// 已实现行为（顺序即契约：只读门禁全部在前 → 占幂等键 → 才允许写入）：
	// 1. 参数：lease_id 必填（proto JoinRoomReq 第 1 行注释「先有租约才能订阅，防止无凭据的订阅注入」
	//    → ErrEmptyLeaseID）、room_id>0、mid>=0（0 是游客，是否放行由配额 allow_guest 决定）、
	//    node_id 经 CleanNodeID 归一且非空（ErrEmptyNodeID）、request_id 必填、conn_id 给了就必须合法。
	// 2. 租约凭据：gate.usableLease 一次完成「存在 + (room_id, mid) 三元组 + 非终态 + 未过期」，
	//    不通过时 joined=false + 对应 DROP_REASON（BAD_TICKET / PERMISSION_DENIED），不返回空成功。
	//    额外一致性守卫 lgwJoinConnGuard：conn_id 与 node_id 必须与租约登记值同源——
	//    租约的承载节点是本服务在 Acquire 时写定的，Join 若允许换节点就能把别人的连接
	//    投影到攻击者选择的节点上（房间路由会跟着错），三元组之外的这条守卫按越权处理。
	// 3. topics：repository.NormalizeTopics 去空白/小写/去重，空列表回落默认全集
	//    （danmaku/state/interaction，刻意不含 moderation/anchor_tip）；未知 topic 与超量
	//    **返回 error**（ErrUnknownTopic/ErrTooManyTopics）而不是静默忽略——拼错 topic 的连接
	//    会永久收不到该类消息，属必须可解释的失败。逐通道按角色门禁（TopicGateByRole，
	//    依据是「谁能发这类消息」），越权 topics → joined=false + DROP_REASON_PERMISSION_DENIED + 越权审计。
	// 4. 房间可进房：向 live-room 问 Broadcastable（ROOM_CLOSED 的判定权在房间所有者，AGENTS.md §5）。
	//    Rooms.Available()==false 或 RPC 故障时**返回显式错误**（ErrLiveRoomNotConfigured），
	//    绝不把「问不到房间状态」当成「可以加入」；只有所有者给出的真实结论
	//    （FINISHED/BANNED/DISABLED 或房间不存在）才翻译成 joined=false + DROP_REASON_ROOM_CLOSED。
	// 5. 配额：resolveQuota(ROOM, room_id) 的 MaxConnections/AllowGuest；连接数取
	//    Leases.RoomConnectionCount（Redis 集合口径）。取数失败**直接返回错误**而不是按 0 判定
	//    （见 getroomroutelogic.go 的告警：「0 读数只做观测，不做权限依据」）。
	//    比较用 count > max 而不是 >=：Acquire 已把本连接登记进房间集合
	//    （leasestore.go Acquire 第 260-273 行的注释即为此约束），本连接已在 max 的名额里。
	// 6. 幂等：request_id 命中且既有租约 joined_at>0 时**只回读、零写入、零审计**，
	//    joined_at 与 subscribed_topics 原样回显（Subscribe 不覆盖既有 joined_at，
	//    所以「首次结果」就在记录里，不需要另存结论）；命中但 joined_at==0 说明首次请求
	//    崩在订阅写入之前（窗口里还是 pending），按未受理重跑一遍——Register 是条件 UPDATE、
	//    Subscribe 是 SADD，两者都是集合语义，可安全重放（同 drainroomroutelogic.go 第 3 步口径）。
	// 7. 路由登记（本方法是 live_gw_room_route 的主要写入点）：RoomRoutes.Register 每房间一行，
	//    primary_node=in.node_id、shard_count=DefaultRoomShardCount、replica_nodes 由
	//    NormalizeReplicaNodes(nil) 保证空集合写 "[]"；行处于 DRAINING 时按 Register 的契约返回
	//    ErrRouteDraining，本方法把它原样上抛并说明「把连接引到别的节点」，绝不复活排空中的节点。
	//    成功后必须 invalidateRouteCache，否则广播还会打到旧节点最多一个 RoomRouteCacheTTLSeconds 周期。
	// 8. 订阅写 Redis（Leases.Subscribe，ttlSeconds 恒传 0：Join 不续租，TTL 只由 Acquire/Renew 决定，
	//    否则「反复 Join」就成了绕过 MaxLeaseTTLSeconds 的续租通道）。连接成员与 topics **不落 MySQL**。
	// 9. 返回：joined_at/subscribed_topics 取订阅后的真实记录；room_connection_count 是 Redis 读数
	//    （失败时按 0 回显并记降级日志，与 lgwServingConnections 同口径）；route_state 走
	//    gate.routeForFanout —— 与 BroadcastToRoom 同一份视图，避免「Join 回显的路由和广播实际打到的节点不一致」。
	// 10. 审计：只有**凭据/权限类拒绝**落 live_gw_broadcast_log（state=DENIED，kind=MODERATION，
	//    message_id 带秒级时间戳天然收敛），复用 lgwAuditCredentialDenial；
	//    加入成功与「房间满/房间已关闭」不写审计行——那是逐连接高频易失事件，
	//    README 数据分层明确禁止把逐条连接状态写进 MySQL，且 ReleaseConnectionLease 同样不写；
	//    房间维度的事实已经落在 live_gw_room_route.updated_by/trace_id 与 Redis 订阅集合里。
	// 11. 错误映射：拒绝走 joined=false + deny_reason；参数非法/依赖故障返回 error。
	if in == nil {
		return nil, model.ErrEmptyLeaseID
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	leaseID := strings.TrimSpace(in.GetLeaseId())
	connID := strings.TrimSpace(in.GetConnId())
	roomID, mid := in.GetRoomId(), in.GetMid()
	traceID := strings.TrimSpace(in.GetTraceId())

	// --- 1. 参数 ---
	if leaseID == "" {
		return nil, model.ErrEmptyLeaseID
	}
	if err := g.requireRoomID(roomID); err != nil {
		return nil, err
	}
	if err := g.requireMid(mid); err != nil {
		return nil, err
	}
	nodeID, err := g.requireNodeID(in.GetNodeId())
	if err != nil {
		return nil, err
	}
	if err := g.requireRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	if connID != "" {
		if err := g.requireConnID(connID); err != nil {
			return nil, err
		}
	}

	// --- 2. 租约凭据（三元组 + 终态 + 时效）---
	rec, drop, detail, err := g.usableLease(l.ctx, leaseID, roomID, mid)
	if err != nil {
		return nil, err
	}
	if drop != 0 {
		lgwAuditCredentialDenial(l.ctx, g, "join-denied", roomID, mid, leaseID, drop, traceID)
		g.errorf("live-gateway: JoinRoom 凭据不通过 room_id=%d mid=%d lease_id=%s drop=%d %s",
			roomID, mid, safeIdent(leaseID), drop, safeIdent(detail))
		return &rpc.JoinRoomReply{Joined: false, DenyReason: dropReason(drop)}, nil
	}
	if detail := lgwJoinConnGuard(rec, connID, nodeID); detail != "" {
		lgwAuditCredentialDenial(l.ctx, g, "join-denied", roomID, mid, leaseID, model.DropPermissionDenied, traceID)
		g.errorf("live-gateway: JoinRoom 越权 room_id=%d mid=%d lease_id=%s: %s", roomID, mid, safeIdent(leaseID), detail)
		return &rpc.JoinRoomReply{Joined: false, DenyReason: dropReason(model.DropPermissionDenied)}, nil
	}

	// --- 3. topics 归一 + 角色门禁 ---
	topics, err := repository.NormalizeTopics(in.GetTopics(), g.cfg().MaxTopicsPerSubscription)
	if err != nil {
		return nil, err
	}
	role := model.NormalizeRole(rec.Role)
	for _, t := range topics {
		if gerr := repository.TopicGateByRole(t, role); gerr != nil {
			lgwAuditCredentialDenial(l.ctx, g, "join-topic-denied", roomID, mid, leaseID,
				model.DropPermissionDenied, traceID)
			g.errorf("live-gateway: JoinRoom 订阅越权 room_id=%d mid=%d role=%d: %v", roomID, mid, role, gerr)
			return &rpc.JoinRoomReply{Joined: false, DenyReason: dropReason(model.DropPermissionDenied)}, nil
		}
	}

	// --- 4. 房间可进房（live-room 判定，未接线不降级为放行）---
	canJoin, why, err := lgwRoomJoinable(l.ctx, g, roomID)
	if err != nil {
		return nil, err
	}
	if !canJoin {
		g.errorf("live-gateway: JoinRoom 房间不可进房 room_id=%d mid=%d: %s", roomID, mid, safeIdent(why))
		return &rpc.JoinRoomReply{Joined: false, DenyReason: dropReason(model.DropRoomClosed)}, nil
	}

	// --- 5. 配额与在线数 ---
	eff, err := g.resolveQuota(l.ctx, model.QuotaScopeRoom, roomID)
	if err != nil {
		return nil, err
	}
	if mid == 0 && !eff.AllowGuest {
		lgwAuditCredentialDenial(l.ctx, g, "join-guest-denied", roomID, mid, leaseID,
			model.DropPermissionDenied, traceID)
		g.errorf("live-gateway: JoinRoom 游客接入被配额拒绝 room_id=%d", roomID)
		return &rpc.JoinRoomReply{Joined: false, DenyReason: dropReason(model.DropPermissionDenied)}, nil
	}
	count, err := g.svcCtx.Leases.RoomConnectionCount(l.ctx, roomID, lgwScanLimit(g))
	if err != nil {
		// 计数不可用时宁可让接入层重试，也不能按 0 放行——那等于「Redis 抖动 → 房间看起来没人 → 无限接入」。
		return nil, fmt.Errorf("live-gateway: JoinRoom 房间连接数读数不可用，无法做配额判定 room_id=%d: %w", roomID, err)
	}
	if eff.MaxConnections > 0 && count > eff.MaxConnections {
		g.errorf("live-gateway: JoinRoom 房间连接数超限 room_id=%d count=%d max=%d", roomID, count, eff.MaxConnections)
		return &rpc.JoinRoomReply{Joined: false, DenyReason: dropReason(model.DropRateLimited)}, nil
	}

	// --- 6. request_id 幂等：命中即回读，零写入零审计（放在全部只读门禁之后）---
	replay, _, err := g.idempotentReplay(l.ctx, joinRPCName, in.GetRequestId())
	if err != nil {
		return nil, err
	}
	if replay {
		cur, gerr := g.svcCtx.Leases.Get(l.ctx, leaseID)
		if gerr != nil {
			return nil, gerr
		}
		if cur != nil && cur.JoinedAt > 0 {
			g.infof("live-gateway: JoinRoom 按 request_id 重放，回显既有订阅不重复登记 room_id=%d lease_id=%s joined_at=%d",
				roomID, safeIdent(leaseID), cur.JoinedAt)
			return l.lgwJoinReply(cur, roomID)
		}
		g.infof("live-gateway: JoinRoom request_id 命中的首次请求未完成订阅写入，按未受理重新执行 room_id=%d lease_id=%s",
			roomID, safeIdent(leaseID))
	}

	// --- 7. 路由登记 ---
	store, err := g.svcCtx.RequireStore()
	if err != nil {
		return nil, err
	}
	shard := g.cfg().DefaultRoomShardCount
	if shard < 1 {
		shard = 1
	}
	route := &model.LiveGwRoomRoute{
		RoomId:      roomID,
		PrimaryNode: nodeID,
		ShardCount:  shard,
		UpdatedBy:   joinRouteWriter,
		TraceId:     traceID,
	}
	if _, rerr := store.RoomRoutes.Register(l.ctx, route); rerr != nil {
		if errors.Is(rerr, model.ErrRouteDraining) {
			return nil, fmt.Errorf("%w: room_id=%d 的路由正在排空，请把该连接引到其它节点（node=%s）",
				rerr, roomID, safeIdent(nodeID))
		}
		return nil, rerr
	}
	g.invalidateRouteCache(l.ctx, roomID)

	// --- 8. 订阅登记（Redis；ttlSeconds=0 表示不借 Join 续租）---
	if serr := g.svcCtx.Leases.Subscribe(l.ctx, leaseID, roomID, mid, topics, 0); serr != nil {
		return nil, fmt.Errorf("live-gateway: JoinRoom 订阅登记失败 room_id=%d lease_id=%s: %w",
			roomID, safeIdent(leaseID), serr)
	}
	after, err := g.svcCtx.Leases.Get(l.ctx, leaseID)
	if err != nil {
		return nil, err
	}
	if after == nil {
		// 刚 Subscribe 成功就读不到：Redis 正在丢键，如实报依赖故障而不是回一个假 joined=true。
		return nil, fmt.Errorf("%w: lease %s disappeared right after subscribe", model.ErrLeaseNotFound, safeIdent(leaseID))
	}
	g.rememberIdempotent(l.ctx, joinRPCName, in.GetRequestId(), nodeID)
	g.infof("live-gateway: 已加入房间 room_id=%d mid=%d lease_id=%s node=%s role=%d topics=%v",
		roomID, mid, safeIdent(leaseID), safeIdent(nodeID), role, topics)
	return l.lgwJoinReply(after, roomID)
}

// lgwJoinReply 组装 JoinRoom 成功回显：joined_at/subscribed_topics 来自 Redis 记录本身，
// room_connection_count 是易失读数（取不到按 0 回显并记降级日志），route_state 走广播同一条读路径。
func (l *JoinRoomLogic) lgwJoinReply(rec *repository.LeaseRecord, roomID int64) (*rpc.JoinRoomReply, error) {
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	count, err := g.svcCtx.Leases.RoomConnectionCount(l.ctx, roomID, lgwScanLimit(g))
	if err != nil {
		g.errorf("live-gateway: 房间 %d 连接数读数不可用，room_connection_count 按 0 回显（仅观测）: %v", roomID, err)
		count = 0
	}
	row, _, err := g.routeForFanout(l.ctx, roomID)
	if err != nil {
		return nil, err
	}
	var st int32
	if row != nil {
		st = row.State
	} else {
		// 刚 Register 完就没有行，只可能是并发清理或 DB 故障；订阅已生效，不能因此把成功说成失败，
		// 但必须把「路由状态未知」暴露出来（ROUTE_STATE_UNSPECIFIED + 告警），与 GetRoomRoute 的
		// 「绝不拿空路由伪装有效路由」同方向。
		g.errorf("live-gateway: JoinRoom 回读房间 %d 路由为空，route_state 回显 UNSPECIFIED", roomID)
	}
	return &rpc.JoinRoomReply{
		Joined:              true,
		JoinedAt:            rec.JoinedAt,
		RoomConnectionCount: count,
		RouteState:          routeState(st),
		DenyReason:          dropReason(model.DropOK),
		SubscribedTopics:    append([]string(nil), rec.Topics...),
	}, nil
}

// lgwJoinConnGuard 租约登记的连接身份与承载节点必须与本次 Join 一致，
// 返回非空即越权描述。租约的 node_id 由 Acquire 写定，Join 阶段换节点等于
// 把「谁的连接」改绑到调用方挑的节点上，房间路由与后续扇出都会跟着错。
func lgwJoinConnGuard(rec *repository.LeaseRecord, connID, nodeID string) string {
	if rec == nil {
		return ""
	}
	if connID != "" && rec.ConnID != connID {
		return fmt.Sprintf("lease belongs to conn %s, join asked for conn %s", safeIdent(rec.ConnID), safeIdent(connID))
	}
	if rec.NodeID != "" && rec.NodeID != nodeID {
		return fmt.Sprintf("lease is carried by node %s, join asked for node %s", safeIdent(rec.NodeID), safeIdent(nodeID))
	}
	return ""
}

// lgwRoomJoinable 房间可进房判定。
//
// 只有 live-room 给出的**真实结论**才能变成拒绝（DROP_REASON_ROOM_CLOSED）；
// 未接线与 RPC 故障一律返回错误 —— 与 gateAnchorOwnership 的降级方向一致（拿不到真值就收紧），
// 但进房比「角色降级」影响更大，这里没有可保留的中间态，所以是显式失败而不是放行。
func lgwRoomJoinable(ctx context.Context, g gate, roomID int64) (bool, string, error) {
	if !g.svcCtx.Rooms.Available() {
		return false, "", fmt.Errorf("%w: room_id=%d 的可进房判定问不到真值，JoinRoom 不以「未接线」当作「可加入」",
			repository.ErrLiveRoomNotConfigured, roomID)
	}
	ok, reason, err := g.svcCtx.Rooms.Broadcastable(ctx, roomID)
	if err != nil {
		if errors.Is(err, model.ErrInvalidRoomID) {
			// 房间所有者明确回答「没有这个房间」：这是真结论，按房间不可进房处理。
			return false, "room not found in live-room", nil
		}
		return false, "", err
	}
	return ok, reason, nil
}
