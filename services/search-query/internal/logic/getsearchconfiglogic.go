package logic

import (
	"context"

	"go-video/services/search-query/internal/svc"
	"go-video/services/search-query/model"
	"go-video/services/search-query/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetSearchConfigLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetSearchConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetSearchConfigLogic {
	return &GetSearchConfigLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetSearchConfig 该端/分区的排序与分页能力配置（无 UI 硬编码）。
//
// 这里只返回“服务端能做什么”：搜索类型、排序、时长筛选的枚举集合与分页限制，
// 客户端据此渲染筛选项；文案、顺序、样式等资源位行为不在服务端（AGENTS.md §6）。
//
// 关于分区：当前实现没有分区级排序覆盖（无对应配置表），zone_id 只用于参数校验，
// 该缺口已在 README「已知缺口」中登记。
func (l *GetSearchConfigLogic) GetSearchConfig(in *rpc.GetSearchConfigReq) (*rpc.GetSearchConfigReply, error) {
	if in == nil {
		in = &rpc.GetSearchConfigReq{}
	}
	repo := l.svcCtx.Repository
	cfg := repo.Conf()

	if !isSupportedPlatform(in.Platform) {
		return nil, model.ErrInvalidPlatform
	}
	if _, err := docTypesOf(in.SearchType); err != nil {
		return nil, err
	}
	if in.ZoneId < 0 {
		return nil, model.ErrInvalidPage
	}

	sorts := supportedSorts(in.SearchType)
	defaultSort, fellBack := defaultSortFor(in.SearchType, cfg.DefaultSort)
	if fellBack {
		l.Errorf("search-query/GetSearchConfig: configured default_sort=%d is unusable for type=%v, served %d instead",
			cfg.DefaultSort, in.SearchType, defaultSort)
	}

	return &rpc.GetSearchConfigReply{
		DefaultSort:        defaultSort,
		SupportedSorts:     sorts,
		SupportedTypes:     supportedTypes(),
		SupportedDurations: supportedDurations(),
		PsDefault:          cfg.PsDefault,
		PsLimit:            cfg.PsLimit,
		MaxOffset:          cfg.MaxOffset,
		KeywordMaxLen:      int32(cfg.KeywordMaxLen),
		CacheTtlSeconds:    int32(cfg.CacheTTLSeconds),
		EngineAvailable:    repo.EngineAvailable(),
	}, nil
}

// defaultSortFor 解析该搜索类型下真正可用的默认排序，第二个返回值表示是否发生回落。
// 配置写错（例如给全站搜索配了「最多粉丝」）时回落到综合排序，
// 而不是让客户端拿到一个自己用不了的默认值。
func defaultSortFor(t rpc.SearchType, configured int32) (rpc.SortMode, bool) {
	mode := rpc.SortMode(configured)
	if mode == rpc.SortMode_SORT_UNSPECIFIED {
		mode = rpc.SortMode_SORT_COMPREHENSIVE
	}
	if _, err := sortFieldsOf(t, mode); err != nil {
		return rpc.SortMode_SORT_COMPREHENSIVE, true
	}
	return mode, false
}

// supportedSorts 该搜索类型允许的排序集合（按客户端展示顺序）。
func supportedSorts(t rpc.SearchType) []rpc.SortMode {
	modes := []rpc.SortMode{
		rpc.SortMode_SORT_COMPREHENSIVE,
		rpc.SortMode_SORT_LATEST,
		rpc.SortMode_SORT_MOST_VIEW,
		rpc.SortMode_SORT_HOT_SCORE,
	}
	if _, err := sortFieldsOf(t, rpc.SortMode_SORT_MOST_FANS); err == nil {
		modes = append(modes, rpc.SortMode_SORT_MOST_FANS)
	}
	return modes
}

// supportedTypes 支持的搜索类型（不含 UNSPECIFIED 占位值）。
func supportedTypes() []rpc.SearchType {
	return []rpc.SearchType{
		rpc.SearchType_SEARCH_TYPE_ALL,
		rpc.SearchType_SEARCH_TYPE_VIDEO,
		rpc.SearchType_SEARCH_TYPE_USER,
		rpc.SearchType_SEARCH_TYPE_PGC,
	}
}

// supportedDurations 支持的时长筛选（不含 UNSPECIFIED 占位值）。
func supportedDurations() []rpc.DurationBucket {
	return []rpc.DurationBucket{
		rpc.DurationBucket_DURATION_LT_1MIN,
		rpc.DurationBucket_DURATION_1_10MIN,
		rpc.DurationBucket_DURATION_10_30MIN,
		rpc.DurationBucket_DURATION_30_60MIN,
		rpc.DurationBucket_DURATION_GT_60MIN,
	}
}
