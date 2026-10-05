package logic

// checkplayable_test.go 覆盖 CheckPlayable 与 ExpireWindow 两个方法，
// 但真正的判定链在 repository.CheckPlayable（缓存 → DB → 时间 → 状态推进），
// 因此用例一律从 logic 入口进入、以替身轨迹与缓存内容作断言。
//
// 这里钉住的是「误播/误拒」两条代价最高的方向：
//  1. 不可播必须是**结论**（playable=false, err=nil），不是错误；DB 故障必须是错误，
//     不能被吞成 false（否则上游 catalog/playback 会把「下游挂了」当成「没版权」）。
//  2. 缓存 key 必须含 content_type 与 region：跨地区授权互不覆盖。
//  3. 只有「已过期」的窗口才允许被后台推进为 expired；未生效与已撤权都不得改状态。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/rights/model"
	"go-video/services/rights/rpc"
)

const (
	pgc = int32(model.ContentTypePGC)
	ugc = int32(model.ContentTypeUGC)
)

// === CheckPlayable：守卫 ===

func TestCheckPlayableGuardsTouchNothing(t *testing.T) {
	st := newStore()
	l := NewCheckPlayableLogic(context.Background(), newTestSvc(st))

	_, err := l.CheckPlayable(&rpc.CheckReq{ContentId: 0, ContentType: rpc.ContentType_CONTENT_TYPE_PGC, Region: "CN"})
	wantErrIs(t, "content_id=0", err, model.ErrInvalidContentID)
	wantNoCall(t, "content_id=0", st, 0)

	_, err = l.CheckPlayable(&rpc.CheckReq{ContentId: -1, Region: "CN"})
	wantErrIs(t, "content_id<0", err, model.ErrInvalidContentID)
	wantNoCall(t, "content_id<0", st, 0)

	_, err = l.CheckPlayable(&rpc.CheckReq{ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_PGC})
	wantErrIs(t, "region 为空", err, model.ErrInvalidRegion)
	wantNoCall(t, "region 为空", st, 0)
}

// === CheckPlayable：判定链 ===

func TestCheckPlayableActiveWindowReadsThroughAndBackfills(t *testing.T) {
	st := newStore()
	w := seedWindow(st, 5, pgc, "TW", nowPlus(-100), nowPlus(3600), model.WindowStateActive)
	l := NewCheckPlayableLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.CheckPlayable(&rpc.CheckReq{ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_PGC, Region: "TW"})
	wantNoErr(t, "有效窗口", err)
	wantOps(t, "有效窗口", st.log.opsFrom(before), []string{
		"cache.Get:rights:chk:5:1:TW",
		"win.FindActive:5/1/TW",
		"cache.Set:rights:chk:5:1:TW",
	})
	wantEQ(t, "有效窗口", "playable", reply.GetPlayable(), true)
	wantEQ(t, "有效窗口", "window_id", reply.GetWindowId(), w.WindowID)
	wantEQ(t, "有效窗口", "end_time 透传给调用方做 TTL 上限", reply.GetEndTime(), w.EndTime)

	// 回填的必须是正缓存，且带上命中的窗口与结束时间。
	got, ok := st.cache.entry(5, pgc, "TW")
	wantEQ(t, "有效窗口", "回填命中", ok, true)
	wantEQ(t, "有效窗口", "回填 playable", got.playable, true)
	wantEQ(t, "有效窗口", "回填 window_id", got.windowID, w.WindowID)
	wantEQ(t, "有效窗口", "回填 end_time", got.endTime, w.EndTime)
}

func TestCheckPlayableCacheHitNeverTouchesDB(t *testing.T) {
	st := newStore()
	// 库里放一个**互相对应不上**的窗口：如果命中缓存还去查库，下面的值就会露馅。
	seedWindow(st, 5, pgc, "TW", nowPlus(-100), nowPlus(9999), model.WindowStateActive)
	st.cache.data[keyCheck(5, pgc, "TW")] = chkEntry{playable: true, windowID: 4242, endTime: 1700000999}
	l := NewCheckPlayableLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.CheckPlayable(&rpc.CheckReq{ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_PGC, Region: "TW"})
	wantNoErr(t, "命中缓存", err)
	wantOps(t, "命中缓存", st.log.opsFrom(before), []string{"cache.Get:rights:chk:5:1:TW"})
	wantEQ(t, "命中缓存", "playable 取自缓存", reply.GetPlayable(), true)
	wantEQ(t, "命中缓存", "window_id 取自缓存", reply.GetWindowId(), int64(4242))
	wantEQ(t, "命中缓存", "end_time 取自缓存", reply.GetEndTime(), int64(1700000999))
	wantEQ(t, "命中缓存", "不回填", st.log.countPrefix("cache.Set"), 0)
}

func TestCheckPlayableNegativeCacheHitAnswersNotPlayable(t *testing.T) {
	st := newStore()
	seedWindow(st, 5, pgc, "TW", nowPlus(-100), nowPlus(3600), model.WindowStateActive)
	st.cache.data[keyCheck(5, pgc, "TW")] = chkEntry{} // 负缓存：不可播
	l := NewCheckPlayableLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.CheckPlayable(&rpc.CheckReq{ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_PGC, Region: "TW"})
	wantNoErr(t, "负缓存", err)
	wantOps(t, "负缓存", st.log.opsFrom(before), []string{"cache.Get:rights:chk:5:1:TW"})
	wantEQ(t, "负缓存", "playable", reply.GetPlayable(), false)
	// 库里其实有有效窗口：命中负缓存时**不会**去查库，所以这个 false 是缓存给的结论。
	// 这正是短 TTL（默认 120s）存在的理由——TTL 到期后才会重新落库看到那条窗口。
	wantEQ(t, "负缓存", "未查库", st.log.countPrefix("win.FindActive"), 0)
}

func TestCheckPlayableNoWindowIsAnswerNotError(t *testing.T) {
	st := newStore()
	l := NewCheckPlayableLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.CheckPlayable(&rpc.CheckReq{ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_PGC, Region: "TW"})
	wantNoErr(t, "无窗口", err) // 关键：不可播是结论，不是错误
	wantOps(t, "无窗口", st.log.opsFrom(before), []string{
		"cache.Get:rights:chk:5:1:TW",
		"win.FindActive:5/1/TW",
		"cache.Set:rights:chk:5:1:TW",
	})
	wantEQ(t, "无窗口", "playable", reply.GetPlayable(), false)
	wantEQ(t, "无窗口", "window_id 为 0", reply.GetWindowId(), int64(0))
	wantEQ(t, "无窗口", "end_time 为 0", reply.GetEndTime(), int64(0))
	got, ok := st.cache.entry(5, pgc, "TW")
	wantEQ(t, "无窗口", "落了负缓存", ok, true)
	wantEQ(t, "无窗口", "负缓存 playable", got.playable, false)
}

func TestCheckPlayableNotStartedYetIsNotAdvancedToExpired(t *testing.T) {
	st := newStore()
	w := seedWindow(st, 5, pgc, "TW", nowPlus(3600), nowPlus(7200), model.WindowStateActive)
	l := NewCheckPlayableLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.CheckPlayable(&rpc.CheckReq{ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_PGC, Region: "TW"})
	wantNoErr(t, "未生效窗口", err)
	wantEQ(t, "未生效窗口", "playable", reply.GetPlayable(), false)
	wantOps(t, "未生效窗口", st.log.opsFrom(before), []string{
		"cache.Get:rights:chk:5:1:TW",
		"win.FindActive:5/1/TW",
		"cache.Set:rights:chk:5:1:TW",
	})
	// 未生效 ≠ 已过期：把还没开始的窗口推成 expired 会让整纸授权提前失效。
	wantEQ(t, "未生效窗口", "不推进状态", st.log.countPrefix("win.UpdateState"), 0)
	wantEQ(t, "未生效窗口", "库存仍为 active", st.window.state(w.WindowID), int32(model.WindowStateActive))
}

func TestCheckPlayableExpiredWindowAdvancesAndInvalidates(t *testing.T) {
	st := newStore()
	w := seedWindow(st, 5, pgc, "TW", nowPlus(-7200), nowPlus(-1), model.WindowStateActive)
	l := NewCheckPlayableLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.CheckPlayable(&rpc.CheckReq{ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_PGC, Region: "TW"})
	wantNoErr(t, "已过期窗口", err)
	wantEQ(t, "已过期窗口", "playable", reply.GetPlayable(), false)
	wantOps(t, "已过期窗口", st.log.opsFrom(before), []string{
		"cache.Get:rights:chk:5:1:TW",
		"win.FindActive:5/1/TW",
		"cache.Set:rights:chk:5:1:TW", // 先落负缓存
		"win.UpdateState:1->2",        // 再推进为 expired
		"cache.Del:rights:chk:5:1:TW", // 失效刚写的缓存，避免下次命中旧值
	})
	wantEQ(t, "已过期窗口", "库存推进为 expired", st.window.state(w.WindowID), int32(model.WindowStateExpired))
	_, stillCached := st.cache.entry(5, pgc, "TW")
	wantEQ(t, "已过期窗口", "缓存已清", stillCached, false)
}

func TestCheckPlayableRevokedWindowStaysRevoked(t *testing.T) {
	st := newStore()
	w := seedWindow(st, 5, pgc, "TW", nowPlus(-7200), nowPlus(3600), model.WindowStateRevoked)
	l := NewCheckPlayableLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.CheckPlayable(&rpc.CheckReq{ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_PGC, Region: "TW"})
	wantNoErr(t, "已撤权窗口", err)
	wantEQ(t, "已撤权窗口", "playable", reply.GetPlayable(), false)
	wantOps(t, "已撤权窗口", st.log.opsFrom(before), []string{
		"cache.Get:rights:chk:5:1:TW",
		"win.FindActive:5/1/TW", // 只查 active：撤权的窗口根本不该被捞出来
		"cache.Set:rights:chk:5:1:TW",
	})
	// 撤权是终局判定，不能被过期推进改写状态（否则审计上分不清「到期」与「撤回」）。
	wantEQ(t, "已撤权窗口", "不推进状态", st.log.countPrefix("win.UpdateState"), 0)
	wantEQ(t, "已撤权窗口", "库存仍为 revoked", st.window.state(w.WindowID), int32(model.WindowStateRevoked))
}

func TestCheckPlayableDBErrorPropagatesNotAnsweredAsDenied(t *testing.T) {
	st := newStore()
	down := errors.New("too many connections")
	st.window.failWith("FindActiveByContentRegion", down)
	l := NewCheckPlayableLogic(context.Background(), newTestSvc(st))

	_, err := l.CheckPlayable(&rpc.CheckReq{ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_PGC, Region: "TW"})
	wantErrIs(t, "窗口查询失败", err, down)
	if errors.Is(err, model.ErrWindowNotFound) {
		t.Errorf("DB 故障被伪装成「没有窗口」：%v", err)
	}
	wantEQ(t, "窗口查询失败", "不写缓存（结论未知就别缓存）", st.log.countPrefix("cache.Set"), 0)
}

func TestCheckPlayableCacheReadFailureDegradesToDB(t *testing.T) {
	st := newStore()
	st.cache.failGet = errors.New("redis down")
	w := seedWindow(st, 5, pgc, "TW", nowPlus(-100), nowPlus(3600), model.WindowStateActive)
	l := NewCheckPlayableLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.CheckPlayable(&rpc.CheckReq{ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_PGC, Region: "TW"})
	wantNoErr(t, "缓存故障降级", err)
	wantOps(t, "缓存故障降级", st.log.opsFrom(before), []string{
		"cache.Get:rights:chk:5:1:TW",
		"win.FindActive:5/1/TW",
		"cache.Set:rights:chk:5:1:TW",
	})
	// 口径（与 catalog 相反）：rights 的读侧允许 Redis 故障时落库答「不可播」，
	// 因为这里读的是自己的库，结论仍然可信；写侧（catalog 上架）才要求不可用即拒绝。
	wantEQ(t, "缓存故障降级", "仍按库判定", reply.GetPlayable(), true)
	wantEQ(t, "缓存故障降级", "window_id", reply.GetWindowId(), w.WindowID)
}

func TestCheckPlayableRegionAndContentTypeAreOwnCacheKeys(t *testing.T) {
	st := newStore()
	// 只给 TW/PGC 授权：CN 与 TW/UGC 都必须答不可播，且不得覆写 TW/PGC 的正缓存。
	seedWindow(st, 5, pgc, "TW", nowPlus(-100), nowPlus(3600), model.WindowStateActive)
	l := NewCheckPlayableLogic(context.Background(), newTestSvc(st))

	tw, err := l.CheckPlayable(&rpc.CheckReq{ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_PGC, Region: "TW"})
	wantNoErr(t, "TW/PGC", err)
	wantEQ(t, "TW/PGC", "playable", tw.GetPlayable(), true)

	cn, err := l.CheckPlayable(&rpc.CheckReq{ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_PGC, Region: "CN"})
	wantNoErr(t, "CN/PGC", err)
	wantEQ(t, "CN/PGC", "playable", cn.GetPlayable(), false)

	ug, err := l.CheckPlayable(&rpc.CheckReq{ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_UGC, Region: "TW"})
	wantNoErr(t, "TW/UGC", err)
	wantEQ(t, "TW/UGC", "playable", ug.GetPlayable(), false)

	// 三个维度各自一条缓存；后两次的负缓存没有把第一次的正缓存冲掉。
	wantEQ(t, "三条独立缓存", "TW/PGC 仍为可播", st.cache.data[keyCheck(5, pgc, "TW")].playable, true)
	wantEQ(t, "三条独立缓存", "CN/PGC 为不可播", st.cache.data[keyCheck(5, pgc, "CN")].playable, false)
	wantEQ(t, "三条独立缓存", "TW/UGC 为不可播", st.cache.data[keyCheck(5, ugc, "TW")].playable, false)
	wantEQ(t, "三条独立缓存", "三次各自查库", st.log.countPrefix("win.FindActive"), 3)

	// 再来一次 TW/PGC：应当命中正缓存，不再查库（证明 key 没被后面的写覆盖）。
	before := st.log.snapshot()
	again, err := l.CheckPlayable(&rpc.CheckReq{ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_PGC, Region: "TW"})
	wantNoErr(t, "再次 TW/PGC", err)
	wantEQ(t, "再次 TW/PGC", "playable", again.GetPlayable(), true)
	wantOps(t, "再次 TW/PGC", st.log.opsFrom(before), []string{"cache.Get:rights:chk:5:1:TW"})
}

// === ExpireWindow ===

func TestExpireWindowStatePreconditions(t *testing.T) {
	cases := []struct {
		label     string
		state     int32
		windowID  int64
		wantErr   error
		seedState bool
	}{
		{"窗口不存在", 0, 404, model.ErrWindowNotFound, false},
		{"重复过期", model.WindowStateExpired, 0, model.ErrWindowExpired, true},
		{"已撤权不可再过期", model.WindowStateRevoked, 0, model.ErrWindowNotActive, true},
	}
	for _, c := range cases {
		st := newStore()
		l := NewExpireWindowLogic(context.Background(), newTestSvc(st))
		id := c.windowID
		if c.seedState {
			id = seedWindow(st, 5, pgc, "TW", nowPlus(-7200), nowPlus(3600), c.state).WindowID
		}
		before := st.log.snapshot()

		_, err := l.ExpireWindow(&rpc.WindowReq{WindowId: id})
		wantErrIs(t, c.label, err, c.wantErr)
		wantOps(t, c.label, st.log.opsFrom(before), []string{"win.FindOne:" + itoa(id)})
		wantEQ(t, c.label, "不改状态", st.log.countPrefix("win.UpdateState"), 0)
		wantEQ(t, c.label, "不动缓存", st.log.countPrefix("cache.Del"), 0)
	}
}

func TestExpireWindowGuardRejectsBeforeStore(t *testing.T) {
	st := newStore()
	l := NewExpireWindowLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	_, err := l.ExpireWindow(&rpc.WindowReq{WindowId: 0})
	wantErrIs(t, "window_id=0", err, model.ErrInvalidWindowID)
	wantNoCall(t, "window_id=0", st, before)
}

func TestExpireWindowSuccessAdvancesAndInvalidatesCache(t *testing.T) {
	st := newStore()
	w := seedWindow(st, 5, pgc, "TW", nowPlus(-7200), nowPlus(3600), model.WindowStateActive)
	// 先放一条正缓存：撤权后如果没失效，客户端在 TTL 内仍能拿到可播结论。
	st.cache.data[keyCheck(5, pgc, "TW")] = chkEntry{playable: true, windowID: w.WindowID, endTime: w.EndTime}
	l := NewExpireWindowLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.ExpireWindow(&rpc.WindowReq{WindowId: w.WindowID})
	wantNoErr(t, "手动过期", err)
	wantOps(t, "手动过期", st.log.opsFrom(before), []string{
		"win.FindOne:" + itoa(w.WindowID),
		"win.UpdateState:" + itoa(w.WindowID) + "->2",
		"cache.Del:rights:chk:5:1:TW",
	})
	wantEQ(t, "手动过期", "库存状态", st.window.state(w.WindowID), int32(model.WindowStateExpired))
	wantEQ(t, "手动过期", "回复状态", reply.GetWindow().GetState(), rpc.WindowState_WINDOW_STATE_EXPIRED)
	wantEQ(t, "手动过期", "回复 window_id", reply.GetWindow().GetWindowId(), w.WindowID)
	wantEQ(t, "手动过期", "回复 region 投影", reply.GetWindow().GetRegion(), "TW")
	wantEQ(t, "手动过期", "回复 content_type 投影", reply.GetWindow().GetContentType(), rpc.ContentType_CONTENT_TYPE_PGC)
	_, cached := st.cache.entry(5, pgc, "TW")
	wantEQ(t, "手动过期", "缓存已失效", cached, false)
}

func TestExpireWindowUpdateFailureKeepsCacheAndState(t *testing.T) {
	st := newStore()
	w := seedWindow(st, 5, pgc, "TW", nowPlus(-7200), nowPlus(3600), model.WindowStateActive)
	st.cache.data[keyCheck(5, pgc, "TW")] = chkEntry{playable: true, windowID: w.WindowID, endTime: w.EndTime}
	boom := errors.New("deadlock")
	st.window.failWith("UpdateState", boom)
	l := NewExpireWindowLogic(context.Background(), newTestSvc(st))

	_, err := l.ExpireWindow(&rpc.WindowReq{WindowId: w.WindowID})
	wantErrIs(t, "状态写入失败", err, boom)
	wantEQ(t, "状态写入失败", "库存仍 active", st.window.state(w.WindowID), int32(model.WindowStateActive))
	wantEQ(t, "状态写入失败", "不失效缓存", st.log.countPrefix("cache.Del"), 0)
	_, cached := st.cache.entry(5, pgc, "TW")
	wantEQ(t, "状态写入失败", "旧缓存仍在（下次仍判可播，直到 TTL 到期）", cached, true)
}

func TestExpireWindowFindErrorPropagates(t *testing.T) {
	st := newStore()
	down := errors.New("read timeout")
	st.window.failWith("FindOne", down)
	l := NewExpireWindowLogic(context.Background(), newTestSvc(st))

	_, err := l.ExpireWindow(&rpc.WindowReq{WindowId: 7})
	wantErrIs(t, "窗口读失败", err, down)
	wantEQ(t, "窗口读失败", "不写状态", st.log.countPrefix("win.UpdateState"), 0)
}
