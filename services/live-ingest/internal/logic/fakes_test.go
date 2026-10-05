// 本文件是 live-ingest logic 测试的内存替身：一张「假库」+ 9 个 model 接口的内存实现
// + 一个只实现 TransactCtx 的假连接。
//
// 为什么可以不改实现就打针进来（AGENTS.md §4 的分层正好给了这个缝隙）：
//   - repository.Repository 的 9 个 model 字段是**导出接口字段**，测试可整体替换；
//   - repository.New 接受任意 sqlx.SqlConn，本服务的事务入口只有 TransactCtx 一个方法；
//   - logic 从不直接用 Redis（缓存键空间只是文档承诺），所以 Redis 留 nil 即可。
//
// 语义保真要求（替身不保真，测试就是自证清白）：
//   - TransactCtx：成功即提交，失败整库还原 —— 等价 MySQL 回滚，
//     所以「拒绝的请求零副作用」是可被证伪的断言而不是口号；
//   - 唯一索引冲突必须回带 MySQL 原文案（Error 1062 / Duplicate entry），
//     否则 model.IsDuplicate 那条幂等分支根本没被执行到；
//   - report_id / publish_request 空值不参与唯一冲突（真库用 nullableString 落 NULL）；
//   - 状态迁移一律条件 UPDATE + RowsAffected 判定，替身照抄这个形状，
//     否则「CAS 输了」这一类分支永远测不到。
//
// 未实现的方法：每个替身都内嵌了真实 model 接口（值为 nil）。
// 一旦被测代码调用到替身没覆盖的方法，会立刻 panic 在方法名上——
// 这是刻意保留的响铃：它等价于「logic 偷偷写了我没建模的表」，不能静默放过。
package logic

import (
	"context"
	"fmt"
	"sort"

	"go-video/services/live-ingest/model"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// fakeDB 是 9 张 live_* 表的内存投影。
type fakeDB struct {
	keys      map[int64]*model.StreamKey
	streams   map[string]*model.Stream
	nodes     map[string]*model.IngestNode
	assigns   map[int64]*model.NodeAssignment
	events    map[int64]*model.StreamEvent
	outbox    map[int64]*model.EventOutbox
	callbacks map[int64]*model.CdnCallback
	interrupt map[int64]*model.StreamInterruption
	reports   map[int64]*model.StreamHealthReport

	nextID  int64
	txCalls int
	// methodCalls 记录「被测代码调了哪个 model 方法几次」，用于钉住
	// 「读路径不写库」「重放不再占配额」这类次数契约。
	methodCalls map[string]int
	// callLog 是 methodCalls 的**有序**留痕（同名方法每调一次就追加一项）。
	// 计数断言区分不开「顺序不同但次数相同」的两组写入：
	// 「先释放租约再退配额」与「先退配额再释放租约」的 methodCalls 完全一样，
	// 而前者才是正确的（配额退早了，租约 CAS 失败回滚时会少退一次配额；
	// 停流的收尾三件事的先后还决定「失败时库里剩什么」）。
	//
	// 记的是**尝试**：snapshot/restore 不回滚计数器与日志，因此被回滚的事务里
	// 那些调用仍然留在日志上——与真 MySQL「事务内 SQL 确实发出去过、只是没提交」一致。
	// 用例若只想看提交成功的那一段，配合 requireSameEffects/requireDelta 一起断言。
	callLog []string

	// 下面三个开关复刻「另一事务抢先提交」：真库里读到的行与条件 UPDATE 命中的行
	// 之间隔着并发写，替身读写同一行就永远造不出 CAS 失败，
	// 「锁输了不许谎报成功」这一类分支会静默变成不可达。
	// forceTransitionMiss 让 Stream.ApplyTransition 无条件回 false（状态/seq 已抢走）。
	forceTransitionMiss bool
	// forceHealthApplyMiss 让 Stream.ApplyHealthTx 无条件回 false（流在事务外被停掉）。
	forceHealthApplyMiss bool
	// forceReportDuplicate 让 StreamHealthReport.Insert 无条件回 1062（同 report_id 并发上报）。
	forceReportDuplicate bool
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		keys:        map[int64]*model.StreamKey{},
		streams:     map[string]*model.Stream{},
		nodes:       map[string]*model.IngestNode{},
		assigns:     map[int64]*model.NodeAssignment{},
		events:      map[int64]*model.StreamEvent{},
		outbox:      map[int64]*model.EventOutbox{},
		callbacks:   map[int64]*model.CdnCallback{},
		interrupt:   map[int64]*model.StreamInterruption{},
		reports:     map[int64]*model.StreamHealthReport{},
		methodCalls: map[string]int{},
	}
}

func (db *fakeDB) id() int64 { db.nextID++; return db.nextID }

// call 记一次方法调用（名称形如 "Stream.ApplyTransition"）。
func (db *fakeDB) call(name string) int { return db.methodCalls[name] }

// bump 是所有 model 方法唯一的计数入口，顺序留痕也在这里一并记录：
// 任何新增替身方法只要调 bump，就自动进入 callLog，不会出现「有计数没顺序」的死角。
func (db *fakeDB) bump(name string) {
	db.methodCalls[name]++
	db.callLog = append(db.callLog, name)
}

// dupErr 复刻 go-sql-driver 的唯一索引冲突文案：model.IsDuplicate 靠它识别重放。
func dupErr(index, value string) error {
	return fmt.Errorf("Error 1062 (23000): Duplicate entry '%s' for key '%s'", value, index)
}

func clonePtrMap[K comparable, V any](src map[K]*V) map[K]*V {
	dst := make(map[K]*V, len(src))
	for k, v := range src {
		if v == nil {
			dst[k] = nil
			continue
		}
		cp := *v
		dst[k] = &cp
	}
	return dst
}

type dbSnapshot struct {
	keys      map[int64]*model.StreamKey
	streams   map[string]*model.Stream
	nodes     map[string]*model.IngestNode
	assigns   map[int64]*model.NodeAssignment
	events    map[int64]*model.StreamEvent
	outbox    map[int64]*model.EventOutbox
	callbacks map[int64]*model.CdnCallback
	interrupt map[int64]*model.StreamInterruption
	reports   map[int64]*model.StreamHealthReport
	nextID    int64
}

func (db *fakeDB) snapshot() dbSnapshot {
	return dbSnapshot{
		keys: clonePtrMap(db.keys), streams: clonePtrMap(db.streams), nodes: clonePtrMap(db.nodes),
		assigns: clonePtrMap(db.assigns), events: clonePtrMap(db.events), outbox: clonePtrMap(db.outbox),
		callbacks: clonePtrMap(db.callbacks), interrupt: clonePtrMap(db.interrupt),
		reports: clonePtrMap(db.reports), nextID: db.nextID,
	}
}

func (db *fakeDB) restore(s dbSnapshot) {
	db.keys, db.streams, db.nodes = s.keys, s.streams, s.nodes
	db.assigns, db.events, db.outbox = s.assigns, s.events, s.outbox
	db.callbacks, db.interrupt, db.reports = s.callbacks, s.interrupt, s.reports
	db.nextID = s.nextID
}

// fakeConn 只实现被测代码真正用到的 TransactCtx；其余方法内嵌 nil 接口，
// 被调用即 panic（等价于「logic 用了我没建模的事务形态」）。
type fakeConn struct {
	sqlx.SqlConn
	db *fakeDB
}

func (c *fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	c.db.txCalls++
	snap := c.db.snapshot()
	if err := fn(ctx, &fakeTx{}); err != nil {
		c.db.restore(snap)
		return err
	}
	return nil
}

// fakeTx 只需要「非 nil」：applyStreamTransition 用 tx == nil 判定误用在事务外。
type fakeTx struct{ sqlx.Session }

// --- live_stream ---

type fakeStream struct {
	model.StreamModel
	db *fakeDB
}

func (f *fakeStream) Insert(_ context.Context, _ sqlx.Session, s *model.Stream) error {
	f.db.bump("Stream.Insert")
	if s == nil || s.StreamID == "" {
		return model.ErrInvalidStreamId
	}
	if _, ok := f.db.streams[s.StreamID]; ok {
		return dupErr("PRIMARY", s.StreamID)
	}
	if s.PublishRequestID != "" {
		for _, row := range f.db.streams {
			if row.PublishRequestID == s.PublishRequestID {
				return dupErr("uniq_publish_request", s.PublishRequestID)
			}
		}
	}
	cp := *s
	if cp.State == 0 {
		cp.State = model.StreamStateIdle
	}
	if cp.HealthState == 0 {
		cp.HealthState = model.HealthStateNoData
	}
	cp.Ctime, cp.Mtime = f.db.nowOr(cp.Ctime), f.db.nowOr(cp.Ctime)
	f.db.streams[cp.StreamID] = &cp
	return nil
}

func (f *fakeStream) FindOne(_ context.Context, streamID string) (*model.Stream, error) {
	f.db.bump("Stream.FindOne")
	return cloneStream(f.db.streams[streamID]), nil
}

func (f *fakeStream) LockByID(ctx context.Context, _ sqlx.Session, streamID string) (*model.Stream, error) {
	f.db.bump("Stream.LockByID")
	return f.FindOne(ctx, streamID)
}

func (f *fakeStream) FindByPublishRequest(_ context.Context, requestID string) (*model.Stream, error) {
	f.db.bump("Stream.FindByPublishRequest")
	if requestID == "" {
		return nil, model.ErrIdempotencyKeyRequired
	}
	for _, row := range f.db.streams {
		if row.PublishRequestID == requestID {
			return cloneStream(row), nil
		}
	}
	return nil, nil
}

// FindActiveByRoom 复刻真 SQL：WHERE room_id=? AND state IN（非终态） ORDER BY ctime DESC LIMIT 1。
// 与 FindActiveByStreamName 同样按 ctime 取最新、stream_id 兜底去确定化：
// 真 MySQL 里 ctime 相同的多行「取哪一行」是未定义的，所以用例必须给候选行铺不同的 ctime，
// 不能靠这里的兜底次序立断言（否则替身的排序规则会被误当成契约）。
func (f *fakeStream) FindActiveByRoom(_ context.Context, roomID int64) (*model.Stream, error) {
	f.db.bump("Stream.FindActiveByRoom")
	var best *model.Stream
	for _, row := range f.db.streams {
		if row.RoomID != roomID || model.TerminalStreamState(row.State) {
			continue
		}
		if best == nil || row.Ctime > best.Ctime || (row.Ctime == best.Ctime && row.StreamID > best.StreamID) {
			best = row
		}
	}
	return cloneStream(best), nil
}

func (f *fakeStream) FindActiveByStreamName(_ context.Context, streamName string) (*model.Stream, error) {
	f.db.bump("Stream.FindActiveByStreamName")
	var best *model.Stream
	for _, row := range f.db.streams {
		if row.StreamName != streamName || model.TerminalStreamState(row.State) {
			continue
		}
		if best == nil || row.StreamID > best.StreamID {
			best = row
		}
	}
	return cloneStream(best), nil
}

// ListActiveByKey 复刻真 SQL：WHERE key_id=? AND state IN（非终态）
// ORDER BY stream_id ASC LIMIT clampLimit(limit,100)。
// 排序与上限都要照抄：吊销级联停流的「停掉几条」完全由这两者决定，
// 若替身按 map 迭代序返回，「级联越界多停了别的密钥的流」就测不出来。
func (f *fakeStream) ListActiveByKey(_ context.Context, keyID int64, limit int32) ([]*model.Stream, error) {
	f.db.bump("Stream.ListActiveByKey")
	if keyID <= 0 {
		return nil, model.ErrInvalidKeyId
	}
	var out []*model.Stream
	for _, row := range f.db.streams {
		if row.KeyID != keyID || model.TerminalStreamState(row.State) {
			continue
		}
		out = append(out, cloneStream(row))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StreamID < out[j].StreamID })
	max := limit
	if max <= 0 {
		max = 20 // clampLimit(0,100) = 100/5
	}
	if max > 100 {
		max = 100
	}
	if int(max) < len(out) {
		out = out[:max]
	}
	return out, nil
}

// ApplyTransition 复刻真 SQL 的形状：WHERE stream_id=? AND state=? AND seq=newSeq-1。
// 任一条件不满足即 RowsAffected=0，调用方拿 false 走 ErrConcurrentUpdate。
func (f *fakeStream) ApplyTransition(_ context.Context, _ sqlx.Session, streamID string,
	fromState, toState int32, newSeq, at int64, stopReason int32, stopDetail string) (bool, error) {
	f.db.bump("Stream.ApplyTransition")
	if f.db.forceTransitionMiss {
		// 等价于「读完之后、写回之前，另一事务把 state/seq 抢走了」。
		return false, nil
	}
	row := f.db.streams[streamID]
	if row == nil || row.State != fromState || row.Seq != newSeq-1 {
		return false, nil
	}
	row.State = toState
	row.Seq = newSeq
	row.StateChangedAt = at
	if toState == model.StreamStatePublishing && row.PublishStartedAt == 0 {
		row.PublishStartedAt = at
	}
	if toState == model.StreamStateStopped {
		row.StopReason = stopReason
		row.StopDetail = stopDetail
	}
	return true, nil
}

// ApplyHealthTx 复刻真 SQL：八列 SET（含 last_heartbeat_at = GREATEST(旧值, ?)）
// + WHERE stream_id=? AND state IN（非终态），RowsAffected 判定。
//
// GREATEST 与「只改非终态」两处都必须照抄：
//   - 前者是「迟到的采样不能把心跳拉回去」的唯一实现，写成直接赋值就永远测不到；
//   - 后者让「事务外已被停流」的采样拿到 false，logic 才会整笔回滚而不是留下无主样本。
func (f *fakeStream) ApplyHealthTx(_ context.Context, _ sqlx.Session, streamID string,
	p model.StreamHealthPatch) (bool, error) {
	f.db.bump("Stream.ApplyHealthTx")
	if f.db.forceHealthApplyMiss {
		return false, nil
	}
	row := f.db.streams[streamID]
	if row == nil || model.TerminalStreamState(row.State) {
		return false, nil
	}
	if p.TriggeredInterrupt {
		// 触发过断流的采样必须留下 CRITICAL 痕迹，不能被「暂时正常」的采样抹掉。
		p.HealthState = model.HealthStateCritical
	}
	row.HealthState = p.HealthState
	row.HealthReportedAt = p.ReportedAt
	row.VideoBitrateBps = p.VideoBitrateBps
	row.AudioBitrateBps = p.AudioBitrateBps
	row.FpsX100 = p.FpsX100
	row.PacketLossPpm = p.PacketLossPpm
	if p.ReportedAt > row.LastHeartbeatAt {
		row.LastHeartbeatAt = p.ReportedAt
	}
	return true, nil
}

func (f *fakeStream) OpenInterruption(_ context.Context, _ sqlx.Session, streamID string) (bool, error) {
	f.db.bump("Stream.OpenInterruption")
	row := f.db.streams[streamID]
	if row == nil {
		return false, nil
	}
	row.InterruptionCount++
	return true, nil
}

func (f *fakeStream) CloseInterruption(_ context.Context, _ sqlx.Session, streamID string, seconds int64) (bool, error) {
	f.db.bump("Stream.CloseInterruption")
	row := f.db.streams[streamID]
	if row == nil {
		return false, nil
	}
	row.InterruptedTotalSecs += seconds
	return true, nil
}

func (f *fakeStream) SetNode(_ context.Context, _ sqlx.Session, streamID, nodeID string, expectPrev string) (bool, error) {
	f.db.bump("Stream.SetNode")
	row := f.db.streams[streamID]
	if row == nil || row.NodeID != expectPrev {
		return false, nil
	}
	row.NodeID = nodeID
	return true, nil
}

// MarkStopped 真 SQL 带 state=STOPPED 条件且忽略 RowsAffected，这里照抄。
// 计数留痕是「读路径不写库」探针的一部分：健康/事件读侧一旦顺手补一刀停流，必须看得见。
func (f *fakeStream) MarkStopped(_ context.Context, _ sqlx.Session, streamID string) error {
	f.db.bump("Stream.MarkStopped")
	if row := f.db.streams[streamID]; row != nil && row.State == model.StreamStateStopped {
		row.HealthState = model.HealthStateNoData
	}
	return nil
}

func (f *fakeStream) ListByFilter(_ context.Context, flt model.StreamFilter) ([]*model.Stream, error) {
	f.db.bump("Stream.ListByFilter")
	rows := filterStreams(f.db.streams, flt)
	// 真 SQL：ORDER BY last_heartbeat_at ASC（最久没心跳的排最前，巡检第一页就是要处理的）
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].LastHeartbeatAt != rows[j].LastHeartbeatAt {
			return rows[i].LastHeartbeatAt < rows[j].LastHeartbeatAt
		}
		return rows[i].StreamID < rows[j].StreamID
	})
	if flt.Offset > 0 && int(flt.Offset) >= len(rows) {
		return nil, nil
	}
	if flt.Offset > 0 {
		rows = rows[flt.Offset:]
	}
	if flt.Limit > 0 && int(flt.Limit) < len(rows) {
		rows = rows[:flt.Limit]
	}
	return rows, nil
}

func (f *fakeStream) CountByFilter(_ context.Context, flt model.StreamFilter) (int64, error) {
	return int64(len(filterStreams(f.db.streams, flt))), nil
}

func filterStreams(all map[string]*model.Stream, flt model.StreamFilter) []*model.Stream {
	activeOnly := flt.State == 0
	var out []*model.Stream
	for _, row := range all {
		if len(flt.RoomIDs) > 0 && !containsInt64(flt.RoomIDs, row.RoomID) {
			continue
		}
		if flt.AnchorMid > 0 && row.AnchorMid != flt.AnchorMid {
			continue
		}
		if flt.NodeID != "" && row.NodeID != flt.NodeID {
			continue
		}
		if activeOnly && model.TerminalStreamState(row.State) {
			continue
		}
		if !activeOnly && row.State != flt.State {
			continue
		}
		if flt.Protocol > 0 && row.Protocol != flt.Protocol {
			continue
		}
		if flt.MaxHeartbeat > 0 && row.LastHeartbeatAt > flt.MaxHeartbeat {
			continue
		}
		out = append(out, cloneStream(row))
	}
	return out
}

func containsInt64(list []int64, v int64) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func cloneStream(p *model.Stream) *model.Stream {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// nowOr 给替身一个确定的「落库时间」：种子传 0 时用递增计数，避免同一秒内不可分辨。
func (db *fakeDB) nowOr(v int64) int64 {
	if v != 0 {
		return v
	}
	return 1700000000
}

// --- live_stream_key ---

type fakeStreamKey struct {
	model.StreamKeyModel
	db *fakeDB
}

func (f *fakeStreamKey) Insert(_ context.Context, _ sqlx.Session, k *model.StreamKey) (int64, error) {
	f.db.bump("StreamKey.Insert")
	if k == nil || k.StreamName == "" || k.KeyHash == "" {
		return 0, model.ErrKeyRefUnresolvable
	}
	if k.RequestID != "" {
		for _, row := range f.db.keys {
			if row.RequestID == k.RequestID {
				return 0, dupErr("uniq_request_id", k.RequestID)
			}
		}
	}
	for _, row := range f.db.keys {
		if row.KeyHash == k.KeyHash {
			return 0, dupErr("uniq_key_hash", "hash")
		}
	}
	cp := *k
	cp.KeyID = f.db.id()
	if cp.State == 0 {
		cp.State = model.KeyStateActive
	}
	if cp.Version == 0 {
		cp.Version = 1
	}
	cp.Ctime, cp.Mtime = f.db.nowOr(cp.Ctime), f.db.nowOr(cp.Ctime)
	f.db.keys[cp.KeyID] = &cp
	return cp.KeyID, nil
}

func (f *fakeStreamKey) FindOne(_ context.Context, keyID int64) (*model.StreamKey, error) {
	f.db.bump("StreamKey.FindOne")
	return cloneKey(f.db.keys[keyID]), nil
}

func (f *fakeStreamKey) LockByID(ctx context.Context, _ sqlx.Session, keyID int64) (*model.StreamKey, error) {
	f.db.bump("StreamKey.LockByID")
	return f.FindOne(ctx, keyID)
}

func (f *fakeStreamKey) FindByKeyHash(_ context.Context, hash string) (*model.StreamKey, error) {
	f.db.bump("StreamKey.FindByKeyHash")
	if hash == "" {
		return nil, nil
	}
	for _, row := range f.db.keys {
		if row.KeyHash == hash {
			return cloneKey(row), nil
		}
	}
	return nil, nil
}

func (f *fakeStreamKey) FindByIdempotencyRequest(_ context.Context, requestID string) (*model.StreamKey, error) {
	f.db.bump("StreamKey.FindByIdempotencyRequest")
	if requestID == "" {
		return nil, model.ErrIdempotencyKeyRequired
	}
	for _, row := range f.db.keys {
		if row.RequestID == requestID {
			return cloneKey(row), nil
		}
	}
	return nil, nil
}

func (f *fakeStreamKey) FindCurrentByStreamName(_ context.Context, streamName string) (*model.StreamKey, error) {
	var best *model.StreamKey
	for _, row := range f.db.keys {
		if row.StreamName != streamName || !model.AuthAllowedKeyState(row.State) {
			continue
		}
		if best == nil || row.KeyID > best.KeyID {
			best = row
		}
	}
	return cloneKey(best), nil
}

func (f *fakeStreamKey) TransitionState(_ context.Context, _ sqlx.Session, keyID int64,
	toState int32, reason string, fromStates ...int32) (bool, error) {
	f.db.bump("StreamKey.TransitionState")
	row := f.db.keys[keyID]
	if row == nil {
		return false, nil
	}
	matched := false
	for _, from := range fromStates {
		if row.State == from {
			matched = true
			break
		}
	}
	if !matched {
		return false, nil
	}
	row.State = toState
	row.Reason = reason
	return true, nil
}

// LinkRotation 真 SQL 只接受 state=ACTIVE 的旧密钥：第二次轮转必然在 CAS 上失败。
func (f *fakeStreamKey) LinkRotation(_ context.Context, _ sqlx.Session, oldKeyID, newKeyID, graceUntil int64) (bool, error) {
	f.db.bump("StreamKey.LinkRotation")
	row := f.db.keys[oldKeyID]
	if row == nil || row.State != model.KeyStateActive {
		return false, nil
	}
	row.State = model.KeyStateRotating
	row.RotateToKeyID = newKeyID
	row.GraceUntil = graceUntil
	return true, nil
}

// ClaimActiveStream 真 SQL 条件 current_stream_id=”：这才是「一把密钥一条活流」的硬闸门。
func (f *fakeStreamKey) ClaimActiveStream(_ context.Context, _ sqlx.Session, keyID int64, streamID string) (bool, error) {
	f.db.bump("StreamKey.ClaimActiveStream")
	row := f.db.keys[keyID]
	if row == nil || row.CurrentStreamID != "" {
		return false, nil
	}
	row.CurrentStreamID = streamID
	return true, nil
}

func (f *fakeStreamKey) ReleaseActiveStream(_ context.Context, _ sqlx.Session, keyID int64, streamID string) (bool, error) {
	f.db.bump("StreamKey.ReleaseActiveStream")
	row := f.db.keys[keyID]
	if row == nil || row.CurrentStreamID != streamID {
		return false, nil
	}
	row.CurrentStreamID = ""
	return true, nil
}

func (f *fakeStreamKey) CountActiveStreams(_ context.Context, keyID int64) (int64, error) {
	var n int64
	for _, s := range f.db.streams {
		if s.KeyID == keyID && !model.TerminalStreamState(s.State) {
			n++
		}
	}
	return n, nil
}

func (f *fakeStreamKey) ListByFilter(_ context.Context, flt model.StreamKeyFilter) ([]*model.StreamKey, error) {
	out := filterKeys(f.db.keys, flt)
	// 真 SQL：ORDER BY key_id DESC（最新代次在前，运营面第一页就是当前在用的密钥）
	sort.Slice(out, func(i, j int) bool { return out[i].KeyID > out[j].KeyID })
	if flt.Offset > 0 && int(flt.Offset) >= len(out) {
		return nil, nil
	}
	if flt.Offset > 0 {
		out = out[flt.Offset:]
	}
	if flt.Limit > 0 && int(flt.Limit) < len(out) {
		out = out[:flt.Limit]
	}
	return out, nil
}

// CountByFilter 复刻真 SQL：SELECT COUNT(*) WHERE <同一套条件>——不带 ORDER/LIMIT/OFFSET。
// 早先这里直接数 ListByFilter 的返回条数，于是 total 恒等于「本页条数」，
// 「翻到第 2 页 total 掉到 ps」这类真缺陷会被替身自己掩盖掉。
func (f *fakeStreamKey) CountByFilter(_ context.Context, flt model.StreamKeyFilter) (int64, error) {
	return int64(len(filterKeys(f.db.keys, flt))), nil
}

func filterKeys(all map[int64]*model.StreamKey, flt model.StreamKeyFilter) []*model.StreamKey {
	var out []*model.StreamKey
	for _, row := range all {
		if flt.RoomID > 0 && row.RoomID != flt.RoomID {
			continue
		}
		if flt.AnchorMid > 0 && row.AnchorMid != flt.AnchorMid {
			continue
		}
		if flt.State > 0 && row.State != flt.State {
			continue
		}
		out = append(out, cloneKey(row))
	}
	return out
}

func cloneKey(p *model.StreamKey) *model.StreamKey {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// --- live_ingest_node ---

type fakeIngestNode struct {
	model.IngestNodeModel
	db *fakeDB
}

func (f *fakeIngestNode) FindOne(_ context.Context, nodeID string) (*model.IngestNode, error) {
	f.db.bump("IngestNode.FindOne")
	row := f.db.nodes[nodeID]
	if row == nil {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

// Upsert 复刻 model.IngestNodeModel.Upsert 的两条不同 SQL 路径：
//   - 心跳：只碰 active_streams/health_score/last_heartbeat_at/mtime，且 WHERE state<>OFFLINE
//     （不在线的节点心跳必须空转，而不是把注册字段一起刷进库里）；
//   - 注册：ON DUPLICATE KEY UPDATE 的列清单里**没有** active_streams —— 注册不许改派生计数。
func (f *fakeIngestNode) Upsert(_ context.Context, n *model.IngestNode, heartbeatOnly bool) (bool, error) {
	f.db.bump("IngestNode.Upsert")
	if n == nil || n.NodeID == "" {
		return false, model.ErrNodeNotFound
	}
	existing := f.db.nodes[n.NodeID]
	if heartbeatOnly {
		if existing == nil {
			// 真 SQL 此处 RowsAffected=0，model 回查后给 ErrNodeNotFound。
			return false, model.ErrNodeNotFound
		}
		if existing.State == model.NodeStateOffline {
			// WHERE state <> OFFLINE 不命中：一行都不改，也不建档。
			return false, nil
		}
		cp := *existing
		cp.ActiveStreams = n.ActiveStreams
		cp.HealthScore = n.HealthScore
		cp.LastHeartbeatAt = n.LastHeartbeatAt
		f.db.nodes[n.NodeID] = &cp
		return false, nil
	}
	if existing == nil {
		cp := *n
		if cp.State == 0 {
			cp.State = model.NodeStateOnline
		}
		f.db.nodes[cp.NodeID] = &cp
		return true, nil
	}
	keepActive := existing.ActiveStreams
	cp := *n
	cp.ActiveStreams = keepActive
	if cp.State == 0 {
		cp.State = existing.State
	}
	f.db.nodes[cp.NodeID] = &cp
	return false, nil
}

// ListCandidates 复刻真 SQL：ONLINE && capacity>active && 协议位匹配，
// 预排 ORDER health_score DESC, active_streams ASC, node_id ASC LIMIT ?。
func (f *fakeIngestNode) ListCandidates(_ context.Context, protocolMask uint32, region string, limit int32) ([]*model.IngestNode, error) {
	f.db.bump("IngestNode.ListCandidates")
	var out []*model.IngestNode
	for _, row := range f.db.nodes {
		if row.State != model.NodeStateOnline || row.CapacityStreams <= row.ActiveStreams {
			continue
		}
		if protocolMask != 0 && row.ProtocolMask&protocolMask == 0 {
			continue
		}
		if region != "" && row.Region != region {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].HealthScore != out[j].HealthScore {
			return out[i].HealthScore > out[j].HealthScore
		}
		if out[i].ActiveStreams != out[j].ActiveStreams {
			return out[i].ActiveStreams < out[j].ActiveStreams
		}
		return out[i].NodeID < out[j].NodeID
	})
	max := limit
	if max <= 0 {
		max = 10
	}
	if int(max) < len(out) {
		out = out[:max]
	}
	return out, nil
}

func (f *fakeIngestNode) ReserveQuota(_ context.Context, _ sqlx.Session, nodeID string) (bool, error) {
	f.db.bump("IngestNode.ReserveQuota")
	row := f.db.nodes[nodeID]
	if row == nil || row.State != model.NodeStateOnline || row.ActiveStreams >= row.CapacityStreams {
		return false, nil
	}
	row.ActiveStreams++
	return true, nil
}

func (f *fakeIngestNode) ReleaseQuota(_ context.Context, _ sqlx.Session, nodeID string) (bool, error) {
	f.db.bump("IngestNode.ReleaseQuota")
	row := f.db.nodes[nodeID]
	if row == nil || row.ActiveStreams <= 0 {
		return false, nil
	}
	row.ActiveStreams--
	return true, nil
}

// ListByFilter 复刻真 SQL：WHERE capacity_streams <> -1 [+region=?][+protocol_mask & ?<>0][+state=?]
// ORDER BY health_score DESC, node_id ASC + pageArgs(Limit,100,Offset)。
//
// 四处细节都是为了「过滤条件进了 SQL」可被证伪：
//   - 恒真条件 capacity_streams <> -1 会把配额列为 -1 的行整体挡掉，替身不照抄就永远看不见；
//   - protocol 只在 ProtocolMask 认得该枚举时才进 WHERE（未知枚举真库不过滤）；
//   - state 只在 validNodeState 时才进 WHERE（logic 已先拒非法值，替身仍要照抄条件形状）；
//   - 排序是 health_score 降序 + node_id 升序，不是 node_id 升序：翻页会按分数把差节点挤到后页。
func (f *fakeIngestNode) ListByFilter(_ context.Context, flt model.IngestNodeFilter) ([]*model.IngestNode, error) {
	f.db.bump("IngestNode.ListByFilter")
	out := filterNodes(f.db.nodes, flt)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].HealthScore != out[j].HealthScore {
			return out[i].HealthScore > out[j].HealthScore
		}
		return out[i].NodeID < out[j].NodeID
	})
	return applyPage(out, flt.Limit, flt.Offset, 100), nil
}

// CountByFilter 复刻真 SQL：SELECT COUNT(*) WHERE <同一套条件>——不带 ORDER/LIMIT/OFFSET。
// 早先这里直接数 ListByFilter 的返回条数，于是 total 恒等于「本页条数」，
// 「翻到第 2 页 total 掉到 ps」这类真缺陷会被替身自己掩盖掉。
func (f *fakeIngestNode) CountByFilter(_ context.Context, flt model.IngestNodeFilter) (int64, error) {
	f.db.bump("IngestNode.CountByFilter")
	return int64(len(filterNodes(f.db.nodes, flt))), nil
}

func filterNodes(all map[string]*model.IngestNode, flt model.IngestNodeFilter) []*model.IngestNode {
	var out []*model.IngestNode
	for _, row := range all {
		if row.CapacityStreams == -1 {
			continue
		}
		if flt.Region != "" && row.Region != flt.Region {
			continue
		}
		if flt.Protocol > 0 {
			mask, ok := model.ProtocolMask(flt.Protocol)
			if !ok || row.ProtocolMask&mask == 0 {
				continue
			}
		}
		if validNodeStateFilter(flt.State) && row.State != flt.State {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	return out
}

// validNodeStateFilter 与 model 的私有 validNodeState 同集合（ ONLINE/DRAINING/OFFLINE ）。
func validNodeStateFilter(state int32) bool {
	switch state {
	case model.NodeStateOnline, model.NodeStateDraining, model.NodeStateOffline:
		return true
	default:
		return false
	}
}

// applyPage 复刻 model.pageArgs 的「LIMIT clampLimit(limit,max) OFFSET 负值归零」。
// 必须复刻而不是只切数组：logic 传 Limit=0 时真 SQL 取 max/5 而不是「不限量」。
func applyPage[T any](rows []T, limit, offset, max int32) []T {
	if offset < 0 {
		offset = 0
	}
	if int(offset) >= len(rows) {
		return nil
	}
	if offset > 0 {
		rows = rows[offset:]
	}
	if n := clampLimitLikeModel(limit, max); int(n) < len(rows) {
		rows = rows[:n]
	}
	return rows
}

// clampLimitLikeModel 复刻 model.clampLimit：非正取上限的五分之一，超上限夹到上限。
func clampLimitLikeModel(limit, max int32) int32 {
	if limit <= 0 {
		return max / 5
	}
	if limit > max {
		return max
	}
	return limit
}

// --- live_node_assignment ---

type fakeNodeAssignment struct {
	model.NodeAssignmentModel
	db *fakeDB
}

func (f *fakeNodeAssignment) Insert(_ context.Context, _ sqlx.Session, a *model.NodeAssignment) (int64, error) {
	f.db.bump("NodeAssignment.Insert")
	if a == nil || a.RequestID == "" {
		return 0, model.ErrIdempotencyKeyRequired
	}
	for _, row := range f.db.assigns {
		if row.RequestID == a.RequestID {
			return 0, dupErr("uniq_request_id", a.RequestID)
		}
	}
	cp := *a
	cp.AssignmentID = f.db.id()
	if cp.State == 0 {
		cp.State = model.AssignmentStateActive
	}
	cp.Ctime, cp.Mtime = f.db.nowOr(cp.Ctime), f.db.nowOr(cp.Ctime)
	f.db.assigns[cp.AssignmentID] = &cp
	return cp.AssignmentID, nil
}

func (f *fakeNodeAssignment) FindOne(_ context.Context, id int64) (*model.NodeAssignment, error) {
	return cloneAssignment(f.db.assigns[id]), nil
}

func (f *fakeNodeAssignment) FindByRequest(_ context.Context, requestID string) (*model.NodeAssignment, error) {
	f.db.bump("NodeAssignment.FindByRequest")
	if requestID == "" {
		return nil, model.ErrIdempotencyKeyRequired
	}
	for _, row := range f.db.assigns {
		if row.RequestID == requestID {
			return cloneAssignment(row), nil
		}
	}
	return nil, nil
}

// FindActiveByStream 真 SQL：state=ACTIVE 按 key_id DESC LIMIT 1。
func (f *fakeNodeAssignment) FindActiveByStream(_ context.Context, _ sqlx.Session, streamID string) (*model.NodeAssignment, error) {
	f.db.bump("NodeAssignment.FindActiveByStream")
	var best *model.NodeAssignment
	for _, row := range f.db.assigns {
		if row.StreamID != streamID || row.State != model.AssignmentStateActive {
			continue
		}
		if best == nil || row.AssignmentID > best.AssignmentID {
			best = row
		}
	}
	return cloneAssignment(best), nil
}

// Release 真 SQL 是 state=ACTIVE + node_id 双条件 CAS，且目标态只能是 RELEASED/MIGRATED。
func (f *fakeNodeAssignment) Release(_ context.Context, _ sqlx.Session, assignmentID int64,
	toState int32, at int64, reason, nodeID string) (bool, error) {
	f.db.bump("NodeAssignment.Release")
	if toState != model.AssignmentStateReleased && toState != model.AssignmentStateMigrated {
		return false, fmt.Errorf("live_node_assignment Release: 非法目标状态 %d", toState)
	}
	row := f.db.assigns[assignmentID]
	if row == nil || row.State != model.AssignmentStateActive {
		return false, nil
	}
	if nodeID != "" && row.NodeID != nodeID {
		return false, nil
	}
	row.State = toState
	row.ReleasedAt = at
	row.ReleaseReason = reason
	return true, nil
}

func (f *fakeNodeAssignment) CountActiveByNode(_ context.Context, nodeID string) (int64, error) {
	var n int64
	for _, row := range f.db.assigns {
		if row.NodeID == nodeID && row.State == model.AssignmentStateActive {
			n++
		}
	}
	return n, nil
}

// ListByFilter 复刻真 SQL：WHERE state <> 0 [+stream_id=?][+node_id=?][+room_id=?][+state=?]
// ORDER BY assignment_id DESC + pageArgs(Limit,100,Offset)。
// state 只在 validAssignmentState 时才进 WHERE（与 model 的 build 同形状）。
func (f *fakeNodeAssignment) ListByFilter(_ context.Context, flt model.NodeAssignmentFilter) ([]*model.NodeAssignment, error) {
	f.db.bump("NodeAssignment.ListByFilter")
	out := filterAssignments(f.db.assigns, flt)
	sort.Slice(out, func(i, j int) bool { return out[i].AssignmentID > out[j].AssignmentID })
	return applyPage(out, flt.Limit, flt.Offset, 100), nil
}

// CountByFilter 复刻真 SQL：SELECT COUNT(*) WHERE <同一套条件>——不带 ORDER/LIMIT/OFFSET。
// 这里原先直接数 ListByFilter 的条数，于是「第 2 页 total 掉到 ps」会被替身掩盖。
func (f *fakeNodeAssignment) CountByFilter(_ context.Context, flt model.NodeAssignmentFilter) (int64, error) {
	f.db.bump("NodeAssignment.CountByFilter")
	return int64(len(filterAssignments(f.db.assigns, flt))), nil
}

func filterAssignments(all map[int64]*model.NodeAssignment, flt model.NodeAssignmentFilter) []*model.NodeAssignment {
	var out []*model.NodeAssignment
	for _, row := range all {
		if row.State == 0 {
			continue
		}
		if flt.StreamID != "" && row.StreamID != flt.StreamID {
			continue
		}
		if flt.NodeID != "" && row.NodeID != flt.NodeID {
			continue
		}
		if flt.RoomID > 0 && row.RoomID != flt.RoomID {
			continue
		}
		if validAssignmentStateFilter(flt.State) && row.State != flt.State {
			continue
		}
		out = append(out, cloneAssignment(row))
	}
	return out
}

// validAssignmentStateFilter 与 model 的私有 validAssignmentState 同集合。
func validAssignmentStateFilter(state int32) bool {
	switch state {
	case model.AssignmentStateActive, model.AssignmentStateReleased, model.AssignmentStateMigrated:
		return true
	default:
		return false
	}
}

func cloneAssignment(p *model.NodeAssignment) *model.NodeAssignment {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// --- live_stream_event ---

type fakeStreamEvent struct {
	model.StreamEventModel
	db *fakeDB
}

// Insert 复刻三个唯一索引：uniq_event_id / uniq_stream_seq / uniq_report_id（空串走 NULL 不冲突）。
func (f *fakeStreamEvent) Insert(_ context.Context, _ sqlx.Session, e *model.StreamEvent) (int64, error) {
	f.db.bump("StreamEvent.Insert")
	if e == nil || e.EventID == "" {
		return 0, model.ErrInvalidStreamId
	}
	for _, row := range f.db.events {
		if row.EventID == e.EventID {
			return 0, dupErr("uniq_event_id", e.EventID)
		}
		if e.ReportID != "" && row.ReportID == e.ReportID {
			return 0, dupErr("uniq_report_id", e.ReportID)
		}
		if e.Seq > 0 && row.StreamID == e.StreamID && row.Seq == e.Seq {
			return 0, dupErr("uniq_stream_seq", fmt.Sprintf("%s#%d", e.StreamID, e.Seq))
		}
	}
	cp := *e
	cp.ID = f.db.id()
	cp.Ctime = f.db.nowOr(cp.Ctime)
	if cp.OccurredAt == 0 {
		cp.OccurredAt = cp.Ctime
	}
	f.db.events[cp.ID] = &cp
	return cp.ID, nil
}

func (f *fakeStreamEvent) FindByReportID(_ context.Context, reportID string) (*model.StreamEvent, error) {
	f.db.bump("StreamEvent.FindByReportID")
	if reportID == "" {
		return nil, model.ErrIdempotencyKeyRequired
	}
	for _, row := range f.db.events {
		if row.ReportID == reportID {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeStreamEvent) FindByStreamSeq(_ context.Context, streamID string, seq int64) (*model.StreamEvent, error) {
	f.db.bump("StreamEvent.FindByStreamSeq")
	if seq <= 0 {
		return nil, nil
	}
	for _, row := range f.db.events {
		if row.StreamID == streamID && row.Seq == seq {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeStreamEvent) MaxSeq(_ context.Context, streamID string) (int64, error) {
	var max int64
	for _, row := range f.db.events {
		if row.StreamID == streamID && row.Seq > max {
			max = row.Seq
		}
	}
	return max, nil
}

func (f *fakeStreamEvent) ListAfterSeq(_ context.Context, streamID string, afterSeq int64, limit int32, desc bool) ([]*model.StreamEvent, error) {
	f.db.bump("StreamEvent.ListAfterSeq")
	var out []*model.StreamEvent
	for _, row := range f.db.events {
		if row.StreamID != streamID || row.Seq <= afterSeq {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if desc {
			return out[i].Seq > out[j].Seq
		}
		return out[i].Seq < out[j].Seq
	})
	if limit > 0 && int(limit) < len(out) {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStreamEvent) ListByEventIDs(_ context.Context, ids []string) ([]*model.StreamEvent, error) {
	f.db.bump("StreamEvent.ListByEventIDs")
	var out []*model.StreamEvent
	for _, id := range ids {
		for _, row := range f.db.events {
			if row.EventID == id {
				cp := *row
				out = append(out, &cp)
				break
			}
		}
	}
	return out, nil
}

// --- live_ingest_outbox ---

type fakeOutbox struct {
	model.EventOutboxModel
	db *fakeDB
}

func (f *fakeOutbox) Insert(_ context.Context, _ sqlx.Session, o *model.EventOutbox) error {
	f.db.bump("Outbox.Insert")
	if o == nil || o.EventID == "" {
		return model.ErrInvalidStreamId
	}
	for _, row := range f.db.outbox {
		if row.EventID == o.EventID {
			return dupErr("uniq_event_id", o.EventID)
		}
	}
	cp := *o
	cp.ID = f.db.id()
	if cp.State == 0 {
		cp.State = model.OutboxStatePending
	}
	cp.Ctime = f.db.nowOr(cp.Ctime)
	cp.Mtime = cp.Ctime
	cp.OccurredAt = f.db.nowOr(cp.OccurredAt)
	f.db.outbox[cp.ID] = &cp
	return nil
}

func (f *fakeOutbox) ResetFailed(_ context.Context, _ sqlx.Session, eventIDs []string, limit int32, reason string) (int64, error) {
	f.db.bump("Outbox.ResetFailed")
	wanted := map[string]bool{}
	for _, id := range eventIDs {
		wanted[id] = true
	}
	ids := make([]int64, 0, len(f.db.outbox))
	for id, row := range f.db.outbox {
		if row.State != model.OutboxStateFailed {
			continue
		}
		if len(wanted) > 0 && !wanted[row.EventID] {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(wanted) == 0 && limit > 0 && int(limit) < len(ids) {
		ids = ids[:limit]
	}
	for _, id := range ids {
		row := f.db.outbox[id]
		row.State = model.OutboxStatePending
		row.RetryCount = 0
		row.NextRetryAt = 0
		row.LastError = reason
	}
	return int64(len(ids)), nil
}

// CountByState 复刻真 SQL：SELECT COUNT(*) WHERE state = ?。
// 计数探针是「remaining_failed 在同一事务里读」这条契约的唯一证伪手段。
func (f *fakeOutbox) CountByState(_ context.Context, state int32) (int64, error) {
	f.db.bump("Outbox.CountByState")
	var n int64
	for _, row := range f.db.outbox {
		if row.State == state {
			n++
		}
	}
	return n, nil
}

// Checkpoint 复刻真 SQL 的六个聚合表达式：
// MIN(id) 与 MIN(occurred_at) 是**各自独立**地在全套 PENDING 行上取的，
// 不是「取最小 id 那一行的 occurred_at」——两条早到晚写的行（或晚到先写的行）
// 会让 oldest_pending_id 与 oldest_pending_at 指向不同的行，
// 用「滞后 = 最老事件年龄」做告警的调用方就依赖这个独立取值的口径。
func (f *fakeOutbox) Checkpoint(_ context.Context) (*model.OutboxCheckpoint, error) {
	f.db.bump("Outbox.Checkpoint")
	cp := &model.OutboxCheckpoint{}
	var sawPending bool
	for _, row := range f.db.outbox {
		switch row.State {
		case model.OutboxStatePublished:
			if row.ID > cp.LastPublishedID {
				cp.LastPublishedID = row.ID
			}
			if row.Mtime > cp.LastPublishedAt {
				cp.LastPublishedAt = row.Mtime
			}
		case model.OutboxStatePending:
			cp.PendingCount++
			if cp.OldestPendingID == 0 || row.ID < cp.OldestPendingID {
				cp.OldestPendingID = row.ID
			}
			// MIN(occurred_at) 逐字照抄：0 也是一个合法取值（它会把最小值压到 0），
			// 「0 就当没看过」那种哨兵写法会让 oldest_pending_at 偏大、lag_seconds 偏小，
			// 恰好把「发布器卡在最老一条上」这类告警吃掉。
			if !sawPending || row.OccurredAt < cp.OldestPendingAt {
				cp.OldestPendingAt, sawPending = row.OccurredAt, true
			}
		case model.OutboxStateFailed:
			cp.FailedCount++
		}
	}
	return cp, nil
}

func (f *fakeOutbox) ListByState(_ context.Context, state, limit int32) ([]*model.EventOutbox, error) {
	f.db.bump("Outbox.ListByState")
	// 复刻 model/outbox.go:255 的 WHERE state = ? ORDER BY id ASC LIMIT clampLimit(limit,500)。
	// 截断条件曾经写成「limit >= 已收集数就 break」，第一行必停，读侧的 limit 断言全部自证。
	max := clampLimitLikeModel(limit, 500)
	var out []*model.EventOutbox
	ids := make([]int64, 0, len(f.db.outbox))
	for id := range f.db.outbox {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		row := f.db.outbox[id]
		if row.State != state {
			continue
		}
		cp := *row
		out = append(out, &cp)
		if int32(len(out)) >= max {
			break
		}
	}
	return out, nil
}

// --- live_cdn_callback ---

type fakeCdnCallback struct {
	model.CdnCallbackModel
	db *fakeDB
}

func (f *fakeCdnCallback) Insert(_ context.Context, cb *model.CdnCallback) (int64, error) {
	f.db.bump("CdnCallback.Insert")
	if cb == nil || cb.Nonce == "" {
		return 0, model.ErrIdempotencyKeyRequired
	}
	for _, row := range f.db.callbacks {
		if row.Nonce == cb.Nonce {
			return 0, dupErr("uniq_nonce", cb.Nonce)
		}
	}
	cp := *cb
	cp.ID = f.db.id()
	cp.Ctime = f.db.nowOr(cp.Ctime)
	f.db.callbacks[cp.ID] = &cp
	return cp.ID, nil
}

func (f *fakeCdnCallback) FindByNonce(_ context.Context, nonce string) (*model.CdnCallback, error) {
	f.db.bump("CdnCallback.FindByNonce")
	for _, row := range f.db.callbacks {
		if row.Nonce == nonce {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

// --- live_stream_interruption ---

type fakeInterruption struct {
	model.StreamInterruptionModel
	db *fakeDB
}

func (f *fakeInterruption) Insert(_ context.Context, _ sqlx.Session, in *model.StreamInterruption) (int64, error) {
	f.db.bump("StreamInterruption.Insert")
	if in == nil || in.StreamID == "" {
		return 0, model.ErrInvalidStreamId
	}
	cp := *in
	cp.InterruptionID = f.db.id()
	cp.Ctime, cp.Mtime = f.db.nowOr(cp.Ctime), f.db.nowOr(cp.Mtime)
	f.db.interrupt[cp.InterruptionID] = &cp
	return cp.InterruptionID, nil
}

func (f *fakeInterruption) FindOpenByStream(_ context.Context, streamID string) (*model.StreamInterruption, error) {
	f.db.bump("StreamInterruption.FindOpenByStream")
	var best *model.StreamInterruption
	for _, row := range f.db.interrupt {
		if row.StreamID != streamID || row.EndedAt != 0 {
			continue
		}
		if best == nil || row.InterruptionID > best.InterruptionID {
			best = row
		}
	}
	if best == nil {
		return nil, nil
	}
	cp := *best
	return &cp, nil
}

func (f *fakeInterruption) NextEpisodeNo(_ context.Context, _ sqlx.Session, streamID string) (int32, error) {
	var max int32
	for _, row := range f.db.interrupt {
		if row.StreamID == streamID && row.EpisodeNo > max {
			max = row.EpisodeNo
		}
	}
	return max + 1, nil
}

// Close 真 SQL 条件是 ended_at = 0：重复关闭拿不到行，调用方按并发失败回滚。
//
// reason 的写法逐字照抄 CONCAT(reason, IF(reason = ”, ”, ' | '), ?)：
//   - 区间「为什么开」与「为什么关」两条文本是**追加**而不是覆盖，
//     直接赋值的替身会让「运营看到的中断原因只剩后半句」这类缺陷看不见；
//   - 分隔符只看**旧值**是否为空，与新值是否为空无关：旧值非空而新值为空时，
//     真库会留下一个尾部 " | "。看着别扭，但替身自作聪明地省掉它就成了「替身比实现更干净」，
//     读侧断言会因此永远测不到（见 interruptionlist_test 的对应用例）。
func (f *fakeInterruption) Close(_ context.Context, _ sqlx.Session, id, endedAt, durationSeconds, endReason int64,
	endEventID, reason string) (bool, error) {
	f.db.bump("StreamInterruption.Close")
	row := f.db.interrupt[id]
	if row == nil || row.EndedAt != 0 {
		return false, nil
	}
	row.EndedAt = endedAt
	row.DurationSeconds = durationSeconds
	row.EndReason = int32(endReason)
	row.EndEventID = endEventID
	sep := ""
	if row.Reason != "" {
		sep = " | "
	}
	row.Reason += sep + appendTextLikeModel(reason, 128)
	return true, nil
}

// appendTextLikeModel 复刻 model.truncate(reason,128)：按字节上限截断且不切半个 rune。
// 真库里 reason 列宽 255，而 CONCAT 会把开/关两段拼起来，因此截断只作用在**新增的那一段**。
func appendTextLikeModel(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}

// ListByFilter 复刻真 SQL：WHERE episode_no <> 0 [+stream_id=?][+room_id=?][+ended_at = 0]
// [+started_at >= ?][+started_at <= ?] ORDER BY started_at ASC LIMIT clampLimit(MaxResults,500)。
//
// 早先这里只看 StreamID/RoomID/OnlyOpen，且不排序不限量：
// 「时间窗没进 WHERE」「limit 没生效」「按 started_at 还是按主键排」三种真缺陷都会被替身吃掉。
// started_at 相同的多行在真 MySQL 里次序未定义，所以兜底按 interruption_id 只为确定性，
// 用例必须给候选行铺不同的 started_at，不能靠这里的兜底次序立断言。
func (f *fakeInterruption) ListByFilter(_ context.Context, flt model.InterruptionFilter) ([]*model.StreamInterruption, error) {
	f.db.bump("StreamInterruption.ListByFilter")
	out := filterInterruptions(f.db.interrupt, flt)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartedAt != out[j].StartedAt {
			return out[i].StartedAt < out[j].StartedAt
		}
		return out[i].InterruptionID < out[j].InterruptionID
	})
	max := clampLimitLikeModel(flt.MaxResults, 500)
	if int(max) < len(out) {
		out = out[:max]
	}
	return out, nil
}

// CountByFilter 复刻真 SQL：COUNT(*) over (SELECT ... WHERE <同一套条件>
// ORDER BY interruption_id ASC LIMIT hardLimit)，且 cnt >= hardLimit 时回 -1
// （达到上限不谎报精确值）。MaxResults 在这里不参与——两处的口径本就不同。
func (f *fakeInterruption) CountByFilter(_ context.Context, flt model.InterruptionFilter, hardLimit int64) (int64, error) {
	f.db.bump("StreamInterruption.CountByFilter")
	if hardLimit <= 0 {
		hardLimit = 10000
	}
	// 子查询的 LIMIT hardLimit 先截断，再计数：所以命中数达到上限时 cnt 恰为 hardLimit。
	rows := filterInterruptions(f.db.interrupt, flt)
	cnt := int64(len(rows))
	if cnt > hardLimit {
		cnt = hardLimit
	}
	if cnt >= hardLimit {
		return -1, nil
	}
	return cnt, nil
}

func filterInterruptions(all map[int64]*model.StreamInterruption, flt model.InterruptionFilter) []*model.StreamInterruption {
	var out []*model.StreamInterruption
	for _, row := range all {
		if row.EpisodeNo == 0 {
			continue
		}
		if flt.StreamID != "" && row.StreamID != flt.StreamID {
			continue
		}
		if flt.RoomID > 0 && row.RoomID != flt.RoomID {
			continue
		}
		if flt.OnlyOpen && row.EndedAt != 0 {
			continue
		}
		if flt.StartTime > 0 && row.StartedAt < flt.StartTime {
			continue
		}
		if flt.EndTime > 0 && row.StartedAt > flt.EndTime {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	return out
}

// --- live_stream_health_report ---

type fakeHealthReport struct {
	model.StreamHealthReportModel
	db *fakeDB
}

func (f *fakeHealthReport) Insert(_ context.Context, _ sqlx.Session, r *model.StreamHealthReport) (int64, error) {
	f.db.bump("StreamHealthReport.Insert")
	if r == nil || r.StreamID == "" {
		return 0, model.ErrInvalidStreamId
	}
	if f.db.forceReportDuplicate {
		// 等价于「预检读不到，但插入时另一事务的同一 report_id 先提交了」。
		return 0, dupErr("uniq_report_id", r.ReportID)
	}
	if r.ReportID != "" {
		for _, row := range f.db.reports {
			if row.ReportID == r.ReportID {
				return 0, dupErr("uniq_report_id", r.ReportID)
			}
		}
	}
	cp := *r
	cp.ID = f.db.id()
	cp.Ctime = f.db.nowOr(cp.Ctime)
	if cp.OccurredAt == 0 {
		cp.OccurredAt = cp.Ctime
	}
	f.db.reports[cp.ID] = &cp
	return cp.ID, nil
}

func (f *fakeHealthReport) FindByReportID(_ context.Context, reportID string) (*model.StreamHealthReport, error) {
	f.db.bump("StreamHealthReport.FindByReportID")
	if reportID == "" {
		return nil, model.ErrIdempotencyKeyRequired
	}
	for _, row := range f.db.reports {
		if row.ReportID == reportID {
			cp := *row
			return &cp, nil
		}
	}
	return nil, nil
}

// ListRecent 复刻真 SQL：WHERE stream_id=? [+occurred_at >= ?]
// ORDER BY occurred_at DESC, id DESC LIMIT clampLimit(limit,300) 后整体反转。
// 「先按 DESC 截断再反转」是关键：直接取前 limit 条会把最近的点丢掉、留下最老的，
// 连续 CRITICAL 判定就会用一批过期采样算出「还在危险」。
func (f *fakeHealthReport) ListRecent(_ context.Context, streamID string, since int64, limit int32) ([]*model.StreamHealthReport, error) {
	f.db.bump("StreamHealthReport.ListRecent")
	var out []*model.StreamHealthReport
	for _, row := range f.db.reports {
		if row.StreamID != streamID || (since > 0 && row.OccurredAt < since) {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OccurredAt != out[j].OccurredAt {
			return out[i].OccurredAt < out[j].OccurredAt
		}
		return out[i].ID < out[j].ID
	})
	if max := clampLimitLikeModel(limit, 300); int(max) < len(out) {
		out = out[len(out)-int(max):]
	}
	return out, nil
}

func (f *fakeHealthReport) Aggregate(_ context.Context, streamID string, since int64) (*model.HealthAggregate, error) {
	agg := &model.HealthAggregate{}
	var sum int64
	// hasMin 而不是「min == 0 就当没看过」：真 SQL 是 MIN(video_bitrate_bps)，
	// 码率 0 的采样点（节点侧统计不出来的那类）必须能把最低值压到 0。
	// 用 0 当哨兵会让替身把 0 之后第一条采样当成最小值，「最低码率被抬高」这类缺陷就被掩盖了。
	hasMin := false
	for _, row := range f.db.reports {
		if row.StreamID != streamID || row.OccurredAt < since {
			continue
		}
		agg.SampleCount++
		sum += row.VideoBitrateBps
		if !hasMin || row.VideoBitrateBps < agg.MinVideoBitrate {
			agg.MinVideoBitrate, hasMin = row.VideoBitrateBps, true
		}
		if row.PacketLossPpm > agg.MaxPacketLossPpm {
			agg.MaxPacketLossPpm = row.PacketLossPpm
		}
		if row.OccurredAt > agg.LatestReportedAt {
			agg.LatestReportedAt = row.OccurredAt
		}
	}
	if agg.SampleCount > 0 {
		agg.AvgVideoBitrate = sum / int64(agg.SampleCount)
	}
	return agg, nil
}
