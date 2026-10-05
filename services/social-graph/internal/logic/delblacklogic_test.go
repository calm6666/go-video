package logic

// delblacklogic_test.go 覆盖 DelBlack 的 4 类断言：守卫（无自发保护）、正常路径逐字段投影、
// 下游失败传播、本域不变量（三种「无行/已取消」幂等口径、取消拉黑绝不复活关注、
// 与 Follow 的「先取消拉黑才能关注」配对）。

import (
	"context"
	"testing"
	"time"

	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"
)

// seededBlackTime 是布进 relation_black 的原始时间戳；mtime 必须被刷新、ctime 不许动。
const (
	delBlackCtime = 1_600_000_777
	delBlackMtime = 1_600_000_778
)

func delBlackSeed(st *store, state int32) *model.RelationBlack {
	return seedBlack(st, &model.RelationBlack{
		ID: 555, Mid: 2001, BlackMid: 3002, State: state,
		Ctime: delBlackCtime, Mtime: delBlackMtime,
	})
}

func mustDelBlack(t *testing.T, l *DelBlackLogic, in *rpc.BlackReq) error {
	t.Helper()
	_, err := l.DelBlack(in)
	return err
}

func TestDelBlackGuards(t *testing.T) {
	cases := []struct {
		name       string
		mid, black int64
		want       error
	}{
		{"mid 为 0", 0, 3002, model.ErrInvalidMid},
		{"mid 为负", -1, 3002, model.ErrInvalidMid},
		{"black_mid 为 0", 2001, 0, model.ErrInvalidBlackMid},
		{"black_mid 为负", 2001, -9, model.ErrInvalidBlackMid},
		{"两侧同为负数先报 mid", -5, -5, model.ErrInvalidMid},
		{"两侧同为 0 先报 mid", 0, 0, model.ErrInvalidMid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			delBlackSeed(e.st, blackNormal)
			l := NewDelBlackLogic(context.Background(), e.svcCtx)
			wantGuardRejected(t, e.st, c.name, c.want, func() error {
				return mustDelBlack(t, l, &rpc.BlackReq{Mid: c.mid, BlackMid: c.black, RealIp: "10.1.2.3"})
			})
		})
	}
}

// TestDelBlackHasNoSelfGuard 钉住代码事实：AddBlack 有 ErrSelfAction，DelBlack 没有。
// 于是「取消自己拉黑自己」会照常打到 SQL 上（布一行 id=555 的自发黑名单即被软删）。
func TestDelBlackHasNoSelfGuard(t *testing.T) {
	e := newEnv(t)
	st := e.st
	src := seedBlack(st, &model.RelationBlack{
		ID: 555, Mid: 2001, BlackMid: 2001, State: blackNormal, Ctime: delBlackCtime, Mtime: delBlackMtime,
	})
	l := NewDelBlackLogic(context.Background(), e.svcCtx)

	wantNoErr(t, "取消自发拉黑", mustDelBlack(t, l, &rpc.BlackReq{Mid: 2001, BlackMid: 2001}))
	wantOps(t, "取消自发拉黑链路", st.log.ops, []string{
		"black.Delete:select:2001>2001",
		"black.Delete:write:2001>2001",
	})
	wantEQ(t, "取消自发拉黑", "state", st.blacks.stateOf(2001, 2001), blackGone)
	wantEQ(t, "取消自发拉黑", "id 复用旧行", st.blacks.row(2001, 2001).ID, src.ID)
	// AddBlack 侧的对照：同一对参数被守卫挡下，一次调用都不发。
	abL := NewAddBlackLogic(context.Background(), e.svcCtx)
	before := st.log.snapshot()
	wantErrMessage(t, "AddBlack 仍挡自发", mustAddBlack(t, abL, &rpc.BlackReq{Mid: 2001, BlackMid: 2001}), model.ErrSelfAction.Error())
	wantNoCall(t, "AddBlack 仍挡自发", st, before)
}

func TestDelBlackSoftDeletesRowAndKeepsCtime(t *testing.T) {
	e := newEnv(t)
	st := e.st
	now := time.Now().Unix()
	src := delBlackSeed(st, blackNormal)
	// 关注表与计数表也布好，用来证明「取消拉黑」一个字节都不碰它们。
	seedFollow(st, distinctFollow(0, 3002, 2001, 1_655_000_500))
	seedStat(st, &model.RelationStat{Mid: 2001, Following: 7, Follower: 3, Ctime: 1_600_000_001, Mtime: 1_600_000_002})

	l := NewDelBlackLogic(context.Background(), e.svcCtx)
	got, err := l.DelBlack(&rpc.BlackReq{Mid: 2001, BlackMid: 3002})
	wantNoErr(t, "取消拉黑", err)
	if got == nil {
		t.Fatalf("取消拉黑：响应 = nil, want 非空 EmptyReply")
	}
	wantOps(t, "取消拉黑链路", st.log.ops, []string{
		"black.Delete:select:2001>3002",
		"black.Delete:write:2001>3002",
	})
	row := st.blacks.row(2001, 3002)
	wantEQ(t, "取消拉黑后的行", "id 复用旧行", row.ID, src.ID)
	wantEQ(t, "取消拉黑后的行", "mid", row.Mid, src.Mid)
	wantEQ(t, "取消拉黑后的行", "black_mid", row.BlackMid, src.BlackMid)
	wantEQ(t, "取消拉黑后的行", "state 软删", row.State, blackGone)
	wantEQ(t, "取消拉黑后的行", "ctime 保持", row.Ctime, delBlackCtime)
	if row.Mtime == delBlackMtime {
		t.Errorf("取消拉黑后的行：mtime = %d 未刷新（UPDATE 未生效？）", row.Mtime)
	}
	assertAround(t, "取消拉黑后的行", "mtime", row.Mtime, now, 5)
	wantEQ(t, "取消拉黑", "行数不新增", st.blacks.countRows(), 1)

	// 只写黑名单：关注、计数、缓存、特别关注全部零调用。
	wantCount(t, "取消拉黑", st.log, "follow.", 0)
	wantCount(t, "取消拉黑", st.log, "cache.", 0)
	wantCount(t, "取消拉黑", st.log, "stat.", 0)
	wantCount(t, "取消拉黑", st.log, "special.", 0)
	assertStatRow(t, "取消拉黑后计数不变", st.stats.row(2001), 7, 3)
	wantEQ(t, "取消拉黑后关注不变", "state", st.follows.stateOfFollow(3002, 2001), followNormal)
}

// TestDelBlackDoesNotRestoreTheAutoUnfollow 是拉黑闭环里最容易被期待错的一点：
// AddBlack 会顺手取关，DelBlack 却不会把关注还回来（也不补计数）。
// 这里是**代码事实**，配对用例见 TestDelBlackThenFollowIsAllowed。
func TestDelBlackDoesNotRestoreTheAutoUnfollow(t *testing.T) {
	e := newEnv(t)
	st := e.st
	blackL := NewAddBlackLogic(context.Background(), e.svcCtx)
	followRow := addBlackSeedIn(st)
	wantNoErr(t, "拉黑", mustAddBlack(t, blackL, &rpc.BlackReq{Mid: 2001, BlackMid: 3002}))
	before := st.log.snapshot()

	l := NewDelBlackLogic(context.Background(), e.svcCtx)
	wantNoErr(t, "取消拉黑", mustDelBlack(t, l, &rpc.BlackReq{Mid: 2001, BlackMid: 3002}))
	wantOps(t, "取消拉黑链路", st.log.opsFrom(before), []string{
		"black.Delete:select:2001>3002",
		"black.Delete:write:2001>3002",
	})
	// 关注行依旧软删、双方计数依旧被扣、缓存不会回填。
	wantEQ(t, "取消拉黑后", "关注 state 仍是 1", st.follows.stateOfFollow(2001, 3002), followGone)
	wantEQ(t, "取消拉黑后", "关注行 id 保留", st.follows.row(2001, 3002).ID, followRow.ID)
	assertStatRow(t, "取消拉黑后 2001", st.stats.row(2001), 6, 3)
	assertStatRow(t, "取消拉黑后 3002", st.stats.row(3002), 11, 4)
	wantCount(t, "取消拉黑后", st.log, "cache.Add", 0)
	wantEQ(t, "取消拉黑后", "缓存里也没回填关注", st.cache.cachedIsFollowing(2001, 3002), false)
}

// TestDelBlackThenFollowIsAllowed 走完「拉黑 → 关注被拒 → 取消拉黑 → 关注成功」闭环，
// 证明 Follow 的黑名单守卫读的正是 DelBlack 写的那一行（含 state 过滤）。
func TestDelBlackThenFollowIsAllowed(t *testing.T) {
	e := newEnv(t)
	st := e.st
	delBlackSeed(st, blackNormal)
	followL := NewFollowLogic(context.Background(), e.svcCtx)
	blackL := NewDelBlackLogic(context.Background(), e.svcCtx)

	wantErrMessage(t, "拉黑期间关注", mustFollow(t, followL, &rpc.FollowReq{Mid: 2001, FollowerMid: 3002}),
		model.ErrBlackNeedCancelFollow.Error())
	wantOps(t, "拉黑期间关注链路", st.log.ops, []string{"black.FindOne:2001>3002"})
	wantEQ(t, "拉黑期间关注", "关注未落库", st.follows.countRows(), 0)

	before := st.log.snapshot()
	wantNoErr(t, "取消拉黑", mustDelBlack(t, blackL, &rpc.BlackReq{Mid: 2001, BlackMid: 3002}))
	wantNoErr(t, "取消拉黑后关注", mustFollow(t, followL, &rpc.FollowReq{Mid: 2001, FollowerMid: 3002}))
	wantStringsEQ(t, "闭环", "DelBlack 之后的调用", st.log.opsFrom(before), []string{
		"black.Delete:select:2001>3002",
		"black.Delete:write:2001>3002",
		"black.FindOne:2001>3002",
		"follow.Upsert:select:2001>3002",
		"follow.Upsert:write:2001>3002/0",
		"cache.IncrFollowingCount:2001/+1",
		"cache.IncrFollowerCount:3002/+1",
		"cache.AddFollowing:2001>3002",
		"cache.AddFollower:3002>2001",
		"stat.Incr:update:2001/+1/+0",
		// 本用例没布 relation_stat 行，UPDATE 影响 0 行 ⇒ 走 INSERT 分支（max(delta,0)）。
		"stat.Incr:insert:2001/+1/+0",
		"stat.Incr:update:3002/+0/+1",
		"stat.Incr:insert:3002/+0/+1",
	})
	wantEQ(t, "闭环", "关注 state", st.follows.stateOfFollow(2001, 3002), followNormal)
	assertStatRow(t, "闭环后 2001", st.stats.row(2001), 1, 0)
	assertStatRow(t, "闭环后 3002", st.stats.row(3002), 0, 1)
}

func TestDelBlackMissingAndAlreadyGoneAreBothIdempotentSuccess(t *testing.T) {
	cases := []struct {
		name       string
		seed       bool
		wantState  int32
		wantWrites int
	}{
		{"无黑名单行", false, -1, 0},
		{"已经取消过", true, blackGone, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			if c.seed {
				delBlackSeed(st, blackGone)
			}
			l := NewDelBlackLogic(context.Background(), e.svcCtx)

			wantNoErr(t, c.name, mustDelBlack(t, l, &rpc.BlackReq{Mid: 2001, BlackMid: 3002}))
			// 只发一条 SELECT 探状态，没有 UPDATE。
			wantOps(t, c.name+"链路", st.log.ops, []string{"black.Delete:select:2001>3002"})
			wantEQ(t, c.name, "state", st.blacks.stateOf(2001, 3002), c.wantState)
			wantCount(t, c.name, st.log, "black.Delete:write", c.wantWrites)
			wantCount(t, c.name, st.log, "cache.", 0)
		})
	}
}

func TestPropagatesDelBlackFailures(t *testing.T) {
	t.Run("读旧状态失败", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		delBlackSeed(st, blackNormal)
		st.blacks.failWith("DeleteSelect", errStore)
		l := NewDelBlackLogic(context.Background(), e.svcCtx)

		err := mustDelBlack(t, l, &rpc.BlackReq{Mid: 2001, BlackMid: 3002})
		wantErrIs(t, "取消拉黑 SELECT 失败", err, errStore)
		wantErrMessage(t, "取消拉黑 SELECT 失败", err, "relation_black Delete select: social-graph-test: store unavailable")
		wantOps(t, "取消拉黑 SELECT 失败链路", st.log.ops, []string{"black.Delete:select:2001>3002"})
		wantEQ(t, "取消拉黑 SELECT 失败", "黑名单没被改", st.blacks.stateOf(2001, 3002), blackNormal)
	})
	t.Run("UPDATE 失败", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		src := delBlackSeed(st, blackNormal)
		st.blacks.failWith("Delete", errStore)
		l := NewDelBlackLogic(context.Background(), e.svcCtx)

		err := mustDelBlack(t, l, &rpc.BlackReq{Mid: 2001, BlackMid: 3002})
		wantErrIs(t, "取消拉黑 UPDATE 失败", err, errStore)
		wantErrMessage(t, "取消拉黑 UPDATE 失败", err, "relation_black Delete: social-graph-test: store unavailable")
		wantOps(t, "取消拉黑 UPDATE 失败链路", st.log.ops, []string{
			"black.Delete:select:2001>3002",
			"black.Delete:write:2001>3002",
		})
		// 语句发了但没生效：行必须仍是原值（不得假成功）。
		assertBlackRow(t, "取消拉黑 UPDATE 失败后行未变", st.blacks.row(2001, 3002), src)
		// 失败后依然挡住关注：黑名单还在，Follow 仍被拒。
		wantErrMessage(t, "失败后关注仍被拒",
			mustFollow(t, NewFollowLogic(context.Background(), e.svcCtx), &rpc.FollowReq{Mid: 2001, FollowerMid: 3002}),
			model.ErrBlackNeedCancelFollow.Error())
	})
}

// assertBlackRow 逐字段核对 relation_black 的 6 列（nil 视为失败）。
func assertBlackRow(t *testing.T, label string, got, src *model.RelationBlack) {
	t.Helper()
	if got == nil || src == nil {
		t.Fatalf("%s：行缺失 got=%v src=%v", label, got, src)
	}
	wantEQ(t, label, "id", got.ID, src.ID)
	wantEQ(t, label, "mid", got.Mid, src.Mid)
	wantEQ(t, label, "black_mid", got.BlackMid, src.BlackMid)
	wantEQ(t, label, "state", got.State, src.State)
	wantEQ(t, label, "ctime", got.Ctime, src.Ctime)
	wantEQ(t, label, "mtime", got.Mtime, src.Mtime)
}

// addBlackSeedIn 复用 addblacklogic_test.go 的布景，但把返回收窄成关注行。
func addBlackSeedIn(st *store) *model.RelationFollow {
	out, _ := addBlackSeed(st)
	return out
}
