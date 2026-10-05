package logic

// GetDeliveryStatus 用例：调用方读回的必须是「库里的状态」，不是「请求时的期望」。
//
// 契约上最容易出问题的三点，本文件逐条钉住：
//   ① 不存在时是 found=false + 无错误（不是报错，也不是返回一条伪造的空记录）；
//   ② 供应商回执（provider / provider_msg_id / sent_at / payload_digest）必须原样带出，
//      否则运营核对账目时无据可依；
//   ③ 隐私字段不得外泄：params_json（渲染变量快照）、biz_group_key、source_event_id、lang
//      都不在 rpc.DeliveryInfo 里，logic 也不得把它们塞进别的字段。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/notification/internal/policy"
	"go-video/services/notification/internal/provider"
	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

func (e *env) callStatus(t *testing.T, deliveryID string) (*rpc.GetDeliveryStatusReply, error) {
	t.Helper()
	return NewGetDeliveryStatusLogic(context.Background(), e.svcCtx).
		GetDeliveryStatus(&rpc.GetDeliveryStatusReq{DeliveryId: deliveryID})
}

// statusRow 铺一条字段互不相同的投递任务，方便逐字段断言投影没有串位。
func statusRow(id string) *model.NotificationDelivery {
	return &model.NotificationDelivery{
		DeliveryId: id, BizKey: "row-key-" + id, BizGroupKey: "grp-" + id,
		Mid: midAlice, Channel: model.ChannelPush, TemplateCode: codeShip, TemplateVersion: 7,
		Lang: model.LangZhCN, TargetRef: "device-token-a",
		ParamsJson: `{"phone":"13800000000"}`, PayloadDigest: "digest-" + id,
		State: model.DeliveryStateSent, Provider: "apns", ProviderMsgId: "pmid-9",
		Priority: policy.PriorityHigh, RetryCount: 2, NextRetryAt: 0,
		LastError: "", SentAt: 1800, ExpireAt: 2400, SourceEventId: "evt-src",
		TraceId: "trace-9", Ctime: 1000, Mtime: 1800,
	}
}

// TestGetDeliveryStatusReadsBackStoredRow 全字段投影 + 只做一次读，不写库。
func TestGetDeliveryStatusReadsBackStoredRow(t *testing.T) {
	e := newEnv(t)
	e.seedDelivery(t, statusRow("DLV-1"))

	m := e.mark()
	got, err := e.callStatus(t, "DLV-1")
	wantNoErr(t, "查状态", err)
	wantOps(t, "查状态只做一次主键读", e.ops(m), []string{"deliv.FindOne:DLV-1"})
	wantEQ(t, "查状态", "found", got.GetFound(), true)

	d := got.GetDelivery()
	if d == nil {
		t.Fatal("found=true 时 delivery 不得为空")
	}
	wantEQ(t, "投影", "delivery_id", d.GetDeliveryId(), "DLV-1")
	wantEQ(t, "投影", "biz_key", d.GetBizKey(), "row-key-DLV-1")
	wantEQ(t, "投影", "mid", d.GetMid(), midAlice)
	wantEQ(t, "投影", "channel", d.GetChannel(), rpc.Channel_CHANNEL_PUSH)
	wantEQ(t, "投影", "template_code", d.GetTemplateCode(), codeShip)
	wantEQ(t, "投影", "template_version", d.GetTemplateVersion(), int32(7))
	wantEQ(t, "投影", "state", d.GetState(), rpc.DeliveryState_DELIVERY_SENT)
	wantEQ(t, "投影", "provider", d.GetProvider(), "apns")
	wantEQ(t, "投影", "provider_msg_id", d.GetProviderMsgId(), "pmid-9")
	wantEQ(t, "投影", "retry_count", d.GetRetryCount(), int32(2))
	wantEQ(t, "投影", "sent_at", d.GetSentAt(), int64(1800))
	wantEQ(t, "投影", "expire_at", d.GetExpireAt(), int64(2400))
	wantEQ(t, "投影", "priority", d.GetPriority(), policy.PriorityHigh)
	wantEQ(t, "投影", "trace_id", d.GetTraceId(), "trace-9")
	wantEQ(t, "投影", "ctime", d.GetCtime(), int64(1000))

	// 隐私：变量快照里放了手机号，绝不能随状态查询外泄。
	wantNotContains(t, "投影不得带出 params_json", got.String(), "13800000000")
	// biz_group_key / source_event_id / lang 不在契约里，也不得借 payload_digest 等字段夹带。
	wantNotContains(t, "投影不得带出请求级键", got.String(), "grp-DLV-1")
	wantNotContains(t, "投影不得带出内部事件 ID", got.String(), "evt-src")
}

// TestGetDeliveryStatusNotFoundIsFoundFalse 不存在的 ID：found=false 且无错误。
// 同时钉住「本方法只认 delivery_id」——把 biz_key 当 ID 传进来也查不到，
// 这是契约而不是实现细节，客户端混用两种键时必须得到明确的「不存在」。
func TestGetDeliveryStatusNotFoundIsFoundFalse(t *testing.T) {
	e := newEnv(t)
	e.seedDelivery(t, statusRow("DLV-1"))

	m := e.mark()
	got, err := e.callStatus(t, "DLV-missing")
	wantNoErr(t, "不存在的 ID", err)
	wantOps(t, "不存在的 ID", e.ops(m), []string{"deliv.FindOne:DLV-missing"})
	wantEQ(t, "不存在的 ID", "found", got.GetFound(), false)
	if got.GetDelivery() != nil {
		t.Errorf("found=false 时不得返回投递记录，实际 %+v", got.GetDelivery())
	}

	// 拿行级幂等键当 delivery_id 查：同样是「不存在」，不得顺手命中已存在的那行。
	got2, err := e.callStatus(t, "row-key-DLV-1")
	wantNoErr(t, "按 biz_key 查", err)
	wantEQ(t, "按 biz_key 查", "found", got2.GetFound(), false)
}

// TestGetDeliveryStatusDistinguishesPendingFromAbsent 「任务不存在」和「任务还没投出去」
// 必须给出不同答案（这是本方法存在的理由）：未投递时 found=true + state=pending + 回执字段为空。
func TestGetDeliveryStatusDistinguishesPendingFromAbsent(t *testing.T) {
	e := newEnv(t)
	row := statusRow("DLV-2")
	row.State = model.DeliveryStatePending
	row.Provider, row.ProviderMsgId, row.PayloadDigest, row.SentAt = "", "", "", 0
	e.seedDelivery(t, row)

	got, err := e.callStatus(t, "DLV-2")
	wantNoErr(t, "待投递", err)
	wantEQ(t, "待投递", "found", got.GetFound(), true)
	d := got.GetDelivery()
	wantEQ(t, "待投递", "state", d.GetState(), rpc.DeliveryState_DELIVERY_PENDING)
	wantEQ(t, "待投递", "provider 尚未确定", d.GetProvider(), "")
	wantEQ(t, "待投递", "provider_msg_id 未伪造", d.GetProviderMsgId(), "")
	wantEQ(t, "待投递", "sent_at 未伪造", d.GetSentAt(), int64(0))
}

// TestGetDeliveryStatusSeesDispatchReceipt 落库后经由 Dispatcher 外发，再按返回的
// delivery_id 读回：必须看到 sent + 供应商回执。
// delivery_id 是 ULID（非确定性），所以这里按「先读出 id、再按它查」的方式断言，
// 不把 ULID 字面量写进期望序列。
func TestGetDeliveryStatusSeesDispatchReceipt(t *testing.T) {
	push := newProvider(provider.ChannelPush, "pmid-real-1")
	e := newEnv(t, withSyncSend(push))
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)

	reply, err := e.send(t, pushReq("ship-9", recip(midAlice, "device-token-a")))
	wantNoErr(t, "同步发送", err)
	if len(reply.GetDeliveries()) != 1 {
		t.Fatalf("投递记录数 = %d, want 1", len(reply.GetDeliveries()))
	}
	id := reply.GetDeliveries()[0].GetDeliveryId()

	m := e.mark()
	got, err := e.callStatus(t, id)
	wantNoErr(t, "读回落库结果", err)
	wantOps(t, "读回落库结果", e.ops(m), []string{"deliv.FindOne:" + id})
	wantTrue(t, "读回落库结果", "found", got.GetFound())
	d := got.GetDelivery()
	wantEQ(t, "读回落库结果", "state", d.GetState(), rpc.DeliveryState_DELIVERY_SENT)
	wantEQ(t, "读回落库结果", "provider", d.GetProvider(), "fake-push")
	wantEQ(t, "读回落库结果", "provider_msg_id", d.GetProviderMsgId(), "pmid-real-1")
	wantTrue(t, "读回落库结果", "sent_at 由投递写入", d.GetSentAt() > 0)
	wantTrue(t, "读回落库结果", "payload_digest 由投递写入", d.GetPayloadDigest() != "")
	calls, _ := push.Snapshot()
	wantEQ(t, "读回落库结果", "供应商只被调用一次", calls, 1)
}

// TestGetDeliveryStatusRejectsInvalidRequestsAndPropagatesStoreError 入参校验零读库；
// 读库失败必须原样上抛，绝不能退化成 found=false（那会让运营以为任务被清了）。
func TestGetDeliveryStatusRejectsInvalidRequestsAndPropagatesStoreError(t *testing.T) {
	e := newEnv(t)
	e.seedDelivery(t, statusRow("DLV-1"))

	m := e.mark()
	_, err := e.callStatus(t, "")
	wantErrContains(t, "空 delivery_id", err, "delivery_id is required")
	if _, err := NewGetDeliveryStatusLogic(context.Background(), e.svcCtx).GetDeliveryStatus(nil); err == nil {
		t.Error("nil 请求必须报错")
	} else {
		wantErrContains(t, "nil 请求", err, "nil request")
	}
	wantOps(t, "入参校验不读库", e.ops(m), nil)

	// 空格 ID 不在本方法的校验范围内（没有 Trim），会真去查一次并得到「不存在」。
	// 这里钉住真实行为：它不会被当成空串报错，也不会命中任何行。
	m2 := e.mark()
	got, err := e.callStatus(t, "   ")
	wantNoErr(t, "空白 delivery_id", err)
	wantEQ(t, "空白 delivery_id", "found", got.GetFound(), false)
	wantOps(t, "空白 delivery_id 仍查一次库", e.ops(m2), []string{"deliv.FindOne:   "})

	down := errors.New("notification_delivery FindOne: db down")
	e.delivSpy.Fail("FindOne", down)
	m3 := e.mark()
	reply, err := e.callStatus(t, "DLV-1")
	wantErrIs(t, "投递表读失败", err, down)
	if reply != nil {
		t.Errorf("读库失败时不得返回 found=false 冒充「不存在」，实际 %+v", reply)
	}
	wantOps(t, "投递表读失败", e.ops(m3), []string{"deliv.FindOne:DLV-1"})
}
