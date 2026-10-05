// 本文件是 internal/logic 全部单测共用的内存 fake 与构造脚手架。
//
// 为什么必须自带 fake 而不是连真依赖：
//  1. 本仓库的测试门禁禁止连任何 MySQL / Redis（本机 3306 是维护者实例，绝不触碰）；
//  2. 被测 logic 的契约里有大半是「依赖不可用时必须显式失败」（见 svc.ServiceContext.Notes），
//     真依赖反而造不出这些场景，fake 可以逐方法注入错误。
//
// fake 的语义一律对齐 repository.RedisLeaseStore 与 model 的既有实现（同名方法的契约即注释），
// 包括：Acquire 同连接重连先把旧租约置 RELEASED、去重键的 pending 占位、BanUntil 取房间/用户两键的
// 较晚值、配额继承链自外向内覆盖、UpdateState 的条件 UPDATE 语义。
// 对得不完全就会测出一个现实中不存在的行为，那比不测更糟。
//
// 四个 model 接口的 fake 沿用本仓库风格：内嵌接口再覆写被测方法，
// 未实现的方法落到 nil 接口直接 panic，误用会立刻暴露而不是静默通过。
package logic

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/metadata"

	"go-video/services/live-gateway/internal/config"
	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
)

func TestMain(m *testing.M) {
	// logic 里大量 g.errorf 是「降级但不失败」的观测输出，测试阶段全部静音，
	// 真正的结论一律由断言给出（不看日志）。
	logx.Disable()
	os.Exit(m.Run())
}

// 编译期契约：fake 必须与真依赖的接口逐方法对齐。
// 生产接口一旦新增方法，这里立刻编译失败，而不是让某个用例拿到「nil 接口」的假象。
var (
	_ repository.LeaseStore            = (*fakeLeaseStore)(nil)
	_ repository.RoomGate              = (*fakeRoomGate)(nil)
	_ repository.Transport             = (*fakeTransport)(nil)
	_ model.LiveGwRoomRouteModel       = (*fakeRoomRoutes)(nil)
	_ model.LiveGwAccessQuotaModel     = (*fakeQuotas)(nil)
	_ model.LiveGwBroadcastLogModel    = (*fakeBroadcastLogs)(nil)
	_ model.LiveGwReconnectTicketModel = (*fakeTickets)(nil)
)

// --- 时钟 ---

// fakeClock 注入 model 的唯一时钟源：租约到期、票据有效期、封禁窗口必须同源才能构造边界。
type fakeClock struct {
	mu  sync.Mutex
	cur time.Time
}

// newFakeClock 从 at 起固定时钟，并在测试结束时恢复系统时间。
func newFakeClock(t *testing.T, at time.Time) *fakeClock {
	t.Helper()
	c := &fakeClock{cur: at}
	restore := model.SetClock(c.now)
	t.Cleanup(restore)
	return c
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

func (c *fakeClock) unix() int64 { return c.now().Unix() }

// advance 推进 d 秒并返回新的 Unix 秒（model.NowUnix 的可见值随之改变）。
func (c *fakeClock) advance(d time.Duration) int64 {
	c.mu.Lock()
	c.cur = c.cur.Add(d)
	n := c.cur.Unix()
	c.mu.Unlock()
	return n
}

// --- 上下文与调用方归因 ---

// ctxAs 按 caller.go 的约定注入 x-gw-caller-* metadata（唯一的可信主体通道）。
func ctxAs(t *testing.T, attested, role, mid, service string) context.Context {
	t.Helper()
	pairs := []string{}
	if attested != "" {
		pairs = append(pairs, mdCallerAttested, attested)
	}
	if role != "" {
		pairs = append(pairs, mdCallerRole, role)
	}
	if mid != "" {
		pairs = append(pairs, mdCallerMid, mid)
	}
	if service != "" {
		pairs = append(pairs, mdCallerService, service)
	}
	if len(pairs) == 0 {
		return context.Background()
	}
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(pairs...))
}

// ctxClient 客户端链路：完全没有 metadata（未归因）。
func ctxClient(t *testing.T) context.Context { return ctxAs(t, "", "", "", "") }

// ctxOperator 归因的运营主体，可选带人类 mid。
func ctxOperator(t *testing.T, mid string) context.Context {
	return ctxAs(t, "true", "OPERATOR", mid, "")
}

// ctxService 归因的内部服务主体（service 是 x-gw-caller-service）。
func ctxService(t *testing.T, service string) context.Context {
	return ctxAs(t, "true", "SERVICE", "", service)
}

// --- LeaseStore 内存实现 ---

// fakeLeaseStore 覆盖 repository.LeaseStore 全部 30 个方法，语义对齐 redisLeaseStore。
// 每个方法都支持按方法名注入错误（fail 字段），用于验证「依赖故障必须原样返回，不得降级为假成功」。
type fakeLeaseStore struct {
	mu sync.Mutex

	leases   map[string]*repository.LeaseRecord
	conns    map[string]string   // node|conn -> lease_id
	roomSet  map[int64][]string  // room_id -> lease_ids（有序，便于断言）
	userSet  map[string][]string // room|mid -> lease_ids
	msgs     map[string]string   // room|message_id -> 首次结论
	events   map[string]string   // room|event_id -> 首次结论
	requests map[string]string   // rpc|request_id -> 结论
	rates    map[string]int32    // label -> 窗口内已用量
	bans     map[string]int64    // 封禁键 -> until
	tickets  map[string]string   // ticket_hash -> ticket_id
	seqs     map[int64]int64     // room_id -> 已受理广播序号
	offline  map[string][2]int64 // room|mid -> {断开秒, 当时序号}
	cache    map[string][]byte   // 通用 JSON 读缓存
	epoch    int32

	fails map[string]error
	calls map[string]int
}

func newFakeLeaseStore() *fakeLeaseStore {
	return &fakeLeaseStore{
		leases:   map[string]*repository.LeaseRecord{},
		conns:    map[string]string{},
		roomSet:  map[int64][]string{},
		userSet:  map[string][]string{},
		msgs:     map[string]string{},
		events:   map[string]string{},
		requests: map[string]string{},
		rates:    map[string]int32{},
		bans:     map[string]int64{},
		tickets:  map[string]string{},
		seqs:     map[int64]int64{},
		offline:  map[string][2]int64{},
		cache:    map[string][]byte{},
		fails:    map[string]error{},
		calls:    map[string]int{},
	}
}

// failWith 让第 method 个方法返回 err（err 为 nil 即恢复正常）。
func (f *fakeLeaseStore) failWith(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[method] = err
}

// callCount 统计某方法被调用了几次，用于断言「只读路径零写入」这类结论。
func (f *fakeLeaseStore) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *fakeLeaseStore) guard(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[method]++
	if err, ok := f.fails[method]; ok {
		return err
	}
	return nil
}

func userSetKey(roomID, mid int64) string { return fmt.Sprintf("%d|%d", roomID, mid) }

func banRoomKey(roomID, mid int64) string { return fmt.Sprintf("r|%d|%d", roomID, mid) }

func banUserKey(mid int64) string { return fmt.Sprintf("u|%d", mid) }

func (f *fakeLeaseStore) Acquire(_ context.Context, rec *repository.LeaseRecord, ttlSeconds int) (*repository.LeaseRecord, error) {
	if err := f.guard("Acquire"); err != nil {
		return nil, err
	}
	if rec == nil || rec.LeaseID == "" {
		return nil, fmt.Errorf("%w: lease_id is required to store a lease", repository.ErrStoreUnavailable)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := rec.NodeID + "|" + rec.ConnID
	if prevID, ok := f.conns[key]; ok && prevID != rec.LeaseID {
		// 同连接重连：旧租约先置 RELEASED 再删键，不留双活租约（与 redisLeaseStore.Acquire 同语义）。
		if prev, exists := f.leases[prevID]; exists && !model.IsLeaseTerminal(prev.State) {
			prev.State = model.LeaseStateReleased
			f.deleteLocked(prev)
		}
	}
	f.conns[key] = rec.LeaseID
	cp := repository.CloneLease(rec)
	cp.State = model.LeaseStateActive
	if cp.IssuedAt == 0 {
		cp.IssuedAt = model.NowUnix()
	}
	if cp.ExpireAt == 0 && ttlSeconds > 0 {
		cp.ExpireAt = cp.IssuedAt + int64(ttlSeconds)
	}
	f.leases[cp.LeaseID] = cp
	f.addSetsLocked(cp)
	return repository.CloneLease(cp), nil
}

func (f *fakeLeaseStore) Get(_ context.Context, leaseID string) (*repository.LeaseRecord, error) {
	if err := f.guard("Get"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(leaseID) == "" {
		return nil, model.ErrEmptyLeaseID
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.leases[leaseID]
	if !ok || rec == nil {
		return nil, nil
	}
	return repository.CloneLease(rec), nil
}

func (f *fakeLeaseStore) Put(_ context.Context, rec *repository.LeaseRecord) error {
	if err := f.guard("Put"); err != nil {
		return err
	}
	if rec == nil || rec.LeaseID == "" {
		return fmt.Errorf("%w: lease_id is required to store a lease", repository.ErrStoreUnavailable)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leases[rec.LeaseID] = repository.CloneLease(rec)
	f.addSetsLocked(rec)
	return nil
}

func (f *fakeLeaseStore) Delete(_ context.Context, rec *repository.LeaseRecord) error {
	if err := f.guard("Delete"); err != nil {
		return err
	}
	if rec == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteLocked(rec)
	return nil
}

// deleteLocked 调用方持锁：删 lease 键、删 conn 幂等键、移出房间/用户集合、写断线视图。
func (f *fakeLeaseStore) deleteLocked(rec *repository.LeaseRecord) {
	delete(f.leases, rec.LeaseID)
	if rec.NodeID != "" && rec.ConnID != "" {
		k := rec.NodeID + "|" + rec.ConnID
		if f.conns[k] == rec.LeaseID {
			delete(f.conns, k)
		}
	}
	f.roomSet[rec.RoomID] = removeStr(f.roomSet[rec.RoomID], rec.LeaseID)
	uk := userSetKey(rec.RoomID, rec.Mid)
	f.userSet[uk] = removeStr(f.userSet[uk], rec.LeaseID)
	if rec.Mid > 0 {
		f.offline[uk] = [2]int64{model.NowUnix(), f.seqs[rec.RoomID]}
	}
}

func (f *fakeLeaseStore) addSetsLocked(rec *repository.LeaseRecord) {
	if rec.RoomID <= 0 {
		return
	}
	f.roomSet[rec.RoomID] = addStr(f.roomSet[rec.RoomID], rec.LeaseID)
	if rec.Mid > 0 {
		uk := userSetKey(rec.RoomID, rec.Mid)
		f.userSet[uk] = addStr(f.userSet[uk], rec.LeaseID)
	}
}

func addStr(list []string, v string) []string {
	for _, item := range list {
		if item == v {
			return list
		}
	}
	return append(list, v)
}

func removeStr(list []string, v string) []string {
	out := list[:0]
	for _, item := range list {
		if item != v {
			out = append(out, item)
		}
	}
	return out
}

// listLocked 调用方持锁：按登记的 lease_id 升序返回集合成员对应的记录（陈旧成员顺手清理）。
func (f *fakeLeaseStore) listLocked(ids []string, limit int32) (leases []*repository.LeaseRecord, scanned int32, truncated bool) {
	order := append([]string(nil), ids...)
	sort.Strings(order)
	if limit <= 0 {
		limit = defaultConnectionScanLimit
	}
	for _, id := range order {
		rec, ok := f.leases[id]
		if !ok || rec == nil {
			continue
		}
		scanned++
		if int32(len(leases)) >= limit {
			return leases, scanned, true
		}
		leases = append(leases, repository.CloneLease(rec))
	}
	return leases, scanned, false
}

func (f *fakeLeaseStore) ListRoomLeases(_ context.Context, roomID int64, limit int32) ([]*repository.LeaseRecord, int32, bool, error) {
	if err := f.guard("ListRoomLeases"); err != nil {
		return nil, 0, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	leases, scanned, truncated := f.listLocked(f.roomSet[roomID], limit)
	return leases, scanned, truncated, nil
}

func (f *fakeLeaseStore) ListUserLeases(_ context.Context, roomID, mid int64, limit int32) ([]*repository.LeaseRecord, error) {
	if err := f.guard("ListUserLeases"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	leases, _, _ := f.listLocked(f.userSet[userSetKey(roomID, mid)], limit)
	out := make([]*repository.LeaseRecord, 0, len(leases))
	for _, rec := range leases {
		if rec.RoomID == roomID && rec.Mid == mid {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (f *fakeLeaseStore) RoomConnectionCount(_ context.Context, roomID int64, limit int32) (int32, error) {
	if err := f.guard("RoomConnectionCount"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	leases, _, _ := f.listLocked(f.roomSet[roomID], limit)
	var n int32
	now := model.NowUnix()
	for _, rec := range leases {
		if rec.EffectiveState(now) == model.LeaseStateActive {
			n++
		}
	}
	return n, nil
}

func (f *fakeLeaseStore) Subscribe(_ context.Context, leaseID string, roomID, mid int64, topics []string, ttlSeconds int) error {
	if err := f.guard("Subscribe"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.leases[leaseID]
	if !ok {
		return model.ErrLeaseNotFound
	}
	rec.Topics = append([]string(nil), topics...)
	if rec.JoinedAt == 0 {
		rec.JoinedAt = model.NowUnix()
	}
	if ttlSeconds > 0 {
		if want := model.NowUnix() + int64(ttlSeconds); rec.ExpireAt < want {
			rec.ExpireAt = want
		}
	}
	rec.RoomID = roomID
	f.roomSet[roomID] = addStr(f.roomSet[roomID], leaseID)
	if mid > 0 {
		uk := userSetKey(roomID, mid)
		f.userSet[uk] = addStr(f.userSet[uk], leaseID)
	}
	return nil
}

func (f *fakeLeaseStore) Unsubscribe(_ context.Context, leaseID string, roomID, mid int64, topics []string) ([]string, error) {
	if err := f.guard("Unsubscribe"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.leases[leaseID]
	if !ok {
		return nil, nil // 订阅关系不存在：退订重放必须无副作用
	}
	if len(topics) == 0 {
		f.roomSet[roomID] = removeStr(f.roomSet[roomID], leaseID)
		uk := userSetKey(roomID, mid)
		f.userSet[uk] = removeStr(f.userSet[uk], leaseID)
		rec.Topics = nil
		return nil, nil
	}
	drop := map[string]struct{}{}
	for _, t := range topics {
		drop[t] = struct{}{}
	}
	kept := make([]string, 0, len(rec.Topics))
	for _, t := range rec.Topics {
		if _, hit := drop[t]; !hit {
			kept = append(kept, t)
		}
	}
	rec.Topics = kept
	return kept, nil
}

// claimLocked 调用方持锁：SETNX 语义，抢到返回 ("", true)，否则回既有结论。
func claimLocked(m map[string]string, key string) (string, bool) {
	if v, ok := m[key]; ok {
		return v, false
	}
	m[key] = lgwOutcomePending
	return "", true
}

func (f *fakeLeaseStore) ClaimMessage(_ context.Context, roomID int64, messageID string, _ int) (string, bool, error) {
	if err := f.guard("ClaimMessage"); err != nil {
		return "", false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out, fresh := claimLocked(f.msgs, fmt.Sprintf("%d|%s", roomID, messageID))
	return out, fresh, nil
}

func (f *fakeLeaseStore) RememberMessage(_ context.Context, roomID int64, messageID, outcome string, _ int) error {
	if err := f.guard("RememberMessage"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if outcome == "" {
		outcome = lgwOutcomePending
	}
	f.msgs[fmt.Sprintf("%d|%s", roomID, messageID)] = outcome
	return nil
}

func (f *fakeLeaseStore) ClaimEvent(_ context.Context, roomID int64, eventID string, _ int) (string, bool, error) {
	if err := f.guard("ClaimEvent"); err != nil {
		return "", false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out, fresh := claimLocked(f.events, fmt.Sprintf("%d|%s", roomID, eventID))
	return out, fresh, nil
}

func (f *fakeLeaseStore) RememberEvent(_ context.Context, roomID int64, eventID, outcome string, _ int) error {
	if err := f.guard("RememberEvent"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if outcome == "" {
		outcome = lgwOutcomePending
	}
	f.events[fmt.Sprintf("%d|%s", roomID, eventID)] = outcome
	return nil
}

func (f *fakeLeaseStore) ClaimRequest(_ context.Context, rpcName, requestID string, _ int) (string, bool, error) {
	if err := f.guard("ClaimRequest"); err != nil {
		return "", false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out, fresh := claimLocked(f.requests, rpcName+"|"+requestID)
	return out, fresh, nil
}

func (f *fakeLeaseStore) RememberRequest(_ context.Context, rpcName, requestID, value string, _ int) error {
	if err := f.guard("RememberRequest"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if value == "" {
		value = lgwOutcomePending
	}
	f.requests[rpcName+"|"+requestID] = value
	return nil
}

// seedRequest 预置幂等窗口结论（跳过真实写路径，专测回放分支）。
func (f *fakeLeaseStore) seedRequest(rpcName, requestID, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests[rpcName+"|"+requestID] = value
}

// seedMessage 预置广播去重窗口的既有结论。
func (f *fakeLeaseStore) seedMessage(roomID int64, messageID, outcome string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs[fmt.Sprintf("%d|%s", roomID, messageID)] = outcome
}

// rateCount 读某条限流键在窗口内的累计次数。限流键的拼法（bqps:room:* / dqps:mid:*）
// 本身就是契约：键拼错等于两个维度各限各的、互不相干，必须有断言手段。
func (f *fakeLeaseStore) rateCount(label string) int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rates[label]
}

func (f *fakeLeaseStore) AllowRate(_ context.Context, label string, limit int32, _ int) (bool, int32, error) {
	if err := f.guard("AllowRate"); err != nil {
		return false, 0, err
	}
	if limit <= 0 {
		// 0/负数是「未设上限」的配额语义，不计数。
		return true, limit, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rates[label]++
	n := f.rates[label]
	if n > limit {
		return false, 0, nil
	}
	return true, limit - n, nil
}

func (f *fakeLeaseStore) SetBan(_ context.Context, roomID, mid, until int64, perRoom bool) error {
	if err := f.guard("SetBan"); err != nil {
		return err
	}
	if mid <= 0 {
		return model.ErrInvalidMid
	}
	if until <= 0 {
		return nil
	}
	key := banUserKey(mid)
	if perRoom {
		key = banRoomKey(roomID, mid)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bans[key] = until
	return nil
}

func (f *fakeLeaseStore) BanUntil(_ context.Context, roomID, mid int64) (int64, error) {
	if err := f.guard("BanUntil"); err != nil {
		return 0, err
	}
	if mid <= 0 {
		return 0, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var latest int64
	for _, key := range []string{banRoomKey(roomID, mid), banUserKey(mid)} {
		if v := f.bans[key]; v > latest {
			latest = v
		}
	}
	return latest, nil
}

func (f *fakeLeaseStore) PutTicket(_ context.Context, ticketHash, ticketID string, _ int) error {
	if err := f.guard("PutTicket"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tickets[ticketHash] = ticketID
	return nil
}

func (f *fakeLeaseStore) ConsumeTicket(_ context.Context, ticketHash string) (string, bool, error) {
	if err := f.guard("ConsumeTicket"); err != nil {
		return "", false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.tickets[ticketHash]
	if !ok {
		return "", false, nil
	}
	delete(f.tickets, ticketHash) // GETDEL：一次性消费
	return id, true, nil
}

func (f *fakeLeaseStore) DropTicket(_ context.Context, ticketHash string) error {
	if err := f.guard("DropTicket"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tickets, ticketHash)
	return nil
}

func (f *fakeLeaseStore) hasTicketBit(ticketHash string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.tickets[ticketHash]
	return ok
}

func (f *fakeLeaseStore) BumpRoomSeq(_ context.Context, roomID int64) (int64, error) {
	if err := f.guard("BumpRoomSeq"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seqs[roomID]++
	return f.seqs[roomID], nil
}

func (f *fakeLeaseStore) MarkOffline(_ context.Context, roomID, mid, at, seq int64, _ int) error {
	if err := f.guard("MarkOffline"); err != nil {
		return err
	}
	if mid <= 0 {
		return nil
	}
	if at == 0 {
		at = model.NowUnix()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offline[userSetKey(roomID, mid)] = [2]int64{at, seq}
	return nil
}

func (f *fakeLeaseStore) OfflineView(_ context.Context, roomID, mid int64) (int64, int64, error) {
	if err := f.guard("OfflineView"); err != nil {
		return 0, 0, err
	}
	if mid <= 0 {
		return 0, 0, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.offline[userSetKey(roomID, mid)]
	if !ok {
		return 0, 0, nil // 绝不伪造历史
	}
	missed := f.seqs[roomID] - v[1]
	if missed < 0 {
		missed = 0
	}
	return v[0], missed, nil
}

func (f *fakeLeaseStore) CacheGet(_ context.Context, key string, into any) (bool, error) {
	if err := f.guard("CacheGet"); err != nil {
		return false, err
	}
	f.mu.Lock()
	raw, ok := f.cache[key]
	f.mu.Unlock()
	if !ok || len(raw) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return false, nil // 脏缓存按未命中回源
	}
	return true, nil
}

func (f *fakeLeaseStore) CacheSet(_ context.Context, key string, value any, ttlSeconds int) error {
	if err := f.guard("CacheSet"); err != nil {
		return err
	}
	if ttlSeconds <= 0 {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cache[key] = raw
	return nil
}

func (f *fakeLeaseStore) CacheDel(_ context.Context, keys ...string) error {
	if err := f.guard("CacheDel"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range keys {
		delete(f.cache, k)
	}
	return nil
}

func (f *fakeLeaseStore) QuotaEpoch(_ context.Context) (int32, error) {
	if err := f.guard("QuotaEpoch"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.epoch, nil
}

func (f *fakeLeaseStore) BumpQuotaEpoch(_ context.Context) (int32, error) {
	if err := f.guard("BumpQuotaEpoch"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.epoch++
	return f.epoch, nil
}

// seedLease 直接放一条租约进 store（含房间/用户集合），供读侧与凭据判定用例起步。
func (f *fakeLeaseStore) seedLease(rec *repository.LeaseRecord) *repository.LeaseRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := repository.CloneLease(rec)
	if cp.State == 0 {
		cp.State = model.LeaseStateActive
	}
	f.leases[cp.LeaseID] = cp
	f.addSetsLocked(cp)
	if cp.NodeID != "" && cp.ConnID != "" {
		f.conns[cp.NodeID+"|"+cp.ConnID] = cp.LeaseID
	}
	return cp
}

// --- RoomGate ---

// fakeRoomGate 可控的房间门禁：归属主播、可否广播、以及「未接线」与「故障」三种形态。
type fakeRoomGate struct {
	available     bool
	owner         int64
	ownerErr      error
	broadcastable bool
	reason        string
	broadcastErr  error

	ownerCalls     int
	broadcastCalls int
}

func (g *fakeRoomGate) RoomOwner(context.Context, int64) (int64, error) {
	g.ownerCalls++
	if g.ownerErr != nil {
		return 0, g.ownerErr
	}
	return g.owner, nil
}

func (g *fakeRoomGate) IsOwner(_ context.Context, roomID, mid int64) (bool, error) {
	if mid <= 0 {
		return false, nil // 与 liveRoomGate 同口径：游客永不可能是房主，不必问 RPC
	}
	owner, err := g.RoomOwner(nil, roomID)
	if err != nil {
		return false, err
	}
	return owner == mid, nil
}

func (g *fakeRoomGate) Broadcastable(context.Context, int64) (bool, string, error) {
	g.broadcastCalls++
	if g.broadcastErr != nil {
		return false, "", g.broadcastErr
	}
	return g.broadcastable, g.reason, nil
}

func (g *fakeRoomGate) Available() bool { return g.available }

// --- Transport ---

// fakeTransport 记录每一次下发请求，供断言扇出节点、定向租约与载荷形状。
type fakeTransport struct {
	available  bool
	result     *repository.FanoutResult
	fanoutErr  error
	deliverErr error
	closeErr   error
	closed     int32

	fanoutReqs  []*repository.FanoutRequest
	deliverReqs []*repository.FanoutRequest
	deliverArgs [][]string
	closeNode   string
	closeLeases []string
}

func (t *fakeTransport) Available() bool { return t.available }

func (t *fakeTransport) Fanout(_ context.Context, req *repository.FanoutRequest) (*repository.FanoutResult, error) {
	t.fanoutReqs = append(t.fanoutReqs, req)
	if t.fanoutErr != nil {
		return nil, t.fanoutErr
	}
	return t.copyResult(req), nil
}

func (t *fakeTransport) Deliver(_ context.Context, req *repository.FanoutRequest, leaseIDs []string) (*repository.FanoutResult, error) {
	t.deliverReqs = append(t.deliverReqs, req)
	t.deliverArgs = append(t.deliverArgs, append([]string(nil), leaseIDs...))
	if t.deliverErr != nil {
		return nil, t.deliverErr
	}
	return t.copyResult(req), nil
}

func (t *fakeTransport) copyResult(req *repository.FanoutRequest) *repository.FanoutResult {
	if t.result != nil {
		return &repository.FanoutResult{FanoutNodes: t.result.FanoutNodes, TargetedConnections: t.result.TargetedConnections}
	}
	nodes := int32(0)
	if req != nil {
		nodes = int32(len(req.Nodes))
	}
	return &repository.FanoutResult{FanoutNodes: nodes, TargetedConnections: nodes}
}

func (t *fakeTransport) CloseConnections(_ context.Context, nodeID string, leaseIDs []string, _ string) (int32, error) {
	t.closed++
	t.closeNode = nodeID
	t.closeLeases = append([]string(nil), leaseIDs...)
	if t.closeErr != nil {
		return 0, t.closeErr
	}
	return int32(len(leaseIDs)), nil
}

// unwiredTransport 与生产装配同源的未接线通道（下发必然 ErrTransportUnavailable）。
func unwiredTransport() repository.Transport { return repository.UnwiredTransport{} }

// --- 四张表的 model fake（内嵌接口 + 覆写被测方法，未覆写的直接 panic） ---

type fakeRoomRoutes struct {
	model.LiveGwRoomRouteModel

	rows      map[int64]*model.LiveGwRoomRoute
	order     []int64
	nextSeq   int64
	failOn    map[string]error
	registers int

	// raceDrainOnCall 让第 N 次（从 1 计）UpdateState 变成「并发对手抢先落地」：
	// 先把行推进到 DRAINING（主节点不动、version+1），再返回 0 行，
	// 等价于对手的 UPDATE 先满足版本条件、本请求的 WHERE version=expected 必然打不中。
	//
	// 为什么必须有这条注入缝（仅测试可见，不改生产语义）：DrainRoomRoute 有两条分支只在并发下存在——
	//  1. 条件更新 0 行但回读已是 DRAINING → 幂等成功（不重复推进 version、不重复审计）；
	//  2. 两步迁移的第 2 步打不中 → 路由必须停在 DRAINING@旧节点，绝不能出现「指向空节点」。
	// 真库要靠两个发布脚本同时打同一房间才撞得出来，单测没有注入缝就永远覆盖不到这两条边，
	// 而这两条边恰好是「排空说成功但其实没排」与「排空半途把房间指向空节点」两个事故点。
	// 与 fakeTickets.raceConsume 同一手法。
	raceDrainOnCall int
	updateCalls     int
}

func newFakeRoomRoutes() *fakeRoomRoutes {
	return &fakeRoomRoutes{rows: map[int64]*model.LiveGwRoomRoute{}, failOn: map[string]error{}}
}

func (m *fakeRoomRoutes) fail(method string, err error) { m.failOn[method] = err }

func (m *fakeRoomRoutes) seed(rows ...*model.LiveGwRoomRoute) {
	for _, r := range rows {
		if _, exists := m.rows[r.RoomId]; !exists {
			m.order = append(m.order, r.RoomId)
		}
		cp := *r
		if cp.ReplicaNodes == "" {
			cp.ReplicaNodes = "[]"
		}
		m.rows[cp.RoomId] = &cp
	}
}

func (m *fakeRoomRoutes) FindOne(_ context.Context, roomID int64) (*model.LiveGwRoomRoute, error) {
	if err := m.failOn["FindOne"]; err != nil {
		return nil, err
	}
	row, ok := m.rows[roomID]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

// Register 复刻默认实现：SERVING/OFFLINE 可被重新承接（version+1），DRAINING 返回行 + ErrRouteDraining。
func (m *fakeRoomRoutes) Register(_ context.Context, route *model.LiveGwRoomRoute) (*model.LiveGwRoomRoute, error) {
	m.registers++
	if err := m.failOn["Register"]; err != nil {
		return nil, err
	}
	if route.RoomId <= 0 {
		return nil, model.ErrInvalidRoomID
	}
	if route.PrimaryNode == "" {
		return nil, model.ErrEmptyNodeID
	}
	if route.ShardCount < 1 {
		route.ShardCount = 1
	}
	if err := route.NormalizeReplicaNodes(nil); err != nil {
		return nil, err
	}
	cur, ok := m.rows[route.RoomId]
	if ok {
		if cur.State == model.RouteStateDraining {
			cp := *cur
			return &cp, fmt.Errorf("room_id=%d: %w", cur.RoomId, model.ErrRouteDraining)
		}
		cur.PrimaryNode = route.PrimaryNode
		cur.ReplicaNodes = route.ReplicaNodes
		cur.ShardCount = route.ShardCount
		cur.State = model.RouteStateServing
		cur.Version++
		cur.TraceId = route.TraceId
		if route.UpdatedBy != "" {
			cur.UpdatedBy = route.UpdatedBy
		}
		cp := *cur
		return &cp, nil
	}
	now := model.NowUnix()
	inserted := *route
	inserted.State = model.RouteStateServing
	inserted.Version = 1
	if inserted.Ctime == 0 {
		inserted.Ctime = now
	}
	if inserted.Mtime == 0 {
		inserted.Mtime = now
	}
	m.seed(&inserted)
	cp := inserted
	return &cp, nil
}

// UpdateState 复刻条件 UPDATE：room_id + state IN(fromStates) [+ primary_node] [+ version] 才生效。
func (m *fakeRoomRoutes) UpdateState(_ context.Context, roomID int64, nodeID string, fromStates []int32,
	expectedVersion int64, to int32, patch model.RoutePatch) (int64, error) {
	if err := m.failOn["UpdateState"]; err != nil {
		return 0, err
	}
	if len(fromStates) == 0 {
		return 0, fmt.Errorf("UpdateState: empty fromStates %w", model.ErrInvalidTransition)
	}
	row, ok := m.rows[roomID]
	if !ok {
		return 0, nil
	}
	m.updateCalls++
	if m.raceDrainOnCall > 0 && m.updateCalls == m.raceDrainOnCall {
		// 注入缝生效：对手先落地（本请求的目标状态被写成 DRAINING@同主节点），本请求 0 行。
		row.State = model.RouteStateDraining
		row.Version++
		row.Mtime = model.NowUnix()
		return 0, nil
	}
	if nodeID != "" && row.PrimaryNode != nodeID {
		return 0, nil
	}
	inFrom := false
	for _, s := range fromStates {
		if row.State == s {
			inFrom = true
		}
	}
	if !inFrom {
		return 0, nil
	}
	if expectedVersion > 0 && row.Version != expectedVersion {
		return 0, nil
	}
	if p := patch.PrimaryNode; p != nil {
		if *p == "" {
			return 0, model.ErrEmptyNodeID
		}
		row.PrimaryNode = *p
	}
	if p := patch.ReplicaNodes; p != nil {
		tmp := &model.LiveGwRoomRoute{}
		if err := tmp.NormalizeReplicaNodes(*p); err != nil {
			return 0, err
		}
		row.ReplicaNodes = tmp.ReplicaNodes
	}
	if p := patch.ShardCount; p != nil {
		if *p < 1 {
			return 0, fmt.Errorf("shard_count=%d must be >= 1", *p)
		}
		row.ShardCount = *p
	}
	if p := patch.DrainReason; p != nil {
		row.DrainReason = *p
	}
	if p := patch.Operator; p != nil && *p != "" {
		row.UpdatedBy = *p
	}
	if p := patch.TraceID; p != nil && *p != "" {
		row.TraceId = *p
	}
	row.State = to
	row.Version++
	row.Mtime = model.NowUnix()
	return 1, nil
}

func (m *fakeRoomRoutes) List(_ context.Context, f model.RoomRouteFilter) ([]*model.LiveGwRoomRoute, int32, error) {
	if err := m.failOn["List"]; err != nil {
		return nil, 0, err
	}
	ids := append([]int64(nil), m.order...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var out []*model.LiveGwRoomRoute
	for _, id := range ids {
		row := m.rows[id]
		if f.State > 0 && row.State != f.State {
			continue
		}
		if f.NodeId != "" && !routeHasNode(row, f.NodeId) {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	total := int32(len(out))
	limit, offset := fakePage(f.Pn, f.Ps, f.MaxPageSize)
	return window(out, limit, offset), total, nil
}

func (m *fakeRoomRoutes) CountByNode(_ context.Context, nodeID string, states []int32) (int64, error) {
	if err := m.failOn["CountByNode"]; err != nil {
		return 0, err
	}
	if nodeID == "" {
		return 0, model.ErrEmptyNodeID
	}
	if len(states) == 0 {
		states = []int32{model.RouteStateServing, model.RouteStateDraining}
	}
	var n int64
	for _, row := range m.rows {
		if !routeHasNode(row, nodeID) {
			continue
		}
		for _, s := range states {
			if row.State == s {
				n++
				break
			}
		}
	}
	return n, nil
}

func (m *fakeRoomRoutes) ListByNode(_ context.Context, nodeID string, states []int32, afterRoomID int64, limit int32) ([]*model.LiveGwRoomRoute, error) {
	if err := m.failOn["ListByNode"]; err != nil {
		return nil, err
	}
	if nodeID == "" {
		return nil, model.ErrEmptyNodeID
	}
	ids := append([]int64(nil), m.order...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var out []*model.LiveGwRoomRoute
	for _, id := range ids {
		row := m.rows[id]
		if id <= afterRoomID || !routeHasNode(row, nodeID) {
			continue
		}
		if len(states) > 0 {
			hit := false
			for _, s := range states {
				if row.State == s {
					hit = true
				}
			}
			if !hit {
				continue
			}
		}
		cp := *row
		out = append(out, &cp)
		if limit > 0 && int32(len(out)) >= limit {
			break
		}
	}
	return out, nil
}

func routeHasNode(row *model.LiveGwRoomRoute, nodeID string) bool {
	if row.PrimaryNode == nodeID {
		return true
	}
	replicas, err := row.ReplicaNodesList()
	if err != nil {
		return false
	}
	for _, r := range replicas {
		if r == nodeID {
			return true
		}
	}
	return false
}

func fakePage(pn, ps, maxPS int32) (limit, offset int32) {
	if pn < 1 {
		pn = 1
	}
	if ps <= 0 {
		ps = 20
	}
	if maxPS > 0 && ps > maxPS {
		ps = maxPS
	}
	return ps, (pn - 1) * ps
}

func window[T any](rows []T, limit, offset int32) []T {
	if int32(len(rows)) <= offset {
		return nil
	}
	end := offset + limit
	if int32(len(rows)) < end {
		end = int32(len(rows))
	}
	return rows[offset:end]
}

type fakeQuotas struct {
	model.LiveGwAccessQuotaModel

	rows    []*model.LiveGwAccessQuota
	nextID  int64
	failOn  map[string]error
	creates int
	updates int
}

func newFakeQuotas(rows ...*model.LiveGwAccessQuota) *fakeQuotas {
	m := &fakeQuotas{rows: append([]*model.LiveGwAccessQuota(nil), rows...), failOn: map[string]error{}}
	for _, r := range m.rows {
		if r.Id > m.nextID {
			m.nextID = r.Id
		}
	}
	return m
}

func (m *fakeQuotas) fail(method string, err error) { m.failOn[method] = err }

func (m *fakeQuotas) find(scope int32, scopeID int64) *model.LiveGwAccessQuota {
	for _, r := range m.rows {
		if r.Scope == scope && r.ScopeId == scopeID {
			cp := *r
			return &cp
		}
	}
	return nil
}

func (m *fakeQuotas) FindOne(_ context.Context, scope int32, scopeID int64) (*model.LiveGwAccessQuota, error) {
	if err := m.failOn["FindOne"]; err != nil {
		return nil, err
	}
	row := m.find(scope, scopeID)
	if row == nil {
		return nil, nil
	}
	return row, nil
}

func (m *fakeQuotas) FindByRequestID(_ context.Context, requestID string) (*model.LiveGwAccessQuota, error) {
	if err := m.failOn["FindByRequestID"]; err != nil {
		return nil, err
	}
	for _, r := range m.rows {
		if r.RequestId == requestID {
			cp := *r
			return &cp, nil
		}
	}
	return nil, nil
}

// Create 复刻 UNIQUE(scope,scope_id) 语义：已存在即 ErrVersionConflict，强制先读后带版本更新。
func (m *fakeQuotas) Create(_ context.Context, q *model.LiveGwAccessQuota) (int64, error) {
	m.creates++
	if err := m.failOn["Create"]; err != nil {
		return 0, err
	}
	if err := model.CheckQuotaBounds(q, 0, 0, 0); err != nil {
		return 0, err
	}
	if cur := m.find(q.Scope, q.ScopeId); cur != nil {
		return 0, fmt.Errorf("scope=%d scope_id=%d: %w", q.Scope, q.ScopeId, model.ErrVersionConflict)
	}
	m.nextID++
	inserted := *q
	inserted.Id = m.nextID
	inserted.Version = 1
	if inserted.Ctime == 0 {
		inserted.Ctime = model.NowUnix()
	}
	m.rows = append(m.rows, &inserted)
	return m.nextID, nil
}

// Update 复刻条件 UPDATE：WHERE (scope, scope_id, version=expected) 且 expected>0。
func (m *fakeQuotas) Update(_ context.Context, scope int32, scopeID, expectedVersion int64, q *model.LiveGwAccessQuota) (int64, error) {
	m.updates++
	if err := m.failOn["Update"]; err != nil {
		return 0, err
	}
	if expectedVersion <= 0 {
		return 0, fmt.Errorf("Update: expected_version=%d must be > 0: %w", expectedVersion, model.ErrVersionConflict)
	}
	cur := m.find(scope, scopeID)
	if cur == nil {
		return 0, nil
	}
	if cur.Version != expectedVersion {
		return 0, nil
	}
	target := m.rowsIndexOf(scope, scopeID)
	patched := *q
	patched.Id = cur.Id
	patched.Scope = cur.Scope
	patched.ScopeId = cur.ScopeId
	patched.Version = cur.Version + 1
	patched.Ctime = cur.Ctime
	if patched.Mtime == 0 {
		patched.Mtime = model.NowUnix()
	}
	m.rows[target] = &patched
	return 1, nil
}

func (m *fakeQuotas) rowsIndexOf(scope int32, scopeID int64) int {
	for i, r := range m.rows {
		if r.Scope == scope && r.ScopeId == scopeID {
			return i
		}
	}
	return -1
}

func (m *fakeQuotas) List(_ context.Context, f model.AccessQuotaFilter) ([]*model.LiveGwAccessQuota, int32, error) {
	if err := m.failOn["List"]; err != nil {
		return nil, 0, err
	}
	var out []*model.LiveGwAccessQuota
	for _, r := range m.rows {
		if f.Scope > 0 && r.Scope != f.Scope {
			continue
		}
		if f.ScopeId != 0 && r.ScopeId != f.ScopeId {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	total := int32(len(out))
	limit, offset := fakePage(f.Pn, f.Ps, f.MaxPageSize)
	return window(out, limit, offset), total, nil
}

// Resolve 复刻继承链：自外向内（先 GLOBAL 再本层）逐字段覆盖，0 值表示继承。
func (m *fakeQuotas) Resolve(_ context.Context, scope int32, scopeID int64, defaults model.QuotaDefaults) (*model.EffectiveQuota, error) {
	if err := m.failOn["Resolve"]; err != nil {
		return nil, err
	}
	if !model.ValidQuotaScope(scope) {
		return nil, model.ErrInvalidQuotaScope
	}
	eff := &model.EffectiveQuota{
		MaxConnections:   defaults.MaxConnections,
		BroadcastQps:     defaults.BroadcastQps,
		DanmakuQps:       defaults.DanmakuQps,
		LeaseTtlSeconds:  defaults.LeaseTtlSeconds,
		TicketTtlSeconds: defaults.TicketTtlSeconds,
		MaxPayloadBytes:  defaults.MaxPayloadBytes,
		AllowGuest:       defaults.AllowGuest,
	}
	chain := model.QuotaScopeChain(scope)
	for i := len(chain) - 1; i >= 0; i-- {
		layer := chain[i]
		layerID := int64(0)
		if layer != model.QuotaScopeGlobal {
			layerID = scopeID
		}
		row := m.find(layer, layerID)
		if row == nil {
			continue
		}
		applyFakeQuotaLayer(eff, row, layer)
	}
	return eff, nil
}

func applyFakeQuotaLayer(eff *model.EffectiveQuota, row *model.LiveGwAccessQuota, layer int32) {
	hit := false
	if row.MaxConnections != 0 {
		eff.MaxConnections = row.MaxConnections
		hit = true
	}
	if row.BroadcastQps != 0 {
		eff.BroadcastQps = row.BroadcastQps
		hit = true
	}
	if row.DanmakuQps != 0 {
		eff.DanmakuQps = row.DanmakuQps
		hit = true
	}
	if row.LeaseTtlSeconds != 0 {
		eff.LeaseTtlSeconds = row.LeaseTtlSeconds
		hit = true
	}
	if row.TicketTtlSeconds != 0 {
		eff.TicketTtlSeconds = row.TicketTtlSeconds
		hit = true
	}
	if row.MaxPayloadBytes != 0 {
		eff.MaxPayloadBytes = row.MaxPayloadBytes
		hit = true
	}
	if row.AllowGuest != 0 {
		eff.AllowGuest = row.AllowGuest == 1
		hit = true
	}
	if hit {
		eff.HitScopes = append(eff.HitScopes, fmt.Sprintf("%d:%d", layer, row.ScopeId))
	}
}

type fakeBroadcastLogs struct {
	model.LiveGwBroadcastLogModel

	rows    []*model.LiveGwBroadcastLog
	nextID  int64
	failOn  map[string]error
	finds   []string // 兜底回读记「查了哪一列」（FindByMessage/FindByEvent），用于钉住去重维度没走错列
	inserts int
	dups    int
}

func newFakeBroadcastLogs() *fakeBroadcastLogs {
	return &fakeBroadcastLogs{failOn: map[string]error{}}
}

func (m *fakeBroadcastLogs) fail(method string, err error) { m.failOn[method] = err }

func (m *fakeBroadcastLogs) all() []*model.LiveGwBroadcastLog {
	return append([]*model.LiveGwBroadcastLog(nil), m.rows...)
}

// Insert 复刻 UNIQUE(room_id,message_id)：重复投递不追加行，返回 (0,nil) 由调用方判 duplicated。
func (m *fakeBroadcastLogs) Insert(_ context.Context, l *model.LiveGwBroadcastLog) (int64, error) {
	m.inserts++
	if err := m.failOn["Insert"]; err != nil {
		return 0, err
	}
	if l.RoomId <= 0 {
		return 0, model.ErrInvalidRoomID
	}
	if l.MessageId == "" {
		return 0, model.ErrEmptyMessageID
	}
	if !model.ValidBroadcastLogState(l.State) {
		return 0, fmt.Errorf("fake: invalid broadcast log state %d", l.State)
	}
	if l.State == model.BroadcastLogSent && l.DropReason != model.DropOK {
		return 0, fmt.Errorf("fake: sent row must carry DROP_REASON_OK, got %d", l.DropReason)
	}
	for _, r := range m.rows {
		if r.RoomId == l.RoomId && r.MessageId == l.MessageId {
			m.dups++
			return 0, nil
		}
	}
	m.nextID++
	cp := *l
	cp.Id = m.nextID
	if cp.Ctime == 0 {
		cp.Ctime = model.NowUnix()
	}
	m.rows = append(m.rows, &cp)
	return m.nextID, nil
}

func (m *fakeBroadcastLogs) FindByMessage(_ context.Context, roomID int64, messageID string) (*model.LiveGwBroadcastLog, error) {
	m.finds = append(m.finds, "FindByMessage:"+messageID)
	if err := m.failOn["FindByMessage"]; err != nil {
		return nil, err
	}
	for _, r := range m.rows {
		if r.RoomId == roomID && r.MessageId == messageID {
			cp := *r
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *fakeBroadcastLogs) FindByEvent(_ context.Context, roomID int64, eventID string) (*model.LiveGwBroadcastLog, error) {
	m.finds = append(m.finds, "FindByEvent:"+eventID)
	if err := m.failOn["FindByEvent"]; err != nil {
		return nil, err
	}
	for _, r := range m.rows {
		if r.RoomId == roomID && r.EventId == eventID {
			cp := *r
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *fakeBroadcastLogs) List(_ context.Context, f model.BroadcastLogFilter) ([]*model.LiveGwBroadcastLog, int32, error) {
	if err := m.failOn["List"]; err != nil {
		return nil, 0, err
	}
	var out []*model.LiveGwBroadcastLog
	for _, r := range m.rows {
		if r.RoomId != f.RoomId {
			continue
		}
		if f.Kind > 0 && r.Kind != f.Kind {
			continue
		}
		if f.SenderMid > 0 && r.SenderMid != f.SenderMid {
			continue
		}
		if f.OnlyDropped && r.State == model.BroadcastLogSent {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	total := int32(len(out))
	limit, offset := fakePage(f.Pn, f.Ps, f.MaxPageSize)
	// 与 model 一致：按 id 倒序（最新在前）。
	rev := make([]*model.LiveGwBroadcastLog, 0, len(out))
	for i := len(out) - 1; i >= 0; i-- {
		rev = append(rev, out[i])
	}
	return window(rev, limit, offset), total, nil
}

func (m *fakeBroadcastLogs) CountByRoomSince(_ context.Context, roomID int64, since int64) (*model.BroadcastLogStats, error) {
	if err := m.failOn["CountByRoomSince"]; err != nil {
		return nil, err
	}
	stats := &model.BroadcastLogStats{}
	for _, r := range m.rows {
		if r.RoomId != roomID || r.Ctime < since {
			continue
		}
		switch r.State {
		case model.BroadcastLogSent:
			stats.Sent++
		case model.BroadcastLogDropped:
			stats.Dropped++
		case model.BroadcastLogDenied:
			stats.Denied++
		case model.BroadcastLogDuplicated:
			stats.Duplicated++
		}
	}
	return stats, nil
}

func (m *fakeBroadcastLogs) PurgeBefore(_ context.Context, before int64, limit int32) (int64, error) {
	if err := m.failOn["PurgeBefore"]; err != nil {
		return 0, err
	}
	kept := make([]*model.LiveGwBroadcastLog, 0, len(m.rows))
	var deleted int64
	for _, r := range m.rows {
		if r.Ctime < before && (limit <= 0 || int32(deleted) < limit) {
			deleted++
			continue
		}
		kept = append(kept, r)
	}
	m.rows = kept
	return deleted, nil
}

type fakeTickets struct {
	model.LiveGwReconnectTicketModel

	rows    []*model.LiveGwReconnectTicket
	nextID  int64
	failOn  map[string]error
	inserts int
	// raceConsume 为真时，Consume 模拟「并发对手在本次请求读行之后抢先消费了这张票」：
	// 行被对手置 USED 并回填对手的 lease_id，本请求的条件更新返回 0 行。
	// 真库下这条分支只能靠两个请求撞出来，单测必须显式注入才能验证兑换的补偿路径。
	raceConsume bool
}

func newFakeTickets(rows ...*model.LiveGwReconnectTicket) *fakeTickets {
	m := &fakeTickets{rows: append([]*model.LiveGwReconnectTicket(nil), rows...), failOn: map[string]error{}}
	for _, r := range m.rows {
		if r.Id > m.nextID {
			m.nextID = r.Id
		}
	}
	return m
}

func (m *fakeTickets) fail(method string, err error) { m.failOn[method] = err }

func (m *fakeTickets) all() []*model.LiveGwReconnectTicket {
	return append([]*model.LiveGwReconnectTicket(nil), m.rows...)
}

func (m *fakeTickets) Insert(_ context.Context, t *model.LiveGwReconnectTicket) (int64, error) {
	m.inserts++
	if err := m.failOn["Insert"]; err != nil {
		return 0, err
	}
	if t.TicketId == "" {
		return 0, model.ErrEmptyTicket
	}
	if t.RequestId == "" {
		return 0, model.ErrEmptyRequestID
	}
	if t.RoomId <= 0 {
		return 0, model.ErrInvalidRoomID
	}
	if t.Mid < 0 {
		return 0, model.ErrInvalidMid
	}
	if t.ExpireAt <= t.IssuedAt {
		return 0, fmt.Errorf("expire_at=%d issued_at=%d %w", t.ExpireAt, t.IssuedAt, model.ErrTicketExpired)
	}
	if t.TicketHash == "" {
		return 0, model.ErrEmptyTicket
	}
	for _, r := range m.rows {
		if r.RequestId == t.RequestId || r.TicketHash == t.TicketHash || r.TicketId == t.TicketId {
			return 0, fmt.Errorf("request_id=%s: %w", t.RequestId, model.ErrRequestIdDuplicated)
		}
	}
	m.nextID++
	cp := *t
	cp.Id = m.nextID
	if cp.State == 0 {
		cp.State = model.TicketStateIssued
	}
	if cp.Ctime == 0 {
		cp.Ctime = model.NowUnix()
	}
	m.rows = append(m.rows, &cp)
	return m.nextID, nil
}

func (m *fakeTickets) FindOne(_ context.Context, ticketID string) (*model.LiveGwReconnectTicket, error) {
	if err := m.failOn["FindOne"]; err != nil {
		return nil, err
	}
	if ticketID == "" {
		return nil, model.ErrEmptyTicket
	}
	for _, r := range m.rows {
		if r.TicketId == ticketID {
			cp := *r
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *fakeTickets) FindByHash(_ context.Context, ticketHash string) (*model.LiveGwReconnectTicket, error) {
	if err := m.failOn["FindByHash"]; err != nil {
		return nil, err
	}
	if ticketHash == "" {
		return nil, model.ErrEmptyTicket
	}
	for _, r := range m.rows {
		if r.TicketHash == ticketHash {
			cp := *r
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *fakeTickets) FindByRequestID(_ context.Context, requestID string) (*model.LiveGwReconnectTicket, error) {
	if err := m.failOn["FindByRequestID"]; err != nil {
		return nil, err
	}
	if requestID == "" {
		return nil, nil
	}
	for _, r := range m.rows {
		if r.RequestId == requestID {
			cp := *r
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *fakeTickets) List(_ context.Context, f model.TicketFilter) ([]*model.LiveGwReconnectTicket, int32, error) {
	if err := m.failOn["List"]; err != nil {
		return nil, 0, err
	}
	var out []*model.LiveGwReconnectTicket
	for _, r := range m.rows {
		if f.RoomId > 0 && r.RoomId != f.RoomId {
			continue
		}
		if f.Mid > 0 && r.Mid != f.Mid {
			continue
		}
		if f.State > 0 && r.State != f.State {
			continue
		}
		if f.LeaseId != "" && r.LeaseId != f.LeaseId {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	total := int32(len(out))
	limit, offset := fakePage(f.Pn, f.Ps, f.MaxPageSize)
	rev := make([]*model.LiveGwReconnectTicket, 0, len(out))
	for i := len(out) - 1; i >= 0; i-- {
		rev = append(rev, out[i])
	}
	return window(rev, limit, offset), total, nil
}

// Consume 复刻「只有 state=ISSUED 且三元组匹配且未过期才置 USED」的条件 UPDATE。
func (m *fakeTickets) Consume(_ context.Context, ticketID string, roomID, mid int64, newLeaseID string, usedAt int64) (int64, error) {
	if err := m.failOn["Consume"]; err != nil {
		return 0, err
	}
	if ticketID == "" {
		return 0, model.ErrEmptyTicket
	}
	if usedAt <= 0 {
		usedAt = model.NowUnix()
	}
	for _, r := range m.rows {
		if r.TicketId != ticketID {
			continue
		}
		if m.raceConsume {
			// 对手的 UPDATE 先落地：本请求的条件更新（WHERE state=ISSUED）必然打不中。
			r.State = model.TicketStateUsed
			r.UsedAt = usedAt
			r.NewLeaseId = "lgwl_racer"
			return 0, nil
		}
		if r.State != model.TicketStateIssued || r.RoomId != roomID || r.Mid != mid {
			return 0, nil
		}
		if r.ExpireAt <= usedAt {
			return 0, nil
		}
		r.State = model.TicketStateUsed
		r.UsedAt = usedAt
		r.NewLeaseId = newLeaseID
		r.Mtime = usedAt
		return 1, nil
	}
	return 0, nil
}

func (m *fakeTickets) RevokeByTicketID(_ context.Context, ticketID, reason, operator string) (int64, error) {
	if err := m.failOn["RevokeByTicketID"]; err != nil {
		return 0, err
	}
	if ticketID == "" {
		return 0, model.ErrEmptyTicket
	}
	if reason == "" {
		return 0, model.ErrEmptyReason
	}
	for _, r := range m.rows {
		if r.TicketId == ticketID {
			if r.State != model.TicketStateIssued {
				return 0, nil
			}
			r.State = model.TicketStateRevoked
			r.RevokeReason = reason
			r.RevokedBy = operator
			r.Mtime = model.NowUnix()
			return 1, nil
		}
	}
	return 0, nil
}

func (m *fakeTickets) RevokeByRoomMid(_ context.Context, roomID, mid int64, reason, operator string, limit int32) (int64, error) {
	if err := m.failOn["RevokeByRoomMid"]; err != nil {
		return 0, err
	}
	var n int64
	for _, r := range m.rows {
		if r.RoomId != roomID || r.Mid != mid || r.State != model.TicketStateIssued {
			continue
		}
		if limit > 0 && n >= int64(limit) {
			break
		}
		r.State = model.TicketStateRevoked
		r.RevokeReason = reason
		r.RevokedBy = operator
		n++
	}
	return n, nil
}

func (m *fakeTickets) MarkExpired(_ context.Context, now int64, limit int32) (int64, error) {
	if err := m.failOn["MarkExpired"]; err != nil {
		return 0, err
	}
	var n int64
	for _, r := range m.rows {
		if r.State != model.TicketStateIssued || r.ExpireAt > now {
			continue
		}
		if limit > 0 && n >= int64(limit) {
			break
		}
		r.State = model.TicketStateExpired
		n++
	}
	return n, nil
}

func (m *fakeTickets) CountUnusedByRoomMid(_ context.Context, roomID, mid int64) (int64, error) {
	if err := m.failOn["CountUnusedByRoomMid"]; err != nil {
		return 0, err
	}
	var n int64
	for _, r := range m.rows {
		if r.RoomId == roomID && r.Mid == mid && r.State == model.TicketStateIssued {
			n++
		}
	}
	return n, nil
}

// --- 装配脚手架 ---

// testEnv 一套「依赖全部可控」的 ServiceContext 与它的 fake 句柄。
type testEnv struct {
	t      *testing.T
	Svc    *svc.ServiceContext
	Leases *fakeLeaseStore
	Rooms  *fakeRoomGate
	Tp     *fakeTransport
	Routes *fakeRoomRoutes
	Quotas *fakeQuotas
	Logs   *fakeBroadcastLogs
	Ticket *fakeTickets

	Clock *fakeClock
}

// testSignerKey 只存在于测试文件的临时密钥（≥16 字节，永不进生产配置）。
const testSignerKey = "unit-test-only-hmac-key-32bytes"

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{t: t}
	e.Leases = newFakeLeaseStore()
	e.Rooms = &fakeRoomGate{available: true, owner: anchorMid, broadcastable: true}
	e.Tp = &fakeTransport{available: true}
	e.Routes = newFakeRoomRoutes()
	e.Quotas = newFakeQuotas()
	e.Logs = newFakeBroadcastLogs()
	e.Ticket = newFakeTickets()

	signer, err := repository.NewSigner(testSignerKey, "LIVEGW_TEST_TICKET_KEY")
	if err != nil {
		t.Fatalf("测试签名器构造失败: %v", err)
	}
	e.Clock = newFakeClock(t, time.Unix(testBaseTime, 0).UTC())
	e.Svc = &svc.ServiceContext{
		Config: e.conf(),
		Leases: e.Leases,
		Store:  &repository.Store{RoomRoutes: e.Routes, Quotas: e.Quotas, BroadcastLogs: e.Logs, Tickets: e.Ticket},
		Rooms:  e.Rooms,
		Fanout: e.Tp,
		Signer: signer,
	}
	return e
}

const (
	testBaseTime int64 = 1_800_000_000 // 固定起点，避免测试依赖真实时钟
	roomID       int64 = 7001
	otherRoom    int64 = 7002
	mid          int64 = 9100
	anchorMid    int64 = 5001
	otherMid     int64 = 9200
	nodeA              = "gw-node-a"
	nodeB              = "gw-node-b"
)

// conf 用与 etc 默认值同口径的配置（json default 标签只在 conf.Load 时生效，测试里必须显式填）。
func (e *testEnv) conf() config.Config {
	c := config.Config{LiveGateway: config.LiveGatewayConf{
		DefaultLeaseTTLSeconds:    30,
		MaxLeaseTTLSeconds:        300,
		MinLeaseTTLSeconds:        10,
		DefaultTicketTTLSeconds:   120,
		MaxTicketTTLSeconds:       600,
		HeartbeatMaxSkewSeconds:   300,
		ReconnectGraceSeconds:     60,
		MaxPayloadBytes:           1024,
		DefaultRoomBroadcastQps:   200,
		DefaultUserBroadcastQps:   5,
		DefaultMaxRoomConnections: 50000,
		AllowGuestByDefault:       true,
		PayloadDigestBytes:        32,
		RoomRouteCacheTTLSeconds:  10,
		QuotaCacheTTLSeconds:      60,
		ConnectionScanLimit:       500,
		MaxPageSize:               50,
		DedupWindowSeconds:        600,
		RateWindowSeconds:         1,
		MaxTicketsPerSubject:      5,
		DefaultRoomShardCount:     1,
		MaxIdLenBytes:             64,
		MaxTargetRoles:            6,
		MaxTopicsPerSubscription:  8,
		TrustedSourceServices:     []string{"moderation"},
		BroadcastLogRetentionDays: 30,
	}, Security: config.SecurityConf{TicketSignKeyRef: "LIVEGW_TEST_TICKET_KEY"}}
	return c
}

// apply 把可能已改过的配置重新写回 ServiceContext（各用例常需微调单个开关）。
func (e *testEnv) apply(mut func(*config.LiveGatewayConf)) {
	lg := e.Svc.Config.LiveGateway
	mut(&lg)
	e.Svc.Config.LiveGateway = lg
}

// markStoreMissing 模拟 DataSource 未配置（Store==nil），用于验证 RequireStore 的显式失败。
func (e *testEnv) markStoreMissing() { e.Svc.Store = nil }

// markSignerMissing 模拟密钥未注入。
func (e *testEnv) markSignerMissing() {
	e.Svc.Signer = repository.MissingSigner{Ref: "LIVEGW_TEST_TICKET_KEY"}
}

// markRoomGateUnwired 模拟 live-room 客户端未接线（fail-closed 降级路径）。
func (e *testEnv) markRoomGateUnwired() { e.Svc.Rooms = repository.UnwiredRoomGate{} }

// markTransportUnwired 模拟下发通道未接线。
func (e *testEnv) markTransportUnwired() { e.Svc.Fanout = repository.UnwiredTransport{} }

// addQuotaRow 直接往配额表塞一行（绕过 UpsertAccessQuota 的校验与 CAS）。
// 用途是「配额已存在」这一既成事实：限流与载荷上限用例要的是继承结果，
// 不该顺带把写接口的行为也测进读路径（写接口由 access_quota_test.go 单独覆盖）。
func (e *testEnv) addQuotaRow(q *model.LiveGwAccessQuota) *model.LiveGwAccessQuota {
	cp := *q
	if cp.Id == 0 {
		e.Quotas.nextID++
		cp.Id = e.Quotas.nextID
	}
	if cp.Version == 0 {
		cp.Version = 1
	}
	if cp.Ctime == 0 {
		cp.Ctime = e.Clock.unix()
	}
	e.Quotas.rows = append(e.Quotas.rows, &cp)
	return &cp
}

// seedActiveLease 放一条活租约，返回它的 ID。
func (e *testEnv) seedActiveLease(leaseID, connID, nodeID string, room, user int64, role int32) *repository.LeaseRecord {
	return e.Leases.seedLease(&repository.LeaseRecord{
		LeaseID:  leaseID,
		ConnID:   connID,
		NodeID:   nodeID,
		RoomID:   room,
		Mid:      user,
		Role:     role,
		State:    model.LeaseStateActive,
		IssuedAt: e.Clock.unix(),
		ExpireAt: e.Clock.unix() + 30,
	})
}
