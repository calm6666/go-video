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

type FsDefinitionStateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 变更特征版本状态（DRAFT/ACTIVE/RETIRED；状态机与理由要求由服务判定）
func NewFsDefinitionStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *FsDefinitionStateLogic {
	return &FsDefinitionStateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// FsDefinitionState 转发 feature-store UpdateFeatureState（推进某个版本的 DRAFT→ACTIVE→RETIRED）。
//
// state 与 version 都必须是正数：0 在这个入口是「没选」（UNSPECIFIED 不是任何一种状态），
// 而不是读侧的「按 ACTIVE 指针」——按指针改状态等于改一个调用方没点过的版本。
// 迁移是否合法（能否跳过 ACTIVE 直接 RETIRED、RETIRED 后能否复活、上线是否要求回填完成）
// 与 reason 的最低要求全在服务侧；网关只保证「必填位非空」，不改写理由也不补默认理由。
// 回执里的 definition 是服务落库后的真实状态行（含 mtime），不是请求的复读。
func (l *FsDefinitionStateLogic) FsDefinitionState(req *types.ParamFsDefinitionState) (resp *types.FsDefinitionStateResponse, err error) {
	if l.svcCtx.FeatureStore == nil {
		return nil, errFeatureStoreNotConfigured
	}
	if req == nil {
		return nil, errFsRequestMissing
	}
	operator, err := fsOperator(l.ctx, "fsDefinitionState")
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
	if err := fsPositive("state", req.State); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.FeatureStore.UpdateFeatureState(l.ctx, &featurestorerpc.UpdateFeatureStateReq{
		FeatureKey: req.FeatureKey,
		Version:    req.Version,
		State:      featurestorerpc.FeatureState(req.State),
		Operator:   operator,
		Reason:     req.Reason,
		RequestId:  req.IdempotencyKey,
	})
	if err != nil {
		l.Errorf("gateway/admin/fsDefinitionState: feature_key=%s version=%d state=%d operator=%s trace_id=%s err=%v",
			req.FeatureKey, req.Version, req.State, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/fsDefinitionState: feature_key=%s version=%d state=%d reused=%t operator=%s",
		req.FeatureKey, req.Version, req.State, reply.GetReused(), operator)
	return &types.FsDefinitionStateResponse{
		Code:    0,
		Message: "ok",
		Data: types.FsDefinitionStateData{
			Definition: fsDefinitionToAPI(reply.GetDefinition()),
			Reused:     reply.GetReused(),
		},
		TTL: 0,
	}, nil
}
