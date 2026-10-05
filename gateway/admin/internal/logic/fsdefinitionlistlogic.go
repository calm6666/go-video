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

type FsDefinitionListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 特征定义目录分页（含 DRAFT/RETIRED，可按 scope/source/state/隐私上限过滤）
func NewFsDefinitionListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *FsDefinitionListLogic {
	return &FsDefinitionListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// FsDefinitionList 转发 feature-store ListFeatureDefinitions（特征目录分页）。
//
// 这是「有哪些特征」的排障入口，与 spm 的口径注册表读面同档：不带会话、不占权限点。
// 四个过滤位都以 0 = 不限为契约语义（entity_scope/source/state 的 UNSPECIFIED 在这里
// 就是「这个维度不过滤」，与写入口里「UNSPECIFIED 出现即拒」正好相反），因此只挡负数。
// max_privacy_level 是调用方按自身授权主动收敛的上限，网关不代为放宽也不代为收紧。
// pn/ps 在这里是必填：本服务契约的 ps 没有「0 = 默认页大小」（1..100 之外直接拒），
// 所以 0 挡在网关，上限与超限行为仍由服务判定，网关不夹取。
func (l *FsDefinitionListLogic) FsDefinitionList(req *types.ParamFsDefinitionList) (resp *types.FsDefinitionListResponse, err error) {
	if l.svcCtx.FeatureStore == nil {
		return nil, errFeatureStoreNotConfigured
	}
	if req == nil {
		return nil, errFsRequestMissing
	}
	if err := fsPaging(req.Pn, req.Ps); err != nil {
		return nil, err
	}
	if err := fsNonNeg("entity_scope", int64(req.EntityScope)); err != nil {
		return nil, err
	}
	if err := fsNonNeg("source", int64(req.Source)); err != nil {
		return nil, err
	}
	if err := fsNonNeg("state", int64(req.State)); err != nil {
		return nil, err
	}
	if err := fsPrivacyFilter("max_privacy_level", req.MaxPrivacyLevel); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.FeatureStore.ListFeatureDefinitions(l.ctx, &featurestorerpc.ListFeatureDefinitionsReq{
		FeatureKeyPrefix: req.FeatureKeyPrefix,
		EntityScope:      featurestorerpc.EntityScope(req.EntityScope),
		Source:           featurestorerpc.FeatureSource(req.Source),
		State:            featurestorerpc.FeatureState(req.State),
		MaxPrivacyLevel:  featurestorerpc.PrivacyLevel(req.MaxPrivacyLevel),
		Pn:               req.Pn,
		Ps:               req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/fsDefinitionList: prefix=%s pn=%d ps=%d err=%v",
			req.FeatureKeyPrefix, req.Pn, req.Ps, err)
		return nil, err
	}
	return &types.FsDefinitionListResponse{
		Code:    0,
		Message: "ok",
		Data: types.FsDefinitionListData{
			Definitions: fsDefinitionsToAPI(reply.GetDefinitions()),
			Total:       reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
