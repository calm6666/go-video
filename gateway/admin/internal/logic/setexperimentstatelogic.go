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

type SetExperimentStateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 实验状态迁移（RUNNING/PAUSED/STOPPED，与模型状态分属不同权限点）
func NewSetExperimentStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetExperimentStateLogic {
	return &SetExperimentStateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SetExperimentState 转发 recommend-rank SetExperimentState。
// 与 /rank/model/state 分属两个权限点：暂停保持已分桶（只停新增分流），结束是终态，
// 影响面与可逆性都不同，不该由同一个权限点覆盖。
// 目标状态取值（RUNNING/PAUSED/STOPPED）与迁移是否合法（例如 STOPPED 不能再回 RUNNING）
// 由服务判定；changed=false 表示已达目标态，是正常结论而不是失败。
// operator 由会话渲染，reason 与 idempotency_key 必填并原样透传。
func (l *SetExperimentStateLogic) SetExperimentState(req *types.ParamRankExperimentStateSet) (resp *types.RankExperimentStateSetResponse, err error) {
	if l.svcCtx.RecommendRank == nil {
		return nil, errRankServiceNotConfigured
	}
	if req == nil {
		return nil, errRecommendRequestMissing
	}
	operator, err := recommendOperator(l.ctx, "setExperimentState")
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("exp_key", req.ExpKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("variant_key", req.VariantKey); err != nil {
		return nil, err
	}
	if err := recommendPositive("target_state", req.TargetState); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.RecommendRank.SetExperimentState(l.ctx, &rankrpc.SetExperimentStateReq{
		ExpKey:         req.ExpKey,
		VariantKey:     req.VariantKey,
		TargetState:    rankrpc.ExperimentState(req.TargetState),
		Operator:       operator,
		Reason:         req.Reason,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		l.Errorf("gateway/admin/setExperimentState: exp_key=%s variant_key=%s target_state=%d operator=%s idempotency_key=%s err=%v",
			req.ExpKey, req.VariantKey, req.TargetState, operator, req.IdempotencyKey, err)
		return nil, err
	}
	return &types.RankExperimentStateSetResponse{
		Code:    0,
		Message: "ok",
		Data: types.RankExperimentStateSetData{
			Changed:      reply.GetChanged(),
			State:        int32(reply.GetState()),
			Deduplicated: reply.GetDeduplicated(),
		},
		TTL: 0,
	}, nil
}
