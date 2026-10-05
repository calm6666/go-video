package logic

// fakes_test.go 是 upload logic 测试的内存替身集合。
//
// 为什么需要注入缝：upload 的 ServiceContext.Repository 是具体类型
// *repository.Repository，生产构造 repository.New 走真 Redis + 真 MySQL，测试无处塞替身。
// 因此本包用例统一用 repository.NewWithDeps(内存缓存, fakeConn, 内存 sessionMd,
// 内存 chunkMd, 内存 outboxMd, 假 OSS 配置) 组装**真实的 Repository**，只把它的
// 5 个依赖与连接换成替身。这样一来
// 「缓存快路径是否跳过 DB、状态机门槛、分片清单校验、完成/取消的写入次序、
// 完成上传的四写是否同事务」整条判定链都在被测路径上，而不是把 Repository 也 mock 掉。
//
// 四条替身纪律（comment / rights / playback / transcode 几轮踩过的坑，这里逐条守住）：
//  1. 每次读都返回**值拷贝**：logic/repository 的写回不得污染库存行，否则
//     「有没有真的落库」这类断言会被共享指针掩盖。
//  2. 副作用按**顺序**记进同一个 callLog（四个 model 替身共享），断言序列而不是次数：
//     upload 要紧的结论是「签发前打了几次 DB」「取消半途失败留下的是活会话还是死清单」。
//  3. 每步先记录再判故障：失败的那一次调用同样出现在序列里，否则「打到了依赖才失败」
//     和「根本没打依赖」两种形态在断言里长得一样。
//  4. 布数据走替身的**静默写入路径**（warm / put / seedOne），布景不进 callLog。
//
// 与 model/*.go 的 SQL 语义逐条对齐：
//   - session.UpdateState（及 UpdateStateTx）在 RowsAffected==0 时返回 ErrUploadNotFound，
//     而 UpdateAssetId / SetMd5 不检查影响行（真 SQL 更新 0 行也不报错）；
//   - 三个写各有 Tx 变体：repository 的事务回调只能拿到 Tx 版本，非 Tx 版本在 model 层
//     就是「以 conn 为会话」的同一实现，因此替身对两者记**不同**的 op 名；
//   - chunk.MarkUploaded 在行不存在时返回 ErrChunkNotFound；
//   - chunk.InsertBatch 对空清单直接 return，不发任何 SQL；
//   - chunk.ListByUpload 带 ORDER BY chunk_no ASC；
//   - outbox.Insert 只记一行，并把「是否拿到事务会话」留在 gotTx 上；
//   - model 的 FindOne 语义是「不存在返回 (nil, nil)」，不是 sql.ErrNoRows。
//
// 覆盖边界（如实声明）：替身只复刻 model 层 SQL 的**语义**，不证明 SQL 与列名本身；
// `services/upload/model/uploadmodel.go`、`uploadchunkmodel.go`、`uploadoutboxmodel.go`
// 的占位符个数，`upload_session`/`upload_chunk`/`upload_outbox` 的索引与唯一约束、
// 以及 chunk.id/outbox.id 的 AUTO_INCREMENT 都无法在纯内存用例里证伪
// （替身按代码意图自增分配 id）。fakeConn 也不会模拟 MySQL 的回滚：
// 它只能证明「几次写落在同一次 TransactCtx 的会话上、失败时事务被标记回滚」，
// 不能证明「已写入的行会被数据库撤回」。OSS 侧 CompleteMultipartUpload/AbortMultipartUpload
// 仍是 TODO 占位，因此本包**不可能**断言到真实分片合并；
// media.task.v1 的投递（internal/publisher + `-tags upload_kafka`）不在本包路径上，
// 由 publisher 包的用例锁定。相关缺口登记在 services/upload/README.md。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/upload/internal/repository"
	"go-video/services/upload/internal/svc"
	"go-video/services/upload/model"
	rpc "go-video/services/upload/rpc"
)

// 布景用固定时间：早于用例运行时刻，这样「写操作把 mtime 推进了」可断言。
const seedCtime int64 = 1772704800 // 2026-03-05

const (
	testBucket   = "hilizi-ugc-upload"
	testEndpoint = "oss-cn-test.example.com"
	testTTL      = int64(900) // 15 分钟，与仓库默认值一致
)

func defaultOSS() repository.OSSConfig {
	return repository.OSSConfig{Bucket: testBucket, Endpoint: testEndpoint, PresignTTLSeconds: testTTL}
}

// --- 调用序列记录 ---

type callLog struct{ ops []string }

func (l *callLog) rec(op string) { l.ops = append(l.ops, op) }

// assert 断言**完整顺序**。只断言次数会把「缓存命中跳过 DB」和
// 「先删清单再改状态」这类次序结论掩盖掉，所以这里是整序列比较。
func (l *callLog) assert(t *testing.T, want ...string) {
	t.Helper()
	got := l.snapshot()
	if got != joinLines(want) {
		t.Fatalf("依赖调用序列不符\n--- want ---\n%s\n--- got ---\n%s\n", joinLines(want), got)
	}
}

func (l *callLog) assertEmpty(t *testing.T) {
	t.Helper()
	if len(l.ops) != 0 {
		t.Fatalf("门槛校验阶段不该打任何依赖，实际打了：\n%s", l.snapshot())
	}
}

func (l *callLog) snapshot() string { return joinLines(l.ops) }

func joinLines(ss []string) string {
	if len(ss) == 0 {
		return "<none>"
	}
	return strings.Join(ss, "\n  ")
}

// --- 缓存替身（repository.Cacher） ---

type fakeCache struct {
	log        *callLog
	states     map[string]int32
	getErr     error
	getErrCall int // >0 时 getErr 只在该次 GetSession 上生效
	getCalls   int
	setErr     error
	delErr     error
}

var _ repository.Cacher = (*fakeCache)(nil)

func newFakeCache(l *callLog) *fakeCache {
	return &fakeCache{log: l, states: map[string]int32{}}
}

func (c *fakeCache) Ping(context.Context) error {
	c.log.rec("cache.Ping")
	return nil
}

func (c *fakeCache) SetSession(_ context.Context, uploadID string, state int32) error {
	c.log.rec(fmt.Sprintf("cache.SetSession:%s=%d", uploadID, state))
	if c.setErr != nil {
		return c.setErr // 真 Redis 写失败不会留下缓存项
	}
	c.states[uploadID] = state
	return nil
}

func (c *fakeCache) GetSession(_ context.Context, uploadID string) (int32, bool, error) {
	c.log.rec("cache.GetSession:" + uploadID)
	c.getCalls++
	if c.getErr != nil && (c.getErrCall == 0 || c.getCalls == c.getErrCall) {
		return 0, false, c.getErr
	}
	state, ok := c.states[uploadID]
	return state, ok, nil
}

func (c *fakeCache) DelSession(_ context.Context, uploadID string) error {
	c.log.rec("cache.DelSession:" + uploadID)
	if c.delErr != nil {
		return c.delErr
	}
	delete(c.states, uploadID)
	return nil
}

// warm 静默布缓存，不记 callLog。
func (c *fakeCache) warm(uploadID string, state int32) { c.states[uploadID] = state }

func (c *fakeCache) lookup(uploadID string) (int32, bool) {
	s, ok := c.states[uploadID]
	return s, ok
}

// --- upload_session 替身 ---

type fakeSessions struct {
	log            *callLog
	byID           map[string]*model.UploadSession
	insertErr      error
	findErr        error
	findErrCall    int // >0 时 findErr 只在该次 FindOne 上生效
	findCalls      int
	updateStateErr error
	setAssetErr    error
	setMd5Err      error
	// txWrites 记录「以事务会话为参数」的写次数：CompleteUpload 的三次会话写
	// 必须全部走 Tx 变体并拿到非 nil 会话，否则它们根本不在同一事务里。
	txWrites int
}

func newFakeSessions(l *callLog) *fakeSessions {
	return &fakeSessions{log: l, byID: map[string]*model.UploadSession{}}
}

var _ model.UploadSessionModel = (*fakeSessions)(nil)

func (s *fakeSessions) Insert(_ context.Context, ssn *model.UploadSession) error {
	s.log.rec("sessions.Insert")
	if s.insertErr != nil {
		return s.insertErr
	}
	cp := *ssn
	s.byID[cp.UploadId] = &cp
	return nil
}

func (s *fakeSessions) FindOne(_ context.Context, uploadID string) (*model.UploadSession, error) {
	s.log.rec("sessions.FindOne:" + uploadID)
	s.findCalls++
	if s.findErr != nil && (s.findErrCall == 0 || s.findCalls == s.findErrCall) {
		return nil, s.findErr
	}
	row, ok := s.byID[uploadID]
	if !ok {
		return nil, nil // 与 defaultUploadSessionModel 一致：无行不等于错误
	}
	cp := *row
	return &cp, nil
}

func (s *fakeSessions) UpdateState(_ context.Context, uploadID string, state int32) error {
	s.log.rec(fmt.Sprintf("sessions.UpdateState:%s=%d", uploadID, state))
	return s.applyState(nil, uploadID, state)
}

func (s *fakeSessions) UpdateStateTx(_ context.Context, tx sqlx.Session, uploadID string, state int32) error {
	s.log.rec(fmt.Sprintf("sessions.UpdateStateTx:%s=%d", uploadID, state))
	return s.applyState(tx, uploadID, state)
}

// applyState 是两个变体的共同实现（model 侧的非 Tx 方法就是「以 conn 为会话」的同一实现）。
// RowsAffected==0 → ErrUploadNotFound 的语义与 defaultUploadSessionModel 一致。
func (s *fakeSessions) applyState(tx sqlx.Session, uploadID string, state int32) error {
	if tx != nil {
		s.txWrites++
	}
	if s.updateStateErr != nil {
		return s.updateStateErr
	}
	row, ok := s.byID[uploadID]
	if !ok {
		return model.ErrUploadNotFound // 真 SQL 的 RowsAffected==0 分支
	}
	row.State = state
	row.Mtime = time.Now().Unix()
	return nil
}

func (s *fakeSessions) UpdateAssetId(_ context.Context, uploadID, assetID string) error {
	s.log.rec(fmt.Sprintf("sessions.UpdateAssetId:%s=%s", uploadID, assetID))
	return s.applyAssetId(nil, uploadID, assetID)
}

func (s *fakeSessions) UpdateAssetIdTx(_ context.Context, tx sqlx.Session, uploadID, assetID string) error {
	s.log.rec(fmt.Sprintf("sessions.UpdateAssetIdTx:%s=%s", uploadID, assetID))
	return s.applyAssetId(tx, uploadID, assetID)
}

// applyAssetId 不检查影响行：与 model 一致（真 SQL 更新 0 行也不报错）。
func (s *fakeSessions) applyAssetId(tx sqlx.Session, uploadID, assetID string) error {
	if tx != nil {
		s.txWrites++
	}
	if s.setAssetErr != nil {
		return s.setAssetErr
	}
	if row, ok := s.byID[uploadID]; ok {
		row.AssetId = assetID
		row.Mtime = time.Now().Unix()
	}
	return nil
}

func (s *fakeSessions) SetMd5(_ context.Context, uploadID, md5 string) error {
	s.log.rec(fmt.Sprintf("sessions.SetMd5:%s=%s", uploadID, md5))
	return s.applyMd5(nil, uploadID, md5)
}

func (s *fakeSessions) SetMd5Tx(_ context.Context, tx sqlx.Session, uploadID, md5 string) error {
	s.log.rec(fmt.Sprintf("sessions.SetMd5Tx:%s=%s", uploadID, md5))
	return s.applyMd5(tx, uploadID, md5)
}

func (s *fakeSessions) applyMd5(tx sqlx.Session, uploadID, md5 string) error {
	if tx != nil {
		s.txWrites++
	}
	if s.setMd5Err != nil {
		return s.setMd5Err
	}
	if row, ok := s.byID[uploadID]; ok {
		row.Md5 = md5
		row.Mtime = time.Now().Unix()
	}
	return nil
}

func (s *fakeSessions) put(ssn *model.UploadSession) {
	cp := *ssn
	s.byID[cp.UploadId] = &cp
}

func (s *fakeSessions) get(uploadID string) (*model.UploadSession, bool) {
	row, ok := s.byID[uploadID]
	if !ok {
		return nil, false
	}
	cp := *row
	return &cp, true
}

func (s *fakeSessions) all() []*model.UploadSession {
	out := make([]*model.UploadSession, 0, len(s.byID))
	for _, row := range s.byID {
		cp := *row
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UploadId < out[j].UploadId })
	return out
}

// --- upload_chunk 替身 ---

type fakeChunks struct {
	log         *callLog
	byUpload    map[string][]*model.UploadChunk
	nextID      int64
	insertErr   error
	listErr     error
	markErr     error
	markErrCall int // >0 时 markErr 只在该次 MarkUploaded 上生效
	markCalls   int
	deleteErr   error
}

func newFakeChunks(l *callLog) *fakeChunks {
	return &fakeChunks{log: l, byUpload: map[string][]*model.UploadChunk{}}
}

var _ model.UploadChunkModel = (*fakeChunks)(nil)

func (c *fakeChunks) InsertBatch(_ context.Context, chunks []*model.UploadChunk) error {
	if len(chunks) == 0 {
		return nil // 真实现空清单直接 return，不发 SQL
	}
	uploadID := chunks[0].UploadId
	c.log.rec(fmt.Sprintf("chunks.InsertBatch:%s=%d", uploadID, len(chunks)))
	if c.insertErr != nil {
		return c.insertErr
	}
	for _, ch := range chunks {
		c.nextID++
		cp := *ch
		cp.Id = c.nextID // 替身按 AUTO_INCREMENT 意图分配 id，不证明迁移里该列确实自增
		c.byUpload[cp.UploadId] = append(c.byUpload[cp.UploadId], &cp)
	}
	return nil
}

func (c *fakeChunks) ListByUpload(_ context.Context, uploadID string) ([]*model.UploadChunk, error) {
	c.log.rec("chunks.ListByUpload:" + uploadID)
	if c.listErr != nil {
		return nil, c.listErr
	}
	return c.rows(uploadID), nil // 副本 + 升序，对应 ORDER BY chunk_no ASC
}

func (c *fakeChunks) FindOne(_ context.Context, uploadID string, chunkNo int32) (*model.UploadChunk, error) {
	c.log.rec(fmt.Sprintf("chunks.FindOne:%s=%d", uploadID, chunkNo))
	for _, row := range c.byUpload[uploadID] {
		if row.ChunkNo == chunkNo {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

func (c *fakeChunks) MarkUploaded(_ context.Context, uploadID string, chunkNo int32, etag string) error {
	c.log.rec(fmt.Sprintf("chunks.MarkUploaded:%s=%d", uploadID, chunkNo))
	c.markCalls++
	if c.markErr != nil && (c.markErrCall == 0 || c.markCalls == c.markErrCall) {
		return c.markErr
	}
	for _, row := range c.byUpload[uploadID] {
		if row.ChunkNo == chunkNo {
			row.Etag = etag
			row.State = model.ChunkStateUploaded
			row.Mtime = time.Now().Unix()
			return nil
		}
	}
	return model.ErrChunkNotFound // 真 SQL 的 RowsAffected==0 分支
}

func (c *fakeChunks) DeleteByUpload(_ context.Context, uploadID string) error {
	c.log.rec("chunks.DeleteByUpload:" + uploadID)
	if c.deleteErr != nil {
		return c.deleteErr
	}
	delete(c.byUpload, uploadID)
	return nil
}

// seedOne 静默布一个分片行。
func (c *fakeChunks) seedOne(uploadID string, chunkNo int32, size int64, state int32, etag string) {
	c.nextID++
	c.byUpload[uploadID] = append(c.byUpload[uploadID], &model.UploadChunk{
		Id: c.nextID, UploadId: uploadID, ChunkNo: chunkNo, Size: size,
		Etag: etag, State: state, Ctime: seedCtime, Mtime: seedCtime,
	})
}

// rows 返回按 chunk_no 升序的副本。
func (c *fakeChunks) rows(uploadID string) []*model.UploadChunk {
	out := make([]*model.UploadChunk, 0, len(c.byUpload[uploadID]))
	for _, row := range c.byUpload[uploadID] {
		cp := *row
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ChunkNo < out[j].ChunkNo })
	return out
}

func (c *fakeChunks) count(uploadID string) int { return len(c.byUpload[uploadID]) }

// --- upload_outbox 替身 ---

// fakeOutbox 只实现 CompleteUpload 用到的 Insert。其余方法落在嵌入的 nil 接口上会直接 panic：
// logic 路径上出现 ListPending/Mark* 就说明「上传接口在自己投事件」，绕过了发布循环。
type fakeOutbox struct {
	model.UploadOutboxModel
	log    *callLog
	rows   []*model.UploadOutbox
	gotTx  bool // Insert 是否拿到了事务会话（false = repository 没走 TransactCtx）
	insErr error
	nextID int64
}

var _ model.UploadOutboxModel = (*fakeOutbox)(nil)

func newFakeOutbox(l *callLog) *fakeOutbox {
	return &fakeOutbox{log: l}
}

func (f *fakeOutbox) Insert(_ context.Context, tx sqlx.Session, out *model.UploadOutbox) error {
	f.log.rec(fmt.Sprintf("outbox.Insert:%s/%s", out.EventType, out.AggregateID))
	f.gotTx = tx != nil
	if f.insErr != nil {
		return f.insErr
	}
	f.nextID++
	cp := *out
	cp.ID = f.nextID
	f.rows = append(f.rows, &cp)
	return nil
}

// only 取唯一一行；多行说明同一次完成写了重复事件。
func (f *fakeOutbox) only(t *testing.T) *model.UploadOutbox {
	t.Helper()
	if len(f.rows) != 1 {
		t.Fatalf("upload_outbox 期望恰好 1 行，实际 %d 行", len(f.rows))
	}
	cp := *f.rows[0]
	return &cp
}

// --- 事务连接替身 ---

// txSession 只是「拿到了事务会话」的凭证；model 替身只判断它非 nil。
// 任何方法调用都会在嵌入的 nil 接口上 panic，这正是我们想要的：
// 替身路径上不允许出现「绕过 model 直接对 tx 发 SQL」。
type txSession struct{ sqlx.Session }

// fakeConn 只实现 upload 用到的 TransactCtx。其它方法落在嵌入的 nil 接口上会直接 panic，
// 等价于「logic 单测路径上不允许出现任何直连 SQL」；真出现了就是越界，让它炸出来。
type fakeConn struct {
	sqlx.SqlConn
	transactions int
	rolledBack   int
	transactErr  error // 非 nil 时 TransactCtx 在进入回调前就失败（复刻「开事务就失败」）
}

func (f *fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	f.transactions++
	if f.transactErr != nil {
		f.rolledBack++
		return f.transactErr
	}
	if err := fn(ctx, &txSession{}); err != nil {
		f.rolledBack++
		return err
	}
	return nil
}

var _ sqlx.SqlConn = (*fakeConn)(nil)

// --- 装配 ---

type fixture struct {
	log      *callLog
	cache    *fakeCache
	sessions *fakeSessions
	chunks   *fakeChunks
	outbox   *fakeOutbox
	conn     *fakeConn
	svcCtx   *svc.ServiceContext
	oss      repository.OSSConfig
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureOSS(t, defaultOSS())
}

func newFixtureOSS(t *testing.T, oss repository.OSSConfig) *fixture {
	t.Helper()
	lg := &callLog{}
	f := &fixture{
		log:      lg,
		cache:    newFakeCache(lg),
		sessions: newFakeSessions(lg),
		chunks:   newFakeChunks(lg),
		outbox:   newFakeOutbox(lg),
		conn:     &fakeConn{},
		oss:      oss,
	}
	f.svcCtx = &svc.ServiceContext{
		Repository: repository.NewWithDeps(f.cache, f.conn, f.sessions, f.chunks, f.outbox, oss),
	}
	return f
}

// seedSession 静默布一个会话及其 PENDING 分片清单，返回库存行副本。
func (f *fixture) seedSession(uploadID string, state, totalChunks int32, chunkSize int64) *model.UploadSession {
	ssn := &model.UploadSession{
		UploadId:    uploadID,
		Mid:         1001,
		Filename:    "demo.mp4",
		Size:        int64(totalChunks) * chunkSize,
		Typeid:      16,
		Bucket:      f.oss.Bucket,
		ObjectKey:   fmt.Sprintf("uploads/1001/%s/demo.mp4", uploadID),
		State:       state,
		ChunkSize:   chunkSize,
		TotalChunks: totalChunks,
		Md5:         "stored-md5",
		Ctime:       seedCtime,
		Mtime:       seedCtime,
	}
	f.sessions.put(ssn)
	for i := int32(1); i <= totalChunks; i++ {
		f.chunks.seedOne(uploadID, i, chunkSize, model.ChunkStatePending, "")
	}
	cp := *ssn
	return &cp
}

func (f *fixture) session(t *testing.T, uploadID string) *model.UploadSession {
	t.Helper()
	row, ok := f.sessions.get(uploadID)
	if !ok {
		t.Fatalf("upload_session 里没有 %s", uploadID)
	}
	return row
}

func (f *fixture) mustNoSession(t *testing.T, uploadID string) {
	t.Helper()
	if _, ok := f.sessions.get(uploadID); ok {
		t.Fatalf("不该写出 upload_session 行，却有：%+v", f.sessions.byID[uploadID])
	}
}

// uploadedSize 按 logic 的同一口径（state>=UPLOADED 的分片大小之和）从库里算，
// 用来把「应答里的进度」和「库里的分片行」对起来，而不是两个都信替身。
func (f *fixture) uploadedSize(uploadID string) int64 {
	var total int64
	for _, c := range f.chunks.rows(uploadID) {
		if c.State >= model.ChunkStateUploaded {
			total += c.Size
		}
	}
	return total
}

// --- 5 个构造器的调用入口（ctx 用 example 默认，本服务不吃 ctx 值） ---

func (f *fixture) initUpload(t *testing.T, in *rpc.InitUploadReq) (*rpc.InitUploadReply, error) {
	t.Helper()
	return NewInitUploadLogic(context.Background(), f.svcCtx).InitUpload(in)
}

func (f *fixture) getUploadUrl(t *testing.T, in *rpc.GetUrlReq) (*rpc.GetUrlReply, error) {
	t.Helper()
	return NewGetUploadUrlLogic(context.Background(), f.svcCtx).GetUploadUrl(in)
}

func (f *fixture) completeUpload(t *testing.T, in *rpc.CompleteUploadReq) (*rpc.CompleteUploadReply, error) {
	t.Helper()
	return NewCompleteUploadLogic(context.Background(), f.svcCtx).CompleteUpload(in)
}

func (f *fixture) abortUpload(t *testing.T, in *rpc.AbortUploadReq) (*rpc.EmptyReply, error) {
	t.Helper()
	return NewAbortUploadLogic(context.Background(), f.svcCtx).AbortUpload(in)
}

func (f *fixture) getUploadStatus(t *testing.T, in *rpc.UploadStatusReq) (*rpc.UploadStatusReply, error) {
	t.Helper()
	return NewGetUploadStatusLogic(context.Background(), f.svcCtx).GetUploadStatus(in)
}

// --- 小断言 ---

// assertUnixWindow 钉「ts 落在 want±slack」：仓库用 time.Now() 算过期时间和 mtime，
// 纯内存用例不能钉绝对秒，只能钉窗口；slack 给到 5 秒足够覆盖 CI 抖动，
// 又小到能区分「TTL 用了默认值」和「TTL 用了配置值」。
func assertUnixWindow(t *testing.T, label string, got, want, slack int64) {
	t.Helper()
	if got < want || got > want+slack {
		t.Fatalf("%s=%d，期望落在 [%d,%d]", label, got, want, want+slack)
	}
}

func mustErrIs(t *testing.T, label string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s 的错误=%v，期望 %v", label, err, want)
	}
}
