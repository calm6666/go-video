package logic

// 导出链路：CreateAuditExport（提交）→ RunAuditExportTask（推进）→ GetAuditExport（取件）。
//
// 这三步是审计数据「离开本系统」的唯一出口，因此用例钉住四件事：
//  1. 提交阶段绝不产出文件（不碰对象存储），只落一行 pending 任务；
//  2. 推进阶段任何一步做不到都要收敛到 failed，绝不停在 running，
//     也绝不「库里记 succeeded 而桶里没有对象」；
//  3. 取件阶段只对 succeeded 且未到期的产物签发地址，地址本身是凭证，不得进任何留痕列；
//  4. 三步都以 request_id 幂等，重放不产生第二份产物、不写第二条痕。

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"go-video/services/audit/internal/repository"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"
)

// legalExportFilterJSON 既是一份能通过 parseExportFilter 的条件快照，
// 也是 encode() 的输出基准（键序 = 结构体字段序，见 TestCreateAuditExportStoresBlindSnapshot）。
const legalExportFilterJSON = `{"start_at":1772701200,"end_at":1772704800,"action_domain":"media"}`

const exportTestReason = "配合监管检查导出当日媒资处置记录"

func goodExportReq(requestID string) *rpc.CreateAuditExportReq {
	return &rpc.CreateAuditExportReq{
		Ctx:          testCallContext(requestID),
		StartAt:      testNow - 3600,
		EndAt:        testNow,
		ActionDomain: "media",
		Format:       "csv",
		Reason:       exportTestReason,
	}
}

// seedExportTask 跳过提交接口直接落一行任务，用于单独测推进器与取件器。
func (f *fixture) seedExportTask(t *testing.T, requestID, format, filterJSON string,
	mutate func(*model.ExportTask)) *model.ExportTask {
	t.Helper()
	task := &model.ExportTask{RequestID: requestID, OperatorID: 7, CallerService: "admin-api",
		FilterJSON: filterJSON, Format: format, State: model.ExportStatePending}
	if mutate != nil {
		mutate(task)
	}
	if _, err := f.exports.Insert(context.Background(), task); err != nil {
		t.Fatalf("预置任务失败：%v", err)
	}
	return task
}

func (f *fixture) mustExportTask(t *testing.T, taskID int64) *model.ExportTask {
	t.Helper()
	row, err := f.exports.FindOne(context.Background(), taskID)
	if err != nil {
		t.Fatalf("回读任务 %d 失败：%v", taskID, err)
	}
	return row
}

// exportTrail 取导出/归档链路上已落库的自审计留痕（按 seq 升序）。
// 「期望有条数」用它（trailOn 在空集上会 Fatal）；「期望零条」要用 chainOf，
// 否则是先 Fatal 而不是先失败断言。
func (f *fixture) exportTrail(t *testing.T) []*model.AuditEntry {
	t.Helper()
	return f.trailOn(t, selfDomainDataAccess)
}

func (f *fixture) exportTrailCount() int {
	return len(f.chainOf(model.ChainKey(selfDomainDataAccess, f.now)))
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// csvCol 按表头名取列下标：断言写成列名而不是写死数字，改列序时才会由表头一致性用例报出来。
func csvCol(t *testing.T, name string) int {
	t.Helper()
	for i, h := range exportCSVHeader {
		if h == name {
			return i
		}
	}
	t.Fatalf("导出表头里没有列 %s", name)
	return -1
}

// --- 提交 ---

func TestCreateAuditExportRejectsBeforeWritingTask(t *testing.T) {
	// 空请求单独判：表内用例都要先造一份合法请求再改坏一个字段。
	if _, err := NewCreateAuditExportLogic(context.Background(), svcCtx()).
		CreateAuditExport(nil); !errors.Is(err, model.ErrRequestRequired) {
		t.Fatalf("空请求 err = %v，期望 ErrRequestRequired", err)
	}
	day := int64(86400)
	cases := []struct {
		name    string
		mutate  func(*rpc.CreateAuditExportReq)
		wantErr error
	}{
		{"无 request_id", func(r *rpc.CreateAuditExportReq) { r.Ctx.RequestId = "" }, model.ErrRequestIDRequired},
		{"无 reason", func(r *rpc.CreateAuditExportReq) { r.Reason = "   " }, model.ErrReasonRequired},
		{"未知格式", func(r *rpc.CreateAuditExportReq) { r.Format = "xlsx" }, model.ErrExportFormatUnsupported},
		{"无 start_at", func(r *rpc.CreateAuditExportReq) { r.StartAt = 0 }, model.ErrQueryRangeRequired},
		{"区间倒置", func(r *rpc.CreateAuditExportReq) { r.StartAt, r.EndAt = testNow, testNow-1 },
			model.ErrQueryRangeRequired},
		{"跨度过宽", func(r *rpc.CreateAuditExportReq) { r.StartAt = testNow - day*200 },
			model.ErrQueryRangeTooWide},
		{"无收窄维度", func(r *rpc.CreateAuditExportReq) { r.ActionDomain = "" }, model.ErrQueryTooBroad},
		{"裸 target_id 不算收窄", func(r *rpc.CreateAuditExportReq) {
			r.ActionDomain = ""
			r.TargetId = "work-1"
		}, model.ErrQueryTooBroad},
		{"负 actor_id", func(r *rpc.CreateAuditExportReq) { r.ActorId = -3 }, model.ErrActorIDInvalid},
		{"未知 actor_type", func(r *rpc.CreateAuditExportReq) { r.ActorType = rpc.ActorType(88) }, nil},
		{"非法 action_domain", func(r *rpc.CreateAuditExportReq) { r.ActionDomain = "Media-DB" },
			model.ErrActionDomainInvalid},
		{"action 形态非法", func(r *rpc.CreateAuditExportReq) { r.ActionDomain = ""; r.Action = "approve" },
			model.ErrActionInvalid},
		{"条件里带手机号", func(r *rpc.CreateAuditExportReq) { r.TargetId = "13800138000" },
			model.ErrDigestLooksPII},
		{"动机里带手机号", func(r *rpc.CreateAuditExportReq) { r.Reason = "核对 13800138000 的处置" },
			model.ErrDigestLooksPII},
		{"动机里带邮箱", func(r *rpc.CreateAuditExportReq) { r.Reason = "发给 someone@example.com 的申诉" },
			model.ErrDigestLooksPII},
		{"动机里带身份证", func(r *rpc.CreateAuditExportReq) { r.Reason = "11010119900307461X 的案件" },
			model.ErrDigestLooksPII},
		{"动机超长", func(r *rpc.CreateAuditExportReq) { r.Reason = strings.Repeat("字", 600) },
			model.ErrFieldTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			in := goodExportReq("req-exp-" + strings.ReplaceAll(tc.name, " ", "-"))
			tc.mutate(in)
			reply, err := NewCreateAuditExportLogic(context.Background(), svcCtx()).CreateAuditExport(in)
			if err == nil {
				t.Fatal("非法导出请求必须报错")
			}
			if reply != nil {
				t.Fatalf("拒绝时不得回响应：%+v", reply)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && !strings.Contains(err.Error(), "unknown") {
				t.Fatalf("契约外枚举值必须被点名：%v", err)
			}
			// 关键：被拒的请求一行任务、一行留痕都不留。
			if got := len(f.exports.byID); got != 0 {
				t.Fatalf("被拒请求仍建了任务：%d 行", got)
			}
			if got := f.countAll(); got != 0 {
				t.Fatalf("被拒请求仍写了 %d 行留痕", got)
			}
			// 提交阶段根本不碰对象存储：不存在「顺手生成一个空文件」。
			if got := len(f.store.created); got != 0 {
				t.Fatalf("提交阶段就写了对象：%v", f.store.created)
			}
		})
	}
}

func TestCreateAuditExportStoresBlindSnapshotAndStaysPending(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	reply, err := NewCreateAuditExportLogic(context.Background(), svcCtx()).
		CreateAuditExport(goodExportReq("req-exp-ok"))
	if err != nil {
		t.Fatal(err)
	}
	if reply.GetReused() {
		t.Fatal("首次提交不得报 reused")
	}
	task := f.mustExportTask(t, reply.GetTask().GetTaskId())
	if task.State != model.ExportStatePending {
		t.Fatalf("提交后状态 = %s，本服务不内置 worker，必须停在 pending", task.State)
	}
	if task.Format != "csv" || task.RowCount != 0 || task.LastSeq != 0 {
		t.Fatalf("任务被推进器提前写过了：%+v", task)
	}
	if task.Bucket != "" || task.ObjectKey != "" || task.FileHash != "" || task.ExpireAt != 0 {
		t.Fatalf("还没有产物却要填产物字段：%+v", task)
	}
	// 快照文本逐字节可预期（键序 = 结构体字段序），同一条件恒定产出同一文本。
	wantJSON := `{"start_at":1772701200,"end_at":1772704800,"action_domain":"media","reason":"` +
		exportTestReason + `"}`
	if task.FilterJSON != wantJSON {
		t.Fatalf("快照文本 = %s，期望 %s", task.FilterJSON, wantJSON)
	}
	// 反向钉住解析器：推进器只信快照，快照必须能原样还原成条件。
	back, perr := parseExportFilter(task.FilterJSON)
	if perr != nil {
		t.Fatalf("刚落库的快照解析不回来：%v", perr)
	}
	if back.StartAt != testNow-3600 || back.EndAt != testNow || back.ActionDomain != "media" ||
		back.Reason != exportTestReason {
		t.Fatalf("快照还原失真：%+v", back)
	}
	// 快照里没有任何明文敏感形态（条件值在扫描阶段就被拒了）。
	if model.LooksLikePII(task.FilterJSON) {
		t.Fatalf("filter_json 命中明文敏感形态：%s", task.FilterJSON)
	}
	// 归因列取自 CallContext，而不是调用方自称的字段。
	if task.OperatorID != 7 || task.CallerService != "admin-api" || task.TraceID != "trace-1" {
		t.Fatalf("任务归因异常：%+v", task)
	}
	trail := f.exportTrail(t)
	if len(trail) != 1 || trail[0].Action != actionExportSubmit {
		t.Fatalf("提交留痕异常：%+v", trail)
	}
	for _, digest := range []string{trail[0].BeforeDigest, trail[0].AfterDigest} {
		if !model.ValidDigest(digest) {
			t.Fatalf("留痕摘要不在白名单内：%q", digest)
		}
		if strings.Contains(digest, task.FilterJSON) {
			t.Fatal("留痕把整份快照抄了一遍")
		}
	}
	if got := len(f.store.created); got != 0 {
		t.Fatalf("提交阶段写了 %d 个对象", got)
	}
}

// format 归一化的三档：空 → csv、大小写与空格 → 小写、其余 → 拒。
func TestCreateAuditExportNormalizesFormat(t *testing.T) {
	for _, tc := range []struct{ in, want string }{{"", "csv"}, {"CSV", "csv"}, {"  Json  ", "json"}} {
		t.Run("format="+tc.in, func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			in := goodExportReq("req-fmt-" + strings.ReplaceAll(tc.in, " ", "_"))
			in.Format = tc.in
			reply, err := NewCreateAuditExportLogic(context.Background(), svcCtx()).CreateAuditExport(in)
			if err != nil {
				t.Fatal(err)
			}
			if got := f.mustExportTask(t, reply.GetTask().GetTaskId()).Format; got != tc.want {
				t.Fatalf("format = %q，期望归一化为 %q", got, tc.want)
			}
			// 归一化后的格式必须进对象键，否则取件时按 key 找不到文件。
			if k := exportObjectKey("req-fmt-"+strings.ReplaceAll(tc.in, " ", "_"), tc.want); !strings.HasSuffix(k, "."+tc.want) {
				t.Fatalf("对象键后缀与格式不符：%s", k)
			}
		})
	}
}

func TestCreateAuditExportIsIdempotentByRequestID(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	ctx := context.Background()
	logic := NewCreateAuditExportLogic(ctx, svcCtx())
	first, err := logic.CreateAuditExport(goodExportReq("req-exp-idem"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := logic.CreateAuditExport(goodExportReq("req-exp-idem"))
	if err != nil {
		t.Fatalf("重复提交必须幂等成功而不是撞唯一键报错：%v", err)
	}
	if !second.GetReused() {
		t.Fatal("重复提交未报 reused")
	}
	if second.GetTask().GetTaskId() != first.GetTask().GetTaskId() {
		t.Fatalf("两次提交拿到了不同任务：%d vs %d",
			first.GetTask().GetTaskId(), second.GetTask().GetTaskId())
	}
	if got := len(f.exports.byID); got != 1 {
		t.Fatalf("同一 request_id 建了 %d 个任务", got)
	}
	// 复用路径提前 return，不写第二条痕：否则重放 100 次就能把留痕链灌满噪声。
	if got := f.exportTrailCount(); got != 1 {
		t.Fatalf("重复提交后留痕 %d 条，期望仍是 1 条", got)
	}
	// 幂等键是 request_id 而不是条件指纹：换 request_id 就是另一次申请。
	other, err := logic.CreateAuditExport(goodExportReq("req-exp-idem-2"))
	if err != nil {
		t.Fatal(err)
	}
	if other.GetReused() {
		t.Fatal("不同 request_id 不该复用同一任务")
	}
	if got := len(f.exports.byID); got != 2 {
		t.Fatalf("任务数 = %d，期望 2", got)
	}
}

// racyExports 复现「回查通过后、插入之前，另一个进程用同一 request_id 抢先建了任务」。
// 真库里那是 uniq_request_id 上的一次 1062；这里让第一次回查扑空、
// 并在插入前把对手行落库，从而在不连 MySQL 的前提下走到回查兜底分支。
type racyExports struct {
	*fakeExports
	probed int
	rival  *model.ExportTask
}

func (r *racyExports) FindByRequestID(ctx context.Context, requestID string) (*model.ExportTask, error) {
	r.probed++
	if r.probed == 1 {
		return nil, nil
	}
	return r.fakeExports.FindByRequestID(ctx, requestID)
}

func (r *racyExports) Insert(ctx context.Context, task *model.ExportTask) (int64, error) {
	// 对手先落库（占住 request_id），本请求的插入随之撞唯一键。
	if _, err := r.fakeExports.Insert(ctx, r.rival); err != nil {
		return 0, err
	}
	return r.fakeExports.Insert(ctx, task)
}

func TestCreateAuditExportConcurrentDuplicateResolvesToExistingTask(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	rival := &model.ExportTask{RequestID: "req-exp-race", OperatorID: 9, CallerService: "cron-sweep",
		FilterJSON: legalExportFilterJSON, Format: "csv", State: model.ExportStatePending}
	f.deps.exports = &racyExports{fakeExports: f.exports, rival: rival}
	reply, err := NewCreateAuditExportLogic(context.Background(), svcCtx()).
		CreateAuditExport(goodExportReq("req-exp-race"))
	if err != nil {
		t.Fatalf("并发重复提交必须按幂等成功：%v", err)
	}
	if !reply.GetReused() {
		t.Fatal("未报 reused")
	}
	if len(f.exports.byID) != 1 {
		t.Fatalf("库里应只有对手那一条任务：%+v", f.exports.byID)
	}
	got := f.mustExportTask(t, reply.GetTask().GetTaskId())
	if got.OperatorID != 9 || got.CallerService != "cron-sweep" {
		t.Fatalf("回放的必须是先落库那条任务，而不是本请求想建的：%+v", got)
	}
	// 抢输的一方不写留痕：任务不归它，写痕会把「谁申请的导出」记错。
	if n := f.exportTrailCount(); n != 0 {
		t.Fatalf("抢输的请求写了 %d 条留痕：%+v", n, f.chainOf(model.ChainKey(selfDomainDataAccess, f.now)))
	}
}

// --- 推进 ---

func TestRunAuditExportTaskWritesSelfProvableObject(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	key := model.ChainKey("media", testNow)
	rows := seedChain(key, 5, 900)
	// 给首行一条带逗号与引号的 reason：只有走 encoding/csv 才不会写出无法解析的文件。
	rows[0].Reason = "含, 逗号 与 \"引号\" 的处置说明"
	rows[0].EntryHash = model.ComputeEntryHash(rows[0])
	f.putRows(rows...)
	// 两批扫完：批大小由配置决定，测试里改配置必须能观察到批次数与 LIMIT 变化。
	f.withOpts(func(o *repository.Options) { o.ExportBatchRows = 2 })
	task := f.seedExportTask(t, "req-run-ok", "csv", legalExportFilterJSON, nil)
	f.entries.scanPages = [][]*model.AuditEntry{rows, rows, rows}

	reply, err := NewRunAuditExportTaskLogic(context.Background(), svcCtx()).
		RunAuditExportTask(&rpc.RunAuditExportTaskReq{Ctx: testCallContext("req-run-ctx"), TaskId: task.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	if !reply.GetFinished() || reply.GetExportedRows() != 5 {
		t.Fatalf("回参异常：%+v", reply)
	}
	if len(f.entries.scanCalls) != 3 || len(f.entries.scanLimits) != 3 {
		t.Fatalf("扫描次数 = %d，期望 3 批", len(f.entries.scanCalls))
	}
	for i, lim := range f.entries.scanLimits {
		if lim != 2 {
			t.Fatalf("第 %d 批 LIMIT = %d，未跟随 ExportBatchRows", i, lim)
		}
	}
	prev := int64(-1)
	for i, c := range f.entries.scanCalls {
		if c <= prev {
			t.Fatalf("第 %d 批游标未前进：%d -> %d", i, prev, c)
		}
		prev = c
	}
	if got := len(f.exports.progress); got != 3 {
		t.Fatalf("AddProgress 次数 = %d，每批都要推游标：%v", got, f.exports.progress)
	}
	fresh := f.mustExportTask(t, task.TaskID)
	if fresh.State != model.ExportStateSucceeded {
		t.Fatalf("终态 = %s，期望 succeeded", fresh.State)
	}
	if fresh.RowCount != 5 || fresh.ErrMsg != "" || fresh.FinishedAt == 0 {
		t.Fatalf("成功任务上不该有失败痕迹：%+v", fresh)
	}
	objectKey := exportObjectKey(task.RequestID, task.Format)
	if fresh.ObjectKey != objectKey || fresh.Bucket != f.opts.Storage.Bucket {
		t.Fatalf("产物引用 = %s/%s，期望 %s/%s", fresh.Bucket, fresh.ObjectKey,
			f.opts.Storage.Bucket, objectKey)
	}
	if fresh.ExpireAt != testNow+f.opts.ObjectTTLSeconds {
		t.Fatalf("expire_at = %d，期望 now+ObjectTTLSeconds=%d", fresh.ExpireAt, testNow+f.opts.ObjectTTLSeconds)
	}
	obj := f.store.objects[objectKey]
	if obj == nil || !obj.committed || obj.aborted {
		t.Fatalf("对象未正常落地：%+v", obj)
	}
	if got := len(f.store.created); got != 1 {
		t.Fatalf("一次推进创建了 %d 个对象：%v", got, f.store.created)
	}
	// file_hash 必须是对象字节的 sha256hex —— 它是下载方唯一的自证手段。
	if !isHashHex(fresh.FileHash) || fresh.FileHash != sha256Hex(obj.buf) {
		t.Fatalf("file_hash = %s，与对象实际摘要 %s 不符", fresh.FileHash, sha256Hex(obj.buf))
	}
	if fresh.ObjectSize != int64(len(obj.buf)) {
		t.Fatalf("object_size = %d，对象实际 %d 字节", fresh.ObjectSize, len(obj.buf))
	}
	records, derr := csv.NewReader(strings.NewReader(string(obj.buf))).ReadAll()
	if derr != nil {
		t.Fatalf("导出文件不是合法 CSV：%v", derr)
	}
	if len(records) != 6 {
		t.Fatalf("CSV 行数 = %d，期望 1 表头 + 5 数据", len(records))
	}
	if strings.Join(records[0], ",") != strings.Join(exportCSVHeader, ",") {
		t.Fatalf("表头不等于 exportCSVHeader：%v", records[0])
	}
	idCol, hashCol, reasonCol := csvCol(t, "entry_id"), csvCol(t, "entry_hash"), csvCol(t, "reason")
	for i, rec := range records[1:] {
		if len(rec) != len(exportCSVHeader) {
			t.Fatalf("第 %d 行列数 = %d，期望 %d", i, len(rec), len(exportCSVHeader))
		}
		if rec[idCol] != strconv.FormatInt(rows[i].EntryID, 10) {
			t.Fatalf("第 %d 行 entry_id = %s，期望 %d", i, rec[idCol], rows[i].EntryID)
		}
		if rec[hashCol] != rows[i].EntryHash {
			t.Fatalf("第 %d 行 entry_hash 与链上不符：%s vs %s", i, rec[hashCol], rows[i].EntryHash)
		}
	}
	if records[1][reasonCol] != rows[0].Reason {
		t.Fatalf("含逗号/引号的 reason 未被正确转义：%q vs %q", records[1][reasonCol], rows[0].Reason)
	}
	// 导出文件里没有未加盐的来源标识原文。
	assertNoRaw(t, "导出文件", map[string]string{"body": string(obj.buf)}, f.saltOf())
	// 收敛留痕。
	trail := f.exportTrail(t)
	last := trail[len(trail)-1]
	if last.Action != actionExportFinish {
		t.Fatalf("收敛留痕 action = %s", last.Action)
	}
	for _, digest := range []string{last.BeforeDigest, last.AfterDigest} {
		if !model.ValidDigest(digest) {
			t.Fatalf("收敛留痕摘要非法：%q", digest)
		}
		if strings.Contains(digest, fresh.FileHash) {
			t.Fatalf("留痕把 file_hash 原文抄了一遍：%q", digest)
		}
	}
}

func TestRunAuditExportTaskNeverLeavesTaskRunningOnFailure(t *testing.T) {
	cases := []struct {
		name     string
		filter   string
		setup    func(*fixture)
		wantErr  error
		wantMsg  string
		wantObjs int
	}{
		{"快照不可解析", `{"start_at":0,"end_at":0}`, nil, model.ErrExportFilterInvalid,
			"导出条件快照不可解析", 0},
		{"扫描失败", legalExportFilterJSON, func(f *fixture) {
			f.entries.scanErr = errors.New("audit_entry ScanAfter: lost connection")
			f.entries.scanErrAt = 1
		}, nil, "扫描热表失败", 1},
		{"对象存储写不进", legalExportFilterJSON, func(f *fixture) {
			f.store.createErr = errors.New("s3: put object refused")
		}, nil, "创建导出对象失败", 0},
		{"行数超上限", legalExportFilterJSON, func(f *fixture) {
			rows := seedChain(model.ChainKey("media", testNow), 4, 1200)
			f.putRows(rows...)
			f.entries.scanPages = [][]*model.AuditEntry{rows, rows}
			f.withOpts(func(o *repository.Options) {
				o.ExportBatchRows = 3
				o.ExportMaxRows = 3
			})
		}, model.ErrExportRowLimit, "导出行数超过单任务上限", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			task := f.seedExportTask(t, "req-run-"+tc.name, "csv", tc.filter, nil)
			if tc.setup != nil {
				tc.setup(f)
			}
			reply, err := NewRunAuditExportTaskLogic(context.Background(), svcCtx()).
				RunAuditExportTask(&rpc.RunAuditExportTaskReq{
					Ctx: testCallContext("req-run-fail-" + tc.name), TaskId: task.TaskID})
			if err == nil {
				t.Fatal("失败路径必须报错")
			}
			if reply != nil {
				t.Fatalf("失败时不得回响应对象：%+v", reply)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			fresh := f.mustExportTask(t, task.TaskID)
			if fresh.State != model.ExportStateFailed {
				t.Fatalf("终态 = %s，期望 failed（绝不允许停在 running）", fresh.State)
			}
			if !strings.Contains(fresh.ErrMsg, tc.wantMsg) {
				t.Fatalf("err_msg = %q，应说明 %q", fresh.ErrMsg, tc.wantMsg)
			}
			if len(fresh.ErrMsg) > model.MaxErrMsgBytes {
				t.Fatalf("err_msg 超出列宽：%d", len(fresh.ErrMsg))
			}
			if strings.ContainsAny(fresh.ErrMsg, "\r\n") {
				t.Fatalf("err_msg 带换行会破坏日志与导出列：%q", fresh.ErrMsg)
			}
			if fresh.Bucket != "" || fresh.ObjectKey != "" || fresh.FileHash != "" {
				t.Fatalf("失败任务却带着产物引用：%+v", fresh)
			}
			// 失败任务不回传部分行数：下游按 row_count 计数会把半截文件当完整。
			if fresh.RowCount != 0 {
				t.Fatalf("失败任务 row_count = %d，期望 0", fresh.RowCount)
			}
			if got := len(f.store.created); got != tc.wantObjs {
				t.Fatalf("对象创建次数 = %d，期望 %d：%v", got, tc.wantObjs, f.store.created)
			}
			// 「库里记 failed」时桶里不得有已提交产物。
			for _, k := range f.store.created {
				if o := f.store.objects[k]; o != nil && o.committed {
					t.Fatalf("任务已 failed 而对象 %s 已 commit：桶里留下无人认领的产物", k)
				}
			}
			if got := len(f.exports.finishes); got != 1 || f.exports.finishes[0] != model.ExportStateFailed {
				t.Fatalf("Finish 调用 = %v，期望恰好一次 failed", f.exports.finishes)
			}
		})
	}
}

func TestRunAuditExportTaskRespectsOwnershipAndTerminalStates(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		state     string
		lastSeq   int64
		rowCount  int64
		wantErr   error
		claimLose bool // 让 Claim 模拟「被别人抢先一步」
	}{
		{"pending 被抢占", model.ExportStatePending, 0, 0, nil, true},
		{"running 已有游标", model.ExportStateRunning, 903, 3, model.ErrExportAlreadyClaimed, false},
		{"running 零进度可接管", model.ExportStateRunning, 0, 0, nil, false},
		{"succeeded 不可重跑", model.ExportStateSucceeded, 0, 5, model.ErrTaskBadTransition, false},
		{"failed 不可重跑", model.ExportStateFailed, 0, 0, model.ErrTaskBadTransition, false},
		{"expired 不可重跑", model.ExportStateExpired, 0, 5, model.ErrTaskBadTransition, false},
		{"canceled 不可重跑", model.ExportStateCanceled, 0, 0, model.ErrTaskBadTransition, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			rows := seedChain(model.ChainKey("media", testNow), 3, 1500)
			f.putRows(rows...)
			f.entries.scanPages = [][]*model.AuditEntry{rows}
			task := f.seedExportTask(t, "req-own-"+strings.ReplaceAll(tc.name, " ", "-"), "json",
				legalExportFilterJSON, func(e *model.ExportTask) {
					e.State = tc.state
					e.LastSeq = tc.lastSeq
					e.RowCount = tc.rowCount
				})
			if tc.claimLose {
				f.exports.claimFails = 1
			}
			reply, err := NewRunAuditExportTaskLogic(ctx, svcCtx()).
				RunAuditExportTask(&rpc.RunAuditExportTaskReq{Ctx: testCallContext("req-own-ctx"), TaskId: task.TaskID})
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
				}
				if reply != nil {
					t.Fatalf("拒绝时不得回响应：%+v", reply)
				}
				if got := len(f.store.created); got != 0 {
					t.Fatalf("没有推进权却写了对象：%v", f.store.created)
				}
				if fresh := f.mustExportTask(t, task.TaskID); fresh.State != tc.state {
					t.Fatalf("被拒的推进改写了任务状态：%s -> %s", tc.state, fresh.State)
				}
				if len(f.exports.claims) != 0 {
					t.Fatalf("无推进权的分支仍去 Claim 了：%v", f.exports.claims)
				}
			case tc.claimLose:
				// 拿不到推进权：安静结束而不报错，让 cron 下一轮再来。
				if err != nil {
					t.Fatalf("被别人持有应安静返回：%v", err)
				}
				if reply.GetTask().GetState() != model.ExportStatePending {
					t.Fatalf("回参状态 = %s，期望原样回报 pending", reply.GetTask().GetState())
				}
				if got := len(f.store.created); got != 0 {
					t.Fatalf("没有推进权却写了对象：%v", f.store.created)
				}
				if got := len(f.exports.finishes); got != 0 {
					t.Fatalf("没有推进权却收敛了状态：%v", f.exports.finishes)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if len(f.store.created) == 0 {
					t.Fatal("拿到推进权却没写对象")
				}
				if got := f.exports.claims; len(got) != 1 || got[0] != tc.state {
					t.Fatalf("Claim 期望状态 = %v，期望基于原状态 %s", got, tc.state)
				}
			}
		})
	}
}

func TestRunAuditExportTaskRejectsBadRequests(t *testing.T) {
	ctx := context.Background()
	logic := NewRunAuditExportTaskLogic(ctx, svcCtx())
	f := newFixture(t)
	f.use(t)
	cases := []struct {
		name string
		in   *rpc.RunAuditExportTaskReq
	}{
		{"空请求", nil},
		{"无 ctx", &rpc.RunAuditExportTaskReq{TaskId: 1}},
		{"无 request_id", &rpc.RunAuditExportTaskReq{Ctx: &rpc.CallContext{CallerService: "cron"}, TaskId: 1}},
		{"task_id 缺失", &rpc.RunAuditExportTaskReq{Ctx: testCallContext("r-1")}},
		{"task_id 为负", &rpc.RunAuditExportTaskReq{Ctx: testCallContext("r-2"), TaskId: -9}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := logic.RunAuditExportTask(tc.in)
			if err == nil || reply != nil {
				t.Fatalf("非法推进请求竟然成功：%+v %v", reply, err)
			}
			if !errors.Is(err, model.ErrRequestRequired) && !errors.Is(err, model.ErrRequestIDRequired) {
				t.Fatalf("err = %v，期望入参类哨兵", err)
			}
			if got := len(f.store.created); got != 0 {
				t.Fatalf("入参未过就写对象：%v", f.store.created)
			}
		})
	}
	// 任务不存在要明确报 ErrTaskNotFound，而不是回一个空任务。
	if _, err := logic.RunAuditExportTask(&rpc.RunAuditExportTaskReq{
		Ctx: testCallContext("r-3"), TaskId: 4242,
	}); !errors.Is(err, model.ErrTaskNotFound) {
		t.Fatalf("err = %v，期望 ErrTaskNotFound", err)
	}
}

func TestRunAuditExportTaskJSONFormatHasNoHeaderAndStaysParsable(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	rows := seedChain(model.ChainKey("media", testNow), 2, 1700)
	f.putRows(rows...)
	f.entries.scanPages = [][]*model.AuditEntry{rows}
	task := f.seedExportTask(t, "req-run-json", "json", legalExportFilterJSON, nil)
	if _, err := NewRunAuditExportTaskLogic(context.Background(), svcCtx()).
		RunAuditExportTask(&rpc.RunAuditExportTaskReq{Ctx: testCallContext("req-run-json-ctx"), TaskId: task.TaskID}); err != nil {
		t.Fatal(err)
	}
	obj := f.store.objects[exportObjectKey(task.RequestID, "json")]
	if obj == nil {
		t.Fatal("对象未落地")
	}
	body := string(obj.buf)
	if strings.HasPrefix(body, "entry_id") {
		t.Fatal("json 格式混进了 CSV 表头")
	}
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("JSONL 行数 = %d，期望每行一条：%q", len(lines), body)
	}
	for _, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Fatalf("JSONL 单行不是合法 JSON：%s", line)
		}
	}
}

// 空结果也要能收敛成 succeeded：导出 0 条是合法结论，不是故障。
func TestRunAuditExportTaskEmptyResultSucceedsWithZeroRows(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	task := f.seedExportTask(t, "req-run-empty", "csv", legalExportFilterJSON, nil)
	f.entries.scanPages = [][]*model.AuditEntry{nil}
	reply, err := NewRunAuditExportTaskLogic(context.Background(), svcCtx()).
		RunAuditExportTask(&rpc.RunAuditExportTaskReq{Ctx: testCallContext("req-run-empty-ctx"), TaskId: task.TaskID})
	if err != nil {
		t.Fatalf("条件命中 0 条不是调用失败：%v", err)
	}
	if reply.GetExportedRows() != 0 || !reply.GetFinished() {
		t.Fatalf("回参：%+v", reply)
	}
	fresh := f.mustExportTask(t, task.TaskID)
	if fresh.State != model.ExportStateSucceeded || fresh.RowCount != 0 {
		t.Fatalf("空导出终态：%+v", fresh)
	}
	obj := f.store.objects[exportObjectKey(task.RequestID, "csv")]
	records, derr := csv.NewReader(strings.NewReader(string(obj.buf))).ReadAll()
	if derr != nil || len(records) != 1 {
		t.Fatalf("空导出应只剩表头行：%v %v", records, derr)
	}
}

// --- 取件 ---

func TestGetAuditExportOnlySucceededTasksHandOutAUrl(t *testing.T) {
	ctx := context.Background()
	for _, st := range []string{model.ExportStatePending, model.ExportStateRunning, model.ExportStateFailed,
		model.ExportStateCanceled, model.ExportStateExpired} {
		t.Run(st, func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			f.store.presign = "https://oss.example/audit-export/x.csv?sig=SECRET-abcdef"
			task := f.seedExportTask(t, "req-get-"+st, "csv", legalExportFilterJSON,
				func(e *model.ExportTask) { e.State = st })
			reply, err := NewGetAuditExportLogic(ctx, svcCtx()).
				GetAuditExport(&rpc.GetAuditExportReq{Ctx: testCallContext("req-get-ctx"), TaskId: task.TaskID})
			if err != nil {
				t.Fatalf("未就绪任务不是调用失败：%v", err)
			}
			if reply.GetDownloadUrl() != "" || reply.GetUrlExpireAt() != 0 {
				t.Fatalf("state=%s 却回了下载地址：%+v", st, reply)
			}
			if reply.GetTask().GetState() != st {
				t.Fatalf("回参状态 = %s，期望原样回报 %s", reply.GetTask().GetState(), st)
			}
			// 没签发地址就不该打对象存储，也不该留下「取件成功」的痕。
			if len(f.store.presignKey) != 0 {
				t.Fatalf("state=%s 仍签了地址：%v", st, f.store.presignKey)
			}
			if got := f.exportTrailCount(); got != 0 {
				t.Fatalf("state=%s 未发地址却写了 %d 条留痕", st, got)
			}
		})
	}
}

func TestGetAuditExportSucceedsThenTrailsThePickup(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	const signature = "SECRET-abcdef"
	f.store.presign = "https://oss.example/audit-export/x.csv?sig=" + signature
	fileHash := strings.Repeat("a", model.HashHexLen)
	task := f.seedExportTask(t, "req-get-ok", "csv", legalExportFilterJSON, func(e *model.ExportTask) {
		e.State = model.ExportStateSucceeded
		e.Bucket = f.opts.Storage.Bucket
		e.ObjectKey = exportObjectKey("req-get-ok", "csv")
		e.FileHash = fileHash
		e.ExpireAt = testNow + 3600
		e.RowCount = 5
	})
	reply, err := NewGetAuditExportLogic(context.Background(), svcCtx()).
		GetAuditExport(&rpc.GetAuditExportReq{Ctx: testCallContext("req-get-req"), TaskId: task.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	if reply.GetDownloadUrl() != f.store.presign {
		t.Fatalf("地址 = %q", reply.GetDownloadUrl())
	}
	if want := testNow + f.opts.PresignTTLSeconds; reply.GetUrlExpireAt() != want {
		t.Fatalf("url_expire_at = %d，期望 %d", reply.GetUrlExpireAt(), want)
	}
	if got := f.store.presignTTL; len(got) != 1 || got[0] != f.opts.PresignTTLSeconds {
		t.Fatalf("签发的 TTL 未跟随配置：%v", f.store.presignTTL)
	}
	if got := f.store.presignKey; len(got) != 1 || got[0] != task.Bucket+"/"+task.ObjectKey {
		t.Fatalf("签发对象定位 = %v", f.store.presignKey)
	}
	// 取件是「存证离开本系统」的出口，必须留痕。
	trail := f.exportTrail(t)
	last := trail[len(trail)-1]
	if last.Action != actionExportDownload || last.CallerService != selfCallerService {
		t.Fatalf("取件留痕异常：%+v", last)
	}
	if !model.ValidDigest(last.BeforeDigest) {
		t.Fatalf("取件留痕摘要非法：%q", last.BeforeDigest)
	}
	// 签出来的地址是凭证：它不得出现在任何库里列。
	assertNoRaw(t, "取件留痕", stringFields(last), signature, f.store.presign)
	// file_hash 只能以摘要形态留痕。
	if strings.Contains(last.BeforeDigest, fileHash) {
		t.Fatalf("留痕抄了 file_hash 原文：%q", last.BeforeDigest)
	}
}

func TestGetAuditExportConvergesExpiredObject(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	f.store.presign = "https://oss.example/x?sig=abc"
	task := f.seedExportTask(t, "req-expire", "csv", legalExportFilterJSON, func(e *model.ExportTask) {
		e.State = model.ExportStateSucceeded
		e.Bucket = "b"
		e.ObjectKey = "k"
		e.FileHash = strings.Repeat("b", model.HashHexLen)
		e.ExpireAt = testNow // 正好到期：>= 判定必须算已过期
	})
	reply, err := NewGetAuditExportLogic(context.Background(), svcCtx()).
		GetAuditExport(&rpc.GetAuditExportReq{Ctx: testCallContext("req-expire-ctx"), TaskId: task.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	if reply.GetDownloadUrl() != "" {
		t.Fatal("已到期对象不得继续签发地址")
	}
	if reply.GetTask().GetState() != model.ExportStateExpired {
		t.Fatalf("状态 = %s，期望 expired", reply.GetTask().GetState())
	}
	if got := f.exports.transitions; len(got) != 1 ||
		got[0] != model.ExportStateSucceeded+"→"+model.ExportStateExpired {
		t.Fatalf("状态推进 = %v", f.exports.transitions)
	}
	if len(f.store.presignKey) != 0 {
		t.Fatalf("到期任务仍去签了地址：%v", f.store.presignKey)
	}
	if got := f.exportTrailCount(); got != 0 {
		t.Fatalf("到期不发地址却写了 %d 条取件留痕", got)
	}
	// 状态收敛是不可逆的：再问一次仍是 expired，不会又回到 succeeded。
	if again := f.mustExportTask(t, task.TaskID); again.State != model.ExportStateExpired {
		t.Fatalf("库里状态 = %s", again.State)
	}
	// 差一秒都不算过期。
	f2 := newFixture(t)
	f2.use(t)
	f2.store.presign = "https://oss.example/x?sig=abc"
	task2 := f2.seedExportTask(t, "req-not-expired", "csv", legalExportFilterJSON, func(e *model.ExportTask) {
		e.State = model.ExportStateSucceeded
		e.Bucket = "b"
		e.ObjectKey = "k"
		e.FileHash = strings.Repeat("b", model.HashHexLen)
		e.ExpireAt = testNow + 1
	})
	r2, err2 := NewGetAuditExportLogic(context.Background(), svcCtx()).
		GetAuditExport(&rpc.GetAuditExportReq{Ctx: testCallContext("req-2"), TaskId: task2.TaskID})
	if err2 != nil || r2.GetDownloadUrl() == "" {
		t.Fatalf("差一秒到期就拒发地址：%+v %v", r2, err2)
	}
}

func TestGetAuditEntryAndExportRequireLocators(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.use(t)
	cases := []struct {
		name    string
		call    func() error
		wantErr error
	}{
		{"导出任务：两个键都不给", func() error {
			_, err := NewGetAuditExportLogic(ctx, svcCtx()).
				GetAuditExport(&rpc.GetAuditExportReq{Ctx: testCallContext("g1")})
			return err
		}, model.ErrTaskIDOrRequestIDRequired},
		{"导出任务：request_id 只有空白", func() error {
			_, err := NewGetAuditExportLogic(ctx, svcCtx()).
				GetAuditExport(&rpc.GetAuditExportReq{Ctx: testCallContext("g2"), RequestId: "  "})
			return err
		}, model.ErrTaskIDOrRequestIDRequired},
		{"导出任务：读接口也要幂等键", func() error {
			_, err := NewGetAuditExportLogic(ctx, svcCtx()).GetAuditExport(&rpc.GetAuditExportReq{
				Ctx: &rpc.CallContext{CallerService: "admin-api"}, TaskId: 1})
			return err
		}, model.ErrRequestIDRequired},
		{"导出任务：task_id 不存在", func() error {
			_, err := NewGetAuditExportLogic(ctx, svcCtx()).
				GetAuditExport(&rpc.GetAuditExportReq{Ctx: testCallContext("g3"), TaskId: 999})
			return err
		}, model.ErrTaskNotFound},
		{"导出任务：request_id 不存在", func() error {
			_, err := NewGetAuditExportLogic(ctx, svcCtx()).
				GetAuditExport(&rpc.GetAuditExportReq{Ctx: testCallContext("g4"), RequestId: "nope"})
			return err
		}, model.ErrTaskNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
		})
	}
	// 数据面故障不得降级成「任务不存在」。
	f.exports.findErr = errors.New("audit_export_task FindOne: Error 1146: table doesn't exist")
	defer func() { f.exports.findErr = nil }()
	reply, err := NewGetAuditExportLogic(ctx, svcCtx()).
		GetAuditExport(&rpc.GetAuditExportReq{Ctx: testCallContext("g5"), TaskId: 1})
	if err == nil || reply != nil {
		t.Fatalf("库故障必须报错：%+v %v", reply, err)
	}
	if errors.Is(err, model.ErrTaskNotFound) {
		t.Fatalf("库故障被当成了「任务不存在」：%v", err)
	}
}

func TestGetAuditExportByRequestIDMatchesByTaskID(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	task := f.seedExportTask(t, "req-both-ways", "csv", legalExportFilterJSON, nil)
	byReq, err := NewGetAuditExportLogic(context.Background(), svcCtx()).
		GetAuditExport(&rpc.GetAuditExportReq{Ctx: testCallContext("w1"), RequestId: task.RequestID})
	if err != nil {
		t.Fatal(err)
	}
	byID, err := NewGetAuditExportLogic(context.Background(), svcCtx()).
		GetAuditExport(&rpc.GetAuditExportReq{Ctx: testCallContext("w2"), TaskId: task.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	if byReq.GetTask().GetTaskId() != byID.GetTask().GetTaskId() {
		t.Fatalf("两种定位口径找到了不同任务：%d vs %d",
			byReq.GetTask().GetTaskId(), byID.GetTask().GetTaskId())
	}
	if byReq.GetDownloadUrl() != "" {
		t.Fatal("pending 任务不该有地址")
	}
}
