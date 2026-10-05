package logic

// SendNotification 用例。校验/幂等/配额/偏好的判定链在 internal/send 的**真实** Enqueuer 里
// （fakes_test.go 用 repository.NewWithModels 装配），本文件因此能证明 logic 的两条独有职责：
// ① 把 Enqueuer 的显式拒绝原样上抛（不吞错、不降级成“看起来受理了”）；
// ② SyncSend 只对本次新建且待投递的任务立即投一次，投递失败绝不改写已受理的响应。

import (
	"errors"
	"testing"
	"time"

	"go-video/services/notification/internal/config"
	"go-video/services/notification/internal/policy"
	"go-video/services/notification/internal/provider"
	"go-video/services/notification/internal/repository"
	"go-video/services/notification/internal/send"
	"go-video/services/notification/internal/testx"
	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

// quietWindowAroundNow 返回以当前时刻为中心、前后各 30 分钟的免打扰窗口（HH:MM）。
//
// Dispatcher 的时钟字段是包内私有的，logic 测试无法注入固定时刻，因此窗口按墙钟派生；
// ±30 分钟的余量远大于单条用例耗时，跨零点时落到 InQuietHours 的跨天分支，仍然覆盖当前时刻。
// 注意：本 helper 只服务「静默时段应顺延而非拦截」这一条断言；
// 配额与关闭通道的判定发生在入队时、与墙钟无关，不需要这类窗口。
func quietWindowAroundNow(t *testing.T) (start, end string) {
	t.Helper()
	loc, err := policy.LoadLocation(tzShanghai, "")
	wantNoErr(t, "加载测试时区", err)
	now := time.Now().In(loc)
	return now.Add(-30 * time.Minute).Format("15:04"), now.Add(30 * time.Minute).Format("15:04")
}

// seedQuietWindow 预置一条「当前时刻正处于免打扰时段」的偏好。
func (e *env) seedQuietWindow(t *testing.T, mid int64) {
	t.Helper()
	start, end := quietWindowAroundNow(t)
	e.dnd.Seed(&model.NotificationDndPref{
		Mid: mid, QuietStart: start, QuietEnd: end, Timezone: tzShanghai,
		State: model.DndStateOn, Ctime: 1000, Mtime: 1000,
	})
}

// TestSendNotificationAcceptedRow 正常路径：落库一条待投递任务，字段快照与请求一致。
func TestSendNotificationAcceptedRow(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)

	from := e.mark()
	reply, err := e.send(t, pushReq("ship-1", recip(midAlice, "device-token-a")))
	wantNoErr(t, "SendNotification", err)

	if len(reply.GetDeliveries()) != 1 {
		t.Fatalf("deliveries 条数 = %d, want 1", len(reply.GetDeliveries()))
	}
	got := reply.GetDeliveries()[0]
	wantEQ(t, "响应", "state", got.GetState(), rpc.DeliveryState_DELIVERY_PENDING)
	wantEQ(t, "响应", "duplicate", reply.GetDuplicate(), false)
	wantEQ(t, "响应", "suppressed", reply.GetSuppressed(), int32(0))
	wantEQ(t, "响应", "channel", got.GetChannel(), rpc.Channel_CHANNEL_PUSH)
	wantEQ(t, "响应", "mid", got.GetMid(), midAlice)
	wantEQ(t, "响应", "template_code", got.GetTemplateCode(), codeShip)
	wantEQ(t, "响应", "trace_id", got.GetTraceId(), "trace-1")
	// 幂等键是 sha256 派生值，不断言字面量，只断言「已派生且稳定长度」，
	// 断重复投递要落到同一行由下面的回放用例证明。
	wantEQ(t, "响应", "biz_key 长度", len(got.GetBizKey()), 64)

	row := e.onlyDelivery(t)
	wantEQ(t, "落库行", "state", row.State, model.DeliveryStatePending)
	wantEQ(t, "落库行", "biz_group_key", row.BizGroupKey, "ship-1")
	wantEQ(t, "落库行", "lang", row.Lang, model.LangZhCN)
	// 版本必须锁定：发布新版本后旧任务仍按当时版本渲染。
	wantEQ(t, "落库行", "template_version", row.TemplateVersion, int32(1))
	wantEQ(t, "落库行", "params_json", row.ParamsJson, `{"order_no":"SO-1"}`)
	wantEQ(t, "落库行", "target_ref", row.TargetRef, "device-token-a")
	// RPC 直投没有来源事件。
	wantEQ(t, "落库行", "source_event_id", row.SourceEventId, "")
	wantTrue(t, "落库行", "ctime 已由生产代码填充", row.Ctime > 0)

	wantOps(t, "读库顺序", e.ops(from), []string{
		"tmpl.FindByState:order_shipped/1/zh-CN/st2",
		"dnd.FindOne:1001",
		"deliv.Insert:gk=ship-1/mid1001",
	})
}

// TestSendNotificationQuotaUnavailableFailsClosed 配额存储不可用时必须整批拒绝：
// 既不能降级成“跳过频控继续发”，也不能留下半条任务。
func TestSendNotificationQuotaUnavailableFailsClosed(t *testing.T) {
	e := newEnv(t, withNotify(func(c *config.NotificationConf) { c.DailyQuotaPerMid = 5 }))
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)
	redisDown := errors.New("redis: connection refused")
	e.quota.Fail = redisDown

	from := e.mark()
	_, err := e.send(t, pushReq("ship-1", recip(midAlice, "device-token-a")))
	wantErrIs(t, "配额不可用", err, repository.ErrQuotaUnavailable)
	wantErrIs(t, "错误链保留原始故障", err, redisDown)
	wantEQ(t, "配额不可用", "投递表行数", e.deliveryCount(), 0)
	wantOps(t, "拒绝前只读过偏好与配额", e.ops(from), []string{
		"tmpl.FindByState:order_shipped/1/zh-CN/st2",
		"dnd.FindOne:1001",
		"quota.Incr",
	})
}

// TestSendNotificationOverQuotaSuppressed 超配额是“重试也不会变好”，落终态 suppressed
// 并给出原因，而不是无限排队。
func TestSendNotificationOverQuotaSuppressed(t *testing.T) {
	e := newEnv(t, withNotify(func(c *config.NotificationConf) { c.DailyQuotaPerMid = 1 }))
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)

	first, err := e.send(t, pushReq("ship-1", recip(midAlice, "device-token-a")))
	wantNoErr(t, "首条请求", err)
	wantEQ(t, "首条请求", "state", first.GetDeliveries()[0].GetState(), rpc.DeliveryState_DELIVERY_PENDING)

	from := e.mark()
	second, err := e.send(t, pushReq("ship-2", recip(midAlice, "device-token-a")))
	wantNoErr(t, "第二条请求", err)
	wantEQ(t, "第二条请求", "suppressed 计数", second.GetSuppressed(), int32(1))
	row2 := second.GetDeliveries()[0]
	wantEQ(t, "第二条请求", "state", row2.GetState(), rpc.DeliveryState_DELIVERY_SUPPRESSED)
	wantContains(t, "第二条请求", "last_error", row2.GetLastError(), "over daily quota")

	rows := e.deliveriesByMid(t, midAlice)
	if len(rows) != 2 {
		t.Fatalf("投递表行数 = %d, want 2", len(rows))
	}
	wantEQ(t, "落库行", "第二条状态", rows[len(rows)-1].State, model.DeliveryStateSuppressed)
	// 超配额仍会落一行 suppressed（保留可审计的拦截原因），但判定链止于配额：
	// 不再重试设 TTL、也不触达任何状态流转 UPDATE。
	wantOps(t, "第二条请求的调用序列", e.ops(from), []string{
		"tmpl.FindByState:order_shipped/1/zh-CN/st2",
		"dnd.FindOne:1001",
		"quota.Incr",
		"deliv.Insert:gk=ship-2/mid1001",
	})
	// 配额用尽后不再新建待投递任务。
	wantEQ(t, "落库行", "待投递行数", countState(rows, model.DeliveryStatePending), 1)
	wantEQ(t, "配额调用次数", "quota.Incr", e.tr.Count("quota.Incr"), 2)
	wantEQ(t, "首次计数设 TTL", "quota.Expire", e.tr.Count("quota.Expire:172800"), 1)
}

// TestSendNotificationBizKeyReplayAddsNoRow biz_key 幂等回放：同一请求重发返回既有行，
// 不新增任务、也不因为 SyncSend 二次外发。
func TestSendNotificationBizKeyReplayAddsNoRow(t *testing.T) {
	push := newProvider(provider.ChannelPush, "msg-1")
	e := newEnv(t, withSyncSend(push))
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)

	in := pushReq("ship-1", recip(midAlice, "device-token-a"))
	first, err := e.send(t, in)
	wantNoErr(t, "首次请求", err)
	wantEQ(t, "首次请求", "duplicate", first.GetDuplicate(), false)
	wantEQ(t, "首次请求", "provider 调用次数", push.Calls, 1)
	deliveryID := first.GetDeliveries()[0].GetDeliveryId()

	from := e.mark()
	replay, err := e.send(t, in)
	wantNoErr(t, "回放请求", err)
	wantEQ(t, "回放请求", "duplicate", replay.GetDuplicate(), true)
	wantEQ(t, "回放请求", "返回同一 delivery_id", replay.GetDeliveries()[0].GetDeliveryId(), deliveryID)
	wantEQ(t, "回放请求", "投递表行数", e.deliveryCount(), 1)
	// 关键不变量：幂等命中不得再次外发（否则用户收到两条相同提醒）。
	wantEQ(t, "回放请求", "provider 调用次数", push.Calls, 1)
	wantOps(t, "回放只做唯一键回查", e.ops(from), []string{
		"tmpl.FindByState:order_shipped/1/zh-CN/st2",
		"dnd.FindOne:1001",
		"deliv.Insert:gk=ship-1/mid1001",
		"deliv.FindByBizKey",
	})
}

// TestSendNotificationSameMidTwoDevicesBothAccepted 行级幂等键包含设备标识：
// 同一 mid 的两台设备必须是两行任务，否则第二台设备永远收不到通知。
func TestSendNotificationSameMidTwoDevicesBothAccepted(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)

	_, err := e.send(t, pushReq("ship-1",
		recip(midAlice, "device-token-a"), recip(midAlice, "device-token-b")))
	wantNoErr(t, "同 mid 两设备", err)
	wantEQ(t, "同 mid 两设备", "投递表行数", e.deliveryCount(), 2)

	// 同一请求里重复的接收人只投一次（行级唯一索引会拒绝第二行）。
	_, err = e.send(t, pushReq("ship-2", recip(midBob, "tok"), recip(midBob, "tok")))
	wantNoErr(t, "同请求重复接收人", err)
	wantEQ(t, "同请求重复接收人", "投递表行数", e.deliveryCount(), 3)
}

// TestSendNotificationMutedChannelSuppressedAtEnqueue 用户显式关闭通道属于硬偏好：
// 入队即落 suppressed，且绝不触达供应商（不是“发出去再说”，也不是静默丢弃请求）。
func TestSendNotificationMutedChannelSuppressedAtEnqueue(t *testing.T) {
	push := newProvider(provider.ChannelPush, "msg-1")
	e := newEnv(t, withSyncSend(push))
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)
	e.seedMuted(t, midAlice, model.ChannelPush)

	_, err := e.send(t, pushReq("ship-1", recip(midAlice, "device-token-a")))
	wantNoErr(t, "关闭通道仍应受理", err)
	row := e.onlyDelivery(t)
	wantEQ(t, "落库行", "state", row.State, model.DeliveryStateSuppressed)
	wantContains(t, "落库行", "last_error", row.LastError, "channel muted by user preference")
	wantEQ(t, "关闭通道", "provider 调用次数", push.Calls, 0)
	// suppressed 属于已落库终态，不进 SyncSend 的立即投递集合。
	wantEQ(t, "关闭通道", "deliv.MarkSent 次数", e.tr.Count("deliv.MarkSent:"+row.DeliveryId), 0)
}

// TestSendNotificationQuietHoursDefersNotSuppress 免打扰时段只顺延、不丢消息：
// 入队阶段不判时段（判时段在 Dispatcher），落库仍是待投递；同步投递时被 held 成 retry。
func TestSendNotificationQuietHoursDefersNotSuppress(t *testing.T) {
	push := newProvider(provider.ChannelPush, "msg-1")
	e := newEnv(t, withSyncSend(push))
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)
	e.seedQuietWindow(t, midAlice)

	reply, err := e.send(t, pushReq("ship-1", recip(midAlice, "device-token-a")))
	wantNoErr(t, "免打扰时段内请求", err)
	wantEQ(t, "免打扰时段", "suppressed 计数", reply.GetSuppressed(), int32(0))

	row := e.onlyDelivery(t)
	// 关键：既不是 suppressed（永久丢弃），也不是 sent（打扰了用户），而是 retry（等窗口结束）。
	wantEQ(t, "免打扰时段", "state", row.State, model.DeliveryStateRetry)
	wantEQ(t, "免打扰时段", "provider 调用次数", push.Calls, 0)
	wantContains(t, "免打扰时段", "last_error", row.LastError, "held: within quiet hours")
	// 顺延不消耗重试次数，否则用户会因时段长短被误判成投递失败。
	wantEQ(t, "免打扰时段", "retry_count", row.RetryCount, int32(0))
	wantTrue(t, "免打扰时段", "next_retry_at 已被推后", row.NextRetryAt > 0)
}

// TestSendNotificationHighPriorityCrossesQuietHours 高优先（验证码/安全提醒）可越过时段静音，
// 但通道硬偏好仍然生效 —— 两条边界不能写反。
func TestSendNotificationHighPriorityCrossesQuietHours(t *testing.T) {
	push := newProvider(provider.ChannelPush, "msg-1")
	e := newEnv(t, withSyncSend(push))
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)
	e.seedQuietWindow(t, midAlice)

	in := pushReq("ship-1", recip(midAlice, "device-token-a"))
	in.Priority = rpc.Priority_PRIORITY_HIGH
	m := e.mark()
	reply, err := e.send(t, in)
	wantNoErr(t, "高优先请求", err)
	// 契约：deliveries 反映「任务已 durable 落库」，不是供应商回执（见 logic 的头注释）。
	wantEQ(t, "高优先", "响应状态（入队快照）",
		reply.GetDeliveries()[0].GetState(), rpc.DeliveryState_DELIVERY_PENDING)
	// 真实结果只能从库里读：高优先越过了免打扰时段，确实投出去了。
	row := e.onlyDelivery(t)
	wantEQ(t, "高优先", "落库状态", row.State, model.DeliveryStateSent)
	wantEQ(t, "高优先", "provider 调用次数", push.Calls, 1)
	// 投递写入的真实链是两步，不是一步：Dispatcher.Dispatch 在调用供应商之前
	// 先做一次「领取」写入（dispatcher.go:236 MarkDeliveryRetry，把 next_retry_at
	// 推到 ClaimGrace 之外，多实例下只有一个 worker 真正发送），
	// 发送成功后再由 send 落 MarkSent（dispatcher.go:330）。
	// 这不是 logic 侧多余的写：去掉它 SyncSend 与后台扫描就会对同一行并发外发。
	// 领取写用的是原 retry_count（不消耗重试次数），所以下面仍断 retry_count=0。
	wantOps(t, "高优先投递写入（领取 MarkRetry 先于 MarkSent）", e.opsContaining(m, "deliv.Mark"),
		[]string{
			"deliv.MarkRetry:" + row.DeliveryId,
			"deliv.MarkSent:" + row.DeliveryId,
		})
	wantEQ(t, "高优先", "领取不消耗重试次数", row.RetryCount, int32(0))
	wantContains(t, "高优先", "落库回执供应商", row.Provider, "fake-push")

	// 同一偏好换成「用户关闭 push 通道」：高优先也不能发。
	e.seedMuted(t, midBob, model.ChannelPush)
	in2 := pushReq("ship-2", recip(midBob, "device-token-b"))
	in2.Priority = rpc.Priority_PRIORITY_HIGH
	_, err = e.send(t, in2)
	wantNoErr(t, "高优先 + 关闭通道请求", err)
	wantEQ(t, "高优先 + 关闭通道", "provider 调用次数", push.Calls, 1)
}

// TestSendNotificationValidationRejectedExplicitly 入参校验失败必须显式报错且零落库，
// 不允许“返回空 deliveries + nil 错误”这种看起来成功的写法。
func TestSendNotificationValidationRejectedExplicitly(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)

	cases := []struct {
		name  string
		build func() *rpc.SendNotificationReq
		want  error
		msg   string
	}{
		{"未指定通道", func() *rpc.SendNotificationReq {
			r := pushReq("ship-1", recip(midAlice, "tok"))
			r.Channel = rpc.Channel_CHANNEL_UNSPECIFIED
			return r
		}, model.ErrInvalidChannel, "channel=0"},
		{"缺少幂等键", func() *rpc.SendNotificationReq {
			return pushReq("", recip(midAlice, "tok"))
		}, nil, "biz_key or idempotency_key is required"},
		{"没有接收人", func() *rpc.SendNotificationReq { return pushReq("ship-1") }, send.ErrNoRecipients, ""},
		{"空指针接收人", func() *rpc.SendNotificationReq {
			return pushReq("ship-1", nil)
		}, send.ErrRecipientNil, ""},
		{"接收人无法定位", func() *rpc.SendNotificationReq {
			return pushReq("ship-1", &rpc.Recipient{})
		}, send.ErrNoRecipientTarget, ""},
		{"过期时间已过", func() *rpc.SendNotificationReq {
			r := pushReq("ship-1", recip(midAlice, "tok"))
			r.ExpireAt = time.Now().Unix() - 60
			return r
		}, send.ErrExpiredRequest, ""},
		{"参数含明文手机号", func() *rpc.SendNotificationReq {
			r := pushReq("ship-1", recip(midAlice, "tok"))
			r.TemplateParams = map[string]string{"order_no": "13800001234"}
			return r
		}, policy.ErrSensitiveParam, ""},
		{"参数尝试注入占位符", func() *rpc.SendNotificationReq {
			r := pushReq("ship-1", recip(midAlice, "tok"))
			r.TemplateParams = map[string]string{"order_no": "{{admin_note}}"}
			return r
		}, policy.ErrRenderUnknownVar, ""},
		{"模板缺版本", func() *rpc.SendNotificationReq {
			r := pushReq("ship-1", recip(midAlice, "tok"))
			r.TemplateCode = "no_such_template"
			return r
		}, model.ErrTemplateNotFound, ""},
		{"变量缺失", func() *rpc.SendNotificationReq {
			r := pushReq("ship-1", recip(midAlice, "tok"))
			r.TemplateParams = map[string]string{}
			return r
		}, policy.ErrRenderMissingVar, "order_no"},
	}

	for _, tc := range cases {
		from := e.mark()
		reply, err := e.send(t, tc.build())
		switch {
		case err == nil:
			t.Errorf("%s：期望显式错误，实际 reply=%v err=nil", tc.name, reply)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%s：错误 = %v, want errors.Is(..., %v)", tc.name, err, tc.want)
		case tc.msg != "" && !contains(err.Error(), tc.msg):
			t.Errorf("%s：错误 %q 未包含线索 %q", tc.name, err.Error(), tc.msg)
		}
		if reply != nil {
			t.Errorf("%s：拒绝时不得返回半成品 reply，实际 %v", tc.name, reply)
		}
		if n := e.deliveryCount(); n != 0 {
			t.Fatalf("%s：拒绝后投递表行数 = %d, want 0", tc.name, n)
		}
		if got := e.opsContaining(from, "deliv.Insert"); len(got) != 0 {
			t.Errorf("%s：拒绝路径不得写投递表，实际 %v", tc.name, got)
		}
	}
}

// TestSendNotificationStorageErrorPropagates 存储错误原样上抛，
// 不能因为“已经分配了 delivery_id”就报成功。
func TestSendNotificationStorageErrorPropagates(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)
	storeDown := errors.New("insert notification_delivery: down")
	e.delivSpy.Fail("Insert", storeDown)

	_, err := e.send(t, pushReq("ship-1", recip(midAlice, "device-token-a")))
	wantErrIs(t, "写库失败", err, storeDown)
	wantEQ(t, "写库失败", "投递表行数", e.deliveryCount(), 0)
}

// TestSendNotificationPrefReadFailureRejectsBatch 偏好读不到时 fail-closed 拒绝整批：
// 数据源抖动既不能把通知写成成功，也不能把用户永久拦在门外。
func TestSendNotificationPrefReadFailureRejectsBatch(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)
	prefDown := errors.New("select notification_dnd_pref: timeout")
	e.dndSpy.Fail("FindOne", prefDown)

	_, err := e.send(t, pushReq("ship-1", recip(midAlice, "device-token-a")))
	wantErrContains(t, "偏好读失败", err, "拒绝投递")
	wantErrIs(t, "偏好读失败", err, prefDown)
	wantEQ(t, "偏好读失败", "投递表行数", e.deliveryCount(), 0)
}

// TestSendNotificationSyncSendDispatchesOnce SyncSend：落库后立即投一次，
// 供应商回执（适配器名/消息 ID/内容摘要）写回同一行，响应与落库一致。
func TestSendNotificationSyncSendDispatchesOnce(t *testing.T) {
	push := newProvider(provider.ChannelPush, "msg-88")
	e := newEnv(t, withSyncSend(push))
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)

	reply, err := e.send(t, pushReq("ship-1", recip(midAlice, "device-token-a")))
	wantNoErr(t, "SyncSend", err)
	// 契约：deliveries 只表示「任务已 durable 落库」，同步投递的结果**不回填响应**
	// （见 sendnotificationlogic.go 的方法注释；失败路径用例同样断 PENDING）。
	// 因此这里断响应为入队态，而「同步确实投出去了」由下面的行状态与 provider 调用次数证明。
	wantEQ(t, "SyncSend", "响应状态", reply.GetDeliveries()[0].GetState(), rpc.DeliveryState_DELIVERY_PENDING)

	row := e.onlyDelivery(t)
	wantEQ(t, "SyncSend", "provider", row.Provider, "fake-push")
	wantEQ(t, "SyncSend", "provider_msg_id", row.ProviderMsgId, "msg-88")
	wantEQ(t, "SyncSend", "state", row.State, model.DeliveryStateSent)
	wantTrue(t, "SyncSend", "sent_at 已回写", row.SentAt > 0)
	// 隐私不变量：落库的是渲染结果摘要，明文正文不进 notification_delivery。
	wantEQ(t, "SyncSend", "payload_digest", row.PayloadDigest, policy.Digest(tplTitle, "订单 SO-1 已发货"))
	wantNotContains(t, "SyncSend", row.ParamsJson, "SO-1 已发货")

	_, last := push.Snapshot()
	if last == nil {
		t.Fatal("provider 未收到请求")
	}
	wantEQ(t, "SyncSend", "外发标题", last.Title, tplTitle)
	wantEQ(t, "SyncSend", "外发正文", last.Body, "订单 SO-1 已发货")
	wantEQ(t, "SyncSend", "外发通道", last.Channel, provider.ChannelPush)
	wantEQ(t, "SyncSend", "外发幂等键", last.IdempotencyKey, row.BizKey)
	wantEQ(t, "SyncSend", "provider 调用次数", push.Calls, 1)
}

// TestSendNotificationSyncSendFailureKeepsAcceptedReply 投递失败不得把已受理的请求报成错误：
// 那会诱导调用方换 biz_key 重发，造成重复打扰；事实由状态行承载。
func TestSendNotificationSyncSendFailureKeepsAcceptedReply(t *testing.T) {
	push := &testx.Provider{Chan: provider.ChannelPush, AdapterName: "fake-push", Err: errors.New("厂商限流")}
	e := newEnv(t, withSyncSend(push))
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)

	reply, err := e.send(t, pushReq("ship-1", recip(midAlice, "device-token-a")))
	wantNoErr(t, "厂商失败仍应返回受理", err)
	wantEQ(t, "厂商失败", "响应状态", reply.GetDeliveries()[0].GetState(), rpc.DeliveryState_DELIVERY_PENDING)

	row := e.onlyDelivery(t)
	wantEQ(t, "厂商失败", "落库状态", row.State, model.DeliveryStateRetry)
	wantContains(t, "厂商失败", "last_error", row.LastError, "厂商限流")
	wantEQ(t, "厂商失败", "retry_count", row.RetryCount, int32(1))
	wantEQ(t, "厂商失败", "provider 调用次数", push.Calls, 1)
}

// TestSendNotificationSyncSendWithoutProviderDoesNotFakeSuccess 通道适配器未配置时
// 任务必须留在可恢复状态（retry），绝不写成 sent。
func TestSendNotificationSyncSendWithoutProviderDoesNotFakeSuccess(t *testing.T) {
	sms := newProvider(provider.ChannelSMS, "sms-1")
	e := newEnv(t, withSyncSend(sms)) // 只注册了 sms 适配器
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)

	_, err := e.send(t, pushReq("ship-1", recip(midAlice, "device-token-a")))
	wantNoErr(t, "push 无适配器", err)
	row := e.onlyDelivery(t)
	wantEQ(t, "push 无适配器", "state", row.State, model.DeliveryStateRetry)
	wantEQ(t, "push 无适配器", "provider", row.Provider, "")
	wantEQ(t, "push 无适配器", "sms 适配器调用次数", sms.Calls, 0)
}

// TestSendNotificationSyncSendWithoutDispatcherLeavesTaskPending
// 配置组合 SyncSend=true 而 DispatcherEnabled=false 时，logic 只记日志、把任务留给下次扫描，
// 既不能报错误（任务已 durable 落库），也不能自己去伪造投递结果。
func TestSendNotificationSyncSendWithoutDispatcherLeavesTaskPending(t *testing.T) {
	push := newProvider(provider.ChannelPush, "msg-1")
	e := newEnv(t,
		// 只注册适配器，不打开 Dispatcher：复现「运维误配」这一组合。
		withProviders(push),
		withNotify(func(c *config.NotificationConf) {
			c.SyncSend = true
			c.DispatcherEnabled = false
		}),
	)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)

	if e.svcCtx.Dispatcher != nil {
		t.Fatal("装配与预期不符：DispatcherEnabled=false 时不应有 Dispatcher")
	}

	reply, err := e.send(t, pushReq("ship-1", recip(midAlice, "device-token-a")))
	wantNoErr(t, "无 Dispatcher 的 SyncSend", err)
	wantEQ(t, "无 Dispatcher 的 SyncSend", "响应状态",
		reply.GetDeliveries()[0].GetState(), rpc.DeliveryState_DELIVERY_PENDING)
	row := e.onlyDelivery(t)
	wantEQ(t, "无 Dispatcher 的 SyncSend", "落库状态", row.State, model.DeliveryStatePending)
	wantEQ(t, "无 Dispatcher 的 SyncSend", "不外发", push.Calls, 0)
	wantEQ(t, "无 Dispatcher 的 SyncSend", "deliv.MarkSent 次数", e.tr.CountPrefix("deliv.MarkSent"), 0)
}

// countState 统计某状态的行数（假件返回快照，可安全遍历）。
func countState(rows []*model.NotificationDelivery, state int32) int {
	n := 0
	for _, r := range rows {
		if r.State == state {
			n++
		}
	}
	return n
}
