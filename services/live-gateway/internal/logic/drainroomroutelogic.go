package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// drainReasonMaxBytes 与迁移 SQL 的 live_gw_room_route.drain_reason VARCHAR(64) 对齐。
// gate.requireReason 允许 256 字节（那是给日志与通用审计的宽度），落到本列会被静默截断，
// 截断后的原因与运营敲进去的不是同一条记录——审计里出现「半个原因」比报错更难复盘。
const drainReasonMaxBytes = 64

// drainRPC 是 request_id 幂等窗口用的伪 rpc 名（LeaseStore.ClaimRequest 的键空间）。
const drainRPC = "drain-room-route"

type DrainRoomRouteLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDrainRoomRouteLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DrainRoomRouteLogic {
	return &DrainRoomRouteLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 排空某节点上的房间路由（优雅下线，版本号乐观校验）
func (l *DrainRoomRouteLogic) DrainRoomRoute(in *rpc.DrainRoomRouteReq) (*rpc.RoomRouteInfo, error) {
	// 已实现行为（发布期节点优雅下线的唯一入口，错一步就是全房间断流）：
	// 1. 门禁顺序：处置类写门禁（gate.requireDispositionWrite，默认 fail-closed）→ 参数 → 幂等窗口。
	//    权限先于写存储：未归因主体连幂等键都不该占住。
	// 2. 参数：room_id>0、node_id 经 CleanNodeID 归一且非空、reason 必填且不超列宽、
	//    operator 必填（gate.requireOperatorField）、request_id 必填、expected_version>0
	//    （缺版本等于放弃乐观并发：两个发布脚本同时排空同一房间时会互相覆盖）。
	// 3. 幂等：request_id 命中时**只重读、零写入、零审计**，直接回显当前行（重复排空不再 version+1，
	//    否则发布重试会把版本号刷飞，后续 CAS 全部冲突）。首次请求失败在回填结论之前，
	//    窗口里留下 "pending"，重试按未受理重新执行一遍——排空本身是条件 UPDATE，可安全重放。
	// 4. 只能排空自己节点的路由：FindOne 后 primary_node != node_id → ErrPermissionDenied +
	//    一行 DENIED 审计（越权排空别人节点会让房间广播凭空消失，是本方法最高收益的攻击面）。
	// 5. 已 DRAINING / 已幂等：按成功返回当前行，不报错也不重复审计（proto 幂等键列即 request_id +
	//    expected_version 乐观锁；同一次排空的重放不产生新事实）。
	// 6. 状态机：只有 SERVING→DRAINING 合法（model.IsValidRouteTransition + UpdateState 的
	//    fromStates 双保险），OFFLINE→DRAINING 一律 ErrInvalidTransition。
	// 7. target_node_id 非空时是「迁移」，实现为两步条件更新：
	//    A→DRAINING（带 expected_version 的 CAS，只出不进）→ DRAINING→SERVING 且 primary_node=B。
	//    不用 RoomRoutes.Register 承接 B：Register 的 fromStates 只有 SERVING/OFFLINE，
	//    对 DRAINING 行按设计返回 ErrRouteDraining（严禁复活排空中的节点），先 Register 又会让
	//    A 直接消失、跳过只出不进。两步都各自原子，B 只与 state=SERVING 同一条语句写入，
	//    因此**任何时刻都不存在「路由指向空节点」**：第 2 步失败时行停在 DRAINING@A，
	//    回滚到新节点不可见的状态并返回 ErrVersionConflict 让运营带新版本重试。
	//    迁移同时把 A 从 replica_nodes 摘掉，否则扇出还会打到 A（只出不进的口径就漏了）。
	// 8. 扇出后果：fanoutTargets 在 state=DRAINING 时不把 primary 计入下发节点，
	//    所以排空后新广播不再命中 A；存量连接的重连指令由 WS 接入层读取 DRAINING 后下发（未接线）。
	// 9. 每次成功的状态变更后失效路由读缓存（gate.invalidateRouteCache），否则广播还会打到旧节点
	//    最多一个 RoomRouteCacheTTLSeconds 周期。
	// 10. 审计：状态推进的每一步写一行 live_gw_broadcast_log（message_id 带推进后的 version，
	//    天然不重复），路由行本身同时带 drain_reason/updated_by/trace_id（AGENTS.md §8 处置留痕）。
	//    审计写入失败只记告警不推翻业务结论：留痕已经在 live_gw_room_route 行里，
	//    把已生效的排空伪装成失败会诱导运营去硬切节点。
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	c, err := g.requireDispositionWrite("DrainRoomRoute")
	if err != nil {
		return nil, err
	}
	if err := g.requireRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	roomID := in.GetRoomId()
	nodeID, err := g.requireNodeID(in.GetNodeId())
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(in.GetReason())
	if err := g.requireReason(reason); err != nil {
		return nil, err
	}
	if len(reason) > drainReasonMaxBytes {
		return nil, fmt.Errorf("%w: reason too long (max %d bytes for live_gw_room_route.drain_reason)",
			model.ErrEmptyReason, drainReasonMaxBytes)
	}
	operator := strings.TrimSpace(in.GetOperator())
	if err := g.requireOperatorField(operator); err != nil {
		return nil, err
	}
	if in.GetExpectedVersion() <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required to drain a route", model.ErrVersionConflict)
	}
	targetNode := strings.TrimSpace(in.GetTargetNodeId())
	if targetNode != "" {
		if targetNode, err = g.requireNodeID(targetNode); err != nil {
			return nil, err
		}
		if targetNode == nodeID {
			return nil, fmt.Errorf("%w: target_node_id equals the node being drained (%s)",
				model.ErrInvalidTransition, safeIdent(nodeID))
		}
	}
	traceID := strings.TrimSpace(in.GetTraceId())

	// --- 幂等窗口 ---
	replay, _, err := g.idempotentReplay(l.ctx, drainRPC, in.GetRequestId())
	if err != nil {
		return nil, err
	}
	store, err := g.svcCtx.RequireStore()
	if err != nil {
		return nil, err
	}
	if replay {
		// 重放：只回读当前真值，零写入、零审计。
		cur, ferr := store.RoomRoutes.FindOne(l.ctx, roomID)
		if ferr != nil {
			return nil, ferr
		}
		if cur == nil {
			return nil, fmt.Errorf("%w: room_id=%d", model.ErrRouteNotFound, roomID)
		}
		g.infof("live-gateway: DrainRoomRoute 按 request_id 重放，回显当前路由不重复推进 room_id=%d node=%s version=%d",
			roomID, safeIdent(nodeID), cur.Version)
		return lgwRouteInfoOf(l.ctx, g, store, cur)
	}

	row, err := store.RoomRoutes.FindOne(l.ctx, roomID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, fmt.Errorf("%w: room_id=%d", model.ErrRouteNotFound, roomID)
	}
	if row.PrimaryNode != nodeID {
		lgwAuditRouteDrain(l.ctx, g, roomID, fmt.Sprintf("drain-denied:%d:%d", roomID, model.NowUnix()),
			c, model.KindModeration, model.BroadcastLogDenied, model.DropPermissionDenied, traceID)
		g.errorf("live-gateway: 排空请求指向非本房间主节点 room_id=%d 请求节点=%s 实际主节点=%s（%s）",
			roomID, safeIdent(nodeID), safeIdent(row.PrimaryNode), c.String())
		return nil, fmt.Errorf("%w: node %s is not the primary node of room %d (primary=%s)",
			model.ErrPermissionDenied, safeIdent(nodeID), roomID, safeIdent(row.PrimaryNode))
	}
	if row.State == model.RouteStateDraining {
		g.infof("live-gateway: 房间 %d 路由已处于 DRAINING（node=%s version=%d），排空按幂等成功返回",
			roomID, safeIdent(nodeID), row.Version)
		return lgwRouteInfoOf(l.ctx, g, store, row)
	}
	if !model.IsValidRouteTransition(row.State, model.RouteStateDraining) {
		return nil, fmt.Errorf("%w: room route %d state=%d cannot go to DRAINING",
			model.ErrInvalidTransition, roomID, row.State)
	}

	patch := model.RoutePatch{DrainReason: &reason, Operator: &operator, TraceID: &traceID}
	aff, err := store.RoomRoutes.UpdateState(l.ctx, roomID, nodeID,
		[]int32{model.RouteStateServing}, in.GetExpectedVersion(), model.RouteStateDraining, patch)
	if err != nil {
		return nil, err
	}
	if aff == 0 {
		// 0 行有六种解释，逐一回读定位；其中「并发下已被排空」是幂等成功（info 非 nil、err 为 nil）。
		info, cerr := lgwDrainConflict(l.ctx, g, store, roomID, nodeID, in.GetExpectedVersion())
		if cerr != nil {
			return nil, cerr
		}
		g.rememberIdempotent(l.ctx, drainRPC, in.GetRequestId(), nodeID)
		return info, nil
	}
	g.invalidateRouteCache(l.ctx, roomID)
	drained, err := store.RoomRoutes.FindOne(l.ctx, roomID)
	if err != nil {
		return nil, err
	}
	if drained == nil {
		return nil, fmt.Errorf("%w: room_id=%d disappeared after drain", model.ErrRouteNotFound, roomID)
	}
	lgwAuditRouteDrain(l.ctx, g, roomID, fmt.Sprintf("drain:%d:%d", roomID, drained.Version),
		c, model.KindSystem, model.BroadcastLogSent, model.DropOK, traceID)
	if targetNode == "" {
		nodes, draining := fanoutTargets(drained.PrimaryNode, lgwReplicaLog(g, drained), drained.State)
		g.infof("live-gateway: 房间 %d 节点 %s 已排空 version=%d reason=%s operator=%s，扇出节点=%v 排空中=%v",
			roomID, safeIdent(nodeID), drained.Version, safeIdent(reason), safeIdent(operator), nodes, draining)
		g.rememberIdempotent(l.ctx, drainRPC, in.GetRequestId(), drained.PrimaryNode)
		return lgwRouteInfoOf(l.ctx, g, store, drained)
	}

	// --- 迁移：DRAINING@A → SERVING@B（一条原子条件 UPDATE 写 primary_node + state） ---
	replicas := lgwReplicaLog(g, drained)
	kept := lgwWithoutNode(append(replicas, targetNode), nodeID)
	migrate := model.RoutePatch{PrimaryNode: &targetNode, ReplicaNodes: &kept, DrainReason: &reason,
		Operator: &operator, TraceID: &traceID}
	aff2, err := store.RoomRoutes.UpdateState(l.ctx, roomID, "",
		[]int32{model.RouteStateDraining}, drained.Version, model.RouteStateServing, migrate)
	if err != nil {
		return nil, err
	}
	if aff2 == 0 {
		// B 从未被写进路由行，所以没有「指向空节点」的脏状态需要清理；行停在 DRAINING@A（只不出进）。
		g.errorf("live-gateway: 房间 %d 迁移到 %s 未命中条件更新，路由停在 DRAINING@%s，请带新 version 重试",
			roomID, safeIdent(targetNode), safeIdent(nodeID))
		return nil, fmt.Errorf("%w: room %d drained but migration to %s needs a fresh expected_version",
			model.ErrVersionConflict, roomID, safeIdent(targetNode))
	}
	g.invalidateRouteCache(l.ctx, roomID)
	migrated, err := store.RoomRoutes.FindOne(l.ctx, roomID)
	if err != nil {
		return nil, err
	}
	if migrated == nil {
		return nil, fmt.Errorf("%w: room_id=%d disappeared after migration", model.ErrRouteNotFound, roomID)
	}
	lgwAuditRouteDrain(l.ctx, g, roomID, fmt.Sprintf("migrate:%d:%d", roomID, migrated.Version),
		c, model.KindSystem, model.BroadcastLogSent, model.DropOK, traceID)
	g.infof("live-gateway: 房间 %d 路由已从 %s 迁移到 %s version=%d reason=%s operator=%s",
		roomID, safeIdent(nodeID), safeIdent(targetNode), migrated.Version, safeIdent(reason), safeIdent(operator))
	g.rememberIdempotent(l.ctx, drainRPC, in.GetRequestId(), migrated.PrimaryNode)
	return lgwRouteInfoOf(l.ctx, g, store, migrated)
}

// lgwDrainConflict 条件更新 0 行时回读，把「没打中」翻译成唯一那个真实原因，绝不笼统报「失败」。
// 返回 (info, nil) 表示并发下别人先排空了同一节点：按幂等成功回显当前行（不报错、不重复审计、
// 不再推进 version），调用点直接把 info 回给客户端。
func lgwDrainConflict(ctx context.Context, g gate, store *repository.Store, roomID int64, nodeID string,
	expectedVersion int64) (*rpc.RoomRouteInfo, error) {
	cur, ferr := store.RoomRoutes.FindOne(ctx, roomID)
	if ferr != nil {
		return nil, ferr
	}
	if cur == nil {
		return nil, fmt.Errorf("%w: room_id=%d", model.ErrRouteNotFound, roomID)
	}
	if cur.PrimaryNode != nodeID {
		return nil, fmt.Errorf("%w: node %s is not the primary node of room %d (primary=%s)",
			model.ErrPermissionDenied, safeIdent(nodeID), roomID, safeIdent(cur.PrimaryNode))
	}
	if cur.State == model.RouteStateDraining {
		g.infof("live-gateway: 房间 %d 已被并发排空（version=%d），本次按幂等成功回显，不重复推进版本",
			roomID, cur.Version)
		return lgwRouteInfoOf(ctx, g, store, cur)
	}
	if cur.Version != expectedVersion {
		return nil, fmt.Errorf("%w: room %d version=%d, expected=%d", model.ErrVersionConflict, roomID,
			cur.Version, expectedVersion)
	}
	return nil, fmt.Errorf("%w: room route %d state=%d cannot go to DRAINING",
		model.ErrInvalidTransition, roomID, cur.State)
}

// lgwRouteInfoOf 投影最新行（含 Redis 连接数读数），是 DrainRoomRoute 唯一的出口投影。
func lgwRouteInfoOf(ctx context.Context, g gate, store *repository.Store,
	row *model.LiveGwRoomRoute) (*rpc.RoomRouteInfo, error) {
	replicas := lgwReplicaLog(g, row)
	return routeInfo(row, replicas, lgwServingConnections(ctx, g, row.RoomId, nil)), nil
}

// lgwReplicaLog 解 replica_nodes；脏数据记告警并按空处理（与 routeForFanout 同口径：
// 主节点仍有效，不能因为副本列脏就让排空接口报错）。
func lgwReplicaLog(g gate, row *model.LiveGwRoomRoute) []string {
	if row == nil {
		return nil
	}
	nodes, err := row.ReplicaNodesList()
	if err != nil {
		g.errorf("live-gateway: live_gw_room_route replica_nodes 解析失败 room_id=%d，按无副本处理: %v", row.RoomId, err)
		return nil
	}
	return nodes
}

// lgwWithoutNode 从节点列表里去掉一个节点（保持原顺序，去空白与重复）。
func lgwWithoutNode(nodes []string, dropNode string) []string {
	out := make([]string, 0, len(nodes))
	seen := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		n = strings.TrimSpace(n)
		if n == "" || n == dropNode {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

// lgwAuditRouteDrain 把一次排空/迁移（或被拒的排空尝试）写进 live_gw_broadcast_log。
// 写失败只记告警：路由行自身的 drain_reason/updated_by/trace_id 已经是留痕，
// 不能因为审计表不可用就把已生效的排空伪装成失败（那会诱导运营去硬切节点）。
func lgwAuditRouteDrain(ctx context.Context, g gate, roomID int64, messageID string, c caller,
	kind, state, drop int32, traceID string) {
	err := g.writeAudit(ctx, auditParams{
		RoomID:     roomID,
		Kind:       kind,
		MessageID:  messageID,
		SenderMid:  c.mid,
		SenderRole: c.role,
		State:      state,
		Drop:       drop,
		Source:     "live-gateway/route",
		TraceID:    traceID,
	})
	if err != nil {
		g.errorf("live-gateway: 路由处置审计写入失败 room_id=%d message_id=%s: %v", roomID, safeIdent(messageID), err)
	}
}
