package logic

// addblacklogic_test.go 覆盖 AddBlack 的 4 类断言：守卫、正常路径逐字段投影（黑名单行 + 自动取关的完整
// 十步轨迹）、下游失败传播、本域不变量（幂等不二次落库/不二次 -1、只删发起方向、软删恢复不刷 ctime、
// 拉黑与取关两写无事务、缓存写失败被吞后的脏读窗口）。

import (
	"context"
	"testing"
	"time"

	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"
)

const (
	abActor  = 2001
	abTarget = 3002
	// 两个方向的关注行 ctime 互不相同，「只删发起方向」才有辨识度。
	abBlackCtime = 1_655_000_100 // abActor → abTarget 的关注行
	abRevCtime   = 1_655_000_200 // abTarget → abActor 的反向关注行
)

// addBlackSeed 布「abActor 关注了 abTarget，且 abTarget 也关注了 abActor」这对双向关注，
// 外加双方计数快照与 Redis 关注集合：这样「只删发起方向」与「计数只扣一侧」都能逐字段核对。
func addBlackSeed(st *store) (*model.RelationFollow, *model.RelationFollow) {
	out := seedFollow(st, &model.RelationFollow{
		Mid: abActor, FollowerMid: abTarget, State: followNormal,
		Ctime: abBlackCtime, Mtime: abBlackCtime + 1,
	})
	rev := seedFollow(st, &model.RelationFollow{
		Mid: abTarget, FollowerMid: abActor, State: followNormal,
		Ctime: abRevCtime, Mtime: abRevCtime + 1,
	})
	seedStat(st, &model.RelationStat{Mid: abActor, Following: 7, Follower: 3, Ctime: 1_600_000_001, Mtime: 1_600_000_002})
	seedStat(st, &model.RelationStat{Mid: abTarget, Following: 11, Follower: 5, Ctime: 1_600_000_003, Mtime: 1_600_000_004})
	st.cache.warmCounts(abActor, 7, 3)
	st.cache.warmCounts(abTarget, 11, 5)
	st.cache.warmFollowing(abActor, abTarget)
	st.cache.warmFollowing(abTarget, abActor)
	return out, rev
}

func mustAddBlack(t *testing.T, l *AddBlackLogic, in *rpc.BlackReq) error {
	t.Helper()
	_, err := l.AddBlack(in)
	return err
}

func TestAddBlackGuards(t *testing.T) {
	cases := []struct {
		name       string
		mid, black int64
		want       error
	}{
		{"mid 为 0", 0, abTarget, model.ErrInvalidMid},
		{"mid 为负", -1, abTarget, model.ErrInvalidMid},
		{"black_mid 为 0", abActor, 0, model.ErrInvalidBlackMid},
		{"black_mid 为负", abActor, -9, model.ErrInvalidBlackMid},
		{"自己拉黑自己", abActor, abActor, model.ErrSelfAction},
		// 守卫顺序：mid 先于 black_mid，两者同为非法值时报 mid 错。
		{"两侧同为负数先报 mid", -5, -5, model.ErrInvalidMid},
		{"两侧同为 0 先报 mid", 0, 0, model.ErrInvalidMid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			addBlackSeed(e.st)
			l := NewAddBlackLogic(context.Background(), e.svcCtx)
			wantGuardRejected(t, e.st, c.name, c.want, func() error {
				return mustAddBlack(t, l, &rpc.BlackReq{Mid: c.mid, BlackMid: c.black, RealIp: "10.1.2.3"})
			})
		})
	}
}

// TestAddBlackFirstTimeWritesBlackRowAndUnfollowsOutgoingOnly 是本方法的核心轨迹：
// 先写黑名单，再复用 Unfollow 全链路（软删关注行 + 四个缓存增量 + 两条 DB 计数）。
func TestAddBlackFirstTimeWritesBlackRowAndUnfollowsOutgoingOnly(t *testing.T) {
	e := newEnv(t)
	st := e.st
	out, rev := addBlackSeed(st)
	now := time.Now().Unix()

	l := NewAddBlackLogic(context.Background(), e.svcCtx)
	got, err := l.AddBlack(&rpc.BlackReq{Mid: abActor, BlackMid: abTarget})
	wantNoErr(t, "首次拉黑", err)
	if got == nil {
		t.Fatalf("首次拉黑：响应 = nil, want 非空 EmptyReply")
	}

	wantOps(t, "首次拉黑链路", st.log.ops, []string{
		"black.Upsert:select:2001>3002",
		"black.Upsert:write:2001>3002/0",
		"follow.Delete:select:2001>3002",
		"follow.Delete:write:2001>3002",
		"cache.IncrFollowingCount:2001/-1",
		"cache.IncrFollowerCount:3002/-1",
		"cache.DelFollowing:2001>3002",
		"cache.DelFollower:3002>2001",
		"stat.Incr:update:2001/-1/+0",
		"stat.Incr:update:3002/+0/-1",
	})

	// 黑名单行逐字段：id 自增（601 起）、方向 mid→black_mid、state=0、ctime=mtime=当前。
	row := st.blacks.row(abActor, abTarget)
	if row == nil {
		t.Fatalf("首次拉黑：relation_black 未落库")
	}
	wantEQ(t, "黑名单行", "id", row.ID, int64(601))
	wantEQ(t, "黑名单行", "mid（拉黑发起方）", row.Mid, int64(abActor))
	wantEQ(t, "黑名单行", "black_mid（被拉黑者）", row.BlackMid, int64(abTarget))
	wantEQ(t, "黑名单行", "state", row.State, blackNormal)
	assertAround(t, "黑名单行", "ctime", row.Ctime, now, 5)
	assertAround(t, "黑名单行", "mtime", row.Mtime, now, 5)

	// 发起方向的关注行：软删而非物理删，id/ctime/attr 保留，只有 state+mtime 变。
	after := st.follows.row(abActor, abTarget)
	wantEQ(t, "被取关的行", "state", after.State, followGone)
	wantEQ(t, "被取关的行", "id 保留", after.ID, out.ID)
	wantEQ(t, "被取关的行", "ctime 保留", after.Ctime, out.Ctime)
	if after.Mtime == out.Mtime {
		t.Errorf("被取关的行：mtime = %d 未刷新（UPDATE 未生效？）", after.Mtime)
	}

	// TODO(缺陷)：Repository.AddBlack 的注释声明「仅删 mid→blackMid 的关注记录」，
	// 反向行保持原样是**代码事实**，这里钉住它。后果：被拉黑者的关注列表仍含拉黑者、
	// 其粉丝计数也不扣，而本服务没有任何 RPC 能把「谁拉黑了我」这一维过滤条件透给读侧。
	untouched := st.follows.row(abTarget, abActor)
	assertFollowRow(t, "反向关注行不得被动", untouched, rev)
	wantCount(t, "反向关注行不得被动", st.log, "follow.Delete:write:3002>2001", 0)

	// 计数：只扣「发起方的关注数」与「被拉黑者的粉丝数」，另两个方向一个字节都不许动。
	assertStatRow(t, "计数快照 2001", st.stats.row(abActor), 6, 3)
	assertStatRow(t, "计数快照 3002", st.stats.row(abTarget), 11, 4)
	wantEQ(t, "计数快照", "行数不新增", st.stats.countRows(), 2)
	fc, fcHit := st.cache.cachedFollowing(abActor)
	rc, rcHit := st.cache.cachedFollower(abTarget)
	wantEQ(t, "Redis 计数器", "2001.following 命中", fcHit, true)
	wantEQ(t, "Redis 计数器", "2001.following", fc, int64(6))
	wantEQ(t, "Redis 计数器", "3002.follower 命中", rcHit, true)
	wantEQ(t, "Redis 计数器", "3002.follower", rc, int64(4))
	revFc, _ := st.cache.cachedFollowing(abTarget)
	revRc, _ := st.cache.cachedFollower(abActor)
	wantEQ(t, "Redis 计数器反向污染", "3002.following", revFc, int64(11))
	wantEQ(t, "Redis 计数器反向污染", "2001.follower", revRc, int64(3))

	// 缓存清理方向必须是「2001 的关注集合里去掉 3002」+「3002 的粉丝时间轴里去掉 2001」。
	wantEQ(t, "关注集合", "2001 仍含 3002", st.cache.cachedIsFollowing(abActor, abTarget), false)
	wantEQ(t, "关注集合", "2001 的键被 SREM 搬空后消失", st.cache.followSetExists(abActor), false)
	wantEQ(t, "粉丝时间轴", "3002 仍含 2001", st.cache.zsetHas(abTarget, abActor), false)
	wantEQ(t, "关注集合反向污染", "3002 仍含 2001 于自己的集合", st.cache.cachedIsFollowing(abTarget, abActor), true)

	// 拉黑不碰特别关注表、不新增黑名单行、不清计数缓存键。
	wantEQ(t, "拉黑副作用", "黑名单行数", st.blacks.countRows(), 1)
	wantEQ(t, "拉黑副作用", "特别关注行数", st.specials.countRows(), 0)
	wantCount(t, "拉黑副作用", st.log, "special.", 0)
	wantCount(t, "拉黑副作用", st.log, "cache.Set", 0)
	wantCount(t, "拉黑副作用", st.log, "cache.Get", 0)
}

// TestRepeatAddBlackDoesNotRewriteBlackRowNorDoubleDecrement 对应迁移 SQL 的
// UNIQUE KEY uniq_mid_black (mid, black_mid)：第二次拉黑读到 oldState=0 == 目标 state，
// 一条写都不发；取关已无关注行，也就没有第二次 -1。
func TestRepeatAddBlackDoesNotRewriteBlackRowNorDoubleDecrement(t *testing.T) {
	e := newEnv(t)
	st := e.st
	addBlackSeed(st)
	l := NewAddBlackLogic(context.Background(), e.svcCtx)
	req := &rpc.BlackReq{Mid: abActor, BlackMid: abTarget}

	wantNoErr(t, "第一次拉黑", mustAddBlack(t, l, req))
	first := st.blacks.row(abActor, abTarget)
	before := st.log.snapshot()
	wantNoErr(t, "第二次拉黑", mustAddBlack(t, l, req))

	wantOps(t, "重复拉黑链路", st.log.opsFrom(before), []string{
		"black.Upsert:select:2001>3002",
		"follow.Delete:select:2001>3002",
	})
	wantCount(t, "重复拉黑", st.log, "black.Upsert:write", 1)
	wantCount(t, "重复拉黑", st.log, "follow.Delete:write", 1)
	wantCount(t, "重复拉黑", st.log, "stat.Incr", 2) // 两次增量只在第一次拉黑时发生
	wantCount(t, "重复拉黑", st.log, "cache.Incr", 2)
	second := st.blacks.row(abActor, abTarget)
	wantEQ(t, "重复拉黑后黑名单行", "id", second.ID, first.ID)
	wantEQ(t, "重复拉黑后黑名单行", "state", second.State, first.State)
	wantEQ(t, "重复拉黑后黑名单行", "ctime", second.Ctime, first.Ctime)
	wantEQ(t, "重复拉黑后黑名单行", "mtime 不刷新", second.Mtime, first.Mtime)
	assertStatRow(t, "重复拉黑后 2001", st.stats.row(abActor), 6, 3)
	assertStatRow(t, "重复拉黑后 3002", st.stats.row(abTarget), 11, 4)
}

// TestAddBlackRestoresSoftDeletedRowWithoutRefreshingCtime：取消拉黑后再拉黑走
// INSERT ... ON DUPLICATE KEY UPDATE state,mtime —— ctime 保持第一次拉黑的时间。
func TestAddBlackRestoresSoftDeletedRowWithoutRefreshingCtime(t *testing.T) {
	e := newEnv(t)
	st := e.st
	now := time.Now().Unix()
	src := seedBlack(st, &model.RelationBlack{
		ID: 555, Mid: abActor, BlackMid: abTarget, State: blackGone,
		Ctime: 1_600_000_555, Mtime: 1_600_000_556,
	})
	l := NewAddBlackLogic(context.Background(), e.svcCtx)

	wantNoErr(t, "恢复拉黑", mustAddBlack(t, l, &rpc.BlackReq{Mid: abActor, BlackMid: abTarget}))
	wantOps(t, "恢复拉黑链路", st.log.ops, []string{
		"black.Upsert:select:2001>3002",
		"black.Upsert:write:2001>3002/0",
		"follow.Delete:select:2001>3002", // 无关注行 ⇒ oldState=-1 ⇒ 取关幂等，一步都不多发
	})
	row := st.blacks.row(abActor, abTarget)
	wantEQ(t, "恢复拉黑后的行", "id 复用旧行", row.ID, src.ID)
	wantEQ(t, "恢复拉黑后的行", "state 回到 0", row.State, blackNormal)
	wantEQ(t, "恢复拉黑后的行", "ctime 不被 ON DUPLICATE 刷新", row.Ctime, src.Ctime)
	if row.Mtime == src.Mtime {
		t.Errorf("恢复拉黑后的行：mtime = %d 未刷新", row.Mtime)
	}
	assertAround(t, "恢复拉黑后的行", "mtime", row.Mtime, now, 5)
	// 没有关注行就不该有任何计数动作。
	wantCount(t, "恢复拉黑", st.log, "cache.", 0)
	wantCount(t, "恢复拉黑", st.log, "stat.", 0)
	wantEQ(t, "恢复拉黑", "无快照则不建 relation_stat 行", st.stats.countRows(), 0)
}

// TestAddBlackOnMissingFollowStillWritesOnlyBlackRow 说明「拉黑」与「取关」是两件事：
// 没关注过也能拉黑，且不会因此凭空建出关注行或计数行。
func TestAddBlackOnMissingFollowStillWritesOnlyBlackRow(t *testing.T) {
	e := newEnv(t)
	st := e.st
	// 双向都没关注，但布了第三方的计数快照，确保「不新增行」不是无行可查的假象。
	seedStat(st, &model.RelationStat{Mid: abActor, Following: 7, Follower: 3, Ctime: 1_600_000_001, Mtime: 1_600_000_002})
	l := NewAddBlackLogic(context.Background(), e.svcCtx)

	wantNoErr(t, "无关注拉黑", mustAddBlack(t, l, &rpc.BlackReq{Mid: abActor, BlackMid: abTarget}))
	wantOps(t, "无关注拉黑链路", st.log.ops, []string{
		"black.Upsert:select:2001>3002",
		"black.Upsert:write:2001>3002/0",
		"follow.Delete:select:2001>3002",
	})
	wantEQ(t, "无关注拉黑", "关注表行数", st.follows.countRows(), 0)
	assertStatRow(t, "无关注拉黑后 2001", st.stats.row(abActor), 7, 3)
}

// TestAddBlackDoesNotBlockReverseFollow 钉住黑名单的**单向**语义（与 Follow 侧的
// TestFollowIgnoresReverseBlacklist 成对）：2001 拉黑 3002 之后，3002 依然能关注 2001。
// TODO(缺陷)：Follow 只查「发起方有没有拉黑目标」，不查「目标有没有拉黑发起方」，
// 于是拉黑挡不住被拉黑者反向关注，黑名单的「看不见对方」承诺在读侧无法兑现。
func TestAddBlackDoesNotBlockReverseFollow(t *testing.T) {
	e := newEnv(t)
	st := e.st
	abL := NewAddBlackLogic(context.Background(), e.svcCtx)
	wantNoErr(t, "拉黑", mustAddBlack(t, abL, &rpc.BlackReq{Mid: abActor, BlackMid: abTarget}))

	before := st.log.snapshot()
	fL := NewFollowLogic(context.Background(), e.svcCtx)
	wantNoErr(t, "被拉黑者反向关注", mustFollow(t, fL, &rpc.FollowReq{Mid: abTarget, FollowerMid: abActor}))
	wantOps(t, "反向关注链路", st.log.opsFrom(before), []string{
		"black.FindOne:3002>2001", // 只查了 3002→2001 这个方向，没有反查 2001→3002
		"follow.Upsert:select:3002>2001",
		"follow.Upsert:write:3002>2001/0",
		"cache.IncrFollowingCount:3002/+1",
		"cache.IncrFollowerCount:2001/+1",
		"cache.AddFollowing:3002>2001",
		"cache.AddFollower:2001>3002",
		"stat.Incr:update:3002/+1/+0",
		"stat.Incr:insert:3002/+1/+0",
		"stat.Incr:update:2001/+0/+1",
		"stat.Incr:insert:2001/+0/+1",
	})
	wantEQ(t, "反向关注落库", "state", st.follows.stateOfFollow(abTarget, abActor), followNormal)
	wantEQ(t, "黑名单仍在", "state", st.blacks.stateOf(abActor, abTarget), blackNormal)
}

// TestAddBlackUnfollowFailureLeavesBlackRowCommitted 登记无事务两写：
// 黑名单已落库后取关失败，整个调用返错但**不回滚**，用户以为拉黑失败、其实已被拉黑。
// TODO(缺陷)
func TestAddBlackUnfollowFailureLeavesBlackRowCommitted(t *testing.T) {
	e := newEnv(t)
	st := e.st
	out, _ := addBlackSeed(st)
	st.follows.failWith("Delete", errStore)
	l := NewAddBlackLogic(context.Background(), e.svcCtx)

	err := mustAddBlack(t, l, &rpc.BlackReq{Mid: abActor, BlackMid: abTarget})
	wantErrIs(t, "拉黑后半途失败", err, errStore)
	wantErrMessage(t, "拉黑后半途失败", err, "relation_follow Delete: social-graph-test: store unavailable")
	wantOps(t, "拉黑后半途失败链路", st.log.ops, []string{
		"black.Upsert:select:2001>3002",
		"black.Upsert:write:2001>3002/0",
		"follow.Delete:select:2001>3002",
		"follow.Delete:write:2001>3002",
	})
	// 半状态：黑名单生效 + 关注依旧 + 计数一分未动。
	wantEQ(t, "拉黑后半途失败", "黑名单 state", st.blacks.stateOf(abActor, abTarget), blackNormal)
	wantEQ(t, "拉黑后半途失败", "关注 state 未软删", st.follows.stateOfFollow(abActor, abTarget), followNormal)
	assertFollowRow(t, "拉黑后半途失败的行未回滚", st.follows.row(abActor, abTarget), out)
	assertStatRow(t, "拉黑后半途失败 2001", st.stats.row(abActor), 7, 3)
	assertStatRow(t, "拉黑后半途失败 3002", st.stats.row(abTarget), 11, 5)
}

func TestPropagatesAddBlackFailures(t *testing.T) {
	cases := []struct {
		name    string
		inject  func(st *store)
		wantErr string
		want    []string
		// 每条用例额外说明黑名单行到底落没落
		wantBlackStored bool
	}{
		{
			name:            "读旧状态失败",
			inject:          func(st *store) { st.blacks.failWith("UpsertSelect", errStore) },
			wantErr:         "relation_black Upsert select: social-graph-test: store unavailable",
			want:            []string{"black.Upsert:select:2001>3002"},
			wantBlackStored: false,
		},
		{
			name:            "写黑名单失败",
			inject:          func(st *store) { st.blacks.failWith("Upsert", errStore) },
			wantErr:         "relation_black Upsert: social-graph-test: store unavailable",
			want:            []string{"black.Upsert:select:2001>3002", "black.Upsert:write:2001>3002/0"},
			wantBlackStored: false,
		},
		{
			name:            "取关读状态失败",
			inject:          func(st *store) { st.follows.failWith("DeleteSelect", errStore) },
			wantErr:         "relation_follow Delete select: social-graph-test: store unavailable",
			want:            []string{"black.Upsert:select:2001>3002", "black.Upsert:write:2001>3002/0", "follow.Delete:select:2001>3002"},
			wantBlackStored: true,
		},
		{
			name:            "计数增量失败",
			inject:          func(st *store) { st.stats.failWith("IncrUpdate", errStore) },
			wantErr:         "relation_stat Incr update: social-graph-test: store unavailable",
			wantBlackStored: true,
			want: []string{
				"black.Upsert:select:2001>3002",
				"black.Upsert:write:2001>3002/0",
				"follow.Delete:select:2001>3002",
				"follow.Delete:write:2001>3002",
				"cache.IncrFollowingCount:2001/-1",
				"cache.IncrFollowerCount:3002/-1",
				"cache.DelFollowing:2001>3002",
				"cache.DelFollower:3002>2001",
				"stat.Incr:update:2001/-1/+0",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			addBlackSeed(st)
			c.inject(st)
			l := NewAddBlackLogic(context.Background(), e.svcCtx)

			err := mustAddBlack(t, l, &rpc.BlackReq{Mid: abActor, BlackMid: abTarget})
			wantErrIs(t, c.name, err, errStore)
			wantErrMessage(t, c.name, err, c.wantErr)
			wantOps(t, c.name+"链路", st.log.ops, c.want)
			wantEQ(t, c.name, "黑名单行是否落库", st.blacks.row(abActor, abTarget) != nil, c.wantBlackStored)
			if got := st.blacks.stateOf(abActor, abTarget); (got == blackNormal) != c.wantBlackStored {
				t.Errorf("%s：黑名单 state = %d, want 落库=%v", c.name, got, c.wantBlackStored)
			}
		})
	}
}

// TestAddBlackSucceedsWhenCacheWritesFail 证明四个缓存写全部「失败即忽略」（_ = err），
// 业务只依赖 DB 计数兜底；轨迹里仍要看到调用发生过。
func TestAddBlackSucceedsWhenCacheWritesFail(t *testing.T) {
	for _, m := range []string{"IncrFollowingCount", "IncrFollowerCount", "DelFollowing", "DelFollower"} {
		t.Run(m, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			addBlackSeed(st)
			st.cache.failWith(m, errCache)
			l := NewAddBlackLogic(context.Background(), e.svcCtx)

			wantNoErr(t, "缓存 "+m+" 失败仍算成功", mustAddBlack(t, l, &rpc.BlackReq{Mid: abActor, BlackMid: abTarget}))
			wantCount(t, "缓存 "+m+" 失败", st.log, "cache."+m+":", 1)
			assertStatRow(t, "缓存失败后的 2001", st.stats.row(abActor), 6, 3)
			assertStatRow(t, "缓存失败后的 3002", st.stats.row(abTarget), 11, 4)
		})
	}
}

// TestAddBlackCacheDelFailureLeavesFollowingCacheAhead 是上一条的**后果**用例：
// SREM 被吞掉后关注集合里仍有对方，IsFollowing 走缓存命中直接答「还关注着」，
// 而库里早已 state=1。TODO(缺陷)：关注集合没有重建/失效通道（见 README）。
func TestAddBlackCacheDelFailureLeavesFollowingCacheAhead(t *testing.T) {
	e := newEnv(t)
	st := e.st
	addBlackSeed(st)
	st.cache.failWith("DelFollowing", errCache)
	abL := NewAddBlackLogic(context.Background(), e.svcCtx)
	wantNoErr(t, "拉黑（缓存清理失败）", mustAddBlack(t, abL, &rpc.BlackReq{Mid: abActor, BlackMid: abTarget}))

	wantEQ(t, "脏缓存", "库里已取关", st.follows.stateOfFollow(abActor, abTarget), followGone)
	wantEQ(t, "脏缓存", "缓存里仍在关注集合", st.cache.cachedIsFollowing(abActor, abTarget), true)

	before := st.log.snapshot()
	isL := NewIsFollowingLogic(context.Background(), e.svcCtx)
	ok, err := isL.IsFollowing(&rpc.RelationReq{Mid: abActor, Owner: abTarget})
	wantNoErr(t, "脏缓存下的查询", err)
	wantEQ(t, "脏缓存下的查询", "following", ok.GetFollowing(), true)
	wantOps(t, "脏缓存下的查询链路", st.log.opsFrom(before), []string{"cache.IsFollowing:2001>3002"})
	wantCount(t, "脏缓存下的查询", st.log, "follow.FindOne", 0) // 命中即短路，不会回库自愈
}
