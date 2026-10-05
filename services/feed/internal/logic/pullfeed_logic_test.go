package logic

// pullfeed_logic_test.go 锁游标拉取的四条结论：
//   - 正常翻页**可续拉**（不重）；但同秒动态会被整段跳过（漏，缺陷 D11）；
//   - next_cursor / has_more 的口径只由「探针」决定，与过滤后的条目数无关，
//     整页被删时会退化成「空列表 + has_more=true + cursor=0」（缺陷 D12）；
//   - 缓存 miss 才回源 DB 并预热；**缓存读错误不回退**（缺陷 D13）；
//   - 空收件箱没有负缓存，每次请求都打 DB（缺陷 D14）。

import (
	"context"
	"slices"
	"testing"

	"go-video/services/feed/model"
	"go-video/services/feed/rpc"
)

// seedInboxEntry 布一条「主表 + 作者 + 粉丝收件箱 ZSet」都齐的动态，返回动态 ID。
func seedInboxEntry(st *store, follower, ctime, oid int64) int64 {
	row := st.outbox.seed(101, oid, ctime)
	st.cache.seedZSet(inboxKey(follower), row.ID, row.Ctime)
	return row.ID
}

func TestPullFeedPagesForwardWithoutDuplicates(t *testing.T) {
	st := newStore()
	const follower = int64(201)
	var ids []int64
	for i, ctime := range []int64{1_000, 900, 800, 700, 600} {
		ids = append(ids, seedInboxEntry(st, follower, ctime, int64(5000+i)))
	}

	var seen []int64
	var cursor int64
	for page := 1; page <= 4; page++ {
		st.log.reset()
		reply, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
			&rpc.PullFeedReq{Mid: follower, Cursor: cursor, Ps: 2})
		wantNoErr(t, "翻页", err)
		wantCountIn(t, "每页只读一次 ZSet", st.log.ops, "cache.RangeInbox:", 1)
		wantCountIn(t, "命中缓存不回源 DB", st.log.ops, "feed_inbox.ListByMid:", 0)
		seen = append(seen, feedIDs(reply.Items)...)
		if !reply.HasMore {
			wantEQ(t, "最后一页的 next_cursor 必须归零", "next_cursor", reply.NextCursor, int64(0))
			break
		}
		wantEQ(t, "中间页 next_cursor 必须能续拉", "next_cursor > 0", reply.NextCursor > 0, true)
		cursor = reply.NextCursor
	}
	wantInt64sEQ(t, "五页合起来不重不漏", "seen", seen, ids)
}

// TestPullFeedSameCtimeEntriesAreSkipped 钉住缺陷 D11：
// 游标只有一个 int64，承载不了 (ctime, feed_id) 复合位置（rpc/feed.proto:75），
// ZSet 读又是严格 `ctime < cursor`（repository.go:112-114），
// 所以同一秒发布的第二条动态在翻页时**永远拿不到**——不是重复，是丢。
func TestPullFeedSameCtimeEntriesAreSkipped(t *testing.T) {
	st := newStore()
	const follower = int64(201)
	sameSecond := []int64{
		seedInboxEntry(st, follower, 1_000, 5001),
		seedInboxEntry(st, follower, 1_000, 5002),
	}
	older := seedInboxEntry(st, follower, 900, 5003)

	var seen []int64
	var cursor int64
	for page := 1; page <= 3; page++ {
		reply, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
			&rpc.PullFeedReq{Mid: follower, Cursor: cursor, Ps: 1})
		wantNoErr(t, "同秒翻页", err)
		seen = append(seen, feedIDs(reply.Items)...)
		if !reply.HasMore {
			break
		}
		cursor = reply.NextCursor
	}

	var lost []int64
	for _, id := range sameSecond {
		if !slices.Contains(seen, id) {
			lost = append(lost, id)
		}
	}
	wantEQ(t, "库里 3 条、翻完只拿到 2 条", "seen 条数", len(seen), 2)
	wantEQ(t, "最后一页是较早的那条", "seen 末项", seen[len(seen)-1], older)
	wantEQ(t, "同秒动态漏掉的条数（漏，不是重复）", "lost 条数", len(lost), 1)
}

// TestPullFeedExactlyOnePageReportsNoMore 锁 has_more/next_cursor 口径：
// 恰好 ps 条时探针读不到第 ps+1 条 → has_more=false、next_cursor=0（repository.go:434-456）。
// 客户端据此停止下拉，不会多打一次空页。
func TestPullFeedExactlyOnePageReportsNoMore(t *testing.T) {
	st := newStore()
	const follower = int64(201)
	ids := []int64{
		seedInboxEntry(st, follower, 1_000, 5001),
		seedInboxEntry(st, follower, 900, 5002),
	}
	st.log.reset()
	reply, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
		&rpc.PullFeedReq{Mid: follower, Ps: 2})
	wantNoErr(t, "整两页", err)
	wantOps(t, "只读一次 ZSet + 一次回表", st.log.ops, []string{
		"cache.RangeInbox:mid=201:cursor=0:limit=3",
		"feed_outbox.FindMany:" + joinInt64(ids),
	})
	wantEQ(t, "两页", "has_more", reply.HasMore, false)
	wantEQ(t, "两页", "next_cursor", reply.NextCursor, int64(0))
	wantInt64sEQ(t, "两页条目（ctime 倒序）", "items", feedIDs(reply.Items), ids)
}

// TestPullFeedAllDeletedPageStalls 钉住缺陷 D12：
// 探针判定 has_more 时看的是「ZSet 截断前的条数」，而过滤已删除条目发生在回表之后
// （repository.go:434-456），于是整页都被删除时返回**空列表 + has_more=true + next_cursor=0**。
// 客户端按 next_cursor=0 续拉等于从头重扫 → 永远停在同一页，看不到后面存活的内容。
func TestPullFeedAllDeletedPageStalls(t *testing.T) {
	st := newStore()
	const follower = int64(201)
	dead := make([]int64, 5)
	for i := range dead {
		id := seedInboxEntry(st, follower, int64(1_000-i*10), int64(5000+i))
		st.outbox.rows[id].State = 2 // 主表已软删，但 ZSet 成员还在（DeleteFeed 只按粉丝集合清理）
	}
	alive := seedInboxEntry(st, follower, 500, 5099)
	st.log.reset()

	reply, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
		&rpc.PullFeedReq{Mid: follower, Ps: 2})
	wantNoErr(t, "整页已删", err)
	wantInt64sEQ(t, "第一页没有可展示条目", "items", feedIDs(reply.Items), nil)
	wantEQ(t, "却又声称还有下一页", "has_more", reply.HasMore, true)
	wantEQ(t, "游标退化成 0（续拉=从头重扫）", "next_cursor", reply.NextCursor, int64(0))

	// 用 0 当游标重放：结果一模一样，存活的那条永远读不到 → 死循环。
	for round := 1; round <= 2; round++ {
		again, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
			&rpc.PullFeedReq{Mid: follower, Cursor: reply.NextCursor, Ps: 2})
		wantNoErr(t, "按 next_cursor 重放", err)
		wantEQ(t, "重放第 "+itoa(int64(round))+" 轮仍是空页", "items", len(again.Items), 0)
		wantEQ(t, "重放第 "+itoa(int64(round))+" 轮 has_more 不变", "has_more", again.HasMore, true)
	}
	// 证明「只要肯把游标推到已删区之下，存活内容是读得到的」——问题在游标语义而不是数据。
	forward, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
		&rpc.PullFeedReq{Mid: follower, Cursor: 950, Ps: 2})
	wantNoErr(t, "手工推游标", err)
	wantInt64sEQ(t, "手工跳过已删区后能读到存活动态", "items", feedIDs(forward.Items), []int64{alive})
}

// TestPullFeedStaleZSetMemberBlocksDbFallback 钉住缺陷 D15：
// ZSet 里还留着成员、主表已经没有那一行（删除时 RemInbox 失败会被吞，repository.go:556），
// 此时 len(ids)!=0 → 不走回源分支（repository.go:395），
// 于是这个用户的关注流**永久空白**，且没有任何路径把陈旧成员清掉。
func TestPullFeedStaleZSetMemberBlocksDbFallback(t *testing.T) {
	st := newStore()
	const follower = int64(201)
	st.cache.seedZSet(inboxKey(follower), 999, 1_000) // 幽灵成员：主表无此 id
	alive := st.outbox.seed(101, 5002, 900)
	st.inbox.seed(follower, alive.ID, 101, 900) // DB 投影里确实还有一条可读

	st.log.reset()
	reply, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
		&rpc.PullFeedReq{Mid: follower, Ps: 20})
	wantNoErr(t, "幽灵成员", err)
	wantOps(t, "不回源、不清理：只回表查那个不存在的 id", st.log.ops, []string{
		"cache.RangeInbox:mid=201:cursor=0:limit=21",
		"feed_outbox.FindMany:999",
	})
	wantEQ(t, "关注流被幽灵成员堵成空白（库里那条读不到）", "items", len(reply.Items), 0)
	wantEQ(t, "探针也没读到第二条（ZSet 里只有幽灵）", "has_more", reply.HasMore, false)
}

func TestPullFeedRepositoryErrorPropagates(t *testing.T) {
	st := newStore()
	const follower = int64(201)
	seedInboxEntry(st, follower, 1_000, 5001)
	st.fault().failWith("feed_outbox.FindMany", errInjected)

	reply, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
		&rpc.PullFeedReq{Mid: follower, Ps: 20})
	wantErrIs(t, "回表失败必须原样透传", err, errInjected)
	if reply != nil {
		t.Fatalf("出错时不应返回半截响应：%+v", reply)
	}
}

// TestPullFeedCacheErrorDoesNotFallBackToDb 钉住缺陷 D13（repository.go:391-394）：
// ZSet 读错误直接向上抛，不进回源分支——DB 里明明有完整投影（feed_inbox），
// Redis 抖动一次就让关注流整体不可用，与「缓存缺失时回源」的同一函数口径自相矛盾。
func TestPullFeedCacheErrorDoesNotFallBackToDb(t *testing.T) {
	st := newStore()
	const follower = int64(201)
	alive := seedInboxEntry(st, follower, 1_000, 5001)
	st.inbox.seed(follower, alive, 101, 1_000)
	st.fault().failWith("cache.RangeInbox", errCacheDown)

	st.log.reset()
	reply, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
		&rpc.PullFeedReq{Mid: follower, Ps: 20})
	wantErrIs(t, "缓存读错误原样透传（不降级）", err, errCacheDown)
	if reply != nil {
		t.Fatalf("出错时不应返回响应：%+v", reply)
	}
	wantOps(t, "没有走 feed_inbox 回源＝降级缺失", st.log.ops, []string{
		"cache.RangeInbox:mid=201:cursor=0:limit=21",
	})
}

// TestPullFeedCacheMissReadsDbAndWarms 锁回源+预热：
// ZSet 空 → 按 LIMIT ps+1 读投影表 → 逐条 ZADD 预热（含探针那条）→ 回表取主表。
// 第二次读命中预热结果，不再打 DB。
func TestPullFeedCacheMissReadsDbAndWarms(t *testing.T) {
	st := newStore()
	const follower = int64(201)
	var ids []int64
	for i, ctime := range []int64{1_000, 900, 800} {
		row := st.outbox.seed(101, int64(5000+i), ctime)
		ids = append(ids, row.ID)
		st.inbox.seed(follower, row.ID, 101, ctime)
	}
	st.log.reset()
	reply, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
		&rpc.PullFeedReq{Mid: follower, Ps: 2})
	wantNoErr(t, "miss 回源", err)
	wantOps(t, "回源并预热（探针那条也预热）", st.log.ops, []string{
		"cache.RangeInbox:mid=201:cursor=0:limit=3",
		"feed_inbox.ListByMid:mid=201:cursor=0:limit=3",
		"cache.AddInbox:mid=201:fid=" + itoa(ids[0]),
		"cache.AddInbox:mid=201:fid=" + itoa(ids[1]),
		"cache.AddInbox:mid=201:fid=" + itoa(ids[2]),
		"feed_outbox.FindMany:" + joinInt64(ids[:2]),
	})
	wantInt64sEQ(t, "回源首页条目", "items", feedIDs(reply.Items), ids[:2])

	st.log.reset()
	_, err = NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
		&rpc.PullFeedReq{Mid: follower, Cursor: reply.NextCursor, Ps: 2})
	wantNoErr(t, "预热后续拉", err)
	wantCountIn(t, "第二次不再回源 DB", st.log.ops, "feed_inbox.ListByMid:", 0)
	wantCountIn(t, "第二次仍只读一次 ZSet", st.log.ops, "cache.RangeInbox:", 1)
}

// TestPullFeedEmptyInboxHitsDbEveryTime 钉住缺陷 D14（repository.go:395-412）：
// 「确实没有动态」与「缓存未预热」在 ZSet 层不可区分（都是 0 个成员），
// 而回源后只有非空才预热 → 空收件箱用户的每一次下拉都打一次 feed_inbox。
func TestPullFeedEmptyInboxHitsDbEveryTime(t *testing.T) {
	st := newStore()
	for round := 1; round <= 3; round++ {
		st.log.reset()
		reply, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
			&rpc.PullFeedReq{Mid: 201, Ps: 20})
		wantNoErr(t, "空收件箱", err)
		wantOps(t, "每轮都回源一次且无预热", st.log.ops, []string{
			"cache.RangeInbox:mid=201:cursor=0:limit=21",
			"feed_inbox.ListByMid:mid=201:cursor=0:limit=21",
		})
		wantEQ(t, "空收件箱", "items", len(reply.Items), 0)
		wantEQ(t, "空收件箱", "has_more", reply.HasMore, false)
	}
}

// TestPullFeedDbFaultOnFallbackPropagates 锁回源分支的错误语义：DB 失败不得被当成空列表。
func TestPullFeedDbFaultOnFallbackPropagates(t *testing.T) {
	st := newStore()
	st.fault().failWith("feed_inbox.ListByMid", errInjected)
	reply, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
		&rpc.PullFeedReq{Mid: 201, Ps: 20})
	wantErrIs(t, "回源失败原样透传", err, errInjected)
	if reply != nil {
		t.Fatalf("出错时不应返回响应：%+v", reply)
	}
	wantOps(t, "失败前只做过一次 ZSet 读与一次回源", st.log.ops, []string{
		"cache.RangeInbox:mid=201:cursor=0:limit=21",
		"feed_inbox.ListByMid:mid=201:cursor=0:limit=21",
	})
}

// TestPullFeedProjectsEveryModelField 锁 FeedOutbox → FeedItem 的投影字段（helpers.go:9-25）：
// 逐字段核对，尤其 otype/action 的枚举透传与 forward_id。
func TestPullFeedProjectsEveryModelField(t *testing.T) {
	st := newStore()
	const follower = int64(201)
	row := st.outbox.put(&model.FeedOutbox{
		Mid: 101, Oid: 5001, Otype: 2, Action: 2, Ctime: 1_000, State: 0,
		Title: "番剧更新", Cover: "cover.png", Uri: "bilibili://pgc/5001", ForwardID: 777,
	})
	st.cache.seedZSet(inboxKey(follower), row.ID, row.Ctime)

	reply, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
		&rpc.PullFeedReq{Mid: follower, Ps: 20})
	wantNoErr(t, "投影", err)
	if len(reply.Items) != 1 {
		t.Fatalf("items 条数 = %d, want 1", len(reply.Items))
	}
	got := reply.Items[0]
	wantEQ(t, "投影", "id", got.Id, row.ID)
	wantEQ(t, "投影", "mid", got.Mid, int64(101))
	wantEQ(t, "投影", "oid", got.Oid, int64(5001))
	wantEQ(t, "投影", "otype", got.Otype, rpc.OType_OTYPE_PGC_WORK)
	wantEQ(t, "投影", "action", got.Action, rpc.Action_ACTION_FORWARD)
	wantEQ(t, "投影", "ctime", got.Ctime, int64(1_000))
	wantEQ(t, "投影", "title", got.Title, "番剧更新")
	wantEQ(t, "投影", "cover", got.Cover, "cover.png")
	wantEQ(t, "投影", "uri", got.Uri, "bilibili://pgc/5001")
	wantEQ(t, "投影", "forward_id", got.ForwardId, int64(777))
}
