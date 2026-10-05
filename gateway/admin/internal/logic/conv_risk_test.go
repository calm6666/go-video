package logic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	riskcontrolrpc "go-video/services/risk-control/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧口径：枚举是否被正确挡在入口、投影是否搬运了服务端字段、
// 分页归一是否与服务端同源。打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，
// 不建 gRPC 连接、不碰数据库。

type fakeRiskControl struct {
	riskcontrolrpc.RiskControlClient
	rules      *riskcontrolrpc.ListRulesReply
	entry      *riskcontrolrpc.UpsertListEntryReply
	err        error
	rulesReq   *riskcontrolrpc.ListRulesReq
	entryReq   *riskcontrolrpc.UpsertListEntryReq
	callCount  int
	entryCalls int
}

func (f *fakeRiskControl) ListRules(_ context.Context, in *riskcontrolrpc.ListRulesReq,
	_ ...grpc.CallOption) (*riskcontrolrpc.ListRulesReply, error) {
	f.rulesReq = in
	f.callCount++
	return f.rules, f.err
}

func (f *fakeRiskControl) UpsertListEntry(_ context.Context, in *riskcontrolrpc.UpsertListEntryReq,
	_ ...grpc.CallOption) (*riskcontrolrpc.UpsertListEntryReply, error) {
	f.entryReq = in
	f.entryCalls++
	return f.entry, f.err
}

func TestRiskGuardedAction(t *testing.T) {
	cases := []struct {
		name             string
		in               int32
		allowUnspecified bool
		wantErr          bool
	}{
		{"投稿", 1, false, false},
		{"直播开播", 7, false, false},
		{"写请求禁止 UNSPECIFIED", 0, false, true},
		{"过滤条件允许 UNSPECIFIED", 0, true, false},
		{"越界动作", 8, true, true},
		{"负数动作", -1, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := riskGuardedAction(tc.in, tc.allowUnspecified)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("riskGuardedAction(%d, %v) = %v, want error", tc.in, tc.allowUnspecified, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if int32(got) != tc.in {
				t.Fatalf("got %d, want %d", got, tc.in)
			}
		})
	}
}

func TestRiskMetricAndFilterVariant(t *testing.T) {
	if _, err := riskMetric(int32(riskcontrolrpc.Metric_METRIC_DEVICE_MID_COUNT)); err != nil {
		t.Fatalf("metric 5 should be valid: %v", err)
	}
	if _, err := riskMetric(0); err == nil {
		t.Fatal("riskMetric(0) must be rejected：UNSPECIFIED 不是可评估指标")
	}
	if _, err := riskMetric(6); err == nil {
		t.Fatal("riskMetric(6) must be rejected")
	}
	// 列表过滤条件走另一个口径：0 = 不过滤。
	got, err := riskMetricFilter(0)
	if err != nil || got != riskcontrolrpc.Metric_METRIC_UNSPECIFIED {
		t.Fatalf("riskMetricFilter(0) = %v, %v; want UNSPECIFIED, nil", got, err)
	}
	if _, err := riskMetricFilter(9); err == nil {
		t.Fatal("riskMetricFilter(9) must be rejected")
	}
}

func TestRiskCompareOp(t *testing.T) {
	if _, err := riskCompareOp(int32(riskcontrolrpc.CompareOp_OP_GTE)); err != nil {
		t.Fatalf("op 2 should be valid: %v", err)
	}
	for _, v := range []int32{0, 6, -1} {
		if _, err := riskCompareOp(v); err == nil {
			t.Fatalf("riskCompareOp(%d) must be rejected", v)
		}
	}
}

func TestRiskPunitiveDecision(t *testing.T) {
	for _, v := range []int32{2, 3, 4} {
		got, err := riskPunitiveDecision(v, "decision")
		if err != nil || int32(got) != v {
			t.Fatalf("riskPunitiveDecision(%d) = %v, %v", v, got, err)
		}
	}
	// ALLOW 是放行、UNSPECIFIED 是漏传，都不能当成处罚下发。
	for _, v := range []int32{0, 1, 5} {
		_, err := riskPunitiveDecision(v, "punish_decision")
		if err == nil {
			t.Fatalf("riskPunitiveDecision(%d) must be rejected", v)
		}
		if !strings.Contains(err.Error(), "punish_decision") {
			t.Fatalf("error should name the offending field, got %v", err)
		}
	}
}

func TestRiskListTypeTargetTypeAndState(t *testing.T) {
	if _, err := riskListType(int32(riskcontrolrpc.ListType_LIST_TYPE_WHITE), false); err != nil {
		t.Fatalf("list_type 2 should be valid: %v", err)
	}
	if _, err := riskListType(0, false); err == nil {
		t.Fatal("写请求必须显式给 list_type")
	}
	if _, err := riskListType(0, true); err != nil {
		t.Fatalf("过滤条件允许 list_type=0（不过滤）: %v", err)
	}
	if _, err := riskListType(3, true); err == nil {
		t.Fatal("list_type 3 must be rejected")
	}
	if _, err := riskTargetType(int32(riskcontrolrpc.TargetType_TARGET_TYPE_IP_HASH), false); err != nil {
		t.Fatalf("target_type 3 should be valid: %v", err)
	}
	if _, err := riskTargetType(0, false); err == nil {
		t.Fatal("写请求必须显式给 target_type")
	}
	if _, err := riskTargetType(4, true); err == nil {
		t.Fatal("target_type 4 must be rejected")
	}
	for _, v := range []int32{0, 1, 2, 3} {
		if _, err := riskPunishmentState(v); err != nil {
			t.Fatalf("state %d should be valid: %v", v, err)
		}
	}
	if _, err := riskPunishmentState(4); err == nil {
		t.Fatal("punishment state 4 must be rejected")
	}
	if err := riskStateFilter("state", -1); err != nil {
		t.Fatalf("state -1（不过滤）应通过: %v", err)
	}
	for _, v := range []int32{-2, 2} {
		if err := riskStateFilter("state", v); err == nil {
			t.Fatalf("riskStateFilter(%d) must be rejected", v)
		}
	}
}

func TestNormalizeRiskPage(t *testing.T) {
	cases := []struct {
		name   string
		pn, ps int32
		wantPn int32
		wantPs int32
	}{
		{"全零走默认", 0, 0, 1, 20},
		{"负值走默认", -3, -3, 1, 20},
		{"页大小截断到 50", 2, 500, 2, riskMaxPageSize},
		{"上限本身保留", 1, riskMaxPageSize, 1, riskMaxPageSize},
		{"区间内保留", 4, 30, 4, 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pn, ps := normalizeRiskPage(tc.pn, tc.ps)
			if pn != tc.wantPn || ps != tc.wantPs {
				t.Fatalf("normalizeRiskPage(%d,%d) = (%d,%d), want (%d,%d)", tc.pn, tc.ps, pn, ps, tc.wantPn, tc.wantPs)
			}
		})
	}
}

func TestRiskProjectionsNilInput(t *testing.T) {
	if got := riskRuleToAPI(nil); got != (types.RiskRuleItem{}) {
		t.Fatalf("riskRuleToAPI(nil) = %+v, want zero item", got)
	}
	if got := riskPunishmentToAPI(nil); got != (types.RiskPunishmentItem{}) {
		t.Fatalf("riskPunishmentToAPI(nil) = %+v, want zero item", got)
	}
	if got := riskPunishmentSnapshotToAPI(nil); got != (types.RiskPunishmentSnapshotItem{}) {
		t.Fatalf("riskPunishmentSnapshotToAPI(nil) = %+v, want zero item", got)
	}
	if got := riskDeviceProfileToAPI(nil); got.DeviceHash != "" || got.RiskScore != 0 || len(got.Labels) != 0 {
		t.Fatalf("riskDeviceProfileToAPI(nil) = %+v, want zero item", got)
	}
	if got := riskListEntryToAPI(nil); got != (types.RiskListEntryItem{}) {
		t.Fatalf("riskListEntryToAPI(nil) = %+v, want zero item", got)
	}
	// nil 列表必须投影成空切片：后台拿到的是 []，不是 null。
	if rules := riskRulesToAPI(nil); rules == nil || len(rules) != 0 {
		t.Fatalf("riskRulesToAPI(nil) = %+v", rules)
	}
	if hits := riskRuleHitsToAPI(nil); hits == nil || len(hits) != 0 {
		t.Fatalf("riskRuleHitsToAPI(nil) = %+v", hits)
	}
	if list := riskPunishmentsToAPI(nil); list == nil || len(list) != 0 {
		t.Fatalf("riskPunishmentsToAPI(nil) = %+v", list)
	}
	if list := riskListEntriesToAPI(nil); list == nil || len(list) != 0 {
		t.Fatalf("riskListEntriesToAPI(nil) = %+v", list)
	}
	if got := riskRulesToAPI([]*riskcontrolrpc.Rule{nil}); len(got) != 1 || got[0].RuleId != 0 {
		t.Fatalf("riskRulesToAPI with nil element = %+v（nil 元素应投影成零值而非 panic）", got)
	}
}

func TestRiskRuleAndPunishmentProjectionKeepsAuditFields(t *testing.T) {
	rule := riskRuleToAPI(&riskcontrolrpc.Rule{
		RuleId: 7, Name: "burst-comment", ActionType: riskcontrolrpc.GuardedAction_ACTION_COMMENT,
		Metric: riskcontrolrpc.Metric_METRIC_ACTION_COUNT, Op: riskcontrolrpc.CompareOp_OP_GTE,
		Threshold: 30, WindowSeconds: 60, Decision: riskcontrolrpc.Decision_DECISION_CHALLENGE,
		Priority: 5, State: 1, Version: 3, Operator: 99,
	})
	if rule.Version != 3 || rule.Operator != 99 {
		t.Fatalf("version/operator must survive projection（裁决要用版本解释）: %+v", rule)
	}
	if rule.ActionType != 2 || rule.Metric != 1 || rule.Op != 2 || rule.Decision != 2 {
		t.Fatalf("enum not projected as int: %+v", rule)
	}
	punishment := riskPunishmentToAPI(&riskcontrolrpc.Punishment{
		PunishmentId: 11, Mid: 22, Scope: riskcontrolrpc.GuardedAction_ACTION_SUBMIT_VIDEO,
		Decision: riskcontrolrpc.Decision_DECISION_BLOCK, Reason: "运营内部说明", ReasonCode: "risk.block",
		State: riskcontrolrpc.PunishmentState_PUNISHMENT_STATE_LIFTED, LiftOperator: 77, IdempotencyKey: "k-1",
	})
	if punishment.Reason != "运营内部说明" || punishment.LiftOperator != 77 || punishment.IdempotencyKey != "k-1" {
		t.Fatalf("处罚审计字段丢失：reason/lift_operator 只在 /admin 链路出现: %+v", punishment)
	}
	if punishment.State != 2 || punishment.Scope != 1 || punishment.Decision != 3 {
		t.Fatalf("enum not projected as int: %+v", punishment)
	}
}

func TestRiskDeviceProfileProjectionHasNoRawDeviceId(t *testing.T) {
	got := riskDeviceProfileToAPI(&riskcontrolrpc.DeviceProfile{
		DeviceHash: "abc123", Labels: []string{"emulator"}, RiskScore: 88, RelatedMidCount: 4,
	})
	if got.DeviceHash != "abc123" || got.RelatedMidCount != 4 {
		t.Fatalf("profile projection wrong: %+v", got)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "emulator" {
		t.Fatalf("labels not carried: %+v", got.Labels)
	}
}

func TestRiskInt64IdsCopiesAndNeverReturnsNil(t *testing.T) {
	if got := riskInt64Ids(nil); got == nil {
		t.Fatal("riskInt64Ids(nil) = nil, want empty slice")
	}
	src := []int64{1, 2, 3}
	got := riskInt64Ids(src)
	src[0] = 99
	if got[0] != 1 {
		t.Fatalf("riskInt64Ids must copy, got %v", got)
	}
}

func TestListRiskRulesLogicValidation(t *testing.T) {
	cases := []struct {
		name string
		req  *types.ParamListRiskRules
	}{
		{"缺 operator_id", &types.ParamListRiskRules{}},
		{"未知 action_type", &types.ParamListRiskRules{OperatorId: 9, ActionType: 8}},
		{"未知 metric", &types.ParamListRiskRules{OperatorId: 9, Metric: 9}},
		{"非法 state", &types.ParamListRiskRules{OperatorId: 9, State: 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeRiskControl{rules: &riskcontrolrpc.ListRulesReply{}}
			l := NewListRiskRulesLogic(context.Background(), &svc.ServiceContext{RiskControl: fake})
			if _, err := l.ListRiskRules(tc.req); err == nil {
				t.Fatal("want error")
			}
			if fake.callCount != 0 {
				t.Fatalf("校验失败时不得调用下游，实际调用 %d 次", fake.callCount)
			}
		})
	}
}

func TestListRiskRulesLogicWithoutClientConfigured(t *testing.T) {
	l := NewListRiskRulesLogic(context.Background(), &svc.ServiceContext{})
	if _, err := l.ListRiskRules(&types.ParamListRiskRules{OperatorId: 9}); err == nil {
		t.Fatal("未配置 risk-control 时应返回错误")
	}
}

func TestListRiskRulesLogicForwardsFiltersAndPaging(t *testing.T) {
	fake := &fakeRiskControl{rules: &riskcontrolrpc.ListRulesReply{
		Rules: []*riskcontrolrpc.Rule{{RuleId: 7, Version: 3}}, Total: 1, Pn: 2, Ps: 50,
	}}
	l := NewListRiskRulesLogic(context.Background(), &svc.ServiceContext{RiskControl: fake})
	resp, err := l.ListRiskRules(&types.ParamListRiskRules{
		ActionType: 0, Metric: 0, State: -1, Pn: 2, Ps: 500, OperatorId: 9,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.rulesReq.GetActionType() != riskcontrolrpc.GuardedAction_ACTION_UNSPECIFIED ||
		fake.rulesReq.GetMetric() != riskcontrolrpc.Metric_METRIC_UNSPECIFIED ||
		fake.rulesReq.GetState() != -1 {
		t.Fatalf("过滤条件应原样表达「不过滤」: %+v", fake.rulesReq)
	}
	if fake.rulesReq.GetPs() != riskMaxPageSize {
		t.Fatalf("ps = %d, want %d（与服务端上限一致，不得放大）", fake.rulesReq.GetPs(), riskMaxPageSize)
	}
	if resp.Code != 0 || resp.Message != "ok" {
		t.Fatalf("envelope = code=%d message=%s", resp.Code, resp.Message)
	}
	if len(resp.Data.Rules) != 1 || resp.Data.Rules[0].Version != 3 || resp.Data.Total != 1 {
		t.Fatalf("data = %+v", resp.Data)
	}
}

func TestListRiskRulesLogicPropagatesDownstreamError(t *testing.T) {
	sentinel := errors.New("risk-control: unavailable")
	fake := &fakeRiskControl{err: sentinel}
	l := NewListRiskRulesLogic(context.Background(), &svc.ServiceContext{RiskControl: fake})
	if _, err := l.ListRiskRules(&types.ParamListRiskRules{OperatorId: 9}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want downstream error propagated unchanged", err)
	}
}

func TestUpsertRiskListEntryLogicValidation(t *testing.T) {
	// 注意：这里不再包含「缺 operator_id」。风控的 operator 属于后台账号 ID 空间，
	// 现在以登录会话为准（见 TestUpsertRiskListEntryLogicSessionOwnsOperator），
	// 客户端不声明是合法输入，不再是入口拒绝理由。
	base := func() *types.ParamUpsertRiskListEntry {
		return &types.ParamUpsertRiskListEntry{
			ListType: int32(riskcontrolrpc.ListType_LIST_TYPE_BLACK), TargetType: int32(riskcontrolrpc.TargetType_TARGET_TYPE_MID),
			TargetValue: "12345", Reason: "批量灌水", OperatorId: 9, State: 1, IdempotencyKey: "batch-1",
		}
	}
	mutate := map[string]func(r *types.ParamUpsertRiskListEntry){
		"缺 idempotency_key": func(r *types.ParamUpsertRiskListEntry) { r.IdempotencyKey = "   " },
		"缺 target_value":    func(r *types.ParamUpsertRiskListEntry) { r.TargetValue = "" },
		"未显式给 list_type":    func(r *types.ParamUpsertRiskListEntry) { r.ListType = 0 },
		"未显式给 target_type":  func(r *types.ParamUpsertRiskListEntry) { r.TargetType = 0 },
		"未知 list_type":      func(r *types.ParamUpsertRiskListEntry) { r.ListType = 3 },
		"未知 target_type":    func(r *types.ParamUpsertRiskListEntry) { r.TargetType = 4 },
		"非法 state":          func(r *types.ParamUpsertRiskListEntry) { r.State = 2 },
		"负 duration":        func(r *types.ParamUpsertRiskListEntry) { r.DurationSeconds = -1 },
	}
	for name, fn := range mutate {
		t.Run(name, func(t *testing.T) {
			req := base()
			fn(req)
			fake := &fakeRiskControl{entry: &riskcontrolrpc.UpsertListEntryReply{}}
			l := NewUpsertRiskListEntryLogic(withAdminSession(gateAdminID), &svc.ServiceContext{RiskControl: fake})
			if _, err := l.UpsertRiskListEntry(req); err == nil {
				t.Fatal("want error")
			}
			if fake.entryCalls != 0 {
				t.Fatalf("校验失败时不得写库，实际调用 %d 次", fake.entryCalls)
			}
		})
	}
}

func TestUpsertRiskListEntryLogicWritesOperatorAndState(t *testing.T) {
	fake := &fakeRiskControl{entry: &riskcontrolrpc.UpsertListEntryReply{
		Entry: &riskcontrolrpc.ListEntry{Id: 5, ListType: riskcontrolrpc.ListType_LIST_TYPE_BLACK,
			TargetType: riskcontrolrpc.TargetType_TARGET_TYPE_IP_HASH, TargetValue: "deadbeef", Operator: gateAdminID, State: 0},
		Created: true,
	}}
	l := NewUpsertRiskListEntryLogic(withAdminSession(gateAdminID), &svc.ServiceContext{RiskControl: fake})
	resp, err := l.UpsertRiskListEntry(&types.ParamUpsertRiskListEntry{
		ListType: 1, TargetType: 3, TargetValue: "deadbeef", OperatorId: 9, State: 0, IdempotencyKey: "batch-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.entryReq.GetOperator() != gateAdminID {
		t.Fatalf("operator = %d, want %d（审计主体以会话为准，客户端声明的 9 不得落到风控）",
			fake.entryReq.GetOperator(), gateAdminID)
	}
	if fake.entryReq.GetState() != 0 {
		t.Fatalf("state = %d, want 0（停用是合法值，不能被默认值兜底改写）", fake.entryReq.GetState())
	}
	if fake.entryReq.GetDurationSeconds() != 0 {
		t.Fatalf("duration_seconds = %d, want 0（0 = 永久）", fake.entryReq.GetDurationSeconds())
	}
	if fake.entryCalls != 1 {
		t.Fatalf("entryCalls = %d, want 1", fake.entryCalls)
	}
	if !resp.Data.Created || resp.Data.Entry.Id != 5 || resp.Data.Entry.State != 0 {
		t.Fatalf("data = %+v", resp.Data)
	}
}

// 风控写入口的审计主体只能来自登录会话：没有会话时即使入参齐全也必须拒绝，并且一次下游调用都不能发出。
func TestUpsertRiskListEntryLogicSessionOwnsOperator(t *testing.T) {
	req := func() *types.ParamUpsertRiskListEntry {
		return &types.ParamUpsertRiskListEntry{
			ListType: int32(riskcontrolrpc.ListType_LIST_TYPE_BLACK), TargetType: int32(riskcontrolrpc.TargetType_TARGET_TYPE_MID),
			TargetValue: "12345", Reason: "批量灌水", State: 1, IdempotencyKey: "batch-1",
		}
	}

	noSession := &fakeRiskControl{entry: &riskcontrolrpc.UpsertListEntryReply{}}
	// 客户端替自己填一个 operator_id，也不能代替会话。
	r := req()
	r.OperatorId = 9
	if _, err := NewUpsertRiskListEntryLogic(context.Background(),
		&svc.ServiceContext{RiskControl: noSession}).UpsertRiskListEntry(r); err == nil ||
		!strings.Contains(err.Error(), "admin session required") {
		t.Fatalf("无会话时 err = %v，want 拒绝", err)
	}
	if noSession.entryCalls != 0 {
		t.Fatalf("无会话被拒后仍调用下游 %d 次", noSession.entryCalls)
	}

	// 会话存在、客户端不声明：由会话补齐主体后放行。
	filled := &fakeRiskControl{entry: &riskcontrolrpc.UpsertListEntryReply{}}
	if _, err := NewUpsertRiskListEntryLogic(withAdminSession(gateAdminID),
		&svc.ServiceContext{RiskControl: filled}).UpsertRiskListEntry(req()); err != nil {
		t.Fatalf("客户端未声明 operator_id 时应由会话补齐, got %v", err)
	}
	if filled.entryReq.GetOperator() != gateAdminID {
		t.Fatalf("operator = %d, want %d", filled.entryReq.GetOperator(), gateAdminID)
	}
	if filled.entryCalls != 1 {
		t.Fatalf("entryCalls = %d, want 1", filled.entryCalls)
	}
}
