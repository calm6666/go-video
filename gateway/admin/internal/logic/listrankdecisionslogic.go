// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	rankrpc "go-video/services/recommend-rank/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRankDecisionsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 排序决策摘要分页（实验/模型/场景过滤，可只看降级）
func NewListRankDecisionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRankDecisionsLogic {
	return &ListRankDecisionsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ListRankDecisions 转发 recommend-rank ListRankDecisions。
// 全部过滤位都是「空/0 = 不过滤」的合法哨兵，网关不填默认值：
// 一旦网关把 exp_key 空串兜成某个实验，后台看到的样本就不是它要的样本了。
// only_degraded 是灰度期最常按的开关（只看降级的那批），原样透传，
// 服务是否因此走索引、页大小上限 MaxDecisionPage 如何夹取都不归网关管。
// 结果为空投影成 []，与「rank 没接」（错误）严格区分。
func (l *ListRankDecisionsLogic) ListRankDecisions(req *types.ParamRankDecisionList) (resp *types.RankDecisionListResponse, err error) {
	if l.svcCtx.RecommendRank == nil {
		return nil, errRankServiceNotConfigured
	}
	if req == nil {
		return nil, errRecommendRequestMissing
	}
	if err := recommendTimeWindow(req.FromTime, req.ToTime); err != nil {
		return nil, err
	}
	if err := recommendPaging(req.Pn, req.Ps); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.RecommendRank.ListRankDecisions(l.ctx, &rankrpc.ListRankDecisionsReq{
		ExpKey:       req.ExpKey,
		VariantKey:   req.VariantKey,
		ModelKey:     req.ModelKey,
		ModelVersion: req.ModelVersion,
		Scene:        req.Scene,
		FromTime:     req.FromTime,
		ToTime:       req.ToTime,
		OnlyDegraded: req.OnlyDegraded,
		Pn:           req.Pn,
		Ps:           req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listRankDecisions: exp_key=%s model_key=%s pn=%d err=%v",
			req.ExpKey, req.ModelKey, req.Pn, err)
		return nil, err
	}
	return &types.RankDecisionListResponse{
		Code:    0,
		Message: "ok",
		Data: types.RankDecisionListData{
			List:    rankDecisionsToAPI(reply.GetEntries()),
			HasMore: reply.GetHasMore(),
		},
		TTL: 0,
	}, nil
}
