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

type ListPoolVersionsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 池版本台账（含生成批次与产出方，回滚可行性检查带 include_retired）
func NewListPoolVersionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListPoolVersionsLogic {
	return &ListPoolVersionsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ListPoolVersions 转发 recommend-recall ListPoolVersions。
// limit 只做非负门槛：0 = 服务默认，上限 MaxVersionList 由服务夹取。
// include_retired 原样透传——排查「能不能回滚到那一版」时必须看得见 RETIRED 行，
// 但哪些版本真的可回滚是服务的判定（网关不按 state 自己过滤一遍，那会出现两套口径）。
// current_version=0 是「这个池没有 CURRENT、在线不出数」的显式结论，不回写成错误。
func (l *ListPoolVersionsLogic) ListPoolVersions(req *types.ParamRecommendPoolVersionList) (resp *types.RecommendPoolVersionListResponse, err error) {
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
	if err := recommendNonNeg("limit", int64(req.Limit)); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.RecommendRecall.ListPoolVersions(l.ctx, &recallrpc.ListPoolVersionsReq{
		Pool:           pool,
		Limit:          req.Limit,
		IncludeRetired: req.IncludeRetired,
	})
	if err != nil {
		l.Errorf("gateway/admin/listPoolVersions: source=%d pool_key=%s limit=%d include_retired=%t err=%v",
			req.Source, req.PoolKey, req.Limit, req.IncludeRetired, err)
		return nil, err
	}
	return &types.RecommendPoolVersionListResponse{
		Code:    0,
		Message: "ok",
		Data: types.RecommendPoolVersionListData{
			List:           recallPoolVersionsToAPI(reply.GetVersions()),
			CurrentVersion: reply.GetCurrentVersion(),
		},
		TTL: 0,
	}, nil
}
