// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	rankrpc "go-video/services/recommend-rank/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpsertExperimentLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新建/修改实验变体（分桶区间与 hash_seed 变更需 reason 说明）
func NewUpsertExperimentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertExperimentLogic {
	return &UpsertExperimentLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// UpsertExperiment 转发 recommend-rank UpsertExperiment。
// operator 由会话渲染，reason 必填：hash_seed 变更会让同一主体重新分桶（等于把已进组的用户
// 换到别的变体），没有理由的改动无法审计。
// 区间 [bucket_start, bucket_end) 是否合法（0<=start<end<=1000）、同层是否互斥、
// (exp_key,variant_key) 是否已存在、model_version 是否可绑定都由服务判定，网关不复算分桶口径。
// overrides 是受控参数覆盖 JSON——契约禁止出现商业化字段，但**由服务校验**，
// 网关不解析它的键、也不因自己不认识某个键而拒绝。
// 回包的 experiment 是服务落库后的整行（含 revision/state/ctime），网关原样投影，
// 不用表单值覆盖：revision 由服务 +1，表单看不到真实结论。
func (l *UpsertExperimentLogic) UpsertExperiment(req *types.ParamRankExperimentUpsert) (resp *types.RankExperimentUpsertResponse, err error) {
	if l.svcCtx.RecommendRank == nil {
		return nil, errRankServiceNotConfigured
	}
	if req == nil {
		return nil, errRecommendRequestMissing
	}
	operator, err := recommendOperator(l.ctx, "upsertExperiment")
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("exp_key", req.ExpKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("variant_key", req.VariantKey); err != nil {
		return nil, err
	}
	if err := recommendNonNeg("bucket_start", int64(req.BucketStart)); err != nil {
		return nil, err
	}
	if err := recommendNonNeg("bucket_end", int64(req.BucketEnd)); err != nil {
		return nil, err
	}
	if err := recommendNonNeg("start_at", req.StartAt); err != nil {
		return nil, err
	}
	if err := recommendNonNeg("end_at", req.EndAt); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.RecommendRank.UpsertExperiment(l.ctx, &rankrpc.UpsertExperimentReq{
		ExpKey:               req.ExpKey,
		VariantKey:           req.VariantKey,
		LayerKey:             req.LayerKey,
		HashSeed:             req.HashSeed,
		BucketStart:          req.BucketStart,
		BucketEnd:            req.BucketEnd,
		ModelKey:             req.ModelKey,
		ModelVersion:         req.ModelVersion,
		FeatureConfigVersion: req.FeatureConfigVersion,
		Overrides:            req.Overrides,
		StartAt:              req.StartAt,
		EndAt:                req.EndAt,
		Operator:             operator,
		Reason:               req.Reason,
		IdempotencyKey:       req.IdempotencyKey,
	})
	if err != nil {
		l.Errorf("gateway/admin/upsertExperiment: exp_key=%s variant_key=%s buckets=[%d,%d) operator=%s idempotency_key=%s err=%v",
			req.ExpKey, req.VariantKey, req.BucketStart, req.BucketEnd, operator, req.IdempotencyKey, err)
		return nil, err
	}
	return &types.RankExperimentUpsertResponse{
		Code:    0,
		Message: "ok",
		Data: types.RankExperimentUpsertData{
			Experiment:   rankExperimentToAPI(reply.GetExperiment()),
			Deduplicated: reply.GetDeduplicated(),
		},
		TTL: 0,
	}, nil
}
