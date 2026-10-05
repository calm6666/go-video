package logic

import (
	"context"
	"errors"
	"testing"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	searchindexerrpc "go-video/services/search-indexer/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧的投影与入参归一口径：内嵌生成的 client 接口打桩，
// 不建 gRPC 连接、不碰数据库，保证测试可在无网络环境下秒级跑完。

type fakeSearchIndexer struct {
	searchindexerrpc.SearchIndexerClient
	tasks     *searchindexerrpc.ListRebuildTasksReply
	err       error
	req       *searchindexerrpc.ListRebuildTasksReq
	callCount int
}

func (f *fakeSearchIndexer) ListRebuildTasks(_ context.Context, in *searchindexerrpc.ListRebuildTasksReq,
	_ ...grpc.CallOption) (*searchindexerrpc.ListRebuildTasksReply, error) {
	f.req = in
	f.callCount++
	return f.tasks, f.err
}

func TestNormalizeSearchLimit(t *testing.T) {
	cases := []struct {
		name string
		in   int32
		want int32
	}{
		{"缺省回落 20", 0, searchDefaultRebuildLimit},
		{"负值回落 20", -7, searchDefaultRebuildLimit},
		{"超过上限不截断而是回落", searchMaxRebuildLimit + 1, searchDefaultRebuildLimit},
		{"远大于上限", 5000, searchDefaultRebuildLimit},
		{"上限本身保留", searchMaxRebuildLimit, searchMaxRebuildLimit},
		{"区间内保留", 7, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeSearchLimit(tc.in); got != tc.want {
				t.Fatalf("normalizeSearchLimit(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestRebuildTaskToAPINilIsZeroItem(t *testing.T) {
	got := rebuildTaskToAPI(nil)
	if got != (types.SearchRebuildTaskItem{}) {
		t.Fatalf("rebuildTaskToAPI(nil) = %+v, want zero item", got)
	}
}

func TestRebuildTaskToAPIKeepsServerStrings(t *testing.T) {
	in := &searchindexerrpc.RebuildTask{
		TaskId:      "task-1",
		Scope:       "partition",
		ScopeValue:  "video",
		State:       "running",
		CursorValue: "1000",
		Total:       10,
		Processed:   4,
		Failed:      1,
		TargetIndex: "content_v2_20260901",
		Alias:       "content_active",
		Operator:    "ops-a",
		RequestId:   "req-1",
		LastError:   "opensearch timeout",
		DlqCount:    3,
	}
	got := rebuildTaskToAPI(in)
	if got.TaskId != in.TaskId || got.State != in.State || got.Scope != in.Scope {
		t.Fatalf("identity/state lost: %+v", got)
	}
	if got.TargetIndex != in.TargetIndex || got.Alias != in.Alias || got.RequestId != in.RequestId {
		t.Fatalf("index/alias/request mapping wrong: %+v", got)
	}
	if got.DlqCount != 3 || got.LastError != in.LastError {
		t.Fatalf("dlq_count/last_error mapping wrong: %+v", got)
	}
}

func TestRebuildTasksToAPIAndAliasStatusesNilInput(t *testing.T) {
	tasks := rebuildTasksToAPI(nil)
	if tasks == nil {
		t.Fatal("rebuildTasksToAPI(nil) = nil, want empty slice (后台应拿到 [] 而非 null)")
	}
	if len(tasks) != 0 {
		t.Fatalf("rebuildTasksToAPI(nil) len = %d, want 0", len(tasks))
	}
	aliases := aliasStatusesToAPI(nil)
	if aliases == nil {
		t.Fatal("aliasStatusesToAPI(nil) = nil, want empty slice")
	}
	if len(aliases) != 0 {
		t.Fatalf("aliasStatusesToAPI(nil) len = %d, want 0", len(aliases))
	}
	// 列表里的 nil 元素（proto 未填）也必须投影成零值，不能 panic。
	if got := rebuildTasksToAPI([]*searchindexerrpc.RebuildTask{nil}); len(got) != 1 || got[0].TaskId != "" {
		t.Fatalf("rebuildTasksToAPI with nil element = %+v", got)
	}
}

func TestAliasStatusesToAPIDocCountMinusOnePassThrough(t *testing.T) {
	got := aliasStatusesToAPI([]*searchindexerrpc.AliasStatus{
		{Alias: "content_active", ActiveIndex: "content_v2", DocCount: -1, Health: "missing", State: "active"},
		{Alias: "content_gray", ActiveIndex: "content_v3", DocCount: 0, Health: "green", State: "history"},
	})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].DocCount != -1 {
		t.Fatalf("doc_count = %d, want -1（服务端读不到时网关不得改写成 0）", got[0].DocCount)
	}
	if got[0].Health != "missing" || got[0].State != "active" {
		t.Fatalf("health/state not passed through: %+v", got[0])
	}
	if got[1].DocCount != 0 {
		t.Fatalf("doc_count = %d, want 0", got[1].DocCount)
	}
}

func TestListRebuildTasksLogicRequiresOperator(t *testing.T) {
	fake := &fakeSearchIndexer{}
	l := NewListRebuildTasksLogic(context.Background(), &svc.ServiceContext{SearchIndexer: fake})
	if _, err := l.ListRebuildTasks(&types.ParamListRebuildTasks{OperatorId: 0}); err == nil {
		t.Fatal("operator_id=0 应被拒绝：审计必须有主体")
	}
	if fake.callCount != 0 {
		t.Fatalf("校验失败时不得调用下游，实际调用 %d 次", fake.callCount)
	}
}

func TestListRebuildTasksLogicWithoutClientConfigured(t *testing.T) {
	l := NewListRebuildTasksLogic(context.Background(), &svc.ServiceContext{})
	if _, err := l.ListRebuildTasks(&types.ParamListRebuildTasks{OperatorId: 9}); err == nil {
		t.Fatal("未配置 search-indexer 时应返回错误")
	}
}

func TestListRebuildTasksLogicNormalizesLimitAndEnvelope(t *testing.T) {
	fake := &fakeSearchIndexer{tasks: &searchindexerrpc.ListRebuildTasksReply{
		Tasks:      []*searchindexerrpc.RebuildTask{{TaskId: "task-1", DlqCount: 2}},
		NextCursor: "cursor-2",
	}}
	l := NewListRebuildTasksLogic(context.Background(), &svc.ServiceContext{SearchIndexer: fake})
	resp, err := l.ListRebuildTasks(&types.ParamListRebuildTasks{
		State:      "running",
		Cursor:     "cursor-1",
		Limit:      500,
		OperatorId: 9,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.req.GetLimit() != searchDefaultRebuildLimit {
		t.Fatalf("limit = %d, want %d（网关不得替运营放大页大小）", fake.req.GetLimit(), searchDefaultRebuildLimit)
	}
	if fake.req.GetState() != "running" || fake.req.GetCursor() != "cursor-1" {
		t.Fatalf("filters not forwarded: %+v", fake.req)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("envelope = code=%d message=%s ttl=%d", resp.Code, resp.Message, resp.TTL)
	}
	if resp.Data.NextCursor != "cursor-2" || len(resp.Data.Tasks) != 1 || resp.Data.Tasks[0].DlqCount != 2 {
		t.Fatalf("data = %+v", resp.Data)
	}
}

func TestListRebuildTasksLogicPropagatesDownstreamError(t *testing.T) {
	sentinel := errors.New("search-indexer: unavailable")
	fake := &fakeSearchIndexer{err: sentinel}
	l := NewListRebuildTasksLogic(context.Background(), &svc.ServiceContext{SearchIndexer: fake})
	if _, err := l.ListRebuildTasks(&types.ParamListRebuildTasks{OperatorId: 9}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want downstream error propagated unchanged", err)
	}
}
