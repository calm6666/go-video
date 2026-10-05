package logic

import (
	"context"
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

// 本文件覆盖房间广播链（BroadcastToRoom → runBroadcast）的判定与结论。
// 这一条链是本服务最核心的写路径，门禁顺序本身就是契约：
//
//	参数 → 载荷上限 → 时效 → 发送者鉴权 → 房间可广播 → 路由 → 去重 → 限流 → 扇出 → 审计
//
// 所以断言重点不在「返回了 accepted=true」，而在三件事：
//  1. 结论必须是真实原因（丢在哪一步、审计里留下哪一行、扇出打到哪几个节点）；
//  2. 顺序不能漂（鉴权在去重之前、去重在限流之前，两侧都用「零调用次数」钉住）；
//  3. 失败不降级成假成功（依赖故障必须原样返回错误，可丢弃类才走 accepted=false + drop_reason）。

// --- 构造助手 ---

// bcastReq 一条最小可用的观众态房间广播请求（凭据由用例补齐）。
func bcastReq(messageID string, kind rpc.BroadcastKind) *rpc.BroadcastToRoomReq {
	return &rpc.BroadcastToRoomReq{
		RoomId: roomID, Kind: kind, MessageId: messageID,
		SenderMid: mid, SenderRole: rpc.ConnRole_CONN_ROLE_VIEWER,
	}
}

// errRouteProbe 路由读探针：把它注入 fake 的 FindOne，就能区分
// 「这一步根本没读路由」与「读了路由并把读表错误当成结论返回」。
var errRouteProbe = errors.New("route-read-probe")

// replicaJSON 把节点列表编码成 live_gw_room_route.replica_nodes 的 JSON 形状（model 只认这个格式）。
func replicaJSON(nodes ...string) string {
	if len(nodes) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		parts = append(parts, fmt.Sprintf("%q", n))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// seedRoute 预置一条房间路由。广播链在路由不是 SERVING 时直接判 NO_ROUTE，
// 所以任何「正常下发」用例都必须先播种，否则测的是缺路由这条分支而不是扇出。
func seedRoute(e *testEnv, room int64, state int32, primary string, replicas ...string) *model.LiveGwRoomRoute {
	row := &model.LiveGwRoomRoute{
		RoomId: room, PrimaryNode: primary, ReplicaNodes: replicaJSON(replicas...),
		ShardCount: 1, State: state, Version: 1, Ctime: e.Clock.unix(), Mtime: e.Clock.unix(),
	}
	e.Routes.seed(row)
	cp := *row
	return &cp
}

// mustSeedSenderLease 播种一条活租约并同时把它当作广播凭据的出处。
func mustSeedSenderLease(t *testing.T, e *testEnv, leaseID string, room, user int64, role int32) *repository.LeaseRecord {
	t.Helper()
	rec := e.seedActiveLease(leaseID, "conn-"+leaseID, nodeA, room, user, role)
	if rec == nil {
		t.Fatalf("播种租约失败: %s", leaseID)
	}
	return rec
}

// onlyAudit 断言审计表恰好一行并返回它：多一行意味着某条路径重复落账（或该落的没落）。
func onlyAudit(t *testing.T, e *testEnv) *model.LiveGwBroadcastLog {
	t.Helper()
	rows := e.Logs.all()
	if len(rows) != 1 {
		t.Fatalf("审计表应恰好 1 行，实际 %d 行: %+v", len(rows), rows)
	}
	return rows[0]
}

// assertNoFanout 断言下发通道一次都没被碰过：这是「丢弃发生在扇出之前」的唯一硬证据。
func assertNoFanout(t *testing.T, e *testEnv) {
	t.Helper()
	if n := len(e.Tp.fanoutReqs); n != 0 {
		t.Fatalf("不该扇出，实际调用 Fanout %d 次: %+v", n, e.Tp.fanoutReqs[0])
	}
}

// assertFanoutOnce 断言恰好一次扇出（重放与拒出都不该有第二次）。
func assertFanoutOnce(t *testing.T, e *testEnv) {
	t.Helper()
	if n := len(e.Tp.fanoutReqs); n != 1 {
		t.Fatalf("应恰好扇出一次，实际 %d 次: %+v", n, e.Tp.fanoutReqs)
	}
}

// --- 参数门禁 ---

func TestBroadcastToRoomParameterGate(t *testing.T) {
	longID := strings.Repeat("m", 65)
	cases := []struct {
		name string
		req  *rpc.BroadcastToRoomReq
		want error
	}{
		{"空请求", nil, model.ErrInvalidRoomID},
		{"房间号为 0", &rpc.BroadcastToRoomReq{RoomId: 0, MessageId: "m", Kind: rpc.BroadcastKind_BROADCAST_KIND_DANMAKU}, model.ErrInvalidRoomID},
		{"房间号为负", &rpc.BroadcastToRoomReq{RoomId: -9, MessageId: "m", Kind: rpc.BroadcastKind_BROADCAST_KIND_DANMAKU}, model.ErrInvalidRoomID},
		{"message_id 为空", bcastReq("", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU), model.ErrEmptyMessageID},
		{"message_id 只有空白", bcastReq("  \t ", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU), model.ErrEmptyMessageID},
		// 超长/含空白的 message_id 由 checkIdent 统一收敛成 ErrEmptyRequestID（见交付报告：错误分类与字段名不符）。
		{"message_id 超长", bcastReq(longID, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU), model.ErrEmptyRequestID},
		{"message_id 含空格", bcastReq("m 1", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU), model.ErrEmptyRequestID},
		{"kind 未指定", bcastReq("m1", rpc.BroadcastKind_BROADCAST_KIND_UNSPECIFIED), model.ErrInvalidBroadcastKind},
		{"kind 越界", bcastReq("m1", rpc.BroadcastKind(99)), model.ErrInvalidBroadcastKind},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			lease := mustSeedSenderLease(t, e, "lgwl_p1", roomID, mid, model.RoleViewer)
			if tc.req != nil {
				tc.req.SenderLeaseId = lease.LeaseID
			}
			got, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(tc.req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("期望错误 %v，实际 %v", tc.want, err)
			}
			if got != nil {
				t.Fatalf("参数被拒时不得返回响应体，实际 %+v", got)
			}
			// 参数阶段不允许触碰任何存储：非法请求既不能占幂等键，也不能留审计行。
			assertNoFanout(t, e)
			if n := e.Leases.callCount("ClaimMessage"); n != 0 {
				t.Fatalf("参数非法却占了幂等键，ClaimMessage 调用 %d 次", n)
			}
			if len(e.Logs.all()) != 0 {
				t.Fatalf("参数非法不该落审计行: %+v", e.Logs.all())
			}
		})
	}
}

func TestBroadcastToRoomTopicFilterGate(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	lease := mustSeedSenderLease(t, e, "lgwl_topic_gate", roomID, mid, model.RoleViewer)
	logic := NewBroadcastToRoomLogic(ctxClient(t), e.Svc)

	// 未知子通道必须报错而不是静默忽略：拼错通道名会让消息永久收不到，比报错难查得多。
	req := bcastReq("m-unknown-topic", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req.SenderLeaseId = lease.LeaseID
	req.TargetTopics = []string{"danmaku", "dmakku"}
	if _, err := logic.BroadcastToRoom(req); err == nil ||
		!strings.Contains(err.Error(), "target_topics") || !strings.Contains(err.Error(), "dmakku") {
		t.Fatalf("未知子通道必须带字段名与非法值报错，实际 %v", err)
	}
	assertNoFanout(t, e)

	// 通道数是扇出放大器参数：超过 MaxTopicsPerSubscription 必须拒（配置为 8）。
	req2 := bcastReq("m-many-topic", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req2.SenderLeaseId = lease.LeaseID
	req2.TargetTopics = []string{"danmaku", "state", "interaction", "system", "moderation", "anchor_tip", "danmaku2", "x", "y"}
	if _, err := logic.BroadcastToRoom(req2); err == nil || !errors.Is(err, repository.ErrTooManyTopics) {
		t.Fatalf("通道数越界应报 ErrTooManyTopics，实际 %v", err)
	}
	if n := e.Leases.callCount("ClaimMessage"); n != 0 {
		t.Fatalf("通道过滤器非法时不该占用幂等键，实际 %d 次", n)
	}
}

// --- 正常路径：结论、审计与下发形状 ---

func TestBroadcastToRoomHappyPathConclusionAndAudit(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	lease := mustSeedSenderLease(t, e, "lgwl_ok", roomID, mid, model.RoleViewer)
	payload := []byte(`{"text":"开播第一条弹幕"}`)

	req := bcastReq("msg-happy", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req.SenderLeaseId = lease.LeaseID
	req.Payload = payload
	req.TraceId = "trace-bcast-1"

	reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
	if err != nil {
		t.Fatalf("合法广播不该失败: %v", err)
	}
	if !reply.GetAccepted() || reply.GetDropReason() != rpc.DropReason_DROP_REASON_OK {
		t.Fatalf("结论应为 accepted+DROP_REASON_OK，实际 %+v", reply)
	}
	if reply.GetMessageId() != "msg-happy" {
		t.Fatalf("回显的 message_id 必须是服务端 trim 后的入参，实际 %q", reply.GetMessageId())
	}
	if reply.GetFanoutNodes() != 1 || reply.GetTargetedConnections() != 1 {
		t.Fatalf("单节点路由应打到 1 个节点，实际 fanout=%d targeted=%d",
			reply.GetFanoutNodes(), reply.GetTargetedConnections())
	}
	if reply.GetEnqueuedAt() != e.Clock.unix() {
		t.Fatalf("受理时刻必须取服务端时钟，实际 %d", reply.GetEnqueuedAt())
	}
	// 成功路径不回报余额：Remaining 只在被限流时才有意义（proto 字段语义是「还剩多少可用」，
	// 未做限流判定时报数字反而会让客户端以为服务端在按窗口记账）。
	if reply.GetRateRemaining() != 0 {
		t.Fatalf("未被限流时 rate_remaining 必须保持 0，实际 %d", reply.GetRateRemaining())
	}

	if len(e.Tp.fanoutReqs) != 1 {
		t.Fatalf("应恰好扇出一次，实际 %d 次", len(e.Tp.fanoutReqs))
	}
	sent := e.Tp.fanoutReqs[0]
	if sent.RoomID != roomID || sent.MessageID != "msg-happy" || sent.Kind != model.KindDanmaku {
		t.Fatalf("下发请求的房间/幂等键/类别不匹配: %+v", sent)
	}
	if strings.Join(sent.Nodes, ",") != nodeA {
		t.Fatalf("扇出节点应为 [%s]，实际 %v", nodeA, sent.Nodes)
	}
	if string(sent.Payload) != string(payload) {
		t.Fatalf("下发载荷必须原样透传（本服务不改写正文）")
	}

	row := onlyAudit(t, e)
	if row.State != model.BroadcastLogSent || row.DropReason != model.DropOK {
		t.Fatalf("SENT 行的 drop_reason 必须是 OK（无因丢弃无法排障），实际 state=%d drop=%d", row.State, row.DropReason)
	}
	if row.MessageId != "msg-happy" || row.RoomId != roomID || row.Kind != model.KindDanmaku {
		t.Fatalf("审计定位列不匹配: %+v", row)
	}
	// 审计里是「服务端裁决后」的发送者，而不是客户端自报值：这里两者相同，故另有用例覆盖不同情形。
	if row.SenderMid != mid || row.SenderRole != model.RoleViewer {
		t.Fatalf("审计发送者应为凭据里的 mid+role，实际 mid=%d role=%d", row.SenderMid, row.SenderRole)
	}
	if row.PayloadBytes != int32(len(payload)) {
		t.Fatalf("payload_bytes 应等于真实字节数 %d，实际 %d", len(payload), row.PayloadBytes)
	}
	if len(row.PayloadDigest) != 32 || strings.Contains(row.PayloadDigest, "弹幕") {
		t.Fatalf("payload_digest 应是 32 位摘要而不是正文，实际 %q", row.PayloadDigest)
	}
	if got := payloadDigest(payload, e.Svc.Config.LiveGateway.PayloadDigestBytes); row.PayloadDigest != got {
		t.Fatalf("审计摘要应与 payloadDigest 同口径（同一份 sha256 前缀），期望 %q 实际 %q", got, row.PayloadDigest)
	}
	if row.TraceId != "trace-bcast-1" {
		t.Fatalf("trace_id 未透传进审计: %+v", row)
	}
	// 幂等结论必须回填，否则重试会在去重窗口里各扇出一次。
	if v := e.Leases.msgs[fmt.Sprintf("%d|%s", roomID, "msg-happy")]; v != "1|1|1|1" {
		t.Fatalf("去重窗口应回填 <accepted|drop|fanout|targeted>=1|1|1|1，实际 %q", v)
	}
}

// --- 载荷上限 ---

func TestBroadcastToRoomPayloadLimit(t *testing.T) {
	cases := []struct {
		name       string
		quotaCap   int32 // 0 表示不写 ROOM 层配额行
		payloadLen int
		wantAccept bool
	}{
		{"恰好等于进程上限", 0, 1024, true},
		{"超进程上限 1 字节", 0, 1025, false},
		{"房间层降配后按房间为准", 100, 101, false},
		{"房间层降配但载荷很小", 100, 100, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			seedRoute(e, roomID, model.RouteStateServing, nodeA)
			if tc.quotaCap > 0 {
				e.addQuotaRow(&model.LiveGwAccessQuota{
					Scope: model.QuotaScopeRoom, ScopeId: roomID, MaxPayloadBytes: tc.quotaCap, Version: 1,
				})
			}
			lease := mustSeedSenderLease(t, e, "lgwl_payload", roomID, mid, model.RoleViewer)
			req := bcastReq("msg-payload", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
			req.SenderLeaseId = lease.LeaseID
			req.Payload = []byte(strings.Repeat("x", tc.payloadLen))

			reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
			if err != nil {
				t.Fatalf("超限是可丢弃结论，不该返回错误: %v", err)
			}
			if reply.GetAccepted() != tc.wantAccept {
				t.Fatalf("accepted 应为 %v，实际 %+v", tc.wantAccept, reply)
			}
			if tc.wantAccept {
				if len(e.Tp.fanoutReqs) != 1 {
					t.Fatalf("合法载荷应扇出一次")
				}
				return
			}
			if reply.GetDropReason() != rpc.DropReason_DROP_REASON_PAYLOAD_TOO_LARGE {
				t.Fatalf("超限必须回 DROP_REASON_PAYLOAD_TOO_LARGE，实际 %v", reply.GetDropReason())
			}
			assertNoFanout(t, e)
			// 载荷上限在去重与限流之前：超限消息不该烧掉幂等键、也不该计入 QPS 窗口。
			if n := e.Leases.callCount("ClaimMessage"); n != 0 {
				t.Fatalf("超限消息不该占幂等键，ClaimMessage %d 次", n)
			}
			if n := e.Leases.callCount("AllowRate"); n != 0 {
				t.Fatalf("超限消息不该计限流，AllowRate %d 次", n)
			}
			row := onlyAudit(t, e)
			if row.State != model.BroadcastLogDropped || row.DropReason != model.DropPayloadTooLarge {
				t.Fatalf("超限审计应为 DROPPED+PAYLOAD_TOO_LARGE，实际 state=%d drop=%d", row.State, row.DropReason)
			}
			if row.PayloadBytes != int32(tc.payloadLen) {
				t.Fatalf("超限行也要记下真实体积（排障要知道多大），实际 %d", row.PayloadBytes)
			}
		})
	}
}

// TestBroadcastPayloadLimitMustClampToProcessCap 钉住载荷上限 = min(生效配额, 进程上限)：
// 房间级配额只能往下调上限，不能把进程护栏抬到任意高度（否则一条运营配置就等于取消载荷上限，
// 而 UpsertAccessQuota 的 CheckQuotaBounds 在进程配置 MaxPayloadBytes=0 时连上界都不校）。
func TestBroadcastPayloadLimitMustClampToProcessCap(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	e.addQuotaRow(&model.LiveGwAccessQuota{
		Scope: model.QuotaScopeRoom, ScopeId: roomID, MaxPayloadBytes: 1 << 20, Version: 1,
	})
	lease := mustSeedSenderLease(t, e, "lgwl_clamp", roomID, mid, model.RoleViewer)
	req := bcastReq("msg-clamp", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req.SenderLeaseId = lease.LeaseID
	req.Payload = []byte(strings.Repeat("x", 4096)) // 进程上限是 1024
	reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
	if err != nil {
		t.Fatalf("不该返回错误: %v", err)
	}
	if reply.GetAccepted() {
		t.Fatalf("进程上限必须兜住配额行，实际受理了 4096 字节载荷")
	}
	if reply.GetDropReason() != rpc.DropReason_DROP_REASON_PAYLOAD_TOO_LARGE {
		t.Fatalf("夹取后的拒发必须说明是载荷超限，实际 %+v", reply)
	}
	assertNoFanout(t, e)
	// 反向：配额往下调仍然生效（否则「按房间收紧」这个用途也被夹取搞坏了）。
	e2 := newTestEnv(t)
	seedRoute(e2, roomID, model.RouteStateServing, nodeA)
	e2.addQuotaRow(&model.LiveGwAccessQuota{
		Scope: model.QuotaScopeRoom, ScopeId: roomID, MaxPayloadBytes: 128, Version: 1,
	})
	req2 := bcastReq("msg-tighter", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req2.SenderLeaseId = mustSeedSenderLease(t, e2, "lgwl_tighter", roomID, mid, model.RoleViewer).LeaseID
	req2.Payload = []byte(strings.Repeat("x", 512))
	r2, err := NewBroadcastToRoomLogic(ctxClient(t), e2.Svc).BroadcastToRoom(req2)
	if err != nil {
		t.Fatalf("不该返回错误: %v", err)
	}
	if r2.GetAccepted() || r2.GetDropReason() != rpc.DropReason_DROP_REASON_PAYLOAD_TOO_LARGE {
		t.Fatalf("比进程上限更严的房间配额必须照常生效: %+v", r2)
	}
}

// --- 时效 ---

func TestBroadcastToRoomExpiryGate(t *testing.T) {
	now := testBaseTime
	cases := []struct {
		name       string
		expireAt   int64
		wantAccept bool
		wantDrop   rpc.DropReason
	}{
		{"未设置时效", 0, true, rpc.DropReason_DROP_REASON_OK},
		{"未来时刻", now + 60, true, rpc.DropReason_DROP_REASON_OK},
		{"恰好到点即视为过期", now, false, rpc.DropReason_DROP_REASON_NO_SUBSCRIBER},
		{"已过期", now - 1, false, rpc.DropReason_DROP_REASON_NO_SUBSCRIBER},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			seedRoute(e, roomID, model.RouteStateServing, nodeA)
			lease := mustSeedSenderLease(t, e, "lgwl_expire", roomID, mid, model.RoleViewer)
			req := bcastReq("msg-expire", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
			req.SenderLeaseId = lease.LeaseID
			req.ExpireAt = tc.expireAt
			reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
			if err != nil {
				t.Fatalf("过期是可丢弃结论: %v", err)
			}
			if reply.GetAccepted() != tc.wantAccept || reply.GetDropReason() != tc.wantDrop {
				t.Fatalf("期望 accepted=%v drop=%v，实际 %+v", tc.wantAccept, tc.wantDrop, reply)
			}
			if tc.wantAccept {
				return
			}
			// 过期既不占幂等键也不占配额（时效判定在鉴权之前、去重之前）。
			assertNoFanout(t, e)
			if n := e.Leases.callCount("ClaimMessage"); n != 0 {
				t.Fatalf("过期消息不该占幂等键，ClaimMessage %d 次", n)
			}
			if e.Rooms.broadcastCalls != 0 {
				t.Fatalf("过期消息不该去问房间可广播性，实际问了 %d 次", e.Rooms.broadcastCalls)
			}
			row := onlyAudit(t, e)
			if row.State != model.BroadcastLogDropped || row.DropReason != model.DropNoSubscriber {
				t.Fatalf("过期应为 DROPPED+NO_SUBSCRIBER（契约里没有「已过期」一档，见 broadcastcore.go:87）, 实际 state=%d drop=%d",
					row.State, row.DropReason)
			}
		})
	}
}

// --- 发送者鉴权 ---

func TestBroadcastToRoomSenderAdjudication(t *testing.T) {
	cases := []struct {
		name string
		// 凭据形态
		lease  func(e *testEnv) string
		noCred bool
		// 期望
		wantDrop int32
		// 期望审计状态
		wantState int32
		advance   time.Duration // 推进时钟，用来造「租约已过期」
	}{
		{
			name: "带自己房间的活租约", wantDrop: model.DropOK, wantState: model.BroadcastLogSent,
			lease: func(e *testEnv) string {
				return mustSeedSenderLease(t, e, "lgwl_a1", roomID, mid, model.RoleViewer).LeaseID
			},
		},
		{
			name: "不带任何凭据", noCred: true, wantDrop: model.DropPermissionDenied, wantState: model.BroadcastLogDenied,
		},
		{
			name: "租约不存在", wantDrop: model.DropBadTicket, wantState: model.BroadcastLogDenied,
			lease: func(e *testEnv) string { return "lgwl_not_here" },
		},
		{
			name: "租约属于别的房间", wantDrop: model.DropPermissionDenied, wantState: model.BroadcastLogDenied,
			lease: func(e *testEnv) string {
				return mustSeedSenderLease(t, e, "lgwl_a2", otherRoom, mid, model.RoleViewer).LeaseID
			},
		},
		{
			name: "租约是别人的", wantDrop: model.DropPermissionDenied, wantState: model.BroadcastLogDenied,
			lease: func(e *testEnv) string {
				return mustSeedSenderLease(t, e, "lgwl_a3", roomID, otherMid, model.RoleViewer).LeaseID
			},
		},
		{
			name: "租约已主动离场", wantDrop: model.DropBadTicket, wantState: model.BroadcastLogDenied,
			lease: func(e *testEnv) string {
				rec := mustSeedSenderLease(t, e, "lgwl_a4", roomID, mid, model.RoleViewer)
				rec.State = model.LeaseStateReleased
				return rec.LeaseID
			},
		},
		{
			name: "租约已被踢下线", wantDrop: model.DropBadTicket, wantState: model.BroadcastLogDenied,
			lease: func(e *testEnv) string {
				rec := mustSeedSenderLease(t, e, "lgwl_a5", roomID, mid, model.RoleViewer)
				rec.State = model.LeaseStateKicked
				return rec.LeaseID
			},
		},
		{
			name: "租约已过期", wantDrop: model.DropBadTicket, wantState: model.BroadcastLogDenied,
			advance: 31 * time.Second, // seedActiveLease 给 30 秒 TTL
			lease: func(e *testEnv) string {
				return mustSeedSenderLease(t, e, "lgwl_a6", roomID, mid, model.RoleViewer).LeaseID
			},
		},
		{
			name: "房管发弹幕", wantDrop: model.DropOK, wantState: model.BroadcastLogSent,
			lease: func(e *testEnv) string {
				return mustSeedSenderLease(t, e, "lgwl_a7", roomID, mid, model.RoleRoomAdmin).LeaseID
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			seedRoute(e, roomID, model.RouteStateServing, nodeA)
			req := bcastReq("msg-auth", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
			if tc.lease != nil {
				req.SenderLeaseId = tc.lease(e)
			}
			// 时钟必须在凭据播种之后推进：否则「过期」用例里的 30 秒 TTL 会从新时刻起算，
			// 测到的是一条活租约。
			if tc.advance > 0 {
				e.Clock.advance(tc.advance)
			}
			reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
			if err != nil {
				t.Fatalf("鉴权拒出是可解释结论，不该返回错误: %v", err)
			}
			if int32(reply.GetDropReason()) != tc.wantDrop {
				t.Fatalf("期望 drop=%d，实际 %+v", tc.wantDrop, reply)
			}
			if reply.GetAccepted() != (tc.wantDrop == model.DropOK) {
				t.Fatalf("accepted 与 drop 不自洽: %+v", reply)
			}
			if tc.wantDrop != model.DropOK {
				assertNoFanout(t, e)
			} else {
				assertFanoutOnce(t, e)
			}
			row := onlyAudit(t, e)
			if row.State != tc.wantState {
				t.Fatalf("审计状态应为 %d，实际 %d", tc.wantState, row.State)
			}
			if row.DropReason != tc.wantDrop {
				t.Fatalf("审计的丢弃原因必须是真实原因，期望 %d 实际 %d", tc.wantDrop, row.DropReason)
			}
			// 鉴权阶段的审计里只有自报值可写（真角色还没裁决出来），这一点必须被固定下来：
			// 「拒了但查不到是谁拒的」在追责时与「没拒」等价。
			if row.SenderMid != mid {
				t.Fatalf("审计必须留下被拒的 mid，实际 %d", row.SenderMid)
			}
		})
	}
}

func TestBroadcastToRoomKindPermissionMatrix(t *testing.T) {
	cases := []struct {
		name       string
		role       rpc.ConnRole
		kind       rpc.BroadcastKind
		wantAccept bool
		wantDrop   int32
	}{
		{"观众发弹幕", rpc.ConnRole_CONN_ROLE_VIEWER, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU, true, model.DropOK},
		{"观众发互动提示", rpc.ConnRole_CONN_ROLE_VIEWER, rpc.BroadcastKind_BROADCAST_KIND_INTERACTION, true, model.DropOK},
		{"观众发房间状态", rpc.ConnRole_CONN_ROLE_VIEWER, rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE, false, model.DropPermissionDenied},
		{"观众发审核处置", rpc.ConnRole_CONN_ROLE_VIEWER, rpc.BroadcastKind_BROADCAST_KIND_MODERATION, false, model.DropPermissionDenied},
		{"观众发主播提词", rpc.ConnRole_CONN_ROLE_VIEWER, rpc.BroadcastKind_BROADCAST_KIND_ANCHOR_TIP, false, model.DropPermissionDenied},
		{"主播发弹幕", rpc.ConnRole_CONN_ROLE_ANCHOR, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU, true, model.DropOK},
		// 权限矩阵先于凭据校验：ANCHOR 属「非可信来源」，主播态客户端也发不了房间状态。
		{"主播发房间状态", rpc.ConnRole_CONN_ROLE_ANCHOR, rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE, false, model.DropPermissionDenied},
		// 房管可发弹幕/互动提示/「系统公告」文本，但 kind=SYSTEM 落在 KindRequiresTrustedSender 里：
		// 系统类消息一律要求 SERVICE/OPERATOR，房管矩阵允许也不够 —— 两道门是「与」关系，取更严的一侧。
		{"房管发 kind=SYSTEM 公告", rpc.ConnRole_CONN_ROLE_ROOM_ADMIN, rpc.BroadcastKind_BROADCAST_KIND_SYSTEM, false, model.DropPermissionDenied},
		{"房管发互动提示", rpc.ConnRole_CONN_ROLE_ROOM_ADMIN, rpc.BroadcastKind_BROADCAST_KIND_INTERACTION, true, model.DropOK},
		{"房管不能发房间状态", rpc.ConnRole_CONN_ROLE_ROOM_ADMIN, rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE, false, model.DropPermissionDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			seedRoute(e, roomID, model.RouteStateServing, nodeA)
			// 矩阵里允许的角色都有对应租约，保证结论来自矩阵而不是凭据缺失。
			lease := mustSeedSenderLease(t, e, "lgwl_matrix", roomID, mid, int32(tc.role))
			if tc.role == rpc.ConnRole_CONN_ROLE_ANCHOR {
				// ANCHOR 必须真是 live-room 认定的房主，否则 gateAnchorOwnership 会先把角色降成 VIEWER。
				lease.Mid = anchorMid
				e.Rooms.owner = anchorMid
			}
			req := &rpc.BroadcastToRoomReq{
				RoomId: roomID, Kind: tc.kind, MessageId: "msg-matrix",
				SenderMid: lease.Mid, SenderRole: tc.role, SenderLeaseId: lease.LeaseID,
			}
			reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
			if err != nil {
				t.Fatalf("不该返回错误: %v", err)
			}
			if reply.GetAccepted() != tc.wantAccept {
				t.Fatalf("期望 accepted=%v，实际 %+v", tc.wantAccept, reply)
			}
			if int32(reply.GetDropReason()) != tc.wantDrop {
				t.Fatalf("期望 drop=%d，实际 %+v", tc.wantDrop, reply)
			}
			if !tc.wantAccept {
				assertNoFanout(t, e)
				if n := e.Leases.callCount("Get"); n != 0 {
					t.Fatalf("矩阵拒出必须发生在读凭据之前（原因不能退化成 BAD_TICKET），Get 调用 %d 次", n)
				}
				row := onlyAudit(t, e)
				if row.State != model.BroadcastLogDenied {
					t.Fatalf("越权必须留 DENIED 行，实际 state=%d", row.State)
				}
			}
		})
	}
}

// TestBroadcastSenderCredentialRoleMustBeRecheckedAgainstMatrix 钉住凭据换角色后的矩阵复检。
// resolveSender 先跑一遍矩阵（helpers.go:matrixDrop），读到租约/票据后才把 v.Role 换成
// 服务端判定角色；若换完不复检，「未归因客户端 + 自己那张 VIEWER 租约 + sender_role=OPERATOR」
// 就能把 ROOM_STATE / SYSTEM / MODERATION / ANCHOR_TIP 发出去，而
// model.RoleAllowedToSend(VIEWER, 这些类别) 明确为 false —— 矩阵形同虚设。
// 租约与重连票据两条凭据分支都要复检；归因主体（metadata 认 SERVICE/OPERATOR）不在此列，
// 它的矩阵判定按归因角色（宽）、凭据只用于记账，见 TestBroadcastToRoomAttestedCallerExemptions。
func TestBroadcastSenderCredentialRoleMustBeRecheckedAgainstMatrix(t *testing.T) {
	// 事实源先行：矩阵本身判 VIEWER 不可发 ROOM_STATE，越权只能来自「换了角色不复检」。
	if model.RoleAllowedToSend(model.RoleViewer, model.KindRoomState) {
		t.Fatal("权限矩阵事实源本身异常：VIEWER 被判定可发 ROOM_STATE")
	}
	// 反向前提：自报 OPERATOR 必须过得了第一遍（凭据之前的）矩阵，否则本用例的拒出就
	// 可能来自第一遍而不是复检，用例失去判别力。
	if !model.RoleAllowedToSend(model.RoleOperator, model.KindRoomState) {
		t.Fatal("前提不成立：OPERATOR 被判定不可发 ROOM_STATE，本用例测不到「换了角色不复检」")
	}
	cases := []struct {
		name     string
		cred     func(t *testing.T, e *testEnv, req *rpc.BroadcastToRoomReq)
		evidence func(t *testing.T, e *testEnv)
	}{
		{"租约", func(t *testing.T, e *testEnv, req *rpc.BroadcastToRoomReq) {
			t.Helper()
			req.SenderLeaseId = mustSeedSenderLease(t, e, "lgwl_esc_lease", roomID, mid, model.RoleViewer).LeaseID
		}, func(t *testing.T, e *testEnv) {
			t.Helper()
			// 拒出必须发生在租约读出来之后：证明 PERMISSION_DENIED 来自复检，
			// 而不是「矩阵在读凭据前就拒了」（那条路径由 KindPermissionMatrix 用例钉住，且 Get 恰好 0 次）。
			if n := e.Leases.callCount("Get"); n == 0 {
				t.Fatalf("凭据从未被读取，拒出不可能来自角色复检，用例已失去判别力：Get 调用 %d 次", n)
			}
		}},
		{"重连票据", func(t *testing.T, e *testEnv, req *rpc.BroadcastToRoomReq) {
			t.Helper()
			lease := mustSeedSenderLease(t, e, "lgwl_esc_ticket", roomID, mid, model.RoleViewer)
			// 票据的角色继承自签发它的租约，所以这张票据同样是观众态。
			req.SenderTicket, _ = mustIssueTicket(t, e, lease.LeaseID, "req-escalation-ticket")
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			seedRoute(e, roomID, model.RouteStateServing, nodeA)
			req := bcastReq("msg-escalation", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE)
			// 自报角色：caller.go 的注释明确它「不能作为授权依据」。
			req.SenderRole = rpc.ConnRole_CONN_ROLE_OPERATOR
			tc.cred(t, e, req)
			reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
			if err != nil {
				t.Fatalf("不该返回错误: %v", err)
			}
			if reply.GetAccepted() {
				t.Fatalf("观众态凭据不得发出 ROOM_STATE，实际受理并扇出 %+v", reply)
			}
			// 结论必须是权限拒绝，不能被降级成 BAD_TICKET / NO_SUBSCRIBER —— 那说明凭据本身被判坏了，
			// 而不是「真角色过不了矩阵」，两条路径的排障方向完全不同。
			if reply.GetDropReason() != rpc.DropReason_DROP_REASON_PERMISSION_DENIED {
				t.Fatalf("越权提权必须回 PERMISSION_DENIED，实际 %+v", reply)
			}
			assertNoFanout(t, e)
			row := onlyAudit(t, e)
			if row.State != model.BroadcastLogDenied {
				t.Fatalf("越权必须留 DENIED 审计，实际 state=%d", row.State)
			}
			// 非 SENT 行记的是自报角色（auditDrop 用 ClaimedRole）：取证要看的正是「谁在冒充 OPERATOR」。
			// 受理成功的那一行才记解析后的真角色，两侧不对称是刻意的。
			if row.SenderRole != model.RoleOperator {
				t.Fatalf("拒出审计要留下冒充的角色自报值，实际 sender_role=%d", row.SenderRole)
			}
			if tc.evidence != nil {
				tc.evidence(t, e)
			}
		})
	}
}

func TestBroadcastToRoomAttestedCallerExemptions(t *testing.T) {
	cases := []struct {
		name     string
		ctx      context.Context
		claimed  rpc.ConnRole
		mid      int64
		lease    bool
		wantRole int32
		wantMid  int64
	}{
		// 归因主体优先于自报角色：填错的 claimed_role 不能给内部链路降权。
		{"归因内部服务自报观众", ctxService(t, "moderation"), rpc.ConnRole_CONN_ROLE_VIEWER, 0, false, model.RoleService, 0},
		{"归因运营自报主播", ctxOperator(t, "777"), rpc.ConnRole_CONN_ROLE_ANCHOR, 0, false, model.RoleOperator, 0},
		// 归因主体免凭据，但一旦带上用户态凭据，审计记的就是凭据里的真实发送者（内部代发不改记主体）。
		// 注意两侧不对称：矩阵按归因角色（宽）判，归因记凭据角色（实）—— 反向的「凭据角色比自报低却不再复检」
		// 是一条真实的提权通道，已由 resolveSender 的复检关掉并在
		// TestBroadcastSenderCredentialRoleMustBeRecheckedAgainstMatrix 里钉住；本用例只覆盖归因主体这一侧
		// （它跳过复检，否则 moderation 带一张观众租约代发公告会被误拒）。
		{"归因服务带用户凭据时按凭据记账", ctxService(t, "moderation"), rpc.ConnRole_CONN_ROLE_SERVICE, mid, true, model.RoleViewer, mid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			seedRoute(e, roomID, model.RouteStateServing, nodeA)
			req := bcastReq("msg-attested", rpc.BroadcastKind_BROADCAST_KIND_SYSTEM)
			req.SenderRole = tc.claimed
			req.SenderMid = tc.mid
			if tc.lease {
				req.SenderLeaseId = mustSeedSenderLease(t, e, "lgwl_att", roomID, mid, model.RoleViewer).LeaseID
			}
			reply, err := NewBroadcastToRoomLogic(tc.ctx, e.Svc).BroadcastToRoom(req)
			if err != nil {
				t.Fatalf("归因主体不该失败: %v", err)
			}
			if !reply.GetAccepted() {
				t.Fatalf("归因主体发的系统公告被拒: %+v", reply)
			}
			row := onlyAudit(t, e)
			if row.SenderRole != tc.wantRole || row.SenderMid != tc.wantMid {
				t.Fatalf("审计归因应为 %d/%d，实际 %d/%d", tc.wantRole, tc.wantMid, row.SenderRole, row.SenderMid)
			}
		})
	}
}

func TestBroadcastToRoomUnattestedCannotClaimPrivilege(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	// 自报 OPERATOR 但没有任何凭据：矩阵放行（自报角色即 OPERATOR），凭据门禁必须把它挡住。
	req := bcastReq("msg-claim-ops", rpc.BroadcastKind_BROADCAST_KIND_MODERATION)
	req.SenderRole = rpc.ConnRole_CONN_ROLE_OPERATOR
	req.SenderMid = mid
	reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
	if err != nil {
		t.Fatalf("不该返回错误: %v", err)
	}
	if reply.GetAccepted() || reply.GetDropReason() != rpc.DropReason_DROP_REASON_PERMISSION_DENIED {
		t.Fatalf("未归因主体自报运营必须被拒，实际 %+v", reply)
	}
	assertNoFanout(t, e)
	row := onlyAudit(t, e)
	if row.State != model.BroadcastLogDenied || row.SenderRole != model.RoleOperator {
		t.Fatalf("DENIED 行要记下它自报的角色（越权探测的画像），实际 %+v", row)
	}
}

func TestBroadcastToRoomTicketCredentialIsVerifiedNotConsumed(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	lease := mustSeedSenderLease(t, e, "lgwl_ticket_src", roomID, mid, model.RoleViewer)
	ticket, info := mustIssueTicket(t, e, lease.LeaseID, "req-bcast-ticket")

	req := bcastReq("msg-with-ticket", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req.SenderTicket = ticket
	reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
	if err != nil {
		t.Fatalf("票据鉴权不该失败: %v", err)
	}
	if !reply.GetAccepted() {
		t.Fatalf("合法票据应放行，实际 %+v", reply)
	}
	// 广播鉴权只校验不消费：消费点只能是 Redeem，否则客户端下一次重连必失败。
	row := revokedRow(t, e, info.GetTicketId())
	if !e.Leases.hasTicketBit(row.TicketHash) {
		t.Fatalf("广播不得消费票据的 Redis 有效位（Redis 里的键还在才算没消费）")
	}
	if row.State != model.TicketStateIssued {
		t.Fatalf("广播鉴权不得把票据推成终态，实际 state=%d", row.State)
	}
	if n := e.Leases.callCount("ConsumeTicket"); n != 0 {
		t.Fatalf("广播路径不该调用 ConsumeTicket，实际 %d 次", n)
	}
}

// --- 顺序契约：鉴权在去重之前 ---

func TestBroadcastToRoomAuthPrecedesDedup(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	// 攻击者复用一条「已成功受理过」的 message_id 来发越权消息：
	// 若去重在鉴权之前，这次探测会被判 DUPLICATED 且一行审计都不留。
	e.Leases.seedMessage(roomID, "msg-reuse", encodeBroadcastOutcome(broadcastOutcome{Accepted: true, Drop: model.DropOK}))
	req := bcastReq("msg-reuse", rpc.BroadcastKind_BROADCAST_KIND_MODERATION)
	req.SenderLeaseId = mustSeedSenderLease(t, e, "lgwl_reuse", roomID, mid, model.RoleViewer).LeaseID

	reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
	if err != nil {
		t.Fatalf("不该返回错误: %v", err)
	}
	if reply.GetDuplicated() {
		t.Fatalf("越权探测不能被幂等回放掩盖: %+v", reply)
	}
	if reply.GetDropReason() != rpc.DropReason_DROP_REASON_PERMISSION_DENIED {
		t.Fatalf("结论必须是 PERMISSION_DENIED，实际 %+v", reply)
	}
	if n := e.Leases.callCount("ClaimMessage"); n != 0 {
		t.Fatalf("鉴权在去重之前，ClaimMessage 不该被调用，实际 %d 次", n)
	}
	row := onlyAudit(t, e)
	if row.State != model.BroadcastLogDenied {
		t.Fatalf("越权必须留 DENIED 审计，实际 state=%d", row.State)
	}
}

// --- 路由与房间可广播性 ---

func TestBroadcastToRoomRoomGateDecisions(t *testing.T) {
	cases := []struct {
		name       string
		unwired    bool
		roomClosed bool
		reason     string
		gateErr    error
		wantErr    bool
		wantDrop   rpc.DropReason
	}{
		{name: "房间可广播", wantDrop: rpc.DropReason_DROP_REASON_OK},
		{name: "房间已关闭", roomClosed: true, reason: "room ended", wantDrop: rpc.DropReason_DROP_REASON_ROOM_CLOSED},
		{name: "live-room 未接线", unwired: true, wantErr: true},
		{name: "live-room 查询故障", gateErr: errors.New("rpc unavailable"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			seedRoute(e, roomID, model.RouteStateServing, nodeA)
			// 注入探针错误：门禁这一步就出结论时，绝不能已经去读路由表（顺序即成本，也即故障面）。
			if tc.roomClosed || tc.wantErr {
				e.Routes.fail("FindOne", errRouteProbe)
			}
			e.Rooms.broadcastable = !tc.roomClosed
			e.Rooms.reason = tc.reason
			e.Rooms.broadcastErr = tc.gateErr
			if tc.unwired {
				e.markRoomGateUnwired()
			}
			req := bcastReq("msg-roomgate", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
			req.SenderLeaseId = mustSeedSenderLease(t, e, "lgwl_rg", roomID, mid, model.RoleViewer).LeaseID
			reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("房间状态问不到真值时必须显式失败，不能默认「能发」，实际 reply=%+v", reply)
				}
				if tc.unwired && !errors.Is(err, repository.ErrLiveRoomNotConfigured) {
					t.Fatalf("未接线应报 ErrLiveRoomNotConfigured（README 已知缺口里的 fail-closed 口径），实际 %v", err)
				}
				if !tc.unwired && errors.Is(err, errRouteProbe) {
					t.Fatalf("房间门禁故障时不该去读路由表（读到了假结论）: %v", err)
				}
				assertNoFanout(t, e)
				return
			}
			if err != nil {
				t.Fatalf("不该返回错误: %v", err)
			}
			if reply.GetDropReason() != tc.wantDrop {
				t.Fatalf("期望 drop=%v，实际 %+v", tc.wantDrop, reply)
			}
			row := onlyAudit(t, e)
			if !tc.roomClosed {
				// 可广播：门禁放行后照常走完扇出。
				assertFanoutOnce(t, e)
				if row.State != model.BroadcastLogSent {
					t.Fatalf("可广播时该落 SENT，实际 %+v", row)
				}
				return
			}
			// 房间不可广播时必须「一次都不打」：判据在门禁，扇出没发生才算真的没送达。
			assertNoFanout(t, e)
			// 房间是否可广播由 live-room 判定，本服务不复判也不改写原因。
			if row.State != model.BroadcastLogDropped || row.DropReason != model.DropRoomClosed {
				t.Fatalf("房间不可广播应落 DROPPED+ROOM_CLOSED，实际 %+v", row)
			}
			if e.Rooms.broadcastCalls != 1 {
				t.Fatalf("房间可广播性应恰好问一次，实际 %d 次", e.Rooms.broadcastCalls)
			}
		})
	}
}

func TestBroadcastToRoomRouteDecisions(t *testing.T) {
	cases := []struct {
		name      string
		state     int32
		hasRow    bool
		replicas  []string
		wantDrop  rpc.DropReason
		wantNodes string
		wantSend  bool
	}{
		{name: "SERVING 主节点", hasRow: true, state: model.RouteStateServing, wantDrop: rpc.DropReason_DROP_REASON_OK, wantNodes: nodeA, wantSend: true},
		{name: "无路由行", wantDrop: rpc.DropReason_DROP_REASON_NO_ROUTE},
		{name: "OFFLINE 路由", hasRow: true, state: model.RouteStateOffline, wantDrop: rpc.DropReason_DROP_REASON_NO_ROUTE},
		{name: "DRAINING 且只有主节点", hasRow: true, state: model.RouteStateDraining, wantDrop: rpc.DropReason_DROP_REASON_NO_ROUTE},
		{name: "DRAINING 带副本", hasRow: true, state: model.RouteStateDraining, replicas: []string{nodeB},
			wantDrop: rpc.DropReason_DROP_REASON_OK, wantNodes: nodeB, wantSend: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			if tc.hasRow {
				seedRoute(e, roomID, tc.state, nodeA, tc.replicas...)
			}
			req := bcastReq("msg-route", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
			req.SenderLeaseId = mustSeedSenderLease(t, e, "lgwl_rt", roomID, mid, model.RoleViewer).LeaseID
			reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
			if err != nil {
				t.Fatalf("无路由是可丢弃结论: %v", err)
			}
			if reply.GetDropReason() != tc.wantDrop {
				t.Fatalf("期望 drop=%v，实际 %+v", tc.wantDrop, reply)
			}
			if !tc.wantSend {
				assertNoFanout(t, e)
				row := onlyAudit(t, e)
				if row.State != model.BroadcastLogDropped || row.DropReason != model.DropNoRoute {
					t.Fatalf("路由不可用应落 DROPPED+NO_ROUTE，实际 %+v", row)
				}
				// 路由判定点在去重之前（broadcastcore.go:128 早于 :149）：键都不能出现。
				// 若在路由失败时就抢了幂等键，路由恢复后这条消息永远重投不出去。
				if n := e.Leases.callCount("ClaimMessage"); n != 0 {
					t.Fatalf("路由丢弃不该抢幂等键，ClaimMessage %d 次", n)
				}
				if _, ok := e.Leases.msgs[fmt.Sprintf("%d|%s", roomID, "msg-route")]; ok {
					t.Fatalf("路由丢弃不能留下任何幂等结论，否则同一条消息永久重投不出去")
				}
				return
			}
			if len(e.Tp.fanoutReqs) != 1 {
				t.Fatalf("应扇出一次")
			}
			if got := strings.Join(e.Tp.fanoutReqs[0].Nodes, ","); got != tc.wantNodes {
				t.Fatalf("扇出节点应为 %q，实际 %q", tc.wantNodes, got)
			}
		})
	}
}

func TestBroadcastToRoomDirtyReplicaNodesDegradeToPrimary(t *testing.T) {
	e := newTestEnv(t)
	e.Routes.seed(&model.LiveGwRoomRoute{
		RoomId: roomID, PrimaryNode: nodeA, ReplicaNodes: "{not-json", ShardCount: 1,
		State: model.RouteStateServing, Version: 1,
	})
	req := bcastReq("msg-dirty", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req.SenderLeaseId = mustSeedSenderLease(t, e, "lgwl_dirty", roomID, mid, model.RoleViewer).LeaseID
	reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
	if err != nil {
		t.Fatalf("副本列脏数据不该拒发（主节点仍有效）: %v", err)
	}
	if !reply.GetAccepted() {
		t.Fatalf("应仍按主节点下发: %+v", reply)
	}
	if got := strings.Join(e.Tp.fanoutReqs[0].Nodes, ","); got != nodeA {
		t.Fatalf("脏副本列必须按「无副本」处理，实际节点 %q", got)
	}
}

// --- 幂等 ---

func TestBroadcastToRoomIdempotentReplayDoesNotRefanout(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA, nodeB)
	lease := mustSeedSenderLease(t, e, "lgwl_idem", roomID, mid, model.RoleViewer)
	logic := NewBroadcastToRoomLogic(ctxClient(t), e.Svc)
	req := bcastReq("msg-idem", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req.SenderLeaseId = lease.LeaseID

	first, err := logic.BroadcastToRoom(req)
	if err != nil {
		t.Fatalf("首次广播失败: %v", err)
	}
	if !first.GetAccepted() || first.GetDuplicated() || first.GetFanoutNodes() != 2 {
		t.Fatalf("首次结论异常: %+v", first)
	}
	ratesAfterFirst := e.Leases.callCount("AllowRate")

	req2 := bcastReq("msg-idem", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req2.SenderLeaseId = lease.LeaseID
	req2.Payload = []byte(`{"text":"换了正文也不该被第二次投递"}`)
	second, err := logic.BroadcastToRoom(req2)
	if err != nil {
		t.Fatalf("重放不该报错: %v", err)
	}
	if len(e.Tp.fanoutReqs) != 1 {
		t.Fatalf("重放不得二次扇出，实际 %d 次", len(e.Tp.fanoutReqs))
	}
	if !second.GetDuplicated() || !second.GetAccepted() {
		t.Fatalf("重放必须复述首次结论 accepted+duplicated: %+v", second)
	}
	if second.GetDropReason() != rpc.DropReason_DROP_REASON_OK || second.GetFanoutNodes() != 2 || second.GetTargetedConnections() != 2 {
		t.Fatalf("重放的结论与首次不一致: %+v", second)
	}
	if second.GetMessageId() != "msg-idem" {
		t.Fatalf("重放要回显幂等键，实际 %q", second.GetMessageId())
	}
	// 去重在限流之前：上游重试风暴不能吃光正常用户的配额。
	if n := e.Leases.callCount("AllowRate"); n != ratesAfterFirst {
		t.Fatalf("重放不该再计限流，AllowRate 从 %d 变成 %d", ratesAfterFirst, n)
	}
	if len(e.Logs.all()) != 1 {
		t.Fatalf("重放不该再落审计行，实际 %d 行", len(e.Logs.all()))
	}
	// 契约缺口：结论编码只有 4 个数（broadcastcore.go:404-414），enqueued_at 在重放里丢了。
	if second.GetEnqueuedAt() != 0 {
		t.Fatalf("当前实现重放不回 enqueued_at（若此断言失败说明已修复，请同步交付报告）: %d", second.GetEnqueuedAt())
	}
}

func TestBroadcastToRoomConcurrentPendingIsAskedToRetry(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	lease := mustSeedSenderLease(t, e, "lgwl_pending", roomID, mid, model.RoleViewer)
	// 另一个请求刚抢键、结论还没回填：这时不能猜结论，只能让调用方按同一幂等键重试。
	e.Leases.seedMessage(roomID, "msg-pending", lgwOutcomePending)
	req := bcastReq("msg-pending", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req.SenderLeaseId = lease.LeaseID
	reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
	if err != nil {
		t.Fatalf("并发中不是错误: %v", err)
	}
	if reply.GetAccepted() || reply.GetDuplicated() {
		t.Fatalf("并发中既不能报成功也不能报重放: %+v", reply)
	}
	if reply.GetDropReason() != rpc.DropReason_DROP_REASON_DUPLICATED {
		t.Fatalf("并发中应回 DUPLICATED 让调用方重试，实际 %+v", reply)
	}
	assertNoFanout(t, e)
	if len(e.Logs.all()) != 0 {
		t.Fatalf("并发中不该落审计（首次请求会落）: %+v", e.Logs.all())
	}
}

func TestBroadcastToRoomUnparsableDedupOutcomeFailsLoud(t *testing.T) {
	for _, bad := range []string{"garbage", "2|1|1|1", ""} {
		t.Run("值="+bad, func(t *testing.T) {
			e := newTestEnv(t)
			seedRoute(e, roomID, model.RouteStateServing, nodeA)
			lease := mustSeedSenderLease(t, e, "lgwl_bad_outcome", roomID, mid, model.RoleViewer)
			e.Leases.seedMessage(roomID, "msg-bad", bad)
			req := bcastReq("msg-bad", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
			req.SenderLeaseId = lease.LeaseID
			// 空串与 "pending" 同义（broadcastcore.go:266 两者一并判「并发中」）：
			// 键存在就意味着有人抢到了，猜成首次请求会对同一 message_id 扇出两次。
			if bad == "" {
				reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
				if err != nil {
					t.Fatalf("并发中不是错误: %v", err)
				}
				if reply.GetDropReason() != rpc.DropReason_DROP_REASON_DUPLICATED || reply.GetAccepted() {
					t.Fatalf("空结论应按并发中回放: %+v", reply)
				}
				assertNoFanout(t, e)
				return
			}
			reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
			if err == nil {
				t.Fatalf("无法解析的结论必须报错而不是当首次请求重发，实际 reply=%+v", reply)
			}
			if reply != nil {
				t.Fatalf("出错时不得返回响应体: %+v", reply)
			}
			assertNoFanout(t, e)
		})
	}
}

func TestBroadcastToRoomDedupWindowOutageFallsBackToLedger(t *testing.T) {
	probe := errors.New("redis down")
	t.Run("账本里有首次结论则回放", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA, nodeB)
		lease := mustSeedSenderLease(t, e, "lgwl_fallback", roomID, mid, model.RoleViewer)
		e.Leases.failWith("ClaimMessage", probe)
		e.Logs.rows = append(e.Logs.rows, &model.LiveGwBroadcastLog{
			Id: 1, MessageId: "msg-ledger", RoomId: roomID, Kind: model.KindDanmaku,
			SenderMid: mid, SenderRole: model.RoleViewer, State: model.BroadcastLogSent,
			DropReason: model.DropOK, FanoutNodes: 2, TargetedConnections: 9, Ctime: testBaseTime + 5,
		})
		req := bcastReq("msg-ledger", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
		req.SenderLeaseId = lease.LeaseID
		reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
		if err != nil {
			t.Fatalf("Redis 抖动但有账本兜底时不该失败: %v", err)
		}
		if !reply.GetAccepted() || !reply.GetDuplicated() {
			t.Fatalf("应按账本回放首次结论: %+v", reply)
		}
		if reply.GetTargetedConnections() != 9 || reply.GetFanoutNodes() != 2 {
			t.Fatalf("回放要用账本里的真实数字，而不是本服务的猜测: %+v", reply)
		}
		if reply.GetEnqueuedAt() != testBaseTime+5 {
			t.Fatalf("账本回放能恢复 ctime 作为受理时刻，实际 %d", reply.GetEnqueuedAt())
		}
		assertNoFanout(t, e)
		if len(e.Logs.all()) != 1 {
			t.Fatalf("兜底回放不得再写审计: %+v", e.Logs.all())
		}
	})
	t.Run("账本也没有则原样失败", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		lease := mustSeedSenderLease(t, e, "lgwl_fallback2", roomID, mid, model.RoleViewer)
		e.Leases.failWith("ClaimMessage", probe)
		req := bcastReq("msg-never-sent", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
		req.SenderLeaseId = lease.LeaseID
		reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
		// Redis 不可用绝不当作「首次请求」放行：那会让每次重试各扇出一次。
		if !errors.Is(err, probe) {
			t.Fatalf("去重窗口与账本都问不到时必须失败，实际 err=%v reply=%+v", err, reply)
		}
		assertNoFanout(t, e)
	})
}

// TestBroadcastEventDedupFallbackLooksUpEventColumn 钉住兜底路径的回读列：
// Redis 去重窗口抖动时，DB 兜底必须按本次去重维度查账本。系统事件的幂等键是 event_id，
// 而审计行的 message_id 是渲染出来的 "evt:"+event_id（forwardsystemeventlogic.go:98），
// 兜底若固定查 message_id 列就永远落空，「开播/关播」这类关键状态会在抖动窗口里丢投递。
// 口径：事件路径查 FindByEvent，消息路径查 FindByMessage。
func TestBroadcastEventDedupFallbackLooksUpEventColumn(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	e.Leases.failWith("ClaimEvent", errors.New("redis down"))
	e.Logs.rows = append(e.Logs.rows, &model.LiveGwBroadcastLog{
		Id: 1, MessageId: "evt:evt-1", EventId: "evt-1", RoomId: roomID, Kind: model.KindRoomState,
		State: model.BroadcastLogSent, DropReason: model.DropOK, FanoutNodes: 1, TargetedConnections: 3,
	})
	reply, err := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc).ForwardSystemEvent(
		&rpc.ForwardSystemEventReq{RoomId: roomID, EventId: "evt-1", Kind: rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE, EventType: "room.open", SourceService: "moderation"})
	if err != nil {
		t.Fatalf("账本里已有首次结论，兜底必须回放而不是失败: %v", err)
	}
	if !reply.GetAccepted() || !reply.GetDuplicated() {
		t.Fatalf("期望 accepted+duplicated，实际 %+v", reply)
	}
	// 判别力来自这一条：回放结论也可能是「Redis 那次其实成功了」得到的，必须确认兜底真的查了事件列。
	if len(e.Logs.finds) != 1 || e.Logs.finds[0] != "FindByEvent:evt-1" {
		t.Fatalf("事件去重兜底必须且只查一次 FindByEvent(evt-1)，实际 %v", e.Logs.finds)
	}
	assertNoFanout(t, e)
	assertNoWrites(t, e)
}

// TestBroadcastMessageDedupFallbackLooksUpMessageColumn 是上一条的另一侧：
// 消息路径的兜底不许顺手改去查 event_id 列（弹幕行的 event_id 恒为空串，那样兜底同样永远落空）。
func TestBroadcastMessageDedupFallbackLooksUpMessageColumn(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	e.Leases.failWith("ClaimMessage", errors.New("redis down"))
	e.Logs.rows = append(e.Logs.rows, &model.LiveGwBroadcastLog{
		Id: 1, MessageId: "msg-fallback", RoomId: roomID, Kind: model.KindDanmaku, SenderMid: mid,
		SenderRole: model.RoleViewer, State: model.BroadcastLogSent, DropReason: model.DropOK,
		FanoutNodes: 1, TargetedConnections: 5,
	})
	req := bcastReq("msg-fallback", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req.SenderLeaseId = mustSeedSenderLease(t, e, "lgwl_fb_msg", roomID, mid, model.RoleViewer).LeaseID
	reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
	if err != nil {
		t.Fatalf("账本里已有首次结论，兜底必须回放而不是失败: %v", err)
	}
	if !reply.GetAccepted() || !reply.GetDuplicated() {
		t.Fatalf("期望 accepted+duplicated，实际 %+v", reply)
	}
	if len(e.Logs.finds) != 1 || e.Logs.finds[0] != "FindByMessage:msg-fallback" {
		t.Fatalf("消息去重兜底必须且只查一次 FindByMessage(msg-fallback)，实际 %v", e.Logs.finds)
	}
	assertNoFanout(t, e)
}

// --- 限流 ---

func TestBroadcastToRoomRateLimit(t *testing.T) {
	t.Run("房间层先判且用户层不计次", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID, BroadcastQps: 1, Version: 1})
		lease := mustSeedSenderLease(t, e, "lgwl_room_qps", roomID, mid, model.RoleViewer)
		logic := NewBroadcastToRoomLogic(ctxClient(t), e.Svc)

		req := bcastReq("msg-q1", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
		req.SenderLeaseId = lease.LeaseID
		if first, err := logic.BroadcastToRoom(req); err != nil || !first.GetAccepted() {
			t.Fatalf("第一条应被受理: reply=%+v err=%v", first, err)
		}
		req2 := bcastReq("msg-q2", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
		req2.SenderLeaseId = lease.LeaseID
		second, err := logic.BroadcastToRoom(req2)
		if err != nil {
			t.Fatalf("限流是结论不是错误: %v", err)
		}
		if second.GetAccepted() || second.GetDropReason() != rpc.DropReason_DROP_REASON_RATE_LIMITED {
			t.Fatalf("第二条应被房间层限流: %+v", second)
		}
		if second.GetRateRemaining() != 0 {
			t.Fatalf("超限时报剩余 0 才有退避意义，实际 %d", second.GetRateRemaining())
		}
		// 第一条已扇出、第二条被限流：总数仍是 1，这才是「被限流那次没投递」。
		assertFanoutOnce(t, e)
		if got := e.Leases.rateCount("bqps:room:7001"); got != 2 {
			t.Fatalf("房间层限流键 bqps:room:%d 应计 2 次（含被拒那次），实际 %d", roomID, got)
		}
		if got := e.Leases.rateCount("dqps:mid:9100"); got != 1 {
			t.Fatalf("房间层已拒时用户层不该再计次，实际 dqps:mid:%d=%d", mid, got)
		}
		row := e.Logs.all()[len(e.Logs.all())-1]
		if row.State != model.BroadcastLogDropped || row.DropReason != model.DropRateLimited {
			t.Fatalf("限流要落 DROPPED+RATE_LIMITED 行，实际 %+v", row)
		}
	})

	t.Run("priority=1 豁免限流但不豁免鉴权", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID, BroadcastQps: 1, Version: 1})
		// 配额已打满，但高优先级消息不走限流判定：AllowRate 一次都不该被调用。
		req := bcastReq("msg-prio", rpc.BroadcastKind_BROADCAST_KIND_SYSTEM)
		req.SenderRole = rpc.ConnRole_CONN_ROLE_OPERATOR
		req.Priority = 1
		reply, err := NewBroadcastToRoomLogic(ctxOperator(t, ""), e.Svc).BroadcastToRoom(req)
		if err != nil || !reply.GetAccepted() {
			t.Fatalf("高优先级归因消息不该被限流: reply=%+v err=%v", reply, err)
		}
		if n := e.Leases.callCount("AllowRate"); n != 0 {
			t.Fatalf("priority=1 不该触碰限流，AllowRate %d 次", n)
		}
		// 同一条消息换成未归因客户端 + 观众角色就被拒：豁免只给了限流，没给鉴权。
		req2 := bcastReq("msg-prio-2", rpc.BroadcastKind_BROADCAST_KIND_SYSTEM)
		req2.SenderRole = rpc.ConnRole_CONN_ROLE_VIEWER
		req2.Priority = 1
		reply2, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req2)
		if err != nil {
			t.Fatalf("不该返回错误: %v", err)
		}
		if reply2.GetDropReason() != rpc.DropReason_DROP_REASON_PERMISSION_DENIED {
			t.Fatalf("priority=1 不能越过权限矩阵: %+v", reply2)
		}
	})

	t.Run("配额 0 表示不限制", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		// 配置默认值也归零：Resolve 展开后 broadcast_qps=0，语义是「未设上限」。
		e.apply(func(c *config.LiveGatewayConf) {
			c.DefaultRoomBroadcastQps = 0
			c.DefaultUserBroadcastQps = 0
		})
		for i := 0; i < 3; i++ {
			req := bcastReq(fmt.Sprintf("msg-unlimited-%d", i), rpc.BroadcastKind_BROADCAST_KIND_SYSTEM)
			req.SenderRole = rpc.ConnRole_CONN_ROLE_OPERATOR
			if reply, err := NewBroadcastToRoomLogic(ctxOperator(t, ""), e.Svc).BroadcastToRoom(req); err != nil || !reply.GetAccepted() {
				t.Fatalf("上限为 0 时不该限流: reply=%+v err=%v", reply, err)
			}
		}
		if n := e.Leases.callCount("AllowRate"); n != 0 {
			t.Fatalf("不限流时不该调用 AllowRate（也不该留下计数键），实际 %d 次", n)
		}
	})

	t.Run("房间弹幕总量比用户层更严时以房间为准", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		// 用户层默认 100，房间层 danmaku_qps=1：全局放松的个体额度不能淹掉房间的弹幕总量约束。
		e.apply(func(c *config.LiveGatewayConf) { c.DefaultUserBroadcastQps = 100 })
		e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID, DanmakuQps: 1, Version: 1})
		lease := mustSeedSenderLease(t, e, "lgwl_stricter", roomID, mid, model.RoleViewer)
		logic := NewBroadcastToRoomLogic(ctxClient(t), e.Svc)
		for i, msg := range []string{"msg-s1", "msg-s2"} {
			req := bcastReq(msg, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
			req.SenderLeaseId = lease.LeaseID
			reply, err := logic.BroadcastToRoom(req)
			if err != nil {
				t.Fatalf("不该返回错误: %v", err)
			}
			if i == 0 && !reply.GetAccepted() {
				t.Fatalf("第一条弹幕应被受理: %+v", reply)
			}
			if i == 1 {
				if reply.GetDropReason() != rpc.DropReason_DROP_REASON_RATE_LIMITED {
					t.Fatalf("第二条应被房间层弹幕约束限住: %+v", reply)
				}
				// 被限流的那条不该投递：总数仍是第一条的那一次。
				assertFanoutOnce(t, e)
			}
		}
		if got := e.Leases.rateCount("dqps:mid:9100"); got != 2 {
			t.Fatalf("用户层限流键应按房间层更严的上限计次，实际 %d", got)
		}
	})

	t.Run("非用户态消息不占用户额度", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		req := bcastReq("msg-not-user-scoped", rpc.BroadcastKind_BROADCAST_KIND_SYSTEM)
		req.SenderRole = rpc.ConnRole_CONN_ROLE_SERVICE
		req.SenderMid = mid
		if _, err := NewBroadcastToRoomLogic(ctxService(t, "moderation"), e.Svc).BroadcastToRoom(req); err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		// UserScoped 只对弹幕/互动提示为 true：系统消息不该出现 dqps 键。
		if got := e.Leases.rateCount(fmt.Sprintf("dqps:mid:%d", mid)); got != 0 {
			t.Fatalf("系统消息不该计用户额度，实际 dqps:mid:%d=%d", mid, got)
		}
	})
}

// TestBroadcastRateLimitedDropMustBackfillDedupOutcome 钉住限流丢弃也回填幂等结论：
// 去重键在限流之前就已占位，不回填则 (room_id, message_id) 在 DedupWindowSeconds（默认 600 秒）
// 里一直是 "pending"，客户端按「并发中，请重试」的提示反复重试也拿不到真实结论，
// 一条弹幕被这条限流记录吃掉十分钟。
func TestBroadcastRateLimitedDropMustBackfillDedupOutcome(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID, BroadcastQps: 1, Version: 1})
	lease := mustSeedSenderLease(t, e, "lgwl_burn", roomID, mid, model.RoleViewer)
	logic := NewBroadcastToRoomLogic(ctxClient(t), e.Svc)

	req := bcastReq("msg-warm", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req.SenderLeaseId = lease.LeaseID
	if _, err := logic.BroadcastToRoom(req); err != nil {
		t.Fatalf("预热失败: %v", err)
	}
	req2 := bcastReq("msg-burned", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req2.SenderLeaseId = lease.LeaseID
	if r, err := logic.BroadcastToRoom(req2); err != nil || r.GetDropReason() != rpc.DropReason_DROP_REASON_RATE_LIMITED {
		t.Fatalf("第二条应限流: reply=%+v err=%v", r, err)
	}
	// 限流窗口已过（1 秒），同一条 message_id 重试应重新得到限流判定而非「并发中」。
	e.Clock.advance(2 * time.Second)
	req3 := bcastReq("msg-burned", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req3.SenderLeaseId = lease.LeaseID
	r3, err := logic.BroadcastToRoom(req3)
	if err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	if r3.GetDropReason() != rpc.DropReason_DROP_REASON_RATE_LIMITED {
		t.Fatalf("重试应重新判定限流，实际 %+v", r3)
	}
	// 结论来自幂等回放：必须如实标 duplicated，否则上游以为这是一次新判定。
	if !r3.GetDuplicated() {
		t.Fatalf("重试走的是幂等回放，必须标 duplicated: %+v", r3)
	}
	// 三条请求里只有预热那条该被投递：限流与其回放都不得扇出。
	assertFanoutOnce(t, e)
}

// --- 下发通道失败时的结论与审计 ---

func TestBroadcastToRoomTransportFailureConclusions(t *testing.T) {
	t.Run("未接线且 require_reliable=false 时是丢弃", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA, nodeB)
		e.markTransportUnwired()
		req := bcastReq("msg-unwired", rpc.BroadcastKind_BROADCAST_KIND_SYSTEM)
		req.SenderRole = rpc.ConnRole_CONN_ROLE_SERVICE
		reply, err := NewBroadcastToRoomLogic(ctxService(t, "moderation"), e.Svc).BroadcastToRoom(req)
		if err != nil {
			t.Fatalf("可丢弃语义不该返回错误: %v", err)
		}
		if reply.GetAccepted() || reply.GetDropReason() != rpc.DropReason_DROP_REASON_TRANSPORT_UNAVAILABLE {
			t.Fatalf("未接线必须显式回 TRANSPORT_UNAVAILABLE，不能报成功: %+v", reply)
		}
		if reply.GetFanoutNodes() != 2 {
			t.Fatalf("丢弃也要说明本应打到几个节点，实际 %d", reply.GetFanoutNodes())
		}
		row := onlyAudit(t, e)
		if row.State != model.BroadcastLogDropped || row.DropReason != model.DropTransportUnavailable {
			t.Fatalf("未接线丢弃要落 DROPPED+TRANSPORT_UNAVAILABLE，实际 %+v", row)
		}
	})

	t.Run("未接线且 require_reliable=true 时必须失败", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.markTransportUnwired()
		req := bcastReq("msg-reliable", rpc.BroadcastKind_BROADCAST_KIND_MODERATION)
		req.RequireReliable = true
		req.SenderRole = rpc.ConnRole_CONN_ROLE_OPERATOR
		reply, err := NewBroadcastToRoomLogic(ctxOperator(t, ""), e.Svc).BroadcastToRoom(req)
		if !errors.Is(err, model.ErrTransportUnavailable) {
			t.Fatalf("审核处置「静默丢失」等于处置未执行，必须让上游看见: err=%v reply=%+v", err, reply)
		}
		if reply != nil {
			t.Fatalf("返回错误时不得同时给响应体: %+v", reply)
		}
		// 失败也要留审计凭证，否则「发过但没送达」在追责时与「没发」等价。
		row := onlyAudit(t, e)
		if row.State != model.BroadcastLogDropped || row.DropReason != model.DropTransportUnavailable {
			t.Fatalf("reliable 失败也要落审计行，实际 %+v", row)
		}
	})

	t.Run("非未接线的通道故障不看 require_reliable", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.Tp.fanoutErr = errors.New("node gw-node-a unreachable")
		req := bcastReq("msg-node-err", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
		req.SenderLeaseId = mustSeedSenderLease(t, e, "lgwl_nodeerr", roomID, mid, model.RoleViewer).LeaseID
		// 契约上 require_reliable=false 表示「可丢弃」，但只有 ErrTransportUnavailable 走丢弃分支
		// （helpers.go:575-586），真实节点故障无论 reliable 与否都返回错误 —— 见交付报告。
		if _, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req); err == nil {
			t.Fatalf("当前实现对非未接线故障一律报错（若已改为按 reliable 分叉，请同步更新本用例）")
		}
	})

	t.Run("审计落库失败仍算受理", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.Logs.fail("Insert", errors.New("audit table unavailable"))
		req := bcastReq("msg-audit-fail", rpc.BroadcastKind_BROADCAST_KIND_SYSTEM)
		req.SenderRole = rpc.ConnRole_CONN_ROLE_SERVICE
		reply, err := NewBroadcastToRoomLogic(ctxService(t, "moderation"), e.Svc).BroadcastToRoom(req)
		// 消息确实发出去了，回滚不回来；这里只能继续报受理（Detail 里标 audit-write-failed 进日志）。
		if err != nil || !reply.GetAccepted() {
			t.Fatalf("reply=%+v err=%v", reply, err)
		}
		if len(e.Tp.fanoutReqs) != 1 {
			t.Fatalf("不该重复投递")
		}
		// 结论仍要回填幂等键：否则重试会二次扇出，比少一条审计糟糕得多。
		if v := e.Leases.msgs[fmt.Sprintf("%d|%s", roomID, "msg-audit-fail")]; v != "1|1|1|1" {
			t.Fatalf("审计失败也必须回填幂等结论，实际 %q", v)
		}
	})
}

func TestBroadcastToRoomZeroTargetedConnectionsIsStillSent(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	// 接入层回报「打到 0 个连接」（房间此刻没人在线）。
	e.Tp.result = &repository.FanoutResult{FanoutNodes: 1, TargetedConnections: 0}
	req := bcastReq("msg-nobody", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req.SenderLeaseId = mustSeedSenderLease(t, e, "lgwl_nobody", roomID, mid, model.RoleViewer).LeaseID
	reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
	if err != nil {
		t.Fatalf("房间没人不是错误: %v", err)
	}
	// 当前实现只对「定向投递」（LeaseIDs 非空）判 NO_SUBSCRIBER，房间扇出命中 0 连接仍落 SENT。
	// 调用方靠 target_connections=0 仍可区分，但 broadcasttoroomlogic.go:44 的说法与此不一致 —— 见交付报告。
	if !reply.GetAccepted() || reply.GetTargetedConnections() != 0 || reply.GetFanoutNodes() != 1 {
		t.Fatalf("期望 accepted+targeted=0: %+v", reply)
	}
	row := onlyAudit(t, e)
	if row.State != model.BroadcastLogSent || row.TargetedConnections != 0 {
		t.Fatalf("账本要如实记录「打到节点但 0 连接」，实际 %+v", row)
	}
}

// --- 目标过滤器 ---

func TestBroadcastToRoomTargetFiltersShape(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	lease := mustSeedSenderLease(t, e, "lgwl_filter", roomID, mid, model.RoleViewer)
	req := bcastReq("msg-filter", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req.SenderLeaseId = lease.LeaseID
	req.TargetRoles = []string{" viewer ", "viewer", "", "anchor", "\t"}
	req.TargetTopics = []string{"DANMAKU", "danmaku", "State"}
	reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
	if err != nil || !reply.GetAccepted() {
		t.Fatalf("reply=%+v err=%v", reply, err)
	}
	sent := e.Tp.fanoutReqs[0]
	// 角色过滤器：去空白 + 保序去重（空白项传给接入层会退化成「匹配不到任何连接」的静默丢消息）。
	if got := strings.Join(sent.Roles, ","); got != "viewer,anchor" {
		t.Fatalf("target_roles 应被 trim+去重成 viewer,anchor，实际 %q", got)
	}
	// 子通道过滤器：归一大小写与去重由 NormalizeTopics 负责。
	if got := strings.Join(sent.Topics, ","); got != "danmaku,state" {
		t.Fatalf("target_topics 应归一为 danmaku,state，实际 %q", got)
	}
}

func TestBroadcastToRoomEmptyTopicMeansAllSubscribed(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	lease := mustSeedSenderLease(t, e, "lgwl_alltopic", roomID, mid, model.RoleViewer)
	req := bcastReq("msg-all-topics", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req.SenderLeaseId = lease.LeaseID
	if _, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req); err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	// 空过滤器必须保持空：NormalizeTopics(nil) 会展开成固定默认三路，
	// 「发给全体」被写成「发给那几路」会让新增通道静默收不到消息。
	if got := e.Tp.fanoutReqs[0].Topics; got != nil {
		t.Fatalf("未指定子通道时下发请求的 Topics 必须是 nil，实际 %v", got)
	}
	if got := e.Tp.fanoutReqs[0].Roles; len(got) != 0 {
		t.Fatalf("未指定角色时 Roles 必须为空，实际 %v", got)
	}
}

// TestBroadcastTargetRolesMustRespectMaxTargetRoles 钉住配置项真的落地：
// config.LiveGatewayConf.MaxTargetRoles 注释写「越界直接拒，防把广播写成按角色枚举的放大器」，
// 但 role 列表此前原样交给下发通道。上限与子通道同性质，必须在参数阶段拒掉。
func TestBroadcastTargetRolesMustRespectMaxTargetRoles(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	lease := mustSeedSenderLease(t, e, "lgwl_roles", roomID, mid, model.RoleViewer)
	req := bcastReq("msg-roles-cap", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req.SenderLeaseId = lease.LeaseID
	req.TargetRoles = []string{"a", "b", "c", "d", "e", "f", "g"} // 配置上限 6
	_, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
	if !errors.Is(err, repository.ErrTooManyTargetRoles) {
		t.Fatalf("超过 MaxTargetRoles 的角色过滤器必须以哨兵错误拒掉，实际 %v", err)
	}
	// 参数阶段就得出结论：不能留下审计、去重键或半次扇出。
	assertNoFanout(t, e)
	assertNoWrites(t, e)
	if n := e.Leases.callCount("ClaimMessage"); n != 0 {
		t.Fatalf("越界的角色过滤器不该占掉幂等键，ClaimMessage %d 次", n)
	}

	// 边界：恰好等于上限放行（上限是「防放大器」，不是把该功能关掉）。
	e2 := newTestEnv(t)
	seedRoute(e2, roomID, model.RouteStateServing, nodeA)
	req2 := bcastReq("msg-roles-edge", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req2.SenderLeaseId = mustSeedSenderLease(t, e2, "lgwl_roles_edge", roomID, mid, model.RoleViewer).LeaseID
	req2.TargetRoles = []string{"a", "b", "c", "d", "e", "f"}
	reply, err := NewBroadcastToRoomLogic(ctxClient(t), e2.Svc).BroadcastToRoom(req2)
	if err != nil {
		t.Fatalf("恰好到上限不该被拒: %v", err)
	}
	if !reply.GetAccepted() {
		t.Fatalf("恰好到上限应受理: %+v", reply)
	}
	// 去重后的列表才参与计数：填 7 个重复项不该被当成越界（否则客户端无法用同一份常量表重试）。
	if got := e2.Tp.fanoutReqs[0].Roles; len(got) != 6 {
		t.Fatalf("下发请求应原样带上 6 个角色，实际 %v", got)
	}
}

// --- 跨房隔离 ---

func TestBroadcastToRoomCrossRoomIsolation(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	seedRoute(e, otherRoom, model.RouteStateServing, nodeB)
	leaseA := mustSeedSenderLease(t, e, "lgwl_roomA", roomID, mid, model.RoleViewer)
	leaseB := mustSeedSenderLease(t, e, "lgwl_roomB", otherRoom, mid, model.RoleViewer)
	logic := NewBroadcastToRoomLogic(ctxClient(t), e.Svc)

	// 同一个 message_id 在两个房间各投一次：幂等键是 (room_id, message_id)，不能互相吃掉。
	for _, tc := range []struct {
		room    int64
		leaseID string
		node    string
	}{
		{roomID, leaseA.LeaseID, nodeA},
		{otherRoom, leaseB.LeaseID, nodeB},
	} {
		req := &rpc.BroadcastToRoomReq{
			RoomId: tc.room, Kind: rpc.BroadcastKind_BROADCAST_KIND_DANMAKU, MessageId: "msg-shared",
			SenderMid: mid, SenderRole: rpc.ConnRole_CONN_ROLE_VIEWER, SenderLeaseId: tc.leaseID,
		}
		reply, err := logic.BroadcastToRoom(req)
		if err != nil || !reply.GetAccepted() {
			t.Fatalf("房间 %d 的广播应被受理: reply=%+v err=%v", tc.room, reply, err)
		}
	}
	if len(e.Tp.fanoutReqs) != 2 {
		t.Fatalf("两个房间各投一次，实际扇出 %d 次", len(e.Tp.fanoutReqs))
	}
	for i, sent := range e.Tp.fanoutReqs {
		wantRoom, wantNode := roomID, nodeA
		if i == 1 {
			wantRoom, wantNode = otherRoom, nodeB
		}
		if sent.RoomID != wantRoom || strings.Join(sent.Nodes, ",") != wantNode {
			t.Fatalf("第 %d 次下发串台了: room=%d nodes=%v", i, sent.RoomID, sent.Nodes)
		}
	}
	// 两间房各留一行审计，且 dedup 键互不相同。
	if len(e.Logs.all()) != 2 {
		t.Fatalf("两间房应各有 1 行审计: %+v", e.Logs.all())
	}
	for _, r := range []int64{roomID, otherRoom} {
		if v := e.Leases.msgs[fmt.Sprintf("%d|%s", r, "msg-shared")]; v != "1|1|1|1" {
			t.Fatalf("房间 %d 的幂等键未独立回填，实际 %q", r, v)
		}
	}
	// 反向：A 房的租约不能拿去发 B 房（三元组判定按请求里的 room_id）。
	req := &rpc.BroadcastToRoomReq{
		RoomId: otherRoom, Kind: rpc.BroadcastKind_BROADCAST_KIND_DANMAKU, MessageId: "msg-steal",
		SenderMid: mid, SenderRole: rpc.ConnRole_CONN_ROLE_VIEWER, SenderLeaseId: leaseA.LeaseID,
	}
	reply, err := logic.BroadcastToRoom(req)
	if err != nil {
		t.Fatalf("不该返回错误: %v", err)
	}
	if reply.GetDropReason() != rpc.DropReason_DROP_REASON_PERMISSION_DENIED {
		t.Fatalf("跨房用别人的房间凭据必须被拒: %+v", reply)
	}
	if len(e.Tp.fanoutReqs) != 2 {
		t.Fatalf("跨房越权不该触发第三次扇出")
	}
	// 限流键也按房间分开：B 房的额度不能被 A 房的高流量吃掉。
	if got := e.Leases.rateCount(fmt.Sprintf("bqps:room:%d", otherRoom)); got != 1 {
		t.Fatalf("房间层限流键必须按 room_id 分开，实际 bqps:room:%d=%d", otherRoom, got)
	}
}

// --- 依赖缺失 ---

func TestBroadcastToRoomWithoutStoreOrLeasesFailsLoud(t *testing.T) {
	t.Run("MySQL 未配置", func(t *testing.T) {
		e := newTestEnv(t)
		e.markStoreMissing()
		req := bcastReq("msg-nostore", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
		req.SenderLeaseId = mustSeedSenderLease(t, e, "lgwl_ns", roomID, mid, model.RoleViewer).LeaseID
		reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
		// 载荷上限可以降级（只影响这一条消息），但路由必须问得到：拿不到就不能编造结论。
		if !errors.Is(err, repository.ErrStoreUnavailable) {
			t.Fatalf("期望 ErrStoreUnavailable，实际 err=%v reply=%+v", err, reply)
		}
		assertNoFanout(t, e)
	})
	t.Run("Redis 未配置", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.Leases.failWith("Get", repository.ErrStoreUnavailable)
		req := bcastReq("msg-nolease", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
		req.SenderLeaseId = "lgwl_any"
		reply, err := NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(req)
		if !errors.Is(err, repository.ErrStoreUnavailable) {
			t.Fatalf("凭据存储不可用不得降级成「没这个租约」的 BAD_TICKET 结论: err=%v reply=%+v", err, reply)
		}
		assertNoFanout(t, e)
	})
}
