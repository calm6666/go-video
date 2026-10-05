// 本文件是 logic 包的手写投递推进层（Outbox 逐行状态机、批次收敛、死信重放入队），
// 不是 goctl 生成产物。
//
// 为什么单独成文件：RetryPendingDelivery 与 ReplayDeadLetter 共用同一套
// 「租约/围栏 + 状态机 + 台账投影 + 批次收敛」动作，任何一处自己写一遍都会出现
// 「两条路径对同一行给出矛盾结论」（AGENTS.md §5 幂等与可复现）。
//
// 三条硬底线：
//  1. 投递结果只有 ec_pending_delivery 能说话：台账（ec_event_record）与批次计数列都是投影，
//     投影写失败只记日志，绝不因为投影失败就把「已发出去的事件」说成未投递（也不可反之）。
//  2. 每一次行迁移都带租约持有者（worker）做围栏：applied=false 说明租约已被别的实例接管，
//     本实例必须放弃这一行，否则两个运营/cron 会把同一事件投两次或标两次死信。
//  3. 外发的任何错误文本先过 errorText（脱敏 + 列宽收敛）：broker 报错里可能带连接串账号。

package logic

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/model"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// errSkipReplay 事务内用来表达「该行已是终态，本轮跳过」。
// 用哨兵而不是返回 nil：让上层能区分「跳过（不算失败）」与「真的没处置成功」。
var errSkipReplay = errors.New("event-collector: dead letter already handled, skip replay")

// pendingSender 是投递通道（Kafka producer）的最小抽象。
//
// 只暴露 Send(row)：Outbox 行携带 topic / envelope_event_id / payload_digest，
// 真正的生产者据此从对象存储取正文并发送（README 已知缺口：正文不入库）。
type pendingSender interface {
	Send(ctx context.Context, row *model.PendingDelivery) error
}

// dispatcherSender 解析投递通道。
//
// 这是本服务唯一的 MQ 接线点：接线（internal/dispatcher + kq producer）完成后只需替换本
// 函数体，runPendingRound 的逐行状态机不用动。
//
// 现在两种情况都必须返回 model.ErrDispatcherMissing：
//   - Dispatch.Endpoints 未配置：本就不投递，事件留在 Outbox；
//   - 配了 Endpoints 但生产者未接线：此时若继续走逐行状态机，
//     每一次「发送失败」都会被写成 attempts+1，几轮后把完好无损的事件推进死信 ——
//     比不投更糟。所以判定必须发生在 ClaimDue 之前（见 RetryPendingDelivery 注释）。
func dispatcherSender(s *svc.ServiceContext) (pendingSender, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: service context is nil", model.ErrDispatcherMissing)
	}
	if !s.DispatcherEnabled() {
		return nil, fmt.Errorf("%w: Dispatch.Endpoints 未配置，事件留在 Outbox 由 RetryPendingDelivery 推进",
			model.ErrDispatcherMissing)
	}
	return nil, fmt.Errorf("%w: Dispatch.Endpoints 已配置但 MQ 生产者尚未接线（internal/dispatcher 未实现，"+
		"见 services/event-collector/README.md 已知缺口）", model.ErrDispatcherMissing)
}

// 投递投影的合法来源集合。
//
// recordFromOpen 是正常推进路径（PENDING/RETRYING → SENT/RETRYING/DEAD）；
// recordFromAny 额外含 DEAD/SENT：死信重放、崩溃接管与重复轮次后台账可能停在终态，
// MarkDelivery 的 state IN 条件让「同一结论重复回写」天然幂等。
// 注意 helpers.go 的 terminalDeliveryFrom() 刻意不含 PENDING：
// Outbox 行还在途时，不允许任何路径凭空把台账写成 SENT。
var (
	recordFromOpen = []int32{model.DeliveryStatePending, model.DeliveryStateRetrying}
	recordFromAny  = append(append([]int32(nil), recordFromOpen...), terminalDeliveryFrom()...)
)

// batchDelta 本轮某批次的投递投影增量。
type batchDelta struct {
	dispatched int32
	dead       int32
}

// roundOutcome 一轮投递推进的统计。
type roundOutcome struct {
	scanned  int32
	sent     int32
	retrying int32
	dead     int32
	// fenced 因租约被接管而无法落结论的行数（计入 scanned，不计入 sent/retrying/dead）。
	fenced    int32
	nextRunAt int64
	deltas    map[string]*batchDelta
}

func (o *roundOutcome) add(batchID string, dispatched, dead int32) {
	if batchID == "" {
		return
	}
	d := o.deltas[batchID]
	if d == nil {
		d = &batchDelta{}
		o.deltas[batchID] = d
	}
	d.dispatched += dispatched
	d.dead += dead
}

// runPendingRound 推进一批到期的 Outbox 行（ClaimDue → 逐行投递 → 状态机迁移）。
//
// 取时点只有一个（now 由调用方给，与服务端接收口径一致），否则「按 t1 判定退避、
// 按 t2 写 next_retry_at」会让同一行的退避序列不可复现。
func runPendingRound(ctx context.Context, s *svc.ServiceContext, sender pendingSender,
	topic string, now int64, limit int32, operator string) (*roundOutcome, error) {

	if sender == nil {
		return nil, model.ErrDispatcherMissing
	}
	// 投递重试预算取自 ACTIVE 策略（与采集侧同一份 resolveLimits）：
	// 无 ACTIVE 策略时它回落到 config，退避参数仍然可用，投递推进不该因策略缺失而停摆。
	lim, err := resolveLimits(ctx, s, "")
	if err != nil {
		return nil, err
	}
	lease := s.Config.Dispatch.LeaseSeconds
	if lease <= 0 {
		lease = 1
	}
	worker := workerID(operator, now)
	out := &roundOutcome{deltas: make(map[string]*batchDelta)}

	// 先回收过期租约：worker 崩溃后行会停在 RETRYING + 已过期租约，
	// 不回收就会出现「永久卡在投递中」的行（ClaimDue 只捞租约空闲/到期的行）。
	if reaped, rerr := s.Pending.ReapExpiredLeases(ctx, now, limit); rerr != nil {
		logx.Errorf("event-collector/logic: 回收过期投递租约失败（本轮继续，租约到期后自愈）: %v", rerr)
	} else if reaped > 0 {
		logx.Infof("event-collector/logic: 回收过期投递租约 %d 行 worker=%s", reaped, worker)
	}

	rows, err := s.Pending.ClaimDue(ctx, topic, now, now+lease, worker, limit)
	if err != nil {
		return nil, err
	}
	out.scanned = int32(len(rows))
	var latestRetry int64
	for _, row := range rows {
		if row == nil {
			continue
		}
		res := settleRow(ctx, s, sender, row, lim, worker, now)
		switch res {
		case rowResultSent:
			out.sent++
			out.add(row.BatchID, 1, 0)
		case rowResultRetrying:
			out.retrying++
			if row.NextRetryAt > latestRetry {
				latestRetry = row.NextRetryAt
			}
		case rowResultDead:
			out.dead++
			out.add(row.BatchID, 0, 1)
		case rowResultFenced:
			out.fenced++
		}
	}
	out.nextRunAt = nextRoundAt(now, latestRetry, s.Config.Dispatch.RetryBaseSeconds)

	// 批次投影与收敛放在逐行之后：一次本轮同批只更新一行计数（增量聚合），
	// 减少热点行的 CAS 冲突。
	for batchID, d := range out.deltas {
		if serr := settleBatch(ctx, s, batchID, d.dispatched, d.dead); serr != nil {
			logx.Errorf("event-collector/logic: 批次投递投影收敛失败 batch_id=%s: %v", fitColumn(batchID, maxBatchIDBytes), serr)
			// 投影失败不回滚已完成的投递：Outbox 是真值，缺口由批次计数重算任务补齐
			// （README 已知缺口）。这里明确报错而不是吞掉，运维能从日志看到需要重算。
		}
	}
	return out, nil
}

// rowResult 单行投递结论。
type rowResult int

const (
	rowResultSent rowResult = iota
	rowResultRetrying
	rowResultDead
	rowResultFenced
)

// settleRow 投递单行并完成状态机迁移。
//
// 顺序：真正发送 → 用带 worker 围栏的 CAS 写 Outbox 终态 → 回写台账投影。
// 反过来（先写库再发送）会在「写完库进程崩溃」时留下永不再投的行。
func settleRow(ctx context.Context, s *svc.ServiceContext, sender pendingSender,
	row *model.PendingDelivery, lim limits, worker string, now int64) rowResult {

	attempts := row.Attempts + 1
	sendErr := sender.Send(ctx, row)
	if sendErr != nil {
		logx.Errorf("event-collector/logic: 投递失败 event_id=%s topic=%s attempts=%d reason=%s: %v",
			fitColumn(row.EventID, maxEventIDBytes), fitColumn(row.Topic, maxTopicBytes), attempts,
			classifyDeliveryError(sendErr), errorText(sendErr))
		return failRow(ctx, s, row, lim, worker, attempts, sendErr, now)
	}

	applied, err := s.Pending.MarkSent(ctx, row.ID, worker, now)
	if err != nil {
		// 已发出但状态没写上：下一轮租约到期后会重投，下游按信封 event_id 去重
		// （spm 侧同一唯一键），所以「重复发送」不会产生第二条事实；
		// 这里按未收敛计数（retrying）而不是 sent，避免对外报一个偏乐观的成功数。
		logx.Errorf("event-collector/logic: MarkSent 失败 event_id=%s: %v", fitColumn(row.EventID, maxEventIDBytes), err)
		return rowResultRetrying
	}
	if !applied {
		logx.Errorf("event-collector/logic: 投递租约已被接管，本实例放弃该结论 event_id=%s worker=%s",
			fitColumn(row.EventID, maxEventIDBytes), fitColumn(worker, maxLeaseOwnerBytes))
		return rowResultFenced
	}
	markRecordDelivery(ctx, s, row.EventID, model.DeliveryStateSent, row.Topic, attempts, 0, "")
	return rowResultSent
}

// failRow 处理投递失败：退避重试或转死信。
func failRow(ctx context.Context, s *svc.ServiceContext, row *model.PendingDelivery, lim limits,
	worker string, attempts int32, sendErr error, now int64) rowResult {

	reason := classifyDeliveryError(sendErr)
	detail := fitColumn("["+reason+"] "+errorText(sendErr), maxReasonBytes)

	if model.ShouldDeadLetter(attempts, lim.deliverMaxAttempts) {
		// 先写死信、再把 Outbox 转 DEAD：
		// 反序会出现「Outbox 已 DEAD 但没有死信行」——那意味着事件既没投出去也没人知道，
		// 是三者中最不可恢复的状态。InsertIgnore 幂等，重跑不会覆盖首次失败原因。
		if derr := writeDeadLetter(ctx, s, row, reason, detail, attempts); derr != nil {
			logx.Errorf("event-collector/logic: 写死信失败，保留 Outbox 待重试 event_id=%s: %v",
				fitColumn(row.EventID, maxEventIDBytes), derr)
			// 释放租约把行交还队列，不消耗重试预算（这次根本没成功写到该记的地方）。
			if _, rerr := s.Pending.ReleaseLease(ctx, row.ID, worker); rerr != nil {
				logx.Errorf("event-collector/logic: 释放租约失败 event_id=%s: %v",
					fitColumn(row.EventID, maxEventIDBytes), rerr)
			}
			return rowResultFenced
		}
		applied, err := s.Pending.MarkDead(ctx, row.ID, worker, attempts, detail)
		if err != nil || !applied {
			logx.Errorf("event-collector/logic: MarkDead 未生效 event_id=%s err=%v",
				fitColumn(row.EventID, maxEventIDBytes), err)
			return rowResultFenced
		}
		markRecordDelivery(ctx, s, row.EventID, model.DeliveryStateDead, row.Topic, attempts, 0, detail)
		return rowResultDead
	}

	next := model.NextRetryAt(now, attempts, lim.retryBaseSeconds, lim.retryMaxSeconds)
	applied, err := s.Pending.MarkRetry(ctx, row.ID, worker, attempts, next, detail)
	if err != nil || !applied {
		logx.Errorf("event-collector/logic: MarkRetry 未生效 event_id=%s err=%v",
			fitColumn(row.EventID, maxEventIDBytes), err)
		return rowResultFenced
	}
	markRecordDelivery(ctx, s, row.EventID, model.DeliveryStateRetrying, row.Topic, attempts, next, detail)
	return rowResultRetrying
}

// markRecordDelivery 回写台账投递投影（best-effort + 显式日志）。
//
// 不用事务包一层：Outbox 真值已经落定，投影写失败时把整轮变成错误反而掩盖真实投递结果。
// 差异由批次/台账重算任务修正（README 已知缺口）。
func markRecordDelivery(ctx context.Context, s *svc.ServiceContext, eventID string, to int32,
	topic string, attempts int32, nextRetryAt int64, lastError string) {

	applied, err := s.Records.MarkDelivery(ctx, nil, eventID, recordFromAny, to, topic, attempts,
		nextRetryAt, fitColumn(lastError, maxReasonBytes))
	if err != nil {
		logx.Errorf("event-collector/logic: 台账投递投影回写失败 event_id=%s to=%d: %v",
			fitColumn(eventID, maxEventIDBytes), to, err)
		return
	}
	if !applied {
		logx.Errorf("event-collector/logic: 台账投递投影未迁移（行不存在或已被并发推进）event_id=%s to=%d",
			fitColumn(eventID, maxEventIDBytes), to)
	}
}

// writeDeadLetter 写一条投递死信（原文不入库，只存摘要与脱敏后的失败分类）。
func writeDeadLetter(ctx context.Context, s *svc.ServiceContext, row *model.PendingDelivery,
	reason, detail string, attempts int32) error {

	dl := &model.DeadLetter{
		EventID:       fitColumn(row.EventID, maxEventIDBytes),
		BatchID:       fitColumn(row.BatchID, maxBatchIDBytes),
		Topic:         fitColumn(row.Topic, maxTopicBytes),
		PayloadDigest: fitColumn(row.PayloadDigest, maxDigestBytes),
		Reason:        fitColumn(reason, maxReasonClassBytes),
		ReasonDetail:  fitColumn(detail, maxReasonBytes),
		Attempts:      attempts,
		State:         model.DeadLetterOpen,
		CreatedAt:     timeNow(),
	}
	// 归因列来自台账（event_type 只有那里有）：读不到不阻断死信写入——
	// 死信行的第一职责是「这条事件卡住了、为什么」，缺一个 event_type 不影响它被处置。
	if rec, err := s.Records.FindByEventID(ctx, row.EventID); err == nil && rec != nil {
		dl.EventType = fitColumn(rec.EventType, maxEventTypeBytes)
		if dl.PayloadDigest == "" {
			dl.PayloadDigest = fitColumn(rec.PayloadDigest, maxDigestBytes)
		}
		if dl.BatchID == "" {
			dl.BatchID = fitColumn(rec.BatchID, maxBatchIDBytes)
		}
	} else if err != nil && !model.IsNotFound(err) {
		return fmt.Errorf("event-collector: 回查台账以写死信失败 event_id=%s: %w",
			fitColumn(row.EventID, maxEventIDBytes), err)
	}
	if dl.BatchID == "" {
		// batch_id 为空的死信无法回溯上报方，宁可不写也不要留一条查不到来源的记录：
		// 交给 Outbox 下一轮重试（调用方会把本轮标成未收敛）。
		return fmt.Errorf("event-collector: 死信缺少 batch_id，拒绝写入 event_id=%s",
			fitColumn(row.EventID, maxEventIDBytes))
	}
	created, err := s.DeadLetters.InsertIgnore(ctx, nil, dl)
	if err != nil {
		return err
	}
	if !created {
		logx.Infof("event-collector/logic: 死信行已存在（uniq_event_topic），保留首次原因 event_id=%s topic=%s",
			fitColumn(row.EventID, maxEventIDBytes), fitColumn(row.Topic, maxTopicBytes))
	}
	return nil
}

// settleBatch 累加批次投递投影并在整批收敛时写 finished_at。
//
// 有死信的批次最终停在 PARTIAL：Finish 会把状态写成 DISPATCHED（= 全部已发送），
// 对出现死信的批次那是个假结论，所以同事务里再迁到 PARTIAL 并保留原 top_reason。
func settleBatch(ctx context.Context, s *svc.ServiceContext, batchID string,
	dispatchedDelta, deadDelta int32) error {

	return s.DB.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		bm := s.Batches.WithSession(session)
		if dispatchedDelta != 0 || deadDelta != 0 {
			applied, err := bm.MarkDispatchProgress(ctx, session, batchID, dispatchedDelta, deadDelta)
			if err != nil {
				return err
			}
			if !applied {
				return fmt.Errorf("%w: 批次 %s 投递投影累加未生效（批次行不存在？）",
					model.ErrConcurrentUpdate, fitColumn(batchID, maxBatchIDBytes))
			}
		}
		open, err := s.Records.WithSession(session).CountOpenByBatch(ctx, batchID)
		if err != nil {
			return err
		}
		if open > 0 {
			// 仍有在途事件：批次没收敛，finished_at 必须留在 0。
			return nil
		}
		row, err := bm.FindByBatchID(ctx, batchID)
		if err != nil {
			return err
		}
		if _, err := bm.Finish(ctx, session, batchID); err != nil {
			return err
		}
		if row.Dead > 0 {
			if _, err := bm.MarkState(ctx, session, batchID,
				[]int32{model.BatchStateDispatched, model.BatchStateValidated, model.BatchStateDispatching,
					model.BatchStatePartial},
				model.BatchStatePartial, row.TopReason, row.LastError); err != nil {
				return err
			}
		}
		return nil
	})
}

// nextRoundAt 计算建议的下一轮时刻。
//
// 口径（proto next_run_at 注释）：本轮 claimed 行里最晚的 next_retry_at 与「批间隔」取小 ——
// 取「最晚」保证每条被领取的行都有机会重试，取小则保证 cron 不会按小时级退避空转。
func nextRoundAt(now, latestRetry, gapSeconds int64) int64 {
	gap := gapSeconds
	if gap <= 0 {
		gap = 1
	}
	at := now + gap
	if latestRetry > 0 && latestRetry < at {
		at = latestRetry
	}
	if at < now {
		// 退避时间已被外部改到过去：至少回到 now，否则 cron 会形成忙等循环打爆 DB。
		return now
	}
	return at
}

// replayOneDeadLetter 处置单条死信：open→replayed + 重新入队 + 台账回 PENDING（同一事务）。
//
// 逐行成事而非批量 CAS：一次重放里「已处置的」「Outbox 行已被清理的」「正常入队的」
// 三类行会同时出现，只有逐行才能给出 replayed/skipped/failed 的诚实拆分；
// 单行事务失败自动回滚，该行留在 open，不出现「已重放但没入队」的假象。
//
// 不重新采样、不重新脱敏：首次入库时定型的 policy_version/sanitize_version 保持归因
// （要改判必须 ActivateDispatchPolicy 新版本 + 数据回填，见 proto 注释）。
// 重新入队刻意复用原 envelope_event_id：下游按信封 event_id 去重，
// 所以「重放」不可能把一条已投递成功的事件变成第二条事实。
func replayOneDeadLetter(ctx context.Context, s *svc.ServiceContext, dl *model.DeadLetter,
	operator, idemKey, reason string, now int64) (replayed bool, failed error) {

	err := s.DB.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		applied, err := s.DeadLetters.WithSession(session).MarkHandled(ctx, session,
			[]int64{dl.ID}, model.DeadLetterReplayed, fitColumn(operator, maxOperatorBytes),
			fitColumn(idemKey, maxIdemKeyBytes), redactSensitive(reason), now)
		if err != nil {
			return err
		}
		if applied == 0 {
			// 只可能来自 state=open 的条件不成立：该行已是终态，重复重放按 skipped 计。
			return errSkipReplay
		}
		requeued, err := s.Pending.WithSession(session).Requeue(ctx, session, dl.EventID, dl.Topic, "")
		if err != nil {
			return err
		}
		if !requeued {
			exists, err := outboxRowExists(ctx, s, session, dl.EventID, dl.Topic)
			if err != nil {
				return err
			}
			if !exists {
				// Outbox 行已被清理任务删掉：死信表不存正文，也没有 envelope_event_id，
				// 无从重建一条投递行。此时必须回滚成 open 并进 failed_ids，
				// 绝不能留下「标了 replayed 但永远不会有人投」的死信。
				return fmt.Errorf("%w: ec_pending_delivery 行不存在，无法重建投递（正文不入库）",
					model.ErrPendingNotFound)
			}
			// 行已在队列里（PENDING/RETRYING）或已发送：不重复入队，重放结论仍然成立。
			logx.Infof("event-collector/logic: 死信对应投递行不在 DEAD，按不重复入队处理 event_id=%s topic=%s",
				fitColumn(dl.EventID, maxEventIDBytes), fitColumn(dl.Topic, maxTopicBytes))
		}
		if _, err := s.Records.WithSession(session).MarkDelivery(ctx, session, dl.EventID,
			[]int32{model.DeliveryStateDead}, model.DeliveryStatePending, dl.Topic, 0,
			model.NextRetryAt(now, 0, s.Config.Dispatch.RetryBaseSeconds, s.Config.Dispatch.RetryMaxSeconds),
			""); err != nil {
			return err
		}
		return nil
	})
	switch {
	case errors.Is(err, errSkipReplay):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// outboxRowExists 判断某 (event_id, topic) 的投递行是否存在。
func outboxRowExists(ctx context.Context, s *svc.ServiceContext, session sqlx.Session,
	eventID, topic string) (bool, error) {

	rows, err := s.Pending.WithSession(session).ListByEventID(ctx, eventID)
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if r != nil && r.Topic == strings.TrimSpace(topic) {
			return true, nil
		}
	}
	return false, nil
}

// canonicalTopics 列出本服务理论上会产出的全部 topic（健康度里「这个 topic 根本没在跑」
// 也要看得见）。
//
// 组合口径与采集侧同源：event_type 白名单（categorySuffix）× 1..SupportedSchemaVersion
// 经 topicForEvent 生成，不硬编码字符串，否则新增类别时健康面板会静默漏一栏。
func canonicalTopics(s *svc.ServiceContext) []string {
	serverMax := s.Config.Collector.SupportedSchemaVersion
	if serverMax < model.MinSupportedSchemaVersion {
		serverMax = model.MinSupportedSchemaVersion
	}
	out := make([]string, 0, len(categorySuffix)*int(serverMax-model.MinSupportedSchemaVersion+1))
	for _, suffix := range categorySuffix {
		for v := int32(model.MinSupportedSchemaVersion); v <= serverMax; v++ {
			out = append(out, topicForEvent(eventTypePrefix+suffix, v))
		}
	}
	return out
}

// mergeTopicStats 把「实测积压」与「理论 topic 清单」并起来，缺项补 0。
func mergeTopicStats(stats []model.TopicStat, deadOpen map[string]int64, canonical []string) []topicHealth {
	byTopic := make(map[string]topicHealth, len(stats)+len(canonical))
	for _, st := range stats {
		h := byTopic[st.Topic]
		h.topic = st.Topic
		h.pending = st.Pending
		h.retrying = st.Retrying
		h.sentLastHour = st.SentLastHour
		h.oldestPending = st.OldestPendingCtime
		byTopic[st.Topic] = h
	}
	for topic, n := range deadOpen {
		h := byTopic[topic]
		h.topic = topic
		h.deadOpen = n
		byTopic[topic] = h
	}
	for _, topic := range canonical {
		if _, ok := byTopic[topic]; !ok {
			byTopic[topic] = topicHealth{topic: topic}
		}
	}
	out := make([]topicHealth, 0, len(byTopic))
	for _, h := range byTopic {
		out = append(out, h)
	}
	sortTopicHealth(out)
	return out
}

// sortTopicHealth topic 升序固定输出：健康面板做同比时顺序抖动会被误读成「积压换了 topic」。
func sortTopicHealth(list []topicHealth) {
	sort.Slice(list, func(i, j int) bool { return list[i].topic < list[j].topic })
}

// topicHealth 单个 topic 的健康视图（映射到 rpc.TopicHealth 在 logic 方法内做）。
type topicHealth struct {
	topic         string
	pending       int64
	retrying      int64
	deadOpen      int64
	sentLastHour  int64
	oldestPending int64
}
