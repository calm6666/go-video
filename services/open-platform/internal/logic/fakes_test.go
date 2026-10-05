package logic

// fakes_test.go 提供本包 logic 单测用的「内存版 model」与「假事务」。
//
// 为什么可以这样注入（以及为什么这不算伪装通过）：
//   - svc.ServiceContext 的 model 字段都是接口类型，测试直接赋值即可；
//   - 真正的并发正确性建立在 MySQL 的 uniq_* 索引与条件 UPDATE 上，单测不复刻锁；
//     这里复刻的是**语义**：唯一键命中即 created=false、CAS 条件不命中即 applied=false、
//     撤销位点 revoked_at 一旦写下不可回退、FindActive 按 secret_id 倒序。
//     断言的是 logic 面对这些返回值时的裁决顺序与副作用，这正是无库条件下可证明的部分；
//   - 每个 fake 只内嵌接口并覆写被测路径用到的方法，其余方法由内嵌的 nil 接口提升：
//     测试一旦走到未实现的方法会当场 panic（响的失败），不会被悄悄写成「通过」；
//   - TransactCtx 在入口快照内存态、回调报错时整体回滚，
//     因此可以真正断言「事务中途失败不留半成品行」。
//     注意：调用计数（calls）不参与回滚——它记录「发生过几次尝试」，回滚只撤销数据状态；
//     断言副作用一律「计数增量 + 数据状态」两条一起看。
//
// 本文件只服务本包测试，不连接 MySQL/Redis/etcd：Cache 恒为 nil，
// 判定链在设计上就不依赖缓存（tokencheck.go 文件头），因此测试路径与线上一致。

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go-video/common/ratelimit"
	"go-video/services/open-platform/internal/config"
	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

const (
	testPepper        = "unit-test-credential-pepper"
	testWebhookPepper = "unit-test-webhook-pepper"

	testAppID int64 = 9001
	testOwner int64 = 42001
	testMid   int64 = 77001
	testApp2  int64 = 9002 // 另一个应用，用于越权断言

	testScopeR = "video.read"
	testScopeW = "video.publish"
	testAPIC   = "video.publish.create"
)

// nowTS 真实时钟：logic 一律走 nowUnix()，用例用「相对现在的偏移」构造时间，
// 不依赖挂钟具体读数，也不给生产代码开时钟注入口。
func nowTS() int64 { return time.Now().Unix() }

// ---------------------------------------------------------------- 断言小工具

// wantOK 断言成功并回传结果。Go 的两返回值无法内联，调用方按
// 「x, err := f(); x = wantOK(t, x, err, "…")」两行写。
func wantOK[T any](t *testing.T, got T, err error, label string) T {
	t.Helper()
	if err != nil {
		t.Fatalf("%s：意外失败：%v", label, err)
	}
	return got
}

// wantFail 断言「未通过门禁」：必须回指定哨兵，且响应体为空。
// reply 往往是 typed nil 指针，`reply != nil` 恒真，因此用 reflect 判空。
func wantFail(t *testing.T, reply any, err error, want error, label string) {
	t.Helper()
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("%s：应报 %v，实际 err=%v", label, want, err)
	}
	if isNilPtr(reply) {
		return
	}
	t.Fatalf("%s：失败路径不得带回响应体：%+v", label, reply)
}

// isNilPtr 判「接口里装的到底是不是 nil」。
func isNilPtr(v any) bool {
	rv := reflect.ValueOf(v)
	return !rv.IsValid() || (rv.Kind() == reflect.Ptr && rv.IsNil())
}

// wantCalls 断言某个 model 方法的调用增量（before 由调用方在动作前取 db.count 得到）。
// 用增量而不是绝对值：一条判定链里读几次由实现细节决定，但「写没发生、发生几次」是契约。
func wantCalls(t *testing.T, db *store, op string, before, want int, label string) {
	t.Helper()
	if got := db.count(op) - before; got != want {
		t.Fatalf("%s：%s 调用增量=%d，期望 %d", label, op, got, want)
	}
}

// wantNoWrites 断言「零副作用」：整条写路径一次都没发生。
// 遍历所有写侧方法名，避免每加一个写就漏一条断言。
func wantNoWrites(t *testing.T, db *store, before map[string]int, label string) {
	t.Helper()
	for op, n := range before {
		if db.count(op) != n {
			t.Fatalf("%s：%s 不该被调用（%d → %d）", label, op, n, db.count(op))
		}
	}
}

// writeOps 全部写侧方法名（断言零副作用用）。读方法不进这个集合：
// 判定链读几次是实现细节，但「写了没有」是契约。
var writeOps = []string{
	"DB.TransactCtx", "Apps.NextVersion",
	"Apps.InsertTx", "Apps.UpdateProfile", "Apps.UpdateStatus",
	"Secrets.InsertTx", "Secrets.MarkHistory", "Secrets.MarkAllHistory",
	"Secrets.TouchUsed",
	"AppScopes.Request", "AppScopes.Grant", "AppScopes.Revoke",
	"AuthCodes.InsertTx", "AuthCodes.MarkUsed", "AuthCodes.IncrReplay",
	"Grants.FindOrCreate", "Grants.MarkRevoked", "Grants.RevokeByMid",
	"Grants.RevokeByAppWithScopes", "Grants.TouchTokenHead", "Grants.TouchLastCode",
	"Tokens.InsertTx", "Tokens.MarkRotated", "Tokens.MarkRevoked",
	"Tokens.RevokeByGrant", "Tokens.RevokeByMid", "Tokens.RevokeByApp",
	"Tokens.RevokeByAppWithScopes", "Tokens.TouchUsed",
	"QuotaPolicies.Upsert", "QuotaPolicies.Disable",
	"QuotaUsages.Add", "QuotaUsages.RecomputeOverwrite",
	"CallLogs.Insert",
	"WebhookEndpoints.Insert", "WebhookEndpoints.SoftDelete",
	"WebhookEndpoints.MarkVerified", "WebhookEndpoints.SetEnabled",
	"WebhookDeliveries.Insert", "WebhookDeliveries.SuppressByEndpoint",
	"WebhookDeliveries.ResetForReplay",
}

// snapshotWrites 取当前所有写侧计数，供 wantNoWrites 对照。
func snapshotWrites(db *store) map[string]int {
	out := make(map[string]int, len(writeOps))
	for _, op := range writeOps {
		out[op] = db.count(op)
	}
	return out
}

// ---------------------------------------------------------------- 内存库

type store struct {
	apps      map[int64]*model.Application
	secrets   map[int64]*model.AppSecret
	scopes    map[string]*model.Scope
	appscopes map[int64]map[string]*model.AppScope
	granted   map[int64]map[string]bool
	codes     map[int64]*model.AuthCode
	grants    map[int64]*model.Grant
	tokens    map[int64]*model.Token
	policies  map[int64]*model.QuotaPolicy
	usage     map[string]*model.QuotaUsage
	logs      map[string]*model.ApiCallLog
	eps       []*model.WebhookEndpoint
	dels      []*model.WebhookDelivery

	seq map[string]int64

	calls map[string]int
	errs  map[string]error
	// hooks 在某个 model 方法被调用的**瞬间**执行注入动作（一次性）。
	// 只用于表达「logic 读到快照之后、事务内 CAS 之前，库里的行被别人改掉了」这类交错：
	// 纯前置构造做不到（logic 读的就是同一个值），改 logic 返回的副本更是不影响库。
	hooks map[string][]func()
}

func newStore() *store {
	return &store{
		apps:      map[int64]*model.Application{},
		secrets:   map[int64]*model.AppSecret{},
		scopes:    map[string]*model.Scope{},
		appscopes: map[int64]map[string]*model.AppScope{},
		granted:   map[int64]map[string]bool{},
		codes:     map[int64]*model.AuthCode{},
		grants:    map[int64]*model.Grant{},
		tokens:    map[int64]*model.Token{},
		policies:  map[int64]*model.QuotaPolicy{},
		usage:     map[string]*model.QuotaUsage{},
		logs:      map[string]*model.ApiCallLog{},
		seq:       map[string]int64{},
		calls:     map[string]int{},
		errs:      map[string]error{},
		hooks:     map[string][]func(){},
	}
}

// errFakeDown 通用下游故障。
var errFakeDown = errors.New("fake: downstream unavailable")

// hit 记一次调用；先跑该算子的一次性钩子（模拟并发交错），再返回注入错误。
func (db *store) hit(op string) error {
	db.calls[op]++
	if hs := db.hooks[op]; len(hs) > 0 {
		hs[0]()
		db.hooks[op] = hs[1:]
	}
	return db.errs[op]
}

func (db *store) count(op string) int { return db.calls[op] }

// failOn 让某个 model 方法从此刻起报错：用于「依赖故障必须 fail closed」的用例。
func (db *store) failOn(op string, err error) { db.errs[op] = err }

// onHit 在某个 model 方法下一次被调用的瞬间执行 fn（一次性）。
func (db *store) onHit(op string, fn func()) { db.hooks[op] = append(db.hooks[op], fn) }

func (db *store) next(k string) int64 { db.seq[k]++; return db.seq[k] }

// --- 快照 / 回滚（只覆盖数据态，不覆盖调用计数）

type txSnapshot struct {
	apps      map[int64]*model.Application
	secrets   map[int64]*model.AppSecret
	scopes    map[string]*model.Scope
	appscopes map[int64]map[string]*model.AppScope
	grants    map[int64]*model.Grant
	tokens    map[int64]*model.Token
	codes     map[int64]*model.AuthCode
	policies  map[int64]*model.QuotaPolicy
	usage     map[string]*model.QuotaUsage
	logs      map[string]*model.ApiCallLog
	granted   map[int64]map[string]bool
	eps       []*model.WebhookEndpoint
	dels      []*model.WebhookDelivery
	seq       map[string]int64
}

func cloneI64[V any](m map[int64]*V) map[int64]*V {
	out := make(map[int64]*V, len(m))
	for k, v := range m {
		c := *v
		out[k] = &c
	}
	return out
}

func cloneStr[V any](m map[string]*V) map[string]*V {
	out := make(map[string]*V, len(m))
	for k, v := range m {
		c := *v
		out[k] = &c
	}
	return out
}

func cloneNestedStr[V any](m map[int64]map[string]*V) map[int64]map[string]*V {
	out := make(map[int64]map[string]*V, len(m))
	for appID, rows := range m {
		inner := make(map[string]*V, len(rows))
		for k, v := range rows {
			c := *v
			inner[k] = &c
		}
		out[appID] = inner
	}
	return out
}

func cloneGranted(m map[int64]map[string]bool) map[int64]map[string]bool {
	out := make(map[int64]map[string]bool, len(m))
	for k, v := range m {
		inner := make(map[string]bool, len(v))
		for s, g := range v {
			inner[s] = g
		}
		out[k] = inner
	}
	return out
}

func (db *store) snapshot() *txSnapshot {
	return &txSnapshot{
		apps:      cloneI64(db.apps),
		secrets:   cloneI64(db.secrets),
		scopes:    cloneStr(db.scopes),
		appscopes: cloneNestedStr(db.appscopes),
		grants:    cloneI64(db.grants),
		tokens:    cloneI64(db.tokens),
		codes:     cloneI64(db.codes),
		policies:  cloneI64(db.policies),
		usage:     cloneStr(db.usage),
		logs:      cloneStr(db.logs),
		granted:   cloneGranted(db.granted),
		eps:       append([]*model.WebhookEndpoint(nil), db.eps...),
		dels:      append([]*model.WebhookDelivery(nil), db.dels...),
		seq:       map[string]int64{},
	}
}

func (db *store) restore(s *txSnapshot) {
	db.apps, db.secrets, db.grants = s.apps, s.secrets, s.grants
	db.tokens, db.policies, db.usage = s.tokens, s.policies, s.usage
	db.logs, db.granted = s.logs, s.granted
	db.scopes, db.appscopes, db.codes = s.scopes, s.appscopes, s.codes
	db.eps = append([]*model.WebhookEndpoint(nil), s.eps...)
	db.dels = append([]*model.WebhookDelivery(nil), s.dels...)
	db.seq = map[string]int64{}
	for k, v := range s.seq {
		db.seq[k] = v
	}
}

// fakeConn 只实现真正被测的 TransactCtx，其余 sqlx 方法由内嵌 nil 接口提升：
// 任何「想拿真连接执行 SQL」的写法都会当场 panic。
type fakeConn struct {
	sqlx.SqlConn
	db *store
}

func (c fakeConn) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	c.db.calls["DB.TransactCtx"]++
	snap := c.db.snapshot()
	// session 传 nil：本包 fake 一律忽略 session。真实现的 pick(session, conn) 在 session
	// 非空时走同一会话，内存版里「事务内的写」对后续读天然可见，语义等价。
	if err := fn(ctx, nil); err != nil {
		c.db.restore(snap)
		return err
	}
	return nil
}

// ---------------------------------------------------------------- op_app

type fakeApps struct {
	model.ApplicationModel
	db *store
}

func (f fakeApps) FindByID(_ context.Context, appID int64) (*model.Application, error) {
	if err := f.db.hit("Apps.FindByID"); err != nil {
		return nil, err
	}
	app, ok := f.db.apps[appID]
	if !ok {
		return nil, nil
	}
	c := *app
	return &c, nil
}

func (f fakeApps) NextVersion(_ context.Context, appID int64) (int32, error) {
	if err := f.db.hit("Apps.NextVersion"); err != nil {
		return 0, err
	}
	app, ok := f.db.apps[appID]
	if !ok {
		return 0, model.ErrAppNotFound
	}
	app.Version++
	return app.Version, nil
}

// insert 复刻真实现的三档唯一键语义：
// uniq_owner_name 冲突单独暴露（注册幂等不得掩盖重名）、
// uniq_register_token 命中回既有行且 created=false（幂等重放）、
// uniq_app_key 命中回普通错误（logic 侧靠 uniqueAppKey 预检，走到这里就是真冲突）。
func (f fakeApps) insert(_ context.Context, _ sqlx.Session, app *model.Application) (int64, bool, error) {
	if err := f.db.hit("Apps.InsertTx"); err != nil {
		return 0, false, err
	}
	if app == nil || app.RegisterToken == "" {
		return 0, false, model.ErrClientTokenRequired
	}
	for _, old := range f.db.apps {
		if old.OwnerMid == app.OwnerMid && old.Name == app.Name {
			return 0, false, model.ErrDuplicateAppName
		}
	}
	for _, old := range f.db.apps {
		if old.RegisterToken == app.RegisterToken {
			return old.AppID, false, nil
		}
	}
	if app.AppKey != "" {
		for _, old := range f.db.apps {
			if old.AppKey == app.AppKey {
				return 0, false, errors.New("fake: uniq_app_key conflict")
			}
		}
	}
	id := f.db.next("app")
	now := nowTS()
	row := *app
	row.AppID = id
	row.Version = 1 // 真实现的 INSERT 硬编码 version=1
	row.Ctime, row.Mtime = now, now
	f.db.apps[id] = &row
	return id, true, nil
}

func (f fakeApps) Insert(ctx context.Context, app *model.Application) (int64, bool, error) {
	return f.insert(ctx, nil, app)
}

func (f fakeApps) InsertTx(ctx context.Context, _ sqlx.Session,
	app *model.Application) (int64, bool, error) {
	return f.insert(ctx, nil, app)
}

func (f fakeApps) FindByAppKey(_ context.Context, appKey string) (*model.Application, error) {
	if err := f.db.hit("Apps.FindByAppKey"); err != nil {
		return nil, err
	}
	if appKey == "" {
		return nil, model.ErrAppKeyRequired
	}
	for _, app := range f.db.apps {
		if app.AppKey == appKey {
			c := *app
			return &c, nil
		}
	}
	return nil, nil
}

func (f fakeApps) FindByRegisterToken(_ context.Context, token string) (*model.Application, error) {
	if err := f.db.hit("Apps.FindByRegisterToken"); err != nil {
		return nil, err
	}
	if token == "" {
		return nil, model.ErrClientTokenRequired
	}
	for _, app := range f.db.apps {
		if app.RegisterToken == token {
			c := *app
			return &c, nil
		}
	}
	return nil, nil
}

// list 复刻真实现的 (mtime, app_id) 倒序游标 + LIMIT ps（多取一条由 logic 负责，
// 因此这里严格执行 ps，logic 的 trimPage 才有「是否还有下一页」的真依据）。
func (f fakeApps) list(match func(*model.Application) bool,
	cursorTime, cursorID int64, ps int32) ([]*model.Application, error) {
	if ps <= 0 {
		return nil, model.ErrInvalidPage
	}
	var rows []*model.Application
	for _, app := range f.db.apps {
		if match(app) {
			c := *app
			rows = append(rows, &c)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Mtime != rows[j].Mtime {
			return rows[i].Mtime > rows[j].Mtime
		}
		return rows[i].AppID > rows[j].AppID
	})
	if cursorTime > 0 {
		var kept []*model.Application
		for _, app := range rows {
			if app.Mtime < cursorTime || (app.Mtime == cursorTime && app.AppID < cursorID) {
				kept = append(kept, app)
			}
		}
		rows = kept
	}
	if int32(len(rows)) > ps {
		rows = rows[:ps]
	}
	return rows, nil
}

func (f fakeApps) ListByOwner(_ context.Context, ownerMid int64, status int32,
	cursorTime, cursorID int64, ps int32) ([]*model.Application, error) {
	if err := f.db.hit("Apps.ListByOwner"); err != nil {
		return nil, err
	}
	return f.list(func(a *model.Application) bool {
		return a.OwnerMid == ownerMid && (status <= 0 || a.Status == status)
	}, cursorTime, cursorID, ps)
}

func (f fakeApps) ListAll(_ context.Context, status int32, cursorTime, cursorID int64,
	ps int32) ([]*model.Application, error) {
	if err := f.db.hit("Apps.ListAll"); err != nil {
		return nil, err
	}
	return f.list(func(a *model.Application) bool {
		return status <= 0 || a.Status == status
	}, cursorTime, cursorID, ps)
}

// UpdateProfile CAS：WHERE app_id=? AND version=?，0 行即 ErrConcurrentUpdate。
// 三个字段全空时真实现直接返回 nil（不无谓自增版本），这里也照做。
func (f fakeApps) UpdateProfile(_ context.Context, appID int64, name, description,
	redirectURIs string, expectedVersion int32) error {
	if err := f.db.hit("Apps.UpdateProfile"); err != nil {
		return err
	}
	if name == "" && description == "" && redirectURIs == "" {
		return nil
	}
	app, ok := f.db.apps[appID]
	if !ok || app.Version != expectedVersion {
		return model.ErrConcurrentUpdate
	}
	if name != "" {
		app.Name = name
	}
	if description != "" {
		app.Description = description
	}
	if redirectURIs != "" {
		app.RedirectURIs = redirectURIs
	}
	app.Version++
	app.Mtime = nowTS()
	return nil
}

// UpdateStatus 复刻真实现的两道闸门：状态机图 + WHERE status=from 的 CAS。
func (f fakeApps) UpdateStatus(_ context.Context, appID int64, from, to int32,
	operator int64, reason string, offlineAt int64) error {
	if err := f.db.hit("Apps.UpdateStatus"); err != nil {
		return err
	}
	if !model.CanTransitionAppStatus(from, to) {
		return model.ErrInvalidStateTransition
	}
	if from == to {
		return nil
	}
	app, ok := f.db.apps[appID]
	if !ok || app.Status != from {
		return model.ErrConcurrentUpdate
	}
	if offlineAt == 0 && to == model.AppStatusOffline {
		offlineAt = nowTS()
	}
	app.Status = to
	app.Version++
	app.LastOperator = operator
	app.StatusReason = reason
	app.OfflineAt = offlineAt
	app.Mtime = nowTS()
	return nil
}

// ---------------------------------------------------------------- op_app_secret

type fakeSecrets struct {
	model.AppSecretModel
	db *store
}

func (f fakeSecrets) Insert(ctx context.Context, s *model.AppSecret) (int64, error) {
	return f.InsertTx(ctx, nil, s)
}

func (f fakeSecrets) InsertTx(_ context.Context, _ sqlx.Session, s *model.AppSecret) (int64, error) {
	if err := f.db.hit("Secrets.InsertTx"); err != nil {
		return 0, err
	}
	// 复刻 model 的入参门禁：明文绝不允许透传到本层，salt/hash 缺一即拒。
	if s == nil || s.AppID <= 0 || s.Hash == "" || s.Salt == "" {
		return 0, model.ErrSecretNotConfigured
	}
	id := f.db.next("secret")
	now := nowTS()
	row := *s
	row.SecretID = id
	row.Status = model.SecretStatusActive
	row.Ctime, row.Mtime = now, now
	f.db.secrets[id] = &row
	return id, nil
}

func (f fakeSecrets) FindByID(_ context.Context, secretID int64) (*model.AppSecret, error) {
	if err := f.db.hit("Secrets.FindByID"); err != nil {
		return nil, err
	}
	row, ok := f.db.secrets[secretID]
	if !ok {
		return nil, nil
	}
	c := *row
	return &c, nil
}

func (f fakeSecrets) FindActive(_ context.Context, appID, now int64) ([]*model.AppSecret, error) {
	if err := f.db.hit("Secrets.FindActive"); err != nil {
		return nil, err
	}
	var out []*model.AppSecret
	for _, s := range f.db.secrets {
		if s.AppID == appID && s.Status == model.SecretStatusActive &&
			(s.ExpiresAt == 0 || s.ExpiresAt > now) {
			c := *s
			out = append(out, &c)
		}
	}
	// 真实现是 ORDER BY secret_id DESC：宽限期内可能同时两把，首元素即「本次被替换的那把」。
	sort.Slice(out, func(i, j int) bool { return out[i].SecretID > out[j].SecretID })
	return out, nil
}

func (f fakeSecrets) markHistory(_ context.Context, secretID, expiresAt int64) error {
	if err := f.db.hit("Secrets.MarkHistory"); err != nil {
		return err
	}
	s, ok := f.db.secrets[secretID]
	if !ok || s.Status != model.SecretStatusActive {
		return nil // 条件 UPDATE 未命中：不是错误，只是 0 行
	}
	if expiresAt > 0 {
		// 宽限期：保持生效位，只把到期时刻提前到 deadline。
		s.ExpiresAt = expiresAt
	} else {
		s.Status = model.SecretStatusHistory
		s.ExpiresAt = 0
	}
	s.Mtime = nowTS()
	return nil
}

func (f fakeSecrets) MarkHistory(ctx context.Context, secretID, expiresAt int64) error {
	return f.markHistory(ctx, secretID, expiresAt)
}

func (f fakeSecrets) MarkHistoryTx(ctx context.Context, _ sqlx.Session, secretID, expiresAt int64) error {
	return f.markHistory(ctx, secretID, expiresAt)
}

func (f fakeSecrets) MarkAllHistory(_ context.Context, appID, operator int64, reason string) (int64, error) {
	if err := f.db.hit("Secrets.MarkAllHistory"); err != nil {
		return 0, err
	}
	var n int64
	for _, s := range f.db.secrets {
		if s.AppID != appID || s.Status != model.SecretStatusActive {
			continue
		}
		s.Status = model.SecretStatusHistory
		s.ExpiresAt = 0
		s.OperatorMid = operator
		s.RotateReason = reason
		s.Mtime = nowTS()
		n++
	}
	return n, nil
}

func (f fakeSecrets) CountActive(_ context.Context, appID, now int64) (int64, error) {
	if err := f.db.hit("Secrets.CountActive"); err != nil {
		return 0, err
	}
	var n int64
	for _, s := range f.db.secrets {
		if s.AppID == appID && s.Usable(now) {
			n++
		}
	}
	return n, nil
}

func (f fakeSecrets) TouchUsed(_ context.Context, secretID, ts int64) error {
	if err := f.db.hit("Secrets.TouchUsed"); err != nil {
		return err
	}
	if s, ok := f.db.secrets[secretID]; ok {
		s.LastUsedAt = ts
	}
	return nil
}

// SummariesByApps 列表投影的批量密钥概览：口径与 CountActive 一致，
// 但对所有 app 一次算完（逐个 CountActive 就是 N+1，测试要能区分两者）。
func (f fakeSecrets) SummariesByApps(_ context.Context, appIDs []int64,
	now int64) (map[int64]*model.SecretSummary, error) {
	if err := f.db.hit("Secrets.SummariesByApps"); err != nil {
		return nil, err
	}
	out := make(map[int64]*model.SecretSummary, len(appIDs))
	want := make(map[int64]struct{}, len(appIDs))
	for _, id := range appIDs {
		want[id] = struct{}{}
	}
	for _, s := range f.db.secrets {
		if _, ok := want[s.AppID]; !ok {
			continue
		}
		sum := out[s.AppID]
		if sum == nil {
			sum = &model.SecretSummary{}
			out[s.AppID] = sum
		}
		sum.Total++
		if s.Usable(now) {
			sum.Active++
		}
		if s.Ctime > sum.LastCtime {
			sum.LastCtime = s.Ctime
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- 目录与审批

type fakeScopes struct {
	model.ScopeModel
	db *store
}

func (f fakeScopes) FindByScope(_ context.Context, scope string) (*model.Scope, error) {
	if err := f.db.hit("Scopes.FindByScope"); err != nil {
		return nil, err
	}
	def, ok := f.db.scopes[scope]
	if !ok {
		return nil, nil
	}
	c := *def
	return &c, nil
}

func (f fakeScopes) FindByScopes(_ context.Context, scopes []string) (map[string]*model.Scope, error) {
	if err := f.db.hit("Scopes.FindByScopes"); err != nil {
		return nil, err
	}
	out := map[string]*model.Scope{}
	for _, sc := range scopes {
		if def, ok := f.db.scopes[sc]; ok {
			c := *def
			out[sc] = &c
		}
	}
	return out, nil
}

// List 目录全量：真实现是 ORDER BY scope ASC，且 onlyEnabled 时过滤 enabled<>1。
func (f fakeScopes) List(_ context.Context, onlyEnabled bool) ([]*model.Scope, error) {
	if err := f.db.hit("Scopes.List"); err != nil {
		return nil, err
	}
	var out []*model.Scope
	for _, def := range f.db.scopes {
		if onlyEnabled && def.Enabled != 1 {
			continue
		}
		c := *def
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scope < out[j].Scope })
	return out, nil
}

func (f fakeScopes) Upsert(_ context.Context, s *model.Scope) error {
	if err := f.db.hit("Scopes.Upsert"); err != nil {
		return err
	}
	if s == nil || s.Scope == "" {
		return model.ErrScopeUnknown
	}
	row := *s
	f.db.scopes[s.Scope] = &row
	return nil
}

type fakeAppScopes struct {
	model.AppScopeModel
	db *store
}

// appScopeRows 取（必要时创建）某应用的 scope 关系行集合。
func (db *store) appScopeRows(appID int64) map[string]*model.AppScope {
	if db.appscopes[appID] == nil {
		db.appscopes[appID] = map[string]*model.AppScope{}
	}
	return db.appscopes[appID]
}

// transition 复刻真实现：授予/回收都落到 uniq (app_id, scope) 的同一行，重复调用幂等；
// granted 视图（FindGrantedScopes / ListGranted 的读侧）随状态同步，
// 这样「回收后 token 立刻拿不到该 scope」才是可断言的。
func (f fakeAppScopes) transition(op string, appID int64, scopes []string, operator int64,
	reason string, state int32) error {
	if err := f.db.hit(op); err != nil {
		return err
	}
	if appID <= 0 {
		return model.ErrInvalidAppID
	}
	rows := f.db.appScopeRows(appID)
	if f.db.granted[appID] == nil {
		f.db.granted[appID] = map[string]bool{}
	}
	now := nowTS()
	for _, sc := range scopes {
		row, ok := rows[sc]
		if !ok {
			row = &model.AppScope{ID: f.db.next("appscope"), AppID: appID, Scope: sc, Ctime: now}
			rows[sc] = row
		}
		row.State = state
		row.Operator = operator
		row.Reason = reason
		row.Mtime = now
		f.db.granted[appID][sc] = state == model.AppScopeGranted
	}
	return nil
}

// Request 登记申请：已存在的行不得覆盖审批结论（真实现只更新 requested_by/mtime）。
func (f fakeAppScopes) Request(_ context.Context, appID int64, scopes []string, requestedBy int64) error {
	if err := f.db.hit("AppScopes.Request"); err != nil {
		return err
	}
	if appID <= 0 {
		return model.ErrInvalidAppID
	}
	rows := f.db.appScopeRows(appID)
	now := nowTS()
	for _, sc := range scopes {
		if row, ok := rows[sc]; ok {
			row.RequestedBy = requestedBy // 不覆盖 state：申请不推翻审批结论
			row.Mtime = now
			continue
		}
		rows[sc] = &model.AppScope{ID: f.db.next("appscope"), AppID: appID, Scope: sc,
			State: model.AppScopePending, RequestedBy: requestedBy, Ctime: now, Mtime: now}
	}
	return nil
}

func (f fakeAppScopes) Grant(_ context.Context, _ sqlx.Session, appID int64, scopes []string,
	operator int64, reason string) error {
	return f.transition("AppScopes.Grant", appID, scopes, operator, reason, model.AppScopeGranted)
}

func (f fakeAppScopes) Revoke(_ context.Context, _ sqlx.Session, appID int64, scopes []string,
	operator int64, reason string) error {
	return f.transition("AppScopes.Revoke", appID, scopes, operator, reason, model.AppScopeRevoked)
}

// ListByApp 含待审批与已回收行（ListScopes 的 granted_state 判定依赖这一点）。
func (f fakeAppScopes) ListByApp(_ context.Context, appID int64) (map[string]*model.AppScope, error) {
	if err := f.db.hit("AppScopes.ListByApp"); err != nil {
		return nil, err
	}
	out := map[string]*model.AppScope{}
	for sc, row := range f.db.appscopes[appID] {
		c := *row
		out[sc] = &c
	}
	return out, nil
}

// GrantedByApps 批量读侧：与 ListGranted 同一 granted 视图，一次取完（逐个 ListGranted 就是 N+1）。
func (f fakeAppScopes) GrantedByApps(_ context.Context, appIDs []int64) (map[int64][]string, error) {
	if err := f.db.hit("AppScopes.GrantedByApps"); err != nil {
		return nil, err
	}
	out := make(map[int64][]string, len(appIDs))
	for _, appID := range appIDs {
		var list []string
		for sc, g := range f.db.granted[appID] {
			if g {
				list = append(list, sc)
			}
		}
		sort.Strings(list)
		if len(list) > 0 {
			out[appID] = list
		}
	}
	return out, nil
}

func (f fakeAppScopes) FindGrantedScopes(_ context.Context, appID int64, candidates []string) ([]string, error) {
	if err := f.db.hit("AppScopes.FindGrantedScopes"); err != nil {
		return nil, err
	}
	granted := f.db.granted[appID]
	var out []string
	for _, c := range candidates {
		if granted[c] {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f fakeAppScopes) ListGranted(_ context.Context, appID int64) ([]string, error) {
	if err := f.db.hit("AppScopes.ListGranted"); err != nil {
		return nil, err
	}
	var out []string
	for sc, g := range f.db.granted[appID] {
		if g {
			out = append(out, sc)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ---------------------------------------------------------------- op_grant

type fakeGrants struct {
	model.GrantModel
	db *store
}

func (f fakeGrants) FindByID(_ context.Context, grantID int64) (*model.Grant, error) {
	if err := f.db.hit("Grants.FindByID"); err != nil {
		return nil, err
	}
	g, ok := f.db.grants[grantID]
	if !ok {
		return nil, nil
	}
	c := *g
	return &c, nil
}

func (f fakeGrants) FindByAppMid(_ context.Context, appID, mid int64) (*model.Grant, error) {
	if err := f.db.hit("Grants.FindByAppMid"); err != nil {
		return nil, err
	}
	if appID <= 0 || mid <= 0 {
		return nil, model.ErrInvalidAppID
	}
	for _, g := range f.db.grants {
		if g.AppID == appID && g.Mid == mid {
			c := *g
			return &c, nil
		}
	}
	return nil, nil
}

// ListByMid 复刻真实现的游标语义：只返回 revoked_at=0 的行，按 (mtime, grant_id) 倒序。
func (f fakeGrants) ListByMid(_ context.Context, mid int64, cursorTime, cursorID int64,
	ps int32) ([]*model.Grant, error) {
	if err := f.db.hit("Grants.ListByMid"); err != nil {
		return nil, err
	}
	if ps <= 0 {
		return nil, model.ErrInvalidPage
	}
	var rows []*model.Grant
	for _, g := range f.db.grants {
		if g.Mid == mid && g.RevokedAt == 0 {
			c := *g
			rows = append(rows, &c)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Mtime != rows[j].Mtime {
			return rows[i].Mtime > rows[j].Mtime
		}
		return rows[i].GrantID > rows[j].GrantID
	})
	if cursorID > 0 {
		var kept []*model.Grant
		for _, g := range rows {
			if g.Mtime < cursorTime || (g.Mtime == cursorTime && g.GrantID < cursorID) {
				kept = append(kept, g)
			}
		}
		rows = kept
	}
	if int32(len(rows)) > ps {
		rows = rows[:ps]
	}
	return rows, nil
}

func (f fakeGrants) MarkRevoked(_ context.Context, _ sqlx.Session, grantID, operator int64,
	reason string, now int64) (bool, error) {
	if err := f.db.hit("Grants.MarkRevoked"); err != nil {
		return false, err
	}
	g, ok := f.db.grants[grantID]
	if !ok {
		return false, nil
	}
	if g.RevokedAt != 0 {
		return false, nil // WHERE revoked_at = 0：重复撤销不生效（幂等锚点）
	}
	g.Status = model.GrantStatusRevoked
	g.RevokedAt = now
	g.RevokeReason = reason
	g.RevokeOperator = operator
	g.Mtime = now
	return true, nil
}

func (f fakeGrants) RevokeByMid(_ context.Context, _ sqlx.Session, mid, operator int64,
	reason string, now int64) (int64, error) {
	if err := f.db.hit("Grants.RevokeByMid"); err != nil {
		return 0, err
	}
	var n int64
	for _, g := range f.db.grants {
		if g.Mid != mid || g.RevokedAt != 0 {
			continue
		}
		g.Status = model.GrantStatusRevoked
		g.RevokedAt = now
		g.RevokeReason = reason
		g.RevokeOperator = operator
		g.Mtime = now
		n++
	}
	return n, nil
}

func (f fakeGrants) TouchTokenHead(_ context.Context, _ sqlx.Session, grantID, fromTokenID,
	toTokenID int64, now int64) (bool, error) {
	if err := f.db.hit("Grants.TouchTokenHead"); err != nil {
		return false, err
	}
	g, ok := f.db.grants[grantID]
	if !ok || g.CurrentTokenID != fromTokenID {
		return false, nil // CAS：链头已被并发的另一次签发前移
	}
	g.CurrentTokenID = toTokenID
	g.RotateSeq++
	g.Mtime = now
	return true, nil
}

// FindOrCreate 复刻 uniq (app_id, mid) 的 UPSERT 语义：
// 命中即覆盖 scope 快照、置为生效、清空撤销位点（重新授权必须能覆盖历史撤销），
// 且「服务端不接受隐式同意」这一护栏在 model 层也存在。
func (f fakeGrants) FindOrCreate(_ context.Context, _ sqlx.Session, appID, mid int64,
	scopes []string, consentGiven bool) (int64, bool, error) {
	if err := f.db.hit("Grants.FindOrCreate"); err != nil {
		return 0, false, err
	}
	if appID <= 0 {
		return 0, false, model.ErrInvalidAppID
	}
	if mid <= 0 || !consentGiven {
		return 0, false, model.ErrConsentRequired
	}
	now := nowTS()
	for _, g := range f.db.grants {
		if g.AppID != appID || g.Mid != mid {
			continue
		}
		g.Scope = model.JoinScopes(scopes)
		g.Status = model.GrantStatusActive
		g.ConsentGiven = 1
		g.ConsentAt = now
		g.RevokedAt = 0
		g.RevokeReason = ""
		g.RevokeOperator = 0
		g.Mtime = now
		return g.GrantID, false, nil
	}
	g := &model.Grant{GrantID: f.db.next("grant"), AppID: appID, Mid: mid,
		Scope: model.JoinScopes(scopes), Status: model.GrantStatusActive,
		ConsentGiven: 1, ConsentAt: now, Ctime: now, Mtime: now}
	f.db.grants[g.GrantID] = g
	return g.GrantID, true, nil
}

// listBy 共用游标分页：只返回 revoked_at=0 的行（运营侧口径与用户侧一致）。
func (f fakeGrants) listBy(match func(*model.Grant) bool, cursorTime, cursorID int64,
	ps int32) ([]*model.Grant, error) {
	if ps <= 0 {
		return nil, model.ErrInvalidPage
	}
	var rows []*model.Grant
	for _, g := range f.db.grants {
		if g.RevokedAt == 0 && match(g) {
			c := *g
			rows = append(rows, &c)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Mtime != rows[j].Mtime {
			return rows[i].Mtime > rows[j].Mtime
		}
		return rows[i].GrantID > rows[j].GrantID
	})
	if cursorTime > 0 {
		var kept []*model.Grant
		for _, g := range rows {
			if g.Mtime < cursorTime || (g.Mtime == cursorTime && g.GrantID < cursorID) {
				kept = append(kept, g)
			}
		}
		rows = kept
	}
	if int32(len(rows)) > ps {
		rows = rows[:ps]
	}
	return rows, nil
}

func (f fakeGrants) ListByApp(_ context.Context, appID int64, cursorTime, cursorID int64,
	ps int32) ([]*model.Grant, error) {
	if err := f.db.hit("Grants.ListByApp"); err != nil {
		return nil, err
	}
	if appID <= 0 {
		return nil, model.ErrInvalidAppID
	}
	return f.listBy(func(g *model.Grant) bool { return g.AppID == appID }, cursorTime, cursorID, ps)
}

func (f fakeGrants) CountActiveByApp(_ context.Context, appID int64) (int64, error) {
	if err := f.db.hit("Grants.CountActiveByApp"); err != nil {
		return 0, err
	}
	var n int64
	for _, g := range f.db.grants {
		if g.AppID == appID && g.RevokedAt == 0 {
			n++
		}
	}
	return n, nil
}

// RevokeByAppWithScopes 定点回收：只命中 scope 快照含被回收集合的行（FIND_IN_SET 口径），
// 且 revoked_at=0 才生效——集合化 UPDATE 而不是让 logic 分页遍历。
func (f fakeGrants) RevokeByAppWithScopes(_ context.Context, _ sqlx.Session, appID int64,
	scopes []string, operator int64, reason string, now int64) (int64, error) {
	if err := f.db.hit("Grants.RevokeByAppWithScopes"); err != nil {
		return 0, err
	}
	if appID <= 0 {
		return 0, model.ErrInvalidAppID
	}
	if len(scopes) == 0 {
		return 0, nil
	}
	var n int64
	for _, g := range f.db.grants {
		if g.AppID != appID || g.RevokedAt != 0 {
			continue
		}
		stored := model.SplitScopes(g.Scope)
		inScopeSet := false
		for _, s := range scopes {
			if model.ContainsScope(stored, s) {
				inScopeSet = true
				break
			}
		}
		if !inScopeSet {
			continue
		}
		g.Status = model.GrantStatusRevoked
		g.RevokedAt = now
		g.RevokeReason = reason
		g.RevokeOperator = operator
		g.Mtime = now
		n++
	}
	return n, nil
}

func (f fakeGrants) TouchLastCode(_ context.Context, _ sqlx.Session, grantID, codeID, now int64) error {
	if err := f.db.hit("Grants.TouchLastCode"); err != nil {
		return err
	}
	if grantID <= 0 || codeID <= 0 {
		return model.ErrInvalidAppID
	}
	if g, ok := f.db.grants[grantID]; ok {
		g.LastCodeID = codeID
		g.Mtime = now
	}
	return nil
}

func (f fakeGrants) ScopeOf(_ context.Context, grantID int64) ([]string, error) {
	if err := f.db.hit("Grants.ScopeOf"); err != nil {
		return nil, err
	}
	g, ok := f.db.grants[grantID]
	if !ok {
		return nil, nil
	}
	return model.SplitScopes(g.Scope), nil
}

// ---------------------------------------------------------------- op_token

type fakeTokens struct {
	model.TokenModel
	db *store
}

func (f fakeTokens) Insert(ctx context.Context, tok *model.Token) (int64, error) {
	return f.InsertTx(ctx, nil, tok)
}

func (f fakeTokens) InsertTx(_ context.Context, _ sqlx.Session, tok *model.Token) (int64, error) {
	if err := f.db.hit("Tokens.InsertTx"); err != nil {
		return 0, err
	}
	// 复刻 model 的哈希闸门：哈希为空意味着明文被透传到本层，必须当场炸。
	if tok == nil || tok.AccessHash == "" || tok.RefreshHash == "" {
		return 0, model.ErrTokenInvalid
	}
	for _, dup := range f.db.tokens {
		if dup.AccessHash == tok.AccessHash || dup.RefreshHash == tok.RefreshHash {
			return 0, model.ErrConcurrentUpdate // uniq_access_hash / uniq_refresh_hash 冲突
		}
	}
	id := f.db.next("token")
	now := nowTS()
	row := *tok
	row.TokenID = id
	if row.State == 0 {
		row.State = model.TokenStateActive
	}
	row.Ctime, row.Mtime = now, now
	f.db.tokens[id] = &row
	return id, nil
}

func (f fakeTokens) FindByID(_ context.Context, tokenID int64) (*model.Token, error) {
	if err := f.db.hit("Tokens.FindByID"); err != nil {
		return nil, err
	}
	tok, ok := f.db.tokens[tokenID]
	if !ok {
		return nil, nil
	}
	c := *tok
	return &c, nil
}

func (f fakeTokens) findByHash(op string, access bool, hash string) (*model.Token, error) {
	if err := f.db.hit(op); err != nil {
		return nil, err
	}
	if hash == "" {
		return nil, nil
	}
	for _, tok := range f.db.tokens {
		if (access && tok.AccessHash == hash) || (!access && tok.RefreshHash == hash) {
			c := *tok
			return &c, nil
		}
	}
	return nil, nil
}

func (f fakeTokens) FindByAccessHash(_ context.Context, hash string) (*model.Token, error) {
	return f.findByHash("Tokens.FindByAccessHash", true, hash)
}

func (f fakeTokens) FindByRefreshHash(_ context.Context, hash string) (*model.Token, error) {
	return f.findByHash("Tokens.FindByRefreshHash", false, hash)
}

func (f fakeTokens) FindAnyByHash(_ context.Context, hash string) (*model.Token, error) {
	if err := f.db.hit("Tokens.FindAnyByHash"); err != nil {
		return nil, err
	}
	if hash == "" {
		return nil, nil
	}
	for _, tok := range f.db.tokens {
		if tok.AccessHash == hash || tok.RefreshHash == hash {
			c := *tok
			return &c, nil
		}
	}
	return nil, nil
}

// markState 复刻「条件 UPDATE 的 rowsAffected>0」语义：allow 是 WHERE state IN (...) 集合。
func (f fakeTokens) markState(_ context.Context, key string, tokenID int64, to int32,
	reason string, allow []int32, now int64) (bool, error) {
	if err := f.db.hit(key); err != nil {
		return false, err
	}
	tok, ok := f.db.tokens[tokenID]
	if !ok {
		return false, nil
	}
	allowed := false
	for _, s := range allow {
		if tok.State == s {
			allowed = true
			break
		}
	}
	if !allowed {
		return false, nil
	}
	tok.State = to
	tok.Mtime = now
	switch to {
	case model.TokenStateRotated:
		tok.RotatedAt = now
		tok.RevokeReason = reason
	case model.TokenStateRevoked:
		tok.RevokedAt = now
		tok.RevokeReason = reason
	}
	return true, nil
}

func (f fakeTokens) MarkRotated(ctx context.Context, _ sqlx.Session, tokenID, now int64,
	reason string) (bool, error) {
	return f.markState(ctx, "Tokens.MarkRotated", tokenID, model.TokenStateRotated, reason,
		[]int32{model.TokenStateActive}, now)
}

func (f fakeTokens) MarkRevoked(ctx context.Context, _ sqlx.Session, tokenID, now int64,
	reason string) (bool, error) {
	return f.markState(ctx, "Tokens.MarkRevoked", tokenID, model.TokenStateRevoked, reason,
		[]int32{model.TokenStateActive, model.TokenStateRotated}, now)
}

func (f fakeTokens) revokeWhere(_ context.Context, key string, match func(*model.Token) bool,
	now int64, reason string) (int64, error) {
	if err := f.db.hit(key); err != nil {
		return 0, err
	}
	var n int64
	for _, tok := range f.db.tokens {
		if !match(tok) || (tok.State != model.TokenStateActive && tok.State != model.TokenStateRotated) {
			continue // 真实现同样是 UPDATE ... WHERE <cond> AND state IN (ACTIVE, ROTATED)
		}
		tok.State = model.TokenStateRevoked
		tok.RevokedAt = now
		tok.RevokeReason = reason
		tok.Mtime = now
		n++
	}
	return n, nil
}

func (f fakeTokens) RevokeByGrant(ctx context.Context, _ sqlx.Session, grantID, now int64,
	reason string) (int64, error) {
	if grantID <= 0 {
		return 0, model.ErrInvalidAppID
	}
	return f.revokeWhere(ctx, "Tokens.RevokeByGrant", func(tok *model.Token) bool {
		return tok.GrantID == grantID
	}, now, reason)
}

func (f fakeTokens) RevokeByMid(ctx context.Context, _ sqlx.Session, mid, now int64,
	reason string) (int64, error) {
	if mid <= 0 {
		return 0, model.ErrConsentRequired
	}
	return f.revokeWhere(ctx, "Tokens.RevokeByMid", func(tok *model.Token) bool {
		return tok.Mid == mid
	}, now, reason)
}

func (f fakeTokens) TouchUsed(_ context.Context, tokenID, ts int64) error {
	if err := f.db.hit("Tokens.TouchUsed"); err != nil {
		return err
	}
	if tok, ok := f.db.tokens[tokenID]; ok {
		tok.LastUsedAt = ts
	}
	return nil
}

func (f fakeTokens) RevokeByApp(ctx context.Context, _ sqlx.Session, appID, now int64,
	reason string) (int64, error) {
	if appID <= 0 {
		return 0, model.ErrInvalidAppID
	}
	return f.revokeWhere(ctx, "Tokens.RevokeByApp", func(tok *model.Token) bool {
		return tok.AppID == appID
	}, now, reason)
}

// RevokeByAppWithScopes 定点作废：只命中 scope 快照含被回收 scope 的行。
// 与 Grants.RevokeByAppWithScopes 同一集合化口径——回收的生效性不得随数据规模退化。
func (f fakeTokens) RevokeByAppWithScopes(ctx context.Context, _ sqlx.Session, appID int64,
	scopes []string, now int64, reason string) (int64, error) {
	if appID <= 0 {
		return 0, model.ErrInvalidAppID
	}
	if len(scopes) == 0 {
		return 0, nil
	}
	return f.revokeWhere(ctx, "Tokens.RevokeByAppWithScopes", func(tok *model.Token) bool {
		if tok.AppID != appID {
			return false
		}
		stored := model.SplitScopes(tok.Scope)
		for _, s := range scopes {
			if model.ContainsScope(stored, s) {
				return true
			}
		}
		return false
	}, now, reason)
}

// ListByGrant 签发链历史（倒序）：观测轮换链，链头在最前。
func (f fakeTokens) ListByGrant(_ context.Context, grantID int64, ps int32) ([]*model.Token, error) {
	if err := f.db.hit("Tokens.ListByGrant"); err != nil {
		return nil, err
	}
	if ps <= 0 {
		return nil, model.ErrInvalidPage
	}
	var rows []*model.Token
	for _, tok := range f.db.tokens {
		if tok.GrantID == grantID {
			c := *tok
			rows = append(rows, &c)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].TokenID > rows[j].TokenID })
	if int32(len(rows)) > ps {
		rows = rows[:ps]
	}
	return rows, nil
}

// ---------------------------------------------------------------- 配额

type fakeQuotaPolicies struct {
	model.QuotaPolicyModel
	db *store
}

// Upsert 复刻 uniq (app_id, api_code, window_seconds) 的 UPSERT：
// 命中即原地更新限额/启停/操作人/原因并保留 policy_id 与 ctime（created=false），
// 因此「policy_id=0 新建 vs 同三元组更新」在内存里同样可区分。
func (f fakeQuotaPolicies) Upsert(_ context.Context, p *model.QuotaPolicy) (int64, bool, error) {
	if err := f.db.hit("QuotaPolicies.Upsert"); err != nil {
		return 0, false, err
	}
	if p == nil || p.AppID < 0 {
		return 0, false, model.ErrInvalidAppID
	}
	if p.APICode == "" {
		return 0, false, model.ErrQuotaPolicyNotFound
	}
	if p.WindowSeconds <= 0 {
		return 0, false, model.ErrWindowInvalid
	}
	if p.Operator <= 0 {
		return 0, false, model.ErrOperatorRequired
	}
	// 商业化红线在 model 层再拦一次：绕过 logic 的直写也落不了库。
	if model.IsForbiddenScopeCategory(p.APICode) {
		return 0, false, model.ErrForbiddenScopeCategory
	}
	now := nowTS()
	for _, old := range f.db.policies {
		if old.AppID == p.AppID && old.APICode == p.APICode && old.WindowSeconds == p.WindowSeconds {
			old.QuotaLimit = p.QuotaLimit
			old.Enabled = p.Enabled
			old.Operator = p.Operator
			old.Reason = p.Reason
			old.Mtime = now
			return old.PolicyID, false, nil
		}
	}
	row := *p
	row.PolicyID = f.db.next("policy")
	row.Ctime, row.Mtime = now, now
	f.db.policies[row.PolicyID] = &row
	return row.PolicyID, true, nil
}

func (f fakeQuotaPolicies) FindByID(_ context.Context, policyID int64) (*model.QuotaPolicy, error) {
	if err := f.db.hit("QuotaPolicies.FindByID"); err != nil {
		return nil, err
	}
	p, ok := f.db.policies[policyID]
	if !ok {
		return nil, nil
	}
	c := *p
	return &c, nil
}

// ListByApp 运营侧分页：app_id 精确匹配（0 就是全局层，不是「不过滤」），
// api_code 为空表示不过滤；游标 (mtime, policy_id) 倒序。
func (f fakeQuotaPolicies) ListByApp(_ context.Context, appID int64, apiCode string,
	cursorTime, cursorID int64, ps int32) ([]*model.QuotaPolicy, error) {
	if err := f.db.hit("QuotaPolicies.ListByApp"); err != nil {
		return nil, err
	}
	if ps <= 0 {
		return nil, model.ErrInvalidPage
	}
	var rows []*model.QuotaPolicy
	for _, p := range f.db.policies {
		if p.AppID != appID || (apiCode != "" && p.APICode != apiCode) {
			continue
		}
		c := *p
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Mtime != rows[j].Mtime {
			return rows[i].Mtime > rows[j].Mtime
		}
		return rows[i].PolicyID > rows[j].PolicyID
	})
	if cursorTime > 0 {
		var kept []*model.QuotaPolicy
		for _, p := range rows {
			if p.Mtime < cursorTime || (p.Mtime == cursorTime && p.PolicyID < cursorID) {
				kept = append(kept, p)
			}
		}
		rows = kept
	}
	if int32(len(rows)) > ps {
		rows = rows[:ps]
	}
	return rows, nil
}

func (f fakeQuotaPolicies) Disable(_ context.Context, policyID, operator int64,
	reason string) (bool, error) {
	if err := f.db.hit("QuotaPolicies.Disable"); err != nil {
		return false, err
	}
	if policyID <= 0 {
		return false, model.ErrQuotaPolicyNotFound
	}
	if operator <= 0 {
		return false, model.ErrOperatorRequired
	}
	p, ok := f.db.policies[policyID]
	if !ok || p.Enabled != 1 {
		return false, nil // WHERE enabled = 1：重复停用 0 行
	}
	p.Enabled = 0
	p.Operator = operator
	p.Reason = reason
	p.Mtime = nowTS()
	return true, nil
}

// ListCandidates 返回四象限候选，生效层级的收窄判定留给 logic 侧的 quota.go。
func (f fakeQuotaPolicies) ListCandidates(_ context.Context, appID int64,
	apiCode string) ([]*model.QuotaPolicy, error) {
	if err := f.db.hit("QuotaPolicies.ListCandidates"); err != nil {
		return nil, err
	}
	if apiCode == "" {
		return nil, model.ErrQuotaPolicyNotFound
	}
	var out []*model.QuotaPolicy
	for _, p := range f.db.policies {
		if p.Enabled != 1 {
			continue // 真实现 WHERE enabled = 1
		}
		appOK := p.AppID == appID || p.AppID == model.GlobalAppID
		apiOK := p.APICode == apiCode || p.APICode == model.AnyAPICode
		if appOK && apiOK {
			c := *p
			out = append(out, &c)
		}
	}
	return out, nil
}

type fakeQuotaUsages struct {
	model.QuotaUsageModel
	db *store
}

func usageKey(appID int64, apiCode string, windowSeconds, windowStart int64) string {
	return apiCode + "|" + strconv.FormatInt(appID, 10) + "|" +
		strconv.FormatInt(windowSeconds, 10) + "|" + strconv.FormatInt(windowStart, 10)
}

func (f fakeQuotaUsages) Add(_ context.Context, appID int64, apiCode string, windowSeconds,
	windowStart, delta, limitSnapshot int64) (*model.QuotaUsage, error) {
	if err := f.db.hit("QuotaUsages.Add"); err != nil {
		return nil, err
	}
	if windowSeconds <= 0 {
		return nil, model.ErrWindowInvalid
	}
	if delta <= 0 {
		delta = 1
	}
	key := usageKey(appID, apiCode, windowSeconds, windowStart)
	row, ok := f.db.usage[key]
	if !ok {
		row = &model.QuotaUsage{
			UsageID: f.db.next("usage"), AppID: appID, APICode: apiCode,
			WindowSeconds: windowSeconds, WindowStart: windowStart,
			WindowEnd: windowStart + windowSeconds, Ctime: nowTS(),
		}
		f.db.usage[key] = row
	}
	row.Used += delta
	row.LimitSnapshot = limitSnapshot
	row.UpdatedAt = nowTS()
	c := *row
	return &c, nil
}

func (f fakeQuotaUsages) Peek(_ context.Context, appID int64, apiCode string, windowSeconds,
	windowStart int64) (*model.QuotaUsage, error) {
	if err := f.db.hit("QuotaUsages.Peek"); err != nil {
		return nil, err
	}
	row, ok := f.db.usage[usageKey(appID, apiCode, windowSeconds, windowStart)]
	if !ok {
		return nil, nil
	}
	c := *row
	return &c, nil
}

// ListByApp windowStart=0 表示「不限窗口起点」（真实现同样只在 >0 时加条件）。
func (f fakeQuotaUsages) ListByApp(_ context.Context, appID int64, apiCode string,
	windowStart int64) ([]*model.QuotaUsage, error) {
	if err := f.db.hit("QuotaUsages.ListByApp"); err != nil {
		return nil, err
	}
	var rows []*model.QuotaUsage
	for _, u := range f.db.usage {
		if u.AppID != appID || (apiCode != "" && u.APICode != apiCode) ||
			(windowStart > 0 && u.WindowStart != windowStart) {
			continue
		}
		c := *u
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].WindowStart != rows[j].WindowStart {
			return rows[i].WindowStart > rows[j].WindowStart
		}
		if rows[i].APICode != rows[j].APICode {
			return rows[i].APICode < rows[j].APICode
		}
		return rows[i].WindowSeconds < rows[j].WindowSeconds
	})
	return rows, nil
}

// ListWindowsInRange 列出 [from,to) 内已存在的窗口行：重算靠它找「流水有记录但投影缺失」的窗口。
func (f fakeQuotaUsages) ListWindowsInRange(_ context.Context, appID int64, apiCode string,
	windowSeconds, from, to int64) ([]*model.QuotaUsage, error) {
	if err := f.db.hit("QuotaUsages.ListWindowsInRange"); err != nil {
		return nil, err
	}
	if windowSeconds <= 0 || to <= from {
		return nil, model.ErrWindowInvalid
	}
	var rows []*model.QuotaUsage
	for _, u := range f.db.usage {
		if u.WindowSeconds != windowSeconds || u.WindowStart < from || u.WindowStart >= to {
			continue
		}
		if (appID > 0 && u.AppID != appID) || (apiCode != "" && u.APICode != apiCode) {
			continue
		}
		c := *u
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].WindowStart < rows[j].WindowStart })
	return rows, nil
}

// RecomputeOverwrite 用流水真值覆盖窗口计数并回传修正量（新值-旧值，可为负）：
// 「dry_run 位必须真的是 dry_run」就靠这个增量与行数变化来证明。
func (f fakeQuotaUsages) RecomputeOverwrite(_ context.Context, appID int64, apiCode string,
	windowSeconds, windowStart, used, limitSnapshot int64) (int64, error) {
	if err := f.db.hit("QuotaUsages.RecomputeOverwrite"); err != nil {
		return 0, err
	}
	if windowSeconds <= 0 {
		return 0, model.ErrWindowInvalid
	}
	if used < 0 {
		used = 0
	}
	key := usageKey(appID, apiCode, windowSeconds, windowStart)
	row, ok := f.db.usage[key]
	if !ok {
		row = &model.QuotaUsage{
			UsageID: f.db.next("usage"), AppID: appID, APICode: apiCode,
			WindowSeconds: windowSeconds, WindowStart: windowStart,
			WindowEnd: windowStart + windowSeconds, Ctime: nowTS(),
		}
		f.db.usage[key] = row
	}
	old := row.Used
	row.Used = used
	row.LimitSnapshot = limitSnapshot
	row.UpdatedAt = nowTS()
	return used - old, nil
}

// ---------------------------------------------------------------- 调用流水

type fakeCallLogs struct {
	model.ApiCallLogModel
	db *store
}

func (f fakeCallLogs) Insert(_ context.Context, l *model.ApiCallLog) (int64, bool, error) {
	if err := f.db.hit("CallLogs.Insert"); err != nil {
		return 0, false, err
	}
	// 复刻 model 的必填校验：request_id 是唯一键，app_id NOT NULL 且必须 >0。
	if l == nil || l.RequestID == "" || l.AppID <= 0 ||
		(l.Allowed != model.CallAllowed && l.Allowed != model.CallDenied) {
		return 0, false, model.ErrInvalidAppID
	}
	if row, ok := f.db.logs[l.RequestID]; ok {
		return row.CallLogID, false, nil // uniq_request_id 命中 → 幂等重放
	}
	id := f.db.next("calllog")
	row := *l
	row.CallLogID = id
	if row.Ctime == 0 {
		row.Ctime = nowTS()
	}
	f.db.logs[row.RequestID] = &row
	return id, true, nil
}

func (f fakeCallLogs) FindByRequestID(_ context.Context, requestID string) (*model.ApiCallLog, error) {
	if err := f.db.hit("CallLogs.FindByRequestID"); err != nil {
		return nil, err
	}
	row, ok := f.db.logs[requestID]
	if !ok {
		return nil, nil
	}
	c := *row
	return &c, nil
}

// ListByCursor 观测分页：cursor 为 (ctime, call_log_id) 倒序位点，ps 严格生效。
func (f fakeCallLogs) ListByCursor(_ context.Context, appID int64, apiCode string, onlyDenied bool,
	cursorTime, cursorID int64, ps int32) ([]*model.ApiCallLog, error) {
	if err := f.db.hit("CallLogs.ListByCursor"); err != nil {
		return nil, err
	}
	if ps <= 0 {
		return nil, model.ErrInvalidPage
	}
	var rows []*model.ApiCallLog
	for _, l := range f.db.logs {
		if (appID > 0 && l.AppID != appID) || (apiCode != "" && l.APICode != apiCode) ||
			(onlyDenied && l.Allowed != model.CallDenied) {
			continue
		}
		c := *l
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Ctime != rows[j].Ctime {
			return rows[i].Ctime > rows[j].Ctime
		}
		return rows[i].CallLogID > rows[j].CallLogID
	})
	if cursorTime > 0 {
		var kept []*model.ApiCallLog
		for _, l := range rows {
			if l.Ctime < cursorTime || (l.Ctime == cursorTime && l.CallLogID < cursorID) {
				kept = append(kept, l)
			}
		}
		rows = kept
	}
	if int32(len(rows)) > ps {
		rows = rows[:ps]
	}
	return rows, nil
}

// CountByWindow [from,to) 流水条数；api_code 为空或 "*" 都是「全接口」。
func (f fakeCallLogs) CountByWindow(_ context.Context, appID int64, apiCode string,
	from, to int64) (int64, error) {
	if err := f.db.hit("CallLogs.CountByWindow"); err != nil {
		return 0, err
	}
	if to <= from {
		return 0, model.ErrWindowInvalid
	}
	var n int64
	for _, l := range f.db.logs {
		if l.Ctime < from || l.Ctime >= to {
			continue
		}
		if (appID > 0 && l.AppID != appID) ||
			(apiCode != "" && apiCode != model.AnyAPICode && l.APICode != apiCode) {
			continue
		}
		n++
	}
	return n, nil
}

// ListWindowTotals 按 AlignWindow 同一取齐规则分组统计流水：
// 重算的「对账真值」就是这里，与 logic 的窗口边界算法必须一致，否则重算永远收敛不了。
func (f fakeCallLogs) ListWindowTotals(_ context.Context, appID int64, apiCode string,
	windowSeconds, from, to int64) ([]*model.QuotaWindowTotal, error) {
	if err := f.db.hit("CallLogs.ListWindowTotals"); err != nil {
		return nil, err
	}
	if windowSeconds <= 0 || to <= from {
		return nil, model.ErrWindowInvalid
	}
	totals := map[int64]int64{}
	for _, l := range f.db.logs {
		if l.Ctime < from || l.Ctime >= to {
			continue
		}
		if (appID > 0 && l.AppID != appID) ||
			(apiCode != "" && apiCode != model.AnyAPICode && l.APICode != apiCode) {
			continue
		}
		w := l.Ctime / windowSeconds * windowSeconds
		totals[w]++
	}
	keys := make([]int64, 0, len(totals))
	for w := range totals {
		keys = append(keys, w)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]*model.QuotaWindowTotal, 0, len(keys))
	for _, w := range keys {
		out = append(out, &model.QuotaWindowTotal{WindowStart: w, Total: totals[w]})
	}
	return out, nil
}

// ---------------------------------------------------------------- webhook

type fakeWebhookEndpoints struct {
	model.WebhookEndpointModel
	db *store
}

func (f fakeWebhookEndpoints) ListMatching(_ context.Context, appID int64,
	eventType int32) ([]*model.WebhookEndpoint, error) {
	if err := f.db.hit("WebhookEndpoints.ListMatching"); err != nil {
		return nil, err
	}
	var out []*model.WebhookEndpoint
	for _, ep := range f.db.eps {
		if ep.AppID != appID || ep.EventType != eventType || !ep.Deliverable() {
			continue // 真实现按 verified/enabled/deleted 过滤
		}
		c := *ep
		out = append(out, &c)
	}
	return out, nil
}

// Insert 复刻 uniq (app_id, event_type, callback_url)：重复注册回既有 ID 且 created=false，
// 连同软删行一起命中——否则「删了再注册同一个地址」会绕过抑制语义产生第二个端点。
func (f fakeWebhookEndpoints) Insert(_ context.Context, e *model.WebhookEndpoint) (int64, bool, error) {
	if err := f.db.hit("WebhookEndpoints.Insert"); err != nil {
		return 0, false, err
	}
	if e == nil || e.AppID <= 0 {
		return 0, false, model.ErrInvalidAppID
	}
	if !model.ValidWebhookEventType(e.EventType) {
		return 0, false, model.ErrInvalidEventType
	}
	if strings.TrimSpace(e.URL) == "" {
		return 0, false, model.ErrInvalidWebhookURL
	}
	for _, old := range f.db.eps {
		if old.AppID == e.AppID && old.EventType == e.EventType && old.URL == e.URL {
			return old.EndpointID, false, nil
		}
	}
	now := nowTS()
	row := *e
	row.EndpointID = f.db.next("endpoint")
	if row.SignKeyVersion <= 0 {
		row.SignKeyVersion = 1
	}
	row.Enabled = 1
	row.VerifiedAt = 0 // 新端点一律未验证：challenge 通过前不可投递
	row.DeletedAt = 0
	row.Ctime, row.Mtime = now, now
	f.db.eps = append(f.db.eps, &row)
	return row.EndpointID, true, nil
}

// FindByID 含软删行（投递记录要能解释归属）。
func (f fakeWebhookEndpoints) FindByID(_ context.Context, endpointID int64) (*model.WebhookEndpoint, error) {
	if err := f.db.hit("WebhookEndpoints.FindByID"); err != nil {
		return nil, err
	}
	for _, ep := range f.db.eps {
		if ep.EndpointID == endpointID {
			c := *ep
			return &c, nil
		}
	}
	return nil, nil
}

// ListByApp includeDisabled=false 时只回未删除的启用端点；排序 (event_type, endpoint_id) 升序。
func (f fakeWebhookEndpoints) ListByApp(_ context.Context, appID int64,
	includeDisabled bool) ([]*model.WebhookEndpoint, error) {
	if err := f.db.hit("WebhookEndpoints.ListByApp"); err != nil {
		return nil, err
	}
	if appID <= 0 {
		return nil, model.ErrInvalidAppID
	}
	var out []*model.WebhookEndpoint
	for _, ep := range f.db.eps {
		if ep.AppID != appID || ep.DeletedAt != 0 {
			continue
		}
		if !includeDisabled && ep.Enabled != 1 {
			continue
		}
		c := *ep
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].EventType != out[j].EventType {
			return out[i].EventType < out[j].EventType
		}
		return out[i].EndpointID < out[j].EndpointID
	})
	return out, nil
}

// CountByApp 不含软删行：配置上限统计的是「还在收事件的端点数」。
func (f fakeWebhookEndpoints) CountByApp(_ context.Context, appID int64) (int64, error) {
	if err := f.db.hit("WebhookEndpoints.CountByApp"); err != nil {
		return 0, err
	}
	var n int64
	for _, ep := range f.db.eps {
		if ep.AppID == appID && ep.DeletedAt == 0 {
			n++
		}
	}
	return n, nil
}

// MarkVerified CAS：仅当 verified_at=0 且未删除，幂等。
func (f fakeWebhookEndpoints) MarkVerified(_ context.Context, endpointID, now int64) (bool, error) {
	if err := f.db.hit("WebhookEndpoints.MarkVerified"); err != nil {
		return false, err
	}
	if endpointID <= 0 {
		return false, model.ErrWebhookNotFound
	}
	for _, ep := range f.db.eps {
		if ep.EndpointID != endpointID || ep.VerifiedAt != 0 || ep.DeletedAt != 0 {
			continue
		}
		ep.VerifiedAt = now
		ep.ChallengeHash = ""
		ep.Mtime = now
		return true, nil
	}
	return false, nil
}

func (f fakeWebhookEndpoints) SetEnabled(_ context.Context, endpointID int64, enabled int8,
	operator int64) (bool, error) {
	if err := f.db.hit("WebhookEndpoints.SetEnabled"); err != nil {
		return false, err
	}
	if endpointID <= 0 {
		return false, model.ErrWebhookNotFound
	}
	if operator <= 0 {
		return false, model.ErrOperatorRequired
	}
	for _, ep := range f.db.eps {
		if ep.EndpointID != endpointID || ep.DeletedAt != 0 {
			continue
		}
		ep.Enabled = enabled
		ep.Mtime = nowTS()
		return true, nil
	}
	return false, nil
}

// SoftDelete CAS WHERE deleted_at=0：第二次 deleted=false（幂等），但抑制仍会执行。
func (f fakeWebhookEndpoints) SoftDelete(_ context.Context, _ sqlx.Session, endpointID int64,
	operator int64, reason string, now int64) (bool, error) {
	if err := f.db.hit("WebhookEndpoints.SoftDelete"); err != nil {
		return false, err
	}
	if endpointID <= 0 {
		return false, model.ErrWebhookNotFound
	}
	if operator <= 0 {
		return false, model.ErrOperatorRequired
	}
	for _, ep := range f.db.eps {
		if ep.EndpointID != endpointID || ep.DeletedAt != 0 {
			continue
		}
		ep.Enabled = 0
		ep.DeletedAt = now
		ep.DeleteReason = reason
		ep.Mtime = now
		return true, nil
	}
	return false, nil
}

type fakeWebhookDeliveries struct {
	model.WebhookDeliveryModel
	db *store
}

func (f fakeWebhookDeliveries) Insert(_ context.Context, d *model.WebhookDelivery) (int64, bool, error) {
	if err := f.db.hit("WebhookDeliveries.Insert"); err != nil {
		return 0, false, err
	}
	for _, old := range f.db.dels {
		if old.EventID == d.EventID && old.EndpointID == d.EndpointID {
			return old.DeliveryID, false, nil // uniq_event_endpoint 去重
		}
	}
	id := f.db.next("delivery")
	row := *d
	row.DeliveryID = id
	row.Ctime = nowTS()
	f.db.dels = append(f.db.dels, &row)
	return id, true, nil
}

func (f fakeWebhookDeliveries) find(op string, deliveryID int64) (*model.WebhookDelivery, error) {
	if err := f.db.hit(op); err != nil {
		return nil, err
	}
	for _, d := range f.db.dels {
		if d.DeliveryID == deliveryID {
			c := *d
			return &c, nil
		}
	}
	return nil, nil
}

func (f fakeWebhookDeliveries) FindByID(_ context.Context, deliveryID int64) (*model.WebhookDelivery, error) {
	return f.find("WebhookDeliveries.FindByID", deliveryID)
}

// ListByEventID 同一事件的全部任务：matched_endpoints 与 deduplicated 都据此判定。
func (f fakeWebhookDeliveries) ListByEventID(_ context.Context, eventID string) ([]*model.WebhookDelivery, error) {
	if err := f.db.hit("WebhookDeliveries.ListByEventID"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(eventID) == "" {
		return nil, model.ErrEventIDRequired
	}
	var out []*model.WebhookDelivery
	for _, d := range f.db.dels {
		if d.EventID == eventID {
			c := *d
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EndpointID < out[j].EndpointID })
	return out, nil
}

// ResetForReplay CAS WHERE state IN (DEAD, IGNORED)：attempt 归零、重回 pending。
// 命中不了就 applied=false（例如 PENDING 行不可人工重放），这是「重放不制造新事件」的锚点。
func (f fakeWebhookDeliveries) ResetForReplay(_ context.Context, deliveryID, operator int64,
	reason string, now int64) (bool, error) {
	if err := f.db.hit("WebhookDeliveries.ResetForReplay"); err != nil {
		return false, err
	}
	if deliveryID <= 0 {
		return false, model.ErrDeliveryNotFound
	}
	if operator <= 0 {
		return false, model.ErrOperatorRequired
	}
	for _, d := range f.db.dels {
		if d.DeliveryID != deliveryID {
			continue
		}
		if d.State != model.DeliveryStateDead && d.State != model.DeliveryStateIgnored {
			return false, nil
		}
		d.State = model.DeliveryStatePending
		d.Attempt = 0
		d.NextRetryAt = now
		d.LastError = strings.TrimSpace(reason)
		d.LeaseUntil = 0
		d.Mtime = now
		return true, nil
	}
	return false, nil
}

// SuppressByEndpoint pending/delivering/retry_scheduled → ignored（终态），
// 已成功的行不得被动过——「删端点不影响历史结论」是审计要求。
func (f fakeWebhookDeliveries) SuppressByEndpoint(_ context.Context, _ sqlx.Session, endpointID int64,
	reason string, now int64) (int64, error) {
	if err := f.db.hit("WebhookDeliveries.SuppressByEndpoint"); err != nil {
		return 0, err
	}
	if endpointID <= 0 {
		return 0, model.ErrWebhookNotFound
	}
	var n int64
	for _, d := range f.db.dels {
		if d.EndpointID != endpointID {
			continue
		}
		switch d.State {
		case model.DeliveryStatePending, model.DeliveryStateDelivering, model.DeliveryStateRetryScheduled:
			d.State = model.DeliveryStateIgnored
			d.NextRetryAt = now
			d.LastError = reason
			d.LeaseUntil = 0
			d.Mtime = now
			n++
		}
	}
	return n, nil
}

// ListByCursor 与真实现同一条 SELECT：payload 列不取。
// 内存里存的正文保留（重放要用），投影出来的行必须没有正文，否则「响应不回显正文」这条
// 断言就成了自证——fake 先替真库把列裁掉，测试才有意义。
func (f fakeWebhookDeliveries) ListByCursor(_ context.Context, appID, endpointID int64, state int32,
	onlyFailed bool, cursorTime, cursorID int64, ps int32) ([]*model.WebhookDelivery, error) {
	if err := f.db.hit("WebhookDeliveries.ListByCursor"); err != nil {
		return nil, err
	}
	if ps <= 0 {
		return nil, model.ErrInvalidPage
	}
	var rows []*model.WebhookDelivery
	for _, d := range f.db.dels {
		if (appID > 0 && d.AppID != appID) || (endpointID > 0 && d.EndpointID != endpointID) ||
			(state > 0 && d.State != state) {
			continue
		}
		if onlyFailed && d.State != model.DeliveryStateDead && d.State != model.DeliveryStateRetryScheduled {
			continue
		}
		c := *d
		c.Payload = ""
		rows = append(rows, &c)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Ctime != rows[j].Ctime {
			return rows[i].Ctime > rows[j].Ctime
		}
		return rows[i].DeliveryID > rows[j].DeliveryID
	})
	if cursorTime > 0 {
		var kept []*model.WebhookDelivery
		for _, d := range rows {
			if d.Ctime < cursorTime || (d.Ctime == cursorTime && d.DeliveryID < cursorID) {
				kept = append(kept, d)
			}
		}
		rows = kept
	}
	if int32(len(rows)) > ps {
		rows = rows[:ps]
	}
	return rows, nil
}

// fakeAuthCodes 授权码表：复刻 uniq_hash 与 used_at=0 的 CAS——
// 这两条正是「一次性消费」在库侧的全部锚点，logic 的裁决必须建在它们之上。
type fakeAuthCodes struct {
	model.AuthCodeModel
	db *store
}

func (f fakeAuthCodes) Insert(ctx context.Context, a *model.AuthCode) (int64, bool, error) {
	return f.insert(ctx, a)
}

func (f fakeAuthCodes) InsertTx(ctx context.Context, _ sqlx.Session,
	a *model.AuthCode) (int64, bool, error) {
	return f.insert(ctx, a)
}

func (f fakeAuthCodes) insert(_ context.Context, a *model.AuthCode) (int64, bool, error) {
	if err := f.db.hit("AuthCodes.InsertTx"); err != nil {
		return 0, false, err
	}
	// 与真实现同一入参闸门：哈希/盐为空意味着明文被透传到本层。
	if a == nil || a.AppID <= 0 {
		return 0, false, model.ErrInvalidAppID
	}
	if a.Mid <= 0 {
		return 0, false, model.ErrConsentRequired
	}
	if a.Hash == "" || a.Salt == "" {
		return 0, false, model.ErrAuthCodeInvalid
	}
	for _, old := range f.db.codes {
		if old.Hash == a.Hash {
			return old.CodeID, false, nil // uniq_hash：不返回旧行内容语义，只报未创建
		}
	}
	id := f.db.next("code")
	now := nowTS()
	row := *a
	row.CodeID = id
	row.UsedAt = 0
	row.ConsumedByTokenID = 0
	row.Ctime, row.Mtime = now, now
	f.db.codes[id] = &row
	return id, true, nil
}

func (f fakeAuthCodes) FindByID(_ context.Context, codeID int64) (*model.AuthCode, error) {
	if err := f.db.hit("AuthCodes.FindByID"); err != nil {
		return nil, err
	}
	row, ok := f.db.codes[codeID]
	if !ok {
		return nil, nil
	}
	c := *row
	return &c, nil
}

func (f fakeAuthCodes) FindByHash(_ context.Context, hash string) (*model.AuthCode, error) {
	if err := f.db.hit("AuthCodes.FindByHash"); err != nil {
		return nil, err
	}
	if hash == "" {
		return nil, model.ErrAuthCodeInvalid
	}
	for _, row := range f.db.codes {
		if row.Hash == hash {
			c := *row
			return &c, nil
		}
	}
	return nil, nil
}

// MarkUsed CAS：WHERE code_id=? AND used_at=0。第二次调用 applied=false，即重放。
func (f fakeAuthCodes) MarkUsed(_ context.Context, _ sqlx.Session, codeID, tokenID,
	now int64) (bool, error) {
	if err := f.db.hit("AuthCodes.MarkUsed"); err != nil {
		return false, err
	}
	if codeID <= 0 {
		return false, model.ErrAuthCodeInvalid
	}
	row, ok := f.db.codes[codeID]
	if !ok || row.UsedAt != 0 {
		return false, nil
	}
	row.UsedAt = now
	row.ConsumedByTokenID = tokenID
	row.Mtime = now
	return true, nil
}

// IncrReplay 错误路径也留痕：重放尝试必须计入，且不影响 used_at 位点。
func (f fakeAuthCodes) IncrReplay(_ context.Context, codeID int64) error {
	if err := f.db.hit("AuthCodes.IncrReplay"); err != nil {
		return err
	}
	if row, ok := f.db.codes[codeID]; ok {
		row.ReplayCount++
		row.Mtime = nowTS()
	}
	return nil
}

func (f fakeAuthCodes) CountRecentByMid(_ context.Context, mid, since int64) (int64, error) {
	if err := f.db.hit("AuthCodes.CountRecentByMid"); err != nil {
		return 0, err
	}
	var n int64
	for _, row := range f.db.codes {
		if row.Mid == mid && row.Ctime >= since {
			n++
		}
	}
	return n, nil
}

// ---------------------------------------------------------------- 组装

// newTestSvc 装配一个只依赖内存的 ServiceContext：
// Cache 恒为 nil（判定链设计上不依赖缓存），WriteLimiter 用真实令牌桶（额度给足）。
func newTestSvc(db *store) *svc.ServiceContext {
	cfg := config.Config{}
	cfg.Security.CredentialPepper = testPepper
	cfg.Security.WebhookMasterPepper = testWebhookPepper
	cfg.Security.KeyVersion = 1
	cfg.Security.SignatureSkewSeconds = 300
	cfg.Security.NonceTTLSeconds = 600
	cfg.OpenPlatform = config.OpenPlatformConf{
		AuthCodeTTLSeconds:      60,
		AccessTokenTTLSeconds:   3600,
		RefreshTokenTTLSeconds:  2592000,
		PageSize:                20,
		MaxPageSize:             50,
		WebhookMaxAttempts:      6,
		WebhookRetryBaseSeconds: 30,
		WebhookRetryMaxSeconds:  3600,
		WebhookLeaseSeconds:     60,
		WebhookPayloadMaxBytes:  32768,
		IntrospectCacheSeconds:  0, // 关闭定位缓存：判定必须完全走 DB 真值
	}
	return &svc.ServiceContext{
		Config:            cfg,
		DB:                fakeConn{db: db},
		Apps:              fakeApps{db: db},
		Secrets:           fakeSecrets{db: db},
		Scopes:            fakeScopes{db: db},
		AppScopes:         fakeAppScopes{db: db},
		AuthCodes:         fakeAuthCodes{db: db},
		Grants:            fakeGrants{db: db},
		Tokens:            fakeTokens{db: db},
		QuotaPolicies:     fakeQuotaPolicies{db: db},
		QuotaUsages:       fakeQuotaUsages{db: db},
		CallLogs:          fakeCallLogs{db: db},
		WebhookEndpoints:  fakeWebhookEndpoints{db: db},
		WebhookDeliveries: fakeWebhookDeliveries{db: db},
		WriteLimiter:      ratelimit.NewTokenBucket(10000, 10000),
	}
}

// limitedSvc 把写令牌桶调成 0 容量：验证「限流发生在写库之前」（写路径零副作用）。
func limitedSvc(db *store) *svc.ServiceContext {
	s := newTestSvc(db)
	s.WriteLimiter = ratelimit.NewTokenBucket(0, 0)
	return s
}

// ---------------------------------------------------------------- 种子数据

// seedApp 建一个 ACTIVE 应用与其已获批 scope 集。
// 获批集同时写进审批关系行（state=GRANTED）：ListScopes 的 granted_state 与
// GrantApplicationScopes 的「重叠即拒绝」判定读的是关系行，只写 granted 视图会让这两条
// 断言看不到真值。
func seedApp(db *store, appID, ownerMid int64, scopes ...string) *model.Application {
	now := nowTS()
	app := &model.Application{
		AppID: appID, AppKey: "opk_" + strconv.FormatInt(appID, 10), Name: "t", OwnerMid: ownerMid,
		Status: model.AppStatusActive, Version: 1, Ctime: now, Mtime: now,
	}
	db.apps[appID] = app
	if len(scopes) > 0 {
		db.granted[appID] = map[string]bool{}
		for _, sc := range scopes {
			db.granted[appID][sc] = true
			db.appScopeRows(appID)[sc] = &model.AppScope{
				ID: db.next("appscope"), AppID: appID, Scope: sc,
				State: model.AppScopeGranted, Operator: 1, Ctime: now, Mtime: now,
			}
		}
	}
	return app
}

// seedSecret 落一把已知明文的密钥（哈希口径与 model 一致：HMAC(pepper, salt||明文)）。
func seedSecret(t *testing.T, db *store, appID int64, plain string) *model.AppSecret {
	t.Helper()
	hash, err := model.HashSecret(testPepper, "salt0", plain)
	wantOK(t, hash, err, "HashSecret")
	now := nowTS()
	id := db.next("secret")
	row := &model.AppSecret{SecretID: id, AppID: appID, Salt: "salt0", Hash: hash,
		Status: model.SecretStatusActive, Ctime: now, Mtime: now}
	db.secrets[id] = row
	return row
}

// seedScopeDir 权限点目录：access 决定是否需要用户同意。
func seedScopeDir(db *store, scope string, access int32) {
	db.scopes[scope] = &model.Scope{Scope: scope, DisplayName: scope, Access: access,
		RiskLevel: model.ScopeRiskLow, Enabled: 1}
}

// seedGrant 建一条授权关系。
func seedGrant(db *store, appID, mid int64, scopes []string, consent int8) *model.Grant {
	now := nowTS()
	g := &model.Grant{
		GrantID: db.next("grant"), AppID: appID, Mid: mid, Scope: model.JoinScopes(scopes),
		Status: model.GrantStatusActive, ConsentGiven: consent, ConsentAt: now,
		Ctime: now - 100, Mtime: now - 100,
	}
	db.grants[g.GrantID] = g
	return g
}

// seedToken 在 grant 下签一代 token（哈希按 logic 同一算式算），返回 access/refresh 明文。
func seedToken(t *testing.T, db *store, grant *model.Grant, scopes []string,
	ageOffset, accessLeft, refreshLeft int64) (string, string, *model.Token) {
	t.Helper()
	access, refresh := mustPlaintext(t), mustPlaintext(t)
	ah, err := model.HashCredential(testPepper, access)
	wantOK(t, ah, err, "HashCredential(access)")
	rh, err := model.HashCredential(testPepper, refresh)
	wantOK(t, rh, err, "HashCredential(refresh)")

	now := nowTS()
	id := db.next("token")
	tok := &model.Token{
		TokenID: id, GrantID: grant.GrantID, AppID: grant.AppID, Mid: grant.Mid,
		GrantType:  model.GrantTypeAuthorizationCode,
		AccessSalt: "s", AccessHash: ah, RefreshSalt: "s", RefreshHash: rh,
		Scope:            model.JoinScopes(scopes),
		AccessExpiresAt:  now + accessLeft,
		RefreshExpiresAt: now + refreshLeft,
		State:            model.TokenStateActive,
		Ctime:            now - ageOffset, Mtime: now - ageOffset,
	}
	db.tokens[id] = tok
	if grant.CurrentTokenID == 0 {
		grant.CurrentTokenID = id
	}
	return access, refresh, tok
}

// mustPlaintext 一枚合法形态的凭证明文（64 位 hex，与 requireLen 的 credentialBytes*2 对齐）。
func mustPlaintext(t *testing.T) string {
	t.Helper()
	v, err := model.RandomHex(credentialBytes)
	wantOK(t, v, err, "RandomHex")
	return v
}

// mustHash 明文 → 按值定位哈希（与 logic 同一算式，测试绝不另写一套）。
func mustHash(t *testing.T, plain string) string {
	t.Helper()
	h, err := model.HashCredential(testPepper, plain)
	wantOK(t, h, err, "HashCredential")
	return h
}

// seedQuota 落一条生效配额规则。
func seedQuota(db *store, appID int64, apiCode string, windowSeconds, limit int64) *model.QuotaPolicy {
	now := nowTS()
	p := &model.QuotaPolicy{PolicyID: db.next("policy"), AppID: appID, APICode: apiCode,
		WindowSeconds: windowSeconds, QuotaLimit: limit, Enabled: 1, Ctime: now, Mtime: now}
	db.policies[p.PolicyID] = p
	return p
}

// seedEndpoint 落一个可投递（已验证、启用、未删除）的回调端点。
func seedEndpoint(db *store, appID int64, eventType int32) *model.WebhookEndpoint {
	ep := &model.WebhookEndpoint{
		EndpointID: db.next("endpoint"), AppID: appID, EventType: eventType,
		URL: "https://example.test/hook", SignKeyVersion: 1, Enabled: 1, VerifiedAt: nowTS(),
	}
	db.eps = append(db.eps, ep)
	return ep
}

// seedEndpointAt 落一个可控状态的端点：verified/enabled/deleted 三种「不可投递」形态
// 各自的成因不同（未验证 / 被停用 / 已删除），测试要能分别构造。
func seedEndpointAt(db *store, appID int64, eventType int32, url string, verified, enabled bool,
	deletedAt int64) *model.WebhookEndpoint {
	now := nowTS()
	ep := &model.WebhookEndpoint{
		EndpointID: db.next("endpoint"), AppID: appID, EventType: eventType, URL: url,
		SignKeyVersion: 1, VerifiedAt: 0, Enabled: 0, DeletedAt: deletedAt, Ctime: now, Mtime: now,
	}
	if verified {
		ep.VerifiedAt = now
	}
	if enabled {
		ep.Enabled = 1
	}
	db.eps = append(db.eps, ep)
	return ep
}

// seedDelivery 落一条投递任务（state 由调用方给，正文按重放需要留全）。
func seedDelivery(db *store, appID int64, ep *model.WebhookEndpoint, eventID string,
	state int32, payload string) *model.WebhookDelivery {
	now := nowTS()
	d := &model.WebhookDelivery{
		DeliveryID: db.next("delivery"), AppID: appID, EndpointID: ep.EndpointID,
		EventType: ep.EventType, EventID: eventID, Payload: payload,
		PayloadDigest: "sha256:" + eventID, State: state, MaxAttempts: 6,
		NextRetryAt: now, Ctime: now, Mtime: now,
	}
	db.dels = append(db.dels, d)
	return d
}

// seedScopeFull 目录项的完整形态：风险位与「是否需要用户同意」都影响门禁，
// seedScopeDir 只覆盖低风险读权限，写权限与高风险必须用这个。
func seedScopeFull(db *store, scope string, access, risk int32, consent int8, enabled int8) {
	db.scopes[scope] = &model.Scope{Scope: scope, DisplayName: scope, Access: access,
		RiskLevel: risk, RequiresUserConsent: consent, Enabled: enabled}
}

// seedWriteScope 落一个「写权限 + 声明需用户同意」的目录项：
// 缺 consent 声明时 requireWriteScopeConsent 会先拒，这条路径的正例必须有这个前提。
func seedWriteScope(db *store, scope string) {
	seedScopeFull(db, scope, model.ScopeAccessWrite, model.ScopeRiskLow, 1, 1)
}

// seedAuthCodeRow 直接落一行授权码（已知明文），返回明文与行。
// 用于构造「已使用」「已过期」「属于另一个应用」这三类负例前提。
func seedAuthCodeRow(t *testing.T, db *store, appID, mid, grantID int64, scopes []string,
	redirect, state string, expiresAt int64) (string, *model.AuthCode) {
	t.Helper()
	plain := mustPlaintext(t)
	now := nowTS()
	row := &model.AuthCode{
		CodeID: db.next("code"), AppID: appID, Mid: mid, Salt: "salt0",
		Hash: mustHash(t, plain), Scope: model.JoinScopes(scopes), RedirectURI: redirect,
		State: state, GrantID: grantID, ExpiresAt: expiresAt, Ctime: now, Mtime: now,
	}
	db.codes[row.CodeID] = row
	return plain, row
}

// seedStatusApp 落一个指定状态的应用（状态机与归属门禁的负例入口）。
func seedStatusApp(db *store, appID, ownerMid int64, status int32) *model.Application {
	app := seedApp(db, appID, ownerMid)
	app.Status = status
	return app
}

// 编译期确保 fake 满足接口：漏方法在编译期暴露，而不是运行到才 panic。
var (
	_ sqlx.SqlConn               = fakeConn{}
	_ model.ApplicationModel     = fakeApps{}
	_ model.AppSecretModel       = fakeSecrets{}
	_ model.ScopeModel           = fakeScopes{}
	_ model.AppScopeModel        = fakeAppScopes{}
	_ model.AuthCodeModel        = fakeAuthCodes{}
	_ model.GrantModel           = fakeGrants{}
	_ model.TokenModel           = fakeTokens{}
	_ model.QuotaPolicyModel     = fakeQuotaPolicies{}
	_ model.QuotaUsageModel      = fakeQuotaUsages{}
	_ model.ApiCallLogModel      = fakeCallLogs{}
	_ model.WebhookEndpointModel = fakeWebhookEndpoints{}
	_ model.WebhookDeliveryModel = fakeWebhookDeliveries{}
)
