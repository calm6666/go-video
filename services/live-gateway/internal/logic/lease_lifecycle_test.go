package logic

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go-video/services/live-gateway/internal/config"
	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"
)

// 本文件覆盖连接租约族四个方法（Acquire / Renew / Release / Get）的完整判定链。
// 这一族是本服务的「钱与在线态」所在：角色裁决、配额、封禁窗口、幂等回放与凭据脱敏都在这里，
// 所以断言全部围绕「结论必须是真实原因」而不是「不报错就算过」。

// --- 共用断言 ---

// assertTicketShape 校验票据串形状：opaque ticket_id + "." + 64 位十六进制签名。
// 票据串里不得出现 room_id / mid 明文（proto 头部约定），所以只能按形状与摘要反查。
func assertTicketShape(t *testing.T, ticket string) string {
	t.Helper()
	idx := strings.LastIndex(ticket, ".")
	if idx <= 0 || idx == len(ticket)-1 {
		t.Fatalf("票据缺少签名分隔符: %q", ticket)
	}
	ticketID, sig := ticket[:idx], ticket[idx+1:]
	if !strings.HasPrefix(ticketID, "lgwt_") {
		t.Fatalf("ticket_id 前缀应为 lgwt_，实际 %q", ticketID)
	}
	if len(sig) != 64 {
		t.Fatalf("签名长度应为 64，实际 %d", len(sig))
	}
	for i := 0; i < len(sig); i++ {
		c := sig[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("签名不是小写十六进制: %q", sig)
		}
	}
	return ticketID
}

// acquireReq 一条最小可用的 Acquire 请求。
func acquireReq(connID, requestID string) *rpc.AcquireConnectionLeaseReq {
	return &rpc.AcquireConnectionLeaseReq{
		RoomId: roomID, Mid: mid, ConnId: connID, NodeId: nodeA, RequestId: requestID,
	}
}

// --- Acquire：参数门禁 ---

func TestAcquireConnectionLeaseParameterGate(t *testing.T) {
	long := strings.Repeat("c", 65)
	cases := []struct {
		name string
		req  *rpc.AcquireConnectionLeaseReq
		want error
	}{
		{"空请求", nil, model.ErrInvalidRoomID},
		{"房间号非正", &rpc.AcquireConnectionLeaseReq{RoomId: 0, Mid: mid, ConnId: "c1", NodeId: nodeA, RequestId: "r1"}, model.ErrInvalidRoomID},
		{"房间号为负", &rpc.AcquireConnectionLeaseReq{RoomId: -7, Mid: mid, ConnId: "c1", NodeId: nodeA, RequestId: "r1"}, model.ErrInvalidRoomID},
		{"mid 为负", &rpc.AcquireConnectionLeaseReq{RoomId: roomID, Mid: -1, ConnId: "c1", NodeId: nodeA, RequestId: "r1"}, model.ErrInvalidMid},
		{"conn_id 为空", &rpc.AcquireConnectionLeaseReq{RoomId: roomID, Mid: mid, ConnId: "  ", NodeId: nodeA, RequestId: "r1"}, model.ErrEmptyConnID},
		{"conn_id 超长", &rpc.AcquireConnectionLeaseReq{RoomId: roomID, Mid: mid, ConnId: long, NodeId: nodeA, RequestId: "r1"}, model.ErrEmptyConnID},
		{"conn_id 含空格", &rpc.AcquireConnectionLeaseReq{RoomId: roomID, Mid: mid, ConnId: "c 1", NodeId: nodeA, RequestId: "r1"}, model.ErrEmptyConnID},
		{"node_id 为空", &rpc.AcquireConnectionLeaseReq{RoomId: roomID, Mid: mid, ConnId: "c1", NodeId: "", RequestId: "r1"}, model.ErrEmptyNodeID},
		{"request_id 为空", &rpc.AcquireConnectionLeaseReq{RoomId: roomID, Mid: mid, ConnId: "c1", NodeId: nodeA, RequestId: ""}, model.ErrEmptyRequestID},
		{"request_id 超长", &rpc.AcquireConnectionLeaseReq{RoomId: roomID, Mid: mid, ConnId: "c1", NodeId: nodeA, RequestId: long}, model.ErrEmptyRequestID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			got, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e.Svc).AcquireConnectionLease(tc.req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("期望错误 %v，实际 %v", tc.want, err)
			}
			if got != nil {
				t.Fatalf("参数被拒时不得返回响应体，实际 %+v", got)
			}
			// 参数阶段绝不能触碰存储：否则非法请求也能消耗幂等键或写出租约。
			if n := e.Leases.callCount("Acquire"); n != 0 {
				t.Fatalf("参数非法却写入了租约存储，Acquire 调用 %d 次", n)
			}
		})
	}
}

func TestAcquireConnectionLeaseClientInfoRedaction(t *testing.T) {
	cases := []struct {
		name   string
		hash   string
		wantOK bool
	}{
		{"不带设备摘要合法", "", true},
		{"32 位摘要", strings.Repeat("a", 32), true},
		{"64 位摘要", strings.Repeat("F", 64), true},
		{"非十六进制（设备号明文）", "device-plain-value-0123456789abcdef", false},
		{"长度不足 32", strings.Repeat("a", 31), false},
		{"超过 64 字节", strings.Repeat("a", 65), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			req := acquireReq("c-hash", "r-hash")
			req.Client = &rpc.ClientInfo{Platform: rpc.Platform_PLATFORM_IOS, AppVersion: "1.2.3", DeviceIdHash: tc.hash}
			reply, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e.Svc).AcquireConnectionLease(req)
			if !tc.wantOK {
				if !errors.Is(err, model.ErrInvalidClientInfo) {
					t.Fatalf("明文设备号必须被拒（ErrInvalidClientInfo），实际 err=%v reply=%+v", err, reply)
				}
				return
			}
			if err != nil {
				t.Fatalf("合法客户端标识不应失败: %v", err)
			}
			if !reply.GetAllowed() {
				t.Fatalf("合法客户端标识被拒: %s", reply.GetDenyDetail())
			}
			stored, gerr := e.Leases.Get(ctxClient(t), reply.GetLease().GetLeaseId())
			if gerr != nil {
				t.Fatalf("回读租约失败: %v", gerr)
			}
			if stored.AppVersion != "1.2.3" || int32(stored.Platform) != model.PlatformIOS {
				t.Fatalf("客户端版本/平台未落进租约: %+v", stored)
			}
			if want := strings.ToLower(tc.hash); stored.DeviceIDHash != want {
				t.Fatalf("device_id_hash 应为小写摘要 %q，实际 %q", want, stored.DeviceIDHash)
			}
		})
	}
}

// --- Acquire：角色裁决（本服务是唯一可信裁决点） ---

func TestAcquireConnectionLeaseRoleAdjudication(t *testing.T) {
	cases := []struct {
		name       string
		claimed    rpc.ConnRole
		mid        int64
		unwired    bool  // live-room 未接线
		gateErr    error // live-room 查询故障
		operatorMD bool  // 带归因 metadata
		wantRole   rpc.ConnRole
		wantDetail string // 空表示不应有降级说明
		wantErr    string // 非空表示应返回错误
	}{
		{name: "未声明角色按观众", claimed: rpc.ConnRole_CONN_ROLE_UNSPECIFIED, mid: mid, wantRole: rpc.ConnRole_CONN_ROLE_VIEWER},
		{name: "观众自报运营被拒", claimed: rpc.ConnRole_CONN_ROLE_OPERATOR, mid: mid, wantErr: "permission denied"},
		{name: "观众自报内部服务被拒", claimed: rpc.ConnRole_CONN_ROLE_SERVICE, mid: mid, wantErr: "permission denied"},
		{name: "主播且确为房主", claimed: rpc.ConnRole_CONN_ROLE_ANCHOR, mid: anchorMid, wantRole: rpc.ConnRole_CONN_ROLE_ANCHOR},
		{name: "主播但不是房主", claimed: rpc.ConnRole_CONN_ROLE_ANCHOR, mid: otherMid, wantRole: rpc.ConnRole_CONN_ROLE_VIEWER,
			wantDetail: "not the owner"},
		{name: "游客声明主播", claimed: rpc.ConnRole_CONN_ROLE_ANCHOR, mid: 0, wantRole: rpc.ConnRole_CONN_ROLE_VIEWER,
			wantDetail: "guest connection cannot hold the anchor role"},
		{name: "live-room 未接线则降级", claimed: rpc.ConnRole_CONN_ROLE_ANCHOR, mid: anchorMid, unwired: true,
			wantRole: rpc.ConnRole_CONN_ROLE_VIEWER, wantDetail: "connected as VIEWER"},
		{name: "房管无法校验故降级", claimed: rpc.ConnRole_CONN_ROLE_ROOM_ADMIN, mid: mid,
			wantRole: rpc.ConnRole_CONN_ROLE_VIEWER, wantDetail: "room admin role unverifiable"},
		{name: "归因运营优先于自报", claimed: rpc.ConnRole_CONN_ROLE_VIEWER, mid: mid, operatorMD: true,
			wantRole: rpc.ConnRole_CONN_ROLE_OPERATOR},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			if tc.unwired {
				e.markRoomGateUnwired()
			}
			if tc.gateErr != nil {
				e.Rooms.ownerErr = tc.gateErr
			}
			callCtx := ctxClient(t)
			if tc.operatorMD {
				callCtx = ctxOperator(t, "")
			}
			req := acquireReq("c-role", "r-role")
			req.Mid, req.ClaimedRole = tc.mid, tc.claimed
			reply, err := NewAcquireConnectionLeaseLogic(callCtx, e.Svc).AcquireConnectionLease(req)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("期望错误含 %q，实际 err=%v reply=%+v", tc.wantErr, err, reply)
				}
				if reply != nil {
					t.Fatalf("越权声明不得同时返回响应体: %+v", reply)
				}
				if n := e.Leases.callCount("Acquire"); n != 0 {
					t.Fatalf("越权声明不应签发租约，Acquire 调用 %d 次", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该失败: %v", err)
			}
			if !reply.GetAllowed() {
				t.Fatalf("被降级放行不是拒绝，reply=%+v", reply)
			}
			if reply.GetLease().GetRole() != tc.wantRole {
				t.Fatalf("角色判定应为 %v，实际 %v", tc.wantRole, reply.GetLease().GetRole())
			}
			if tc.wantDetail == "" {
				if reply.GetDenyDetail() != "" {
					t.Fatalf("不该有降级说明: %q", reply.GetDenyDetail())
				}
				return
			}
			if !strings.Contains(reply.GetDenyDetail(), tc.wantDetail) {
				t.Fatalf("deny_detail 应说明 %q，实际 %q", tc.wantDetail, reply.GetDenyDetail())
			}
		})
	}
}

// live-room 报的是「故障」而不是「未接线」时，必须原样失败，不能悄悄降成 VIEWER 放行。
func TestAcquireConnectionLeaseAnchorGateErrorPropagates(t *testing.T) {
	e := newTestEnv(t)
	e.Rooms.ownerErr = errors.New("live-room rpc deadline exceeded")
	req := acquireReq("c-err", "r-err")
	req.Mid, req.ClaimedRole = anchorMid, rpc.ConnRole_CONN_ROLE_ANCHOR
	reply, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e.Svc).AcquireConnectionLease(req)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("门禁故障必须原样返回，实际 err=%v reply=%+v", err, reply)
	}
	if reply != nil {
		t.Fatalf("门禁故障不得返回假结论: %+v", reply)
	}
}

func TestAcquireConnectionLeaseInternalCallerTakesAttestedRole(t *testing.T) {
	e := newTestEnv(t)
	req := acquireReq("c-svc", "r-svc")
	req.ClaimedRole = rpc.ConnRole_CONN_ROLE_VIEWER // 自报小角色也不该被降权
	reply, err := NewAcquireConnectionLeaseLogic(ctxService(t, "live-ingest"), e.Svc).AcquireConnectionLease(req)
	if err != nil {
		t.Fatalf("内部服务接入失败: %v", err)
	}
	if got := reply.GetLease().GetRole(); got != rpc.ConnRole_CONN_ROLE_SERVICE {
		t.Fatalf("归因主体应覆盖自报角色，实际 %v", got)
	}
}

// --- Acquire：租约本体与票据 ---

func TestAcquireConnectionLeaseHappyPath(t *testing.T) {
	e := newTestEnv(t)
	now := e.Clock.unix()
	reply, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e.Svc).AcquireConnectionLease(acquireReq("c-ok", "r-ok"))
	if err != nil {
		t.Fatalf("正常接入失败: %v", err)
	}
	if !reply.GetAllowed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_OK {
		t.Fatalf("应放行且原因为 OK: %+v", reply)
	}
	info := reply.GetLease()
	if info == nil || !strings.HasPrefix(info.GetLeaseId(), "lgwl_") {
		t.Fatalf("未回显租约: %+v", reply)
	}
	if info.GetState() != rpc.LeaseState_LEASE_STATE_ACTIVE {
		t.Fatalf("新租约状态应为 ACTIVE，实际 %v", info.GetState())
	}
	if info.GetExpireAt() != now+30 || info.GetTtlSeconds() != 30 {
		t.Fatalf("TTL 应为配置默认 30 秒，实际 expire=%d ttl=%d", info.GetExpireAt(), info.GetTtlSeconds())
	}
	if info.GetRoomId() != roomID || info.GetMid() != mid || info.GetNodeId() != nodeA {
		t.Fatalf("租约三元组与请求不一致: %+v", info)
	}
	if reply.GetQuotaRoomConns() != 1 {
		t.Fatalf("quota_room_conns 应为受理后的 1（含自己这条），实际 %d", reply.GetQuotaRoomConns())
	}
	ticketID := assertTicketShape(t, info.GetReconnectTicket())
	rows := e.Ticket.all()
	if len(rows) != 1 {
		t.Fatalf("应随租约签发 1 张票据，实际 %d 行", len(rows))
	}
	row := rows[0]
	if row.TicketId != ticketID {
		t.Fatalf("票据 ID 与票据串不符: %q vs %q", row.TicketId, ticketID)
	}
	if row.TicketHash != model.TicketHash(info.GetReconnectTicket()) {
		t.Fatalf("落库必须是票据摘要而不是原文")
	}
	if row.LeaseId != info.GetLeaseId() || row.RoomId != roomID || row.Mid != mid || row.IssueReason != ticketReasonNormal {
		t.Fatalf("票据登记值不符: %+v", row)
	}
	if row.State != model.TicketStateIssued || row.ExpireAt != now+120 {
		t.Fatalf("票据应为 ISSUED 且默认 120 秒有效: %+v", row)
	}
	if !e.Leases.hasTicketBit(row.TicketHash) {
		t.Fatalf("Redis 票据有效位缺失，该票换不了租约")
	}
	// 只把 ticket_id 写回租约：明文永不进 Redis。
	stored, gerr := e.Leases.Get(ctxClient(t), info.GetLeaseId())
	if gerr != nil {
		t.Fatalf("回读租约失败: %v", gerr)
	}
	if stored.TicketID != ticketID {
		t.Fatalf("ticket_id 未回填租约: %+v", stored)
	}
	if stored.ReconnectTicket != "" {
		t.Fatalf("票据明文不得写进 Redis: %+v", stored)
	}
	if e.Leases.callCount("Delete") != 0 {
		t.Fatalf("正常签发不该删任何键")
	}
}

func TestAcquireConnectionLeaseTTLClamp(t *testing.T) {
	e := newTestEnv(t)
	e.Quotas = newFakeQuotas(&model.LiveGwAccessQuota{
		Id: 1, Scope: model.QuotaScopeRoom, ScopeId: roomID, LeaseTtlSeconds: 45, TicketTtlSeconds: 90, Version: 1,
	})
	e.Svc.Store.Quotas = e.Quotas
	cases := []struct {
		name      string
		key       string
		requested int32
		want      int32
	}{
		{"请求值优先", "a", 60, 60},
		{"未请求则用配额", "b", 0, 45},
		{"小于下限夹到 10", "c", 1, 10},
		{"超上限夹到 MaxLeaseTTLSeconds", "d", 99999, 300},
		{"负数走配额", "e", -5, 45},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := acquireReq("c-ttl-"+tc.key, "r-ttl-"+tc.key)
			req.TtlSeconds = tc.requested
			reply, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e.Svc).AcquireConnectionLease(req)
			if err != nil {
				t.Fatalf("接入失败: %v", err)
			}
			if got := reply.GetLease().GetTtlSeconds(); got != tc.want {
				t.Fatalf("TTL 应为 %d，实际 %d", tc.want, got)
			}
			rows := e.Ticket.all()
			last := rows[len(rows)-1]
			wantExpire := e.Clock.unix() + 90 // 票据 TTL 同样来自配额（未请求时用配额）
			if last.ExpireAt != wantExpire {
				t.Fatalf("票据有效期应按配额 90 秒，实际 %+v", last)
			}
		})
	}
}

// 签名密钥未注入时：租约必须仍然有效（票据能力缺失不该把接入打死），但绝不发一张「不签名」的票。
func TestAcquireConnectionLeaseKeepsLeaseWhenSignerMissing(t *testing.T) {
	e := newTestEnv(t)
	e.markSignerMissing()
	reply, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e.Svc).AcquireConnectionLease(acquireReq("c-nosigner", "r-nosigner"))
	if err != nil {
		t.Fatalf("签名密钥缺失不该让接入失败: %v", err)
	}
	if !reply.GetAllowed() || reply.GetLease() == nil {
		t.Fatalf("租约应照常签发: %+v", reply)
	}
	if reply.GetLease().GetReconnectTicket() != "" {
		t.Fatalf("无密钥时不得回显票据")
	}
	if len(e.Ticket.all()) != 0 {
		t.Fatalf("无密钥时不得留下未签名的票据行")
	}
}

// DataSource 未配置时必须显式失败（RequireStore），而不是拿默认配额放行。
func TestAcquireConnectionLeaseFailsWithoutStore(t *testing.T) {
	e := newTestEnv(t)
	e.markStoreMissing()
	reply, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e.Svc).AcquireConnectionLease(acquireReq("c-nodb", "r-nodb"))
	if !errors.Is(err, repository.ErrStoreUnavailable) {
		t.Fatalf("期望 ErrStoreUnavailable，实际 %v", err)
	}
	if reply != nil {
		t.Fatalf("DB 不可用不得返回假成功: %+v", reply)
	}
	if n := e.Leases.callCount("Acquire"); n != 0 {
		t.Fatalf("配额读不到就不该签发租约，Acquire 调用 %d 次", n)
	}
}

// --- Acquire：配额 / 封禁 / 排空 ---

func TestAcquireConnectionLeaseDenials(t *testing.T) {
	t.Run("游客被配额禁止", func(t *testing.T) {
		e := newTestEnv(t)
		e.apply(func(c *config.LiveGatewayConf) { c.AllowGuestByDefault = false })
		req := acquireReq("c-guest", "r-guest")
		req.Mid = 0
		reply, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e.Svc).AcquireConnectionLease(req)
		if err != nil {
			t.Fatalf("业务拒绝不该是 error: %v", err)
		}
		if reply.GetAllowed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_PERMISSION_DENIED {
			t.Fatalf("游客应被配额拒绝: %+v", reply)
		}
		if !strings.Contains(reply.GetDenyDetail(), "allow_guest") {
			t.Fatalf("拒绝理由要指向 allow_guest: %q", reply.GetDenyDetail())
		}
		if n := e.Leases.callCount("Acquire"); n != 0 {
			t.Fatalf("被拒的接入不得写存储")
		}
	})

	t.Run("房间连接数达配额", func(t *testing.T) {
		e := newTestEnv(t)
		e.Quotas = newFakeQuotas(&model.LiveGwAccessQuota{
			Id: 1, Scope: model.QuotaScopeRoom, ScopeId: roomID, MaxConnections: 1, Version: 1,
		})
		e.Svc.Store.Quotas = e.Quotas
		e.seedActiveLease("lgwl_existing", "c-existing", nodeA, roomID, otherMid, model.RoleViewer)
		reply, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e.Svc).AcquireConnectionLease(acquireReq("c-quota", "r-quota"))
		if err != nil {
			t.Fatalf("配额拒绝不该是 error: %v", err)
		}
		if reply.GetAllowed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_RATE_LIMITED {
			t.Fatalf("应回 RATE_LIMITED: %+v", reply)
		}
		if reply.GetQuotaRoomConns() != 0 || !strings.Contains(reply.GetDenyDetail(), "(1/1)") {
			t.Fatalf("拒绝理由要回读实际配额读数 (1/1)，实际 %q conns=%d", reply.GetDenyDetail(), reply.GetQuotaRoomConns())
		}
		if n := e.Leases.callCount("Acquire"); n != 0 {
			t.Fatalf("超配额不得签发新租约")
		}
	})

	t.Run("禁止重连窗口内", func(t *testing.T) {
		e := newTestEnv(t)
		banUntil := e.Clock.unix() + 60
		if err := e.Leases.SetBan(ctxClient(t), roomID, mid, banUntil, true); err != nil {
			t.Fatalf("预置封禁失败: %v", err)
		}
		reply, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e.Svc).AcquireConnectionLease(acquireReq("c-ban", "r-ban"))
		if err != nil {
			t.Fatalf("封禁拒绝不该是 error: %v", err)
		}
		if reply.GetAllowed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_PERMISSION_DENIED {
			t.Fatalf("封禁期内应拒绝: %+v", reply)
		}
		if !strings.Contains(reply.GetDenyDetail(), "60s left") {
			t.Fatalf("要给出剩余封禁秒数便于客户端提示，实际 %q", reply.GetDenyDetail())
		}
	})

	t.Run("封禁窗口已过则放行", func(t *testing.T) {
		e := newTestEnv(t)
		if err := e.Leases.SetBan(ctxClient(t), roomID, mid, e.Clock.unix()-1, true); err != nil {
			t.Fatalf("预置封禁失败: %v", err)
		}
		reply, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e.Svc).AcquireConnectionLease(acquireReq("c-expiredban", "r-exp"))
		if err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		if !reply.GetAllowed() {
			t.Fatalf("封禁过期后应恢复接入: %+v", reply)
		}
	})

	t.Run("房间路由排空中不接新连接", func(t *testing.T) {
		e := newTestEnv(t)
		e.Routes.seed(&model.LiveGwRoomRoute{RoomId: roomID, PrimaryNode: nodeA, ReplicaNodes: "[]",
			ShardCount: 1, State: model.RouteStateDraining, Version: 3})
		reply, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e.Svc).AcquireConnectionLease(acquireReq("c-drain", "r-drain"))
		if err != nil {
			t.Fatalf("排空拒绝不该是 error: %v", err)
		}
		if reply.GetAllowed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_NO_ROUTE {
			t.Fatalf("DRAINING 房间应回 NO_ROUTE: %+v", reply)
		}
		if !strings.Contains(reply.GetDenyDetail(), nodeA) {
			t.Fatalf("要指明排空节点，实际 %q", reply.GetDenyDetail())
		}
		// 只读校验：不得改动路由，也不得登记新路由。
		if e.Routes.rows[roomID].Version != 3 || e.Routes.registers != 0 {
			t.Fatalf("Acquire 不得写路由表: %+v registers=%d", e.Routes.rows[roomID], e.Routes.registers)
		}
	})

	t.Run("路由依赖故障原样返回", func(t *testing.T) {
		e := newTestEnv(t)
		e.Routes.fail("FindOne", errors.New("db down"))
		reply, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e.Svc).AcquireConnectionLease(acquireReq("c-routefail", "r-rf"))
		if err == nil || !strings.Contains(err.Error(), "db down") {
			t.Fatalf("路由读失败必须原样返回，实际 err=%v reply=%+v", err, reply)
		}
	})

	t.Run("连接计数依赖故障不得当成零人", func(t *testing.T) {
		e := newTestEnv(t)
		e.Leases.failWith("RoomConnectionCount", errors.New("redis unavailable"))
		reply, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e.Svc).AcquireConnectionLease(acquireReq("c-countfail", "r-cf"))
		if err == nil || !strings.Contains(err.Error(), "redis unavailable") {
			t.Fatalf("计数失败要报错而不是放行，实际 err=%v reply=%+v", err, reply)
		}
		if reply != nil {
			t.Fatalf("依赖故障不得返回假结论: %+v", reply)
		}
	})
}

// --- Acquire：request_id 幂等回放 ---

func TestAcquireConnectionLeaseIdempotentReplay(t *testing.T) {
	e := newTestEnv(t)
	first, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e.Svc).AcquireConnectionLease(acquireReq("c-replay", "r-replay"))
	if err != nil {
		t.Fatalf("首次接入失败: %v", err)
	}
	acquiresBefore := e.Leases.callCount("Acquire")
	second, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e.Svc).AcquireConnectionLease(acquireReq("c-replay-2", "r-replay"))
	if err != nil {
		t.Fatalf("重放不该失败: %v", err)
	}
	if !second.GetAllowed() {
		t.Fatalf("重放应回放同一条租约: %+v", second)
	}
	if second.GetLease().GetLeaseId() != first.GetLease().GetLeaseId() {
		t.Fatalf("重放给出了另一条租约: %q vs %q", second.GetLease().GetLeaseId(), first.GetLease().GetLeaseId())
	}
	if second.GetLease().GetReconnectTicket() != "" {
		t.Fatalf("一次性票据不得在重放里第二次外溢")
	}
	if !strings.Contains(second.GetDenyDetail(), "idempotent replay") {
		t.Fatalf("重放必须自证是重放: %q", second.GetDenyDetail())
	}
	if e.Leases.callCount("Acquire") != acquiresBefore {
		t.Fatalf("重放不得再签一条租约")
	}
	if len(e.Ticket.all()) != 1 {
		t.Fatalf("重放不得再签票据，实际 %d 行", len(e.Ticket.all()))
	}

	t.Run("结论尚未回填时让调用方重试", func(t *testing.T) {
		e2 := newTestEnv(t)
		e2.Leases.seedRequest(acquireRPCName, "r-pending", lgwOutcomePending)
		reply, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e2.Svc).AcquireConnectionLease(acquireReq("c-pending", "r-pending"))
		if !errors.Is(err, model.ErrRequestIdDuplicated) {
			t.Fatalf("占位值不是结论，应回 ErrRequestIdDuplicated，实际 %v", err)
		}
		if reply != nil {
			t.Fatalf("未回填时不得伪造结论: %+v", reply)
		}
	})

	t.Run("首答租约已消失时不得冒充", func(t *testing.T) {
		e3 := newTestEnv(t)
		e3.Leases.seedRequest(acquireRPCName, "r-gone", "lgwl_vanished")
		reply, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e3.Svc).AcquireConnectionLease(acquireReq("c-gone", "r-gone"))
		if err != nil {
			t.Fatalf("这是真实结论，不该是 error: %v", err)
		}
		if reply.GetAllowed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_BAD_TICKET {
			t.Fatalf("首答租约已不在应回 BAD_TICKET 并让调用方换新 request_id: %+v", reply)
		}
		if e3.Leases.callCount("Acquire") != 0 {
			t.Fatalf("不能靠新签一条租约冒充同一次请求的结果")
		}
	})
}

// 只读门禁必须先于幂等占键：校验失败的请求不该把 request_id 烧掉。
func TestAcquireConnectionLeaseGatePrecedesIdempotencyClaim(t *testing.T) {
	e := newTestEnv(t)
	req := acquireReq("c-order", "r-order")
	req.Mid, req.ClaimedRole = otherMid, rpc.ConnRole_CONN_ROLE_OPERATOR // 越权声明
	if _, err := NewAcquireConnectionLeaseLogic(ctxClient(t), e.Svc).AcquireConnectionLease(req); err == nil {
		t.Fatalf("越权声明应失败")
	}
	e.Leases.mu.Lock()
	_, claimed := e.Leases.requests[acquireRPCName+"|r-order"]
	e.Leases.mu.Unlock()
	if claimed {
		t.Fatalf("鉴权失败前就占了幂等键，客户端换角色重试会撞到自己的占位值")
	}
}

// --- Renew ---

func TestRenewConnectionLeaseNotFoundAndParams(t *testing.T) {
	e := newTestEnv(t)
	l := NewRenewConnectionLeaseLogic(ctxClient(t), e.Svc)
	if _, err := l.RenewConnectionLease(nil); !errors.Is(err, model.ErrEmptyLeaseID) {
		t.Fatalf("空请求应回 ErrEmptyLeaseID，实际 %v", err)
	}
	if _, err := l.RenewConnectionLease(&rpc.RenewConnectionLeaseReq{LeaseId: " ", RoomId: roomID, Mid: mid}); !errors.Is(err, model.ErrEmptyLeaseID) {
		t.Fatalf("空白 lease_id 应回 ErrEmptyLeaseID，实际 %v", err)
	}
	if _, err := l.RenewConnectionLease(&rpc.RenewConnectionLeaseReq{LeaseId: "lgwl_x", RoomId: 0, Mid: mid}); !errors.Is(err, model.ErrInvalidRoomID) {
		t.Fatalf("room_id<=0 应回 ErrInvalidRoomID，实际 %v", err)
	}
	if _, err := l.RenewConnectionLease(&rpc.RenewConnectionLeaseReq{LeaseId: "lgwl_x", RoomId: roomID, Mid: -1}); !errors.Is(err, model.ErrInvalidMid) {
		t.Fatalf("mid<0 应回 ErrInvalidMid，实际 %v", err)
	}
	// 键已被回收：与「还在但过期」是两种结论，必须报错让客户端重新 Acquire。
	reply, err := l.RenewConnectionLease(&rpc.RenewConnectionLeaseReq{LeaseId: "lgwl_missing", RoomId: roomID, Mid: mid})
	if !errors.Is(err, model.ErrLeaseNotFound) {
		t.Fatalf("租约不存在应回 ErrLeaseNotFound，实际 %v", err)
	}
	if reply != nil {
		t.Fatalf("不得把「查不到」伪装成续租结果: %+v", reply)
	}
	if e.Leases.callCount("Put") != 0 {
		t.Fatalf("服务端绝不能靠续租静默签发新租约")
	}
}

func TestRenewConnectionLeaseTripletMismatch(t *testing.T) {
	cases := []struct {
		name string
		req  *rpc.RenewConnectionLeaseReq
	}{
		{"房间不符", &rpc.RenewConnectionLeaseReq{LeaseId: "lgwl_t1", RoomId: otherRoom, Mid: mid, ConnId: "c-t1"}},
		{"用户不符", &rpc.RenewConnectionLeaseReq{LeaseId: "lgwl_t1", RoomId: roomID, Mid: otherMid, ConnId: "c-t1"}},
		{"连接不符", &rpc.RenewConnectionLeaseReq{LeaseId: "lgwl_t1", RoomId: roomID, Mid: mid, ConnId: "c-other"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.seedActiveLease("lgwl_t1", "c-t1", nodeA, roomID, mid, model.RoleViewer)
			reply, err := NewRenewConnectionLeaseLogic(ctxClient(t), e.Svc).RenewConnectionLease(tc.req)
			if !errors.Is(err, model.ErrTripletMismatch) {
				t.Fatalf("三元组不匹配应回 ErrTripletMismatch，实际 %v", err)
			}
			if reply != nil {
				t.Fatalf("越权不返回结果体: %+v", reply)
			}
			if e.Leases.callCount("Put") != 0 {
				t.Fatalf("越权续租不得回写租约")
			}
			// 越权必须留痕：live_gw_broadcast_log 里多一行 DENIED。
			rows := e.Logs.all()
			if len(rows) != 1 {
				t.Fatalf("越权续租应落 1 行 DENIED 审计，实际 %d 行", len(rows))
			}
			if rows[0].State != model.BroadcastLogDenied || rows[0].DropReason != model.DropPermissionDenied ||
				rows[0].Kind != model.KindModeration {
				t.Fatalf("审计行结论不对: %+v", rows[0])
			}
		})
	}
}

func TestRenewConnectionLeaseTerminalStates(t *testing.T) {
	cases := []struct {
		name       string
		state      int32
		banOffset  int64 // 0 表示不写封禁
		wantReason rpc.DropReason
	}{
		{"被踢且在禁止重连窗口内", model.LeaseStateKicked, 120, rpc.DropReason_DROP_REASON_PERMISSION_DENIED},
		{"被踢但吊销期已过", model.LeaseStateKicked, 0, rpc.DropReason_DROP_REASON_BAD_TICKET},
		{"已释放", model.LeaseStateReleased, 120, rpc.DropReason_DROP_REASON_BAD_TICKET},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			if tc.banOffset > 0 {
				if err := e.Leases.SetBan(ctxClient(t), roomID, mid, e.Clock.unix()+tc.banOffset, false); err != nil {
					t.Fatalf("预置封禁失败: %v", err)
				}
			}
			rec := e.seedActiveLease("lgwl_term", "c-term", nodeA, roomID, mid, model.RoleViewer)
			rec.State = tc.state
			e.Leases.Put(ctxClient(t), rec)
			putsBefore := e.Leases.callCount("Put")
			reply, err := NewRenewConnectionLeaseLogic(ctxClient(t), e.Svc).RenewConnectionLease(
				&rpc.RenewConnectionLeaseReq{LeaseId: "lgwl_term", RoomId: roomID, Mid: mid, ConnId: "c-term"})
			if err != nil {
				t.Fatalf("终态拒绝不该是 error: %v", err)
			}
			if reply.GetRenewed() {
				t.Fatalf("终态租约不能复活: %+v", reply)
			}
			if reply.GetDenyReason() != tc.wantReason {
				t.Fatalf("拒绝原因应为 %v，实际 %v", tc.wantReason, reply.GetDenyReason())
			}
			if reply.GetState() != leaseState(tc.state) {
				t.Fatalf("要回显真实终态，实际 %v", reply.GetState())
			}
			if reply.GetTtlSeconds() != 0 {
				t.Fatalf("终态不得给出剩余 TTL: %+v", reply)
			}
			if e.Leases.callCount("Put") != putsBefore {
				t.Fatalf("终态不可回写")
			}
		})
	}
}

func TestRenewConnectionLeaseRevivesExpiredAndCountsOnce(t *testing.T) {
	e := newTestEnv(t)
	now := e.Clock.unix()
	e.seedActiveLease("lgwl_rn", "c-rn", nodeA, roomID, mid, model.RoleViewer)
	if _, err := e.Leases.Get(ctxClient(t), "lgwl_rn"); err != nil {
		t.Fatalf("store 异常: %v", err)
	}
	// 造一条已过期的租约（键还在，宽限期内可续）。
	e.Leases.Put(ctxClient(t), &repository.LeaseRecord{
		LeaseID: "lgwl_rn", ConnID: "c-rn", NodeID: nodeA, RoomID: roomID, Mid: mid,
		Role: model.RoleViewer, State: model.LeaseStateActive, IssuedAt: now - 60, ExpireAt: now - 5,
		LastHeartbeatAt: now - 60,
	})
	req := &rpc.RenewConnectionLeaseReq{LeaseId: "lgwl_rn", RoomId: roomID, Mid: mid, ConnId: "c-rn", TtlSeconds: 20}
	reply, err := NewRenewConnectionLeaseLogic(ctxClient(t), e.Svc).RenewConnectionLease(req)
	if err != nil {
		t.Fatalf("过期租约在宽限期内应可续租: %v", err)
	}
	if !reply.GetRenewed() || reply.GetState() != rpc.LeaseState_LEASE_STATE_ACTIVE {
		t.Fatalf("续租结论不对: %+v", reply)
	}
	if reply.GetExpireAt() != now+20 || reply.GetTtlSeconds() != 20 {
		t.Fatalf("expire_at 必须按 now+ttl 重算而不是旧值累加: %+v", reply)
	}
	stored, _ := e.Leases.Get(ctxClient(t), "lgwl_rn")
	if stored.RenewCount != 1 {
		t.Fatalf("首次续租 renew_count 应为 1，实际 %d", stored.RenewCount)
	}
	if stored.LastHeartbeatAt != now {
		t.Fatalf("续租等价一次心跳，应推进 last_heartbeat_at，实际 %d", stored.LastHeartbeatAt)
	}
	// 同一秒的重传只生效一次（去重键 lease_id|秒）。
	again, err := NewRenewConnectionLeaseLogic(ctxClient(t), e.Svc).RenewConnectionLease(req)
	if err != nil {
		t.Fatalf("同秒重传不该失败: %v", err)
	}
	if !again.GetRenewed() {
		t.Fatalf("同秒重传按既有结论返回成功: %+v", again)
	}
	stored2, _ := e.Leases.Get(ctxClient(t), "lgwl_rn")
	if stored2.RenewCount != 1 {
		t.Fatalf("同秒重传把 renew_count 累加成 %d，去重窗口失效", stored2.RenewCount)
	}
	// 跨过一秒之后才允许第二次真正续租。
	e.Clock.advance(1 * time.Second)
	if _, err := NewRenewConnectionLeaseLogic(ctxClient(t), e.Svc).RenewConnectionLease(req); err != nil {
		t.Fatalf("跨秒续租失败: %v", err)
	}
	stored3, _ := e.Leases.Get(ctxClient(t), "lgwl_rn")
	if stored3.RenewCount != 2 {
		t.Fatalf("跨秒后应真正续租一次，实际 renew_count=%d", stored3.RenewCount)
	}
	// 续租是纯 Redis 操作：零 MySQL 写入（README 数据分层）。
	if e.Logs.inserts != 0 || e.Ticket.inserts != 0 {
		t.Fatalf("续租不应写任何审计/票据表，logs=%d tickets=%d", e.Logs.inserts, e.Ticket.inserts)
	}
}

// 活租约也不能靠无限续租绕过 Kick 写的封禁：封禁只挡 Acquire 等于没封。
func TestRenewConnectionLeaseBlockedByBanWindow(t *testing.T) {
	e := newTestEnv(t)
	e.seedActiveLease("lgwl_ban", "c-ban", nodeA, roomID, mid, model.RoleViewer)
	if err := e.Leases.SetBan(ctxClient(t), roomID, mid, e.Clock.unix()+90, false); err != nil {
		t.Fatalf("预置封禁失败: %v", err)
	}
	reply, err := NewRenewConnectionLeaseLogic(ctxClient(t), e.Svc).RenewConnectionLease(
		&rpc.RenewConnectionLeaseReq{LeaseId: "lgwl_ban", RoomId: roomID, Mid: mid, ConnId: "c-ban"})
	if err != nil {
		t.Fatalf("封禁拒绝不该是 error: %v", err)
	}
	if reply.GetRenewed() || reply.GetDenyReason() != rpc.DropReason_DROP_REASON_PERMISSION_DENIED {
		t.Fatalf("封禁期内不得续租: %+v", reply)
	}
	if e.Leases.callCount("Put") != 0 {
		t.Fatalf("被拒的续租不得回写租约")
	}
	rows := e.Logs.all()
	if len(rows) != 1 || rows[0].State != model.BroadcastLogDenied {
		t.Fatalf("封禁拦截也要留痕，实际 %d 行", len(rows))
	}
}

func TestRenewConnectionLeaseStorageFailurePropagates(t *testing.T) {
	e := newTestEnv(t)
	e.seedActiveLease("lgwl_pfail", "c-pfail", nodeA, roomID, mid, model.RoleViewer)
	e.Leases.failWith("Put", errors.New("redis write failed"))
	reply, err := NewRenewConnectionLeaseLogic(ctxClient(t), e.Svc).RenewConnectionLease(
		&rpc.RenewConnectionLeaseReq{LeaseId: "lgwl_pfail", RoomId: roomID, Mid: mid, ConnId: "c-pfail"})
	if err == nil || !strings.Contains(err.Error(), "redis write failed") {
		t.Fatalf("回写失败必须原样返回，实际 err=%v reply=%+v", err, reply)
	}
	if reply != nil {
		t.Fatalf("写失败不得返回续租成功: %+v", reply)
	}
}

// --- Release ---

func TestReleaseConnectionLeaseParamsAndIdempotence(t *testing.T) {
	cases := []struct {
		name    string
		req     *rpc.ReleaseConnectionLeaseReq
		wantErr error
	}{
		{"空请求", nil, model.ErrEmptyLeaseID},
		{"既无 lease_id 也无 mid", &rpc.ReleaseConnectionLeaseReq{RoomId: roomID}, model.ErrEmptyLeaseID},
		{"按 mid 定位但缺房间号", &rpc.ReleaseConnectionLeaseReq{Mid: mid}, model.ErrInvalidRoomID},
		{"房间号为负", &rpc.ReleaseConnectionLeaseReq{LeaseId: "lgwl_x", RoomId: -1, Mid: mid}, model.ErrInvalidRoomID},
		{"mid 为负", &rpc.ReleaseConnectionLeaseReq{LeaseId: "lgwl_x", Mid: -1}, model.ErrInvalidMid},
		{"原因含换行：当前只校验长度不拦注入", &rpc.ReleaseConnectionLeaseReq{LeaseId: "lgwl_x", Reason: "a\nb\tc"}, nil},
		{"原因超长", &rpc.ReleaseConnectionLeaseReq{LeaseId: "lgwl_x", Reason: strings.Repeat("r", 257)}, model.ErrEmptyReason},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.seedActiveLease("lgwl_rel", "c-rel", nodeA, roomID, mid, model.RoleViewer)
			reply, err := NewReleaseConnectionLeaseLogic(ctxClient(t), e.Svc).ReleaseConnectionLease(tc.req)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("期望 %v，实际 %v", tc.wantErr, err)
				}
				if e.Leases.callCount("Delete") != 0 {
					t.Fatalf("参数被拒时不得删除任何租约")
				}
				return
			}
			if err != nil {
				t.Fatalf("合法 reason 形状不该失败: %v", err)
			}
			if reply == nil {
				t.Fatalf("应返回 EmptyReply")
			}
		})
	}
}

func TestReleaseConnectionLeaseScenarios(t *testing.T) {
	t.Run("租约已不存在按幂等成功", func(t *testing.T) {
		e := newTestEnv(t)
		reply, err := NewReleaseConnectionLeaseLogic(ctxClient(t), e.Svc).ReleaseConnectionLease(
			&rpc.ReleaseConnectionLeaseReq{LeaseId: "lgwl_gone", RoomId: roomID, Mid: mid})
		if err != nil || reply == nil {
			t.Fatalf("断线清理不该永远重试: err=%v reply=%+v", err, reply)
		}
		if e.Leases.callCount("Delete") != 0 {
			t.Fatalf("没有目标时不该调用删除")
		}
	})

	t.Run("三元组守卫拦住越权释放", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_own", "c-own", nodeA, roomID, mid, model.RoleViewer)
		_, err := NewReleaseConnectionLeaseLogic(ctxClient(t), e.Svc).ReleaseConnectionLease(
			&rpc.ReleaseConnectionLeaseReq{LeaseId: "lgwl_own", RoomId: otherRoom, Mid: mid})
		if !errors.Is(err, model.ErrTripletMismatch) {
			t.Fatalf("替别人断线必须拦住，实际 %v", err)
		}
		if e.Leases.callCount("Delete") != 0 {
			t.Fatalf("越权释放不得删除租约")
		}
		if rows := e.Logs.all(); len(rows) != 1 || rows[0].State != model.BroadcastLogDenied {
			t.Fatalf("越权释放要落 DENIED 审计，实际 %d 行", len(rows))
		}
	})

	t.Run("正常释放清理计数与断线视图", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_ok", "c-ok", nodeA, roomID, mid, model.RoleViewer)
		if _, err := NewReleaseConnectionLeaseLogic(ctxClient(t), e.Svc).ReleaseConnectionLease(
			&rpc.ReleaseConnectionLeaseReq{LeaseId: "lgwl_ok", RoomId: roomID, Mid: mid, ConnId: "c-ok", Reason: "normal"}); err != nil {
			t.Fatalf("释放失败: %v", err)
		}
		if rec, _ := e.Leases.Get(ctxClient(t), "lgwl_ok"); rec != nil {
			t.Fatalf("租约键未删除")
		}
		if n, err := e.Leases.RoomConnectionCount(ctxClient(t), roomID, 10); err != nil || n != 0 {
			t.Fatalf("房间连接计数应归零，实际 n=%d err=%v", n, err)
		}
		if at, _, err := e.Leases.OfflineView(ctxClient(t), roomID, mid); err != nil || at == 0 {
			t.Fatalf("要写断线视图供重连回显，实际 at=%d err=%v", at, err)
		}
		// 释放不碰路由表（MySQL 零写入）。
		if e.Routes.registers != 0 {
			t.Fatalf("释放连接不得登记/改写房间路由")
		}
	})

	t.Run("被踢记录保持终态不删键", func(t *testing.T) {
		e := newTestEnv(t)
		rec := e.seedActiveLease("lgwl_k", "c-k", nodeA, roomID, mid, model.RoleViewer)
		rec.State = model.LeaseStateKicked
		e.Leases.Put(ctxClient(t), rec)
		if _, err := NewReleaseConnectionLeaseLogic(ctxClient(t), e.Svc).ReleaseConnectionLease(
			&rpc.ReleaseConnectionLeaseReq{LeaseId: "lgwl_k", RoomId: roomID, Mid: mid}); err != nil {
			t.Fatalf("重复释放应幂等成功: %v", err)
		}
		after, _ := e.Leases.Get(ctxClient(t), "lgwl_k")
		if after == nil || after.State != model.LeaseStateKicked {
			t.Fatalf("被踢记录要留给排障与「被踢不能复活」判定，实际 %+v", after)
		}
		if e.Leases.callCount("Delete") != 0 {
			t.Fatalf("终态不得删键")
		}
	})

	t.Run("按 room+mid 批量收口多端连接", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_m1", "c-m1", nodeA, roomID, mid, model.RoleViewer)
		e.seedActiveLease("lgwl_m2", "c-m2", nodeB, roomID, mid, model.RoleViewer)
		e.seedActiveLease("lgwl_other", "c-m3", nodeA, roomID, otherMid, model.RoleViewer)
		if _, err := NewReleaseConnectionLeaseLogic(ctxClient(t), e.Svc).ReleaseConnectionLease(
			&rpc.ReleaseConnectionLeaseReq{RoomId: roomID, Mid: mid, Reason: "shutdown"}); err != nil {
			t.Fatalf("批量释放失败: %v", err)
		}
		for _, id := range []string{"lgwl_m1", "lgwl_m2"} {
			if rec, _ := e.Leases.Get(ctxClient(t), id); rec != nil {
				t.Fatalf("%s 未被释放", id)
			}
		}
		if rec, _ := e.Leases.Get(ctxClient(t), "lgwl_other"); rec == nil {
			t.Fatalf("别人的连接被这次释放带走了")
		}
	})

	t.Run("点名单条连接时不带走其余", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_p1", "c-p1", nodeA, roomID, mid, model.RoleViewer)
		e.seedActiveLease("lgwl_p2", "c-p2", nodeA, roomID, mid, model.RoleViewer)
		if _, err := NewReleaseConnectionLeaseLogic(ctxClient(t), e.Svc).ReleaseConnectionLease(
			&rpc.ReleaseConnectionLeaseReq{RoomId: roomID, Mid: mid, ConnId: "c-p1"}); err != nil {
			t.Fatalf("释放失败: %v", err)
		}
		if rec, _ := e.Leases.Get(ctxClient(t), "lgwl_p1"); rec != nil {
			t.Fatalf("点名的连接没被释放")
		}
		if rec, _ := e.Leases.Get(ctxClient(t), "lgwl_p2"); rec == nil {
			t.Fatalf("未点名的连接被顺手释放了")
		}
	})

	t.Run("Redis 删除失败原样返回", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_dfail", "c-d", nodeA, roomID, mid, model.RoleViewer)
		e.Leases.failWith("Delete", errors.New("redis down"))
		_, err := NewReleaseConnectionLeaseLogic(ctxClient(t), e.Svc).ReleaseConnectionLease(
			&rpc.ReleaseConnectionLeaseReq{LeaseId: "lgwl_dfail", RoomId: roomID, Mid: mid})
		if err == nil || !strings.Contains(err.Error(), "redis down") {
			t.Fatalf("删除失败要报错，否则客户端以为已断线而房间计数还留着，实际 %v", err)
		}
	})
}

// --- Get ---

func TestGetConnectionLease(t *testing.T) {
	t.Run("参数门禁", func(t *testing.T) {
		e := newTestEnv(t)
		l := NewGetConnectionLeaseLogic(ctxClient(t), e.Svc)
		if _, err := l.GetConnectionLease(nil); !errors.Is(err, model.ErrEmptyLeaseID) {
			t.Fatalf("空请求应回 ErrEmptyLeaseID，实际 %v", err)
		}
		if _, err := l.GetConnectionLease(&rpc.ConnectionLeaseReq{}); !errors.Is(err, model.ErrEmptyLeaseID) {
			t.Fatalf("两种定位都缺应回 ErrEmptyLeaseID，实际 %v", err)
		}
		if _, err := l.GetConnectionLease(&rpc.ConnectionLeaseReq{Mid: mid, RoomId: 0}); !errors.Is(err, model.ErrInvalidRoomID) {
			t.Fatalf("按 mid 定位缺房间号应回 ErrInvalidRoomID，实际 %v", err)
		}
	})

	t.Run("按 lease_id 命中且不回显票据", func(t *testing.T) {
		e := newTestEnv(t)
		e.Leases.seedLease(&repository.LeaseRecord{
			LeaseID: "lgwl_g1", ConnID: "c-g1", NodeID: nodeA, RoomID: roomID, Mid: mid, Role: model.RoleAnchor,
			State: model.LeaseStateActive, IssuedAt: e.Clock.unix() - 5, ExpireAt: e.Clock.unix() + 25,
			RenewCount: 2, LastHeartbeatAt: e.Clock.unix() - 1,
			ReconnectTicket: "lgwt_secret.0000000000000000000000000000000000000000000000000000000000000000",
		})
		reply, err := NewGetConnectionLeaseLogic(ctxClient(t), e.Svc).GetConnectionLease(&rpc.ConnectionLeaseReq{LeaseId: "lgwl_g1"})
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		if !reply.GetFound() {
			t.Fatalf("应命中: %+v", reply)
		}
		if reply.GetLease().GetReconnectTicket() != "" {
			t.Fatalf("只读口回显票据等于代他人重连，必须恒为空")
		}
		if reply.GetLease().GetState() != rpc.LeaseState_LEASE_STATE_ACTIVE || reply.GetLease().GetTtlSeconds() != 25 {
			t.Fatalf("投影状态/TTL 不对: %+v", reply.GetLease())
		}
		if reply.GetLease().GetRenewCount() != 2 {
			t.Fatalf("renew_count 未投影: %+v", reply.GetLease())
		}
		if reply.GetConnectionCount() != 1 {
			t.Fatalf("该 (room,mid) 有效连接数应为 1，实际 %d", reply.GetConnectionCount())
		}
		// 只读、无副作用。
		if e.Leases.callCount("Put") != 0 || e.Leases.callCount("Delete") != 0 || e.Leases.callCount("Acquire") != 0 {
			t.Fatalf("Get 必须零写入")
		}
	})

	t.Run("未命中回 found=false", func(t *testing.T) {
		e := newTestEnv(t)
		reply, err := NewGetConnectionLeaseLogic(ctxClient(t), e.Svc).GetConnectionLease(&rpc.ConnectionLeaseReq{LeaseId: "lgwl_none"})
		if err != nil || reply.GetFound() || reply.GetLease() != nil {
			t.Fatalf("未命中应回 found=false: err=%v reply=%+v", err, reply)
		}
	})

	t.Run("三元组不符不回显内容", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_g2", "c-g2", nodeA, roomID, mid, model.RoleViewer)
		l := NewGetConnectionLeaseLogic(ctxClient(t), e.Svc)
		if _, err := l.GetConnectionLease(&rpc.ConnectionLeaseReq{LeaseId: "lgwl_g2", RoomId: otherRoom}); !errors.Is(err, model.ErrTripletMismatch) {
			t.Fatalf("房间不符应回 ErrTripletMismatch，实际 %v", err)
		}
		first := e.Logs.all()
		if len(first) != 1 {
			t.Fatalf("首次越权探测应落 1 行审计，实际 %d 行", len(first))
		}
		if first[0].State != model.BroadcastLogDenied || first[0].Kind != model.KindModeration ||
			first[0].DropReason != model.DropPermissionDenied {
			t.Fatalf("越权审计应为 MODERATION/DENIED/PERMISSION_DENIED，实际 %+v", first[0])
		}
		// 同一 lease 的第二次探测（同房间维度）：也必须拒绝，且不得回显内容。
		if _, err := l.GetConnectionLease(&rpc.ConnectionLeaseReq{LeaseId: "lgwl_g2", Mid: otherMid}); !errors.Is(err, model.ErrTripletMismatch) {
			t.Fatalf("mid 不符应回 ErrTripletMismatch，实际 %v", err)
		}
		// 同一秒内同 (场景, 连接) 的探测塌缩成一行：message_id = "<scene>:<lease_id>:<Unix 秒>"，
		// 撞 UNIQUE(room_id,message_id) 后 Insert 返回 (0,nil)。这是「坏客户端心跳风暴不得变成
		// MySQL 写入风暴」的设计约束，不是漏写审计。
		if len(e.Logs.all()) != 1 || e.Logs.dups != 1 {
			t.Fatalf("同秒重复探测应塌缩为 1 行并计 1 次冲突，实际 %d 行 / %d 冲突", len(e.Logs.all()), e.Logs.dups)
		}
		// 换到下一秒再探测：必须新落一行（塌缩只在同秒内，跨秒的探测行为仍可追溯）。
		e.Clock.advance(time.Second)
		if _, err := l.GetConnectionLease(&rpc.ConnectionLeaseReq{LeaseId: "lgwl_g2", RoomId: otherRoom}); !errors.Is(err, model.ErrTripletMismatch) {
			t.Fatalf("跨秒探测仍应回 ErrTripletMismatch，实际 %v", err)
		}
		if got := len(e.Logs.all()); got != 2 {
			t.Fatalf("跨秒探测应另落 1 行，实际 %d 行", got)
		}
		if e.Leases.callCount("Put") != 0 || e.Leases.callCount("Delete") != 0 {
			t.Fatalf("越权探测路径必须零写入")
		}
	})

	t.Run("按 room+mid 只认有效连接", func(t *testing.T) {
		e := newTestEnv(t)
		now := e.Clock.unix()
		e.Leases.seedLease(&repository.LeaseRecord{LeaseID: "lgwl_exp", ConnID: "c-e", NodeID: nodeA, RoomID: roomID,
			Mid: mid, Role: model.RoleViewer, State: model.LeaseStateActive, IssuedAt: now - 90, ExpireAt: now - 30})
		e.Leases.seedLease(&repository.LeaseRecord{LeaseID: "lgwl_act", ConnID: "c-a", NodeID: nodeA, RoomID: roomID,
			Mid: mid, Role: model.RoleViewer, State: model.LeaseStateActive, IssuedAt: now - 5, ExpireAt: now + 25})
		reply, err := NewGetConnectionLeaseLogic(ctxClient(t), e.Svc).GetConnectionLease(
			&rpc.ConnectionLeaseReq{RoomId: roomID, Mid: mid})
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		if !reply.GetFound() || reply.GetLease().GetLeaseId() != "lgwl_act" {
			t.Fatalf("过期记录不算在线，应命中活的那条: %+v", reply)
		}
		if reply.GetConnectionCount() != 1 {
			t.Fatalf("有效连接数应为 1，实际 %d", reply.GetConnectionCount())
		}
	})

	t.Run("全员过期回 found=false", func(t *testing.T) {
		e := newTestEnv(t)
		now := e.Clock.unix()
		e.Leases.seedLease(&repository.LeaseRecord{LeaseID: "lgwl_only", ConnID: "c-o", NodeID: nodeA, RoomID: roomID,
			Mid: mid, Role: model.RoleViewer, State: model.LeaseStateActive, IssuedAt: now - 90, ExpireAt: now - 10})
		reply, err := NewGetConnectionLeaseLogic(ctxClient(t), e.Svc).GetConnectionLease(
			&rpc.ConnectionLeaseReq{RoomId: roomID, Mid: mid})
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		if reply.GetFound() {
			t.Fatalf("只剩过期记录不该报在线: %+v", reply)
		}
	})

	t.Run("Redis 故障不得当成查无此人", func(t *testing.T) {
		e := newTestEnv(t)
		e.Leases.failWith("Get", errors.New("redis unavailable"))
		reply, err := NewGetConnectionLeaseLogic(ctxClient(t), e.Svc).GetConnectionLease(&rpc.ConnectionLeaseReq{LeaseId: "lgwl_x"})
		if err == nil || !strings.Contains(err.Error(), "redis unavailable") {
			t.Fatalf("依赖故障必须报错，把故障翻译成 found=false 会被读成「确实没人」，实际 err=%v reply=%+v", err, reply)
		}
		e.Leases.failWith("Get", nil)
		e.Leases.failWith("ListUserLeases", errors.New("scan failed"))
		if _, err := NewGetConnectionLeaseLogic(ctxClient(t), e.Svc).GetConnectionLease(
			&rpc.ConnectionLeaseReq{RoomId: roomID, Mid: mid}); err == nil {
			t.Fatalf("按 (room,mid) 定位时扫描失败也要报错")
		}
	})

	t.Run("游客租约按命中这条计数", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedActiveLease("lgwl_guest", "c-g", nodeA, roomID, 0, model.RoleViewer)
		reply, err := NewGetConnectionLeaseLogic(ctxClient(t), e.Svc).GetConnectionLease(&rpc.ConnectionLeaseReq{LeaseId: "lgwl_guest"})
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		if reply.GetConnectionCount() != 1 {
			t.Fatalf("游客没有 (room,mid) 索引，命中这条即为 1，实际 %d", reply.GetConnectionCount())
		}
	})
}
