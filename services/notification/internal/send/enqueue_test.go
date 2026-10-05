package send

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"go-video/services/notification/internal/policy"
	"go-video/services/notification/internal/repository"
	"go-video/services/notification/internal/testx"
	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

// 入队用例单测：幂等去重、语言回落、偏好/频控拦截、入口即拒的校验。
// 全程内存假件，不连 MySQL/Redis，也不调用任何供应商接口。

const (
	codeShip  = "order_shipped"
	utcDayRef = "2026-03-14T06:00:00Z"
)

type env struct {
	tmpl    *testx.TemplateModel
	deliv   *testx.DeliveryModel
	dnd     *testx.DndPrefModel
	quota   *testx.QuotaCounter
	repo    *repository.Repository
	enqueue *Enqueuer
	fixed   time.Time
}

func newEnv(t *testing.T, opt Options) *env {
	t.Helper()
	e := &env{
		tmpl:  testx.NewTemplateModel(),
		deliv: testx.NewDeliveryModel(),
		dnd:   testx.NewDndPrefModel(),
		quota: testx.NewQuotaCounter(),
	}
	e.repo = repository.NewWithModels(e.quota, e.tmpl, e.deliv,
		testx.NewOffsetModel(), testx.NewDeadLetterModel(), e.dnd)
	if opt.DefaultTimezone == "" {
		opt.DefaultTimezone = "Asia/Shanghai"
	}
	enq, err := New(e.repo, opt)
	if err != nil {
		t.Fatalf("send.New: %v", err)
	}
	e.fixed = time.Date(2026, 3, 14, 6, 0, 0, 0, time.UTC)
	e.enqueue = enq.WithClock(func() time.Time { return e.fixed })
	return e
}

// seedTemplate 预置一个已发布模板。
func (e *env) seedTemplate(code string, channel int32, lang string, title, body string) {
	e.tmpl.Seed(&model.NotificationTemplate{
		TemplateCode: code, Channel: channel, Lang: lang, TitleTpl: title, BodyTpl: body,
		Version: 1, State: model.TemplateStatePublished, Operator: "op-1",
	})
}

func pushReq(bizKey string, recips ...*rpc.Recipient) *rpc.SendNotificationReq {
	return &rpc.SendNotificationReq{
		Recipients:     recips,
		Channel:        rpc.Channel_CHANNEL_PUSH,
		TemplateCode:   codeShip,
		TemplateParams: map[string]string{"order_no": "SO-1"},
		BizKey:         bizKey,
	}
}

func (e *env) enqueueOnce(t *testing.T, in *rpc.SendNotificationReq) *Result {
	t.Helper()
	res, err := e.enqueue.Enqueue(context.Background(), in)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return res
}

// TestEnqueueBizKeyIdempotent：同一 biz_key 重复提交只产生一行任务，
// 第二次必须回放既有任务并置 duplicate=true。
func TestEnqueueBizKeyIdempotent(t *testing.T) {
	e := newEnv(t, Options{DefaultLanguage: model.LangZhCN})
	e.seedTemplate(codeShip, model.ChannelPush, model.LangZhCN, "发货通知", "订单 {{order_no}} 已发货")
	in := pushReq("order-ship:SO-1", &rpc.Recipient{Mid: 1001, TargetRef: "device-a"})

	first := e.enqueueOnce(t, in)
	if len(first.Reply.GetDeliveries()) != 1 {
		t.Fatalf("首次应产出 1 条投递，got %d", len(first.Reply.GetDeliveries()))
	}
	if first.Reply.GetDuplicate() {
		t.Error("首次提交不应标记 duplicate")
	}
	if len(first.NewDeliveryIDs) != 1 {
		t.Errorf("首次应给出待投递 ID，got %v", first.NewDeliveryIDs)
	}

	second := e.enqueueOnce(t, pushReq("order-ship:SO-1", &rpc.Recipient{Mid: 1001, TargetRef: "device-a"}))
	if !second.Reply.GetDuplicate() {
		t.Error("幂等命中时必须置 duplicate=true")
	}
	if len(second.NewDeliveryIDs) != 0 {
		t.Errorf("幂等命中不得再次外发，got %v", second.NewDeliveryIDs)
	}
	if second.Reply.GetDeliveries()[0].GetDeliveryId() != first.Reply.GetDeliveries()[0].GetDeliveryId() {
		t.Error("幂等回放必须返回既有任务，而不是新建一条")
	}
	if len(e.deliv.Rows()) != 1 {
		t.Errorf("任务行数 = %d, want 1", len(e.deliv.Rows()))
	}
	if e.deliv.Duplicates != 1 {
		t.Errorf("唯一索引命中次数 = %d, want 1", e.deliv.Duplicates)
	}
}

// TestEnqueueDifferentBizKeyNotDeduped：换个 biz_key 就是新的一次提醒，不能被误判重复。
func TestEnqueueDifferentBizKeyNotDeduped(t *testing.T) {
	e := newEnv(t, Options{DefaultLanguage: model.LangZhCN})
	e.seedTemplate(codeShip, model.ChannelPush, model.LangZhCN, "发货通知", "订单 {{order_no}} 已发货")
	e.enqueueOnce(t, pushReq("k-1", &rpc.Recipient{Mid: 1001, TargetRef: "device-a"}))
	res := e.enqueueOnce(t, pushReq("k-2", &rpc.Recipient{Mid: 1001, TargetRef: "device-a"}))
	if len(res.NewDeliveryIDs) != 1 || res.Reply.GetDuplicate() {
		t.Errorf("不同 biz_key 应新建任务: %v dup=%v", res.NewDeliveryIDs, res.Reply.GetDuplicate())
	}
}

// TestEnqueueRowLevelDedupe：同一请求里重复的接收人只投一次（组内去重 + 唯一索引双保险）。
func TestEnqueueRowLevelDedupe(t *testing.T) {
	e := newEnv(t, Options{DefaultLanguage: model.LangZhCN})
	e.seedTemplate(codeShip, model.ChannelPush, model.LangZhCN, "发货通知", "订单 {{order_no}} 已发货")
	res := e.enqueueOnce(t, pushReq("k-1",
		&rpc.Recipient{Mid: 1001, TargetRef: "device-a"},
		&rpc.Recipient{Mid: 1001, TargetRef: "device-a"},
		&rpc.Recipient{Mid: 1001, TargetRef: "device-b"},
		&rpc.Recipient{Mid: 1002, TargetRef: "device-a"},
	))
	if len(e.deliv.Rows()) != 3 {
		t.Errorf("同 mid 不同设备/同设备不同 mid 必须分行，重复项合并 -> want 3 行, got %d", len(e.deliv.Rows()))
	}
	if !res.Reply.GetDuplicate() {
		t.Error("组内重复接收人应置 duplicate")
	}
}

// TestEnqueueMultiDeviceSameMid：同一 mid 的两台设备都要收到，不能被幂等键吃掉一行。
func TestEnqueueMultiDeviceSameMid(t *testing.T) {
	e := newEnv(t, Options{DefaultLanguage: model.LangZhCN})
	e.seedTemplate(codeShip, model.ChannelPush, model.LangZhCN, "发货通知", "订单 {{order_no}}")
	keys := map[string]bool{}
	for _, dev := range []string{"d1", "d2", "d3"} {
		res := e.enqueueOnce(t, pushReq("k-1", &rpc.Recipient{Mid: 1001, DeviceId: dev}))
		if len(res.NewDeliveryIDs) != 1 {
			t.Fatalf("设备 %s 的任务未建立", dev)
		}
		rows := e.deliv.Rows()
		row := rows[res.NewDeliveryIDs[0]]
		if keys[row.BizKey] {
			t.Errorf("设备 %s 的行级幂等键与前面重复，多设备投递会丢消息", dev)
		}
		keys[row.BizKey] = true
	}
}

// TestEnqueueLangFallback：接收人语言 -> 请求默认语言 -> 全局默认，逐级回落且落库记录命中语言。
func TestEnqueueLangFallback(t *testing.T) {
	e := newEnv(t, Options{DefaultLanguage: model.LangZhCN})
	e.seedTemplate(codeShip, model.ChannelPush, model.LangEn, "Shipped", "order {{order_no}}")
	e.seedTemplate(codeShip, model.ChannelPush, model.LangZhTW, "已出貨", "訂單 {{order_no}}")
	e.seedTemplate(codeShip, model.ChannelPush, model.LangZhCN, "已发货", "订单 {{order_no}}")

	cases := []struct {
		name    string
		recip   *rpc.Recipient
		reqDef  rpc.Language
		wantLag string
	}{
		{"本人指定 en", &rpc.Recipient{Mid: 1, Language: rpc.Language_LANGUAGE_EN}, rpc.Language_LANGUAGE_UNSPECIFIED, model.LangEn},
		{"本人指定 zh-TW", &rpc.Recipient{Mid: 2, Language: rpc.Language_LANGUAGE_ZH_TW}, rpc.Language_LANGUAGE_UNSPECIFIED, model.LangZhTW},
		{"回落请求默认", &rpc.Recipient{Mid: 3}, rpc.Language_LANGUAGE_EN, model.LangEn},
		{"回落全局默认", &rpc.Recipient{Mid: 4}, rpc.Language_LANGUAGE_UNSPECIFIED, model.LangZhCN},
	}
	for _, c := range cases {
		in := pushReq("lang:"+c.name, &rpc.Recipient{Mid: c.recip.Mid, Language: c.recip.Language, TargetRef: "device-a"})
		in.DefaultLanguage = c.reqDef
		res := e.enqueueOnce(t, in)
		rows := e.deliv.Rows()
		row := rows[res.NewDeliveryIDs[0]]
		if row.Lang != c.wantLag {
			t.Errorf("%s: 命中语言 = %q, want %q", c.name, row.Lang, c.wantLag)
		}
	}
}

// TestEnqueueTemplateNotFoundRejected：没有可用已发布模板时显式失败，不落一条注定失败的任务。
func TestEnqueueTemplateNotFoundRejected(t *testing.T) {
	e := newEnv(t, Options{DefaultLanguage: model.LangZhCN})
	e.tmpl.Seed(&model.NotificationTemplate{
		TemplateCode: codeShip, Channel: model.ChannelPush, Lang: model.LangZhCN,
		TitleTpl: "t", BodyTpl: "b", Version: 1, State: model.TemplateStateDraft,
	})
	_, err := e.enqueue.Enqueue(context.Background(), pushReq("k-1", &rpc.Recipient{Mid: 1, TargetRef: "d"}))
	if !errors.Is(err, model.ErrTemplateNotFound) {
		t.Fatalf("want ErrTemplateNotFound, got %v", err)
	}
	if len(e.deliv.Rows()) != 0 {
		t.Error("校验失败时不得落任务")
	}
}

// TestEnqueueMissingVarRejectedAtEntry：变量缺失属于调用方错误，必须在入口拒绝而不是进死信。
func TestEnqueueMissingVarRejectedAtEntry(t *testing.T) {
	e := newEnv(t, Options{DefaultLanguage: model.LangZhCN})
	e.seedTemplate(codeShip, model.ChannelPush, model.LangZhCN, "发货通知", "订单 {{order_no}} 由 {{courier}} 配送")
	in := pushReq("k-1", &rpc.Recipient{Mid: 1, TargetRef: "d"})
	_, err := e.enqueue.Enqueue(context.Background(), in)
	if !errors.Is(err, policy.ErrRenderMissingVar) {
		t.Fatalf("want ErrRenderMissingVar, got %v", err)
	}
	if len(e.deliv.Rows()) != 0 {
		t.Error("变量缺失时不得落任务")
	}
	// 补齐变量后同一 biz_key 可正常入队。
	in.TemplateParams["courier"] = "顺丰"
	res := e.enqueueOnce(t, in)
	if len(res.NewDeliveryIDs) != 1 {
		t.Error("补齐变量后应入队成功")
	}
	// params_json 落的是渲染变量快照。
	rows := e.deliv.Rows()
	var snapshot map[string]string
	if err := json.Unmarshal([]byte(rows[res.NewDeliveryIDs[0]].ParamsJson), &snapshot); err != nil {
		t.Fatalf("params_json 不是合法 JSON: %v", err)
	}
	if snapshot["courier"] != "顺丰" || snapshot["order_no"] != "SO-1" {
		t.Errorf("变量快照不完整: %v", snapshot)
	}
}

// TestEnqueueInjectionRejected：模板参数里的明文号码/通道字段必须被拒。
func TestEnqueueInjectionRejected(t *testing.T) {
	e := newEnv(t, Options{DefaultLanguage: model.LangZhCN})
	e.seedTemplate(codeShip, model.ChannelPush, model.LangZhCN, "发货通知", "订单 {{order_no}}")
	bad := []map[string]string{
		{"order_no": "SO-1", "phone": "13800001111"},
		{"order_no": "SO-1", "email": "a@b.com"},
		{"order_no": "SO-1", "channel": "sms"},
		{"order_no": "{{secret}}"},
	}
	for _, p := range bad {
		in := pushReq("k-"+strings.Join(keysOf(p), "-"), &rpc.Recipient{Mid: 1, TargetRef: "d"})
		in.TemplateParams = p
		if _, err := e.enqueue.Enqueue(context.Background(), in); err == nil {
			t.Errorf("参数 %v 应被拒绝", p)
		}
	}
	if len(e.deliv.Rows()) != 0 {
		t.Error("非法参数不得落任务")
	}
}

// TestEnqueueRequestValidation：入口校验清单。
func TestEnqueueRequestValidation(t *testing.T) {
	e := newEnv(t, Options{DefaultLanguage: model.LangZhCN, MaxRecipients: 2})
	e.seedTemplate(codeShip, model.ChannelPush, model.LangZhCN, "发货通知", "订单 {{order_no}}")
	r := func(mid int64) *rpc.Recipient { return &rpc.Recipient{Mid: mid, TargetRef: "d"} }

	cases := []struct {
		name string
		in   *rpc.SendNotificationReq
		want error
	}{
		{"无接收人", &rpc.SendNotificationReq{Channel: rpc.Channel_CHANNEL_PUSH, TemplateCode: codeShip, BizKey: "k"}, ErrNoRecipients},
		{"接收人超量", pushReq("k-max", r(1), r(2), r(3)), ErrTooManyRecipients},
		{"无幂等键", func() *rpc.SendNotificationReq {
			in := pushReq("", r(1))
			in.IdempotencyKey = ""
			return in
		}(), nil},
		{"无模板码", func() *rpc.SendNotificationReq { in := pushReq("k-1", r(1)); in.TemplateCode = "  "; return in }(), nil},
		{"通道未指定", func() *rpc.SendNotificationReq {
			in := pushReq("k-1", r(1))
			in.Channel = rpc.Channel_CHANNEL_UNSPECIFIED
			return in
		}(), nil},
		{"过期时间在过去", func() *rpc.SendNotificationReq {
			in := pushReq("k-1", r(1))
			in.ExpireAt = e.fixed.Add(-time.Hour).Unix()
			return in
		}(), ErrExpiredRequest},
		{"nil 接收人", pushReq("k-1", nil), ErrRecipientNil},
		{"既无 mid 也无 target", pushReq("k-1", &rpc.Recipient{}), ErrNoRecipientTarget},
	}
	for _, c := range cases {
		_, err := e.enqueue.Enqueue(context.Background(), c.in)
		if err == nil {
			t.Errorf("%s: 期望被拒绝", c.name)
			continue
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: got %v want %v", c.name, err, c.want)
		}
	}
	if len(e.deliv.Rows()) != 0 {
		t.Error("校验失败时不得落任务")
	}
}

// TestEnqueueMutedChannelSuppressed：用户关闭该通道 -> 落 suppressed 终态，不占外发额度。
func TestEnqueueMutedChannelSuppressed(t *testing.T) {
	e := newEnv(t, Options{DefaultLanguage: model.LangZhCN, DndEnabled: true})
	e.seedTemplate(codeShip, model.ChannelPush, model.LangZhCN, "发货通知", "订单 {{order_no}}")
	e.dnd.Seed(&model.NotificationDndPref{
		Mid: 5001, State: model.DndStateOn, MutedChannels: model.MaskOfChannels([]int32{model.ChannelPush}),
	})
	res := e.enqueueOnce(t, pushReq("k-1", &rpc.Recipient{Mid: 5001, TargetRef: "d"}))
	rows := e.deliv.Rows()
	row := rows[res.Reply.GetDeliveries()[0].GetDeliveryId()]
	if row.State != model.DeliveryStateSuppressed {
		t.Errorf("状态 = %d, want suppressed", row.State)
	}
	if !strings.Contains(row.LastError, "muted") {
		t.Errorf("拦截需留原因, got %q", row.LastError)
	}
	if res.Reply.GetSuppressed() != 1 {
		t.Errorf("suppressed 计数 = %d, want 1", res.Reply.GetSuppressed())
	}
	if len(res.NewDeliveryIDs) != 0 {
		t.Error("被拦截任务不得进入外发列表")
	}
}

// TestEnqueueQuietHoursNotSuppressed：免打扰时段只是“现在不该发”，
// 必须落成可投递任务由 Dispatcher 顺延，落 suppressed 会把该发的提醒永久丢掉。
func TestEnqueueQuietHoursNotSuppressed(t *testing.T) {
	e := newEnv(t, Options{DefaultLanguage: model.LangZhCN, DndEnabled: true, DefaultTimezone: "Asia/Shanghai"})
	e.seedTemplate(codeShip, model.ChannelPush, model.LangZhCN, "发货通知", "订单 {{order_no}}")
	// fixed = 06:00 UTC = 北京 14:00；窗口 13:00-15:00 覆盖当前时刻。
	e.dnd.Seed(&model.NotificationDndPref{
		Mid: 6001, State: model.DndStateOn, QuietStart: "13:00", QuietEnd: "15:00", Timezone: "Asia/Shanghai",
	})
	res := e.enqueueOnce(t, pushReq("k-1", &rpc.Recipient{Mid: 6001, TargetRef: "d"}))
	row := e.deliv.Rows()[res.Reply.GetDeliveries()[0].GetDeliveryId()]
	if row.State != model.DeliveryStatePending {
		t.Errorf("静默时段内入队应为 pending（由 Dispatcher 顺延），got %d", row.State)
	}
}

// TestEnqueueQuotaExceededSuppressed：每日配额用尽后落 suppressed。
func TestEnqueueQuotaExceededSuppressed(t *testing.T) {
	e := newEnv(t, Options{DefaultLanguage: model.LangZhCN, DailyQuotaPerMid: 2})
	e.seedTemplate(codeShip, model.ChannelPush, model.LangZhCN, "发货通知", "订单 {{order_no}}")
	for i := 1; i <= 2; i++ {
		res := e.enqueueOnce(t, pushReq(quotaKey(i), &rpc.Recipient{Mid: 7001, TargetRef: "d"}))
		if res.Reply.GetSuppressed() != 0 {
			t.Fatalf("第 %d 次不该被拦", i)
		}
	}
	res := e.enqueueOnce(t, pushReq(quotaKey(3), &rpc.Recipient{Mid: 7001, TargetRef: "d"}))
	row := e.deliv.Rows()[res.Reply.GetDeliveries()[0].GetDeliveryId()]
	if row.State != model.DeliveryStateSuppressed || !strings.Contains(row.LastError, "quota") {
		t.Errorf("超配额应落 suppressed, got state=%d reason=%q", row.State, row.LastError)
	}
	// 未超限的用户不受影响。
	if r2 := e.enqueueOnce(t, pushReq("k-other", &rpc.Recipient{Mid: 7002, TargetRef: "d"})); r2.Reply.GetSuppressed() != 0 {
		t.Error("其他用户配额独立")
	}
}

// TestEnqueueQuotaFailClosed：配额存储不可用时必须拒绝整批请求，
// 既不能绕过频控，也不能把通知写成成功。
func TestEnqueueQuotaFailClosed(t *testing.T) {
	e := newEnv(t, Options{DefaultLanguage: model.LangZhCN, DailyQuotaPerMid: 5})
	e.seedTemplate(codeShip, model.ChannelPush, model.LangZhCN, "发货通知", "订单 {{order_no}}")
	e.quota.Fail = errors.New("redis down")
	_, err := e.enqueue.Enqueue(context.Background(), pushReq("k-1", &rpc.Recipient{Mid: 8001, TargetRef: "d"}))
	if !errors.Is(err, repository.ErrQuotaUnavailable) {
		t.Fatalf("want ErrQuotaUnavailable, got %v", err)
	}
	if len(e.deliv.Rows()) != 0 {
		t.Error("频控不可用时不得落任务")
	}
}

// TestEnqueuePreferenceReadFailureFailsClosed：偏好读不到时拒绝请求，由上游按同一 biz_key 重试。
func TestEnqueuePreferenceReadFailureFailsClosed(t *testing.T) {
	e := newEnv(t, Options{DefaultLanguage: model.LangZhCN})
	e.seedTemplate(codeShip, model.ChannelPush, model.LangZhCN, "发货通知", "订单 {{order_no}}")
	repo := repository.NewWithModels(e.quota, e.tmpl, e.deliv, testx.NewOffsetModel(),
		testx.NewDeadLetterModel(), brokenPref{})
	enq, err := New(repo, Options{DefaultLanguage: model.LangZhCN, DefaultTimezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enq.Enqueue(context.Background(), pushReq("k-1", &rpc.Recipient{Mid: 9001, TargetRef: "d"})); err == nil {
		t.Fatal("偏好不可用时应拒绝整批请求")
	}
	if len(e.deliv.Rows()) != 0 {
		t.Error("偏好不可用时不得落任务")
	}
}

// TestEnqueueRecordsSourceEvent：事件入口的 event_id 要落到任务上，便于按事件回溯。
func TestEnqueueRecordsSourceEvent(t *testing.T) {
	e := newEnv(t, Options{DefaultLanguage: model.LangZhCN})
	e.seedTemplate(codeShip, model.ChannelPush, model.LangZhCN, "发货通知", "订单 {{order_no}}")
	res, err := e.enqueue.Enqueue(policy.WithEventID(context.Background(), "evt-123"),
		pushReq("k-1", &rpc.Recipient{Mid: 1, TargetRef: "d"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := e.deliv.Rows()[res.NewDeliveryIDs[0]].SourceEventId; got != "evt-123" {
		t.Errorf("source_event_id = %q", got)
	}
}

// TestEnqueueLocksTemplateVersion：任务落库时锁定模板版本，运营后续发布新版本不影响在途任务。
func TestEnqueueLocksTemplateVersion(t *testing.T) {
	e := newEnv(t, Options{DefaultLanguage: model.LangZhCN})
	e.seedTemplate(codeShip, model.ChannelPush, model.LangZhCN, "发货通知", "订单 {{order_no}}")
	res := e.enqueueOnce(t, pushReq("k-1", &rpc.Recipient{Mid: 1, TargetRef: "d"}))
	first := e.deliv.Rows()[res.NewDeliveryIDs[0]]
	if first.TemplateVersion != 1 {
		t.Fatalf("锁定版本 = %d", first.TemplateVersion)
	}
	// 发布 v2 后再来一条，锁的是新版本；旧任务仍指向 v1。
	e.tmpl.Seed(&model.NotificationTemplate{
		TemplateCode: codeShip, Channel: model.ChannelPush, Lang: model.LangZhCN,
		TitleTpl: "发货通知2", BodyTpl: "订单 {{order_no}} 已发出", Version: 2, State: model.TemplateStatePublished,
	})
	res2 := e.enqueueOnce(t, pushReq("k-2", &rpc.Recipient{Mid: 2, TargetRef: "d"}))
	if got := e.deliv.Rows()[res2.NewDeliveryIDs[0]].TemplateVersion; got != 2 {
		t.Errorf("新任务版本 = %d, want 2", got)
	}
	if e.deliv.Rows()[first.DeliveryId].TemplateVersion != 1 {
		t.Error("在途任务的模板版本被改写")
	}
}

// TestSendImplementsSender：EventHandler 的发送入口与 gRPC 入口共用同一用例。
func TestSendImplementsSender(t *testing.T) {
	e := newEnv(t, Options{DefaultLanguage: model.LangZhCN})
	e.seedTemplate(codeShip, model.ChannelPush, model.LangZhCN, "发货通知", "订单 {{order_no}}")
	var sender interface {
		Send(ctx context.Context, in *rpc.SendNotificationReq) (*rpc.SendNotificationReply, error)
	} = e.enqueue
	reply, err := sender.Send(context.Background(), pushReq("k-1", &rpc.Recipient{Mid: 1, TargetRef: "d"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.GetDeliveries()) != 1 {
		t.Errorf("reply = %v", reply)
	}
}

// TestNewRejectsBadConfig：时区/默认语言非法必须启动期报错，不能运行期把窗口算偏。
func TestNewRejectsBadConfig(t *testing.T) {
	e := newEnv(t, Options{DefaultLanguage: model.LangZhCN})
	if _, err := New(e.repo, Options{DefaultTimezone: "Mars/Olympus"}); err == nil {
		t.Error("非法时区应报错")
	}
	if _, err := New(e.repo, Options{DefaultLanguage: "klingon"}); err == nil {
		t.Error("非法默认语言应报错")
	}
	if _, err := New(nil, Options{}); err == nil {
		t.Error("缺少 repository 应报错")
	}
	// 默认语言为空时回落中文。
	enq, err := New(e.repo, Options{DefaultTimezone: "UTC"})
	if err != nil || enq == nil {
		t.Fatalf("合法配置应构造成功: %v", err)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func quotaKey(i int) string { return "quota-case-" + strconv.Itoa(i) }

// brokenPref 模拟偏好库不可用：读必须失败，让上层 fail-closed。
type brokenPref struct{}

func (brokenPref) FindOne(context.Context, int64) (*model.NotificationDndPref, error) {
	return nil, errors.New("notification_dnd_pref unavailable")
}
func (brokenPref) Upsert(context.Context, *model.NotificationDndPref) error {
	return errors.New("notification_dnd_pref unavailable")
}
