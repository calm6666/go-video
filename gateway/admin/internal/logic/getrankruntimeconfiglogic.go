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

type GetRankRuntimeConfigLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 在线排序参数与当前生效的模型/特征/实验
func NewGetRankRuntimeConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRankRuntimeConfigLogic {
	return &GetRankRuntimeConfigLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// GetRankRuntimeConfig 转发 recommend-rank GetRankRuntimeConfig。
// model_key 空表示「服务默认 model_key」，网关不挑一个填上去（猜错会让后台看错模型的配置）。
// active_model_version 为空是必须让后台看见的事实：按契约此时在线必然降级，
// 网关不把它兜成「看起来正常」；running_experiments 只含 RUNNING 变体（服务口径），
// 要看暂停/结束的历史变体得走实验台账，本路由不代为补全。
// config_revision 是灰度核对用的配置代次摘要，网关原样回传、不自算。
func (l *GetRankRuntimeConfigLogic) GetRankRuntimeConfig(req *types.ParamRankRuntimeConfig) (resp *types.RankRuntimeConfigResponse, err error) {
	if l.svcCtx.RecommendRank == nil {
		return nil, errRankServiceNotConfigured
	}
	if req == nil {
		return nil, errRecommendRequestMissing
	}
	reply, err := l.svcCtx.RecommendRank.GetRankRuntimeConfig(l.ctx, &rankrpc.GetRankRuntimeConfigReq{
		Scene:    req.Scene,
		ModelKey: req.ModelKey,
	})
	if err != nil {
		l.Errorf("gateway/admin/getRankRuntimeConfig: scene=%s model_key=%s err=%v", req.Scene, req.ModelKey, err)
		return nil, err
	}
	return &types.RankRuntimeConfigResponse{
		Code:    0,
		Message: "ok",
		Data: types.RankRuntimeConfigData{
			ModelKey:             reply.GetModelKey(),
			ActiveModelVersion:   reply.GetActiveModelVersion(),
			FeatureConfigVersion: reply.GetFeatureConfigVersion(),
			MaxCandidates:        reply.GetMaxCandidates(),
			MaxReturn:            reply.GetMaxReturn(),
			Objectives:           rankStrings(reply.GetObjectives()),
			DegradeEnabled:       reply.GetDegradeEnabled(),
			Fallback:             int32(reply.GetFallback()),
			ScoreBudgetMs:        reply.GetScoreBudgetMs(),
			TtlSeconds:           reply.GetTtlSeconds(),
			RunningExperiments:   rankExperimentsToAPI(reply.GetRunningExperiments()),
			ConfigRevision:       reply.GetConfigRevision(),
		},
		TTL: 0,
	}, nil
}
