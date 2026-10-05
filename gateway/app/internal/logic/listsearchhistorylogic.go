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

type ListSearchHistoryLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 本人搜索历史
func NewListSearchHistoryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListSearchHistoryLogic {
	return &ListSearchHistoryLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListSearchHistoryLogic) ListSearchHistory(req *types.ParamSearchHistory) (resp *types.SearchHistoryResponse, err error) {
	if l.svcCtx.SearchQuery == nil {
		return nil, errors.New("search-query service not configured")
	}
	reply, err := l.svcCtx.SearchQuery.ListSearchHistory(l.ctx, &searchqueryrpc.ListSearchHistoryReq{
		Mid:      req.Mid,
		Cursor:   req.Cursor,
		Limit:    req.Limit,
		Platform: req.Platform,
	})
	if err != nil {
		l.Errorf("gateway/app/listSearchHistory: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.SearchHistoryResponse{
		Code:    0,
		Message: "ok",
		Data: types.SearchHistoryData{
			Items:      searchHistoryItemsToAPI(reply.GetItems()),
			NextCursor: reply.GetNextCursor(),
			HasMore:    reply.GetHasMore(),
		},
		TTL: 0,
	}, nil
}
