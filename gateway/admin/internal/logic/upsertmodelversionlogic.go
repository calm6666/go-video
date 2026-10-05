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

type UpsertModelVersionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 登记/更新模型版本元数据（版本不可变，元数据变更 revision+1）
func NewUpsertModelVersionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertModelVersionLogic {
	return &UpsertModelVersionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// UpsertModelVersion 转发 recommend-rank UpsertModelVersion。
// operator 由会话渲染（gateway/admin:<admin_id>），表单不声明；reason 与 idempotency_key 必填，
// 幂等键原样透传。objective 是否在受控集合内、权重总和上限（服务校验不超过 10）、
// feature_config_version 是否已登记、version 是否已被占用全部由服务判定，网关不复算。
// artifact_ref 只是对象存储 key：模型本体与任何密钥都不经网关，网关也不代为签名下载地址。
// offline_metrics 是仅展示用的 JSON 文本，网关不解析、不校验其结构（服务也只当审计证据）。
func (l *UpsertModelVersionLogic) UpsertModelVersion(req *types.ParamRankModelVersionUpsert) (resp *types.RankModelVersionUpsertResponse, err error) {
	if l.svcCtx.RecommendRank == nil {
		return nil, errRankServiceNotConfigured
	}
	if req == nil {
		return nil, errRecommendRequestMissing
	}
	operator, err := recommendOperator(l.ctx, "upsertModelVersion")
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("model_key", req.ModelKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("version", req.Version); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("feature_config_version", req.FeatureConfigVersion); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.RecommendRank.UpsertModelVersion(l.ctx, &rankrpc.UpsertModelVersionReq{
		ModelKey:             req.ModelKey,
		Version:              req.Version,
		FeatureConfigVersion: req.FeatureConfigVersion,
		ObjectiveWeights:     rankObjectiveWeightsForRPC(req.ObjectiveWeights),
		ArtifactRef:          req.ArtifactRef,
		OfflineMetrics:       req.OfflineMetrics,
		Operator:             operator,
		Reason:               req.Reason,
		IdempotencyKey:       req.IdempotencyKey,
	})
	if err != nil {
		l.Errorf("gateway/admin/upsertModelVersion: model_key=%s version=%s operator=%s idempotency_key=%s err=%v",
			req.ModelKey, req.Version, operator, req.IdempotencyKey, err)
		return nil, err
	}
	return &types.RankModelVersionUpsertResponse{
		Code:    0,
		Message: "ok",
		Data: types.RankModelVersionUpsertData{
			ModelKey:     reply.GetModelKey(),
			Version:      reply.GetVersion(),
			State:        int32(reply.GetState()),
			Revision:     reply.GetRevision(),
			Deduplicated: reply.GetDeduplicated(),
		},
		TTL: 0,
	}, nil
}
