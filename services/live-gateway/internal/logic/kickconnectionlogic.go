package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// kickRPCName request_id 幂等窗口里本方法的键名（与 proto 的 rpc 名一致，避免与广播族撞键）。
const kickRPCName = "KickConnection"

// kickTicketPageCap 一次 Kick 能枚举并删除 Redis 有效位的票据条数上限。
// model 的 TicketFilter.List 走 clampPage（上限 MaxPageSize），所以这里取「扫描上限与该夹取值的较小值」，
// 多出来的 ISSUED 票仍会被 MySQL 置 REVOKED（兑换必失败），只是有效位要等自身 TTL 回收。
const kickTicketPageCap = 50

type KickConnectionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewKickConnectionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *KickConnectionLogic {
	return &KickConnectionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 强制下线（风控/审核/主播踢人），可同时撤销票据与写禁止重连窗口
func (l *KickConnectionLogic) KickConnection(in *rpc.KickConnectionReq) (*rpc.KickConnectionReply, error) {
	// 实现要点（处置类接口，判定比下发更严）：
	// 1. 参数：room_id>0；mid 与 lease_id 至少一个（都没有=影响面全房间，直接拒）；reason 必填
	//    （ErrEmptyReason）、operator 必填（ErrEmptyOperator）、request_id 必填（处置幂等键）、
	//    ban_seconds>=0。conn_id 非空时只踢单条连接。
	// 2. 处置授权：g.requireDispositionWrite 是唯一依据——只认 gRPC metadata 归因的
	//    OPERATOR/SERVICE，请求里的 operator/claimed 角色一律不作授权输入。未归因主体默认拒绝
	//    （AllowUnattestedOperatorWrites=false），被拒的踢人尝试落 state=DENIED 审计。
	// 3. 目标定位：lease_id 与 (room_id, mid)/conn_id 不一致 → ErrTripletMismatch + 越权审计，
	//    绝不按「更宽松的那个」执行；按 mid 定位时扫描量撞 ConnectionScanLimit 就报错
	//    （只踢一半人却宣称踢完是假结论）。
	// 4. 执行顺序（先断票再断连，反序会让被踢端拿票秒级重连回来）：
	//    a) revoke_tickets=true：先枚举 ISSUED 票拿到 hash → RevokeByRoomMid（条件更新，只推
	//       ISSUED→REVOKED）→ 逐条 DropTicket 删 Redis 有效位（只改 DB 会让兑换快路径继续放行）；
	//    b) ban_seconds>0 时写 ban 窗口，取 max(既有窗口, now+ban_seconds)：已生效的封禁绝不回退；
	//       键维度由 reason 决定（banned/risk 走用户维度，其余走房间维度 = 影响面更小的默认）；
	//    c) 目标租约置 KICKED（终态，IsValidLeaseTransition 保证之后不可复活）并用
	//       Unsubscribe 清订阅集合与房间计数，墓碑记录保留供 GetConnectionLease/Renew 解释；
	//    d) CloseConnections 关掉物理连接：未接线只把结论降级成
	//       DROP_REASON_TRANSPORT_UNAVAILABLE，a/b/c 的失效写入已经完成且可解释。
	// 5. 幂等：同一 request_id 重放回放首次结论（不重复计数、不延长封禁、不重复撤票）；
	//    已是终态的租约不计入 kicked_connections。
	// 6. ban_until 回显与 Redis 一致（0 = 不禁止重连）。
	// 7. 审计：kicked/dropped/denied 都落 live_gw_broadcast_log（kind=MODERATION，
	//    message_id = "kick:"+原因短标+request_id 摘要 → 重放撞唯一键，不会灌满审计表）。
	// 8. 商业化边界：无付费直播/投币/会员相关处置分支（AGENTS.md §1）。
	// 9. 错误映射：越权 → kicked=false + deny_reason（不报错，避免上游把越权当依赖故障重试）；
	//    参数非法/依赖故障 → 返回 error。
	if in == nil {
		return nil, model.ErrEmptyReason
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	roomID, mid := in.GetRoomId(), in.GetMid()
	leaseID := strings.TrimSpace(in.GetLeaseId())
	connID := strings.TrimSpace(in.GetConnId())
	reason := strings.TrimSpace(in.GetReason())
	if err := g.requireRoomID(roomID); err != nil {
		return nil, err
	}
	if err := g.requireMid(mid); err != nil {
		return nil, err
	}
	if leaseID == "" && mid <= 0 {
		return nil, fmt.Errorf("%w: kick requires lease_id or mid > 0, an empty locator would hit the whole room",
			model.ErrEmptyLeaseID)
	}
	if err := g.requireReason(reason); err != nil {
		return nil, err
	}
	if err := g.requireOperatorField(in.GetOperator()); err != nil {
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
	if in.GetBanSeconds() < 0 {
		return nil, fmt.Errorf("live-gateway: ban_seconds must be >= 0, got %d", in.GetBanSeconds())
	}

	// --- 2. 处置授权（fail-closed）---
	c, err := g.requireDispositionWrite(kickRPCName)
	if err != nil {
		if !errors.Is(err, model.ErrPermissionDenied) {
			return nil, err
		}
		// 被拒的踢人尝试本身是风控信号：先留痕再回结论（不落库就不能复盘「谁试过踢谁」）。
		lgwAuditKickAttempt(l.ctx, g, "kick-denied", roomID, in, c, model.BroadcastLogDenied,
			model.DropPermissionDenied)
		g.errorf("live-gateway: KickConnection 被拒 caller=%s room_id=%d mid=%d lease_id=%s",
			c.String(), roomID, mid, safeIdent(leaseID))
		return &rpc.KickConnectionReply{Kicked: false, DenyReason: dropReason(model.DropPermissionDenied)}, nil
	}

	// --- 3. 目标定位（只读；先定位再占幂等键，避免纯校验失败就把 request_id 烧掉）---
	limit := lgwScanLimit(g)
	var targets []*repository.LeaseRecord
	if leaseID != "" {
		rec, lerr := g.leaseLookup(l.ctx, leaseID)
		if lerr != nil {
			return nil, lerr
		}
		if rec != nil {
			if detail := lgwKickLocatorGuard(rec, roomID, mid, connID); detail != "" {
				lgwAuditCredentialDenial(l.ctx, g, "kick-denied", rec.RoomID, mid, rec.LeaseID,
					model.DropPermissionDenied, in.GetTraceId())
				return nil, fmt.Errorf("%w: %s", model.ErrTripletMismatch, detail)
			}
			targets = append(targets, rec)
		}
	} else {
		recs, lerr := g.svcCtx.Leases.ListUserLeases(l.ctx, roomID, mid, limit)
		if lerr != nil {
			return nil, lerr
		}
		if lgwPossiblyTruncated(int32(len(recs)), limit) {
			return nil, fmt.Errorf("%w: kick target set hit ConnectionScanLimit=%d for room_id=%d mid=%d",
				model.ErrScanLimitTooLarge, limit, roomID, mid)
		}
		for _, rec := range recs {
			if rec == nil || rec.RoomID != roomID || rec.Mid != mid {
				continue // 陈旧集合成员：不属于本 (room, mid) 的连接不在处置影响面里
			}
			if connID != "" && rec.ConnID != connID {
				continue
			}
			targets = append(targets, rec)
		}
	}
	// 处置主体的 mid：lease_id 定位时用租约登记值，避免把封禁写到错误的用户上。
	subjectMid := mid
	if subjectMid <= 0 && len(targets) > 0 {
		subjectMid = targets[0].Mid
	}

	// --- 5. request_id 幂等：命中即回放首次结论（放在只读定位之后，见上）---
	replay, value, err := g.idempotentReplay(l.ctx, kickRPCName, in.GetRequestId())
	if err != nil {
		return nil, err
	}
	if replay {
		res, derr := decodeKickResult(value)
		if derr != nil {
			// 首次请求的结论还没回填（崩在执行中途）：宁可变通重试，也不伪造一个「成功」给调用方。
			return nil, fmt.Errorf("%w: previous KickConnection conclusion not recorded yet, retry same request_id: %v",
				model.ErrRequestIdDuplicated, derr)
		}
		return res.reply(), nil
	}

	// --- 4a. 先断票（MySQL 状态 + Redis 有效位）---
	var revoked int32
	if in.GetRevokeTickets() {
		if subjectMid <= 0 {
			g.errorf("live-gateway: KickConnection 跳过批量撤票：游客（mid=0）票据按 (room,mid) 撤销" +
				"影响面是整个房间的游客，须走 RevokeReconnectTicket 按 ticket_id 精确撤销")
		} else {
			n, rerr := lgwRevokeSubjectTickets(l.ctx, g, roomID, subjectMid, reason, in.GetOperator(), limit)
			if rerr != nil {
				return nil, rerr
			}
			revoked = n
		}
	}

	// --- 4b. 禁止重连窗口 ---
	now := model.NowUnix()
	banUntil, err := lgwReadBanUntil(l.ctx, g, roomID, subjectMid)
	if err != nil {
		return nil, err
	}
	if in.GetBanSeconds() > 0 {
		if subjectMid <= 0 {
			return nil, fmt.Errorf("%w: ban window requires mid > 0, a guest ban key would cover every guest of the room",
				model.ErrInvalidMid)
		}
		if want := now + int64(in.GetBanSeconds()); want > banUntil {
			banUntil = want // 只延长不缩短：已生效的封禁不能被一次「短封禁」冲掉
		}
		if serr := g.svcCtx.Leases.SetBan(l.ctx, roomID, subjectMid, banUntil, !lgwBanIsUserScoped(reason)); serr != nil {
			return nil, serr
		}
	}

	// --- 4c. 租约置 KICKED 并清订阅集合（先失效，再断物理连接）---
	var kickedConns int32
	closeByNode := make(map[string][]string, 2)
	for _, rec := range targets {
		if model.IsLeaseTerminal(rec.State) {
			continue // 重复 Kick 不重复计入 kicked_connections
		}
		// Unsubscribe 把连接摘出房间/用户集合并清 topics（内部会重读记录并回写），
		// 所以 KICKED 的写入必须排在它之后，否则终态会被它写回的旧记录冲掉。
		if _, uerr := g.svcCtx.Leases.Unsubscribe(l.ctx, rec.LeaseID, rec.RoomID, rec.Mid, nil); uerr != nil {
			return nil, uerr
		}
		if model.IsValidLeaseTransition(rec.State, model.LeaseStateKicked) {
			rec.State = model.LeaseStateKicked
			rec.Topics = nil
			if perr := g.svcCtx.Leases.Put(l.ctx, rec); perr != nil {
				return nil, perr
			}
		} else {
			// model.leaseTransitions 里没有 EXPIRED→KICKED 这条边，本方法不放宽状态机：
			// 连接已摘出房间集合，复活由 ban_until 窗口挡住（Renew 也查该窗口），处置效果一致。
			g.errorf("live-gateway: 租约 %s 状态 %d 无 →KICKED 的状态机边，已仅摘出订阅集合；"+
				"复活由 ban_until 窗口拦截", safeIdent(rec.LeaseID), rec.State)
		}
		kickedConns++
		if rec.NodeID != "" {
			closeByNode[rec.NodeID] = append(closeByNode[rec.NodeID], rec.LeaseID)
		}
	}

	// --- 4d. 关闭物理连接（未接线不撤销已完成的失效写入）---
	drop := model.DropOK
	if len(closeByNode) > 0 && !g.svcCtx.Fanout.Available() {
		g.errorf("live-gateway: KickConnection 接入层未接线，%d 条连接的凭据已失效但 socket 仍可能开着",
			kickedConns)
	}
	for nodeID, ids := range closeByNode {
		if _, cerr := g.svcCtx.Fanout.CloseConnections(l.ctx, nodeID, ids, reason); cerr != nil {
			if !errors.Is(cerr, model.ErrTransportUnavailable) {
				g.errorf("live-gateway: CloseConnections node=%s 失败: %v", safeIdent(nodeID), cerr)
			}
			// 结论降级而不是报错：a/b/c 已经生效，心跳/续租/下发三条路径都会把这条连接判死，
			// 「下发失败」不是把已完成的处置重新说成失败的理由。
			drop = model.DropTransportUnavailable
		}
	}

	// --- 结论：没有任何东西被处置时如实回 false + 可解释原因 ---
	kicked := kickedConns > 0 || in.GetBanSeconds() > 0 || revoked > 0
	switch {
	case kicked:
	case len(targets) == 0:
		drop = model.DropBadTicket // 租约键已被回收：目标当时就不在线
	case drop == model.DropOK:
		drop = model.DropNoSubscriber // 有连接但都是终态记录，没有可断开的目标
	}
	// 审计状态口径：只有「全部生效且连接已关闭」才记 SENT，
	// 其余（通道未接线 / 目标当时不在线 / 无可断开目标）一律 DROPPED，drop_reason 说明是哪一种。
	state := model.BroadcastLogDropped
	if drop == model.DropOK {
		state = model.BroadcastLogSent
	}

	res := kickResult{kicked: kicked, kickedConns: kickedConns, revoked: revoked, banUntil: banUntil, drop: drop}
	// --- 7. 审计（先落审计再回填幂等结论：结论可回放的前提是这次处置留了痕）---
	lgwAuditKickAttempt(l.ctx, g, "kick", roomID, in, c, state, drop)
	g.rememberIdempotent(l.ctx, kickRPCName, in.GetRequestId(), res.encode())
	if !kicked {
		g.errorf("live-gateway: KickConnection 未产生处置 room_id=%d mid=%d lease_id=%s drop=%d",
			roomID, subjectMid, safeIdent(leaseID), drop)
	}
	return res.reply(), nil
}

// kickResult 一次 Kick 的结论（幂等回放用）。drop 一并存着：
// 回放必须回到同一份可解释结论（kicked=false 时也要说清当时是「目标不在线」还是「无可断开目标」）。
type kickResult struct {
	kicked      bool
	kickedConns int32
	revoked     int32
	banUntil    int64
	drop        int32
}

// encode 把结论压成 5 段竖线串存进 request_id 幂等窗口。
// 存结论而不是存「发生过」标记：重放要给回同一份结论（ban_until 不回退也不延长）。
func (r kickResult) encode() string {
	b := 0
	if r.kicked {
		b = 1
	}
	return strings.Join([]string{
		strconv.Itoa(b),
		strconv.FormatInt(int64(r.kickedConns), 10),
		strconv.FormatInt(int64(r.revoked), 10),
		strconv.FormatInt(r.banUntil, 10),
		strconv.FormatInt(int64(r.drop), 10),
	}, "|")
}

func decodeKickResult(v string) (kickResult, error) {
	parts := strings.Split(strings.TrimSpace(v), "|")
	if len(parts) != 5 {
		return kickResult{}, fmt.Errorf("malformed kick conclusion %q", safeIdent(v))
	}
	flag, err := strconv.Atoi(parts[0])
	if err != nil {
		return kickResult{}, err
	}
	conns, err := strconv.ParseInt(parts[1], 10, 32)
	if err != nil {
		return kickResult{}, err
	}
	tickets, err := strconv.ParseInt(parts[2], 10, 32)
	if err != nil {
		return kickResult{}, err
	}
	ban, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return kickResult{}, err
	}
	drop, err := strconv.ParseInt(parts[4], 10, 32)
	if err != nil {
		return kickResult{}, err
	}
	return kickResult{
		kicked:      flag == 1,
		kickedConns: int32(conns),
		revoked:     int32(tickets),
		banUntil:    ban,
		drop:        int32(drop),
	}, nil
}

func (r kickResult) reply() *rpc.KickConnectionReply {
	return &rpc.KickConnectionReply{
		Kicked:            r.kicked,
		KickedConnections: r.kickedConns,
		RevokedTickets:    r.revoked,
		BanUntil:          r.banUntil,
		DenyReason:        dropReason(r.drop),
	}
}

// lgwKickLocatorGuard lease_id 与请求里的 (room_id, mid, conn_id) 的一致性判定，
// 返回非空字符串即越权描述。绝不「按更宽松的那个字段执行」。
func lgwKickLocatorGuard(rec *repository.LeaseRecord, roomID, mid int64, connID string) string {
	if rec == nil {
		return ""
	}
	if rec.RoomID != roomID {
		return fmt.Sprintf("lease belongs to room %d, kick issued for room %d", rec.RoomID, roomID)
	}
	if mid > 0 && rec.Mid != mid {
		return fmt.Sprintf("lease belongs to mid %d, kick issued for mid %d", rec.Mid, mid)
	}
	if connID != "" && rec.ConnID != connID {
		return "conn_id does not match the lease"
	}
	return ""
}

// lgwBanIsUserScoped 封禁维度由 reason 决定（README：ban_until 只在 Redis）：
// banned/risk 是「人」的问题，跨房间生效；room_closed/anchor_block 是「这个房间」的处置。
// 未知原因走房间维度 —— 影响面更小的那一个是默认，而不是「跨全站」。
func lgwBanIsUserScoped(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "banned", "ban", "risk", "risk_control":
		return true
	default:
		return false
	}
}

// lgwReadBanUntil 读取既有的禁止重连窗口（回显与 Redis 一致）。
// 游客（mid<=0）没有封禁键（repository 明确拒绝），直接回 0 表示「不禁止重连」。
func lgwReadBanUntil(ctx context.Context, g gate, roomID, mid int64) (int64, error) {
	if mid <= 0 {
		return 0, nil
	}
	ban, err := g.svcCtx.Leases.BanUntil(ctx, roomID, mid)
	if err != nil {
		return 0, err
	}
	return ban, nil
}

// lgwRevokeSubjectTickets 撤销该主体在该房间的全部未使用票据，并删掉 Redis 有效位。
//
// 顺序是「先枚举拿 hash → MySQL 条件更新 → 删有效位」：List 只能读到 ISSUED 的行，
// 一旦先 UPDATE 成 REVOKED 就再也取不到 ticket_hash，有效位会残留到自然过期。
// 有效位删除失败只记日志：MySQL 行已是 REVOKED，兑换的条件更新打不中，凭据实际已失效。
func lgwRevokeSubjectTickets(ctx context.Context, g gate, roomID, mid int64,
	reason, operator string, scanLimit int32) (int32, error) {
	store, err := g.svcCtx.RequireStore()
	if err != nil {
		return 0, err
	}
	ps := scanLimit
	if ps > kickTicketPageCap {
		ps = kickTicketPageCap
	}
	issued, total, lerr := store.Tickets.List(ctx, model.TicketFilter{
		RoomId: roomID, Mid: mid, State: model.TicketStateIssued, Pn: 1, Ps: ps,
	})
	if lerr != nil {
		return 0, lerr
	}
	if int(total) > len(issued) {
		// 超过一页的 ISSUED 票只出现在「票据风暴」里（MaxTicketsPerSubject 本该挡住）。
		// 批量撤销仍会覆盖它们（RevokeByRoomMid 的 LIMIT 独立），残留的是有效位，故显式告警。
		g.errorf("live-gateway: room_id=%d mid=%d 有 %d 张未使用票据，超过本次枚举的 %d 张，"+
			"多出的有效位由自身 TTL 回收（DB 已是 REVOKED，兑换必失败）", roomID, mid, total, len(issued))
	}
	n, rerr := store.Tickets.RevokeByRoomMid(ctx, roomID, mid, reason, operator, scanLimit)
	if rerr != nil {
		return 0, rerr
	}
	for _, row := range issued {
		if row == nil || row.TicketHash == "" {
			continue
		}
		if derr := g.svcCtx.Leases.DropTicket(ctx, row.TicketHash); derr != nil {
			g.errorf("live-gateway: 票据 %s 有效位删除失败（DB 已 REVOKED，兑换仍会失败）: %v",
				safeIdent(row.TicketId), derr)
		}
	}
	return strInt(n), nil
}

// lgwAuditKickAttempt 把一次 Kick（含被拒的尝试）落进 live_gw_broadcast_log。
//
// kind=MODERATION、sender 是**归因主体**而不是请求字段：metadata 里的 service/mid 优先，
// 未归因时记 unattested，这样审计里不会出现「自报的运营名字为处置背书」。
// message_id = "kick:<原因短标>:<request_id 摘要>"：同一 request_id 重放撞 uniq_room_message，
// 处置留痕只有一行；不同 request_id 的两条处置各留一行（这是两次独立的权力行使）。
func lgwAuditKickAttempt(ctx context.Context, g gate, scene string, roomID int64,
	in *rpc.KickConnectionReq, c caller, state, drop int32) {
	if roomID <= 0 || in == nil {
		return
	}
	digest := payloadDigest([]byte(strings.TrimSpace(in.GetRequestId())), 16)
	messageID := scene + ":" + lgwReasonSlug(in.GetReason()) + ":" + digest
	err := g.writeAudit(ctx, auditParams{
		RoomID:     roomID,
		Kind:       model.KindModeration,
		MessageID:  messageID,
		SenderMid:  lgwKickAuditMid(c),
		SenderRole: c.role,
		Payload:    []byte(strings.TrimSpace(in.GetReason())),
		State:      state,
		Drop:       drop,
		Source:     lgwKickActorLabel(c),
		TraceID:    in.GetTraceId(),
	})
	if err != nil {
		// 处置已生效，审计写不进去必须被看见（AGENTS.md §8：处置留痕不是可选项）。
		g.errorf("live-gateway: KickConnection 审计写入失败 room_id=%d message_id=%s: %v", roomID, messageID, err)
	}
}

// lgwKickAuditMid 审计里的发起主体：只有 metadata 归因出的 mid 才算数，
// 否则记 0（系统或未归因），绝不把被踢者的 mid 写成发起者。
func lgwKickAuditMid(c caller) int64 {
	if c.attested && c.mid > 0 {
		return c.mid
	}
	return 0
}

// lgwKickActorLabel 审计的 source_service：归因主体标识，未归因时明确写 unattested。
func lgwKickActorLabel(c caller) string {
	if !c.isPrivileged() {
		return "unattested"
	}
	if c.service != "" {
		return "service:" + c.service
	}
	if c.mid > 0 {
		return "operator:" + strconv.FormatInt(c.mid, 10)
	}
	return "operator"
}

// lgwReasonSlug 把自由文本 reason 收敛成能进 message_id 的短标识（只留字母数字下划线，最长 16 字节）。
// reason 原文不进 message_id：那是索引列，也是运营肉眼读的「为什么」，混进任意文本会让审计不可查。
func lgwReasonSlug(reason string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(reason)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
		if b.Len() >= 16 {
			break
		}
	}
	if b.Len() == 0 {
		return "custom"
	}
	return b.String()
}
