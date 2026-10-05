package policy

import (
	"context"
	"errors"
	"testing"

	"go-video/services/risk-control/model"
)

// fakeStore 是 policy.Store 的内存实现：单测在此注入「DB 故障 / Redis 故障 / 缓存命中」，
// 不需要任何真实中间件（AGENTS.md §9：不得为了让测试通过而放宽校验）。
type fakeStore struct {
	cached      *Result
	cachedHit   bool
	facts       *Facts
	loadErr     error
	storeErr    error
	loadCalls   int
	storeCalls  int
	lookupCalls int
	lastInput   Input
	storedRes   *Result
}

func (f *fakeStore) LookupCached(_ context.Context, requestID string) (*Result, bool) {
	f.lookupCalls++
	if !f.cachedHit || f.cached == nil {
		return nil, false
	}
	out := *f.cached
	out.RequestID = requestID
	return &out, true
}

func (f *fakeStore) LoadFacts(_ context.Context, in Input) (*Facts, error) {
	f.loadCalls++
	f.lastInput = in
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	if f.facts == nil {
		return &Facts{CounterAvailable: true}, nil
	}
	return f.facts, nil
}

func (f *fakeStore) StoreResult(_ context.Context, in Input, res *Result) error {
	f.storeCalls++
	f.lastInput = in
	f.storedRes = res
	return f.storeErr
}

var errDBDown = errors.New("dial tcp 127.0.0.1:3306: connect: connection refused")

// TestDecideDegradePerActionRisk 覆盖 DB 故障时的 ALLOW-on-error / BLOCK-on-error 分界。
func TestDecideDegradePerActionRisk(t *testing.T) {
	cases := []struct {
		name       string
		action     int32
		cfg        Config
		wantDecide int32
		wantCode   string
	}{
		{"submit_video_block_on_error", model.ActionSubmitVideo, Config{}, model.DecisionBlock, ActionCodeUnavailable},
		{"login_block_on_error", model.ActionLogin, Config{}, model.DecisionBlock, ActionCodeUnavailable},
		{"rename_block_on_error", model.ActionRename, Config{}, model.DecisionBlock, ActionCodeUnavailable},
		{"live_start_block_on_error", model.ActionLiveStart, Config{}, model.DecisionBlock, ActionCodeUnavailable},
		{"comment_allow_on_error", model.ActionComment, Config{}, model.DecisionAllow, ActionCodeNone},
		{"danmaku_allow_on_error", model.ActionDanmaku, Config{}, model.DecisionAllow, ActionCodeNone},
		{"follow_allow_on_error", model.ActionFollow, Config{}, model.DecisionAllow, ActionCodeNone},
		// OnDbFailureDefault=block 时 svc 会把 DefaultDecision 配成 BLOCK：全量保守拒绝。
		{"conservative_mode_blocks_low_risk", model.ActionComment,
			Config{Degrade: DegradePolicy{DefaultDecision: model.DecisionBlock}}, model.DecisionBlock, ActionCodeUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := &fakeStore{loadErr: errDBDown}
			cfg := c.cfg
			if len(cfg.Degrade.HighRiskActions) == 0 {
				cfg.Degrade.HighRiskActions = append([]int32{}, model.ActionSubmitVideo, model.ActionLogin, model.ActionRename, model.ActionLiveStart)
			}
			res, err := NewEngine(src, cfg).Decide(context.Background(), Input{RequestID: "req-x", Mid: 7, Action: c.action, Now: testNow})
			if err != nil {
				t.Fatalf("依赖故障必须走降级而不是返回错误: %v", err)
			}
			if res.Decision != c.wantDecide || !res.Degraded || res.Basis != BasisFallbackDB {
				t.Fatalf("decision=%d degraded=%v basis=%q, want %d/true/%q", res.Decision, res.Degraded, res.Basis, c.wantDecide, BasisFallbackDB)
			}
			if res.ActionCode != c.wantCode {
				t.Fatalf("ActionCode=%q, want %q", res.ActionCode, c.wantCode)
			}
			if res.Score != decisionScore(c.wantDecide) {
				t.Fatalf("降级分数应为裁决基线分，实际 %d", res.Score)
			}
			if res.RequestID != "req-x" {
				t.Fatalf("降级裁决仍要回带 request_id，实际 %q", res.RequestID)
			}
			if src.storeCalls != 1 || src.storedRes == nil {
				t.Fatalf("降级裁决仍要交给 StoreResult（repository 侧按 basis 跳过 DB 日志、只写回放缓存），实际 storeCalls=%d", src.storeCalls)
			}
			if src.storedRes.Basis != BasisFallbackDB {
				t.Fatalf("StoreResult 收到的 basis=%q, want %q", src.storedRes.Basis, BasisFallbackDB)
			}
		})
	}
}

func TestDecideChallengeDegradeKeepsTTL(t *testing.T) {
	src := &fakeStore{loadErr: errDBDown}
	cfg := Config{
		ChallengeTTLSeconds: 120,
		Degrade: DegradePolicy{
			HighRiskActions: []int32{model.ActionComment},
			// 允许运营把某动作的降级策略配成 CHALLENGE（例如只要求人机校验）。
			HighRiskDecision: model.DecisionChallenge,
			DefaultDecision:  model.DecisionAllow,
		},
	}
	res, err := NewEngine(src, cfg).Decide(context.Background(), Input{RequestID: "r", Mid: 1, Action: model.ActionComment, Now: testNow})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != model.DecisionChallenge || res.ChallengeTTLSeconds != 120 || res.ActionCode != ActionCodeChallenge {
		t.Fatalf("CHALLENGE 降级路径不完整: %+v", res)
	}
}

func TestDecideReviewDegradeCode(t *testing.T) {
	src := &fakeStore{loadErr: errDBDown}
	cfg := Config{Degrade: DegradePolicy{HighRiskActions: []int32{model.ActionComment}, HighRiskDecision: model.DecisionReview}}
	res, err := NewEngine(src, cfg).Decide(context.Background(), Input{RequestID: "r", Mid: 1, Action: model.ActionComment, Now: testNow})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != model.DecisionReview || res.ActionCode != ActionCodeReview {
		t.Fatalf("REVIEW 降级路径不完整: %+v", res)
	}
}

func TestDecideReplaysCachedDecision(t *testing.T) {
	src := &fakeStore{
		cachedHit: true,
		cached:    &Result{Decision: model.DecisionBlock, Basis: BasisBlacklist, Score: 100},
		loadErr:   errDBDown,
	}
	res, err := NewEngine(src, DefaultConfig()).Decide(context.Background(), Input{RequestID: "same-request", Mid: 1, Action: model.ActionComment, Now: testNow})
	if err != nil {
		t.Fatal(err)
	}
	if res.Basis != BasisBlacklist || res.Decision != model.DecisionBlock {
		t.Fatalf("应回放缓存裁决，实际 %+v", res)
	}
	if src.loadCalls != 0 || src.storeCalls != 0 {
		t.Fatalf("幂等回放不得重复装载事实或写日志，load=%d store=%d", src.loadCalls, src.storeCalls)
	}
	if res.RequestID != "same-request" {
		t.Fatalf("回放要按本次 request_id 回包，实际 %q", res.RequestID)
	}
}

func TestDecideGeneratesRequestIDAndDefaultsNow(t *testing.T) {
	src := &fakeStore{facts: &Facts{CounterAvailable: true}}
	res, err := NewEngine(src, DefaultConfig()).Decide(context.Background(), Input{Mid: 1, Action: model.ActionComment})
	if err != nil {
		t.Fatal(err)
	}
	if res.RequestID == "" {
		t.Fatal("缺省 request_id 时必须生成，否则幂等回放与审计日志无从关联")
	}
	if src.lastInput.Now <= 0 {
		t.Fatal("缺省 Now 时必须填充当前时间，时间边界判断依赖它")
	}
}

func TestDecideRejectsInvalidAction(t *testing.T) {
	for _, action := range []int32{model.ActionAll, 8, -1} {
		src := &fakeStore{}
		if _, err := NewEngine(src, DefaultConfig()).Decide(context.Background(), Input{RequestID: "r", Action: action, Now: testNow}); !errors.Is(err, model.ErrInvalidTarget) {
			t.Fatalf("动作 %d 应被拒绝，实际 err=%v", action, err)
		}
		if src.loadCalls != 0 {
			t.Fatal("非法入参不得触达数据层")
		}
	}
}

// 审计写入失败不能翻转已产出的裁决：客户端必须拿到结论，日志完整性是次要问题。
func TestDecideStoreFailureDoesNotFlipDecision(t *testing.T) {
	src := &fakeStore{
		storeErr: errors.New("insert risk_check_log failed"),
		facts: &Facts{
			Observations:     []Observation{countObservation(3, 60, 99, model.OpGTE, 10, model.DecisionBlock, 1)},
			CounterAvailable: true,
		},
	}
	res, err := NewEngine(src, DefaultConfig()).Decide(context.Background(), Input{RequestID: "r", Mid: 1, Action: model.ActionComment, Now: testNow})
	if err != nil {
		t.Fatalf("StoreResult 失败不应外泄为错误: %v", err)
	}
	if res.Decision != model.DecisionBlock || res.Basis != BasisRules {
		t.Fatalf("裁决被审计失败污染: %+v", res)
	}
	if src.storeCalls != 1 {
		t.Fatalf("storeCalls=%d, want 1", src.storeCalls)
	}
}

func TestDecideNormalPathMarksDegradedWhenCounterBlind(t *testing.T) {
	src := &fakeStore{facts: &Facts{
		Observations:     []Observation{{RuleID: 5, Metric: model.MetricDeviceActionCount, Op: model.OpGTE, Threshold: 3, Available: false}},
		CounterAvailable: false,
	}}
	res, err := NewEngine(src, DefaultConfig()).Decide(context.Background(), Input{RequestID: "r", Mid: 1, Action: model.ActionFollow, Now: testNow})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Degraded || res.Decision != model.DecisionAllow || res.Basis != BasisNoRule {
		t.Fatalf("Redis 故障但事实可读时应放行并标记降级，实际 %+v", res)
	}
	if len(res.SkippedRuleIDs) != 1 || res.SkippedRuleIDs[0] != 5 {
		t.Fatalf("计数不可用的规则应被跳过，实际 %v", res.SkippedRuleIDs)
	}
}
