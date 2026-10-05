package logic

// listuserfeed_logic_test.go 锁作者主页读侧：
//   - 读的是 feed:outbox:{vmid}，与关注流 feed:inbox:{mid} 是两条键空间，不串；
//   - **没有任何 DB 回源**（与同文件的 PullFeed 不对称，缺陷 D7）：ZSet 一旦丢失，
//     作者主页就是空白，即使 feed_outbox 表里行都在；
//   - 已删除/缺失成员会被过滤，但游标口径与 PullFeed 共用同一段代码（缺陷 D11/D12 同样适用）。

import (
	"context"
	"testing"

	"go-video/services/feed/model"
	"go-video/services/feed/rpc"
)

func TestListUserFeedReadsOutboxOfViewedUser(t *testing.T) {
	st := newStore()
	author := int64(101)
	a1 := st.outbox.seed(author, 5001, 1_000)
	a2 := st.outbox.seed(author, 5002, 900)
	st.cache.seedZSet(outboxKey(author), a1.ID, a1.Ctime)
	st.cache.seedZSet(outboxKey(author), a2.ID, a2.Ctime)
	// 访客自己也是内容生产者：主页读取绝不能落到访客的键空间上。
	visitor := int64(201)
	v1 := st.outbox.seed(visitor, 5101, 800)
	st.cache.seedZSet(outboxKey(visitor), v1.ID, v1.Ctime)
	st.cache.seedZSet(inboxKey(visitor), a1.ID, a1.Ctime)

	st.log.reset()
	reply, err := NewListUserFeedLogic(context.Background(), st.svcCtx()).ListUserFeed(
		&rpc.ListUserFeedReq{Vmid: author, Mid: visitor, Ps: 20})
	wantNoErr(t, "主页读取", err)
	wantOps(t, "只读被访问者的发件箱", st.log.ops, []string{
		"cache.RangeOutbox:mid=101:cursor=0:limit=21",
		"feed_outbox.FindMany:" + joinInt64([]int64{a1.ID, a2.ID}),
	})
	wantInt64sEQ(t, "主页条目（ctime 倒序）", "items", feedIDs(reply.Items), []int64{a1.ID, a2.ID})
}

// TestListUserFeedHasNoDbFallback 钉住缺陷 D7（repository.go:417-427 对比 395-412）：
// 发件箱 ZSet 为空时直接返回空，不回源 feed_outbox（PullFeed 有这条支路）。
// 现实触发路径很多：Redis 逐出、键被误删、以及 PushFeed 的 AddOutbox 失败被吞（缺陷 D9）。
func TestListUserFeedHasNoDbFallback(t *testing.T) {
	st := newStore()
	author := int64(101)
	alive := st.outbox.seed(author, 5001, 1_000)
	st.outbox.seed(author, 5002, 900)

	reply, err := NewListUserFeedLogic(context.Background(), st.svcCtx()).ListUserFeed(
		&rpc.ListUserFeedReq{Vmid: author, Ps: 20})
	wantNoErr(t, "ZSet 空", err)
	wantOps(t, "一次 ZSet 读，零次回表，也没有按 mid 扫主表的支路", st.log.ops, []string{
		"cache.RangeOutbox:mid=101:cursor=0:limit=21",
	})
	wantEQ(t, "作者主页空白（表里还有 2 行）", "items", len(reply.Items), 0)
	wantEQ(t, "空白页不该骗客户端还有下一页", "has_more", reply.HasMore, false)
	wantEQ(t, "库里存活动态的 ID 只是没被读到", "row exists", st.outbox.rows[alive.ID] != nil, true)
}

func TestListUserFeedSkipsDeletedAndMissingMembers(t *testing.T) {
	st := newStore()
	author := int64(101)
	alive := st.outbox.seed(author, 5001, 1_000)
	deleted := st.outbox.put(&model.FeedOutbox{
		Mid: author, Oid: 5002, Ctime: 900, State: model.FeedStateDeleted,
	})
	st.cache.seedZSet(outboxKey(author), alive.ID, alive.Ctime)
	st.cache.seedZSet(outboxKey(author), deleted.ID, deleted.Ctime)
	st.cache.seedZSet(outboxKey(author), 999, 800) // 主表没有的幽灵成员

	st.log.reset()
	reply, err := NewListUserFeedLogic(context.Background(), st.svcCtx()).ListUserFeed(
		&rpc.ListUserFeedReq{Vmid: author, Ps: 20})
	wantNoErr(t, "混合成员", err)
	wantOps(t, "三个成员一起回表，ZSet 顺序保留", st.log.ops, []string{
		"cache.RangeOutbox:mid=101:cursor=0:limit=21",
		"feed_outbox.FindMany:" + joinInt64([]int64{alive.ID, deleted.ID, 999}),
	})
	wantInt64sEQ(t, "已删除与幽灵成员都不出现", "items", feedIDs(reply.Items), []int64{alive.ID})
	wantEQ(t, "过滤不影响探针口径（只读到 3 条 < limit）", "has_more", reply.HasMore, false)
}

func TestListUserFeedPagesForward(t *testing.T) {
	st := newStore()
	author := int64(101)
	var ids []int64
	for i, ctime := range []int64{1_500, 1_400, 1_300} {
		row := st.outbox.seed(author, int64(5000+i), ctime)
		st.cache.seedZSet(outboxKey(author), row.ID, row.Ctime)
		ids = append(ids, row.ID)
	}

	var seen []int64
	var cursor int64
	for page := 1; page <= 3; page++ {
		reply, err := NewListUserFeedLogic(context.Background(), st.svcCtx()).ListUserFeed(
			&rpc.ListUserFeedReq{Vmid: author, Cursor: cursor, Ps: 1})
		wantNoErr(t, "主页翻页", err)
		seen = append(seen, feedIDs(reply.Items)...)
		if !reply.HasMore {
			wantEQ(t, "末页游标归零", "next_cursor", reply.NextCursor, int64(0))
			break
		}
		cursor = reply.NextCursor
	}
	wantInt64sEQ(t, "三页合起来不重不漏（ctime 互异时）", "seen", seen, ids)
}

func TestListUserFeedCursorGuardAndErrorPassthrough(t *testing.T) {
	st := newStore()
	author := int64(101)
	row := st.outbox.seed(author, 5001, 1_000)
	st.cache.seedZSet(outboxKey(author), row.ID, row.Ctime)

	from := st.log.snapshot()
	_, err := NewListUserFeedLogic(context.Background(), st.svcCtx()).ListUserFeed(
		&rpc.ListUserFeedReq{Vmid: author, Cursor: -1})
	wantErrIs(t, "负游标", err, model.ErrInvalidCursor)
	wantNoCall(t, "负游标", st, from)

	st.fault().failWith("cache.RangeOutbox", errCacheDown)
	st.log.reset()
	reply, err := NewListUserFeedLogic(context.Background(), st.svcCtx()).ListUserFeed(
		&rpc.ListUserFeedReq{Vmid: author, Ps: 20})
	wantErrIs(t, "ZSet 读错误原样透传（不降级）", err, errCacheDown)
	if reply != nil {
		t.Fatalf("出错时不应返回响应：%+v", reply)
	}
	wantOps(t, "缓存故障时没有第二条读路径", st.log.ops, []string{
		"cache.RangeOutbox:mid=101:cursor=0:limit=21",
	})
}
