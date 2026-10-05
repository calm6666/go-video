package logic

import (
	"context"
	"testing"

	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"
)

// TestDelFavRejectsGuardsBeforeTouchingDeps fid 不校验：favorite_item.fid=0 是「默认夹」，
// 取消收藏允许不带夹子（model 的兜底 UPDATE 正是为此存在）。
func TestDelFavRejectsGuardsBeforeTouchingDeps(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.DelFavReq
		want error
	}{
		{"mid 为 0", &rpc.DelFavReq{Mid: 0, Oid: 10001, Fid: 33, Tp: 2}, model.ErrInvalidMid},
		{"mid 为负", &rpc.DelFavReq{Mid: -7, Oid: 10001, Fid: 33, Tp: 2}, model.ErrInvalidMid},
		{"oid 为 0", &rpc.DelFavReq{Mid: 7, Oid: 0, Fid: 33, Tp: 2}, model.ErrInvalidOid},
		{"oid 为负", &rpc.DelFavReq{Mid: 7, Oid: -1, Fid: 33, Tp: 2}, model.ErrInvalidOid},
		{"fid 为 0 合法（默认夹）", &rpc.DelFavReq{Mid: 7, Oid: 10001, Fid: 0, Tp: 2}, nil},
		{"mid 优先于 oid", &rpc.DelFavReq{Mid: 0, Oid: 0}, model.ErrInvalidMid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedFavItem(st, 7, 10001, 33, 2, 11, 0)
			before := st.log.snapshot()
			got, err := NewDelFavLogic(context.Background(), newTestSvc(st)).DelFav(tc.in)
			if tc.want == nil {
				// 合法入参必须真的走到依赖，否则这张守卫表是空的。
				wantNoErr(t, tc.name, err)
				if st.log.snapshot() == before {
					t.Fatalf("%s：合法入参却没有任何依赖调用", tc.name)
				}
				return
			}
			wantFail(t, tc.name, got, err, tc.want)
			wantNoCall(t, tc.name, st, before)
		})
	}
}

// TestDelFavMarksRowCancelledAndRefreshesCache 正常路径逐字段 + 顺序。
func TestDelFavMarksRowCancelledAndRefreshesCache(t *testing.T) {
	st := newStore()
	seedFavItem(st, 7, 10001, 33, 2, 11, 0)

	got, err := NewDelFavLogic(context.Background(), newTestSvc(st)).
		DelFav(&rpc.DelFavReq{Mid: 7, Oid: 10001, Fid: 33, Tp: 2})
	wantNoErr(t, "DelFav", err)
	if got == nil {
		t.Fatalf("DelFav() 响应 = nil")
	}
	wantOps(t, "DelFav", st.log.opsFrom(0), []string{
		"favItem.Del:7:10001:2:33",
		"cache.SetIsFavored:7:10001:2:0",
		"cache.DelFolders:7",
	})
	row := st.favItem.get(7, 10001, 2)
	if row == nil {
		t.Fatalf("favorite_item 行丢了（软删不该删行）")
	}
	wantEQ(t, "取消收藏", "State 软删", row.State, int32(1))
	wantEQ(t, "取消收藏", "Fid 不变", row.Fid, int64(33))
	wantEQ(t, "取消收藏", "Oid 不变", row.Oid, int64(10001))
	wantEQ(t, "取消收藏", "没走不带 fid 的兜底", st.favItem.fallbacks, 0)
	isRecentUnix(t, "取消收藏", "Mtime 已刷新", row.Mtime)
	// 收藏标记改成 false（不是删 key），且不能顺手写 true。
	faved, hit := st.cache.isFavoredCached(7, 10001, 2)
	wantEQ(t, "取消收藏", "缓存命中", hit, true)
	wantEQ(t, "取消收藏", "缓存说未收藏", faved, false)
}

// TestDelFavIsIdempotentOnMissingRow 本域不变量：取消一个从没收藏过的对象，
// 两段 UPDATE 都受影响 0 行，但必须**不报错**（客户端重复点「取消」不应看到 5xx）。
func TestDelFavIsIdempotentOnMissingRow(t *testing.T) {
	st := newStore()

	got, err := NewDelFavLogic(context.Background(), newTestSvc(st)).
		DelFav(&rpc.DelFavReq{Mid: 7, Oid: 10001, Fid: 33, Tp: 2})
	wantNoErr(t, "取消不存在的收藏", err)
	if got == nil {
		t.Fatalf("DelFav() 响应 = nil")
	}
	wantOps(t, "取消不存在的收藏", st.log.opsFrom(0), []string{
		"favItem.Del:7:10001:2:33",
		"cache.SetIsFavored:7:10001:2:0",
		"cache.DelFolders:7",
	})
	wantEQ(t, "取消不存在的收藏", "没有凭空插入行", st.favItem.rowCount(), 0)
	wantEQ(t, "取消不存在的收藏", "走了一次兜底 UPDATE", st.favItem.fallbacks, 1)
}

// TestDelFavRepeatedCancelKeepsState 已取消的行再取消：按 fid + state=0 不命中 →
// 兜底也不命中（state 已经是 1）→ 仍不报错，state 保持 1。
func TestDelFavRepeatedCancelKeepsState(t *testing.T) {
	st := newStore()
	seedFavItem(st, 7, 10001, 33, 2, 11, 1)

	l := NewDelFavLogic(context.Background(), newTestSvc(st))
	if _, err := l.DelFav(&rpc.DelFavReq{Mid: 7, Oid: 10001, Fid: 33, Tp: 2}); err != nil {
		t.Fatalf("第一次取消：%v", err)
	}
	if _, err := l.DelFav(&rpc.DelFavReq{Mid: 7, Oid: 10001, Fid: 33, Tp: 2}); err != nil {
		t.Fatalf("第二次取消：%v", err)
	}
	wantCount(t, "重复取消", st.log, "favItem.Del:", 2)
	wantEQ(t, "重复取消", "行数仍为 1", st.favItem.rowCount(), 1)
	wantEQ(t, "重复取消", "State", st.favItem.get(7, 10001, 2).State, int32(1))
	wantEQ(t, "重复取消", "两次都走兜底", st.favItem.fallbacks, 2)
}

// TestDelFavWrongFidFallsBackToAnyFolder 传错 fid 时，兜底 UPDATE（不带 fid）仍会把
// 那一行取消掉。这是 model 的既有口径，用例锁住它，避免有人「顺手加个 fid 校验」
// 改变客户端可见行为。
func TestDelFavWrongFidFallsBackToAnyFolder(t *testing.T) {
	st := newStore()
	seedFavItem(st, 7, 10001, 33, 2, 11, 0)

	_, err := NewDelFavLogic(context.Background(), newTestSvc(st)).
		DelFav(&rpc.DelFavReq{Mid: 7, Oid: 10001, Fid: 99, Tp: 2})
	wantNoErr(t, "fid 不匹配", err)
	wantEQ(t, "fid 不匹配", "走了兜底", st.favItem.fallbacks, 1)
	wantEQ(t, "fid 不匹配", "行被取消", st.favItem.get(7, 10001, 2).State, int32(1))
	wantEQ(t, "fid 不匹配", "Fid 保留原值（兜底 SQL 不改 fid）", st.favItem.get(7, 10001, 2).Fid, int64(33))
}

// TestDelFavOnlyOwnTpRowIsTouched tp 参与唯一键：同 oid 不同 tp 的行不得被波及。
func TestDelFavOnlyOwnTpRowIsTouched(t *testing.T) {
	st := newStore()
	seedFavItem(st, 7, 10001, 33, 2, 11, 0)
	seedFavItem(st, 7, 10001, 33, 12, 0, 0) // 同 oid 的音频收藏

	_, err := NewDelFavLogic(context.Background(), newTestSvc(st)).
		DelFav(&rpc.DelFavReq{Mid: 7, Oid: 10001, Fid: 33, Tp: 2})
	wantNoErr(t, "DelFav", err)
	wantEQ(t, "按 tp 隔离", "视频收藏已取消", st.favItem.get(7, 10001, 2).State, int32(1))
	wantEQ(t, "按 tp 隔离", "音频收藏不受影响", st.favItem.get(7, 10001, 12).State, int32(0))
}

// TestDelFavPropagatesModelFailure UPDATE 失败时不得把缓存改成「未收藏」——
// 否则用户刷新后看到收藏还在，但按钮态已经变了。
func TestDelFavPropagatesModelFailure(t *testing.T) {
	st := newStore()
	seedFavItem(st, 7, 10001, 33, 2, 11, 0)
	st.cache.warmIsFavored(7, 10001, 2, true)
	st.favItem.failWith("Del", errBoom)

	got, err := NewDelFavLogic(context.Background(), newTestSvc(st)).
		DelFav(&rpc.DelFavReq{Mid: 7, Oid: 10001, Fid: 33, Tp: 2})
	wantFail(t, "Del 失败", got, err, errBoom)
	wantOps(t, "Del 失败后的调用", st.log.opsFrom(0), []string{"favItem.Del:7:10001:2:33"})
	wantEQ(t, "Del 失败", "State 未改", st.favItem.get(7, 10001, 2).State, int32(0))
	faved, _ := st.cache.isFavoredCached(7, 10001, 2)
	wantEQ(t, "Del 失败", "缓存仍是已收藏", faved, true)
}

// TestDelFavSwallowsCacheFailure 缓存失败静默：取消已落库，返回成功。
func TestDelFavSwallowsCacheFailure(t *testing.T) {
	st := newStore()
	seedFavItem(st, 7, 10001, 33, 2, 11, 0)
	st.cache.failWith("SetIsFavored", errBoom)
	st.cache.failWith("DelFolders", errBoom)

	got, err := NewDelFavLogic(context.Background(), newTestSvc(st)).
		DelFav(&rpc.DelFavReq{Mid: 7, Oid: 10001, Fid: 33, Tp: 2})
	wantNoErr(t, "缓存失败不影响取消收藏", err)
	if got == nil {
		t.Fatalf("DelFav() 响应 = nil")
	}
	wantEQ(t, "缓存失败", "已软删", st.favItem.get(7, 10001, 2).State, int32(1))
	wantCount(t, "缓存失败", st.log, "cache.", 2)
}
