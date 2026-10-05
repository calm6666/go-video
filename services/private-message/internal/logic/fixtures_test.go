// 本文件只放用例之间共用的脚手架：种子数据、快捷调用与「副作用为零」的探针。
// 测试替身（内存库/事务/日志/下游 gRPC）在 fakes_test.go 与 downstream_test.go。
//
// 为什么种子数据一律走 model 写接口而不是直接塞 map：
// 只有经过真写路径（fakes_test.go 里逐条复刻了唯一键与校验语义），
// 「库里有这一行」才等价于「生产库里可能出现这一行」——非法种子会先被挡掉。
// 例外是 rawMessage：它专门造只有 DBA 手工改数据才会出现的现场
// （已清理正文、已进入终态、缺会话主体的成员行），而这些正是 fail-closed 分支的入口条件。

package logic

import (
	"testing"

	"go-video/services/private-message/model"
	"go-video/services/private-message/rpc"
)

const (
	alice = int64(101)
	bob   = int64(202)
	// mallory 与 alice/bob 都没有会话：越权用例的调用者。
	mallory = int64(909)
	// 正文里刻意带一个可被字符串搜索识别的哨兵，隐私用例靠它证明「日志/错误/下游请求里没有原文」。
	secretBody = "SECRET-BODY-绝不外发的原文-13800000000"
)

type pair struct {
	convID           int64
	a, b             int64
	aMember, bMember model.ConversationMember
}

// peerOf_ 返回 p 里对方的 mid。
func (p pair) otherOf(mid int64) int64 {
	if mid == p.a {
		return p.b
	}
	return p.a
}

// seedPair 建一个正常会话 + 两行成员投影（走 logic 建档时同一个 newMemberRow，
// 因此「种子形状」与生产建档形状不会漂移）。
func (e *env) seedPair(t *testing.T, a, b int64) pair {
	t.Helper()
	conv, created, err := e.svc.Conversations.FindOrCreate(bg(), model.PairKey(a, b), min(a, b), max(a, b))
	if err != nil || conv == nil {
		t.Fatalf("seed 会话 %d-%d：%v", a, b, err)
	}
	if !created {
		t.Fatalf("seed 会话 %d-%d：预期新建，实为命中已有行", a, b)
	}
	rows := []*model.ConversationMember{
		newMemberRow(conv.ConversationID, a, b),
		newMemberRow(conv.ConversationID, b, a),
	}
	if err := e.svc.Members.Ensure(bg(), nil, rows); err != nil {
		t.Fatalf("seed 成员行：%v", err)
	}
	am, ok := e.st.memberRow(conv.ConversationID, a)
	if !ok {
		t.Fatalf("seed 后缺成员行 conv=%d mid=%d", conv.ConversationID, a)
	}
	bm, _ := e.st.memberRow(conv.ConversationID, b)
	return pair{convID: conv.ConversationID, a: a, b: b, aMember: am, bMember: bm}
}

// seedMessage 走 Messages.Insert 落一条**真实密文**的消息：正文只经过 newMessageCipher，
// 因此种子里的 content_cipher 与生产同构，「解密后等于原文」才是可证的。
func (e *env) seedMessage(t *testing.T, p pair, sender, receiver, seq int64, content string, state int32) *model.Message {
	t.Helper()
	mc, err := newMessageCipher(e.svc.Config)
	if err != nil {
		t.Fatalf("seed 加密器：%v", err)
	}
	blob, err := mc.encrypt(content)
	if err != nil {
		t.Fatalf("seed 加密：%v", err)
	}
	preview := buildPreview(model.MsgTypeText, content, e.svc.Config.PrivateMessage.PreviewRunes)
	msg := &model.Message{
		ConversationID: p.convID, Seq: seq, SenderMid: sender, ReceiverMid: receiver,
		MsgType: model.MsgTypeText, ContentCipher: blob, KeyVersion: mc.version(),
		ContentHash: mc.hash(content), Preview: preview, State: state,
		ClientMsgID: clientMsgIDFor(p.convID, seq),
	}
	id, created, err := e.svc.Messages.Insert(bg(), nil, msg)
	if err != nil || !created {
		t.Fatalf("seed 消息 seq=%d：%v", seq, err)
	}
	out, ok := e.st.msgRow(id)
	if !ok {
		t.Fatalf("seed 消息后回查不到 msg_id=%d", id)
	}
	// 投影时间取落库行的 ctime：fake 只在自己的副本上盖时间戳，msg.Ctime 仍是 0，
	// 而生产传的是本地 now。种子若跟着 0，成员行的 last_msg_time 恒为 0，
	// 「同秒多条会话靠 id 续翻」这类前提会静默退化成永真。
	if err := e.svc.Members.ApplyIncoming(bg(), nil, p.convID, sender, receiver, id, seq,
		model.MsgTypeText, preview, out.Ctime); err != nil {
		t.Fatalf("seed 投影：%v", err)
	}
	if err := e.svc.Conversations.TouchLastMessage(bg(), nil, p.convID, id, seq, out.Ctime); err != nil {
		t.Fatalf("seed 会话指针：%v", err)
	}
	return &out
}

// clientMsgIDFor 给种子消息一个稳定的幂等键：真库里 client_msg_id 由客户端提供，
// 种子必须自带，否则 model 层会按「缺幂等键」拒绝（那正是我们要测的规则之一）。
//
// 键必须按会话区分：真库的唯一键是 uniq_sender_client_msg(sender_mid, client_msg_id)，
// 只按 seq 生成的话，同一个 sender 在两条会话里种 seq=1 就会撞成「幂等回放」，
// 第二会话的种子根本插不进去（跨会话越权用例因此拿不到现场）。
func clientMsgIDFor(conversationID, seq int64) string {
	return "seed-client-msg-" + itoa(conversationID) + "-" + itoa(seq)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// rawMessage 改一行已成形的消息（造终态/已清理/脏数据现场）：
// 这些形状没有任何业务写路径能产出（撤回会写审计、清理会置 content_purged），
// 只能按「库里被手工改坏」来造，也正因为如此它们只用于测 fail-closed 分支。
func (e *env) rawMessage(t *testing.T, msgID int64, mutate func(*model.Message)) *model.Message {
	t.Helper()
	row, ok := e.st.msgRow(msgID)
	if !ok {
		t.Fatalf("库里没有 msg_id=%d", msgID)
	}
	mutate(&row)
	e.st.msgs[msgID] = row
	out, _ := e.st.msgRow(msgID)
	return &out
}

// rawMember 同上，改成员投影行。
func (e *env) rawMember(t *testing.T, conversationID, mid int64, mutate func(*model.ConversationMember)) {
	t.Helper()
	row, ok := e.st.memberRow(conversationID, mid)
	if !ok {
		t.Fatalf("库里没有成员行 conv=%d mid=%d", conversationID, mid)
	}
	mutate(&row)
	e.st.members[memberKey(conversationID, mid)] = row
}

func (e *env) memberRow(t *testing.T, conversationID, mid int64) model.ConversationMember {
	t.Helper()
	row, ok := e.st.memberRow(conversationID, mid)
	if !ok {
		t.Fatalf("库里没有成员行 conv=%d mid=%d", conversationID, mid)
	}
	return row
}

func (e *env) msgRow(t *testing.T, msgID int64) model.Message {
	t.Helper()
	row, ok := e.st.msgRow(msgID)
	if !ok {
		t.Fatalf("库里没有 msg_id=%d", msgID)
	}
	return row
}

func (e *env) convRow(t *testing.T, conversationID int64) model.Conversation {
	t.Helper()
	row, ok := e.st.convRow(conversationID)
	if !ok {
		t.Fatalf("库里没有 conversation_id=%d", conversationID)
	}
	return row
}

// --- 快捷调用（每个都直接走 logic 入口，不抄近路） ---

func (e *env) listMessages(t *testing.T, conversationID, viewer int64, cursorSeq int64, ps int32) (*rpc.ListMessagesReply, error) {
	t.Helper()
	return NewListMessagesLogic(bg(), e.svc).ListMessages(&rpc.ListMessagesReq{
		ConversationId: conversationID, Mid: viewer, CursorSeq: cursorSeq, Ps: ps,
	})
}

func (e *env) markRead(t *testing.T, conversationID, mid, readSeq int64) (*rpc.MarkReadReply, error) {
	t.Helper()
	return NewMarkReadLogic(bg(), e.svc).MarkRead(&rpc.MarkReadReq{ConversationId: conversationID, Mid: mid, ReadSeq: readSeq})
}

func (e *env) hideConversation(t *testing.T, conversationID, mid int64, hide bool) error {
	t.Helper()
	_, err := NewHideConversationLogic(bg(), e.svc).HideConversation(&rpc.HideConversationReq{
		Mid: mid, ConversationId: conversationID, Hide: hide,
	})
	return err
}

func (e *env) send(t *testing.T, in *rpc.SendMessageReq) (*rpc.SendMessageReply, error) {
	t.Helper()
	if in.ClientMsgId == "" {
		panic("发送用例必须自带 client_msg_id：留空等于测不到幂等键分支")
	}
	return NewSendMessageLogic(bg(), e.svc).SendMessage(in)
}

func (e *env) sendText(t *testing.T, sender, receiver int64, clientMsgID, content string) (*rpc.SendMessageReply, error) {
	t.Helper()
	return e.send(t, &rpc.SendMessageReq{
		Mid: sender, PeerMid: receiver, MsgType: rpc.MsgType_MSG_TYPE_TEXT,
		Content: content, ClientMsgId: clientMsgID,
	})
}

func (e *env) withdraw(t *testing.T, msgID, operator int64, source rpc.WithdrawSource, reason string, auditTaskID int64) (*rpc.WithdrawMessageReply, error) {
	t.Helper()
	return NewWithdrawMessageLogic(bg(), e.svc).WithdrawMessage(&rpc.WithdrawMessageReq{
		MsgId: msgID, OperatorMid: operator, Source: source, Reason: reason, AuditTaskId: auditTaskID,
	})
}

func (e *env) report(t *testing.T, msgID, reporter int64, reason int32, description string) (*rpc.ReportMessageReply, error) {
	t.Helper()
	return NewReportMessageLogic(bg(), e.svc).ReportMessage(&rpc.ReportMessageReq{
		MsgId: msgID, ReporterMid: reporter, Reason: reason, Description: description,
	})
}

func (e *env) verdict(t *testing.T, msgID, taskID int64, v rpc.ModerationVerdict, eventID, reason string) (*rpc.ApplyModerationVerdictReply, error) {
	t.Helper()
	return NewApplyModerationVerdictLogic(bg(), e.svc).ApplyModerationVerdict(&rpc.ApplyModerationVerdictReq{
		MsgId: msgID, TaskId: taskID, Verdict: v, EventId: eventID, Reason: reason,
	})
}

// --- 副作用为零的探针 ---

type writeMark struct {
	tx, plain, txs int
}

func (e *env) writeMark() writeMark {
	return writeMark{tx: len(e.st.sessionWrites), plain: len(e.st.plainWrites), txs: e.st.txs}
}

func (e *env) txWritesSince(m writeMark) []string { return e.st.sessionWrites[m.tx:] }
func (e *env) plainWritesSince(m writeMark) []string {
	return append([]string(nil), e.st.plainWrites[m.plain:]...)
}

// contentReaders 是「把消息行读进内存」的两个读接口：ListBeforeSeq 是整页正文的唯一入口，
// FindByID 是单条正文的入口。 requireMembership 自身会读会话主体行，所以「会话行被读过」
// 不能当成越权证据；但「一条消息行都没读」就必然「一行都没解密」。
var messageRowReaders = []string{"Messages.ListBeforeSeq", "Messages.FindByID", "Messages.FindByClientMsgID", "Messages.FindByIDs", "Messages.LastVisible"}

// loadedMessageRows 是「有多少条消息行被物化进了 logic 的内存」：
// 读的是 fakes_test.go 里的 rowsLoaded 账本（真正取到的行数），不是读接口的调用次数。
// 区别只在「消息本就不存在」这一类被拒调用上：它必须查一次才能回答「不存在」，
// 但一行都没取到 —— 没有内容出境，也就没有可泄漏的东西。
// 只要行真的存在（越权读到别人的消息），这里必然涨，断言强度不降。
func (e *env) loadedMessageRows() int {
	var n int
	for _, name := range messageRowReaders {
		n += e.st.rowsLoaded[name]
	}
	return n
}

// probe 是一次调用前后的差分观测点：行数、写入、事务、消息行读取、成员证明。
type probe struct {
	rows        rows
	wm          writeMark
	msgRows     int
	membersFind int
}

func (e *env) probe() probe {
	return probe{
		rows:        e.snapshotRows(),
		wm:          e.writeMark(),
		msgRows:     e.loadedMessageRows(),
		membersFind: e.calls("Members.Find"),
	}
}

// requireRejectedNoSideEffects 断言「这次被拒的调用什么都没留下」：
// 行没多没少、没有写入（事务内外都没有）、没开事务，且一条消息行都没被取进内存。
// 消息行没被取到 ⇒ 解密这一步不可能被走到（这是进程内能给出的最强隐私证明：
// Cache 是具体类型 *redis.Redis，「缓存键被不被碰」无法在不起真 Redis 的前提下观测，
// 键格式与前缀由 bounds 用例按纯函数钉住）。
func (e *env) requireRejectedNoSideEffects(t *testing.T, before probe, label string) {
	t.Helper()
	e.requireSameRowsAndWrites(t, before, label)
	if got := e.loadedMessageRows() - before.msgRows; got != 0 {
		t.Fatalf("%s：被拒后仍把 %d 条消息行读进了内存，等于先取数据再判权限", label, got)
	}
}

// requireSameRowsAndWrites 是没带「读」断言的弱一档探针：
// 用于授权证明本身就要先读消息行的入口（撤回、举报：Messages.FindByID 在 requireMembership 之前）。
func (e *env) requireSameRowsAndWrites(t *testing.T, before probe, label string) {
	t.Helper()
	e.requireSameRows(t, before.rows, label)
	if got := e.plainWritesSince(before.wm); len(got) > 0 {
		t.Fatalf("%s：产生了事务外写入 %v", label, got)
	}
	if got := e.txWritesSince(before.wm); len(got) > 0 {
		t.Fatalf("%s：产生了写入 %v", label, got)
	}
	if got := e.st.txs - before.wm.txs; got != 0 {
		t.Fatalf("%s：产生了 %d 个事务", label, got)
	}
}

// requireMembershipProved 断言这次调用确实查过成员表（授权唯一来源），
// 否则「被拒」可能只是因为参数先被挡了，测不到授权本身。
func (e *env) requireMembershipProved(t *testing.T, before probe, label string) {
	t.Helper()
	if got := e.calls("Members.Find") - before.membersFind; got <= 0 {
		t.Fatalf("%s：没有查成员表（Members.Find），说明授权门禁被绕过了", label)
	}
}
