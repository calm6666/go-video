package logic

// 不变量 1（幂等）：同一个 batch_id / event_id 重放不产生第二份台账与 Outbox，
// 且必须回「已受理」（首次结论的回放），而不是「又收了一遍」。
// 真值约束在 MySQL 唯一键（uniq_batch_id / uniq_event_id），Redis 短路只是加速器，
// 因此这些用例全部在 Cache=nil 的形态下跑：幂等必须仍然成立。

import (
	"strings"
	"testing"

	"go-video/services/event-collector/internal/config"
	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"
)

func topicV1(eventType string) string { return eventType + ".v1" }

func TestCollectEvents_SameBatchIDReplayProducesNoSecondLedger(t *testing.T) {
	e := newTestEnv(t)
	req := collectReq("batch-idem-1", playEvent("evt-idem-a"), clickEvent("evt-idem-b"))

	rep1, err := e.collect(req)
	r1 := wantOK(t, rep1, err, "首次采集")
	requireEqual(t, r1.State, rpc.BatchState_BATCH_STATE_VALIDATED, "首次批次态")
	requireEqual(t, r1.Total, int32(2), "首次 total")
	requireEqual(t, r1.Accepted, int32(2), "首次 accepted")

	batch := e.mustBatch(t, "batch-idem-1")
	requireEqual(t, batch.Total, int32(2), "批次台账 total 只能累加一次")
	requireEqual(t, batch.Accepted, int32(2), "批次台账 accepted 只能累加一次")
	requireEqual(t, len(e.recordsOfBatch("batch-idem-1")), 2, "事件台账行数")
	requireEqual(t, len(e.db.pending), 2, "Outbox 行数")
	e.mustPending(t, "evt-idem-a", topicV1("behavior.play"))

	before := e.effects()
	callsBefore := e.call("records.InsertIgnoreMany")

	rep2, err := e.collect(req)
	r2 := wantOK(t, rep2, err, "同 batch_id 重放")
	requireEqual(t, r2.State, rpc.BatchState_BATCH_STATE_VALIDATED, "重放必须回首次的已受理态")
	requireEqual(t, r2.Total, int32(2), "重放 total 与首次一致")
	requireEqual(t, r2.Accepted, int32(2), "重放 accepted 不得二次累加")
	for _, res := range r2.Results {
		requireEqual(t, res.Decision, rpc.EventDecision_EVENT_DECISION_ACCEPTED, "重放逐条结论")
		if !strings.Contains(res.Message, "回放") {
			t.Fatalf("重放必须自报「这是回放」，实际 message=%q", res.Message)
		}
	}
	e.requireSameEffects(t, before, "同 batch_id 重放")
	if got := e.call("records.InsertIgnoreMany"); got != callsBefore {
		t.Fatalf("重放路径不能再次写台账：records.InsertIgnoreMany %d→%d 次", callsBefore, got)
	}
	requireEqual(t, len(e.db.pending), 2, "重放不得产生第二条 Outbox")
}

func TestCollectEvents_SameEventIDAcrossBatchesCountsDuplicated(t *testing.T) {
	e := newTestEnv(t)
	first, err := e.collect(collectReq("batch-dup-src", playEvent("evt-shared")))
	wantOK(t, first, err, "首批采集")

	before := e.effects()
	second, err := e.collect(collectReq("batch-dup-other", playEvent("evt-shared")))
	r2 := wantOK(t, second, err, "跨批同 event_id")
	requireEqual(t, r2.Duplicated, int32(1), "跨批重复必须计 DUPLICATED")
	requireEqual(t, r2.Accepted, int32(0), "重复事件不得再计 accepted")
	requireEqual(t, r2.Results[0].Decision, rpc.EventDecision_EVENT_DECISION_DUPLICATED, "逐条结论")
	// 这条请求本身是成功的：换了 batch_id 就是一个新批次，因此批次台账必须多一行（否则跨批
	// 重复这件事无处留痕）。requireSameEffects 是「被拒请求零副作用」的断言，打在这里是错的接缝。
	// 真正的跨批去重不变量在下面三条：事件行、Outbox 行、外呼一律不得增加。
	after := e.effects()
	if after.rows[tblBatches] != before.rows[tblBatches]+1 {
		t.Fatalf("跨批重复必须另记一条批次台账 before=%d after=%d",
			before.rows[tblBatches], after.rows[tblBatches])
	}
	after.rows[tblBatches] = before.rows[tblBatches]
	if after.rows != before.rows {
		t.Fatalf("跨批重复写出了事件/策略/Outbox/死信行 before=%v after=%v", before.rows, after.rows)
	}
	if after.sends != before.sends {
		t.Fatalf("跨批重复触达了 sender before=%d after=%d", before.sends, after.sends)
	}

	rec := e.mustRecord(t, "evt-shared")
	requireEqual(t, rec.BatchID, "batch-dup-src", "重复事件不得改写首次归属批次")
	if len(e.pendingOf("evt-shared")) != 1 {
		t.Fatalf("重复事件不得产生第二条 Outbox，实际 %d 条", len(e.pendingOf("evt-shared")))
	}
}

func TestCollectEvents_DuplicateEventIDInsideOneRequest(t *testing.T) {
	e := newTestEnv(t)
	rep, err := e.collect(collectReq("batch-indup",
		playEvent("evt-twin"), clickEvent("evt-other"), playEvent("evt-twin")))
	r := wantOK(t, rep, err, "同请求内重复 event_id")
	requireEqual(t, r.Total, int32(3), "total 按请求条数计")
	requireEqual(t, r.Accepted, int32(2), "accepted 只计首次出现")
	requireEqual(t, r.Duplicated, int32(1), "同请求内重复计 DUPLICATED")
	requireEqual(t, r.Results[2].Decision, rpc.EventDecision_EVENT_DECISION_DUPLICATED, "第三条结论")
	requireEqual(t, len(e.recordsOfBatch("batch-indup")), 2, "只落两条台账")
	requireEqual(t, len(e.db.pending), 2, "只入两条 Outbox")
}

func TestIngestServerEvents_IdempotencyKeyMustMatchStoredBatch(t *testing.T) {
	e := newTestEnv(t)
	first, err := e.ingestServer(serverReq("batch-srv-1", "engagement", "action-42", playEvent("evt-srv-1")))
	wantOK(t, first, err, "服务端首次上报")
	requireEqual(t, e.mustBatch(t, "batch-srv-1").IdempotencyKey, "action-42", "幂等键归因列")

	before := e.effects()
	_, err = e.ingestServer(serverReq("batch-srv-1", "engagement", "action-99", playEvent("evt-srv-1")))
	wantFail(t, err, model.ErrIdempotencyKeyConflict, "同 batch_id 换幂等键")
	e.requireSameEffects(t, before, "幂等键冲突请求")

	same, err := e.ingestServer(serverReq("batch-srv-1", "engagement", "action-42", playEvent("evt-srv-1")))
	r := wantOK(t, same, err, "同键重放")
	requireEqual(t, r.State, rpc.BatchState_BATCH_STATE_VALIDATED, "同键重放回首次已受理")
	requireEqual(t, r.Accepted, int32(1), "同键重放不二次累加")
	e.requireSameEffects(t, before, "同键重放")
}

func TestCollectEvents_ConcurrentBatchWriteRollsBackAndServesReplay(t *testing.T) {
	e := newTestEnv(t)
	// 并发对手已经把本批写完：本进程的 FindByBatchID 预检查不到（读偏序），
	// 事务内 Insert 撞 uniq_batch_id 必须整事务回滚并改走只读回放。
	e.seedBatch(t, &model.IngestBatch{BatchID: "batch-race", Source: model.SourceClient,
		State: model.BatchStateValidated, Total: 1, Accepted: 1, TopReason: reasonNone()})
	e.seedRecord(t, &model.EventRecord{EventID: "evt-race", BatchID: "batch-race",
		EventType: "behavior.play", Category: int32(rpc.BehaviorCategory_BEHAVIOR_CATEGORY_PLAY),
		SchemaVersion: 1, Decision: model.DecisionAccepted, Reason: reasonNone(),
		DeliveryState: model.DeliveryStatePending, Topic: topicV1("behavior.play")})
	e.seedPending(t, &model.PendingDelivery{EventID: "evt-race", BatchID: "batch-race",
		Topic: topicV1("behavior.play"), State: model.DeliveryStatePending})

	e.db.missFirst["batches.FindByBatchID"] = true
	before := e.effects()

	rep, err := e.collect(collectReq("batch-race", playEvent("evt-race")))
	r := wantOK(t, rep, err, "并发让路后改走回放")
	requireEqual(t, r.State, rpc.BatchState_BATCH_STATE_VALIDATED, "回放结论")
	requireEqual(t, r.Accepted, int32(1), "回放不得重复累加")
	e.requireSameEffects(t, before, "并发重放（事务回滚 + 只读回放）")
	if e.db.commits != 0 {
		t.Fatalf("撞 uniq_batch_id 的事务绝不能提交，实际 commits=%d", e.db.commits)
	}
	if e.db.rollbacks != 1 {
		t.Fatalf("期望回滚 1 次，实际 %d", e.db.rollbacks)
	}
}

func TestCollectEvents_RejectedBatchRedriveReusesSameLedgerRow(t *testing.T) {
	e := newTestEnv(t)
	e.withConfig(func(c *config.Config) { c.Collector.MaxEventsPerBatch = 1 })

	deferred, err := e.collect(collectReq("batch-redrive", playEvent("evt-rd-1"), playEvent("evt-rd-2")))
	rd := wantOK(t, deferred, err, "条数超限整批拒收")
	requireEqual(t, rd.State, rpc.BatchState_BATCH_STATE_REJECTED, "整批拒收态")
	requireEqual(t, rd.Total, int32(2), "拒收回带请求条数")
	requireTrue(t, rd.RetryAfterMs > 0, "拒收必须给退避提示")
	for _, res := range rd.Results {
		requireEqual(t, res.Decision, rpc.EventDecision_EVENT_DECISION_DEFERRED, "逐条 DEFERRED")
		requireEqual(t, res.Reason, rpc.RejectReason_REJECT_BATCH_TOO_LARGE, "整批拒收原因码")
	}
	requireEqual(t, len(e.db.records), 0, "整批拒收不得留事件台账")
	requireEqual(t, len(e.db.pending), 0, "整批拒收不得入 Outbox")
	batch := e.mustBatch(t, "batch-redrive")
	requireEqual(t, batch.State, model.BatchStateRejected, "拒收台账态")
	requireEqual(t, batch.TopReason, batchTooLargeReason(), "拒收原因归因")

	// 放宽上限后用同一个 batch_id 重发：必须复用同一行（redrive），不得新增批次行。
	e.withConfig(func(c *config.Config) { c.Collector.MaxEventsPerBatch = 50 })
	again, err := e.collect(collectReq("batch-redrive", playEvent("evt-rd-1"), playEvent("evt-rd-2")))
	r2 := wantOK(t, again, err, "同 batch_id 重驱动")
	requireEqual(t, r2.State, rpc.BatchState_BATCH_STATE_VALIDATED, "重驱动后收进")
	requireEqual(t, len(e.db.batches), 1, "重驱动必须复用同一批次行")
	b := e.mustBatch(t, "batch-redrive")
	requireEqual(t, b.State, model.BatchStateValidated, "重驱动状态推进")
	requireEqual(t, b.Total, int32(2), "重驱动计数只累加一次")
	requireEqual(t, b.TopReason, reasonNone(), "收进后主因归零")
	requireEqual(t, len(e.db.records), 2, "重驱动落两条台账")
	requireEqual(t, len(e.db.pending), 2, "重驱动入两条 Outbox")
}

func TestCollectEvents_SampledOutStaysSampledOutOnReplay(t *testing.T) {
	// 采样以 event_id 为随机源，结论必须可重放：同一批次重放后仍回 SAMPLED_OUT，
	// 台账行保持「完整可重算」（decision=4 留痕、不投递），Outbox 一条都不许多。
	e := newTestEnv(t)
	e.seedActivePolicyWithSample(t, "2026.09.01-1", []model.SampleRule{
		{EventType: model.SampleWildcard, SampleBps: 0},
		{EventType: "behavior.quality", SampleBps: 10000, QualityEvents: true},
	})

	rep, err := e.collect(collectReq("batch-sample",
		clickEvent("evt-sampled"), qualityEvent("evt-quality")))
	r := wantOK(t, rep, err, "采样批次")
	requireEqual(t, r.SampledOut, int32(1), "非质量类按规则丢弃")
	requireEqual(t, r.Accepted, int32(1), "质量类事件豁免采样")
	requireEqual(t, r.Results[0].Decision, rpc.EventDecision_EVENT_DECISION_SAMPLED_OUT, "丢弃结论")
	requireEqual(t, len(e.db.records), 2, "被丢弃事件也要留台账行（可重算）")
	requireEqual(t, len(e.db.pending), 1, "被丢弃事件不得入 Outbox")
	rec := e.mustRecord(t, "evt-sampled")
	requireEqual(t, rec.Decision, model.DecisionSampledOut, "台账结论")
	requireEqual(t, rec.DeliveryState, model.DeliveryStateNone, "被丢弃事件不得有投递态")
	requireEqual(t, rec.Topic, topicV1("behavior.click"), "topic 保留「本可投到哪」")

	before := e.effects()
	again, err := e.collect(collectReq("batch-sample",
		clickEvent("evt-sampled"), qualityEvent("evt-quality")))
	r2 := wantOK(t, again, err, "采样批次重放")
	requireEqual(t, r2.SampledOut, int32(1), "重放仍是 SAMPLED_OUT")
	requireEqual(t, r2.Accepted, int32(1), "重放不得把丢弃翻成受理")
	e.requireSameEffects(t, before, "采样批次重放")
}
