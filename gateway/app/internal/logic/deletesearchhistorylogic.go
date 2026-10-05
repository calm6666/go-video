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

type DeleteSearchHistoryLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除单个搜索历史词（需 confirm=true，物理删除）
func NewDeleteSearchHistoryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteSearchHistoryLogic {
	return &DeleteSearchHistoryLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// DeleteSearchHistory 隐私数据物理删除，必须带 confirm=true；
// 网关不做软删保留（AGENTS.md §5 隐私要求）。
func (l *DeleteSearchHistoryLogic) DeleteSearchHistory(req *types.ParamDeleteSearchHistory) (resp *types.SearchHistoryDeleteResponse, err error) {
	if l.svcCtx.SearchQuery == nil {
		return nil, errors.New("search-query service not configured")
	}
	if !req.Confirm {
		return nil, errors.New("gateway/app: confirm must be true")
	}
	reply, err := l.svcCtx.SearchQuery.DeleteSearchHistory(l.ctx, &searchqueryrpc.DeleteSearchHistoryReq{
		Mid:       req.Mid,
		Keyword:   req.Keyword,
		Confirm:   true,
		RequestId: req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/app/deleteSearchHistory: mid=%d keyword=%s err=%v", req.Mid, req.Keyword, err)
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
