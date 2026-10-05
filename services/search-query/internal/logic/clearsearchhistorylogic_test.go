package logic

import (
	"context"
	"errors"
	"testing"

	"go-video/services/search-query/model"
	"go-video/services/search-query/rpc"
)

// clearReq 给一条「所有守卫都能过」的基准清空请求（各用例只改自己要的那一项）。
func clearReq() *rpc.ClearSearchHistoryReq {
	return &rpc.ClearSearchHistoryReq{Mid: 88, Confirm: true, RequestId: "req-clear-1", TraceId: "trace-clear-1"}
}

func doClearHistory(t *testing.T, st *store, in *rpc.ClearSearchHistoryReq) (*rpc.ClearSearchHistoryReply, error) {
	t.Helper()
	return NewClearSearchHistoryLogic(context.Background(), st.svcCtx()).ClearSearchHistory(in)
}

// TestClearSearchHistoryRejectsInvalidRequestsBeforeAnyDependency 守卫表：
// 清空是不可逆的物理删除，因此 confirm/mid 任一不合格都必须在**打库之前**被拒——
// 调用轨迹为空即证明连一次 Redis/引擎/SQL 都没发生（隐私接口不允许「先查一眼再拒」）。
func TestClearSearchHistoryRejectsInvalidRequestsBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name  string
		req   *rpc.ClearSearchHistoryReq
		want  error
		cause string
	}{
		{"空请求", nil, model.ErrInvalidMid, "in==nil 时无法确定归属，直接按非法 mid 拒"},
		{"游客 mid=0", bad(clearReq(), func(r *rpc.ClearSearchHistoryReq) { r.Mid = 0 }), model.ErrInvalidMid,
			"历史只在登录态存在，清空 0 号会误伤所有游客上报"},
		{"负 mid", bad(clearReq(), func(r *rpc.ClearSearchHistoryReq) { r.Mid = -88 }), model.ErrInvalidMid,
			"负 mid 不是用户，不允许当作删除条件"},
		{"未二次确认", bad(clearReq(), func(r *rpc.ClearSearchHistoryReq) { r.Confirm = false }), model.ErrConfirmRequired,
			"破坏性操作必须显式 confirm=true（AGENTS.md 隐私要求）"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, testConfig())
			// 库里有真实历史：守卫若失效，这些行会被删掉，countBy 断言会额外变红。
			st.history.seedRow(88, "开源软件", model.HistoryStateNormal, "android", nowMinus(300), nowMinus(60))
			st.history.seedRow(88, "番剧", model.HistoryStateNormal, "ios", nowMinus(200), nowMinus(50))

			reply, err := doClearHistory(t, st, tc.req)
			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：被拒的请求不应返回响应体，实际 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, st, 0)
			if n := st.history.countBy(88); n != 2 {
				t.Errorf("%s：守卫拒绝后本人历史行数 = %d, want 2（一条都不许少）", tc.name, n)
			}
		})
	}
}

// TestClearSearchHistoryErasesOnlyTheCallersOwnRows 正常路径逐字段投影 + 作用域不变量：
// 清空只清本人，且不分状态（运营标记/待清理行同样必须被擦掉，否则隐私擦除留有副本）。
func TestClearSearchHistoryErasesOnlyTheCallersOwnRows(t *testing.T) {
	st := newStore(t, testConfig())
	// 有辨识度的布景：本人 3 种状态各一行 + 同名关键词的他人行。
	st.history.seedRow(88, "开源软件", model.HistoryStateNormal, "android", nowMinus(900), nowMinus(100))
	st.history.seedRow(88, "番剧 推荐", model.HistoryStateFlagged, "ios", nowMinus(800), nowMinus(90))
	st.history.seedRow(88, "深夜电台", model.HistoryStateTombstone, "harmony", nowMinus(700), nowMinus(80))
	st.history.seedRow(99, "开源软件", model.HistoryStateNormal, "desktop", nowMinus(600), nowMinus(70))
	st.history.seedRow(99, "直播回放", model.HistoryStateTombstone, "web", nowMinus(500), nowMinus(60))
	before := st.history.countBy(99)

	reply, err := doClearHistory(t, st, clearReq())
	wantNoErr(t, "清空历史", err)
	wantEQ(t, "清空历史", "Deleted（含非正常状态行）", reply.Deleted, int64(3))
	wantEQ(t, "清空历史", "HardDeleted（物理删除，不保留可恢复副本）", reply.HardDeleted, true)

	// 顺序断言：整个用例只发生一次 DELETE，没有读、没有缓存、没有引擎。
	wantOps(t, "清空历史调用序列", st.log.ops, []string{"history.DeleteAll:88"})

	if n := st.history.countBy(88); n != 0 {
		t.Errorf("清空历史：本人剩余行数 = %d, want 0", n)
	}
	for _, kw := range []string{"开源软件", "番剧 推荐", "深夜电台"} {
		if row, ok := st.history.get(88, kw); ok {
			t.Errorf("清空历史：行未被擦除 %v", row)
		}
	}
	// 别人的行一行都不能少（作用域越界是隐私事故）。
	if n := st.history.countBy(99); n != before {
		t.Errorf("清空历史：他人行数 = %d, want %d", n, before)
	}
	for _, kw := range []string{"开源软件", "直播回放"} {
		row, ok := st.history.get(99, kw)
		if !ok {
			t.Fatalf("清空历史：他人的 %s 行被误删", kw)
		}
		wantEQ(t, "他人行完整性", "Mid", row.Mid, int64(99))
	}
}

// TestClearSearchHistoryIsIdempotentAndNeverGated 两条语义：
//   - 幂等：重复清空返回 deleted=0 且仍算成功（客户端不必区分「没得删」和「删失败」）；
//   - 不受 Search.HistoryEnabled 限制：擦除能力必须始终可用（关采集不等于关删除）。
func TestClearSearchHistoryIsIdempotentAndNeverGated(t *testing.T) {
	cfg := testConfig()
	cfg.Search.HistoryEnabled = false
	st := newStore(t, cfg)
	st.history.seedRow(88, "开源软件", model.HistoryStateNormal, "android", nowMinus(900), nowMinus(100))

	first, err := doClearHistory(t, st, clearReq())
	wantNoErr(t, "关闭采集时仍可清空", err)
	wantEQ(t, "关闭采集时仍可清空", "Deleted", first.Deleted, int64(1))
	wantEQ(t, "关闭采集时仍可清空", "HardDeleted", first.HardDeleted, true)

	// 第二次：历史已空，仍成功且 deleted=0（幂等，不是 ErrHistoryNotFound）。
	second, err := doClearHistory(t, st, clearReq())
	wantNoErr(t, "重复清空", err)
	wantEQ(t, "重复清空", "Deleted（无匹配行仍算成功）", second.Deleted, int64(0))
	wantEQ(t, "重复清空", "HardDeleted", second.HardDeleted, true)

	wantOps(t, "两次清空的调用序列", st.log.ops, []string{"history.DeleteAll:88", "history.DeleteAll:88"})
	wantEQ(t, "重复清空不产生新行", "本人行数", st.history.countBy(88), 0)
}

// TestClearSearchHistoryPropagatesDeleteFailure 下游失败如实传出：
// 错误不被吞掉、不返回半截成功，且库里的行不会被“部分擦除”。
func TestClearSearchHistoryPropagatesDeleteFailure(t *testing.T) {
	st := newStore(t, testConfig())
	boom := errors.New("search_history DeleteAll: connection reset by peer")
	st.history.failWith("DeleteAll", boom)
	st.history.seedRow(88, "开源软件", model.HistoryStateNormal, "android", nowMinus(900), nowMinus(100))
	st.history.seedRow(99, "开源软件", model.HistoryStateNormal, "desktop", nowMinus(800), nowMinus(90))

	reply, err := doClearHistory(t, st, clearReq())
	wantErrIs(t, "清空失败", err, boom)
	if reply != nil {
		t.Errorf("清空失败：不得返回伪造的成功（deleted/HardDeleted），实际 %+v", reply)
	}
	wantOps(t, "清空失败的调用序列", st.log.ops, []string{"history.DeleteAll:88"})
	// 删除未发生：本人与他人的行都还在（失败不得被当成「已擦除」上报给客户端）。
	if n := st.history.countBy(88); n != 1 {
		t.Errorf("清空失败：本人剩余行数 = %d, want 1", n)
	}
	if n := st.history.countBy(99); n != 1 {
		t.Errorf("清空失败：他人剩余行数 = %d, want 1", n)
	}
}
