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

// issueTicketReason 本方法签发的票据场景标记（proto ReconnectTicketInfo.issue_reason：normal/reconnect/room_switch）。
// IssueReconnectTicketReq 里没有场景字段，而这条 RPC 存在的唯一目的就是断线重连，
// 所以固定写 reconnect：留成空串会让审计里「为什么发这张票」变成谜。
const issueTicketReason = "reconnect"

type IssueReconnectTicketLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewIssueReconnectTicketLogic(ctx context.Context, svcCtx *svc.ServiceContext) *IssueReconnectTicketLogic {
	return &IssueReconnectTicketLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 以现有租约为凭据签发一次性重连票据
func (l *IssueReconnectTicketLogic) IssueReconnectTicket(in *rpc.IssueReconnectTicketReq) (*rpc.ReconnectTicketInfo, error) {
	// 已实现行为：
	// 1. 参数：lease_id 非空（ErrEmptyLeaseID：票据必须以租约为凭据，防止无凭据批量索票）、
	//    room_id>0、mid>=0、request_id 必填（ErrEmptyRequestID，签发幂等键）。
	// 2. 凭据校验（gate.leaseLookup + tripleMatch + 终态/时效，与 JoinRoom/LeaveRoom 同一批构件）：
	//    键不存在 → ErrLeaseNotFound；(room_id, mid) 不符 → 一行 DENIED 审计 + ErrTripletMismatch；
	//    KICKED → ErrLeaseKicked；RELEASED → ErrLeaseNotFound（该凭据已不存在）；
	//    EffectiveState==EXPIRED → ErrLeaseExpired。
	//    禁止为将死租约发票据：否则等于用一张票把过期连接的存活期无限续上。
	// 3. 禁止重连窗口：KickConnection 写入的 ban_until 未到 → ErrReconnectBanned，
	//    被封禁的连接不能靠票据绕过断开（读数复用 lgwReadBanUntil）。
	// 4. TTL：gate.clampTicketTTL(请求值, 配额 ROOM 层的 ticket_ttl_seconds)，
	//    夹进 [1, LiveGateway.MaxTicketTTLSeconds]；配额解析失败**不回退默认值**（helpers.resolveQuota 的约束）。
	//    作用域取 ROOM：票据是「回到这个房间」的凭据，房间层是它与 Acquire/Join 共同的配额层。
	// 5. 签名器不可用（Signer.Available()==false，即 MissingSigner）时**在任何写入之前**显式失败
	//    （ErrSignerMissing），绝不签发不签名票据，也不退化成固定 key（proto ReconnectTicketInfo.ticket
	//    注释「HMAC 签名，不含明文身份信息」+ svc.Notes 的同一条承诺）。
	// 6. 签发本体复用 gate.issueTicket（与 Acquire 第 8 步同一条路径，不造平行实现）：
	//    newID("lgwt") → Signer.Sign(claims) → Tickets.Insert（只写 model.TicketHash 摘要，
	//    票据原文永不进 MySQL 也不进 Redis 值）→ Leases.PutTicket(ticket_hash → ticket_id, TTL=票据剩余秒)。
	//    role/conn_id/node_id 一律取**租约登记值**（服务端判定结果），不接受调用方重新声明角色。
	// 7. 幂等：issueTicket 内已按 request_id 先回读（FindByRequestID）并用行内断言重算签名回放
	//    （rebuildTicket 会校验重算串的摘要与行内 ticket_hash 同源，换密钥后拒绝回放），
	//    并发下 Insert 撞 ErrRequestIdDuplicated 也只回读那一张，绝不产生第二张票。
	//    因此本方法不再叠加 Redis 幂等窗口：DB 的 uniq(request_id) 是更强的那一道。
	// 8. 防「票据风暴」：issueTicket 内按 MaxTicketsPerSubject 判 CountUnusedByRoomMid，超限 ErrQuotaExceeded。
	// 9. 审计：live_gw_reconnect_ticket 那一行本身就是签发审计（who/when/凭哪张租约/trace_id），
	//    不再往 live_gw_broadcast_log 写逐连接事件行（README 数据分层禁止项）。
	// 10. 错误映射：本方法返回体没有 allowed 字段，不存在「返回空票据当成功」的合法路径，
	//    参数/越权/依赖故障一律返回 error；成功时 ticket 只在这里回显一次。
	if in == nil {
		return nil, model.ErrEmptyLeaseID
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	leaseID := strings.TrimSpace(in.GetLeaseId())
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
	if err := g.requireRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}

	// --- 5. 签名器前置门禁（早于任何 DB 写入，避免留下签不出去的 ISSUED 行）---
	if !g.svcCtx.Signer.Available() {
		return nil, fmt.Errorf("%w: refusing to issue an unsigned reconnect ticket for room_id=%d mid=%d",
			repository.ErrSignerMissing, roomID, mid)
	}

	// --- 2. 凭据校验 ---
	rec, err := g.leaseLookup(l.ctx, leaseID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("%w: lease_id=%s，票据必须以现有租约为凭据", model.ErrLeaseNotFound, safeIdent(leaseID))
	}
	if !tripleMatch(rec, roomID, mid) {
		lgwAuditCredentialDenial(l.ctx, g, "ticket-issue-denied", roomID, mid, leaseID, model.DropPermissionDenied, traceID)
		g.errorf("live-gateway: IssueReconnectTicket 越权：租约属于 (room %d, mid %d)，请求的是 (room %d, mid %d)",
			rec.RoomID, rec.Mid, roomID, mid)
		return nil, fmt.Errorf("%w: ticket credential belongs to (room %d, mid %d)",
			model.ErrTripletMismatch, rec.RoomID, rec.Mid)
	}
	switch {
	case rec.State == model.LeaseStateKicked:
		return nil, fmt.Errorf("%w: lease_id=%s", model.ErrLeaseKicked, safeIdent(leaseID))
	case model.IsLeaseTerminal(rec.State):
		return nil, fmt.Errorf("%w: lease_id=%s already released", model.ErrLeaseNotFound, safeIdent(leaseID))
	}
	now := model.NowUnix()
	if rec.EffectiveState(now) == model.LeaseStateExpired {
		return nil, fmt.Errorf("%w: lease_id=%s expire_at=%d now=%d", model.ErrLeaseExpired, safeIdent(leaseID), rec.ExpireAt, now)
	}

	// --- 3. 禁止重连窗口 ---
	if ban, berr := lgwReadBanUntil(l.ctx, g, roomID, mid); berr != nil {
		return nil, berr
	} else if ban > now {
		g.errorf("live-gateway: IssueReconnectTicket 被禁止重连窗口拦下 room_id=%d mid=%d ban_until=%d", roomID, mid, ban)
		return nil, fmt.Errorf("%w: room_id=%d mid=%d until=%d", model.ErrReconnectBanned, roomID, mid, ban)
	}

	// --- 4. TTL（请求值 → 配额值 → 配置默认，夹进 [1, MaxTicketTTLSeconds]）---
	eff, err := g.resolveQuota(l.ctx, model.QuotaScopeRoom, roomID)
	if err != nil {
		return nil, err
	}
	ttl := g.clampTicketTTL(in.GetTtlSeconds(), eff.TicketTtlSeconds)

	// --- 6. 签发（role/conn/node 取租约登记值，不接受客户端重新声明）---
	row, ticket, err := g.issueTicket(l.ctx, ticketRequest{
		RoomID:     roomID,
		Mid:        mid,
		ConnID:     rec.ConnID,
		NodeID:     rec.NodeID,
		Role:       rec.Role,
		TTLSeconds: ttl,
		RequestID:  strings.TrimSpace(in.GetRequestId()),
		LeaseID:    rec.LeaseID,
		Reason:     issueTicketReason,
		TraceID:    traceID,
	})
	if err != nil {
		return nil, err
	}
	g.infof("live-gateway: 已签发重连票据 ticket_id=%s room_id=%d mid=%d lease_id=%s ttl=%d role=%d",
		safeIdent(row.TicketId), roomID, mid, safeIdent(rec.LeaseID), ttl, row.Role)
	return ticketInfo(row, ticket), nil
}
