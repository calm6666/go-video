package logic

// 本文件提供 membership logic 单测用的「内存版 model」与「假事务」。
//
// 为什么可以这样注入（以及为什么这不是伪装）：
//   - svc.ServiceContext 的六个 model 字段是接口类型，测试可以直接赋值；
//   - 真正的并发正确性由 MySQL 的 uniq_* / PRIMARY KEY 与事务内 SELECT 保证，单元测试无法也
//     不该复刻锁；这里复刻的是**语义**：唯一键命中即返回可被 IsDuplicate 识别的错误、
//     CAS 条件不命中即 RowsAffected=0（logic 据此判定失败）、回调报错整体回滚、
//     「没有这行」返回 (nil, nil) 而「读不动这行」返回错误；
//   - 断言的是 logic 面对这些返回值的裁决，也就是本轮唯一可在无库条件下证明的部分；
//   - 每个假实现只嵌入接口并覆写被测路径用到的方法，其余方法由内嵌的 nil 接口提升而来，
//     一旦测试走到未实现的分支会立刻 panic —— 失败是响的，不会被写成「通过」。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"go-video/services/membership/internal/config"
	"go-video/services/membership/internal/svc"
	"go-video/services/membership/model"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// nowSec 与 model.NowUnix() 同源（真实时钟）。
// 用例一律用「相对现在的偏移」构造时间，避免依赖挂钟具体读数。
func nowSec() int64 { return time.Now().Unix() }

// dupErr 复刻 MySQL 1062 的报文形态：model 的 isDuplicateErr 就是按报文判定的
// （见 model/errors.go 的说明：不 import 驱动专有错误类型）。
// 因此这里必须造报文而不是造一个自定义错误类型，否则测的是假实现的脾气。
func dupErr(key string) error {
	return fmt.Errorf("Error 1062 (23000): Duplicate entry '%s' for key '%s'", key, key)
}

// maxRequestIDWidth 是 request_id 的列宽（VARCHAR(64)）。
// logic 的 maxRequestIdLen 必须与它相等，由 TestRequireRequestIdHonoursColumnWidth 钉住；
// 与建表语句的对照在 model/migration_parity_test.go。
const maxRequestIDWidth = 64

// 读失败注入用的哨兵错误：断言「错误原样上抛」时按 errors.Is 比对，
// 不用报文匹配，避免把「恰好包含关键字」当成「同一次故障」。
var (
	errPlanDown     = errors.New("fake: mb_plan read failed")
	errLogDown      = errors.New("fake: mb_plan_change_log read failed")
	errLedgerDown   = errors.New("fake: mb_grant read failed")
	errIdentityDown = errors.New("fake: mb_membership read failed")
	errRequestDown  = errors.New("fake: mb_biz_request read failed")
)

func memberKey(mid int64, vipType int32) string { return fmt.Sprintf("%d:%d", mid, vipType) }

// fakeDB 是一次测试的全部内存态。TransactCtx 在入口快照它、回调报错时整体回滚，
// 这样才能真正断言「CAS 冲突/唯一键冲突不得留下半条结果」。
type fakeDB struct {
	plans      map[int64]*model.Plan
	planLogs   map[string]*model.PlanChangeLog // key: request_id（唯一索引）
	ents       map[string]*model.Entitlement   // key: code（uniq_code）
	members    map[string]*model.Membership    // key: mid:vip（uniq_mid_vip_type）
	grants     map[int64]*model.Grant
	bizRequest map[string]*model.BizRequest // key: request_id（主键）

	nextPlan   int64
	nextLog    int64
	nextEnt    int64
	nextMember int64
	nextGrant  int64

	// 读失败注入：证明「DB 故障不会被折叠成未开通/未找到/无台账」。
	planErr    error
	entErr     error
	memberErr  error
	grantErr   error
	requestErr error
	planLogErr error

	// planCodePreReadMiss 模拟「按 plan_code 预读时还没有这行、真正 INSERT 时已被并发占走」：
	// 只有第一次 FindByCode 返回 nil，冲突后的回查能看到那行。
	planCodePreReadMiss bool
	planCodePreReadDone bool

	// membershipConcurrentCommit 模拟「身份行被读到之后、写回之前，另一事务提交了同一行」：
	// 读路径先把库里的 version +1，于是按旧 version 的 UPDATE 条件不命中（RowsAffected=0），
	// logic 必须据此判定 ErrConcurrentUpdate 而不是假装写成功。
	membershipConcurrentCommit bool

	// membershipAppearsOnInsert 模拟「事务内读时还没有身份行、INSERT 时已被并发首开占走」：
	// 读路径暂时看不见那行（已在 map 里），INSERT 时才撞 uniq_mid_vip_type。
	// 这条路径的结论只能是 ErrConcurrentUpdate——幂等键此时并没有冲突。
	membershipAppearsOnInsert bool

	// entAppearsOnInsert 同上一条，只是撞的是 mb_entitlement.uniq_code：
	// 并发方那行已 seed 在 map 里，但读路径暂时看不见，INSERT 时才冲突。
	entAppearsOnInsert bool

	// grantPreReadMiss 模拟「预读幂等键时那行还不存在、真正写入时唯一键已被并发占走」：
	// 只有第一次 FindByRequestID 返回 nil，回查阶段能看到冲突行。
	grantPreReadMiss bool
	grantPreReadDone bool

	txRuns int

	// 调用计数不参与快照/回滚，只用于「批量读不打成逐码读」「非法 mid 不查库」这类路径断言。
	entFindByCodeCalls      int
	entListByCodesCalls     int
	memberListByMidCalls    int
	grantFindByRequestCalls int

	// 读侧列表方法的调用计数与入参快照：
	// 计数用来证明「守卫不通过时根本没触库」（而不是靠日志猜），
	// 入参快照用来证明「过滤条件按口径折算下去了」（枚举序数 vs 位掩码、裁剪后的 limit/offset）。
	planFindOneCalls    int
	planFindByCodeCalls int
	planListOnSaleCalls int
	planListAdminCalls  int
	entListCalls        int
	memberListExpCalls  int
	grantListCalls      int
	lastPlanQuery       model.PlanQuery
	lastGrantQuery      model.GrantQuery
	lastEntListEnabled  bool
	lastExpireScan      expireScanArgs
	lastExpireAutoRenew bool
	lastOnSaleScan      onSaleScanArgs
}

// expireScanArgs 记 ListExpiring 的区间与批量上限（闭区间 + limit 裁剪结果）。
type expireScanArgs struct {
	from, to, limit int64
}

// onSaleScanArgs 记 ListOnSale 的两个过滤入参（平台位掩码与档位；0 表示不过滤）。
type onSaleScanArgs struct {
	platformBit uint32
	vipType     int32
}

// 真 SQL 里写死的读上限（见 model/plan.go、model/entitlement.go、model/membership.go）。
// 假实现必须复刻，否则「静默截断」这类行为在测试里看不见。
const (
	entListSQLRowLimit    = 500 // mb_entitlement List ORDER BY ... LIMIT 500
	planOnSaleSQLRowLimit = 200 // mb_plan ListOnSale ORDER BY plan_id LIMIT 200
)

func newFakeDB() *fakeDB {
	return &fakeDB{
		plans:      map[int64]*model.Plan{},
		planLogs:   map[string]*model.PlanChangeLog{},
		ents:       map[string]*model.Entitlement{},
		members:    map[string]*model.Membership{},
		grants:     map[int64]*model.Grant{},
		bizRequest: map[string]*model.BizRequest{},
	}
}

func (db *fakeDB) snapshot() *fakeDB {
	s := &fakeDB{
		plans:      map[int64]*model.Plan{},
		planLogs:   map[string]*model.PlanChangeLog{},
		ents:       map[string]*model.Entitlement{},
		members:    map[string]*model.Membership{},
		grants:     map[int64]*model.Grant{},
		bizRequest: map[string]*model.BizRequest{},
		nextPlan:   db.nextPlan,
		nextLog:    db.nextLog,
		nextEnt:    db.nextEnt,
		nextMember: db.nextMember,
		nextGrant:  db.nextGrant,
		txRuns:     db.txRuns,
	}
	for k, v := range db.plans {
		c := *v
		s.plans[k] = &c
	}
	for k, v := range db.planLogs {
		c := *v
		s.planLogs[k] = &c
	}
	for k, v := range db.ents {
		c := *v
		s.ents[k] = &c
	}
	for k, v := range db.members {
		c := *v
		s.members[k] = &c
	}
	for k, v := range db.grants {
		c := *v
		s.grants[k] = &c
	}
	for k, v := range db.bizRequest {
		c := *v
		s.bizRequest[k] = &c
	}
	return s
}

func (db *fakeDB) restore(s *fakeDB) {
	db.plans = s.plans
	db.planLogs = s.planLogs
	db.ents = s.ents
	db.members = s.members
	db.grants = s.grants
	db.bizRequest = s.bizRequest
	db.nextPlan, db.nextLog = s.nextPlan, s.nextLog
	db.nextEnt, db.nextMember, db.nextGrant = s.nextEnt, s.nextMember, s.nextGrant
}

// grantByRequest 按 request_id 找最新一条台账（真 SQL 里 request_id 是唯一索引）。
func (db *fakeDB) grantByRequest(requestID string) *model.Grant {
	var best *model.Grant
	for _, g := range db.grants {
		if g.RequestID == requestID && (best == nil || g.GrantID > best.GrantID) {
			c := *g
			best = &c
		}
	}
	return best
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

// --- mb_plan ---

type fakePlans struct {
	model.PlanModel
	db *fakeDB
}

func (f fakePlans) FindOne(_ context.Context, planID int64) (*model.Plan, error) {
	f.db.planFindOneCalls++
	if f.db.planErr != nil {
		return nil, f.db.planErr
	}
	row, ok := f.db.plans[planID]
	if !ok {
		return nil, nil
	}
	c := *row
	return &c, nil
}

func (f fakePlans) FindByCode(_ context.Context, code string) (*model.Plan, error) {
	f.db.planFindByCodeCalls++
	if f.db.planErr != nil {
		return nil, f.db.planErr
	}
	for _, p := range f.db.plans {
		if p.PlanCode == code {
			if f.db.planCodePreReadMiss && !f.db.planCodePreReadDone {
				// 预读发生在并发创建提交之前：看不见，只能靠唯一索引兜底。
				f.db.planCodePreReadDone = true
				return nil, nil
			}
			c := *p
			return &c, nil
		}
	}
	return nil, nil
}

// InsertTx 复刻 uniq_plan_code(plan_code) 的语义：命中即 1062，不返回「已存在但也算成功」。
func (f fakePlans) InsertTx(_ context.Context, _ sqlx.Session, p *model.Plan) (int64, error) {
	for _, row := range f.db.plans {
		if row.PlanCode == p.PlanCode {
			return 0, dupErr("uniq_plan_code")
		}
	}
	f.db.nextPlan++
	now := nowSec()
	stored := *p
	stored.PlanID = f.db.nextPlan
	stored.Version = 1
	if stored.Ctime == 0 {
		stored.Ctime = now
	}
	stored.Mtime = now
	f.db.plans[stored.PlanID] = &stored
	p.PlanID = stored.PlanID
	p.Version = stored.Version
	return stored.PlanID, nil
}

// UpdateTx 的 CAS 条件与真 SQL 一致：WHERE plan_id = ? AND version = ?。
// state 不在 UPDATE 列表里（上下架只能走 CASStateTx）。
func (f fakePlans) UpdateTx(_ context.Context, _ sqlx.Session, p *model.Plan, expectedVersion int64) (bool, error) {
	row, ok := f.db.plans[p.PlanID]
	if !ok || row.Version != expectedVersion {
		return false, nil
	}
	row.PlanCode = p.PlanCode
	row.Name = p.Name
	row.Description = p.Description
	row.VipType = p.VipType
	row.DurationDays = p.DurationDays
	row.UnitCount = p.UnitCount
	row.PriceMinor = p.PriceMinor
	row.PromPriceMinor = p.PromPriceMinor
	row.Currency = p.Currency
	row.PlatformMask = p.PlatformMask
	row.AutoRenewSupported = p.AutoRenewSupported
	row.UpdatedBy = p.UpdatedBy
	row.Version++
	row.Mtime = nowSec()
	return true, nil
}

// CASStateTx 的条件是 state = fromState AND version = expectedVersion，两个条件缺一即 0 行。
func (f fakePlans) CASStateTx(_ context.Context, _ sqlx.Session, planID int64,
	fromState, toState int32, expectedVersion int64, updatedBy string) (bool, error) {
	row, ok := f.db.plans[planID]
	if !ok || row.State != fromState || row.Version != expectedVersion {
		return false, nil
	}
	row.State = toState
	row.UpdatedBy = updatedBy
	row.Version++
	row.Mtime = nowSec()
	return true, nil
}

// ListOnSale 复刻终端面真 SQL：恒定 state=ON_SALE，platformBit!=0 时按位与过滤，
// vipType!=0 时按档位过滤，plan_id 升序，上限 200 行（model/plan.go 的 LIMIT 200）。
func (f fakePlans) ListOnSale(_ context.Context, platformBit uint32, vipType int32) ([]*model.Plan, error) {
	f.db.planListOnSaleCalls++
	f.db.lastOnSaleScan = onSaleScanArgs{platformBit: platformBit, vipType: vipType}
	if f.db.planErr != nil {
		return nil, f.db.planErr
	}
	var rows []*model.Plan
	for _, p := range f.db.plans {
		if p.State != model.PlanStateOnSale {
			continue
		}
		if platformBit != 0 && p.PlatformMask&platformBit == 0 {
			continue
		}
		if vipType != 0 && p.VipType != vipType {
			continue
		}
		c := *p
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].PlanID < rows[j].PlanID })
	if len(rows) > planOnSaleSQLRowLimit {
		rows = rows[:planOnSaleSQLRowLimit]
	}
	return rows, nil
}

// ListAdmin 复刻运营面真 SQL：状态/档位/keyword 前缀过滤 + COUNT 总数 +
// plan_id 倒序分页；总数为 0 时不查第二趟（返回 nil, 0, nil）。
// keyword 的两列 collation 不同：plan_code 是 utf8mb4_bin（逐字节、大小写敏感），
// name 走表级 utf8mb4_unicode_ci（大小写折叠），前缀匹配必须按各自口径复刻。
func (f fakePlans) ListAdmin(_ context.Context, q model.PlanQuery) ([]*model.Plan, int64, error) {
	f.db.planListAdminCalls++
	f.db.lastPlanQuery = q
	if f.db.planErr != nil {
		return nil, 0, f.db.planErr
	}
	kw := strings.TrimSpace(q.Keyword)
	var matched []*model.Plan
	for _, p := range f.db.plans {
		if q.State != 0 && p.State != q.State {
			continue
		}
		if q.VipType != 0 && p.VipType != q.VipType {
			continue
		}
		if kw != "" && !planKeywordHit(p, kw) {
			continue
		}
		c := *p
		matched = append(matched, &c)
	}
	total := int64(len(matched))
	if total == 0 {
		return nil, 0, nil
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].PlanID > matched[j].PlanID })
	return paginate(matched, q.Offset, q.Limit), total, nil
}

// planKeywordHit 判定 (plan_code LIKE 'kw%' OR name LIKE 'kw%')。
func planKeywordHit(p *model.Plan, kw string) bool {
	if strings.HasPrefix(p.PlanCode, kw) {
		return true
	}
	return strings.HasPrefix(strings.ToLower(p.Name), strings.ToLower(kw))
}

// paginate 按 offset/limit 切一页；offset 越界给空页而不是错误（真 SQL 的 LIMIT/OFFSET 语义）。
func paginate[T any](rows []T, offset, limit int64) []T {
	if offset >= int64(len(rows)) {
		return nil
	}
	out := rows[offset:]
	if limit > 0 && int64(len(out)) > limit {
		out = out[:limit]
	}
	return out
}

// --- mb_plan_change_log ---

type fakePlanLogs struct {
	model.PlanChangeLogModel
	db *fakeDB
}

func (f fakePlanLogs) InsertTx(_ context.Context, _ sqlx.Session, l *model.PlanChangeLog) (int64, error) {
	if _, dup := f.db.planLogs[l.RequestID]; dup {
		return 0, dupErr("uniq_request_id")
	}
	f.db.nextLog++
	stored := *l
	stored.LogID = f.db.nextLog
	if stored.Ctime == 0 {
		stored.Ctime = nowSec()
	}
	f.db.planLogs[stored.RequestID] = &stored
	l.LogID = stored.LogID
	return stored.LogID, nil
}

func (f fakePlanLogs) FindByRequestID(_ context.Context, requestID string) (*model.PlanChangeLog, error) {
	if f.db.planLogErr != nil {
		return nil, f.db.planLogErr
	}
	row, ok := f.db.planLogs[requestID]
	if !ok {
		return nil, nil
	}
	c := *row
	return &c, nil
}

func (f fakePlanLogs) IsDuplicate(err error) bool { return isDuplicateLike(err) }

// --- mb_entitlement ---

type fakeEntitlements struct {
	model.EntitlementModel
	db *fakeDB
}

func (f fakeEntitlements) FindByCode(_ context.Context, code string) (*model.Entitlement, error) {
	f.db.entFindByCodeCalls++
	if f.db.entErr != nil {
		return nil, f.db.entErr
	}
	row, ok := f.db.ents[code]
	if !ok {
		return nil, nil
	}
	if f.db.entAppearsOnInsert {
		// 并发提交发生在本次读与写之间：读时还不存在，写时才撞 uniq_code。
		return nil, nil
	}
	c := *row
	return &c, nil
}

func (f fakeEntitlements) ListByCodes(_ context.Context, codes []string) ([]*model.Entitlement, error) {
	f.db.entListByCodesCalls++
	if f.db.entErr != nil {
		return nil, f.db.entErr
	}
	var out []*model.Entitlement
	for _, code := range codes {
		if row, ok := f.db.ents[code]; ok {
			c := *row
			out = append(out, &c)
		}
	}
	return out, nil
}

// List 复刻目录读真 SQL：enabledOnly 时只出 enabled=1，按 (min_vip_type, entitlement_id) 升序，
// 并且**静默截断到 500 行**（model/entitlement.go 的 LIMIT 500，契约里没有分页字段）。
// 这条上限必须复刻出来，否则「目录超过 500 个码」在测试里是隐形的。
func (f fakeEntitlements) List(_ context.Context, enabledOnly bool) ([]*model.Entitlement, error) {
	f.db.entListCalls++
	f.db.lastEntListEnabled = enabledOnly
	if f.db.entErr != nil {
		return nil, f.db.entErr
	}
	var rows []*model.Entitlement
	for _, e := range f.db.ents {
		if enabledOnly && e.Enabled != 1 {
			continue
		}
		c := *e
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].MinVipType != rows[j].MinVipType {
			return rows[i].MinVipType < rows[j].MinVipType
		}
		return rows[i].EntitlementID < rows[j].EntitlementID
	})
	if len(rows) > entListSQLRowLimit {
		rows = rows[:entListSQLRowLimit]
	}
	return rows, nil
}

func (f fakeEntitlements) InsertTx(_ context.Context, _ sqlx.Session, e *model.Entitlement) (int64, error) {
	if _, dup := f.db.ents[e.Code]; dup {
		f.db.entAppearsOnInsert = false // 冲突已经暴露给 logic，读路径恢复可见
		return 0, dupErr("uniq_code")
	}
	f.db.nextEnt++
	now := nowSec()
	stored := *e
	stored.EntitlementID = f.db.nextEnt
	stored.Version = 1
	stored.Ctime, stored.Mtime = now, now
	f.db.ents[stored.Code] = &stored
	e.EntitlementID = stored.EntitlementID
	e.Version = stored.Version
	return stored.EntitlementID, nil
}

// UpdateTx 不更新 code（真 SQL 的 SET 列表里没有它）：权益码是跨服务稳定引用。
func (f fakeEntitlements) UpdateTx(_ context.Context, _ sqlx.Session, e *model.Entitlement,
	expectedVersion int64) (bool, error) {
	row, ok := f.db.ents[e.Code]
	if !ok || row.Version != expectedVersion {
		return false, nil
	}
	row.Name = e.Name
	row.Description = e.Description
	row.MinVipType = e.MinVipType
	row.Enabled = e.Enabled
	row.UpdatedBy = e.UpdatedBy
	row.Version++
	row.Mtime = nowSec()
	return true, nil
}

func (f fakeEntitlements) IsDuplicate(err error) bool { return isDuplicateLike(err) }

// --- mb_membership ---

type fakeMemberships struct {
	model.MembershipModel
	db *fakeDB
}

func (f fakeMemberships) FindOne(_ context.Context, mid int64, vipType int32) (*model.Membership, error) {
	if f.db.memberErr != nil {
		return nil, f.db.memberErr
	}
	row, ok := f.db.members[memberKey(mid, vipType)]
	if !ok {
		return nil, nil
	}
	if f.db.membershipAppearsOnInsert {
		// 并发首开发生在本次读与写之间：读时还不存在，写时才撞唯一键。
		f.db.membershipAppearsOnInsert = false
		return nil, nil
	}
	c := *row
	return &c, nil
}

func (f fakeMemberships) FindOneTx(ctx context.Context, _ sqlx.Session, mid int64, vipType int32,
) (*model.Membership, error) {
	// 只读路径不注入：并发提交只发生在事务内的「读后写」窗口，那才是 CAS 的真实场景。
	// 一次性开关：本次交给 logic 的是并发提交**之前**的快照，库里那行的 version 已前移，
	// 于是按旧 version 的 UPDATE 条件不命中（RowsAffected=0）。
	if f.db.membershipConcurrentCommit {
		f.db.membershipConcurrentCommit = false
		if row, ok := f.db.members[memberKey(mid, vipType)]; ok {
			stale := *row
			row.Version++
			return &stale, nil
		}
	}
	return f.FindOne(ctx, mid, vipType)
}

func (f fakeMemberships) ListByMid(_ context.Context, mid int64) ([]*model.Membership, error) {
	f.db.memberListByMidCalls++
	if f.db.memberErr != nil {
		return nil, f.db.memberErr
	}
	var out []*model.Membership
	for _, row := range f.db.members {
		if row.Mid == mid {
			c := *row
			out = append(out, &c)
		}
	}
	return out, nil
}

// ListExpiring 复刻 cron 扫描真 SQL：expire_at 闭区间 [from,to]、auto_renew 位过滤、
// 按 (expire_at, membership_id) 升序、limit 截断（limit<=0 时按 model 的兜底取 100）。
func (f fakeMemberships) ListExpiring(_ context.Context, from, to int64, autoRenewOnly bool, limit int64,
) ([]*model.Membership, error) {
	f.db.memberListExpCalls++
	f.db.lastExpireScan = expireScanArgs{from: from, to: to, limit: limit}
	f.db.lastExpireAutoRenew = autoRenewOnly
	if f.db.memberErr != nil {
		return nil, f.db.memberErr
	}
	if limit <= 0 {
		limit = 100
	}
	var rows []*model.Membership
	for _, m := range f.db.members {
		if m.ExpireAt < from || m.ExpireAt > to {
			continue
		}
		if autoRenewOnly && m.AutoRenew != 1 {
			continue
		}
		c := *m
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ExpireAt != rows[j].ExpireAt {
			return rows[i].ExpireAt < rows[j].ExpireAt
		}
		return rows[i].MembershipID < rows[j].MembershipID
	})
	if int64(len(rows)) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (f fakeMemberships) InsertTx(_ context.Context, _ sqlx.Session, m *model.Membership) (int64, error) {
	key := memberKey(m.Mid, m.VipType)
	if _, dup := f.db.members[key]; dup {
		return 0, dupErr("uniq_mid_vip_type")
	}
	f.db.nextMember++
	now := nowSec()
	stored := *m
	stored.MembershipID = f.db.nextMember
	stored.Version = 1
	if stored.Ctime == 0 {
		stored.Ctime = now
	}
	stored.Mtime = now
	f.db.members[key] = &stored
	m.MembershipID = stored.MembershipID
	m.Version = stored.Version
	return stored.MembershipID, nil
}

func (f fakeMemberships) UpdateTx(_ context.Context, _ sqlx.Session, m *model.Membership) (bool, error) {
	row, ok := f.db.members[memberKey(m.Mid, m.VipType)]
	if !ok {
		return false, fmt.Errorf("mb_membership Update: no row for %d:%d", m.Mid, m.VipType)
	}
	if row.Version != m.Version {
		return false, nil // WHERE version = ? 不命中：RowsAffected = 0
	}
	row.ExpireAt = m.ExpireAt
	row.AutoRenew = m.AutoRenew
	row.AutoRenewChannel = m.AutoRenewChannel
	row.AutoRenewSignedAt = m.AutoRenewSignedAt
	row.Source = m.Source
	row.PaidMonthCount = m.PaidMonthCount
	row.Version++
	row.Mtime = nowSec()
	return true, nil
}

func (f fakeMemberships) IsDuplicate(err error) bool { return isDuplicateLike(err) }

// --- mb_grant ---

type fakeGrants struct {
	model.GrantModel
	db *fakeDB
}

func (f fakeGrants) FindByRequestID(_ context.Context, requestID string) (*model.Grant, error) {
	f.db.grantFindByRequestCalls++
	if f.db.grantErr != nil {
		return nil, f.db.grantErr
	}
	row := f.db.grantByRequest(requestID)
	if row == nil {
		return nil, nil
	}
	if f.db.grantPreReadMiss && !f.db.grantPreReadDone {
		// 预读发生在并发提交之前：看不见，只能靠唯一索引兜底。
		f.db.grantPreReadDone = true
		return nil, nil
	}
	return row, nil
}

// FindAction 取该档位针对 expireAt 的最新一条该动作台账（与真 SQL 的 ORDER BY grant_id DESC 一致）。
func (f fakeGrants) FindAction(_ context.Context, mid int64, vipType int32, action string,
	expireAt int64) (*model.Grant, error) {
	var best *model.Grant
	for _, g := range f.db.grants {
		if g.Mid == mid && g.VipType == vipType && g.Action == action && g.AfterExpireAt == expireAt {
			if best == nil || g.GrantID > best.GrantID {
				c := *g
				best = &c
			}
		}
	}
	return best, nil
}

// List 复刻台账分页真 SQL（model/grant.go 的 grantQueryClause）：
// mid/vip_type/source/biz_order_no 精确匹配（0 或空表示不过滤，biz_order_no 先 TrimSpace）、
// ctime 闭区间、grant_id 倒序、COUNT 出总数；总数为 0 时不查第二趟。
func (f fakeGrants) List(_ context.Context, q model.GrantQuery) ([]*model.Grant, int64, error) {
	f.db.grantListCalls++
	f.db.lastGrantQuery = q
	if f.db.grantErr != nil {
		return nil, 0, f.db.grantErr
	}
	biz := strings.TrimSpace(q.BizOrderNo)
	var matched []*model.Grant
	for _, g := range f.db.grants {
		if q.Mid != 0 && g.Mid != q.Mid {
			continue
		}
		if q.VipType != 0 && g.VipType != q.VipType {
			continue
		}
		if q.Source != 0 && g.Source != q.Source {
			continue
		}
		if biz != "" && g.BizOrderNo != biz {
			continue
		}
		if q.FromTs > 0 && g.Ctime < q.FromTs {
			continue
		}
		if q.ToTs > 0 && g.Ctime > q.ToTs {
			continue
		}
		c := *g
		matched = append(matched, &c)
	}
	total := int64(len(matched))
	if total == 0 {
		return nil, 0, nil
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].GrantID > matched[j].GrantID })
	return paginate(matched, q.Offset, q.Limit), total, nil
}

func (f fakeGrants) InsertTx(_ context.Context, _ sqlx.Session, g *model.Grant) (int64, error) {
	if f.db.grantByRequest(g.RequestID) != nil {
		return 0, dupErr("uniq_request_id")
	}
	f.db.nextGrant++
	stored := *g
	stored.GrantID = f.db.nextGrant
	if stored.Ctime == 0 {
		stored.Ctime = nowSec()
	}
	f.db.grants[stored.GrantID] = &stored
	g.GrantID = stored.GrantID
	return stored.GrantID, nil
}

func (f fakeGrants) IsDuplicate(err error) bool { return isDuplicateLike(err) }

// --- mb_biz_request ---

type fakeRequests struct {
	model.BizRequestModel
	db *fakeDB
}

func (f fakeRequests) InsertTx(_ context.Context, _ sqlx.Session, r *model.BizRequest) error {
	if _, dup := f.db.bizRequest[r.RequestID]; dup {
		return dupErr("PRIMARY")
	}
	stored := *r
	if stored.Ctime == 0 {
		stored.Ctime = nowSec()
	}
	f.db.bizRequest[stored.RequestID] = &stored
	return nil
}

func (f fakeRequests) FindByRequestIDTx(_ context.Context, _ sqlx.Session,
	requestID string) (*model.BizRequest, error) {
	if f.db.requestErr != nil {
		return nil, f.db.requestErr
	}
	row, ok := f.db.bizRequest[requestID]
	if !ok {
		return nil, nil
	}
	c := *row
	return &c, nil
}

// UpdateResultTx 在真 SQL 里是 UPDATE ... WHERE request_id = ?：行不存在时影响 0 行，
// 但 sqlx 不把它当错误（幂等键是本事务刚插进去的，正常路径必然命中）。
func (f fakeRequests) UpdateResultTx(_ context.Context, _ sqlx.Session, requestID string, resultID int64) error {
	if row, ok := f.db.bizRequest[requestID]; ok {
		row.ResultID = resultID
	}
	return nil
}

func (f fakeRequests) IsDuplicate(err error) bool { return isDuplicateLike(err) }

// --- 装配 ---

// isDuplicateLike 与 model 的判定同源（按 1062 / Duplicate entry 报文）。
// TestFakeDuplicatePredicateMatchesModel 会把这里的实现和 model 的真实实现做对照，
// 防止「假实现自己认一套、生产认另一套」导致幂等分支被假通过。
func isDuplicateLike(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1062") || strings.Contains(msg, "Duplicate entry")
}

func newTestSvc(t *testing.T) (*svc.ServiceContext, *fakeDB) {
	t.Helper()
	db := newFakeDB()
	ctx := &svc.ServiceContext{
		Config:      config.Config{Membership: testConf()},
		Plan:        fakePlans{db: db},
		PlanLog:     fakePlanLogs{db: db},
		Entitlement: fakeEntitlements{db: db},
		Membership:  fakeMemberships{db: db},
		Grant:       fakeGrants{db: db},
		Request:     fakeRequests{db: db},
		DB:          fakeConn{db: db},
	}
	return ctx, db
}

// testConf 取 etc/membership.v1.yaml 里同一组数值；
// internal/config/config_load_test.go 已经锁死「yaml == default tag」，这里只需与之对齐。
func testConf() config.MembershipConf {
	return config.MembershipConf{
		MaxGrantDeltaDays:         3660,
		ExpireScanMaxLimit:        500,
		MaxPageSize:               100,
		DefaultPageSize:           20,
		DefaultCurrency:           "CNY",
		MembershipCacheTTLSeconds: 10,
	}
}

// --- 播种 ---

func seedPlan(db *fakeDB, p *model.Plan) *model.Plan {
	row := *p
	if row.PlanID == 0 {
		db.nextPlan++
		row.PlanID = db.nextPlan
	}
	if row.PlanCode == "" {
		row.PlanCode = fmt.Sprintf("plan-%d", row.PlanID)
	}
	if row.Version == 0 {
		row.Version = 1
	}
	db.plans[row.PlanID] = &row
	c := row
	return &c
}

func seedEntitlement(db *fakeDB, e *model.Entitlement) *model.Entitlement {
	row := *e
	if row.EntitlementID == 0 {
		db.nextEnt++
		row.EntitlementID = db.nextEnt
	}
	if row.Version == 0 {
		row.Version = 1
	}
	db.ents[row.Code] = &row
	c := row
	return &c
}

// seedMembership 放一条身份行。expireAt 用相对 now 的偏移传入，便于跨秒边界稳定断言。
func seedMembership(db *fakeDB, m *model.Membership) *model.Membership {
	row := *m
	if row.MembershipID == 0 {
		db.nextMember++
		row.MembershipID = db.nextMember
	}
	if row.Version == 0 {
		row.Version = 1
	}
	db.members[memberKey(row.Mid, row.VipType)] = &row
	c := row
	return &c
}

func seedGrant(db *fakeDB, g *model.Grant) *model.Grant {
	row := *g
	if row.GrantID == 0 {
		db.nextGrant++
		row.GrantID = db.nextGrant
	}
	db.grants[row.GrantID] = &row
	c := row
	return &c
}

// seedPlanLog 预置一条套餐变更台账（request_id 是 UNIQUE，重复播种直接炸，
// 免得测试里无意间用两条同键台账冒充「幂等」）。
func seedPlanLog(db *fakeDB, l *model.PlanChangeLog) *model.PlanChangeLog {
	row := *l
	if _, dup := db.planLogs[row.RequestID]; dup {
		panic("seedPlanLog: duplicate request_id " + row.RequestID)
	}
	db.nextLog++
	row.LogID = db.nextLog
	if row.Ctime == 0 {
		row.Ctime = nowSec()
	}
	db.planLogs[row.RequestID] = &row
	c := row
	return &c
}

// seedRequest 预置一条 mb_biz_request 幂等键（主键即 request_id）。
func seedRequest(db *fakeDB, r *model.BizRequest) *model.BizRequest {
	row := *r
	if _, dup := db.bizRequest[row.RequestID]; dup {
		panic("seedRequest: duplicate request_id " + row.RequestID)
	}
	if row.Ctime == 0 {
		row.Ctime = nowSec()
	}
	db.bizRequest[row.RequestID] = &row
	c := row
	return &c
}

// activePremium 放一条「大会员、还剩 30 天」的身份行。
func activePremium(db *fakeDB, mid int64) *model.Membership {
	return seedMembership(db, &model.Membership{
		Mid: mid, VipType: model.VipTypePremium,
		StartAt:  nowSec() - 40*model.SecondsPerDay,
		ExpireAt: nowSec() + 30*model.SecondsPerDay,
	})
}
