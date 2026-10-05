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

type GetRankDecisionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 排序决策回放（按 decision_id 或 request_id）
func NewGetRankDecisionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRankDecisionLogic {
	return &GetRankDecisionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// GetRankDecision 转发 recommend-rank GetRankDecision。
// decision_id 与 request_id 至少给一个；两个都给时谁优先由服务判定，网关不猜也不清洗。
// found 表达「服务有没有回这一条」（契约：entry 不存在时为 null），
// 未命中不返回全零值冒充命中——那会让后台以为「这次排序真的 0 输入 0 输出」。
// top_aids/result_digest 是核对「有没有凭空产生 aid」的证据列，必须回传；
// subject_id（mid 字符串或设备 sha256 摘要）只在响应体里出现，网关日志一律不打（AGENTS.md §7）。
func (l *GetRankDecisionLogic) GetRankDecision(req *types.ParamRankDecisionGet) (resp *types.RankDecisionResponse, err error) {
	if l.svcCtx.RecommendRank == nil {
		return nil, errRankServiceNotConfigured
	}
	if req == nil {
		return nil, errRecommendRequestMissing
	}
	if !recommendAtLeastOne(req.DecisionId, req.RequestId) {
		return nil, errRankDecisionSubjectRequired
	}
	reply, err := l.svcCtx.RecommendRank.GetRankDecision(l.ctx, &rankrpc.GetRankDecisionReq{
		DecisionId: req.DecisionId,
		RequestId:  req.RequestId,
	})
	if err != nil {
		// 只打主键：decision_id/request_id 是随机/受控 ID，不是用户资料。
		l.Errorf("gateway/admin/getRankDecision: decision_id=%s request_id=%s err=%v",
			req.DecisionId, req.RequestId, err)
		return nil, err
	}
	entry := reply.GetEntry()
	return &types.RankDecisionResponse{
		Code:    0,
		Message: "ok",
		Data: types.RankDecisionData{
			Entry: rankDecisionToAPI(entry),
			Found: entry != nil,
		},
		TTL: 0,
	}, nil
}
