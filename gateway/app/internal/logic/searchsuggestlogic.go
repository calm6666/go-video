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

type SearchSuggestLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 输入前缀联想
func NewSearchSuggestLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SearchSuggestLogic {
	return &SearchSuggestLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *SearchSuggestLogic) SearchSuggest(req *types.ParamSuggest) (resp *types.SuggestResponse, err error) {
	if l.svcCtx.SearchQuery == nil {
		return nil, errors.New("search-query service not configured")
	}
	reply, err := l.svcCtx.SearchQuery.Suggest(l.ctx, &searchqueryrpc.SuggestReq{
		Keyword:    req.Keyword,
		Limit:      req.Limit,
		ViewerMid:  req.ViewerMid,
		Platform:   req.Platform,
		AppVersion: req.AppVersion,
		SearchType: searchqueryrpc.SearchType(req.SearchType),
	})
	if err != nil {
		l.Errorf("gateway/app/searchSuggest: keyword=%s err=%v", req.Keyword, err)
		return nil, err
	}
	return &types.SuggestResponse{
		Code:    0,
		Message: "ok",
		Data:    types.SuggestData{Items: suggestItemsToAPI(reply.GetItems())},
		TTL:     int64(reply.GetTtl()),
	}, nil
}
