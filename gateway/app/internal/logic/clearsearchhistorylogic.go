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

type ClearSearchHistoryLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 清空本人搜索历史（需 confirm=true，物理删除）
func NewClearSearchHistoryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ClearSearchHistoryLogic {
	return &ClearSearchHistoryLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ClearSearchHistoryLogic) ClearSearchHistory(req *types.ParamClearSearchHistory) (resp *types.SearchHistoryDeleteResponse, err error) {
	if l.svcCtx.SearchQuery == nil {
		return nil, errors.New("search-query service not configured")
	}
	if !req.Confirm {
		return nil, errors.New("gateway/app: confirm must be true")
	}
	reply, err := l.svcCtx.SearchQuery.ClearSearchHistory(l.ctx, &searchqueryrpc.ClearSearchHistoryReq{
		Mid:       req.Mid,
		Confirm:   true,
		RequestId: req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/app/clearSearchHistory: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.SearchHistoryDeleteResponse{
		Code:    0,
		Message: "ok",
		Data: types.SearchHistoryDeleteData{
			Deleted:     reply.GetDeleted(),
			HardDeleted: reply.GetHardDeleted(),
		},
		TTL: 0,
	}, nil
}
