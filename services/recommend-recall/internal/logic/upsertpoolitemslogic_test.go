// 本文件钉住 UpsertPoolItems 的写入语义：条目写入与版本行的关系（首批登记 BUILDING、
// 末批封版 READY）、主键冲突时是插入还是更新、批量上下界、失败后库里留下什么形态，
// 以及最关键的一条 —— **绝不写别的域**（不碰 recall_pool_current、不产生 outbox 事件：
// 指针切换是 Publish/Rollback 的专属动作，AGENTS.md §5）。
package logic

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"go-video/common/idempotency"
	"go-video/services/recommend-recall/internal/config"
	"go-video/services/recommend-recall/internal/repository"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"
)

func upsertReq(version int64, batchID string, items ...*rpc.PoolItemInput) *rpc.UpsertPoolItemsReq {
	return &rpc.UpsertPoolItemsReq{
		Pool: testPoolRef(srcHot32, "global"), Version: version, BatchId: batchID,
		Generator: "cron-pool-builder", IdempotencyKey: "ups-" + batchID + "-" + fmt.Sprint(len(items)),
		Items: items,
	}
}

func aids(aid int64, more ...int64) []*rpc.PoolItemInput {
	out := []*rpc.PoolItemInput{{Aid: aid, Score: 1}}
	for _, a := range more {
		out = append(out, &rpc.PoolItemInput{Aid: a, Score: 1})
	}
	return out
}

// scorePair 给一条带分数的条目。
func scorePair(aid int64, score float64) *rpc.PoolItemInput {
	return &rpc.PoolItemInput{Aid: aid, Score: score}
}

func upsertReplyState(reply *rpc.UpsertPoolItemsReply) rpc.PoolVersionState {
	return reply.GetState()
}

// 入参门禁逐项拒绝，且不触达任何依赖（认领幂等键都在门禁之后，避免坏请求占住租约）。
func TestUpsertPoolItemsGuardsRejectBeforeAnyDependencyCall(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*rpc.UpsertPoolItemsReq)
		nilReq   bool
		cause    error
		contains string
	}{
		{"nil 请求", nil, true, model.ErrRequestRequired, "UpsertPoolItemsReq"},
		{"缺池", func(r *rpc.UpsertPoolItemsReq) { r.Pool = nil }, false, model.ErrInvalidPoolKey, ""},
		{"版本 0", func(r *rpc.UpsertPoolItemsReq) { r.Version = 0 }, false, model.ErrInvalidVersion, "version=0"},
		{"批次号空白", func(r *rpc.UpsertPoolItemsReq) { r.BatchId = " " }, false, model.ErrBatchIDRequired, "batch_id required"},
		{"批次号超长", func(r *rpc.UpsertPoolItemsReq) { r.BatchId = strings.Repeat("b", colRefID+1) }, false,
			model.ErrFieldTooLong, "batch_id"},
		{"产出方空白", func(r *rpc.UpsertPoolItemsReq) { r.Generator = "" }, false, model.ErrOperatorRequired, "generator required"},
		{"产出方超长", func(r *rpc.UpsertPoolItemsReq) { r.Generator = strings.Repeat("g", colRefID+1) }, false,
			model.ErrFieldTooLong, "generator"},
		{"幂等键空白", func(r *rpc.UpsertPoolItemsReq) { r.IdempotencyKey = "" }, false,
			model.ErrIdempotencyKeyRequired, "idempotency_key required"},
		{"条目结构版本不支持", func(r *rpc.UpsertPoolItemsReq) { r.SchemaVersion = 99 }, false,
			model.ErrSchemaVersionUnsupported, "request=99"},
		{"空批次", func(r *rpc.UpsertPoolItemsReq) { r.Items = nil }, false, model.ErrItemsRequired, ""},
		{"批次超配置上限", func(r *rpc.UpsertPoolItemsReq) { r.Items = manyItems(testBatchMax + 1) }, false,
			model.ErrTooManyItems, fmt.Sprintf("> %d", testBatchMax)},
		{"aid 非正数", func(r *rpc.UpsertPoolItemsReq) { r.Items = []*rpc.PoolItemInput{scorePair(1, 1), scorePair(0, 2)} },
			false, model.ErrInvalidAid, "items[1].aid=0"},
		{"分数 NaN", func(r *rpc.UpsertPoolItemsReq) { r.Items = []*rpc.PoolItemInput{scorePair(7, math.NaN())} },
			false, model.ErrInvalidScore, "items[0]"},
		{"分数 +Inf", func(r *rpc.UpsertPoolItemsReq) { r.Items = []*rpc.PoolItemInput{scorePair(7, math.Inf(1))} },
			false, model.ErrInvalidScore, "score=+Inf"},
		{"同批重复 aid", func(r *rpc.UpsertPoolItemsReq) {
			r.Items = []*rpc.PoolItemInput{scorePair(7, 1), scorePair(8, 2), scorePair(7, 3)}
		}, false, model.ErrDuplicateItem, "items[2].aid=7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			req := upsertReq(6, "b1", aids(11, 12, 13)...)
			if tc.mutate != nil {
				tc.mutate(req)
			}
			var in *rpc.UpsertPoolItemsReq
			if !tc.nilReq {
				in = req
			}
			writesBefore := e.st.writes
			_, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(in)
			requireErrIs(t, err, tc.cause)
			if tc.contains != "" && !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("错误文本缺定位信息 %q：%v", tc.contains, err)
			}
			if got := e.calls().total(); got != 0 {
				t.Fatalf("门禁拒绝后仍有 %d 次依赖调用：%v", got, e.calls().ops())
			}
			if e.st.writes != writesBefore {
				t.Fatalf("门禁拒绝后仍发生了写入：%d -> %d", writesBefore, e.st.writes)
			}
		})
	}
}

const testBatchMax = 1000 // 与 testRecallConf().MaxBatchItems 同值

// 必需依赖缺失是装配缺陷，不是可降级状态：必须明确报错，
// 不能回一个"写入 0 条、状态 BUILDING"的成功响应（那会让作业以为已经写完并去封版）。
func TestUpsertPoolItemsWithoutRepositoryFailsInsteadOfReportingZeroWritten(t *testing.T) {
	e := newEnv(t)
	e.svcCtx.Repository = nil
	_, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(
		upsertReq(6, "b1", scorePair(11, 9)))
	requireErrIs(t, err, repository.ErrSourceNotConfigured)
	if got := e.calls().total(); got != 0 {
		t.Fatalf("依赖未装配却仍有调用：%v", e.calls().ops())
	}
}

// 恰好等于配置上限的一批必须放行：上限是"拒绝"而不是"裁剪"，边界值走哪边要写死。
func TestUpsertPoolItemsAcceptsExactlyMaxBatchItems(t *testing.T) {
	e := newEnv(t)
	_, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(
		upsertReq(6, "b-full", manyItems(testBatchMax)...))
	if err != nil {
		t.Fatalf("恰好 %d 条应被接受（超限才拒）：%v", testBatchMax, err)
	}
	if got := e.st.itemCountOfVersion(srcHot32, "global", 6); got != testBatchMax {
		t.Fatalf("落库条目数=%d，期望 %d", got, testBatchMax)
	}
}

// 上限来自配置而不是模型常量：配置调小后同一批必须被拒（模型层 2000 的上限是第二道独立闸门）。
func TestUpsertPoolItemsBatchCapComesFromConfigNotModelConstant(t *testing.T) {
	e := newEnvWithRecall(t, func(rc *config.RecallConf) { rc.MaxBatchItems = 2 })
	_, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(
		upsertReq(6, "b-cap", aids(11, 12, 13)...))
	requireErrIs(t, err, model.ErrTooManyItems)
	if !strings.Contains(err.Error(), "items=3 > 2") {
		t.Fatalf("错误没回显配置上限：%v", err)
	}
	if got := e.calls().total(); got != 0 {
		t.Fatalf("配置闸门却触达了依赖：%v", e.calls().ops())
	}
}

// 首批写入：登记 BUILDING 版本 + 写条目 + 回填 item_count，全部在同一个事务里；
// 指针表一行都不许碰（不建壳、不切换），也不产生任何事件。
func TestUpsertPoolItemsFirstBatchRegistersBuildingVersionInsideOneTransaction(t *testing.T) {
	e := newEnv(t)

	reply, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(
		upsertReq(6, "b1", scorePair(11, 9), scorePair(12, 8), scorePair(13, 7)))
	if err != nil {
		t.Fatalf("首批写入失败：%v", err)
	}
	if reply.GetVersion() != 6 || reply.GetWritten() != 3 || reply.GetItemCount() != 3 {
		t.Fatalf("应答=%+v，期望 version=6 written=3 item_count=3", reply)
	}
	if upsertReplyState(reply) != rpc.PoolVersionState_POOL_VERSION_STATE_BUILDING {
		t.Fatalf("未声明末批就封版了：%s", reply.GetState())
	}
	if reply.GetDeduplicated() {
		t.Fatal("首次执行不是重放")
	}

	assertOps(t, e.calls(), 0,
		"Idempotency.Claim", "Transact", "PoolVersion.FindForUpdate", "PoolVersion.Register",
		"PoolVersion.FindForUpdate", "Pool.BatchUpsertInTx", "Pool.CountByVersionInTx",
		"PoolVersion.UpdateItemCount", "Idempotency.MarkSucceeded", "Transact.Commit")

	ver, ok := e.st.versionOf(srcHot32, "global", 6)
	if !ok {
		t.Fatal("版本登记行没建出来")
	}
	if ver.State != model.VersionStateBuilding || ver.BatchID != "b1" || ver.ItemCount != 3 ||
		ver.SchemaVersion != model.PoolVersionSchemaVersion || ver.Operator != "cron-pool-builder" ||
		ver.PublishedAt != 0 {
		t.Fatalf("版本行形态错：%+v", ver)
	}
	if got := e.st.itemAids(srcHot32, "global", 6); !sameInt64(got, []int64{11, 12, 13}) {
		t.Fatalf("条目落库不对：%v", got)
	}
	assertPointerUntouched(t, e)
	assertNoOutboxRows(t, e)
}

// 末批封版：BUILDING -> READY 走条件更新（from=BUILDING），State/Sealed 反映在应答里。
func TestUpsertPoolItemsLastBatchSealsVersionToReady(t *testing.T) {
	e := newEnv(t)
	req := upsertReq(6, "b1", scorePair(11, 9), scorePair(12, 8))
	req.IsLastBatch = true

	reply, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(req)
	if err != nil {
		t.Fatalf("末批写入失败：%v", err)
	}
	if upsertReplyState(reply) != rpc.PoolVersionState_POOL_VERSION_STATE_READY {
		t.Fatalf("末批应答状态=%s，期望 READY", reply.GetState())
	}
	if reply.GetItemCount() != 2 || reply.GetWritten() != 2 {
		t.Fatalf("应答=%+v", reply)
	}
	ver := stateAt(e, 6)
	if ver.State != model.VersionStateReady {
		t.Fatalf("版本行没封版：%+v", ver)
	}
	assertOps(t, e.calls(), 8, "PoolVersion.UpdateState", "Idempotency.MarkSucceeded", "Transact.Commit")
	assertPointerUntouched(t, e)
}

// 已封版/已上线/已退役/已失败的版本一律不可原地追加条目 —— 这是"已发布版本不可变"的落点。
// 拒绝必须发生在 Register 之前（Register 的 ODKU 会刷新元数据，也是一种改写）。
func TestUpsertPoolItemsRefusesToRewriteSealedVersions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state int32
	}{
		{"READY 已封版", model.VersionStateReady},
		{"CURRENT 在线", model.VersionStateCurrent},
		{"RETIRED 已退役", model.VersionStateRetired},
		{"FAILED 已失败", model.VersionStateFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.st.seedVersion(srcHot32, "global", 6, "b1", tc.state, 3)
			e.st.seedItems(srcHot32, "global", 6, item{11, 9}, item{12, 8}, item{13, 7})
			itemsBefore := e.st.totalItemRows()

			_, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(
				upsertReq(6, "b1", scorePair(14, 5)))
			requireErrIs(t, err, model.ErrVersionImmutable)
			if !strings.Contains(err.Error(), "state=") {
				t.Fatalf("错误没带当前状态：%v", err)
			}
			if !strings.Contains(err.Error(), "PublishPoolVersion") {
				t.Fatalf("错误必须指向正确出路（写新版本再切指针）：%v", err)
			}
			if got := e.st.totalItemRows(); got != itemsBefore {
				t.Fatalf("被拒的写入仍落了条目：%d -> %d", itemsBefore, got)
			}
			if ver := stateAt(e, 6); ver.State != tc.state || ver.ItemCount != 3 {
				t.Fatalf("被拒的写入改写了版本行：%+v", ver)
			}
			if e.calls().has("Pool.BatchUpsertInTx") || e.calls().has("PoolVersion.Register") {
				t.Fatalf("不可变版本竟进了写入步骤：%v", e.calls().ops())
			}
			if !e.calls().has("Transact.Rollback") {
				t.Fatalf("没记录回滚：%v", e.calls().ops())
			}
		})
	}
}

// 同一版本号被另一个批次复用必须硬失败（两个作业串写同一版本）。
// 拒绝依据是登记行的 batch_id，不是请求里带的批次号。
func TestUpsertPoolItemsRejectsVersionOwnedByAnotherBatch(t *testing.T) {
	e := newEnv(t)
	e.st.seedVersion(srcHot32, "global", 6, "batch-A", model.VersionStateBuilding, 3)
	e.st.seedItems(srcHot32, "global", 6, item{11, 9}, item{12, 8}, item{13, 7})

	_, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(
		upsertReq(6, "batch-B", scorePair(14, 5)))
	requireErrIs(t, err, model.ErrVersionReuseBlocked)
	for _, want := range []string{"belongs to batch \"batch-A\"", "refused batch \"batch-B\""} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误没同时给出双方批次号（调用方无法判断谁的版本）：%v", err)
		}
	}
	if got := e.st.itemAids(srcHot32, "global", 6); !sameInt64(got, []int64{11, 12, 13}) {
		t.Fatalf("串写被拒后条目被改了：%v", got)
	}
	if ver := stateAt(e, 6); ver.BatchID != "batch-A" {
		t.Fatalf("版本归属被改了：%+v", ver)
	}
}

// 主键冲突走 ON DUPLICATE KEY UPDATE：重放同一批不产生第二行，
// written 是 MySQL 的影响行数（新插入=1、值未变=0、更新=2）而不是新增行数，
// item_count 由事务内 COUNT(*) 回填而不是"旧值 + 本批条数"。
func TestUpsertPoolItemsOnConflictUpdatesInPlaceAndCountsFromDatabase(t *testing.T) {
	e := newEnv(t)
	first := upsertReq(6, "b1", scorePair(11, 9), scorePair(12, 8), scorePair(13, 7))
	first.IdempotencyKey = "ups-1"
	if _, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(first); err != nil {
		t.Fatalf("首批失败：%v", err)
	}

	// 同一批原样再写一次（换了幂等键，所以不是幂等重放而是真的又执行了一遍）。
	again := upsertReq(6, "b1", scorePair(11, 9), scorePair(12, 8), scorePair(13, 7))
	again.IdempotencyKey = "ups-2"
	reply, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(again)
	if err != nil {
		t.Fatalf("重放同批失败：%v", err)
	}
	if reply.GetWritten() != 0 {
		t.Fatalf("值完全未变时 ODKU 影响行数应为 0，实际 %d", reply.GetWritten())
	}
	if reply.GetItemCount() != 3 {
		t.Fatalf("重放同一批后 item_count=%d，期望仍是 3（不能用旧值+本批条数回填）", reply.GetItemCount())
	}
	if got := e.st.itemAids(srcHot32, "global", 6); !sameInt64(got, []int64{11, 12, 13}) {
		t.Fatalf("重放产生了额外条目：%v", got)
	}
	if n := e.st.itemCountOfVersion(srcHot32, "global", 6); n != 3 {
		t.Fatalf("库里的条目行数=%d，期望 3", n)
	}

	// 改分数再来一次：影响行数=2*条数，条数不变。
	update := upsertReq(6, "b1", scorePair(11, 100), scorePair(12, 100), scorePair(13, 100))
	update.IdempotencyKey = "ups-3"
	reply, err = NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(update)
	if err != nil {
		t.Fatalf("更新分数失败：%v", err)
	}
	if reply.GetWritten() != 6 || reply.GetItemCount() != 3 {
		t.Fatalf("更新分数的应答=%+v，期望 written=6（2*3 影响行数）item_count=3", reply)
	}
	// 末批追加一条新 aid：条数按 COUNT(*) 变 4。
	appended := upsertReq(6, "b1", scorePair(14, 1))
	appended.IsLastBatch = true
	appended.IdempotencyKey = "ups-4"
	reply, err = NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(appended)
	if err != nil {
		t.Fatalf("追加条目失败：%v", err)
	}
	if reply.GetWritten() != 1 || reply.GetItemCount() != 4 {
		t.Fatalf("追加一条的应答=%+v，期望 written=1 item_count=4", reply)
	}
	if ver := stateAt(e, 6); ver.State != model.VersionStateReady || ver.ItemCount != 4 {
		t.Fatalf("末批后版本行形态错：%+v", ver)
	}
}

// 分批累积：第二批只写自己那几条，item_count 是全版本累计而不是本批条数。
func TestUpsertPoolItemsSecondBatchReportsCumulativeItemCount(t *testing.T) {
	e := newEnv(t)
	if _, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(
		upsertReq(6, "b1", scorePair(11, 9), scorePair(12, 8))); err != nil {
		t.Fatalf("首批失败：%v", err)
	}
	e.calls().entries = nil

	second := upsertReq(6, "b1", scorePair(13, 7), scorePair(14, 6), scorePair(15, 5))
	second.IsLastBatch = true
	second.IdempotencyKey = "ups-second"
	reply, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(second)
	if err != nil {
		t.Fatalf("第二批失败：%v", err)
	}
	if reply.GetWritten() != 3 || reply.GetItemCount() != 5 {
		t.Fatalf("应答=%+v，期望 written=3 item_count=5（累计）", reply)
	}
	// 第二批不再重读一次不存在的行：FindForUpdate 命中即可，Register 只刷元数据。
	assertOps(t, e.calls(), 0,
		"Idempotency.Claim", "Transact", "PoolVersion.FindForUpdate", "PoolVersion.Register",
		"Pool.BatchUpsertInTx", "Pool.CountByVersionInTx", "PoolVersion.UpdateItemCount",
		"PoolVersion.UpdateState", "Idempotency.MarkSucceeded", "Transact.Commit")
	ver := stateAt(e, 6)
	if ver.State != model.VersionStateReady || ver.ItemCount != 5 || ver.BatchID != "b1" {
		t.Fatalf("封版后的版本行：%+v", ver)
	}
}

// 封版那一步的条件更新未命中（状态被别人推过）必须整事务失败：
// 条目与幂等标记一起回滚，不能留下"条目已落但版本状态未知"的半成品。
func TestUpsertPoolItemsSealConflictRollsBackWholeTransaction(t *testing.T) {
	e := newEnv(t)
	// 版本行必须在事务开始前就存在：对手那条 UPDATE 是"已提交"的变更，
	// 只能落在本事务快照之外已有的行上（真实的 InnoDB 里，未提交的 INSERT 根本不会被别人改到）。
	// 若让首批 upsert 自己建行，回滚会连行一起抹掉，重放的对手变更就无处附着。
	e.st.seedVersion(srcHot32, "global", 6, "b1", model.VersionStateBuilding, 0)
	e.st.on("PoolVersion.UpdateState", func(args ...interface{}) {
		e.st.commitExternally(func() {
			e.st.versions[rowKey{source: srcHot32, poolKey: "global", version: 6}].State = model.VersionStateFailed
		})
	})
	req := upsertReq(6, "b1", scorePair(11, 9), scorePair(12, 8))
	req.IsLastBatch = true

	_, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(req)
	requireErrIs(t, err, model.ErrVersionStateConflict)
	// 对手把行推成 FAILED 是已提交事实，必须留下；本事务的条目则必须消失。
	if n := e.st.itemCountOfVersion(srcHot32, "global", 6); n != 0 {
		t.Fatalf("封版失败后条目没回滚干净，剩 %d 行", n)
	}
	ver, ok := e.st.versionOf(srcHot32, "global", 6)
	if !ok || ver.State != model.VersionStateFailed {
		t.Fatalf("外部已提交的状态被误回滚：%+v ok=%t", ver, ok)
	}
	if ok && ver.ItemCount != 0 {
		t.Fatalf("本事务的 item_count 回填没回滚：%+v", ver)
	}
	if claim, _ := e.st.claimOf(model.IdempotencyScopeUpsertPoolItems, "ups-b1-2"); claim.State !=
		string(idempotency.StateFailed) {
		t.Fatalf("失败后幂等键没放回可重试状态：%+v", claim)
	}
}

// 中途存储故障：首批登记的版本行也要跟着回滚（不能留下 item_count=0 的 BUILDING 孤儿行）。
func TestUpsertPoolItemsMidwayFailureLeavesNoHalfWrittenVersion(t *testing.T) {
	e := newEnv(t)
	e.st.failOn("Pool.BatchUpsertInTx", errStoreDown)

	_, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(
		upsertReq(6, "b1", scorePair(11, 9), scorePair(12, 8)))
	requireErrIs(t, err, errStoreDown)
	if _, ok := e.st.versionOf(srcHot32, "global", 6); ok {
		t.Fatalf("条目写入失败但 BUILDING 版本行留下了：%+v", stateAt(e, 6))
	}
	if n := e.st.itemCountOfVersion(srcHot32, "global", 6); n != 0 {
		t.Fatalf("失败仍留下 %d 条条目", n)
	}
	assertPointerUntouched(t, e)
	assertNoOutboxRows(t, e)
	claim, ok := e.st.claimOf(model.IdempotencyScopeUpsertPoolItems, "ups-b1-2")
	if !ok || claim.State != string(idempotency.StateFailed) {
		t.Fatalf("幂等键没落库或状态错：%+v ok=%t", claim, ok)
	}
	if !strings.Contains(claim.LastError, "storage is down") {
		t.Fatalf("last_error 没记下原始故障（排障时只剩一个 failed）：%+v", claim)
	}
}

// 同键同参数重放：回放原结果并标 deduplicated，不再写一次条目、不再产生第二次封版。
func TestUpsertPoolItemsReplayDoesNotWriteTwice(t *testing.T) {
	e := newEnv(t)
	req := upsertReq(6, "b1", scorePair(11, 9), scorePair(12, 8))
	req.IsLastBatch = true
	logic := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx)
	first, err := logic.UpsertPoolItems(req)
	if err != nil {
		t.Fatalf("首执行失败：%v", err)
	}
	entriesBefore := e.calls().total()

	again, err := logic.UpsertPoolItems(req)
	if err != nil {
		t.Fatalf("重放失败：%v", err)
	}
	if !again.GetDeduplicated() {
		t.Fatal("同键同参数没标 deduplicated")
	}
	if again.GetVersion() != first.GetVersion() || again.GetWritten() != first.GetWritten() ||
		again.GetItemCount() != first.GetItemCount() || again.GetState() != first.GetState() {
		t.Fatalf("重放应答与原执行不一致：%+v vs %+v", again, first)
	}
	assertOps(t, e.calls(), entriesBefore, "Idempotency.Claim")
	if n := e.st.itemCountOfVersion(srcHot32, "global", 6); n != 2 {
		t.Fatalf("重放后条目行数=%d，期望 2", n)
	}
	if ver := stateAt(e, 6); ver.State != model.VersionStateReady {
		t.Fatalf("重放改写了版本状态：%+v", ver)
	}
}

// 同键不同条数/不同 aid 集合：指纹不同即硬失败，绝不"执行其中一份语义"。
func TestUpsertPoolItemsSameKeyDifferentPayloadIsRejected(t *testing.T) {
	e := newEnv(t)
	req := upsertReq(6, "b1", scorePair(11, 9), scorePair(12, 8))
	req.IdempotencyKey = "shared"
	if _, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(req); err != nil {
		t.Fatalf("首执行失败：%v", err)
	}

	conflict := upsertReq(6, "b1", scorePair(11, 9), scorePair(12, 8), scorePair(13, 7))
	conflict.IdempotencyKey = "shared"
	_, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(conflict)
	requireErrIs(t, err, model.ErrIdempotencyFingerprintMismatch)
	if n := e.st.itemCountOfVersion(srcHot32, "global", 6); n != 2 {
		t.Fatalf("指纹冲突仍落了条目：%d 行", n)
	}

	otherVersion := upsertReq(7, "b1", scorePair(11, 9), scorePair(12, 8))
	otherVersion.IdempotencyKey = "shared"
	if _, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(otherVersion); !errorsIs(
		err, model.ErrIdempotencyFingerprintMismatch) {
		t.Fatalf("版本号也进指纹，换版本复用键必须被拒：%v", err)
	}
}

// 回放载荷只落控制信息，不落条目原文：
// 条目本身按 (source, pool_key, version) 在 recall_pool 可查，复制进按 expire_at 过期的控制位表
// 等于给候选集做了一份第二事实源（AGENTS.md §5 定位约定）。
func TestUpsertPoolItemsStoredPayloadHoldsNoCandidateRows(t *testing.T) {
	e := newEnv(t)
	req := upsertReq(6, "b1", scorePair(11, 9), scorePair(12, 8))
	req.IsLastBatch = true
	if _, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(req); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	claim, ok := e.st.claimOf(model.IdempotencyScopeUpsertPoolItems, "ups-b1-2")
	if !ok {
		t.Fatal("幂等记录没落库")
	}
	var stored upsertOutcome
	if err := json.Unmarshal([]byte(claim.ResultPayload), &stored); err != nil {
		t.Fatalf("回放载荷解不开：%v（原文 %s）", err, claim.ResultPayload)
	}
	if stored.Version != 6 || stored.BatchID != "b1" || stored.ItemCount != 2 ||
		stored.State != model.VersionStateReady || !stored.Sealed || stored.Deduplicated {
		t.Fatalf("回放载荷内容错：%+v", stored)
	}
	for _, leak := range []string{"11", "12", "score", "aid"} {
		if strings.Contains(claim.ResultPayload, leak) {
			t.Fatalf("回放载荷泄漏了条目信息 %q：%s", leak, claim.ResultPayload)
		}
	}
	// Deduplicated 是 json:"-"：落库再读回不得把首执行标成重放。
	if strings.Contains(claim.ResultPayload, "dedup") {
		t.Fatalf("重放标记进了持久化载荷：%s", claim.ResultPayload)
	}
}

// schema_version 传 0 表示"用服务当前版本"，登记行必须落到服务版本而不是 0：
// 接受 0 进库等于写一批在线读不懂的条目。
func TestUpsertPoolItemsZeroSchemaVersionFallsBackToServiceVersion(t *testing.T) {
	e := newEnv(t)
	req := upsertReq(6, "b1", scorePair(11, 9))
	req.SchemaVersion = 0
	if _, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(req); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	ver := stateAt(e, 6)
	if ver.SchemaVersion != model.PoolVersionSchemaVersion {
		t.Fatalf("登记的条目结构版本=%d，期望服务当前版本 %d", ver.SchemaVersion, model.PoolVersionSchemaVersion)
	}
}

// 写路径只碰自己拥有的两张表：不建/不切指针、不写事件、不改审计表。
// 单独一条用例把它钉死，AGENTS.md §5 的"召回不得改他域主数据"才有可执行的判据。
func TestUpsertPoolItemsWritesNothingOutsidePoolTables(t *testing.T) {
	e := newEnv(t)
	e.st.seedPointer(srcHot32, "global", 5, "batch-v5", 1700000500)
	ptrBefore := pointerNow(e)
	logsBefore := e.st.logCount()

	for _, req := range []*rpc.UpsertPoolItemsReq{
		upsertReq(7, "bA", scorePair(11, 9)),
		lastBatch(upsertReq(7, "bA", scorePair(12, 8))),
		upsertReq(8, "bB", scorePair(21, 9)),
	} {
		if _, err := NewUpsertPoolItemsLogic(e.ctx(), e.svcCtx).UpsertPoolItems(req); err != nil {
			t.Fatalf("写入失败：%v", err)
		}
	}
	if ptr := pointerNow(e); ptr != ptrBefore {
		t.Fatalf("池条目写入动了在线指针：%+v -> %+v", ptrBefore, ptr)
	}
	assertNoOutboxRows(t, e)
	if got := e.st.logCount(); got != logsBefore {
		t.Fatalf("写路径改了审计表：%d -> %d", logsBefore, got)
	}
	for _, op := range e.calls().ops() {
		if strings.HasPrefix(op, "Current.") || strings.HasPrefix(op, "Outbox.") ||
			strings.HasPrefix(op, "RequestLog.") {
			t.Fatalf("出现了越界依赖调用 %s：%v", op, e.calls().ops())
		}
	}
}

// --- 本文件专用装配件 ---

func manyItems(n int) []*rpc.PoolItemInput {
	out := make([]*rpc.PoolItemInput, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &rpc.PoolItemInput{Aid: int64(1000 + i), Score: float64(i)})
	}
	return out
}

func lastBatch(r *rpc.UpsertPoolItemsReq) *rpc.UpsertPoolItemsReq {
	r.IsLastBatch = true
	r.IdempotencyKey = r.GetIdempotencyKey() + "-last"
	return r
}

func assertPointerUntouched(t *testing.T, e *env) {
	t.Helper()
	if _, ok := e.st.pointerOf(srcHot32, "global"); ok {
		t.Fatal("写条目路径建出了 recall_pool_current 行")
	}
	for _, op := range e.calls().ops() {
		if strings.HasPrefix(op, "Current.") {
			t.Fatalf("写条目路径碰了指针表：%s（序列 %v）", op, e.calls().ops())
		}
	}
}

func assertNoOutboxRows(t *testing.T, e *env) {
	t.Helper()
	if rows := e.st.outboxOf(); len(rows) != 0 {
		t.Fatalf("写条目路径产生了事件：%+v", rows)
	}
}

func errorsIs(err, want error) bool {
	return err != nil && strings.Contains(err.Error(), want.Error())
}
