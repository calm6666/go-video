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

type SetModelVersionStateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 切换模型版本状态（READY/ACTIVE/RETIRED；激活即回滚开关，需 reason）
func NewSetModelVersionStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetModelVersionStateLogic {
	return &SetModelVersionStateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SetModelVersionState 转发 recommend-rank SetModelVersionState。
// 这是推荐链路的回滚开关：ACTIVE 每个 model_key 至多一个，激活新版本即让上一个下线，
// 所以 previous_active_version 必须回传给后台核对「被换掉的是哪一个」。
// 目标状态取值集合（契约只允许 READY/ACTIVE/RETIRED）与迁移合法性由服务判定，
// 网关不复制状态机、不把非法迁移兜成成功；reason 是契约必填的审计项。
// event_id 按契约本期预留（rank README 记录未接 MQ），为空表示尚无事件，网关不伪造。
func (l *SetModelVersionStateLogic) SetModelVersionState(req *types.ParamRankModelStateSet) (resp *types.RankModelStateSetResponse, err error) {
	if l.svcCtx.RecommendRank == nil {
		return nil, errRankServiceNotConfigured
	}
	if req == nil {
		return nil, errRecommendRequestMissing
	}
	operator, err := recommendOperator(l.ctx, "setModelVersionState")
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("model_key", req.ModelKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("version", req.Version); err != nil {
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
	reply, err := l.svcCtx.RecommendRank.SetModelVersionState(l.ctx, &rankrpc.SetModelVersionStateReq{
		ModelKey:       req.ModelKey,
		Version:        req.Version,
		TargetState:    rankrpc.ModelVersionState(req.TargetState),
		Operator:       operator,
		Reason:         req.Reason,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		l.Errorf("gateway/admin/setModelVersionState: model_key=%s version=%s target_state=%d operator=%s idempotency_key=%s err=%v",
			req.ModelKey, req.Version, req.TargetState, operator, req.IdempotencyKey, err)
		return nil, err
	}
	return &types.RankModelStateSetResponse{
		Code:    0,
		Message: "ok",
		Data: types.RankModelStateSetData{
			Changed:               reply.GetChanged(),
			State:                 int32(reply.GetState()),
			PreviousActiveVersion: reply.GetPreviousActiveVersion(),
			Deduplicated:          reply.GetDeduplicated(),
			EventId:               reply.GetEventId(),
		},
		TTL: 0,
	}, nil
}
