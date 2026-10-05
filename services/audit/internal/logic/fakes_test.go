package logic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/audit/internal/repository"
	"go-video/services/audit/internal/svc"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"
)

// 本文件是 logic 包的测试夹具：五个 model 接口的内存假实现 + 假对象存储 + deps 装配。
//
// 刻意不连 MySQL、不起 gRPC、不用 time.Sleep：
// 审计服务的核心承诺（链连续性、状态机、幂等、分页边界、脱敏）全都是纯判定，
// 能在内存里被钉死的东西没有理由推到集成测试里去。
// 唯一确实做不到的分支（真库的唯一键并发、SQL 文本正确性）在 README 里列为
// 「未覆盖 + 原因」，不靠放宽断言蒙过去。

// --- 时钟与固定入参 ---

// testNow 固定为 2026-03-05T10:00:00Z：链名会是 <domain>/2026-03-05，
// 与保留期/到期判定共用的时区无关（全部按 Unix 秒 + UTC 日）。
const testNow int64 = 1772704800

func testCallContext(requestID string) *rpc.CallContext {
	return &rpc.CallContext{
		CallerService: "admin-api",
		OperatorId:    7,
		TraceId:       "trace-1",
		RequestId:     requestID,
	}
}

func validDraft(eventID string) *rpc.AuditEntryDraft {
	return &rpc.AuditEntryDraft{
		EventId:      eventID,
		ActorType:    rpc.ActorType_ACTOR_TYPE_ADMIN,
		ActorId:      7,
		ActorName:    "运营小王",
		Action:       "media.approve",
		ActionDomain: "media",
		TargetType:   "work",
		TargetId:     "work-1",
		Result:       rpc.AuditResult_AUDIT_RESULT_OK,
		SourceApp:    rpc.SourceApp_SOURCE_APP_ADMIN_WEB,
		Reason:       "内容合规通过",
		OccurredAt:   testNow,
	}
}

// --- 假 audit_entry ---

type fakeEntries struct {
	byID    map[int64]*model.AuditEntry
	byEvent map[string]*model.AuditEntry
	nextID  int64

	inserted []string

	insertErr error
	findErr   error

	listRows  []*model.AuditEntry
	listTotal int64
	listErr   error
	listF     []model.EntryFilter

	rangeRows  []*model.AuditEntry
	rangeErr   error
	rangeLimit []int32
	rangeCalls []string

	scanPages  [][]*model.AuditEntry
	scanCalls  []int64
	scanLimits []int32
	scanErr    error
	scanErrAt  int // 第 N 次（从 1 计）ScanAfter 返回 scanErr

	markCalls []string
	markRange [][2]int64
	markErr   error
	marked    int64

	maxSeq int64
}

func newFakeEntries() *fakeEntries {
	return &fakeEntries{
		byID:    map[int64]*model.AuditEntry{},
		byEvent: map[string]*model.AuditEntry{},
	}
}

func (f *fakeEntries) Insert(_ context.Context, _ sqlx.Session, e *model.AuditEntry) (int64, error) {
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	if e.EventID == "" {
		return 0, model.ErrEventIDRequired
	}
	if _, ok := f.byEvent[e.EventID]; ok {
		// 真实驱动在这里抛 1062，repository 归一成唯一键冲突；
		// 假实现直接返回哨兵，足以让 logic 的「回查兜底」分支被覆盖。
		return 0, model.ErrTaskExists
	}
	f.nextID++
	row := *e
	row.EntryID = f.nextID
	f.byID[row.EntryID] = &row
	f.byEvent[row.EventID] = &row
	f.inserted = append(f.inserted, row.EventID)
	e.EntryID = row.EntryID
	if row.Seq > f.maxSeq {
		f.maxSeq = row.Seq
	}
	return row.EntryID, nil
}

func (f *fakeEntries) FindOne(_ context.Context, entryID int64) (*model.AuditEntry, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	row, ok := f.byID[entryID]
	if !ok {
		return nil, model.ErrEntryNotFound
	}
	cp := *row
	return &cp, nil
}

func (f *fakeEntries) FindByEventID(_ context.Context, eventID string) (*model.AuditEntry, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	row, ok := f.byEvent[eventID]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakeEntries) FindByEventIDs(_ context.Context, eventIDs []string) (map[string]*model.AuditEntry, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	out := make(map[string]*model.AuditEntry, len(eventIDs))
	for _, id := range eventIDs {
		row, ok := f.byEvent[id]
		if !ok {
			continue
		}
		cp := *row
		out[id] = &cp
	}
	return out, nil
}

func (f *fakeEntries) List(_ context.Context, ftr model.EntryFilter) ([]*model.AuditEntry, int64, error) {
	f.listF = append(f.listF, ftr)
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.listRows, f.listTotal, nil
}

func (f *fakeEntries) ListByChainRange(_ context.Context, chainKey string, fromSeq, toSeq int64, limit int32) ([]*model.AuditEntry, error) {
	f.rangeCalls = append(f.rangeCalls, fmt.Sprintf("%s:%d-%d", chainKey, fromSeq, toSeq))
	f.rangeLimit = append(f.rangeLimit, limit)
	if f.rangeErr != nil {
		return nil, f.rangeErr
	}
	var out []*model.AuditEntry
	for _, r := range f.rangeRows {
		if r.ChainKey != chainKey || r.Seq < fromSeq || r.Seq > toSeq {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	if limit > 0 && int32(len(out)) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeEntries) MarkArchived(_ context.Context, chainKey string, fromSeq, toSeq int64, ts int64) (int64, error) {
	f.markCalls = append(f.markCalls, chainKey)
	f.markRange = append(f.markRange, [2]int64{fromSeq, toSeq})
	if f.markErr != nil {
		return 0, f.markErr
	}
	n := int64(0)
	for _, row := range f.byID {
		if row.ChainKey == chainKey && row.Seq >= fromSeq && row.Seq <= toSeq && row.ArchivedAt == 0 {
			row.ArchivedAt = ts
			n++
		}
	}
	f.marked += n
	return n, nil
}

func (f *fakeEntries) MaxSeq(_ context.Context, chainKey string) (int64, error) {
	return f.maxSeq, nil
}

func (f *fakeEntries) ScanAfter(_ context.Context, _ model.EntryFilter, afterEntryID int64, limit int32) ([]*model.AuditEntry, error) {
	f.scanCalls = append(f.scanCalls, afterEntryID)
	f.scanLimits = append(f.scanLimits, limit)
	call := len(f.scanCalls)
	if f.scanErr != nil && f.scanErrAt == call {
		return nil, f.scanErr
	}
	if len(f.scanPages) == 0 {
		return nil, nil
	}
	page := f.scanPages[0]
	if call-1 < len(f.scanPages) {
		page = f.scanPages[call-1]
	} else {
		page = nil
	}
	out := make([]*model.AuditEntry, 0, len(page))
	for _, r := range page {
		if r.EntryID <= afterEntryID {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	if limit > 0 && int32(len(out)) > limit {
		out = out[:limit]
	}
	return out, nil
}

// snapshot/restore 让假事务具备回滚语义：事务失败后库里的东西必须回到事务前。
// 存的是值拷贝，因此 restore 之后即使调用方还持有旧指针也改不动夹具里的行。
func (f *fakeEntries) snapshot() map[int64]model.AuditEntry {
	out := make(map[int64]model.AuditEntry, len(f.byID))
	for id, r := range f.byID {
		out[id] = *r
	}
	return out
}

func (f *fakeEntries) restore(snap map[int64]model.AuditEntry) {
	f.byID = make(map[int64]*model.AuditEntry, len(snap))
	f.byEvent = make(map[string]*model.AuditEntry, len(snap))
	var maxID int64
	for id, v := range snap {
		row := v
		f.byID[id] = &row
		f.byEvent[row.EventID] = &row
		if id > maxID {
			maxID = id
		}
	}
	f.nextID = maxID
}

// fakeSession 只为满足「写入必须在事务里」这一签名约束：
// 假 model 不会调用 session 上的任何方法，真实现里 LockForUpdate 拿不到 session 会直接失败。
type fakeSession struct{ sqlx.Session }

// --- 假 audit_chain_head ---

type fakeChains struct {
	heads map[string]*model.ChainHead

	ensureErr error
	lockErr   error
	// conflicts：前 N 次 Advance 判定为「被并发抢先」，同时把链头推进一格，
	// 于是重试那一轮会读到新的 prev_hash/seq —— 正是链冲突裁决要覆盖的形状。
	conflicts    int
	advanceErr   error
	advanceCalls []int64
	conflictSeq  []int64
	ensured      []string
	locked       []string
}

func newFakeChains() *fakeChains {
	return &fakeChains{heads: map[string]*model.ChainHead{}}
}

func (f *fakeChains) Ensure(_ context.Context, _ sqlx.Session, chainKey string) error {
	if f.ensureErr != nil {
		return f.ensureErr
	}
	if _, ok := f.heads[chainKey]; !ok {
		f.heads[chainKey] = &model.ChainHead{ChainKey: chainKey}
	}
	f.ensured = append(f.ensured, chainKey)
	return nil
}

func (f *fakeChains) LockForUpdate(_ context.Context, session sqlx.Session, chainKey string) (*model.ChainHead, error) {
	if session == nil {
		return nil, model.ErrChainConflict
	}
	if f.lockErr != nil {
		return nil, f.lockErr
	}
	h, ok := f.heads[chainKey]
	if !ok {
		return nil, model.ErrChainHeadMissing
	}
	cp := *h
	f.locked = append(f.locked, chainKey)
	return &cp, nil
}

func (f *fakeChains) Advance(_ context.Context, _ sqlx.Session, chainKey string,
	expectSeq int64, head *model.ChainHead) (bool, error) {
	if f.advanceErr != nil {
		return false, f.advanceErr
	}
	f.advanceCalls = append(f.advanceCalls, expectSeq)
	cur := f.heads[chainKey]
	if cur == nil {
		return false, model.ErrChainHeadMissing
	}
	if f.conflicts > 0 {
		f.conflicts--
		f.conflictSeq = append(f.conflictSeq, expectSeq)
		// 模拟竞争者先落了一条：seq +1、尾摘要换成一个可复算的假摘要。
		cp := *cur
		cp.Seq = cur.Seq + 1
		cp.EntryCount = cur.EntryCount + 1
		cp.LastEntryID = cur.LastEntryID + 1
		sum := sha256.Sum256([]byte(fmt.Sprintf("competitor/%s/%d", chainKey, cp.Seq)))
		cp.LastHash = hex.EncodeToString(sum[:])
		f.heads[chainKey] = &cp
		return false, nil
	}
	if cur.Seq != expectSeq {
		return false, nil
	}
	cp := *head
	f.heads[chainKey] = &cp
	return true, nil
}

func (f *fakeChains) FindOne(_ context.Context, chainKey string) (*model.ChainHead, error) {
	h, ok := f.heads[chainKey]
	if !ok {
		return nil, model.ErrChainHeadMissing
	}
	cp := *h
	return &cp, nil
}

func (f *fakeChains) List(_ context.Context, _ string, _, _ int32) ([]*model.ChainHead, int64, error) {
	out := make([]*model.ChainHead, 0, len(f.heads))
	for _, h := range f.heads {
		cp := *h
		out = append(out, &cp)
	}
	return out, int64(len(out)), nil
}

func (f *fakeChains) snapshot() map[string]model.ChainHead {
	out := make(map[string]model.ChainHead, len(f.heads))
	for k, h := range f.heads {
		out[k] = *h
	}
	return out
}

func (f *fakeChains) restore(snap map[string]model.ChainHead) {
	f.heads = make(map[string]*model.ChainHead, len(snap))
	for k, v := range snap {
		h := v
		f.heads[k] = &h
	}
}

// --- 假 audit_export_task ---

type fakeExports struct {
	byID   map[int64]*model.ExportTask
	byReq  map[string]int64
	nextID int64

	insertErr   error
	findErr     error
	notFoundErr error
	listRows    []*model.ExportTask
	listTotal   int64
	listErr     error
	listF       []model.ExportTaskFilter
	progress    []string
	claims      []string
	transitions []string
	finishes    []string
	claimFails  int
}

func newFakeExports() *fakeExports {
	return &fakeExports{byID: map[int64]*model.ExportTask{}, byReq: map[string]int64{}}
}

func (f *fakeExports) Insert(_ context.Context, t *model.ExportTask) (int64, error) {
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	if t.RequestID == "" {
		return 0, model.ErrRequestIDRequired
	}
	if id, ok := f.byReq[t.RequestID]; ok {
		_ = id
		return 0, model.ErrTaskExists
	}
	f.nextID++
	row := *t
	row.TaskID = f.nextID
	if row.State == "" {
		row.State = model.ExportStatePending
	}
	f.byID[row.TaskID] = &row
	f.byReq[row.RequestID] = row.TaskID
	t.TaskID = row.TaskID
	return row.TaskID, nil
}

func (f *fakeExports) FindOne(_ context.Context, taskID int64) (*model.ExportTask, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	row, ok := f.byID[taskID]
	if !ok {
		return nil, model.ErrTaskNotFound
	}
	cp := *row
	return &cp, nil
}

func (f *fakeExports) FindByRequestID(_ context.Context, requestID string) (*model.ExportTask, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	id, ok := f.byReq[requestID]
	if !ok {
		return nil, nil
	}
	row := f.byID[id]
	cp := *row
	return &cp, nil
}

func (f *fakeExports) Claim(_ context.Context, taskID int64, expectState string) (bool, error) {
	f.claims = append(f.claims, expectState)
	row, ok := f.byID[taskID]
	if !ok {
		return false, model.ErrTaskNotFound
	}
	if f.claimFails > 0 {
		f.claimFails--
		return false, nil
	}
	if row.State != expectState {
		return false, nil
	}
	row.State = model.ExportStateRunning
	if row.StartedAt == 0 {
		row.StartedAt = testNow
	}
	return true, nil
}

func (f *fakeExports) AddProgress(_ context.Context, taskID int64, rows, lastSeq int64) error {
	row, ok := f.byID[taskID]
	if !ok {
		return model.ErrTaskNotFound
	}
	if row.State != model.ExportStateRunning {
		return errors.New("audit_export_task: 非 running 不允许推进游标")
	}
	row.RowCount += rows
	row.LastSeq = lastSeq
	f.progress = append(f.progress, fmt.Sprintf("+%d@%d", rows, lastSeq))
	return nil
}

func (f *fakeExports) Finish(_ context.Context, taskID int64, toState string, obj model.ObjectRef, errMsg string) (bool, error) {
	if !model.CanExportTransition(model.ExportStateRunning, toState) {
		return false, model.ErrTaskBadTransition
	}
	row, ok := f.byID[taskID]
	if !ok {
		return false, model.ErrTaskNotFound
	}
	if row.State != model.ExportStateRunning {
		return false, nil
	}
	row.State = toState
	row.RowCount = obj.TotalRows
	row.Bucket = obj.Bucket
	row.ObjectKey = obj.ObjectKey
	row.ObjectSize = obj.Size
	row.FileHash = obj.FileHash
	row.ExpireAt = obj.ExpireAt
	row.ErrMsg = errMsg
	row.FinishedAt = testNow
	f.finishes = append(f.finishes, toState)
	return true, nil
}

func (f *fakeExports) TransitionState(_ context.Context, taskID int64, fromState, toState string) (bool, error) {
	if !model.CanExportTransition(fromState, toState) {
		return false, model.ErrTaskBadTransition
	}
	f.transitions = append(f.transitions, fromState+"→"+toState)
	row, ok := f.byID[taskID]
	if !ok || row.State != fromState {
		return false, nil
	}
	row.State = toState
	return true, nil
}

func (f *fakeExports) List(_ context.Context, ft model.ExportTaskFilter) ([]*model.ExportTask, int64, error) {
	f.listF = append(f.listF, ft)
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.listRows, f.listTotal, nil
}

// --- 假 audit_retention_policy ---

type fakePolicies struct {
	byDomain map[string]*model.RetentionPolicy
	nextID   int64

	insertErr error
	findErr   error
	updateErr error
	// findErrOnCall>0 时 findErr 只在第 N 次 FindOne 上生效：用来复现
	// 「写成功之后回读才失败」这类只落在最后一次读上的故障。
	// 不加这个开关，findErr 会连第一次读一起打掉，就测不到已落库的残留形态。
	findErrOnCall int
	findCalls     int
	listRows      []*model.RetentionPolicy
	listTotal     int64
	listErr       error
	inserts       []string
	updates       []string
	forceMiss     bool
}

func newFakePolicies() *fakePolicies {
	return &fakePolicies{byDomain: map[string]*model.RetentionPolicy{}}
}

func (f *fakePolicies) Insert(_ context.Context, p *model.RetentionPolicy) (int64, error) {
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	if _, ok := f.byDomain[p.ActionDomain]; ok {
		return 0, model.ErrPolicyExists
	}
	f.nextID++
	row := *p
	row.PolicyID = f.nextID
	row.Version = 1
	row.Ctime = testNow
	f.byDomain[row.ActionDomain] = &row
	f.inserts = append(f.inserts, row.ActionDomain)
	p.PolicyID = row.PolicyID
	p.Version = row.Version
	return row.PolicyID, nil
}

func (f *fakePolicies) FindOne(_ context.Context, actionDomain string) (*model.RetentionPolicy, error) {
	f.findCalls++
	// findErrOnCall==0 表示「每次读都失败」（沿用 findErr 的原有语义）；
	// >0 时只在指定那一次失败。
	if f.findErr != nil && (f.findErrOnCall == 0 || f.findCalls == f.findErrOnCall) {
		return nil, f.findErr
	}
	row, ok := f.byDomain[actionDomain]
	if !ok {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakePolicies) FindEffective(ctx context.Context, actionDomain string) (*model.RetentionPolicy, error) {
	row, err := f.FindOne(ctx, actionDomain)
	if err != nil || row != nil {
		return row, err
	}
	return f.FindOne(ctx, model.DefaultPolicyDomain)
}

func (f *fakePolicies) UpdateWithVersion(_ context.Context, p *model.RetentionPolicy, expect int64) (bool, error) {
	if f.updateErr != nil {
		return false, f.updateErr
	}
	f.updates = append(f.updates, fmt.Sprintf("%s/%d", p.ActionDomain, expect))
	row, ok := f.byDomain[p.ActionDomain]
	if !ok {
		return false, nil
	}
	if f.forceMiss || row.Version != expect {
		return false, nil
	}
	row.HotDays = p.HotDays
	row.ArchiveAfterDays = p.ArchiveAfterDays
	row.DeleteAfterDays = p.DeleteAfterDays
	row.State = p.State
	row.Operator = p.Operator
	row.Remark = p.Remark
	row.Version++
	row.Mtime = testNow
	return true, nil
}

func (f *fakePolicies) List(_ context.Context, state, _, _ int32) ([]*model.RetentionPolicy, int64, error) {
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.listRows, f.listTotal, nil
}

// --- 假 audit_archive_batch ---

type fakeArchives struct {
	byID   map[int64]*model.ArchiveBatch
	byReq  map[string]int64
	nextID int64

	insertErr error
	findErr   error
	listRows  []*model.ArchiveBatch
	listTotal int64
	listErr   error
	listF     []model.ArchiveBatchFilter
	covering  *model.ArchiveBatch
	inserts   []string
	moves     []string
}

func newFakeArchives() *fakeArchives {
	return &fakeArchives{byID: map[int64]*model.ArchiveBatch{}, byReq: map[string]int64{}}
}

func (f *fakeArchives) Insert(_ context.Context, b *model.ArchiveBatch) (int64, error) {
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	if b.RequestID == "" {
		return 0, model.ErrRequestIDRequired
	}
	if id, ok := f.byReq[b.RequestID]; ok {
		_ = id
		return 0, model.ErrTaskExists
	}
	f.nextID++
	row := *b
	row.BatchID = f.nextID
	if row.State == "" {
		row.State = model.BatchStatePending
	}
	f.byID[row.BatchID] = &row
	f.byReq[row.RequestID] = row.BatchID
	b.BatchID = row.BatchID
	f.inserts = append(f.inserts, fmt.Sprintf("%s/%d-%d", row.ChainKey, row.FromSeq, row.ToSeq))
	return row.BatchID, nil
}

func (f *fakeArchives) FindOne(_ context.Context, batchID int64) (*model.ArchiveBatch, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	row, ok := f.byID[batchID]
	if !ok {
		return nil, model.ErrBatchNotFound
	}
	cp := *row
	return &cp, nil
}

func (f *fakeArchives) FindByRequestID(_ context.Context, requestID string) (*model.ArchiveBatch, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	id, ok := f.byReq[requestID]
	if !ok {
		return nil, nil
	}
	cp := *f.byID[id]
	return &cp, nil
}

func (f *fakeArchives) FindCovering(_ context.Context, chainKey string, fromSeq, toSeq int64) (*model.ArchiveBatch, error) {
	if f.covering == nil {
		return nil, nil
	}
	c := f.covering
	if c.ChainKey != chainKey || c.FromSeq > fromSeq || c.ToSeq < toSeq {
		return nil, nil
	}
	cp := *c
	return &cp, nil
}

func (f *fakeArchives) TransitionState(_ context.Context, batchID int64, fromState, toState,
	manifestHash, lastEntryHash, errMsg string) (bool, error) {
	if !model.CanBatchTransition(fromState, toState) {
		return false, model.ErrTaskBadTransition
	}
	f.moves = append(f.moves, fromState+"→"+toState)
	row, ok := f.byID[batchID]
	if !ok || row.State != fromState {
		return false, nil
	}
	row.State = toState
	row.ErrMsg = errMsg
	// 与真实现一致：两个哈希只能落一次，之后不可改。
	if row.ManifestHash == "" {
		row.ManifestHash = manifestHash
	}
	if row.LastEntryHash == "" {
		row.LastEntryHash = lastEntryHash
	}
	switch toState {
	case model.BatchStateVerified, model.BatchStatePurged, model.BatchStateFailed:
		row.FinishedAt = testNow
	}
	return true, nil
}

func (f *fakeArchives) List(_ context.Context, ft model.ArchiveBatchFilter) ([]*model.ArchiveBatch, int64, error) {
	f.listF = append(f.listF, ft)
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.listRows, f.listTotal, nil
}

// --- 假对象存储 ---

type fakeObject struct {
	bucket    string
	key       string
	buf       []byte
	committed bool
	aborted   bool
	writeErr  error
	// tamper 在 Commit 前追加字节：制造「对象侧摘要 ≠ 本地清单摘要」的不一致。
	tamper []byte
}

func (o *fakeObject) Write(p []byte) (int, error) {
	if o.writeErr != nil {
		return 0, o.writeErr
	}
	o.buf = append(o.buf, p...)
	return len(p), nil
}

func (o *fakeObject) Commit() (objectLocation, error) {
	data := o.buf
	if len(o.tamper) > 0 {
		data = append(append([]byte{}, data...), o.tamper...)
	}
	o.committed = true
	sum := sha256.Sum256(data)
	return objectLocation{
		Bucket:    o.bucket,
		ObjectKey: o.key,
		Size:      int64(len(data)),
		SHA256Hex: hex.EncodeToString(sum[:]),
	}, nil
}

func (o *fakeObject) Abort() error {
	o.aborted = true
	return nil
}

type fakeStore struct {
	objects   map[string]*fakeObject
	created   []string
	createErr error
	// failKey：只对某个键失败（模拟「清单写成功、条目文件写失败」的半程失败）。
	failKey    string
	presign    string
	presignErr error
	presignTTL []int64
	presignKey []string
	ttl        int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string]*fakeObject{}}
}

func (s *fakeStore) CreateObject(_ context.Context, bucket, objectKey string) (objectWriter, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	if s.failKey != "" && s.failKey == objectKey {
		return nil, model.ErrObjectStorageUnsupported
	}
	o := &fakeObject{bucket: bucket, key: objectKey}
	s.objects[objectKey] = o
	s.created = append(s.created, objectKey)
	return o, nil
}

func (s *fakeStore) PresignDownload(_ context.Context, bucket, objectKey string, ttl int64) (string, int64, error) {
	s.presignTTL = append(s.presignTTL, ttl)
	s.presignKey = append(s.presignKey, bucket+"/"+objectKey)
	if s.presignErr != nil {
		return "", 0, s.presignErr
	}
	if s.presign == "" {
		return "", 0, nil
	}
	return s.presign, testNow + ttl, nil
}

// --- deps 装配 ---

type fixture struct {
	entries  *fakeEntries
	chains   *fakeChains
	exports  *fakeExports
	policies *fakePolicies
	archives *fakeArchives
	store    *fakeStore
	opts     repository.Options
	now      int64
	deps     deps
	tb       testing.TB
}

func newFixture(t testing.TB) *fixture {
	t.Helper()
	f := &fixture{
		entries:  newFakeEntries(),
		chains:   newFakeChains(),
		exports:  newFakeExports(),
		policies: newFakePolicies(),
		archives: newFakeArchives(),
		store:    newFakeStore(),
		now:      testNow,
		tb:       t,
	}
	f.opts = repository.Options{
		IpHashSalt: []byte("unit-test-salt"),
		Storage: repository.StorageConf{
			Enabled: true,
			Bucket:  "audit-archive-bucket",
		},
	}
	// 走 withDefaults，测试里拿到的上限与生产默认值同源，不再手抄一遍数字。
	r := repository.New(nil, nil, f.opts)
	f.opts = r.Options()
	f.deps = f.build()
	return f
}

func (f *fixture) build() deps {
	d := deps{
		entries:  f.entries,
		chains:   f.chains,
		exports:  f.exports,
		policies: f.policies,
		archives: f.archives,
		storage:  f.store,
		now:      func() int64 { return f.now },
		opts:     f.opts,
		clampPage: func(pn, ps int32) (int32, int32, error) {
			return f.repo().ClampPage(pn, ps)
		},
		checkQueryWindow: func(startAt, endAt int64, narrowed bool) error {
			return f.repo().CheckQueryWindow(startAt, endAt, narrowed)
		},
		maxRangeSeconds: f.repo().MaxRangeSeconds,
	}
	d.transact = func(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
		snapE := f.entries.snapshot()
		snapC := f.chains.snapshot()
		if err := fn(ctx, fakeSession{}); err != nil {
			f.entries.restore(snapE)
			f.chains.restore(snapC)
			return err
		}
		return nil
	}
	d.purge = func(ctx context.Context, b *model.ArchiveBatch, ts int64) (int64, error) {
		if b.State != model.BatchStateVerified {
			return 0, model.ErrBatchUnverified
		}
		if b.ManifestHash == "" {
			return 0, repository.ErrPurgeWithoutVerify
		}
		return f.entries.MarkArchived(ctx, b.ChainKey, b.FromSeq, b.ToSeq, ts)
	}
	return d
}

func (f *fixture) repo() *repository.Repository {
	// clampPage/checkQueryWindow/MaxRangeSeconds 只依赖归一化后的 opts，与连接无关，
	// 因此这里现构造一个不碰 IO 的 Repository 实例，而不为了拿纯函数去碰任何连接。
	// 必须跟着 f.opts 走：测试里改配置来验证「上限随配置生效」才不是假的。
	return repository.New(nil, nil, f.opts)
}

// withOpts 覆盖参数并重建 deps（分页上限、导出批次等按用例调整）。
func (f *fixture) withOpts(mut func(*repository.Options)) {
	mut(&f.opts)
	f.deps = f.build()
}

// use 把 buildDeps 换成本夹具的 deps，并注册还原（logic 用例通过它取数据面）。
func (f *fixture) use(t *testing.T) {
	t.Helper()
	prev := buildDeps
	buildDeps = func(*svc.ServiceContext) deps { return f.deps }
	t.Cleanup(func() { buildDeps = prev })
}

func svcCtx() *svc.ServiceContext { return &svc.ServiceContext{} }

// putRows 把预置行放进假库（并按 entry_id 单调推进游标），供读侧用例使用。
func (f *fixture) putRows(rows ...*model.AuditEntry) {
	for _, r := range rows {
		cp := *r
		f.entries.byID[cp.EntryID] = &cp
		f.entries.byEvent[cp.EventID] = &cp
		if cp.EntryID > f.entries.nextID {
			f.entries.nextID = cp.EntryID
		}
	}
}

// putHead 预置链头（校验/归档用例需要「链尾到哪」这一事实）。
func (f *fixture) putHead(chainKey string, seq int64, lastHash string) {
	f.chains.heads[chainKey] = &model.ChainHead{
		ChainKey: chainKey, Seq: seq, EntryCount: seq, LastHash: lastHash, FirstAt: testNow - seq*60,
	}
}

// chainOf 返回假库里指定链上按 seq 升序的行，便于对留痕做断言。
func (f *fixture) chainOf(chainKey string) []*model.AuditEntry {
	var out []*model.AuditEntry
	for _, r := range f.entries.byID {
		if r.ChainKey == chainKey {
			cp := *r
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// countAll 返回假库当前行数。
func (f *fixture) countAll() int { return len(f.entries.byID) }

// --- 造链数据 ---

// seedChain 造一条完整可重放的链：seq 从 1 连续递增，prev/entry_hash 全部按 V1 序列化重算。
func seedChain(chainKey string, n int, entryIDBase int64) []*model.AuditEntry {
	rows := make([]*model.AuditEntry, 0, n)
	prev := model.GenesisHash(chainKey)
	for i := 1; i <= n; i++ {
		row := &model.AuditEntry{
			EntryID:       entryIDBase + int64(i),
			EventID:       fmt.Sprintf("ev-%s-%d", chainKey, i),
			SchemaVersion: model.SchemaVersion,
			ChainKey:      chainKey,
			Seq:           int64(i),
			ActorType:     model.ActorAdmin,
			ActorID:       7,
			Action:        "media.approve",
			ActionDomain:  model.DomainOfChainKey(chainKey),
			Result:        model.ResultOK,
			SourceApp:     model.SourceAdminWeb,
			OccurredAt:    testNow - int64(n-i)*60,
			PrevHash:      prev,
			Ctime:         testNow,
		}
		row.EntryHash = model.ComputeEntryHash(row)
		prev = row.EntryHash
		rows = append(rows, row)
	}
	return rows
}
