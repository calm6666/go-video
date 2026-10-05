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

type SearchLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 关键词搜索（cursor 优先分页；引擎不可用返回明确错误）
func NewSearchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SearchLogic {
	return &SearchLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// Search 聚合搜索命中。引擎不可用时 search-query 返回错误，网关原样抛出，
// 绝不降级成"空结果 + code 0"（AGENTS.md §9：不得伪造成功）。
func (l *SearchLogic) Search(req *types.ParamSearch) (resp *types.SearchResponse, err error) {
	if l.svcCtx.SearchQuery == nil {
		return nil, errors.New("search-query service not configured")
	}
	reply, err := l.svcCtx.SearchQuery.Search(l.ctx, &searchqueryrpc.SearchReq{
		Keyword:         req.Keyword,
		SearchType:      searchqueryrpc.SearchType(req.SearchType),
		ZoneId:          req.ZoneId,
		Duration:        searchqueryrpc.DurationBucket(req.Duration),
		Sort:            searchqueryrpc.SortMode(req.Sort),
		Pn:              req.Pn,
		Ps:              req.Ps,
		Cursor:          req.Cursor,
		ViewerMid:       req.ViewerMid,
		Platform:        req.Platform,
		AppVersion:      req.AppVersion,
		RequestId:       req.RequestId,
		PublishedAfter:  req.PublishedAfter,
		PublishedBefore: req.PublishedBefore,
	})
	if err != nil {
		l.Errorf("gateway/app/search: keyword=%s type=%d mid=%d err=%v", req.Keyword, req.SearchType, req.ViewerMid, err)
		return nil, err
	}
	return &types.SearchResponse{
		Code:    0,
		Message: "ok",
		Data: types.SearchData{
			Hits:         searchHitsToAPI(reply.GetHits()),
			Total:        reply.GetTotal(),
			Pn:           reply.GetPn(),
			Ps:           reply.GetPs(),
			NextCursor:   reply.GetNextCursor(),
			HasMore:      reply.GetHasMore(),
			SafeFiltered: reply.GetSafeFiltered(),
			CacheHit:     reply.GetCacheHit(),
		},
		TTL: int64(reply.GetTtl()),
	}, nil
}
