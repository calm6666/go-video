package logic

// webhook_delivery_test.go：投递链路三法的契约测试
// （EnqueueWebhookEvent / ListWebhookDeliveries / RetryWebhookDelivery）。
//
// 契约依据：proto:566-627（入队/分页/重放三组字段）、webhookevent.go 文件头
// （两条入口共用同一门禁，内部路径不得成为绕过正文扫描的后门）、
// model/op_webhook_delivery.go:34-41（uniq_event_endpoint 幂等 + 正文只回摘要）。
//
// 本文件钉住的不变量：
//  1. 扇出规模：一个可投递端点一行，matched == 实际入队行数（== delivery_ids 长度），
//     未验证/停用/已删/订别的事件的行一律不计入，且 matched 不得在入队前就被写成端点数；
//  2. 幂等：同 (event_id, endpoint_id) 重放只回既有 ID、既不新增行也不覆盖已入库正文，
//     新增端点后再重放只补那一行（Kafka at-least-once 的全部依赖）；
//  3. 正文门禁按配置生效：字节上限 = WebhookPayloadMaxBytes（边界两侧各测一次），
//     凭证键名与 PII 一律拒整条而不是截断；失败路径零行；
//  4. 台账不回显正文：ListWebhookDeliveries 只给 payload_digest，库里正文仍在（重放要用）；
//  5. 复合序 (ctime DESC, delivery_id DESC) 必须被真实钉住：构造与主键不同向的时间 +
//     一对同 ctime 的行，否则「按主键排也能过」；
//  6. 重放只承认 DEAD/IGNORED，DEAD 还要 ignore_dead 显式确认；重放把 attempt 归零、
//     清租约，但 max_attempts 是入队快照不得被抬高；成功后行变 PENDING，故不可连环重放；
//  7. 重放与 worker 抢占互斥：CAS 落败方必须回库里真值（DELIVERING）而不是自己希望的 PENDING，
//     且绝不把抢占者的 attempt/lease 抹掉；
//  8. 内部撤销通知尽力而为：入队失败绝不能把已提交的撤销事务改成失败。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"
)

const (
	// evtBodyMarker 投递正文里的哨兵串：出现在任何响应里就说明台账把正文读出来了。
	evtBodyMarker = "BODY-MUST-NOT-LEAVE-DB"
	// evtTooOldSeconds 比 webhookLookbackSeconds 更旧的安全量（见该用例注释）。
	evtTooOldSeconds = 3600
)

// ---------------------------------------------------------------- 调用与构造小工具

func callEnqueueWebhookEvent(t *testing.T, s *svc.ServiceContext,
	in *rpc.EnqueueWebhookEventReq) (*rpc.EnqueueWebhookEventReply, error) {
	t.Helper()
	return NewEnqueueWebhookEventLogic(t.Context(), s).EnqueueWebhookEvent(in)
}

func callListWebhookDeliveries(t *testing.T, s *svc.ServiceContext,
	in *rpc.ListWebhookDeliveriesReq) (*rpc.ListWebhookDeliveriesReply, error) {
	t.Helper()
	return NewListWebhookDeliveriesLogic(t.Context(), s).ListWebhookDeliveries(in)
}

func callRetryWebhookDelivery(t *testing.T, s *svc.ServiceContext,
	in *rpc.RetryWebhookDeliveryReq) (*rpc.RetryWebhookDeliveryReply, error) {
	t.Helper()
	return NewRetryWebhookDeliveryLogic(t.Context(), s).RetryWebhookDelivery(in)
}

// enqueueReq 一条合法的入队请求基线，用例按需要改单个字段。
func enqueueReq(appID int64, eventType rpc.WebhookEventType, eventID, payload string,
	occurredAt int64) *rpc.EnqueueWebhookEventReq {
	return &rpc.EnqueueWebhookEventReq{
		AppId: appID, EventType: eventType, EventId: eventID, Payload: payload,
		OccurredAt: occurredAt, Mid: testMid, BizType: "video", BizId: "aid-1",
	}
}

// retryReq 重放请求基线：reason 必填、DEAD 需显式确认位。
func retryReq(deliveryID, operator int64, ignoreDead bool, reason string) *rpc.RetryWebhookDeliveryReq {
	return &rpc.RetryWebhookDeliveryReq{
		DeliveryId: deliveryID, OperatorMid: operator, IgnoreDead: ignoreDead, Reason: reason,
	}
}

// deliveryRow 按主键取内存里的投递行（找不到返回 nil）：断言「响应 == 入库真值」用。
func deliveryRow(db *store, deliveryID int64) *model.WebhookDelivery {
	for _, d := range db.dels {
		if d.DeliveryID == deliveryID {
			return d
		}
	}
	return nil
}

// deliveriesFor 该端点该事件的行数：扇出与去重都只看这个数。
func deliveriesFor(db *store, endpointID int64, eventID string) int {
	var n int
	for _, d := range db.dels {
		if d.EndpointID == endpointID && d.EventID == eventID {
			n++
		}
	}
	return n
}

// assertFreshDeliveryTruth 逐字段核对「新入队一行」应有的真值。
// 只断言 err==nil 等于什么都没测：这一行是后续退避、签名与死信的全部输入。
func assertFreshDeliveryTruth(t *testing.T, d *model.WebhookDelivery, appID, endpointID int64,
	eventType int32, eventID, payload string, occurredAt int64, maxAttempts int32, label string) {
	t.Helper()
	if d == nil {
		t.Fatalf("%s：投递行不存在", label)
	}
	wantDigest := model.DigestPayload(payload)
	if d.AppID != appID || d.EndpointID != endpointID || d.EventType != eventType || d.EventID != eventID {
		t.Fatalf("%s：归属/事件错位 app=%d ep=%d ev=%d eid=%s", label,
			d.AppID, d.EndpointID, d.EventType, d.EventID)
	}
	if d.Payload != payload {
		t.Fatalf("%s：入库正文与门禁后的正文不一致\n门禁 %q\n入库 %q", label, payload, d.Payload)
	}
	if d.PayloadDigest != wantDigest {
		t.Fatalf("%s：摘要=%q，期望 sha256(入库正文)=%q", label, d.PayloadDigest, wantDigest)
	}
	if d.State != model.DeliveryStatePending || d.Attempt != 0 {
		t.Fatalf("%s：新任务必须从 PENDING/attempt=0 起算，实际 state=%d attempt=%d",
			label, d.State, d.Attempt)
	}
	if d.MaxAttempts != maxAttempts {
		t.Fatalf("%s：max_attempts=%d，期望入队时的配置快照 %d", label, d.MaxAttempts, maxAttempts)
	}
	// next_retry_at 播种成 occurred_at：事件发生即刻可投，重试链从这一刻起算。
	if d.NextRetryAt != occurredAt {
		t.Fatalf("%s：next_retry_at=%d，期望 occurred_at=%d", label, d.NextRetryAt, occurredAt)
	}
	if d.Ctime <= 0 {
		t.Fatalf("%s：ctime 未落：%d", label, d.Ctime)
	}
	if d.LeaseUntil != 0 || d.LastError != "" || d.LastStatusCode != 0 {
		t.Fatalf("%s：新任务不该带租约/错误/状态码：lease=%d err=%q code=%d",
			label, d.LeaseUntil, d.LastError, d.LastStatusCode)
	}
}

// assertDeliveryInfoMatchesRow 核对台账投影的每一个字段都等于库里的真值。
func assertDeliveryInfoMatchesRow(t *testing.T, info *rpc.WebhookDeliveryInfo,
	row *model.WebhookDelivery, label string) {
	t.Helper()
	if row == nil {
		t.Fatalf("%s：库里没有这一行，响应却给了它", label)
	}
	if info.GetDeliveryId() != row.DeliveryID || info.GetAppId() != row.AppID ||
		info.GetEndpointId() != row.EndpointID {
		t.Fatalf("%s：归属投影失真：%+v 行=%+v", label, info, row)
	}
	if info.GetEventType() != rpc.WebhookEventType(row.EventType) || info.GetEventId() != row.EventID {
		t.Fatalf("%s：事件投影失真：%s vs %s", label, info.GetEventId(), row.EventID)
	}
	if info.GetState() != rpc.WebhookDeliveryState(row.State) || info.GetAttempt() != row.Attempt ||
		info.GetMaxAttempts() != row.MaxAttempts || info.GetNextRetryAt() != row.NextRetryAt {
		t.Fatalf("%s：状态机投影失真 state=%d/%d attempt=%d/%d max=%d/%d next=%d/%d", label,
			info.GetState(), row.State, info.GetAttempt(), row.Attempt,
			info.GetMaxAttempts(), row.MaxAttempts, info.GetNextRetryAt(), row.NextRetryAt)
	}
	if info.GetPayloadDigest() != row.PayloadDigest || info.GetLastError() != row.LastError ||
		info.GetLastStatusCode() != row.LastStatusCode || info.GetCtime() != row.Ctime ||
		info.GetMtime() != row.Mtime {
		t.Fatalf("%s：摘要/错误/时间投影失真：%+v", label, info)
	}
}

// ---------------------------------------------------------------- ListMatching 替身

// matchingStub 覆写 ListMatching，其余方法委托给原 fake（内嵌接口值）。
//
// 为什么需要它：本包 fake 无条件按 app_id 过滤，表达不出 model/op_webhook_endpoint.go:204
// 的「appID=0 即平台级广播（不加 app_id 条件）」；另外「未验证永不投递」在 logic 里
// 是第二道判据（webhookevent.go:96），fake 忠实过滤后那条分支根本走不到。
// 两处都需要一个可控的 ListMatching，而不是去改共享 fake 的语义。
type matchingStub struct {
	model.WebhookEndpointModel
	db *store
	// faithful=true 复刻真 SQL（广播 + Deliverable 过滤 + endpoint_id ASC）；
	// false 则把自己给的行原样交出去，用来模拟「SQL 被改写后放进了不可投递行」。
	faithful bool
	leak     []*model.WebhookEndpoint
}

func (m matchingStub) ListMatching(_ context.Context, appID int64,
	eventType int32) ([]*model.WebhookEndpoint, error) {
	if err := m.db.hit("WebhookEndpoints.ListMatching"); err != nil {
		return nil, err
	}
	if !m.faithful {
		return m.leak, nil
	}
	var out []*model.WebhookEndpoint
	for _, ep := range m.db.eps {
		// 与真实现同一条 WHERE：appID=0 时不加 app_id 条件。
		if (appID > 0 && ep.AppID != appID) || ep.EventType != eventType || !ep.Deliverable() {
			continue
		}
		c := *ep
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EndpointID < out[j].EndpointID })
	return out, nil
}

// ---------------------------------------------------------------- 入队：正常扇出

func TestEnqueueWebhookEvent_FanOutWritesOneRowPerDeliverableEndpoint(t *testing.T) {
	db, s := webhookFixture(t)
	hookA := seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult, hookURL, true, true, 0)
	hookB := seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult, hookURL2, true, true, 0)
	// 四类「不该投递」的形态各自成因不同，都必须被排除在 matched 之外。
	seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult,
		"https://unverified.example.test/v1", false, true, 0)
	seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult,
		"https://disabled.example.test/v1", true, false, 0)
	seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult,
		"https://deleted.example.test/v1", true, true, nowTS())
	seedEndpoint(db, testAppID, model.WebhookEventQuotaWarning) // 订的是别的事件
	seedEndpoint(db, testApp2, model.WebhookEventContentPublishResult)

	occurred := nowTS() - 5
	raw := "\n {\"aid\":9001,\"title\":\"投稿完成\"} \t"
	normalized := strings.TrimSpace(raw)
	before := snapshotWrites(db)
	matchedBefore := db.count("WebhookEndpoints.ListMatching")

	reply, err := callEnqueueWebhookEvent(t, s, enqueueReq(testAppID,
		rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT, "publish:9001", raw, occurred))
	reply = wantOK(t, reply, err, "两个端点订阅同一事件")

	if reply.GetMatchedEndpoints() != 2 || len(reply.GetDeliveryIds()) != 2 {
		t.Fatalf("matched=%d ids=%v，期望 2 行扇出", reply.GetMatchedEndpoints(), reply.GetDeliveryIds())
	}
	if reply.GetDeduplicated() {
		t.Fatal("首次入队不得报 deduplicated=true，生产方据此判断是否补投")
	}
	if len(db.dels) != 2 {
		t.Fatalf("投递行数=%d，期望 2：%+v", len(db.dels), db.dels)
	}
	// 真实现按 endpoint_id ASC 入队（ListMatching 的 ORDER BY），响应顺序必须与之一致。
	for i, wantEP := range []int64{hookA.EndpointID, hookB.EndpointID} {
		if reply.GetDeliveryIds()[i] != db.dels[i].DeliveryID || db.dels[i].EndpointID != wantEP {
			t.Fatalf("第 %d 行 id=%d ep=%d，期望 ep=%d", i, reply.GetDeliveryIds()[i],
				db.dels[i].EndpointID, wantEP)
		}
		assertFreshDeliveryTruth(t, db.dels[i], testAppID, wantEP,
			model.WebhookEventContentPublishResult, "publish:9001", normalized, occurred,
			s.Config.OpenPlatform.WebhookMaxAttempts, fmt.Sprintf("扇出第 %d 行", i))
	}
	// 一次入队只扫一次端点表：读面不得退化成「每端点一次查询」。
	wantCalls(t, db, "WebhookEndpoints.ListMatching", matchedBefore, 1, "一次入队一次匹配")
	// 写侧只发生 WebhookDeliveries.Insert，且恰好 2 次。
	wantOnlyWriteAttempts(t, db, before, map[string]int{
		"WebhookDeliveries.Insert": 2,
	}, "入队只写投递表")

	// max_attempts 是「入队时」的配置快照：调参必须改下一批而不是硬编码 6。
	s.Config.OpenPlatform.WebhookMaxAttempts = 3
	before = snapshotWrites(db)
	reply, err = callEnqueueWebhookEvent(t, s, enqueueReq(testAppID,
		rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT, "publish:9002", raw, occurred))
	reply = wantOK(t, reply, err, "改配置后再入队")
	if len(db.dels) != 4 {
		t.Fatalf("投递行数=%d，期望 4", len(db.dels))
	}
	for _, d := range db.dels[2:] {
		if d.MaxAttempts != 3 || d.EventID != "publish:9002" {
			t.Fatalf("配置未生效：max=%d eid=%s", d.MaxAttempts, d.EventID)
		}
	}
	assertFreshDeliveryTruth(t, db.dels[2], testAppID, hookA.EndpointID,
		model.WebhookEventContentPublishResult, "publish:9002", normalized, occurred, 3, "配置快照")
	// 前两条不被后续入队改动。
	if db.dels[0].MaxAttempts != 6 || db.dels[1].MaxAttempts != 6 {
		t.Fatal("既有行的 max_attempts 被后续事件改写：退避预算必须逐事件冻结")
	}
}

// ---------------------------------------------------------------- 入队：幂等与去重

func TestEnqueueWebhookEvent_SameEventIDReplayKeepsSingleRowAndEchoesExistingIDs(t *testing.T) {
	db, s := webhookFixture(t)
	hookA := seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult, hookURL, true, true, 0)
	hookB := seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult, hookURL2, true, true, 0)
	occurred := nowTS() - 5
	first := `{"aid":7,"v":1}`
	// 第二次带「修订过的正文」撞同一 event_id：绝不能覆盖已入库的正文与摘要，
	// 否则应用侧按旧正文算的签名与台账对不上，重放也会投出一份没生产过的事件。
	second := `{"aid":7,"v":2}`

	r1, err := callEnqueueWebhookEvent(t, s, enqueueReq(testAppID,
		rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT, "publish:7", first, occurred))
	r1 = wantOK(t, r1, err, "首次入队")
	if r1.GetMatchedEndpoints() != 2 || len(r1.GetDeliveryIds()) != 2 || r1.GetDeduplicated() {
		t.Fatalf("首次入队 matched=%d ids=%v dedup=%t", r1.GetMatchedEndpoints(),
			r1.GetDeliveryIds(), r1.GetDeduplicated())
	}
	idsBefore := append([]int64(nil), r1.GetDeliveryIds()...)
	stateBefore := deliveryState(db)

	before := snapshotWrites(db)
	r2, err := callEnqueueWebhookEvent(t, s, enqueueReq(testAppID,
		rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT, "publish:7", second, occurred))
	r2 = wantOK(t, r2, err, "同 event_id 重放入队")
	if !r2.GetDeduplicated() {
		t.Fatal("uniq_event_endpoint 命中却未报 deduplicated：Kafka 重放会以为没撞车")
	}
	if r2.GetMatchedEndpoints() != 2 || len(r2.GetDeliveryIds()) != 2 {
		t.Fatalf("重放 matched=%d ids=%v，期望回既有 2 行", r2.GetMatchedEndpoints(), r2.GetDeliveryIds())
	}
	for i, id := range idsBefore {
		if r2.GetDeliveryIds()[i] != id {
			t.Fatalf("重放回了新的 delivery_id：[%d]=%d，期望 %d", i, r2.GetDeliveryIds()[i], id)
		}
	}
	if len(db.dels) != 2 {
		t.Fatalf("重放后行数=%d，期望 2（一行都不能多）", len(db.dels))
	}
	assertDeliveryStateUnchanged(t, db, stateBefore, "重放不得覆盖已入库正文/摘要/状态")
	// 撞唯一键仍是「一次写尝试」，但没有产生第二行：行数与内容都由上面的快照钉住。
	wantOnlyWriteAttempts(t, db, before, map[string]int{
		"WebhookDeliveries.Insert": 2,
	}, "重放只对既有键各撞一次")

	// 新端点订阅后重放同一事件：只补那一行，另两行不动。
	hookC := seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult,
		"https://hook3.example.test/v1", true, true, 0)
	stateBefore = deliveryState(db)
	r3, err := callEnqueueWebhookEvent(t, s, enqueueReq(testAppID,
		rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT, "publish:7", second, occurred))
	r3 = wantOK(t, r3, err, "补一个端点后重放")
	if r3.GetMatchedEndpoints() != 3 || len(db.dels) != 3 || !r3.GetDeduplicated() {
		t.Fatalf("补投 matched=%d 行数=%d dedup=%t，期望 3/3/true",
			r3.GetMatchedEndpoints(), len(db.dels), r3.GetDeduplicated())
	}
	if db.dels[2].EndpointID != hookC.EndpointID || db.dels[2].Payload != second {
		t.Fatalf("补的那一行不对：ep=%d payload=%q", db.dels[2].EndpointID, db.dels[2].Payload)
	}
	// 前两行原样：补投不能把已投递中的任务重置回 PENDING。
	for id, want := range stateBefore {
		if got := deliveryState(db)[id]; got != want {
			t.Fatalf("补投改了既有投递 %d：\n改前 %s\n改后 %s", id, want, got)
		}
	}

	// 不同 event_id、同端点：不是重放，必须各成一行。
	if _, err := callEnqueueWebhookEvent(t, s, enqueueReq(testAppID,
		rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT, "publish:8", first, occurred)); err != nil {
		t.Fatalf("新事件入队失败：%v", err)
	}
	for _, ep := range []*model.WebhookEndpoint{hookA, hookB, hookC} {
		if n := deliveriesFor(db, ep.EndpointID, "publish:8"); n != 1 {
			t.Fatalf("新事件在端点 %d 上行数=%d，期望 1", ep.EndpointID, n)
		}
	}
	// 行数守恒：2（首发）+1（补投）+3（新事件按三个端点扇出）= 6，且 delivery_id 互不重复。
	if len(db.dels) != 6 {
		t.Fatalf("投递行数=%d，期望 6", len(db.dels))
	}
	seen := map[int64]bool{}
	for _, d := range db.dels {
		if seen[d.DeliveryID] {
			t.Fatalf("同一 delivery_id 出现两次：%d", d.DeliveryID)
		}
		seen[d.DeliveryID] = true
	}
}

// ---------------------------------------------------------------- 入队：参数与正文门禁

func TestEnqueueWebhookEvent_GatesAreSideEffectFree(t *testing.T) {
	db, s := webhookFixture(t)
	// 先放一个可投递端点：否则「零行」会因为没人订阅而恒真。
	seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult, hookURL, true, true, 0)
	now := nowTS()
	long := strings.Repeat("e", maxEventIDRunes+1)
	longBiz := strings.Repeat("b", maxBizRefRunes+1)
	oversize := "{" + strings.Repeat(" ", int(s.Config.OpenPlatform.WebhookPayloadMaxBytes)-1) + "}"

	ev := rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT
	cases := []struct {
		name string
		mut  func(in *rpc.EnqueueWebhookEventReq)
		want error
	}{
		{"应用 ID 为负数不得当成广播", func(in *rpc.EnqueueWebhookEventReq) { in.AppId = -testAppID }, model.ErrInvalidAppID},
		{"应用不存在", func(in *rpc.EnqueueWebhookEventReq) { in.AppId = 12345 }, model.ErrAppNotFound},
		{"事件类型 UNSPECIFIED", func(in *rpc.EnqueueWebhookEventReq) {
			in.EventType = rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_UNSPECIFIED
		}, model.ErrInvalidEventType},
		{"事件类型未知值不解释成全事件", func(in *rpc.EnqueueWebhookEventReq) { in.EventType = rpc.WebhookEventType(99) }, model.ErrInvalidEventType},
		{"事件 ID 空白", func(in *rpc.EnqueueWebhookEventReq) { in.EventId = "   " }, model.ErrEventIDRequired},
		{"事件 ID 超长（幂等键不得截断）", func(in *rpc.EnqueueWebhookEventReq) { in.EventId = long }, model.ErrEventIDRequired},
		{"事件 ID 含空白（唯一键不得有歧义）", func(in *rpc.EnqueueWebhookEventReq) { in.EventId = "a b" }, model.ErrEventIDRequired},
		{"发生时间为 0", func(in *rpc.EnqueueWebhookEventReq) { in.OccurredAt = 0 }, model.ErrWindowInvalid},
		{"发生时间为负", func(in *rpc.EnqueueWebhookEventReq) { in.OccurredAt = -1 }, model.ErrWindowInvalid},
		{"发生时间超过补投窗口", func(in *rpc.EnqueueWebhookEventReq) {
			in.OccurredAt = now - webhookLookbackSeconds - evtTooOldSeconds
		}, errEventTooOld},
		{"正文为空", func(in *rpc.EnqueueWebhookEventReq) { in.Payload = "" }, errPayloadNotJSON},
		{"正文不是合法 JSON", func(in *rpc.EnqueueWebhookEventReq) { in.Payload = "aid=1" }, errPayloadNotJSON},
		{"正文超字节上限拒整条而不是截断", func(in *rpc.EnqueueWebhookEventReq) { in.Payload = oversize }, model.ErrPayloadTooBig},
		{"正文带 client_secret", func(in *rpc.EnqueueWebhookEventReq) {
			in.Payload = `{"aid":1,"client_secret":"s"}`
		}, errPayloadCarriesCredential},
		{"正文带 access_token 键名（大小写无关）", func(in *rpc.EnqueueWebhookEventReq) {
			in.Payload = `{"Access_Token":"s"}`
		}, errPayloadCarriesCredential},
		{"正文带带分隔符手机号", func(in *rpc.EnqueueWebhookEventReq) {
			in.Payload = `{"aid":1,"note":"138-0013-8000"}`
		}, errWebhookPayloadCarriesPII},
		{"正文带 18 位身份证形态", func(in *rpc.EnqueueWebhookEventReq) {
			in.Payload = `{"id_no":"110101199003072316"}`
		}, errWebhookPayloadCarriesPII},
		{"正文带联系方式键下的裸手机号", func(in *rpc.EnqueueWebhookEventReq) {
			in.Payload = `{"mobile":"13800138000"}`
		}, errWebhookPayloadCarriesPII},
		{"关联用户为负数", func(in *rpc.EnqueueWebhookEventReq) { in.Mid = -1 }, errWebhookMidInvalid},
		{"biz_type 超长拒而不是截断", func(in *rpc.EnqueueWebhookEventReq) { in.BizType = longBiz }, errWebhookBizRefTooLong},
		{"biz_id 超长拒而不是截断", func(in *rpc.EnqueueWebhookEventReq) { in.BizId = longBiz }, errWebhookBizRefTooLong},
	}
	for _, tc := range cases {
		in := enqueueReq(testAppID, ev, "publish:1", `{"aid":1}`, now-5)
		tc.mut(in)
		before := snapshotWrites(db)
		matched := db.count("WebhookEndpoints.ListMatching")
		reply, err := callEnqueueWebhookEvent(t, s, in)
		wantFail(t, reply, err, tc.want, tc.name)
		wantNoWrites(t, db, before, tc.name)
		// 门禁必须在扫端点之前：越界参数不该产生一次全表匹配。
		wantCalls(t, db, "WebhookEndpoints.ListMatching", matched, 0, tc.name)
		if len(db.dels) != 0 {
			t.Fatalf("%s：门禁拒了却留下 %d 行投递", tc.name, len(db.dels))
		}
	}

	// mid=0 是合法值（平台级事件无归属用户），别把「拒绝负数」写成「拒绝 0」。
	zero := enqueueReq(testAppID, ev, "publish:0", `{"aid":1}`, now-5)
	zero.Mid = 0
	accept(t, s, zero, "mid=0（无归属用户）")
	// 恰好 maxEventIDRunes 与 maxBizRefRunes 是允许的（只测拒绝侧会漏掉 off-by-one）。
	edge := enqueueReq(testAppID, ev, strings.Repeat("e", maxEventIDRunes), `{"aid":1}`, now-5)
	edge.BizId = strings.Repeat("b", maxBizRefRunes)
	accept(t, s, edge, "长度上界")
	// 窗口边界：比上界新即可入队（留 60 秒余量，避免与实现内部的 nowUnix 抢时钟）。
	accept(t, s, enqueueReq(testAppID, ev, "publish:edge", `{"aid":1}`,
		now-webhookLookbackSeconds+60), "补投窗口内最旧事件")
	// 补投窗口是 24 小时量级（防历史重放打爆端点），改这个常量必须让本用例变红。
	if webhookLookbackSeconds != 86400 {
		t.Fatalf("webhookLookbackSeconds=%d，契约是 86400 秒", webhookLookbackSeconds)
	}
	if len(db.dels) != 3 {
		t.Fatalf("三个正例入队后行数=%d，期望 3", len(db.dels))
	}
}

// accept 正例：必须成功且 matched==1（夹具里只有一个可投递端点）。
// 用它而不是裸调用，是为了让「放行」也带上可失败的断言。
func accept(t *testing.T, s *svc.ServiceContext, in *rpc.EnqueueWebhookEventReq, label string) {
	t.Helper()
	reply, err := callEnqueueWebhookEvent(t, s, in)
	reply = wantOK(t, reply, err, label)
	if reply.GetMatchedEndpoints() != 1 {
		t.Fatalf("%s：matched=%d，期望 1", label, reply.GetMatchedEndpoints())
	}
}

func TestEnqueueWebhookEvent_PayloadSizeGateFollowsConfiguredMax(t *testing.T) {
	db, s := webhookFixture(t)
	seedEndpointAt(db, testAppID, model.WebhookEventQuotaWarning, hookURL, true, true, 0)
	max := s.Config.OpenPlatform.WebhookPayloadMaxBytes
	if max != 32768 {
		t.Fatalf("夹具的 WebhookMaxPayloadBytes=%d，期望与 etc 示例配置一致（32768）", max)
	}
	// 纯空白填充的 {} 是合法 JSON，长度可精确控制：正好等于上限必须放行。
	atMax := "{" + strings.Repeat(" ", int(max)-2) + "}"
	overMax := "{" + strings.Repeat(" ", int(max)-1) + "}"
	if len(atMax) != int(max) || len(overMax) != int(max)+1 {
		t.Fatalf("构造失真：%d / %d", len(atMax), len(overMax))
	}
	ev := rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_QUOTA_WARNING
	now := nowTS()

	before := snapshotWrites(db)
	reply, err := callEnqueueWebhookEvent(t, s, enqueueReq(testAppID, ev, "quota:at-max", atMax, now-5))
	reply = wantOK(t, reply, err, "正文恰好等于上限")
	if reply.GetMatchedEndpoints() != 1 || len(db.dels) != 1 {
		t.Fatalf("上限内未入队：matched=%d 行数=%d", reply.GetMatchedEndpoints(), len(db.dels))
	}
	assertFreshDeliveryTruth(t, db.dels[0], testAppID, 1, model.WebhookEventQuotaWarning,
		"quota:at-max", atMax, now-5, 6, "上限内正文")

	before = snapshotWrites(db)
	reply, err = callEnqueueWebhookEvent(t, s, enqueueReq(testAppID, ev, "quota:over-max", overMax, now-5))
	wantFail(t, reply, err, model.ErrPayloadTooBig, "正文超上限 1 字节")
	wantNoWrites(t, db, before, "正文超上限")
	if len(db.dels) != 1 {
		t.Fatalf("超限正文留下了 %d 行", len(db.dels))
	}

	// 换个小上限必须立刻生效：证明门禁读的是配置而不是编译期常量。
	s.Config.OpenPlatform.WebhookPayloadMaxBytes = 64
	small := "{" + strings.Repeat(" ", 62) + "}"  // 恰好 64 字节
	tooBig := "{" + strings.Repeat(" ", 63) + "}" // 65 字节
	if len(small) != 64 || len(tooBig) != 65 {
		t.Fatalf("构造失真：%d / %d", len(small), len(tooBig))
	}
	if _, err := callEnqueueWebhookEvent(t, s, enqueueReq(testAppID, ev, "quota:64", small, now-5)); err != nil {
		t.Fatalf("64 字节在新上限内应放行：%v", err)
	}
	before = snapshotWrites(db)
	reply, err = callEnqueueWebhookEvent(t, s, enqueueReq(testAppID, ev, "quota:65", tooBig, now-5))
	wantFail(t, reply, err, model.ErrPayloadTooBig, "65 字节超新上限")
	wantNoWrites(t, db, before, "新上限拒掉的正文")
	if len(db.dels) != 2 {
		t.Fatalf("投递行数=%d，期望 2（只允许 64 字节那条进来）", len(db.dels))
	}

	// 上限漏配（<=0）必须退回 32768 的默认值，而不是「拒一切」或「不限长度」。
	s.Config.OpenPlatform.WebhookPayloadMaxBytes = 0
	if _, err := callEnqueueWebhookEvent(t, s, enqueueReq(testAppID, ev, "quota:default-max", atMax, now-5)); err != nil {
		t.Fatalf("未配上限时应回落到 32768：%v", err)
	}
	reply, err = callEnqueueWebhookEvent(t, s, enqueueReq(testAppID, ev, "quota:default-over", overMax, now-5))
	wantFail(t, reply, err, model.ErrPayloadTooBig, "未配上限时仍受默认值约束")
}

// ---------------------------------------------------------------- 入队：未验证永不投递

func TestEnqueueWebhookEvent_NeverEnqueuesUndeliverableEndpointEvenIfSQLRegresses(t *testing.T) {
	db, s := webhookFixture(t)
	occurred := nowTS() - 5
	unverified := seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult,
		"https://unverified.example.test/v1", false, true, 0)
	disabled := seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult,
		"https://disabled.example.test/v1", true, false, 0)
	deleted := seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult,
		"https://deleted.example.test/v1", true, true, nowTS())
	good := seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult, hookURL, true, true, 0)

	// leak：把 model 层未来可能被改写的风险当成已发生的事实，验证 logic 的第二道判据。
	s.WebhookEndpoints = matchingStub{WebhookEndpointModel: s.WebhookEndpoints, db: db,
		faithful: false, leak: []*model.WebhookEndpoint{unverified, disabled, deleted}}
	before := snapshotWrites(db)
	reply, err := callEnqueueWebhookEvent(t, s, enqueueReq(testAppID,
		rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT, "leak:1", `{"aid":1}`, occurred))
	reply = wantOK(t, reply, err, "全部不可投递时仍是成功（未订阅不是错误）")
	if reply.GetMatchedEndpoints() != 0 || len(reply.GetDeliveryIds()) != 0 {
		t.Fatalf("不可投递端点被计入 matched=%d ids=%v：未验证地址一旦可投就是 SSRF 跳板",
			reply.GetMatchedEndpoints(), reply.GetDeliveryIds())
	}
	wantNoWrites(t, db, before, "不可投递端点零写")
	if len(db.dels) != 0 {
		t.Fatalf("留下了 %d 行投递", len(db.dels))
	}

	// 混合：只有可投递那一行入队，matched 取实际入队数而不是端点数。
	s.WebhookEndpoints = matchingStub{WebhookEndpointModel: s.WebhookEndpoints, db: db,
		faithful: false, leak: []*model.WebhookEndpoint{unverified, good, deleted, disabled}}
	reply, err = callEnqueueWebhookEvent(t, s, enqueueReq(testAppID,
		rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT, "leak:2", `{"aid":1}`, occurred))
	reply = wantOK(t, reply, err, "混合列表")
	if reply.GetMatchedEndpoints() != 1 || len(db.dels) != 1 ||
		db.dels[0].EndpointID != good.EndpointID {
		t.Fatalf("matched=%d 行数=%d ep=%d，期望 1/1/%d", reply.GetMatchedEndpoints(),
			len(db.dels), db.dels[0].EndpointID, good.EndpointID)
	}
}

func TestEnqueueWebhookEvent_PlatformBroadcastAttributesRowToOwningApp(t *testing.T) {
	db, s := webhookFixture(t)
	// app_id=0 的广播不做应用状态门禁（平台级事件不属于任何单个应用），
	// 所以这里刻意把一个应用置为停用，验证「入队照旧、重放另判」。
	susp := db.apps[testApp2]
	susp.Status = model.AppStatusSuspended
	hook1 := seedEndpointAt(db, testAppID, model.WebhookEventContentOffline, hookURL, true, true, 0)
	hook2 := seedEndpointAt(db, testApp2, model.WebhookEventContentOffline, hookURL2, true, true, 0)
	hook3 := seedEndpointAt(db, testApp2, model.WebhookEventContentOffline,
		"https://hook3.example.test/v1", true, true, 0)
	seedEndpointAt(db, testAppID, model.WebhookEventContentOffline,
		"https://unverified.example.test/v1", false, true, 0)
	s.WebhookEndpoints = matchingStub{WebhookEndpointModel: s.WebhookEndpoints, db: db, faithful: true}
	occurred := nowTS() - 5

	in := enqueueReq(0, rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_OFFLINE, "offline:aid9",
		`{"aid":9}`, occurred)
	in.Mid = 0
	reply, err := callEnqueueWebhookEvent(t, s, in)
	reply = wantOK(t, reply, err, "平台级广播入队")
	if reply.GetMatchedEndpoints() != 3 || len(reply.GetDeliveryIds()) != 3 {
		t.Fatalf("广播 matched=%d ids=%v，期望 3 行（含停用应用的两个端点）",
			reply.GetMatchedEndpoints(), reply.GetDeliveryIds())
	}
	// 关键归属：广播事件的每一行都记在端点所属应用名下，否则台账串了应用，
	// 而 ListWebhookDeliveries 也就成了跨应用枚举接口。
	own := map[int64]int64{hook1.EndpointID: testAppID, hook2.EndpointID: testApp2,
		hook3.EndpointID: testApp2}
	for _, d := range db.dels {
		if want, ok := own[d.EndpointID]; !ok || d.AppID != want {
			t.Fatalf("广播行 ep=%d 记成 app=%d，期望 app=%d", d.EndpointID, d.AppID, want)
		}
	}
	// 真实现按 endpoint_id ASC 入队。
	for i, want := range []int64{hook1.EndpointID, hook2.EndpointID, hook3.EndpointID} {
		if db.dels[i].EndpointID != want {
			t.Fatalf("第 %d 行 ep=%d，期望 %d（ListMatching 按 endpoint_id 升序）",
				i, db.dels[i].EndpointID, want)
		}
	}

	// 读侧隔离：A 应用只能看到自己那一条，B 的两条不可见。
	listA, err := callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{AppId: testAppID})
	listA = wantOK(t, listA, err, "A 看广播台账")
	if len(listA.GetList()) != 1 || listA.GetList()[0].GetEndpointId() != hook1.EndpointID {
		t.Fatalf("A 应用看到 %d 行：%+v", len(listA.GetList()), listA.GetList())
	}
	listB, err := callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{AppId: testApp2})
	listB = wantOK(t, listB, err, "B 看广播台账")
	if len(listB.GetList()) != 2 {
		t.Fatalf("B 应用看到 %d 行，期望 2", len(listB.GetList()))
	}

	// 没有任何端点订阅该事件：matched=0 是正常态，不是错误（生产方自行决定）。
	reply, err = callEnqueueWebhookEvent(t, s, enqueueReq(testAppID,
		rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_QUOTA_WARNING, "quota:none", `{"aid":1}`, occurred))
	reply = wantOK(t, reply, err, "无订阅者")
	if reply.GetMatchedEndpoints() != 0 || len(reply.GetDeliveryIds()) != 0 || reply.GetDeduplicated() {
		t.Fatalf("无订阅者返回 matched=%d ids=%v dedup=%t", reply.GetMatchedEndpoints(),
			reply.GetDeliveryIds(), reply.GetDeduplicated())
	}
}

// ---------------------------------------------------------------- 入队：故障与 fail closed

func TestEnqueueWebhookEvent_FailClosedOnPepperConfigAndDependency(t *testing.T) {
	db, s := webhookFixture(t)
	occurred := nowTS() - 5
	ev := rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT
	req := func(id string) *rpc.EnqueueWebhookEventReq {
		return enqueueReq(testAppID, ev, id, `{"aid":1}`, occurred)
	}

	// 签名派生根缺失：宁可不入队也不要制造一批注定验签失败的任务。
	s.Config.Security.WebhookMasterPepper = ""
	before := snapshotWrites(db)
	matched := db.count("WebhookEndpoints.ListMatching")
	reply, err := callEnqueueWebhookEvent(t, s, req("pp:1"))
	wantFail(t, reply, err, model.ErrSecretVerificationUnavailable, "缺 pepper")
	wantNoWrites(t, db, before, "缺 pepper")
	wantCalls(t, db, "WebhookEndpoints.ListMatching", matched, 0, "缺 pepper 不扫端点表")
	s.Config.Security.WebhookMasterPepper = testWebhookPepper

	seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult, hookURL, true, true, 0)

	// max_attempts 未配：投出去也没有退避预算，等于任务永远悬着——必须在匹配到端点后拒。
	s.Config.OpenPlatform.WebhookMaxAttempts = 0
	before = snapshotWrites(db)
	reply, err = callEnqueueWebhookEvent(t, s, req("ma:1"))
	wantFail(t, reply, err, model.ErrInvalidStateTransition, "max_attempts 未配且有订阅者")
	wantNoWrites(t, db, before, "max_attempts 未配")
	if len(db.dels) != 0 {
		t.Fatalf("留下了 %d 行", len(db.dels))
	}
	s.Config.OpenPlatform.WebhookMaxAttempts = 6

	// 端点匹配失败必须原样上抛（回空 matched 等于把故障说成「没人订阅」）。
	db.failOn("WebhookEndpoints.ListMatching", errFakeDown)
	before = snapshotWrites(db)
	reply, err = callEnqueueWebhookEvent(t, s, req("dep:1"))
	wantFail(t, reply, err, errFakeDown, "扫端点失败")
	wantNoWrites(t, db, before, "扫端点失败")
	db.failOn("WebhookEndpoints.ListMatching", nil)

	// 入队中途失败：已经失败的那一次是唯一写尝试，且不能吞掉错误继续扇出。
	seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult, hookURL2, true, true, 0)
	db.failOn("WebhookDeliveries.Insert", errFakeDown)
	before = snapshotWrites(db)
	reply, err = callEnqueueWebhookEvent(t, s, req("ins:1"))
	wantFail(t, reply, err, errFakeDown, "入队写失败")
	wantOnlyWriteAttempts(t, db, before, map[string]int{
		"WebhookDeliveries.Insert": 1,
	}, "写失败即中止，不重试不吞错")
	if len(db.dels) != 0 {
		t.Fatalf("写失败却留下 %d 行", len(db.dels))
	}
	db.failOn("WebhookDeliveries.Insert", nil)

	// 限流发生在扫端点之前：被拒的请求不得留下任何读放大。
	limited := limitedSvc(db)
	limited.WebhookEndpoints = s.WebhookEndpoints
	before = snapshotWrites(db)
	matched = db.count("WebhookEndpoints.ListMatching")
	reply, err = callEnqueueWebhookEvent(t, limited, req("rl:1"))
	wantFail(t, reply, err, model.ErrRateLimited, "写侧限流")
	wantNoWrites(t, db, before, "写侧限流")
	wantCalls(t, db, "WebhookEndpoints.ListMatching", matched, 0, "限流先于扫端点")
}

func TestEnqueueWebhookEvent_FailureMustNotBlockGrantRevoke(t *testing.T) {
	db, s := webhookFixture(t)
	// 撤销链路里的内部通知走同一个 enqueueWebhookEvent（webhookevent.go:152）。
	// 这里的契约是「尽力而为」：入队失败绝不能把已提交的撤销事务改成失败——
	// 撤销必须成功，否则用户的「已撤销」状态会被一次外呼故障回滚。
	ep := seedEndpointAt(db, testAppID, model.WebhookEventGrantRevoked, hookURL, true, true, 0)
	g1 := seedGrant(db, testAppID, testMid, []string{testScopeR}, 1)
	g2 := seedGrant(db, testAppID, testMid+1, []string{testScopeR}, 1)

	db.failOn("WebhookDeliveries.Insert", errFakeDown)
	reply, err := callRevokeGrant(t, s, testMid)
	reply = wantOK(t, reply, err, "入队故障下撤销仍要成功")
	if reply.GetGrantsRevoked() != 1 {
		t.Fatalf("grants_revoked=%d，期望 1", reply.GetGrantsRevoked())
	}
	if g := db.grants[g1.GrantID]; g.RevokedAt == 0 || g.Status != model.GrantStatusRevoked {
		t.Fatalf("通知失败连带撤销没生效：revoked_at=%d status=%d", g.RevokedAt, g.Status)
	}
	if len(db.dels) != 0 {
		t.Fatalf("入队失败却留下了 %d 行", len(db.dels))
	}
	db.failOn("WebhookDeliveries.Insert", nil)

	// 故障修好后，下一笔撤销的通知正常入队：证明失败点确实只在通知侧。
	before := nowUnix()
	reply, err = callRevokeGrant(t, s, testMid+1)
	reply = wantOK(t, reply, err, "故障恢复后撤销")
	if len(db.dels) != 1 {
		t.Fatalf("恢复后通知行数=%d，期望 1", len(db.dels))
	}
	row := db.dels[0]
	// 正文是拼出来的，构造必须逐字节钉住：字段缺一个，应用侧就解不出「哪条授权没了」。
	wantPayload := fmt.Sprintf(`{"grant_id":%d,"app_id":%d,"mid":%d,"reason":"用户主动撤销"}`,
		g2.GrantID, testAppID, testMid+1)
	if row.AppID != testAppID || row.EndpointID != ep.EndpointID ||
		row.EventType != model.WebhookEventGrantRevoked ||
		row.EventID != "grant-revoked:"+strconv.FormatInt(g2.GrantID, 10) {
		t.Fatalf("通知归属/事件错位：app=%d ep=%d ev=%d eid=%s",
			row.AppID, row.EndpointID, row.EventType, row.EventID)
	}
	if row.Payload != wantPayload {
		t.Fatalf("撤销通知正文与契约不符\n实际 %q\n期望 %q", row.Payload, wantPayload)
	}
	if row.PayloadDigest != model.DigestPayload(wantPayload) {
		t.Fatalf("摘要不是入库正文的摘要：%s", row.PayloadDigest)
	}
	if row.State != model.DeliveryStatePending || row.Attempt != 0 || row.MaxAttempts != 6 {
		t.Fatalf("通知必须从 PENDING/attempt=0/max=6 起算：state=%d attempt=%d max=%d",
			row.State, row.Attempt, row.MaxAttempts)
	}
	// occurred_at = 通知产生那一刻（内部路径没有「事件更早发生」的概念），必须落在本次调用附近。
	if got := row.NextRetryAt; got < before || got > nowUnix()+1 {
		t.Fatalf("next_retry_at=%d 不在本次撤销的时间窗 [%d,%d] 内", got, before, nowUnix()+1)
	}
	// 正文只允许 ID 与原因：既不能带凭证字段，也不能带签名派生根。
	if payloadHasCredentialKey(row.Payload) || !json.Valid([]byte(row.Payload)) {
		t.Fatalf("撤销通知正文不合法或带凭证：%s", row.Payload)
	}
	if strings.Contains(row.Payload, testWebhookPepper) || strings.Contains(row.Payload, testPepper) {
		t.Fatal("撤销通知正文里出现了密钥材料")
	}

	// 自由文本必须被转义进 JSON：否则整条通知会被 normalizeEventPayload 判非法而静默丢失。
	g3 := seedGrant(db, testAppID, testMid+2, []string{testScopeR}, 1)
	rough := "违规\t\"引号\" 与 \n 换行"
	if _, err := callRevokeGrant(t, s, testMid+2, rough); err != nil {
		t.Fatalf("含控制字符的原因撤销失败：%v", err)
	}
	if len(db.dels) != 2 {
		t.Fatalf("转义后的正文未入队，行数=%d", len(db.dels))
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(db.dels[1].Payload), &doc); err != nil {
		t.Fatalf("撤销通知正文不是合法 JSON：%v\n%s", err, db.dels[1].Payload)
	}
	if doc["reason"] != rough || int64(doc["grant_id"].(float64)) != g3.GrantID {
		t.Fatalf("正文转义后取不回原值：reason=%q grant_id=%v", doc["reason"], doc["grant_id"])
	}
}

// callRevokeGrant 撤销「testMid 对 testAppID」的全部授权：内部通知的触发点。
// 目标用 mid 定位，因为 revokeGrant 读的正是 (app_id, mid) 唯一键，
// 传 grant_id 进来只会让这个参数变成摆设。
func callRevokeGrant(t *testing.T, s *svc.ServiceContext, mid int64,
	reason ...string) (*rpc.RevokeAuthorizationReply, error) {
	t.Helper()
	r := "用户主动撤销"
	if len(reason) > 0 {
		r = reason[0]
	}
	return NewRevokeAuthorizationLogic(t.Context(), s).RevokeAuthorization(
		&rpc.RevokeAuthorizationReq{
			Target: rpc.RevokeTarget_REVOKE_TARGET_GRANT, AppId: testAppID, Mid: mid,
			OperatorMid: mid, Reason: r,
		})
}

// ---------------------------------------------------------------- 台账分页

func TestListWebhookDeliveries_CursorWalkPinsCompositeOrderAndNeverReturnsBody(t *testing.T) {
	db, s := webhookFixture(t)
	ep := seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult, hookURL, true, true, 0)
	base := nowTS()
	// ctime 刻意与播种顺序（=主键顺序）不同向，并留一对同 ctime 的行：
	// 前者挡住「按 delivery_id 排也能蒙过」，后者挡住「只按 ctime 排、tie-break 丢了」。
	// 真实排序口径见 model/op_webhook_delivery.go:393-400 的 ListByCursor：
	// ORDER BY ctime DESC, delivery_id DESC，位点条件 (ctime < ? OR (ctime = ? AND delivery_id < ?))。
	ctimes := []int64{base - 30, base - 10, base - 70, base - 10, base - 20}
	states := []int32{model.DeliveryStatePending, model.DeliveryStateSuccess, model.DeliveryStateDead,
		model.DeliveryStateRetryScheduled, model.DeliveryStateIgnored}
	var rows []*model.WebhookDelivery
	for i, ct := range ctimes {
		d := seedDelivery(db, testAppID, ep, fmt.Sprintf("page:%d", i), states[i],
			fmt.Sprintf(`{"aid":%d,"body":"%s-%d"}`, i, evtBodyMarker, i))
		// seedDelivery 一律给 ctime=now，排序键必须逐行改成上面那条不同向的序列。
		d.Ctime, d.Mtime = ct, ct
		d.Attempt = int32(i)
		d.LastStatusCode = int64(500 + i)
		d.LastError = fmt.Sprintf("upstream refused %d", i)
		rows = append(rows, d)
	}
	// 手工排出的期望序：同为 base-10 的两行里 delivery_id 大者先（rows[3] 再 rows[1]），
	// 之后 base-20、base-30、base-70。
	wantOrder := []int64{rows[3].DeliveryID, rows[1].DeliveryID, rows[4].DeliveryID,
		rows[0].DeliveryID, rows[2].DeliveryID}

	var walk []int64
	cursor := ""
	for page := 0; page < 6; page++ {
		before := snapshotWrites(db)
		opened := db.count("WebhookDeliveries.ListByCursor")
		reply, err := callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{
			AppId: testAppID, OperatorMid: hookOperator, Ps: 2, Cursor: cursor})
		reply = wantOK(t, reply, err, fmt.Sprintf("第 %d 页", page+1))
		if len(reply.GetList()) > 2 {
			t.Fatalf("第 %d 页 n=%d 超过 ps=2", page+1, len(reply.GetList()))
		}
		for _, info := range reply.GetList() {
			walk = append(walk, info.GetDeliveryId())
			assertDeliveryInfoMatchesRow(t, info, deliveryRow(db, info.GetDeliveryId()),
				fmt.Sprintf("第 %d 页 delivery_id=%d", page+1, info.GetDeliveryId()))
		}
		// 每页只问一次：读面不得退化成「逐行再查一次」。
		wantCalls(t, db, "WebhookDeliveries.ListByCursor", opened, 1, "每页一次查询")
		wantNoWrites(t, db, before, "翻页全程不写库")
		// 台账不回正文：正文只以摘要出现（重放要用的正文仍留在库里，见下面的对照）。
		mustNoPlaintextInReply(t, reply, "投递台账", evtBodyMarker)
		cursor = reply.GetNextCursor()
		if !reply.GetHasMore() {
			break
		}
		if cursor == "" {
			t.Fatal("has_more=true 却不给位点")
		}
	}
	if len(walk) != 5 {
		t.Fatalf("翻页共取 %d 行，期望 5：%v", len(walk), walk)
	}
	for i := range walk {
		if walk[i] != wantOrder[i] {
			t.Fatalf("第 %d 行 delivery_id=%d，期望 %d（复合序 ctime DESC, delivery_id DESC）",
				i, walk[i], wantOrder[i])
		}
	}
	// 位点走完再续一页：必须是空结果而不是回到首页。
	tail, err := callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{
		AppId: testAppID, Ps: 2, Cursor: cursor})
	tail = wantOK(t, tail, err, "尾页之后")
	if len(tail.GetList()) != 0 || tail.GetHasMore() || tail.GetNextCursor() != "" {
		t.Fatalf("尾页之后仍有数据：n=%d has_more=%t cursor=%q",
			len(tail.GetList()), tail.GetHasMore(), tail.GetNextCursor())
	}
	// 「不回显」不等于「丢正文」：行里必须还留着，否则 RetryWebhookDelivery 无物可投。
	for _, d := range db.dels {
		if !strings.Contains(d.Payload, evtBodyMarker) {
			t.Fatalf("delivery_id=%d 的正文被分页读走了：%q", d.DeliveryID, d.Payload)
		}
	}
	// 摘要必须对得上入库正文：应用侧凭它判断收到的回调有没有被改。
	first, err := callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{AppId: testAppID, Ps: 1})
	first = wantOK(t, first, err, "首页一行")
	if first.GetList()[0].GetPayloadDigest() != rows[3].PayloadDigest {
		t.Fatalf("摘要投影不是入库行的：%s vs %s", first.GetList()[0].GetPayloadDigest(),
			rows[3].PayloadDigest)
	}
}

func TestListWebhookDeliveries_FiltersAreScopedToThisApp(t *testing.T) {
	db, s := webhookFixture(t)
	epA := seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult, hookURL, true, true, 0)
	epB := seedEndpointAt(db, testAppID, model.WebhookEventContentOffline, hookURL2, true, true, 0)
	epOther := seedEndpointAt(db, testApp2, model.WebhookEventContentPublishResult, hookURL, true, true, 0)
	deadA := seedDelivery(db, testAppID, epA, "f:dead", model.DeliveryStateDead, `{"aid":1}`)
	pendA := seedDelivery(db, testAppID, epA, "f:pend", model.DeliveryStatePending, `{"aid":2}`)
	okB := seedDelivery(db, testAppID, epB, "f:ok", model.DeliveryStateSuccess, `{"aid":3}`)
	other := seedDelivery(db, testApp2, epOther, "f:other", model.DeliveryStatePending, `{"aid":4}`)

	// 默认：本应用全部，别的应用一行都不该出现。
	// 不传端点过滤器时不该读端点表（归属校验只在给了 endpoint_id 时才需要），
	// 所以位点必须在调用之前取，否则这个断言会因为「before==after」而恒真。
	findByIDBefore := db.count("WebhookEndpoints.FindByID")
	all, err := callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{AppId: testAppID})
	all = wantOK(t, all, err, "本应用全量")
	if len(all.GetList()) != 3 {
		t.Fatalf("n=%d，期望 3：%v", len(all.GetList()), idsOf(all.GetList()))
	}
	for _, info := range all.GetList() {
		if info.GetAppId() != testAppID {
			t.Fatalf("串到了别人的台账：delivery_id=%d app=%d", info.GetDeliveryId(), info.GetAppId())
		}
	}
	wantCalls(t, db, "WebhookEndpoints.FindByID", findByIDBefore, 0, "无端点过滤器")

	// 端点过滤器：只有该端点的两行，且同一 ctime 内按 delivery_id 倒序。
	byEP, err := callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{
		AppId: testAppID, EndpointId: epA.EndpointID})
	byEP = wantOK(t, byEP, err, "按端点过滤")
	if fmt.Sprint(idsOf(byEP.GetList())) !=
		fmt.Sprint([]int64{pendA.DeliveryID, deadA.DeliveryID}) {
		t.Fatalf("端点过滤器返回 %v，期望 [%d %d]", idsOf(byEP.GetList()),
			pendA.DeliveryID, deadA.DeliveryID)
	}

	// 状态过滤器：已定义值各自只返回自己那批（且只在本应用内），未知值不当成「不过滤」。
	for _, tc := range []struct {
		state int32
		want  []int64
	}{
		{model.DeliveryStatePending, []int64{pendA.DeliveryID}},
		{model.DeliveryStateSuccess, []int64{okB.DeliveryID}},
		{model.DeliveryStateDead, []int64{deadA.DeliveryID}},
		{model.DeliveryStateDelivering, nil},
		{model.DeliveryStateRetryScheduled, nil},
		{model.DeliveryStateIgnored, nil},
	} {
		got, err := callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{
			AppId: testAppID, State: rpc.WebhookDeliveryState(tc.state)})
		got = wantOK(t, got, err, fmt.Sprintf("状态过滤 %d", tc.state))
		// 别人的同状态行不能混进来：testApp2 也有一条 PENDING，期望里只有本应用那一条。
		if fmt.Sprint(idsOf(got.GetList())) != fmt.Sprint(tc.want) {
			t.Fatalf("state=%d 返回 %v，期望 %v", tc.state, idsOf(got.GetList()), tc.want)
		}
	}
	// 未知状态值绝不当成「不过滤」：那会让运营以为看到了全部。
	for _, bad := range []int32{7, 99, -1} {
		before := snapshotWrites(db)
		reply, err := callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{
			AppId: testAppID, State: rpc.WebhookDeliveryState(bad)})
		wantFail(t, reply, err, errInvalidDeliveryStateFilter, fmt.Sprintf("未知状态 %d", bad))
		wantNoWrites(t, db, before, fmt.Sprintf("未知状态 %d", bad))
	}

	// 软删端点的台账必须仍列得出来（死信复盘要归属），DeleteWebhook 已验证过抑制语义。
	epA.DeletedAt = nowTS()
	afterDelete, err := callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{
		AppId: testAppID, EndpointId: epA.EndpointID})
	afterDelete = wantOK(t, afterDelete, err, "已删端点仍要可列")
	if len(afterDelete.GetList()) != 2 {
		t.Fatalf("已删端点列出 %d 行，期望 2（丢了归属就没法复盘死信）", len(afterDelete.GetList()))
	}

	// 拿别人的 endpoint_id 交叉查询 = 与「不存在」同口径拒绝，不给可探测差异。
	for _, tc := range []struct {
		name string
		ep   int64
	}{
		{"跨应用 endpoint_id", epOther.EndpointID},
		{"不存在的 endpoint_id", 987654},
		{"负数 endpoint_id", -epA.EndpointID},
	} {
		before := snapshotWrites(db)
		opened := db.count("WebhookDeliveries.ListByCursor")
		reply, err := callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{
			AppId: testAppID, EndpointId: tc.ep})
		wantFail(t, reply, err, model.ErrWebhookNotFound, tc.name)
		wantNoWrites(t, db, before, tc.name)
		// 归属没确认之前不得开始扫描台账。
		wantCalls(t, db, "WebhookDeliveries.ListByCursor", opened, 0, tc.name)
	}
	// 别人的台账自己不可见（另一侧只能看到它自己那一行）。
	otherSide, err := callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{AppId: testApp2})
	otherSide = wantOK(t, otherSide, err, "B 应用只看自己")
	if len(otherSide.GetList()) != 1 || otherSide.GetList()[0].GetDeliveryId() != other.DeliveryID {
		t.Fatalf("B 应用看到 %+v，期望只有它自己那一行", otherSide.GetList())
	}
}

// idsOf 投影列表里的 delivery_id，失败信息用。
func idsOf(list []*rpc.WebhookDeliveryInfo) []int64 {
	out := make([]int64, 0, len(list))
	for _, i := range list {
		out = append(out, i.GetDeliveryId())
	}
	return out
}

func TestListWebhookDeliveries_PageGatesAndFailClosed(t *testing.T) {
	db, s := webhookFixture(t)
	ep := seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult, hookURL, true, true, 0)
	for i := 0; i < 3; i++ {
		seedDelivery(db, testAppID, ep, fmt.Sprintf("gate:%d", i), model.DeliveryStatePending, `{"a":1}`)
	}

	// 应用归属先于一切：0/负数/不存在都不该扫台账。
	for _, tc := range []struct {
		name string
		app  int64
		want error
	}{
		{"app_id 为 0", 0, model.ErrInvalidAppID},
		{"app_id 为负", -testAppID, model.ErrInvalidAppID},
		{"应用不存在", 4242, model.ErrAppNotFound},
	} {
		before := snapshotWrites(db)
		opened := db.count("WebhookDeliveries.ListByCursor")
		reply, err := callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{AppId: tc.app})
		wantFail(t, reply, err, tc.want, tc.name)
		wantNoWrites(t, db, before, tc.name)
		wantCalls(t, db, "WebhookDeliveries.ListByCursor", opened, 0, tc.name)
	}
	// 应用查得到之后才判身份：负数 operator 是脏参数。
	db.failOn("Apps.FindByID", errFakeDown)
	before := snapshotWrites(db)
	reply, err := callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{AppId: testAppID})
	wantFail(t, reply, err, errFakeDown, "应用读失败必须 fail closed")
	wantNoWrites(t, db, before, "应用读失败")
	db.failOn("Apps.FindByID", nil)
	reply, err = callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{
		AppId: testAppID, OperatorMid: -1})
	wantFail(t, reply, err, model.ErrOperatorRequired, "operator_mid 为负")

	// owner 自查（0）与运营（>0）读到的行集必须相同：差别只在日志。
	asOwner, err := callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{AppId: testAppID})
	asOwner = wantOK(t, asOwner, err, "owner 自查")
	asOperator, err := callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{
		AppId: testAppID, OperatorMid: hookOperator})
	asOperator = wantOK(t, asOperator, err, "运营查")
	if fmt.Sprint(idsOf(asOwner.GetList())) != fmt.Sprint(idsOf(asOperator.GetList())) {
		t.Fatalf("两种身份看到的台账不同：%v vs %v",
			idsOf(asOwner.GetList()), idsOf(asOperator.GetList()))
	}

	// ps：不传走配置默认值，上限本身允许、+1 才拒，负数是脏参数。
	// 行集必须真的多于上限，否则「页长」断言会因为数据太少而恒真。
	for i := 0; i < 48; i++ {
		seedDelivery(db, testAppID, ep, fmt.Sprintf("wide:%d", i),
			model.DeliveryStateRetryScheduled, `{"a":1}`)
	}
	total := len(db.dels)
	cfg := s.Config.OpenPlatform
	if total <= int(cfg.MaxPageSize) {
		t.Fatalf("夹具行数=%d，必须多于 MaxPageSize=%d 才测得出页长", total, cfg.MaxPageSize)
	}
	def, err := callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{AppId: testAppID})
	def = wantOK(t, def, err, "默认页长")
	if len(def.GetList()) != int(cfg.PageSize) || !def.GetHasMore() {
		t.Fatalf("默认页长=%d has_more=%t，期望 %d/true",
			len(def.GetList()), def.GetHasMore(), cfg.PageSize)
	}
	atLimit, err := callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{
		AppId: testAppID, Ps: cfg.MaxPageSize})
	atLimit = wantOK(t, atLimit, err, "上限页长")
	if len(atLimit.GetList()) != int(cfg.MaxPageSize) || !atLimit.GetHasMore() {
		t.Fatalf("上限页长=%d has_more=%t，期望 %d/true",
			len(atLimit.GetList()), atLimit.GetHasMore(), cfg.MaxPageSize)
	}
	for _, tc := range []struct {
		name   string
		ps     int32
		cursor string
		want   error
	}{
		{"ps 超上限拒而不是静默裁剪", cfg.MaxPageSize + 1, "", model.ErrPsTooLarge},
		{"ps 为负", -1, "", model.ErrInvalidPage},
		{"游标不是 base64", 0, "not-a-cursor", model.ErrInvalidCursor},
		{"游标缺一半（没有时间位点）", 0, encodeCursor(0, 0)[:2], model.ErrInvalidCursor},
		{"游标时间位点为负", 0, encodeCursor(-1, 5), model.ErrInvalidCursor},
		{"游标主键位点为负", 0, encodeCursor(5, -1), model.ErrInvalidCursor},
	} {
		before = snapshotWrites(db)
		reply, err = callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{
			AppId: testAppID, Ps: tc.ps, Cursor: tc.cursor})
		wantFail(t, reply, err, tc.want, tc.name)
		wantNoWrites(t, db, before, tc.name)
	}
	// 合法游标（含 0 位点）必须被接受：只测非法侧会把「首页位点」写死成非 0 而不自知。
	if _, err = callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{
		AppId: testAppID, Cursor: encodeCursor(0, 0)}); err != nil {
		t.Fatalf("零位点游标应当成首页：%v", err)
	}

	// 台账读失败必须原样上抛：回空列表等于把故障说成「没有投递」。
	db.failOn("WebhookDeliveries.ListByCursor", errFakeDown)
	before = snapshotWrites(db)
	reply, err = callListWebhookDeliveries(t, s, &rpc.ListWebhookDeliveriesReq{AppId: testAppID})
	wantFail(t, reply, err, errFakeDown, "台账读失败")
	wantNoWrites(t, db, before, "台账读失败")
}

// ---------------------------------------------------------------- 重放

// retryFixture 一条处于 state 的投递任务，连同它的 ACTIVE 应用与「已验证且启用」端点。
//
// attempt / max_attempts / next_retry_at / lease_until / last_error 一律按「真的在重试链上
// 跑满过」的样子给：重放要断言的是「哪些列被 CAS 改掉、哪些必须原样保留」，夹具全是零值
// 就什么都断不出来（例如 lease 一直是 0，就永远测不出「重放必须清租约」）。
func retryFixture(t *testing.T, state int32) (*store, *svc.ServiceContext,
	*model.WebhookEndpoint, *model.WebhookDelivery) {
	t.Helper()
	db, s := webhookFixture(t)
	ep := seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult, hookURL, true, true, 0)
	d := seedDelivery(db, testAppID, ep, "replay:1", state, `{"grant_id":77}`)
	now := nowTS()
	d.PayloadDigest = model.DigestPayload(d.Payload)
	d.Attempt = s.Config.OpenPlatform.WebhookMaxAttempts // 预算用尽，这正是它进死信的成因
	d.NextRetryAt = now - 600                            // 最后一跳早已过期
	d.LastError = "dial tcp 10.0.0.1:443: i/o timeout"
	d.LastStatusCode = 504
	d.Ctime = now - 3600
	d.Mtime = now - 600
	return db, s, ep, d
}

func TestRetryWebhookDelivery_DeadReplayResetsAttemptButKeepsBudgetAndHistory(t *testing.T) {
	db, s, ep, d := retryFixture(t, model.DeliveryStateDead)
	cfg := s.Config.OpenPlatform
	// 脏数据：死信行本不该带租约，但 worker 崩溃留下的脏值必须被这次重放清掉，
	// 否则 ListDue 只能干等 lease 到期，重放等于没重放。
	d.LeaseUntil = nowTS() + cfg.WebhookLeaseSeconds*10
	payload, digest := d.Payload, d.PayloadDigest

	before := snapshotWrites(db)
	t0 := nowTS()
	reply, err := callRetryWebhookDelivery(t, s, retryReq(d.DeliveryID, hookOperator, true, "  运营复核后重放  "))
	t1 := nowTS()
	reply = wantOK(t, reply, err, "重放死信")
	if !reply.GetReplayed() {
		t.Fatal("CAS 命中的重放必须 replayed=true")
	}
	if reply.GetDeliveryId() != d.DeliveryID {
		t.Fatalf("delivery_id=%d，库里是 %d", reply.GetDeliveryId(), d.DeliveryID)
	}
	if int32(reply.GetState()) != model.DeliveryStatePending {
		t.Fatalf("state=%d，期望重放后 PENDING(%d)", reply.GetState(), model.DeliveryStatePending)
	}
	// 响应字段必须等于库里此刻的真值，而不是「希望它变成」的值。
	if reply.GetNextRetryAt() != d.NextRetryAt {
		t.Fatalf("next_retry_at reply=%d row=%d", reply.GetNextRetryAt(), d.NextRetryAt)
	}
	if reply.GetNextRetryAt() < t0 || reply.GetNextRetryAt() > t1 {
		t.Fatalf("next_retry_at=%d 不在本次调用窗口 [%d,%d] 内：重放行应当即刻可投",
			reply.GetNextRetryAt(), t0, t1)
	}
	wantOnlyWriteAttempts(t, db, before, map[string]int{
		"WebhookDeliveries.ResetForReplay": 1}, "重放死信")

	if d.State != model.DeliveryStatePending || d.Attempt != 0 {
		t.Fatalf("state=%d attempt=%d，期望 PENDING/attempt 归零（人工重放=重新走完整退避）",
			d.State, d.Attempt)
	}
	// 重放不得顺手抬高预算：max_attempts 是入队时的配置快照。
	if d.MaxAttempts != cfg.WebhookMaxAttempts || d.MaxAttempts != 6 {
		t.Fatalf("max_attempts=%d，期望仍是入队快照 %d", d.MaxAttempts, cfg.WebhookMaxAttempts)
	}
	if d.LeaseUntil != 0 {
		t.Fatalf("lease_until=%d，重放必须清租约", d.LeaseUntil)
	}
	// 重放原因入 last_error（问责链）：落库的是裁掉空白后的原文。
	if d.LastError != "运营复核后重放" {
		t.Fatalf("last_error=%q，期望裁空白后的重放理由", d.LastError)
	}
	// 正文与摘要不能被重放改写：worker 重发的是同一个事件，签名摘要必须仍对得上。
	if d.Payload != payload || d.PayloadDigest != digest {
		t.Fatalf("重放改写了正文/摘要：%q/%q", d.Payload, d.PayloadDigest)
	}
	// 历史结论保留：ResetForReplay 的 UPDATE 列表里没有 last_status_code，
	// 抹掉它等于丢掉「当初为什么失败」的证据。
	if d.LastStatusCode != 504 {
		t.Fatalf("last_status_code=%d，期望仍是 504", d.LastStatusCode)
	}
	if d.Mtime < t0 || d.Mtime > t1 {
		t.Fatalf("mtime=%d 未落在本次调用窗口 [%d,%d]", d.Mtime, t0, t1)
	}
	if d.AppID != testAppID || d.EndpointID != ep.EndpointID || d.EventID != "replay:1" {
		t.Fatalf("重放改动了归属：app=%d ep=%d eid=%s", d.AppID, d.EndpointID, d.EventID)
	}

	// 成功后行已是 PENDING：连环重放必须被拒，且一行都不再改（不然一次误点就能刷掉整条链）。
	again := snapshotWrites(db)
	snap := deliveryState(db)
	second, err := callRetryWebhookDelivery(t, s, retryReq(d.DeliveryID, hookOperator, true, "再来一次"))
	wantFail(t, second, err, model.ErrDeliveryNotRetryable, "重放成功后再重放")
	wantNoWrites(t, db, again, "重放成功后再重放")
	assertDeliveryStateUnchanged(t, db, snap, "重放成功后再重放")
}

func TestRetryWebhookDelivery_IgnoredNeedsNoConfirmButDeadDoes(t *testing.T) {
	// IGNORED 是「端点删除/应用下线」抑制出来的终态：它没有被自动重试放弃过，
	// 所以不需要 ignore_dead 这一确认位。
	db, s, _, d := retryFixture(t, model.DeliveryStateIgnored)
	d.LastError = "端点已删除，任务被抑制"
	before := snapshotWrites(db)
	reply, err := callRetryWebhookDelivery(t, s, retryReq(d.DeliveryID, hookOperator, false, "端点已恢复，补投"))
	reply = wantOK(t, reply, err, "重放 IGNORED")
	if !reply.GetReplayed() || int32(reply.GetState()) != model.DeliveryStatePending {
		t.Fatalf("IGNORED 重放结果 replayed=%t state=%d", reply.GetReplayed(), reply.GetState())
	}
	wantOnlyWriteAttempts(t, db, before, map[string]int{
		"WebhookDeliveries.ResetForReplay": 1}, "重放 IGNORED")
	if d.State != model.DeliveryStatePending || d.Attempt != 0 || d.LeaseUntil != 0 {
		t.Fatalf("state=%d attempt=%d lease=%d，期望 PENDING/0/0", d.State, d.Attempt, d.LeaseUntil)
	}
	if d.LastError != "端点已恢复，补投" {
		t.Fatalf("last_error=%q，期望被重放理由覆盖（旧抑制原因已完成使命）", d.LastError)
	}

	// DEAD 是「自动重试已放弃」的终态：缺省拒绝，避免误点一次就把一批已放弃的外呼重新打出去。
	db2, s2, _, d2 := retryFixture(t, model.DeliveryStateDead)
	guard := snapshotWrites(db2)
	snap := deliveryState(db2)
	reply, err = callRetryWebhookDelivery(t, s2, retryReq(d2.DeliveryID, hookOperator, false, "手滑点了一次"))
	wantFail(t, reply, err, model.ErrDeliveryNotRetryable, "DEAD 未显式确认")
	wantNoWrites(t, db2, guard, "DEAD 未显式确认")
	assertDeliveryStateUnchanged(t, db2, snap, "DEAD 未显式确认")

	// 确认位放行的是「这一条死信」，不是「绕过状态校验」：同一行第二次确认仍要成功一次就变 PENDING。
	reply, err = callRetryWebhookDelivery(t, s2, retryReq(d2.DeliveryID, hookOperator, true, "确认重放死信"))
	reply = wantOK(t, reply, err, "DEAD 确认后重放")
	if !reply.GetReplayed() {
		t.Fatal("确认后 CAS 应命中")
	}
	if d2.State != model.DeliveryStatePending || d2.Attempt != 0 {
		t.Fatalf("确认后 state=%d attempt=%d，期望 PENDING/0", d2.State, d2.Attempt)
	}
}

func TestRetryWebhookDelivery_NonReplayableStatesAndParamGatesAreZeroSideEffect(t *testing.T) {
	// 已在自动重试链上的三种状态 + 终态成功：一律拒绝。SUCCESS 尤其绝不复活——
	// 那会产生第二次对外副作用（README:73）。ignore_dead=true 也开不了这把锁。
	for _, state := range []int32{model.DeliveryStatePending, model.DeliveryStateDelivering,
		model.DeliveryStateRetryScheduled, model.DeliveryStateSuccess} {
		for _, confirm := range []bool{false, true} {
			db, s, _, d := retryFixture(t, state)
			before := snapshotWrites(db)
			snap := deliveryState(db)
			label := fmt.Sprintf("state=%d ignore_dead=%t", state, confirm)
			reply, err := callRetryWebhookDelivery(t, s, retryReq(d.DeliveryID, hookOperator, confirm, "想再打一次"))
			wantFail(t, reply, err, model.ErrDeliveryNotRetryable, label)
			wantNoWrites(t, db, before, label)
			assertDeliveryStateUnchanged(t, db, snap, label)
		}
	}

	// 参数门禁全部先于查库：脏请求不得产生读放大，更不得碰台账。
	db, s, _, d := retryFixture(t, model.DeliveryStateDead)
	for _, tc := range []struct {
		name   string
		id     int64
		op     int64
		reason string
		want   error
	}{
		{"delivery_id=0", 0, hookOperator, "确认重放死信", model.ErrDeliveryNotFound},
		{"delivery_id 为负", -d.DeliveryID, hookOperator, "确认重放死信", model.ErrDeliveryNotFound},
		// 主键缺失优先于身份/理由缺失：脏 id 连「谁在问」都不必知道。
		{"id 与身份同缺", 0, 0, "", model.ErrDeliveryNotFound},
		{"operator=0（重放是运营处置，不允许应用侧自助）", d.DeliveryID, 0, "确认重放死信", model.ErrOperatorRequired},
		{"operator 为负", d.DeliveryID, -1, "确认重放死信", model.ErrOperatorRequired},
		{"身份齐了但没写理由", d.DeliveryID, hookOperator, "", errReasonRequired},
		{"理由只有空白", d.DeliveryID, hookOperator, "  \t ", errReasonRequired},
	} {
		before := snapshotWrites(db)
		findBefore := db.count("WebhookDeliveries.FindByID")
		reply, err := callRetryWebhookDelivery(t, s, retryReq(tc.id, tc.op, true, tc.reason))
		wantFail(t, reply, err, tc.want, tc.name)
		wantNoWrites(t, db, before, tc.name)
		wantCalls(t, db, "WebhookDeliveries.FindByID", findBefore, 0, tc.name)
	}

	// 不存在的 delivery_id 与「不该出现在这张表里的」同口径：NotFound，且零写。
	before := snapshotWrites(db)
	findBefore := db.count("WebhookDeliveries.FindByID")
	reply, err := callRetryWebhookDelivery(t, s, retryReq(987654, hookOperator, true, "确认重放死信"))
	wantFail(t, reply, err, model.ErrDeliveryNotFound, "不存在的 delivery_id")
	wantNoWrites(t, db, before, "不存在的 delivery_id")
	wantCalls(t, db, "WebhookDeliveries.FindByID", findBefore, 1, "不存在的 delivery_id")

	// 理由长度门禁按 rune 计（运营写的是中文）：255 汉字 = 765 字节必须放行，256 汉字必须拒。
	// 选 765 字节正是为了区分「按字节量」的错实现；入库侧 model.maxDeliveryErrLen 的
	// 200 字节截断属于 model 层职责（fake 不模拟），所以这里只断前缀，不断尾部。
	for _, tc := range []struct {
		name   string
		reason string
		want   error
	}{
		{"255 个汉字（rune 上界内）", strings.Repeat("审", 255), nil},
		{"256 个汉字", strings.Repeat("审", 256), errReasonTooLong},
	} {
		dbx, sx, _, dx := retryFixture(t, model.DeliveryStateDead)
		guard := snapshotWrites(dbx)
		reply, err := callRetryWebhookDelivery(t, sx, retryReq(dx.DeliveryID, hookOperator, true, tc.reason))
		if tc.want != nil {
			wantFail(t, reply, err, tc.want, tc.name)
			wantNoWrites(t, dbx, guard, tc.name)
			continue
		}
		wantOK(t, reply, err, tc.name)
		if dx.State != model.DeliveryStatePending || !strings.HasPrefix(dx.LastError, strings.Repeat("审", 10)) {
			t.Fatalf("%s：重放放行却没把理由落库：state=%d last_error=%q", tc.name, dx.State, dx.LastError)
		}
	}
}

// TestRetryWebhookDelivery_LoserOfRaceEchoesDBTruth 钉的是「重放与 worker 抢占互斥」。
//
// 交错用 db.onHit 造：logic 读到 DEAD 快照之后、ResetForReplay 的 CAS 之前，
// 另一个进程按 model.Claim（op_webhook_delivery.go:252-262）把这一行抢走了。
// 纯前置构造做不到这件事（logic 读的就是同一个值），所以必须用一次性钩子。
func TestRetryWebhookDelivery_LoserOfRaceEchoesDBTruth(t *testing.T) {
	db, s, _, d := retryFixture(t, model.DeliveryStateDead)
	lease := s.Config.OpenPlatform.WebhookLeaseSeconds
	if lease <= 0 {
		t.Fatalf("夹具 WebhookLeaseSeconds=%d，必须 > 0 才测得出租约", lease)
	}
	attemptBefore := d.Attempt
	errBefore := d.LastError
	nextBefore := d.NextRetryAt

	db.onHit("WebhookDeliveries.ResetForReplay", func() {
		row := deliveryRow(db, d.DeliveryID)
		row.State = model.DeliveryStateDelivering
		row.Attempt++
		row.LeaseUntil = nowTS() + lease
		row.Mtime = nowTS()
	})

	before := snapshotWrites(db)
	findBefore := db.count("WebhookDeliveries.FindByID")
	reply, err := callRetryWebhookDelivery(t, s, retryReq(d.DeliveryID, hookOperator, true, "确认重放死信"))
	reply = wantOK(t, reply, err, "CAS 落败仍算成功（幂等语义）")
	if reply.GetReplayed() {
		t.Fatal("CAS 命中 0 行却报 replayed=true，运营会以为重放生效了")
	}
	// 回库里此刻的真值：状态是抢占者写的 DELIVERING，不是本方法希望的 PENDING。
	if int32(reply.GetState()) != model.DeliveryStateDelivering {
		t.Fatalf("state=%d，期望回库里真值 DELIVERING(%d)", reply.GetState(), model.DeliveryStateDelivering)
	}
	if reply.GetNextRetryAt() != nextBefore {
		t.Fatalf("next_retry_at=%d，期望仍是抢占前的 %d（Claim 不改这一列，本方法也不该改）",
			reply.GetNextRetryAt(), nextBefore)
	}
	// 落败方绝不能把抢占者的 attempt/lease/last_error 抹掉：
	// 抹掉 attempt 等于白送一次外呼预算，抹掉 lease 等于让同一行被两个 worker 同时领走。
	if d.State != model.DeliveryStateDelivering || d.Attempt != attemptBefore+1 {
		t.Fatalf("抢占者的行被改动：state=%d attempt=%d，期望 DELIVERING/%d",
			d.State, d.Attempt, attemptBefore+1)
	}
	if d.LeaseUntil <= nowTS() {
		t.Fatalf("lease_until=%d 被清掉了，抢占者还在跑这一行", d.LeaseUntil)
	}
	if d.LastError != errBefore {
		t.Fatalf("last_error 被落败方的重放理由覆盖：%q（原 %q）", d.LastError, errBefore)
	}
	if d.NextRetryAt != nextBefore || d.Payload == "" {
		t.Fatalf("落败方改动了不该改的列：next=%d payload=%q", d.NextRetryAt, d.Payload)
	}
	wantOnlyWriteAttempts(t, db, before, map[string]int{
		"WebhookDeliveries.ResetForReplay": 1}, "CAS 落败")
	// 一次初始读 + 一次「回库里真值」的重读，多一次就是白读，少一次就是拿快照充数。
	wantCalls(t, db, "WebhookDeliveries.FindByID", findBefore, 2, "CAS 落败")

	// 重读也失败时不得回「希望值」：那等于把故障说成一次成功的重放。
	db2, s2, _, d2 := retryFixture(t, model.DeliveryStateDead)
	db2.onHit("WebhookDeliveries.ResetForReplay", func() {
		row := deliveryRow(db2, d2.DeliveryID)
		row.State = model.DeliveryStateDelivering
		row.Attempt++
		row.LeaseUntil = nowTS() + lease
		// 故障只在 CAS 之后注入：failOn 对该算子是永久开关，放在 CAS 之前
		// 会让首读就失败，用例就退化成「读不到行」而不是「落败后重读失败」。
		db2.failOn("WebhookDeliveries.FindByID", errFakeDown)
	})
	guard := snapshotWrites(db2)
	reply, err = callRetryWebhookDelivery(t, s2, retryReq(d2.DeliveryID, hookOperator, true, "确认重放死信"))
	wantFail(t, reply, err, errFakeDown, "落败后重读失败")
	wantOnlyWriteAttempts(t, db2, guard, map[string]int{
		"WebhookDeliveries.ResetForReplay": 1}, "落败后重读失败")
}

func TestRetryWebhookDelivery_PreconditionsFailClosed(t *testing.T) {
	// 每一例都是「台账里的行看起来可重放，但外围前提已不成立」：
	// 少了任何一道门，本方法就会对已删除/未验证的地址再打一枪（SSRF 复放器），
	// 或者把正文已被清理的事件投成一个必然验签失败的假动作。
	for _, tc := range []struct {
		name  string
		want  error
		tweak func(db *store, ep *model.WebhookEndpoint, d *model.WebhookDelivery)
	}{
		{"应用已停用", model.ErrApplicationNotActive,
			func(db *store, _ *model.WebhookEndpoint, _ *model.WebhookDelivery) {
				db.apps[testAppID].Status = model.AppStatusSuspended
			}},
		{"应用行没了（脏台账）", model.ErrAppNotFound,
			func(db *store, _ *model.WebhookEndpoint, _ *model.WebhookDelivery) {
				delete(db.apps, testAppID)
			}},
		{"端点行没了", model.ErrWebhookNotFound,
			func(_ *store, _ *model.WebhookEndpoint, d *model.WebhookDelivery) {
				d.EndpointID = 987654
			}},
		{"端点属于别人（脏归属）", model.ErrWebhookNotFound,
			func(_ *store, ep *model.WebhookEndpoint, _ *model.WebhookDelivery) {
				ep.AppID = testApp2
			}},
		{"端点已删除", model.ErrWebhookNotFound,
			func(_ *store, ep *model.WebhookEndpoint, _ *model.WebhookDelivery) {
				ep.DeletedAt = nowTS()
			}},
		// 未验证先于停用判定：两道门都给脏行时，报出的必须是「从未验证」这一条。
		{"端点未验证（且停用）", model.ErrWebhookUnverified,
			func(_ *store, ep *model.WebhookEndpoint, _ *model.WebhookDelivery) {
				ep.VerifiedAt, ep.Enabled = 0, 0
			}},
		{"端点被停用", errWebhookEndpointDisabled,
			func(_ *store, ep *model.WebhookEndpoint, _ *model.WebhookDelivery) {
				ep.Enabled = 0
			}},
		{"正文已按保留期清理", model.ErrPayloadUnavailable,
			func(_ *store, _ *model.WebhookEndpoint, d *model.WebhookDelivery) {
				d.Payload = ""
			}},
		{"端点读失败", errFakeDown,
			func(db *store, _ *model.WebhookEndpoint, _ *model.WebhookDelivery) {
				db.failOn("WebhookEndpoints.FindByID", errFakeDown)
			}},
		{"应用读失败", errFakeDown,
			func(db *store, _ *model.WebhookEndpoint, _ *model.WebhookDelivery) {
				db.failOn("Apps.FindByID", errFakeDown)
			}},
	} {
		db, s, ep, d := retryFixture(t, model.DeliveryStateDead)
		tc.tweak(db, ep, d)
		before := snapshotWrites(db)
		snap := deliveryState(db)
		reply, err := callRetryWebhookDelivery(t, s, retryReq(d.DeliveryID, hookOperator, true, "确认重放死信"))
		wantFail(t, reply, err, tc.want, tc.name)
		wantNoWrites(t, db, before, tc.name)
		assertDeliveryStateUnchanged(t, db, snap, tc.name)
	}

	// 状态门禁先于应用与端点门禁：状态都不对时连「为什么要重放」都不必查。
	db, s, _, d := retryFixture(t, model.DeliveryStateSuccess)
	db.apps[testAppID].Status = model.AppStatusSuspended
	before := snapshotWrites(db)
	appBefore := db.count("Apps.FindByID")
	reply, err := callRetryWebhookDelivery(t, s, retryReq(d.DeliveryID, hookOperator, true, "确认重放死信"))
	wantFail(t, reply, err, model.ErrDeliveryNotRetryable, "状态门禁先于应用门禁")
	wantNoWrites(t, db, before, "状态门禁先于应用门禁")
	wantCalls(t, db, "Apps.FindByID", appBefore, 0, "状态门禁先于应用门禁")

	// 写令牌桶空了：所有只读门禁都已通过，但绝不发生 CAS（保护 MySQL 的口径与其他写入口一致）。
	db2, _, _, d2 := retryFixture(t, model.DeliveryStateDead)
	guard := snapshotWrites(db2)
	snap2 := deliveryState(db2)
	reply, err = callRetryWebhookDelivery(t, limitedSvc(db2), retryReq(d2.DeliveryID, hookOperator, true, "确认重放死信"))
	wantFail(t, reply, err, model.ErrRateLimited, "写令牌桶无余量")
	wantOnlyWriteAttempts(t, db2, guard, map[string]int{}, "写令牌桶无余量")
	assertDeliveryStateUnchanged(t, db2, snap2, "写令牌桶无余量")

	// CAS 自身失败必须原样上抛：报成功会让运营以为已重放，实际行仍是 DEAD。
	db3, s3, _, d3 := retryFixture(t, model.DeliveryStateDead)
	db3.failOn("WebhookDeliveries.ResetForReplay", errFakeDown)
	before3 := snapshotWrites(db3)
	snap3 := deliveryState(db3)
	reply, err = callRetryWebhookDelivery(t, s3, retryReq(d3.DeliveryID, hookOperator, true, "确认重放死信"))
	wantFail(t, reply, err, errFakeDown, "CAS 失败")
	wantOnlyWriteAttempts(t, db3, before3, map[string]int{
		"WebhookDeliveries.ResetForReplay": 1}, "CAS 失败")
	assertDeliveryStateUnchanged(t, db3, snap3, "CAS 失败")
	if d3.State != model.DeliveryStateDead || d3.Attempt != s3.Config.OpenPlatform.WebhookMaxAttempts {
		t.Fatalf("CAS 失败后行被改动：state=%d attempt=%d", d3.State, d3.Attempt)
	}
}

// TestRetryWebhookDeliveryBackoffCurveComesFromConfiguredTriple 钉退避三元组：
// WebhookRetryBaseSeconds / WebhookRetryMaxSeconds / WebhookMaxAttempts。
//
// 诚实说明：本服务只有生产者侧，投递 worker 不在这里（internal/consumer 目录不存在），
// 所以曲线的唯一消费者是 worker，重放能改变的只有 attempt 这一列。本用例因此钉三件事：
//  1. 曲线由配置算出（换一对配置值必须换一条曲线，否则常数和配置谁也分不清）；
//  2. 重放把 attempt 归零 ⇒ 下一跳回到 base，即「重新走一遍完整退避」，
//     而 max_attempts 仍是入队快照 ⇒ 最坏还是同样次数后进死信，不会因重放拿到额外预算；
//  3. base 翻倍到 max 封顶、且不因位移溢出翻回小值。
func TestRetryWebhookDeliveryBackoffCurveComesFromConfiguredTriple(t *testing.T) {
	db, s, _, d := retryFixture(t, model.DeliveryStateDead)
	cfg := s.Config.OpenPlatform
	// 夹具三元组 == 生产默认（etc/open-platform.yaml:35-37 与 config.go:80-85 的 default= 一致）。
	// 下面的期望值是按 base=30/cap=3600 手算的，配置漂移时这一行先给出明确提示。
	if cfg.WebhookMaxAttempts != 6 || cfg.WebhookRetryBaseSeconds != 30 || cfg.WebhookRetryMaxSeconds != 3600 {
		t.Fatalf("夹具三元组漂移：max=%d base=%d cap=%d，期望 6/30/3600",
			cfg.WebhookMaxAttempts, cfg.WebhookRetryBaseSeconds, cfg.WebhookRetryMaxSeconds)
	}
	// now=0 让 NextRetryAt 直接返回「延迟秒数」，便于逐条比对。
	for _, tc := range []struct {
		attempt int32
		want    int64
	}{
		{1, 30}, {2, 60}, {3, 120}, {4, 240}, {5, 480}, {6, 960},
		{7, 1920}, {8, 3600}, {9, 3600}, {20, 3600},
	} {
		got := model.NextRetryAt(0, cfg.WebhookRetryBaseSeconds, cfg.WebhookRetryMaxSeconds, tc.attempt)
		if got != tc.want {
			t.Fatalf("第 %d 次尝试退避 %d 秒，期望 %d 秒（base 翻倍、封顶 %d）",
				tc.attempt, got, tc.want, cfg.WebhookRetryMaxSeconds)
		}
	}
	// 单调不减：溢出（位移被钳在 20）也不得把退避翻回小值。
	for a := int32(2); a <= 40; a++ {
		prev := model.NextRetryAt(0, cfg.WebhookRetryBaseSeconds, cfg.WebhookRetryMaxSeconds, a-1)
		cur := model.NextRetryAt(0, cfg.WebhookRetryBaseSeconds, cfg.WebhookRetryMaxSeconds, a)
		if cur < prev {
			t.Fatalf("attempt=%d 退避 %d < 上一跳 %d，退避只能更长不能更短", a, cur, prev)
		}
	}
	if big := model.NextRetryAt(0, cfg.WebhookRetryBaseSeconds, cfg.WebhookRetryMaxSeconds, 1<<30); big != cfg.WebhookRetryMaxSeconds {
		t.Fatalf("attempt 极大时退避=%d，期望封顶=%d", big, cfg.WebhookRetryMaxSeconds)
	}

	// 配置真的被读：换一对值必须换一条曲线。
	cfg.WebhookRetryBaseSeconds, cfg.WebhookRetryMaxSeconds = 45, 600
	for _, tc := range []struct {
		attempt int32
		want    int64
	}{
		{1, 45}, {2, 90}, {3, 180}, {4, 360}, {5, 600}, {6, 600},
	} {
		got := model.NextRetryAt(0, cfg.WebhookRetryBaseSeconds, cfg.WebhookRetryMaxSeconds, tc.attempt)
		if got != tc.want {
			t.Fatalf("base=45/cap=600 时第 %d 次退避 %d，期望 %d", tc.attempt, got, tc.want)
		}
	}
	// 全 0 的退化配置落到同一套默认曲线（model 的兜底）。config.go:133 的校验器
	// 已让这种配置起不来，这里钉的是「即使绕过校验也不会退化成立即重试风暴」。
	cfg.WebhookRetryBaseSeconds, cfg.WebhookRetryMaxSeconds = 0, 0
	if hop := model.NextRetryAt(0, cfg.WebhookRetryBaseSeconds, cfg.WebhookRetryMaxSeconds, 1); hop != 30 {
		t.Fatalf("退化配置首跳=%d，期望兜底回默认 30", hop)
	}
	if hop := model.NextRetryAt(0, cfg.WebhookRetryBaseSeconds, cfg.WebhookRetryMaxSeconds, 40); hop != 3600 {
		t.Fatalf("退化配置远跳=%d，期望兜底封顶 3600", hop)
	}
	cfg.WebhookRetryBaseSeconds, cfg.WebhookRetryMaxSeconds = 30, 3600

	// 重放前后各自落在曲线的哪一跳：attempt 归零 ⇒ 从 base 重新开始。
	lastHop := model.NextRetryAt(0, cfg.WebhookRetryBaseSeconds, cfg.WebhookRetryMaxSeconds, d.Attempt)
	if d.Attempt != cfg.WebhookMaxAttempts {
		t.Fatalf("夹具 attempt=%d，期望已用尽预算 %d", d.Attempt, cfg.WebhookMaxAttempts)
	}
	if lastHop != 960 {
		t.Fatalf("重放前下一跳=%d，期望第 6 跳 960", lastHop)
	}
	before := snapshotWrites(db)
	reply, err := callRetryWebhookDelivery(t, s, retryReq(d.DeliveryID, hookOperator, true, "确认重放死信"))
	reply = wantOK(t, reply, err, "重放以重启退避")
	wantOnlyWriteAttempts(t, db, before, map[string]int{
		"WebhookDeliveries.ResetForReplay": 1}, "重放以重启退避")
	firstHop := model.NextRetryAt(0, cfg.WebhookRetryBaseSeconds, cfg.WebhookRetryMaxSeconds, d.Attempt)
	if firstHop != cfg.WebhookRetryBaseSeconds {
		t.Fatalf("重放后 attempt=%d，下一跳应为 base=%d，实际 %d", d.Attempt, cfg.WebhookRetryBaseSeconds, firstHop)
	}
	if firstHop >= lastHop {
		t.Fatalf("重放没有把退避拉回起点：before=%d after=%d", lastHop, firstHop)
	}
	// 重放「即刻可投」：next_retry_at 是本次调用的时刻，而不是曲线上的下一个点。
	if d.NextRetryAt < reply.GetNextRetryAt()-1 || d.NextRetryAt > nowTS()+1 {
		t.Fatalf("next_retry_at=%d 不在本次调用附近", d.NextRetryAt)
	}
	// 预算不外加：重放后的行最坏仍是在同样次数之后进死信。
	if d.MaxAttempts != cfg.WebhookMaxAttempts || d.MaxAttempts != 6 {
		t.Fatalf("max_attempts=%d，期望仍是入队快照 6", d.MaxAttempts)
	}
}
