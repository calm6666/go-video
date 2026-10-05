// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	collectorrpc "go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CollectorPolicyUpsertLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新建/修改策略草稿（state 不可声明，服务按 DRAFT 落；ACTIVE 不可原地改）
func NewCollectorPolicyUpsertLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CollectorPolicyUpsertLogic {
	return &CollectorPolicyUpsertLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CollectorPolicyUpsert 转发 event-collector UpsertDispatchPolicy。
//
// 本路由只写草稿，不写「生效」：表单没有 state 位，网关构造 policy 时也刻意不设置
// PolicyState（保持 UNSPECIFIED），服务据此按 DRAFT 落库并拒绝任何非 DRAFT 的声明。
// 这样 collector:policy/update（配置一条采样规则）与 collector:policy/enable
// （让它对所有终端上报生效）才是两个可分别授予的能力——后者直接影响所有下游特征完整性。
//
// 数值位的 0 一律是「继承服务/config 默认」的哨兵（policy.go 的 inherit32/inherit64），
// 因此网关只挡负数；采样基点区间、salt_ref 是否是大写环境变量名、field_whitelist 是否
// 覆盖内置隐私底线、条目数上限（maxPolicyListItems）与版本语义化格式全部由服务判定。
// 盐值本身从不出现在请求、响应或日志里（AGENTS.md §7）。
//
// created=true 表示新建了版本、false 表示改的是既有草稿，两者都是成功；
// ACTIVE 不可原地改（ErrActivePolicyImmutable）、ARCHIVED 只读（ErrPolicyNotDraft）
// 与并发被抢改（ErrConcurrentUpdate）都原样经四字段信封抛出，不折叠成「保存成功」。
func (l *CollectorPolicyUpsertLogic) CollectorPolicyUpsert(req *types.ParamCollectorPolicyUpsert) (resp *types.CollectorPolicyResponse, err error) {
	if l.svcCtx.EventCollector == nil {
		return nil, errCollectorServiceNotConfigured
	}
	if req == nil {
		return nil, errCollectorRequestMissing
	}
	operator, err := collectorOperator(l.ctx, "collectorPolicyUpsert")
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("version", req.Version); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("salt_ref", req.SaltRef); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := collectorNonNeg32("salt_version", req.SaltVersion); err != nil {
		return nil, err
	}
	for _, v := range []struct {
		field string
		num   int64
	}{
		{"max_events_per_batch", int64(req.MaxEventsPerBatch)},
		{"max_request_bytes", req.MaxRequestBytes},
		{"max_event_payload_bytes", int64(req.MaxEventPayloadBytes)},
		{"max_clock_skew_seconds", int64(req.MaxClockSkewSeconds)},
		{"max_backfill_seconds", int64(req.MaxBackfillSeconds)},
		{"keyword_max_runes", int64(req.KeywordMaxRunes)},
		{"retention_days", int64(req.RetentionDays)},
		{"deliver_max_attempts", int64(req.DeliverMaxAttempts)},
		{"retry_base_seconds", req.RetryBaseSeconds},
		{"retry_max_seconds", req.RetryMaxSeconds},
	} {
		if err := collectorNonNeg(v.field, v.num); err != nil {
			return nil, err
		}
	}
	for _, r := range req.SampleRules {
		if err := collectorNonNeg32("sample_rules.sample_bps", r.SampleBps); err != nil {
			return nil, err
		}
		if err := requireNonEmpty("sample_rules.event_type", r.EventType); err != nil {
			return nil, err
		}
	}
	reply, err := l.svcCtx.EventCollector.UpsertDispatchPolicy(l.ctx, &collectorrpc.UpsertDispatchPolicyReq{
		Policy:         collectorPolicyForRPC(req),
		IdempotencyKey: req.IdempotencyKey,
		Operator:       operator,
	})
	if err != nil {
		l.Errorf("gateway/admin/collectorPolicyUpsert: version=%s operator=%s err=%v", req.Version, operator, err)
		return nil, err
	}
	return &types.CollectorPolicyResponse{
		Code:    0,
		Message: "ok",
		Data: types.CollectorPolicyData{
			Policy:  collectorPolicyToAPI(reply.GetPolicy()),
			Created: reply.GetCreated(),
		},
		TTL: 0,
	}, nil
}
