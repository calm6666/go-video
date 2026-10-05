package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/redis"

	"go-video/services/live-gateway/model"
)

// LeaseRecord 是 Redis 里的一条连接租约（在线态唯一事实源，README 数据分层）。
//
// 为什么整条存 JSON 而不是 hash：租约的字段总是成批读写（判定三元组要同时看 mid/room/state/expire），
// 拆成 hash 字段只会让每次判定变成多次 HGET，且没有原子性收益。
// 心跳计数与 max_seq 也在同一份记录里：并发心跳重传理论上会丢一次观测计数，
// 但心跳是 QoE 观测、不参与任何权限判定，为此引入 Lua 反而让「读一次、判一次、写一次」的
// 语义变得不可单测（本仓库不允许连真实 Redis 跑测试）。
type LeaseRecord struct {
	LeaseID         string   `json:"lease_id"`
	ConnID          string   `json:"conn_id"`
	NodeID          string   `json:"node_id"`
	RoomID          int64    `json:"room_id"`
	Mid             int64    `json:"mid"`
	Role            int32    `json:"role"`
	State           int32    `json:"state"`
	IssuedAt        int64    `json:"issued_at"`
	ExpireAt        int64    `json:"expire_at"`
	RenewCount      int64    `json:"renew_count"`
	LastHeartbeatAt int64    `json:"last_heartbeat_at"`
	HeartbeatCount  int64    `json:"heartbeat_count"`
	MaxSeq          int32    `json:"max_seq"`
	Topics          []string `json:"topics,omitempty"`
	JoinedAt        int64    `json:"joined_at,omitempty"`
	ReconnectTicket string   `json:"reconnect_ticket,omitempty"`
	TicketID        string   `json:"ticket_id,omitempty"`
	DeviceIDHash    string   `json:"device_id_hash,omitempty"`
	Platform        int32    `json:"platform,omitempty"`
	AppVersion      string   `json:"app_version,omitempty"`
	RequestID       string   `json:"request_id,omitempty"`
	TraceID         string   `json:"trace_id,omitempty"`
}

// CloneLease 浅拷贝（切片单独复制）：logic 判定完要回写记录，直接改指针会把读到的旧值一起改掉。
func CloneLease(in *LeaseRecord) *LeaseRecord {
	if in == nil {
		return nil
	}
	out := *in
	if in.Topics != nil {
		out.Topics = append([]string(nil), in.Topics...)
	}
	return &out
}

// EffectiveState 按当前时刻给出租约的真实状态：记录里存的 ACTIVE 可能已经跨过 expire_at。
// Redis 键在过期后还会多留一个「断线宽限期」，就是为了区分 EXPIRED（可复活）与彻底不存在（须重连）。
func (r *LeaseRecord) EffectiveState(now int64) int32 {
	if r == nil {
		return model.LeaseStateUnspecified
	}
	if r.State == model.LeaseStateReleased || r.State == model.LeaseStateKicked {
		return r.State
	}
	if model.Expired(r.ExpireAt, now) {
		return model.LeaseStateExpired
	}
	return model.LeaseStateActive
}

// RemainingTTL 剩余有效秒数（下限 0），供 LeaseInfo.ttl_seconds 回显。
func (r *LeaseRecord) RemainingTTL(now int64) int32 {
	if r == nil || r.ExpireAt <= now {
		return 0
	}
	return int32(r.ExpireAt - now)
}

// storeTTL 计算这条记录在 Redis 里应存多少秒：剩余有效期 + 断线宽限期（可续租/可复用订阅视图）。
func (r *LeaseRecord) storeTTL(graceSeconds int) int {
	ttl := int(r.RemainingTTL(model.NowUnix())) + graceSeconds
	if ttl < 1 {
		ttl = 1
	}
	return ttl
}

// LeaseStore 长连接在线态存储。唯一实现是 Redis（redisLeaseStore）；
// 接口存在的意义是让 logic 的鉴权/配额/审计路径可单测（禁止连真实 Redis 跑测试）。
type LeaseStore interface {
	// Acquire 以 (node_id, conn_id) 为幂等键签发租约。
	// 同键已有租约时按「同连接重连」语义把旧租约置 RELEASED 再签新租约，不留双活租约。
	Acquire(ctx context.Context, rec *LeaseRecord, ttlSeconds int) (*LeaseRecord, error)
	// Get 读租约；不存在返回 (nil, nil)（Redis 键已回收 = 客户端必须重新 Acquire）。
	Get(ctx context.Context, leaseID string) (*LeaseRecord, error)
	// Put 覆盖写回（续租、心跳、订阅变更），TTL 按记录自身的有效期 + 宽限期重算。
	Put(ctx context.Context, rec *LeaseRecord) error
	// Delete 彻底删除租约键并把它从房间/用户集合里摘掉（释放、踢下线）。
	Delete(ctx context.Context, rec *LeaseRecord) error

	// ListRoomLeases 房间内的租约快照，最多扫 limit 条；
	// scanned 是被扫描的集合成员数（可能大于返回条数：陈旧成员会被顺手清理）。
	// truncated=true 表示集合还没扫完就到 limit，调用方必须据此决定是否报错而不是当成全量。
	ListRoomLeases(ctx context.Context, roomID int64, limit int32) (leases []*LeaseRecord, scanned int32, truncated bool, err error)
	// ListUserLeases 该用户在该房间的租约（多端同时在线）。
	ListUserLeases(ctx context.Context, roomID, mid int64, limit int32) ([]*LeaseRecord, error)
	// RoomConnectionCount 房间当前**有效**连接数（集合减去已过期成员，不近似）。
	RoomConnectionCount(ctx context.Context, roomID int64, limit int32) (int32, error)

	// Subscribe 登记订阅关系（topics 已由 logic 归一）并把连接计入房间/用户集合。
	Subscribe(ctx context.Context, leaseID string, roomID, mid int64, topics []string, ttlSeconds int) error
	// Unsubscribe topics 为空表示退订并移出房间；非空只收窄订阅集合。返回更新后的 topics。
	Unsubscribe(ctx context.Context, leaseID string, roomID, mid int64, topics []string) ([]string, error)

	// ClaimMessage 占用广播去重窗口：fresh=true 表示本次是首次受理；
	// fresh=false 时 outcome 是首次受理的结论串（供回放 duplicated）。
	ClaimMessage(ctx context.Context, roomID int64, messageID string, ttlSeconds int) (outcome string, fresh bool, err error)
	// RememberMessage 回填本条消息的受理结论。
	RememberMessage(ctx context.Context, roomID int64, messageID, outcome string, ttlSeconds int) error
	// ClaimEvent / RememberEvent 系统事件的去重（键是 (room_id, event_id)）。
	ClaimEvent(ctx context.Context, roomID int64, eventID string, ttlSeconds int) (string, bool, error)
	RememberEvent(ctx context.Context, roomID int64, eventID, outcome string, ttlSeconds int) error
	// ClaimRequest / RememberRequest 写接口 request_id 幂等窗口（返回首次结果标识）。
	ClaimRequest(ctx context.Context, rpcName, requestID string, ttlSeconds int) (string, bool, error)
	RememberRequest(ctx context.Context, rpcName, requestID, value string, ttlSeconds int) error

	// AllowRate 固定窗口限流：label 是「维度:标识」形式的调用方自拼标签，
	// 返回是否放行与窗口内剩余量。limit<=0 表示不限制（配额语义里的 0 = 继承/无上限）。
	AllowRate(ctx context.Context, label string, limit int32, windowSeconds int) (allowed bool, remaining int32, err error)

	// SetBan 写禁止重连窗口；perRoom=false 时按用户维度（跨房间生效）。
	SetBan(ctx context.Context, roomID, mid, until int64, perRoom bool) error
	// BanUntil 读取禁止重连时刻：先看房间维度，再看用户维度，取更晚的一个。
	BanUntil(ctx context.Context, roomID, mid int64) (int64, error)

	// PutTicket 写票据有效位（键是 ticket_hash，值是 ticket_id）。
	PutTicket(ctx context.Context, ticketHash, ticketID string, ttlSeconds int) error
	// ConsumeTicket 原子取走并删除有效位（一次性消费的快路径）。
	// found=false 表示有效位不存在（已消费 / 已过期 / 已被撤销）。
	ConsumeTicket(ctx context.Context, ticketHash string) (ticketID string, found bool, err error)
	// DropTicket 撤销时删除有效位（不能只改 DB，否则 Redis 快路径还会放行兑换）。
	DropTicket(ctx context.Context, ticketHash string) error

	// BumpRoomSeq 房间已受理广播序号 +1 并返回新值（断线期间漏消息估算的分子）。
	BumpRoomSeq(ctx context.Context, roomID int64) (int64, error)
	// MarkOffline 记录该 (room, mid) 的断开时刻与当时序号，保留 grace 秒供重连回显。
	MarkOffline(ctx context.Context, roomID, mid, at, seq int64, graceSeconds int) error
	// OfflineView 读断线视图；不存在返回 (0, 0, nil)，绝不伪造历史。
	OfflineView(ctx context.Context, roomID, mid int64) (lastOfflineAt, missedEstimate int64, err error)

	// CacheGet / CacheSet / CacheDel 通用 JSON 读缓存（路由与配额用它，键由本包构造）。
	CacheGet(ctx context.Context, key string, into any) (bool, error)
	CacheSet(ctx context.Context, key string, value any, ttlSeconds int) error
	CacheDel(ctx context.Context, keys ...string) error

	// QuotaEpoch 读配额代次（0 表示从未有过变更）。
	QuotaEpoch(ctx context.Context) (int32, error)
	// BumpQuotaEpoch 递增配额代次并返回新值：UpsertAccessQuota 成功后必须调用，
	// 否则「降配封禁」要等满 QuotaCacheTTLSeconds 才生效。
	BumpQuotaEpoch(ctx context.Context) (int32, error)
}

// redisLeaseStore 是 LeaseStore 的 Redis 实现。
type redisLeaseStore struct {
	rds   *redis.Redis
	grace int // ReconnectGraceSeconds：租约键在 expire 后多留的秒数（订阅视图/宽限续租）
	// 去重与限流窗口（秒），来自配置，不在代码里写死。
	dedupWindow int
	rateWindow  int
}

// NewRedisLeaseStore 构造 Redis 实现。rds 为 nil 时返回错误：
// Redis 是本服务的硬依赖（在线态真值），不允许带着 nil store 对外服务。
func NewRedisLeaseStore(rds *redis.Redis, graceSeconds, dedupWindowSeconds, rateWindowSeconds int) (LeaseStore, error) {
	if rds == nil {
		return nil, fmt.Errorf("%w: CacheRedis is required for connection lease", ErrStoreUnavailable)
	}
	if graceSeconds < 0 {
		graceSeconds = 0
	}
	if dedupWindowSeconds <= 0 {
		dedupWindowSeconds = defaultDedupWindowSeconds
	}
	if rateWindowSeconds <= 0 {
		rateWindowSeconds = defaultRateWindowSeconds
	}
	return &redisLeaseStore{rds: rds, grace: graceSeconds, dedupWindow: dedupWindowSeconds, rateWindow: rateWindowSeconds}, nil
}

// Ping 依赖探活（svc 启动诊断与降级判定用）。
func (s *redisLeaseStore) Ping(ctx context.Context) error {
	if !s.rds.PingCtx(ctx) {
		return fmt.Errorf("%w: redis ping failed", ErrStoreUnavailable)
	}
	return nil
}

func wrapStoreErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, redis.Nil) {
		return nil
	}
	return fmt.Errorf("live-gateway/lease %s: %w", op, err)
}

func (s *redisLeaseStore) Acquire(ctx context.Context, rec *LeaseRecord, ttlSeconds int) (*LeaseRecord, error) {
	if rec == nil || rec.LeaseID == "" {
		return nil, fmt.Errorf("%w: lease_id is required to store a lease", ErrStoreUnavailable)
	}
	if err := checkKeyPart("conn_id", rec.ConnID); err != nil {
		return nil, err
	}
	if err := checkKeyPart("node_id", rec.NodeID); err != nil {
		return nil, err
	}
	connKey := fmt.Sprintf(keyConnLease, rec.NodeID, rec.ConnID)
	// SETNX 抢到 conn 键 = 本连接当前归本次签发所有；抢不到就是同连接重连。
	ok, err := s.rds.SetnxCtx(ctx, connKey, rec.LeaseID)
	if err != nil {
		return nil, wrapStoreErr("Acquire setnx", err)
	}
	if !ok {
		prevID, gerr := s.rds.GetCtx(ctx, connKey)
		if gerr != nil && !errors.Is(gerr, redis.Nil) {
			return nil, wrapStoreErr("Acquire get prev", gerr)
		}
		if prevID != "" && prevID != rec.LeaseID {
			prev, perr := s.Get(ctx, prevID)
			if perr != nil {
				return nil, perr
			}
			if prev != nil && !model.IsLeaseTerminal(prev.State) {
				// 旧租约标 RELEASED 并清订阅/计数：留着双活租约会让她人的 Renew 与心跳互相覆盖。
				prev.State = model.LeaseStateReleased
				if perr := s.Delete(ctx, prev); perr != nil {
					return nil, perr
				}
			}
		}
		if err := s.rds.SetCtx(ctx, connKey, rec.LeaseID); err != nil {
			return nil, wrapStoreErr("Acquire set", err)
		}
	}
	rec.State = model.LeaseStateActive
	if rec.IssuedAt == 0 {
		rec.IssuedAt = model.NowUnix()
	}
	if rec.ExpireAt == 0 && ttlSeconds > 0 {
		rec.ExpireAt = rec.IssuedAt + int64(ttlSeconds)
	}
	if err := s.Put(ctx, rec); err != nil {
		return nil, err
	}
	// 房间/用户集合是「连接索引」：RoomConnectionCount 与 ListRoomConnections 都以它为口径，
	// 所以必须在签发时就登记，而不是等 JoinRoom —— 否则配额判定看到的连接数永远少一条，
	// 一个刚 Acquire 完还没 Join 的连接能绕过 MaxConnections（README 数据分层的配额约束）。
	// topics 仍只由 Subscribe/Unsubscribe 维护：那是「订阅子通道」，与「在不在房间里」是两件事。
	if rec.RoomID > 0 {
		if _, err := s.rds.SaddCtx(ctx, roomLeaseKey(rec.RoomID), rec.LeaseID); err != nil {
			return nil, wrapStoreErr("Acquire room set", err)
		}
		if rec.Mid > 0 {
			if _, err := s.rds.SaddCtx(ctx, userLeaseKey(rec.RoomID, rec.Mid), rec.LeaseID); err != nil {
				return nil, wrapStoreErr("Acquire user set", err)
			}
		}
	}
	if err := s.rds.ExpireCtx(ctx, connKey, rec.storeTTL(s.grace)); err != nil {
		return nil, wrapStoreErr("Acquire conn ttl", err)
	}
	return CloneLease(rec), nil
}

func (s *redisLeaseStore) Get(ctx context.Context, leaseID string) (*LeaseRecord, error) {
	if strings.TrimSpace(leaseID) == "" {
		return nil, model.ErrEmptyLeaseID
	}
	raw, err := s.rds.GetCtx(ctx, fmt.Sprintf(keyLease, leaseID))
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, wrapStoreErr("Get", err)
	}
	if raw == "" {
		return nil, nil
	}
	var rec LeaseRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		// 脏数据不静默当成无租约：删掉它并报错，让运维能看到「键在但读不出」。
		_, _ = s.rds.DelCtx(ctx, fmt.Sprintf(keyLease, leaseID))
		return nil, fmt.Errorf("%w: lease %s payload corrupt: %v", ErrStoreUnavailable, leaseID, err)
	}
	if rec.LeaseID == "" {
		rec.LeaseID = leaseID
	}
	return &rec, nil
}

func (s *redisLeaseStore) Put(ctx context.Context, rec *LeaseRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("live-gateway/lease Put: %w", err)
	}
	if err := s.rds.SetexCtx(ctx, fmt.Sprintf(keyLease, rec.LeaseID), string(raw), rec.storeTTL(s.grace)); err != nil {
		return wrapStoreErr("Put", err)
	}
	// conn 键与租约同生死：不刷新 TTL 会出现「租约还在、conn 映射没了」，
	// 下次 Acquire 就抢不到幂等键而留下双活租约。
	if rec.ConnID != "" && rec.NodeID != "" {
		connKey := fmt.Sprintf(keyConnLease, rec.NodeID, rec.ConnID)
		if cur, gerr := s.rds.GetCtx(ctx, connKey); gerr == nil && cur == rec.LeaseID {
			_ = s.rds.ExpireCtx(ctx, connKey, rec.storeTTL(s.grace))
		}
	}
	return nil
}

func (s *redisLeaseStore) Delete(ctx context.Context, rec *LeaseRecord) error {
	if rec == nil {
		return nil
	}
	if _, err := s.rds.DelCtx(ctx, fmt.Sprintf(keyLease, rec.LeaseID)); err != nil {
		return wrapStoreErr("Delete", err)
	}
	if rec.NodeID != "" && rec.ConnID != "" {
		if cur, gerr := s.rds.GetCtx(ctx, fmt.Sprintf(keyConnLease, rec.NodeID, rec.ConnID)); gerr == nil &&
			cur == rec.LeaseID {
			_, _ = s.rds.DelCtx(ctx, fmt.Sprintf(keyConnLease, rec.NodeID, rec.ConnID))
		}
	}
	if _, err := s.rds.SremCtx(ctx, roomLeaseKey(rec.RoomID), rec.LeaseID); err != nil {
		return wrapStoreErr("Delete room set", err)
	}
	_, _ = s.rds.SremCtx(ctx, userLeaseKey(rec.RoomID, rec.Mid), rec.LeaseID)
	// 订阅视图按 (room, mid) 保留一个宽限期，RedeemReconnectTicket 期内可复用（README §Release 第 5 步）。
	if rec.Mid > 0 {
		seq, _ := s.currentRoomSeq(ctx, rec.RoomID)
		if err := s.MarkOffline(ctx, rec.RoomID, rec.Mid, model.NowUnix(), seq, s.grace); err != nil {
			return err
		}
	}
	return nil
}

func roomLeaseKey(roomID int64) string { return fmt.Sprintf(keyRoomLeases, roomID) }

func userLeaseKey(roomID, mid int64) string { return fmt.Sprintf(keyRoomUser, roomID, mid) }

// leaseIDs 取集合成员并顺手清理租约已消失的陈旧成员（Redis set 没有元素级 TTL）。
func (s *redisLeaseStore) leaseIDs(ctx context.Context, setKey string, limit int32) ([]string, int32, bool, error) {
	if limit <= 0 {
		limit = 500
	}
	var (
		cursor  uint64
		found   []string
		scanned int32
	)
	for {
		members, next, err := s.rds.SscanCtx(ctx, setKey, cursor, "*", int64(limit)+1)
		if err != nil {
			return nil, 0, false, wrapStoreErr("Sscan "+setKey, err)
		}
		for _, m := range members {
			scanned++
			exists, eerr := s.rds.ExistsCtx(ctx, fmt.Sprintf(keyLease, m))
			if eerr != nil {
				return nil, scanned, false, wrapStoreErr("Exists", eerr)
			}
			if !exists {
				_, _ = s.rds.SremCtx(ctx, setKey, m)
				continue
			}
			found = append(found, m)
			if int32(len(found)) >= limit {
				return found, scanned, next != 0, nil
			}
		}
		if next == 0 {
			return found, scanned, false, nil
		}
		cursor = next
	}
}

func (s *redisLeaseStore) ListRoomLeases(ctx context.Context, roomID int64, limit int32) ([]*LeaseRecord, int32, bool, error) {
	ids, scanned, truncated, err := s.leaseIDs(ctx, roomLeaseKey(roomID), limit)
	if err != nil {
		return nil, 0, false, err
	}
	leases := make([]*LeaseRecord, 0, len(ids))
	for _, id := range ids {
		rec, gerr := s.Get(ctx, id)
		if gerr != nil {
			return nil, scanned, truncated, gerr
		}
		if rec != nil {
			leases = append(leases, rec)
		}
	}
	return leases, scanned, truncated, nil
}

func (s *redisLeaseStore) ListUserLeases(ctx context.Context, roomID, mid int64, limit int32) ([]*LeaseRecord, error) {
	ids, _, _, err := s.leaseIDs(ctx, userLeaseKey(roomID, mid), limit)
	if err != nil {
		return nil, err
	}
	out := make([]*LeaseRecord, 0, len(ids))
	for _, id := range ids {
		rec, gerr := s.Get(ctx, id)
		if gerr != nil {
			return nil, gerr
		}
		if rec != nil && rec.RoomID == roomID && rec.Mid == mid {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (s *redisLeaseStore) RoomConnectionCount(ctx context.Context, roomID int64, limit int32) (int32, error) {
	ids, _, _, err := s.leaseIDs(ctx, roomLeaseKey(roomID), limit)
	if err != nil {
		return 0, err
	}
	now := model.NowUnix()
	var active int32
	for _, id := range ids {
		rec, gerr := s.Get(ctx, id)
		if gerr != nil {
			return 0, gerr
		}
		if rec != nil && rec.EffectiveState(now) == model.LeaseStateActive {
			active++
		}
	}
	return active, nil
}

func (s *redisLeaseStore) Subscribe(ctx context.Context, leaseID string, roomID, mid int64,
	topics []string, ttlSeconds int) error {
	rec, err := s.Get(ctx, leaseID)
	if err != nil {
		return err
	}
	if rec == nil {
		return model.ErrLeaseNotFound
	}
	rec.Topics = append([]string(nil), topics...)
	if rec.JoinedAt == 0 {
		rec.JoinedAt = model.NowUnix()
	}
	if ttlSeconds > 0 && rec.ExpireAt < model.NowUnix()+int64(ttlSeconds) {
		rec.ExpireAt = model.NowUnix() + int64(ttlSeconds)
	}
	if err := s.Put(ctx, rec); err != nil {
		return err
	}
	if _, err := s.rds.SaddCtx(ctx, roomLeaseKey(roomID), leaseID); err != nil {
		return wrapStoreErr("Subscribe room set", err)
	}
	if mid > 0 {
		if _, err := s.rds.SaddCtx(ctx, userLeaseKey(roomID, mid), leaseID); err != nil {
			return wrapStoreErr("Subscribe user set", err)
		}
	}
	return nil
}

func (s *redisLeaseStore) Unsubscribe(ctx context.Context, leaseID string, roomID, mid int64,
	topics []string) ([]string, error) {
	rec, err := s.Get(ctx, leaseID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		// 订阅关系不存在：退订重放必须无副作用（LeaveRoom 第 4 步），按成功返回。
		return nil, nil
	}
	if len(topics) == 0 {
		if _, err := s.rds.SremCtx(ctx, roomLeaseKey(roomID), leaseID); err != nil {
			return nil, wrapStoreErr("Unsubscribe room set", err)
		}
		_, _ = s.rds.SremCtx(ctx, userLeaseKey(roomID, mid), leaseID)
		rec.Topics = nil
		return nil, s.Put(ctx, rec)
	}
	drop := make(map[string]struct{}, len(topics))
	for _, t := range topics {
		drop[t] = struct{}{}
	}
	kept := make([]string, 0, len(rec.Topics))
	for _, t := range rec.Topics {
		if _, hit := drop[t]; hit {
			continue
		}
		kept = append(kept, t)
	}
	rec.Topics = kept
	return kept, s.Put(ctx, rec)
}

// --- 去重与幂等窗口 ---

func (s *redisLeaseStore) claimOnce(ctx context.Context, key string, ttl int) (string, bool, error) {
	ok, err := s.rds.SetnxCtx(ctx, key, outcomePending)
	if err != nil {
		return "", false, wrapStoreErr("claim "+key, err)
	}
	if ok {
		_ = s.rds.ExpireCtx(ctx, key, ttl)
		return "", true, nil
	}
	prev, gerr := s.rds.GetCtx(ctx, key)
	if gerr != nil && !errors.Is(gerr, redis.Nil) {
		return "", false, wrapStoreErr("claim get "+key, gerr)
	}
	if prev == "" {
		// 抢到锁的那个请求崩在回填之前：键已被别人读到空值，重新占位继续走，
		// 否则这条 message_id 会永久卡在 PENDING。
		if err := s.rds.SetCtx(ctx, key, outcomePending); err != nil {
			return "", false, wrapStoreErr("claim reset "+key, err)
		}
		_ = s.rds.ExpireCtx(ctx, key, ttl)
		return "", true, nil
	}
	return prev, false, nil
}

func (s *redisLeaseStore) rememberOnce(ctx context.Context, key, value string, ttl int) error {
	if value == "" {
		value = outcomePending
	}
	if err := s.rds.SetexCtx(ctx, key, value, ttl); err != nil {
		return wrapStoreErr("remember "+key, err)
	}
	return nil
}

const (
	// outcomePending 是「已抢键、结论未回填」的占位值，回放时视为无结论。
	outcomePending = "pending"
)

func (s *redisLeaseStore) ClaimMessage(ctx context.Context, roomID int64, messageID string, ttl int) (string, bool, error) {
	if err := checkKeyPart("message_id", messageID); err != nil {
		return "", false, err
	}
	if ttl <= 0 {
		ttl = s.dedupWindow
	}
	return s.claimOnce(ctx, fmt.Sprintf(keyMsgDedup, roomID, messageID), ttl)
}

func (s *redisLeaseStore) RememberMessage(ctx context.Context, roomID int64, messageID, outcome string, ttl int) error {
	if err := checkKeyPart("message_id", messageID); err != nil {
		return err
	}
	if ttl <= 0 {
		ttl = s.dedupWindow
	}
	return s.rememberOnce(ctx, fmt.Sprintf(keyMsgDedup, roomID, messageID), outcome, ttl)
}

func (s *redisLeaseStore) ClaimEvent(ctx context.Context, roomID int64, eventID string, ttl int) (string, bool, error) {
	if err := checkKeyPart("event_id", eventID); err != nil {
		return "", false, err
	}
	if ttl <= 0 {
		ttl = s.dedupWindow
	}
	return s.claimOnce(ctx, fmt.Sprintf(keyEventDedup, roomID, eventID), ttl)
}

func (s *redisLeaseStore) RememberEvent(ctx context.Context, roomID int64, eventID, outcome string, ttl int) error {
	if err := checkKeyPart("event_id", eventID); err != nil {
		return err
	}
	if ttl <= 0 {
		ttl = s.dedupWindow
	}
	return s.rememberOnce(ctx, fmt.Sprintf(keyEventDedup, roomID, eventID), outcome, ttl)
}

func (s *redisLeaseStore) ClaimRequest(ctx context.Context, rpcName, requestID string, ttl int) (string, bool, error) {
	if err := checkKeyPart("request_id", requestID); err != nil {
		return "", false, err
	}
	if ttl <= 0 {
		ttl = s.dedupWindow
	}
	return s.claimOnce(ctx, fmt.Sprintf(keyRequest, rpcName, requestID), ttl)
}

func (s *redisLeaseStore) RememberRequest(ctx context.Context, rpcName, requestID, value string, ttl int) error {
	if err := checkKeyPart("request_id", requestID); err != nil {
		return err
	}
	if ttl <= 0 {
		ttl = s.dedupWindow
	}
	return s.rememberOnce(ctx, fmt.Sprintf(keyRequest, rpcName, requestID), value, ttl)
}

// --- 限流 ---

func (s *redisLeaseStore) AllowRate(ctx context.Context, label string, limit int32, windowSeconds int) (bool, int32, error) {
	if limit <= 0 {
		// 0/负数是「继承 = 未设上限」的配额语义（README：0 表示继承，不是关闭）。
		return true, limit, nil
	}
	if windowSeconds <= 0 {
		windowSeconds = s.rateWindow
	}
	bucket := model.NowUnix() / int64(windowSeconds)
	key := fmt.Sprintf(keyRate, label, bucket)
	n, err := s.rds.IncrCtx(ctx, key)
	if err != nil {
		return false, 0, wrapStoreErr("rate "+label, err)
	}
	if n == 1 {
		// TTL 只要覆盖窗口即可；窗口已编码在键名里，TTL 只用于回收。
		_ = s.rds.ExpireCtx(ctx, key, windowSeconds*2)
	}
	if n > int64(limit) {
		return false, 0, nil
	}
	return true, int32(int64(limit) - n), nil
}

// --- 禁止重连窗口 ---

func banKey(roomID, mid int64, perRoom bool) string {
	if perRoom {
		return fmt.Sprintf(keyBanRoomUser, roomID, mid)
	}
	return fmt.Sprintf(keyBanUser, mid)
}

func (s *redisLeaseStore) SetBan(ctx context.Context, roomID, mid, until int64, perRoom bool) error {
	if mid <= 0 {
		// 游客（mid=0）的封禁键会退化成「该房间所有游客」共用一个键，影响面不可控。
		return model.ErrInvalidMid
	}
	if until <= 0 {
		return nil
	}
	ttl := int(until - model.NowUnix())
	if ttl <= 0 {
		return nil
	}
	key := banKey(roomID, mid, perRoom)
	if err := s.rds.SetexCtx(ctx, key, strconv.FormatInt(until, 10), ttl); err != nil {
		return wrapStoreErr("SetBan", err)
	}
	return nil
}

func (s *redisLeaseStore) BanUntil(ctx context.Context, roomID, mid int64) (int64, error) {
	if mid <= 0 {
		return 0, nil
	}
	var latest int64
	for _, key := range []string{banKey(roomID, mid, true), banKey(roomID, mid, false)} {
		raw, err := s.rds.GetCtx(ctx, key)
		if err != nil {
			if errors.Is(err, redis.Nil) {
				continue
			}
			return 0, wrapStoreErr("BanUntil", err)
		}
		if raw == "" {
			continue
		}
		v, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil {
			continue
		}
		if v > latest {
			latest = v
		}
	}
	return latest, nil
}

// --- 票据有效位 ---

func (s *redisLeaseStore) PutTicket(ctx context.Context, ticketHash, ticketID string, ttlSeconds int) error {
	if err := checkKeyPart("ticket_hash", ticketHash); err != nil {
		return err
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 1
	}
	if err := s.rds.SetexCtx(ctx, fmt.Sprintf(keyTicket, ticketHash), ticketID, ttlSeconds); err != nil {
		return wrapStoreErr("PutTicket", err)
	}
	return nil
}

func (s *redisLeaseStore) ConsumeTicket(ctx context.Context, ticketHash string) (string, bool, error) {
	if err := checkKeyPart("ticket_hash", ticketHash); err != nil {
		return "", false, err
	}
	id, err := s.rds.GetDelCtx(ctx, fmt.Sprintf(keyTicket, ticketHash))
	if err != nil {
		return "", false, wrapStoreErr("ConsumeTicket", err)
	}
	if id == "" {
		return "", false, nil
	}
	return id, true, nil
}

func (s *redisLeaseStore) DropTicket(ctx context.Context, ticketHash string) error {
	if strings.TrimSpace(ticketHash) == "" {
		return nil
	}
	if _, err := s.rds.DelCtx(ctx, fmt.Sprintf(keyTicket, ticketHash)); err != nil {
		return wrapStoreErr("DropTicket", err)
	}
	return nil
}

// --- 断线视图与漏消息估算 ---

func (s *redisLeaseStore) BumpRoomSeq(ctx context.Context, roomID int64) (int64, error) {
	n, err := s.rds.IncrCtx(ctx, fmt.Sprintf(keyRoomSeq, roomID))
	if err != nil {
		return 0, wrapStoreErr("BumpRoomSeq", err)
	}
	// 序号键只要活着就够；每次受理都续一次，房间长期无人时自然回收。
	_ = s.rds.ExpireCtx(ctx, fmt.Sprintf(keyRoomSeq, roomID), s.dedupWindow*6)
	return n, nil
}

func (s *redisLeaseStore) currentRoomSeq(ctx context.Context, roomID int64) (int64, error) {
	raw, err := s.rds.GetCtx(ctx, fmt.Sprintf(keyRoomSeq, roomID))
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, wrapStoreErr("currentRoomSeq", err)
	}
	if raw == "" {
		return 0, nil
	}
	return strconv.ParseInt(raw, 10, 64)
}

func (s *redisLeaseStore) MarkOffline(ctx context.Context, roomID, mid, at, seq int64, graceSeconds int) error {
	if mid <= 0 {
		return nil // 游客没有可复用的订阅视图，不写这个键
	}
	if graceSeconds <= 0 {
		return nil
	}
	if at == 0 {
		at = model.NowUnix()
	}
	val := strconv.FormatInt(at, 10) + "|" + strconv.FormatInt(seq, 10)
	if err := s.rds.SetexCtx(ctx, fmt.Sprintf(keyOfflineView, roomID, mid), val, graceSeconds); err != nil {
		return wrapStoreErr("MarkOffline", err)
	}
	return nil
}

func (s *redisLeaseStore) OfflineView(ctx context.Context, roomID, mid int64) (int64, int64, error) {
	if mid <= 0 {
		return 0, 0, nil
	}
	raw, err := s.rds.GetCtx(ctx, fmt.Sprintf(keyOfflineView, roomID, mid))
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, 0, nil
		}
		return 0, 0, wrapStoreErr("OfflineView", err)
	}
	if raw == "" {
		return 0, 0, nil
	}
	parts := strings.SplitN(raw, "|", 2)
	at, aerr := strconv.ParseInt(parts[0], 10, 64)
	if aerr != nil {
		return 0, 0, nil
	}
	var seq int64
	if len(parts) == 2 {
		seq, _ = strconv.ParseInt(parts[1], 10, 64)
	}
	cur, cerr := s.currentRoomSeq(ctx, roomID)
	if cerr != nil {
		return at, 0, nil // 估算口径可缺，不因此让重连失败
	}
	missed := cur - seq
	if missed < 0 {
		missed = 0
	}
	return at, missed, nil
}

// --- 通用 JSON 读缓存 ---

func (s *redisLeaseStore) CacheGet(ctx context.Context, key string, into any) (bool, error) {
	raw, err := s.rds.GetCtx(ctx, key)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return false, nil
		}
		return false, wrapStoreErr("CacheGet "+key, err)
	}
	if raw == "" {
		return false, nil
	}
	if err := json.Unmarshal([]byte(raw), into); err != nil {
		_, _ = s.rds.DelCtx(ctx, key)
		return false, nil // 脏缓存当未命中回源，不阻断请求
	}
	return true, nil
}

func (s *redisLeaseStore) CacheSet(ctx context.Context, key string, value any, ttlSeconds int) error {
	if ttlSeconds <= 0 {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("live-gateway/lease CacheSet: %w", err)
	}
	return wrapStoreErr("CacheSet "+key, s.rds.SetexCtx(ctx, key, string(raw), ttlSeconds))
}

func (s *redisLeaseStore) CacheDel(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	_, err := s.rds.DelCtx(ctx, keys...)
	return wrapStoreErr("CacheDel", err)
}

func (s *redisLeaseStore) QuotaEpoch(ctx context.Context) (int32, error) {
	raw, err := s.rds.GetCtx(ctx, keyQuotaEpoch)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, wrapStoreErr("QuotaEpoch", err)
	}
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		// 代次被写坏（有人手工改了键）时不能当 0 用：那会让所有旧缓存重新变成「有效」。
		// 显式报错，调用方回源 DB 而不是拿脏缓存。
		return 0, fmt.Errorf("%w: quota epoch %q is not a number", ErrStoreUnavailable, raw)
	}
	return int32(n), nil
}

func (s *redisLeaseStore) BumpQuotaEpoch(ctx context.Context) (int32, error) {
	n, err := s.rds.IncrCtx(ctx, keyQuotaEpoch)
	if err != nil {
		return 0, wrapStoreErr("BumpQuotaEpoch", err)
	}
	return int32(n), nil
}
