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

// 本文件覆盖 JoinRoom（订阅登记 + 房间路由登记）。
//
// JoinRoom 是本服务唯一会**写 live_gw_room_route** 的读多写少入口，也是「先有租约才能订阅」
// 这条防订阅注入约束的执行点，所以断言重点在三件事：
//  1. 全部只读门禁（凭据三元组 / conn-node 守卫 / topics 角色门禁 / 房间可进房 / 配额）
//     必须排在幂等占位与任何写入之前——非法请求既不能烧掉 request_id，也不能留下路由行；
//  2. 结论必须是真实原因（BAD_TICKET / PERMISSION_DENIED / ROOM_CLOSED / RATE_LIMITED），
//     绝不允许「joined=false 但没有任何原因」这种空成功；
//  3. 加入成功只写 Redis 订阅集合与一行路由投影：逐连接状态不落 MySQL（README 数据分层）。

// joinReq 一条最小可用的 Join 请求（凭据与 topics 由用例补齐）。
//
// topics 必须显式给：topics 留空会回落 repository.DefaultTopics()（danmaku/state/interaction），
// 而 state 对应 KindRoomState，RoleAllowedToSend 只允许 OPERATOR/SERVICE 发这一类，
// 于是**任何客户端角色的空 topics 请求都会在订阅门禁被拒**。这是实现与词表注释相冲突的
// 真实缺陷，已单独用 TestJoinRoomDefaultTopicSetIsUnusableByClientRoles 钉住并登记 README；
// 本文件其余用例走「接入层显式上报观众需要的两个通道」这条可用路径。
func joinReq(leaseID, requestID string) *rpc.JoinRoomReq {
	return &rpc.JoinRoomReq{
		LeaseId: leaseID, ConnId: "conn-" + leaseID, RoomId: roomID, Mid: mid,
		NodeId: nodeA, RequestId: requestID, Topics: []string{"danmaku", "interaction"},
	}
}

// joinLease 播种一条与 joinReq 同源的活租约（同 conn、同 node、同 (room, mid)）。
func joinLease(e *testEnv, leaseID string, user int64, role int32) *repository.LeaseRecord {
	return e.seedActiveLease(leaseID, "conn-"+leaseID, nodeA, roomID, user, role)
}

// --- 参数门禁 ---

func TestJoinRoomParameterGate(t *testing.T) {
	longID := strings.Repeat("q", 65)
	cases := []struct {
		name string
		mut  func(*rpc.JoinRoomReq)
		want error
	}{
		{"lease_id 为空", func(r *rpc.JoinRoomReq) { r.LeaseId = "  " }, model.ErrEmptyLeaseID},
		{"房间号非正", func(r *rpc.JoinRoomReq) { r.RoomId = 0 }, model.ErrInvalidRoomID},
		{"房间号为负", func(r *rpc.JoinRoomReq) { r.RoomId = -7 }, model.ErrInvalidRoomID},
		{"mid 为负", func(r *rpc.JoinRoomReq) { r.Mid = -1 }, model.ErrInvalidMid},
		{"node_id 为空", func(r *rpc.JoinRoomReq) { r.NodeId = "" }, model.ErrEmptyNodeID},
		{"node_id 含空格", func(r *rpc.JoinRoomReq) { r.NodeId = "gw node" }, model.ErrEmptyNodeID},
		{"request_id 为空", func(r *rpc.JoinRoomReq) { r.RequestId = "" }, model.ErrEmptyRequestID},
		{"request_id 超长", func(r *rpc.JoinRoomReq) { r.RequestId = longID }, model.ErrEmptyRequestID},
		{"conn_id 超长", func(r *rpc.JoinRoomReq) { r.ConnId = longID }, model.ErrEmptyConnID},
		{"conn_id 含空白", func(r *rpc.JoinRoomReq) { r.ConnId = "c 1" }, model.ErrEmptyConnID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			joinLease(e, "lgwl_join_param", mid, model.RoleViewer)
			req := joinReq("lgwl_join_param", "req-join-param")
			if tc.mut != nil {
				tc.mut(req)
			}
			got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("期望错误 %v，实际 %v", tc.want, err)
			}
			if got != nil {
				t.Fatalf("参数被拒时不得返回响应体，实际 %+v", got)
			}
			// 参数阶段不得触碰任何写入：幂等键、路由表、订阅集合、审计都必须是零。
			assertJoinNoWrite(t, e)
		})
	}
	// in == nil 单独走一条：上面那张表无法表达「整个请求体缺失」。
	e := newTestEnv(t)
	if got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(nil); !errors.Is(err, model.ErrEmptyLeaseID) || got != nil {
		t.Fatalf("空请求体必须报 ErrEmptyLeaseID，实际 err=%v reply=%+v", err, got)
	}
}

// assertJoinNoWrite 断言 Join 的一条拒出路径没有产生任何持久副作用。
func assertJoinNoWrite(t *testing.T, e *testEnv) {
	t.Helper()
	if n := e.Leases.callCount("ClaimRequest"); n != 0 {
		t.Fatalf("非法/被拒请求却占了幂等键，ClaimRequest 调用 %d 次", n)
	}
	if n := e.Leases.callCount("Subscribe"); n != 0 {
		t.Fatalf("非法/被拒请求却写了订阅集合，Subscribe 调用 %d 次", n)
	}
	if e.Routes.registers != 0 {
		t.Fatalf("非法/被拒请求却登记了路由，Register 调用 %d 次", e.Routes.registers)
	}
}

// --- 租约凭据 ---

func TestJoinRoomCredentialRejectionsCarryRealReason(t *testing.T) {
	cases := []struct {
		name      string
		prepare   func(t *testing.T, e *testEnv) string
		wantLease string
		wantDrop  int32
	}{
		{
			name: "租约不存在按无效凭据",
			prepare: func(_ *testing.T, _ *testEnv) string {
				return "lgwl_join_missing"
			},
			wantDrop: model.DropBadTicket,
		},
		{
			name: "三元组不符按越权",
			prepare: func(_ *testing.T, e *testEnv) string {
				joinLease(e, "lgwl_join_other_mid", otherMid, model.RoleViewer)
				return "lgwl_join_other_mid"
			},
			wantDrop: model.DropPermissionDenied,
		},
		{
			name: "终态租约按无效凭据",
			prepare: func(_ *testing.T, e *testEnv) string {
				rec := joinLease(e, "lgwl_join_released", mid, model.RoleViewer)
				rec.State = model.LeaseStateReleased
				return "lgwl_join_released"
			},
			wantDrop: model.DropBadTicket,
		},
		{
			name: "已过期租约按无效凭据",
			prepare: func(_ *testing.T, e *testEnv) string {
				rec := joinLease(e, "lgwl_join_expired", mid, model.RoleViewer)
				rec.ExpireAt = e.Clock.unix() - 1
				return "lgwl_join_expired"
			},
			wantDrop: model.DropBadTicket,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			leaseID := tc.prepare(t, e)
			got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(joinReq(leaseID, "req-join-cred"))
			if err != nil {
				t.Fatalf("凭据不通过是结论不是故障，不应返回 error: %v", err)
			}
			if got.GetJoined() {
				t.Fatalf("凭据不通过却宣布加入成功: %+v", got)
			}
			if got.GetDenyReason() != dropReason(tc.wantDrop) {
				t.Fatalf("拒绝原因应为 %d，实际 %v", tc.wantDrop, got.GetDenyReason())
			}
			assertJoinNoWrite(t, e)
			// 凭据/权限类拒出必须留痕（AGENTS.md §8）。
			row := onlyAudit(t, e)
			if row.State != model.BroadcastLogDenied || row.DropReason != tc.wantDrop {
				t.Fatalf("凭据拒出审计应为 DENIED/%d，实际 %d/%d", tc.wantDrop, row.State, row.DropReason)
			}
			if !strings.HasPrefix(row.MessageId, "join-denied:") {
				t.Fatalf("审计定位串应标明场景，实际 %q", row.MessageId)
			}
		})
	}
}

// Join 换房间必须被三元组门禁挡住。
//
// TODO(缺陷)：leaveroomlogic.go:55 的注释以「切房顺序是先 Join 新房间再 Leave 旧房间」为依据
// 解释为什么 LeaveRoom 不写 OfflineView，但 joinroomlogic.go:117 的 gate.usableLease →
// helpers.go:334 tripleMatch 要求 rec.RoomID == room_id，因此「拿现有租约 Join 另一个房间」
// 恒为 PERMISSION_DENIED，注释描述的那条流程在本服务里不可达（真实的切房路径是重新 Acquire，
// 见 fakes_test.go Acquire 的同连接重连语义）。属注释与实现的口径冲突，登记在 README。
func TestJoinRoomCannotSwitchToAnotherRoomWithSameLease(t *testing.T) {
	e := newTestEnv(t)
	joinLease(e, "lgwl_join_switch", mid, model.RoleViewer)
	req := joinReq("lgwl_join_switch", "req-join-switch")
	req.RoomId = otherRoom
	req.ConnId = "conn-lgwl_join_switch"
	got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(req)
	if err != nil {
		t.Fatalf("换房间应是拒出结论而不是 error: %v", err)
	}
	if got.GetJoined() || got.GetDenyReason() != dropReason(model.DropPermissionDenied) {
		t.Fatalf("换房间必须 PERMISSION_DENIED，实际 %+v", got)
	}
	// 另一间房的路由与订阅都没被这条请求碰过。
	if _, ok := e.Routes.rows[otherRoom]; ok {
		t.Fatalf("被拒的换房请求却为目标房间登记了路由")
	}
	assertJoinNoWrite(t, e)
}

// 三元组之外的第二道守卫：租约登记的 conn/node 才是连接身份，Join 不许改绑。
func TestJoinRoomConnAndNodeGuard(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*rpc.JoinRoomReq)
	}{
		{"conn_id 与租约不同", func(r *rpc.JoinRoomReq) { r.ConnId = "conn-hijacked" }},
		{"node_id 与租约不同", func(r *rpc.JoinRoomReq) { r.NodeId = nodeB }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			joinLease(e, "lgwl_join_guard", mid, model.RoleViewer)
			req := joinReq("lgwl_join_guard", "req-join-guard")
			tc.mut(req)
			got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(req)
			if err != nil {
				t.Fatalf("改绑应是越权结论而不是 error: %v", err)
			}
			if got.GetJoined() || got.GetDenyReason() != dropReason(model.DropPermissionDenied) {
				t.Fatalf("越权改绑被当成成功: %+v", got)
			}
			assertJoinNoWrite(t, e)
			// 路由行不能被攻击者选择的节点污染——这条是本用例的全部意义。
			if _, ok := e.Routes.rows[roomID]; ok {
				t.Fatalf("越权 Join 写出了房间路由")
			}
			if len(e.Logs.all()) != 1 {
				t.Fatalf("越权 Join 应留一行 DENIED 审计，实际 %+v", e.Logs.all())
			}
		})
	}
	// 判别性对照：conn_id 留空（接入层没上报）时不该被同一条守卫误杀。
	e := newTestEnv(t)
	joinLease(e, "lgwl_join_noconn", mid, model.RoleViewer)
	req := joinReq("lgwl_join_noconn", "req-join-noconn")
	req.ConnId = ""
	if got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(req); err != nil || !got.GetJoined() {
		t.Fatalf("conn_id 为空应按租约自身放行，实际 err=%v reply=%+v", err, got)
	}
}

// --- topics：词表门禁与角色门禁 ---

func TestJoinRoomTopicGate(t *testing.T) {
	t.Run("未知子通道报错而不是静默忽略", func(t *testing.T) {
		e := newTestEnv(t)
		joinLease(e, "lgwl_join_topic", mid, model.RoleViewer)
		req := joinReq("lgwl_join_topic", "req-join-topic")
		req.Topics = []string{"danmaku", "dmakku"}
		got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(req)
		if !errors.Is(err, repository.ErrUnknownTopic) || got != nil {
			t.Fatalf("拼错通道名必须报 ErrUnknownTopic，实际 err=%v reply=%+v", err, got)
		}
		if !strings.Contains(err.Error(), "dmakku") {
			t.Fatalf("错误文本必须指名是哪一个通道，实际 %v", err)
		}
		assertJoinNoWrite(t, e)
	})

	t.Run("通道数量越界被拒", func(t *testing.T) {
		e := newTestEnv(t)
		joinLease(e, "lgwl_join_many", mid, model.RoleViewer)
		req := joinReq("lgwl_join_many", "req-join-many")
		req.Topics = []string{"danmaku", "state", "interaction", "system", "moderation", "anchor_tip", "a", "b", "c"}
		if _, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(req); !errors.Is(err, repository.ErrTooManyTopics) {
			t.Fatalf("超量通道应报 ErrTooManyTopics，实际 %v", err)
		}
		assertJoinNoWrite(t, e)
	})

	// 订阅门禁的依据是「谁能发这类消息」：观众订 moderation 等于免费拿处置情报流。
	t.Run("观众订阅处置通道按越权拒出", func(t *testing.T) {
		e := newTestEnv(t)
		joinLease(e, "lgwl_join_role", mid, model.RoleViewer)
		req := joinReq("lgwl_join_role", "req-join-role")
		req.Topics = []string{"danmaku", "moderation"}
		got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(req)
		if err != nil {
			t.Fatalf("订阅越权应是拒出结论而不是 error: %v", err)
		}
		if got.GetJoined() || got.GetDenyReason() != dropReason(model.DropPermissionDenied) {
			t.Fatalf("观众订 moderation 被放行: %+v", got)
		}
		assertJoinNoWrite(t, e)
		row := onlyAudit(t, e)
		if !strings.HasPrefix(row.MessageId, "join-topic-denied:") {
			t.Fatalf("订阅越权审计应标明 topic 场景，实际 %q", row.MessageId)
		}
	})

	// 判别性对照：同一份 topics 换成主播态租约必须放行，证明拒出来自角色而不是通道词表。
	t.Run("主播订阅提词通道放行", func(t *testing.T) {
		e := newTestEnv(t)
		joinLease(e, "lgwl_join_anchor", anchorMid, model.RoleAnchor)
		req := joinReq("lgwl_join_anchor", "req-join-anchor")
		req.Mid = anchorMid
		req.Topics = []string{"anchor_tip"}
		got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(req)
		if err != nil || !got.GetJoined() {
			t.Fatalf("主播订 anchor_tip 应放行，实际 err=%v reply=%+v", err, got)
		}
		if strings.Join(got.GetSubscribedTopics(), ",") != "anchor_tip" {
			t.Fatalf("subscribed_topics 应原样回显归一结果，实际 %v", got.GetSubscribedTopics())
		}
	})
}

// TODO(缺陷)：topics 留空（接入层不上报）时回落 repository.DefaultTopics()，
// 而默认集含 state（model.KindRoomState），RoleAllowedToSend 只放行 OPERATOR/SERVICE
// （model/errors.go:266-286），因此三种客户端角色都无法用默认集进房——
// repository/topics.go:23 的注释「默认全集（观众态连接实际需要的三类）」与门禁互相矛盾。
// 后果：客户端不显式上报 topics 就永久进不了房（PERMISSION_DENIED），
// 而 proto 里 topics 是可选字段。属实现缺陷，不改生产代码，登记在 README。
func TestJoinRoomDefaultTopicSetIsUnusableByClientRoles(t *testing.T) {
	for _, role := range []int32{model.RoleViewer, model.RoleAnchor, model.RoleRoomAdmin} {
		e := newTestEnv(t)
		user := mid
		if role == model.RoleAnchor {
			user = anchorMid
		}
		joinLease(e, "lgwl_join_default", user, role)
		req := joinReq("lgwl_join_default", "req-join-default")
		req.Mid = user
		req.Topics = nil // 交给实现回落默认全集
		got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(req)
		if err != nil {
			t.Fatalf("role=%d 前提不成立（应为拒出结论）: %v", role, err)
		}
		if got.GetJoined() || got.GetDenyReason() != dropReason(model.DropPermissionDenied) {
			t.Fatalf("role=%d 用默认 topics 竟然加入了：%+v（本用例的前提是它被拒）", role, got)
		}
		assertJoinNoWrite(t, e)
	}
	// 判别性对照：同一份默认集换成 OPERATOR 态租约必须放行，证明拒出来自角色矩阵而不是词表本身。
	e := newTestEnv(t)
	opLease := e.seedActiveLease("lgwl_join_default_op", "conn-lgwl_join_default_op", nodeA, roomID, mid, model.RoleOperator)
	req := joinReq("lgwl_join_default_op", "req-join-default-op")
	req.Topics = nil
	got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(req)
	if err != nil || !got.GetJoined() {
		t.Fatalf("OPERATOR 用默认集应放行，实际 err=%v reply=%+v", err, got)
	}
	if strings.Join(got.GetSubscribedTopics(), ",") != strings.Join(repository.DefaultTopics(), ",") {
		t.Fatalf("默认集回显应为 %v，实际 %v", repository.DefaultTopics(), got.GetSubscribedTopics())
	}
	if opLease.JoinedAt == 0 {
		t.Fatalf("前提不成立：OPERATOR 这条应已写入 joined_at")
	}
}

// --- 房间可进房（live-room 判定权） ---

func TestJoinRoomRoomGate(t *testing.T) {
	t.Run("live-room 未接线不得当成可加入", func(t *testing.T) {
		e := newTestEnv(t)
		joinLease(e, "lgwl_join_unwired", mid, model.RoleViewer)
		e.markRoomGateUnwired()
		got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(joinReq("lgwl_join_unwired", "req-join-unwired"))
		if !errors.Is(err, repository.ErrLiveRoomNotConfigured) || got != nil {
			t.Fatalf("未接线必须显式失败，实际 err=%v reply=%+v", err, got)
		}
		assertJoinNoWrite(t, e)
	})

	t.Run("房间真结论为不可广播时按 ROOM_CLOSED 拒出", func(t *testing.T) {
		e := newTestEnv(t)
		joinLease(e, "lgwl_join_closed", mid, model.RoleViewer)
		e.Rooms.broadcastable = false
		e.Rooms.reason = "ROOM_FINISHED"
		got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(joinReq("lgwl_join_closed", "req-join-closed"))
		if err != nil {
			t.Fatalf("房间已关闭是可解释结论，不应返回 error: %v", err)
		}
		if got.GetJoined() || got.GetDenyReason() != dropReason(model.DropRoomClosed) {
			t.Fatalf("应回 ROOM_CLOSED，实际 %+v", got)
		}
		assertJoinNoWrite(t, e)
		// 房间不可进房不是越权：README 明确只有凭据/权限类拒出落审计。
		if len(e.Logs.all()) != 0 {
			t.Fatalf("房间关闭不该落 DENIED 审计: %+v", e.Logs.all())
		}
	})

	t.Run("房间不存在是真结论", func(t *testing.T) {
		e := newTestEnv(t)
		joinLease(e, "lgwl_join_nofound", mid, model.RoleViewer)
		e.Rooms.broadcastErr = model.ErrInvalidRoomID
		got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(joinReq("lgwl_join_nofound", "req-join-nofound"))
		if err != nil {
			t.Fatalf("live-room 明确回答房间不存在时应翻译成 ROOM_CLOSED，实际 %v", err)
		}
		if got.GetJoined() || got.GetDenyReason() != dropReason(model.DropRoomClosed) {
			t.Fatalf("应回 ROOM_CLOSED，实际 %+v", got)
		}
	})

	t.Run("live-room 故障原样上抛", func(t *testing.T) {
		e := newTestEnv(t)
		joinLease(e, "lgwl_join_gateerr", mid, model.RoleViewer)
		e.Rooms.broadcastErr = errRoomGateProbe
		got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(joinReq("lgwl_join_gateerr", "req-join-gateerr"))
		if !errors.Is(err, errRoomGateProbe) || got != nil {
			t.Fatalf("门禁故障必须原样返回，不能降级成结论，实际 err=%v reply=%+v", err, got)
		}
		assertJoinNoWrite(t, e)
	})
}

// errRoomGateProbe 区分「live-room 报错了」与「logic 自己造了个结论」。
var errRoomGateProbe = errors.New("live-room-probe")

// --- 配额 ---

func TestJoinRoomQuotaGate(t *testing.T) {
	t.Run("游客在 allow_guest 关闭的房间被拒", func(t *testing.T) {
		e := newTestEnv(t)
		e.apply(func(c *config.LiveGatewayConf) { c.AllowGuestByDefault = false })
		rec := e.seedActiveLease("lgwl_join_guest", "conn-lgwl_join_guest", nodeA, roomID, 0, model.RoleViewer)
		if rec.Mid != 0 {
			t.Fatalf("前提不成立：游客租约 mid 应为 0，实际 %d", rec.Mid)
		}
		req := joinReq("lgwl_join_guest", "req-join-guest")
		req.Mid = 0
		req.ConnId = "conn-lgwl_join_guest"
		got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(req)
		if err != nil {
			t.Fatalf("游客接入被配额拒是结论不是故障: %v", err)
		}
		if got.GetJoined() || got.GetDenyReason() != dropReason(model.DropPermissionDenied) {
			t.Fatalf("allow_guest=false 时游客仍被放行: %+v", got)
		}
		assertJoinNoWrite(t, e)
		row := onlyAudit(t, e)
		if !strings.HasPrefix(row.MessageId, "join-guest-denied:") {
			t.Fatalf("游客拒出审计应标明场景，实际 %q", row.MessageId)
		}

		// 判别性对照：同一套数据，只放开 allow_guest 就必须加入成功。
		e2 := newTestEnv(t)
		e2.seedActiveLease("lgwl_join_guest_ok", "conn-lgwl_join_guest_ok", nodeA, roomID, 0, model.RoleViewer)
		req2 := joinReq("lgwl_join_guest_ok", "req-join-guest-ok")
		req2.Mid = 0
		req2.ConnId = "conn-lgwl_join_guest_ok"
		if got2, err2 := NewJoinRoomLogic(ctxClient(t), e2.Svc).JoinRoom(req2); err2 != nil || !got2.GetJoined() {
			t.Fatalf("对照用例（默认允许游客）应加入成功，实际 err=%v reply=%+v", err2, got2)
		}
	})

	t.Run("房间连接数超限按 RATE_LIMITED 拒出", func(t *testing.T) {
		e := newTestEnv(t)
		e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID, MaxConnections: 1})
		joinLease(e, "lgwl_join_full", mid, model.RoleViewer)
		e.seedActiveLease("lgwl_join_extra", "conn-extra", nodeA, roomID, otherMid, model.RoleViewer)
		got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(joinReq("lgwl_join_full", "req-join-full"))
		if err != nil {
			t.Fatalf("房间满是可解释结论: %v", err)
		}
		if got.GetJoined() || got.GetDenyReason() != dropReason(model.DropRateLimited) {
			t.Fatalf("应回 RATE_LIMITED，实际 %+v", got)
		}
		assertJoinNoWrite(t, e)
	})

	t.Run("计数取不到时不得按 0 放行", func(t *testing.T) {
		e := newTestEnv(t)
		joinLease(e, "lgwl_join_cntfail", mid, model.RoleViewer)
		e.Leases.failWith("RoomConnectionCount", errCountProbe)
		got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(joinReq("lgwl_join_cntfail", "req-join-cntfail"))
		if !errors.Is(err, errCountProbe) || got != nil {
			t.Fatalf("读数故障必须上抛（否则 Redis 抖动 = 无限接入），实际 err=%v reply=%+v", err, got)
		}
		if !strings.Contains(err.Error(), "配额判定") {
			t.Fatalf("错误必须说明是配额判定受阻，实际 %v", err)
		}
		assertJoinNoWrite(t, e)
	})

	t.Run("MySQL 未配置时不得退回默认配额", func(t *testing.T) {
		e := newTestEnv(t)
		joinLease(e, "lgwl_join_nodb", mid, model.RoleViewer)
		e.markStoreMissing()
		if _, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(joinReq("lgwl_join_nodb", "req-join-nodb")); !errors.Is(err, repository.ErrStoreUnavailable) {
			t.Fatalf("DataSource 缺失必须显式失败，实际 %v", err)
		}
		assertJoinNoWrite(t, e)
	})
}

// errCountProbe 连接数读数探针。
var errCountProbe = errors.New("count-read-probe")

// --- 幂等窗口 ---

func TestJoinRoomIdempotentReplayIsReadOnly(t *testing.T) {
	e := newTestEnv(t)
	joinLease(e, "lgwl_join_idem", mid, model.RoleViewer)
	logic := NewJoinRoomLogic(ctxClient(t), e.Svc)
	first, err := logic.JoinRoom(joinReq("lgwl_join_idem", "req-join-idem"))
	if err != nil || !first.GetJoined() {
		t.Fatalf("首次 Join 失败: err=%v reply=%+v", err, first)
	}
	registersAfterFirst, subsAfterFirst := e.Routes.registers, e.Leases.callCount("Subscribe")
	if first.GetJoinedAt() != e.Clock.unix() {
		t.Fatalf("前提不成立：首次 joined_at 应为当前时刻，实际 %d", first.GetJoinedAt())
	}

	// 推进时钟：若重放走了写入路径，joined_at 会跟着变（Subscribe 不覆盖既有 joined_at 才使回显成立）。
	e.Clock.advance(5 * time.Second)

	second, err := logic.JoinRoom(joinReq("lgwl_join_idem", "req-join-idem"))
	if err != nil {
		t.Fatalf("幂等重放不应失败: %v", err)
	}
	if !second.GetJoined() {
		t.Fatalf("重放必须回显已加入: %+v", second)
	}
	if second.GetJoinedAt() != first.GetJoinedAt() {
		t.Fatalf("重放回显的 joined_at 必须是首次值 %d，实际 %d", first.GetJoinedAt(), second.GetJoinedAt())
	}
	if strings.Join(second.GetSubscribedTopics(), ",") != strings.Join(first.GetSubscribedTopics(), ",") {
		t.Fatalf("重放必须原样回显 subscribed_topics: %v vs %v", first.GetSubscribedTopics(), second.GetSubscribedTopics())
	}
	if e.Routes.registers != registersAfterFirst {
		t.Fatalf("重放重复登记路由：Register 从 %d 次变成 %d 次", registersAfterFirst, e.Routes.registers)
	}
	if e.Leases.callCount("Subscribe") != subsAfterFirst {
		t.Fatalf("重放重复写订阅：Subscribe 从 %d 次变成 %d 次", subsAfterFirst, e.Leases.callCount("Subscribe"))
	}
	if len(e.Logs.all()) != 0 {
		t.Fatalf("重放不该产生任何审计行: %+v", e.Logs.all())
	}
}

func TestJoinRoomReplayWithPendingConclusionRedoesTheWrite(t *testing.T) {
	e := newTestEnv(t)
	// 首次请求崩在订阅写入之前：窗口里还是 pending，租约 joined_at 仍为 0。
	joinLease(e, "lgwl_join_pending", mid, model.RoleViewer)
	e.Leases.seedRequest(joinRPCName, "req-join-pending", lgwOutcomePending)
	got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(joinReq("lgwl_join_pending", "req-join-pending"))
	if err != nil {
		t.Fatalf("pending 窗口应按未受理重跑: %v", err)
	}
	if !got.GetJoined() {
		t.Fatalf("重跑后必须真的加入: %+v", got)
	}
	if e.Routes.registers != 1 || e.Leases.callCount("Subscribe") != 1 {
		t.Fatalf("重跑应各写一次，实际 register=%d subscribe=%d", e.Routes.registers, e.Leases.callCount("Subscribe"))
	}
	if got.GetJoinedAt() != e.Clock.unix() {
		t.Fatalf("重跑写入的 joined_at 应为当前时刻，实际 %d", got.GetJoinedAt())
	}
}

func TestJoinRoomClaimRequestFailurePropagates(t *testing.T) {
	e := newTestEnv(t)
	joinLease(e, "lgwl_join_claim", mid, model.RoleViewer)
	e.Leases.failWith("ClaimRequest", errClaimProbe)
	if _, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(joinReq("lgwl_join_claim", "req-join-claim")); !errors.Is(err, errClaimProbe) {
		t.Fatalf("Redis 幂等窗口故障不得当成首次请求放行，实际 %v", err)
	}
	if e.Routes.registers != 0 {
		t.Fatalf("幂等窗口不可用时不该已经写路由")
	}
}

// errClaimProbe 幂等占位探针。
var errClaimProbe = errors.New("claim-probe")

// --- 路由登记与订阅写入 ---

func TestJoinRoomHappyPathWritesOnlyRouteAndRedis(t *testing.T) {
	e := newTestEnv(t)
	rec := joinLease(e, "lgwl_join_ok", mid, model.RoleViewer)
	before := rec.ExpireAt
	e.seedActiveLease("lgwl_join_peer", "conn-peer", nodeA, roomID, otherMid, model.RoleViewer)

	got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(joinReq("lgwl_join_ok", "req-join-ok"))
	if err != nil {
		t.Fatalf("合法 Join 失败: %v", err)
	}
	if !got.GetJoined() || got.GetDenyReason() != dropReason(model.DropOK) {
		t.Fatalf("应成功加入且 deny_reason=OK，实际 %+v", got)
	}
	if got.GetJoinedAt() != e.Clock.unix() {
		t.Fatalf("joined_at 应为服务端受理时刻 %d，实际 %d", e.Clock.unix(), got.GetJoinedAt())
	}
	if got.GetRoomConnectionCount() != 2 {
		t.Fatalf("room_connection_count 应为 Redis 集合读数 2，实际 %d", got.GetRoomConnectionCount())
	}
	if got.GetRouteState() != rpc.RouteState_ROUTE_STATE_SERVING {
		t.Fatalf("刚登记的路由应为 SERVING，实际 %v", got.GetRouteState())
	}
	if want := "danmaku,interaction"; strings.Join(got.GetSubscribedTopics(), ",") != want {
		t.Fatalf("subscribed_topics 应回显归一后的订阅集 %q，实际 %v", want, got.GetSubscribedTopics())
	}

	// 路由行的内容就是本服务对「这个房间由谁承接」的唯一投影。
	row := e.Routes.rows[roomID]
	if row == nil {
		t.Fatalf("JoinRoom 必须登记 live_gw_room_route")
	}
	if row.PrimaryNode != nodeA || row.State != model.RouteStateServing || row.Version != 1 {
		t.Fatalf("路由行形状不符: %+v", row)
	}
	if row.ShardCount != 1 {
		t.Fatalf("shard_count 应取 DefaultRoomShardCount=1，实际 %d", row.ShardCount)
	}
	if row.UpdatedBy != joinRouteWriter {
		t.Fatalf("updated_by 必须是固定主体 %q，实际 %q", joinRouteWriter, row.UpdatedBy)
	}
	if row.ReplicaNodes != "[]" {
		t.Fatalf("replica_nodes 空集合必须写 \"[]\"，实际 %q", row.ReplicaNodes)
	}

	// 订阅只写 Redis：Join 不续租，ExpireAt 不得被改动。
	after, gerr := e.Leases.Get(ctxClient(t), "lgwl_join_ok")
	if gerr != nil {
		t.Fatalf("回读租约失败: %v", gerr)
	}
	if after.ExpireAt != before {
		t.Fatalf("Join 借订阅续租了（%d → %d），这条通道会绕过 MaxLeaseTTLSeconds", before, after.ExpireAt)
	}
	if len(after.Topics) != 2 || after.JoinedAt == 0 {
		t.Fatalf("订阅结果未落进租约: %+v", after)
	}

	// 逐连接状态不落 MySQL：成功路径零审计行、零票据写入。
	if len(e.Logs.all()) != 0 {
		t.Fatalf("加入成功不该写审计表（README 数据分层）: %+v", e.Logs.all())
	}
	// 写路径必须主动失效路由缓存，否则广播还会打到旧节点一个缓存周期。
	if n := e.Leases.callCount("CacheDel"); n != 1 {
		t.Fatalf("登记路由后应恰好失效一次路由缓存，实际 %d 次", n)
	}
	if n := e.Leases.callCount("RememberRequest"); n != 1 {
		t.Fatalf("应回填一次幂等结论，实际 %d 次", n)
	}
}

func TestJoinRoomDrainingRouteRefusesToReviveNode(t *testing.T) {
	e := newTestEnv(t)
	joinLease(e, "lgwl_join_drain", mid, model.RoleViewer)
	seedRoute(e, roomID, model.RouteStateDraining, nodeA)
	got, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(joinReq("lgwl_join_drain", "req-join-drain"))
	if err == nil || got != nil {
		t.Fatalf("排空中的路由必须显式失败，实际 err=%v reply=%+v", err, got)
	}
	if !errors.Is(err, model.ErrRouteDraining) {
		t.Fatalf("应包装 ErrRouteDraining，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "排空") {
		t.Fatalf("错误要给出可执行结论（引到别的节点），实际 %v", err)
	}
	if e.Leases.callCount("Subscribe") != 0 {
		t.Fatalf("路由拒绝后还写了订阅集合")
	}
	// 排空状态不许被 Join 复活成 SERVING。
	if e.Routes.rows[roomID].State != model.RouteStateDraining {
		t.Fatalf("Join 把 DRAINING 路由复活了")
	}
}

// 订阅集合写入失败必须原样上报：路由已登记但成员集合缺人，是本方法唯一
// 不回滚的既成事实，因此绝不能把错误吞成 joined=true。
func TestJoinRoomSubscribeFailurePropagates(t *testing.T) {
	e := newTestEnv(t)
	joinLease(e, "lgwl_join_subfail", mid, model.RoleViewer)
	e.Leases.failWith("Subscribe", errSubscribeProbe)
	if _, err := NewJoinRoomLogic(ctxClient(t), e.Svc).JoinRoom(joinReq("lgwl_join_subfail", "req-join-subfail")); !errors.Is(err, errSubscribeProbe) {
		t.Fatalf("订阅写入故障必须原样返回，实际 %v", err)
	}
	// 路由已登记是既成事实：本方法不回滚，但也不得回显 joined=true。
	if e.Routes.registers != 1 {
		t.Fatalf("前提不成立：Register 应先于 Subscribe 发生")
	}
	if n := e.Leases.callCount("RememberRequest"); n != 0 {
		t.Fatalf("失败的 Join 不该回填幂等结论（否则重放会拿到假的 joined_at），实际 %d 次", n)
	}
}

// errSubscribeProbe 订阅写入探针。
var errSubscribeProbe = errors.New("subscribe-probe")
