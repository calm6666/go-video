package logic

// unread_logic_test.go 锁未读口径（feed:unread:{mid} 计数器 + feed_unread 表）：
//   - 域结论：**未读数是一个投影，不是即时统计**——只有 PushFeed 的 INCRBY/IncrBy 双写在推进它，
//     8 个 rpc 里没有任何重算入口（GetUnreadCount 只做「缓存命中即返回 / miss 回表一次」）；
//   - 计数用「缓存值 vs 表里真实行数」两个数并列断言，不用「永远返回 0」糊过去；
//   - GetUnreadCount 的 cache-miss ≠ cache-error：错误必须原样抛出，不许悄悄降级成 0；
//   - ClearUnread 的两侧写入顺序（先缓存后 DB）与 DB 失败后的复活窗口（缺陷 D16）。

import (
	"context"
	"testing"

	"go-video/services/feed/rpc"
)

func TestGetUnreadCountCacheHitIsAuthoritative(t *testing.T) {
	st := newStore()
	follower := int64(7001)
	st.cache.seedUnread(follower, 7)
	st.unread.seed(follower, 99) // 表里是另一个数：谁赢就说明谁是权威

	st.log.reset()
	reply, err := NewGetUnreadCountLogic(context.Background(), st.svcCtx()).GetUnreadCount(
		&rpc.MidReq{Mid: follower})
	wantNoErr(t, "读未读", err)
	wantEQ(t, "返回的是缓存计数器值，不是 DB 值", "unread", reply.Unread, int64(7))
	wantOps(t, "缓存命中即返回，不回表", st.log.ops, []string{
		"cache.GetUnread:" + itoa(follower),
	})
}

func TestGetUnreadCountMissBackfillsAndWarms(t *testing.T) {
	st := newStore()
	follower := int64(7001)
	st.unread.seed(follower, 5)
	// 只布 DB，不布缓存（Redis 逐出后的形状）。

	st.log.reset()
	reply, err := NewGetUnreadCountLogic(context.Background(), st.svcCtx()).GetUnreadCount(
		&rpc.MidReq{Mid: follower})
	wantNoErr(t, "缓存 miss", err)
	wantEQ(t, "回表读到真实值", "unread", reply.Unread, int64(5))
	wantOps(t, "miss → 回表 → 预热", st.log.ops, []string{
		"cache.GetUnread:" + itoa(follower),
		"feed_unread.Get:" + itoa(follower),
		"cache.SetUnread:" + itoa(follower) + "=5",
	})
	v, ok := st.cache.unreadValue(follower)
	wantEQ(t, "预热后缓存键存在", "hit", ok, true)
	wantEQ(t, "预热值与 DB 一致", "cached", v, int64(5))

	st.log.reset()
	_, err = NewGetUnreadCountLogic(context.Background(), st.svcCtx()).GetUnreadCount(
		&rpc.MidReq{Mid: follower})
	wantNoErr(t, "第二次读", err)
	wantOps(t, "第二次直接命中缓存", st.log.ops, []string{
		"cache.GetUnread:" + itoa(follower),
	})
}

// TestGetUnreadCountNoDbRowWarmsZero 表里没有这个 mid（新用户/清理后）→ 0，
// 且这个 0 会被写回缓存，等价于负缓存：后续读取不再回表。
func TestGetUnreadCountNoDbRowWarmsZero(t *testing.T) {
	st := newStore()
	freshman := int64(7002)

	st.log.reset()
	reply, err := NewGetUnreadCountLogic(context.Background(), st.svcCtx()).GetUnreadCount(
		&rpc.MidReq{Mid: freshman})
	wantNoErr(t, "全新用户", err)
	wantEQ(t, "无行即 0（不是错误）", "unread", reply.Unread, int64(0))
	wantOps(t, "miss → 回表 0 → 预热 0", st.log.ops, []string{
		"cache.GetUnread:" + itoa(freshman),
		"feed_unread.Get:" + itoa(freshman),
		"cache.SetUnread:" + itoa(freshman) + "=0",
	})

	st.log.reset()
	if _, err := NewGetUnreadCountLogic(context.Background(), st.svcCtx()).GetUnreadCount(
		&rpc.MidReq{Mid: freshman}); err != nil {
		t.Fatalf("第二次读取失败：%v", err)
	}
	wantOps(t, "第二次不再回表（0 也被缓存住了）", st.log.ops, []string{
		"cache.GetUnread:" + itoa(freshman),
	})
}

// TestGetUnreadCountCacheErrorDoesNotDegrade 缓存报错 ≠ 缓存 miss：
// 必须原样抛出，不能退化成「回表一次」，更不能返回 0 让红点消失。
func TestGetUnreadCountCacheErrorDoesNotDegrade(t *testing.T) {
	st := newStore()
	follower := int64(7001)
	st.unread.seed(follower, 5)
	st.cache.seedUnread(follower, 7)
	st.fault().failWith("cache.GetUnread", errCacheDown)

	st.log.reset()
	reply, err := NewGetUnreadCountLogic(context.Background(), st.svcCtx()).GetUnreadCount(
		&rpc.MidReq{Mid: follower})
	wantErrIs(t, "缓存错误原样透传", err, errCacheDown)
	if reply != nil {
		t.Fatalf("出错时不应返回应答：%+v", reply)
	}
	wantOps(t, "没有第二条读路径", st.log.ops, []string{
		"cache.GetUnread:" + itoa(follower),
	})
}

func TestGetUnreadCountDbErrorPropagatesAndSkipsWarm(t *testing.T) {
	st := newStore()
	follower := int64(7001)
	st.fault().failWith("feed_unread.Get", errInjected)

	st.log.reset()
	_, err := NewGetUnreadCountLogic(context.Background(), st.svcCtx()).GetUnreadCount(
		&rpc.MidReq{Mid: follower})
	wantErrIs(t, "回表错误原样透传", err, errInjected)
	wantOps(t, "读失败时不得预热", st.log.ops, []string{
		"cache.GetUnread:" + itoa(follower),
		"feed_unread.Get:" + itoa(follower),
	})
	_, ok := st.cache.unreadValue(follower)
	wantEQ(t, "缓存里没被写入任何值", "key exists", ok, false)
}

// TestUnreadCounterIsProjectionNotLiveCount 用「计数器 vs inbox 行数」两个数并列，
// 钉住未读是投影：PushFeed 推进它，DeleteFeed 不回退它（缺陷 D17）。
// 结果：删掉动态后红点数字比真正可见的条数大，且服务里没有任何自愈路径。
func TestUnreadCounterIsProjectionNotLiveCount(t *testing.T) {
	st := newStore()
	author := int64(101)
	follower := int64(7001)
	st.cache.seedFollowers(author, follower)
	svcCtx := st.svcCtx()

	for i := 0; i < 3; i++ {
		if _, err := NewPushFeedLogic(context.Background(), svcCtx).PushFeed(
			&rpc.PushFeedReq{Mid: author, Oid: int64(5001 + i)}); err != nil {
			t.Fatalf("第 %d 条推送失败：%v", i+1, err)
		}
	}
	cached, _ := st.cache.unreadValue(follower)
	wantEQ(t, "3 条动态 → 计数器 3", "cache unread", cached, int64(3))
	wantEQ(t, "3 条动态 → DB 3", "db unread", st.unread.value(follower), int64(3))
	wantEQ(t, "3 条动态 → inbox 3 行", "inbox rows", len(st.inbox.rowsOf(follower)), 3)

	feedID := st.outbox.lastID()
	from := st.log.snapshot()
	if _, err := NewDeleteFeedLogic(context.Background(), svcCtx).DeleteFeed(
		&rpc.DeleteFeedReq{Mid: author, FeedId: feedID}); err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	// 删除路径里既没有缓存 INCRBY，也没有 DB 计数回退——投影只进不出。
	wantCountIn(t, "删除动态不回收未读", st.log.opsFrom(from), "cache.IncrUnread", 0)
	wantCountIn(t, "删除动态不回退 DB 计数", st.log.opsFrom(from), "feed_unread.IncrBy", 0)
	cached, _ = st.cache.unreadValue(follower)
	wantEQ(t, "删除后红点仍是 3", "cache unread", cached, int64(3))
	wantEQ(t, "删除后 DB 仍是 3", "db unread", st.unread.value(follower), int64(3))
	wantEQ(t, "实际可见的收件行只剩 2", "inbox rows", len(st.inbox.rowsOf(follower)), 2)

	reply, err := NewGetUnreadCountLogic(context.Background(), svcCtx).GetUnreadCount(
		&rpc.MidReq{Mid: follower})
	wantNoErr(t, "读未读", err)
	wantEQ(t, "接口报出的数与真实行数不一致（差 1）", "unread", reply.Unread, int64(3))
}

func TestClearUnreadWritesBothSides(t *testing.T) {
	st := newStore()
	follower := int64(7001)
	st.cache.seedUnread(follower, 5)
	st.unread.seed(follower, 5)

	st.log.reset()
	reply, err := NewClearUnreadLogic(context.Background(), st.svcCtx()).ClearUnread(
		&rpc.MidReq{Mid: follower})
	wantNoErr(t, "清零", err)
	wantEQ(t, "应答", "reply", reply != nil, true)
	wantOps(t, "先清缓存再清 DB", st.log.ops, []string{
		"cache.ClearUnread:" + itoa(follower),
		"feed_unread.Clear:" + itoa(follower),
	})
	v, ok := st.cache.unreadValue(follower)
	wantEQ(t, "缓存键仍然存在（SET 0，不是 DEL）", "hit", ok, true)
	wantEQ(t, "缓存值为 0", "cached", v, int64(0))
	wantEQ(t, "DB 值为 0", "db", st.unread.value(follower), int64(0))
}

// TestClearUnreadDbFailureResurrectsStaleCount 缺陷 D16（repository.go:537-542）：
// 清零先写缓存、后写 DB，且 DB 失败会返回错误但**缓存已经清了**。
// 后果：只要之后缓存键被逐出，GetUnreadCount 就把旧的未读数原封不动地读回来并再次预热，
// 用户「已读」这件事凭空消失，而调用方看到的错误在重试时也无法挽回（Clear 是幂等的 upsert，
// 但客户端多半把第一次的失败当成没做成而不再重试）。
func TestClearUnreadDbFailureResurrectsStaleCount(t *testing.T) {
	st := newStore()
	follower := int64(7001)
	st.cache.seedUnread(follower, 9)
	st.unread.seed(follower, 9)
	st.fault().failWith("feed_unread.Clear", errInjected)

	st.log.reset()
	_, err := NewClearUnreadLogic(context.Background(), st.svcCtx()).ClearUnread(
		&rpc.MidReq{Mid: follower})
	wantErrIs(t, "DB 清零失败要报错", err, errInjected)
	wantOps(t, "错误发生时缓存已经动了", st.log.ops, []string{
		"cache.ClearUnread:" + itoa(follower),
		"feed_unread.Clear:" + itoa(follower),
	})
	cached, _ := st.cache.unreadValue(follower)
	wantEQ(t, "缓存已经是 0", "cache", cached, int64(0))
	wantEQ(t, "DB 还是 9", "db", st.unread.value(follower), int64(9))

	// 缓存键被逐出（Redis maxmemory / 人工清理）之后，旧值复活：
	st.fault().failWith("feed_unread.Clear", nil)
	st.cache.dropUnread(follower)
	st.log.reset()
	reply, err := NewGetUnreadCountLogic(context.Background(), st.svcCtx()).GetUnreadCount(
		&rpc.MidReq{Mid: follower})
	wantNoErr(t, "复活读取", err)
	wantEQ(t, "未读数复活成 9", "unread", reply.Unread, int64(9))
	wantOps(t, "复活还会被预热成新的权威值", st.log.ops, []string{
		"cache.GetUnread:" + itoa(follower),
		"feed_unread.Get:" + itoa(follower),
		"cache.SetUnread:" + itoa(follower) + "=9",
	})
}

func TestClearUnreadCacheFailureKeepsDbUntouched(t *testing.T) {
	st := newStore()
	follower := int64(7001)
	st.cache.seedUnread(follower, 9)
	st.unread.seed(follower, 9)
	st.fault().failWith("cache.ClearUnread", errCacheDown)

	st.log.reset()
	_, err := NewClearUnreadLogic(context.Background(), st.svcCtx()).ClearUnread(
		&rpc.MidReq{Mid: follower})
	wantErrIs(t, "缓存清零失败要报错", err, errCacheDown)
	wantOps(t, "第一步失败即止，DB 不动", st.log.ops, []string{
		"cache.ClearUnread:" + itoa(follower),
	})
	cached, _ := st.cache.unreadValue(follower)
	wantEQ(t, "缓存值未变", "cache", cached, int64(9))
	wantEQ(t, "DB 值未变", "db", st.unread.value(follower), int64(9))

	// 重试可以彻底修好（与 D16 那条不可复活的路径形成对照）。
	st.fault().failWith("cache.ClearUnread", nil)
	st.log.reset()
	if _, err := NewClearUnreadLogic(context.Background(), st.svcCtx()).ClearUnread(
		&rpc.MidReq{Mid: follower}); err != nil {
		t.Fatalf("重试清零失败：%v", err)
	}
	wantOps(t, "重试两步都走完", st.log.ops, []string{
		"cache.ClearUnread:" + itoa(follower),
		"feed_unread.Clear:" + itoa(follower),
	})
	cached, _ = st.cache.unreadValue(follower)
	wantEQ(t, "重试后两侧都归零", "cache", cached, int64(0))
	wantEQ(t, "重试后两侧都归零", "db", st.unread.value(follower), int64(0))
}

// TestClearUnreadOnFreshUserCreatesZeroRow 清零对没有行的 mid 也 upsert 一行 0，
// 并创建缓存键（生产 SetCtx 无 TTL，见 repository.go:243-250 的写入不带过期）。
// 这条不是缺陷，但它是「清零后 GetUnreadCount 再也不回表」的根因，锁住它。
func TestClearUnreadOnFreshUserCreatesZeroRow(t *testing.T) {
	st := newStore()
	freshman := int64(7003)

	st.log.reset()
	if _, err := NewClearUnreadLogic(context.Background(), st.svcCtx()).ClearUnread(
		&rpc.MidReq{Mid: freshman}); err != nil {
		t.Fatalf("新用户清零失败：%v", err)
	}
	wantOps(t, "两步都执行", st.log.ops, []string{
		"cache.ClearUnread:" + itoa(freshman),
		"feed_unread.Clear:" + itoa(freshman),
	})
	wantEQ(t, "DB 里多出一行 0（upsert）", "db", st.unread.value(freshman), int64(0))
	state, ok := st.unread.rows[freshman]
	wantEQ(t, "DB 行存在", "row", ok, true)
	wantEQ(t, "DB 行值", "unread", state.Unread, int64(0))

	st.log.reset()
	if _, err := NewGetUnreadCountLogic(context.Background(), st.svcCtx()).GetUnreadCount(
		&rpc.MidReq{Mid: freshman}); err != nil {
		t.Fatalf("清零后读取失败：%v", err)
	}
	wantOps(t, "清零后读走缓存，不回表", st.log.ops, []string{
		"cache.GetUnread:" + itoa(freshman),
	})
}

// TestUnreadFlowsFromFanoutNotFromPull 拉流不消未读：
// PullFeed 把 3 条动态全部返回给用户之后，未读仍然是 3——
// 服务里没有任何「读即清」的语义，客户端必须显式调 ClearUnread。
func TestUnreadFlowsFromFanoutNotFromPull(t *testing.T) {
	st := newStore()
	author := int64(101)
	follower := int64(7001)
	st.cache.seedFollowers(author, follower)
	var ids []int64
	for i := 0; i < 3; i++ {
		if _, err := NewPushFeedLogic(context.Background(), st.svcCtx()).PushFeed(
			&rpc.PushFeedReq{Mid: author, Oid: int64(5100 + i)}); err != nil {
			t.Fatalf("推送失败：%v", err)
		}
		ids = append(ids, st.outbox.lastID())
	}

	st.log.reset()
	reply, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
		&rpc.PullFeedReq{Mid: follower, Ps: 20})
	wantNoErr(t, "拉流", err)
	// 三条动态 ctime 同秒 → ZSet 同分按 member 十进制字符串倒序，最大 ID 在前。
	wantInt64sEQ(t, "三条动态都可见", "items", feedIDs(reply.Items), revIds(ids))
	wantOps(t, "拉流不碰未读计数器", st.log.ops, []string{
		"cache.RangeInbox:mid=" + itoa(follower) + ":cursor=0:limit=21",
		"feed_outbox.FindMany:" + joinInt64(revIds(ids)),
	})
	cached, _ := st.cache.unreadValue(follower)
	wantEQ(t, "看完之后红点数字不变", "cache unread", cached, int64(3))
	wantEQ(t, "DB 也不变", "db unread", st.unread.value(follower), int64(3))
}

// revIds 返回倒序副本（同分成员在 ZSet 里按 member 字符串倒序，ID 递增即等价于倒序）。
func revIds(in []int64) []int64 {
	out := make([]int64, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}
