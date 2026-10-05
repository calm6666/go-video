package logic

// followlogic_test.go 覆盖 Follow 的 4 类断言：守卫、正常路径逐字段投影、下游失败传播、
// 本域不变量（幂等不二次落库/不二次 +1、黑名单单向校验、重新关注不刷新 ctime、无事务两写）。

import (
	"context"
	"testing"
	"time"

	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"
)

// followSeed 布一对已存在的计数快照（2001 关注数 7、3002 粉丝数 5），
// 让首次关注的期望轨迹只有 9 条，且 delta 方向能被逐字段核对。
func followSeed(st *store) {
	seedStat(st, &model.RelationStat{Mid: 2001, Following: 7, Follower: 3, Ctime: 1_600_000_001, Mtime: 1_600_000_002})
	seedStat(st, &model.RelationStat{Mid: 3002, Following: 11, Follower: 5, Ctime: 1_600_000_003, Mtime: 1_600_000_004})
	st.cache.warmCounts(2001, 7, 3)
	st.cache.warmCounts(3002, 11, 5)
}

func TestFollowGuards(t *testing.T) {
	cases := []struct {
		name          string
		mid, follower int64
		want          error
	}{
		{"mid 为 0", 0, 3002, model.ErrInvalidMid},
		{"mid 为负", -1, 3002, model.ErrInvalidMid},
		{"follower_mid 为 0", 2001, 0, model.ErrInvalidFollowerMid},
		{"follower_mid 为负", 2001, -7, model.ErrInvalidFollowerMid},
		{"自己关注自己", 2001, 2001, model.ErrSelfAction},
		// 守卫顺序：mid 先于 follower_mid，两者同为负数时报 mid 错。
		{"两侧同为负数先报 mid", -5, -5, model.ErrInvalidMid},
		{"两侧同为 0 先报 mid", 0, 0, model.ErrInvalidMid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			l := NewFollowLogic(context.Background(), e.svcCtx)
			wantGuardRejected(t, e.st, c.name, c.want, func() error {
				_, err := l.Follow(&rpc.FollowReq{Mid: c.mid, FollowerMid: c.follower})
				return err
			})
		})
	}
}

func TestFollowFirstTimeWritesRelationAndBumpsBothCountsOnce(t *testing.T) {
	e := newEnv(t)
	st := e.st
	followSeed(st)
	now := time.Now().Unix()

	l := NewFollowLogic(context.Background(), e.svcCtx)
	got, err := l.Follow(&rpc.FollowReq{Mid: 2001, FollowerMid: 3002, RealIp: "10.1.2.3"})
	wantNoErr(t, "首次关注", err)
	if got == nil {
		t.Fatalf("首次关注：响应 = nil, want 非空 EmptyReply")
	}

	wantOps(t, "首次关注链路", st.log.ops, []string{
		"black.FindOne:2001>3002",
		"follow.Upsert:select:2001>3002",
		"follow.Upsert:write:2001>3002/0",
		"cache.IncrFollowingCount:2001/+1",
		"cache.IncrFollowerCount:3002/+1",
		"cache.AddFollowing:2001>3002",
		"cache.AddFollower:3002>2001",
		"stat.Incr:update:2001/+1/+0",
		"stat.Incr:update:3002/+0/+1",
	})

	// 关系行逐字段：id 自增（901 起）、方向 mid→follower_mid、state=0。
	row := st.follows.row(2001, 3002)
	if row == nil {
		t.Fatalf("首次关注：relation_follow 未落库")
	}
	wantEQ(t, "关注行", "id", row.ID, int64(901))
	wantEQ(t, "关注行", "mid（关注发起方）", row.Mid, int64(2001))
	wantEQ(t, "关注行", "follower_mid（被关注者）", row.FollowerMid, int64(3002))
	wantEQ(t, "关注行", "state", row.State, followNormal)
	wantEQ(t, "关注行", "attr", row.Attr, int32(0))
	assertAround(t, "关注行", "ctime", row.Ctime, now, 5)
	assertAround(t, "关注行", "mtime", row.Mtime, now, 5)
	wantEQ(t, "关注行", "总行数", st.follows.countRows(), 1)

	// 计数：mid 的关注数 +1、follower_mid 的粉丝数 +1，另两个方向的计数一个字节都不许动。
	assertStatRow(t, "计数快照 2001", st.stats.row(2001), 8, 3)
	assertStatRow(t, "计数快照 3002", st.stats.row(3002), 11, 6)
	cFc, fcHit := st.cache.cachedFollowing(2001)
	cRc, rcHit := st.cache.cachedFollower(3002)
	wantEQ(t, "Redis 计数器", "2001.following 命中", fcHit, true)
	wantEQ(t, "Redis 计数器", "2001.following", cFc, int64(8))
	wantEQ(t, "Redis 计数器", "3002.follower 命中", rcHit, true)
	wantEQ(t, "Redis 计数器", "3002.follower", cRc, int64(6))
	fc2, _ := st.cache.cachedFollowing(3002)
	rc2, _ := st.cache.cachedFollower(2001)
	wantEQ(t, "Redis 计数器反向污染", "3002.following", fc2, int64(11))
	wantEQ(t, "Redis 计数器反向污染", "2001.follower", rc2, int64(3))

	// 关注集合与粉丝时间轴（只写不读，本服务无读侧用例）：方向必须是「3002 的粉丝里有 2001」。
	wantEQ(t, "关注集合", "2001 含 3002", st.cache.cachedIsFollowing(2001, 3002), true)
	score, zhit := st.cache.followerZScore(3002, 2001)
	wantEQ(t, "粉丝时间轴", "3002 的键存在", zhit, true)
	assertAround(t, "粉丝时间轴", "score(2001)", score, now, 5)
	_, zWrong := st.cache.followerZScore(2001, 3002)
	wantEQ(t, "粉丝时间轴方向", "2001 的键不应出现 3002", zWrong, false)

	// 关注不碰黑名单表、不碰特别关注表、不删任何缓存。
	wantEQ(t, "关注副作用", "黑名单行数", st.blacks.countRows(), 0)
	wantEQ(t, "关注副作用", "特别关注行数", st.specials.countRows(), 0)
	wantCount(t, "关注副作用", st.log, "cache.DelFollowing", 0)
	wantCount(t, "关注副作用", st.log, "cache.DelFollower", 0)
}

func TestFollowRepeatDoesNotRewriteRowOrDoubleCount(t *testing.T) {
	e := newEnv(t)
	st := e.st
	followSeed(st)
	l := NewFollowLogic(context.Background(), e.svcCtx)
	req := &rpc.FollowReq{Mid: 2001, FollowerMid: 3002}

	wantNoErr(t, "第一次关注", mustFollow(t, l, req))
	first := st.follows.row(2001, 3002)
	before := st.log.snapshot()

	// 第二次：迁移 SQL 的 UNIQUE KEY (mid, follower_mid) 让 Upsert 读到 oldState=0，
	// 与目标 state 相同 ⇒ 一条 INSERT/UPDATE 都不发，计数与缓存增量全部跳过。
	wantNoErr(t, "重复关注", mustFollow(t, l, req))
	wantOps(t, "重复关注链路", st.log.opsFrom(before), []string{
		"black.FindOne:2001>3002",
		"follow.Upsert:select:2001>3002",
	})
	wantCount(t, "重复关注", st.log, "follow.Upsert:write", 1) // 整场只写了第一次那一条
	wantCount(t, "重复关注", st.log, "stat.Incr", 2)           // 两条 DB 计数增量只在第一次发生
	wantCount(t, "重复关注", st.log, "cache.Incr", 2)
	assertStatRow(t, "重复关注后 2001", st.stats.row(2001), 8, 3)
	assertStatRow(t, "重复关注后 3002", st.stats.row(3002), 11, 6)
	fc, _ := st.cache.cachedFollowing(2001)
	wantEQ(t, "重复关注后 Redis 关注数", "2001.following", fc, int64(8))

	// 行本身一字未改（含 mtime：ON DUPLICATE 不发，就不会有时间抖动）。
	second := st.follows.row(2001, 3002)
	assertFollowRow(t, "重复关注后行未变", second, first)
	wantEQ(t, "重复关注后行数", "行数", st.follows.countRows(), 1)
}

func TestFollowAfterUnfollowRestoresRowWithoutRefreshingCtime(t *testing.T) {
	e := newEnv(t)
	st := e.st
	followSeed(st)
	const oldCtime = 1_650_000_000
	gone := seedFollow(st, &model.RelationFollow{
		Mid: 2001, FollowerMid: 3002, State: followGone, Ctime: oldCtime, Mtime: 1_650_000_009,
	})
	l := NewFollowLogic(context.Background(), e.svcCtx)
	wantNoErr(t, "重新关注", mustFollow(t, l, &rpc.FollowReq{Mid: 2001, FollowerMid: 3002}))

	wantOps(t, "重新关注链路", st.log.ops, []string{
		"black.FindOne:2001>3002",
		"follow.Upsert:select:2001>3002",
		"follow.Upsert:write:2001>3002/0",
		"cache.IncrFollowingCount:2001/+1",
		"cache.IncrFollowerCount:3002/+1",
		"cache.AddFollowing:2001>3002",
		"cache.AddFollower:3002>2001",
		"stat.Incr:update:2001/+1/+0",
		"stat.Incr:update:3002/+0/+1",
	})
	row := st.follows.row(2001, 3002)
	wantEQ(t, "重新关注", "state", row.State, followNormal)
	// ON DUPLICATE KEY UPDATE 的列表只有 state 与 mtime ⇒ 主键与 ctime 都保留 ⇒
	// 「关注列表按 ctime DESC 排序」时，重新关注会排到老位置（见 listfollowinglogic_test.go 的口径断言）。
	wantEQ(t, "重新关注", "沿用同一行 id", row.ID, gone.ID)
	wantEQ(t, "重新关注", "ctime 不刷新", row.Ctime, oldCtime)
	assertAround(t, "重新关注", "mtime 刷新", row.Mtime, time.Now().Unix(), 5)
	wantEQ(t, "重新关注", "行数不翻倍", st.follows.countRows(), 1)
	assertStatRow(t, "重新关注后 2001", st.stats.row(2001), 8, 3)
}

func TestFollowRejectedWhenActorHasBlackedTarget(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedBlack(st, distinctBlack(0, 2001, 3002, 1_700_000_000))
	l := NewFollowLogic(context.Background(), e.svcCtx)

	before := st.log.snapshot()
	_, err := l.Follow(&rpc.FollowReq{Mid: 2001, FollowerMid: 3002})
	wantErrIs(t, "拉黑后关注", err, model.ErrBlackNeedCancelFollow)
	wantErrMessage(t, "拉黑后关注", err, "social-graph: black require cancel follow first")
	// 拒绝必须发生在任何写入之前：只读了黑名单。
	wantOps(t, "拉黑后关注链路", st.log.opsFrom(before), []string{"black.FindOne:2001>3002"})
	wantEQ(t, "拉黑后关注", "关注行数", st.follows.countRows(), 0)
	wantEQ(t, "拉黑后关注", "计数行数", st.stats.countRows(), 0)
	wantCount(t, "拉黑后关注", st.log, "cache.", 0)
}

func TestFollowAllowedWhenBlackRowIsSoftDeleted(t *testing.T) {
	e := newEnv(t)
	st := e.st
	followSeed(st)
	seedBlack(st, &model.RelationBlack{
		Mid: 2001, BlackMid: 3002, State: blackGone, Ctime: 1_700_000_000, Mtime: 1_700_000_005,
	})
	l := NewFollowLogic(context.Background(), e.svcCtx)
	wantNoErr(t, "取消拉黑后关注", mustFollow(t, l, &rpc.FollowReq{Mid: 2001, FollowerMid: 3002}))
	wantEQ(t, "取消拉黑后关注", "关注行数", st.follows.countRows(), 1)
	wantOps(t, "取消拉黑后关注首步", st.log.ops[:2], []string{
		"black.FindOne:2001>3002",
		"follow.Upsert:select:2001>3002",
	})
}

// TODO(缺陷): 只校验「关注发起方拉黑了被关注者」这一个方向。
// 被关注者把发起方拉黑时，关注照样成功，且实现连反向那一行都没查过（见断言）。
// 定位：internal/logic/followlogic.go 只调用 IsBlacked(mid, follower_mid)。
func TestFollowIgnoresReverseBlacklist(t *testing.T) {
	e := newEnv(t)
	st := e.st
	followSeed(st)
	// 3002（被关注者）已拉黑 2001（发起方）。
	seedBlack(st, distinctBlack(0, 3002, 2001, 1_700_000_000))
	l := NewFollowLogic(context.Background(), e.svcCtx)

	wantNoErr(t, "被对方拉黑仍能关注", mustFollow(t, l, &rpc.FollowReq{Mid: 2001, FollowerMid: 3002}))
	wantCount(t, "反向黑名单", st.log, "black.FindOne:3002>2001", 0)
	wantStringsEQ(t, "黑名单读取", "读的是哪一行", st.log.opsWith("black.FindOne"), []string{"black.FindOne:2001>3002"})
	wantEQ(t, "反向拉黑时关注仍落库", "关注行 state", st.follows.row(2001, 3002).State, followNormal)
}

func TestFollowPropagatesBlackLookupFailure(t *testing.T) {
	e := newEnv(t)
	st := e.st
	st.blacks.failWith("FindOne", errStore)
	l := NewFollowLogic(context.Background(), e.svcCtx)

	before := st.log.snapshot()
	_, err := l.Follow(&rpc.FollowReq{Mid: 2001, FollowerMid: 3002})
	wantErrIs(t, "黑名单读失败", err, errStore)
	wantErrMessage(t, "黑名单读失败", err, "relation_black FindOne: social-graph-test: store unavailable")
	wantNoCall(t, "黑名单读失败后不得有写入", st, before+1)
	wantEQ(t, "黑名单读失败", "关注行数", st.follows.countRows(), 0)
}

func TestPropagatesFollowUpsertFailures(t *testing.T) {
	cases := []struct {
		name    string
		failKey string
		wantMsg string
		// wantFollowOps 是失败前真正发出去的 relation_follow 语句：
		// Upsert 的 write 轨迹在**发语句之前**记录，所以「INSERT 失败」这一例里 write 会出现（试了但没成）。
		wantFollowOps []string
	}{
		{
			name:          "SELECT 旧状态失败",
			failKey:       "UpsertSelect",
			wantMsg:       "relation_follow Upsert select: social-graph-test: store unavailable",
			wantFollowOps: []string{"follow.Upsert:select:2001>3002"},
		},
		{
			name:          "INSERT 失败",
			failKey:       "Upsert",
			wantMsg:       "relation_follow Upsert: social-graph-test: store unavailable",
			wantFollowOps: []string{"follow.Upsert:select:2001>3002", "follow.Upsert:write:2001>3002/0"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			followSeed(st)
			st.follows.failWith(c.failKey, errStore)
			l := NewFollowLogic(context.Background(), e.svcCtx)

			_, err := l.Follow(&rpc.FollowReq{Mid: 2001, FollowerMid: 3002})
			wantErrIs(t, c.name, err, errStore)
			wantErrMessage(t, c.name, err, c.wantMsg)
			wantStringsEQ(t, c.name, "relation_follow 语句", st.log.opsWith("follow."), c.wantFollowOps)
			// 关系没落成 ⇒ 计数与缓存一律不得先动（不落半截数据）。
			wantCount(t, c.name, st.log, "stat.Incr", 0)
			wantCount(t, c.name, st.log, "cache.", 0)
			wantEQ(t, c.name, "关注行数", st.follows.countRows(), 0)
			assertStatRow(t, c.name+" 后 2001", st.stats.row(2001), 7, 3)
		})
	}
}

// TODO(缺陷): 关系行、Redis 计数器、relation_stat 两次增量是 4 次独立写入且无事务。
// 中间任一步失败都会留下「关系已成立但计数缺一半」的状态，而重试关注会被幂等分支跳过，
// 计数永远补不回来（仓库里也没有回刷 relation_stat 的定时任务）。
// 定位：internal/repository/repository.go 的 Repository.Follow。用例把这条半截状态钉成行为哨兵。
func TestFollowStatIncrementFailureLeavesHalfWrittenState(t *testing.T) {
	e := newEnv(t)
	st := e.st
	// 只给 2001 布快照，3002 走 INSERT 分支。
	seedStat(st, &model.RelationStat{Mid: 2001, Following: 7, Follower: 3, Ctime: 1_600_000_001, Mtime: 1_600_000_002})
	st.cache.warmCounts(2001, 7, 3)
	st.stats.failWith("IncrInsert", errStore)
	l := NewFollowLogic(context.Background(), e.svcCtx)

	_, err := l.Follow(&rpc.FollowReq{Mid: 2001, FollowerMid: 3002})
	wantErrIs(t, "粉丝计数落库失败", err, errStore)
	wantErrMessage(t, "粉丝计数落库失败", err, "relation_stat Incr insert: social-graph-test: store unavailable")
	wantOps(t, "失败前的完整链路", st.log.ops, []string{
		"black.FindOne:2001>3002",
		"follow.Upsert:select:2001>3002",
		"follow.Upsert:write:2001>3002/0",
		"cache.IncrFollowingCount:2001/+1",
		"cache.IncrFollowerCount:3002/+1",
		"cache.AddFollowing:2001>3002",
		"cache.AddFollower:3002>2001",
		"stat.Incr:update:2001/+1/+0",
		"stat.Incr:update:3002/+0/+1",
		"stat.Incr:insert:3002/+0/+1",
	})
	// 半截状态：关系已成立、发起方关注数已 +1、被关注者快照根本没建出来。
	wantEQ(t, "半截状态", "关注行 state", st.follows.row(2001, 3002).State, followNormal)
	assertStatRow(t, "半截状态 2001", st.stats.row(2001), 8, 3)
	if got := st.stats.row(3002); got != nil {
		t.Fatalf("半截状态：3002 的快照 = %+v, want 仍不存在", got)
	}

	// 重试：幂等分支直接跳过 ⇒ 客户端拿到过一次失败，关系却已生效，且计数缺口永久保留。
	st.stats.failWith("IncrInsert", nil)
	before := st.log.snapshot()
	wantNoErr(t, "重试关注", mustFollow(t, l, &rpc.FollowReq{Mid: 2001, FollowerMid: 3002}))
	wantStringsEQ(t, "重试关注", "重试只做了两件事", st.log.opsFrom(before), []string{
		"black.FindOne:2001>3002",
		"follow.Upsert:select:2001>3002",
	})
	if got := st.stats.row(3002); got != nil {
		t.Fatalf("重试关注后 3002 的快照 = %+v, want 仍不存在（缺陷：计数无法自愈）", got)
	}
}

func TestFollowReturnsErrorWhenStatUpdateFails(t *testing.T) {
	e := newEnv(t)
	st := e.st
	followSeed(st)
	st.stats.failWith("IncrUpdate", errStore)
	l := NewFollowLogic(context.Background(), e.svcCtx)

	_, err := l.Follow(&rpc.FollowReq{Mid: 2001, FollowerMid: 3002})
	wantErrIs(t, "计数 UPDATE 失败", err, errStore)
	wantErrMessage(t, "计数 UPDATE 失败", err, "relation_stat Incr update: social-graph-test: store unavailable")
	wantStringsEQ(t, "计数 UPDATE 失败", "stat 调用", st.log.opsWith("stat.Incr"), []string{"stat.Incr:update:2001/+1/+0"})
	assertStatRow(t, "失败后 2001", st.stats.row(2001), 7, 3)
	assertStatRow(t, "失败后 3002", st.stats.row(3002), 11, 5)
	// 关系行与 Redis 计数器已经落了 —— 与 DB 快照当场分叉（同上一条缺陷）。
	wantEQ(t, "失败后关系已成立", "关注行 state", st.follows.row(2001, 3002).State, followNormal)
	fc, _ := st.cache.cachedFollowing(2001)
	wantEQ(t, "失败后 Redis 关注数", "2001.following", fc, int64(8))
}

func TestFollowSucceedsWhenCacheWritesFail(t *testing.T) {
	cases := []struct {
		name    string
		failKey string
	}{
		{"关注数计数器写失败", "IncrFollowingCount"},
		{"粉丝数计数器写失败", "IncrFollowerCount"},
		{"关注集合 SADD 失败", "AddFollowing"},
		{"粉丝时间轴 ZADD 失败", "AddFollower"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			followSeed(st)
			st.cache.failWith(c.failKey, errCache)
			l := NewFollowLogic(context.Background(), e.svcCtx)

			// Repository 里这 4 处都是 `_ = err`：缓存不是权威源，失败必须被容忍，
			// 但 DB 快照增量（后两步）照样要跑完。
			wantNoErr(t, c.name, mustFollow(t, l, &rpc.FollowReq{Mid: 2001, FollowerMid: 3002}))
			wantCount(t, c.name, st.log, "stat.Incr:update", 2)
			assertStatRow(t, c.name+" 后 2001", st.stats.row(2001), 8, 3)
			assertStatRow(t, c.name+" 后 3002", st.stats.row(3002), 11, 6)
			wantEQ(t, c.name, "关注行已落库", st.follows.row(2001, 3002).State, followNormal)
		})
	}
}

func TestFollowCreatesStatRowsLazilyWhenSnapshotMissing(t *testing.T) {
	e := newEnv(t)
	st := e.st
	l := NewFollowLogic(context.Background(), e.svcCtx)
	wantNoErr(t, "无快照关注", mustFollow(t, l, &rpc.FollowReq{Mid: 2001, FollowerMid: 3002}))
	wantStringsEQ(t, "无快照关注", "stat 语句序列", st.log.opsWith("stat.Incr"), []string{
		"stat.Incr:update:2001/+1/+0",
		"stat.Incr:insert:2001/+1/+0",
		"stat.Incr:update:3002/+0/+1",
		"stat.Incr:insert:3002/+0/+1",
	})
	assertStatRow(t, "建出来的 2001", st.stats.row(2001), 1, 0)
	assertStatRow(t, "建出来的 3002", st.stats.row(3002), 0, 1)
	wantEQ(t, "建出来的快照数", "行数", st.stats.countRows(), 2)
}

// mustFollow 让「调用 + 只要错误」的样板短一点。
func mustFollow(t *testing.T, l *FollowLogic, in *rpc.FollowReq) error {
	t.Helper()
	_, err := l.Follow(in)
	return err
}

// assertStatRow 逐字段核对 relation_stat 的两个计数（nil 视为失败）。
func assertStatRow(t *testing.T, label string, got *model.RelationStat, following, follower int64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s：relation_stat 无行, want following=%d follower=%d", label, following, follower)
	}
	wantEQ(t, label, "following", got.Following, following)
	wantEQ(t, label, "follower", got.Follower, follower)
}
