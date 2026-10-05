package logic

// delcachelogic_test.go 覆盖 DelCache（logic/delcachelogic.go:30-36 →
// repository.DelCache internal/repository/cache_delay.go:28-35 → reWarm 同文件 39-54
// → cache.DelCache internal/repository/cache.go:287-296）。
//
// 被测判定链：action=="updateVip" 时先入 5 秒延迟队列 → 删该 mid 的
// i3_/c3_/v3_/p3_ 四个键 → 立即回温（Info→Card→Profile→Vip 的固定顺序）→
// 把 repository 收集的 []error **只写进日志**，然后回一个空的 DelCacheReply。
//
// 钉住的事实：
//  1. 本方法是 15 个里唯一「返回值里带 error 列表却被丢弃」的：delcachelogic.go:32-34
//     只 l.Errorf，第 35 行照样 `return &rpc.DelCacheReply{}, nil`；
//     TestDelCacheSwallowsRepositoryErrors 同时直调 Repository 证明那 4 条错误
//     确实被交到了 logic 手上（不是 repository 就没报）；
//  2. 失效失败时**回温也读不到新值**（CacheInfo 命中旧值即短路），
//     于是「调用方拿到 200、缓存里还是旧资料」是同一个故障的两面；
//  3. 回温顺序 = Info→Card→Profile→Vip（cache_delay.go:41-52 的书写顺序），
//     与 cache.DelCache 的删键顺序（i3_,c3_,v3_,p3_，cache.go:288）**不一致**：
//     Profile 在 Vip 之前删、却在 Vip 之前回温，Vip 的键序与回温序不同；
//  4. 只有 action 精确等于 "updateVip" 才入延迟队列（cache_delay.go:29 的 ==），
//     大小写/前后空格变体都不入队；二次失效由 DrainDelayedCache 驱动（本注入缝不启协程）；
//  5. mid 不校验（mid=0 会真的去删 i3_0 等键并回温）。
//
// 覆盖不到的分支（如实声明）：
//   - 生产的 cacheDelayProc 协程（cache_delay.go:58-69）按 5 秒 ticker 触发，
//     单测不启协程，改用 DrainDelayedCache(now) 以任意 now 驱动同一份消费逻辑。

import (
	"context"
	"errors"
	"testing"
	"time"

	"go-video/services/account/rpc"
)

func callDelCache(t *testing.T, e *env, mid int64, action string) (*rpc.DelCacheReply, error) {
	t.Helper()
	return NewDelCacheLogic(context.Background(), e.svcCtx).DelCache(&rpc.DelCacheReq{Mid: mid, Action: action})
}

const delMid = int64(70001)

// delCacheKeysInDeleteOrder 复刻 cache.go:288 的删键顺序（注意 Vip 在 Profile 之前）。
func delCacheKeysInDeleteOrder(mid int64) string {
	return infoKey(mid) + "," + cardKey(mid) + "," + vipKey(mid) + "," + profileKey(mid)
}

// reWarmOps 复刻 cache_delay.go:41-52 的回温序列在替身上应有的轨迹
// （user-profile 已注入但没布数据 → 每条都降级成只剩 mid 的残值并回填）。
func reWarmOps(mid int64) []string {
	m := itoa(mid)
	return []string{
		"cache.CacheInfo:" + infoKey(mid),
		"userProfile.Base:" + m,
		"cache.AddCacheInfo:" + infoKey(mid),
		"cache.CacheCard:" + cardKey(mid),
		"userProfile.Member:" + m,
		"cache.AddCacheCard:" + cardKey(mid),
		"cache.CacheProfile:" + profileKey(mid),
		"userProfile.Member:" + m,
		"userProfile.RealnameStatus:" + m,
		"account.FindOne:" + m,
		"cred.FindByMid:" + m,
		"cache.AddCacheProfile:" + profileKey(mid),
		"cache.CacheVip:" + vipKey(mid),
		"cache.AddCacheVip:" + vipKey(mid),
	}
}

// TestDelCacheDeletesThenRewarmsInOrder 正常路径：先删四个键，再按
// Info→Card→Profile→Vip 回温，整条序列 15 步固定不变。
func TestDelCacheDeletesThenRewarmsInOrder(t *testing.T) {
	e := newEnv(t)
	st := e.st
	st.cache.warmMsg(infoKey(delMid), &rpc.Info{Mid: delMid, Name: "旧昵称"}, 999)
	st.log.reset()

	reply, err := callDelCache(t, e, delMid, "")
	wantNoErr(t, "DelCache", err)
	if reply == nil {
		t.Fatalf("reply = nil, want 非 nil 空答复")
	}
	wantProto(t, "答复是空消息", "DelCacheReply", reply, &rpc.DelCacheReply{})

	want := append([]string{"cache.DelCache:" + delCacheKeysInDeleteOrder(delMid)}, reWarmOps(delMid)...)
	wantOps(t, "删键 + 回温的完整序列", e.ops(0), want)

	// 删键真的生效了：旧昵称被回温的降级残值覆盖。
	got, ok := msgAs[*rpc.Info](st.cache, infoKey(delMid))
	if !ok {
		t.Fatalf("回温没写 %s（当前 key：%v）", infoKey(delMid), st.cache.keys())
	}
	wantProto(t, "回温写回的是降级残值", infoKey(delMid), got, &rpc.Info{Mid: delMid})
	if ttl, ok := st.cache.ttlOf(infoKey(delMid)); !ok || ttl != 3600 {
		t.Errorf("回温后 TTL = %d(存在=%v), want 3600（布景值 999 必须被覆盖）", ttl, ok)
	}
	// 回温出来的 Vip 是零值消息 → 编码成空串 → 读回来仍是 miss（缺口 22 同形）。
	if bs, ok := st.cache.msgBytesOf(vipKey(delMid)); !ok || len(bs) != 0 {
		t.Errorf("前提已变：v3_ 回填字节长度 = %d(存在=%v), want 0（零值消息编成空串）", len(bs), ok)
	}
}

// TestDelCacheSwallowsRepositoryErrors Redis 删除失败时，repository 把 4 条错误
// 原样交给 logic，logic 却只写日志并回 200。
//
// TODO(缺陷)（本批缺口 27）：delcachelogic.go:31-35 —— errs 非空也 `return &rpc.DelCacheReply{}, nil`。
// 这个方法是 user-profile 等**资料变更方**在写完库后调用来失效缓存的；一旦 Redis 抖动，
// 调用方以为失效成功，而旧资料还会按 cacheExpireSeconds 活最长一小时，
// 且这次故障只在服务端日志里（logx.Disable 后测试进程里连日志都没有）。
// 此处钉住**当前**行为，不改生产代码。
func TestDelCacheSwallowsRepositoryErrors(t *testing.T) {
	const mid = int64(70001)
	boom := errors.New("account/test: redis DEL 失败")

	// 先把「repository 确实会上报错误」钉住：否则 logic 的吞错无从谈起。
	e0 := newRawStore(withoutUserProfile())
	e0.cache.failWith("DelCache", boom)
	errs := e0.repo.DelCache(context.Background(), mid, "")
	if len(errs) != 4 {
		t.Fatalf("Repository.DelCache 返回 %d 条错误, want 4（四个键各一条）—— 前提已变，请复核 cache_delay.go:32", len(errs))
	}
	if !errors.Is(errs[0], boom) {
		t.Errorf("上报的错误 = %v, want 原始故障 %v", errs[0], boom)
	}

	// 同一故障下走 logic：答复与成功路径完全同形。
	e := newEnv(t, withoutUserProfile())
	st := e.st
	st.cache.warmMsg(infoKey(mid), &rpc.Info{Mid: mid, Name: "旧昵称"}, 999)
	st.cache.warmMsg(cardKey(mid), &rpc.Card{Mid: mid, Name: "旧名片"}, 999)
	st.cache.warmMsg(profileKey(mid), &rpc.Profile{Mid: mid, Name: "旧资料"}, 999)
	st.cache.warmMsg(vipKey(mid), &rpc.VipInfo{Type: 2, DueDate: 888}, 999)
	st.cache.failWith("DelCache", boom)
	st.log.reset()

	reply, err := callDelCache(t, e, mid, "")
	wantNoErr(t, "logic 不得把删除失败外传（这是被钉住的当前行为）", err)
	wantProto(t, "失败与成功的答复同形", "DelCacheReply", reply, &rpc.DelCacheReply{})
	wantNotContains(t, "故障原文不得进答复", reply.String(), boom.Error())

	// 删除没生效 → 回温的每一步都「命中旧值即短路」，只读不写：
	// 旧资料原封不动地留在缓存里，且没人知道。
	wantOps(t, "删除失败时回温全程命中旧值、一次都不回填", e.ops(0), append(
		[]string{"cache.DelCache:" + delCacheKeysInDeleteOrder(mid)},
		"cache.CacheInfo:"+infoKey(mid),
		"cache.CacheCard:"+cardKey(mid),
		"cache.CacheProfile:"+profileKey(mid),
		"cache.CacheVip:"+vipKey(mid),
	))
	stored, ok := msgAs[*rpc.Info](st.cache, infoKey(mid))
	if !ok {
		t.Fatalf("前提已变：%s 竟然不在了", infoKey(mid))
	}
	wantProto(t, "旧值仍被当成有效缓存", infoKey(mid), stored, &rpc.Info{Mid: mid, Name: "旧昵称"})
	if ttl, ok := st.cache.ttlOf(infoKey(mid)); !ok || ttl != 999 {
		t.Errorf("旧值 TTL = %d(存在=%v), want 仍是布景的 999（说明这一小时窗口没被重置）", ttl, ok)
	}
	// 四个键一个都没掉：会员残影同样还在（旧资料/旧名片/旧会员/旧简介全都在）。
	vip, ok := msgAs[*rpc.VipInfo](st.cache, vipKey(mid))
	if !ok {
		t.Fatalf("前提已变：%s 竟然被删掉了", vipKey(mid))
	}
	wantProto(t, "会员旧值仍在缓存", vipKey(mid), vip, &rpc.VipInfo{Type: 2, DueDate: 888})
	card, ok := msgAs[*rpc.Card](st.cache, cardKey(mid))
	if !ok {
		t.Fatalf("前提已变：%s 竟然被删掉了", cardKey(mid))
	}
	wantProto(t, "名片旧值仍在缓存", cardKey(mid), card, &rpc.Card{Mid: mid, Name: "旧名片"})
}

// TestDelCacheUpdateVipEnqueuesSecondInvalidation action=updateVip 入 5 秒延迟队列：
// 立即这次先做「删 + 回温」，到期后再做一次，且第二次**不再入队**（不成环）。
func TestDelCacheUpdateVipEnqueuesSecondInvalidation(t *testing.T) {
	e := newEnv(t, withoutUserProfile())
	st := e.st
	st.log.reset()
	now := time.Now()

	reply, err := callDelCache(t, e, delMid, "updateVip")
	wantNoErr(t, "DelCache(updateVip)", err)
	if reply == nil {
		t.Fatalf("reply = nil")
	}
	first := e.ops(0)
	wantOps(t, "updateVip 的立即那次与非 updateVip 相同", first,
		append([]string{"cache.DelCache:" + delCacheKeysInDeleteOrder(delMid)},
			reWarmOpsNoProfile(delMid)...))

	// 4.99 秒时还没到期：不许偷跑。
	st.log.reset()
	st.repo.DrainDelayedCache(now.Add(4990 * time.Millisecond))
	wantOps(t, "未到期不得二次失效", e.ops(0), nil)

	// 5 秒整：弹出并二次失效（这次是 cache_delay.go:84-88 的同一套动作）。
	st.log.reset()
	st.repo.DrainDelayedCache(now.Add(5 * time.Second))
	second := e.ops(0)
	wantOps(t, "到期后二次失效 + 回温", second,
		append([]string{"cache.DelCache:" + delCacheKeysInDeleteOrder(delMid)},
			reWarmOpsNoProfile(delMid)...))
	wantCount(t, "二次失效只删一次键", second, "cache.DelCache:", 1)

	// 队列已空：再排一次什么都不做。
	st.log.reset()
	st.repo.DrainDelayedCache(now.Add(time.Hour))
	wantOps(t, "队列已排空", e.ops(0), nil)
}

// reWarmOpsNoProfile 是 user-profile 未注入时的回温序列（raw.go:14-16、77-79、153-155
// 都不问下游，只 Profile 仍读本地两张表）。用它同时证明
// 「DelCache 的回温不依赖 user-profile 是否配好」。
func reWarmOpsNoProfile(mid int64) []string {
	m := itoa(mid)
	return []string{
		"cache.CacheInfo:" + infoKey(mid),
		"cache.AddCacheInfo:" + infoKey(mid),
		"cache.CacheCard:" + cardKey(mid),
		"cache.AddCacheCard:" + cardKey(mid),
		"cache.CacheProfile:" + profileKey(mid),
		"account.FindOne:" + m,
		"cred.FindByMid:" + m,
		"cache.AddCacheProfile:" + profileKey(mid),
		"cache.CacheVip:" + vipKey(mid),
		"cache.AddCacheVip:" + vipKey(mid),
	}
}

// TestDelCacheActionMustMatchExactly 只有字面量 "updateVip" 入队（cache_delay.go:29 的 ==）：
// 大小写、空格、空串、其它动作名都只失效一次、不入队。
// 反向哨兵：如果实现改成 strings.EqualFold 或 TrimSpace，本用例会红。
func TestDelCacheActionMustMatchExactly(t *testing.T) {
	for _, action := range []string{"", "UpdateVip", "updatevip", "UPDATEVIP", " updateVip", "updateVip ", "update_vip", "vip"} {
		t.Run("action="+action, func(t *testing.T) {
			e := newEnv(t, withoutUserProfile())
			st := e.st
			st.log.reset()

			_, err := callDelCache(t, e, delMid, action)
			wantNoErr(t, "DelCache", err)
			wantCount(t, "立即这次删一遍键", e.ops(0), "cache.DelCache:", 1)

			st.log.reset()
			st.repo.DrainDelayedCache(time.Now().Add(24 * time.Hour))
			wantOps(t, "变体动作不得入延迟队列", e.ops(0), nil)
		})
	}
}

// TestDelCacheMidZeroNotValidated mid=0 不校验：真的去删 i3_0 等四个键并回温。
// 若上游（网关/user-profile）把「未登录」的 0 传进来，就会白刷一轮缓存。
func TestDelCacheMidZeroNotValidated(t *testing.T) {
	e := newEnv(t, withoutUserProfile())
	st := e.st
	st.log.reset()

	_, err := callDelCache(t, e, 0, "")
	wantNoErr(t, "DelCache(mid=0)", err)
	wantOps(t, "mid=0 照删照回温", e.ops(0),
		append([]string{"cache.DelCache:i3_0,c3_0,v3_0,p3_0"}, reWarmOpsNoProfile(0)...))
}

// TestDelCacheOnlyAffectsTargetMid 失效范围只有该 mid 的四个键：
// 别人的 i3_/c3_/v3_/p3_ 一律不动。
func TestDelCacheOnlyAffectsTargetMid(t *testing.T) {
	const (
		target = delMid
		other  = int64(70002)
	)
	e := newEnv(t, withoutUserProfile())
	st := e.st
	for _, key := range []string{infoKey(other), cardKey(other), vipKey(other), profileKey(other)} {
		st.cache.warmRawMsg(key, []byte{0xff, 0xff}, 42) // 坏值：只用于「有没有被动过」
	}
	st.log.reset()

	_, err := callDelCache(t, e, target, "")
	wantNoErr(t, "DelCache", err)
	for _, key := range []string{infoKey(other), cardKey(other), vipKey(other), profileKey(other)} {
		bs, ok := st.cache.msgBytesOf(key)
		if !ok || len(bs) != 2 || bs[0] != 0xff {
			t.Errorf("%s 被误删/覆写了（当前字节 %v 存在=%v）", key, bs, ok)
		}
		if ttl, ok := st.cache.ttlOf(key); !ok || ttl != 42 {
			t.Errorf("%s 的 TTL 被改动：%d(存在=%v), want 42", key, ttl, ok)
		}
	}
	wantNoOpsWith(t, "失效范围", e.ops(0), infoKey(other))
	wantNoOpsWith(t, "失效范围", e.ops(0), cardKey(other))
}
