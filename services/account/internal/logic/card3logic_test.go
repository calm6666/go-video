package logic

// card3logic_test.go 覆盖 Card3（logic/card3logic.go:29-39 → repository.Card
// internal/repository/account.go:69-86 → RawCard raw.go:76-102）。
//
// 被测判定链：读 c3_<mid> → 命中即返回缓存值（不回源）→ 未命中只调 **一次**
// user-profile.Member → 搬 7 个字段（mid/name/sex/face/sign/rank/level）+ official
// → 回填 c3_<mid>。会员/挂件/勋章三个字段按 AGENTS.md §1 一律不填；
// silence 也**从不填**（见 TestCard3SilenceNeverReflectsLocalBan 与 README 缺口 24）。
//
// 钉住的事实：
//  1. Card3 的数据面只有 user-profile.Member：不读 RealnameStatus、不读本地
//     account / account_credential、不读 social-graph（名片是公开面，越界就是越权）；
//  2. Member 失败或回空（mid 不存在）都退化成「只有 mid 的名片」，不报错
//     （raw.go:81-84、raw.go:85-87）；
//  3. 退化值同样带 3600 秒 TTL 进缓存（缺口 17 的 Card 形态）；
//  4. profile.level/birthday/moral 里后两个不进名片；
//  5. 名片的 mid 取的是下游回的 mid（raw.go:88），缓存键却是请求的 mid（缺口 20 同形）。
//
// 覆盖不到的分支（如实声明）：
//   - `err != nil`（card3logic.go:31-34）与 `card == nil`（35-37）不可达：
//     RawCard 的三条 return 全是非 nil + nil error，Repository.Card 也照原样透出
//     （README 缺口 18）；
//   - `r.userProfile == nil`（raw.go:77-79）不可达：注入缝 always 装一个下游替身，
//     生产上只有配置里没有 UserProfileRpc 时才会为 nil（属 svc 装配面，不在本包）。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/account/internal/repository"
	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func callCard3(t *testing.T, e *env, mid int64) (*rpc.CardReply, error) {
	t.Helper()
	return NewCard3Logic(context.Background(), e.svcCtx).Card3(&rpc.MidReq{Mid: mid, RealIp: "1.2.3.4"})
}

// TestCard3CacheHitSkipsDownstream 命中即短路：Member 一次都不许多调。
func TestCard3CacheHitSkipsDownstream(t *testing.T) {
	const mid = int64(70001)
	cached := &rpc.Card{Mid: mid, Name: "缓存里的昵称", Level: 9, Sign: "缓存签名"}
	e := newEnv(t)
	st := e.st
	st.cache.warmMsg(cardKey(mid), cached, 11)
	st.log.reset()

	reply, err := callCard3(t, e, mid)
	wantNoErr(t, "缓存命中的 Card3", err)
	wantProto(t, "命中的名片", "card", reply.GetCard(), cached)
	wantOps(t, "命中只该读一次缓存", e.ops(0), []string{"cache.CacheCard:" + cardKey(mid)})
	wantNoOpsWith(t, "命中路径", e.ops(0), "userProfile.")
	if ttl, ok := st.cache.ttlOf(cardKey(mid)); !ok || ttl != 11 {
		t.Errorf("命中路径改了 TTL：%d(存在=%v), want 保持 11", ttl, ok)
	}
}

// TestCard3CacheMissMapsMemberFieldsAndLeavesCommercialFieldsNil 未命中的字段映射：
// 只搬 7 个字段 + official，商业化三字段与 silence 一律不填，且只调 Member 一根线。
func TestCard3CacheMissMapsMemberFieldsAndLeavesCommercialFieldsNil(t *testing.T) {
	const mid = int64(70001)
	e := newEnv(t, withDownstream(mid, Downstream{Member: fullMember(mid), Realname: i32(1)}))
	st := e.st
	st.account.put(&model.Account{Mid: mid, Status: 1, IsTourist: 1, CreatedAt: 1700000000})
	st.cred.put(phoneCred(mid, 0))
	st.log.reset()

	want := &rpc.Card{
		Mid: mid, Name: "阿莉", Sex: "女", Face: "https://cdn/f.png", Sign: "签名",
		Rank: 5, Level: 6,
		Official: &rpc.OfficialInfo{Role: 1, Title: "官方账号", Desc: "认证说明"},
		// Silence/Vip/Pendant/Nameplate 都不填：前者是缺口 24，后三者是 AGENTS.md §1。
	}
	reply, err := callCard3(t, e, mid)
	wantNoErr(t, "Card3", err)
	wantProto(t, "名片字段映射", "card", reply.GetCard(), want)
	wantOps(t, "只读缓存 + 只调 Member + 回填", e.ops(0), []string{
		"cache.CacheCard:" + cardKey(mid),
		"userProfile.Member:" + itoa(mid),
		"cache.AddCacheCard:" + cardKey(mid),
	})
	// 名片的数据面：实名状态、本地两张表、关系统计都不许被顺手带上。
	for _, needle := range []string{
		"userProfile.RealnameStatus", "userProfile.Base", "userProfile.Bases",
		"account.Find", "cred.Find", "socialGraph.",
	} {
		wantNoOpsWith(t, "Card3 的调用轨迹", e.ops(0), needle)
	}
	got, ok := msgAs[*rpc.Card](st.cache, cardKey(mid))
	if !ok {
		t.Fatalf("回填没落到 %s（当前 key：%v）", cardKey(mid), st.cache.keys())
	}
	wantProto(t, "回填内容", cardKey(mid), got, want)
	if ttl, ok := st.cache.ttlOf(cardKey(mid)); !ok || ttl != 3600 {
		t.Errorf("回填 TTL = %d(存在=%v), want 3600", ttl, ok)
	}
}

// TestCard3UnknownMidBecomesMidOnlyCardAndIsCached 下游接线但这个 mid 查不到：
// 适配器口径是 (nil, nil)（raw.go:85-87），名片退化成「只剩 mid」。
func TestCard3UnknownMidBecomesMidOnlyCardAndIsCached(t *testing.T) {
	const (
		known   = int64(70001)
		unknown = int64(70002)
	)
	e := newEnv(t, withDownstream(known, Downstream{Member: fullMember(known)}))
	st := e.st
	st.log.reset()

	reply, err := callCard3(t, e, unknown)
	wantNoErr(t, "查无此人不得报错", err)
	wantProto(t, "只剩 mid 的名片", "card", reply.GetCard(), &rpc.Card{Mid: unknown})
	wantOps(t, "回源一次即回填", e.ops(0), []string{
		"cache.CacheCard:" + cardKey(unknown),
		"userProfile.Member:" + itoa(unknown),
		"cache.AddCacheCard:" + cardKey(unknown),
	})
	// 已布数据的 known 不受牵连：它的名片不会被这次请求改写。
	if st.cache.has(cardKey(known)) {
		t.Errorf("查 unknown 却写了 %s", cardKey(known))
	}
	// 第二跳：unknown 的残值被当真值命中（缺口 17 的 Card 形态）。
	st.log.reset()
	reply2, err := callCard3(t, e, unknown)
	wantNoErr(t, "第二次 Card3", err)
	wantProto(t, "残值被复用", "card", reply2.GetCard(), &rpc.Card{Mid: unknown})
	wantOps(t, "命中后不再回源", e.ops(0), []string{"cache.CacheCard:" + cardKey(unknown)})
}

// TestCard3DownstreamFaultDegradesToMidOnlyCardAndCachesIt user-profile 不可用时：
// 应答只剩 mid，不报错，而且这份残值带满 TTL 进缓存。
//
// TODO(缺陷)（README 缺口 17 的 Card 形态）：raw.go:79-84 吞掉 Member 的错误后
// 照样回填（account.go:80-85 无条件 AddCacheCard），于是一次下游抖动会让「查不到人」
// 在名片上固化一小时。
func TestCard3DownstreamFaultDegradesToMidOnlyCardAndCachesIt(t *testing.T) {
	const mid = int64(70001)
	e := newEnv(t, withDownstream(mid, Downstream{Member: fullMember(mid)}))
	st := e.st
	st.userProfile.failWith("Member", errors.New("account/test: user-profile 宕机"))
	st.log.reset()

	reply, err := callCard3(t, e, mid)
	wantNoErr(t, "下游故障的 Card3 必须降级不报错", err)
	wantProto(t, "只剩 mid 的降级名片", "card", reply.GetCard(), &rpc.Card{Mid: mid})
	wantOps(t, "故障照样走完回源 + 回填", e.ops(0), []string{
		"cache.CacheCard:" + cardKey(mid),
		"userProfile.Member:" + itoa(mid),
		"cache.AddCacheCard:" + cardKey(mid),
	})
	got, ok := msgAs[*rpc.Card](st.cache, cardKey(mid))
	if !ok {
		t.Fatalf("前提已变：降级名片竟然没进缓存，请复核 account.go:80-85")
	}
	wantProto(t, "进缓存的降级名片", cardKey(mid), got, &rpc.Card{Mid: mid})
	if ttl, ok := st.cache.ttlOf(cardKey(mid)); !ok || ttl != 3600 {
		t.Errorf("降级值 TTL = %d(存在=%v), want 3600 —— 缺口 17 的影响窗口长度正是它", ttl, ok)
	}
	// 下游恢复后，命中路径仍然不会去问 Member：残值占住了缓存。
	st.userProfile.clearFaults()
	st.log.reset()
	_, err = callCard3(t, e, mid)
	wantNoErr(t, "恢复后的第二次 Card3", err)
	wantOps(t, "降级值已占住缓存", e.ops(0), []string{"cache.CacheCard:" + cardKey(mid)})
}

// TestCard3SilenceNeverReflectsLocalBan 同一个禁言账号（account.status=1）：
// Profile3 报 silence=1，Card3 永远报 silence=0 —— 名片上的禁言标识形同虚设。
//
// TODO(缺陷)（README 缺口 24）：rpc/account.proto:16 给 Card 定义了 silence 字段，
// 但 RawCard（raw.go:86-101）压根不读本地 account 表（只有 RawProfile 在
// raw.go:179-188 做这件事）。客户端若按名片的 silence 决定「能不能发弹幕/评论」，
// 被禁言用户会看到自己能发言。此处钉住**当前**行为并留下对照。
func TestCard3SilenceNeverReflectsLocalBan(t *testing.T) {
	const mid = int64(70001)
	newSeededEnv := func(t *testing.T) *env {
		e := newEnv(t, withDownstream(mid, Downstream{Member: fullMember(mid), Realname: i32(0)}))
		e.st.account.put(&model.Account{Mid: mid, Status: 1}) // status=1 即禁言，见 Profile3 口径
		return e
	}

	cardReply, err := callCard3(t, newSeededEnv(t), mid)
	wantNoErr(t, "Card3", err)
	wantEQ(t, "名片的 silence（不读本地表）", "值", cardReply.GetCard().GetSilence(), int32(0))

	profileReply, err := callProfile3(t, newSeededEnv(t), mid)
	wantNoErr(t, "Profile3", err)
	wantEQ(t, "同一个人资料的 silence=1（证明布景真的生效）", "值",
		profileReply.GetProfile().GetSilence(), int32(1))

	// 名片链路确实没读 account 表 —— 这不是「读了但条件写错」，而是根本没接。
	e := newSeededEnv(t)
	e.st.log.reset()
	_, err = callCard3(t, e, mid)
	wantNoErr(t, "Card3", err)
	wantNoOpsWith(t, "Card3 的调用轨迹", e.ops(0), "account.FindOne")
}

// TestCard3AdoptsDownstreamMidUnderRequestedKey 与 Profile3 同形：
// card.mid 取下游回的 mid（raw.go:88），缓存键却是请求的 mid（account.go:83）。
//
// TODO(缺陷)（README 缺口 20）：下游串号时，A 的名片会挂在 B 的缓存键上供一小时，
// 且响应 mid 与请求 mid 不一致，调用方无从发现。此处钉住当前无校验的行为。
func TestCard3AdoptsDownstreamMidUnderRequestedKey(t *testing.T) {
	const (
		requested = int64(70001)
		returned  = int64(999)
	)
	e := newEnv(t, withDownstream(requested, Downstream{
		Member: &repository.UserProfileMember{
			UserProfileBase: repository.UserProfileBase{Mid: returned, Name: "串号的名片"},
		},
	}))
	st := e.st
	st.log.reset()

	reply, err := callCard3(t, e, requested)
	wantNoErr(t, "Card3", err)
	wantEQ(t, "名片 mid 跟着下游跑", "值", reply.GetCard().GetMid(), returned)
	got, ok := msgAs[*rpc.Card](st.cache, cardKey(requested))
	if !ok {
		t.Fatalf("前提已变：%s 没被写（当前 key：%v）", cardKey(requested), st.cache.keys())
	}
	wantEQ(t, "请求键下挂着的 mid（键与载荷不一致）", "值", got.GetMid(), returned)
	wantOps(t, "回源只用请求 mid 查下游", e.ops(0), []string{
		"cache.CacheCard:" + cardKey(requested),
		"userProfile.Member:" + itoa(requested),
		"cache.AddCacheCard:" + cardKey(requested),
	})
}

// TestCard3MidZeroAndOneUseDifferentCacheKeys mid 不校验：c3_0 与 c3_1 各自独立，
// 且下游也按各自 mid 各问一次。
func TestCard3MidZeroAndOneUseDifferentCacheKeys(t *testing.T) {
	for _, mid := range []int64{0, 1} {
		t.Run(itoa(mid), func(t *testing.T) {
			e := newEnv(t, withDownstream(mid, Downstream{
				Member: &repository.UserProfileMember{
					UserProfileBase: repository.UserProfileBase{Mid: mid, Name: "mid" + itoa(mid)},
				},
			}))
			reply, err := callCard3(t, e, mid)
			wantNoErr(t, "Card3", err)
			wantProto(t, "响应", "card", reply.GetCard(), &rpc.Card{Mid: mid, Name: "mid" + itoa(mid)})
			wantOps(t, "缓存键按 mid 派生", e.ops(0), []string{
				"cache.CacheCard:" + cardKey(mid),
				"userProfile.Member:" + itoa(mid),
				"cache.AddCacheCard:" + cardKey(mid),
			})
		})
	}
}
