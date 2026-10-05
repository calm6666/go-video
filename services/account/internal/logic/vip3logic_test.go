package logic

// vip3logic_test.go 覆盖 Vip3（logic/vip3logic.go:30-45 → repository.Vip
// internal/repository/account.go:148-166 → RawVip raw.go:212-214 →
// Cache/AddCacheVip cache.go:241-273）。
//
// 被测判定链：读 v3_<mid> → 命中则把缓存里 VipInfo 的 4 个字段原样搬进 VipReply →
// 未命中回源（RawVip 恒返回 &rpc.VipInfo{}，会员属 AGENTS.md §1 商业化范围外）→
// 回填 v3_<mid> → 同样搬 4 个字段。
//
// 钉住的事实：
//  1. Vip3 全程**只碰缓存**：不调 user-profile、不调 social-graph、不读任何本地表
//     （会员一旦哪天要接真数据源，必须先改这里，用例会在轨迹断言上红）；
//  2. logic 层是纯字段搬运：不做「到期时间是否已过」的判断，DueDate 原样透传
//     ——见 TestVip3DoesNotInterpretDueDate 的到期前三连（前 1 秒 / 整点 / 后 1 秒），
//     服务端把判定权完全交给客户端；
//  3. 缓存读的是 protobuf 字节：解不出来的坏值与空串一律按 miss 处理并覆盖重写；
//     零值回填编码成空串，于是 v3_ 这层对会员查询实际永远读不出命中（缺口 22）；
//  4. 回填的 TTL 拉满 3600（cacheExpireSeconds）。
//
// 覆盖不到的分支（如实声明）：`err != nil`（vip3logic.go:32-35）与 `vip == nil`
// （36-38）两支都不可达 —— RawVip 恒 return (&rpc.VipInfo{}, nil)（raw.go:213），
// Repository.Vip 的两条出口都非空（命中返回非 nil 的缓存值，未命中返回 RawVip 的
// &rpc.VipInfo{}），而 CacheVip 把读故障/坏值都吞成 (nil, nil)（cache.go:243-256）：
// 见 README 缺口 18。

import (
	"context"
	"testing"

	"go-video/services/account/rpc"
)

func callVip3(t *testing.T, e *env, mid int64) (*rpc.VipReply, error) {
	t.Helper()
	return NewVip3Logic(context.Background(), e.svcCtx).Vip3(&rpc.MidReq{Mid: mid, RealIp: "1.2.3.4"})
}

// TestVip3ColdCacheReturnsZeroValueAndTouchesNoDownstream 冷缓存：只读一次 + 回填一次，
// 四字段全零，且证明会员查询没有偷偷依赖任何下游/本地表。
func TestVip3ColdCacheReturnsZeroValueAndTouchesNoDownstream(t *testing.T) {
	const mid = int64(70001)
	e := newEnv(t) // 一个下游都不布：Vip3 若敢调下游就会拿到 errDownstreamWired
	st := e.st
	st.log.reset()

	reply, err := callVip3(t, e, mid)
	wantNoErr(t, "Vip3", err)
	wantProto(t, "冷缓存的会员信息", "reply", reply, &rpc.VipReply{})
	wantOps(t, "只读一次缓存 + 回填一次", e.ops(0), []string{
		"cache.CacheVip:" + vipKey(mid),
		"cache.AddCacheVip:" + vipKey(mid),
	})
	for _, needle := range []string{"userProfile.", "socialGraph.", "account.Find", "cred.Find"} {
		wantNoOpsWith(t, "会员查询的调用轨迹", e.ops(0), needle)
	}
	if ttl, ok := st.cache.ttlOf(vipKey(mid)); !ok || ttl != 3600 {
		t.Errorf("回填 TTL = %d(存在=%v), want 3600", ttl, ok)
	}
}

// TestVip3HotCachePassesThroughAllFourFields 命中路径的四字段透传：
// Type/Status/DueDate/VipPayType 缺任何一条搬运都会红。
//
// 注意：这条分支在**当前生产上不可达**（回源恒零值，见下一条用例），
// 保留它是对契约的保险——会员真接上数据源之后必须原样透传，不得二次加工。
func TestVip3HotCachePassesThroughAllFourFields(t *testing.T) {
	const mid = int64(70001)
	cached := &rpc.VipInfo{Type: 2, Status: 1, DueDate: 1893456000, VipPayType: 3}
	e := newEnv(t)
	st := e.st
	st.cache.warmMsg(vipKey(mid), cached, 11)
	st.log.reset()

	reply, err := callVip3(t, e, mid)
	wantNoErr(t, "Vip3", err)
	wantProto(t, "命中后的四字段透传", "reply", reply,
		&rpc.VipReply{Type: 2, Status: 1, DueDate: 1893456000, VipPayType: 3})
	// 命中不得再回填（否则每次读都是一次 SETEX 写放大）。
	wantOps(t, "命中只读不写", e.ops(0), []string{"cache.CacheVip:" + vipKey(mid)})
	if ttl, ok := st.cache.ttlOf(vipKey(mid)); !ok || ttl != 11 {
		t.Errorf("命中路径改了 TTL：%d(存在=%v), want 保持 11", ttl, ok)
	}
}

// TestVip3DoesNotInterpretDueDate 到期前 1 秒 / 整点 / 到期后 1 秒三连：
// 服务端三次的唯一差别就是 DueDate 数字本身，绝不出现「过期就清零」「过期就 status=0」。
func TestVip3DoesNotInterpretDueDate(t *testing.T) {
	const (
		mid      = int64(70001)
		dueAt    = int64(1893456000) // 2030-01-01 前后某个固定值，用例不依赖真实时钟
		oneSec   = int64(1)
		statusOf = int32(1)
	)
	for _, tc := range []struct {
		name string
		due  int64
	}{
		{"到期前 1 秒", dueAt - oneSec},
		{"正好到期这一刻", dueAt},
		{"到期后 1 秒", dueAt + oneSec},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.st.cache.warmMsg(vipKey(mid), &rpc.VipInfo{Type: 1, Status: statusOf, DueDate: tc.due, VipPayType: 2}, 3600)
			e.st.log.reset()

			reply, err := callVip3(t, e, mid)
			wantNoErr(t, "Vip3", err)
			wantEQ(t, "due_date 原样透传", "值", reply.GetDueDate(), tc.due)
			wantEQ(t, "type 不被过期逻辑改写", "值", reply.GetType(), int32(1))
			wantEQ(t, "status 不被过期逻辑改写", "值", reply.GetStatus(), statusOf)
			wantEQ(t, "vip_pay_type 不被过期逻辑改写", "值", reply.GetVipPayType(), int32(2))
		})
	}
}

// TestVip3UnparsableCachedBytesTreatedAsMissAndOverwritten v3_ 键上是一坨解不出来的
// 字节时：读侧按 miss 处理（cache.go:253-256 的 Unmarshal 失败口径），
// 回源后用零值把它覆盖掉——坏值不会一直赖在那儿。
// （跨类型的**合法**字节不在这儿：那是另一种后果，见 README 缺口 23 与
// TestInfo3CrossTypeCachedBytesAreServedAsValidHit。）
func TestVip3UnparsableCachedBytesTreatedAsMissAndOverwritten(t *testing.T) {
	const mid = int64(70001)
	e := newEnv(t)
	st := e.st
	st.cache.warmRawMsg(vipKey(mid), brokenProtoBytes, 11)
	st.log.reset()

	reply, err := callVip3(t, e, mid)
	wantNoErr(t, "坏值必须走 miss 而不是报错", err)
	wantProto(t, "坏值按零值降级", "reply", reply, &rpc.VipReply{})
	wantOps(t, "坏值：读一次 + 覆盖重写一次", e.ops(0), []string{
		"cache.CacheVip:" + vipKey(mid),
		"cache.AddCacheVip:" + vipKey(mid),
	})
	if _, ok := msgAs[*rpc.VipInfo](st.cache, vipKey(mid)); ok {
		t.Errorf("前提已变：零值回填竟然读得出来，请复核 addMsg 的字节口径与 cache.go:249-251")
	}
	// 覆盖写确实发生了：键还在，但字节长度已经是 0（坏值不再赖在那儿）。
	bs, ok := st.cache.msgBytesOf(vipKey(mid))
	if !ok {
		t.Fatalf("覆盖写没落到 %s（当前 key：%v）", vipKey(mid), st.cache.keys())
	}
	if len(bs) != 0 {
		t.Errorf("坏值没被覆盖：%s 仍有 %d 字节：%v", vipKey(mid), len(bs), bs)
	}
}

// TestVip3BackfillIsAnInvalidWriteBecauseZeroValueMarshalsEmpty 连按三次 Vip3：
// 每一次都是「读 miss → 写空串」，缓存从来没服务过一次。
//
// TODO(缺陷)（README 缺口 22）：RawVip 恒返回零值（raw.go:213），而
// proto.Marshal(&rpc.VipInfo{}) 得到**空字节串**，CacheVip 又把空串读成 miss
// （cache.go:249-251）。于是 v3_ 这一层对会员查询完全无效：每次请求白发一条
// GET + 一条 SETEX，并且 SETEX 还会把 TTL 重新拉满 3600（键一直被续期、一直被当 miss）。
// 影响面是性能与 Redis 写放大，不影响应答正确性（返回的确实是零值会员）。
// 真正修法要么去掉会员的缓存层，要么让 RawVip 未实现时返回 (nil, nil) 并让
// Repository.Vip 跳过回填 —— 本轮不改生产代码，这里钉住**当前**行为。
func TestVip3BackfillIsAnInvalidWriteBecauseZeroValueMarshalsEmpty(t *testing.T) {
	const mid = int64(70001)
	e := newEnv(t)
	st := e.st
	st.log.reset()

	for i := 1; i <= 3; i++ {
		reply, err := callVip3(t, e, mid)
		wantNoErr(t, "Vip3", err)
		wantProto(t, "第 "+itoa(int64(i))+" 次仍然拿到零值", "reply", reply, &rpc.VipReply{})
		wantCount(t, "缓存读次数", e.ops(0), "cache.CacheVip:", i)
		wantCount(t, "回填写次数", e.ops(0), "cache.AddCacheVip:", i)
	}
	// 键确实存在（写了 3 次），但值是空串：读回来仍是 miss。
	bs, ok := st.cache.msgBytesOf(vipKey(mid))
	if !ok || len(bs) != 0 {
		t.Errorf("v3_%d 应为「键在、值为空串」，实得 (%v, 存在=%v)", mid, bs, ok)
	}
	if ttl, ok := st.cache.ttlOf(vipKey(mid)); !ok || ttl != 3600 {
		t.Errorf("空值仍被续期到 %d(存在=%v)，want 3600 —— 无效写的另一半代价", ttl, ok)
	}
	wantEQ(t, "缓存里只该有 v3_ 这一个键", "值", len(st.cache.keys()), 1)
}

// TestVip3MidZeroAndOneUseDifferentCacheKeys mid 不校验，v3_0 / v3_1 各自独立：
// 布在 v3_0 上的会员不得服务 mid=1，也不得被 mid=1 的回填覆盖。
func TestVip3MidZeroAndOneUseDifferentCacheKeys(t *testing.T) {
	e := newEnv(t)
	st := e.st
	st.cache.warmMsg(vipKey(0), &rpc.VipInfo{Type: 1, Status: 1, DueDate: 100, VipPayType: 1}, 11)
	st.log.reset()

	reply, err := callVip3(t, e, 1)
	wantNoErr(t, "Vip3 mid=1", err)
	wantProto(t, "mid=1 不能吃到 v3_0 的值", "reply", reply, &rpc.VipReply{})
	wantNoOpsWith(t, "mid=1 的调用轨迹", e.ops(0), "cache.CacheVip:"+vipKey(0))
	wantOps(t, "mid=1 读写自己的键", e.ops(0), []string{
		"cache.CacheVip:" + vipKey(1),
		"cache.AddCacheVip:" + vipKey(1),
	})
	// v3_0 的载荷与 TTL 都原封不动（回填没有串到别人的键上）。
	got, ok := msgAs[*rpc.VipInfo](st.cache, vipKey(0))
	if !ok {
		t.Fatalf("v3_0 的布景被破坏了（当前 key：%v）", st.cache.keys())
	}
	wantProto(t, "v3_0 未被串号", "v3_0", got, &rpc.VipInfo{Type: 1, Status: 1, DueDate: 100, VipPayType: 1})
	if ttl, ok := st.cache.ttlOf(vipKey(0)); !ok || ttl != 11 {
		t.Errorf("v3_0 的 TTL 被改：%d(存在=%v), want 保持 11", ttl, ok)
	}

	// 反向：mid=0 必须命中自己的键。
	before := len(e.ops(0))
	reply0, err := callVip3(t, e, 0)
	wantNoErr(t, "Vip3 mid=0", err)
	wantProto(t, "mid=0 命中预置值", "reply", reply0, &rpc.VipReply{Type: 1, Status: 1, DueDate: 100, VipPayType: 1})
	wantOps(t, "命中不写", e.ops(before), []string{"cache.CacheVip:" + vipKey(0)})
}
