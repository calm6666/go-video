package logic

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	cronrpc "go-video/services/cron/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 cron 的口径：枚举双向映射一一对应、0 值不被当成合法枚举、
// operator 只来自会话身份、写接口 idempotency_key/reason 门槛、乐观锁版本号边界、
// 游标分页参数的非负与形态校验、服务端记账字段不得由客户端声明，
// 以及 rpc→types 投影逐字段不丢（时间戳、状态机、fence_token、takeover_count）。
//
// 「调度参数是否自洽」「expected_version 是否冲突」「租约归属」「游标单调性」
// 都是 services/cron 的领域规则，网关不复算（AGENTS.md §5/§9）：这里断言的是
// 「网关把入参原样交给下游 + 下游结论原样回传」，而不是「网关自己判过」。
// 打桩方式与 audit/ops-config 测试一致：内嵌生成的 client 接口 + 覆盖所需方法，
// 不建 gRPC 连接、不碰数据库。

var errCronFakeDownstream = errors.New("cron downstream unavailable")

// cronAdminFake 是 cron RPC 的假客户端：记录入参、返回预置响应或预置错误。
type cronAdminFake struct {
	cronrpc.CronClient

	err   error
	calls int

	listTasksReq   *cronrpc.ListTasksReq
	listTasksReply *cronrpc.ListTasksReply
	getTaskReq     *cronrpc.GetTaskReq
	getTaskReply   *cronrpc.GetTaskReply

	listRunsReq   *cronrpc.ListTaskRunsReq
	listRunsReply *cronrpc.ListTaskRunsReply
	getRunReq     *cronrpc.GetTaskRunReq
	getRunReply   *cronrpc.GetTaskRunReply

	listCpReq   *cronrpc.ListCheckpointsReq
	listCpReply *cronrpc.ListCheckpointsReply
	getCpReq    *cronrpc.GetCheckpointReq
	getCpReply  *cronrpc.GetCheckpointReply
	saveCpReq   *cronrpc.SaveCheckpointReq
	saveCpReply *cronrpc.SaveCheckpointReply

	listLeasesReq   *cronrpc.ListLeasesReq
	listLeasesReply *cronrpc.ListLeasesReply
	getLeaseReq     *cronrpc.GetLeaseReq
	getLeaseReply   *cronrpc.GetLeaseReply

	listAuditsReq   *cronrpc.ListTaskAuditsReq
	listAuditsReply *cronrpc.ListTaskAuditsReply

	healthReq   *cronrpc.GetSchedulerHealthReq
	healthReply *cronrpc.GetSchedulerHealthReply

	registerReq   *cronrpc.RegisterTaskReq
	registerReply *cronrpc.RegisterTaskReply
	updateReq     *cronrpc.UpdateTaskReq
	updateReply   *cronrpc.UpdateTaskReply
	pauseReq      *cronrpc.PauseTaskReq
	resumeReq     *cronrpc.ResumeTaskReq
	disableReq    *cronrpc.DisableTaskReq
	opReply       *cronrpc.TaskOperationReply
	triggerReq    *cronrpc.TriggerTaskReq
	triggerReply  *cronrpc.TriggerTaskReply
	retryReq      *cronrpc.RetryRunReq
	retryReply    *cronrpc.RetryRunReply
}

func (f *cronAdminFake) ListTasks(_ context.Context, in *cronrpc.ListTasksReq,
	_ ...grpc.CallOption) (*cronrpc.ListTasksReply, error) {
	f.calls++
	f.listTasksReq = in
	return f.listTasksReply, f.err
}

func (f *cronAdminFake) GetTask(_ context.Context, in *cronrpc.GetTaskReq,
	_ ...grpc.CallOption) (*cronrpc.GetTaskReply, error) {
	f.calls++
	f.getTaskReq = in
	return f.getTaskReply, f.err
}

func (f *cronAdminFake) ListTaskRuns(_ context.Context, in *cronrpc.ListTaskRunsReq,
	_ ...grpc.CallOption) (*cronrpc.ListTaskRunsReply, error) {
	f.calls++
	f.listRunsReq = in
	return f.listRunsReply, f.err
}

func (f *cronAdminFake) GetTaskRun(_ context.Context, in *cronrpc.GetTaskRunReq,
	_ ...grpc.CallOption) (*cronrpc.GetTaskRunReply, error) {
	f.calls++
	f.getRunReq = in
	return f.getRunReply, f.err
}

func (f *cronAdminFake) ListCheckpoints(_ context.Context, in *cronrpc.ListCheckpointsReq,
	_ ...grpc.CallOption) (*cronrpc.ListCheckpointsReply, error) {
	f.calls++
	f.listCpReq = in
	return f.listCpReply, f.err
}

func (f *cronAdminFake) GetCheckpoint(_ context.Context, in *cronrpc.GetCheckpointReq,
	_ ...grpc.CallOption) (*cronrpc.GetCheckpointReply, error) {
	f.calls++
	f.getCpReq = in
	return f.getCpReply, f.err
}

func (f *cronAdminFake) SaveCheckpoint(_ context.Context, in *cronrpc.SaveCheckpointReq,
	_ ...grpc.CallOption) (*cronrpc.SaveCheckpointReply, error) {
	f.calls++
	f.saveCpReq = in
	return f.saveCpReply, f.err
}

func (f *cronAdminFake) ListLeases(_ context.Context, in *cronrpc.ListLeasesReq,
	_ ...grpc.CallOption) (*cronrpc.ListLeasesReply, error) {
	f.calls++
	f.listLeasesReq = in
	return f.listLeasesReply, f.err
}

func (f *cronAdminFake) GetLease(_ context.Context, in *cronrpc.GetLeaseReq,
	_ ...grpc.CallOption) (*cronrpc.GetLeaseReply, error) {
	f.calls++
	f.getLeaseReq = in
	return f.getLeaseReply, f.err
}

func (f *cronAdminFake) ListTaskAudits(_ context.Context, in *cronrpc.ListTaskAuditsReq,
	_ ...grpc.CallOption) (*cronrpc.ListTaskAuditsReply, error) {
	f.calls++
	f.listAuditsReq = in
	return f.listAuditsReply, f.err
}

func (f *cronAdminFake) GetSchedulerHealth(_ context.Context, in *cronrpc.GetSchedulerHealthReq,
	_ ...grpc.CallOption) (*cronrpc.GetSchedulerHealthReply, error) {
	f.calls++
	f.healthReq = in
	return f.healthReply, f.err
}

func (f *cronAdminFake) RegisterTask(_ context.Context, in *cronrpc.RegisterTaskReq,
	_ ...grpc.CallOption) (*cronrpc.RegisterTaskReply, error) {
	f.calls++
	f.registerReq = in
	return f.registerReply, f.err
}

func (f *cronAdminFake) UpdateTask(_ context.Context, in *cronrpc.UpdateTaskReq,
	_ ...grpc.CallOption) (*cronrpc.UpdateTaskReply, error) {
	f.calls++
	f.updateReq = in
	if f.err != nil {
		return nil, f.err
	}
	if f.updateReply != nil {
		return f.updateReply, nil
	}
	return &cronrpc.UpdateTaskReply{Definition: f.updateReq.GetDefinition()}, nil
}

func (f *cronAdminFake) PauseTask(_ context.Context, in *cronrpc.PauseTaskReq,
	_ ...grpc.CallOption) (*cronrpc.TaskOperationReply, error) {
	f.calls++
	f.pauseReq = in
	return f.opReply, f.err
}

func (f *cronAdminFake) ResumeTask(_ context.Context, in *cronrpc.ResumeTaskReq,
	_ ...grpc.CallOption) (*cronrpc.TaskOperationReply, error) {
	f.calls++
	f.resumeReq = in
	return f.opReply, f.err
}

func (f *cronAdminFake) DisableTask(_ context.Context, in *cronrpc.DisableTaskReq,
	_ ...grpc.CallOption) (*cronrpc.TaskOperationReply, error) {
	f.calls++
	f.disableReq = in
	return f.opReply, f.err
}

func (f *cronAdminFake) TriggerTask(_ context.Context, in *cronrpc.TriggerTaskReq,
	_ ...grpc.CallOption) (*cronrpc.TriggerTaskReply, error) {
	f.calls++
	f.triggerReq = in
	return f.triggerReply, f.err
}

func (f *cronAdminFake) RetryRun(_ context.Context, in *cronrpc.RetryRunReq,
	_ ...grpc.CallOption) (*cronrpc.RetryRunReply, error) {
	f.calls++
	f.retryReq = in
	return f.retryReply, f.err
}

// --- 测试夹具 ---

func cronAdminSvc(fake cronrpc.CronClient) *svc.ServiceContext {
	return &svc.ServiceContext{Cron: fake}
}

// cronAdminSessionCtx 模拟 AdminPermission 中间件已解析出会话身份的请求上下文。
func cronAdminSessionCtx() context.Context {
	return middleware.WithAdmin(context.Background(), middleware.AdminIdentity{
		AdminID: 77,
		Roles:   []string{"scheduler_operator"},
	})
}

// cronFixtureDefinitionRPC 每个字段都取互不相同的非零值：
// 投影时漏掉或错位任何一列，与期望结构体的 DeepEqual 就会失败。
func cronFixtureDefinitionRPC() *cronrpc.TaskDefinition {
	return &cronrpc.TaskDefinition{
		TaskId:               101,
		TaskKey:              "rights.expire_scan",
		Name:                 "版权到期扫描",
		Handler:              "rightsExpireScan",
		TaskGroup:            "rights",
		ScheduleType:         cronrpc.ScheduleType_SCHEDULE_TYPE_CRON,
		CronExpr:             "0 */5 * * *",
		IntervalSeconds:      300,
		Timezone:             "Asia/Shanghai",
		TimeoutSeconds:       240,
		MaxAttempts:          3,
		RetryBaseSeconds:     30,
		RetryMaxSeconds:      600,
		ConcurrencyLimit:     1,
		LeaseTtlSeconds:      120,
		MisfirePolicy:        cronrpc.MisfirePolicy_MISFIRE_POLICY_FIRE_ALL,
		MisfireBackfillLimit: 5,
		Params:               `{"batch":200}`,
		SecretRefs:           "CRON_OSS_KEY",
		State:                cronrpc.TaskState_TASK_STATE_PAUSED,
		NextFireAt:           1700000300,
		LastFireAt:           1700000000,
		LastSuccessAt:        1699999700,
		LastError:            "scan timeout after 240s",
		Version:              7,
		Owner:                "rights-team",
		Operator:             "gateway/admin:77",
		Ctime:                1690000000,
		Mtime:                1699999999,
	}
}

func cronFixtureRunRPC() *cronrpc.RunRecord {
	return &cronrpc.RunRecord{
		RunId:         501,
		TaskKey:       "index.rebuild_daily",
		PlannedAt:     1700000000,
		Attempt:       2,
		TriggerType:   cronrpc.TriggerType_TRIGGER_TYPE_RETRY,
		State:         cronrpc.RunState_RUN_STATE_RETRYING,
		LeaseOwner:    "cron-worker-3-1123-a1b2",
		LeaseExpireAt: 1700000120,
		FenceToken:    9,
		StartedAt:     1700000005,
		FinishedAt:    1700000105,
		DurationMs:    100000,
		ResultSummary: "scanned=1200,updated=34",
		LastError:     "es bulk rejected",
		NextRetryAt:   1700000400,
		TraceId:       "trace-run-501",
		Ctime:         1700000001,
		Mtime:         1700000106,
	}
}

func cronFixtureCheckpointRPC() *cronrpc.Checkpoint {
	return &cronrpc.Checkpoint{
		TaskKey:  "report.daily_export",
		ScopeKey: "shard=7",
		Value:    987654321,
		ValueStr: "idx-report-2026.09",
		Version:  4,
		Operator: "gateway/admin:77",
		Ctime:    1690000000,
		Mtime:    1699999999,
	}
}

func cronFixtureLeaseRPC() *cronrpc.LeaseInfo {
	return &cronrpc.LeaseInfo{
		LeaseKey:      "cleanup.expired_media/shard=3",
		Owner:         "cron-worker-1-808-c3d4",
		FenceToken:    12,
		ExpireAt:      1700000500,
		AcquiredAt:    1700000300,
		TakeoverCount: 2,
	}
}

func cronFixtureAuditRPC() *cronrpc.TaskAudit {
	return &cronrpc.TaskAudit{
		Id:        9001,
		TaskKey:   "rights.expire_scan",
		Action:    "pause",
		FromState: "enabled",
		ToState:   "paused",
		Operator:  "gateway/admin:77",
		Detail:    `{"reason":"上游 rights 发布中"}`,
		TraceId:   "trace-audit-9001",
		Ctime:     1700000000,
	}
}

func cronFixtureGroupHealthRPC() *cronrpc.GroupHealth {
	return &cronrpc.GroupHealth{
		TaskGroup:          "rights",
		EnabledTasks:       12,
		PausedTasks:        3,
		DueBacklog:         41,
		Running:            2,
		Retrying:           5,
		FailedLastHour:     7,
		ExpiredLeases:      1,
		OldestDuePlannedAt: 1699990000,
	}
}

// --- 枚举双向映射 ---

// assertCronEnumTable 校验一张「proto↔后台」枚举表：条目数、编号、反查表与 UNSPECIFIED。
func assertCronEnumTable[V cronEnum](t *testing.T, kind string, toAPI map[V]int32,
	fromAPI map[int32]V, names map[int32]string) {
	t.Helper()
	if len(toAPI) != len(names) {
		t.Fatalf("%s：网关表 %d 条 != proto 枚举 %d 条，两侧不同源", kind, len(toAPI), len(names))
	}
	for num, name := range names {
		v, ok := toAPI[V(num)]
		if !ok {
			t.Errorf("proto %s=%d 在网关投影表里没有条目", name, num)
			continue
		}
		if int32(v) != num {
			t.Errorf("proto %s=%d 投影成后台 %d，编号漂移", name, num, int32(v))
		}
		back, ok := fromAPI[num]
		if !ok {
			t.Errorf("%s=%d 无法由后台值反查 proto 枚举", name, num)
			continue
		}
		if int32(back) != num {
			t.Errorf("%s=%d 反查得到 %v", name, num, back)
		}
	}
	// UNSPECIFIED 必须显式存在并映射为 0，否则无法区分「未指定」与第一个枚举。
	if n, ok := toAPI[V(0)]; !ok || n != 0 {
		t.Errorf("%s 缺少 UNSPECIFIED→0 的条目", kind)
	}
	if _, ok := fromAPI[0]; !ok {
		t.Errorf("%s 反查表缺少 0→UNSPECIFIED", kind)
	}
}

// TestCronEnumTablesMatchRpcContract 用反向覆盖率写法锁定五组枚举：
// proto 侧每个 _name 条目（含 UNSPECIFIED）都必须在网关表里且编号一致，
// 新增枚举值而未确认后台口径时这里必须失败。
func TestCronEnumTablesMatchRpcContract(t *testing.T) {
	assertCronEnumTable(t, "TaskState", cronTaskStateToAPI, cronTaskStateFromAPI, cronrpc.TaskState_name)
	assertCronEnumTable(t, "ScheduleType", cronScheduleTypeToAPI, cronScheduleTypeFromAPI, cronrpc.ScheduleType_name)
	assertCronEnumTable(t, "MisfirePolicy", cronMisfirePolicyToAPI, cronMisfirePolicyFromAPI, cronrpc.MisfirePolicy_name)
	assertCronEnumTable(t, "RunState", cronRunStateToAPI, cronRunStateFromAPI, cronrpc.RunState_name)
	assertCronEnumTable(t, "TriggerType", cronTriggerTypeToAPI, cronTriggerTypeFromAPI, cronrpc.TriggerType_name)
}

// TestCronZeroValueIsNotSilentlyValidEnum 保证 0 只表示「未指定」，
// 既不会被写成第一个枚举，也不会在必须显式指定的地方被放行。
func TestCronZeroValueIsNotSilentlyValidEnum(t *testing.T) {
	// 查询/注册语境：0 合法且必须保持 UNSPECIFIED（注册时由 cron 落库为 ENABLED）。
	st, err := cronTaskState(0)
	if err != nil || st != cronrpc.TaskState_TASK_STATE_UNSPECIFIED {
		t.Fatalf("state=0 应映射为 UNSPECIFIED，实际 %v err=%v", st, err)
	}
	rs, err := cronRunStateFilter(0)
	if err != nil || rs != cronrpc.RunState_RUN_STATE_UNSPECIFIED {
		t.Fatalf("run state=0 应映射为 UNSPECIFIED，实际 %v err=%v", rs, err)
	}
	mp, err := cronMisfirePolicy(0)
	if err != nil || mp != cronrpc.MisfirePolicy_MISFIRE_POLICY_UNSPECIFIED {
		t.Fatalf("misfire=0 应映射为 UNSPECIFIED，实际 %v err=%v", mp, err)
	}
	// 写语境：调度方式必须显式给出，0 不能被当成 SCHEDULE_TYPE_CRON。
	if _, err := cronScheduleType(0); err == nil {
		t.Fatal("schedule_type=0 被当成合法调度方式")
	}
	if _, err := cronScheduleType(int32(cronrpc.ScheduleType_SCHEDULE_TYPE_MANUAL)); err != nil {
		t.Fatalf("schedule_type=3 应合法，实际 %v", err)
	}
	// 未知编号必须报错，而不是静默退化成 0（「全部」）。
	for _, bad := range []int32{-1, 4, 99} {
		if _, err := cronTaskState(bad); err == nil {
			t.Errorf("task state=%d 被当成合法枚举", bad)
		}
	}
	for _, bad := range []int32{-1, 9, 42} {
		if _, err := cronRunStateFilter(bad); err == nil {
			t.Errorf("run state=%d 被当成合法枚举", bad)
		}
	}
	if _, err := cronRunStateFilter(int32(cronrpc.RunState_RUN_STATE_SKIPPED)); err != nil {
		t.Fatalf("run state=8 应合法，实际 %v", err)
	}
	// 未投影出的 rpc 值不得被压成 0：宁可暴露原编号，也不让后台显示成「未指定」。
	if got := cronRunToAPI(&cronrpc.RunRecord{State: 42}).State; got != 42 {
		t.Fatalf("未知 run state 应原样透传编号，实际 %d", got)
	}
	if got := cronTaskToAPI(&cronrpc.TaskDefinition{State: 77}).State; got != 77 {
		t.Fatalf("未知 task state 应原样透传编号，实际 %d", got)
	}
}

// TestCronProjectionKeepsEveryField 逐个结构体做 DeepEqual，防止漏投影列。
func TestCronProjectionKeepsEveryField(t *testing.T) {
	def := cronFixtureDefinitionRPC()
	if got := cronTaskToAPI(def); !reflect.DeepEqual(got, types.CronTaskDefinition{
		TaskId: 101, TaskKey: "rights.expire_scan", Name: "版权到期扫描", Handler: "rightsExpireScan",
		TaskGroup: "rights", ScheduleType: 1, CronExpr: "0 */5 * * *", IntervalSeconds: 300,
		Timezone: "Asia/Shanghai", TimeoutSeconds: 240, MaxAttempts: 3, RetryBaseSeconds: 30,
		RetryMaxSeconds: 600, ConcurrencyLimit: 1, LeaseTtlSeconds: 120, MisfirePolicy: 3,
		MisfireBackfillLimit: 5, Params: `{"batch":200}`, SecretRefs: "CRON_OSS_KEY", State: 2,
		NextFireAt: 1700000300, LastFireAt: 1700000000, LastSuccessAt: 1699999700,
		LastError: "scan timeout after 240s", Version: 7, Owner: "rights-team",
		Operator: "gateway/admin:77", Ctime: 1690000000, Mtime: 1699999999,
	}) {
		t.Fatalf("任务定义投影不一致：%+v", got)
	}

	run := cronFixtureRunRPC()
	if got := cronRunToAPI(run); !reflect.DeepEqual(got, types.CronTaskRun{
		RunId: 501, TaskKey: "index.rebuild_daily", PlannedAt: 1700000000, Attempt: 2,
		TriggerType: 3, State: 3, LeaseOwner: "cron-worker-3-1123-a1b2", LeaseExpireAt: 1700000120,
		FenceToken: 9, StartedAt: 1700000005, FinishedAt: 1700000105, DurationMs: 100000,
		ResultSummary: "scanned=1200,updated=34", LastError: "es bulk rejected",
		NextRetryAt: 1700000400, TraceId: "trace-run-501", Ctime: 1700000001, Mtime: 1700000106,
	}) {
		t.Fatalf("执行记录投影不一致：%+v", got)
	}

	cp := cronFixtureCheckpointRPC()
	if got := cronCheckpointToAPI(cp); !reflect.DeepEqual(got, types.CronCheckpoint{
		TaskKey: "report.daily_export", ScopeKey: "shard=7", Value: 987654321,
		ValueStr: "idx-report-2026.09", Version: 4, Operator: "gateway/admin:77",
		Ctime: 1690000000, Mtime: 1699999999,
	}) {
		t.Fatalf("游标投影不一致：%+v", got)
	}

	lease := cronFixtureLeaseRPC()
	if got := cronLeaseToAPI(lease); !reflect.DeepEqual(got, types.CronLease{
		LeaseKey: "cleanup.expired_media/shard=3", Owner: "cron-worker-1-808-c3d4",
		FenceToken: 12, ExpireAt: 1700000500, AcquiredAt: 1700000300, TakeoverCount: 2,
	}) {
		t.Fatalf("租约投影不一致：%+v", got)
	}

	audit := cronFixtureAuditRPC()
	if got := cronTaskAuditToAPI(audit); !reflect.DeepEqual(got, types.CronTaskAudit{
		Id: 9001, TaskKey: "rights.expire_scan", Action: "pause", FromState: "enabled",
		ToState: "paused", Operator: "gateway/admin:77", Detail: `{"reason":"上游 rights 发布中"}`,
		TraceId: "trace-audit-9001", Ctime: 1700000000,
	}) {
		t.Fatalf("任务审计投影不一致：%+v", got)
	}

	group := cronFixtureGroupHealthRPC()
	if got := cronGroupHealthToAPI(group); !reflect.DeepEqual(got, types.CronGroupHealth{
		TaskGroup: "rights", EnabledTasks: 12, PausedTasks: 3, DueBacklog: 41, Running: 2,
		Retrying: 5, FailedLastHour: 7, ExpiredLeases: 1, OldestDuePlannedAt: 1699990000,
	}) {
		t.Fatalf("分组健康度投影不一致：%+v", got)
	}

	// nil（found=false 时的空对象）投影成零值而不是 panic，后台据此区分「不存在」与「全零」。
	if got := cronTaskToAPI(nil); !reflect.DeepEqual(got, types.CronTaskDefinition{}) {
		t.Fatalf("nil 任务定义应投影为零值，实际 %+v", got)
	}
	if got := cronRunToAPI(nil); !reflect.DeepEqual(got, types.CronTaskRun{}) {
		t.Fatalf("nil 执行记录应投影为零值，实际 %+v", got)
	}
	if got := cronCheckpointToAPI(nil); !reflect.DeepEqual(got, types.CronCheckpoint{}) {
		t.Fatalf("nil 游标应投影为零值，实际 %+v", got)
	}
	if got := cronLeaseToAPI(nil); !reflect.DeepEqual(got, types.CronLease{}) {
		t.Fatalf("nil 租约应投影为零值，实际 %+v", got)
	}
	if got := cronTaskAuditToAPI(nil); !reflect.DeepEqual(got, types.CronTaskAudit{}) {
		t.Fatalf("nil 审计应投影为零值，实际 %+v", got)
	}
	if got := cronGroupHealthToAPI(nil); !reflect.DeepEqual(got, types.CronGroupHealth{}) {
		t.Fatalf("nil 分组健康度应投影为零值，实际 %+v", got)
	}
	// oldest_due_planned_at=0 表示「无积压」，不得被改写成任何默认时间。
	if got := cronGroupHealthToAPI(&cronrpc.GroupHealth{TaskGroup: "cleanup"}).OldestDuePlannedAt; got != 0 {
		t.Fatalf("无积压被投影成 %d", got)
	}
}

// --- 只读面：参数透传 + 错误路径 ---

func TestCronListTasksPassesThroughAndProjects(t *testing.T) {
	fake := &cronAdminFake{listTasksReply: &cronrpc.ListTasksReply{
		List:       []*cronrpc.TaskDefinition{cronFixtureDefinitionRPC()},
		NextCursor: "rights.expire_scan",
		HasMore:    true,
		Total:      37,
	}}
	resp, err := NewCronListTasksLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).
		CronListTasks(&types.ParamCronListTasks{
			State: 2, TaskGroup: "rights", Handler: "rightsExpireScan",
			Cursor: "rights.expire_scan", PageSize: 50,
		})
	if err != nil {
		t.Fatalf("ListTasks 失败: %v", err)
	}
	in := fake.listTasksReq
	if in.GetState() != cronrpc.TaskState_TASK_STATE_PAUSED || in.GetTaskGroup() != "rights" ||
		in.GetHandler() != "rightsExpireScan" || in.GetCursor() != "rights.expire_scan" || in.GetPageSize() != 50 {
		t.Fatalf("入参未原样透传：%+v", in)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封不符合约定：%+v", resp)
	}
	if resp.Data.NextCursor != "rights.expire_scan" || !resp.Data.HasMore || resp.Data.Total != 37 {
		t.Fatalf("游标分页字段丢失：%+v", resp.Data)
	}
	if !reflect.DeepEqual(resp.Data.List[0], cronTaskToAPI(cronFixtureDefinitionRPC())) {
		t.Fatalf("列表项投影不完整：%+v", resp.Data.List[0])
	}
}

func TestCronListTasksBoundaryErrors(t *testing.T) {
	cases := []struct {
		name string
		req  *types.ParamCronListTasks
	}{
		{name: "未知状态", req: &types.ParamCronListTasks{State: 9}},
		{name: "负 page_size", req: &types.ParamCronListTasks{PageSize: -1}},
		{name: "超长游标", req: &types.ParamCronListTasks{Cursor: strings.Repeat("k", cronMaxCursorLen+1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &cronAdminFake{}
			if _, err := NewCronListTasksLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).CronListTasks(tc.req); err == nil {
				t.Fatal("越界入参被放行")
			}
			if fake.calls != 0 {
				t.Fatalf("校验失败仍调用了下游 %d 次", fake.calls)
			}
		})
	}
	// page_size=0 是「用服务端默认值」的合法语义，0 值不得被当成越界。
	fake := &cronAdminFake{}
	if _, err := NewCronListTasksLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).
		CronListTasks(&types.ParamCronListTasks{PageSize: 0}); err != nil {
		t.Fatalf("page_size=0 应透传给 cron 判定，实际 %v", err)
	}
	if fake.listTasksReq.GetPageSize() != 0 {
		t.Fatalf("网关截断了 page_size：%d", fake.listTasksReq.GetPageSize())
	}
}

func TestCronGetTaskFoundDerivedFromDefinition(t *testing.T) {
	fake := &cronAdminFake{getTaskReply: &cronrpc.GetTaskReply{Definition: cronFixtureDefinitionRPC()}}
	resp, err := NewCronGetTaskLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).
		CronGetTask(&types.ParamCronGetTask{TaskKey: "rights.expire_scan"})
	if err != nil {
		t.Fatalf("GetTask 失败: %v", err)
	}
	if !resp.Data.Found || resp.Data.Definition.Version != 7 {
		t.Fatalf("命中时 found/version 不正确：%+v", resp.Data)
	}
	if fake.getTaskReq.GetTaskKey() != "rights.expire_scan" {
		t.Fatalf("task_key 未透传：%+v", fake.getTaskReq)
	}

	// 下游返回空 definition：未命中不是错误，但也不能伪造数据。
	empty := &cronAdminFake{getTaskReply: &cronrpc.GetTaskReply{}}
	resp, err = NewCronGetTaskLogic(cronAdminSessionCtx(), cronAdminSvc(empty)).
		CronGetTask(&types.ParamCronGetTask{TaskKey: "missing.task"})
	if err != nil {
		t.Fatalf("未命中不应报错: %v", err)
	}
	if resp.Data.Found || !reflect.DeepEqual(resp.Data.Definition, types.CronTaskDefinition{}) {
		t.Fatalf("未命中被投影成有数据：%+v", resp.Data)
	}

	noKey := &cronAdminFake{}
	if _, err := NewCronGetTaskLogic(cronAdminSessionCtx(), cronAdminSvc(noKey)).
		CronGetTask(&types.ParamCronGetTask{TaskKey: "   "}); err == nil {
		t.Fatal("空白 task_key 被放行")
	}
	if noKey.calls != 0 {
		t.Fatalf("缺参仍调用下游 %d 次", noKey.calls)
	}
}

func TestCronListTaskRunsValidatesWindowAndState(t *testing.T) {
	fake := &cronAdminFake{}
	if _, err := NewCronListTaskRunsLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).
		CronListTaskRuns(&types.ParamCronListTaskRuns{State: 42}); err == nil {
		t.Fatal("未知 run state 被放行")
	}
	if _, err := NewCronListTaskRunsLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).
		CronListTaskRuns(&types.ParamCronListTaskRuns{PlannedFrom: -5}); err == nil {
		t.Fatal("负 planned_from 被放行")
	}
	if _, err := NewCronListTaskRunsLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).
		CronListTaskRuns(&types.ParamCronListTaskRuns{PlannedFrom: 200, PlannedTo: 100}); err == nil {
		t.Fatal("反向时间窗被放行")
	}
	if fake.calls != 0 {
		t.Fatalf("校验失败仍调用下游 %d 次", fake.calls)
	}

	ok := &cronAdminFake{listRunsReply: &cronrpc.ListTaskRunsReply{
		List: []*cronrpc.RunRecord{cronFixtureRunRPC()}, NextCursor: "501", HasMore: true, Total: 8,
	}}
	resp, err := NewCronListTaskRunsLogic(cronAdminSessionCtx(), cronAdminSvc(ok)).
		CronListTaskRuns(&types.ParamCronListTaskRuns{
			TaskKey: "index.rebuild_daily", State: 3, PlannedFrom: 100, PlannedTo: 200,
			Cursor: "501", PageSize: 20,
		})
	if err != nil {
		t.Fatalf("ListTaskRuns 失败: %v", err)
	}
	in := ok.listRunsReq
	if in.GetState() != cronrpc.RunState_RUN_STATE_RETRYING || in.GetPlannedFrom() != 100 ||
		in.GetPlannedTo() != 200 || in.GetCursor() != "501" || in.GetPageSize() != 20 ||
		in.GetTaskKey() != "index.rebuild_daily" {
		t.Fatalf("入参未原样透传：%+v", in)
	}
	if resp.Data.NextCursor != "501" || !resp.Data.HasMore || resp.Data.Total != 8 {
		t.Fatalf("分页字段丢失：%+v", resp.Data)
	}
	if resp.Data.List[0].FenceToken != 9 || resp.Data.List[0].Attempt != 2 ||
		resp.Data.List[0].LeaseExpireAt != 1700000120 {
		t.Fatalf("栅栏令牌/尝试次数/租约到期被丢弃：%+v", resp.Data.List[0])
	}
}

func TestCronGetTaskRunRequiresRunID(t *testing.T) {
	fake := &cronAdminFake{getRunReply: &cronrpc.GetTaskRunReply{Run: cronFixtureRunRPC()}}
	runResp, err := NewCronGetTaskRunLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).
		CronGetTaskRun(&types.ParamCronGetTaskRun{RunId: 501})
	if err != nil {
		t.Fatalf("GetTaskRun 失败: %v", err)
	}
	if !runResp.Data.Found || runResp.Data.Run.RunId != 501 || runResp.Data.Run.TraceId != "trace-run-501" {
		t.Fatalf("执行记录投影不完整：%+v", runResp.Data)
	}
	if _, err := NewCronGetTaskRunLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).
		CronGetTaskRun(&types.ParamCronGetTaskRun{RunId: 0}); err == nil {
		t.Fatal("run_id=0 被当成合法查询")
	}

	// 下游没有回 run 时不得伪造 found=true。
	empty := &cronAdminFake{getRunReply: &cronrpc.GetTaskRunReply{}}
	resp, err := NewCronGetTaskRunLogic(cronAdminSessionCtx(), cronAdminSvc(empty)).
		CronGetTaskRun(&types.ParamCronGetTaskRun{RunId: 777})
	if err != nil {
		t.Fatalf("未命中不应报错: %v", err)
	}
	if resp.Data.Found || !reflect.DeepEqual(resp.Data.Run, types.CronTaskRun{}) {
		t.Fatalf("未命中被投影成有数据：%+v", resp.Data)
	}
}

func TestCronCheckpointReadRoutes(t *testing.T) {
	fake := &cronAdminFake{
		listCpReply: &cronrpc.ListCheckpointsReply{
			List: []*cronrpc.Checkpoint{cronFixtureCheckpointRPC()}, NextCursor: "report.daily_export",
			HasMore: true, Total: 3,
		},
		getCpReply: &cronrpc.GetCheckpointReply{Checkpoint: cronFixtureCheckpointRPC(), Found: true},
	}
	listResp, err := NewCronListCheckpointsLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).
		CronListCheckpoints(&types.ParamCronListCheckpoints{TaskKey: "report.daily_export", PageSize: 10})
	if err != nil {
		t.Fatalf("ListCheckpoints 失败: %v", err)
	}
	if listResp.Data.List[0].ValueStr != "idx-report-2026.09" || listResp.Data.List[0].Version != 4 {
		t.Fatalf("游标字符串或 CAS 版本丢失：%+v", listResp.Data.List[0])
	}
	if fake.listCpReq.GetTaskKey() != "report.daily_export" || fake.listCpReq.GetPageSize() != 10 {
		t.Fatalf("列表入参未透传：%+v", fake.listCpReq)
	}

	getResp, err := NewCronGetCheckpointLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).
		CronGetCheckpoint(&types.ParamCronGetCheckpoint{TaskKey: "report.daily_export"})
	if err != nil {
		t.Fatalf("GetCheckpoint 失败: %v", err)
	}
	if !getResp.Data.Found || getResp.Data.Checkpoint.Value != 987654321 {
		t.Fatalf("found/value 不正确：%+v", getResp.Data)
	}
	if fake.getCpReq.GetScopeKey() != "" {
		t.Fatalf("scope_key 空串是「默认游标」，不得被改写：%q", fake.getCpReq.GetScopeKey())
	}

	// scope_key 空串合法，task_key 空白非法。
	empty := &cronAdminFake{getCpReply: &cronrpc.GetCheckpointReply{Found: false}}
	getResp, err = NewCronGetCheckpointLogic(cronAdminSessionCtx(), cronAdminSvc(empty)).
		CronGetCheckpoint(&types.ParamCronGetCheckpoint{TaskKey: "never.advanced"})
	if err != nil {
		t.Fatalf("未推进过游标不是错误: %v", err)
	}
	if getResp.Data.Found || !reflect.DeepEqual(getResp.Data.Checkpoint, types.CronCheckpoint{}) {
		t.Fatalf("found=false 却带回了游标数据：%+v", getResp.Data)
	}
	if _, err := NewCronGetCheckpointLogic(cronAdminSessionCtx(), cronAdminSvc(empty)).
		CronGetCheckpoint(&types.ParamCronGetCheckpoint{}); err == nil {
		t.Fatal("缺 task_key 被放行")
	}
	if _, err := NewCronListCheckpointsLogic(cronAdminSessionCtx(), cronAdminSvc(empty)).
		CronListCheckpoints(&types.ParamCronListCheckpoints{PageSize: -3}); err == nil {
		t.Fatal("负 page_size 被放行")
	}
}

func TestCronLeaseReadRoutes(t *testing.T) {
	fake := &cronAdminFake{
		listLeasesReply: &cronrpc.ListLeasesReply{
			List: []*cronrpc.LeaseInfo{cronFixtureLeaseRPC()}, NextCursor: "42", HasMore: false, Total: 1,
		},
		getLeaseReply: &cronrpc.GetLeaseReply{Lease: cronFixtureLeaseRPC(), Found: true},
	}
	listResp, err := NewCronListLeasesLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).
		CronListLeases(&types.ParamCronListLeases{TaskKey: "cleanup.expired_media", OnlyExpired: true, Now: 1700000400, PageSize: 5})
	if err != nil {
		t.Fatalf("ListLeases 失败: %v", err)
	}
	if !fake.listLeasesReq.GetOnlyExpired() || fake.listLeasesReq.GetNow() != 1700000400 {
		t.Fatalf("only_expired/now 未透传：%+v", fake.listLeasesReq)
	}
	if listResp.Data.List[0].TakeoverCount != 2 || listResp.Data.List[0].FenceToken != 12 {
		t.Fatalf("抢占证据丢失：%+v", listResp.Data.List[0])
	}
	if listResp.Data.NextCursor != "42" || listResp.Data.HasMore {
		t.Fatalf("游标字段丢失：%+v", listResp.Data)
	}

	// now=0 表示由 cron 取服务端当前时间，网关不得自己填一个时间。
	zero := &cronAdminFake{listLeasesReply: &cronrpc.ListLeasesReply{}}
	if _, err := NewCronListLeasesLogic(cronAdminSessionCtx(), cronAdminSvc(zero)).
		CronListLeases(&types.ParamCronListLeases{}); err != nil {
		t.Fatalf("now=0 应合法: %v", err)
	}
	if zero.listLeasesReq.GetNow() != 0 {
		t.Fatalf("网关替调用方决定了 now=%d", zero.listLeasesReq.GetNow())
	}
	if _, err := NewCronListLeasesLogic(cronAdminSessionCtx(), cronAdminSvc(zero)).
		CronListLeases(&types.ParamCronListLeases{Now: -1}); err == nil {
		t.Fatal("负 now 被放行")
	}

	getResp, err := NewCronGetLeaseLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).
		CronGetLease(&types.ParamCronGetLease{TaskKey: "cleanup.expired_media", Scope: "shard=3"})
	if err != nil {
		t.Fatalf("GetLease 失败: %v", err)
	}
	if !getResp.Data.Found || getResp.Data.Lease.LeaseKey != "cleanup.expired_media/shard=3" {
		t.Fatalf("租约投影不完整：%+v", getResp.Data)
	}
	if fake.getLeaseReq.GetScope() != "shard=3" {
		t.Fatalf("scope 未透传：%+v", fake.getLeaseReq)
	}
	if _, err := NewCronGetLeaseLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).
		CronGetLease(&types.ParamCronGetLease{}); err == nil {
		t.Fatal("缺 task_key 被放行")
	}
}

func TestCronAuditAndHealthReadRoutes(t *testing.T) {
	fake := &cronAdminFake{
		listAuditsReply: &cronrpc.ListTaskAuditsReply{
			List: []*cronrpc.TaskAudit{cronFixtureAuditRPC()}, NextCursor: "9001", HasMore: true, Total: 12,
		},
		healthReply: &cronrpc.GetSchedulerHealthReply{
			ServerTime: 1700000555, Version: "cron@1.2.3", Groups: []*cronrpc.GroupHealth{cronFixtureGroupHealthRPC()},
		},
	}
	auditResp, err := NewCronListTaskAuditsLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).
		CronListTaskAudits(&types.ParamCronListTaskAudits{
			TaskKey: "rights.expire_scan", Action: "pause", CtimeFrom: 100, CtimeTo: 200, PageSize: 20,
		})
	if err != nil {
		t.Fatalf("ListTaskAudits 失败: %v", err)
	}
	in := fake.listAuditsReq
	if in.GetAction() != "pause" || in.GetCtimeFrom() != 100 || in.GetCtimeTo() != 200 || in.GetPageSize() != 20 {
		t.Fatalf("审计过滤条件未透传：%+v", in)
	}
	if auditResp.Data.List[0].Detail == "" || auditResp.Data.List[0].TraceId != "trace-audit-9001" ||
		auditResp.Data.List[0].FromState != "enabled" || auditResp.Data.List[0].ToState != "paused" {
		t.Fatalf("审计证据字段丢失：%+v", auditResp.Data.List[0])
	}
	if _, err := NewCronListTaskAuditsLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).
		CronListTaskAudits(&types.ParamCronListTaskAudits{CtimeFrom: 300, CtimeTo: 100}); err == nil {
		t.Fatal("反向时间窗被放行")
	}

	healthResp, err := NewCronSchedulerHealthLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).
		CronSchedulerHealth(&types.ParamCronSchedulerHealth{})
	if err != nil {
		t.Fatalf("GetSchedulerHealth 失败: %v", err)
	}
	if healthResp.Data.ServerTime != 1700000555 || healthResp.Data.Version != "cron@1.2.3" {
		t.Fatalf("server_time/version 丢失：%+v", healthResp.Data)
	}
	if healthResp.Data.Groups[0].DueBacklog != 41 || healthResp.Data.Groups[0].ExpiredLeases != 1 {
		t.Fatalf("积压与过期租约丢失：%+v", healthResp.Data.Groups[0])
	}
	if fake.healthReq.GetNow() != 0 || fake.healthReq.GetTaskGroup() != "" {
		t.Fatalf("now=0/全部分组被改写：%+v", fake.healthReq)
	}
	if _, err := NewCronSchedulerHealthLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).
		CronSchedulerHealth(&types.ParamCronSchedulerHealth{Now: -1}); err == nil {
		t.Fatal("负 now 被放行")
	}
}

// --- 写面：会话身份、幂等键与服务端记账字段 ---

func TestCronWriteRoutesRequireSessionIdentity(t *testing.T) {
	noSession := context.Background()
	cases := map[string]func() error{
		"register": func() error {
			_, err := NewCronRegisterTaskLogic(noSession, cronAdminSvc(&cronAdminFake{})).
				CronRegisterTask(&types.ParamCronRegisterTask{IdempotencyKey: "k1"})
			return err
		},
		"update": func() error {
			_, err := NewCronUpdateTaskLogic(noSession, cronAdminSvc(&cronAdminFake{})).
				CronUpdateTask(&types.ParamCronUpdateTask{TaskKey: "a.b", ExpectedVersion: 1})
			return err
		},
		"pause": func() error {
			_, err := NewCronPauseTaskLogic(noSession, cronAdminSvc(&cronAdminFake{})).
				CronPauseTask(&types.ParamCronPauseTask{TaskKey: "a.b", Reason: "r", IdempotencyKey: "k"})
			return err
		},
		"resume": func() error {
			_, err := NewCronResumeTaskLogic(noSession, cronAdminSvc(&cronAdminFake{})).
				CronResumeTask(&types.ParamCronResumeTask{TaskKey: "a.b", IdempotencyKey: "k"})
			return err
		},
		"disable": func() error {
			_, err := NewCronDisableTaskLogic(noSession, cronAdminSvc(&cronAdminFake{})).
				CronDisableTask(&types.ParamCronDisableTask{TaskKey: "a.b", Reason: "r", IdempotencyKey: "k"})
			return err
		},
		"trigger": func() error {
			_, err := NewCronTriggerTaskLogic(noSession, cronAdminSvc(&cronAdminFake{})).
				CronTriggerTask(&types.ParamCronTriggerTask{TaskKey: "a.b", IdempotencyKey: "k"})
			return err
		},
		"retry": func() error {
			_, err := NewCronRetryRunLogic(noSession, cronAdminSvc(&cronAdminFake{})).
				CronRetryRun(&types.ParamCronRetryRun{RunId: 1, IdempotencyKey: "k", Reason: "r"})
			return err
		},
		"save checkpoint": func() error {
			_, err := NewCronSaveCheckpointLogic(noSession, cronAdminSvc(&cronAdminFake{})).
				CronSaveCheckpoint(&types.ParamCronSaveCheckpoint{
					Checkpoint: types.CronCheckpoint{TaskKey: "a.b"}, IdempotencyKey: "k"})
			return err
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, errCronSessionRequired) {
				t.Fatalf("无会话身份时应 fail-closed，实际 err=%v", err)
			}
		})
	}
}

func TestCronWriteRoutesRequireIdempotencyKey(t *testing.T) {
	ctx := cronAdminSessionCtx()
	cases := map[string]func(svcCtx *svc.ServiceContext) error{
		"register": func(s *svc.ServiceContext) error {
			_, err := NewCronRegisterTaskLogic(ctx, s).CronRegisterTask(&types.ParamCronRegisterTask{
				Definition: types.CronTaskDefinition{
					TaskKey: "a.b", Name: "n", Handler: "h",
					ScheduleType: int32(cronrpc.ScheduleType_SCHEDULE_TYPE_MANUAL),
				},
			})
			return err
		},
		"pause": func(s *svc.ServiceContext) error {
			_, err := NewCronPauseTaskLogic(ctx, s).CronPauseTask(&types.ParamCronPauseTask{TaskKey: "a.b", Reason: "r"})
			return err
		},
		"resume": func(s *svc.ServiceContext) error {
			_, err := NewCronResumeTaskLogic(ctx, s).CronResumeTask(&types.ParamCronResumeTask{TaskKey: "a.b"})
			return err
		},
		"disable": func(s *svc.ServiceContext) error {
			_, err := NewCronDisableTaskLogic(ctx, s).CronDisableTask(&types.ParamCronDisableTask{TaskKey: "a.b", Reason: "r"})
			return err
		},
		"trigger": func(s *svc.ServiceContext) error {
			_, err := NewCronTriggerTaskLogic(ctx, s).CronTriggerTask(&types.ParamCronTriggerTask{TaskKey: "a.b"})
			return err
		},
		"retry": func(s *svc.ServiceContext) error {
			_, err := NewCronRetryRunLogic(ctx, s).CronRetryRun(&types.ParamCronRetryRun{RunId: 5, Reason: "r"})
			return err
		},
		"save checkpoint": func(s *svc.ServiceContext) error {
			_, err := NewCronSaveCheckpointLogic(ctx, s).CronSaveCheckpoint(&types.ParamCronSaveCheckpoint{
				Checkpoint: types.CronCheckpoint{TaskKey: "a.b"}})
			return err
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &cronAdminFake{}
			if err := call(cronAdminSvc(fake)); err == nil {
				t.Fatal("空 idempotency_key 被放行")
			} else if !strings.Contains(err.Error(), "idempotency_key") {
				t.Fatalf("错误应指向 idempotency_key，实际 %v", err)
			}
			if fake.calls != 0 {
				t.Fatalf("缺幂等键仍调用下游 %d 次", fake.calls)
			}
		})
	}
}

func TestCronStateOperationsRequireReason(t *testing.T) {
	ctx := cronAdminSessionCtx()
	if _, err := NewCronPauseTaskLogic(ctx, cronAdminSvc(&cronAdminFake{})).
		CronPauseTask(&types.ParamCronPauseTask{TaskKey: "a.b", IdempotencyKey: "k"}); err == nil {
		t.Fatal("暂停未带 reason 被放行")
	}
	if _, err := NewCronDisableTaskLogic(ctx, cronAdminSvc(&cronAdminFake{})).
		CronDisableTask(&types.ParamCronDisableTask{TaskKey: "a.b", IdempotencyKey: "k"}); err == nil {
		t.Fatal("停用未带 reason 被放行")
	}
	if _, err := NewCronRetryRunLogic(ctx, cronAdminSvc(&cronAdminFake{})).
		CronRetryRun(&types.ParamCronRetryRun{RunId: 5, IdempotencyKey: "k"}); err == nil {
		t.Fatal("人工重试未带 reason 被放行")
	}
}

func TestCronRegisterTaskAssemblesDefinition(t *testing.T) {
	fake := &cronAdminFake{registerReply: &cronrpc.RegisterTaskReply{
		Definition: cronFixtureDefinitionRPC(), Created: false, DedupeReason: "uniq_task_key",
	}}
	resp, err := NewCronRegisterTaskLogic(cronAdminSessionCtx(), cronAdminSvc(fake)).
		CronRegisterTask(&types.ParamCronRegisterTask{
			Definition: types.CronTaskDefinition{
				TaskKey: "rights.expire_scan", Name: "版权到期扫描", Handler: "rightsExpireScan",
				TaskGroup: "rights", ScheduleType: int32(cronrpc.ScheduleType_SCHEDULE_TYPE_CRON),
				CronExpr: "0 */5 * * *", Timezone: "Asia/Shanghai", TimeoutSeconds: 240,
				MaxAttempts: 3, RetryBaseSeconds: 30, RetryMaxSeconds: 600, ConcurrencyLimit: 1,
				LeaseTtlSeconds: 120, MisfirePolicy: int32(cronrpc.MisfirePolicy_MISFIRE_POLICY_FIRE_ALL),
				MisfireBackfillLimit: 5, Params: `{"batch":200}`, SecretRefs: "CRON_OSS_KEY",
				State: int32(cronrpc.TaskState_TASK_STATE_PAUSED), Owner: "rights-team",
				// 服务端记账字段：客户端传了也必须被丢弃。
				TaskId: 999, Version: 42, NextFireAt: 123, LastFireAt: 456, LastSuccessAt: 789,
				LastError: "伪造", Operator: "someone-else", Ctime: 1, Mtime: 2,
			},
			IdempotencyKey: "register-rights-001",
			TraceId:        "trace-reg-1",
		})
	if err != nil {
		t.Fatalf("RegisterTask 失败: %v", err)
	}
	in := fake.registerReq
	if in.GetIdempotencyKey() != "register-rights-001" || in.GetTraceId() != "trace-reg-1" {
		t.Fatalf("幂等键/trace_id 未透传：%+v", in)
	}
	if in.GetOperator() != "gateway/admin:77" {
		t.Fatalf("operator 必须由会话身份渲染，实际 %q", in.GetOperator())
	}
	d := in.GetDefinition()
	if d.GetTaskKey() != "rights.expire_scan" || d.GetHandler() != "rightsExpireScan" ||
		d.GetScheduleType() != cronrpc.ScheduleType_SCHEDULE_TYPE_CRON ||
		d.GetMisfirePolicy() != cronrpc.MisfirePolicy_MISFIRE_POLICY_FIRE_ALL ||
		d.GetState() != cronrpc.TaskState_TASK_STATE_PAUSED {
		t.Fatalf("定义主体字段丢失：%+v", d)
	}
	if d.GetTaskId() != 0 || d.GetVersion() != 0 || d.GetNextFireAt() != 0 || d.GetLastFireAt() != 0 ||
		d.GetLastSuccessAt() != 0 || d.GetLastError() != "" || d.GetOperator() != "" ||
		d.GetCtime() != 0 || d.GetMtime() != 0 {
		t.Fatalf("服务端记账字段被客户端声明污染：%+v", d)
	}
	if resp.Data.Created || resp.Data.DedupeReason != "uniq_task_key" {
		t.Fatalf("幂等重入结论丢失：%+v", resp.Data)
	}
	if resp.Data.Definition.Version != 7 {
		t.Fatalf("回读定义未投影：%+v", resp.Data.Definition)
	}
}

func TestCronRegisterTaskRequiredFields(t *testing.T) {
	ctx := cronAdminSessionCtx()
	base := types.CronTaskDefinition{
		TaskKey: "a.b", Name: "n", Handler: "h",
		ScheduleType: int32(cronrpc.ScheduleType_SCHEDULE_TYPE_CRON),
	}
	cases := []struct {
		name  string
		def   types.CronTaskDefinition
		field string
	}{
		{name: "缺 task_key", def: withDef(base, func(d *types.CronTaskDefinition) { d.TaskKey = "" }), field: "task_key"},
		{name: "缺 name", def: withDef(base, func(d *types.CronTaskDefinition) { d.Name = "  " }), field: "name"},
		{name: "缺 handler", def: withDef(base, func(d *types.CronTaskDefinition) { d.Handler = "" }), field: "handler"},
		{name: "调度方式未指定", def: withDef(base, func(d *types.CronTaskDefinition) { d.ScheduleType = 0 }), field: "schedule_type"},
		{name: "调度方式未知", def: withDef(base, func(d *types.CronTaskDefinition) { d.ScheduleType = 9 }), field: "schedule type"},
		{name: "状态未知", def: withDef(base, func(d *types.CronTaskDefinition) { d.State = 5 }), field: "task state"},
		{name: "misfire 未知", def: withDef(base, func(d *types.CronTaskDefinition) { d.MisfirePolicy = 6 }), field: "misfire policy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &cronAdminFake{}
			req := &types.ParamCronRegisterTask{Definition: tc.def, IdempotencyKey: "k"}
			if _, err := NewCronRegisterTaskLogic(ctx, cronAdminSvc(fake)).CronRegisterTask(req); err == nil {
				t.Fatalf("非法定义被放行（应拒绝 %s）", tc.field)
			} else if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("错误未指明 %s：%v", tc.field, err)
			}
			if fake.calls != 0 {
				t.Fatalf("非法定义仍调用下游 %d 次", fake.calls)
			}
		})
	}
}

func withDef(d types.CronTaskDefinition, mutate func(*types.CronTaskDefinition)) types.CronTaskDefinition {
	out := d
	mutate(&out)
	return out
}

func TestCronUpdateTaskGuardsWithVersionAndTaskKey(t *testing.T) {
	ctx := cronAdminSessionCtx()
	def := types.CronTaskDefinition{
		TaskKey: "a.b", Name: "n", Handler: "h",
		ScheduleType: int32(cronrpc.ScheduleType_SCHEDULE_TYPE_INTERVAL), IntervalSeconds: 60,
		State: int32(cronrpc.TaskState_TASK_STATE_DISABLED), Version: 3, TaskId: 9,
	}
	fake := &cronAdminFake{}
	resp, err := NewCronUpdateTaskLogic(ctx, cronAdminSvc(fake)).
		CronUpdateTask(&types.ParamCronUpdateTask{TaskKey: "a.b", Definition: def, ExpectedVersion: 7, TraceId: "t-2"})
	if err != nil {
		t.Fatalf("UpdateTask 失败: %v", err)
	}
	in := fake.updateReq
	if in.GetTaskKey() != "a.b" || in.GetExpectedVersion() != 7 || in.GetTraceId() != "t-2" {
		t.Fatalf("路由参数未透传：%+v", in)
	}
	if in.GetOperator() != "gateway/admin:77" {
		t.Fatalf("operator 未由会话渲染：%q", in.GetOperator())
	}
	if in.GetDefinition().GetState() != cronrpc.TaskState_TASK_STATE_UNSPECIFIED {
		t.Fatalf("state 不在此接口生效，却被带给下游：%v", in.GetDefinition().GetState())
	}
	if in.GetDefinition().GetVersion() != 0 || in.GetDefinition().GetTaskId() != 0 {
		t.Fatalf("乐观锁版本/主键由服务端维护，不得由客户端声明：%+v", in.GetDefinition())
	}
	if !resp.Data.Found || resp.Data.Definition.Handler != "h" {
		t.Fatalf("回读定义未投影：%+v", resp.Data)
	}

	// expected_version 是 Update 唯一的并发防线：0/负数都等于放弃乐观锁。
	for _, v := range []int64{0, -1} {
		zero := &cronAdminFake{}
		if _, err := NewCronUpdateTaskLogic(ctx, cronAdminSvc(zero)).
			CronUpdateTask(&types.ParamCronUpdateTask{TaskKey: "a.b", Definition: def, ExpectedVersion: v}); err == nil {
			t.Fatalf("expected_version=%d 被放行", v)
		}
		if zero.calls != 0 {
			t.Fatalf("非法版本号仍调用下游 %d 次", zero.calls)
		}
	}
	// definition.task_key 与路由参数不一致时必须拒绝，而不是任选一个执行。
	conflict := &cronAdminFake{}
	if _, err := NewCronUpdateTaskLogic(ctx, cronAdminSvc(conflict)).
		CronUpdateTask(&types.ParamCronUpdateTask{
			TaskKey: "a.b", ExpectedVersion: 1,
			Definition: withDef(def, func(d *types.CronTaskDefinition) { d.TaskKey = "c.d" }),
		}); err == nil {
		t.Fatal("task_key 冲突被放行")
	}
	if conflict.calls != 0 {
		t.Fatalf("冲突请求仍调用下游 %d 次", conflict.calls)
	}
	// 空白 task_key 既不在路由也不在定义里。
	if _, err := NewCronUpdateTaskLogic(ctx, cronAdminSvc(&cronAdminFake{})).
		CronUpdateTask(&types.ParamCronUpdateTask{TaskKey: "", ExpectedVersion: 1,
			Definition: withDef(def, func(d *types.CronTaskDefinition) { d.TaskKey = "" })}); err == nil {
		t.Fatal("缺 task_key 的更新被放行")
	}
}

func TestCronStateOperationsProjectChangedAndAuditID(t *testing.T) {
	ctx := cronAdminSessionCtx()
	fake := &cronAdminFake{opReply: &cronrpc.TaskOperationReply{
		Definition: cronFixtureDefinitionRPC(), Changed: false, AuditId: 0,
	}}
	calls := map[string]func() (*types.CronTaskOperationResponse, error){
		"pause": func() (*types.CronTaskOperationResponse, error) {
			return NewCronPauseTaskLogic(ctx, cronAdminSvc(fake)).
				CronPauseTask(&types.ParamCronPauseTask{TaskKey: "rights.expire_scan", Reason: "上游发布中",
					ExpectedVersion: 7, IdempotencyKey: "p-1", TraceId: "t-1"})
		},
		"resume": func() (*types.CronTaskOperationResponse, error) {
			return NewCronResumeTaskLogic(ctx, cronAdminSvc(fake)).
				CronResumeTask(&types.ParamCronResumeTask{TaskKey: "rights.expire_scan", IdempotencyKey: "r-1"})
		},
		"disable": func() (*types.CronTaskOperationResponse, error) {
			return NewCronDisableTaskLogic(ctx, cronAdminSvc(fake)).
				CronDisableTask(&types.ParamCronDisableTask{TaskKey: "rights.expire_scan", Reason: "下线",
					IdempotencyKey: "d-1"})
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			resp, err := call()
			if err != nil {
				t.Fatalf("%s 失败: %v", name, err)
			}
			if resp.Data.Changed {
				t.Fatalf("changed=false 的幂等重入被改写成成功变更：%+v", resp.Data)
			}
			if resp.Data.AuditId != 0 {
				t.Fatalf("audit_id 被伪造：%d", resp.Data.AuditId)
			}
			if resp.Data.Definition.State != int32(cronrpc.TaskState_TASK_STATE_PAUSED) ||
				resp.Data.Definition.Version != 7 {
				t.Fatalf("回读定义未投影：%+v", resp.Data.Definition)
			}
		})
	}
	// expected_version 负数非法；0 表示「不校验版本」，是合法值。
	neg := &cronAdminFake{}
	if _, err := NewCronPauseTaskLogic(ctx, cronAdminSvc(neg)).
		CronPauseTask(&types.ParamCronPauseTask{TaskKey: "a.b", Reason: "r", IdempotencyKey: "k",
			ExpectedVersion: -1}); err == nil {
		t.Fatal("负 expected_version 被放行")
	}
	if _, err := NewCronResumeTaskLogic(ctx, cronAdminSvc(neg)).
		CronResumeTask(&types.ParamCronResumeTask{TaskKey: "a.b", IdempotencyKey: "k",
			ExpectedVersion: 0}); err != nil {
		t.Fatalf("expected_version=0 应透传给 cron 判定，实际 %v", err)
	}
}

func TestCronTriggerAndRetryProjectRun(t *testing.T) {
	ctx := cronAdminSessionCtx()
	fake := &cronAdminFake{
		triggerReply: &cronrpc.TriggerTaskReply{Run: cronFixtureRunRPC(), Created: true},
		retryReply:   &cronrpc.RetryRunReply{Run: cronFixtureRunRPC(), Created: false},
	}
	trig, err := NewCronTriggerTaskLogic(ctx, cronAdminSvc(fake)).
		CronTriggerTask(&types.ParamCronTriggerTask{
			TaskKey: "index.rebuild_daily", Params: `{"force":true}`, PlannedAt: 1699999000,
			IdempotencyKey: "t-1", TraceId: "trace-t-1",
		})
	if err != nil {
		t.Fatalf("TriggerTask 失败: %v", err)
	}
	in := fake.triggerReq
	if in.GetParams() != `{"force":true}` || in.GetPlannedAt() != 1699999000 ||
		in.GetIdempotencyKey() != "t-1" || in.GetOperator() != "gateway/admin:77" {
		t.Fatalf("触发入参未透传：%+v", in)
	}
	if !trig.Data.Created || trig.Data.Run.FenceToken != 9 || trig.Data.Run.PlannedAt != 1700000000 {
		t.Fatalf("触发结果投影不完整：%+v", trig.Data)
	}
	if _, err := NewCronTriggerTaskLogic(ctx, cronAdminSvc(fake)).
		CronTriggerTask(&types.ParamCronTriggerTask{TaskKey: "a.b", PlannedAt: -1, IdempotencyKey: "k"}); err == nil {
		t.Fatal("负 planned_at 被放行")
	}

	retry, err := NewCronRetryRunLogic(ctx, cronAdminSvc(fake)).
		CronRetryRun(&types.ParamCronRetryRun{RunId: 501, IdempotencyKey: "rt-1", Reason: "补跑"})
	if err != nil {
		t.Fatalf("RetryRun 失败: %v", err)
	}
	if fake.retryReq.GetRunId() != 501 || fake.retryReq.GetReason() != "补跑" ||
		fake.retryReq.GetIdempotencyKey() != "rt-1" {
		t.Fatalf("重试入参未透传：%+v", fake.retryReq)
	}
	if retry.Data.Created {
		t.Fatalf("created=false 被改写成新建：%+v", retry.Data)
	}
	if retry.Data.Run.Attempt != 2 {
		t.Fatalf("attempt 丢失：%+v", retry.Data.Run)
	}
	if _, err := NewCronRetryRunLogic(ctx, cronAdminSvc(fake)).
		CronRetryRun(&types.ParamCronRetryRun{RunId: 0, IdempotencyKey: "k", Reason: "r"}); err == nil {
		t.Fatal("run_id=0 被放行")
	}
}

func TestCronSaveCheckpointDropsClientVersion(t *testing.T) {
	ctx := cronAdminSessionCtx()
	fake := &cronAdminFake{saveCpReply: &cronrpc.SaveCheckpointReply{
		Checkpoint: cronFixtureCheckpointRPC(), Advanced: true,
	}}
	resp, err := NewCronSaveCheckpointLogic(ctx, cronAdminSvc(fake)).
		CronSaveCheckpoint(&types.ParamCronSaveCheckpoint{
			Checkpoint: types.CronCheckpoint{
				TaskKey: "report.daily_export", ScopeKey: "shard=7", Value: 987654322,
				ValueStr: "idx-report-2026.10", Version: 99, Operator: "someone-else", Ctime: 1, Mtime: 2,
			},
			ExpectedVersion: 4, IdempotencyKey: "cp-1", TraceId: "trace-cp-1",
		})
	if err != nil {
		t.Fatalf("SaveCheckpoint 失败: %v", err)
	}
	in := fake.saveCpReq
	cp := in.GetCheckpoint()
	if cp.GetTaskKey() != "report.daily_export" || cp.GetScopeKey() != "shard=7" ||
		cp.GetValue() != 987654322 || cp.GetValueStr() != "idx-report-2026.10" {
		t.Fatalf("游标载荷未透传：%+v", cp)
	}
	if cp.GetVersion() != 0 {
		t.Fatalf("CAS 版本只能由 expected_version 表达，客户端 version=%d 应被丢弃", cp.GetVersion())
	}
	if cp.GetOperator() != "gateway/admin:77" || in.GetOperator() != "gateway/admin:77" {
		t.Fatalf("operator 被客户端声明污染：%q / %q", cp.GetOperator(), in.GetOperator())
	}
	if in.GetExpectedVersion() != 4 || in.GetIdempotencyKey() != "cp-1" || in.GetTraceId() != "trace-cp-1" {
		t.Fatalf("CAS/幂等上下文未透传：%+v", in)
	}
	if !resp.Data.Advanced || resp.Data.Checkpoint.Version != 4 {
		t.Fatalf("推进结论未投影：%+v", resp.Data)
	}

	// expected_version=0（要求尚不存在）合法；负数与缺 task_key 非法。
	zero := &cronAdminFake{}
	if _, err := NewCronSaveCheckpointLogic(ctx, cronAdminSvc(zero)).
		CronSaveCheckpoint(&types.ParamCronSaveCheckpoint{
			Checkpoint: types.CronCheckpoint{TaskKey: "a.b"}, IdempotencyKey: "k"}); err != nil {
		t.Fatalf("expected_version=0 应合法，实际 %v", err)
	}
	if zero.saveCpReq.GetExpectedVersion() != 0 {
		t.Fatalf("0 被改写为 %d", zero.saveCpReq.GetExpectedVersion())
	}
	if _, err := NewCronSaveCheckpointLogic(ctx, cronAdminSvc(zero)).
		CronSaveCheckpoint(&types.ParamCronSaveCheckpoint{
			Checkpoint: types.CronCheckpoint{TaskKey: "a.b"}, ExpectedVersion: -1, IdempotencyKey: "k"}); err == nil {
		t.Fatal("负 expected_version 被放行")
	}
	if _, err := NewCronSaveCheckpointLogic(ctx, cronAdminSvc(zero)).
		CronSaveCheckpoint(&types.ParamCronSaveCheckpoint{Checkpoint: types.CronCheckpoint{},
			IdempotencyKey: "k"}); err == nil {
		t.Fatal("缺 checkpoint.task_key 被放行")
	}
}

// --- 通用错误路径：客户端未配置 / 请求体缺失 / 下游错误原样上抛 ---

func TestCronRoutesFailClosedWithoutClient(t *testing.T) {
	ctx := cronAdminSessionCtx()
	empty := &svc.ServiceContext{}
	cases := map[string]func() error{
		"cronListTasks": func() error {
			_, err := NewCronListTasksLogic(ctx, empty).CronListTasks(&types.ParamCronListTasks{})
			return err
		},
		"cronGetTask": func() error {
			_, err := NewCronGetTaskLogic(ctx, empty).CronGetTask(&types.ParamCronGetTask{TaskKey: "a.b"})
			return err
		},
		"cronListTaskRuns": func() error {
			_, err := NewCronListTaskRunsLogic(ctx, empty).CronListTaskRuns(&types.ParamCronListTaskRuns{})
			return err
		},
		"cronGetTaskRun": func() error {
			_, err := NewCronGetTaskRunLogic(ctx, empty).CronGetTaskRun(&types.ParamCronGetTaskRun{RunId: 1})
			return err
		},
		"cronListCheckpoints": func() error {
			_, err := NewCronListCheckpointsLogic(ctx, empty).CronListCheckpoints(&types.ParamCronListCheckpoints{})
			return err
		},
		"cronGetCheckpoint": func() error {
			_, err := NewCronGetCheckpointLogic(ctx, empty).CronGetCheckpoint(&types.ParamCronGetCheckpoint{TaskKey: "a.b"})
			return err
		},
		"cronListLeases": func() error {
			_, err := NewCronListLeasesLogic(ctx, empty).CronListLeases(&types.ParamCronListLeases{})
			return err
		},
		"cronGetLease": func() error {
			_, err := NewCronGetLeaseLogic(ctx, empty).CronGetLease(&types.ParamCronGetLease{TaskKey: "a.b"})
			return err
		},
		"cronListTaskAudits": func() error {
			_, err := NewCronListTaskAuditsLogic(ctx, empty).CronListTaskAudits(&types.ParamCronListTaskAudits{})
			return err
		},
		"cronSchedulerHealth": func() error {
			_, err := NewCronSchedulerHealthLogic(ctx, empty).CronSchedulerHealth(&types.ParamCronSchedulerHealth{})
			return err
		},
		"cronRegisterTask": func() error {
			_, err := NewCronRegisterTaskLogic(ctx, empty).CronRegisterTask(&types.ParamCronRegisterTask{})
			return err
		},
		"cronUpdateTask": func() error {
			_, err := NewCronUpdateTaskLogic(ctx, empty).CronUpdateTask(&types.ParamCronUpdateTask{})
			return err
		},
		"cronPauseTask": func() error {
			_, err := NewCronPauseTaskLogic(ctx, empty).CronPauseTask(&types.ParamCronPauseTask{})
			return err
		},
		"cronResumeTask": func() error {
			_, err := NewCronResumeTaskLogic(ctx, empty).CronResumeTask(&types.ParamCronResumeTask{})
			return err
		},
		"cronDisableTask": func() error {
			_, err := NewCronDisableTaskLogic(ctx, empty).CronDisableTask(&types.ParamCronDisableTask{})
			return err
		},
		"cronTriggerTask": func() error {
			_, err := NewCronTriggerTaskLogic(ctx, empty).CronTriggerTask(&types.ParamCronTriggerTask{})
			return err
		},
		"cronRetryRun": func() error {
			_, err := NewCronRetryRunLogic(ctx, empty).CronRetryRun(&types.ParamCronRetryRun{})
			return err
		},
		"cronSaveCheckpoint": func() error {
			_, err := NewCronSaveCheckpointLogic(ctx, empty).CronSaveCheckpoint(&types.ParamCronSaveCheckpoint{})
			return err
		},
	}
	if len(cases) != 18 {
		t.Fatalf("cron 域 logic 应有 18 条，实际 %d", len(cases))
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, errCronServiceNotConfigured) {
				t.Fatalf("未配置 CronRPC 时应返回服务不可用，实际 %v", err)
			}
		})
	}
}

func TestCronRoutesRejectMissingRequestBody(t *testing.T) {
	svcCtx := cronAdminSvc(&cronAdminFake{
		listTasksReply:  &cronrpc.ListTasksReply{},
		registerReply:   &cronrpc.RegisterTaskReply{},
		getTaskReply:    &cronrpc.GetTaskReply{},
		getRunReply:     &cronrpc.GetTaskRunReply{},
		listRunsReply:   &cronrpc.ListTaskRunsReply{},
		listCpReply:     &cronrpc.ListCheckpointsReply{},
		getCpReply:      &cronrpc.GetCheckpointReply{},
		saveCpReply:     &cronrpc.SaveCheckpointReply{},
		listLeasesReply: &cronrpc.ListLeasesReply{},
		getLeaseReply:   &cronrpc.GetLeaseReply{},
		listAuditsReply: &cronrpc.ListTaskAuditsReply{},
		healthReply:     &cronrpc.GetSchedulerHealthReply{},
		updateReply:     &cronrpc.UpdateTaskReply{},
		opReply:         &cronrpc.TaskOperationReply{},
		triggerReply:    &cronrpc.TriggerTaskReply{},
	})
	cases := map[string]func() error{
		"cronListTasks": func() error {
			_, err := NewCronListTasksLogic(context.Background(), svcCtx).CronListTasks(nil)
			return err
		},
		"cronGetTask": func() error {
			_, err := NewCronGetTaskLogic(context.Background(), svcCtx).CronGetTask(nil)
			return err
		},
		"cronListTaskRuns": func() error {
			_, err := NewCronListTaskRunsLogic(context.Background(), svcCtx).CronListTaskRuns(nil)
			return err
		},
		"cronGetTaskRun": func() error {
			_, err := NewCronGetTaskRunLogic(context.Background(), svcCtx).CronGetTaskRun(nil)
			return err
		},
		"cronListCheckpoints": func() error {
			_, err := NewCronListCheckpointsLogic(context.Background(), svcCtx).CronListCheckpoints(nil)
			return err
		},
		"cronGetCheckpoint": func() error {
			_, err := NewCronGetCheckpointLogic(context.Background(), svcCtx).CronGetCheckpoint(nil)
			return err
		},
		"cronListLeases": func() error {
			_, err := NewCronListLeasesLogic(context.Background(), svcCtx).CronListLeases(nil)
			return err
		},
		"cronGetLease": func() error {
			_, err := NewCronGetLeaseLogic(context.Background(), svcCtx).CronGetLease(nil)
			return err
		},
		"cronListTaskAudits": func() error {
			_, err := NewCronListTaskAuditsLogic(context.Background(), svcCtx).CronListTaskAudits(nil)
			return err
		},
		"cronSchedulerHealth": func() error {
			_, err := NewCronSchedulerHealthLogic(context.Background(), svcCtx).CronSchedulerHealth(nil)
			return err
		},
		"cronRegisterTask": func() error {
			_, err := NewCronRegisterTaskLogic(cronAdminSessionCtx(), svcCtx).CronRegisterTask(nil)
			return err
		},
		"cronUpdateTask": func() error {
			_, err := NewCronUpdateTaskLogic(cronAdminSessionCtx(), svcCtx).CronUpdateTask(nil)
			return err
		},
		"cronPauseTask": func() error {
			_, err := NewCronPauseTaskLogic(cronAdminSessionCtx(), svcCtx).CronPauseTask(nil)
			return err
		},
		"cronResumeTask": func() error {
			_, err := NewCronResumeTaskLogic(cronAdminSessionCtx(), svcCtx).CronResumeTask(nil)
			return err
		},
		"cronDisableTask": func() error {
			_, err := NewCronDisableTaskLogic(cronAdminSessionCtx(), svcCtx).CronDisableTask(nil)
			return err
		},
		"cronTriggerTask": func() error {
			_, err := NewCronTriggerTaskLogic(cronAdminSessionCtx(), svcCtx).CronTriggerTask(nil)
			return err
		},
		"cronRetryRun": func() error {
			_, err := NewCronRetryRunLogic(cronAdminSessionCtx(), svcCtx).CronRetryRun(nil)
			return err
		},
		"cronSaveCheckpoint": func() error {
			_, err := NewCronSaveCheckpointLogic(cronAdminSessionCtx(), svcCtx).CronSaveCheckpoint(nil)
			return err
		},
	}
	if len(cases) != 18 {
		t.Fatalf("cron 域 logic 应有 18 条，实际 %d", len(cases))
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, errCronRequestMissing) {
				t.Fatalf("请求体缺失应明确报错，实际 %v", err)
			}
		})
	}
}

func TestCronDownstreamErrorPropagates(t *testing.T) {
	ctx := cronAdminSessionCtx()
	fake := &cronAdminFake{err: errCronFakeDownstream}
	cases := map[string]func() error{
		"list tasks": func() error {
			_, err := NewCronListTasksLogic(ctx, cronAdminSvc(fake)).CronListTasks(&types.ParamCronListTasks{})
			return err
		},
		"get task": func() error {
			_, err := NewCronGetTaskLogic(ctx, cronAdminSvc(fake)).CronGetTask(&types.ParamCronGetTask{TaskKey: "a.b"})
			return err
		},
		"list runs": func() error {
			_, err := NewCronListTaskRunsLogic(ctx, cronAdminSvc(fake)).CronListTaskRuns(&types.ParamCronListTaskRuns{})
			return err
		},
		"get run": func() error {
			_, err := NewCronGetTaskRunLogic(ctx, cronAdminSvc(fake)).CronGetTaskRun(&types.ParamCronGetTaskRun{RunId: 1})
			return err
		},
		"list checkpoints": func() error {
			_, err := NewCronListCheckpointsLogic(ctx, cronAdminSvc(fake)).CronListCheckpoints(&types.ParamCronListCheckpoints{})
			return err
		},
		"get checkpoint": func() error {
			_, err := NewCronGetCheckpointLogic(ctx, cronAdminSvc(fake)).CronGetCheckpoint(&types.ParamCronGetCheckpoint{TaskKey: "a.b"})
			return err
		},
		"list leases": func() error {
			_, err := NewCronListLeasesLogic(ctx, cronAdminSvc(fake)).CronListLeases(&types.ParamCronListLeases{})
			return err
		},
		"get lease": func() error {
			_, err := NewCronGetLeaseLogic(ctx, cronAdminSvc(fake)).CronGetLease(&types.ParamCronGetLease{TaskKey: "a.b"})
			return err
		},
		"list audits": func() error {
			_, err := NewCronListTaskAuditsLogic(ctx, cronAdminSvc(fake)).CronListTaskAudits(&types.ParamCronListTaskAudits{})
			return err
		},
		"health": func() error {
			_, err := NewCronSchedulerHealthLogic(ctx, cronAdminSvc(fake)).CronSchedulerHealth(&types.ParamCronSchedulerHealth{})
			return err
		},
		"register": func() error {
			_, err := NewCronRegisterTaskLogic(ctx, cronAdminSvc(fake)).CronRegisterTask(&types.ParamCronRegisterTask{
				Definition: types.CronTaskDefinition{TaskKey: "a.b", Name: "n", Handler: "h",
					ScheduleType: int32(cronrpc.ScheduleType_SCHEDULE_TYPE_MANUAL)},
				IdempotencyKey: "k",
			})
			return err
		},
		"update": func() error {
			_, err := NewCronUpdateTaskLogic(ctx, cronAdminSvc(fake)).CronUpdateTask(&types.ParamCronUpdateTask{
				TaskKey: "a.b", ExpectedVersion: 1,
				Definition: types.CronTaskDefinition{TaskKey: "a.b", Name: "n", Handler: "h",
					ScheduleType: int32(cronrpc.ScheduleType_SCHEDULE_TYPE_MANUAL)},
			})
			return err
		},
		"pause": func() error {
			_, err := NewCronPauseTaskLogic(ctx, cronAdminSvc(fake)).CronPauseTask(&types.ParamCronPauseTask{
				TaskKey: "a.b", Reason: "r", IdempotencyKey: "k"})
			return err
		},
		"resume": func() error {
			_, err := NewCronResumeTaskLogic(ctx, cronAdminSvc(fake)).CronResumeTask(&types.ParamCronResumeTask{
				TaskKey: "a.b", IdempotencyKey: "k"})
			return err
		},
		"disable": func() error {
			_, err := NewCronDisableTaskLogic(ctx, cronAdminSvc(fake)).CronDisableTask(&types.ParamCronDisableTask{
				TaskKey: "a.b", Reason: "r", IdempotencyKey: "k"})
			return err
		},
		"trigger": func() error {
			_, err := NewCronTriggerTaskLogic(ctx, cronAdminSvc(fake)).CronTriggerTask(&types.ParamCronTriggerTask{
				TaskKey: "a.b", IdempotencyKey: "k"})
			return err
		},
		"retry": func() error {
			_, err := NewCronRetryRunLogic(ctx, cronAdminSvc(fake)).CronRetryRun(&types.ParamCronRetryRun{
				RunId: 1, IdempotencyKey: "k", Reason: "r"})
			return err
		},
		"save checkpoint": func() error {
			_, err := NewCronSaveCheckpointLogic(ctx, cronAdminSvc(fake)).CronSaveCheckpoint(&types.ParamCronSaveCheckpoint{
				Checkpoint: types.CronCheckpoint{TaskKey: "a.b"}, IdempotencyKey: "k"})
			return err
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, errCronFakeDownstream) {
				t.Fatalf("下游错误应原样上抛而不被伪装成成功，实际 %v", err)
			}
		})
	}
}
