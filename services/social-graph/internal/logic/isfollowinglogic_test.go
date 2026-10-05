package logic

// isfollowinglogic_test.go 覆盖 IsFollowing：守卫、缓存命中/读穿四条路径、下游失败传播，
// 以及「查询不查黑名单」「命中即短路」两条本域口径。

import (
	"context"
	"testing"

	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"
)

func TestIsFollowingGuards(t *testing.T) {
	cases := []struct {
		name       string
		mid, owner int64
		want       error
	}{
		{"mid 为 0", 0, 3002, model.ErrInvalidMid},
		{"mid 为负", -9, 3002, model.ErrInvalidMid},
		{"owner 为 0", 2001, 0, model.ErrInvalidOwnerMid},
		{"owner 为负", 2001, -9, model.ErrInvalidOwnerMid},
		{"两侧同为负数先报 mid", -3, -3, model.ErrInvalidMid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			l := NewIsFollowingLogic(context.Background(), e.svcCtx)
			wantGuardRejected(t, e.st, c.name, c.want, func() error {
				_, err := l.IsFollowing(&rpc.RelationReq{Mid: c.mid, Owner: c.owner})
				return err
			})
		})
	}
}

// TestIsFollowingHasNoSelfGuard 与 Follow/Unfollow/AddSpecial 不同，
// 读侧没有 mid==owner 的自相关守卫（关注表里根本没有自关行，读一次无副作用）。
// 用例把这条不对称钉住：改文案/加守卫时必须有意识地解绑。
func TestIsFollowingHasNoSelfGuard(t *testing.T) {
	e := newEnv(t)
	st := e.st
	l := NewIsFollowingLogic(context.Background(), e.svcCtx)

	got, err := l.IsFollowing(&rpc.RelationReq{Mid: 2001, Owner: 2001})
	wantNoErr(t, "自查询", err)
	wantEQ(t, "自查询", "following", got.GetFollowing(), false)
	wantOps(t, "自查询链路", st.log.ops, []string{
		"cache.IsFollowing:2001>2001",
		"follow.FindOne:2001>2001",
		"cache.DelFollowing:2001>2001",
	})
}

func TestIsFollowingCacheHitTrueSkipsDatabase(t *testing.T) {
	e := newEnv(t)
	st := e.st
	st.cache.warmFollowing(2001, 3002)
	l := NewIsFollowingLogic(context.Background(), e.svcCtx)

	got, err := l.IsFollowing(&rpc.RelationReq{Mid: 2001, Owner: 3002})
	wantNoErr(t, "缓存命中已关注", err)
	wantEQ(t, "缓存命中已关注", "following", got.GetFollowing(), true)
	wantOps(t, "缓存命中已关注链路", st.log.ops, []string{"cache.IsFollowing:2001>3002"})
	wantCount(t, "缓存命中已关注", st.log, "follow.", 0)
}

// TestIsFollowingCacheHitFalseSkipsDatabase 钉住「命中即短路」：
// 集合 key 存在但不含该成员时，**不回源**。所以 key 存在而内容不完整（缺一次 SADD、
// 部分重建）会稳定返回错误的 false，且本服务没有重建该 key 的路径（Cache.DelFollowSet 零调用方）。
func TestIsFollowingCacheHitFalseSkipsDatabase(t *testing.T) {
	e := newEnv(t)
	st := e.st
	st.cache.warmFollowing(2001, 3003) // 同一 mid 的集合里只有别人
	seedFollow(st, &model.RelationFollow{
		Mid: 2001, FollowerMid: 3002, State: followNormal, Ctime: 1_650_000_040, Mtime: 1_650_000_041,
	})
	l := NewIsFollowingLogic(context.Background(), e.svcCtx)

	got, err := l.IsFollowing(&rpc.RelationReq{Mid: 2001, Owner: 3002})
	wantNoErr(t, "缓存命中未关注", err)
	wantEQ(t, "缓存命中未关注", "following（库里明明是 0，被缓存掩盖）", got.GetFollowing(), false)
	wantOps(t, "缓存命中未关注链路", st.log.ops, []string{"cache.IsFollowing:2001>3002"})
	wantCount(t, "缓存命中未关注", st.log, "follow.FindOne", 0)
}

func TestIsFollowingCacheMissReadsThroughAndBackfills(t *testing.T) {
	cases := []struct {
		name       string
		seedState  int32
		seedRow    bool
		wantFollow bool
		backfillOp string
	}{
		{"库里有正常行", followNormal, true, true, "cache.AddFollowing:2001>3002"},
		{"库里只有软删行", followGone, true, false, "cache.DelFollowing:2001>3002"},
		{"库里没有这一行", 0, false, false, "cache.DelFollowing:2001>3002"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			if c.seedRow {
				seedFollow(st, &model.RelationFollow{
					Mid: 2001, FollowerMid: 3002, State: c.seedState,
					Ctime: 1_650_000_050, Mtime: 1_650_000_051,
				})
			}
			l := NewIsFollowingLogic(context.Background(), e.svcCtx)

			got, err := l.IsFollowing(&rpc.RelationReq{Mid: 2001, Owner: 3002})
			wantNoErr(t, c.name, err)
			wantEQ(t, c.name, "following", got.GetFollowing(), c.wantFollow)
			wantOps(t, c.name+"链路", st.log.ops, []string{
				"cache.IsFollowing:2001>3002",
				"follow.FindOne:2001>3002",
				c.backfillOp,
			})
		})
	}
}

// TestNegativeBackfillDoesNotCreateFollowSetKey 钉住 fake 与 Redis 的一条真实口径：
// 对不存在的 key 做 SREM 不会把 key 建出来，否则 miss 会被伪装成 hit=false。
func TestNegativeBackfillDoesNotCreateFollowSetKey(t *testing.T) {
	e := newEnv(t)
	st := e.st
	l := NewIsFollowingLogic(context.Background(), e.svcCtx)

	_, err := l.IsFollowing(&rpc.RelationReq{Mid: 2001, Owner: 3002})
	wantNoErr(t, "冷读未关注", err)
	wantEQ(t, "冷读未关注", "关注集合 key 未被 SREM 凭空建出", st.cache.followSetExists(2001), false)
}

func TestPropagatesIsFollowingFailures(t *testing.T) {
	t.Run("缓存读失败必须上抛且不回源", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		st.cache.failWith("IsFollowing", errCache)
		l := NewIsFollowingLogic(context.Background(), e.svcCtx)

		got, err := l.IsFollowing(&rpc.RelationReq{Mid: 2001, Owner: 3002})
		wantErrIs(t, "缓存读失败", err, errCache)
		// Repository 直接 return err，未包装 —— 文案漂移要能被发现。
		wantErrMessage(t, "缓存读失败", err, "social-graph-test: cache unavailable")
		wantOps(t, "缓存读失败链路", st.log.ops, []string{"cache.IsFollowing:2001>3002"})
		if got != nil {
			t.Fatalf("缓存读失败：响应 = %+v, want nil（不得伪成功）", got)
		}
	})
	t.Run("回源查库失败必须上抛且不回填", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		st.follows.failWith("FindOne", errStore)
		l := NewIsFollowingLogic(context.Background(), e.svcCtx)

		got, err := l.IsFollowing(&rpc.RelationReq{Mid: 2001, Owner: 3002})
		wantErrIs(t, "回源失败", err, errStore)
		wantErrMessage(t, "回源失败", err, "relation_follow FindOne: social-graph-test: store unavailable")
		wantOps(t, "回源失败链路", st.log.ops, []string{
			"cache.IsFollowing:2001>3002",
			"follow.FindOne:2001>3002",
		})
		wantCount(t, "回源失败", st.log, "cache.AddFollowing", 0)
		wantCount(t, "回源失败", st.log, "cache.DelFollowing", 0)
		if got != nil {
			t.Fatalf("回源失败：响应 = %+v, want nil", got)
		}
	})
	t.Run("回填失败被容忍", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		seedFollow(st, &model.RelationFollow{
			Mid: 2001, FollowerMid: 3002, State: followNormal, Ctime: 1_650_000_060, Mtime: 1_650_000_061,
		})
		st.cache.failWith("AddFollowing", errCache)
		l := NewIsFollowingLogic(context.Background(), e.svcCtx)

		got, err := l.IsFollowing(&rpc.RelationReq{Mid: 2001, Owner: 3002})
		wantNoErr(t, "回填失败", err)
		wantEQ(t, "回填失败", "following（答案仍来自库里）", got.GetFollowing(), true)
		wantOps(t, "回填失败链路", st.log.ops, []string{
			"cache.IsFollowing:2001>3002",
			"follow.FindOne:2001>3002",
			"cache.AddFollowing:2001>3002",
		})
	})
}

// TestIsFollowingIgnoresBlacklist 钉住读侧口径：被拉黑者查「我是否关注拉黑我的人」
// 时，黑名单根本不在判定链上（可见性过滤是调用方/feed 的责任）。
func TestIsFollowingIgnoresBlacklist(t *testing.T) {
	e := newEnv(t)
	st := e.st
	st.cache.warmFollowing(2001, 3002)
	seedBlack(st, distinctBlack(0, 3002, 2001, 1_700_000_000)) // 3002 拉黑了 2001
	l := NewIsFollowingLogic(context.Background(), e.svcCtx)

	got, err := l.IsFollowing(&rpc.RelationReq{Mid: 2001, Owner: 3002})
	wantNoErr(t, "被拉黑后的关注查询", err)
	wantEQ(t, "被拉黑后的关注查询", "following", got.GetFollowing(), true)
	wantCount(t, "被拉黑后的关注查询", st.log, "black.", 0)
}
