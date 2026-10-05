package logic

// info3logic_test.go 覆盖 Info3（logic/info3logic.go:29-39 → repository.Info
// internal/repository/account.go:12-30 → RawInfo raw.go:13-33 → cache.go:113-146）。
//
// 被测判定链：读 i3_<mid> → 命中即**直接返回缓存值、不回源、不重写 TTL**；
// 未命中（含读故障与解不出来的坏字节，两者都被吞成 miss）才调 user-profile.Base，
// 拿到结果后回填 i3_<mid>，TTL 固定 cacheExpireSeconds=3600。
// 回填在测试装配里是同步执行器（repository.NewWithDeps 的 syncRunner），
// 生产是 fanout 入队异步执行：内容一致、时序不同，因此本文件的序列断言只对
// 「哪些步骤发生了、彼此先后」负责，不代表生产同一毫秒的交错。
//
// 钉住的事实：
//  1. Info3 只做「透传 + nil 兜底」，**不校验 mid**：mid=0 与 mid=1 是两条不同的缓存键；
//  2. 键上解不出来的字节按 miss 处理并被回源结果覆盖（cache.go:126-129）；
//     但**跨类型的合法字节不会被认出来**——Card 的 1/2 号字段与 Info 同为
//     (varint, string)，会被当成一次正常命中供出一小时
//     （README 缺口 23，哨兵用例 TestInfo3CrossTypeCachedBytesAreServedAsValidHit）；
//  3. 下游故障/用户不存在时返回的降级值 {mid} 会**照样带 3600 秒 TTL 进缓存**，
//     user-profile 恢复后最长 1 小时内本接口继续回空昵称（README 缺口 17，哨兵用例
//     TestInfo3DownstreamFaultDegradesButPoisonsCache）。
//
// 覆盖不到的分支（如实声明）：`if err != nil` 那一支不可达——RawInfo 的四条 return
// 全部返回 nil error（raw.go:15/20/23/32），Repository.Info 唯一可能外传的错误来自
// RawInfo，因此它是防御性死分支，不是遗漏的必测路径（同口径见 README 缺口 18）。

import (
	"context"
	"testing"

	"go-video/services/account/internal/repository"
	"go-video/services/account/rpc"
)

func callInfo3(t *testing.T, e *env, mid int64) (*rpc.InfoReply, error) {
	t.Helper()
	return NewInfo3Logic(context.Background(), e.svcCtx).Info3(&rpc.MidReq{Mid: mid, RealIp: "1.2.3.4"})
}

// TestInfo3CacheHitReturnsCachedValueWithoutDownstream 命中即短路：一步都不许多走，
// 也不许碰缓存的 TTL（读路径写成 Setex 会让热 key 永不过期）。
func TestInfo3CacheHitReturnsCachedValueWithoutDownstream(t *testing.T) {
	const mid = int64(70001)
	cached := &rpc.Info{Mid: mid, Name: "缓存里的昵称", Sex: "男", Face: "https://cdn/cached.png", Sign: "缓存签名", Rank: 42}
	e := newEnv(t)
	st := e.st
	st.cache.warmMsg(infoKey(mid), cached, 7) // TTL 故意不是 3600：命中路径不该动它
	st.log.reset()

	reply, err := callInfo3(t, e, mid)
	wantNoErr(t, "缓存命中的 Info3", err)
	wantProto(t, "命中的响应", "info", reply.GetInfo(), cached)
	wantOps(t, "命中只该读一次缓存", e.ops(0), []string{"cache.CacheInfo:" + infoKey(mid)})
	wantNoOpsWith(t, "命中路径", e.ops(0), "userProfile.")
	wantNoOpsWith(t, "命中路径", e.ops(0), "cache.AddCacheInfo")
	if ttl, ok := st.cache.ttlOf(infoKey(mid)); !ok || ttl != 7 {
		t.Errorf("命中路径改了缓存 TTL：ttl=%d(存在=%v), want 保持布景值 7", ttl, ok)
	}

	// 纪律 1：从缓存读出来的是值拷贝，调用方改回复不许污染库存。
	reply.Info.Name = "被调用方改掉了"
	got, ok := msgAs[*rpc.Info](st.cache, infoKey(mid))
	if !ok {
		t.Fatalf("前置条件破坏：i3_ 缓存不见了")
	}
	wantEQ(t, "缓存库存", "name", got.Name, "缓存里的昵称")
}

// TestInfo3CacheMissFallsBackToUserProfileAndBackfills 未命中 → 回源 → 回填，
// 并检查回填的**内容与过期时间**（只断言「调过 AddCacheInfo」不足以证明写对了值）。
func TestInfo3CacheMissFallsBackToUserProfileAndBackfills(t *testing.T) {
	const mid = int64(70001)
	base := &repository.UserProfileBase{Mid: mid, Name: "真昵称", Sex: "女", Face: "https://cdn/real.png", Sign: "真签名", Rank: 7}
	want := &rpc.Info{Mid: mid, Name: "真昵称", Sex: "女", Face: "https://cdn/real.png", Sign: "真签名", Rank: 7}
	e := newEnv(t, withDownstream(mid, Downstream{Base: base}))
	st := e.st

	reply, err := callInfo3(t, e, mid)
	wantNoErr(t, "回源 Info3", err)
	wantProto(t, "回源结果按 Base 六字段映射", "info", reply.GetInfo(), want)
	wantOps(t, "回源+回填的调用序列", e.ops(0), []string{
		"cache.CacheInfo:" + infoKey(mid),
		"userProfile.Base:" + itoa(mid),
		"cache.AddCacheInfo:" + infoKey(mid),
	})

	got, ok := msgAs[*rpc.Info](st.cache, infoKey(mid))
	if !ok {
		t.Fatalf("回填没有落到 i3_ 键（当前缓存 key 列表 %v）", st.cache.keys())
	}
	wantProto(t, "回填内容", "i3_"+itoa(mid), got, want)
	if ttl, ok := st.cache.ttlOf(infoKey(mid)); !ok || ttl != 3600 {
		t.Errorf("回填 TTL = %d(存在=%v), want 3600（cache.go:24 cacheExpireSeconds）", ttl, ok)
	}

	// 回填真的生效：第二次调用即命中，不再打下游。
	st.log.reset()
	second, err := callInfo3(t, e, mid)
	wantNoErr(t, "第二次 Info3", err)
	wantProto(t, "第二次的响应", "info", second.GetInfo(), want)
	wantOps(t, "第二次只该读缓存", e.ops(0), []string{"cache.CacheInfo:" + infoKey(mid)})
}

// TestInfo3DownstreamFaultDegradesButPoisonsCache 下游故障（此处为「未接线」响铃）
// 被 Repository 吞掉、降级成 {mid}，查询照常成功——这一半是设计（AGENTS.md §5 不因下游
// 不可用而打死读接口）。
//
// TODO(缺陷)（README 缺口 17）：另一半不是设计——降级值也带 3600 秒 TTL 写进缓存
// （raw.go:18-21 返回非 nil 降级值 + account.go:24-28 只要 raw != nil 就回填），
// 于是 user-profile 抖动一次，这个 mid 就有一小时拿不到昵称头像。
// 下面三条断言钉住**当前**行为；修好后这三条会红，届时应把期望改成「不回填降级值」。
func TestInfo3DownstreamFaultDegradesButPoisonsCache(t *testing.T) {
	const mid = int64(70001)
	e := newEnv(t) // 故意不布下游：Base 返回 errDownstreamWired
	st := e.st
	degraded := &rpc.Info{Mid: mid}

	reply, err := callInfo3(t, e, mid)
	wantNoErr(t, "下游故障必须降级而不是报错", err)
	wantProto(t, "降级值只剩 mid", "info", reply.GetInfo(), degraded)
	wantOps(t, "下游故障的调用序列", e.ops(0), []string{
		"cache.CacheInfo:" + infoKey(mid),
		"userProfile.Base:" + itoa(mid),
		"cache.AddCacheInfo:" + infoKey(mid),
	})

	got, ok := msgAs[*rpc.Info](st.cache, infoKey(mid))
	if !ok {
		t.Fatalf("前提已变：降级值竟然没进缓存，请复核 raw.go:18-21 与 account.go:24-28 后改写本用例")
	}
	wantProto(t, "进了缓存的内容", "i3_"+itoa(mid), got, degraded)
	if ttl, ok := st.cache.ttlOf(infoKey(mid)); !ok || ttl != 3600 {
		t.Errorf("降级缓存 TTL = %d(存在=%v), want 3600 —— 缺口 17 的影响窗口长度正是它", ttl, ok)
	}

	// 后果：下一次连下游都不查了，降级值被当真值命中。
	st.log.reset()
	_, err = callInfo3(t, e, mid)
	wantNoErr(t, "第二次 Info3", err)
	wantOps(t, "降级值已占住缓存", e.ops(0), []string{"cache.CacheInfo:" + infoKey(mid)})
}

// brokenProtoBytes 是一坨**必然解不出来**的 protobuf：tag 0x12 声明 field 2 是
// 长度前缀字段、长度 5，但后面只剩 1 字节 → proto.Unmarshal 一定报
// "cannot parse invalid wire-format data"，生产的 Cache* 于是按 miss 处理
// （cache.go:126-129）。
//
// 为什么不用「另一个消息类型的合法字节」当坏值：那样解得出来（见
// TestInfo3CrossTypeCachedBytesAreServedAsValidHit），断言的就不是坏值分支了。
var brokenProtoBytes = []byte{0x12, 0x05, 'a'}

// TestInfo3UnparsableCachedBytesTreatedAsMissAndOverwritten 缓存键下是一坨解不出来的
// 字节（线上等价于值被截断/写坏）：必须按 miss 回源并覆盖，不能报错，
// 也不能把坏值一直留在键上。
func TestInfo3UnparsableCachedBytesTreatedAsMissAndOverwritten(t *testing.T) {
	const mid = int64(70001)
	base := &repository.UserProfileBase{Mid: mid, Name: "回源昵称"}
	e := newEnv(t, withDownstream(mid, Downstream{Base: base}))
	st := e.st
	st.cache.warmRawMsg(infoKey(mid), brokenProtoBytes, 3600)
	st.log.reset()

	reply, err := callInfo3(t, e, mid)
	wantNoErr(t, "坏值不得把查询打死", err)
	wantProto(t, "坏值按 miss 回源", "info", reply.GetInfo(), &rpc.Info{Mid: mid, Name: "回源昵称"})
	wantOps(t, "坏值回源的调用序列", e.ops(0), []string{
		"cache.CacheInfo:" + infoKey(mid),
		"userProfile.Base:" + itoa(mid),
		"cache.AddCacheInfo:" + infoKey(mid),
	})
	got, ok := msgAs[*rpc.Info](st.cache, infoKey(mid))
	if !ok {
		t.Fatalf("坏值没被覆盖：%s 仍解不出合法 *rpc.Info", infoKey(mid))
	}
	wantProto(t, "覆盖后的缓存值", infoKey(mid), got, &rpc.Info{Mid: mid, Name: "回源昵称"})
}

// TestInfo3CrossTypeCachedBytesAreServedAsValidHit 缓存键下挂的是**别的消息类型**的
// 合法字节（线上等价于 Redis 实例被复用、或字段号漂移）：
// 生产的 CacheInfo 只把字节 proto.Unmarshal 进 *rpc.Info，不看任何类型标签，
// 而 Card 与 Info 的 1/2 号字段恰好同为 (varint, string) ——于是坏值被当成一次
// 正常命中，昵称直接来自名片那条消息，且一小时不外发一次 RPC。
//
// TODO(缺陷)（README 缺口 23）：cache.go:119-133 的读法没有任何「这是不是 Info」的
// 校验（既没有类型前缀也没有 schema 版本号），跨类型/演进后的字节只要 wire type
// 对得上就会以真值面貌供出一小时。此处钉住**当前**行为（不是认可它）。
func TestInfo3CrossTypeCachedBytesAreServedAsValidHit(t *testing.T) {
	const mid = int64(70001)
	base := &repository.UserProfileBase{Mid: mid, Name: "回源昵称"}
	e := newEnv(t, withDownstream(mid, Downstream{Base: base}))
	st := e.st
	st.cache.warmMsg(infoKey(mid), &rpc.Card{Mid: mid, Name: "串号的名片"}, 3600)
	st.log.reset()

	reply, err := callInfo3(t, e, mid)
	wantNoErr(t, "串号值不得把查询打死", err)
	wantProto(t, "串号值被当真值命中", "info", reply.GetInfo(), &rpc.Info{Mid: mid, Name: "串号的名片"})
	wantOps(t, "命中后不回源、不覆盖", e.ops(0), []string{"cache.CacheInfo:" + infoKey(mid)})
	// 键上的字节仍是那串 Card（没有任何纠正动作）。
	got, ok := msgAs[*rpc.Card](st.cache, infoKey(mid))
	if !ok {
		t.Fatalf("前提已变：i3_%d 不再是能按 Card 解出来的字节", mid)
	}
	wantProto(t, "未被纠正的坏值", infoKey(mid), got, &rpc.Card{Mid: mid, Name: "串号的名片"})
}

// TestInfo3MidZeroAndOneUseDifferentCacheKeys mid=0 与 mid=1 的边界对：
// Info3 不做任何入参校验，0 号也照样查、照样回填，且缓存键按 mid 逐字派生。
// （若实现漏了「mid 非正即拒」以外的任何短路，比如把 0 当成「未指定」直接回空响应，
// 这一轮的 i3_0 序列断言与 name 断言都会红。）
func TestInfo3MidZeroAndOneUseDifferentCacheKeys(t *testing.T) {
	for _, mid := range []int64{0, 1} {
		t.Run(itoa(mid), func(t *testing.T) {
			name := "mid" + itoa(mid)
			base := &repository.UserProfileBase{Mid: mid, Name: name}
			e := newEnv(t, withDownstream(mid, Downstream{Base: base}))

			reply, err := callInfo3(t, e, mid)
			wantNoErr(t, "Info3", err)
			wantProto(t, "响应", "info", reply.GetInfo(), &rpc.Info{Mid: mid, Name: name})
			wantOps(t, "缓存键必须按 mid 派生", e.ops(0), []string{
				"cache.CacheInfo:i3_" + itoa(mid),
				"userProfile.Base:" + itoa(mid),
				"cache.AddCacheInfo:i3_" + itoa(mid),
			})
		})
	}
}
