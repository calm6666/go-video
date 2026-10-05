package logic

import (
	"context"
	"testing"

	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"
)

// TestIsFavoredsRejectsGuardsBeforeTouchingDeps 三档：mid → 空列表（短路成功）→ 超 100。
// 注意超限时复用的是 ErrTooManyMessageIDs（文案写的是 message_ids），
// 对 IsFavoreds 的 oids 来说文案不准，见 README 已知缺口。
func TestIsFavoredsRejectsGuardsBeforeTouchingDeps(t *testing.T) {
	oids := make([]int64, 101)
	for i := range oids {
		oids[i] = int64(1000 + i)
	}
	cases := []struct {
		name string
		in   *rpc.IsFavoredsReq
		want error
	}{
		{"mid 为 0", &rpc.IsFavoredsReq{Mid: 0, Oids: []int64{10001}, Tp: 2}, model.ErrInvalidMid},
		{"mid 为负", &rpc.IsFavoredsReq{Mid: -7, Oids: []int64{10001}, Tp: 2}, model.ErrInvalidMid},
		{"101 个 oid 超限", &rpc.IsFavoredsReq{Mid: 7, Oids: oids, Tp: 2}, model.ErrTooManyMessageIDs},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedFavItem(st, 7, 10001, 33, 2, 11, 0)
			before := st.log.snapshot()
			got, err := NewIsFavoredsLogic(context.Background(), newTestSvc(st)).IsFavoreds(tc.in)
			wantFail(t, tc.name, got, err, tc.want)
			wantNoCall(t, tc.name, st, before)
		})
	}

	t.Run("空列表短路返回空 map", func(t *testing.T) {
		st := newStore()
		before := st.log.snapshot()
		got, err := NewIsFavoredsLogic(context.Background(), newTestSvc(st)).
			IsFavoreds(&rpc.IsFavoredsReq{Mid: 7, Oids: nil, Tp: 2})
		wantNoErr(t, "空列表", err)
		if got == nil {
			t.Fatalf("空列表应返回空 map 而不是 nil")
		}
		wantEQ(t, "空列表", "faveds 长度", len(got.Faveds), 0)
		wantNoCall(t, "空列表", st, before)
	})

	t.Run("刚好 100 个不超限", func(t *testing.T) {
		st := newStore()
		oids := make([]int64, 100)
		for i := range oids {
			oids[i] = int64(1000 + i)
		}
		got, err := NewIsFavoredsLogic(context.Background(), newTestSvc(st)).
			IsFavoreds(&rpc.IsFavoredsReq{Mid: 7, Oids: oids, Tp: 2})
		wantNoErr(t, "100 个 oid", err)
		wantEQ(t, "100 个 oid", "faveds 长度", len(got.Faveds), 0)
		wantCount(t, "100 个 oid", st.log, "favItem.IsFavoreds:", 1)
	})
}

// TestIsFavoredsProjectsFoundRowsOnly 逐条核对投影，并锁定「查不到的 oid 不出现在 map 里」：
// 客户端必须按「缺键 = 未收藏」处理，而不是服务端补 false。
func TestIsFavoredsProjectsFoundRowsOnly(t *testing.T) {
	st := newStore()
	seedFavItem(st, 7, 10001, 33, 2, 11, 0)  // 有效收藏
	seedFavItem(st, 7, 10002, 33, 2, 11, 1)  // 已取消
	seedFavItem(st, 7, 10004, 33, 12, 0, 0)  // 别的 tp，不该被读到
	seedFavItem(st, 99, 10001, 33, 2, 11, 0) // 别人的，不该被读到

	got, err := NewIsFavoredsLogic(context.Background(), newTestSvc(st)).
		IsFavoreds(&rpc.IsFavoredsReq{Mid: 7, Oids: []int64{10001, 10002, 10003}, Tp: 2})
	wantNoErr(t, "IsFavoreds", err)
	wantOps(t, "IsFavoreds", st.log.opsFrom(0), []string{"favItem.IsFavoreds:7:2:10001|10002|10003"})
	wantEQ(t, "IsFavoreds", "map 条数（只含查到的行）", len(got.Faveds), 2)
	wantEQ(t, "IsFavoreds", "10001 已收藏", got.Faveds[10001], true)
	wantEQ(t, "IsFavoreds", "10002 已取消 → false", got.Faveds[10002], false)
	_, absent := got.Faveds[10003]
	wantEQ(t, "IsFavoreds", "10003 无行 → 键都不出现", absent, false)
	_, otherTp := got.Faveds[10004]
	wantEQ(t, "IsFavoreds", "不同 tp 未混入", otherTp, false)
}

// TestIsFavoredsBypassesCache 记录真实口径：批量接口**完全不走缓存**
// （单条 IsFavored 才有 eng:fav:* 缓存），所以批量读永远回源。
// 这是与单条接口的一致性差异：AddFav 只失效/更新单条 key，批量结果不受影响。
func TestIsFavoredsBypassesCache(t *testing.T) {
	st := newStore()
	seedFavItem(st, 7, 10001, 33, 2, 11, 0)
	st.cache.warmIsFavored(7, 10001, 2, false) // 缓存说是未收藏

	got, err := NewIsFavoredsLogic(context.Background(), newTestSvc(st)).
		IsFavoreds(&rpc.IsFavoredsReq{Mid: 7, Oids: []int64{10001}, Tp: 2})
	wantNoErr(t, "批量读", err)
	wantEQ(t, "批量读", "以库为准", got.Faveds[10001], true)
	wantOps(t, "批量读", st.log.opsFrom(0), []string{"favItem.IsFavoreds:7:2:10001"})
	wantCount(t, "批量读", st.log, "cache.", 0)
}

// TestIsFavoredsDeduplicatesOids IN (?) 传重复 oid 时只产出一个键，且只发一次查询。
func TestIsFavoredsDeduplicatesOids(t *testing.T) {
	st := newStore()
	seedFavItem(st, 7, 10001, 33, 2, 11, 0)

	got, err := NewIsFavoredsLogic(context.Background(), newTestSvc(st)).
		IsFavoreds(&rpc.IsFavoredsReq{Mid: 7, Oids: []int64{10001, 10001}, Tp: 2})
	wantNoErr(t, "重复 oid", err)
	wantEQ(t, "重复 oid", "map 条数", len(got.Faveds), 1)
	wantOps(t, "重复 oid", st.log.opsFrom(0), []string{"favItem.IsFavoreds:7:2:10001|10001"})
}

// TestIsFavoredsPropagatesModelFailure 唯一依赖失败必须透出，不能返回空 map 当「都没收藏」。
func TestIsFavoredsPropagatesModelFailure(t *testing.T) {
	st := newStore()
	seedFavItem(st, 7, 10001, 33, 2, 11, 0)
	st.favItem.failWith("IsFavoreds", errBoom)

	got, err := NewIsFavoredsLogic(context.Background(), newTestSvc(st)).
		IsFavoreds(&rpc.IsFavoredsReq{Mid: 7, Oids: []int64{10001}, Tp: 2})
	wantFail(t, "IsFavoreds 失败", got, err, errBoom)
	wantOps(t, "IsFavoreds 失败后的调用", st.log.opsFrom(0), []string{"favItem.IsFavoreds:7:2:10001"})
}
