package logic

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	collectorrpc "go-video/services/event-collector/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 event-collector 的口径，与 recommend / live-media 测试同一套边界：
//
//  1. rpc→types **逐字段不丢**：批次的七类计数（accepted/duplicated/rejected/sampled_out/
//     dispatched/dead + total）、事件的 delivery_* 五列、死信的 reason/attempts/state、
//     策略的每一位数值与 operator/ctime/mtime——它们全是「这批为什么被采掉、这条到底出去了没有」
//     的证据，裁掉一条就等于让后台靠猜；
//  2. 入参**原样交给下游**：batch_id/event_id/topic/cursor/idempotency_key 只 TrimSpace 判空、
//     绝不改写（改一个字符的幂等键等于换了执行权）；采样规则的顺序与重复项不裁剪不去重；
//     expected_current_version 是调用方给的 CAS 位，空值不代填；
//  3. 写入口门槛：会话身份（缺失即 fail-closed）、operator 只能由会话渲染、
//     幂等键/审计 reason 非空、数值位非负；
//  4. 未配置下游、请求体缺失、入参形态非法时**不得**产生下游调用，也不得折叠成空台账冒充成功；
//  5. found=false / skipped / failed_ids / dead>0 都是**结论**而非错误，空列表投影成 []（非 nil）。
//
// 刻意不断言下游业务结论（AGENTS.md §5/§8）：event_type 白名单、采样基点区间、schema_version 上限、
// 时钟漂移与回补窗口、page_size 上限、cursor 语法、90 天窗口与无界扫描拒绝、策略生命周期
// （DRAFT→ACTIVE→ARCHIVED）、盐能否取到、单次重放条数上限、租约归属——全部由
// services/event-collector 判定。干跑路由（/schema/validate）连负数都不挡，因为越界值正是被测用例。
//
// 隐私口径（AGENTS.md §7）另有专门断言：台账响应只出现 device_hash/ip_segment/salt_version/salt_ref，
// 明文设备号与 IP 既不出现在响应里，也不出现在网关补算的摘要里。
//
// 打桩方式与 recommend 一致：内嵌生成的 client 接口 + 只覆盖本域用到的 13 个方法。
// 未覆盖的 CollectEvents / IngestServerEvents 一旦被调用会直接 panic——这正是
// 「终端与服务端上报入口不属 admin 面」这条边界的机器可检表达。不建 gRPC 连接、不碰数据库。

var errCollectorFakeDownstream = errors.New("collector downstream unavailable")

// collectorAdminFake 记录本域 13 个方法的入参，并返回预置回包/错误。
type collectorAdminFake struct {
	collectorrpc.EventCollectorClient

	err   error
	calls int

	validateReq *collectorrpc.ValidateEventSchemaReq
	validate    *collectorrpc.ValidateEventSchemaReply

	batchReq *collectorrpc.GetIngestBatchReq
	batch    *collectorrpc.GetIngestBatchReply
	batches  *collectorrpc.ListIngestBatchesReq
	batchLs  *collectorrpc.ListIngestBatchesReply

	recordReq *collectorrpc.GetEventRecordReq
	record    *collectorrpc.GetEventRecordReply
	records   *collectorrpc.ListEventRecordsReq
	recordLs  *collectorrpc.ListEventRecordsReply

	deadReq   *collectorrpc.ListDeadLettersReq
	deadLs    *collectorrpc.ListDeadLettersReply
	replayReq *collectorrpc.ReplayDeadLetterReq
	replay    *collectorrpc.ReplayDeadLetterReply

	activeReq   *collectorrpc.GetActiveDispatchPolicyReq
	activeReply *collectorrpc.DispatchPolicyReply
	policiesReq *collectorrpc.ListDispatchPoliciesReq
	policies    *collectorrpc.ListDispatchPoliciesReply
	upsertReq   *collectorrpc.UpsertDispatchPolicyReq
	upsert      *collectorrpc.DispatchPolicyReply
	activateOp  *collectorrpc.ActivateDispatchPolicyReq
	activate    *collectorrpc.DispatchPolicyReply

	healthReq *collectorrpc.GetCollectorHealthReq
	health    *collectorrpc.GetCollectorHealthReply
	retryReq  *collectorrpc.RetryPendingDeliveryReq
	retry     *collectorrpc.RetryPendingDeliveryReply
}

func (f *collectorAdminFake) ValidateEventSchema(_ context.Context, in *collectorrpc.ValidateEventSchemaReq,
	_ ...grpc.CallOption) (*collectorrpc.ValidateEventSchemaReply, error) {
	f.calls++
	f.validateReq = in
	return f.validate, f.err
}

func (f *collectorAdminFake) GetIngestBatch(_ context.Context, in *collectorrpc.GetIngestBatchReq,
	_ ...grpc.CallOption) (*collectorrpc.GetIngestBatchReply, error) {
	f.calls++
	f.batchReq = in
	return f.batch, f.err
}

func (f *collectorAdminFake) ListIngestBatches(_ context.Context, in *collectorrpc.ListIngestBatchesReq,
	_ ...grpc.CallOption) (*collectorrpc.ListIngestBatchesReply, error) {
	f.calls++
	f.batches = in
	return f.batchLs, f.err
}

func (f *collectorAdminFake) GetEventRecord(_ context.Context, in *collectorrpc.GetEventRecordReq,
	_ ...grpc.CallOption) (*collectorrpc.GetEventRecordReply, error) {
	f.calls++
	f.recordReq = in
	return f.record, f.err
}

func (f *collectorAdminFake) ListEventRecords(_ context.Context, in *collectorrpc.ListEventRecordsReq,
	_ ...grpc.CallOption) (*collectorrpc.ListEventRecordsReply, error) {
	f.calls++
	f.records = in
	return f.recordLs, f.err
}

func (f *collectorAdminFake) ListDeadLetters(_ context.Context, in *collectorrpc.ListDeadLettersReq,
	_ ...grpc.CallOption) (*collectorrpc.ListDeadLettersReply, error) {
	f.calls++
	f.deadReq = in
	return f.deadLs, f.err
}

func (f *collectorAdminFake) ReplayDeadLetter(_ context.Context, in *collectorrpc.ReplayDeadLetterReq,
	_ ...grpc.CallOption) (*collectorrpc.ReplayDeadLetterReply, error) {
	f.calls++
	f.replayReq = in
	return f.replay, f.err
}

func (f *collectorAdminFake) GetActiveDispatchPolicy(_ context.Context, in *collectorrpc.GetActiveDispatchPolicyReq,
	_ ...grpc.CallOption) (*collectorrpc.DispatchPolicyReply, error) {
	f.calls++
	f.activeReq = in
	return f.activeReply, f.err
}

func (f *collectorAdminFake) ListDispatchPolicies(_ context.Context, in *collectorrpc.ListDispatchPoliciesReq,
	_ ...grpc.CallOption) (*collectorrpc.ListDispatchPoliciesReply, error) {
	f.calls++
	f.policiesReq = in
	return f.policies, f.err
}

func (f *collectorAdminFake) UpsertDispatchPolicy(_ context.Context, in *collectorrpc.UpsertDispatchPolicyReq,
	_ ...grpc.CallOption) (*collectorrpc.DispatchPolicyReply, error) {
	f.calls++
	f.upsertReq = in
	return f.upsert, f.err
}

func (f *collectorAdminFake) ActivateDispatchPolicy(_ context.Context, in *collectorrpc.ActivateDispatchPolicyReq,
	_ ...grpc.CallOption) (*collectorrpc.DispatchPolicyReply, error) {
	f.calls++
	f.activateOp = in
	return f.activate, f.err
}

func (f *collectorAdminFake) GetCollectorHealth(_ context.Context, in *collectorrpc.GetCollectorHealthReq,
	_ ...grpc.CallOption) (*collectorrpc.GetCollectorHealthReply, error) {
	f.calls++
	f.healthReq = in
	return f.health, f.err
}

func (f *collectorAdminFake) RetryPendingDelivery(_ context.Context, in *collectorrpc.RetryPendingDeliveryReq,
	_ ...grpc.CallOption) (*collectorrpc.RetryPendingDeliveryReply, error) {
	f.calls++
	f.retryReq = in
	return f.retry, f.err
}

// --- 测试脚手架 ---

// collectorSvc 只填本域用到的一个客户端，其余字段留零值：logic 不该依赖别的下游。
func collectorSvc(cli collectorrpc.EventCollectorClient) *svc.ServiceContext {
	return &svc.ServiceContext{EventCollector: cli}
}

// collectorSessionCtx 模拟 AdminPermission 中间件已解析出会话身份的请求上下文。
func collectorSessionCtx() context.Context {
	return middleware.WithAdmin(context.Background(), middleware.AdminIdentity{
		AdminID: 77,
		Roles:   []string{"collector_ops"},
	})
}

// collectorOperatorFixture 是会话身份渲染出的 operator：77 取自上面的会话，客户端无法声明。
const collectorOperatorFixture = "gateway/admin:77"

// collectorAssertEnvelope 固定后台面四字段信封：成功恒 code=0/message=ok，
// 且采集台账读数一律 ttl=0——排障时缓存「几分钟前的在途状态」是假的现场。
func collectorAssertEnvelope(t *testing.T, code int, message string, ttl int64) {
	t.Helper()
	if code != 0 || message != "ok" || ttl != 0 {
		t.Fatalf("响应信封不符合后台口径：code=%d message=%q ttl=%d", code, message, ttl)
	}
}

// collectorBlank 把入参结构里名为 field 的字符串字段改成**空白**（不是空串）：
// 门槛按 TrimSpace 判空，用空白才能证明「空白键同样被拒」这条口径。
func collectorBlank(t *testing.T, body any, field string) {
	t.Helper()
	v := reflect.ValueOf(body).Elem().FieldByName(field)
	if !v.IsValid() || v.Kind() != reflect.String {
		t.Fatalf("%T 没有字符串字段 %s", body, field)
	}
	v.SetString("   ")
}

// collectorCallFn 执行一条路由并返回错误（req 传 nil 即覆盖「请求体缺失」分支）。
type collectorCallFn func(ctx context.Context, s *svc.ServiceContext, req any) error

// collectorRoute 描述一条路由的测试位：是否写入口（决定要不要会话身份）、
// 有无请求体（/health 与 /policy/active 在契约里就没有入参）、合法入参构造器与调用位。
type collectorRoute struct {
	name   string
	write  bool
	noBody bool
	body   func() any
	call   collectorCallFn
}

// collectorRoutes 覆盖本域接入的全部 13 条路由（9 读 + 4 写）。
func collectorRoutes() []collectorRoute {
	return []collectorRoute{
		{"collectorValidateSchema", false, false, func() any { return collectorValidateBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamCollectorSchemaValidate
				if req != nil {
					p, _ = req.(*types.ParamCollectorSchemaValidate)
				}
				_, err := NewCollectorValidateSchemaLogic(ctx, s).CollectorValidateSchema(p)
				return err
			}},
		{"collectorBatchGet", false, false, func() any { return collectorBatchGetBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamCollectorBatchGet
				if req != nil {
					p, _ = req.(*types.ParamCollectorBatchGet)
				}
				_, err := NewCollectorBatchGetLogic(ctx, s).CollectorBatchGet(p)
				return err
			}},
		{"collectorBatchList", false, false, func() any { return collectorBatchListBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamCollectorBatchList
				if req != nil {
					p, _ = req.(*types.ParamCollectorBatchList)
				}
				_, err := NewCollectorBatchListLogic(ctx, s).CollectorBatchList(p)
				return err
			}},
		{"collectorEventGet", false, false, func() any { return collectorEventGetBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamCollectorEventGet
				if req != nil {
					p, _ = req.(*types.ParamCollectorEventGet)
				}
				_, err := NewCollectorEventGetLogic(ctx, s).CollectorEventGet(p)
				return err
			}},
		{"collectorEventList", false, false, func() any { return collectorEventListBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamCollectorEventList
				if req != nil {
					p, _ = req.(*types.ParamCollectorEventList)
				}
				_, err := NewCollectorEventListLogic(ctx, s).CollectorEventList(p)
				return err
			}},
		{"collectorDeadLetterList", false, false, func() any { return collectorDeadLetterListBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamCollectorDeadLetterList
				if req != nil {
					p, _ = req.(*types.ParamCollectorDeadLetterList)
				}
				_, err := NewCollectorDeadLetterListLogic(ctx, s).CollectorDeadLetterList(p)
				return err
			}},
		{"collectorPolicyActive", false, true, nil,
			func(ctx context.Context, s *svc.ServiceContext, _ any) error {
				_, err := NewCollectorPolicyActiveLogic(ctx, s).CollectorPolicyActive()
				return err
			}},
		{"collectorPolicyList", false, false, func() any { return collectorPolicyListBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamCollectorPolicyList
				if req != nil {
					p, _ = req.(*types.ParamCollectorPolicyList)
				}
				_, err := NewCollectorPolicyListLogic(ctx, s).CollectorPolicyList(p)
				return err
			}},
		{"collectorHealth", false, true, nil,
			func(ctx context.Context, s *svc.ServiceContext, _ any) error {
				_, err := NewCollectorHealthLogic(ctx, s).CollectorHealth()
				return err
			}},
		{"collectorDeliveryRetry", true, false, func() any { return collectorRetryBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamCollectorDeliveryRetry
				if req != nil {
					p, _ = req.(*types.ParamCollectorDeliveryRetry)
				}
				_, err := NewCollectorDeliveryRetryLogic(ctx, s).CollectorDeliveryRetry(p)
				return err
			}},
		{"collectorDeadLetterReplay", true, false, func() any { return collectorReplayBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamCollectorDeadLetterReplay
				if req != nil {
					p, _ = req.(*types.ParamCollectorDeadLetterReplay)
				}
				_, err := NewCollectorDeadLetterReplayLogic(ctx, s).CollectorDeadLetterReplay(p)
				return err
			}},
		{"collectorPolicyUpsert", true, false, func() any { return collectorPolicyUpsertBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamCollectorPolicyUpsert
				if req != nil {
					p, _ = req.(*types.ParamCollectorPolicyUpsert)
				}
				_, err := NewCollectorPolicyUpsertLogic(ctx, s).CollectorPolicyUpsert(p)
				return err
			}},
		{"collectorPolicyActivate", true, false, func() any { return collectorPolicyActivateBody() },
			func(ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamCollectorPolicyActivate
				if req != nil {
					p, _ = req.(*types.ParamCollectorPolicyActivate)
				}
				_, err := NewCollectorPolicyActivateLogic(ctx, s).CollectorPolicyActivate(p)
				return err
			}},
	}
}

func collectorRouteByName(t *testing.T, name string) collectorRoute {
	t.Helper()
	for _, rt := range collectorRoutes() {
		if rt.name == name {
			return rt
		}
	}
	t.Fatalf("路由 %s 未登记在 collectorRoutes()", name)
	return collectorRoute{}
}

func collectorCast[T any](t *testing.T, body any) T {
	t.Helper()
	v, ok := body.(T)
	if !ok {
		var zero T
		t.Fatalf("入参类型应为 %T，实际 %T", zero, body)
	}
	return v
}

// --- 合法入参（每条路由一份，既做 happy 路径也做门槛用例的基底） ---

func collectorValidateBody() *types.ParamCollectorSchemaValidate {
	return &types.ParamCollectorSchemaValidate{
		Source: int32(collectorrpc.Source_SOURCE_CLIENT), EventId: "ev-20260920-0001",
		EventType: "behavior.play", SchemaVersion: 1, OccurredAt: 1700000000, ReportedAt: 1700000002,
		Category: int32(collectorrpc.BehaviorCategory_BEHAVIOR_CATEGORY_PLAY), TraceId: "trace-0001",
		ContentType: "video", ContentId: 9007199254740993, Aid: 1001, Vid: "BV1xx", TargetMid: 42,
		SessionId: "sess-1", PositionMs: 1234, DurationMs: 5678, BufferCount: 2, FirstFrameMs: 320,
		AvgBitrate: 2400, ErrorCode: "", Keyword: "", ResultIndex: 3, TargetUrl: "",
		Payload: "{\"k\":1}",
		CtxMid:  42, CtxDeviceId: "device-plain-001", CtxDeviceType: "imei", CtxIp: "203.0.113.9",
		CtxPlatform: int32(collectorrpc.Platform_PLATFORM_ANDROID), CtxAppId: "android",
		CtxAppVersion: "1.8.0", CtxSdkVersion: "2.3.1", CtxOsVersion: "14", CtxNetworkType: "wifi",
		CtxModel: "M2011K2C", CtxRegion: "CN-ZJ", CtxSessionId: "sess-1", CtxPage: "player",
		CtxSpm: "from.season.plays",
	}
}

func collectorBatchGetBody() *types.ParamCollectorBatchGet {
	return &types.ParamCollectorBatchGet{BatchId: "batch-20260920-01"}
}

func collectorBatchListBody() *types.ParamCollectorBatchList {
	return &types.ParamCollectorBatchList{
		Source: int32(collectorrpc.Source_SOURCE_CLIENT), State: int32(collectorrpc.BatchState_BATCH_STATE_PARTIAL),
		Mid: 42, DeviceHash: "devhash-aa", IpSegment: "203.0.113.0/24",
		CtimeFrom: 1700000000, CtimeTo: 1700086400, Cursor: "cur-1", PageSize: 50,
	}
}

func collectorEventGetBody() *types.ParamCollectorEventGet {
	return &types.ParamCollectorEventGet{EventId: "ev-20260920-0001"}
}

func collectorEventListBody() *types.ParamCollectorEventList {
	return &types.ParamCollectorEventList{
		BatchId: "batch-20260920-01", EventType: "behavior.play",
		Category:      int32(collectorrpc.BehaviorCategory_BEHAVIOR_CATEGORY_PLAY),
		Decision:      int32(collectorrpc.EventDecision_EVENT_DECISION_REJECTED),
		Reason:        int32(collectorrpc.RejectReason_REJECT_CLOCK_SKEW),
		DeliveryState: int32(collectorrpc.DeliveryState_DELIVERY_STATE_RETRYING),
		Topic:         "spm.behavior", Mid: 42, DeviceHash: "devhash-aa",
		CtimeFrom: 1700000000, CtimeTo: 1700086400, Cursor: "cur-2", PageSize: 20,
	}
}

func collectorDeadLetterListBody() *types.ParamCollectorDeadLetterList {
	return &types.ParamCollectorDeadLetterList{
		Topic: "spm.behavior", State: "open", CtimeFrom: 1700000000, CtimeTo: 1700086400,
		Cursor: "cur-3", PageSize: 30,
	}
}

func collectorPolicyListBody() *types.ParamCollectorPolicyList {
	return &types.ParamCollectorPolicyList{
		State: int32(collectorrpc.PolicyState_POLICY_STATE_ARCHIVED), Cursor: "cur-4", PageSize: 10,
	}
}

func collectorRetryBody() *types.ParamCollectorDeliveryRetry {
	return &types.ParamCollectorDeliveryRetry{
		Topic: "spm.behavior", Now: 1700000000, Limit: 500, IdempotencyKey: "idem-retry-0001",
	}
}

func collectorReplayBody() *types.ParamCollectorDeadLetterReplay {
	return &types.ParamCollectorDeadLetterReplay{
		DeadLetterIds: []int64{11, 12, 13}, IdempotencyKey: "idem-replay-0001",
		Reason: "MQ 抖动已恢复，重放这三条",
	}
}

func collectorPolicyUpsertBody() *types.ParamCollectorPolicyUpsert {
	return &types.ParamCollectorPolicyUpsert{
		Version: "p-2026w38-1",
		SampleRules: []types.CollectorSampleRule{
			{EventType: "behavior.play", SampleBps: 10000},
			// 同一条 event_type 登记两遍也要原样交给服务：去重会把配置错误藏起来。
			{EventType: "behavior.play", SampleBps: 500},
			{EventType: "*", SampleBps: 3000, QualityEvents: true},
		},
		SaltVersion: 3, SaltRef: "EC_DEVICE_SALT",
		FieldWhitelist: []string{"position_ms", "duration_ms"}, DropFields: []string{"keyword"},
		MaxEventsPerBatch: 200, MaxRequestBytes: 1048576, MaxEventPayloadBytes: 4096,
		MaxClockSkewSeconds: 300, MaxBackfillSeconds: 86400, KeywordMaxRunes: 32,
		RetentionDays: 90, DeliverMaxAttempts: 5, RetryBaseSeconds: 30, RetryMaxSeconds: 3600,
		Note: "曝光采样降到 30%", IdempotencyKey: "idem-policy-0001",
	}
}

func collectorPolicyActivateBody() *types.ParamCollectorPolicyActivate {
	return &types.ParamCollectorPolicyActivate{
		Version: "p-2026w38-1", ExpectedCurrentVersion: "p-2026w37-2",
		IdempotencyKey: "idem-activate-0001", Reason: "灰度通过，切生效版本",
	}
}

// --- 下游回包夹具 ---

func collectorBatchFixture() *collectorrpc.IngestBatch {
	return &collectorrpc.IngestBatch{
		Id: 9001, BatchId: "batch-20260920-01",
		Source:        collectorrpc.Source_SOURCE_CLIENT,
		CallerService: "gateway/app", Platform: collectorrpc.Platform_PLATFORM_ANDROID,
		AppId: "android", AppVersion: "1.8.0", SdkVersion: "2.3.1", Mid: 42,
		DeviceHash: "devhash-aa", IpSegment: "203.0.113.0/24", SaltVersion: 3, PolicyVersion: "p-2026w38-1",
		Total: 200, Accepted: 180, Duplicated: 5, Rejected: 10, SampledOut: 5, Dispatched: 170, Dead: 2,
		RequestBytes: 65536,
		State:        collectorrpc.BatchState_BATCH_STATE_PARTIAL,
		TopReason:    collectorrpc.RejectReason_REJECT_CLOCK_SKEW,
		LastError:    "mq timeout after 5 attempts", TraceId: "trace-0001",
		ReceivedAt: 1700000000, FinishedAt: 1700000060, Ctime: 1700000000, Mtime: 1700000060,
	}
}

func collectorBatchToAPIFixture() types.CollectorIngestBatch {
	return types.CollectorIngestBatch{
		Id: 9001, BatchId: "batch-20260920-01", Source: int32(collectorrpc.Source_SOURCE_CLIENT),
		CallerService: "gateway/app", Platform: int32(collectorrpc.Platform_PLATFORM_ANDROID),
		AppId: "android", AppVersion: "1.8.0", SdkVersion: "2.3.1", Mid: 42,
		DeviceHash: "devhash-aa", IpSegment: "203.0.113.0/24", SaltVersion: 3,
		PolicyVersion: "p-2026w38-1", Total: 200, Accepted: 180, Duplicated: 5, Rejected: 10,
		SampledOut: 5, Dispatched: 170, Dead: 2, RequestBytes: 65536,
		State:     int32(collectorrpc.BatchState_BATCH_STATE_PARTIAL),
		TopReason: int32(collectorrpc.RejectReason_REJECT_CLOCK_SKEW),
		LastError: "mq timeout after 5 attempts", TraceId: "trace-0001",
		ReceivedAt: 1700000000, FinishedAt: 1700000060, Ctime: 1700000000, Mtime: 1700000060,
	}
}

func collectorRecordFixture() *collectorrpc.EventRecord {
	return &collectorrpc.EventRecord{
		Id: 7001, EventId: "ev-20260920-0001", BatchId: "batch-20260920-01",
		EventType: "behavior.play", Category: collectorrpc.BehaviorCategory_BEHAVIOR_CATEGORY_PLAY,
		SchemaVersion: 1, OccurredAt: 1700000000, ReceivedAt: 1700000002, ClockSkewSeconds: -2,
		Decision:      collectorrpc.EventDecision_EVENT_DECISION_ACCEPTED,
		Reason:        collectorrpc.RejectReason_REJECT_REASON_NONE,
		ReasonDetail:  "",
		DeliveryState: collectorrpc.DeliveryState_DELIVERY_STATE_SENT,
		Topic:         "spm.behavior", EnvelopeEventId: "env-0001", DeliveryAttempts: 2, NextRetryAt: 0,
		LastError: "",
		Mid:       42, DeviceHash: "devhash-aa", IpSegment: "203.0.113.0/24", SaltVersion: 3,
		ContentType: "video", ContentId: 9007199254740993, Vid: "BV1xx", TargetMid: 42,
		PayloadDigest: "sha256:aaaa", PayloadBytes: 128, SanitizeVersion: "sv-1",
		PolicyVersion: "p-2026w38-1", TraceId: "trace-0001", Ctime: 1700000002, Mtime: 1700000060,
	}
}

func collectorPolicyFixture(state collectorrpc.PolicyState) *collectorrpc.DispatchPolicy {
	return &collectorrpc.DispatchPolicy{
		Version: "p-2026w38-1", State: state,
		SampleRules: []*collectorrpc.SampleRule{
			{EventType: "behavior.play", SampleBps: 10000},
			{EventType: "*", SampleBps: 3000, QualityEvents: true},
		},
		SaltVersion: 3, SaltRef: "EC_DEVICE_SALT",
		FieldWhitelist:       []string{"position_ms", "duration_ms"},
		DropFields:           []string{"keyword"},
		MaxEventsPerBatch:    200,
		MaxRequestBytes:      1048576,
		MaxEventPayloadBytes: 4096,
		MaxClockSkewSeconds:  300,
		MaxBackfillSeconds:   86400,
		KeywordMaxRunes:      32,
		RetentionDays:        90,
		DeliverMaxAttempts:   5,
		RetryBaseSeconds:     30,
		RetryMaxSeconds:      3600,
		Note:                 "曝光采样降到 30%", Operator: collectorOperatorFixture,
		Ctime: 1700000000, Mtime: 1700000400,
	}
}

func collectorDeadLetterFixture() *collectorrpc.DeadLetter {
	return &collectorrpc.DeadLetter{
		Id: 501, EventId: "ev-20260920-0001", BatchId: "batch-20260920-01",
		EventType: "behavior.play", Topic: "spm.behavior", PayloadDigest: "sha256:aaaa",
		Reason: "mq_unreachable", Attempts: 5, State: "open",
		CreatedAt: 1700000300, HandledAt: 0, Operator: "",
	}
}

// --- 1. 投影：rpc 回包逐字段进后台结构，一条都不能丢 ---

func TestCollectorBatchGetProjectsEveryField(t *testing.T) {
	f := &collectorAdminFake{batch: &collectorrpc.GetIngestBatchReply{Found: true, Batch: collectorBatchFixture()}}
	resp, err := NewCollectorBatchGetLogic(context.Background(), collectorSvc(f)).
		CollectorBatchGet(collectorBatchGetBody())
	if err != nil {
		t.Fatalf("CollectorBatchGet: %v", err)
	}
	collectorAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if !resp.Data.Found {
		t.Fatal("服务回了这一条，found 必须为 true")
	}
	if want := collectorBatchToAPIFixture(); !reflect.DeepEqual(resp.Data.Batch, want) {
		t.Fatalf("批次台账投影不完整（七类计数与 top_reason 是「丢了多少、为什么丢」的唯一证据）：\n got %+v\nwant %+v",
			resp.Data.Batch, want)
	}
	if f.batchReq.GetBatchId() != "batch-20260920-01" {
		t.Fatalf("batch_id 未原样透传：%q", f.batchReq.GetBatchId())
	}
}

func TestCollectorBatchGetFoundFalseIsConclusionNotError(t *testing.T) {
	f := &collectorAdminFake{batch: &collectorrpc.GetIngestBatchReply{}} // 服务说没有
	resp, err := NewCollectorBatchGetLogic(context.Background(), collectorSvc(f)).
		CollectorBatchGet(collectorBatchGetBody())
	if err != nil {
		t.Fatalf("未命中不是错误：got %v", err)
	}
	if resp.Data.Found {
		t.Fatal("batch 缺席时 found 必须为 false")
	}
	if resp.Data.Batch.Total != 0 || resp.Data.Batch.BatchId != "" {
		t.Fatalf("未命中却回了计数，会被读成「收到过一个空批次」：%+v", resp.Data.Batch)
	}
}

func TestCollectorBatchListProjectsAndPassesFilters(t *testing.T) {
	f := &collectorAdminFake{batchLs: &collectorrpc.ListIngestBatchesReply{
		List: []*collectorrpc.IngestBatch{collectorBatchFixture()}, NextCursor: "cur-next",
		HasMore: true, Total: 1234,
	}}
	body := collectorBatchListBody()
	resp, err := NewCollectorBatchListLogic(context.Background(), collectorSvc(f)).CollectorBatchList(body)
	if err != nil {
		t.Fatalf("CollectorBatchList: %v", err)
	}
	collectorAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if len(resp.Data.List) != 1 || !reflect.DeepEqual(resp.Data.List[0], collectorBatchToAPIFixture()) {
		t.Fatalf("批次分页投影不完整：%+v", resp.Data.List[0])
	}
	if resp.Data.NextCursor != "cur-next" || !resp.Data.HasMore || resp.Data.Total != 1234 {
		t.Fatalf("翻页游标/总数未回传：%+v", resp.Data)
	}
	got := f.batches
	if got.GetSource() != collectorrpc.Source_SOURCE_CLIENT || got.GetState() != collectorrpc.BatchState_BATCH_STATE_PARTIAL ||
		got.GetMid() != body.Mid || got.GetDeviceHash() != body.DeviceHash || got.GetIpSegment() != body.IpSegment ||
		got.GetCtimeFrom() != body.CtimeFrom || got.GetCtimeTo() != body.CtimeTo ||
		got.GetCursor() != body.Cursor || got.GetPageSize() != body.PageSize {
		t.Fatalf("过滤条件未原样透传：%+v vs %+v", got, body)
	}
}

func TestCollectorListRoutesProjectEmptyAsNotNilSlices(t *testing.T) {
	f := &collectorAdminFake{
		batchLs:  &collectorrpc.ListIngestBatchesReply{},
		recordLs: &collectorrpc.ListEventRecordsReply{},
		deadLs:   &collectorrpc.ListDeadLettersReply{},
		policies: &collectorrpc.ListDispatchPoliciesReply{},
		health:   &collectorrpc.GetCollectorHealthReply{},
		validate: &collectorrpc.ValidateEventSchemaReply{},
	}
	s := collectorSvc(f)
	ctx := context.Background()
	batchResp, err := NewCollectorBatchListLogic(ctx, s).CollectorBatchList(collectorBatchListBody())
	if err != nil {
		t.Fatalf("CollectorBatchList: %v", err)
	}
	if batchResp.Data.List == nil {
		t.Fatal("空批次台账必须投影成 []，不能回 null")
	}
	evResp, err := NewCollectorEventListLogic(ctx, s).CollectorEventList(collectorEventListBody())
	if err != nil || evResp.Data.List == nil {
		t.Fatalf("空事件台账必须投影成 []：%v", err)
	}
	dlResp, err := NewCollectorDeadLetterListLogic(ctx, s).CollectorDeadLetterList(collectorDeadLetterListBody())
	if err != nil || dlResp.Data.List == nil {
		t.Fatalf("空死信台账必须投影成 []：%v", err)
	}
	plResp, err := NewCollectorPolicyListLogic(ctx, s).CollectorPolicyList(collectorPolicyListBody())
	if err != nil || plResp.Data.List == nil {
		t.Fatalf("空策略台账必须投影成 []：%v", err)
	}
	hResp, err := NewCollectorHealthLogic(ctx, s).CollectorHealth()
	if err != nil || hResp.Data.Topics == nil {
		t.Fatalf("空 topic 积压必须投影成 []：%v", err)
	}
	vResp, err := NewCollectorValidateSchemaLogic(ctx, s).CollectorValidateSchema(collectorValidateBody())
	if err != nil || vResp.Data.MissingFields == nil {
		t.Fatalf("缺失字段列表必须投影成 []：%v", err)
	}
}

func TestCollectorEventGetProjectsDeliveryTruth(t *testing.T) {
	r := collectorRecordFixture()
	f := &collectorAdminFake{record: &collectorrpc.GetEventRecordReply{Found: true, Record: r}}
	resp, err := NewCollectorEventGetLogic(context.Background(), collectorSvc(f)).
		CollectorEventGet(collectorEventGetBody())
	if err != nil {
		t.Fatalf("CollectorEventGet: %v", err)
	}
	collectorAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	want := types.CollectorEventRecord{
		Id: 7001, EventId: "ev-20260920-0001", BatchId: "batch-20260920-01",
		EventType: "behavior.play", Category: int32(collectorrpc.BehaviorCategory_BEHAVIOR_CATEGORY_PLAY),
		SchemaVersion: 1, OccurredAt: 1700000000, ReceivedAt: 1700000002, ClockSkewSeconds: -2,
		Decision:      int32(collectorrpc.EventDecision_EVENT_DECISION_ACCEPTED),
		Reason:        int32(collectorrpc.RejectReason_REJECT_REASON_NONE),
		DeliveryState: int32(collectorrpc.DeliveryState_DELIVERY_STATE_SENT),
		Topic:         "spm.behavior", EnvelopeEventId: "env-0001", DeliveryAttempts: 2,
		Mid: 42, DeviceHash: "devhash-aa", IpSegment: "203.0.113.0/24", SaltVersion: 3,
		ContentType: "video", ContentId: 9007199254740993, Vid: "BV1xx", TargetMid: 42,
		PayloadDigest: "sha256:aaaa", PayloadBytes: 128, SanitizeVersion: "sv-1",
		PolicyVersion: "p-2026w38-1", TraceId: "trace-0001", Ctime: 1700000002, Mtime: 1700000060,
	}
	if !reflect.DeepEqual(resp.Data.Record, want) {
		t.Fatalf("事件台账投影不完整（delivery_* 已被服务用 Outbox 真值覆盖，网关不得再改一个字）：\n got %+v\nwant %+v",
			resp.Data.Record, want)
	}
	// clock_skew_seconds 是**有符号**读数（客户端时钟慢于服务端时为负），网关的非负门槛不得波及它。
	if resp.Data.Record.ClockSkewSeconds != -2 {
		t.Fatalf("负数 clock_skew_seconds 被折掉了：%d", resp.Data.Record.ClockSkewSeconds)
	}
	if f.recordReq.GetEventId() != "ev-20260920-0001" {
		t.Fatalf("event_id 未原样透传：%q", f.recordReq.GetEventId())
	}
}

func TestCollectorEventGetFoundFalseWhenRecordMissing(t *testing.T) {
	f := &collectorAdminFake{record: &collectorrpc.GetEventRecordReply{}}
	resp, err := NewCollectorEventGetLogic(context.Background(), collectorSvc(f)).
		CollectorEventGet(collectorEventGetBody())
	if err != nil {
		t.Fatalf("未命中不是错误：got %v", err)
	}
	if resp.Data.Found || resp.Data.Record.DeliveryState != 0 {
		t.Fatalf("record 缺席时应 found=false 且不留投递状态：%+v", resp.Data)
	}
}

func TestCollectorEventListKeepsDecisionAndDeliveryIndependent(t *testing.T) {
	f := &collectorAdminFake{recordLs: &collectorrpc.ListEventRecordsReply{
		List: []*collectorrpc.EventRecord{collectorRecordFixture()}, HasMore: true, Total: 3,
	}}
	body := collectorEventListBody()
	resp, err := NewCollectorEventListLogic(context.Background(), collectorSvc(f)).CollectorEventList(body)
	if err != nil {
		t.Fatalf("CollectorEventList: %v", err)
	}
	collectorAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if len(resp.Data.List) != 1 || resp.Data.Total != 3 || !resp.Data.HasMore {
		t.Fatalf("分页结论投影不完整：%+v", resp.Data)
	}
	got := f.records
	// 「被拒」（decision/reason）与「投递失败」（delivery_state）是两个维度，网关不做映射。
	if got.GetDecision() != collectorrpc.EventDecision_EVENT_DECISION_REJECTED ||
		got.GetReason() != collectorrpc.RejectReason_REJECT_CLOCK_SKEW ||
		got.GetDeliveryState() != collectorrpc.DeliveryState_DELIVERY_STATE_RETRYING {
		t.Fatalf("三个独立维度被折算了：%+v", got)
	}
	if got.GetBatchId() != body.BatchId || got.GetEventType() != body.EventType ||
		got.GetTopic() != body.Topic || got.GetDeviceHash() != body.DeviceHash ||
		got.GetCategory() != collectorrpc.BehaviorCategory(body.Category) ||
		got.GetCursor() != body.Cursor || got.GetPageSize() != body.PageSize {
		t.Fatalf("过滤条件未原样透传：%+v vs %+v", got, body)
	}
}

func TestCollectorDeadLetterListProjectsReasonVerbatim(t *testing.T) {
	f := &collectorAdminFake{deadLs: &collectorrpc.ListDeadLettersReply{
		List: []*collectorrpc.DeadLetter{collectorDeadLetterFixture()}, NextCursor: "cur-d", HasMore: true, Total: 9,
	}}
	body := collectorDeadLetterListBody()
	resp, err := NewCollectorDeadLetterListLogic(context.Background(), collectorSvc(f)).CollectorDeadLetterList(body)
	if err != nil {
		t.Fatalf("CollectorDeadLetterList: %v", err)
	}
	collectorAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	want := types.CollectorDeadLetterInfo{
		Id: 501, EventId: "ev-20260920-0001", BatchId: "batch-20260920-01", EventType: "behavior.play",
		Topic: "spm.behavior", PayloadDigest: "sha256:aaaa", Reason: "mq_unreachable",
		Attempts: 5, State: "open", CreatedAt: 1700000300,
	}
	if !reflect.DeepEqual(resp.Data.List[0], want) {
		t.Fatalf("死信投影被改写了（reason 是稳定枚举，翻译过一次筛选条件就对不上了）：\n got %+v\nwant %+v",
			resp.Data.List[0], want)
	}
	if f.deadReq.GetTopic() != body.Topic || f.deadReq.GetState() != "open" ||
		f.deadReq.GetCursor() != body.Cursor || f.deadReq.GetPageSize() != body.PageSize {
		t.Fatalf("死信过滤条件未原样透传：%+v", f.deadReq)
	}
}

func TestCollectorPolicyActiveProjectsServerTruth(t *testing.T) {
	f := &collectorAdminFake{activeReply: &collectorrpc.DispatchPolicyReply{
		Policy: collectorPolicyFixture(collectorrpc.PolicyState_POLICY_STATE_ACTIVE), Created: false,
	}}
	resp, err := NewCollectorPolicyActiveLogic(context.Background(), collectorSvc(f)).CollectorPolicyActive()
	if err != nil {
		t.Fatalf("CollectorPolicyActive: %v", err)
	}
	collectorAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	want := collectorPolicyToAPI(f.activeReply.Policy)
	if !reflect.DeepEqual(resp.Data.Policy, want) {
		t.Fatalf("策略投影不完整：\n got %+v\nwant %+v", resp.Data.Policy, want)
	}
	if resp.Data.Policy.SaltRef != "EC_DEVICE_SALT" {
		t.Fatalf("salt_ref 应回环境变量名：%q", resp.Data.Policy.SaltRef)
	}
	// 生效版本读取刻意不缓存：ttl 必须为 0，否则面板会与采集实际用的版本漂移。
	if resp.TTL != 0 {
		t.Fatalf("ttl=%d", resp.TTL)
	}
	// 无 ACTIVE 时服务外抛错误而不是回空策略：网关不得折叠成 code=0 + 全零策略。
	f2 := &collectorAdminFake{err: errors.New("event-collector: no active dispatch policy")}
	if _, err := NewCollectorPolicyActiveLogic(context.Background(), collectorSvc(f2)).CollectorPolicyActive(); err == nil {
		t.Fatal("下游报「无生效策略」时被折叠成了成功")
	}
}

func TestCollectorPolicyListIncludesArchived(t *testing.T) {
	f := &collectorAdminFake{policies: &collectorrpc.ListDispatchPoliciesReply{
		List: []*collectorrpc.DispatchPolicy{
			collectorPolicyFixture(collectorrpc.PolicyState_POLICY_STATE_DRAFT),
			collectorPolicyFixture(collectorrpc.PolicyState_POLICY_STATE_ARCHIVED),
		},
	}}
	body := collectorPolicyListBody()
	resp, err := NewCollectorPolicyListLogic(context.Background(), collectorSvc(f)).CollectorPolicyList(body)
	if err != nil {
		t.Fatalf("CollectorPolicyList: %v", err)
	}
	collectorAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if len(resp.Data.List) != 2 || resp.Data.List[1].State != int32(collectorrpc.PolicyState_POLICY_STATE_ARCHIVED) {
		t.Fatalf("ARCHIVED 版本是历史批次的归因依据，必须看得见：%+v", resp.Data.List)
	}
	if f.policiesReq.GetState() != collectorrpc.PolicyState_POLICY_STATE_ARCHIVED ||
		f.policiesReq.GetCursor() != body.Cursor || f.policiesReq.GetPageSize() != body.PageSize {
		t.Fatalf("策略翻页条件未原样透传：%+v", f.policiesReq)
	}
}

func TestCollectorHealthProjectsFactBitsWithoutVerdict(t *testing.T) {
	f := &collectorAdminFake{health: &collectorrpc.GetCollectorHealthReply{
		ServerTime: 1700000500,
		Topics: []*collectorrpc.TopicHealth{{
			Topic: "spm.behavior", Pending: 30, Retrying: 4, DeadOpen: 2, SentLastHour: 9000,
			OldestPendingCtime: 1700000100,
		}},
		BatchesRejectedLastHour: 7, RateLimitedLastHour: 12,
		ActiveSaltVersion: 0, PolicyVersion: "p-2026w38-1", SaltRef: "EC_DEVICE_SALT",
		SaltAvailable: false, Version: "1.4.2",
	}}
	resp, err := NewCollectorHealthLogic(context.Background(), collectorSvc(f)).CollectorHealth()
	if err != nil {
		t.Fatalf("CollectorHealth: %v", err)
	}
	collectorAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	want := types.CollectorHealthData{
		ServerTime: 1700000500,
		Topics: []types.CollectorTopicHealth{{
			Topic: "spm.behavior", Pending: 30, Retrying: 4, DeadOpen: 2, SentLastHour: 9000,
			OldestPendingCtime: 1700000100,
		}},
		BatchesRejectedLastHour: 7, RateLimitedLastHour: 12,
		ActiveSaltVersion: 0, PolicyVersion: "p-2026w38-1", SaltRef: "EC_DEVICE_SALT",
		SaltAvailable: false, Version: "1.4.2",
	}
	if !reflect.DeepEqual(resp.Data, want) {
		t.Fatalf("健康度投影不完整：\n got %+v\nwant %+v", resp.Data, want)
	}
	// 盐缺失（active_salt_version=0 + salt_available=false）必须原样带出：
	// 网关若合成一个「健康」布尔，就等于把最要紧的隐私风险位藏起来。
	if resp.Data.ActiveSaltVersion != 0 || resp.Data.SaltAvailable {
		t.Fatalf("盐不可用的事实位被改写：%+v", resp.Data)
	}
	if _, ok := reflect.TypeOf(resp.Data).FieldByName("Healthy"); ok {
		t.Fatal("网关不得自行合成健康结论位")
	}
}

func TestCollectorValidateSchemaPassesEveryInputAndHidesPlaintext(t *testing.T) {
	f := &collectorAdminFake{validate: &collectorrpc.ValidateEventSchemaReply{
		Valid: false, Decision: collectorrpc.EventDecision_EVENT_DECISION_REJECTED,
		Reason:        collectorrpc.RejectReason_REJECT_INVALID_METRIC,
		MissingFields: []string{"trace_id", "content_id"},
		EventType:     "behavior.play", Topic: "spm.behavior", SchemaVersion: 1,
		Note: "keyword 已截断至 32 字",
	}}
	body := collectorValidateBody()
	resp, err := NewCollectorValidateSchemaLogic(context.Background(), collectorSvc(f)).CollectorValidateSchema(body)
	if err != nil {
		t.Fatalf("CollectorValidateSchema: %v", err)
	}
	collectorAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if want := (types.CollectorSchemaValidateData{
		Valid: false, Decision: int32(collectorrpc.EventDecision_EVENT_DECISION_REJECTED),
		Reason:        int32(collectorrpc.RejectReason_REJECT_INVALID_METRIC),
		MissingFields: []string{"trace_id", "content_id"},
		EventType:     "behavior.play", Topic: "spm.behavior", SchemaVersion: 1,
		Note: "keyword 已截断至 32 字",
	}); !reflect.DeepEqual(resp.Data, want) {
		t.Fatalf("干跑结论投影不完整：\n got %+v\nwant %+v", resp.Data, want)
	}
	ev := f.validateReq.GetEvent()
	if ev.GetEventId() != body.EventId || ev.GetEventType() != body.EventType ||
		ev.GetSchemaVersion() != body.SchemaVersion || ev.GetOccurredAt() != body.OccurredAt ||
		ev.GetContentId() != body.ContentId || ev.GetPositionMs() != body.PositionMs ||
		ev.GetPayload() != body.Payload {
		t.Fatalf("干跑事件未逐字段交给服务：%+v", ev)
	}
	// 干跑刻意不挡负数、不补 event_id/occurred_at：那正是待测的判定用例。
	neg := collectorValidateBody()
	neg.EventId, neg.OccurredAt, neg.PositionMs = "", 0, -1
	nf := &collectorAdminFake{validate: &collectorrpc.ValidateEventSchemaReply{}}
	if _, err := NewCollectorValidateSchemaLogic(context.Background(), collectorSvc(nf)).CollectorValidateSchema(neg); err != nil {
		t.Fatalf("干跑应把越界值送进服务而不是在网关被吃掉：%v", err)
	}
	if nf.validateReq.GetEvent().GetEventId() != "" || nf.validateReq.GetEvent().GetPositionMs() != -1 {
		t.Fatalf("网关替客户端补了字段/夹了负数：%+v", nf.validateReq.GetEvent())
	}
	// 明文设备号/IP 只作为入参交给服务算摘要，响应里一个字节都不能出现（AGENTS.md §7）。
	pt := f.validateReq.GetContext()
	if pt.GetDeviceId() != body.CtxDeviceId || pt.GetIp() != body.CtxIp {
		t.Fatalf("明文设备号/IP 未交给服务判定：%+v", pt)
	}
	if pt.GetDeviceHash() != "" || pt.GetIpSegment() != "" {
		t.Fatal("网关不代算摘要：算摘要要用盐，而盐只在服务侧可取")
	}
	assertJSONFreeOf(t, resp, []string{body.CtxDeviceId, body.CtxIp})
}

// --- 2. 写入口：入参逐字段交给下游，operator 只来自会话 ---

func TestCollectorDeliveryRetryProjectsCounters(t *testing.T) {
	f := &collectorAdminFake{retry: &collectorrpc.RetryPendingDeliveryReply{
		Scanned: 500, Sent: 470, Retrying: 28, Dead: 2, NextRunAt: 1700000600,
	}}
	body := collectorRetryBody()
	resp, err := NewCollectorDeliveryRetryLogic(collectorSessionCtx(), collectorSvc(f)).CollectorDeliveryRetry(body)
	if err != nil {
		t.Fatalf("CollectorDeliveryRetry: %v", err)
	}
	collectorAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if want := (types.CollectorDeliveryRetryData{Scanned: 500, Sent: 470, Retrying: 28, Dead: 2, NextRunAt: 1700000600}); !reflect.DeepEqual(resp.Data, want) {
		t.Fatalf("推进结论投影不完整：\n got %+v\nwant %+v", resp.Data, want)
	}
	// dead>0 是这一轮的事实而非失败：不得折成 HTTP 错误或零值。
	if resp.Data.Dead != 2 {
		t.Fatalf("dead 计数被吞：%+v", resp.Data)
	}
	got := f.retryReq
	if got.GetTopic() != body.Topic || got.GetNow() != body.Now || got.GetLimit() != body.Limit ||
		got.GetIdempotencyKey() != "idem-retry-0001" {
		t.Fatalf("推进入参未原样透传：%+v vs %+v", got, body)
	}
	if got.GetOperator() != collectorOperatorFixture {
		t.Fatalf("operator 应来自会话：%q", got.GetOperator())
	}
}

func TestCollectorDeadLetterReplayProjectsSkippedAndFailed(t *testing.T) {
	f := &collectorAdminFake{replay: &collectorrpc.ReplayDeadLetterReply{
		Replayed: 1, Skipped: 1, FailedIds: []int64{13},
	}}
	body := collectorReplayBody()
	resp, err := NewCollectorDeadLetterReplayLogic(collectorSessionCtx(), collectorSvc(f)).CollectorDeadLetterReplay(body)
	if err != nil {
		t.Fatalf("CollectorDeadLetterReplay: %v", err)
	}
	collectorAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if want := (types.CollectorDeadLetterReplayData{Replayed: 1, Skipped: 1, FailedIds: []int64{13}}); !reflect.DeepEqual(resp.Data, want) {
		t.Fatalf("重放结论投影不完整（failed_ids 决定「同一批 ID 能否原样再来一次」）：\n got %+v\nwant %+v", resp.Data, want)
	}
	if !reflect.DeepEqual(f.replayReq.GetDeadLetterIds(), []int64{11, 12, 13}) {
		t.Fatalf("死信 ID 列表被改写（不去重、不排序、不裁剪）：%+v", f.replayReq.GetDeadLetterIds())
	}
	if f.replayReq.GetReason() != body.Reason || f.replayReq.GetIdempotencyKey() != "idem-replay-0001" ||
		f.replayReq.GetOperator() != collectorOperatorFixture {
		t.Fatalf("重放入参未原样透传：%+v", f.replayReq)
	}
}

func TestCollectorPolicyUpsertSendsDraftOnly(t *testing.T) {
	f := &collectorAdminFake{upsert: &collectorrpc.DispatchPolicyReply{
		Policy: collectorPolicyFixture(collectorrpc.PolicyState_POLICY_STATE_DRAFT), Created: true,
	}}
	body := collectorPolicyUpsertBody()
	resp, err := NewCollectorPolicyUpsertLogic(collectorSessionCtx(), collectorSvc(f)).CollectorPolicyUpsert(body)
	if err != nil {
		t.Fatalf("CollectorPolicyUpsert: %v", err)
	}
	collectorAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if !resp.Data.Created {
		t.Fatal("created=true 是「新建了草稿」的结论，不得丢")
	}
	got := f.upsertReq
	if got.GetOperator() != collectorOperatorFixture || got.GetIdempotencyKey() != "idem-policy-0001" {
		t.Fatalf("upsert 顶层审计位未原样透传：%+v", got)
	}
	p := got.GetPolicy()
	if p.GetVersion() != body.Version || p.GetSaltVersion() != body.SaltVersion ||
		p.GetSaltRef() != body.SaltRef || p.GetNote() != body.Note ||
		p.GetMaxRequestBytes() != body.MaxRequestBytes || p.GetRetryBaseSeconds() != body.RetryBaseSeconds ||
		!reflect.DeepEqual(p.GetFieldWhitelist(), body.FieldWhitelist) ||
		!reflect.DeepEqual(p.GetDropFields(), body.DropFields) {
		t.Fatalf("策略字段未原样交给服务：%+v vs %+v", p, body)
	}
	// **本路由只写草稿**：policy.state 保持 UNSPECIFIED，服务据此按 DRAFT 落库。
	// 生效只能走 /policy/activate（collector:policy/enable），否则「改配置」顺带等于「发布」。
	if p.GetState() != collectorrpc.PolicyState_POLICY_STATE_UNSPECIFIED {
		t.Fatalf("upsert 不得声明状态，实际送出 %v", p.GetState())
	}
	if p.GetCtime() != 0 || p.GetMtime() != 0 || p.GetOperator() != "" {
		t.Fatalf("ctime/mtime/operator 是库的事实列，不得由表单代填：%+v", p)
	}
	// 采样规则条数、顺序与重复项原样交付（不补 "*" 兜底、不去重）。
	if len(p.GetSampleRules()) != 3 || p.GetSampleRules()[0].GetSampleBps() != 10000 ||
		p.GetSampleRules()[1].GetSampleBps() != 500 || !p.GetSampleRules()[2].GetQualityEvents() {
		t.Fatalf("sample_rules 被网关整理过：%+v", p.GetSampleRules())
	}
}

func TestCollectorPolicyActivatePassesCasVerbatim(t *testing.T) {
	f := &collectorAdminFake{activate: &collectorrpc.DispatchPolicyReply{
		Policy: collectorPolicyFixture(collectorrpc.PolicyState_POLICY_STATE_ACTIVE), Created: false,
	}}
	body := collectorPolicyActivateBody()
	resp, err := NewCollectorPolicyActivateLogic(collectorSessionCtx(), collectorSvc(f)).CollectorPolicyActivate(body)
	if err != nil {
		t.Fatalf("CollectorPolicyActivate: %v", err)
	}
	collectorAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	// 本方法不新建版本：created 恒 false，如实投影而不省略字段。
	if resp.Data.Created {
		t.Fatal("activate 不新建版本，created 不得为 true")
	}
	got := f.activateOp
	if got.GetVersion() != body.Version ||
		got.GetExpectedCurrentVersion() != body.ExpectedCurrentVersion ||
		got.GetIdempotencyKey() != "idem-activate-0001" || got.GetReason() != body.Reason ||
		got.GetOperator() != collectorOperatorFixture {
		t.Fatalf("激活入参未原样透传：%+v vs %+v", got, body)
	}
	// expected_current_version 为空表示「不管当前是哪版都切」，网关不替调用方补一个 CAS 值。
	noCAS := collectorPolicyActivateBody()
	noCAS.ExpectedCurrentVersion = ""
	nf := &collectorAdminFake{activate: &collectorrpc.DispatchPolicyReply{}}
	if _, err := NewCollectorPolicyActivateLogic(collectorSessionCtx(), collectorSvc(nf)).CollectorPolicyActivate(noCAS); err != nil {
		t.Fatalf("省略 CAS 位应交给服务判定：%v", err)
	}
	if nf.activateOp.GetExpectedCurrentVersion() != "" {
		t.Fatalf("网关代填了 CAS 值：%q", nf.activateOp.GetExpectedCurrentVersion())
	}
}

// TestCollectorFormsExposeNoOperator 钉住两条「表单说自己」的禁区：
//  1. 四条写入口的表单类型都没有 operator/admin 位——操作者只能由会话渲染；
//  2. /policy/upsert 的表单没有 state 位——否则该路由就顺带获得了激活能力。
func TestCollectorFormsExposeNoOperator(t *testing.T) {
	forms := map[string]any{
		"collectorDeliveryRetry":    collectorRetryBody(),
		"collectorDeadLetterReplay": collectorReplayBody(),
		"collectorPolicyUpsert":     collectorPolicyUpsertBody(),
		"collectorPolicyActivate":   collectorPolicyActivateBody(),
	}
	if len(forms) != 4 {
		t.Fatalf("写入口应为 4 条，实际 %d", len(forms))
	}
	for name, body := range forms {
		tp := reflect.TypeOf(body).Elem()
		for i := 0; i < tp.NumField(); i++ {
			tag := strings.ToLower(tp.Field(i).Tag.Get("json"))
			if strings.Contains(tag, "operator") || strings.Contains(tag, "admin") {
				t.Fatalf("%s 的表单暴露了操作者位 %s（%s）：只能由会话渲染", name, tp.Field(i).Name, tag)
			}
			if name == "collectorPolicyUpsert" && strings.HasPrefix(tag, "state") {
				t.Fatalf("policy/upsert 不得暴露 state 位（生效是 collector:policy/enable 的职责）")
			}
		}
	}
}

// TestCollectorReadResponsesCarryOnlyPseudonyms 检查本域全部响应结构里
// 只有别名位（device_hash/ip_segment/salt_version/salt_ref/payload_digest），
// 没有任何能承载明文设备号、IP、盐值的字段（AGENTS.md §7）。
func TestCollectorReadResponsesCarryOnlyPseudonyms(t *testing.T) {
	for _, tp := range []reflect.Type{
		reflect.TypeOf(types.CollectorIngestBatch{}),
		reflect.TypeOf(types.CollectorEventRecord{}),
		reflect.TypeOf(types.CollectorDeadLetterInfo{}),
		reflect.TypeOf(types.CollectorDispatchPolicy{}),
		reflect.TypeOf(types.CollectorHealthData{}),
		reflect.TypeOf(types.CollectorSchemaValidateData{}),
	} {
		var sawHash, sawSegment, sawSaltRef bool
		for i := 0; i < tp.NumField(); i++ {
			name := strings.ToLower(tp.Field(i).Name)
			tag := strings.ToLower(tp.Field(i).Tag.Get("json"))
			switch {
			case strings.Contains(name, "deviceid"), strings.Contains(name, "phone"),
				strings.Contains(tag, "device_id"), strings.Contains(tag, "phone"):
				t.Fatalf("%s 暴露了明文设备号位 %s", tp.Name(), name)
			case name == "ip" || strings.Contains(tag, "\"ip,") || strings.Contains(tag, "\"ip\""):
				t.Fatalf("%s 暴露了明文 IP 位 %s", tp.Name(), name)
			case strings.Contains(name, "saltvalue"), strings.Contains(name, "saltsecret"):
				t.Fatalf("%s 暴露了盐值位 %s（响应只允许 salt_version/salt_ref）", tp.Name(), name)
			}
			sawHash = sawHash || strings.Contains(name, "devicehash")
			sawSegment = sawSegment || strings.Contains(name, "ipsegment")
			sawSaltRef = sawSaltRef || strings.Contains(name, "saltref")
		}
		if tp.Name() == "CollectorIngestBatch" && !(sawHash && sawSegment) {
			t.Fatal("批次台账必须回带 device_hash/ip_segment，否则跨源归因做不了")
		}
		if tp.Name() == "CollectorDispatchPolicy" && !sawSaltRef {
			t.Fatal("策略必须回带 salt_ref（环境变量名），盐值本身永不出面")
		}
	}
}

// --- 3. fail-closed 门槛：门槛不过就不产生下游调用 ---

func TestCollectorRouteTableCoversAllThirteenRoutes(t *testing.T) {
	var reads, writes, noBody int
	for _, rt := range collectorRoutes() {
		if rt.write {
			writes++
		} else {
			reads++
		}
		if rt.noBody {
			noBody++
		}
	}
	if reads != 9 || writes != 4 {
		t.Fatalf("本域应为 9 个读入口 + 4 个写入口，实际 %d + %d", reads, writes)
	}
	if noBody != 2 {
		t.Fatalf("/health 与 /policy/active 契约里就没有入参，应为 2 条，实际 %d", noBody)
	}
}

func TestCollectorRoutesFailClosedWithoutClient(t *testing.T) {
	// 未配置下游时不退化成伪造空台账：那会让后台把「下游没接」读成「这段时间没有事件」，
	// 而「没有事件」在采集域最容易被误读成「埋点正常但用户没来」。
	for _, rt := range collectorRoutes() {
		var body any
		if rt.body != nil {
			body = rt.body()
		}
		err := rt.call(collectorSessionCtx(), &svc.ServiceContext{}, body)
		if !errors.Is(err, errCollectorServiceNotConfigured) {
			t.Fatalf("%s 未配置客户端时应返回明确的 not configured，got %v", rt.name, err)
		}
		if !strings.Contains(err.Error(), "service not configured") {
			t.Fatalf("%s 错误消息不可定位：%v", rt.name, err)
		}
	}
}

func TestCollectorRoutesRejectMissingBody(t *testing.T) {
	f := &collectorAdminFake{}
	for _, rt := range collectorRoutes() {
		if rt.noBody {
			continue // 这两条路由在契约里就没有请求体
		}
		err := rt.call(collectorSessionCtx(), collectorSvc(f), nil)
		if !errors.Is(err, errCollectorRequestMissing) {
			t.Fatalf("%s 请求体缺失时应返回 %v，got %v", rt.name, errCollectorRequestMissing, err)
		}
	}
	if f.calls != 0 {
		t.Fatalf("请求体缺失却打了下游 %d 次", f.calls)
	}
}

func TestCollectorRoutesPassDownstreamErrorsThrough(t *testing.T) {
	// 13 条路由共用一个假客户端，逐条预置回包，确保每条都真的走到下游再报错。
	f := &collectorAdminFake{
		err:         errCollectorFakeDownstream,
		validate:    &collectorrpc.ValidateEventSchemaReply{},
		batch:       &collectorrpc.GetIngestBatchReply{},
		batchLs:     &collectorrpc.ListIngestBatchesReply{},
		record:      &collectorrpc.GetEventRecordReply{},
		recordLs:    &collectorrpc.ListEventRecordsReply{},
		deadLs:      &collectorrpc.ListDeadLettersReply{},
		activeReply: &collectorrpc.DispatchPolicyReply{},
		policies:    &collectorrpc.ListDispatchPoliciesReply{},
		health:      &collectorrpc.GetCollectorHealthReply{},
		retry:       &collectorrpc.RetryPendingDeliveryReply{},
		replay:      &collectorrpc.ReplayDeadLetterReply{},
		upsert:      &collectorrpc.DispatchPolicyReply{},
		activate:    &collectorrpc.DispatchPolicyReply{},
	}
	ctx := collectorSessionCtx()
	for _, rt := range collectorRoutes() {
		var body any
		if rt.body != nil {
			body = rt.body()
		}
		err := rt.call(ctx, collectorSvc(f), body)
		if !errors.Is(err, errCollectorFakeDownstream) {
			t.Fatalf("%s 把下游错误改写了：%v", rt.name, err)
		}
	}
	if f.calls != 13 {
		t.Fatalf("下游调用数应为 13，实际 %d", f.calls)
	}
}

func TestCollectorWriteRoutesRequireSessionIdentity(t *testing.T) {
	f := &collectorAdminFake{
		retry: &collectorrpc.RetryPendingDeliveryReply{}, replay: &collectorrpc.ReplayDeadLetterReply{},
		upsert: &collectorrpc.DispatchPolicyReply{}, activate: &collectorrpc.DispatchPolicyReply{},
	}
	for _, rt := range collectorRoutes() {
		if !rt.write {
			continue
		}
		// context.Background()：拿不到会话身份，说明这条路由没被 AdminPermission 保护（表/挂载漂移），
		// logic 必须 fail-closed 而不是照发下游。
		err := rt.call(context.Background(), collectorSvc(f), rt.body())
		if !errors.Is(err, errCollectorSessionRequired) {
			t.Fatalf("%s 缺会话身份时应拒绝，got %v", rt.name, err)
		}
		if !strings.Contains(err.Error(), "session identity required") {
			t.Fatalf("%s 错误消息不可定位：%v", rt.name, err)
		}
	}
	if f.calls != 0 {
		t.Fatalf("无会话身份却打了下游 %d 次", f.calls)
	}
}

func TestCollectorReadRoutesDoNotRequireSession(t *testing.T) {
	// 读面不挂 AdminPermission（避免操作读放大），因此没有会话身份也必须能读；
	// 这同时钉住「读路由不写操作者」——网关不会凭空造一个 operator 传给服务。
	f := &collectorAdminFake{
		validate: &collectorrpc.ValidateEventSchemaReply{}, batch: &collectorrpc.GetIngestBatchReply{},
		batchLs: &collectorrpc.ListIngestBatchesReply{}, record: &collectorrpc.GetEventRecordReply{},
		recordLs: &collectorrpc.ListEventRecordsReply{}, deadLs: &collectorrpc.ListDeadLettersReply{},
		activeReply: &collectorrpc.DispatchPolicyReply{}, policies: &collectorrpc.ListDispatchPoliciesReply{},
		health: &collectorrpc.GetCollectorHealthReply{},
	}
	var reads int
	for _, rt := range collectorRoutes() {
		if rt.write {
			continue
		}
		var body any
		if rt.body != nil {
			body = rt.body()
		}
		if err := rt.call(context.Background(), collectorSvc(f), body); err != nil {
			t.Fatalf("读入口 %s 不该要求会话身份，got %v", rt.name, err)
		}
		reads++
	}
	if reads != 9 || f.calls != 9 {
		t.Fatalf("读调用数应为 9，实际 routes=%d calls=%d", reads, f.calls)
	}
}

func TestCollectorWriteRoutesRequireNonBlankKeys(t *testing.T) {
	f := &collectorAdminFake{}
	ctx := collectorSessionCtx()
	cases := []struct{ route, field, want string }{
		{"collectorDeliveryRetry", "IdempotencyKey", "idempotency_key"},
		{"collectorDeadLetterReplay", "IdempotencyKey", "idempotency_key"},
		{"collectorDeadLetterReplay", "Reason", "reason"},
		{"collectorPolicyUpsert", "IdempotencyKey", "idempotency_key"},
		{"collectorPolicyUpsert", "Version", "version"},
		{"collectorPolicyUpsert", "SaltRef", "salt_ref"},
		{"collectorPolicyActivate", "IdempotencyKey", "idempotency_key"},
		{"collectorPolicyActivate", "Version", "version"},
		{"collectorPolicyActivate", "Reason", "reason"},
	}
	for _, tc := range cases {
		rt := collectorRouteByName(t, tc.route)
		body := rt.body()
		collectorBlank(t, body, tc.field)
		err := rt.call(ctx, collectorSvc(f), body)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s 应拒绝空白的 %s，got %v", tc.route, tc.want, err)
		}
		if !strings.HasPrefix(err.Error(), "gateway/admin:") {
			t.Fatalf("%s 错误消息应带 gateway/admin 前缀以便定位，got %v", tc.route, err)
		}
	}
	if f.calls != 0 {
		t.Fatalf("空白键却打了下游 %d 次", f.calls)
	}
}

func TestCollectorSubjectKeyRequired(t *testing.T) {
	f := &collectorAdminFake{}
	s := collectorSvc(f)
	for _, tc := range []struct {
		route string
		set   func(t *testing.T, body any)
	}{
		{"collectorBatchGet", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorBatchGet](t, b).BatchId = "   "
		}},
		{"collectorEventGet", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorEventGet](t, b).EventId = ""
		}},
	} {
		rt := collectorRouteByName(t, tc.route)
		body := rt.body()
		tc.set(t, body)
		if err := rt.call(context.Background(), s, body); !errors.Is(err, errCollectorSubjectRequired) {
			t.Fatalf("%s 主体键为空时应拒绝，got %v", rt.name, err)
		}
	}
	// 主体键为空时下游会按空主键查一条不存在的行，回「未命中」而不是「你少传了参数」，
	// 所以这里必须一次调用都没有。
	if f.calls != 0 {
		t.Fatalf("主体键为空却打了下游 %d 次", f.calls)
	}
}

func TestCollectorSubjectKeyIsNeverRewritten(t *testing.T) {
	f := &collectorAdminFake{batch: &collectorrpc.GetIngestBatchReply{}}
	body := collectorBatchGetBody()
	body.BatchId = " batch-1 " // 前后空白：只用来判空，不改写原值（截断/归一化就等于换了一个键）
	if _, err := NewCollectorBatchGetLogic(context.Background(), collectorSvc(f)).CollectorBatchGet(body); err != nil {
		t.Fatalf("CollectorBatchGet: %v", err)
	}
	if got := f.batchReq.GetBatchId(); got != " batch-1 " {
		t.Fatalf("batch_id 被网关改写：%q", got)
	}
}

func TestCollectorDeadLetterReplayIDsMustBePositive(t *testing.T) {
	f := &collectorAdminFake{}
	ctx := collectorSessionCtx()
	// 空列表在下游只会变成「处置了 0 条」的成功结论，白占一个幂等键还看不出错。
	empty := collectorReplayBody()
	empty.DeadLetterIds = nil
	if _, err := NewCollectorDeadLetterReplayLogic(ctx, collectorSvc(f)).CollectorDeadLetterReplay(empty); !errors.Is(err, errCollectorDeadLetterIDsRequired) {
		t.Fatalf("空 ID 列表应拒绝，got %v", err)
	}
	for _, ids := range [][]int64{{0}, {-3, 12}} {
		b := collectorReplayBody()
		b.DeadLetterIds = ids
		_, err := NewCollectorDeadLetterReplayLogic(ctx, collectorSvc(f)).CollectorDeadLetterReplay(b)
		if err == nil || !strings.Contains(err.Error(), "dead_letter_ids must be > 0") {
			t.Fatalf("ID %v 应拒绝，got %v", ids, err)
		}
	}
	if f.calls != 0 {
		t.Fatalf("被拒的重放请求打了下游 %d 次", f.calls)
	}
}

// TestCollectorShapeGates 固定「网关只做形态门槛」这条边界：下面断言的全是形态/符号问题
// （负数、时间窗倒置、空白必填位），不含任何领域结论（页大小上限、cursor 语法、90 天窗口、
// 无界扫描拒绝、采样基点区间、版本格式、状态机合法性都在 services/event-collector）。
func TestCollectorShapeGates(t *testing.T) {
	cases := []struct {
		name   string
		route  string
		mutate func(t *testing.T, body any)
		want   string
	}{
		{"批次窗口倒置", "collectorBatchList", func(t *testing.T, b any) {
			p := collectorCast[*types.ParamCollectorBatchList](t, b)
			p.CtimeFrom, p.CtimeTo = 200, 100
		}, "ctime_from must be <= ctime_to"},
		{"批次窗口起点为负", "collectorBatchList", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorBatchList](t, b).CtimeFrom = -1
		}, "ctime_from must be >= 0"},
		{"批次窗口终点为负", "collectorBatchList", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorBatchList](t, b).CtimeTo = -1
		}, "ctime_to must be >= 0"},
		{"批次页大小为负", "collectorBatchList", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorBatchList](t, b).PageSize = -1
		}, "page_size must be >= 0"},
		{"批次 mid 为负", "collectorBatchList", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorBatchList](t, b).Mid = -1
		}, "mid must be >= 0"},
		{"事件窗口倒置", "collectorEventList", func(t *testing.T, b any) {
			p := collectorCast[*types.ParamCollectorEventList](t, b)
			p.CtimeFrom, p.CtimeTo = 200, 100
		}, "ctime_from must be <= ctime_to"},
		{"事件 decision 为负", "collectorEventList", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorEventList](t, b).Decision = -1
		}, "decision must be >= 0"},
		{"事件 reason 为负", "collectorEventList", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorEventList](t, b).Reason = -1
		}, "reason must be >= 0"},
		{"事件 delivery_state 为负", "collectorEventList", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorEventList](t, b).DeliveryState = -1
		}, "delivery_state must be >= 0"},
		{"事件 category 为负", "collectorEventList", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorEventList](t, b).Category = -1
		}, "category must be >= 0"},
		{"死信窗口为负", "collectorDeadLetterList", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorDeadLetterList](t, b).CtimeFrom = -5
		}, "ctime_from must be >= 0"},
		{"死信页大小为负", "collectorDeadLetterList", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorDeadLetterList](t, b).PageSize = -5
		}, "page_size must be >= 0"},
		{"策略状态为负", "collectorPolicyList", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorPolicyList](t, b).State = -1
		}, "state must be >= 0"},
		{"策略页大小为负", "collectorPolicyList", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorPolicyList](t, b).PageSize = -1
		}, "page_size must be >= 0"},
		{"推进 now 为负", "collectorDeliveryRetry", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorDeliveryRetry](t, b).Now = -1
		}, "now must be >= 0"},
		{"推进 limit 为负", "collectorDeliveryRetry", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorDeliveryRetry](t, b).Limit = -1
		}, "limit must be >= 0"},
		{"策略 salt_version 为负", "collectorPolicyUpsert", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorPolicyUpsert](t, b).SaltVersion = -1
		}, "salt_version must be >= 0"},
		{"策略 retention_days 为负", "collectorPolicyUpsert", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorPolicyUpsert](t, b).RetentionDays = -1
		}, "retention_days must be >= 0"},
		{"策略 retry_max_seconds 为负", "collectorPolicyUpsert", func(t *testing.T, b any) {
			collectorCast[*types.ParamCollectorPolicyUpsert](t, b).RetryMaxSeconds = -1
		}, "retry_max_seconds must be >= 0"},
		{"采样规则基点为负", "collectorPolicyUpsert", func(t *testing.T, b any) {
			p := collectorCast[*types.ParamCollectorPolicyUpsert](t, b)
			p.SampleRules[0].SampleBps = -1
		}, "sample_rules.sample_bps must be >= 0"},
		{"采样规则事件名为空白", "collectorPolicyUpsert", func(t *testing.T, b any) {
			p := collectorCast[*types.ParamCollectorPolicyUpsert](t, b)
			p.SampleRules[1].EventType = "   "
		}, "sample_rules.event_type required"},
	}
	ctx := collectorSessionCtx()
	for _, tc := range cases {
		rt := collectorRouteByName(t, tc.route)
		f := &collectorAdminFake{}
		body := rt.body()
		tc.mutate(t, body)
		err := rt.call(ctx, collectorSvc(f), body)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s：%s 期望错误含 %q，got %v", tc.name, tc.route, tc.want, err)
		}
		if !strings.HasPrefix(err.Error(), "gateway/admin:") {
			t.Fatalf("%s：错误消息应带 gateway/admin 前缀以便定位，got %v", tc.name, err)
		}
		if f.calls != 0 {
			t.Fatalf("%s：形态不过却打了下游 %d 次", tc.name, f.calls)
		}
	}
}

// TestCollectorZeroIsInheritedSentinelNotRejected 钉住 0 在本域的语义：
// now=0 用服务端当前时间、limit=0 用 Dispatch.BatchSize、page_size=0 用默认页大小、
// ctime_*=0 表示该侧不设界、mid=0 是游客。把它们当「未填」而代填默认值，
// 就等于替调用方决定了一份他自己没要的读数。
func TestCollectorZeroIsInheritedSentinelNotRejected(t *testing.T) {
	f := &collectorAdminFake{
		batchLs:  &collectorrpc.ListIngestBatchesReply{},
		retry:    &collectorrpc.RetryPendingDeliveryReply{},
		records:  nil,
		recordLs: &collectorrpc.ListEventRecordsReply{},
	}
	s := collectorSvc(f)
	zeroList := &types.ParamCollectorBatchList{}
	if _, err := NewCollectorBatchListLogic(context.Background(), s).CollectorBatchList(zeroList); err != nil {
		t.Fatalf("全零批次查询应通过：%v", err)
	}
	if f.batches.GetPageSize() != 0 || f.batches.GetCtimeFrom() != 0 || f.batches.GetCtimeTo() != 0 {
		t.Fatalf("网关代填了默认值：%+v", f.batches)
	}
	zeroRetry := &types.ParamCollectorDeliveryRetry{IdempotencyKey: "idem-0", Topic: "   "}
	if _, err := NewCollectorDeliveryRetryLogic(collectorSessionCtx(), s).CollectorDeliveryRetry(zeroRetry); err != nil {
		t.Fatalf("now/limit 为 0 的推进应通过（0 = 用服务当前时间与默认批量）：%v", err)
	}
	if f.retryReq.GetNow() != 0 || f.retryReq.GetLimit() != 0 || f.retryReq.GetTopic() != "   " {
		t.Fatalf("now/limit/topic 被改写：%+v", f.retryReq)
	}
	zeroEvents := &types.ParamCollectorEventList{}
	if _, err := NewCollectorEventListLogic(context.Background(), s).CollectorEventList(zeroEvents); err != nil {
		t.Fatalf("全零事件查询应通过（无界扫描的拒绝在服务侧）：%v", err)
	}
	if f.records.GetMid() != 0 {
		t.Fatal("mid=0（游客）被改写了")
	}
}

// TestCollectorWriteSessionGateRunsBeforeShapeGate 证明门槛顺序：
// 客户端未配置 → 请求体缺失 → 会话身份 → 形态。
// 无会话时必须先报鉴权问题，不能让参数错误盖掉 fail-closed 的鉴权结论。
func TestCollectorWriteSessionGateRunsBeforeShapeGate(t *testing.T) {
	f := &collectorAdminFake{}
	rt := collectorRouteByName(t, "collectorPolicyActivate")
	body := collectorPolicyActivateBody()
	body.Version, body.Reason, body.IdempotencyKey = "", "", ""
	err := rt.call(context.Background(), collectorSvc(f), body)
	if !errors.Is(err, errCollectorSessionRequired) {
		t.Fatalf("应先拒会话身份，got %v", err)
	}
	if err = rt.call(collectorSessionCtx(), collectorSvc(f), body); err == nil ||
		!strings.Contains(err.Error(), "version required") {
		t.Fatalf("有会话时应报 version 空白，got %v", err)
	}
	if f.calls != 0 {
		t.Fatalf("被拒的写请求打了下游 %d 次", f.calls)
	}
}

// assertJSONFreeOf 断言响应序列化后不含给定的明文片段（隐私边界的机器可检表达）。
func assertJSONFreeOf(t *testing.T, resp any, banned []string) {
	t.Helper()
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("响应无法序列化：%v", err)
	}
	for _, s := range banned {
		if s == "" {
			continue
		}
		if strings.Contains(string(raw), s) {
			t.Fatalf("响应里出现了明文片段 %q（AGENTS.md §7）：%s", s, raw)
		}
	}
}
