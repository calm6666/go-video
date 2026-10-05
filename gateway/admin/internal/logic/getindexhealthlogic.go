// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	searchindexerrpc "go-video/services/search-indexer/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetIndexHealthLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询索引/别名健康与重试、死信积压
func NewGetIndexHealthLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetIndexHealthLogic {
	return &GetIndexHealthLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 索引健康：聚合 search-indexer GetIndexHealth RPC。
// alias 为空表示返回全部已登记别名；doc_count=-1、health=missing 等降级值原样透出，
// 网关不把「读不到」粉饰成 0 或 green（AGENTS.md §9）。
func (l *GetIndexHealthLogic) GetIndexHealth(req *types.ParamIndexHealth) (resp *types.SearchIndexHealthResponse, err error) {
	if l.svcCtx.SearchIndexer == nil {
		return nil, errors.New("search-indexer service not configured")
	}
	if err := requireOperatorID(req.OperatorId); err != nil {
		return nil, err
	}
	// 契约缺口：searchindexer.v1.GetIndexHealthReq 没有 operator 字段，
	// 巡检人只能记在网关日志里。
	reply, err := l.svcCtx.SearchIndexer.GetIndexHealth(l.ctx, &searchindexerrpc.GetIndexHealthReq{
		Alias: req.Alias,
	})
	if err != nil {
		l.Errorf("gateway/admin/getIndexHealth: alias=%s operator_id=%d err=%v", req.Alias, req.OperatorId, err)
		return nil, err
	}
	return &types.SearchIndexHealthResponse{
		Code:    0,
		Message: "ok",
		Data: types.SearchIndexHealthData{
			Aliases:      aliasStatusesToAPI(reply.GetAliases()),
			RetryPending: reply.GetRetryPending(),
			DeadLetter:   reply.GetDeadLetter(),
			OverallState: reply.GetOverallState(),
		},
		TTL: 0,
	}, nil
}
