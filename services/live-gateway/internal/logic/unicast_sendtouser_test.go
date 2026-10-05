package logic

// unicast_sendtouser_test.go 覆盖 SendToUser（定向单播到「某个用户在本房间的在线连接」）。
//
// 这条链与 BroadcastToRoom 共用 g.runBroadcast，但它在鉴权**之前**多做一次目标定位读
// （sendtouserlogic.go:74），并在「目标不在线」时直接终局返回（:88-98）。
// 因此本文件的断言重点有三层：
//   1. 参数门禁必须在任何存储读之前（用 callCount 钉住「一步都没走」）；
//   2. 目标集合只算 ACTIVE 租约，且必须走精确 Deliver 而不是整房 Fanout；
//   3. 「无连接是结论不是错误」与「越权必须留 DENIED 审计」这两条承诺的**交界处**：
//      离线短路发生在鉴权之前，所以同一请求在目标在线/离线时给出不同结论 ——
//      TestSendToUserOfflineShortCircuitSkipsAuthorisation 把这条不对称钉成事实（README 已知缺口）。
//
// 幂等键的口径也在本文件钉死：实现按 (room_id, message_id) 去重，而 proto:443 写的是
// (room, target, message_id)。用例证明**现状**是前者，因此同 message_id 打第二个目标会静默丢失。

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"
)

// --- 构造与断言助手（本文件专用，uni 前缀避免与其他文件的同名量混淆） ---

// uniEQ 比较两个可比较值：值不符直接 Fatal，因为本文件的每条断言都是结论级的。
func uniEQ[T comparable](t *testing.T, label string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %#v, want %#v", label, got, want)
	}
}

// uniNoErr 断言没有错误：本服务里「对方不在线」「越权」都是结论而不是 error，
// 用 err 表达它们会让调用方把正常结果当成故障重试。
func uniNoErr(t *testing.T, label string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s 不该返回错误: %v", label, err)
	}
}

func uniItoa(n int64) string { return strconv.FormatInt(n, 10) }

// uniReq 一条最小可用的单播请求（发送者凭据由用例补齐）。
func uniReq(messageID string, target int64, kind rpc.BroadcastKind) *rpc.SendToUserReq {
	return &rpc.SendToUserReq{
		RoomId: roomID, TargetMid: target, Kind: kind, MessageId: messageID,
		SenderMid: mid, SenderRole: rpc.ConnRole_CONN_ROLE_VIEWER,
	}
}

func sendUni(t *testing.T, e *testEnv, req *rpc.SendToUserReq) (*rpc.SendToUserReply, error) {
	t.Helper()
	return NewSendToUserLogic(ctxClient(t), e.Svc).SendToUser(req)
}

// seedTargetLease 播一条目标侧租约：ttl 为相对当前时钟的存活秒数，state=0 时按 ACTIVE 入库。
// 目标定位只看 EffectiveState（sendtouserlogic.go:83），所以「过期」「被踢」都必须用这条造，
// 而不是让用例依赖 fake 的默认排序。
func seedTargetLease(e *testEnv, leaseID string, user, ttl int64, state int32) *repository.LeaseRecord {
	rec := &repository.LeaseRecord{
		LeaseID: leaseID, ConnID: "conn-" + leaseID, NodeID: nodeA,
		RoomID: roomID, Mid: user, Role: model.RoleViewer, State: state,
		IssuedAt: e.Clock.unix(), ExpireAt: e.Clock.unix() + ttl,
	}
	if rec.State == 0 {
		rec.State = model.LeaseStateActive
	}
	return e.Leases.seedLease(rec)
}

// assertDeliverOnly 断言单播只走精确投递（wantCalls 次）、一次都没有整房扇出。
// Fanout 与 Deliver 是两个不同的通道方法：单播落到 Fanout 等于把定向消息广播给全房间。
func assertDeliverOnly(t *testing.T, e *testEnv, wantCalls int) {
	t.Helper()
	if n := len(e.Tp.fanoutReqs); n != 0 {
		t.Fatalf("单播绝不能走整房扇出，实际 Fanout %d 次", n)
	}
	if n := len(e.Tp.deliverReqs); n != wantCalls {
		t.Fatalf("Deliver 调用次数 = %d, want %d", n, wantCalls)
	}
}

// deliverLeaseIDs 返回第 i 次 Deliver 的投递目标（listLocked 按 lease_id 升序，故顺序可断言）。
func deliverLeaseIDs(t *testing.T, e *testEnv, i int) []string {
	t.Helper()
	if len(e.Tp.deliverArgs) <= i {
		t.Fatalf("没有第 %d 次 Deliver 调用（共 %d 次）", i+1, len(e.Tp.deliverArgs))
	}
	return e.Tp.deliverArgs[i]
}

// assertNoStorageCalls 断言这些存储方法一次都没被调用过。
func assertNoStorageCalls(t *testing.T, e *testEnv, methods ...string) {
	t.Helper()
	for _, m := range methods {
		if n := e.Leases.callCount(m); n != 0 {
			t.Fatalf("不该碰 %s，实际 %d 次", m, n)
		}
	}
}

// --- 1. 参数门禁：必须一步存储都不碰 ---

func TestSendToUserParameterGateTouchesNoStorage(t *testing.T) {
	longID := strings.Repeat("m", 65)
	cases := []struct {
		name string
		req  *rpc.SendToUserReq
		want error
	}{
		{"空请求", nil, model.ErrInvalidRoomID},
		{"房间号为 0", &rpc.SendToUserReq{RoomId: 0, TargetMid: mid, MessageId: "m1",
			Kind: rpc.BroadcastKind_BROADCAST_KIND_DANMAKU}, model.ErrInvalidRoomID},
		{"房间号为负", &rpc.SendToUserReq{RoomId: -9, TargetMid: mid, MessageId: "m1",
			Kind: rpc.BroadcastKind_BROADCAST_KIND_DANMAKU}, model.ErrInvalidRoomID},
		// 单播不允许 target=0：游客态的 mid 就是 0，允许它等于给全房间游客发定向消息。
		{"目标为游客 0", uniReq("m1", 0, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU), model.ErrInvalidMid},
		{"目标为负", uniReq("m1", -5, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU), model.ErrInvalidMid},
		{"message_id 为空", uniReq("", mid, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU), model.ErrEmptyMessageID},
		{"message_id 只有空白", uniReq(" \t ", mid, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU), model.ErrEmptyMessageID},
		{"message_id 超长", uniReq(longID, mid, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU), model.ErrEmptyRequestID},
		{"message_id 含空格", uniReq("m 1", mid, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU), model.ErrEmptyRequestID},
		{"kind 未指定", uniReq("m1", mid, rpc.BroadcastKind_BROADCAST_KIND_UNSPECIFIED), model.ErrInvalidBroadcastKind},
		{"kind 越界", uniReq("m1", mid, rpc.BroadcastKind(99)), model.ErrInvalidBroadcastKind},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			// 即使库里有一条完全能满足请求的目标租约，参数错也必须在读它之前拒掉。
			seedTargetLease(e, "lgwu_gate", mid, 30, 0)
			reply, err := sendUni(t, e, tc.req)
			if reply != nil {
				t.Errorf("应答 = %+v, want nil", reply)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("错误 = %v, want %v", err, tc.want)
			}
			assertNoStorageCalls(t, e, "ListUserLeases", "Get", "ClaimMessage", "AllowRate", "CacheGet")
			if n := len(e.Logs.all()); n != 0 {
				t.Fatalf("参数错不该落审计，实际 %d 行", n)
			}
		})
	}
}

// --- 2. 目标定位：只有 ACTIVE 租约算命中，无命中是结论 ---

func TestSendToUserCountsOnlyActiveTargetLeases(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	// 同一目标四种形态：两条 ACTIVE（lease_id 升序即投递序）、一条 KICKED、一条已离场，
	// 另有一条 ACTIVE 但 TTL 只有 5 秒（下面推进 6 秒后变 EXPIRED）。
	live1 := seedTargetLease(e, "lgwu_live_1", otherMid, 30, 0)
	live2 := seedTargetLease(e, "lgwu_live_2", otherMid, 300, 0)
	seedTargetLease(e, "lgwu_kicked", otherMid, 300, model.LeaseStateKicked)
	seedTargetLease(e, "lgwu_released", otherMid, 300, model.LeaseStateReleased)
	seedTargetLease(e, "lgwu_stale", otherMid, 5, 0)
	// 别人的 ACTIVE 租约不得被算进目标集（room+mid 双条件）。
	seedTargetLease(e, "lgwu_other_mid", mid, 300, 0)
	// 同 mid 但不同房间的 ACTIVE 租约同样不算命中。
	e.Leases.seedLease(&repository.LeaseRecord{
		LeaseID: "lgwu_other_room", ConnID: "c-or", NodeID: nodeA,
		RoomID: otherRoom, Mid: otherMid, Role: model.RoleViewer,
		State: model.LeaseStateActive, IssuedAt: e.Clock.unix(), ExpireAt: e.Clock.unix() + 300,
	})
	// 先固定投递序（lease_id 升序），再让第 5 条过期：这样「少一条」只能是过期造成的，
	// 而不是播种顺序或 limit 截断。
	e.Clock.advance(6 * time.Second)
	sender := mustSeedSenderLease(t, e, "lgwu_sender", roomID, mid, model.RoleViewer)
	// delivered_leases 的口径是「接入层回报的命中连接数」（sendtouserlogic.go:38-39），
	// 不是本服务数出来的目标条数，所以这里让回报值与目标条数**同为 2 但来源不同**：
	// 只有 Deliver 的参数能证明目标集合，只有应答字段能证明回报值被原样投影。
	e.Tp.result = &repository.FanoutResult{FanoutNodes: 1, TargetedConnections: 2}

	req := uniReq("msg-active", otherMid, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req.SenderLeaseId = sender.LeaseID
	reply, err := sendUni(t, e, req)
	uniNoErr(t, "单播", err)
	uniEQ(t, "结果", reply.GetResult(), rpc.DeliveryResult_DELIVERY_RESULT_SENT)
	uniEQ(t, "回报的命中数原样投影", reply.GetDeliveredLeases(), int32(2))
	uniEQ(t, "回显 message_id", reply.GetMessageId(), "msg-active")
	assertDeliverOnly(t, e, 1)
	if got, want := strings.Join(deliverLeaseIDs(t, e, 0), ","), live1.LeaseID+","+live2.LeaseID; got != want {
		t.Fatalf("投递集合 = %q, want %q（过期/踢出/已离场/别人的/别的房间都不该出现）", got, want)
	}
	row := onlyAudit(t, e)
	uniEQ(t, "审计状态", row.State, model.BroadcastLogSent)
	uniEQ(t, "审计的命中数必须与应答一致", row.TargetedConnections, int32(2))
	uniEQ(t, "审计的被投递者", row.SenderMid, mid)
	uniEQ(t, "审计的 message_id", row.MessageId, "msg-active")
}

// TestSendToUserDoesNotFabricateDeliveredCount 接入层回报 0 条命中时（连接刚好在投递瞬间全掉线），
// 本服务必须出 NO_SUBSCRIBER 而不许把「我给了 2 条 lease_id」当成「已送达 2 条」
// （broadcastcore.go:206-214 的位置选择：命中数为 0 是可丢弃结论，不是错误）。
// 这条用例是 sendtouserlogic.go:38-39「猜出来的已送达 N 人比 0 更有害」的唯一硬证据。
func TestSendToUserDoesNotFabricateDeliveredCount(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	seedTargetLease(e, "lgwu_z1", otherMid, 300, 0)
	seedTargetLease(e, "lgwu_z2", otherMid, 300, 0)
	sender := mustSeedSenderLease(t, e, "lgwu_zs", roomID, mid, model.RoleViewer)
	e.Tp.result = &repository.FanoutResult{FanoutNodes: 1, TargetedConnections: 0}

	req := uniReq("msg-zero", otherMid, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req.SenderLeaseId = sender.LeaseID
	reply, err := sendUni(t, e, req)
	uniNoErr(t, "命中 0 是可丢弃结论", err)
	uniEQ(t, "结果", reply.GetResult(), rpc.DeliveryResult_DELIVERY_RESULT_UNSPECIFIED)
	uniEQ(t, "原因", int32(reply.GetDenyReason()), model.DropNoSubscriber)
	uniEQ(t, "绝不伪造送达数", reply.GetDeliveredLeases(), int32(0))
	// 投递请求确实发出了（目标集合算对了），失败在回报侧 —— 两者必须能被区分开。
	assertDeliverOnly(t, e, 1)
	if got := strings.Join(deliverLeaseIDs(t, e, 0), ","); got != "lgwu_z1,lgwu_z2" {
		t.Fatalf("投递集合 = %q, want lgwu_z1,lgwu_z2", got)
	}
	row := onlyAudit(t, e)
	uniEQ(t, "审计状态", row.State, model.BroadcastLogDropped)
	uniEQ(t, "审计的命中数与回报一致", row.TargetedConnections, int32(0))
}

func TestSendToUserNoActiveConnectionIsConclusionNotError(t *testing.T) {
	cases := []struct {
		name string
		seed func(e *testEnv)
	}{
		{"房间完全没有租约", func(e *testEnv) {}},
		{"只有别人的租约", func(e *testEnv) { seedTargetLease(e, "lgwu_someone", mid, 30, 0) }},
		{"目标在别的房间", func(e *testEnv) {
			e.Leases.seedLease(&repository.LeaseRecord{
				LeaseID: "lgwu_or", ConnID: "c-or", NodeID: nodeA, RoomID: otherRoom,
				Mid: otherMid, Role: model.RoleViewer, State: model.LeaseStateActive,
				IssuedAt: e.Clock.unix(), ExpireAt: e.Clock.unix() + 30,
			})
		}},
		{"目标的租约已被踢", func(e *testEnv) {
			seedTargetLease(e, "lgwu_k", otherMid, 30, model.LeaseStateKicked)
		}},
		{"目标的租约已离场", func(e *testEnv) {
			seedTargetLease(e, "lgwu_r", otherMid, 30, model.LeaseStateReleased)
		}},
		{"目标的租约 TTL 已过", func(e *testEnv) {
			seedTargetLease(e, "lgwu_e", otherMid, 5, 0)
			e.Clock.advance(6 * time.Second)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			seedRoute(e, roomID, model.RouteStateServing, nodeA)
			tc.seed(e)
			// 越权类别（MODERATION）+ 完全无凭据的发送者：结论仍必须是 NO_LEASE，
			// 因为目标定位在鉴权之前（这条正是「离线短路跳过鉴权」的可观测形态）。
			reply, err := sendUni(t, e, uniReq("msg-nl", otherMid,
				rpc.BroadcastKind_BROADCAST_KIND_MODERATION))
			uniNoErr(t, "对方不在线是正常业务结论", err)
			if reply == nil {
				t.Fatalf("应答 = nil, want NO_LEASE 结论")
			}
			uniEQ(t, "结果", reply.GetResult(), rpc.DeliveryResult_DELIVERY_RESULT_NO_LEASE)
			uniEQ(t, "无命中时不该有投递数", reply.GetDeliveredLeases(), int32(0))
			uniEQ(t, "回显 message_id", reply.GetMessageId(), "msg-nl")
			assertDeliverOnly(t, e, 0)
			// 不落审计是刻意设计（sendtouserlogic.go:89-91：否则「对方不在线」会灌满审计表），
			// 代价是越权探测在目标离线时零痕迹 —— 见下一条用例。
			if n := len(e.Logs.all()); n != 0 {
				t.Fatalf("NO_LEASE 不该落审计，实际 %d 行", n)
			}
			assertNoStorageCalls(t, e, "Get", "ClaimMessage", "AllowRate")
		})
	}
}

// --- 3. 离线短路与鉴权的交界：同一请求的两种结论（现状哨兵） ---

// TestSendToUserOfflineShortCircuitSkipsAuthorisation 把「鉴权顺序与 BroadcastToRoom 完全一致」
// 这句承诺的真实边界钉住（sendtouserlogic.go:34-36 vs :74-98）：目标定位在鉴权之前，所以
//   - 目标在线 → 走完整鉴权 → DENIED + 一行 DENIED 审计；
//   - 目标离线 → NO_LEASE + 零审计 + 零凭据读。
//
// 两者可观测的差异使任何调用方（无需 lease、无需 ticket、无需角色）都能把 SendToUser
// 当「某用户此刻在本房间是否在线」的探针：DENIED/其它结论 = 在线，NO_LEASE = 不在线。
// 本用例不放宽断言来掩盖它——它要求两条分支都按现状成立，并把不对称本身写成可比的事实。
func TestSendToUserOfflineShortCircuitSkipsAuthorisation(t *testing.T) {
	// (a) 目标在线：同一个无凭据越权请求得到 DENIED。
	online := newTestEnv(t)
	seedRoute(online, roomID, model.RouteStateServing, nodeA)
	seedTargetLease(online, "lgwu_on", otherMid, 30, 0)
	onlineReply, onlineErr := sendUni(t, online, uniReq("msg-probe-on", otherMid,
		rpc.BroadcastKind_BROADCAST_KIND_MODERATION))
	uniNoErr(t, "目标在线时不该返回错误", onlineErr)
	uniEQ(t, "在线目标 → 走到鉴权并拒出", onlineReply.GetResult(),
		rpc.DeliveryResult_DELIVERY_RESULT_DENIED)
	uniEQ(t, "拒出原因", int32(onlineReply.GetDenyReason()), model.DropPermissionDenied)
	row := onlyAudit(t, online)
	uniEQ(t, "越权必须留 DENIED 行", row.State, model.BroadcastLogDenied)
	assertNoStorageCalls(t, online, "Get")

	// (b) 目标离线：完全相同的请求（只换 message_id 避开去重）得到 NO_LEASE 且零审计。
	offline := newTestEnv(t)
	seedRoute(offline, roomID, model.RouteStateServing, nodeA)
	offReply, offErr := sendUni(t, offline, uniReq("msg-probe-off", otherMid,
		rpc.BroadcastKind_BROADCAST_KIND_MODERATION))
	uniNoErr(t, "离线目标的错误口径", offErr)
	uniEQ(t, "离线目标 → 短路在鉴权之前", offReply.GetResult(),
		rpc.DeliveryResult_DELIVERY_RESULT_NO_LEASE)
	uniEQ(t, "短路时不给拒出原因（没有 DENIED 可解释）",
		int32(offReply.GetDenyReason()), int32(rpc.DropReason_DROP_REASON_UNSPECIFIED))
	if n := len(offline.Logs.all()); n != 0 {
		t.Fatalf("离线短路不该落审计，实际 %d 行", n)
	}

	// (c) 两个结论必须不同：这条断言是「在线与否可被区分」的直接证据。
	if onlineReply.GetResult() == offReply.GetResult() {
		t.Fatalf("在线与离线必须给出不同 result（前提已变则需重读 sendtouserlogic.go:88）")
	}

	// (d) 可信来源（SERVICE + 受信任服务名）也一样先短路：只读一次目标索引就终局返回。
	svcEnv := newTestEnv(t)
	seedRoute(svcEnv, roomID, model.RouteStateServing, nodeA)
	req := uniReq("msg-probe-svc", otherMid, rpc.BroadcastKind_BROADCAST_KIND_MODERATION)
	req.SenderRole = rpc.ConnRole_CONN_ROLE_SERVICE
	reply, err := NewSendToUserLogic(ctxService(t, "moderation"), svcEnv.Svc).SendToUser(req)
	uniNoErr(t, "服务态离线目标", err)
	uniEQ(t, "可信来源也一样先短路", reply.GetResult(),
		rpc.DeliveryResult_DELIVERY_RESULT_NO_LEASE)
	uniEQ(t, "离线短路只读一次目标租约索引",
		int64(svcEnv.Leases.callCount("ListUserLeases")), int64(1))
}

// --- 4. 幂等键：(room, message_id)，不含 target ---

// TestSendToUserIdempotencyKeyExcludesTarget 钉住 proto:443 与实现的口径差：
// 契约写的是 (room,target,message_id) 唯一，实现按 (room,message_id) 去重（:40-44 自认），
// 于是「同一条 message_id 打两个目标」时第二个目标**静默收不到**，
// 而应答仍回 SENT + 第一个目标的命中数（回放结论）。
func TestSendToUserIdempotencyKeyExcludesTarget(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	seedTargetLease(e, "lgwu_a", otherMid, 300, 0)
	seedTargetLease(e, "lgwu_b", anchorMid, 300, 0)
	sender := mustSeedSenderLease(t, e, "lgwu_sender2", roomID, mid, model.RoleViewer)

	first := uniReq("msg-shared", otherMid, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	first.SenderLeaseId = sender.LeaseID
	r1, err := sendUni(t, e, first)
	uniNoErr(t, "首次单播", err)
	uniEQ(t, "首次结果", r1.GetResult(), rpc.DeliveryResult_DELIVERY_RESULT_SENT)
	assertDeliverOnly(t, e, 1)

	// 换目标、同 message_id：去重命中 → 不投递给第二个目标，也不新增审计行。
	second := uniReq("msg-shared", anchorMid, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	second.SenderLeaseId = sender.LeaseID
	r2, err := sendUni(t, e, second)
	uniNoErr(t, "重放不该是错误", err)
	assertDeliverOnly(t, e, 1) // 第二次没有 Deliver：目标 anchor 什么都没收到
	if got := len(e.Logs.all()); got != 1 {
		t.Fatalf("重放不该新增审计行，实际 %d 行", got)
	}
	// 回放的 delivered_leases 来自首行，而不是第二个目标的真实命中数。
	uniEQ(t, "回放的命中数沿用首次结论（与第二个目标无关）",
		r2.GetDeliveredLeases(), r1.GetDeliveredLeases())
	uniEQ(t, "重放结果", r2.GetResult(), rpc.DeliveryResult_DELIVERY_RESULT_SENT)

	// 反证：换 message_id 就打得到第二个目标 —— 说明键里确实只有 room+message_id。
	third := uniReq("msg-shared-b", anchorMid, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	third.SenderLeaseId = sender.LeaseID
	r3, err := sendUni(t, e, third)
	uniNoErr(t, "改 message_id 后的单播", err)
	uniEQ(t, "结果", r3.GetResult(), rpc.DeliveryResult_DELIVERY_RESULT_SENT)
	assertDeliverOnly(t, e, 2)
	if got := strings.Join(deliverLeaseIDs(t, e, 1), ","); got != "lgwu_b" {
		t.Fatalf("第二次投递目标 = %q, want lgwu_b", got)
	}
}

// --- 5. 限流维度：USER 链按发送者而不是目标 ---

func TestSendToUserRateLimitsSenderNotTarget(t *testing.T) {
	roomQuota := &model.LiveGwAccessQuota{
		Scope: model.QuotaScopeRoom, ScopeId: roomID, BroadcastQps: 200, DanmakuQps: 100,
	}

	t.Run("目标被降配不影响发送者额度", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.addQuotaRow(roomQuota)
		// 目标（otherMid）在 USER 维度被降到 1 次：若限流按目标算，第二次必然 RATE_LIMITED。
		e.addQuotaRow(&model.LiveGwAccessQuota{
			Scope: model.QuotaScopeUser, ScopeId: otherMid, DanmakuQps: 1,
		})
		e.addQuotaRow(&model.LiveGwAccessQuota{
			Scope: model.QuotaScopeUser, ScopeId: mid, DanmakuQps: 100,
		})
		seedTargetLease(e, "lgwu_rate_t", otherMid, 300, 0)
		sender := mustSeedSenderLease(t, e, "lgwu_rate_s", roomID, mid, model.RoleViewer)

		for i, id := range []string{"msg-r1", "msg-r2"} {
			req := uniReq(id, otherMid, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
			req.SenderLeaseId = sender.LeaseID
			reply, err := sendUni(t, e, req)
			uniNoErr(t, "两条单播", err)
			if reply.GetResult() != rpc.DeliveryResult_DELIVERY_RESULT_SENT {
				t.Fatalf("第 %d 条结果 = %v，目标的降配不该作用于发送者", i+1, reply.GetResult())
			}
		}
		assertDeliverOnly(t, e, 2)
		uniEQ(t, "发送者维度计数", e.Leases.rateCount("dqps:mid:"+uniItoa(mid)), int32(2))
		uniEQ(t, "目标维度一次都没计", e.Leases.rateCount("dqps:mid:"+uniItoa(otherMid)), int32(0))
	})

	t.Run("发送者被降配时第二条被限流", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.addQuotaRow(roomQuota)
		e.addQuotaRow(&model.LiveGwAccessQuota{
			Scope: model.QuotaScopeUser, ScopeId: otherMid, DanmakuQps: 100,
		})
		e.addQuotaRow(&model.LiveGwAccessQuota{
			Scope: model.QuotaScopeUser, ScopeId: mid, DanmakuQps: 1,
		})
		seedTargetLease(e, "lgwu_rate_t2", otherMid, 300, 0)
		sender := mustSeedSenderLease(t, e, "lgwu_rate_s2", roomID, mid, model.RoleViewer)

		req := uniReq("msg-q1", otherMid, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
		req.SenderLeaseId = sender.LeaseID
		r1, err := sendUni(t, e, req)
		uniNoErr(t, "首条", err)
		uniEQ(t, "首条", r1.GetResult(), rpc.DeliveryResult_DELIVERY_RESULT_SENT)

		req2 := uniReq("msg-q2", otherMid, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
		req2.SenderLeaseId = sender.LeaseID
		r2, err := sendUni(t, e, req2)
		uniNoErr(t, "超限是结论不是错误", err)
		// 单播语义：限流不翻译成 DENIED（DENIED 专指越权），留 UNSPECIFIED + 可读原因。
		uniEQ(t, "被限流的结果", r2.GetResult(), rpc.DeliveryResult_DELIVERY_RESULT_UNSPECIFIED)
		uniEQ(t, "被限流的原因", int32(r2.GetDenyReason()), model.DropRateLimited)
		assertDeliverOnly(t, e, 1)
	})
}

// --- 6. 载荷上限与通道未接线 ---

func TestSendToUserPayloadCapSharedWithBroadcast(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	seedTargetLease(e, "lgwu_pay", otherMid, 300, 0)
	sender := mustSeedSenderLease(t, e, "lgwu_pay_s", roomID, mid, model.RoleViewer)

	// 载荷正文用全 0 填充：审计只存摘要与字节数，用例也不该把可识别文本带进日志。
	req := uniReq("msg-big", otherMid, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req.SenderLeaseId = sender.LeaseID
	req.Payload = make([]byte, 1025) // conf 的 MaxPayloadBytes=1024
	reply, err := sendUni(t, e, req)
	uniNoErr(t, "超限是可丢弃结论", err)
	uniEQ(t, "结果", reply.GetResult(), rpc.DeliveryResult_DELIVERY_RESULT_UNSPECIFIED)
	uniEQ(t, "原因", int32(reply.GetDenyReason()), model.DropPayloadTooLarge)
	assertDeliverOnly(t, e, 0)
	row := onlyAudit(t, e)
	uniEQ(t, "审计状态", row.State, model.BroadcastLogDropped)
	uniEQ(t, "审计里的载荷字节数", row.PayloadBytes, int32(1025))
	if row.PayloadDigest == "" {
		t.Errorf("审计必须有载荷摘要，实际为空")
	}

	// 边界：恰好等于上限必须放行（证明判定是 > 而不是 >=）。
	req2 := uniReq("msg-edge", otherMid, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
	req2.SenderLeaseId = sender.LeaseID
	req2.Payload = make([]byte, 1024)
	r2, err := sendUni(t, e, req2)
	uniNoErr(t, "恰好等于上限", err)
	uniEQ(t, "结果", r2.GetResult(), rpc.DeliveryResult_DELIVERY_RESULT_SENT)
	assertDeliverOnly(t, e, 1)
}

func TestSendToUserTransportUnwired(t *testing.T) {
	t.Run("require_reliable=false 时是可丢弃结论", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		seedTargetLease(e, "lgwu_tw", otherMid, 300, 0)
		sender := mustSeedSenderLease(t, e, "lgwu_tw_s", roomID, mid, model.RoleViewer)
		e.markTransportUnwired()

		req := uniReq("msg-tw", otherMid, rpc.BroadcastKind_BROADCAST_KIND_DANMAKU)
		req.SenderLeaseId = sender.LeaseID
		reply, err := sendUni(t, e, req)
		uniNoErr(t, "未接线默认丢弃", err)
		uniEQ(t, "结果", reply.GetResult(), rpc.DeliveryResult_DELIVERY_RESULT_TRANSPORT_UNAVAILABLE)
		uniEQ(t, "结果与原因同因", int32(reply.GetDenyReason()), model.DropTransportUnavailable)
		// 命中数必须是 0：通道没接线时把「目标有连接」报成「已投递 N 条」就是伪造送达。
		uniEQ(t, "未接线不得谎报投递数", reply.GetDeliveredLeases(), int32(0))
	})

	t.Run("require_reliable=true 时必须上抛错误", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		seedTargetLease(e, "lgwu_tw2", otherMid, 300, 0)
		e.markTransportUnwired()

		req := uniReq("msg-tw2", otherMid, rpc.BroadcastKind_BROADCAST_KIND_MODERATION)
		req.SenderRole = rpc.ConnRole_CONN_ROLE_SERVICE
		req.RequireReliable = true
		reply, err := NewSendToUserLogic(ctxService(t, "moderation"), e.Svc).SendToUser(req)
		if reply != nil {
			t.Errorf("应答 = %+v, want nil", reply)
		}
		if err == nil {
			t.Fatalf("错误 = nil, want 非 nil：审核处置不得静默丢失")
		}
		if n := len(e.Logs.all()); n != 1 {
			t.Fatalf("可靠失败也要留一行审计，实际 %d 行", n)
		}
	})
}

// --- 7. 依赖故障原样外传 ---

func TestSendToUserPropagatesStoreFailures(t *testing.T) {
	e := newTestEnv(t)
	boom := errors.New("live-gateway/redis: user lease index read down")
	e.Leases.failWith("ListUserLeases", boom)

	reply, err := sendUni(t, e, uniReq("msg-fault", otherMid,
		rpc.BroadcastKind_BROADCAST_KIND_DANMAKU))
	if reply != nil {
		t.Errorf("应答 = %+v, want nil", reply)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want 原样外传 %v", err, boom)
	}
	// 读故障绝不能被折成 NO_LEASE：那会让「在线态不可用」看起来像「对方不在线」。
	if n := len(e.Logs.all()); n != 0 {
		t.Fatalf("故障发生在鉴权之前，不该落审计，实际 %d 行", n)
	}
}
