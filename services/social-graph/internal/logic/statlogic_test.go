package logic

// statlogic_test.go 覆盖 Stat：守卫、缓存双命中不查库、任一 miss 的回源与回填、
// 下游失败传播，以及两条本域口径（悄悄关注位恒为 0、命中那一路会被 miss 那一路拖累）。

import (
	"context"
	"testing"

	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"
)

func TestStatGuards(t *testing.T) {
	cases := []struct {
		name string
		mid  int64
		want error
	}{
		{"mid 为 0", 0, model.ErrInvalidMid},
		{"mid 为负", -4, model.ErrInvalidMid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			l := NewStatLogic(context.Background(), e.svcCtx)
			wantGuardRejected(t, e.st, c.name, c.want, func() error {
				_, err := l.Stat(&rpc.MidReq{Mid: c.mid})
				return err
			})
		})
	}
}

func TestStatBothCountersHitSkipsDatabase(t *testing.T) {
	e := newEnv(t)
	st := e.st
	// DB 快照故意与缓存不同：命中路径若偷偷回库，7/3 会变成 1/2。
	seedStat(st, &model.RelationStat{Mid: 2001, Following: 1, Follower: 2, Whisper: 0, Ctime: 1_600_000_001, Mtime: 1_600_000_002})
	st.cache.warmCounts(2001, 7, 3)
	l := NewStatLogic(context.Background(), e.svcCtx)

	got, err := l.Stat(&rpc.MidReq{Mid: 2001})
	wantNoErr(t, "双命中", err)
	wantEQ(t, "双命中", "following", got.GetFollowing(), int64(7))
	wantEQ(t, "双命中", "follower", got.GetFollower(), int64(3))
	wantEQ(t, "双命中", "whisper", int64(got.GetWhisper()), int64(0))
	wantOps(t, "双命中链路", st.log.ops, []string{
		"cache.GetFollowingCount:2001",
		"cache.GetFollowerCount:2001",
	})
	wantCount(t, "双命中", st.log, "stat.", 0)
	wantCount(t, "双命中", st.log, "follow.", 0)
}

// TODO(缺陷): 两个计数器分别判定，**任一 miss 就整对改用 DB 快照**，
// 于是另一个已命中的权威 Redis 值被直接丢弃（这里 7 被 1 顶掉），
// 反向也一样：Redis 说 7、快照说 1，读到的到底是哪个取决于另一个键在不在。
// 定位：internal/repository/repository.go 的 Repository.Stat。用例把当前行为钉成哨兵。
func TestStatOneMissDropsTheHitCounter(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedStat(st, &model.RelationStat{Mid: 2001, Following: 1, Follower: 2, Ctime: 1_600_000_001, Mtime: 1_600_000_002})
	st.cache.warmCounts(2001, 7, 0)
	// 只让 following 命中：删掉 follower 键（warmCounts 会写两个，所以直接改内部状态）。
	delete(st.cache.follower, 2001)
	l := NewStatLogic(context.Background(), e.svcCtx)

	got, err := l.Stat(&rpc.MidReq{Mid: 2001})
	wantNoErr(t, "单侧 miss", err)
	wantEQ(t, "单侧 miss", "following（缓存里的 7 被丢弃）", got.GetFollowing(), int64(1))
	wantEQ(t, "单侧 miss", "follower", got.GetFollower(), int64(2))
	wantOps(t, "单侧 miss 链路", st.log.ops, []string{
		"cache.GetFollowingCount:2001",
		"cache.GetFollowerCount:2001",
		"stat.Find:2001",
		"cache.SetFollowingCount:2001",
		"cache.SetFollowerCount:2001",
	})
	f, fHit := st.cache.cachedFollowing(2001)
	r, _ := st.cache.cachedFollower(2001)
	wantEQ(t, "回刷后的 Redis", "following 被快照覆盖", f, int64(1))
	wantEQ(t, "回刷后的 Redis", "following 命中", fHit, true)
	wantEQ(t, "回刷后的 Redis", "follower 建出来了", r, int64(2))
}

// TODO(缺陷): 快照缺失且计数器只命中一半时，直接返回 0/0 **且跳过回填**
// （`if stat == nil { return }` 在两次 Set*Count 之前）。
// 后果：该用户每次 Stat 都回库，且已命中的那一半计数被无声吞掉（无负缓存、无告警）。
// 定位：internal/repository/repository.go 的 Repository.Stat。
func TestStatMissingSnapshotDropsHitCounterAndNeverBackfills(t *testing.T) {
	e := newEnv(t)
	st := e.st
	st.cache.warmCounts(2001, 7, 0)
	delete(st.cache.follower, 2001)
	l := NewStatLogic(context.Background(), e.svcCtx)

	got, err := l.Stat(&rpc.MidReq{Mid: 2001})
	wantNoErr(t, "无快照", err)
	wantEQ(t, "无快照", "following（明明命中 7）", got.GetFollowing(), int64(0))
	wantEQ(t, "无快照", "follower", got.GetFollower(), int64(0))
	wantOps(t, "无快照链路", st.log.ops, []string{
		"cache.GetFollowingCount:2001",
		"cache.GetFollowerCount:2001",
		"stat.Find:2001",
	})
	wantCount(t, "无快照", st.log, "cache.SetFollowingCount", 0)
	wantCount(t, "无快照", st.log, "cache.SetFollowerCount", 0)

	// 第二次读完全重复同样的链路 —— 没有任何负缓存把它止住。
	before := st.log.snapshot()
	_, err = l.Stat(&rpc.MidReq{Mid: 2001})
	wantNoErr(t, "第二次无快照", err)
	wantOps(t, "第二次无快照链路", st.log.opsFrom(before), []string{
		"cache.GetFollowingCount:2001",
		"cache.GetFollowerCount:2001",
		"stat.Find:2001",
	})
}

func TestStatWhisperIsAlwaysZero(t *testing.T) {
	e := newEnv(t)
	st := e.st
	// 库里 whisper 有值也不许透出：本期悄悄关注不实现（proto 注释固定 0）。
	seedStat(st, &model.RelationStat{Mid: 2001, Following: 4, Follower: 6, Whisper: 9, Ctime: 1_600_000_001, Mtime: 1_600_000_002})
	l := NewStatLogic(context.Background(), e.svcCtx)

	got, err := l.Stat(&rpc.MidReq{Mid: 2001})
	wantNoErr(t, "whisper 保留位", err)
	wantEQ(t, "whisper 保留位", "following", got.GetFollowing(), int64(4))
	wantEQ(t, "whisper 保留位", "follower", got.GetFollower(), int64(6))
	wantEQ(t, "whisper 保留位", "whisper", got.GetWhisper(), int32(0))
	wantEQ(t, "whisper 保留位", "库里的 whisper 未被改写", st.stats.row(2001).Whisper, int64(9))
}

func TestPropagatesStatFailures(t *testing.T) {
	cases := []struct {
		name        string
		inject      func(st *store, err error)
		wantErr     error
		wantMsg     string
		wantOps     []string
		wantNoCalls []string
	}{
		{
			name:    "关注数计数器读失败",
			inject:  func(st *store, err error) { st.cache.failWith("GetFollowingCount", err) },
			wantErr: errCache,
			wantMsg: "social-graph-test: cache unavailable",
			wantOps: []string{"cache.GetFollowingCount:2001"},
			wantNoCalls: []string{
				"cache.GetFollowerCount", "stat.Find", "cache.SetFollowingCount",
			},
		},
		{
			name: "粉丝数计数器读失败",

			inject:  func(st *store, err error) { st.cache.failWith("GetFollowerCount", err) },
			wantErr: errCache,
			wantMsg: "social-graph-test: cache unavailable",
			wantOps: []string{"cache.GetFollowingCount:2001", "cache.GetFollowerCount:2001"},
			wantNoCalls: []string{
				"stat.Find", "cache.SetFollowingCount",
			},
		},
		{
			name:    "回源查快照失败",
			inject:  func(st *store, err error) { st.stats.failWith("Find", err) },
			wantErr: errStore,
			wantMsg: "relation_stat Find: social-graph-test: store unavailable",
			wantOps: []string{
				"cache.GetFollowingCount:2001", "cache.GetFollowerCount:2001", "stat.Find:2001",
			},
			wantNoCalls: []string{"cache.SetFollowingCount", "cache.SetFollowerCount"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			seedStat(st, &model.RelationStat{Mid: 2001, Following: 1, Follower: 2, Ctime: 1_600_000_001, Mtime: 1_600_000_002})
			c.inject(st, c.wantErr)
			l := NewStatLogic(context.Background(), e.svcCtx)

			got, err := l.Stat(&rpc.MidReq{Mid: 2001})
			wantErrIs(t, c.name, err, c.wantErr)
			wantErrMessage(t, c.name, err, c.wantMsg)
			wantOps(t, c.name+"链路", st.log.ops, c.wantOps)
			if got != nil {
				t.Fatalf("%s：响应 = %+v, want nil（不得回 0/0 冒充成功）", c.name, got)
			}
			for _, frag := range c.wantNoCalls {
				wantCount(t, c.name+" 未发生的调用", st.log, frag, 0)
			}
		})
	}
}

func TestStatBackfillWriteFailuresAreSwallowed(t *testing.T) {
	cases := []struct {
		name    string
		failKey string
	}{
		{"关注数回刷 SETEX 失败", "SetFollowingCount"},
		{"粉丝数回刷 SETEX 失败", "SetFollowerCount"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			seedStat(st, &model.RelationStat{Mid: 2001, Following: 1, Follower: 2, Ctime: 1_600_000_001, Mtime: 1_600_000_002})
			st.cache.failWith(c.failKey, errCache)
			l := NewStatLogic(context.Background(), e.svcCtx)

			got, err := l.Stat(&rpc.MidReq{Mid: 2001})
			// 回填是 best-effort：失败不得影响本次读数（值来自快照）。
			wantNoErr(t, c.name, err)
			wantEQ(t, c.name, "following", got.GetFollowing(), int64(1))
			wantEQ(t, c.name, "follower", got.GetFollower(), int64(2))
			// 两条回填语句都要发过，第一条失败不短路第二条。
			wantStringsEQ(t, c.name, "回填调用", st.log.opsWith("cache.Set"), []string{
				"cache.SetFollowingCount:2001", "cache.SetFollowerCount:2001",
			})
		})
	}
}

// TestStatSeesFollowIncrement 跨方法一致性：Follow 之后不预置任何缓存，
// Stat 走「Redis 命中 + 快照回源」的混合路径，两个口径必须给出同一个关注数。
func TestStatSeesFollowIncrement(t *testing.T) {
	e := newEnv(t)
	st := e.st
	followL := NewFollowLogic(context.Background(), e.svcCtx)
	_, err := followL.Follow(&rpc.FollowReq{Mid: 2001, FollowerMid: 3002})
	wantNoErr(t, "关注", err)

	statL := NewStatLogic(context.Background(), e.svcCtx)
	got, err := statL.Stat(&rpc.MidReq{Mid: 2001})
	wantNoErr(t, "关注后查计数", err)
	fc, _ := st.cache.cachedFollowing(2001)
	wantEQ(t, "关注后查计数", "following（来自 Redis 计数器）", got.GetFollowing(), fc)
	wantEQ(t, "关注后查计数", "following 与快照一致", got.GetFollowing(), st.stats.row(2001).Following)
	wantEQ(t, "关注后查计数", "follower（2001 没被任何人关注）", got.GetFollower(), int64(0))

	// 3002 一侧：粉丝数必须落在 3002 而不是 2001 上。
	got2, err := statL.Stat(&rpc.MidReq{Mid: 3002})
	wantNoErr(t, "查被关注方计数", err)
	wantEQ(t, "查被关注方计数", "follower", got2.GetFollower(), int64(1))
	wantEQ(t, "查被关注方计数", "following", got2.GetFollowing(), int64(0))
	wantEQ(t, "关注方向", "2001 的 follower 仍是 0", st.stats.row(2001).Follower, int64(0))
	wantEQ(t, "关注方向", "3002 的 following 仍是 0", st.stats.row(3002).Following, int64(0))
}
