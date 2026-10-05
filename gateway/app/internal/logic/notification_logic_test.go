package logic

import (
	"context"
	"errors"
	"testing"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	notificationrpc "go-video/services/notification/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 notification 的口径：终端只暴露本人通道偏好与免打扰设置，
// Channel 枚举与 int32 通道码的双向转换、found 语义、信封四字段。
// 打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库（AGENTS.md §9）。

type fakeNotificationClient struct {
	notificationrpc.NotificationClient

	err   error
	calls int

	getReq    *notificationrpc.GetDndPreferenceReq
	getReply  *notificationrpc.GetDndPreferenceReply
	updateReq *notificationrpc.UpdateDndPreferenceReq
	update    *notificationrpc.UpdateDndPreferenceReply
}

func (f *fakeNotificationClient) GetDndPreference(_ context.Context, in *notificationrpc.GetDndPreferenceReq,
	_ ...grpc.CallOption) (*notificationrpc.GetDndPreferenceReply, error) {
	f.calls++
	f.getReq = in
	return f.getReply, f.err
}

func (f *fakeNotificationClient) UpdateDndPreference(_ context.Context, in *notificationrpc.UpdateDndPreferenceReq,
	_ ...grpc.CallOption) (*notificationrpc.UpdateDndPreferenceReply, error) {
	f.calls++
	f.updateReq = in
	return f.update, f.err
}

func TestNotificationGetDndProjectsChannelEnumAndFound(t *testing.T) {
	fake := &fakeNotificationClient{getReply: &notificationrpc.GetDndPreferenceReply{
		Preference: &notificationrpc.DndPreference{
			Mid:           777,
			MutedChannels: []notificationrpc.Channel{notificationrpc.Channel_CHANNEL_SMS, notificationrpc.Channel_CHANNEL_EMAIL},
			QuietStart:    "22:00",
			QuietEnd:      "08:00",
			Timezone:      "Asia/Shanghai",
			Enabled:       true,
			Ctime:         1700,
			Mtime:         1800,
		},
		Found: true,
	}}
	l := NewGetDndPreferenceLogic(context.Background(), &svc.ServiceContext{Notification: fake})
	resp, err := l.GetDndPreference(&types.ParamGetDnd{Mid: 777})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.getReq.GetMid() != 777 {
		t.Fatalf("mid = %d, want 777", fake.getReq.GetMid())
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}
	if !resp.Data.Found {
		t.Fatal("found 必须投影服务侧结果")
	}
	got := resp.Data.Preference
	// 枚举 → JSON int32：客户端按通道码渲染，不能拿到 protobuf 枚举类型。
	if len(got.MutedChannels) != 2 || got.MutedChannels[0] != int32(notificationrpc.Channel_CHANNEL_SMS) ||
		got.MutedChannels[1] != int32(notificationrpc.Channel_CHANNEL_EMAIL) {
		t.Fatalf("muted_channels = %v, want [2 3]", got.MutedChannels)
	}
	if got.Mid != 777 || got.QuietStart != "22:00" || got.QuietEnd != "08:00" ||
		got.Timezone != "Asia/Shanghai" || !got.Enabled || got.Ctime != 1700 || got.Mtime != 1800 {
		t.Fatalf("preference 投影不完整: %+v", got)
	}
}

func TestNotificationGetDndMissingPreferenceKeepsEmptyListNotNil(t *testing.T) {
	fake := &fakeNotificationClient{getReply: &notificationrpc.GetDndPreferenceReply{Found: false}}
	l := NewGetDndPreferenceLogic(context.Background(), &svc.ServiceContext{Notification: fake})
	resp, err := l.GetDndPreference(&types.ParamGetDnd{Mid: 777})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// preference 为 nil（从未设置）也必须投影成零值 + 空数组，避免客户端解引用 null。
	if resp.Data.Found {
		t.Fatal("found = true, want false")
	}
	if resp.Data.Preference.MutedChannels == nil || len(resp.Data.Preference.MutedChannels) != 0 {
		t.Fatalf("muted_channels = %v, want 空切片", resp.Data.Preference.MutedChannels)
	}
	if resp.Data.Preference.Mid != 0 || resp.Data.Preference.Timezone != "" {
		t.Fatalf("网关不该伪造默认偏好: %+v", resp.Data.Preference)
	}
}

func TestNotificationUpdateDndConvertsChannelsFullOverwrite(t *testing.T) {
	fake := &fakeNotificationClient{update: &notificationrpc.UpdateDndPreferenceReply{
		Preference: &notificationrpc.DndPreference{
			Mid:           777,
			MutedChannels: []notificationrpc.Channel{notificationrpc.Channel_CHANNEL_PUSH},
			Enabled:       false,
			Ctime:         1700,
			Mtime:         1900,
		},
	}}
	l := NewUpdateDndPreferenceLogic(context.Background(), &svc.ServiceContext{Notification: fake})
	resp, err := l.UpdateDndPreference(&types.ParamUpdateDnd{
		Mid:           777,
		MutedChannels: []int32{int32(notificationrpc.Channel_CHANNEL_PUSH)},
		QuietStart:    "",
		QuietEnd:      "",
		Timezone:      "Asia/Shanghai",
		Enabled:       false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// int32 通道码 → Channel 枚举必须显式转换，且列表原样覆盖（全量覆盖语义）。
	if len(fake.updateReq.GetMutedChannels()) != 1 ||
		fake.updateReq.GetMutedChannels()[0] != notificationrpc.Channel_CHANNEL_PUSH {
		t.Fatalf("muted_channels = %v, want [CHANNEL_PUSH]", fake.updateReq.GetMutedChannels())
	}
	if fake.updateReq.GetMid() != 777 || fake.updateReq.GetTimezone() != "Asia/Shanghai" ||
		fake.updateReq.GetEnabled() {
		t.Fatalf("UpdateDndPreference 入参未透传: %+v", fake.updateReq)
	}
	// 空通道列表走同一条转换路径：请求侧为空、响应侧投影成 []，表示全部允许而不是 null。
	empty := &fakeNotificationClient{update: &notificationrpc.UpdateDndPreferenceReply{
		Preference: &notificationrpc.DndPreference{Mid: 777},
	}}
	emptyResp, err := NewUpdateDndPreferenceLogic(context.Background(), &svc.ServiceContext{Notification: empty}).
		UpdateDndPreference(&types.ParamUpdateDnd{Mid: 777})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := empty.updateReq.GetMutedChannels(); len(got) != 0 {
		t.Fatalf("muted_channels = %v, want 空", got)
	}
	if emptyResp.Data.Preference.MutedChannels == nil || len(emptyResp.Data.Preference.MutedChannels) != 0 {
		t.Fatalf("muted_channels = %v, want 空切片", emptyResp.Data.Preference.MutedChannels)
	}
	if resp.Data.Preference.MutedChannels[0] != int32(notificationrpc.Channel_CHANNEL_PUSH) {
		t.Fatalf("回显 preference = %+v", resp.Data.Preference)
	}
	// 写入成功后设置必然存在；UpdateDndPreferenceReply 无 found 字段，网关固定回 true。
	if !resp.Data.Found {
		t.Fatal("更新成功后 found 必须为 true")
	}
	if empty.calls != 1 || fake.calls != 1 {
		t.Fatalf("calls = %d/%d, want 1/1", fake.calls, empty.calls)
	}
}

func TestNotificationDndLogicsWithoutClientConfigured(t *testing.T) {
	empty := &svc.ServiceContext{}
	if _, err := NewGetDndPreferenceLogic(context.Background(), empty).
		GetDndPreference(&types.ParamGetDnd{Mid: 777}); err == nil {
		t.Fatal("getDndPreference：未配置 NotificationRPC 时必须报错")
	}
	if _, err := NewUpdateDndPreferenceLogic(context.Background(), empty).
		UpdateDndPreference(&types.ParamUpdateDnd{Mid: 777}); err == nil {
		t.Fatal("updateDndPreference：未配置 NotificationRPC 时必须报错，不能假装已保存")
	}
}

func TestNotificationDndLogicsPropagateDownstreamError(t *testing.T) {
	sentinel := errors.New("notification: unknown channel")
	fake := &fakeNotificationClient{err: sentinel}
	if _, err := NewGetDndPreferenceLogic(context.Background(), &svc.ServiceContext{Notification: fake}).
		GetDndPreference(&types.ParamGetDnd{Mid: 777}); !errors.Is(err, sentinel) {
		t.Fatalf("getDndPreference err = %v, want %v", err, sentinel)
	}
	if _, err := NewUpdateDndPreferenceLogic(context.Background(), &svc.ServiceContext{Notification: fake}).
		UpdateDndPreference(&types.ParamUpdateDnd{Mid: 777}); !errors.Is(err, sentinel) {
		t.Fatalf("updateDndPreference err = %v, want %v（HH:MM/时区合法性由服务判定后原样上抛）", err, sentinel)
	}
}
