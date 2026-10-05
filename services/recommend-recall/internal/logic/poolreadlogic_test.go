package logic

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"go-video/services/recommend-recall/internal/config"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"
)

// ============================================================================
// 运维面读路径：GetPoolSnapshot（某池某版本的条目分页）与 ListPoolVersions（版本与批次）
// ============================================================================
//
// 这两个方法不在在线召回链路上，但它们是"为什么没出数"的唯一排查入口，
// 因此这里钉的是四条口径：
//  1. 门禁拒绝时一次依赖都不碰，且门禁之间有可判别的先后顺序；
//  2. CURRENT 版本只能经 recall_pool_current 指针解析（state=CURRENT 是可重建的冗余镜像，
//     按它找会在镜像漂移时读到"没在出数"的版本）；
//  3. "池没上线"必须是错误，不能是 items 为空的"成功"（后者会把排障引向"发布了空批次"）；
//  4. 分页总数取版本行的登记值，且这两个方法一行都不写（AGENTS.md §5）。

// --- GetPoolSnapshot ---

func TestGetPoolSnapshotGuardsRejectWithoutAnyDependency(t *testing.T) {
	longKey := strings.Repeat("z", model.MaxPoolKeyLen+1)
	cases := []struct {
		name string
		in   *rpc.GetPoolSnapshotReq
		want error
	}{
		{name: "nil 请求", in: nil, want: model.ErrRequestRequired},
		{name: "缺 pool", in: &rpc.GetPoolSnapshotReq{Version: 7}, want: model.ErrInvalidPoolKey},
		{name: "未知召回路 99", in: &rpc.GetPoolSnapshotReq{Pool: testPoolRef(99, "global")}, want: model.ErrInvalidSource},
		{name: "热门池用了关注的键", in: &rpc.GetPoolSnapshotReq{Pool: testPoolRef(model.SourceHot, "mid:1001")},
			want: model.ErrInvalidPoolKey},
		{name: "冷启动池用了不存在的平台", in: &rpc.GetPoolSnapshotReq{Pool: testPoolRef(model.SourceCold, "platform:9")},
			want: model.ErrInvalidPoolKey},
		{name: "分区编号写 0", in: &rpc.GetPoolSnapshotReq{Pool: testPoolRef(model.SourceHot, "zone:0")},
			want: model.ErrInvalidPoolKey},
		{name: "池键超列宽", in: &rpc.GetPoolSnapshotReq{Pool: testPoolRef(model.SourceHot, longKey)},
			want: model.ErrPoolKeyTooLong},
		{name: "页码为负", in: &rpc.GetPoolSnapshotReq{Pool: testPoolRef(model.SourceHot, "global"), Pn: -1},
			want: model.ErrPageTooDeep},
		{name: "页大小超单页上限", in: &rpc.GetPoolSnapshotReq{Pool: testPoolRef(model.SourceHot, "global"), Ps: 201},
			want: model.ErrLimitTooLarge},
		{name: "深翻越过硬上限", in: &rpc.GetPoolSnapshotReq{Pool: testPoolRef(model.SourceHot, "global"), Pn: 502, Ps: 200},
			want: model.ErrPageTooDeep},
		{name: "版本为负", in: &rpc.GetPoolSnapshotReq{Pool: testPoolRef(model.SourceHot, "global"), Version: -3},
			want: model.ErrInvalidVersion},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.st.seedPointer(model.SourceHot, "global", 7, "batch-fake-7", 1700000060)
			e.st.seedVersion(model.SourceHot, "global", 7, "batch-fake-7", model.VersionStateCurrent, 4)
			e.st.seedItems(model.SourceHot, "global", 7, item{101, 0.9}, item{102, 0.8})
			writes := e.st.writes

			lg := NewGetPoolSnapshotLogic(e.ctx(), e.svcCtx)
			reply, err := lg.GetPoolSnapshot(tc.in)
			if reply != nil {
				t.Fatalf("门禁必须拒绝，实际拿到回复 %+v", reply)
			}
			requireErrIs(t, err, tc.want)
			if got := e.calls().total(); got != 0 {
				t.Fatalf("门禁拒绝却碰了 %d 次依赖: %v", got, e.calls().entries)
			}
			if e.st.writes != writes {
				t.Fatalf("门禁拒绝却写了 %d 次", e.st.writes-writes)
			}
		})
	}
}

// TestGetPoolSnapshotGuardOrderIsDiscriminable 钉门禁的先后：
// 池寻址 -> 分页 -> 版本号 -> 指针 -> 版本行 -> 条目。
// 顺序漂了一处，"参数错在哪个字段"的提示就会变成另一个字段的提示。
func TestGetPoolSnapshotGuardOrderIsDiscriminable(t *testing.T) {
	e := newEnv(t)
	e.st.seedPointer(model.SourceHot, "global", 7, "batch-fake-7", 1700000060)
	e.st.seedVersion(model.SourceHot, "global", 7, "batch-fake-7", model.VersionStateCurrent, 4)
	e.st.seedItems(model.SourceHot, "global", 7, item{101, 0.9}, item{102, 0.8})
	lg := NewGetPoolSnapshotLogic(e.ctx(), e.svcCtx)

	// 池与页码同时错：报池。
	requireErrIs(t, snapshotErr(lg, &rpc.GetPoolSnapshotReq{Pool: testPoolRef(99, "global"), Pn: -1}), model.ErrInvalidSource)
	// 页码与版本同时错：报页码（pageArgs 在版本校验之前）。
	requireErrIs(t, snapshotErr(lg, &rpc.GetPoolSnapshotReq{
		Pool: testPoolRef(model.SourceHot, "global"), Pn: -1, Version: -1}), model.ErrPageTooDeep)
	// 单页大小超上限 + 版本为负：报分页。
	requireErrIs(t, snapshotErr(lg, &rpc.GetPoolSnapshotReq{
		Pool: testPoolRef(model.SourceHot, "global"), Ps: 9999, Version: -1}), model.ErrLimitTooLarge)
	// 版本为负：报版本号，且以上任何一次都没触发依赖。
	requireErrIs(t, snapshotErr(lg, &rpc.GetPoolSnapshotReq{
		Pool: testPoolRef(model.SourceHot, "global"), Version: -1}), model.ErrInvalidVersion)
	if got := e.calls().total(); got != 0 {
		t.Fatalf("四段门禁校验共碰了 %d 次依赖，期望 0: %v", got, e.calls().entries)
	}
}

func snapshotErr(lg *GetPoolSnapshotLogic, in *rpc.GetPoolSnapshotReq) error {
	_, err := lg.GetPoolSnapshot(in)
	return err
}

// TestGetPoolSnapshotZeroVersionResolvesThroughPointerNotStateMirror 钉住
// "读当前生效版本"只能经 recall_pool_current：版本表里同时存在两行 state=CURRENT 时，
// 指针指向哪个才算哪个。
func TestGetPoolSnapshotZeroVersionResolvesThroughPointerNotStateMirror(t *testing.T) {
	e := newEnv(t)
	// 指针在 v7；v9 的镜像状态也被写成 CURRENT（镜像可重建，落后于指针是允许的）。
	e.st.seedVersion(model.SourceHot, "global", 9, "batch-fake-9", model.VersionStateCurrent, 1)
	e.st.seedItems(model.SourceHot, "global", 9, item{201, 0.5})
	e.st.seedVersion(model.SourceHot, "global", 7, "batch-fake-7", model.VersionStateCurrent, 2)
	e.st.seedItems(model.SourceHot, "global", 7, item{102, 0.8}, item{101, 0.9})
	e.st.seedPointer(model.SourceHot, "global", 7, "batch-fake-7", 1700000060)
	writes := e.st.writes

	lg := NewGetPoolSnapshotLogic(e.ctx(), e.svcCtx)
	reply, err := lg.GetPoolSnapshot(&rpc.GetPoolSnapshotReq{Pool: testPoolRef(model.SourceHot, "global")})
	if err != nil {
		t.Fatalf("指针指向的 v7 应当可读: %v", err)
	}
	if reply.GetVersion() != 7 || reply.GetBatchId() != "batch-fake-7" {
		t.Fatalf("读到版本 %d/批次 %q，期望指针指向的 7/batch-fake-7", reply.GetVersion(), reply.GetBatchId())
	}
	if reply.GetItemCount() != 2 {
		t.Fatalf("item_count=%d，期望版本行登记的 2", reply.GetItemCount())
	}
	if got := aidListOf(reply.GetItems()); got != "101,102" {
		t.Fatalf("条目顺序 %s，期望按 score DESC, aid ASC 的 101,102（播种顺序是 102 先插）", got)
	}
	assertOps(t, e.calls(), 0, "Current.FindOne", "PoolVersion.FindOne", "Pool.ListByVersion")
	if e.calls().countOf("Pool.TopByVersion") != 0 {
		t.Fatal("运维分页走了在线 TopN 路径，会污染在线读缓存")
	}
	if e.st.writes != writes {
		t.Fatalf("读路径写了 %d 次，期望 0", e.st.writes-writes)
	}
}

// TestGetPoolSnapshotExplicitVersionSkipsPointerRead 钉住"显式版本号直接读那一版"：
// 排障要能读到已退役版本，此时不该被指针拦下，也不该多一次指针读。
func TestGetPoolSnapshotExplicitVersionSkipsPointerRead(t *testing.T) {
	e := newEnv(t)
	e.st.seedVersion(model.SourceHot, "global", 9, "batch-fake-9", model.VersionStateRetired, 1)
	e.st.seedItems(model.SourceHot, "global", 9, item{201, 0.5})
	e.st.seedPointer(model.SourceHot, "global", 7, "batch-fake-7", 1700000060)

	lg := NewGetPoolSnapshotLogic(e.ctx(), e.svcCtx)
	reply, err := lg.GetPoolSnapshot(&rpc.GetPoolSnapshotReq{
		Pool: testPoolRef(model.SourceHot, "global"), Version: 9})
	if err != nil {
		t.Fatalf("显式读退役版本应当成功: %v", err)
	}
	if reply.GetState() != rpc.PoolVersionState_POOL_VERSION_STATE_RETIRED {
		t.Fatalf("state=%v，期望如实回显 RETIRED", reply.GetState())
	}
	assertOps(t, e.calls(), 0, "PoolVersion.FindOne", "Pool.ListByVersion")
}

// TestGetPoolSnapshotNeverPublishedIsErrorNotEmptySuccess 钉住"没上线"与"空池"的区分：
// 三种"读不到当前版本"都必须是错误，绝不能是一条 items 为空、has_more=false 的成功响应。
func TestGetPoolSnapshotNeverPublishedIsErrorNotEmptySuccess(t *testing.T) {
	t.Run("没有指针行", func(t *testing.T) {
		e := newEnv(t)
		lg := NewGetPoolSnapshotLogic(e.ctx(), e.svcCtx)
		reply, err := lg.GetPoolSnapshot(&rpc.GetPoolSnapshotReq{Pool: testPoolRef(model.SourceHot, "global")})
		requireErrIs(t, err, model.ErrPoolNotFound)
		if reply != nil {
			t.Fatalf("池没上线却回了回复: %+v", reply)
		}
		if err.Error() == model.ErrPoolNotFound.Error() {
			t.Fatalf("错误文本要指出是哪个池没上线，实际只有 %q", err.Error())
		}
		assertOps(t, e.calls(), 0, "Current.FindOne")
	})
	t.Run("指针行在但 version=0", func(t *testing.T) {
		e := newEnv(t)
		e.st.seedPointer(model.SourceHot, "global", 0, "", 0)
		lg := NewGetPoolSnapshotLogic(e.ctx(), e.svcCtx)
		reply, err := lg.GetPoolSnapshot(&rpc.GetPoolSnapshotReq{Pool: testPoolRef(model.SourceHot, "global")})
		requireErrIs(t, err, model.ErrPoolNotFound)
		if reply != nil {
			t.Fatalf("version=0 的指针却回了回复: %+v", reply)
		}
		if !strings.Contains(err.Error(), "never published") {
			t.Fatalf("错误文本要说明是\"从未发布\"，实际 %v", err)
		}
		// 只读了指针就停下：不许继续去条目表捞一把再冒充空池。
		assertOps(t, e.calls(), 0, "Current.FindOne")
	})
	t.Run("指针指向的版本行没了", func(t *testing.T) {
		e := newEnv(t)
		e.st.seedPointer(model.SourceHot, "global", 7, "batch-fake-7", 1700000060)
		lg := NewGetPoolSnapshotLogic(e.ctx(), e.svcCtx)
		_, err := lg.GetPoolSnapshot(&rpc.GetPoolSnapshotReq{Pool: testPoolRef(model.SourceHot, "global")})
		requireErrIs(t, err, model.ErrVersionNotFound)
		assertOps(t, e.calls(), 0, "Current.FindOne", "PoolVersion.FindOne")
	})
	t.Run("池上线了但这一版没有条目", func(t *testing.T) {
		e := newEnv(t)
		e.st.seedVersion(model.SourceHot, "global", 7, "batch-fake-7", model.VersionStateCurrent, 0)
		e.st.seedPointer(model.SourceHot, "global", 7, "batch-fake-7", 1700000060)
		lg := NewGetPoolSnapshotLogic(e.ctx(), e.svcCtx)
		reply, err := lg.GetPoolSnapshot(&rpc.GetPoolSnapshotReq{Pool: testPoolRef(model.SourceHot, "global")})
		if err != nil {
			t.Fatalf("已上线的空版本是合法数据状态，不该报错: %v", err)
		}
		if len(reply.GetItems()) != 0 || reply.GetHasMore() {
			t.Fatalf("空版本回复 items=%d has_more=%t，期望 0/false", len(reply.GetItems()), reply.GetHasMore())
		}
	})
}

// TestGetPoolSnapshotHasMoreAndPagesUseRegisteredItemCount 逐页比对条目与 has_more：
// 总数取版本行登记值（已发布版本不可变，登记值即事实），不额外 COUNT(*)。
func TestGetPoolSnapshotHasMoreAndPagesUseRegisteredItemCount(t *testing.T) {
	entries := []item{{103, 0.5}, {101, 0.9}, {102, 0.9}, {104, 0.7}}
	cases := []struct {
		name        string
		registered  int64
		pn, ps      int32
		wantItems   string
		wantHasMore bool
		wantCount   int32
	}{
		{name: "第一页", registered: 6, pn: 1, ps: 2, wantItems: "101,102", wantHasMore: true, wantCount: 6},
		{name: "第二页仍说还有（登记值 6 大于实到 4 条）", registered: 6, pn: 2, ps: 2, wantItems: "104,103", wantHasMore: true, wantCount: 6},
		{name: "登记与实到一致时末页不再翻", registered: 4, pn: 2, ps: 2, wantItems: "104,103", wantHasMore: false, wantCount: 4},
		{name: "登记偏小的版本也会读完", registered: 1, pn: 1, ps: 2, wantItems: "101,102", wantHasMore: false, wantCount: 1},
		{name: "越界页只回空列表", registered: 4, pn: 3, ps: 2, wantItems: "", wantHasMore: false, wantCount: 4},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.st.seedVersion(model.SourceHot, "global", 7, "batch-fake-7", model.VersionStateCurrent, tc.registered)
			e.st.seedItems(model.SourceHot, "global", 7, entries...)
			e.st.seedPointer(model.SourceHot, "global", 7, "batch-fake-7", 1700000060)
			lg := NewGetPoolSnapshotLogic(e.ctx(), e.svcCtx)
			reply, err := lg.GetPoolSnapshot(&rpc.GetPoolSnapshotReq{
				Pool: testPoolRef(model.SourceHot, "global"), Version: 7, Pn: tc.pn, Ps: tc.ps})
			if err != nil {
				t.Fatalf("读取失败: %v", err)
			}
			if got := aidListOf(reply.GetItems()); got != tc.wantItems {
				t.Fatalf("第 %d 页条目 %q，期望 %q", tc.pn, got, tc.wantItems)
			}
			if reply.GetHasMore() != tc.wantHasMore {
				t.Fatalf("has_more=%t，期望 %t（登记值 %d、页 %d/大小 %d、本页 %d 条）",
					reply.GetHasMore(), tc.wantHasMore, tc.registered, tc.pn, tc.ps, len(reply.GetItems()))
			}
			if reply.GetItemCount() != tc.wantCount {
				t.Fatalf("item_count=%d，期望登记值 %d", reply.GetItemCount(), tc.wantCount)
			}
			if reply.GetPool().GetPoolKey() != "global" || int32(reply.GetPool().GetSource()) != model.SourceHot {
				t.Fatalf("回复必须回显读的是哪个池，实际 %v", reply.GetPool())
			}
			assertOps(t, e.calls(), 0, "PoolVersion.FindOne", "Pool.ListByVersion")
		})
	}
}

// TestGetPoolSnapshotPublishedAtComesFromVersionRowNotPointer 把"时间取镜像"钉成一条：
// published_at 取 recall_pool_version 行，而不是 recall_pool_current 指针。
// 镜像没回填时这里就是 0（proto 注释的"未上线为 0"被用在了已上线的池上），
// 属运维面显示口径缺口，已登记进 README；用例按现状钉住，不改生产代码。
func TestGetPoolSnapshotPublishedAtComesFromVersionRowNotPointer(t *testing.T) {
	e := newEnv(t)
	row := e.st.seedVersion(model.SourceHot, "global", 7, "batch-fake-7", model.VersionStateCurrent, 1)
	row.PublishedAt = 1700000050
	e.st.seedItems(model.SourceHot, "global", 7, item{101, 0.9})
	e.st.seedPointer(model.SourceHot, "global", 7, "batch-fake-7", 1700000060)
	lg := NewGetPoolSnapshotLogic(e.ctx(), e.svcCtx)
	reply, err := lg.GetPoolSnapshot(&rpc.GetPoolSnapshotReq{Pool: testPoolRef(model.SourceHot, "global")})
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if reply.GetPublishedAt() != 1700000050 {
		t.Fatalf("published_at=%d，期望版本行镜像 1700000050（指针的 1700000060 不回显）", reply.GetPublishedAt())
	}
}

func TestGetPoolSnapshotDependencyFailuresPropagateRaw(t *testing.T) {
	cases := []struct {
		name     string
		inject   func(e *env)
		wantText string
		wantOps  []string
	}{
		{name: "指针读失败", inject: func(e *env) { e.st.errPointerFindOne = errors.New("pool closed") },
			wantText: "pool closed", wantOps: []string{"Current.FindOne"}},
		{name: "版本行读失败", inject: func(e *env) { e.st.errVersionFindOne = errors.New("deadlock") },
			wantText: "deadlock", wantOps: []string{"Current.FindOne", "PoolVersion.FindOne"}},
		{name: "条目读失败", inject: func(e *env) { e.st.errListByVersion = errors.New("query killed") },
			wantText: "query killed", wantOps: []string{"Current.FindOne", "PoolVersion.FindOne", "Pool.ListByVersion"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.st.seedPointer(model.SourceHot, "global", 7, "batch-fake-7", 1700000060)
			e.st.seedVersion(model.SourceHot, "global", 7, "batch-fake-7", model.VersionStateCurrent, 1)
			e.st.seedItems(model.SourceHot, "global", 7, item{101, 0.9})
			tc.inject(e)
			lg := NewGetPoolSnapshotLogic(e.ctx(), e.svcCtx)
			reply, err := lg.GetPoolSnapshot(&rpc.GetPoolSnapshotReq{Pool: testPoolRef(model.SourceHot, "global")})
			if reply != nil {
				t.Fatalf("依赖故障必须失败，实际回复 %+v", reply)
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("原始错误文本要透传，实际 %v", err)
			}
			// 运维面读路径不做降级伪装：故障不能变成空列表。
			if errors.Is(err, model.ErrPoolNotFound) || errors.Is(err, model.ErrVersionNotFound) {
				t.Fatalf("存储故障被翻译成了\"池/版本不存在\": %v", err)
			}
			assertOps(t, e.calls(), 0, tc.wantOps...)
		})
	}
}

// --- ListPoolVersions ---

func TestListPoolVersionsGuardsRejectWithoutAnyDependency(t *testing.T) {
	cases := []struct {
		name      string
		in        *rpc.ListPoolVersionsReq
		want      error
		wantRejed bool
	}{
		{name: "nil 请求", in: nil, want: model.ErrRequestRequired, wantRejed: true},
		{name: "缺 pool", in: &rpc.ListPoolVersionsReq{}, want: model.ErrInvalidPoolKey, wantRejed: true},
		{name: "未知召回路", in: &rpc.ListPoolVersionsReq{Pool: testPoolRef(99, "global")},
			want: model.ErrInvalidSource, wantRejed: true},
		{name: "标签池用了协同键", in: &rpc.ListPoolVersionsReq{Pool: testPoolRef(model.SourceTag, "aid:1")},
			want: model.ErrInvalidPoolKey, wantRejed: true},
		{name: "limit 超上限（拒绝而不是静默裁小）", in: &rpc.ListPoolVersionsReq{
			Pool: testPoolRef(model.SourceHot, "global"), Limit: 101}, want: model.ErrLimitTooLarge, wantRejed: true},
		{name: "limit 恰好等于配置上限则放行", in: &rpc.ListPoolVersionsReq{
			Pool: testPoolRef(model.SourceHot, "global"), Limit: 100}, wantRejed: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			writes := e.st.writes
			lg := NewListPoolVersionsLogic(e.ctx(), e.svcCtx)
			_, err := lg.ListPoolVersions(tc.in)
			if !tc.wantRejed {
				if err != nil {
					t.Fatalf("limit=MaxVersionList 是合法边界，不应被拒: %v", err)
				}
				return
			}
			requireErrIs(t, err, tc.want)
			if got := e.calls().total(); got != 0 {
				t.Fatalf("门禁拒绝却碰了 %d 次依赖: %v", got, e.calls().entries)
			}
			if e.st.writes != writes {
				t.Fatalf("门禁拒绝却写了 %d 次", e.st.writes-writes)
			}
		})
	}
}

// TestListPoolVersionsNegativeLimitFallsBackToConfiguredMax 钉住一个容易读反的归一：
// 负 limit 不报错，而是按配置上限取（超上限才拒）。
// 这条差异写进 README：调用方传 -1 拿到的是"最多 100 行"，不是"0 行"。
func TestListPoolVersionsNegativeLimitFallsBackToConfiguredMax(t *testing.T) {
	e := newEnv(t)
	e.st.seedVersion(model.SourceHot, "global", 3, "batch-fake-3", model.VersionStateReady, 1)
	lg := NewListPoolVersionsLogic(e.ctx(), e.svcCtx)
	if _, err := lg.ListPoolVersions(&rpc.ListPoolVersionsReq{
		Pool: testPoolRef(model.SourceHot, "global"), Limit: -1}); err != nil {
		t.Fatalf("负 limit 应按配置上限归一而不是报错: %v", err)
	}
	if got := entryAt(e.calls(), 0); got != "PoolVersion.ListByPool(1|global|false|100)" {
		t.Fatalf("下发条件 %q，期望归一到配置上限 100", got)
	}
}

// TestListPoolVersionsRetiredFilterIsPushedIntoTheRead 钉住"状态过滤在读取侧做"：
// 传下去的 limit 是"取多少行"，不是"筛后剩多少行"。
func TestListPoolVersionsRetiredFilterIsPushedIntoTheRead(t *testing.T) {
	e := newEnv(t)
	e.st.seedVersion(model.SourceHot, "global", 9, "batch-fake-9", model.VersionStateRetired, 1)
	e.st.seedVersion(model.SourceHot, "global", 8, "batch-fake-8", model.VersionStateFailed, 1)
	e.st.seedVersion(model.SourceHot, "global", 7, "batch-fake-7", model.VersionStateCurrent, 1)
	e.st.seedVersion(model.SourceHot, "global", 6, "batch-fake-6", model.VersionStateReady, 1)
	e.st.seedPointer(model.SourceHot, "global", 7, "batch-fake-7", 1700000060)
	lg := NewListPoolVersionsLogic(e.ctx(), e.svcCtx)

	reply, err := lg.ListPoolVersions(&rpc.ListPoolVersionsReq{Pool: testPoolRef(model.SourceHot, "global")})
	if err != nil {
		t.Fatalf("默认查询失败: %v", err)
	}
	if got := entryAt(e.calls(), 0); got != "PoolVersion.ListByPool(1|global|false|100)" {
		t.Fatalf("下发给读取的条件是 %q，期望 \"PoolVersion.ListByPool(1|global|false|100)\"", got)
	}
	if got := versionListOf(reply.GetVersions()); got != "8,7,6" {
		t.Fatalf("不含退役版本时的列表 %s，期望按 version DESC 的 8,7,6", got)
	}
	// 逐行原样透传镜像状态：这里刻意不"帮忙"把 READY 改写成 CURRENT，
	// 也不把 FAILED 抹成 UNSPECIFIED —— 读侧一旦粉饰，运维就查不到是哪一批坏了。
	if states := stateListOf(reply.GetVersions()); states !=
		"POOL_VERSION_STATE_FAILED,POOL_VERSION_STATE_CURRENT,POOL_VERSION_STATE_READY" {
		t.Fatalf("状态回显 %s，期望逐行原样透传", states)
	}

	all, err := lg.ListPoolVersions(&rpc.ListPoolVersionsReq{
		Pool: testPoolRef(model.SourceHot, "global"), IncludeRetired: true})
	if err != nil {
		t.Fatalf("含退役查询失败: %v", err)
	}
	if got := versionListOf(all.GetVersions()); got != "9,8,7,6" {
		t.Fatalf("含退役版本列表 %s，期望 9,8,7,6", got)
	}
	if states := stateListOf(all.GetVersions()); !strings.HasPrefix(states, "POOL_VERSION_STATE_RETIRED,") {
		t.Fatalf("include_retired=true 时首行状态 %q，期望仍原样回显 RETIRED", states)
	}
	if got := entryAt(e.calls(), 2); got != "PoolVersion.ListByPool(1|global|true|100)" {
		t.Fatalf("下发条件 %q，期望 include_retired=true 原样下发", got)
	}
	// 追溯字段：一行版本必须带 batch_id 与 generator，否则"候选能追溯到哪个批次"这条链就断了。
	first := all.GetVersions()[0]
	if first.GetBatchId() != "batch-fake-9" || first.GetGenerator() != "fake-job" {
		t.Fatalf("版本行追溯字段缺失: batch=%q generator=%q", first.GetBatchId(), first.GetGenerator())
	}
	assertOps(t, e.calls(), 0,
		"PoolVersion.ListByPool", "Current.FindOne", "PoolVersion.ListByPool", "Current.FindOne")
}

// TestListPoolVersionsCurrentVersionIsPointerNotNewestRow 钉 current_version 的语义：
// 它是"在线此刻出哪一批"，不是"最新登记的那个版本"。
func TestListPoolVersionsCurrentVersionIsPointerNotNewestRow(t *testing.T) {
	e := newEnv(t)
	e.st.seedVersion(model.SourceHot, "global", 9, "batch-fake-9", model.VersionStateReady, 1)
	e.st.seedVersion(model.SourceHot, "global", 7, "batch-fake-7", model.VersionStateCurrent, 1)
	e.st.seedPointer(model.SourceHot, "global", 7, "batch-fake-7", 1700000060)
	lg := NewListPoolVersionsLogic(e.ctx(), e.svcCtx)
	reply, err := lg.ListPoolVersions(&rpc.ListPoolVersionsReq{Pool: testPoolRef(model.SourceHot, "global")})
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if reply.GetCurrentVersion() != 7 {
		t.Fatalf("current_version=%d，期望指针指向的 7（不是最新版本行 9）", reply.GetCurrentVersion())
	}
	if versionListOf(reply.GetVersions()) != "9,7" {
		t.Fatalf("版本列表 %s，期望 9,7", versionListOf(reply.GetVersions()))
	}
}

// TestListPoolVersionsToleratesMissingPointerButNotNegativeVersion 钉两侧口径：
// "该池没上线"在这里是 current_version=0 的正常答案（报 ErrPoolNotFound 会把排障信息一起挡掉），
// 但指针版本是负数属于数据被改坏，必须失败而不是回 0。
func TestListPoolVersionsToleratesMissingPointerButNotNegativeVersion(t *testing.T) {
	t.Run("没有指针行", func(t *testing.T) {
		e := newEnv(t)
		e.st.seedVersion(model.SourceHot, "global", 3, "batch-fake-3", model.VersionStateFailed, 1)
		lg := NewListPoolVersionsLogic(e.ctx(), e.svcCtx)
		reply, err := lg.ListPoolVersions(&rpc.ListPoolVersionsReq{Pool: testPoolRef(model.SourceHot, "global")})
		if err != nil {
			t.Fatalf("池没上线不该报错（列版本正是为了排这个），实际 %v", err)
		}
		if reply.GetCurrentVersion() != 0 {
			t.Fatalf("current_version=%d，期望 0 表示该池在线不出数", reply.GetCurrentVersion())
		}
		if versionListOf(reply.GetVersions()) != "3" {
			t.Fatalf("版本列表 %s，期望仍给出登记行", versionListOf(reply.GetVersions()))
		}
		assertOps(t, e.calls(), 0, "PoolVersion.ListByPool", "Current.FindOne")
	})
	t.Run("指针版本为负", func(t *testing.T) {
		e := newEnv(t)
		e.st.seedVersion(model.SourceHot, "global", 3, "batch-fake-3", model.VersionStateFailed, 1)
		e.st.seedPointer(model.SourceHot, "global", -2, "batch-fake-3", 0)
		lg := NewListPoolVersionsLogic(e.ctx(), e.svcCtx)
		reply, err := lg.ListPoolVersions(&rpc.ListPoolVersionsReq{Pool: testPoolRef(model.SourceHot, "global")})
		if err == nil || reply != nil {
			t.Fatalf("负指针版本必须失败，实际 reply=%+v err=%v", reply, err)
		}
		if !strings.Contains(err.Error(), "negative pointer version") {
			t.Fatalf("错误要说明是数据被改坏，实际 %v", err)
		}
	})
}

// TestListPoolVersionsPointerStorageFailureIsNotSwallowedAsZero 钉住吞错的边界：
// 只有 ErrPoolNotFound 被翻译为 0，其它错误必须原样上抛。
func TestListPoolVersionsPointerStorageFailureIsNotSwallowedAsZero(t *testing.T) {
	e := newEnv(t)
	e.st.seedVersion(model.SourceHot, "global", 3, "batch-fake-3", model.VersionStateFailed, 1)
	e.st.errPointerFindOne = errors.New("too many connections")
	lg := NewListPoolVersionsLogic(e.ctx(), e.svcCtx)
	reply, err := lg.ListPoolVersions(&rpc.ListPoolVersionsReq{Pool: testPoolRef(model.SourceHot, "global")})
	if reply != nil {
		t.Fatalf("存储故障却回了回复: %+v", reply)
	}
	if err == nil || !strings.Contains(err.Error(), "too many connections") {
		t.Fatalf("故障文本必须透传而不是被当成\"没上线\"，实际 %v", err)
	}
	if errors.Is(err, model.ErrPoolNotFound) {
		t.Fatal("存储故障被错误地翻译成了池不存在")
	}
}

// TestListPoolVersionsLimitGateFollowsConfiguredMax 钉住拒绝值取自配置而不是写死：
// 把 MaxVersionList 放大到 model 硬上限 500 后，limit=500 合法、501 被拒。
func TestListPoolVersionsLimitGateFollowsConfiguredMax(t *testing.T) {
	e := newEnvWithRecall(t, func(rc *config.RecallConf) { rc.MaxVersionList = model.MaxVersionListLimit })
	e.st.seedVersion(model.SourceHot, "global", 3, "batch-fake-3", model.VersionStateReady, 1)
	lg := NewListPoolVersionsLogic(e.ctx(), e.svcCtx)
	if _, err := lg.ListPoolVersions(&rpc.ListPoolVersionsReq{
		Pool: testPoolRef(model.SourceHot, "global"), Limit: model.MaxVersionListLimit}); err != nil {
		t.Fatalf("limit=500 在 MaxVersionList=500 下应合法: %v", err)
	}
	if got := entryAt(e.calls(), 0); !strings.HasSuffix(got, "|500)") {
		t.Fatalf("下发 limit 没跟着配置走: %q", got)
	}
	_, err := lg.ListPoolVersions(&rpc.ListPoolVersionsReq{
		Pool: testPoolRef(model.SourceHot, "global"), Limit: model.MaxVersionListLimit + 1})
	requireErrIs(t, err, model.ErrLimitTooLarge)
}

// --- 小工具 ---

func aidListOf(in []*rpc.PoolItem) string {
	parts := make([]string, 0, len(in))
	for _, it := range in {
		parts = append(parts, strconv.FormatInt(it.GetAid(), 10))
	}
	return strings.Join(parts, ",")
}

func versionListOf(in []*rpc.PoolVersionInfo) string {
	parts := make([]string, 0, len(in))
	for _, v := range in {
		parts = append(parts, strconv.FormatInt(v.GetVersion(), 10))
	}
	return strings.Join(parts, ",")
}

func stateListOf(in []*rpc.PoolVersionInfo) string {
	parts := make([]string, 0, len(in))
	for _, v := range in {
		parts = append(parts, v.GetState().String())
	}
	return strings.Join(parts, ",")
}
