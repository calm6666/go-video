// 本文件覆盖 CloseStreamLogic（强制停流）。
//
// 要钉住的契约按重要性排序：
//  1. 写序——停流的三件收尾（健康归位 / 密钥活跃指针 / 租约+配额）与事件、Outbox 的先后
//     决定「中途失败时库里剩什么」，而次数断言区分不开「次数相同顺序不同」，
//     所以本文件用替身的有序调用留痕（fakeDB.callLog）整段锁死。
//  2. 状态可达集——IDLE/PUBLISHING/INTERRUPTED 可关，STOPPED 只能回放。
//  3. 守卫与预检必须在触库之前返回（零 SQL、零事务）。
//  4. 幂等锚点——终态才是判定锚点，request_id 只在「还要写」的那一侧生效。
//
// 与 README「已知缺口」的关系：网关是 stub（缺口 1）、发布器未接线（缺口 2）、
// 停流不清 node_id（缺口 #1，已 Skip）都不在这里重复登记；本文件只登记新发现的口径问题。
package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/live-ingest/internal/config"
	"go-video/services/live-ingest/internal/repository"
	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// --- 夹具 ---

// closeFixture 是一条「正在推流、占着密钥活跃位、占着某节点一个配额与一条生效租约」的流。
// 停流的收尾断言必须从这种完整形态出发：任何一环缺席都会让「少写一步」看起来是对的。
type closeFixture struct {
	key       *model.StreamKey
	stream    *model.Stream
	node      *model.IngestNode
	assignReq string
}

func (f closeFixture) lease(t *testing.T, e *testEnv) *model.NodeAssignment {
	t.Helper()
	return e.assignmentRow(t, f.assignReq)
}

// seedCloseFixture 铺一条 PUBLISHING 形态的完整流；state/seq 由调用方在 s 里覆盖。
func (e *testEnv) seedCloseFixture(t *testing.T, streamID string, state int32, seq int64) closeFixture {
	t.Helper()
	key := e.seedKey(t, &model.StreamKey{
		StreamName: "live_close_" + streamID, RoomID: 71, AnchorMid: 1001, Version: 1,
		ProtocolMask: model.ProtocolMaskRtmp, MaxStreams: 2,
	})
	fx := closeFixture{key: key, assignReq: "assign-" + streamID}
	fx.stream = e.streamRow(t, e.seedAssignedNode(t,
		&model.Stream{StreamID: streamID, RoomID: 71, AnchorMid: 1001, KeyID: key.KeyID,
			Protocol: 1, State: state, Seq: seq, NodeID: "node-close"},
		&model.IngestNode{NodeID: "node-close", ProtocolMask: model.ProtocolMaskRtmp,
			CapacityStreams: 3, HealthScore: 88, ActiveStreams: 0},
		fx.assignReq).StreamID)
	fx.node = e.nodeRow(t, "node-close")
	return fx
}

// openWindow 给一条 INTERRUPTED 流铺「进行中的断流区间」，started_at 固定在 ago 秒前，
// 这样「区间收尾」与「累计中断秒数」都是可证伪的具体数字而不是「大于 0」。
func (e *testEnv) openWindow(t *testing.T, streamID string, roomID int64, nodeID string, ago int64) *model.StreamInterruption {
	t.Helper()
	return e.seedInterruption(t, &model.StreamInterruption{
		StreamID: streamID, RoomID: roomID, EpisodeNo: 1, NodeID: nodeID,
		StartedAt: nowUnix() - ago, StartEventID: "EVT-OPEN-" + streamID, Reason: "心跳丢失",
	})
}

// --- 依赖缺席与守卫：必须零触库 ---

func TestCloseStream_NoRepositoryIsError(t *testing.T) {
	e := newTestEnv(t)
	mark, before := e.markCalls(), e.effects()
	reply, err := NewCloseStreamLogic(bg(), e.withoutRepository()).
		CloseStream(&rpc.CloseStreamReq{StreamId: "S-ANY", RequestId: "c-any", OperatorMid: 9001, Admin: true})
	wantFail(t, err, errNoRepository, "无 Repository")
	if reply != nil {
		t.Fatalf("无 Repository 时不得返回响应体（零值响应会被上游当成停流成功）：%+v", reply)
	}
	e.requireNoCallsAfter(t, mark, "无 Repository")
	e.requireSameEffects(t, before, "无 Repository")
}

// TestCloseStream_GuardsTouchNothing 钉住「入参不合法就连一条 SQL 都不该发出去」：
// 停流是运营在故障时按的按钮，把校验拖到事务里等于在数据库抖动时让按钮更慢。
func TestCloseStream_GuardsTouchNothing(t *testing.T) {
	longID := strings.Repeat("x", maxStreamRefBytes+1)
	longReq := strings.Repeat("r", maxIdempotencyKeyBytes+1)
	longReason := strings.Repeat("理", maxReasonRunes+1)
	cases := []struct {
		name   string
		want   error
		mut    func(*rpc.CloseStreamReq)
		reason string
	}{
		{"空 stream_id", model.ErrInvalidStreamId, func(in *rpc.CloseStreamReq) { in.StreamId = "" }, ""},
		{"stream_id 含空格", model.ErrInvalidStreamId, func(in *rpc.CloseStreamReq) { in.StreamId = "S A" }, ""},
		{"stream_id 含斜杠", model.ErrInvalidStreamId, func(in *rpc.CloseStreamReq) { in.StreamId = "S/A" }, ""},
		{"stream_id 超列宽", model.ErrInvalidStreamId, func(in *rpc.CloseStreamReq) { in.StreamId = longID }, ""},
		{"空 request_id", model.ErrIdempotencyKeyRequired, func(in *rpc.CloseStreamReq) { in.RequestId = "" },
			"没有幂等键的写重放就是两次副作用"},
		{"request_id 全空格", model.ErrIdempotencyKeyRequired, func(in *rpc.CloseStreamReq) { in.RequestId = "   " }, ""},
		{"request_id 超列宽", model.ErrIdempotencyKeyRequired, func(in *rpc.CloseStreamReq) { in.RequestId = longReq }, ""},
		{"operator_mid=0", model.ErrOperatorRequired, func(in *rpc.CloseStreamReq) { in.OperatorMid = 0 },
			"没有归因主体的写不可受理"},
		{"operator_mid 为负", model.ErrOperatorRequired, func(in *rpc.CloseStreamReq) { in.OperatorMid = -7 }, ""},
		{"reason 超长", model.ErrReasonTooLong, func(in *rpc.CloseStreamReq) { in.Reason = longReason },
			"超长必须拒绝而不是截断，否则审计文本与库值不一致"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			fx := e.seedCloseFixture(t, "S-GUARD", model.StreamStatePublishing, 1)
			mark, before := e.markCalls(), e.effects()
			in := &rpc.CloseStreamReq{StreamId: fx.stream.StreamID, RequestId: "c-ok", OperatorMid: 9001, Admin: true}
			c.mut(in)
			_, err := NewCloseStreamLogic(bg(), e.svc).CloseStream(in)
			wantFail(t, err, c.want, c.name)
			e.requireNoCallsAfter(t, mark, c.name)
			e.requireSameEffects(t, before, c.name)
			e.requireNoTransaction(t, before, c.name)
		})
	}
}

// TestCloseStream_GuardOrderIsStable 钉住守卫次序（stream → request → operator → reason）：
// 次序会决定「一个全错的请求为什么错」，排错时调用方按第一个错改就行。
func TestCloseStream_GuardOrderIsStable(t *testing.T) {
	e := newTestEnv(t)
	mark := e.markCalls()
	// 四项全非法：只有 stream_id 的错会被报出来。
	_, err := NewCloseStreamLogic(bg(), e.svc).CloseStream(&rpc.CloseStreamReq{
		StreamId: "", RequestId: "", OperatorMid: 0, Reason: strings.Repeat("理", maxReasonRunes+1)})
	wantFail(t, err, model.ErrInvalidStreamId, "全非法")
	_, err = NewCloseStreamLogic(bg(), e.svc).CloseStream(&rpc.CloseStreamReq{
		StreamId: "S-OK", RequestId: "", OperatorMid: 0, Reason: strings.Repeat("理", maxReasonRunes+1)})
	wantFail(t, err, model.ErrIdempotencyKeyRequired, "stream 合法后看 request")
	_, err = NewCloseStreamLogic(bg(), e.svc).CloseStream(&rpc.CloseStreamReq{
		StreamId: "S-OK", RequestId: "c-1", OperatorMid: 0, Reason: strings.Repeat("理", maxReasonRunes+1)})
	wantFail(t, err, model.ErrOperatorRequired, "request 合法后看 operator")
	_, err = NewCloseStreamLogic(bg(), e.svc).CloseStream(&rpc.CloseStreamReq{
		StreamId: "S-OK", RequestId: "c-1", OperatorMid: 9001, Reason: strings.Repeat("理", maxReasonRunes+1)})
	wantFail(t, err, model.ErrReasonTooLong, "最后才是 reason")
	e.requireNoCallsAfter(t, mark, "守卫次序")
}

func TestCloseStream_UnknownStreamIsNotFoundWithoutTransaction(t *testing.T) {
	e := newTestEnv(t)
	mark, before := e.markCalls(), e.effects()
	_, err := e.closeStream(t, "S-MISSING", "c-missing")
	wantFail(t, err, model.ErrStreamNotFound, "流不存在")
	// 只读了一次 live_stream：预检查不到就不该开事务。
	e.requireCallsFrom(t, mark, []string{"Stream.FindOne"}, "流不存在的触库形状")
	e.requireNoTransaction(t, before, "流不存在")
	e.requireSameEffects(t, before, "流不存在")
}

// --- 归属判定：admin 是唯一能关别人流的开关 ---

func TestCloseStream_OwnershipAdminDecidesNotOperatorIdentity(t *testing.T) {
	e := newTestEnv(t)
	e.seedCloseFixture(t, "S-OWNER", model.StreamStatePublishing, 1)
	before := e.effects()

	// 非运营 + 不是主播本人：拒绝，且只停在一次读上。
	mark := e.markCalls()
	_, err := e.closeStream(t, "S-OWNER", "c-other", func(in *rpc.CloseStreamReq) {
		in.OperatorMid = 2002
		in.Admin = false
	})
	wantFail(t, err, model.ErrOperatorRequired, "越权停流")
	e.requireCallsFrom(t, mark, []string{"Stream.FindOne"}, "越权停流的触库形状")
	e.requireSameEffects(t, before, "越权停流")

	// 判别对：同一 operator、同一 stream，只把 admin 打开就必须成功。
	e.mustCloseStream(t, "S-OWNER", "c-admin", func(in *rpc.CloseStreamReq) { in.OperatorMid = 2002 })
	if row := e.streamRow(t, "S-OWNER"); row.State != model.StreamStateStopped {
		t.Fatalf("admin=true 的运营切断未生效：state=%d", row.State)
	}

	// 判别对 2：主播本人（operator_mid == anchor_mid）不带 admin 也能关自己的流。
	fx2 := e.seedCloseFixture(t, "S-SELF", model.StreamStatePublishing, 1)
	e.mustCloseStream(t, "S-SELF", "c-self", func(in *rpc.CloseStreamReq) {
		in.OperatorMid = fx2.stream.AnchorMid
		in.Admin = false
	})
	if row := e.streamRow(t, "S-SELF"); row.State != model.StreamStateStopped {
		t.Fatalf("主播自关未生效：state=%d", row.State)
	}
}

// --- stop_reason 归一：UNSPECIFIED 时按「谁在关」补，而不是留 0 进事件 ---

func TestCloseStream_StopReasonNormalizedByActorWhenUnspecified(t *testing.T) {
	cases := []struct {
		name      string
		operator  int64
		admin     bool
		explicit  rpc.StopReason
		wantModel int32
	}{
		{"运营关别人的流", 2002, true, rpc.StopReason_STOP_REASON_UNSPECIFIED, model.StopReasonAdmin},
		{"主播关自己的流", 1001, true, rpc.StopReason_STOP_REASON_UNSPECIFIED, model.StopReasonAnchorStop},
		{"主播关自己的流（非运营）", 1001, false, rpc.StopReason_STOP_REASON_UNSPECIFIED, model.StopReasonAnchorStop},
		// 显式值优先：归一只填 UNSPECIFIED，绝不改写调用方声明的原因。
		{"显式 NODE_TIMEOUT 不被归一", 1001, false, rpc.StopReason_STOP_REASON_NODE_TIMEOUT, model.StopReasonNodeTimeout},
		{"显式 ADMIN 不被主播身份改写", 1001, true, rpc.StopReason_STOP_REASON_ADMIN, model.StopReasonAdmin},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			fx := e.seedCloseFixture(t, fmt.Sprintf("S-SR%d", i), model.StreamStatePublishing, 1)
			requestID := fmt.Sprintf("c-sr%d", i)
			reply := e.mustCloseStream(t, fx.stream.StreamID, requestID, func(in *rpc.CloseStreamReq) {
				in.OperatorMid, in.Admin, in.StopReason = c.operator, c.admin, c.explicit
			})
			if reply.GetState() != rpc.StreamState_STREAM_STATE_STOPPED {
				t.Fatalf("%s：响应状态必须是 STOPPED，实得 %v", c.name, reply.GetState())
			}
			row := e.streamRow(t, fx.stream.StreamID)
			if row.StopReason != c.wantModel {
				t.Fatalf("%s：live_stream.stop_reason 归一不符：got=%d want=%d", c.name, row.StopReason, c.wantModel)
			}
			// 事件列与流列必须同口径，否则按事件统计停流原因的报表会分裂。
			ev := e.eventRow(t, fx.stream.StreamID, row.Seq)
			if ev.StopReason != c.wantModel {
				t.Fatalf("%s：事件与流行的 stop_reason 不同源：event=%d stream=%d", c.name, ev.StopReason, row.StopReason)
			}
			if ev.ReportID != requestID {
				t.Fatalf("%s：事件的 report_id 必须是本次幂等键，实得 %q", c.name, ev.ReportID)
			}
		})
	}
}

// 缺陷:CloseStream 的 reason 不是必填，空原因可以让「为什么停」三处审计文本同时为空
//
// 事实：checkReason("reason", in.Reason, false) 允许空串，而 reason 同时是
// live_stream.stop_detail、live_stream_event.reason 与 live_node_assignment.release_reason
// 的唯一来源（closestreamlogic.go:50 + streamstate.go:145,213,248,275）。
// 后果：运营面板与租约释放记录只剩「谁停的」，没有「为什么停」，事后对账无从下手。
// 关键矛盾：ErrReasonRequired 自己的注释写着「破坏性写（吊销、**强制停流**）必须留归因文本」
// （model/errors.go:308-309），而强制停流这一路传的是 required=false——
// 同一个包内对同一件事有两种口径，吊销（revokestreamkeylogic.go:47）与
// 事件放行（retryfailedeventslogic.go:58）都是 true。
// 本用例不放宽、不 Skip，按现状钉死，等实现改为必填即成为回归防线。
func TestCloseStream_EmptyReasonLeavesAllAuditTextEmpty(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedCloseFixture(t, "S-NOREASON", model.StreamStatePublishing, 1)
	reply := e.mustCloseStream(t, "S-NOREASON", "c-noreason", func(in *rpc.CloseStreamReq) { in.Reason = "" })
	if !reply.GetApplied() {
		t.Fatalf("空原因不该让停流失败：replayed=%v", reply.GetReplayed())
	}
	row := e.streamRow(t, "S-NOREASON")
	ev := e.eventRow(t, "S-NOREASON", row.Seq)
	lease := fx.lease(t, e)
	if row.StopDetail != "" || ev.Reason != "" || lease.ReleaseReason != "" {
		t.Fatalf("空原因本应让三处审计文本都为空，实得 stop_detail=%q event.reason=%q release_reason=%q",
			row.StopDetail, ev.Reason, lease.ReleaseReason)
	}
	if row.StopReason != model.StopReasonAdmin {
		t.Fatalf("原因文本为空不影响 stop_reason 归一，实得 %d", row.StopReason)
	}
}

// --- 写序与落库效果（核心用例） ---

// TestCloseStream_WriteOrderForPublishingStream 整段锁死一次运营强制停流产生的 model 调用序列。
//
// 顺序为什么是契约而不是实现细节：
//   - MarkStopped 在 ApplyTransition 之后：健康归位的 SQL 带 state=STOPPED 条件，反过来就空转；
//   - ReleaseActiveStream 在租约释放之前：密钥活跃位先清，新的推流才不会被一个正在收尾的流挡住；
//   - NodeAssignment.Release 在 ReleaseQuota 之前：配额先退而租约 CAS 失败会整体回滚，
//     但反过来（先释放租约、后退配额）失败时能明确看出「租约没了配额还占着」，
//     现顺序失败则两行都没动，运维看到的偏差窗口更小；
//   - 事件与 Outbox 在最后：InterruptionID 只有区间开合后才知道，而 outbox.event_id 必须等于事件 event_id。
func TestCloseStream_WriteOrderForPublishingStream(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedCloseFixture(t, "S-ORDER", model.StreamStatePublishing, 4)
	before, mark := e.effects(), e.markCalls()

	reply := e.mustCloseStream(t, "S-ORDER", "c-order", func(in *rpc.CloseStreamReq) {
		in.Reason = "违规内容切断"
	})
	e.requireCallsFrom(t, mark, []string{
		"Stream.FindOne",                    // 事务外预检（存在性 + 归属）
		"Stream.LockByID", "Stream.FindOne", // 事务内行锁重读
		"Stream.ApplyTransition",            // state+seq 双条件 CAS
		"Stream.MarkStopped",                // 健康位归位
		"Stream.SetNode",                    // 清掉流行上的节点指针（与 ReleaseIngestNode 同口径）
		"StreamKey.ReleaseActiveStream",     // 密钥活跃指针
		"NodeAssignment.FindActiveByStream", // 找生效租约
		"NodeAssignment.Release",            // 租约 → RELEASED
		"IngestNode.ReleaseQuota",           // 配额归还
		"StreamEvent.Insert",                // 事件（含 report_id 幂等锚点）
		"Outbox.Insert",                     // live.state.v1 信封
		"Stream.FindOne",                    // 事务提交后回读回显
	}, "停流写序")

	// 应有写入恰好是「1 事件 + 1 outbox」，别的表只改不增。
	e.requireDelta(t, before, [9]int{0, 0, 0, 0, 1, 1, 0, 0, 0}, "停流写入增量")

	row := e.streamRow(t, "S-ORDER")
	if row.State != model.StreamStateStopped || row.Seq != 5 || row.StopReason != model.StopReasonAdmin ||
		row.StopDetail != "违规内容切断" || row.StateChangedAt <= 0 {
		t.Fatalf("停流落库不符：%+v", row)
	}
	if row.HealthState != model.HealthStateNoData {
		t.Fatalf("MarkStopped 未把健康位归位：%d", row.HealthState)
	}
	if got := e.keyRow(t, fx.key.KeyID).CurrentStreamID; got != "" {
		t.Fatalf("密钥活跃指针未释放：%s", got)
	}
	lease := fx.lease(t, e)
	if lease.State != model.AssignmentStateReleased || lease.ReleasedAt <= 0 ||
		lease.ReleaseReason != "违规内容切断" || lease.NodeID != "node-close" {
		t.Fatalf("租约释放不符：%+v", lease)
	}
	if got := e.nodeRow(t, "node-close").ActiveStreams; got != fx.node.ActiveStreams-1 {
		t.Fatalf("节点配额未归还：active=%d（停流前 %d）", got, fx.node.ActiveStreams)
	}
	ev := e.eventRow(t, "S-ORDER", 5)
	if ev.FromState != model.StreamStatePublishing || ev.ToState != model.StreamStateStopped ||
		ev.ReportID != "c-order" || ev.Source != model.EventSourceAdmin || ev.Seq != 5 {
		t.Fatalf("停流事件不符：%+v", ev)
	}
	if ev.EventID != reply.GetEventId() || ev.NodeID != "node-close" {
		t.Fatalf("事件与响应/节点不符：event_id=%s node=%s", ev.EventID, ev.NodeID)
	}
}

// TestCloseStream_LiveIngestOwnedCloseStillEmitsOutboxRow 钉住：
// 由 live-ingest 自己发起的关流（不是入口上报）也必须产出 live.state.v1 信封，
// 否则 live-room 的投影在「运营切断」这一类事件上永久落后。
func TestCloseStream_LiveIngestOwnedCloseStillEmitsOutboxRow(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedCloseFixture(t, "S-ENVELOPE", model.StreamStatePublishing, 1)
	reply := e.mustCloseStream(t, "S-ENVELOPE", "c-envelope", func(in *rpc.CloseStreamReq) { in.Reason = "风控" })

	pending, err := e.repo.Outbox.ListByState(bg(), model.OutboxStatePending, 10)
	wantOK(t, pending, err, "查询待发布")
	if len(pending) != 1 {
		t.Fatalf("停流必须写 1 行待发布 outbox，实得 %d", len(pending))
	}
	row := pending[0]
	if row.EventID != reply.GetEventId() || row.EventType != model.EventTypeStreamState ||
		row.SchemaVersion != model.SchemaVersionStreamState || row.AggregateType != model.AggregateTypeStream ||
		row.AggregateID != "S-ENVELOPE" || row.StreamID != "S-ENVELOPE" || row.Seq != 2 || row.RoomID != fx.stream.RoomID {
		t.Fatalf("outbox 行的路由字段不符：%+v", row)
	}
	var env struct {
		EventID       string          `json:"event_id"`
		EventType     string          `json:"event_type"`
		SchemaVersion int             `json:"schema_version"`
		OccurredAt    string          `json:"occurred_at"`
		Producer      string          `json:"producer"`
		AggregateType string          `json:"aggregate_type"`
		AggregateID   string          `json:"aggregate_id"`
		Payload       json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(row.Payload), &env); err != nil {
		t.Fatalf("payload 不是合法信封 JSON：%v", err)
	}
	if env.EventID != reply.GetEventId() || env.Producer != "live-ingest" || env.AggregateID != "S-ENVELOPE" ||
		env.EventType != model.EventTypeStreamState || env.SchemaVersion != model.SchemaVersionStreamState ||
		env.OccurredAt == "" {
		t.Fatalf("信封字段不符：%+v", env)
	}
	var body stateEventPayload
	if err := json.Unmarshal(env.Payload, &body); err != nil {
		t.Fatalf("业务负载解析失败：%v", err)
	}
	// 停流负载里必须带终态与新 seq：消费方按 (stream_id, seq) 拒绝回退。
	if body.StreamState != model.StreamStateStopped || body.StreamSeq != 2 || body.StreamID != "S-ENVELOPE" ||
		body.Reason != "风控" {
		t.Fatalf("停流负载口径不符：%+v", body)
	}
	// 隐私纪律：信封不含任何密钥材料。
	for _, leak := range []string{fx.key.KeyHash, "key_hash", "plaintext", "key_ref"} {
		if strings.Contains(row.Payload, leak) {
			t.Fatalf("事件信封里出现了密钥字段 %q", leak)
		}
	}
}

// TestCloseStream_NodesWithoutLeaseSkipReleasePaths 钉住「没有生效租约」时不能硬造一次释放：
// 配额是从租约派生的，找不到租约就不动节点计数，否则节点会被记少、后续分配超卖。
func TestCloseStream_NodesWithoutLeaseSkipReleasePaths(t *testing.T) {
	e := newTestEnv(t)
	key := e.seedKey(t, &model.StreamKey{StreamName: "live_no_lease", RoomID: 72, AnchorMid: 1001,
		Version: 1, ProtocolMask: model.ProtocolMaskRtmp})
	// IDLE 流：签发过但没推上来，既没租约也没配额。
	s := e.seedStream(t, &model.Stream{StreamID: "S-IDLE", RoomID: 72, AnchorMid: 1001, KeyID: key.KeyID,
		Protocol: 1, State: model.StreamStateIdle, Seq: 0, NodeID: "node-empty"})
	e.seedNode(t, &model.IngestNode{NodeID: "node-empty", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 3, HealthScore: 50})
	mark, before := e.markCalls(), e.effects()

	e.mustCloseStream(t, "S-IDLE", "c-idle")
	e.requireCallsFrom(t, mark, []string{
		"Stream.FindOne", "Stream.LockByID", "Stream.FindOne", "Stream.ApplyTransition",
		"Stream.MarkStopped", "Stream.SetNode", "StreamKey.ReleaseActiveStream", "NodeAssignment.FindActiveByStream",
		"StreamEvent.Insert", "Outbox.Insert", "Stream.FindOne",
	}, "无租约停流写序")
	if got := e.nodeRow(t, "node-empty").ActiveStreams; got != 0 {
		t.Fatalf("无租约却动了节点配额：active=%d", got)
	}
	e.requireDelta(t, before, [9]int{0, 0, 0, 0, 1, 1, 0, 0, 0}, "IDLE 停流增量")
	row := e.streamRow(t, s.StreamID)
	if row.State != model.StreamStateStopped || row.Seq != 1 {
		t.Fatalf("IDLE→STOPPED 未生效：state=%d seq=%d", row.State, row.Seq)
	}
}

// TestCloseStream_AllNonTerminalStatesAreClosable 给出「哪些状态可关」的完整答案：
// IDLE/PUBLISHING/INTERRUPTED 都能被停，且每次都是 seq+1 的真迁移。
func TestCloseStream_AllNonTerminalStatesAreClosable(t *testing.T) {
	cases := []struct {
		name     string
		state    int32
		seq      int64
		open     bool
		wantFrom int32
	}{
		{"IDLE 可关", model.StreamStateIdle, 0, false, model.StreamStateIdle},
		{"PUBLISHING 可关", model.StreamStatePublishing, 1, false, model.StreamStatePublishing},
		{"INTERRUPTED 可关", model.StreamStateInterrupted, 2, false, model.StreamStateInterrupted},
		{"INTERRUPTED（带进行中区间）可关", model.StreamStateInterrupted, 2, true, model.StreamStateInterrupted},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			id := fmt.Sprintf("S-MAT%d", i)
			fx := e.seedCloseFixture(t, id, c.state, c.seq)
			if c.open {
				e.openWindow(t, id, fx.stream.RoomID, "node-close", 30)
			}
			reply := e.mustCloseStream(t, id, fmt.Sprintf("c-mat%d", i))
			mustTrue(t, reply.GetApplied(), c.name+" applied")
			mustFalse(t, reply.GetReplayed(), c.name+" replayed")
			if reply.GetSeq() != c.seq+1 || reply.GetState() != rpc.StreamState_STREAM_STATE_STOPPED {
				t.Fatalf("%s：响应不符 seq=%d state=%v", c.name, reply.GetSeq(), reply.GetState())
			}
			ev := e.eventRow(t, id, c.seq+1)
			if ev.FromState != c.wantFrom || ev.ToState != model.StreamStateStopped {
				t.Fatalf("%s：迁移方向 %d→%d", c.name, ev.FromState, ev.ToState)
			}
			if got := e.nodeRow(t, "node-close").ActiveStreams; got != fx.node.ActiveStreams-1 {
				t.Fatalf("%s：配额未归还 active=%d", c.name, got)
			}
		})
	}
}

// --- 断流区间收尾（写序里的中间段） ---

// TestCloseStream_ClosesOpenInterruptionBeforeKeyAndLease 钉两件事：
//  1. 区间合上发生在 MarkStopped/ReleaseActiveStream/Release 之前（顺序见写序断言）；
//  2. end_reason 由 stop_reason 决定：主动切=CLOSED，节点超时=TIMEOUT——
//     这是「断流时长报表」区分「主播自己走了」与「节点掉了」的唯一依据。
func TestCloseStream_ClosesOpenInterruptionBeforeKeyAndLease(t *testing.T) {
	cases := []struct {
		name       string
		stopReason rpc.StopReason
		wantEnd    int32
	}{
		{"运营切断记 CLOSED", rpc.StopReason_STOP_REASON_ADMIN, model.InterruptionEndClosed},
		{"主播下播也记 CLOSED", rpc.StopReason_STOP_REASON_ANCHOR_STOP, model.InterruptionEndClosed},
		{"节点超时记 TIMEOUT", rpc.StopReason_STOP_REASON_NODE_TIMEOUT, model.InterruptionEndTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			fx := e.seedCloseFixture(t, "S-WIN", model.StreamStateInterrupted, 2)
			open := e.openWindow(t, "S-WIN", fx.stream.RoomID, "node-close", 45)
			mark, before := e.markCalls(), e.effects()

			reply := e.mustCloseStream(t, "S-WIN", "c-win", func(in *rpc.CloseStreamReq) { in.StopReason = c.stopReason })
			e.requireCallOrder(t, mark, []string{
				"Stream.ApplyTransition",
				"StreamInterruption.FindOpenByStream",
				"StreamInterruption.Close",
				"Stream.CloseInterruption",
				"Stream.MarkStopped",
				"StreamKey.ReleaseActiveStream",
				"NodeAssignment.FindActiveByStream",
				"NodeAssignment.Release",
				"IngestNode.ReleaseQuota",
				"StreamEvent.Insert",
				"Outbox.Insert",
			}, "区间收尾早于密钥与租约")

			row := e.streamRow(t, "S-WIN")
			// 区间行的 ended_at/duration/end_reason 与流上的累计秒数必须同源。
			window := e.db.interrupt[open.InterruptionID]
			if window.EndedAt <= window.StartedAt {
				t.Fatalf("区间未闭合或时间倒挂：started=%d ended=%d", window.StartedAt, window.EndedAt)
			}
			if window.DurationSeconds < 44 || window.DurationSeconds > 47 {
				t.Fatalf("区间时长应≈45 秒，实得 %d", window.DurationSeconds)
			}
			if window.EndReason != c.wantEnd {
				t.Fatalf("end_reason 不符：got=%d want=%d", window.EndReason, c.wantEnd)
			}
			if window.EndEventID != reply.GetEventId() {
				t.Fatalf("区间的 end_event_id 未指向本次停流事件：%s vs %s", window.EndEventID, reply.GetEventId())
			}
			// 开因与关因是**追加**而不是覆盖：真 SQL 用
			// reason = CONCAT(reason, IF(reason='','', ' | '), ?)（model/streaminterruption.go:154），
			// 覆盖写法会让「运营只看见后半句」这类缺陷看不见。
			// 追加的那一段来自请求的 reason 字段（closeReq 固定「运营切断」，见 testsupport_test.go:635），
			// 与 StopReason 枚举无关——枚举只决定 end_reason。
			if window.Reason != "心跳丢失 | 运营切断" {
				t.Fatalf("区间未保留开因并追加关因：%q", window.Reason)
			}
			if row.InterruptedTotalSecs != window.DurationSeconds {
				t.Fatalf("流上累计中断秒数与区间时长不同源：stream=%d window=%d", row.InterruptedTotalSecs, window.DurationSeconds)
			}
			if reply.GetInterruptedTotalSeconds() != row.InterruptedTotalSecs {
				t.Fatalf("响应回显的累计秒数不是提交后回读值：reply=%d row=%d",
					reply.GetInterruptedTotalSeconds(), row.InterruptedTotalSecs)
			}
			if got := e.delta(before); got[7] != 0 || got[4] != 1 || got[5] != 1 {
				t.Fatalf("闭合既有区间不该新增断流行，增量=%v", got)
			}
			ev := e.eventRow(t, "S-WIN", row.Seq)
			if ev.InterruptionID != open.InterruptionID || ev.InterruptedSeconds != window.DurationSeconds {
				t.Fatalf("停流事件未带上区间：%+v", ev)
			}
		})
	}
}

// TestCloseStream_InterruptedWithoutOpenWindowStillStops 是判别对的另一半：
// INTERRUPTED 但区间已被（扫描器/别处）合上时，停流不得因此失败，也不再写 Close。
// 若这里放宽成「找不到区间就报错」，运营在区间数据残缺时永远关不掉流。
func TestCloseStream_InterruptedWithoutOpenWindowStillStops(t *testing.T) {
	e := newTestEnv(t)
	e.seedCloseFixture(t, "S-NOWIN", model.StreamStateInterrupted, 2)
	// 一条已闭合的历史区间（ended_at != 0）：FindOpenByStream 必须查不到开区间。
	e.seedInterruption(t, &model.StreamInterruption{StreamID: "S-NOWIN", RoomID: 71, EpisodeNo: 1,
		NodeID: "node-close", StartedAt: 1700000000, EndedAt: 1700000060, DurationSeconds: 60,
		EndReason: model.InterruptionEndReconnected, StartEventID: "EVT-A", EndEventID: "EVT-B", Reason: "历史区间"})
	mark := e.markCalls()
	reply := e.mustCloseStream(t, "S-NOWIN", "c-nowin")
	e.requireCallOrder(t, mark, []string{
		"Stream.ApplyTransition", "StreamInterruption.FindOpenByStream",
		"Stream.MarkStopped", "StreamEvent.Insert", "Outbox.Insert",
	}, "无可合区间时的写序")
	if e.call("StreamInterruption.Close") != 0 {
		t.Fatalf("没有进行中的区间却调了 Close：%d 次", e.call("StreamInterruption.Close"))
	}
	row := e.streamRow(t, "S-NOWIN")
	if row.State != model.StreamStateStopped {
		t.Fatalf("区间残缺不该挡住停流：state=%d", row.State)
	}
	if row.InterruptedTotalSecs != 0 || reply.GetInterruptedTotalSeconds() != 0 {
		t.Fatalf("无区间可合时不应累加秒数：row=%d reply=%d", row.InterruptedTotalSecs, reply.GetInterruptedTotalSeconds())
	}
}

// --- 终态与幂等 ---

func TestCloseStream_AlreadyStoppedIsReplayWithFirstEvent(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedCloseFixture(t, "S-REPLAY", model.StreamStateStopped, 3)
	first := &model.StreamEvent{EventID: "EVT-FIRST-STOP", StreamID: "S-REPLAY", RoomID: fx.stream.RoomID,
		Seq: 3, FromState: model.StreamStatePublishing, ToState: model.StreamStateStopped,
		NodeID: "node-close", StopReason: model.StopReasonAdmin, ReportID: "c-original",
		Source: model.EventSourceAdmin, Reason: "首次停流", OccurredAt: 1700000300}
	if _, err := e.repo.StreamEvent.Insert(bg(), nil, first); err != nil {
		t.Fatalf("seed 首次停流事件：%v", err)
	}
	mark, before := e.markCalls(), e.effects()

	reply := e.mustCloseStream(t, "S-REPLAY", "c-second")
	mustTrue(t, reply.GetReplayed(), "二次停流 replayed")
	mustFalse(t, reply.GetApplied(), "二次停流 applied")
	if reply.GetSeq() != 3 || reply.GetEventId() != "EVT-FIRST-STOP" {
		t.Fatalf("回放必须回显首次迁移的 seq/event_id，实得 seq=%d event=%s", reply.GetSeq(), reply.GetEventId())
	}
	if reply.GetState() != rpc.StreamState_STREAM_STATE_STOPPED {
		t.Fatalf("回放的状态回显不符：%v", reply.GetState())
	}
	if !strings.Contains(reply.GetMessage(), "此前已停止") {
		t.Fatalf("回放文案必须说明本次未产生新事件：%q", reply.GetMessage())
	}
	// 零写入 + 零收尾动作：不再退配额、不再释放租约、不再写事件与 outbox。
	e.requireCallsFrom(t, mark, []string{
		"Stream.FindOne", "Stream.LockByID", "Stream.FindOne", "StreamEvent.FindByStreamSeq", "Stream.FindOne",
	}, "终态回放的触库形状")
	e.requireDelta(t, before, [9]int{}, "终态回放写入增量")
	if got := e.nodeRow(t, "node-close").ActiveStreams; got != fx.node.ActiveStreams {
		t.Fatalf("回放重复退配额：active=%d（应仍为 %d）", got, fx.node.ActiveStreams)
	}
}

// 缺陷:CloseStream 的跨流幂等键冲突只在「流还未停」时才拦得住
//
// 事实：closestreamlogic.go:31-33 声明「request_id 同时写入事件的 report_id，
// 防止同一个幂等键停掉两条不同的流」。但该保护来自 uniq_report_id 唯一索引，
// 只有走到 StreamEvent.Insert 才会触发；目标流已是 STOPPED 时
// applyStreamTransition 在 streamstate.go:114-130 的同态 no-op 分支直接返回，
// 完全不碰事件表，冲突判定被跳过。
// 后果：调用方拿着「A 流的幂等键」去关「已停的 B 流」会得到 replayed=true 的成功，
// 而 replayed 的语义（proto:358「true 表示命中 request_id」）在此处是假的——
// 命中的是 B 流的终态，不是本次 request_id。
// 判别对见本用例两段：活流复用同键 → ErrConcurrentUpdate；已停流复用同键 → 成功回放。
func TestCloseStream_RequestIDSchemeOnlyGuardsLiveStreams(t *testing.T) {
	e := newTestEnv(t)
	// A 流：先被 c-shared 这个幂等键停掉，report_id 已占位。
	a := e.seedCloseFixture(t, "S-A", model.StreamStatePublishing, 1)
	e.mustCloseStream(t, a.stream.StreamID, "c-shared")

	// 第一段（活流）：同一个幂等键去关 B 流必须被唯一索引挡住并整体回滚。
	b := e.seedCloseFixture(t, "S-B", model.StreamStatePublishing, 1)
	before, mark := e.effects(), e.markCalls()
	_, err := e.closeStream(t, "S-B", "c-shared")
	wantFail(t, err, model.ErrConcurrentUpdate, "复用他流的幂等键关活流")
	e.requireSameEffects(t, before, "复用幂等键关活流必须零痕迹")
	e.requireCallOrder(t, mark, []string{"Stream.ApplyTransition", "StreamEvent.Insert"}, "冲突发生在写事件时")
	if row := e.streamRow(t, "S-B"); row.State != model.StreamStatePublishing {
		t.Fatalf("冲突后 B 流被改状态：state=%d", row.State)
	}
	if got := e.nodeRow(t, "node-close").ActiveStreams; got != b.node.ActiveStreams {
		t.Fatalf("冲突后配额被提前归还：active=%d", got)
	}

	// 第二段（已停流）：同一个幂等键关一条「本来就停了」的流却是成功回放，
	// 且换一个完全不同的幂等键结果一模一样——request_id 在这条路径上没有任何作用。
	c := e.seedCloseFixture(t, "S-C", model.StreamStateStopped, 2)
	e.mustCloseStream(t, c.stream.StreamID, "c-shared-again")
	c2 := e.seedCloseFixture(t, "S-C2", model.StreamStateStopped, 2)
	second := e.mustCloseStream(t, c2.stream.StreamID, "c-brand-new-key")
	mustTrue(t, second.GetReplayed(), "已停流换键仍回放")
	if second.GetApplied() {
		t.Fatalf("已停流绝不能 applied=true")
	}
}

// TestCloseStream_ReplayEchoesEmptyEventIdWhenRowIsInconsistent 处理畸形现场：
// 库里是 STOPPED 但该 seq 没有事件行（手工清理事件表/回填未完成时会出现）。
// 契约是「仍按幂等成功回放，但 event_id 回空串」——不能 panic，也不能伪造一个 ID。
func TestCloseStream_ReplayEchoesEmptyEventIdWhenRowIsInconsistent(t *testing.T) {
	e := newTestEnv(t)
	e.seedCloseFixture(t, "S-NOEVENT", model.StreamStateStopped, 7)
	reply := e.mustCloseStream(t, "S-NOEVENT", "c-noevent")
	if reply.GetEventId() != "" {
		t.Fatalf("事件行缺席时不得伪造 event_id：%q", reply.GetEventId())
	}
	if reply.GetSeq() != 7 || !reply.GetReplayed() {
		t.Fatalf("应回显库里的 seq 并按回放处理：seq=%d replayed=%v", reply.GetSeq(), reply.GetReplayed())
	}
	if reply.GetState() != rpc.StreamState_STREAM_STATE_STOPPED {
		t.Fatalf("终态回显不符：%v", reply.GetState())
	}
}

// TestCloseStream_TerminalStateCannotBeReopened 从停流侧验证「终态无出边」：
// CloseStream 之后入口再把流推回 PUBLISHING 必须失败，
// 否则「运营切断」会被节点的一次迟到上报抹掉。
func TestCloseStream_TerminalStateCannotBeReopened(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedCloseFixture(t, "S-LOCKED", model.StreamStatePublishing, 1)
	e.mustCloseStream(t, "S-LOCKED", "c-locked")
	before := e.effects()

	_, err := e.report(t, reportReq("S-LOCKED", "rep-reopen", rpc.StreamState_STREAM_STATE_PUBLISHING))
	wantFail(t, err, model.ErrTerminalStream, "终态重开")
	e.requireSameEffects(t, before, "终态重开被拒后不得有写入")
	// 二次 CloseStream 也只能回放，不会产生第二条停流事件。
	again := e.mustCloseStream(t, "S-LOCKED", "c-locked-2")
	mustTrue(t, again.GetReplayed(), "二次停流")
	if got := e.delta(before); got[4] != 0 || got[5] != 0 {
		t.Fatalf("终态路径写入了事件/outbox，增量=%v", got)
	}
	if got := e.keyRow(t, fx.key.KeyID).CurrentStreamID; got != "" {
		t.Fatalf("密钥活跃指针被重新占用：%s", got)
	}
}

// --- 失败注入：每一层依赖失败时库里剩什么 ---

// failingAssignmentRelease 复刻「租约 CAS 输给并发迁移」：Release 回 false。
//
// 替身仍要 bump：留痕记的是「这条 SQL 发出去过」，真库在事务里也确实发出去过、只是被回滚了。
// 不记录就等于让「释放有没有被调用」变成断言不到的死角。
type failingAssignmentRelease struct {
	*fakeNodeAssignment
}

func (f *failingAssignmentRelease) Release(context.Context, sqlx.Session, int64, int32, int64, string, string) (bool, error) {
	f.db.bump("NodeAssignment.Release")
	return false, nil
}

// drainedQuotaNode 复刻「租约释放成功但节点没有配额可退」（节点行被删/计数已被清空）。
type drainedQuotaNode struct {
	*fakeIngestNode
}

func (f *drainedQuotaNode) ReleaseQuota(context.Context, sqlx.Session, string) (bool, error) {
	f.db.bump("IngestNode.ReleaseQuota")
	return false, nil
}

func TestCloseStream_CasLossRollsBackWithoutAnyTrace(t *testing.T) {
	e := newTestEnv(t)
	e.seedCloseFixture(t, "S-CAS", model.StreamStatePublishing, 1)
	e.db.forceTransitionMiss = true
	before, mark := e.effects(), e.markCalls()

	_, err := e.closeStream(t, "S-CAS", "c-cas")
	wantFail(t, err, model.ErrConcurrentUpdate, "CAS 抢输")
	e.requireSameEffects(t, before, "CAS 抢输")
	e.requireCallsFrom(t, mark, []string{
		"Stream.FindOne", "Stream.LockByID", "Stream.FindOne", "Stream.ApplyTransition",
	}, "CAS 抢输的触库形状（收尾一步都不做）")
	if got := e.effects().txs; got != before.txs+1 {
		t.Fatalf("CAS 路径应只开一次事务，实得 %d", got)
	}
	row := e.streamRow(t, "S-CAS")
	if row.State != model.StreamStatePublishing || row.Seq != 1 {
		t.Fatalf("CAS 失败后状态被改写：state=%d seq=%d", row.State, row.Seq)
	}
	// 事件表没写过，也就没有留下任何「report_id 已占用」的痕迹，重试可以再试。
	if got := e.call("StreamEvent.Insert"); got != 0 {
		t.Fatalf("CAS 抢输不得写事件，实得 %d 次", got)
	}
}

func TestCloseStream_EventOrOutboxFailureRollsBackWholeTail(t *testing.T) {
	cases := []struct {
		name string
		bump func(*testEnv)
	}{
		{"事件写失败", func(e *testEnv) { e.repo.StreamEvent = &failingStreamEvent{&fakeStreamEvent{db: e.db}} }},
		{"Outbox 写失败", func(e *testEnv) { e.repo.Outbox = &failingOutbox{fakeOutbox: &fakeOutbox{db: e.db}} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			fx := e.seedCloseFixture(t, "S-HALF", model.StreamStateInterrupted, 2)
			win := e.openWindow(t, "S-HALF", fx.stream.RoomID, "node-close", 20)
			c.bump(e)
			before := e.effects()

			_, err := e.closeStream(t, "S-HALF", "c-half")
			if err == nil {
				t.Fatalf("%s 时停流必须整体失败", c.name)
			}
			e.requireSameEffects(t, before, c.name+"必须回滚")
			row := e.streamRow(t, "S-HALF")
			if row.State != model.StreamStateInterrupted || row.Seq != 2 || row.StopDetail != "" {
				t.Fatalf("%s 回滚不完整：state=%d seq=%d stop_detail=%q", c.name, row.State, row.Seq, row.StopDetail)
			}
			// 同事务里的区间闭合、健康归位、租约与配额释放也必须一起退回。
			if w := e.db.interrupt[win.InterruptionID]; w.EndedAt != 0 || w.DurationSeconds != 0 || w.EndEventID != "" {
				t.Fatalf("%s：断流区间被单独闭合 %+v", c.name, w)
			}
			if got := e.nodeRow(t, "node-close").ActiveStreams; got != fx.node.ActiveStreams {
				t.Fatalf("%s：配额被单独归还 active=%d", c.name, got)
			}
			if lease := fx.lease(t, e); lease.State != model.AssignmentStateActive || lease.ReleasedAt != 0 {
				t.Fatalf("%s：租约被单独释放 %+v", c.name, lease)
			}
			if got := e.keyRow(t, fx.key.KeyID).CurrentStreamID; got != "S-HALF" {
				t.Fatalf("%s：密钥活跃指针被单独清空 %q", c.name, got)
			}
		})
	}
}

// failingStreamEvent 让事件写失败（真库对应 uniq_stream_seq 冲突或语句超时）。
// 同样保留 bump：「失败前是否真的发过这条 INSERT」是判定「半截事实从哪来」的唯一线索。
type failingStreamEvent struct {
	*fakeStreamEvent
}

func (f *failingStreamEvent) Insert(context.Context, sqlx.Session, *model.StreamEvent) (int64, error) {
	f.db.bump("StreamEvent.Insert")
	return 0, errors.New("live_stream_event 写入失败")
}

func TestCloseStream_LeaseReleaseCasLossRollsBack(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedCloseFixture(t, "S-LEASE", model.StreamStatePublishing, 1)
	e.repo.NodeAssignment = &failingAssignmentRelease{fakeNodeAssignment: &fakeNodeAssignment{db: e.db}}
	before, mark := e.effects(), e.markCalls()

	_, err := e.closeStream(t, "S-LEASE", "c-lease")
	wantFail(t, err, model.ErrConcurrentUpdate, "租约 CAS 抢输")
	e.requireSameEffects(t, before, "租约 CAS 抢输")
	e.requireCallOrder(t, mark, []string{
		"Stream.ApplyTransition", "Stream.MarkStopped", "StreamKey.ReleaseActiveStream",
		"NodeAssignment.FindActiveByStream", "NodeAssignment.Release",
	}, "租约失败发生在退配额之前")
	if got := e.call("IngestNode.ReleaseQuota"); got != 0 {
		t.Fatalf("租约释放失败后不得退配额，实得 %d 次", got)
	}
	row := e.streamRow(t, "S-LEASE")
	if row.State != model.StreamStatePublishing {
		t.Fatalf("回滚不完整：state=%d", row.State)
	}
	if got := e.keyRow(t, fx.key.KeyID).CurrentStreamID; got != "S-LEASE" {
		t.Fatalf("回滚不完整：指针被清空 %q", got)
	}
}

// 缺陷:配额无退路时报 ErrNodeNotFound，但事务已把租约释放写过一遍
//
// 事实：streamstate.go:256-264 在 ReleaseQuota 回 false 时报
// 「ErrNodeNotFound: node quota already drained」并整体回滚——语义是对的（宁可让运维看到），
// 但错误里没有 node_id/assignment_id，运营只看到「节点不存在」，
// 而真因是「节点行存在、active_streams 已经是 0」（计数漂移，不是缺行）。
// 本用例钉住两点可证伪的部分：错误哨兵与「零残留」。
func TestCloseStream_QuotaAlreadyDrainedIsNotSilent(t *testing.T) {
	e := newTestEnv(t)
	e.seedCloseFixture(t, "S-DRAINED", model.StreamStatePublishing, 1)
	e.repo.IngestNode = &drainedQuotaNode{fakeIngestNode: &fakeIngestNode{db: e.db}}
	before, mark := e.effects(), e.markCalls()

	_, err := e.closeStream(t, "S-DRAINED", "c-drained")
	wantFail(t, err, model.ErrNodeNotFound, "配额无退路")
	if !strings.Contains(err.Error(), "quota") {
		t.Fatalf("错误文案必须点明是配额退无可退：%v", err)
	}
	e.requireSameEffects(t, before, "配额异常必须整体回滚")
	e.requireCallOrder(t, mark, []string{"NodeAssignment.Release", "IngestNode.ReleaseQuota"}, "先释放租约再退配额")
	if got := e.call("StreamEvent.Insert"); got != 0 {
		t.Fatalf("配额异常时不得写事件，实得 %d 次", got)
	}
}

// --- CDN/媒体入口：桩到底会被调用吗？ ---

// TestCloseStream_NeverTouchesIngestGateway 回答「CDN 桩失败时停流会怎样」：
// 答案是根本不会走到桩——CloseStreamLogic 全程不引用 svcCtx.Gateway
// （closestreamlogic.go:34-116 里没有任何 Gateway 调用点），
// 因此厂商侧未配置既不改变结果、也不可能成为停流失败的原因。
// 对照：repository.stubIngestGateway 三个方法都恒回 ErrCdnNotConfigured（用例里现问现证，
// 不靠 README 的措辞推断）。
func TestCloseStream_NeverTouchesIngestGateway(t *testing.T) {
	e := newTestEnv(t)
	e.seedCloseFixture(t, "S-GW", model.StreamStatePublishing, 1)
	// 网关全部失败也不该影响停流：把替身设成「一律报错」。
	e.gw.kickErr = repository.ErrCdnNotConfigured
	e.gw.probeErr = repository.ErrCdnNotConfigured
	e.gw.bindErr = repository.ErrCdnNotConfigured

	e.mustCloseStream(t, "S-GW", "c-gw")
	if e.gw.kickCalls != 0 || e.gw.probeCall != 0 || e.gw.bindCall != 0 {
		t.Fatalf("停流不该触达接入网关：kick=%d probe=%d bind=%d", e.gw.kickCalls, e.gw.probeCall, e.gw.bindCall)
	}
	if row := e.streamRow(t, "S-GW"); row.State != model.StreamStateStopped {
		t.Fatalf("网关缺席不影响停流：state=%d", row.State)
	}

	// 桩的真实返回值（不是「假设它报错」）：无论 Cdn.Enabled 真假都回 ErrCdnNotConfigured。
	for _, enabled := range []bool{false, true} {
		cdn := config.CdnConf{Enabled: enabled, PublishDomains: []string{"push.example.com"}}
		gw := repository.NewIngestGateway(cdn)
		if err := gw.KickStream(bg(), "node-close", "S-GW", "operator"); !errors.Is(err, repository.ErrCdnNotConfigured) {
			t.Fatalf("Cdn.Enabled=%v 时 KickStream 应回 ErrCdnNotConfigured，实得 %v", enabled, err)
		}
		if _, err := gw.ProbeStream(bg(), "node-close", "S-GW"); !errors.Is(err, repository.ErrCdnNotConfigured) {
			t.Fatalf("Cdn.Enabled=%v 时 ProbeStream 应回 ErrCdnNotConfigured，实得 %v", enabled, err)
		}
		if err := gw.BindCallbackDomain(bg(), "push.example.com"); !errors.Is(err, repository.ErrCdnNotConfigured) {
			t.Fatalf("Cdn.Enabled=%v 时 BindCallbackDomain 应回 ErrCdnNotConfigured，实得 %v", enabled, err)
		}
	}
}

// --- 链路字段与列宽 ---

func TestCloseStream_TraceIDIsTruncatedToColumnWidth(t *testing.T) {
	e := newTestEnv(t)
	e.seedCloseFixture(t, "S-TRACE", model.StreamStatePublishing, 1)
	long := strings.Repeat("t", maxTraceIDBytes+40)
	e.mustCloseStream(t, "S-TRACE", "c-trace", func(in *rpc.CloseStreamReq) { in.TraceId = long })

	row := e.streamRow(t, "S-TRACE")
	ev := e.eventRow(t, "S-TRACE", row.Seq)
	if len(ev.TraceID) != maxTraceIDBytes {
		t.Fatalf("trace_id 应截到列宽 %d，实得 %d", maxTraceIDBytes, len(ev.TraceID))
	}
	if !strings.HasPrefix(long, ev.TraceID) {
		t.Fatalf("截断必须保留前缀（trace 是关联句柄，取尾巴接不上链路）")
	}
	pending, err := e.repo.Outbox.ListByState(bg(), model.OutboxStatePending, 5)
	wantOK(t, pending, err, "查询 outbox")
	if len(pending) != 1 || !strings.Contains(pending[0].Payload, ev.TraceID) {
		t.Fatalf("信封 trace_id 应与事件列一致（同一句柄）：%+v", pending)
	}
}

// --- 响应与库一致性 ---

func TestCloseStream_ReplyComesFromCommittedRowNotStaleInput(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedCloseFixture(t, "S-ECHO", model.StreamStateInterrupted, 2)
	e.openWindow(t, "S-ECHO", fx.stream.RoomID, "node-close", 10)
	reply := e.mustCloseStream(t, "S-ECHO", "c-echo")
	row := e.streamRow(t, "S-ECHO")

	if reply.GetState() != rpcStreamState(row.State) {
		t.Fatalf("响应状态与库不符：reply=%v row=%d", reply.GetState(), row.State)
	}
	if reply.GetInterruptedTotalSeconds() != row.InterruptedTotalSecs {
		t.Fatalf("累计中断秒数与库不符：reply=%d row=%d", reply.GetInterruptedTotalSeconds(), row.InterruptedTotalSecs)
	}
	if reply.GetSeq() != row.Seq {
		t.Fatalf("seq 与库不符：reply=%d row=%d", reply.GetSeq(), row.Seq)
	}
	if !strings.Contains(reply.GetMessage(), "已停流并释放") {
		t.Fatalf("成功路径的文案必须说明释放了什么：%q", reply.GetMessage())
	}
}

func TestCloseStream_ServiceContextWiredWithoutGatewayStillWorks(t *testing.T) {
	// README 缺口 1 的口径核对：停流不依赖网关，因此 svcCtx.Gateway 为 nil 也必须能关流。
	// （缺陷 #12：整个 logic 包没有一处 KickStream 调用点，强制停流只是「账面停」，
	// 节点侧仍在推。此处钉住「不依赖」这一事实，登记缺口的动作在 README 已完成。）
	e := newTestEnv(t)
	fx := e.seedCloseFixture(t, "S-NOGW", model.StreamStatePublishing, 1)
	repo := e.repo
	noGw := &svc.ServiceContext{Config: e.svc.Config, Repository: repo}
	_, err := NewCloseStreamLogic(bg(), noGw).CloseStream(&rpc.CloseStreamReq{
		StreamId: fx.stream.StreamID, RequestId: "c-nogw", OperatorMid: 9001, Admin: true, Reason: "无网关",
	})
	wantOK(t, (*rpc.CloseStreamReply)(nil), err, "无网关停流")
	if row := e.streamRow(t, "S-NOGW"); row.State != model.StreamStateStopped {
		t.Fatalf("无网关时停流未落库：state=%d", row.State)
	}
}
