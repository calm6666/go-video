// 本文件钉住 GetUnreadSummary 的计数口径。它是最容易「测了等于没测」的一类读接口：
// 只断言 `UnreadTotal >= 0` 会永远通过，而真正的事故恰恰是数字口径错了却仍然合法。
// 因此这里每条断言都落到具体数字，并且和「直接从成员投影行重算出来的数」对齐：
//
//	1. 只聚合传入 mid 自己的成员行（本方法不存在「查别人的角标」这条入口）；
//	2. hide_state 口径恒为「不含隐藏会话」（getunreadsummarylogic.go:40 声明与
//	   ListConversations 默认过滤一致）——不一致时端上角标和列表会对不上；
//	3. 与 MarkRead / 新消息落库严格一致：读到即 0，新到即 +1，一条不多一条不少；
//	4. 缓存只是加速层：未配置和「配了但挂了」都必须回源 MySQL 真值，
//	   绝不能把「拿不到缓存」当成「没有未读」（那是把角标清零的假成功）；
//	5. 入参守卫发生在触库之前，依赖错误原样上抛。
//
// 诚实边界：缓存「命中并回放 cached 载荷」这条分支至今无法在进程内测——
// svc.ServiceContext.Cache 是具体类型 *redis.Redis，包里没有进程内 redis server，
// 引入 miniredis 属于新增第三方依赖（本轮禁止）。所以这里能证的是「缓存不可用不改变结果」，
// 不是「缓存命中时结果也正确」。

package logic

import (
	"context"
	"strings"
	"testing"
	"time"

	"go-video/services/private-message/model"
	"go-video/services/private-message/rpc"
)

// carol 是第三个真实用户 mid：角标要按 mid 切分，第三方在场才测得出「数串到了谁身上」。
const carol = int64(303)

func callUnreadSummary(t *testing.T, e *env, ctx context.Context, mid int64, force bool) (*rpc.GetUnreadSummaryReply, error) {
	t.Helper()
	return NewGetUnreadSummaryLogic(ctx, e.svc).GetUnreadSummary(&rpc.GetUnreadSummaryReq{Mid: mid, Force: force})
}

// unreadSummary 取角标并当场要求成功：本文件里「返回错误」永远不是可接受的结果。
func unreadSummary(t *testing.T, e *env, mid int64, label string) *rpc.GetUnreadSummaryReply {
	t.Helper()
	got, err := callUnreadSummary(t, e, bg(), mid, false)
	return wantOK(t, got, err, label)
}

// unreadFromRows 独立重算「某个 mid 的未读投影」，作为被测返回值的对照事实源：
// 与 model.SumUnread 的 WHERE 同口径（unread_count > 0 且默认不含隐藏会话）。
// 测试自己算一遍，而不是把被测代码的返回值当成期望值。
func unreadFromRows(t *testing.T, e *env, mid int64, includeHidden bool) (int64, int64) {
	t.Helper()
	var total, convs int64
	for _, m := range e.st.members {
		if m.Mid != mid || m.UnreadCount <= 0 {
			continue
		}
		if !includeHidden && m.HideState != model.HideStateNormal {
			continue
		}
		total += m.UnreadCount
		convs++
	}
	return total, convs
}

// requireMatchesRows 把「接口返回」与「投影重算」两处一起钉住：
// 只钉其中之一会漏掉两类相反的错——两者同时错成同一个数（口径整体跑偏），
// 或接口对但种子本身就与生产 SQL 不同构。
func requireMatchesRows(t *testing.T, e *env, mid int64, got *rpc.GetUnreadSummaryReply, label string) {
	t.Helper()
	total, convs := unreadFromRows(t, e, mid, false)
	if got.UnreadTotal != total || got.UnreadConversations != convs {
		t.Fatalf("%s：角标 (%d,%d) 与成员投影重算值 (%d,%d) 不一致", label,
			got.UnreadTotal, got.UnreadConversations, total, convs)
	}
}

// TestGetUnreadSummaryCountsOnlyCallersOwnProjection 钉住「按传入 mid 聚合」这条数据归属边界：
// 同一条会话里发送方与接收方的角标是两个数，无关第三方恒为 0。
func TestGetUnreadSummaryCountsOnlyCallersOwnProjection(t *testing.T) {
	e := newEnv(t)
	p := e.seedPair(t, alice, bob)
	// 第二条会话有成员行但没有消息：它必须一个未读都不贡献，
	// 否则「有会话就算未读会话」这种错口径也能通过。
	e.seedPair(t, alice, carol)
	// bob 收 alice 两条 → bob 未读 2；alice 自己发的这两条对 alice 不算未读（ApplyIncoming 口径）。
	e.seedMessage(t, p, alice, bob, 1, "第一条", model.MsgStateNormal)
	e.seedMessage(t, p, alice, bob, 2, "第二条", model.MsgStateNormal)

	before := e.probe()
	mark := e.markCalls()

	gotBob := unreadSummary(t, e, bob, "bob 取角标")
	requireMatchesRows(t, e, bob, gotBob, "bob 的角标")
	if gotBob.UnreadTotal != 2 || gotBob.UnreadConversations != 1 {
		t.Fatalf("bob 应有 2 条未读/1 个会话，实得 %d/%d", gotBob.UnreadTotal, gotBob.UnreadConversations)
	}

	// 数据来源唯一：整个读路径只有 Members.SumUnread 一次，没碰消息表也没碰会话表。
	// 「顺手 JOIN pm_message 数一遍」在这里会立刻变成 Messages.* 非零。
	if n := mark.callsOf("Members.SumUnread"); n != 1 {
		t.Fatalf("一次角标查询应恰好回源 Members.SumUnread 一次，实得 %d 次", n)
	}
	requireOnlyHandleRead(t, mark, "Members", "未读汇总")

	gotAlice := unreadSummary(t, e, alice, "alice 取角标")
	if gotAlice.UnreadTotal != 0 || gotAlice.UnreadConversations != 0 {
		t.Fatalf("发送方自己的两条消息不该算进自己的角标，实得 %d/%d", gotAlice.UnreadTotal, gotAlice.UnreadConversations)
	}
	// carol 有成员行（与 alice 的空会话）但没有未读：必须是 0，不能继承 alice/bob 的数。
	gotCarol := unreadSummary(t, e, carol, "carol 取角标")
	if gotCarol.UnreadTotal != 0 || gotCarol.UnreadConversations != 0 {
		t.Fatalf("carol 的空会话被算成未读：%d/%d", gotCarol.UnreadTotal, gotCarol.UnreadConversations)
	}
	// mallory 与这两条会话都无关：库里有未读行时他的角标也必须为 0。
	gotMallory := unreadSummary(t, e, mallory, "mallory 取角标")
	if gotMallory.UnreadTotal != 0 || gotMallory.UnreadConversations != 0 {
		t.Fatalf("无关 mid 的角标被别人的成员行污染：%d/%d", gotMallory.UnreadTotal, gotMallory.UnreadConversations)
	}

	now := time.Now().Unix()
	if gotBob.ComputedAt <= 0 || gotBob.ComputedAt > now+1 {
		t.Fatalf("computed_at %d 不在 (0, 当前时间] 区间", gotBob.ComputedAt)
	}
	if gotMallory.ComputedAt != gotBob.ComputedAt && gotMallory.ComputedAt < gotBob.ComputedAt {
		t.Fatalf("computed_at 出现回退：%d -> %d", gotBob.ComputedAt, gotMallory.ComputedAt)
	}
	e.requireSameRows(t, before.rows, "未读汇总（只读）")
}

// TestGetUnreadSummarySelfReportedMidReadsOthersBadge 钉住「mid 完全由调用方自报」这条事实。
//
// 本方法签名里没有任何可信身份：mid 就是 req.Mid，logic 只做 checkMid（>0）。
// 因此「mallory 用 bob 的 mid 调用」必然拿到 bob 的角标——这是当前生产的真实行为，
// 不是测试想要它成立。
//
// 缺陷：services/private-message/internal/logic/getunreadsummarylogic.go:44 —— mid 取自请求体、
// 无任何可信来源校验 —— 网关侧 gateway/app/api/app.api:3118 起这条路由未挂 jwt、
// mid 是 form 参数（gateway/app/internal/logic/listpmconversationslogic.go:39 直接转发 req.Mid），
// 所以任意登录（甚至未登录，取决于网关最终配置）用户都能读别人的未读角标，
// 并能用它当「别人是否收到了新消息」的探针（未读从 N 变 N+1 即泄露对端行为）。
// 修法方向：mid 一律由网关注入会话凭证、本服务只信 ctx（与 gateway/admin 的 operator 口径一致）。
//
// ⚠ 改生产代码前不要动这条断言的期望。
func TestGetUnreadSummarySelfReportedMidReadsOthersBadge(t *testing.T) {
	e := newEnv(t)
	p := e.seedPair(t, alice, bob)
	e.seedMessage(t, p, alice, bob, 1, "给 bob 的内容", model.MsgStateNormal)
	e.seedMessage(t, p, alice, bob, 2, "还是给 bob 的", model.MsgStateNormal)

	bobTruth := unreadSummary(t, e, bob, "bob 自己的角标")
	// 同样的返回值，由一个与 bob 毫无关系的 mid 通过自报 bob 拿到。
	// 期望值刻意写成「与 bob 真值相等」：一旦本服务改成只信可信身份，这里必然红。
	stolen, err := callUnreadSummary(t, e, bg(), bob, false)
	wantOK(t, stolen, err, "mallory 自报 mid=bob")
	if stolen.UnreadTotal != bobTruth.UnreadTotal || stolen.UnreadConversations != bobTruth.UnreadConversations {
		t.Fatalf("自报 mid 拿到的角标与真值不同（%d/%d vs %d/%d）：本用例的前提（mid 自报即生效）已变化，需同步改期望",
			stolen.UnreadTotal, stolen.UnreadConversations, bobTruth.UnreadTotal, bobTruth.UnreadConversations)
	}
	if stolen.UnreadTotal != 2 || stolen.UnreadConversations != 1 {
		t.Fatalf("bob 的角标应为 2/1（种子只有两条未读），实得 %d/%d", stolen.UnreadTotal, stolen.UnreadConversations)
	}
	// 反向也要钉住：本服务不会「因为 ctx 里没有可信身份」而拒绝，
	// 这条断言是「可信身份缺失时不 fail-closed」的直接证据。
	if _, err := callUnreadSummary(t, e, context.Background(), bob, true); err != nil {
		t.Fatalf("无凭证 ctx 调用被拒：%v（若这是修复后的行为，请连同上面的期望一起更新）", err)
	}
}

// TestGetUnreadSummaryExcludesHiddenConversations 钉住 getunreadsummarylogic.go:60 那条口径：
// hide_state 恒按「不含隐藏会话」算，与 ListConversations 默认过滤对得上。
func TestGetUnreadSummaryExcludesHiddenConversations(t *testing.T) {
	e := newEnv(t)
	sg := e.attachSocialGraphOnly(t)
	sg.follow(alice, bob)
	sg.follow(bob, alice)
	p := e.seedPair(t, alice, bob)
	e.seedMessage(t, p, alice, bob, 1, "未读消息一", model.MsgStateNormal)
	e.seedMessage(t, p, alice, bob, 2, "未读消息二", model.MsgStateNormal)

	if reply := unreadSummary(t, e, bob, "隐藏前"); reply.UnreadTotal != 2 {
		t.Fatalf("隐藏前 bob 未读应为 2，实得 %d", reply.UnreadTotal)
	}
	if err := e.hideConversation(t, p.convID, bob, true); err != nil {
		t.Fatalf("bob 隐藏会话：%v", err)
	}
	// 种子没变、成员行的 unread_count 也没被隐藏操作抹掉：变的只有 hide_state。
	// 因此「返回 0」只能来自查询层的过滤，这正是本用例要证的口径。
	if row := e.memberRow(t, p.convID, bob); row.UnreadCount != 2 || row.HideState != model.HideStateHidden {
		t.Fatalf("隐藏后成员行状态异常：unread=%d hide=%d（用例前提不成立，断言会失真）", row.UnreadCount, row.HideState)
	}

	reply := unreadSummary(t, e, bob, "隐藏后")
	if reply.UnreadTotal != 0 || reply.UnreadConversations != 0 {
		t.Fatalf("隐藏会话后角标仍是 %d/%d：本方法必须固定按不含隐藏会话汇总", reply.UnreadTotal, reply.UnreadConversations)
	}
	requireMatchesRows(t, e, bob, reply, "隐藏后的角标")
	// 对方（alice）不受本方隐藏影响：她那一行的 unread_count 本来就是 0，
	// 这里改钉「隐藏 bob 不会把 alice 的读数也改掉」——用 alice 取消隐藏前后一致来证。
	before := unreadSummary(t, e, alice, "alice 隐藏前")
	if err := e.hideConversation(t, p.convID, alice, true); err != nil {
		t.Fatalf("alice 隐藏会话：%v", err)
	}
	after := unreadSummary(t, e, alice, "alice 隐藏后")
	if before.UnreadTotal != after.UnreadTotal {
		t.Fatalf("alice 隐藏自己的会话把未读数改了：%d -> %d", before.UnreadTotal, after.UnreadTotal)
	}

	// 对照：include_hidden=true 的会话列表会把这条隐藏会话的未读算进 unread_total，
	// 那是入参差异不是口径漂移；未读汇总没有这个入参，所以恒按 false。
	hiddenPage, err := NewListConversationsLogic(bg(), e.svc).ListConversations(&rpc.ListConversationsReq{
		Mid: bob, IncludeHidden: true,
	})
	wantOK(t, hiddenPage, err, "ListConversations(include_hidden=true)")
	if hiddenPage.UnreadTotal != 2 {
		t.Fatalf("ListConversations(include_hidden=true) 的 unread_total 应为 2，实得 %d", hiddenPage.UnreadTotal)
	}
	def, err := NewListConversationsLogic(bg(), e.svc).ListConversations(&rpc.ListConversationsReq{Mid: bob})
	wantOK(t, def, err, "ListConversations 默认口径")
	if def.UnreadTotal != 0 {
		t.Fatalf("ListConversations 默认口径应与角标一致（0），实得 %d", def.UnreadTotal)
	}
}

// TestGetUnreadSummaryFollowsMarkReadAndNewMessages 把「汇总 = 投影之和」这条不变量
// 钉在三个连续时刻上：发信后、读到末尾后、又收到一条后。
// 顺带钉住 MarkRead 的截断语义：伪造一个超过会话 last_seq 的游标不会把未读抹掉。
func TestGetUnreadSummaryFollowsMarkReadAndNewMessages(t *testing.T) {
	e := newEnv(t)
	p := e.seedPair(t, alice, bob)
	e.seedMessage(t, p, alice, bob, 1, "一", model.MsgStateNormal)
	e.seedMessage(t, p, alice, bob, 2, "二", model.MsgStateNormal)

	first := unreadSummary(t, e, bob, "起点")
	if first.UnreadTotal != 2 || first.UnreadConversations != 1 {
		t.Fatalf("起点未读应为 2/1，实得 %d/%d", first.UnreadTotal, first.UnreadConversations)
	}

	// 游标伪造到 seq=9：MarkRead 按 last_seq 截断为 2（markreadlogic.go 的 GREATEST 下界）。
	mk, err := e.markRead(t, p.convID, bob, 9)
	wantOK(t, mk, err, "越界游标 MarkRead")
	if !mk.Changed || mk.ReadSeq != 2 {
		t.Fatalf("越界游标应按 last_seq 截断，实得 read_seq=%d changed=%v", mk.ReadSeq, mk.Changed)
	}
	second, err := callUnreadSummary(t, e, bg(), bob, true)
	wantOK(t, second, err, "MarkRead 后")
	if second.UnreadTotal != 0 || second.UnreadConversations != 0 {
		t.Fatalf("MarkRead 后角标仍为 %d/%d：汇总与游标没写在同一行上", second.UnreadTotal, second.UnreadConversations)
	}
	requireMatchesRows(t, e, bob, second, "MarkRead 后的角标")
	if second.ComputedAt < first.ComputedAt {
		t.Fatalf("computed_at 倒挂：%d -> %d", first.ComputedAt, second.ComputedAt)
	}

	// 再来一条：接收方角标 +1，会话数仍是 1（同一会话不重复计数）。
	e.seedMessage(t, p, alice, bob, 3, "三", model.MsgStateNormal)
	third, err := callUnreadSummary(t, e, bg(), bob, true)
	wantOK(t, third, err, "新消息后")
	if third.UnreadTotal != 1 || third.UnreadConversations != 1 {
		t.Fatalf("新消息后应为 1/1，实得 %d/%d", third.UnreadTotal, third.UnreadConversations)
	}
	requireMatchesRows(t, e, bob, third, "新消息后的角标")
	if third.ComputedAt < second.ComputedAt {
		t.Fatalf("computed_at 倒挂：%d -> %d", second.ComputedAt, third.ComputedAt)
	}
	// 发送方（alice）的角标始终为 0：bob 的游标推进与隐藏都不该写到她那行上。
	if got := unreadSummary(t, e, alice, "alice 全程"); got.UnreadTotal != 0 || got.UnreadConversations != 0 {
		t.Fatalf("alice 的角标被 bob 的未读污染：%d/%d", got.UnreadTotal, got.UnreadConversations)
	}
	// 三个时刻都要求「接口值 == 投影重算值」，其中第三个时刻的投影是 1/1：
	// 这条同时钉住了「汇总不是常数、也不是消息总数」。
}

// TestGetUnreadSummarySourcesFromMySQLWhenCacheUnavailable 钉住「缓存不是事实源」：
//  1. Cache==nil（helper 短路）与 Cache 指向一个连不上的实例（真实调用失败）两种形态下，
//     返回值都必须是 MySQL 真值，而不是 (0,0) 这种「拿不到就当没有」的假成功；
//  2. force=true/false 都不改变这一结论；
//  3. 读失败与写失败都不影响结果，也不对外报错（README：缓存缺失只回源）。
func TestGetUnreadSummarySourcesFromMySQLWhenCacheUnavailable(t *testing.T) {
	e := newEnv(t)
	p := e.seedPair(t, alice, bob)
	e.seedMessage(t, p, alice, bob, 1, "缓存挂了也要看得见", model.MsgStateNormal)

	// 形态一：没配缓存（newEnv 里 Cache 就是 nil）。
	first := unreadSummary(t, e, bob, "未配置缓存")
	if first.UnreadTotal != 1 {
		t.Fatalf("未配置缓存时未读应为 1，实得 %d", first.UnreadTotal)
	}
	mark := e.markCalls()
	second, err := callUnreadSummary(t, e, bg(), bob, false)
	wantOK(t, second, err, "未配置缓存第二次")
	if second.UnreadTotal != 1 {
		t.Fatalf("第二次调用未读应为 1，实得 %d", second.UnreadTotal)
	}
	// Cache==nil 时也必须回源：这是「缓存不是事实源」最直接的证据。
	if mark.callsOf("Members.SumUnread") != 1 {
		t.Fatalf("没配缓存时没有回源 MySQL（SumUnread %d 次）", mark.callsOf("Members.SumUnread"))
	}

	// 形态二：配了但连不上（真实 redis 客户端 + 短截止点，保证不拖住测试）。
	e.svc.Cache = deadCache(t)
	ctx := shortCtx(t)
	cacheMark := e.markCalls()
	before := e.calls("Members.SumUnread")
	got, err := callUnreadSummary(t, e, ctx, bob, false)
	wantOK(t, got, err, "缓存故障时取角标")
	if got.UnreadTotal != 1 || got.UnreadConversations != 1 {
		t.Fatalf("缓存故障时角标被算成 (%d,%d)，应为 (1,1)：「拿不到缓存」不等于「没有未读」",
			got.UnreadTotal, got.UnreadConversations)
	}
	if e.calls("Members.SumUnread") == before {
		t.Fatal("缓存故障时没有回源 Members.SumUnread：角标成了缓存的奴隶")
	}
	forced, err := callUnreadSummary(t, e, ctx, bob, true)
	wantOK(t, forced, err, "force=true 且缓存故障")
	if forced.UnreadTotal != 1 {
		t.Fatalf("force=true 时未读应为 1，实得 %d", forced.UnreadTotal)
	}
	// 缓存挂了的这两次调用同样只许读成员表：读路径不会因为「补缓存」去扫消息表。
	requireOnlyHandleRead(t, cacheMark, "Members", "缓存故障下的两次调用")
	if n := cacheMark.callsOf("Members.SumUnread"); n != 2 {
		t.Fatalf("两次调用应各回源一次，Members.SumUnread 实得 %d 次", n)
	}
	e.svc.Cache = nil
}

// TestUnreadCacheKeyIsNamespaced 按纯函数钉住缓存键：
// 前缀是本服务独立的键空间（AGENTS.md §5 的 key 边界），mid 段决定「谁的角标」。
// 「配错前缀 = 读到别的服务的键」在进程内不可观测，键格式是唯一可证的那半。
func TestUnreadCacheKeyIsNamespaced(t *testing.T) {
	if got, want := unreadCacheKey(bob), cacheKeyPrefix+"unread|202"; got != want {
		t.Fatalf("未读缓存键格式变了：%q，期望 %q", got, want)
	}
	if unreadCacheKey(alice) == unreadCacheKey(bob) {
		t.Fatal("两个 mid 的角标键相同：会互相顶掉")
	}
	// 与其它三类键不撞命名空间：角标被频控计数覆盖会直接算错未读。
	keys := map[string]string{
		"unread":  unreadCacheKey(alice),
		"verdict": pairVerdictCacheKey("blk", alice, bob),
		"rate":    sendRateKey(alice),
		"quota":   strangerQuotaKey(alice, "2026-09-22"),
	}
	seen := map[string]string{}
	for kind, k := range keys {
		if !strings.HasPrefix(k, cacheKeyPrefix) {
			t.Fatalf("%s 键缺服务命名空间前缀：%q", kind, k)
		}
		if prev, ok := seen[k]; ok {
			t.Fatalf("%s 与 %s 用了同一个键 %q", kind, prev, k)
		}
		seen[k] = kind
	}
}

// TestGetUnreadSummaryGuardsAndPropagatesErrors 钉住「守卫先于触库」与依赖错误原样上抛。
func TestGetUnreadSummaryGuardsAndPropagatesErrors(t *testing.T) {
	e := newEnv(t)
	p := e.seedPair(t, alice, bob)
	e.seedMessage(t, p, alice, bob, 1, "有未读", model.MsgStateNormal)

	for _, tc := range []struct {
		name string
		mid  int64
	}{
		{name: "mid=0", mid: 0},
		{name: "mid 负数", mid: -7},
		{name: "mid = int64 最小值", mid: -9223372036854775808},
	} {
		mark := e.markCalls()
		before := e.probe()
		reply, err := callUnreadSummary(t, e, bg(), tc.mid, false)
		wantErr(t, err, model.ErrInvalidMid, tc.name)
		if reply != nil {
			t.Fatalf("%s：入参被拒却回了非 nil 响应 %v", tc.name, reply)
		}
		// 守卫发生在任何一次查询之前：非法 mid 不允许扫成员表（那是白送的探测面）。
		if n := mark.handleCallsOf("Members"); n != 0 {
			t.Fatalf("%s：守卫之后仍查了成员表 %d 次", tc.name, n)
		}
		e.requireSameRowsAndWrites(t, before, tc.name)
	}

	// 依赖错误原样上抛：不能退化成 (0,0) 的成功。
	e.st.failTx["Members.SumUnread"] = model.ErrConcurrentUpdate
	_, err := callUnreadSummary(t, e, bg(), bob, false)
	wantFail(t, err, model.ErrConcurrentUpdate, "Members.SumUnread 故障")
	delete(e.st.failTx, "Members.SumUnread")

	// force=true 必须跳过缓存读、直接回源：否则「刚 MarkRead 完仍然红点」无法用 force 自救。
	// 这里的注入错误刻意用依赖层而不是哨兵：验证的是「原样上抛」，不是某个业务码。
	e.st.failTx["Members.SumUnread"] = errInjected
	forced, err := callUnreadSummary(t, e, bg(), bob, true)
	wantFail(t, err, errInjected, "force=true 时 SumUnread 故障")
	if forced != nil {
		t.Fatalf("依赖失败时仍回了响应体：%v", forced)
	}
	delete(e.st.failTx, "Members.SumUnread")

	// 恢复后仍然读到真值：错误注入没有把库改坏。
	got := unreadSummary(t, e, bob, "恢复后")
	if got.UnreadTotal != 1 {
		t.Fatalf("恢复后的未读应为 1，实得 %d", got.UnreadTotal)
	}
}
