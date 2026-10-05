// 本文件覆盖 PrunePoolVersions（不可逆删除路径，由 services/cron 调用）。
//
// 三道保护必须各自能被证明"确实挡住了"，而不是只写在注释里：
//  1. 保留窗口（PrunableBefore 的水位线：窗口内版本连候选都不进）；
//  2. 指针保护（镜像漂移时 state 说 RETIRED 但指针还指着它 -> 一行都不许动）；
//  3. 事务内复核（FindForUpdate 读到并发上线/并发删除 -> 跳过该版本）。
//
// 另外钉住两条删除语义：删除顺序固定"先条目、后登记行"（反了会留下无登记的孤儿条目），
// 以及 deleted_rows 一律是真实行数（条目行 + 登记行），没有"整体成功"这种含糊值。
package logic

import (
	"context"
	"strings"
	"testing"

	"go-video/services/recommend-recall/internal/config"
	"go-video/services/recommend-recall/internal/repository"
	"go-video/services/recommend-recall/internal/svc"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"
)

func pruneReq(keep int32, maxRows int64, dryRun bool, operator string) *rpc.PrunePoolVersionsReq {
	return &rpc.PrunePoolVersionsReq{
		Pool: testPoolRef(srcHot32, "global"), KeepVersions: keep,
		MaxRows: maxRows, DryRun: dryRun, Operator: operator,
	}
}

// 门禁顺序与"拒绝前一个存储调用都不发起"。
func TestPrunePoolVersionsRejectsArgumentsBeforeAnyQuery(t *testing.T) {
	cases := []struct {
		name string
		req  *rpc.PrunePoolVersionsReq
		want error
	}{
		{"nil 请求", nil, model.ErrRequestRequired},
		{"缺池", &rpc.PrunePoolVersionsReq{Operator: "ops-1", KeepVersions: 2}, model.ErrInvalidPoolKey},
		{"source=0", &rpc.PrunePoolVersionsReq{Pool: &rpc.PoolRef{Source: rpc.Source(0), PoolKey: "global"},
			KeepVersions: 2, Operator: "ops-1"}, model.ErrInvalidSource},
		{"池键语法错", &rpc.PrunePoolVersionsReq{Pool: testPoolRef(srcHot32, "zone:"),
			KeepVersions: 2, Operator: "ops-1"}, model.ErrInvalidPoolKey},
		{"operator 缺失", pruneReq(2, 0, false, "   "), model.ErrOperatorRequired},
		{"operator 超列宽", pruneReq(2, 0, false, strings.Repeat("o", colRefID+1)), model.ErrFieldTooLong},
		{"keep 低于配置下限", pruneReq(1, 0, false, "ops-1"), model.ErrKeepVersionsTooSmall},
		{"keep 超 model 上限", pruneReq(model.MaxVersionListLimit+1, 0, false, "ops-1"), model.ErrLimitTooLarge},
		{"max_rows 负", pruneReq(2, -1, false, "ops-1"), model.ErrInvalidLimit},
		{"max_rows 超配置", pruneReq(2, e_pruneMaxRows+1, false, "ops-1"), model.ErrLimitTooLarge},
	}
	for _, tc := range cases {
		e := newEnv(t)
		reply, err := NewPrunePoolVersionsLogic(e.ctx(), e.svcCtx).PrunePoolVersions(tc.req)
		requireErrIs(t, err, tc.want)
		if reply != nil {
			t.Fatalf("%s：拒绝却回了响应 %+v", tc.name, reply)
		}
		if got := e.calls().total(); got != 0 {
			t.Fatalf("%s：拒绝前已触库 %d 次：%v", tc.name, got, e.calls().ops())
		}
	}

	// 顺序证据：池、operator、keep、max_rows 四道门按实现顺序生效（同时坏时先报前者）。
	e := newEnv(t)
	_, err := NewPrunePoolVersionsLogic(e.ctx(), e.svcCtx).PrunePoolVersions(
		&rpc.PrunePoolVersionsReq{KeepVersions: 0, MaxRows: -1})
	requireErrIs(t, err, model.ErrInvalidPoolKey)
	e2 := newEnv(t)
	_, err = NewPrunePoolVersionsLogic(e2.ctx(), e.svcCtx).PrunePoolVersions(pruneReq(1, -1, false, ""))
	requireErrIs(t, err, model.ErrOperatorRequired)
	e3 := newEnv(t)
	_, err = NewPrunePoolVersionsLogic(e3.ctx(), e.svcCtx).PrunePoolVersions(pruneReq(1, 2001, false, "ops-1"))
	requireErrIs(t, err, model.ErrKeepVersionsTooSmall)

	// max_rows 显式给到 model 硬上限之上（配置 PruneMaxRows=2000 更小，先被配置门挡住）。
	e4 := newEnv(t)
	_, err = NewPrunePoolVersionsLogic(e4.ctx(), e.svcCtx).PrunePoolVersions(pruneReq(2, model.MaxDeleteRows+1, false, "ops-1"))
	requireErrIs(t, err, model.ErrLimitTooLarge)

	// Repository 未装配：本方法有守卫，回 ErrSourceNotConfigured 而不是 panic。
	l := NewPrunePoolVersionsLogic(context.Background(), &svc.ServiceContext{Config: config.Config{}})
	_, err = l.PrunePoolVersions(pruneReq(2, 0, false, "ops-1"))
	requireErrIs(t, err, repository.ErrSourceNotConfigured)
}

// e_pruneMaxRows 与 testRecallConf().PruneMaxRows 同源（不新造数字）。
var e_pruneMaxRows = testRecallConf().PruneMaxRows

// 保留窗口内没有版本可清：如实回全零，且除了水位线这一次查询以外什么都不发。
func TestPrunePoolVersionsStopsWhenEverythingIsInsideTheRollbackWindow(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		e := newEnv(t)
		e.st.seedVersion(srcHot32, "global", 2, "b2", model.VersionStateRetired, 1)
		e.st.seedVersion(srcHot32, "global", 1, "b1", model.VersionStateFailed, 1)
		e.st.seedItems(srcHot32, "global", 2, item{aid: 21, score: 5})
		e.st.seedItems(srcHot32, "global", 1, item{aid: 11, score: 5})
		writesBefore := e.st.writes

		reply, err := NewPrunePoolVersionsLogic(e.ctx(), e.svcCtx).PrunePoolVersions(
			pruneReq(3, 0, dryRun, "ops-1"))
		if err != nil {
			t.Fatalf("dry_run=%t 时失败：%v", dryRun, err)
		}
		if reply.GetScannedVersions() != 0 || reply.GetDeletedRows() != 0 || reply.GetHasMore() {
			t.Fatalf("窗口内不该有任何可清理内容，回了 %+v", reply)
		}
		if reply.GetDryRun() != dryRun {
			t.Errorf("dry_run 回显=%t，期望 %t（调用方要据此判断这次是否真的删过）", reply.GetDryRun(), dryRun)
		}
		assertOps(t, e.calls(), 0, "PoolVersion.PrunableBefore")
		if e.st.versionRowCount() != 2 || e.st.totalItemRows() != 2 {
			t.Fatalf("水位线判定为 0 却动了数据：版本行=%d 条目=%d", e.st.versionRowCount(), e.st.totalItemRows())
		}
		if e.st.writes != writesBefore {
			t.Errorf("空清理仍计了写入：%d -> %d", writesBefore, e.st.writes)
		}
	}
}

// 正常清理：条目先删、登记行后删，同事务；计数是真实行数之和。
func TestPrunePoolVersionsDeletesItemsBeforeRegistryRowsInTheSameTransaction(t *testing.T) {
	e := newEnv(t)
	// v1 可清理（RETIRED、在窗口外、不是指针版本）；v2 恰在水位线上（保留窗口内）；v3 在线。
	e.st.seedVersion(srcHot32, "global", 3, "b3", model.VersionStateCurrent, 3)
	e.st.seedVersion(srcHot32, "global", 2, "b2", model.VersionStateRetired, 2)
	v1 := e.st.seedVersion(srcHot32, "global", 1, "b1", model.VersionStateRetired, 2)
	e.st.seedItems(srcHot32, "global", 3, item{aid: 31, score: 9}, item{aid: 32, score: 8}, item{aid: 33, score: 7})
	e.st.seedItems(srcHot32, "global", 2, item{aid: 21, score: 9}, item{aid: 22, score: 8})
	e.st.seedItems(srcHot32, "global", 1, item{aid: 11, score: 9}, item{aid: 12, score: 8})
	e.st.seedPointer(srcHot32, "global", 3, "b3", nowUnix())
	writesBefore := e.st.writes

	reply, err := NewPrunePoolVersionsLogic(e.ctx(), e.svcCtx).PrunePoolVersions(pruneReq(2, 0, false, "ops-1"))
	if err != nil {
		t.Fatalf("清理失败：%v", err)
	}
	// 2 条条目 + 1 行登记 = 3，必须逐项相加而不是"整体成功"。
	if reply.GetScannedVersions() != 1 || reply.GetDeletedRows() != 3 || reply.GetDryRun() ||
		reply.GetHasMore() {
		t.Fatalf("聚合值错：%+v（期望 scanned=1 deleted=3 dry_run=false has_more=false）", reply)
	}
	assertOps(t, e.calls(), 0,
		"PoolVersion.PrunableBefore", "PoolVersion.ListPrunable", "Current.FindOne",
		"Transact", "PoolVersion.FindForUpdate", "Pool.DeleteByVersionsInTx",
		"Pool.CountByVersionInTx", "PoolVersion.Delete", "Transact.Commit")
	// 水位线：keep=2 时 Desc[3,2,1] 取第 2 个 = 2，因此 v2 本身必须留在窗口内。
	if got := entryAt(e.calls(), 1); got != "PoolVersion.ListPrunable(1|global|2|100)" {
		t.Fatalf("ListPrunable 入参=%q，期望水位线 2、limit=配置 MaxVersionList 100", got)
	}
	if got := entryAt(e.calls(), 0); got != "PoolVersion.PrunableBefore(1|global|2)" {
		t.Fatalf("PrunableBefore 入参=%q，期望 (source|pool_key|keep)=(1|global|2)", got)
	}
	// 库里剩下的必须正好是"在线 + 窗口内"两批，一行不多一行不少。
	if _, ok := e.st.versionOf(srcHot32, "global", 1); ok {
		t.Errorf("v1 的登记行没删掉")
	}
	if _, ok := e.st.versionOf(srcHot32, "global", 2); !ok {
		t.Errorf("v2 在保留窗口内却被删了")
	}
	if _, ok := e.st.versionOf(srcHot32, "global", 3); !ok {
		t.Errorf("在线版本 v3 被删了")
	}
	if n := e.st.itemCountOfVersion(srcHot32, "global", 1); n != 0 {
		t.Errorf("v1 还剩 %d 条孤儿条目（登记行已删而条目还在，下一轮清理再也找不到它们）", n)
	}
	if n := e.st.itemCountOfVersion(srcHot32, "global", 3); n != 3 {
		t.Errorf("在线版本的条目被动过：剩 %d 条", n)
	}
	if ptr, ok := e.st.pointerOf(srcHot32, "global"); !ok || ptr.Version != 3 {
		t.Errorf("指针被清理动作改写了：%+v", ptr)
	}
	if v1.State != model.VersionStateRetired {
		t.Errorf("种子行被就地改写（state=%s），清理不该复用同一份内存对象", toRPCState(v1.State).String())
	}
	if e.st.writes <= writesBefore {
		t.Fatalf("删除路径一次写都没发生？writes=%d", e.st.writes)
	}
	// 本路径不占幂等表（定位键是 (source,pool_key,version)，删除可重放）。
	if e.calls().has("Idempotency.Claim") {
		t.Errorf("清理走了幂等表：%v", e.calls().ops())
	}
	if len(e.st.outboxOf()) != 0 {
		t.Errorf("清理写了 Outbox（契约里没有这件事）：%+v", e.st.outboxOf())
	}

	// 重放：再点一次同样参数，库里已无 v1，必须诚实回 scanned=0/deleted=0 而不是重复"成功删除"。
	e2calls := e.calls().total()
	reply2, err := NewPrunePoolVersionsLogic(e.ctx(), e.svcCtx).PrunePoolVersions(pruneReq(2, 0, false, "ops-1"))
	if err != nil {
		t.Fatalf("重放清理失败：%v", err)
	}
	if reply2.GetScannedVersions() != 0 || reply2.GetDeletedRows() != 0 {
		t.Fatalf("重放回了 %+v，期望全零（v1 已经不存在）", reply2)
	}
	assertOps(t, e.calls(), e2calls,
		"PoolVersion.PrunableBefore", "PoolVersion.ListPrunable", "Current.FindOne")
}

// dry_run 只登记候选：一个 DELETE 都不发、不改任何行。
func TestPrunePoolVersionsDryRunChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.st.seedVersion(srcHot32, "global", 3, "b3", model.VersionStateCurrent, 2)
	e.st.seedVersion(srcHot32, "global", 2, "b2", model.VersionStateRetired, 1)
	e.st.seedVersion(srcHot32, "global", 1, "b1", model.VersionStateRetired, 2)
	e.st.seedItems(srcHot32, "global", 1, item{aid: 11, score: 9}, item{aid: 12, score: 8})
	e.st.seedItems(srcHot32, "global", 3, item{aid: 31, score: 9}, item{aid: 32, score: 8})
	e.st.seedPointer(srcHot32, "global", 3, "b3", nowUnix())
	writesBefore, rowsBefore, itemsBefore := e.st.writes, e.st.versionRowCount(), e.st.totalItemRows()

	reply, err := NewPrunePoolVersionsLogic(e.ctx(), e.svcCtx).PrunePoolVersions(pruneReq(2, 0, true, "ops-1"))
	if err != nil {
		t.Fatalf("dry_run 失败：%v", err)
	}
	if reply.GetScannedVersions() != 1 || reply.GetDeletedRows() != 0 || !reply.GetDryRun() {
		t.Fatalf("dry_run 聚合值错：%+v", reply)
	}
	// dry_run 连事务都不该开：候选取完就返回。
	assertOps(t, e.calls(), 0, "PoolVersion.PrunableBefore", "PoolVersion.ListPrunable", "Current.FindOne")
	if e.st.writes != writesBefore || e.st.versionRowCount() != rowsBefore ||
		e.st.totalItemRows() != itemsBefore {
		t.Fatalf("dry_run 改了库：writes %d->%d，版本行 %d->%d，条目 %d->%d",
			writesBefore, e.st.writes, rowsBefore, e.st.versionRowCount(), itemsBefore, e.st.totalItemRows())
	}
	if n := e.st.itemCountOfVersion(srcHot32, "global", 1); n != 2 {
		t.Fatalf("dry_run 把候选条目删了，剩 %d 条", n)
	}
}

// 第二道保护：state 镜像说 RETIRED，但指针还指着它 —— 在线正在出数，一行都不许动，
// 而且连事务都不该开（selectPrunable 在进事务前就挡住）。
func TestPrunePoolVersionsProtectsThePointerVersionDespiteRetiredMirror(t *testing.T) {
	e := newEnv(t)
	e.st.seedVersion(srcHot32, "global", 3, "b3", model.VersionStateRetired, 1)
	e.st.seedVersion(srcHot32, "global", 2, "b2", model.VersionStateRetired, 1)
	e.st.seedVersion(srcHot32, "global", 1, "b1", model.VersionStateRetired, 2)
	e.st.seedItems(srcHot32, "global", 1, item{aid: 11, score: 9}, item{aid: 12, score: 8})
	// 指针漂移到 v1（它的 state 镜像被误标成 RETIRED）。
	e.st.seedPointer(srcHot32, "global", 1, "b1", nowUnix())

	reply, err := NewPrunePoolVersionsLogic(e.ctx(), e.svcCtx).PrunePoolVersions(pruneReq(2, 0, false, "ops-1"))
	if err != nil {
		t.Fatalf("失败：%v", err)
	}
	if reply.GetScannedVersions() != 1 || reply.GetDeletedRows() != 0 {
		t.Fatalf("被指针保护的版本仍被统计为已清理：%+v", reply)
	}
	assertOps(t, e.calls(), 0, "PoolVersion.PrunableBefore", "PoolVersion.ListPrunable", "Current.FindOne")
	if e.st.calls.has("Transact") {
		t.Fatalf("指针保护失效：进了事务 %v", e.st.calls.ops())
	}
	if _, ok := e.st.versionOf(srcHot32, "global", 1); !ok {
		t.Errorf("在线出数的 v1 登记行被删了")
	}
	if n := e.st.itemCountOfVersion(srcHot32, "global", 1); n != 2 {
		t.Errorf("在线出数的 v1 条目被删了，剩 %d 条", n)
	}
}

// 第三道保护：候选取完之后、删除之前被并发上线（复核读到 CURRENT）必须整批跳过。
func TestPrunePoolVersionsRecheckCatchesConcurrentPublish(t *testing.T) {
	e := newEnv(t)
	e.st.seedVersion(srcHot32, "global", 3, "b3", model.VersionStateCurrent, 1)
	e.st.seedVersion(srcHot32, "global", 2, "b2", model.VersionStateRetired, 1)
	e.st.seedVersion(srcHot32, "global", 1, "b1", model.VersionStateRetired, 2)
	e.st.seedItems(srcHot32, "global", 1, item{aid: 11, score: 9}, item{aid: 12, score: 8})
	e.st.seedPointer(srcHot32, "global", 3, "b3", nowUnix())

	key := rowKey{source: srcHot32, poolKey: "global", version: 1}
	e.st.on("PoolVersion.FindForUpdate", func(args ...interface{}) {
		e.st.commitExternally(func() { e.st.versions[key].State = model.VersionStateCurrent })
	})

	reply, err := NewPrunePoolVersionsLogic(e.ctx(), e.svcCtx).PrunePoolVersions(pruneReq(2, 0, false, "ops-1"))
	if err != nil {
		t.Fatalf("复核挡下并发上线时不该报错：%v", err)
	}
	if reply.GetDeletedRows() != 0 {
		t.Fatalf("并发上线后仍删了 %d 行", reply.GetDeletedRows())
	}
	assertOps(t, e.calls(), 0,
		"PoolVersion.PrunableBefore", "PoolVersion.ListPrunable", "Current.FindOne",
		"Transact", "PoolVersion.FindForUpdate", "Transact.Commit")
	if e.st.calls.has("Pool.DeleteByVersionsInTx") {
		t.Fatalf("复核通过后还删了条目：%v", e.st.calls.ops())
	}
	// 对手已提交的 state 必须留下（MySQL 里别人的提交不会被我的 ROLLBACK/跳过抹掉）。
	if row, ok := e.st.versionOf(srcHot32, "global", 1); !ok || row.State != model.VersionStateCurrent {
		t.Fatalf("并发上线的提交被本用例改写了：%+v ok=%t", row, ok)
	}
	if n := e.st.itemCountOfVersion(srcHot32, "global", 1); n != 2 {
		t.Fatalf("在线版本的条目被删了，剩 %d 条", n)
	}
}

// 复核时登记行已被别的作业删掉：结论如实记为"行状态变了"，不是错误、也不是"清理成功"。
func TestPrunePoolVersionsToleratesRowAlreadyDeleted(t *testing.T) {
	e := newEnv(t)
	e.st.seedVersion(srcHot32, "global", 3, "b3", model.VersionStateCurrent, 1)
	e.st.seedVersion(srcHot32, "global", 2, "b2", model.VersionStateRetired, 1)
	e.st.seedVersion(srcHot32, "global", 1, "b1", model.VersionStateRetired, 2)
	e.st.seedItems(srcHot32, "global", 1, item{aid: 11, score: 9}, item{aid: 12, score: 8})
	e.st.seedPointer(srcHot32, "global", 3, "b3", nowUnix())
	// keep=2 -> 水位线 Desc[3,2,1] 第 2 个 = 2，候选 {v1}；再注入"行已不见"覆盖 pruneInTx 的第一条分支。
	e.st.failOn("PoolVersion.FindForUpdate", model.ErrVersionNotFound)

	reply, err := NewPrunePoolVersionsLogic(e.ctx(), e.svcCtx).PrunePoolVersions(pruneReq(2, 0, false, "ops-1"))
	if err != nil {
		t.Fatalf("登记行已被删除不该让整次清理失败：%v", err)
	}
	if reply.GetScannedVersions() != 1 || reply.GetDeletedRows() != 0 {
		t.Fatalf("聚合值错：%+v", reply)
	}
	assertOps(t, e.calls(), 0,
		"PoolVersion.PrunableBefore", "PoolVersion.ListPrunable", "Current.FindOne",
		"Transact", "PoolVersion.FindForUpdate", "Transact.Commit")
	if n := e.st.itemCountOfVersion(srcHot32, "global", 1); n != 2 {
		t.Fatalf("复核都未通过却删了条目，剩 %d 条", n)
	}
}

// 命中 max_rows：条目只删了一部分，登记行必须保留（否则剩下的条目变成孤儿），
// 并把 has_more 如实置真让 cron 继续。
func TestPrunePoolVersionsPartialItemDeleteKeepsRegistryRows(t *testing.T) {
	e := newEnv(t)
	e.st.seedVersion(srcHot32, "global", 4, "b4", model.VersionStateCurrent, 1)
	e.st.seedVersion(srcHot32, "global", 2, "b2", model.VersionStateRetired, 3)
	e.st.seedVersion(srcHot32, "global", 1, "b1", model.VersionStateRetired, 3)
	e.st.seedItems(srcHot32, "global", 1, item{aid: 11, score: 9}, item{aid: 12, score: 8}, item{aid: 13, score: 7})
	e.st.seedPointer(srcHot32, "global", 4, "b4", nowUnix())

	// keep=2 -> 水位线 Desc[4,2,1] 第 2 个 = 2，候选 = {v1}；max_rows=1 只删得动 1 行。
	reply, err := NewPrunePoolVersionsLogic(e.ctx(), e.svcCtx).PrunePoolVersions(pruneReq(2, 1, false, "ops-1"))
	if err != nil {
		t.Fatalf("失败：%v", err)
	}
	if reply.GetDeletedRows() != 1 || !reply.GetHasMore() {
		t.Fatalf("命中 max_rows 后聚合值错：%+v（期望 deleted=1 has_more=true）", reply)
	}
	assertOps(t, e.calls(), 0,
		"PoolVersion.PrunableBefore", "PoolVersion.ListPrunable", "Current.FindOne",
		"Transact", "PoolVersion.FindForUpdate", "Pool.DeleteByVersionsInTx", "Transact.Commit")
	if _, ok := e.st.versionOf(srcHot32, "global", 1); !ok {
		t.Fatalf("条目没删完就把登记行删了，剩下 %d 条条目永久失去版本登记",
			e.st.itemCountOfVersion(srcHot32, "global", 1))
	}
	if n := e.st.itemCountOfVersion(srcHot32, "global", 1); n != 2 {
		t.Errorf("v1 剩 %d 条条目，期望 3-1=2", n)
	}
	// 下一批（cron 重放）应继续把剩下的删完。
	reply2, err := NewPrunePoolVersionsLogic(e.ctx(), e.svcCtx).PrunePoolVersions(pruneReq(2, 0, false, "ops-1"))
	if err != nil {
		t.Fatalf("第二批清理失败：%v", err)
	}
	if reply2.GetDeletedRows() != 3 || reply2.GetHasMore() {
		t.Fatalf("第二批聚合值错：%+v（期望 deleted=3=2 条条目+1 行登记，has_more=false）", reply2)
	}
	if _, ok := e.st.versionOf(srcHot32, "global", 1); ok {
		t.Errorf("删干净后登记行仍在")
	}
}

// 复核之后、DELETE 之前被并发上线：删除 SQL 的状态条件（state IN (RETIRED,FAILED)）
// 是最后一道防线。此时条目已清空但登记行还在，结论必须改成 row_state_changed 且 has_more=true，
// 绝不允许继续谎报 "pruned"。
func TestPrunePoolVersionsDeleteSQLStateGuardIsTheLastLine(t *testing.T) {
	e := newEnv(t)
	e.st.seedVersion(srcHot32, "global", 4, "b4", model.VersionStateCurrent, 1)
	e.st.seedVersion(srcHot32, "global", 2, "b2", model.VersionStateRetired, 1)
	e.st.seedVersion(srcHot32, "global", 1, "b1", model.VersionStateRetired, 2)
	e.st.seedItems(srcHot32, "global", 1, item{aid: 11, score: 9}, item{aid: 12, score: 8})
	e.st.seedPointer(srcHot32, "global", 4, "b4", nowUnix())

	key := rowKey{source: srcHot32, poolKey: "global", version: 1}
	e.st.on("PoolVersion.Delete", func(args ...interface{}) {
		e.st.commitExternally(func() { e.st.versions[key].State = model.VersionStateCurrent })
	})

	reply, err := NewPrunePoolVersionsLogic(e.ctx(), e.svcCtx).PrunePoolVersions(pruneReq(2, 0, false, "ops-1"))
	if err != nil {
		t.Fatalf("失败：%v", err)
	}
	// 条目 2 行删了、登记行 0 行 -> deleted_rows=2（不是 3）。
	if reply.GetDeletedRows() != 2 || !reply.GetHasMore() {
		t.Fatalf("聚合值/续批标记错：%+v（期望 deleted=2 has_more=true）", reply)
	}
	row, ok := e.st.versionOf(srcHot32, "global", 1)
	if !ok {
		t.Fatalf("state 已被并发推到 CURRENT，登记行却仍被删掉了（SQL 的状态防线没生效）")
	}
	if row.State != model.VersionStateCurrent {
		t.Errorf("并发上线的提交没留下：state=%s", toRPCState(row.State).String())
	}
	assertOps(t, e.calls(), 0,
		"PoolVersion.PrunableBefore", "PoolVersion.ListPrunable", "Current.FindOne",
		"Transact", "PoolVersion.FindForUpdate", "Pool.DeleteByVersionsInTx",
		"Pool.CountByVersionInTx", "PoolVersion.Delete", "Transact.Commit")
}

// 事务里任一步失败必须整事务回滚：条目、登记行都留在原样，外部已提交的变更除外。
func TestPrunePoolVersionsTransactionFailureRollsBackBothDeletes(t *testing.T) {
	e := newEnv(t)
	e.st.seedVersion(srcHot32, "global", 4, "b4", model.VersionStateCurrent, 1)
	e.st.seedVersion(srcHot32, "global", 2, "b2", model.VersionStateRetired, 1)
	e.st.seedVersion(srcHot32, "global", 1, "b1", model.VersionStateRetired, 2)
	e.st.seedItems(srcHot32, "global", 1, item{aid: 11, score: 9}, item{aid: 12, score: 8})
	e.st.seedPointer(srcHot32, "global", 4, "b4", nowUnix())
	e.st.failOn("PoolVersion.Delete", errStoreDown)

	reply, err := NewPrunePoolVersionsLogic(e.ctx(), e.svcCtx).PrunePoolVersions(pruneReq(2, 0, false, "ops-1"))
	requireErrIs(t, err, errStoreDown)
	if reply != nil {
		t.Fatalf("删除失败却回了应答：%+v", reply)
	}
	assertOps(t, e.calls(), 0,
		"PoolVersion.PrunableBefore", "PoolVersion.ListPrunable", "Current.FindOne",
		"Transact", "PoolVersion.FindForUpdate", "Pool.DeleteByVersionsInTx",
		"Pool.CountByVersionInTx", "PoolVersion.Delete", "Transact.Rollback")
	if n := e.st.itemCountOfVersion(srcHot32, "global", 1); n != 2 {
		t.Errorf("回滚不干净：v1 条目剩 %d 条，期望 2（一行都没删）", n)
	}
	if row, ok := e.st.versionOf(srcHot32, "global", 1); !ok || row.State != model.VersionStateRetired {
		t.Errorf("回滚后登记行形态变了：%+v ok=%t", row, ok)
	}

	// 条目删除本身失败：同样整事务失败，登记行必须还在（不能出现"条目没了、登记行没了"的半态）。
	e2 := newEnv(t)
	e2.st.seedVersion(srcHot32, "global", 4, "b4", model.VersionStateCurrent, 1)
	e2.st.seedVersion(srcHot32, "global", 3, "b3", model.VersionStateRetired, 1)
	e2.st.seedVersion(srcHot32, "global", 1, "b1", model.VersionStateRetired, 2)
	e2.st.seedItems(srcHot32, "global", 1, item{aid: 11, score: 9}, item{aid: 12, score: 8})
	e2.st.seedPointer(srcHot32, "global", 4, "b4", nowUnix())
	e2.st.failOn("Pool.DeleteByVersionsInTx", errStoreDown)
	if _, err := NewPrunePoolVersionsLogic(e2.ctx(), e2.svcCtx).PrunePoolVersions(pruneReq(2, 0, false, "ops-1")); err == nil {
		t.Fatalf("条目删除失败被吞掉了")
	}
	if e2.st.calls.has("PoolVersion.Delete") {
		t.Errorf("条目删除失败后仍去删登记行：%v", e2.st.calls.ops())
	}
	if _, ok := e2.st.versionOf(srcHot32, "global", 1); !ok {
		t.Errorf("条目没删成，登记行却没了")
	}
}

// has_more 的另一条来源：候选取满一页（MaxVersionList）说明窗口外还有版本，cron 必须继续调度。
func TestPrunePoolVersionsHasMoreWhenTheCandidatePageIsFull(t *testing.T) {
	e := newEnvWithRecall(t, func(rc *config.RecallConf) { rc.MaxVersionList = 2 })
	for v, state := range map[int64]int32{1: model.VersionStateRetired, 2: model.VersionStateRetired,
		3: model.VersionStateRetired, 4: model.VersionStateCurrent} {
		e.st.seedVersion(srcHot32, "global", v, "b", state, 0)
	}
	e.st.seedPointer(srcHot32, "global", 4, "b", nowUnix())

	reply, err := NewPrunePoolVersionsLogic(e.ctx(), e.svcCtx).PrunePoolVersions(pruneReq(2, 0, true, "ops-1"))
	if err != nil {
		t.Fatalf("失败：%v", err)
	}
	// keep=2 -> 水位线 Desc[4,3,2,1] 第 2 个 = 3，候选 <3 的是 {1,2}，正好取满 2 条。
	if reply.GetScannedVersions() != 2 || !reply.GetHasMore() {
		t.Fatalf("候选取满一页却没报 has_more：%+v", reply)
	}
	if got := entryAt(e.calls(), 1); got != "PoolVersion.ListPrunable(1|global|3|2)" {
		t.Fatalf("MaxVersionList 没配置化地下推给 ListPrunable：%q（期望水位线 3、limit 2）", got)
	}
	// 上限调大后同一份数据只剩 2 个候选、不再有下一页（证明 has_more 是算出来的不是猜的）。
	e2 := newEnvWithRecall(t, func(rc *config.RecallConf) { rc.MaxVersionList = 100 })
	for v, state := range map[int64]int32{1: model.VersionStateRetired, 2: model.VersionStateRetired,
		3: model.VersionStateRetired, 4: model.VersionStateCurrent} {
		e2.st.seedVersion(srcHot32, "global", v, "b", state, 0)
	}
	e2.st.seedPointer(srcHot32, "global", 4, "b", nowUnix())
	reply2, err := NewPrunePoolVersionsLogic(e2.ctx(), e2.svcCtx).PrunePoolVersions(pruneReq(2, 0, true, "ops-1"))
	if err != nil {
		t.Fatalf("失败：%v", err)
	}
	if reply2.GetHasMore() {
		t.Errorf("MaxVersionList=100、候选只有 2 条时仍报 has_more：%+v", reply2)
	}
	if reply2.GetScannedVersions() != 2 {
		t.Errorf("scanned=%d，期望 2", reply2.GetScannedVersions())
	}
}

// 状态保护（第三道里的 model SQL 条件）：CURRENT/READY 版本连候选都不该出现，
// 因此 logic 根本读不到它们 —— 用两个"只有在线版本可清"的池证明这一点。
func TestPrunePoolVersionsNeverSeesActiveVersionsAsCandidates(t *testing.T) {
	e := newEnv(t)
	e.st.seedVersion(srcHot32, "global", 3, "b3", model.VersionStateCurrent, 5)
	e.st.seedItems(srcHot32, "global", 3, item{aid: 31, score: 9}, item{aid: 32, score: 8})
	e.st.seedPointer(srcHot32, "global", 3, "b3", nowUnix())

	// keep=2 时 Desc[3] 取不到第 2 个 -> 水位线 0：只有一个在线版本可回滚的池没有清理内容。
	reply, err := NewPrunePoolVersionsLogic(e.ctx(), e.svcCtx).PrunePoolVersions(pruneReq(2, 0, false, "ops-1"))
	if err != nil {
		t.Fatalf("失败：%v", err)
	}
	if reply.GetScannedVersions() != 0 || reply.GetDeletedRows() != 0 {
		t.Fatalf("在线版本被当成清理目标：%+v", reply)
	}
	if n := e.st.itemCountOfVersion(srcHot32, "global", 3); n != 2 {
		t.Fatalf("在线池条目被动过，剩 %d 条", n)
	}
	// 只有 READY（封版未上线）也不清：契约里可清理状态恒为 RETIRED/FAILED。
	e.st.seedVersion(srcHot32, "global", 2, "b2", model.VersionStateReady, 1)
	reply2, err := NewPrunePoolVersionsLogic(e.ctx(), e.svcCtx).PrunePoolVersions(pruneReq(2, 0, false, "ops-1"))
	if err != nil {
		t.Fatalf("失败：%v", err)
	}
	if reply2.GetScannedVersions() != 0 {
		t.Errorf("READY 版本被列入了清理候选：%+v", reply2)
	}
	assertOps(t, e.calls(), 1,
		"PoolVersion.PrunableBefore", "PoolVersion.ListPrunable", "Current.FindOne")
}
