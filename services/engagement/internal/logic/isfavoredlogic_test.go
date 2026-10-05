package logic

import (
	"context"
	"testing"

	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"
)

// TestIsFavoredRejectsGuardsBeforeTouchingDeps mid → oid 两条守卫，都在读缓存之前。
func TestIsFavoredRejectsGuardsBeforeTouchingDeps(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.IsFavoredReq
		want error
	}{
		{"mid 为 0", &rpc.IsFavoredReq{Mid: 0, Oid: 10001, Tp: 2}, model.ErrInvalidMid},
		{"mid 为负", &rpc.IsFavoredReq{Mid: -7, Oid: 10001, Tp: 2}, model.ErrInvalidMid},
		{"oid 为 0", &rpc.IsFavoredReq{Mid: 7, Oid: 0, Tp: 2}, model.ErrInvalidOid},
		{"oid 为负", &rpc.IsFavoredReq{Mid: 7, Oid: -1, Tp: 2}, model.ErrInvalidOid},
		{"mid 优先于 oid", &rpc.IsFavoredReq{Mid: 0, Oid: 0}, model.ErrInvalidMid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedFavItem(st, 7, 10001, 33, 2, 11, 0)
			before := st.log.snapshot()
			got, err := NewIsFavoredLogic(context.Background(), newTestSvc(st)).IsFavored(tc.in)
			wantFail(t, tc.name, got, err, tc.want)
			wantNoCall(t, tc.name, st, before)
		})
	}
}

// TestIsFavoredServesFromCacheOnHit 缓存命中（正缓存）时**不回源**，
// 也不回填（回填会造成无谓的 SET）。
func TestIsFavoredServesFromCacheOnHit(t *testing.T) {
	st := newStore()
	st.cache.warmIsFavored(7, 10001, 2, true)
	// 库里其实是「已取消」：一旦回源就会和缓存不一致，用例据此判断有没有真的读库。
	seedFavItem(st, 7, 10001, 33, 2, 11, 1)

	got, err := NewIsFavoredLogic(context.Background(), newTestSvc(st)).
		IsFavored(&rpc.IsFavoredReq{Mid: 7, Oid: 10001, Tp: 2})
	wantNoErr(t, "命中正缓存", err)
	wantEQ(t, "命中正缓存", "Faved（以缓存为准）", got.Faved, true)
	wantOps(t, "命中正缓存", st.log.opsFrom(0), []string{"cache.GetIsFavored:7:10001:2"})
	wantCount(t, "命中正缓存", st.log, "favItem.IsFavored", 0)
	wantCount(t, "命中正缓存", st.log, "cache.SetIsFavored", 0)
}

// TestIsFavoredServesNegativeCache 负缓存同样短路：未收藏的结论也带 60s TTL，
// 否则「取消收藏后立刻再问一次」会打穿到 DB。
func TestIsFavoredServesNegativeCache(t *testing.T) {
	st := newStore()
	st.cache.warmIsFavored(7, 10001, 2, false)
	seedFavItem(st, 7, 10001, 33, 2, 11, 0) // 库里其实是已收藏：证明答案来自缓存

	got, err := NewIsFavoredLogic(context.Background(), newTestSvc(st)).
		IsFavored(&rpc.IsFavoredReq{Mid: 7, Oid: 10001, Tp: 2})
	wantNoErr(t, "命中负缓存", err)
	wantEQ(t, "命中负缓存", "Faved", got.Faved, false)
	wantOps(t, "命中负缓存", st.log.opsFrom(0), []string{"cache.GetIsFavored:7:10001:2"})
}

// TestIsFavoredMissReadsThroughAndBackfillsTrue 读穿 + 回填正标记。
func TestIsFavoredMissReadsThroughAndBackfillsTrue(t *testing.T) {
	st := newStore()
	seedFavItem(st, 7, 10001, 33, 2, 11, 0)

	got, err := NewIsFavoredLogic(context.Background(), newTestSvc(st)).
		IsFavored(&rpc.IsFavoredReq{Mid: 7, Oid: 10001, Tp: 2})
	wantNoErr(t, "读穿", err)
	wantEQ(t, "读穿", "Faved", got.Faved, true)
	wantOps(t, "读穿", st.log.opsFrom(0), []string{
		"cache.GetIsFavored:7:10001:2",
		"favItem.IsFavored:7:10001:2",
		"cache.SetIsFavored:7:10001:2:1",
	})
	faved, hit := st.cache.isFavoredCached(7, 10001, 2)
	wantEQ(t, "读穿", "回填命中", hit, true)
	wantEQ(t, "读穿", "回填值", faved, true)
}

// TestIsFavoredMissReadsThroughAndBackfillsFalse 查无此行也要回填 false：
// 「没收藏过」是大多数请求，不回填就等于每次读都打库。
func TestIsFavoredMissReadsThroughAndBackfillsFalse(t *testing.T) {
	st := newStore()

	got, err := NewIsFavoredLogic(context.Background(), newTestSvc(st)).
		IsFavored(&rpc.IsFavoredReq{Mid: 7, Oid: 10001, Tp: 2})
	wantNoErr(t, "读穿未收藏", err)
	wantEQ(t, "读穿未收藏", "Faved", got.Faved, false)
	wantOps(t, "读穿未收藏", st.log.opsFrom(0), []string{
		"cache.GetIsFavored:7:10001:2",
		"favItem.IsFavored:7:10001:2",
		"cache.SetIsFavored:7:10001:2:0",
	})
	faved, hit := st.cache.isFavoredCached(7, 10001, 2)
	wantEQ(t, "读穿未收藏", "回填了负标记", hit, true)
	wantEQ(t, "读穿未收藏", "回填值", faved, false)
}

// TestIsFavoredCancelledRowIsFalse favorite_item 是软删表：state=1 的行必须报未收藏，
// 而不是「有行就算收藏」。
func TestIsFavoredCancelledRowIsFalse(t *testing.T) {
	st := newStore()
	seedFavItem(st, 7, 10001, 33, 2, 11, 1)

	got, err := NewIsFavoredLogic(context.Background(), newTestSvc(st)).
		IsFavored(&rpc.IsFavoredReq{Mid: 7, Oid: 10001, Tp: 2})
	wantNoErr(t, "已取消的收藏", err)
	wantEQ(t, "已取消的收藏", "Faved", got.Faved, false)
	wantCount(t, "已取消的收藏", st.log, "cache.SetIsFavored:", 1)
}

// TestIsFavoredTpIsPartOfTheKey 同 oid 不同 tp 是不同对象，缓存与库都按 tp 隔离。
func TestIsFavoredTpIsPartOfTheKey(t *testing.T) {
	st := newStore()
	seedFavItem(st, 7, 10001, 33, 2, 11, 0)

	vid, err := NewIsFavoredLogic(context.Background(), newTestSvc(st)).
		IsFavored(&rpc.IsFavoredReq{Mid: 7, Oid: 10001, Tp: 2})
	wantNoErr(t, "视频", err)
	audio, err := NewIsFavoredLogic(context.Background(), newTestSvc(st)).
		IsFavored(&rpc.IsFavoredReq{Mid: 7, Oid: 10001, Tp: 12})
	wantNoErr(t, "音频", err)
	wantEQ(t, "按 tp 隔离", "视频已收藏", vid.Faved, true)
	wantEQ(t, "按 tp 隔离", "音频未收藏", audio.Faved, false)
	wantOps(t, "按 tp 隔离", st.log.opsFrom(0), []string{
		"cache.GetIsFavored:7:10001:2", "favItem.IsFavored:7:10001:2", "cache.SetIsFavored:7:10001:2:1",
		"cache.GetIsFavored:7:10001:12", "favItem.IsFavored:7:10001:12", "cache.SetIsFavored:7:10001:12:0",
	})
}

// TestIsFavoredPropagatesCacheReadFailure 读缓存失败时**不回源**：
// Repository 直接把错误透出（宁可不答，也不在 Redis 抖动时打爆 MySQL）。
func TestIsFavoredPropagatesCacheReadFailure(t *testing.T) {
	st := newStore()
	seedFavItem(st, 7, 10001, 33, 2, 11, 0)
	st.cache.failWith("GetIsFavored", errBoom)

	got, err := NewIsFavoredLogic(context.Background(), newTestSvc(st)).
		IsFavored(&rpc.IsFavoredReq{Mid: 7, Oid: 10001, Tp: 2})
	wantFail(t, "读缓存失败", got, err, errBoom)
	wantOps(t, "读缓存失败后的调用", st.log.opsFrom(0), []string{"cache.GetIsFavored:7:10001:2"})
}

// TestIsFavoredPropagatesModelFailure 回源失败时不得回填缓存（否则把故障态钉成 60s 的假结论）。
func TestIsFavoredPropagatesModelFailure(t *testing.T) {
	st := newStore()
	st.favItem.failWith("IsFavored", errBoom)

	got, err := NewIsFavoredLogic(context.Background(), newTestSvc(st)).
		IsFavored(&rpc.IsFavoredReq{Mid: 7, Oid: 10001, Tp: 2})
	wantFail(t, "回源失败", got, err, errBoom)
	wantOps(t, "回源失败后的调用", st.log.opsFrom(0), []string{
		"cache.GetIsFavored:7:10001:2", "favItem.IsFavored:7:10001:2",
	})
	_, hit := st.cache.isFavoredCached(7, 10001, 2)
	wantEQ(t, "回源失败", "没有写脏缓存", hit, false)
}

// TestIsFavoredSwallowsBackfillFailure 回填失败静默：结论仍按库里读到的值返回。
func TestIsFavoredSwallowsBackfillFailure(t *testing.T) {
	st := newStore()
	seedFavItem(st, 7, 10001, 33, 2, 11, 0)
	st.cache.failWith("SetIsFavored", errBoom)

	got, err := NewIsFavoredLogic(context.Background(), newTestSvc(st)).
		IsFavored(&rpc.IsFavoredReq{Mid: 7, Oid: 10001, Tp: 2})
	wantNoErr(t, "回填失败", err)
	wantEQ(t, "回填失败", "Faved 仍来自库里", got.Faved, true)
	wantOps(t, "回填失败", st.log.opsFrom(0), []string{
		"cache.GetIsFavored:7:10001:2", "favItem.IsFavored:7:10001:2", "cache.SetIsFavored:7:10001:2:1",
	})
	_, hit := st.cache.isFavoredCached(7, 10001, 2)
	wantEQ(t, "回填失败", "缓存没写上", hit, false)
}
