package logic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"go-video/common/idgen"
	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/model"
)

// 本文件是 22 个 RPC 共用的门禁构件。每一条都对应 README/proto 里的一条安全约束，
// 集中实现的意义是：广播、单播、弹幕转发三条下发路径不可能出现「权限口径不一致」。

// defaultMaxIDLenBytes 标识列长度兜底（与迁移 SQL 的 VARCHAR(64) 对齐）。
const defaultMaxIDLenBytes = 64

// 超长标识的拒绝理由：MySQL 非严格模式会静默截断，截断后的 request_id 与客户端记的那条不是同一个值，
// 幂等重放就会变成「重放没命中 → 又写一行」，比直接报错难查十倍。
func (g gate) checkIdent(name, v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%w: %s is required", model.ErrEmptyRequestID, name)
	}
	if len(v) > g.maxIDLen || strings.ContainsAny(v, " \t\r\n") {
		return fmt.Errorf("%w: %s must be 1..%d bytes without whitespace", model.ErrEmptyRequestID, name, g.maxIDLen)
	}
	return nil
}

func (g gate) requireRequestID(v string) error {
	if strings.TrimSpace(v) == "" {
		return model.ErrEmptyRequestID
	}
	return g.checkIdent("request_id", v)
}

func (g gate) requireMessageID(v string) error {
	if strings.TrimSpace(v) == "" {
		return model.ErrEmptyMessageID
	}
	return g.checkIdent("message_id", v)
}

func (g gate) requireEventID(v string) error {
	if strings.TrimSpace(v) == "" {
		return model.ErrEmptyEventID
	}
	return g.checkIdent("event_id", v)
}

func (g gate) requireRoomID(roomID int64) error {
	if roomID <= 0 {
		return model.ErrInvalidRoomID
	}
	return nil
}

// requireMid 允许 0（游客）：是否放行由配额 allow_guest 决定，不是由代码写死。
func (g gate) requireMid(mid int64) error {
	if mid < 0 {
		return model.ErrInvalidMid
	}
	return nil
}

func (g gate) requireConnID(v string) error {
	if strings.TrimSpace(v) == "" {
		return model.ErrEmptyConnID
	}
	if len(v) > g.maxIDLen || strings.ContainsAny(v, " \t\r\n") {
		return fmt.Errorf("%w: conn_id must be 1..%d bytes without whitespace", model.ErrEmptyConnID, g.maxIDLen)
	}
	return nil
}

func (g gate) requireNodeID(v string) (string, error) {
	return repository.CleanNodeID(v)
}

// requireReason 处置类动作必须留原因，空原因的审计行等于没审计。
func (g gate) requireReason(v string) error {
	if strings.TrimSpace(v) == "" {
		return model.ErrEmptyReason
	}
	if len(v) > 256 {
		return fmt.Errorf("%w: reason too long (max 256 bytes)", model.ErrEmptyReason)
	}
	return nil
}

// requireOperatorField 校验 operator 展示字段（真正的主体归因来自 metadata，见 caller.go）。
func (g gate) requireOperatorField(v string) error {
	if strings.TrimSpace(v) == "" {
		return model.ErrEmptyOperator
	}
	if len(v) > 64 || strings.ContainsAny(v, "\n\r") {
		return fmt.Errorf("%w: operator must be 1..64 bytes without newlines", model.ErrEmptyOperator)
	}
	return nil
}

// clampLeaseTTL 租约 TTL 夹取：请求值 → 配额值 → 配置默认，最后夹进 [min, max]。
// Acquire / Renew / Redeem / Join 共用同一个函数，否则三条路径给出的 TTL 会互相矛盾。
func (g gate) clampLeaseTTL(requested, quotaTTL int32) int {
	cfg := g.cfg()
	v := requested
	if v <= 0 {
		v = quotaTTL
	}
	if v <= 0 {
		v = cfg.DefaultLeaseTTLSeconds
	}
	min, max := cfg.MinLeaseTTLSeconds, cfg.MaxLeaseTTLSeconds
	if min <= 0 {
		min = 10
	}
	if max < min {
		max = min
	}
	if v < min {
		v = min
	}
	if v > max {
		v = max
	}
	return int(v)
}

// clampTicketTTL 票据 TTL 夹取：下限固定 1 秒（票据没有「太小打爆 Redis」的问题，但有 0 秒即过期的坑），
// 上限受配置与配额双重约束。
func (g gate) clampTicketTTL(requested, quotaTTL int32) int {
	cfg := g.cfg()
	v := requested
	if v <= 0 {
		v = quotaTTL
	}
	if v <= 0 {
		v = cfg.DefaultTicketTTLSeconds
	}
	if v <= 0 {
		v = 120
	}
	max := cfg.MaxTicketTTLSeconds
	if max <= 0 {
		max = 600
	}
	if v > max {
		v = max
	}
	if v < 1 {
		v = 1
	}
	return int(v)
}

// quotaDefaultsFromConfig 把进程配置翻成 model 的配额兜底层。
// 顺序是「DB 继承链 → 进程默认」：无 GLOBAL 行时不报错，README 明确「配置是兜底，数据以 DB 为准」。
func quotaDefaultsFromConfig(cfg configView) model.QuotaDefaults {
	return model.QuotaDefaults{
		MaxConnections:   cfg.defaultMaxRoomConnections(),
		BroadcastQps:     cfg.defaultRoomBroadcastQps(),
		DanmakuQps:       cfg.defaultUserBroadcastQps(),
		LeaseTtlSeconds:  cfg.defaultLeaseTTL(),
		TicketTtlSeconds: cfg.defaultTicketTTL(),
		MaxPayloadBytes:  cfg.maxPayloadBytes(),
		AllowGuest:       cfg.allowGuestByDefault(),
	}
}

// resolveQuota 解析生效配额（含继承链），并按 QuotaCacheTTLSeconds 读缓存。
//
// 缓存失效靠「配额代次 epoch」：任何一次 UpsertAccessQuota 都 INCR epoch，
// 于是 GLOBAL 降配立刻对所有 ROOM 解析结果生效，不需要 SCAN 出几百个键逐个删。
// Redis 不可用时直接回源 DB —— 拿不到缓存不是返回默认值的理由，
// 默认值和运营意图相反时，限流判定会静默偏离（README 数据分层里的显式禁令）。
func (g gate) resolveQuota(ctx context.Context, scope int32, scopeID int64) (*model.EffectiveQuota, error) {
	if !model.ValidQuotaScope(scope) {
		return nil, model.ErrInvalidQuotaScope
	}
	defaults := quotaDefaultsFromConfig(g)
	ttl := g.cfg().QuotaCacheTTLSeconds
	cacheOK := false
	var epoch int32
	if ttl > 0 {
		if e, err := g.svcCtx.Leases.QuotaEpoch(ctx); err == nil {
			epoch, cacheOK = e, true
			var cached model.EffectiveQuota
			hit, cerr := g.svcCtx.Leases.CacheGet(ctx, repository.QuotaCacheKey(epoch, scope, scopeID), &cached)
			if cerr == nil && hit {
				return &cached, nil
			}
		} else {
			g.errorf("live-gateway: 配额读缓存不可用，本次直接回源 DB: %v", err)
		}
	}
	store, err := g.svcCtx.RequireStore()
	if err != nil {
		return nil, err
	}
	eff, err := store.Quotas.Resolve(ctx, scope, scopeID, defaults)
	if err != nil {
		// 解析失败绝不降级为默认值：那会让「运营已降配」被静默读成「按默认放行」。
		return nil, err
	}
	if cacheOK {
		if serr := g.svcCtx.Leases.CacheSet(ctx, repository.QuotaCacheKey(epoch, scope, scopeID), eff, ttl); serr != nil {
			g.errorf("live-gateway: 配额读缓存写入失败（不影响正确性）: %v", serr)
		}
	}
	return eff, nil
}

// payloadDigest 只产出摘要，载荷正文永不入库也不进日志（AGENTS.md §6 + README 数据分层）。
func payloadDigest(payload []byte, digestBytes int) string {
	if len(payload) == 0 {
		return ""
	}
	if digestBytes <= 0 || digestBytes > 64 {
		digestBytes = 32
	}
	sum := sha256.Sum256(payload)
	hexStr := hex.EncodeToString(sum[:])
	if digestBytes > len(hexStr) {
		digestBytes = len(hexStr)
	}
	return hexStr[:digestBytes]
}

func payloadBytes(payload []byte) int32 {
	if len(payload) > int(^uint32(0)>>1) {
		return int32(^uint32(0) >> 1)
	}
	return int32(len(payload))
}

// auditParams 一行广播审计的入参。state/drop_reason 的组合合法性由 model.Insert 兜底校验，
// 这里只保证「摘要而非正文」——审计表里出现弹幕正文是本次交付最不允许的事故。
type auditParams struct {
	RoomID     int64
	Kind       int32
	MessageID  string
	EventID    string
	SenderMid  int64
	SenderRole int32
	Payload    []byte
	Fanout     int32
	Targeted   int32
	State      int32
	Drop       int32
	Source     string
	TraceID    string
}

// writeAudit 落审计行。返回错误而不是忽略：
// 处置类调用「发出去了但查不到」与「没发出去」在追责时同样不可接受（AGENTS.md §8 处置留痕）。
// 唯一例外是 kind 非法的参数门禁阶段——那一行本就不该进表，由调用方自行决定不调用。
func (g gate) writeAudit(ctx context.Context, p auditParams) error {
	store, err := g.svcCtx.RequireStore()
	if err != nil {
		return err
	}
	row := &model.LiveGwBroadcastLog{
		MessageId:           p.MessageID,
		RoomId:              p.RoomID,
		Kind:                p.Kind,
		SenderMid:           p.SenderMid,
		SenderRole:          model.NormalizeRole(p.SenderRole),
		EventId:             p.EventID,
		PayloadDigest:       payloadDigest(p.Payload, g.cfg().PayloadDigestBytes),
		PayloadBytes:        payloadBytes(p.Payload),
		FanoutNodes:         p.Fanout,
		TargetedConnections: p.Targeted,
		State:               p.State,
		DropReason:          p.Drop,
		SourceService:       safeIdent(p.Source),
		TraceId:             p.TraceID,
		Ctime:               model.NowUnix(),
	}
	if row.State == model.BroadcastLogSent {
		row.DropReason = model.DropOK
	}
	if _, err := store.BroadcastLogs.Insert(ctx, row); err != nil {
		return fmt.Errorf("live-gateway: broadcast audit: %w", err)
	}
	return nil
}

// --- 配额视图：把 config 的字段名收敛成一组语义化访问器，避免 quotaDefaultsFromConfig 里出现裸配置名 ---

type configView interface {
	defaultMaxRoomConnections() int32
	defaultRoomBroadcastQps() int32
	defaultUserBroadcastQps() int32
	defaultLeaseTTL() int32
	defaultTicketTTL() int32
	maxPayloadBytes() int32
	allowGuestByDefault() bool
}

func (g gate) defaultMaxRoomConnections() int32 { return g.cfg().DefaultMaxRoomConnections }
func (g gate) defaultRoomBroadcastQps() int32   { return g.cfg().DefaultRoomBroadcastQps }
func (g gate) defaultUserBroadcastQps() int32   { return g.cfg().DefaultUserBroadcastQps }
func (g gate) defaultLeaseTTL() int32           { return g.cfg().DefaultLeaseTTLSeconds }
func (g gate) defaultTicketTTL() int32          { return g.cfg().DefaultTicketTTLSeconds }
func (g gate) allowGuestByDefault() bool        { return g.cfg().AllowGuestByDefault }
func (g gate) maxPayloadBytes() int32 {
	if v := g.cfg().MaxPayloadBytes; v > 0 {
		return v
	}
	return 32768
}

// --- 租约三元组与凭据 ---

// leaseLookup 按 lease_id 取租约；不存在返回 (nil, nil) 由调用方翻译成各自的业务结论
// （Renew 要报错、GetConnectionLease 要回 found=false、广播要按 BAD_TICKET 丢弃）。
func (g gate) leaseLookup(ctx context.Context, leaseID string) (*repository.LeaseRecord, error) {
	if strings.TrimSpace(leaseID) == "" {
		return nil, model.ErrEmptyLeaseID
	}
	return g.svcCtx.Leases.Get(ctx, leaseID)
}

// tripleMatch 服务端判定的三元组一致性：room_id 与 mid 都必须与租约登记值相同。
// 任何一处不同都按越权处理，绝不「顺手改绑」——那等于把别人的连接劫持过来。
func tripleMatch(rec *repository.LeaseRecord, roomID, mid int64) bool {
	if rec == nil {
		return false
	}
	if rec.RoomID != roomID {
		return false
	}
	if mid >= 0 && rec.Mid != mid {
		return false
	}
	return true
}

// usableLease 判定租约是否还能作为凭据：存在、非终态、未过期。
// 返回的 reason 是可解释结论（BAD_TICKET / PERMISSION_DENIED），err 只在存储故障时非空。
func (g gate) usableLease(ctx context.Context, leaseID string, roomID, mid int64) (*repository.LeaseRecord, int32, string, error) {
	if strings.TrimSpace(leaseID) == "" {
		return nil, model.DropPermissionDenied, "sender credential required", nil
	}
	rec, err := g.leaseLookup(ctx, leaseID)
	if err != nil {
		return nil, 0, "", err
	}
	if rec == nil {
		return nil, model.DropBadTicket, "lease not found or expired from cache", nil
	}
	if !tripleMatch(rec, roomID, mid) {
		return nil, model.DropPermissionDenied, "credential does not match (room_id, mid)", nil
	}
	if model.IsLeaseTerminal(rec.State) {
		return nil, model.DropBadTicket, "lease already released or kicked", nil
	}
	if rec.EffectiveState(model.NowUnix()) == model.LeaseStateExpired {
		return nil, model.DropBadTicket, "lease expired", nil
	}
	return rec, 0, "", nil
}

// resolveSender 是三条下发路径共用的发送者鉴权（权限矩阵 + 凭据三元组）。
//
// 判定顺序即契约：先归一角色 → 可信来源类别 → 权限矩阵 → 凭据。
// 矩阵判定放前面，是为了让「VIEWER 发 MODERATION」这类明确越权在审计里
// 不因缺少凭据而被降级成 BAD_TICKET（原因必须是真实的那一个）。
type senderVerdict struct {
	Role   int32
	Mid    int64
	Lease  *repository.LeaseRecord
	Drop   int32 // 0 表示通过
	Detail string
	// PrivilegedCaller 表示可信主体（attested OPERATOR/SERVICE），可免用户态凭据。
	PrivilegedCaller bool
}

func (g gate) resolveSender(ctx context.Context, roomID, senderMid int64, claimedRole, kind int32, leaseID, ticket string) (senderVerdict, error) {
	c := callerFrom(ctx)
	role := model.NormalizeRole(claimedRole)
	v := senderVerdict{Role: role, Mid: senderMid, PrivilegedCaller: c.isPrivileged()}

	if c.isPrivileged() {
		// 归因主体优先于自报角色：内部服务不该被一个填错的 claimed_role 降权，
		// 客户端也不该靠自报 OPERATOR 提权（两条路径都只认 metadata）。
		v.Role = c.role
		role = c.role
	}

	if drop, detail := g.matrixDrop(role, kind); drop != 0 {
		v.Drop, v.Detail = drop, detail
		return v, nil
	}

	// 可信内部主体可免凭据；其余一律要有租约或票据，且必须与 (mid, room_id) 三元组吻合。
	if v.PrivilegedCaller && leaseID == "" && ticket == "" {
		return v, nil
	}
	if leaseID == "" && ticket == "" {
		v.Drop = model.DropPermissionDenied
		v.Detail = "user-scoped send requires sender_lease_id or sender_ticket"
		return v, nil
	}
	if leaseID != "" {
		rec, drop, detail, err := g.usableLease(ctx, leaseID, roomID, v.Mid)
		if err != nil {
			return v, err
		}
		if drop != 0 {
			v.Drop, v.Detail = drop, detail
			return v, nil
		}
		// 租约上的服务端判定角色才是真角色：claimed_role 只在内部链路里被采信。
		v.Role = model.NormalizeRole(rec.Role)
		v.Mid = rec.Mid
		v.Lease = rec
		// 换完角色必须复检矩阵：否则「VIEWER 租约 + 自报 OPERATOR」会用自报角色过闸、
		// 只把审计归因降级，等于给未归因客户端开提权通道。
		// 归因主体（metadata 认定）不在此列——它的矩阵判定按归因角色（宽），凭据只用于记账。
		if !v.PrivilegedCaller {
			if drop, detail := g.matrixDrop(v.Role, kind); drop != 0 {
				v.Drop, v.Detail = drop, detail
				return v, nil
			}
		}
		return g.gateAnchorOwnership(ctx, v, roomID)
	}
	tk, drop, detail, err := g.verifyTicketCredential(ctx, ticket, roomID, v.Mid)
	if err != nil {
		return v, err
	}
	if drop != 0 {
		v.Drop, v.Detail = drop, detail
		return v, nil
	}
	v.Role = model.NormalizeRole(tk.Role)
	v.Mid = tk.Mid
	if !v.PrivilegedCaller {
		if drop, detail := g.matrixDrop(v.Role, kind); drop != 0 {
			v.Drop, v.Detail = drop, detail
			return v, nil
		}
	}
	return g.gateAnchorOwnership(ctx, v, roomID)
}

// matrixDrop 是发送侧权限矩阵的两条判定（可信类别 + 角色-类别白名单），
// 供「自报角色」与「凭据裁决出的角色」各跑一次。
func (g gate) matrixDrop(role, kind int32) (int32, string) {
	if model.KindRequiresTrustedSender(kind) && role != model.RoleService && role != model.RoleOperator {
		return model.DropPermissionDenied, "broadcast kind requires trusted sender (SERVICE/OPERATOR)"
	}
	if !model.RoleAllowedToSend(role, kind) {
		return model.DropPermissionDenied, fmt.Sprintf("role %d cannot send kind %d", role, kind)
	}
	return 0, ""
}

// gateAnchorOwnership ANCHOR 角色的最终确认：租约上的 ANCHOR 是 Acquire 时问过 live-room 的结论，
// 但房间可能已经换绑主播，下发前必须再问一次（README 权限矩阵的时效性要求）。
// 问不到时 fail-closed 降为 VIEWER —— 与 Acquire 同方向，绝不把「问不到」当成「判定通过」。
func (g gate) gateAnchorOwnership(ctx context.Context, v senderVerdict, roomID int64) (senderVerdict, error) {
	if v.Role != model.RoleAnchor {
		return v, nil
	}
	ok, err := g.svcCtx.Rooms.IsOwner(ctx, roomID, v.Mid)
	if err != nil {
		if errors.Is(err, repository.ErrLiveRoomNotConfigured) {
			g.errorf("live-gateway: live-room 未接线，ANCHOR(mid=%d) 在房间 %d 降级为 VIEWER", v.Mid, roomID)
			v.Role = model.RoleViewer
			v.Detail = "anchor role downgraded: live-room ownership unverifiable"
			return v, nil
		}
		return v, err
	}
	if !ok {
		v.Role = model.RoleViewer
		v.Detail = "anchor role downgraded: mid is not the room owner"
	}
	return v, nil
}

// verifyTicketCredential 用票据（不消费）证明发送者身份：验签 + 三元组 + 状态/时效。
// 只校验不兑换是刻意的：Redeem 才是一次性消费点，广播鉴权消费票据会让客户端下一次重连必失败。
func (g gate) verifyTicketCredential(ctx context.Context, ticket string, roomID, mid int64) (*model.LiveGwReconnectTicket, int32, string, error) {
	if strings.TrimSpace(ticket) == "" {
		return nil, model.DropPermissionDenied, "sender_ticket is empty", nil
	}
	if !g.svcCtx.Signer.Available() {
		return nil, 0, "", fmt.Errorf("%w: cannot verify sender_ticket", repository.ErrSignerMissing)
	}
	ticketID, err := g.svcCtx.Signer.VerifyTicketShape(ticket)
	if err != nil {
		return nil, model.DropBadTicket, "malformed sender_ticket", nil
	}
	store, serr := g.svcCtx.RequireStore()
	if serr != nil {
		return nil, 0, "", serr
	}
	row, ferr := store.Tickets.FindByHash(ctx, model.TicketHash(ticket))
	if ferr != nil {
		return nil, 0, "", ferr
	}
	if row == nil {
		return nil, model.DropBadTicket, "sender_ticket unknown", nil
	}
	if row.TicketId != ticketID {
		// ticket_id 与摘要不同源：票据串被拼接过，直接判 BAD_TICKET，不回显任何票据字段。
		return nil, model.DropBadTicket, "sender_ticket id mismatch", nil
	}
	if verr := g.svcCtx.Signer.Verify(ticket, ticketClaimsOf(row)); verr != nil {
		return nil, model.DropBadTicket, "sender_ticket signature invalid", nil
	}
	if row.RoomId != roomID || (mid >= 0 && row.Mid != mid) {
		return nil, model.DropPermissionDenied, "sender_ticket does not match (room_id, mid)", nil
	}
	switch {
	case model.IsTicketTerminal(row.State):
		return nil, model.DropBadTicket, "sender_ticket already used or revoked", nil
	case model.Expired(row.ExpireAt, model.NowUnix()):
		return nil, model.DropBadTicket, "sender_ticket expired", nil
	}
	return row, 0, "", nil
}

// ticketClaimsOf 从 DB 行还原签名断言（票据串本身不含明文身份，断言真值只在行里）。
func ticketClaimsOf(row *model.LiveGwReconnectTicket) repository.TicketClaims {
	return repository.TicketClaims{
		TicketID: row.TicketId,
		RoomID:   row.RoomId,
		Mid:      row.Mid,
		ConnID:   row.ConnId,
		Role:     row.Role,
		ExpireAt: row.ExpireAt,
	}
}

// pageParams 归一分页：pn<1 归 1；ps<=0 用默认 20；ps>maxPS 直接拒绝而不是截断
// （静默截断会让调用方以为「还有下一页」而写出错误的翻页循环）。
func (g gate) pageParams(pn, ps int32) (int32, int32, error) {
	maxPS := g.cfg().MaxPageSize
	if maxPS <= 0 {
		maxPS = 50
	}
	if pn < 1 {
		pn = 1
	}
	if ps <= 0 {
		ps = 20
	}
	if ps > maxPS {
		return 0, 0, fmt.Errorf("%w: ps=%d, max=%d", model.ErrPsTooLarge, ps, maxPS)
	}
	return pn, ps, nil
}

// fanoutTargets 计算扇出节点：主节点必带，副本节点带全部，DRAINING 节点单独回报。
// 路由处于 DRAINING 时 primary 仍在列表里是刻意的——排空期存量连接还得收到消息，
// 但 fanout_nodes 的语义是「本服务判定要打的节点数」，把排空节点算进去会让运营以为没排干净。
func fanoutTargets(primary string, replicas []string, state int32) (nodes []string, draining []string) {
	nodes = make([]string, 0, len(replicas)+1)
	draining = make([]string, 0, 2)
	appendNode := func(list []string, n string) []string {
		n = strings.TrimSpace(n)
		if n == "" {
			return list
		}
		for _, exist := range list {
			if exist == n {
				return list
			}
		}
		return append(list, n)
	}
	if state == model.RouteStateDraining {
		draining = appendNode(draining, primary)
	} else {
		nodes = appendNode(nodes, primary)
	}
	for _, r := range replicas {
		if state == model.RouteStateDraining && strings.TrimSpace(r) == strings.TrimSpace(primary) {
			continue
		}
		nodes = appendNode(nodes, r)
	}
	return nodes, draining
}

// fanout 执行下发并把「通道未接线」翻译成可丢弃结论还是错误。
// require_reliable=true 时必须显式失败：审核处置「静默丢失」等于处置未执行。
func (g gate) fanout(ctx context.Context, req *repository.FanoutRequest, requireReliable bool) (*repository.FanoutResult, int32, error) {
	res, err := g.svcCtx.Fanout.Fanout(ctx, req)
	if err != nil {
		if errors.Is(err, model.ErrTransportUnavailable) {
			if requireReliable {
				return nil, model.DropTransportUnavailable, err
			}
			return &repository.FanoutResult{FanoutNodes: int32(len(req.Nodes))}, model.DropTransportUnavailable, nil
		}
		return nil, 0, err
	}
	return res, 0, nil
}

// deliver 是定向投递版本（SendToUser），语义同上。
func (g gate) deliver(ctx context.Context, req *repository.FanoutRequest, leaseIDs []string, requireReliable bool) (*repository.FanoutResult, int32, error) {
	res, err := g.svcCtx.Fanout.Deliver(ctx, req, leaseIDs)
	if err != nil {
		if errors.Is(err, model.ErrTransportUnavailable) {
			if requireReliable {
				return nil, model.DropTransportUnavailable, err
			}
			return &repository.FanoutResult{FanoutNodes: int32(len(req.Nodes)), TargetedConnections: int32(len(leaseIDs))},
				model.DropTransportUnavailable, nil
		}
		return nil, 0, err
	}
	return res, 0, nil
}

// routeForFanout 读房间路由（含读缓存），返回行 + 副本节点。
// 无行/非 SERVING 由调用方翻译成 DROP_REASON_NO_ROUTE；ErrRouteNotFound 只在需要报错的路径上用。
func (g gate) routeForFanout(ctx context.Context, roomID int64) (*model.LiveGwRoomRoute, []string, error) {
	store, err := g.svcCtx.RequireStore()
	if err != nil {
		return nil, nil, err
	}
	ttl := g.cfg().RoomRouteCacheTTLSeconds
	cacheKey := repository.RouteCacheKey(roomID)
	if ttl > 0 {
		var cached model.LiveGwRoomRoute
		hit, cerr := g.svcCtx.Leases.CacheGet(ctx, cacheKey, &cached)
		if cerr == nil && hit && cached.RoomId == roomID {
			replicas, derr := cached.ReplicaNodesList()
			if derr != nil {
				g.errorf("live-gateway: 路由缓存 replica_nodes 脏数据，回源 DB: %v", derr)
			} else {
				return &cached, replicas, nil
			}
		}
	}
	row, err := store.RoomRoutes.FindOne(ctx, roomID)
	if err != nil {
		return nil, nil, err
	}
	if row == nil {
		return nil, nil, nil
	}
	replicas, derr := row.ReplicaNodesList()
	if derr != nil {
		// 解析失败按「无副本」继续而不是拒发：路由主节点仍有效，副本只在分片广播时才需要；
		// 但必须记告警，脏数据要被人看见（静默当无副本会让大房间少打一半节点）。
		g.errorf("live-gateway: live_gw_room_route replica_nodes 解析失败 room_id=%d，按无副本处理: %v", roomID, derr)
		replicas = nil
	}
	if ttl > 0 {
		if serr := g.svcCtx.Leases.CacheSet(ctx, cacheKey, row, ttl); serr != nil {
			g.errorf("live-gateway: 路由读缓存写入失败（不影响正确性）: %v", serr)
		}
	}
	return row, replicas, nil
}

// invalidateRouteCache 写路径主动失效（Drain/Register 后必须调，否则广播还会打到旧节点一个缓存周期）。
func (g gate) invalidateRouteCache(ctx context.Context, roomID int64) {
	if err := g.svcCtx.Leases.CacheDel(ctx, repository.RouteCacheKey(roomID)); err != nil {
		g.errorf("live-gateway: 路由缓存失效失败 room_id=%d，最多 %d 秒内广播可能打到旧节点: %v",
			roomID, g.cfg().RoomRouteCacheTTLSeconds, err)
	}
}

// idempotentReplay 写接口幂等窗口：先占 Redis 键，命中即回放首次结论。
// 返回 replay=true 时 value 是首次结果标识（lease_id / ticket_id / ...）。
func (g gate) idempotentReplay(ctx context.Context, rpcName, requestID string) (bool, string, error) {
	if err := g.requireRequestID(requestID); err != nil {
		return false, "", err
	}
	value, fresh, err := g.svcCtx.Leases.ClaimRequest(ctx, rpcName, requestID, g.dedupWindow())
	if err != nil {
		// Redis 不可用时不能当作「首次请求」放行：那会让重试各写一行，幂等键形同不存在。
		return false, "", err
	}
	if !fresh {
		return true, value, nil
	}
	return false, "", nil
}

// rememberIdempotent 回填幂等结论；失败只记日志（业务结果已产生，回填失败不该让调用方重试整条写路径）。
func (g gate) rememberIdempotent(ctx context.Context, rpcName, requestID, value string) {
	if err := g.svcCtx.Leases.RememberRequest(ctx, rpcName, requestID, value, g.dedupWindow()); err != nil {
		g.errorf("live-gateway: %s 幂等结论回填失败 request_id=%s: %v", rpcName, safeIdent(requestID), err)
	}
}

// newID 生成本服务的实体标识："<prefix>_<ULID>"（common/idgen，单调递增熵，字典序≈时间序）。
// 租约 ID 与票据 ID 都落在 VARCHAR(64) 里：31 字符，留足余量，也保证不可猜。
func newID(prefix string) string {
	id, err := idgen.Prefixed(prefix)
	if err != nil {
		// idgen 的熵源是 crypto/rand，出错即机器随机源不可用。
		// 这里不静默退化到时间戳：票据/租约 ID 可猜等于凭据可猜，进程宁可起不来。
		panic(fmt.Sprintf("live-gateway: idgen unavailable: %v", err))
	}
	return id
}

// dedupWindow 去重/幂等窗口秒数（Redis 键 TTL）。配置非正数时兜底 600s：
// 窗口为 0 等于「不记幂等」，重试会各写一行，那是比多存 600 秒更糟的结果。
func (g gate) dedupWindow() int {
	v := int(g.cfg().DedupWindowSeconds)
	if v <= 0 {
		return 600
	}
	return v
}

// rateLimit 固定窗口限流。limit<=0 表示不限制（配额 0 的继承语义已由 Resolve 展开，这里只剩「显式无限」）。
func (g gate) rateLimit(ctx context.Context, label string, limit int32) (bool, int32, error) {
	if limit <= 0 {
		return true, 0, nil
	}
	window := int(g.cfg().RateWindowSeconds)
	return g.svcCtx.Leases.AllowRate(ctx, label, limit, window)
}

// --- 票据签发（Acquire 第 8 步与 IssueReconnectTicket 共用同一条路径）---

// issueTicket 签一张一次性重连票据：HMAC 签名 → 只把 sha256 摘要写进 DB 与 Redis 有效位。
//
// 返回的票据原文由调用方回显一次，永不落库（README 安全约束）。
// request_id 重放时凭 ticket_id + 行内断言**重算**签名得到同一串：
// HMAC-SHA256 对同一断言是确定性的，所以「幂等回放原票据」不需要存明文，
// 也不需要把凭据交给 Redis 的另一个键 —— 只有持有签名密钥的服务端能重建它。
func (g gate) issueTicket(ctx context.Context, p ticketRequest) (*model.LiveGwReconnectTicket, string, error) {
	if !g.svcCtx.Signer.Available() {
		return nil, "", fmt.Errorf("%w: refusing to issue an unsigned reconnect ticket", repository.ErrSignerMissing)
	}
	store, err := g.svcCtx.RequireStore()
	if err != nil {
		return nil, "", err
	}
	if p.RequestID != "" {
		row, ferr := store.Tickets.FindByRequestID(ctx, p.RequestID)
		if ferr != nil {
			return nil, "", ferr
		}
		if row != nil {
			replay, rerr := g.rebuildTicket(*row)
			if rerr != nil {
				return nil, "", rerr
			}
			return row, replay, nil
		}
	}
	if limit := g.cfg().MaxTicketsPerSubject; limit > 0 {
		used, cerr := store.Tickets.CountUnusedByRoomMid(ctx, p.RoomID, p.Mid)
		if cerr != nil {
			return nil, "", cerr
		}
		if used >= int64(limit) {
			return nil, "", fmt.Errorf("%w: %d unused tickets for room %d, max %d per subject",
				model.ErrQuotaExceeded, used, p.RoomID, limit)
		}
	}
	now := model.NowUnix()
	claims := repository.TicketClaims{
		TicketID: newID("lgwt"),
		RoomID:   p.RoomID,
		Mid:      p.Mid,
		ConnID:   p.ConnID,
		Role:     model.NormalizeRole(p.Role),
		ExpireAt: now + int64(p.TTLSeconds),
	}
	ticket, serr := g.svcCtx.Signer.Sign(claims)
	if serr != nil {
		return nil, "", serr
	}
	row := &model.LiveGwReconnectTicket{
		TicketId:    claims.TicketID,
		TicketHash:  model.TicketHash(ticket),
		RoomId:      p.RoomID,
		Mid:         p.Mid,
		ConnId:      p.ConnID,
		NodeId:      p.NodeID,
		Role:        claims.Role,
		State:       model.TicketStateIssued,
		IssuedAt:    now,
		ExpireAt:    claims.ExpireAt,
		LeaseId:     p.LeaseID,
		IssueReason: p.Reason,
		RequestId:   p.RequestID,
		TraceId:     p.TraceID,
	}
	if _, ierr := store.Tickets.Insert(ctx, row); ierr != nil {
		if !errors.Is(ierr, model.ErrRequestIdDuplicated) {
			return nil, "", ierr
		}
		// 并发下另一个请求用同一 request_id 先签成功了：回读它那一张，绝不产生第二张票。
		existing, ferr := store.Tickets.FindByRequestID(ctx, p.RequestID)
		if ferr != nil {
			return nil, "", ferr
		}
		if existing == nil {
			return nil, "", ierr
		}
		replay, rerr := g.rebuildTicket(*existing)
		if rerr != nil {
			return nil, "", rerr
		}
		return existing, replay, nil
	}
	if perr := g.svcCtx.Leases.PutTicket(ctx, row.TicketHash, row.TicketId, p.TTLSeconds); perr != nil {
		// Redis 有效位写不进去 = 这张票换不了租约。留着 DB 里的 ISSUED 行只会让运营以为发出去了，
		// 所以显式失败；已插入的行由撤销/清扫路径收敛（不做「静默成功」）。
		return nil, "", fmt.Errorf("live-gateway: ticket validity bit unavailable: %w", perr)
	}
	return row, ticket, nil
}

// rebuildTicket 用行内断言重算票据原文（仅用于 request_id 幂等回放）。
//
// 回放前必须自证一致性：重算串的 sha256 要等于行里的 ticket_hash。
// 不相等只可能是「这张票是另一把密钥/另一个环境签的」（例如迁移时换了 TicketSignKeyRef），
// 此时造一个能用的新串发给调用方，等于用新密钥复活旧凭据 —— 宁可报「无法回放」。
func (g gate) rebuildTicket(row model.LiveGwReconnectTicket) (string, error) {
	rebuilt, err := g.svcCtx.Signer.Sign(ticketClaimsOf(&row))
	if err != nil {
		return "", err
	}
	if row.TicketHash != "" && model.TicketHash(rebuilt) != row.TicketHash {
		return "", fmt.Errorf("%w: ticket %s was signed by a different key, replay refused",
			model.ErrTicketNotFound, safeIdent(row.TicketId))
	}
	return rebuilt, nil
}

// ticketRequest 是 issueTicket 的入参（字段与 live_gw_reconnect_ticket 一一对应）。
type ticketRequest struct {
	RoomID     int64
	Mid        int64
	ConnID     string
	NodeID     string
	Role       int32
	TTLSeconds int
	RequestID  string
	LeaseID    string
	Reason     string
	TraceID    string
}

// strInt 把可能为负的计数安全转成 int32（Redis 计数在越界时夹住而不是回绕）。
func strInt(v int64) int32 {
	if v > int64(^uint32(0)>>1) {
		return int32(^uint32(0) >> 1)
	}
	if v < -int64(^uint32(0)>>1) {
		return -int32(^uint32(0) >> 1)
	}
	return int32(v)
}
