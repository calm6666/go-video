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

type FsDefinitionGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单个特征定义读（version=0 = 当前 ACTIVE 版本；found=false 不伪造口径）
func NewFsDefinitionGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *FsDefinitionGetLogic {
	return &FsDefinitionGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// FsDefinitionGet 转发 feature-store GetFeatureDefinition（读一个特征版本的定义行）。
//
// version=0 是契约里的合法哨兵「按 ACTIVE 指针解析」，网关不把它换成具体版本号：
// 换了就等于替调用方决定读哪一版，而 ACTIVE 指针随时可能被别人切走。
// found=false 有两种来源（这个 key 从未注册 / 注册了但没有生效版本），服务不伪造口径，
// 网关也不把两者折成一种结论或回一个空 definition 让后台以为「有定义但字段是空的」。
func (l *FsDefinitionGetLogic) FsDefinitionGet(req *types.ParamFsDefinitionGet) (resp *types.FsDefinitionGetResponse, err error) {
	if l.svcCtx.FeatureStore == nil {
		return nil, errFeatureStoreNotConfigured
	}
	if req == nil {
		return nil, errFsRequestMissing
	}
	if err := requireNonEmpty("feature_key", req.FeatureKey); err != nil {
		return nil, err
	}
	if err := fsNonNeg("version", int64(req.Version)); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.FeatureStore.GetFeatureDefinition(l.ctx, &featurestorerpc.GetFeatureDefinitionReq{
		FeatureKey: req.FeatureKey,
		Version:    req.Version,
	})
	if err != nil {
		// key 是口径标识（可含业务含义），只记寻址位与结论，不记定义正文。
		l.Errorf("gateway/admin/fsDefinitionGet: feature_key=%s version=%d err=%v",
			req.FeatureKey, req.Version, err)
		return nil, err
	}
	return &types.FsDefinitionGetResponse{
		Code:    0,
		Message: "ok",
		Data: types.FsDefinitionGetData{
			Found:      reply.GetFound(),
			Definition: fsDefinitionToAPI(reply.GetDefinition()),
		},
		TTL: 0,
	}, nil
}
