// 本文件钉住本服务最重要的那条不变量：
//
//	参与者授权只有 requireMembership 一个来源，未过授权的调用「零副作用」——
//	不写行、不开事务、不读消息行，因此也不可能解密任何正文。
//
// 两条测法上的讲究：
//  1. 负向用例必须配正向对照（TestMemberCanReadOwnConversationInPlaintext）：
//     否则「越权被拒」可能只是因为整条链路根本跑不通，测不到授权本身；
//  2. 「没解密」不能只靠读代码相信：把库里的密文换成随机字节后，
//     任何解密尝试都会以 ErrDecryptFailed 暴露出来。被拒时错误里只有
//     ErrNotConversationMember ⇒ 解密这一步没被走到（进程内可证的最强形式）。
//     Cache 是具体类型 *redis.Redis，进程内无法观测「缓存键有没有被碰」，
//     键格式与前缀改由 bounds 用例按纯函数钉住。

package logic

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strings"
	"testing"

	"go-video/services/private-message/model"
	"go-video/services/private-message/rpc"
)

// garbleCipher 把一条消息的密文换成非 ASCII 字节序列：真实解密必然失败。
// 用它把「有没有尝试解密」变成一个可断言的错误码。
func (e *env) garbleCipher(t *testing.T, msgID int64) {
	t.Helper()
	e.rawMessage(t, msgID, func(m *model.Message) {
		blob := make([]byte, len(m.ContentCipher)+5)
		for i := range blob {
			blob[i] = byte(i*7 + 3)
		}
		m.ContentCipher = blob
	})
}

// requireRefusedUnauthenticated 断言一次越权调用：
// 错误口径是 target（成员表门禁 → ErrNotConversationMember，快照门禁 → ErrWithdrawForbidden），
// 而且绝不能是 ErrConversationNotFound 或解密失败，错误文本只带主键、没有任何写入/事务。
//
// readsMsgRow=true 用于撤回/举报：这两条按设计先 Messages.FindByID 定位行
// （不查这一行就不知道 conversation_id），因此只断「什么都没写」。
func (e *env) requireRefusedUnauthenticated(t *testing.T, before probe, err error, target error, label string, readsMsgRow bool) {
	t.Helper()
	wantErr(t, err, target, label)
	if errors.Is(err, model.ErrConversationNotFound) {
		t.Fatalf("%s：越权错误区分出了「会话不存在」，等于开放用错误码枚举会话 ID：%v", label, err)
	}
	if errors.Is(err, model.ErrDecryptFailed) || errors.Is(err, ErrCipherBlobTruncated) {
		t.Fatalf("%s：越权调用走到了解密（%v），隐私门禁失效", label, err)
	}
	if strings.Contains(err.Error(), secretBody) {
		t.Fatalf("%s：错误文本泄漏正文：%v", label, err)
	}
	if readsMsgRow {
		e.requireSameRowsAndWrites(t, before, label)
		return
	}
	e.requireRejectedNoSideEffects(t, before, label)
}

// TestNonMemberCallerRejectedWithZeroSideEffects 覆盖所有以 mid 为调用者的读/写入口：
// mallory 与 alice/bob 的会话毫无关系，每条路径都必须在任何数据流出前拒绝。
func TestNonMemberCallerRejectedWithZeroSideEffects(t *testing.T) {
	cases := []struct {
		name        string
		readsMsgRow bool
		// want 是这条路径上「越权」应有的 sentinel：
		// 撤回的用户侧分支先核对消息行上的 sender/receiver 快照，因此 mallory 命中的是
		// ErrWithdrawForbidden；其余入口的越权口径统一是 ErrNotConversationMember。
		want error
		run  func(t *testing.T, e *env, p pair, msgID int64) error
	}{
		{name: "ListMessages", run: func(t *testing.T, e *env, p pair, _ int64) error {
			_, err := e.listMessages(t, p.convID, mallory, 0, 20)
			return err
		}},
		{name: "MarkRead", run: func(t *testing.T, e *env, p pair, _ int64) error {
			_, err := e.markRead(t, p.convID, mallory, 9)
			return err
		}},
		{name: "SendMessage", run: func(t *testing.T, e *env, p pair, _ int64) error {
			// 越权向量是「拿着别人的 conversation_id 往里发」：mallory 知道会话 ID，
			// 但不是那条会话的成员。陌生人按 peer_mid 首聊是另一条被契约允许的路径
			// （见 TestStrangerSendIsGatedByReceiverSetting），不能混在这里当越权样本。
			_, err := e.send(t, &rpc.SendMessageReq{
				Mid: mallory, ConversationId: p.convID, MsgType: rpc.MsgType_MSG_TYPE_TEXT,
				Content: secretBody, ClientMsgId: "mallory-1",
			})
			return err
		}},
		{name: "WithdrawMessage/SENDER", readsMsgRow: true, run: func(t *testing.T, e *env, _ pair, msgID int64) error {
			_, err := e.withdraw(t, msgID, mallory, rpc.WithdrawSource_WITHDRAW_SOURCE_SENDER, "", 0)
			return err
		}, want: model.ErrWithdrawForbidden},
		{name: "WithdrawMessage/RECEIVER", readsMsgRow: true, run: func(t *testing.T, e *env, _ pair, msgID int64) error {
			_, err := e.withdraw(t, msgID, mallory, rpc.WithdrawSource_WITHDRAW_SOURCE_RECEIVER, "", 0)
			return err
		}, want: model.ErrWithdrawForbidden},
		{name: "WithdrawMessage/MODERATION 无对账凭据", run: func(t *testing.T, e *env, _ pair, msgID int64) error {
			_, err := e.withdraw(t, msgID, mallory, rpc.WithdrawSource_WITHDRAW_SOURCE_MODERATION, "", 0)
			return err
		}, want: model.ErrWithdrawForbidden},
		{name: "ReportMessage", readsMsgRow: true, run: func(t *testing.T, e *env, _ pair, msgID int64) error {
			_, err := e.report(t, msgID, mallory, 1, "harassment")
			return err
		}},
		{name: "HideConversation", run: func(t *testing.T, e *env, p pair, _ int64) error {
			return e.hideConversation(t, p.convID, mallory, true)
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.attachSocialGraphOnly(t)
			p := e.seedPair(t, alice, bob)
			msg := e.seedMessage(t, p, alice, bob, 1, secretBody, model.MsgStateNormal)
			// 先把密文打坏再让 mallory 尝试：若实现顺序是「先解密再判权限」，
			// 错误会变成 ErrDecryptFailed 而不是 ErrNotConversationMember。
			e.garbleCipher(t, msg.MsgID)

			before := e.probe()
			err := tc.run(t, e, p, msg.MsgID)
			target := tc.want
			if target == nil {
				target = model.ErrNotConversationMember
			}
			e.requireRefusedUnauthenticated(t, before, err, target, tc.name, tc.readsMsgRow)
		})
	}
}

// TestRejectedCallsDidAskTheMemberTable 是上一条的补充：
// 「被拒」必须来自成员表查询失败，而不是某个更靠前的参数校验顺手挡掉的——
// 那样等于没测到授权门禁。每条路径都必须至少查一次 Members.Find
// （发送侧与读侧同用 requireMembership，定位方式不同不等于授权门槛不同）。
func TestRejectedCallsDidAskTheMemberTable(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, e *env, p pair, msgID int64) error
	}{
		{name: "ListMessages", run: func(t *testing.T, e *env, p pair, _ int64) error {
			_, err := e.listMessages(t, p.convID, mallory, 0, 20)
			return err
		}},
		{name: "MarkRead", run: func(t *testing.T, e *env, p pair, _ int64) error {
			_, err := e.markRead(t, p.convID, mallory, 9)
			return err
		}},
		{name: "ReportMessage", run: func(t *testing.T, e *env, _ pair, msgID int64) error {
			_, err := e.report(t, msgID, mallory, 1, "harassment")
			return err
		}},
		{name: "SendMessage", run: func(t *testing.T, e *env, p pair, _ int64) error {
			_, err := e.send(t, &rpc.SendMessageReq{
				Mid: mallory, ConversationId: p.convID, MsgType: rpc.MsgType_MSG_TYPE_TEXT,
				Content: secretBody, ClientMsgId: "mallory-2",
			})
			return err
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			sg := e.attachSocialGraphOnly(t)
			p := e.seedPair(t, alice, bob)
			msg := e.seedMessage(t, p, alice, bob, 1, secretBody, model.MsgStateNormal)
			blackBefore := sg.blackCalls()
			before := e.probe()
			err := tc.run(t, e, p, msg.MsgID)
			wantErr(t, err, model.ErrNotConversationMember, tc.name)
			e.requireMembershipProved(t, before, tc.name)
			// 授权先于关系判定：不是成员就不该去问 social-graph（否则越权者可打下游 QPS）。
			if got := sg.blackCalls() - blackBefore; got != 0 {
				t.Fatalf("%s：越权调用扇出了 %d 次 social-graph 请求", tc.name, got)
			}
		})
	}
}

// TestSendMessageSelfPairIsNotMember 钉住「给自己发私信」：
// peer==sender 时成员行不存在（建档时只写两行不同 mid 的投影），必须按非成员拒绝，
// 而不是退化成「会话不存在」，也不是退化成 ErrSelfConversation——
// 成员关系只有成员表一个来源，用独立错误码回答「自聊」就是把
// 「这一对我有没有成员行」这件事编码进了错误分支里。
func TestSendMessageSelfPairIsNotMember(t *testing.T) {
	e := newEnv(t)
	before := e.probe()
	_, err := e.send(t, &rpc.SendMessageReq{
		Mid: alice, PeerMid: alice, MsgType: rpc.MsgType_MSG_TYPE_TEXT,
		Content: secretBody, ClientMsgId: "self-1",
	})
	wantErr(t, err, model.ErrNotConversationMember, "给自己发私信")
	if errors.Is(err, model.ErrSelfConversation) {
		t.Fatalf("自聊退化成了建档语义的错误码（%v），与发送侧的成员口径不一致", err)
	}
	e.requireRejectedNoSideEffects(t, before, "给自己发私信")
}

// TestStrangerSendIsGatedByReceiverSetting 钉住「陌生人首聊」这一条被契约允许的路径：
// conversation_id=0 时按 peer_mid 定位/建档（rpc/privatemessage.proto:186-187
// 「conversation_id 为 0 时按 peer_mid 建会话」），此刻成员表里还没有行可证，
// 所以放行与否由「新会话日配额 + 接收方 allow_from / reject_stranger 门槛」决定
// （README 发送门禁顺序里紧接「会话定位」的那一步）。
//
// 它与越权用例的分工必须清楚：跨会话写入走 conversation_id（见上面两条用例），
// 首聊走 peer_mid（这一条）。两支都要求零副作用，但被拒的理由不同 ——
// 把首聊也当成越权，等于删掉「陌生人门槛」这整段契约；
// 反过来只报 peer_mid 就能对既有会话写入，则是绕过成员表（见 TestSendMessagePairPathMustProveMembershipToo）。
func TestStrangerSendIsGatedByReceiverSetting(t *testing.T) {
	e := newEnv(t)
	d := e.attachDownstream(t)
	p := e.seedPair(t, alice, bob)
	e.seedMessage(t, p, alice, bob, 1, secretBody, model.MsgStateNormal)

	// ① 单向关注仍不够：站点缺省 RejectStrangerByDefault=true 要求互关。
	d.sg.follow(alice, mallory)
	before := e.probe()
	_, err := e.sendText(t, mallory, alice, "stranger-1", secretBody)
	wantErr(t, err, model.ErrBlockedByPeer, "非互关陌生人首聊")
	if errors.Is(err, model.ErrNotConversationMember) {
		t.Fatalf("首聊被当成越权拒绝（%v），陌生人永远建不出会话：%v", model.ErrNotConversationMember, err)
	}
	if d.sg.blackCalls() == 0 {
		t.Fatal("首聊路径没扇出 social-graph 判定：接收方门槛根本没被执行")
	}
	e.requireRejectedNoSideEffects(t, before, "非互关陌生人首聊")

	// ② 互关后同一份入参真的建得出会话：成员行由发送事务里的 Ensure 产生，
	//    这一步不查成员表也没查过任何消息行 —— 它是「peer_mid 建档」被契约允许的证据。
	d.sg.follow(mallory, alice)
	reply, err := e.sendText(t, mallory, alice, "stranger-2", "在吗")
	wantOK(t, reply, err, "互关后的首聊")
	if reply.GetConversationId() == 0 || reply.GetMsgId() == 0 || reply.GetReplayed() {
		t.Fatalf("首聊没落成一条新消息：%+v", reply)
	}
	view, err := e.listMessages(t, reply.GetConversationId(), alice, 0, 20)
	wantOK(t, view, err, "接收方视角的首聊")
	if len(view.List) != 1 || view.List[0].GetContent() != "在吗" {
		t.Fatalf("首条消息未对接收方可见：%+v", view.List)
	}
	// 建档产出的两行成员投影 mid 必然不同（这正是「自聊」在成员表里永远无解的原因）。
	malloryRow := e.memberRow(t, reply.GetConversationId(), mallory)
	aliceRow := e.memberRow(t, reply.GetConversationId(), alice)
	if malloryRow.PeerMid != alice || aliceRow.PeerMid != mallory {
		t.Fatalf("成员投影的对方 mid 异常：%d/%d", malloryRow.PeerMid, aliceRow.PeerMid)
	}
}

// TestSendMessagePairPathMustProveMembershipToo 钉住发送侧那条真实的越权面：
// peer_mid 建档被契约允许，但 pair_key 命中「既有会话」时也必须与显式 conversation_id
// 过同一份成员证明。修复前这一支只做 peerOf(conv, sender, 0) 取接收方，
// 成员行被删（退出会话/数据修复后最可能的形态）也照样往里写，
// 等于「把 conversation_id 换成 peer_mid」就绕过了成员表。
func TestSendMessagePairPathMustProveMembershipToo(t *testing.T) {
	e := newEnv(t)
	d := e.attachDownstream(t)
	d.sg.follow(alice, mallory)
	d.sg.follow(mallory, alice)
	own := e.seedPair(t, alice, mallory)
	e.seedMessage(t, own, mallory, alice, 1, "他们俩的话", model.MsgStateNormal)

	// 正向对照：成员行在，peer_mid 这条路继续发到同一条会话，接收方由会话主体决定。
	before := e.probe()
	reply, err := e.sendText(t, mallory, alice, "pair-1", "还在吗")
	wantOK(t, reply, err, "既有 pair 上继续发送")
	if reply.GetConversationId() != own.convID {
		t.Fatalf("peer_mid 定位漂到了别的会话：%d != %d", reply.GetConversationId(), own.convID)
	}
	if got := e.msgRow(t, reply.GetMsgId()); got.ReceiverMid != alice {
		t.Fatalf("接收方不由会话主体决定：%+v", got)
	}
	e.requireMembershipProved(t, before, "peer_mid 命中既有会话")

	// 越权：mallory 的成员行没了（会话主体还在），只报 peer_mid 也必须按非成员拒绝。
	delete(e.st.members, memberKey(own.convID, mallory))
	before = e.probe()
	_, err = e.sendText(t, mallory, alice, "pair-2", "偷偷再发一条")
	wantErr(t, err, model.ErrNotConversationMember, "成员行已失效的 pair 发送")
	e.requireRejectedNoSideEffects(t, before, "成员行已失效的 pair 发送")
}

// TestMemberOfAnotherConversationHasNoAccess 是比「完全无关用户」更强的一档越权：
// mallory 与 alice 之间有自己的会话，因此「他是个有成员行的真实用户」，
// 但他对 alice 与 bob 之间那条会话依然什么都没有。
func TestMemberOfAnotherConversationHasNoAccess(t *testing.T) {
	e := newEnv(t)
	sg := e.attachSocialGraphOnly(t)
	sg.follow(alice, mallory)
	sg.follow(mallory, alice)
	own := e.seedPair(t, alice, mallory)
	e.seedMessage(t, own, alice, mallory, 1, "他们俩的话", model.MsgStateNormal)

	victim := e.seedPair(t, alice, bob)
	msg := e.seedMessage(t, victim, alice, bob, 1, secretBody, model.MsgStateNormal)

	before := e.probe()
	_, err := e.listMessages(t, victim.convID, mallory, 0, 20)
	wantErr(t, err, model.ErrNotConversationMember, "跨会话读取")
	e.requireMembershipProved(t, before, "跨会话读取")

	// 举报同理：他知道 msg_id、也是真实用户，但那条不属于他的会话。
	before = e.probe()
	_, err = e.report(t, msg.MsgID, mallory, 1, "harassment")
	wantErr(t, err, model.ErrNotConversationMember, "跨会话举报")
	e.requireMembershipProved(t, before, "跨会话举报")
	e.requireSameRowsAndWrites(t, before, "跨会话举报")

	before = e.probe()
	err = e.hideConversation(t, victim.convID, mallory, true)
	wantErr(t, err, model.ErrNotConversationMember, "跨会话隐藏")
	e.requireSameRowsAndWrites(t, before, "跨会话隐藏")

	// 他只能读到自己那条会话。
	reply, err := e.listMessages(t, own.convID, mallory, 0, 20)
	wantOK(t, reply, err, "本会话仍可读")
	if len(reply.List) != 1 || reply.List[0].GetContent() != "他们俩的话" {
		t.Fatalf("本会话读取结果异常：%+v", reply.List)
	}
}

// TestMemberRowWithoutConversationBodyIsNotMember 造「成员行在、会话主体不在」的脏数据：
// 必须按「不是成员」拒绝，而不是把内部不一致暴露成 ErrConversationNotFound。
func TestMemberRowWithoutConversationBodyIsNotMember(t *testing.T) {
	e := newEnv(t)
	p := e.seedPair(t, alice, bob)
	msg := e.seedMessage(t, p, alice, bob, 1, secretBody, model.MsgStateNormal)
	delete(e.st.convs, p.convID)

	before := e.probe()
	_, err := e.listMessages(t, p.convID, alice, 0, 20)
	e.requireRefusedUnauthenticated(t, before, err, model.ErrNotConversationMember, "缺会话主体的成员行", false)
	e.requireMembershipProved(t, before, "缺会话主体的成员行")

	before = e.probe()
	_, err = e.markRead(t, p.convID, alice, 1)
	e.requireRefusedUnauthenticated(t, before, err, model.ErrNotConversationMember, "缺会话主体时前移游标", false)

	before = e.probe()
	_, err = e.withdraw(t, msg.MsgID, alice, rpc.WithdrawSource_WITHDRAW_SOURCE_SENDER, "", 0)
	e.requireRefusedUnauthenticated(t, before, err, model.ErrNotConversationMember, "缺会话主体时撤回", true)
	e.requireMembershipProved(t, before, "缺会话主体时撤回")
}

// TestNotMemberErrorCarriesOnlyIDs 钉住错误文本的信息量：主键可以有，正文不能有。
func TestNotMemberErrorCarriesOnlyIDs(t *testing.T) {
	e := newEnv(t)
	_, err := requireMembership(bg(), e.svc, 5001, mallory)
	if err == nil {
		t.Fatal("预期拒绝")
	}
	text := err.Error()
	for _, want := range []string{"conversation_id=5001", "mid=909"} {
		if !strings.Contains(text, want) {
			t.Fatalf("错误文本缺少可诊断字段 %s：%s", want, text)
		}
	}
	for _, leak := range []string{secretBody, "cipher", "content=", "preview="} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(leak)) {
			t.Fatalf("错误文本含敏感字段 %s：%s", leak, text)
		}
	}
	if !strings.Contains(text, model.ErrNotConversationMember.Error()) {
		t.Fatalf("错误文本未回灌 sentinel：%s", text)
	}
}

// TestMessageNotFoundDoesNotRevealConversation 钉住「消息不存在」的口径：
// 只回 ErrMessageNotFound，不回任何会话/成员信息（否则 msg_id 成了反查会话的入口）。
func TestMessageNotFoundDoesNotRevealConversation(t *testing.T) {
	e := newEnv(t)
	e.attachSocialGraphOnly(t)
	before := e.probe()
	_, err := e.report(t, 424242, mallory, 1, "x")
	wantErr(t, err, model.ErrMessageNotFound, "举报不存在的消息")
	if strings.Contains(err.Error(), "conversation_id") {
		t.Fatalf("「消息不存在」的错误泄漏了会话 ID：%v", err)
	}
	e.requireRejectedNoSideEffects(t, before, "举报不存在的消息")

	_, err = e.withdraw(t, 424242, alice, rpc.WithdrawSource_WITHDRAW_SOURCE_ADMIN, "", 0)
	wantErr(t, err, model.ErrMessageNotFound, "撤回不存在的消息")
	_, err = e.verdict(t, 424242, 555, rpc.ModerationVerdict_VERDICT_PASS, "evt-1", "r")
	wantErr(t, err, model.ErrMessageNotFound, "回写不存在消息的结论")
}

// TestOperatorAndSystemSubjectGates 钉住运营/系统侧入口的主体门禁：
// 「机器主体也会调用」的入口（审核结论回写、留存清理）允许 proto 里那个 0
// （ApplyModerationVerdictReq.operator「处理人（0 表示机审）」、
// PurgeExpiredMessagesReq.operator「触发者（cron 传 0）」），
// 但允许 0 绝不等于不看这个字段：负数既不是自然人也不是系统，一律按缺主体拒。
// 纯运营入口（举报处置）没有系统档，0 就是不合法。
//
// 这条用例存在的理由：主体校验一旦被当成「调用方自觉」，清理与改判这两条
// 不可逆/影响可见性的写就成了无主写入，事后无法归因（AGENTS.md §8）。
func TestOperatorAndSystemSubjectGates(t *testing.T) {
	cases := []struct {
		name string
		// allowSystem：该入口的契约取值里 0 是合法主体（cron/机审）。
		allowSystem bool
		// pastGate 是「主体已过关」后落到的数据层结果（nil 表示这一步就成功）。
		pastGate error
		run      func(t *testing.T, e *env, subject int64) error
	}{
		{name: "ApplyModerationVerdict", allowSystem: true, pastGate: model.ErrMessageNotFound,
			run: func(t *testing.T, e *env, subject int64) error {
				_, err := NewApplyModerationVerdictLogic(bg(), e.svc).ApplyModerationVerdict(&rpc.ApplyModerationVerdictReq{
					MsgId: 424242, TaskId: 555, Verdict: rpc.ModerationVerdict_VERDICT_PASS,
					EventId: "evt-gate", Operator: subject,
				})
				return err
			}},
		{name: "PurgeExpiredMessages", allowSystem: true,
			run: func(t *testing.T, e *env, subject int64) error {
				// dry_run：只统计不删，主体校验必须发生在扫描之前。
				_, err := NewPurgeExpiredMessagesLogic(bg(), e.svc).PurgeExpiredMessages(&rpc.PurgeExpiredMessagesReq{
					DryRun: true, Operator: subject,
				})
				return err
			}},
		{name: "HandleReport", pastGate: model.ErrReportNotFound,
			run: func(t *testing.T, e *env, subject int64) error {
				_, err := NewHandleReportLogic(bg(), e.svc).HandleReport(&rpc.HandleReportReq{
					ReportId: 424242, Handler: subject, Action: rpc.ReportAction_REPORT_ACTION_DISMISS,
					IdempotencyKey: "gate-key",
				})
				return err
			}},
	}
	seeded := func(t *testing.T) *env {
		t.Helper()
		e := newEnv(t)
		e.attachSocialGraphOnly(t)
		p := e.seedPair(t, alice, bob)
		e.seedMessage(t, p, alice, bob, 1, secretBody, model.MsgStateNormal)
		return e
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// ① 负主体：缺主体，且门禁在任何数据访问之前。
			for _, subject := range []int64{-1, -424242} {
				e := seeded(t)
				before := e.probe()
				err := tc.run(t, e, subject)
				label := fmt.Sprintf("%s operator=%d", tc.name, subject)
				wantFail(t, err, model.ErrOperatorRequired, label)
				e.requireRejectedNoSideEffects(t, before, label)
			}
			// ② 0 与正管理员：0 只对系统侧入口成立；过关后一律落到数据层结果。
			for _, subject := range []int64{0, 7} {
				e := seeded(t)
				label := fmt.Sprintf("%s operator=%d", tc.name, subject)
				err := tc.run(t, e, subject)
				if subject == 0 && !tc.allowSystem {
					wantFail(t, err, model.ErrOperatorRequired, label+"（该入口没有系统主体档）")
					continue
				}
				if errors.Is(err, model.ErrOperatorRequired) {
					t.Fatalf("%s：合法主体被当成缺主体拒绝：%v", label, err)
				}
				if tc.pastGate == nil {
					if err != nil {
						t.Fatalf("%s：预期过关，实得 %v", label, err)
					}
					continue
				}
				wantErr(t, err, tc.pastGate, label+"（主体已过门禁，剩下的应是数据层结论）")
			}
		})
	}
}

// TestWithdrawSnapshotsDoNotReplaceTheMemberTable 是本服务授权模型的那条关键区分：
// 消息行上的 sender_mid/receiver_mid 只是快照，撤回仍必须过成员表。
// 造「快照说他是发送者、成员表里没有他」的现场（数据修复/迁移后最可能出现），
// 结论必须是 ErrNotConversationMember，而不是「快照优先」放行。
func TestWithdrawSnapshotsDoNotReplaceTheMemberTable(t *testing.T) {
	e := newEnv(t)
	p := e.seedPair(t, alice, bob)
	msg := e.seedMessage(t, p, alice, bob, 1, secretBody, model.MsgStateNormal)
	delete(e.st.members, memberKey(p.convID, alice))

	e.garbleCipher(t, msg.MsgID)
	before := e.probe()
	_, err := e.withdraw(t, msg.MsgID, alice, rpc.WithdrawSource_WITHDRAW_SOURCE_SENDER, "", 0)
	e.requireRefusedUnauthenticated(t, before, err, model.ErrNotConversationMember, "快照优先于成员表", true)
	e.requireMembershipProved(t, before, "快照优先于成员表")

	// 同一份现场下 ADMIN 分支不受影响：运营侧靠 checkOperator 而不是成员表，
	// 但它同样没有任何读正文的能力（见枚举用例）。
	if _, adminErr := e.withdraw(t, msg.MsgID, alice, rpc.WithdrawSource_WITHDRAW_SOURCE_ADMIN, "风控处置", 0); adminErr != nil {
		t.Fatalf("ADMIN 撤回不该依赖成员行：%v", adminErr)
	}
}

// TestPendingReviewInvisibleToPeer 钉住「机审结论前回自己」这条：
// 同一行对发送者返回明文，对接收方整行丢弃（不是返回占位文案）。
func TestPendingReviewInvisibleToPeer(t *testing.T) {
	e := newEnv(t)
	e.attachSocialGraphOnly(t)
	p := e.seedPair(t, alice, bob)
	e.seedMessage(t, p, alice, bob, 1, secretBody, model.MsgStatePendingReview)
	e.seedMessage(t, p, alice, bob, 2, "正常的一条", model.MsgStateNormal)

	senderView, err := e.listMessages(t, p.convID, alice, 0, 20)
	wantOK(t, senderView, err, "发送者视角")
	if len(senderView.List) != 2 {
		t.Fatalf("发送者应看到 2 条（含待审），实得 %d", len(senderView.List))
	}
	if senderView.List[0].GetSeq() != 2 {
		t.Fatalf("首页应按 seq 倒序，实得第一条 seq=%d", senderView.List[0].GetSeq())
	}
	if got := senderView.List[1].GetContent(); got != secretBody {
		t.Fatalf("待审消息对发送者应回明文，实得 %q", got)
	}

	peerView, err := e.listMessages(t, p.convID, bob, 0, 20)
	wantOK(t, peerView, err, "接收方视角")
	if len(peerView.List) != 1 || peerView.List[0].GetSeq() != 2 {
		t.Fatalf("待审消息泄漏给接收方：%+v", peerView.List)
	}
	if peerView.HasMore || peerView.NextCursorSeq != 0 {
		t.Fatalf("一页装得下时不该给游标：%d/%v", peerView.NextCursorSeq, peerView.HasMore)
	}
}

// TestWithdrawnRowNeverLeaksPlaintextOrMediaRef 钉住终态行的投影：
// 状态位换占位文案，media_ref 同步隐去（留着 asset 引用等于绕过撤回继续拉媒体）。
func TestWithdrawnRowNeverLeaksPlaintextOrMediaRef(t *testing.T) {
	e := newEnv(t)
	e.attachSocialGraphOnly(t)
	p := e.seedPair(t, alice, bob)
	msg := e.seedMessage(t, p, alice, bob, 1, secretBody, model.MsgStateNormal)
	e.rawMessage(t, msg.MsgID, func(m *model.Message) {
		m.State = model.MsgStateWithdrawn
		m.MediaRef = "asset-9001"
		m.WithdrawTime = m.Ctime
	})

	for _, viewer := range []int64{alice, bob} {
		reply, err := e.listMessages(t, p.convID, viewer, 0, 20)
		wantOK(t, reply, err, "已撤回行投影")
		if len(reply.List) != 1 {
			t.Fatalf("已撤回行应仍在列表里（占位），实得 %d 条", len(reply.List))
		}
		got := reply.List[0]
		if got.GetContent() != contentWithdrawn {
			t.Fatalf("已撤回消息 content=%q，期望占位 %q", got.GetContent(), contentWithdrawn)
		}
		if got.GetMediaRef() != "" {
			t.Fatalf("已撤回消息仍回传 media_ref=%q", got.GetMediaRef())
		}
		if got.GetState() != model.MsgStateWithdrawn {
			t.Fatalf("状态位应原样回传，实得 %d", got.GetState())
		}
	}
}

// TestPurgedRowKeepsMetadataButLosesContent 钉住留存到期后的形态：
// 行、seq、审计还在，正文换成占位文案——「内容已按留存策略删除」而不是故障。
func TestPurgedRowKeepsMetadataButLosesContent(t *testing.T) {
	e := newEnv(t)
	e.attachSocialGraphOnly(t)
	p := e.seedPair(t, alice, bob)
	msg := e.seedMessage(t, p, alice, bob, 1, secretBody, model.MsgStateNormal)
	e.rawMessage(t, msg.MsgID, func(m *model.Message) {
		m.ContentCipher = nil
		m.KeyVersion = 0
		m.MediaRef = ""
		m.ContentPurged = model.ContentPurgedYes
	})

	for _, viewer := range []int64{alice, bob} {
		reply, err := e.listMessages(t, p.convID, viewer, 0, 20)
		wantOK(t, reply, err, "已清理行投影")
		if len(reply.List) != 1 {
			t.Fatalf("清理后行还在，实得 %d 条", len(reply.List))
		}
		if got := reply.List[0].GetContent(); got != contentPurged {
			t.Fatalf("content=%q，期望 %q", got, contentPurged)
		}
		if reply.List[0].GetSeq() != 1 {
			t.Fatalf("清理不应影响 seq：%d", reply.List[0].GetSeq())
		}
	}
}

// TestHideConversationAffectsOnlyCallersOwnRow 是本服务唯一「不用 requireMembership 的写」：
// 授权靠 UPDATE 的 WHERE (conversation_id, mid) 本身。越权改不到对方的投影，
// 正常隐藏也只改自己一行，且绝不删消息。
func TestHideConversationAffectsOnlyCallersOwnRow(t *testing.T) {
	e := newEnv(t)
	sg := e.attachSocialGraphOnly(t)
	sg.follow(alice, bob)
	sg.follow(bob, alice)
	p := e.seedPair(t, alice, bob)
	msg := e.seedMessage(t, p, alice, bob, 1, secretBody, model.MsgStateNormal)

	before := e.probe()
	err := e.hideConversation(t, p.convID, mallory, true)
	e.requireRefusedUnauthenticated(t, before, err, model.ErrNotConversationMember, "非成员隐藏会话", false)

	if got := e.memberRow(t, p.convID, alice).HideState; got != model.HideStateNormal {
		t.Fatalf("mallory 的越权调用改了 alice 的投影行：hide_state=%d", got)
	}
	if err := e.hideConversation(t, p.convID, alice, true); err != nil {
		t.Fatalf("本方隐藏会话：%v", err)
	}
	if got := e.memberRow(t, p.convID, bob).HideState; got != model.HideStateNormal {
		t.Fatalf("隐藏本方会话把对方的投影也改了：hide_state=%d", got)
	}
	reply, err := e.listMessages(t, p.convID, bob, 0, 20)
	wantOK(t, reply, err, "对方在隐藏后仍能读会话")
	if len(reply.List) != 1 {
		t.Fatalf("对方看到的行数为 %d", len(reply.List))
	}
	if _, ok := e.st.msgRow(msg.MsgID); !ok {
		t.Fatal("隐藏会话删掉了消息行")
	}
}

// TestBlockedRelationReadsAsEmptyPage 钉住读取侧的反骚扰口径：
// 命中拉黑时整页按空返回（而不是留一堆占位行），并且完全不查消息表。
func TestBlockedRelationReadsAsEmptyPage(t *testing.T) {
	cases := []struct {
		name string
		// blocking 返回「谁拉黑谁」：viewer 侧或对方侧命中都应空页。
		blockedBy, blocks int64
	}{
		{name: "对方拉黑我", blockedBy: bob, blocks: alice},
		{name: "我拉黑对方", blockedBy: alice, blocks: bob},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			sg := e.attachSocialGraphOnly(t)
			sg.block(tc.blockedBy, tc.blocks)
			p := e.seedPair(t, alice, bob)
			e.seedMessage(t, p, alice, bob, 1, secretBody, model.MsgStateNormal)

			before := e.probe()
			reply, err := e.listMessages(t, p.convID, alice, 0, 20)
			wantOK(t, reply, err, "拉黑命中时的读取")
			if len(reply.List) != 0 {
				t.Fatalf("拉黑命中仍返回 %d 条正文", len(reply.List))
			}
			if reply.ReadSeq != e.memberRow(t, p.convID, alice).ReadSeq {
				t.Fatalf("ReadSeq 回传错值：%d", reply.ReadSeq)
			}
			if got := e.loadedMessageRows() - before.msgRows; got != 0 {
				t.Fatalf("拉黑命中后仍取了 %d 条消息行", got)
			}
			e.requireSameRowsAndWrites(t, before, "拉黑命中读取")
		})
	}
}

// TestSocialGraphFailureReadsAsEmptyPageNotAsVisible 钉住「拿不到判定 ≠ 全部可见」：
// social-graph 故障时这一页按命中拉黑处理（宁可不给看），而不是放行整页正文；
// 未配置时更是直接报 ErrSocialGraphNotConfigured。
func TestSocialGraphFailureReadsAsEmptyPageNotAsVisible(t *testing.T) {
	e := newEnv(t)
	sg := e.attachSocialGraphOnly(t)
	sg.setFailBlack(true)
	p := e.seedPair(t, alice, bob)
	e.seedMessage(t, p, alice, bob, 1, secretBody, model.MsgStateNormal)

	reply, err := e.listMessages(t, p.convID, alice, 0, 20)
	wantOK(t, reply, err, "下游故障时的读取")
	if len(reply.List) != 0 {
		t.Fatalf("下游故障时放行了 %d 条正文", len(reply.List))
	}

	e2 := newEnv(t)
	p2 := e2.seedPair(t, alice, bob)
	e2.seedMessage(t, p2, alice, bob, 1, secretBody, model.MsgStateNormal)
	_, err = e2.listMessages(t, p2.convID, alice, 0, 20)
	wantErr(t, err, model.ErrSocialGraphNotConfigured, "未配置 social-graph 时的读取")
}

// --- 授权面枚举（对源码 AST 做穷尽，而不是对某几个用例穷尽） ---

// logicEntry 是一个对外 logic 入口的授权契约。
//
// scope 取值：
//   - member   ：必须有成员证明（Members.Find 命中），且明文出口只属于 ListMessages
//   - self     ：只能操作调用者自己的数据（不得触达会话/消息表）
//   - operator ：运营/系统侧（checkOperator 必需），绝不触达明文出口
//   - pair     ：按 (mid, peer) 建档（checkPair 必需）
//
// accessors 是钉死的「允许触达的数据接口」集合：出现集合之外的接口就是新增越权面，
// 必须显式在此登记并由读者判断是否合理（超集会被测出来）。
// inTx 列出「必须在事务内发生的写」——跨表写必须一次提交（AGENTS.md §8）。
type logicEntry struct {
	scope     string
	auth      []string
	accessors []string
	inTx      []string
	plaintext bool
}

var logicContracts = map[string]logicEntry{
	"GetOrCreateConversation": {scope: "pair", auth: []string{"checkPair"}, accessors: []string{
		"Conversations.FindOrCreateInTx", "Members.Ensure",
	}},
	"SendMessage": {scope: "member", auth: []string{"requireMembership"}, accessors: []string{
		"Messages.FindByClientMsgID",
		// 会话定位：两条定位路径共用同一份成员证明（requireMembership）。
		"Conversations.FindByPairKey", "Members.Find", "Conversations.FindByID",
		// 反骚扰门禁：接收方偏好与「这个发送者在本会话内有没有历史」。
		"Settings.FindByMid", "Messages.HasAnyFromSender",
		"Conversations.FindOrCreateInTx", "Conversations.AllocateSeq", "Conversations.TouchLastMessage",
		"Members.Ensure", "Messages.Insert", "Members.ApplyIncoming",
		// 提交后送审的审计回写（事务外，且只在机审开启时发生）。
		"Messages.BindAuditTask",
	}, inTx: []string{
		"Conversations.FindOrCreateInTx", "Conversations.AllocateSeq", "Conversations.TouchLastMessage",
		"Members.Ensure", "Messages.Insert", "Members.ApplyIncoming",
	}},
	"ListConversations": {scope: "self", accessors: []string{
		"Members.ListByMid", "Conversations.FindByIDs", "Members.SumUnread",
	}},
	"ListMessages": {scope: "member", auth: []string{"requireMembership"}, accessors: []string{
		"Members.Find", "Conversations.FindByID", "Messages.ListBeforeSeq",
	}, plaintext: true},
	"MarkRead": {scope: "member", auth: []string{"requireMembership"}, accessors: []string{
		"Members.Find", "Conversations.FindByID", "Members.MarkRead",
	}},
	"GetUnreadSummary": {scope: "self", accessors: []string{"Members.SumUnread"}},
	"WithdrawMessage": {scope: "member", auth: []string{"requireMembership"}, accessors: []string{
		"Messages.FindByID", "Members.Find", "Conversations.FindByID",
		"Messages.MarkState", "WithdrawLogs.Insert", "Members.DecrementUnreadIfUnread", "Members.RefreshPreview",
	}, inTx: []string{
		"Messages.MarkState", "WithdrawLogs.Insert", "Members.DecrementUnreadIfUnread", "Members.RefreshPreview",
	}},
	"HideConversation":  {scope: "self", accessors: []string{"Members.SetHidden"}},
	"UpdateUserSetting": {scope: "self", accessors: []string{"Settings.Upsert", "Settings.FindByMid"}},
	"GetUserSetting":    {scope: "self", accessors: []string{"Settings.FindByMid"}},
	"ReportMessage": {scope: "member", auth: []string{"requireMembership"}, accessors: []string{
		"Messages.FindByID", "Members.Find", "Conversations.FindByID",
		"Reports.Insert", "Reports.FindByID", "Reports.BindAuditTask",
	}},
	"ListReports": {scope: "operator", auth: []string{"checkOperator"}, accessors: []string{
		"Reports.ListByCursor",
	}},
	"HandleReport": {scope: "operator", auth: []string{"checkOperator"}, accessors: []string{
		"Reports.FindByHandleKey", "Reports.FindByID", "Reports.MarkHandledInTx", "Reports.BindAuditTask",
		"Messages.FindByID", "Messages.MarkState", "WithdrawLogs.Insert",
		"Members.DecrementUnreadIfUnread", "Members.RefreshPreview",
	}, inTx: []string{
		// 处置与连带撤回必须一次提交：这四步都发生在 withdrawInTx / MarkHandledInTx 里，
		// 而它们只能从 TransactCtx 闭包调用（由下面的事务步骤调用点检查保证）。
		"Reports.MarkHandledInTx", "Messages.MarkState", "WithdrawLogs.Insert",
		"Members.DecrementUnreadIfUnread", "Members.RefreshPreview",
	}},
	"ApplyModerationVerdict": {scope: "operator", auth: []string{"checkSubjectOrOperator"}, accessors: []string{
		"Messages.FindByID", "Messages.MarkState", "Messages.BindAuditEvent",
		// REJECT 是处置动作：状态位、审计流水、未读与摘要投影四件事同一事务。
		"WithdrawLogs.Insert", "Members.DecrementUnreadIfUnread", "Members.RefreshPreview",
	}, inTx: []string{
		"Messages.MarkState", "WithdrawLogs.Insert",
		"Members.DecrementUnreadIfUnread", "Members.RefreshPreview",
	}},
	"PurgeExpiredMessages": {scope: "operator", auth: []string{"checkOperator"}, accessors: []string{
		"Messages.CountPurgeCandidates", "Messages.ListPurgeCandidates", "Messages.PurgeByIDs",
	}},
}

// dataHandleIdents 是 ServiceContext 上真正指向 pm_* 表的句柄名。
// 只有挂在这些句柄上的方法调用才算「数据接口」，从而把 logx/time/rpc 客户端调用排除在外。
var dataHandleIdents = map[string]bool{
	"Conversations": true, "Members": true, "Messages": true,
	"Settings": true, "Reports": true, "WithdrawLogs": true,
}

// plaintextOutlets 是「解密后的正文进入响应」的全部出口。
// 这三个函数之外不存在第三条能把 ContentCipher 变成响应字段的路径（见文件头注释）。
var plaintextOutlets = []string{"decryptContent", "projectMessage", "messageInfo"}

type funcNode struct {
	key        string
	plainCalls []string
	accessors  map[string]bool
	inTxAccess map[string]bool
	// txHelper 标记「包内名字以 InTx 结尾的普通函数」：它只接收调用方的 session，
	// 因此它的每一次数据写都发生在调用方的事务里（parseLogicPackage 会钉住这一点）。
	// 没有这条规则，withdrawInTx 这类共享事务步骤会把跨表写藏在函数边界外，
	// 事务判定（inTx）在调用方永远收不齐。
	txHelper bool
	// helperSites / txHelperSites 统计本函数内对 *InTx 普通函数的调用点总数与
	// 「位于 TransactCtx 闭包内」的调用点数：全包两者相等才允许 txHelper 归属。
	helperSites   map[string]int
	txHelperSites map[string]int
}

func (n *funcNode) addAccessor(chain string, inTx bool) {
	n.accessors[chain] = true
	if inTx {
		n.inTxAccess[chain] = true
	}
}

// selectorChain 把一个 SelectorExpr 展平成 ident 链（如 l.svcCtx.Members.Find → [...]），
// 链上有非 ident 节点（如函数调用结果）时返回 nil：那种调用不算本包的数据接口。
func selectorChain(expr ast.Expr) []string {
	switch e := expr.(type) {
	case *ast.Ident:
		return []string{e.Name}
	case *ast.SelectorExpr:
		base := selectorChain(e.X)
		if base == nil || e.Sel == nil {
			return nil
		}
		return append(base, e.Sel.Name)
	case *ast.ParenExpr:
		return selectorChain(e.X)
	}
	return nil
}

// parseLogicPackage 解析当前包的非测试文件，回「函数名 → 直接调用 + 数据接口」图。
// go test 的工作目录就是包目录，因此这里用 "." 而不是拼源码绝对路径。
//
// 除了解析，它还钉住一条生产代码约束：名字以 InTx 结尾的包内普通函数必须
//  1. 真的接收 sqlx.Session 参数（否则命名在撒谎），且
//  2. 每个调用点都落在某个 TransactCtx 闭包里。
//
// 两条一起成立时，才允许把该函数内部的写按「事务内」归属到调用方（见 funcNode.txHelper）。
func parseLogicPackage(t *testing.T) map[string]*funcNode {
	t.Helper()
	fset := token.NewFileSet()
	// parser.ParseDir 的 filter 是「保留」语义（返回 true 才被解析）：
	// 这里留生产代码、排除 *_test.go，否则本用例会把自己的测试替身当成被测实现。
	skipTests := func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }
	pkgs, err := parser.ParseDir(fset, ".", skipTests, 0)
	if err != nil {
		t.Fatalf("解析包目录失败：%v", err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("预期一个包，实得 %d", len(pkgs))
	}
	nodes := map[string]*funcNode{}
	for _, pkg := range pkgs {
		for file, astFile := range pkg.Files {
			for _, decl := range astFile.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				key := fd.Name.Name
				recvType, recvVar := "", ""
				if fd.Recv != nil && len(fd.Recv.List) > 0 {
					recvVar = firstIdent(fd.Recv.List[0].Names)
					recvType = typeName(fd.Recv.List[0].Type)
					if recvType == "" {
						t.Fatalf("%s: 认不出接收者类型（生成代码形态变了？）", file)
					}
					key = recvType + "." + fd.Name.Name
				}
				node := &funcNode{key: key, accessors: map[string]bool{}, inTxAccess: map[string]bool{},
					helperSites: map[string]int{}, txHelperSites: map[string]int{}}
				if fd.Recv == nil && strings.HasSuffix(fd.Name.Name, "InTx") {
					node.txHelper = true
					if !declTakesSession(fd) {
						t.Errorf("%s: 名字以 InTx 结尾却没有 sqlx.Session 参数——命名在撒谎，"+
							"事务边界无法静态证明", key)
					}
				}
				// 先找出所有 TransactCtx 闭包里的调用：这些是「事务内的写」。
				txCalls := map[*ast.CallExpr]bool{}
				ast.Inspect(fd, func(n ast.Node) bool {
					ce, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					chain := selectorChain(ce.Fun)
					if len(chain) == 0 || chain[len(chain)-1] != "TransactCtx" {
						return true
					}
					for _, arg := range ce.Args {
						lit, ok := arg.(*ast.FuncLit)
						if !ok {
							continue
						}
						ast.Inspect(lit, func(m ast.Node) bool {
							if c2, ok := m.(*ast.CallExpr); ok {
								txCalls[c2] = true
							}
							return true
						})
					}
					return true
				})
				ast.Inspect(fd, func(n ast.Node) bool {
					ce, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch fn := ce.Fun.(type) {
					case *ast.Ident:
						node.plainCalls = append(node.plainCalls, fn.Name)
						if strings.HasSuffix(fn.Name, "InTx") {
							node.helperSites[fn.Name]++
							if txCalls[ce] {
								node.txHelperSites[fn.Name]++
							}
						}
					case *ast.SelectorExpr:
						chain := selectorChain(fn)
						if len(chain) == 0 {
							return true
						}
						last := chain[len(chain)-1]
						if len(chain) >= 2 && dataHandleIdents[chain[len(chain)-2]] {
							node.addAccessor(chain[len(chain)-2]+"."+last, txCalls[ce])
						}
						// 兄弟方法（l.replay → SendMessageLogic.replay）。
						if len(chain) == 2 && recvVar != "" && chain[0] == recvVar {
							node.plainCalls = append(node.plainCalls, recvType+"."+last)
						}
					}
					return true
				})
				nodes[key] = node
			}
		}
	}
	if len(nodes) == 0 {
		t.Fatal("没解析出任何函数：包目录或解析规则不对")
	}
	// *InTx 归属的成立前提：包内每个 *InTx 调用点都在 TransactCtx 闭包里。
	// 任何一处漏网（比如以后有人图省事在事务外调 withdrawInTx），这里就会失败，
	// 从而让「helper 的写算作调用方事务内」这条推论始终成立。
	total, inTxTotal := map[string]int{}, map[string]int{}
	for _, node := range nodes {
		for name, n := range node.helperSites {
			total[name] += n
			inTxTotal[name] += node.txHelperSites[name]
		}
	}
	for name, n := range total {
		if nodes[name] == nil || !nodes[name].txHelper {
			t.Errorf("%s: 调用了名字以 InTx 结尾的函数，但包内没有对应定义（同名外部函数？）", name)
			continue
		}
		if k := inTxTotal[name]; k != n {
			t.Errorf("%s：%d 个调用点里只有 %d 个在 TransactCtx 闭包内——它只接 session，"+
				"事务外调用必然写不进同一笔事务", name, n, k)
		}
	}
	return nodes
}

// declTakesSession 判一个函数签名里是否有 sqlx.Session 参数（含 *sqlx.Session、[]sqlx.Session）。
func declTakesSession(fd *ast.FuncDecl) bool {
	if fd.Type == nil || fd.Type.Params == nil {
		return false
	}
	for _, field := range fd.Type.Params.List {
		if typeChain(field.Type) == "sqlx.Session" {
			return true
		}
	}
	return false
}

// typeChain 把一个类型表达式展平成 "包.类型" 字符串（认不出形态时回 ""）。
func typeChain(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return typeChain(e.X)
	case *ast.ArrayType:
		return typeChain(e.Elt)
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		base := typeChain(e.X)
		if base == "" || e.Sel == nil {
			return ""
		}
		return base + "." + e.Sel.Name
	case *ast.ParenExpr:
		return typeChain(e.X)
	}
	return ""
}

func firstIdent(list []*ast.Ident) string {
	if len(list) == 0 {
		return ""
	}
	return list[0].Name
}

func typeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return typeName(e.X)
	case *ast.Ident:
		return e.Name
	case *ast.IndexExpr: // 泛型接收者 *T[P]
		return typeName(e.X)
	}
	return ""
}

// reachable 回 transitive closure（含自身）。
func reachable(nodes map[string]*funcNode, entry string) map[string]bool {
	out := map[string]bool{entry: true}
	stack := []string{entry}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		node := nodes[cur]
		if node == nil {
			continue
		}
		for _, call := range node.plainCalls {
			key := call
			if nodes[key] == nil {
				// 同名的包内普通函数（如 requireMembership）优先；否则可能是内建函数/外部方法。
				if !containsName(nodes, call) {
					continue
				}
			}
			if out[key] {
				continue
			}
			out[key] = true
			stack = append(stack, key)
		}
	}
	return out
}

func containsName(nodes map[string]*funcNode, name string) bool {
	for k := range nodes {
		if k == name {
			return true
		}
	}
	return false
}

// TestLogicEntryPointsAuthorizationEnumeration 用源码结构（而非用例覆盖）钉住不变量：
// 没有一条路径可以绕过成员证明触达会话/消息数据，明文出口只有一个。
//
// 新增第 16 个 logic 入口会先在这里失败：必须显式登记它的授权范围与允许的数据接口。
func TestLogicEntryPointsAuthorizationEnumeration(t *testing.T) {
	nodes := parseLogicPackage(t)
	if t.Failed() {
		t.FailNow()
	}

	entries := map[string]*funcNode{}
	for key, node := range nodes {
		parts := strings.Split(key, ".")
		if len(parts) != 2 {
			continue // 包内普通函数
		}
		recv, name := parts[0], parts[1]
		if !strings.HasSuffix(recv, "Logic") || !ast.IsExported(name) {
			continue
		}
		entries[name] = node
	}
	if len(entries) != len(logicContracts) {
		var extra, missing []string
		for name := range entries {
			if _, ok := logicContracts[name]; !ok {
				extra = append(extra, name)
			}
		}
		for name := range logicContracts {
			if _, ok := entries[name]; !ok {
				missing = append(missing, name)
			}
		}
		sort.Strings(extra)
		sort.Strings(missing)
		t.Fatalf("logic 入口集合与契约表不一致：实得 %d，契约 %d；未登记=%v 已失效=%v",
			len(entries), len(logicContracts), extra, missing)
	}

	// 明文出口的归属：先算出「谁能到 projectMessage/decryptContent/messageInfo」。
	plainUsers := map[string][]string{}
	for name, node := range entries {
		reach := reachable(nodes, node.key)
		for _, outlet := range plaintextOutlets {
			if reach[outlet] {
				plainUsers[name] = append(plainUsers[name], outlet)
			}
		}
	}
	for name, got := range plainUsers {
		want := logicContracts[name].plaintext
		if !want {
			t.Errorf("%s：不该触达明文出口，实得 %v（唯一合法出口是 ListMessages）", name, got)
		}
	}
	if len(plainUsers) != 1 || plainUsers["ListMessages"] == nil {
		t.Fatalf("明文出口归属异常：只有 ListMessages 能解密，实得 %v", plainUsers)
	}

	allAccessors := func(nodes map[string]*funcNode, reach map[string]bool) (map[string]bool, map[string]bool) {
		out := map[string]bool{}
		inTx := map[string]bool{}
		for key := range reach {
			node := nodes[key]
			if node == nil {
				continue
			}
			for a := range node.accessors {
				out[a] = true
				// node.txHelper 成立即「整个函数只在调用方事务里跑」，写全部算事务内；
				// 前提由 parseLogicPackage 的调用点检查兜底，这里不重复论证。
				if node.txHelper || node.inTxAccess[a] {
					inTx[a] = true
				}
			}
			for a := range node.inTxAccess {
				inTx[a] = true
			}
		}
		return out, inTx
	}

	for name, want := range logicContracts {
		name, want := name, want
		t.Run(name, func(t *testing.T) {
			node := entries[name]
			reach := reachable(nodes, node.key)

			got, txOnly := allAccessors(nodes, reach)
			allow := setOf(want.accessors)
			for a := range got {
				if !allow[a] {
					t.Errorf("%s 触达了未登记的数据接口 %s（新增越权面：需在契约表显式论证）", name, a)
				}
			}
			for a := range allow {
				if !got[a] {
					t.Errorf("%s 的契约登记了 %s 但实际没触达（契约过期，说明实现改了）", name, a)
				}
			}
			for _, w := range want.inTx {
				if !txOnly[w] {
					t.Errorf("%s：%s 必须只在事务内发生（跨表写要一次提交）", name, w)
				}
			}

			switch want.scope {
			case "member":
				if !got["Members.Find"] {
					t.Errorf("%s：没有查成员表，等于没有授权门禁", name)
				}
			case "self":
				for a := range got {
					if a == "Conversations.FindByIDs" {
						// 唯一的豁免：会话主体行只按「自己的成员行」给出的 id 回表补投影
						// （ListConversations 先 Members.ListByMid(mid) 再 FindByIDs），
						// 授权来源仍是成员表；下面的 Members.ListByMid 检查就是这条豁免的前提。
						// README「查询侧统一过滤」与 listconversationslogic.go 要点 1/5 同口径。
						continue
					}
					if strings.HasPrefix(a, "Messages.") || strings.HasPrefix(a, "Conversations.") {
						t.Errorf("%s 自称只管自己（scope=self），却触达了 %s", name, a)
					}
					if a == "Members.SetHidden" {
						// 隐藏会话的授权在 UPDATE 的 WHERE (conversation_id, mid) 内完成，
						// 这是本服务唯一「以写证明」的路径，README 已记录。
						continue
					}
					if strings.HasPrefix(a, "Members.") && want.scope == "self" &&
						a != "Members.ListByMid" && a != "Members.SumUnread" {
						t.Errorf("%s 触达了不该触达的成员表接口 %s", name, a)
					}
				}
				if !reach["checkMid"] {
					t.Errorf("%s：scope=self 但没有 checkMid，调用者身份未被证明", name)
				}
				if got["Conversations.FindByIDs"] && !got["Members.ListByMid"] {
					t.Errorf("%s：scope=self 却回表读会话主体，且没有先扫自己的成员行——"+
						"没有 Members.ListByMid 就证明不了这些 id 属于调用者", name)
				}
			case "operator":
				if !reach["checkOperator"] {
					t.Errorf("%s：运营侧入口没有 checkOperator", name)
				}
				for _, forbidden := range []string{"Messages.ListBeforeSeq", "Messages.FindByClientMsgID", "Messages.LastVisible"} {
					if got[forbidden] {
						t.Errorf("%s 是运营侧入口却触达了整页/正文读取接口 %s", name, forbidden)
					}
				}
			case "pair":
				for _, auth := range want.auth {
					if !reach[auth] {
						t.Errorf("%s：建档前必须调 %s", name, auth)
					}
				}
			}
			if want.scope == "member" {
				found := false
				for _, auth := range want.auth {
					if reach[auth] {
						found = true
					}
				}
				if len(want.auth) > 0 && !found {
					t.Errorf("%s：没有调用 %v 中的任何一个作为授权来源", name, want.auth)
				}
			}
			// 整页正文只有一个入口：ListMessages。
			if got["Messages.ListBeforeSeq"] && name != "ListMessages" {
				t.Errorf("%s 触达了整页消息读取接口", name)
			}
		})
	}
}

func setOf(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, v := range list {
		out[v] = true
	}
	return out
}
