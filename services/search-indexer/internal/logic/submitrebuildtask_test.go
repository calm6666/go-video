// submitrebuildtask_test.go 覆盖 SubmitRebuildTask：提交阶段只写 MySQL、
// request_id 幂等、先校验再落库、目标索引名预分配但绝不建索引。
package logic

import (
	"context"
	"regexp"
	"strconv"
	"testing"

	"go-video/services/search-indexer/internal/repository"
	"go-video/services/search-indexer/model"
	"go-video/services/search-indexer/rpc"
)

func submitReq(scope, scopeValue, alias, requestID string) *rpc.SubmitRebuildTaskReq {
	return &rpc.SubmitRebuildTaskReq{Scope: scope, ScopeValue: scopeValue, Alias: alias, RequestId: requestID, Operator: "admin-1"}
}

// 正常提交：pending + 游标起点 1 + 预分配目标索引，全程不碰 OpenSearch。
func TestSubmitRebuildTask_RegistersTaskWithoutTouchingOpenSearch(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	reply, err := NewSubmitRebuildTaskLogic(context.Background(), s.svcCtx).SubmitRebuildTask(
		submitReq(model.ScopeFull, "忽略", "", "req-1"))
	wantNoErr(t, err)
	if !regexp.MustCompile(`^sit_[0-9A-Z]{26}$`).MatchString(reply.TaskId) {
		t.Fatalf("task_id 必须是 sit_<ULID>: %q", reply.TaskId)
	}
	wantEQ(t, reply.State, model.TaskStatePending, "reply.State")
	wantEQ(t, reply.Duplicated, false, "reply.Duplicated")
	// 索引名规则：<alias>_<schema>_<unix秒>，全小写，可反复重建不撞名。
	if !regexp.MustCompile(`^` + testPrefix + `_v1_\d{10}$`).MatchString(reply.TargetIndex) {
		t.Fatalf("target_index = %q", reply.TargetIndex)
	}
	wantOps(t, s.ops(), "task.FindByRequestID:req-1", "task.Insert:req-1")
	wantNoCall(t, s.ops(), "es.")      // OpenSearch 抖动时也要能可靠登记任务
	wantNoCall(t, s.ops(), "version.") // 不预建索引版本

	row := s.taskMd.byRequestID("req-1")
	if row == nil {
		t.Fatal("任务未落库")
	}
	wantEQ(t, row.TaskID, reply.TaskId, "落库 task_id 与响应一致")
	wantEQ(t, row.State, model.TaskStatePending, "落库 state")
	wantEQ(t, row.CursorValue, "1", "续跑游标起点（content_id 从 1 发号）")
	wantEQ(t, row.Scope, model.ScopeFull, "落库 scope")
	wantEQ(t, row.ScopeValue, "", "full 忽略取值")
	wantEQ(t, row.Alias, testPrefix, "空别名落到默认别名")
	wantEQ(t, row.TargetIndex, reply.TargetIndex, "落库 target_index")
	wantEQ(t, row.Operator, "admin-1", "operator 留痕")
	wantEQ(t, row.Total, int64(0), "total 由 runner 首片估算")
	wantEQ(t, row.Processed, int64(0), "processed")
	wantEQ(t, row.Failed, int64(0), "failed")
	wantEQ(t, row.LastError, "", "last_error")
	wantEQ(t, row.StartedAt, int64(0), "未开跑不得写 started_at")
	wantEQ(t, row.FinishedAt, int64(0), "未结束不得写 finished_at")
	isRecentUnix(t, row.Ctime, "ctime")
	wantEQ(t, row.Mtime, row.Ctime, "新任务 mtime 与 ctime 同源")
}

// 幂等：同一 request_id 重放返回既有任务，绝不新建任务、也绝不重新分配索引。
func TestSubmitRebuildTask_ReplayReturnsExistingTask(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.taskMd.seed(&model.SearchIndexTask{
		TaskID: "sit_EXISTING00000000000000", Scope: model.ScopeFull, State: model.TaskStateRunning,
		CursorValue: "100001", TargetIndex: testPrefix + "_v1_555", Alias: testPrefix,
		RequestID: "req-1", Operator: "admin-1", Ctime: 100, Mtime: 200,
	})
	reply, err := NewSubmitRebuildTaskLogic(context.Background(), s.svcCtx).SubmitRebuildTask(
		submitReq(model.ScopeFull, "", "", "req-1"))
	wantNoErr(t, err)
	wantEQ(t, reply.Duplicated, true, "reply.Duplicated")
	wantEQ(t, reply.TaskId, "sit_EXISTING00000000000000", "必须返回既有 task_id")
	wantEQ(t, reply.TargetIndex, testPrefix+"_v1_555", "不得重新分配索引")
	wantEQ(t, reply.State, model.TaskStateRunning, "既有状态原样上报（不回写成 pending）")
	wantOps(t, s.ops(), "task.FindByRequestID:req-1")
	wantNoCall(t, s.ops(), "task.Insert")
}

// 并发提交撞 uniq_request_id：用对手创建的任务应答。
func TestSubmitRebuildTask_ConcurrentInsertAdoptsOtherTask(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.taskMd.collisionRow = &model.SearchIndexTask{
		TaskID: "sit_OTHER0000000000000000", Scope: model.ScopeFull, State: model.TaskStatePending,
		CursorValue: "1", TargetIndex: testPrefix + "_v1_666", Alias: testPrefix, RequestID: "req-1",
	}
	reply, err := NewSubmitRebuildTaskLogic(context.Background(), s.svcCtx).SubmitRebuildTask(
		submitReq(model.ScopeFull, "", "", "req-1"))
	wantNoErr(t, err)
	wantEQ(t, reply.Duplicated, true, "reply.Duplicated")
	wantEQ(t, reply.TaskId, "sit_OTHER0000000000000000", "reply.TaskId")
	wantEQ(t, reply.TargetIndex, testPrefix+"_v1_666", "reply.TargetIndex")
	wantOps(t, s.ops(),
		"task.FindByRequestID:req-1",
		"task.Insert:req-1",
		"task.FindByRequestID:req-1",
	)
}

// 唯一索引报了冲突却又查不到任务：必须报错，不能返回一个「看似成功」的空响应。
func TestSubmitRebuildTask_CollisionButInvisibleTaskFails(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.taskMd.collisionRow = &model.SearchIndexTask{
		TaskID: "sit_FOREIGN00000000000000", RequestID: "another-req", State: model.TaskStatePending,
	}
	reply, err := NewSubmitRebuildTaskLogic(context.Background(), s.svcCtx).SubmitRebuildTask(
		submitReq(model.ScopeFull, "", "", "req-1"))
	wantErr(t, err, "冲突但任务不可见")
	wantContains(t, err.Error(), "冲突但任务不可见", "错误原因")
	if reply != nil {
		t.Fatalf("失败时响应必须为 nil, got %+v", reply)
	}
	wantCount(t, s.ops(), "task.FindByRequestID", 2)
}

// request_id 是幂等键，缺失必须快速失败；而且它的校验早于 scope。
func TestSubmitRebuildTask_RequiresRequestID(t *testing.T) {
	for _, rid := range []string{"", "   ", "\t"} {
		s := newTestStore(t, defaultOptions())
		reply, err := NewSubmitRebuildTaskLogic(context.Background(), s.svcCtx).SubmitRebuildTask(
			submitReq(model.ScopeFull, "", "", rid))
		wantErrIs(t, err, model.ErrInvalidRequestID, "request_id="+strconv.Quote(rid))
		if reply != nil {
			t.Fatalf("失败时响应必须为 nil, got %+v", reply)
		}
		wantOps(t, s.ops())
	}
	// 顺序：request_id 先于 scope 校验（两个都坏时报前者）。
	s := newTestStore(t, defaultOptions())
	_, err := NewSubmitRebuildTaskLogic(context.Background(), s.svcCtx).SubmitRebuildTask(
		submitReq("author", "1", "", ""))
	wantErrIs(t, err, model.ErrInvalidRequestID, "校验优先级")
}

// 先校验再落库：非法 scope 一个依赖调用都不发，也不分配索引名。
func TestSubmitRebuildTask_InvalidScopeRejectsBeforeAnyIO(t *testing.T) {
	cases := []struct{ name, scope, scopeValue string }{
		{"未知 scope", "author", "1"},
		{"空 scope", "", ""},
		{"分区上下界颠倒", model.ScopePartition, "2000-1000"},
		{"分区缺上界", model.ScopePartition, "1000"},
		{"分区非数字", model.ScopePartition, "a-b"},
		{"内容类型 0", model.ScopeContentType, "0"},
		{"内容类型 4", model.ScopeContentType, "4"},
		{"内容类型非数字", model.ScopeContentType, "ugc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestStore(t, defaultOptions())
			reply, err := NewSubmitRebuildTaskLogic(context.Background(), s.svcCtx).SubmitRebuildTask(
				submitReq(c.scope, c.scopeValue, "", "req-1"))
			wantErrIs(t, err, model.ErrInvalidScope, "非法 scope")
			if reply != nil {
				t.Fatalf("失败时响应必须为 nil, got %+v", reply)
			}
			wantOps(t, s.ops())
		})
	}
}

// 取值归一化：scope_value 落库的是规范化结果，alias 归一化决定 target_index 前缀。
func TestSubmitRebuildTask_NormalizesScopeValueAndAlias(t *testing.T) {
	cases := []struct {
		name          string
		scope         string
		scopeValue    string
		alias         string
		wantScopeVal  string
		wantAlias     string
		wantIndexPrex string
	}{
		{name: "按内容类型去空白", scope: model.ScopeContentType, scopeValue: " 2 ", wantScopeVal: "2", wantAlias: testPrefix, wantIndexPrex: testPrefix + "_v1_"},
		{name: "分区区间重排", scope: model.ScopePartition, scopeValue: " 1000 - 1999 ", wantScopeVal: "1000-1999", wantAlias: testPrefix, wantIndexPrex: testPrefix + "_v1_"},
		{name: "非默认别名归一化小写", scope: model.ScopeFull, alias: "  Live_Zone ", wantScopeVal: "", wantAlias: "live_zone", wantIndexPrex: "live_zone_v1_"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestStore(t, defaultOptions())
			reply, err := NewSubmitRebuildTaskLogic(context.Background(), s.svcCtx).SubmitRebuildTask(
				submitReq(c.scope, c.scopeValue, c.alias, "req-x"))
			wantNoErr(t, err)
			row := s.taskMd.byRequestID("req-x")
			if row == nil {
				t.Fatal("任务未落库")
			}
			wantEQ(t, row.ScopeValue, c.wantScopeVal, "规范化后的 scope_value")
			wantEQ(t, row.Alias, c.wantAlias, "规范化后的 alias")
			wantEQ(t, reply.TargetIndex, row.TargetIndex, "响应与落库一致")
			if len(reply.TargetIndex) < len(c.wantIndexPrex) || reply.TargetIndex[:len(c.wantIndexPrex)] != c.wantIndexPrex {
				t.Fatalf("target_index 必须以 %q 开头: %q", c.wantIndexPrex, reply.TargetIndex)
			}
		})
	}
}

// 依赖失败传播：不吞错、不假装任务已登记。
func TestSubmitRebuildTask_DependencyFailuresPropagate(t *testing.T) {
	t.Run("幂等查询失败", func(t *testing.T) {
		s := newTestStore(t, defaultOptions())
		s.fail("task.FindByRequestID", errOther)
		reply, err := NewSubmitRebuildTaskLogic(context.Background(), s.svcCtx).SubmitRebuildTask(
			submitReq(model.ScopeFull, "", "", "req-1"))
		wantErrIs(t, err, errOther, "必须传播")
		if reply != nil {
			t.Fatalf("失败时响应必须为 nil, got %+v", reply)
		}
		wantNoCall(t, s.ops(), "task.Insert") // 查不清就别写，否则可能造出第二个任务
	})
	t.Run("任务落库失败", func(t *testing.T) {
		s := newTestStore(t, defaultOptions())
		s.fail("task.Insert", errOther)
		_, err := NewSubmitRebuildTaskLogic(context.Background(), s.svcCtx).SubmitRebuildTask(
			submitReq(model.ScopeFull, "", "", "req-1"))
		wantErrIs(t, err, errOther, "必须传播")
	})
}

// 别名切换的索引前缀约束与提交侧同源：Options.NewIndexName。
func TestSubmitRebuildTask_TargetIndexMatchesRepositoryNaming(t *testing.T) {
	s := newTestStore(t, repository.Options{IndexPrefix: "GC", SchemaVersion: "V2"})
	wantEQ(t, s.opts.IndexPrefix, "gc", "IndexPrefix 必须小写（OpenSearch 索引名不允许大写）")
	wantEQ(t, s.opts.SchemaVersion, "v2", "SchemaVersion 必须小写")
	reply, err := NewSubmitRebuildTaskLogic(context.Background(), s.svcCtx).SubmitRebuildTask(
		submitReq(model.ScopeFull, "", "", "req-n"))
	wantNoErr(t, err)
	if !regexp.MustCompile(`^gc_v2_\d{10}$`).MatchString(reply.TargetIndex) {
		t.Fatalf("target_index = %q, 期望 gc_v2_<unix秒>", reply.TargetIndex)
	}
}
