// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	searchqueryrpc "go-video/services/search-query/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SearchConfigLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 该端/分区的排序与分页能力配置
func NewSearchConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SearchConfigLogic {
	return &SearchConfigLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SearchConfig 返回该端/分区的搜索能力参数（枚举与分页上限），
// 客户端据此渲染筛选项；网关不写死任何端专属 UI 行为（AGENTS.md §6）。
func (l *SearchConfigLogic) SearchConfig(req *types.ParamSearchConfig) (resp *types.SearchConfigResponse, err error) {
	if l.svcCtx.SearchQuery == nil {
		return nil, errors.New("search-query service not configured")
	}
	reply, err := l.svcCtx.SearchQuery.GetSearchConfig(l.ctx, &searchqueryrpc.GetSearchConfigReq{
		Platform:   req.Platform,
		AppVersion: req.AppVersion,
		SearchType: searchqueryrpc.SearchType(req.SearchType),
		ZoneId:     req.ZoneId,
	})
	if err != nil {
		l.Errorf("gateway/app/searchConfig: platform=%s zone=%d err=%v", req.Platform, req.ZoneId, err)
		return nil, err
	}
	sorts := make([]int32, 0, len(reply.GetSupportedSorts()))
	for _, s := range reply.GetSupportedSorts() {
		sorts = append(sorts, int32(s))
	}
	searchTypes := make([]int32, 0, len(reply.GetSupportedTypes()))
	for _, t := range reply.GetSupportedTypes() {
		searchTypes = append(searchTypes, int32(t))
	}
	durations := make([]int32, 0, len(reply.GetSupportedDurations()))
	for _, d := range reply.GetSupportedDurations() {
		durations = append(durations, int32(d))
	}
	return &types.SearchConfigResponse{
		Code:    0,
		Message: "ok",
		Data: types.SearchConfigData{
			DefaultSort:        int32(reply.GetDefaultSort()),
			SupportedSorts:     sorts,
			SupportedTypes:     searchTypes,
			SupportedDurations: durations,
			PsDefault:          reply.GetPsDefault(),
			PsLimit:            reply.GetPsLimit(),
			MaxOffset:          reply.GetMaxOffset(),
			KeywordMaxLen:      reply.GetKeywordMaxLen(),
			CacheTtlSeconds:    reply.GetCacheTtlSeconds(),
			EngineAvailable:    reply.GetEngineAvailable(),
		},
		TTL: int64(reply.GetCacheTtlSeconds()),
	}, nil
}
