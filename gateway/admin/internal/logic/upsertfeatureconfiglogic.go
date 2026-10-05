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

type UpsertFeatureConfigLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 登记/更新特征配置版本（feature_keys 清单与缺失值策略）
func NewUpsertFeatureConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertFeatureConfigLogic {
	return &UpsertFeatureConfigLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// UpsertFeatureConfig 转发 recommend-rank UpsertFeatureConfig。
// operator 由会话渲染；feature_keys 按表单顺序原样交给服务（条数上限 MaxFeatureKeys、
// key 是否在受控清单内、missing_policy 取值集合 default/drop_source/reject 都由服务判定），
// 网关不去重、不排序、不裁剪——去重会把「登记了两遍同一个特征」这件事藏起来。
// feature_store_scene 只是将来接 feature-store 的读取场景 key，本期不产生任何下游调用。
func (l *UpsertFeatureConfigLogic) UpsertFeatureConfig(req *types.ParamRankFeatureConfigUpsert) (resp *types.RankFeatureConfigUpsertResponse, err error) {
	if l.svcCtx.RecommendRank == nil {
		return nil, errRankServiceNotConfigured
	}
	if req == nil {
		return nil, errRecommendRequestMissing
	}
	operator, err := recommendOperator(l.ctx, "upsertFeatureConfig")
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("config_version", req.ConfigVersion); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(req.FeatureKeys))
	keys = append(keys, req.FeatureKeys...)
	reply, err := l.svcCtx.RecommendRank.UpsertFeatureConfig(l.ctx, &rankrpc.UpsertFeatureConfigReq{
		ConfigVersion:     req.ConfigVersion,
		FeatureKeys:       keys,
		MissingPolicy:     req.MissingPolicy,
		FeatureStoreScene: req.FeatureStoreScene,
		Operator:          operator,
		Reason:            req.Reason,
		IdempotencyKey:    req.IdempotencyKey,
	})
	if err != nil {
		l.Errorf("gateway/admin/upsertFeatureConfig: config_version=%s keys=%d operator=%s idempotency_key=%s err=%v",
			req.ConfigVersion, len(req.FeatureKeys), operator, req.IdempotencyKey, err)
		return nil, err
	}
	return &types.RankFeatureConfigUpsertResponse{
		Code:    0,
		Message: "ok",
		Data: types.RankFeatureConfigUpsertData{
			ConfigVersion: reply.GetConfigVersion(),
			FeatureCount:  reply.GetFeatureCount(),
			Revision:      reply.GetRevision(),
			Deduplicated:  reply.GetDeduplicated(),
		},
		TTL: 0,
	}, nil
}
