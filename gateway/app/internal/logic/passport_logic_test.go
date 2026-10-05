package logic

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	accountrpc "go-video/services/account/rpc"

	"google.golang.org/grpc"
)

// 本文件覆盖网关侧 passport 补充路由（设置/重置/历史校验/登录记录）：
// RSA 密文原样透传、回复投影、以及最重要的安全口径——响应体与错误路径都不得回显口令。
// 打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库（AGENTS.md §9）。

const (
	passportOldPwdCipher  = "RSA_BASE64_OLD_CIPHER"
	passportNewPwdCipher  = "RSA_BASE64_NEW_CIPHER"
	passportHistoryCipher = "RSA_BASE64_HISTORY_1,RSA_BASE64_HISTORY_2"
)

type fakeAccountClient struct {
	accountrpc.AccountClient

	setPasswordReq    *accountrpc.SetPasswordReq
	resetPasswordReq  *accountrpc.ResetPasswordReq
	checkHistoryReq   *accountrpc.CheckHistoryPwdReq
	checkHistoryReply *accountrpc.CheckHistoryPwdReply
	loginLogsReq      *accountrpc.LoginLogsReq
	loginLogsReply    *accountrpc.LoginLogsReply
	calls             int
	err               error
}

func (f *fakeAccountClient) SetPassword(_ context.Context, in *accountrpc.SetPasswordReq,
	_ ...grpc.CallOption) (*accountrpc.DelCacheReply, error) {
	f.calls++
	f.setPasswordReq = in
	return &accountrpc.DelCacheReply{}, f.err
}

func (f *fakeAccountClient) ResetPassword(_ context.Context, in *accountrpc.ResetPasswordReq,
	_ ...grpc.CallOption) (*accountrpc.DelCacheReply, error) {
	f.calls++
	f.resetPasswordReq = in
	return &accountrpc.DelCacheReply{}, f.err
}

func (f *fakeAccountClient) CheckHistoryPassword(_ context.Context, in *accountrpc.CheckHistoryPwdReq,
	_ ...grpc.CallOption) (*accountrpc.CheckHistoryPwdReply, error) {
	f.calls++
	f.checkHistoryReq = in
	return f.checkHistoryReply, f.err
}

func (f *fakeAccountClient) LoginLogs(_ context.Context, in *accountrpc.LoginLogsReq,
	_ ...grpc.CallOption) (*accountrpc.LoginLogsReply, error) {
	f.calls++
	f.loginLogsReq = in
	return f.loginLogsReply, f.err
}

// assertPasswordFreeEnvelope 校验成功信封的结构，并确认序列化后的响应体不含任何口令材料。
func assertPasswordFreeEnvelope(t *testing.T, resp *types.EmptyResponse, passwords ...string) {
	t.Helper()
	if resp == nil {
		t.Fatal("resp = nil, want 成功信封")
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d, want 0/ok/0", resp.Code, resp.Message, resp.TTL)
	}
	if resp.Data != (types.EmptyData{}) {
		t.Fatalf("EmptyResponse.data 必须是空对象, got %+v", resp.Data)
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, p := range passwords {
		if strings.Contains(string(raw), p) {
			t.Fatalf("响应体回显了口令材料 %q: %s", p, raw)
		}
	}
	if !strings.Contains(string(raw), `"data":{}`) {
		t.Fatalf("无数据时 data 应为 {}，got %s", raw)
	}
}

func TestPassportSetPasswordForwardsCiphertext(t *testing.T) {
	fake := &fakeAccountClient{}
	l := NewSetPasswordLogic(context.Background(), &svc.ServiceContext{Account: fake})
	resp, err := l.SetPassword(&types.ParamSetPassword{
		Mid: 7, OldPassword: passportOldPwdCipher, NewPassword: passportNewPwdCipher, IP: "10.0.0.7",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 密文原样透传：网关不做解密、不做强度校验（AGENTS.md §6 安全边界在 account 服务）。
	if fake.setPasswordReq.GetMid() != 7 || fake.setPasswordReq.GetIp() != "10.0.0.7" {
		t.Fatalf("mid/ip 未透传: %+v", fake.setPasswordReq)
	}
	if fake.setPasswordReq.GetOldPassword() != passportOldPwdCipher ||
		fake.setPasswordReq.GetNewPassword() != passportNewPwdCipher {
		t.Fatalf("RSA 密文被网关改写: %+v", fake.setPasswordReq)
	}
	assertPasswordFreeEnvelope(t, resp, passportOldPwdCipher, passportNewPwdCipher)

	// 首次设置密码：旧密码留空是合法语义，网关不得替客户端补值。
	fake2 := &fakeAccountClient{}
	if _, err = NewSetPasswordLogic(context.Background(), &svc.ServiceContext{Account: fake2}).
		SetPassword(&types.ParamSetPassword{Mid: 7, NewPassword: passportNewPwdCipher}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake2.setPasswordReq.GetOldPassword() != "" {
		t.Fatalf("首次设置不应补旧密码: %+v", fake2.setPasswordReq)
	}
}

func TestPassportSetPasswordErrors(t *testing.T) {
	fake := &fakeAccountClient{}
	l := NewSetPasswordLogic(context.Background(), &svc.ServiceContext{Account: fake})

	sentinel := errors.New("account: old password mismatch")
	fake.err = sentinel
	if _, err := l.SetPassword(&types.ParamSetPassword{Mid: 7, NewPassword: passportNewPwdCipher}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want 下游错误原样上抛（口令材料不得改写错误）", err)
	}

	if _, err := NewSetPasswordLogic(context.Background(), &svc.ServiceContext{}).
		SetPassword(&types.ParamSetPassword{Mid: 7}); err == nil {
		t.Fatal("未配置 account 客户端时必须报错")
	}
}

func TestPassportResetPasswordForwardsCaptureAndCiphertext(t *testing.T) {
	fake := &fakeAccountClient{}
	resp, err := NewResetPasswordLogic(context.Background(), &svc.ServiceContext{Account: fake}).
		ResetPassword(&types.ParamResetPassword{
			Account: "13800000000", CaptureCode: "123456", NewPassword: passportNewPwdCipher, IP: "10.0.0.9",
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.resetPasswordReq.GetAccount() != "13800000000" ||
		fake.resetPasswordReq.GetCaptureCode() != "123456" ||
		fake.resetPasswordReq.GetNewPassword() != passportNewPwdCipher ||
		fake.resetPasswordReq.GetIp() != "10.0.0.9" {
		t.Fatalf("重置入参未透传: %+v", fake.resetPasswordReq)
	}
	assertPasswordFreeEnvelope(t, resp, passportNewPwdCipher, "123456")

	sentinel := errors.New("account: capture code expired")
	fake.err = sentinel
	if _, err = NewResetPasswordLogic(context.Background(), &svc.ServiceContext{Account: fake}).
		ResetPassword(&types.ParamResetPassword{Account: "13800000000"}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want 下游错误原样上抛", err)
	}

	if _, err = NewResetPasswordLogic(context.Background(), &svc.ServiceContext{}).
		ResetPassword(&types.ParamResetPassword{}); err == nil {
		t.Fatal("未配置 account 客户端时必须报错")
	}
}

func TestPassportCheckHistoryPasswordReturnsHitSequence(t *testing.T) {
	fake := &fakeAccountClient{checkHistoryReply: &accountrpc.CheckHistoryPwdReply{Result: "0,1"}}
	l := NewCheckHistoryPasswordLogic(context.Background(), &svc.ServiceContext{Account: fake})
	resp, err := l.CheckHistoryPassword(&types.ParamCheckHistoryPassword{Mid: 7, Password: passportHistoryCipher})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.checkHistoryReq.GetMid() != 7 || fake.checkHistoryReq.GetPassword() != passportHistoryCipher {
		t.Fatalf("历史校验入参未透传: %+v", fake.checkHistoryReq)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}
	// result 与入参一一对应，客户端按位置判定，网关不解析也不改写序列。
	if resp.Data.Result != "0,1" {
		t.Fatalf("result = %q, want 0,1", resp.Data.Result)
	}
	raw, marshalErr := json.Marshal(resp)
	if marshalErr != nil {
		t.Fatalf("marshal: %v", marshalErr)
	}
	if strings.Contains(string(raw), passportHistoryCipher) {
		t.Fatalf("响应体回显了口令密文: %s", raw)
	}

	sentinel := errors.New("account: history unavailable")
	fake.err = sentinel
	if _, err = l.CheckHistoryPassword(&types.ParamCheckHistoryPassword{Mid: 7}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want 下游错误原样上抛", err)
	}

	if _, err = NewCheckHistoryPasswordLogic(context.Background(), &svc.ServiceContext{}).
		CheckHistoryPassword(&types.ParamCheckHistoryPassword{Mid: 7}); err == nil {
		t.Fatal("未配置 account 客户端时必须报错")
	}
}

func TestPassportLoginLogsMapsEveryField(t *testing.T) {
	fake := &fakeAccountClient{loginLogsReply: &accountrpc.LoginLogsReply{
		Logs: []*accountrpc.LoginLog{
			{
				Mid: 7, Ip: "10.0.0.7", Ts: 1700000000, LoginType: 1, Status: 0,
				Reason: "", Device: "android/3.2.1",
			},
			{
				Mid: 7, Ip: "10.0.0.8", Ts: 1700000100, LoginType: 2, Status: 1,
				Reason: "bad_password", Device: "ios/3.2.1",
			},
			nil, // proto 列表里的未填元素也必须安全投影
		},
	}}
	l := NewLoginLogsLogic(context.Background(), &svc.ServiceContext{Account: fake})
	resp, err := l.LoginLogs(&types.ParamLoginLogs{Mid: 7, Limit: 50})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.loginLogsReq.GetMid() != 7 || fake.loginLogsReq.GetLimit() != 50 {
		t.Fatalf("登录记录入参未透传: %+v", fake.loginLogsReq)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}
	if len(resp.Data.Logs) != 3 {
		t.Fatalf("logs = %d, want 3", len(resp.Data.Logs))
	}
	first := resp.Data.Logs[0]
	if first.Mid != 7 || first.IP != "10.0.0.7" || first.Ts != 1700000000 ||
		first.LoginType != 1 || first.Status != 0 || first.Reason != "" || first.Device != "android/3.2.1" {
		t.Fatalf("首条日志字段投影不完整: %+v", first)
	}
	second := resp.Data.Logs[1]
	if second.IP != "10.0.0.8" || second.Ts != 1700000100 || second.LoginType != 2 ||
		second.Status != 1 || second.Reason != "bad_password" || second.Device != "ios/3.2.1" {
		t.Fatalf("失败原因与设备必须透出（用户自查异常登录的依据）: %+v", second)
	}
	if resp.Data.Logs[2] != (types.PassportLoginLog{}) {
		t.Fatalf("nil 元素应投影成零值, got %+v", resp.Data.Logs[2])
	}
}

func TestPassportLoginLogsEmptyListAndGuards(t *testing.T) {
	fake := &fakeAccountClient{loginLogsReply: &accountrpc.LoginLogsReply{}}
	resp, err := NewLoginLogsLogic(context.Background(), &svc.ServiceContext{Account: fake}).
		LoginLogs(&types.ParamLoginLogs{Mid: 7})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Data.Logs == nil || len(resp.Data.Logs) != 0 {
		t.Fatalf("logs = %v, want 空数组（客户端应拿到 [] 而非 null）", resp.Data.Logs)
	}

	sentinel := errors.New("account: too many requests")
	fake.err = sentinel
	if _, err = NewLoginLogsLogic(context.Background(), &svc.ServiceContext{Account: fake}).
		LoginLogs(&types.ParamLoginLogs{Mid: 7}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want 下游错误原样上抛", err)
	}

	if _, err = NewLoginLogsLogic(context.Background(), &svc.ServiceContext{}).
		LoginLogs(&types.ParamLoginLogs{Mid: 7}); err == nil {
		t.Fatal("未配置 account 客户端时必须报错")
	}
}

func TestPassportLoginLogsToAPINilList(t *testing.T) {
	got := passportLoginLogsToAPI(nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("passportLoginLogsToAPI(nil) = %v, want 非 nil 空切片", got)
	}
}
