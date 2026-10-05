package logic

// UpdateDndPreference / GetDndPreference 用例：偏好读写两侧的真实口径。
//
// 这两个方法是「免打扰」的唯一入口，但判定发生在 internal/send 与 internal/consumer，
// 所以本文件重点证明三件事：
//   ① 写侧真的落库（读回而不是回显请求）、全量覆盖 + 幂等（ON DUPLICATE KEY 不动 ctime）；
//   ② 读侧无记录时返回可渲染的默认值 + found=false，而不是 nil 或报错；
//   ③ 写侧的通道位掩码与投递侧的 IsChannelMuted 必须同口径 ——
//      掩码写错的方向要么是「用户关了通道仍然被骚扰」，要么是「关掉 push 结果 sms 也没了」。

import (
	"context"
	"errors"
	"testing"
	"time"

	"go-video/services/notification/internal/policy"
	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

func (e *env) callUpdateDnd(t *testing.T, in *rpc.UpdateDndPreferenceReq) (*rpc.UpdateDndPreferenceReply, error) {
	t.Helper()
	return NewUpdateDndPreferenceLogic(context.Background(), e.svcCtx).UpdateDndPreference(in)
}

func (e *env) callGetDnd(t *testing.T, in *rpc.GetDndPreferenceReq) (*rpc.GetDndPreferenceReply, error) {
	t.Helper()
	return NewGetDndPreferenceLogic(context.Background(), e.svcCtx).GetDndPreference(in)
}

// TestUpdateDndPreferenceWritesThenReadsBack 正常写入：一次 Upsert + 一次读回，
// 响应里的 preference 必须是「库里的值」（ctime/mtime 只有落库后才有），
// 而不是把请求字段原样回显。
func TestUpdateDndPreferenceWritesThenReadsBack(t *testing.T) {
	e := newEnv(t)

	m := e.mark()
	reply, err := e.callUpdateDnd(t, &rpc.UpdateDndPreferenceReq{
		Mid: midAlice, MutedChannels: []rpc.Channel{rpc.Channel_CHANNEL_PUSH, rpc.Channel_CHANNEL_EMAIL},
		QuietStart: "22:30", QuietEnd: "07:10", Timezone: tzShanghai, Enabled: true,
	})
	wantNoErr(t, "写偏好", err)
	wantOps(t, "写偏好：先覆盖再读回", e.ops(m), []string{"dnd.Upsert:1001", "dnd.FindOne:1001"})

	saved := e.pref(t, midAlice)
	if saved == nil {
		t.Fatal("偏好未落库")
	}
	wantEQ(t, "落库行", "muted_channels",
		saved.MutedChannels, model.MaskOfChannels([]int32{model.ChannelPush, model.ChannelEmail}))
	wantEQ(t, "落库行", "quiet_start", saved.QuietStart, "22:30")
	wantEQ(t, "落库行", "quiet_end", saved.QuietEnd, "07:10")
	wantEQ(t, "落库行", "timezone", saved.Timezone, tzShanghai)
	// enabled=true 必须落成 state=on：Dispatcher 只在 state=on 时才判时段。
	wantEQ(t, "落库行", "state", saved.State, model.DndStateOn)
	wantTrue(t, "落库行", "ctime 由 repository 填充", saved.Ctime > 0)
	wantTrue(t, "落库行", "mtime 由 repository 刷新", saved.Mtime > 0)

	got := reply.GetPreference()
	wantEQ(t, "响应", "mid", got.GetMid(), midAlice)
	wantEQ(t, "响应", "enabled", got.GetEnabled(), true)
	wantEQ(t, "响应", "timezone", got.GetTimezone(), tzShanghai)
	wantEQ(t, "响应", "quiet_start", got.GetQuietStart(), "22:30")
	// 位掩码必须还原成通道列表（客户端按列表渲染「已关闭」开关）。
	wantChannelsEQ(t, "响应 muted_channels",
		got.GetMutedChannels(), []rpc.Channel{rpc.Channel_CHANNEL_PUSH, rpc.Channel_CHANNEL_EMAIL})
	wantTrue(t, "响应", "ctime 来自读回而不是请求回显", got.GetCtime() > 0)
}

// TestUpdateDndPreferenceIsFullOverwriteAndIdempotent 全量覆盖语义：
// 重复提交同一请求结果一致（幂等），且第二次不得把 ctime 改掉
// —— 真实 SQL 是 ON DUPLICATE KEY UPDATE ... 且不含 ctime 列（model/notificationdndpref.go:64）。
// 关掉全部通道 = 传空列表，不是“再传一次旧值”。
func TestUpdateDndPreferenceIsFullOverwriteAndIdempotent(t *testing.T) {
	e := newEnv(t)
	in := &rpc.UpdateDndPreferenceReq{
		Mid: midAlice, MutedChannels: []rpc.Channel{rpc.Channel_CHANNEL_SMS},
		QuietStart: "23:00", QuietEnd: "07:00", Timezone: tzShanghai, Enabled: true,
	}
	first, err := e.callUpdateDnd(t, in)
	wantNoErr(t, "首次提交", err)

	m := e.mark()
	second, err := e.callUpdateDnd(t, in)
	wantNoErr(t, "重复提交", err)
	wantOps(t, "重复提交仍是覆盖 + 读回", e.ops(m), []string{"dnd.Upsert:1001", "dnd.FindOne:1001"})

	wantEQ(t, "重复提交", "ctime 不被改写", second.GetPreference().GetCtime(), first.GetPreference().GetCtime())
	wantEQ(t, "重复提交", "muted_channels 稳定",
		second.GetPreference().GetMutedChannels()[0], rpc.Channel_CHANNEL_SMS)
	// 偏好表按 mid 一行：重复提交不得产生第二行（读回仍是同一条）。
	saved := e.pref(t, midAlice)
	wantEQ(t, "重复提交", "state 仍为开启", saved.State, model.DndStateOn)

	// 清空静音：传空列表就是「全部通道允许」，必须覆盖掉旧掩码。
	_, err = e.callUpdateDnd(t, &rpc.UpdateDndPreferenceReq{Mid: midAlice, Timezone: tzShanghai})
	wantNoErr(t, "清空偏好", err)
	cleared := e.pref(t, midAlice)
	wantEQ(t, "清空偏好", "muted_channels", cleared.MutedChannels, int32(0))
	wantEQ(t, "清空偏好", "state 关闭", cleared.State, model.DndStateOff)
	wantEQ(t, "清空偏好", "quiet_start 清空", cleared.QuietStart, "")
}

// TestUpdateDndPreferenceDefaultsEmptyTimezone 时区留空必须回落服务默认时区，
// 而不是把空串落库 —— Dispatcher 用 pref.Timezone 定位免打扰窗口，
// 空串会让它退回配置默认值，但偏好行本身要能自解释（GetDndPreference 直接回给客户端）。
func TestUpdateDndPreferenceDefaultsEmptyTimezone(t *testing.T) {
	e := newEnv(t)
	_, err := e.callUpdateDnd(t, &rpc.UpdateDndPreferenceReq{
		Mid: midBob, QuietStart: "08:00", QuietEnd: "08:00", Enabled: true,
	})
	wantNoErr(t, "未填时区", err)
	saved := e.pref(t, midBob)
	wantEQ(t, "未填时区", "timezone 回落配置默认", saved.Timezone, tzShanghai)
	// start==end 属于「无效配置但不报错」的输入，落库原样保留，
	// 由 policy.InQuietHours 判定为「不设时段」（dnd.go:81）；这里锁住口径不外溢。
	wantEQ(t, "未填时区", "quiet_start", saved.QuietStart, "08:00")
	wantTrue(t, "未填时区", "InQuietHours 不判静音", func() bool {
		loc, lerr := policy.LoadLocation(saved.Timezone, "")
		if lerr != nil {
			return false
		}
		in, ierr := policy.InQuietHours(time.Now(), loc, saved.QuietStart, saved.QuietEnd)
		return ierr == nil && !in
	}())
}

// TestUpdateDndPreferenceRejectsInvalidRequests 入参校验必须零写入：
// 偏好是「一用户一行」的覆盖式写入，校验阶段落库会把用户已有设置冲掉。
func TestUpdateDndPreferenceRejectsInvalidRequests(t *testing.T) {
	e := newEnv(t)
	// 先铺一条“用户已设置好”的偏好，拒绝路径必须原样保留它。
	e.seedMuted(t, midAlice, model.ChannelPush)

	cases := []struct {
		name string
		req  *rpc.UpdateDndPreferenceReq
		want error
		msg  string
	}{
		{"未给 mid", &rpc.UpdateDndPreferenceReq{Timezone: tzShanghai}, nil, "mid is required"},
		{"mid 为负", &rpc.UpdateDndPreferenceReq{Mid: -1}, nil, "mid is required"},
		{"不支持的通道枚举", &rpc.UpdateDndPreferenceReq{Mid: midAlice,
			MutedChannels: []rpc.Channel{rpc.Channel(9)}}, model.ErrInvalidChannel, ""},
		{"只给 quiet_start", &rpc.UpdateDndPreferenceReq{Mid: midAlice, QuietStart: "22:00"},
			ErrQuietTimePair, ""},
		{"只给 quiet_end", &rpc.UpdateDndPreferenceReq{Mid: midAlice, QuietEnd: "07:00"},
			ErrQuietTimePair, ""},
		{"时段小时越界", &rpc.UpdateDndPreferenceReq{Mid: midAlice, QuietStart: "24:00",
			QuietEnd: "07:00"}, policy.ErrInvalidClock, ""},
		// 注意不是 "7:00"：policy.ParseClock 按 ":" 切分后 strconv.Atoi，
		// "7:00" 会被接受成 07:00（dnd.go:27-35），只有完全缺冒号才判非法。
		{"时段缺冒号", &rpc.UpdateDndPreferenceReq{Mid: midAlice, QuietStart: "0700",
			QuietEnd: "22:00"}, policy.ErrInvalidClock, `must be HH:MM`},
		{"时区不存在", &rpc.UpdateDndPreferenceReq{Mid: midAlice, Timezone: "Mars/Olympus"},
			policy.ErrInvalidTimezone, `"Mars/Olympus"`},
	}
	for _, tc := range cases {
		m := e.mark()
		got, err := e.callUpdateDnd(t, tc.req)
		switch {
		case err == nil:
			t.Errorf("%s：期望显式错误，实际 reply=%+v", tc.name, got)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%s：错误 = %v, want errors.Is(..., %v)", tc.name, err, tc.want)
		case tc.msg != "" && !contains(err.Error(), tc.msg):
			t.Errorf("%s：错误 %q 未包含线索 %q", tc.name, err.Error(), tc.msg)
		}
		if got != nil {
			t.Errorf("%s：拒绝时不得返回偏好，实际 %+v", tc.name, got)
		}
		// 校验先于读库：非法请求既不查也不写。
		wantOps(t, tc.name+"：零读写", e.ops(m), nil)
		wantEQ(t, tc.name, "原有偏好未被冲掉", e.pref(t, midAlice).MutedChannels, model.MaskPush)
	}

	// nil 请求同样显式报错。
	if _, err := e.callUpdateDnd(t, nil); err == nil {
		t.Error("nil 请求必须报错")
	} else {
		wantErrContains(t, "nil 请求", err, "nil request")
	}
}

// TestUpdateDndPreferenceStorageFailures 两类存储故障的口径不同，必须分开钉：
//   - 写入失败：一行都不能改，也不做读回；
//   - 读回失败：写入已经生效，此时必须报错（不能返回“看起来成功”的 preference=nil），
//     但库里的值以真实落库结果为准。
func TestUpdateDndPreferenceStorageFailures(t *testing.T) {
	writeDown := errors.New("notification_dnd_pref Upsert: dead")
	e := newEnv(t)
	e.seedMuted(t, midAlice, model.ChannelPush)
	e.dndSpy.Fail("Upsert", writeDown)

	m := e.mark()
	in := &rpc.UpdateDndPreferenceReq{Mid: midAlice, QuietStart: "20:00", QuietEnd: "21:00",
		Timezone: tzShanghai, Enabled: true}
	reply, err := e.callUpdateDnd(t, in)
	wantErrIs(t, "写入失败", err, writeDown)
	if reply != nil {
		t.Errorf("写入失败时不得返回偏好，实际 %+v", reply)
	}
	wantOps(t, "写入失败不做读回", e.ops(m), []string{"dnd.Upsert:1001"})
	// 库存量原样保留：静音位没有被新请求覆盖掉。
	wantEQ(t, "写入失败", "原掩码保留", e.pref(t, midAlice).MutedChannels, model.MaskPush)
	wantEQ(t, "写入失败", "原时段保留", e.pref(t, midAlice).QuietStart, "")

	// 撤销故障后同一条请求可以正常重放（不是“一次失败永久拉黑”）。
	e.dndSpy.Recover("Upsert")
	m2 := e.mark()
	ok, err := e.callUpdateDnd(t, in)
	wantNoErr(t, "恢复后重放", err)
	wantOps(t, "恢复后重放", e.ops(m2), []string{"dnd.Upsert:1001", "dnd.FindOne:1001"})
	wantEQ(t, "恢复后重放", "时段已写入", ok.GetPreference().GetQuietStart(), "20:00")
	// 全量覆盖语义的代价：这次请求没带 muted_channels，旧的静音位就被清成 0。
	// 契约要求调用方每次提交完整通道集合，本用例把这条容易踩的口径钉在测试里。
	wantEQ(t, "恢复后重放", "静音位被清空（全量覆盖）", e.pref(t, midAlice).MutedChannels, int32(0))

	// 读回失败：写入已经落库，错误必须上抛。
	readDown := errors.New("notification_dnd_pref FindOne: timeout")
	e.dndSpy.Fail("FindOne", readDown)
	m3 := e.mark()
	reply3, err := e.callUpdateDnd(t, &rpc.UpdateDndPreferenceReq{
		Mid: midBob, MutedChannels: []rpc.Channel{rpc.Channel_CHANNEL_EMAIL}, Timezone: tzShanghai})
	wantErrIs(t, "读回失败", err, readDown)
	if reply3 != nil {
		t.Errorf("读回失败时不得返回半成品偏好，实际 %+v", reply3)
	}
	wantOps(t, "读回失败也要走完两次调用", e.ops(m3), []string{"dnd.Upsert:1002", "dnd.FindOne:1002"})
	// 库里确实写进去了（静默读回，不经过 Spy）。
	wantEQ(t, "读回失败", "落库掩码", e.pref(t, midBob).MutedChannels, model.MaskEmail)
}

// TestUpdateDndPreferenceMaskRoundTripsThroughDelivery 写侧掩码与投递侧判定的闭环：
// 关掉短信通道后，push 仍要能发（掩码不能串位），sms 必须被 suppressed。
// 光断 dnd 表的位掩码数值证明不了这一点 —— 掩码写歪一位在单表里完全看不出来。
func TestUpdateDndPreferenceMaskRoundTripsThroughDelivery(t *testing.T) {
	e := newEnv(t)
	e.seedPublished(t, codeShip, model.ChannelPush, model.LangZhCN, tplTitle, tplBody)
	e.seedPublished(t, codeShip, model.ChannelSMS, model.LangZhCN, tplTitle, tplBody)

	_, err := e.callUpdateDnd(t, &rpc.UpdateDndPreferenceReq{
		Mid: midAlice, MutedChannels: []rpc.Channel{rpc.Channel_CHANNEL_SMS}, Timezone: tzShanghai,
	})
	wantNoErr(t, "关闭短信通道", err)

	pushRow, err := e.send(t, pushReq("ship-1", recip(midAlice, "device-token-a")))
	wantNoErr(t, "push 不受短信静音影响", err)
	wantEQ(t, "push 不受短信静音影响", "state",
		pushRow.GetDeliveries()[0].GetState(), rpc.DeliveryState_DELIVERY_PENDING)

	sms := pushReq("sms-1", recip(midAlice, "device-token-a"))
	sms.Channel = rpc.Channel_CHANNEL_SMS
	smsReply, err := e.send(t, sms)
	wantNoErr(t, "sms 被静音拦截", err)
	wantEQ(t, "sms 被静音拦截", "suppressed 计数", smsReply.GetSuppressed(), int32(1))
	wantEQ(t, "sms 被静音拦截", "落库状态",
		e.deliveryByGroup(t, "sms-1").State, model.DeliveryStateSuppressed)
	wantContains(t, "sms 被静音拦截", "last_error",
		e.deliveryByGroup(t, "sms-1").LastError, "channel muted by user preference")
	wantEQ(t, "sms 被静音拦截", "push 行数不变", len(e.deliveriesByMid(t, midAlice)), 2)
}

// TestGetDndPreferenceDefaultsWhenUnset 从未设置过的用户：found=false + 可直接渲染的默认值。
// 关键契约是「不报错、也不返回 nil preference」，同时不得伪造 enabled=true 或静音通道。
func TestGetDndPreferenceDefaultsWhenUnset(t *testing.T) {
	e := newEnv(t)
	// 表里有别的用户的设置，证明默认值不是“顺手返回了别人的行”。
	e.seedMuted(t, midBob, model.ChannelPush, model.ChannelSMS)

	m := e.mark()
	got, err := e.callGetDnd(t, &rpc.GetDndPreferenceReq{Mid: midAlice})
	wantNoErr(t, "无偏好", err)
	wantOps(t, "无偏好只做一次查询", e.ops(m), []string{"dnd.FindOne:1001"})
	wantEQ(t, "无偏好", "found", got.GetFound(), false)
	pref := got.GetPreference()
	if pref == nil {
		t.Fatal("preference 不得为 nil：客户端要直接渲染表单")
	}
	wantEQ(t, "无偏好", "mid", pref.GetMid(), midAlice)
	wantEQ(t, "无偏好", "enabled", pref.GetEnabled(), false)
	wantEQ(t, "无偏好", "timezone 取服务默认", pref.GetTimezone(), tzShanghai)
	wantEQ(t, "无偏好", "静音通道数", len(pref.GetMutedChannels()), 0)
	wantEQ(t, "无偏好", "quiet_start", pref.GetQuietStart(), "")
	wantEQ(t, "无偏好", "ctime 不伪造", pref.GetCtime(), int64(0))
}

// TestGetDndPreferenceReadsStoredRow 有记录时如实投影：found=true、
// 位掩码还原成通道列表（升序）、enabled 只由 state 决定。
func TestGetDndPreferenceReadsStoredRow(t *testing.T) {
	e := newEnv(t)
	e.dnd.Seed(&model.NotificationDndPref{
		Mid: midAlice, MutedChannels: model.MaskOfChannels([]int32{model.ChannelEmail, model.ChannelPush}),
		QuietStart: "22:00", QuietEnd: "08:00", Timezone: "UTC", State: model.DndStateOn,
		Ctime: 1000, Mtime: 2000,
	})

	m := e.mark()
	got, err := e.callGetDnd(t, &rpc.GetDndPreferenceReq{Mid: midAlice})
	wantNoErr(t, "有偏好", err)
	wantOps(t, "有偏好", e.ops(m), []string{"dnd.FindOne:1001"})
	wantEQ(t, "有偏好", "found", got.GetFound(), true)
	pref := got.GetPreference()
	wantEQ(t, "有偏好", "enabled", pref.GetEnabled(), true)
	wantEQ(t, "有偏好", "timezone", pref.GetTimezone(), "UTC")
	wantEQ(t, "有偏好", "quiet_end", pref.GetQuietEnd(), "08:00")
	wantEQ(t, "有偏好", "ctime", pref.GetCtime(), int64(1000))
	wantEQ(t, "有偏好", "mtime", pref.GetMtime(), int64(2000))
	// 掩码里的位必须按通道升序还原，顺序稳定客户端才不会闪。
	wantChannelsEQ(t, "有偏好 muted_channels", pref.GetMutedChannels(),
		[]rpc.Channel{rpc.Channel_CHANNEL_PUSH, rpc.Channel_CHANNEL_EMAIL})

	// 读侧必须是值拷贝：改返回结构不得污染库存行（假件纪律 ① 的可见证据）。
	pref.QuietStart = "tampered"
	wantEQ(t, "读侧返回拷贝", "库存 quiet_start 不变", e.pref(t, midAlice).QuietStart, "22:00")

	// state=off 的用户即使留着时段，enabled 也必须是 false（总开关优先于时段）。
	e.dnd.Seed(&model.NotificationDndPref{
		Mid: midBob, QuietStart: "22:00", QuietEnd: "08:00", Timezone: "UTC",
		State: model.DndStateOff, Ctime: 1000, Mtime: 1000,
	})
	off, err := e.callGetDnd(t, &rpc.GetDndPreferenceReq{Mid: midBob})
	wantNoErr(t, "关闭总开关", err)
	wantEQ(t, "关闭总开关", "found", off.GetFound(), true)
	wantEQ(t, "关闭总开关", "enabled", off.GetPreference().GetEnabled(), false)
}

// TestGetDndPreferenceValidationAndStoreError mid 校验先于读库；读库失败原样上抛，
// 绝不退化成「查不到 = 没设置过」的默认值（那会让客户端把已关通道的人显示成全开）。
func TestGetDndPreferenceValidationAndStoreError(t *testing.T) {
	e := newEnv(t)
	e.seedMuted(t, midAlice, model.ChannelPush)

	m := e.mark()
	_, err := e.callGetDnd(t, &rpc.GetDndPreferenceReq{})
	wantErrContains(t, "mid 缺失", err, "mid is required")
	_, err = e.callGetDnd(t, nil)
	wantErrContains(t, "nil 请求", err, "nil request")
	wantOps(t, "入参校验不读库", e.ops(m), nil)

	prefDown := errors.New("notification_dnd_pref FindOne: db down")
	e.dndSpy.Fail("FindOne", prefDown)
	m2 := e.mark()
	reply, err := e.callGetDnd(t, &rpc.GetDndPreferenceReq{Mid: midAlice})
	wantErrIs(t, "偏好读失败", err, prefDown)
	if reply != nil {
		t.Errorf("读库失败时不得返回默认偏好冒充「没设置过」，实际 %+v", reply)
	}
	wantOps(t, "偏好读失败", e.ops(m2), []string{"dnd.FindOne:1001"})
}
