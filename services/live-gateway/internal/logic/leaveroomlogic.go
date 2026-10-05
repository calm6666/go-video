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

type LeaveRoomLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewLeaveRoomLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LeaveRoomLogic {
	return &LeaveRoomLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 退出房间
func (l *LeaveRoomLogic) LeaveRoom(in *rpc.LeaveRoomReq) (*rpc.EmptyReply, error) {
	// 已实现行为：
	// 1. 参数：lease_id 非空（ErrEmptyLeaseID）、room_id>0（ErrInvalidRoomID）、mid>=0；
	//    conn_id 给了就必须与租约登记值一致；reason 只作日志与审计，不影响是否退订，
	//    但形状必须合法（gate.requireReason 拦住换行，否则日志行会被污染）。
	//    proto LeaveRoomReq 没有 request_id 字段，所以本方法不占幂等窗口：
	//    幂等性由 Redis 集合语义本身保证（SREM 不在集合里的成员是成功的空操作，
	//    README 服务表里 LeaveRoom 的幂等键写的就是 (lease_id, room_id)「不在集合里是成功」）。
	// 2. 三元组核验：gate.tripleMatch（room_id + mid）+ conn_id 一致性。
	//    退订是破坏性操作（会让该连接收不到该类消息），越权退订等于替别人静音，
	//    不匹配一律 ErrTripletMismatch + 一行 DENIED 审计，绝不「按更宽松的字段执行」。
	// 3. 与 usableLease 的差别（刻意）：本方法**不拒绝已过期（EXPIRED）的租约**。
	//    宽限期内的断线正是最需要清理订阅的一侧；终态（RELEASED/KICKED）按第 5 步幂等返回。
	// 4. topics 语义：空 → 退订全部并把连接移出房间/用户集合（房间连接数随之下降，
	//    计数走集合大小而不是 DECR，见 helpers 与 leasestore 的 RoomConnectionCount 口径）；
	//    非空 → 只从订阅集合移除这些子通道，连接仍在房间内。
	//    校验复用 repository.ValidTopic 词表（未知 topic 拒绝并指名），
	//    但不能用 NormalizeTopics：它把「空列表」解释成「默认全集」，与 proto
	//    LeaveRoomReq.topics 的「空表示退订全部」正好相反。
	// 5. 幂等：租约键已被回收（Get 返回 nil）、或已处终态 → 按「已退出」返回 EmptyReply，
	//    不报 ErrLeaseNotFound（否则客户端断线清理会永远重试）；Unsubscribe 内部对
	//    「订阅关系不存在」同样返回无副作用的成功（leasestore.go Unsubscribe 第 484-487 行）。
	// 6. 只退订阅，**不删租约、不写 MarkOffline**（README 服务表第 79/87 行的分工：
	//    ReleaseConnectionLease = 删租约 + 退订 + 断线视图；LeaveRoom = SREM）：
	//    退订时连接可能仍然开着（切房顺序是先 Join 新房间再 Leave 旧房间），
	//    此时写 OfflineView 会伪造出「该用户已断开」的事实，让 RedeemReconnectTicket
	//    回显错误的 last_offline_at；租约保持可续租，重连只需再 Join 一次。
	// 7. 路由不动：最后一个连接退出**不**把 live_gw_room_route 置 OFFLINE，也不失效路由缓存。
	//    路由是节点重启后重建广播的投影，只有房间关闭或 DrainRoomRoute 才推进状态
	//    （本方法对 MySQL 零写入，README 数据分层）。
	// 8. 审计：只有越权（三元组不匹配）落 live_gw_broadcast_log（复用 lgwAuditCredentialDenial，
	//    message_id 带秒级时间戳收敛重复探测）；正常退订是逐连接高频易失事件，不落 MySQL。
	// 9. 错误映射：成功返回 EmptyReply；越权/参数返回 model 哨兵；Redis 故障原样返回，不降级为成功。
	if in == nil {
		return nil, model.ErrEmptyLeaseID
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	leaseID := strings.TrimSpace(in.GetLeaseId())
	connID := strings.TrimSpace(in.GetConnId())
	roomID, mid := in.GetRoomId(), in.GetMid()
	reason := strings.TrimSpace(in.GetReason())
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
	if connID != "" {
		if err := g.requireConnID(connID); err != nil {
			return nil, err
		}
	}
	if reason != "" {
		if err := g.requireReason(reason); err != nil {
			return nil, err
		}
	}
	leaveAll := len(in.GetTopics()) == 0
	topics, err := lgwLeaveTopics(in.GetTopics(), g.cfg().MaxTopicsPerSubscription)
	if err != nil {
		return nil, err
	}

	// --- 2/3. 定位租约并核验三元组 ---
	rec, err := g.leaseLookup(l.ctx, leaseID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		g.infof("live-gateway: LeaveRoom 未命中既有租约（键已回收），按已退出返回 room_id=%d lease_id=%s reason=%s",
			roomID, safeIdent(leaseID), safeIdent(reason))
		return &rpc.EmptyReply{}, nil
	}
	if detail := lgwLeaveGuard(rec, roomID, mid, connID); detail != "" {
		lgwAuditCredentialDenial(l.ctx, g, "leave-denied", roomID, mid, leaseID, model.DropPermissionDenied, traceID)
		g.errorf("live-gateway: LeaveRoom 越权 room_id=%d mid=%d lease_id=%s: %s", roomID, mid, safeIdent(leaseID), detail)
		return nil, fmt.Errorf("%w: %s", model.ErrTripletMismatch, detail)
	}
	if model.IsLeaseTerminal(rec.State) {
		// 已释放/已被踢：订阅随连接一起失效，重复退出按幂等成功返回（不报错、不写审计）。
		g.infof("live-gateway: 租约 %s 已处终态 state=%d，退出房间按幂等返回", safeIdent(rec.LeaseID), rec.State)
		return &rpc.EmptyReply{}, nil
	}

	// --- 4/5. 退订（topics 为空 = 移出房间集合；非空 = 只收窄子通道）---
	kept, err := g.svcCtx.Leases.Unsubscribe(l.ctx, rec.LeaseID, rec.RoomID, rec.Mid, topics)
	if err != nil {
		// Redis 故障必须原样返回：吞掉它会让客户端以为已退订，而房间集合与广播扇出目标里还留着这条连接。
		return nil, fmt.Errorf("live-gateway: leave room %d (lease %s): %w", roomID, safeIdent(rec.LeaseID), err)
	}
	g.infof("live-gateway: 已退出房间 room_id=%d mid=%d lease_id=%s 全部退订=%v 剩余子通道=%v reason=%s",
		roomID, mid, safeIdent(rec.LeaseID), leaveAll, kept, safeIdent(reason))
	return &rpc.EmptyReply{}, nil
}

// lgwLeaveTopics 校验退订列表：与订阅共用 repository.ValidTopic 词表（口径只有一份），
// 但保留 proto 的「空列表 = 退订全部」语义 —— 所以不能复用 NormalizeTopics，
// 它把空列表展开成默认全集，用在退订上会把「全退」变成「一个都不退」。
func lgwLeaveTopics(raw []string, maxTopics int32) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if maxTopics > 0 && int32(len(raw)) > maxTopics {
		return nil, fmt.Errorf("%w: %d topics, max %d", repository.ErrTooManyTopics, len(raw), maxTopics)
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		t := strings.ToLower(strings.TrimSpace(item))
		if t == "" {
			return nil, fmt.Errorf("%w: empty topic in list", repository.ErrUnknownTopic)
		}
		if !repository.ValidTopic(t) {
			return nil, fmt.Errorf("%w: %q", repository.ErrUnknownTopic, t)
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out, nil
}

// lgwLeaveGuard 退订前的身份守卫：请求里给了什么就校什么，返回非空即越权描述。
// room/mid 判定直接复用 gate 层的 tripleMatch（helpers.go 里三元组的唯一口径），
// 只在它之上补上 conn_id —— 与 lgwReleaseTripleGuard 同一条线（两个入口都是破坏性操作，不能一严一松）。
func lgwLeaveGuard(rec *repository.LeaseRecord, roomID, mid int64, connID string) string {
	if rec == nil {
		return ""
	}
	if !tripleMatch(rec, roomID, mid) {
		return fmt.Sprintf("lease belongs to (room %d, mid %d), leave asked for (room %d, mid %d)",
			rec.RoomID, rec.Mid, roomID, mid)
	}
	if connID != "" && rec.ConnID != connID {
		return fmt.Sprintf("lease belongs to conn %s, leave asked for conn %s", safeIdent(rec.ConnID), safeIdent(connID))
	}
	return ""
}
