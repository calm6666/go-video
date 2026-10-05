package logic

// addspeciallogic_test.go 覆盖 AddSpecial 的 4 类断言：守卫、正常路径逐字段投影、下游失败传播、
// 本域不变量（「必先关注」的两条判定来源、幂等不二次落库、特别关注位不覆盖关注位、
// 位与黑名单/取关状态的冲突组合）。

import (
	"context"
	"testing"
	"time"

	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"
)

// specialSeedCtime 布进 relation_follow 的关注建立时间；特别关注行与它互不覆盖。
const specialSeedCtime = 1_657_000_100

// addSpecialSeed 布「2001 关注了 3002」这一条事实（关注行 + 可选缓存），
// 因为 AddSpecial 的前置校验只看这个方向。
func addSpecialSeed(st *store, warmCache bool) *model.RelationFollow {
	row := seedFollow(st, &model.RelationFollow{
		Mid: 2001, FollowerMid: 3002, State: followNormal,
		Ctime: specialSeedCtime, Mtime: specialSeedCtime + 7,
	})
	if warmCache {
		st.cache.warmFollowing(2001, 3002)
	}
	return row
}

func mustAddSpecial(t *testing.T, l *AddSpecialLogic, in *rpc.SpecialReq) error {
	t.Helper()
	_, err := l.AddSpecial(in)
	return err
}

func TestAddSpecialGuards(t *testing.T) {
	cases := []struct {
		name    string
		mid, sp int64
		want    error
	}{
		{"mid 为 0", 0, 3002, model.ErrInvalidMid},
		{"mid 为负", -1, 3002, model.ErrInvalidMid},
		{"special_mid 为 0", 2001, 0, model.ErrInvalidSpecialMid},
		{"special_mid 为负", 2001, -8, model.ErrInvalidSpecialMid},
		{"自己特别关注自己", 2001, 2001, model.ErrSelfAction},
		{"两侧同为负数先报 mid", -5, -5, model.ErrInvalidMid},
		{"两侧同为 0 先报 mid", 0, 0, model.ErrInvalidMid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			addSpecialSeed(e.st, true)
			l := NewAddSpecialLogic(context.Background(), e.svcCtx)
			wantGuardRejected(t, e.st, c.name, c.want, func() error {
				return mustAddSpecial(t, l, &rpc.SpecialReq{Mid: c.mid, SpecialMid: c.sp, RealIp: "10.1.2.3"})
			})
		})
	}
}

// TestAddSpecialWithCacheHitWritesOnlySpecialRow 是「位不互相覆盖」的正证：
// 关注判定命中缓存后直接短路，全程不读关注表、不写关注表、不动任何计数。
func TestAddSpecialWithCacheHitWritesOnlySpecialRow(t *testing.T) {
	e := newEnv(t)
	st := e.st
	now := time.Now().Unix()
	src := addSpecialSeed(st, true)
	l := NewAddSpecialLogic(context.Background(), e.svcCtx)

	got, err := l.AddSpecial(&rpc.SpecialReq{Mid: 2001, SpecialMid: 3002})
	wantNoErr(t, "特别关注（缓存判定）", err)
	if got == nil {
		t.Fatalf("特别关注（缓存判定）：响应 = nil, want 非空 EmptyReply")
	}
	wantOps(t, "特别关注（缓存判定）链路", st.log.ops, []string{
		"cache.IsFollowing:2001>3002",
		"special.Upsert:select:2001>3002",
		"special.Upsert:write:2001>3002/0",
	})

	row := st.specials.row(2001, 3002)
	if row == nil {
		t.Fatalf("特别关注（缓存判定）：relation_special 未落库")
	}
	wantEQ(t, "特别关注行", "id", row.ID, int64(401))
	wantEQ(t, "特别关注行", "mid（发起方）", row.Mid, int64(2001))
	wantEQ(t, "特别关注行", "special_mid（被特别关注者）", row.SpecialMid, int64(3002))
	wantEQ(t, "特别关注行", "state", row.State, specialNormal)
	assertAround(t, "特别关注行", "ctime", row.Ctime, now, 5)
	assertAround(t, "特别关注行", "mtime", row.Mtime, now, 5)
	wantEQ(t, "特别关注", "特别关注行数", st.specials.countRows(), 1)

	// 关注行一个字节都不许变（attr/state/ctime/mtime/id 全部原样）。
	assertFollowRow(t, "特别关注后关注行", st.follows.row(2001, 3002), src)
	wantCount(t, "特别关注", st.log, "follow.", 0)
	wantCount(t, "特别关注", st.log, "cache.Incr", 0)
	wantCount(t, "特别关注", st.log, "cache.Add", 0)
	wantCount(t, "特别关注", st.log, "stat.", 0)
	wantCount(t, "特别关注", st.log, "black.", 0)
	wantEQ(t, "特别关注", "计数表无行也不被建出来", st.stats.countRows(), 0)
}

// TestAddSpecialWithColdCacheReadsThroughAndBackfills 说明「必先关注」的第二个判定来源：
// 缓存 miss 时回查关注表，并顺手回填关注集合，然后才写特别关注位。
func TestAddSpecialWithColdCacheReadsThroughAndBackfills(t *testing.T) {
	e := newEnv(t)
	st := e.st
	addSpecialSeed(st, false)
	l := NewAddSpecialLogic(context.Background(), e.svcCtx)

	wantNoErr(t, "特别关注（回源判定）", mustAddSpecial(t, l, &rpc.SpecialReq{Mid: 2001, SpecialMid: 3002}))
	wantOps(t, "特别关注（回源判定）链路", st.log.ops, []string{
		"cache.IsFollowing:2001>3002",
		"follow.FindOne:2001>3002",
		"cache.AddFollowing:2001>3002",
		"special.Upsert:select:2001>3002",
		"special.Upsert:write:2001>3002/0",
	})
	wantEQ(t, "特别关注（回源判定）", "特别关注 state", st.specials.stateOf(2001, 3002), specialNormal)
	wantEQ(t, "特别关注（回源判定）", "关注集合已回填", st.cache.cachedIsFollowing(2001, 3002), true)
}

// TestAddSpecialRequiresExistingFollow 钉住 ErrSpecialNeedFollow 的三种「没关注」形态：
// 从来没关注过、已取关（软删）、只被对方关注（反向）。三种都必须拒绝且不写特别关注行。
func TestAddSpecialRequiresExistingFollow(t *testing.T) {
	cases := []struct {
		name  string
		seed  func(st *store)
		ops   []string
		setup func(st *store)
	}{
		{
			name: "从来没关注过",
			ops: []string{
				"cache.IsFollowing:2001>3002",
				"follow.FindOne:2001>3002",
				"cache.DelFollowing:2001>3002", // 负回填：把「没关注」也写进缓存
			},
		},
		{
			name: "已经取关",
			seed: func(st *store) {
				seedFollow(st, &model.RelationFollow{
					Mid: 2001, FollowerMid: 3002, State: followGone,
					Ctime: specialSeedCtime, Mtime: specialSeedCtime + 7,
				})
			},
			ops: []string{
				"cache.IsFollowing:2001>3002",
				"follow.FindOne:2001>3002",
				"cache.DelFollowing:2001>3002",
			},
		},
		{
			name: "只有反向关注",
			seed: func(st *store) {
				seedFollow(st, &model.RelationFollow{
					Mid: 3002, FollowerMid: 2001, State: followNormal,
					Ctime: specialSeedCtime, Mtime: specialSeedCtime + 7,
				})
			},
			ops: []string{
				"cache.IsFollowing:2001>3002",
				"follow.FindOne:2001>3002",
				"cache.DelFollowing:2001>3002",
			},
		},
		{
			name: "缓存已判定为未关注（不回源）",
			setup: func(st *store) {
				st.cache.warmFollowing(2001, 4321) // 键存在但成员不是 3002 ⇒ hit=true, following=false
			},
			ops: []string{"cache.IsFollowing:2001>3002"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			if c.seed != nil {
				c.seed(st)
			}
			if c.setup != nil {
				c.setup(st)
			}
			l := NewAddSpecialLogic(context.Background(), e.svcCtx)

			err := mustAddSpecial(t, l, &rpc.SpecialReq{Mid: 2001, SpecialMid: 3002})
			wantErrIs(t, c.name, err, model.ErrSpecialNeedFollow)
			wantErrMessage(t, c.name, err, model.ErrSpecialNeedFollow.Error())
			wantOps(t, c.name+"链路", st.log.ops, c.ops)
			wantEQ(t, c.name, "特别关注行数", st.specials.countRows(), 0)
			wantCount(t, c.name, st.log, "special.", 0)
		})
	}
}

// TestRepeatAddSpecialDoesNotRewriteRow 对应 UNIQUE KEY uniq_mid_special (mid, special_mid)：
// 第二次特别关注读到 oldState=0 == 目标 state，一条写都不发，mtime 也不刷新。
func TestRepeatAddSpecialDoesNotRewriteRow(t *testing.T) {
	e := newEnv(t)
	st := e.st
	addSpecialSeed(st, true)
	l := NewAddSpecialLogic(context.Background(), e.svcCtx)
	req := &rpc.SpecialReq{Mid: 2001, SpecialMid: 3002}

	wantNoErr(t, "第一次特别关注", mustAddSpecial(t, l, req))
	first := st.specials.row(2001, 3002)
	before := st.log.snapshot()
	wantNoErr(t, "第二次特别关注", mustAddSpecial(t, l, req))

	wantOps(t, "重复特别关注链路", st.log.opsFrom(before), []string{
		"cache.IsFollowing:2001>3002",
		"special.Upsert:select:2001>3002",
	})
	wantCount(t, "重复特别关注", st.log, "special.Upsert:write", 1)
	second := st.specials.row(2001, 3002)
	wantEQ(t, "重复特别关注后行", "id", second.ID, first.ID)
	wantEQ(t, "重复特别关注后行", "state", second.State, first.State)
	wantEQ(t, "重复特别关注后行", "ctime", second.Ctime, first.Ctime)
	wantEQ(t, "重复特别关注后行", "mtime 不刷新", second.Mtime, first.Mtime)
	wantEQ(t, "重复特别关注后行", "行数", st.specials.countRows(), 1)
}

// TestAddSpecialRestoresSoftDeletedRowWithoutRefreshingCtime：取消后再设位，
// ON DUPLICATE KEY UPDATE 只改 state+mtime。
func TestAddSpecialRestoresSoftDeletedRowWithoutRefreshingCtime(t *testing.T) {
	e := newEnv(t)
	st := e.st
	now := time.Now().Unix()
	addSpecialSeed(st, true)
	src := seedSpecial(st, &model.RelationSpecial{
		ID: 444, Mid: 2001, SpecialMid: 3002, State: specialGone,
		Ctime: 1_600_000_444, Mtime: 1_600_000_445,
	})
	l := NewAddSpecialLogic(context.Background(), e.svcCtx)

	wantNoErr(t, "恢复特别关注", mustAddSpecial(t, l, &rpc.SpecialReq{Mid: 2001, SpecialMid: 3002}))
	wantOps(t, "恢复特别关注链路", st.log.ops, []string{
		"cache.IsFollowing:2001>3002",
		"special.Upsert:select:2001>3002",
		"special.Upsert:write:2001>3002/0",
	})
	row := st.specials.row(2001, 3002)
	wantEQ(t, "恢复特别关注后的行", "id 复用旧行", row.ID, src.ID)
	wantEQ(t, "恢复特别关注后的行", "state 回到 0", row.State, specialNormal)
	wantEQ(t, "恢复特别关注后的行", "ctime 不被刷新", row.Ctime, src.Ctime)
	assertAround(t, "恢复特别关注后的行", "mtime", row.Mtime, now, 5)
	wantEQ(t, "恢复特别关注", "行数不新增", st.specials.countRows(), 1)
}

// TestAddSpecialIgnoresBlacklist 是「黑名单 × 关系位」交叉的第四格：
// 特别关注既不查黑名单，也不管库里关注早已被 AddBlack 软删——只要 Redis 关注集合还留着成员就放行。
// TODO(缺陷)：拉黑后（缓存未清干净时）仍能给自己已拉黑的人设特别关注位，
// 且这条位会一直残留到 DelSpecial（Unfollow/AddBlack 都不清位，见 README 已知缺口）。
func TestAddSpecialIgnoresBlacklist(t *testing.T) {
	e := newEnv(t)
	st := e.st
	addSpecialSeed(st, true)
	seedBlack(st, distinctBlack(0, 2001, 3002, 1_657_000_900)) // 2001 已拉黑 3002
	l := NewAddSpecialLogic(context.Background(), e.svcCtx)

	wantNoErr(t, "给已拉黑的人设特别关注", mustAddSpecial(t, l, &rpc.SpecialReq{Mid: 2001, SpecialMid: 3002}))
	wantCount(t, "给已拉黑的人设位", st.log, "black.", 0)
	wantEQ(t, "给已拉黑的人设位", "特别关注位已写", st.specials.stateOf(2001, 3002), specialNormal)
	wantOps(t, "给已拉黑的人设位链路", st.log.ops, []string{
		"cache.IsFollowing:2001>3002",
		"special.Upsert:select:2001>3002",
		"special.Upsert:write:2001>3002/0",
	})
}

// TestAddSpecialAfterUnfollowRefusesButLeavesStaleFlag 把两个已知缺陷接在一起：
// 取关不清特别关注位（旧位仍是 state=0），而 AddSpecial 此时会正确拒绝——
// 于是库里长期留着「没关注却特别关注」的孤儿位，读侧列表又完全不体现它。
// TODO(缺陷)
func TestAddSpecialAfterUnfollowRefusesButLeavesStaleFlag(t *testing.T) {
	e := newEnv(t)
	st := e.st
	addSpecialSeed(st, true)
	seedSpecial(st, &model.RelationSpecial{
		ID: 444, Mid: 2001, SpecialMid: 3002, State: specialNormal,
		Ctime: 1_600_000_444, Mtime: 1_600_000_445,
	})
	unL := NewUnfollowLogic(context.Background(), e.svcCtx)
	wantNoErr(t, "取关", mustUnfollow(t, unL, &rpc.UnfollowReq{Mid: 2001, FollowerMid: 3002}))
	before := st.log.snapshot()

	spL := NewAddSpecialLogic(context.Background(), e.svcCtx)
	err := mustAddSpecial(t, spL, &rpc.SpecialReq{Mid: 2001, SpecialMid: 3002})
	wantErrMessage(t, "取关后重设位", err, model.ErrSpecialNeedFollow.Error())
	wantOps(t, "取关后重设位链路", st.log.opsFrom(before), []string{
		"cache.IsFollowing:2001>3002",
		"follow.FindOne:2001>3002",
		"cache.DelFollowing:2001>3002",
	})
	// 孤儿位仍在（state=0），而且没人清它。
	wantEQ(t, "取关后重设位", "残留的特别关注位", st.specials.stateOf(2001, 3002), specialNormal)
	wantEQ(t, "取关后重设位", "关注位已软删", st.follows.stateOfFollow(2001, 3002), followGone)
}

func TestPropagatesAddSpecialFailures(t *testing.T) {
	cases := []struct {
		name        string
		inject      func(st *store)
		is          error
		wantErr     string
		want        []string
		wantWritten bool
		cold        bool // true = 不预热关注集合，逼判定回源
	}{
		{
			name:    "关注判定读缓存失败",
			inject:  func(st *store) { st.cache.failWith("IsFollowing", errCache) },
			is:      errCache,
			wantErr: errCache.Error(), // Repository 没包装缓存错误，整串就是哨兵文案
			want:    []string{"cache.IsFollowing:2001>3002"},
		},
		{
			name:        "关注判定回源失败",
			inject:      func(st *store) { st.follows.failWith("FindOne", errStore) },
			is:          errStore,
			wantErr:     "relation_follow FindOne: social-graph-test: store unavailable",
			want:        []string{"cache.IsFollowing:2001>3002", "follow.FindOne:2001>3002"},
			wantWritten: false,
			cold:        true,
		},
		{
			name:        "读特别关注旧状态失败",
			inject:      func(st *store) { st.specials.failWith("UpsertSelect", errStore) },
			is:          errStore,
			wantErr:     "relation_special Upsert select: social-graph-test: store unavailable",
			want:        []string{"cache.IsFollowing:2001>3002", "special.Upsert:select:2001>3002"},
			wantWritten: false,
		},
		{
			name:        "写特别关注行失败",
			inject:      func(st *store) { st.specials.failWith("Upsert", errStore) },
			is:          errStore,
			wantErr:     "relation_special Upsert: social-graph-test: store unavailable",
			want:        []string{"cache.IsFollowing:2001>3002", "special.Upsert:select:2001>3002", "special.Upsert:write:2001>3002/0"},
			wantWritten: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			addSpecialSeed(st, !c.cold)
			c.inject(st)
			l := NewAddSpecialLogic(context.Background(), e.svcCtx)

			_, err := l.AddSpecial(&rpc.SpecialReq{Mid: 2001, SpecialMid: 3002})
			wantErrIs(t, c.name, err, c.is)
			wantErrMessage(t, c.name, err, c.wantErr)
			wantOps(t, c.name+"链路", st.log.ops, c.want)
			wantEQ(t, c.name, "特别关注行是否落库", st.specials.row(2001, 3002) != nil, c.wantWritten)
			// 任何一步失败都不许顺手改关注关系或计数。
			wantCount(t, c.name, st.log, "follow.Upsert", 0)
			wantCount(t, c.name, st.log, "stat.", 0)
		})
	}
}

// TestAddSpecialSucceedsWhenBackfillWriteFails 说明回填缓存的写失败被吞（_ = err）：
// 判定已经完成，位照样落库，调用返回成功。
func TestAddSpecialSucceedsWhenBackfillWriteFails(t *testing.T) {
	e := newEnv(t)
	st := e.st
	addSpecialSeed(st, false)
	st.cache.failWith("AddFollowing", errCache)
	l := NewAddSpecialLogic(context.Background(), e.svcCtx)

	wantNoErr(t, "回填失败仍成功", mustAddSpecial(t, l, &rpc.SpecialReq{Mid: 2001, SpecialMid: 3002}))
	wantOps(t, "回填失败链路", st.log.ops, []string{
		"cache.IsFollowing:2001>3002",
		"follow.FindOne:2001>3002",
		"cache.AddFollowing:2001>3002",
		"special.Upsert:select:2001>3002",
		"special.Upsert:write:2001>3002/0",
	})
	wantEQ(t, "回填失败", "位已落库", st.specials.stateOf(2001, 3002), specialNormal)
	wantEQ(t, "回填失败", "缓存集合没被建出来", st.cache.followSetExists(2001), false)
}
