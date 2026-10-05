package logic

// pushfeed_logic_test.go 锁写扩散链路的四条结论：
//   - fan-out 的**顺序与条数**（每个粉丝恰好「ZSet → 投影表 → 缓存计数 → DB 计数」四步）；
//   - **没有扇出上限**（README 声称本期不做限流）与**部分失败不上报**（爆炸半径只能用行数差看到）；
//   - **没有幂等键**（AGENTS.md §5 要求写接口具备，本域三条唯一约束都用不上）；
//   - **ctime 完全不被校验**，负值能落库但走不到缓存读侧。

import (
	"context"
	"strconv"
	"testing"
	"time"

	"go-video/services/feed/rpc"
)

// opsFanoutOne 拼一个粉丝的 fan-out 四条期望调用。
func opsFanoutOne(mid, feedID int64) []string {
	return []string{
		"cache.AddInbox:mid=" + itoa(mid) + ":fid=" + itoa(feedID),
		"feed_inbox.Add:mid=" + itoa(mid) + ":fid=" + itoa(feedID),
		"cache.IncrUnread:mid=" + itoa(mid) + ":+1",
		"feed_unread.IncrBy:mid=" + itoa(mid) + ":+1",
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestPushFeedFansOutToEveryFollower(t *testing.T) {
	st := newStore()
	st.cache.seedFollowers(101, 201, 202, 203)
	st.cache.seedUnread(201, 5)
	st.unread.seed(201, 5)

	from := st.log.snapshot()
	reply, err := NewPushFeedLogic(context.Background(), st.svcCtx()).PushFeed(&rpc.PushFeedReq{
		Mid: 101, Oid: 5001, Otype: rpc.OType_OTYPE_UGC_VIDEO, Action: rpc.Action_ACTION_PUBLISH,
		Ctime: 1_700, Title: "hello", Cover: "c.png", Uri: "bilibili://video/5001", Source: "video",
	})
	wantNoErr(t, "写扩散推送", err)
	// 契约只回 EmptyReply：新建的 feed_id 不回传（缺陷 D2），调用方无法确认落到了哪一行。
	wantEQ(t, "响应", "reply 非空", reply != nil, true)
	wantEQ(t, "响应", "字段数", reply.ProtoReflect().Descriptor().Fields().Len(), 0)

	feedID := st.outbox.lastID() // 自增主键不可预知：先读回再拼期望
	want := append([]string{
		"feed_outbox.Insert:mid=101:oid=5001",
		"cache.AddOutbox:mid=101:fid=" + itoa(feedID),
		"cache.GetFollowers:101",
	}, opsFanoutOne(201, feedID)...)
	want = append(want, opsFanoutOne(202, feedID)...)
	want = append(want, opsFanoutOne(203, feedID)...)
	wantOps(t, "扇出序列", st.log.opsFrom(from), want)

	// ZSet score 必须等于落库 ctime：repository 复用 Insert 写回的 f.Ctime（model/feedmodel.go:51-56）。
	for _, follower := range []int64{201, 202, 203} {
		score, ok := st.cache.zscore(inboxKey(follower), feedID)
		if !ok {
			t.Fatalf("粉丝 %d 的收件箱 ZSet 里没有这条动态", follower)
		}
		wantEQ(t, "收件箱 ZSet score", "ctime", score, int64(1_700))
	}
	outScore, ok := st.cache.zscore(outboxKey(101), feedID)
	if !ok {
		t.Fatalf("作者发件箱 ZSet 里没有这条动态")
	}
	wantEQ(t, "发件箱 ZSet score", "ctime", outScore, int64(1_700))

	// 投影表：每人一行，author_mid 与 ctime 与主表一致。
	for _, follower := range []int64{201, 202, 203} {
		wantInt64sEQ(t, "粉丝收件箱投影行数", "rows", st.inbox.rowsOf(follower), []int64{feedID})
	}
	wantEQ(t, "主表 mid", "mid", st.outbox.rows[feedID].Mid, int64(101))
	wantEQ(t, "主表 title", "title", st.outbox.rows[feedID].Title, "hello")
	wantEQ(t, "主表 state", "state", st.outbox.rows[feedID].State, int32(0))

	// 未读：热计数器与 DB 投影同步各 +1（201 从 5 → 6，202/203 从缺失 → 1）。
	v201, _ := st.cache.unreadValue(201)
	wantEQ(t, "粉丝 201 缓存未读", "unread", v201, int64(6))
	wantEQ(t, "粉丝 201 DB 未读", "unread", st.unread.value(201), int64(6))
	v203, ok := st.cache.unreadValue(203)
	if !ok {
		t.Fatalf("粉丝 203 的未读计数器没有被 INCRBY 创建")
	}
	wantEQ(t, "粉丝 203 缓存未读", "unread", v203, int64(1))
	wantEQ(t, "粉丝 203 DB 未读", "unread", st.unread.value(203), int64(1))
}

// TestPushFeedServerSideCtime 锁 ctime=0 的口径（proto 第 62 行：0 表示服务端取当前时间）：
// 非确定值先读回再拼期望，同时卡住「不能是 0」这个真实结论。
func TestPushFeedServerSideCtime(t *testing.T) {
	st := newStore()
	st.cache.seedFollowers(101, 201)
	start := time.Now().Unix()
	_, err := NewPushFeedLogic(context.Background(), st.svcCtx()).PushFeed(&rpc.PushFeedReq{
		Mid: 101, Oid: 5001, Otype: rpc.OType_OTYPE_UGC_VIDEO, Action: rpc.Action_ACTION_PUBLISH, Ctime: 0,
	})
	wantNoErr(t, "ctime=0 推送", err)
	end := time.Now().Unix()

	feedID := st.outbox.lastID()
	ctime := st.outbox.rows[feedID].Ctime
	if ctime < start || ctime > end {
		t.Fatalf("主表 ctime = %d, want ∈ [%d,%d]（服务端补时）", ctime, start, end)
	}
	// ZSet 与投影表用的必须同一个补出来的 ctime，而不是 0（否则整条动态沉底、永远拉不到）。
	score, _ := st.cache.zscore(inboxKey(201), feedID)
	wantEQ(t, "收件箱 score 与主表一致", "ctime", score, ctime)
	outScore, _ := st.cache.zscore(outboxKey(101), feedID)
	wantEQ(t, "发件箱 score 与主表一致", "ctime", outScore, ctime)
	if len(st.inbox.rows) != 1 || st.inbox.rows[0].Ctime != ctime {
		t.Fatalf("feed_inbox 投影 ctime = %+v, want [%d]", st.inbox.rows, ctime)
	}
}

// TestPushFeedFollowerSetMissingVersusEmpty 区分两种「不扇出」的成因，
// 但钉住它们的**可观测结果完全相同**（repository.go:359-366 把 hit=false 与空集合并列）：
// 集合未初始化（social-graph 事件还没到）与确实无粉丝，调用方无从分辨。
func TestPushFeedFollowerSetMissingVersusEmpty(t *testing.T) {
	missing := newStore()
	_, err := NewPushFeedLogic(context.Background(), missing.svcCtx()).PushFeed(
		&rpc.PushFeedReq{Mid: 101, Oid: 5001, Ctime: 1_700})
	wantNoErr(t, "粉丝集合缺失", err)
	missingID := missing.outbox.lastID()
	wantOps(t, "粉丝集合缺失", missing.log.ops, []string{
		"feed_outbox.Insert:mid=101:oid=5001",
		"cache.AddOutbox:mid=101:fid=" + itoa(missingID),
		"cache.GetFollowers:101",
	})

	empty := newStore()
	empty.cache.seedFollowers(101) // EXISTS=1 但 SMEMBERS 为空
	_, err = NewPushFeedLogic(context.Background(), empty.svcCtx()).PushFeed(
		&rpc.PushFeedReq{Mid: 101, Oid: 5001, Ctime: 1_700})
	wantNoErr(t, "粉丝集合为空", err)
	emptyID := empty.outbox.lastID()
	wantOps(t, "粉丝集合为空", empty.log.ops, []string{
		"feed_outbox.Insert:mid=101:oid=5001",
		"cache.AddOutbox:mid=101:fid=" + itoa(emptyID),
		"cache.GetFollowers:101",
	})
	wantEQ(t, "两种成因的投影结果", "inbox 行数", len(empty.inbox.rows), len(missing.inbox.rows))
}

// TestPushFeedSwallowsFollowerSetError 钉住缺陷 D8（repository.go:360-362）：
// 粉丝集合读失败时**返回成功且静默不扇出**，调用方无法区分「无粉丝」与「Redis 故障」，
// 因此没有重试/补偿入口，这条动态对全体粉丝永久不可见（outbox 里有、inbox 里没有）。
func TestPushFeedSwallowsFollowerSetError(t *testing.T) {
	st := newStore()
	st.cache.seedFollowers(101, 201, 202)
	st.fault().failWith("cache.GetFollowers", errCacheDown)

	from := st.log.snapshot()
	_, err := NewPushFeedLogic(context.Background(), st.svcCtx()).PushFeed(
		&rpc.PushFeedReq{Mid: 101, Oid: 5001, Ctime: 1_700})
	wantNoErr(t, "Redis 故障时 PushFeed 仍报成功", err)
	feedID := st.outbox.lastID()
	wantOps(t, "扇出被整段跳过", st.log.opsFrom(from), []string{
		"feed_outbox.Insert:mid=101:oid=5001",
		"cache.AddOutbox:mid=101:fid=" + itoa(feedID),
		"cache.GetFollowers:101",
	})
	wantEQ(t, "投影行数（应为 0：故障被吞）", "inbox rows", len(st.inbox.rows), 0)
	wantInt64sEQ(t, "主表仍然落地", "outbox ids", []int64{feedID}, []int64{feedID})
}

// TestPushFeedSwallowsOutboxZAddError 钉住缺陷 D9（repository.go:354-357 的 `_ = err`）：
// 作者发件箱 ZSet 写失败被吞，但扇出照常继续 → **粉丝关注流有、作者主页没有**，
// 而 ListUserFeed 没有 DB 回源（repository.go:417-427），这条动态在主页侧永久缺失。
func TestPushFeedSwallowsOutboxZAddError(t *testing.T) {
	st := newStore()
	st.cache.seedFollowers(101, 201)
	st.fault().failWith("cache.AddOutbox", errCacheDown)

	_, err := NewPushFeedLogic(context.Background(), st.svcCtx()).PushFeed(
		&rpc.PushFeedReq{Mid: 101, Oid: 5001, Ctime: 1_700})
	wantNoErr(t, "发件箱 ZSet 写失败仍报成功", err)
	feedID := st.outbox.lastID()

	if _, ok := st.cache.zscore(outboxKey(101), feedID); ok {
		t.Fatalf("发件箱 ZSet 不该有成员（写入被注入为失败）")
	}
	wantInt64sEQ(t, "粉丝收件箱仍已扇出", "inbox rows of 201", st.inbox.rowsOf(201), []int64{feedID})

	st.log.reset()
	home, err := NewListUserFeedLogic(context.Background(), st.svcCtx()).ListUserFeed(
		&rpc.ListUserFeedReq{Vmid: 101, Ps: 20})
	wantNoErr(t, "主页读取", err)
	wantOps(t, "主页只读 ZSet，不回源 outbox 表", st.log.ops, []string{
		"cache.RangeOutbox:mid=101:cursor=0:limit=21",
	})
	wantEQ(t, "作者主页看不到自己刚发的动态（无 DB 回源）", "items", len(home.Items), 0)
}

// TestPushFeedPartialFanOutIsInvisible 用行数差锁死爆炸半径（缺陷 D10，repository.go:368-378）：
// fan-out 循环里四条写全部 `_ =` 吞错，只有第 2 个粉丝的投影写失败时，
// 调用方拿到的仍是成功，而**未读计数(3) 比可读投影行数(2) 多 1**——
// 计数是投影值而非实时算，所以这个差值不会被任何读路径纠正。
func TestPushFeedPartialFanOutIsInvisible(t *testing.T) {
	st := newStore()
	st.cache.seedFollowers(101, 201, 202, 203)
	// 只让第 2 次 feed_inbox.Add 发作（202 那一站）。
	st.fault().failOn("feed_inbox.Add", 2, errInjected)

	from := st.log.snapshot()
	_, err := NewPushFeedLogic(context.Background(), st.svcCtx()).PushFeed(
		&rpc.PushFeedReq{Mid: 101, Oid: 5001, Ctime: 1_700})
	wantNoErr(t, "部分失败仍报成功", err)

	feedID := st.outbox.lastID()
	ops := st.log.opsFrom(from)
	wantCountIn(t, "ZSet 扇出次数", ops, "cache.AddInbox:", 3)
	wantCountIn(t, "投影写入尝试次数", ops, "feed_inbox.Add:", 3)
	wantCountIn(t, "缓存计数次数", ops, "cache.IncrUnread:", 3)
	wantCountIn(t, "DB 计数次数", ops, "feed_unread.IncrBy:", 3)

	// 爆炸半径：投影少一行，计数不少。
	wantInt64sEQ(t, "粉丝 201 投影", "rows", st.inbox.rowsOf(201), []int64{feedID})
	wantInt64sEQ(t, "粉丝 203 投影", "rows", st.inbox.rowsOf(203), []int64{feedID})
	wantInt64sEQ(t, "粉丝 202 投影（写失败）", "rows", st.inbox.rowsOf(202), nil)
	for _, follower := range []int64{201, 202, 203} {
		if _, ok := st.cache.zscore(inboxKey(follower), feedID); !ok {
			t.Fatalf("粉丝 %d 的 ZSet 应已写入（AddInbox 在投影之前）", follower)
		}
		wantEQ(t, "三个粉丝的未读计数都被推进", "db unread", st.unread.value(follower), int64(1))
	}
	wantEQ(t, "投影总行数 vs 计数推进数（差 1）", "inbox rows", len(st.inbox.rows), 2)

	// 202 的未读数指向一条自己库里没有的动态：读侧仍会把它算成未读。
	unread, err := NewGetUnreadCountLogic(context.Background(), st.svcCtx()).GetUnreadCount(&rpc.MidReq{Mid: 202})
	wantNoErr(t, "读 202 未读", err)
	wantEQ(t, "202 未读数（虚高）", "unread", unread.Unread, int64(1))
}

// TestPushFeedInboxAddFailureAfterZAdd 锁「同一站里第 1 次就失败」的极端：
// ZSet 已写、投影与计数全失败 → 关注流里能看到这条，但重启用后 ZSet 丢失就再也看不到。
func TestPushFeedInboxAddFailureAfterZAdd(t *testing.T) {
	st := newStore()
	st.cache.seedFollowers(101, 201)
	st.fault().failWith("feed_inbox.Add", errInjected)

	_, err := NewPushFeedLogic(context.Background(), st.svcCtx()).PushFeed(
		&rpc.PushFeedReq{Mid: 101, Oid: 5001, Ctime: 1_700})
	wantNoErr(t, "投影写失败仍报成功", err)
	feedID := st.outbox.lastID()
	wantEQ(t, "投影行数", "inbox rows", len(st.inbox.rows), 0)
	if _, ok := st.cache.zscore(inboxKey(201), feedID); !ok {
		t.Fatalf("ZSet 应先于投影写入（repository.go:369-376 的顺序）")
	}
}

// TestPushFeedHasNoIdempotencyKey 钉住缺陷 D1：
// PushFeed 没有幂等键（请求体里 Source/Operator 被丢弃 → 溯源丢失另记 D23，
// 主表也没有唯一约束，
// deploy/migrations/feed/000001_create_feed_tables.sql:11-25），
// 同一条稿件被重复推送就是 N 条动态 + N 次未读，粉丝看到重复内容。
func TestPushFeedHasNoIdempotencyKey(t *testing.T) {
	st := newStore()
	st.cache.seedFollowers(101, 201)
	req := &rpc.PushFeedReq{
		Mid: 101, Oid: 5001, Otype: rpc.OType_OTYPE_UGC_VIDEO,
		Action: rpc.Action_ACTION_PUBLISH, Ctime: 1_700, Source: "video",
	}
	_, err := NewPushFeedLogic(context.Background(), st.svcCtx()).PushFeed(req)
	wantNoErr(t, "第一次推送", err)
	firstID := st.outbox.lastID()
	st.log.reset()
	_, err = NewPushFeedLogic(context.Background(), st.svcCtx()).PushFeed(req)
	wantNoErr(t, "第二次同内容推送（不报错）", err)
	secondID := st.outbox.lastID()

	if firstID == secondID {
		t.Fatalf("两次推送复用了同一个 feed_id：%d，替身与实现不符", firstID)
	}
	wantCountIn(t, "第二次推送重新走完整扇出", st.log.ops, "feed_inbox.Add:", 1)
	wantInt64sEQ(t, "粉丝收件箱出现两条重复动态", "rows", st.inbox.rowsOf(201), []int64{secondID, firstID})
	v, _ := st.cache.unreadValue(201)
	wantEQ(t, "未读数被推了两遍", "unread", v, int64(2))
}

// TestPushFeedNegativeCtimeIsInvisibleOnHomeButVisibleInFollowStream 钉住缺陷 D4：
// ctime 不校验（pushfeedlogic.go:37-47 直接透传），负值能落库，
// 但 ZSet 区间读的 min 固定 0（repository.go:115 / 156），于是：
//   - 作者主页（ListUserFeed 只读 ZSet）永远看不到这条；
//   - 粉丝关注流（PullFeed 有 DB 回源）反而能读到它，并把它按 -5 回填进 ZSet——
//     回填是无效写，下一次读依旧落到 DB 回源，缓存永远热不起来。
func TestPushFeedNegativeCtimeIsInvisibleOnHomeButVisibleInFollowStream(t *testing.T) {
	st := newStore()
	st.cache.seedFollowers(101, 201)
	_, err := NewPushFeedLogic(context.Background(), st.svcCtx()).PushFeed(
		&rpc.PushFeedReq{Mid: 101, Oid: 5001, Ctime: -5})
	wantNoErr(t, "负 ctime 推送", err)
	feedID := st.outbox.lastID()
	wantEQ(t, "负 ctime 照样入库", "ctime", st.outbox.rows[feedID].Ctime, int64(-5))

	st.log.reset()
	pull, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(&rpc.PullFeedReq{Mid: 201, Ps: 20})
	wantNoErr(t, "关注流读取", err)
	wantOps(t, "ZSet 读空 → 回源 DB → 无效回填", st.log.ops, []string{
		"cache.RangeInbox:mid=201:cursor=0:limit=21",
		"feed_inbox.ListByMid:mid=201:cursor=0:limit=21",
		"cache.AddInbox:mid=201:fid=" + itoa(feedID),
		"feed_outbox.FindMany:" + itoa(feedID),
	})
	wantInt64sEQ(t, "关注流能读到负 ctime 动态", "items", feedIDs(pull.Items), []int64{feedID})

	st.log.reset()
	home, err := NewListUserFeedLogic(context.Background(), st.svcCtx()).ListUserFeed(
		&rpc.ListUserFeedReq{Vmid: 101, Ps: 20})
	wantNoErr(t, "主页读取", err)
	wantOps(t, "主页只读 ZSet（负分被 min=0 过滤，且没有回源）", st.log.ops, []string{
		"cache.RangeOutbox:mid=101:cursor=0:limit=21",
	})
	wantEQ(t, "作者主页读不到自己刚发的动态", "items", len(home.Items), 0)
}
