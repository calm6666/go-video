package logic

// ListDeadLetters 用例：死信列表的分页、排序、过滤与投影口径。
//
// 这里要钉住的四件事：
//   ① total 是「过滤后的总行数」，与页码无关，越界页只清空列表不清空 total；
//   ② 排序是 ctime DESC, id DESC（model/notificationdeadletter.go:178 ORDER BY ctime DESC, id DESC），
//      用例给每行不同的 ctime，让顺序可预测，否则「只断言集合相等」抓不到排序回归；
//   ③ 过滤条件全是 AND，且 0 值表示「不过滤」——state=UNSPECIFIED 不是「查 state=0」；
//   ④ 投影只回传契约已有的 10 个字段：死信表里的 source / delivery_id 两列不会出现在响应里，
//      同时请求侧也没有 source 过滤位 —— 见 TestListDeadLettersCannotAskForSource，
//      这条缺口必须用断言钉住，否则以后补契约字段时没人知道行为变了。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

func (e *env) callListDeadLetters(t *testing.T, in *rpc.ListDeadLettersReq) (*rpc.ListDeadLettersReply, error) {
	t.Helper()
	return NewListDeadLettersLogic(context.Background(), e.svcCtx).ListDeadLetters(in)
}

// deadRow 构造一条死信。name 进 payload_digest，便于把「行」和「断言」对上；
// 主键由假件分配，用例只能通过 seedDeadLetters 的返回值引用它。
func deadRow(name, source, eventID, topic string, state int32, ctime int64) *model.NotificationDeadLetter {
	return &model.NotificationDeadLetter{
		EventId: eventID, EventType: "notification.request", Topic: topic,
		Source: source, DeliveryId: "DLV-" + name, PayloadDigest: "digest-" + name,
		Reason: "provider timeout after max retries", State: state, Ctime: ctime, Mtime: ctime,
	}
}

// seedFourDeadLetters 铺 4 条：事件死信 2 条（ctime 300/100）+ 投递死信 2 条（ctime 200/400），
// 状态覆盖 pending / retried / discarded，顺序刻意与 seed 顺序不同，这样排序才有东西可验。
func (e *env) seedFourDeadLetters(t *testing.T) map[string]int64 {
	t.Helper()
	ids := e.seedDeadLetters(t,
		deadRow("evt-old", model.DeadLetterSourceEvent, "evt-old", "notification.request.v1", model.DeadLetterStatePending, 300),
		deadRow("dlv-mid", model.DeadLetterSourceDelivery, "", "delivery:order_shipped", model.DeadLetterStateRetried, 200),
		deadRow("evt-new", model.DeadLetterSourceEvent, "evt-new", "notification.request.v1", model.DeadLetterStateDiscarded, 100),
		deadRow("dlv-new", model.DeadLetterSourceDelivery, "evt-7", "delivery:login_code", model.DeadLetterStatePending, 400),
	)
	return map[string]int64{
		"evt-old": ids[0], "dlv-mid": ids[1], "evt-new": ids[2], "dlv-new": ids[3],
	}
}

// namesOfDead 按服务端给的顺序取 payload_digest（用例靠它同时核对顺序与命中集合）。
func namesOfDead(rows []*rpc.DeadLetterInfo) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.GetPayloadDigest())
	}
	return out
}

func TestListDeadLettersPaginatesAndOrdersByCtimeDesc(t *testing.T) {
	e := newEnv(t)
	id := e.seedFourDeadLetters(t)

	m := e.mark()
	p1, err := e.callListDeadLetters(t, &rpc.ListDeadLettersReq{Pn: 1, Ps: 2})
	wantNoErr(t, "第 1 页", err)
	wantOps(t, "第 1 页", e.ops(m), []string{listDeadOp("", "", "", 0, 1, 2)})
	wantStringsEQ(t, "第 1 页顺序（ctime 倒序）", namesOfDead(p1.GetDeadLetters()), []string{"digest-dlv-new", "digest-evt-old"})
	wantEQ(t, "第 1 页", "total", p1.GetTotal(), int64(4))

	p2, err := e.callListDeadLetters(t, &rpc.ListDeadLettersReq{Pn: 2, Ps: 2})
	wantNoErr(t, "第 2 页", err)
	wantStringsEQ(t, "第 2 页", namesOfDead(p2.GetDeadLetters()), []string{"digest-dlv-mid", "digest-evt-new"})
	wantEQ(t, "第 2 页", "total 不随页码变化", p2.GetTotal(), int64(4))

	// 越界页：空列表 + total 原样 + 非 nil 切片（客户端不该被迫判空）。
	p9, err := e.callListDeadLetters(t, &rpc.ListDeadLettersReq{Pn: 3, Ps: 2})
	wantNoErr(t, "越界页", err)
	wantEQ(t, "越界页", "行数", len(p9.GetDeadLetters()), 0)
	wantEQ(t, "越界页", "total 仍为全量", p9.GetTotal(), int64(4))
	if p9.GetDeadLetters() == nil {
		t.Error("越界页：dead_letters = nil, want 非 nil 空切片")
	}

	// 主键必须原样回传，否则运营拿着列表也调不动 RetryDeadLetter。
	wantInt64s(t, "第 1 页主键顺序", []int64{
		p1.GetDeadLetters()[0].GetId(), p1.GetDeadLetters()[1].GetId(),
	}, []int64{id["dlv-new"], id["evt-old"]})
}

// TestListDeadLettersProjectsContractFieldsAndKeepsPrivacy 逐字段核对投影，
// 同时钉住隐私口径：死信表里不存原始报文，响应里也只能有摘要与脱敏原因。
func TestListDeadLettersProjectsContractFieldsAndKeepsPrivacy(t *testing.T) {
	e := newEnv(t)
	id := e.seedFourDeadLetters(t)

	got, err := e.callListDeadLetters(t, &rpc.ListDeadLettersReq{EventId: "evt-new", Pn: 1, Ps: 10})
	wantNoErr(t, "按 event_id 查", err)
	wantEQ(t, "按 event_id 查", "行数", len(got.GetDeadLetters()), 1)
	info := got.GetDeadLetters()[0]
	row := e.deadLetter(t, id["evt-new"])

	wantEQ(t, "投影", "id", info.GetId(), row.Id)
	wantEQ(t, "投影", "event_id", info.GetEventId(), row.EventId)
	wantEQ(t, "投影", "event_type", info.GetEventType(), row.EventType)
	wantEQ(t, "投影", "topic", info.GetTopic(), row.Topic)
	wantEQ(t, "投影", "payload_digest", info.GetPayloadDigest(), row.PayloadDigest)
	wantEQ(t, "投影", "reason", info.GetReason(), row.Reason)
	wantEQ(t, "投影", "state", info.GetState(), rpc.DeadLetterState_DEAD_LETTER_DISCARDED)
	wantEQ(t, "投影", "operator", info.GetOperator(), row.Operator)
	wantEQ(t, "投影", "ctime", info.GetCtime(), row.Ctime)
	wantEQ(t, "投影", "mtime", info.GetMtime(), row.Mtime)

	// 隐私：整条响应不得带出原始信封（假件里 payload 从没进死信表，
	// 这里钉的是「以后有人往 reason/digest 里塞原文也会被这行抓到」）。
	wantNotContains(t, "投影", info.String(), "13800000000")
	wantNotContains(t, "投影", info.String(), row.DeliveryId)
}

func TestListDeadLettersFiltersAreAndComposedWithZeroMeansNoFilter(t *testing.T) {
	e := newEnv(t)
	e.seedFourDeadLetters(t)

	cases := []struct {
		name  string
		req   *rpc.ListDeadLettersReq
		op    string
		want  []string
		total int64
	}{
		{"按 event_id（投递死信也带 event_id，命中 dlv-new）",
			&rpc.ListDeadLettersReq{EventId: "evt-7", Pn: 1, Ps: 10},
			listDeadOp("evt-7", "", "", 0, 1, 10), []string{"digest-dlv-new"}, 1},
		{"按 topic", &rpc.ListDeadLettersReq{Topic: "delivery:order_shipped", Pn: 1, Ps: 10},
			listDeadOp("", "delivery:order_shipped", "", 0, 1, 10), []string{"digest-dlv-mid"}, 1},
		{"按状态 pending", &rpc.ListDeadLettersReq{State: rpc.DeadLetterState_DEAD_LETTER_PENDING, Pn: 1, Ps: 10},
			listDeadOp("", "", "", 1, 1, 10), []string{"digest-dlv-new", "digest-evt-old"}, 2},
		{"状态未指定 = 不过滤（不是查 state=0）",
			&rpc.ListDeadLettersReq{State: rpc.DeadLetterState_DEAD_LETTER_STATE_UNSPECIFIED, Pn: 1, Ps: 10},
			listDeadOp("", "", "", 0, 1, 10),
			[]string{"digest-dlv-new", "digest-evt-old", "digest-dlv-mid", "digest-evt-new"}, 4},
		{"终态 retried 也必须查得到（审计不能只看待处理）",
			&rpc.ListDeadLettersReq{State: rpc.DeadLetterState_DEAD_LETTER_RETRIED, Pn: 1, Ps: 10},
			listDeadOp("", "", "", 2, 1, 10), []string{"digest-dlv-mid"}, 1},
		{"event_id + 状态（AND）",
			&rpc.ListDeadLettersReq{EventId: "evt-7", State: rpc.DeadLetterState_DEAD_LETTER_PENDING, Pn: 1, Ps: 10},
			listDeadOp("evt-7", "", "", 1, 1, 10), []string{"digest-dlv-new"}, 1},
		{"矛盾组合不得退化成无条件全表",
			&rpc.ListDeadLettersReq{EventId: "evt-old", Topic: "delivery:order_shipped", Pn: 1, Ps: 10},
			listDeadOp("evt-old", "delivery:order_shipped", "", 0, 1, 10), nil, 0},
	}
	for _, tc := range cases {
		m := e.mark()
		got, err := e.callListDeadLetters(t, tc.req)
		wantNoErr(t, tc.name, err)
		wantOps(t, tc.name+"：过滤条件必须原样下推到 model", e.ops(m), []string{tc.op})
		wantEQ(t, tc.name, "total", got.GetTotal(), tc.total)
		if tc.want == nil {
			if got.GetDeadLetters() == nil {
				t.Errorf("%s：空结果 dead_letters = nil, want 非 nil 空切片", tc.name)
			}
			continue
		}
		wantStringsEQ(t, tc.name, namesOfDead(got.GetDeadLetters()), tc.want)
	}
}

// TestListDeadLettersCannotAskForSource 钉住一个真实缺口：
// model.DeadLetterFilter 有 Source 位、生产 SQL 也确实拼了 source = ?
// （model/notificationdeadletter.go:49 与 :159），但
//   - rpc.ListDeadLettersReq 没有 source 字段，listdeadletterslogic.go:36-40 于是永远传空串；
//   - rpc.DeadLetterInfo 也没有 source / delivery_id 字段（policy/projection.go:118-136）。
//
// 结果是运营面既没法「只看投递死信」，也没法从列表里认出某条死信对应哪个投递任务，
// 而 RetryDeadLetter 又必须按 id 调用 —— 只能靠人肉试。
// 本用例断言的是当前真实行为（两类死信混在一起返回、轨迹里 src= 恒为空），
// 一旦契约补上来源字段，这里会红，提醒同步更新 README 缺口清单。
func TestListDeadLettersCannotAskForSource(t *testing.T) {
	e := newEnv(t)
	e.seedFourDeadLetters(t)

	m := e.mark()
	got, err := e.callListDeadLetters(t, &rpc.ListDeadLettersReq{Pn: 1, Ps: 10})
	wantNoErr(t, "无来源过滤的全量查询", err)
	wantOps(t, "无来源过滤", e.ops(m), []string{listDeadOp("", "", "", 0, 1, 10)})
	wantEQ(t, "无来源过滤", "total", got.GetTotal(), int64(4))

	// 事件死信与投递死信混在同一页里，且响应字段无法区分两者。
	seenEvent, seenDelivery := false, false
	for _, info := range got.GetDeadLetters() {
		switch info.GetPayloadDigest() {
		case "digest-evt-old", "digest-evt-new":
			seenEvent = true
		case "digest-dlv-mid", "digest-dlv-new":
			seenDelivery = true
		}
		if info.GetId() == 0 {
			t.Errorf("投影：%+v 主键为 0，运营无法据此重投", info)
		}
	}
	wantTrue(t, "无来源过滤", "事件死信与投递死信同页返回", seenEvent && seenDelivery)

	// 用 topic 间接筛「投递死信」是当前唯一的绕路办法，但它依赖 dispatcher
	// 写入的 "delivery:<template_code>" 约定（consumer/dispatcher.go:436），契约里没写。
	m2 := e.mark()
	byTopic, err := e.callListDeadLetters(t, &rpc.ListDeadLettersReq{Topic: "delivery:login_code", Pn: 1, Ps: 10})
	wantNoErr(t, "按 topic 绕路筛投递死信", err)
	wantOps(t, "按 topic 绕路", e.ops(m2), []string{listDeadOp("", "delivery:login_code", "", 0, 1, 10)})
	wantEQ(t, "按 topic 绕路", "行数", len(byTopic.GetDeadLetters()), 1)
}

func TestListDeadLettersNormalizesPageParamsBeforeHittingModel(t *testing.T) {
	e := newEnv(t)
	e.seedFourDeadLetters(t)

	cases := []struct {
		name     string
		pn, ps   int32
		wantPage string
	}{
		{"页码 0 回落第 1 页", 0, 10, listDeadOp("", "", "", 0, 1, 10)},
		{"负页码回落第 1 页", -3, 10, listDeadOp("", "", "", 0, 1, 10)},
		{"每页 0 回落 20", 1, 0, listDeadOp("", "", "", 0, 1, 20)},
		{"每页负数回落 20", 1, -5, listDeadOp("", "", "", 0, 1, 20)},
		// logic 侧先夹到 100，所以 model 的 ErrPsTooLarge 从 RPC 永远拿不到，
		// 这里钉「不报错 + 夹紧」，不钉一个不可能发生的错误。
		{"每页超上限被夹紧", 1, 500, listDeadOp("", "", "", 0, 1, 100)},
	}
	for _, tc := range cases {
		m := e.mark()
		got, err := e.callListDeadLetters(t, &rpc.ListDeadLettersReq{Pn: tc.pn, Ps: tc.ps})
		wantNoErr(t, tc.name, err)
		wantOps(t, tc.name, e.ops(m), []string{tc.wantPage})
		wantEQ(t, tc.name, "total", got.GetTotal(), int64(4))
	}
}

func TestListDeadLettersRejectsNilAndPropagatesStoreError(t *testing.T) {
	e := newEnv(t)
	e.seedFourDeadLetters(t)

	m := e.mark()
	reply, err := e.callListDeadLetters(t, nil)
	wantErrContains(t, "nil 请求", err, "nil request")
	if reply != nil {
		t.Errorf("nil 请求不得返回响应，实际 %+v", reply)
	}
	wantOps(t, "nil 请求不读库", e.ops(m), nil)

	down := errors.New("notification_dead_letter List: db down")
	e.deadSpy.Fail("List", down)
	m2 := e.mark()
	r2, err2 := e.callListDeadLetters(t, &rpc.ListDeadLettersReq{Pn: 1, Ps: 10})
	wantErrIs(t, "死信表读失败", err2, down)
	if r2 != nil {
		t.Errorf("读库失败时不得返回空列表冒充「一条死信都没有」，实际 %+v", r2)
	}
	wantOps(t, "死信表读失败", e.ops(m2), []string{listDeadOp("", "", "", 0, 1, 10)})
}
