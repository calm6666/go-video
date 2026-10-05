package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/model"
)

// lgwOutcomePending 去重键「已抢键、结论未回填」的占位串。
//
// 它必须与 repository/leasestore.go 的 outcomePending 同值：那边抢键后先写占位，
// 本服务把结论回填进去，重放请求读到的就是首次结论。占位串不匹配会把「重放」误判成
// 「首次请求」，从而对同一 message_id 扇出两次。
const lgwOutcomePending = "pending"

// broadcastIntent 四条下发入口（房间广播 / 弹幕转发 / 系统事件 / 单播）共用的判定入参。
//
// 四个方法各自从 proto 请求渲染这份意图，之后走同一条门禁链——下发是本服务最核心的路径，
// 两条路径各写一遍鉴权与限流迟早漂移（这是本服务最怕的 bug 类别，README 权限矩阵同口径）。
type broadcastIntent struct {
	RoomID       int64
	Kind         int32
	MessageID    string
	EventID      string
	SenderMid    int64
	ClaimedRole  int32
	LeaseID      string
	Ticket       string
	Payload      []byte
	Topics       []string
	Roles        []string
	ExpireAt     int64
	Priority     int32
	Reliable     bool
	Source       string
	TraceID      string
	DedupEvent   bool     // 系统事件按 event_id 去重（而非 message_id）
	UserScoped   bool     // 弹幕/单播：额外按 USER 层 DanmakuQps 限流
	LeaseIDs     []string // 单播的精确投递目标（空=节点侧按订阅集合匹配）
	AuditMessage string   // 审计行的 message_id（单播/事件可自带定位串）
}

// broadcastOutcome 下发判定结论。Err 非空表示「必须让上游看见」的失败
// （require_reliable 的下发失败、依赖故障），此时 Accepted/Drop 都不该被当真。
type broadcastOutcome struct {
	Accepted   bool
	Duplicated bool
	Drop       int32
	Detail     string
	Fanout     int32
	Targeted   int32
	EnqueuedAt int64
	Remaining  int32
	Err        error
}

// lgwDrop 组装一个「可解释的丢弃」结论。
func lgwDrop(drop int32, detail string) broadcastOutcome {
	return broadcastOutcome{Accepted: false, Drop: drop, Detail: detail}
}

// runBroadcast 本服务唯一的下发判定链，顺序即契约（原桩注释的 1→9 步）：
//
//	参数（调用方已校） → 载荷上限 → 时效 → 发送者鉴权 → 房间可广播 → 路由
//	→ 去重 → 限流 → 扇出 → 审计
//
// 两处刻意的位置选择：
//   - 鉴权在去重之前：越权尝试即使带着一个重复的 message_id 也不会走到扇出，且首次出现的
//     message_id 会留下 DENIED 审计，否则攻击者只要复用一条旧 message_id 就能把越权探测变成无痕操作。
//     注意该承诺的边界：审计唯一键就是 (room_id, message_id)，同一 message_id 已有受理行时
//     DENIED 行会被 model.Insert 当成重复投递吞掉（README 已知缺口 13）。
//   - 去重在限流之前：重放不该消耗配额，否则上游重试风暴会把正常用户的额度吃光。
func (g gate) runBroadcast(ctx context.Context, in broadcastIntent) broadcastOutcome {
	// --- 2. 载荷上限（生效配额夹到进程上限）---
	maxPayload := g.maxPayloadForRoom(ctx, in.RoomID)
	if maxPayload > 0 && int32(len(in.Payload)) > maxPayload {
		out := lgwDrop(model.DropPayloadTooLarge,
			fmt.Sprintf("payload %d bytes exceeds max_payload_bytes=%d", len(in.Payload), maxPayload))
		g.auditDrop(ctx, in, model.BroadcastLogDropped, out)
		return out
	}

	// --- 3. 时效：过期消息不再下发，也不占配额 ---
	if in.ExpireAt > 0 && model.Expired(in.ExpireAt, model.NowUnix()) {
		// 契约缺口：rpc.DropReason 里没有「已过期」这一档（本轮禁止改 proto），
		// 这里选 NO_SUBSCRIBER 而不是 RATE_LIMITED——两者都是「可丢弃的正常结果」，
		// 但把过期说成「没人订阅」比说成「被限流」更接近调用方该有的动作（别再重试）。
		out := lgwDrop(model.DropNoSubscriber,
			fmt.Sprintf("message expired at %d (now=%d)", in.ExpireAt, model.NowUnix()))
		g.auditDrop(ctx, in, model.BroadcastLogDropped, out)
		return out
	}

	// --- 4. 发送者鉴权（NormalizeRole → 可信类别 → 权限矩阵 → 三元组凭据）---
	v, err := g.resolveSender(ctx, in.RoomID, in.SenderMid, in.ClaimedRole, in.Kind, in.LeaseID, in.Ticket)
	if err != nil {
		return broadcastOutcome{Err: err}
	}
	if v.Drop != 0 {
		out := lgwDrop(v.Drop, v.Detail)
		// 越权必须落 DENIED：「拒了但查不到」在追责时与「没拒」等价（AGENTS.md §8）。
		g.auditDrop(ctx, in, model.BroadcastLogDenied, out)
		g.errorf("live-gateway: 下发被鉴权拒出 room_id=%d kind=%d mid=%d claimed_role=%d drop=%d: %s",
			in.RoomID, in.Kind, in.SenderMid, in.ClaimedRole, v.Drop, v.Detail)
		return out
	}
	senderMid, senderRole := v.Mid, v.Role

	// --- 房间可广播性（live-room 是房间状态的真值所有者，本服务不复判）---
	if !g.svcCtx.Rooms.Available() {
		// 未接线：不能默认「房间能发」（README 已知缺口）。
		return broadcastOutcome{Err: fmt.Errorf("%w: cannot confirm room_id=%d is broadcastable",
			repository.ErrLiveRoomNotConfigured, in.RoomID)}
	}
	ok, why, berr := g.svcCtx.Rooms.Broadcastable(ctx, in.RoomID)
	if berr != nil {
		return broadcastOutcome{Err: berr}
	}
	if !ok {
		out := lgwDrop(model.DropRoomClosed, "room is not broadcastable: "+why)
		g.auditDrop(ctx, in, model.BroadcastLogDropped, out)
		return out
	}

	// --- 5. 路由 ---
	route, replicas, err := g.routeForFanout(ctx, in.RoomID)
	if err != nil {
		return broadcastOutcome{Err: err}
	}
	if route == nil || (route.State != model.RouteStateServing && route.State != model.RouteStateDraining) {
		out := lgwDrop(model.DropNoRoute, "no serving route for room")
		g.auditDrop(ctx, in, model.BroadcastLogDropped, out)
		return out
	}
	nodes, draining := fanoutTargets(route.PrimaryNode, replicas, route.State)
	if len(nodes) == 0 {
		out := lgwDrop(model.DropNoRoute, "route has no deliverable node (all draining)")
		g.auditDrop(ctx, in, model.BroadcastLogDropped, out)
		return out
	}

	// --- 6. 幂等/去重：广播键 (room_id, message_id)，系统事件键 (room_id, event_id) ---
	dedupID, claimFn, rememberFn, findFn := in.MessageID, g.claimMessage, g.rememberMessage, g.findMessageRow
	if in.DedupEvent {
		dedupID, claimFn, rememberFn, findFn = in.EventID, g.claimEvent, g.rememberEvent, g.findEventRow
	}
	if out, done := g.dedupOnce(ctx, in.RoomID, dedupID, claimFn, findFn); done {
		return out
	}

	// --- 7. 限流（priority=1 豁免限流，但上面的鉴权一步都没豁免）---
	if in.Priority != 1 {
		if out, limited := g.enforceBroadcastRate(ctx, in, senderMid); limited {
			g.auditDrop(ctx, in, model.BroadcastLogDropped, out)
			// 结论必须回填：去重键已在本步之前占位，不回填就让同一条 message_id 在
			// DedupWindowSeconds 里一直是「并发中，请重试」——客户端照提示重试，
			// 拿到的永远是并发提示而不是「这条被限流丢了」，一条弹幕被白吃掉十分钟。
			g.rememberOutcome(ctx, in, rememberFn, out)
			return out
		}
	}

	// --- 8. 扇出 ---
	req := &repository.FanoutRequest{
		RoomID: in.RoomID,
		// 与审计定位串同一口径：系统事件入口没有 message_id 字段，直接传 in.MessageID 会让
		// 节点侧收到一条空 message_id 的消息——既无法按消息幂等去重，排障日志也认不出是哪条。
		MessageID: in.auditMessageID(),
		Kind:      in.Kind,
		Nodes:     nodes,
		LeaseIDs:  in.LeaseIDs,
		Topics:    in.Topics,
		Roles:     in.Roles,
		Payload:   in.Payload,
		ExpireAt:  in.ExpireAt,
		TraceID:   in.TraceID,
	}
	var (
		res  *repository.FanoutResult
		drop int32
	)
	if len(in.LeaseIDs) > 0 {
		res, drop, err = g.deliver(ctx, req, in.LeaseIDs, in.Reliable)
	} else {
		res, drop, err = g.fanout(ctx, req, in.Reliable)
	}
	if err != nil {
		// require_reliable=true 的下发失败：审计也要留下这一行，然后让上游看见错误。
		fail := lgwDrop(model.DropTransportUnavailable, err.Error())
		g.auditDrop(ctx, in, model.BroadcastLogDropped, fail)
		g.rememberOutcome(ctx, in, rememberFn, fail)
		return broadcastOutcome{Err: err, Drop: model.DropTransportUnavailable, Detail: err.Error()}
	}
	if drop != 0 {
		out := lgwDrop(drop, "delivery channel unavailable, message dropped (require_reliable=false)")
		out.Fanout = int32(len(nodes))
		g.auditDrop(ctx, in, model.BroadcastLogDropped, out)
		g.rememberOutcome(ctx, in, rememberFn, out)
		return out
	}

	// --- 命中连接数为 0：这是正常可丢弃结果（房间没人），不是错误（原桩第 10 步）---
	targeted := res.TargetedConnections
	if targeted == 0 && len(in.LeaseIDs) > 0 {
		out := lgwDrop(model.DropNoSubscriber, "no live connection for the target subject")
		out.Fanout = res.FanoutNodes
		g.auditDrop(ctx, in, model.BroadcastLogDropped, out)
		g.rememberOutcome(ctx, in, rememberFn, out)
		return out
	}

	// --- 9. 受理成功：审计只存摘要与字节数，正文永不入库 ---
	accepted := broadcastOutcome{
		Accepted:   true,
		Drop:       model.DropOK,
		Fanout:     res.FanoutNodes,
		Targeted:   targeted,
		EnqueuedAt: model.NowUnix(),
	}
	if len(draining) > 0 {
		// 排空节点没被打到是正确行为，但要在结论里说清楚，否则运营以为「没排干净」。
		accepted.Detail = fmt.Sprintf("%d draining node(s) excluded from fanout", len(draining))
	}
	audit := in
	audit.SenderMid = senderMid
	if aerr := g.writeAudit(ctx, auditParams{
		RoomID: audit.RoomID, Kind: audit.Kind, MessageID: audit.auditMessageID(),
		EventID: audit.EventID, SenderMid: senderMid, SenderRole: senderRole,
		Payload: audit.Payload, Fanout: accepted.Fanout, Targeted: accepted.Targeted,
		State: model.BroadcastLogSent, Drop: model.DropOK,
		Source: audit.Source, TraceID: audit.TraceID,
	}); aerr != nil {
		// 消息确实已经下发，回滚不回来；此处只能把「查不到」这件事本身报出去，
		// 让上游知道这条投递没有审计凭证（比静默成功诚实）。
		g.errorf("live-gateway: 下发成功但审计落库失败 room_id=%d message_id=%s kind=%d: %v",
			in.RoomID, safeIdent(in.auditMessageID()), in.Kind, aerr)
		accepted.Detail = strings.TrimSpace(accepted.Detail + " audit-write-failed")
	}
	g.rememberOutcome(ctx, in, rememberFn, accepted)
	return accepted
}

// auditMessageID 审计行用哪个串做幂等键：系统事件允许自带定位串，其余就是 message_id。
func (in broadcastIntent) auditMessageID() string {
	if in.AuditMessage != "" {
		return in.AuditMessage
	}
	return in.MessageID
}

// dedupOnce 去重结论：命中已回填的结论就回放，占位（并发中）与未命中都按首次处理。
// find 是同一去重维度的账本回读口径（Redis 抖动时才用到）。
func (g gate) dedupOnce(ctx context.Context, roomID int64, id string,
	claim func(context.Context, int64, string, int) (string, bool, error),
	find func(context.Context, int64, string) (*model.LiveGwBroadcastLog, error)) (broadcastOutcome, bool) {
	if id == "" {
		return broadcastOutcome{}, false
	}
	outcome, fresh, err := claim(ctx, roomID, id, g.dedupWindow())
	if err != nil {
		// Redis 去重窗口不可用**不**当作「首次请求」放行：那会让重试各扇出一次。
		// 降级顺序是「问 DB 唯一键兜底」，DB 也问不到才返回错误。
		g.errorf("live-gateway: 去重窗口不可用，回退 DB 唯一键判定 room_id=%d id=%s: %v", roomID, safeIdent(id), err)
		if replay, ok := g.dedupFromDB(ctx, roomID, id, find); ok {
			return replay, true
		}
		return broadcastOutcome{Err: err}, true
	}
	if fresh {
		return broadcastOutcome{}, false
	}
	if outcome == "" || outcome == lgwOutcomePending {
		// 并发对手刚抢完键还没回填结论：这一次不猜结论，让调用方按同一幂等键重试。
		return lgwDrop(model.DropDuplicated, "concurrent request with the same id is still in flight, retry"), true
	}
	out, perr := parseBroadcastOutcome(outcome)
	if perr != nil {
		return broadcastOutcome{Err: fmt.Errorf("live-gateway: 去重窗口里的结论无法解析 id=%s: %w", safeIdent(id), perr)}, true
	}
	out.Duplicated = true
	return out, true
}

// dedupFromDB 用 live_gw_broadcast_log 的唯一键兜底判定是否已经受理过。
// find 由调用方按去重维度给出（消息查 message_id、事件查 event_id），这里不猜列。
func (g gate) dedupFromDB(ctx context.Context, roomID int64, id string,
	find func(context.Context, int64, string) (*model.LiveGwBroadcastLog, error)) (broadcastOutcome, bool) {
	row, qerr := find(ctx, roomID, id)
	if qerr != nil || row == nil {
		return broadcastOutcome{}, false
	}
	// 表里有行 = 这一条已经被处理过；具体是 SENT 还是 DROPPED 直接照行里的结论回放，
	// 不猜「上次大概成功了」。
	return broadcastOutcome{
		Accepted:   row.State == model.BroadcastLogSent,
		Duplicated: true,
		Drop:       row.DropReason,
		Detail:     "replayed from the broadcast ledger (dedup window unavailable)",
		Fanout:     row.FanoutNodes,
		Targeted:   row.TargetedConnections,
		EnqueuedAt: row.Ctime,
	}, true
}

// enforceBroadcastRate 房间层 BroadcastQps + 用户态维度 DanmakuQps（USER 链更严格的一侧）。
// 任一侧超限就出 RATE_LIMITED，并把还剩多少带回去让客户端退避。
func (g gate) enforceBroadcastRate(ctx context.Context, in broadcastIntent, senderMid int64) (broadcastOutcome, bool) {
	room, err := g.resolveQuota(ctx, model.QuotaScopeRoom, in.RoomID)
	if err != nil {
		return broadcastOutcome{Err: err}, true
	}
	if allowed, remaining, rerr := g.rateLimit(ctx,
		fmt.Sprintf("bqps:room:%d", in.RoomID), room.BroadcastQps); rerr != nil {
		return broadcastOutcome{Err: rerr}, true
	} else if !allowed {
		return broadcastOutcome{Drop: model.DropRateLimited, Remaining: remaining,
			Detail: fmt.Sprintf("room broadcast_qps %d reached", room.BroadcastQps)}, true
	}
	if !in.UserScoped || senderMid <= 0 {
		return broadcastOutcome{}, false
	}
	// USER 链（USER→GLOBAL）：个体降配不能被房间大配额淹掉（model.QuotaScopeChain 口径）。
	user, err := g.resolveQuota(ctx, model.QuotaScopeUser, senderMid)
	if err != nil {
		return broadcastOutcome{Err: err}, true
	}
	limit := user.DanmakuQps
	if limit <= 0 || room.DanmakuQps > 0 && room.DanmakuQps < limit {
		limit = room.DanmakuQps // 房间层的弹幕总量约束更紧时以房间为准
	}
	if allowed, remaining, rerr := g.rateLimit(ctx,
		fmt.Sprintf("dqps:mid:%d", senderMid), limit); rerr != nil {
		return broadcastOutcome{Err: rerr}, true
	} else if !allowed {
		return broadcastOutcome{Drop: model.DropRateLimited, Remaining: remaining,
			Detail: fmt.Sprintf("sender danmaku_qps %d reached", limit)}, true
	}
	return broadcastOutcome{}, false
}

// maxPayloadForRoom 载荷上限：生效配额（含继承链）与进程上限**取较小值**，解析不到时退回进程上限。
// 这里允许降级是因为「上限」只影响拒发还是放行一条消息，不涉及权限判定；
// 与 resolveQuota 在鉴权路径上「问不到就报错」的方向不同，是刻意的。
//
// 夹取而不是直接采用配额值：配额是「按房间往下调」的粒度，不是抬护栏的口子。
// 任何一行 ROOM 配额都能把上限设成任意大（UpsertAccessQuota 的 CheckQuotaBounds 在进程配置
// MaxPayloadBytes=0 时连上界都不校），不夹取就等于让一条运营配置取消掉载荷上限。
func (g gate) maxPayloadForRoom(ctx context.Context, roomID int64) int32 {
	limit := g.maxPayloadBytes()
	if eff, err := g.resolveQuota(ctx, model.QuotaScopeRoom, roomID); err == nil && eff.MaxPayloadBytes > 0 {
		if eff.MaxPayloadBytes < limit {
			return eff.MaxPayloadBytes
		}
		return limit
	} else if err != nil {
		g.errorf("live-gateway: 配额解析失败，载荷上限退回进程配置 room_id=%d: %v", roomID, err)
	}
	return limit
}

// auditDrop 落一条非 SENT 的审计行。drop=0 时补 DUPLICATED 以外的最接近原因，
// 因为 model.Insert 拒绝「非 SENT 但没有原因」的行（无因丢弃无法排障）。
func (g gate) auditDrop(ctx context.Context, in broadcastIntent, state int32, out broadcastOutcome) {
	drop := out.Drop
	if drop == 0 || !model.ValidDropReason(drop) {
		drop = model.DropUnspecified
	}
	audit := in
	if aerr := g.writeAudit(ctx, auditParams{
		RoomID: audit.RoomID, Kind: audit.Kind, MessageID: audit.auditMessageID(),
		EventID: audit.EventID, SenderMid: audit.SenderMid, SenderRole: audit.ClaimedRole,
		Payload: audit.Payload, Fanout: out.Fanout, Targeted: out.Targeted,
		State: state, Drop: drop, Source: audit.Source, TraceID: audit.TraceID,
	}); aerr != nil {
		g.errorf("live-gateway: 丢弃审计落库失败 room_id=%d message_id=%s state=%d drop=%d: %v",
			in.RoomID, safeIdent(in.auditMessageID()), state, drop, aerr)
	}
}

func (g gate) claimMessage(ctx context.Context, roomID int64, id string, ttl int) (string, bool, error) {
	return g.svcCtx.Leases.ClaimMessage(ctx, roomID, id, ttl)
}

func (g gate) rememberMessage(ctx context.Context, roomID int64, id, outcome string, ttl int) error {
	return g.svcCtx.Leases.RememberMessage(ctx, roomID, id, outcome, ttl)
}

func (g gate) claimEvent(ctx context.Context, roomID int64, id string, ttl int) (string, bool, error) {
	return g.svcCtx.Leases.ClaimEvent(ctx, roomID, id, ttl)
}

func (g gate) rememberEvent(ctx context.Context, roomID int64, id, outcome string, ttl int) error {
	return g.svcCtx.Leases.RememberEvent(ctx, roomID, id, outcome, ttl)
}

// findMessageRow / findEventRow 是去重 DB 兜底的两种回读口径，与 claimEvent/claimMessage 一一对应。
// 不能合并成一个：系统事件的审计行 message_id 是渲染出来的 "evt:"+event_id，
// 拿 event_id 去查 message_id 列永远落空，兜底就等于没有。
// 账本问不到（含 store 未接线）时按「未命中」返回，由调用方沿用原有降级判断。
func (g gate) findMessageRow(ctx context.Context, roomID int64, id string) (*model.LiveGwBroadcastLog, error) {
	store, err := g.svcCtx.RequireStore()
	if err != nil {
		return nil, nil
	}
	return store.BroadcastLogs.FindByMessage(ctx, roomID, id)
}

func (g gate) findEventRow(ctx context.Context, roomID int64, id string) (*model.LiveGwBroadcastLog, error) {
	store, err := g.svcCtx.RequireStore()
	if err != nil {
		return nil, nil
	}
	return store.BroadcastLogs.FindByEvent(ctx, roomID, id)
}

// rememberOutcome 回填幂等结论。失败只告警：下发本身已经完成，
// 让调用方重试整条写路径会造成二次扇出，比「重放时少一次结论」糟糕得多。
//
// 反向的缺口也要说清：抢到键之后、结论回填之前出硬错（限流依赖故障、配额解析失败）时，
// 这里没有「释放去重键」的算子，那条 message_id 会在 DedupWindowSeconds 窗口内被判
// 「并发中，请重试」——窗口一到自动恢复，比「让 logic 自己删一个不认识格式的 Redis 键」安全。
func (g gate) rememberOutcome(ctx context.Context, in broadcastIntent,
	remember func(context.Context, int64, string, string, int) error, out broadcastOutcome) {
	id := in.MessageID
	if in.DedupEvent {
		id = in.EventID
	}
	if id == "" {
		return
	}
	if err := remember(ctx, in.RoomID, id, encodeBroadcastOutcome(out), g.dedupWindow()); err != nil {
		g.errorf("live-gateway: 去重结论回填失败 room_id=%d id=%s: %v", in.RoomID, safeIdent(id), err)
	}
}

// 结论编码：<accepted 0/1>|<drop>|<fanout>|<targeted>
//
// 只有四个数就够回放：重放要复述的是「上次受理了吗、丢在哪、打到多少人」，
// detail 是人读的文本，进不了 Redis 值也不该进（超长串会污染去重键）。
func encodeBroadcastOutcome(out broadcastOutcome) string {
	a := 0
	if out.Accepted {
		a = 1
	}
	return fmt.Sprintf("%d|%d|%d|%d", a, out.Drop, out.Fanout, out.Targeted)
}

func parseBroadcastOutcome(v string) (broadcastOutcome, error) {
	var a, drop, fanout, targeted int
	if _, err := fmt.Sscanf(v, "%d|%d|%d|%d", &a, &drop, &fanout, &targeted); err != nil {
		return broadcastOutcome{}, err
	}
	if a != 0 && a != 1 {
		return broadcastOutcome{}, fmt.Errorf("malformed accepted flag %q", v)
	}
	return broadcastOutcome{
		Accepted: a == 1, Drop: int32(drop), Fanout: int32(fanout), Targeted: int32(targeted),
		Detail: "replayed from the dedup window",
	}, nil
}

// lgwTrimStrings 去掉空白项并保序去重（角色/子通道过滤器用）。
func lgwTrimStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// lgwIsTransportErr 判定错误是否是「通道未接线」——四个入口的 reliable 分支都按这一条分叉。
func lgwIsTransportErr(err error) bool {
	return errors.Is(err, model.ErrTransportUnavailable)
}
