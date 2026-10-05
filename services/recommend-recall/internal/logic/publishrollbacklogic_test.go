// 本文件钉住 PublishPoolVersion / RollbackPoolVersion 的切换语义（两者共用 poolswitch.go 的
// switchArgs.run）：版本推进是否合法、指针切换是否原子、失败后库里留下什么形态、
// 并发下是否 CAS 未命中而不是谎报成功。
//
// 装配口径见 fakes_test.go 头注释：六个 model 走语义复刻替身，TransactCtx 是**可回滚**的内存事务，
// 因此"回滚后指针还停在旧版本""事件行没留下"这类断言测的是生产编排代码而不是替身自洽。
// 覆盖边界：替身没有行锁与隔离级别，真并发无法在此复现，只能在生产代码"读→判断→带条件写"的
// 读与写之间注入一次对手已提交的变更（store.commitExternally），检验 CAS/复核是否真的挡住。
package logic

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"go-video/common/idempotency"
	"go-video/services/recommend-recall/internal/repository"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"
)

const (
	pubOperator = "cron-pool-builder"
	pubReason   = "release candidate"
)

func publishReq(version int64, idemKey string) *rpc.PublishPoolVersionReq {
	return &rpc.PublishPoolVersionReq{
		Pool: testPoolRef(srcHot32, "global"), Version: version,
		Operator: pubOperator, Reason: pubReason, IdempotencyKey: idemKey,
	}
}

func rollbackReq(version int64, idemKey string) *rpc.RollbackPoolVersionReq {
	return &rpc.RollbackPoolVersionReq{
		Pool: testPoolRef(srcHot32, "global"), TargetVersion: version,
		Operator: pubOperator, Reason: pubReason, IdempotencyKey: idemKey,
	}
}

// switchFingerprint 复刻 switchArgs.run 的指纹算法：测试要预置一条"同键同指纹"的幂等记录，
// 只能按同一算法算出来，不能凭空写 64 个字符（否则重放分支根本没被走到，用例是假绿）。
func switchFingerprint(scope string, version int64, rollback bool) string {
	return repository.RequestFingerprint(scope,
		fmt.Sprintf("%d:%s", srcHot32, "global"),
		strconv.FormatInt(version, 10), strconv.FormatBool(rollback))
}

// publishPoolAt 装一个热池：指针在 ptrVersion，版本 5/6 的登记状态由入参给定。
// 上线用例用 (Current, Ready)，回滚用例用 (Ready/Retired/…, Current) —— 版本对正好相反。
// 条目行同时种上：切换路径一条都不许碰它们（已发布版本不可变 + AGENTS.md §5）。
func publishPoolAt(t *testing.T, ptrVersion int64, v5State, v6State int32) *env {
	t.Helper()
	e := newEnv(t)
	e.st.seedVersion(srcHot32, "global", 5, "batch-v5", v5State, 3)
	e.st.seedVersion(srcHot32, "global", 6, "batch-v6", v6State, 3)
	e.st.seedItems(srcHot32, "global", 5, item{11, 9}, item{12, 8}, item{13, 7})
	e.st.seedItems(srcHot32, "global", 6, item{21, 9}, item{22, 8}, item{23, 7})
	e.st.seedPointer(srcHot32, "global", ptrVersion, fmt.Sprintf("batch-v%d", ptrVersion), 1700000500)
	return e
}

// assertPoolItemsUntouched 断言 recall_pool 条目一行未动。
func assertPoolItemsUntouched(t *testing.T, e *env, rowsBefore int) {
	t.Helper()
	if got := e.st.totalItemRows(); got != rowsBefore {
		t.Fatalf("切换路径改动了条目行数：%d -> %d", rowsBefore, got)
	}
	if got := e.st.itemAids(srcHot32, "global", 5); !sameInt64(got, []int64{11, 12, 13}) {
		t.Fatalf("版本 5 条目被改写：%v", got)
	}
	if got := e.st.itemAids(srcHot32, "global", 6); !sameInt64(got, []int64{21, 22, 23}) {
		t.Fatalf("版本 6 条目被改写：%v", got)
	}
}

func sameInt64(a, b []int64) bool {
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

func stateAt(e *env, version int64) model.RecallPoolVersion {
	row, ok := e.st.versionOf(srcHot32, "global", version)
	if !ok {
		return model.RecallPoolVersion{State: -1}
	}
	return row
}

func pointerNow(e *env) model.RecallPoolCurrent {
	ptr, ok := e.st.pointerOf(srcHot32, "global")
	if !ok {
		return model.RecallPoolCurrent{Version: -1}
	}
	return ptr
}

func claimNow(scope, key string, e *env) (model.RecallIdempotency, bool) {
	return e.st.claimOf(scope, key)
}

// 入参门禁必须挡在任何依赖调用之前：切换是写操作，一次"参数就不对"的调用
// 若已经认领了幂等键，就会把该键占进 PENDING 租约，白白让调用方等租约过期才能重试。
func TestPublishPoolVersionGuardsRejectBeforeAnyDependencyCall(t *testing.T) {
	// 用 mutate 而不是复制请求结构：protobuf 消息内含 sync.Mutex，复制会被 vet 拦住。
	cases := []struct {
		name     string
		nilReq   bool
		mutate   func(*rpc.PublishPoolVersionReq)
		cause    error
		contains string
	}{
		{"nil 请求", true, nil, model.ErrRequestRequired, "PublishPoolVersionReq"},
		{"缺池", false, func(r *rpc.PublishPoolVersionReq) { r.Pool = nil },
			model.ErrInvalidPoolKey, "pool is required"},
		{"未定义召回路", false, func(r *rpc.PublishPoolVersionReq) {
			r.Pool = &rpc.PoolRef{Source: rpc.Source_SOURCE_UNSPECIFIED, PoolKey: "global"}
		}, model.ErrInvalidSource, "source=0"},
		{"池键语法不合", false, func(r *rpc.PublishPoolVersionReq) { r.Pool = testPoolRef(srcHot32, "mid:7") },
			model.ErrInvalidPoolKey, ""},
		{"版本 0", false, func(r *rpc.PublishPoolVersionReq) { r.Version = 0 },
			model.ErrInvalidVersion, "version=0"},
		{"版本负数", false, func(r *rpc.PublishPoolVersionReq) { r.Version = -3 },
			model.ErrInvalidVersion, "version=-3"},
		{"操作者空白", false, func(r *rpc.PublishPoolVersionReq) { r.Operator = "   " },
			model.ErrOperatorRequired, "operator required"},
		{"操作者超长", false, func(r *rpc.PublishPoolVersionReq) { r.Operator = strings.Repeat("o", colRefID+1) },
			model.ErrFieldTooLong, "operator"},
		{"原因空白", false, func(r *rpc.PublishPoolVersionReq) { r.Reason = "" },
			model.ErrReasonRequired, "reason required"},
		{"原因超长", false, func(r *rpc.PublishPoolVersionReq) { r.Reason = strings.Repeat("r", colNote+1) },
			model.ErrFieldTooLong, "reason"},
		{"幂等键空白", false, func(r *rpc.PublishPoolVersionReq) { r.IdempotencyKey = "  " },
			model.ErrIdempotencyKeyRequired, "idempotency_key required"},
		{"幂等键超长", false, func(r *rpc.PublishPoolVersionReq) {
			r.IdempotencyKey = strings.Repeat("k", colIdempotencyKey+1)
		}, model.ErrFieldTooLong, "idempotency_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			req := publishReq(6, "pub-guard")
			if tc.mutate != nil {
				tc.mutate(req)
			}
			var in *rpc.PublishPoolVersionReq
			if !tc.nilReq {
				in = req
			}
			writesBefore := e.st.writes
			_, err := NewPublishPoolVersionLogic(e.ctx(), e.svcCtx).PublishPoolVersion(in)
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

// 门禁顺序也要钉：版本非法时报 ErrInvalidVersion 而不是 ErrOperatorRequired，
// 否则调用方按错误码分流会被引到"补操作者"这条修不掉的方向。
func TestPublishPoolVersionGuardOrderIsFixed(t *testing.T) {
	e := newEnv(t)
	all := publishReq(0, "pub-order")
	all.Operator, all.Reason = "", ""
	_, err := NewPublishPoolVersionLogic(e.ctx(), e.svcCtx).PublishPoolVersion(all)
	requireErrIs(t, err, model.ErrInvalidVersion)

	all.Version = 6
	_, err = NewPublishPoolVersionLogic(e.ctx(), e.svcCtx).PublishPoolVersion(all)
	requireErrIs(t, err, model.ErrOperatorRequired)

	all.Operator = pubOperator
	_, err = NewPublishPoolVersionLogic(e.ctx(), e.svcCtx).PublishPoolVersion(all)
	requireErrIs(t, err, model.ErrReasonRequired)
	if got := e.calls().total(); got != 0 {
		t.Fatalf("门禁阶段不应触达依赖：%v", e.calls().ops())
	}
}

// 首次上线：建指针行 + CAS 0->6 + 版本置 CURRENT + 事件登记 + 幂等标记，一次提交。
// 逐项断言调用序列，防止"漏掉 Outbox 只切指针"这类撕裂被当成成功。
func TestPublishPoolVersionFirstGoLiveCommitsAllFourWrites(t *testing.T) {
	e := newEnv(t)
	e.st.seedVersion(srcHot32, "global", 6, "batch-v6", model.VersionStateReady, 3)
	e.st.seedItems(srcHot32, "global", 6, item{21, 9}, item{22, 8}, item{23, 7})
	writesBefore := e.st.writes

	reply, err := NewPublishPoolVersionLogic(e.ctx(), e.svcCtx).PublishPoolVersion(publishReq(6, "pub-first"))
	if err != nil {
		t.Fatalf("首次上线失败：%v", err)
	}
	if !reply.GetSwitched() || reply.GetPreviousVersion() != 0 || reply.GetCurrentVersion() != 6 {
		t.Fatalf("应答版本对=%d->%d switched=%t，期望 0->6 true",
			reply.GetPreviousVersion(), reply.GetCurrentVersion(), reply.GetSwitched())
	}
	if reply.GetDeduplicated() {
		t.Fatal("首次执行不得标 deduplicated")
	}
	if reply.GetEventId() == "" {
		t.Fatal("切换必须回带 event_id（下游据此失效缓存）")
	}

	assertOps(t, e.calls(), 0,
		"Idempotency.Claim", "Transact", "Current.EnsureRow", "Current.FindOneForUpdate",
		"PoolVersion.FindForUpdate", "Current.Switch", "PoolVersion.FindCurrentForUpdate",
		"PoolVersion.SetPublishedAt", "Outbox.Insert", "Idempotency.MarkSucceeded", "Transact.Commit")

	ptr := pointerNow(e)
	if ptr.Version != 6 || ptr.PreviousVersion != 0 || ptr.BatchID != "batch-v6" ||
		ptr.SwitchCount != 1 || ptr.Operator != pubOperator || ptr.Note != pubReason || ptr.PublishedAt <= 0 {
		t.Fatalf("指针形态错：%+v", ptr)
	}
	ver := stateAt(e, 6)
	if ver.State != model.VersionStateCurrent || ver.PublishedAt != ptr.PublishedAt {
		t.Fatalf("版本行镜像错：%+v（指针 published_at=%d）", ver, ptr.PublishedAt)
	}

	rows := e.st.outboxOf()
	if len(rows) != 1 {
		t.Fatalf("事件行数=%d，期望 1", len(rows))
	}
	if rows[0].EventID != reply.GetEventId() || rows[0].EventType != model.EventPoolPublished ||
		rows[0].AggregateType != model.AggregateTypePool ||
		rows[0].AggregateID != "1:global:6" || rows[0].State != model.OutboxStatePending {
		t.Fatalf("事件行错：%+v", rows[0])
	}
	// 三处时间戳同源：否则回放看起来像三次操作。
	if rows[0].OccurredAt != ptr.PublishedAt {
		t.Fatalf("事件 occurred_at=%d 与指针 published_at=%d 不同源", rows[0].OccurredAt, ptr.PublishedAt)
	}
	payload := publishedPayloadOf(t, rows[0].Payload)
	if payload.Version != 6 || payload.PreviousVersion != 0 || payload.BatchID != "batch-v6" ||
		payload.ItemCount != 3 || payload.Rollback || payload.Operator != pubOperator ||
		payload.PublishedAt != ptr.PublishedAt || payload.Source != srcHot32 || payload.PoolKey != "global" ||
		payload.Generator != "fake-job" {
		t.Fatalf("事件 payload 错：%+v", payload)
	}

	claim, ok := claimNow(model.IdempotencyScopePublishPoolVersion, "pub-first", e)
	if !ok {
		t.Fatal("幂等记录没落库")
	}
	if claim.State != string(idempotency.StateSucceeded) || claim.EventID != reply.GetEventId() ||
		claim.LeaseExpireAt != 0 {
		t.Fatalf("幂等记录形态错：%+v", claim)
	}
	if !strings.Contains(claim.ResultPayload, `"current_version":6`) {
		t.Fatalf("回放载荷没带版本对：%s", claim.ResultPayload)
	}
	if e.st.writes == writesBefore {
		t.Fatal("整条写路径一次写入都没有，说明生产代码没被执行")
	}
}

// 正常版本推进：6 上线时把 5 退役，指针记 previous_version，switch_count 累加，条目不动。
func TestPublishPoolVersionAdvancesAndRetiresPreviousCurrent(t *testing.T) {
	e := publishPoolAt(t, 5, model.VersionStateCurrent, model.VersionStateReady)
	itemsBefore := e.st.totalItemRows()

	reply, err := NewPublishPoolVersionLogic(e.ctx(), e.svcCtx).PublishPoolVersion(publishReq(6, "pub-adv"))
	if err != nil {
		t.Fatalf("版本推进失败：%v", err)
	}
	if reply.GetPreviousVersion() != 5 || reply.GetCurrentVersion() != 6 || !reply.GetSwitched() {
		t.Fatalf("应答版本对=%d->%d switched=%t，期望 5->6 true",
			reply.GetPreviousVersion(), reply.GetCurrentVersion(), reply.GetSwitched())
	}
	assertOps(t, e.calls(), 0,
		"Idempotency.Claim", "Transact", "Current.EnsureRow", "Current.FindOneForUpdate",
		"PoolVersion.FindForUpdate", "Current.Switch", "PoolVersion.FindCurrentForUpdate",
		"PoolVersion.UpdateState", "PoolVersion.SetPublishedAt", "Outbox.Insert",
		"Idempotency.MarkSucceeded", "Transact.Commit")

	old := stateAt(e, 5)
	if old.State != model.VersionStateRetired {
		t.Fatalf("旧版本没被退役：%+v", old)
	}
	if cur := stateAt(e, 6); cur.State != model.VersionStateCurrent {
		t.Fatalf("目标版本没置 CURRENT：%+v", cur)
	}
	ptr := pointerNow(e)
	if ptr.Version != 6 || ptr.PreviousVersion != 5 || ptr.SwitchCount != 2 {
		t.Fatalf("指针形态错：%+v", ptr)
	}
	assertPoolItemsUntouched(t, e, itemsBefore)
}

// BUILDING/FAILED/未登记的版本都不可上线，且必须报 ErrVersionNotReady（不是"池不存在"）；
// 库里一点没变，幂等键放回 FAILED 可立即重试。
func TestPublishPoolVersionRejectsUnpublishableVersionsAndLeavesStoreIntact(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seed  bool
		state int32
	}{
		{"BUILDING 未封版", true, model.VersionStateBuilding},
		{"FAILED 生成失败", true, model.VersionStateFailed},
		{"从未登记", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := publishPoolAt(t, 5, model.VersionStateCurrent, tc.state)
			if !tc.seed {
				delete(e.st.versions, rowKey{source: srcHot32, poolKey: "global", version: 6})
			}
			itemsBefore := e.st.totalItemRows()

			_, err := NewPublishPoolVersionLogic(e.ctx(), e.svcCtx).PublishPoolVersion(publishReq(6, "pub-bad"))
			requireErrIs(t, err, model.ErrVersionNotReady)
			for _, want := range []string{"source=1", "pool_key=global", "version=6"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("错误文本缺 %s（运维无法定位是哪个池哪个版本）：%v", want, err)
				}
			}
			ptr := pointerNow(e)
			if ptr.Version != 5 || ptr.SwitchCount != 1 {
				t.Fatalf("被拒的切换仍动了指针：%+v", ptr)
			}
			if v6, ok := e.st.versionOf(srcHot32, "global", 6); tc.seed && ok && v6.State != tc.state {
				t.Fatalf("被拒的版本状态被改写：%+v", v6)
			}
			assertPoolItemsUntouched(t, e, itemsBefore)
			if rows := e.st.outboxOf(); len(rows) != 0 {
				t.Fatalf("未发生的切换留下了事件行：%+v", rows)
			}
			// 事务回滚后幂等键必须回到可重试状态，否则同一把键要等租约过期才能重试。
			claim, ok := claimNow(model.IdempotencyScopePublishPoolVersion, "pub-bad", e)
			if !ok {
				t.Fatal("幂等记录没落库（认领应在事务外自动提交）")
			}
			if claim.State != string(idempotency.StateFailed) || claim.LastError == "" {
				t.Fatalf("失败后幂等键没放回可重试状态：%+v", claim)
			}
			if !e.calls().has("Transact.Rollback") {
				t.Fatalf("没记录回滚，事务边界不可信：%v", e.calls().ops())
			}
		})
	}
}

// 空版本不得上线：条目数为 0 时在线侧分不清"池没内容"与"池没上线"，降级原因会报错。
// 判据是登记行的 item_count（封版时由 COUNT(*) 回填），不是实数条目。
func TestPublishPoolVersionRejectsVersionWithoutItems(t *testing.T) {
	e := publishPoolAt(t, 5, model.VersionStateCurrent, model.VersionStateReady)
	e.st.versions[rowKey{source: srcHot32, poolKey: "global", version: 6}].ItemCount = 0

	_, err := NewPublishPoolVersionLogic(e.ctx(), e.svcCtx).PublishPoolVersion(publishReq(6, "pub-noitems"))
	requireErrIs(t, err, model.ErrVersionNoItems)
	if e.calls().has("Current.Switch") {
		t.Fatalf("空版本竟然推进到 CAS 一步：%v", e.calls().ops())
	}
	if ptr := pointerNow(e); ptr.Version != 5 {
		t.Fatalf("指针被动了：%+v", ptr)
	}
}

// 指针已在目标版本：幂等无操作 —— 不移动指针、不发事件（事件描述"池快照变了"，没变就不该有新事件），
// 只补齐撕裂的 state 镜像。
func TestPublishPoolVersionOnCurrentPointerIsNoOpWithoutEvent(t *testing.T) {
	// 镜像撕裂：指针在 6，但版本行还停在 READY。
	e := publishPoolAt(t, 6, model.VersionStateCurrent, model.VersionStateReady)

	reply, err := NewPublishPoolVersionLogic(e.ctx(), e.svcCtx).PublishPoolVersion(publishReq(6, "pub-noop"))
	if err != nil {
		t.Fatalf("幂等无操作分支报错：%v", err)
	}
	if reply.GetSwitched() || reply.GetPreviousVersion() != 6 || reply.GetCurrentVersion() != 6 {
		t.Fatalf("应答=%+v，期望 switched=false 且版本对 6->6", reply)
	}
	if reply.GetEventId() != "" {
		t.Fatalf("没有真实切换却回了 event_id：%s", reply.GetEventId())
	}
	if rows := e.st.outboxOf(); len(rows) != 0 {
		t.Fatalf("没有真实切换却登记了事件：%+v", rows)
	}
	ptr := pointerNow(e)
	if ptr.SwitchCount != 1 || ptr.Version != 6 {
		t.Fatalf("幂等无操作仍动了指针计数：%+v", ptr)
	}
	if ver := stateAt(e, 6); ver.State != model.VersionStateCurrent {
		t.Fatalf("state 镜像没按指针补齐：%+v", ver)
	}
	assertOps(t, e.calls(), 0,
		"Idempotency.Claim", "Transact", "Current.EnsureRow", "Current.FindOneForUpdate",
		"PoolVersion.FindForUpdate", "PoolVersion.SetPublishedAt", "Idempotency.MarkSucceeded",
		"Transact.Commit")
}

// 并发对手在 CAS 前把指针切走了：必须报 ErrSwitchConflict 而不是"覆盖对方的结果"，
// 且本事务的写入全部回滚、对手已提交的指针保留。
func TestPublishPoolVersionLosesCASMustNotOverwriteNorReportSuccess(t *testing.T) {
	e := publishPoolAt(t, 5, model.VersionStateCurrent, model.VersionStateReady)
	itemsBefore := e.st.totalItemRows()
	e.st.on("Current.Switch", func(args ...interface{}) {
		e.st.commitExternally(func() {
			ptr := e.st.pointers[ptrKey{source: srcHot32, poolKey: "global"}]
			ptr.Version, ptr.BatchID, ptr.PreviousVersion, ptr.SwitchCount = 7, "batch-v7", 5, 2
		})
	})

	_, err := NewPublishPoolVersionLogic(e.ctx(), e.svcCtx).PublishPoolVersion(publishReq(6, "pub-race"))
	requireErrIs(t, err, model.ErrSwitchConflict)
	if e.calls().has("Transact.Commit") {
		t.Fatal("CAS 未命中却提交了事务")
	}

	ptr := pointerNow(e)
	if ptr.Version != 7 || ptr.SwitchCount != 2 || ptr.BatchID != "batch-v7" {
		t.Fatalf("冲突后指针没保留对手已提交的结果（被覆盖或被回滚掉了）：%+v", ptr)
	}
	v5, v6 := stateAt(e, 5), stateAt(e, 6)
	if v5.State != model.VersionStateCurrent || v6.State != model.VersionStateReady || v6.PublishedAt != 0 {
		t.Fatalf("冲突事务回滚不干净：v5=%+v v6=%+v", v5, v6)
	}
	if rows := e.st.outboxOf(); len(rows) != 0 {
		t.Fatalf("未提交的切换留下了事件：%+v", rows)
	}
	assertPoolItemsUntouched(t, e, itemsBefore)
	claim, _ := claimNow(model.IdempotencyScopePublishPoolVersion, "pub-race", e)
	if claim.State != string(idempotency.StateFailed) {
		t.Fatalf("CAS 冲突后幂等键状态=%q，期望 failed（可立即重试）", claim.State)
	}
}

// 事件登记失败必须带走整事务（含已移动的指针）：
// "指针切了但没有事件"正是下游缓存永不失效的那种撕裂。
func TestPublishPoolVersionEventFailureRollsBackPointerSwitch(t *testing.T) {
	e := publishPoolAt(t, 5, model.VersionStateCurrent, model.VersionStateReady)
	e.st.failOn("Outbox.Insert", errStoreDown)

	_, err := NewPublishPoolVersionLogic(e.ctx(), e.svcCtx).PublishPoolVersion(publishReq(6, "pub-outbox"))
	requireErrIs(t, err, errStoreDown)

	ptr := pointerNow(e)
	if ptr.Version != 5 || ptr.SwitchCount != 1 || ptr.PreviousVersion != 0 {
		t.Fatalf("事件失败后指针没回滚：%+v", ptr)
	}
	v5, v6 := stateAt(e, 5), stateAt(e, 6)
	if v5.State != model.VersionStateCurrent || v6.State != model.VersionStateReady {
		t.Fatalf("版本状态没随事务回滚：v5=%+v v6=%+v", v5, v6)
	}
	if rows := e.st.outboxOf(); len(rows) != 0 {
		t.Fatalf("失败的事件行留下了：%+v", rows)
	}
	// 幂等标记与业务写同事务：回滚后它不能是 SUCCEEDED，否则重试会回放到一次没发生的切换。
	claim, _ := claimNow(model.IdempotencyScopePublishPoolVersion, "pub-outbox", e)
	if claim.State == string(idempotency.StateSucceeded) {
		t.Fatalf("业务写回滚了但幂等标记成功：%+v", claim)
	}
	if !e.calls().has("Transact.Rollback") {
		t.Fatalf("未记录回滚：%v", e.calls().ops())
	}
}

// 幂等重放：同键同指纹且已 SUCCEEDED —— 回放原结果、不产生第二次切换、不产生第二个事件。
func TestPublishPoolVersionReplaysStoredResultWithoutSecondSwitch(t *testing.T) {
	e := publishPoolAt(t, 5, model.VersionStateCurrent, model.VersionStateReady)
	stored := `{"switched":true,"previous_version":5,"current_version":6,"event_id":"evt-old-1"}`
	e.st.seedClaim(model.IdempotencyScopePublishPoolVersion, "pub-rep",
		switchFingerprint(model.IdempotencyScopePublishPoolVersion, 6, false),
		string(idempotency.StateSucceeded), stored, 0, 1800000000)

	reply, err := NewPublishPoolVersionLogic(e.ctx(), e.svcCtx).PublishPoolVersion(publishReq(6, "pub-rep"))
	if err != nil {
		t.Fatalf("重放失败：%v", err)
	}
	if !reply.GetDeduplicated() || !reply.GetSwitched() ||
		reply.GetPreviousVersion() != 5 || reply.GetCurrentVersion() != 6 || reply.GetEventId() != "evt-old-1" {
		t.Fatalf("重放没回原结果：%+v", reply)
	}
	assertOps(t, e.calls(), 0, "Idempotency.Claim")
	ptr := pointerNow(e)
	if ptr.Version != 5 || ptr.SwitchCount != 1 {
		t.Fatalf("重放造成了第二次切换：%+v", ptr)
	}
	if rows := e.st.outboxOf(); len(rows) != 0 {
		t.Fatalf("重放产生了第二个事件：%+v", rows)
	}
}

// 重放载荷为空（上次执行没写完）时必须报错让调用方重发，不能回一个零值响应冒充上次的结果。
func TestPublishPoolVersionReplayWithEmptyPayloadFailsLoudly(t *testing.T) {
	e := publishPoolAt(t, 5, model.VersionStateCurrent, model.VersionStateReady)
	e.st.seedClaim(model.IdempotencyScopePublishPoolVersion, "pub-hold",
		switchFingerprint(model.IdempotencyScopePublishPoolVersion, 6, false),
		string(idempotency.StateSucceeded), "", 0, 1800000000)

	_, err := NewPublishPoolVersionLogic(e.ctx(), e.svcCtx).PublishPoolVersion(publishReq(6, "pub-hold"))
	requireErrIs(t, err, model.ErrIdempotencyExists)
	if !e.calls().has("Idempotency.Claim") {
		t.Fatal("没走认领")
	}
	if e.calls().has("Transact") {
		t.Fatalf("回放失败却起了事务：%v", e.calls().ops())
	}
}

// 同键不同请求体：调用方复用键却改了参数，硬失败而不是执行其中任一份语义。
func TestPublishPoolVersionSameKeyDifferentVersionIsRejected(t *testing.T) {
	e := publishPoolAt(t, 5, model.VersionStateCurrent, model.VersionStateReady)
	e.st.seedClaim(model.IdempotencyScopePublishPoolVersion, "pub-mix",
		switchFingerprint(model.IdempotencyScopePublishPoolVersion, 6, false),
		string(idempotency.StateSucceeded), `{"switched":true}`, 0, 1800000000)

	_, err := NewPublishPoolVersionLogic(e.ctx(), e.svcCtx).PublishPoolVersion(publishReq(9, "pub-mix"))
	requireErrIs(t, err, model.ErrIdempotencyFingerprintMismatch)
	if ptr := pointerNow(e); ptr.Version != 5 {
		t.Fatalf("指纹冲突却动了指针：%+v", ptr)
	}
}

// 上线与回滚各自一个幂等作用域：同一把键不应既能解释成上线又能解释成回滚。
func TestPublishAndRollbackIdempotencyScopesAreIsolated(t *testing.T) {
	e := publishPoolAt(t, 5, model.VersionStateCurrent, model.VersionStateReady)
	e.st.seedClaim(model.IdempotencyScopeRollbackPoolVersion, "shared-key",
		switchFingerprint(model.IdempotencyScopeRollbackPoolVersion, 5, true),
		string(idempotency.StateSucceeded),
		`{"switched":true,"previous_version":6,"current_version":5,"event_id":"evt-rb"}`, 0, 1800000000)

	reply, err := NewPublishPoolVersionLogic(e.ctx(), e.svcCtx).PublishPoolVersion(publishReq(6, "shared-key"))
	if err != nil {
		t.Fatalf("回滚作用域里的同名记录不该挡住上线：%v", err)
	}
	if reply.GetDeduplicated() {
		t.Fatal("跨作用域误判为重放")
	}
	if ptr := pointerNow(e); ptr.Version != 6 {
		t.Fatalf("上线被回滚作用域的键挡住了：%+v", ptr)
	}
}

// 过期租约必须能重新执行（业务写已回滚、没有半成功状态），否则一次故障就锁死这个键。
func TestPublishPoolVersionExpiredLeaseAllowsReExecution(t *testing.T) {
	e := publishPoolAt(t, 5, model.VersionStateCurrent, model.VersionStateReady)
	e.st.seedClaim(model.IdempotencyScopePublishPoolVersion, "pub-expired",
		switchFingerprint(model.IdempotencyScopePublishPoolVersion, 6, false),
		string(idempotency.StatePending), "", 1, 1800000000)

	reply, err := NewPublishPoolVersionLogic(e.ctx(), e.svcCtx).PublishPoolVersion(publishReq(6, "pub-expired"))
	if err != nil {
		t.Fatalf("过期租约应允许重新执行：%v", err)
	}
	if reply.GetDeduplicated() {
		t.Fatal("重新执行不是重放")
	}
	if ptr := pointerNow(e); ptr.Version != 6 {
		t.Fatalf("重新执行没切指针：%+v", ptr)
	}
}

// 回滚主路径：退回 RETIRED 的历史版本，事件带 rollback=true，
// 被替换下去的版本退役而不是删除（否则一次回滚就把前滚可能性永久删掉）。
func TestRollbackPoolVersionSwitchesBackAndMarksRollback(t *testing.T) {
	e := publishPoolAt(t, 6, model.VersionStateRetired, model.VersionStateCurrent)
	itemsBefore := e.st.totalItemRows()

	reply, err := NewRollbackPoolVersionLogic(e.ctx(), e.svcCtx).RollbackPoolVersion(rollbackReq(5, "rb-1"))
	if err != nil {
		t.Fatalf("回滚失败：%v", err)
	}
	if !reply.GetSwitched() || reply.GetPreviousVersion() != 6 || reply.GetCurrentVersion() != 5 {
		t.Fatalf("应答版本对=%d->%d switched=%t，期望 6->5 true",
			reply.GetPreviousVersion(), reply.GetCurrentVersion(), reply.GetSwitched())
	}
	if reply.GetEventId() == "" {
		t.Fatal("回滚同样要回带 event_id")
	}
	assertOps(t, e.calls(), 0,
		"Idempotency.Claim", "Transact", "Current.EnsureRow", "Current.FindOneForUpdate",
		"PoolVersion.FindForUpdate", "Current.Switch", "PoolVersion.FindCurrentForUpdate",
		"PoolVersion.UpdateState", "PoolVersion.SetPublishedAt", "Outbox.Insert",
		"Idempotency.MarkSucceeded", "Transact.Commit")

	ptr := pointerNow(e)
	if ptr.Version != 5 || ptr.PreviousVersion != 6 || ptr.SwitchCount != 2 || ptr.BatchID != "batch-v5" {
		t.Fatalf("回滚后指针形态错：%+v", ptr)
	}
	if v6 := stateAt(e, 6); v6.State != model.VersionStateRetired {
		t.Fatalf("被替换下去的版本没退役：%+v", v6)
	}
	if _, ok := e.st.versionOf(srcHot32, "global", 6); !ok {
		t.Fatal("被替换的版本行被删掉了（前滚可能性丢失）")
	}
	v5 := stateAt(e, 5)
	if v5.State != model.VersionStateCurrent || v5.PublishedAt != ptr.PublishedAt {
		t.Fatalf("回滚目标没置 CURRENT：%+v", v5)
	}
	assertPoolItemsUntouched(t, e, itemsBefore)

	rows := e.st.outboxOf()
	if len(rows) != 1 {
		t.Fatalf("事件行数=%d，期望 1", len(rows))
	}
	payload := publishedPayloadOf(t, rows[0].Payload)
	if !payload.Rollback {
		t.Fatalf("回滚事件必须 rollback=true（下游要区分新批次上线与退回旧批次）：%+v", payload)
	}
	if payload.Version != 5 || payload.PreviousVersion != 6 {
		t.Fatalf("回滚事件描述的不是目标版本：%+v", payload)
	}
	if rows[0].AggregateID != "1:global:5" {
		t.Fatalf("事件聚合根 ID 不是回滚目标：%+v", rows[0])
	}
	if claim, ok := claimNow(model.IdempotencyScopeRollbackPoolVersion, "rb-1", e); !ok ||
		claim.State != string(idempotency.StateSucceeded) {
		t.Fatalf("回滚幂等记录没落到独立作用域：%+v ok=%t", claim, ok)
	}
}

// 回滚的可拒绝面：BUILDING/FAILED/未登记一律 ErrRollbackTargetInvalid（不是 ErrVersionNotReady），
// 错误码本身要携带"这是运营回滚入口"的定位信息。
func TestRollbackPoolVersionRejectsUnswitchableTargetsWithOwnSentinel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seed  bool
		state int32
	}{
		{"BUILDING", true, model.VersionStateBuilding},
		{"FAILED", true, model.VersionStateFailed},
		{"从未登记", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 现状：6 在线，5 是回滚候选（状态由用例给定）。
			e := publishPoolAt(t, 6, tc.state, model.VersionStateCurrent)
			if !tc.seed {
				delete(e.st.versions, rowKey{source: srcHot32, poolKey: "global", version: 5})
			}

			_, err := NewRollbackPoolVersionLogic(e.ctx(), e.svcCtx).RollbackPoolVersion(rollbackReq(5, "rb-bad"))
			requireErrIs(t, err, model.ErrRollbackTargetInvalid)
			if strings.Contains(err.Error(), model.ErrVersionNotReady.Error()) {
				t.Fatalf("回滚入口不得复用上线哨兵 ErrVersionNotReady：%v", err)
			}
			if ptr := pointerNow(e); ptr.Version != 6 || ptr.SwitchCount != 1 {
				t.Fatalf("被拒回滚动了在线指针：%+v", ptr)
			}
			if rows := e.st.outboxOf(); len(rows) != 0 {
				t.Fatalf("被拒回滚留下事件：%+v", rows)
			}
			if v5, ok := e.st.versionOf(srcHot32, "global", 5); tc.seed && ok && v5.State != tc.state {
				t.Fatalf("被拒的目标版本状态被改写：%+v", v5)
			}
		})
	}
}

// 已知缺口哨兵（README「已知缺口」第 1 条）：
// 回滚入口接受一个从未上线过的 READY 版本，且事件仍标 rollback=true。
// 这与 rollbackpoolversionlogic.go:33「目标状态来源是 RETIRED」自相矛盾 ——
// 下游按 rollback 标记做"退回旧批次"处理，实际收到的是根本没出过数的新批次。
// 本用例钉的是**当前真实行为**而不是它应当如此；生产代码修好后这条必须同步改。
func TestRollbackPoolVersionAcceptsNeverPublishedReadyVersionKnownGap(t *testing.T) {
	// 版本 5 从未上线（published_at=0）但状态是 READY，版本 6 在线。
	e := publishPoolAt(t, 6, model.VersionStateReady, model.VersionStateCurrent)

	reply, err := NewRollbackPoolVersionLogic(e.ctx(), e.svcCtx).RollbackPoolVersion(rollbackReq(5, "rb-ready"))
	if err != nil {
		t.Fatalf("当前实现允许回滚到 READY 版本，用例必须跟着红：%v", err)
	}
	if !reply.GetSwitched() || reply.GetCurrentVersion() != 5 {
		t.Fatalf("应答异常：%+v", reply)
	}
	if v5 := stateAt(e, 5); v5.State != model.VersionStateCurrent {
		t.Fatalf("READY 版本被回滚点亮了（见 README 已知缺口）：%+v", v5)
	}
	rows := e.st.outboxOf()
	if len(rows) != 1 {
		t.Fatalf("事件行数=%d", len(rows))
	}
	if payload := publishedPayloadOf(t, rows[0].Payload); !payload.Rollback {
		t.Fatalf("回滚入口的事件仍带 rollback=true（矛盾点本身）：%+v", payload)
	}
}

// 回滚也走 CAS：指针被别人切走后不得覆盖，且整事务回滚。
func TestRollbackPoolVersionLosesCASAndRollsBackStateMirror(t *testing.T) {
	e := publishPoolAt(t, 6, model.VersionStateRetired, model.VersionStateCurrent)
	itemsBefore := e.st.totalItemRows()
	e.st.on("PoolVersion.FindForUpdate", func(args ...interface{}) {
		e.st.commitExternally(func() {
			ptr := e.st.pointers[ptrKey{source: srcHot32, poolKey: "global"}]
			ptr.Version, ptr.PreviousVersion, ptr.SwitchCount = 7, 6, 2
		})
	})

	_, err := NewRollbackPoolVersionLogic(e.ctx(), e.svcCtx).RollbackPoolVersion(rollbackReq(5, "rb-race"))
	requireErrIs(t, err, model.ErrSwitchConflict)
	ptr := pointerNow(e)
	if ptr.Version != 7 || ptr.SwitchCount != 2 {
		t.Fatalf("冲突后必须保留对手已提交的指针：%+v", ptr)
	}
	v5, v6 := stateAt(e, 5), stateAt(e, 6)
	if v5.State != model.VersionStateRetired || v6.State != model.VersionStateCurrent {
		t.Fatalf("回滚失败但状态镜像被改写了：v5=%+v v6=%+v", v5, v6)
	}
	assertPoolItemsUntouched(t, e, itemsBefore)
}

// 退役旧 CURRENT 的条件更新未命中（状态被别人推过）必须整事务失败，
// 而不是留下"两个版本同时 CURRENT"的镜像。
func TestPublishPoolVersionFailedRetireAbortsWholeSwitch(t *testing.T) {
	e := publishPoolAt(t, 5, model.VersionStateCurrent, model.VersionStateReady)
	e.st.on("PoolVersion.UpdateState", func(args ...interface{}) {
		// 对手在本事务读到旧 CURRENT 之后把它推成了 READY：from=CURRENT 的条件更新必然落空。
		e.st.commitExternally(func() {
			e.st.versions[rowKey{source: srcHot32, poolKey: "global", version: 5}].State = model.VersionStateReady
		})
	})

	_, err := NewPublishPoolVersionLogic(e.ctx(), e.svcCtx).PublishPoolVersion(publishReq(6, "pub-retire"))
	requireErrIs(t, err, model.ErrVersionStateConflict)
	if ptr := pointerNow(e); ptr.Version != 5 {
		t.Fatalf("退役失败后指针必须回到旧值：%+v", ptr)
	}
	if v6 := stateAt(e, 6); v6.State != model.VersionStateReady {
		t.Fatalf("目标版本状态被留下半边：%+v", v6)
	}
	if rows := e.st.outboxOf(); len(rows) != 0 {
		t.Fatalf("失败事务留下事件：%+v", rows)
	}
}

// 回滚的入参门禁与上线同构，但哨兵独立：target_version 非法时不触达任何依赖。
func TestRollbackPoolVersionGuardsRejectBeforeAnyDependencyCall(t *testing.T) {
	e := newEnv(t)
	_, err := NewRollbackPoolVersionLogic(e.ctx(), e.svcCtx).RollbackPoolVersion(nil)
	requireErrIs(t, err, model.ErrRequestRequired)

	_, err = NewRollbackPoolVersionLogic(e.ctx(), e.svcCtx).RollbackPoolVersion(rollbackReq(0, "k"))
	requireErrIs(t, err, model.ErrInvalidVersion)
	if !strings.Contains(err.Error(), "target_version=0") {
		t.Fatalf("错误文本应指明 target_version（而不是复用上线的 version 一词）：%v", err)
	}

	noKey := rollbackReq(5, "")
	_, err = NewRollbackPoolVersionLogic(e.ctx(), e.svcCtx).RollbackPoolVersion(noKey)
	requireErrIs(t, err, model.ErrIdempotencyKeyRequired)

	noReason := rollbackReq(5, "k")
	noReason.Reason = "  "
	_, err = NewRollbackPoolVersionLogic(e.ctx(), e.svcCtx).RollbackPoolVersion(noReason)
	requireErrIs(t, err, model.ErrReasonRequired)

	if got := e.calls().total(); got != 0 {
		t.Fatalf("门禁拒绝后仍有依赖调用：%v", e.calls().ops())
	}
}

// --- 本文件专用装配件 ---

// publishedPayloadOf 从事件行的信封 JSON 里取出 recall.pool.published 的 payload。
// 解不开即信封契约破了；event_type 一并校验，避免"事件行在但类型错"被 payload 断言放过。
func publishedPayloadOf(t *testing.T, envelopeJSON string) poolPublishedPayload {
	t.Helper()
	var env struct {
		EventType   string          `json:"event_type"`
		AggregateID string          `json:"aggregate_id"`
		Payload     json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(envelopeJSON), &env); err != nil {
		t.Fatalf("事件信封解不开：%v（原文 %s）", err, envelopeJSON)
	}
	if env.EventType != model.EventPoolPublished {
		t.Fatalf("事件类型错：%s", env.EventType)
	}
	if env.AggregateID == "" {
		t.Fatal("信封缺 aggregate_id")
	}
	var payload poolPublishedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("事件 payload 解不开：%v（原文 %s）", err, env.Payload)
	}
	return payload
}

// errStoreDown 是存储层故障的代表错误（不映射任何业务哨兵，必须原样透传）。
var errStoreDown = errors.New("storage is down")
