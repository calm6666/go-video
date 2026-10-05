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

type GetPoolSnapshotLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 池快照：读某个池某个版本的条目（version=0 表示当前生效版本）
func NewGetPoolSnapshotLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetPoolSnapshotLogic {
	return &GetPoolSnapshotLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// GetPoolSnapshot 转发 recommend-recall GetPoolSnapshot。
// version=0 是「读 recall_pool_current 指向的 CURRENT 版本」的合法哨兵，不是缺参；
// 页大小上限（MaxPoolSnapshotPage）与「这个池到底有没有这个版本」都由服务判定，
// 网关不改写 pn/ps、不缓存结果——池切了版本后台必须立刻看见新指针，多一层网关缓存
// 就多一段「谁在看旧池子」的争议（与 live 字典同一口径）。
// items 为空只表示「这一段没有条目」，与下游没接（错误）是两回事。
func (l *GetPoolSnapshotLogic) GetPoolSnapshot(req *types.ParamRecommendPoolSnapshot) (resp *types.RecommendPoolSnapshotResponse, err error) {
	if l.svcCtx.RecommendRecall == nil {
		return nil, errRecallServiceNotConfigured
	}
	if req == nil {
		return nil, errRecommendRequestMissing
	}
	pool, err := recommendPoolRef(req.Source, req.PoolKey)
	if err != nil {
		return nil, err
	}
	if err := recommendNonNeg("version", req.Version); err != nil {
		return nil, err
	}
	if err := recommendPaging(req.Pn, req.Ps); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.RecommendRecall.GetPoolSnapshot(l.ctx, &recallrpc.GetPoolSnapshotReq{
		Pool:    pool,
		Version: req.Version,
		Pn:      req.Pn,
		Ps:      req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/getPoolSnapshot: source=%d pool_key=%s version=%d pn=%d err=%v",
			req.Source, req.PoolKey, req.Version, req.Pn, err)
		return nil, err
	}
	return &types.RecommendPoolSnapshotResponse{
		Code:    0,
		Message: "ok",
		Data: types.RecommendPoolSnapshotData{
			Pool:        recallPoolEcho(reply.GetPool(), types.RecommendPoolRef{Source: req.Source, PoolKey: req.PoolKey}),
			Version:     reply.GetVersion(),
			BatchId:     reply.GetBatchId(),
			State:       int32(reply.GetState()),
			ItemCount:   reply.GetItemCount(),
			Items:       recallPoolItemsToAPI(reply.GetItems()),
			HasMore:     reply.GetHasMore(),
			PublishedAt: reply.GetPublishedAt(),
		},
		TTL: 0,
	}, nil
}
