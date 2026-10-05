package logic

// 归档链路 ArchiveAuditEntries：把一条哈希链上「已过保留期」的一段连续区间搬到对象存储，
// 并在 audit_archive_batch 留下自证凭证；purge_hot=true 时只把热表行标记 archived_at。
//
// 归档是唯一能让审计行离开热表的路径，因此它必须同时满足两件事：
//  1. 搬走的必须是「完整、连续、可重放、已过保留期」的区间——少一条、多一条、
//     序号有洞、重放断链，任何一条不满足都不允许推进 verified；
//  2. 状态机 pending → writing → verified → purged 逐级不可跳过，任一步失败停在 failed，
//     并且失败时台账上不许留下任何「产物已经落地」的引用（桶里没文件而库里记 verified，
//     等于给一个不存在的文件背书，比不归档严重得多）。
//
// 本文件用同一套判别口径覆盖三档下游故障，因为它们对客户端与恢复流程的含义完全不同：
//   - 原始错误上抛：数据面/对象存储自己失败（调用方该重试）；
//   - 换成不透明哨兵：下游回传了「合格形状」的东西但自证不合格（摘要、清单内容）；
//   - 就地吞掉只写日志：自审计留痕写不进去（批次状态已落库，回滚会抹掉「清单已落地」这一事实）。
//
// 与校验接口的条数上限差异也在这里钉住：VerifyAuditChain 受 Query.MaxVerifyEntries 限制，
// 归档区间必须整段验完才允许搬走，因此上限是 Archive.MaxEntriesPerBatch。

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/audit/internal/repository"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"
)

// --- 夹具扩展：种子数据与副作用轨迹 ---

const archiveDay = int64(86400)

// digestValueHexLen 是留痕摘要里每个维度值的长度（digestOf 取 sha256 前 16 位）。
const digestValueHexLen = 16

// 四类「驱动/客户端自己失败」的形状：不带任何业务哨兵，用来证明 logic 不翻译上游错误。
var (
	chainErr  = errors.New("audit_chain_head FindOne: Error 1205: Lock wait timeout exceeded")
	policyErr = errors.New("audit_retention_policy FindOne: Error 1040: too many connections")
	batchErr  = errors.New("audit_archive_batch FindByRequestID: lost connection to MySQL server")
	coverErr  = errors.New("audit_archive_batch FindCovering: Error 1213: Deadlock found")
)

// archiveAgedNewest 把区间内最新一条的业务时间整体落到 40 天前。
// 到期判定看的是「区间内最大 occurred_at」，所以用真实的相对时间而不是把
// archive_after_days 配成 0：只有这样才能同时钉住「已到期」与「未到期」两侧。
const archiveAgedNewest = testNow - 40*archiveDay

// seedAgedChain 与 fakes_test.go 的 seedChain 同构，但可以指定最新一条的业务时间。
// 平移时间戳后必须整条重算：occurred_at 参与哈希，尾摘要一变下一环的 prev_hash 就跟着变。
func seedAgedChain(chainKey string, n int, entryIDBase, newest int64) []*model.AuditEntry {
	rows := seedChain(chainKey, n, entryIDBase)
	prev := model.GenesisHash(chainKey)
	for i, row := range rows {
		row.OccurredAt = newest - int64(n-i-1)*60
		row.PrevHash = prev
		row.EntryHash = model.ComputeEntryHash(row)
		prev = row.EntryHash
	}
	return rows
}

// seedArchiveSubject 预置「一条 3 行的链 + 链头封口在 seq=3 + 一份已过期的 media 策略」。
func (f *fixture) seedArchiveSubject(key string, n int, archiveAfterDays int32) []*model.AuditEntry {
	rows := seedAgedChain(key, n, 300, archiveAgedNewest)
	f.putRows(rows...)
	f.entries.rangeRows = rows
	f.putHead(key, int64(n), rows[n-1].EntryHash)
	f.putPolicy(&model.RetentionPolicy{
		PolicyID: 1, ActionDomain: model.DomainOfChainKey(key),
		HotDays: 90, ArchiveAfterDays: archiveAfterDays, State: model.StateEnable, Version: 1,
	})
	return rows
}

func (f *fixture) putPolicy(p *model.RetentionPolicy) {
	cp := *p
	f.policies.byDomain[cp.ActionDomain] = &cp
	if cp.PolicyID > f.policies.nextID {
		f.policies.nextID = cp.PolicyID
	}
}

// seedDefaultPolicy 预置回退用的 default 策略（真库里是迁移种子行）。
func (f *fixture) seedDefaultPolicy(archiveAfterDays int32) {
	f.putPolicy(&model.RetentionPolicy{
		PolicyID: 9, ActionDomain: model.DefaultPolicyDomain,
		HotDays: 90, ArchiveAfterDays: archiveAfterDays, State: model.StateEnable, Version: 1,
	})
}

// goodArchiveReq 归档 chainKey 上 [from,to] 这一段；purgeHot 决定是否顺带标记热表。
func goodArchiveReq(requestID, chainKey string, from, to int64, purgeHot bool) *rpc.ArchiveAuditEntriesReq {
	return &rpc.ArchiveAuditEntriesReq{
		Ctx: testCallContext(requestID), ChainKey: chainKey, FromSeq: from, ToSeq: to, PurgeHot: purgeHot,
	}
}

func (f *fixture) mustBatch(t *testing.T, batchID int64) *model.ArchiveBatch {
	t.Helper()
	row, err := f.archives.FindOne(context.Background(), batchID)
	if err != nil {
		t.Fatalf("回读归档批次 %d 失败：%v", batchID, err)
	}
	return row
}

// batchOn 返回假库里唯一的批次（用例里只建一个批次时用，省掉到处传 batchID）。
func (f *fixture) onlyBatch(t *testing.T) *model.ArchiveBatch {
	t.Helper()
	if len(f.archives.byID) != 1 {
		t.Fatalf("期望恰好 1 个批次，实得 %d", len(f.archives.byID))
	}
	for _, r := range f.archives.byID {
		cp := *r
		return &cp
	}
	return nil
}

// assertFailedBatch 钉住「任一环节失败都停在 failed 并说明原因」：状态、原因文本、完成时间。
//
// 「停在 failed」而不是「停在 writing」是恢复流程的关键：停在 writing 的批次会被下一次
// 归档作业当成进行中而永远不动，停在 failed 才允许人工按区间重投。
func assertFailedBatch(t *testing.T, f *fixture, batchID int64, wantMsg string) *model.ArchiveBatch {
	t.Helper()
	row := f.mustBatch(t, batchID)
	if row.State != model.BatchStateFailed {
		t.Fatalf("批次状态 = %s，期望 failed（归档绝不允许停在 writing）", row.State)
	}
	if !strings.Contains(row.ErrMsg, wantMsg) {
		t.Fatalf("err_msg = %q，应说明 %q", row.ErrMsg, wantMsg)
	}
	if len(row.ErrMsg) > model.MaxErrMsgBytes {
		t.Fatalf("err_msg 超出列宽：%d", len(row.ErrMsg))
	}
	if strings.ContainsAny(row.ErrMsg, "\r\n") {
		t.Fatalf("err_msg 带换行会破坏日志与导出列：%q", row.ErrMsg)
	}
	if row.FinishedAt == 0 {
		t.Fatal("失败批次没有 finished_at，覆盖率与 SLA 统计会漏掉这一批")
	}
	return row
}

// assertNoArtifactReference 钉住「清单还没自证通过，台账上就不许出现任何产物痕迹」。
func assertNoArtifactReference(t *testing.T, row *model.ArchiveBatch) {
	t.Helper()
	if row.ManifestHash != "" || row.LastEntryHash != "" {
		t.Fatalf("失败批次带着清单摘要，事后无从判断它到底验过没有：%+v", row)
	}
}

// archiveTrail 返回本服务 system 链上的归档留痕（按 seq 升序）。
func (f *fixture) archiveTrail(t *testing.T) []*model.AuditEntry {
	t.Helper()
	return f.chainOf(model.ChainKey(selfDomainSystem, f.now))
}

// assertDims 断言留痕摘要恰好是 want 这几对维度（键升序、`;` 分隔）。
//
// 摘要列只收 `字段=16hex` 白名单摘要（helpers.go 的 digestPairs 对所有值一律压摘要），
// 因此这里比的是「每个字段的值的摘要」而不是原文：
// 少记一个字段、记错一个值、把原文抄进摘要列，任何一条都会让本断言变红。
func assertDims(t *testing.T, label, got string, want map[string]string) {
	t.Helper()
	if !model.ValidDigest(got) {
		t.Fatalf("%s 不是合法的摘要列文本：%q", label, got)
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		v := want[k]
		if len(v) > digestValueHexLen && strings.Contains(got, v) {
			t.Fatalf("%s 把长原文照抄进了摘要列：%q 含 %q", label, got, v)
		}
		pairs = append(pairs, k+"="+digestOf(v))
	}
	if wantText := strings.Join(pairs, ";"); got != wantText {
		t.Fatalf("%s =\n%q\n期望\n%q\n（明文期望值：%v）", label, got, wantText, want)
	}
}

// --- 副作用轨迹替身（retention_test.go 共用） ---
//
// 归档「按什么顺序碰了哪些依赖」本身就是结论：先落清单再标记账号、留痕在状态推进之后、
// 幂等回查在建批次之前，这些都是只能在顺序上才看得出来的性质。
// 自审计的链上装配（预检 FindByEventIDs / Ensure / LockForUpdate / Advance）折叠成
// 一条 entries.Insert:<action>——要断言的是「什么时候写了哪条痕」，摊成 5 步会淹掉批次推进。
type traceRec struct{ steps []string }

func (r *traceRec) add(s string) { r.steps = append(r.steps, s) }

func (r *traceRec) assert(t *testing.T, want ...string) {
	t.Helper()
	if len(r.steps) != len(want) {
		t.Fatalf("副作用序列长度 = %d，期望 %d\n实得：%s\n期望：%s",
			len(r.steps), len(want), strings.Join(r.steps, " | "), strings.Join(want, " | "))
	}
	for i := range want {
		if r.steps[i] != want[i] {
			t.Fatalf("第 %d 步 = %q，期望 %q\n完整实得序列：%s", i+1, r.steps[i], want[i],
				strings.Join(r.steps, " | "))
		}
	}
}

func (r *traceRec) assertEmpty(t *testing.T) {
	t.Helper()
	if len(r.steps) != 0 {
		t.Fatalf("入参守卫阶段一次依赖都不许碰，实得：%s", strings.Join(r.steps, " | "))
	}
}

func (r *traceRec) count() int { return len(r.steps) }

type tracedChains struct {
	*fakeChains
	rec *traceRec
}

func (c tracedChains) FindOne(ctx context.Context, chainKey string) (*model.ChainHead, error) {
	c.rec.add("chains.FindOne:" + chainKey)
	return c.fakeChains.FindOne(ctx, chainKey)
}

type tracedEntries struct {
	*fakeEntries
	rec *traceRec
}

func (e tracedEntries) Insert(ctx context.Context, session sqlx.Session, row *model.AuditEntry) (int64, error) {
	e.rec.add("entries.Insert:" + row.Action)
	return e.fakeEntries.Insert(ctx, session, row)
}

func (e tracedEntries) ListByChainRange(ctx context.Context, chainKey string, from, to int64, limit int32) ([]*model.AuditEntry, error) {
	e.rec.add(fmt.Sprintf("entries.ListByChainRange:%s:%d-%d limit=%d", chainKey, from, to, limit))
	return e.fakeEntries.ListByChainRange(ctx, chainKey, from, to, limit)
}

type tracedPolicies struct {
	*fakePolicies
	rec *traceRec
}

func (p tracedPolicies) FindEffective(ctx context.Context, actionDomain string) (*model.RetentionPolicy, error) {
	p.rec.add("policies.FindEffective:" + actionDomain)
	return p.fakePolicies.FindEffective(ctx, actionDomain)
}

func (p tracedPolicies) FindOne(ctx context.Context, actionDomain string) (*model.RetentionPolicy, error) {
	p.rec.add("policies.FindOne:" + actionDomain)
	return p.fakePolicies.FindOne(ctx, actionDomain)
}

func (p tracedPolicies) Insert(ctx context.Context, row *model.RetentionPolicy) (int64, error) {
	p.rec.add("policies.Insert:" + row.ActionDomain)
	return p.fakePolicies.Insert(ctx, row)
}

func (p tracedPolicies) UpdateWithVersion(ctx context.Context, row *model.RetentionPolicy, expect int64) (bool, error) {
	p.rec.add(fmt.Sprintf("policies.UpdateWithVersion:%s@%d", row.ActionDomain, expect))
	return p.fakePolicies.UpdateWithVersion(ctx, row, expect)
}

type tracedArchives struct {
	*fakeArchives
	rec *traceRec
}

func (a tracedArchives) Insert(ctx context.Context, b *model.ArchiveBatch) (int64, error) {
	a.rec.add(fmt.Sprintf("archives.Insert:%s:%d-%d", b.ChainKey, b.FromSeq, b.ToSeq))
	return a.fakeArchives.Insert(ctx, b)
}

func (a tracedArchives) FindOne(ctx context.Context, batchID int64) (*model.ArchiveBatch, error) {
	a.rec.add("archives.FindOne:" + strconv.FormatInt(batchID, 10))
	return a.fakeArchives.FindOne(ctx, batchID)
}

func (a tracedArchives) FindByRequestID(ctx context.Context, requestID string) (*model.ArchiveBatch, error) {
	a.rec.add("archives.FindByRequestID:" + requestID)
	return a.fakeArchives.FindByRequestID(ctx, requestID)
}

func (a tracedArchives) FindCovering(ctx context.Context, chainKey string, from, to int64) (*model.ArchiveBatch, error) {
	a.rec.add(fmt.Sprintf("archives.FindCovering:%s:%d-%d", chainKey, from, to))
	return a.fakeArchives.FindCovering(ctx, chainKey, from, to)
}

func (a tracedArchives) TransitionState(ctx context.Context, batchID int64, from, to,
	manifestHash, lastEntryHash, errMsg string) (bool, error) {
	a.rec.add(fmt.Sprintf("archives.TransitionState:%d:%s→%s", batchID, from, to))
	return a.fakeArchives.TransitionState(ctx, batchID, from, to, manifestHash, lastEntryHash, errMsg)
}

type tracedStore struct {
	*fakeStore
	rec *traceRec
}

func (s tracedStore) CreateObject(ctx context.Context, bucket, objectKey string) (objectWriter, error) {
	s.rec.add("storage.CreateObject:" + bucket + "/" + objectKey)
	return s.fakeStore.CreateObject(ctx, bucket, objectKey)
}

// traceAll 把五张表 + 对象存储的替身换成记账壳。
// 必须在 withOpts 之后调用：withOpts 会重建 deps，先装壳会被覆盖掉。
//
// 清热表那一步单独记：fixture 的 purge 闭包直接调 f.entries（绕过 deps 壳），
// 所以这里包一层记录「按哪一段、哪个时间戳去标记」，记的是批次里的区间——
// 正是 repository.PurgeMark 实际使用的定位键。
func (f *fixture) traceAll() *traceRec {
	rec := &traceRec{}
	f.deps.entries = tracedEntries{fakeEntries: f.entries, rec: rec}
	f.deps.chains = tracedChains{fakeChains: f.chains, rec: rec}
	f.deps.policies = tracedPolicies{fakePolicies: f.policies, rec: rec}
	f.deps.archives = tracedArchives{fakeArchives: f.archives, rec: rec}
	f.deps.storage = tracedStore{fakeStore: f.store, rec: rec}
	if base := f.deps.purge; base != nil {
		f.deps.purge = func(ctx context.Context, b *model.ArchiveBatch, ts int64) (int64, error) {
			rec.add(fmt.Sprintf("purge:%s:%d-%d@%d", b.ChainKey, b.FromSeq, b.ToSeq, ts))
			return base(ctx, b, ts)
		}
	}
	return rec
}

// --- 造故障的替身 ---
//
// 这些形状（对象侧摘要缺失、并发抢输、推进未命中、留痕写不进）在内存替身之外只能靠
// 真库并发或真 S3 客户端才能造出来，而本服务本期没引入任何对象存储客户端，
// 因此只能在替身上造；它们钉住的判定全部在 logic 里，不是替身自证。

// rivalArchives 复现「幂等预检漏、插入时对手已提交」：第一次回查扑空，
// 插入前把对手行落库（占住 request_id），于是回查兜底分支一定命中。
type rivalArchives struct {
	*fakeArchives
	probed int
	rival  *model.ArchiveBatch
}

func (r *rivalArchives) FindByRequestID(ctx context.Context, requestID string) (*model.ArchiveBatch, error) {
	r.probed++
	if r.probed == 1 {
		return nil, nil
	}
	return r.fakeArchives.FindByRequestID(ctx, requestID)
}

func (r *rivalArchives) Insert(ctx context.Context, b *model.ArchiveBatch) (int64, error) {
	if _, err := r.fakeArchives.Insert(ctx, r.rival); err != nil {
		return 0, err
	}
	return r.fakeArchives.Insert(ctx, b)
}

// alwaysExistsArchives 让插入永远撞唯一键而回查仍然扑空（「库里明明有冲突却查不到」的形状）。
// 这一支必须把原始错误原样抛出：报成 reused 就是在给一个不存在的批次背书。
type alwaysExistsArchives struct{ *fakeArchives }

func (a alwaysExistsArchives) Insert(context.Context, *model.ArchiveBatch) (int64, error) {
	return 0, model.ErrTaskExists
}

func (a alwaysExistsArchives) FindByRequestID(context.Context, string) (*model.ArchiveBatch, error) {
	return nil, nil
}

// stuckArchives 让「推进到某个状态」永远未命中（返回 false, nil），模拟并发对手抢先推进。
type stuckArchives struct {
	*fakeArchives
	blockTo string
}

func (s stuckArchives) TransitionState(ctx context.Context, batchID int64, from, to,
	manifestHash, lastEntryHash, errMsg string) (bool, error) {
	if to == s.blockTo {
		return false, nil
	}
	return s.fakeArchives.TransitionState(ctx, batchID, from, to, manifestHash, lastEntryHash, errMsg)
}

// coverErrArchives 只让「覆盖预检」这一步失败：共用替身的 FindCovering 不读 findErr，
// 而这一支恰恰是「查不出有没有覆盖就不许再建第二个批次」的关键判定。
type coverErrArchives struct {
	*fakeArchives
	err error
}

func (a coverErrArchives) FindCovering(context.Context, string, int64, int64) (*model.ArchiveBatch, error) {
	return nil, a.err
}

// lieStore 让对象存储回传不真实的自证信息或中途失败：
// 摘要为空/被换掉、object_key 与请求不符、落地内容多一截（本地清单摘要就对不上）、
// Write/Commit 失败。真实现里这些都是 S3/OSS 客户端的行为，本期未引入客户端。
type lieStore struct {
	*fakeStore
	target    string // 生效的对象键；空串 = 对所有对象生效
	blankSHA  bool
	fakeSHA   string
	fakeKey   string
	tamper    []byte
	writeErr  error
	commitErr error
}

func (s lieStore) CreateObject(ctx context.Context, bucket, objectKey string) (objectWriter, error) {
	w, err := s.fakeStore.CreateObject(ctx, bucket, objectKey)
	if err != nil {
		return nil, err
	}
	if s.target != "" && s.target != objectKey {
		return w, nil
	}
	if o := s.fakeStore.objects[objectKey]; o != nil {
		o.tamper = s.tamper
	}
	return lyingWriter{objectWriter: w, s: s}, nil
}

type lyingWriter struct {
	objectWriter
	s lieStore
}

func (w lyingWriter) Write(p []byte) (int, error) {
	if w.s.writeErr != nil {
		return 0, w.s.writeErr
	}
	return w.objectWriter.Write(p)
}

func (w lyingWriter) Commit() (objectLocation, error) {
	if w.s.commitErr != nil {
		return objectLocation{}, w.s.commitErr
	}
	loc, err := w.objectWriter.Commit()
	if err != nil {
		return loc, err
	}
	if w.s.blankSHA {
		loc.SHA256Hex = ""
	}
	if w.s.fakeSHA != "" {
		loc.SHA256Hex = w.s.fakeSHA
	}
	if w.s.fakeKey != "" {
		loc.ObjectKey = w.s.fakeKey
	}
	return loc, nil
}

// trailFailingEntries 让第 n 次自审计写入失败（第 0 次是业务行写入，本用例里没有）。
type trailFailingEntries struct {
	*fakeEntries
	nth      int
	writes   int
	recorded *model.AuditEntry
}

func (e *trailFailingEntries) Insert(ctx context.Context, session sqlx.Session, row *model.AuditEntry) (int64, error) {
	e.writes++
	if e.writes == e.nth {
		cp := *row
		e.recorded = &cp
		return 0, errors.New("audit_entry Insert: Error 1213: Deadlock found when trying to get lock")
	}
	return e.fakeEntries.Insert(ctx, session, row)
}

// realEffectivePolicies 复刻真实 model 的 FindEffective：域策略不存在**或已停用**都回落到 default
// （model/auditretentionpolicy.go:128-144）。共用替身 fakePolicies 只在「不存在」时回落，
// 因此需要这一层才能观察到「停用后回落」对上层判定的影响，见 README「策略停用不挡归档」。
type realEffectivePolicies struct{ *fakePolicies }

func (p realEffectivePolicies) FindEffective(ctx context.Context, actionDomain string) (*model.RetentionPolicy, error) {
	row, err := p.FindOne(ctx, actionDomain)
	if err != nil {
		return nil, err
	}
	if row != nil && row.State == model.StateEnable {
		return row, nil
	}
	fallback, err := p.FindOne(ctx, model.DefaultPolicyDomain)
	if err != nil {
		return nil, err
	}
	if fallback == nil {
		return nil, model.ErrPolicyNotFound
	}
	return fallback, nil
}

// --- 1. 入参守卫：被拒时一次依赖都不许碰 ---

func TestArchiveAuditEntriesRejectsBeforeTouchingData(t *testing.T) {
	const key = "media/2026-03-05"

	// 空请求单独判：表内用例都要先造一份合法请求再改坏一个字段。
	f0 := newFixture(t)
	f0.use(t)
	rec0 := f0.traceAll()
	// 前提条件全部就绪（链头、策略、桶），此时唯一能挡住的就是入参守卫本身。
	f0.seedArchiveSubject(key, 3, 30)
	if reply, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
		ArchiveAuditEntries(nil); !errors.Is(err, model.ErrRequestRequired) || reply != nil {
		t.Fatalf("空请求 err = %v reply = %+v，期望 ErrRequestRequired 且不回响应", err, reply)
	}
	rec0.assertEmpty(t)

	cases := []struct {
		name    string
		mutate  func(*rpc.ArchiveAuditEntriesReq)
		wantErr error
	}{
		{"无 ctx", func(r *rpc.ArchiveAuditEntriesReq) { r.Ctx = nil }, model.ErrRequestRequired},
		{"缺 caller_service", func(r *rpc.ArchiveAuditEntriesReq) { r.Ctx.CallerService = "  " }, model.ErrCallerRequired},
		{"缺 request_id", func(r *rpc.ArchiveAuditEntriesReq) { r.Ctx.RequestId = "" }, model.ErrRequestIDRequired},
		{"request_id 超列宽", func(r *rpc.ArchiveAuditEntriesReq) {
			r.Ctx.RequestId = strings.Repeat("r", model.MaxRequestIDBytes+1)
		}, model.ErrFieldTooLong},
		{"trace_id 超列宽", func(r *rpc.ArchiveAuditEntriesReq) {
			r.Ctx.TraceId = strings.Repeat("t", model.MaxTraceIDBytes+1)
		}, model.ErrFieldTooLong},
		{"负 operator_id", func(r *rpc.ArchiveAuditEntriesReq) { r.Ctx.OperatorId = -1 }, model.ErrActorIDInvalid},
		{"chain_key 为空", func(r *rpc.ArchiveAuditEntriesReq) { r.ChainKey = "   " }, model.ErrChainKeyRequired},
		{"chain_key 只有动作域", func(r *rpc.ArchiveAuditEntriesReq) { r.ChainKey = "media" }, model.ErrChainKeyRequired},
		{"chain_key 日期段不完整", func(r *rpc.ArchiveAuditEntriesReq) { r.ChainKey = "media/2026" },
			model.ErrChainKeyRequired},
		{"chain_key 大写", func(r *rpc.ArchiveAuditEntriesReq) { r.ChainKey = "MEDIA/2026-03-05" },
			model.ErrChainKeyRequired},
		{"chain_key 尾随多余段", func(r *rpc.ArchiveAuditEntriesReq) { r.ChainKey = key + "/x" },
			model.ErrChainKeyRequired},
		{"from_seq 为负", func(r *rpc.ArchiveAuditEntriesReq) { r.FromSeq = -1 }, model.ErrArchiveRangeInvalid},
		{"to_seq 为负", func(r *rpc.ArchiveAuditEntriesReq) { r.ToSeq = -9 }, model.ErrArchiveRangeInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			rec := f.traceAll()
			f.seedArchiveSubject(key, 3, 30)
			in := goodArchiveReq("req-arc-guard", key, 2, 3, true)
			tc.mutate(in)
			reply, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).ArchiveAuditEntries(in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if reply != nil {
				t.Fatalf("拒绝时不得回响应对象：%+v", reply)
			}
			rec.assertEmpty(t)
			if got := len(f.archives.byID); got != 0 {
				t.Fatalf("被拒请求建了 %d 个批次", got)
			}
			if got := len(f.store.created); got != 0 {
				t.Fatalf("被拒请求写了对象：%v", f.store.created)
			}
			if got := len(f.archiveTrail(t)); got != 0 {
				t.Fatalf("被拒请求写了 %d 条归档留痕", got)
			}
		})
	}
}

// 守卫的先后顺序同样是契约：先拒「无法归因的调用」，再拒「定位不到对象的入参」。
// 顺序反了会让调用方看到 chain_key 错误，而真正的问题是它没带幂等键。
func TestArchiveAuditEntriesGuardOrdering(t *testing.T) {
	const key = "media/2026-03-05"
	cases := []struct {
		name    string
		mutates []func(*rpc.ArchiveAuditEntriesReq)
		want    string
		wantErr error
	}{
		{"ctx 守卫先于 chain_key", []func(*rpc.ArchiveAuditEntriesReq){
			func(r *rpc.ArchiveAuditEntriesReq) { r.Ctx.RequestId = "" },
			func(r *rpc.ArchiveAuditEntriesReq) { r.ChainKey = "坏键名" },
		}, "request_id", model.ErrRequestIDRequired},
		{"链名字符集先于区间数值", []func(*rpc.ArchiveAuditEntriesReq){
			func(r *rpc.ArchiveAuditEntriesReq) { r.ChainKey = "media/20260305" },
			func(r *rpc.ArchiveAuditEntriesReq) { r.FromSeq = -1 },
		}, "chain_key", model.ErrChainKeyRequired},
		{"区间符号先于读链头", []func(*rpc.ArchiveAuditEntriesReq){
			func(r *rpc.ArchiveAuditEntriesReq) { r.ToSeq = -1 },
			func(r *rpc.ArchiveAuditEntriesReq) { r.ChainKey = "nonexistent/2026-03-05" },
		}, "seq range", model.ErrArchiveRangeInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			rec := f.traceAll()
			f.seedArchiveSubject(key, 3, 30)
			in := goodArchiveReq("req-arc-order", key, 2, 3, true)
			for _, m := range tc.mutates {
				m(in)
			}
			_, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).ArchiveAuditEntries(in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误文案没有点出真正先失败的字段 %q：%v", tc.want, err)
			}
			rec.assertEmpty(t)
		})
	}
}

// --- 2. 读侧依赖故障：三类错误语义要分得开 ---

func TestArchiveAuditEntriesPropagatesReadFailures(t *testing.T) {
	const key = "media/2026-03-05"

	cases := []struct {
		name       string
		setup      func(*fixture)
		wantErr    error
		wantRaw    error   // 非 nil 时要求错误原样带上游消息（不许被改写成哨兵）
		wantUnlike []error // 不许被降级成的哨兵
	}{
		{"链不存在：明确报链头缺失", func(f *fixture) {
			f.seedArchiveSubject(key, 3, 30)
			delete(f.chains.heads, key) // 有行没头：链还在长，归档只认已封口部分
		}, model.ErrChainHeadMissing, nil, nil},
		{"链头读失败：原始错误上抛", func(f *fixture) {
			f.seedArchiveSubject(key, 3, 30)
			f.deps.chains = rawErrChains{fakeChains: f.chains, err: chainErr}
		}, nil, chainErr, []error{
			model.ErrArchiveRangeInvalid, model.ErrChainHeadMissing, model.ErrArchiveNotDue,
			model.ErrArchiveRangeTooLarge, model.ErrPolicyNotFound,
		}},
		{"策略读失败：原始错误上抛", func(f *fixture) {
			f.seedArchiveSubject(key, 3, 30)
			f.policies.findErr = policyErr
		}, nil, policyErr, []error{model.ErrPolicyNotFound}},
		{"无生效策略：拒绝归档而不是按默认窗口搬走", func(f *fixture) {
			f.seedArchiveSubject(key, 3, 30)
			delete(f.policies.byDomain, model.DefaultPolicyDomain)
			delete(f.policies.byDomain, "media")
		}, model.ErrPolicyNotFound, nil, nil},
		{"域策略停用：归档跳过该域", func(f *fixture) {
			f.seedArchiveSubject(key, 3, 30)
			f.policies.byDomain["media"].State = model.StateDisable
		}, model.ErrPolicyNotFound, nil, nil},
		{"桶未配置：显式失败而不是建批次", func(f *fixture) {
			f.seedArchiveSubject(key, 3, 30)
			f.withOpts(func(o *repository.Options) { o.Storage.Bucket = "" })
		}, model.ErrObjectStorageMissing, nil, nil},
		{"幂等回查失败：不许跳过幂等预检直接建批次", func(f *fixture) {
			f.seedArchiveSubject(key, 3, 30)
			f.archives.findErr = batchErr
		}, nil, batchErr, nil},
		{"覆盖批次查询失败：不许当作「没有覆盖」继续建新批", func(f *fixture) {
			f.seedArchiveSubject(key, 3, 30)
			f.deps.archives = coverErrArchives{fakeArchives: f.archives, err: coverErr}
		}, nil, coverErr, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			tc.setup(f)
			// 链不存在那一例刻意不预置 key 的链头，其余用例都要有链头。
			reply, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
				ArchiveAuditEntries(goodArchiveReq("req-arc-readfail", key, 2, 3, true))
			if err == nil {
				t.Fatalf("数据面故障必须报错，实得 %+v", reply)
			}
			if reply != nil {
				t.Fatalf("失败时不得回响应对象：%+v", reply)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			for _, bad := range tc.wantUnlike {
				if errors.Is(err, bad) {
					t.Fatalf("数据面故障被降级成了 %v：%v", bad, err)
				}
			}
			if tc.wantRaw != nil {
				// 「原样上抛」= 错误链里认得出上游那个哨兵/错误对象，且文案逐字带着驱动消息：
				// 归档作业靠文本区分「瞬时故障可重试」与「区间本身有问题」。
				if !errors.Is(err, tc.wantRaw) || !strings.Contains(err.Error(), tc.wantRaw.Error()) {
					t.Fatalf("err = %v，期望原样带上 %v", err, tc.wantRaw)
				}
			}
			// 读阶段失败：一个批次、一个对象、一条留痕都不许有。
			if got := len(f.archives.byID); got != 0 {
				t.Fatalf("读阶段失败仍建了 %d 个批次", got)
			}
			if got := len(f.store.created); got != 0 {
				t.Fatalf("读阶段失败仍写了对象：%v", f.store.created)
			}
			if got := len(f.archiveTrail(t)); got != 0 {
				t.Fatalf("读阶段失败仍写了 %d 条留痕", got)
			}
			if got := len(f.entries.markCalls); got != 0 {
				t.Fatalf("读阶段失败仍标记了 %d 次热表行：%v", got, f.entries.markCalls)
			}
			for _, row := range f.chainOf(key) {
				if row.ArchivedAt != 0 {
					t.Fatalf("读阶段失败却把链上第 %d 行标成已归档：%d", row.Seq, row.ArchivedAt)
				}
			}
		})
	}

	// 「链不存在」那一例的真实形状是 FindOne 返回 ErrChainHeadMissing：
	// 上面把它和「无策略」混在一起判不够狠，这里单独钉一次数据面完全没被推进。
	t.Run("链不存在时策略与批次表都不该被打", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		rec := f.traceAll()
		f.seedDefaultPolicy(30)
		if _, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
			ArchiveAuditEntries(goodArchiveReq("req-arc-nohead", key, 1, 0, false)); !errors.Is(err, model.ErrChainHeadMissing) {
			t.Fatalf("err = %v，期望 ErrChainHeadMissing", err)
		}
		rec.assert(t, "chains.FindOne:"+key)
	})
}

// 链头「存在但读不动」的形状：替身返回一个不带任何业务哨兵的原生驱动错误。
type rawErrChains struct {
	*fakeChains
	err error
}

func (c rawErrChains) FindOne(context.Context, string) (*model.ChainHead, error) {
	return nil, c.err
}

// --- 3. 幂等与覆盖复用 ---

func TestArchiveAuditEntriesIsIdempotentByRequestID(t *testing.T) {
	const key = "media/2026-03-05"
	f := newFixture(t)
	f.use(t)
	rows := f.seedArchiveSubject(key, 3, 30)
	l := NewArchiveAuditEntriesLogic(context.Background(), svcCtx())

	first, err := l.ArchiveAuditEntries(goodArchiveReq("req-arc-idem", key, 1, 3, true))
	if err != nil {
		t.Fatal(err)
	}
	if first.GetReused() {
		t.Fatal("首次归档不得报 reused")
	}
	batchID := first.GetBatch().GetBatchId()
	// 首批凭证本身要自证：区间尾摘要取自区间最后一行，清单摘要是本地重放值。
	if got := first.GetBatch().GetLastEntryHash(); got != rows[2].EntryHash {
		t.Fatalf("last_entry_hash = %s，期望区间尾行 seq=3 的 %s", got, rows[2].EntryHash)
	}

	// 原样重放：报既有批次，不写第二份清单、不写第二条痕、不二次标记热表。
	second, err := l.ArchiveAuditEntries(goodArchiveReq("req-arc-idem", key, 1, 3, true))
	if err != nil {
		t.Fatalf("重放必须幂等成功而不是撞唯一键：%v", err)
	}
	if !second.GetReused() {
		t.Fatal("重放未报 reused")
	}
	if got := second.GetBatch(); !reflect.DeepEqual(got, first.GetBatch()) {
		t.Fatalf("重放回了不一样的批次：\n%+v\n%+v", first.GetBatch(), got)
	}
	if got := len(f.archives.byID); got != 1 {
		t.Fatalf("同一 request_id 建了 %d 个批次", got)
	}
	if got := len(f.store.created); got != 2 {
		t.Fatalf("重放写出了第二份产物：%v", f.store.created)
	}
	if got := len(f.archiveTrail(t)); got != 3 {
		t.Fatalf("重放后留痕 %d 条，期望仍是 3 条（writing/verified/purged）", got)
	}
	if got := len(f.entries.markCalls); got != 1 {
		t.Fatalf("重放标记热表 %d 次，期望 1：%v", got, f.entries.markCalls)
	}
	// 幂等键是 request_id，不是区间：换 request_id 就是另一次作业。
	if _, err := l.ArchiveAuditEntries(goodArchiveReq("req-arc-idem-2", key, 1, 1, false)); err != nil {
		t.Fatal(err)
	}
	if got := len(f.archives.byID); got != 2 {
		t.Fatalf("不同 request_id 应各自成批，实得 %d", got)
	}
	if batchID != 1 {
		t.Fatalf("首批 batch_id = %d，期望 1（回参必须带库内主键而不是零值）", batchID)
	}
	// 第二个批次只搬 seq=1 那一行。
	if got := f.mustBatch(t, 2).RowCount; got != 1 {
		t.Fatalf("第二批次 row_count = %d，期望 1", got)
	}
	// 首批仍是 purged，重放没有把它倒回 verified。
	if got := f.mustBatch(t, batchID).State; got != model.BatchStatePurged {
		t.Fatalf("首批状态 = %s，期望 purged", got)
	}
}

// 已封口区间被更早的批次整体覆盖时，复用返回既有凭证：
// 「同一段链被搬两次」会产生两份清单，事后无法判断哪份是真的。
func TestArchiveAuditEntriesReusesCoveringBatch(t *testing.T) {
	const key = "media/2026-03-05"
	f := newFixture(t)
	f.use(t)
	f.seedArchiveSubject(key, 3, 30)
	f.archives.covering = &model.ArchiveBatch{
		BatchID: 77, RequestID: "req-arc-elder", ChainKey: key, FromSeq: 1, ToSeq: 5, RowCount: 5,
		Bucket: f.opts.Storage.Bucket, ObjectKey: "audit-archive/media-2026-03-05/covered.manifest.csv",
		ManifestHash: strings.Repeat("1", model.HashHexLen), State: model.BatchStateVerified,
		OperatorID: 9, TraceID: "trace-elder", FinishedAt: testNow - 60,
	}
	reply, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
		ArchiveAuditEntries(goodArchiveReq("req-arc-covered", key, 2, 3, true))
	if err != nil {
		t.Fatal(err)
	}
	if !reply.GetReused() {
		t.Fatal("已被覆盖的区间必须报 reused")
	}
	if got := reply.GetBatch(); got.GetBatchId() != 77 || got.GetState() != model.BatchStateVerified ||
		got.GetOperatorId() != 9 || got.GetCtime() != 0 || got.GetTraceId() != "trace-elder" {
		t.Fatalf("复用必须原样回既有凭证：%+v", got)
	}
	if got := len(f.archives.byID); got != 0 {
		t.Fatalf("复用路径建了 %d 个新批次", got)
	}
	if got := len(f.archives.inserts); got != 0 {
		t.Fatalf("复用路径仍插入批次：%v", f.archives.inserts)
	}
	if got := len(f.store.created); got != 0 {
		t.Fatalf("复用路径仍写清单：%v", f.store.created)
	}
	if got := len(f.archiveTrail(t)); got != 0 {
		t.Fatalf("复用路径仍写留痕：%d 条", got)
	}
	if got := len(f.entries.markCalls); got != 0 {
		t.Fatalf("复用路径仍标记热表：%v", f.entries.markCalls)
	}
	// 覆盖判定只看「是否包住请求区间」，与 request_id 无关：换一个不相关的 request_id 也复用。
	if _, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
		ArchiveAuditEntries(goodArchiveReq("req-arc-covered-2", key, 3, 3, false)); err != nil {
		t.Fatal(err)
	}
	if got := len(f.archives.byID); got != 0 {
		t.Fatalf("换 request_id 后仍复用了覆盖批次：新批次 %d 个", got)
	}
}

// 并发窗口：幂等预检没看到对手、插入时撞唯一键、回查才拿到先落库那一行。
func TestArchiveAuditEntriesConcurrentDuplicateResolvesToExistingBatch(t *testing.T) {
	const key = "media/2026-03-05"
	f := newFixture(t)
	f.use(t)
	f.seedArchiveSubject(key, 3, 30)
	rival := &model.ArchiveBatch{RequestID: "req-arc-race", ChainKey: key, FromSeq: 1, ToSeq: 3,
		RowCount: 3, Bucket: "cold-rival", ObjectKey: "rival.manifest.csv", State: model.BatchStatePending,
		OperatorID: 9, TraceID: "trace-rival"}
	f.deps.archives = &rivalArchives{fakeArchives: f.archives, rival: rival}

	reply, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
		ArchiveAuditEntries(goodArchiveReq("req-arc-race", key, 1, 3, true))
	if err != nil {
		t.Fatalf("并发撞唯一键必须转成幂等成功：%v", err)
	}
	if !reply.GetReused() || reply.GetBatch().GetOperatorId() != 9 {
		t.Fatalf("回放的必须是先落库那条批次：%+v", reply.GetBatch())
	}
	if got := len(f.archives.byID); got != 1 {
		t.Fatalf("库里应有对手那一条，实得 %d", got)
	}
	// 抢输的一方不推进状态机、不写清单、不标记热表、不留痕：批次不归它。
	if got := len(f.archives.moves); got != 0 {
		t.Fatalf("抢输的一方推进了状态机：%v", f.archives.moves)
	}
	if got := len(f.store.created); got != 0 {
		t.Fatalf("抢输的一方写了清单：%v", f.store.created)
	}
	if got := len(f.entries.markCalls); got != 0 {
		t.Fatalf("抢输的一方标记了热表：%v", f.entries.markCalls)
	}
	if got := len(f.archiveTrail(t)); got != 0 {
		t.Fatalf("抢输的一方写了 %d 条留痕", got)
	}
}

// 回查扑空（库里报了唯一键冲突却查不到行）时不许假装 reused。
func TestArchiveAuditEntriesUniqueConflictWithoutRivalStaysError(t *testing.T) {
	const key = "media/2026-03-05"
	f := newFixture(t)
	f.use(t)
	f.seedArchiveSubject(key, 3, 30)
	f.deps.archives = alwaysExistsArchives{fakeArchives: f.archives}
	reply, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
		ArchiveAuditEntries(goodArchiveReq("req-arc-ghost", key, 1, 3, true))
	if reply != nil {
		t.Fatalf("查不到对手批次时不得回响应：%+v", reply)
	}
	if !errors.Is(err, model.ErrTaskExists) {
		t.Fatalf("err = %v，期望原样 ErrTaskExists（报 reused 等于给不存在的批次背书）", err)
	}
	if got := len(f.archiveTrail(t)); got != 0 {
		t.Fatalf("建批失败仍写了 %d 条留痕", got)
	}
}

// pending→writing 未命中（被并发推进）时以库为准复用返回，绝不双写清单。
func TestArchiveAuditEntriesLosesClaimAndReReadsBatch(t *testing.T) {
	const key = "media/2026-03-05"
	f := newFixture(t)
	f.use(t)
	f.seedArchiveSubject(key, 3, 30)
	f.deps.archives = stuckArchives{fakeArchives: f.archives, blockTo: model.BatchStateWriting}
	reply, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
		ArchiveAuditEntries(goodArchiveReq("req-arc-claim", key, 1, 3, true))
	if err != nil {
		t.Fatalf("抢锁失败不是调用失败：%v", err)
	}
	if !reply.GetReused() {
		t.Fatal("未命中推进权必须报 reused")
	}
	if got := len(f.store.created); got != 0 {
		t.Fatalf("没有推进权却写了清单：%v", f.store.created)
	}
	if got := len(f.archiveTrail(t)); got != 0 {
		t.Fatalf("没有推进权却写了 %d 条留痕", got)
	}
	if got := len(f.entries.markCalls); got != 0 {
		t.Fatalf("没有推进权却标记了热表：%v", f.entries.markCalls)
	}
	// 复用回的是库里那一行的原样状态（这里仍是 pending），不是本请求想推进到的 writing。
	if got := reply.GetBatch().GetState(); got != model.BatchStatePending {
		t.Fatalf("回参状态 = %s，期望原样回报库内状态", got)
	}
}

// --- 4. 区间语义：缺省、夹取、上下界 ---

func TestArchiveAuditEntriesRangeSemantics(t *testing.T) {
	const key = "media/2026-03-05"
	cases := []struct {
		name      string
		headSeq   int64
		from, to  int64
		wantFrom  int64
		wantTo    int64
		wantCount int64
		wantErr   string
	}{
		{"to_seq=0 取链头", 3, 1, 0, 1, 3, 3, ""},
		{"to_seq 超过链头夹到链头", 3, 1, 99, 1, 3, 3, ""},
		{"from_seq=0 归一为 1", 3, 0, 2, 1, 2, 2, ""},
		{"两端都缺省 = 整条已封口链", 3, 0, 0, 1, 3, 3, ""},
		{"只归档最后一行", 3, 3, 3, 3, 3, 1, ""},
		{"空链（head_seq=0）无已封口条目", 0, 1, 0, 0, 0, 0, "无已封口条目"},
		{"from 落在链头之后", 3, 4, 0, 0, 0, 0, "无已封口条目"},
		{"from 大于请求 to", 3, 2, 1, 0, 0, 0, "无已封口条目"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			rows := seedAgedChain(key, 3, 300, archiveAgedNewest)
			f.putRows(rows...)
			f.entries.rangeRows = rows
			if tc.headSeq > 0 {
				f.putHead(key, tc.headSeq, rows[tc.headSeq-1].EntryHash)
			} else {
				f.chains.heads[key] = &model.ChainHead{ChainKey: key}
			}
			f.putPolicy(&model.RetentionPolicy{PolicyID: 1, ActionDomain: "media",
				HotDays: 90, ArchiveAfterDays: 30, State: model.StateEnable, Version: 1})
			f.withOpts(func(o *repository.Options) { o.ArchiveMaxEntries = 10 })

			reply, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
				ArchiveAuditEntries(goodArchiveReq("req-arc-range", key, tc.from, tc.to, false))
			if tc.wantErr != "" {
				if err == nil || !errors.Is(err, model.ErrArchiveRangeInvalid) {
					t.Fatalf("err = %v，期望 ErrArchiveRangeInvalid(%s)", err, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误未说明 %q：%v", tc.wantErr, err)
				}
				if got := len(f.archives.byID); got != 0 {
					t.Fatalf("区间非法仍建了 %d 个批次", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := reply.GetBatch()
			if got.GetFromSeq() != tc.wantFrom || got.GetToSeq() != tc.wantTo ||
				got.GetRowCount() != tc.wantCount {
				t.Fatalf("实际归档区间 = [%d,%d] count=%d，期望 [%d,%d] count=%d",
					got.GetFromSeq(), got.GetToSeq(), got.GetRowCount(),
					tc.wantFrom, tc.wantTo, tc.wantCount)
			}
			// 夹取后的区间必须与对象键一致：否则清单指向的文件与台账说的不是一段。
			manifest, _ := archiveObjectKeys(key, tc.wantFrom, tc.wantTo)
			if got.GetObjectKey() != manifest {
				t.Fatalf("object_key = %s，期望 %s", got.GetObjectKey(), manifest)
			}
			// 回参必须如实回报夹取结果（调用方靠它决定下一片从哪开始），且不是 reused。
			if reply.GetReused() {
				t.Fatal("新建批次不得报 reused")
			}
		})
	}
}

// 单批条数上限取 Archive.MaxEntriesPerBatch，而不是在线校验的 Query.MaxVerifyEntries。
// 两个数一旦混用，就会出现「归档作业按 5 万条分片、校验接口只肯验 5 万条」的互相甩锅。
func TestArchiveAuditEntriesBatchLimitFollowsArchiveConfig(t *testing.T) {
	const key = "media/2026-03-05"
	f := newFixture(t)
	f.use(t)
	rows := f.seedArchiveSubject(key, 4, 30)
	// 判别前提：整段都已过归档线，此时唯一能挡住的就是条数上限本身。
	if due := newestOccurredAt(rows) + 30*archiveDay; due > testNow {
		t.Fatalf("种子区间未到期（eligible_at=%d > now=%d），本用例判的就不再是上限", due, testNow)
	}
	f.withOpts(func(o *repository.Options) {
		o.ArchiveMaxEntries = 3
		o.MaxVerifyEntries = 2 // 刻意设得比归档上限小：证明用的是归档那一把尺子
	})

	// 4 条 > 上限 3：整段拒绝，且是在读链头/读策略之后、建批次之前拒的（不产生批次）。
	_, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
		ArchiveAuditEntries(goodArchiveReq("req-arc-too-big", key, 1, 4, false))
	if !errors.Is(err, model.ErrArchiveRangeTooLarge) {
		t.Fatalf("err = %v，期望 ErrArchiveRangeTooLarge", err)
	}
	if !strings.Contains(err.Error(), "请分片") {
		t.Fatalf("超限错误必须要求分片：%v", err)
	}
	if got := len(f.archives.byID); got != 0 {
		t.Fatalf("超限请求建了 %d 个批次", got)
	}
	if got := len(f.archiveTrail(t)); got != 0 {
		t.Fatalf("超限请求写了 %d 条留痕", got)
	}
	// 正好等于上限：放过（差一即拒会让分片算法永远差最后一档）。
	reply, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
		ArchiveAuditEntries(goodArchiveReq("req-arc-at-limit", key, 1, 3, false))
	if err != nil {
		t.Fatalf("等于上限应放过：%v", err)
	}
	if got := reply.GetBatch().GetRowCount(); got != 3 {
		t.Fatalf("row_count = %d，期望 3", got)
	}
	// 上限配成 0/负数也要能兜到 1 而不是「一次搬走 0 条」的假成功。
	f.withOpts(func(o *repository.Options) { o.ArchiveMaxEntries = 0 })
	_, err = NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
		ArchiveAuditEntries(goodArchiveReq("req-arc-limit-zero", key, 1, 2, false))
	if !errors.Is(err, model.ErrArchiveRangeTooLarge) {
		t.Fatalf("上限为 0 时应夹到 1 并拒绝 2 条区间，实得 %v", err)
	}
	// 单条区间仍能过。
	if _, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
		ArchiveAuditEntries(goodArchiveReq("req-arc-limit-one", key, 1, 1, false)); err != nil {
		t.Fatalf("夹到 1 后单条区间应放过：%v", err)
	}
	if got := len(f.archiveTrail(t)); got == 0 {
		t.Fatal("成功批次没有留痕")
	}
}

// --- 5. 整段证据不足时一律 failed ---

func TestArchiveAuditEntriesRefusesUnsealedOrBrokenSegments(t *testing.T) {
	const key = "media/2026-03-05"
	cases := []struct {
		name          string
		setup         func(*fixture, []*model.AuditEntry)
		wantErr       error  // 回给调用方的错误必须是这个哨兵（fail() 原样返回 cause）
		wantCauseText string // 回参错误文本必须点出的定位信息（作业靠它决定怎么分片/重投）
		wantMsg       string // 落在批次 err_msg 里的阶段说明
		wantMoves     []string
	}{
		{
			name: "区间未到期",
			setup: func(f *fixture, _ []*model.AuditEntry) {
				f.policies.byDomain["media"].ArchiveAfterDays = 60 // 40 天的区间还没到线
			},
			wantErr: model.ErrArchiveNotDue, wantCauseText: "archive_after_days=60",
			wantMsg:   "区间未到期",
			wantMoves: []string{"pending→writing", "writing→failed"},
		},
		{
			name: "正好到期（边界放过）",
			setup: func(f *fixture, _ []*model.AuditEntry) {
				f.policies.byDomain["media"].ArchiveAfterDays = 40
			},
			// now == eligible_at 恰好放过（helpers.go 里判的是 now < eligible_at）：
			// 收严成「必须多等一天」这一例就要变红。
			wantMoves: []string{"pending→writing", "writing→verified", "verified→purged"},
		},
		{
			name: "区间有洞（行数与序号区间不符）",
			setup: func(f *fixture, rows []*model.AuditEntry) {
				// 热表里少了 seq=2 那一行：不是篡改，是被物理删掉了，同样不许整段搬走。
				var kept []*model.AuditEntry
				for _, r := range rows {
					if r.Seq != 2 {
						kept = append(kept, r)
					}
				}
				f.entries.rangeRows = kept
			},
			wantErr: model.ErrArchiveRangeGap, wantCauseText: "取到 2 行，期望 3 行",
			wantMsg:   "区间行数与序号区间不符",
			wantMoves: []string{"pending→writing", "writing→failed"},
		},
		{
			name: "链被改写（重放不过）",
			setup: func(_ *fixture, rows []*model.AuditEntry) {
				rows[1].Reason = "事后改写的原因" // 参与哈希的列被改，entry_hash 立刻复算不上
			},
			wantErr: model.ErrArchiveRangeGap, wantCauseText: "seq=2 entry_id=302 reason=entry_hash_mismatch",
			wantMsg:   "哈希链重放未通过",
			wantMoves: []string{"pending→writing", "writing→failed"},
		},
		{
			name: "prev_hash 被接错",
			setup: func(_ *fixture, rows []*model.AuditEntry) {
				rows[2].PrevHash = rows[0].EntryHash
			},
			wantErr: model.ErrArchiveRangeGap, wantCauseText: "seq=3 entry_id=303 reason=prev_hash_mismatch",
			wantMsg:   "哈希链重放未通过",
			wantMoves: []string{"pending→writing", "writing→failed"},
		},
		{
			name: "区间读取失败（上游错误原样上抛）",
			setup: func(f *fixture, _ []*model.AuditEntry) {
				f.entries.rangeErr = errors.New("audit_entry ListByChainRange: lost connection")
			},
			// fail() 返回的是 cause：这里没有任何业务哨兵，调用方拿到的就是驱动原文。
			wantCauseText: "lost connection",
			wantMsg:       "读取归档区间失败",
			wantMoves:     []string{"pending→writing", "writing→failed"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			rows := f.seedArchiveSubject(key, 3, 30)
			if tc.setup != nil {
				tc.setup(f, rows)
			}
			// 归档整段 [1,3]：purge_hot=true，好让「证据不完整」也顺带钉住不许清热表。
			_, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
				ArchiveAuditEntries(goodArchiveReq("req-arc-evidence", key, 1, 3, true))

			if tc.wantMsg == "" {
				// 「正好到期」那一例是成功路径。
				if err != nil {
					t.Fatalf("正好到期应放过：%v", err)
				}
				if got := len(f.store.created); got != 2 {
					t.Fatalf("清单+条目对象 = %d，期望 2：%v", got, f.store.created)
				}
				assertMoves(t, f, tc.wantMoves)
				return
			}
			if err == nil {
				t.Fatal("证据不完整的区间必须拒绝归档")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if tc.wantCauseText != "" && !strings.Contains(err.Error(), tc.wantCauseText) {
				t.Fatalf("回参错误没点出定位信息 %q：%v", tc.wantCauseText, err)
			}
			batch := assertFailedBatch(t, f, 1, tc.wantMsg)
			// 台账里的 err_msg 必须同时带上阶段说明与原因原文：只留一句「归档失败」无从排障。
			if tc.wantErr != nil && !strings.Contains(batch.ErrMsg, err.Error()) {
				t.Fatalf("err_msg = %q，应逐字带上回参错误 %v", batch.ErrMsg, err)
			}
			assertMoves(t, f, tc.wantMoves)
			// 未通过证据检查的批次不许留下任何产物引用：桶里根本没有文件。
			assertNoArtifactReference(t, batch)
			// 未通过证据检查的批次绝不允许把热表行标成已归档：那会让在线查询立刻看不见这段。
			if got := len(f.entries.markCalls); got != 0 {
				t.Fatalf("证据不完整仍标记了热表：%v", f.entries.markCalls)
			}
			for _, row := range f.chainOf(key) {
				if row.ArchivedAt != 0 {
					t.Fatalf("链上第 %d 行被提前标成已归档", row.Seq)
				}
			}
			if got := len(f.store.created); got != 0 {
				t.Fatalf("证据不完整仍写了对象：%v", f.store.created)
			}
			// 失败也要留痕（result=error）：归档台账必须能回答「这批为什么没搬走」。
			trail := f.archiveTrail(t)
			if len(trail) == 0 {
				t.Fatal("归档失败没写任何留痕")
			}
			last := trail[len(trail)-1]
			if last.Action != actionArchiveVerified {
				t.Fatalf("失败留痕 action = %s，期望 %s", last.Action, actionArchiveVerified)
			}
			if last.Result != model.ResultError {
				t.Fatalf("失败留痕 result = %d，期望 error(3)", last.Result)
			}
			assertDims(t, "失败留痕 after", last.AfterDigest, map[string]string{"state": model.BatchStateFailed})
		})
	}
}

func assertMoves(t *testing.T, f *fixture, want []string) {
	t.Helper()
	if !reflect.DeepEqual(f.archives.moves, want) {
		t.Fatalf("状态推进序列 = %v，期望 %v", f.archives.moves, want)
	}
}

// 段前行不存在时不拿创世摘要冒充锚点：replayChain 会跳过第一行的 prev 判定。
// 这是「没有锚点就是没有」的现状（与 VerifyAuditChain 同一口径），钉住它是为了
// 让「将来想收严成必须有锚点」这件事一定会让本用例变红。
func TestArchiveAuditEntriesWithoutAnchorSkipsFirstPrevCheck(t *testing.T) {
	const key = "media/2026-03-05"
	f := newFixture(t)
	f.use(t)
	rows := f.seedArchiveSubject(key, 3, 30)
	// 热表里删掉 seq=1（模拟历史已被 DBA 物理清理），归档 [2,3]。
	var kept []*model.AuditEntry
	for _, r := range rows {
		if r.Seq >= 2 {
			kept = append(kept, r)
		}
	}
	f.entries.rangeRows = kept
	delete(f.entries.byID, rows[0].EntryID)

	reply, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
		ArchiveAuditEntries(goodArchiveReq("req-arc-noanchor", key, 2, 3, false))
	if err != nil {
		t.Fatalf("无锚点时现状是「跳过第一行 prev 判定」而不是拒绝归档：%v", err)
	}
	if reply.GetBatch().GetState() != model.BatchStateVerified {
		t.Fatalf("批次状态 = %s", reply.GetBatch().GetState())
	}
	// 锚点读的是 [1,1] 那一行，limit=1；读不到就退回「无锚点」而不是报错。
	if got := f.entries.rangeCalls; !reflect.DeepEqual(got, []string{key + ":2-3", key + ":1-1"}) {
		t.Fatalf("区间读顺序 = %v", got)
	}
}

// --- 6. 对象存储：失败与不自证 ---

func TestArchiveAuditEntriesStopsOnObjectWriteFailure(t *testing.T) {
	const key = "media/2026-03-05"
	manifestKey, entriesKey := archiveObjectKeys(key, 1, 3)
	cases := []struct {
		name          string
		setup         func(*fixture)
		wantMsg       string
		wantErrIs     error // 必须原样认得的上游错误
		wantErrUnLike error // 不许被换成的不透明哨兵
		wantCreated   []string
		wantAborted   []string
		wantCommit    []string // 允许「已经提交」的对象：清单已落地而条目文件失败是真实半程状态
	}{
		{
			name: "清单对象创建被拒",
			// 桶策略拒绝 put：形状是一条驱动原文错误，不带任何本服务哨兵。
			setup:         func(f *fixture) { f.store.createErr = errors.New("s3: bucket policy denies put") },
			wantMsg:       "创建归档清单对象失败",
			wantErrUnLike: model.ErrObjectStorageUnsupported,
		},
		{
			name: "清单写入断流",
			setup: func(f *fixture) {
				f.deps.storage = lieStore{fakeStore: f.store, target: manifestKey,
					writeErr: errors.New("s3: put part broken pipe")}
			},
			wantMsg:     "写归档清单失败",
			wantCreated: []string{manifestKey},
			wantAborted: []string{manifestKey},
		},
		{
			name: "清单提交失败",
			setup: func(f *fixture) {
				f.deps.storage = lieStore{fakeStore: f.store, target: manifestKey,
					commitErr: errors.New("s3: complete multipart: 503 SlowDown")}
			},
			wantMsg:     "提交归档清单失败",
			wantCreated: []string{manifestKey},
			wantAborted: []string{manifestKey},
		},
		{
			name: "条目对象创建被拒（清单已落地，批次仍须 failed）",
			setup: func(f *fixture) {
				f.store.failKey = entriesKey
			},
			wantMsg:     "创建归档条目对象失败",
			wantErrIs:   model.ErrObjectStorageUnsupported,
			wantCreated: []string{manifestKey},
			wantCommit:  []string{manifestKey},
		},
		{
			name: "条目内容写入断流",
			setup: func(f *fixture) {
				f.deps.storage = lieStore{fakeStore: f.store, target: entriesKey,
					writeErr: errors.New("s3: entries object broken pipe")}
			},
			wantMsg:     "写归档条目对象失败",
			wantCreated: []string{manifestKey, entriesKey},
			wantCommit:  []string{manifestKey},
			wantAborted: []string{entriesKey},
		},
		{
			name: "条目对象提交失败",
			setup: func(f *fixture) {
				f.deps.storage = lieStore{fakeStore: f.store, target: entriesKey,
					commitErr: errors.New("s3: entries complete multipart refused")}
			},
			wantMsg:     "提交归档条目对象失败",
			wantCreated: []string{manifestKey, entriesKey},
			wantCommit:  []string{manifestKey},
			wantAborted: []string{entriesKey},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			f.seedArchiveSubject(key, 3, 30)
			tc.setup(f)
			_, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
				ArchiveAuditEntries(goodArchiveReq("req-arc-obj-"+tc.name, key, 1, 3, true))
			if err == nil {
				t.Fatal("对象写不进去必须报错：清单没落地就不许报成功")
			}
			if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("err = %v，期望原样带上 %v", err, tc.wantErrIs)
			}
			if tc.wantErrUnLike != nil && errors.Is(err, tc.wantErrUnLike) {
				t.Fatalf("上游错误被换成了不透明哨兵 %v：%v", tc.wantErrUnLike, err)
			}
			// 阶段说明落在批次 err_msg 里（回参是上游 cause），两者都要对。
			batch := assertFailedBatch(t, f, 1, tc.wantMsg)
			assertNoArtifactReference(t, batch)
			assertMoves(t, f, []string{"pending→writing", "writing→failed"})

			// 桶里的实际痕迹必须与期望一模一样：多一个对象、少一次 Abort 都是「台账与桶不一致」。
			if !reflect.DeepEqual(f.store.created, tc.wantCreated) {
				t.Fatalf("创建过的对象 = %v，期望 %v", f.store.created, tc.wantCreated)
			}
			var gotAborted, gotCommitted []string
			for _, k := range f.store.created {
				o := f.store.objects[k]
				if o.aborted {
					gotAborted = append(gotAborted, k)
				}
				if o.committed {
					gotCommitted = append(gotCommitted, k)
				}
			}
			if !reflect.DeepEqual(gotAborted, tc.wantAborted) {
				t.Fatalf("被 Abort 的对象 = %v，期望 %v（半成品必须放弃，否则桶里留下半截清单）",
					gotAborted, tc.wantAborted)
			}
			if !reflect.DeepEqual(gotCommitted, tc.wantCommit) {
				t.Fatalf("已提交的对象 = %v，期望 %v", gotCommitted, tc.wantCommit)
			}
			if got := len(f.entries.markCalls); got != 0 {
				t.Fatalf("产物不完整仍标记热表：%v", f.entries.markCalls)
			}
			for _, row := range f.chainOf(key) {
				if row.ArchivedAt != 0 {
					t.Fatalf("产物不完整却把第 %d 行标成已归档", row.Seq)
				}
			}
			// 留痕：一条 writing（成功）+ 一条 verified 阶段的 error 痕。
			trail := f.archiveTrail(t)
			if len(trail) != 2 {
				t.Fatalf("留痕 %d 条，期望 2：%+v", len(trail), trail)
			}
			if trail[0].Action != actionArchiveWriting || trail[0].Result != model.ResultOK {
				t.Fatalf("第一条痕应为 ok 级 writing：%+v", trail[0])
			}
			if trail[1].Action != actionArchiveVerified || trail[1].Result != model.ResultError {
				t.Fatalf("第二条痕应为 error 级 verified：%+v", trail[1])
			}
			assertDims(t, "失败痕 after", trail[1].AfterDigest, map[string]string{"state": model.BatchStateFailed})
		})
	}
}

// 对象侧自证不合格：三类形状都要停在 writing→failed，并且错误被换成不透明哨兵。
func TestArchiveAuditEntriesRejectsUnselfProvingObjects(t *testing.T) {
	const key = "media/2026-03-05"
	manifestKey, entriesKey := archiveObjectKeys(key, 1, 3)
	cases := []struct {
		name    string
		store   lieStore
		wantErr error
		wantMsg string
	}{
		{"清单摘要不一致（对象侧多了一截）", lieStore{target: manifestKey, tamper: []byte("appended-by-bad-actor")},
			model.ErrBatchUnverified, "清单摘要不一致"},
		{"清单对象没回摘要", lieStore{target: manifestKey, blankSHA: true},
			model.ErrObjectStorageUnsupported, "清单对象自证不完整"},
		{"清单对象换了键名", lieStore{target: manifestKey, fakeKey: "audit-archive/stolen.manifest.csv"},
			model.ErrObjectStorageUnsupported, "清单对象自证不完整"},
		{"条目对象没回摘要", lieStore{target: entriesKey, blankSHA: true},
			model.ErrObjectStorageUnsupported, "条目对象自证不完整"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			f.seedArchiveSubject(key, 3, 30)
			st := tc.store
			st.fakeStore = f.store
			f.deps.storage = st
			_, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
				ArchiveAuditEntries(goodArchiveReq("req-arc-lie-"+tc.name, key, 1, 3, true))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			batch := assertFailedBatch(t, f, 1, tc.wantMsg)
			// 自证不合格的清单不能留在台账上当作凭证。
			assertNoArtifactReference(t, batch)
			assertMoves(t, f, []string{"pending→writing", "writing→failed"})
			if got := len(f.entries.markCalls); got != 0 {
				t.Fatalf("清单不可信仍标记热表：%v", f.entries.markCalls)
			}
			if got := len(f.archiveTrail(t)); got != 2 {
				t.Fatalf("留痕 %d 条，期望 2", got)
			}
		})
	}
}

// --- 7. purge_hot：只标记 archived_at，且必须有已校验清单 ---

func TestArchiveAuditEntriesPurgeHot(t *testing.T) {
	const key = "media/2026-03-05"

	t.Run("purge_hot=false 只归档不标记", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		rows := f.seedArchiveSubject(key, 3, 30)
		reply, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
			ArchiveAuditEntries(goodArchiveReq("req-arc-nopurge", key, 1, 3, false))
		if err != nil {
			t.Fatal(err)
		}
		if got := reply.GetBatch().GetState(); got != model.BatchStateVerified {
			t.Fatalf("不清热表时状态 = %s，期望 verified", got)
		}
		if got := len(f.entries.markCalls); got != 0 {
			t.Fatalf("purge_hot=false 却标记了热表：%v", f.entries.markCalls)
		}
		for _, r := range f.chainOf(key) {
			if r.ArchivedAt != 0 {
				t.Fatalf("链上第 %d 行被标成已归档", r.Seq)
			}
		}
		if got := len(f.archiveTrail(t)); got != 2 {
			t.Fatalf("留痕 %d 条，期望 writing + verified", got)
		}
		_ = rows
	})

	t.Run("purge 未装配时停在 verified 而不伪造 purged", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		f.seedArchiveSubject(key, 3, 30)
		f.deps.purge = nil
		reply, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
			ArchiveAuditEntries(goodArchiveReq("req-arc-nopurgemgr", key, 1, 3, true))
		if reply != nil {
			t.Fatalf("失败时不得回响应：%+v", reply)
		}
		if !errors.Is(err, model.ErrBatchUnverified) {
			t.Fatalf("err = %v，期望 ErrBatchUnverified（不能标记就不能报已清热表）", err)
		}
		if !strings.Contains(err.Error(), "未装配 PurgeMark") {
			t.Fatalf("错误未点明缺装配：%v", err)
		}
		// 清单是真的、热表行还在：状态必须停在 verified，不是 failed。
		if got := f.mustBatch(t, 1); got.State != model.BatchStateVerified {
			t.Fatalf("批次状态 = %s，期望 verified", got.State)
		}
		assertMoves(t, f, []string{"pending→writing", "writing→verified"})
		if got := len(f.archiveTrail(t)); got != 2 {
			t.Fatalf("留痕 %d 条，期望 2", got)
		}
	})

	t.Run("标记热表失败停在 verified", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		f.seedArchiveSubject(key, 3, 30)
		f.entries.markErr = errors.New("audit_entry MarkArchived: Error 1213 deadlock")
		_, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
			ArchiveAuditEntries(goodArchiveReq("req-arc-markfail", key, 1, 3, true))
		if err == nil || !strings.Contains(err.Error(), "MarkArchived") {
			t.Fatalf("标记失败的原始错误必须原样上抛：%v", err)
		}
		if errors.Is(err, model.ErrBatchUnverified) {
			t.Fatal("上游故障不许被翻译成业务哨兵")
		}
		row := f.mustBatch(t, 1)
		if row.State != model.BatchStateVerified {
			t.Fatalf("批次状态 = %s，期望停在 verified（清单是真的，热表行也还在）", row.State)
		}
		if row.ManifestHash == "" {
			t.Fatal("停在 verified 的批次必须保留清单摘要，否则事后无从判断丢在哪一步")
		}
		assertMoves(t, f, []string{"pending→writing", "writing→verified"})
		for _, r := range f.chainOf(key) {
			if r.ArchivedAt != 0 {
				t.Fatalf("标记失败却写进了 archived_at：seq=%d", r.Seq)
			}
		}
		trail := f.archiveTrail(t)
		last := trail[len(trail)-1]
		if last.Action != actionArchivePurged || last.Result != model.ResultError {
			t.Fatalf("清热表失败必须留一条 error 级 purged 痕：%+v", last)
		}
		assertDims(t, "purged 失败痕 after", last.AfterDigest, map[string]string{"state": model.BatchStateVerified})
	})

	t.Run("行已标记而 purged 未命中（现状：只告警不回滚）", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		f.seedArchiveSubject(key, 3, 30)
		f.deps.archives = stuckArchives{fakeArchives: f.archives, blockTo: model.BatchStatePurged}
		reply, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
			ArchiveAuditEntries(goodArchiveReq("req-arc-purgeclaim", key, 1, 3, true))
		if reply != nil {
			t.Fatalf("失败时不得回响应：%+v", reply)
		}
		if !errors.Is(err, model.ErrTaskBadTransition) {
			t.Fatalf("err = %v，期望 ErrTaskBadTransition", err)
		}
		// 现状：热表行的 archived_at 已经写下去了，批次却仍停在 verified —— 实现只打 Error 日志，
		// 既不补偿推进也不回滚标记（回滚会把「已经搬走」的事实抹掉，那更糟）。
		row := f.mustBatch(t, 1)
		if row.State != model.BatchStateVerified {
			t.Fatalf("批次状态 = %s，期望 verified", row.State)
		}
		marked := 0
		for _, r := range f.chainOf(key) {
			if r.ArchivedAt != 0 {
				marked++
			}
		}
		if marked != 3 {
			t.Fatalf("标记数 = %d，期望 3（本用例钉的就是「行已标记而状态没推进」这个窗口）", marked)
		}
		trail := f.archiveTrail(t)
		last := trail[len(trail)-1]
		if last.Action != actionArchivePurged || last.Result != model.ResultError {
			t.Fatalf("必须留一条 error 级 purged 痕：%+v", last)
		}
		assertDims(t, "purged 未命中痕 after", last.AfterDigest, map[string]string{
			"state": model.BatchStateVerified, "marked": "3",
		})
	})
}

// --- 8. 正常路径：逐字段投影 + 按顺序的副作用序列 ---

func TestArchiveAuditEntriesHappyPath(t *testing.T) {
	const key = "media/2026-03-05"
	f := newFixture(t)
	f.use(t)
	rows := f.seedArchiveSubject(key, 3, 30)
	manifestKey, entriesKey := archiveObjectKeys(key, 2, 3)
	rec := f.traceAll()

	reply, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
		ArchiveAuditEntries(goodArchiveReq("req-arc-ok", key, 2, 3, true))
	if err != nil {
		t.Fatal(err)
	}
	if reply.GetReused() {
		t.Fatal("首次归档不得报 reused")
	}

	// 副作用序列：先读链头定区间 → 读策略判到期 → 幂等预检 → 覆盖预检 → 落批次 →
	// 推 writing → 写痕 → 读区间 → 读段前锚点 → 落两个对象 → 推 verified → 写痕 →
	// 标记热表 → 推 purged → 写痕 → 回读台账。顺序即契约，任何一步提前或推后都会留下窗口。
	rec.assert(t,
		"chains.FindOne:"+key,
		"policies.FindEffective:media",
		"archives.FindByRequestID:req-arc-ok",
		"archives.FindCovering:"+key+":2-3",
		"archives.Insert:"+key+":2-3",
		"archives.TransitionState:1:pending→writing",
		"entries.Insert:"+actionArchiveWriting,
		fmt.Sprintf("entries.ListByChainRange:%s:2-3 limit=3", key),
		fmt.Sprintf("entries.ListByChainRange:%s:1-1 limit=1", key),
		"storage.CreateObject:"+f.opts.Storage.Bucket+"/"+manifestKey,
		"storage.CreateObject:"+f.opts.Storage.Bucket+"/"+entriesKey,
		"archives.TransitionState:1:writing→verified",
		"entries.Insert:"+actionArchiveVerified,
		fmt.Sprintf("purge:%s:2-3@%d", key, testNow),
		"archives.TransitionState:1:verified→purged",
		"entries.Insert:"+actionArchivePurged,
		"archives.FindOne:1",
	)

	got := reply.GetBatch()
	// 逐字段投影：种子值全部互不相同，取错数据源立刻可见。
	// 尤其 operator_id/trace_id 取的是 CallContext，而不是链上条目的 actor_id/trace_id。
	if got.GetBatchId() != 1 {
		t.Fatalf("batch_id = %d，期望 1", got.GetBatchId())
	}
	if got.GetRequestId() != "req-arc-ok" || got.GetChainKey() != key {
		t.Fatalf("幂等键/链名投影异常：%+v", got)
	}
	if got.GetFromSeq() != 2 || got.GetToSeq() != 3 || got.GetRowCount() != 2 {
		t.Fatalf("区间投影 = [%d,%d] count=%d，期望 [2,3] count=2",
			got.GetFromSeq(), got.GetToSeq(), got.GetRowCount())
	}
	if got.GetBucket() != f.opts.Storage.Bucket || got.GetObjectKey() != manifestKey {
		t.Fatalf("产物引用 = %s/%s，期望 %s/%s", got.GetBucket(), got.GetObjectKey(),
			f.opts.Storage.Bucket, manifestKey)
	}
	wantManifestHash := manifestHash(rows[1:])
	if got.GetManifestHash() != wantManifestHash || !isHashHex(got.GetManifestHash()) {
		t.Fatalf("manifest_hash = %s，期望本地清单摘要 %s", got.GetManifestHash(), wantManifestHash)
	}
	if got.GetLastEntryHash() != rows[2].EntryHash {
		t.Fatalf("last_entry_hash = %s，期望区间尾行 %d 的 entry_hash %s",
			got.GetLastEntryHash(), rows[2].Seq, rows[2].EntryHash)
	}
	if got.GetState() != model.BatchStatePurged {
		t.Fatalf("终态 = %s，期望 purged", got.GetState())
	}
	if got.GetOperatorId() != 7 || got.GetTraceId() != "trace-1" {
		t.Fatalf("归因投影 = operator %d trace %q，期望 7 / trace-1", got.GetOperatorId(), got.GetTraceId())
	}
	if got.GetErrMsg() != "" {
		t.Fatalf("成功批次带着 err_msg：%q", got.GetErrMsg())
	}
	if got.GetFinishedAt() != testNow {
		t.Fatalf("finished_at = %d，期望 %d", got.GetFinishedAt(), testNow)
	}
	// 台账与回参同源：批次行必须和回参一模一样（防止「回参美化、库里另一套」）。
	stored := f.mustBatch(t, 1)
	if !reflect.DeepEqual(got, archiveBatchView(stored)) {
		t.Fatalf("回参与库内批次不一致：\n回参 %+v\n库内 %+v", got, stored)
	}

	// 两个对象的实际内容：清单是「这段链被完整搬走了」的证据，条目文件是查询侧回读的来源。
	manifestObj := f.store.objects[manifestKey]
	if manifestObj == nil || !manifestObj.committed || manifestObj.aborted {
		t.Fatalf("清单对象未正常落地：%+v", manifestObj)
	}
	wantLines := strings.Builder{}
	for _, r := range rows[1:] {
		wantLines.WriteString(strconv.FormatInt(r.EntryID, 10) + "," + strconv.FormatInt(r.Seq, 10) +
			"," + r.EntryHash + "\n")
	}
	if string(manifestObj.buf) != wantLines.String() {
		t.Fatalf("清单内容不符：\n实得 %q\n期望 %q", string(manifestObj.buf), wantLines.String())
	}
	entriesObj := f.store.objects[entriesKey]
	if entriesObj == nil || !entriesObj.committed {
		t.Fatalf("条目对象未落地：%+v", entriesObj)
	}
	lines := strings.Split(strings.TrimSuffix(string(entriesObj.buf), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("条目对象行数 = %d，期望 2（只搬 [2,3] 这一段）", len(lines))
	}
	for i, line := range lines {
		if !strings.Contains(line, strconv.FormatInt(rows[1+i].EntryID, 10)) {
			t.Fatalf("条目对象第 %d 行不是区间内那一行：%s", i, line)
		}
	}
	// 归档不碰导出任务表（两个通道各有各的台账）。
	if n := len(f.exports.byID); n != 0 {
		t.Fatalf("归档写了 %d 行导出任务", n)
	}

	// 三条阶段留痕逐条核对：动作域、对象类型、目标 ID、前后维度、结果码、actor 归因。
	sysKey := model.ChainKey(selfDomainSystem, testNow)
	before := map[string]string{"chain_key": key, "from_seq": "2", "to_seq": "3"}
	wantTrails := []struct {
		action string
		after  map[string]string
		reason string
	}{
		{actionArchiveWriting, map[string]string{"state": model.BatchStateWriting}, "开始归档链区间"},
		{actionArchiveVerified, map[string]string{
			"manifest_hash": wantManifestHash, "last_entry_hash": rows[2].EntryHash, "row_count": "2",
		}, "归档清单校验通过"},
		{actionArchivePurged, map[string]string{"marked": "2", "row_count": "2"}, "热表行已标记 archived_at"},
	}
	trails := f.archiveTrail(t)
	if len(trails) != len(wantTrails) {
		t.Fatalf("留痕 %d 条，期望 %d 条", len(trails), len(wantTrails))
	}
	batchIDText := strconv.FormatInt(stored.BatchID, 10)
	for i, want := range wantTrails {
		row := trails[i]
		if row.Action != want.action {
			t.Fatalf("第 %d 条留痕 action = %s，期望 %s", i, row.Action, want.action)
		}
		if row.ActionDomain != selfDomainSystem || row.ChainKey != sysKey {
			t.Fatalf("第 %d 条留痕不在 system 动作域：%+v", i, row)
		}
		if row.TargetType != targetArchiveBatch || row.TargetID != batchIDText {
			t.Fatalf("第 %d 条留痕定位 = %s/%s，期望 %s/%s", i,
				row.TargetType, row.TargetID, targetArchiveBatch, batchIDText)
		}
		if row.Reason != want.reason {
			t.Fatalf("第 %d 条留痕 reason = %q，期望 %q", i, row.Reason, want.reason)
		}
		if row.Result != model.ResultOK {
			t.Fatalf("第 %d 条留痕 result = %d，期望 ok(1)", i, row.Result)
		}
		if row.ActorType != int32(rpc.ActorType_ACTOR_TYPE_ADMIN) || row.ActorID != 7 {
			t.Fatalf("第 %d 条留痕 actor = %d/%d，期望 admin(1)/7", i, row.ActorType, row.ActorID)
		}
		if row.SourceApp != model.SourceInternalRPC || row.CallerService != selfCallerService {
			t.Fatalf("第 %d 条留痕归因 = %s/%d，期望 audit/internal_rpc", i, row.CallerService, row.SourceApp)
		}
		assertDims(t, fmt.Sprintf("第 %d 条留痕 before", i), row.BeforeDigest, before)
		assertDims(t, fmt.Sprintf("第 %d 条留痕 after", i), row.AfterDigest, want.after)
		// event_id 以动作为前缀、以哈希为后缀：同一秒内的两条同阶段痕不会互相顶掉。
		if !strings.HasPrefix(row.EventID, want.action+"/") {
			t.Fatalf("第 %d 条留痕 event_id = %s", i, row.EventID)
		}
	}
	// 摘要列只收 16hex 白名单摘要：清单摘要本身也是被压过摘要写进去的（列宽 255 放不下
	// 两个 64hex + 其它维度），核对过 assertDims 的等值比对，这里再钉一次「没抄原文」。
	if strings.Contains(trails[1].AfterDigest, wantManifestHash) {
		t.Fatalf("verified 留痕把 64hex 清单摘要原文抄进了摘要列：%q", trails[1].AfterDigest)
	}

	// 热表行：只被标记 archived_at，参与哈希的列一个都没动。
	for _, r := range f.chainOf(key) {
		seeded := rows[r.Seq-1]
		if r.EntryHash != seeded.EntryHash || r.PrevHash != seeded.PrevHash || r.Reason != seeded.Reason {
			t.Fatalf("归档改写了链上第 %d 行的哈希参与列", r.Seq)
		}
		if r.Seq >= 2 {
			if r.ArchivedAt != testNow {
				t.Fatalf("第 %d 行 archived_at = %d，期望 %d", r.Seq, r.ArchivedAt, testNow)
			}
		} else if r.ArchivedAt != 0 {
			t.Fatalf("第 %d 行不在归档区间里却被标成已归档", r.Seq)
		}
	}
	// 归档后的链仍然自证完整（archived_at 不参与哈希）。
	f.entries.rangeRows = f.chainOf(key)
	if v := verifyChain(t, "req-arc-after-verify", key, 0, 0); !v.GetIntact() {
		t.Fatalf("归档后的链不自证完整：%+v", v)
	}
}

// 留痕写不进去时只打 Error 日志：批次状态已经落库，回滚会抹掉「清单已落地」这一事实。
// 这是刻意的设计（archiveauditentrieslogic.go 的 trail 注释），也是一处审计缺口：
// 归档做成了、台账上却没有痕，README「已知缺口」按域登记。
func TestArchiveAuditEntriesTrailFailureIsSwallowedAndBatchStaysVerified(t *testing.T) {
	const key = "media/2026-03-05"
	f := newFixture(t)
	f.use(t)
	f.seedArchiveSubject(key, 3, 30)
	flake := &trailFailingEntries{fakeEntries: f.entries, nth: 1}
	f.deps.entries = flake

	reply, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
		ArchiveAuditEntries(goodArchiveReq("req-arc-swaptrail", key, 1, 3, false))
	if err != nil {
		t.Fatalf("留痕写失败不得改成整次归档失败（清单已经真的落地了）：%v", err)
	}
	if got := reply.GetBatch().GetState(); got != model.BatchStateVerified {
		t.Fatalf("批次状态 = %s，期望 verified", got)
	}
	if flake.recorded == nil || flake.recorded.Action != actionArchiveWriting {
		t.Fatalf("本用例没能在 writing 留痕上造失败：%+v", flake.recorded)
	}
	// writing 那条痕丢了，verified 那条仍然写成功：痕迹缺一环而流程照常收敛。
	trails := f.archiveTrail(t)
	if len(trails) != 1 || trails[0].Action != actionArchiveVerified {
		t.Fatalf("留痕 = %+v，期望只剩 verified 一条（writing 那条被吞掉了）", trails)
	}
	// 吞错误但不静默：批次上的产物引用完好，说明「没痕」不等于「没做」。
	if f.mustBatch(t, 1).ManifestHash == "" {
		t.Fatal("清单摘要丢了：verified 批次必须能自证")
	}
}

// --- 9. 现状钉住：策略停用不会让归档跳过该域 ---

// 真实 FindEffective 的口径是「域策略不存在**或已停用**都回落到 default」
// （model/auditretentionpolicy.go:128-144）。因此把某个域的归档关掉，
// 实际效果是改用 default 的窗口继续归档，而不是跳过该域 ——
// 与 logic 里那条「策略已停用，归档作业跳过该域」的错误文案（archiveauditentrieslogic.go:98-101）
// 以及 RetentionPolicy.State 的列注释（model/auditretentionpolicy.go:34-35）正好相反。
// 本用例按现状钉住，若要收严（default 也不允许接管、或停用即拒归档）改哪里见 README。
func TestArchiveAuditEntriesDisabledPolicyFallsBackToDefault(t *testing.T) {
	const key = "media/2026-03-05"
	rows := seedAgedChain(key, 3, 300, archiveAgedNewest)

	// 两侧窗口刻意互相矛盾：被停用那一行的天数与 default 的天数给出相反结论，
	// 于是「放行 / 拒绝」直接暴露判定到底取了哪一行。
	cases := []struct {
		name        string
		mediaDays   int32
		defaultDays int32
		wantState   string
		wantErr     error
	}{
		{"停用 media(45) + default(30)：照常搬走", 45, 30, model.BatchStateVerified, nil},
		{"停用 media(30) + default(45)：按 default 判未到期", 30, 45, "", model.ErrArchiveNotDue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			f.putRows(rows...)
			f.entries.rangeRows = rows
			f.putHead(key, 3, rows[2].EntryHash)
			f.putPolicy(&model.RetentionPolicy{PolicyID: 1, ActionDomain: "media",
				HotDays: 90, ArchiveAfterDays: tc.mediaDays, State: model.StateDisable, Version: 1})
			f.seedDefaultPolicy(tc.defaultDays)
			f.deps.policies = realEffectivePolicies{fakePolicies: f.policies}

			reply, err := NewArchiveAuditEntriesLogic(context.Background(), svcCtx()).
				ArchiveAuditEntries(goodArchiveReq("req-arc-disabled", key, 1, 3, false))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
				}
				// 取的是 default 那一行的数值：错误文本里的天数就是证据。
				if !strings.Contains(err.Error(), fmt.Sprintf("archive_after_days=%d ", tc.defaultDays)) {
					t.Fatalf("到期判定没用 default 的天数（%d）：%v", tc.defaultDays, err)
				}
				// 现状：批次行在「到期判定」之前就已落库（archiveauditentrieslogic.go:137 早于 194），
				// 所以未到期拒绝不表现为零批次，而是一条 pending→failed 的批次 + 一条 ERROR 留痕。
				// 若要收严成「判定不过就不留批次行」，改的是这段先后顺序，不是本用例。
				batch := assertFailedBatch(t, f, 1, "区间未到期")
				assertNoArtifactReference(t, batch)
				if batch.FromSeq != 1 || batch.ToSeq != 3 {
					t.Fatalf("未到期批次记录的区间 = %d-%d，期望 1-3（判定用的是整段 default 窗口）",
						batch.FromSeq, batch.ToSeq)
				}
				return
			}
			if err != nil {
				t.Fatalf("现状：停用域策略不会挡住归档（回落 default 后照常搬走）：%v", err)
			}
			if got := reply.GetBatch().GetState(); got != tc.wantState {
				t.Fatalf("批次状态 = %s，期望 %s", got, tc.wantState)
			}
			// 搬走的确实是 default 那一行允许的整段：批次落了库、清单自证齐了。
			if f.mustBatch(t, 1).ManifestHash == "" {
				t.Fatal("回落 default 放行却没写清单摘要")
			}
		})
	}
}
