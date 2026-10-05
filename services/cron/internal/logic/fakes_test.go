package logic

// 本文件提供 logic 单测用的「内存版 model」与「假事务」。
//
// 为什么可以这样注入（以及为什么这不是伪装）：
//   - svc.ServiceContext 的五个 model 字段是接口类型，测试可以直接赋值；
//   - 真正的并发正确性由 MySQL 的 uniq_* 与 SELECT ... FOR UPDATE 保证，单元测试无法也不该
//     复刻锁；这里复刻的是**语义**：唯一键命中即 created=false、CAS 条件不命中即返回 false、
//     fence_token 只在抢占时 +1、终态行不可回改。断言的是 logic 面对这些返回值的裁决，
//     也就是本轮唯一可在无库条件下证明的部分；
//   - 每个假实现只嵌入接口并覆写被测路径用到的方法，其余方法由内嵌的 nil 接口提升而来，
//     一旦测试走到未实现的分支会立刻 panic —— 失败是响的，不会被写成「通过」。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"go-video/services/cron/internal/config"
	"go-video/services/cron/internal/registry"
	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// fakeNow 与 svcCtx.ServerTime() 同源（真实时钟）。
// 用例一律用「相对现在的偏移」构造时间，避免依赖挂钟具体读数；边界处留够秒数余量。
func fakeNow() int64 { return time.Now().Unix() }

// fakeDB 是一次测试的全部内存态。TransactCtx 在入口快照它、回调报错时整体回滚，
// 这样才能真正断言「游标 CAS 冲突不得留下半条结果」。
type fakeDB struct {
	runs        map[int64]*model.TaskRun
	leases      map[string]*model.TaskLease
	defs        map[string]*model.TaskDefinition
	checkpoints map[string]*model.TaskCheckpoint
	audits      []*model.TaskAudit
	auditErr    error

	// casMissOnUpdate 模拟「UPDATE ... WHERE version=? 未命中行」——即并发里有别的
	// 写入者先提交了新版本。真实数据库必然会出现这种返回，logic 必须有裁决。
	casMissOnUpdate bool
	// unreadable 注入「唯一键说这行存在、按 key 却读不回来」的库不一致，
	// 用于驱动 RegisterTask 的显式失败分支（绝不回零值冒充成功）。
	unreadable map[string]bool
	// listCalls 记录 ListByCursor 收到的实参序列（轨迹只统计被测调用，
	// 布数据一律走 seedDefinition 直写 map，不经过这里）。
	listCalls []listCall
	// stateCalls 记录 SetStateTx 收到的实参序列，用于证明「一次迁移之后，
	// 下一次迁移判定读的是库里最新行」（乐观锁版本号必须跟着涨）。
	// 与 listCalls 同一取舍：回滚不会撤销已记录的调用，所以轨迹只用于提交成功的链路。
	stateCalls []stateCall

	// maxAttemptStaleBy 注入「事务外那次 MAX(attempt) 读到旧快照」：TriggerTask 的取号
	// 发生在 Transact 之前，真库里两个实例会取到同一个号，只有一个能命中 uniq_fire_attempt。
	// 这是无并发条件下唯一能复现该竞态的开关，只作用于非事务的 MaxAttempt
	// （RetryRun 用 MaxAttemptTx 在事务内取号，与真 SQL 一致，不受该开关影响）。
	maxAttemptStaleBy int32
	// countRunningErr 注入「RUNNING 计数读失败」，驱动 ListDueTasks 的整体失败分支
	// （那里宁可报错也不回 running_count=0 的假清单）。
	countRunningErr error

	// 以下轨迹与 listCalls 同一语义：只统计 model 方法实参，布数据不走这些方法。
	countRunningCalls []string
	runCalls          []runListCall
	leaseCalls        []leaseListCall
	cpCalls           []cpListCall
	dueCalls          []dueCall
	auditCalls        []auditListCall
	// readCalls 是「单行读 + 取号」的调用轨迹（按发生顺序），用于证明
	// 「入参非法的请求必须在第一次读库之前就被拒掉」。
	// 与上面几个轨迹同一取舍：只记 model 方法，seed* 直写 map 不经过这里。
	readCalls []string

	nextRun  int64
	nextLeas int64
	nextCp   int64
	nextAud  int64
	nextDef  int64

	txRuns    int
	txRollups int
}

// runListCall 一次 cron_task_run 游标分页的实参快照。
type runListCall struct {
	taskKey                string
	state                  int32
	plannedFrom, plannedTo int64
	cursorID               int64
	limit                  int
}

// leaseListCall 一次 cron_task_lease 游标分页的实参快照。
type leaseListCall struct {
	taskKey     string
	onlyExpired bool
	now         int64
	cursorID    int64
	limit       int
}

// cpListCall 一次 cron_task_checkpoint 游标分页的实参快照。
type cpListCall struct {
	taskKey   string
	cursorKey string
	limit     int
}

// dueCall 一次到期扫描（ListDue）的实参快照。
type dueCall struct {
	now, lookahead int64
	group          string
	limit          int
}

// auditListCall 一次 cron_task_audit 游标分页的实参快照。
type auditListCall struct {
	taskKey            string
	action             string
	ctimeFrom, ctimeTo int64
	cursorID           int64
	limit              int
}

// listCall 一次 cron_task_definition 游标分页的实参快照。
type listCall struct {
	state    int32
	group    string
	handler  string
	cursorID int64
	limit    int
}

// stateCall 一次 SetStateTx 的实参快照（from/to 与乐观锁版本）。
type stateCall struct {
	from, to        int32
	expectedVersion int64
	operator        string
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		runs:        map[int64]*model.TaskRun{},
		leases:      map[string]*model.TaskLease{},
		defs:        map[string]*model.TaskDefinition{},
		checkpoints: map[string]*model.TaskCheckpoint{},
		unreadable:  map[string]bool{},
	}
}

func checkpointKey(taskKey, scopeKey string) string { return taskKey + "\x1f" + scopeKey }

func (db *fakeDB) snapshot() *fakeDB {
	cp := &fakeDB{
		runs:        map[int64]*model.TaskRun{},
		leases:      map[string]*model.TaskLease{},
		defs:        map[string]*model.TaskDefinition{},
		checkpoints: map[string]*model.TaskCheckpoint{},
		txRuns:      db.txRuns,
		txRollups:   db.txRollups,
		nextRun:     db.nextRun,
		nextLeas:    db.nextLeas,
		nextCp:      db.nextCp,
		nextAud:     db.nextAud,
		nextDef:     db.nextDef,
		auditErr:    db.auditErr,
	}
	for k, v := range db.runs {
		c := *v
		cp.runs[k] = &c
	}
	for k, v := range db.leases {
		c := *v
		cp.leases[k] = &c
	}
	for k, v := range db.defs {
		c := *v
		cp.defs[k] = &c
	}
	for k, v := range db.checkpoints {
		c := *v
		cp.checkpoints[k] = &c
	}
	cp.audits = append([]*model.TaskAudit(nil), db.audits...)
	return cp
}

func (db *fakeDB) restore(s *fakeDB) {
	db.runs = s.runs
	db.leases = s.leases
	db.defs = s.defs
	db.checkpoints = s.checkpoints
	db.audits = s.audits
	db.nextRun, db.nextLeas, db.nextCp, db.nextAud = s.nextRun, s.nextLeas, s.nextCp, s.nextAud
	db.nextDef = s.nextDef
	db.txRollups++
}

// fakeConn 只实现真正被测的 TransactCtx（把 model 的 *Tx 方法串起来），其余 sqlx 方法
// 由内嵌的 nil 接口提升：任何「想用真连接」的写法都会当场 panic。
type fakeConn struct {
	sqlx.SqlConn
	db *fakeDB
}

func (c fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	c.db.txRuns++
	snap := c.db.snapshot()
	if err := fn(ctx, nil); err != nil {
		c.db.restore(snap)
		return err
	}
	return nil
}

// --- cron_task_run ---

type fakeRuns struct {
	model.TaskRunModel
	db *fakeDB
}

func (f fakeRuns) FindOne(_ context.Context, runID int64) (*model.TaskRun, error) {
	f.db.readCalls = append(f.db.readCalls, fmt.Sprintf("run:%d", runID))
	return f.find(runID), nil
}

func (f fakeRuns) FindOneTx(_ context.Context, _ sqlx.Session, runID int64) (*model.TaskRun, error) {
	return f.find(runID), nil
}

func (f fakeRuns) find(runID int64) *model.TaskRun {
	row, ok := f.db.runs[runID]
	if !ok {
		return nil
	}
	c := *row
	return &c
}

func (f fakeRuns) FindByFireTx(_ context.Context, _ sqlx.Session, taskKey string,
	plannedAt int64, attempt int32) (*model.TaskRun, error) {
	for _, r := range f.db.runs {
		if r.TaskKey == taskKey && r.PlannedAt == plannedAt && r.Attempt == attempt {
			c := *r
			return &c, nil
		}
	}
	return nil, nil
}

// ClaimForFire 复刻 uniq_fire_attempt(task_key, planned_at, attempt) 的语义。
func (f fakeRuns) ClaimForFire(_ context.Context, _ sqlx.Session, in *model.TaskRun) (*model.TaskRun, bool, error) {
	for _, r := range f.db.runs {
		if r.TaskKey == in.TaskKey && r.PlannedAt == in.PlannedAt && r.Attempt == in.Attempt {
			c := *r
			return &c, false, nil
		}
	}
	f.db.nextRun++
	now := fakeNow()
	stored := *in
	stored.ID = f.db.nextRun
	stored.Ctime, stored.Mtime = now, now
	f.db.runs[stored.ID] = &stored
	c := stored
	return &c, true, nil
}

func (f fakeRuns) StartTx(_ context.Context, _ sqlx.Session, runID int64, owner string,
	fence, ttl int64) (bool, error) {
	r, ok := f.db.runs[runID]
	if !ok || r.State != model.RunStatePending {
		return false, nil
	}
	now := fakeNow()
	r.State = model.RunStateRunning
	r.LeaseOwner, r.FenceToken = owner, fence
	r.LeaseExpireAt, r.StartedAt, r.Mtime = now+ttl, now, now
	return true, nil
}

func (f fakeRuns) TakeoverTx(_ context.Context, _ sqlx.Session, runID int64, owner string,
	oldFence, newFence, ttl int64, reason string) (bool, error) {
	r, ok := f.db.runs[runID]
	if !ok {
		return false, nil
	}
	now := fakeNow()
	// CAS 条件与真 SQL 一致：仍是 RUNNING、栅栏仍是旧值、租约确已过期。
	if r.State != model.RunStateRunning || r.FenceToken != oldFence || !model.LeaseExpired(r.LeaseExpireAt, now) {
		return false, nil
	}
	r.LeaseOwner, r.FenceToken = owner, newFence
	r.LeaseExpireAt, r.Mtime = now+ttl, now
	r.LastError = reason
	return true, nil
}

func (f fakeRuns) RenewTx(_ context.Context, _ sqlx.Session, runID int64, owner string,
	fence, ttl int64) (bool, error) {
	r, ok := f.db.runs[runID]
	if !ok || r.State != model.RunStateRunning || r.LeaseOwner != owner || r.FenceToken != fence {
		return false, nil
	}
	now := fakeNow()
	r.LeaseExpireAt, r.Mtime = now+ttl, now
	return true, nil
}

func (f fakeRuns) ReportTx(_ context.Context, _ sqlx.Session, runID int64, owner string, fence int64,
	fromState, toState int32, resultSummary, lastError string, nextRetryAt, durationMs int64) (bool, error) {
	r, ok := f.db.runs[runID]
	if !ok {
		return false, model.ErrRunNotFound
	}
	if r.State != fromState || r.FenceToken != fence {
		return false, nil
	}
	if owner != "" && r.LeaseOwner != owner {
		return false, nil
	}
	if !model.CanTransition(fromState, toState) {
		return false, fmt.Errorf("%w: %s -> %s", model.ErrStateTransition,
			model.RunStateName(fromState), model.RunStateName(toState))
	}
	now := fakeNow()
	r.State = toState
	r.NextRetryAt = nextRetryAt
	r.ResultSummary = resultSummary
	r.LastError = lastError
	r.DurationMs = durationMs
	r.Mtime = now
	if model.IsTerminalRunState(toState) {
		r.FinishedAt = now
		// 终态必须把租约交回（expire_at=0），否则别的实例会白等到 TTL 结束。
		r.LeaseExpireAt = 0
	}
	return true, nil
}

func (f fakeRuns) CountRunningTx(_ context.Context, _ sqlx.Session, taskKey string) (int64, error) {
	var n int64
	for _, r := range f.db.runs {
		if r.TaskKey == taskKey && r.State == model.RunStateRunning {
			n++
		}
	}
	return n, nil
}

// CountRunning 复刻非事务版 SQL 的完整条件：
// `state = RUNNING AND task_key = ? AND lease_expire_at > now()`。
// 与上一行的 CountRunningTx 的唯一差异就是过期判定（那条是上一轮为领取侧写的，
// 本方法的调用方 ListDueTasks 依赖「崩溃实例的过期 RUNNING 行不再占用并发额度」）。
func (f fakeRuns) CountRunning(_ context.Context, taskKey string) (int64, error) {
	f.db.countRunningCalls = append(f.db.countRunningCalls, taskKey)
	if f.db.countRunningErr != nil {
		return 0, f.db.countRunningErr
	}
	now := fakeNow()
	var n int64
	for _, r := range f.db.runs {
		if r.TaskKey == taskKey && r.State == model.RunStateRunning && r.LeaseExpireAt > now {
			n++
		}
	}
	return n, nil
}

// MaxAttempt / MaxAttemptTx 复刻 SELECT MAX(attempt) WHERE task_key=? AND planned_at=?。
// 无记录时真库返回 NULL，model 用 sql.NullInt64 兜成 0。
func (f fakeRuns) MaxAttempt(_ context.Context, taskKey string, plannedAt int64) (int32, error) {
	f.db.readCalls = append(f.db.readCalls, fmt.Sprintf("max_attempt:%s@%d", taskKey, plannedAt))
	highest := f.maxAttempt(taskKey, plannedAt)
	if stale := f.db.maxAttemptStaleBy; stale > 0 && highest >= stale {
		highest -= stale
	}
	return highest, nil
}

func (f fakeRuns) MaxAttemptTx(_ context.Context, _ sqlx.Session, taskKey string,
	plannedAt int64) (int32, error) {
	return f.maxAttempt(taskKey, plannedAt), nil
}

func (f fakeRuns) maxAttempt(taskKey string, plannedAt int64) int32 {
	var max int32
	for _, r := range f.db.runs {
		if r.TaskKey == taskKey && r.PlannedAt == plannedAt && r.Attempt > max {
			max = r.Attempt
		}
	}
	return max
}

// ListByCursor 复刻「按 (planned_at, id) 倒序 + 过滤 + LIMIT」，游标条件是 id < cursorID，
// 以及「满页才给下一页游标」的判定（model/taskrun.go 的 ListByCursor）。
func (f fakeRuns) ListByCursor(_ context.Context, taskKey string, state int32,
	plannedFrom, plannedTo, cursorID int64, limit int) ([]*model.TaskRun, int64, error) {
	if limit <= 0 {
		limit = model.DefaultPageSize
	}
	f.db.runCalls = append(f.db.runCalls, runListCall{
		taskKey: taskKey, state: state, plannedFrom: plannedFrom,
		plannedTo: plannedTo, cursorID: cursorID, limit: limit,
	})
	rows := f.filtered(taskKey, state, plannedFrom, plannedTo)
	if cursorID > 0 {
		kept := make([]*model.TaskRun, 0, len(rows))
		for _, r := range rows {
			if r.ID < cursorID {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	next := int64(0)
	if len(rows) == limit {
		next = rows[len(rows)-1].ID
	}
	return rows, next, nil
}

func (f fakeRuns) CountByFilter(_ context.Context, taskKey string, state int32,
	plannedFrom, plannedTo int64) (int64, error) {
	return int64(len(f.filtered(taskKey, state, plannedFrom, plannedTo))), nil
}

// filtered 按 (task_key, state, planned_at 区间) 过滤，返回 (planned_at, id) 倒序的副本集合。
func (f fakeRuns) filtered(taskKey string, state int32, plannedFrom, plannedTo int64) []*model.TaskRun {
	out := make([]*model.TaskRun, 0, len(f.db.runs))
	for _, r := range f.db.runs {
		if taskKey != "" && r.TaskKey != taskKey {
			continue
		}
		if state != model.RunStateUnspecified && r.State != state {
			continue
		}
		if plannedFrom > 0 && r.PlannedAt < plannedFrom {
			continue
		}
		if plannedTo > 0 && r.PlannedAt > plannedTo {
			continue
		}
		c := *r
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PlannedAt != out[j].PlannedAt {
			return out[i].PlannedAt > out[j].PlannedAt
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// CountByGroupStates 复刻「cron_task_run JOIN cron_task_definition ON task_key
// WHERE ctime >= since GROUP BY (task_group, state)」：定义已被删除的执行不再计入分组。
func (f fakeRuns) CountByGroupStates(_ context.Context, since int64) ([]model.GroupRunStateCount, error) {
	type key struct {
		group string
		state int32
	}
	counts := map[key]int64{}
	for _, r := range f.db.runs {
		if r.Ctime < since {
			continue
		}
		def, ok := f.db.defs[r.TaskKey]
		if !ok {
			continue
		}
		counts[key{def.TaskGroup, r.State}]++
	}
	out := make([]model.GroupRunStateCount, 0, len(counts))
	for k, v := range counts {
		out = append(out, model.GroupRunStateCount{TaskGroup: k.group, State: k.state, Total: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TaskGroup != out[j].TaskGroup {
			return out[i].TaskGroup < out[j].TaskGroup
		}
		return out[i].State < out[j].State
	})
	return out, nil
}

// --- cron_task_lease ---

type fakeLeases struct {
	model.TaskLeaseModel
	db *fakeDB
}

// AcquireTx 复刻「FOR UPDATE 行 → 无人持有/自己持有/已过期 才给锁，抢占即 fence+1」。
func (f fakeLeases) AcquireTx(_ context.Context, _ sqlx.Session, leaseKey, taskKey, scope, owner string,
	ttl, runID int64) (*model.LeaseAcquire, error) {
	if strings.TrimSpace(leaseKey) == "" {
		return nil, fmt.Errorf("%w: lease_key 不能为空", model.ErrLeaseNotHeld)
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("%w: ttl=%d", model.ErrInvalidLeaseTTL, ttl)
	}
	now := fakeNow()
	row, ok := f.db.leases[leaseKey]
	if !ok {
		f.db.nextLeas++
		row = &model.TaskLease{
			ID: f.db.nextLeas, LeaseKey: leaseKey, TaskKey: taskKey, Scope: scope,
			OwnerInstance: owner, FenceToken: 1, AcquiredAt: now, RenewedAt: now,
			ExpireAt: now + ttl, RunID: runID, Ctime: now, Mtime: now,
		}
		f.db.leases[leaseKey] = row
		return &model.LeaseAcquire{Acquired: true, Lease: row}, nil
	}
	snapshot := *row
	switch {
	case row.OwnerInstance == owner:
		row.RenewedAt, row.ExpireAt, row.RunID, row.Mtime = now, now+ttl, runID, now
		return &model.LeaseAcquire{Acquired: true, Lease: row}, nil
	case row.ExpireAt <= now || row.OwnerInstance == "":
		prev := row.OwnerInstance
		row.OwnerInstance, row.FenceToken = owner, row.FenceToken+1
		row.AcquiredAt, row.RenewedAt, row.ExpireAt = now, now, now+ttl
		row.RunID, row.Mtime = runID, now
		if prev != "" {
			row.TakeoverCount++
		}
		return &model.LeaseAcquire{Acquired: true, TakenOver: prev != "", Lease: row}, nil
	default:
		return &model.LeaseAcquire{Acquired: false, Lease: &snapshot}, nil
	}
}

func (f fakeLeases) RenewTx(_ context.Context, _ sqlx.Session, leaseKey, owner string,
	fence, ttl int64) (bool, error) {
	row, ok := f.db.leases[leaseKey]
	if !ok || row.OwnerInstance != owner || row.FenceToken != fence {
		return false, nil
	}
	now := fakeNow()
	row.RenewedAt, row.ExpireAt, row.Mtime = now, now+ttl, now
	return true, nil
}

func (f fakeLeases) ReleaseTx(_ context.Context, _ sqlx.Session, leaseKey, owner string, fence int64) (bool, error) {
	row, ok := f.db.leases[leaseKey]
	if !ok || row.OwnerInstance != owner || row.FenceToken != fence {
		return false, nil
	}
	now := fakeNow()
	// 释放只清持有者与过期时间，fence_token 绝不回退（否则被顶掉的旧实例又能写）。
	row.OwnerInstance, row.ExpireAt, row.RunID, row.Mtime = "", 0, 0, now
	return true, nil
}

func (f fakeLeases) FindOne(_ context.Context, leaseKey string) (*model.TaskLease, error) {
	f.db.readCalls = append(f.db.readCalls, "lease:"+leaseKey)
	return f.findLease(leaseKey), nil
}

func (f fakeLeases) FindOneTx(_ context.Context, _ sqlx.Session, leaseKey string) (*model.TaskLease, error) {
	return f.findLease(leaseKey), nil
}

func (f fakeLeases) findLease(leaseKey string) *model.TaskLease {
	row, ok := f.db.leases[leaseKey]
	if !ok {
		return nil
	}
	c := *row
	return &c
}

func (f fakeLeases) FindByRun(_ context.Context, runID int64) (*model.TaskLease, error) {
	for _, row := range f.db.leases {
		if row.RunID == runID && runID > 0 {
			c := *row
			return &c, nil
		}
	}
	return nil, nil
}

// ListByCursor 复刻「按 (expire_at, id) 升序 + 过滤 + LIMIT」：
// onlyExpired 的条件是 `expire_at > 0 AND expire_at <= now`（已释放的行 expire_at=0，
// 既不算持有也不算过期），游标条件是 id > cursorID。
func (f fakeLeases) ListByCursor(_ context.Context, taskKey string, onlyExpired bool,
	now, cursorID int64, limit int) ([]*model.TaskLease, int64, error) {
	if limit <= 0 {
		limit = model.DefaultPageSize
	}
	f.db.leaseCalls = append(f.db.leaseCalls, leaseListCall{
		taskKey: taskKey, onlyExpired: onlyExpired, now: now, cursorID: cursorID, limit: limit,
	})
	rows := f.filtered(taskKey, onlyExpired, now)
	if cursorID > 0 {
		kept := make([]*model.TaskLease, 0, len(rows))
		for _, r := range rows {
			if r.ID > cursorID {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	next := int64(0)
	if len(rows) == limit {
		next = rows[len(rows)-1].ID
	}
	return rows, next, nil
}

func (f fakeLeases) CountByFilter(_ context.Context, taskKey string, onlyExpired bool, now int64) (int64, error) {
	return int64(len(f.filtered(taskKey, onlyExpired, now))), nil
}

// CountExpiredByGroup 复刻「cron_task_lease JOIN cron_task_definition
// WHERE owner_instance <> ” AND expire_at > 0 AND expire_at <= now GROUP BY task_group」。
// 与 ListByCursor 的 onlyExpired 差一个 owner 条件：无人持有的行不算「失联实例遗留」。
func (f fakeLeases) CountExpiredByGroup(_ context.Context, now int64) ([]model.GroupCount, error) {
	counts := map[string]int64{}
	for _, l := range f.db.leases {
		if l.OwnerInstance == "" || l.ExpireAt <= 0 || l.ExpireAt > now {
			continue
		}
		def, ok := f.db.defs[l.TaskKey]
		if !ok {
			continue
		}
		counts[def.TaskGroup]++
	}
	out := make([]model.GroupCount, 0, len(counts))
	for g, v := range counts {
		out = append(out, model.GroupCount{TaskGroup: g, Total: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TaskGroup < out[j].TaskGroup })
	return out, nil
}

// filtered 按 (task_key, 过期判定) 过滤，返回 (expire_at, id) 升序的副本集合。
func (f fakeLeases) filtered(taskKey string, onlyExpired bool, now int64) []*model.TaskLease {
	out := make([]*model.TaskLease, 0, len(f.db.leases))
	for _, l := range f.db.leases {
		if taskKey != "" && l.TaskKey != taskKey {
			continue
		}
		if onlyExpired && !(l.ExpireAt > 0 && l.ExpireAt <= now) {
			continue
		}
		c := *l
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ExpireAt != out[j].ExpireAt {
			return out[i].ExpireAt < out[j].ExpireAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// --- cron_task_definition ---

type fakeDefs struct {
	model.TaskDefinitionModel
	db *fakeDB
}

func (f fakeDefs) FindOne(_ context.Context, taskKey string) (*model.TaskDefinition, error) {
	f.db.readCalls = append(f.db.readCalls, "definition:"+taskKey)
	return f.find(taskKey), nil
}

func (f fakeDefs) FindOneTx(_ context.Context, _ sqlx.Session, taskKey string) (*model.TaskDefinition, error) {
	return f.find(taskKey), nil
}

func (f fakeDefs) find(taskKey string) *model.TaskDefinition {
	if f.db.unreadable[taskKey] {
		return nil // 注入「唯一键说有、读回没有」：真库只会在主从/锁异常下出现，logic 必须显式失败
	}
	row, ok := f.db.defs[taskKey]
	if !ok {
		return nil
	}
	c := *row
	return &c
}

// InsertTx 复刻 uniq_task_key + ON DUPLICATE KEY UPDATE id=id：
// 命中唯一键时既有行一个字节都不改，只报告 existed=true。
// 与真 SQL 一致的两点细节：① Ctime/Mtime/Version 在 Exec 前就被写进入参 d；
// ② 自增主键不回填到 d（真 SQL 没读 LastInsertId），所以入库行的 ID 与入参 ID 不同。
func (f fakeDefs) InsertTx(_ context.Context, _ sqlx.Session, d *model.TaskDefinition) (bool, error) {
	if d.TaskKey == "" {
		return false, model.ErrTaskKeyEmpty
	}
	now := fakeNow()
	if d.Ctime == 0 {
		d.Ctime = now
	}
	d.Mtime = now
	if d.Version == 0 {
		d.Version = 1
	}
	if _, ok := f.db.defs[d.TaskKey]; ok {
		return true, nil
	}
	f.db.nextDef++
	stored := *d
	stored.ID = f.db.nextDef
	f.db.defs[d.TaskKey] = &stored
	return false, nil
}

// UpdateMutableTx 的 SET 列表与真 SQL 逐列一致：
// state / next_fire_at / last_fire_at / last_success_at / last_error / ctime / task_key
// 都不在其中，乐观锁未命中就是「一行都没改」。
func (f fakeDefs) UpdateMutableTx(_ context.Context, _ sqlx.Session, d *model.TaskDefinition,
	expectedVersion int64) (bool, error) {
	if d.TaskKey == "" {
		return false, model.ErrTaskKeyEmpty
	}
	if expectedVersion <= 0 {
		return false, model.ErrVersionConflict
	}
	row, ok := f.db.defs[d.TaskKey]
	if !ok || f.db.casMissOnUpdate || row.Version != expectedVersion {
		return false, nil
	}
	row.Name, row.Handler, row.TaskGroup = d.Name, d.Handler, d.TaskGroup
	row.ScheduleType, row.CronExpr, row.IntervalSeconds = d.ScheduleType, d.CronExpr, d.IntervalSeconds
	row.Timezone, row.TimeoutSeconds, row.MaxAttempts = d.Timezone, d.TimeoutSeconds, d.MaxAttempts
	row.RetryBaseSeconds, row.RetryMaxSeconds = d.RetryBaseSeconds, d.RetryMaxSeconds
	row.ConcurrencyLimit, row.LeaseTTLSeconds = d.ConcurrencyLimit, d.LeaseTTLSeconds
	row.MisfirePolicy, row.MisfireBackfillLim = d.MisfirePolicy, d.MisfireBackfillLim
	row.Params, row.SecretRefs, row.Owner, row.Operator = d.Params, d.SecretRefs, d.Owner, d.Operator
	row.Version, row.Mtime = row.Version+1, fakeNow()
	return true, nil
}

// ListByCursor 复刻「按 id 升序 + 过滤 + LIMIT」，以及「满页才给下一页游标」的判定。
func (f fakeDefs) ListByCursor(_ context.Context, state int32, group, handler string,
	cursorID int64, limit int) ([]*model.TaskDefinition, int64, error) {
	if limit <= 0 {
		limit = model.DefaultPageSize
	}
	f.db.listCalls = append(f.db.listCalls, listCall{
		state: state, group: group, handler: handler, cursorID: cursorID, limit: limit,
	})
	rows := f.filtered(state, group, handler)
	if cursorID > 0 {
		kept := make([]*model.TaskDefinition, 0, len(rows))
		for _, r := range rows {
			if r.ID > cursorID {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	next := int64(0)
	if len(rows) == limit && limit > 0 {
		next = rows[len(rows)-1].ID
	}
	return rows, next, nil
}

func (f fakeDefs) CountByFilter(_ context.Context, state int32, group, handler string) (int64, error) {
	return int64(len(f.filtered(state, group, handler))), nil
}

// ListDue 复刻到期扫描的唯一真值条件：
// `state = ENABLED AND next_fire_at > 0 AND next_fire_at <= now + lookahead`
// （可选 task_group 过滤）+ ORDER BY next_fire_at ASC LIMIT ?。
// 两个约束缺一不可：只看指针会把暂停任务扫出来，只看状态会把指针为 0 的
// 手动/暂停任务当成「一直到期」。
func (f fakeDefs) ListDue(_ context.Context, now, lookaheadSeconds int64, group string,
	limit int) ([]*model.TaskDefinition, error) {
	if limit <= 0 {
		limit = model.DefaultPageSize
	}
	if lookaheadSeconds < 0 {
		lookaheadSeconds = 0
	}
	f.db.dueCalls = append(f.db.dueCalls, dueCall{now: now, lookahead: lookaheadSeconds, group: group, limit: limit})
	deadline := now + lookaheadSeconds
	out := make([]*model.TaskDefinition, 0, len(f.db.defs))
	for _, row := range f.db.defs {
		if row.State != model.TaskStateEnabled || row.NextFireAt <= 0 || row.NextFireAt > deadline {
			continue
		}
		if group != "" && row.TaskGroup != group {
			continue
		}
		c := *row
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NextFireAt != out[j].NextFireAt {
			return out[i].NextFireAt < out[j].NextFireAt
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// CountByGroupState 复刻「GROUP BY task_group, state ORDER BY task_group, state」，
// 三种状态（含 DISABLED）都会出现在结果里，聚合口径由 logic 负责。
func (f fakeDefs) CountByGroupState(_ context.Context) ([]model.GroupStateCount, error) {
	type key struct {
		group string
		state int32
	}
	counts := map[key]int64{}
	for _, row := range f.db.defs {
		counts[key{row.TaskGroup, row.State}]++
	}
	out := make([]model.GroupStateCount, 0, len(counts))
	for k, v := range counts {
		out = append(out, model.GroupStateCount{TaskGroup: k.group, State: k.state, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TaskGroup != out[j].TaskGroup {
			return out[i].TaskGroup < out[j].TaskGroup
		}
		return out[i].State < out[j].State
	})
	return out, nil
}

// filtered 按 (state, task_group, handler) 过滤并返回 id 升序的副本集合。
func (f fakeDefs) filtered(state int32, group, handler string) []*model.TaskDefinition {
	out := make([]*model.TaskDefinition, 0, len(f.db.defs))
	for _, row := range f.db.defs {
		if state != model.TaskStateUnspecified && row.State != state {
			continue
		}
		if group != "" && row.TaskGroup != group {
			continue
		}
		if handler != "" && row.Handler != handler {
			continue
		}
		c := *row
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (f fakeDefs) MarkFiredTx(_ context.Context, _ sqlx.Session, taskKey string, plannedAt, nextFireAt int64) error {
	row, ok := f.db.defs[taskKey]
	if !ok || row.State != model.TaskStateEnabled {
		return nil
	}
	if row.LastFireAt > plannedAt {
		return nil
	}
	row.LastFireAt, row.NextFireAt = plannedAt, nextFireAt
	return nil
}

func (f fakeDefs) MarkResultTx(_ context.Context, _ sqlx.Session, taskKey string, successAt int64, lastError string) error {
	if row, ok := f.db.defs[taskKey]; ok {
		if successAt > row.LastSuccessAt {
			row.LastSuccessAt = successAt
		}
		row.LastError = lastError
	}
	return nil
}

func (f fakeDefs) SetNextFireAtTx(_ context.Context, _ sqlx.Session, taskKey string, nextFireAt int64) error {
	if row, ok := f.db.defs[taskKey]; ok {
		if nextFireAt < 0 {
			nextFireAt = 0 // 真 SQL：SetNextFireAtTx 把负值夹到 0
		}
		row.NextFireAt = nextFireAt
	}
	return nil
}

// SetStateTx 复刻 SetState 的 SQL：乐观锁 CAS + version+1，
// 并且 next_fire_at = CASE WHEN to_state = PAUSED THEN 0 ELSE next_fire_at END。
// 少了这个 CASE 就会把「暂停退出到期扫描」这一条真实语义测丢。
func (f fakeDefs) SetStateTx(_ context.Context, _ sqlx.Session, taskKey string, fromState, toState int32,
	expectedVersion int64, operator string) (bool, error) {
	f.db.stateCalls = append(f.db.stateCalls, stateCall{
		from: fromState, to: toState, expectedVersion: expectedVersion, operator: operator,
	})
	row, ok := f.db.defs[taskKey]
	if !ok || row.Version != expectedVersion {
		return false, nil
	}
	row.State, row.Operator, row.Version = toState, operator, row.Version+1
	if toState == model.TaskStatePaused {
		row.NextFireAt = 0
	}
	row.Mtime = fakeNow()
	return true, nil
}

// --- cron_task_checkpoint ---

type fakeCheckpoints struct {
	model.TaskCheckpointModel
	db *fakeDB
}

// SaveTx 复刻 CAS：expectedVersion=0 要求行不存在，>0 要求当前版本相等。
func (f fakeCheckpoints) SaveTx(_ context.Context, _ sqlx.Session, c *model.TaskCheckpoint,
	expectedVersion int64) (bool, error) {
	key := checkpointKey(c.TaskKey, c.ScopeKey)
	now := fakeNow()
	row, ok := f.db.checkpoints[key]
	if !ok {
		if expectedVersion != 0 {
			return false, nil
		}
		f.db.nextCp++
		stored := *c
		stored.ID, stored.Version, stored.Ctime, stored.Mtime = f.db.nextCp, 1, now, now
		f.db.checkpoints[key] = &stored
		return true, nil
	}
	if row.Version != expectedVersion {
		return false, nil
	}
	row.Value, row.ValueStr, row.Operator = c.Value, c.ValueStr, c.Operator
	row.Version, row.Mtime = row.Version+1, now
	return true, nil
}

func (f fakeCheckpoints) Save(ctx context.Context, c *model.TaskCheckpoint, expectedVersion int64) (bool, error) {
	return f.SaveTx(ctx, nil, c, expectedVersion)
}

// FindOne 记录读轨迹；FindOneTx 复用它（见下方实现），因此事务内读也会出现在同一轨迹里。
func (f fakeCheckpoints) FindOne(_ context.Context, taskKey, scopeKey string) (*model.TaskCheckpoint, error) {
	f.db.readCalls = append(f.db.readCalls, "checkpoint:"+taskKey+"/"+scopeKey)
	row, ok := f.db.checkpoints[checkpointKey(taskKey, scopeKey)]
	if !ok {
		return nil, nil
	}
	c := *row
	return &c, nil
}

func (f fakeCheckpoints) FindOneTx(ctx context.Context, _ sqlx.Session,
	taskKey, scopeKey string) (*model.TaskCheckpoint, error) {
	return f.FindOne(ctx, taskKey, scopeKey)
}

// ListByCursor 复刻「按 (task_key, scope_key) 升序 + 复合游标严格大于 + LIMIT」。
// 游标是 model.CompositeCursor 生成的 task_key + "\x1f" + scope_key；缺分隔符即非法，
// 这里必须把错误原样冒出去（logic 侧不允许退化成「当作第一页」）。
func (f fakeCheckpoints) ListByCursor(_ context.Context, taskKey, cursorKey string,
	limit int) ([]*model.TaskCheckpoint, string, error) {
	if limit <= 0 {
		limit = model.DefaultPageSize
	}
	f.db.cpCalls = append(f.db.cpCalls, cpListCall{taskKey: taskKey, cursorKey: cursorKey, limit: limit})
	cursorTask, cursorScope := "", ""
	if cursorKey != "" {
		t, s, err := model.SplitCompositeCursor(cursorKey)
		if err != nil {
			return nil, "", err
		}
		cursorTask, cursorScope = t, s
	}
	out := make([]*model.TaskCheckpoint, 0, len(f.db.checkpoints))
	for _, row := range f.db.checkpoints {
		if taskKey != "" && row.TaskKey != taskKey {
			continue
		}
		if cursorKey != "" && !(row.TaskKey > cursorTask ||
			(row.TaskKey == cursorTask && row.ScopeKey > cursorScope)) {
			continue
		}
		c := *row
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TaskKey != out[j].TaskKey {
			return out[i].TaskKey < out[j].TaskKey
		}
		return out[i].ScopeKey < out[j].ScopeKey
	})
	if len(out) > limit {
		out = out[:limit]
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = model.CompositeCursor(last.TaskKey, last.ScopeKey)
	}
	return out, next, nil
}

// CountByTask 与 ListByCursor 用同一套过滤条件（只差游标与 LIMIT），
// 否则 total 与当页行集会互相矛盾。
func (f fakeCheckpoints) CountByTask(_ context.Context, taskKey string) (int64, error) {
	var n int64
	for _, row := range f.db.checkpoints {
		if taskKey != "" && row.TaskKey != taskKey {
			continue
		}
		n++
	}
	return n, nil
}

// --- cron_task_audit ---

type fakeAudits struct {
	model.TaskAuditModel
	db *fakeDB
}

func (f fakeAudits) Insert(_ context.Context, _ sqlx.Session, a *model.TaskAudit) error {
	if f.db.auditErr != nil {
		return f.db.auditErr
	}
	f.db.nextAud++
	c := *a
	c.ID = f.db.nextAud
	if c.Ctime == 0 {
		c.Ctime = fakeNow()
	}
	f.db.audits = append(f.db.audits, &c)
	a.ID = c.ID
	return nil
}

// ListByCursor 复刻「ORDER BY id DESC + 过滤 + LIMIT」，游标条件 id < cursorID。
func (f fakeAudits) ListByCursor(_ context.Context, taskKey, action string,
	ctimeFrom, ctimeTo, cursorID int64, limit int) ([]*model.TaskAudit, int64, error) {
	if limit <= 0 {
		limit = model.DefaultPageSize
	}
	f.db.auditCalls = append(f.db.auditCalls, auditListCall{
		taskKey: taskKey, action: action, ctimeFrom: ctimeFrom, ctimeTo: ctimeTo,
		cursorID: cursorID, limit: limit,
	})
	rows := f.filtered(taskKey, action, ctimeFrom, ctimeTo)
	if cursorID > 0 {
		kept := make([]*model.TaskAudit, 0, len(rows))
		for _, r := range rows {
			if r.ID < cursorID {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	next := int64(0)
	if len(rows) == limit {
		next = rows[len(rows)-1].ID
	}
	return rows, next, nil
}

func (f fakeAudits) CountByFilter(_ context.Context, taskKey, action string,
	ctimeFrom, ctimeTo int64) (int64, error) {
	return int64(len(f.filtered(taskKey, action, ctimeFrom, ctimeTo))), nil
}

// filtered 按 (task_key, action, ctime 区间) 过滤，返回 id 倒序的副本集合。
func (f fakeAudits) filtered(taskKey, action string, ctimeFrom, ctimeTo int64) []*model.TaskAudit {
	out := make([]*model.TaskAudit, 0, len(f.db.audits))
	for _, a := range f.db.audits {
		if taskKey != "" && a.TaskKey != taskKey {
			continue
		}
		if action != "" && a.Action != action {
			continue
		}
		if ctimeFrom > 0 && a.Ctime < ctimeFrom {
			continue
		}
		if ctimeTo > 0 && a.Ctime > ctimeTo {
			continue
		}
		c := *a
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// --- 装配 ---

// newTestSvc 构造一份「只换掉数据访问、其余全用真实实现」的 ServiceContext：
// LeaseTTL/PageSize/ServerTime/WorkerID/Transact 都是被测代码，不能被替身污染。
// Registry 给的是**空的**真实注册表：注册/更新是否放行完全由 Spec 决定，
// 需要某个 handler 可用时用 registerHandler 显式登记（不许造第二套注册表语义）。
func newTestSvc(t *testing.T) (*svc.ServiceContext, *fakeDB) {
	t.Helper()
	db := newFakeDB()
	ctx := &svc.ServiceContext{
		Config: config.Config{
			Lease: config.LeaseConf{
				DefaultTTLSeconds: 300,
				MinTTLSeconds:     30,
				MaxTTLSeconds:     86400,
				PreemptionEnabled: true,
			},
			Task: config.TaskConf{
				DefaultPageSize:       20,
				MaxPageSize:           100,
				MaxParamsBytes:        4096,
				MaxResultSummaryBytes: 1024,
				DefaultTimezone:       "UTC",
			},
		},
		TaskDefinitions: fakeDefs{db: db},
		Runs:            fakeRuns{db: db},
		Leases:          fakeLeases{db: db},
		Checkpoints:     &fakeCheckpoints{db: db},
		Audits:          fakeAudits{db: db},
		Conn:            fakeConn{db: db},
		Registry:        registry.New(),
	}
	return ctx, db
}

// noopHandler 处理器占位实现：本轮 logic 单测只关心「这个 handler 在本进程存不存在」
// 以及 Spec 的代码边界（SerialOnly / max_attempts 上限 / TTL 下限），
// 不执行任何业务动作（真正的处理器在第二轮 internal/handlers 落地）。
func noopHandler(context.Context, *registry.Input) (*registry.Output, error) {
	return &registry.Output{}, nil
}

// registerHandler 往被测 ServiceContext 的注册表里登记一个 handler。
// Spec.WithHandler 是本服务给包外装配留的最小注入缝：handler 字段私有，
// 没有它就没有任何路径能从 logic 包里造出「已注册且可执行」的 handler。
func registerHandler(t *testing.T, svcCtx *svc.ServiceContext, spec registry.Spec) {
	t.Helper()
	if svcCtx.Registry == nil {
		t.Fatal("newTestSvc 必须给出真实注册表，否则注入缝被绕过")
	}
	if err := svcCtx.Registry.Register(spec.WithHandler(noopHandler)); err != nil {
		t.Fatalf("登记 handler %s: %v", spec.Name, err)
	}
}

// registerOK 登记一个「只有名字、没有任何代码边界」的处理器，即注册表放行一切配置。
func registerOK(t *testing.T, svcCtx *svc.ServiceContext, name string) {
	t.Helper()
	registerHandler(t, svcCtx, registry.Spec{Name: name})
}

// seedDefinition 放一条最小可用的任务定义（调度字段按需再改）。
// 静默写入：不经过任何 model 方法，因此不会污染 listCalls/txRuns 这类调用轨迹。
func seedDefinition(db *fakeDB, d *model.TaskDefinition) *model.TaskDefinition {
	row := *d
	if row.Version == 0 {
		row.Version = 1
	}
	if row.ID == 0 {
		row.ID = int64(len(db.defs) + 1)
	}
	if row.ID > db.nextDef {
		db.nextDef = row.ID // 之后 InsertTx 的自增号必须排在种子行后面
	}
	db.defs[row.TaskKey] = &row
	c := row
	return &c
}

// seedRunning 放一条「已被 owner 持有且未过期」的执行记录，用于栅栏/归属用例。
func seedRunning(db *fakeDB, runID int64, taskKey, owner string, fence int64, expireAt int64) *model.TaskRun {
	row := &model.TaskRun{
		ID: runID, TaskKey: taskKey, PlannedAt: fakeNow() - 60, Attempt: 1,
		State: model.RunStateRunning, LeaseOwner: owner, FenceToken: fence,
		LeaseExpireAt: expireAt, StartedAt: fakeNow() - 50, Ctime: fakeNow(), Mtime: fakeNow(),
	}
	db.runs[runID] = row
	if runID > db.nextRun {
		db.nextRun = runID
	}
	c := *row
	return &c
}

// seedLease 放一条任务级租约。expireAt=0 表示已释放。
func seedLease(db *fakeDB, leaseKey, taskKey, owner string, fence, expireAt, runID int64) *model.TaskLease {
	row := &model.TaskLease{
		ID: db.nextLeas + 1, LeaseKey: leaseKey, TaskKey: taskKey, OwnerInstance: owner,
		FenceToken: fence, ExpireAt: expireAt, RunID: runID, Ctime: fakeNow(), Mtime: fakeNow(),
	}
	db.nextLeas = row.ID
	db.leases[leaseKey] = row
	c := *row
	return &c
}

// seedRun 放一条任意形态的执行记录（静默写入，不产生调用轨迹）。
// 缺省补齐：attempt=1、ctime/mtime=now、state=PENDING，其余按入参原样入库。
func seedRun(db *fakeDB, r *model.TaskRun) *model.TaskRun {
	row := *r
	if row.ID == 0 {
		db.nextRun++
		row.ID = db.nextRun
	}
	if row.ID > db.nextRun {
		db.nextRun = row.ID
	}
	if row.Attempt == 0 {
		row.Attempt = 1
	}
	if row.State == model.RunStateUnspecified {
		row.State = model.RunStatePending
	}
	if row.Ctime == 0 {
		row.Ctime = fakeNow()
	}
	if row.Mtime == 0 {
		row.Mtime = row.Ctime
	}
	db.runs[row.ID] = &row
	c := row
	return &c
}

// seedAuditRow 放一条审计（静默写入，不经过 fakeAudits.Insert，因此不受 auditErr 影响）。
// id=0 时按追加序自动编号；ctime=0 时取当前秒。
func seedAuditRow(db *fakeDB, a *model.TaskAudit) *model.TaskAudit {
	row := *a
	if row.ID == 0 {
		db.nextAud++
		row.ID = db.nextAud
	}
	if row.ID > db.nextAud {
		db.nextAud = row.ID
	}
	if row.Ctime == 0 {
		row.Ctime = fakeNow()
	}
	db.audits = append(db.audits, &row)
	c := row
	return &c
}

// seedCursor 放一条增量游标（version 恒为 1：读侧用例不关心 CAS 历史）。
func seedCursor(db *fakeDB, taskKey, scopeKey string, value int64, valueStr string) *model.TaskCheckpoint {
	key := checkpointKey(taskKey, scopeKey)
	if _, ok := db.checkpoints[key]; ok {
		panic("seedCursor: 同一 (task_key, scope_key) 被铺了两次，uniq_task_scope 会被破坏")
	}
	db.nextCp++
	now := fakeNow()
	row := &model.TaskCheckpoint{
		ID: db.nextCp, TaskKey: taskKey, ScopeKey: scopeKey,
		Value: value, ValueStr: valueStr, Version: 1, Ctime: now, Mtime: now,
	}
	db.checkpoints[key] = row
	c := *row
	return &c
}

// onlyRun 取库里唯一一条执行记录，多条即视为「重复排队」。
func onlyRun(t *testing.T, db *fakeDB) *model.TaskRun {
	t.Helper()
	if len(db.runs) != 1 {
		t.Fatalf("库里应有恰好 1 条执行记录，得到 %d 条：%v", len(db.runs), runIDs(db.runs))
	}
	for _, r := range db.runs {
		c := *r
		return &c
	}
	return nil
}

// runIDs 把执行记录集压成「id:task_key@planned#attempt」文本序列，用于失败信息。
func runIDs(runs map[int64]*model.TaskRun) []string {
	out := make([]string, 0, len(runs))
	for id, r := range runs {
		out = append(out, fmt.Sprintf("%d:%s@%d#%d/%s", id, r.TaskKey, r.PlannedAt, r.Attempt,
			model.RunStateName(r.State)))
	}
	sort.Strings(out)
	return out
}
