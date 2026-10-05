// 本文件钉住两个「会话面」入口：GetOrCreateConversation（建档）与 ListConversations（列表）。
//
// 建档侧真正要证的不是「能建出来」，而是三件容易写错的事：
//  1. 幂等来自 uniq_pair_key，同一对用户反复进会话页只能有一行会话（第二行=数据分裂）；
//  2. 会话行与两行成员投影必须同事务：成员行是越权判定的唯一依据，
//     缺行的会话谁都读不到，而崩溃窗口必须能被下一次调用自愈（getorcreateconversationlogic.go:56）；
//  3. 建档不判关系也不扣配额（README 门禁 c 的分工），因此它必须一次下游都不调用。
//
// 列表侧真正要证的也是三件事：
//  1. 只扫登录者自己的成员行（idx_mid_list），页里出现的每一行都属于他；
//  2. 过滤在查询层：黑名单双向命中、隐藏会话、脏投影行都不外漏，
//     而「下游未配置」与「下游故障」是两种结果（整页拒 vs 丢该行）；
//  3. 摘要只来自成员投影列，整个读路径一条消息行都不取（隐私 P4 的进程内最强证明）。
//
// 隐私约束（AGENTS.md §7）：这里的 mid 全是假值（101/202/303/909），
// 断言里只出现「哨兵字符串是否泄漏」，不引入任何真实标识。

package logic

import (
	"strings"
	"testing"

	"go-video/services/private-message/model"
	"go-video/services/private-message/rpc"
)

func callGetOrCreate(t *testing.T, e *env, mid, peer int64) (*rpc.GetOrCreateConversationReply, error) {
	t.Helper()
	return NewGetOrCreateConversationLogic(bg(), e.svc).GetOrCreateConversation(&rpc.GetOrCreateConversationReq{
		Mid: mid, PeerMid: peer,
	})
}

func callListConversations(t *testing.T, e *env, in *rpc.ListConversationsReq) (*rpc.ListConversationsReply, error) {
	t.Helper()
	return NewListConversationsLogic(bg(), e.svc).ListConversations(in)
}

func listConversations(t *testing.T, e *env, in *rpc.ListConversationsReq, label string) *rpc.ListConversationsReply {
	t.Helper()
	got, err := callListConversations(t, e, in)
	return wantOK(t, got, err, label)
}

// convIDs 把一页里的会话主键按返回顺序取出来（翻页断言只关心顺序与集合，不关心投影细节）。
func convIDs(page *rpc.ListConversationsReply) []int64 {
	out := make([]int64, 0, len(page.List))
	for _, c := range page.List {
		out = append(out, c.ConversationId)
	}
	return out
}

// sgBlackPairs 取回第 from 次之后 social-graph 实际收到的 IsBlacked 查询对（[mid, owner]）。
// 判定的「对象」也是隐私：这里钉的是「列表只围绕调用者自己的对端发问」，
// 而不是只看发了几次。
func sgBlackPairs(t *testing.T, sg *fakeSocialGraphServer, from int) [][2]int64 {
	t.Helper()
	sg.mu.Lock()
	defer sg.mu.Unlock()
	if from > len(sg.blackReqs) {
		t.Fatalf("脚手架记账被破坏：social-graph 累计请求数 %d 小于快照 %d", len(sg.blackReqs), from)
	}
	out := make([][2]int64, 0, len(sg.blackReqs)-from)
	for _, r := range sg.blackReqs[from:] {
		out = append(out, [2]int64{r.GetMid(), r.GetOwner()})
	}
	return out
}

// TestGetOrCreateConversationIsIdempotentOnPairKey 钉住幂等的来源与落地形状：
// (a,b) 与 (b,a) 命中同一行，会话 1 行 + 成员 2 行，重放不多一行。
func TestGetOrCreateConversationIsIdempotentOnPairKey(t *testing.T) {
	e := newEnv(t)
	mark := e.markCalls()
	txm := e.txMark()

	first, err := callGetOrCreate(t, e, alice, bob)
	wantOK(t, first, err, "首次建档")
	if !first.Created || first.ConversationId <= 0 {
		t.Fatalf("首次建档应回 created=true 与正数主键，实得 created=%v id=%d", first.Created, first.ConversationId)
	}
	if first.State != model.ConversationStateNormal || first.Ctime <= 0 {
		t.Fatalf("新建会话状态/创建时间异常：state=%d ctime=%d", first.State, first.Ctime)
	}
	convsAfterFirst, membersAfterFirst := len(e.st.convs), len(e.st.members)
	if convsAfterFirst != 1 || membersAfterFirst != 2 {
		t.Fatalf("首次建档落地应为 1 会话 + 2 成员行，实得 %d + %d", convsAfterFirst, membersAfterFirst)
	}
	if n := e.txsSince(txm); n != 1 {
		t.Fatalf("建档必须恰好一个事务（会话行与成员行不能分开提交），实得 %d 个", n)
	}

	// 反序调用：pair_key 由 model.PairKey 归一，(b,a) 必须命中同一行。
	second, err := callGetOrCreate(t, e, bob, alice)
	wantOK(t, second, err, "反序重放")
	if second.Created {
		t.Fatal("反序调用被当成新建：pair_key 归一失效，同一对用户会出现两行会话")
	}
	if second.ConversationId != first.ConversationId {
		t.Fatalf("反序调用命中了另一行会话：%d -> %d", first.ConversationId, second.ConversationId)
	}
	if len(e.st.convs) != 1 || len(e.st.members) != 2 {
		t.Fatalf("重放后行数变成 %d 会话 + %d 成员", len(e.st.convs), len(e.st.members))
	}

	// 第三次（正序）仍然不动：幂等不是「第二次特例」。
	third, err := callGetOrCreate(t, e, alice, bob)
	wantOK(t, third, err, "第三次重放")
	if third.Created || third.ConversationId != first.ConversationId {
		t.Fatalf("第三次重放又建了一行：created=%v id=%d", third.Created, third.ConversationId)
	}
	if len(e.st.convs) != 1 {
		t.Fatalf("三次调用后会话行数 %d，应为 1", len(e.st.convs))
	}

	// 落库形状：pair_key 与 user_a/user_b 的顺序由同一规则决定，成员投影全零。
	conv := e.convRow(t, first.ConversationId)
	if conv.PairKey != model.PairKey(alice, bob) || conv.PairKey != model.PairKey(bob, alice) {
		t.Fatalf("pair_key 不满足交换不变量：%q", conv.PairKey)
	}
	if conv.UserA != min(alice, bob) || conv.UserB != max(alice, bob) {
		t.Fatalf("user_a/user_b 未按大小归一：%d/%d", conv.UserA, conv.UserB)
	}
	for _, mid := range []int64{alice, bob} {
		m := e.memberRow(t, first.ConversationId, mid)
		wantPeer := conv.UserB
		if mid == conv.UserB {
			wantPeer = conv.UserA
		}
		if m.PeerMid != wantPeer {
			t.Fatalf("mid=%d 的成员行 peer_mid=%d，应为对方 %d", mid, m.PeerMid, wantPeer)
		}
		// 建档不伪造「会话里有内容」：展示列与游标必须全空/全零。
		if m.ReadSeq != 0 || m.UnreadCount != 0 || m.HideState != model.HideStateNormal ||
			m.LastMsgID != 0 || m.LastSeq != 0 || m.LastPreview != "" {
			t.Fatalf("成员投影初值不干净：%+v", m)
		}
	}
	// 建档只碰这两张表：读了消息表说明「顺手回一条内容」，读偏好表说明「顺手判门槛」——
	// 两者都不属于建档（README：配额与门禁在 SendMessage 上）。
	requireHandlesRead(t, mark, []string{"Conversations", "Members"}, "建档")
	if n := mark.callsOf("Conversations.FindOrCreateInTx"); n != 3 {
		t.Fatalf("三次调用应各走一次 FindOrCreateInTx，实得 %d 次", n)
	}
}

// TestGetOrCreateConversationHealsMissingMemberRows 钉住那段「每轮都补成员行」的自愈：
// 崩溃在「会话已建、成员行未写」窗口时会留下一个谁都读不到的会话，
// 重进会话页必须把它补回来，而且不再新建会话行。
func TestGetOrCreateConversationHealsMissingMemberRows(t *testing.T) {
	e := newEnv(t)
	first, err := callGetOrCreate(t, e, alice, bob)
	wantOK(t, first, err, "首次建档")
	convID := first.ConversationId

	// 造崩溃窗口：只删 bob 那一行成员投影（会话主体还在）。
	delete(e.st.members, memberKey(convID, bob))
	if _, ok := e.st.memberRow(convID, bob); ok {
		t.Fatal("种子没删掉成员行，本用例测不到崩溃窗口")
	}

	second, err := callGetOrCreate(t, e, alice, bob)
	wantOK(t, second, err, "自愈重放")
	if second.ConversationId != convID {
		t.Fatalf("自愈时另开了一行会话：%d -> %d", convID, second.ConversationId)
	}
	if len(e.st.convs) != 1 {
		t.Fatalf("自愈不该新建会话行，现有 %d 行", len(e.st.convs))
	}
	healed, ok := e.st.memberRow(convID, bob)
	if !ok {
		t.Fatal("重放没有补回缺失的成员行：这个会话对 bob 永远不可见")
	}
	if healed.PeerMid != alice || healed.UnreadCount != 0 || healed.ReadSeq != 0 {
		t.Fatalf("补回来的成员行形状不对：%+v", healed)
	}
	// alice 那一行不能被动到（Ensure 只在缺行时插入）。
	if aliceRow := e.memberRow(t, convID, alice); aliceRow.PeerMid != bob {
		t.Fatalf("自愈把 alice 的成员行改了：peer_mid=%d", aliceRow.PeerMid)
	}
}

// TestGetOrCreateConversationGuardsBeforeAnyWrite 钉住入参守卫：
// 自聊与缺主体都在开事务之前被拒，且不留任何行。
func TestGetOrCreateConversationGuardsBeforeAnyWrite(t *testing.T) {
	e := newEnv(t)
	before := e.probe()
	for _, tc := range []struct {
		name      string
		mid, peer int64
		want      error
	}{
		{name: "mid=0", mid: 0, peer: bob, want: model.ErrInvalidMid},
		{name: "peer=0", mid: alice, peer: 0, want: model.ErrInvalidMid},
		{name: "mid 负数", mid: -1, peer: bob, want: model.ErrInvalidMid},
		{name: "peer 负数", mid: alice, peer: -8, want: model.ErrInvalidMid},
		{name: "自聊", mid: alice, peer: alice, want: model.ErrSelfConversation},
	} {
		mark := e.markCalls()
		txm := e.txMark()
		got, err := callGetOrCreate(t, e, tc.mid, tc.peer)
		wantErr(t, err, tc.want, tc.name)
		if got != nil {
			t.Fatalf("%s：被拒却回了会话体 %v", tc.name, got)
		}
		// 守卫先于事务、也先于任何一次表访问。
		if n := e.txsSince(txm); n != 0 {
			t.Fatalf("%s：入参被拒仍开了 %d 个事务", tc.name, n)
		}
		if n := mark.handleCallsOf("Conversations") + mark.handleCallsOf("Members"); n != 0 {
			t.Fatalf("%s：入参被拒仍访问了会话/成员表 %d 次", tc.name, n)
		}
		e.requireSameRowsAndWrites(t, before, tc.name)
	}
	// 自聊必须是 ErrSelfConversation 而不是 ErrInvalidMid：
	// 网关要按这两个不同哨兵回不同文案（「不能给自己发」vs「未登录」）。
	_, err := callGetOrCreate(t, e, bob, bob)
	wantErr(t, err, model.ErrSelfConversation, "自聊与缺主体的错误码必须不同")
	if err != nil && strings.Contains(err.Error(), "invalid mid") {
		t.Fatalf("自聊回成了缺主体的错误：%v", err)
	}
}

// TestGetOrCreateConversationJudgesNoRelationship 钉住建档的职责边界：
// 关系/门槛/配额都不在建档判，因此它一次下游都不能调。
func TestGetOrCreateConversationJudgesNoRelationship(t *testing.T) {
	e := newEnv(t)
	d := e.attachDownstream(t)
	// 双向拉黑：建档仍然成功（会话行不是内容），但发送侧的门禁必须挡住这一对。
	d.sg.block(alice, bob)
	d.sg.block(bob, alice)

	got, err := callGetOrCreate(t, e, alice, bob)
	wantOK(t, got, err, "被拉黑的一对建档")
	if !got.Created {
		t.Fatal("被拉黑的一对建档失败：建档不该判关系")
	}
	if n := d.sg.blackCalls(); n != 0 {
		t.Fatalf("建档查了 %d 次黑名单：门禁顺序里这一步不该存在（README 门禁 c 在 SendMessage）", n)
	}
	if n := len(d.risk.requests()); n != 0 {
		t.Fatalf("建档调了 %d 次风控", n)
	}
	if n := len(d.moder.requests()); n != 0 {
		t.Fatalf("建档调了 %d 次机审", n)
	}
	// 也不写内容：会话与成员之外一行都不许多。
	if len(e.st.msgs) != 0 || len(e.st.reports) != 0 || len(e.st.settings) != 0 {
		t.Fatalf("建档写了内容/举报/偏好：msg=%d report=%d setting=%d", len(e.st.msgs), len(e.st.reports), len(e.st.settings))
	}
}

// TestGetOrCreateConversationPropagatesErrorAndRollsBack 钉住事务边界：
// 成员行写失败时会话行必须一起回滚（否则留下谁都读不到的孤儿会话），
// 且依赖错误原样上抛。
func TestGetOrCreateConversationPropagatesErrorAndRollsBack(t *testing.T) {
	e := newEnv(t)

	e.st.failTx["Conversations.FindOrCreateInTx"] = errInjected
	got, err := callGetOrCreate(t, e, alice, bob)
	wantFail(t, err, errInjected, "会话写入失败")
	if got != nil {
		t.Fatalf("失败仍回了会话体：%v", got)
	}
	if len(e.st.convs) != 0 || len(e.st.members) != 0 {
		t.Fatalf("会话写入失败后仍有行落地：conv=%d member=%d", len(e.st.convs), len(e.st.members))
	}
	delete(e.st.failTx, "Conversations.FindOrCreateInTx")

	// 关键一支：会话行已经写进事务，成员行失败 ⇒ 整个事务回滚，不能留孤儿会话。
	e.st.failTx["Members.Ensure"] = errInjected
	_, err = callGetOrCreate(t, e, alice, bob)
	wantFail(t, err, errInjected, "成员行写入失败")
	if len(e.st.convs) != 0 {
		t.Fatalf("成员行失败后会话行没回滚，留下 %d 行孤儿会话（谁都读不到，且会挡住后续建档）", len(e.st.convs))
	}
	if len(e.st.members) != 0 {
		t.Fatalf("成员行失败后仍落了 %d 行", len(e.st.members))
	}
	delete(e.st.failTx, "Members.Ensure")

	// 撤销注入后同一个 pair 仍能建档并回 created=true：上面的回滚必须「干净」到不留痕迹，
	// 否则这里会拿到 created=false（一个没人能读的会话）。
	after, err := callGetOrCreate(t, e, alice, bob)
	wantOK(t, after, err, "回滚后重新建档")
	if !after.Created {
		t.Fatal("回滚后重建成「命中已有行」：说明上一次留下了没被回滚的会话行")
	}
	if len(e.st.convs) != 1 || len(e.st.members) != 2 {
		t.Fatalf("重建后行数 %d/%d，应为 1/2", len(e.st.convs), len(e.st.members))
	}
}

// TestGetOrCreateConversationSelfReportedPairCreatesRowsForOthers 钉住建档面的两个缺口。
//
// 缺陷一（存在性探针）：getorcreateconversationlogic.go:69-72 —— created 字段直接回给调用方，
// 而 mid/peer_mid 都是自报值（网关侧 gateway/app/api/app.api:3119 已注明「未配置 jwt 中间件」），
// 于是任何人用一个 boolean 就能判定「任意两个人之间是否已有会话」——
// 这是一条不经过任何内容读取的关系事实泄露（谁和谁聊过）。
// 缺陷二（无界建行）：同一处没有任何配额/频率限制（实现要点 4 明确「不扣陌生人配额」），
// 遍历 peer_mid 即可以 O(1) 成本为任意用户对刷 pm_conversation + 2 行成员投影。
// 修法方向：mid 取可信身份；created 不对外（或只在对调用者本人是成员时回）；
// 建档入口按调用者加独立配额（与 SendMessage 的日配额分开计）。
//
// ⚠ 改生产代码前不要动这条断言的期望。
func TestGetOrCreateConversationSelfReportedPairCreatesRowsForOthers(t *testing.T) {
	e := newEnv(t)

	// 调用者既不是 alice 也不是 carol：他 nevertheless 能把这两人「连」起来。
	got, err := callGetOrCreate(t, e, alice, carol)
	wantOK(t, got, err, "无关调用者为 alice/carol 建档")
	if !got.Created {
		t.Fatal("建档没生效：本用例的前提（mid/peer 全自报）已变化，需同步改期望")
	}
	if len(e.st.convs) != 1 || len(e.st.members) != 2 {
		t.Fatalf("行数 %d/%d，应为 1/2", len(e.st.convs), len(e.st.members))
	}
	// 落地行的归属是 alice 与 carol，与调用者无关：证明「服务不校验调用者是不是主体」。
	conv := e.convRow(t, got.ConversationId)
	if conv.UserA != min(alice, carol) || conv.UserB != max(alice, carol) {
		t.Fatalf("会话主体不是被指定的那对：%d/%d", conv.UserA, conv.UserB)
	}
	for _, mid := range []int64{alice, carol} {
		if _, ok := e.st.memberRow(got.ConversationId, mid); !ok {
			t.Fatalf("缺 mid=%d 的成员行", mid)
		}
	}
	// 第二次调用回 created=false —— 这就是那条布尔探针。
	again, err := callGetOrCreate(t, e, alice, carol)
	wantOK(t, again, err, "第二次建档（探针）")
	if again.Created || again.ConversationId != got.ConversationId {
		t.Fatalf("探针语义变了：created=%v id=%d", again.Created, again.ConversationId)
	}
	// 遍历 peer_mid 就能批量建行：每一次调用都必须稳定多出一行会话 + 2 行成员投影，
	// 且增量为线性、无配额封顶（这正是缺陷二的口径）。
	convs0, members0 := len(e.st.convs), len(e.st.members)
	for i, peer := range []int64{bob, mallory, 404} {
		if _, err := callGetOrCreate(t, e, alice, peer); err != nil {
			t.Fatalf("为 alice/%d 建档失败：%v", peer, err)
		}
		if n := len(e.st.convs) - convs0; n != i+1 {
			t.Fatalf("第 %d 次刷行后会话增量 = %d, want %d（多一行少一行都说明建行口径变了）", i+1, n, i+1)
		}
		if n := len(e.st.members) - members0; n != 2*(i+1) {
			t.Fatalf("第 %d 次刷行后成员投影增量 = %d, want %d", i+1, n, 2*(i+1))
		}
	}
	if len(e.st.convs) != 4 || len(e.st.members) != 8 {
		t.Fatalf("行数 %d/%d，应为「1 次 + 3 次刷行」= 4 会话 / 8 成员行：本用例钉的是「刷行没有上限」",
			len(e.st.convs), len(e.st.members))
	}
}

// TestListConversationsPagesByDualColumnCursor 用「自己按 SQL 口径排一遍序」的期望
// 钉住 (last_msg_time, id) 双列游标：三条会话的消息落在同一秒，
// 单列时间游标必然翻不全或重复。
func TestListConversationsPagesByDualColumnCursor(t *testing.T) {
	e := newEnv(t)
	sg := e.attachSocialGraphOnly(t)
	sg.follow(alice, bob)
	p1 := e.seedPair(t, bob, alice)
	p2 := e.seedPair(t, bob, carol)
	p3 := e.seedPair(t, bob, mallory)
	for _, p := range []pair{p1, p2, p3} {
		e.seedMessage(t, p, p.otherOf(bob), bob, 1, "同一秒的一条消息", model.MsgStateNormal)
	}
	// 「同一秒」这个前提必须自己造出来，不能指望时钟：seed 三条消息期间跨了一秒，
	// 双列游标就退化成单列、本用例前提消失。同时 last_msg_time 必须 >0 ——
	// encodeTimeIDCursor 对 0 静默回空串，logic 会因此把 has_more 抹成 false（翻页假通过）。
	const sameSecond = int64(1700000000)
	for _, p := range []pair{p1, p2, p3} {
		e.rawMember(t, p.convID, bob, func(m *model.ConversationMember) { m.LastMsgTime = sameSecond })
	}
	// 前提检查：三条的成员 ID 必须严格递增、last_msg_time 必须相同（否则测不到双列游标）。
	idOf := func(convID int64) int64 { return e.memberRow(t, convID, bob).ID }
	times := map[int64]bool{}
	for _, convID := range []int64{p1.convID, p2.convID, p3.convID} {
		times[e.memberRow(t, convID, bob).LastMsgTime] = true
	}
	if len(times) != 1 || !times[sameSecond] {
		t.Fatalf("三条会话的 last_msg_time 不同（%v），本用例的双列游标前提不成立", times)
	}
	if !(idOf(p1.convID) < idOf(p2.convID) && idOf(p2.convID) < idOf(p3.convID)) {
		t.Fatalf("成员行 ID 非递增：%d %d %d", idOf(p1.convID), idOf(p2.convID), idOf(p3.convID))
	}
	// 期望顺序独立算出来：DESC(last_msg_time, id)。
	want := []int64{p3.convID, p2.convID, p1.convID}

	var (
		cursor  string
		got     []int64
		pages   int
		perPage = int32(1)
	)
	for {
		if pages >= 10 {
			t.Fatalf("翻页 10 次仍未结束：游标不前进（%q）", cursor)
		}
		page := listConversations(t, e, &rpc.ListConversationsReq{Mid: bob, Cursor: cursor, Ps: perPage}, "翻页")
		pages++
		ids := convIDs(page)
		if int32(len(ids)) > perPage {
			t.Fatalf("第 %d 页回了 %d 条，超过 ps=%d：无界返回", pages, len(ids), perPage)
		}
		got = append(got, ids...)
		if !page.HasMore {
			if page.NextCursor != "" {
				t.Fatalf("最后一页仍给了游标 %q", page.NextCursor)
			}
			break
		}
		if page.NextCursor == "" {
			t.Fatalf("第 %d 页 has_more=true 却没有游标：客户端会停在半路", pages)
		}
		cursor = page.NextCursor
	}
	if pages != 3 {
		t.Fatalf("三条会话按 ps=1 应恰好 3 页，实得 %d 页", pages)
	}
	if len(got) != len(want) {
		t.Fatalf("翻页共拿到 %d 条，期望 %d 条", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条会话不符：期望 %d，实得 %d（全部翻页结果 %v）", i+1, want[i], got[i], got)
		}
	}
	// 不重复、不漏：这是双列游标唯一的正确性定义。
	seen := map[int64]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("翻页出现重复会话 %d：(time,id) 游标退化成了单列", id)
		}
		seen[id] = true
	}
}

// TestListConversationsPageSizeAndCursorGuards 钉住「守卫全部先于查库」与页大小上下限。
func TestListConversationsPageSizeAndCursorGuards(t *testing.T) {
	e := newEnv(t)
	// 读侧门禁的真值在 social-graph，未配置时整页 fail-closed（那条分支由
	// TestListConversationsSocialGraphMissingRejectsWholePage 单独钉），所以「合法页大小」
	// 这一段必须先把下游接上，否则测的是「下游缺失」而不是页大小。
	e.attachSocialGraphOnly(t)
	p := e.seedPair(t, alice, bob)
	e.seedMessage(t, p, alice, bob, 1, "先有一条消息", model.MsgStateNormal)

	// 守卫顺序：mid 非法时即使 ps 也非法，回的仍是 ErrInvalidMid（先主体后细节）。
	mark := e.markCalls()
	_, err := callListConversations(t, e, &rpc.ListConversationsReq{Mid: 0, Ps: 999})
	wantErr(t, err, model.ErrInvalidMid, "mid 优先于 ps")
	if n := mark.handleCallsOf("Members"); n != 0 {
		t.Fatalf("mid 被拒仍扫了成员表 %d 次", n)
	}

	for _, tc := range []struct {
		name string
		in   *rpc.ListConversationsReq
		want error
	}{
		{name: "ps 负数", in: &rpc.ListConversationsReq{Mid: bob, Ps: -1}, want: model.ErrInvalidPage},
		{name: "ps 超上限", in: &rpc.ListConversationsReq{Mid: bob, Ps: 51}, want: model.ErrPsTooLarge},
		{name: "游标不是 time:id", in: &rpc.ListConversationsReq{Mid: bob, Cursor: "abc"}, want: model.ErrInvalidCursor},
		{name: "游标缺 id 段", in: &rpc.ListConversationsReq{Mid: bob, Cursor: "1700000000"}, want: model.ErrInvalidCursor},
		{name: "游标 id 为 0", in: &rpc.ListConversationsReq{Mid: bob, Cursor: "1700000000:0"}, want: model.ErrInvalidCursor},
		{name: "游标时间为负", in: &rpc.ListConversationsReq{Mid: bob, Cursor: "-1:5"}, want: model.ErrInvalidCursor},
	} {
		mark := e.markCalls()
		before := e.probe()
		got, err := callListConversations(t, e, tc.in)
		wantErr(t, err, tc.want, tc.name)
		if got != nil {
			t.Fatalf("%s：被拒却回了一页 %v", tc.name, got.List)
		}
		if n := mark.handleCallsOf("Members") + mark.handleCallsOf("Conversations"); n != 0 {
			t.Fatalf("%s：入参被拒仍查了 %d 次表（不可解析的游标绝不允许退化成第一页）", tc.name, n)
		}
		e.requireSameRowsAndWrites(t, before, tc.name)
	}

	// ps 恰好等于上限合法；ps=0 走服务端默认（两者都不该报错）。
	for _, ps := range []int32{0, 50} {
		page := listConversations(t, e, &rpc.ListConversationsReq{Mid: bob, Ps: ps}, "合法页大小")
		if len(page.List) != 1 {
			t.Fatalf("ps=%d 应回 1 条，实得 %d", ps, len(page.List))
		}
		if page.HasMore {
			t.Fatalf("ps=%d 只有一条数据却回 has_more=true", ps)
		}
	}
}

// TestListConversationsOnlyReturnsCallersOwnRows 钉住行级隔离：
// 与 alice 的会话不会出现在 mallory 的页里，即使他手里拿着 conversation_id。
func TestListConversationsOnlyReturnsCallersOwnRows(t *testing.T) {
	e := newEnv(t)
	e.attachSocialGraphOnly(t)
	p := e.seedPair(t, alice, bob)
	e.seedMessage(t, p, alice, bob, 1, "只属于这两个人", model.MsgStateNormal)
	// mallory 自己有一条会话，用来证明「空页」不是因为整个查询坏了。
	mp := e.seedPair(t, mallory, carol)
	e.seedMessage(t, mp, carol, mallory, 1, "mallory 自己的", model.MsgStateNormal)

	mark := e.markCalls()
	page := listConversations(t, e, &rpc.ListConversationsReq{Mid: mallory}, "mallory 的会话列表")
	if len(page.List) != 1 {
		t.Fatalf("mallory 应只看到自己那 1 条会话，实得 %d 条", len(page.List))
	}
	if page.List[0].ConversationId != mp.convID || page.List[0].PeerMid != carol {
		t.Fatalf("mallory 的页里混进了别人的会话：conv=%d peer=%d", page.List[0].ConversationId, page.List[0].PeerMid)
	}
	if strings.Contains(page.List[0].LastPreview, "只属于这两个人") {
		t.Fatal("mallory 的列表里出现了 alice/bob 会话的摘要")
	}
	// 扫成员表 + 按 ID 批量取会话主体，此外不碰任何一张表。
	if n := mark.callsOf("Members.ListByConversation"); n != 0 {
		t.Fatalf("列表顺手按会话查了成员表 %d 次（那会绕过 mid 边界）", n)
	}
	requireHandlesRead(t, mark, []string{"Members", "Conversations"}, "整个列表调用")

	// 与 bob 无关的 mid 拿不到 bob 的行：alice/bob 那条会话对 mallory 完全不存在。
	// bob 自己能看到（对照，证明上面「看不到」是隔离而不是数据缺失）。
	bobPage := listConversations(t, e, &rpc.ListConversationsReq{Mid: bob}, "bob 的会话列表")
	if len(bobPage.List) != 1 || bobPage.List[0].ConversationId != p.convID {
		t.Fatalf("bob 看不到自己的会话（对照失败）：%d 条", len(bobPage.List))
	}
}

// TestListConversationsSelfReportedMidLeaksOthersPreviews 钉住列表入口的归属缺口。
//
// 缺陷：services/private-message/internal/logic/listconversationslogic.go:44-47 —— mid 取自请求体，
// 服务侧只做 checkMid(>0)，同文件 :30 的注释把越权边界写成「只按登录者自己的 mid 扫成员表」，
// 但「登录者」是谁没有任何可信来源：
//   - gateway/app/api/app.api:3119 已注明「gateway/app 未配置 go-zero jwt 中间件」；
//   - gateway/app/internal/handler/routes.go:1083 这条 /private-message 路由组只挂了 WithPrefix；
//   - gateway/app/internal/logic/listpmconversationslogic.go:39 直接把 form 参数 req.Mid 转给本服务。
//
// 后果比未读汇总严重：ConversationInfo 带 last_preview（正文脱敏摘要）与 peer_mid，
// 自报 mid 即可以一次一页地读出任意用户的完整通讯录式会话轨迹（和谁聊、最后一条摘要、有没有未读）。
// 修法方向：mid 由网关注入可信身份并与本服务参数强校验；README「越权在网关拦截」的前提交付
// 必须落到 middleware 配置上，否则本服务的 self 域入口全部不成立。
//
// ⚠ 改生产代码前不要动这条断言的期望。
func TestListConversationsSelfReportedMidLeaksOthersPreviews(t *testing.T) {
	e := newEnv(t)
	e.attachSocialGraphOnly(t)
	p := e.seedPair(t, alice, bob)
	// 摘要本身是脱敏过的：这里用一条不含引流形态的正常文本，
	// 才能证明「泄漏的是真实摘要内容」而不是「泄漏了一句占位文案」。
	body := "周六下午三点老地方见，记得带那份合同"
	e.seedMessage(t, p, alice, bob, 1, body, model.MsgStateNormal)

	truth := listConversations(t, e, &rpc.ListConversationsReq{Mid: bob}, "bob 自己的列表")
	if len(truth.List) != 1 {
		t.Fatalf("bob 的列表应有 1 条，实得 %d", len(truth.List))
	}
	// 与 bob 无关的第三方，只是把 mid 填成 bob：拿到的是 bob 的那一行。
	stolen, err := callListConversations(t, e, &rpc.ListConversationsReq{Mid: bob})
	wantOK(t, stolen, err, "自报 mid=bob 读别人列表")
	if len(stolen.List) != len(truth.List) {
		t.Fatalf("自报 mid 拿到的条数与真值不同（%d vs %d）：本用例前提已变化，需同步改期望",
			len(stolen.List), len(truth.List))
	}
	for i, info := range stolen.List {
		if info.ConversationId != truth.List[i].ConversationId || info.PeerMid != alice {
			t.Fatalf("自报 mid 拿到的行与 bob 的真值不一致：%+v", info)
		}
		// 这一条是本用例的核心：对方给的摘要文本原样出境了。
		if !strings.Contains(info.LastPreview, "周六下午") {
			t.Fatalf("摘要内容没有随自报 mid 出境（前提变化，请同步改期望）：%q", info.LastPreview)
		}
	}
	// 反向对照：不报 bob 的 mid 就拿不到他的行（隔离在「行级」是有效的，缺的只是身份校验）。
	other := listConversations(t, e, &rpc.ListConversationsReq{Mid: mallory}, "mallory 的列表")
	if len(other.List) != 0 {
		t.Fatalf("mallory 用自己的 mid 也看到了会话：%d 条（种子前提被破坏）", len(other.List))
	}
}

// TestListConversationsNeverReadsMessageTable 钉住摘要的来源：成员投影列，不回表取正文。
// 「一条消息行都没被读进内存」是本服务里能给出的最强隐私证明（没有读取就没有解密）。
func TestListConversationsNeverReadsMessageTable(t *testing.T) {
	e := newEnv(t)
	sg := e.attachSocialGraphOnly(t)
	sg.follow(alice, bob)
	p := e.seedPair(t, alice, bob)
	e.seedMessage(t, p, alice, bob, 1, "点击查看 www.example.com 领取", model.MsgStateNormal)
	e.seedMessage(t, p, alice, bob, 2, secretBody, model.MsgStateNormal)

	mark := e.markCalls()
	rowsBefore := e.loadedMessageRows()
	page := listConversations(t, e, &rpc.ListConversationsReq{Mid: bob}, "带两条正文的列表")
	if len(page.List) != 1 {
		t.Fatalf("应有 1 条会话，实得 %d", len(page.List))
	}
	if got := e.loadedMessageRows() - rowsBefore; got != 0 {
		t.Fatalf("会话列表把 %d 条消息行读进了内存：摘要必须只来自成员投影列", got)
	}
	requireHandlesRead(t, mark, []string{"Members", "Conversations"}, "会话列表")
	for _, name := range messageRowReaders {
		if n := mark.callsOf(name); n != 0 {
			t.Fatalf("会话列表调了 %s %d 次：读正文的口子不在本入口", name, n)
		}
	}
	info := page.List[0]
	if strings.Contains(info.LastPreview, "example.com") || strings.Contains(info.LastPreview, "SECRET-BODY") {
		t.Fatalf("摘要里出现了原文/域名：%q", info.LastPreview)
	}
	if !strings.Contains(info.LastPreview, "已隐去") {
		t.Fatalf("引流形态没被隐去：%q", info.LastPreview)
	}
	if strings.Contains(e.logs.joined(), secretBody) {
		t.Fatal("日志里出现了消息原文")
	}
	// 最后一条消息的投影必须指向第二条（seq 单调推进），否则端上排序会错。
	last := e.memberRow(t, p.convID, bob)
	if info.LastSeq != last.LastSeq || info.LastMsgId != last.LastMsgID {
		t.Fatalf("列表投影与成员行不符：info=(seq %d,msg %d) row=(%d,%d)",
			info.LastSeq, info.LastMsgId, last.LastSeq, last.LastMsgID)
	}
}

// TestListConversationsQuerySideFilters 钉住四种过滤的精确组合：
// 隐藏（本方）、黑名单（双向）、脏投影（成员行有、会话主体没了）、只看未读。
// 每种过滤都单独占一条会话，因此「少了几条」能直接对上「哪条规则生效了」。
func TestListConversationsQuerySideFilters(t *testing.T) {
	e := newEnv(t)
	sg := e.attachSocialGraphOnly(t)
	hidden := e.seedPair(t, bob, alice) // bob 会隐藏它（有未读）
	blocked := e.seedPair(t, bob, carol)
	dangling := e.seedPair(t, bob, mallory) // 会话主体被删（脏投影）
	read := e.seedPair(t, bob, 404)         // 读过了：未读 0
	for _, p := range []pair{hidden, blocked, dangling, read} {
		e.seedMessage(t, p, p.otherOf(bob), bob, 1, "一条未读", model.MsgStateNormal)
	}
	if err := e.hideConversation(t, hidden.convID, bob, true); err != nil {
		t.Fatalf("bob 隐藏会话：%v", err)
	}
	if mk, err := e.markRead(t, read.convID, bob, 1); err != nil || !mk.Changed {
		t.Fatalf("把对照会话读到末尾：%v changed=%v", err, mk != nil && mk.Changed)
	}
	sg.block(carol, bob) // 对方拉黑我：这一行必须剔除
	delete(e.st.convs, dangling.convID)
	before := e.probe()

	def := listConversations(t, e, &rpc.ListConversationsReq{Mid: bob}, "默认口径")
	if ids := convIDs(def); len(ids) != 1 || ids[0] != read.convID {
		t.Fatalf("默认口径应只剩「已读过」那 1 条（隐藏/拉黑/脏投影各被一条规则剔除），实得 %v", ids)
	}

	// include_hidden 只放开隐藏，不放开黑名单与脏投影。
	withHidden := listConversations(t, e, &rpc.ListConversationsReq{Mid: bob, IncludeHidden: true}, "含隐藏")
	if len(withHidden.List) != 2 {
		t.Fatalf("含隐藏应为 2 条，实得 %v", convIDs(withHidden))
	}
	for _, info := range withHidden.List {
		if info.ConversationId == blocked.convID {
			t.Fatal("被对方拉黑的会话仍出现在列表里（反骚扰门禁被读取路径绕过）")
		}
		if info.ConversationId == dangling.convID {
			t.Fatal("缺会话主体的脏投影仍被外露")
		}
		if info.ConversationId == hidden.convID && !info.Hidden {
			t.Fatal("include_hidden=true 时该行没标 hidden=true，端上无法区分")
		}
		if info.ConversationId == read.convID && info.UnreadCount != 0 {
			t.Fatalf("已读到末尾的会话仍报未读 %d", info.UnreadCount)
		}
	}

	// only_unread 作用在查询层（WHERE unread_count>0）：与 hide 组合后结果不同，四个象限都要对上。
	if ids := convIDs(listConversations(t, e, &rpc.ListConversationsReq{Mid: bob, OnlyUnread: true}, "只看未读")); len(ids) != 0 {
		t.Fatalf("默认口径下「有未读且未隐藏」的行都该被过滤掉，实得 %v", ids)
	}
	both := listConversations(t, e, &rpc.ListConversationsReq{Mid: bob, IncludeHidden: true, OnlyUnread: true}, "含隐藏+只看未读")
	if ids := convIDs(both); len(ids) != 1 || ids[0] != hidden.convID {
		t.Fatalf("含隐藏且只看未读应只剩 hidden 那条，实得 %v", ids)
	}

	// 被丢弃的两行都要留下可定位的日志（只带主键，不带内容），否则线上永远查不出「少了一条」。
	logs := e.logs.joined()
	if !strings.Contains(logs, "成员行缺会话主体") || !strings.Contains(logs, itoa(dangling.convID)) {
		t.Fatalf("脏投影的丢弃没有可定位日志：%q", logs)
	}
	if strings.Contains(logs, "一条未读") {
		t.Fatal("日志里出现了摘要/正文文本")
	}
	e.requireSameRowsAndWrites(t, before, "会话列表（只读）")
}

// TestListConversationsSocialGraphMissingRejectsWholePage 钉住两种「拿不到判定」的分野：
// 未配置 = 整页拒绝（不能把「没接上」当成「已通过」）；下游故障 = 只丢该行。
func TestListConversationsSocialGraphMissingRejectsWholePage(t *testing.T) {
	// 形态一：没有 social-graph 客户端。
	e1 := newEnv(t)
	p1 := e1.seedPair(t, alice, bob)
	e1.seedMessage(t, p1, alice, bob, 1, "有一条数据", model.MsgStateNormal)
	got, err := callListConversations(t, e1, &rpc.ListConversationsReq{Mid: bob})
	wantFail(t, err, model.ErrSocialGraphNotConfigured, "未配置 social-graph")
	if got != nil && len(got.List) != 0 {
		t.Fatalf("整页被拒却还回了一半数据：%v", convIDs(got))
	}
	// 没有可展示行时不需要判定，因此不该因为「没配置」而拒绝空页。
	emptyPage, err := callListConversations(t, e1, &rpc.ListConversationsReq{Mid: mallory})
	wantOK(t, emptyPage, err, "无行时不依赖下游")
	if len(emptyPage.List) != 0 {
		t.Fatalf("mallory 不该有会话：%v", convIDs(emptyPage))
	}

	// 形态二：配置了但故障 —— 宁缺毋滥，丢掉该行而不是整页失败。
	e2 := newEnv(t)
	sg := e2.attachSocialGraphOnly(t)
	p2 := e2.seedPair(t, alice, bob)
	e2.seedMessage(t, p2, alice, bob, 1, "有一条数据", model.MsgStateNormal)
	ok := e2.seedPair(t, bob, carol)
	e2.seedMessage(t, ok, carol, bob, 1, "另一条", model.MsgStateNormal)
	sg.setFailBlack(true)
	page := listConversations(t, e2, &rpc.ListConversationsReq{Mid: bob}, "下游故障时的列表")
	if len(page.List) != 0 {
		t.Fatalf("social-graph 故障时仍给行了 %v：故障判定必须按命中处理（宁可少给）", convIDs(page))
	}
	if page.UnreadTotal != 2 {
		t.Fatalf("对照检查失败：bob 应有 2 条未读，实得 %d", page.UnreadTotal)
	}
	sg.setFailBlack(false)
	if got := len(convIDs(listConversations(t, e2, &rpc.ListConversationsReq{Mid: bob}, "下游恢复后"))); got != 2 {
		t.Fatalf("下游恢复后应回到 2 条，实得 %d（故障注入把库改坏了？）", got)
	}
}

// TestListConversationsDependsOnlyOnMysqlForReadFailures 钉住依赖错误原样上抛。
func TestListConversationsDependsOnlyOnMysqlForReadFailures(t *testing.T) {
	e := newEnv(t)
	sg := e.attachSocialGraphOnly(t)
	p := e.seedPair(t, alice, bob)
	e.seedMessage(t, p, alice, bob, 1, "一条消息", model.MsgStateNormal)

	for _, name := range []string{"Members.ListByMid", "Conversations.FindByIDs", "Members.SumUnread"} {
		e.st.failTx[name] = errInjected
		got, err := callListConversations(t, e, &rpc.ListConversationsReq{Mid: bob})
		wantFail(t, err, errInjected, name+" 故障")
		if got != nil {
			t.Fatalf("%s 故障时仍回了响应体（list %d 条）", name, len(got.List))
		}
		delete(e.st.failTx, name)
	}
	// 注入撤销后必须回到真值：三处注入都没有改写数据。
	page := listConversations(t, e, &rpc.ListConversationsReq{Mid: bob}, "注入撤销后")
	if len(page.List) != 1 || page.UnreadTotal != 1 {
		t.Fatalf("撤销注入后结果异常：%d 条 unread=%d", len(page.List), page.UnreadTotal)
	}
	_ = sg
}

// TestListConversationsUnreadTotalIsMessageTotalNotConversationCount 钉住 unread_total 的两处口径问题。
//
// 缺陷一（契约与实现不符）：listconversationslogic.go:106 —— 取的是
// `Members.SumUnread` 的第一个返回值（未读**消息总数**），而 privatemessage.proto:167
// 对该字段的注释是「该用户未读会话数（过滤后的口径）」。
// 端上若按契约理解，会话列表上的数字会比实际少一个数量级（一条会话里 5 条未读只算 5 而不是 1）。
//
// 缺陷二（不受查询侧过滤约束）：同一行调用没有排除被拉黑/缺主体的行，
// 而它旁边的 list 已经按 README 的「过滤后的口径」筛过一遍 ——
// 于是会出现「列表空的但角标显示有 N 条未读」的自相矛盾（用户点进去看不到东西）。
// 修法方向：二选一 —— 要么把 proto 注释改成「未读消息总数」，
// 要么改成回 SumUnread 的第二个返回值；并且过滤口径与 list 对齐（或在 proto 里明确「不过滤」）。
//
// ⚠ 改生产代码前不要动这两条断言的期望。
func TestListConversationsUnreadTotalIsMessageTotalNotConversationCount(t *testing.T) {
	e := newEnv(t)
	sg := e.attachSocialGraphOnly(t)
	p := e.seedPair(t, alice, bob)
	e.seedMessage(t, p, alice, bob, 1, "第一条未读", model.MsgStateNormal)
	e.seedMessage(t, p, alice, bob, 2, "第二条未读", model.MsgStateNormal)
	e.seedMessage(t, p, alice, bob, 3, "第三条未读", model.MsgStateNormal)
	// 第二个会话一条未读：「有未读的会话数」=2，「未读消息总数」=4。
	p2 := e.seedPair(t, bob, carol)
	e.seedMessage(t, p2, carol, bob, 1, "另一会话的一条", model.MsgStateNormal)

	page := listConversations(t, e, &rpc.ListConversationsReq{Mid: bob}, "两条会话各有未读")
	if len(page.List) != 2 {
		t.Fatalf("应有 2 条会话，实得 %d", len(page.List))
	}
	if page.UnreadTotal != 4 {
		t.Fatalf("unread_total 实得 %d：本用例钉的是「它回的是未读消息总数（proto:167 说的是会话数）」", page.UnreadTotal)
	}
	var perConv int64
	for _, info := range page.List {
		perConv += info.UnreadCount
	}
	if perConv != 4 {
		t.Fatalf("逐行未读之和 %d 与 unread_total %d 不一致（同口径时应相等）", perConv, page.UnreadTotal)
	}

	// 缺陷二：把两条会话都变成「读不到」，总数仍然照报。
	sg.block(alice, bob) // alice 拉黑 bob → bob 看不到与 alice 的会话（2 条未读）
	sg.block(carol, bob) // 同理，另一条也看不到（1 条未读）
	suppressed := listConversations(t, e, &rpc.ListConversationsReq{Mid: bob}, "全部被拉黑后的列表")
	if len(suppressed.List) != 0 {
		t.Fatalf("被拉黑的会话仍出现在列表里：%v", convIDs(suppressed))
	}
	if suppressed.UnreadTotal != 4 {
		t.Fatalf("unread_total 与 list 的过滤口径变了（实得 %d，本用例钉的是「整页被过滤后它仍报 4」）", suppressed.UnreadTotal)
	}
}
