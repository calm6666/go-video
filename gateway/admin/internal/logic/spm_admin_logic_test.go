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
	spmrpc "go-video/services/spm/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 spm 的口径（AGENTS.md §5/§7）：
//   - rpc→types 投影逐字段不丢（found/reused/stale/windows_failed/last_error/payload_digest 都在）；
//   - BatchGetMetrics 的 map 摊平成稳定序（map 遍历序随机，不排序会让同一请求两次刷新顺序不同）；
//   - operator 只能由会话渲染成 gateway/admin:<admin_id>，五条受保护入口拿不到会话就 fail-closed；
//   - 写入口的形状门槛（幂等键非空、枚举位不为 UNSPECIFIED、窗口区间不倒置、重算必须显式版本）
//     一律在调用下游之前失败；
//   - 契约里的 0 值哨兵（全部主体 / 不限主体 / 全部指标 / ACTIVE 版本 / 最近闭合窗口 / 当前时间）
//     原样下传，网关不替调用方挑值；
//   - 未配置 SpmRPC 时十五口一律报错，绝不回空榜单、也绝不伪造 found=true。
//
// 「口径版本能否使用」「公式与事件类型是否自洽」「窗口区间多大算过大」「作业该不该受理」
// 「ps 上限 100」全是 services/spm 的领域规则，网关不复算 —— 这里断言的是
// 「入参原样交给下游 + 下游结论原样回传」。打桩方式与 cron/audit/recommend 测试一致：
// 内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库。

var errSpmFakeDownstream = errors.New("spm downstream unavailable")

// spmAdminFake 记录每次调用的入参，并按预置值返回响应或错误。
type spmAdminFake struct {
	spmrpc.SpmClient

	err   error
	calls int

	getMetricReq   *spmrpc.GetMetricReq
	getMetricReply *spmrpc.GetMetricReply
	batchGetReq    *spmrpc.BatchGetMetricsReq
	batchGetReply  *spmrpc.BatchGetMetricsReply
	hotReq         *spmrpc.ListHotSubjectsReq
	hotReply       *spmrpc.ListHotSubjectsReply
	interestReq    *spmrpc.GetUserInterestReq
	interestReply  *spmrpc.GetUserInterestReply
	retentionReq   *spmrpc.GetRetentionReq
	retentionReply *spmrpc.GetRetentionReply

	recomputeReq   *spmrpc.RecomputeMetricsReq
	recomputeReply *spmrpc.RecomputeMetricsReply
	upsertReq      *spmrpc.UpsertMetricDefinitionReq
	upsertReply    *spmrpc.UpsertMetricDefinitionReply
	stateReq       *spmrpc.UpdateMetricDefinitionStateReq
	stateReply     *spmrpc.UpdateMetricDefinitionStateReply
	defGetReq      *spmrpc.GetMetricDefinitionReq
	defGetReply    *spmrpc.GetMetricDefinitionReply
	defListReq     *spmrpc.ListMetricDefinitionsReq
	defListReply   *spmrpc.ListMetricDefinitionsReply

	submitReq   *spmrpc.SubmitAggregationJobReq
	submitReply *spmrpc.SubmitAggregationJobReply
	jobGetReq   *spmrpc.GetAggregationJobReq
	jobGetReply *spmrpc.GetAggregationJobReply
	jobListReq  *spmrpc.ListAggregationJobsReq
	jobListRepl *spmrpc.ListAggregationJobsReply
	consReq     *spmrpc.ListConsumerStateReq
	consReply   *spmrpc.ListConsumerStateReply
	dlReq       *spmrpc.ListDeadLettersReq
	dlReply     *spmrpc.ListDeadLettersReply
}

func (f *spmAdminFake) GetMetric(_ context.Context, in *spmrpc.GetMetricReq,
	_ ...grpc.CallOption) (*spmrpc.GetMetricReply, error) {
	f.calls++
	f.getMetricReq = in
	return f.getMetricReply, f.err
}

func (f *spmAdminFake) BatchGetMetrics(_ context.Context, in *spmrpc.BatchGetMetricsReq,
	_ ...grpc.CallOption) (*spmrpc.BatchGetMetricsReply, error) {
	f.calls++
	f.batchGetReq = in
	return f.batchGetReply, f.err
}

func (f *spmAdminFake) ListHotSubjects(_ context.Context, in *spmrpc.ListHotSubjectsReq,
	_ ...grpc.CallOption) (*spmrpc.ListHotSubjectsReply, error) {
	f.calls++
	f.hotReq = in
	return f.hotReply, f.err
}

func (f *spmAdminFake) GetUserInterest(_ context.Context, in *spmrpc.GetUserInterestReq,
	_ ...grpc.CallOption) (*spmrpc.GetUserInterestReply, error) {
	f.calls++
	f.interestReq = in
	return f.interestReply, f.err
}

func (f *spmAdminFake) GetRetention(_ context.Context, in *spmrpc.GetRetentionReq,
	_ ...grpc.CallOption) (*spmrpc.GetRetentionReply, error) {
	f.calls++
	f.retentionReq = in
	return f.retentionReply, f.err
}

func (f *spmAdminFake) RecomputeMetrics(_ context.Context, in *spmrpc.RecomputeMetricsReq,
	_ ...grpc.CallOption) (*spmrpc.RecomputeMetricsReply, error) {
	f.calls++
	f.recomputeReq = in
	return f.recomputeReply, f.err
}

func (f *spmAdminFake) UpsertMetricDefinition(_ context.Context, in *spmrpc.UpsertMetricDefinitionReq,
	_ ...grpc.CallOption) (*spmrpc.UpsertMetricDefinitionReply, error) {
	f.calls++
	f.upsertReq = in
	return f.upsertReply, f.err
}

func (f *spmAdminFake) UpdateMetricDefinitionState(_ context.Context, in *spmrpc.UpdateMetricDefinitionStateReq,
	_ ...grpc.CallOption) (*spmrpc.UpdateMetricDefinitionStateReply, error) {
	f.calls++
	f.stateReq = in
	return f.stateReply, f.err
}

func (f *spmAdminFake) GetMetricDefinition(_ context.Context, in *spmrpc.GetMetricDefinitionReq,
	_ ...grpc.CallOption) (*spmrpc.GetMetricDefinitionReply, error) {
	f.calls++
	f.defGetReq = in
	return f.defGetReply, f.err
}

func (f *spmAdminFake) ListMetricDefinitions(_ context.Context, in *spmrpc.ListMetricDefinitionsReq,
	_ ...grpc.CallOption) (*spmrpc.ListMetricDefinitionsReply, error) {
	f.calls++
	f.defListReq = in
	return f.defListReply, f.err
}

func (f *spmAdminFake) SubmitAggregationJob(_ context.Context, in *spmrpc.SubmitAggregationJobReq,
	_ ...grpc.CallOption) (*spmrpc.SubmitAggregationJobReply, error) {
	f.calls++
	f.submitReq = in
	return f.submitReply, f.err
}

func (f *spmAdminFake) GetAggregationJob(_ context.Context, in *spmrpc.GetAggregationJobReq,
	_ ...grpc.CallOption) (*spmrpc.GetAggregationJobReply, error) {
	f.calls++
	f.jobGetReq = in
	return f.jobGetReply, f.err
}

func (f *spmAdminFake) ListAggregationJobs(_ context.Context, in *spmrpc.ListAggregationJobsReq,
	_ ...grpc.CallOption) (*spmrpc.ListAggregationJobsReply, error) {
	f.calls++
	f.jobListReq = in
	return f.jobListRepl, f.err
}

func (f *spmAdminFake) ListConsumerState(_ context.Context, in *spmrpc.ListConsumerStateReq,
	_ ...grpc.CallOption) (*spmrpc.ListConsumerStateReply, error) {
	f.calls++
	f.consReq = in
	return f.consReply, f.err
}

func (f *spmAdminFake) ListDeadLetters(_ context.Context, in *spmrpc.ListDeadLettersReq,
	_ ...grpc.CallOption) (*spmrpc.ListDeadLettersReply, error) {
	f.calls++
	f.dlReq = in
	return f.dlReply, f.err
}

// --- 测试夹具 ---

func spmAdminSvc(fake spmrpc.SpmClient) *svc.ServiceContext {
	return &svc.ServiceContext{Spm: fake}
}

// spmAdminSessionCtx 模拟 AdminPermission 中间件已解析出会话身份的请求上下文。
func spmAdminSessionCtx() context.Context {
	return middleware.WithAdmin(context.Background(), middleware.AdminIdentity{
		AdminID: 77,
		Roles:   []string{"data_operator"},
	})
}

// spmNoSessionCtx 是「没挂上会话身份」的请求上下文：受保护入口必须 fail-closed。
func spmNoSessionCtx() context.Context {
	return context.Background()
}

func spmFixturePointRPC() *spmrpc.MetricPoint {
	return &spmrpc.MetricPoint{
		MetricKey:     "完播率",
		MetricVersion: 3,
		SubjectType:   spmrpc.SubjectType_SUBJECT_TYPE_AID,
		SubjectId:     10086,
		WindowType:    spmrpc.WindowType_WINDOW_TYPE_DAY,
		WindowStart:   1700000000,
		Value:         0.6125,
		Numerator:     49,
		Denominator:   80,
		SampleCount:   80,
		EventTime:     1700006400,
	}
}

func spmFixtureDefinitionRPC() *spmrpc.MetricDefinition {
	return &spmrpc.MetricDefinition{
		MetricKey:        "完播率",
		MetricVersion:    3,
		Name:             "完播率（含拖拽回看）",
		Formula:          "sum(有效播放时长)/sum(视频时长)，按 (mid,aid) 去重",
		Unit:             "ratio",
		SupportedWindows: []spmrpc.WindowType{spmrpc.WindowType_WINDOW_TYPE_DAY, spmrpc.WindowType_WINDOW_TYPE_HOUR},
		SourceEventTypes: "playback.heartbeat,behavior.exposure",
		State:            spmrpc.DefinitionState_DEFINITION_STATE_ACTIVE,
		Description:      "v3 引入拖拽回看，v2 的历史窗口不回算",
		CreatedBy:        "gateway/admin:77",
		Ctime:            1690000000,
		Mtime:            1699999999,
	}
}

func spmFixtureJobRPC() *spmrpc.AggregationJob {
	return &spmrpc.AggregationJob{
		JobId:           4001,
		JobType:         spmrpc.JobType_JOB_TYPE_OFFLINE_BACKFILL,
		State:           spmrpc.JobState_JOB_STATE_RUNNING,
		SubjectType:     spmrpc.SubjectType_SUBJECT_TYPE_AID,
		SubjectId:       10086,
		MetricKey:       "完播率",
		MetricVersion:   3,
		WindowType:      spmrpc.WindowType_WINDOW_TYPE_DAY,
		WindowStartFrom: 1699000000,
		WindowStartTo:   1700000000,
		WindowsTotal:    12,
		WindowsDone:     7,
		WindowsFailed:   1,
		RequestId:       "spm-backfill-2026-09-22-01",
		Operator:        "gateway/admin:77",
		Reason:          "故障单 OPS-312：09-18 消费中断回填",
		LastError:       "window 1699400000 sample missing",
		Ctime:           1700000001,
		Mtime:           1700000106,
		FinishedAt:      0,
	}
}

// --- 投影 ---

// TestSpmProjectionKeepsEveryField 逐结构体 DeepEqual：每个字段取互不相同的非零值，
// 漏投影、错列或把未知枚举压成 0 都会让这里失败。
func TestSpmProjectionKeepsEveryField(t *testing.T) {
	if got := spmMetricPointToAPI(spmFixturePointRPC()); got.Value != 0.6125 ||
		got.MetricKey != "完播率" || got.MetricVersion != 3 || got.SubjectType != int32(spmrpc.SubjectType_SUBJECT_TYPE_AID) ||
		got.SubjectId != 10086 || got.WindowType != int32(spmrpc.WindowType_WINDOW_TYPE_DAY) ||
		got.WindowStart != 1700000000 || got.Numerator != 49 || got.Denominator != 80 ||
		got.SampleCount != 80 || got.EventTime != 1700006400 {
		t.Fatalf("指标点投影丢字段：%+v", got)
	}

	def := spmDefinitionToAPI(spmFixtureDefinitionRPC())
	if def.State != int32(spmrpc.DefinitionState_DEFINITION_STATE_ACTIVE) || def.CreatedBy != "gateway/admin:77" ||
		def.Ctime != 1690000000 || def.Mtime != 1699999999 || def.Unit != "ratio" ||
		def.Description == "" || def.Formula == "" || def.SourceEventTypes != "playback.heartbeat,behavior.exposure" {
		t.Fatalf("口径投影丢字段：%+v", def)
	}
	if len(def.SupportedWindows) != 2 || def.SupportedWindows[0] != int32(spmrpc.WindowType_WINDOW_TYPE_DAY) ||
		def.SupportedWindows[1] != int32(spmrpc.WindowType_WINDOW_TYPE_HOUR) {
		t.Fatalf("支持窗口投影不完整：%v", def.SupportedWindows)
	}

	job := spmJobToAPI(spmFixtureJobRPC())
	if job.JobId != 4001 || job.JobType != int32(spmrpc.JobType_JOB_TYPE_OFFLINE_BACKFILL) ||
		job.State != int32(spmrpc.JobState_JOB_STATE_RUNNING) || job.WindowsTotal != 12 ||
		job.WindowsDone != 7 || job.WindowsFailed != 1 || job.RequestId == "" ||
		job.Operator != "gateway/admin:77" || job.Reason == "" || job.LastError == "" ||
		job.Ctime != 1700000001 || job.Mtime != 1700000106 || job.FinishedAt != 0 {
		t.Fatalf("作业投影丢字段（失败窗口数与 last_error 是排障证据）：%+v", job)
	}

	if got := spmInterestsToAPI([]*spmrpc.GetUserInterestReply_Interest{{
		InterestKey: "zone:28", Weight: 0.42, SampleCount: 31, EventTime: 1700000000,
	}}); len(got) != 1 || got[0].InterestKey != "zone:28" || got[0].Weight != 0.42 ||
		got[0].SampleCount != 31 || got[0].EventTime != 1700000000 {
		t.Fatalf("兴趣投影不完整：%+v", got)
	}

	if got := spmRetentionPointsToAPI([]*spmrpc.GetRetentionReply_RetentionPoint{{
		DayOffset: 0, CohortSize: 1000, Retained: 640, Rate: 0.64,
	}}); len(got) != 1 || got[0].DayOffset != 0 || got[0].CohortSize != 1000 ||
		got[0].Retained != 640 || got[0].Rate != 0.64 {
		t.Fatalf("留存投影不完整：%+v", got)
	}

	if got := spmHotSubjectsToAPI([]*spmrpc.ListHotSubjectsReply_HotSubject{{
		SubjectId: 10086, Value: 12.5, Numerator: 49, Denominator: 80, Rank: 3,
	}}); len(got) != 1 || got[0].Rank != 3 || got[0].Value != 12.5 || got[0].SubjectId != 10086 ||
		got[0].Numerator != 49 || got[0].Denominator != 80 {
		t.Fatalf("榜单投影不完整：%+v", got)
	}

	if got := spmConsumerRowsToAPI([]*spmrpc.ListConsumerStateReply_Row{{
		Topic: "spm.behavior.event", State: spmrpc.ConsumerState_CONSUMER_STATE_RETRY, Count: 8,
		OldestCtime: 1699990000, LastMsgOffset: 991, LastEventTime: 1699999000,
	}}); len(got) != 1 || got[0].State != int32(spmrpc.ConsumerState_CONSUMER_STATE_RETRY) ||
		got[0].Count != 8 || got[0].OldestCtime != 1699990000 || got[0].LastMsgOffset != 991 ||
		got[0].LastEventTime != 1699999000 {
		t.Fatalf("消费状态投影不完整：%+v", got)
	}

	if got := spmDeadLettersToAPI([]*spmrpc.ListDeadLettersReply_DeadLetter{{
		Id: 77, EventId: "evt-1", EventType: "playback.heartbeat", Topic: "spm.behavior.event",
		PayloadDigest: "sha256:ab", PayloadPreview: `{"aid":1}`, Reason: "schema_version 不支持",
		State: "open", Ctime: 1700000000,
	}}); len(got) != 1 || got[0].PayloadDigest != "sha256:ab" || got[0].PayloadPreview != `{"aid":1}` ||
		got[0].Reason == "" || got[0].State != "open" || got[0].Ctime != 1700000000 {
		t.Fatalf("死信投影不完整（digest/preview 是死因证据）：%+v", got)
	}
}

// TestSpmProjectionsReturnEmptySliceNotNil 锁住「列表回 [] 而不是 null」：
// 下游给空列表时投影必须是长度 0 的非 nil 切片。
func TestSpmProjectionsReturnEmptySliceNotNil(t *testing.T) {
	cases := map[string]any{
		"points":     spmMetricPointsToAPI(nil),
		"fromMap":    spmMetricPointsFromMap(nil),
		"hot":        spmHotSubjectsToAPI(nil),
		"interests":  spmInterestsToAPI(nil),
		"retention":  spmRetentionPointsToAPI(nil),
		"defs":       spmDefinitionsToAPI(nil),
		"jobs":       spmJobsToAPI(nil),
		"consumer":   spmConsumerRowsToAPI(nil),
		"deadWords":  spmDeadLettersToAPI(nil),
		"windows":    spmWindowTypesToAPI(nil),
		"windowsFor": spmWindowTypesForRPC(nil),
	}
	for name, v := range cases {
		if reflect.ValueOf(v).IsNil() {
			t.Errorf("%s 投影返回 nil，后台会收到 null 而不是 []", name)
		}
	}
	// nil 结构体不 panic：返回零值行，让后台看到「空」而不是 500。
	if got := spmMetricPointToAPI(nil); got.MetricKey != "" {
		t.Errorf("nil 指标点应投影成零值：%+v", got)
	}
	if got := spmDefinitionToAPI(nil); got.State != 0 {
		t.Errorf("nil 口径应投影成零值：%+v", got)
	}
	if got := spmJobToAPI(nil); got.JobId != 0 {
		t.Errorf("nil 作业应投影成零值：%+v", got)
	}
}

// TestSpmMetricPointsFromMapIsStablyOrdered 针对 map 遍历序随机：同一份数据两次投影
// 必须同序，否则后台刷新会误读成「数据变了」。
func TestSpmMetricPointsFromMapIsStablyOrdered(t *testing.T) {
	points := map[string]*spmrpc.MetricPoint{
		"b@v1:20": {MetricKey: "b", MetricVersion: 1, WindowStart: 20},
		"a@v2:10": {MetricKey: "a", MetricVersion: 2, WindowStart: 10},
		"a@v1:30": {MetricKey: "a", MetricVersion: 1, WindowStart: 30},
		"a@v1:10": {MetricKey: "a", MetricVersion: 1, WindowStart: 10},
	}
	var first []types.SpmMetricPoint
	for round := 0; round < 20; round++ {
		got := spmMetricPointsFromMap(points)
		if len(got) != 4 {
			t.Fatalf("应摊平 4 条，实际 %d", len(got))
		}
		if got[0].MetricKey != "a" || got[0].MetricVersion != 1 || got[0].WindowStart != 10 ||
			got[1].WindowStart != 30 || got[2].MetricVersion != 2 || got[3].MetricKey != "b" {
			t.Fatalf("排序键不是 (metric_key, metric_version, window_start)：%+v", got)
		}
		if round == 0 {
			first = got
			continue
		}
		for i := range first {
			if first[i] != got[i] {
				t.Fatalf("第 %d 轮顺序变了：%+v vs %+v", round, first, got)
			}
		}
	}
}

// TestSpmDefinitionForRPCLeavesServerOwnedFieldsEmpty 锁住审计三位：
// created_by/ctime/mtime 既不在表单位也不由网关造，只能由服务按会话与库时钟渲染。
func TestSpmDefinitionForRPCLeavesServerOwnedFieldsEmpty(t *testing.T) {
	got := spmDefinitionForRPC(types.SpmMetricDefinitionInput{
		MetricKey:        "完播率",
		MetricVersion:    3,
		Name:             "完播率",
		Formula:          "sum(a)/sum(b)",
		Unit:             "ratio",
		SupportedWindows: []int32{1, 2},
		SourceEventTypes: "playback.heartbeat",
		State:            1,
		Description:      "为什么需要新版本",
	})
	if got.CreatedBy != "" || got.Ctime != 0 || got.Mtime != 0 {
		t.Fatalf("网关自报经办人或本地造时间等于伪造审计主体：%+v", got)
	}
	if len(got.SupportedWindows) != 2 || got.SupportedWindows[0] != spmrpc.WindowType(1) ||
		got.SupportedWindows[1] != spmrpc.WindowType(2) {
		t.Fatalf("窗口枚举未逐位转换：%+v", got.SupportedWindows)
	}
	if got.State != spmrpc.DefinitionState(1) || got.MetricKey != "完播率" || got.MetricVersion != 3 ||
		got.Formula != "sum(a)/sum(b)" || got.Unit != "ratio" || got.SourceEventTypes != "playback.heartbeat" {
		t.Fatalf("表单元字段未原样下传：%+v", got)
	}
}

// --- 身份与门槛 ---

// TestSpmProtectedRoutesRequireSession 五条受保护入口在没有会话身份时必须拒绝且不打下游。
func TestSpmProtectedRoutesRequireSession(t *testing.T) {
	cases := []struct {
		name string
		call func(ctx context.Context, s *svc.ServiceContext) error
	}{
		{"interestGet", func(ctx context.Context, s *svc.ServiceContext) error {
			_, err := NewSpmUserInterestGetLogic(ctx, s).SpmUserInterestGet(&types.ParamSpmUserInterestGet{Mid: 1})
			return err
		}},
		{"definitionUpsert", func(ctx context.Context, s *svc.ServiceContext) error {
			_, err := NewSpmMetricDefinitionUpsertLogic(ctx, s).SpmMetricDefinitionUpsert(&types.ParamSpmMetricDefinitionUpsert{
				Definition:     types.SpmMetricDefinitionInput{MetricKey: "完播率", Name: "完播率"},
				IdempotencyKey: "k-1",
			})
			return err
		}},
		{"definitionState", func(ctx context.Context, s *svc.ServiceContext) error {
			_, err := NewSpmMetricDefinitionStateLogic(ctx, s).SpmMetricDefinitionState(&types.ParamSpmMetricDefinitionState{
				MetricKey: "完播率", MetricVersion: 2, State: 2, Reason: "上线 v2", IdempotencyKey: "k-1",
			})
			return err
		}},
		{"jobSubmit", func(ctx context.Context, s *svc.ServiceContext) error {
			_, err := NewSpmAggregationJobSubmitLogic(ctx, s).SpmAggregationJobSubmit(&types.ParamSpmAggregationJobSubmit{
				JobType: 2, WindowType: 1, WindowStartFrom: 100, IdempotencyKey: "k-1",
			})
			return err
		}},
		{"metricRecompute", func(ctx context.Context, s *svc.ServiceContext) error {
			_, err := NewSpmMetricRecomputeLogic(ctx, s).SpmMetricRecompute(&types.ParamSpmMetricRecompute{
				SubjectType: 1, SubjectId: 1, MetricKey: "完播率", MetricVersion: 3, WindowType: 1,
				WindowStartFrom: 100, IdempotencyKey: "k-1",
			})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &spmAdminFake{}
			if err := tc.call(spmNoSessionCtx(), spmAdminSvc(fake)); !errors.Is(err, errSpmSessionRequired) {
				t.Fatalf("无会话应返回 errSpmSessionRequired，实际 %v", err)
			}
			if fake.calls != 0 {
				t.Fatalf("无会话却打了下游 %d 次", fake.calls)
			}
			if err := tc.call(spmAdminSessionCtx(), spmAdminSvc(fake)); err != nil {
				t.Fatalf("有会话应通过身份门槛，实际 %v", err)
			}
			if fake.calls != 1 {
				t.Fatalf("应恰好调用下游一次，实际 %d", fake.calls)
			}
		})
	}
}

// TestSpmOperatorIsRenderedFromSessionOnly 四条带 operator 位的入口：值必须是
// gateway/admin:<会话 admin_id>，表单里没有也造不出别人的身份。
func TestSpmOperatorIsRenderedFromSessionOnly(t *testing.T) {
	fake := &spmAdminFake{}
	s := spmAdminSvc(fake)

	if _, err := NewSpmMetricDefinitionUpsertLogic(spmAdminSessionCtx(), s).SpmMetricDefinitionUpsert(
		&types.ParamSpmMetricDefinitionUpsert{
			Definition:     types.SpmMetricDefinitionInput{MetricKey: "完播率", Name: "完播率", MetricVersion: 3},
			IdempotencyKey: "up-1",
		}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSpmMetricDefinitionStateLogic(spmAdminSessionCtx(), s).SpmMetricDefinitionState(
		&types.ParamSpmMetricDefinitionState{
			MetricKey: "完播率", MetricVersion: 3, State: 2, Reason: "上线", IdempotencyKey: "st-1",
		}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSpmAggregationJobSubmitLogic(spmAdminSessionCtx(), s).SpmAggregationJobSubmit(
		&types.ParamSpmAggregationJobSubmit{
			JobType: 2, WindowType: 1, WindowStartFrom: 100, IdempotencyKey: "jb-1",
		}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSpmMetricRecomputeLogic(spmAdminSessionCtx(), s).SpmMetricRecompute(
		&types.ParamSpmMetricRecompute{
			SubjectType: 1, SubjectId: 1, MetricKey: "完播率", MetricVersion: 3, WindowType: 1,
			WindowStartFrom: 100, IdempotencyKey: "rc-1",
		}); err != nil {
		t.Fatal(err)
	}

	for name, got := range map[string]string{
		"upsert":    fake.upsertReq.GetOperator(),
		"state":     fake.stateReq.GetOperator(),
		"jobSubmit": fake.submitReq.GetOperator(),
		"recompute": fake.recomputeReq.GetOperator(),
	} {
		if got != "gateway/admin:77" {
			t.Errorf("%s 的 operator=%q，应为会话渲染的 gateway/admin:77", name, got)
		}
	}
	// 幂等键原样进 request_id（改一个字符等于换一次执行权）。
	for name, got := range map[string]string{
		"upsert":    fake.upsertReq.GetRequestId(),
		"state":     fake.stateReq.GetRequestId(),
		"jobSubmit": fake.submitReq.GetRequestId(),
		"recompute": fake.recomputeReq.GetRequestId(),
	} {
		if got == "" {
			t.Errorf("%s 的 request_id 为空，写入口没有幂等键", name)
		}
	}
}

// TestSpmIdempotencyKeyPassesThroughUnchanged 幂等键只做判空、绝不 trim/改写：
// 悄悄去空格会让运营以为重发的是同一把键。
func TestSpmIdempotencyKeyPassesThroughUnchanged(t *testing.T) {
	fake := &spmAdminFake{}
	const key = "  spm-job-001  "
	if _, err := NewSpmAggregationJobSubmitLogic(spmAdminSessionCtx(), spmAdminSvc(fake)).SpmAggregationJobSubmit(
		&types.ParamSpmAggregationJobSubmit{
			JobType: 2, WindowType: 1, WindowStartFrom: 100, IdempotencyKey: key,
		}); err != nil {
		t.Fatal(err)
	}
	if fake.submitReq.GetRequestId() != key {
		t.Fatalf("幂等键被改写：下传 %q", fake.submitReq.GetRequestId())
	}

	for _, blank := range []string{"", "   ", "\t\n"} {
		empty := &spmAdminFake{}
		_, err := NewSpmAggregationJobSubmitLogic(spmAdminSessionCtx(), spmAdminSvc(empty)).SpmAggregationJobSubmit(
			&types.ParamSpmAggregationJobSubmit{
				JobType: 2, WindowType: 1, WindowStartFrom: 100, IdempotencyKey: blank,
			})
		if err == nil || !strings.Contains(err.Error(), "idempotency_key") {
			t.Errorf("空幂等键 %q 应被点名拒绝，实际 %v", blank, err)
		}
		if empty.calls != 0 {
			t.Errorf("空幂等键却打了下游 %d 次", empty.calls)
		}
	}
}

// TestSpmShapeGatesRejectBeforeCallingDownstream 每个门槛都点名报错且零下游调用。
// 这些是「不可能形状」：UNSPECIFIED 枚举、负数、缺失主体、倒置窗口区间、空主键。
func TestSpmShapeGatesRejectBeforeCallingDownstream(t *testing.T) {
	cases := []struct {
		name  string
		field string
		call  func(s *svc.ServiceContext) error
	}{
		{"metricGetZeroSubjectType", "subject_type", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricGetLogic(context.Background(), s).SpmMetricGet(&types.ParamSpmMetricGet{
				SubjectType: 0, SubjectId: 1, MetricKey: "完播率", WindowType: 1,
			})
			return err
		}},
		{"metricGetNegativeWindowStart", "window_start", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricGetLogic(context.Background(), s).SpmMetricGet(&types.ParamSpmMetricGet{
				SubjectType: 1, SubjectId: 1, MetricKey: "完播率", WindowType: 1, WindowStart: -1,
			})
			return err
		}},
		{"metricGetEmptyMetricKey", "metric_key", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricGetLogic(context.Background(), s).SpmMetricGet(&types.ParamSpmMetricGet{
				SubjectType: 1, SubjectId: 1, MetricKey: "  ", WindowType: 1,
			})
			return err
		}},
		{"batchGetZeroWindowType", "window_type", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricBatchGetLogic(context.Background(), s).SpmMetricBatchGet(&types.ParamSpmMetricBatchGet{
				SubjectType: 1, SubjectId: 1, WindowType: 0,
				Keys: []types.SpmMetricKey{{MetricKey: "完播率"}},
			})
			return err
		}},
		{"batchGetKeyMissingMetricKey", "keys.metric_key", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricBatchGetLogic(context.Background(), s).SpmMetricBatchGet(&types.ParamSpmMetricBatchGet{
				SubjectType: 1, SubjectId: 1, WindowType: 1,
				Keys: []types.SpmMetricKey{{MetricKey: ""}},
			})
			return err
		}},
		{"batchGetNegativeWindowCount", "window_count", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricBatchGetLogic(context.Background(), s).SpmMetricBatchGet(&types.ParamSpmMetricBatchGet{
				SubjectType: 1, SubjectId: 1, WindowType: 1, WindowCount: -2,
				Keys: []types.SpmMetricKey{{MetricKey: "完播率"}},
			})
			return err
		}},
		{"hotListNegativePn", "pn", func(s *svc.ServiceContext) error {
			_, err := NewSpmHotSubjectListLogic(context.Background(), s).SpmHotSubjectList(&types.ParamSpmHotSubjectList{
				SubjectType: 1, MetricKey: "完播率", WindowType: 1, Pn: -1,
			})
			return err
		}},
		{"hotListNegativePs", "ps", func(s *svc.ServiceContext) error {
			_, err := NewSpmHotSubjectListLogic(context.Background(), s).SpmHotSubjectList(&types.ParamSpmHotSubjectList{
				SubjectType: 1, MetricKey: "完播率", WindowType: 1, Ps: -1,
			})
			return err
		}},
		{"hotListNegativeZone", "zone_id", func(s *svc.ServiceContext) error {
			_, err := NewSpmHotSubjectListLogic(context.Background(), s).SpmHotSubjectList(&types.ParamSpmHotSubjectList{
				SubjectType: 1, MetricKey: "完播率", WindowType: 1, ZoneId: -1,
			})
			return err
		}},
		{"retentionZeroCohortType", "cohort_type", func(s *svc.ServiceContext) error {
			_, err := NewSpmRetentionGetLogic(context.Background(), s).SpmRetentionGet(&types.ParamSpmRetentionGet{
				CohortType: 0, CohortDate: 1700000000,
			})
			return err
		}},
		{"interestZeroMid", "mid", func(s *svc.ServiceContext) error {
			_, err := NewSpmUserInterestGetLogic(spmAdminSessionCtx(), s).SpmUserInterestGet(&types.ParamSpmUserInterestGet{})
			return err
		}},
		{"interestNegativeTopN", "top_n", func(s *svc.ServiceContext) error {
			_, err := NewSpmUserInterestGetLogic(spmAdminSessionCtx(), s).SpmUserInterestGet(&types.ParamSpmUserInterestGet{
				Mid: 1, TopN: -1,
			})
			return err
		}},
		{"jobGetNoSubject", "job_id or request_id", func(s *svc.ServiceContext) error {
			_, err := NewSpmAggregationJobGetLogic(context.Background(), s).SpmAggregationJobGet(&types.ParamSpmAggregationJobGet{})
			return err
		}},
		{"jobGetBlankRequestId", "job_id or request_id", func(s *svc.ServiceContext) error {
			_, err := NewSpmAggregationJobGetLogic(context.Background(), s).SpmAggregationJobGet(
				&types.ParamSpmAggregationJobGet{RequestId: " \n"})
			return err
		}},
		{"definitionStateZeroVersion", "metric_version", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricDefinitionStateLogic(spmAdminSessionCtx(), s).SpmMetricDefinitionState(
				&types.ParamSpmMetricDefinitionState{
					MetricKey: "完播率", MetricVersion: 0, State: 2, Reason: "上线", IdempotencyKey: "k",
				})
			return err
		}},
		{"definitionStateZeroTarget", "state", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricDefinitionStateLogic(spmAdminSessionCtx(), s).SpmMetricDefinitionState(
				&types.ParamSpmMetricDefinitionState{
					MetricKey: "完播率", MetricVersion: 2, State: 0, Reason: "上线", IdempotencyKey: "k",
				})
			return err
		}},
		{"definitionStateEmptyReason", "reason", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricDefinitionStateLogic(spmAdminSessionCtx(), s).SpmMetricDefinitionState(
				&types.ParamSpmMetricDefinitionState{
					MetricKey: "完播率", MetricVersion: 2, State: 2, Reason: "", IdempotencyKey: "k",
				})
			return err
		}},
		{"definitionUpsertEmptyName", "definition.name", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricDefinitionUpsertLogic(spmAdminSessionCtx(), s).SpmMetricDefinitionUpsert(
				&types.ParamSpmMetricDefinitionUpsert{
					Definition:     types.SpmMetricDefinitionInput{MetricKey: "完播率", Name: ""},
					IdempotencyKey: "k",
				})
			return err
		}},
		{"jobSubmitZeroType", "job_type", func(s *svc.ServiceContext) error {
			_, err := NewSpmAggregationJobSubmitLogic(spmAdminSessionCtx(), s).SpmAggregationJobSubmit(
				&types.ParamSpmAggregationJobSubmit{
					JobType: 0, WindowType: 1, WindowStartFrom: 100, IdempotencyKey: "k",
				})
			return err
		}},
		{"jobSubmitWindowReversed", "window_start_from", func(s *svc.ServiceContext) error {
			_, err := NewSpmAggregationJobSubmitLogic(spmAdminSessionCtx(), s).SpmAggregationJobSubmit(
				&types.ParamSpmAggregationJobSubmit{
					JobType: 2, WindowType: 1, WindowStartFrom: 200, WindowStartTo: 100, IdempotencyKey: "k",
				})
			return err
		}},
		{"recomputeZeroVersion", "metric_version", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricRecomputeLogic(spmAdminSessionCtx(), s).SpmMetricRecompute(
				&types.ParamSpmMetricRecompute{
					SubjectType: 1, SubjectId: 1, MetricKey: "完播率", MetricVersion: 0, WindowType: 1,
					WindowStartFrom: 100, IdempotencyKey: "k",
				})
			return err
		}},
		{"recomputeZeroSubjectId", "subject_id", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricRecomputeLogic(spmAdminSessionCtx(), s).SpmMetricRecompute(
				&types.ParamSpmMetricRecompute{
					SubjectType: 1, SubjectId: 0, MetricKey: "完播率", MetricVersion: 3, WindowType: 1,
					WindowStartFrom: 100, IdempotencyKey: "k",
				})
			return err
		}},
		{"recomputeEmptyMetricKey", "metric_key", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricRecomputeLogic(spmAdminSessionCtx(), s).SpmMetricRecompute(
				&types.ParamSpmMetricRecompute{
					SubjectType: 1, SubjectId: 1, MetricKey: "", MetricVersion: 3, WindowType: 1,
					WindowStartFrom: 100, IdempotencyKey: "k",
				})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &spmAdminFake{}
			err := tc.call(spmAdminSvc(fake))
			if err == nil {
				t.Fatalf("%s=%q 形状非法却被放行", tc.name, tc.field)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("错误未点名字段 %s：%v", tc.field, err)
			}
			if fake.calls != 0 {
				t.Errorf("门槛未拦住，下游被打了 %d 次", fake.calls)
			}
		})
	}
}

// TestSpmZeroSentinelsPassThroughVerbatim 契约里合法的 0 值哨兵必须原样下传：
// 网关替调用方「补一个具体值」等于凭空圈定一个没人要的范围。
func TestSpmZeroSentinelsPassThroughVerbatim(t *testing.T) {
	fake := &spmAdminFake{}
	if _, err := NewSpmAggregationJobSubmitLogic(spmAdminSessionCtx(), spmAdminSvc(fake)).SpmAggregationJobSubmit(
		&types.ParamSpmAggregationJobSubmit{
			JobType: 1, WindowType: 3, IdempotencyKey: "jb-2", Reason: "  ", TraceId: "t-1",
		}); err != nil {
		t.Fatal(err)
	}
	in := fake.submitReq
	if in.GetSubjectType() != spmrpc.SubjectType(0) || in.GetSubjectId() != 0 ||
		in.GetMetricKey() != "" || in.GetMetricVersion() != 0 ||
		in.GetWindowStartFrom() != 0 || in.GetWindowStartTo() != 0 {
		t.Fatalf("0 值哨兵被改写：%+v", in)
	}
	// reason 是可选留痕位：空串原样下传，网关不编一句「后台手工触发」。
	if in.GetReason() != "  " {
		t.Fatalf("可选 reason 被改写为 %q", in.GetReason())
	}

	// 读口的 metric_version=0 / window_start=0 同样是哨兵（ACTIVE 版本 / 最近闭合窗口）。
	read := &spmAdminFake{}
	if _, err := NewSpmMetricGetLogic(context.Background(), spmAdminSvc(read)).SpmMetricGet(
		&types.ParamSpmMetricGet{SubjectType: 1, SubjectId: 1, MetricKey: "完播率", WindowType: 1}); err != nil {
		t.Fatal(err)
	}
	if read.getMetricReq.GetMetricVersion() != 0 || read.getMetricReq.GetWindowStart() != 0 {
		t.Fatalf("读口的 0 哨兵被改写：%+v", read.getMetricReq)
	}
}

// TestSpmDefinitionStateAndUpsertVerbatimFields 逐字段核对写入口下传值（不被夹取或换默认）。
func TestSpmDefinitionStateAndUpsertVerbatimFields(t *testing.T) {
	fake := &spmAdminFake{}
	s := spmAdminSvc(fake)
	if _, err := NewSpmMetricDefinitionStateLogic(spmAdminSessionCtx(), s).SpmMetricDefinitionState(
		&types.ParamSpmMetricDefinitionState{
			MetricKey: "完播率@v3", MetricVersion: 3, State: 3, Reason: "口径变更单 OPS-9", IdempotencyKey: " st-9 ",
		}); err != nil {
		t.Fatal(err)
	}
	if fake.stateReq.GetMetricKey() != "完播率@v3" || fake.stateReq.GetMetricVersion() != 3 ||
		fake.stateReq.GetState() != spmrpc.DefinitionState(3) || fake.stateReq.GetReason() != "口径变更单 OPS-9" ||
		fake.stateReq.GetRequestId() != " st-9 " {
		t.Fatalf("状态迁移入参未原样下传：%+v", fake.stateReq)
	}

	if _, err := NewSpmMetricDefinitionUpsertLogic(spmAdminSessionCtx(), s).SpmMetricDefinitionUpsert(
		&types.ParamSpmMetricDefinitionUpsert{
			Definition: types.SpmMetricDefinitionInput{
				MetricKey: "完播率", MetricVersion: 0, Name: "n", Formula: "f", Unit: "ratio",
				SupportedWindows: []int32{0, 3}, SourceEventTypes: "a,b", State: 0, Description: "d",
			},
			IdempotencyKey: "up-2",
		}); err != nil {
		t.Fatal(err)
	}
	// metric_version=0 与 state=0 在登记表单里是「由服务落 DRAFT / 服务拒」的合法哨兵，
	// 网关只挡负数，不提前替服务挑版本。
	d := fake.upsertReq.GetDefinition()
	if d.GetMetricVersion() != 0 || d.GetState() != spmrpc.DefinitionState(0) ||
		d.GetCreatedBy() != "" || d.GetCtime() != 0 || d.GetMtime() != 0 ||
		d.GetDescription() != "d" || d.GetUnit() != "ratio" || len(d.GetSupportedWindows()) != 2 {
		t.Fatalf("口径登记入参被改写或补值：%+v", d)
	}
}

// --- 结论透传与错误 ---

// TestSpmNotFoundAndFlagsAreNotFolded found=false / reused=true / stale=true 都是服务的结论，
// 网关不把它们折叠成「空结果」或「新作业」。
func TestSpmNotFoundAndFlagsAreNotFolded(t *testing.T) {
	fake := &spmAdminFake{
		getMetricReply: &spmrpc.GetMetricReply{Found: false, Point: &spmrpc.MetricPoint{Value: 0}},
		jobGetReply:    &spmrpc.GetAggregationJobReply{Found: false},
		defGetReply:    &spmrpc.GetMetricDefinitionReply{Found: false},
		interestReply:  &spmrpc.GetUserInterestReply{Stale: true, MetricVersion: 4},
		submitReply:    &spmrpc.SubmitAggregationJobReply{JobId: 4001, Reused: true, Job: spmFixtureJobRPC()},
		recomputeReply: &spmrpc.RecomputeMetricsReply{JobId: 4002, Reused: true, WindowsPlanned: 0},
		stateReply:     &spmrpc.UpdateMetricDefinitionStateReply{Reused: true, Definition: spmFixtureDefinitionRPC()},
		upsertReply:    &spmrpc.UpsertMetricDefinitionReply{Created: false, Reused: true, Definition: spmFixtureDefinitionRPC()},
	}
	s := spmAdminSvc(fake)

	metric, err := NewSpmMetricGetLogic(context.Background(), s).SpmMetricGet(&types.ParamSpmMetricGet{
		SubjectType: 1, SubjectId: 1, MetricKey: "完播率", WindowType: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if metric.Code != 0 || metric.Message != "ok" || metric.TTL != 0 {
		t.Fatalf("信封不符：%+v", metric)
	}
	if metric.Data.Found {
		t.Error("found=false 被翻成 true：空窗口会被读成「这个视频完播率是 0」")
	}

	job, err := NewSpmAggregationJobGetLogic(context.Background(), s).SpmAggregationJobGet(
		&types.ParamSpmAggregationJobGet{JobId: 4001})
	if err != nil {
		t.Fatal(err)
	}
	if job.Data.Found || job.Data.Job.JobId != 0 {
		t.Fatalf("作业 found=false 被折叠成一条假作业：%+v", job.Data)
	}
	// request_id 二选一命中时原样透传（幂等回放用）。
	if _, err := NewSpmAggregationJobGetLogic(context.Background(), s).SpmAggregationJobGet(
		&types.ParamSpmAggregationJobGet{RequestId: "  jb-2"}); err != nil {
		t.Fatal(err)
	}
	if fake.jobGetReq.GetRequestId() != "  jb-2" || fake.jobGetReq.GetJobId() != 0 {
		t.Fatalf("request_id 被 trim 或 job_id 被补值：%+v", fake.jobGetReq)
	}

	interest, err := NewSpmUserInterestGetLogic(spmAdminSessionCtx(), s).SpmUserInterestGet(
		&types.ParamSpmUserInterestGet{Mid: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !interest.Data.Stale || interest.Data.MetricVersion != 4 {
		t.Fatalf("stale 结论丢失：%+v", interest.Data)
	}

	submit, err := NewSpmAggregationJobSubmitLogic(spmAdminSessionCtx(), s).SpmAggregationJobSubmit(
		&types.ParamSpmAggregationJobSubmit{JobType: 2, WindowType: 1, WindowStartFrom: 100, IdempotencyKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if !submit.Data.Reused || submit.Data.Job.JobId != 4001 || submit.Data.Job.WindowsFailed != 1 {
		t.Fatalf("reused/作业详情未原样回：%+v", submit.Data)
	}

	recompute, err := NewSpmMetricRecomputeLogic(spmAdminSessionCtx(), s).SpmMetricRecompute(
		&types.ParamSpmMetricRecompute{
			SubjectType: 1, SubjectId: 1, MetricKey: "完播率", MetricVersion: 3, WindowType: 1,
			WindowStartFrom: 100, IdempotencyKey: "k",
		})
	if err != nil {
		t.Fatal(err)
	}
	if !recompute.Data.Reused || recompute.Data.JobId != 4002 || recompute.Data.WindowsPlanned != 0 {
		t.Fatalf("重算结论被改写：%+v", recompute.Data)
	}
}

// TestSpmDownstreamErrorPropagatesVerbatim 下游错误原样上抛，不改写成成功也不空转成 []。
func TestSpmDownstreamErrorPropagatesVerbatim(t *testing.T) {
	fake := &spmAdminFake{err: errSpmFakeDownstream}
	s := spmAdminSvc(fake)
	if _, err := NewSpmMetricDefinitionListLogic(context.Background(), s).SpmMetricDefinitionList(
		&types.ParamSpmMetricDefinitionList{}); !errors.Is(err, errSpmFakeDownstream) {
		t.Fatalf("口径列表应透传下游错误，实际 %v", err)
	}
	if _, err := NewSpmHotSubjectListLogic(context.Background(), s).SpmHotSubjectList(
		&types.ParamSpmHotSubjectList{SubjectType: 1, MetricKey: "完播率", WindowType: 1}); !errors.Is(err, errSpmFakeDownstream) {
		t.Fatalf("榜单应透传下游错误，实际 %v", err)
	}
	if _, err := NewSpmAggregationJobSubmitLogic(spmAdminSessionCtx(), s).SpmAggregationJobSubmit(
		&types.ParamSpmAggregationJobSubmit{JobType: 2, WindowType: 1, IdempotencyKey: "k"}); !errors.Is(err, errSpmFakeDownstream) {
		t.Fatalf("作业提交应透传下游错误，实际 %v", err)
	}
	if fake.calls != 3 {
		t.Fatalf("应各打下游一次，实际 %d", fake.calls)
	}
}

// TestSpmEveryRouteFailsLoudlyWithoutClient 未配置 SpmRPC 时十五口一律报错：
// 不退化成空榜单（那会把「下游没接」读成「这条内容没人看」）。
func TestSpmEveryRouteFailsLoudlyWithoutClient(t *testing.T) {
	empty := &svc.ServiceContext{}
	cases := []struct {
		name string
		call func(s *svc.ServiceContext) error
	}{
		{"metricGet", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricGetLogic(context.Background(), s).SpmMetricGet(&types.ParamSpmMetricGet{
				SubjectType: 1, SubjectId: 1, MetricKey: "k", WindowType: 1})
			return err
		}},
		{"metricBatchGet", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricBatchGetLogic(context.Background(), s).SpmMetricBatchGet(&types.ParamSpmMetricBatchGet{
				SubjectType: 1, SubjectId: 1, WindowType: 1})
			return err
		}},
		{"hotSubjectList", func(s *svc.ServiceContext) error {
			_, err := NewSpmHotSubjectListLogic(context.Background(), s).SpmHotSubjectList(&types.ParamSpmHotSubjectList{
				SubjectType: 1, MetricKey: "k", WindowType: 1})
			return err
		}},
		{"retentionGet", func(s *svc.ServiceContext) error {
			_, err := NewSpmRetentionGetLogic(context.Background(), s).SpmRetentionGet(&types.ParamSpmRetentionGet{
				CohortType: 1})
			return err
		}},
		{"definitionGet", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricDefinitionGetLogic(context.Background(), s).SpmMetricDefinitionGet(
				&types.ParamSpmMetricDefinitionGet{MetricKey: "k"})
			return err
		}},
		{"definitionList", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricDefinitionListLogic(context.Background(), s).SpmMetricDefinitionList(
				&types.ParamSpmMetricDefinitionList{})
			return err
		}},
		{"jobGet", func(s *svc.ServiceContext) error {
			_, err := NewSpmAggregationJobGetLogic(context.Background(), s).SpmAggregationJobGet(
				&types.ParamSpmAggregationJobGet{JobId: 1})
			return err
		}},
		{"jobList", func(s *svc.ServiceContext) error {
			_, err := NewSpmAggregationJobListLogic(context.Background(), s).SpmAggregationJobList(
				&types.ParamSpmAggregationJobList{})
			return err
		}},
		{"consumerStateList", func(s *svc.ServiceContext) error {
			_, err := NewSpmConsumerStateListLogic(context.Background(), s).SpmConsumerStateList(
				&types.ParamSpmConsumerStateList{})
			return err
		}},
		{"deadLetterList", func(s *svc.ServiceContext) error {
			_, err := NewSpmDeadLetterListLogic(context.Background(), s).SpmDeadLetterList(
				&types.ParamSpmDeadLetterList{})
			return err
		}},
		{"interestGet", func(s *svc.ServiceContext) error {
			_, err := NewSpmUserInterestGetLogic(spmAdminSessionCtx(), s).SpmUserInterestGet(
				&types.ParamSpmUserInterestGet{Mid: 1})
			return err
		}},
		{"definitionUpsert", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricDefinitionUpsertLogic(spmAdminSessionCtx(), s).SpmMetricDefinitionUpsert(
				&types.ParamSpmMetricDefinitionUpsert{
					Definition:     types.SpmMetricDefinitionInput{MetricKey: "k", Name: "n"},
					IdempotencyKey: "k"})
			return err
		}},
		{"definitionState", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricDefinitionStateLogic(spmAdminSessionCtx(), s).SpmMetricDefinitionState(
				&types.ParamSpmMetricDefinitionState{
					MetricKey: "k", MetricVersion: 1, State: 2, Reason: "r", IdempotencyKey: "k"})
			return err
		}},
		{"jobSubmit", func(s *svc.ServiceContext) error {
			_, err := NewSpmAggregationJobSubmitLogic(spmAdminSessionCtx(), s).SpmAggregationJobSubmit(
				&types.ParamSpmAggregationJobSubmit{JobType: 1, WindowType: 1, IdempotencyKey: "k"})
			return err
		}},
		{"metricRecompute", func(s *svc.ServiceContext) error {
			_, err := NewSpmMetricRecomputeLogic(spmAdminSessionCtx(), s).SpmMetricRecompute(
				&types.ParamSpmMetricRecompute{
					SubjectType: 1, SubjectId: 1, MetricKey: "k", MetricVersion: 1, WindowType: 1, IdempotencyKey: "k"})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(empty); !errors.Is(err, errSpmServiceNotConfigured) {
				t.Fatalf("未配置下游应返回 errSpmServiceNotConfigured，实际 %v", err)
			}
		})
	}
}

// TestSpmReadRoutesDoNotRequireSession 十处只读不挂判定也不取会话身份：
// 后台列表页每次刷新都会打一次 RPC，读口拿不到会话也必须能读。
func TestSpmReadRoutesDoNotRequireSession(t *testing.T) {
	fake := &spmAdminFake{
		hotReply:       &spmrpc.ListHotSubjectsReply{Total: 1, WindowStart: 1700000000, MetricVersion: 3},
		defListReply:   &spmrpc.ListMetricDefinitionsReply{Total: 1},
		jobListRepl:    &spmrpc.ListAggregationJobsReply{Total: 1},
		consReply:      &spmrpc.ListConsumerStateReply{Total: 1},
		dlReply:        &spmrpc.ListDeadLettersReply{Total: 1},
		retentionReply: &spmrpc.GetRetentionReply{MetricVersion: 3},
		batchGetReply:  &spmrpc.BatchGetMetricsReply{},
		defGetReply:    &spmrpc.GetMetricDefinitionReply{Found: true, Definition: spmFixtureDefinitionRPC()},
	}
	noSession := spmNoSessionCtx()
	if _, err := NewSpmHotSubjectListLogic(noSession, spmAdminSvc(fake)).SpmHotSubjectList(
		&types.ParamSpmHotSubjectList{SubjectType: 1, MetricKey: "完播率", WindowType: 1}); err != nil {
		t.Fatalf("榜单读取不该要求会话身份：%v", err)
	}
	if _, err := NewSpmMetricDefinitionListLogic(noSession, spmAdminSvc(fake)).SpmMetricDefinitionList(
		&types.ParamSpmMetricDefinitionList{Pn: 1, Ps: 20}); err != nil {
		t.Fatalf("口径列表读取不该要求会话身份：%v", err)
	}
	if _, err := NewSpmAggregationJobListLogic(noSession, spmAdminSvc(fake)).SpmAggregationJobList(
		&types.ParamSpmAggregationJobList{}); err != nil {
		t.Fatalf("作业列表读取不该要求会话身份：%v", err)
	}
	if _, err := NewSpmConsumerStateListLogic(noSession, spmAdminSvc(fake)).SpmConsumerStateList(
		&types.ParamSpmConsumerStateList{}); err != nil {
		t.Fatalf("消费状态读取不该要求会话身份：%v", err)
	}
	if _, err := NewSpmDeadLetterListLogic(noSession, spmAdminSvc(fake)).SpmDeadLetterList(
		&types.ParamSpmDeadLetterList{}); err != nil {
		t.Fatalf("死信读取不该要求会话身份：%v", err)
	}
	if _, err := NewSpmRetentionGetLogic(noSession, spmAdminSvc(fake)).SpmRetentionGet(
		&types.ParamSpmRetentionGet{CohortType: 1, CohortDate: 1700000000}); err != nil {
		t.Fatalf("留存读取不该要求会话身份：%v", err)
	}
	if _, err := NewSpmMetricBatchGetLogic(noSession, spmAdminSvc(fake)).SpmMetricBatchGet(
		&types.ParamSpmMetricBatchGet{SubjectType: 1, SubjectId: 1, WindowType: 1,
			Keys: []types.SpmMetricKey{{MetricKey: "完播率"}, {MetricKey: "互动率", MetricVersion: 2}}}); err != nil {
		t.Fatalf("批量读不该要求会话身份：%v", err)
	}
	if _, err := NewSpmMetricDefinitionGetLogic(noSession, spmAdminSvc(fake)).SpmMetricDefinitionGet(
		&types.ParamSpmMetricDefinitionGet{MetricKey: "完播率"}); err != nil {
		t.Fatalf("口径单读不该要求会话身份：%v", err)
	}
	if fake.defGetReq.GetMetricVersion() != 0 {
		t.Fatalf("未给版本时应下传 0（ACTIVE 哨兵），实际 %d", fake.defGetReq.GetMetricVersion())
	}
	if len(fake.batchGetReq.GetKeys()) != 2 {
		t.Fatalf("keys 未逐条下传：%+v", fake.batchGetReq.GetKeys())
	}
	if fake.jobListReq.GetSince() != 0 || fake.jobListReq.GetJobType() != spmrpc.JobType(0) {
		t.Fatalf("作业列表的「不限」哨兵被改写：%+v", fake.jobListReq)
	}
}

// TestSpmListRoutesReturnEmptySliceNotNil 下游回空列表时后台必须收到 []（长度 0 非 nil）。
func TestSpmListRoutesReturnEmptySliceNotNil(t *testing.T) {
	fake := &spmAdminFake{
		hotReply:       &spmrpc.ListHotSubjectsReply{},
		defListReply:   &spmrpc.ListMetricDefinitionsReply{},
		jobListRepl:    &spmrpc.ListAggregationJobsReply{},
		consReply:      &spmrpc.ListConsumerStateReply{},
		dlReply:        &spmrpc.ListDeadLettersReply{},
		retentionReply: &spmrpc.GetRetentionReply{},
	}
	ctx := context.Background()
	s := spmAdminSvc(fake)

	hot, err := NewSpmHotSubjectListLogic(ctx, s).SpmHotSubjectList(&types.ParamSpmHotSubjectList{
		SubjectType: 1, MetricKey: "完播率", WindowType: 1})
	if err != nil {
		t.Fatal(err)
	}
	if hot.Data.Subjects == nil || len(hot.Data.Subjects) != 0 {
		t.Fatalf("空榜单应回 []，实际 %#v", hot.Data.Subjects)
	}
	if hot.Data.WindowStart != 0 || hot.Data.MetricVersion != 0 {
		t.Fatalf("服务给出的实际窗口/版本未回传：%+v", hot.Data)
	}
	defs, err := NewSpmMetricDefinitionListLogic(ctx, s).SpmMetricDefinitionList(&types.ParamSpmMetricDefinitionList{})
	if err != nil {
		t.Fatal(err)
	}
	if defs.Data.Definitions == nil {
		t.Fatal("空口径列表应回 []")
	}
	jobs, err := NewSpmAggregationJobListLogic(ctx, s).SpmAggregationJobList(&types.ParamSpmAggregationJobList{})
	if err != nil {
		t.Fatal(err)
	}
	if jobs.Data.Jobs == nil {
		t.Fatal("空作业列表应回 []")
	}
	cons, err := NewSpmConsumerStateListLogic(ctx, s).SpmConsumerStateList(&types.ParamSpmConsumerStateList{})
	if err != nil {
		t.Fatal(err)
	}
	if cons.Data.Rows == nil {
		t.Fatal("空消费状态应回 []")
	}
	dl, err := NewSpmDeadLetterListLogic(ctx, s).SpmDeadLetterList(&types.ParamSpmDeadLetterList{})
	if err != nil {
		t.Fatal(err)
	}
	if dl.Data.Items == nil {
		t.Fatal("空死信应回 []")
	}
	ret, err := NewSpmRetentionGetLogic(ctx, s).SpmRetentionGet(&types.ParamSpmRetentionGet{CohortType: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ret.Data.Points == nil {
		t.Fatal("空留存曲线应回 []")
	}
	batch, err := NewSpmMetricBatchGetLogic(ctx, s).SpmMetricBatchGet(&types.ParamSpmMetricBatchGet{
		SubjectType: 1, SubjectId: 1, WindowType: 1, Keys: []types.SpmMetricKey{{MetricKey: "完播率"}}})
	if err != nil {
		t.Fatal(err)
	}
	if batch.Data.Points == nil {
		t.Fatal("空批量读应回 []")
	}
}
