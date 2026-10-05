package logic

// fakes_test.go 是 playback logic 测试的内存替身集合。
//
// 为什么需要注入缝：playback 的 ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造走 repository.New（真 Redis + 真 MySQL），测试无处塞替身。因此本包用例统一用
// repository.NewWithDeps(内存缓存, fakeConn, 内存 sessionMd/progressMd/outboxMd) 组装
// **真实的 Repository**，只把它的 5 个依赖换成替身——这样「缓存读穿/回填、状态推进、
// 计数去重、进度+Outbox 同事务」整条链路都在被测路径上，而不是把 Repository 也 mock 掉。
// 见 internal/repository/cache.go 的 Cacher 注释。
//
// 四条替身纪律（catalog / rights 两轮踩过的坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**：logic 里的写回不得污染库存行，否则「有没有真的落库」
//     这类断言会被共享指针掩盖。
//  2. 副作用按**顺序**记录（callLog），断言序列而不是只断言次数：playback 要紧的结论是
//     「拒绝签发时有没有留下会话」「到期拒绝时有没有失效缓存」「首次放行有没有只加一次计数」。
//  3. 错误注入按方法粒度（faultInjector.failWith("FindOne", err)），不用一个全局 err。
//  4. 布数据走替身的**静默写入路径**（seedSession / seedProgress / cache.warm → put 而非公开的
//     Insert / Upsert）：布景不算被测调用，因此轨迹断言可以直接从 0 开始数。
//     如果哪天改成用公开方法布景，就必须先在布景之后取 before := st.log.snapshot()。
//
// 与 catalog / rights 多出来的一条纪律：GetPlaybackToken 生成的 session_id 是 ULID（不可预测），
// 因此涉及它的轨迹只能用 wantCount + 回读库存断言，只有**布景给定的** session_id 才允许
// 出现在精确序列期望里。
//
// 覆盖边界（如实声明）：
//   - 替身只复刻 model 层 SQL 的**语义**（唯一索引冲突、状态门槛、GREATEST 只前进、
//     ORDER BY mtime DESC LIMIT 1），不证明 SQL 与列名本身；
//   - 不做 LIMIT/OFFSET 切片：playback 的 model 没有分页方法；
//   - 事务的原子性与回滚由 MySQL 保证，替身只能断言「进度与 Outbox 两次写入拿到的是
//     同一个非 nil 事务会话、且只开了一个事务」；
//   - Redis 的 SETEX/SETNX/INCR 由 fakeCache 按同语义复刻，不验证真实 Redis 行为与 TTL 编码
//     （TTL 钳制在 repository 包的 sessionTTL 用例里已覆盖）。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/playback/internal/repository"
	"go-video/services/playback/internal/signurl"
	"go-video/services/playback/internal/svc"
	"go-video/services/playback/model"
)

// --- 测试常量（真实签名器配置，纯计算不涉及外部依赖） ---

const (
	testPrivateKey = "unit-test-cdn-private-key"
	testBaseURL    = "https://play.example.com"
	testKeyID      = "key-2026-09"
	testTokenTTL   = int64(1800)
)

// --- 断言小工具（本包共享；与 rights 包同名不同文件，互不影响） ---

func wantEQ[T comparable](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s = %#v, want %#v", label, field, got, want)
	}
}

func wantErrIs(t *testing.T, label string, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want %v", label, want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s：错误 = %v, want errors.Is(%v)", label, err, want)
	}
}

func wantNoErr(t *testing.T, label string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s：%v", label, err)
	}
}

// wantNoCall 断言 from 之后没有发生任何依赖调用（守卫必须发生在触库/触缓存之前）。
func wantNoCall(t *testing.T, label string, st *store, from int) {
	t.Helper()
	if ops := st.log.opsFrom(from); len(ops) != 0 {
		t.Fatalf("%s：守卫拒绝后仍发生依赖调用 %v", label, ops)
	}
}

func wantOps(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：调用序列 = [%s], want [%s]", label, strings.Join(got, " → "), strings.Join(want, " → "))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s：第 %d 次调用 = %s, want %s（完整序列 [%s]",
				label, i+1, got[i], want[i], strings.Join(got, " → "))
		}
	}
}

// wantCount 断言某类调用发生的次数（用于 session_id 不可预测的写侧断言）。
// 布数据走替身的静默写入路径（见 seedSession），因此这里数的是**被测代码**的调用。
func wantCount(t *testing.T, label string, log *callLog, prefix string, want int) {
	t.Helper()
	if got := log.countPrefix(prefix); got != want {
		t.Errorf("%s：%s* 调用次数 = %d, want %d（完整序列 [%s]）", label, prefix, got, want, strings.Join(log.ops, " → "))
	}
}

// assertAround 断言墙钟派生值落在期望附近：logic 内部用 time.Now()，跨秒抖动不可避免，
// 因此只允许 [-slack, +1] 的偏差；窗口封顶类断言必须用精确相等（见各用例注释）。
func assertAround(t *testing.T, label, field string, got, want, slack int64) {
	t.Helper()
	if got < want-slack || got > want+slack {
		t.Errorf("%s：%s = %d, want %d±%d", label, field, got, want, slack)
	}
}

// --- 调用轨迹 ---

type callLog struct{ ops []string }

func (c *callLog) add(format string, args ...any) {
	c.ops = append(c.ops, fmt.Sprintf(format, args...))
}

func (c *callLog) countPrefix(prefix string) int {
	n := 0
	for _, o := range c.ops {
		if strings.HasPrefix(o, prefix) {
			n++
		}
	}
	return n
}

func (c *callLog) snapshot() int             { return len(c.ops) }
func (c *callLog) opsFrom(from int) []string { return c.ops[from:] }

// --- 错误注入 ---

type faultInjector struct{ by map[string]error }

func (f *faultInjector) failWith(method string, err error) {
	if f.by == nil {
		f.by = map[string]error{}
	}
	f.by[method] = err
}

func (f *faultInjector) fail(method string) error { return f.by[method] }

// --- Redis key 复刻（与 repository 未导出的常量逐字对齐；不一致时读穿用例即红） ---

func keySession(sessionID string) string  { return fmt.Sprintf("pb:s:%s", sessionID) }
func keyVerified(sessionID string) string { return fmt.Sprintf("pb:v:%s", sessionID) }
func keyPlayCount(contentType, contentID int64) string {
	return fmt.Sprintf("pb:pc:%d:%d", contentType, contentID)
}

// --- 缓存替身 ---

// fakeCache 实现 repository.Cacher。miss 返回 (nil, nil)，与真实 Cache 处理 redis.Nil 一致；
// MarkVerifiedOnce 复刻 SETNX：只有第一次返回 true。
type fakeCache struct {
	faultInjector
	log      *callLog
	sessions map[string]*model.PlaybackSession
	verified map[string]int   // 已打标记的会话
	counts   map[string]int64 // 播放计数
	ttls     map[string]int   // MarkVerifiedOnce 收到的 TTL，按 key 记录
}

func newFakeCache(log *callLog) *fakeCache {
	return &fakeCache{
		log:      log,
		sessions: map[string]*model.PlaybackSession{},
		verified: map[string]int{},
		counts:   map[string]int64{},
		ttls:     map[string]int{},
	}
}

func (f *fakeCache) Ping(context.Context) error { f.log.add("cache.Ping"); return nil }

func (f *fakeCache) GetSession(_ context.Context, sessionID string) (*model.PlaybackSession, error) {
	key := keySession(sessionID)
	f.log.add("cache.GetSession:%s", key)
	if err := f.fail("GetSession"); err != nil {
		return nil, err
	}
	s, ok := f.sessions[key]
	if !ok {
		return nil, nil
	}
	cp := *s
	return &cp, nil
}

func (f *fakeCache) SetSession(_ context.Context, s *model.PlaybackSession, _ int64) error {
	key := keySession(s.SessionId)
	f.log.add("cache.SetSession:%s", key)
	if err := f.fail("SetSession"); err != nil {
		return err
	}
	cp := *s
	f.sessions[key] = &cp
	return nil
}

func (f *fakeCache) DelSession(_ context.Context, sessionID string) error {
	key := keySession(sessionID)
	f.log.add("cache.Del:%s", key)
	if err := f.fail("DelSession"); err != nil {
		return err
	}
	delete(f.sessions, key)
	return nil
}

func (f *fakeCache) MarkVerifiedOnce(_ context.Context, sessionID string, ttl int) (bool, error) {
	key := keyVerified(sessionID)
	f.log.add("cache.MarkVerified:%s/%d", key, ttl)
	if err := f.fail("MarkVerifiedOnce"); err != nil {
		return false, err
	}
	if f.verified[key] > 0 {
		return false, nil // SETNX 已存在
	}
	f.verified[key] = 1
	f.ttls[key] = ttl
	return true, nil
}

func (f *fakeCache) IncrPlayCount(_ context.Context, contentType, contentID int64) (int64, error) {
	key := keyPlayCount(contentType, contentID)
	f.log.add("cache.Incr:%s", key)
	if err := f.fail("IncrPlayCount"); err != nil {
		return 0, err
	}
	f.counts[key]++
	return f.counts[key], nil
}

func (f *fakeCache) PlayCount(_ context.Context, contentType, contentID int64) (int64, error) {
	key := keyPlayCount(contentType, contentID)
	f.log.add("cache.PlayCount:%s", key)
	if err := f.fail("PlayCount"); err != nil {
		return 0, err
	}
	return f.counts[key], nil
}

// warm 直接预热会话缓存（布数据，不写轨迹：缓存命中用例要区分「读穿回填」和「本来就命中」）。
func (f *fakeCache) warm(s *model.PlaybackSession) {
	cp := *s
	f.sessions[keySession(s.SessionId)] = &cp
}

// cached 读缓存里的会话副本；miss 返回 nil。
func (f *fakeCache) cached(sessionID string) *model.PlaybackSession {
	s, ok := f.sessions[keySession(sessionID)]
	if !ok {
		return nil
	}
	cp := *s
	return &cp
}

func (f *fakeCache) count(contentType, contentID int64) int64 {
	return f.counts[keyPlayCount(contentType, contentID)]
}

// --- 会话 model 替身 ---

type fakeSessionModel struct {
	faultInjector
	log   *callLog
	rows  map[string]*model.PlaybackSession
	byReq map[string]string
	// pendingReveal 是「并发重放」模拟：本次 Insert 命中它时才把行放进库里，
	// 从而让 logic 的第一次 FindByRequest 读不到、Insert 报唯一键冲突、第二次读得到。
	pendingReveal *model.PlaybackSession
}

func newFakeSessionModel(log *callLog) *fakeSessionModel {
	return &fakeSessionModel{log: log, rows: map[string]*model.PlaybackSession{}, byReq: map[string]string{}}
}

// Insert 复刻真实 SQL 的 uniq_request_id / PRIMARY 冲突口径：命中唯一键返回
// ErrDuplicateRequest（等价于 ON DUPLICATE KEY UPDATE mtime=mtime 的 0 行受影响）。
func (f *fakeSessionModel) Insert(_ context.Context, s *model.PlaybackSession) error {
	f.log.add("session.Insert:%s/%s", s.SessionId, s.RequestId)
	if err := f.fail("Insert"); err != nil {
		return err
	}
	if f.pendingReveal != nil {
		f.reveal()
		return model.ErrDuplicateRequest
	}
	return f.put(s)
}

// put 是冲突语义的实际实现；seedSession 直接调它来布数据（不记轨迹），
// 这样「布了多少行」不会污染对**被测代码**调用序列的断言。
func (f *fakeSessionModel) put(s *model.PlaybackSession) error {
	if _, ok := f.rows[s.SessionId]; ok {
		return model.ErrDuplicateRequest
	}
	if _, ok := f.byReq[s.RequestId]; ok {
		return model.ErrDuplicateRequest
	}
	cp := *s
	f.rows[s.SessionId] = &cp
	f.byReq[s.RequestId] = s.SessionId
	return nil
}

func (f *fakeSessionModel) reveal() {
	p := f.pendingReveal
	f.pendingReveal = nil
	f.rows[p.SessionId] = p
	f.byReq[p.RequestId] = p.SessionId
}

func (f *fakeSessionModel) FindOne(_ context.Context, sessionID string) (*model.PlaybackSession, error) {
	f.log.add("session.FindOne:%s", sessionID)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[sessionID]
	if !ok {
		return nil, nil // 与 model 一致：查无此行返回 (nil, nil)
	}
	cp := *row
	return &cp, nil
}

func (f *fakeSessionModel) FindByRequest(_ context.Context, requestID string) (*model.PlaybackSession, error) {
	f.log.add("session.FindByRequest:%s", requestID)
	if err := f.fail("FindByRequest"); err != nil {
		return nil, err
	}
	id, ok := f.byReq[requestID]
	if !ok {
		return nil, nil
	}
	cp := *f.rows[id]
	return &cp, nil
}

// MarkExpired 复刻 UPDATE ... WHERE session_id=? AND state=active AND expire_at<=?：
// 不满足门槛时受影响 0 行，也不算错误。
func (f *fakeSessionModel) MarkExpired(_ context.Context, sessionID string, now int64) error {
	f.log.add("session.MarkExpired:%s", sessionID)
	if err := f.fail("MarkExpired"); err != nil {
		return err
	}
	if row, ok := f.rows[sessionID]; ok && row.State == model.SessionStateActive && row.ExpireAt <= now {
		row.State = model.SessionStateExpired
		row.Mtime = now
	}
	return nil
}

func (f *fakeSessionModel) RevokeByContent(_ context.Context, contentType int32, contentID int64) (int64, error) {
	f.log.add("session.RevokeByContent:%d/%d", contentType, contentID)
	if err := f.fail("RevokeByContent"); err != nil {
		return 0, err
	}
	var aff int64
	for _, row := range f.rows {
		if row.ContentType == contentType && row.ContentId == contentID && row.State == model.SessionStateActive {
			row.State = model.SessionStateRevoked
			aff++
		}
	}
	return aff, nil
}

// state 读库内当前状态（值读，避免用例误改库存行）。
func (f *fakeSessionModel) state(sessionID string) int32 {
	row, ok := f.rows[sessionID]
	if !ok {
		return -1
	}
	return row.State
}

func (f *fakeSessionModel) get(sessionID string) *model.PlaybackSession {
	row, ok := f.rows[sessionID]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

func (f *fakeSessionModel) only() *model.PlaybackSession {
	if len(f.rows) != 1 {
		panic(fmt.Sprintf("期望库里只有 1 行会话，实际 %d 行", len(f.rows)))
	}
	for _, row := range f.rows {
		cp := *row
		return &cp
	}
	panic("unreachable")
}

// --- 进度 model 替身 ---

type fakeProgressModel struct {
	faultInjector
	log     *callLog
	rows    map[string]*model.PlaybackProgress
	nextID  int64
	gotTx   map[string]bool // Upsert 是否拿到事务会话
	upserts int
}

func newFakeProgressModel(log *callLog) *fakeProgressModel {
	return &fakeProgressModel{log: log, rows: map[string]*model.PlaybackProgress{}, gotTx: map[string]bool{}}
}

// Upsert 复刻 ON DUPLICATE KEY UPDATE 口径：position_ms 取 GREATEST（只前进不回退），
// buffer_count 累加，duration_ms/avg_bitrate/last_error/mtime 以最新心跳为准。
func (f *fakeProgressModel) Upsert(_ context.Context, tx sqlx.Session, p *model.PlaybackProgress) (*model.PlaybackProgress, error) {
	f.log.add("progress.Upsert:%s/%d", p.SessionId, p.PositionMs)
	f.upserts++
	if err := f.fail("Upsert"); err != nil {
		return nil, err
	}
	f.gotTx[p.SessionId] = tx != nil
	ctime := p.Ctime
	if ctime == 0 {
		ctime = time.Now().Unix()
	}
	row, ok := f.rows[p.SessionId]
	if !ok {
		f.nextID++
		stored := *p
		stored.ID = f.nextID
		stored.Ctime = ctime
		stored.Mtime = ctime
		f.rows[p.SessionId] = &stored
		row = &stored
	} else {
		if p.PositionMs > row.PositionMs {
			row.PositionMs = p.PositionMs
		}
		row.DurationMs = p.DurationMs
		row.BufferCount += p.BufferCount
		row.AvgBitrate = p.AvgBitrate
		row.LastError = p.LastError
		row.Mtime = ctime
	}
	cp := *row
	return &cp, nil
}

func (f *fakeProgressModel) FindOne(_ context.Context, sessionID string) (*model.PlaybackProgress, error) {
	f.log.add("progress.FindOne:%s", sessionID)
	if err := f.fail("FindOne"); err != nil {
		return nil, err
	}
	row, ok := f.rows[sessionID]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

// FindLatestByMid 复刻 WHERE mid=? AND content_type=? AND content_id=? ORDER BY mtime DESC LIMIT 1。
// 真实 SQL 在 mtime 相等时不确定，因此用例布景必须给出不同的 mtime。
func (f *fakeProgressModel) FindLatestByMid(_ context.Context, mid int64, contentType int32, contentID int64) (*model.PlaybackProgress, error) {
	f.log.add("progress.FindLatestByMid:%d/%d/%d", mid, contentType, contentID)
	if err := f.fail("FindLatestByMid"); err != nil {
		return nil, err
	}
	var best *model.PlaybackProgress
	for _, row := range f.rows {
		if row.Mid != mid || row.ContentType != contentType || row.ContentId != contentID {
			continue
		}
		if best == nil || row.Mtime > best.Mtime {
			cp := *row
			best = &cp
		}
	}
	return best, nil
}

func (f *fakeProgressModel) get(sessionID string) *model.PlaybackProgress {
	row, ok := f.rows[sessionID]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

func (f *fakeProgressModel) only() *model.PlaybackProgress {
	if len(f.rows) != 1 {
		panic(fmt.Sprintf("期望库里只有 1 行进度，实际 %d 行", len(f.rows)))
	}
	for _, row := range f.rows {
		cp := *row
		return &cp
	}
	panic("unreachable")
}

// --- Outbox model 替身 ---

type fakeOutboxModel struct {
	faultInjector
	log    *callLog
	rows   []*model.PlaybackOutbox
	gotTx  bool
	nextID int64
}

func newFakeOutboxModel(log *callLog) *fakeOutboxModel {
	return &fakeOutboxModel{log: log}
}

func (f *fakeOutboxModel) Insert(_ context.Context, tx sqlx.Session, out *model.PlaybackOutbox) error {
	f.log.add("outbox.Insert:%s/%s", out.EventType, out.AggregateID)
	f.gotTx = tx != nil
	if err := f.fail("Insert"); err != nil {
		return err
	}
	f.nextID++
	cp := *out
	cp.ID = f.nextID
	f.rows = append(f.rows, &cp)
	return nil
}

func (f *fakeOutboxModel) ListPending(_ context.Context, now int64, limit int32) ([]*model.PlaybackOutbox, error) {
	f.log.add("outbox.ListPending:%d/%d", now, limit)
	if err := f.fail("ListPending"); err != nil {
		return nil, err
	}
	var out []*model.PlaybackOutbox
	for _, row := range f.rows {
		if row.State == model.OutboxStatePending && row.NextRetryAt <= now {
			cp := *row
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeOutboxModel) MarkPublished(_ context.Context, id, _ int64) error {
	f.log.add("outbox.MarkPublished:%d", id)
	return f.fail("MarkPublished")
}

func (f *fakeOutboxModel) MarkRetry(_ context.Context, id int64, retryCount int32, nextRetryAt int64, lastError string) error {
	f.log.add("outbox.MarkRetry:%d", id)
	if err := f.fail("MarkRetry"); err != nil {
		return err
	}
	for _, row := range f.rows {
		if row.ID == id {
			row.RetryCount = retryCount
			row.NextRetryAt = nextRetryAt
			row.LastError = lastError
		}
	}
	return nil
}

func (f *fakeOutboxModel) MarkFailed(_ context.Context, id int64, lastError string) error {
	f.log.add("outbox.MarkFailed:%d", id)
	if err := f.fail("MarkFailed"); err != nil {
		return err
	}
	for _, row := range f.rows {
		if row.ID == id {
			row.State = model.OutboxStateFailed
			row.LastError = lastError
		}
	}
	return nil
}

func (f *fakeOutboxModel) only() *model.PlaybackOutbox {
	if len(f.rows) != 1 {
		panic(fmt.Sprintf("期望 outbox 只有 1 行，实际 %d 行", len(f.rows)))
	}
	cp := *f.rows[0]
	return &cp
}

// --- 事务连接替身 ---

// txSession 只是「拿到了事务会话」的凭证；fake models 只判断它非 nil。
type txSession struct{ sqlx.Session }

// fakeConn 只实现 playback 用到的 TransactCtx。其它方法落在嵌入的 nil 接口上会直接 panic，
// 等价于「logic 单测路径上不允许出现任何直连 SQL」——真出现了就是越界，让它炸出来。
type fakeConn struct {
	sqlx.SqlConn
	transactions int
	rolledBack   int
}

func (f *fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	f.transactions++
	if err := fn(ctx, &txSession{}); err != nil {
		f.rolledBack++
		return err
	}
	return nil
}

var _ sqlx.SqlConn = (*fakeConn)(nil)

// --- rights 替身 ---

// fakeRights 实现 repository.RightsChecker。
// 注意入参编号：CheckPlayable 的 contentType 是 **playback 口径**（1=UGC、2=PGC），
// 到 rights 契约枚举的映射发生在真实 rightsClient 内部，由 repository 包的用例锁定。
// 默认 playable=false：忘记布景的用例会被判「不可播」而失败，不会静默放过。
type fakeRights struct {
	log      *callLog
	calls    []string
	playable bool
	endTime  int64
	err      error
}

func (f *fakeRights) CheckPlayable(_ context.Context, contentID int64, contentType int32, region string) (bool, int64, error) {
	f.log.add("rights.CheckPlayable:%d/%d/%s", contentID, contentType, region)
	f.calls = append(f.calls, fmt.Sprintf("%d/%d/%s", contentID, contentType, region))
	if f.err != nil {
		return false, 0, f.err
	}
	return f.playable, f.endTime, nil
}

// --- 装配 ---

type store struct {
	log      *callLog
	cache    *fakeCache
	sessions *fakeSessionModel
	progress *fakeProgressModel
	outbox   *fakeOutboxModel
	conn     *fakeConn
	repo     *repository.Repository
}

func newStore() *store {
	log := &callLog{}
	st := &store{
		log:      log,
		cache:    newFakeCache(log),
		sessions: newFakeSessionModel(log),
		progress: newFakeProgressModel(log),
		outbox:   newFakeOutboxModel(log),
		conn:     &fakeConn{},
	}
	st.repo = repository.NewWithDeps(st.cache, st.conn, st.sessions, st.progress, st.outbox)
	return st
}

// env 是一次用例的完整运行时：真实 Repository（依赖为替身）+ 真实 Signer + rights 替身。
// Config 留空零值即可：这 4 个 logic 只读 Repository/Rights/Signer 三个字段。
type env struct {
	t      *testing.T
	st     *store
	rights *fakeRights
	svcCtx *svc.ServiceContext
}

// newEnv 装配启用防盗链签名的运行时（等价于生产/预发配置）。
func newEnv(t *testing.T) *env { return newEnvSigner(t, testPrivateKey, true) }

// newEnvUnsigned 装配关闭签名的运行时（等价于 dev/test：Signer.Signed()==false）。
func newEnvUnsigned(t *testing.T) *env { return newEnvSigner(t, testPrivateKey, false) }

// newEnvSigner 装配指定私钥与开关的运行时。privateKey 为空串用来复现「私钥未配置」事故场景。
func newEnvSigner(t *testing.T, privateKey string, enableAuthKey bool) *env {
	t.Helper()
	st := newStore()
	signer, err := signurl.New(signurl.Config{
		BaseURL:       testBaseURL,
		PrivateKey:    privateKey,
		KeyID:         testKeyID,
		TokenTTL:      testTokenTTL,
		EnableAuthKey: enableAuthKey,
		AllowUnsigned: !enableAuthKey,
	})
	if err != nil {
		t.Fatalf("构造 Signer 失败：%v", err)
	}
	r := &fakeRights{log: st.log}
	return &env{
		t:      t,
		st:     st,
		rights: r,
		svcCtx: &svc.ServiceContext{Repository: st.repo, Rights: r, Signer: signer},
	}
}

// authKeyFor 用与生产一致的 A 型规则算出某 URI 在 expireAt 时刻的合法 auth_key。
// 走 signurl.Hash 而不是解析逻辑返回值，才能表达「CDN 侧独立重算签名」这一外部事实。
func authKeyFor(t *testing.T, uri string, expireAt int64, uid, rand string) string {
	t.Helper()
	if uid == "" {
		uid = "0"
	}
	return fmt.Sprintf("%d-%s-%s-%s", expireAt, rand, uid, signurl.Hash(uri, expireAt, rand, uid, testPrivateKey))
}

// seedSession 静默布一行会话（唯一索引口径仍生效；不写轨迹，见纪律 4）。
// 返回库里那一行的副本。
func seedSession(t *testing.T, st *store, s *model.PlaybackSession) *model.PlaybackSession {
	t.Helper()
	if err := st.sessions.put(s); err != nil {
		t.Fatalf("布景写入会话失败：%v", err)
	}
	return st.sessions.get(s.SessionId)
}

// seedProgress 静默布一行播放进度。
func seedProgress(t *testing.T, st *store, p *model.PlaybackProgress) *model.PlaybackProgress {
	t.Helper()
	st.progress.nextID++
	p.ID = st.progress.nextID
	stored := *p
	st.progress.rows[p.SessionId] = &stored
	return st.progress.get(p.SessionId)
}

// nowPlus 相对真实 now 偏移若干秒：logic 内部用 time.Now()（不可注入），
// 布景只留安全余量，不依赖秒级边界（见本文件头纪律与 memory 里的墙钟教训）。
func nowPlus(delta int64) int64 { return time.Now().Unix() + delta }
