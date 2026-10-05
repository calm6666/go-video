package logic

// 手写 fake：内存版 MySQL（五张台账表）+ 令牌桶 + 投递 sender + 日志捕获。
// 语义必须与 services/event-collector/model 的真实实现逐条对齐，
// 否则测试通过的是 fake 而不是 logic（AGENTS.md §9）。
// 不连 MySQL/Redis：Cache *redis.Redis 是具体类型，无法注入 fake，
// 因此所有用例都在 Cache=nil 的降级形态下跑（与 services/spm 的既有做法一致）。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go-video/common/ratelimit"
	"go-video/services/event-collector/model"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// fakeDB 是五张表的内存副本，语义对齐真实 model：
// uniq_batch_id / uniq_event_id / uniq_version / uniq_active / uniq_event_topic
// 都以「命中即静默跳过并回 created=false / inserted 行数不足」实现。
type fakeDB struct {
	batches  map[int64]*model.IngestBatch
	records  map[int64]*model.EventRecord
	policies map[int64]*model.DispatchPolicy
	pending  map[int64]*model.PendingDelivery
	dead     map[int64]*model.DeadLetter

	next  int64
	calls map[string]int
	// clock 非 0 时作为写入行的 ctime/mtime，便于分页用例造出可预期的时间序。
	clock int64

	txCalls   int
	begins    int
	commits   int
	rollbacks int

	// 故障注入：key 是下面各方法开头 tick 的算子名。
	fail      map[string]error
	shortRows map[string]int64
	noApply   map[string]bool
	// missFirst 让某算子第一次读取时「查不到」（模拟并发对手在你读之后写入），
	// 用于驱动 errReplayBatch → serveConcurrentReplay 这条真实并发路径。
	missFirst map[string]bool
	// commitErr 模拟「业务写成功但 COMMIT 失败」：整库还原并抛错。
	commitErr error
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		batches:   map[int64]*model.IngestBatch{},
		records:   map[int64]*model.EventRecord{},
		policies:  map[int64]*model.DispatchPolicy{},
		pending:   map[int64]*model.PendingDelivery{},
		dead:      map[int64]*model.DeadLetter{},
		next:      1000,
		calls:     map[string]int{},
		fail:      map[string]error{},
		shortRows: map[string]int64{},
		noApply:   map[string]bool{},
		missFirst: map[string]bool{},
	}
}

func (d *fakeDB) tick(op string) { d.calls[op]++ }

func (d *fakeDB) missOnce(op string) bool {
	if !d.missFirst[op] {
		return false
	}
	d.missFirst[op] = false
	return true
}

func (d *fakeDB) errAt(op string) error {
	if err := d.fail[op]; err != nil {
		return err
	}
	return nil
}

// rowsAffected 把 shortRows[op] 当成「实际新增行数」：负数表示按入参全量新增。
func (d *fakeDB) rowsAffected(op string, want int64) int64 {
	if n, ok := d.shortRows[op]; ok {
		return n
	}
	return want
}

func (d *fakeDB) unapplied(op string) bool { return d.noApply[op] }

func (d *fakeDB) stamp() int64 {
	if d.clock > 0 {
		return d.clock
	}
	return timeNowUnix()
}

func (d *fakeDB) id() int64 { d.next++; return d.next }

type dbSnapshot struct {
	batches  map[int64]*model.IngestBatch
	records  map[int64]*model.EventRecord
	policies map[int64]*model.DispatchPolicy
	pending  map[int64]*model.PendingDelivery
	dead     map[int64]*model.DeadLetter
	next     int64
}

func copyBatches(m map[int64]*model.IngestBatch) map[int64]*model.IngestBatch {
	out := make(map[int64]*model.IngestBatch, len(m))
	for k, v := range m {
		cp := *v
		out[k] = &cp
	}
	return out
}

func copyRecords(m map[int64]*model.EventRecord) map[int64]*model.EventRecord {
	out := make(map[int64]*model.EventRecord, len(m))
	for k, v := range m {
		cp := *v
		out[k] = &cp
	}
	return out
}

func copyPolicies(m map[int64]*model.DispatchPolicy) map[int64]*model.DispatchPolicy {
	out := make(map[int64]*model.DispatchPolicy, len(m))
	for k, v := range m {
		cp := *v
		out[k] = &cp
	}
	return out
}

func copyPending(m map[int64]*model.PendingDelivery) map[int64]*model.PendingDelivery {
	out := make(map[int64]*model.PendingDelivery, len(m))
	for k, v := range m {
		cp := *v
		out[k] = &cp
	}
	return out
}

func copyDead(m map[int64]*model.DeadLetter) map[int64]*model.DeadLetter {
	out := make(map[int64]*model.DeadLetter, len(m))
	for k, v := range m {
		cp := *v
		out[k] = &cp
	}
	return out
}

func (d *fakeDB) snapshot() dbSnapshot {
	return dbSnapshot{
		batches:  copyBatches(d.batches),
		records:  copyRecords(d.records),
		policies: copyPolicies(d.policies),
		pending:  copyPending(d.pending),
		dead:     copyDead(d.dead),
		next:     d.next,
	}
}

func (d *fakeDB) restore(s dbSnapshot) {
	d.batches, d.records, d.policies = s.batches, s.records, s.policies
	d.pending, d.dead, d.next = s.pending, s.dead, s.next
}

// runTx 是 TransactCtx 的内存语义：快照 → 执行 → 失败或提交错误则整库还原。
func (d *fakeDB) runTx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	d.txCalls++
	d.begins++
	snap := d.snapshot()
	err := fn(ctx, &fakeSession{db: d})
	if err == nil {
		err = d.commitErr
	}
	if err != nil {
		d.restore(snap)
		d.rollbacks++
		return err
	}
	d.commits++
	return nil
}

// fakeConn 只实现 logic 真正用到的方法，其余走内嵌 nil 接口（一旦被用即 panic，
// 从而暴露「测试没覆盖到的真实 SQL 依赖」而不是静默返回空结果）。
type fakeConn struct {
	sqlx.SqlConn
	db *fakeDB
}

func (c *fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	return c.db.runTx(ctx, fn)
}

func (c *fakeConn) Transact(fn func(sqlx.Session) error) error {
	return c.db.runTx(context.Background(), func(_ context.Context, s sqlx.Session) error {
		return fn(s)
	})
}

type fakeSession struct {
	sqlx.Session
	db *fakeDB
}

// ---------------------------------------------------------------- 批次台账

type batchesModel struct{ *fakeDB }

var _ model.IngestBatchModel = batchesModel{}

func (m batchesModel) Insert(_ context.Context, _ sqlx.Session, b *model.IngestBatch) (int64, bool, error) {
	m.tick("batches.Insert")
	if err := m.errAt("batches.Insert"); err != nil {
		return 0, false, err
	}
	if b == nil || b.BatchID == "" || len(b.BatchID) > 64 {
		return 0, false, model.ErrBatchIDRequired
	}
	if !model.ValidSource(b.Source) {
		return 0, false, model.ErrSourceNotAllowed(b.Source)
	}
	if exist, ok := m.byBatchID(b.BatchID); ok {
		return exist.ID, false, nil
	}
	cp := *b
	cp.ID = m.id()
	if cp.State == 0 {
		cp.State = model.BatchStateReceived
	}
	ts := m.stamp()
	cp.Ctime, cp.Mtime = ts, ts
	if cp.ReceivedAt == 0 {
		cp.ReceivedAt = ts
	}
	m.batches[cp.ID] = &cp
	return cp.ID, true, nil
}

func (d *fakeDB) byBatchID(batchID string) (*model.IngestBatch, bool) {
	for _, b := range d.batches {
		if b.BatchID == batchID {
			return b, true
		}
	}
	return nil, false
}

func (m batchesModel) FindByBatchID(_ context.Context, batchID string) (*model.IngestBatch, error) {
	m.tick("batches.FindByBatchID")
	if err := m.errAt("batches.FindByBatchID"); err != nil {
		return nil, err
	}
	if m.missOnce("batches.FindByBatchID") {
		return nil, model.ErrBatchNotFound
	}
	b, ok := m.byBatchID(batchID)
	if !ok {
		return nil, model.ErrBatchNotFound
	}
	cp := *b
	return &cp, nil
}

func (m batchesModel) Accumulate(_ context.Context, _ sqlx.Session, batchID string,
	total, accepted, duplicated, rejected, sampledOut int32) error {
	m.tick("batches.Accumulate")
	if err := m.errAt("batches.Accumulate"); err != nil {
		return err
	}
	if b, ok := m.byBatchID(batchID); ok {
		b.Total += total
		b.Accepted += accepted
		b.Duplicated += duplicated
		b.Rejected += rejected
		b.SampledOut += sampledOut
		b.Mtime = m.stamp()
	}
	return nil
}

func (m batchesModel) UpdateAttribution(_ context.Context, _ sqlx.Session, batchID, policyVersion string,
	saltVersion int32, requestBytes int64) error {
	m.tick("batches.UpdateAttribution")
	if err := m.errAt("batches.UpdateAttribution"); err != nil {
		return err
	}
	if b, ok := m.byBatchID(batchID); ok {
		b.PolicyVersion = policyVersion
		b.SaltVersion = saltVersion
		b.RequestBytes = requestBytes
		b.Mtime = m.stamp()
	}
	return nil
}

func (m batchesModel) MarkState(_ context.Context, _ sqlx.Session, batchID string,
	from []int32, to, topReason int32, lastError string) (bool, error) {
	m.tick("batches.MarkState")
	if err := m.errAt("batches.MarkState"); err != nil {
		return false, err
	}
	if batchID == "" {
		return false, model.ErrBatchIDRequired
	}
	if !model.ValidBatchState(to) || len(from) == 0 {
		return false, model.ErrInvalidStateTransition
	}
	b, ok := m.byBatchID(batchID)
	if !ok || !inInt32(from, b.State) || m.unapplied("batches.MarkState") {
		return false, nil
	}
	b.State = to
	b.TopReason = topReason
	b.LastError = clip(lastError, 512)
	b.Mtime = m.stamp()
	return true, nil
}

func (m batchesModel) MarkDispatchProgress(_ context.Context, _ sqlx.Session, batchID string,
	dispatchedDelta, deadDelta int32) (bool, error) {
	m.tick("batches.MarkDispatchProgress")
	if err := m.errAt("batches.MarkDispatchProgress"); err != nil {
		return false, err
	}
	b, ok := m.byBatchID(batchID)
	if !ok || m.unapplied("batches.MarkDispatchProgress") {
		return false, nil
	}
	b.Dispatched += dispatchedDelta
	b.Dead += deadDelta
	b.Mtime = m.stamp()
	return true, nil
}

func (m batchesModel) Finish(_ context.Context, _ sqlx.Session, batchID string) (bool, error) {
	m.tick("batches.Finish")
	if err := m.errAt("batches.Finish"); err != nil {
		return false, err
	}
	b, ok := m.byBatchID(batchID)
	if !ok || b.FinishedAt != 0 || m.unapplied("batches.Finish") {
		return false, nil
	}
	b.FinishedAt = m.stamp()
	b.State = model.BatchStateDispatched
	b.Mtime = m.stamp()
	return true, nil
}

func (m batchesModel) RecountFromRecords(_ context.Context, _ sqlx.Session, batchID string) error {
	m.tick("batches.RecountFromRecords")
	return m.errAt("batches.RecountFromRecords")
}

func (m batchesModel) List(_ context.Context, f model.IngestBatchFilter, cur model.Cursor,
	ps int32) ([]*model.IngestBatch, error) {
	m.tick("batches.List")
	if err := m.errAt("batches.List"); err != nil {
		return nil, err
	}
	if ps <= 0 {
		return nil, model.ErrInvalidPage
	}
	var rows []*model.IngestBatch
	for _, b := range m.batches {
		if matchBatchFilter(b, f) && withinCursor(b.Ctime, b.ID, cur) {
			cp := *b
			rows = append(rows, &cp)
		}
	}
	sortByCtimeDesc(rows)
	return clipRows(rows, ps), nil
}

func (m batchesModel) Count(_ context.Context, f model.IngestBatchFilter) (int64, error) {
	m.tick("batches.Count")
	if err := m.errAt("batches.Count"); err != nil {
		return 0, err
	}
	var n int64
	for _, b := range m.batches {
		if matchBatchFilter(b, f) {
			n++
		}
	}
	return n, nil
}

func (m batchesModel) CountRejectedSince(_ context.Context, since int64) (int64, error) {
	m.tick("batches.CountRejectedSince")
	if err := m.errAt("batches.CountRejectedSince"); err != nil {
		return 0, err
	}
	var n int64
	for _, b := range m.batches {
		if b.State == model.BatchStateRejected && b.Ctime >= since {
			n++
		}
	}
	return n, nil
}

func (m batchesModel) CountTopReasonSince(_ context.Context, topReason int32, since int64) (int64, error) {
	m.tick("batches.CountTopReasonSince")
	if err := m.errAt("batches.CountTopReasonSince"); err != nil {
		return 0, err
	}
	if !model.ValidReason(topReason) {
		return 0, model.ErrInvalidStateTransition
	}
	var n int64
	for _, b := range m.batches {
		if b.State == model.BatchStateRejected && b.TopReason == topReason && b.Ctime >= since {
			n++
		}
	}
	return n, nil
}

func (m batchesModel) DeleteBefore(_ context.Context, ctimeBefore int64, limit int32) (int64, error) {
	m.tick("batches.DeleteBefore")
	if ctimeBefore <= 0 {
		return 0, model.ErrInvalidPage
	}
	if limit <= 0 {
		limit = 1000
	}
	if limit > 500 {
		return 0, model.ErrBatchLimitTooLarge
	}
	var n int64
	for id, b := range m.batches {
		if b.Ctime < ctimeBefore && n < int64(limit) {
			delete(m.batches, id)
			n++
		}
	}
	return n, nil
}

func (m batchesModel) WithSession(_ sqlx.Session) model.IngestBatchModel { return m }

func matchBatchFilter(b *model.IngestBatch, f model.IngestBatchFilter) bool {
	switch {
	case f.Source != 0 && b.Source != f.Source:
		return false
	case f.State != 0 && b.State != f.State:
		return false
	case f.Mid != 0 && b.Mid != f.Mid:
		return false
	case f.DeviceHash != "" && b.DeviceHash != f.DeviceHash:
		return false
	case f.IPSegment != "" && b.IPSegment != f.IPSegment:
		return false
	case f.CtimeFrom != 0 && b.Ctime < f.CtimeFrom:
		return false
	case f.CtimeTo != 0 && b.Ctime > f.CtimeTo:
		return false
	}
	return true
}

// ---------------------------------------------------------------- 事件台账

type recordsModel struct{ *fakeDB }

var _ model.EventRecordModel = recordsModel{}

func (m recordsModel) InsertIgnoreMany(_ context.Context, _ sqlx.Session,
	rows []*model.EventRecord) (int64, error) {
	m.tick("records.InsertIgnoreMany")
	if err := m.errAt("records.InsertIgnoreMany"); err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	if len(rows) > 500 {
		return 0, model.ErrBatchTooLarge
	}
	want := int64(len(rows))
	for _, r := range rows {
		if r == nil || r.EventID == "" || len(r.EventID) > 64 {
			return 0, model.ErrEventIDRequired
		}
		if r.BatchID == "" {
			return 0, model.ErrBatchIDRequired
		}
		if _, ok := m.byEventID(r.EventID); ok {
			want-- // INSERT IGNORE：已有 uniq_event_id 的行静默跳过
			continue
		}
		cp := *r
		cp.ID = m.id()
		ts := m.stamp()
		if cp.Ctime == 0 {
			cp.Ctime = ts
		}
		cp.Mtime = ts
		if cp.ReceivedAt == 0 {
			cp.ReceivedAt = ts
		}
		m.records[cp.ID] = &cp
	}
	return m.rowsAffected("records.InsertIgnoreMany", want), nil
}

func (m recordsModel) Insert(ctx context.Context, session sqlx.Session,
	r *model.EventRecord) (int64, bool, error) {
	m.tick("records.Insert")
	n, err := m.InsertIgnoreMany(ctx, session, []*model.EventRecord{r})
	if err != nil {
		return 0, false, err
	}
	if n == 0 {
		exist, ok := m.byEventID(r.EventID)
		if !ok {
			return 0, false, model.ErrRecordNotFound
		}
		return exist.ID, false, nil
	}
	for _, row := range m.records {
		if row.EventID == r.EventID {
			return row.ID, true, nil
		}
	}
	return 0, false, model.ErrRecordNotFound
}

func (d *fakeDB) byEventID(eventID string) (*model.EventRecord, bool) {
	for _, r := range d.records {
		if r.EventID == eventID {
			return r, true
		}
	}
	return nil, false
}

func (m recordsModel) FindByEventID(_ context.Context, eventID string) (*model.EventRecord, error) {
	m.tick("records.FindByEventID")
	if err := m.errAt("records.FindByEventID"); err != nil {
		return nil, err
	}
	r, ok := m.byEventID(eventID)
	if !ok {
		return nil, model.ErrRecordNotFound
	}
	cp := *r
	return &cp, nil
}

func (m recordsModel) ListByEventIDs(_ context.Context, ids []string) (map[string]*model.EventRecord, error) {
	m.tick("records.ListByEventIDs")
	if err := m.errAt("records.ListByEventIDs"); err != nil {
		return nil, err
	}
	out := map[string]*model.EventRecord{}
	if len(ids) == 0 {
		return out, nil
	}
	if len(ids) > 500 {
		return nil, model.ErrBatchTooLarge
	}
	for _, id := range ids {
		if id == "" {
			return nil, model.ErrEventIDRequired
		}
		if r, ok := m.byEventID(id); ok {
			cp := *r
			out[id] = &cp
		}
	}
	return out, nil
}

func (m recordsModel) MarkDelivery(_ context.Context, _ sqlx.Session, eventID string,
	from []int32, to int32, topic string, attempts int32, nextRetryAt int64,
	lastError string) (bool, error) {
	m.tick("records.MarkDelivery")
	if err := m.errAt("records.MarkDelivery"); err != nil {
		return false, err
	}
	if eventID == "" {
		return false, model.ErrEventIDRequired
	}
	if !model.ValidDeliveryState(to) || len(from) == 0 {
		return false, model.ErrInvalidStateTransition
	}
	r, ok := m.byEventID(eventID)
	if !ok || !inInt32(from, r.DeliveryState) {
		return false, nil
	}
	r.DeliveryState = to
	if topic != "" {
		r.Topic = topic
	}
	r.DeliveryAttempts = attempts
	r.NextRetryAt = nextRetryAt
	r.LastError = clip(lastError, 512)
	r.Mtime = m.stamp()
	return true, nil
}

func (m recordsModel) List(_ context.Context, f model.EventRecordFilter, cur model.Cursor,
	ps int32) ([]*model.EventRecord, error) {
	m.tick("records.List")
	if err := m.errAt("records.List"); err != nil {
		return nil, err
	}
	if ps <= 0 {
		return nil, model.ErrInvalidPage
	}
	var rows []*model.EventRecord
	for _, r := range m.records {
		if matchRecordFilter(r, f) && withinCursor(r.Ctime, r.ID, cur) {
			cp := *r
			rows = append(rows, &cp)
		}
	}
	sortRecordsByCtimeDesc(rows)
	return clipRecordRows(rows, ps), nil
}

func (m recordsModel) Count(_ context.Context, f model.EventRecordFilter) (int64, error) {
	m.tick("records.Count")
	if err := m.errAt("records.Count"); err != nil {
		return 0, err
	}
	var n int64
	for _, r := range m.records {
		if matchRecordFilter(r, f) {
			n++
		}
	}
	return n, nil
}

func (m recordsModel) CountReasonSince(_ context.Context, reason int32, since int64) (int64, error) {
	m.tick("records.CountReasonSince")
	if err := m.errAt("records.CountReasonSince"); err != nil {
		return 0, err
	}
	if !model.ValidReason(reason) {
		return 0, model.ErrInvalidStateTransition
	}
	var n int64
	for _, r := range m.records {
		if r.Reason == reason && r.Ctime >= since {
			n++
		}
	}
	return n, nil
}

func (m recordsModel) CountOpenSince(_ context.Context, since int64) (int64, error) {
	m.tick("records.CountOpenSince")
	var n int64
	for _, r := range m.records {
		if model.DeliveryStateOpen(r.DeliveryState) && r.Ctime >= since {
			n++
		}
	}
	return n, nil
}

func (m recordsModel) CountOpenByBatch(_ context.Context, batchID string) (int64, error) {
	m.tick("records.CountOpenByBatch")
	if err := m.errAt("records.CountOpenByBatch"); err != nil {
		return 0, err
	}
	if batchID == "" {
		return 0, model.ErrBatchIDRequired
	}
	var n int64
	for _, r := range m.records {
		if r.BatchID == batchID && model.DeliveryStateOpen(r.DeliveryState) {
			n++
		}
	}
	return n, nil
}

func (m recordsModel) DeleteBefore(_ context.Context, ctimeBefore int64, limit int32) (int64, error) {
	m.tick("records.DeleteBefore")
	if ctimeBefore <= 0 {
		return 0, model.ErrInvalidPage
	}
	if limit <= 0 {
		limit = 1000
	}
	var n int64
	for id, r := range m.records {
		if r.Ctime < ctimeBefore && n < int64(limit) {
			delete(m.records, id)
			n++
		}
	}
	return n, nil
}

func (m recordsModel) WithSession(_ sqlx.Session) model.EventRecordModel { return m }

func matchRecordFilter(r *model.EventRecord, f model.EventRecordFilter) bool {
	switch {
	case f.BatchID != "" && r.BatchID != f.BatchID:
		return false
	case f.EventType != "" && r.EventType != f.EventType:
		return false
	case f.Category != 0 && r.Category != f.Category:
		return false
	case f.Decision != 0 && r.Decision != f.Decision:
		return false
	case f.Reason != 0 && r.Reason != f.Reason:
		return false
	case f.DeliveryState != 0 && r.DeliveryState != f.DeliveryState:
		return false
	case f.Topic != "" && r.Topic != f.Topic:
		return false
	case f.Mid != 0 && r.Mid != f.Mid:
		return false
	case f.DeviceHash != "" && r.DeviceHash != f.DeviceHash:
		return false
	case f.CtimeFrom != 0 && r.Ctime < f.CtimeFrom:
		return false
	case f.CtimeTo != 0 && r.Ctime > f.CtimeTo:
		return false
	}
	return true
}

// ---------------------------------------------------------------- 分发策略

type policiesModel struct{ *fakeDB }

var _ model.DispatchPolicyModel = policiesModel{}

func (m policiesModel) InsertDraft(_ context.Context, _ sqlx.Session,
	p *model.DispatchPolicy) (int64, bool, error) {
	m.tick("policies.InsertDraft")
	if err := m.errAt("policies.InsertDraft"); err != nil {
		return 0, false, err
	}
	if err := checkPolicyShape(p); err != nil {
		return 0, false, err
	}
	if exist, ok := m.byVersion(p.Version); ok {
		return exist.ID, false, nil
	}
	cp := *p
	cp.ID = m.id()
	cp.State = model.PolicyStateDraft
	ts := m.stamp()
	cp.Ctime, cp.Mtime = ts, ts
	m.policies[cp.ID] = &cp
	return cp.ID, true, nil
}

func (m policiesModel) UpdateDraft(_ context.Context, _ sqlx.Session,
	p *model.DispatchPolicy) (bool, error) {
	m.tick("policies.UpdateDraft")
	if err := m.errAt("policies.UpdateDraft"); err != nil {
		return false, err
	}
	if err := checkPolicyShape(p); err != nil {
		return false, err
	}
	cur, ok := m.byVersion(p.Version)
	if !ok || cur.State != model.PolicyStateDraft || m.unapplied("policies.UpdateDraft") {
		return false, nil
	}
	id, state, ctime := cur.ID, cur.State, cur.Ctime
	cp := *p
	cp.ID, cp.State, cp.Ctime = id, state, ctime
	cp.Mtime = m.stamp()
	m.policies[id] = &cp
	return true, nil
}

func (d *fakeDB) byVersion(v string) (*model.DispatchPolicy, bool) {
	for _, p := range d.policies {
		if p.Version == v {
			return p, true
		}
	}
	return nil, false
}

func (m policiesModel) FindByVersion(_ context.Context, version string) (*model.DispatchPolicy, error) {
	m.tick("policies.FindByVersion")
	if err := m.errAt("policies.FindByVersion"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(version) == "" {
		return nil, model.ErrPolicyNotFound
	}
	p, ok := m.byVersion(version)
	if !ok {
		return nil, model.ErrPolicyNotFound
	}
	cp := *p
	return &cp, nil
}

func (m policiesModel) FindActive(_ context.Context) (*model.DispatchPolicy, error) {
	m.tick("policies.FindActive")
	if err := m.errAt("policies.FindActive"); err != nil {
		return nil, err
	}
	p, ok := m.activeRow()
	if !ok {
		return nil, model.ErrNoActivePolicy
	}
	cp := *p
	return &cp, nil
}

func (d *fakeDB) activeRow() (*model.DispatchPolicy, bool) {
	for _, p := range d.policies {
		if p.State == model.PolicyStateActive {
			return p, true
		}
	}
	return nil, false
}

func (m policiesModel) Activate(_ context.Context, session sqlx.Session,
	version, expectedCurrent, operator string) (string, error) {
	m.tick("policies.Activate")
	if err := m.errAt("policies.Activate"); err != nil {
		return "", err
	}
	if strings.TrimSpace(version) == "" {
		return "", model.ErrPolicyNotFound
	}
	if strings.TrimSpace(operator) == "" {
		return "", model.ErrOperatorRequired
	}
	// uniq_active(active_flag) 只能靠事务内的行锁建立：没有 session 就必须失败关闭。
	if session == nil {
		return "", model.ErrInvalidStateTransition
	}
	current := ""
	if cur, ok := m.activeRow(); ok {
		current = cur.Version
	}
	if expectedCurrent != "" && current != expectedCurrent {
		return current, model.ErrConcurrentUpdate
	}
	if current == version {
		return current, nil
	}
	if m.unapplied("policies.Activate") {
		return current, model.ErrConcurrentUpdate
	}
	if cur, ok := m.activeRow(); ok {
		cur.State = model.PolicyStateArchived
		cur.Mtime = m.stamp()
	}
	target, ok := m.byVersion(version)
	if !ok {
		return current, model.ErrPolicyNotFound
	}
	if target.State != model.PolicyStateDraft {
		return current, model.ErrPolicyNotDraft
	}
	target.State = model.PolicyStateActive
	target.Mtime = m.stamp()
	return current, nil
}

func (m policiesModel) Archive(_ context.Context, session sqlx.Session,
	version, operator string) (bool, error) {
	m.tick("policies.Archive")
	if session == nil || strings.TrimSpace(operator) == "" {
		return false, model.ErrOperatorRequired
	}
	p, ok := m.byVersion(version)
	if !ok {
		return false, model.ErrPolicyNotFound
	}
	if !model.CanTransitPolicyState(p.State, model.PolicyStateArchived) {
		return false, model.ErrInvalidStateTransition
	}
	p.State = model.PolicyStateArchived
	p.Mtime = m.stamp()
	return true, nil
}

func (m policiesModel) List(_ context.Context, state int32, afterID int64,
	ps int32) ([]*model.DispatchPolicy, error) {
	m.tick("policies.List")
	if err := m.errAt("policies.List"); err != nil {
		return nil, err
	}
	if ps <= 0 {
		return nil, model.ErrInvalidPage
	}
	if state != 0 && !model.ValidPolicyState(state) {
		return nil, model.ErrInvalidStateTransition
	}
	var rows []*model.DispatchPolicy
	for _, p := range m.policies {
		if state != 0 && p.State != state {
			continue
		}
		if afterID > 0 && p.ID >= afterID {
			continue
		}
		cp := *p
		rows = append(rows, &cp)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID > rows[j].ID })
	return clipPolicyRows(rows, ps), nil
}

func (m policiesModel) Count(_ context.Context, state int32) (int64, error) {
	m.tick("policies.Count")
	if err := m.errAt("policies.Count"); err != nil {
		return 0, err
	}
	if state != 0 && !model.ValidPolicyState(state) {
		return 0, model.ErrInvalidStateTransition
	}
	var n int64
	for _, p := range m.policies {
		if state == 0 || p.State == state {
			n++
		}
	}
	return n, nil
}

func (m policiesModel) WithSession(_ sqlx.Session) model.DispatchPolicyModel { return m }

// checkPolicyShape 复刻 model.DispatchPolicyModel 入参校验的硬前置部分，
// 让 fake 在「行根本不该存在」时与真实实现抛同一个哨兵。
func checkPolicyShape(p *model.DispatchPolicy) error {
	if p == nil {
		return model.ErrPolicyNotFound
	}
	v := strings.TrimSpace(p.Version)
	if v == "" || len(v) > 64 {
		return fmt.Errorf("event-collector: policy version invalid")
	}
	if p.SaltVersion <= 0 || strings.TrimSpace(p.SaltRef) == "" {
		return model.ErrSaltMissing
	}
	if p.MaxEventsPerBatch <= 0 || p.MaxRequestBytes <= 0 || p.MaxEventPayloadBytes <= 0 {
		return model.ErrBatchTooLarge
	}
	if p.MaxClockSkewSeconds <= 0 || p.MaxBackfillSeconds <= 0 {
		return model.ErrInvalidStateTransition
	}
	if p.DeliverMaxAttempts < 0 || p.RetryBaseSeconds <= 0 || p.RetryMaxSeconds < p.RetryBaseSeconds {
		return model.ErrInvalidStateTransition
	}
	rules, err := model.DecodeSampleRules(p.SampleRulesJSON)
	if err != nil {
		return fmt.Errorf("event-collector: invalid sample_rules json: %w", err)
	}
	if !model.ValidateSampleRules(rules) {
		return model.ErrInvalidStateTransition
	}
	if _, err := model.DecodeStringList(p.FieldWhitelistJSON); err != nil {
		return fmt.Errorf("event-collector: invalid field_whitelist json: %w", err)
	}
	if _, err := model.DecodeStringList(p.DropFieldsJSON); err != nil {
		return fmt.Errorf("event-collector: invalid drop_fields json: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- 待投递队列

type pendingModel struct{ *fakeDB }

var _ model.PendingDeliveryModel = pendingModel{}

func (m pendingModel) InsertIgnore(_ context.Context, _ sqlx.Session,
	p *model.PendingDelivery) (bool, error) {
	m.tick("pending.InsertIgnore")
	if err := m.errAt("pending.InsertIgnore"); err != nil {
		return false, err
	}
	if p == nil || p.EventID == "" || p.Topic == "" {
		return false, model.ErrEventIDRequired
	}
	state := p.State
	if state == 0 {
		state = model.DeliveryStatePending
	}
	if !model.DeliveryStateOpen(state) && state != model.DeliveryStateSent && state != model.DeliveryStateDead {
		return false, model.ErrInvalidStateTransition
	}
	if _, ok := m.byEventTopic(p.EventID, p.Topic); ok {
		return false, nil
	}
	cp := *p
	cp.ID = m.id()
	cp.State = state
	ts := m.stamp()
	if cp.Ctime == 0 {
		cp.Ctime = ts
	}
	cp.Mtime = ts
	m.pending[cp.ID] = &cp
	return true, nil
}

func (d *fakeDB) byEventTopic(eventID, topic string) (*model.PendingDelivery, bool) {
	for _, p := range d.pending {
		if p.EventID == eventID && p.Topic == topic {
			return p, true
		}
	}
	return nil, false
}

func (m pendingModel) InsertIgnoreMany(ctx context.Context, session sqlx.Session,
	rows []*model.PendingDelivery) (int64, error) {
	m.tick("pending.InsertIgnoreMany")
	if err := m.errAt("pending.InsertIgnoreMany"); err != nil {
		return 0, err
	}
	var n int64
	want := int64(len(rows))
	for _, r := range rows {
		created, err := m.InsertIgnore(ctx, session, r)
		if err != nil {
			return 0, err
		}
		if !created {
			want--
		}
		n++
	}
	_ = n
	return m.rowsAffected("pending.InsertIgnoreMany", want), nil
}

func (m pendingModel) ClaimDue(_ context.Context, topic string, now, until int64,
	worker string, limit int32) ([]*model.PendingDelivery, error) {
	m.tick("pending.ClaimDue")
	if err := m.errAt("pending.ClaimDue"); err != nil {
		return nil, err
	}
	if worker == "" {
		return nil, model.ErrOperatorRequired
	}
	if limit <= 0 {
		return nil, model.ErrBatchLimitTooLarge
	}
	if limit > 500 {
		limit = 500
	}
	if until <= now {
		return nil, model.ErrInvalidStateTransition
	}
	var ids []int64
	for _, p := range m.pending {
		if !model.DeliveryStateOpen(p.State) {
			continue
		}
		if topic != "" && p.Topic != topic {
			continue
		}
		if p.NextRetryAt > now {
			continue
		}
		if p.LeaseUntil != 0 && p.LeaseUntil > now {
			continue
		}
		ids = append(ids, p.ID)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := m.pending[ids[i]], m.pending[ids[j]]
		if a.NextRetryAt != b.NextRetryAt {
			return a.NextRetryAt < b.NextRetryAt
		}
		return a.ID < b.ID
	})
	if int64(len(ids)) > int64(limit) {
		ids = ids[:limit]
	}
	out := make([]*model.PendingDelivery, 0, len(ids))
	for _, id := range ids {
		p := m.pending[id]
		p.LeaseOwner = worker
		p.LeaseUntil = until
		p.Mtime = now
		cp := *p
		out = append(out, &cp)
	}
	return out, nil
}

// fence 复刻真实实现的 WHERE id=? AND lease_owner=?。
func (m pendingModel) fence(id int64, worker string) (*model.PendingDelivery, bool) {
	if id <= 0 || worker == "" {
		return nil, false
	}
	p, ok := m.pending[id]
	if !ok || p.LeaseOwner != worker {
		return nil, false
	}
	return p, true
}

func (m pendingModel) MarkSent(_ context.Context, id int64, worker string, sentAt int64) (bool, error) {
	m.tick("pending.MarkSent")
	if err := m.errAt("pending.MarkSent"); err != nil {
		return false, err
	}
	if id <= 0 || worker == "" {
		return false, model.ErrPendingNotFound
	}
	p, ok := m.fence(id, worker)
	if !ok {
		return false, nil
	}
	p.State = model.DeliveryStateSent
	p.SentAt = sentAt
	p.LeaseOwner, p.LeaseUntil = "", 0
	p.LastError = ""
	p.Mtime = m.stamp()
	return true, nil
}

func (m pendingModel) MarkRetry(_ context.Context, id int64, worker string, attempts int32,
	nextRetryAt int64, lastError string) (bool, error) {
	m.tick("pending.MarkRetry")
	if err := m.errAt("pending.MarkRetry"); err != nil {
		return false, err
	}
	if id <= 0 || worker == "" {
		return false, model.ErrPendingNotFound
	}
	if !model.ValidDeliveryState(model.DeliveryStateRetrying) {
		return false, model.ErrInvalidStateTransition
	}
	if nextRetryAt <= 0 {
		return false, model.ErrInvalidStateTransition
	}
	p, ok := m.fence(id, worker)
	if !ok {
		return false, nil
	}
	p.State = model.DeliveryStateRetrying
	p.Attempts = attempts
	p.NextRetryAt = nextRetryAt
	p.LastError = clip(lastError, 512)
	p.LeaseOwner, p.LeaseUntil = "", 0
	p.Mtime = m.stamp()
	return true, nil
}

func (m pendingModel) MarkDead(_ context.Context, id int64, worker string,
	attempts int32, lastError string) (bool, error) {
	m.tick("pending.MarkDead")
	if err := m.errAt("pending.MarkDead"); err != nil {
		return false, err
	}
	if id <= 0 || worker == "" {
		return false, model.ErrPendingNotFound
	}
	p, ok := m.fence(id, worker)
	if !ok {
		return false, nil
	}
	p.State = model.DeliveryStateDead
	p.Attempts = attempts
	p.LastError = clip(lastError, 512)
	p.LeaseOwner, p.LeaseUntil = "", 0
	p.Mtime = m.stamp()
	return true, nil
}

func (m pendingModel) ReleaseLease(_ context.Context, id int64, worker string) (bool, error) {
	m.tick("pending.ReleaseLease")
	if err := m.errAt("pending.ReleaseLease"); err != nil {
		return false, err
	}
	if id <= 0 || worker == "" {
		return false, model.ErrPendingNotFound
	}
	p, ok := m.fence(id, worker)
	if !ok || !model.DeliveryStateOpen(p.State) {
		return false, nil
	}
	p.LeaseOwner, p.LeaseUntil = "", 0
	p.Mtime = m.stamp()
	return true, nil
}

func (m pendingModel) Requeue(_ context.Context, _ sqlx.Session, eventID, topic,
	lastError string) (bool, error) {
	m.tick("pending.Requeue")
	if err := m.errAt("pending.Requeue"); err != nil {
		return false, err
	}
	if eventID == "" || topic == "" {
		return false, model.ErrEventIDRequired
	}
	p, ok := m.byEventTopic(eventID, topic)
	if !ok || p.State != model.DeliveryStateDead {
		return false, nil
	}
	p.State = model.DeliveryStatePending
	p.Attempts = 0
	p.NextRetryAt = 0
	p.LeaseOwner, p.LeaseUntil, p.SentAt = "", 0, 0
	p.LastError = clip(lastError, 512)
	p.Mtime = m.stamp()
	return true, nil
}

func (m pendingModel) ReapExpiredLeases(_ context.Context, now int64, limit int32) (int64, error) {
	m.tick("pending.ReapExpiredLeases")
	if err := m.errAt("pending.ReapExpiredLeases"); err != nil {
		return 0, err
	}
	if now <= 0 {
		return 0, model.ErrInvalidPage
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		return 0, model.ErrBatchLimitTooLarge
	}
	var n int64
	for _, p := range m.pending {
		if p.LeaseUntil == 0 || p.LeaseUntil >= now || !model.DeliveryStateOpen(p.State) {
			continue
		}
		p.LeaseOwner, p.LeaseUntil = "", 0
		p.Mtime = now
		if n++; n >= int64(limit) {
			break
		}
	}
	return n, nil
}

func (m pendingModel) TopicStats(_ context.Context, since int64) ([]model.TopicStat, error) {
	m.tick("pending.TopicStats")
	if err := m.errAt("pending.TopicStats"); err != nil {
		return nil, err
	}
	stat := map[string]*model.TopicStat{}
	for _, p := range m.pending {
		s, ok := stat[p.Topic]
		if !ok {
			s = &model.TopicStat{Topic: p.Topic}
			stat[p.Topic] = s
		}
		switch {
		case p.State == model.DeliveryStatePending:
			s.Pending++
			if s.OldestPendingCtime == 0 || p.Ctime < s.OldestPendingCtime {
				s.OldestPendingCtime = p.Ctime
			}
		case p.State == model.DeliveryStateRetrying:
			s.Retrying++
			if s.OldestPendingCtime == 0 || p.Ctime < s.OldestPendingCtime {
				s.OldestPendingCtime = p.Ctime
			}
		case p.State == model.DeliveryStateSent && p.SentAt >= since:
			s.SentLastHour++
		}
	}
	out := make([]model.TopicStat, 0, len(stat))
	for _, s := range stat {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Topic < out[j].Topic })
	return out, nil
}

func (m pendingModel) CountOpen(_ context.Context) (int64, error) {
	m.tick("pending.CountOpen")
	var n int64
	for _, p := range m.pending {
		if model.DeliveryStateOpen(p.State) {
			n++
		}
	}
	return n, nil
}

func (m pendingModel) ListByEventID(_ context.Context, eventID string) ([]*model.PendingDelivery, error) {
	m.tick("pending.ListByEventID")
	if err := m.errAt("pending.ListByEventID"); err != nil {
		return nil, err
	}
	if eventID == "" {
		return nil, model.ErrEventIDRequired
	}
	var out []*model.PendingDelivery
	for _, p := range m.pending {
		if p.EventID == eventID {
			cp := *p
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m pendingModel) DeleteSentBefore(_ context.Context, ctimeBefore int64, limit int32) (int64, error) {
	m.tick("pending.DeleteSentBefore")
	if ctimeBefore <= 0 {
		return 0, model.ErrInvalidPage
	}
	var n int64
	for id, p := range m.pending {
		if p.State == model.DeliveryStateSent && p.Ctime < ctimeBefore && n < int64(limit) {
			delete(m.pending, id)
			n++
		}
	}
	return n, nil
}

func (m pendingModel) WithSession(_ sqlx.Session) model.PendingDeliveryModel { return m }

// -------------------------------------------------------------------- 死信

type deadModel struct{ *fakeDB }

var _ model.DeadLetterModel = deadModel{}

func (m deadModel) InsertIgnore(_ context.Context, _ sqlx.Session,
	d *model.DeadLetter) (bool, error) {
	m.tick("dead.InsertIgnore")
	if err := m.errAt("dead.InsertIgnore"); err != nil {
		return false, err
	}
	if d == nil || d.EventID == "" || d.Topic == "" {
		return false, model.ErrEventIDRequired
	}
	state := d.State
	if state == "" {
		state = model.DeadLetterOpen
	}
	if !model.ValidDeadLetterState(state) {
		return false, model.ErrInvalidStateTransition
	}
	for _, row := range m.dead {
		if row.EventID == d.EventID && row.Topic == d.Topic {
			return false, nil // uniq_event_topic
		}
	}
	cp := *d
	cp.ID = m.id()
	cp.State = state
	ts := m.stamp()
	if cp.CreatedAt == 0 {
		cp.CreatedAt = ts
	}
	cp.Ctime, cp.Mtime = ts, ts
	m.dead[cp.ID] = &cp
	return true, nil
}

func (m deadModel) FindByID(_ context.Context, id int64) (*model.DeadLetter, error) {
	m.tick("dead.FindByID")
	if err := m.errAt("dead.FindByID"); err != nil {
		return nil, err
	}
	d, ok := m.dead[id]
	if !ok {
		return nil, model.ErrDeadLetterNotFound
	}
	cp := *d
	return &cp, nil
}

func (m deadModel) ListByIDs(_ context.Context, _ sqlx.Session, ids []int64) ([]*model.DeadLetter, error) {
	m.tick("dead.ListByIDs")
	if err := m.errAt("dead.ListByIDs"); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > 500 {
		return nil, model.ErrBatchLimitTooLarge
	}
	var out []*model.DeadLetter
	for _, id := range ids {
		if id <= 0 {
			return nil, model.ErrDeadLetterNotFound
		}
		if d, ok := m.dead[id]; ok {
			cp := *d
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m deadModel) MarkHandled(_ context.Context, _ sqlx.Session, ids []int64, to, operator,
	replayKey, reason string, handledAt int64) (int64, error) {
	m.tick("dead.MarkHandled")
	if err := m.errAt("dead.MarkHandled"); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if len(ids) > 500 {
		return 0, model.ErrBatchLimitTooLarge
	}
	if !model.ValidDeadLetterState(to) || to == model.DeadLetterOpen {
		return 0, model.ErrInvalidStateTransition
	}
	if strings.TrimSpace(operator) == "" || strings.TrimSpace(reason) == "" {
		return 0, model.ErrOperatorRequired
	}
	var applied int64
	for _, id := range ids {
		if id <= 0 {
			return 0, model.ErrDeadLetterNotFound
		}
		d, ok := m.dead[id]
		if !ok || d.State != model.DeadLetterOpen {
			continue // WHERE state='open'
		}
		d.State = to
		d.Operator = operator
		d.ReplayKey = replayKey
		d.ReplayReason = clip(reason, 512)
		d.HandledAt = handledAt
		d.Mtime = m.stamp()
		applied++
	}
	return applied, nil
}

func (m deadModel) List(_ context.Context, topic, state string, from, to int64,
	cur model.Cursor, ps int32) ([]*model.DeadLetter, error) {
	m.tick("dead.List")
	if err := m.errAt("dead.List"); err != nil {
		return nil, err
	}
	if ps <= 0 {
		return nil, model.ErrInvalidPage
	}
	if state != "" && !model.ValidDeadLetterState(state) {
		return nil, model.ErrInvalidStateTransition
	}
	var rows []*model.DeadLetter
	for _, d := range m.dead {
		if matchDeadFilter(d, topic, state, from, to) && withinCursor(d.Ctime, d.ID, cur) {
			cp := *d
			rows = append(rows, &cp)
		}
	}
	sortDeadByCtimeDesc(rows)
	return clipDeadRows(rows, ps), nil
}

func (m deadModel) Count(_ context.Context, topic, state string, from, to int64) (int64, error) {
	m.tick("dead.Count")
	if err := m.errAt("dead.Count"); err != nil {
		return 0, err
	}
	if state != "" && !model.ValidDeadLetterState(state) {
		return 0, model.ErrInvalidStateTransition
	}
	var n int64
	for _, d := range m.dead {
		if matchDeadFilter(d, topic, state, from, to) {
			n++
		}
	}
	return n, nil
}

func (m deadModel) CountOpenByTopic(_ context.Context) (map[string]int64, error) {
	m.tick("dead.CountOpenByTopic")
	if err := m.errAt("dead.CountOpenByTopic"); err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, d := range m.dead {
		if d.State == model.DeadLetterOpen {
			out[d.Topic]++
		}
	}
	return out, nil
}

func (m deadModel) DeleteBefore(_ context.Context, ctimeBefore int64, limit int32) (int64, error) {
	m.tick("dead.DeleteBefore")
	if ctimeBefore <= 0 {
		return 0, model.ErrInvalidPage
	}
	var n int64
	for id, d := range m.dead {
		if d.State == model.DeadLetterOpen {
			continue
		}
		if d.Ctime < ctimeBefore && n < int64(limit) {
			delete(m.dead, id)
			n++
		}
	}
	return n, nil
}

func (m deadModel) WithSession(_ sqlx.Session) model.DeadLetterModel { return m }

func matchDeadFilter(d *model.DeadLetter, topic, state string, from, to int64) bool {
	switch {
	case topic != "" && d.Topic != topic:
		return false
	case state != "" && d.State != state:
		return false
	case from != 0 && d.Ctime < from:
		return false
	case to != 0 && d.Ctime > to:
		return false
	}
	return true
}

// ------------------------------------------------------------------ 公共小工具

func inInt32(set []int32, v int32) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

func clip(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max]
}

func withinCursor(ctime, id int64, cur model.Cursor) bool {
	if cur.ID <= 0 {
		return true
	}
	return ctime < cur.Ctime || (ctime == cur.Ctime && id < cur.ID)
}

type batchRow interface{ ctimeID() (int64, int64) }

func sortByCtimeDesc(rows []*model.IngestBatch) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Ctime != rows[j].Ctime {
			return rows[i].Ctime > rows[j].Ctime
		}
		return rows[i].ID > rows[j].ID
	})
}

func clipRows(rows []*model.IngestBatch, ps int32) []*model.IngestBatch {
	if int32(len(rows)) > ps {
		return rows[:ps]
	}
	return rows
}

func sortRecordsByCtimeDesc(rows []*model.EventRecord) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Ctime != rows[j].Ctime {
			return rows[i].Ctime > rows[j].Ctime
		}
		return rows[i].ID > rows[j].ID
	})
}

func clipRecordRows(rows []*model.EventRecord, ps int32) []*model.EventRecord {
	if int32(len(rows)) > ps {
		return rows[:ps]
	}
	return rows
}

func sortDeadByCtimeDesc(rows []*model.DeadLetter) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Ctime != rows[j].Ctime {
			return rows[i].Ctime > rows[j].Ctime
		}
		return rows[i].ID > rows[j].ID
	})
}

func clipDeadRows(rows []*model.DeadLetter, ps int32) []*model.DeadLetter {
	if int32(len(rows)) > ps {
		return rows[:ps]
	}
	return rows
}

func clipPolicyRows(rows []*model.DispatchPolicy, ps int32) []*model.DispatchPolicy {
	if int32(len(rows)) > ps {
		return rows[:ps]
	}
	return rows
}

// ------------------------------------------------------------------ 其他依赖

// fakeLimiter 实现 common/ratelimit.Limiter：注入 ErrLimitExceed / ErrDeadline /
// 任意非哨兵故障，用来验证「限流不可用时的降级方向」（AGENTS.md §5）。
type fakeLimiter struct {
	err      error
	hits     int
	ops      []ratelimit.Op
	doneCall int
}

var _ ratelimit.Limiter = (*fakeLimiter)(nil)

func (l *fakeLimiter) Allow(_ context.Context) (func(ratelimit.Op), error) {
	l.hits++
	if l.err != nil {
		return nil, l.err
	}
	return func(op ratelimit.Op) {
		l.doneCall++
		l.ops = append(l.ops, op)
	}, nil
}

// fakeSender 实现包内 pendingSender：按 event_id 注入失败，验证退避/死信分支。
type fakeSender struct {
	calls  []*model.PendingDelivery
	errFor map[string]error
	err    error
}

var _ pendingSender = (*fakeSender)(nil)

func (s *fakeSender) Send(_ context.Context, row *model.PendingDelivery) error {
	cp := *row
	s.calls = append(s.calls, &cp)
	if err := s.errFor[row.EventID]; err != nil {
		return err
	}
	return s.err
}

func (s *fakeSender) sentTopics() []string {
	var out []string
	for _, r := range s.calls {
		out = append(out, r.Topic)
	}
	return out
}

// fakeLogWriter 捕获 logx 输出，用于断言「错误路径不落敏感明文」。
type fakeLogWriter struct {
	mu   sync.Mutex
	text []string
}

func (w *fakeLogWriter) Alert(v any)                          { w.add("severe", v) }
func (w *fakeLogWriter) Close() error                         { return nil }
func (w *fakeLogWriter) Debug(v any, fields ...logx.LogField) { w.add("debug", v, fields...) }
func (w *fakeLogWriter) Error(v any, fields ...logx.LogField) { w.add("error", v, fields...) }
func (w *fakeLogWriter) Info(v any, fields ...logx.LogField)  { w.add("info", v, fields...) }
func (w *fakeLogWriter) Severe(v any)                         { w.add("severe", v) }
func (w *fakeLogWriter) Slow(v any, fields ...logx.LogField)  { w.add("slow", v, fields...) }
func (w *fakeLogWriter) Stack(v any)                          { w.add("error", v) }
func (w *fakeLogWriter) Stat(v any, fields ...logx.LogField)  { w.add("stat", v, fields...) }

// fieldsText 把结构化字段并成 `key=value` 一起留痕：logx 把 Infow 的字段作为可变参数
// 传给每个 writer，忽略它们就等于让按字段值做的断言全部失效（正向永假、隐私反向永真）。
func fieldsText(fields []logx.LogField) string {
	var b strings.Builder
	for _, f := range fields {
		fmt.Fprintf(&b, " %s=%v", f.Key, f.Value)
	}
	return b.String()
}

func (w *fakeLogWriter) add(kind string, v any, fields ...logx.LogField) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.text = append(w.text, kind+" "+fmt.Sprint(v)+fieldsText(fields))
}

func (w *fakeLogWriter) errors() []string { return w.filter("error ", "severe ") }
func (w *fakeLogWriter) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.text...)
}

func (w *fakeLogWriter) filter(prefixes ...string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, line := range w.text {
		for _, p := range prefixes {
			if strings.HasPrefix(line, p) {
				out = append(out, line)
				break
			}
		}
	}
	return out
}

func (w *fakeLogWriter) joined() string    { return strings.Join(w.errors(), "\n") }
func (w *fakeLogWriter) joinedAll() string { return strings.Join(w.all(), "\n") }
