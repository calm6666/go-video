package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go-video/services/notification/internal/policy"
	"go-video/services/notification/internal/provider"
	"go-video/services/notification/internal/repository"
	"go-video/services/notification/internal/testx"
	"go-video/services/notification/model"
)

// 投递调度器单测（AGENTS.md §9：内容投递必须有注入防护与失败路径测试）。
// 核心断言口径：任何“没能真正投递”的分支都不得把状态写成 sent，
// 失败必须按退避阶梯推进，耗尽后进死信留档。全部内存假件，不连中间件、不发网络。

type denv struct {
	tmpl    *testx.TemplateModel
	deliv   *testx.DeliveryModel
	offset  *testx.OffsetModel
	dead    *testx.DeadLetterModel
	dnd     *testx.DndPrefModel
	quota   *testx.QuotaCounter
	repo    *repository.Repository
	reg     *provider.Registry
	d       *Dispatcher
	fixed   time.Time
	policy_ DispatchPolicy
}

// 固定“当前时刻”：北京 2026-03-14 10:00（UTC 02:00），避开常见免打扰窗口。
var fixedNow = time.Date(2026, 3, 14, 2, 0, 0, 0, time.UTC)

func newDenv(t *testing.T, dp DispatchPolicy, provs ...provider.Provider) *denv {
	t.Helper()
	e := &denv{
		tmpl:   testx.NewTemplateModel(),
		deliv:  testx.NewDeliveryModel(),
		offset: testx.NewOffsetModel(),
		dead:   testx.NewDeadLetterModel(),
		dnd:    testx.NewDndPrefModel(),
		quota:  testx.NewQuotaCounter(),
	}
	e.repo = repository.NewWithModels(e.quota, e.tmpl, e.deliv, e.offset, e.dead, e.dnd)
	e.reg = provider.NewRegistry()
	for _, p := range provs {
		if err := e.reg.Register(p); err != nil {
			t.Fatalf("注册 fake provider: %v", err)
		}
	}
	if dp.DefaultTimezone == "" {
		dp.DefaultTimezone = "Asia/Shanghai"
	}
	if len(dp.BackoffSeconds) == 0 {
		dp.BackoffSeconds = []int64{60, 300, 1800}
	}
	if dp.MaxRetries == 0 {
		dp.MaxRetries = 3
	}
	d, err := NewDispatcher(e.repo, e.reg, provider.NewPassthroughResolver(), dp)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	e.policy_ = d.policy
	e.d = d
	e.fixed = fixedNow
	d.now = func() time.Time { return e.fixed }
	return e
}

// seedTemplate 预置一个已发布模板。
func (e *denv) seedTemplate(code string, channel int32, lang, title, body string, version int32) {
	e.tmpl.Seed(&model.NotificationTemplate{
		TemplateCode: code, Channel: channel, Lang: lang, TitleTpl: title, BodyTpl: body,
		Version: version, State: model.TemplateStatePublished, Operator: "op-1",
	})
}

func params(p map[string]string) string {
	raw, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// seedDelivery 预置一条待投递任务。
func (e *denv) seedDelivery(mut func(*model.NotificationDelivery)) *model.NotificationDelivery {
	row := &model.NotificationDelivery{
		DeliveryId: "DLV-1", BizKey: "bk-1", BizGroupKey: "grp-1",
		Mid: 1001, Channel: model.ChannelPush, TemplateCode: "order_shipped", TemplateVersion: 1,
		Lang: model.LangZhCN, TargetRef: "device-token-1", ParamsJson: params(map[string]string{"order_no": "SO-1"}),
		State: model.DeliveryStatePending, Priority: policy.PriorityNormal, Ctime: e.fixed.Unix(), Mtime: e.fixed.Unix(),
	}
	if mut != nil {
		mut(row)
	}
	e.deliv.Seed(row)
	return row
}

func (e *denv) row(id string) *model.NotificationDelivery {
	t := e.deliv.Rows()[id]
	if t == nil {
		panic("row missing: " + id)
	}
	return t
}

func (e *denv) dispatch(t *testing.T, id string) {
	t.Helper()
	if err := e.d.Dispatch(context.Background(), id); err != nil {
		t.Fatalf("Dispatch(%s): %v", id, err)
	}
}

func acceptedPush() *testx.Provider {
	return &testx.Provider{Chan: provider.ChannelPush, AdapterName: "fake-push", Accepted: true, MsgID: "msg-1"}
}

// TestDispatchProviderNotConfiguredNeverSent：通道适配器未配置时必须显式失败并退避重试，
// 绝不能伪造“发送成功”（这是本服务最容易出错、后果最重的一条路径）。
func TestDispatchProviderNotConfiguredNeverSent(t *testing.T) {
	e := newDenv(t, DispatchPolicy{}) // 不注册任何适配器
	e.seedTemplate("order_shipped", model.ChannelPush, model.LangZhCN, "发货通知", "订单 {{order_no}} 已发货", 1)
	row := e.seedDelivery(nil)

	e.dispatch(t, row.DeliveryId)

	got := e.row(row.DeliveryId)
	if got.State == model.DeliveryStateSent {
		t.Fatal("未配置通道适配器却写成了 sent")
	}
	if got.State != model.DeliveryStateRetry {
		t.Fatalf("state = %d, want retry", got.State)
	}
	if !strings.Contains(got.LastError, provider.ErrProviderNotConfigured.Error()) {
		t.Errorf("last_error 应保留未配置原因, got %q", got.LastError)
	}
	if got.RetryCount != 1 || got.NextRetryAt != e.fixed.Add(60*time.Second).Unix() {
		t.Errorf("首次失败应退避 60s: count=%d next=%d", got.RetryCount, got.NextRetryAt)
	}
	if len(e.dead.Rows()) != 0 {
		t.Error("尚未耗尽重试，不应登记死信")
	}
}

// TestDispatchRetryLadderThenDeadLetter：退避序列逐级拉长，耗尽后转死信并留档。
func TestDispatchRetryLadderThenDeadLetter(t *testing.T) {
	fake := &testx.Provider{Chan: provider.ChannelPush, AdapterName: "fake-push", Err: errors.New("provider 503")}
	e := newDenv(t, DispatchPolicy{}, fake)
	e.seedTemplate("order_shipped", model.ChannelPush, model.LangZhCN, "发货通知", "订单 {{order_no}}", 1)
	row := e.seedDelivery(nil)

	want := []struct {
		count int32
		delta time.Duration
	}{
		{1, 60 * time.Second},
		{2, 300 * time.Second},
	}
	for i, w := range want {
		e.fixed = fixedNow.Add(time.Duration(i+1) * time.Hour) // 跨过领取窗口，模拟下一次到期扫描
		e.dispatch(t, row.DeliveryId)
		got := e.row(row.DeliveryId)
		if got.State != model.DeliveryStateRetry || got.RetryCount != w.count {
			t.Fatalf("第 %d 轮: state=%d count=%d, want retry/%d", i+1, got.State, got.RetryCount, w.count)
		}
		if got.NextRetryAt != e.fixed.Add(w.delta).Unix() {
			t.Errorf("第 %d 轮 next_retry_at = %d, want %d", i+1, got.NextRetryAt, e.fixed.Add(w.delta).Unix())
		}
	}
	// 第 3 次失败：retry_count 达到 MaxRetries=3 -> dead_letter + 死信留档。
	e.fixed = fixedNow.Add(3 * time.Hour)
	e.dispatch(t, row.DeliveryId)
	got := e.row(row.DeliveryId)
	if got.State != model.DeliveryStateDeadLetter {
		t.Fatalf("重试耗尽后 state = %d, want dead_letter", got.State)
	}
	if !strings.Contains(got.LastError, "retries exhausted") {
		t.Errorf("last_error = %q", got.LastError)
	}
	dls := e.dead.Rows()
	if len(dls) != 1 {
		t.Fatalf("死信行数 = %d, want 1", len(dls))
	}
	if dls[0].Source != model.DeadLetterSourceDelivery || dls[0].DeliveryId != row.DeliveryId {
		t.Errorf("死信留档内容异常: %+v", dls[0])
	}
	if dls[0].State != model.DeadLetterStatePending {
		t.Errorf("死信应为待处置状态, got %d", dls[0].State)
	}
	if fake.Calls != 3 {
		t.Errorf("供应商调用次数 = %d, want 3", fake.Calls)
	}
	// 死信是终态：再扫一次不会被重新投递（RetryDeadLetter 才能复活）。
	e.fixed = fixedNow.Add(10 * time.Hour)
	if n, err := e.d.ProcessOnce(context.Background()); err != nil || n != 0 {
		t.Errorf("死信任务不应再被扫描: n=%d err=%v", n, err)
	}
}

// TestDispatchAcceptedMarksSentWithDigest：只有供应商明确受理才写 sent，
// 并落摘要（不落明文正文）、适配器名与回执 ID。
func TestDispatchAcceptedMarksSentWithDigest(t *testing.T) {
	fake := acceptedPush()
	e := newDenv(t, DispatchPolicy{}, fake)
	e.seedTemplate("order_shipped", model.ChannelPush, model.LangZhCN, "发货通知", "订单 {{order_no}} 已发货", 1)
	row := e.seedDelivery(nil)

	e.dispatch(t, row.DeliveryId)

	got := e.row(row.DeliveryId)
	if got.State != model.DeliveryStateSent {
		t.Fatalf("state = %d, want sent", got.State)
	}
	if got.Provider != "fake-push" || got.ProviderMsgId != "msg-1" {
		t.Errorf("回执未落库: %q %q", got.Provider, got.ProviderMsgId)
	}
	wantDigest := policy.Digest("发货通知", "订单 SO-1 已发货")
	if got.PayloadDigest != wantDigest {
		t.Errorf("payload_digest = %q, want %q", got.PayloadDigest, wantDigest)
	}
	if strings.Contains(got.PayloadDigest, "SO-1") || got.ParamsJson == "" {
		t.Error("摘要不得包含正文内容")
	}
	if got.SentAt != e.fixed.Unix() || got.NextRetryAt != 0 {
		t.Errorf("sent_at/next_retry_at 异常: %d %d", got.SentAt, got.NextRetryAt)
	}
	_, calls := fake.Snapshot()
	if calls == nil || calls.IdempotencyKey != got.BizKey || calls.TargetRef != "device-token-1" {
		t.Errorf("发给供应商的请求缺少幂等键或收件标识: %+v", calls)
	}
}

// TestDispatchUnacceptedResultNotSent：适配器返回“未受理”或空回执时一律按失败处理。
func TestDispatchUnacceptedResultNotSent(t *testing.T) {
	t.Run("未受理", func(t *testing.T) {
		fake := &testx.Provider{Chan: provider.ChannelPush, Accepted: false}
		e := newDenv(t, DispatchPolicy{}, fake)
		e.seedTemplate("order_shipped", model.ChannelPush, model.LangZhCN, "t", "订单 {{order_no}}", 1)
		row := e.seedDelivery(nil)
		e.dispatch(t, row.DeliveryId)
		if got := e.row(row.DeliveryId); got.State != model.DeliveryStateRetry {
			t.Errorf("state = %d, want retry（未知结果不得写成已发送）", got.State)
		}
	})
	t.Run("空回执", func(t *testing.T) {
		fake := &testx.Provider{Chan: provider.ChannelPush, NilResult: true}
		e := newDenv(t, DispatchPolicy{}, fake)
		e.seedTemplate("order_shipped", model.ChannelPush, model.LangZhCN, "t", "订单 {{order_no}}", 1)
		row := e.seedDelivery(nil)
		e.dispatch(t, row.DeliveryId)
		if got := e.row(row.DeliveryId); got.State != model.DeliveryStateRetry || got.ProviderMsgId != "" {
			t.Errorf("state=%d msg=%q, want retry 且无回执", got.State, got.ProviderMsgId)
		}
	})
}

// TestDispatchQuietHoursHoldsWithoutBurningRetries：静默时段只做顺延，
// 既不发送也不消耗重试次数（否则一夜之间就把提醒全部打成死信）。
func TestDispatchQuietHoursHoldsWithoutBurningRetries(t *testing.T) {
	fake := acceptedPush()
	e := newDenv(t, DispatchPolicy{DndEnabled: true}, fake)
	e.seedTemplate("order_shipped", model.ChannelPush, model.LangZhCN, "t", "订单 {{order_no}}", 1)
	// fixedNow = 北京 10:00，窗口 09:00-11:00 命中。
	e.dnd.Seed(&model.NotificationDndPref{
		Mid: 1001, State: model.DndStateOn, QuietStart: "09:00", QuietEnd: "11:00", Timezone: "Asia/Shanghai",
	})
	row := e.seedDelivery(nil)

	e.dispatch(t, row.DeliveryId)

	got := e.row(row.DeliveryId)
	if got.State != model.DeliveryStateRetry {
		t.Fatalf("state = %d, want retry（顺延而非拦截）", got.State)
	}
	if got.RetryCount != 0 {
		t.Errorf("顺延不得消耗重试次数, got %d", got.RetryCount)
	}
	if !strings.Contains(got.LastError, "quiet hours") {
		t.Errorf("last_error = %q", got.LastError)
	}
	const hold = 5 * 60
	if got.NextRetryAt != e.fixed.Unix()+hold {
		t.Errorf("顺延步长 = %d, want %d", got.NextRetryAt-e.fixed.Unix(), hold)
	}
	if fake.Calls != 0 {
		t.Error("静默时段内不得调用供应商")
	}
	// 高优先（验证码/安全提醒）越过节静窗口。
	high := e.seedDelivery(func(r *model.NotificationDelivery) {
		r.DeliveryId, r.BizKey, r.Priority = "DLV-H", "bk-h", policy.PriorityHigh
	})
	e.dispatch(t, high.DeliveryId)
	if e.row(high.DeliveryId).State != model.DeliveryStateSent {
		t.Errorf("高优先应发出, got %d", e.row(high.DeliveryId).State)
	}
	// 窗口结束后普通提醒恢复投递。
	e.fixed = fixedNow.Add(2 * time.Hour) // 北京 12:00，出窗
	e.dispatch(t, row.DeliveryId)
	if got := e.row(row.DeliveryId); got.State != model.DeliveryStateSent {
		t.Errorf("出窗后应投递成功, got %d %q", got.State, got.LastError)
	}
}

// TestDispatchMutedChannelSuppressedWithoutSend：用户关闭该通道 -> suppressed 终态，绝不外发。
func TestDispatchMutedChannelSuppressedWithoutSend(t *testing.T) {
	fake := acceptedPush()
	e := newDenv(t, DispatchPolicy{DndEnabled: false}, fake) // DndEnabled=false 也要尊重硬偏好
	e.seedTemplate("order_shipped", model.ChannelPush, model.LangZhCN, "t", "订单 {{order_no}}", 1)
	e.dnd.Seed(&model.NotificationDndPref{
		Mid: 1001, State: model.DndStateOn, MutedChannels: model.MaskOfChannels([]int32{model.ChannelPush}),
	})
	row := e.seedDelivery(func(r *model.NotificationDelivery) { r.Priority = policy.PriorityHigh })

	e.dispatch(t, row.DeliveryId)

	got := e.row(row.DeliveryId)
	if got.State != model.DeliveryStateSuppressed {
		t.Errorf("state = %d, want suppressed", got.State)
	}
	if fake.Calls != 0 {
		t.Error("关闭通道后仍调用了供应商")
	}
}

// TestDispatchPreferenceUnavailableFailsClosed：偏好读不到时宁可不发，
// 也不能打扰一个可能已关闭通道的用户。
func TestDispatchPreferenceUnavailableFailsClosed(t *testing.T) {
	fake := acceptedPush()
	e := newDenv(t, DispatchPolicy{DndEnabled: true}, fake)
	e.seedTemplate("order_shipped", model.ChannelPush, model.LangZhCN, "t", "订单 {{order_no}}", 1)
	// 重新装配一个偏好库不可用的调度器（fake 注入故障，不连中间件）。
	e.repo = repository.NewWithModels(e.quota, e.tmpl, e.deliv, e.offset, e.dead, brokenPref{})
	d, err := NewDispatcher(e.repo, e.reg, provider.NewPassthroughResolver(), DispatchPolicy{
		DndEnabled: true, DefaultTimezone: "Asia/Shanghai", BackoffSeconds: []int64{60, 300, 1800}, MaxRetries: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	d.now = func() time.Time { return e.fixed }
	e.d = d
	row := e.seedDelivery(nil)

	if err := d.Dispatch(context.Background(), row.DeliveryId); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	got := e.row(row.DeliveryId)
	if got.State == model.DeliveryStateSent {
		t.Fatal("偏好不可用却写成 sent")
	}
	if got.State != model.DeliveryStateSuppressed || !strings.Contains(got.LastError, "preference unavailable") {
		t.Errorf("state=%d reason=%q, want suppressed + 原因", got.State, got.LastError)
	}
	if fake.Calls != 0 {
		t.Error("fail-closed 时不得调用供应商")
	}
}

// TestDispatchExpiredSuppressed：过期任务不再打扰用户。
func TestDispatchExpiredSuppressed(t *testing.T) {
	fake := acceptedPush()
	e := newDenv(t, DispatchPolicy{}, fake)
	e.seedTemplate("order_shipped", model.ChannelPush, model.LangZhCN, "t", "订单 {{order_no}}", 1)
	row := e.seedDelivery(func(r *model.NotificationDelivery) { r.ExpireAt = e.fixed.Unix() - 1 })

	e.dispatch(t, row.DeliveryId)

	if got := e.row(row.DeliveryId); got.State != model.DeliveryStateSuppressed ||
		!strings.Contains(got.LastError, "expired") {
		t.Errorf("state=%d reason=%q", got.State, got.LastError)
	}
	if fake.Calls != 0 {
		t.Error("过期任务不得外发")
	}
}

// TestDispatchContactNotWiredRetriesNotDeadLetters：
// account.v1 没有取联系方式的 RPC，sms 只能显式失败并按退避等待契约补齐，不能判死也不能假成功。
func TestDispatchContactNotWiredRetriesNotDeadLetters(t *testing.T) {
	fake := &testx.Provider{Chan: provider.ChannelSMS, Accepted: true}
	e := newDenv(t, DispatchPolicy{}, fake)
	e.seedTemplate("sms_code", model.ChannelSMS, model.LangZhCN, "t", "验证码 {{code}}", 1)
	row := e.seedDelivery(func(r *model.NotificationDelivery) {
		r.DeliveryId, r.BizKey, r.Channel, r.TemplateCode, r.TargetRef, r.ParamsJson =
			"DLV-S", "bk-s", model.ChannelSMS, "sms_code", "", params(map[string]string{"code": "123456"})
	})

	e.dispatch(t, row.DeliveryId)

	got := e.row(row.DeliveryId)
	if got.State != model.DeliveryStateRetry {
		t.Fatalf("state = %d, want retry", got.State)
	}
	if !strings.Contains(got.LastError, "contact source not wired") {
		t.Errorf("last_error = %q", got.LastError)
	}
	if fake.Calls != 0 {
		t.Error("联系方式都没解析成功却调用了供应商")
	}
	// 非法 target_ref（含换行）属于配置级错误 -> suppressed，不无限重试。
	bad := e.seedDelivery(func(r *model.NotificationDelivery) {
		r.DeliveryId, r.BizKey, r.Channel, r.TemplateCode, r.TargetRef = "DLV-B", "bk-b", model.ChannelPush, "order_shipped", "bad\nref"
	})
	e.dispatch(t, bad.DeliveryId)
	if got := e.row(bad.DeliveryId); got.State != model.DeliveryStateSuppressed {
		t.Errorf("畸形 target_ref 应 suppressed, got %d", got.State)
	}
}

// TestDispatchPermanentTemplateProblemGoesDeadLetter：
// 模板版本丢失/变量缺失属于“重试也不会变好”的错误，直接进死信交人工处理。
func TestDispatchPermanentTemplateProblemGoesDeadLetter(t *testing.T) {
	fake := acceptedPush()
	e := newDenv(t, DispatchPolicy{}, fake)
	e.seedTemplate("order_shipped", model.ChannelPush, model.LangZhCN, "t", "订单 {{order_no}} 由 {{courier}} 配送", 1)
	row := e.seedDelivery(func(r *model.NotificationDelivery) {
		r.ParamsJson = params(map[string]string{"order_no": "SO-1"}) // 缺 courier
	})
	e.dispatch(t, row.DeliveryId)
	got := e.row(row.DeliveryId)
	if got.State != model.DeliveryStateDeadLetter {
		t.Errorf("变量缺失应直接进死信, got %d", got.State)
	}
	if fake.Calls != 0 {
		t.Error("渲染失败不得调用供应商")
	}
	if len(e.dead.Rows()) != 1 {
		t.Errorf("死信留档 = %d 条", len(e.dead.Rows()))
	}

	missing := e.seedDelivery(func(r *model.NotificationDelivery) {
		r.DeliveryId, r.BizKey, r.TemplateVersion = "DLV-M", "bk-m", 99 // 没有这个版本
	})
	e.dispatch(t, missing.DeliveryId)
	if got := e.row(missing.DeliveryId); got.State != model.DeliveryStateDeadLetter {
		t.Errorf("模板版本缺失应进死信, got %d", got.State)
	}

	// params_json 被写坏（非法 JSON）也属于永久错误。
	broken := e.seedDelivery(func(r *model.NotificationDelivery) {
		r.DeliveryId, r.BizKey, r.ParamsJson = "DLV-J", "bk-j", "{not json"
	})
	e.dispatch(t, broken.DeliveryId)
	if got := e.row(broken.DeliveryId); got.State != model.DeliveryStateDeadLetter {
		t.Errorf("params_json 非法应进死信, got %d", got.State)
	}
}

// TestDispatchSkipsTerminalRows：终态任务不得被再次投递（多实例/重放的安全底线）。
func TestDispatchSkipsTerminalRows(t *testing.T) {
	fake := acceptedPush()
	e := newDenv(t, DispatchPolicy{}, fake)
	e.seedTemplate("order_shipped", model.ChannelPush, model.LangZhCN, "t", "订单 {{order_no}}", 1)
	for _, st := range []int32{model.DeliveryStateSent, model.DeliveryStateSuppressed, model.DeliveryStateFailed} {
		id := "DLV-T" + string(rune('0'+st))
		e.seedDelivery(func(r *model.NotificationDelivery) { r.DeliveryId, r.BizKey = id, "bk"+id; r.State = st })
		e.dispatch(t, id)
		if got := e.row(id); got.State != st {
			t.Errorf("终态 %d 被改写为 %d", st, got.State)
		}
	}
	if fake.Calls != 0 {
		t.Errorf("终态任务调用了供应商 %d 次", fake.Calls)
	}
}

// TestDispatchClaimWindowPreventsDoubleSend：Dispatch 领取任务后把 next_retry_at 推出领取窗口，
// 同一轮里第二个实例扫描不到该任务，因此供应商只被调用一次。
func TestDispatchClaimWindowPreventsDoubleSend(t *testing.T) {
	fake := acceptedPush()
	e := newDenv(t, DispatchPolicy{Batch: 10}, fake)
	e.seedTemplate("order_shipped", model.ChannelPush, model.LangZhCN, "t", "订单 {{order_no}}", 1)
	e.seedDelivery(func(r *model.NotificationDelivery) { r.DeliveryId, r.BizKey = "DLV-A", "bk-a" })
	e.seedDelivery(func(r *model.NotificationDelivery) { r.DeliveryId, r.BizKey = "DLV-B", "bk-b" })
	ctx := context.Background()

	n, err := e.d.ProcessOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("首轮处理 %d 条", n)
	}
	// 时钟未推进：两条任务已被领取（next_retry_at = now + ClaimGrace），第二轮扫不到。
	if n2, err := e.d.ProcessOnce(ctx); err != nil || n2 != 0 {
		t.Errorf("领取窗口失效：第二轮处理了 %d 条 (err=%v)", n2, err)
	}
	if fake.Calls != 2 {
		t.Errorf("供应商调用次数 = %d, want 2（每条只投一次）", fake.Calls)
	}
}

// TestProcessOnceRespectsDueTimeAndPriority：未到 next_retry_at 的任务不得被扫描；
// 到期任务按优先级降序、next_retry_at 升序处理。
func TestProcessOnceRespectsDueTimeAndPriority(t *testing.T) {
	fake := acceptedPush()
	e := newDenv(t, DispatchPolicy{Batch: 10}, fake)
	e.seedTemplate("order_shipped", model.ChannelPush, model.LangZhCN, "t", "订单 {{order_no}}", 1)
	e.seedDelivery(func(r *model.NotificationDelivery) {
		r.DeliveryId, r.BizKey, r.State, r.Priority = "DLV-NOTDUE", "bk-nd", model.DeliveryStateRetry, policy.PriorityHigh
		r.NextRetryAt = e.fixed.Add(time.Hour).Unix()
	})
	e.seedDelivery(func(r *model.NotificationDelivery) {
		r.DeliveryId, r.BizKey, r.Priority = "DLV-LOW", "bk-low", policy.PriorityLow
	})
	e.seedDelivery(func(r *model.NotificationDelivery) {
		r.DeliveryId, r.BizKey, r.Priority = "DLV-HIGH", "bk-high", policy.PriorityHigh
	})

	if n, err := e.d.ProcessOnce(context.Background()); err != nil || n != 2 {
		t.Fatalf("只应处理到期的 2 条: n=%d err=%v", n, err)
	}
	if got := e.row("DLV-NOTDUE"); got.State != model.DeliveryStateRetry {
		t.Errorf("未到期任务被改动: %d", got.State)
	}
	if len(fake.Reqs) == 0 {
		t.Fatal("无投递记录")
	}
	// 高优先先投。
	if fake.Reqs[0].DeliveryID != "DLV-HIGH" {
		t.Errorf("首次投递 = %s, want DLV-HIGH", fake.Reqs[0].DeliveryID)
	}
}

// TestNewDispatcherRejectsBadTimezone：时区写错必须启动期失败，不能退化成 UTC。
func TestNewDispatcherRejectsBadTimezone(t *testing.T) {
	e := newDenv(t, DispatchPolicy{})
	if _, err := NewDispatcher(nil, e.reg, nil, DispatchPolicy{}); err == nil {
		t.Error("缺少 repository 应报错")
	}
	if _, err := NewDispatcher(e.repo, nil, nil, DispatchPolicy{DefaultTimezone: "Mars/Olympus"}); err == nil {
		t.Error("非法时区应报错而不是退化 UTC")
	}
	// 空时区回落 UTC 是允许的（配置项有默认值，回落只针对未填写）。
	d, err := NewDispatcher(e.repo, nil, nil, DispatchPolicy{DefaultTimezone: "  "})
	if err != nil || d == nil {
		t.Errorf("空时区应回落 UTC: %v", err)
	}
	// 非法退避阶梯会被清洗成默认值，不会算出 0 退避。
	d2, err := NewDispatcher(e.repo, nil, nil, DispatchPolicy{DefaultTimezone: "UTC", BackoffSeconds: []int64{0, -1}})
	if err != nil {
		t.Fatal(err)
	}
	if got := policy.BackoffDelay(1, d2.policy.BackoffSeconds); got <= 0 {
		t.Errorf("退避阶梯未清洗: %v", got)
	}
}

// TestDispatchUnblocksAfterProviderConfigured：运维补配适配器后，
// 之前 retry 的任务会自动恢复投递（证明“未配置”没有被误判成永久失败）。
func TestDispatchUnblocksAfterProviderConfigured(t *testing.T) {
	e := newDenv(t, DispatchPolicy{}) // 初始无适配器
	e.seedTemplate("order_shipped", model.ChannelPush, model.LangZhCN, "t", "订单 {{order_no}}", 1)
	row := e.seedDelivery(nil)
	e.dispatch(t, row.DeliveryId)
	if got := e.row(row.DeliveryId); got.State != model.DeliveryStateRetry {
		t.Fatalf("首轮应 retry, got %d", got.State)
	}
	fake := acceptedPush()
	if err := e.reg.Register(fake); err != nil {
		t.Fatal(err)
	}
	e.fixed = e.fixed.Add(time.Hour)
	e.dispatch(t, row.DeliveryId)
	if got := e.row(row.DeliveryId); got.State != model.DeliveryStateSent {
		t.Errorf("补配后应恢复投递, got %d %q", got.State, got.LastError)
	}
}

// brokenPref 模拟偏好表不可用（读失败必须 fail-closed）。
type brokenPref struct{}

func (brokenPref) FindOne(context.Context, int64) (*model.NotificationDndPref, error) {
	return nil, errors.New("notification_dnd_pref unavailable")
}
func (brokenPref) Upsert(context.Context, *model.NotificationDndPref) error {
	return errors.New("notification_dnd_pref unavailable")
}
