package logic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/live-gateway/internal/config"
	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"
)

// 本文件覆盖重连票据族三个方法（Issue / Redeem / Revoke）。
// 这一族是本服务里唯一「凭据 ↔ 连接」互换的地方，风险最高的三条：
//  1. 用别人的票换自己的租约（越权收益最大）；
//  2. 一张票换两次租约（一次性消费没做干净）；
//  3. 密钥没注入时静默签发/放行（等于发不发凭据都一样）。
// 所以断言全部围绕「结论必须来自真实判定」：allowed=false 要给出准确的 drop 与票据真实状态，
// 依赖故障必须是 error 而不是降级成功，任何拒绝路径都必须零租约、零票据行。

// --- 布置脚手架 ---

// tkSeed 描述一行「与真实签名同源」的票据记录。
// 直接建行而不是走 Issue，是为了能构造 Issue 产生不了的初始状态（USED/REVOKED/过期/外来签名）。
type tkSeed struct {
	ticketID  string
	room      int64
	user      int64
	guest     bool // 显式游客票（mid=0）；不写则默认落在测试主体 mid 上
	connID    string
	nodeID    string
	leaseID   string
	role      int32
	state     int32
	ttl       int
	requestID string
	bit       bool // 是否同步写 Redis 有效位（兑换的前置条件）
}

// seedTicket 用测试签名器签出原文，并按生产同一规则（model.TicketHash）落摘要行。
func (e *testEnv) seedTicket(s tkSeed) (*model.LiveGwReconnectTicket, string) {
	e.t.Helper()
	if s.ticketID == "" {
		s.ticketID = newID("lgwt")
	}
	if s.room == 0 {
		s.room = roomID
	}
	if !s.guest && s.user == 0 {
		s.user = mid
	}
	if s.role == 0 {
		s.role = model.RoleViewer
	}
	if s.ttl == 0 {
		s.ttl = 120
	}
	now := e.Clock.unix()
	claims := repository.TicketClaims{
		TicketID: s.ticketID, RoomID: s.room, Mid: s.user, ConnID: s.connID,
		Role: s.role, ExpireAt: now + int64(s.ttl),
	}
	ticket, err := e.Svc.Signer.Sign(claims)
	if err != nil {
		e.t.Fatalf("签名测试票据失败: %v", err)
	}
	row := &model.LiveGwReconnectTicket{
		Id: int64(len(e.Ticket.rows) + 1), TicketId: s.ticketID, TicketHash: model.TicketHash(ticket),
		RoomId: s.room, Mid: s.user, ConnId: s.connID, NodeId: s.nodeID, Role: s.role,
		State: s.state, IssuedAt: now, ExpireAt: claims.ExpireAt, LeaseId: s.leaseID,
		RequestId: s.requestID, IssueReason: ticketReasonNormal,
	}
	if row.State == 0 {
		row.State = model.TicketStateIssued
	}
	e.Ticket.rows = append(e.Ticket.rows, row)
	if s.bit {
		if perr := e.Leases.PutTicket(context.Background(), row.TicketHash, row.TicketId, s.ttl); perr != nil {
			e.t.Fatalf("预置票据有效位失败: %v", perr)
		}
	}
	return row, ticket
}

func issueReq(leaseID, requestID string) *rpc.IssueReconnectTicketReq {
	return &rpc.IssueReconnectTicketReq{LeaseId: leaseID, RoomId: roomID, Mid: mid, RequestId: requestID}
}

func redeemReq(ticket, connID, requestID string) *rpc.RedeemReconnectTicketReq {
	return &rpc.RedeemReconnectTicketReq{
		Ticket: ticket, RoomId: roomID, Mid: mid, ConnId: connID, NodeId: nodeA, RequestId: requestID,
	}
}

func revokeReq(requestID string) *rpc.RevokeReconnectTicketReq {
	return &rpc.RevokeReconnectTicketReq{
		RoomId: roomID, Mid: mid, Reason: "banned", Operator: "ops-1001", RequestId: requestID,
	}
}

// mustIssueTicket 走真实签发路径拿一张可用票据（Redeem 的正常前置，保证串/行/Redis 位三者同源）。
func mustIssueTicket(t *testing.T, e *testEnv, leaseID, requestID string) (string, *rpc.ReconnectTicketInfo) {
	t.Helper()
	info, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(issueReq(leaseID, requestID))
	if err != nil {
		t.Fatalf("签发重连票据失败: %v", err)
	}
	return info.GetTicket(), info
}

// assertNoTicketLeak 断言票据原文既不进 MySQL 行也不进 Redis 值（README 安全约束）。
func assertNoTicketLeak(t *testing.T, e *testEnv, ticket string) {
	t.Helper()
	for _, row := range e.Ticket.all() {
		if strings.Contains(row.TicketId, ticket) || strings.Contains(row.ConnId, ticket) ||
			row.TicketHash == ticket {
			t.Fatalf("票据原文出现在 DB 行里: ticket_id=%s", row.TicketId)
		}
	}
	for hash, id := range e.Leases.tickets {
		if id == ticket || hash == ticket {
			t.Fatalf("票据原文出现在 Redis 值里: %s", id)
		}
	}
}

func revokedRow(t *testing.T, e *testEnv, ticketID string) *model.LiveGwReconnectTicket {
	t.Helper()
	row, err := e.Ticket.FindOne(context.Background(), ticketID)
	if err != nil {
		t.Fatalf("回读票据行失败: %v", err)
	}
	if row == nil {
		t.Fatalf("票据行 %s 不存在", ticketID)
	}
	return row
}

// --- Issue：参数门禁 ---

func TestIssueReconnectTicketParameterGate(t *testing.T) {
	longID := strings.Repeat("r", 65)
	cases := []struct {
		name string
		req  *rpc.IssueReconnectTicketReq
		want error
	}{
		{"空请求", nil, model.ErrEmptyLeaseID},
		{"缺少租约凭据", &rpc.IssueReconnectTicketReq{RoomId: roomID, Mid: mid, RequestId: "r1"}, model.ErrEmptyLeaseID},
		{"租约凭据全空格", &rpc.IssueReconnectTicketReq{LeaseId: "   ", RoomId: roomID, Mid: mid, RequestId: "r1"}, model.ErrEmptyLeaseID},
		{"房间号非正", &rpc.IssueReconnectTicketReq{LeaseId: "lgwl_1", RoomId: 0, Mid: mid, RequestId: "r1"}, model.ErrInvalidRoomID},
		{"mid 为负", &rpc.IssueReconnectTicketReq{LeaseId: "lgwl_1", RoomId: roomID, Mid: -1, RequestId: "r1"}, model.ErrInvalidMid},
		{"缺少幂等键", &rpc.IssueReconnectTicketReq{LeaseId: "lgwl_1", RoomId: roomID, Mid: mid, RequestId: "  "}, model.ErrEmptyRequestID},
		{"幂等键超长", &rpc.IssueReconnectTicketReq{LeaseId: "lgwl_1", RoomId: roomID, Mid: mid, RequestId: longID}, model.ErrEmptyRequestID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			_, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(tc.req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("应回 %v，实际 %v", tc.want, err)
			}
			if e.Ticket.inserts != 0 || len(e.Ticket.rows) != 0 {
				t.Fatalf("参数非法不得留下票据行，实际插入 %d 次", e.Ticket.inserts)
			}
			if e.Leases.callCount("Get") != 0 {
				t.Fatalf("参数非法不得读租约")
			}
		})
	}
}

// --- Issue：签名器门禁必须早于任何写入 ---

func TestIssueReconnectTicketSignerGatePrecedesEveryWrite(t *testing.T) {
	e := newTestEnv(t)
	e.seedActiveLease("lgwl_src", "c-src", nodeA, roomID, mid, model.RoleViewer)
	e.markSignerMissing()

	_, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(issueReq("lgwl_src", "r-signer"))
	if !errors.Is(err, repository.ErrSignerMissing) {
		t.Fatalf("密钥未注入应回 ErrSignerMissing，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "unsigned") {
		t.Fatalf("错误须说明拒绝签发未签名票据，实际 %v", err)
	}
	// 「早于任何写入」的实际含义：连租约都没读，票据行与 Redis 位都没有。
	if e.Leases.callCount("Get") != 0 {
		t.Fatalf("签名器门禁必须先于凭据校验，实际读了 %d 次租约", e.Leases.callCount("Get"))
	}
	if e.Ticket.inserts != 0 || len(e.Ticket.rows) != 0 || len(e.Leases.tickets) != 0 {
		t.Fatalf("签名器不可用时不得留下票据行或有效位")
	}
	if len(e.Logs.rows) != 0 {
		t.Fatalf("签发失败不是越权，不得写审计，实际 %d 行", len(e.Logs.rows))
	}
}

// --- Issue：凭据校验 ---

func TestIssueReconnectTicketCredentialGate(t *testing.T) {
	t.Run("租约不存在", func(t *testing.T) {
		e := newTestEnv(t)
		_, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(issueReq("lgwl_none", "r-1"))
		if !errors.Is(err, model.ErrLeaseNotFound) {
			t.Fatalf("应回 ErrLeaseNotFound，实际 %v", err)
		}
		if e.Ticket.inserts != 0 {
			t.Fatalf("无凭据不得发票")
		}
	})

	t.Run("三元组不符回越权并留审计", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_src", "c-src", nodeA, roomID, mid, model.RoleViewer)
		req := issueReq("lgwl_src", "r-2")
		req.RoomId = otherRoom // 拿别人房间的租约索票
		_, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(req)
		if !errors.Is(err, model.ErrTripletMismatch) {
			t.Fatalf("应回 ErrTripletMismatch，实际 %v", err)
		}
		if e.Ticket.inserts != 0 {
			t.Fatalf("越权索票不得发出票据")
		}
		rows := e.Logs.all()
		if len(rows) != 1 {
			t.Fatalf("越权索票应落 1 行审计，实际 %d 行", len(rows))
		}
		if rows[0].State != model.BroadcastLogDenied || rows[0].Kind != model.KindModeration ||
			rows[0].DropReason != model.DropPermissionDenied {
			t.Fatalf("审计行应为 MODERATION/DENIED/PERMISSION_DENIED，实际 %+v", rows[0])
		}
		if !strings.HasPrefix(rows[0].MessageId, "ticket-issue-denied:lgwl_src:") {
			t.Fatalf("审计 message_id 应带场景与租约定位符，实际 %s", rows[0].MessageId)
		}
	})

	t.Run("被踢连接不能再索票", func(t *testing.T) {
		e := newTestEnv(t)
		e.Leases.seedLease(&repository.LeaseRecord{LeaseID: "lgwl_k", ConnID: "c-k", NodeID: nodeA, RoomID: roomID,
			Mid: mid, Role: model.RoleViewer, State: model.LeaseStateKicked, IssuedAt: e.Clock.unix() - 10,
			ExpireAt: e.Clock.unix() + 20})
		if _, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(issueReq("lgwl_k", "r-3")); !errors.Is(err, model.ErrLeaseKicked) {
			t.Fatalf("应回 ErrLeaseKicked，实际 %v", err)
		}
	})

	t.Run("已释放租约视同不存在", func(t *testing.T) {
		e := newTestEnv(t)
		e.Leases.seedLease(&repository.LeaseRecord{LeaseID: "lgwl_r", ConnID: "c-r", NodeID: nodeA, RoomID: roomID,
			Mid: mid, Role: model.RoleViewer, State: model.LeaseStateReleased, IssuedAt: e.Clock.unix() - 10,
			ExpireAt: e.Clock.unix() + 20})
		if _, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(issueReq("lgwl_r", "r-4")); !errors.Is(err, model.ErrLeaseNotFound) {
			t.Fatalf("终态 RELEASED 的凭据已不存在，应回 ErrLeaseNotFound，实际 %v", err)
		}
	})

	t.Run("将死租约不得续命", func(t *testing.T) {
		e := newTestEnv(t)
		e.Leases.seedLease(&repository.LeaseRecord{LeaseID: "lgwl_e", ConnID: "c-e", NodeID: nodeA, RoomID: roomID,
			Mid: mid, Role: model.RoleViewer, State: model.LeaseStateActive, IssuedAt: e.Clock.unix() - 90,
			ExpireAt: e.Clock.unix() - 1})
		if _, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(issueReq("lgwl_e", "r-5")); !errors.Is(err, model.ErrLeaseExpired) {
			t.Fatalf("过期租约应回 ErrLeaseExpired（禁止用票据把存活期无限续上），实际 %v", err)
		}
	})

	t.Run("Redis 读数失败原样上抛", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_src", "c-src", nodeA, roomID, mid, model.RoleViewer)
		e.Leases.failWith("Get", repository.ErrStoreUnavailable)
		if _, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(issueReq("lgwl_src", "r-6")); !errors.Is(err, repository.ErrStoreUnavailable) {
			t.Fatalf("依赖故障不得翻译成「租约不存在」，实际 %v", err)
		}
	})
}

// --- Issue：禁止重连窗口 ---

func TestIssueReconnectTicketBanWindow(t *testing.T) {
	t.Run("封禁未到期拒绝发票", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_src", "c-src", nodeA, roomID, mid, model.RoleViewer)
		if err := e.Leases.SetBan(context.Background(), roomID, mid, e.Clock.unix()+60, true); err != nil {
			t.Fatalf("预置封禁失败: %v", err)
		}
		_, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(issueReq("lgwl_src", "r-ban"))
		if !errors.Is(err, model.ErrReconnectBanned) {
			t.Fatalf("应回 ErrReconnectBanned，实际 %v", err)
		}
		if e.Ticket.inserts != 0 {
			t.Fatalf("被封禁的连接不能靠票据绕过断开")
		}
	})

	t.Run("封禁已到期可发票", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_src", "c-src", nodeA, roomID, mid, model.RoleViewer)
		if err := e.Leases.SetBan(context.Background(), roomID, mid, e.Clock.unix()-1, true); err != nil {
			t.Fatalf("预置封禁失败: %v", err)
		}
		if _, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(issueReq("lgwl_src", "r-expired-ban")); err != nil {
			t.Fatalf("已过期的封禁不该继续拦： %v", err)
		}
	})

	t.Run("全局封禁同样命中房间维度读数", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_src", "c-src", nodeA, roomID, mid, model.RoleViewer)
		if err := e.Leases.SetBan(context.Background(), 0, mid, e.Clock.unix()+120, false); err != nil {
			t.Fatalf("预置全局封禁失败: %v", err)
		}
		if _, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(issueReq("lgwl_src", "r-global")); !errors.Is(err, model.ErrReconnectBanned) {
			t.Fatalf("跨房间封禁应取 room/user 两个键的较大值，实际 %v", err)
		}
	})

	t.Run("封禁读数不可用不得当作未封禁", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_src", "c-src", nodeA, roomID, mid, model.RoleViewer)
		e.Leases.failWith("BanUntil", repository.ErrStoreUnavailable)
		if _, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(issueReq("lgwl_src", "r-banerr")); !errors.Is(err, repository.ErrStoreUnavailable) {
			t.Fatalf("读数故障必须上抛而不是放行，实际 %v", err)
		}
	})
}

// --- Issue：TTL 夹取与配额 ---

func TestIssueReconnectTicketTTLClamp(t *testing.T) {
	cases := []struct {
		name        string
		key         string
		requested   int32
		quotaTicket int32
		want        int64
	}{
		{"请求值优先", "a", 30, 90, 30},
		{"请求值为 0 用配额 ROOM 层", "b", 0, 90, 90},
		{"请求值为负用配额", "c", -1, 90, 90},
		{"无配额行用配置默认 120", "d", 0, 0, 120},
		{"超上限夹到 MaxTicketTTLSeconds", "e", 99999, 0, 600},
		{"下限 1 秒不被夹成 0", "f", 1, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			if tc.quotaTicket > 0 {
				e.Quotas = newFakeQuotas(&model.LiveGwAccessQuota{
					Id: 1, Scope: model.QuotaScopeRoom, ScopeId: roomID, TicketTtlSeconds: tc.quotaTicket, Version: 1,
				})
				e.Svc.Store.Quotas = e.Quotas
			}
			e.seedActiveLease("lgwl_ttl", "c-ttl", nodeA, roomID, mid, model.RoleViewer)
			req := issueReq("lgwl_ttl", "r-ttl-"+tc.key)
			req.TtlSeconds = tc.requested
			info, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(req)
			if err != nil {
				t.Fatalf("签发失败: %v", err)
			}
			if got := info.GetExpireAt() - info.GetIssuedAt(); got != tc.want {
				t.Fatalf("票据有效期应为 %d 秒，实际 %d 秒", tc.want, got)
			}
			row := revokedRow(t, e, info.GetTicketId())
			if row.ExpireAt-row.IssuedAt != tc.want {
				t.Fatalf("落库的 expire_at 与回显不一致：库 %d 回显 %d", row.ExpireAt-row.IssuedAt, tc.want)
			}
		})
	}
}

func TestIssueReconnectTicketQuotaResolveFailurePropagates(t *testing.T) {
	e := newTestEnv(t)
	e.seedActiveLease("lgwl_src", "c-src", nodeA, roomID, mid, model.RoleViewer)
	e.Quotas.fail("Resolve", repository.ErrStoreUnavailable)
	e.Svc.Store.Quotas = e.Quotas
	if _, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(issueReq("lgwl_src", "r-q")); !errors.Is(err, repository.ErrStoreUnavailable) {
		t.Fatalf("配额解析失败不得回退默认值，实际 %v", err)
	}
	if e.Ticket.inserts != 0 {
		t.Fatalf("解析失败时不得发票")
	}
}

// --- Issue：正常路径 ---

func TestIssueReconnectTicketHappyPath(t *testing.T) {
	e := newTestEnv(t)
	e.seedActiveLease("lgwl_src", "c-src", nodeA, roomID, mid, model.RoleViewer)
	traceID := "trace-issue-1"
	req := issueReq("lgwl_src", "r-happy")
	req.TraceId = traceID

	info, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(req)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	ticketID := assertTicketShape(t, info.GetTicket())
	if ticketID != info.GetTicketId() {
		t.Fatalf("回显的 ticket_id 与票据串不同源: %s vs %s", info.GetTicketId(), ticketID)
	}
	if info.GetRoomId() != roomID || info.GetMid() != mid || info.GetConnId() != "c-src" || info.GetNodeId() != nodeA {
		t.Fatalf("票据绑定关系应取自租约: %+v", info)
	}
	if info.GetState() != rpc.TicketState_TICKET_STATE_ISSUED || info.GetUsedAt() != 0 {
		t.Fatalf("新票应为 state=ISSUED/used_at=0: %+v", info)
	}
	if info.GetIssueReason() != "reconnect" {
		t.Fatalf("本方法签发的票据场景应固定为 reconnect，实际 %q", info.GetIssueReason())
	}
	if info.GetTraceId() != traceID {
		t.Fatalf("trace_id 未随票据留痕: %+v", info)
	}

	row := revokedRow(t, e, info.GetTicketId())
	if row.Role != model.RoleViewer || row.State != model.TicketStateIssued {
		t.Fatalf("行内角色/状态不对: %+v", row)
	}
	if row.LeaseId != "lgwl_src" {
		t.Fatalf("票据必须记下凭据租约，实际 %q", row.LeaseId)
	}
	if row.TicketHash != model.TicketHash(info.GetTicket()) {
		t.Fatalf("行内摘要必须是票据原文的 sha256")
	}
	if !e.Leases.hasTicketBit(row.TicketHash) || e.Leases.tickets[row.TicketHash] != row.TicketId {
		t.Fatalf("Redis 有效位必须写入且值只是 ticket_id")
	}
	assertNoTicketLeak(t, e, info.GetTicket())
	if len(e.Logs.rows) != 0 {
		t.Fatalf("签发不写逐连接审计行（票据行本身就是审计），实际 %d 行", len(e.Logs.rows))
	}
	if e.Leases.callCount("Acquire") != 0 || e.Leases.callCount("Put") != 0 {
		t.Fatalf("签票不得动连接")
	}
}

func TestIssueReconnectTicketRoleComesFromLease(t *testing.T) {
	e := newTestEnv(t)
	e.Rooms.owner = anchorMid
	e.seedActiveLease("lgwl_a", "c-a", nodeA, roomID, anchorMid, model.RoleAnchor)
	req := &rpc.IssueReconnectTicketReq{LeaseId: "lgwl_a", RoomId: roomID, Mid: anchorMid, RequestId: "r-role"}

	info, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(req)
	if err != nil {
		t.Fatalf("为主播租约签票失败: %v", err)
	}
	if info.GetRole() != rpc.ConnRole_CONN_ROLE_ANCHOR {
		t.Fatalf("票据角色应沿用租约登记值 ANCHOR，实际 %v", info.GetRole())
	}
	if e.Rooms.ownerCalls != 0 {
		t.Fatalf("签票阶段不得复判主播归属（那是 live-room 的领域），实际问了 %d 次", e.Rooms.ownerCalls)
	}
	// 游客凭据同样能索票：allow_guest 只约束「换连接」，不约束「要票」。
	e2 := newTestEnv(t)
	e2.seedActiveLease("lgwl_g", "c-g", nodeA, roomID, 0, model.RoleViewer)
	gInfo, err := NewIssueReconnectTicketLogic(ctxClient(t), e2.Svc).IssueReconnectTicket(
		&rpc.IssueReconnectTicketReq{LeaseId: "lgwl_g", RoomId: roomID, Mid: 0, RequestId: "r-guest"})
	if err != nil || gInfo.GetMid() != 0 {
		t.Fatalf("游客租约应能签票: err=%v info=%+v", err, gInfo)
	}
	if e2.Leases.callCount("BanUntil") != 0 {
		t.Fatalf("mid=0 没有封禁维度可读，不应白问 Redis")
	}
}

// --- Issue：幂等与票据风暴 ---

func TestIssueReconnectTicketRequestIDReplay(t *testing.T) {
	e := newTestEnv(t)
	e.seedActiveLease("lgwl_src", "c-src", nodeA, roomID, mid, model.RoleViewer)

	first, info1 := mustIssueTicket(t, e, "lgwl_src", "r-replay")
	puts1 := e.Leases.callCount("PutTicket")
	if puts1 != 1 {
		t.Fatalf("首次签发应写 1 次有效位，实际 %d", puts1)
	}
	second, info2 := mustIssueTicket(t, e, "lgwl_src", "r-replay")
	if first != second {
		t.Fatalf("同 request_id 必须回放同一张票据原文")
	}
	if info1.GetTicketId() != info2.GetTicketId() || info1.GetExpireAt() != info2.GetExpireAt() {
		t.Fatalf("回放应给同一行: %+v vs %+v", info1, info2)
	}
	if e.Ticket.inserts != 1 || len(e.Ticket.rows) != 1 {
		t.Fatalf("幂等回放不得产生第二张票，实际 %d 行", len(e.Ticket.rows))
	}
	if got := e.Leases.callCount("PutTicket"); got != puts1 {
		t.Fatalf("回放不得再写有效位，实际 %d 次", got)
	}

	// 换 request_id 才是一张小新的票（同一把凭据租约可以有多张未使用票据，受上限约束）。
	third, info3 := mustIssueTicket(t, e, "lgwl_src", "r-replay-2")
	if third == first || info3.GetTicketId() == info1.GetTicketId() {
		t.Fatalf("不同 request_id 应签新票")
	}
	if len(e.Ticket.rows) != 2 {
		t.Fatalf("应有 2 行票据，实际 %d", len(e.Ticket.rows))
	}
}

func TestIssueReconnectTicketReplayRefusedAfterKeyRotation(t *testing.T) {
	e := newTestEnv(t)
	e.seedActiveLease("lgwl_src", "c-src", nodeA, roomID, mid, model.RoleViewer)

	other, err := repository.NewSigner("another-unit-test-only-hmac-key-32b", "LIVEGW_OTHER_TICKET_KEY")
	if err != nil {
		t.Fatalf("构造第二把密钥失败: %v", err)
	}
	now := e.Clock.unix()
	claims := repository.TicketClaims{TicketID: "lgwt_oldkey", RoomID: roomID, Mid: mid, ConnID: "c-src", Role: model.RoleViewer, ExpireAt: now + 120}
	oldTicket, err := other.Sign(claims)
	if err != nil {
		t.Fatalf("用旧密钥签票失败: %v", err)
	}
	e.Ticket.rows = append(e.Ticket.rows, &model.LiveGwReconnectTicket{
		Id: 1, TicketId: claims.TicketID, TicketHash: model.TicketHash(oldTicket), RoomId: roomID, Mid: mid,
		ConnId: claims.ConnID, NodeId: nodeA, Role: claims.Role, State: model.TicketStateIssued,
		IssuedAt: now, ExpireAt: claims.ExpireAt, LeaseId: "lgwl_src", RequestId: "r-rotate",
		IssueReason: ticketReasonNormal,
	})

	_, err = NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(issueReq("lgwl_src", "r-rotate"))
	if !errors.Is(err, model.ErrTicketNotFound) {
		t.Fatalf("换密钥后应拒绝回放，回 ErrTicketNotFound，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "replay refused") {
		t.Fatalf("错误须说明是回放被拒，实际 %v", err)
	}
	if e.Ticket.inserts != 0 || len(e.Ticket.rows) != 1 {
		t.Fatalf("拒绝回放时不得偷偷补发一张新票（那等于用新密钥复活旧凭据）")
	}
}

func TestIssueReconnectTicketStormCap(t *testing.T) {
	e := newTestEnv(t)
	e.seedActiveLease("lgwl_src", "c-src", nodeA, roomID, mid, model.RoleViewer)
	for i := 0; i < 5; i++ {
		e.seedTicket(tkSeed{room: roomID, user: mid, connID: "c-src", nodeID: nodeA, leaseID: "lgwl_src"})
	}
	_, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(issueReq("lgwl_src", "r-storm"))
	if !errors.Is(err, model.ErrQuotaExceeded) {
		t.Fatalf("达到 MaxTicketsPerSubject 应回 ErrQuotaExceeded，实际 %v", err)
	}
	if e.Ticket.inserts != 0 {
		t.Fatalf("超上限不得再插入票据")
	}

	t.Run("上限为 0 表示不限制", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_src", "c-src", nodeA, roomID, mid, model.RoleViewer)
		for i := 0; i < 5; i++ {
			e.seedTicket(tkSeed{room: roomID, user: mid, connID: "c-src", nodeID: nodeA, leaseID: "lgwl_src"})
		}
		e.apply(func(c *config.LiveGatewayConf) { c.MaxTicketsPerSubject = 0 })
		if _, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(issueReq("lgwl_src", "r-nocap")); err != nil {
			t.Fatalf("配置为 0 是显式「不限制」: %v", err)
		}
	})

	t.Run("计数不可用不得当作 0", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_src", "c-src", nodeA, roomID, mid, model.RoleViewer)
		e.Ticket.fail("CountUnusedByRoomMid", errors.New("db down"))
		if _, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(issueReq("lgwl_src", "r-count")); err == nil {
			t.Fatalf("票据计数读不到时必须失败，不能按「一张都没有」放行")
		}
	})
}

func TestIssueReconnectTicketRedisBitFailureFailsLoudly(t *testing.T) {
	e := newTestEnv(t)
	e.seedActiveLease("lgwl_src", "c-src", nodeA, roomID, mid, model.RoleViewer)
	e.Leases.failWith("PutTicket", repository.ErrStoreUnavailable)

	_, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(issueReq("lgwl_src", "r-bit"))
	if !errors.Is(err, repository.ErrStoreUnavailable) {
		t.Fatalf("有效位写不进去应显式失败（这张票换不了租约），实际 %v", err)
	}
	// 诚实记录现状：DB 行已经落了，但没有有效位，兑换必失败；由撤销/清扫路径收敛。
	if len(e.Ticket.rows) != 1 || e.Ticket.rows[0].State != model.TicketStateIssued {
		t.Fatalf("应留下 1 行 ISSUED 供清扫，实际 %+v", e.Ticket.rows)
	}
	if len(e.Leases.tickets) != 0 {
		t.Fatalf("有效位未写入才对")
	}
}

func TestIssueReconnectTicketStoreMissing(t *testing.T) {
	e := newTestEnv(t)
	e.seedActiveLease("lgwl_src", "c-src", nodeA, roomID, mid, model.RoleViewer)
	e.markStoreMissing()
	_, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(issueReq("lgwl_src", "r-nostore"))
	if !errors.Is(err, repository.ErrStoreUnavailable) {
		t.Fatalf("应回 ErrStoreUnavailable，实际 %v", err)
	}
}

// --- Redeem：参数门禁 ---

func TestRedeemReconnectTicketParameterGate(t *testing.T) {
	t.Run("空请求", func(t *testing.T) {
		e := newTestEnv(t)
		_, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(nil)
		if !errors.Is(err, model.ErrEmptyTicket) {
			t.Fatalf("应回 ErrEmptyTicket，实际 %v", err)
		}
		if e.Leases.callCount("Acquire") != 0 || e.Leases.callCount("ConsumeTicket") != 0 {
			t.Fatalf("空请求不得有任何副作用")
		}
	})
	cases := []struct {
		name string
		mut  func(*rpc.RedeemReconnectTicketReq)
		want error
	}{
		{"缺少票据", func(r *rpc.RedeemReconnectTicketReq) { r.Ticket = "" }, model.ErrEmptyTicket},
		{"房间号非正", func(r *rpc.RedeemReconnectTicketReq) { r.RoomId = 0 }, model.ErrInvalidRoomID},
		{"mid 为负", func(r *rpc.RedeemReconnectTicketReq) { r.Mid = -1 }, model.ErrInvalidMid},
		{"缺少新连接标识", func(r *rpc.RedeemReconnectTicketReq) { r.ConnId = " " }, model.ErrEmptyConnID},
		{"连接标识含空格", func(r *rpc.RedeemReconnectTicketReq) { r.ConnId = "c 1" }, model.ErrEmptyConnID},
		{"缺少承载节点", func(r *rpc.RedeemReconnectTicketReq) { r.NodeId = "" }, model.ErrEmptyNodeID},
		{"缺少幂等键", func(r *rpc.RedeemReconnectTicketReq) { r.RequestId = "" }, model.ErrEmptyRequestID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})
			req := redeemReq(ticket, "c-new", "r-1")
			tc.mut(req)
			_, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("应回 %v，实际 %v", tc.want, err)
			}
			if e.Leases.callCount("Acquire") != 0 || e.Leases.callCount("ConsumeTicket") != 0 {
				t.Fatalf("参数非法不得消费凭据或签发租约")
			}
		})
	}
}

func TestRedeemReconnectTicketNodeIDTrimmedNotRewritten(t *testing.T) {
	e := newTestEnv(t)
	e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, mid, model.RoleViewer)
	_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})

	// CleanNodeID 的口径是「去首尾空白 + 拒绝含内部空白/超长」，不改大小写：
	// 节点标识由部署方命名，本服务无权替它做规范化（否则路由表里的名字就对不上了）。
	req := redeemReq(ticket, "c-new", "r-node")
	req.NodeId = "  " + nodeB + "  "
	reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(req)
	if err != nil {
		t.Fatalf("兑换失败: %v", err)
	}
	if got := reply.GetLease().GetNodeId(); got != nodeB {
		t.Fatalf("node_id 应去掉首尾空白后原样保留，实际 %q", got)
	}
	stored, gerr := e.Leases.Get(context.Background(), reply.GetLease().GetLeaseId())
	if gerr != nil || stored == nil || stored.NodeID != nodeB {
		t.Fatalf("Redis 记录也必须落归一后的节点，err=%v rec=%+v", gerr, stored)
	}

	bad := redeemReq(ticket, "c-new2", "r-node-bad")
	bad.NodeId = "gw node a"
	if _, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(bad); !errors.Is(err, model.ErrEmptyNodeID) {
		t.Fatalf("含内部空白的 node_id 是畸形输入，应回 ErrEmptyNodeID，实际 %v", err)
	}
}

// --- Redeem：签名器与验签 ---

func TestRedeemReconnectTicketSignerMissingIsErrorNotSilentAllow(t *testing.T) {
	e := newTestEnv(t)
	_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})
	e.markSignerMissing()

	_, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-s"))
	if !errors.Is(err, repository.ErrSignerMissing) {
		t.Fatalf("密钥缺失必须报依赖故障，实际 %v", err)
	}
	if e.Leases.callCount("Acquire") != 0 || e.Leases.callCount("ConsumeTicket") != 0 {
		t.Fatalf("验签不了就绝不消费凭据、绝不发租约")
	}
}

func TestRedeemReconnectTicketCredentialShapes(t *testing.T) {
	forgedKey, err := repository.NewSigner("forged-key-32-bytes-unit-test!!", "LIVEGW_FORGED")
	if err != nil {
		t.Fatalf("构造外来签名器失败: %v", err)
	}

	t.Run("垃圾串按 BAD_TICKET 拒绝且不报错", func(t *testing.T) {
		e := newTestEnv(t)
		reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(
			redeemReq("not-even-a-ticket", "c-new", "r-junk"))
		if err != nil {
			t.Fatalf("形状非法是正常拒绝，不该返回 error: %v", err)
		}
		if reply.GetAllowed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_BAD_TICKET {
			t.Fatalf("应回 allowed=false + BAD_TICKET，实际 %+v", reply)
		}
		if reply.GetTicketState() != rpc.TicketState_TICKET_STATE_UNSPECIFIED {
			t.Fatalf("库里没有这一行，状态应回 UNSPECIFIED（这张票不在本服务账上），实际 %v", reply.GetTicketState())
		}
		if len(e.Logs.rows) != 0 {
			t.Fatalf("垃圾输入不是越权，不该占审计位，实际 %d 行", len(e.Logs.rows))
		}
	})

	t.Run("形状合法但未入库", func(t *testing.T) {
		e := newTestEnv(t)
		claims := repository.TicketClaims{TicketID: "lgwt_unknown", RoomID: roomID, Mid: mid, ConnID: "c-x", Role: model.RoleViewer, ExpireAt: e.Clock.unix() + 120}
		ticket, serr := e.Svc.Signer.Sign(claims)
		if serr != nil {
			t.Fatalf("签名失败: %v", serr)
		}
		reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-u"))
		if err != nil {
			t.Fatalf("兑换失败: %v", err)
		}
		if reply.GetAllowed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_BAD_TICKET {
			t.Fatalf("未入库票据必须拒: %+v", reply)
		}
	})

	t.Run("篡改签名位后按摘要定位不上", func(t *testing.T) {
		e := newTestEnv(t)
		_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})
		i := strings.LastIndex(ticket, ".")
		last := ticket[len(ticket)-1]
		mutated := ticket[:len(ticket)-1]
		if last == 'a' {
			mutated += "b"
		} else {
			mutated += "a"
		}
		reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(mutated, "c-new", "r-mut"))
		if err != nil {
			t.Fatalf("兑换失败: %v", err)
		}
		if reply.GetAllowed() || i < 0 {
			t.Fatalf("改动一位签名就换不了票: %+v", reply)
		}
		if len(e.Logs.rows) != 0 {
			t.Fatalf("未定位到行的篡改不写审计（没有可归属的房间）")
		}
	})

	t.Run("ticket_id 与摘要不同源", func(t *testing.T) {
		e := newTestEnv(t)
		claims := repository.TicketClaims{TicketID: "lgwt_real", RoomID: roomID, Mid: mid, ConnID: "c-old", Role: model.RoleViewer, ExpireAt: e.Clock.unix() + 120}
		ticket, serr := e.Svc.Signer.Sign(claims)
		if serr != nil {
			t.Fatalf("签名失败: %v", serr)
		}
		// 行里被写成另一个 ticket_id（拼接过的票据串）：即使签名本身自洽也不能认。
		e.Ticket.rows = append(e.Ticket.rows, &model.LiveGwReconnectTicket{
			Id: 1, TicketId: "lgwt_other", TicketHash: model.TicketHash(ticket), RoomId: roomID, Mid: mid,
			ConnId: claims.ConnID, State: model.TicketStateIssued, IssuedAt: e.Clock.unix(), ExpireAt: claims.ExpireAt,
		})
		reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-id"))
		if err != nil {
			t.Fatalf("兑换失败: %v", err)
		}
		if reply.GetAllowed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_BAD_TICKET {
			t.Fatalf("串与行不同源必须拒: %+v", reply)
		}
	})

	t.Run("外来密钥签的票验不过", func(t *testing.T) {
		e := newTestEnv(t)
		claims := repository.TicketClaims{TicketID: "lgwt_forged", RoomID: roomID, Mid: mid, ConnID: "c-old", Role: model.RoleViewer, ExpireAt: e.Clock.unix() + 120}
		ticket, ferr := forgedKey.Sign(claims)
		if ferr != nil {
			t.Fatalf("外来签名器失败: %v", ferr)
		}
		// 模拟 DB 行被外来写入者按同一 hash 落了行：验签仍是唯一防线。
		e.Ticket.rows = append(e.Ticket.rows, &model.LiveGwReconnectTicket{
			Id: 1, TicketId: claims.TicketID, TicketHash: model.TicketHash(ticket), RoomId: roomID, Mid: mid,
			ConnId: claims.ConnID, State: model.TicketStateIssued, IssuedAt: e.Clock.unix(), ExpireAt: claims.ExpireAt,
		})
		e.Leases.tickets[model.TicketHash(ticket)] = claims.TicketID
		reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-f"))
		if err != nil {
			t.Fatalf("兑换失败: %v", err)
		}
		if reply.GetAllowed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_BAD_TICKET {
			t.Fatalf("非本密钥签发的票据必须拒: %+v", reply)
		}
		if e.Leases.callCount("ConsumeTicket") != 0 {
			t.Fatalf("验签失败时不得消费有效位")
		}
	})

	t.Run("按摘要查库失败原样上抛", func(t *testing.T) {
		e := newTestEnv(t)
		_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})
		e.Ticket.fail("FindByHash", errors.New("db down"))
		if _, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-db")); err == nil {
			t.Fatalf("依赖故障不能翻译成 allowed=false")
		}
	})
}

func TestRedeemReconnectTicketTripletMismatch(t *testing.T) {
	e := newTestEnv(t)
	_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})

	req := redeemReq(ticket, "c-new", "r-trip")
	req.RoomId = otherRoom // 拿别人房间的票换到自己房间
	reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(req)
	if err != nil {
		t.Fatalf("越权是响应体结论，不该返回 error: %v", err)
	}
	if reply.GetAllowed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_PERMISSION_DENIED {
		t.Fatalf("三元组不符应回 PERMISSION_DENIED，实际 %+v", reply)
	}
	if reply.GetTicketState() != rpc.TicketState_TICKET_STATE_ISSUED {
		t.Fatalf("行仍 ISSUED，状态回显应如实: %+v", reply)
	}
	rows := e.Logs.all()
	if len(rows) != 1 {
		t.Fatalf("越权兑换应落 1 行 DENIED，实际 %d 行", len(rows))
	}
	if rows[0].RoomId != otherRoom {
		t.Fatalf("审计要记被侵害的房间（请求里的 %d），实际 %d", otherRoom, rows[0].RoomId)
	}
	if rows[0].State != model.BroadcastLogDenied || rows[0].DropReason != model.DropPermissionDenied {
		t.Fatalf("审计行分类不对: %+v", rows[0])
	}
	if !strings.HasPrefix(rows[0].MessageId, "ticket-redeem-denied:lgwt_") {
		t.Fatalf("审计 message_id 要带场景与票据定位符（复盘要用），实际 %s", rows[0].MessageId)
	}
	// 回显给调用方的一律是脱敏文案：探测者不该从拒绝理由里拿到任何凭据标识。
	if strings.Contains(reply.GetDenyDetail(), "lgwt_") || strings.Contains(reply.GetDenyDetail(), ticket) {
		t.Fatalf("拒绝理由泄露凭据标识: %q", reply.GetDenyDetail())
	}
	if reply.GetLease() != nil {
		t.Fatalf("被拒的兑换不得回显租约")
	}
	if e.Leases.callCount("Acquire") != 0 {
		t.Fatalf("越权不得签发租约")
	}
	if !e.Leases.hasTicketBit(model.TicketHash(ticket)) {
		t.Fatalf("被拒的兑换不得消费票据（否则成了「越权探测顺手毁掉别人的凭据」）")
	}
	// 正确的三元组仍能换：拒绝没有把票据状态污染。
	if ok, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-ok")); err != nil || !ok.GetAllowed() {
		t.Fatalf("三元组吻合时应可兑换: err=%v reply=%+v", err, ok)
	}

	t.Run("mid 不符同样拒且不改票据状态", func(t *testing.T) {
		e := newTestEnv(t)
		_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})
		req := redeemReq(ticket, "c-new", "r-mid")
		req.Mid = otherMid
		reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(req)
		if err != nil || reply.GetAllowed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_PERMISSION_DENIED {
			t.Fatalf("mid 不符应回 PERMISSION_DENIED: err=%v reply=%+v", err, reply)
		}
		if e.Ticket.rows[0].State != model.TicketStateIssued {
			t.Fatalf("被拒的兑换不得推进票据状态")
		}
	})
}

func TestRedeemReconnectTicketTerminalAndExpiredStates(t *testing.T) {
	cases := []struct {
		name      string
		state     int32
		expired   bool // 行仍 ISSUED 但已过时效
		wantState rpc.TicketState
		wantDrop  rpc.DropReason
		wantAudit int
	}{
		{"已使用", model.TicketStateUsed, false, rpc.TicketState_TICKET_STATE_USED, rpc.DropReason_DROP_REASON_BAD_TICKET, 0},
		{"已撤销改判越权", model.TicketStateRevoked, false, rpc.TicketState_TICKET_STATE_REVOKED, rpc.DropReason_DROP_REASON_PERMISSION_DENIED, 1},
		{"清扫后已过期", model.TicketStateExpired, false, rpc.TicketState_TICKET_STATE_EXPIRED, rpc.DropReason_DROP_REASON_BAD_TICKET, 0},
		{"未清扫但已过时效", model.TicketStateIssued, true, rpc.TicketState_TICKET_STATE_EXPIRED, rpc.DropReason_DROP_REASON_BAD_TICKET, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			ttl := 120
			if tc.expired {
				ttl = -1
			}
			_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", state: tc.state, ttl: ttl, bit: true})
			reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-term"))
			if err != nil {
				t.Fatalf("终态票据是正常拒绝，不该返回 error: %v", err)
			}
			if reply.GetAllowed() {
				t.Fatalf("终态/过期票据绝不能换到租约: %+v", reply)
			}
			if reply.GetTicketState() != tc.wantState {
				t.Fatalf("ticket_state 应回真实状态 %v，实际 %v", tc.wantState, reply.GetTicketState())
			}
			if reply.GetDenyReason() != tc.wantDrop {
				t.Fatalf("drop 应为 %v，实际 %v（detail=%s）", tc.wantDrop, reply.GetDenyReason(), reply.GetDenyDetail())
			}
			if got := len(e.Logs.rows); got != tc.wantAudit {
				t.Fatalf("审计行数应为 %d，实际 %d", tc.wantAudit, got)
			}
			if e.Leases.callCount("Acquire") != 0 || e.Leases.callCount("ConsumeTicket") != 0 {
				t.Fatalf("判定失败时不得消费票据或签发租约")
			}
		})
	}
}

// --- Redeem：一次性消费 ---

func TestRedeemReconnectTicketHappyPath(t *testing.T) {
	e := newTestEnv(t)
	old := e.Leases.seedLease(&repository.LeaseRecord{LeaseID: "lgwl_old", ConnID: "c-old", NodeID: nodeA,
		RoomID: roomID, Mid: mid, Role: model.RoleViewer, State: model.LeaseStateActive,
		IssuedAt: e.Clock.unix() - 20, ExpireAt: e.Clock.unix() + 10, Topics: []string{"room", "chat"},
		JoinedAt: e.Clock.unix() - 300, DeviceIDHash: "devhash", Platform: model.PlatformIOS,
		AppVersion: "1.2.3", HeartbeatCount: 7})
	if old == nil {
		t.Fatalf("预置旧租约失败")
	}
	_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})
	now := e.Clock.unix()
	if err := e.Leases.MarkOffline(context.Background(), roomID, mid, now-45, 3, 60); err != nil {
		t.Fatalf("预置断线视图失败: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, berr := e.Leases.BumpRoomSeq(context.Background(), roomID); berr != nil {
			t.Fatalf("推进房间序号失败: %v", berr)
		}
	}

	reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-redeem"))
	if err != nil {
		t.Fatalf("兑换失败: %v", err)
	}
	if !reply.GetAllowed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_OK {
		t.Fatalf("正常兑换应 allowed=true + deny=OK，实际 %+v", reply)
	}
	if reply.GetTicketState() != rpc.TicketState_TICKET_STATE_USED {
		t.Fatalf("兑换后票据状态应为 USED，实际 %v", reply.GetTicketState())
	}
	newLeaseID := reply.GetLease().GetLeaseId()
	if !strings.HasPrefix(newLeaseID, "lgwl_") || newLeaseID == "lgwl_old" {
		t.Fatalf("必须换到一条新租约: %+v", reply.GetLease())
	}
	if reply.GetLease().GetConnId() != "c-new" || reply.GetLease().GetNodeId() != nodeA {
		t.Fatalf("新租约必须落在请求的新连接/新节点: %+v", reply.GetLease())
	}
	if reply.GetLease().GetState() != rpc.LeaseState_LEASE_STATE_ACTIVE {
		t.Fatalf("新租约应 ACTIVE: %+v", reply.GetLease())
	}
	if reply.GetLease().GetReconnectTicket() != "" {
		t.Fatalf("兑换掉一张票后不得再随租约发一张并不存在的新凭据")
	}
	if got := reply.GetLastOfflineAt(); got != now-45 {
		t.Fatalf("last_offline_at 应回显断线视图时刻 %d，实际 %d", now-45, got)
	}
	if got := reply.GetMissedMessageEstimate(); got != 2 {
		t.Fatalf("missed_message_estimate 应为 2，实际 %d", got)
	}

	row := revokedRow(t, e, assertTicketShape(t, ticket))
	if row.State != model.TicketStateUsed || row.UsedAt != now || row.NewLeaseId != newLeaseID {
		t.Fatalf("票据行必须收敛成 USED 并回填新租约: %+v", row)
	}
	if e.Leases.hasTicketBit(row.TicketHash) {
		t.Fatalf("有效位必须被 GETDEL 消费")
	}
	stored, gerr := e.Leases.Get(context.Background(), newLeaseID)
	if gerr != nil || stored == nil {
		t.Fatalf("新租约必须落在 Redis: err=%v", gerr)
	}
	if strings.Join(stored.Topics, ",") != "room,chat" || stored.JoinedAt != now-300 {
		t.Fatalf("宽限期内的订阅视图必须带过来，否则重连后收不到消息: %+v", stored)
	}
	if stored.DeviceIDHash != "devhash" || stored.Platform != old.Platform || stored.AppVersion != "1.2.3" {
		t.Fatalf("脱敏客户端字段必须原样带过来: %+v", stored)
	}
	if stored.RenewCount != 1 || stored.HeartbeatCount != 7 {
		t.Fatalf("renew_count 递增、心跳计数延续: %+v", stored)
	}
	if cnt, cerr := e.Leases.RoomConnectionCount(context.Background(), roomID, 500); cerr != nil || cnt != 1 {
		t.Fatalf("同 conn 重连不得留双活，房间连接数应为 1，实际 %d (err=%v)", cnt, cerr)
	}
	if len(e.Logs.rows) != 0 {
		t.Fatalf("兑换的收敛写在票据行上，不写逐连接审计: %d 行", len(e.Logs.rows))
	}
	assertNoTicketLeak(t, e, ticket)
}

func TestRedeemReconnectTicketSameConnOnlyOnce(t *testing.T) {
	e := newTestEnv(t)
	e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, mid, model.RoleViewer)
	_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})
	logical := e.Leases.callCount("Acquire")

	first, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-once"))
	if err != nil || !first.GetAllowed() {
		t.Fatalf("首次兑换失败: err=%v reply=%+v", err, first)
	}
	second, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new2", "r-once2"))
	if err != nil {
		t.Fatalf("重复兑换是正常拒绝，不该返回 error: %v", err)
	}
	if second.GetAllowed() || second.GetDenyReason() != rpc.DropReason_DROP_REASON_BAD_TICKET ||
		second.GetTicketState() != rpc.TicketState_TICKET_STATE_USED {
		t.Fatalf("一次性凭据第二次必须拒为 USED/BAD_TICKET: %+v", second)
	}
	if !strings.Contains(second.GetDenyDetail(), "one-time") {
		t.Fatalf("拒绝理由要说明一次性凭据，实际 %q", second.GetDenyDetail())
	}
	if got := e.Leases.callCount("Acquire"); got != logical+1 {
		t.Fatalf("第二次兑换不得再签租约，Acquire 次数 %d（基线 %d）", got, logical)
	}
	if cnt, _ := e.Leases.RoomConnectionCount(context.Background(), roomID, 500); cnt != 1 {
		t.Fatalf("房间连接数必须还是 1，实际 %d", cnt)
	}
}

func TestRedeemReconnectTicketValidityBit(t *testing.T) {
	t.Run("有效位已缺失按已消费处理", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, mid, model.RoleViewer)
		row, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})
		if err := e.Leases.DropTicket(context.Background(), row.TicketHash); err != nil {
			t.Fatalf("删除有效位失败: %v", err)
		}
		reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-nobit"))
		if err != nil || reply.GetAllowed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_BAD_TICKET {
			t.Fatalf("无有效位应拒为 BAD_TICKET: err=%v reply=%+v", err, reply)
		}
		if !strings.Contains(reply.GetDenyDetail(), "validity bit") {
			t.Fatalf("拒绝理由应指向有效位，实际 %q", reply.GetDenyDetail())
		}
		if e.Leases.callCount("Acquire") != 0 {
			t.Fatalf("没有有效位就绝不签租约")
		}
		if e.Ticket.rows[0].State != model.TicketStateIssued {
			t.Fatalf("有效位缺失时 DB 行不该被推进")
		}
	})

	t.Run("有效位与行不同源显式拒绝", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, mid, model.RoleViewer)
		row, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})
		// 模拟别的写入者动了这个键：值不再是本行的 ticket_id。
		if perr := e.Leases.PutTicket(context.Background(), row.TicketHash, "lgwt_someone_else", 120); perr != nil {
			t.Fatalf("覆写有效位失败: %v", perr)
		}
		reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-src"))
		if err != nil || reply.GetAllowed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_BAD_TICKET {
			t.Fatalf("不同源必须拒: err=%v reply=%+v", err, reply)
		}
		if !strings.Contains(reply.GetDenyDetail(), "does not match") {
			t.Fatalf("理由应指向不同源，实际 %q", reply.GetDenyDetail())
		}
		if e.Leases.callCount("Acquire") != 0 || e.Ticket.rows[0].State != model.TicketStateIssued {
			t.Fatalf("来历不明的断言不能拿来发凭据")
		}
		if e.Leases.hasTicketBit(row.TicketHash) {
			t.Fatalf("GETDEL 已经消费掉键，重复兑换在 Redis 侧也已封死")
		}
	})

	t.Run("有效位读数故障原样上抛", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, mid, model.RoleViewer)
		_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})
		e.Leases.failWith("ConsumeTicket", repository.ErrStoreUnavailable)
		if _, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-consumeerr")); !errors.Is(err, repository.ErrStoreUnavailable) {
			t.Fatalf("依赖故障不得翻译成 allowed=false，实际 %v", err)
		}
	})
}

// --- Redeem：补偿与幂等 ---

func TestRedeemReconnectTicketConsumeMissCompensates(t *testing.T) {
	e := newTestEnv(t)
	e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, mid, model.RoleViewer)
	_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})
	e.Ticket.raceConsume = true // 并发对手抢先消费

	reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-race"))
	if err != nil {
		t.Fatalf("并发未命中是正常拒绝: %v", err)
	}
	if reply.GetAllowed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_BAD_TICKET {
		t.Fatalf("条件更新打不中必须拒: %+v", reply)
	}
	if !strings.Contains(reply.GetDenyDetail(), "one-time credential") {
		t.Fatalf("结论要落在回读到的真实状态上（对手已置 USED），实际 %q", reply.GetDenyDetail())
	}
	if reply.GetTicketState() != rpc.TicketState_TICKET_STATE_USED {
		t.Fatalf("回读应给出对手消费后的真实状态 USED，实际 %v", reply.GetTicketState())
	}
	// 补偿：刚签发的租约必须收回，不留悬空在线。
	if cnt, cerr := e.Leases.RoomConnectionCount(context.Background(), roomID, 500); cerr != nil || cnt != 0 {
		t.Fatalf("回滚后房间连接数应为 0，实际 %d (err=%v)", cnt, cerr)
	}
	if got := e.Leases.callCount("Acquire"); got != 1 {
		t.Fatalf("本次确实签过一次又被收回，Acquire 次数应为 1，实际 %d", got)
	}
	if e.Leases.callCount("Delete") == 0 {
		t.Fatalf("补偿路径必须走 Delete")
	}
	if row := e.Ticket.rows[0]; row.NewLeaseId == "" || row.NewLeaseId == reply.GetLease().GetLeaseId() {
		t.Fatalf("行里的 new_lease_id 必须是赢家的，而不是本请求的: %+v", row)
	}
}

func TestRedeemReconnectTicketConsumeErrorCompensatesAndFails(t *testing.T) {
	e := newTestEnv(t)
	e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, mid, model.RoleViewer)
	_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})
	e.Ticket.fail("Consume", errors.New("db down"))

	_, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-dberr"))
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("DB 消费失败必须上抛，实际 %v", err)
	}
	if cnt, _ := e.Leases.RoomConnectionCount(context.Background(), roomID, 500); cnt != 0 {
		t.Fatalf("失败前已签发的租约必须收回，实际房间连接数 %d", cnt)
	}
	if e.Ticket.rows[0].State != model.TicketStateIssued {
		t.Fatalf("消费失败时票据行不得被说成 USED")
	}
}

func TestRedeemReconnectTicketIdempotentReplay(t *testing.T) {
	t.Run("幂等键命中且首次租约仍在则原样回显", func(t *testing.T) {
		e := newTestEnv(t)
		first := e.seedActiveLease("lgwl_first", "c-first", nodeA, roomID, mid, model.RoleViewer)
		_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})
		// 幂等窗口里已回填首次结论（真实链路里由 RememberRequest 写入）。
		e.Leases.seedRequest(redeemRPCName, "r-idem", first.LeaseID)

		reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-idem"))
		if err != nil || !reply.GetAllowed() {
			t.Fatalf("重放应回显首次租约: err=%v reply=%+v", err, reply)
		}
		if reply.GetLease().GetLeaseId() != first.LeaseID {
			t.Fatalf("必须回显同一条租约，实际 %+v", reply.GetLease())
		}
		if e.Leases.callCount("Acquire") != 0 || e.Leases.callCount("ConsumeTicket") != 0 {
			t.Fatalf("重放必须零写入：不得再消费票据或再签租约")
		}
		// 诚实记录现状：重放回显 ticket_state=USED，但本次出示的这张票并未被消费（行仍 ISSUED、位仍在）。
		if e.Ticket.rows[0].State != model.TicketStateIssued || !e.Leases.hasTicketBit(e.Ticket.rows[0].TicketHash) {
			t.Fatalf("重放分支不该动票据状态与有效位")
		}
		if reply.GetTicketState() != rpc.TicketState_TICKET_STATE_USED {
			t.Fatalf("回放的结论字段应仍是首次的 USED，实际 %v", reply.GetTicketState())
		}
	})

	t.Run("首次已消费票据后同键不再命中幂等分支", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, mid, model.RoleViewer)
		_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})

		first, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-idem"))
		if err != nil || !first.GetAllowed() {
			t.Fatalf("首次兑换失败: err=%v reply=%+v", err, first)
		}
		acquires := e.Leases.callCount("Acquire")
		second, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-idem"))
		if err != nil {
			t.Fatalf("重放失败应是响应体结论: %v", err)
		}
		// 现状（已作为缺陷上报）：proto 里 request_id 注释承诺「同 request_id 重放返回同一新租约」，
		// 但终态判定先于幂等分支，所以一次成功后重试拿到的是 BAD_TICKET/USED 而不是原租约。
		// 好在这条路径不产生任何额外副作用，客户端至多回落到重新 Join。
		if second.GetAllowed() || second.GetDenyReason() != rpc.DropReason_DROP_REASON_BAD_TICKET ||
			second.GetTicketState() != rpc.TicketState_TICKET_STATE_USED {
			t.Fatalf("重放不得换到第二条租约: %+v", second)
		}
		if e.Leases.callCount("Acquire") != acquires {
			t.Fatalf("重试不得再签租约")
		}
		if cnt, _ := e.Leases.RoomConnectionCount(context.Background(), roomID, 500); cnt != 1 {
			t.Fatalf("房间连接数必须仍是 1，实际 %d", cnt)
		}
	})
}

func TestRedeemReconnectTicketReplayWithoutLeaseReExecutes(t *testing.T) {
	t.Run("结论仍是 pending 时按未受理重跑", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, mid, model.RoleViewer)
		_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})
		e.Leases.seedRequest(redeemRPCName, "r-pending", lgwOutcomePending)

		reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-pending"))
		if err != nil || !reply.GetAllowed() {
			t.Fatalf("首次请求崩在中途（未消费票据）时应能重跑成功: err=%v reply=%+v", err, reply)
		}
		if v := e.Leases.requests[redeemRPCName+"|r-pending"]; v != reply.GetLease().GetLeaseId() {
			t.Fatalf("幂等结论必须回填新租约 ID，实际 %q", v)
		}
	})

	t.Run("首次已消费票据但租约已回收", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, mid, model.RoleViewer)
		row, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old"})
		e.Leases.seedRequest(redeemRPCName, "r-gone", "lgwl_vanished")
		_ = row // 本用例刻意不放有效位（首次请求已把它 GETDEL 掉）

		reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-gone"))
		if err != nil {
			t.Fatalf("重跑失败应是响应体结论: %v", err)
		}
		if reply.GetAllowed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_BAD_TICKET {
			t.Fatalf("票据已消费时不得凭空造租约: %+v", reply)
		}
		if e.Leases.callCount("Acquire") != 0 {
			t.Fatalf("重跑打不开凭据时绝不能签发租约（那等于白送一条连接）")
		}
	})
}

// --- Redeem：角色、配额、游客 ---

func TestRedeemReconnectTicketAnchorRoleReadjudicated(t *testing.T) {
	t.Run("房间已换主播则降为观众", func(t *testing.T) {
		e := newTestEnv(t)
		e.Rooms.owner = otherMid // live-room 说主播已经是别人
		e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, anchorMid, model.RoleAnchor)
		_, ticket := e.seedTicket(tkSeed{user: anchorMid, connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", role: model.RoleAnchor, bit: true})
		req := redeemReq(ticket, "c-new", "r-anchor")
		req.Mid = anchorMid

		reply, err := NewRedeemReconnectTicketLogic(ctxAs(t, "true", "SERVICE", "", "live-room"), e.Svc).RedeemReconnectTicket(req)
		if err != nil {
			t.Fatalf("兑换失败: %v", err)
		}
		if reply.GetLease().GetRole() != rpc.ConnRole_CONN_ROLE_VIEWER {
			t.Fatalf("ANCHOR 必须重新确认归属，归属已变则降为 VIEWER，实际 %v", reply.GetLease().GetRole())
		}
		if e.Rooms.ownerCalls == 0 {
			t.Fatalf("兑换路径必须重新问归属（房间可能已换主播）")
		}
	})

	t.Run("归属未变则沿用主播角色", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, anchorMid, model.RoleAnchor)
		_, ticket := e.seedTicket(tkSeed{user: anchorMid, connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", role: model.RoleAnchor, bit: true})
		req := redeemReq(ticket, "c-new", "r-anchor-ok")
		req.Mid = anchorMid
		reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(req)
		if err != nil || reply.GetLease().GetRole() != rpc.ConnRole_CONN_ROLE_ANCHOR {
			t.Fatalf("仍是主播就该保留 ANCHOR: err=%v reply=%+v", err, reply)
		}
	})

	t.Run("RoomGate 未接线按既有口径降级", func(t *testing.T) {
		e := newTestEnv(t)
		e.markRoomGateUnwired()
		e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, anchorMid, model.RoleAnchor)
		_, ticket := e.seedTicket(tkSeed{user: anchorMid, connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", role: model.RoleAnchor, bit: true})
		req := redeemReq(ticket, "c-new", "r-unwired")
		req.Mid = anchorMid
		reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(req)
		if err != nil || reply.GetLease().GetRole() != rpc.ConnRole_CONN_ROLE_VIEWER {
			t.Fatalf("未接线是可解释的降级（VIEWER），不该报错: err=%v reply=%+v", err, reply)
		}
	})

	t.Run("归属查询真故障必须上抛", func(t *testing.T) {
		e := newTestEnv(t)
		e.Rooms.ownerErr = errors.New("live-room unavailable")
		e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, anchorMid, model.RoleAnchor)
		_, ticket := e.seedTicket(tkSeed{user: anchorMid, connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", role: model.RoleAnchor, bit: true})
		req := redeemReq(ticket, "c-new", "r-err")
		req.Mid = anchorMid
		if _, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(req); err == nil {
			t.Fatalf("问不到真值时不能猜，必须报错")
		}
		if e.Leases.callCount("ConsumeTicket") != 0 {
			t.Fatalf("归属问不清时不得消费票据")
		}
	})
}

func TestRedeemReconnectTicketGuestDenied(t *testing.T) {
	e := newTestEnv(t)
	e.apply(func(c *config.LiveGatewayConf) { c.AllowGuestByDefault = false })
	e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, 0, model.RoleViewer)
	_, ticket := e.seedTicket(tkSeed{guest: true, connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})
	req := redeemReq(ticket, "c-new", "r-guest")
	req.Mid = 0

	reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(req)
	if err != nil {
		t.Fatalf("游客被配额拒绝是响应体结论: %v", err)
	}
	if reply.GetAllowed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_PERMISSION_DENIED {
		t.Fatalf("重连不得绕过 allow_guest: %+v", reply)
	}
	if e.Leases.callCount("Acquire") != 0 || e.Ticket.rows[0].State != model.TicketStateIssued {
		t.Fatalf("拒绝时既不签租约也不消费票据")
	}
	if got := len(e.Logs.rows); got != 1 {
		t.Fatalf("越权类拒绝应留 1 行审计，实际 %d", got)
	}
}

func TestRedeemReconnectTicketQuotaBoundary(t *testing.T) {
	// 与 Acquire/Join 的口径差异（`count > max` vs `count >= max`）在这里被固定成可观察事实：
	// 房间连接数读的是「此刻还在集合里的 ACTIVE 条数」，含本主体那条旧连接，
	// 于是 max_connections=1 时第 2 条连接（count=1）仍放行、第 3 条才拒。
	// 见交付报告里的缺陷条目；本用例断言的是「当前真实语义」，改口径时必须同步改测试。
	cases := []struct {
		name     string
		existing int
		allowed  bool
	}{
		{"仅本主体旧连接 1 条（等于上限）当前放行", 0, true},
		{"另有 1 条（合计 2 条，超上限）拒绝", 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.Quotas = newFakeQuotas(&model.LiveGwAccessQuota{
				Id: 1, Scope: model.QuotaScopeRoom, ScopeId: roomID, MaxConnections: 1, Version: 1,
			})
			e.Svc.Store.Quotas = e.Quotas
			for i := 0; i < tc.existing; i++ {
				e.seedActiveLease("lgwl_x"+string(rune('a'+i)), "c-x"+string(rune('a'+i)), nodeA, roomID, otherMid, model.RoleViewer)
			}
			e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, mid, model.RoleViewer)
			_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})

			reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-quota"))
			if err != nil {
				t.Fatalf("配额拒绝是响应体结论，不该报错: %v", err)
			}
			if reply.GetAllowed() != tc.allowed {
				t.Fatalf("allowed 期望 %v，实际 %+v", tc.allowed, reply)
			}
			if !tc.allowed {
				if reply.GetDenyReason() != rpc.DropReason_DROP_REASON_RATE_LIMITED {
					t.Fatalf("超配额应回 RATE_LIMITED，实际 %+v", reply)
				}
				if !strings.Contains(reply.GetDenyDetail(), "/1") {
					t.Fatalf("拒绝理由要带上 count/max，实际 %q", reply.GetDenyDetail())
				}
			}
		})
	}

	t.Run("连接数读数不可用不得按 0 放行", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, mid, model.RoleViewer)
		_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})
		e.Leases.failWith("RoomConnectionCount", repository.ErrStoreUnavailable)
		_, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-count"))
		if !errors.Is(err, repository.ErrStoreUnavailable) {
			t.Fatalf("读不到连接数时必须失败，实际 %v", err)
		}
		if e.Leases.callCount("ConsumeTicket") != 0 {
			t.Fatalf("判定不成立时不得消费票据")
		}
	})
}

func TestRedeemReconnectTicketLeaseTTLFromQuota(t *testing.T) {
	e := newTestEnv(t)
	e.Quotas = newFakeQuotas(&model.LiveGwAccessQuota{
		Id: 1, Scope: model.QuotaScopeRoom, ScopeId: roomID, LeaseTtlSeconds: 45, Version: 1,
	})
	e.Svc.Store.Quotas = e.Quotas
	e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, mid, model.RoleViewer)
	_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})

	req := redeemReq(ticket, "c-new", "r-ttl")
	reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(req)
	if err != nil || !reply.GetAllowed() {
		t.Fatalf("兑换失败: err=%v reply=%+v", err, reply)
	}
	if got := reply.GetLease().GetExpireAt() - reply.GetLease().GetIssuedAt(); got != 45 {
		t.Fatalf("重连租约有效期应取配额 45 秒，实际 %d", got)
	}
	req2 := redeemReq(ticket, "c-new2", "r-ttl2") // 票据已消费，这条只用于验证夹取上限的分支不 panic
	req2.TtlSeconds = 999999
	if _, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(req2); err != nil {
		t.Fatalf("第二次兑换应是响应体结论: %v", err)
	}
}

func TestRedeemReconnectTicketOfflineViewDegrades(t *testing.T) {
	e := newTestEnv(t)
	e.seedActiveLease("lgwl_old", "c-old", nodeA, roomID, mid, model.RoleViewer)
	_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_old", bit: true})
	e.Leases.failWith("OfflineView", repository.ErrStoreUnavailable)

	reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-view"))
	if err != nil {
		t.Fatalf("断线视图是可丢弃语义: %v", err)
	}
	if !reply.GetAllowed() || reply.GetLastOfflineAt() != 0 || reply.GetMissedMessageEstimate() != 0 {
		t.Fatalf("视图读不到时回 0 而不是伪造历史: %+v", reply)
	}
}

func TestRedeemReconnectTicketNoStaleViewAcrossSubjects(t *testing.T) {
	// lease_id 指向另一个 (room, mid) 时不能把别人的订阅视图接过来。
	e := newTestEnv(t)
	e.Leases.seedLease(&repository.LeaseRecord{LeaseID: "lgwl_other", ConnID: "c-other", NodeID: nodeB,
		RoomID: otherRoom, Mid: otherMid, Role: model.RoleViewer, State: model.LeaseStateActive,
		IssuedAt: e.Clock.unix() - 5, ExpireAt: e.Clock.unix() + 25, Topics: []string{"secret-topic"},
		DeviceIDHash: "someone-else"})
	_, ticket := e.seedTicket(tkSeed{connID: "c-old", nodeID: nodeA, leaseID: "lgwl_other", bit: true})

	reply, err := NewRedeemReconnectTicketLogic(ctxClient(t), e.Svc).RedeemReconnectTicket(redeemReq(ticket, "c-new", "r-cross"))
	if err != nil || !reply.GetAllowed() {
		t.Fatalf("兑换失败: err=%v reply=%+v", err, reply)
	}
	stored, gerr := e.Leases.Get(context.Background(), reply.GetLease().GetLeaseId())
	if gerr != nil || stored == nil {
		t.Fatalf("回读新租约失败: %v", gerr)
	}
	if len(stored.Topics) != 0 || stored.DeviceIDHash != "" || stored.RenewCount != 0 {
		t.Fatalf("跨主体的陈旧视图不得接到新租约上: %+v", stored)
	}
}

// --- Revoke ---

func TestRevokeReconnectTicketParameterGate(t *testing.T) {
	longTicket := strings.Repeat("t", 65)
	cases := []struct {
		name string
		req  *rpc.RevokeReconnectTicketReq
		want error
	}{
		{"空请求", nil, model.ErrEmptyTicket},
		{"没有任何定位范围", &rpc.RevokeReconnectTicketReq{Reason: "banned", Operator: "ops", RequestId: "r1"}, model.ErrEmptyTicket},
		{"票据 ID 超长", &rpc.RevokeReconnectTicketReq{TicketId: longTicket, Reason: "banned", Operator: "ops", RequestId: "r1"}, model.ErrEmptyTicket},
		{"票据 ID 含空格", &rpc.RevokeReconnectTicketReq{TicketId: "lgwt_a b", Reason: "banned", Operator: "ops", RequestId: "r1"}, model.ErrEmptyTicket},
		{"批量缺少房间号", &rpc.RevokeReconnectTicketReq{Mid: mid, Reason: "banned", Operator: "ops", RequestId: "r1"}, model.ErrInvalidRoomID},
		{"批量 mid 为 0 会扫全房游客", &rpc.RevokeReconnectTicketReq{RoomId: roomID, Mid: 0, Reason: "banned", Operator: "ops", RequestId: "r1"}, model.ErrInvalidMid},
		{"批量 mid 为负", &rpc.RevokeReconnectTicketReq{RoomId: roomID, Mid: -1, Reason: "banned", Operator: "ops", RequestId: "r1"}, model.ErrInvalidMid},
		{"缺少原因", &rpc.RevokeReconnectTicketReq{TicketId: "lgwt_a", Operator: "ops", RequestId: "r1"}, model.ErrEmptyReason},
		{"原因超长", &rpc.RevokeReconnectTicketReq{TicketId: "lgwt_a", Reason: strings.Repeat("x", 257), Operator: "ops", RequestId: "r1"}, model.ErrEmptyReason},
		{"缺少操作者", &rpc.RevokeReconnectTicketReq{TicketId: "lgwt_a", Reason: "banned", RequestId: "r1"}, model.ErrEmptyOperator},
		{"操作者含换行", &rpc.RevokeReconnectTicketReq{TicketId: "lgwt_a", Reason: "banned", Operator: "ops\n伪造一行", RequestId: "r1"}, model.ErrEmptyOperator},
		{"缺少幂等键", &rpc.RevokeReconnectTicketReq{TicketId: "lgwt_a", Reason: "banned", Operator: "ops"}, model.ErrEmptyRequestID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.seedTicket(tkSeed{ticketID: "lgwt_a", user: mid, connID: "c-a", nodeID: nodeA, bit: true})
			_, err := NewRevokeReconnectTicketLogic(ctxOperator(t, "1001"), e.Svc).RevokeReconnectTicket(tc.req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("应回 %v，实际 %v", tc.want, err)
			}
			if e.Ticket.rows[0].State != model.TicketStateIssued {
				t.Fatalf("参数非法不得推进票据状态")
			}
			if len(e.Logs.rows) != 0 {
				t.Fatalf("参数非法阶段不落审计，实际 %d 行", len(e.Logs.rows))
			}
		})
	}
}

func TestRevokeReconnectTicketRequiresAttestedDispositionCaller(t *testing.T) {
	t.Run("未归因主体被拒且留痕", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedTicket(tkSeed{ticketID: "lgwt_a", user: mid, connID: "c-a", nodeID: nodeA, bit: true})
		req := &rpc.RevokeReconnectTicketReq{TicketId: "lgwt_a", Reason: "banned", Operator: "self-reported", RequestId: "r-auth"}

		_, err := NewRevokeReconnectTicketLogic(ctxClient(t), e.Svc).RevokeReconnectTicket(req)
		if !errors.Is(err, model.ErrPermissionDenied) {
			t.Fatalf("处置类写接口默认 fail-closed，实际 %v", err)
		}
		rows := e.Logs.all()
		if len(rows) != 1 {
			t.Fatalf("被拒的撤销尝试必须留痕，实际 %d 行", len(rows))
		}
		if rows[0].State != model.BroadcastLogDenied || rows[0].DropReason != model.DropPermissionDenied {
			t.Fatalf("留痕应为 DENIED/PERMISSION_DENIED: %+v", rows[0])
		}
		if rows[0].SenderMid != 0 || rows[0].SourceService != "unattested" {
			t.Fatalf("主体取自归因结果而不是请求字段：mid 应为 0、source 应为 unattested，实际 %+v", rows[0])
		}
		if rows[0].RoomId != roomID {
			t.Fatalf("只给 ticket_id 时要按该行房间记账，实际 %d", rows[0].RoomId)
		}
		if e.Ticket.rows[0].State != model.TicketStateIssued {
			t.Fatalf("被拒的撤销不得推进票据状态")
		}
	})

	t.Run("自报 OPERATOR 请求体字段不构成授权", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedTicket(tkSeed{ticketID: "lgwt_a", user: mid, connID: "c-a", nodeID: nodeA, bit: true})
		req := revokeReq("r-self")
		req.TicketId = "lgwt_a"
		req.Operator = "自称的运营"
		if _, err := NewRevokeReconnectTicketLogic(ctxAs(t, "true", "VIEWER", "1001", ""), e.Svc).RevokeReconnectTicket(req); !errors.Is(err, model.ErrPermissionDenied) {
			t.Fatalf("metadata 里自报非内部角色应按未归因处理，实际 %v", err)
		}
	})

	t.Run("放开开关是可用的逃生阀但要留告警口径", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedTicket(tkSeed{ticketID: "lgwt_a", user: mid, connID: "c-a", nodeID: nodeA, bit: true})
		e.apply(func(c *config.LiveGatewayConf) { c.AllowUnattestedOperatorWrites = true })
		req := revokeReq("r-open")
		req.TicketId = "lgwt_a"
		reply, err := NewRevokeReconnectTicketLogic(ctxClient(t), e.Svc).RevokeReconnectTicket(req)
		if err != nil || !reply.GetRevoked() {
			t.Fatalf("开关打开后应放行（明知风险放开）: err=%v reply=%+v", err, reply)
		}
		if rows := e.Logs.all(); len(rows) != 1 || rows[0].SourceService != "unattested" {
			t.Fatalf("放开时审计仍要标出未归因，实际 %+v", rows)
		}
	})
}

func TestRevokeReconnectTicketSingleHappyPath(t *testing.T) {
	e := newTestEnv(t)
	e.seedTicket(tkSeed{ticketID: "lgwt_a", user: mid, connID: "c-a", nodeID: nodeA, leaseID: "lgwl_a", bit: true})
	req := revokeReq("r-revoke")
	req.TicketId = "lgwt_a"
	req.TraceId = "trace-revoke"

	reply, err := NewRevokeReconnectTicketLogic(ctxOperator(t, "1001"), e.Svc).RevokeReconnectTicket(req)
	if err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if !reply.GetRevoked() || reply.GetRevokedCount() != 1 ||
		reply.GetState() != rpc.TicketState_TICKET_STATE_REVOKED {
		t.Fatalf("撤销结论不对: %+v", reply)
	}
	row := revokedRow(t, e, "lgwt_a")
	if row.State != model.TicketStateRevoked || row.RevokeReason != "banned" || row.RevokedBy != "ops-1001" {
		t.Fatalf("票据行必须记原因与操作者: %+v", row)
	}
	if e.Leases.hasTicketBit(row.TicketHash) {
		t.Fatalf("Redis 有效位必须同步删除（否则撤销延迟到票据自然过期）")
	}
	rows := e.Logs.all()
	if len(rows) != 1 {
		t.Fatalf("一次真实推进应留 1 行处置审计，实际 %d 行", len(rows))
	}
	if rows[0].State != model.BroadcastLogSent || rows[0].DropReason != model.DropOK || rows[0].Kind != model.KindModeration {
		t.Fatalf("处置留痕应为 SENT/OK/MODERATION: %+v", rows[0])
	}
	if rows[0].SenderMid != 1001 || rows[0].SenderRole != model.RoleOperator {
		t.Fatalf("审计主体取自归因 metadata（1001/OPERATOR），实际 %+v", rows[0])
	}
	if !strings.HasPrefix(rows[0].MessageId, "ticket-revoke:banned:") {
		t.Fatalf("message_id 应带原因短标，实际 %s", rows[0].MessageId)
	}
	if rows[0].TraceId != "trace-revoke" {
		t.Fatalf("trace_id 未落审计: %+v", rows[0])
	}
}

func TestRevokeReconnectTicketNotFoundAndMismatch(t *testing.T) {
	t.Run("票据不存在", func(t *testing.T) {
		e := newTestEnv(t)
		req := revokeReq("r-404")
		req.TicketId = "lgwt_missing"
		_, err := NewRevokeReconnectTicketLogic(ctxOperator(t, "1001"), e.Svc).RevokeReconnectTicket(req)
		if !errors.Is(err, model.ErrTicketNotFound) {
			t.Fatalf("应回 ErrTicketNotFound（没有真实状态可回显），实际 %v", err)
		}
		if len(e.Logs.rows) != 0 {
			t.Fatalf("定位不到行时无房号可记，不该造审计行，实际 %d 行", len(e.Logs.rows))
		}
	})

	t.Run("房间号与票据归属不符", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedTicket(tkSeed{ticketID: "lgwt_a", user: mid, connID: "c-a", nodeID: nodeA, bit: true})
		req := revokeReq("r-room")
		req.TicketId = "lgwt_a"
		req.RoomId = otherRoom
		_, err := NewRevokeReconnectTicketLogic(ctxOperator(t, "1001"), e.Svc).RevokeReconnectTicket(req)
		if !errors.Is(err, model.ErrTripletMismatch) {
			t.Fatalf("应回 ErrTripletMismatch，实际 %v", err)
		}
		rows := e.Logs.all()
		if len(rows) != 1 || rows[0].State != model.BroadcastLogDenied {
			t.Fatalf("「用别人房间号记账」是越权信号，应落 1 行 DENIED，实际 %+v", rows)
		}
		if rows[0].RoomId != otherRoom {
			t.Fatalf("审计记请求里的房间（被侵害方），实际 %d", rows[0].RoomId)
		}
		if e.Ticket.rows[0].State != model.TicketStateIssued {
			t.Fatalf("越权撤销不得推进状态")
		}
	})

	t.Run("mid 与票据归属不符", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedTicket(tkSeed{ticketID: "lgwt_a", user: mid, connID: "c-a", nodeID: nodeA, bit: true})
		req := revokeReq("r-mid")
		req.TicketId = "lgwt_a"
		req.Mid = otherMid
		if _, err := NewRevokeReconnectTicketLogic(ctxOperator(t, "1001"), e.Svc).RevokeReconnectTicket(req); !errors.Is(err, model.ErrTripletMismatch) {
			t.Fatalf("应回 ErrTripletMismatch，实际 %v", err)
		}
	})
}

func TestRevokeReconnectTicketStateMachine(t *testing.T) {
	t.Run("已使用的票撤不回", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedTicket(tkSeed{ticketID: "lgwt_u", user: mid, connID: "c-u", nodeID: nodeA, state: model.TicketStateUsed})
		req := revokeReq("r-used")
		req.TicketId = "lgwt_u"
		reply, err := NewRevokeReconnectTicketLogic(ctxOperator(t, "1001"), e.Svc).RevokeReconnectTicket(req)
		if err != nil {
			t.Fatalf("撤销已消费票是「无操作」而不是错误: %v", err)
		}
		if reply.GetRevoked() || reply.GetRevokedCount() != 0 ||
			reply.GetState() != rpc.TicketState_TICKET_STATE_USED {
			t.Fatalf("必须回显真实状态 USED 且不报错: %+v", reply)
		}
		if e.Ticket.rows[0].State != model.TicketStateUsed {
			t.Fatalf("绝不能把 USED 改写成 REVOKED")
		}
		if e.Leases.callCount("DropTicket") != 0 {
			t.Fatalf("无推进时不该动有效位")
		}
		if len(e.Logs.rows) != 0 {
			t.Fatalf("无真实推进不留处置痕，实际 %d 行", len(e.Logs.rows))
		}
	})

	t.Run("已过期的票同样无操作", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedTicket(tkSeed{ticketID: "lgwt_e", user: mid, connID: "c-e", nodeID: nodeA, state: model.TicketStateExpired})
		req := revokeReq("r-exp")
		req.TicketId = "lgwt_e"
		reply, err := NewRevokeReconnectTicketLogic(ctxOperator(t, "1001"), e.Svc).RevokeReconnectTicket(req)
		if err != nil || reply.GetState() != rpc.TicketState_TICKET_STATE_EXPIRED {
			t.Fatalf("应无操作回显 EXPIRED: err=%v reply=%+v", err, reply)
		}
	})

	t.Run("重复撤销按幂等成功且不重复审计", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedTicket(tkSeed{ticketID: "lgwt_r", user: mid, connID: "c-r", nodeID: nodeA, state: model.TicketStateRevoked})
		req := revokeReq("r-twice")
		req.TicketId = "lgwt_r"
		reply, err := NewRevokeReconnectTicketLogic(ctxOperator(t, "1001"), e.Svc).RevokeReconnectTicket(req)
		if err != nil {
			t.Fatalf("重复撤销不该报错: %v", err)
		}
		if !reply.GetRevoked() || reply.GetRevokedCount() != 0 ||
			reply.GetState() != rpc.TicketState_TICKET_STATE_REVOKED {
			t.Fatalf("应回 revoked=true/count=0/state=REVOKED: %+v", reply)
		}
		if e.Leases.callCount("DropTicket") != 1 {
			t.Fatalf("幂等分支仍要确保有效位已清（可能首次没删成）")
		}
		if len(e.Logs.rows) != 0 {
			t.Fatalf("重复撤销不重复审计，实际 %d 行", len(e.Logs.rows))
		}
	})
}

func TestRevokeReconnectTicketBatchBySubject(t *testing.T) {
	e := newTestEnv(t)
	var hashes []string
	for i := 0; i < 3; i++ {
		row, _ := e.seedTicket(tkSeed{user: mid, connID: "c-a", nodeID: nodeA, leaseID: "lgwl_a", bit: true})
		hashes = append(hashes, row.TicketHash)
	}
	otherRow, _ := e.seedTicket(tkSeed{user: otherMid, connID: "c-b", nodeID: nodeA, bit: true})
	otherRoomRow, _ := e.seedTicket(tkSeed{room: otherRoom, user: mid, connID: "c-c", nodeID: nodeA, bit: true})

	req := revokeReq("r-batch")
	reply, err := NewRevokeReconnectTicketLogic(ctxService(t, "moderation"), e.Svc).RevokeReconnectTicket(req)
	if err != nil {
		t.Fatalf("批量撤销失败: %v", err)
	}
	if !reply.GetRevoked() || reply.GetRevokedCount() != 3 ||
		reply.GetState() != rpc.TicketState_TICKET_STATE_REVOKED {
		t.Fatalf("应撤掉该主体 3 张，实际 %+v", reply)
	}
	for _, h := range hashes {
		if e.Leases.hasTicketBit(h) {
			t.Fatalf("被撤销的票据有效位必须全部删除")
		}
	}
	if !e.Leases.hasTicketBit(otherRow.TicketHash) || !e.Leases.hasTicketBit(otherRoomRow.TicketHash) {
		t.Fatalf("撤销范围必须限定在 (room_id, mid)，不得波及其它主体")
	}
	for _, row := range e.Ticket.all() {
		switch row.TicketHash {
		case otherRow.TicketHash, otherRoomRow.TicketHash:
			if row.State != model.TicketStateIssued {
				t.Fatalf("越界撤销了 %s", row.TicketId)
			}
		}
	}
	rows := e.Logs.all()
	if len(rows) != 1 {
		t.Fatalf("一次批量处置留 1 行审计，实际 %d 行", len(rows))
	}
	if rows[0].SenderRole != model.RoleService || rows[0].SourceService != "service:moderation" {
		t.Fatalf("审计主体应取归因的内部服务: %+v", rows[0])
	}
}

func TestRevokeReconnectTicketBatchConclusion(t *testing.T) {
	t.Run("该主体只剩已使用的票", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedTicket(tkSeed{user: mid, connID: "c-a", nodeID: nodeA, state: model.TicketStateUsed})
		reply, err := NewRevokeReconnectTicketLogic(ctxOperator(t, "1001"), e.Svc).RevokeReconnectTicket(revokeReq("r-only-used"))
		if err != nil {
			t.Fatalf("无票可撤是「无事可做」而不是错误: %v", err)
		}
		if reply.GetRevoked() || reply.GetRevokedCount() != 0 ||
			reply.GetState() != rpc.TicketState_TICKET_STATE_USED {
			t.Fatalf("必须回显最近一张的真实状态: %+v", reply)
		}
		if len(e.Logs.rows) != 0 {
			t.Fatalf("无推进不留处置痕")
		}
	})

	t.Run("该主体从来没有票", func(t *testing.T) {
		e := newTestEnv(t)
		_, err := NewRevokeReconnectTicketLogic(ctxOperator(t, "1001"), e.Svc).RevokeReconnectTicket(revokeReq("r-none"))
		if !errors.Is(err, model.ErrTicketNotFound) {
			t.Fatalf("一行都没有应回 ErrTicketNotFound，实际 %v", err)
		}
	})

	t.Run("票据表读数故障原样上抛", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedTicket(tkSeed{user: mid, connID: "c-a", nodeID: nodeA})
		e.Ticket.fail("List", errors.New("db down"))
		if _, err := NewRevokeReconnectTicketLogic(ctxOperator(t, "1001"), e.Svc).RevokeReconnectTicket(revokeReq("r-db")); err == nil {
			t.Fatalf("枚举失败时不得当作「没有票可撤」")
		}
	})
}

func TestRevokeReconnectTicketIdempotentReplay(t *testing.T) {
	e := newTestEnv(t)
	row, _ := e.seedTicket(tkSeed{ticketID: "lgwt_a", user: mid, connID: "c-a", nodeID: nodeA, bit: true})
	req := revokeReq("r-replay")
	req.TicketId = "lgwt_a"
	ctx := ctxOperator(t, "1001")

	first, err := NewRevokeReconnectTicketLogic(ctx, e.Svc).RevokeReconnectTicket(req)
	if err != nil || !first.GetRevoked() {
		t.Fatalf("首次撤销失败: err=%v reply=%+v", err, first)
	}
	drops := e.Leases.callCount("DropTicket")
	logs := len(e.Logs.rows)

	second, err := NewRevokeReconnectTicketLogic(ctx, e.Svc).RevokeReconnectTicket(req)
	if err != nil {
		t.Fatalf("重放失败: %v", err)
	}
	if second.GetRevoked() != first.GetRevoked() || second.GetRevokedCount() != first.GetRevokedCount() ||
		second.GetState() != first.GetState() {
		t.Fatalf("三个结论字段都要回放: first=%+v second=%+v", first, second)
	}
	if e.Leases.callCount("DropTicket") != drops || len(e.Logs.rows) != logs {
		t.Fatalf("重放必须零写入零审计：DropTicket %d→%d，审计 %d→%d",
			drops, e.Leases.callCount("DropTicket"), logs, len(e.Logs.rows))
	}
	if e.Ticket.rows[0].RevokeReason != "banned" || e.Ticket.rows[0].RevokedBy != "ops-1001" {
		t.Fatalf("回放不得改写首次留痕: %+v", e.Ticket.rows[0])
	}
	if e.Leases.hasTicketBit(row.TicketHash) {
		t.Fatalf("有效位仍是删除状态")
	}
}

func TestRevokeReconnectTicketBitDropFailureStillSucceeds(t *testing.T) {
	e := newTestEnv(t)
	e.seedTicket(tkSeed{ticketID: "lgwt_a", user: mid, connID: "c-a", nodeID: nodeA, bit: true})
	e.Leases.failWith("DropTicket", repository.ErrStoreUnavailable)
	req := revokeReq("r-droperr")
	req.TicketId = "lgwt_a"

	reply, err := NewRevokeReconnectTicketLogic(ctxOperator(t, "1001"), e.Svc).RevokeReconnectTicket(req)
	if err != nil || !reply.GetRevoked() {
		t.Fatalf("DB 已 REVOKED，撤销在真值层面已生效，不该报失败: err=%v reply=%+v", err, reply)
	}
	if e.Ticket.rows[0].State != model.TicketStateRevoked {
		t.Fatalf("票据行必须已 REVOKED")
	}
}

func TestRevokeReconnectTicketStoreMissing(t *testing.T) {
	e := newTestEnv(t)
	req := revokeReq("r-nostore")
	req.TicketId = "lgwt_a"
	e.markStoreMissing()
	if _, err := NewRevokeReconnectTicketLogic(ctxOperator(t, "1001"), e.Svc).RevokeReconnectTicket(req); !errors.Is(err, repository.ErrStoreUnavailable) {
		t.Fatalf("应回 ErrStoreUnavailable，实际 %v", err)
	}
}
