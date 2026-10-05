package logic

// 测试基座：与 services/ops-config/internal/logic/testsupport_test.go 同族的本仓写法。
// 断言一律「真值 + 差值」：计数用调用前后的差，行集用业务键探针，
// 被拒请求必须零副作用（AGENTS.md §9）。

import (
	"context"
	"errors"
	"testing"
	"time"

	"go-video/services/event-collector/internal/config"
	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

func bg() context.Context { return context.Background() }

func timeNowUnix() int64 { return time.Now().Unix() }

// wantErr 只认 errors.Is：断言的是「同一条哨兵」，不是字符串巧合。
func wantErr(t *testing.T, err error, target error, label string) {
	t.Helper()
	if target == nil {
		if err != nil {
			t.Fatalf("%s: 期望成功，实际 err=%v", label, err)
		}
		return
	}
	if !errors.Is(err, target) {
		t.Fatalf("%s: 期望 %v，实际 %v", label, target, err)
	}
}

// wantFail 比 wantErr 多一档：把「失败被伪造成成功」当成最坏的一类通过。
func wantFail(t *testing.T, err error, target error, label string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: 期望失败并抛 %v，实际返回 nil —— 失败被伪造成功是最坏的一类通过", label, target)
	}
	wantErr(t, err, target, label)
}

func wantOK[T any](t *testing.T, got T, err error, label string) T {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: 期望成功，实际 err=%v", label, err)
	}
	return got
}

func mustReply(t *testing.T, reply any, label string) {
	t.Helper()
	if reply == nil {
		t.Fatalf("%s: 返回了 nil reply", label)
	}
}

func copyOf[T any](p *T) *T {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// ------------------------------------------------------------ 副作用快照

// tableRows 是 effectCounts.rows 的下标口径（五张台账表）。
const (
	tblBatches = iota
	tblRecords
	tblPolicies
	tblPending
	tblDead
)

type effectCounts struct {
	rows  [5]int
	sends int
}

func (e *testEnv) effects() effectCounts {
	var c effectCounts
	c.rows[tblBatches] = len(e.db.batches)
	c.rows[tblRecords] = len(e.db.records)
	c.rows[tblPolicies] = len(e.db.policies)
	c.rows[tblPending] = len(e.db.pending)
	c.rows[tblDead] = len(e.db.dead)
	c.sends = len(e.sender.calls)
	return c
}

// requireSameEffects 断言「被拒/失败请求零副作用」：行集与外呼一次都没多。
// 事务 begin 不计入：回滚的事务本就不该留下行。
func (e *testEnv) requireSameEffects(t *testing.T, before effectCounts, label string) {
	t.Helper()
	got := e.effects()
	if got.rows != before.rows {
		t.Fatalf("%s: 失败请求写出了行 before=%v after=%v", label, before.rows, got.rows)
	}
	if got.sends != before.sends {
		t.Fatalf("%s: 失败请求触达了下游 sender before=%d after=%d", label, before.sends, got.sends)
	}
}

func (e *testEnv) call(op string) int { return e.db.calls[op] }

func (e *testEnv) callsOf(ops ...string) int {
	var n int
	for _, op := range ops {
		n += e.db.calls[op]
	}
	return n
}

// ------------------------------------------------------------ 业务键探针

func (e *testEnv) mustBatch(t *testing.T, batchID string) *model.IngestBatch {
	t.Helper()
	row, ok := e.db.byBatchID(batchID)
	if !ok {
		t.Fatalf("批次行不存在 batch_id=%s（用例前提失效）", batchID)
	}
	return row
}

func (e *testEnv) mustRecord(t *testing.T, eventID string) *model.EventRecord {
	t.Helper()
	row, ok := e.db.byEventID(eventID)
	if !ok {
		t.Fatalf("事件台账行不存在 event_id=%s（用例前提失效）", eventID)
	}
	return row
}

func (e *testEnv) mustPending(t *testing.T, eventID, topic string) *model.PendingDelivery {
	t.Helper()
	row, ok := e.db.byEventTopic(eventID, topic)
	if !ok {
		t.Fatalf("待投递行不存在 event_id=%s topic=%s（Outbox 漏写）", eventID, topic)
	}
	return row
}

func (e *testEnv) mustPolicy(t *testing.T, version string) *model.DispatchPolicy {
	t.Helper()
	row, ok := e.db.byVersion(version)
	if !ok {
		t.Fatalf("策略行不存在 version=%s（用例前提失效）", version)
	}
	return row
}

func (e *testEnv) mustDead(t *testing.T, id int64) *model.DeadLetter {
	t.Helper()
	row, ok := e.db.dead[id]
	if !ok {
		t.Fatalf("死信行不存在 id=%d（用例前提失效）", id)
	}
	return row
}

func (e *testEnv) activePolicies() []*model.DispatchPolicy {
	var out []*model.DispatchPolicy
	for _, p := range e.db.policies {
		if p.State == model.PolicyStateActive {
			out = append(out, p)
		}
	}
	return out
}

func (e *testEnv) mustOneActive(t *testing.T, label string) *model.DispatchPolicy {
	t.Helper()
	rows := e.activePolicies()
	if len(rows) != 1 {
		t.Fatalf("%s: ACTIVE 策略必须恰好一版（uniq_active），实际 %d 版", label, len(rows))
	}
	return rows[0]
}

// pendingOf 按 event_id 找全部投递行（一条事件可能投多个 topic）。
func (e *testEnv) pendingOf(eventID string) []*model.PendingDelivery {
	var out []*model.PendingDelivery
	for _, p := range e.db.pending {
		if p.EventID == eventID {
			out = append(out, p)
		}
	}
	return out
}

func (e *testEnv) recordsOfBatch(batchID string) []*model.EventRecord {
	var out []*model.EventRecord
	for _, r := range e.db.records {
		if r.BatchID == batchID {
			out = append(out, r)
		}
	}
	return out
}

// ------------------------------------------------------------ 种子

// withClock 在指定时间戳下写入种子行（分页/留存用例需要可预期的 ctime）。
func (e *testEnv) withClock(ts int64, fn func()) {
	old := e.db.clock
	e.db.clock = ts
	defer func() { e.db.clock = old }()
	fn()
}

func (e *testEnv) seedBatch(t *testing.T, b *model.IngestBatch) *model.IngestBatch {
	t.Helper()
	cp := *b
	if cp.Ctime != 0 {
		e.withClock(cp.Ctime, func() {
			if _, _, err := e.svc.Batches.Insert(bg(), nil, &cp); err != nil {
				t.Fatalf("种子批次写入失败: %v", err)
			}
		})
	} else if _, _, err := e.svc.Batches.Insert(bg(), nil, &cp); err != nil {
		t.Fatalf("种子批次写入失败: %v", err)
	}
	row := e.mustBatch(t, b.BatchID)
	if b.State != 0 && b.State != row.State {
		if _, err := e.svc.Batches.MarkState(bg(), nil, b.BatchID,
			[]int32{model.BatchStateReceived, model.BatchStateValidated, model.BatchStateDispatching,
				model.BatchStatePartial, model.BatchStateDispatched}, b.State, b.TopReason, b.LastError); err != nil {
			t.Fatalf("种子批次置态失败: %v", err)
		}
	}
	return e.mustBatch(t, b.BatchID)
}

func (e *testEnv) seedRecord(t *testing.T, r *model.EventRecord) *model.EventRecord {
	t.Helper()
	cp := *r
	if cp.BatchID == "" {
		cp.BatchID = "seed-batch"
	}
	write := func() {
		if _, err := e.svc.Records.InsertIgnoreMany(bg(), nil, []*model.EventRecord{&cp}); err != nil {
			t.Fatalf("种子事件写入失败: %v", err)
		}
	}
	if cp.Ctime != 0 {
		e.withClock(cp.Ctime, write)
	} else {
		write()
	}
	return e.mustRecord(t, r.EventID)
}

func (e *testEnv) seedPending(t *testing.T, p *model.PendingDelivery) *model.PendingDelivery {
	t.Helper()
	cp := *p
	write := func() {
		if _, err := e.svc.Pending.InsertIgnore(bg(), nil, &cp); err != nil {
			t.Fatalf("种子待投递行写入失败: %v", err)
		}
	}
	if cp.Ctime != 0 {
		e.withClock(cp.Ctime, write)
	} else {
		write()
	}
	row := e.mustPending(t, p.EventID, p.Topic)
	if p.State != 0 && p.State != row.State {
		row.State = p.State
	}
	row.Attempts = p.Attempts
	row.NextRetryAt = p.NextRetryAt
	row.LeaseOwner = p.LeaseOwner
	row.LeaseUntil = p.LeaseUntil
	row.SentAt = p.SentAt
	return row
}

func (e *testEnv) seedDead(t *testing.T, d *model.DeadLetter) *model.DeadLetter {
	t.Helper()
	cp := *d
	write := func() {
		if _, err := e.svc.DeadLetters.InsertIgnore(bg(), nil, &cp); err != nil {
			t.Fatalf("种子死信写入失败: %v", err)
		}
	}
	if cp.Ctime != 0 {
		e.withClock(cp.Ctime, write)
	} else {
		write()
	}
	row := e.mustDeadByEventTopic(t, d.EventID, d.Topic)
	if d.State != "" {
		row.State = d.State
	}
	row.Attempts = d.Attempts
	row.HandledAt = d.HandledAt
	row.Operator = d.Operator
	return row
}

func (e *testEnv) mustDeadByEventTopic(t *testing.T, eventID, topic string) *model.DeadLetter {
	t.Helper()
	for _, v := range e.db.dead {
		if v.EventID == eventID && v.Topic == topic {
			return v
		}
	}
	t.Fatalf("死信行不存在 event_id=%s topic=%s（用例前提失效）", eventID, topic)
	return nil
}

// seedPolicy 走真实 InsertDraft 路径（含 checkPolicyShape），再按需置态。
func (e *testEnv) seedPolicy(t *testing.T, p *model.DispatchPolicy, state int32) *model.DispatchPolicy {
	t.Helper()
	cp := *p
	cp.State = model.PolicyStateDraft
	if cp.Ctime != 0 {
		e.withClock(cp.Ctime, func() {
			if _, _, err := e.svc.Policies.InsertDraft(bg(), nil, &cp); err != nil {
				t.Fatalf("种子策略写入失败: %v", err)
			}
		})
	} else if _, _, err := e.svc.Policies.InsertDraft(bg(), nil, &cp); err != nil {
		t.Fatalf("种子策略写入失败: %v", err)
	}
	row := e.mustPolicy(t, p.Version)
	switch state {
	case 0:
	case model.PolicyStateDraft:
	case model.PolicyStateActive:
		e.forceActive(t, p.Version)
	default:
		row.State = state
	}
	return e.mustPolicy(t, p.Version)
}

// forceActive 直接置 ACTIVE（测试前提，不等价于业务路径：业务路径必须走 Activate）。
func (e *testEnv) forceActive(t *testing.T, version string) {
	t.Helper()
	row, ok := e.db.byVersion(version)
	if !ok {
		t.Fatalf("策略版本不存在 %s", version)
	}
	for _, p := range e.db.policies {
		if p.State == model.PolicyStateActive {
			p.State = model.PolicyStateArchived
		}
	}
	row.State = model.PolicyStateActive
}

// newPolicyDraft 给一版能通过 model 侧校验的合法草稿。
func newPolicyDraft(version string) *model.DispatchPolicy {
	return &model.DispatchPolicy{
		Version:              version,
		SampleRulesJSON:      "[]",
		SaltVersion:          1,
		SaltRef:              "EVENT_COLLECTOR_SALT_V1",
		FieldWhitelistJSON:   "[]",
		DropFieldsJSON:       "[]",
		MaxEventsPerBatch:    100,
		MaxRequestBytes:      1 << 20,
		MaxEventPayloadBytes: 4096,
		MaxClockSkewSeconds:  300,
		MaxBackfillSeconds:   3600,
		KeywordMaxRunes:      32,
		RetentionDays:        30,
		DeliverMaxAttempts:   3,
		RetryBaseSeconds:     30,
		RetryMaxSeconds:      600,
		Note:                 "test draft " + version,
		Operator:             "tester",
	}
}

// seedActivePolicy 落一版无采样规则（等价于按 config 默认全量）的 ACTIVE 策略。
func (e *testEnv) seedActivePolicy(t *testing.T, version string) *model.DispatchPolicy {
	t.Helper()
	return e.seedActivePolicyWithSample(t, version, nil)
}

// seedActivePolicyWithSample 落一版带采样规则的 ACTIVE 策略。
func (e *testEnv) seedActivePolicyWithSample(t *testing.T, version string,
	rules []model.SampleRule) *model.DispatchPolicy {
	t.Helper()
	raw, err := model.EncodeSampleRules(rules)
	requireNoError(t, err, "编码采样规则")
	p := newPolicyDraft(version)
	p.SampleRulesJSON = raw
	return e.seedPolicy(t, p, model.PolicyStateActive)
}

// ------------------------------------------------------------ 环境

func testConfig() config.Config {
	c := config.Config{
		CacheRedis: redis.RedisConf{Type: "node", Host: "127.0.0.1:6379"},
		DataSource: "test:test@tcp(127.0.0.1:3306)/test",
		Privacy: config.PrivacyConf{
			SaltRef:     "EVENT_COLLECTOR_SALT_V1",
			SaltVersion: 1,
			SaltValue:   "unit-test-salt-0123456789abcdef",
		},
		Collector: config.CollectorConf{
			PageSize:                20,
			MaxPageSize:             200,
			MaxEventsPerBatch:       200,
			MaxRequestBytes:         1 << 20,
			MaxEventPayloadBytes:    4096,
			MaxClockSkewSeconds:     300,
			MaxBackfillSeconds:      86400,
			KeywordMaxRunes:         32,
			DefaultSampleBps:        10000,
			SupportedSchemaVersion:  1,
			IPSegmentBits:           24,
			MidQps:                  200,
			DeviceQps:               200,
			IPSegmentQps:            2000,
			GlobalQps:               2000,
			GlobalBurst:             800,
			DegradedRejectRatioBps:  0,
			ReportIntervalSeconds:   10,
			RetryHintMs:             1000,
			RetentionDays:           30,
			DeadLetterRetentionDays: 90,
			BatchLimit:              200,
			MaxReplayPerRequest:     100,
		},
		Dispatch: config.DispatchConf{
			BatchSize:                 50,
			DeliverMaxAttempts:        3,
			RetryBaseSeconds:          30,
			RetryMaxSeconds:           600,
			LeaseSeconds:              60,
			RetryScanLookaheadSeconds: 86400,
		},
	}
	return c
}

type testEnv struct {
	t      *testing.T
	cfg    config.Config
	svc    *svc.ServiceContext
	db     *fakeDB
	conn   *fakeConn
	lim    *fakeLimiter
	sender *fakeSender
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	cfg := testConfig()
	db := newFakeDB()
	conn := &fakeConn{db: db}
	lim := &fakeLimiter{}
	sender := &fakeSender{errFor: map[string]error{}}
	e := &testEnv{
		t:      t,
		cfg:    cfg,
		db:     db,
		conn:   conn,
		lim:    lim,
		sender: sender,
		svc: &svc.ServiceContext{
			Config:        cfg,
			DB:            conn,
			Cache:         nil, // *redis.Redis 是具体类型：一律跑「缓存不可用」的降级形态
			Batches:       batchesModel{db},
			Records:       recordsModel{db},
			Policies:      policiesModel{db},
			Pending:       pendingModel{db},
			DeadLetters:   deadModel{db},
			GlobalLimiter: lim,
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("testConfig 未通过启动期自检，用例前提本身就是坏的: %v", err)
	}
	t.Cleanup(func() { e.db.commitErr = nil })
	return e
}

// withConfig 在替换配置后重建 ServiceContext（保留同一个内存库）。
func (e *testEnv) withConfig(mut func(*config.Config)) {
	mut(&e.cfg)
	e.svc.Config = e.cfg
}

func (e *testEnv) noSalt() {
	e.cfg.Privacy.SaltValue = ""
	e.svc.Config = e.cfg
}

func captureLogs(t *testing.T) *fakeLogWriter {
	t.Helper()
	w := &fakeLogWriter{}
	logx.AddWriter(w)
	return w
}

// ------------------------------------------------------------ 请求构造

func nowTS() int64 { return time.Now().Unix() }

func validCtx() *rpc.EventContext {
	return &rpc.EventContext{
		Mid:        10086,
		DeviceId:   "plain-device-id-8827",
		DeviceType: "oaid",
		Ip:         "203.0.113.77",
		Platform:   rpc.Platform_PLATFORM_ANDROID,
		AppId:      "com.hilihili.test",
		AppVersion: "1.2.3",
		SdkVersion: "0.9.1",
	}
}

func serverCtx() *rpc.EventContext {
	return &rpc.EventContext{
		Mid:      10086,
		Platform: rpc.Platform_PLATFORM_SERVER,
		AppId:    "server",
	}
}

// playEvent 是一条「应当被受理」的最小事件。
func playEvent(eventID string) *rpc.BehaviorEvent {
	return &rpc.BehaviorEvent{
		EventId:       eventID,
		EventType:     "behavior.play",
		Category:      rpc.BehaviorCategory_BEHAVIOR_CATEGORY_PLAY,
		SchemaVersion: 1,
		OccurredAt:    nowTS(),
		ContentId:     4242,
		PositionMs:    1000,
		DurationMs:    60000,
		Payload:       `{"play_duration_ms":1000,"source":"feed"}`,
	}
}

func clickEvent(eventID string) *rpc.BehaviorEvent {
	return &rpc.BehaviorEvent{
		EventId:       eventID,
		EventType:     "behavior.click",
		Category:      rpc.BehaviorCategory_BEHAVIOR_CATEGORY_CLICK,
		SchemaVersion: 1,
		OccurredAt:    nowTS(),
		ContentId:     4242,
		Payload:       `{"result_index":1}`,
	}
}

func qualityEvent(eventID string) *rpc.BehaviorEvent {
	return &rpc.BehaviorEvent{
		EventId:       eventID,
		EventType:     "behavior.quality",
		Category:      rpc.BehaviorCategory_BEHAVIOR_CATEGORY_QUALITY,
		SchemaVersion: 1,
		OccurredAt:    nowTS(),
		SessionId:     "sess-1",
		BufferCount:   2,
		FirstFrameMs:  320,
		Payload:       `{"buffer_count":2}`,
	}
}

func searchEvent(eventID, keyword string) *rpc.BehaviorEvent {
	return &rpc.BehaviorEvent{
		EventId:       eventID,
		EventType:     "behavior.search",
		Category:      rpc.BehaviorCategory_BEHAVIOR_CATEGORY_SEARCH,
		SchemaVersion: 1,
		OccurredAt:    nowTS(),
		Keyword:       keyword,
		Payload:       `{"result_count":10}`,
	}
}

func collectReq(batchID string, events ...*rpc.BehaviorEvent) *rpc.CollectEventsReq {
	return &rpc.CollectEventsReq{
		BatchId: batchID,
		Source:  rpc.Source_SOURCE_CLIENT,
		Context: validCtx(),
		Events:  events,
	}
}

func serverReq(batchID, caller, idemKey string, events ...*rpc.BehaviorEvent) *rpc.IngestServerEventsReq {
	return &rpc.IngestServerEventsReq{
		BatchId:        batchID,
		CallerService:  caller,
		IdempotencyKey: idemKey,
		Context:        serverCtx(),
		Events:         events,
		TraceId:        "trace-server-batch-1",
	}
}

// ------------------------------------------------------------ 调用入口

func (e *testEnv) collect(req *rpc.CollectEventsReq) (*rpc.CollectEventsReply, error) {
	return NewCollectEventsLogic(bg(), e.svc).CollectEvents(req)
}

func (e *testEnv) ingestServer(req *rpc.IngestServerEventsReq) (*rpc.IngestServerEventsReply, error) {
	return NewIngestServerEventsLogic(bg(), e.svc).IngestServerEvents(req)
}

func (e *testEnv) validate(in *rpc.ValidateEventSchemaReq) (*rpc.ValidateEventSchemaReply, error) {
	return NewValidateEventSchemaLogic(bg(), e.svc).ValidateEventSchema(in)
}

func (e *testEnv) retry(in *rpc.RetryPendingDeliveryReq) (*rpc.RetryPendingDeliveryReply, error) {
	return NewRetryPendingDeliveryLogic(bg(), e.svc).RetryPendingDelivery(in)
}

func (e *testEnv) replay(in *rpc.ReplayDeadLetterReq) (*rpc.ReplayDeadLetterReply, error) {
	return NewReplayDeadLetterLogic(bg(), e.svc).ReplayDeadLetter(in)
}

func (e *testEnv) upsert(in *rpc.UpsertDispatchPolicyReq) (*rpc.DispatchPolicyReply, error) {
	return NewUpsertDispatchPolicyLogic(bg(), e.svc).UpsertDispatchPolicy(in)
}

func (e *testEnv) activate(in *rpc.ActivateDispatchPolicyReq) (*rpc.DispatchPolicyReply, error) {
	return NewActivateDispatchPolicyLogic(bg(), e.svc).ActivateDispatchPolicy(in)
}

func (e *testEnv) activePolicy(in *rpc.GetActiveDispatchPolicyReq) (*rpc.DispatchPolicyReply, error) {
	return NewGetActiveDispatchPolicyLogic(bg(), e.svc).GetActiveDispatchPolicy(in)
}

func (e *testEnv) listPolicies(in *rpc.ListDispatchPoliciesReq) (*rpc.ListDispatchPoliciesReply, error) {
	return NewListDispatchPoliciesLogic(bg(), e.svc).ListDispatchPolicies(in)
}

func (e *testEnv) health(in *rpc.GetCollectorHealthReq) (*rpc.GetCollectorHealthReply, error) {
	return NewGetCollectorHealthLogic(bg(), e.svc).GetCollectorHealth(in)
}

func (e *testEnv) getBatch(in *rpc.GetIngestBatchReq) (*rpc.GetIngestBatchReply, error) {
	return NewGetIngestBatchLogic(bg(), e.svc).GetIngestBatch(in)
}

func (e *testEnv) listBatches(in *rpc.ListIngestBatchesReq) (*rpc.ListIngestBatchesReply, error) {
	return NewListIngestBatchesLogic(bg(), e.svc).ListIngestBatches(in)
}

func (e *testEnv) getRecord(in *rpc.GetEventRecordReq) (*rpc.GetEventRecordReply, error) {
	return NewGetEventRecordLogic(bg(), e.svc).GetEventRecord(in)
}

func (e *testEnv) listRecords(in *rpc.ListEventRecordsReq) (*rpc.ListEventRecordsReply, error) {
	return NewListEventRecordsLogic(bg(), e.svc).ListEventRecords(in)
}

func (e *testEnv) listDead(in *rpc.ListDeadLettersReq) (*rpc.ListDeadLettersReply, error) {
	return NewListDeadLettersLogic(bg(), e.svc).ListDeadLetters(in)
}

// retryWithSender 用注入的 fakeSender 直接驱动一轮 Outbox 推进（cron 侧入口）。
func retryWithSender(e *testEnv, sender pendingSender, topic string, now int64,
	limit int32, operator string) (*roundOutcome, error) {
	return runPendingRound(bg(), e.svc, sender, topic, now, limit, operator)
}

// ------------------------------------------------------------ 断言小工具

func requireTrue(t *testing.T, cond bool, label string) {
	t.Helper()
	if !cond {
		t.Fatalf("%s: 断言不成立", label)
	}
}

func requireEqual[T comparable](t *testing.T, got, want T, label string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: 期望 %v，实际 %v", label, want, got)
	}
}

func requireNoError(t *testing.T, err error, label string) {
	t.Helper()
	wantErr(t, err, nil, label)
}

var errInjected = errors.New("injected db failure")
