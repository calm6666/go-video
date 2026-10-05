package logic

import (
	"errors"
	"strings"
	"testing"

	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"
)

// 本文件覆盖 LeaveRoom（退订，SREM 语义）。
//
// LeaveRoom 与 JoinRoom 是同一个连接的两半，但它刻意不比 JoinRoom 更严也不更松，
// 三处差异是本文件的主要断言对象：
//  1. proto LeaveRoomReq 没有 request_id → 本方法不占幂等窗口，幂等由集合语义保证；
//     所以「非法请求烧掉 request_id」这类故障在 LeaveRoom 里不存在，反过来
//     「同一个 LeaseRoomReq 调两次必须两次都成功」才是它的幂等契约。
//  2. topics 的**空值方向相反**：JoinRoom 空 = 默认全集，LeaveRoom 空 = 退订全部，
//     因此实现里不能复用 NormalizeTopics（leaveroomlogic.go 的 lgwLeaveTopics）。
//  3. 状态机口径不同：LeaveRoom **不拒已过期租约**（宽限期内的断线最需要清理订阅），
//     但终态（RELEASED/KICKED）按已退出幂等返回。
//
// 另一条硬约束：LeaveRoom 对 MySQL 零写入、不删租约、不写断线视图、不动路由 ——
// 它是 ReleaseConnectionLease 的子集，越界就会伪造「该用户已断开」。

// leaveReq 一条与 joinLease 同源的退订请求（topics/reason 由用例补齐）。
func leaveReq(leaseID string) *rpc.LeaveRoomReq {
	return &rpc.LeaveRoomReq{
		LeaseId: leaseID, ConnId: "conn-" + leaseID, RoomId: roomID, Mid: mid,
	}
}

// leaveLease 播种一条已订阅 danmaku+interaction 的活租约（退订用例的公共起点）。
func leaveLease(e *testEnv, leaseID string, user int64, role int32) *repository.LeaseRecord {
	rec := e.seedActiveLease(leaseID, "conn-"+leaseID, nodeA, roomID, user, role)
	rec.Topics = []string{"danmaku", "interaction"}
	return rec
}

// --- 参数门禁 ---

func TestLeaveRoomParameterGate(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*rpc.LeaveRoomReq)
		want error
	}{
		{"lease_id 为空", func(r *rpc.LeaveRoomReq) { r.LeaseId = "  " }, model.ErrEmptyLeaseID},
		{"房间号非正", func(r *rpc.LeaveRoomReq) { r.RoomId = 0 }, model.ErrInvalidRoomID},
		{"mid 为负", func(r *rpc.LeaveRoomReq) { r.Mid = -1 }, model.ErrInvalidMid},
		{"conn_id 超长", func(r *rpc.LeaveRoomReq) { r.ConnId = strings.Repeat("c", 65) }, model.ErrEmptyConnID},
		{"conn_id 含空白", func(r *rpc.LeaveRoomReq) { r.ConnId = "c 1" }, model.ErrEmptyConnID},
		{"reason 超长", func(r *rpc.LeaveRoomReq) { r.Reason = strings.Repeat("r", 257) }, model.ErrEmptyReason},
		{"未知子通道", func(r *rpc.LeaveRoomReq) { r.Topics = []string{"dmakku"} }, repository.ErrUnknownTopic},
		{"通道列表里有空串", func(r *rpc.LeaveRoomReq) { r.Topics = []string{"danmaku", "   "} }, repository.ErrUnknownTopic},
		{"通道数量越界", func(r *rpc.LeaveRoomReq) {
			r.Topics = []string{"danmaku", "state", "interaction", "system", "moderation", "anchor_tip", "a", "b", "c"}
		}, repository.ErrTooManyTopics},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			rec := leaveLease(e, "lgwl_leave_param", mid, model.RoleViewer)
			req := leaveReq("lgwl_leave_param")
			tc.mut(req)
			got, err := NewLeaveRoomLogic(ctxClient(t), e.Svc).LeaveRoom(req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("期望错误 %v，实际 %v", tc.want, err)
			}
			if got != nil {
				t.Fatalf("参数被拒时不得返回响应体，实际 %+v", got)
			}
			// 参数阶段不得触碰任何写：订阅集合、租约、断线视图、审计都必须是零。
			assertLeaveUnsubscribes(t, e, 0)
			assertLeaveDoesNotTouchLeaseOrRoute(t, e)
			assertNoWrites(t, e)
			if len(rec.Topics) != 2 {
				t.Fatalf("参数被拒却改动了订阅: %+v", rec.Topics)
			}
		})
	}
	// in == nil 单独一条：上面的表表达不了「整个请求体缺失」。
	e := newTestEnv(t)
	if got, err := NewLeaveRoomLogic(ctxClient(t), e.Svc).LeaveRoom(nil); !errors.Is(err, model.ErrEmptyLeaseID) || got != nil {
		t.Fatalf("空请求体必须报 ErrEmptyLeaseID，实际 err=%v reply=%+v", err, got)
	}
}

// assertLeaveDoesNotTouchLeaseOrRoute 断言这条路径没越出「退订」这一步：
// 不改租约、不删租约、不写断线视图、不占幂等窗口、不写 MySQL 任何一张表、不动路由缓存。
// 单独拆出来而不复用 assertNoWrites，是因为「越权退订」这一条**本来就该**落一行 DENIED 审计。
func assertLeaveDoesNotTouchLeaseOrRoute(t *testing.T, e *testEnv) {
	t.Helper()
	if n := e.Leases.callCount("Put"); n != 0 {
		t.Fatalf("LeaveRoom 不该改写租约（那是 Release/Kick 的职责），实际 Put %d 次", n)
	}
	if n := e.Leases.callCount("Delete"); n != 0 {
		t.Fatalf("LeaveRoom 不该删租约，实际 Delete %d 次", n)
	}
	if n := e.Leases.callCount("MarkOffline"); n != 0 {
		t.Fatalf("LeaveRoom 不该写断线视图（退订时连接可能还开着），实际 MarkOffline %d 次", n)
	}
	if n := e.Leases.callCount("ClaimRequest"); n != 0 {
		t.Fatalf("proto 里 LeaveRoom 没有 request_id，不该占幂等窗口，实际 ClaimRequest %d 次", n)
	}
	if n := e.Leases.callCount("CacheDel"); n != 0 {
		t.Fatalf("路由没动就不该失效路由缓存，实际 CacheDel %d 次", n)
	}
	if n := e.Quotas.creates + e.Quotas.updates; n != 0 {
		t.Fatalf("不该向 live_gw_access_quota 写入，实际写 %d 次", n)
	}
	if n := e.Routes.registers; n != 0 {
		t.Fatalf("不该向 live_gw_room_route 写入，实际 Register %d 次", n)
	}
	if n := e.Ticket.inserts; n != 0 {
		t.Fatalf("退订不该写重连票据，实际 Insert %d 次", n)
	}
}

// assertLeaveUnsubscribes 断言 Unsubscribe 恰好被调用 n 次。
func assertLeaveUnsubscribes(t *testing.T, e *testEnv, n int) {
	t.Helper()
	if got := e.Leases.callCount("Unsubscribe"); got != n {
		t.Fatalf("Unsubscribe 应调用 %d 次，实际 %d 次", n, got)
	}
}

// roomCount 读房间连接数（Redis 集合口径，与 JoinRoom 的配额判定同一份事实源）。
func roomCount(t *testing.T, e *testEnv) int32 {
	t.Helper()
	n, err := e.Leases.RoomConnectionCount(ctxClient(t), roomID, 500)
	if err != nil {
		t.Fatalf("房间连接数读数失败: %v", err)
	}
	return n
}

// --- 三元组守卫 ---

func TestLeaveRoomTripletGuardRejectsAndAudits(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*rpc.LeaveRoomReq)
	}{
		{"替别人的房间退订", func(r *rpc.LeaveRoomReq) { r.RoomId = otherRoom }},
		{"替别人的账号退订", func(r *rpc.LeaveRoomReq) { r.Mid = otherMid }},
		{"conn_id 改绑", func(r *rpc.LeaveRoomReq) { r.ConnId = "conn-hijacked" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			rec := leaveLease(e, "lgwl_leave_guard", mid, model.RoleViewer)
			req := leaveReq("lgwl_leave_guard")
			tc.mut(req)
			got, err := NewLeaveRoomLogic(ctxClient(t), e.Svc).LeaveRoom(req)
			if !errors.Is(err, model.ErrTripletMismatch) {
				t.Fatalf("越权退订必须报三元组不符，实际 err=%v reply=%+v", err, got)
			}
			if got != nil {
				t.Fatalf("越权时不得返回响应体，实际 %+v", got)
			}
			// 越权退订等于替别人静音：订阅必须原样保留。
			if len(rec.Topics) != 2 {
				t.Fatalf("被拒的退订却收窄了别人的订阅: %v", rec.Topics)
			}
			assertLeaveUnsubscribes(t, e, 0)
			assertLeaveDoesNotTouchLeaseOrRoute(t, e)
			// 破坏性操作的越权必须留痕（AGENTS.md §8）。
			row := onlyAudit(t, e)
			if row.State != model.BroadcastLogDenied || row.DropReason != model.DropPermissionDenied {
				t.Fatalf("越权退订审计应为 DENIED/PERMISSION_DENIED，实际 %d/%d", row.State, row.DropReason)
			}
			if !strings.HasPrefix(row.MessageId, "leave-denied:") {
				t.Fatalf("审计定位串应标明场景，实际 %q", row.MessageId)
			}
		})
	}
	// 判别性对照：同一份数据只把 mid 改回租约自身，就必须退订成功。
	e := newTestEnv(t)
	leaveLease(e, "lgwl_leave_guard_ok", mid, model.RoleViewer)
	okReq := leaveReq("lgwl_leave_guard_ok")
	okReq.Mid = mid
	if got, err := NewLeaveRoomLogic(ctxClient(t), e.Svc).LeaveRoom(okReq); err != nil || got == nil {
		t.Fatalf("对照用例（三元组相符）应退订成功，实际 err=%v reply=%+v", err, got)
	}
}

// 守卫排在终态幂等之前：拿已释放的租约去退「别人的房间」不能变成幂等成功。
func TestLeaveRoomGuardRunsBeforeTerminalIdempotency(t *testing.T) {
	e := newTestEnv(t)
	rec := leaveLease(e, "lgwl_leave_order", mid, model.RoleViewer)
	rec.State = model.LeaseStateReleased
	req := leaveReq("lgwl_leave_order")
	req.RoomId = otherRoom
	if _, err := NewLeaveRoomLogic(ctxClient(t), e.Svc).LeaveRoom(req); !errors.Is(err, model.ErrTripletMismatch) {
		t.Fatalf("终态幂等不得掩盖越权，实际 %v", err)
	}
	if e.Leases.callCount("Unsubscribe") != 0 {
		t.Fatalf("越权路径不该走到退订")
	}
}

// --- 幂等：三种「已经没有了」的形态 ---

func TestLeaveRoomIsIdempotentWhenLeaseIsGoneOrTerminal(t *testing.T) {
	t.Run("租约键已被回收按已退出返回", func(t *testing.T) {
		e := newTestEnv(t)
		// 什么都不播种：Get 返回 (nil, nil)，实现必须说「已退出」而不是 ErrLeaseNotFound，
		// 否则客户端断线清理会永远重试。
		got, err := NewLeaveRoomLogic(ctxClient(t), e.Svc).LeaveRoom(leaveReq("lgwl_leave_missing"))
		if err != nil || got == nil {
			t.Fatalf("键已回收应幂等成功，实际 err=%v reply=%+v", err, got)
		}
		assertLeaveUnsubscribes(t, e, 0)
		assertLeaveDoesNotTouchLeaseOrRoute(t, e)
		assertNoWrites(t, e)
		if n := len(e.Logs.all()); n != 0 {
			t.Fatalf("幂等返回不该落审计行，实际 %d 行", n)
		}
	})

	for _, st := range []struct {
		name  string
		state int32
	}{
		{"已释放租约", model.LeaseStateReleased},
		{"已被踢租约", model.LeaseStateKicked},
	} {
		t.Run(st.name+"按已退出返回", func(t *testing.T) {
			e := newTestEnv(t)
			rec := leaveLease(e, "lgwl_leave_term", mid, model.RoleViewer)
			rec.State = st.state
			got, err := NewLeaveRoomLogic(ctxClient(t), e.Svc).LeaveRoom(leaveReq("lgwl_leave_term"))
			if err != nil || got == nil {
				t.Fatalf("终态重复退出应幂等成功，实际 err=%v reply=%+v", err, got)
			}
			// 终态时订阅随连接一起失效：连 Unsubscribe 都不该发生。
			assertLeaveUnsubscribes(t, e, 0)
			assertLeaveDoesNotTouchLeaseOrRoute(t, e)
			assertNoWrites(t, e)
			if len(e.Logs.all()) != 0 {
				t.Fatalf("幂等返回不该落审计: %+v", e.Logs.all())
			}
		})
	}
}

// LeaveRoom 刻意不拒已过期租约（与 usableLease 相反）：宽限期内的断线正是最需要
// 清理订阅的一侧。判别性对照用同一条过期租约跑 JoinRoom，那边必须给 BAD_TICKET。
func TestLeaveRoomAcceptsExpiredLeaseButJoinRoomRejectsIt(t *testing.T) {
	e := newTestEnv(t)
	rec := leaveLease(e, "lgwl_leave_expired", mid, model.RoleViewer)
	rec.ExpireAt = e.Clock.unix() - 1
	if rec.EffectiveState(e.Clock.unix()) != model.LeaseStateExpired {
		t.Fatalf("前提不成立：租约应为有效态 EXPIRED")
	}
	got, err := NewLeaveRoomLogic(ctxClient(t), e.Svc).LeaveRoom(leaveReq("lgwl_leave_expired"))
	if err != nil || got == nil {
		t.Fatalf("过期租约的退订必须被执行（否则房间集合永远留着死连接），实际 err=%v reply=%+v", err, got)
	}
	if n := e.Leases.callCount("Unsubscribe"); n != 1 {
		t.Fatalf("应恰好退订一次，实际 %d 次", n)
	}
	// 但它仍然不是「有效凭据」：同一时刻的 JoinRoom 必须按无效凭据拒出。
	e2 := newTestEnv(t)
	jrec := joinLease(e2, "lgwl_leave_expired", mid, model.RoleViewer)
	jrec.ExpireAt = e2.Clock.unix() - 1
	jgot, jerr := NewJoinRoomLogic(ctxClient(t), e2.Svc).JoinRoom(joinReq("lgwl_leave_expired", "req-leave-exp-join"))
	if jerr != nil {
		t.Fatalf("对照用例前提不成立: %v", jerr)
	}
	if jgot.GetJoined() || jgot.GetDenyReason() != dropReason(model.DropBadTicket) {
		t.Fatalf("JoinRoom 竟接受了过期租约: %+v", jgot)
	}
}

// --- topics 语义：空 = 全退，非空 = 只收窄 ---

func TestLeaveRoomTopicSemantics(t *testing.T) {
	t.Run("空 topics 退订全部并移出房间集合", func(t *testing.T) {
		e := newTestEnv(t)
		rec := leaveLease(e, "lgwl_leave_all", mid, model.RoleViewer)
		e.seedActiveLease("lgwl_leave_peer", "conn-peer", nodeA, roomID, otherMid, model.RoleViewer)
		before := roomCount(t, e)
		if before != 2 {
			t.Fatalf("前提不成立：房间内应有 2 条活连接，实际 %d", before)
		}
		got, lerr := NewLeaveRoomLogic(ctxClient(t), e.Svc).LeaveRoom(leaveReq("lgwl_leave_all"))
		if lerr != nil || got == nil {
			t.Fatalf("退订全部失败: %v", lerr)
		}
		if len(rec.Topics) != 0 {
			t.Fatalf("空 topics 必须清掉全部子通道，实际 %v", rec.Topics)
		}
		if after := roomCount(t, e); after != before-1 {
			t.Fatalf("全退后房间连接数应从 %d 降到 %d，实际 %d", before, before-1, after)
		}
	})

	t.Run("非空 topics 只收窄且连接仍在房间内", func(t *testing.T) {
		e := newTestEnv(t)
		rec := leaveLease(e, "lgwl_leave_narrow", mid, model.RoleViewer)
		before := roomCount(t, e)
		req := leaveReq("lgwl_leave_narrow")
		req.Topics = []string{"DANMAKU"} // 归一：大写也要命中
		if got, lerr := NewLeaveRoomLogic(ctxClient(t), e.Svc).LeaveRoom(req); lerr != nil || got == nil {
			t.Fatalf("收窄订阅失败: %v", lerr)
		}
		if strings.Join(rec.Topics, ",") != "interaction" {
			t.Fatalf("应只剩 interaction，实际 %v", rec.Topics)
		}
		if after := roomCount(t, e); after != before {
			t.Fatalf("只退一个子通道不该把连接移出房间（计数 %d → %d）", before, after)
		}
	})

	// 判别性对照：同一个「未知通道」在退订侧报错、在订阅侧也报错，但**空列表**两侧方向相反。
	// JoinRoom 的空列表会展开成默认全集，LeaveRoom 的空列表必须什么都不留。
	t.Run("空白通道名报错而不是静默忽略", func(t *testing.T) {
		e := newTestEnv(t)
		rec := leaveLease(e, "lgwl_leave_blank", mid, model.RoleViewer)
		req := leaveReq("lgwl_leave_blank")
		req.Topics = []string{"  "}
		if _, err := NewLeaveRoomLogic(ctxClient(t), e.Svc).LeaveRoom(req); !errors.Is(err, repository.ErrUnknownTopic) {
			t.Fatalf("只含空白名的列表应报错，实际 %v", err)
		}
		if strings.Join(rec.Topics, ",") != "danmaku,interaction" {
			t.Fatalf("报错路径改动了订阅: %v", rec.Topics)
		}
	})

	t.Run("重复与大小写归一后只退一次", func(t *testing.T) {
		e := newTestEnv(t)
		rec := leaveLease(e, "lgwl_leave_dup", mid, model.RoleViewer)
		rec.Topics = []string{"danmaku", "interaction", "state"}
		req := leaveReq("lgwl_leave_dup")
		req.Topics = []string{"Danmaku", "danmaku", " STATE "}
		if _, err := NewLeaveRoomLogic(ctxClient(t), e.Svc).LeaveRoom(req); err != nil {
			t.Fatalf("归一后的退订失败: %v", err)
		}
		if strings.Join(rec.Topics, ",") != "interaction" {
			t.Fatalf("重复条目应去重后退掉 danmaku/state，实际 %v", rec.Topics)
		}
	})
}

// --- 失败路径 ---

func TestLeaveRoomUnsubscribeFailurePropagates(t *testing.T) {
	e := newTestEnv(t)
	leaveLease(e, "lgwl_leave_fail", mid, model.RoleViewer)
	e.Leases.failWith("Unsubscribe", errUnsubscribeProbe)
	got, err := NewLeaveRoomLogic(ctxClient(t), e.Svc).LeaveRoom(leaveReq("lgwl_leave_fail"))
	if !errors.Is(err, errUnsubscribeProbe) || got != nil {
		t.Fatalf("Redis 故障必须原样返回（吞掉它 = 客户端以为退订了，扇出目标里还留着它），实际 err=%v reply=%+v", err, got)
	}
	if !strings.Contains(err.Error(), "leave room") {
		t.Fatalf("错误要能定位到房间与租约，实际 %v", err)
	}
	// 依赖故障不是越权：不落 DENIED 审计（否则会污染取证口径）。
	if len(e.Logs.all()) != 0 {
		t.Fatalf("Redis 故障不该落审计行: %+v", e.Logs.all())
	}
}

// 租约读取本身故障时不得按「键已回收」幂等放过。
func TestLeaveRoomLeaseLookupFailurePropagates(t *testing.T) {
	e := newTestEnv(t)
	leaveLease(e, "lgwl_leave_getfail", mid, model.RoleViewer)
	e.Leases.failWith("Get", errGetProbe)
	if _, err := NewLeaveRoomLogic(ctxClient(t), e.Svc).LeaveRoom(leaveReq("lgwl_leave_getfail")); !errors.Is(err, errGetProbe) {
		t.Fatalf("读租约故障不得降级成「已退出」，实际 %v", err)
	}
	if n := e.Leases.callCount("Unsubscribe"); n != 0 {
		t.Fatalf("身份还没核验就退订了，Unsubscribe %d 次", n)
	}
}

// errUnsubscribeProbe / errGetProbe 存储读失败探针。
var (
	errUnsubscribeProbe = errors.New("unsubscribe-probe")
	errGetProbe         = errors.New("lease-get-probe")
)

// --- 边界：LeaveRoom 不越权做 ReleaseConnectionLease 的事 ---

func TestLeaveRoomTouchesNeitherMySQLNorRouteNorLeaseState(t *testing.T) {
	e := newTestEnv(t)
	rec := leaveLease(e, "lgwl_leave_isolate", mid, model.RoleViewer)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	beforeExpire, beforeState, beforeIssued := rec.ExpireAt, rec.State, rec.IssuedAt

	req := leaveReq("lgwl_leave_isolate")
	req.Reason = "client_switch_room"
	req.TraceId = "trace-leave-1"
	got, err := NewLeaveRoomLogic(ctxClient(t), e.Svc).LeaveRoom(req)
	if err != nil || got == nil {
		t.Fatalf("合法退订失败: %v", err)
	}
	// MySQL 三张表全零：README 数据分层禁止把逐连接易失状态写进关系库。
	assertNoWrites(t, e)
	if n := e.Ticket.inserts; n != 0 {
		t.Fatalf("退订不该写重连票据，实际 Insert %d 次", n)
	}
	// 租约本身留着（可续租，重连只需再 Join 一次）：状态机不能被退订推进。
	after, gerr := e.Leases.Get(ctxClient(t), "lgwl_leave_isolate")
	if gerr != nil {
		t.Fatalf("回读租约失败: %v", gerr)
	}
	if after == nil {
		t.Fatalf("LeaveRoom 把租约删了（那是 ReleaseConnectionLease 的职责）")
	}
	if after.State != beforeState || after.ExpireAt != beforeExpire || after.IssuedAt != beforeIssued {
		t.Fatalf("退订改写了租约状态机: %+v", after)
	}
	if after.JoinedAt != 0 {
		t.Fatalf("前提不成立：本用例的租约从未 Join，joined_at 应为 0，实际 %d", after.JoinedAt)
	}
	// 路由不动：最后一个连接退出不置 OFFLINE，也不失效路由缓存。
	if e.Routes.rows[roomID].State != model.RouteStateServing {
		t.Fatalf("退订推进了路由状态: %+v", e.Routes.rows[roomID])
	}
	if n := e.Leases.callCount("CacheDel"); n != 0 {
		t.Fatalf("路由没动就不该失效路由缓存，实际 CacheDel %d 次", n)
	}
	// 断线视图不写：写了就等于伪造「该用户已断开」，会让票据回显错误的 last_offline_at。
	assertLeaveUnsubscribes(t, e, 1)
	assertLeaveDoesNotTouchLeaseOrRoute(t, e)
	// 成功退订是逐连接高频事件，不落审计。
	if len(e.Logs.all()) != 0 {
		t.Fatalf("正常退订不该落审计: %+v", e.Logs.all())
	}
}

// 同一份请求连发两次都必须成功（无 request_id 的幂等只能靠集合语义）。
func TestLeaveRoomRepeatedCallsStaySuccessful(t *testing.T) {
	e := newTestEnv(t)
	leaveLease(e, "lgwl_leave_twice", mid, model.RoleViewer)
	logic := NewLeaveRoomLogic(ctxClient(t), e.Svc)
	first, err := logic.LeaveRoom(leaveReq("lgwl_leave_twice"))
	if err != nil || first == nil {
		t.Fatalf("首次退订失败: %v", err)
	}
	second, err := logic.LeaveRoom(leaveReq("lgwl_leave_twice"))
	if err != nil || second == nil {
		t.Fatalf("重复退订必须按已退出成功返回，实际 err=%v reply=%+v", err, second)
	}
	if n := e.Leases.callCount("Unsubscribe"); n != 2 {
		t.Fatalf("两次都应真正走一遍集合移除（幂等由 SREM 本身保证），实际 %d 次", n)
	}
	if len(e.Logs.all()) != 0 {
		t.Fatalf("重复退订不该产生审计行: %+v", e.Logs.all())
	}
}

// TODO(缺陷)：leaveroomlogic.go:35 声称「reason 形状必须合法（gate.requireReason 拦住换行，
// 否则日志行会被污染）」，但 helpers.go:85-93 的 requireReason 只查「非空 + ≤256 字节」，
// 含换行的 reason 会被原样送进 g.infof/g.errorf 的日志行（本用例钉住现状）。
// 属注释与实现口径冲突：日志注入面没有被拦住。不改生产代码，登记在 README。
func TestLeaveRoomReasonWithNewlineIsNotBlocked(t *testing.T) {
	e := newTestEnv(t)
	leaveLease(e, "lgwl_leave_reason_nl", mid, model.RoleViewer)
	req := leaveReq("lgwl_leave_reason_nl")
	req.Reason = "client_left\nfake log line"
	got, err := NewLeaveRoomLogic(ctxClient(t), e.Svc).LeaveRoom(req)
	if err != nil || got == nil {
		t.Fatalf("前提不成立：当前实现并不拦换行，实际 err=%v reply=%+v", err, got)
	}
	// 判别性对照：同一条参数线上，超长 reason 确实被拦（证明不是「reason 完全没校验」）。
	e2 := newTestEnv(t)
	leaveLease(e2, "lgwl_leave_reason_long", mid, model.RoleViewer)
	req2 := leaveReq("lgwl_leave_reason_long")
	req2.Reason = strings.Repeat("r", 257)
	if _, err2 := NewLeaveRoomLogic(ctxClient(t), e2.Svc).LeaveRoom(req2); !errors.Is(err2, model.ErrEmptyReason) {
		t.Fatalf("超长 reason 应被拒，实际 %v", err2)
	}
}
