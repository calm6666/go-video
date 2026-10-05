package logic

// guards_logic_test.go 锁 8 个 rpc 方法的入参守卫：
// 拒绝必须在触库/触缓存**之前**发生（wantNoCall 保证一次依赖调用都没发生），
// 并且错误就是 model 包里那个哨兵本身，没有被换成别的语义。

import (
	"context"
	"strconv"
	"testing"

	"go-video/services/feed/model"
	"go-video/services/feed/rpc"
)

func TestPushFeedGuards(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.PushFeedReq
		want error
	}{
		{"mid 为 0", &rpc.PushFeedReq{Mid: 0, Oid: 5001}, model.ErrInvalidMid},
		{"mid 为负", &rpc.PushFeedReq{Mid: -1, Oid: 5001}, model.ErrInvalidMid},
		{"oid 为 0", &rpc.PushFeedReq{Mid: 101, Oid: 0}, model.ErrInvalidOid},
		{"oid 为负", &rpc.PushFeedReq{Mid: 101, Oid: -7}, model.ErrInvalidOid},
		// 守卫顺序：两个字段同时非法时先报 mid（pushfeedlogic.go:31-36 的判断次序）。
		{"mid 与 oid 同时非法先报 mid", &rpc.PushFeedReq{Mid: 0, Oid: 0}, model.ErrInvalidMid},
	}
	for _, tc := range cases {
		st := newStore()
		_, err := NewPushFeedLogic(context.Background(), st.svcCtx()).PushFeed(tc.in)
		wantErrIs(t, tc.name, err, tc.want)
		wantNoCall(t, tc.name, st, 0)
	}
}

// TestPushFeedAcceptsUnspecifiedOtypeAndAction 钉住**当前真实行为**（缺陷 D3）：
// otype/action 完全不校验，OTYPE_UNSPECIFIED(0)、ACTION_UNSPECIFIED(0) 会照写入库。
// 哪天加了校验，本用例会红，提醒同步契约与文档，而不是悄悄改变入库口径。
func TestPushFeedAcceptsUnspecifiedOtypeAndAction(t *testing.T) {
	st := newStore()
	st.cache.seedFollowers(101) // 空粉丝集合：只走 outbox，便于单独看入库字段
	_, err := NewPushFeedLogic(context.Background(), st.svcCtx()).PushFeed(&rpc.PushFeedReq{
		Mid: 101, Oid: 5001, Otype: rpc.OType_OTYPE_UNSPECIFIED, Action: rpc.Action_ACTION_UNSPECIFIED,
		Source: "video", Operator: "system",
	})
	wantNoErr(t, "未指定 otype/action 的推送", err)
	feedID := st.outbox.lastID()
	wantEQ(t, "未指定 otype", "入库 otype", st.outbox.rows[feedID].Otype, int32(0))
	wantEQ(t, "未指定 action", "入库 action", st.outbox.rows[feedID].Action, int32(0))
}

func TestPullFeedGuards(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.PullFeedReq
		want error
	}{
		{"mid 为 0", &rpc.PullFeedReq{Mid: 0}, model.ErrInvalidMid},
		{"mid 为负", &rpc.PullFeedReq{Mid: -3}, model.ErrInvalidMid},
		{"cursor 为负", &rpc.PullFeedReq{Mid: 201, Cursor: -1}, model.ErrInvalidCursor},
		// 守卫顺序：mid 先于 cursor（pullfeedlogic.go:30-35）。
		{"mid 与 cursor 同时非法先报 mid", &rpc.PullFeedReq{Mid: 0, Cursor: -1}, model.ErrInvalidMid},
	}
	for _, tc := range cases {
		st := newStore()
		_, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(tc.in)
		wantErrIs(t, tc.name, err, tc.want)
		wantNoCall(t, tc.name, st, 0)
	}
}

func TestListUserFeedGuards(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ListUserFeedReq
		want error
	}{
		{"vmid 为 0", &rpc.ListUserFeedReq{Vmid: 0}, model.ErrInvalidVmid},
		{"vmid 为负", &rpc.ListUserFeedReq{Vmid: -1}, model.ErrInvalidVmid},
		{"cursor 为负", &rpc.ListUserFeedReq{Vmid: 101, Cursor: -5}, model.ErrInvalidCursor},
		// 守卫顺序：vmid 先于 cursor（listuserfeedlogic.go:30-35）。
		{"vmid 与 cursor 同时非法先报 vmid", &rpc.ListUserFeedReq{Vmid: 0, Cursor: -1}, model.ErrInvalidVmid},
	}
	for _, tc := range cases {
		st := newStore()
		_, err := NewListUserFeedLogic(context.Background(), st.svcCtx()).ListUserFeed(tc.in)
		wantErrIs(t, tc.name, err, tc.want)
		wantNoCall(t, tc.name, st, 0)
	}
}

// TestListUserFeedIgnoresViewerMid 钉住缺陷 D6：
// proto 声明 mid 用于「可见性判断，0 表示未登录」（rpc/feed.proto:90），
// 但 logic 与 repository 都没有读这个字段——未登录与拉黑视角返回完全相同的内容。
func TestListUserFeedIgnoresViewerMid(t *testing.T) {
	st := newStore()
	row := st.outbox.seed(101, 5001, 1_000)
	st.cache.seedZSet(outboxKey(101), row.ID, row.Ctime)

	logic := NewListUserFeedLogic(context.Background(), st.svcCtx())
	from := st.log.snapshot()
	anon, err := logic.ListUserFeed(&rpc.ListUserFeedReq{Vmid: 101, Mid: 0, Ps: 20})
	wantNoErr(t, "未登录视角", err)
	other, err := logic.ListUserFeed(&rpc.ListUserFeedReq{Vmid: 101, Mid: 999, Ps: 20})
	wantNoErr(t, "他人视角", err)

	wantOps(t, "两次请求口径必须完全一致（mid 被忽略）", st.log.opsFrom(from), []string{
		"cache.RangeOutbox:mid=101:cursor=0:limit=21", "feed_outbox.FindMany:" + strconv.FormatInt(row.ID, 10),
		"cache.RangeOutbox:mid=101:cursor=0:limit=21", "feed_outbox.FindMany:" + strconv.FormatInt(row.ID, 10),
	})
	wantInt64sEQ(t, "两次请求口径必须完全一致（mid 被忽略）", "可见集合", feedIDs(anon.Items), feedIDs(other.Items))
	wantInt64sEQ(t, "未登录视角", "items", feedIDs(anon.Items), []int64{row.ID})
}

func TestPinFeedGuards(t *testing.T) {
	st := newStore()
	from := st.log.snapshot()
	_, err := NewPinFeedLogic(context.Background(), st.svcCtx()).PinFeed(&rpc.PinFeedReq{Mid: 0, FeedId: 7})
	wantErrIs(t, "PinFeed mid 非法", err, model.ErrInvalidMid)
	wantNoCall(t, "PinFeed mid 非法", st, from)

	from = st.log.snapshot()
	_, err = NewPinFeedLogic(context.Background(), st.svcCtx()).PinFeed(&rpc.PinFeedReq{Mid: 101, FeedId: 0})
	wantErrIs(t, "PinFeed feed_id 非法", err, model.ErrInvalidFeedID)
	wantNoCall(t, "PinFeed feed_id 非法", st, from)

	// 两个字段同时非法时先报 mid（pinfeedlogic.go:30-35）。
	from = st.log.snapshot()
	_, err = NewPinFeedLogic(context.Background(), st.svcCtx()).PinFeed(&rpc.PinFeedReq{Mid: -1, FeedId: -1})
	wantErrIs(t, "PinFeed 双非法先报 mid", err, model.ErrInvalidMid)
	wantNoCall(t, "PinFeed 双非法先报 mid", st, from)
}

func TestUnpinFeedGuards(t *testing.T) {
	st := newStore()
	from := st.log.snapshot()
	_, err := NewUnpinFeedLogic(context.Background(), st.svcCtx()).UnpinFeed(&rpc.UnpinFeedReq{Mid: 0, FeedId: 7})
	wantErrIs(t, "UnpinFeed mid 非法", err, model.ErrInvalidMid)
	wantNoCall(t, "UnpinFeed mid 非法", st, from)

	from = st.log.snapshot()
	_, err = NewUnpinFeedLogic(context.Background(), st.svcCtx()).UnpinFeed(&rpc.UnpinFeedReq{Mid: 101, FeedId: -2})
	wantErrIs(t, "UnpinFeed feed_id 非法", err, model.ErrInvalidFeedID)
	wantNoCall(t, "UnpinFeed feed_id 非法", st, from)
}

func TestDeleteFeedGuards(t *testing.T) {
	st := newStore()
	from := st.log.snapshot()
	_, err := NewDeleteFeedLogic(context.Background(), st.svcCtx()).DeleteFeed(&rpc.DeleteFeedReq{Mid: 0, FeedId: 7})
	wantErrIs(t, "mid 非法", err, model.ErrInvalidMid)
	wantNoCall(t, "mid 非法", st, from)

	from = st.log.snapshot()
	_, err = NewDeleteFeedLogic(context.Background(), st.svcCtx()).DeleteFeed(&rpc.DeleteFeedReq{Mid: 101, FeedId: 0})
	wantErrIs(t, "feed_id 非法", err, model.ErrInvalidFeedID)
	wantNoCall(t, "feed_id 非法", st, from)

	// 双非法先报 mid（deletefeedlogic.go:31-36）。
	from = st.log.snapshot()
	_, err = NewDeleteFeedLogic(context.Background(), st.svcCtx()).DeleteFeed(&rpc.DeleteFeedReq{})
	wantErrIs(t, "全零请求", err, model.ErrInvalidMid)
	wantNoCall(t, "全零请求", st, from)
}

func TestUnreadGuards(t *testing.T) {
	st := newStore()
	from := st.log.snapshot()
	_, err := NewGetUnreadCountLogic(context.Background(), st.svcCtx()).GetUnreadCount(&rpc.MidReq{Mid: 0})
	wantErrIs(t, "GetUnreadCount mid 非法", err, model.ErrInvalidMid)
	wantNoCall(t, "GetUnreadCount mid 非法", st, from)

	from = st.log.snapshot()
	_, err = NewGetUnreadCountLogic(context.Background(), st.svcCtx()).GetUnreadCount(&rpc.MidReq{Mid: -9})
	wantErrIs(t, "GetUnreadCount mid 为负", err, model.ErrInvalidMid)
	wantNoCall(t, "GetUnreadCount mid 为负", st, from)

	from = st.log.snapshot()
	_, err = NewClearUnreadLogic(context.Background(), st.svcCtx()).ClearUnread(&rpc.MidReq{Mid: 0})
	wantErrIs(t, "ClearUnread mid 非法", err, model.ErrInvalidMid)
	wantNoCall(t, "ClearUnread mid 非法", st, from)

	from = st.log.snapshot()
	_, err = NewClearUnreadLogic(context.Background(), st.svcCtx()).ClearUnread(&rpc.MidReq{Mid: -1})
	wantErrIs(t, "ClearUnread mid 为负", err, model.ErrInvalidMid)
	wantNoCall(t, "ClearUnread mid 为负", st, from)
}

// TestPsClampIsSilent 钉住 ps 的越界处理口径（缺陷 D5）：
// ps>50 与 ps<=0 都被**静默折成 20**，既不报错也不折到上限 50；
// model.ErrPsTooLarge（model/errors.go:12）全仓无人返回，是死哨兵。
// 越界不会变成「一次大扫描」：ZSet 探针 limit 恒为归一后的 ps+1。
func TestPsClampIsSilent(t *testing.T) {
	cases := []struct {
		name string
		ps   int32
		want int32
	}{
		{"ps 为 0 折成默认", 0, 20},
		{"ps 为负折成默认", -1, 20},
		{"ps 超上限折成默认而不是上限", 51, 20},
		{"ps 远超上限同样折成默认", 1_000_000, 20},
		{"ps 等于上限原样放行", 50, 50},
		{"ps 为 1 原样放行", 1, 1},
	}
	for _, tc := range cases {
		st := newStore()
		st.cache.seedZSet(inboxKey(201), 7, 1_000)
		from := st.log.snapshot()
		_, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(&rpc.PullFeedReq{Mid: 201, Ps: tc.ps})
		wantNoErr(t, tc.name, err)
		wantEQ(t, tc.name, "normalizePs", normalizePs(tc.ps), tc.want)
		wantOps(t, tc.name, st.log.opsFrom(from), []string{
			"cache.RangeInbox:mid=201:cursor=0:limit=" + strconv.FormatInt(int64(tc.want+1), 10),
			"feed_outbox.FindMany:7",
		})
	}
}

// TestPullFeedHugeCursorCannotWidenScan 锁「越界游标不能变成全表扫」：
// cursor 大于库里所有 ctime 时，读到的仍然是同一份有序集合的前 limit 条，
// 且不会因为 cursor 非法（负数）而绕过滤条件——负数在守卫处就被拒了（见 TestPullFeedGuards）。
func TestPullFeedHugeCursorCannotWidenScan(t *testing.T) {
	st := newStore()
	r1 := st.outbox.seed(101, 5001, 1_000)
	r2 := st.outbox.seed(101, 5002, 900)
	r3 := st.outbox.seed(101, 5003, 800)
	for _, row := range []*model.FeedOutbox{r1, r2, r3} {
		st.cache.seedZSet(inboxKey(201), row.ID, row.Ctime)
	}
	from := st.log.snapshot()
	reply, err := NewPullFeedLogic(context.Background(), st.svcCtx()).PullFeed(
		&rpc.PullFeedReq{Mid: 201, Cursor: 9_223_372_036_854_775_806, Ps: 2})
	wantNoErr(t, "游标大于全部 ctime", err)
	// 探针那条只参与 has_more 判定，不回表（repository.go:434-439 先截断再 FindMany）。
	wantOps(t, "游标越界只多取一条探针", st.log.opsFrom(from), []string{
		"cache.RangeInbox:mid=201:cursor=9223372036854775806:limit=3",
		"feed_outbox.FindMany:" + joinInt64([]int64{r1.ID, r2.ID}),
	})
	// ps=2、库 3 条 → 返回 2 条 + has_more，第三页靠 next_cursor 续拉。
	wantInt64sEQ(t, "游标大于全部 ctime", "首页条目", feedIDs(reply.Items), []int64{r1.ID, r2.ID})
	wantEQ(t, "首页", "has_more", reply.HasMore, true)
}
