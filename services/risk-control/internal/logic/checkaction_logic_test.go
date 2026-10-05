package logic

// checkaction_logic_test.go 覆盖降级链黑名单 → 生效处罚 → 白名单 → 兜底限流 → 规则 → 降级。
//
// 被测路径是**真实 Repository + 真实 policy.Engine**（见 fakes_test.go 的装配说明），
// 所以这里断言的不是「logic 有没有转调」，而是整条链路的四类要紧结论：
//  1. 依赖故障必须返回带 basis/degraded 的可解释裁决，而不是裸错误；
//  2. 命中顺序与 skipped_rule_ids 的真实性（「读不到」绝不冒充「0 命中」）；
//  3. 明文 IP / 设备号原文不得进入日志列、缓存值或响应；
//  4. 调用序列可复现：规则缓存命中即不回源、降级即跳过 risk_check_log。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"go-video/services/risk-control/internal/policy"
	"go-video/services/risk-control/model"
	"go-video/services/risk-control/rpc"
)

const testMid = int64(42)

func commentReq(requestID string) *rpc.CheckActionReq {
	return &rpc.CheckActionReq{
		RequestId: requestID,
		Mid:       testMid,
		Action:    rpc.GuardedAction_ACTION_COMMENT,
		DeviceId:  rawDeviceSN,
		IpHash:    ipHashHex,
	}
}

func checkAction(t *testing.T, st *store, in *rpc.CheckActionReq) (*rpc.CheckActionReply, error) {
	t.Helper()
	return NewCheckActionLogic(context.Background(), st.svcCtx).CheckAction(in)
}

// counterRule 是一条频率规则（窗口 60 → 档位 60 → 桶 10 → 固定 7 个桶 key）。
func counterRule(name string, metric string, threshold int64, window int64, decision, priority int32) model.RiskRule {
	return model.RiskRule{
		Name: name, ActionType: model.ActionComment, Metric: metric,
		Op: model.OpGTE, Threshold: threshold, WindowSeconds: window,
		Decision: decision, Priority: priority, State: model.StateEnabled, Version: 1,
	}
}

func TestCheckActionRuleHitOrderAndSkippedTruthfulness(t *testing.T) {
	st := newStore(t)
	dh := devHash()

	rA := st.seedRule(counterRule("r-A", model.MetricActionCount, 5, 60, model.DecisionBlock, 9))
	rB := st.seedRule(model.RiskRule{Name: "r-B", ActionType: model.ActionComment, Metric: model.MetricDeviceRiskScore,
		Op: model.OpGTE, Threshold: 80, WindowSeconds: 60, Decision: model.DecisionReview,
		Priority: 7, State: model.StateEnabled, Version: 1})
	rC := st.seedRule(counterRule("r-C", model.MetricDeviceActionCount, 5, 60, model.DecisionChallenge, 5))
	// 观测值存在但未过阈值：既不是 skipped 也不是 hit。
	st.seedRule(counterRule("r-E", model.MetricActionCount, 1000, 600, model.DecisionBlock, 3))
	// 窗口 7200 > 最大档位 3600：不可观测，必须进 skipped，绝不退化成近似窗口。
	rD := st.seedRule(counterRule("r-D", model.MetricIpActionCount, 5, 7200, model.DecisionBlock, 1))

	st.seedDevice(model.RiskDeviceProfile{DeviceHash: dh, RiskScore: 90, RelatedMidCount: 2})
	st.redis.seedCounter(model.MetricActionCount, subjectOfMid(testMid), model.ActionComment, 60, 10)
	st.redis.seedCounter(model.MetricDeviceActionCount, subjectOfDevice(dh), model.ActionComment, 60, 6)

	reply, err := checkAction(t, st, commentReq("req-hits"))
	wantNoErr(t, "CheckAction", err)

	wantOps(t, "调用序列", st.log.all(), []string{
		"redis.Get:rc:ck:req-hits",
		"list.FindActive:3",
		"punish.ExpireStale:42",
		"punish.ListActive:42",
		"redis.Get:rc:rl:2",
		"rule.ListActive:2",
		"redis.Setex:rc:rl:2",
		"redis.Mget:7", // r-A：窗口 60 → 档位 60 → 7 桶
		"device.FindOne:" + dh,
		"redis.Mget:7", // r-C
		"redis.Mget:7", // r-E
		"log.Insert:req-hits",
		"redis.Setex:rc:ck:req-hits",
	})
	// r-D 不可观测：一个 MGET 都不该为它发出去（近似冒充会误判）。
	if n := st.log.countPrefix("redis.Mget"); n != 3 {
		t.Fatalf("MGET 次数 = %d, want 3（不可观测规则不得读桶）", n)
	}

	wantEQ(t, "裁决", "decision", int32(reply.Decision), int32(rpc.Decision_DECISION_BLOCK))
	wantEQ(t, "裁决", "basis", reply.Basis, policy.BasisRules)
	wantEQ(t, "裁决", "evaluated", reply.Evaluated, true)
	wantEQ(t, "裁决", "degraded", reply.Degraded, false)
	wantEQ(t, "裁决", "action_code", reply.ActionCode, policy.ActionCodeBlocked)
	// 命中顺序固定 (priority DESC, rule_id ASC)，与观测值大小无关。
	wantInt64s(t, "裁决", "hit_rule_ids", reply.HitRuleIds, []int64{rA, rB, rC})
	wantInt64s(t, "裁决", "skipped_rule_ids", reply.SkippedRuleIds, []int64{rD})
	// BLOCK 基线 100 + 每多命中一条 +5，上限 100。
	wantEQ(t, "裁决", "score", reply.Score, int32(100))
	if len(reply.RuleHits) != 3 {
		t.Fatalf("rule_hits = %d, want 3", len(reply.RuleHits))
	}
	for i, h := range reply.RuleHits {
		wantEQ(t, fmt.Sprintf("rule_hits[%d]", i), "rule_id", h.RuleId, reply.HitRuleIds[i])
		wantEQ(t, fmt.Sprintf("rule_hits[%d]", i), "version", h.Version, 1)
	}
	wantEQ(t, "rule_hits[0]", "observed", reply.RuleHits[0].Observed, int64(10))
	wantEQ(t, "rule_hits[0]", "metric", int32(reply.RuleHits[0].Metric), int32(rpc.Metric_METRIC_ACTION_COUNT))
	wantEQ(t, "rule_hits[1]", "metric", int32(reply.RuleHits[1].Metric), int32(rpc.Metric_METRIC_DEVICE_RISK_SCORE))
	wantEQ(t, "rule_hits[1]", "observed", reply.RuleHits[1].Observed, int64(90))
	wantEQ(t, "rule_hits[2]", "observed", reply.RuleHits[2].Observed, int64(6))
	if reply.Punishment != nil {
		t.Fatalf("无处罚时不得下发快照: %+v", reply.Punishment)
	}

	// 审计列只允许受控标识。
	row, ok := st.checkLogRow("req-hits")
	if !ok {
		t.Fatal("risk_check_log 未落库")
	}
	wantEQ(t, "审计行", "device_hash", row.DeviceHash, dh)
	wantEQ(t, "审计行", "ip_hash", row.IPHash, ipHashHex)
	wantEQ(t, "审计行", "basis", row.Basis, policy.BasisRules)
	wantEQ(t, "审计行", "degraded", row.Degraded, int32(0))
	wantEQ(t, "审计行", "hit_rule_ids", row.HitRuleIDs,
		model.FormatHits([]int64{rA, rB, rC}, []int32{1, 1, 1}))
}

func TestCheckActionSecondCallHitsRuleCache(t *testing.T) {
	st := newStore(t)
	st.seedRule(counterRule("r-none", model.MetricActionCount, 5, 60, model.DecisionBlock, 9))

	first, err := checkAction(t, st, commentReq("req-c1"))
	wantNoErr(t, "首次裁决", err)
	wantEQ(t, "首次裁决", "basis", first.Basis, policy.BasisNoRule)

	second, err := checkAction(t, st, commentReq("req-c2"))
	wantNoErr(t, "二次裁决", err)
	wantEQ(t, "二次裁决", "basis", second.Basis, policy.BasisNoRule)

	wantOps(t, "第二次调用序列", st.log.opsFrom(len([]string{
		"redis.Get:rc:ck:req-c1", "list.FindActive:3", "punish.ExpireStale:42", "punish.ListActive:42",
		"redis.Get:rc:rl:2", "rule.ListActive:2", "redis.Setex:rc:rl:2", "redis.Mget:7",
		"log.Insert:req-c1", "redis.Setex:rc:ck:req-c1",
	})), []string{
		"redis.Get:rc:ck:req-c2",
		"list.FindActive:3",
		"punish.ExpireStale:42",
		"punish.ListActive:42",
		"redis.Get:rc:rl:2", // 命中规则缓存：不回源
		"redis.Mget:7",
		"log.Insert:req-c2",
		"redis.Setex:rc:ck:req-c2",
	})
	if n := st.log.countPrefix("rule.ListActive"); n != 1 {
		t.Fatalf("rule.ListActive 次数 = %d, want 1（规则缓存必须生效）", n)
	}
	// 规则缓存的 TTL 必须来自配置，而不是随手写死。
	wantEQ(t, "规则缓存", "ttl", st.redis.cacheTTL["rc:rl:2"], testRuleCacheS)
	wantEQ(t, "裁决回放缓存", "ttl", st.redis.cacheTTL["rc:ck:req-c2"], testDecisionCacheS)
}

func TestCheckActionTrimsHitDetailsButNotScore(t *testing.T) {
	st := newStore(t, withMaxHits(2))
	for _, n := range []string{"r-t1", "r-t2", "r-t3"} {
		st.seedRule(counterRule(n, model.MetricActionCount, 5, 60, model.DecisionChallenge, 9))
	}
	// 三条同优先级：rule_id ASC 决定顺序。全部命中（阈值 5，观测 9）。
	st.redis.seedCounter(model.MetricActionCount, subjectOfMid(testMid), model.ActionComment, 60, 9)

	reply, err := checkAction(t, st, commentReq("req-trim"))
	wantNoErr(t, "CheckAction", err)
	wantEQ(t, "裁剪", "decision", int32(reply.Decision), int32(rpc.Decision_DECISION_CHALLENGE))
	wantEQ(t, "裁剪", "len(rule_hits)", len(reply.RuleHits), 2)
	wantInt64s(t, "裁剪", "hit_rule_ids", reply.HitRuleIds, []int64{1, 2})
	// score 仍按**全部**命中计算：25 + 5*(3-1)。裁剪只减明细，不改结论置信度。
	wantEQ(t, "裁剪", "score", reply.Score, int32(35))
	// CHALLENGE 才带校验有效期。
	wantEQ(t, "裁剪", "challenge_ttl", reply.ChallengeTtlSeconds, int64(300))
	wantEQ(t, "裁剪", "action_code", reply.ActionCode, policy.ActionCodeChallenge)
}

func TestCheckActionBlacklistShortCircuitsEvaluation(t *testing.T) {
	st := newStore(t)
	st.seedListEntry(model.RiskList{ListType: model.ListTypeBlack, TargetType: model.TargetTypeMid,
		TargetValue: "42", Reason: "刷评", Operator: 9, State: model.StateEnabled})
	// 一条本来会命中的计数规则：黑名单必须让它完全不参与结论。
	st.seedRule(counterRule("r-bad", model.MetricActionCount, 5, 60, model.DecisionBlock, 9))
	st.redis.seedCounter(model.MetricActionCount, subjectOfMid(testMid), model.ActionComment, 60, 99)

	reply, err := checkAction(t, st, commentReq("req-black"))
	wantNoErr(t, "CheckAction", err)
	wantOps(t, "调用序列", st.log.all(), []string{
		"redis.Get:rc:ck:req-black",
		"list.FindActive:3",
		"punish.ExpireStale:42",
		"punish.ListActive:42",
		"redis.Get:rc:rl:2",
		"rule.ListActive:2",
		"redis.Setex:rc:rl:2",
		"redis.Mget:7",
		"log.Insert:req-black",
		"redis.Setex:rc:ck:req-black",
	})
	wantEQ(t, "黑名单", "decision", int32(reply.Decision), int32(rpc.Decision_DECISION_BLOCK))
	wantEQ(t, "黑名单", "basis", reply.Basis, policy.BasisBlacklist)
	wantEQ(t, "黑名单", "score", reply.Score, int32(100))
	wantEQ(t, "黑名单", "action_code", reply.ActionCode, policy.ActionCodeBlocked)
	wantEQ(t, "黑名单", "evaluated", reply.Evaluated, false)
	wantEQ(t, "黑名单", "degraded", reply.Degraded, false)
	if len(reply.HitRuleIds) != 0 || len(reply.RuleHits) != 0 {
		t.Fatalf("黑名单命中时不得出现规则命中: %v", reply.HitRuleIds)
	}
	// 已知缺口：命中说明只在 Facts 里，Result/响应/落库都没有它，
	// 运营拿到 basis=blacklist 无从知道是哪条条目（README 缺口 #18）。
	if strings.Contains(fmt.Sprintf("%v", reply), "刷评") {
		t.Fatalf("运营内部说明不得下发: %v", reply)
	}
}

func TestCheckActionWhitelistSkipsEvaluationButNotFactReads(t *testing.T) {
	st := newStore(t)
	st.seedListEntry(model.RiskList{ListType: model.ListTypeWhite, TargetType: model.TargetTypeMid,
		TargetValue: "42", Operator: 9, State: model.StateEnabled})
	st.seedRule(counterRule("r-white", model.MetricActionCount, 5, 60, model.DecisionBlock, 9))
	st.redis.seedCounter(model.MetricActionCount, subjectOfMid(testMid), model.ActionComment, 60, 99)

	reply, err := checkAction(t, st, commentReq("req-white"))
	wantNoErr(t, "CheckAction", err)
	wantEQ(t, "白名单", "decision", int32(reply.Decision), int32(rpc.Decision_DECISION_ALLOW))
	wantEQ(t, "白名单", "basis", reply.Basis, policy.BasisWhitelist)
	wantEQ(t, "白名单", "score", reply.Score, int32(0))
	wantEQ(t, "白名单", "action_code", reply.ActionCode, "")
	wantEQ(t, "白名单", "evaluated", reply.Evaluated, false)
	if len(reply.SkippedRuleIds) != 0 {
		t.Fatalf("白名单跳过评估，不应把规则算成 skipped: %v", reply.SkippedRuleIds)
	}
	// 如实记录生产行为：白名单只免「评估」，不免事实装载 —— 计数桶仍被读过一次。
	if n := st.log.countPrefix("redis.Mget"); n != 1 {
		t.Fatalf("MGET 次数 = %d, want 1（白名单不减少读数放大）", n)
	}
}

func TestCheckActionActivePunishmentBeatsWhitelist(t *testing.T) {
	st := newStore(t)
	now := time.Now().Unix()
	st.seedListEntry(model.RiskList{ListType: model.ListTypeWhite, TargetType: model.TargetTypeMid,
		TargetValue: "42", Operator: 9, State: model.StateEnabled})
	st.seedPunishment(model.RiskPunishment{Mid: testMid, Scope: model.ActionAll, Decision: model.DecisionChallenge,
		Reason: "白名单也不能替运营解罚", ReasonCode: "risk.punish.challenge", Operator: 9,
		StartAt: now - 60, EndAt: now + 120, State: model.PunishmentStateActive, IdempotencyKey: "pk-1"})

	reply, err := checkAction(t, st, commentReq("req-punish"))
	wantNoErr(t, "CheckAction", err)
	wantEQ(t, "处罚优先", "basis", reply.Basis, policy.BasisPunishment)
	wantEQ(t, "处罚优先", "decision", int32(reply.Decision), int32(rpc.Decision_DECISION_CHALLENGE))
	wantEQ(t, "处罚优先", "score", reply.Score, int32(25))
	wantEQ(t, "处罚优先", "action_code", reply.ActionCode, policy.ActionCodePunished)
	if reply.Punishment == nil {
		t.Fatal("生效处罚必须下发快照，客户端要渲染剩余时长")
	}
	wantEQ(t, "处罚快照", "scope", int32(reply.Punishment.Scope), int32(model.ActionAll))
	wantEQ(t, "处罚快照", "permanent", reply.Punishment.Permanent, false)
	wantEQ(t, "处罚快照", "reason_code", reply.Punishment.ReasonCode, "risk.punish.challenge")
	// 端上摘要故意不含 reason / operator（AGENTS.md §6）。
	if strings.Contains(fmt.Sprintf("%v", reply.Punishment), "白名单也不能替运营解罚") {
		t.Fatalf("处罚 reason 不得下发终端: %+v", reply.Punishment)
	}
	// 时间只能落在 [119,120]：challenge_ttl = min(配置 300, 剩余秒数)。
	wantRangeInt64(t, "处罚快照", "remaining_seconds", reply.Punishment.RemainingSeconds, 119, 120)
	wantRangeInt64(t, "裁决", "challenge_ttl", reply.ChallengeTtlSeconds, 119, 120)

	// 规则评估被完全跳过（处罚优先于规则），但处罚读取必然先发生。
	wantOps(t, "调用序列", st.log.all(), []string{
		"redis.Get:rc:ck:req-punish",
		"list.FindActive:3",
		"punish.ExpireStale:42",
		"punish.ListActive:42",
		"redis.Get:rc:rl:2",
		"rule.ListActive:2",
		"redis.Setex:rc:rl:2",
		"log.Insert:req-punish",
		"redis.Setex:rc:ck:req-punish",
	})
}

func TestCheckActionPunishmentExpiryIsAdvancedOnRead(t *testing.T) {
	st := newStore(t)
	now := time.Now().Unix()
	id := st.seedPunishment(model.RiskPunishment{Mid: testMid, Scope: model.ActionComment,
		Decision: model.DecisionBlock, ReasonCode: "risk.punish.block", Operator: 9,
		StartAt: now - 300, EndAt: now - 1, State: model.PunishmentStateActive, IdempotencyKey: "pk-expire"})

	reply, err := checkAction(t, st, commentReq("req-expire"))
	wantNoErr(t, "CheckAction", err)
	wantEQ(t, "到期处罚", "basis", reply.Basis, policy.BasisNoRule)
	wantEQ(t, "到期处罚", "decision", int32(reply.Decision), int32(rpc.Decision_DECISION_ALLOW))
	// 读取即推进：不依赖 cron 是否跑过。
	wantEQ(t, "到期处罚", "库内 state", st.punishmentRow(id).State, model.PunishmentStateExpired)
	// 推进必须发生在列举生效处罚**之前**，否则过期处罚会先参与这一次裁决。
	ops := st.log.all()
	wantInt64s(t, "推进位序", "ExpireStale→ListActive 下标", []int64{
		int64(indexOfOp(t, ops, "punish.ExpireStale:42")),
		int64(indexOfOp(t, ops, "punish.ListActive:42")),
	}, []int64{2, 3})
}

func TestCheckActionReplaysCachedDecision(t *testing.T) {
	st := newStore(t)
	cached := &policy.Result{
		RequestID: "req-replay", Decision: model.DecisionBlock, Score: 100,
		HitRuleIDs: []int64{77}, HitVersions: []int32{3},
		Basis: policy.BasisRules, ActionCode: policy.ActionCodeBlocked, Evaluated: true,
	}
	bs, err := json.Marshal(cached)
	wantNoErr(t, "构造回放值", err)
	st.redis.warm("rc:ck:req-replay", string(bs))

	reply, err := checkAction(t, st, commentReq("req-replay"))
	wantNoErr(t, "CheckAction", err)
	wantOps(t, "回放只读缓存", st.log.all(), []string{"redis.Get:rc:ck:req-replay"})
	wantEQ(t, "回放", "decision", int32(reply.Decision), int32(rpc.Decision_DECISION_BLOCK))
	wantInt64s(t, "回放", "hit_rule_ids", reply.HitRuleIds, []int64{77})
	wantEQ(t, "回放", "score", reply.Score, int32(100))
}

func TestCheckActionCorruptedCacheIsTreatedAsMiss(t *testing.T) {
	st := newStore(t)
	st.redis.warm("rc:ck:req-corrupt", "{not-json")

	reply, err := checkAction(t, st, commentReq("req-corrupt"))
	wantNoErr(t, "CheckAction", err)
	wantOps(t, "调用序列", st.log.all(), []string{
		"redis.Get:rc:ck:req-corrupt",
		"list.FindActive:3",
		"punish.ExpireStale:42",
		"punish.ListActive:42",
		"redis.Get:rc:rl:2",
		"rule.ListActive:2",
		"redis.Setex:rc:rl:2",
		"log.Insert:req-corrupt",
		"redis.Setex:rc:ck:req-corrupt",
	})
	wantEQ(t, "缓存损坏", "basis", reply.Basis, policy.BasisNoRule)
	// 损坏值必须被新裁决覆盖，否则同一个 request_id 永远拿不到结论。
	wantEQ(t, "缓存损坏", "覆盖后的值含 request_id",
		strings.Contains(st.redis.kv["rc:ck:req-corrupt"], "req-corrupt"), true)
}

func TestCheckActionMissingRequestIDGetsReplayableID(t *testing.T) {
	st := newStore(t)
	reply, err := checkAction(t, st, commentReq(""))
	wantNoErr(t, "CheckAction", err)
	if reply.RequestId == "" {
		t.Fatal("缺省 request_id 必须由引擎补齐，否则调用方无法回放同一裁决")
	}
	row, ok := st.checkLogRow(reply.RequestId)
	if !ok {
		t.Fatalf("按回包 request_id 找不到审计行，kv=%v", keysOf(st.redis.kv))
	}
	wantEQ(t, "生成 ID", "落库 request_id", row.RequestID, reply.RequestId)
	if _, ok := st.redis.kv["rc:ck:"+reply.RequestId]; !ok {
		t.Fatalf("裁决回放缓存 key 必须与回包 request_id 一致，实际 kv=%v", keysOf(st.redis.kv))
	}
}

// --- 降级链：依赖故障必须是「可解释裁决」而不是裸错误 ---

func TestCheckActionDegradesOnListFailureWithExplainedDecision(t *testing.T) {
	boom := errors.New("list db down")
	st := newStore(t)
	st.list.failWith("FindActive", boom)

	reply, err := checkAction(t, st, commentReq("req-deg-list"))
	wantNoErr(t, "低危动作降级不得返回裸错误", err)
	wantEQ(t, "低危降级", "decision", int32(reply.Decision), int32(rpc.Decision_DECISION_ALLOW))
	wantEQ(t, "低危降级", "basis", reply.Basis, policy.BasisFallbackDB)
	wantEQ(t, "低危降级", "degraded", reply.Degraded, true)
	wantEQ(t, "低危降级", "evaluated", reply.Evaluated, false)
	wantEQ(t, "低危降级", "score", reply.Score, int32(0))
	wantEQ(t, "低危降级", "action_code", reply.ActionCode, "")
	// 关键：降级时**跳过** risk_check_log（不再压故障中的连接池），但仍写回放缓存。
	wantOps(t, "调用序列", st.log.all(), []string{
		"redis.Get:rc:ck:req-deg-list",
		"list.FindActive:3",
		"redis.Setex:rc:ck:req-deg-list",
	})
	if n := st.log.countPrefix("log.Insert"); n != 0 {
		t.Fatalf("降级路径不应写审计表，实际 %d 次", n)
	}
	// 降级结论也要能回放：第二次同 request_id 直接拿到同一个 BLOCK/ALLOW 裁决。
	st.list.clear("FindActive")
	second, err := checkAction(t, st, commentReq("req-deg-list"))
	wantNoErr(t, "重放降级裁决", err)
	wantEQ(t, "重放降级", "basis", second.Basis, policy.BasisFallbackDB)
	wantEQ(t, "重放降级", "decision", int32(second.Decision), int32(rpc.Decision_DECISION_ALLOW))
}

func TestCheckActionHighRiskActionBlocksOnRuleFailure(t *testing.T) {
	ruleDown := errors.New("rule db down")
	punishDown := errors.New("punishment db down")
	listDown := errors.New("list db down")
	st := newStore(t)
	st.rule.failWith("ListActiveByAction", ruleDown)

	reply, err := checkAction(t, st, &rpc.CheckActionReq{RequestId: "req-deg-rule", Mid: testMid,
		Action: rpc.GuardedAction_ACTION_LOGIN, DeviceId: rawDeviceSN})
	wantNoErr(t, "高危动作降级", err)
	wantOps(t, "调用序列", st.log.all(), []string{
		"redis.Get:rc:ck:req-deg-rule",
		"list.FindActive:2",
		"punish.ExpireStale:42",
		"punish.ListActive:42",
		"redis.Get:rc:rl:5",
		"rule.ListActive:5",
		"redis.Setex:rc:ck:req-deg-rule",
	})
	wantEQ(t, "高危降级", "decision", int32(reply.Decision), int32(rpc.Decision_DECISION_BLOCK))
	wantEQ(t, "高危降级", "basis", reply.Basis, policy.BasisFallbackDB)
	wantEQ(t, "高危降级", "action_code", reply.ActionCode, policy.ActionCodeUnavailable)
	wantEQ(t, "高危降级", "score", reply.Score, int32(100))
	wantEQ(t, "高危降级", "degraded", reply.Degraded, true)
	// 处罚读取失败同样打整条链路（三步里任何一步 error 都进降级）。
	st2 := newStore(t)
	st2.punish.failWith("ListActiveByMid", punishDown)
	reply2, err2 := checkAction(t, st2, &rpc.CheckActionReq{RequestId: "req-deg-punish", Mid: testMid,
		Action: rpc.GuardedAction_ACTION_SUBMIT_VIDEO})
	wantNoErr(t, "投稿降级", err2)
	wantOps(t, "调用序列", st2.log.opsFrom(len([]string{"redis.Get:rc:ck:req-deg-punish", "list.FindActive:1",
		"punish.ExpireStale:42", "punish.ListActive:42"})), []string{"redis.Setex:rc:ck:req-deg-punish"})
	wantEQ(t, "投稿降级", "decision", int32(reply2.Decision), int32(rpc.Decision_DECISION_BLOCK))

	// 配置成 block-on-error 时，低危动作也被拒 —— 故障面按配置放大，必须可观测。
	st3 := newStore(t, withOnDBFailureBlock())
	st3.list.failWith("FindActive", listDown)
	reply3, err3 := checkAction(t, st3, commentReq("req-deg-blockall"))
	wantNoErr(t, "全量 block 配置", err3)
	wantEQ(t, "全量 block", "decision", int32(reply3.Decision), int32(rpc.Decision_DECISION_BLOCK))
	wantEQ(t, "全量 block", "basis", reply3.Basis, policy.BasisFallbackDB)
	wantEQ(t, "全量 block", "degraded", reply3.Degraded, true)
}

func TestCheckActionCounterFailureIsUnobservableNotZero(t *testing.T) {
	boom := errors.New("redis down")
	st := newStore(t)
	st.seedRule(counterRule("r-count", model.MetricActionCount, 5, 60, model.DecisionBlock, 9))
	st.seedRule(model.RiskRule{Name: "r-device", ActionType: model.ActionComment, Metric: model.MetricDeviceRiskScore,
		Op: model.OpGTE, Threshold: 80, WindowSeconds: 60, Decision: model.DecisionBlock,
		Priority: 5, State: model.StateEnabled, Version: 1})
	// 桶里有值但读不到：结论只能是「不可观测」，不得当成 0 命中放行。
	st.redis.seedCounter(model.MetricActionCount, subjectOfMid(testMid), model.ActionComment, 60, 99)
	st.seedDevice(model.RiskDeviceProfile{DeviceHash: devHash(), RiskScore: 10})
	st.redis.failWith("Mget", boom)

	reply, err := checkAction(t, st, commentReq("req-blind"))
	wantNoErr(t, "计数故障", err)
	wantEQ(t, "计数故障", "basis", reply.Basis, policy.BasisNoRule)
	wantEQ(t, "计数故障", "decision", int32(reply.Decision), int32(rpc.Decision_DECISION_ALLOW))
	wantEQ(t, "计数故障", "degraded", reply.Degraded, true)
	wantEQ(t, "计数故障", "evaluated", reply.Evaluated, true)
	wantInt64s(t, "计数故障", "skipped_rule_ids", reply.SkippedRuleIds, []int64{1})
	// 一次 MGET 失败即判定计数器整体不可读：后续计数规则不再试读。
	if n := st.log.countPrefix("redis.Mget"); n != 1 {
		t.Fatalf("MGET 次数 = %d, want 1（失败后不得逐条重试放大故障）", n)
	}
	// 非计数类事实不受影响：设备画像仍被读取（故障面不扩散）。
	wantEQ(t, "计数故障", "画像读取次数", st.log.countPrefix("device.FindOne"), 1)
	// 审计行必须标出降级，否则事后回放会把「没风险」误读成结论。
	row, ok := st.checkLogRow("req-blind")
	if !ok {
		t.Fatal("计数故障不影响 DB，审计日志必须落库")
	}
	wantEQ(t, "审计行", "degraded", row.Degraded, int32(1))
	wantEQ(t, "审计行", "basis", row.Basis, policy.BasisNoRule)
}

func TestCheckActionLocalFallbackBlocksWhenCounterBlind(t *testing.T) {
	boom := errors.New("redis down")
	st := newStore(t, withLocalFallback(3))
	st.seedRule(counterRule("r-fb", model.MetricActionCount, 5, 60, model.DecisionBlock, 9))

	// 兜底窗口只由上报驱动（repository 只在 Report 里 observe）。
	for i := 0; i < 4; i++ {
		out, err := NewReportActionLogic(context.Background(), st.svcCtx).ReportAction(&rpc.ReportActionReq{
			Mid: testMid, Action: rpc.GuardedAction_ACTION_COMMENT, Count: 1,
		})
		wantNoErr(t, fmt.Sprintf("上报第 %d 次", i+1), err)
		wantEQ(t, "上报", "deduplicated", out.Deduplicated, false)
	}
	st.redis.failWith("Mget", boom)

	reply, err := checkAction(t, st, commentReq("req-fallback"))
	wantNoErr(t, "兜底限流", err)
	wantEQ(t, "兜底", "decision", int32(reply.Decision), int32(rpc.Decision_DECISION_BLOCK))
	wantEQ(t, "兜底", "basis", reply.Basis, policy.BasisFallbackLocalLimit)
	wantEQ(t, "兜底", "degraded", reply.Degraded, true)
	wantEQ(t, "兜底", "evaluated", reply.Evaluated, false)
	wantEQ(t, "兜底", "action_code", reply.ActionCode, policy.ActionCodeUnavailable)
	wantEQ(t, "兜底", "score", reply.Score, int32(100))
	// 兜底分支在规则评估之前返回：skipped 为空由裁决语义决定（basis 已解释原因）。
	if len(reply.SkippedRuleIds) != 0 {
		t.Fatalf("兜底命中时不进入规则观测，skipped=%v", reply.SkippedRuleIds)
	}
	// 未超阈值的实例只做影子计数，不得参与常态裁决。
	st2 := newStore(t, withLocalFallback(100))
	rule2 := st2.seedRule(counterRule("r-fb2", model.MetricActionCount, 5, 60, model.DecisionBlock, 9))
	st2.redis.seedCounter(model.MetricActionCount, subjectOfMid(testMid), model.ActionComment, 60, 99)
	st2.redis.failWith("Mget", boom)
	reply2, err2 := checkAction(t, st2, commentReq("req-fallback-low"))
	wantNoErr(t, "兜底未触发", err2)
	wantEQ(t, "兜底未触发", "decision", int32(reply2.Decision), int32(rpc.Decision_DECISION_ALLOW))
	wantEQ(t, "兜底未触发", "basis", reply2.Basis, policy.BasisNoRule)
	wantEQ(t, "兜底未触发", "degraded", reply2.Degraded, true)
	// 兜底阈值没到，但不可观测的规则照样要如实上报。
	wantInt64s(t, "兜底未触发", "skipped", reply2.SkippedRuleIds, []int64{rule2})
	// 关闭兜底（默认 limit<=0）时，Redis 故障期间只剩 skipped 这一条线索。
	st3 := newStore(t)
	st3.seedRule(counterRule("r-off", model.MetricActionCount, 5, 60, model.DecisionBlock, 9))
	st3.redis.failWith("Mget", boom)
	reply3, err3 := checkAction(t, st3, commentReq("req-fallback-off"))
	wantNoErr(t, "兜底关闭", err3)
	wantEQ(t, "兜底关闭", "basis", reply3.Basis, policy.BasisNoRule)
	wantInt64s(t, "兜底关闭", "skipped", reply3.SkippedRuleIds, []int64{1})
}

func TestCheckActionMissingDeviceProfileIsSkipped(t *testing.T) {
	st := newStore(t)
	id := st.seedRule(model.RiskRule{Name: "r-noscore", ActionType: model.ActionComment,
		Metric: model.MetricDeviceRiskScore, Op: model.OpGTE, Threshold: 1, WindowSeconds: 60,
		Decision: model.DecisionBlock, Priority: 9, State: model.StateEnabled, Version: 1})

	reply, err := checkAction(t, st, commentReq("req-nodev"))
	wantNoErr(t, "画像缺失", err)
	wantOps(t, "调用序列", st.log.all(), []string{
		"redis.Get:rc:ck:req-nodev",
		"list.FindActive:3",
		"punish.ExpireStale:42",
		"punish.ListActive:42",
		"redis.Get:rc:rl:2",
		"rule.ListActive:2",
		"redis.Setex:rc:rl:2",
		"device.FindOne:" + devHash(),
		"log.Insert:req-nodev",
		"redis.Setex:rc:ck:req-nodev",
	})
	// 实测行为：profile==nil 与「读失败」走同一分支 → skipped。
	// 代码注释却写「设备从未出现过时按 0 分处理（确实是低风险）」，二者矛盾（README 缺口 #17）。
	wantInt64s(t, "画像缺失", "skipped_rule_ids", reply.SkippedRuleIds, []int64{id})
	wantEQ(t, "画像缺失", "basis", reply.Basis, policy.BasisNoRule)
	wantEQ(t, "画像缺失", "degraded", reply.Degraded, false)
}

func TestCheckActionProfileReadFailureIsSkippedOnce(t *testing.T) {
	boom := errors.New("device db down")
	st := newStore(t)
	a := st.seedRule(model.RiskRule{Name: "r-score", ActionType: model.ActionComment, Metric: model.MetricDeviceRiskScore,
		Op: model.OpGTE, Threshold: 1, WindowSeconds: 60, Decision: model.DecisionBlock, Priority: 9,
		State: model.StateEnabled, Version: 1})
	b := st.seedRule(model.RiskRule{Name: "r-midcount", ActionType: model.ActionComment, Metric: model.MetricDeviceMidCount,
		Op: model.OpGTE, Threshold: 1, WindowSeconds: 60, Decision: model.DecisionBlock, Priority: 8,
		State: model.StateEnabled, Version: 1})
	st.device.failWith("FindOne", boom)

	reply, err := checkAction(t, st, commentReq("req-devfail"))
	wantNoErr(t, "画像读失败", err)
	wantInt64s(t, "画像读失败", "skipped_rule_ids", reply.SkippedRuleIds, []int64{a, b})
	// 同一次裁决内只回源一次（profileLoaded 缓存住错误，不逐条规则重复打库）。
	if n := st.log.countPrefix("device.FindOne"); n != 1 {
		t.Fatalf("device.FindOne 次数 = %d, want 1", n)
	}
}

func TestCheckActionMissingSubjectSkipsThatDimension(t *testing.T) {
	st := newStore(t)
	r1 := st.seedRule(counterRule("r-ip", model.MetricIpActionCount, 5, 60, model.DecisionBlock, 9))
	r2 := st.seedRule(counterRule("r-dev", model.MetricDeviceActionCount, 5, 60, model.DecisionBlock, 8))
	r3 := st.seedRule(counterRule("r-mid", model.MetricActionCount, 5, 60, model.DecisionBlock, 7))
	st.redis.seedCounter(model.MetricActionCount, subjectOfMid(testMid), model.ActionComment, 60, 7)

	// 只报 mid：device/ip 主体缺失 → 不可观测，而不是 0 命中。
	reply, err := checkAction(t, st, &rpc.CheckActionReq{RequestId: "req-subject", Mid: testMid,
		Action: rpc.GuardedAction_ACTION_COMMENT})
	wantNoErr(t, "主体缺失", err)
	wantOps(t, "调用序列", st.log.all(), []string{
		"redis.Get:rc:ck:req-subject",
		"list.FindActive:1",
		"punish.ExpireStale:42",
		"punish.ListActive:42",
		"redis.Get:rc:rl:2",
		"rule.ListActive:2",
		"redis.Setex:rc:rl:2",
		"redis.Mget:7",
		"log.Insert:req-subject",
		"redis.Setex:rc:ck:req-subject",
	})
	wantInt64s(t, "主体缺失", "skipped", reply.SkippedRuleIds, []int64{r1, r2})
	wantInt64s(t, "主体缺失", "hit", reply.HitRuleIds, []int64{r3})
	wantEQ(t, "主体缺失", "decision", int32(reply.Decision), int32(rpc.Decision_DECISION_BLOCK))
}

// --- 隐私红线与守卫 ---

func TestCheckActionNeverLeaksRawPII(t *testing.T) {
	st := newStore(t)
	st.seedListEntry(model.RiskList{ListType: model.ListTypeBlack, TargetType: model.TargetTypeDevice,
		TargetValue: devHash(), Reason: "设备群控", Operator: 9, State: model.StateEnabled})
	st.seedRule(counterRule("r-leak", model.MetricDeviceActionCount, 5, 60, model.DecisionBlock, 9))

	reply, err := checkAction(t, st, commentReq("req-privacy"))
	wantNoErr(t, "CheckAction", err)

	// 1) 响应：原文与整段摘要都不该出现。
	assertNoRawPII(t, "响应", fmt.Sprintf("%v", reply))
	// 2) 审计列：只允许 64 位摘要。
	row, ok := st.checkLogRow("req-privacy")
	if !ok {
		t.Fatal("审计行缺失")
	}
	assertNoRawPII(t, "审计行", fmt.Sprintf("%+v", row))
	wantEQ(t, "审计行", "device_hash 是摘要", row.DeviceHash, devHash())
	// 3) 裁决回放缓存值。
	assertNoRawPII(t, "回放缓存", st.redis.kv["rc:ck:req-privacy"])
	// 4) 规则缓存值里只有规则定义，不含主体标识。
	assertNoRawPII(t, "规则缓存", st.redis.kv["rc:rl:2"])
	// 5) 名单命中说明只出现截断摘要（describeListEntry 的 TruncateHash(x,12)）。
	wantEQ(t, "黑名单", "basis", reply.Basis, policy.BasisBlacklist)
	if strings.Contains(fmt.Sprintf("%v", reply), devHash()) {
		t.Fatalf("完整设备摘要不得进响应: %v", reply)
	}
}

func TestCheckActionRejectsInvalidInputBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name  string
		in    *rpc.CheckActionReq
		want  error
		label string
	}{
		{"裸 IP", &rpc.CheckActionReq{RequestId: "r", Mid: testMid,
			Action: rpc.GuardedAction_ACTION_COMMENT, IpHash: rawIPv4}, model.ErrRawIPForbidden, "ip"},
		{"host:port", &rpc.CheckActionReq{RequestId: "r", Mid: testMid,
			Action: rpc.GuardedAction_ACTION_COMMENT, IpHash: rawIPv4 + ":8080"}, model.ErrRawIPForbidden, "ip"},
		{"非十六进制摘要", &rpc.CheckActionReq{RequestId: "r", Mid: testMid,
			Action: rpc.GuardedAction_ACTION_COMMENT, IpHash: "zzzzzzzzzzzzzzzz"}, model.ErrRawIPForbidden, "ip"},
		{"动作越界", &rpc.CheckActionReq{RequestId: "r", Mid: testMid,
			Action: rpc.GuardedAction(9)}, model.ErrInvalidTarget, "action"},
		{"未指定动作", &rpc.CheckActionReq{RequestId: "r", Mid: testMid,
			Action: rpc.GuardedAction_ACTION_UNSPECIFIED}, model.ErrInvalidTarget, "action"},
		{"空请求", nil, model.ErrInvalidTarget, "nil"},
	}
	for _, c := range cases {
		st := newStore(t)
		mark := st.log.snapshot()
		reply, err := checkAction(t, st, c.in)
		wantErrIs(t, c.name, err, c.want)
		if reply != nil {
			t.Errorf("%s：拒绝时不得返回裁决体", c.name)
		}
		wantNoCall(t, c.name+"（守卫前不得触依赖）", st, mark)
	}

	// request_context 只校验体积，本期不参与评估也不落库。
	st := newStore(t)
	mark := st.log.snapshot()
	big := map[string]string{}
	for i := 0; i <= maxRequestContextEntries; i++ {
		big[fmt.Sprintf("k%d", i)] = "v"
	}
	_, err := checkAction(t, st, &rpc.CheckActionReq{RequestId: "r", Mid: testMid,
		Action: rpc.GuardedAction_ACTION_COMMENT, RequestContext: big})
	wantErrIs(t, "上下文超条数", err, model.ErrInvalidTarget)
	wantNoCall(t, "上下文超条数", st, mark)
}

func TestCheckActionAuditWriteFailureDoesNotFlipDecision(t *testing.T) {
	boom := errors.New("audit write down")
	st := newStore(t)
	// 只让回放缓存写失败：审计行仍应落库，裁决也不得翻转。
	st.redis.failWith("Setex", boom)
	ruleID := st.seedRule(counterRule("r-audit", model.MetricActionCount, 5, 60, model.DecisionBlock, 9))
	st.redis.seedCounter(model.MetricActionCount, subjectOfMid(testMid), model.ActionComment, 60, 9)

	reply, err := checkAction(t, st, commentReq("req-audit"))
	wantNoErr(t, "缓存写失败必须被吞掉", err)
	wantEQ(t, "缓存写失败", "decision", int32(reply.Decision), int32(rpc.Decision_DECISION_BLOCK))
	wantEQ(t, "缓存写失败", "basis", reply.Basis, policy.BasisRules)
	wantOps(t, "调用序列", st.log.all(), []string{
		"redis.Get:rc:ck:req-audit",
		"list.FindActive:3",
		"punish.ExpireStale:42",
		"punish.ListActive:42",
		"redis.Get:rc:rl:2",
		"rule.ListActive:2",
		"redis.Setex:rc:rl:2", // 尝试写规则缓存：失败但确实发过命令
		"redis.Mget:7",
		"log.Insert:req-audit",
		"redis.Setex:rc:ck:req-audit",
	})
	// Setex 失败 ⇒ 规则缓存与回放缓存都没写成：kv 里只应有「尝试过」的 key 名，值为空。
	if v := st.redis.kv["rc:ck:req-audit"]; v != "" {
		t.Fatalf("Setex 失败却写入了值: %q", v)
	}
	first, ok := st.checkLogRow("req-audit")
	if !ok {
		t.Fatal("审计行必须落库（写的是 log.Insert，不是缓存）")
	}
	wantEQ(t, "审计行", "decision", first.Decision, model.DecisionBlock)

	// 缓存故障未恢复期间同 request_id 只能重算；恢复后再重算则演示 INSERT IGNORE 保留首条：
	// 规则已停用 → 本次裁决是 ALLOW/no_rule，但审计表里那条 BLOCK/rules 不能被改写。
	st.redis.clear("Setex")
	st.rule.rows[ruleID].State = model.StateDisabled
	second, err := checkAction(t, st, commentReq("req-audit"))
	wantNoErr(t, "重算", err)
	wantEQ(t, "重算", "decision", int32(second.Decision), int32(rpc.Decision_DECISION_ALLOW))
	wantEQ(t, "重算", "basis", second.Basis, policy.BasisNoRule)
	wantEQ(t, "重算", "log.Insert 次数", st.log.countPrefix("log.Insert"), 2)
	after, ok := st.checkLogRow("req-audit")
	if !ok {
		t.Fatal("审计行丢失")
	}
	wantEQ(t, "重算", "审计表保留首次 decision", after.Decision, model.DecisionBlock)
	wantEQ(t, "重算", "审计表保留首次 basis", after.Basis, policy.BasisRules)

	// 审计写失败（DB 故障的另一半）同样只影响完整性：裁决不变、且不再重复写。
	st3 := newStore(t)
	st3.checkLog.failWith("Insert", boom)
	st3.seedRule(counterRule("r-audit2", model.MetricActionCount, 5, 60, model.DecisionBlock, 9))
	st3.redis.seedCounter(model.MetricActionCount, subjectOfMid(testMid), model.ActionComment, 60, 9)
	reply3, err3 := checkAction(t, st3, commentReq("req-audit-fail"))
	wantNoErr(t, "审计写失败必须被吞掉", err3)
	wantEQ(t, "审计写失败", "decision", int32(reply3.Decision), int32(rpc.Decision_DECISION_BLOCK))
	wantOps(t, "审计写失败尾段", st3.log.opsFrom(8), []string{
		"log.Insert:req-audit-fail",
		"redis.Setex:rc:ck:req-audit-fail",
	})
	if _, ok := st3.checkLogRow("req-audit-fail"); ok {
		t.Fatal("注入的写失败不得被吞成「已落库」")
	}
}

// --- 小工具 ---

func assertNoRawPII(t *testing.T, label, blob string) {
	t.Helper()
	for _, bad := range []string{rawDeviceSN, strings.ToLower(rawDeviceSN), rawIPv4} {
		if strings.Contains(blob, bad) {
			t.Fatalf("%s 含敏感原文 %q：%s", label, bad, blob)
		}
	}
}

func indexOfOp(t *testing.T, ops []string, want string) int {
	t.Helper()
	for i, o := range ops {
		if o == want {
			return i
		}
	}
	t.Fatalf("调用序列缺少 %q：%v", want, ops)
	return -1
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
