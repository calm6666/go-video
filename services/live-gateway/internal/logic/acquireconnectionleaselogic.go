package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// acquireRPCName request_id 幂等窗口里本方法的键名（与 proto rpc 名一致）。
const acquireRPCName = "AcquireConnectionLease"

// ticketReasonNormal 随租约下发票据的 issue_reason（proto ReconnectTicketInfo 注释里的三个取值之一）。
const ticketReasonNormal = "normal"

// leaseIDPrefix 租约 ID 前缀：newID("lgwl") 产出 "lgwl_<ULID>"。
// 幂等回放用它辨别「结论已回填」还是「只有占位值」，因为 repository 的占位常量未导出。
const leaseIDPrefix = "lgwl_"

// maxDeviceIDHashBytes device_id_hash 的形态门禁上限：
// proto 约定它是 sha256 hex 前 32 位，允许 32/64 两种摘要长度；超过即按「误传设备号明文」拒绝。
const maxDeviceIDHashBytes = 64

type AcquireConnectionLeaseLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAcquireConnectionLeaseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AcquireConnectionLeaseLogic {
	return &AcquireConnectionLeaseLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 申请连接租约：校验 (mid, room_id, role) 与配额，签发 lease_id + 重连票据
func (l *AcquireConnectionLeaseLogic) AcquireConnectionLease(in *rpc.AcquireConnectionLeaseReq) (*rpc.AcquireConnectionLeaseReply, error) {
	// 实现要点（门禁顺序是契约的一部分：鉴权先于配额，配额先于写存储）：
	// 1. 参数：room_id>0、mid>=0（0 是合法游客）、conn_id/node_id 非空（(node_id, conn_id) 是租约
	//    幂等键的一半）、request_id 非空；client.device_id_hash 必须是摘要形状，超长/非十六进制
	//    按「误传设备号明文」拒绝（ErrInvalidClientInfo），本服务永不接受 IP/设备号原值。
	// 2. 幂等：request_id 命中即回放**同一条租约**（allowed=true + 回显剩余 TTL），不再判角色、
	//    不再消耗配额。票据原文只在首次签发那一次回显，重放不回显（一次性凭据不外溢第二次）。
	//    repository 没有 FindByRequestID(node,conn,request) 这个访问器（缺口见交付报告），
	//    这里用 LeaseStore.ClaimRequest/RememberRequest 存 lease_id 达成同一语义。
	// 3. 角色判定（本服务是唯一可信裁决点）：claimed_role 经 NormalizeRole 收敛，未识别一律 VIEWER；
	//    ANCHOR/ROOM_ADMIN 都要问 live-room（房管身份无法校验时同样降级），问不到或问出「不是」
	//    都降级 VIEWER 并在 deny_detail 说明——绝不把「问不到」当成「判定通过」；
	//    OPERATOR/SERVICE 只接受 metadata 归因的内部主体，客户端链路自报这两个角色一律 ErrPermissionDenied。
	// 4. 配额：g.resolveQuota(QuotaScopeRoom, room_id)（继承链 + QuotaCacheTTLSeconds 读缓存）；
	//    mid=0 且 !AllowGuest → allowed=false + PERMISSION_DENIED（游客策略是配置不是代码）；
	//    Redis 房间有效连接数 >= MaxConnections → allowed=false + RATE_LIMITED，
	//    quota_room_conns 回读实际值；计数扫描量按 min(MaxConnections, ConnectionScanLimit) 走，
	//    房间规模已超过扫描上限时判定不可证，选择「放行 + error 级告警」而不是把大房间封死
	//    （repository 未暴露 SCARD 之类的 O(1) 集合计数，缺口见交付报告）。
	// 5. 禁止重连窗口：BanUntil(room_id, mid) > now → allowed=false + PERMISSION_DENIED
	//    并在 deny_detail 给出解禁时刻（Kick 写的封禁只有 Redis 一个真值源）。
	// 6. TTL：clampLeaseTTL(请求值 → 配额 LeaseTtlSeconds → DefaultLeaseTTLSeconds，
	//    夹进 [MinLeaseTTLSeconds, MaxLeaseTTLSeconds])，夹取结果原样回显，客户端不必自己算。
	// 7. 路由只读校验：房间路由处于 DRAINING 时不接新连接（allowed=false + NO_ROUTE + deny_detail 指向别的节点）；
	//    本方法**不登记** live_gw_room_route——路由写入点是 JoinRoom（见其注释第 4 步），
	//    两条路径都登记会出现「谁先写谁定 primary」的竞态。
	// 8. 签发：Leases.Acquire 以 (node_id, conn_id) SETNX + ULID lease_id；同连接重连由 store
	//    先把旧租约置 RELEASED 再签新的，不留双活租约（否则 Renew/心跳会互相覆盖 last_heartbeat_at）。
	// 9. 随租约下发一次性重连票据（复用 helpers.issueTicket：HMAC 签名 → 只存 sha256 摘要，
	//    明文只在本次响应出现一次）。签名密钥未注入 / 票据张数超限时**保留租约**并记 error：
	//    票据能力缺失不该把「接入」整体打死（ServiceContext.Notes 只把签名故障点名到 Issue/Redeem），
	//    客户端丢了票据走 IssueReconnectTicket 补，而不是不能看直播。
	// 10. 错误映射：allowed=false 的业务拒绝走正常响应且必带 deny_reason + deny_detail；
	//    参数非法/越权/依赖故障返回 error；绝不返回 allowed=true 的空租约。
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	roomID, mid := in.GetRoomId(), in.GetMid()
	connID := strings.TrimSpace(in.GetConnId())
	requestID := strings.TrimSpace(in.GetRequestId())
	traceID := strings.TrimSpace(in.GetTraceId())
	if err := g.requireRoomID(roomID); err != nil {
		return nil, err
	}
	if err := g.requireMid(mid); err != nil {
		return nil, err
	}
	if err := g.requireConnID(connID); err != nil {
		return nil, err
	}
	nodeID, err := g.requireNodeID(in.GetNodeId())
	if err != nil {
		return nil, err
	}
	if err := g.requireRequestID(requestID); err != nil {
		return nil, err
	}
	if err := lgwCheckClientInfo(in.GetClient()); err != nil {
		g.errorf("live-gateway: Acquire 客户端标识被拒 room_id=%d mid=%d conn=%s: %v", roomID, mid, safeIdent(connID), err)
		return nil, err
	}

	deny := func(drop int32, detail string) (*rpc.AcquireConnectionLeaseReply, error) {
		return &rpc.AcquireConnectionLeaseReply{
			Allowed: false, DenyReason: dropReason(drop), DenyDetail: detail,
		}, nil
	}

	// --- 3. 角色判定（先于配额：越权声明不该被配额读数掩盖）---
	role, roleDetail, err := lgwAdjudicateRole(l.ctx, g, int32(in.GetClaimedRole()), roomID, mid)
	if err != nil {
		return nil, err
	}

	// --- 4. 配额（含继承链）---
	eff, err := g.resolveQuota(l.ctx, model.QuotaScopeRoom, roomID)
	if err != nil {
		return nil, err
	}
	if mid == 0 && !eff.AllowGuest {
		return deny(model.DropPermissionDenied, "guest connections are not allowed by this room's quota (allow_guest=false)")
	}
	limit := lgwScanLimit(g)
	countLimit := limit
	if eff.MaxConnections > 0 && eff.MaxConnections < limit {
		countLimit = eff.MaxConnections // 配额低于扫描上限时按配额扫，判定是精确的
	}
	current, cerr := g.svcCtx.Leases.RoomConnectionCount(l.ctx, roomID, countLimit)
	if cerr != nil {
		return nil, cerr
	}
	if eff.MaxConnections > 0 && current >= eff.MaxConnections {
		return deny(model.DropRateLimited,
			fmt.Sprintf("room connection quota reached (%d/%d)", current, eff.MaxConnections))
	}
	if eff.MaxConnections > limit && current >= limit {
		// 房间规模已超过 ConnectionScanLimit：Redis 侧没有 O(1) 计数（SCARD 未暴露，见交付报告缺口），
		// 扫到的只是前 limit 条，此时「没超配额」是不可证的。刻意选择「放行 + 显式告警」而不是
		// 「拒绝大房间的所有新连接」：配额判定要能被证明才有意义，不能被证明时不该拿它当理由封人。
		g.errorf("live-gateway: room_id=%d 连接数达到扫描上限 %d（配额 %d），本次受理的配额判定不可证，"+
			"请调大 LiveGateway.ConnectionScanLimit 或接入集合计数", roomID, limit, eff.MaxConnections)
	}

	// --- 5. 禁止重连窗口（Redis 唯一真值）---
	ban, berr := lgwReadBanUntil(l.ctx, g, roomID, mid)
	if berr != nil {
		return nil, berr
	}
	if now := model.NowUnix(); ban > now {
		return deny(model.DropPermissionDenied, fmt.Sprintf("reconnect forbidden until %d (%ds left)", ban, ban-now))
	}

	// --- 7. 路由校验（只读）：排空中的房间不接新连接 ---
	route, _, rerr := g.routeForFanout(l.ctx, roomID)
	if rerr != nil {
		return nil, rerr
	}
	if route != nil && route.State == model.RouteStateDraining {
		return deny(model.DropNoRoute,
			fmt.Sprintf("room route is draining on node %s, acquire from another node", safeIdent(route.PrimaryNode)))
	}

	// --- 2. request_id 幂等：只读门禁全过之后再占键，校验失败不会把 request_id 烧掉 ---
	replay, value, err := g.idempotentReplay(l.ctx, acquireRPCName, requestID)
	if err != nil {
		return nil, err
	}
	if replay {
		return lgwAcquireReplay(l.ctx, g, value, requestID, current)
	}

	// --- 6/8. TTL 夹取 + 签发租约 ---
	ttl := g.clampLeaseTTL(in.GetTtlSeconds(), eff.LeaseTtlSeconds)
	now := model.NowUnix()
	rec := &repository.LeaseRecord{
		LeaseID:    newID("lgwl"), // 必须与 leaseIDPrefix 同口径（幂等回放靠它辨别占位值）
		ConnID:     connID,
		NodeID:     nodeID,
		RoomID:     roomID,
		Mid:        mid,
		Role:       role,
		State:      model.LeaseStateActive,
		IssuedAt:   now,
		ExpireAt:   now + int64(ttl),
		RequestID:  requestID,
		TraceID:    traceID,
		Platform:   int32(in.GetClient().GetPlatform()),
		AppVersion: strings.TrimSpace(in.GetClient().GetAppVersion()),
	}
	if h := strings.TrimSpace(in.GetClient().GetDeviceIdHash()); h != "" {
		rec.DeviceIDHash = strings.ToLower(h)
	}
	stored, aerr := g.svcCtx.Leases.Acquire(l.ctx, rec, ttl)
	if aerr != nil {
		return nil, aerr
	}

	// --- 9. 随租约下发一次性重连票据（失败保留租约，见要点 9）---
	ticketPlain := lgwIssueTicketWithLease(l.ctx, g, stored, eff, requestID, traceID)

	g.rememberIdempotent(l.ctx, acquireRPCName, requestID, stored.LeaseID)
	info := leaseInfo(stored, ticketPlain)
	// quota_room_conns 是「受理完成后的房间有效连接数」：本次新签发的这条已计入 Redis 集合，
	// 所以要在受理前读到的基数上加一条，否则客户端看到的「房间里有多少人」永远少自己一个。
	afterAdmission := current
	if current < 1<<30 { // 计数只可能到 ConnectionScanLimit，这里只是防 int32 回绕的兜底
		afterAdmission = current + 1
	}
	if roleDetail != "" {
		// 放行但角色被降级：结论写进 deny_detail，客户端与排障都能看到「为什么不是主播」。
		return &rpc.AcquireConnectionLeaseReply{
			Lease: info, Allowed: true, DenyReason: dropReason(model.DropOK),
			DenyDetail: roleDetail, QuotaRoomConns: afterAdmission,
		}, nil
	}
	return &rpc.AcquireConnectionLeaseReply{
		Lease:          info,
		Allowed:        true,
		DenyReason:     dropReason(model.DropOK),
		QuotaRoomConns: afterAdmission,
	}, nil
}

// lgwCheckClientInfo 客户端标识的脱敏门禁：只接受摘要形状，原值一律拒绝。
func lgwCheckClientInfo(info *rpc.ClientInfo) error {
	if info == nil {
		return nil
	}
	hash := strings.TrimSpace(info.GetDeviceIdHash())
	if hash == "" {
		return nil // 不带设备摘要是合法的（客户端可选择不上报），不是「原值为空」
	}
	if len(hash) > maxDeviceIDHashBytes || !lgwIsHexDigest(hash) {
		return fmt.Errorf("%w: device_id_hash must be a 32..%d byte hex digest", model.ErrInvalidClientInfo,
			maxDeviceIDHashBytes)
	}
	return nil
}

// lgwIsHexDigest 判定是否十六进制串（设备号原值通常含非 hex 字符，这一步就能挡住最常见的误传）。
func lgwIsHexDigest(v string) bool {
	if len(v) < 32 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			continue
		}
		return false
	}
	return true
}

// lgwAdjudicateRole 本服务唯一的角色裁决点：claimed_role 只是「请求」，返回值才是「判定」。
//
// 返回的 detail 非空表示「按更小的权限收敛」的原因，会写进响应的 deny_detail。
// 越权声明（客户端链路自报 OPERATOR/SERVICE）返回 ErrPermissionDenied：这类声明不是「降级」而是「造假」，
// 把它降成 VIEWER 放行等于告诉调用方「填错角色也能拿到连接」。
func lgwAdjudicateRole(ctx context.Context, g gate, claimed int32, roomID, mid int64) (int32, string, error) {
	c := callerFrom(ctx)
	if c.isPrivileged() {
		// 归因主体优先于自报角色：内部服务不该被一个填错的 claimed_role 降权，
		// 客户端也不该靠自报 OPERATOR 提权（两条路径都只认 metadata）。
		if c.role == model.RoleService {
			return model.RoleService, "", nil
		}
		return model.RoleOperator, "", nil
	}
	switch claimed {
	case model.RoleOperator, model.RoleService:
		return 0, "", fmt.Errorf("%w: client connection cannot claim role %d, internal callers are identified by gRPC metadata",
			model.ErrPermissionDenied, claimed)
	case model.RoleUnspecified:
		g.infof("live-gateway: Acquire claimed_role 未指定，按 VIEWER 处理 room_id=%d mid=%d", roomID, mid)
		return model.RoleViewer, "", nil
	case model.RoleAnchor, model.RoleRoomAdmin:
		// 两种「房间内的管理角色」都必须问得到真值才承认；问不到就按最小权限收敛。
		ok, reason, err := lgwVerifyRoomPrivilegedRole(ctx, g, claimed, roomID, mid)
		if err != nil {
			return 0, "", err
		}
		if ok {
			return claimed, "", nil
		}
		return model.RoleViewer, reason, nil
	default:
		return model.RoleViewer, "", nil
	}
}

// lgwVerifyRoomPrivilegedRole 向 live-room 问 ANCHOR/ROOM_ADMIN 的真值。
//
// 本仓库的 RoomGate 只有「房间归属主播」一个查询口（没有房管名单，缺口见交付报告），
// 所以 ROOM_ADMIN 恒为「校验不了」；校验不了就按最小权限收敛，绝不因为「问不出来」而放行。
func lgwVerifyRoomPrivilegedRole(ctx context.Context, g gate, role int32, roomID, mid int64) (bool, string, error) {
	if role == model.RoleRoomAdmin {
		return false, "room admin role unverifiable, connected as VIEWER", nil
	}
	if mid <= 0 {
		return false, "guest connection cannot hold the anchor role, connected as VIEWER", nil
	}
	ok, err := g.svcCtx.Rooms.IsOwner(ctx, roomID, mid)
	if err != nil {
		if errors.Is(err, repository.ErrLiveRoomNotConfigured) {
			g.errorf("live-gateway: live-room 未接线，ANCHOR(mid=%d) 在房间 %d 的声明无法校验，按 VIEWER 接入", mid, roomID)
			return false, "anchor ownership unverifiable (live-room rpc not wired), connected as VIEWER", nil
		}
		return false, "", err
	}
	if !ok {
		return false, "mid is not the owner of this room, connected as VIEWER", nil
	}
	return true, "", nil
}

// lgwAcquireReplay 回放首次签发的租约：同一条 lease_id，不重新判角色、不重耗配额、不再回显票据。
func lgwAcquireReplay(ctx context.Context, g gate, value, requestID string, current int32) (*rpc.AcquireConnectionLeaseReply, error) {
	leaseID := strings.TrimSpace(value)
	// 幂等窗口的占位值（已抢键、结论未回填）与任何脏值都不是租约 ID：
	// 与其伪造一个结论，不如让调用方用同一 request_id 稍后重试。
	if !strings.HasPrefix(leaseID, leaseIDPrefix) {
		return nil, fmt.Errorf("%w: AcquireConnectionLease conclusion for request_id=%s not recorded yet, retry",
			model.ErrRequestIdDuplicated, safeIdent(requestID))
	}
	rec, err := g.leaseLookup(ctx, leaseID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		// 首答的租约已经不在了（过期/被释放）：这是真实结论，不能拿一条新租约冒充「同一次请求的结果」。
		return &rpc.AcquireConnectionLeaseReply{
			Allowed: false, DenyReason: dropReason(model.DropBadTicket),
			DenyDetail:     "the lease issued for this request_id is gone, acquire again with a new request_id",
			QuotaRoomConns: current,
		}, nil
	}
	return &rpc.AcquireConnectionLeaseReply{
		Lease:          leaseInfo(rec, ""),
		Allowed:        true,
		DenyReason:     dropReason(model.DropOK),
		DenyDetail:     "idempotent replay of request_id: reconnect ticket is not echoed again",
		QuotaRoomConns: current,
	}, nil
}

// lgwIssueTicketWithLease 与租约同批签一张一次性重连票据，返回票据原文（只回显这一次）。
//
// 任何失败都只记日志并返回空串：租约已经签发，把「票据能力缺失」放大成「连不上」是更糟的结果。
// 票据断言里的角色用的是**判定后**的角色（stored.Role），不是客户端自报值。
func lgwIssueTicketWithLease(ctx context.Context, g gate, stored *repository.LeaseRecord,
	eff *model.EffectiveQuota, requestID, traceID string) string {
	if !g.svcCtx.Signer.Available() {
		g.errorf("live-gateway: 签名密钥未注入，租约 %s 本次不下发重连票据（客户端可走 IssueReconnectTicket 补）",
			safeIdent(stored.LeaseID))
		return ""
	}
	ttl := g.clampTicketTTL(0, eff.TicketTtlSeconds)
	row, ticket, err := g.issueTicket(ctx, ticketRequest{
		RoomID:     stored.RoomID,
		Mid:        stored.Mid,
		ConnID:     stored.ConnID,
		NodeID:     stored.NodeID,
		Role:       stored.Role,
		TTLSeconds: ttl,
		RequestID:  requestID,
		LeaseID:    stored.LeaseID,
		Reason:     ticketReasonNormal,
		TraceID:    traceID,
	})
	if err != nil {
		g.errorf("live-gateway: 租约 %s 的重连票据签发失败（租约仍有效）: %v", safeIdent(stored.LeaseID), err)
		return ""
	}
	// 只把 ticket_id 写回租约：票据明文永不进 Redis（README 安全约束，LeaseRecord.reconnect_ticket 留空）。
	stored.TicketID = row.TicketId
	if perr := g.svcCtx.Leases.Put(ctx, stored); perr != nil {
		g.errorf("live-gateway: 租约 %s 回填 ticket_id 失败（票据已签发，兑换仍可用）: %v",
			safeIdent(stored.LeaseID), perr)
	}
	return ticket
}
