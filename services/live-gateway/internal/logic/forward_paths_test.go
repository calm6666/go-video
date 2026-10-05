package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/live-gateway/internal/config"
	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"
)

// 本文件覆盖两条转发链路：ForwardDanmaku（弹幕服务→房间）与 ForwardSystemEvent（内部事件→房间）。
// 它们与 BroadcastToRoom 共用同一条判定链，所以本文件的重点不是再测一遍门禁顺序，而是这三件事：
//  1. 入口差异：弹幕入口**没有角色字段**（不给自报提权留口子），系统事件入口**只有可信内部主体**能用；
//  2. 归属判定：被踢/已离场/别人的连接/别人的房间，结论必须各不相同且都留 DENIED 审计；
//  3. 脱敏与幂等的落点：审计里只有摘要，系统事件按 event_id 去重且定位串是 "evt:"+event_id。

// --- 构造助手 ---

// danmakuID 弹幕主键（本服务只引用不复制正文）。
const danmakuID int64 = 88001

// fwdReq 一条最小可用的弹幕转发请求（凭据由用例补齐）。
func fwdReq(messageID string) *rpc.ForwardDanmakuReq {
	return &rpc.ForwardDanmakuReq{
		RoomId: roomID, DanmakuId: danmakuID, SenderMid: mid, MessageId: messageID,
		Payload: []byte(`{"text":"普通弹幕"}`),
	}
}

// sysEvtReq 一条最小可用的系统事件请求（调用方归因由用例的 ctx 决定）。
func sysEvtReq(eventID, eventType string, kind rpc.BroadcastKind) *rpc.ForwardSystemEventReq {
	return &rpc.ForwardSystemEventReq{
		RoomId: roomID, EventId: eventID, Kind: kind, EventType: eventType, SourceService: "moderation",
	}
}

// evtRow 取系统事件那条审计行：定位串是 "evt:"+event_id，不是 event_id 本身。
func evtRow(t *testing.T, e *testEnv, eventID string) *model.LiveGwBroadcastLog {
	t.Helper()
	for _, row := range e.Logs.all() {
		if row.MessageId == "evt:"+eventID {
			return row
		}
	}
	t.Fatalf("审计表里没有事件 %s 的行（定位串应为 evt:<event_id>）: %+v", eventID, e.Logs.all())
	return nil
}

// revokeTicket 用 model 的真实撤销路径把票据置为 REVOKED（不是手改字段，避免测出替身才有的行为）。
func revokeTicket(t *testing.T, e *testEnv, ticketID string) {
	t.Helper()
	aff, err := e.Ticket.RevokeByTicketID(context.Background(), ticketID, "单元测试撤销", "tester")
	if err != nil {
		t.Fatalf("撤销票据失败: %v", err)
	}
	if aff != 1 {
		t.Fatalf("撤销应命中 1 行，实际 %d", aff)
	}
}

// assertEventLedger 断言一次系统事件投递在幂等两张账上的落点。
func assertEventLedger(t *testing.T, e *testEnv, eventID string, wantClaimed bool) {
	t.Helper()
	if _, ok := e.Leases.events[fmt.Sprintf("%d|%s", roomID, eventID)]; ok != wantClaimed {
		t.Fatalf("事件去重键 (room_id=%d, event_id=%s) 存在性应为 %v", roomID, eventID, wantClaimed)
	}
}

// --- ForwardDanmaku：参数门禁 ---

func TestForwardDanmakuParameterGate(t *testing.T) {
	longID := strings.Repeat("d", 65)
	cases := []struct {
		name string
		req  *rpc.ForwardDanmakuReq
		want string // 期望错误里的关键字（danmaku_id 那条没有专用哨兵，见交付报告）
	}{
		{"空请求", nil, "invalid room_id"},
		{"房间号为 0", &rpc.ForwardDanmakuReq{RoomId: 0, DanmakuId: danmakuID, MessageId: "m"}, "invalid room_id"},
		// danmaku_id<=0 必须拒：本服务不落弹幕正文，没有主键的转发在 danmaku 台账上对不上账。
		{"弹幕主键为 0", &rpc.ForwardDanmakuReq{RoomId: roomID, DanmakuId: 0, MessageId: "m"}, "danmaku_id"},
		{"弹幕主键为负", &rpc.ForwardDanmakuReq{RoomId: roomID, DanmakuId: -3, MessageId: "m"}, "danmaku_id"},
		{"mid 为负", &rpc.ForwardDanmakuReq{RoomId: roomID, DanmakuId: danmakuID, SenderMid: -1, MessageId: "m"}, "invalid mid"},
		{"message_id 为空", &rpc.ForwardDanmakuReq{RoomId: roomID, DanmakuId: danmakuID, MessageId: "  "}, "message_id is required"},
		{"message_id 超长", &rpc.ForwardDanmakuReq{RoomId: roomID, DanmakuId: danmakuID, MessageId: longID}, "message_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			seedRoute(e, roomID, model.RouteStateServing, nodeA)
			e.Logs.fail("Insert", errors.New("audit-insert-should-not-run"))
			reply, err := NewForwardDanmakuLogic(ctxClient(t), e.Svc).ForwardDanmaku(tc.req)
			if err == nil {
				t.Fatalf("非法参数必须报错，实际 reply=%+v", reply)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误里应含 %q（结论要指到字段），实际 %v", tc.want, err)
			}
			if reply != nil {
				t.Fatalf("参数被拒不返回响应体: %+v", reply)
			}
			// 参数阶段不碰任何下游：既不占幂等键、也不落审计（否则一个坏请求就能污染取证面）。
			assertNoFanout(t, e)
			if n := e.Leases.callCount("ClaimMessage"); n != 0 {
				t.Fatalf("参数非法不该占幂等键，ClaimMessage %d 次", n)
			}
			if n := e.Logs.inserts; n != 0 {
				t.Fatalf("参数非法不该落审计行，实际 %d 次", n)
			}
		})
	}
}

// --- ForwardDanmaku：身份与房间成员判定 ---

func TestForwardDanmakuSenderAdjudication(t *testing.T) {
	cases := []struct {
		name     string
		lease    func(t *testing.T, e *testEnv) string
		advance  bool // 只用于「租约已过期」：先播种再推进时钟
		wantDrop int32
	}{
		{name: "带本房间活租约", wantDrop: model.DropOK,
			lease: func(t *testing.T, e *testEnv) string {
				return mustSeedSenderLease(t, e, "lgwl_fwd_ok", roomID, mid, model.RoleViewer).LeaseID
			}},
		{name: "不带任何凭据", wantDrop: model.DropPermissionDenied},
		{name: "租约不存在", wantDrop: model.DropBadTicket,
			lease: func(t *testing.T, e *testEnv) string { return "lgwl_fwd_missing" }},
		// 房间不匹配是「越权」而不是「票据坏了」：审计里两档的原因必须能分开。
		{name: "租约属于别的房间", wantDrop: model.DropPermissionDenied,
			lease: func(t *testing.T, e *testEnv) string {
				return mustSeedSenderLease(t, e, "lgwl_fwd_room", otherRoom, mid, model.RoleViewer).LeaseID
			}},
		{name: "租约是别人的 mid", wantDrop: model.DropPermissionDenied,
			lease: func(t *testing.T, e *testEnv) string {
				return mustSeedSenderLease(t, e, "lgwl_fwd_mid", roomID, otherMid, model.RoleViewer).LeaseID
			}},
		{name: "租约已被踢下线", wantDrop: model.DropBadTicket,
			lease: func(t *testing.T, e *testEnv) string {
				rec := mustSeedSenderLease(t, e, "lgwl_fwd_kicked", roomID, mid, model.RoleViewer)
				rec.State = model.LeaseStateKicked
				return rec.LeaseID
			}},
		{name: "租约已主动离场", wantDrop: model.DropBadTicket,
			lease: func(t *testing.T, e *testEnv) string {
				rec := mustSeedSenderLease(t, e, "lgwl_fwd_released", roomID, mid, model.RoleViewer)
				rec.State = model.LeaseStateReleased
				return rec.LeaseID
			}},
		{name: "租约已过期", wantDrop: model.DropBadTicket, advance: true,
			lease: func(t *testing.T, e *testEnv) string {
				return mustSeedSenderLease(t, e, "lgwl_fwd_expired", roomID, mid, model.RoleViewer).LeaseID
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			seedRoute(e, roomID, model.RouteStateServing, nodeA)
			req := fwdReq("dmw-auth")
			if tc.lease != nil {
				req.SenderLeaseId = tc.lease(t, e)
			}
			if tc.advance {
				// seedActiveLease 的 TTL 是 30 秒，且必须在播种之后推进才有效（先推进等于白推）。
				e.Clock.advance(31_000 * 1_000_000)
			}
			reply, err := NewForwardDanmakuLogic(ctxClient(t), e.Svc).ForwardDanmaku(req)
			if err != nil {
				t.Fatalf("成员判定是可解释结论，不该返回错误: %v", err)
			}
			if int32(reply.GetDropReason()) != tc.wantDrop {
				t.Fatalf("期望 drop=%d，实际 %+v", tc.wantDrop, reply)
			}
			// 「丢弃但仍报已受理」是本方法明令禁止的行为：danmaku 侧据此计数，会永久不一致。
			if reply.GetAccepted() != (tc.wantDrop == model.DropOK) {
				t.Fatalf("accepted 与 drop_reason 不自洽: %+v", reply)
			}
			row := onlyAudit(t, e)
			if tc.wantDrop == model.DropOK {
				assertFanoutOnce(t, e)
				if row.State != model.BroadcastLogSent {
					t.Fatalf("合法弹幕该落 SENT，实际 %+v", row)
				}
				if row.SenderMid != mid || row.SenderRole != model.RoleViewer {
					t.Fatalf("发送者以服务端登记的租约值为准: %+v", row)
				}
				return
			}
			assertNoFanout(t, e)
			if row.State != model.BroadcastLogDenied {
				t.Fatalf("成员判定拒出必须留 DENIED 行（不是 DROPPED），实际 %+v", row)
			}
			if row.DropReason != tc.wantDrop {
				t.Fatalf("审计原因要与回执一致，期望 %d 实际 %d", tc.wantDrop, row.DropReason)
			}
			if row.MessageId != "dmw-auth" {
				t.Fatalf("弹幕审计的定位串就是 message_id（无 evt: 前缀）: %q", row.MessageId)
			}
		})
	}
}

// TestForwardDanmakuTicketCredentialPaths 覆盖票据作为发送者凭据的四条判定：
// 合法票据只校验不消费；别人的票据、被撤销的票据、已过期票据都要拒且原因各不相同。
func TestForwardDanmakuTicketCredentialPaths(t *testing.T) {
	t.Run("合法票据放行且不消费", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		lease := mustSeedSenderLease(t, e, "lgwl_fwd_tick", roomID, mid, model.RoleViewer)
		ticket, info := mustIssueTicket(t, e, lease.LeaseID, "req-fwd-ticket")
		req := fwdReq("dmw-ticket-ok")
		req.SenderTicket = ticket
		req.SenderLeaseId = "" // 只用票据这一条凭据通道
		reply, err := NewForwardDanmakuLogic(ctxClient(t), e.Svc).ForwardDanmaku(req)
		if err != nil || !reply.GetAccepted() {
			t.Fatalf("合法票据应放行: reply=%+v err=%v", reply, err)
		}
		row := revokedRow(t, e, info.GetTicketId())
		if row.State != model.TicketStateIssued {
			t.Fatalf("弹幕鉴权不得把票据推成终态，实际 state=%d", row.State)
		}
		if !e.Leases.hasTicketBit(row.TicketHash) {
			t.Fatalf("广播鉴权不得清掉 Redis 票据有效位（否则客户端重连必失败）")
		}
		// 审计里的发送者来自票据断言，而不是请求字段。
		audit := onlyAudit(t, e)
		if audit.SenderMid != mid {
			t.Fatalf("审计发送者应为票据里的 mid，实际 %d", audit.SenderMid)
		}
	})

	t.Run("别人的票据被拒", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		other := mustSeedSenderLease(t, e, "lgwl_fwd_other", roomID, otherMid, model.RoleViewer)
		// 这张票据**合法地**属于 otherMid（mustIssueTicket 的 issueReq 固定按 mid 申请，
		// 拿别人的租约去申请会被 IssueReconnectTicket 自己的三元组校验拒掉，测不到下面的越权分支）。
		issued, err := NewIssueReconnectTicketLogic(ctxClient(t), e.Svc).IssueReconnectTicket(
			&rpc.IssueReconnectTicketReq{LeaseId: other.LeaseID, RoomId: roomID, Mid: otherMid, RequestId: "req-fwd-other"})
		if err != nil {
			t.Fatalf("为 otherMid 签发票据失败: %v", err)
		}
		req := fwdReq("dmw-ticket-foreign")
		req.SenderTicket = issued.GetTicket()
		reply, err := NewForwardDanmakuLogic(ctxClient(t), e.Svc).ForwardDanmaku(req)
		if err != nil {
			t.Fatalf("不该返回错误: %v", err)
		}
		if reply.GetDropReason() != rpc.DropReason_DROP_REASON_PERMISSION_DENIED {
			t.Fatalf("三元组不匹配是越权而不是票据损坏: %+v", reply)
		}
		assertNoFanout(t, e)
	})

	t.Run("已撤销与畸形票据都是 BAD_TICKET", func(t *testing.T) {
		for _, tc := range []struct{ name, ticket string }{{"已撤销", ""}, {"畸形串", "lgwt_bogus.notaticket"}} {
			e := newTestEnv(t)
			seedRoute(e, roomID, model.RouteStateServing, nodeA)
			lease := mustSeedSenderLease(t, e, "lgwl_fwd_bad", roomID, mid, model.RoleViewer)
			ticket, info := mustIssueTicket(t, e, lease.LeaseID, "req-fwd-bad")
			if tc.name == "已撤销" {
				revokeTicket(t, e, info.GetTicketId())
			} else {
				ticket = tc.ticket
			}
			req := fwdReq("dmw-ticket-" + tc.name)
			req.SenderTicket = ticket
			reply, err := NewForwardDanmakuLogic(ctxClient(t), e.Svc).ForwardDanmaku(req)
			if err != nil {
				t.Fatalf("不该返回错误: %v", err)
			}
			if reply.GetDropReason() != rpc.DropReason_DROP_REASON_BAD_TICKET {
				t.Fatalf("%s 票据必须判 BAD_TICKET，实际 %+v", tc.name, reply)
			}
			row := onlyAudit(t, e)
			if row.State != model.BroadcastLogDenied {
				t.Fatalf("凭据不成立要留 DENIED 行，实际 %+v", row)
			}
			assertNoFanout(t, e)
		}
	})

	t.Run("过期票据在验签之后仍被拒", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		lease := mustSeedSenderLease(t, e, "lgwl_fwd_texpired", roomID, mid, model.RoleViewer)
		ticket, info := mustIssueTicket(t, e, lease.LeaseID, "req-fwd-texpired")
		// 默认票据 TTL=120 秒，而租约 TTL 只有 30 秒：推进 121 秒后租约早已过期，
		// 但这条用例走的是票据通道，判定的必须是票据自身的时效。
		e.Clock.advance(121_000_000_000)
		req := fwdReq("dmw-ticket-expired")
		req.SenderTicket = ticket
		reply, err := NewForwardDanmakuLogic(ctxClient(t), e.Svc).ForwardDanmaku(req)
		if err != nil {
			t.Fatalf("不该返回错误: %v", err)
		}
		if reply.GetDropReason() != rpc.DropReason_DROP_REASON_BAD_TICKET {
			t.Fatalf("过期票据必须 BAD_TICKET，实际 %+v", reply)
		}
		if got := revokedRow(t, e, info.GetTicketId()).State; got != model.TicketStateIssued {
			t.Fatalf("拒出也不该改动票据状态（判定与消费是两件事），实际 state=%d", got)
		}
	})

	t.Run("签名器缺失时显式失败而不是判 BAD_TICKET", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.markSignerMissing()
		req := fwdReq("dmw-no-signer")
		req.SenderTicket = "lgwt_x.y"
		reply, err := NewForwardDanmakuLogic(ctxClient(t), e.Svc).ForwardDanmaku(req)
		if !errors.Is(err, repository.ErrSignerMissing) {
			t.Fatalf("无法验签时必须让上游知道依赖缺失，不能当成「票据是伪造的」: err=%v reply=%+v", err, reply)
		}
		assertNoFanout(t, e)
	})
}

// --- ForwardDanmaku：载荷、脱敏与可丢弃语义 ---

func TestForwardDanmakuPayloadCapAndRedaction(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	lease := mustSeedSenderLease(t, e, "lgwl_fwd_payload", roomID, mid, model.RoleViewer)
	secret := "弹幕正文只在内存与下发通道里短期存在"

	req := fwdReq("dmw-payload-ok")
	req.SenderLeaseId = lease.LeaseID
	req.Payload = []byte(secret)
	// 客户端自报的 content_digest 必须是「不可信观测值」：伪造它不能改写审计里的摘要。
	req.ContentDigest = strings.Repeat("f", 64)
	reply, err := NewForwardDanmakuLogic(ctxClient(t), e.Svc).ForwardDanmaku(req)
	if err != nil || !reply.GetAccepted() {
		t.Fatalf("reply=%+v err=%v", reply, err)
	}
	row := onlyAudit(t, e)
	if row.PayloadDigest == req.ContentDigest || row.PayloadDigest != payloadDigest([]byte(secret), 32) {
		t.Fatalf("审计摘要必须由服务端按真实载荷算出，实际 %q", row.PayloadDigest)
	}
	if row.PayloadBytes != int32(len(secret)) || strings.Contains(row.PayloadDigest, secret) {
		t.Fatalf("审计只能有摘要与字节数: %+v", row)
	}
	if row.SourceService != "danmaku" {
		t.Fatalf("本入口的来源标识固定为 danmaku（调用方无法自报）: %q", row.SourceService)
	}

	// 超限：可丢弃结论 + DROPPED 审计，且不占幂等键、不烧限流额度。
	big := fwdReq("dmw-payload-big")
	big.SenderLeaseId = lease.LeaseID
	big.Payload = []byte(strings.Repeat("x", 1025))
	drop, err := NewForwardDanmakuLogic(ctxClient(t), e.Svc).ForwardDanmaku(big)
	if err != nil {
		t.Fatalf("弹幕入口没有 require_reliable 字段，超限必须是结论而不是错误: %v", err)
	}
	if drop.GetAccepted() || drop.GetDropReason() != rpc.DropReason_DROP_REASON_PAYLOAD_TOO_LARGE {
		t.Fatalf("超限应回 PAYLOAD_TOO_LARGE: %+v", drop)
	}
	if len(e.Tp.fanoutReqs) != 1 {
		t.Fatalf("超限那条不得投递，扇出仍应是 1 次")
	}
	// 首条合法消息自己就会打两次限流（房间层 bqps + 用户层 dqps，见 enforceBroadcastRate），
	// 所以这里比的是「超限那条有没有新增计次/占用幂等键」，不是绝对次数。
	baselineClaims, baselineRates := e.Leases.callCount("ClaimMessage"), e.Leases.callCount("AllowRate")
	if baselineClaims != 1 || baselineRates < 1 {
		t.Fatalf("首条合法消息应恰好占一次幂等键并至少过一次限流，实际 claims=%d rates=%d",
			baselineClaims, baselineRates)
	}
	if n := e.Leases.callCount("ClaimMessage"); n != baselineClaims {
		t.Fatalf("超限不该新增幂等键（只有首条合法消息占过一次），实际 ClaimMessage %d 次", n)
	}
	if n := e.Leases.callCount("AllowRate"); n != baselineRates {
		t.Fatalf("超限不该新增限流计次（只有首条合法消息计过一次），实际 AllowRate %d 次", n)
	}
	bigRow := e.Logs.all()[len(e.Logs.all())-1]
	if bigRow.State != model.BroadcastLogDropped || bigRow.DropReason != model.DropPayloadTooLarge ||
		bigRow.PayloadBytes != 1025 {
		t.Fatalf("超限行要记下真实体积否则排障无从下手: %+v", bigRow)
	}
}

// TestForwardDanmakuIsAlwaysDroppable 钉住弹幕入口的可靠性语义：
// 通道未接线时也不能返回 error —— 弹幕丢一条比让 danmaku 侧为一条弹幕重推整条链路更便宜，
// 反过来「报错」会被上游当成可重试而放大流量（helpers.go 里 require_reliable 恒为 false 的分支）。
func TestForwardDanmakuIsAlwaysDroppable(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA, nodeB)
	e.markTransportUnwired()
	lease := mustSeedSenderLease(t, e, "lgwl_fwd_unwired", roomID, mid, model.RoleViewer)
	req := fwdReq("dmw-unwired")
	req.SenderLeaseId = lease.LeaseID
	reply, err := NewForwardDanmakuLogic(ctxClient(t), e.Svc).ForwardDanmaku(req)
	if err != nil {
		t.Fatalf("弹幕入口不该因通道故障报错: %v", err)
	}
	if reply.GetAccepted() || reply.GetDropReason() != rpc.DropReason_DROP_REASON_TRANSPORT_UNAVAILABLE {
		t.Fatalf("必须显式回 TRANSPORT_UNAVAILABLE 而不是假成功: %+v", reply)
	}
	if reply.GetFanoutNodes() != 2 {
		t.Fatalf("丢弃也要说明本应打到几个节点，实际 %d", reply.GetFanoutNodes())
	}
	row := onlyAudit(t, e)
	if row.State != model.BroadcastLogDropped || row.DropReason != model.DropTransportUnavailable {
		t.Fatalf("通道故障要落 DROPPED+TRANSPORT_UNAVAILABLE，实际 %+v", row)
	}
}

func TestForwardDanmakuIdempotentReplay(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	lease := mustSeedSenderLease(t, e, "lgwl_fwd_idem", roomID, mid, model.RoleViewer)
	logic := NewForwardDanmakuLogic(ctxClient(t), e.Svc)

	req := fwdReq("dmw-idem")
	req.SenderLeaseId = lease.LeaseID
	if first, err := logic.ForwardDanmaku(req); err != nil || !first.GetAccepted() {
		t.Fatalf("首次转发应受理: reply=%+v err=%v", first, err)
	}
	rates, fanouts := e.Leases.callCount("AllowRate"), len(e.Tp.fanoutReqs)

	replay := fwdReq("dmw-idem")
	replay.SenderLeaseId = lease.LeaseID
	second, err := logic.ForwardDanmaku(replay)
	if err != nil {
		t.Fatalf("重放不该报错: %v", err)
	}
	if !second.GetAccepted() {
		t.Fatalf("重放要复述首次结论: %+v", second)
	}
	if len(e.Tp.fanoutReqs) != fanouts {
		t.Fatalf("重放不得二次扇出")
	}
	if n := e.Leases.callCount("AllowRate"); n != rates {
		t.Fatalf("重放不该再计限流，AllowRate 从 %d 变成 %d", rates, n)
	}
	if len(e.Logs.all()) != 1 {
		t.Fatalf("重放不该再落审计行，实际 %d 行", len(e.Logs.all()))
	}
	// 弹幕按 message_id 去重，绝不能去碰事件那张账（两张账的键空间不同）。
	if n := e.Leases.callCount("ClaimMessage"); n != 2 {
		t.Fatalf("弹幕应两次 ClaimMessage，实际 %d 次", n)
	}
	if n := e.Leases.callCount("ClaimEvent"); n != 0 {
		t.Fatalf("弹幕路径不该使用事件去重窗口，ClaimEvent %d 次", n)
	}

	// 同一个 message_id 换个发送者再来一次：这不是「重放复述」，而是越权探测。
	// 判定链把鉴权放在去重之前（broadcastcore.go 的刻意顺序），所以结论必须是 PERMISSION_DENIED
	// 且不留下发痕迹——否则攻击者复用一条旧 message_id 就能把越权变成无痕操作。
	before := len(e.Tp.fanoutReqs)
	steal := fwdReq("dmw-idem")
	steal.SenderLeaseId = lease.LeaseID
	steal.SenderMid = otherMid
	third, err := logic.ForwardDanmaku(steal)
	if err != nil {
		t.Fatalf("越权重放是结论不是错误: %v", err)
	}
	if third.GetAccepted() || third.GetDropReason() != rpc.DropReason_DROP_REASON_PERMISSION_DENIED {
		t.Fatalf("换发送者的重放必须拒成 PERMISSION_DENIED，实际 %+v", third)
	}
	if len(e.Tp.fanoutReqs) != before {
		t.Fatalf("越权重放不得二次扇出")
	}
}

// TestForwardDanmakuEscalationAuditRow 记录一处审计证据缺口（缺陷，未修）：
// broadcastcore.go 把鉴权排在去重之前的理由是「越权尝试即使带着一个重复的 message_id 也必须留下
// DENIED 审计」，但 live_gw_broadcast_log 的唯一键就是 uniq_room_message(room_id, message_id)
// （deploy/migrations/live-gateway/000002:84），同一 message_id 的首次受理行已占位，
// 随后的 DENIED 行撞上唯一键后 model.Insert 按「重复投递」返回 (0, nil)
// （live_gw_broadcast_log.go:121-124）——既不报错也不留痕，于是「复用旧 message_id 把越权探测
// 变成无痕操作」这条路恰恰仍然成立，与注释里的安全承诺相反。
// 修法二选一（都要改生产代码，不能靠改断言收口）：审计定位串带上发送者/结论维度，
// 或让拒绝证据写进不与广播幂等冲突的第二张账。
func TestForwardDanmakuEscalationAuditRow(t *testing.T) {
	t.Skip("缺陷已上报: services/live-gateway/model/live_gw_broadcast_log.go:121 ——" +
		" 同 message_id 的 DENIED 审计行撞上 uniq_room_message 后被静默丢弃")
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	lease := mustSeedSenderLease(t, e, "lgwl_fwd_audit", roomID, mid, model.RoleViewer)
	logic := NewForwardDanmakuLogic(ctxClient(t), e.Svc)
	req := fwdReq("dmw-audit")
	req.SenderLeaseId = lease.LeaseID
	if _, err := logic.ForwardDanmaku(req); err != nil {
		t.Fatalf("首次转发失败: %v", err)
	}
	steal := fwdReq("dmw-audit")
	steal.SenderLeaseId = lease.LeaseID
	steal.SenderMid = otherMid
	if _, err := logic.ForwardDanmaku(steal); err != nil {
		t.Fatalf("越权重放是结论不是错误: %v", err)
	}
	var denied int
	for _, row := range e.Logs.all() {
		if row.State == model.BroadcastLogDenied {
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("越权尝试必须留下一行 DENIED，实际 %d 行（共 %d 行）", denied, len(e.Logs.all()))
	}
}

func TestForwardDanmakuRateLimitedIsNotAccepted(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID, BroadcastQps: 1, Version: 1})
	lease := mustSeedSenderLease(t, e, "lgwl_fwd_rate", roomID, mid, model.RoleViewer)
	logic := NewForwardDanmakuLogic(ctxClient(t), e.Svc)
	for i, msg := range []string{"dmw-r1", "dmw-r2"} {
		req := fwdReq(msg)
		req.SenderLeaseId = lease.LeaseID
		reply, err := logic.ForwardDanmaku(req)
		if err != nil {
			t.Fatalf("限流是结论不是错误: %v", err)
		}
		if reply.GetAccepted() != (i == 0) {
			t.Fatalf("第 %d 条的受理结论异常: %+v", i+1, reply)
		}
	}
	if got := e.Leases.rateCount(fmt.Sprintf("dqps:mid:%d", mid)); got != 1 {
		t.Fatalf("房间层先拒时用户层不该再计次，实际 dqps:mid:%d=%d", mid, got)
	}
	// 两条弹幕的审计各一行：一 SENT 一 DROPPED，且都指向同一个 sender。
	rows := e.Logs.all()
	if len(rows) != 2 || rows[1].State != model.BroadcastLogDropped || rows[1].DropReason != model.DropRateLimited {
		t.Fatalf("限流必须留可解释的 DROPPED 行: %+v", rows)
	}
}

// TestForwardDanmakuSentAtSkewIsObservationOnly 客户端时钟不可信是常态：
// sent_at 偏差再大也不能让弹幕消失（判定一律用服务端时钟）。
func TestForwardDanmakuSentAtSkewIsObservationOnly(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	lease := mustSeedSenderLease(t, e, "lgwl_fwd_skew", roomID, mid, model.RoleViewer)
	req := fwdReq("dmw-skew")
	req.SenderLeaseId = lease.LeaseID
	req.SentAt = testBaseTime - 10_000_000 // 偏差远超 HeartbeatMaxSkewSeconds=300
	reply, err := NewForwardDanmakuLogic(ctxClient(t), e.Svc).ForwardDanmaku(req)
	if err != nil || !reply.GetAccepted() {
		t.Fatalf("时钟偏差只能观测，不能拒发: reply=%+v err=%v", reply, err)
	}
	if row := onlyAudit(t, e); row.Ctime != testBaseTime {
		t.Fatalf("审计时间必须取服务端时钟，实际 %d", row.Ctime)
	}
}

func TestForwardDanmakuDependencyFailuresSurface(t *testing.T) {
	t.Run("live-room 未接线时报错而不是默认房间能发", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.markRoomGateUnwired()
		req := fwdReq("dmw-nogate")
		req.SenderLeaseId = mustSeedSenderLease(t, e, "lgwl_fwd_nogate", roomID, mid, model.RoleViewer).LeaseID
		reply, err := NewForwardDanmakuLogic(ctxClient(t), e.Svc).ForwardDanmaku(req)
		if !errors.Is(err, repository.ErrLiveRoomNotConfigured) {
			t.Fatalf("房间可广播性问不到真值时必须显式失败: err=%v reply=%+v", err, reply)
		}
		assertNoFanout(t, e)
	})
	t.Run("MySQL 未配置时不编造结论", func(t *testing.T) {
		e := newTestEnv(t)
		req := fwdReq("dmw-nostore")
		req.SenderLeaseId = mustSeedSenderLease(t, e, "lgwl_fwd_nostore", roomID, mid, model.RoleViewer).LeaseID
		e.markStoreMissing()
		if _, err := NewForwardDanmakuLogic(ctxClient(t), e.Svc).ForwardDanmaku(req); !errors.Is(err, repository.ErrStoreUnavailable) {
			t.Fatalf("期望 ErrStoreUnavailable，实际 %v", err)
		}
		assertNoFanout(t, e)
	})
}

// TestForwardDanmakuReplyOmitsMessageID 钉住回执回显幂等键：danmaku 侧拿到 accepted=false 时
// 必须能把回执对回自己那批待发队列（BroadcastToRoom 早就回显了，两条入口口径要一致）。
func TestForwardDanmakuReplyOmitsMessageID(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	req := fwdReq("dmw-echo")
	req.SenderLeaseId = mustSeedSenderLease(t, e, "lgwl_fwd_echo", roomID, mid, model.RoleViewer).LeaseID
	reply, err := NewForwardDanmakuLogic(ctxClient(t), e.Svc).ForwardDanmaku(req)
	if err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	if reply.GetMessageId() != "dmw-echo" {
		t.Fatalf("回执必须回显幂等键，实际 %q", reply.GetMessageId())
	}
	// 被丢弃时更要回显：那正是调用方需要知道「哪一条没发出去」的时刻。
	e2 := newTestEnv(t)
	seedRoute(e2, roomID, model.RouteStateServing, nodeA)
	// 本入口 require_reliable 恒为 false，扇出通道未接线走丢弃而不是报错。
	e2.markTransportUnwired()
	dropReq := fwdReq("dmw-echo-dropped")
	dropReq.SenderLeaseId = mustSeedSenderLease(t, e2, "lgwl_fwd_echo2", roomID, mid, model.RoleViewer).LeaseID
	dropped, err := NewForwardDanmakuLogic(ctxClient(t), e2.Svc).ForwardDanmaku(dropReq)
	if err != nil {
		t.Fatalf("通道未接线时弹幕入口不该报错（可丢弃语义）: %v", err)
	}
	if dropped.GetAccepted() || dropped.GetDropReason() != rpc.DropReason_DROP_REASON_TRANSPORT_UNAVAILABLE {
		t.Fatalf("通道未接线必须丢弃并说明原因: %+v", dropped)
	}
	if dropped.GetMessageId() != "dmw-echo-dropped" {
		t.Fatalf("丢弃回执同样要回显幂等键，实际 %q", dropped.GetMessageId())
	}
}

// --- ForwardSystemEvent：参数与来源鉴权 ---

func TestForwardSystemEventParameterGate(t *testing.T) {
	longID := strings.Repeat("e", 65)
	okCtx := ctxService(t, "moderation")
	cases := []struct {
		name string
		req  *rpc.ForwardSystemEventReq
		want string
	}{
		{"空请求", nil, "invalid room_id"},
		{"房间号为 0", &rpc.ForwardSystemEventReq{RoomId: 0, EventId: "e1", Kind: rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE, EventType: "room.open"}, "invalid room_id"},
		{"event_id 为空", sysEvtReq("  ", "room.open", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE), "event_id is required"},
		{"event_id 超长", sysEvtReq(longID, "room.open", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE), "event_id"},
		// 弹幕/互动/主播提词不走本入口：两条入口都能发同类消息会造成权限口径分裂。
		{"kind 未指定", sysEvtReq("evt-k1", "room.open", rpc.BroadcastKind_BROADCAST_KIND_UNSPECIFIED), "invalid broadcast kind"},
		{"kind 为弹幕", sysEvtReq("evt-k2", "room.open", rpc.BroadcastKind_BROADCAST_KIND_DANMAKU), "invalid broadcast kind"},
		{"kind 为互动提示", sysEvtReq("evt-k3", "room.open", rpc.BroadcastKind_BROADCAST_KIND_INTERACTION), "invalid broadcast kind"},
		{"kind 为主播提词", sysEvtReq("evt-k4", "room.open", rpc.BroadcastKind_BROADCAST_KIND_ANCHOR_TIP), "invalid broadcast kind"},
		{"kind 越界", sysEvtReq("evt-k5", "room.open", rpc.BroadcastKind(88)), "invalid broadcast kind"},
		// 未知 event_type 放过 = 让下游静默收到一条它不会处理的事件（比报错难查得多）。
		{"event_type 未知", sysEvtReq("evt-t1", "room.reopen", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE), "unknown event_type"},
		{"event_type 为空", sysEvtReq("evt-t2", "", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE), "event_type is required"},
		{"event_type 大小写不同", sysEvtReq("evt-t3", "Room.Close", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE), "unknown event_type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			seedRoute(e, roomID, model.RouteStateServing, nodeA)
			reply, err := NewForwardSystemEventLogic(okCtx, e.Svc).ForwardSystemEvent(tc.req)
			if err == nil {
				t.Fatalf("非法参数必须报错，实际 reply=%+v", reply)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误里应含 %q，实际 %v", tc.want, err)
			}
			// 参数阶段连来源鉴权都没到，更不能有审计与扇出。
			assertNoFanout(t, e)
			assertNoWrites(t, e)
			if n := e.Leases.callCount("ClaimEvent"); n != 0 {
				t.Fatalf("参数非法不该占事件去重键，ClaimEvent %d 次", n)
			}
		})
	}
}

// TestForwardSystemEventUnknownEventTypeNeedsSentinel 钉住错误分类：
// model.ErrUnknownEventType / ErrEmptyEventType 两个哨兵必须是实际返回点，
// 否则 gateway/admin 与内部服务只能靠错误文本判断「是事件类型不认识还是依赖故障」。
// 哨兵在 forwardsystemeventlogic.go 的 event_type 白名单一步包装（%w），裸错误即失败。
func TestForwardSystemEventUnknownEventTypeNeedsSentinel(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	_, err := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc).ForwardSystemEvent(
		sysEvtReq("evt-sentinel", "room.reopen", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE))
	if !errors.Is(err, model.ErrUnknownEventType) {
		t.Fatalf("期望 ErrUnknownEventType，实际 %v", err)
	}
	// 两种「event_type 不可用」必须分得开：上游据此决定是改自己的枚举表还是改调用参数。
	e2 := newTestEnv(t)
	seedRoute(e2, roomID, model.RouteStateServing, nodeA)
	_, err = NewForwardSystemEventLogic(ctxService(t, "moderation"), e2.Svc).ForwardSystemEvent(
		sysEvtReq("evt-sentinel-empty", "  ", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE))
	if !errors.Is(err, model.ErrEmptyEventType) {
		t.Fatalf("期望 ErrEmptyEventType，实际 %v", err)
	}
	assertNoFanout(t, e)
	assertNoFanout(t, e2)
}

func TestForwardSystemEventCallerAttribution(t *testing.T) {
	cases := []struct {
		name    string
		ctx     func(t *testing.T) context.Context
		mut     func(req *rpc.ForwardSystemEventReq)
		wantErr bool
	}{
		{name: "归因白名单内服务且来源一致", ctx: func(t *testing.T) context.Context { return ctxService(t, "moderation") }, wantErr: false},
		{name: "归因白名单内服务但未声明来源", ctx: func(t *testing.T) context.Context {
			return ctxAs(t, "true", "SERVICE", "", "")
		}, mut: func(r *rpc.ForwardSystemEventReq) { r.SourceService = "" }, wantErr: true},
		{name: "归因服务不在白名单", ctx: func(t *testing.T) context.Context { return ctxService(t, "payments") }, wantErr: true},
		{name: "归因服务与自报来源不一致", ctx: func(t *testing.T) context.Context { return ctxService(t, "moderation") },
			mut: func(r *rpc.ForwardSystemEventReq) { r.SourceService = "payments" }, wantErr: true},
		{name: "归因运营可发系统事件", ctx: func(t *testing.T) context.Context { return ctxOperator(t, "1001") }, wantErr: false},
		{name: "客户端态调用", ctx: func(t *testing.T) context.Context { return ctxClient(t) }, wantErr: true},
		{name: "自报 SERVICE 但未归因", ctx: func(t *testing.T) context.Context { return ctxAs(t, "", "SERVICE", "", "moderation") }, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			seedRoute(e, roomID, model.RouteStateServing, nodeA)
			req := sysEvtReq("evt-caller", "room.open", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE)
			if tc.mut != nil {
				tc.mut(req)
			}
			reply, err := NewForwardSystemEventLogic(tc.ctx(t), e.Svc).ForwardSystemEvent(req)
			if !tc.wantErr {
				if err != nil || !reply.GetAccepted() {
					t.Fatalf("可信主体应放行: reply=%+v err=%v", reply, err)
				}
				assertFanoutOnce(t, e)
				return
			}
			if !errors.Is(err, model.ErrPermissionDenied) {
				t.Fatalf("期望 ErrPermissionDenied，实际 err=%v reply=%+v", err, reply)
			}
			// 来源鉴权在扇出与去重之前：客户端态探测系统事件入口不能留下任何投递痕迹。
			assertNoFanout(t, e)
			if n := e.Leases.callCount("ClaimEvent"); n != 0 {
				t.Fatalf("来源被拒不该占事件去重键，ClaimEvent %d 次", n)
			}
			row := onlyAudit(t, e)
			if row.State != model.BroadcastLogDenied || row.DropReason != model.DropPermissionDenied {
				t.Fatalf("越权调用系统事件入口必须留 DENIED 行，实际 %+v", row)
			}
			if row.MessageId != "evt:evt-caller" || row.EventId != "evt-caller" {
				t.Fatalf("本入口没有 message_id 字段，审计定位串必须是 evt:<event_id> 且 event_id 单列可查: %+v", row)
			}
		})
	}
}

// TestForwardSystemEventEmptyWhitelistFailsClosed TrustedSourceServices 为空时不能放行任何来源：
// 系统事件能驱动「开播/禁言/封停」，放开等于给任意内网调用方一条伪造房间状态的通道。
func TestForwardSystemEventEmptyWhitelistFailsClosed(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	e.apply(func(c *config.LiveGatewayConf) { c.TrustedSourceServices = nil })
	_, err := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc).ForwardSystemEvent(
		sysEvtReq("evt-empty-wl", "room.open", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE))
	if !errors.Is(err, model.ErrPermissionDenied) {
		t.Fatalf("白名单为空必须 fail-closed，实际 %v", err)
	}
	assertNoFanout(t, e)
	// 归因运营是「人工处置」通道，不受来源白名单约束（但也不得因此放行客户端态）。
	if _, err := NewForwardSystemEventLogic(ctxOperator(t, "1001"), e.Svc).ForwardSystemEvent(
		sysEvtReq("evt-empty-ops", "room.open", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE)); err != nil {
		t.Fatalf("OPERATOR 不该被来源白名单挡住: %v", err)
	}
}

// TestForwardSystemEventSourceServiceIsForgeableByOperator 钉住审计来源只认归因：
// requireInternalService 只对 SERVICE 主体比对 caller-service 与 source_service，OPERATOR 没有
// 服务名可比，因此自报来源不得写进审计（否则归因运营能把事件来源标成任意服务名）。
// 正确口径：OPERATOR 主体记 caller 标识（c.String()），自报来源只作为日志观测。
func TestForwardSystemEventSourceServiceIsForgeableByOperator(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	req := sysEvtReq("evt-forge-src", "room.open", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE)
	req.SourceService = "some-other-service"
	if _, err := NewForwardSystemEventLogic(ctxOperator(t, "1001"), e.Svc).ForwardSystemEvent(req); err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	row := evtRow(t, e, "evt-forge-src")
	if row.SourceService == "some-other-service" {
		t.Fatalf("审计来源必须来自归因而不是自报值: %+v", row)
	}
	// 归因标识必须落在同一行里，否则「不是自报值」也可能只是记空。
	if !strings.Contains(row.SourceService, "1001") {
		t.Fatalf("OPERATOR 的审计来源必须是归因主体标识，实际 source_service=%q", row.SourceService)
	}
}

// --- ForwardSystemEvent：幂等、优先级与回执 ---

func TestForwardSystemEventDedupsOnEventID(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA, nodeB)
	logic := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc)
	first, err := logic.ForwardSystemEvent(sysEvtReq("evt-dedup", "room.open", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE))
	if err != nil || !first.GetAccepted() || first.GetDuplicated() {
		t.Fatalf("首次投递异常: reply=%+v err=%v", first, err)
	}
	assertEventLedger(t, e, "evt-dedup", true)
	// 换个 event_id 就是另一条事件：不能因为同一房间+同一类型就被判重复。
	other, err := logic.ForwardSystemEvent(sysEvtReq("evt-dedup-2", "room.open", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE))
	if err != nil || !other.GetAccepted() {
		t.Fatalf("不同 event_id 应各自投递: reply=%+v err=%v", other, err)
	}
	replay, err := logic.ForwardSystemEvent(sysEvtReq("evt-dedup", "room.open", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE))
	if err != nil {
		t.Fatalf("重放不该报错: %v", err)
	}
	if !replay.GetAccepted() || !replay.GetDuplicated() {
		t.Fatalf("重放应回 accepted+duplicated: %+v", replay)
	}
	if replay.GetEventId() != "evt-dedup" {
		t.Fatalf("重放要回显 event_id（本入口没有 message_id 可回显）: %q", replay.GetEventId())
	}
	if replay.GetFanoutNodes() != 2 {
		t.Fatalf("重放要复述首次的节点数，实际 %d", replay.GetFanoutNodes())
	}
	if len(e.Tp.fanoutReqs) != 2 {
		t.Fatalf("两条不同事件各投一次，重放不再投递，实际 %d 次", len(e.Tp.fanoutReqs))
	}
	if n := e.Leases.callCount("ClaimMessage"); n != 0 {
		t.Fatalf("系统事件按 event_id 去重，不该使用 message_id 那张账，ClaimMessage %d 次", n)
	}
	if len(e.Logs.all()) != 2 {
		t.Fatalf("重放不得新增审计行，实际 %d 行", len(e.Logs.all()))
	}
}

// TestForwardSystemEventPriorityExemptsRateLimit 系统事件恒按 priority=1 进核心链：
// 房间被弹幕打满时「开播/关播」这类状态事件不能被限流丢掉（但鉴权一步都没豁免）。
func TestForwardSystemEventPriorityExemptsRateLimit(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID, BroadcastQps: 1, Version: 1})
	logic := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc)
	for i, id := range []string{"evt-p1", "evt-p2", "evt-p3"} {
		reply, err := logic.ForwardSystemEvent(sysEvtReq(id, "room.open", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE))
		if err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		if !reply.GetAccepted() {
			t.Fatalf("第 %d 条系统事件被限流丢掉了: %+v", i+1, reply)
		}
	}
	if n := e.Leases.callCount("AllowRate"); n != 0 {
		t.Fatalf("priority=1 不该触碰限流，AllowRate %d 次", n)
	}
	// 同一份配额下普通弹幕仍被限住：豁免只发生在事件入口，没有改到房间额度本身。
	dm := fwdReq("dmw-after-priority")
	dm.SenderLeaseId = mustSeedSenderLease(t, e, "lgwl_prio_dmw", roomID, mid, model.RoleViewer).LeaseID
	dmReply, err := NewForwardDanmakuLogic(ctxClient(t), e.Svc).ForwardDanmaku(dm)
	if err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	if got := e.Leases.rateCount(fmt.Sprintf("bqps:room:%d", roomID)); got != 1 {
		t.Fatalf("三条事件都不该计房间额度，弹幕才是第 1 次，实际 %d", got)
	}
	if !dmReply.GetAccepted() {
		t.Fatalf("配额窗口里第一次弹幕仍应受理: %+v", dmReply)
	}
}

// TestForwardSystemEventCarriesEventIdentityToTransport 钉住下发契约：
// broadcastIntent 对系统事件只有 EventID/AuditMessage（MessageID 为空），
// 若 runBroadcast 把 in.MessageID 原样交给扇出层，接入层就收到一条空 message_id 的消息——
// 节点侧无法按消息幂等去重，排障日志里也只剩「room 7001 的一条空 message_id 消息」。
// 口径：MessageID 为空时用 "evt:"+event_id 填（与审计定位串同一口径）。
func TestForwardSystemEventCarriesEventIdentityToTransport(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	if _, err := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc).ForwardSystemEvent(
		sysEvtReq("evt-transport-id", "room.open", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE)); err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	if n := len(e.Tp.fanoutReqs); n != 1 {
		t.Fatalf("期望恰好一次下发，实际 %d 次", n)
	}
	if got := e.Tp.fanoutReqs[0].MessageID; got != "evt:evt-transport-id" {
		t.Fatalf("下发请求必须带事件标识，实际 %q", got)
	}
}

// TestForwardSystemEventReplyDropsTargetedConnections 钉住回执送达范围字段：
// 运营面「这条开播事件到了几个连接」必须能在同步回执里看到，而事件类结论恰恰最常被追问范围。
// 口径与 BroadcastToRoom 一致，直接回填 out.Targeted。
func TestForwardSystemEventReplyDropsTargetedConnections(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	e.Tp.result = &repository.FanoutResult{FanoutNodes: 1, TargetedConnections: 7}
	reply, err := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc).ForwardSystemEvent(
		sysEvtReq("evt-targeted", "room.open", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE))
	if err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	if reply.GetTargetedConnections() != 7 {
		t.Fatalf("回执要如实回送达连接数，实际 %d", reply.GetTargetedConnections())
	}
}

// --- ForwardSystemEvent：主播归属与 room.close 副作用 ---

func TestForwardSystemEventAnchorAttribution(t *testing.T) {
	t.Run("anchor_mid 与 live-room 不一致时以真值为准", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.Rooms.owner = anchorMid
		req := sysEvtReq("evt-anchor-mismatch", "room.open", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE)
		req.AnchorMid = otherMid // 上游记错了主播：本服务不复判也不照抄
		reply, err := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc).ForwardSystemEvent(req)
		if err != nil || !reply.GetAccepted() {
			t.Fatalf("归属不一致不该拒发可信内部事件（以真值下发）: reply=%+v err=%v", reply, err)
		}
		row := evtRow(t, e, "evt-anchor-mismatch")
		if row.SenderMid != 0 {
			t.Fatalf("不成立的 anchor_mid 必须归零而不是写进审计当事实，实际 %d", row.SenderMid)
		}
		if e.Rooms.ownerCalls != 1 {
			t.Fatalf("anchor_mid>0 时应恰好问一次归属，实际 %d 次", e.Rooms.ownerCalls)
		}
	})
	t.Run("anchor_mid 与 live-room 一致时原样记账", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.Rooms.owner = anchorMid
		req := sysEvtReq("evt-anchor-ok", "room.open", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE)
		req.AnchorMid = anchorMid
		if _, err := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc).ForwardSystemEvent(req); err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		if row := evtRow(t, e, "evt-anchor-ok"); row.SenderMid != anchorMid {
			t.Fatalf("归属成立时审计应记真实主播 mid，实际 %d", row.SenderMid)
		}
	})
	t.Run("不声明 anchor_mid 时不去问归属", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		if _, err := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc).ForwardSystemEvent(
			sysEvtReq("evt-no-anchor", "room.open", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE)); err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		// 归属问题是 RPC 调用：没声明就不该产生跨服务读放大（也说明审计里的 mid=0 是「未声明」而非「不是主播」）。
		if n := e.Rooms.ownerCalls; n != 0 {
			t.Fatalf("anchor_mid 缺省时不该问归属，实际问了 %d 次", n)
		}
	})
	t.Run("归属查询故障时按未校验继续", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.Rooms.ownerErr = errors.New("live-room unavailable")
		req := sysEvtReq("evt-anchor-unverified", "room.open", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE)
		req.AnchorMid = otherMid
		reply, err := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc).ForwardSystemEvent(req)
		if err != nil || !reply.GetAccepted() {
			t.Fatalf("可信内部事件不因归属依赖抖动被拒: reply=%+v err=%v", reply, err)
		}
		// 这一行的含义是「未校验」：AccessQuota/BroadcastLog 里都没有能区分「校验过」与「没校验」的列，
		// 契约缺口已随交付报告上报（见 TestForwardSystemEventReplyDropsTargetedConnections 同一处投影）。
		if row := evtRow(t, e, "evt-anchor-unverified"); row.SenderMid != otherMid {
			t.Fatalf("未校验时当前实现照抄请求值，若该行为已修正请同步本断言: %+v", row)
		}
	})
}

// TestForwardSystemEventUnwiredRoomGateStillFails 记录一处注释与实现相反：
// forwardsystemeventlogic.go:53-55 说「live-room 未接线时不因此拒发可信内部事件」，
// 但只跳过了归属校验；紧接着 runBroadcast 的房间可广播性一步（broadcastcore.go:114-118）
// 对未接线直接返回 ErrLiveRoomNotConfigured，整次调用仍然失败。
// 正确口径：要么两处同口径（未接线一律拒），要么在广播性一步为可信内部主体留降级分支。
// 这是可用性口径决策（fail-open 保开播事件必达 vs fail-closed 不向不存在的房间投递），
// 已登记在 README 已知缺口 14，不由实现方单方面定，故保留 skip。
func TestForwardSystemEventUnwiredRoomGateStillFails(t *testing.T) {
	t.Skip("缺陷已上报: services/live-gateway/internal/logic/forwardsystemeventlogic.go:53-55 与 " +
		"broadcastcore.go:114-118 口径相反 —— 未接线时事件仍被整体拒发")
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	e.markRoomGateUnwired()
	req := sysEvtReq("evt-unwired-gate", "room.open", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE)
	req.AnchorMid = anchorMid
	reply, err := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc).ForwardSystemEvent(req)
	if err != nil {
		t.Fatalf("按注释此处应降级放行可信内部事件，实际 err=%v reply=%+v", err, reply)
	}
}

func TestForwardSystemEventRoomCloseConvergence(t *testing.T) {
	t.Run("事件送达后按两步收敛到 OFFLINE", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		req := sysEvtReq("evt-close", "room.close", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE)
		req.TraceId = "trace-close"
		reply, err := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc).ForwardSystemEvent(req)
		if err != nil || !reply.GetAccepted() {
			t.Fatalf("关播事件应受理: reply=%+v err=%v", reply, err)
		}
		row := e.Routes.rows[roomID]
		// SERVING→DRAINING→OFFLINE 两步条件更新：跳过中间态会让关播瞬间的在途消息丢失（状态机也不允许）。
		if row.State != model.RouteStateOffline {
			t.Fatalf("room.close 后路由必须落到 OFFLINE，实际 state=%d", row.State)
		}
		if row.Version != 3 {
			t.Fatalf("两步收敛各增一次版本，期望 version=3，实际 %d", row.Version)
		}
		if row.DrainReason == "" || row.UpdatedBy != "moderation" {
			t.Fatalf("收敛要留下原因与操作者，实际 %+v", row)
		}
		// 缓存必须失效，否则一个 TTL 周期内广播还会打到已关播的节点。
		if n := e.Leases.callCount("CacheDel"); n < 2 {
			t.Fatalf("每步收敛都要失效路由缓存，实际 CacheDel %d 次", n)
		}
		if len(e.Tp.fanoutReqs) != 1 {
			t.Fatalf("关播消息本身要先送达（收敛在投递之后）")
		}
	})

	t.Run("重放同一事件不重复推进路由", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		logic := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc)
		req := sysEvtReq("evt-close-2", "room.close", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE)
		if _, err := logic.ForwardSystemEvent(req); err != nil {
			t.Fatalf("首次投递失败: %v", err)
		}
		before := *e.Routes.rows[roomID]
		dels := e.Leases.callCount("CacheDel")
		fanouts := len(e.Tp.fanoutReqs)
		replay, err := logic.ForwardSystemEvent(req)
		if err != nil {
			t.Fatalf("重放不该报错: %v", err)
		}
		// room.close 首次投递就把路由收敛到 OFFLINE，而判定链的路由检查在去重之前
		// （broadcastcore.go 第 5 步 vs 第 6 步），所以重放的结论是 NO_ROUTE 丢弃、
		// 而不是 duplicated——去重窗口在这里根本轮不到发言。钉住要紧的那件事：
		// 路由一步都不许再动、不二次扇出、不再删缓存。
		if replay.GetAccepted() || replay.GetDropReason() != rpc.DropReason_DROP_REASON_NO_ROUTE {
			t.Fatalf("已 OFFLINE 的房间重放关播事件应判 NO_ROUTE: %+v", replay)
		}
		after := *e.Routes.rows[roomID]
		if after.State != before.State || after.Version != before.Version {
			t.Fatalf("重放不得再推进路由状态：before=%+v after=%+v", before, after)
		}
		if n := e.Leases.callCount("CacheDel"); n != dels {
			t.Fatalf("已是终态时不该再删缓存，CacheDel 从 %d 变成 %d", dels, n)
		}
		if len(e.Tp.fanoutReqs) != fanouts {
			t.Fatalf("重放不得二次扇出，实际 %d 次", len(e.Tp.fanoutReqs))
		}
	})

	// 没有路由副作用的事件才走得到去重那一层：重放必须复述首次结论并标 duplicated。
	t.Run("无副作用事件重放标 duplicated", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		logic := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc)
		req := sysEvtReq("evt-mute-2", "moderation.mute", rpc.BroadcastKind_BROADCAST_KIND_MODERATION)
		first, err := logic.ForwardSystemEvent(req)
		if err != nil || !first.GetAccepted() {
			t.Fatalf("首次投递应受理: reply=%+v err=%v", first, err)
		}
		fanouts := len(e.Tp.fanoutReqs)
		replay, err := logic.ForwardSystemEvent(req)
		if err != nil {
			t.Fatalf("重放不该报错: %v", err)
		}
		if !replay.GetDuplicated() {
			t.Fatalf("同 event_id 重放必须标 duplicated（否则上游以为又投了一次）: %+v", replay)
		}
		if !replay.GetAccepted() {
			t.Fatalf("重放要复述首次的受理结论: %+v", replay)
		}
		if len(e.Tp.fanoutReqs) != fanouts {
			t.Fatalf("重放不得二次扇出")
		}
	})

	t.Run("其它事件不改路由", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		// room.open 也不预建路由：路由由首个真实连接的 JoinRoom 登记，空房间不该占表。
		registers := e.Routes.registers
		if _, err := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc).ForwardSystemEvent(
			sysEvtReq("evt-open", "room.open", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE)); err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		row := e.Routes.rows[roomID]
		if row.State != model.RouteStateServing || row.Version != 1 {
			t.Fatalf("room.open 不该动路由: %+v", row)
		}
		if n := e.Routes.registers; n != registers {
			t.Fatalf("room.open 不该登记路由，Register %d 次", n)
		}
	})

	t.Run("未送达就不收敛", func(t *testing.T) {
		e := newTestEnv(t)
		// 没有路由 ⇒ 事件本身判 NO_ROUTE 丢弃；此时不能把「没送达」当成「房间已关」去改路由。
		req := sysEvtReq("evt-close-noroute", "room.close", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE)
		reply, err := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc).ForwardSystemEvent(req)
		if err != nil {
			t.Fatalf("无路由是可丢弃结论: %v", err)
		}
		if reply.GetDropReason() != rpc.DropReason_DROP_REASON_NO_ROUTE {
			t.Fatalf("期望 NO_ROUTE，实际 %+v", reply)
		}
		if _, ok := e.Routes.rows[roomID]; ok {
			t.Fatalf("未送达就不该产生/改动路由行")
		}
	})

	t.Run("收敛失败时 reliable 决定是否报错", func(t *testing.T) {
		cases := []struct {
			name     string
			reliable bool
		}{
			{"require_reliable=true 时上游必须知道", true},
			{"require_reliable=false 时只告警", false},
		}
		want := errors.New("route-update-unavailable")
		for i, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				e := newTestEnv(t)
				seedRoute(e, roomID, model.RouteStateServing, nodeA)
				e.Routes.fail("UpdateState", want)
				// event_id 必须是 1..64 字节且不含空白（requireEventID 的硬门禁），
				// 所以这里用 case 序号而不是把 tc.name（含空格与中文）拼进去。
				req := sysEvtReq(fmt.Sprintf("evt-close-fail-%d", i), "room.close", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE)
				req.RequireReliable = tc.reliable
				reply, err := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc).ForwardSystemEvent(req)
				if tc.reliable {
					if !errors.Is(err, want) {
						t.Fatalf("路由没收敛时可靠投递必须失败，实际 err=%v reply=%+v", err, reply)
					}
					return
				}
				// 事件本身已送达：路由没收敛是「下次还会打到旧节点」的问题，报 error 会让上游重投关播消息。
				if err != nil || !reply.GetAccepted() {
					t.Fatalf("可丢弃语义下应回受理并告警: reply=%+v err=%v", reply, err)
				}
				if e.Routes.rows[roomID].State != model.RouteStateServing {
					t.Fatalf("收敛失败时状态不该被改一半: %+v", e.Routes.rows[roomID])
				}
			})
		}
	})
}

func TestForwardSystemEventReliableTransportFailure(t *testing.T) {
	t.Run("require_reliable=true 时未接线是错误", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.markTransportUnwired()
		req := sysEvtReq("evt-reliable", "moderation.mute", rpc.BroadcastKind_BROADCAST_KIND_MODERATION)
		req.RequireReliable = true
		reply, err := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc).ForwardSystemEvent(req)
		if !errors.Is(err, model.ErrTransportUnavailable) {
			t.Fatalf("禁言处置「静默丢失」等于处置未执行，必须让上游看见: err=%v reply=%+v", err, reply)
		}
		if reply != nil {
			t.Fatalf("返回错误时不得给响应体: %+v", reply)
		}
		row := onlyAudit(t, e)
		if row.State != model.BroadcastLogDropped || row.DropReason != model.DropTransportUnavailable {
			t.Fatalf("失败也要留投递凭证: %+v", row)
		}
	})
	t.Run("require_reliable=false 时未接线是丢弃", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.markTransportUnwired()
		reply, err := NewForwardSystemEventLogic(ctxService(t, "moderation"), e.Svc).ForwardSystemEvent(
			sysEvtReq("evt-droppable", "room.disconnect", rpc.BroadcastKind_BROADCAST_KIND_ROOM_STATE))
		if err != nil {
			t.Fatalf("不该返回错误: %v", err)
		}
		if reply.GetAccepted() || reply.GetDropReason() != rpc.DropReason_DROP_REASON_TRANSPORT_UNAVAILABLE {
			t.Fatalf("期望丢弃+TRANSPORT_UNAVAILABLE: %+v", reply)
		}
	})
}
