package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// revokeRPCName request_id 幂等窗口里本方法的键名。
const revokeRPCName = "RevokeReconnectTicket"

// revokeTicketIDMaxBytes ticket_id 的形状上限，与迁移 SQL 的 live_gw_reconnect_ticket.ticket_id
// VARCHAR(64) 对齐；本服务自己签发的 ID 是 "lgwt_"+ULID（31 字节），超长即畸形输入。
const revokeTicketIDMaxBytes = 64

type RevokeReconnectTicketLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRevokeReconnectTicketLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevokeReconnectTicketLogic {
	return &RevokeReconnectTicketLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 撤销票据（封禁、踢人、房间关闭）
func (l *RevokeReconnectTicketLogic) RevokeReconnectTicket(in *rpc.RevokeReconnectTicketReq) (*rpc.RevokeReconnectTicketReply, error) {
	// 已实现行为：
	// 1. 参数（proto RevokeReconnectTicketReq 注释）：ticket_id 与 (room_id, mid) 二选一定位，
	//    两者都缺 → ErrEmptyTicket（没有定位范围的撤销等于全表处置）；reason 必填（ErrEmptyReason，
	//    banned/kicked/room_closed/risk 都属处置留痕）；operator 必填（ErrEmptyOperator）；
	//    request_id 必填（处置幂等键）。给了 ticket_id 时若同时给了 room_id/mid，必须与该行登记值一致
	//    ——不一致就是「用别人的房间号给这次撤销记账」的越权信号，回 ErrTripletMismatch。
	// 2. 调用方授权：本方法是**处置类写接口**（config 注释与 caller.go 注释都把 RevokeReconnectTicket
	//    列在 KickConnection/DrainRoomRoute/UpsertAccessQuota 同一组），唯一依据是
	//    gate.requireDispositionWrite 认 gRPC metadata 归因的 OPERATOR/SERVICE，默认 fail-closed
	//    （AllowUnattestedOperatorWrites=false）。请求里的 operator 字段只是「谁登记的」标签，
	//    不作授权输入，未归因主体不能靠自报一个名字撤销他人凭据。被拒的撤销尝试先落 DENIED 审计再返回错误
	//    （同 kickconnectionlogic.go 第 2 步：不落库就无法复盘「谁试过撤谁的票」）。
	//    桩注释里「主播只能撤本房间、房管只能撤观众」的分层需要 live-room 提供房间角色查询，
	//    本服务的 RoomGate 只有 RoomOwner/IsOwner/Broadcastable（AGENTS.md §5 不复判他人领域），
	//    因此该分层在今天的唯一执行点就是本条 fail-closed 门禁 —— 没有放开任何未归因主体。
	// 3. 批量路径护栏：按 (room_id, mid) 撤销时 mid 必须 >0（model.RevokeByRoomMid 里已写死
	//    ErrInvalidMid）——「撤销该房间全部游客票据」的影响面远超一次封禁应有的范围。
	//    批量执行整体复用 lgwRevokeSubjectTickets（KickConnection 4a 步同一个函数）：
	//    先按 (room,mid,ISSUED) 枚举拿到 ticket_hash → RevokeByRoomMid 条件更新 → 逐条 DropTicket。
	// 4. 幂等：request_id 命中时回放首次结论（revoked/revoked_count/state 三个字段都回放），
	//    **零写入、零审计**；结论仍为 pending（首次请求崩在中途）时按未受理重跑 ——
	//    Revoke 是「state=ISSUED」的条件 UPDATE、DropTicket 是 DEL，两者都可安全重放，
	//    审计行的 message_id 带 request_id 摘要，撞 uniq_room_message 时 Insert 直接返回 (0,nil) 不重复留痕。
	//    重复撤销（不同 request_id 打同一张已 REVOKED 的票）按成功返回 revoked=true + count=0 +
	//    state=REVOKED，不报错也**不重复审计**（口径取自 drainroomroutelogic.go 第 5 步
	//    「已 DRAINING / 已幂等：按成功返回当前行，不报错也不重复审计」+ 本方法桩注释第 5 步）。
	// 5. 状态机：单张路径只推进 ISSUED→REVOKED（RevokeByTicketID 的 WHERE 已限定，
	//    与 model.IsValidTicketTransition 同向），返回 0 行时回读区分
	//    不存在（ErrTicketNotFound，没有真实状态可回显）/ 已 USED（撤销已消费的票无意义，
	//    revoked=false + state=USED，并提示应改用 KickConnection 断连接）/ 已 EXPIRED（同上不报错）/
	//    已 REVOKED（幂等成功）。绝不把 USED 强行改写成 REVOKED。
	// 6. 同步失效 Redis：DB 推进一步就 DropTicket 删有效位（Redis 才是「能不能换」的事实源，
	//    只清 DB 会让撤销延迟到票据自身 TTL 才生效）；有效位删除失败只记告警——
	//    行已 REVOKED，兑换的条件更新打不中，凭据实际已失效（同 lgwRevokeSubjectTickets 的口径）。
	// 7. 与 KickConnection 的边界：本方法只撤销票据，**不**释放租约、**不**写禁止重连窗口
	//    （撤销与断开是两件事，运营只想断票时必须能单独调用；桩注释第 7 步）。
	// 8. 审计：一次真实推进（count>0）写一行 live_gw_broadcast_log（kind=MODERATION、state=SENT、
	//    sender 是归因主体而不是请求字段），message_id="ticket-revoke:<原因短标>:<request_id 摘要>"。
	//    撤销不是逐连接高频事件，处置留痕按 AGENTS.md §8 必须落库，这与 JoinRoom/LeaveRoom
	//    「不写逐连接审计行」的分工不矛盾。
	// 9. 错误映射：参数非法/依赖故障/未授权返回 error；「无事可做」的幂等重复走响应体
	//    （revoked=false 时 state 仍回显真实状态，不回显 UNSPECIFIED 让调用方以为没查到）。
	if in == nil {
		return nil, model.ErrEmptyTicket
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	ticketID := strings.TrimSpace(in.GetTicketId())
	roomID, mid := in.GetRoomId(), in.GetMid()
	reason := strings.TrimSpace(in.GetReason())
	operator := strings.TrimSpace(in.GetOperator())
	requestID := strings.TrimSpace(in.GetRequestId())
	traceID := strings.TrimSpace(in.GetTraceId())

	// --- 1. 参数与定位 ---
	if ticketID == "" && roomID <= 0 && mid <= 0 {
		return nil, fmt.Errorf("%w: revoke requires ticket_id or (room_id, mid)", model.ErrEmptyTicket)
	}
	if ticketID != "" {
		if len(ticketID) > revokeTicketIDMaxBytes || strings.ContainsAny(ticketID, " \t\r\n") {
			return nil, fmt.Errorf("%w: ticket_id must be 1..%d bytes without whitespace",
				model.ErrEmptyTicket, revokeTicketIDMaxBytes)
		}
	} else {
		if err := g.requireRoomID(roomID); err != nil {
			return nil, err
		}
		if mid <= 0 {
			return nil, fmt.Errorf("%w: 批量撤销要求 mid > 0，mid=0 会撤掉该房间**全部游客**的票据，"+
				"影响面远超一次封禁应有的范围；请按 ticket_id 精确撤销", model.ErrInvalidMid)
		}
	}
	if err := g.requireReason(reason); err != nil {
		return nil, err
	}
	if err := g.requireOperatorField(operator); err != nil {
		return nil, err
	}
	if err := g.requireRequestID(requestID); err != nil {
		return nil, err
	}

	// --- 2. 处置授权（fail-closed）---
	c, err := g.requireDispositionWrite(revokeRPCName)
	if err != nil {
		lgwAuditTicketRevokeAttempt(l.ctx, g, roomID, ticketID, requestID, reason, c,
			model.BroadcastLogDenied, model.DropPermissionDenied, traceID)
		g.errorf("live-gateway: RevokeReconnectTicket 被拒 caller=%s ticket_id=%s room_id=%d mid=%d reason=%s",
			c.String(), safeIdent(ticketID), roomID, mid, safeIdent(reason))
		return nil, err
	}
	store, err := g.svcCtx.RequireStore()
	if err != nil {
		return nil, err
	}

	// --- 4. request_id 幂等（授权之后、任何写入之前）---
	replay, value, err := g.idempotentReplay(l.ctx, revokeRPCName, requestID)
	if err != nil {
		return nil, err
	}
	if replay {
		if res, derr := decodeRevokeResult(value); derr == nil {
			g.infof("live-gateway: RevokeReconnectTicket 按 request_id 重放，回放首次结论不重复撤销不重复审计"+
				"（ticket_id=%s room_id=%d mid=%d count=%d）", safeIdent(ticketID), roomID, mid, res.count)
			return res.reply(), nil
		}
		g.infof("live-gateway: RevokeReconnectTicket 首次请求结论未回填（幂等键仍是 pending），按未受理重新执行 ticket_id=%s",
			safeIdent(ticketID))
	}

	// --- 3/5/6. 执行 ---
	var (
		count int32
		state int32
	)
	if ticketID != "" {
		row, ferr := store.Tickets.FindOne(l.ctx, ticketID)
		if ferr != nil {
			return nil, ferr
		}
		if row == nil {
			// 没有这一行就没有「真实状态」可回显，按桩注释第 9 步走错误而不是 revoked=false+UNSPECIFIED。
			return nil, fmt.Errorf("%w: ticket_id=%s", model.ErrTicketNotFound, safeIdent(ticketID))
		}
		if roomID > 0 && row.RoomId != roomID {
			lgwAuditTicketRevokeAttempt(l.ctx, g, roomID, ticketID, requestID, reason, c,
				model.BroadcastLogDenied, model.DropPermissionDenied, traceID)
			return nil, fmt.Errorf("%w: ticket belongs to room %d, revoke asked for room %d",
				model.ErrTripletMismatch, row.RoomId, roomID)
		}
		if mid > 0 && row.Mid != mid {
			lgwAuditTicketRevokeAttempt(l.ctx, g, row.RoomId, ticketID, requestID, reason, c,
				model.BroadcastLogDenied, model.DropPermissionDenied, traceID)
			return nil, fmt.Errorf("%w: ticket belongs to mid %d, revoke asked for mid %d",
				model.ErrTripletMismatch, row.Mid, mid)
		}
		aff, rerr := store.Tickets.RevokeByTicketID(l.ctx, ticketID, reason, operator)
		if rerr != nil {
			return nil, rerr
		}
		switch {
		case aff > 0:
			count, state = 1, model.TicketStateRevoked
			lgwDropTicketBit(l.ctx, g, row)
		default:
			// 0 行：回读定位真实原因（条件更新可能刚被并发对手推走）。
			cur, cerr := store.Tickets.FindOne(l.ctx, ticketID)
			if cerr != nil {
				return nil, cerr
			}
			if cur == nil {
				return nil, fmt.Errorf("%w: ticket_id=%s disappeared during revoke", model.ErrTicketNotFound, safeIdent(ticketID))
			}
			state = cur.State
			count = 0
			if state == model.TicketStateRevoked {
				// 并发/重复撤销：按幂等成功回显，不报错、不重复审计（见第 4/5 步）。
				lgwDropTicketBit(l.ctx, g, cur)
				g.infof("live-gateway: 票据 %s 已被撤销（reason=%s by=%s），本次按幂等成功返回",
					safeIdent(ticketID), safeIdent(cur.RevokeReason), safeIdent(cur.RevokedBy))
			} else {
				g.errorf("live-gateway: 票据 %s 当前 state=%d 不可撤销（只允许 ISSUED→REVOKED），撤销按无操作返回",
					safeIdent(ticketID), state)
			}
		}
		roomID = row.RoomId
	} else {
		n, rerr := lgwRevokeSubjectTickets(l.ctx, g, roomID, mid, reason, operator, lgwScanLimit(g))
		if rerr != nil {
			if errors.Is(rerr, model.ErrInvalidMid) {
				return nil, fmt.Errorf("%w: 批量撤销要求 mid > 0（model.RevokeByRoomMid 的同一条约束）", rerr)
			}
			return nil, rerr
		}
		count = n
		if count > 0 {
			state = model.TicketStateRevoked
		} else {
			// 该主体此刻没有可撤销的票：回读最近一行的真实状态回显（proto 要求 revoked=false 也要给状态）。
			rows, _, lerr := store.Tickets.List(l.ctx, model.TicketFilter{RoomId: roomID, Mid: mid, Pn: 1, Ps: 1})
			if lerr != nil {
				return nil, lerr
			}
			if len(rows) == 0 || rows[0] == nil {
				return nil, fmt.Errorf("%w: room_id=%d mid=%d 没有任何票据记录", model.ErrTicketNotFound, roomID, mid)
			}
			state = rows[0].State
			g.infof("live-gateway: room_id=%d mid=%d 无未使用票据可撤销，回显最近一张 state=%d", roomID, mid, state)
		}
	}

	res := revokeResult{revoked: count > 0 || state == model.TicketStateRevoked, count: count, state: state}
	// --- 8. 审计：只有真实推进才留痕，重复撤销不重复审计（第 4/5 步同一条约束）---
	if count > 0 {
		lgwAuditTicketRevokeAttempt(l.ctx, g, roomID, ticketID, requestID, reason, c,
			model.BroadcastLogSent, model.DropOK, traceID)
	}
	g.rememberIdempotent(l.ctx, revokeRPCName, requestID, res.encode())
	g.infof("live-gateway: 撤销重连票据完成 caller=%s operator=%s room_id=%d mid=%d ticket_id=%s count=%d state=%d reason=%s",
		c.String(), safeIdent(operator), roomID, mid, safeIdent(ticketID), count, state, safeIdent(reason))
	return res.reply(), nil
}

// revokeResult 一次撤销的结论（request_id 幂等回放用）。三个字段都要回放：
// 只回放 count 会让重放时的 state 变成 UNSPECIFIED，违反 proto「回显真实状态」的要求。
type revokeResult struct {
	revoked bool
	count   int32
	state   int32
}

func (r revokeResult) encode() string {
	flag := 0
	if r.revoked {
		flag = 1
	}
	return strconv.Itoa(flag) + "|" + strconv.FormatInt(int64(r.count), 10) + "|" + strconv.FormatInt(int64(r.state), 10)
}

func decodeRevokeResult(v string) (revokeResult, error) {
	parts := strings.Split(strings.TrimSpace(v), "|")
	if len(parts) != 3 {
		return revokeResult{}, fmt.Errorf("malformed revoke conclusion %q", safeIdent(v))
	}
	flag, err := strconv.Atoi(parts[0])
	if err != nil {
		return revokeResult{}, err
	}
	count, err := strconv.ParseInt(parts[1], 10, 32)
	if err != nil {
		return revokeResult{}, err
	}
	state, err := strconv.ParseInt(parts[2], 10, 32)
	if err != nil {
		return revokeResult{}, err
	}
	return revokeResult{revoked: flag == 1, count: int32(count), state: int32(state)}, nil
}

func (r revokeResult) reply() *rpc.RevokeReconnectTicketReply {
	return &rpc.RevokeReconnectTicketReply{
		Revoked:      r.revoked,
		RevokedCount: r.count,
		State:        ticketState(r.state),
	}
}

// lgwDropTicketBit 删掉 Redis 里的票据有效位。
// 失败只记告警：DB 行已是 REVOKED，兑换的条件更新（WHERE state=ISSUED）打不中，
// 凭据在真值层面已经失效；把已生效的撤销伪装成失败会诱使运营去动连接本身。
func lgwDropTicketBit(ctx context.Context, g gate, row *model.LiveGwReconnectTicket) {
	if row == nil || row.TicketHash == "" {
		return
	}
	if err := g.svcCtx.Leases.DropTicket(ctx, row.TicketHash); err != nil {
		g.errorf("live-gateway: 票据 %s 有效位删除失败（DB 已 REVOKED，兑换仍会失败）: %v",
			safeIdent(row.TicketId), err)
	}
}

// lgwAuditTicketRevokeAttempt 把一次撤销（或被拒的撤销尝试）写进 live_gw_broadcast_log。
//
// message_id = "ticket-revoke:<原因短标>:<request_id|票据定位符 的摘要>"：同一 request_id 重放同一定位符
// 时撞 uniq_room_message，Insert 直接返回 (0,nil)，处置留痕只有一行；
// 把定位符算进摘要，是为了让「一个 request_id 撤两张不同票据」留下两行而不是被静默收敛掉
// （复用 lgwReasonSlug/payloadDigest，与 KickConnection 的审计同一套收敛手法）。
// sender_mid/source 取**归因主体**：请求里的 operator 是调用方自报的标签，不能替它背书。
// 审计写失败只记告警：票据行的 revoked_reason/revoked_by 已经是留痕，
// 不能因为审计表不可用就把已生效的撤销说成失败。
func lgwAuditTicketRevokeAttempt(ctx context.Context, g gate, roomID int64, ticketID, requestID string,
	reason string, c caller, state, drop int32, traceID string) {
	auditRoom := roomID
	if auditRoom <= 0 {
		// 只给了 ticket_id 时回读该行拿到真实房间号再记账：审计行没有 room_id 就查不出来。
		if store, err := g.svcCtx.RequireStore(); err == nil {
			if row, ferr := store.Tickets.FindOne(ctx, ticketID); ferr == nil && row != nil {
				auditRoom = row.RoomId
			}
		}
	}
	if auditRoom <= 0 {
		g.errorf("live-gateway: 撤销票据审计跳过（无法确定 room_id）ticket_id=%s state=%d drop=%d",
			safeIdent(ticketID), state, drop)
		return
	}
	locator := strings.TrimSpace(ticketID)
	if locator == "" {
		locator = fmt.Sprintf("room%d", auditRoom)
	}
	messageID := "ticket-revoke:" + lgwReasonSlug(reason) + ":" +
		payloadDigest([]byte(strings.TrimSpace(requestID)+"|"+locator), 16)
	if err := g.writeAudit(ctx, auditParams{
		RoomID:     auditRoom,
		Kind:       model.KindModeration,
		MessageID:  messageID,
		SenderMid:  lgwKickAuditMid(c),
		SenderRole: c.role,
		Payload:    []byte(reason + "|" + locator),
		State:      state,
		Drop:       drop,
		Source:     lgwKickActorLabel(c),
		TraceID:    traceID,
	}); err != nil {
		g.errorf("live-gateway: 撤销票据审计写入失败 room_id=%d message_id=%s: %v", auditRoom, safeIdent(messageID), err)
	}
}
