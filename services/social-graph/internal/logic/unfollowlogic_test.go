package logic

// unfollowlogic_test.go 覆盖 Unfollow 的 4 类断言：守卫、正常路径（软删保留行）、
// 下游失败传播、本域不变量（无记录幂等成功、重复取关不二次 -1、计数无事务、特别关注位残留）。

import (
	"context"
	"testing"
	"time"

	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"
)

const unfollowedCtime = 1_650_000_000

// unfollowSeed 布好「2001 关注了 3002」这一整组事实：关系行、双方计数快照、Redis 侧键。
func unfollowSeed(st *store) *model.RelationFollow {
	row := seedFollow(st, &model.RelationFollow{
		Mid: 2001, FollowerMid: 3002, State: followNormal,
		Ctime: unfollowedCtime, Mtime: unfollowedCtime + 1,
	})
	seedStat(st, &model.RelationStat{Mid: 2001, Following: 7, Follower: 3, Ctime: 1_600_000_001, Mtime: 1_600_000_002})
	seedStat(st, &model.RelationStat{Mid: 3002, Following: 11, Follower: 5, Ctime: 1_600_000_003, Mtime: 1_600_000_004})
	st.cache.warmCounts(2001, 7, 3)
	st.cache.warmCounts(3002, 11, 5)
	st.cache.warmFollowing(2001, 3002)
	return row
}

func TestUnfollowGuards(t *testing.T) {
	cases := []struct {
		name          string
		mid, follower int64
		want          error
	}{
		{"mid 为 0", 0, 3002, model.ErrInvalidMid},
		{"mid 为负", -1, 3002, model.ErrInvalidMid},
		{"follower_mid 为 0", 2001, 0, model.ErrInvalidFollowerMid},
		{"follower_mid 为负", 2001, -7, model.ErrInvalidFollowerMid},
		{"自己取关自己", 2001, 2001, model.ErrSelfAction},
		{"两侧同为负数先报 mid", -5, -5, model.ErrInvalidMid},
		{"两侧同为 0 先报 mid", 0, 0, model.ErrInvalidMid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			l := NewUnfollowLogic(context.Background(), e.svcCtx)
			wantGuardRejected(t, e.st, c.name, c.want, func() error {
				_, err := l.Unfollow(&rpc.UnfollowReq{Mid: c.mid, FollowerMid: c.follower})
				return err
			})
		})
	}
}

func TestUnfollowSoftDeletesRowAndDecrementsBothCountsOnce(t *testing.T) {
	e := newEnv(t)
	st := e.st
	src := unfollowSeed(st)
	l := NewUnfollowLogic(context.Background(), e.svcCtx)

	got, err := l.Unfollow(&rpc.UnfollowReq{Mid: 2001, FollowerMid: 3002, RealIp: "10.1.2.3"})
	wantNoErr(t, "取关", err)
	if got == nil {
		t.Fatalf("取关：响应 = nil, want 非空 EmptyReply")
	}
	wantOps(t, "取关链路", st.log.ops, []string{
		"follow.Delete:select:2001>3002",
		"follow.Delete:write:2001>3002",
		"cache.IncrFollowingCount:2001/-1",
		"cache.IncrFollowerCount:3002/-1",
		"cache.DelFollowing:2001>3002",
		"cache.DelFollower:3002>2001",
		"stat.Incr:update:2001/-1/+0",
		"stat.Incr:update:3002/+0/-1",
	})

	row := st.follows.row(2001, 3002)
	if row == nil {
		t.Fatalf("取关：行被物理删除了，迁移 SQL 要求软删保留历史")
	}
	// 软删：行保留（含 id/ctime/mid/follower_mid/attr 原值），只有 state 与 mtime 变。
	wantEQ(t, "取关后的行", "state", row.State, followGone)
	wantEQ(t, "取关后的行", "id", row.ID, src.ID)
	wantEQ(t, "取关后的行", "mid", row.Mid, src.Mid)
	wantEQ(t, "取关后的行", "follower_mid", row.FollowerMid, src.FollowerMid)
	wantEQ(t, "取关后的行", "attr", row.Attr, src.Attr)
	wantEQ(t, "取关后的行", "ctime", row.Ctime, unfollowedCtime)
	assertAround(t, "取关后的行", "mtime", row.Mtime, time.Now().Unix(), 5)
	wantEQ(t, "取关后的行", "行数", st.follows.countRows(), 1)

	assertStatRow(t, "取关后 2001", st.stats.row(2001), 6, 3)
	assertStatRow(t, "取关后 3002", st.stats.row(3002), 11, 4)
	f2001, _ := st.cache.cachedFollowing(2001)
	r3002, _ := st.cache.cachedFollower(3002)
	wantEQ(t, "Redis 计数器", "2001.following", f2001, int64(6))
	wantEQ(t, "Redis 计数器", "3002.follower", r3002, int64(4))
	r2001, r2001Hit := st.cache.cachedFollower(2001)
	f3002, f3002Hit := st.cache.cachedFollowing(3002)
	wantEQ(t, "Redis 计数器", "2001.follower 命中", r2001Hit, true)
	wantEQ(t, "Redis 计数器", "2001.follower 未被牵连", r2001, int64(3))
	wantEQ(t, "Redis 计数器", "3002.following 命中", f3002Hit, true)
	wantEQ(t, "Redis 计数器", "3002.following 未被牵连", f3002, int64(11))
	// SREM 把集合搬空 ⇒ Redis key 消失（下一次读是 miss，而不是 hit=false）。
	wantEQ(t, "关注集合", "2001 的 key 已随空集合消失", st.cache.followSetExists(2001), false)
	_, zHit := st.cache.followerZScore(3002, 2001)
	wantEQ(t, "粉丝时间轴", "3002 里已无 2001", zHit, false)
	// 取关不查黑名单、不查特别关注、不删黑名单行。
	wantCount(t, "取关副作用", st.log, "black.", 0)
	wantCount(t, "取关副作用", st.log, "special.", 0)
}

func TestUnfollowMissingRelationIsIdempotentSuccess(t *testing.T) {
	e := newEnv(t)
	st := e.st
	followSeedNotThere := seedFollow(st, &model.RelationFollow{
		Mid: 2001, FollowerMid: 3003, State: followNormal, Ctime: 1_650_000_020, Mtime: 1_650_000_021,
	})
	l := NewUnfollowLogic(context.Background(), e.svcCtx)

	got, err := l.Unfollow(&rpc.UnfollowReq{Mid: 2001, FollowerMid: 3002})
	// 代码事实：无记录时 model 返回 oldState=-1，Repository 走幂等分支返回 (false, nil)，
	// logic 忽略 changed 直接回 EmptyReply ⇒ **取关不存在的记录是成功，不是报错**。
	wantNoErr(t, "取关不存在的记录", err)
	if got == nil {
		t.Fatalf("取关不存在的记录：响应 = nil, want 非空 EmptyReply")
	}
	wantOps(t, "取关不存在的记录链路", st.log.ops, []string{"follow.Delete:select:2001>3002"})
	wantEQ(t, "取关不存在的记录", "无关行未被牵连", followSeedNotThere.State, followNormal)
	wantEQ(t, "取关不存在的记录", "行数不变", st.follows.countRows(), 1)
	wantCount(t, "取关不存在的记录", st.log, "stat.Incr", 0)
	wantCount(t, "取关不存在的记录", st.log, "cache.", 0)
	wantEQ(t, "取关不存在的记录", "未凭空建出计数快照", st.stats.countRows(), 0)
}

func TestRepeatUnfollowDoesNotDoubleDecrement(t *testing.T) {
	e := newEnv(t)
	st := e.st
	unfollowSeed(st)
	l := NewUnfollowLogic(context.Background(), e.svcCtx)
	req := &rpc.UnfollowReq{Mid: 2001, FollowerMid: 3002}

	wantNoErr(t, "第一次取关", mustUnfollow(t, l, req))
	before := st.log.snapshot()
	wantNoErr(t, "重复取关", mustUnfollow(t, l, req))
	wantOps(t, "重复取关链路", st.log.opsFrom(before), []string{"follow.Delete:select:2001>3002"})
	wantCount(t, "重复取关", st.log, "follow.Delete:write", 1)
	wantCount(t, "重复取关", st.log, "stat.Incr:update", 2) // 只有第一次的两条增量
	wantCount(t, "重复取关", st.log, "cache.Incr", 2)
	assertStatRow(t, "重复取关后 2001", st.stats.row(2001), 6, 3)
	assertStatRow(t, "重复取关后 3002", st.stats.row(3002), 11, 4)
	f, _ := st.cache.cachedFollowing(2001)
	wantEQ(t, "重复取关后 Redis 关注数", "2001.following", f, int64(6))
}

func TestPropagatesUnfollowDeleteFailures(t *testing.T) {
	cases := []struct {
		name       string
		failKey    string
		wantMsg    string
		wantRowOps []string
	}{
		{
			name:       "SELECT 旧状态失败",
			failKey:    "DeleteSelect",
			wantMsg:    "relation_follow Delete select: social-graph-test: store unavailable",
			wantRowOps: []string{"follow.Delete:select:2001>3002"},
		},
		{
			name:       "UPDATE 软删失败",
			failKey:    "Delete",
			wantMsg:    "relation_follow Delete: social-graph-test: store unavailable",
			wantRowOps: []string{"follow.Delete:select:2001>3002", "follow.Delete:write:2001>3002"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			unfollowSeed(st)
			st.follows.failWith(c.failKey, errStore)
			l := NewUnfollowLogic(context.Background(), e.svcCtx)

			_, err := l.Unfollow(&rpc.UnfollowReq{Mid: 2001, FollowerMid: 3002})
			wantErrIs(t, c.name, err, errStore)
			wantErrMessage(t, c.name, err, c.wantMsg)
			wantStringsEQ(t, c.name, "relation_follow 语句", st.log.opsWith("follow."), c.wantRowOps)
			// 关系行必须还在（取关没成），计数与缓存一格未动。
			wantEQ(t, c.name, "关注行仍是已关注", st.follows.row(2001, 3002).State, followNormal)
			wantCount(t, c.name, st.log, "stat.Incr", 0)
			wantCount(t, c.name, st.log, "cache.", 0)
			assertStatRow(t, c.name+" 后 2001", st.stats.row(2001), 7, 3)
		})
	}
}

// TODO(缺陷): 与 Follow 同源的无事务两写：软删已落库、Redis 计数器已 -1，
// 但 relation_stat 的增量失败并向上报错；重试取关又被幂等分支跳过 ⇒ 计数永久偏大。
// 定位：internal/repository/repository.go 的 Repository.Unfollow。
func TestUnfollowStatFailureLeavesRelationAndCacheAhead(t *testing.T) {
	e := newEnv(t)
	st := e.st
	unfollowSeed(st)
	st.stats.failWith("IncrUpdate", errStore)
	l := NewUnfollowLogic(context.Background(), e.svcCtx)

	_, err := l.Unfollow(&rpc.UnfollowReq{Mid: 2001, FollowerMid: 3002})
	wantErrIs(t, "计数 UPDATE 失败", err, errStore)
	wantErrMessage(t, "计数 UPDATE 失败", err, "relation_stat Incr update: social-graph-test: store unavailable")
	wantEQ(t, "半截状态", "关系行已软删", st.follows.row(2001, 3002).State, followGone)
	assertStatRow(t, "半截状态 2001", st.stats.row(2001), 7, 3)
	assertStatRow(t, "半截状态 3002", st.stats.row(3002), 11, 5)
	f, _ := st.cache.cachedFollowing(2001)
	wantEQ(t, "半截状态 Redis", "2001.following 已 -1", f, int64(6))

	// 重试：成功但什么都不补 —— 快照停在 7，而关系已经断了。
	st.stats.failWith("IncrUpdate", nil)
	before := st.log.snapshot()
	wantNoErr(t, "重试取关", mustUnfollow(t, l, &rpc.UnfollowReq{Mid: 2001, FollowerMid: 3002}))
	wantStringsEQ(t, "重试取关", "只读了一次旧状态", st.log.opsFrom(before), []string{"follow.Delete:select:2001>3002"})
	assertStatRow(t, "重试取关后 2001", st.stats.row(2001), 7, 3)
}

func TestUnfollowSucceedsWhenCacheWritesFail(t *testing.T) {
	cases := []struct {
		name    string
		failKey string
	}{
		{"关注数计数器 -1 失败", "IncrFollowingCount"},
		{"粉丝数计数器 -1 失败", "IncrFollowerCount"},
		{"关注集合 SREM 失败", "DelFollowing"},
		{"粉丝时间轴 ZREM 失败", "DelFollower"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			unfollowSeed(st)
			st.cache.failWith(c.failKey, errCache)
			l := NewUnfollowLogic(context.Background(), e.svcCtx)

			wantNoErr(t, c.name, mustUnfollow(t, l, &rpc.UnfollowReq{Mid: 2001, FollowerMid: 3002}))
			wantCount(t, c.name, st.log, "stat.Incr:update", 2)
			assertStatRow(t, c.name+" 后 2001", st.stats.row(2001), 6, 3)
			assertStatRow(t, c.name+" 后 3002", st.stats.row(3002), 11, 4)
			wantEQ(t, c.name, "关系行已软删", st.follows.row(2001, 3002).State, followGone)
		})
	}
}

// TestUnfollowCreatesZeroedSnapshotWhenMissing 钉住 model 的 max(delta,0) 口径：
// 缺快照的用户被取关时不会得到负计数，而是**凭空建出 0/0 行**。
func TestUnfollowCreatesZeroedSnapshotWhenMissing(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedFollow(st, &model.RelationFollow{
		Mid: 2001, FollowerMid: 3002, State: followNormal, Ctime: 1_650_000_030, Mtime: 1_650_000_031,
	})
	l := NewUnfollowLogic(context.Background(), e.svcCtx)

	wantNoErr(t, "无快照取关", mustUnfollow(t, l, &rpc.UnfollowReq{Mid: 2001, FollowerMid: 3002}))
	wantStringsEQ(t, "无快照取关", "stat 语句序列", st.log.opsWith("stat.Incr"), []string{
		"stat.Incr:update:2001/-1/+0",
		"stat.Incr:insert:2001/-1/+0",
		"stat.Incr:update:3002/+0/-1",
		"stat.Incr:insert:3002/+0/-1",
	})
	assertStatRow(t, "凭空建出的 2001", st.stats.row(2001), 0, 0)
	assertStatRow(t, "凭空建出的 3002", st.stats.row(3002), 0, 0)
	wantEQ(t, "凭空建出的快照数", "行数", st.stats.countRows(), 2)
}

// TODO(缺陷): 取关不清特别关注位 ⇒ relation_special 仍为 state=0，
// 与「特别关注必先关注」的约束（以及 proto 的说明）冲突，而本服务没有任何自愈路径
// （没有 ListSpecial/IsSpecial RPC，RelationSpecialModel.FindOne 零调用方）。
// 定位：internal/repository/repository.go 的 Repository.Unfollow 只碰 follow/stat/cache。
func TestUnfollowLeavesSpecialFlagOrphaned(t *testing.T) {
	e := newEnv(t)
	st := e.st
	unfollowSeed(st)
	seedSpecial(st, distinctSpecial(0, 2001, 3002, 1_660_000_000))
	l := NewUnfollowLogic(context.Background(), e.svcCtx)

	wantNoErr(t, "带着特别关注取关", mustUnfollow(t, l, &rpc.UnfollowReq{Mid: 2001, FollowerMid: 3002}))
	wantEQ(t, "取关后", "关注位已软删", st.follows.row(2001, 3002).State, followGone)
	wantEQ(t, "取关后", "特别关注位仍是正常（缺陷）", st.specials.stateOf(2001, 3002), specialNormal)
	wantCount(t, "取关后", st.log, "special.", 0)
}

func mustUnfollow(t *testing.T, l *UnfollowLogic, in *rpc.UnfollowReq) error {
	t.Helper()
	_, err := l.Unfollow(in)
	return err
}
