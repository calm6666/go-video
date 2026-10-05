package logic

// 追加式（write-once / no-update / no-delete）不变量。
//
// 审计服务的全部价值来自「写下去就不能改」。本文件把这条承诺拆成四个可断言层次，
// 逐层钉住，任何一层被削弱都会红：
//  1. 能力面：五个 model 接口的方法集合是封闭白名单——想加一条 UPDATE/DELETE 路径，
//     必须先改这张白名单，评审时一眼可见（反射断言，不靠人肉记得住）；
//  2. SQL 面：扫描 model 包源码，全包不存在 DELETE/TRUNCATE，audit_entry 上只有一条
//     UPDATE、只写 archived_at，并带 archived_at = 0 的写一次护栏；
//  3. 编排面：条目写入只有一条路径（helpers.appendOnce），事务失败必须整笔回滚，
//     链冲突重试不得留下「行写了、链头没推」或反过来的半成品；
//  4. 数据面：幂等重放不改已入库行；改列/删行/搬链/重编号这四类篡改，
//     VerifyAuditChain 必须检出并给出具体断点原因，绝不返回 intact=true。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/audit/internal/repository"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"
)

// --- 1. 能力面：接口方法白名单 ---

// interfaceMethodNames 取接口类型的方法名（升序）。
// 参数须是 reflect.TypeOf((*T)(nil)).Elem() 这种接口类型。
func interfaceMethodNames(tp reflect.Type) []string {
	if tp.Kind() != reflect.Interface {
		panic("interfaceMethodNames 需要接口类型，实得 " + tp.String())
	}
	out := make([]string, 0, tp.NumMethod())
	for i := 0; i < tp.NumMethod(); i++ {
		out = append(out, tp.Method(i).Name)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestModelInterfaceMethodSetsAreClosed 把五个 model 接口的能力面钉成白名单。
//
// audit_entry / audit_chain_head 是存证本体：接口里既没有 Delete 也没有 Update，
// 唯一的写后变更是 MarkArchived（只碰不参与哈希的 archived_at 列）。
// 导出任务与归档批次是「作业状态机」、保留策略是「运维配置」，它们允许推进自己的状态，
// 但同样不允许出现删除路径——台账本身不能被抹掉。
func TestModelInterfaceMethodSetsAreClosed(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
		want []string
	}{
		{
			name: "AuditEntryModel",
			typ:  reflect.TypeOf((*model.AuditEntryModel)(nil)).Elem(),
			want: []string{"FindByEventID", "FindByEventIDs", "FindOne", "Insert", "List",
				"ListByChainRange", "MarkArchived", "MaxSeq", "ScanAfter"},
		},
		{
			name: "AuditChainHeadModel",
			typ:  reflect.TypeOf((*model.AuditChainHeadModel)(nil)).Elem(),
			want: []string{"Advance", "Ensure", "FindOne", "List", "LockForUpdate"},
		},
		{
			name: "ExportTaskModel",
			typ:  reflect.TypeOf((*model.ExportTaskModel)(nil)).Elem(),
			want: []string{"AddProgress", "Claim", "FindByRequestID", "FindOne", "Finish",
				"Insert", "List", "TransitionState"},
		},
		{
			name: "ArchiveBatchModel",
			typ:  reflect.TypeOf((*model.ArchiveBatchModel)(nil)).Elem(),
			want: []string{"FindByRequestID", "FindCovering", "FindOne", "Insert", "List", "TransitionState"},
		},
		{
			name: "RetentionPolicyModel",
			typ:  reflect.TypeOf((*model.RetentionPolicyModel)(nil)).Elem(),
			want: []string{"FindEffective", "FindOne", "Insert", "List", "UpdateWithVersion"},
		},
	}
	banned := []string{"Delete", "Remove", "Erase", "Truncate", "Drop", "Purge", "Clear", "Wipe", "Destroy"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := interfaceMethodNames(tc.typ)
			if !equalStrings(got, tc.want) {
				t.Fatalf("%s 方法集漂移\n got=%v\nwant=%v", tc.name, got, tc.want)
			}
			for _, m := range got {
				for _, b := range banned {
					if strings.Contains(m, b) {
						t.Fatalf("%s 暴露了删除类能力 %s：审计台账不允许被抹掉", tc.name, m)
					}
				}
			}
		})
	}
	// 存证本体连「改业务字段」的入口都不该存在：MarkArchived 是唯一的写后变更。
	for _, m := range interfaceMethodNames(reflect.TypeOf((*model.AuditEntryModel)(nil)).Elem()) {
		if m == "MarkArchived" {
			continue
		}
		if strings.HasPrefix(m, "Update") || strings.HasPrefix(m, "Set") || strings.HasPrefix(m, "Modify") {
			t.Fatalf("AuditEntryModel 出现了改行方法 %s：只能再写一条补偿条目", m)
		}
	}
}

// --- 2. SQL 面：扫描真实 SQL 文本 ---

// TestAuditEntrySQLNeverDeletesAndUpdatesOnlyArchivedAt 扫描 model 包的 SQL 字面量。
//
// 接口白名单挡得住「有人加了个方法」，挡不住「方法名叫 FindOne 却偷偷 DELETE」。
// 因此这里直接对源码文本判定：全包零删除；audit_entry 上唯一的 UPDATE 只写 archived_at，
// 且带 archived_at = 0 护栏（同一区间不能被标记第二次）。
func TestAuditEntrySQLNeverDeletesAndUpdatesOnlyArchivedAt(t *testing.T) {
	dir := filepath.Join("..", "..", "model")
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读 model 目录失败：%v", err)
	}
	var entryUpdates []string
	var scanned int
	for _, fe := range files {
		if fe.IsDir() || !strings.HasSuffix(fe.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, fe.Name()))
		if err != nil {
			t.Fatalf("读 %s 失败：%v", fe.Name(), err)
		}
		scanned++
		up := strings.ToUpper(string(b))
		for _, banned := range []string{"DELETE FROM", "TRUNCATE TABLE", "TRUNCATE ", "DROP TABLE"} {
			if strings.Contains(up, banned) {
				t.Fatalf("model/%s 出现 %s：审计表没有删除路径", fe.Name(), strings.TrimSpace(banned))
			}
		}
		if fe.Name() != "auditentry.go" {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			lu := strings.ToUpper(line)
			if strings.Contains(lu, "UPDATE AUDIT_ENTRY") {
				entryUpdates = append(entryUpdates, strings.TrimSpace(line))
			}
			// SET 后面只允许 archived_at：出现任何哈希输入列就是能改存证内容。
			if i := strings.Index(lu, "SET "); i >= 0 {
				for _, col := range []string{"ENTRY_HASH", "PREV_HASH", "SEQ", "OCCURRED_AT", "REASON",
					"EVENT_ID", "ACTION", "BEFORE_DIGEST", "AFTER_DIGEST", "CHAIN_KEY"} {
					if strings.Contains(lu[i:], "SET "+col) {
						t.Fatalf("model/%s 允许改写哈希输入列：%s", fe.Name(), strings.TrimSpace(line))
					}
				}
			}
		}
	}
	if scanned == 0 {
		t.Fatal("没有扫描到任何 model 源文件，断言是空的")
	}
	if len(entryUpdates) != 1 {
		t.Fatalf("audit_entry 上的 UPDATE 必须只有一条（archived_at），实得 %d 条：%v", len(entryUpdates), entryUpdates)
	}
	stmt := entryUpdates[0]
	if !strings.Contains(stmt, "SET archived_at = ?") {
		t.Fatalf("唯一的 UPDATE 必须只把 archived_at 置为参数：%s", stmt)
	}
	if !strings.Contains(stmt, "archived_at = 0") {
		t.Fatalf("UPDATE 必须带 archived_at = 0 护栏（已归档行不可被改成别的时刻）：%s", stmt)
	}
	sets, _, hasWhere := strings.Cut(strings.SplitN(stmt, "SET ", 2)[1], " WHERE")
	if !hasWhere {
		t.Fatalf("UPDATE 必须带 WHERE 区间限定（没有 WHERE 就是全表改写）：%s", stmt)
	}
	if strings.Contains(sets, ",") {
		t.Fatalf("UPDATE 一次只能改 archived_at 一列：%s", stmt)
	}
}

// TestLedgerWritesOnlyHappenInsideChainAssembly 扫描 logic 包源码：
// audit_entry 的写入点必须只有 helpers.appendOnce 一处。
// 任何「绕过链装配直接 Insert / 自己 MarkArchived」的新代码都会在这里红——
// 那正是能写出可篡改条目的形状。
func TestLedgerWritesOnlyHappenInsideChainAssembly(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var hit []string
	for _, fe := range files {
		name := fe.Name()
		if fe.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读 %s 失败：%v", name, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "entries.Insert(") || strings.Contains(line, "MarkArchived(") {
				hit = append(hit, fmt.Sprintf("%s:%d", name, i+1))
			}
		}
	}
	if len(hit) != 1 {
		t.Fatalf("条目写入点必须唯一，实得 %v", hit)
	}
	if !strings.HasPrefix(hit[0], "helpers.go:") {
		t.Fatalf("条目写入只能发生在链装配里，实得 %s", hit[0])
	}
}

// --- 3/4. 数据面与编排面 ---

// verifyChain 直接走 VerifyAuditChain 用例，不改动假库的区间应答。
//
// requestID 由用例给且每次不同：自审计的 event_id 只由 (action, request_id, now) 派生，
// 同一 request_id 内第二次同动作校验会被幂等去重（这是「同一次调用只留一条痕」的设计），
// 因此测试要留两条痕就必须用两个 request_id。
func verifyChain(t *testing.T, requestID, chainKey string, from, to int64) *rpc.VerifyAuditChainReply {
	t.Helper()
	reply, err := NewVerifyAuditChainLogic(context.Background(), svcCtx()).
		VerifyAuditChain(&rpc.VerifyAuditChainReq{
			Ctx: testCallContext(requestID), ChainKey: chainKey, FromSeq: from, ToSeq: to,
		})
	if err != nil {
		t.Fatalf("VerifyAuditChain(%s) 失败：%v", chainKey, err)
	}
	return reply
}

// replayFromStore 让区间读接口按「已落库的行」应答，再走真实的校验路径。
// 断言的是「写进去的东西能自证」，而不是测试自己再算一遍期望哈希。
func (f *fixture) replayFromStore(t *testing.T, requestID, chainKey string, from, to int64) *rpc.VerifyAuditChainReply {
	t.Helper()
	f.entries.rangeRows = f.chainOf(chainKey)
	return verifyChain(t, requestID, chainKey, from, to)
}

// lastSelfAudit 取最近一条本服务自审计条目（domain=system）。
func (f *fixture) lastSelfAudit(t *testing.T) *model.AuditEntry {
	t.Helper()
	rows := f.chainOf(model.ChainKey(selfDomainSystem, f.now))
	if len(rows) == 0 {
		t.Fatal("读/校验动作没写自审计留痕")
	}
	return rows[len(rows)-1]
}

func TestIdempotentReplayNeverMutatesStoredRow(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	ctx := context.Background()
	cc := testCallContext("req-idem")
	l := NewAppendAuditLogic(ctx, svcCtx())

	first, err := l.AppendAudit(&rpc.AppendAuditReq{Ctx: cc, Entry: validDraft("ev-idem")})
	if err != nil {
		t.Fatal(err)
	}
	if first.GetReused() {
		t.Fatal("首次提交不该是 reused")
	}
	stored := *f.entries.byEvent["ev-idem"]
	headSeq := f.chains.heads[stored.ChainKey].Seq

	// 原样重放。
	second, err := l.AppendAudit(&rpc.AppendAuditReq{Ctx: cc, Entry: validDraft("ev-idem")})
	if err != nil {
		t.Fatal(err)
	}
	if !second.GetReused() {
		t.Fatalf("重放必须 reused=true：%+v", second)
	}
	v1, v2 := first.GetEntry(), second.GetEntry()
	if v1.GetEntryId() != v2.GetEntryId() || v1.GetSeq() != v2.GetSeq() ||
		v1.GetPrevHash() != v2.GetPrevHash() || v1.GetEntryHash() != v2.GetEntryHash() ||
		v1.GetCtime() != v2.GetCtime() {
		t.Fatalf("重放回了不一样的条目：\n%+v\n%+v", v1, v2)
	}

	// 幂等撞车时带着不同内容再来一次（「修正历史」的攻击形状）：
	// 必须原样返回既有行，绝不把新值写进已入库的那一条。
	hijack := validDraft("ev-idem")
	hijack.Action = "media.delete_all"
	hijack.Result = rpc.AuditResult_AUDIT_RESULT_ERROR
	hijack.Reason = "假装这是一次新操作"
	third, err := l.AppendAudit(&rpc.AppendAuditReq{Ctx: cc, Entry: hijack})
	if err != nil {
		t.Fatalf("event_id 命中既有行时不该报错（否则上游会以为没写成功而一直重试）：%v", err)
	}
	if third.GetEntry().GetAction() != stored.Action {
		t.Fatalf("重放把已入库条目的 action 换了：%q -> %q", stored.Action, third.GetEntry().GetAction())
	}
	if after := *f.entries.byEvent["ev-idem"]; !reflect.DeepEqual(stored, after) {
		t.Fatalf("已入库条目被改写：\n before=%+v\n after =%+v", stored, after)
	}
	if f.countAll() != 1 {
		t.Fatalf("三次提交只能有一行，实得 %d", f.countAll())
	}
	if got := f.chains.heads[stored.ChainKey].Seq; got != headSeq {
		t.Fatalf("重放推进了链头：%d -> %d", headSeq, got)
	}
	if got := len(f.chainOf(stored.ChainKey)); got != 1 {
		t.Fatalf("链上出现重复行：%d", got)
	}
	if reply := f.replayFromStore(t, "req-idem-replay", stored.ChainKey, 0, 0); !reply.GetIntact() || reply.GetChecked() != 1 {
		t.Fatalf("重放后的链应自证完整：%+v", reply)
	}
}

// racedWriter 制造「预检漏、回查中」的并发形状：Insert 一律撞唯一键。
type racedWriter struct {
	*fakeEntries
	rival *model.AuditEntry
	finds int
}

func (r *racedWriter) FindByEventIDs(context.Context, []string) (map[string]*model.AuditEntry, error) {
	r.finds++
	if r.finds < 2 {
		// 预检那一趟：对手还没提交。
		return map[string]*model.AuditEntry{}, nil
	}
	cp := *r.rival
	return map[string]*model.AuditEntry{cp.EventID: &cp}, nil
}

func (r *racedWriter) Insert(context.Context, sqlx.Session, *model.AuditEntry) (int64, error) {
	return 0, model.ErrTaskExists
}

func TestConcurrentDuplicateEventIdResolvesToExistingRow(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	// 并发窗口：预检没看到、Insert 撞唯一键、回查才看到对手刚写的那一行。
	key := model.ChainKey("media", testNow)
	rival := &model.AuditEntry{
		EntryID: 900, EventID: "ev-race", SchemaVersion: model.SchemaVersion,
		ChainKey: key, Seq: 1, ActorType: model.ActorAdmin, ActorID: 7,
		Action: "media.approve", ActionDomain: "media", Result: model.ResultOK, SourceApp: model.SourceAdminWeb,
		OccurredAt: testNow, PrevHash: model.GenesisHash(key), Ctime: testNow,
	}
	rival.EntryHash = model.ComputeEntryHash(rival)
	f.deps.entries = &racedWriter{fakeEntries: f.entries, rival: rival}

	reply, err := NewAppendAuditLogic(context.Background(), svcCtx()).
		AppendAudit(&rpc.AppendAuditReq{Ctx: testCallContext("req-race"), Entry: validDraft("ev-race")})
	if err != nil {
		t.Fatalf("并发撞唯一键必须转成幂等成功，实得 %v", err)
	}
	if !reply.GetReused() || reply.GetEntry().GetEntryId() != rival.EntryID {
		t.Fatalf("应原样回对手那一行：%+v", reply.GetEntry())
	}
	if len(f.entries.inserted) != 0 {
		t.Fatalf("重复条目又写了一次：%v", f.entries.inserted)
	}
	// 链头只会被 Ensure 建出来，且失败后随事务回滚；绝不能被推进：
	// 推进了就是给一条不存在的行占了号。
	if h := f.chains.heads[key]; h != nil && (h.Seq != 0 || h.LastHash != "") {
		t.Fatalf("写失败后链头不该变化：%+v", h)
	}
	if !strings.Contains(strings.Join(f.chains.ensured, ","), key) {
		t.Fatalf("没建链头就直接写条目：%v", f.chains.ensured)
	}
	if got := len(f.chainOf(key)); got != 0 {
		t.Fatalf("本侧不该落任何行，实得 %d", got)
	}
}

func TestChainConflictRetryKeepsChainReplayable(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	key := model.ChainKey("media", testNow)
	// 先放一条「对手」已提交的行，并让本侧第一次推进链头时被判成抢先（conflict）。
	// 假事务会连同链头一起回滚，正好对应真实 InnoDB 的行为：对手那一环在事务外已提交，
	// 因此重试那一轮一定读得到 seq=1 的真实尾摘要，而不是回到创世值。
	rival := seedChain(key, 1, 500)[0]
	f.putRows(rival)
	f.putHead(key, 1, rival.EntryHash)
	f.chains.conflicts = 1

	reply, err := NewAppendAuditLogic(context.Background(), svcCtx()).
		AppendAudit(&rpc.AppendAuditReq{Ctx: testCallContext("req-retry"), Entry: validDraft("ev-retry")})
	if err != nil {
		t.Fatal(err)
	}
	row := f.entries.byEvent["ev-retry"]
	if row == nil {
		t.Fatal("重试后条目应落库")
	}
	if row.Seq != 2 || row.PrevHash != rival.EntryHash {
		t.Fatalf("重试后没接上新链头：%+v", row)
	}
	if row.EntryHash != model.ComputeEntryHash(row) {
		t.Fatal("重试轮算出的 entry_hash 与行内容不一致")
	}
	h := f.chains.heads[key]
	if h.Seq != 2 || h.LastHash != row.EntryHash || h.LastEntryID != row.EntryID {
		t.Fatalf("链尾指针没指向真实存在的行：head=%+v row=%+v", h, row)
	}
	if got := reply.GetEntry().GetSeq(); got != row.Seq {
		t.Fatalf("回参 seq 与库内不一致：%d != %d", got, row.Seq)
	}
	// 恰好两次尝试：一次被抢先、一次按新链头写成，没有第三次就说明重试收敛。
	if got := len(f.chains.advanceCalls); got != 2 {
		t.Fatalf("Advance 调了 %d 次，期望 2", got)
	}
	// 整链（对手那一行 + 本条）重放仍然自证完整。
	if v := f.replayFromStore(t, "req-retry-replay", key, 0, 0); !v.GetIntact() || v.GetChecked() != 2 ||
		v.GetLastEntryHash() != row.EntryHash {
		t.Fatalf("重试后的链不自证完整：%+v", v)
	}
}

func TestExhaustedChainRetryLeavesNoPartialWrite(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	f.chains.conflicts = 1 << 10 // 永远抢不过
	_, err := NewAppendAuditLogic(context.Background(), svcCtx()).
		AppendAudit(&rpc.AppendAuditReq{Ctx: testCallContext("req-conflict"), Entry: validDraft("ev-conflict")})
	if !errors.Is(err, model.ErrChainConflict) {
		t.Fatalf("err = %v，期望 ErrChainConflict", err)
	}
	key := model.ChainKey("media", testNow)
	if got := len(f.chainOf(key)); got != 0 {
		t.Fatalf("事务回滚后链上还有 %d 行：条目写了而链头没推进 = 半条链", got)
	}
	if f.countAll() != 0 {
		t.Fatalf("整库行数应为 0，实得 %d", f.countAll())
	}
	// 重试次数必须收敛在配置内：无上限重试会把写入变成挂死。
	if got := len(f.chains.advanceCalls); got != f.opts.ChainRetry {
		t.Fatalf("Advance 调了 %d 次，期望 ChainRetry=%d", got, f.opts.ChainRetry)
	}
}

func TestInsertFailureRollsBackChainHead(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	ctx := context.Background()
	// 先正常写一条，让链头有真实尾摘要。
	if _, err := NewAppendAuditLogic(ctx, svcCtx()).
		AppendAudit(&rpc.AppendAuditReq{Ctx: testCallContext("req-a1"), Entry: validDraft("ev-a1")}); err != nil {
		t.Fatal(err)
	}
	key := model.ChainKey("media", testNow)
	before := *f.chains.heads[key]

	f.entries.insertErr = errors.New("audit_entry Insert: Error 1406: Data too long")
	if _, err := NewAppendAuditLogic(ctx, svcCtx()).
		AppendAudit(&rpc.AppendAuditReq{Ctx: testCallContext("req-a2"), Entry: validDraft("ev-a2")}); err == nil {
		t.Fatal("驱动报错必须原样上抛")
	}
	after := *f.chains.heads[key]
	if after.Seq != before.Seq || after.LastHash != before.LastHash ||
		after.EntryCount != before.EntryCount || after.LastEntryID != before.LastEntryID {
		t.Fatalf("插入失败却推进了链头：before=%+v after=%+v", before, after)
	}
	if got := len(f.chainOf(key)); got != 1 {
		t.Fatalf("失败批次留下了 %d 行半成品", got)
	}
	if v := f.replayFromStore(t, "req-rollback-replay", key, 0, 0); !v.GetIntact() || v.GetChecked() != 1 {
		t.Fatalf("回滚后原链应完好：%+v", v)
	}
}

// TestTamperAndErasureAreDetected 是本轮的核心用例：
// 四类「改历史」的形状都必须被 VerifyAuditChain 检出，并给出准确的断点原因。
func TestTamperAndErasureAreDetected(t *testing.T) {
	const key = "media/2026-03-05"
	cases := []struct {
		name        string
		setup       func(*fixture, []*model.AuditEntry) []*model.AuditEntry
		wantReason  string
		wantSeq     int64
		wantChecked int64
	}{
		{
			name:  "未篡改",
			setup: func(*fixture, []*model.AuditEntry) []*model.AuditEntry { return nil },
		},
		{
			// 直接改列值：把 reason 改成别的意思，行上的 entry_hash 立刻复算不上。
			name: "改参与哈希的列",
			setup: func(_ *fixture, rows []*model.AuditEntry) []*model.AuditEntry {
				rows[1].Reason = "事后改写的原因"
				return rows
			},
			wantReason: "entry_hash_mismatch", wantSeq: 2, wantChecked: 1,
		},
		{
			// 抹掉一行：等价于对 audit_entry 执行 DELETE（接口没有这条路，因此靠重放兜底）。
			name: "删中间一行",
			setup: func(_ *fixture, rows []*model.AuditEntry) []*model.AuditEntry {
				return append(rows[:1], rows[2:]...)
			},
			wantReason: "seq_gap", wantSeq: 2, wantChecked: 1,
		},
		{
			name: "截掉链尾",
			setup: func(_ *fixture, rows []*model.AuditEntry) []*model.AuditEntry {
				return rows[:len(rows)-1]
			},
			wantReason: "seq_gap", wantSeq: 4, wantChecked: 3,
		},
		{
			name: "整段从头删",
			setup: func(_ *fixture, rows []*model.AuditEntry) []*model.AuditEntry {
				return rows[2:]
			},
			wantReason: "seq_gap", wantSeq: 1, wantChecked: 0,
		},
		{
			// 为了掩盖删除而重编号：序号连续了，prev_hash 就接不上。
			name: "重编号掩盖空洞",
			setup: func(_ *fixture, rows []*model.AuditEntry) []*model.AuditEntry {
				rows[2].Seq = 2
				rows[3].Seq = 3
				return []*model.AuditEntry{rows[0], rows[2], rows[3]}
			},
			wantReason: "prev_hash_mismatch", wantSeq: 2, wantChecked: 1,
		},
		{
			// 把另一条链整段搬过来（连 chain_key 一起改）：创世摘要按链隔离，
			// 第一环的 prev_hash 就对不上；chain_key 本身参与哈希，也同时露馅。
			name: "跨链搬运",
			setup: func(_ *fixture, rows []*model.AuditEntry) []*model.AuditEntry {
				alien := seedChain("auth/"+strings.Split(key, "/")[1], 4, 500)
				for _, r := range alien {
					r.ChainKey = key
					r.ActionDomain = "media"
				}
				return alien
			},
			wantReason: "prev_hash_mismatch", wantSeq: 1, wantChecked: 0,
		},
		{
			// 只动序号不补哈希：连续性判定先命中。
			name: "改写序号",
			setup: func(_ *fixture, rows []*model.AuditEntry) []*model.AuditEntry {
				rows[3].Seq = 9
				return rows
			},
			wantReason: "seq_gap", wantSeq: 4, wantChecked: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			rows := seedChain(key, 4, 100)
			f.putHead(key, 4, rows[3].EntryHash)
			if mut := tc.setup(f, rows); mut != nil {
				rows = mut
			}
			f.entries.rangeRows = rows

			reply, err := NewVerifyAuditChainLogic(context.Background(), svcCtx()).
				VerifyAuditChain(&rpc.VerifyAuditChainReq{Ctx: testCallContext("req-tamper"), ChainKey: key})
			if err != nil {
				t.Fatalf("断链是数据结论而不是调用失败：%v", err)
			}
			if tc.wantReason == "" {
				if !reply.GetIntact() || reply.GetChecked() != 4 || reply.GetTruncated() {
					t.Fatalf("完整链被判成断链：%+v", reply)
				}
				if reply.GetLastEntryHash() != rows[3].EntryHash || reply.GetFirstBrokenSeq() != 0 {
					t.Fatalf("完整链的尾摘要/断点字段异常：%+v", reply)
				}
				return
			}
			if reply.GetIntact() {
				t.Fatalf("篡改未被检出：%+v", reply)
			}
			if got := reply.GetBrokenReason(); got != tc.wantReason {
				t.Fatalf("broken_reason = %q，期望 %q", got, tc.wantReason)
			}
			if got := reply.GetFirstBrokenSeq(); got != tc.wantSeq {
				t.Fatalf("first_broken_seq = %d，期望 %d", got, tc.wantSeq)
			}
			// 判定必须在首个断点停住：继续往下只会把同一处篡改重复报成 N 条异常。
			if got := reply.GetChecked(); got != tc.wantChecked {
				t.Fatalf("checked = %d，期望 %d（首个断点位置判定偏移）", got, tc.wantChecked)
			}
			// 断点定位：行在表里就必须给 entry_id（运维要靠它去捞那一行），
			// 序号空洞则是「该有的行根本不在表里」，entry_id 必须留 0 而不是指个假行。
			if tc.wantReason == "seq_gap" {
				if got := reply.GetFirstBrokenEntryId(); got != 0 {
					t.Fatalf("seq_gap 的 entry_id 应为 0，实得 %d", got)
				}
			} else if got := reply.GetFirstBrokenEntryId(); got == 0 {
				t.Fatalf("%s 必须定位到具体行，entry_id 却是 0", tc.wantReason)
			}
			if reply.GetTruncated() {
				t.Fatalf("条数未达上限却报 truncated：%+v", reply)
			}
		})
	}
}

// TestVerifyChainReportsTruncatedInsteadOfIntact：达到单次条数上限时必须报 truncated。
// 「只看了前 N 条」绝不能被说成「整条链验过了」，否则运维拿着 intact=true 就去出合规报告。
func TestVerifyChainReportsTruncatedInsteadOfIntact(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	f.withOpts(func(o *repository.Options) { o.MaxVerifyEntries = 2 })
	const key = "media/2026-03-05"
	rows := seedChain(key, 4, 100)
	f.putHead(key, 4, rows[3].EntryHash)
	f.entries.rangeRows = rows

	reply := verifyChain(t, "req-truncated", key, 1, 4)
	if reply.GetIntact() || !reply.GetTruncated() {
		t.Fatalf("超限必须报 truncated 且不算 intact：%+v", reply)
	}
	// 未覆盖到的第一号必须报出来，运维才知道要从这里续验。
	// 注意：proto 把 truncated 也列为 broken_reason 取值之一，但实现里首个断点
	// 已是 seq_gap（读取窗口被夹在 limit），因此 reason 落在 seq_gap。
	// 结论方向不受影响：intact=false + truncated=true 就是「没验完」，不会被当成通过。
	if reply.GetChecked() != 2 || reply.GetFirstBrokenSeq() != 3 ||
		reply.GetBrokenReason() != "seq_gap" {
		t.Fatalf("truncated 结论异常：%+v", reply)
	}
	// 取区间时多要一条（limit+1），才能区分「正好读完」与「后面还有」。
	if got := f.entries.rangeLimit[0]; got != 3 {
		t.Fatalf("区间读取 limit = %d，期望 max+1=3", got)
	}
	// 上限内的区间照常验完：截断判定不能把合法区间也标成断链。
	if ok := verifyChain(t, "req-within-limit", key, 1, 2); !ok.GetIntact() || ok.GetChecked() != 2 {
		t.Fatalf("上限内区间应完整验过：%+v", ok)
	}
}

func TestVerifyChainFromMiddleAnchorsOnPreviousRow(t *testing.T) {
	const key = "media/2026-03-05"
	setup := func(t *testing.T) (*fixture, []*model.AuditEntry) {
		t.Helper()
		f := newFixture(t)
		f.use(t)
		rows := seedChain(key, 4, 100)
		f.putHead(key, 4, rows[3].EntryHash)
		f.entries.rangeRows = rows
		return f, rows
	}

	t.Run("锚点在位时改 prev_hash 必被检出", func(t *testing.T) {
		f, rows := setup(t)
		rows[2].PrevHash = model.GenesisHash("auth/" + strings.Split(key, "/")[1])
		reply := verifyChain(t, "req-anchor-present", key, 3, 4)
		if reply.GetIntact() || reply.GetBrokenReason() != "prev_hash_mismatch" || reply.GetFirstBrokenSeq() != 3 {
			t.Fatalf("中段起验没接上锚点判定：%+v", reply)
		}
		// 主动多读一行取锚点：这是对契约缺口（请求里没有 expected_prev_hash）的补偿。
		if !strings.Contains(strings.Join(f.entries.rangeCalls, " "), key+":2-2") {
			t.Fatalf("没有回读锚点行：%v", f.entries.rangeCalls)
		}
	})

	// 锚点行本身也被删时：不能拿创世哈希冒充上一环，也不能因此报假断链；
	// 但「这一环没比 prev」必须记进自审计，事后能查出来这个盲区。
	t.Run("锚点缺失时记 anchor=missing 而非假装验过", func(t *testing.T) {
		f, rows := setup(t)
		withAnchor := verifyChain(t, "req-anchor-with", key, 3, 4)
		if !withAnchor.GetIntact() {
			t.Fatalf("锚点在位时完整链应通过：%+v", withAnchor)
		}
		digestWith := f.lastSelfAudit(t).AfterDigest

		f.entries.rangeRows = append([]*model.AuditEntry{rows[0]}, rows[2:]...) // seq=2 那行没了
		without := verifyChain(t, "req-anchor-without", key, 3, 4)
		if !without.GetIntact() {
			t.Fatalf("缺锚点不该被判成断链（无从比对就是无从比对，不是发现了篡改）：%+v", without)
		}
		if without.GetLastEntryHash() != rows[3].EntryHash || without.GetChecked() != 2 {
			t.Fatalf("缺锚点时仍然少验了：%+v", without)
		}
		digestWithout := f.lastSelfAudit(t).AfterDigest
		if n := len(strings.Split(digestWith, ";")); n != 4 {
			t.Fatalf("常规校验的 after 维度应为 4 项，实得 %d：%s", n, digestWith)
		}
		if n := len(strings.Split(digestWithout, ";")); n != 5 {
			t.Fatalf("缺锚点必须多记一笔维度（anchor=missing）：%s", digestWithout)
		}
		if digestWith == digestWithout {
			t.Fatal("有锚点与无锚点的留痕一模一样，等于没记录这个盲区")
		}
		for _, dg := range []string{digestWith, digestWithout} {
			if !model.ValidDigest(dg) {
				t.Fatalf("自审计摘要不在白名单内：%s", dg)
			}
			if strings.Contains(dg, key) {
				t.Fatalf("自审计摘要里出现了查询条件原文：%s", dg)
			}
		}
	})
}

func TestArchivedAtMarkingKeepsChainVerifiableAndRowImmutable(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	ctx := context.Background()
	l := NewAppendAuditLogic(ctx, svcCtx())
	for _, id := range []string{"ev-ar-1", "ev-ar-2"} {
		if _, err := l.AppendAudit(&rpc.AppendAuditReq{Ctx: testCallContext("req-archived"), Entry: validDraft(id)}); err != nil {
			t.Fatal(err)
		}
	}
	key := model.ChainKey("media", testNow)
	before := f.chainOf(key)
	if len(before) != 2 || before[0].ArchivedAt != 0 {
		t.Fatalf("前置数据异常：%+v", before)
	}
	const ts = testNow + 3600
	n, err := f.entries.MarkArchived(ctx, key, 1, 2, ts)
	if err != nil || n != 2 {
		t.Fatalf("MarkArchived = %d, %v", n, err)
	}
	after := f.chainOf(key)
	for i := range after {
		if after[i].ArchivedAt != ts {
			t.Fatalf("archived_at 未落：%+v", after[i])
		}
		cp := *after[i]
		cp.ArchivedAt = before[i].ArchivedAt
		// 把 archived_at 复原后整行必须逐字段相同：归档不可能改动任何存证内容或链字段。
		if !reflect.DeepEqual(cp, *before[i]) {
			t.Fatalf("归档改动了 archived_at 之外的列：\n before=%+v\n after=%+v", *before[i], *after[i])
		}
	}
	// 唯一的写后变更不破坏自证：整链仍然完整。
	if v := f.replayFromStore(t, "req-archived-replay", key, 0, 0); !v.GetIntact() || v.GetChecked() != 2 {
		t.Fatalf("归档后链不再自证：%+v", v)
	}
	// 重复标记必须一条都不动（写一次护栏）。
	again, err := f.entries.MarkArchived(ctx, key, 1, 2, testNow+9999)
	if err != nil || again != 0 {
		t.Fatalf("已归档区间被二次改写：%d, %v", again, err)
	}
	if got := f.chainOf(key)[0].ArchivedAt; got != ts {
		t.Fatalf("archived_at 被覆盖成 %d", got)
	}
}

func TestFutureOccurredAtCannotParkEvidenceOutsideVerifiableChains(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	ctx := context.Background()
	// occurred_at 明显超前的条目会落进一条「今天没人扫」的未来链，
	// 校验与归档都按链头推进，这类条目实际处于无人看守状态，因此必须拒写。
	future := validDraft("ev-future")
	future.OccurredAt = testNow + model.MaxFutureOccurredSkewSeconds + 1
	if _, err := NewAppendAuditLogic(ctx, svcCtx()).
		AppendAudit(&rpc.AppendAuditReq{Ctx: testCallContext("req-future"), Entry: future}); !errors.Is(err, model.ErrOccurredAtFuture) {
		t.Fatalf("err = %v，期望 ErrOccurredAtFuture", err)
	}
	if f.countAll() != 0 {
		t.Fatalf("拒写却落了 %d 行", f.countAll())
	}
	// 历史条目照常落当天的链，链名与 occurred_at 同日，校验按链头一定能覆盖到。
	old := validDraft("ev-old")
	old.OccurredAt = testNow - 86400
	if _, err := NewAppendAuditLogic(ctx, svcCtx()).
		AppendAudit(&rpc.AppendAuditReq{Ctx: testCallContext("req-old"), Entry: old}); err != nil {
		t.Fatal(err)
	}
	want := model.ChainKey("media", testNow-86400)
	row := f.entries.byEvent["ev-old"]
	if row.ChainKey != want {
		t.Fatalf("条目落错链：%s 应为 %s", row.ChainKey, want)
	}
	if h := f.chains.heads[want]; h.Seq != 1 || h.LastHash != row.EntryHash {
		t.Fatalf("链头与条目不一致：%+v row=%+v", h, row)
	}
}

func TestBatchAppendWritesOneContinuousChainPerDomain(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	drafts := []*rpc.AuditEntryDraft{validDraft("ev-c-1"), validDraft("ev-c-2"), validDraft("ev-c-3")}
	// 跨域条目：按「域 + UTC 日」分链，锁顺序按链名升序（防交叉死锁的唯一手段）。
	other := validDraft("ev-c-auth")
	other.Action = "auth.login"
	other.ActionDomain = "auth"
	drafts = append(drafts, other)

	reply, err := NewBatchAppendAuditLogic(context.Background(), svcCtx()).
		BatchAppendAudit(&rpc.BatchAppendAuditReq{Ctx: testCallContext("req-chain"), Entries: drafts})
	if err != nil {
		t.Fatal(err)
	}
	if reply.GetAccepted() != 4 {
		t.Fatalf("accepted=%d，期望 4", reply.GetAccepted())
	}
	mediaKey, authKey := model.ChainKey("media", testNow), model.ChainKey("auth", testNow)
	media := f.chainOf(mediaKey)
	if len(media) != 3 {
		t.Fatalf("media 链行数=%d", len(media))
	}
	for i, r := range media {
		if r.Seq != int64(i+1) {
			t.Fatalf("链内序号不连续：%+v", media)
		}
		wantPrev := model.GenesisHash(mediaKey)
		if i > 0 {
			wantPrev = media[i-1].EntryHash
		}
		if r.PrevHash != wantPrev {
			t.Fatalf("seq=%d 的 prev_hash 没接上前一条", r.Seq)
		}
		if r.EntryHash != model.ComputeEntryHash(r) {
			t.Fatalf("seq=%d 的 entry_hash 不可复算", r.Seq)
		}
	}
	if got := len(f.chainOf(authKey)); got != 1 {
		t.Fatalf("不同动作域必须分链，实得 %d 行", got)
	}
	// 加锁顺序：两条业务链按链名升序各锁一次。
	var locked []string
	for _, k := range f.chains.locked {
		if k == mediaKey || k == authKey {
			locked = append(locked, k)
		}
	}
	if !equalStrings(locked, []string{authKey, mediaKey}) {
		t.Fatalf("加锁顺序不是链名升序：%v", locked)
	}
	for _, k := range []string{mediaKey, authKey} {
		if v := f.replayFromStore(t, "req-replay-"+k, k, 0, 0); !v.GetIntact() {
			t.Fatalf("链 %s 不自证完整：%+v", k, v)
		}
	}
}
