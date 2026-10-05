package logic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	catalogrpc "go-video/services/catalog/rpc"

	"google.golang.org/grpc"
)

// 本文件钉住 catalog 运营写入口的两类主体口径（见 adminsubject.go 文件头）：
//   - CreateWork 的操作人是自由文本，网关只要求「非空 + 有会话」，绝不用 admin_id 改写它；
//   - Publish/Offline Episode 的契约里没有任何操作者位，只能靠会话门槛保证事后可追。
//
// 打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库（AGENTS.md §9）。
// 领域校验（标题/类型/状态机/版权窗口）留在 catalog 服务侧，本文件不重复验证。

type catalogAdminFake struct {
	catalogrpc.CatalogClient

	createWorkReq     *catalogrpc.CreateWorkReq
	createWorkReply   *catalogrpc.WorkReply
	episodeReply      *catalogrpc.EpisodeReply
	publishEpisodeReq *catalogrpc.EpisodeReq
	err               error
	calls             int
}

func (f *catalogAdminFake) CreateWork(_ context.Context, in *catalogrpc.CreateWorkReq,
	_ ...grpc.CallOption) (*catalogrpc.WorkReply, error) {
	f.calls++
	f.createWorkReq = in
	return f.createWorkReply, f.err
}

func (f *catalogAdminFake) PublishEpisode(_ context.Context, in *catalogrpc.EpisodeReq,
	_ ...grpc.CallOption) (*catalogrpc.EpisodeReply, error) {
	f.calls++
	f.publishEpisodeReq = in
	return f.episodeReply, f.err
}

func TestCatalogCreateWorkActorGate(t *testing.T) {
	sentinel := errors.New("catalog: title duplicated")

	// 无会话：即使 operator 合法也必须拒绝，且一次下游调用都不能发出。
	noSession := &catalogAdminFake{}
	if _, err := NewCreateCatalogWorkLogic(context.Background(), &svc.ServiceContext{Catalog: noSession}).
		CreateCatalogWork(&types.ParamCreateWork{Title: "长夜行", Typeid: 1, Operator: "ops-a"}); err == nil ||
		!strings.Contains(err.Error(), "admin session required") {
		t.Fatalf("无会话却放行: %v", err)
	}
	if noSession.calls != 0 {
		t.Fatalf("无会话被拒后仍调用下游 %d 次", noSession.calls)
	}

	// 空操作人（含只有空白）在会话之前就要挡下：台账不能记成无主处置。
	for _, actor := range []string{"", "   "} {
		fake := &catalogAdminFake{}
		if _, err := NewCreateCatalogWorkLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Catalog: fake}).
			CreateCatalogWork(&types.ParamCreateWork{Title: "长夜行", Typeid: 1, Operator: actor}); err == nil ||
			!strings.Contains(err.Error(), "operator") {
			t.Fatalf("空 operator=%q 应点名该字段, got %v", actor, err)
		}
		if fake.calls != 0 {
			t.Fatalf("operator 校验失败不得调用下游，实际调用 %d 次", fake.calls)
		}
	}

	// 有会话 + 有操作人：operator 原样下发，不得被会话里的 admin_id 替换。
	fake := &catalogAdminFake{createWorkReply: &catalogrpc.WorkReply{SeasonId: 301, Title: "长夜行", Typeid: 1}}
	resp, err := NewCreateCatalogWorkLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Catalog: fake}).
		CreateCatalogWork(&types.ParamCreateWork{Title: "长夜行", Cover: "c", Typeid: 1, Intro: "i", Operator: "ops-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.createWorkReq.GetOperator() != "ops-a" {
		t.Fatalf("operator = %q, want ops-a（admin_id 与操作人名不是同一空间，不得覆盖）",
			fake.createWorkReq.GetOperator())
	}
	if fake.createWorkReq.GetTitle() != "长夜行" || fake.createWorkReq.GetTypeid() != 1 {
		t.Fatalf("创建入参未原样透传: %+v", fake.createWorkReq)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 || resp.Data.Work.SeasonId != 301 {
		t.Fatalf("信封/data = code=%d message=%q ttl=%d data=%+v", resp.Code, resp.Message, resp.TTL, resp.Data)
	}

	// 下游错误原样上抛，由 httpresponse 渲染信封。
	fakeErr := &catalogAdminFake{err: sentinel}
	if _, err := NewCreateCatalogWorkLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Catalog: fakeErr}).
		CreateCatalogWork(&types.ParamCreateWork{Title: "长夜行", Typeid: 1, Operator: "ops-a"}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want 下游错误原样上抛", err)
	}

	// 未配置下游必须显式失败，而不是拿空 data 装作成功。
	if _, err := NewCreateCatalogWorkLogic(withAdminSession(gateAdminID), &svc.ServiceContext{}).
		CreateCatalogWork(&types.ParamCreateWork{Title: "长夜行", Typeid: 1, Operator: "ops-a"}); err == nil ||
		!strings.Contains(err.Error(), "catalog service not configured") {
		t.Fatalf("未配置 catalog 时应失败, got %v", err)
	}
}

func TestCatalogPublishEpisodeSessionGate(t *testing.T) {
	// 契约里没有操作者位（缺口见 logic 文件头），所以这里只能验「无会话一律拒绝」。
	noSession := &catalogAdminFake{}
	if _, err := NewPublishCatalogEpisodeLogic(context.Background(), &svc.ServiceContext{Catalog: noSession}).
		PublishCatalogEpisode(&types.ParamCatalogEpid{Epid: 77}); err == nil ||
		!strings.Contains(err.Error(), "admin session required") {
		t.Fatalf("无会话却放行: %v", err)
	}
	if noSession.calls != 0 {
		t.Fatalf("无会话被拒后仍调用下游 %d 次", noSession.calls)
	}

	fake := &catalogAdminFake{}
	if _, err := NewPublishCatalogEpisodeLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Catalog: fake}).
		PublishCatalogEpisode(&types.ParamCatalogEpid{Epid: 77}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.publishEpisodeReq.GetEpid() != 77 || fake.calls != 1 {
		t.Fatalf("发布入参 = %+v calls=%d", fake.publishEpisodeReq, fake.calls)
	}
}
