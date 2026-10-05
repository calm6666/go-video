package logic

// 收件箱游标分页（ListMessages）。
//
// 要紧的结论：
//   - 列表本身只读收件明细，未读总数是附加信息：Redis/快照异常时不得拖垮收件箱读取
//     （记日志、按 0 返回），这是它和其它读接口的关键区别；
//   - 带分类过滤时 unread_total 返回的是该分类的未读数，不是全量；
//   - ps 走服务端默认值与上限，游标不透明。

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"

	"go-video/services/inbox/internal/repository"
	"go-video/services/inbox/model"
	"go-video/services/inbox/rpc"
)

const (
	msgIDOldest = int64(7101)
	msgIDMid    = int64(7102)
	msgIDNewest = int64(7103)
)

// seedList 布三行属于 Alice 的消息（ctime 递增，分类分别 1/2/1），
// 并给出与明细一致的快照。返回按 ctime 倒序的 msg_id 顺序。
func seedList(t *testing.T, st *store) {
	t.Helper()
	m1 := seedMessage(t, st, &model.InboxMessage{
		MsgID: msgIDOldest, Category: model.CategorySystem, MsgType: model.MsgTypeText,
		Title: "第一次审核通过", Content: "稿件 5001 已通过", SenderMid: 0,
		BizType: "submission", BizID: "5001", Extra: `{"k":1}`, Ctime: 1700000111,
	})
	m2 := seedMessage(t, st, &model.InboxMessage{
		MsgID: msgIDMid, Category: model.CategoryEngagement, MsgType: model.MsgTypeLink,
		Title: "有人赞了你", Content: "点赞 3 次", SenderMid: 60001,
		BizType: "like", BizID: "9001", Extra: `{"k":2}`, Ctime: 1700000222,
	})
	m3 := seedMessage(t, st, &model.InboxMessage{
		MsgID: msgIDNewest, Category: model.CategorySystem, MsgType: model.MsgTypeRich,
		Title: "账号安全提醒", Content: "在新设备登录", SenderMid: 0,
		BizType: "security", BizID: "s-1", Extra: `{"k":3}`, Ctime: 1700000333,
	})
	// 三行共用一个自增 id 序：布景显式给 id，保证同秒并列时排序可断言。
	for i, m := range []*model.InboxMessage{m1, m2, m3} {
		seedRow(t, st, &model.InboxUserMessage{
			ID: 400 + int64(i), Mid: midAlice, MsgID: m.MsgID, Category: m.Category,
			ReadState: model.ReadStateUnread, DelState: model.DelStateNormal, Ctime: m.Ctime,
		})
	}
	// Bob 也收到同一条 newest：用于断言列表不会串到别人的收件箱。
	seedRow(t, st, &model.InboxUserMessage{
		ID: 499, Mid: midBob, MsgID: msgIDNewest, Category: model.CategorySystem,
		ReadState: model.ReadStateUnread, DelState: model.DelStateNormal, Ctime: 1700000333,
	})
	seedStat(t, st, midAlice, map[int32]int64{
		model.CategorySystem: 2, model.CategoryEngagement: 1, model.CategoryContent: 0, model.CategoryLive: 0,
	}, 1700000999)
}

func TestListMessagesGuards(t *testing.T) {
	cases := []struct {
		name string
		req  *rpc.ListMessagesReq
		want error
	}{
		{"mid 为 0", &rpc.ListMessagesReq{Mid: 0}, model.ErrInvalidMid},
		{"mid 为负", &rpc.ListMessagesReq{Mid: -9}, model.ErrInvalidMid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seedList(t, e.st)
			before := e.st.log.snapshot()

			reply, err := NewListMessagesLogic(context.Background(), e.svcCtx).ListMessages(tc.req)

			if reply != nil {
				t.Fatalf("%s：拒绝后仍返回响应体 %+v", tc.name, reply)
			}
			wantErrIs(t, tc.name, err, tc.want)
			// mid 守卫在 logic 里，必须早于任何一次读。
			wantNoCall(t, tc.name, e.st, before)
		})
	}
}

// 分类非法、ps 超上限、游标乱码这三条守卫在 repository 里，
// 但同样必须早于对明细表的查询。
func TestListMessagesRejectsBadParamsBeforeQuery(t *testing.T) {
	cases := []struct {
		name string
		req  *rpc.ListMessagesReq
		want error
	}{
		{"分类越界", &rpc.ListMessagesReq{Mid: midAlice, Category: rpc.Category(9)}, model.ErrInvalidCategory},
		{"ps 超上限", &rpc.ListMessagesReq{Mid: midAlice, Ps: 51}, model.ErrPsTooLarge},
		{"游标不是 base64", &rpc.ListMessagesReq{Mid: midAlice, Cursor: "%%%not-base64"}, model.ErrInvalidCursor},
		{"游标缺冒号", &rpc.ListMessagesReq{Mid: midAlice, Cursor: rawURL("1700000333")}, model.ErrInvalidCursor},
		{"游标时间是负数", &rpc.ListMessagesReq{Mid: midAlice, Cursor: rawURL("-1:5")}, model.ErrInvalidCursor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seedList(t, e.st)
			before := e.st.log.snapshot()

			reply, err := NewListMessagesLogic(context.Background(), e.svcCtx).ListMessages(tc.req)

			if reply != nil {
				t.Fatalf("%s：拒绝后仍返回响应体 %+v", tc.name, reply)
			}
			wantErrIs(t, tc.name, err, tc.want)
			wantNoCall(t, tc.name, e.st, before)
		})
	}
}

// rawURL 造一个「base64url 可解但内容非法」的游标，用于守卫用例。
func rawURL(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func TestListMessagesProjectsEveryField(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedList(t, st)
	before := st.log.snapshot()

	reply, err := NewListMessagesLogic(context.Background(), e.svcCtx).ListMessages(
		&rpc.ListMessagesReq{Mid: midAlice, Ps: 2})
	wantNoErr(t, "拉取收件箱", err)

	wantEQ(t, "收件箱首页", "len(List)", len(reply.List), 2)
	wantEQ(t, "收件箱首页", "HasMore", reply.HasMore, true)
	wantEQ(t, "收件箱首页", "UnreadTotal", reply.UnreadTotal, int64(3))
	wantEQ(t, "收件箱首页", "NextCursor", reply.NextCursor,
		repository.EncodeCursor(repository.Cursor{Time: 1700000222, ID: 401}))

	first := reply.List[0]
	wantEQ(t, "第 1 条", "MsgId", first.MsgId, msgIDNewest)
	wantEQ(t, "第 1 条", "Category", first.Category, rpc.Category_CATEGORY_SYSTEM)
	wantEQ(t, "第 1 条", "MsgType", first.MsgType, rpc.MsgType_MSG_TYPE_RICH)
	wantEQ(t, "第 1 条", "Title", first.Title, "账号安全提醒")
	wantEQ(t, "第 1 条", "Content", first.Content, "在新设备登录")
	wantEQ(t, "第 1 条", "SenderMid", first.SenderMid, int64(0))
	wantEQ(t, "第 1 条", "BizType", first.BizType, "security")
	wantEQ(t, "第 1 条", "BizId", first.BizId, "s-1")
	wantEQ(t, "第 1 条", "Extra", first.Extra, `{"k":3}`)
	wantEQ(t, "第 1 条", "Ctime", first.Ctime, int64(1700000333))
	wantEQ(t, "第 1 条", "ReadState", first.ReadState, rpc.ReadState_READ_STATE_UNREAD)

	second := reply.List[1]
	wantEQ(t, "第 2 条", "MsgId", second.MsgId, msgIDMid)
	wantEQ(t, "第 2 条", "Category", second.Category, rpc.Category_CATEGORY_ENGAGEMENT)
	wantEQ(t, "第 2 条", "MsgType", second.MsgType, rpc.MsgType_MSG_TYPE_LINK)
	wantEQ(t, "第 2 条", "SenderMid", second.SenderMid, int64(60001))
	wantEQ(t, "第 2 条", "BizId", second.BizId, "9001")
	wantEQ(t, "第 2 条", "Ctime", second.Ctime, int64(1700000222))

	wantOps(t, "首页的调用链", st.log.opsFrom(before), []string{
		fmt.Sprintf("user.List:%d/0/false/0/3", midAlice),
		fmt.Sprintf("cache.Get:%d", midAlice),
		fmt.Sprintf("stat.ListByMid:%d", midAlice),
		fmt.Sprintf("cache.Set:%d/3", midAlice),
	})
	// 回填：miss 之后 Redis 里必须有完整四分类快照。
	wantMapEQ(t, "回填写入的快照", "cache", st.cache.cached(midAlice),
		map[int32]int64{1: 2, 2: 1, 3: 0, 4: 0})
}

// 第二页用服务端给的 next_cursor 继续翻：只剩最旧那条，且不再有下一页。
func TestListMessagesSecondPage(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedList(t, st)

	first, err := NewListMessagesLogic(context.Background(), e.svcCtx).ListMessages(
		&rpc.ListMessagesReq{Mid: midAlice, Ps: 2})
	wantNoErr(t, "第一页", err)

	before := st.log.snapshot()
	second, err := NewListMessagesLogic(context.Background(), e.svcCtx).ListMessages(
		&rpc.ListMessagesReq{Mid: midAlice, Ps: 2, Cursor: first.NextCursor})
	wantNoErr(t, "第二页", err)

	wantEQ(t, "第二页", "len(List)", len(second.List), 1)
	wantEQ(t, "第二页", "MsgId", second.List[0].MsgId, msgIDOldest)
	wantEQ(t, "第二页", "HasMore", second.HasMore, false)
	wantEQ(t, "第二页", "NextCursor 为空表示到底", second.NextCursor, "")
	// 两页合起来不重不漏。
	wantEQ(t, "翻页不重不漏", "两页 msg_id",
		fmt.Sprint([]int64{first.List[0].MsgId, first.List[1].MsgId, second.List[0].MsgId}),
		fmt.Sprint([]int64{msgIDNewest, msgIDMid, msgIDOldest}))
	wantOps(t, "第二页只读明细与计数", st.log.opsFrom(before), []string{
		fmt.Sprintf("user.List:%d/0/false/1700000222/3", midAlice),
		fmt.Sprintf("cache.Get:%d", midAlice),
	})
}

// 分类过滤：列表只剩该分类，unread_total 也跟着换成该分类的未读数。
func TestListMessagesFiltersByCategory(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedList(t, st)

	reply, err := NewListMessagesLogic(context.Background(), e.svcCtx).ListMessages(
		&rpc.ListMessagesReq{Mid: midAlice, Category: rpc.Category_CATEGORY_ENGAGEMENT})
	wantNoErr(t, "按互动分类过滤", err)

	wantEQ(t, "互动分类", "len(List)", len(reply.List), 1)
	wantEQ(t, "互动分类", "MsgId", reply.List[0].MsgId, msgIDMid)
	wantEQ(t, "互动分类", "UnreadTotal 是该分类的未读数", reply.UnreadTotal, int64(1))
	wantEQ(t, "互动分类", "HasMore", reply.HasMore, false)
}

// unread_only=true 只返回未读；把最新一条标记已读后它必须消失。
func TestListMessagesUnreadOnly(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedList(t, st)
	row, ok := st.rowRef(midAlice, msgIDNewest)
	if !ok {
		t.Fatal("布景未生效")
	}
	row.ReadState = model.ReadStateRead

	reply, err := NewListMessagesLogic(context.Background(), e.svcCtx).ListMessages(
		&rpc.ListMessagesReq{Mid: midAlice, UnreadOnly: true})
	wantNoErr(t, "只看未读", err)

	wantEQ(t, "只看未读", "len(List)", len(reply.List), 2)
	wantEQ(t, "只看未读", "第 1 条", reply.List[0].MsgId, msgIDMid)
	wantEQ(t, "只看未读", "第 2 条", reply.List[1].MsgId, msgIDOldest)
	for _, m := range reply.List {
		wantEQ(t, "只看未读", "ReadState", m.ReadState, rpc.ReadState_READ_STATE_UNREAD)
	}
}

// ps 未指定时走服务端默认值：布 3 条时默认页 20 会一次给完，
// 因此换成小默认页配置来验证「默认值真的生效」而不是「恰好没分页」。
func TestListMessagesUsesConfiguredDefaultPageSize(t *testing.T) {
	conf := testConf()
	conf.PageSize = 1
	e := newEnvConf(t, conf)
	st := e.st
	seedList(t, st)

	reply, err := NewListMessagesLogic(context.Background(), e.svcCtx).ListMessages(
		&rpc.ListMessagesReq{Mid: midAlice})
	wantNoErr(t, "默认页大小", err)
	wantEQ(t, "默认页大小", "len(List)", len(reply.List), 1)
	wantEQ(t, "默认页大小", "HasMore", reply.HasMore, true)
	wantEQ(t, "默认页大小", "多取一条判断下一页的 limit",
		st.log.countPrefix(fmt.Sprintf("user.List:%d/0/false/0/2", midAlice)), 1)
}

// 缓存命中时不回源：列表的 unread_total 直接取 Redis 副本。
func TestListMessagesUnreadComesFromCacheWhenHot(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedList(t, st)
	st.cache.warm(midAlice, map[int32]int64{1: 2, 2: 1, 3: 0, 4: 0})
	before := st.log.snapshot()

	reply, err := NewListMessagesLogic(context.Background(), e.svcCtx).ListMessages(
		&rpc.ListMessagesReq{Mid: midAlice, Category: rpc.Category_CATEGORY_SYSTEM})
	wantNoErr(t, "缓存热时拉列表", err)
	wantEQ(t, "缓存热", "UnreadTotal", reply.UnreadTotal, int64(2))
	wantOps(t, "缓存热时的调用链", st.log.opsFrom(before), []string{
		fmt.Sprintf("user.List:%d/1/false/0/21", midAlice),
		fmt.Sprintf("cache.Get:%d", midAlice),
	})
	wantAbsent(t, "缓存命中不得回源快照表", st.log, "stat.ListByMid")
	wantAbsent(t, "缓存命中不得重算", st.log, "user.CountUnreadByCategory")
}

// 快照缺失（例如快照表被误清）时自动从明细重算并回写：列表仍能返回正确计数。
func TestListMessagesFallsBackToRecomputeWhenStatMissing(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedList(t, st)
	delete(st.tb.stat, midAlice)
	before := st.log.snapshot()

	reply, err := NewListMessagesLogic(context.Background(), e.svcCtx).ListMessages(
		&rpc.ListMessagesReq{Mid: midAlice})
	wantNoErr(t, "快照缺失时拉列表", err)
	wantEQ(t, "快照缺失", "UnreadTotal", reply.UnreadTotal, int64(3))
	wantOps(t, "回源重算的调用链", st.log.opsFrom(before), []string{
		fmt.Sprintf("user.List:%d/0/false/0/21", midAlice),
		fmt.Sprintf("cache.Get:%d", midAlice),
		fmt.Sprintf("stat.ListByMid:%d", midAlice),
		fmt.Sprintf("user.CountUnreadByCategory:%d", midAlice),
		fmt.Sprintf("stat.ReplaceByMid:%d/conn", midAlice),
		fmt.Sprintf("cache.Set:%d/3", midAlice),
	})
	wantMapEQ(t, "回源重算写回的快照", "stat", st.statOf(midAlice),
		map[int32]int64{1: 2, 2: 1, 3: 0, 4: 0})
}

// Redis 不可用（表现为 miss）不影响列表：走快照，不报错。
func TestListMessagesSurvivesCacheUnavailable(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedList(t, st)
	st.cache.unavailable = true
	before := st.log.snapshot()

	reply, err := NewListMessagesLogic(context.Background(), e.svcCtx).ListMessages(
		&rpc.ListMessagesReq{Mid: midAlice})
	wantNoErr(t, "缓存不可用时拉列表", err)
	wantEQ(t, "缓存不可用", "UnreadTotal", reply.UnreadTotal, int64(3))
	wantEQ(t, "缓存不可用", "len(List)", len(reply.List), 3)
	wantOps(t, "缓存不可用时的调用链", st.log.opsFrom(before), []string{
		fmt.Sprintf("user.List:%d/0/false/0/21", midAlice),
		fmt.Sprintf("cache.Get:%d", midAlice),
		fmt.Sprintf("stat.ListByMid:%d", midAlice),
		fmt.Sprintf("cache.Set:%d/3", midAlice),
	})
}

// 未读计数是列表的附加信息：读计数失败只降级为 0，不得让收件箱读不出来。
func TestListMessagesDegradesWhenUnreadReadFails(t *testing.T) {
	e := newEnv(t)
	st := e.st
	errInjected := errors.New("inbox-test-stat-unavailable")
	seedList(t, st)
	st.stats.failWith("ListByMid", errInjected)
	before := st.log.snapshot()

	reply, err := NewListMessagesLogic(context.Background(), e.svcCtx).ListMessages(
		&rpc.ListMessagesReq{Mid: midAlice})
	wantNoErr(t, "计数故障时的列表", err)
	wantEQ(t, "计数故障", "UnreadTotal 降级为 0", reply.UnreadTotal, int64(0))
	wantEQ(t, "计数故障", "len(List) 不受影响", len(reply.List), 3)
	wantEQ(t, "计数故障", "第 1 条仍是最新消息", reply.List[0].MsgId, msgIDNewest)
	wantOps(t, "计数故障时的调用链", st.log.opsFrom(before), []string{
		fmt.Sprintf("user.List:%d/0/false/0/21", midAlice),
		fmt.Sprintf("cache.Get:%d", midAlice),
		fmt.Sprintf("stat.ListByMid:%d", midAlice),
	})
	wantAbsent(t, "计数读失败不得写缓存", st.log, "cache.Set")
}

// 明细读失败是真错误：必须原样上抛，不能返回空列表冒充「没有消息」。
func TestListMessagesPropagatesListError(t *testing.T) {
	e := newEnv(t)
	st := e.st
	errInjected := errors.New("inbox-test-user-message-unavailable")
	seedList(t, st)
	st.users.failWith("List", errInjected)
	before := st.log.snapshot()

	reply, err := NewListMessagesLogic(context.Background(), e.svcCtx).ListMessages(
		&rpc.ListMessagesReq{Mid: midAlice})

	if reply != nil {
		t.Fatalf("明细读失败仍返回响应体 %+v", reply)
	}
	wantErrIs(t, "明细读失败", err, errInjected)
	wantOps(t, "失败即止", st.log.opsFrom(before), []string{
		fmt.Sprintf("user.List:%d/0/false/0/21", midAlice),
	})
	wantAbsent(t, "列表失败不得继续读计数", st.log, "cache.Get")
}
