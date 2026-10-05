package logic

// profilewithstat3logic_test.go 覆盖 ProfileWithStat3
// （logic/profilewithstat3logic.go:29-45 → repository.ProfileWithStat internal/repository/privacy.go:67-87）。
//
// 被测判定链：ProfileWithStat = Profile（p3_ 缓存 → 四路聚合）+ LevelExp + Stat 三段拼接，
// 顺序固定：
//
//	cache.CacheProfile:p3_<mid> → userProfile.Member → userProfile.RealnameStatus
//	→ account.FindOne → cred.FindByMid → cache.AddCacheProfile
//	→ userProfile.LevelExp → socialGraph.Stat
//
// 钉住的事实：
//  1. 只有 profile 段有缓存，等级与关注/粉丝数**每次都回源**（privacy.go:72/77 没有缓存层），
//     所以「刚取关立刻刷新」是对的，而「刚改资料看不到」是 p3_ 那侧的行为；
//  2. 三段彼此独立降级：LevelExp 挂只清 level_info（raw.go:232-234），
//     Stat 挂只清 following/follower（relation.go:112-114），
//     Member 挂只丢下游字段（本地 account/credential 事实仍在），全程 err 恒为 nil；
//  3. coins 恒为 0（privacy.go:85 硬编码 + AGENTS.md §1 商业化范围外），
//     且这不是「忘了填」——它是显式写死的，所以布了 level/stat 也必须为 0；
//  4. ProfileWithStat3 与 Profile3 共用同一个 p3_ 键（repository 层同一方法），
//     一条链的降级值会被另一条链读到，见
//     TestProfileWithStat3ReadsProfile3DegradedCache 与 README 缺口 17。
//
// 覆盖不到的分支（如实声明）：`err != nil`（profilewithstat3logic.go:31-34）、
// `reply == nil`（35-37）、`reply.Profile == nil`（38-40）、`reply.LevelInfo == nil`
// （41-43）四支全部不可达 —— RawProfile 唯一 return 是 (profile, nil)（raw.go:207），
// LevelExp/Stat 在 repository 内部就把错误吞成零值（raw.go:232-234、relation.go:112-114），
// 见 README 缺口 18。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/account/internal/repository"
	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func callProfileWithStat3(t *testing.T, e *env, mid int64) (*rpc.ProfileStatReply, error) {
	t.Helper()
	return NewProfileWithStat3Logic(context.Background(), e.svcCtx).ProfileWithStat3(&rpc.MidReq{Mid: mid, RealIp: "1.2.3.4"})
}

// levelFixture / statFixture 是「下游真的返回数据」的布景值。
// level_info.cur 故意与 fullMember 的 Level: 6 **不同**（这里是 7）：
// profile.level 与 level_info.cur 都来自 user-profile 但是两根不同的 RPC，
// 取同值会让「填错根线」的用例照样通过。
func levelFixture() *rpc.LevelInfo {
	return &rpc.LevelInfo{Cur: 7, Min: 1500, NowExp: 1600, NextExp: 3000}
}

func statFixture() *repository.RelationStat {
	return &repository.RelationStat{Following: 21, Follower: 3}
}

// TestProfileWithStat3MergesThreeStagesInFixedOrder 冷缓存的完整聚合：三段顺序、
// 字段映射、p3_ 回填（且只回填 p3_）一起钉。
func TestProfileWithStat3MergesThreeStagesInFixedOrder(t *testing.T) {
	const mid = int64(70001)
	e := newEnv(t, withDownstream(mid, Downstream{
		Member:   fullMember(mid),
		Realname: i32(1),
		Level:    levelFixture(),
		Stat:     statFixture(),
	}))
	st := e.st
	st.account.put(&model.Account{Mid: mid, Status: 0, CreatedAt: 1700000000})
	st.cred.put(phoneCred(mid, 0))
	st.log.reset()

	wantProfile := &rpc.Profile{
		Mid: mid, Name: "阿莉", Sex: "女", Face: "https://cdn/f.png", Sign: "签名", Rank: 5,
		Level: 6, Birthday: 631152000, Moral: 70, JoinTime: 1700000000,
		TelStatus: 1, Identification: 1,
		Official: &rpc.OfficialInfo{Role: 1, Title: "官方账号", Desc: "认证说明"},
	}
	reply, err := callProfileWithStat3(t, e, mid)
	wantNoErr(t, "ProfileWithStat3", err)
	wantOps(t, "Profile→LevelExp→Stat 三段固定顺序", e.ops(0), []string{
		"cache.CacheProfile:" + profileKey(mid),
		"userProfile.Member:" + itoa(mid),
		"userProfile.RealnameStatus:" + itoa(mid),
		"account.FindOne:" + itoa(mid),
		"cred.FindByMid:" + itoa(mid),
		"cache.AddCacheProfile:" + profileKey(mid),
		"userProfile.LevelExp:" + itoa(mid),
		"socialGraph.Stat:" + itoa(mid),
	})
	wantProto(t, "profile 段", "profile", reply.GetProfile(), wantProfile)
	wantProto(t, "level_info 段", "level_info", reply.GetLevelInfo(), levelFixture())
	wantEQ(t, "following", "值", reply.GetFollowing(), int64(21))
	wantEQ(t, "follower", "值", reply.GetFollower(), int64(3))
	// AGENTS.md §1：硬币属商业化范围外，privacy.go:85 写死 0，布了下游也不会变。
	wantEQ(t, "coins", "值", reply.GetCoins(), float64(0))
	// 回填的只有 p3_：等级与统计没有任何缓存键。
	got, ok := msgAs[*rpc.Profile](st.cache, profileKey(mid))
	if !ok {
		t.Fatalf("回填没落到 %s（当前 key：%v）", profileKey(mid), st.cache.keys())
	}
	wantProto(t, "回填内容", profileKey(mid), got, wantProfile)
	for _, k := range st.cache.keys() {
		if k != profileKey(mid) {
			t.Errorf("ProfileWithStat 多写了缓存键 %q，只应写 %q", k, profileKey(mid))
		}
	}
}

// TestProfileWithStat3CachedProfileStillRefetchesLevelAndStat 缓存命中只短路 profile 段，
// 等级/关注数照旧回源（它们没有缓存层）。这是与 Profile3 的关键差别。
func TestProfileWithStat3CachedProfileStillRefetchesLevelAndStat(t *testing.T) {
	const mid = int64(70001)
	cached := &rpc.Profile{Mid: mid, Name: "缓存里的名字", Level: 9}
	e := newEnv(t, withDownstream(mid, Downstream{Level: levelFixture(), Stat: statFixture()}))
	st := e.st
	st.cache.warmMsg(profileKey(mid), cached, 11)
	st.account.put(&model.Account{Mid: mid, Status: 1, CreatedAt: 999999}) // 命中路径不该看到这些
	st.cred.put(phoneCred(mid, 0))
	st.log.reset()

	reply, err := callProfileWithStat3(t, e, mid)
	wantNoErr(t, "ProfileWithStat3", err)
	wantOps(t, "命中只跳 profile 段", e.ops(0), []string{
		"cache.CacheProfile:" + profileKey(mid),
		"userProfile.LevelExp:" + itoa(mid),
		"socialGraph.Stat:" + itoa(mid),
	})
	wantProto(t, "profile 来自缓存", "profile", reply.GetProfile(), cached)
	wantProto(t, "level_info 来自实时下游", "level_info", reply.GetLevelInfo(), levelFixture())
	wantEQ(t, "following 来自实时下游", "值", reply.GetFollowing(), int64(21))
	if ttl, ok := st.cache.ttlOf(profileKey(mid)); !ok || ttl != 11 {
		t.Errorf("命中路径改了 p3_ TTL：%d(存在=%v), want 保持 11", ttl, ok)
	}
	// 本地表贡献的 silence/tel_status 也不会「补写」进已缓存的资料里。
	wantEQ(t, "缓存资料里的 silence", "值", reply.GetProfile().GetSilence(), int32(0))
}

// TestProfileWithStat3DownstreamLegsDegradeIndependently 三条腿各断一条，
// 另外两条必须原样存在 —— 谁吞掉谁的错误都会让这一组子用例变红。
func TestProfileWithStat3DownstreamLegsDegradeIndependently(t *testing.T) {
	const mid = int64(70001)
	boom := errors.New("account/test: 下游不可用")
	cases := []struct {
		name          string
		failLevel     bool
		failStat      bool
		failMember    bool
		wantName      string
		wantLevelCur  int32
		wantFollowing int64
	}{
		{name: "只有等级失败", failLevel: true, wantName: "阿莉", wantLevelCur: 0, wantFollowing: 21},
		{name: "只有统计失败", failStat: true, wantName: "阿莉", wantLevelCur: 7, wantFollowing: 0},
		{name: "只有资料失败", failMember: true, wantName: "", wantLevelCur: 7, wantFollowing: 21},
		{name: "三条腿全失败", failLevel: true, failStat: true, failMember: true, wantName: "", wantLevelCur: 0, wantFollowing: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, withDownstream(mid, Downstream{
				Member: fullMember(mid), Realname: i32(1), Level: levelFixture(), Stat: statFixture(),
			}))
			st := e.st
			st.account.put(&model.Account{Mid: mid, Status: 1, IsTourist: 1, CreatedAt: 1600000000})
			st.cred.put(phoneCred(mid, 0))
			if tc.failLevel {
				st.userProfile.failWith("LevelExp", boom)
			}
			if tc.failStat {
				st.socialGraph.failWith("Stat", boom)
			}
			if tc.failMember {
				st.userProfile.failWith("Member", boom)
			}
			st.log.reset()

			reply, err := callProfileWithStat3(t, e, mid)
			wantNoErr(t, "ProfileWithStat3 任何一路下游失败都必须降级不报错", err)
			wantEQ(t, "profile.name", "值", reply.GetProfile().GetName(), tc.wantName)
			wantEQ(t, "level_info.cur", "值", reply.GetLevelInfo().GetCur(), tc.wantLevelCur)
			wantEQ(t, "following", "值", reply.GetFollowing(), tc.wantFollowing)
			if tc.failStat {
				wantEQ(t, "follower", "值", reply.GetFollower(), int64(0))
			} else {
				wantEQ(t, "follower", "值", reply.GetFollower(), int64(3))
			}
			// profile.level 走 Member 线（6），level_info.cur 走 LevelExp 线（7）：
			// 两根线取值不同，任何一根填到对方字段上都会在这里红。
			if tc.failMember {
				wantEQ(t, "profile.level（Member 线已失败）", "值", reply.GetProfile().GetLevel(), int32(0))
			} else {
				wantEQ(t, "profile.level（Member 线）", "值", reply.GetProfile().GetLevel(), int32(6))
			}
			// 本地事实不随下游失败而丢（Member 失败那一支尤其要看到 silence/tel）。
			wantEQ(t, "profile.silence（本地 account 表）", "值", reply.GetProfile().GetSilence(), int32(1))
			wantEQ(t, "profile.tel_status（本地 credential 表）", "值", reply.GetProfile().GetTelStatus(), int32(1))
			wantEQ(t, "profile.is_tourist（本地 account 表）", "值", reply.GetProfile().GetIsTourist(), int32(1))
			wantEQ(t, "coins 恒为 0", "值", reply.GetCoins(), float64(0))
			// 三条腿都必须在轨迹里被尝试过：吞错误不等于不调用。
			wantCount(t, "等级回源次数", e.ops(0), "userProfile.LevelExp:", 1)
			wantCount(t, "统计回源次数", e.ops(0), "socialGraph.Stat:", 1)
		})
	}
}

// TestProfileWithStat3ReadsProfile3DegradedCache 两条链共用 p3_ 键：
// Profile3 在下游故障时写进去的残缺资料，会被 ProfileWithStat3 当命中读到，
// 且此后一小时两条链都只能看到这份残值。
//
// TODO(缺陷)（README 缺口 17）：降级值照样带 3600 秒 TTL 回填
// （raw.go:18-21 吞错 + account.go:138-142 无条件回填）。此处钉住**当前**行为：
// 只要 Profile3 先跑过一次故障回源，ProfileWithStat3 就连 Member 都不再尝试。
func TestProfileWithStat3ReadsProfile3DegradedCache(t *testing.T) {
	const mid = int64(70001)
	e := newEnv(t, withDownstream(mid, Downstream{
		Member: fullMember(mid), Realname: i32(1), Level: levelFixture(), Stat: statFixture(),
	}))
	st := e.st
	st.account.put(&model.Account{Mid: mid, Status: 0, CreatedAt: 1600000000})
	st.log.reset()

	// 第一跳：Profile3 在 Member 故障期间回源，残缺资料被写进 p3_。
	st.userProfile.failWith("Member", errors.New("account/test: user-profile 宕机"))
	profileReply, err := callProfile3(t, e, mid)
	wantNoErr(t, "Profile3 降级", err)
	wantEQ(t, "第一跳拿到的 name", "值", profileReply.GetProfile().GetName(), "")
	st.userProfile.clearFaults() // 下游已经恢复
	firstOps := len(e.ops(0))

	// 第二跳：ProfileWithStat3 恢复后仍读到第一跳留下的残值。
	reply, err := callProfileWithStat3(t, e, mid)
	wantNoErr(t, "ProfileWithStat3", err)
	wantOps(t, "第二跳只剩 profile 段命中后的三步", e.ops(firstOps), []string{
		"cache.CacheProfile:" + profileKey(mid),
		"userProfile.LevelExp:" + itoa(mid),
		"socialGraph.Stat:" + itoa(mid),
	})
	wantEQ(t, "恢复后仍看不到昵称（缺口 17 的后果）", "值", reply.GetProfile().GetName(), "")
	wantEQ(t, "等级不受影响（无缓存层）", "值", reply.GetLevelInfo().GetCur(), int32(7))
	wantEQ(t, "粉丝数不受影响（无缓存层）", "值", reply.GetFollower(), int64(3))
	if ttl, ok := st.cache.ttlOf(profileKey(mid)); !ok || ttl != 3600 {
		t.Errorf("残缺资料的 TTL = %d(存在=%v), want 3600（缺口 17 的量化口径）", ttl, ok)
	}
}

// TestProfileWithStat3MidZeroAndOneUseDifferentCacheKeys 与 Profile3 同口径：
// mid 不校验，p3_0 / p3_1 是两条独立缓存，下游也按各自 mid 各查一次。
func TestProfileWithStat3MidZeroAndOneUseDifferentCacheKeys(t *testing.T) {
	e := newEnv(t)
	e.st.cache.warmMsg(profileKey(0), &rpc.Profile{Mid: 0, Name: "游客零号"}, 11)
	e.st.log.reset()

	reply, err := callProfileWithStat3(t, e, 1)
	wantNoErr(t, "ProfileWithStat3 mid=1", err)
	wantProto(t, "mid=1 不能吃到 p3_0 的值", "profile", reply.GetProfile(), &rpc.Profile{Mid: 1})
	wantNoOpsWith(t, "mid=1 的调用轨迹", e.ops(0), "cache.CacheProfile:"+profileKey(0))
	wantOps(t, "mid=1 只查 p3_1", e.ops(0), []string{
		"cache.CacheProfile:" + profileKey(1),
		"userProfile.Member:1",
		"userProfile.RealnameStatus:1",
		"account.FindOne:1",
		"cred.FindByMid:1",
		"cache.AddCacheProfile:" + profileKey(1),
		"userProfile.LevelExp:1",
		"socialGraph.Stat:1",
	})

	// 反向：mid=0 必须命中 p3_0，且绝不能顺手写 p3_1。
	before := len(e.ops(0))
	reply0, err := callProfileWithStat3(t, e, 0)
	wantNoErr(t, "ProfileWithStat3 mid=0", err)
	wantEQ(t, "mid=0 命中预置值", "值", reply0.GetProfile().GetName(), "游客零号")
	wantNoOpsWith(t, "mid=0 的调用轨迹", e.ops(before), "cache.AddCacheProfile:"+profileKey(1))
}
