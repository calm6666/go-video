package logic

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"
)

// 本文件覆盖 ReportClientHeartbeat（心跳簿记：推进 last_heartbeat/max_seq，抽样 QoE）。
//
// 心跳是**最高频**的入口，所以它的失败方向与别的 RPC 相反，本文件围绕三条口径展开：
//  1. 客户端问题（无凭据 / 三元组不符 / 租约已死）必须是 accepted=false + 原因，
//     不能升级成 gRPC error —— 否则一次配置错误就会被客户端重试放大成风暴；
//  2. 依赖故障（Redis 读写失败）必须原样返回 error —— 否则「Redis 挂了」会被伪装成
//     「这个客户端掉线了」，进而误伤在线判定；
//  3. 心跳**永不复活租约**（复活是 Renew/重连的职责），也**永不写 MySQL**（QoE 事件链路本期未接线）。
//
// 另有一条容易写错的边界：max_seq 的初值是 0，而客户端可以从 seq=0 起计数，
// 「从未收过心跳」不能按重传处理，否则第一条心跳被永久吞掉（重传只回结论、零写入）。

// hbReq 一条与 seedActiveLease 同源的心跳。
func hbReq(leaseID string, seq int32) *rpc.ReportClientHeartbeatReq {
	return &rpc.ReportClientHeartbeatReq{
		LeaseId: leaseID, ConnId: "conn-" + leaseID, RoomId: roomID, Mid: mid, Seq: seq,
	}
}

// hbLease 播种一条活租约（conn_id 与 hbReq 同源，TTL 30s）。
// 返回的是**存储里的那条指针**（seedActiveLease 的语义），用例据此直接改状态机字段。
func hbLease(e *testEnv, leaseID string, user int64, role int32) *repository.LeaseRecord {
	return e.seedActiveLease(leaseID, "conn-"+leaseID, nodeA, roomID, user, role)
}

// hbGet 回读存储里的那条租约（心跳写的是整条记录，必须读回而不是改用例里的指针）。
func hbGet(t *testing.T, e *testEnv, leaseID string) *repository.LeaseRecord {
	t.Helper()
	rec, err := e.Leases.Get(ctxClient(t), leaseID)
	if err != nil {
		t.Fatalf("回读租约失败: %v", err)
	}
	if rec == nil {
		t.Fatalf("心跳把租约弄丢了: %s", leaseID)
	}
	return rec
}

// --- 参数形状 ---

func TestReportClientHeartbeatParameterGate(t *testing.T) {
	cases := []struct {
		name string
		req  *rpc.ReportClientHeartbeatReq
		want string
	}{
		{"seq 为负", hbReq("lgwl_hb_seq", -1), "seq must be >= 0"},
		{"房间号非正", func() *rpc.ReportClientHeartbeatReq {
			r := hbReq("lgwl_hb_room", 1)
			r.RoomId = 0
			return r
		}(), model.ErrInvalidRoomID.Error()},
		{"房间号为负", func() *rpc.ReportClientHeartbeatReq {
			r := hbReq("lgwl_hb_roomneg", 1)
			r.RoomId = -3
			return r
		}(), model.ErrInvalidRoomID.Error()},
		{"mid 为负", func() *rpc.ReportClientHeartbeatReq {
			r := hbReq("lgwl_hb_mid", 1)
			r.Mid = -1
			return r
		}(), model.ErrInvalidMid.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			hbLease(e, "lgwl_hb_param", mid, model.RoleViewer)
			got, err := NewReportClientHeartbeatLogic(ctxClient(t), e.Svc).ReportClientHeartbeat(tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("期望错误含 %q，实际 err=%v reply=%+v", tc.want, err, got)
			}
			if got != nil {
				t.Fatalf("参数畸形时不得返回响应体，实际 %+v", got)
			}
			// 形状不合法连凭据都不必查：一次存储访问都不能有。
			assertHeartbeatNoStorageCall(t, e)
		})
	}
	// in == nil：与「seq 为负」同为畸形请求，但表表达不了缺失请求体。
	e := newTestEnv(t)
	if got, err := NewReportClientHeartbeatLogic(ctxClient(t), e.Svc).ReportClientHeartbeat(nil); !errors.Is(err, model.ErrEmptyLeaseID) || got != nil {
		t.Fatalf("空请求体必须报 ErrEmptyLeaseID，实际 err=%v reply=%+v", err, got)
	}
	assertHeartbeatNoStorageCall(t, e)
}

// assertHeartbeatNoStorageCall 断言这条路径完全没碰存储（读与写都算副作用）。
func assertHeartbeatNoStorageCall(t *testing.T, e *testEnv) {
	t.Helper()
	for _, m := range []string{"Get", "Put", "Delete", "MarkOffline"} {
		if n := e.Leases.callCount(m); n != 0 {
			t.Fatalf("畸形请求却访问了存储 %s %d 次", m, n)
		}
	}
	assertNoWrites(t, e)
}

// --- 凭据结论：不回 error，但要给出真实原因 ---

func TestReportClientHeartbeatCredentialConclusionsAreReal(t *testing.T) {
	t.Run("lease_id 为空直接回绝且不查存储", func(t *testing.T) {
		e := newTestEnv(t)
		hbLease(e, "lgwl_hb_noid", mid, model.RoleViewer)
		req := hbReq("", 1)
		got, err := NewReportClientHeartbeatLogic(ctxClient(t), e.Svc).ReportClientHeartbeat(req)
		if err != nil || got == nil {
			t.Fatalf("无凭据心跳应是结论不是 error: %v", err)
		}
		if got.GetAccepted() || got.GetDenyReason() != dropReason(model.DropBadTicket) {
			t.Fatalf("无凭据心跳结论应为 BAD_TICKET，实际 %+v", got)
		}
		if got.GetServerTime() != e.Clock.unix() || got.GetTtlSeconds() != 0 {
			t.Fatalf("无法定位连接时 ttl 只能回 0，server_time 必须是服务端时刻，实际 %+v", got)
		}
		// 这条分支的全部意义：高频路径上不做无谓的存储访问。
		if n := e.Leases.callCount("Get"); n != 0 {
			t.Fatalf("空 lease_id 还去查了存储 %d 次", n)
		}
		assertNoWrites(t, e)
	})

	cases := []struct {
		name     string
		prepare  func(e *testEnv) string
		mut      func(*rpc.ReportClientHeartbeatReq)
		wantDrop int32
		wantTTL  int32
		wantAud  int
	}{
		{
			name:     "租约不存在按无效凭据",
			prepare:  func(_ *testEnv) string { return "lgwl_hb_absent" },
			wantDrop: model.DropBadTicket, wantTTL: 0, wantAud: 0,
		},
		{
			name: "已释放租约按无效凭据",
			prepare: func(e *testEnv) string {
				rec := hbLease(e, "lgwl_hb_released", mid, model.RoleViewer)
				rec.State = model.LeaseStateReleased
				return "lgwl_hb_released"
			},
			// 终态也照实回显记录本身的剩余 TTL：本方法不判定「还能不能用」，只报数；
			// 复活租约是 Renew 的职责，那边会按状态机拒掉终态。
			wantDrop: model.DropBadTicket, wantTTL: 30, wantAud: 0,
		},
		{
			name: "已被踢租约按无效凭据",
			prepare: func(e *testEnv) string {
				rec := hbLease(e, "lgwl_hb_kicked", mid, model.RoleViewer)
				rec.State = model.LeaseStateKicked
				return "lgwl_hb_kicked"
			},
			wantDrop: model.DropBadTicket, wantTTL: 30, wantAud: 0,
		},
		{
			name: "已过期租约按无效凭据",
			prepare: func(e *testEnv) string {
				rec := hbLease(e, "lgwl_hb_exp", mid, model.RoleViewer)
				rec.ExpireAt = e.Clock.unix() - 1
				return "lgwl_hb_exp"
			},
			wantDrop: model.DropBadTicket, wantTTL: 0, wantAud: 0,
		},
		{
			name: "三元组房间号不符按越权",
			prepare: func(e *testEnv) string {
				hbLease(e, "lgwl_hb_room_no", mid, model.RoleViewer)
				return "lgwl_hb_room_no"
			},
			mut:      func(r *rpc.ReportClientHeartbeatReq) { r.RoomId = otherRoom },
			wantDrop: model.DropPermissionDenied, wantTTL: 30, wantAud: 1,
		},
		{
			name: "三元组 mid 不符按越权",
			prepare: func(e *testEnv) string {
				hbLease(e, "lgwl_hb_mid_no", otherMid, model.RoleViewer)
				return "lgwl_hb_mid_no"
			},
			wantDrop: model.DropPermissionDenied, wantTTL: 30, wantAud: 1,
		},
		{
			name: "conn_id 改绑按越权",
			prepare: func(e *testEnv) string {
				hbLease(e, "lgwl_hb_conn_no", mid, model.RoleViewer)
				return "lgwl_hb_conn_no"
			},
			mut:      func(r *rpc.ReportClientHeartbeatReq) { r.ConnId = "conn-hijacked" },
			wantDrop: model.DropPermissionDenied, wantTTL: 30, wantAud: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			leaseID := tc.prepare(e)
			req := hbReq(leaseID, 7)
			if tc.mut != nil {
				tc.mut(req)
			}
			got, err := NewReportClientHeartbeatLogic(ctxClient(t), e.Svc).ReportClientHeartbeat(req)
			if err != nil {
				t.Fatalf("客户端问题不得升级成 gRPC error（重试风暴）: %v", err)
			}
			if got.GetAccepted() || got.GetDenyReason() != dropReason(tc.wantDrop) {
				t.Fatalf("结论应为 %v，实际 %+v", tc.wantDrop, got)
			}
			if got.GetTtlSeconds() != tc.wantTTL {
				t.Fatalf("剩余 TTL 应为 %d，实际 %d", tc.wantTTL, got.GetTtlSeconds())
			}
			// 拒出路径一律零写入：状态机与心跳计数都不动。
			if n := e.Leases.callCount("Put"); n != 0 {
				t.Fatalf("被拒心跳仍写了租约，Put %d 次", n)
			}
			if n := e.Quotas.creates + e.Quotas.updates + e.Routes.registers; n != 0 {
				t.Fatalf("心跳不得写配额表与路由表，实际写 %d 次", n)
			}
			rows := e.Logs.all()
			if len(rows) != tc.wantAud {
				t.Fatalf("审计行数应为 %d，实际 %d: %+v", tc.wantAud, len(rows), rows)
			}
			if tc.wantAud == 0 {
				return
			}
			row := rows[0]
			if row.State != model.BroadcastLogDenied || row.DropReason != model.DropPermissionDenied {
				t.Fatalf("越权心跳审计应为 DENIED/PERMISSION_DENIED，实际 %d/%d", row.State, row.DropReason)
			}
			if !strings.HasPrefix(row.MessageId, "heartbeat-denied:"+leaseID+":") {
				t.Fatalf("审计定位串应为 heartbeat-denied:<lease_id>:<秒>，实际 %q", row.MessageId)
			}
			// 审计记的是**租约自身**的房间（受害方），不是请求里伪冒的房间。
			if row.RoomId != roomID {
				t.Fatalf("审计房间应为租约登记的 %d，实际 %d", roomID, row.RoomId)
			}
		})
	}
}

// 越权心跳审计按秒收敛：同一秒内的风暴只留一行（message_id 带 Unix 秒）。
func TestReportClientHeartbeatDenialAuditConvergesPerSecond(t *testing.T) {
	e := newTestEnv(t)
	hbLease(e, "lgwl_hb_storm", otherMid, model.RoleViewer)
	logic := NewReportClientHeartbeatLogic(ctxClient(t), e.Svc)
	for i := 0; i < 3; i++ {
		if got, err := logic.ReportClientHeartbeat(hbReq("lgwl_hb_storm", int32(i+1))); err != nil || got.GetAccepted() {
			t.Fatalf("前提不成立：第 %d 次应为越权拒出，实际 err=%v reply=%+v", i+1, err, got)
		}
	}
	// fake 的 Insert 按 (room_id, message_id) 去重，所以「一秒一行」是可观察的。
	if n := e.Logs.dups; n == 0 {
		t.Fatalf("同秒重复越权应命中去重键（message_id 带秒级时间戳），实际无重复插入")
	}
	if len(e.Logs.all()) != 1 {
		t.Fatalf("一秒内应只收敛成 1 行，实际 %+v", e.Logs.all())
	}
	// 换到下一秒才允许留下新证据。
	e.Clock.advance(2 * time.Second)
	if _, err := logic.ReportClientHeartbeat(hbReq("lgwl_hb_storm", 9)); err != nil {
		t.Fatalf("下一秒的心跳失败: %v", err)
	}
	if len(e.Logs.all()) != 2 {
		t.Fatalf("跨秒的越权应各留一行，实际 %+v", e.Logs.all())
	}
}

// --- 心跳不复活租约 ---

func TestReportClientHeartbeatNeverResurrectsLease(t *testing.T) {
	e := newTestEnv(t)
	rec := hbLease(e, "lgwl_hb_dead", mid, model.RoleViewer)
	deadExpire := e.Clock.unix() - 5
	rec.ExpireAt = deadExpire
	got, err := NewReportClientHeartbeatLogic(ctxClient(t), e.Svc).ReportClientHeartbeat(hbReq("lgwl_hb_dead", 1))
	if err != nil || got.GetAccepted() {
		t.Fatalf("过期租约的心跳应被拒，实际 err=%v reply=%+v", err, got)
	}
	after := hbGet(t, e, "lgwl_hb_dead")
	if after.ExpireAt != deadExpire || after.HeartbeatCount != 0 || after.MaxSeq != 0 {
		t.Fatalf("心跳复活/续期了租约: %+v", after)
	}
	if after.State != model.LeaseStateActive {
		t.Fatalf("前提不成立：存储里的 state 仍是 ACTIVE（有效态由 ExpireAt 推导），实际 %d", after.State)
	}
	// 判别性对照：只把 ExpireAt 改回未来，同一条心跳就必须受理并推进计数。
	hbLease(e, "lgwl_hb_alive", mid, model.RoleViewer)
	if g2, e2 := NewReportClientHeartbeatLogic(ctxClient(t), e.Svc).ReportClientHeartbeat(hbReq("lgwl_hb_alive", 1)); e2 != nil || !g2.GetAccepted() {
		t.Fatalf("对照用例（活租约）应受理，实际 err=%v reply=%+v", e2, g2)
	}
	if a2 := hbGet(t, e, "lgwl_hb_alive"); a2.HeartbeatCount != 1 || a2.MaxSeq != 1 {
		t.Fatalf("对照用例未推进心跳簿记: %+v", a2)
	}
}

// --- 单调性：seq / last_heartbeat_at ---

// max_seq 初值是 0，客户端也可以从 0 起计数：第一条 seq=0 的心跳不能被当成重传吞掉。
func TestReportClientHeartbeatFirstZeroSeqIsNotRetransmit(t *testing.T) {
	e := newTestEnv(t)
	hbLease(e, "lgwl_hb_zeros", mid, model.RoleViewer)
	got, err := NewReportClientHeartbeatLogic(ctxClient(t), e.Svc).ReportClientHeartbeat(hbReq("lgwl_hb_zeros", 0))
	if err != nil || !got.GetAccepted() {
		t.Fatalf("首条 seq=0 必须受理，实际 err=%v reply=%+v", err, got)
	}
	if n := e.Leases.callCount("Put"); n != 1 {
		t.Fatalf("首条 seq=0 必须落库（否则重传判定永远建立不起来），Put 实际 %d 次", n)
	}
	after := hbGet(t, e, "lgwl_hb_zeros")
	if after.HeartbeatCount != 1 {
		t.Fatalf("心跳计数应为 1，实际 %+v", after)
	}
	// 判别性对照：计数已 >0 之后再回一条 seq=0 才算重传（零写入）。
	if g2, e2 := NewReportClientHeartbeatLogic(ctxClient(t), e.Svc).ReportClientHeartbeat(hbReq("lgwl_hb_zeros", 0)); e2 != nil || !g2.GetAccepted() {
		t.Fatalf("重传也要受理（否则客户端会一直重试），实际 err=%v reply=%+v", e2, g2)
	}
	if n := e.Leases.callCount("Put"); n != 1 {
		t.Fatalf("重传不该写库，Put 变成 %d 次", n)
	}
	if a2 := hbGet(t, e, "lgwl_hb_zeros"); a2.HeartbeatCount != 1 || a2.MaxSeq != 0 {
		t.Fatalf("重传推进了状态: %+v", a2)
	}
}

func TestReportClientHeartbeatSeqMonotonicity(t *testing.T) {
	e := newTestEnv(t)
	hbLease(e, "lgwl_hb_seq_mono", mid, model.RoleViewer)
	logic := NewReportClientHeartbeatLogic(ctxClient(t), e.Svc)
	for _, seq := range []int32{5, 9, 3, 9, 10} {
		got, err := logic.ReportClientHeartbeat(hbReq("lgwl_hb_seq_mono", seq))
		if err != nil || !got.GetAccepted() {
			t.Fatalf("seq=%d 心跳失败: err=%v reply=%+v", seq, err, got)
		}
	}
	after := hbGet(t, e, "lgwl_hb_seq_mono")
	// max_seq 只推进最大值；三条乱序/重复（3、9、再 9）不计簿记。
	if after.MaxSeq != 10 {
		t.Fatalf("max_seq 应为见过的最大值 10，实际 %d", after.MaxSeq)
	}
	if after.HeartbeatCount != 3 {
		t.Fatalf("heartbeat_count 应只统计真正推进的 3 次（5/9/10），实际 %d", after.HeartbeatCount)
	}
	if n := e.Leases.callCount("Put"); n != 3 {
		t.Fatalf("重传不应产生写入，Put 应为 3 次，实际 %d 次", n)
	}
}

// last_heartbeat_at 只由更晚的服务端接收时刻推进：客户端时钟跑在前面也不能把它拨快。
func TestReportClientHeartbeatLastHeartbeatOnlyAdvances(t *testing.T) {
	e := newTestEnv(t)
	rec := hbLease(e, "lgwl_hb_future", mid, model.RoleViewer)
	future := e.Clock.unix() + 3600
	rec.LastHeartbeatAt = future
	req := hbReq("lgwl_hb_future", 1)
	req.ClientTime = future // 客户端自称「已经到这个时刻」
	resp, err := NewReportClientHeartbeatLogic(ctxClient(t), e.Svc).ReportClientHeartbeat(req)
	if err != nil || !resp.GetAccepted() {
		t.Fatalf("心跳应受理，实际 err=%v reply=%+v", err, resp)
	}
	after := hbGet(t, e, "lgwl_hb_future")
	if after.LastHeartbeatAt != future {
		t.Fatalf("服务端时刻被客户端上报的 client_time 覆盖了（%d → %d）", future, after.LastHeartbeatAt)
	}
	// server_time 也必须是服务端自己的时刻：它同时是客户端校时的唯一依据。
	if resp.GetServerTime() != e.Clock.unix() {
		t.Fatalf("server_time 应回 %d，实际 %d（被 client_time=%d 带着走了）",
			e.Clock.unix(), resp.GetServerTime(), req.ClientTime)
	}
	// 判别性对照：过去时刻的 last_heartbeat_at 必须被推进到当前服务端时刻。
	past := hbLease(e, "lgwl_hb_past", mid, model.RoleViewer)
	past.LastHeartbeatAt = e.Clock.unix() - 100
	if _, err := NewReportClientHeartbeatLogic(ctxClient(t), e.Svc).ReportClientHeartbeat(hbReq("lgwl_hb_past", 1)); err != nil {
		t.Fatalf("对照心跳失败: %v", err)
	}
	if a2 := hbGet(t, e, "lgwl_hb_past"); a2.LastHeartbeatAt != e.Clock.unix() {
		t.Fatalf("last_heartbeat_at 应从过去值推进到 %d，实际 %d", e.Clock.unix(), a2.LastHeartbeatAt)
	}
}

// 回给客户端的是服务端时刻与剩余 TTL，并且 TTL 随真实时间流逝而减少（供及时续租）。
func TestReportClientHeartbeatEchoesServerTimeAndRemainingTTL(t *testing.T) {
	e := newTestEnv(t)
	hbLease(e, "lgwl_hb_ttl", mid, model.RoleViewer)
	logic := NewReportClientHeartbeatLogic(ctxClient(t), e.Svc)
	first, err := logic.ReportClientHeartbeat(hbReq("lgwl_hb_ttl", 1))
	if err != nil {
		t.Fatalf("心跳失败: %v", err)
	}
	if first.GetServerTime() != e.Clock.unix() || first.GetTtlSeconds() != 30 {
		t.Fatalf("首条应回 server_time=%d ttl=30，实际 %+v", e.Clock.unix(), first)
	}
	e.Clock.advance(12 * time.Second)
	second, err := logic.ReportClientHeartbeat(hbReq("lgwl_hb_ttl", 2))
	if err != nil {
		t.Fatalf("第二条心跳失败: %v", err)
	}
	if second.GetServerTime() != e.Clock.unix() || second.GetTtlSeconds() != 18 {
		t.Fatalf("TTL 应随时刻推进而减少到 18，实际 %+v", second)
	}
}

// --- QoE 观测：丢样本不丢心跳 ---

func TestReportClientHeartbeatQoEOutliersDropSampleOnly(t *testing.T) {
	cases := []struct {
		name string
		rtt  int32
		lag  int32
	}{
		{"rtt 为负", -1, 10},
		{"rtt 超上限", qoeSampleCeilMS + 1, 10},
		{"lag 为负", 10, -1},
		{"lag 超上限", 10, qoeSampleCeilMS + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			hbLease(e, "lgwl_hb_qoe", mid, model.RoleViewer)
			req := hbReq("lgwl_hb_qoe", 1)
			req.RttMs, req.ReceivedLagMs = tc.rtt, tc.lag
			got, err := NewReportClientHeartbeatLogic(ctxClient(t), e.Svc).ReportClientHeartbeat(req)
			if err != nil || !got.GetAccepted() {
				t.Fatalf("异常观测值不得丢心跳，实际 err=%v reply=%+v", err, got)
			}
			if after := hbGet(t, e, "lgwl_hb_qoe"); after.HeartbeatCount != 1 || after.MaxSeq != 1 {
				t.Fatalf("心跳簿记未推进（说明样本异常被当成了整体失败）: %+v", after)
			}
			// QoE 不落 MySQL（event-collector 链路本期未接线）。
			assertNoWrites(t, e)
		})
	}
	// 判别性对照：边界值本身（0 与 60000）是可采信样本，也必须同样受理。
	e := newTestEnv(t)
	hbLease(e, "lgwl_hb_qoe_ok", mid, model.RoleViewer)
	req := hbReq("lgwl_hb_qoe_ok", 1)
	req.RttMs, req.ReceivedLagMs = 0, qoeSampleCeilMS
	if got, err := NewReportClientHeartbeatLogic(ctxClient(t), e.Svc).ReportClientHeartbeat(req); err != nil || !got.GetAccepted() {
		t.Fatalf("边界观测值应受理，实际 err=%v reply=%+v", err, got)
	}
}

// --- 依赖故障：必须原样返回，不能伪装成客户端掉线 ---

func TestReportClientHeartbeatStorageFailuresPropagate(t *testing.T) {
	t.Run("读租约故障原样返回", func(t *testing.T) {
		e := newTestEnv(t)
		hbLease(e, "lgwl_hb_getfail", mid, model.RoleViewer)
		e.Leases.failWith("Get", errGetProbe)
		got, err := NewReportClientHeartbeatLogic(ctxClient(t), e.Svc).ReportClientHeartbeat(hbReq("lgwl_hb_getfail", 1))
		if !errors.Is(err, errGetProbe) || got != nil {
			t.Fatalf("Redis 读故障不得降级成 accepted=false（那是把故障伪装成掉线），实际 err=%v reply=%+v", err, got)
		}
		if n := e.Leases.callCount("Put"); n != 0 {
			t.Fatalf("身份还没核验就写库，Put %d 次", n)
		}
		assertNoWrites(t, e)
	})

	t.Run("写租约故障原样返回且不回显受理", func(t *testing.T) {
		e := newTestEnv(t)
		hbLease(e, "lgwl_hb_putfail", mid, model.RoleViewer)
		e.Leases.failWith("Put", errPutProbe)
		got, err := NewReportClientHeartbeatLogic(ctxClient(t), e.Svc).ReportClientHeartbeat(hbReq("lgwl_hb_putfail", 1))
		if !errors.Is(err, errPutProbe) || got != nil {
			t.Fatalf("写失败必须返回 error 而不是 accepted=true，实际 err=%v reply=%+v", err, got)
		}
		if after := hbGet(t, e, "lgwl_hb_putfail"); after.HeartbeatCount != 0 {
			t.Fatalf("前提不成立：Put 失败时簿记不该已落库，实际 %+v", after)
		}
		assertNoWrites(t, e)
	})
}

// errPutProbe 租约写入探针。
var errPutProbe = errors.New("lease-put-probe")
