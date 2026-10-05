package logic

import (
	"context"
	"errors"
	"testing"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	moderationrpc "go-video/services/moderation-orchestrator/rpc"

	"google.golang.org/grpc"
)

// 本文件只覆盖网关面向 moderation-orchestrator 的申诉路由：
// 申诉 eligibility 与结论状态判定属于 moderation，网关只转发 mid/ip 并投影回复。
// 打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库（AGENTS.md §9）。

type fakeModerationClient struct {
	moderationrpc.ModerationOrchestratorClient

	appealReq   *moderationrpc.AppealReq
	appealReply *moderationrpc.AppealReply
	calls       int
	err         error
}

func (f *fakeModerationClient) SubmitAppeal(_ context.Context, in *moderationrpc.AppealReq,
	_ ...grpc.CallOption) (*moderationrpc.AppealReply, error) {
	f.calls++
	f.appealReq = in
	return f.appealReply, f.err
}

func TestModerationSubmitAppealProjectsVerdict(t *testing.T) {
	fake := &fakeModerationClient{appealReply: &moderationrpc.AppealReply{
		Appeal: &moderationrpc.Appeal{
			AppealId: 55, TaskId: 88, Mid: 7, Content: "误判了",
			FinalVerdict: moderationrpc.Verdict_VERDICT_REJECT, FinalReason: "版权方已确认",
			Handler: 9, Ctime: 111, Mtime: 222,
		},
	}}
	l := NewSubmitAppealLogic(context.Background(), &svc.ServiceContext{Moderation: fake})
	resp, err := l.SubmitAppeal(&types.ParamSubmitAppeal{
		TaskId: 88, Mid: 7, Content: "误判了", IP: "10.0.0.7",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.appealReq.GetTaskId() != 88 || fake.appealReq.GetMid() != 7 ||
		fake.appealReq.GetContent() != "误判了" || fake.appealReq.GetIp() != "10.0.0.7" {
		t.Fatalf("申诉入参未透传: %+v", fake.appealReq)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}
	got := resp.Data.Appeal
	if got.AppealId != 55 || got.TaskId != 88 || got.Mid != 7 || got.Content != "误判了" ||
		got.FinalReason != "版权方已确认" || got.Handler != 9 || got.Ctime != 111 || got.Mtime != 222 {
		t.Fatalf("appeal = %+v", got)
	}
	// Verdict 是 protobuf 枚举，投影后必须是信封里的 int32，客户端按数值渲染。
	if got.FinalVerdict != int32(moderationrpc.Verdict_VERDICT_REJECT) {
		t.Fatalf("final_verdict = %d, want %d", got.FinalVerdict, moderationrpc.Verdict_VERDICT_REJECT)
	}
}

func TestModerationSubmitAppealUnprocessedAppealKeepsZeroVerdict(t *testing.T) {
	// 刚提交的申诉尚未处理：服务端回零值结论，网关不得替它编造 PASS/REJECT。
	fake := &fakeModerationClient{appealReply: &moderationrpc.AppealReply{
		Appeal: &moderationrpc.Appeal{AppealId: 56, TaskId: 88, Mid: 7, Content: "再看一次", Ctime: 111},
	}}
	resp, err := NewSubmitAppealLogic(context.Background(), &svc.ServiceContext{Moderation: fake}).
		SubmitAppeal(&types.ParamSubmitAppeal{TaskId: 88, Mid: 7, Content: "再看一次"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Data.Appeal.FinalVerdict != 0 || resp.Data.Appeal.Handler != 0 || resp.Data.Appeal.Mtime != 0 {
		t.Fatalf("未处理申诉应保留零值结论, got %+v", resp.Data.Appeal)
	}
	if resp.Data.Appeal.AppealId != 56 {
		t.Fatalf("appeal_id = %d, want 56", resp.Data.Appeal.AppealId)
	}
}

func TestModerationAppealToAPINil(t *testing.T) {
	if got := appealToAPI(nil); got != (types.ModerationAppealInfo{}) {
		t.Fatalf("appealToAPI(nil) = %+v, want 零值", got)
	}
}

func TestModerationSubmitAppealGuards(t *testing.T) {
	// 未配置 ModerationRPC 时必须报错，不能退化成「申诉已受理」的成功信封。
	if _, err := NewSubmitAppealLogic(context.Background(), &svc.ServiceContext{}).
		SubmitAppeal(&types.ParamSubmitAppeal{TaskId: 88, Mid: 7}); err == nil {
		t.Fatal("未配置 moderation 客户端时必须报错")
	}

	// reply.appeal 缺失（proto 允许）时走零值分支，不得 panic。
	fake := &fakeModerationClient{appealReply: &moderationrpc.AppealReply{}}
	resp, err := NewSubmitAppealLogic(context.Background(), &svc.ServiceContext{Moderation: fake}).
		SubmitAppeal(&types.ParamSubmitAppeal{TaskId: 88, Mid: 7})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Data.Appeal != (types.ModerationAppealInfo{}) {
		t.Fatalf("空 reply 应投影成零值, got %+v", resp.Data.Appeal)
	}

	sentinel := errors.New("moderation: only author can appeal")
	fake.err = sentinel
	if _, err = NewSubmitAppealLogic(context.Background(), &svc.ServiceContext{Moderation: fake}).
		SubmitAppeal(&types.ParamSubmitAppeal{TaskId: 88, Mid: 8}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want 下游错误原样上抛（作者资格由 moderation 判定）", err)
	}
}
