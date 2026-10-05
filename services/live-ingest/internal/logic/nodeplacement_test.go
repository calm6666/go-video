// 本文件覆盖不变量 4（节点分配：容量/权重边界、重复分配、释放幂等、失败不伪造节点）
// 与「节点注册/心跳」这两条容量账本的入口。
//
// 分两层打：
//   - 纯函数层（打分与排序）：不变量是「同输入同结果 + 硬约束不被加分碰运气盖掉」，
//     不需要库， fastest 且能把边界铺满；
//   - RPC 层（AssignIngestNode / ReleaseIngestNode / UpsertIngestNode）：
//     配额、分配记录、流上指针三处必须同事务一致，只有从入口打才证伪得了「只改了两处」。
package logic

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"
)

// --- 纯函数：打分 ---

func TestNodeAssignScore_FormulaIsHealthMinusHalfLoadPlusRegionBonus(t *testing.T) {
	cases := []struct {
		name     string
		node     *model.IngestNode
		region   string
		required bool
		want     int32
	}{
		{"空载满分", &model.IngestNode{HealthScore: 100, CapacityStreams: 10}, "", false, 100},
		{"半载扣 25", &model.IngestNode{HealthScore: 100, CapacityStreams: 10, ActiveStreams: 5}, "", false, 75},
		{"负载率向下取整", &model.IngestNode{HealthScore: 90, CapacityStreams: 4, ActiveStreams: 1}, "", false, 78},
		{"命中期望区域加 25", &model.IngestNode{HealthScore: 80, CapacityStreams: 10, Region: "cn-east-1"}, "CN-EAST-1", false, 105},
		{"区域不符只不加分", &model.IngestNode{HealthScore: 80, CapacityStreams: 10, Region: "cn-north-1"}, "cn-east-1", false, 80},
		{"配额缺失按满负载惩罚", &model.IngestNode{HealthScore: 100, CapacityStreams: 0}, "", false, 50},
		{"计数漂移超 100% 时封顶", &model.IngestNode{HealthScore: 100, CapacityStreams: 4, ActiveStreams: 9}, "", false, 50},
		{"负占用按 0 负载", &model.IngestNode{HealthScore: 60, CapacityStreams: 4, ActiveStreams: -3}, "", false, 60},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := nodeAssignScore(c.node, c.region, c.required); got != c.want {
				t.Fatalf("score=%d want=%d（health=%d cap=%d active=%d region=%s）",
					got, c.want, c.node.HealthScore, c.node.CapacityStreams, c.node.ActiveStreams, c.node.Region)
			}
		})
	}
}

func TestNodeAssignScore_HealthyFullNodeLosesToIdleNode(t *testing.T) {
	// 「权重再高也不能盖过容量」：满负载的健康节点必须排在空载的低分节点之后。
	full := &model.IngestNode{NodeID: "full", HealthScore: 100, CapacityStreams: 2, ActiveStreams: 2}
	idle := &model.IngestNode{NodeID: "idle", HealthScore: 61, CapacityStreams: 10}
	if nodeAssignScore(full, "", false) >= nodeAssignScore(idle, "", false) {
		t.Fatalf("满负载健康节点不该赢过空载节点：%d vs %d",
			nodeAssignScore(full, "", false), nodeAssignScore(idle, "", false))
	}
}

func TestNodeAssignScore_RequiredRegionMismatchIsUnusableNotLowScore(t *testing.T) {
	n := &model.IngestNode{NodeID: "far", HealthScore: 100, CapacityStreams: 10, Region: "us-west-1"}
	if got := nodeAssignScore(n, "cn-east-1", true); got != assignScoreUnusable {
		t.Fatalf("强制同区域时异区域节点应判不可用（%d），实得 %d", assignScoreUnusable, got)
	}
	mustFalse(t, assignNodeUsable(assignScoreUnusable), "不可用哨兵分")
	mustTrue(t, assignNodeUsable(assignScoreUnusable+1), "略高于哨兵的分数仍可用")
	// 同一节点在「只是偏好」口径下必须仍可用（加分为负也不该被剔除）
	mustTrue(t, assignNodeUsable(nodeAssignScore(n, "cn-east-1", false)), "偏好区域不该剔除异区域节点")
}

func TestRankIngestNodes_DropsUnusableAndIsOrderIndependent(t *testing.T) {
	mk := func(id string, health, active, cap int32, region string) *model.IngestNode {
		return &model.IngestNode{NodeID: id, HealthScore: health, ActiveStreams: active, CapacityStreams: cap, Region: region}
	}
	nodes := []*model.IngestNode{
		mk("n-b", 90, 0, 10, "cn-east-1"),
		mk("n-a", 90, 0, 10, "cn-east-1"), // 与 n-b 同分同负载：靠 node_id 定序
		mk("n-c", 99, 9, 10, "cn-north-1"),
		mk("n-x", 100, 0, 10, "us-east-1"), // 强制同区域时必须被剔除，而不是垫底
		nil,                                // 候选行可能为 nil，不能让调度路径 panic
	}
	want := []string{"n-a", "n-b"} // n-c 区域不符且强制同区域：整格剔除

	// 顺序无关：把输入倒过来排，结果必须完全一致（打分不能靠数组顺序碰运气）
	rev := make([]*model.IngestNode, 0, len(nodes))
	for i := len(nodes) - 1; i >= 0; i-- {
		rev = append(rev, nodes[i])
	}
	for _, in := range [][]*model.IngestNode{nodes, rev} {
		got := nodeIDs(rankIngestNodes(in, "cn-east-1", true))
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("排序结果不符：got=%v want=%v", got, want)
		}
	}
	// 放宽成「只偏好」后，异区域高分节点重新参与竞争并拿第一（100 无加分 > 90）
	soft := nodeIDs(rankIngestNodes(nodes, "cn-east-1", false))
	if fmt.Sprint(soft) != fmt.Sprint([]string{"n-a", "n-b", "n-x", "n-c"}) {
		t.Fatalf("偏好口径下的候选顺序不符：%v", soft)
	}
}

func TestRankIngestNodes_TieBreakPrefersLowerLoadThenNodeID(t *testing.T) {
	nodes := []*model.IngestNode{
		{NodeID: "z-empty", HealthScore: 80, CapacityStreams: 8},
		{NodeID: "y-half", HealthScore: 90, CapacityStreams: 8, ActiveStreams: 4}, // 90-25=65
		{NodeID: "x-low", HealthScore: 65, CapacityStreams: 8},                    // 65，与 y 同分但更空
	}
	got := nodeIDs(rankIngestNodes(nodes, "", false))
	want := []string{"z-empty", "x-low", "y-half"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("同分先挑负载低者：got=%v want=%v", got, want)
	}
}

func TestCheckRegion_RejectsTooLongInsteadOfTruncating(t *testing.T) {
	if got, err := checkRegion("  cn-east-1  "); err != nil || got != "cn-east-1" {
		t.Fatalf("合法区域应去空白通过：%q %v", got, err)
	}
	if got, err := checkRegion("   "); err != nil || got != "" {
		t.Fatalf("空白区域应视作未限制：%q %v", got, err)
	}
	for _, bad := range []string{"CN-EAST-1/LONG-OVER-16B", "CN EAST 1", "CN\tEAST"} {
		if _, err := checkRegion(bad); err == nil {
			t.Fatalf("区域码 %q 不该通过校验（截断会让就近匹配永远失效）", bad)
		} else if !errors.Is(err, model.ErrNodeNotFound) {
			t.Fatalf("区域码非法应回 ErrNodeNotFound 语义：%v", err)
		}
	}
}

func TestMoveNodeToFrontKeepsRelativeOrder(t *testing.T) {
	nodes := []*model.IngestNode{{NodeID: "a"}, {NodeID: "b"}, {NodeID: "c"}}
	got := nodeIDs(moveNodeToFront(nodes, "c"))
	if fmt.Sprint(got) != fmt.Sprint([]string{"c", "a", "b"}) {
		t.Fatalf("置顶后其余相对顺序应保持：%v", got)
	}
	if fmt.Sprint(nodeIDs(moveNodeToFront(nodes, "zz"))) != fmt.Sprint([]string{"a", "b", "c"}) {
		t.Fatalf("目标不在候选里时不该改动顺序")
	}
	if nodeIDOf(nil) != "" || findIngestNode(nodes, "") != nil {
		t.Fatalf("nil 候选与空 node_id 必须安全返回")
	}
}

func TestAssignReasonText_RecordsScoreSnapshotWhenCallerSilent(t *testing.T) {
	n := &model.IngestNode{NodeID: "n1", HealthScore: 80, CapacityStreams: 10, ActiveStreams: 5}
	text := assignReasonText("", n, nodeAssignScore(n, "", false))
	for _, want := range []string{"n1", "score=55", "health=80", "load=50%"} {
		if !strings.Contains(text, want) {
			t.Fatalf("打分快照缺少 %q：%s", want, text)
		}
	}
	if got := assignReasonText("主播重连回原节点", n, 12); got != "主播重连回原节点" {
		t.Fatalf("调用方给了原因就该以它为准（审计要与操作者说的一致）：%s", got)
	}
}

// --- RPC 层：分配 ---

func TestAssignIngestNode_ReservesQuotaWritesLeaseAndPointsStreamAtomically(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedStream(t, &model.Stream{StreamID: "S-ASG", RoomID: 41, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1})
	e.seedNode(t, &model.IngestNode{NodeID: "n1", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 3, HealthScore: 90, Region: "cn-east-1"})

	// 分数要能在「打分那一刻的节点快照」上复算：运维从分配记录反推「为什么是它」。
	preScore := nodeAssignScore(e.nodeRow(t, "n1"), "cn-east-1", false)
	before := e.effects()
	reply, err := e.assign(t, s.StreamID, rpc.IngestProtocol_PROTOCOL_RTMP, "asg-1", func(r *rpc.AssignIngestNodeReq) {
		r.PreferRegion = "cn-east-1"
	})
	wantOK(t, reply, err, "首次分配")
	if reply.GetNodeId() != "n1" || reply.GetReplayed() {
		t.Fatalf("分配结果不符：%+v", reply)
	}
	if got := e.nodeRow(t, "n1").ActiveStreams; got != 1 {
		t.Fatalf("配额未占用：active=%d", got)
	}
	if got := e.streamRow(t, s.StreamID).NodeID; got != "n1" {
		t.Fatalf("流上未回填节点指针：%q", got)
	}
	lease, err := e.repo.NodeAssignment.FindActiveByStream(bg(), nil, s.StreamID)
	wantOK(t, lease, err, "查询租约")
	if lease == nil || lease.State != model.AssignmentStateActive || lease.RequestID != "asg-1" {
		t.Fatalf("分配记录不符：%+v", lease)
	}
	// 分数落库要能被复算（运维从记录反推「为什么是它」）
	if lease.Score != preScore {
		t.Fatalf("分配记录里的分数=%d 复算应为=%d", lease.Score, preScore)
	}
	// 分配只该产生「1 条租约」这一份行数变化（节点计数是原地 UPDATE，不入行数）
	e.requireDelta(t, before, [9]int{3: 1}, "首次分配的写入集合")
}

func TestAssignIngestNode_NoCandidateFailsClosedWithoutFakeNode(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedStream(t, &model.Stream{StreamID: "S-NODE", RoomID: 42, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1})
	// 唯一的节点已经满配额：必须显式失败，且一个字段都不写。
	e.seedNode(t, &model.IngestNode{NodeID: "full", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 2, ActiveStreams: 2, HealthScore: 100})

	before := e.effects()
	reply, err := e.assign(t, s.StreamID, rpc.IngestProtocol_PROTOCOL_RTMP, "asg-full")
	wantFail(t, err, model.ErrNoAvailableNode, "无可用节点")
	if reply != nil {
		t.Fatalf("失败却回了响应（入口会照着一个空 node_id 去连）：%+v", reply)
	}
	e.requireSameEffects(t, before, "无可用节点必须零副作用")
	if got := e.streamRow(t, s.StreamID).NodeID; got != "" {
		t.Fatalf("分配失败却给流挂了节点：%s", got)
	}
	if n := len(e.db.nodes); n != 1 {
		t.Fatalf("分配路径擅自建了节点行：nodes=%d", n)
	}
}

func TestAssignIngestNode_OfflineAndWrongProtocolNodesAreNotCandidates(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedStream(t, &model.Stream{StreamID: "S-FILT", RoomID: 43, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1})
	e.seedNode(t, &model.IngestNode{NodeID: "off", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 5, HealthScore: 100, State: model.NodeStateOffline})
	e.seedNode(t, &model.IngestNode{NodeID: "srt", ProtocolMask: model.ProtocolMaskSrt,
		CapacityStreams: 5, HealthScore: 100})

	_, err := e.assign(t, s.StreamID, rpc.IngestProtocol_PROTOCOL_RTMP, "asg-filt")
	wantFail(t, err, model.ErrNoAvailableNode, "只有离线/异协议节点时不该分配")
	if n := len(e.db.assigns); n != 0 {
		t.Fatalf("把流放到了离线或异协议节点上：assigns=%d", n)
	}

	// 补一个在线同协议节点后即可分配，并且它必须赢
	e.seedNode(t, &model.IngestNode{NodeID: "ok", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 5, HealthScore: 40})
	reply, err := e.assign(t, s.StreamID, rpc.IngestProtocol_PROTOCOL_RTMP, "asg-filt-2")
	wantOK(t, reply, err, "有可用节点后分配")
	if reply.GetNodeId() != "ok" {
		t.Fatalf("不该选离线或异协议节点，实得 %s", reply.GetNodeId())
	}
}

func TestAssignIngestNode_ReplayedRequestIdYieldsSameNodeAndOneQuota(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedStream(t, &model.Stream{StreamID: "S-REASG", RoomID: 44, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1})
	e.seedNode(t, &model.IngestNode{NodeID: "n1", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 5, HealthScore: 90})

	first, err := e.assign(t, s.StreamID, rpc.IngestProtocol_PROTOCOL_RTMP, "asg-replay")
	wantOK(t, first, err, "首次分配")

	before := e.effects()
	second, err := e.assign(t, s.StreamID, rpc.IngestProtocol_PROTOCOL_RTMP, "asg-replay",
		func(r *rpc.AssignIngestNodeReq) { r.PreferRegion = "cn-east-1" }) // 换个入参也不该重算
	wantOK(t, second, err, "重放分配")
	mustTrue(t, second.GetReplayed(), "重放标记")
	if second.GetNodeId() != first.GetNodeId() || second.GetAssignmentId() != first.GetAssignmentId() {
		t.Fatalf("同一 request_id 返回了不同分配：%s/%d vs %s/%d",
			first.GetNodeId(), first.GetAssignmentId(), second.GetNodeId(), second.GetAssignmentId())
	}
	e.requireSameEffects(t, before, "重放分配必须零写入")
	e.requireNoTransaction(t, before, "request_id 重放应在事务前回放")
	if got := e.nodeRow(t, "n1").ActiveStreams; got != 1 {
		t.Fatalf("重放多占了一份配额：active=%d", got)
	}
	if n := e.call("IngestNode.ReserveQuota"); n != 1 {
		t.Fatalf("重放又抢占了一次配额：ReserveQuota 调用 %d 次", n)
	}
}

func TestAssignIngestNode_CrossStreamRequestIdReuseIsRejected(t *testing.T) {
	e := newTestEnv(t)
	s1 := e.seedStream(t, &model.Stream{StreamID: "S-X1", RoomID: 45, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1})
	s2 := e.seedStream(t, &model.Stream{StreamID: "S-X2", RoomID: 46, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1})
	e.seedNode(t, &model.IngestNode{NodeID: "n1", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 5, HealthScore: 90})
	if _, err := e.assign(t, s1.StreamID, rpc.IngestProtocol_PROTOCOL_RTMP, "shared-req"); err != nil {
		t.Fatalf("首次分配：%v", err)
	}

	before := e.effects()
	_, err := e.assign(t, s2.StreamID, rpc.IngestProtocol_PROTOCOL_RTMP, "shared-req")
	wantFail(t, err, model.ErrIdempotencyKeyRequired, "幂等键跨流复用")
	e.requireSameEffects(t, before, "跨流复用幂等键必须零副作用")
	if got := e.streamRow(t, s2.StreamID).NodeID; got != "" {
		t.Fatalf("被拒的请求把第二条流也挂上了节点：%s", got)
	}
}

func TestAssignIngestNode_DuplicateActiveLeaseIsNotDoubleCounted(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedStream(t, &model.Stream{StreamID: "S-DUP", RoomID: 47, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1})
	e.seedNode(t, &model.IngestNode{NodeID: "n1", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 5, HealthScore: 90})
	if _, err := e.assign(t, s.StreamID, rpc.IngestProtocol_PROTOCOL_RTMP, "dup-1"); err != nil {
		t.Fatalf("首次分配：%v", err)
	}

	before := e.effects()
	// 不同 request_id、同一已分配流：重连常态，直接回当前节点，一条新记录都不该有。
	again, err := e.assign(t, s.StreamID, rpc.IngestProtocol_PROTOCOL_RTMP, "dup-2")
	wantOK(t, again, err, "已有租约时再分配")
	if again.GetNodeId() != "n1" {
		t.Fatalf("未复用当前节点：%s", again.GetNodeId())
	}
	e.requireSameEffects(t, before, "已有生效租约时不得再写分配")
	if got := e.nodeRow(t, "n1").ActiveStreams; got != 1 {
		t.Fatalf("重复分配把配额记成 %d（应为 1）", got)
	}
}

func TestAssignIngestNode_MigrationReservesNewBeforeReleasingOld(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedAssignedNode(t, &model.Stream{StreamID: "S-MIG", RoomID: 48, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1}, &model.IngestNode{NodeID: "old", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 5, HealthScore: 90}, "mig-old")
	e.seedNode(t, &model.IngestNode{NodeID: "new", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 5, HealthScore: 95})
	if got := e.nodeRow(t, "old").ActiveStreams; got != 1 {
		t.Fatalf("种子后旧节点占用应为 1，实得 %d", got)
	}

	reply, err := e.assign(t, s.StreamID, rpc.IngestProtocol_PROTOCOL_RTMP, "mig-new",
		func(r *rpc.AssignIngestNodeReq) { r.ForceReassign = true })
	wantOK(t, reply, err, "迁移")
	if reply.GetNodeId() != "new" || reply.GetPrevNodeId() != "old" {
		t.Fatalf("迁移方向不符：node=%s prev=%s", reply.GetNodeId(), reply.GetPrevNodeId())
	}
	if got := e.nodeRow(t, "old").ActiveStreams; got != 0 {
		t.Fatalf("旧节点配额未归还：active=%d", got)
	}
	if got := e.nodeRow(t, "new").ActiveStreams; got != 1 {
		t.Fatalf("新节点配额未占用：active=%d", got)
	}
	if got := e.streamRow(t, s.StreamID).NodeID; got != "new" {
		t.Fatalf("流指针未迁移：%s", got)
	}
	oldLease, err := e.repo.NodeAssignment.FindOne(bg(), reply.GetAssignmentId())
	wantOK(t, oldLease, err, "回看新租约")
	if oldLease.PrevNodeID != "old" || oldLease.State != model.AssignmentStateActive {
		t.Fatalf("新租约不符：%+v", oldLease)
	}
	// 旧记录必须留在库里（MIGRATED），审计要能还原整条迁移链
	var migrated int
	for _, a := range e.db.assigns {
		if a.State == model.AssignmentStateMigrated {
			migrated++
		}
	}
	if migrated != 1 {
		t.Fatalf("迁移旧租约应置 MIGRATED 并保留，实得 %d 行", migrated)
	}
}

func TestAssignIngestNode_MigrationRefusesToMoveBackToSameNode(t *testing.T) {
	e := newTestEnv(t)
	// 只有一个候选节点，且它就是当前租约持有者：迁移等于没迁移，必须失败而不是自迁自。
	s := e.seedAssignedNode(t, &model.Stream{StreamID: "S-SAME", RoomID: 49, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1}, &model.IngestNode{NodeID: "only",
		ProtocolMask: model.ProtocolMaskRtmp, CapacityStreams: 5, HealthScore: 90}, "same-1")

	_, err := e.assign(t, s.StreamID, rpc.IngestProtocol_PROTOCOL_RTMP, "same-2",
		func(r *rpc.AssignIngestNodeReq) { r.ForceReassign = true })
	wantFail(t, err, model.ErrNoAvailableNode, "唯一候选就是原节点")
	if got := e.nodeRow(t, "only").ActiveStreams; got != 1 {
		t.Fatalf("被拒的迁移改动了配额：active=%d", got)
	}
	lease, err := e.repo.NodeAssignment.FindActiveByStream(bg(), nil, s.StreamID)
	wantOK(t, lease, err, "查询租约")
	if lease == nil || lease.RequestID != "same-1" {
		t.Fatalf("被拒的迁移动了生效租约：%+v", lease)
	}
}

func TestAssignIngestNode_ProtocolMismatchAndTerminalStreamRejected(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedStream(t, &model.Stream{StreamID: "S-PROTO", RoomID: 50, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1})
	e.seedNode(t, &model.IngestNode{NodeID: "both", ProtocolMask: model.ProtocolMaskRtmp | model.ProtocolMaskSrt,
		CapacityStreams: 5, HealthScore: 90})

	before := e.effects()
	_, err := e.assign(t, s.StreamID, rpc.IngestProtocol_PROTOCOL_SRT, "proto-1")
	wantFail(t, err, model.ErrNodeProtocolMismatch, "流的接入协议不可改")
	e.requireSameEffects(t, before, "协议不符必须零副作用")

	stopped := e.seedStream(t, &model.Stream{StreamID: "S-TERM", RoomID: 51, Protocol: 1,
		State: model.StreamStateStopped, Seq: 3})
	afterSeeds := e.effects() // 副作用基线要在两个种子之后取
	_, err = e.assign(t, stopped.StreamID, rpc.IngestProtocol_PROTOCOL_RTMP, "proto-2")
	wantFail(t, err, model.ErrTerminalStream, "终态流不可分配")

	_, err = e.assign(t, "S-GHOST", rpc.IngestProtocol_PROTOCOL_RTMP, "proto-3")
	wantFail(t, err, model.ErrStreamNotFound, "未知流")
	e.requireSameEffects(t, afterSeeds, "三类拒绝都不得留下分配")
}

func TestAssignIngestNode_PreferredNodeRequirementFailsLoudly(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedStream(t, &model.Stream{StreamID: "S-PREF", RoomID: 52, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1})
	e.seedNode(t, &model.IngestNode{NodeID: "good", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 5, HealthScore: 90})
	e.seedNode(t, &model.IngestNode{NodeID: "full", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 1, ActiveStreams: 1, HealthScore: 90})
	e.seedNode(t, &model.IngestNode{NodeID: "srt", ProtocolMask: model.ProtocolMaskSrt,
		CapacityStreams: 5, HealthScore: 99})

	cases := []struct {
		name string
		node string
		want error
	}{
		{"未注册节点", "nope", model.ErrNodeNotFound},
		{"在线但已满配额", "full", model.ErrNodeNotAssignable},
		{"协议不支持", "srt", model.ErrNodeProtocolMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := e.effects()
			_, err := e.assign(t, s.StreamID, rpc.IngestProtocol_PROTOCOL_RTMP, "pref-"+c.node,
				func(r *rpc.AssignIngestNodeReq) { r.PreferNodeId = c.node })
			wantFail(t, err, c.want, "指定节点不可用")
			e.requireSameEffects(t, before, "指定节点不可用时必须零副作用")
			// 关键：绝不静默换成别的可用节点（入口已把「回原节点」的地址下发给客户端）
			if got := e.streamRow(t, s.StreamID).NodeID; got != "" {
				t.Fatalf("擅自换节点了：%s", got)
			}
		})
	}
	// 指定节点可用时它必须赢，即使分数不是最高
	reply, err := e.assign(t, s.StreamID, rpc.IngestProtocol_PROTOCOL_RTMP, "pref-good",
		func(r *rpc.AssignIngestNodeReq) { r.PreferNodeId = "good" })
	wantOK(t, reply, err, "指定可用节点")
	if reply.GetNodeId() != "good" {
		t.Fatalf("未按指定节点分配：%s", reply.GetNodeId())
	}
}

func TestAssignIngestNode_RegionHardConstraintDoesNotFallBackCrossRegion(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedStream(t, &model.Stream{StreamID: "S-RG", RoomID: 53, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1})
	e.seedNode(t, &model.IngestNode{NodeID: "far", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 5, HealthScore: 100, Region: "cn-north-1"})
	e.svc.Config.Cdn.DefaultAssignSameRegion = true

	_, err := e.assign(t, s.StreamID, rpc.IngestProtocol_PROTOCOL_RTMP, "rg-hard",
		func(r *rpc.AssignIngestNodeReq) { r.PreferRegion = "cn-east-1" })
	wantFail(t, err, model.ErrNoAvailableNode, "强制同区域时不得跨区兜底")

	// 放宽成偏好后同一节点应被选中，且分数里没有区域加分
	e.svc.Config.Cdn.DefaultAssignSameRegion = false
	reply, err := e.assign(t, s.StreamID, rpc.IngestProtocol_PROTOCOL_RTMP, "rg-soft",
		func(r *rpc.AssignIngestNodeReq) { r.PreferRegion = "cn-east-1" })
	wantOK(t, reply, err, "偏好区域可跨区")
	if reply.GetNodeId() != "far" || reply.GetScore() != 100 {
		t.Fatalf("偏好口径下应为 far/score=100，实得 %s/%d", reply.GetNodeId(), reply.GetScore())
	}
}

func TestAssignIngestNode_InputValidationAndNoRepository(t *testing.T) {
	e := newTestEnv(t)
	for _, c := range []struct {
		name string
		mut  func(*rpc.AssignIngestNodeReq)
		want error
	}{
		{"空流 ID", func(r *rpc.AssignIngestNodeReq) { r.StreamId = "" }, model.ErrInvalidStreamId},
		{"缺幂等键", func(r *rpc.AssignIngestNodeReq) { r.RequestId = "" }, model.ErrIdempotencyKeyRequired},
		{"协议未指定", func(r *rpc.AssignIngestNodeReq) { r.Protocol = rpc.IngestProtocol_PROTOCOL_UNSPECIFIED }, model.ErrInvalidProtocol},
		{"区域码超长", func(r *rpc.AssignIngestNodeReq) { r.PreferRegion = "REGION-LONGER-THAN-16" }, model.ErrNodeNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := e.effects()
			in := &rpc.AssignIngestNodeReq{StreamId: "S-VAL", RequestId: "val-1",
				Protocol: rpc.IngestProtocol_PROTOCOL_RTMP}
			c.mut(in)
			l := NewAssignIngestNodeLogic(bg(), e.svc)
			_, err := l.AssignIngestNode(in)
			wantFail(t, err, c.want, c.name)
			e.requireSameEffects(t, before, c.name)
			e.requireNoTransaction(t, before, c.name+"：入参校验应在取候选之前")
		})
	}
	svcCtx := withoutRepoEnv(t)
	l := NewAssignIngestNodeLogic(bg(), svcCtx)
	reply, err := l.AssignIngestNode(&rpc.AssignIngestNodeReq{
		StreamId: "S-1", RequestId: "r", Protocol: rpc.IngestProtocol_PROTOCOL_RTMP,
	})
	wantFail(t, err, errNoRepository, "未装配 Repository 的分配")
	if reply != nil {
		t.Fatalf("未装配依赖却回了响应")
	}
}

// --- RPC 层：释放 ---

func TestReleaseIngestNode_ReleasesLeaseQuotaAndPointer(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedAssignedNode(t, &model.Stream{StreamID: "S-REL", RoomID: 54, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1}, &model.IngestNode{NodeID: "n1", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 5, HealthScore: 90}, "rel-1")

	before := e.effects()
	reply, err := e.release(t, s.StreamID, "rel-r1", func(r *rpc.ReleaseIngestNodeReq) { r.NodeId = "n1" })
	wantOK(t, reply, err, "释放")
	mustTrue(t, reply.GetReleased(), "释放标记")
	if reply.GetNodeId() != "n1" {
		t.Fatalf("回显的释放节点不符：%s", reply.GetNodeId())
	}
	if got := e.nodeRow(t, "n1").ActiveStreams; got != 0 {
		t.Fatalf("配额未归还：active=%d", got)
	}
	row := e.streamRow(t, s.StreamID)
	if row.NodeID != "" {
		t.Fatalf("流指针未清：%s", row.NodeID)
	}
	lease, err := e.repo.NodeAssignment.FindActiveByStream(bg(), nil, s.StreamID)
	wantOK(t, lease, err, "查询租约")
	if lease != nil {
		t.Fatalf("仍有生效租约：assignment=%d", lease.AssignmentID)
	}
	// 释放是原地 UPDATE 租约行：不新增行，也不动配额以外的表
	e.requireSameEffects(t, before, "释放不得新增任何行")
}

func TestReleaseIngestNode_UnheldNodeIsIdempotentNoop(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedAssignedNode(t, &model.Stream{StreamID: "S-IDEM", RoomID: 55, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1}, &model.IngestNode{NodeID: "n1", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 5, HealthScore: 90}, "idem-1")

	if _, err := e.release(t, s.StreamID, "idem-2"); err != nil {
		t.Fatalf("首次释放：%v", err)
	}
	before := e.effects()
	// 第二次：已经没有生效租约了，必须幂等成功且一行都不动。
	again, err := e.release(t, s.StreamID, "idem-3")
	wantOK(t, again, err, "重复释放")
	mustFalse(t, again.GetReleased(), "重复释放不得谎称归还了配额")
	e.requireSameEffects(t, before, "重复释放必须零副作用")
	if got := e.nodeRow(t, "n1").ActiveStreams; got != 0 {
		t.Fatalf("重复释放把配额扣成了 %d", got)
	}

	// 带 node_id 护栏时也一样：不持有的节点不能被「顺手」扣配额
	other, err := e.release(t, s.StreamID, "idem-4", func(r *rpc.ReleaseIngestNodeReq) { r.NodeId = "n1" })
	wantOK(t, other, err, "护栏式重复释放")
	e.requireSameEffects(t, before, "未持有节点的释放必须零副作用")
}

func TestReleaseIngestNode_WrongNodeGuardRejectsWithoutTouchingHolder(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedAssignedNode(t, &model.Stream{StreamID: "S-GUARD", RoomID: 56, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1}, &model.IngestNode{NodeID: "holder", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 5, HealthScore: 90}, "guard-1")
	e.seedNode(t, &model.IngestNode{NodeID: "innocent", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 5, HealthScore: 90})

	before := e.effects()
	_, err := e.release(t, s.StreamID, "guard-2", func(r *rpc.ReleaseIngestNodeReq) { r.NodeId = "innocent" })
	wantFail(t, err, model.ErrLeaseNotHeld, "释放非当前持有节点")
	e.requireSameEffects(t, before, "护栏拒绝必须零副作用")
	if got := e.nodeRow(t, "holder").ActiveStreams; got != 1 {
		t.Fatalf("误释放扣掉了真正持有者的配额：active=%d", got)
	}
	if got := e.nodeRow(t, "innocent").ActiveStreams; got != 0 {
		t.Fatalf("无辜节点被扣了配额：active=%d", got)
	}
}

func TestReleaseIngestNode_QuotaDrainRollsBackLeaseRelease(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedAssignedNode(t, &model.Stream{StreamID: "S-DRAIN", RoomID: 57, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1}, &model.IngestNode{NodeID: "n1", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 5, HealthScore: 90}, "drain-1")
	// 制造「租约还在、计数已被清零」的账本断裂现场（等价于节点行被删）。
	e.db.nodes["n1"].ActiveStreams = 0

	before := e.effects()
	_, err := e.release(t, s.StreamID, "drain-2")
	wantFail(t, err, model.ErrNodeNotFound, "配额无处可还")
	if lease, lerr := e.repo.NodeAssignment.FindActiveByStream(bg(), nil, s.StreamID); lerr != nil || lease == nil {
		t.Fatalf("回滚后生效租约应仍在：lease=%+v err=%v", lease, lerr)
	}
	if got := e.streamRow(t, s.StreamID).NodeID; got != "n1" {
		t.Fatalf("失败却清了流指针：%s", got)
	}
	e.requireSameEffects(t, before, "配额断裂必须整体回滚，不能只做一半")
}

func TestReleaseIngestNode_UnknownStreamAndMissingDeps(t *testing.T) {
	e := newTestEnv(t)
	before := e.effects()
	_, err := e.release(t, "S-GHOST", "ghost-1")
	wantFail(t, err, model.ErrStreamNotFound, "未知流的释放")
	e.requireSameEffects(t, before, "未知流必须零副作用")

	_, err = NewReleaseIngestNodeLogic(bg(), withoutRepoEnv(t)).ReleaseIngestNode(
		&rpc.ReleaseIngestNodeReq{StreamId: "S-1", RequestId: "r"})
	wantFail(t, err, errNoRepository, "未装配 Repository 的释放")

	_, err = e.release(t, "S-OK", "")
	wantFail(t, err, model.ErrIdempotencyKeyRequired, "释放缺幂等键")
}

// --- RPC 层：注册与心跳（容量账本的源头） ---

func TestUpsertIngestNode_RegistrationRequiresCapacityAndOperator(t *testing.T) {
	e := newTestEnv(t)
	reg := func(op int64, node *rpc.IngestNodeInfo, create bool) (*rpc.UpsertIngestNodeReply, error) {
		return NewUpsertIngestNodeLogic(bg(), e.svc).UpsertIngestNode(
			&rpc.UpsertIngestNodeReq{Node: node, OperatorMid: op, CreateIfAbsent: create})
	}
	// 运维面写：没有归因主体就不受理
	_, err := reg(0, &rpc.IngestNodeInfo{NodeId: "n9", Protocols: []rpc.IngestProtocol{rpc.IngestProtocol_PROTOCOL_RTMP},
		CapacityStreams: 5}, true)
	wantFail(t, err, model.ErrOperatorRequired, "无操作者注册")

	// 首次建档必须显式声明：拼错的 node_id 不该凭空造出可分配节点
	_, err = reg(9, &rpc.IngestNodeInfo{NodeId: "typo", Protocols: []rpc.IngestProtocol{rpc.IngestProtocol_PROTOCOL_RTMP},
		CapacityStreams: 5}, false)
	wantFail(t, err, model.ErrNodeNotFound, "未声明建档时注册未知节点")

	// 一个协议都不支持的节点进池永远不会被选中，注册它只是制造脏数据
	_, err = reg(9, &rpc.IngestNodeInfo{NodeId: "n9", CapacityStreams: 5}, true)
	wantFail(t, err, model.ErrInvalidProtocol, "无协议注册")

	// 配额缺失的节点按满负载打分，压根不该进池
	_, err = reg(9, &rpc.IngestNodeInfo{NodeId: "n9", Protocols: []rpc.IngestProtocol{rpc.IngestProtocol_PROTOCOL_RTMP},
		CapacityStreams: 0}, true)
	wantFail(t, err, model.ErrNodeNotAssignable, "零配额注册")

	reply, err := reg(9, &rpc.IngestNodeInfo{NodeId: "n9", Protocols: []rpc.IngestProtocol{rpc.IngestProtocol_PROTOCOL_RTMP},
		CapacityStreams: 5, HealthScore: 88}, true)
	wantOK(t, reply, err, "注册")
	mustTrue(t, reply.GetCreated(), "首次注册建档标记")
	if reply.GetNode().GetCapacityStreams() != 5 || reply.GetNode().GetHealthScore() != 88 {
		t.Fatalf("注册回显与入参不符：%+v", reply.GetNode())
	}
}

func TestUpsertIngestNode_HeartbeatNeverOverwritesConfigNorCreatesNode(t *testing.T) {
	e := newTestEnv(t)
	e.seedNode(t, &model.IngestNode{NodeID: "hb", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 8, HealthScore: 70, Region: "cn-east-1"})
	hb := func(req *rpc.UpsertIngestNodeReq) (*rpc.UpsertIngestNodeReply, error) {
		return NewUpsertIngestNodeLogic(bg(), e.svc).UpsertIngestNode(req)
	}

	// 未注册节点的心跳绝不静默建档：否则一个伪造心跳就能把节点塞进分配池
	_, err := hb(&rpc.UpsertIngestNodeReq{OperatorMid: 9, HeartbeatOnly: true,
		Node: &rpc.IngestNodeInfo{NodeId: "ghost", ActiveStreams: 3,
			Protocols: []rpc.IngestProtocol{rpc.IngestProtocol_PROTOCOL_RTMP}}})
	wantFail(t, err, model.ErrNodeNotFound, "未注册节点心跳")
	if _, ok := e.db.nodes["ghost"]; ok {
		t.Fatalf("心跳凭空建出了节点")
	}

	reply, err := hb(&rpc.UpsertIngestNodeReq{OperatorMid: 9, HeartbeatOnly: true,
		Node: &rpc.IngestNodeInfo{NodeId: "hb", ActiveStreams: 3, HealthScore: 55, CapacityStreams: 999,
			Region: "us-west-1", Protocols: []rpc.IngestProtocol{rpc.IngestProtocol_PROTOCOL_SRT}}})
	wantOK(t, reply, err, "心跳")
	row := e.nodeRow(t, "hb")
	if row.ActiveStreams != 3 || row.HealthScore != 55 {
		t.Fatalf("心跳未刷新占用与健康分：active=%d health=%d", row.ActiveStreams, row.HealthScore)
	}
	// 心跳只碰计数列：注册字段被刷写会让运维改的配额被节点自己改回去
	if row.CapacityStreams != 8 || row.Region != "cn-east-1" || row.ProtocolMask != model.ProtocolMaskRtmp {
		t.Fatalf("心跳覆盖了注册字段：cap=%d region=%s mask=%d", row.CapacityStreams, row.Region, row.ProtocolMask)
	}

	// 注册路径（非心跳）不许改派生计数：否则并发分配的结果会被一次注册抹掉
	if _, err := hb(&rpc.UpsertIngestNodeReq{OperatorMid: 9, CreateIfAbsent: true,
		Node: &rpc.IngestNodeInfo{NodeId: "hb", CapacityStreams: 12, ActiveStreams: 0,
			Protocols: []rpc.IngestProtocol{rpc.IngestProtocol_PROTOCOL_RTMP}}}); err != nil {
		t.Fatalf("重复注册：%v", err)
	}
	if got := e.nodeRow(t, "hb").ActiveStreams; got != 3 {
		t.Fatalf("注册改写了派生计数：active=%d（应为心跳留下的 3）", got)
	}
	if got := e.nodeRow(t, "hb").CapacityStreams; got != 12 {
		t.Fatalf("注册未更新容量：capacity=%d", got)
	}
}

// --- 测试辅助 ---

// withoutRepoEnv 复刻「ServiceContext 里没装配 Repository」的裸启动形态。
// 被拒的调用必须回显错误而不是零值成功，否则入口会以为「分配成功」。
func withoutRepoEnv(t *testing.T) *svc.ServiceContext {
	t.Helper()
	e := newTestEnv(t)
	return e.withoutRepository()
}

func (e *testEnv) assign(t *testing.T, streamID string, proto rpc.IngestProtocol, requestID string,
	mut ...func(*rpc.AssignIngestNodeReq)) (*rpc.AssignIngestNodeReply, error) {
	t.Helper()
	in := &rpc.AssignIngestNodeReq{StreamId: streamID, Protocol: proto, RequestId: requestID}
	for _, m := range mut {
		m(in)
	}
	return NewAssignIngestNodeLogic(bg(), e.svc).AssignIngestNode(in)
}

func (e *testEnv) release(t *testing.T, streamID, requestID string,
	mut ...func(*rpc.ReleaseIngestNodeReq)) (*rpc.ReleaseIngestNodeReply, error) {
	t.Helper()
	in := &rpc.ReleaseIngestNodeReq{StreamId: streamID, RequestId: requestID}
	for _, m := range mut {
		m(in)
	}
	return NewReleaseIngestNodeLogic(bg(), e.svc).ReleaseIngestNode(in)
}

func nodeIDs(nodes []*model.IngestNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			out = append(out, "<nil>")
			continue
		}
		out = append(out, n.NodeID)
	}
	return out
}
