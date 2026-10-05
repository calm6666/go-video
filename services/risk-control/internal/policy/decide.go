package policy

import (
	"context"
	"time"

	"go-video/common/idgen"
	"go-video/services/risk-control/model"

	"github.com/zeromicro/go-zero/core/logx"
)

// Store 是引擎依赖的事实来源，由 repository 实现。
// 拆成接口的目的：决策逻辑可以在没有任何外部依赖的单测里跑完整路径（含降级）。
type Store interface {
	// LookupCached 读取同一 request_id 近期已产出的裁决（幂等回放）。
	// 读失败必须当作未命中，不能因为缓存故障影响裁决。
	LookupCached(ctx context.Context, requestID string) (*Result, bool)
	// LoadFacts 装载名单/处罚/规则观测。
	// 返回 error 表示 DB 不可用或事实缺失，引擎据此走降级路径。
	LoadFacts(ctx context.Context, in Input) (*Facts, error)
	// StoreResult 写裁决日志与幂等缓存；错误只影响审计完整性，不改变裁决。
	StoreResult(ctx context.Context, in Input, res *Result) error
}

// Engine 把 Store 与纯函数 Evaluate 串成一次可解释裁决。
type Engine struct {
	src Store
	cfg Config
}

// NewEngine 构造引擎。
func NewEngine(src Store, cfg Config) *Engine {
	return &Engine{src: src, cfg: cfg.WithDefaults()}
}

// Decide 执行一次裁决。
// 返回值只有在入参非法时才为 error；依赖故障一律走降级并置 Result.Degraded，
// 让调用方始终能拿到「一个可解释的裁决」，而不是一个裸的 gRPC 错误。
func (e *Engine) Decide(ctx context.Context, in Input) (*Result, error) {
	if !model.ValidAction(in.Action) {
		return nil, model.ErrInvalidTarget
	}
	if in.Now == 0 {
		in.Now = time.Now().Unix()
	}
	if in.RequestID == "" {
		in.RequestID = idgen.MustULID()
	}

	if cached, ok := e.src.LookupCached(ctx, in.RequestID); ok && cached != nil {
		return cached, nil
	}

	facts, err := e.src.LoadFacts(ctx, in)
	var res *Result
	if err != nil {
		res = e.degradeDB(in)
		logx.WithContext(ctx).Errorf("risk-control: load facts degraded action=%d mid=%d err=%v", in.Action, in.Mid, err)
	} else {
		res = Evaluate(in, *facts, e.cfg)
	}
	res.RequestID = in.RequestID

	// 审计写入失败不翻转裁决：裁决已定，日志完整性是次要问题。
	// DB 已判定不可用时跳过日志写入，避免在故障期间继续压同一份连接池。
	if storeErr := e.src.StoreResult(ctx, in, res); storeErr != nil {
		logx.WithContext(ctx).Errorf("risk-control: store decision failed request_id=%s err=%v", in.RequestID, storeErr)
	}
	return res, nil
}

// degradeDB 按 DegradePolicy 给出依赖故障时的裁决。
// 高危动作（投稿/登录/改名/开播）BLOCK-on-error，低危动作 ALLOW-on-error，
// 理由见 DefaultHighRiskActions 注释与 README。
func (e *Engine) degradeDB(in Input) *Result {
	decision := e.cfg.Degrade.DecisionFor(in.Action)
	res := &Result{
		RequestID: in.RequestID,
		Decision:  decision,
		Basis:     BasisFallbackDB,
		Score:     decisionScore(decision),
		Degraded:  true,
	}
	switch decision {
	case model.DecisionChallenge:
		res.ActionCode = ActionCodeChallenge
		res.ChallengeTTLSeconds = e.cfg.ChallengeTTLSeconds
	case model.DecisionBlock:
		res.ActionCode = ActionCodeUnavailable
	case model.DecisionReview:
		res.ActionCode = ActionCodeReview
	default:
		res.ActionCode = ActionCodeNone
	}
	return res
}
