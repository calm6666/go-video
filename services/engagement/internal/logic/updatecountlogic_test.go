package logic

import (
	"context"
	"testing"

	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"
)

// TestUpdateCountRejectsGuardsBeforeTouchingDeps 只有 business 与 message_id 两条守卫；
// origin_id、两个 change、operator 都不校验（负数、超大值都能进到 SQL）。
func TestUpdateCountRejectsGuardsBeforeTouchingDeps(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.UpdateCountReq
		want error
	}{
		{"business 空", &rpc.UpdateCountReq{Business: "", MessageId: 101, LikeChange: 5}, model.ErrInvalidBusiness},
		{"message_id 为 0", &rpc.UpdateCountReq{Business: likeBiz, MessageId: 0, LikeChange: 5}, model.ErrInvalidMessage},
		{"message_id 为负", &rpc.UpdateCountReq{Business: likeBiz, MessageId: -101, LikeChange: 5}, model.ErrInvalidMessage},
		{"business 优先于 message_id", &rpc.UpdateCountReq{Business: "", MessageId: 0}, model.ErrInvalidBusiness},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedStat(st, likeBiz, likeOrigin, likeMessage, 5, 2, 1, 1)
			before := st.log.snapshot()
			got, err := NewUpdateCountLogic(context.Background(), newTestSvc(st)).UpdateCount(tc.in)
			wantFail(t, tc.name, got, err, tc.want)
			wantNoCall(t, tc.name, st, before)
		})
	}

	// 对照：change 为 0 也是合法请求（不校验），照样落一次 UPDATE。
	t.Run("change 为 0 不报错", func(t *testing.T) {
		st := newStore()
		seedStat(st, likeBiz, likeOrigin, likeMessage, 5, 2, 1, 1)
		got, err := NewUpdateCountLogic(context.Background(), newTestSvc(st)).
			UpdateCount(&rpc.UpdateCountReq{Business: likeBiz, OriginId: likeOrigin, MessageId: likeMessage})
		wantNoErr(t, "零增量", err)
		wantEQ(t, "零增量", "响应体非 nil", got != nil, true)
		wantOps(t, "零增量", st.log.opsFrom(0), []string{
			"stat.UpdateChange:archive:1:101:0/0",
			"cache.IncrLike:lc:archive:1:101:0",
			"cache.IncrDislike:dc:archive:1:101:0",
		})
		wantEQ(t, "零增量", "like_change 不变", st.stat.get(likeBiz, likeOrigin, likeMessage).LikeChange, int64(1))
	})
}

// TestUpdateCountAccumulatesChangesAndKeepsNumber 逐字段核对：
// like_change/dislike_change 是**累加**（SQL 为 like_change = like_change + ?），
// 展示值 like_number/dislike_number 与时间戳一个都不许动 ——
// 运营修正只改修正位，展示值由计数链路负责（AGENTS.md §5 的域内归属）。
func TestUpdateCountAccumulatesChangesAndKeepsNumber(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, likeMessage, 100, 7, 3, 1)

	got, err := NewUpdateCountLogic(context.Background(), newTestSvc(st)).
		UpdateCount(&rpc.UpdateCountReq{
			Business: likeBiz, OriginId: likeOrigin, MessageId: likeMessage,
			LikeChange: -2, DislikeChange: 5, Operator: "op-alice",
		})
	wantNoErr(t, "UpdateCount", err)
	if got == nil {
		t.Fatalf("UpdateCount() 响应 = nil, want 非 nil EmptyReply")
	}
	wantOps(t, "UpdateCount", st.log.opsFrom(0), []string{
		"stat.UpdateChange:archive:1:101:-2/5",
		"cache.IncrLike:lc:archive:1:101:-2",
		"cache.IncrDislike:dc:archive:1:101:5",
	})

	row := st.stat.get(likeBiz, likeOrigin, likeMessage)
	wantEQ(t, "UpdateCount", "LikeChange 累加", row.LikeChange, int64(1))
	wantEQ(t, "UpdateCount", "DislikeChange 累加", row.DislikeChange, int64(6))
	wantEQ(t, "UpdateCount", "LikeNumber 不动", row.LikeNumber, int64(100))
	wantEQ(t, "UpdateCount", "DislikeNumber 不动", row.DislikeNumber, int64(7))
	wantEQ(t, "UpdateCount", "Business 不被改写", row.Business, likeBiz)
	wantEQ(t, "UpdateCount", "OriginID 不被改写", row.OriginID, likeOrigin)
	wantEQ(t, "UpdateCount", "MessageID 不被改写", row.MessageID, likeMessage)
	// 真实 SQL 的 SET 子句里没有 mtime —— 靠 mtime 做增量同步的作业看不到这次修正（已知缺口）。
	wantEQ(t, "UpdateCount", "Mtime 未更新", row.Mtime, int64(1_700_000_000))
	wantEQ(t, "UpdateCount", "Ctime 未更新", row.Ctime, int64(1_700_000_000))
}

// TestUpdateCountShadowCountersMoveTogether DB 修正位与 Redis 影子计数器同向：
// 两处都用同一份 delta，且负 delta 原样传下去（影子计数器允许为负，它是差值不是绝对值）。
func TestUpdateCountShadowCountersMoveTogether(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, likeMessage, 100, 7, 0, 0)

	_, err := NewUpdateCountLogic(context.Background(), newTestSvc(st)).
		UpdateCount(&rpc.UpdateCountReq{Business: likeBiz, OriginId: likeOrigin, MessageId: likeMessage, LikeChange: 9, DislikeChange: -4})
	wantNoErr(t, "影子计数器", err)
	wantEQ(t, "影子计数器", "like 影子增量", st.cache.likeCounter(likeBiz, likeOrigin, likeMessage), int64(9))
	wantEQ(t, "影子计数器", "dislike 影子增量", st.cache.dislikeCounter(likeBiz, likeOrigin, likeMessage), int64(-4))

	// 第二次调用继续累加，不是覆盖。
	_, err = NewUpdateCountLogic(context.Background(), newTestSvc(st)).
		UpdateCount(&rpc.UpdateCountReq{Business: likeBiz, OriginId: likeOrigin, MessageId: likeMessage, LikeChange: 1})
	wantNoErr(t, "第二次修正", err)
	wantEQ(t, "第二次修正", "like 影子增量累计", st.cache.likeCounter(likeBiz, likeOrigin, likeMessage), int64(10))
	wantEQ(t, "第二次修正", "DB 修正位累计", st.stat.get(likeBiz, likeOrigin, likeMessage).LikeChange, int64(10))
}

// TestUpdateCountIsNotIdempotent 与 Like/AddShare 相反，UpdateCount **没有**幂等保护：
// 它既不像 Like 那样先比对旧状态，也不像 AddShare 那样靠唯一键去重，
// 所以 RPC 重试会把同一份修正加两次。这里把「重复调用累加两次」钉成事实，
// 将来加去重（或改语义）时本用例会红。
func TestUpdateCountIsNotIdempotent(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, likeMessage, 100, 7, 0, 0)
	req := &rpc.UpdateCountReq{Business: likeBiz, OriginId: likeOrigin, MessageId: likeMessage, LikeChange: 10}

	for range 3 {
		_, err := NewUpdateCountLogic(context.Background(), newTestSvc(st)).UpdateCount(req)
		wantNoErr(t, "重复修正", err)
	}
	wantEQ(t, "缺陷：无幂等", "三次累加", st.stat.get(likeBiz, likeOrigin, likeMessage).LikeChange, int64(30))
	wantCount(t, "缺陷：无幂等", st.log, "stat.UpdateChange:", 3)
	wantEQ(t, "缺陷：无幂等", "展示值仍不变", st.stat.get(likeBiz, likeOrigin, likeMessage).LikeNumber, int64(100))
}

// TestUpdateCountMissingStatRowLosesCorrectionAndDiverges 缺陷 #3 的现象固化：
// UpdateChange 只有 UPDATE、没有 INSERT，也不看 RowsAffected，所以计数行不存在时
// 「修正成功」返回给运营，实际一位都没落地；紧接着 Redis 影子计数器却被推进了 ——
// DB 与 Redis 就此永久背离，直到定时回刷把 Redis 清零。
func TestUpdateCountMissingStatRowLosesCorrectionAndDiverges(t *testing.T) {
	st := newStore()

	got, err := NewUpdateCountLogic(context.Background(), newTestSvc(st)).
		UpdateCount(&rpc.UpdateCountReq{Business: likeBiz, OriginId: likeOrigin, MessageId: likeMessage, LikeChange: 50})
	wantNoErr(t, "无计数行", err)
	wantEQ(t, "缺陷 #3", "仍回空响应而非错误", got != nil, true)
	wantOps(t, "无计数行", st.log.opsFrom(0), []string{
		"stat.UpdateChange:archive:1:101:50/0",
		"cache.IncrLike:lc:archive:1:101:50",
		"cache.IncrDislike:dc:archive:1:101:0",
	})
	wantEQ(t, "缺陷 #3", "thumbup_stat 里没有新行", st.stat.rowCount(), 0)
	wantEQ(t, "缺陷 #3", "Redis 影子计数器却已 +50", st.cache.likeCounter(likeBiz, likeOrigin, likeMessage), int64(50))
}

// TestUpdateCountPropagatesModelFailure SQL 失败必须透出（运营面看不到假成功），
// 而且**不能**继续推进影子计数器 —— 这是本方法唯一一处真正的失败保护。
func TestUpdateCountPropagatesModelFailure(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, likeMessage, 100, 7, 0, 0)
	st.stat.faultInjector = faultInjector{}
	st.stat.failWith("UpdateChange", errBoom)

	got, err := NewUpdateCountLogic(context.Background(), newTestSvc(st)).
		UpdateCount(&rpc.UpdateCountReq{Business: likeBiz, OriginId: likeOrigin, MessageId: likeMessage, LikeChange: 5, DislikeChange: 5})
	wantFail(t, "UpdateChange 失败", got, err, errBoom)
	wantOps(t, "UpdateChange 失败后的调用", st.log.opsFrom(0), []string{"stat.UpdateChange:archive:1:101:5/5"})
	wantCount(t, "UpdateChange 失败", st.log, "cache.", 0)
	wantEQ(t, "UpdateChange 失败", "修正位未变", st.stat.get(likeBiz, likeOrigin, likeMessage).LikeChange, int64(0))
	wantEQ(t, "UpdateChange 失败", "影子计数器未动", st.cache.likeCounter(likeBiz, likeOrigin, likeMessage), int64(0))
}

// TestUpdateCountSwallowsCacheFailures 缓存侧两次 Incr 的错误都被丢弃：
// 接口回成功，但影子计数器少了一跳（与缺陷 #5 同源：缓存写失败静默）。
// 断言「即使 Redis 挂了就仍然成功」，同时记下 DB 与 Redis 可能不一致。
func TestUpdateCountSwallowsCacheFailures(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, likeMessage, 100, 7, 0, 0)
	st.cache.failWith("IncrLikeCount", errBoom)
	st.cache.failWith("IncrDislikeCount", errOther)

	got, err := NewUpdateCountLogic(context.Background(), newTestSvc(st)).
		UpdateCount(&rpc.UpdateCountReq{Business: likeBiz, OriginId: likeOrigin, MessageId: likeMessage, LikeChange: 5, DislikeChange: 6})
	wantNoErr(t, "缓存两跳都失败", err)
	wantEQ(t, "缓存两跳都失败", "响应非 nil", got != nil, true)
	wantOps(t, "缓存两跳都失败", st.log.opsFrom(0), []string{
		"stat.UpdateChange:archive:1:101:5/6",
		"cache.IncrLike:lc:archive:1:101:5",
		"cache.IncrDislike:dc:archive:1:101:6",
	})
	wantEQ(t, "缓存两跳都失败", "DB 修正位已落", st.stat.get(likeBiz, likeOrigin, likeMessage).LikeChange, int64(5))
	wantEQ(t, "缺陷 #5", "like 影子计数器未动", st.cache.likeCounter(likeBiz, likeOrigin, likeMessage), int64(0))
	wantEQ(t, "缺陷 #5", "dislike 影子计数器未动", st.cache.dislikeCounter(likeBiz, likeOrigin, likeMessage), int64(0))
}

// TestUpdateCountIgnoresOperator 缺陷 #13：请求里的 operator 从未被读取，
// 所以运营改数没有任何审计痕迹（谁改的、改了几次都查不到）。
func TestUpdateCountIgnoresOperator(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, likeMessage, 100, 7, 0, 0)

	_, err := NewUpdateCountLogic(context.Background(), newTestSvc(st)).
		UpdateCount(&rpc.UpdateCountReq{
			Business: likeBiz, OriginId: likeOrigin, MessageId: likeMessage,
			LikeChange: 5, Operator: "op-bob", Ip: "10.1.2.3",
		})
	wantNoErr(t, "带操作人", err)
	// 序列里不出现任何审计写入：只有 1 次 UPDATE + 2 次影子计数器。
	wantOps(t, "缺陷 #13", st.log.opsFrom(0), []string{
		"stat.UpdateChange:archive:1:101:5/0",
		"cache.IncrLike:lc:archive:1:101:5",
		"cache.IncrDislike:dc:archive:1:101:0",
	})
	row := st.stat.get(likeBiz, likeOrigin, likeMessage)
	wantEQ(t, "缺陷 #13", "行里没有操作人字段可承载", row.Mtime, int64(1_700_000_000))
}

// TestUpdateCountOnlyTouchesOwnDomain 越域检查：运营修正计数不得动点赞关系、
// 收藏夹、分享日志，也不得碰收藏相关的缓存 key。
func TestUpdateCountOnlyTouchesOwnDomain(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, likeMessage, 100, 7, 0, 0)
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, likeMessage, model.LikeStateLike, 1_700_000_500)
	seedFavItem(st, likeMid, likeMessage, 33, 2, 11, 0)
	seedShare(st, likeMessage, likeMid, 2, 20260901)
	likedBefore := st.like.get(likeBiz, likeMid, likeMessage)

	_, err := NewUpdateCountLogic(context.Background(), newTestSvc(st)).
		UpdateCount(&rpc.UpdateCountReq{Business: likeBiz, OriginId: likeOrigin, MessageId: likeMessage, LikeChange: 5})
	wantNoErr(t, "UpdateCount", err)

	wantCount(t, "越域检查", st.log, "like.Upsert", 0)
	wantCount(t, "越域检查", st.log, "stat.Incr:", 0)
	wantCount(t, "越域检查", st.log, "favItem.", 0)
	wantCount(t, "越域检查", st.log, "folder.", 0)
	wantCount(t, "越域检查", st.log, "share.", 0)
	wantCount(t, "越域检查", st.log, "cache.GetFolders", 0)
	wantCount(t, "越域检查", st.log, "cache.SetIsFavored", 0)
	after := st.like.get(likeBiz, likeMid, likeMessage)
	wantEQ(t, "越域检查", "点赞关系行原样", after.State, likedBefore.State)
	wantEQ(t, "越域检查", "点赞关系行 ctime 原样", after.Ctime, likedBefore.Ctime)
}
