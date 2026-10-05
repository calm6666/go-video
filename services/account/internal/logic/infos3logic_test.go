package logic

// infos3logic_test.go 覆盖 Infos3（logic/infos3logic.go:29-38 → repository.Infos
// internal/repository/account.go:33-66 → RawInfos raw.go:36-71）。
//
// 被测判定链：mids 为空直接回空 map（一次 Redis 都不碰）→ 按**请求顺序**逐个读
// i3_<mid> → 只有未命中的 mid 被打包成一次 user-profile.Bases → 结果合并
// 「缓存命中的值 + 回源的值」→ 回源结果逐条回填。
//
// 钉住的事实：
//  1. 分组回源：缓存命中的 mid 绝不重复出现在 Bases 参数里（多打一次就是下游放大）；
//  2. 下游只回部分用户时，**不存在的 mid 不进结果、也不写缓存**（没有负缓存），
//     因此下一次调用仍会去查它；
//  3. 回填循环走的是 Go map（account.go:60-64），顺序不可断言，故那几步只比集合；
//  4. 空 mids 与 nil mids 同口径：返回的是**非 nil** 空 map，网关可以无脑 range。
//
// 覆盖不到的分支（如实声明）：`if err != nil` 不可达——RawInfos 三条 return 全为
// nil error（raw.go:38/45/54/70），Repository.Infos 唯一可能的错误源就是它；
// 另外 Infos3 **没有** mids 长度上限（与 InfosByName3 显式截断 100 不对称，README 缺口 19）。

import (
	"context"
	"testing"

	"go-video/services/account/internal/repository"
	"go-video/services/account/rpc"
)

func callInfos3(t *testing.T, e *env, mids []int64) (*rpc.InfosReply, error) {
	t.Helper()
	return NewInfos3Logic(context.Background(), e.svcCtx).Infos3(&rpc.MidsReq{Mids: mids, RealIp: "1.2.3.4"})
}

func baseOf(mid int64, name string) *repository.UserProfileBase {
	return &repository.UserProfileBase{Mid: mid, Name: name, Sex: "保密", Rank: int32(mid % 7)}
}

// TestInfos3EmptyAndNilMidsReturnEmptyMapWithoutAnyCall 空列表与 nil 都必须零调用：
// 这是网关侧「批量查资料但一个 mid 都没解析出来」的常态路径。
func TestInfos3EmptyAndNilMidsReturnEmptyMapWithoutAnyCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		mids []int64
	}{
		{"nil mids", nil},
		{"空切片", []int64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			reply, err := callInfos3(t, e, tc.mids)
			wantNoErr(t, "Infos3", err)
			if reply.GetInfos() == nil {
				t.Errorf("infos = nil, want 非 nil 空 map（调用方要能无脑 range）")
			}
			wantEQ(t, "空入参", "infos 条数", len(reply.GetInfos()), 0)
			wantEQ(t, "空入参不得产生任何调用", "调用数", len(e.ops(0)), 0)
		})
	}
}

// TestInfos3AllCachedSkipsDownstreamInRequestOrder 全命中：只按请求顺序读缓存，
// 一次 Bases 都不许发（顺序由 account.go:39 的切片遍历决定，是可断言的）。
func TestInfos3AllCachedSkipsDownstreamInRequestOrder(t *testing.T) {
	const (
		midA = int64(70001)
		midB = int64(70002)
	)
	e := newEnv(t)
	st := e.st
	infoA := &rpc.Info{Mid: midA, Name: "缓存A", Face: "https://cdn/a.png"}
	infoB := &rpc.Info{Mid: midB, Name: "缓存B", Face: "https://cdn/b.png"}
	st.cache.warmMsg(infoKey(midA), infoA, 60)
	st.cache.warmMsg(infoKey(midB), infoB, 60)
	// 请求顺序与 mid 大小相反：命中顺序必须跟着请求走，不能被排序或去重打乱。
	st.log.reset()

	reply, err := callInfos3(t, e, []int64{midB, midA})
	wantNoErr(t, "全命中的 Infos3", err)
	wantProto(t, "命中值 B", "infos[B]", reply.GetInfos()[midB], infoB)
	wantProto(t, "命中值 A", "infos[A]", reply.GetInfos()[midA], infoA)
	wantEQ(t, "全命中", "infos 条数", len(reply.GetInfos()), 2)
	wantOps(t, "全命中的调用序列", e.ops(0), []string{
		"cache.CacheInfo:" + infoKey(midB),
		"cache.CacheInfo:" + infoKey(midA),
	})
	wantNoOpsWith(t, "全命中路径", e.ops(0), "userProfile.")
	wantNoOpsWith(t, "全命中路径", e.ops(0), "cache.AddCacheInfo")
}

// TestInfos3OnlyMissingMidsGoDownstream 部分命中：Bases 的参数必须只有未命中的那个 mid，
// 回填也只能写它——这正是 repository 分组回源的全部意义。
func TestInfos3OnlyMissingMidsGoDownstream(t *testing.T) {
	const (
		midA = int64(70001) // 缓存里已有
		midB = int64(70002) // 需要回源
	)
	cachedA := &rpc.Info{Mid: midA, Name: "缓存A"}
	e := newEnv(t, withDownstream(midB, Downstream{Base: baseOf(midB, "回源B")}))
	st := e.st
	st.cache.warmMsg(infoKey(midA), cachedA, 60)
	st.log.reset()

	reply, err := callInfos3(t, e, []int64{midA, midB})
	wantNoErr(t, "部分命中的 Infos3", err)
	wantProto(t, "命中的仍取缓存", "infos[A]", reply.GetInfos()[midA], cachedA)
	wantProto(t, "未命中的取回源", "infos[B]", reply.GetInfos()[midB],
		&rpc.Info{Mid: midB, Name: "回源B", Sex: "保密", Rank: int32(midB % 7)})
	wantEQ(t, "部分命中", "infos 条数", len(reply.GetInfos()), 2)
	wantOps(t, "分组回源的调用序列", e.ops(0), []string{
		"cache.CacheInfo:" + infoKey(midA),
		"cache.CacheInfo:" + infoKey(midB),
		"userProfile.Bases:" + itoa(midB),
		"cache.AddCacheInfo:" + infoKey(midB),
	})
	// 命中的那条不许被回源结果覆盖成别的值，也不该被重复写缓存。
	got, ok := msgAs[*rpc.Info](st.cache, infoKey(midA))
	if !ok {
		t.Fatalf("前置条件破坏：A 的缓存不见了")
	}
	wantProto(t, "A 的缓存内容", "i3_A", got, cachedA)
	if ttl, ok := st.cache.ttlOf(infoKey(midA)); !ok || ttl != 60 {
		t.Errorf("A 的缓存 TTL = %d(存在=%v), want 保持 60（命中项不该被重写）", ttl, ok)
	}
}

// TestInfos3UnknownMidIsAbsentAndNotCached 下游查不到的 mid：不在结果里（logic 注释
// 「缺失的 mid 不在结果中」）、**也不写缓存**（无负缓存），因此下一次仍会去查。
func TestInfos3UnknownMidIsAbsentAndNotCached(t *testing.T) {
	const (
		midKnown   = int64(70001)
		midUnknown = int64(70002)
	)
	e := newEnv(t, withDownstream(midKnown, Downstream{Base: baseOf(midKnown, "只认识他")}))
	st := e.st

	reply, err := callInfos3(t, e, []int64{midKnown, midUnknown})
	wantNoErr(t, "含未知 mid 的 Infos3", err)
	wantEQ(t, "未知 mid 不进结果", "infos 条数", len(reply.GetInfos()), 1)
	if _, ok := reply.GetInfos()[midUnknown]; ok {
		t.Errorf("未知 mid 竟然带着零值 Info 进了结果：%v", reply.GetInfos()[midUnknown])
	}
	wantProto(t, "已知 mid 的结果", "infos[known]", reply.GetInfos()[midKnown],
		&rpc.Info{Mid: midKnown, Name: "只认识他", Sex: "保密", Rank: int32(midKnown % 7)})
	wantOps(t, "只回填查到的那条", e.ops(0), []string{
		"cache.CacheInfo:" + infoKey(midKnown),
		"cache.CacheInfo:" + infoKey(midUnknown),
		"userProfile.Bases:" + itoa(midKnown) + "," + itoa(midUnknown),
		"cache.AddCacheInfo:" + infoKey(midKnown),
	})

	// 没有负缓存：第二次仍然会带上未知 mid 去问下游；而这次查完依然不写缓存
	// （raw.go:56-70 的结果 map 里根本没有它，account.go:60 的回填循环自然空转）。
	st.log.reset()
	_, err = callInfos3(t, e, []int64{midKnown, midUnknown})
	wantNoErr(t, "第二次 Infos3", err)
	wantOps(t, "第二次 A 命中、B 仍回源但不落缓存", e.ops(0), []string{
		"cache.CacheInfo:" + infoKey(midKnown),
		"cache.CacheInfo:" + infoKey(midUnknown),
		"userProfile.Bases:" + itoa(midUnknown),
	})
	if st.cache.has(infoKey(midUnknown)) {
		t.Errorf("未知 mid 被写进了缓存（负缓存出现了）：%v", st.cache.keys())
	}
}

// TestInfos3DownstreamFaultDegradesEveryMissingMid 下游故障（未接线响铃）被吞成
// 「每个未命中 mid 都降级成 {mid}」，整批照样回填。
//
// TODO(缺陷)（README 缺口 17 的批量形态）：降级值同样带 3600 秒 TTL 进缓存，
// 一次下游抖动会把整批用户的昵称头像清空一小时。下面三条断言钉当前行为。
func TestInfos3DownstreamFaultDegradesEveryMissingMid(t *testing.T) {
	const (
		midA = int64(70001)
		midB = int64(70002)
	)
	e := newEnv(t) // 不布下游
	st := e.st

	reply, err := callInfos3(t, e, []int64{midA, midB})
	wantNoErr(t, "下游故障的 Infos3 必须降级不报错", err)
	wantProto(t, "A 降级", "infos[A]", reply.GetInfos()[midA], &rpc.Info{Mid: midA})
	wantProto(t, "B 降级", "infos[B]", reply.GetInfos()[midB], &rpc.Info{Mid: midB})
	wantOpsSet(t, "整批回源+整批回填", e.ops(0), []string{
		"cache.CacheInfo:" + infoKey(midA),
		"cache.CacheInfo:" + infoKey(midB),
		"userProfile.Bases:" + itoa(midA) + "," + itoa(midB),
		"cache.AddCacheInfo:" + infoKey(midA),
		"cache.AddCacheInfo:" + infoKey(midB),
	})
	for _, mid := range []int64{midA, midB} {
		if ttl, ok := st.cache.ttlOf(infoKey(mid)); !ok || ttl != 3600 {
			t.Errorf("降级缓存 %s 的 TTL = %d(存在=%v), want 3600", infoKey(mid), ttl, ok)
		}
	}
}

// TestInfos3UnparsableEntryTreatedAsMissingMid 缓存里解不出来的条目按未命中处理：
// 该 mid 会重新回源并被正确值覆盖（与 Info3 同一口径，走的是同一个 CacheInfo 读分支，
// 见 info3logic_test.go 的 brokenProtoBytes）。
func TestInfos3UnparsableEntryTreatedAsMissingMid(t *testing.T) {
	const (
		midA = int64(70001)
		midB = int64(70002)
	)
	e := newEnv(t, withDownstream(midA, Downstream{Base: baseOf(midA, "回源A")}))
	st := e.st
	st.cache.warmRawMsg(infoKey(midA), brokenProtoBytes, 3600)
	st.cache.warmMsg(infoKey(midB), &rpc.Info{Mid: midB, Name: "正常命中B"}, 3600)
	st.log.reset()

	reply, err := callInfos3(t, e, []int64{midA, midB})
	wantNoErr(t, "含坏值的 Infos3", err)
	wantProto(t, "坏值按 miss 回源", "infos[A]", reply.GetInfos()[midA],
		&rpc.Info{Mid: midA, Name: "回源A", Sex: "保密", Rank: int32(midA % 7)})
	wantProto(t, "正常命中仍走缓存", "infos[B]", reply.GetInfos()[midB],
		&rpc.Info{Mid: midB, Name: "正常命中B"})
	wantOps(t, "只有坏值那条回源", e.ops(0), []string{
		"cache.CacheInfo:" + infoKey(midA),
		"cache.CacheInfo:" + infoKey(midB),
		"userProfile.Bases:" + itoa(midA),
		"cache.AddCacheInfo:" + infoKey(midA),
	})
	if _, ok := msgAs[*rpc.Info](st.cache, infoKey(midA)); !ok {
		t.Errorf("坏值没被覆盖：%s 仍解不出合法 *rpc.Info", infoKey(midA))
	}
}
