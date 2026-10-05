package logic

// RetryDeadLetter 用例：两类死信的重投链路与「先重投、后标记」的顺序。
//
// 这里要钉住的五件事：
//   ① 幂等顺序：必须先把投递任务复位 / 重放事件，再把死信标记 retried。
//      反过来的话「标记成功但重投失败」就再也无法通过本接口恢复；
//   ② 源状态守卫：任务不在 dead_letter 状态时复位返回 (false, nil)，
//      此时显式报 ErrIllegalStateTransition，且**不得**把死信标记成已重投；
//   ③ 复位后的行必须真的回到可投递状态（state=pending、retry_count=0、
//      next_retry_at=0、last_error=""），否则 Dispatcher 永远扫不到它；
//   ④ 事件重投依赖 notification_consumer_offset.payload_json，信封缺失时
//      必须是显式 ErrEventPayloadMissing，不能「看起来重投了」；
//   ⑤ 并发处置（别的运营先改过状态）时守卫返回 false：重投已发生，
//      响应里必须如实说明死信已被他人处置，而不是静默冒充本次生效。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

func (e *env) callRetry(t *testing.T, in *rpc.RetryDeadLetterReq) (*rpc.RetryDeadLetterReply, error) {
	t.Helper()
	return NewRetryDeadLetterLogic(context.Background(), e.svcCtx).RetryDeadLetter(in)
}

// deadDelivery 铺一行「已进死信」的投递任务，返回其 delivery_id。
// id 由用例给定（不是 ULID），这样轨迹断言可以是确定字面量。
func (e *env) deadDelivery(t *testing.T, id string) string {
	t.Helper()
	e.seedDelivery(t, &model.NotificationDelivery{
		DeliveryId: id, BizKey: "row-" + id, BizGroupKey: "grp-" + id, Mid: midAlice,
		Channel: model.ChannelPush, TemplateCode: codeShip, TemplateVersion: 1, Lang: model.LangZhCN,
		TargetRef: "device-token-a", ParamsJson: `{"order_no":"SO-1"}`,
		State: model.DeliveryStateDeadLetter, RetryCount: 3, NextRetryAt: 900,
		LastError: "provider: http 500", Ctime: 100, Mtime: 100,
	})
	return id
}

// deadLetterOf 是死信行的公共底稿：主键由假件分配，用例只认返回值。
func deadLetterOf(source, eventID, deliveryID string) *model.NotificationDeadLetter {
	return &model.NotificationDeadLetter{
		EventId: eventID, EventType: "notification.request", Topic: "delivery:order_shipped",
		Source: source, DeliveryId: deliveryID, PayloadDigest: "digest-1",
		Reason: "provider: http 500", State: model.DeadLetterStatePending, Ctime: 100, Mtime: 100,
	}
}

// evtEnvelope 是一条合法的 notification.request.v1 信封（事件死信重放的唯一依据）。
// biz_key 用固定值，投递行按 biz_group_key 读回，避免断言依赖 ULID。
const evtEnvelope = `{"event_id":"evt-1","event_type":"notification.request","schema_version":1,` +
	`"occurred_at":"2026-01-01T00:00:00Z","producer":"test","trace_id":"trace-evt",` +
	`"aggregate_type":"order","aggregate_id":"o-1",` +
	`"payload":{"channel":"push","template_code":"order_shipped",` +
	`"template_params":{"order_no":"SO-9"},` +
	`"recipients":[{"mid":1001,"target_ref":"token-a","lang":"zh-CN"}],` +
	`"biz_key":"evt-biz-1"}}`

// absentDeadID 是一个永远不会存在的主键：只有「故意不播种死信」的用例用它，
// 因为假件的自增位从 1 开始且这些用例一行都没播种。
const absentDeadID = int64(9001)

// evtOffset 铺一条事件状态行（payload_json 是可重放信封）。
func (e *env) evtOffset(t *testing.T, eventID string, state int32, payload string) {
	t.Helper()
	e.seedOffset(t, &model.NotificationConsumerOffset{
		EventId: eventID, EventType: "notification.request", Topic: "notification.request.v1",
		State: state, PayloadJson: payload, RetryCount: 2, TraceId: "trace-evt",
		Ctime: 100, Mtime: 100,
	})
}

func TestRetryDeadLetterResetsDeliveryBeforeMarkingRetried(t *testing.T) {
	e := newEnv(t)
	dlvID := e.deadDelivery(t, "DLV-DEAD-1")
	id := e.seedDeadLetter(t, deadLetterOf(model.DeadLetterSourceDelivery, "", dlvID))

	m := e.mark()
	// operator 带空白：logic 会 Trim 后才落审计列（retrydeadletterlogic.go:45）。
	reply, err := e.callRetry(t, &rpc.RetryDeadLetterReq{Id: id, Operator: "  " + opAdmin + "  "})
	wantNoErr(t, "投递死信重投", err)
	wantOps(t, "投递死信重投：先复位任务、再标记死信", e.ops(m), []string{
		"dead.FindOne:" + itoa(id),
		"deliv.ResetForDeadLetterRetry:DLV-DEAD-1",
		"dead.MarkState:" + itoa(id) + "->2",
	})
	wantStringsEQ(t, "投递死信重投", reply.GetDeliveryIds(), []string{dlvID})
	wantEQ(t, "投递死信重投", "retried", reply.GetRetried(), int32(1))
	wantContains(t, "投递死信重投", "message", reply.GetMessage(), "复位为待投递")

	// 复位后的任务必须真的能被调度器扫到（清掉重试计数与错误原因）。
	d := e.onlyDelivery(t)
	wantEQ(t, "复位后的任务", "state", d.State, model.DeliveryStatePending)
	wantEQ(t, "复位后的任务", "retry_count", d.RetryCount, int32(0))
	wantEQ(t, "复位后的任务", "next_retry_at", d.NextRetryAt, int64(0))
	wantEQ(t, "复位后的任务", "last_error", d.LastError, "")
	wantEQ(t, "复位后的任务", "template_version 仍锁定原版本", d.TemplateVersion, int32(1))

	dl := e.deadLetter(t, id)
	wantEQ(t, "重投后的死信", "state", dl.State, model.DeadLetterStateRetried)
	wantEQ(t, "重投后的死信", "operator 记的是去空白后的操作人", dl.Operator, opAdmin)

	// 第二次调用：死信已处置，必须拒绝，而且一个写都不发生（不重复触达用户）。
	m2 := e.mark()
	_, err = e.callRetry(t, &rpc.RetryDeadLetterReq{Id: id, Operator: opAdmin})
	wantErrIs(t, "重复重投", err, ErrDeadLetterHandled)
	wantOps(t, "重复重投不得再写任何表", e.ops(m2), []string{"dead.FindOne:" + itoa(id)})
	wantEQ(t, "重复重投", "任务仍是 pending（没有被二次复位）", e.onlyDelivery(t).State, model.DeliveryStatePending)
}

// TestRetryDeadLetterRefusesToConsumeWhenResetGuardMisses 钉住顺序不变量的另一半：
// 复位没生效（任务不在 dead_letter 状态）时必须报错，且**不能**把死信标记成已重投——
// 否则这条死信就永久失去了重投入口。
func TestRetryDeadLetterRefusesToConsumeWhenResetGuardMisses(t *testing.T) {
	e := newEnv(t)
	e.seedDelivery(t, &model.NotificationDelivery{
		DeliveryId: "DLV-SENT-1", BizKey: "row-DLV-SENT-1", Mid: midAlice, Channel: model.ChannelPush,
		TemplateCode: codeShip, State: model.DeliveryStateSent, Ctime: 100, Mtime: 100,
	})
	id := e.seedDeadLetter(t, deadLetterOf(model.DeadLetterSourceDelivery, "", "DLV-SENT-1"))

	m := e.mark()
	reply, err := e.callRetry(t, &rpc.RetryDeadLetterReq{Id: id, Operator: opAdmin})
	wantErrIs(t, "任务不在 dead_letter", err, model.ErrIllegalStateTransition)
	wantErrContains(t, "任务不在 dead_letter", err, "DLV-SENT-1")
	if reply != nil {
		t.Errorf("复位失败时不得返回响应，实际 %+v", reply)
	}
	wantOps(t, "复位失败时不得标记死信", e.ops(m), []string{
		"dead.FindOne:" + itoa(id),
		"deliv.ResetForDeadLetterRetry:DLV-SENT-1",
	})
	wantTrue(t, "复位失败", "死信仍是 pending（还能再重投）",
		e.deadLetter(t, id).State == model.DeadLetterStatePending)
	wantEQ(t, "复位失败", "未被改动的任务状态", e.onlyDelivery(t).State, model.DeliveryStateSent)
}

func TestRetryDeadLetterPropagatesResetFailureAndRecovers(t *testing.T) {
	e := newEnv(t)
	dlvID := e.deadDelivery(t, "DLV-DEAD-1")
	id := e.seedDeadLetter(t, deadLetterOf(model.DeadLetterSourceDelivery, "", dlvID))

	down := errors.New("notification_delivery ResetForDeadLetterRetry: db down")
	e.delivSpy.Fail("ResetForDeadLetterRetry", down)

	m := e.mark()
	_, err := e.callRetry(t, &rpc.RetryDeadLetterReq{Id: id, Operator: opAdmin})
	wantErrIs(t, "复位写库失败", err, down)
	wantOps(t, "复位写库失败：没有标记动作", e.ops(m), []string{
		"dead.FindOne:" + itoa(id),
		"deliv.ResetForDeadLetterRetry:DLV-DEAD-1",
	})
	wantTrue(t, "复位写库失败", "死信仍是 pending", e.deadLetter(t, id).State == model.DeadLetterStatePending)
	wantEQ(t, "复位写库失败", "任务仍是 dead_letter", e.onlyDelivery(t).State, model.DeliveryStateDeadLetter)

	e.delivSpy.Recover("ResetForDeadLetterRetry")
	m2 := e.mark()
	_, err = e.callRetry(t, &rpc.RetryDeadLetterReq{Id: id, Operator: opAdmin})
	wantNoErr(t, "恢复后重放", err)
	wantOps(t, "恢复后重放", e.ops(m2), []string{
		"dead.FindOne:" + itoa(id),
		"deliv.ResetForDeadLetterRetry:DLV-DEAD-1",
		"dead.MarkState:" + itoa(id) + "->2",
	})
	wantEQ(t, "恢复后重放", "任务已复位", e.onlyDelivery(t).State, model.DeliveryStatePending)
}

func TestRetryDeadLetterReplaysEventEnvelope(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)
	e.evtOffset(t, "evt-1", model.EventStateReceived, evtEnvelope)
	id := e.seedDeadLetter(t, deadLetterOf(model.DeadLetterSourceEvent, "evt-1", ""))

	m := e.mark()
	reply, err := e.callRetry(t, &rpc.RetryDeadLetterReq{Id: id, Operator: opAdmin})
	wantNoErr(t, "事件死信重放", err)
	// 重放走的是真实事件处理链：领取处理权 -> 落投递任务 -> 回写 succeeded，
	// 之后 logic 才按 source_event_id 读回生成的任务并标记死信。
	wantOps(t, "事件死信重放", e.ops(m), []string{
		"dead.FindOne:" + itoa(id),
		"offset.FindOne:evt-1",
		"offset.InsertIfAbsent:evt-1",
		"offset.FindOne:evt-1",
		"offset.MarkState:evt-1->2",
		"tmpl.FindByState:order_shipped/1/zh-CN/st2",
		"dnd.FindOne:1001",
		"deliv.Insert:gk=evt-biz-1/mid1001",
		"offset.MarkState:evt-1->3",
		"deliv.ListBySourceEvent:evt-1",
		"dead.MarkState:" + itoa(id) + "->2",
	})

	d := e.deliveryByGroup(t, "evt-biz-1")
	wantEQ(t, "重放生成的任务", "state", d.State, model.DeliveryStatePending)
	wantEQ(t, "重放生成的任务", "source_event_id 必须回指原事件", d.SourceEventId, "evt-1")
	wantEQ(t, "重放生成的任务", "模板码", d.TemplateCode, codeShip)
	wantEQ(t, "重放生成的任务", "mid", d.Mid, midAlice)
	wantStringsEQ(t, "重放返回的任务 ID（读回值，不是手写）", reply.GetDeliveryIds(), []string{d.DeliveryId})
	wantEQ(t, "重放", "retried", reply.GetRetried(), int32(1))
	wantContains(t, "重放", "message", reply.GetMessage(), "payload_json")

	// 事件行落到 succeeded 并清空原文（隐私：报文不长期留存）。
	off := e.offsetRow(t, "evt-1")
	wantEQ(t, "重放后的事件行", "state", off.State, model.EventStateSucceeded)
	wantEQ(t, "重放后的事件行", "payload_json 已清空", off.PayloadJson, "")
	wantEQ(t, "重放后的事件行", "retry_count 沿用重放前的值", off.RetryCount, int32(2))
	wantEQ(t, "重放后的死信", "state", e.deadLetter(t, id).State, model.DeadLetterStateRetried)

	// 再点一次：死信已处置，直接拒绝，绝不会第二次落任务。
	m2 := e.mark()
	_, err = e.callRetry(t, &rpc.RetryDeadLetterReq{Id: id, Operator: opAdmin})
	wantErrIs(t, "重复重放", err, ErrDeadLetterHandled)
	wantOps(t, "重复重放", e.ops(m2), []string{"dead.FindOne:" + itoa(id)})
	wantEQ(t, "重复重放", "投递任务数", e.deliveryCount(), 1)
}

// TestRetryDeadLetterSettledEventIsConsumedWithoutRedelivery 钉住一个真实缺陷（见 README 缺口 #1）：
// 事件死信在生产里是由 recordFailure 写下的，事件行停在 dead_letter（consumer/eventhandler.go:240），
// 而 Handle 对「已存在且状态不是 received」的事件直接按重复投递忽略并返回 nil
// （consumer/eventhandler.go:198-202）。于是 RetryDeadLetter 拿到 nil、一条任务都没生成，
// 却仍然把死信标记为 retried（retrydeadletterlogic.go:111）——重投被静默吞掉，且再也无法从本接口恢复。
// 本用例断言的是**当前真实行为**，不是期望行为：修复 consumer 后这里必须转红并更新。
func TestRetryDeadLetterSettledEventIsConsumedWithoutRedelivery(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)
	e.evtOffset(t, "evt-1", model.EventStateDeadLetter, evtEnvelope)
	id := e.seedDeadLetter(t, deadLetterOf(model.DeadLetterSourceEvent, "evt-1", ""))

	m := e.mark()
	reply, err := e.callRetry(t, &rpc.RetryDeadLetterReq{Id: id, Operator: opAdmin})
	wantNoErr(t, "事件行已是 dead_letter", err)
	wantOps(t, "缺口：没有重放，但死信仍被消费", e.ops(m), []string{
		"dead.FindOne:" + itoa(id),
		"offset.FindOne:evt-1",
		"offset.InsertIfAbsent:evt-1",
		"offset.FindOne:evt-1",
		"deliv.ListBySourceEvent:evt-1",
		"dead.MarkState:" + itoa(id) + "->2",
	})
	wantEQ(t, "缺口：没有重放", "一条投递任务都没生成", e.deliveryCount(), 0)
	wantEQ(t, "缺口：没有重放", "retried 如实为 0", reply.GetRetried(), int32(0))
	wantEQ(t, "缺口：没有重放", "delivery_ids 为空", len(reply.GetDeliveryIds()), 0)
	wantTrue(t, "缺口：没有重放", "死信却被推进成 retried（无法再次重投）",
		e.deadLetter(t, id).State == model.DeadLetterStateRetried)
	wantEQ(t, "缺口：没有重放", "事件行状态没被改动", e.offsetRow(t, "evt-1").State, model.EventStateDeadLetter)
}

func TestRetryDeadLetterEventWithoutPayloadFailsExplicitly(t *testing.T) {
	e := newEnv(t)

	// 情况 A：连事件状态行都没有（Outbox 从未落地）。
	idA := e.seedDeadLetter(t, deadLetterOf(model.DeadLetterSourceEvent, "evt-none", ""))
	m := e.mark()
	replyA, err := e.callRetry(t, &rpc.RetryDeadLetterReq{Id: idA, Operator: opAdmin})
	wantErrIs(t, "事件行缺失", err, ErrEventPayloadMissing)
	wantErrContains(t, "事件行缺失", err, "evt-none")
	if replyA != nil {
		t.Errorf("事件行缺失：失败时不得返回响应，实际 %+v", replyA)
	}
	wantOps(t, "事件行缺失", e.ops(m), []string{
		"dead.FindOne:" + itoa(idA),
		"offset.FindOne:evt-none",
	})
	wantTrue(t, "事件行缺失", "死信保持 pending，等生产者重投 Outbox",
		e.deadLetter(t, idA).State == model.DeadLetterStatePending)

	// 情况 B：有状态行但原文为空（超长报文只留摘要，consumer/eventhandler.go:301）。
	e.evtOffset(t, "evt-empty", model.EventStateDeadLetter, "")
	idB := e.seedDeadLetter(t, deadLetterOf(model.DeadLetterSourceEvent, "evt-empty", ""))
	m2 := e.mark()
	replyB, err := e.callRetry(t, &rpc.RetryDeadLetterReq{Id: idB, Operator: opAdmin})
	wantErrIs(t, "信封为空", err, ErrEventPayloadMissing)
	wantErrContains(t, "信封为空", err, "evt-empty")
	if replyB != nil {
		t.Errorf("信封为空：失败时不得返回响应，实际 %+v", replyB)
	}
	wantOps(t, "信封为空", e.ops(m2), []string{
		"dead.FindOne:" + itoa(idB),
		"offset.FindOne:evt-empty",
	})
	wantTrue(t, "信封为空", "死信保持 pending", e.deadLetter(t, idB).State == model.DeadLetterStatePending)
	wantEQ(t, "信封为空", "没有任何投递任务", e.deliveryCount(), 0)
}

// TestRetryDeadLetterSucceededEventIsNotReplayed 钉住「不会二次触达用户」：
// 事件行已是 succeeded 时 ReplayEvent 直接返回 nil（consumer/eventhandler.go:306），
// 既不再落任务，也不报错，死信被标记为已重投。
func TestRetryDeadLetterSucceededEventIsNotReplayed(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)
	e.evtOffset(t, "evt-1", model.EventStateSucceeded, evtEnvelope)
	id := e.seedDeadLetter(t, deadLetterOf(model.DeadLetterSourceEvent, "evt-1", ""))

	m := e.mark()
	_, err := e.callRetry(t, &rpc.RetryDeadLetterReq{Id: id, Operator: opAdmin})
	wantNoErr(t, "事件已成功", err)
	wantOps(t, "事件已成功：只查不改", e.ops(m), []string{
		"dead.FindOne:" + itoa(id),
		"offset.FindOne:evt-1",
		"deliv.ListBySourceEvent:evt-1",
		"dead.MarkState:" + itoa(id) + "->2",
	})
	wantEQ(t, "事件已成功", "没有新任务", e.deliveryCount(), 0)
}

func TestRetryDeadLetterReportsConcurrentHandling(t *testing.T) {
	e := newEnv(t)
	dlvID := e.deadDelivery(t, "DLV-DEAD-1")
	id := e.seedDeadLetter(t, deadLetterOf(model.DeadLetterSourceDelivery, "", dlvID))

	// 模拟并发：logic 读到 pending 并复位任务之后、标记死信之前，另一个运营已经丢弃了它。
	e.deadSpy.Before("MarkState", func() {
		cur := e.deadLetter(t, id)
		cur.State = model.DeadLetterStateDiscarded
		cur.Operator = "op-other-1"
		e.dead.Seed(cur)
	})

	m := e.mark()
	reply, err := e.callRetry(t, &rpc.RetryDeadLetterReq{Id: id, Operator: opAdmin})
	wantNoErr(t, "并发处置", err)
	wantOps(t, "并发处置", e.ops(m), []string{
		"dead.FindOne:" + itoa(id),
		"deliv.ResetForDeadLetterRetry:DLV-DEAD-1",
		"dead.MarkState:" + itoa(id) + "->2",
	})
	wantContains(t, "并发处置", "message", reply.GetMessage(), "已被其他操作人处置")
	wantStringsEQ(t, "并发处置", reply.GetDeliveryIds(), []string{dlvID})

	dl := e.deadLetter(t, id)
	wantEQ(t, "并发处置", "守卫挡住后状态不得被覆盖", dl.State, model.DeadLetterStateDiscarded)
	wantEQ(t, "并发处置", "处置人不得被覆盖", dl.Operator, "op-other-1")
	wantEQ(t, "并发处置", "任务复位已经发生（重投本身幂等）", e.onlyDelivery(t).State, model.DeliveryStatePending)
}

func TestRetryDeadLetterRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name    string
		req     *rpc.RetryDeadLetterReq
		wantErr error
		msg     string
	}{
		{"operator 为空", &rpc.RetryDeadLetterReq{Id: 1, Operator: ""}, ErrOperatorRequired,
			"operator is required"},
		{"operator 只有空白", &rpc.RetryDeadLetterReq{Id: 1, Operator: "   \t "}, ErrOperatorRequired,
			"operator is required"},
		{"id 为 0", &rpc.RetryDeadLetterReq{Id: 0, Operator: opAdmin}, nil, "dead letter id is required"},
		{"id 为负", &rpc.RetryDeadLetterReq{Id: -7, Operator: opAdmin}, nil, "dead letter id is required"},
	}
	for _, tc := range cases {
		e := newEnv(t)
		m := e.mark()
		reply, err := e.callRetry(t, tc.req)
		if err == nil {
			t.Fatalf("%s：必须报错", tc.name)
		}
		if tc.wantErr != nil {
			wantErrIs(t, tc.name, err, tc.wantErr)
		}
		wantErrContains(t, tc.name, err, tc.msg)
		if reply != nil {
			t.Errorf("%s：拒绝时不得返回响应，实际 %+v", tc.name, reply)
		}
		// 参数不合法必须在读库之前挡下：一条 SQL 都不发出。
		wantOps(t, tc.name+"：一个读写都不发生", e.ops(m), nil)
	}

	// 以下分支都要先读到死信，才有后面的判定。
	storyCases := []struct {
		name    string
		dl      *model.NotificationDeadLetter
		nilEvt  bool
		fail    error
		wantErr error
		msg     string
	}{
		{"死信不存在", nil, false, nil, model.ErrNotFound, "dead letter id=" + itoa(absentDeadID)},
		{"死信已被丢弃（终态）", &model.NotificationDeadLetter{
			Source: model.DeadLetterSourceDelivery, DeliveryId: "DLV-X", State: model.DeadLetterStateDiscarded,
		}, false, nil, ErrDeadLetterHandled, "state=3"},
		{"来源未知", &model.NotificationDeadLetter{Source: "sms", State: model.DeadLetterStatePending},
			false, nil, nil, `未知死信来源 "sms"`},
		{"投递死信缺 delivery_id", &model.NotificationDeadLetter{Source: model.DeadLetterSourceDelivery,
			State: model.DeadLetterStatePending}, false, nil, nil, "缺少 delivery_id"},
		{"事件死信缺 event_id", &model.NotificationDeadLetter{Source: model.DeadLetterSourceEvent,
			State: model.DeadLetterStatePending}, false, nil, nil, "缺少 event_id"},
		{"事件处理器未装配", &model.NotificationDeadLetter{Source: model.DeadLetterSourceEvent,
			EventId: "evt-1", State: model.DeadLetterStatePending}, true, nil, nil, "事件处理器未装配"},
		{"死信读失败", nil, false, errors.New("dead letter FindOne: db down"), nil, "db down"},
	}
	for _, tc := range storyCases {
		e := newEnv(t)
		var id int64
		switch {
		case tc.dl != nil:
			id = e.seedDeadLetter(t, tc.dl)
		case tc.fail != nil:
			// 注入读失败也要有一行可读，否则证明不了「错误来自存储而不是没查到」。
			id = e.seedDeadLetter(t, deadLetterOf(model.DeadLetterSourceEvent, "evt-x", ""))
		default:
			// 本用例故意不播种任何死信，因此这个 id 必然不存在。
			id = absentDeadID
		}
		if tc.nilEvt {
			e.svcCtx.Events = nil
		}
		if tc.fail != nil {
			e.deadSpy.Fail("FindOne", tc.fail)
		}
		m := e.mark()
		reply, err := e.callRetry(t, &rpc.RetryDeadLetterReq{Id: id, Operator: opAdmin})
		if err == nil {
			t.Fatalf("%s：必须报错", tc.name)
		}
		if tc.wantErr != nil {
			wantErrIs(t, tc.name, err, tc.wantErr)
		}
		wantErrContains(t, tc.name, err, tc.msg)
		if reply != nil {
			t.Errorf("%s：拒绝时不得返回响应，实际 %+v", tc.name, reply)
		}
		wantOps(t, tc.name+"：拒绝路径只有那次读，不得有写操作", e.ops(m),
			[]string{"dead.FindOne:" + itoa(id)})
		if tc.dl != nil {
			wantTrue(t, tc.name, "死信状态没被动过", e.deadLetter(t, id).State == tc.dl.State)
		}
	}
}

// nil 请求单独一条：表里的 *rpc.RetryDeadLetterReq 零值不等于 nil。
func TestRetryDeadLetterRejectsNilRequest(t *testing.T) {
	e := newEnv(t)
	m := e.mark()
	reply, err := e.callRetry(t, nil)
	wantErrContains(t, "nil 请求", err, "nil request")
	if reply != nil {
		t.Errorf("nil 请求不得返回响应，实际 %+v", reply)
	}
	wantOps(t, "nil 请求不读库", e.ops(m), nil)
}
