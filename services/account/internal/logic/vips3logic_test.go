package logic

// vips3logic_test.go 覆盖 Vips3（logic/vips3logic.go:29-49 → repository.Vips
// internal/repository/account.go:169-203 → RawVips raw.go:218-224）。
//
// 被测判定链：按**请求顺序**逐个读 v3_<mid> → 把未命中的 mid 收进 miss 切片 →
// miss 非空才走 RawVips（恒返回每个 mid 一个 &rpc.VipInfo{}，会员属 AGENTS.md §1
// 商业化范围外）→ 合并进结果 → 按 raw map 回填 v3_<mid> →
// logic 层再把 VipInfo 的 4 个字段搬成 VipReply。
//
// 钉住的事实：
//  1. 空/nil 的 mids 一个缓存键都不碰（len(mids)==0 的短路，account.go:170-172）；
//  2. 读阶段严格按请求顺序、写阶段按 map 迭代顺序（所以写用 wantOpsSet）；
//  3. 全程零下游、零本地表：会员批量查询不许顺手打 user-profile / social-graph / MySQL；
//  4. 重复 mid 会被读两次、但只写一次（raw 是 map，重复键折叠）——
//     这同时证明实现没有对 mids 去重，见 README 缺口 19（Infos3/Vips3 都没有长度上限）；
//  5. 结果里每个请求过的 mid 都有条目（零值也要有），不会因为「下游没这个人」而缺键；
//  6. 零值回填的字节是空串 → 批量会员缓存同样永远读不出命中（缺口 22 的批量形态）。
//
// 覆盖不到的分支（如实声明）：`err != nil`（vips3logic.go:30-34）与 `v == nil`
// （37-40）都不可达 —— RawVips 恒返回 (每个 mid 一个非 nil 零值, nil)（raw.go:218-224），
// Vips 的两条出口也是非 nil 值，见 README 缺口 18。

import (
	"context"
	"testing"

	"go-video/services/account/rpc"
)

func callVips3(t *testing.T, e *env, mids ...int64) (*rpc.VipsReply, error) {
	t.Helper()
	return NewVips3Logic(context.Background(), e.svcCtx).Vips3(&rpc.MidsReq{Mids: mids, RealIp: "1.2.3.4"})
}

// TestVips3EmptyAndNilMidsReturnEmptyMapWithoutAnyCall 空数组与 nil 都要短路：
// 一次缓存都不许碰（少了 len==0 判断的实现会在这里红）。
func TestVips3EmptyAndNilMidsReturnEmptyMapWithoutAnyCall(t *testing.T) {
	t.Run("空数组", func(t *testing.T) {
		e := newEnv(t)
		e.st.log.reset()
		reply, err := callVips3(t, e)
		wantNoErr(t, "Vips3 空 mids", err)
		if got := reply.GetVips(); got == nil || len(got) != 0 {
			t.Errorf("空 mids 的 vips = %v, want 非 nil 空 map", got)
		}
		wantOps(t, "空 mids 不该有任何调用", e.ops(0), nil)
	})
	t.Run("nil mids", func(t *testing.T) {
		e := newEnv(t)
		e.st.log.reset()
		var mids []int64
		reply, err := NewVips3Logic(context.Background(), e.svcCtx).Vips3(&rpc.MidsReq{Mids: mids})
		wantNoErr(t, "Vips3 nil mids", err)
		if got := reply.GetVips(); got == nil || len(got) != 0 {
			t.Errorf("nil mids 的 vips = %v, want 非 nil 空 map", got)
		}
		wantOps(t, "nil mids 不该有任何调用", e.ops(0), nil)
	})
}

// TestVips3AllCachedSkipsRawVipsAndWritesNothing 全命中时不回填、不写任何键。
func TestVips3AllCachedSkipsRawVipsAndWritesNothing(t *testing.T) {
	const (
		midA = int64(70001)
		midB = int64(70002)
	)
	e := newEnv(t)
	st := e.st
	infoA := &rpc.VipInfo{Type: 2, Status: 1, DueDate: 1893456000, VipPayType: 1}
	infoB := &rpc.VipInfo{Type: 1, Status: 1, DueDate: 1793456000, VipPayType: 2}
	st.cache.warmMsg(vipKey(midA), infoA, 60)
	st.cache.warmMsg(vipKey(midB), infoB, 60)
	st.log.reset()

	// 故意按 B、A 的顺序请求：读阶段必须跟着请求顺序走，而不是键的字典序。
	reply, err := callVips3(t, e, midB, midA)
	wantNoErr(t, "Vips3 全命中", err)
	wantOps(t, "按请求顺序各读一次、不回填", e.ops(0), []string{
		"cache.CacheVip:" + vipKey(midB),
		"cache.CacheVip:" + vipKey(midA),
	})
	wantProto(t, "vips[B] 原样透传", "vips[B]", reply.GetVips()[midB],
		&rpc.VipReply{Type: 1, Status: 1, DueDate: 1793456000, VipPayType: 2})
	wantProto(t, "vips[A] 原样透传", "vips[A]", reply.GetVips()[midA],
		&rpc.VipReply{Type: 2, Status: 1, DueDate: 1893456000, VipPayType: 1})
	for _, k := range []string{vipKey(midA), vipKey(midB)} {
		if ttl, ok := st.cache.ttlOf(k); !ok || ttl != 60 {
			t.Errorf("命中路径改了 %s 的 TTL：%d(存在=%v), want 保持 60", k, ttl, ok)
		}
	}
}

// TestVips3OnlyMissingMidsAreReadFromRawAndBackfilled 混合命中的分组、合并与回填，
// 同时钉住「会员批量查询不碰任何下游/本地表」。
func TestVips3OnlyMissingMidsAreReadFromRawAndBackfilled(t *testing.T) {
	const (
		midA = int64(70001) // 已缓存
		midB = int64(70002) // miss
		midC = int64(70003) // miss
	)
	e := newEnv(t) // 一个下游都不布
	st := e.st
	st.cache.warmMsg(vipKey(midA), &rpc.VipInfo{Type: 2, DueDate: 111}, 60)
	st.log.reset()

	reply, err := callVips3(t, e, midA, midB, midC)
	wantNoErr(t, "Vips3", err)
	ops := e.ops(0)
	wantOps(t, "读阶段按请求顺序", ops[:3], []string{
		"cache.CacheVip:" + vipKey(midA),
		"cache.CacheVip:" + vipKey(midB),
		"cache.CacheVip:" + vipKey(midC),
	})
	// 写阶段走 raw map 的迭代顺序（Go 的 map 顺序随机），只能按集合断言。
	wantOpsSet(t, "只回填两个未命中的 mid", ops[3:], []string{
		"cache.AddCacheVip:" + vipKey(midB),
		"cache.AddCacheVip:" + vipKey(midC),
	})
	wantCount(t, "已命中的 A 不该被重写", ops, "cache.AddCacheVip:"+vipKey(midA), 0)
	// 每个请求过的 mid 都得有条目：A 是缓存值，B/C 是零值。
	if got := len(reply.GetVips()); got != 3 {
		t.Errorf("vips 条目数 = %d, want 3（%v）", got, reply.GetVips())
	}
	wantProto(t, "vips[A]", "vips[A]", reply.GetVips()[midA], &rpc.VipReply{Type: 2, DueDate: 111})
	wantProto(t, "vips[B] 零值也要占一个键", "vips[B]", reply.GetVips()[midB], &rpc.VipReply{})
	wantProto(t, "vips[C] 零值也要占一个键", "vips[C]", reply.GetVips()[midC], &rpc.VipReply{})
	// 依赖面：会员批量查询不许顺手打下游或本地表。
	for _, needle := range []string{"userProfile.", "socialGraph.", "account.Find", "cred.Find"} {
		wantNoOpsWith(t, "Vips3 的调用轨迹", ops, needle)
	}
}

// TestVips3DuplicateMidsAreReadTwiceButWrittenOnce 重复 mid 的边界对：
// 读阶段没有去重（同一键读两次），写阶段因为结果是 map 而只落一次。
// 少了任何一侧的口径，序列长度就会变。
func TestVips3DuplicateMidsAreReadTwiceButWrittenOnce(t *testing.T) {
	const mid = int64(70002)
	e := newEnv(t)
	e.st.log.reset()

	reply, err := callVips3(t, e, mid, mid)
	wantNoErr(t, "Vips3 重复 mid", err)
	wantOps(t, "读两次、写一次", e.ops(0), []string{
		"cache.CacheVip:" + vipKey(mid),
		"cache.CacheVip:" + vipKey(mid),
		"cache.AddCacheVip:" + vipKey(mid),
	})
	if got := len(reply.GetVips()); got != 1 {
		t.Errorf("vips 条目数 = %d, want 1（重复 mid 折叠成一个键）：%v", got, reply.GetVips())
	}
	wantCount(t, "回填次数", e.ops(0), "cache.AddCacheVip:", 1)
}

// TestVips3ZeroValueBackfillNeverServesNextCall 缺口 22 的批量形态：
// RawVips 给的零值编码成空串，于是第二次批量查询仍然全员 miss、仍然全员再写一遍。
//
// TODO(缺陷)（README 缺口 22）：与 Vip3 同因（raw.go:218-224 + cache.go:122-124）。
// 会员没接真数据源之前，这条链每次请求都要发 len(mids)+len(miss) 条 Redis 命令，
// 且没有任何一次能由缓存服务。此处钉住**当前**行为（不影响应答正确性）。
func TestVips3ZeroValueBackfillNeverServesNextCall(t *testing.T) {
	const (
		midA = int64(70001)
		midB = int64(70002)
	)
	e := newEnv(t)
	st := e.st
	st.log.reset()

	for i := 1; i <= 2; i++ {
		_, err := callVips3(t, e, midA, midB)
		wantNoErr(t, "Vips3", err)
		wantCount(t, "第 "+itoa(int64(i))+" 轮读次数", e.ops(0), "cache.CacheVip:", 2*i)
		wantCount(t, "第 "+itoa(int64(i))+" 轮写次数", e.ops(0), "cache.AddCacheVip:", 2*i)
	}
	for _, mid := range []int64{midA, midB} {
		bs, ok := st.cache.msgBytesOf(vipKey(mid))
		if !ok || len(bs) != 0 {
			t.Errorf("%s 应为「键在、值为空串」，实得 (%v, 存在=%v)", vipKey(mid), bs, ok)
		}
	}
	// 对照：能编码出非空字节的值就真的会被复用（证明上面不是「替身永远读不出来」）。
	st.cache.warmMsg(vipKey(midA), &rpc.VipInfo{Type: 1, Status: 1}, 60)
	before := len(e.ops(0))
	reply, err := callVips3(t, e, midA, midB)
	wantNoErr(t, "Vips3 对照轮", err)
	wantOpsSet(t, "A 命中、B 仍 miss", e.ops(before), []string{
		"cache.CacheVip:" + vipKey(midA),
		"cache.CacheVip:" + vipKey(midB),
		"cache.AddCacheVip:" + vipKey(midB),
	})
	wantProto(t, "A 由缓存供出真值", "vips[A]", reply.GetVips()[midA], &rpc.VipReply{Type: 1, Status: 1})
}

// TestVips3MidZeroAndOneUseDifferentCacheKeys v3_0 上的真值不得服务 mid=1，
// 反之 mid=1 的回填也不得动 v3_0 的载荷与 TTL。
func TestVips3MidZeroAndOneUseDifferentCacheKeys(t *testing.T) {
	e := newEnv(t)
	st := e.st
	st.cache.warmMsg(vipKey(0), &rpc.VipInfo{Type: 1, Status: 1, DueDate: 100, VipPayType: 1}, 60)
	st.log.reset()

	reply, err := callVips3(t, e, 0, 1)
	wantNoErr(t, "Vips3", err)
	wantOps(t, "两个 mid 各自一条键", e.ops(0), []string{
		"cache.CacheVip:" + vipKey(0),
		"cache.CacheVip:" + vipKey(1),
		"cache.AddCacheVip:" + vipKey(1),
	})
	wantProto(t, "mid=0 命中", "vips[0]", reply.GetVips()[0],
		&rpc.VipReply{Type: 1, Status: 1, DueDate: 100, VipPayType: 1})
	wantProto(t, "mid=1 零值", "vips[1]", reply.GetVips()[1], &rpc.VipReply{})
	got, ok := msgAs[*rpc.VipInfo](st.cache, vipKey(0))
	if !ok {
		t.Fatalf("v3_0 的布景被破坏了（当前 key：%v）", st.cache.keys())
	}
	wantProto(t, "v3_0 未被 mid=1 覆盖", "v3_0", got, &rpc.VipInfo{Type: 1, Status: 1, DueDate: 100, VipPayType: 1})
	if ttl, ok := st.cache.ttlOf(vipKey(0)); !ok || ttl != 60 {
		t.Errorf("v3_0 的 TTL 被改：%d(存在=%v), want 保持 60", ttl, ok)
	}
}
