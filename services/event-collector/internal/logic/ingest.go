// 本文件是 logic 包的手写采集流水线（CollectEvents 与 IngestServerEvents 共用一条路径），
// 不是 goctl 生成产物。两个 goctl 文件只负责把各自的 Req 归一化成 batchRequest。
//
// 顺序铁律（AGENTS.md §5 §7、proto 头部硬约束）：
//
//	硬校验 → 取 ACTIVE 策略 → 取盐并脱敏 → 尺寸/令牌桶/维度限流闸门 → 逐条校验 →
//	确定性采样 → 同事务落库（批次行 + ec_event_record + ec_pending_delivery Outbox）
//
// MQ 发送在事务之外（internal/dispatcher 尚未落地，事件留在 Outbox 由
// RetryPendingDelivery/cron 推进；见 README「已知缺口」）。
// 采样必须晚于脱敏：否则被丢掉的事件仍可能把明文 IP/设备号留在中间日志里。
// 取不到盐必须整批失败：绝不退化成无盐哈希（无盐哈希可被枚举还原 = 明文入库）。

package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go-video/common/idgen"
	"go-video/common/ratelimit"
	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// errReplayBatch 是事务内部的信号：批次行已被另一路并发写入（Insert 返回 created=false），
// 本次必须整体回滚并改走「只读回放」，绝不重复落库、重复计数。
var errReplayBatch = errors.New("event-collector: batch row already exists, serve replay")

// batchRequest 是两个采集方法归一化后的统一入参。
//
// eventCtx 里带明文 device_id / ip，只在本请求生命周期内存在：runBatchIngest 一进来
// 就把它收敛成 privacyFields，之后任何落库、信封、日志都只允许用脱敏结果。
type batchRequest struct {
	// method RPC 名（进 Redis 幂等 key 与日志，不参与业务判定）
	method        string
	batchID       string
	wantSource    int32
	source        int32
	callerService string
	// idempotencyKey 服务端来源的动作幂等键（客户端来源为空）
	idempotencyKey string
	// policyHint 客户端缓存的策略版本；只用于检测「客户端缓存过期」，不作裁决依据。
	policyHint   string
	clientSeq    int32
	requestID    string
	batchTraceID string
	eventCtx     *rpc.EventContext
	events       []*rpc.BehaviorEvent
	// sampling 是否允许采样：SOURCE_SERVER 一律 false（关键埋点不能被采样丢掉）。
	sampling bool
	// bytes 请求体编码后字节数（max_request_bytes 闸门）。
	bytes int64
}

// selfCheck 只判「不查库就不可能正确」的前置条件，越界一律返回明确的 model.Err*。
func (r *batchRequest) selfCheck() error {
	if strings.TrimSpace(r.batchID) == "" || len(r.batchID) > maxBatchIDBytes {
		return model.ErrBatchIDRequired
	}
	if len(r.events) == 0 {
		return model.ErrEventsRequired
	}
	if !model.ValidSource(r.source) {
		return model.ErrSourceNotAllowed(r.source)
	}
	// 来源与方法必须匹配（REJECT_SOURCE_NOT_ALLOWED）：客户端通道不能收服务端埋点
	// （否则不采样的关键事件会绕过服务身份校验），反之亦然。
	if r.source != r.wantSource {
		return model.ErrSourceNotAllowed(r.source)
	}
	if r.source == model.SourceServer {
		if strings.TrimSpace(r.callerService) == "" {
			return fmt.Errorf("%w: caller_service required for SOURCE_SERVER", model.ErrOperatorRequired)
		}
		// 跨进程重试必然带动作幂等键，否则重试只能靠 batch_id 撞库（proto 注释）。
		if strings.TrimSpace(r.idempotencyKey) == "" {
			return model.ErrIdempotencyKeyRequired
		}
	}
	return nil
}

// batchOutcome 是采集流水线的内部结论，由 goctl 方法投影成各自的 Reply。
type batchOutcome struct {
	batchID        string
	state          int32
	total          int32
	accepted       int32
	duplicated     int32
	rejected       int32
	sampledOut     int32
	results        []*rpc.EventResult
	policyVersion  string
	reportInterval int32
	retryAfterMs   int32
	degraded       bool
	// replayed 批次命中幂等回放。CollectEventsReply/IngestServerEventsReply 没有对应字段
	// （proto 缺口，见 README），只能靠 EventResult.message 与批次台账体现。
	replayed bool
	// notices 非致命但必须被运维看到的说明（无 ACTIVE 策略、策略版本过期、dispatcher 未接线）。
	notices []string
	// now 服务端处理时刻（Unix 秒），投影为 CollectEventsReply.server_time 供客户端校时。
	now int64
}

func (o *batchOutcome) toCollectReply() *rpc.CollectEventsReply {
	return &rpc.CollectEventsReply{
		BatchId:                   o.batchID,
		State:                     rpc.BatchState(o.state),
		Total:                     o.total,
		Accepted:                  o.accepted,
		Duplicated:                o.duplicated,
		Rejected:                  o.rejected,
		SampledOut:                o.sampledOut,
		Results:                   o.results,
		PolicyVersion:             o.policyVersion,
		NextReportIntervalSeconds: o.reportInterval,
		RetryAfterMs:              o.retryAfterMs,
		Degraded:                  o.degraded,
		ServerTime:                o.serverTimeValue(),
	}
}

func (o *batchOutcome) toServerReply() *rpc.IngestServerEventsReply {
	return &rpc.IngestServerEventsReply{
		BatchId:       o.batchID,
		State:         rpc.BatchState(o.state),
		Total:         o.total,
		Accepted:      o.accepted,
		Duplicated:    o.duplicated,
		Rejected:      o.rejected,
		Results:       o.results,
		PolicyVersion: o.policyVersion,
		RetryAfterMs:  o.retryAfterMs,
		Degraded:      o.degraded,
	}
}

// serverTimeValue 只在采集入口回带（客户端校时）。零值时补当前时刻，
// 避免客户端拿到 0 把自己的时钟校到 1970。
func (o *batchOutcome) serverTimeValue() int64 {
	if o == nil || o.now <= 0 {
		return timeNow()
	}
	return o.now
}

// ingestRunner 是一次采集请求的执行上下文。
type ingestRunner struct {
	ctx      context.Context
	s        *svc.ServiceContext
	logger   logx.Logger
	req      batchRequest
	lim      limits
	pv       privacyFields
	now      int64
	degraded bool
	notices  []string
	// replayAttempted 防止「并发回放 → 又被拒 → 再回放」的递归：只重走一次。
	replayAttempted bool
}

// runBatchIngest 是采集主流程。返回 error 表示「本批结论无法给出」（依赖故障、
// 取不到盐、来源不合法）；返回 outcome 时整批要么已落库、要么被明确拒收。
func runBatchIngest(ctx context.Context, s *svc.ServiceContext, logger logx.Logger,
	req batchRequest) (*batchOutcome, error) {

	if err := req.selfCheck(); err != nil {
		return nil, err
	}
	lim, err := resolveLimits(ctx, s, req.policyHint)
	if err != nil {
		return nil, err
	}
	salt, saltVersion, err := resolveSalt(s, lim)
	if err != nil {
		// 盐不可用 → 整批失败。这里不写「被拒批次行」：没有盐就算不出 device_hash，
		// 落一行无法归因、也无法复核的记录只会污染台账。
		logger.Errorf("event-collector/logic: %s 取盐失败，整批拒绝 batch_id=%s: %v",
			req.method, req.batchID, err)
		return nil, err
	}
	// 脱敏先于一切落库/日志/判定分支（AGENTS.md §7）。
	pv, err := desensitize(salt, saltVersion, req.eventCtx, lim.ipSegmentBits)
	if err != nil {
		return nil, err
	}
	// device_id 有值却没算出哈希，说明入参与实现不一致（不可能，除非 SaltedHash 被改坏）：
	// 宁可拒批，也不要把「自认为脱敏了其实没有」的批次当成正常接收。
	if pv.deviceHash == "" && strings.TrimSpace(req.eventCtx.GetDeviceId()) != "" {
		return nil, model.ErrSaltMissing
	}
	r := &ingestRunner{ctx: ctx, s: s, logger: logger, req: req, lim: lim, pv: pv,
		now: time.Now().Unix()}
	if lim.activeIsConfig {
		r.degraded = true
		r.notices = append(r.notices, msgPolicyNoActive)
	}
	if lim.stalePolicyHint {
		r.degraded = true
		r.notices = append(r.notices, msgPolicyHintStale)
	}

	existing, err := s.Batches.FindByBatchID(ctx, req.batchID)
	switch {
	case err == nil && existing != nil:
		if err := r.checkIdempotencyKey(existing); err != nil {
			return nil, err
		}
		// 只有「整批被拒」的批次值得重驱动：容量/限流是瞬时状态，一旦被永久化成拒绝，
		// 客户端用同一个 batch_id 重试就永远收不进来（等于丢数据）。
		if existing.State == model.BatchStateRejected && existing.ClientSeq <= req.clientSeq {
			return r.process(existing)
		}
		return r.replay(existing)
	case err != nil && !model.IsNotFound(err):
		return nil, err
	default:
		return r.process(nil)
	}
}

// checkIdempotencyKey 挡住「同一个 batch_id 配了两个 idempotency_key」：那不是重试，
// 而是调用方把两个动作写成了同一个键。回放别人的结论会掩盖这个 bug（并且丢一个动作）。
func (r *ingestRunner) checkIdempotencyKey(existing *model.IngestBatch) error {
	stored := strings.TrimSpace(existing.IdempotencyKey)
	want := strings.TrimSpace(r.req.idempotencyKey)
	if stored == "" || want == "" || stored == want {
		return nil
	}
	r.logger.Errorf("event-collector/logic: %s batch_id=%s 复用了已存在的批次幂等键", r.req.method, r.req.batchID)
	return fmt.Errorf("%w: batch_id=%s", model.ErrIdempotencyKeyConflict, r.req.batchID)
}

// process 执行容量闸门后进入落库路径。existing 非空表示重驱动一个 state=REJECTED 的批次。
func (r *ingestRunner) process(existing *model.IngestBatch) (*batchOutcome, error) {
	// 1) 条数/字节双重上限：越界整批 DEFERRED，不逐条报错放大流量。
	if int32(len(r.req.events)) > r.lim.maxEventsPerBatch || r.req.bytes > r.lim.maxRequestBytes {
		detail := fmt.Sprintf("%s (events=%d/%d, bytes=%d/%d)", msgDeferredSize,
			len(r.req.events), r.lim.maxEventsPerBatch, r.req.bytes, r.lim.maxRequestBytes)
		if err := r.recordWholeBatchRejection(batchTooLargeReason(), detail); err != nil {
			return nil, err
		}
		return r.deferred(batchTooLargeReason(), detail), nil
	}
	// 2) 进程级令牌桶：保护 MySQL 与下游 MQ。
	if r.s.GlobalLimiter == nil {
		return nil, fmt.Errorf("event-collector: global limiter not configured")
	}
	done, err := r.s.GlobalLimiter.Allow(r.ctx)
	if err != nil {
		if !errors.Is(err, ratelimit.ErrLimitExceed) && !errors.Is(err, ratelimit.ErrDeadline) {
			return nil, fmt.Errorf("event-collector: global limiter error: %w", err)
		}
		if err := r.recordWholeBatchRejection(rateLimitedReason(), msgDeferredLimit); err != nil {
			return nil, err
		}
		return r.deferred(rateLimitedReason(), msgDeferredLimit), nil
	}
	outcome, perr := r.ingest(existing != nil)
	// done 必须调用（归还 inflight 计数）：失败按 Ignore 处理，不把 DB 故障算成限流信号。
	if perr != nil {
		done(ratelimit.Ignore)
	} else {
		done(ratelimit.Success)
	}
	return outcome, perr
}

func (r *ingestRunner) ingest(redrive bool) (*batchOutcome, error) {
	// 3) 维度限流（mid / 设备 / IP 段 / caller_service）。
	limited, reason, detail := r.dimensionGate()
	if limited {
		if err := r.recordWholeBatchRejection(reason, detail); err != nil {
			return nil, err
		}
		return r.deferred(reason, detail), nil
	}
	// 4) 逐条校验 + 采样判定（读多写少，先在事务外算完）。
	pl, err := r.plan()
	if err != nil {
		return nil, err
	}
	// 5) 同事务落库：批次行 + 事件台账 + Outbox。
	if err := r.persist(pl, redrive); err != nil {
		if errors.Is(err, errReplayBatch) {
			return r.serveConcurrentReplay(redrive)
		}
		// 落库失败 = 整批未落库。必须显式告知 DEFERRED + 可重试，绝不能标记为已接收。
		r.logger.Errorf("event-collector/logic: %s 整批落库失败 batch_id=%s: %v",
			r.req.method, r.req.batchID, err)
		if rerr := r.recordWholeBatchRejection(pendingReason(), msgStoreFailed); rerr != nil {
			r.logger.Errorf("event-collector/logic: 记录落库失败台账失败 batch_id=%s: %v", r.req.batchID, rerr)
		}
		out := r.deferred(pendingReason(), msgStoreFailed)
		return out, nil
	}
	return r.success(pl), nil
}

// serveConcurrentReplay 处理「事务里发现批次行已存在」：回滚后改走只读回放。
// 只重走一次，避免并发对手也停在 REJECTED 时无限互相让路。
func (r *ingestRunner) serveConcurrentReplay(redrive bool) (*batchOutcome, error) {
	if r.replayAttempted {
		return nil, fmt.Errorf("%w: batch %s keeps racing", model.ErrConcurrentUpdate, r.req.batchID)
	}
	r.replayAttempted = true
	existing, err := r.s.Batches.FindByBatchID(r.ctx, r.req.batchID)
	if err != nil {
		return nil, err
	}
	if err := r.checkIdempotencyKey(existing); err != nil {
		return nil, err
	}
	if existing.State == model.BatchStateRejected && !redrive && existing.ClientSeq <= r.req.clientSeq {
		return r.process(existing)
	}
	return r.replay(existing)
}

// dimensionGate 固定窗口维度限流。返回 limited=true 时整批未接收。
//
// Redis 故障一律 fail-open + degraded：这三个阈值只是保护自身的下限，
// 真值判定在 risk-control（README「疑点」）；缓存抖动时宁可少限流，
// 也不能把全部流量整批拒掉（那等于自建 DoS）。
func (r *ingestRunner) dimensionGate() (bool, int32, string) {
	type dim struct {
		name, value string
		qps         int32
	}
	callerQps := r.lim.callerQps
	dims := make([]dim, 0, 3)
	if r.pv.mid > 0 {
		dims = append(dims, dim{"mid", strconv.FormatInt(r.pv.mid, 10), r.lim.midQps})
	}
	if r.req.source == model.SourceServer {
		// 服务端来源没有设备维度，改按服务身份限流（调用方退避而不是丢数据）。
		dims = append(dims, dim{"caller", fitColumn(r.req.callerService, maxCallerSrvBytes), callerQps})
	} else if r.pv.deviceHash != "" {
		dims = append(dims, dim{"device", r.pv.deviceHash, r.lim.deviceQps})
	}
	if r.pv.ipSegment != "" {
		dims = append(dims, dim{"ipseg", r.pv.ipSegment, r.lim.ipSegmentQps})
	}
	delta := int64(len(r.req.events))
	for _, d := range dims {
		over, err := counterOver(r.ctx, r.s, d.name, d.value, d.qps, delta, r.now)
		if err != nil {
			r.logger.Errorf("event-collector/logic: %s 维度限流计数失败（fail-open）: %v", d.name, err)
			r.degraded = true
			continue
		}
		if over {
			return true, rateLimitedReason(), fmt.Sprintf("%s 维度超过 %d 事件/秒，整批未接收；"+
				"%s", d.name, d.qps, msgDeferredLimit)
		}
	}
	return false, reasonNone(), ""
}

// batchPlan 是「算完但还没落库」的结论集合，三条列表按 index 对齐请求顺序。
type batchPlan struct {
	results    []*rpc.EventResult
	stored     []*model.EventRecord
	pending    []*model.PendingDelivery
	total      int32
	accepted   int32
	duplicated int32
	rejected   int32
	sampledOut int32
	worst      int32
}

func (pl *batchPlan) count(v *eventVerdict) {
	pl.total++
	switch v.decision {
	case model.DecisionAccepted:
		pl.accepted++
	case model.DecisionDuplicated:
		pl.duplicated++
	case model.DecisionRejected:
		pl.rejected++
		pl.worst = worstReason(pl.worst, v.reason)
	case model.DecisionSampledOut:
		pl.sampledOut++
	}
}

// plan 逐条裁决。先批量回查 ec_event_record 做去重预检（uniq_event_id 才是真约束，
// 预检只是为了把「已存在」判成 DUPLICATED 而不是让 INSERT IGNORE 静默丢行），
// 真正的原子性由同事务的 InsertIgnoreMany + 行数校验保证。
func (r *ingestRunner) plan() (*batchPlan, error) {
	known, err := r.existingRecords()
	if err != nil {
		return nil, err
	}
	pl := &batchPlan{results: make([]*rpc.EventResult, 0, len(r.req.events))}
	seen := make(map[string]struct{}, len(r.req.events))
	for i, ev := range r.req.events {
		v := r.judgeOne(ev, known, seen)
		v.rec = r.bindRow(v.rec)
		if v.stored && v.rec != nil {
			pl.stored = append(pl.stored, v.rec)
			if v.decision == model.DecisionAccepted && v.rec != nil && v.rec.DeliveryState == model.DeliveryStatePending {
				pl.pending = append(pl.pending, r.pendingRow(v.rec))
			}
		}
		pl.count(v)
		pl.results = append(pl.results, verdictResult(int32(i), v))
		if v.eventID != "" {
			seen[v.eventID] = struct{}{}
		}
	}
	return pl, nil
}

// bindRow 补上 validateEvent 不可能知道的批次归属列。
func (r *ingestRunner) bindRow(rec *model.EventRecord) *model.EventRecord {
	if rec == nil {
		return nil
	}
	rec.BatchID = fitColumn(r.req.batchID, maxBatchIDBytes)
	return rec
}

func (r *ingestRunner) pendingRow(rec *model.EventRecord) *model.PendingDelivery {
	return &model.PendingDelivery{
		EventID:         rec.EventID,
		BatchID:         rec.BatchID,
		Topic:           rec.Topic,
		EnvelopeEventID: rec.EnvelopeEventID,
		PayloadDigest:   rec.PayloadDigest,
		State:           model.DeliveryStatePending,
		// next_retry_at=0：入队即到期，dispatcher/cron 可立即领取。
		// 真正的首投退避由 model.NextRetryAt 在失败时计算。
		NextRetryAt: 0,
	}
}

// judgeOne 单条裁决：去重 → 校验 → 采样。
func (r *ingestRunner) judgeOne(ev *rpc.BehaviorEvent, known map[string]*model.EventRecord,
	seen map[string]struct{}) *eventVerdict {

	if ev == nil {
		return &eventVerdict{decision: model.DecisionRejected, reason: internalReason(),
			message: "event is nil", stored: false}
	}
	id := strings.TrimSpace(ev.GetEventId())
	// event_id 不合法（空/超长）：validateEvent 会给出 MISSING_EVENT_ID 且没有可落的行。
	if id == "" || len(id) > maxEventIDBytes {
		return validateEvent(ev, r.req.source, r.pv, r.lim, r.now, r.req.batchTraceID)
	}
	if _, dup := seen[id]; dup {
		// 同一批次内重复：只处理首次出现。不落行（会撞 uniq_event_id 被静默丢掉）。
		return &eventVerdict{eventID: id, decision: model.DecisionDuplicated, reason: reasonNone(),
			message: msgInRequestDup, stored: false}
	}
	if row, ok := known[id]; ok {
		if row.BatchID == r.req.batchID {
			// 本批已有的行（重驱动/并发让路）：回读首次结论，绝不重算。
			return replayedVerdict(row)
		}
		return &eventVerdict{eventID: id, decision: model.DecisionDuplicated, reason: reasonNone(),
			message: msgDuplicated, stored: false}
	}
	v := validateEvent(ev, r.req.source, r.pv, r.lim, r.now, r.req.batchTraceID)
	if v.decision != model.DecisionAccepted || v.rec == nil {
		return v
	}
	// 采样在脱敏与校验之后，且以 event_id 为随机源：重放同一事件必得同一结论。
	bps := r.sampleBpsFor(v)
	if !model.SampleHit(v.eventID, bps) {
		v.decision = model.DecisionSampledOut
		v.reason = reasonNone()
		v.message = msgSampledOut
		v.rec.Decision = model.DecisionSampledOut
		v.rec.Reason = reasonNone()
		v.rec.ReasonDetail = fitColumn(msgSampledOut, maxReasonBytes)
		// topic 保留「本可投递到哪」，delivery_state=NONE 才表示没投；不生成信封 ID。
		v.rec.DeliveryState = model.DeliveryStateNone
		v.rec.EnvelopeEventID = ""
		v.rec.NextRetryAt = 0
		return v
	}
	envID, err := idgen.ULID()
	if err != nil {
		// 信封 ID 生成失败：不能标 ACCEPTED 后带空 envelope_event_id 落库，
		// 那会让下游无法去重（等价于伪造已接收），整条按内部错误处理。
		return v.reject(rpc.RejectReason_REJECT_INTERNAL, "信封 event_id 生成失败: "+err.Error())
	}
	v.rec.EnvelopeEventID = fitColumn(envID, maxEventIDBytes)
	v.rec.DeliveryState = model.DeliveryStatePending
	v.rec.NextRetryAt = 0
	v.message = strings.Join(v.notes, "; ")
	return v
}

// sampleBpsFor 取生效采样比例：服务端埋点不采样；策略未匹配按 config 默认（通常全量），
// 绝不把「查不到规则」当成 0（model.EffectiveSampleBps 注释同义）。
func (r *ingestRunner) sampleBpsFor(v *eventVerdict) int32 {
	if !r.req.sampling {
		return model.SampleBase
	}
	isQuality := isQualityCategory(rpc.BehaviorCategory(v.rec.Category))
	if got := model.EffectiveSampleBps(r.lim.sampleRules, v.eventType, isQuality); got >= 0 {
		return got
	}
	return r.lim.defaultSampleBps
}

// replayedVerdict 把本批已存在的台账行还原成结论（幂等回放，对齐 index）。
func replayedVerdict(row *model.EventRecord) *eventVerdict {
	msg := strings.TrimSpace(row.ReasonDetail)
	if msg == "" {
		msg = msgBatchReplayed
	} else {
		msg = msgBatchReplayed + "；首次结论：" + msg
	}
	return &eventVerdict{
		decision:  row.Decision,
		reason:    row.Reason,
		message:   fitColumn(msg, maxReasonBytes),
		rec:       row,
		eventID:   row.EventID,
		eventType: row.EventType,
		topic:     row.Topic,
		// stored=false：本行已在库里，再写一次只是撞唯一键。
		stored: false,
	}
}

// existingRecords 批量回查本批 event_id 的既有台账行。
// 走连接池（事务外只读）：预检只是优化结论来源，最终一致性由事务内行数校验保证。
func (r *ingestRunner) existingRecords() (map[string]*model.EventRecord, error) {
	set := make(map[string]struct{}, len(r.req.events))
	for _, ev := range r.req.events {
		if ev == nil {
			continue
		}
		if id := strings.TrimSpace(ev.GetEventId()); id != "" && len(id) <= maxEventIDBytes {
			set[id] = struct{}{}
		}
	}
	out := make(map[string]*model.EventRecord, len(set))
	if len(set) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	for _, chunk := range chunkIDs(ids, listChunkSize) {
		m, err := r.s.Records.ListByEventIDs(r.ctx, chunk)
		if err != nil {
			return nil, fmt.Errorf("event-collector: 回查 event_id 台账失败: %w", err)
		}
		for k, v := range m {
			out[k] = v
		}
	}
	return out, nil
}

// persist 把整批结论写进一个事务：批次行 + 事件台账 + Outbox 行 + 计数投影 + 状态迁移。
//
// 用 INSERT IGNORE 并逐块核对新增行数：这是唯一可靠的幂等写路径 —— MySQL 对超长值会
// 静默丢行，行数不等必须当成并发冲突回滚，而不是「少写几行也算成功」。
func (r *ingestRunner) persist(pl *batchPlan, redrive bool) error {
	row := r.newBatchRow(model.BatchStateReceived)
	notice := fitColumn(strings.Join(r.allNotices(), "; "), maxReasonBytes)
	return r.s.DB.TransactCtx(r.ctx, func(ctx context.Context, session sqlx.Session) error {
		if !redrive {
			_, created, err := r.s.Batches.Insert(ctx, session, row)
			if err != nil {
				return err
			}
			if !created {
				return errReplayBatch
			}
		} else if err := r.s.Batches.UpdateAttribution(ctx, session, r.req.batchID,
			row.PolicyVersion, row.SaltVersion, row.RequestBytes); err != nil {
			return err
		}
		if n, err := r.s.Records.InsertIgnoreMany(ctx, session, pl.stored); err != nil {
			return err
		} else if int(n) != len(pl.stored) {
			return fmt.Errorf("%w: ec_event_record 期望 %d 行、实际 %d 行（并发重放或值越界）",
				model.ErrConcurrentUpdate, len(pl.stored), n)
		}
		if n, err := r.s.Pending.InsertIgnoreMany(ctx, session, pl.pending); err != nil {
			return err
		} else if int(n) != len(pl.pending) {
			return fmt.Errorf("%w: ec_pending_delivery 期望 %d 行、实际 %d 行（Outbox 与台账不一致）",
				model.ErrConcurrentUpdate, len(pl.pending), n)
		}
		if err := r.s.Batches.Accumulate(ctx, session, r.req.batchID,
			pl.total, pl.accepted, pl.duplicated, pl.rejected, pl.sampledOut); err != nil {
			return err
		}
		applied, err := r.s.Batches.MarkState(ctx, session, r.req.batchID,
			[]int32{model.BatchStateReceived, model.BatchStateRejected}, model.BatchStateValidated,
			topReasonFor(pl.accepted, pl.worst), notice)
		if err != nil {
			return err
		}
		if !applied {
			// 状态没迁移成功说明并发有别人推进过：整事务回滚，改走回放，
			// 不允许留下「事件写了两遍、状态却只推进一次」的半途台账。
			return fmt.Errorf("%w: 批次状态迁移未生效", model.ErrConcurrentUpdate)
		}
		return nil
	})
}

// allNotices 汇总必须被运维看到的非致命状态（策略缺失/过期、dispatcher 未接线）。
func (r *ingestRunner) allNotices() []string {
	out := append([]string(nil), r.notices...)
	if !r.s.DispatcherEnabled() {
		out = append(out, msgNoDispatcher)
	}
	return out
}

// newBatchRow 构造批次行：只写脱敏列与定长安全列。
//
// Total 必须为 0 —— 计数由 Accumulate 累加，插初始值会造成双计。
func (r *ingestRunner) newBatchRow(state int32) *model.IngestBatch {
	mc := r.req.eventCtx
	return &model.IngestBatch{
		BatchID:        fitColumn(r.req.batchID, maxBatchIDBytes),
		Source:         r.req.source,
		CallerService:  fitColumn(strings.TrimSpace(r.req.callerService), maxCallerSrvBytes),
		IdempotencyKey: fitColumn(strings.TrimSpace(r.req.idempotencyKey), maxIdemKeyBytes),
		Platform:       int32(mc.GetPlatform()),
		AppID:          fitColumn(strings.TrimSpace(mc.GetAppId()), maxAppIDBytes),
		AppVersion:     fitColumn(strings.TrimSpace(mc.GetAppVersion()), maxTagBytes),
		SdkVersion:     fitColumn(strings.TrimSpace(mc.GetSdkVersion()), maxTagBytes),
		Mid:            r.pv.mid,
		DeviceHash:     r.pv.deviceHash,
		IPSegment:      r.pv.ipSegment,
		SaltVersion:    r.pv.saltVersion,
		PolicyVersion:  fitColumn(r.lim.policyVersion, maxPolicyVerBytes),
		Total:          0,
		RequestBytes:   r.req.bytes,
		State:          state,
		TopReason:      reasonNone(),
		ClientSeq:      r.req.clientSeq,
		RequestID:      fitColumn(strings.TrimSpace(r.req.requestID), maxRequestIDBytes),
		TraceID:        fitColumn(strings.TrimSpace(r.req.batchTraceID), maxTraceIDBytes),
		ReceivedAt:     r.now,
	}
}

// recordWholeBatchRejection 写「整批被拒」台账，让 GetCollectorHealth 的
// batches_rejected_last_hour / rate_limited_last_hour 有数据来源（事件台账里没有这些事件）。
//
// 只在批次行不存在、或已是 REJECTED 时推进状态：绝不把已落库的批次改回 REJECTED，
// 那等于抹掉「事件已可靠接收」的事实。
func (r *ingestRunner) recordWholeBatchRejection(reason int32, detail string) error {
	row := r.newBatchRow(model.BatchStateRejected)
	row.TopReason = reason
	row.LastError = fitColumn(strings.Join(append([]string{detail}, r.allNotices()...), "; "), maxReasonBytes)
	_, created, err := r.s.Batches.Insert(r.ctx, nil, row)
	if err != nil {
		return fmt.Errorf("event-collector: 写整批拒绝台账失败 batch_id=%s: %w", r.req.batchID, err)
	}
	if created {
		return nil
	}
	applied, err := r.s.Batches.MarkState(r.ctx, nil, r.req.batchID,
		[]int32{model.BatchStateReceived, model.BatchStateRejected}, model.BatchStateRejected,
		reason, row.LastError)
	if err != nil {
		return err
	}
	if !applied {
		r.logger.Errorf("event-collector/logic: batch_id=%s 已被并发接收，本次拒收结论不回写状态", r.req.batchID)
	}
	return nil
}

// deferred 构造「整批未接收」的结论：每条都回 DEFERRED + 同一个原因码与退避提示，
// 客户端据此重试同一 batch_id（幂等，不会翻倍）。
func (r *ingestRunner) deferred(reason int32, detail string) *batchOutcome {
	results := make([]*rpc.EventResult, 0, len(r.req.events))
	for i, ev := range r.req.events {
		var id string
		if ev != nil {
			id = fitColumn(strings.TrimSpace(ev.GetEventId()), maxEventIDBytes)
		}
		results = append(results, &rpc.EventResult{
			Index:    int32(i),
			EventId:  id,
			Decision: rpc.EventDecision_EVENT_DECISION_DEFERRED,
			Reason:   rpc.RejectReason(reason),
			Message:  fitColumn(detail, maxReasonBytes),
		})
	}
	return &batchOutcome{
		batchID:        r.req.batchID,
		state:          model.BatchStateRejected,
		total:          int32(len(r.req.events)),
		results:        results,
		policyVersion:  r.lim.policyVersion,
		reportInterval: r.reportInterval(true),
		retryAfterMs:   r.retryAfterMs(),
		degraded:       true,
		notices:        r.allNotices(),
		now:            r.now,
	}
}

// success 构造已落库批次的结论。
func (r *ingestRunner) success(pl *batchPlan) *batchOutcome {
	degraded := r.degraded
	if !r.s.DispatcherEnabled() {
		// 事件已可靠落在 Outbox，但进程内没有生产者：降级可观测，运维不至于无感。
		degraded = true
	}
	return &batchOutcome{
		batchID:        r.req.batchID,
		state:          model.BatchStateValidated,
		total:          pl.total,
		accepted:       pl.accepted,
		duplicated:     pl.duplicated,
		rejected:       pl.rejected,
		sampledOut:     pl.sampledOut,
		results:        pl.results,
		policyVersion:  r.lim.policyVersion,
		reportInterval: r.reportInterval(degraded),
		degraded:       degraded,
		notices:        r.allNotices(),
		now:            r.now,
	}
}

// replay 回放首次接收时落库的结论（不重复落库、不重复投递、不重复计数）。
func (r *ingestRunner) replay(existing *model.IngestBatch) (*batchOutcome, error) {
	if existing.State == model.BatchStateRejected {
		// 首次接收时整批被拒（未落库）：只有「重驱动」才可能收进来，这里到达说明
		// 本次是更旧的重发（client_seq 落后）。回带首次的拒收结论与退避提示。
		out := r.deferred(existing.TopReason, fitColumn(msgBatchReplayed+"；首次结论："+
			existing.LastError, maxReasonBytes))
		out.replayed = true
		out.state = model.BatchStateRejected
		return out, nil
	}
	known, err := r.existingRecords()
	if err != nil {
		return nil, err
	}
	results := make([]*rpc.EventResult, 0, len(r.req.events))
	seen := make(map[string]struct{}, len(r.req.events))
	var stored int32
	for i, ev := range r.req.events {
		v := r.replayOne(ev, known, seen, &stored)
		if v.message == "" {
			v.message = msgBatchReplayed
		}
		results = append(results, verdictResult(int32(i), v))
		if v.eventID != "" {
			seen[v.eventID] = struct{}{}
		}
	}
	degraded := r.degraded || existing.State == model.BatchStateRejected
	return &batchOutcome{
		batchID:        existing.BatchID,
		state:          existing.State,
		total:          existing.Total,
		accepted:       existing.Accepted,
		duplicated:     existing.Duplicated,
		rejected:       existing.Rejected,
		sampledOut:     existing.SampledOut,
		results:        results,
		policyVersion:  existing.PolicyVersion,
		reportInterval: r.reportInterval(degraded),
		retryAfterMs:   r.deferRetryHint(existing),
		degraded:       degraded,
		replayed:       true,
		notices: append(r.allNotices(), fmt.Sprintf("batch %s 为幂等回放（落库 %d/%d 条）",
			existing.BatchID, stored, len(r.req.events))),
		now: r.now,
	}, nil
}

// replayOne 单条回放：只有「本批已落库的行」能还原首次结论；其余情况必须说清
// 「本条没有落库痕迹」，绝不伪造一个 ACCEPTED。
func (r *ingestRunner) replayOne(ev *rpc.BehaviorEvent, known map[string]*model.EventRecord,
	seen map[string]struct{}, stored *int32) *eventVerdict {

	if ev == nil {
		return &eventVerdict{decision: model.DecisionRejected, reason: internalReason(),
			message: "event is nil"}
	}
	id := strings.TrimSpace(ev.GetEventId())
	if id == "" || len(id) > maxEventIDBytes {
		// 首次接收时同样留不下痕，结论稳定为缺 event_id（与首跑一致，不靠猜）。
		return validateEvent(ev, r.req.source, r.pv, r.lim, r.now, r.req.batchTraceID)
	}
	if _, dup := seen[id]; dup {
		return &eventVerdict{eventID: id, decision: model.DecisionDuplicated,
			reason: reasonNone(), message: msgInRequestDup}
	}
	row, ok := known[id]
	if !ok {
		// 查不到行：要么首次接收时整批被拒（未落库），要么台账已按留存策略清理。
		// 两种情况都不能伪造成「已接收」。
		return &eventVerdict{eventID: id, decision: model.DecisionRejected,
			reason: internalReason(), message: msgOwnRowLost}
	}
	if row.BatchID != r.req.batchID {
		return &eventVerdict{eventID: id, decision: model.DecisionDuplicated,
			reason: reasonNone(), message: msgDuplicated}
	}
	*stored++
	return replayedVerdict(row)
}

// deferRetryHint 回放一个「整批被拒」的批次时回带退避提示：首次拒收是有原因的
// （容量/限流），客户端仍应按提示退避。
func (r *ingestRunner) deferRetryHint(existing *model.IngestBatch) int32 {
	if existing.State != model.BatchStateRejected {
		return 0
	}
	return r.retryAfterMs()
}

func (r *ingestRunner) retryAfterMs() int32 {
	ms := r.lim.retryHintMs
	if ms <= 0 {
		ms = 1000
	}
	if r.degraded {
		ms *= 2
	}
	return ms
}

func (r *ingestRunner) reportInterval(degraded bool) int32 {
	secs := r.lim.reportIntervalSecs
	if secs <= 0 {
		secs = 10
	}
	if degraded {
		// 降级/限流期放大上报间隔，避免客户端密集重试把故障放大成雪崩。
		secs *= 2
	}
	return secs
}

// verdictResult 把内部结论投影成 EventResult（index 与请求严格对齐）。
func verdictResult(index int32, v *eventVerdict) *rpc.EventResult {
	res := &rpc.EventResult{
		Index:    index,
		EventId:  v.eventID,
		Decision: rpc.EventDecision(v.decision),
		Reason:   rpc.RejectReason(v.reason),
		Message:  fitColumn(v.message, maxReasonBytes),
		Topic:    v.topic,
	}
	if v.rec != nil {
		res.SchemaVersion = v.rec.SchemaVersion
	}
	return res
}
