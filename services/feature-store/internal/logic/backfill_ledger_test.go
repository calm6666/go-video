// 测试域 6：回填台账（SubmitBackfillJob / GetBackfillJob / ListBackfillJobs）。
//
// 回填是「给一个还没对外的版本补历史值」，三条读不到的语义必须由测试钉住：
//  1. 提交侧的闸门分两段：入参形态（键/版本/幂等键/操作人/理由/维度/来源）在取得执行权之前，
//     DRAFT·维度·来源·窗口·主体列表这四道在取得执行权之后 —— 于是它们失败后回执里
//     会留下一个 failed 轮次，这个残留形态是可观测契约的一部分；
//  2. 同一 request_id 的重复提交不会插第二条作业，回放读的是台账现值而不是快照；
//     而「摘要里有哪些字段」决定了换窗口/换主体列表算不算同一件事；
//  3. 读侧（Get/List）不补 0、不推算进度，缺行就是 found=false，
//     并且作业台账里的显式主体列表与租约列都不对外投影。
//
// 替身对齐：fakeBackfillJobs.Insert 与真 SQL 一样在入参对象上补 state/ctime/mtime，
// 因此「响应与台账一致」这条断言钉的是 SQL 侧写，不是替身产物。
package logic

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"
)

// --- 造入参与读回副作用 ---

// seedDraft 造一条 DRAFT 版本：回填的目标版本必须是 DRAFT（proto:345）。
// 来源默认给「离线模型产出」——回填是它唯一的导入路径，本文件的请求枚举与之一致。
func seedDraft(f *fixture, key string, version int32, opts ...defOpt) *model.FeatureDefinition {
	f.t.Helper()
	return f.putDef(newDef(key, version, append([]defOpt{withState(model.FeatureStateDraft),
		withSource(model.SourceOfflineModel)}, opts...)...))
}

func callGetJob(f *fixture, jobID int64, requestID string) (*rpc.GetBackfillJobReply, error) {
	return NewGetBackfillJobLogic(f.ctx, f.ServiceContext).GetBackfillJob(
		&rpc.GetBackfillJobReq{JobId: jobID, RequestId: requestID})
}

func callListJobs(f *fixture, in *rpc.ListBackfillJobsReq) (*rpc.ListBackfillJobsReply, error) {
	return NewListBackfillJobsLogic(f.ctx, f.ServiceContext).ListBackfillJobs(in)
}

func callSubmit(f *fixture, in *rpc.SubmitBackfillJobReq) (*rpc.SubmitBackfillJobReply, error) {
	return NewSubmitBackfillJobLogic(f.ctx, f.ServiceContext).SubmitBackfillJob(in)
}

// seedJobRow 直接落一行台账（绕过 Insert 与幂等键），用于造「首次提交在收尾前中断」
// 这类「有作业行、无回执」的形态，以及分页/过滤需要的历史行。
func seedJobRow(f *fixture, requestID, key string, version, scope, source, state int32,
	ctime int64) *model.BackfillJob {
	f.t.Helper()
	f.backfills.nextID++
	row := model.BackfillJob{
		JobID: f.backfills.nextID, FeatureKey: key, Version: version, EntityScope: scope,
		Source: source, State: state, WindowFrom: ctime - 3600, WindowTo: ctime,
		RequestID: requestID, Operator: "data-eng:backfill", Reason: "补历史",
		Ctime: ctime, Mtime: ctime,
	}
	f.backfills.rows[row.JobID] = row
	out := row
	return &out
}

func ledgerRow(f *fixture, jobID int64) (model.BackfillJob, bool) {
	row, ok := f.backfills.rows[jobID]
	return row, ok
}

// --- SubmitBackfillJob：入参闸门在取得执行权之前 ---

func TestSubmitBackfillJobInputGuardsRunBeforeTheExecutionRight(t *testing.T) {
	base := func() *rpc.SubmitBackfillJobReq {
		return &rpc.SubmitBackfillJobReq{
			FeatureKey: "u_play_finish_30d", Version: 2,
			EntityScope: rpc.EntityScope_ENTITY_SCOPE_DEVICE,
			Source:      rpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL,
			WindowFrom:  testNow - 7*24*3600, WindowTo: testNow,
			RequestId: "req-bf-guard", Operator: "data-eng:backfill", Reason: "补 30 日历史",
		}
	}
	cases := []struct {
		name string
		edit func(*rpc.SubmitBackfillJobReq)
		want error
	}{
		{"键形态非法", func(in *rpc.SubmitBackfillJobReq) { in.FeatureKey = "u play finish" },
			model.ErrFeatureKeyRequired},
		{"缺键", func(in *rpc.SubmitBackfillJobReq) { in.FeatureKey = "  " },
			model.ErrFeatureKeyRequired},
		{"版本为 0", func(in *rpc.SubmitBackfillJobReq) { in.Version = 0 },
			model.ErrFeatureVersionRequired},
		// 顺序判别：键先于版本 —— 两个都坏时报的是键。
		{"键先于版本", func(in *rpc.SubmitBackfillJobReq) { in.FeatureKey = "坏键"; in.Version = -1 },
			model.ErrFeatureKeyRequired},
		{"缺幂等键", func(in *rpc.SubmitBackfillJobReq) { in.RequestId = "  " },
			model.ErrRequestIdRequired},
		{"幂等键超长", func(in *rpc.SubmitBackfillJobReq) {
			in.RequestId = strings.Repeat("r", maxRequestIDLen+1)
		}, model.ErrRequestIdRequired},
		{"缺操作人", func(in *rpc.SubmitBackfillJobReq) { in.Operator = "  " },
			model.ErrOperatorRequired},
		{"缺理由", func(in *rpc.SubmitBackfillJobReq) { in.Reason = "  " },
			model.ErrReasonRequired},
		{"维度未声明", func(in *rpc.SubmitBackfillJobReq) {
			in.EntityScope = rpc.EntityScope_ENTITY_SCOPE_UNSPECIFIED
		}, model.ErrEntityScopeRequired},
		{"来源未声明", func(in *rpc.SubmitBackfillJobReq) {
			in.Source = rpc.FeatureSource_FEATURE_SOURCE_UNSPECIFIED
		}, model.ErrSourceRequired},
		// 顺序判别：理由先于维度/来源枚举校验。
		{"理由先于来源", func(in *rpc.SubmitBackfillJobReq) {
			in.Reason = "  "
			in.Source = rpc.FeatureSource_FEATURE_SOURCE_UNSPECIFIED
		}, model.ErrReasonRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			seedDraft(f, "u_play_finish_30d", 2, withScope(model.EntityScopeDevice),
				withSource(model.SourceOfflineModel))
			in := base()
			tc.edit(in)
			_, err := callSubmit(f, in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v, want %v", err, tc.want)
			}
			// 未取得执行权、没读定义、没碰台账。
			assertTouchedNothing(t, f)
		})
	}
}

// --- SubmitBackfillJob：四道语义闸门都发生在取得执行权之后 ---

func TestSubmitBackfillJobSemanticGatesFailAfterTakingTheExecutionRight(t *testing.T) {
	newReq := func(requestID string, version int32, scope rpc.EntityScope,
		source rpc.FeatureSource) *rpc.SubmitBackfillJobReq {
		return &rpc.SubmitBackfillJobReq{
			FeatureKey: "u_play_finish_30d", Version: version, EntityScope: scope, Source: source,
			WindowFrom: testNow - 7*24*3600, WindowTo: testNow,
			RequestId: requestID, Operator: "data-eng:backfill", Reason: "补 30 日历史",
		}
	}
	f := newFixture(t)
	f.registerActive(newDef("u_play_finish_30d", 1, withScope(model.EntityScopeDevice),
		withSource(model.SourceOfflineModel))) // v1 = ACTIVE，不可作为回填目标
	seedDraft(f, "u_play_finish_30d", 2, withScope(model.EntityScopeDevice),
		withSource(model.SourceOfflineModel))

	cases := []struct {
		name      string
		requestID string
		req       *rpc.SubmitBackfillJobReq
		want      error
		code      string
	}{
		{"目标不是 DRAFT", "req-bf-draft",
			newReq("req-bf-draft", 1, rpc.EntityScope_ENTITY_SCOPE_DEVICE,
				rpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL),
			model.ErrBackfillTargetNotDraft, "BACKFILL_TARGET_NOT_DRAFT"},
		{"维度与定义不符", "req-bf-scope",
			newReq("req-bf-scope", 2, rpc.EntityScope_ENTITY_SCOPE_MID,
				rpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL),
			model.ErrEntityScopeMismatch, "ENTITY_SCOPE_MISMATCH"},
		{"来源与定义不符", "req-bf-source",
			newReq("req-bf-source", 2, rpc.EntityScope_ENTITY_SCOPE_DEVICE,
				rpc.FeatureSource_FEATURE_SOURCE_SPM_METRIC),
			model.ErrBackfillSourceMismatch, "BACKFILL_SOURCE_MISMATCH"},
		{"目标版本不存在", "req-bf-missing",
			newReq("req-bf-missing", 9, rpc.EntityScope_ENTITY_SCOPE_DEVICE,
				rpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL),
			model.ErrFeatureNotFound, "FEATURE_NOT_FOUND"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := callSubmit(f, tc.req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v, want %v", err, tc.want)
			}
			// 残留形态：执行权已经取得（Begin 落过一行），失败后收尾成 failed + 可枚举短码。
			if !called(f.receipts.calls, "receipts.Begin") {
				t.Error("the gate ran before the execution right, so no receipt residue is expected")
			}
			row, ok := f.receiptRow(tc.requestID, model.ReceiptOpBackfill)
			if !ok || row.State != model.ReceiptStateFailed || row.ErrorCode != tc.code {
				t.Errorf("receipt = %+v ok=%v, want failed with code %s", row, ok, tc.code)
			}
			if len(f.backfills.rows) != 0 {
				t.Errorf("ledger rows=%d, want 0: a rejected submit must not queue work",
					len(f.backfills.rows))
			}
		})
	}

	// 被拒的提交用同一个 request_id 换个目标版本重试：version 参与摘要，
	// 于是「改目标」不被认成同一件事，重试直接被幂等冲突挡下（台账仍然空）。
	if _, err := callSubmit(f, newReq("req-bf-draft", 2, rpc.EntityScope_ENTITY_SCOPE_DEVICE,
		rpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL)); !errors.Is(err, model.ErrRequestIdReused) {
		t.Errorf("err=%v, want %v: version is part of the request digest", err,
			model.ErrRequestIdReused)
	}
	// 换一个幂等键才真的能提交成功。
	ok, err := callSubmit(f, newReq("req-bf-ok", 2, rpc.EntityScope_ENTITY_SCOPE_DEVICE,
		rpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL))
	if err != nil || ok.GetReused() || ok.GetJobId() != 1 {
		t.Fatalf("submit after the rejections: job_id=%d reused=%v err=%v, want 1/false/nil",
			ok.GetJobId(), ok.GetReused(), err)
	}
	if n := countCalled(f.receipts.calls, "receipts.MarkFailed"); n != len(cases) {
		t.Errorf("MarkFailed calls=%d, want %d (one per rejected submit)", n, len(cases))
	}
	if row, found := ledgerRow(f, 1); !found || row.RequestID != "req-bf-ok" {
		t.Errorf("ledger row = %+v found=%v, request_id=%q", row, found, row.RequestID)
	}
}

// --- SubmitBackfillJob：窗口边界 ---

func TestSubmitBackfillJobWindowBoundaries(t *testing.T) {
	submit := func(f *fixture, requestID string, from, to int64) (*rpc.SubmitBackfillJobReply, error) {
		return callSubmit(f, &rpc.SubmitBackfillJobReq{
			FeatureKey: "u_play_finish_30d", Version: 2,
			EntityScope: rpc.EntityScope_ENTITY_SCOPE_MID,
			Source:      rpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL,
			WindowFrom:  from, WindowTo: to, EntityIds: []string{"10086"},
			RequestId: requestID, Operator: "data-eng:backfill", Reason: "补历史",
		})
	}
	rejected := []struct {
		name     string
		from, to int64
	}{
		{"缺起点", 0, testNow},
		{"起点为负", -5, testNow},
		{"终点早于起点", testNow, testNow - 1},
		{"跨度超过 92 天", testNow - 93*24*3600, testNow},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			seedDraft(f, "u_play_finish_30d", 2)
			_, err := submit(f, "req-bf-window", tc.from, tc.to)
			if !errors.Is(err, model.ErrBackfillWindowInvalid) {
				t.Fatalf("err=%v, want %v", err, model.ErrBackfillWindowInvalid)
			}
			// 窗口是「组装完作业行之后」由 model.ValidateBackfillJob 判的：
			// 于是这一类失败同样已经取得过执行权，并留下 failed 回执。
			if row, ok := f.receiptRow("req-bf-window", model.ReceiptOpBackfill); !ok ||
				row.State != model.ReceiptStateFailed || row.ErrorCode != "BACKFILL_WINDOW_INVALID" {
				t.Errorf("receipt = %+v ok=%v, want failed BACKFILL_WINDOW_INVALID", row, ok)
			}
			if len(f.backfills.rows) != 0 {
				t.Errorf("ledger rows=%d, want 0", len(f.backfills.rows))
			}
		})
	}

	// 边界值：恰好 92 天允许（判据是 >，不是 >=）。
	fSpan := newFixture(t)
	seedDraft(fSpan, "u_play_finish_30d", 2)
	from := testNow - 92*24*3600
	if _, err := submit(fSpan, "req-bf-span-max", from, testNow); err != nil {
		t.Fatalf("a 92-day window must be accepted: %v", err)
	}
	if row, _ := ledgerRow(fSpan, 1); row.WindowFrom != from || row.WindowTo != testNow {
		t.Errorf("window = %d..%d, want %d..%d passed through untouched",
			row.WindowFrom, row.WindowTo, from, testNow)
	}

	// window_to=0 = 提交时刻，落库的是补好的现值而不是 0。
	fZero := newFixture(t)
	seedDraft(fZero, "u_play_finish_30d", 2)
	fZero.advance(120)
	if _, err := submit(fZero, "req-bf-to-zero", testNow-3600, 0); err != nil {
		t.Fatalf("window_to=0: %v", err)
	}
	if row, _ := ledgerRow(fZero, 1); row.WindowTo != fZero.nowUnix() {
		t.Errorf("window_to=%d, want the submit instant %d", row.WindowTo, fZero.nowUnix())
	}

	// 钉住现状：终点晚于当前时刻的窗口没有被挡（与 PurgeExpired 的同类闸门缺失）。
	fFuture := newFixture(t)
	seedDraft(fFuture, "u_play_finish_30d", 2)
	if _, err := submit(fFuture, "req-bf-future", testNow-3600, testNow+86400); err != nil {
		t.Fatalf("a future window_to is accepted today, so this pin must not fail: %v", err)
	}
	if row, _ := ledgerRow(fFuture, 1); row.WindowTo != testNow+86400 {
		t.Errorf("window_to=%d, want the caller's future instant kept", row.WindowTo)
	}

	// 顺序判别：主体列表形态先于窗口 —— 两者都坏时报的是主体错
	// （FormatEntityIDs 在 buildJob 里，ValidateBackfillJob 在它之后）。
	fOrder := newFixture(t)
	seedDraft(fOrder, "u_play_finish_30d", 2)
	_, err := callSubmit(fOrder, &rpc.SubmitBackfillJobReq{
		FeatureKey: "u_play_finish_30d", Version: 2,
		EntityScope: rpc.EntityScope_ENTITY_SCOPE_MID,
		Source:      rpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL,
		WindowFrom:  0, EntityIds: []string{"不是摘要"},
		RequestId: "req-bf-order", Operator: "data-eng:backfill", Reason: "补历史",
	})
	if !errors.Is(err, model.ErrEntityIDInvalid) {
		t.Fatalf("err=%v, want %v", err, model.ErrEntityIDInvalid)
	}
}

// --- SubmitBackfillJob：主体列表规范化、进度分母与投影 ---

func TestSubmitBackfillJobNormalizesEntityIDsAndDerivesTheProgressDenominator(t *testing.T) {
	submitWith := func(f *fixture, requestID string, ids []string) (*rpc.SubmitBackfillJobReply, error) {
		return callSubmit(f, &rpc.SubmitBackfillJobReq{
			FeatureKey: "u_play_finish_30d", Version: 2,
			EntityScope: rpc.EntityScope_ENTITY_SCOPE_MID,
			Source:      rpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL,
			WindowFrom:  testNow - 3600, WindowTo: testNow, EntityIds: ids,
			RequestId: requestID, Operator: "data-eng:backfill", Reason: "补历史",
		})
	}
	f := newFixture(t)
	seedDraft(f, "u_play_finish_30d", 2)
	// 重复项被去掉、空白被裁、分母按去重后的条数算（不是入参数组长度）。
	reply, err := submitWith(f, "req-bf-ids", []string{"10087", "10086", "10086", "  10088  "})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	row, ok := ledgerRow(f, reply.GetJobId())
	if !ok {
		t.Fatal("the submitted job is not in the ledger")
	}
	if row.EntityIDs != "10087,10086,10088" {
		t.Errorf("entity_ids=%q, want dedup + trim applied but the request order kept", row.EntityIDs)
	}
	// 钉住现状：FormatEntityIDs 的注释写「去重后升序」，实现里没有排序
	// （model/featurebackfilljob.go:208-221 只做 trim + 去重，保留请求顺序）。
	// 断点游标按 entity_id 升序推进，未排序的列表正是注释警告的「续跑漏掉前半段」的输入。
	// 判别方式：请求顺序本身是降序开头的，所以「升序」只可能来自实现里的排序。
	parts := strings.Split(row.EntityIDs, ",")
	if sort.StringsAreSorted(parts) {
		t.Errorf("entity_ids=%q 已是升序：实现现在开始排序了，本用例钉的「不排序」现状与注释失效，须一并更新", row.EntityIDs)
	}
	if len(parts) < 2 || parts[0] <= parts[1] {
		t.Errorf("entity_ids=%q 前两项非「请求顺序」，未钉住保留顺序这一现状：want %q", row.EntityIDs, "10087,10086,10088")
	}
	if reply.GetJob().GetEntitiesTotal() != 3 || row.EntitiesTotal != 3 {
		t.Errorf("entities_total reply=%d row=%d, want 3/3",
			reply.GetJob().GetEntitiesTotal(), row.EntitiesTotal)
	}
	if row.EntitiesTruncated != 0 || row.EntitiesDone != 0 || row.EntitiesFailed != 0 {
		t.Errorf("progress columns = truncated=%d done=%d failed=%d, want 0/0/0",
			row.EntitiesTruncated, row.EntitiesDone, row.EntitiesFailed)
	}

	// 空列表 = 全量扫描：分母保持 0，不猜一个「看起来像真话」的进度。
	f2 := newFixture(t)
	seedDraft(f2, "u_play_finish_30d", 2)
	if _, err := submitWith(f2, "req-bf-fullscan", nil); err != nil {
		t.Fatalf("full scan submit: %v", err)
	}
	if row2, _ := ledgerRow(f2, 1); row2.EntityIDs != "" || row2.EntitiesTotal != 0 {
		t.Errorf("full-scan row: entity_ids=%q total=%d, want \"\" and 0",
			row2.EntityIDs, row2.EntitiesTotal)
	}

	// 超上限：报错而不是静默截断（截断 = 补了一半却对外声称成功）。
	f3 := newFixture(t)
	seedDraft(f3, "u_play_finish_30d", 2)
	many := make([]string, 0, model.MaxBackfillEntityIDs+1)
	for i := 0; i <= model.MaxBackfillEntityIDs; i++ {
		many = append(many, strconv.Itoa(1000+i))
	}
	_, err = submitWith(f3, "req-bf-many", many)
	if !errors.Is(err, model.ErrTooManyEntities) {
		t.Fatalf("err=%v, want %v", err, model.ErrTooManyEntities)
	}
	if row3, found := ledgerRow(f3, 1); found {
		t.Errorf("ledger row %d exists with truncated=%d total=%d, want no row at all",
			row3.JobID, row3.EntitiesTruncated, row3.EntitiesTotal)
	}
	if got := f3.receipts.failed; len(got) != 1 || got[0] != "req-bf-many/backfill:TOO_MANY_ENTITIES" {
		t.Errorf("failed receipts=%v, want the submit marked TOO_MANY_ENTITIES", got)
	}

	// 投影：显式主体列表与租约列都不对外（台账里有，响应里不能有）。
	f4 := newFixture(t)
	seedDraft(f4, "u_play_finish_30d", 2, withScope(model.EntityScopeDevice))
	digest := testDeviceID
	if _, err := callSubmit(f4, &rpc.SubmitBackfillJobReq{
		FeatureKey: "u_play_finish_30d", Version: 2,
		EntityScope: rpc.EntityScope_ENTITY_SCOPE_DEVICE,
		Source:      rpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL,
		WindowFrom:  testNow - 3600, WindowTo: testNow, EntityIds: []string{digest},
		RequestId: "req-bf-leak", Operator: "data-eng:backfill", Reason: "补历史",
	}); err != nil {
		t.Fatalf("device-scope submit: %v", err)
	}
	stored, _ := ledgerRow(f4, 1)
	if stored.EntityIDs != digest {
		t.Fatalf("entity_ids=%q, want the digest kept in the ledger", stored.EntityIDs)
	}
	stored.LeaseOwner = "pod-7#worker-3"
	stored.LeaseExpireAt = testNow + 999
	stored.CursorEntityStr = digest
	f4.backfills.rows[1] = stored
	list, err := callListJobs(f4, &rpc.ListBackfillJobsReq{Pn: 1, Ps: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := list.String(); strings.Contains(got, digest) || strings.Contains(got, "pod-7#worker-3") {
		t.Errorf("reply leaks the entity digest or the lease columns: %s", got)
	}
	if got := list.GetJobs()[0]; got.GetRequestId() != "req-bf-leak" {
		t.Errorf("request_id=%q, want it projected back to the submitter", got.GetRequestId())
	}
}

// --- SubmitBackfillJob：auto_switch 基线 ---

func TestSubmitBackfillJobAutoSwitchBaselineIsComparedWithTheLivePointer(t *testing.T) {
	cases := []struct {
		name        string
		pointerless bool
		autoSwitch  bool
		fromVersion int32
		wantErr     error
		wantStored  int32
		pointerRead bool
	}{
		// 基线与指针现值一致才允许延迟切换。
		{"基线等于当前生效版本", false, true, 1, nil, 1, true},
		{"基线指向回填目标自己", false, true, 2, model.ErrVersionConflict, 0, true},
		{"基线为 0 但已有生效版本", false, true, 0, model.ErrVersionConflict, 0, true},
		// from_version=0 的语义是「从无到有」，只在没有 ACTIVE 版本时通过。
		{"首版从无到有", true, true, 0, nil, 0, true},
		// 未开自动切换：基线不参与判定，落 0 以免后来者误读成「已校验过」。
		{"未开自动切换时基线清零", false, false, 7, nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			key := "u_play_finish_30d"
			if tc.pointerless {
				seedDraft(f, key, 1, withSource(model.SourceOfflineModel))
			} else {
				f.registerActive(newDef(key, 1, withSource(model.SourceOfflineModel)))
				seedDraft(f, key, 2, withSource(model.SourceOfflineModel))
			}
			version := int32(2)
			if tc.pointerless {
				version = 1
			}
			_, err := callSubmit(f, &rpc.SubmitBackfillJobReq{
				FeatureKey: key, Version: version,
				EntityScope: rpc.EntityScope_ENTITY_SCOPE_MID,
				Source:      rpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL,
				WindowFrom:  testNow - 3600, WindowTo: testNow, EntityIds: []string{"10086"},
				AutoSwitch: tc.autoSwitch, FromVersion: tc.fromVersion,
				RequestId: "req-bf-auto", Operator: "data-eng:backfill", Reason: "补历史",
			})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err=%v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if len(f.backfills.rows) != 0 {
					t.Error("a rejected auto_switch baseline still queued a job")
				}
				if got := f.receipts.failed; len(got) != 1 ||
					got[0] != "req-bf-auto/backfill:VERSION_CONFLICT" {
					t.Errorf("failed receipts=%v, want VERSION_CONFLICT", got)
				}
			} else if row, ok := ledgerRow(f, 1); !ok || row.FromVersion != tc.wantStored ||
				row.AutoSwitch != tc.autoSwitch {
				t.Errorf("row auto_switch=%v from_version=%d ok=%v, want %v/%d/true",
					row.AutoSwitch, row.FromVersion, ok, tc.autoSwitch, tc.wantStored)
			}
			if got := called(f.pointers.calls, "activeVersions.FindOne"); got != tc.pointerRead {
				t.Errorf("baseline read happened=%v, want %v", got, tc.pointerRead)
			}
		})
	}
}

// --- SubmitBackfillJob：重复提交与幂等摘要的覆盖面 ---

func TestSubmitBackfillJobDuplicateSubmitReusesTheLedgerRowAndReportsLiveProgress(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("u_play_finish_30d", 1))
	seedDraft(f, "u_play_finish_30d", 2)
	req := &rpc.SubmitBackfillJobReq{
		FeatureKey: "u_play_finish_30d", Version: 2,
		EntityScope: rpc.EntityScope_ENTITY_SCOPE_MID,
		Source:      rpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL,
		WindowFrom:  testNow - 7*24*3600, WindowTo: testNow, EntityIds: []string{"10086", "10087"},
		RequestId: "req-bf-dup", Operator: "data-eng:backfill", Reason: "补历史",
	}

	first, err := callSubmit(f, req)
	if err != nil || first.GetReused() || first.GetJobId() != 1 {
		t.Fatalf("first submit: job_id=%d reused=%v err=%v", first.GetJobId(), first.GetReused(), err)
	}
	// 模拟 worker 推进过这条作业：回放必须给出「现在到哪了」，不是首次的快照。
	row, _ := ledgerRow(f, 1)
	row.State = model.BackfillStateRunning
	row.EntitiesDone = 1
	row.CursorEntityID = 10086
	f.backfills.rows[1] = row

	second, err := callSubmit(f, req)
	if err != nil {
		t.Fatalf("duplicate submit: %v", err)
	}
	if !second.GetReused() || second.GetJobId() != 1 {
		t.Errorf("duplicate: job_id=%d reused=%v, want 1/true", second.GetJobId(), second.GetReused())
	}
	if got := second.GetJob(); got.GetState() != rpc.BackfillState_BACKFILL_STATE_RUNNING ||
		got.GetEntitiesDone() != 1 || got.GetCursorEntityId() != 10086 {
		t.Errorf("duplicate reply job = %+v, want the live ledger progress", got)
	}
	if n := countCalled(f.backfills.calls, "backfills.Insert"); n != 1 {
		t.Errorf("Insert calls=%d, want 1: 重复提交不得插第二条作业", n)
	}
	if n := len(f.backfills.rows); n != 1 {
		t.Errorf("ledger rows=%d, want 1", n)
	}
	if n := countCalled(f.receipts.calls, "receipts.Begin"); n != 2 {
		t.Errorf("Begin calls=%d, want 2 (the second one is the one that found the DONE receipt)", n)
	}
	if n := countCalled(f.receipts.calls, "receipts.MarkDone"); n != 1 {
		t.Errorf("MarkDone calls=%d, want 1: 回放不重复收尾", n)
	}

	// 同一 request_id 换窗口/换主体列表：这四个字段都不在摘要输入里
	// （model/featurewritereceipt.go:200-214 只取 op/key/version/scope/entity_id/row_count/operator），
	// 于是第二次被判定为「同一件事」并回放，新窗口从未被读过一行（README §10 已登记）。
	third, err := callSubmit(f, &rpc.SubmitBackfillJobReq{
		FeatureKey: "u_play_finish_30d", Version: 2,
		EntityScope: rpc.EntityScope_ENTITY_SCOPE_MID,
		Source:      rpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL,
		WindowFrom:  testNow - 86400, WindowTo: testNow,
		EntityIds: []string{"20001", "20002", "20003"},
		RequestId: "req-bf-dup", Operator: "data-eng:backfill", Reason: "换一批主体再补一次",
	})
	if err != nil {
		t.Fatalf("submit with a changed window: %v", err)
	}
	if !third.GetReused() || third.GetJobId() != 1 {
		t.Errorf("changed-window submit: job_id=%d reused=%v, want the first job replayed",
			third.GetJobId(), third.GetReused())
	}
	if got := third.GetJob().GetWindowFrom(); got != testNow-7*24*3600 {
		t.Errorf("window_from=%d, want the first job's window kept", got)
	}
	if n := countCalled(f.backfills.calls, "backfills.Insert"); n != 1 {
		t.Errorf("Insert calls=%d, want still 1", n)
	}
	if n := countCalled(f.backfills.calls, "backfills.FindOne"); n != 2 {
		t.Errorf("FindOne calls=%d, want 2 (两个回放各读一次现值)", n)
	}
	if n := countCalled(f.defs.calls, "definitions.FindOne"); n != 1 {
		t.Errorf("definitions.FindOne calls=%d, want 1: 目标定义在回放时根本没再看", n)
	}

	// 对照：operator 参与摘要 —— 换个提交人说同一件事就冲突，而不是被并成一条。
	if _, err := callSubmit(f, &rpc.SubmitBackfillJobReq{
		FeatureKey: "u_play_finish_30d", Version: 2,
		EntityScope: rpc.EntityScope_ENTITY_SCOPE_MID,
		Source:      rpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL,
		WindowFrom:  testNow - 7*24*3600, WindowTo: testNow, EntityIds: []string{"10086"},
		RequestId: "req-bf-dup", Operator: "data-eng:other", Reason: "补历史",
	}); !errors.Is(err, model.ErrRequestIdReused) {
		t.Errorf("err=%v, want %v", err, model.ErrRequestIdReused)
	}
	if n := len(f.backfills.rows); n != 1 {
		t.Errorf("ledger rows=%d, want 1 after the rejected reuse", n)
	}
}

// --- SubmitBackfillJob：唯一键冲突与插入失败 ---

func TestSubmitBackfillJobWithExistingLedgerRowButNoReceiptReusesIt(t *testing.T) {
	f := newFixture(t)
	seedDraft(f, "u_play_finish_30d", 2)
	seeded := seedJobRow(f, "req-bf-orphan", "u_play_finish_30d", 2, model.EntityScopeMid,
		model.SourceOfflineModel, model.BackfillStateRunning, testNow-60)

	// 「有作业行、无回执」= 首次提交落库后在收尾前中断。第二次提交必须复用那一行。
	reply, err := callSubmit(f, &rpc.SubmitBackfillJobReq{
		FeatureKey: "u_play_finish_30d", Version: 2,
		EntityScope: rpc.EntityScope_ENTITY_SCOPE_MID,
		Source:      rpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL,
		WindowFrom:  seeded.WindowFrom, WindowTo: seeded.WindowTo, EntityIds: []string{"10086"},
		RequestId: "req-bf-orphan", Operator: "data-eng:backfill", Reason: "补历史",
	})
	if err != nil {
		t.Fatalf("submit over the interrupted first attempt: %v", err)
	}
	if !reply.GetReused() || reply.GetJobId() != seeded.JobID {
		t.Errorf("reply job_id=%d reused=%v, want %d/true", reply.GetJobId(), reply.GetReused(),
			seeded.JobID)
	}
	if n := len(f.backfills.rows); n != 1 {
		t.Errorf("ledger rows=%d, want 1: uniq_request_id 命中后不得再插一条", n)
	}
	if row, _ := ledgerRow(f, seeded.JobID); row.State != model.BackfillStateRunning {
		t.Errorf("state=%d, want the in-flight job left as it is (reused must not restart it)",
			row.State)
	}
	// 复用路径同样收尾回执：第三次提交走的是回放而不是再一次撞唯一键。
	rcpt, ok := f.receiptRow("req-bf-orphan", model.ReceiptOpBackfill)
	if !ok || rcpt.State != model.ReceiptStateDone || rcpt.AffectedRows != 1 {
		t.Errorf("receipt = %+v ok=%v, want done with affected_rows=1", rcpt, ok)
	}
	third, err := callSubmit(f, &rpc.SubmitBackfillJobReq{
		FeatureKey: "u_play_finish_30d", Version: 2,
		EntityScope: rpc.EntityScope_ENTITY_SCOPE_MID,
		Source:      rpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL,
		WindowFrom:  seeded.WindowFrom, WindowTo: seeded.WindowTo, EntityIds: []string{"10086"},
		RequestId: "req-bf-orphan", Operator: "data-eng:backfill", Reason: "补历史",
	})
	if err != nil || !third.GetReused() || third.GetJobId() != seeded.JobID {
		t.Errorf("third submit: job_id=%d reused=%v err=%v, want the same job replayed",
			third.GetJobId(), third.GetReused(), err)
	}
	if n := countCalled(f.backfills.calls, "backfills.Insert"); n != 1 {
		t.Errorf("Insert calls=%d, want 1 (only the collision attempt reached INSERT)", n)
	}
}

func TestSubmitBackfillJobReportsInsertFailuresInsteadOfFakeSuccess(t *testing.T) {
	req := func(requestID string) *rpc.SubmitBackfillJobReq {
		return &rpc.SubmitBackfillJobReq{
			FeatureKey: "u_play_finish_30d", Version: 2,
			EntityScope: rpc.EntityScope_ENTITY_SCOPE_MID,
			Source:      rpc.FeatureSource_FEATURE_SOURCE_OFFLINE_MODEL,
			WindowFrom:  testNow - 3600, WindowTo: testNow, EntityIds: []string{"10086"},
			RequestId: requestID, Operator: "data-eng:backfill", Reason: "补历史",
		}
	}

	// 唯一键报了冲突、按 request_id 却回查不到行：这是台账与幂等键不自洽的形态，
	// 不能「当作复用」回一个成功，必须失败并留下 failed 回执。
	f := newFixture(t)
	seedDraft(f, "u_play_finish_30d", 2)
	f.backfills.errInsert = model.ErrJobExists
	_, err := callSubmit(f, req("req-bf-ghost"))
	if !errors.Is(err, model.ErrJobExists) {
		t.Fatalf("err=%v, want %v", err, model.ErrJobExists)
	}
	if !strings.Contains(err.Error(), "receipt-less job row") {
		t.Errorf("err=%v, want the diagnosis to say the ledger row has no receipt", err)
	}
	if len(f.backfills.rows) != 0 {
		t.Error("a collision with no matching row still created a job")
	}
	if row, ok := f.receiptRow("req-bf-ghost", model.ReceiptOpBackfill); !ok ||
		row.State != model.ReceiptStateFailed {
		t.Fatalf("receipt = %+v ok=%v, want state failed", row, ok)
	}

	// 回查本身失败时，报的是回查的错误（不是「复用成功」）。
	f2 := newFixture(t)
	seedDraft(f2, "u_play_finish_30d", 2)
	f2.backfills.errInsert = model.ErrJobExists
	f2.backfills.errFindByRequest = errors.New("dial tcp: connection refused")
	_, err = callSubmit(f2, req("req-bf-findfail"))
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err=%v, want the lookup failure surfaced", err)
	}
	if got := f2.receipts.failed; len(got) != 1 || got[0] != "req-bf-findfail/backfill:INTERNAL" {
		t.Errorf("failed receipts=%v, want the lookup failure marked INTERNAL", got)
	}

	// 普通插入失败（死锁一类）：不留作业行，回执 failed，同键可接管重试。
	f3 := newFixture(t)
	seedDraft(f3, "u_play_finish_30d", 2)
	f3.backfills.errInsert = errors.New("error 1213: deadlock found")
	if _, err := callSubmit(f3, req("req-bf-deadlock")); err == nil ||
		!strings.Contains(err.Error(), "deadlock") {
		t.Fatalf("err=%v, want the insert failure surfaced", err)
	}
	if len(f3.backfills.rows) != 0 {
		t.Errorf("ledger rows=%d, want 0", len(f3.backfills.rows))
	}
	f3.backfills.errInsert = nil
	retry, err := callSubmit(f3, req("req-bf-deadlock"))
	if err != nil || retry.GetReused() || retry.GetJobId() != 1 {
		t.Fatalf("retry after the failed insert: job_id=%d reused=%v err=%v, want 1/false/nil",
			retry.GetJobId(), retry.GetReused(), err)
	}
}

// --- GetBackfillJob ---

func TestGetBackfillJobTakesEitherKeyAndReportsMissingRowsAsNotFound(t *testing.T) {
	f := newFixture(t)
	mine := seedJobRow(f, "req-bf-get", "u_play_finish_30d", 2, model.EntityScopeMid,
		model.SourceOfflineModel, model.BackfillStateSucceeded, testNow-60)
	theirs := seedJobRow(f, "req-bf-other", "u_play_finish_30d", 2, model.EntityScopeMid,
		model.SourceOfflineModel, model.BackfillStateFailed, testNow-30)
	theirs.LastError = "source unavailable"
	f.backfills.rows[theirs.JobID] = *theirs

	// 按 job_id 命中：进度与终态按台账现值给出。
	byID, err := callGetJob(f, mine.JobID, "")
	if err != nil || !byID.GetFound() {
		t.Fatalf("by job_id: found=%v err=%v", byID.GetFound(), err)
	}
	if got := byID.GetJob(); got.GetJobId() != mine.JobID ||
		got.GetState() != rpc.BackfillState_BACKFILL_STATE_SUCCEEDED ||
		got.GetCtime() != testNow-60 {
		t.Errorf("job = %+v, want the seeded row projected as it stands", got)
	}

	// 按 request_id 命中：提交方超时没拿到 job_id 时也能查到自己那一次。
	f.backfills.calls = nil
	byKey, err := callGetJob(f, 0, "req-bf-other")
	if err != nil || !byKey.GetFound() {
		t.Fatalf("by request_id: found=%v err=%v", byKey.GetFound(), err)
	}
	if got := byKey.GetJob(); got.GetJobId() != theirs.JobID ||
		got.GetState() != rpc.BackfillState_BACKFILL_STATE_FAILED ||
		!strings.Contains(got.GetLastError(), "source unavailable") {
		t.Errorf("job = %+v, want the failed row with its last_error", got)
	}
	if called(f.backfills.calls, "backfills.FindOne") {
		t.Errorf("request_id 路径按主键查了 %+v，绕过了 uniq_request_id", f.backfills.calls)
	}

	// 两个键都给：job_id 优先，且不再读 request_id。
	f.backfills.calls = nil
	both, err := callGetJob(f, mine.JobID, "req-bf-other")
	if err != nil || both.GetJob().GetJobId() != mine.JobID {
		t.Fatalf("both keys: job_id=%d err=%v, want the job_id winner",
			both.GetJob().GetJobId(), err)
	}
	if called(f.backfills.calls, "backfills.FindByRequestID") {
		t.Errorf("job_id 胜出后仍去按幂等键查： %+v", f.backfills.calls)
	}

	// 缺行 = found=false 且不是错误：运维视图里「不存在」不是故障。
	for _, tc := range []struct {
		name  string
		jobID int64
		reqID string
		calls string
	}{
		{"job_id 不存在", 4242, "", "backfills.FindOne"},
		{"request_id 不存在", 0, "req-bf-never", "backfills.FindByRequestID"},
	} {
		reply, err := callGetJob(f, tc.jobID, tc.reqID)
		if err != nil {
			t.Errorf("%s: err=%v, want found=false with no error", tc.name, err)
			continue
		}
		if reply.GetFound() || reply.GetJob() != nil {
			t.Errorf("%s: found=%v job=%v, want false/nil", tc.name, reply.GetFound(), reply.GetJob())
		}
	}

	// 两个键都不给：报错，且一次读都不发生。
	f.backfills.calls = nil
	if _, err := callGetJob(f, 0, "  "); !errors.Is(err, model.ErrJobNotFound) {
		t.Errorf("err=%v, want %v", err, model.ErrJobNotFound)
	}
	assertTouchedNothing(t, f)
	// 幂等键形态在按 request_id 查之前先校验（超长键不进 SQL）。
	if _, err := callGetJob(f, 0, strings.Repeat("r", maxRequestIDLen+1)); !errors.Is(err,
		model.ErrRequestIdRequired) {
		t.Errorf("err=%v, want %v", err, model.ErrRequestIdRequired)
	}
	assertTouchedNothing(t, f)
}

func TestGetBackfillJobSurfacesModelErrorsAsErrors(t *testing.T) {
	f := newFixture(t)
	seedJobRow(f, "req-bf-err", "u_play_finish_30d", 2, model.EntityScopeMid,
		model.SourceOfflineModel, model.BackfillStatePending, testNow-60)
	f.backfills.errFindOne = errors.New("dial tcp: connection refused")
	if _, err := callGetJob(f, 1, ""); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err=%v, want the read failure surfaced", err)
	}
	f.backfills.errFindOne = nil
	f.backfills.errFindByRequest = errors.New("read timeout")
	if _, err := callGetJob(f, 0, "req-bf-err"); err == nil ||
		!strings.Contains(err.Error(), "read timeout") {
		t.Fatalf("err=%v, want the read failure surfaced, not found=false", err)
	}
}

// --- ListBackfillJobs ---

func TestListBackfillJobsGuardsRunBeforeTheQuery(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ListBackfillJobsReq
		want error
	}{
		{"页码为 0", &rpc.ListBackfillJobsReq{Pn: 0, Ps: 10}, model.ErrLimitTooLarge},
		{"页大小为 0", &rpc.ListBackfillJobsReq{Pn: 1, Ps: 0}, model.ErrLimitTooLarge},
		{"页大小超上限", &rpc.ListBackfillJobsReq{Pn: 1, Ps: model.MaxListPageSize + 1},
			model.ErrLimitTooLarge},
		// 顺序判别：分页参数先于键与状态枚举（三者都坏时报的是分页）。
		{"分页先于键", &rpc.ListBackfillJobsReq{FeatureKey: "坏 键", Pn: 0, Ps: 0},
			model.ErrLimitTooLarge},
		{"键形态非法", &rpc.ListBackfillJobsReq{FeatureKey: "坏 键", Pn: 1, Ps: 10},
			model.ErrFeatureKeyRequired},
		{"状态不在枚举内", &rpc.ListBackfillJobsReq{Pn: 1, Ps: 10, State: 9},
			model.ErrJobStateInvalid},
		{"起点为负", &rpc.ListBackfillJobsReq{Pn: 1, Ps: 10, Since: -1},
			model.ErrBackfillWindowInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			seedJobRow(f, "req-bf-list", "u_play_finish_30d", 2, model.EntityScopeMid,
				model.SourceOfflineModel, model.BackfillStatePending, testNow)
			_, err := callListJobs(f, tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v, want %v", err, tc.want)
			}
			assertTouchedNothing(t, f)
		})
	}
}

func TestListBackfillJobsPassesFiltersDownAndOrdersByJobIDDesc(t *testing.T) {
	f := newFixture(t)
	key := "u_play_finish_30d"
	first := seedJobRow(f, "req-bf-l1", key, 2, model.EntityScopeMid, model.SourceOfflineModel,
		model.BackfillStatePending, testNow-7200)
	second := seedJobRow(f, "req-bf-l2", key, 2, model.EntityScopeMid, model.SourceOfflineModel,
		model.BackfillStateRunning, testNow-3600)
	third := seedJobRow(f, "req-bf-l3", "a_item_heat_7d", 1, model.EntityScopeAid,
		model.SourceOfflineModel, model.BackfillStateSucceeded, testNow-60)
	if first.JobID != 1 || second.JobID != 2 || third.JobID != 3 {
		t.Fatalf("seeded job ids = %d/%d/%d, want 1/2/3", first.JobID, second.JobID, third.JobID)
	}

	reply, err := callListJobs(f, &rpc.ListBackfillJobsReq{
		FeatureKey: key, State: rpc.BackfillState_BACKFILL_STATE_RUNNING,
		Since: testNow - 7200, Pn: 1, Ps: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(reply.GetJobs()) != 1 || reply.GetJobs()[0].GetJobId() != second.JobID {
		t.Fatalf("jobs = %v, want only the RUNNING one", reply.GetJobs())
	}
	if reply.GetTotal() != 1 {
		t.Errorf("total=%d, want 1: total 是过滤后的计数而不是表内行数", reply.GetTotal())
	}
	seen := f.backfills.listSeen
	if len(seen) != 1 {
		t.Fatalf("List calls=%d, want 1", len(seen))
	}
	if seen[0].FeatureKey != key || seen[0].State != model.BackfillStateRunning ||
		seen[0].Since != testNow-7200 || seen[0].Pn != 1 || seen[0].Ps != 10 {
		t.Errorf("filter passed down = %+v, want key/state/since/pn/ps kept verbatim", seen[0])
	}

	// 无过滤：全量、按 job_id 倒序（新作业先看），分页原样下传由 SQL 承担。
	unfiltered, err := callListJobs(f, &rpc.ListBackfillJobsReq{Pn: 1, Ps: 100})
	if err != nil {
		t.Fatalf("unfiltered list: %v", err)
	}
	if unfiltered.GetTotal() != 3 {
		t.Fatalf("total=%d, want 3", unfiltered.GetTotal())
	}
	for i, want := range []int64{3, 2, 1} {
		if got := unfiltered.GetJobs()[i].GetJobId(); got != want {
			t.Errorf("position %d job_id=%d, want %d (job_id DESC)", i, got, want)
		}
	}
	// 第二页：分页参数不是摆设，也没有被 logic 在本地重切。
	page2, err := callListJobs(f, &rpc.ListBackfillJobsReq{Pn: 2, Ps: 2})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2.GetJobs()) != 1 || page2.GetJobs()[0].GetJobId() != 1 || page2.GetTotal() != 3 {
		t.Errorf("page 2 jobs=%v total=%d, want [1] of 3", page2.GetJobs(), page2.GetTotal())
	}
	if f.backfills.listSeen[2].Pn != 2 || f.backfills.listSeen[2].Ps != 2 {
		t.Errorf("page 2 filter = %+v, want pn/ps forwarded", f.backfills.listSeen[2])
	}

	// 越界页：total 照实给，条目为空。
	empty, err := callListJobs(f, &rpc.ListBackfillJobsReq{Pn: 9, Ps: 2})
	if err != nil {
		t.Fatalf("out-of-range page: %v", err)
	}
	if len(empty.GetJobs()) != 0 || empty.GetTotal() != 3 {
		t.Errorf("out-of-range jobs=%v total=%d, want [] with the real total 3",
			empty.GetJobs(), empty.GetTotal())
	}
	// 空表：total=0、jobs 为空而不是报错。
	fNone := newFixture(t)
	none, err := callListJobs(fNone, &rpc.ListBackfillJobsReq{Pn: 1, Ps: 10})
	if err != nil || none.GetTotal() != 0 || len(none.GetJobs()) != 0 {
		t.Errorf("empty ledger: total=%d jobs=%v err=%v, want 0/[]/nil",
			none.GetTotal(), none.GetJobs(), err)
	}
	// 对外分页不碰认领路径：ListClaimable/Claim 是 cron 专用的另一条读。
	for _, name := range []string{"backfills.ListClaimable", "backfills.Claim",
		"backfills.AddProgress", "backfills.Finish", "backfills.Cancel"} {
		if called(f.backfills.calls, name) {
			t.Errorf("unexpected %s during a read-only listing", name)
		}
	}
}
