package logic

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/live-gateway/internal/config"
	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"
)

// 本文件覆盖 DrainRoomRoute —— 发布期节点优雅下线的唯一写入口。
//
// 它和 KickConnection 同为「处置类写接口」，但影响面大一个数量级：一次误排空 = 整个房间的
// 新广播凭空消失。因此断言重点不是「回没回 DRAINING」，而是四条护栏：
//  1. 顺序：授权 → 参数 → 幂等窗口 → 读路由 → 条件更新 → 失效缓存 → 审计 → 回显。
//     前两段必须「零存储调用」，否则一次越权探测或未归因重试就能把 request_id 烧掉、
//     把别人的路由状态推进一格。
//  2. 只有「合法可排空」的路由才被动：非本房间主节点 = 越权（落 DENIED 审计）、
//     OFFLINE→DRAINING 没有状态机边、已 DRAINING 按幂等成功且不重复推进版本。
//  3. 两步迁移原子性：B 只与 state=SERVING 同一条 UPDATE 写入，第 2 步打不中时行必须
//     停在 DRAINING@A —— 任何时刻都不存在「路由指向空节点」。
//  4. 失败与部分进度都要诚实：依赖故障原样上抛（不吞、不伪装成功），
//     读数/审计这类观测性降级不推翻已生效的业务结论。

// drainReq 一条最小可用的排空请求（授权主体由 ctx 决定，版本由用例给）。
func drainReq(room int64, node, requestID string, version int64) *rpc.DrainRoomRouteReq {
	return &rpc.DrainRoomRouteReq{
		RoomId: room, NodeId: node, ExpectedVersion: version,
		Reason: "node_shutdown", RequestId: requestID, Operator: "ops-field",
	}
}

// assertDrainNotStarted 排空还没开始的唯一硬证据：幂等键没占、条件更新没打、
// 路由缓存没失效、审计没落。只断言「返回了错」是不够的——那三类副作用都是真实损害。
func assertDrainNotStarted(t *testing.T, e *testEnv) {
	t.Helper()
	if n := e.Leases.callCount("ClaimRequest"); n != 0 {
		t.Fatalf("参数/授权阶段就占了幂等键（request_id 被烧掉，重试会拿到 pending）: ClaimRequest %d 次", n)
	}
	if n := e.Routes.updateCalls; n != 0 {
		t.Fatalf("参数/授权阶段就推进了路由状态机: UpdateState %d 次", n)
	}
	if n := e.Routes.registers; n != 0 {
		t.Fatalf("排空不该走 Register 承接: Register %d 次", n)
	}
	if n := e.Leases.callCount("CacheDel"); n != 0 {
		t.Fatalf("排空没生效却失效了路由缓存: CacheDel %d 次", n)
	}
	assertNoWrites(t, e)
}

// --- 门禁顺序与参数 ---

func TestDrainRoomRouteParameterGateRejectsBeforeAnyStorageCall(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*rpc.DrainRoomRouteReq)
		want error
		text string
	}{
		{"房间号为 0", func(r *rpc.DrainRoomRouteReq) { r.RoomId = 0 }, model.ErrInvalidRoomID, ""},
		{"房间号为负", func(r *rpc.DrainRoomRouteReq) { r.RoomId = -7 }, model.ErrInvalidRoomID, ""},
		{"node_id 为空", func(r *rpc.DrainRoomRouteReq) { r.NodeId = "  " }, model.ErrEmptyNodeID, ""},
		{"node_id 含空格", func(r *rpc.DrainRoomRouteReq) { r.NodeId = "gw node a" }, model.ErrEmptyNodeID, ""},
		{"reason 为空", func(r *rpc.DrainRoomRouteReq) { r.Reason = "" }, model.ErrEmptyReason, ""},
		// 两道长度门必须分别可测：requireReason 收 256（通用审计宽度），列宽门收 64
		// （live_gw_room_route.drain_reason VARCHAR(64)）。落在两者之间的那段必须被 64 这道挡下，
		// 否则 MySQL 非严格模式会静默截断，审计里出现「半个原因」比报错更难复盘。
		{"reason 超列宽但未超通用门", func(r *rpc.DrainRoomRouteReq) { r.Reason = strings.Repeat("x", 65) },
			model.ErrEmptyReason, "drain_reason"},
		{"reason 超通用门", func(r *rpc.DrainRoomRouteReq) { r.Reason = strings.Repeat("x", 300) },
			model.ErrEmptyReason, "256"},
		{"operator 为空", func(r *rpc.DrainRoomRouteReq) { r.Operator = "" }, model.ErrEmptyOperator, ""},
		{"request_id 为空", func(r *rpc.DrainRoomRouteReq) { r.RequestId = " " }, model.ErrEmptyRequestID, ""},
		{"request_id 含空白", func(r *rpc.DrainRoomRouteReq) { r.RequestId = "req drain" }, model.ErrEmptyRequestID, ""},
		{"expected_version 为 0", func(r *rpc.DrainRoomRouteReq) { r.ExpectedVersion = 0 },
			model.ErrVersionConflict, "expected_version is required"},
		{"expected_version 为负", func(r *rpc.DrainRoomRouteReq) { r.ExpectedVersion = -3 },
			model.ErrVersionConflict, "expected_version is required"},
		{"target_node_id 与要排空的节点相同", func(r *rpc.DrainRoomRouteReq) { r.TargetNodeId = nodeA },
			model.ErrInvalidTransition, "equals the node being drained"},
		{"target_node_id 形状非法", func(r *rpc.DrainRoomRouteReq) { r.TargetNodeId = "gw node b" },
			model.ErrEmptyNodeID, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			seedRoute(e, roomID, model.RouteStateServing, nodeA)
			req := drainReq(roomID, nodeA, "req-drain-param-"+strings.ReplaceAll(tc.name, " ", ""), 1)
			tc.mut(req)
			// 用归因 OPERATOR：这样失败只可能来自参数门，不会和授权门禁混在一起。
			got, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).DrainRoomRoute(req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("期望 %v，实际 %v", tc.want, err)
			}
			if tc.text != "" && !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("错误文本要能解释为什么拒（含 %q），实际 %v", tc.text, err)
			}
			if got != nil {
				t.Fatalf("参数被拒时不得有回显: %+v", got)
			}
			// 唯一硬证据：一条都没执行。
			assertDrainNotStarted(t, e)
			if row := e.Routes.rows[roomID]; row.State != model.RouteStateServing || row.Version != 1 {
				t.Fatalf("被拒的排空改动了路由行: %+v", row)
			}
		})
	}

	// 判别性对照：边界另一侧必须放行，否则上面的断言只是「永远报错」的同义反复。
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	okReq := drainReq(roomID, nodeA, "req-drain-boundary", 1)
	okReq.Reason = strings.Repeat("x", 64) // 恰好等于列宽：必须被放行
	okReq.TargetNodeId = " " + nodeB + " " // 空白由 requireNodeID 归一
	got, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).DrainRoomRoute(okReq)
	if err != nil {
		t.Fatalf("64 字节 reason + 可归一 target 应放行: %v", err)
	}
	if got.GetState() != rpc.RouteState_ROUTE_STATE_SERVING || got.GetNodeId() != nodeB {
		t.Fatalf("迁移结果投影错: %+v", got)
	}
	if e.Routes.updateCalls != 2 {
		t.Fatalf("迁移应是两步条件更新，实际 UpdateState %d 次", e.Routes.updateCalls)
	}
}

// 空请求体在门禁之前就被挡下：连归因都不做，避免把 nil 当成「未归因主体」。
func TestDrainRoomRouteNilRequestRejectedBeforeGate(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	e.Leases.failWith("ClaimRequest", errors.New("should not reach"))
	got, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).DrainRoomRoute(nil)
	if !errors.Is(err, model.ErrInvalidRoomID) || got != nil {
		t.Fatalf("nil 请求必须回 ErrInvalidRoomID 且无回显，实际 err=%v reply=%+v", err, got)
	}
	assertDrainNotStarted(t, e)
}

// 处置类写门禁 fail-closed，且排在参数与幂等窗口之前。
//
// 「权限先于写存储」在这里有具体代价：未归因主体连 request_id 都不该占住，
// 否则一个探测者可以把某个幂等键永久变成 pending，真正的运营请求再来就被判成重放。
func TestDrainRoomRouteDispositionAuthIsFailClosed(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	row := e.Routes.rows[roomID]
	got, err := NewDrainRoomRouteLogic(ctxClient(t), e.Svc).DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-unatt", 1))
	if !errors.Is(err, model.ErrPermissionDenied) || got != nil {
		t.Fatalf("未归因主体不得执行排空，实际 err=%v reply=%+v", err, got)
	}
	assertDrainNotStarted(t, e)
	if row.State != model.RouteStateServing || row.DrainReason != "" {
		t.Fatalf("被拒的排空改动了路由行: %+v", row)
	}

	// 判别性对照一：自报 attested=false + role=OPERATOR 仍按未归因处理。
	if _, err := NewDrainRoomRouteLogic(ctxAs(t, "false", "OPERATOR", "1001", ""), e.Svc).
		DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-forged", 1)); !errors.Is(err, model.ErrPermissionDenied) {
		t.Fatalf("自报 metadata 不能构成内部主体，实际 %v", err)
	}
	// 判别性对照二：归因 SERVICE 同样是可信主体（发布脚本是内部调用方），必须真的执行。
	if _, err := NewDrainRoomRouteLogic(ctxService(t, "deploy"), e.Svc).
		DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-service", 1)); err != nil {
		t.Fatalf("归因 SERVICE 应可排空: %v", err)
	}
	if e.Routes.rows[roomID].State != model.RouteStateDraining {
		t.Fatalf("对照用例未把 DRAINING 落进存储")
	}
}

// 钉住当前行为：未归因的排空尝试**一条审计都不留**，而「排空别人节点」那种越权会留 DENIED 行。
//
// 两条拒绝路径的留痕强度不一致：drainroomroutelogic.go:144-146 在 primary_node 不匹配时写
// DENIED 审计，而 :75-78 的授权拒绝直接 return err。后果是「谁试过排空这个房间」在
// 审计表里查不到（AGENTS.md §8 要求处置留痕）。这里按现状断言并把差异显式化，
// 一旦有人补上 DENIED 审计，本用例会因为行数变化而红。
func TestDrainRoomRouteUnattestedAttemptLeavesNoAuditWhileWrongNodeDoes(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	if _, err := NewDrainRoomRouteLogic(ctxClient(t), e.Svc).
		DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-audit-none", 1)); !errors.Is(err, model.ErrPermissionDenied) {
		t.Fatalf("未归因应被拒: %v", err)
	}
	if n := e.Logs.inserts; n != 0 {
		t.Fatalf("当前实现：授权拒绝路径不落审计（钉住现状，实际 %d 行）", n)
	}

	// 同一身份（归因 OPERATOR）改一个非法字段：越权排空必须留 DENIED。
	if _, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).
		DrainRoomRoute(drainReq(roomID, "gw-node-other", "req-drain-audit-denied", 1)); !errors.Is(err, model.ErrPermissionDenied) {
		t.Fatalf("排空非本房间主节点必须被拒: %v", err)
	}
	row := onlyAudit(t, e)
	if row.State != model.BroadcastLogDenied || row.DropReason != model.DropPermissionDenied {
		t.Fatalf("越权排空应记 DENIED/PERMISSION_DENIED，实际 %d/%d", row.State, row.DropReason)
	}
	if row.Kind != model.KindModeration || row.RoomId != roomID {
		t.Fatalf("处置审计的类别与房间错: %+v", row)
	}
	// 审计的 message_id 带时间戳且落在 message_id 列：不同请求各留一行，重放不新增。
	if !strings.HasPrefix(row.MessageId, fmt.Sprintf("drain-denied:%d:", roomID)) {
		t.Fatalf("被拒审计的 message_id 应为 drain-denied:<room>:<now>，实际 %q", row.MessageId)
	}
}

// 放开过渡开关只放开执行，不放开审计里的主体归因（与 KickConnection 同口径）。
func TestDrainRoomRouteUnattestedEscapeHatchKeepsAuditHonest(t *testing.T) {
	e := newTestEnv(t)
	e.apply(func(c *config.LiveGatewayConf) { c.AllowUnattestedOperatorWrites = true })
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	got, err := NewDrainRoomRouteLogic(ctxClient(t), e.Svc).
		DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-escape", 1))
	if err != nil || got.GetState() != rpc.RouteState_ROUTE_STATE_DRAINING {
		t.Fatalf("开关打开时应可执行（过渡环境），实际 err=%v reply=%+v", err, got)
	}
	row := onlyAudit(t, e)
	if row.SenderMid != 0 {
		t.Fatalf("开关不能把未归因主体洗白成运营（sender_mid 必须记归因结果 0），实际 %d", row.SenderMid)
	}
	if row.State != model.BroadcastLogSent || row.DropReason != model.DropOK {
		t.Fatalf("排空已生效，审计应为 SENT/OK，实际 %d/%d", row.State, row.DropReason)
	}
}

// --- 幂等窗口 ---

// request_id 命中时只回读、零写入、零审计：重复排空不再 version+1。
//
// 这条承诺的代价很具体：如果重放也推进版本，发布脚本重试三次就会把版本刷到 4，
// 运营手里那个 expected_version 全部冲突，后续所有排空都打不中。
func TestDrainRoomRouteReplayOnlyReadsRow(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	l := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc)
	first, err := l.DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-replay", 1))
	if err != nil || first.GetState() != rpc.RouteState_ROUTE_STATE_DRAINING {
		t.Fatalf("首次排空失败: err=%v reply=%+v", err, first)
	}
	if first.GetVersion() != 2 {
		t.Fatalf("首次排空应把版本推到 2，实际 %d", first.GetVersion())
	}
	beforeUpdate, beforeAudit, beforeCacheDel := e.Routes.updateCalls, e.Logs.inserts, e.Leases.callCount("CacheDel")

	second, err := l.DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-replay", 1))
	if err != nil {
		t.Fatalf("重放不该失败: %v", err)
	}
	if e.Routes.updateCalls != beforeUpdate {
		t.Fatalf("重放重复推进了状态机（UpdateState %d → %d）", beforeUpdate, e.Routes.updateCalls)
	}
	if e.Logs.inserts != beforeAudit {
		t.Fatalf("重放重复落了审计（%d → %d 行）", beforeAudit, e.Logs.inserts)
	}
	if n := e.Leases.callCount("CacheDel"); n != beforeCacheDel {
		t.Fatalf("重放没有状态变更，不该再失效缓存（%d → %d）", beforeCacheDel, n)
	}
	if second.GetVersion() != 2 || second.GetState() != rpc.RouteState_ROUTE_STATE_DRAINING {
		t.Fatalf("重放必须回显当前真值而不是猜一个，实际 %+v", second)
	}

	// 判别性对照：换一个 request_id 打到一个新房间（同主节点），同一份代码就必须真的推进版本。
	seedRoute(e, otherRoom, model.RouteStateServing, nodeA)
	third, err := l.DrainRoomRoute(drainReq(otherRoom, nodeA, "req-drain-replay-ctrl", 1))
	if err != nil || third.GetVersion() != 2 {
		t.Fatalf("对照用例（新 request_id）应完成排空，实际 err=%v reply=%+v", err, third)
	}
	if e.Routes.updateCalls != beforeUpdate+1 {
		t.Fatalf("对照用例应恰好多打一次条件更新，实际 %d 次", e.Routes.updateCalls)
	}

	// 幂等窗口故障不得当成「首次请求」放行：那会让重试各推进一格。
	e.Leases.failWith("ClaimRequest", errDrainClaimProbe)
	if _, err := l.DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-claim-fail", 2)); !errors.Is(err, errDrainClaimProbe) {
		t.Fatalf("幂等窗口不可用要原样上抛，实际 %v", err)
	}
}

var errDrainClaimProbe = errors.New("claim-request-probe")

// 钉住当前行为：首次请求在结论回填之前失败时，幂等窗口里留下 pending，
// 而重试把 pending 当成「已受理」直接回显当前行——于是「没排空」被报成成功。
//
// 实现注释（drainroomroutelogic.go:46-48）承诺的是相反结论：「首次请求失败在回填结论之前，
// 窗口里留下 pending，重试按未受理重新执行一遍——排空本身是条件 UPDATE，可安全重放」。
// 代码 :115 只取 replay 布尔、丢掉了 value，因此 :123 的重放分支无法区分
// 「首次结论已回填」与「首次根本没执行成功」。KickConnection 对同一个 pending
// 回的是 ErrRequestIdDuplicated（要求换键重试），两条路径对同一事实给出相反结论。
// 后果可复现：运营脚本第 1 次因 DB 抖动失败、第 2 次拿到 state=SERVING + err=nil，
// 于是发布流程认为「已排空」并继续下线节点，房间还在往那个节点打广播。
func TestDrainRoomRouteFailedFirstAttemptMakesReplayReportUndrainedRowAsSuccess(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	l := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc)
	req := drainReq(roomID, nodeA, "req-drain-pending-gap", 1)

	e.Routes.fail("UpdateState", errDrainUpdateProbe)
	if _, err := l.DrainRoomRoute(req); !errors.Is(err, errDrainUpdateProbe) {
		t.Fatalf("前提不成立：首次请求应因条件更新故障而原样失败，实际 %v", err)
	}
	if n := e.Leases.callCount("ClaimRequest"); n != 1 {
		t.Fatalf("前提不成立：幂等键已被首次请求占住，实际 %d 次", n)
	}
	if row := e.Routes.rows[roomID]; row.State != model.RouteStateServing {
		t.Fatalf("前提不成立：首次请求失败，行应仍是 SERVING，实际 %d", row.State)
	}

	e.Routes.fail("UpdateState", nil) // 依赖恢复，重试本应把排空做完
	got, err := l.DrainRoomRoute(req)
	if err != nil {
		t.Fatalf("当前实现：重试拿到的是 nil error（把未执行报成成功），缺陷已被修复请把本用例改成断言错误。实际 %v", err)
	}
	if got.GetState() != rpc.RouteState_ROUTE_STATE_SERVING || got.GetVersion() != 1 {
		t.Fatalf("钉住现状：重试回显的是未排空的行 state=%d version=%d", got.GetState(), got.GetVersion())
	}
	if e.Routes.updateCalls != 0 {
		t.Fatalf("钉住现状：重试没有重新执行条件更新，实际 %d 次", e.Routes.updateCalls)
	}
	// 判别性对照：换一个 request_id 立刻能排空，证明上面那条不是「路由不可排空」。
	if _, err := l.DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-pending-ctrl", 1)); err != nil {
		t.Fatalf("换 request_id 后应可执行: %v", err)
	}
	if e.Routes.rows[roomID].State != model.RouteStateDraining {
		t.Fatalf("对照用例未把 DRAINING 落进存储")
	}
}

// --- 路由归属与状态机 ---

// 无路由行是「排空目标不存在」，不是「排空成功」：绝不回显一条空路由。
func TestDrainRoomRouteMissingRowIsNotFound(t *testing.T) {
	e := newTestEnv(t)
	got, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).
		DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-norow", 1))
	if !errors.Is(err, model.ErrRouteNotFound) || got != nil {
		t.Fatalf("无行必须回 ErrRouteNotFound，实际 err=%v reply=%+v", err, got)
	}
	if n := e.Routes.updateCalls; n != 0 {
		t.Fatalf("无行却打了条件更新，UpdateState %d 次", n)
	}
	assertNoWrites(t, e)
}

// 只能排空自己承接的路由；非主节点的请求是越权（本方法最高收益的攻击面）。
func TestDrainRoomRouteRefusesNonPrimaryNode(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA, "gw-node-c")
	// nodeA 的副本节点视角：gw-node-c 只是副本，不能把主节点排掉。
	for _, attacker := range []string{"gw-node-c", nodeB} {
		req := drainReq(roomID, attacker, "req-drain-notprimary-"+attacker, 1)
		got, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).DrainRoomRoute(req)
		if !errors.Is(err, model.ErrPermissionDenied) || got != nil {
			t.Fatalf("节点 %s 排空别人的主节点必须被拒，实际 err=%v reply=%+v", attacker, err, got)
		}
		if !strings.Contains(err.Error(), nodeA) {
			t.Fatalf("错误要指出真实主节点（否则运营只能猜），实际 %v", err)
		}
	}
	if e.Routes.updateCalls != 0 {
		t.Fatalf("被拒的排空推进了状态机")
	}
	if row := e.Routes.rows[roomID]; row.State != model.RouteStateServing || row.Version != 1 {
		t.Fatalf("被拒的排空改动了路由行: %+v", row)
	}
	// 判别性对照：同一房间换成主节点的请求就成功。
	if _, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).
		DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-primary-ok", 1)); err != nil {
		t.Fatalf("主节点自己排空应放行: %v", err)
	}
}

// OFFLINE→DRAINING 没有状态机边；已 DRAINING 按幂等成功回显且不重复推进。
//
// 这两条边必须在「一次 UPDATE 都没打」的前提下成立：门禁用 IsValidRouteTransition 预检，
// UPDATE 的 fromStates=[SERVING] 只是第二道保险，两道都在才算守住。
func TestDrainRoomRouteStateMachineEdges(t *testing.T) {
	t.Run("OFFLINE 不可排空", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateOffline, nodeA)
		got, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).
			DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-offline", 1))
		if !errors.Is(err, model.ErrInvalidTransition) || got != nil {
			t.Fatalf("OFFLINE→DRAINING 不在状态机里，实际 err=%v reply=%+v", err, got)
		}
		if e.Routes.updateCalls != 0 || e.Logs.inserts != 0 {
			t.Fatalf("非法迁移却执行了写入（update=%d audit=%d）", e.Routes.updateCalls, e.Logs.inserts)
		}
		if row := e.Routes.rows[roomID]; row.State != model.RouteStateOffline || row.Version != 1 {
			t.Fatalf("非法迁移改动了路由行: %+v", row)
		}
	})

	t.Run("已 DRAINING 幂等成功", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateDraining, nodeA)
		e.Routes.rows[roomID].Version = 6
		e.Routes.rows[roomID].DrainReason = "node_shutdown"
		got, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).
			DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-already", 6))
		if err != nil {
			t.Fatalf("重复排空应按成功返回而不是报错: %v", err)
		}
		if got.GetState() != rpc.RouteState_ROUTE_STATE_DRAINING || got.GetVersion() != 6 {
			t.Fatalf("应回显当前行不推进版本，实际 %+v", got)
		}
		if e.Routes.updateCalls != 0 {
			t.Fatalf("已 DRAINING 不该再打条件更新（重复排空会刷飞版本号）")
		}
		if n := e.Logs.inserts; n != 0 {
			t.Fatalf("已 DRAINING 的重放不该重复审计，实际 %d 行", n)
		}
	})

	// 判别性对照：同一房间的 SERVING 行必须真的推进一格。
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	if _, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).
		DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-serving-ctrl", 1)); err != nil {
		t.Fatalf("SERVING 排空失败: %v", err)
	}
	if e.Routes.updateCalls != 1 || e.Routes.rows[roomID].Version != 2 {
		t.Fatalf("对照用例应推进一次状态并 version+1，实际 update=%d version=%d",
			e.Routes.updateCalls, e.Routes.rows[roomID].Version)
	}
}

// 条件更新 0 行时回读定位唯一真因，绝不笼统报「失败」。
func TestDrainRoomRouteConflictReadbackDistinguishesCauses(t *testing.T) {
	t.Run("版本落后回 ErrVersionConflict 并带上当前版本", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.Routes.rows[roomID].Version = 9 // 别人已经改过
		got, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).
			DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-stale", 3))
		if !errors.Is(err, model.ErrVersionConflict) || got != nil {
			t.Fatalf("expected_version 落后必须回 ErrVersionConflict，实际 err=%v reply=%+v", err, got)
		}
		// 错误文本要给出「服务端当前版本」，否则运营只能反复猜版本重试。
		if !strings.Contains(err.Error(), "version=9") || !strings.Contains(err.Error(), "expected=3") {
			t.Fatalf("冲突文本应同时含当前版本与请求版本，实际 %v", err)
		}
		assertNoWrites(t, e)
	})

	// 并发对手先排空同一节点：本请求条件更新 0 行、回读已是 DRAINING ⇒ 幂等成功。
	// 用注入缝构造（见 fakes_test.go raceDrainOnCall）——这条分支没有第二条到达路径。
	t.Run("并发下已被排空按幂等成功回显", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.Routes.raceDrainOnCall = 1
		got, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).
			DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-race", 1))
		if err != nil {
			t.Fatalf("并发排空是幂等成功而不是失败: %v", err)
		}
		if got.GetState() != rpc.RouteState_ROUTE_STATE_DRAINING || got.GetVersion() != 2 {
			t.Fatalf("应回显对手推进后的那一行（version=2，不再 +1），实际 %+v", got)
		}
		if e.Routes.rows[roomID].Version != 2 {
			t.Fatalf("幂等成功重复推进了版本，实际 %d", e.Routes.rows[roomID].Version)
		}
		if n := e.Logs.inserts; n != 0 {
			t.Fatalf("并发排空由对手留痕，本请求不该重复审计，实际 %d 行", n)
		}
		// 幂等结论仍然回填：否则同一 request_id 的第三次请求会再跑一遍条件更新。
		if v := e.Leases.requests[drainRPC+"|req-drain-race"]; v != nodeA {
			t.Fatalf("应把首次结论（主节点）回填进幂等窗口，实际 %q", v)
		}
		// 缺口：这条路径没失效路由读缓存（见交付报告）。
		// 本进程的缓存里还是那张 SERVING@nodeA，广播最多 RoomRouteCacheTTLSeconds 秒内继续打到已排空节点。
		if n := e.Leases.callCount("CacheDel"); n != 0 {
			t.Fatalf("当前实现：aff==0 分支不调 invalidateRouteCache（钉住现状），实际 %d 次", n)
		}
	})

	// 对照：正常排空这一条就必须失效缓存一次——否则上面那条断言只是「CacheDel 从不出错」。
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	if _, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).
		DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-invalidate-ctrl", 1)); err != nil {
		t.Fatalf("对照排空失败: %v", err)
	}
	if n := e.Leases.callCount("CacheDel"); n != 1 {
		t.Fatalf("正常排空应恰好失效一次路由缓存，实际 %d 次", n)
	}
}

// --- 扇出后果：排空不碰连接，但新广播不再命中被排空节点 ---

// 原桩第 8 步：DRAINING 不承接任何广播；存量连接与重连指令由 WS 接入层负责（未接线）。
//
// 「排空」这个词很容易被人实现成「顺手把连接关掉」，那是全房间断流事故。
// 本用例钉住三件事：DrainRoomRoute 一次都没触碰下发通道；排空后的广播确实 NO_ROUTE 且不扇出；
// 以及「NO_ROUTE 是因为状态而不是因为没有节点」——把 state 翻回 SERVING、副本列表不动，就必须扇出。
func TestDrainRoomRouteDoesNotCloseConnectionsButChangesFanoutTargets(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	for i, user := range []int64{mid, otherMid, anchorMid} {
		e.seedActiveLease(fmt.Sprintf("lgwl_drain_conn%d", i), fmt.Sprintf("conn-drain-%d", i), nodeA, roomID, user, model.RoleViewer)
	}
	got, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).
		DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-connections", 1))
	if err != nil {
		t.Fatalf("排空失败: %v", err)
	}
	if e.Tp.closed != 0 || len(e.Tp.fanoutReqs) != 0 || len(e.Tp.deliverReqs) != 0 {
		t.Fatalf("排空不该关连接或下发（closed=%d fanout=%d deliver=%d）", e.Tp.closed, len(e.Tp.fanoutReqs), len(e.Tp.deliverReqs))
	}
	for _, id := range []string{"lgwl_drain_conn0", "lgwl_drain_conn1", "lgwl_drain_conn2"} {
		if rec := hbGet(t, e, id); rec.State != model.LeaseStateActive {
			t.Fatalf("排空改写了存量租约 %s 的状态机: %+v", id, rec)
		}
	}
	// 读数仍然如实回显（3 条在线连接仍在房间里，只是不再接受新广播）。
	if got.GetServingConnections() != 3 {
		t.Fatalf("serving_connections 应如实回 3，实际 %d", got.GetServingConnections())
	}

	// 排空后无副本 ⇒ 没有任何可下发节点：新广播按 NO_ROUTE 丢弃，一次都不扇出。
	bc := func(messageID string) (*rpc.BroadcastToRoomReply, error) {
		return NewBroadcastToRoomLogic(ctxService(t, "deploy"), e.Svc).BroadcastToRoom(
			&rpc.BroadcastToRoomReq{RoomId: roomID, Kind: rpc.BroadcastKind_BROADCAST_KIND_SYSTEM,
				MessageId: messageID, Payload: []byte("notice")})
	}
	dropped, err := bc("msg-after-drain")
	if err != nil {
		t.Fatalf("无节点可打时是正常丢弃而不是依赖故障: %v", err)
	}
	if dropped.GetAccepted() || dropped.GetDropReason() != dropReason(model.DropNoRoute) {
		t.Fatalf("排空后广播应被 NO_ROUTE 丢弃，实际 %+v", dropped)
	}
	assertNoFanout(t, e)

	// 排空后房间仍有副本节点 ⇒ 扇出只剩副本：被排空的 A 必须从列表里消失（「只出不进」的下发口径）。
	//
	// 先把读缓存关掉（TTL=0）：上面那次广播已把当时的行存进路由缓存，
	// 不关的话这里改副本列表可能读不到，判不出到底是哪一格在起作用。
	e.apply(func(c *config.LiveGatewayConf) { c.RoomRouteCacheTTLSeconds = 0 })
	e.Routes.rows[roomID].ReplicaNodes = replicaJSON(nodeB)
	withReplica, err := bc("msg-after-drain-with-replica")
	if err != nil {
		t.Fatalf("DRAINING 房间有副本时是正常下发而不是依赖故障: %v", err)
	}
	if !withReplica.GetAccepted() || withReplica.GetDropReason() != dropReason(model.DropOK) {
		t.Fatalf("副本节点必须承接排空后的广播，实际 %+v", withReplica)
	}
	if n := len(e.Tp.fanoutReqs); n != 1 {
		t.Fatalf("排空期只允许一次扇出，实际 %d 次", n)
	}
	if nodes := e.Tp.fanoutReqs[0].Nodes; len(nodes) != 1 || nodes[0] != nodeB {
		t.Fatalf("扇出节点必须恰好是副本 %s（排空节点被排除，也不是「打全部」），实际 %v", nodeB, nodes)
	}

	// 判别性对照：同一行、同一副本列表，只把 state 翻回 SERVING ⇒ 主节点重新进扇出列表，
	// 于是上面「只剩副本」确实是被排空状态造成的，而不是「行里没有可用节点」。
	e.Routes.rows[roomID].State = model.RouteStateServing
	serving, err := bc("msg-after-unserving")
	if err != nil || !serving.GetAccepted() {
		t.Fatalf("SERVING + 副本时必须扇出，实际 err=%v reply=%+v", err, serving)
	}
	if n := len(e.Tp.fanoutReqs); n != 2 {
		t.Fatalf("对照组应再扇出一次，累计 2 次，实际 %d 次", n)
	}
	nodes := e.Tp.fanoutReqs[1].Nodes
	if !lgwFanoutContains(nodes, nodeB) {
		t.Fatalf("扇出节点里必须含副本 %s，实际 %v", nodeB, nodes)
	}
	if !lgwFanoutContains(nodes, nodeA) {
		t.Fatalf("SERVING 时主节点必须回到扇出列表，实际 %v", nodes)
	}
}

// lgwFanoutContains 只读断言辅助：扇出节点列表里有没有某个节点。
func lgwFanoutContains(nodes []string, want string) bool {
	for _, n := range nodes {
		if n == want {
			return true
		}
	}
	return false
}

// 排空后运营与广播读的是新状态：靠的是写路径主动失效缓存，不是 TTL 到期。
//
// 证明方式与 route_reads_test.go 同一路数——「让数据库说谎」：
// 先用 GetRoomRoute 把 SERVING@A 喂进缓存，排空后把 DB 行改成 SERVING@Z。
// 缓存真的被失效 ⇒ 回源，读到 Z；缓存没失效 ⇒ 读到还在缓存里的 A。
// 两个结果只会出一个，所以这条断言能真正判红黑；
// 若不失效，广播链路会继续打到已下线节点最多一个 RoomRouteCacheTTLSeconds 周期。
func TestDrainRoomRouteInvalidatesWarmedRouteCache(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	warm, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID})
	if err != nil || warm.GetNodeId() != nodeA {
		t.Fatalf("预热路由缓存失败: err=%v reply=%+v", err, warm)
	}
	if n := e.Leases.callCount("CacheSet"); n != 1 {
		t.Fatalf("缓存里必须先真的有这份 SERVING@A，否则后面的判别性不成立（CacheSet %d 次）", n)
	}

	if _, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).
		DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-cache", 1)); err != nil {
		t.Fatalf("排空失败: %v", err)
	}
	if n := e.Leases.callCount("CacheDel"); n != 1 {
		t.Fatalf("排空必须主动失效路由缓存一次，实际 CacheDel %d 次", n)
	}

	e.Routes.rows[roomID].State = model.RouteStateServing // 谎行：只可能来自 DB，不可能来自排空前的缓存
	e.Routes.rows[roomID].PrimaryNode = "gw-node-liar"
	got, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID})
	if err != nil {
		t.Fatalf("读路由失败: %v", err)
	}
	if got.GetNodeId() != "gw-node-liar" {
		t.Fatalf("读到的是排空前缓存的 %s 而不是回源结果：写路径没失效缓存", got.GetNodeId())
	}

	// 对照：正常排空后、不回毒行时，读到的必须是 DRAINING@A（证明上面那次确实回源了）。
	e3 := newTestEnv(t)
	seedRoute(e3, roomID, model.RouteStateServing, nodeA)
	if _, err := NewGetRoomRouteLogic(ctxClient(t), e3.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID}); err != nil {
		t.Fatalf("预热路由缓存失败: %v", err)
	}
	if _, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e3.Svc).
		DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-cache-real", 1)); err != nil {
		t.Fatalf("排空失败: %v", err)
	}
	after, err := NewGetRoomRouteLogic(ctxClient(t), e3.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID})
	if err != nil {
		t.Fatalf("排空后读路由失败: %v", err)
	}
	if after.GetState() != rpc.RouteState_ROUTE_STATE_DRAINING || after.GetVersion() != 2 {
		t.Fatalf("排空后排面必须读到 DRAINING@version=2，实际 %+v", after)
	}

	// 对照：TTL=0 时读路径完全不碰缓存（既不看也不写），但写路径的失效是**无条件**的。
	// 这里必须真的调一次 GetRoomRoute，否则「CacheGet 为 0」只是因为没人读过，判不出红黑。
	e2 := newTestEnv(t)
	seedRoute(e2, roomID, model.RouteStateServing, nodeA)
	e2.apply(func(c *config.LiveGatewayConf) { c.RoomRouteCacheTTLSeconds = 0 })
	if _, err := NewGetRoomRouteLogic(ctxClient(t), e2.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID}); err != nil {
		t.Fatalf("TTL=0 时读路由失败: %v", err)
	}
	if _, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e2.Svc).
		DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-cache-ttl0", 1)); err != nil {
		t.Fatalf("排空失败: %v", err)
	}
	if n := e2.Leases.callCount("CacheGet") + e2.Leases.callCount("CacheSet"); n != 0 {
		t.Fatalf("TTL=0 时读路径不该碰缓存，实际 Get+Set %d 次", n)
	}
	// 钉住现状（不是理想断言）：invalidateRouteCache 没有 TTL>0 的前置判断，
	// 关掉缓存仍会发一次 DEL。这是安全方向的那一侧 —— 若实现改成「TTL=0 就跳过失效」，
	// 运维中途把 TTL 从 30 调到 0 的那一瞬间，已在 Redis 里的旧条目就要靠 TTL 自然过期，
	// 本断言即红。
	if n := e2.Leases.callCount("CacheDel"); n != 1 {
		t.Fatalf("TTL=0 时排空仍要无条件 DEL 一次路由键，实际 CacheDel %d 次", n)
	}
}

// --- 两步迁移 ---

// target_node_id 非空即迁移：A→DRAINING（CAS）→ DRAINING→SERVING@B（一条原子 UPDATE）。
//
// 断言落在三处「反直觉但必须成立」的点上：
//  1. 不用 Register 承接 B（Register 的 fromStates 不含 DRAINING，会跳过只出不进）；
//  2. 迁移把 A 从 replica_nodes 摘掉，否则扇出还会打到 A，「只出不进」的口径就漏了；
//  3. 每一步各自留一行审计（message_id 带推进后的 version，天然不重复）。
func TestDrainRoomRouteMigrationKeepsRouteNeverEmptyAndDropsOldNode(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA, nodeA, "gw-node-c")
	e.Routes.rows[roomID].ReplicaNodes = replicaJSON("gw-node-c", nodeA) // A 同时是副本（大房间分片残留）
	req := drainReq(roomID, nodeA, "req-drain-migrate", 1)
	req.TargetNodeId = nodeB
	req.TraceId = "trace-migrate-1"

	got, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).DrainRoomRoute(req)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if got.GetState() != rpc.RouteState_ROUTE_STATE_SERVING || got.GetNodeId() != nodeB {
		t.Fatalf("迁移后必须回 SERVING@%s，实际 %+v", nodeB, got)
	}
	if e.Routes.registers != 0 {
		t.Fatalf("迁移不得用 Register 承接（Register 会跳过 DRAINING 只出不进）")
	}
	row := e.Routes.rows[roomID]
	if row.PrimaryNode != nodeB || row.State != model.RouteStateServing {
		t.Fatalf("落库结果错: %+v", row)
	}
	if row.DrainReason != "node_shutdown" || row.TraceId != "trace-migrate-1" {
		t.Fatalf("迁移必须把原因与 trace_id 留在路由行（AGENTS.md §8 处置留痕）: %+v", row)
	}
	// updated_by 落的是**请求字段**（自报值），不是归因主体：与 UpsertAccessQuota 的口径相反，
	// 这里钉住现状并在交付报告里登记（见该用例同名断言）。
	if row.UpdatedBy != "ops-field" {
		t.Fatalf("当前实现：updated_by 取请求自报 operator，实际 %q", row.UpdatedBy)
	}
	replicas, derr := row.ReplicaNodesList()
	if derr != nil {
		t.Fatalf("副本列写坏了: %v", derr)
	}
	// 承重的那半句：被排空的 A 必须从副本列里消失，否则扇出还会打到已下线节点。
	for _, n := range replicas {
		if n == nodeA {
			t.Fatalf("迁移后 A 还留在副本列，扇出会继续打到已下线节点: %v", replicas)
		}
	}
	// 钉住现状（不是理想断言）：实现是 kept = (replicas + target) - A，
	// 于是新主 B 也被写进副本列，与 primary_node 重复。
	// 影响面：fanoutTargets 对 SERVING 会把主节点排在第一位并去重，所以不会多打一次；
	// 但运营读到的 replica_nodes 与 primary_node 自相矛盾，若哪天扇出去掉去重就会翻倍下发。
	// 该冗余已写进交付报告；若实现改成 kept = replicas - A，本断言即红。
	if len(replicas) != 2 || replicas[0] != "gw-node-c" || replicas[1] != nodeB {
		t.Fatalf("当前实现：副本列 = 原副本去掉 A 再加上目标节点，实际 %v", replicas)
	}
	if row.Version != 3 {
		t.Fatalf("两步条件更新应各推进一格（1→2→3），实际 version=%d", row.Version)
	}
	if n := e.Routes.updateCalls; n != 2 {
		t.Fatalf("迁移应恰好两次条件更新，实际 %d 次", n)
	}
	if n := e.Leases.callCount("CacheDel"); n != 2 {
		t.Fatalf("每步状态变更后都要失效缓存，实际 CacheDel %d 次", n)
	}
	// 两步各一行审计，且 message_id 带推进后的 version（同一次迁移的两条事实不能互相吞掉）。
	rows := e.Logs.all()
	if len(rows) != 2 {
		t.Fatalf("应恰好两行审计，实际 %d: %+v", len(rows), rows)
	}
	if rows[0].MessageId != fmt.Sprintf("drain:%d:2", roomID) || rows[1].MessageId != fmt.Sprintf("migrate:%d:3", roomID) {
		t.Fatalf("审计定位串应为 drain:<room>:<version> / migrate:<room>:<version>，实际 %q / %q",
			rows[0].MessageId, rows[1].MessageId)
	}
	for _, r := range rows {
		if r.State != model.BroadcastLogSent || r.DropReason != model.DropOK || r.Kind != model.KindSystem {
			t.Fatalf("已生效的状态变更审计应为 SENT/OK/SYSTEM，实际 %+v", r)
		}
		if r.SenderMid != 1001 {
			t.Fatalf("审计主体只能是归因 mid，实际 %d", r.SenderMid)
		}
	}
}

// 第 2 步打不中时行必须停在 DRAINING@A：B 从未被写进路由行，所以没有「指向空节点」的脏状态。
//
// 这是整个方法最危险的分支——如果实现把 primary_node 先写成 B 再改 state，
// 中间那一刻所有新广播都会丢（fanoutTargets 读到一个不存在/未承接的节点）。
func TestDrainRoomRouteMigrationSecondStepMissLeavesRouteOnOldNode(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA, "gw-node-c")
	e.Routes.raceDrainOnCall = 2 // 第 2 步的对手抢先落地
	req := drainReq(roomID, nodeA, "req-drain-migrate-race", 1)
	req.TargetNodeId = nodeB

	got, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).DrainRoomRoute(req)
	if !errors.Is(err, model.ErrVersionConflict) || got != nil {
		t.Fatalf("第 2 步未命中要显式要求带新版本重试，不得回半份成功，实际 err=%v reply=%+v", err, got)
	}
	if !strings.Contains(err.Error(), nodeB) || !strings.Contains(err.Error(), "fresh expected_version") {
		t.Fatalf("错误要给出可执行结论（对哪个节点重试），实际 %v", err)
	}
	row := e.Routes.rows[roomID]
	if row.PrimaryNode != nodeA {
		t.Fatalf("第 2 步失败后主节点被改写，路由会指向尚未承接的节点: %+v", row)
	}
	if row.State != model.RouteStateDraining {
		t.Fatalf("第 2 步失败后行必须停在 DRAINING（只出不进），实际 state=%d", row.State)
	}
	// 第 1 步的成果要留着（不回滚），因为它已经生效；审计也只有第一步那一行。
	if n := e.Logs.inserts; n != 1 {
		t.Fatalf("第一步留一行审计、第二步未生效不留审计，实际 %d 行", n)
	}
	if n := e.Leases.callCount("CacheDel"); n != 1 {
		t.Fatalf("只有第一步改了状态，应恰好失效一次，实际 %d 次", n)
	}
}

// --- 依赖故障与降级 ---

var errDrainUpdateProbe = errors.New("route-update-state-probe")

// 依赖故障必须原样上抛：把「已生效的排空」伪装成失败会诱导运营去硬切节点，
// 把「没排空」伪装成成功更糟（见上面的 pending 用例），所以两侧都要钉。
func TestDrainRoomRouteStorageFailuresPropagateRaw(t *testing.T) {
	t.Run("DataSource 未配置", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.markStoreMissing()
		_, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).
			DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-nostore", 1))
		if !errors.Is(err, repository.ErrStoreUnavailable) {
			t.Fatalf("缺库要显式失败而不是回「无路由」，实际 %v", err)
		}
	})
	t.Run("读路由故障", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.Routes.fail("FindOne", errRouteProbe)
		if _, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).
			DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-readfail", 1)); !errors.Is(err, errRouteProbe) {
			t.Fatalf("读表故障要原样上抛（不能降级成 ErrRouteNotFound），实际 %v", err)
		}
	})
	t.Run("条件更新故障", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.Routes.fail("UpdateState", errDrainUpdateProbe)
		got, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).
			DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-writefail", 1))
		if !errors.Is(err, errDrainUpdateProbe) || got != nil {
			t.Fatalf("实际 err=%v reply=%+v", err, got)
		}
		// 半途而废不能留下任何「已完成」的痕迹：无审计、无缓存失效、行未变。
		assertNoWrites(t, e)
		if n := e.Leases.callCount("CacheDel"); n != 0 {
			t.Fatalf("状态没变更就失效缓存，实际 %d 次", n)
		}
		if row := e.Routes.rows[roomID]; row.State != model.RouteStateServing || row.DrainReason != "" {
			t.Fatalf("条件更新故障却改动了行: %+v", row)
		}
	})
	t.Run("回读定位故障", func(t *testing.T) {
		e := newTestEnv(t)
		seedRoute(e, roomID, model.RouteStateServing, nodeA)
		e.Routes.raceDrainOnCall = 1 // 条件更新 0 行 → 进入回读定位
		e.Routes.fail("FindOne", errRouteProbe)
		if _, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).
			DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-conflict-readfail", 1)); !errors.Is(err, errRouteProbe) {
			t.Fatalf("0 行时的回读故障不得被翻译成某个业务结论，实际 %v", err)
		}
	})
}

// 审计写失败只记告警：留痕已经在路由行里，把已生效的排空伪装成失败比少一行审计严重得多。
func TestDrainRoomRouteAuditFailureDoesNotUndoDrain(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	e.Logs.fail("Insert", errors.New("audit table unavailable"))
	got, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).
		DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-auditfail", 1))
	if err != nil {
		t.Fatalf("审计写失败不该推翻业务结论: %v", err)
	}
	if got.GetState() != rpc.RouteState_ROUTE_STATE_DRAINING {
		t.Fatalf("回显必须如实反映已生效的排空: %+v", got)
	}
	row := e.Routes.rows[roomID]
	if row.State != model.RouteStateDraining || row.DrainReason != "node_shutdown" {
		t.Fatalf("路由行自身要带处置留痕: %+v", row)
	}
}

// 连接数读数不可用时按 0 回显且不报错（与 GetRoomRoute 同口径：观测值不参与判定）。
func TestDrainRoomRouteServingConnectionsDegradeToZero(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	e.seedActiveLease("lgwl_drain_live", "conn-drain-live", nodeA, roomID, mid, model.RoleViewer)
	e.Leases.failWith("RoomConnectionCount", errors.New("redis unavailable"))
	got, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).
		DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-countfail", 1))
	if err != nil {
		t.Fatalf("读数不可用不该让排空失败: %v", err)
	}
	if got.GetServingConnections() != 0 {
		t.Fatalf("降级时 serving_connections 必须是 0，实际 %d", got.GetServingConnections())
	}
	if got.GetState() != rpc.RouteState_ROUTE_STATE_DRAINING || got.GetVersion() != 2 {
		t.Fatalf("降级不该动到排空结论本身: %+v", got)
	}
}

// 副本列脏数据：主节点仍有效，排空照常执行并按「无副本」投影（与广播路径同口径）。
func TestDrainRoomRouteDirtyReplicaColumnStillDrains(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	e.Routes.rows[roomID].ReplicaNodes = `{"broken":1}`
	got, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).
		DrainRoomRoute(drainReq(roomID, nodeA, "req-drain-dirty", 1))
	if err != nil {
		t.Fatalf("副本列脏不该报错: %v", err)
	}
	if n := len(got.GetReplicaNodes()); n != 0 {
		t.Fatalf("脏副本应按空回显，实际 %d 项", n)
	}
	if got.GetState() != rpc.RouteState_ROUTE_STATE_DRAINING || got.GetNodeId() != nodeA {
		t.Fatalf("排空结论本身不受影响: %+v", got)
	}
}

// Drain 只处置路由：不写配额、不发票据、不碰扇出通道。
func TestDrainRoomRouteTouchesNoQuotaOrTicket(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	req := drainReq(roomID, nodeA, "req-drain-isolate", 1)
	req.TargetNodeId = nodeB
	if _, err := NewDrainRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).DrainRoomRoute(req); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if n := e.Quotas.creates + e.Quotas.updates; n != 0 {
		t.Fatalf("排空不该写配额表，实际 %d 次", n)
	}
	if n := e.Ticket.inserts; n != 0 {
		t.Fatalf("排空不该签发重连票据（重连指令由 WS 接入层下发，本服务未接线），实际 %d 次", n)
	}
	if n := e.Leases.callCount("PutTicket") + e.Leases.callCount("Put") + e.Leases.callCount("Unsubscribe"); n != 0 {
		t.Fatalf("排空动了租约/票据有效位，实际 %d 次", n)
	}
	if n := e.Leases.callCount("BumpQuotaEpoch"); n != 0 {
		t.Fatalf("排空与配额代次无关，不该自增 epoch，实际 %d 次", n)
	}
}
