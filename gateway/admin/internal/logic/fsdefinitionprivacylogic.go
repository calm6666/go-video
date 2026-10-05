// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	featurestorerpc "go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type FsDefinitionPrivacyLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 调整特征版本隐私级别（独立入口、独立留痕；不改变值语义）
func NewFsDefinitionPrivacyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *FsDefinitionPrivacyLogic {
	return &FsDefinitionPrivacyLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// FsDefinitionPrivacy 转发 feature-store UpdateFeaturePrivacy（调整某个版本的隐私级别）。
//
// 它和 /definition/state 分成两条路由是有意的：隐私级别不改值语义，只改「谁能读」，
// 因此必须单独占一个动作、单独留一条审计，不能混在「上线/下线」这一次操作里被顺手改掉。
// privacy_level 必须 > 0：未声明级别的特征不允许存在（0 = UNSPECIFIED 在服务侧也必拒）。
// 级别与主体维度是否自洽（mid 维度能不能降成 PUBLIC_AGGREGATE、降档要不要工单号）由服务判定；
// 网关不比对旧值，也不因为「级别在降」就自行拒绝或放行——那是一次有审计的真实操作。
func (l *FsDefinitionPrivacyLogic) FsDefinitionPrivacy(req *types.ParamFsDefinitionPrivacy) (resp *types.FsDefinitionPrivacyResponse, err error) {
	if l.svcCtx.FeatureStore == nil {
		return nil, errFeatureStoreNotConfigured
	}
	if req == nil {
		return nil, errFsRequestMissing
	}
	operator, err := fsOperator(l.ctx, "fsDefinitionPrivacy")
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("feature_key", req.FeatureKey); err != nil {
		return nil, err
	}
	if err := fsPositive("version", req.Version); err != nil {
		return nil, err
	}
	if err := fsPositive("privacy_level", req.PrivacyLevel); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.FeatureStore.UpdateFeaturePrivacy(l.ctx, &featurestorerpc.UpdateFeaturePrivacyReq{
		FeatureKey:   req.FeatureKey,
		Version:      req.Version,
		PrivacyLevel: featurestorerpc.PrivacyLevel(req.PrivacyLevel),
		Operator:     operator,
		Reason:       req.Reason,
		RequestId:    req.IdempotencyKey,
	})
	if err != nil {
		l.Errorf("gateway/admin/fsDefinitionPrivacy: feature_key=%s version=%d privacy_level=%d operator=%s trace_id=%s err=%v",
			req.FeatureKey, req.Version, req.PrivacyLevel, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/fsDefinitionPrivacy: feature_key=%s version=%d privacy_level=%d reused=%t operator=%s",
		req.FeatureKey, req.Version, req.PrivacyLevel, reply.GetReused(), operator)
	return &types.FsDefinitionPrivacyResponse{
		Code:    0,
		Message: "ok",
		Data: types.FsDefinitionPrivacyData{
			Definition: fsDefinitionToAPI(reply.GetDefinition()),
			Reused:     reply.GetReused(),
		},
		TTL: 0,
	}, nil
}
