package logic

// delspeciallogic_test.go 覆盖 DelSpecial 的 4 类断言：守卫（无自发保护）、正常路径逐字段投影、
// 下游失败传播、本域不变量（无行/已取消两种幂等口径、清位绝不影响关注与计数、
// 它是唯一能清掉「取关后孤儿位」的入口且不校验关注状态）。

import (
	"context"
	"testing"
	"time"

	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"
)

// delSpecialSeed 布一行特别关注位（id=444，时间戳固定）与它所依赖的关注关系。
func delSpecialSeed(st *store, state int32) (*model.RelationSpecial, *model.RelationFollow) {
	sp := seedSpecial(st, &model.RelationSpecial{
		ID: 444, Mid: 2001, SpecialMid: 3002, State: state,
		Ctime: 1_600_000_444, Mtime: 1_600_000_445,
	})
	fw := seedFollow(st, &model.RelationFollow{
		Mid: 2001, FollowerMid: 3002, State: followNormal,
		Ctime: 1_658_000_100, Mtime: 1_658_000_107,
	})
	seedStat(st, &model.RelationStat{Mid: 2001, Following: 7, Follower: 3, Ctime: 1_600_000_001, Mtime: 1_600_000_002})
	seedStat(st, &model.RelationStat{Mid: 3002, Following: 11, Follower: 5, Ctime: 1_600_000_003, Mtime: 1_600_000_004})
	st.cache.warmFollowing(2001, 3002)
	st.cache.warmCounts(2001, 7, 3)
	st.cache.warmCounts(3002, 11, 5)
	return sp, fw
}

func mustDelSpecial(t *testing.T, l *DelSpecialLogic, in *rpc.SpecialReq) error {
	t.Helper()
	_, err := l.DelSpecial(in)
	return err
}

// assertSpecialRow 逐字段核对 relation_special 的 5 列（迁移 SQL 里没有 attr 列）。
func assertSpecialRow(t *testing.T, label string, got, src *model.RelationSpecial) {
	t.Helper()
	if got == nil || src == nil {
		t.Fatalf("%s：行缺失 got=%v src=%v", label, got, src)
	}
	wantEQ(t, label, "id", got.ID, src.ID)
	wantEQ(t, label, "mid", got.Mid, src.Mid)
	wantEQ(t, label, "special_mid", got.SpecialMid, src.SpecialMid)
	wantEQ(t, label, "state", got.State, src.State)
	wantEQ(t, label, "ctime", got.Ctime, src.Ctime)
	wantEQ(t, label, "mtime", got.Mtime, src.Mtime)
}

func TestDelSpecialGuards(t *testing.T) {
	cases := []struct {
		name    string
		mid, sp int64
		want    error
	}{
		{"mid 为 0", 0, 3002, model.ErrInvalidMid},
		{"mid 为负", -1, 3002, model.ErrInvalidMid},
		{"special_mid 为 0", 2001, 0, model.ErrInvalidSpecialMid},
		{"special_mid 为负", 2001, -8, model.ErrInvalidSpecialMid},
		{"两侧同为负数先报 mid", -5, -5, model.ErrInvalidMid},
		{"两侧同为 0 先报 mid", 0, 0, model.ErrInvalidMid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			delSpecialSeed(e.st, specialNormal)
			l := NewDelSpecialLogic(context.Background(), e.svcCtx)
			wantGuardRejected(t, e.st, c.name, c.want, func() error {
				return mustDelSpecial(t, l, &rpc.SpecialReq{Mid: c.mid, SpecialMid: c.sp, RealIp: "10.1.2.3"})
			})
		})
	}
}

// TestDelSpecialHasNoSelfGuard 与 AddSpecial（有 ErrSelfAction）对照：
// 取消侧没有自发保护，「取消自己对自己的特别关注」会真的发两条语句。
func TestDelSpecialHasNoSelfGuard(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedSpecial(st, &model.RelationSpecial{
		ID: 444, Mid: 2001, SpecialMid: 2001, State: specialNormal, Ctime: 1_600_000_444, Mtime: 1_600_000_445,
	})
	l := NewDelSpecialLogic(context.Background(), e.svcCtx)

	wantNoErr(t, "取消自发特别关注", mustDelSpecial(t, l, &rpc.SpecialReq{Mid: 2001, SpecialMid: 2001}))
	wantOps(t, "取消自发特别关注链路", st.log.ops, []string{
		"special.Delete:select:2001>2001",
		"special.Delete:write:2001>2001",
	})
	wantEQ(t, "取消自发特别关注", "state", st.specials.stateOf(2001, 2001), specialGone)
	wantEQ(t, "取消自发特别关注", "id 复用旧行", st.specials.row(2001, 2001).ID, int64(444))

	// AddSpecial 侧对照：同一对参数被守卫挡下，零调用。
	before := st.log.snapshot()
	wantErrMessage(t, "AddSpecial 仍挡自发",
		mustAddSpecial(t, NewAddSpecialLogic(context.Background(), e.svcCtx), &rpc.SpecialReq{Mid: 2001, SpecialMid: 2001}),
		model.ErrSelfAction.Error())
	wantNoCall(t, "AddSpecial 仍挡自发", st, before)
}

func TestDelSpecialSoftDeletesRowAndKeepsCtime(t *testing.T) {
	e := newEnv(t)
	st := e.st
	now := time.Now().Unix()
	sp, fw := delSpecialSeed(st, specialNormal)
	l := NewDelSpecialLogic(context.Background(), e.svcCtx)

	got, err := l.DelSpecial(&rpc.SpecialReq{Mid: 2001, SpecialMid: 3002})
	wantNoErr(t, "取消特别关注", err)
	if got == nil {
		t.Fatalf("取消特别关注：响应 = nil, want 非空 EmptyReply")
	}
	wantOps(t, "取消特别关注链路", st.log.ops, []string{
		"special.Delete:select:2001>3002",
		"special.Delete:write:2001>3002",
	})

	row := st.specials.row(2001, 3002)
	wantEQ(t, "取消特别关注后的行", "id 复用旧行", row.ID, sp.ID)
	wantEQ(t, "取消特别关注后的行", "mid", row.Mid, sp.Mid)
	wantEQ(t, "取消特别关注后的行", "special_mid", row.SpecialMid, sp.SpecialMid)
	wantEQ(t, "取消特别关注后的行", "state 软删", row.State, specialGone)
	wantEQ(t, "取消特别关注后的行", "ctime 保持", row.Ctime, sp.Ctime)
	if row.Mtime == sp.Mtime {
		t.Errorf("取消特别关注后的行：mtime = %d 未刷新（UPDATE 未生效？）", row.Mtime)
	}
	assertAround(t, "取消特别关注后的行", "mtime", row.Mtime, now, 5)
	wantEQ(t, "取消特别关注", "行数不新增", st.specials.countRows(), 1)

	// 清位只清位：关注关系、两个方向的计数、Redis 全部零调用、零改动。
	wantCount(t, "取消特别关注", st.log, "follow.", 0)
	wantCount(t, "取消特别关注", st.log, "cache.", 0)
	wantCount(t, "取消特别关注", st.log, "stat.", 0)
	wantCount(t, "取消特别关注", st.log, "black.", 0)
	assertFollowRow(t, "取消特别关注后关注行", st.follows.row(2001, 3002), fw)
	assertStatRow(t, "取消特别关注后 2001", st.stats.row(2001), 7, 3)
	assertStatRow(t, "取消特别关注后 3002", st.stats.row(3002), 11, 5)
	wantEQ(t, "取消特别关注后缓存", "关注集合仍在", st.cache.cachedIsFollowing(2001, 3002), true)
}

func TestDelSpecialMissingAndAlreadyGoneAreIdempotentSuccess(t *testing.T) {
	cases := []struct {
		name      string
		seed      bool
		wantState int32
	}{
		{"无特别关注行", false, -1},
		{"已经取消过", true, specialGone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			var src *model.RelationSpecial
			if c.seed {
				src, _ = delSpecialSeed(st, specialGone)
			}
			l := NewDelSpecialLogic(context.Background(), e.svcCtx)

			wantNoErr(t, c.name, mustDelSpecial(t, l, &rpc.SpecialReq{Mid: 2001, SpecialMid: 3002}))
			// 只发一条 SELECT 探状态，没有 UPDATE。
			wantOps(t, c.name+"链路", st.log.ops, []string{"special.Delete:select:2001>3002"})
			wantEQ(t, c.name, "state", st.specials.stateOf(2001, 3002), c.wantState)
			if src != nil {
				assertSpecialRow(t, c.name+"后行未变", st.specials.row(2001, 3002), src)
			}
			wantCount(t, c.name, st.log, "follow.", 0)
			wantCount(t, c.name, st.log, "cache.", 0)
		})
	}
}

// TestDelSpecialClearsOrphanFlagWithoutCheckingFollow 补上取关缺陷的出口：
// 特别关注位被 Unfollow 留成孤儿（关注已 state=1）之后，DelSpecial 不校验关注状态，
// 仍然能把位清掉——它是本服务唯一的清理入口（列表读侧完全不体现这个位，见 README）。
func TestDelSpecialClearsOrphanFlagWithoutCheckingFollow(t *testing.T) {
	e := newEnv(t)
	st := e.st
	_, _ = delSpecialSeed(st, specialNormal)
	unL := NewUnfollowLogic(context.Background(), e.svcCtx)
	wantNoErr(t, "取关", mustUnfollow(t, unL, &rpc.UnfollowReq{Mid: 2001, FollowerMid: 3002}))
	before := st.log.snapshot()

	spL := NewDelSpecialLogic(context.Background(), e.svcCtx)
	wantNoErr(t, "清孤儿位", mustDelSpecial(t, spL, &rpc.SpecialReq{Mid: 2001, SpecialMid: 3002}))
	wantOps(t, "清孤儿位链路", st.log.opsFrom(before), []string{
		"special.Delete:select:2001>3002",
		"special.Delete:write:2001>3002",
	})
	wantEQ(t, "清孤儿位", "位已软删", st.specials.stateOf(2001, 3002), specialGone)
	// 关注行不因清位被恢复（仍是取关后的 state=1），计数也不再回调。
	wantEQ(t, "清孤儿位", "关注 state", st.follows.stateOfFollow(2001, 3002), followGone)
	assertStatRow(t, "清孤儿位后 2001", st.stats.row(2001), 6, 3)
	assertStatRow(t, "清孤儿位后 3002", st.stats.row(3002), 11, 4)
	// 取关留下的计数与缓存变化不被动：上面 wantOps 已钉住清位只发了两条 special.Delete。
}

func TestPropagatesDelSpecialFailures(t *testing.T) {
	t.Run("读旧状态失败", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		src, fw := delSpecialSeed(st, specialNormal)
		st.specials.failWith("DeleteSelect", errStore)
		l := NewDelSpecialLogic(context.Background(), e.svcCtx)

		err := mustDelSpecial(t, l, &rpc.SpecialReq{Mid: 2001, SpecialMid: 3002})
		wantErrIs(t, "取消位 SELECT 失败", err, errStore)
		wantErrMessage(t, "取消位 SELECT 失败", err, "relation_special Delete select: social-graph-test: store unavailable")
		wantOps(t, "取消位 SELECT 失败链路", st.log.ops, []string{"special.Delete:select:2001>3002"})
		assertSpecialRow(t, "取消位 SELECT 失败后行未变", st.specials.row(2001, 3002), src)
		assertFollowRow(t, "取消位 SELECT 失败后关注未变", st.follows.row(2001, 3002), fw)
	})
	t.Run("UPDATE 失败", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		src, _ := delSpecialSeed(st, specialNormal)
		st.specials.failWith("Delete", errStore)
		l := NewDelSpecialLogic(context.Background(), e.svcCtx)

		_, err := l.DelSpecial(&rpc.SpecialReq{Mid: 2001, SpecialMid: 3002})
		wantErrIs(t, "取消位 UPDATE 失败", err, errStore)
		wantErrMessage(t, "取消位 UPDATE 失败", err, "relation_special Delete: social-graph-test: store unavailable")
		wantOps(t, "取消位 UPDATE 失败链路", st.log.ops, []string{
			"special.Delete:select:2001>3002",
			"special.Delete:write:2001>3002",
		})
		// 语句发了但没生效：位仍是 state=0，调用方拿到错误而不是假成功。
		assertSpecialRow(t, "取消位 UPDATE 失败后行未变", st.specials.row(2001, 3002), src)
		wantEQ(t, "取消位 UPDATE 失败", "state 未软删", st.specials.stateOf(2001, 3002), specialNormal)
	})
}
