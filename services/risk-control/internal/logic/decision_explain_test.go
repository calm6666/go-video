package logic

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"go-video/services/risk-control/internal/policy"
	"go-video/services/risk-control/model"
	"go-video/services/risk-control/rpc"
)

// testNow 是固定裁决时间戳，避免断言依赖系统时钟。
const testNow = int64(1_700_000_000)

// 本文件覆盖「入参规范化 + 决策解释出口」这两段纯逻辑：
// 明文 PII 必须在进入 model/policy 之前被挡下，命中顺序必须原样出现在响应里。

func TestEnumNumberingStaysInSyncWithProto(t *testing.T) {
	// model 常量与 rpc 枚举数值必须一致（model/types.go 注明「禁止漂移」）。
	pairs := []struct {
		name  string
		mdl   int32
		proto int32
	}{
		{"action_submit_video", model.ActionSubmitVideo, int32(rpc.GuardedAction_ACTION_SUBMIT_VIDEO)},
		{"action_comment", model.ActionComment, int32(rpc.GuardedAction_ACTION_COMMENT)},
		{"action_danmaku", model.ActionDanmaku, int32(rpc.GuardedAction_ACTION_DANMAKU)},
		{"action_follow", model.ActionFollow, int32(rpc.GuardedAction_ACTION_FOLLOW)},
		{"action_login", model.ActionLogin, int32(rpc.GuardedAction_ACTION_LOGIN)},
		{"action_rename", model.ActionRename, int32(rpc.GuardedAction_ACTION_RENAME)},
		{"action_live_start", model.ActionLiveStart, int32(rpc.GuardedAction_ACTION_LIVE_START)},
		{"decision_allow", model.DecisionAllow, int32(rpc.Decision_DECISION_ALLOW)},
		{"decision_challenge", model.DecisionChallenge, int32(rpc.Decision_DECISION_CHALLENGE)},
		{"decision_block", model.DecisionBlock, int32(rpc.Decision_DECISION_BLOCK)},
		{"decision_review", model.DecisionReview, int32(rpc.Decision_DECISION_REVIEW)},
		{"op_gt", model.OpGT, int32(rpc.CompareOp_OP_GT)},
		{"op_eq", model.OpEQ, int32(rpc.CompareOp_OP_EQ)},
		{"list_black", model.ListTypeBlack, int32(rpc.ListType_LIST_TYPE_BLACK)},
		{"list_white", model.ListTypeWhite, int32(rpc.ListType_LIST_TYPE_WHITE)},
		{"target_mid", model.TargetTypeMid, int32(rpc.TargetType_TARGET_TYPE_MID)},
		{"target_device", model.TargetTypeDevice, int32(rpc.TargetType_TARGET_TYPE_DEVICE)},
		{"target_ip_hash", model.TargetTypeIpHash, int32(rpc.TargetType_TARGET_TYPE_IP_HASH)},
		{"punishment_active", model.PunishmentStateActive, int32(rpc.PunishmentState_PUNISHMENT_STATE_ACTIVE)},
		{"punishment_lifted", model.PunishmentStateLifted, int32(rpc.PunishmentState_PUNISHMENT_STATE_LIFTED)},
		{"punishment_expired", model.PunishmentStateExpired, int32(rpc.PunishmentState_PUNISHMENT_STATE_EXPIRED)},
	}
	for _, p := range pairs {
		if p.mdl != p.proto {
			t.Fatalf("%s: model=%d 与 proto=%d 不一致", p.name, p.mdl, p.proto)
		}
	}
}

func TestMetricRoundTripCoversSupportedMetrics(t *testing.T) {
	for name := range model.SupportedMetrics() {
		proto := metricToProto(name)
		if proto == rpc.Metric_METRIC_UNSPECIFIED {
			t.Fatalf("指标 %q 未在 proto 中登记，运营后台将无法展示", name)
		}
		if back := metricFromProto(proto); back != name {
			t.Fatalf("指标 %q 往返后变成 %q", name, back)
		}
	}
	if metricToProto("not_a_metric") != rpc.Metric_METRIC_UNSPECIFIED {
		t.Fatal("未知指标必须映射为 UNSPECIFIED")
	}
	if metricFromProto(rpc.Metric_METRIC_UNSPECIFIED) != "" {
		t.Fatal("UNSPECIFIED 指标应返回空串，由 Validate 拒绝写入规则")
	}
}

func TestCheckActionInputSanitizesPII(t *testing.T) {
	in := &rpc.CheckActionReq{
		RequestId:  "  req-1  ",
		Mid:        42,
		Action:     rpc.GuardedAction_ACTION_LOGIN,
		DeviceId:   " Device-ABC ",
		IpHash:     strings.Repeat("AB", 32),
		Platform:   " android\r\n",
		AppVersion: strings.Repeat("9", 50),
	}
	got, err := checkActionInput(in)
	if err != nil {
		t.Fatalf("合法入参被拒: %v", err)
	}
	if got.RequestID != "req-1" {
		t.Fatalf("request_id 未去空白: %q", got.RequestID)
	}
	if got.DeviceHash != model.DeviceHash("device-abc") || len(got.DeviceHash) != 64 {
		t.Fatalf("device_id 必须先哈希再进引擎，实际 %q", got.DeviceHash)
	}
	if got.IPHash != strings.Repeat("ab", 32) {
		t.Fatalf("ip_hash 应小写归一化，实际 %q", got.IPHash)
	}
	if got.Platform != "android" {
		t.Fatalf("platform 应去掉控制符: %q", got.Platform)
	}
	if len(got.AppVersion) != 32 {
		t.Fatalf("app_version 应裁剪到 32 字节，实际 %d", len(got.AppVersion))
	}
	if got.Now != 0 {
		t.Fatal("Now 由引擎注入，logic 不得伪造时间")
	}

	// 裸 IP：直接拒绝，不在服务端代做哈希（明文 IP 已经进了调用方链路）。
	for _, bad := range []string{"1.2.3.4", "2001:db8::1", "1.2.3.4:5555"} {
		if _, err := checkActionInput(&rpc.CheckActionReq{RequestId: "r", Action: rpc.GuardedAction_ACTION_COMMENT, IpHash: bad}); !errors.Is(err, model.ErrRawIPForbidden) {
			t.Fatalf("ip_hash=%q 应被拒绝，实际 err=%v", bad, err)
		}
	}
	// 动作越界（含 0）视为非法入参，而不是「对所有动作生效」。
	for _, a := range []rpc.GuardedAction{rpc.GuardedAction_ACTION_UNSPECIFIED, rpc.GuardedAction(8)} {
		if _, err := checkActionInput(&rpc.CheckActionReq{RequestId: "r", Action: a}); !errors.Is(err, model.ErrInvalidTarget) {
			t.Fatalf("action=%d 应被拒绝，实际 err=%v", a, err)
		}
	}
	if _, err := checkActionInput(nil); !errors.Is(err, model.ErrInvalidTarget) {
		t.Fatalf("nil 请求应返回 ErrInvalidTarget，实际 %v", err)
	}
	// 未提供设备与 IP 是合法的，只是相应维度不可观测。
	mini, err := checkActionInput(&rpc.CheckActionReq{RequestId: "r", Action: rpc.GuardedAction_ACTION_COMMENT})
	if err != nil || mini.DeviceHash != "" || mini.IPHash != "" {
		t.Fatalf("缺省受控标识应允许通过，实际 %+v err=%v", mini, err)
	}
}

func TestCheckRequestContextSizeLimits(t *testing.T) {
	if err := checkRequestContext(nil); err != nil {
		t.Fatalf("空上下文应通过: %v", err)
	}
	entries := make(map[string]string, maxRequestContextEntries+1)
	for i := 0; i <= maxRequestContextEntries; i++ {
		entries["k"+strconv.Itoa(i)] = "v"
	}
	if err := checkRequestContext(entries); !errors.Is(err, model.ErrInvalidTarget) {
		t.Fatalf("超出条数上限应拒绝（而不是静默截断），实际 %v", err)
	}
	if err := checkRequestContext(map[string]string{"k": strings.Repeat("v", maxRequestContextValLen+1)}); err == nil {
		t.Fatal("超长 value 应拒绝")
	}
	if err := checkRequestContext(map[string]string{strings.Repeat("k", maxRequestContextKeyLen+1): "v"}); err == nil {
		t.Fatal("超长 key 应拒绝")
	}
	if err := checkRequestContext(map[string]string{"k": strings.Repeat("v", maxRequestContextValLen)}); err != nil {
		t.Fatalf("边界内应通过: %v", err)
	}
}

func TestDeviceHashOfPrefersRawDeviceID(t *testing.T) {
	if got := deviceHashOf("DEV-1", strings.Repeat("a", 64)); got != model.DeviceHash("dev-1") {
		t.Fatal("device_id 优先：调用方给的 hash 不可信")
	}
	if got := deviceHashOf("", strings.Repeat("A", 64)); got != strings.Repeat("a", 64) {
		t.Fatalf("device_id 为空时才接受已受控 hash，实际 %q", got)
	}
	if got := deviceHashOf("", "1.2.3.4"); got != "" {
		t.Fatalf("裸 IP 形态的 device_hash 应被丢弃，实际 %q", got)
	}
	if got := deviceHashOf("", ""); got != "" {
		t.Fatalf("两者皆空应返回空串（不可观测但不报错），实际 %q", got)
	}
}

// CheckAction 的决策解释：hit_rule_ids 与 rule_hits 必须同序，
// 且顺序就是引擎的 (priority DESC, rule_id ASC)，网关与客户端不再重排。
func TestDecisionToReplyKeepsExplanationOrder(t *testing.T) {
	res := &policy.Result{
		RequestID:   "req-9",
		Decision:    model.DecisionBlock,
		Score:       100,
		HitRuleIDs:  []int64{4, 22, 11},
		HitVersions: []int32{7, 2, 1},
		Hits: []policy.Hit{
			{RuleID: 4, Version: 7, Priority: 9, Decision: model.DecisionBlock, Metric: model.MetricActionCount, Op: model.OpGTE},
			{RuleID: 22, Version: 2, Priority: 5, Decision: model.DecisionChallenge, Metric: model.MetricIpActionCount, Op: model.OpGT},
			{RuleID: 11, Version: 1, Priority: 1, Decision: model.DecisionReview, Metric: model.MetricDeviceRiskScore, Op: model.OpGTE},
		},
		SkippedRuleIDs:      []int64{88},
		Basis:               policy.BasisRules,
		ActionCode:          policy.ActionCodeBlocked,
		ChallengeTTLSeconds: 300,
		Evaluated:           true,
		Degraded:            true,
	}
	reply := decisionToReply(res, testNow)
	if reply.RequestId != "req-9" || reply.Decision != rpc.Decision_DECISION_BLOCK || reply.Score != 100 {
		t.Fatalf("响应主体映射错误: %+v", reply)
	}
	if len(reply.HitRuleIds) != 3 || reply.HitRuleIds[0] != 4 || reply.HitRuleIds[2] != 11 {
		t.Fatalf("HitRuleIds 顺序被改动: %v", reply.HitRuleIds)
	}
	if len(reply.RuleHits) != 3 {
		t.Fatalf("RuleHits=%d, want 3", len(reply.RuleHits))
	}
	for i, h := range reply.RuleHits {
		if h.RuleId != res.Hits[i].RuleID || h.Version != res.HitVersions[i] || h.Priority != res.Hits[i].Priority {
			t.Fatalf("rule_hits[%d] 与 hit_rule_ids 不同序: %+v", i, h)
		}
	}
	if reply.RuleHits[0].Metric != rpc.Metric_METRIC_ACTION_COUNT || reply.RuleHits[2].Metric != rpc.Metric_METRIC_DEVICE_RISK_SCORE {
		t.Fatalf("指标枚举映射错误: %+v", reply.RuleHits)
	}
	if len(reply.SkippedRuleIds) != 1 || reply.SkippedRuleIds[0] != 88 {
		t.Fatalf("跳过的规则必须回传，否则运营无法发现配错的指标: %v", reply.SkippedRuleIds)
	}
	if reply.Basis != policy.BasisRules || !reply.Degraded || !reply.Evaluated {
		t.Fatalf("basis/降级标记丢失: %+v", reply)
	}
	if reply.Punishment != nil {
		t.Fatal("无处罚时不得返回处罚快照")
	}

	// 处罚快照：客户端展示剩余时长依赖 remaining_seconds，永久处罚必须显式标记。
	withPunish := &policy.Result{
		RequestID: "req-10", Decision: model.DecisionChallenge, Basis: policy.BasisPunishment,
		Punishment: &policy.PunishmentView{ID: 3, Scope: model.ActionComment, Decision: model.DecisionChallenge, StartAt: testNow - 10, EndAt: testNow + 60, ReasonCode: "risk.punish"},
	}
	snap := decisionToReply(withPunish, testNow).Punishment
	if snap.PunishmentId != 3 || snap.Permanent || snap.RemainingSeconds != 60 || snap.EndAt != testNow+60 {
		t.Fatalf("处罚快照错误: %+v", snap)
	}
	withPunish.Punishment.EndAt = 0
	if snap = decisionToReply(withPunish, testNow).Punishment; !snap.Permanent || snap.RemainingSeconds != 0 {
		t.Fatalf("永久处罚快照错误: %+v", snap)
	}

	if empty := decisionToReply(nil, testNow); empty.Decision != rpc.Decision_DECISION_UNSPECIFIED {
		t.Fatal("nil 裁决应返回 UNSPECIFIED，而不是冒充 ALLOW")
	}
}

func TestSanitizeShortString(t *testing.T) {
	if got := sanitizeShortString("  a\tb\nc  ", 10); got != "abc" {
		t.Fatalf("控制符未去除: %q", got)
	}
	if got := sanitizeShortString(strings.Repeat("x", 40), 32); len(got) != 32 {
		t.Fatalf("未裁剪长度: %d", len(got))
	}
	if got := sanitizeShortString("abc", 0); got != "abc" {
		t.Fatalf("maxLen<=0 应只去空白: %q", got)
	}
}
