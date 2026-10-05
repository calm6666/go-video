package logic

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-video/services/live-gateway/internal/config"
	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"
)

// 本文件覆盖 KickConnection（处置类写入口）。
//
// 处置接口与下发接口的判定方向相反，本文件围绕四条契约组织断言：
//  1. 授权 fail-closed：只认 gRPC metadata 归因的 OPERATOR/SERVICE，请求里的 operator 字段
//     永远不是授权输入；被拒的尝试也要留痕（不落库就无法复盘「谁试过踢谁」）。
//  2. 顺序即效果：先断票、再写 ban 窗口、再置 KICKED 清订阅、最后才关物理连接。反序会让被踢端
//     拿旧票秒级重连回来。这里用「在中途 seam 注入故障 + 断言下游计数为零」证明顺序，不改生产代码。
//  3. 幂等：request_id 命中回放首次结论（不重复计数、不延长封禁、不重复撤票）；
//     结论还没回填（pending）时宁可变通重试，也不伪造一个成功。
//  4. 结论必须诚实：没有任何东西被处置时回 kicked=false 并说清是「目标当时不在线」还是
//     「有连接但都是终态记录」；socket 关不掉只把结论降级成 TRANSPORT_UNAVAILABLE，
//     不把已经生效的失效写入重新说成失败。

// kickReq 一条按 lease_id 定位的最小处置请求（授权主体由 ctx 决定）。
func kickReq(leaseID, requestID string) *rpc.KickConnectionReq {
	return &rpc.KickConnectionReq{
		RoomId: roomID, LeaseId: leaseID, Reason: "risk", Operator: "ops-field", RequestId: requestID,
	}
}

// kickByMidReq 一条按 (room, mid) 定位的处置请求。
func kickByMidReq(user int64, requestID string) *rpc.KickConnectionReq {
	return &rpc.KickConnectionReq{
		RoomId: roomID, Mid: user, Reason: "risk", Operator: "ops-field", RequestId: requestID,
	}
}

// kickLease 播种一条与 kickReq 同源的活租约。
func kickLease(e *testEnv, leaseID string, user int64, role int32) *repository.LeaseRecord {
	return e.seedActiveLease(leaseID, "conn-"+leaseID, nodeA, roomID, user, role)
}

// assertKickNothingExecuted 断言这条路径连定位与执行都没开始（授权/参数阶段的唯一硬证据）。
func assertKickNothingExecuted(t *testing.T, e *testEnv) {
	t.Helper()
	for _, m := range []string{"Get", "ListUserLeases", "Unsubscribe", "Put", "SetBan", "DropTicket", "ClaimRequest"} {
		if n := e.Leases.callCount(m); n != 0 {
			t.Fatalf("不该进入定位/执行阶段，实际 %s 调用 %d 次", m, n)
		}
	}
	if n := e.Leases.callCount("BanUntil"); n != 0 {
		t.Fatalf("不该进入 ban 窗口阶段，实际 BanUntil 调用 %d 次", n)
	}
	if e.Ticket.inserts != 0 || e.Routes.registers != 0 {
		t.Fatalf("Kick 不该写票据或路由，实际 insert=%d register=%d", e.Ticket.inserts, e.Routes.registers)
	}
	if n := len(e.Ticket.rows); n != 0 {
		t.Fatalf("前提不成立：本文件不预置票据行，实际 %d 行", n)
	}
}

// --- 参数门禁 ---

func TestKickConnectionParameterGate(t *testing.T) {
	cases := []struct {
		name string
		req  *rpc.KickConnectionReq
		want error
		text string
	}{
		{"空请求体", nil, model.ErrEmptyReason, ""},
		{"房间号非正", func() *rpc.KickConnectionReq {
			r := kickReq("lgwl_kick_p", "req-kick-p1")
			r.RoomId = 0
			return r
		}(), model.ErrInvalidRoomID, ""},
		{"mid 为负", func() *rpc.KickConnectionReq {
			r := kickReq("lgwl_kick_p", "req-kick-p2")
			r.Mid = -1
			return r
		}(), model.ErrInvalidMid, ""},
		{"既无 lease_id 又无 mid（影响面全房间）", func() *rpc.KickConnectionReq {
			r := kickReq("", "req-kick-p3")
			r.Mid = 0
			return r
		}(), model.ErrEmptyLeaseID, "whole room"},
		{"reason 为空", func() *rpc.KickConnectionReq {
			r := kickReq("lgwl_kick_p", "req-kick-p5")
			r.Reason = "  "
			return r
		}(), model.ErrEmptyReason, ""},
		{"operator 为空", func() *rpc.KickConnectionReq {
			r := kickReq("lgwl_kick_p", "req-kick-p6")
			r.Operator = ""
			return r
		}(), model.ErrEmptyOperator, ""},
		{"operator 含换行", func() *rpc.KickConnectionReq {
			r := kickReq("lgwl_kick_p", "req-kick-p7")
			r.Operator = "ops\nfake line"
			return r
		}(), model.ErrEmptyOperator, "newlines"},
		{"request_id 为空", func() *rpc.KickConnectionReq {
			r := kickReq("lgwl_kick_p", "  ")
			return r
		}(), model.ErrEmptyRequestID, ""},
		{"request_id 超长", func() *rpc.KickConnectionReq {
			r := kickReq("lgwl_kick_p", strings.Repeat("r", 65))
			return r
		}(), model.ErrEmptyRequestID, ""},
		{"conn_id 含空白", func() *rpc.KickConnectionReq {
			r := kickReq("lgwl_kick_p", "req-kick-p10")
			r.ConnId = "c 1"
			return r
		}(), model.ErrEmptyConnID, ""},
		{"ban_seconds 为负", func() *rpc.KickConnectionReq {
			r := kickReq("lgwl_kick_p", "req-kick-p11")
			r.BanSeconds = -1
			return r
		}(), nil, "ban_seconds must be >= 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			kickLease(e, "lgwl_kick_p", mid, model.RoleViewer)
			// 用已归因主体：这样失败只能来自参数校验，不会和授权门禁混在一起。
			got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(tc.req)
			if err == nil {
				t.Fatalf("参数非法时必须报错而不是给结论（reply=%+v）", got)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("期望错误 %v，实际 %v", tc.want, err)
			}
			if tc.text != "" && !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("错误文本应含 %q（要能解释为什么拒），实际 %v", tc.text, err)
			}
			if got != nil {
				t.Fatalf("参数非法时不得返回响应体，实际 %+v", got)
			}
			assertKickNothingExecuted(t, e)
			assertNoWrites(t, e)
		})
	}
	// 判别性对照：mid=0 只要给了 lease_id 就是合法定位（游客连接也可被断开）。
	e := newTestEnv(t)
	kickLease(e, "lgwl_kick_guest", 0, model.RoleViewer)
	req := kickReq("lgwl_kick_guest", "req-kick-guest-locator")
	req.Mid = 0
	got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(req)
	if err != nil || !got.GetKicked() {
		t.Fatalf("游客连接按 lease_id 应可被处置，实际 err=%v reply=%+v", err, got)
	}
}

// --- 处置授权：fail-closed，且排在定位与执行之前 ---

func TestKickConnectionDispositionAuthIsFailClosed(t *testing.T) {
	t.Run("未归因主体被拒并留痕", func(t *testing.T) {
		e := newTestEnv(t)
		rec := kickLease(e, "lgwl_kick_unatt", mid, model.RoleViewer)
		got, err := NewKickConnectionLogic(ctxClient(t), e.Svc).KickConnection(kickReq("lgwl_kick_unatt", "req-kick-unatt"))
		if err != nil {
			t.Fatalf("越权要回结论而不是 error（否则上游会把越权当依赖故障重试）: %v", err)
		}
		if got.GetKicked() || got.GetDenyReason() != dropReason(model.DropPermissionDenied) {
			t.Fatalf("未归因主体竟完成了处置: %+v", got)
		}
		// 授权必须先于「读目标」：否则一次越权探测就能枚举房间内的连接。
		assertKickNothingExecuted(t, e)
		if rec.State != model.LeaseStateActive {
			t.Fatalf("被拒的处置改写了租约状态: %+v", rec)
		}
		row := onlyAudit(t, e)
		if row.State != model.BroadcastLogDenied || row.DropReason != model.DropPermissionDenied {
			t.Fatalf("被拒尝试应落 DENIED，实际 %d/%d", row.State, row.DropReason)
		}
		// 审计里的主体只能是归因结果：未归因记 unattested、发起 mid 记 0，
		// 绝不把请求字段 operator="ops-field" 或被告的 mid 当成发起者。
		if row.SourceService != "unattested" || row.SenderMid != 0 {
			t.Fatalf("未归因尝试的审计主体应收紧，实际 source=%q sender_mid=%d", row.SourceService, row.SenderMid)
		}
		// 审计正文列已不存在（只有 payload_digest/payload_bytes），整行渲染都不得出现
		// 请求自报的 operator，否则等于把调用方自填字段写进了留痕。
		if strings.Contains(fmt.Sprintf("%+v", row), "ops-field") {
			t.Fatalf("审计行里出现了请求自报的 operator: %+v", row)
		}
	})

	t.Run("自报角色不构成内部主体", func(t *testing.T) {
		e := newTestEnv(t)
		kickLease(e, "lgwl_kick_selfclaim", mid, model.RoleViewer)
		ctx := ctxAs(t, "true", "ANCHOR", "9100", "")
		got, err := NewKickConnectionLogic(ctx, e.Svc).KickConnection(kickReq("lgwl_kick_selfclaim", "req-kick-selfclaim"))
		if err != nil {
			t.Fatalf("越权回结论: %v", err)
		}
		if got.GetKicked() || got.GetDenyReason() != dropReason(model.DropPermissionDenied) {
			t.Fatalf("attested 但角色不是 OPERATOR/SERVICE 竟被放行: %+v", got)
		}
		assertKickNothingExecuted(t, e)
	})

	// 判别性对照：同一份请求只把归因换成 OPERATOR，就必须真的执行。
	e := newTestEnv(t)
	kickLease(e, "lgwl_kick_ctrl", mid, model.RoleViewer)
	got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(kickReq("lgwl_kick_ctrl", "req-kick-ctrl"))
	if err != nil || !got.GetKicked() {
		t.Fatalf("对照用例（归因 OPERATOR）应完成处置，实际 err=%v reply=%+v", err, got)
	}
	// 读回存储里的落库结果：Put 会替换存储侧的记录对象，种子指针不反映写入。
	if after := hbGet(t, e, "lgwl_kick_ctrl"); after.State != model.LeaseStateKicked {
		t.Fatalf("对照用例未把 KICKED 落进存储: %+v", after)
	}
}

// 放开过渡开关只放开执行，不放开审计里的主体归因。
func TestKickConnectionUnattestedEscapeHatchKeepsAuditHonest(t *testing.T) {
	e := newTestEnv(t)
	e.apply(func(c *config.LiveGatewayConf) { c.AllowUnattestedOperatorWrites = true })
	kickLease(e, "lgwl_kick_escape", mid, model.RoleViewer)
	got, err := NewKickConnectionLogic(ctxClient(t), e.Svc).KickConnection(kickReq("lgwl_kick_escape", "req-kick-escape"))
	if err != nil || !got.GetKicked() {
		t.Fatalf("开关打开时应可执行（过渡环境），实际 err=%v reply=%+v", err, got)
	}
	row := onlyAudit(t, e)
	if row.SourceService != "unattested" || row.SenderMid != 0 {
		t.Fatalf("开关不能把未归因主体洗白成运营：source=%q sender_mid=%d", row.SourceService, row.SenderMid)
	}
	if row.State != model.BroadcastLogSent {
		t.Fatalf("处置已生效，审计状态应为 SENT，实际 %d", row.State)
	}
}

// --- 目标定位一致性 ---

func TestKickConnectionLocatorConsistency(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*rpc.KickConnectionReq)
	}{
		{"拿别的房间的租约来踢", func(r *rpc.KickConnectionReq) { r.RoomId = otherRoom }},
		{"拿别人的租约冒充目标 mid", func(r *rpc.KickConnectionReq) { r.Mid = otherMid }},
		{"conn_id 与租约不符", func(r *rpc.KickConnectionReq) { r.ConnId = "conn-hijacked" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			rec := kickLease(e, "lgwl_kick_loc", mid, model.RoleViewer)
			req := kickReq("lgwl_kick_loc", "req-kick-loc-"+strings.ReplaceAll(tc.name, " ", ""))
			tc.mut(req)
			got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(req)
			if !errors.Is(err, model.ErrTripletMismatch) {
				t.Fatalf("定位不一致必须报错（绝不按更宽松的字段执行），实际 err=%v reply=%+v", err, got)
			}
			if got != nil {
				t.Fatalf("定位不一致时不得返回响应体，实际 %+v", got)
			}
			if rec.State != model.LeaseStateActive {
				t.Fatalf("被拒的处置动到了租约: %+v", rec)
			}
			// 关键顺序证据：幂等键都没占，纯校验失败不该把 request_id 烧掉。
			if n := e.Leases.callCount("ClaimRequest"); n != 0 {
				t.Fatalf("定位校验失败却烧掉了 request_id，ClaimRequest %d 次", n)
			}
			if n := e.Leases.callCount("Unsubscribe") + e.Leases.callCount("Put") + e.Leases.callCount("SetBan"); n != 0 {
				t.Fatalf("定位校验失败却执行了处置，写调用 %d 次", n)
			}
			// 越权处置尝试要留痕，且审计房间是租约自身的房间（受害方）。
			row := onlyAudit(t, e)
			if row.State != model.BroadcastLogDenied || row.RoomId != roomID {
				t.Fatalf("越权处置审计应为 DENIED 且记受害房间，实际 %+v", row)
			}
		})
	}
	// 判别性对照：同一份请求只把房间号改回来，就必须执行。
	e := newTestEnv(t)
	kickLease(e, "lgwl_kick_loc_ok", mid, model.RoleViewer)
	ok := kickReq("lgwl_kick_loc_ok", "req-kick-loc-ok")
	ok.RoomId = roomID
	if got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(ok); err != nil || !got.GetKicked() {
		t.Fatalf("对照用例应完成处置，实际 err=%v reply=%+v", err, got)
	}
}

// 按 mid 定位时扫描量撞上限必须报错：只踢一半人却宣称踢完是假结论。
func TestKickConnectionScanLimitRefusesHalfConclusion(t *testing.T) {
	e := newTestEnv(t)
	e.apply(func(c *config.LiveGatewayConf) { c.ConnectionScanLimit = 2 })
	kickLease(e, "lgwl_kick_scan1", mid, model.RoleViewer)
	kickLease(e, "lgwl_kick_scan2", mid, model.RoleViewer)
	got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(kickByMidReq(mid, "req-kick-scan"))
	if !errors.Is(err, model.ErrScanLimitTooLarge) || got != nil {
		t.Fatalf("扫描撞上限必须报错而不是给半份结论，实际 err=%v reply=%+v", err, got)
	}
	if !strings.Contains(err.Error(), "ConnectionScanLimit") {
		t.Fatalf("错误要给出可执行的调参结论，实际 %v", err)
	}
	if n := e.Leases.callCount("ClaimRequest"); n != 0 {
		t.Fatalf("定位阶段失败不该占幂等键，ClaimRequest %d 次", n)
	}
	assertKickNoEffectOnLeases(t, e, "lgwl_kick_scan1", "lgwl_kick_scan2")

	// 判别性对照：只把上限放宽一位，同一次请求就必须全部踢到。
	e.apply(func(c *config.LiveGatewayConf) { c.ConnectionScanLimit = 3 })
	if g2, err2 := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(kickByMidReq(mid, "req-kick-scan-ok")); err2 != nil ||
		g2.GetKickedConnections() != 2 {
		t.Fatalf("放宽上限后应踢掉 2 条连接，实际 err=%v reply=%+v", err2, g2)
	}
}

// assertKickNoEffectOnLeases 断言这些租约仍是 ACTIVE（未被处置）。
func assertKickNoEffectOnLeases(t *testing.T, e *testEnv, ids ...string) {
	t.Helper()
	for _, id := range ids {
		rec, err := e.Leases.Get(ctxClient(t), id)
		if err != nil || rec == nil {
			t.Fatalf("回读 %s 失败: %v", id, err)
		}
		if rec.State != model.LeaseStateActive {
			t.Fatalf("被拒的处置却推进了 %s 的状态机: %+v", id, rec)
		}
	}
}

// --- 执行顺序：先断票 → ban 窗口 → 置 KICKED → 关物理连接 ---

func TestKickConnectionRevokesTicketsBeforeDroppingConnections(t *testing.T) {
	t.Run("撤票失败则一条连接都不许被断", func(t *testing.T) {
		e := newTestEnv(t)
		kickLease(e, "lgwl_kick_order", mid, model.RoleViewer)
		tk, _ := e.seedTicket(tkSeed{ticketID: "lgwt_order_1", leaseID: "lgwl_kick_order", bit: true})
		e.Ticket.fail("RevokeByRoomMid", errTicketRevokeProbe)
		req := kickReq("lgwl_kick_order", "req-kick-order")
		req.RevokeTickets = true
		if _, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(req); !errors.Is(err, errTicketRevokeProbe) {
			t.Fatalf("撤票故障必须原样上抛，实际 %v", err)
		}
		// 顺序的唯一硬证据：下游三项全部为零。
		if n := e.Leases.callCount("Unsubscribe") + e.Leases.callCount("Put"); n != 0 {
			t.Fatalf("票还没撤销就断了连接（反序会被旧票秒级重连回来），下游写 %d 次", n)
		}
		if n := e.Leases.callCount("SetBan"); n != 0 {
			t.Fatalf("撤票失败后不该继续写 ban 窗口，SetBan %d 次", n)
		}
		if n := e.Leases.callCount("DropTicket"); n != 0 {
			t.Fatalf("MySQL 未撤销就不该删有效位，DropTicket %d 次", n)
		}
		if e.Tp.closed != 0 {
			t.Fatalf("撤票失败却关了物理连接")
		}
		if tk.State != model.TicketStateIssued {
			t.Fatalf("前提不成立：撤票被注入失败，行应仍是 ISSUED，实际 %d", tk.State)
		}
	})

	t.Run("枚举失败则不许改 MySQL 行", func(t *testing.T) {
		e := newTestEnv(t)
		kickLease(e, "lgwl_kick_list", mid, model.RoleViewer)
		e.seedTicket(tkSeed{ticketID: "lgwt_list_1", leaseID: "lgwl_kick_list", bit: true})
		e.Ticket.fail("List", errTicketListProbe)
		req := kickReq("lgwl_kick_list", "req-kick-list")
		req.RevokeTickets = true
		if _, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(req); !errors.Is(err, errTicketListProbe) {
			t.Fatalf("枚举故障必须上抛，实际 %v", err)
		}
		if n := e.Leases.callCount("RevokeByRoomMid") + e.Leases.callCount("DropTicket"); n != 0 {
			t.Fatalf("拿不到 hash 就不该撤销与删有效位，实际 %d 次", n)
		}
		for _, r := range e.Ticket.all() {
			if r.State != model.TicketStateIssued {
				t.Fatalf("List 失败却推进了票据状态: %+v", r)
			}
		}
	})

	t.Run("正常顺序：行 REVOKED 且 Redis 有效位清空", func(t *testing.T) {
		e := newTestEnv(t)
		kickLease(e, "lgwl_kick_tickets", mid, model.RoleViewer)
		row1, _ := e.seedTicket(tkSeed{ticketID: "lgwt_t1", leaseID: "lgwl_kick_tickets", bit: true})
		row2, _ := e.seedTicket(tkSeed{ticketID: "lgwt_t2", leaseID: "lgwl_kick_tickets", bit: true})
		// 别人的票不许被牵连。
		other, _ := e.seedTicket(tkSeed{ticketID: "lgwt_other", user: otherMid, bit: true})
		req := kickReq("lgwl_kick_tickets", "req-kick-tickets")
		req.RevokeTickets = true
		req.BanSeconds = 60
		got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(req)
		if err != nil {
			t.Fatalf("处置失败: %v", err)
		}
		if got.GetRevokedTickets() != 2 {
			t.Fatalf("应撤销 2 张票据，实际 %+v", got)
		}
		for _, r := range []*model.LiveGwReconnectTicket{row1, row2} {
			back := revokedRow(t, e, r.TicketId)
			if back.State != model.TicketStateRevoked {
				t.Fatalf("票据 %s 未被撤销: %+v", r.TicketId, back)
			}
			if back.RevokeReason != "risk" || back.RevokedBy != "ops-field" {
				t.Fatalf("撤销留痕不完整: %+v", back)
			}
			if e.Leases.hasTicketBit(r.TicketHash) {
				t.Fatalf("票据 %s 的 Redis 有效位残留：只改 DB 会让兑换快路径继续放行", r.TicketId)
			}
		}
		if !e.Leases.hasTicketBit(other.TicketHash) {
			t.Fatalf("别人的票据有效位被误删")
		}
	})

	// 有效位删除失败只记日志：DB 已是 REVOKED，兑换的条件更新打不中，凭据实际已失效。
	t.Run("有效位删除失败不撤销已完成的处置", func(t *testing.T) {
		e := newTestEnv(t)
		kickLease(e, "lgwl_kick_dropfail", mid, model.RoleViewer)
		tk, _ := e.seedTicket(tkSeed{ticketID: "lgwt_drop", leaseID: "lgwl_kick_dropfail", bit: true})
		e.Leases.failWith("DropTicket", errDropTicketProbe)
		req := kickReq("lgwl_kick_dropfail", "req-kick-dropfail")
		req.RevokeTickets = true
		got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(req)
		if err != nil {
			t.Fatalf("有效位删除失败不该升级成整体失败: %v", err)
		}
		if !got.GetKicked() || got.GetRevokedTickets() != 1 {
			t.Fatalf("处置结论不完整: %+v", got)
		}
		if revokedRow(t, e, tk.TicketId).State != model.TicketStateRevoked {
			t.Fatalf("票据行未撤销")
		}
	})

	// 游客没有 (room, mid) 维度的封禁键，批量撤票必须跳过而不是「把整个房间的游客都撤了」。
	t.Run("游客跳过批量撤票但仍断开连接", func(t *testing.T) {
		e := newTestEnv(t)
		kickLease(e, "lgwl_kick_gtoast", 0, model.RoleViewer)
		guestTk, _ := e.seedTicket(tkSeed{ticketID: "lgwt_guest", guest: true, bit: true})
		req := kickReq("lgwl_kick_gtoast", "req-kick-gtoast")
		req.Mid = 0
		req.RevokeTickets = true
		got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(req)
		if err != nil || !got.GetKicked() {
			t.Fatalf("游客连接应可断开，实际 err=%v reply=%+v", err, got)
		}
		if got.GetRevokedTickets() != 0 {
			t.Fatalf("游客不该走批量撤票（影响面是整个房间的游客）: %+v", got)
		}
		if revokedRow(t, e, guestTk.TicketId).State != model.TicketStateIssued {
			t.Fatalf("批量撤票越过了游客边界，把房间级票据撤销了")
		}
	})
}

var (
	errTicketRevokeProbe = errors.New("ticket-revoke-probe")
	errTicketListProbe   = errors.New("ticket-list-probe")
	errDropTicketProbe   = errors.New("drop-ticket-probe")
)

// --- ban 窗口 ---

func TestKickConnectionBanWindow(t *testing.T) {
	t.Run("窗口只延长不缩短", func(t *testing.T) {
		e := newTestEnv(t)
		kickLease(e, "lgwl_kick_ban_long", mid, model.RoleViewer)
		long := e.Clock.unix() + 3600
		if err := e.Leases.SetBan(ctxClient(t), roomID, mid, long, false); err != nil {
			t.Fatalf("预置全局封禁失败: %v", err)
		}
		req := kickReq("lgwl_kick_ban_long", "req-kick-ban-long")
		req.BanSeconds = 60
		req.Reason = "banned" // 用户维度：与预置的那把键同一个维度
		got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(req)
		if err != nil {
			t.Fatalf("处置失败: %v", err)
		}
		if got.GetBanUntil() != long {
			t.Fatalf("一次短封禁冲掉了已生效的长封禁（%d → %d）", long, got.GetBanUntil())
		}
	})

	t.Run("无既有窗口时按请求值写", func(t *testing.T) {
		e := newTestEnv(t)
		kickLease(e, "lgwl_kick_ban_new", mid, model.RoleViewer)
		req := kickReq("lgwl_kick_ban_new", "req-kick-ban-new")
		req.BanSeconds = 120
		got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(req)
		if err != nil || got.GetBanUntil() != e.Clock.unix()+120 {
			t.Fatalf("ban_until 应为 now+120，实际 err=%v reply=%+v", err, got)
		}
	})

	// 封禁维度由 reason 决定：risk/banned 是「人」的问题（跨房间），其余是影响面更小的房间维度。
	t.Run("封禁键维度由原因决定", func(t *testing.T) {
		for _, tc := range []struct {
			reason   string
			userWide bool
		}{
			{"banned", true}, {"ban", true}, {"risk", true}, {"RISK_CONTROL", true},
			{"room_closed", false}, {"anchor_block", false}, {"", false},
		} {
			e := newTestEnv(t)
			kickLease(e, "lgwl_kick_dim_"+tc.reason, mid, model.RoleViewer)
			req := kickReq("lgwl_kick_dim_"+tc.reason, "req-kick-dim-"+tc.reason)
			req.BanSeconds = 60
			req.Reason = tc.reason
			if tc.reason == "" {
				req.Reason = "room_closed" // reason 必填，用房间维度原因代表「非用户维度」
			}
			if _, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(req); err != nil {
				t.Fatalf("reason=%s 处置失败: %v", tc.reason, err)
			}
			e.Leases.mu.Lock()
			_, userKey := e.Leases.bans[banUserKey(mid)]
			_, roomKey := e.Leases.bans[banRoomKey(roomID, mid)]
			e.Leases.mu.Unlock()
			if userKey != tc.userWide {
				t.Fatalf("reason=%q 应%s用户维度键，实际 user=%v room=%v",
					tc.reason, map[bool]string{true: "写", false: "不写"}[tc.userWide], userKey, roomKey)
			}
			if roomKey == tc.userWide {
				t.Fatalf("reason=%q 的封禁键维度写反了（user=%v room=%v）", tc.reason, userKey, roomKey)
			}
		}
	})

	// TODO(缺陷)：ban_seconds>0 但目标是游客时，ErrInvalidMid 直到第 4b 步才发现，
	// 此时 request_id 已被 ClaimRequest 占成 pending（kickconnectionlogic.go:161 → :196），
	// 客户端用同一个 request_id 重试会拿到 ErrRequestIdDuplicated「retry」而不是真实的
	// ErrInvalidMid（kickconnectionlogic.go:165-173）。本用例钉住两次答复不一致。
	t.Run("游客封禁请求报参数错但已占住幂等键", func(t *testing.T) {
		e := newTestEnv(t)
		kickLease(e, "lgwl_kick_gban", 0, model.RoleViewer)
		req := kickReq("lgwl_kick_gban", "req-kick-gban")
		req.Mid = 0
		req.BanSeconds = 60
		_, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(req)
		if !errors.Is(err, model.ErrInvalidMid) {
			t.Fatalf("游客没有封禁键，应报 ErrInvalidMid，实际 %v", err)
		}
		if n := e.Leases.callCount("SetBan"); n != 0 {
			t.Fatalf("不该写入房间级游客封禁（影响面全房间），SetBan %d 次", n)
		}
		if n := e.Leases.callCount("ClaimRequest"); n != 1 {
			t.Fatalf("前提不成立：幂等键已被占住，ClaimRequest 实际 %d 次", n)
		}
		_, err2 := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(req)
		if !errors.Is(err2, model.ErrRequestIdDuplicated) {
			t.Fatalf("当前实现：重试拿到的是「请换 request_id」而不是 ErrInvalidMid，实际 %v", err2)
		}
		if errors.Is(err2, model.ErrInvalidMid) {
			t.Fatalf("缺陷已修复：重试能拿到真因，请把本用例改成断言 ErrInvalidMid")
		}
	})
}

// --- 幂等 ---

func TestKickConnectionReplayReturnsFirstConclusion(t *testing.T) {
	e := newTestEnv(t)
	kickLease(e, "lgwl_kick_idem", mid, model.RoleViewer)
	tk, _ := e.seedTicket(tkSeed{ticketID: "lgwt_idem", leaseID: "lgwl_kick_idem", bit: true})
	req := kickReq("lgwl_kick_idem", "req-kick-idem")
	req.RevokeTickets = true
	req.BanSeconds = 600
	logic := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc)

	first, err := logic.KickConnection(req)
	if err != nil || !first.GetKicked() {
		t.Fatalf("首次处置失败: err=%v reply=%+v", err, first)
	}
	snapshot := struct {
		unsubscribe, put, setBan, dropTicket, revoke int
		closed                                       int32
	}{
		e.Leases.callCount("Unsubscribe"), e.Leases.callCount("Put"), e.Leases.callCount("SetBan"),
		e.Leases.callCount("DropTicket"), e.Leases.callCount("RevokeByRoomMid"), e.Tp.closed,
	}
	// 推进时钟：若重放走了执行路径，ban_until 会跟着变（只延长不缩短会把它推得更远）。
	e.Clock.advance(5 * time.Second)

	second, err := logic.KickConnection(req)
	if err != nil {
		t.Fatalf("重放不应失败: %v", err)
	}
	if second.GetKicked() != first.GetKicked() || second.GetKickedConnections() != first.GetKickedConnections() ||
		second.GetRevokedTickets() != first.GetRevokedTickets() || second.GetBanUntil() != first.GetBanUntil() ||
		second.GetDenyReason() != first.GetDenyReason() {
		t.Fatalf("重放必须回到同一份结论，实际首次 %+v 二次 %+v", first, second)
	}
	if got := e.Leases.callCount("Unsubscribe"); got != snapshot.unsubscribe {
		t.Fatalf("重放重复断连（%d → %d）", snapshot.unsubscribe, got)
	}
	if got := e.Leases.callCount("SetBan"); got != snapshot.setBan {
		t.Fatalf("重放重复写封禁窗口（%d → %d）", snapshot.setBan, got)
	}
	if got := e.Leases.callCount("DropTicket"); got != snapshot.dropTicket {
		t.Fatalf("重放重复撤票（%d → %d）", snapshot.dropTicket, got)
	}
	if revokedRow(t, e, tk.TicketId).State != model.TicketStateRevoked {
		t.Fatalf("前提不成立：首次处置应已撤销票据")
	}
	// 同一 request_id 的处置在审计里只有一行（message_id 带 request_id 摘要，撞唯一键）。
	if len(e.Logs.all()) != 1 {
		t.Fatalf("重放不该灌第二行审计: %+v", e.Logs.all())
	}
}

func TestKickConnectionReplayWithPendingConclusionAsksForRetry(t *testing.T) {
	e := newTestEnv(t)
	kickLease(e, "lgwl_kick_pending", mid, model.RoleViewer)
	e.Leases.seedRequest(kickRPCName, "req-kick-pending", lgwOutcomePending)
	got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(kickReq("lgwl_kick_pending", "req-kick-pending"))
	if !errors.Is(err, model.ErrRequestIdDuplicated) || got != nil {
		t.Fatalf("首次结论未回填时必须宁可变通重试，也不伪造成功，实际 err=%v reply=%+v", err, got)
	}
	if !strings.Contains(err.Error(), "retry") {
		t.Fatalf("错误要给出可执行结论（同 request_id 重试），实际 %v", err)
	}
	// 而且这条路径不能已经执行处置。
	if n := e.Leases.callCount("Unsubscribe") + e.Leases.callCount("Put") + e.Leases.callCount("SetBan"); n != 0 {
		t.Fatalf("结论不明的重放却执行了处置，写调用 %d 次", n)
	}
}

func TestKickConnectionClaimRequestFailurePropagates(t *testing.T) {
	e := newTestEnv(t)
	rec := kickLease(e, "lgwl_kick_claim", mid, model.RoleViewer)
	e.Leases.failWith("ClaimRequest", errClaimProbe)
	if _, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(kickReq("lgwl_kick_claim", "req-kick-claim")); !errors.Is(err, errClaimProbe) {
		t.Fatalf("幂等窗口不可用不得当成首次请求执行（否则处置会重复计数），实际 %v", err)
	}
	if rec.State != model.LeaseStateActive {
		t.Fatalf("幂等窗口故障时已经推进了状态机")
	}
}

// --- 状态机与结论诚实 ---

// 重复 Kick 不重复计入 kicked_connections（终态记录只当墓碑留着）。
func TestKickConnectionTerminalTargetsNotCounted(t *testing.T) {
	e := newTestEnv(t)
	kickLease(e, "lgwl_kick_alive", mid, model.RoleViewer)
	dead := kickLease(e, "lgwl_kick_dead", mid, model.RoleViewer)
	dead.State = model.LeaseStateReleased
	got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(kickByMidReq(mid, "req-kick-mixed"))
	if err != nil {
		t.Fatalf("处置失败: %v", err)
	}
	if got.GetKickedConnections() != 1 {
		t.Fatalf("kicked_connections 应为 1（终态记录不计入），实际 %+v", got)
	}
	if a := hbGet(t, e, "lgwl_kick_alive"); a.State != model.LeaseStateKicked {
		t.Fatalf("活租约未置 KICKED: %+v", a)
	}
	if d, derr := e.Leases.Get(ctxClient(t), "lgwl_kick_dead"); derr != nil || d.State != model.LeaseStateReleased {
		t.Fatalf("终态记录被改写了: %+v err=%v", d, derr)
	}
	// 判别性对照：目标全是终态时不能宣称「踢完了」，也不能说成「目标不在线」。
	e2 := newTestEnv(t)
	only := kickLease(e2, "lgwl_kick_only_dead", mid, model.RoleViewer)
	only.State = model.LeaseStateKicked
	g2, err2 := NewKickConnectionLogic(ctxOperator(t, "1001"), e2.Svc).KickConnection(kickByMidReq(mid, "req-kick-deadonly"))
	if err2 != nil {
		t.Fatalf("处置失败: %v", err2)
	}
	if g2.GetKicked() || g2.GetDenyReason() != dropReason(model.DropNoSubscriber) {
		t.Fatalf("有连接但都是终态记录时应回 NO_SUBSCRIBER，实际 %+v", g2)
	}
}

// 目标当时就不在线：如实回 false + BAD_TICKET，而不是「踢成功了」。
func TestKickConnectionMissingTargetIsReportedHonestly(t *testing.T) {
	e := newTestEnv(t)
	got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(kickByMidReq(mid, "req-kick-absent"))
	if err != nil || got.GetKicked() {
		t.Fatalf("目标不存在不应报错也不应宣称成功，实际 err=%v reply=%+v", err, got)
	}
	if got.GetDenyReason() != dropReason(model.DropBadTicket) {
		t.Fatalf("应回 BAD_TICKET（目标当时不在线），实际 %v", got.GetDenyReason())
	}
	if got.GetBanUntil() != 0 || got.GetKickedConnections() != 0 || got.GetRevokedTickets() != 0 {
		t.Fatalf("无目标时结论字段应全零: %+v", got)
	}
	// 仍然落审计（一次权力行使的尝试），状态是 DROPPED 而不是 SENT。
	row := onlyAudit(t, e)
	if row.State != model.BroadcastLogDropped || row.DropReason != model.DropBadTicket {
		t.Fatalf("无目标的处置应记 DROPPED/BAD_TICKET，实际 %d/%d", row.State, row.DropReason)
	}
}

// 存储态确实是 EXPIRED 的租约没有 →KICKED 的状态机边：实现不放宽，只摘出订阅集合，复活由 ban 窗口挡。
func TestKickConnectionExpiredLeaseHasNoKickedEdge(t *testing.T) {
	e := newTestEnv(t)
	rec := e.seedActiveLease("lgwl_kick_expired", "conn-lgwl_kick_expired", nodeA, roomID, mid, model.RoleViewer)
	// 把「已过期」写成既成事实：state 字段本身是 EXPIRED（Redis 里 TTL 已回收前的记录形态）。
	rec.State = model.LeaseStateExpired
	rec.ExpireAt = e.Clock.unix() - 1
	got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(kickReq("lgwl_kick_expired", "req-kick-expired"))
	if err != nil || !got.GetKicked() {
		t.Fatalf("处置应生效（连接已摘出房间集合），实际 err=%v reply=%+v", err, got)
	}
	after := hbGet(t, e, "lgwl_kick_expired")
	if after.State == model.LeaseStateKicked {
		t.Fatalf("实现放宽了 EXPIRED→KICKED 的状态机边（README 状态机表里没有这条）")
	}
	if after.State != model.LeaseStateExpired {
		t.Fatalf("EXPIRED 记录应保持原状态，实际 %d", after.State)
	}
	if got.GetKickedConnections() != 1 {
		t.Fatalf("摘出订阅集合也算一次处置，kicked_connections 应为 1，实际 %+v", got)
	}
	if n := e.Leases.callCount("Unsubscribe"); n != 1 {
		t.Fatalf("应恰好摘出一次订阅，Unsubscribe %d 次", n)
	}
	if n := e.Leases.callCount("Put"); n != 0 {
		t.Fatalf("没有合法状态机边时不该回写租约，Put %d 次", n)
	}
}

// 钉住当前行为：状态机闸门看的是存储态 state 字段，不是 EffectiveState(now)。
//
// 一条 state 仍为 ACTIVE、但 expire_at 已经过去的租约（Redis 惰性回收前的真实形态），
// 在本服务其它路径（连接列表、扇出目标、续租）都被 model.EffectiveState 判成 EXPIRED，
// 唯独 KickConnection 用 rec.State 查 leaseTransitions，于是写成 ACTIVE→KICKED 这条边。
// kickconnectionlogic.go:228 的注释声称「没有 EXPIRED→KICKED 的边，本方法不放宽状态机」，
// 与该分支实际判定的口径不一致：同一个「已过期」事实在两条路径上得到不同状态。
// 这里按可复现的真实行为断言，并在交付报告里登记为缺口（不改生产代码）。
func TestKickConnectionPastExpireAtButActiveStateFieldStillTurnsKicked(t *testing.T) {
	e := newTestEnv(t)
	rec := kickLease(e, "lgwl_kick_lingering", mid, model.RoleViewer)
	rec.ExpireAt = e.Clock.unix() - 1 // 只有 expire_at 过期，state 字段仍是 ACTIVE
	if rec.EffectiveState(e.Clock.unix()) != model.LeaseStateExpired {
		t.Fatalf("前提不成立：这条记录按 EffectiveState 应是 EXPIRED")
	}
	got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(kickReq("lgwl_kick_lingering", "req-kick-lingering"))
	if err != nil {
		t.Fatalf("处置不应失败: %v", err)
	}
	after := hbGet(t, e, "lgwl_kick_lingering")
	if after.State != model.LeaseStateKicked {
		t.Fatalf("当前实现按存储态 state 字段查状态机，ACTIVE→KICKED 成立；结论 %+v after=%+v", got, after)
	}
	// 与上一条用例成对：同为「逻辑上已过期」，只有 state 字段写着 EXPIRED 的那条才不置 KICKED。
	if got.GetKickedConnections() != 1 {
		t.Fatalf("kicked_connections 应为 1，实际 %+v", got)
	}
}

// --- 物理连接关闭：未接线只降级结论，不撤销已完成的失效写入 ---

func TestKickConnectionTransportFailureDowngradesConclusionOnly(t *testing.T) {
	t.Run("接入层未接线", func(t *testing.T) {
		e := newTestEnv(t)
		kickLease(e, "lgwl_kick_unwired", mid, model.RoleViewer)
		e.markTransportUnwired()
		req := kickReq("lgwl_kick_unwired", "req-kick-unwired")
		req.BanSeconds = 60
		got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(req)
		if err != nil {
			t.Fatalf("关不掉 socket 不是依赖故障结论，实际 %v", err)
		}
		if !got.GetKicked() || got.GetDenyReason() != dropReason(model.DropTransportUnavailable) {
			t.Fatalf("应回 kicked=true + TRANSPORT_UNAVAILABLE，实际 %+v", got)
		}
		// 关键：a/b/c 三项失效写入必须已经完成，不能被「关不掉」回滚成没发生。
		if after := hbGet(t, e, "lgwl_kick_unwired"); after.State != model.LeaseStateKicked {
			t.Fatalf("凭据未失效: %+v", after)
		}
		if got.GetBanUntil() == 0 {
			t.Fatalf("ban 窗口未写入: %+v", got)
		}
		row := onlyAudit(t, e)
		if row.State != model.BroadcastLogDropped || row.DropReason != model.DropTransportUnavailable {
			t.Fatalf("审计应记 DROPPED/TRANSPORT_UNAVAILABLE，实际 %d/%d", row.State, row.DropReason)
		}
	})

	t.Run("扇出通道报错同样只降级结论", func(t *testing.T) {
		e := newTestEnv(t)
		kickLease(e, "lgwl_kick_closeerr", mid, model.RoleViewer)
		e.Tp.closeErr = errCloseProbe
		got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(kickReq("lgwl_kick_closeerr", "req-kick-closeerr"))
		if err != nil || !got.GetKicked() {
			t.Fatalf("CloseConnections 报错不得升级成整体失败，实际 err=%v reply=%+v", err, got)
		}
		if got.GetDenyReason() != dropReason(model.DropTransportUnavailable) {
			t.Fatalf("结论未降级: %+v", got)
		}
		if e.Tp.closed != 1 || e.Tp.closeNode != nodeA {
			t.Fatalf("前提不成立：应按租约承载节点关一次连接，实际 closed=%d node=%s", e.Tp.closed, e.Tp.closeNode)
		}
	})

	// 判别性对照：接线正常时结论必须是 OK + SENT（降级不是默认值）。
	e := newTestEnv(t)
	kickLease(e, "lgwl_kick_ok", mid, model.RoleViewer)
	got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(kickReq("lgwl_kick_ok", "req-kick-ok"))
	if err != nil || got.GetDenyReason() != dropReason(model.DropOK) {
		t.Fatalf("接线正常时应回 OK，实际 err=%v reply=%+v", err, got)
	}
	if onlyAudit(t, e).State != model.BroadcastLogSent {
		t.Fatalf("全部生效且连接已关闭时审计应为 SENT")
	}
}

var errCloseProbe = errors.New("close-connections-probe")

// Redis 写失败必须原样返回：处置半途而废不能被说成完成。
func TestKickConnectionStorageFailuresPropagate(t *testing.T) {
	t.Run("订阅集合摘除失败", func(t *testing.T) {
		e := newTestEnv(t)
		rec := kickLease(e, "lgwl_kick_unsub", mid, model.RoleViewer)
		e.Leases.failWith("Unsubscribe", errUnsubscribeProbe)
		if _, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(kickReq("lgwl_kick_unsub", "req-kick-unsub")); !errors.Is(err, errUnsubscribeProbe) {
			t.Fatalf("实际 %v", err)
		}
		if rec.State != model.LeaseStateActive {
			t.Fatalf("摘除失败却推进了状态机")
		}
		if e.Tp.closed != 0 {
			t.Fatalf("租约还没失效就关了物理连接")
		}
	})

	t.Run("KICKED 写入失败", func(t *testing.T) {
		e := newTestEnv(t)
		kickLease(e, "lgwl_kick_put", mid, model.RoleViewer)
		e.Leases.failWith("Put", errPutProbe)
		got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(kickReq("lgwl_kick_put", "req-kick-put"))
		if !errors.Is(err, errPutProbe) || got != nil {
			t.Fatalf("实际 err=%v reply=%+v", err, got)
		}
		// 终态没落库就不能有关闭动作，否则「已处置」的说法与 Redis 事实分叉。
		if e.Tp.closed != 0 {
			t.Fatalf("Put 失败仍关了物理连接")
		}
	})

	t.Run("ban 窗口写入失败", func(t *testing.T) {
		e := newTestEnv(t)
		rec := kickLease(e, "lgwl_kick_setban", mid, model.RoleViewer)
		e.Leases.failWith("SetBan", errSetBanProbe)
		req := kickReq("lgwl_kick_setban", "req-kick-setban")
		req.BanSeconds = 60
		if _, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(req); !errors.Is(err, errSetBanProbe) {
			t.Fatalf("实际 %v", err)
		}
		if rec.State != model.LeaseStateActive {
			t.Fatalf("ban 窗口写失败后不该继续推进租约状态机")
		}
	})
}

var errSetBanProbe = errors.New("set-ban-probe")

// --- 审计形状 ---

// 审计的 message_id 用「原因短标 + request_id 摘要」，重放撞唯一键；不同 request_id 各留一行。
func TestKickConnectionAuditShape(t *testing.T) {
	e := newTestEnv(t)
	kickLease(e, "lgwl_kick_audit", mid, model.RoleViewer)
	req := kickReq("lgwl_kick_audit", "req-kick-audit-1")
	req.Reason = "room closed by anchor"
	req.TraceId = "trace-kick-1"
	callerMid := int64(1001)
	if _, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(req); err != nil {
		t.Fatalf("处置失败: %v", err)
	}
	row := onlyAudit(t, e)
	if row.Kind != model.KindModeration {
		t.Fatalf("处置审计的 kind 应为 MODERATION，实际 %d", row.Kind)
	}
	// 短标独立写死（不复用 lgwReasonSlug 当预期，否则断言退化成同义反复）：
	// "room closed by anchor" → 非字母数字换下划线，第 16 字节截断 = room_closed_by_a。
	if !strings.HasPrefix(row.MessageId, "kick:room_closed_by_a:") {
		t.Fatalf("message_id 应为 kick:<原因短标>:<request_id 摘要>，实际 %q", row.MessageId)
	}
	if len(lgwReasonSlug(req.Reason)) != 16 {
		t.Fatalf("原因短标应被截到 16 字节上限，实际 %q（%d 字节）", lgwReasonSlug(req.Reason), len(lgwReasonSlug(req.Reason)))
	}
	if len(row.MessageId) > 64 {
		t.Fatalf("message_id 是可索引列，必须收敛长度，实际 %d 字节", len(row.MessageId))
	}
	// 表里没有明文正文列：reason 原文只以摘要 + 字节数入库，message_id 只留短标。
	if want := payloadDigest([]byte(req.Reason), e.Svc.Config.LiveGateway.PayloadDigestBytes); row.PayloadDigest != want {
		t.Fatalf("reason 应落 payload_digest（期望 %q 实际 %q）", want, row.PayloadDigest)
	}
	if row.PayloadBytes != int32(len(req.Reason)) {
		t.Fatalf("payload_bytes 应等于 reason 长度，实际 %d", row.PayloadBytes)
	}
	if strings.Contains(fmt.Sprintf("%+v", row), req.Reason) {
		t.Fatalf("审计行不得保存 reason 明文: %+v", row)
	}
	if row.SenderMid != callerMid || row.SourceService != "operator:1001" {
		t.Fatalf("审计主体必须是归因结果（不是请求里的 operator 字段）: %+v", row)
	}
	if row.TraceId != "trace-kick-1" {
		t.Fatalf("trace_id 未透传: %+v", row)
	}
	// 换 request_id 就是第二次独立的权力行使，必须另留一行。
	kickLease(e, "lgwl_kick_audit2", mid, model.RoleViewer)
	req2 := kickReq("lgwl_kick_audit2", "req-kick-audit-2")
	req2.Reason = "room closed by anchor"
	if _, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(req2); err != nil {
		t.Fatalf("第二次处置失败: %v", err)
	}
	if len(e.Logs.all()) != 2 {
		t.Fatalf("两次独立处置应各留一行: %+v", e.Logs.all())
	}
}

// Kick 只处置连接与凭据：既不动房间路由，也不写房间/配额表。
func TestKickConnectionTouchesNoRouteOrQuotaRows(t *testing.T) {
	e := newTestEnv(t)
	kickLease(e, "lgwl_kick_isolate", mid, model.RoleViewer)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	req := kickReq("lgwl_kick_isolate", "req-kick-isolate")
	req.RevokeTickets = true
	req.BanSeconds = 30
	if got, err := NewKickConnectionLogic(ctxOperator(t, "1001"), e.Svc).KickConnection(req); err != nil || !got.GetKicked() {
		t.Fatalf("处置失败: err=%v reply=%+v", err, got)
	}
	if e.Routes.registers != 0 {
		t.Fatalf("Kick 不该登记/改写房间路由，Register %d 次", e.Routes.registers)
	}
	if e.Routes.rows[roomID].State != model.RouteStateServing {
		t.Fatalf("Kick 推进了路由状态: %+v", e.Routes.rows[roomID])
	}
	if n := e.Quotas.creates + e.Quotas.updates; n != 0 {
		t.Fatalf("Kick 不该写配额表，实际 %d 次", n)
	}
	// 订阅集合已摘空：房间连接数随之下降（计数走集合大小，不是 DECR）。
	if n := roomCount(t, e); n != 0 {
		t.Fatalf("被踢连接仍留在房间集合里，读数 %d", n)
	}
}
