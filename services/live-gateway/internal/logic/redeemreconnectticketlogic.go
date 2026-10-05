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

// redeemRPCName request_id 幂等窗口里本方法的键名（proto RedeemReconnectTicketReq.request_id
// 注释：同 request_id 重放返回同一新租约）。
const redeemRPCName = "RedeemReconnectTicket"

type RedeemReconnectTicketLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRedeemReconnectTicketLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RedeemReconnectTicketLogic {
	return &RedeemReconnectTicketLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 用票据换取新租约（校验 mid/room/有效期，一次性消费）
func (l *RedeemReconnectTicketLogic) RedeemReconnectTicket(in *rpc.RedeemReconnectTicketReq) (*rpc.RedeemReconnectTicketReply, error) {
	// 已实现行为（本方法是「凭据换连接」的入口，越权收益最高，全部只读校验先于任何写入）：
	// 1. 参数：ticket 非空（ErrEmptyTicket）、room_id>0、mid>=0、conn_id 合法、node_id 经 CleanNodeID
	//    归一且非空、request_id 必填（新租约的幂等键）。
	// 2~6. 凭据判定复用 gate.verifyTicketCredential（与三条下发路径的票据鉴权**同一条**代码）：
	//    形状校验 → Signer.VerifyTicketShape → 按 model.TicketHash 定位行（客户端只带原文，库里只有摘要）
	//    → ticket_id 与串同源 → Signer.Verify 常量时间验签 → (room_id, mid) 三元组 → 终态/时效。
	//    密钥未注入时它返回 ErrSignerMissing 错误：本方法原样上抛，**不**降级成「跳过验签放行」。
	//    任一失败都是 allowed=false + deny_reason（不报错，让上游能把越权和依赖故障分开处理）：
	//    三元组不匹配 → DROP_REASON_PERMISSION_DENIED + 一行 DENIED 审计（拿别人的票换自己的租约）；
	//    USED/REVOKED/EXPIRED → 先按摘要回读该行，把 ticket_state 说成真实的那一个
	//    （verifyTicketCredential 对终态统一给 BAD_TICKET，回读只为把 USED 与 REVOKED 分清楚，
	//    REVOKED 按 proto 注释改判 PERMISSION_DENIED）。
	// 7. 角色沿用票据登记的 role（proto RedeemReconnectTicketReq 无 claimed_role，
	//    ReconnectTicketInfo.role 注释「换取租约时不再接受客户端声明」）；ANCHOR 必须重新问 live-room
	//    归属（房间可能已换主播）。判定直接复用 gate.gateAnchorOwnership —— 它已包含本服务唯一的
	//    未接线降级口径：ErrLiveRoomNotConfigured 时降为 VIEWER 并记告警，问不到真值绝不清算通过。
	// 8. 配额与 TTL 与 Acquire/Join 同一套（resolveQuota(ROOM) + clampLeaseTTL）：重连不得绕过配额。
	//    房间连接数取不到时返回错误而不是按 0 放行（同 JoinRoom 第 5 步口径）。游客受 allow_guest 约束。
	// 9. 一次性消费顺序（每步都可解释，绝不出现「新租约已签发但票据仍可用」）：
	//    a) Redis GETDEL 有效位（Leases.ConsumeTicket）——取不到就是已消费/已过期/已撤销，
	//       回读该行给结论，**不签发租约**，所以没有部分状态需要回滚；
	//       有效位里的 ticket_id 与 DB 行不同源时显式拒绝（键被别的写入者动过，不能凭它签发租约）；
	//    b) 读签发时那张租约（best-effort）复用断线宽限期内的订阅视图：topics/joined_at/客户端脱敏字段
	//       原样带到新租约，renew_count 递增（config.ReconnectGraceSeconds 的既有约定）；
	//       OfflineView 必须在任何 Delete 之前读，否则 last_offline_at 会被刷新成「现在」而说谎；
	//    c) Leases.Acquire 新租约（同 (node,conn) 的旧租约由 Acquire 自身置 RELEASED，不留双活）；
	//       若旧租约还在且是另一条连接（客户端换 conn_id 重连），在 b 读完后把它一并收口，
	//       否则它会以 ACTIVE 继续占房间配额名额并出现在在线名单里；
	//    d) Tickets.Consume 条件更新（state=ISSUED 且 room/mid/未过期，new_lease_id 回填）。
	//       0 行 = 并发下已被消费/撤销/三元组不符 → 补偿释放刚签发的租约（Leases.Delete）并回读给结论；
	//       d 失败时票据的 Redis 有效位已经没了，所以「重复兑换」在两条存储上都已封死，补偿只是不留悬空租约。
	// 10. 幂等：request_id 命中且窗口里已回填 lease_id 时，只回读该租约原样返回（零写入、不再消费第二张票）；
	//    命中但结论仍是 pending（首次请求崩在中途）或该租约键已回收时，按未受理重新执行一遍 ——
	//    与 drainroomroutelogic.go 第 3 步同一条口径。
	// 11. last_offline_at / missed_message_estimate 来自 Redis 断线视图；视图已被回收或读数失败时回 0 并记
	//     降级日志（proto 注释「可丢弃语义，仅提示」），不伪造历史。
	// 12. 审计：USED 的收敛写在 live_gw_reconnect_ticket 行上（used_at/new_lease_id/trace_id），
	//     不往 live_gw_broadcast_log 写逐连接签发/兑换行；只有越权尝试落 DENIED（同 Issue 第 9 步口径）。
	// 13. 错误映射：allowed=false 的正常拒绝走响应体（必带 deny_reason + ticket_state）；
	//     参数非法与依赖故障返回 error。
	if in == nil {
		return nil, model.ErrEmptyTicket
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	ticket := strings.TrimSpace(in.GetTicket())
	connID := strings.TrimSpace(in.GetConnId())
	roomID, mid := in.GetRoomId(), in.GetMid()
	traceID := strings.TrimSpace(in.GetTraceId())
	requestID := strings.TrimSpace(in.GetRequestId())

	// --- 1. 参数 ---
	if ticket == "" {
		return nil, model.ErrEmptyTicket
	}
	if err := g.requireRoomID(roomID); err != nil {
		return nil, err
	}
	if err := g.requireMid(mid); err != nil {
		return nil, err
	}
	if err := g.requireConnID(connID); err != nil {
		return nil, err
	}
	nodeID, err := g.requireNodeID(in.GetNodeId())
	if err != nil {
		return nil, err
	}
	if err := g.requireRequestID(requestID); err != nil {
		return nil, err
	}

	// --- 2~6. 验签 + 定位 + 三元组 + 状态 + 时效 ---
	row, drop, detail, err := g.verifyTicketCredential(l.ctx, ticket, roomID, mid)
	if err != nil {
		return nil, err
	}
	if drop != 0 {
		return l.lgwRedeemDenyByTicket(g, ticket, roomID, mid, traceID, drop, detail)
	}

	// --- 7. 角色（票据登记值 + ANCHOR 重新确认归属）---
	verdict, verr := g.gateAnchorOwnership(l.ctx, senderVerdict{Role: model.NormalizeRole(row.Role), Mid: mid}, roomID)
	if verr != nil {
		return nil, verr
	}
	role := verdict.Role

	// --- 8. 配额与 TTL ---
	eff, err := g.resolveQuota(l.ctx, model.QuotaScopeRoom, roomID)
	if err != nil {
		return nil, err
	}
	if mid == 0 && !eff.AllowGuest {
		return l.lgwRedeemDenial(g, roomID, mid, row, traceID, model.DropPermissionDenied,
			"guest reconnect not allowed by access quota"), nil
	}
	count, err := g.svcCtx.Leases.RoomConnectionCount(l.ctx, roomID, lgwScanLimit(g))
	if err != nil {
		return nil, fmt.Errorf("live-gateway: RedeemReconnectTicket 房间连接数读数不可用，无法做配额判定 room_id=%d: %w", roomID, err)
	}
	if eff.MaxConnections > 0 && count > eff.MaxConnections {
		return l.lgwRedeemDenial(g, roomID, mid, row, traceID, model.DropRateLimited,
			fmt.Sprintf("room connection quota exceeded (%d/%d)", count, eff.MaxConnections)), nil
	}
	ttl := g.clampLeaseTTL(in.GetTtlSeconds(), eff.LeaseTtlSeconds)

	// --- 10. request_id 幂等（只读校验全部通过后才占键）---
	replay, value, err := g.idempotentReplay(l.ctx, redeemRPCName, requestID)
	if err != nil {
		return nil, err
	}
	if replay {
		if prevID := strings.TrimSpace(value); prevID != "" {
			prev, gerr := g.svcCtx.Leases.Get(l.ctx, prevID)
			if gerr != nil {
				return nil, gerr
			}
			if prev != nil {
				g.infof("live-gateway: RedeemReconnectTicket 按 request_id 重放，回显首次签发的租约 lease_id=%s room_id=%d",
					safeIdent(prev.LeaseID), roomID)
				return l.lgwRedeemGranted(prev), nil
			}
		}
		g.infof("live-gateway: RedeemReconnectTicket request_id 命中的首次请求未留下租约，按未受理重新执行 room_id=%d ticket_id=%s",
			roomID, safeIdent(row.TicketId))
	}

	// --- 9a. Redis 原子消费有效位（一次性消费点）---
	hash := model.TicketHash(ticket)
	bitID, found, err := g.svcCtx.Leases.ConsumeTicket(l.ctx, hash)
	if err != nil {
		return nil, err
	}
	if !found {
		return l.lgwRedeemDenial(g, roomID, mid, row, traceID, model.DropBadTicket,
			"ticket validity bit already consumed, expired or dropped"), nil
	}
	if bitID != "" && bitID != row.TicketId {
		// 有效位与 DB 行不同源：Redis 键被第三方写入过，凭这张票签发租约就是拿一个来历不明的断言发凭据。
		g.errorf("live-gateway: 票据有效位与 DB 行不同源 room_id=%d mid=%d ticket_id=%s（有效位记 %s），拒绝兑换",
			roomID, mid, safeIdent(row.TicketId), safeIdent(bitID))
		return l.lgwRedeemDenial(g, roomID, mid, row, traceID, model.DropBadTicket,
			"ticket validity bit does not match the audit row"), nil
	}

	// --- 9b. 断线视图与宽限期内的订阅视图（先读，后写）---
	now := model.NowUnix()
	oldRec, err := g.leaseLookup(l.ctx, row.LeaseId)
	if err != nil && !errors.Is(err, model.ErrEmptyLeaseID) {
		return nil, err
	}
	offlineAt, missed, oerr := g.svcCtx.Leases.OfflineView(l.ctx, roomID, mid)
	if oerr != nil {
		g.errorf("live-gateway: 房间 %d 用户 %d 断线视图读数不可用，last_offline_at/missed_message_estimate 按 0 回显: %v",
			roomID, mid, oerr)
	}

	// --- 9c. 签发新租约 ---
	newRec := &repository.LeaseRecord{
		LeaseID:  newID("lgwl"),
		ConnID:   connID,
		NodeID:   nodeID,
		RoomID:   roomID,
		Mid:      mid,
		Role:     role,
		State:    model.LeaseStateActive,
		IssuedAt: now,
		ExpireAt: now + int64(ttl),
		TraceID:  traceID,
	}
	lgwCarryReconnectView(newRec, oldRec)
	saved, err := g.svcCtx.Leases.Acquire(l.ctx, newRec, ttl)
	if err != nil {
		return nil, fmt.Errorf("live-gateway: RedeemReconnectTicket 新租约签发失败 ticket_id=%s: %w",
			safeIdent(row.TicketId), err)
	}
	lgwReleaseStaleLease(l.ctx, g, oldRec, saved.LeaseID)

	// --- 9d. DB 消费（条件更新：state=ISSUED 且三元组吻合且未过期）---
	store, err := g.svcCtx.RequireStore()
	if err != nil {
		lgwCompensateRedeem(l.ctx, g, saved, row)
		return nil, err
	}
	aff, err := store.Tickets.Consume(l.ctx, row.TicketId, roomID, mid, saved.LeaseID, now)
	if err != nil {
		lgwCompensateRedeem(l.ctx, g, saved, row)
		return nil, err
	}
	if aff == 0 {
		// 并发下另一路已把这张票消费/撤销掉（或三元组在极短时间内变了）：新租约必须收回。
		lgwCompensateRedeem(l.ctx, g, saved, row)
		cur, ferr := store.Tickets.FindOne(l.ctx, row.TicketId)
		if ferr != nil {
			return nil, ferr
		}
		return l.lgwRedeemDenial(g, roomID, mid, cur, traceID, model.DropBadTicket,
			"ticket was consumed concurrently; the freshly issued lease was reclaimed"), nil
	}
	g.rememberIdempotent(l.ctx, redeemRPCName, requestID, saved.LeaseID)
	g.infof("live-gateway: 票据已兑换新租约 ticket_id=%s lease_id=%s room_id=%d mid=%d role=%d ttl=%d last_offline_at=%d missed≈%d",
		safeIdent(row.TicketId), safeIdent(saved.LeaseID), roomID, mid, role, ttl, offlineAt, missed)

	reply := l.lgwRedeemGranted(saved)
	reply.LastOfflineAt = offlineAt
	reply.MissedMessageEstimate = missed
	return reply, nil
}

// lgwRedeemGranted 组装 allowed=true 的回执。
// 票据位恒传空串：LeaseInfo.reconnect_ticket 只在「刚签发票据」的路径回显（conv.go leaseInfo 注释），
// 本方法刚消费掉一张票，不能再随租约发一张并不存在的新凭据。
func (l *RedeemReconnectTicketLogic) lgwRedeemGranted(rec *repository.LeaseRecord) *rpc.RedeemReconnectTicketReply {
	return &rpc.RedeemReconnectTicketReply{
		Allowed:     true,
		Lease:       leaseInfo(rec, ""),
		TicketState: ticketState(model.TicketStateUsed),
		DenyReason:  dropReason(model.DropOK),
	}
}

// lgwRedeemDenyByTicket 按票据原文回读该行，再交给 lgwRedeemDenial 给结论。
// verifyTicketCredential 在失败分支刻意不回传行（避免把他人票据的绑定关系回显给探测者），
// 所以这里按摘要自己读一次：只用于分类 ticket_state 与写越权审计，不回显任何身份字段。
func (l *RedeemReconnectTicketLogic) lgwRedeemDenyByTicket(g gate, ticket string, roomID, mid int64,
	traceID string, drop int32, detail string) (*rpc.RedeemReconnectTicketReply, error) {
	store, err := g.svcCtx.RequireStore()
	if err != nil {
		return nil, err
	}
	hash := model.TicketHash(ticket)
	row, ferr := store.Tickets.FindByHash(l.ctx, hash)
	if ferr != nil {
		return nil, ferr
	}
	if row != nil && row.RoomId != roomID {
		// 拿别人房间的票来换：审计与拒绝都用请求里的 room_id（被侵害的房间），不回显票据归属的房间。
		g.errorf("live-gateway: 票据跨房间使用被拒（ticket_id=%s 请求 room_id=%d mid=%d）", safeIdent(row.TicketId), roomID, mid)
	}
	return l.lgwRedeemDenial(g, roomID, mid, row, traceID, drop, detail), nil
}

// lgwRedeemDenial 唯一的拒绝出口：把「为什么没换成租约」翻译成 proto 要求的三个字段。
// ticket_state 一律给真实状态（proto RedeemReconnectTicketReply.ticket_state：USED/EXPIRED/REVOKED），
// 行都定位不到时才回 UNSPECIFIED —— 那不是「没查到所以没拒绝」，而是「这张票不在本服务的账上」。
func (l *RedeemReconnectTicketLogic) lgwRedeemDenial(g gate, roomID, mid int64, row *model.LiveGwReconnectTicket,
	traceID string, drop int32, detail string) *rpc.RedeemReconnectTicketReply {
	state := model.TicketStateUnspecified
	ref := "-"
	if row != nil {
		state = row.State
		ref = safeIdent(row.TicketId)
		switch row.State {
		case model.TicketStateRevoked:
			drop = model.DropPermissionDenied
			detail = "ticket revoked"
		case model.TicketStateUsed:
			detail = "ticket already used (one-time credential)"
		case model.TicketStateIssued:
			// 行还是 ISSUED 但被判拒：只剩「已过期」和「三元组不匹配」两种，前者要把回显状态收敛成 EXPIRED。
			if model.Expired(row.ExpireAt, model.NowUnix()) {
				state = model.TicketStateExpired
			}
		}
	}
	if drop == 0 {
		drop = model.DropBadTicket
	}
	if drop == model.DropPermissionDenied {
		// 三元组不匹配 / 已撤销票据的兑换尝试是明确的越权信号：先留痕再回结论。
		lgwAuditCredentialDenial(l.ctx, g, "ticket-redeem-denied", roomID, mid, ref, drop, traceID)
	}
	g.errorf("live-gateway: RedeemReconnectTicket 拒绝 room_id=%d mid=%d ticket=%s drop=%d %s",
		roomID, mid, ref, drop, safeIdent(detail))
	return &rpc.RedeemReconnectTicketReply{
		Allowed:     false,
		TicketState: ticketState(state),
		DenyReason:  dropReason(drop),
		DenyDetail:  safeIdent(detail),
	}
}

// lgwCarryReconnectView 复用断线宽限期内的订阅视图（config.ReconnectGraceSeconds 的既有约定）：
// 旧租约记录还在 Redis 里时，把 topics/joined_at 与脱敏客户端字段带到新租约，
// 让重连后的连接立刻按原订阅收消息，而不是「换到凭据却什么都收不到」。
// 旧记录不存在（键已回收）时保持空视图：客户端需要重新 JoinRoom，这是真实结论而不是降级。
func lgwCarryReconnectView(dst, src *repository.LeaseRecord) {
	if dst == nil || src == nil {
		return
	}
	if src.RoomID != dst.RoomID || src.Mid != dst.Mid {
		return // 陈旧的 lease_id 指向别的 (room, mid)：不能把别人的订阅视图接过来
	}
	dst.Topics = append([]string(nil), src.Topics...)
	dst.JoinedAt = src.JoinedAt
	dst.RenewCount = src.RenewCount + 1
	dst.DeviceIDHash = src.DeviceIDHash
	dst.Platform = src.Platform
	dst.AppVersion = src.AppVersion
	dst.HeartbeatCount = src.HeartbeatCount
}

// lgwReleaseStaleLease 客户端换了 conn_id 重连时，Acquire 的 (node,conn) 幂等键收不到旧租约，
// 旧记录会以 ACTIVE 留在房间集合里直到自身 TTL —— 那既占配额名额又出现在在线名单里。
// 票据本身就是「那条连接已经断了」的凭据，所以这里把它一并收口（终态记录不动，见 Release 第 3 步）。
func lgwReleaseStaleLease(ctx context.Context, g gate, old *repository.LeaseRecord, newLeaseID string) {
	if old == nil || old.LeaseID == newLeaseID || model.IsLeaseTerminal(old.State) {
		return
	}
	if err := g.svcCtx.Leases.Delete(ctx, old); err != nil {
		g.errorf("live-gateway: 重连收口旧租约 %s 失败（将在自身 TTL 后失效）: %v", safeIdent(old.LeaseID), err)
		return
	}
	g.infof("live-gateway: 重连已收口旧租约 %s（新租约 %s）", safeIdent(old.LeaseID), safeIdent(newLeaseID))
}

// lgwCompensateRedeem DB 消费未命中/失败时收回刚签发的租约。
// 收回失败只记告警：票据的 Redis 有效位已被 GETDEL 消费，「票据仍可用」在两条存储上都不成立，
// 悬空租约由自身 TTL 回收（最长 MaxLeaseTTLSeconds），把它说成「兑换成功」才是更糟的结果。
func lgwCompensateRedeem(ctx context.Context, g gate, rec *repository.LeaseRecord, row *model.LiveGwReconnectTicket) {
	if rec == nil {
		return
	}
	if err := g.svcCtx.Leases.Delete(ctx, rec); err != nil {
		g.errorf("live-gateway: 票据 %s 兑换回滚失败，租约 %s 由 TTL 回收: %v",
			safeIdent(row.TicketId), safeIdent(rec.LeaseID), err)
		return
	}
	g.errorf("live-gateway: 票据 %s 兑换未命中条件更新，已收回租约 %s", safeIdent(row.TicketId), safeIdent(rec.LeaseID))
}
