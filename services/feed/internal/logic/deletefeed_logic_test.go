package logic

// deletefeed_logic_test.go 锁删除侧的清理半径：
//   - 一条动态在系统里有 4 份拷贝（主表行、作者 outbox ZSet、粉丝 inbox ZSet、inbox 投影行），
//     删除必须四份都动，本文件用完整调用序列 + 逐份读回来锁死；
//   - repository.go:551 / 556 两处 `_ =` 丢弃了 ZSet 清理的结果（缺陷 D21）：
//     方法仍然返回成功，但粉丝的 ZSet 里留下幽灵成员，之后每次 PullFeed 都要多回一次表；
//   - 最后一步 feed_inbox.DeleteByFeedID 的错误**会**返回，可此时主表与 ZSet 都已清理（缺陷 D22）：
//     能否靠重试补清投影，取决于 SoftDelete 的 WHERE 没有 state 条件所带来的 matched/changed rows
//     时序差（跨秒可重跑修好，同秒只能拿 ErrFeedNotFound）——本文件钉的是可重跑这一支
//     （替身按 matched rows 建模，口径见 fakes_test.go 头），另一支在 README 的 D22 里登记；
//   - 删除不清未读计数、不清置顶（缺陷 D17，与 unread/pin 两个文件互为对照）。

import (
	"context"
	"testing"

	"go-video/services/feed/model"
	"go-video/services/feed/rpc"
)

// pushOne 走真实推送路径布一条动态，并把返回不了 feed_id 的缺口（缺陷 D2）用读回补齐。
func pushOne(t *testing.T, st *store, author, oid int64) int64 {
	t.Helper()
	if _, err := NewPushFeedLogic(context.Background(), st.svcCtx()).PushFeed(
		&rpc.PushFeedReq{Mid: author, Oid: oid}); err != nil {
		t.Fatalf("布防推送 oid=%d 失败：%v", oid, err)
	}
	return st.outbox.lastID()
}

func TestDeleteFeedCleansEveryCopy(t *testing.T) {
	st := newStore()
	author := int64(101)
	f1, f2 := int64(7001), int64(7002)
	st.cache.seedFollowers(author, f1, f2)
	feedID := pushOne(t, st, author, 5001)
	other := pushOne(t, st, author, 5002) // 同作者的其它动态不该被顺手清掉

	st.log.reset()
	reply, err := NewDeleteFeedLogic(context.Background(), st.svcCtx()).DeleteFeed(
		&rpc.DeleteFeedReq{Mid: author, FeedId: feedID})
	wantNoErr(t, "删除自己的动态", err)
	wantEQ(t, "应答", "reply", reply != nil, true)
	wantOps(t, "软删主表 → 清作者 ZSet → 读粉丝 → 逐个清粉丝 ZSet → 清投影", st.log.ops, []string{
		"feed_outbox.SoftDelete:" + itoa(feedID) + "@" + itoa(author),
		"cache.RemOutbox:mid=" + itoa(author) + ":fid=" + itoa(feedID),
		"cache.GetFollowers:" + itoa(author),
		"cache.RemInbox:mid=" + itoa(f1) + ":fid=" + itoa(feedID),
		"cache.RemInbox:mid=" + itoa(f2) + ":fid=" + itoa(feedID),
		"feed_inbox.DeleteByFeedID:" + itoa(feedID),
	})

	state, ok := st.outbox.stateOf(feedID)
	wantEQ(t, "主表软删除", "found", ok, true)
	wantEQ(t, "主表软删除", "state", state, model.FeedStateDeleted)
	wantInt64sEQ(t, "作者 ZSet 只剩另一条", "outbox", st.cache.zmembers(outboxKey(author)), []int64{other})
	wantInt64sEQ(t, "粉丝 1 只剩另一条动态的成员", "inbox f1", st.cache.zmembers(inboxKey(f1)), []int64{other})
	wantInt64sEQ(t, "粉丝 2 只剩另一条动态的成员", "inbox f2", st.cache.zmembers(inboxKey(f2)), []int64{other})
	wantInt64sEQ(t, "投影行按 feed_id 全量清（不看归属）", "f1 rows", st.inbox.rowsOf(f1), []int64{other})
	wantEQ(t, "主表另一条动态仍是正常态", "other state", mustState(t, st, other), model.FeedStateNormal)
}

func mustState(t *testing.T, st *store, id int64) int32 {
	t.Helper()
	state, ok := st.outbox.stateOf(id)
	if !ok {
		t.Fatalf("主表里没有 ID=%d 这一行", id)
	}
	return state
}

// TestDeleteFeedOwnershipAndMissingRejections 三个拒绝分支都必须「零副作用」。
func TestDeleteFeedOwnershipAndMissingRejections(t *testing.T) {
	st := newStore()
	author := int64(101)
	intruder := int64(202)
	f1 := int64(7001)
	st.cache.seedFollowers(author, f1)
	feedID := pushOne(t, st, author, 5001)

	cases := []struct {
		name string
		mid  int64
		fid  int64
		why  string
	}{
		{"别人的动态", intruder, feedID, "越权删除他人动态"},
		{"不存在的动态", author, feedID + 500, "删除不存在的 ID"},
	}
	beforeInbox := len(st.inbox.rowsOf(f1))
	for _, tc := range cases {
		st.log.reset()
		reply, err := NewDeleteFeedLogic(context.Background(), st.svcCtx()).DeleteFeed(
			&rpc.DeleteFeedReq{Mid: tc.mid, FeedId: tc.fid})
		wantErrIs(t, tc.why, err, model.ErrFeedNotFound)
		if reply != nil {
			t.Errorf("%s：出错时不应返回应答：%+v", tc.name, reply)
		}
		wantOps(t, tc.name+"只允许一次软删尝试", st.log.ops, []string{
			"feed_outbox.SoftDelete:" + itoa(tc.fid) + "@" + itoa(tc.mid),
		})
	}
	// 两次拒绝之后，动态的四份拷贝一份都没少。
	wantEQ(t, "主表仍是正常态", "state", mustState(t, st, feedID), model.FeedStateNormal)
	wantInt64sEQ(t, "作者 ZSet 还在", "outbox", st.cache.zmembers(outboxKey(author)), []int64{feedID})
	wantInt64sEQ(t, "粉丝 ZSet 还在", "inbox", st.cache.zmembers(inboxKey(f1)), []int64{feedID})
	wantEQ(t, "投影行数不变", "inbox rows", len(st.inbox.rowsOf(f1)), beforeInbox)

	// 第二次删除同一条：model/feedmodel.go:104-119 的 WHERE 只有 (id, mid)、没有 state 条件，
	// 所以重复删除的结果取决于驱动给的是 matched 还是 changed rows：
	// 同秒重复 → state/mtime 都没变 → 0 行 → ErrFeedNotFound；跨秒重复 → mtime 变了 → 1 行 → 成功。
	// 内存替身按 matched rows 建模（口径写在 fakes_test.go 头），因此这里钉「成功」这一支：
	// 整段清理会重跑一遍，但不复活任何东西——这才是这套无幂等约束的表结构里唯一可靠的部分。
	if _, err := NewDeleteFeedLogic(context.Background(), st.svcCtx()).DeleteFeed(
		&rpc.DeleteFeedReq{Mid: author, FeedId: feedID}); err != nil {
		t.Fatalf("首次删除失败：%v", err)
	}
	st.log.reset()
	_, err := NewDeleteFeedLogic(context.Background(), st.svcCtx()).DeleteFeed(
		&rpc.DeleteFeedReq{Mid: author, FeedId: feedID})
	wantNoErr(t, "重复删除（matched-rows 建模）仍返回成功", err)
	wantOps(t, "重复删除把整段清理原样重跑", st.log.ops, []string{
		"feed_outbox.SoftDelete:" + itoa(feedID) + "@" + itoa(author),
		"cache.RemOutbox:mid=" + itoa(author) + ":fid=" + itoa(feedID),
		"cache.GetFollowers:" + itoa(author),
		"cache.RemInbox:mid=" + itoa(f1) + ":fid=" + itoa(feedID),
		"feed_inbox.DeleteByFeedID:" + itoa(feedID),
	})
	wantEQ(t, "主表仍是删除态", "state", mustState(t, st, feedID), model.FeedStateDeleted)
	wantInt64sEQ(t, "作者 ZSet 没有被多删（本来就是空）", "outbox", st.cache.zmembers(outboxKey(author)), nil)
	wantInt64sEQ(t, "粉丝 ZSet 仍是删后形状", "inbox f1", st.cache.zmembers(inboxKey(f1)), nil)
	wantEQ(t, "投影行没有复活", "rows f1", len(st.inbox.rowsOf(f1)), 0)
}

// TestDeleteFeedFollowerSetMissingAndEmpty 粉丝集合「缺失」与「空」都不做 fan-out 清理，
// 但第 4 步（按 feed_id 清投影）仍然执行——投影才是最终一致的兜底。
func TestDeleteFeedFollowerSetMissingAndEmpty(t *testing.T) {
	st := newStore()
	noSetAuthor := int64(101)
	emptySetAuthor := int64(102)
	f1 := int64(7001)
	st.cache.seedFollowers(emptySetAuthor) // 键存在、成员 0
	ghost := pushOne(t, st, noSetAuthor, 5001)
	quiet := pushOne(t, st, emptySetAuthor, 5101)
	// 手工布一份「粉丝 ZSet 里有成员」的形状：真实环境里 fan-out 之后取关事件清空了集合。
	st.cache.seedZSet(inboxKey(f1), ghost, st.outbox.rows[ghost].Ctime)
	st.inbox.seed(f1, ghost, noSetAuthor, st.outbox.rows[ghost].Ctime)

	st.log.reset()
	_, err := NewDeleteFeedLogic(context.Background(), st.svcCtx()).DeleteFeed(
		&rpc.DeleteFeedReq{Mid: noSetAuthor, FeedId: ghost})
	wantNoErr(t, "无粉丝集合也删得掉", err)
	wantOps(t, "集合缺失 → 跳过 ZSet 清理，但仍清投影", st.log.ops, []string{
		"feed_outbox.SoftDelete:" + itoa(ghost) + "@" + itoa(noSetAuthor),
		"cache.RemOutbox:mid=" + itoa(noSetAuthor) + ":fid=" + itoa(ghost),
		"cache.GetFollowers:" + itoa(noSetAuthor),
		"feed_inbox.DeleteByFeedID:" + itoa(ghost),
	})
	wantInt64sEQ(t, "缺陷：集合缺失时粉丝 ZSet 的幽灵成员留了下来", "inbox f1",
		st.cache.zmembers(inboxKey(f1)), []int64{ghost})
	wantEQ(t, "投影已被清（读侧不会再看到它）", "rows", len(st.inbox.rowsOf(f1)), 0)

	st.log.reset()
	if _, err := NewDeleteFeedLogic(context.Background(), st.svcCtx()).DeleteFeed(
		&rpc.DeleteFeedReq{Mid: emptySetAuthor, FeedId: quiet}); err != nil {
		t.Fatalf("空集合作者删除失败：%v", err)
	}
	wantCountIn(t, "空集合不产生任何 RemInbox", st.log.ops, "cache.RemInbox", 0)
	wantCountIn(t, "投影清理照旧", st.log.ops, "feed_inbox.DeleteByFeedID", 1)
}

// TestDeleteFeedSwallowsFollowerSetReadError 缺陷 D21（repository.go:553-558）：
// 粉丝集合读失败时 `err == nil && hit` 直接把整段清理跳过，方法仍返回成功。
// 后果：所有粉丝的 ZSet 成员全部残留，投影行虽被清掉（读侧看不到），
// 但每次 PullFeed 都要带着这些幽灵成员回表，且没有任何修复入口。
func TestDeleteFeedSwallowsFollowerSetReadError(t *testing.T) {
	st := newStore()
	author := int64(101)
	f1, f2 := int64(7001), int64(7002)
	st.cache.seedFollowers(author, f1, f2)
	feedID := pushOne(t, st, author, 5001)
	st.fault().failWith("cache.GetFollowers", errCacheDown)

	st.log.reset()
	_, err := NewDeleteFeedLogic(context.Background(), st.svcCtx()).DeleteFeed(
		&rpc.DeleteFeedReq{Mid: author, FeedId: feedID})
	wantNoErr(t, "缓存错误被吞（当前行为）", err)
	wantOps(t, "错误之后的清理步骤全部消失", st.log.ops, []string{
		"feed_outbox.SoftDelete:" + itoa(feedID) + "@" + itoa(author),
		"cache.RemOutbox:mid=" + itoa(author) + ":fid=" + itoa(feedID),
		"cache.GetFollowers:" + itoa(author),
		"feed_inbox.DeleteByFeedID:" + itoa(feedID),
	})
	wantInt64sEQ(t, "粉丝 1 的幽灵成员仍在", "inbox f1", st.cache.zmembers(inboxKey(f1)), []int64{feedID})
	wantInt64sEQ(t, "粉丝 2 的幽灵成员仍在", "inbox f2", st.cache.zmembers(inboxKey(f2)), []int64{feedID})

	// 读侧表现：内容看不到（主表已删），但每次都要多回一次表。
	st.log.reset()
	reply, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
		&rpc.PullFeedReq{Mid: f1, Ps: 20})
	wantNoErr(t, "幽灵成员下的拉流", err)
	wantInt64sEQ(t, "看不到已删除的动态", "items", feedIDs(reply.Items), nil)
	wantOps(t, "ZSet 读到 1 条 → 回表 1 次 → 结果为空（且不再降级回 DB）", st.log.ops, []string{
		"cache.RangeInbox:mid=" + itoa(f1) + ":cursor=0:limit=21",
		"feed_outbox.FindMany:" + itoa(feedID),
	})
}

// TestDeleteFeedSwallowsOutboxZremFailure 作者 ZSet 的 ZREM 失败也被吞（repository.go:551）。
func TestDeleteFeedSwallowsOutboxZremFailure(t *testing.T) {
	st := newStore()
	author := int64(101)
	feedID := pushOne(t, st, author, 5001)
	st.fault().failWith("cache.RemOutbox", errCacheDown)

	from := st.log.snapshot()
	_, err := NewDeleteFeedLogic(context.Background(), st.svcCtx()).DeleteFeed(
		&rpc.DeleteFeedReq{Mid: author, FeedId: feedID})
	wantNoErr(t, "ZREM 失败被吞", err)
	ops := st.log.opsFrom(from)
	wantCountIn(t, "ZREM 失败不阻断粉丝清理", ops, "feed_inbox.DeleteByFeedID", 1)
	wantInt64sEQ(t, "作者 ZSet 成员残留（主页要靠主表过滤）", "outbox",
		st.cache.zmembers(outboxKey(author)), []int64{feedID})

	st.log.reset()
	reply, err := NewListUserFeedLogic(context.Background(), st.svcCtx()).ListUserFeed(
		&rpc.ListUserFeedReq{Vmid: author, Ps: 20})
	wantNoErr(t, "主页读取", err)
	wantInt64sEQ(t, "残留成员不会漏进主页结果", "items", feedIDs(reply.Items), nil)
}

// TestDeleteFeedPartialFollowerCleanupFailure 爆炸半径用「哪个粉丝的 ZSet 还剩成员」锁：
// 第 1 个粉丝的 ZREM 失败被吞，第 2 个仍然被清；方法返回成功。
func TestDeleteFeedPartialFollowerCleanupFailure(t *testing.T) {
	st := newStore()
	author := int64(101)
	f1, f2, f3 := int64(7001), int64(7002), int64(7003)
	st.cache.seedFollowers(author, f1, f2, f3)
	feedID := pushOne(t, st, author, 5001)
	st.fault().failOn("cache.RemInbox", 1, errCacheDown)

	from := st.log.snapshot()
	_, err := NewDeleteFeedLogic(context.Background(), st.svcCtx()).DeleteFeed(
		&rpc.DeleteFeedReq{Mid: author, FeedId: feedID})
	wantNoErr(t, "单个粉丝清理失败被吞", err)
	ops := st.log.opsFrom(from)
	wantCountIn(t, "三个粉丝都试过一次 ZREM", ops, "cache.RemInbox", 3)
	wantInt64sEQ(t, "失败的那个成员残留", "inbox f1", st.cache.zmembers(inboxKey(f1)), []int64{feedID})
	wantInt64sEQ(t, "成功的两个清空", "inbox f2", st.cache.zmembers(inboxKey(f2)), nil)
	wantInt64sEQ(t, "成功的两个清空", "inbox f3", st.cache.zmembers(inboxKey(f3)), nil)
}

// TestDeleteFeedProjectionFailureAndRetrySemantics 缺陷 D22 的可观测面（model/feedmodel.go:104-119 +
// repository.go:545-560）：最后一步的错误会返回，但此时主表与所有 ZSet 都已清理完毕。
// 能否靠重试修好，取决于 SoftDelete 的 WHERE 有没有 state 条件——
// 它只按 (id, mid) 匹配，所以跨秒重试能整段重跑（下面钉住），
// 同秒重试则因为 changed rows = 0 拿到 ErrFeedNotFound，投影行永远留在表里。
// 也就是说：删除的幂等语义挂在墙上时钟的秒边界上，投影行没有独立清理入口。
func TestDeleteFeedProjectionFailureAndRetrySemantics(t *testing.T) {
	st := newStore()
	author := int64(101)
	f1 := int64(7001)
	st.cache.seedFollowers(author, f1)
	feedID := pushOne(t, st, author, 5001)
	st.fault().failWith("feed_inbox.DeleteByFeedID", errInjected)

	st.log.reset()
	_, err := NewDeleteFeedLogic(context.Background(), st.svcCtx()).DeleteFeed(
		&rpc.DeleteFeedReq{Mid: author, FeedId: feedID})
	wantErrIs(t, "投影清理失败必须报错（不静默）", err, errInjected)
	wantOps(t, "报错发生在最后一步", st.log.ops, []string{
		"feed_outbox.SoftDelete:" + itoa(feedID) + "@" + itoa(author),
		"cache.RemOutbox:mid=" + itoa(author) + ":fid=" + itoa(feedID),
		"cache.GetFollowers:" + itoa(author),
		"cache.RemInbox:mid=" + itoa(f1) + ":fid=" + itoa(feedID),
		"feed_inbox.DeleteByFeedID:" + itoa(feedID),
	})
	inboxState, ok := st.inbox.stateOf(f1, feedID)
	wantEQ(t, "投影行还在", "found", ok, true)
	wantEQ(t, "投影行还是正常态（没被清）", "state", inboxState, model.FeedStateNormal)

	// 跨秒重试分支：整段重跑，投影行被补清——这是本服务里唯一能修它的路径。
	st.fault().failWith("feed_inbox.DeleteByFeedID", nil)
	st.log.reset()
	_, err = NewDeleteFeedLogic(context.Background(), st.svcCtx()).DeleteFeed(
		&rpc.DeleteFeedReq{Mid: author, FeedId: feedID})
	wantNoErr(t, "重试（跨秒分支）仍可进入清理", err)
	wantOps(t, "重跑的序列与第一次逐字相同", st.log.ops, []string{
		"feed_outbox.SoftDelete:" + itoa(feedID) + "@" + itoa(author),
		"cache.RemOutbox:mid=" + itoa(author) + ":fid=" + itoa(feedID),
		"cache.GetFollowers:" + itoa(author),
		"cache.RemInbox:mid=" + itoa(f1) + ":fid=" + itoa(feedID),
		"feed_inbox.DeleteByFeedID:" + itoa(feedID),
	})
	inboxState, ok = st.inbox.stateOf(f1, feedID)
	wantEQ(t, "重试后投影行存在", "found", ok, true)
	wantEQ(t, "重试后投影行已删除", "state", inboxState, model.FeedStateDeleted)
}

// TestDeleteFeedRaceWithFanoutLeavesGhostMember 并发窗口：
// DeleteFeed 在 t0 读粉丝快照、t1 清 ZSet；期间新增的关注回填把这条动态写进了第三个粉丝的 ZSet。
// 由于第 4 步按 feed_id 清投影，DB 侧干净，但缓存里多出一个谁也不会再清理的幽灵成员。
// repository.go:552-560（读快照与清理之间没有原子性，也没有版本号）。
func TestDeleteFeedRaceWithFanoutLeavesGhostMember(t *testing.T) {
	st := newStore()
	author := int64(101)
	f1, f2 := int64(7001), int64(7002)
	st.cache.seedFollowers(author, f1, f2)
	feedID := pushOne(t, st, author, 5001)
	ctime := st.outbox.rows[feedID].Ctime

	st.raceBefore("feed_inbox.DeleteByFeedID", func() {
		// 关注回填：新粉丝的 ZSet 在这一瞬被写入这条动态。
		st.cache.seedZSet(inboxKey(f2), feedID, ctime)
		st.cache.seedZSet(inboxKey(f1), feedID, ctime)
	})
	_, err := NewDeleteFeedLogic(context.Background(), st.svcCtx()).DeleteFeed(
		&rpc.DeleteFeedReq{Mid: author, FeedId: feedID})
	wantNoErr(t, "并发窗口内的删除", err)
	st.checkRaces(t)
	wantInt64sEQ(t, "两个粉丝的 ZSet 都留下了幽灵成员", "inbox f1",
		st.cache.zmembers(inboxKey(f1)), []int64{feedID})
	wantInt64sEQ(t, "两个粉丝的 ZSet 都留下了幽灵成员", "inbox f2",
		st.cache.zmembers(inboxKey(f2)), []int64{feedID})
	wantEQ(t, "投影已被按 feed_id 清干净", "rows f1", len(st.inbox.rowsOf(f1)), 0)

	st.log.reset()
	reply, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
		&rpc.PullFeedReq{Mid: f1, Ps: 20})
	wantNoErr(t, "幽灵成员拉流", err)
	wantInt64sEQ(t, "内容不可见", "items", feedIDs(reply.Items), nil)
	wantEQ(t, "也不会有下一页", "has_more", reply.HasMore, false)
	wantEQ(t, "游标归零（不会被幽灵成员推着往前走）", "next_cursor", reply.NextCursor, int64(0))
}

// TestDeleteFeedRaceWithUnfollowIsBenign 窗口里取关是无害的：
// 快照里还有这个 mid，ZREM 只是删一个不存在的成员，不产生新的残留。
func TestDeleteFeedRaceWithUnfollowIsBenign(t *testing.T) {
	st := newStore()
	author := int64(101)
	f1, f2 := int64(7001), int64(7002)
	st.cache.seedFollowers(author, f1, f2)
	feedID := pushOne(t, st, author, 5001)

	st.raceBefore("cache.RemInbox", func() {
		st.cache.dropFollower(author, f2) // 取关事件在清理之前生效
	})
	_, err := NewDeleteFeedLogic(context.Background(), st.svcCtx()).DeleteFeed(
		&rpc.DeleteFeedReq{Mid: author, FeedId: feedID})
	wantNoErr(t, "取关窗口内的删除", err)
	st.checkRaces(t)
	wantInt64sEQ(t, "取关者的 ZSet 也被顺手清干净", "inbox f2", st.cache.zmembers(inboxKey(f2)), nil)
	wantInt64sEQ(t, "留下的那个同样清干净", "inbox f1", st.cache.zmembers(inboxKey(f1)), nil)
}

// TestDeleteFeedLeavesPinsBehind 缺陷 D17（另半）：删除不动 feed_pin / feed:pin。
// 后果：置顶行指向一条已删除动态，而且 PinFeed 的归属校验此时会拒绝，
// 唯一的清理入口是 UnpinFeed（它不查主表）。
func TestDeleteFeedLeavesPinsBehind(t *testing.T) {
	st := newStore()
	author := int64(101)
	feedID := pushOne(t, st, author, 5001)
	if _, err := NewPinFeedLogic(context.Background(), st.svcCtx()).PinFeed(
		&rpc.PinFeedReq{Mid: author, FeedId: feedID}); err != nil {
		t.Fatalf("布防置顶失败：%v", err)
	}

	from := st.log.snapshot()
	if _, err := NewDeleteFeedLogic(context.Background(), st.svcCtx()).DeleteFeed(
		&rpc.DeleteFeedReq{Mid: author, FeedId: feedID}); err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	ops := st.log.opsFrom(from)
	wantCountIn(t, "删除路径不碰 feed_pin", ops, "feed_pin.Del", 0)
	wantCountIn(t, "删除路径不碰置顶集合", ops, "cache.RemPin", 0)

	pinState, ok := st.pin.stateOf(author, feedID)
	wantEQ(t, "置顶行仍在", "found", ok, true)
	wantEQ(t, "置顶行仍是正常态（指向已删除动态）", "state", pinState, model.PinStateNormal)
	wantInt64sEQ(t, "缓存置顶集合也还留着成员", "pins", st.cache.pinMembers(author), []int64{feedID})

	// 孤儿置顶只能靠 UnpinFeed 清理；PinFeed 此时会被归属校验拒绝。
	_, err := NewPinFeedLogic(context.Background(), st.svcCtx()).PinFeed(
		&rpc.PinFeedReq{Mid: author, FeedId: feedID})
	wantErrIs(t, "已删除的动态不能再置顶", err, model.ErrFeedNotFound)
	if _, err := NewUnpinFeedLogic(context.Background(), st.svcCtx()).UnpinFeed(
		&rpc.UnpinFeedReq{Mid: author, FeedId: feedID}); err != nil {
		t.Fatalf("取消孤儿置顶失败：%v", err)
	}
	wantInt64sEQ(t, "取消后集合清空", "pins", st.cache.pinMembers(author), nil)
}
