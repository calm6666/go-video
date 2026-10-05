package logic

import (
	"context"
	"testing"

	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"
)

// TestAddFavRejectsGuardsBeforeTouchingDeps 锁三条守卫的顺序与「不触库、不触缓存」。
// fid 也必须为正：favorite_item.fid=0 是「默认夹」的哨兵，但 AddFav 契约要求显式指定夹子，
// 传 0 会把收藏悄悄塞进默认夹，属于伪成功。
func TestAddFavRejectsGuardsBeforeTouchingDeps(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.AddFavReq
		want error
	}{
		{"mid 为 0", &rpc.AddFavReq{Mid: 0, Oid: 101, Fid: 3, Tp: 2}, model.ErrInvalidMid},
		{"mid 为负", &rpc.AddFavReq{Mid: -7, Oid: 101, Fid: 3, Tp: 2}, model.ErrInvalidMid},
		{"oid 为 0（mid 合法）", &rpc.AddFavReq{Mid: 7, Oid: 0, Fid: 3, Tp: 2}, model.ErrInvalidOid},
		{"oid 为负", &rpc.AddFavReq{Mid: 7, Oid: -1, Fid: 3, Tp: 2}, model.ErrInvalidOid},
		{"fid 为 0（mid/oid 合法）", &rpc.AddFavReq{Mid: 7, Oid: 101, Fid: 0, Tp: 2}, model.ErrInvalidFid},
		{"fid 为负", &rpc.AddFavReq{Mid: 7, Oid: 101, Fid: -3, Tp: 2}, model.ErrInvalidFid},
		// 多条非法时报最先命中的一条：mid → oid → fid。
		{"mid 优先于 oid", &rpc.AddFavReq{Mid: 0, Oid: 0, Fid: 3}, model.ErrInvalidMid},
		{"oid 优先于 fid", &rpc.AddFavReq{Mid: 7, Oid: 0, Fid: 0}, model.ErrInvalidOid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			before := st.log.snapshot()
			got, err := NewAddFavLogic(context.Background(), newTestSvc(st)).AddFav(tc.in)
			wantFail(t, tc.name, got, err, tc.want)
			wantNoCall(t, tc.name, st, before)
		})
	}
}

// TestAddFavWritesRowAndRefreshesCache 逐字段核对落库投影与调用顺序。
func TestAddFavWritesRowAndRefreshesCache(t *testing.T) {
	st := newStore()
	in := &rpc.AddFavReq{Mid: 7, Oid: 10001, Fid: 33, Tp: 2, Otype: 11}

	got, err := NewAddFavLogic(context.Background(), newTestSvc(st)).AddFav(in)
	wantNoErr(t, "AddFav", err)
	if got == nil {
		t.Fatalf("AddFav() 响应 = nil, want 非空 EmptyReply")
	}
	// 顺序：先落关系行，再更新「是否已收藏」标记，最后失效收藏夹列表缓存。
	wantOps(t, "AddFav", st.log.opsFrom(0), []string{
		"favItem.Add:7:10001:2:33",
		"cache.SetIsFavored:7:10001:2:1",
		"cache.DelFolders:7",
	})

	row := st.favItem.get(7, 10001, 2)
	if row == nil {
		t.Fatalf("favorite_item 未落库")
	}
	wantEQ(t, "落库", "Mid", row.Mid, int64(7))
	wantEQ(t, "落库", "Oid", row.Oid, int64(10001))
	wantEQ(t, "落库", "Fid", row.Fid, int64(33))
	wantEQ(t, "落库", "Tp", row.Tp, int32(2))
	wantEQ(t, "落库", "Otype", row.Otype, int32(11))
	wantEQ(t, "落库", "State（有效收藏）", row.State, int32(0))
	wantEQ(t, "落库", "主键自增分配", row.ID, int64(1))
	// ctime/mtime 由 model 内部取 now（logic 传的 Ctime/Mtime 被 SQL 忽略），只断非零窗口。
	isRecentUnix(t, "落库", "Ctime", row.Ctime)
	isRecentUnix(t, "落库", "Mtime", row.Mtime)

	// 缓存：布尔标记是「更新」而不是「删除」（DelIsFavored 一次都不该出现），列表缓存必须失效。
	faved, hit := st.cache.isFavoredCached(7, 10001, 2)
	wantEQ(t, "收藏标记缓存", "hit", hit, true)
	wantEQ(t, "收藏标记缓存", "faved", faved, true)
	wantCount(t, "收藏标记缓存", st.log, "cache.DelIsFavored", 0)
	wantEQ(t, "收藏夹列表缓存", "已失效", st.cache.foldersPayload(7), "")
}

// TestAddFavRejectsDifferentFolderAsSecondRow 本域不变量：
// favorite_item 的唯一键是 (mid, oid, tp)，fid 不在其中，所以「换收藏夹」是
// 把已有那一行的 fid 改掉，而不是插第二行（对着迁移 SQL 的 uniq_mid_oid_tp 断言）。
func TestAddFavRejectsDifferentFolderAsSecondRow(t *testing.T) {
	st := newStore()
	l := NewAddFavLogic(context.Background(), newTestSvc(st))
	if _, err := l.AddFav(&rpc.AddFavReq{Mid: 7, Oid: 10001, Fid: 33, Tp: 2, Otype: 11}); err != nil {
		t.Fatalf("首次收藏：%v", err)
	}
	if _, err := l.AddFav(&rpc.AddFavReq{Mid: 7, Oid: 10001, Fid: 44, Tp: 2, Otype: 11}); err != nil {
		t.Fatalf("改夹收藏：%v", err)
	}

	wantEQ(t, "重复收藏", "favorite_item 行数", st.favItem.rowCount(), 1)
	row := st.favItem.get(7, 10001, 2)
	if row == nil {
		t.Fatalf("favorite_item 行丢了")
	}
	wantEQ(t, "重复收藏", "Fid 已改到新夹", row.Fid, int64(44))
	wantEQ(t, "重复收藏", "State 仍为有效", row.State, int32(0))
	wantEQ(t, "重复收藏", "主键未换（是 UPDATE 不是 INSERT）", row.ID, int64(1))
	wantCount(t, "重复收藏", st.log, "favItem.Add:", 2)
}

// TestAddFavRevivesCancelledItem 取消过的收藏再收一次：唯一键命中 → state 从 1（已取消）
// 改回 0，不新增行。这是「重复上报不重复落库」的另一半。
func TestAddFavRevivesCancelledItem(t *testing.T) {
	st := newStore()
	seedFavItem(st, 7, 10001, 33, 2, 11, 1) // state=1：已取消的收藏行

	got, err := NewAddFavLogic(context.Background(), newTestSvc(st)).
		AddFav(&rpc.AddFavReq{Mid: 7, Oid: 10001, Fid: 55, Tp: 2, Otype: 11})
	wantNoErr(t, "重新收藏", err)
	if got == nil {
		t.Fatalf("AddFav() 响应 = nil")
	}
	wantOps(t, "重新收藏", st.log.opsFrom(0), []string{
		"favItem.Add:7:10001:2:55",
		"cache.SetIsFavored:7:10001:2:1",
		"cache.DelFolders:7",
	})
	row := st.favItem.get(7, 10001, 2)
	wantEQ(t, "重新收藏", "State", row.State, int32(0))
	wantEQ(t, "重新收藏", "Fid", row.Fid, int64(55))
	wantEQ(t, "重新收藏", "行数", st.favItem.rowCount(), 1)
}

// TestAddFavPropagatesModelFailure 下游失败：favItem.Add 报错时必须整体失败，
// 且**不能**留下「缓存说已收藏、库说没有」的伪成功。
func TestAddFavPropagatesModelFailure(t *testing.T) {
	st := newStore()
	st.favItem.failWith("Add", errBoom)

	got, err := NewAddFavLogic(context.Background(), newTestSvc(st)).
		AddFav(&rpc.AddFavReq{Mid: 7, Oid: 10001, Fid: 33, Tp: 2, Otype: 11})
	wantFail(t, "Add 失败", got, err, errBoom)
	wantOps(t, "Add 失败后的调用", st.log.opsFrom(0), []string{"favItem.Add:7:10001:2:33"})
	wantEQ(t, "Add 失败", "未落库", st.favItem.rowCount(), 0)
	_, hit := st.cache.isFavoredCached(7, 10001, 2)
	wantEQ(t, "Add 失败", "收藏标记缓存未被污染", hit, false)
}

// TestAddFavSwallowsCacheFailure 记录真实口径：缓存写失败被 Repository 丢弃（`_ =`），
// 主链路仍返回成功。这是刻意的可用性取舍，但缓存会在 TTL 内继续说「未收藏」，
// 见 README 已知缺口（缺陷 #5：缓存写失败静默）。
func TestAddFavSwallowsCacheFailure(t *testing.T) {
	st := newStore()
	st.cache.failWith("SetIsFavored", errBoom)
	st.cache.failWith("DelFolders", errOther)

	got, err := NewAddFavLogic(context.Background(), newTestSvc(st)).
		AddFav(&rpc.AddFavReq{Mid: 7, Oid: 10001, Fid: 33, Tp: 2})
	wantNoErr(t, "缓存写失败不应影响收藏", err)
	if got == nil {
		t.Fatalf("AddFav() 响应 = nil")
	}
	wantEQ(t, "缓存写失败", "关系行已落库", st.favItem.rowCount(), 1)
	wantOps(t, "缓存写失败", st.log.opsFrom(0), []string{
		"favItem.Add:7:10001:2:33", "cache.SetIsFavored:7:10001:2:1", "cache.DelFolders:7",
	})
	// 两次缓存写都没落上，所以既没有标记也没有列表缓存 —— 下次读必须回源。
	_, hit := st.cache.isFavoredCached(7, 10001, 2)
	wantEQ(t, "缓存写失败", "标记仍缺失", hit, false)
}
