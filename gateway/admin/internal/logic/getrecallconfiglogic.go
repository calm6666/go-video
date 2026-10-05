// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	recallrpc "go-video/services/recommend-recall/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetRecallConfigLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 在线召回参数与有 CURRENT 版本的池摘要
func NewGetRecallConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRecallConfigLogic {
	return &GetRecallConfigLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// GetRecallConfig 转发 recommend-recall GetRecallConfig。
// 这条路由是**读服务当前生效参数**，不是「把参数改给某个场景」：proto 里没有任何写入语义，
// 网关也不提供改配置的面（改在线参数属服务配置发布，走 services/ops-config 的既有流程）。
// mid=0 是游客口径的合法值（游客只允许冷启动/热门路，具体哪几路可用由服务回），
// scene 目前是服务级参数的预留位，网关不因它未生效而拒绝请求。
// ttl_seconds 是**服务给终端面的建议缓存秒数**，与响应信封的 ttl 是两件事：
// 网关自己不缓存，信封固定 ttl=0（见 README：客户端不应缓存后台排障读数）。
func (l *GetRecallConfigLogic) GetRecallConfig(req *types.ParamRecommendRecallConfig) (resp *types.RecommendRecallConfigResponse, err error) {
	if l.svcCtx.RecommendRecall == nil {
		return nil, errRecallServiceNotConfigured
	}
	if req == nil {
		return nil, errRecommendRequestMissing
	}
	if err := recommendNonNeg("mid", req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.RecommendRecall.GetRecallConfig(l.ctx, &recallrpc.GetRecallConfigReq{
		Scene: req.Scene,
		Mid:   req.Mid,
	})
	if err != nil {
		l.Errorf("gateway/admin/getRecallConfig: scene=%s mid=%d err=%v", req.Scene, req.Mid, err)
		return nil, err
	}
	return &types.RecommendRecallConfigResponse{
		Code:    0,
		Message: "ok",
		Data: types.RecommendRecallConfigData{
			MaxCandidates:  reply.GetMaxCandidates(),
			DefaultLimit:   reply.GetDefaultLimit(),
			PerSourceMax:   reply.GetPerSourceMax(),
			EnabledSources: recallSourcesToAPI(reply.GetEnabledSources()),
			DefaultSources: recallSourcesToAPI(reply.GetDefaultSources()),
			MaxSeedAids:    reply.GetMaxSeedAids(),
			MaxSeedTags:    reply.GetMaxSeedTags(),
			MaxExcludeAids: reply.GetMaxExcludeAids(),
			DegradeEnabled: reply.GetDegradeEnabled(),
			FallbackSource: int32(reply.GetFallbackSource()),
			TtlSeconds:     reply.GetTtlSeconds(),
			ReadyPools:     recallPoolStatusesToAPI(reply.GetReadyPools()),
		},
		TTL: 0,
	}, nil
}
