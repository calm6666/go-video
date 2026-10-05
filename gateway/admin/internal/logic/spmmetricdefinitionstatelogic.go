// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	spmrpc "go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SpmMetricDefinitionStateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 口径上下架（DRAFT/ACTIVE/RETIRED；retire 后不再写入但历史窗口仍可解释）
func NewSpmMetricDefinitionStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SpmMetricDefinitionStateLogic {
	return &SpmMetricDefinitionStateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SpmMetricDefinitionState 转发 spm UpdateMetricDefinitionState（口径状态迁移）。
//
// metric_version 与 state 都要求正数，但理由不同，都不是复算领域规则：
//   - version=0 在「读」里是「当前 ACTIVE 版本」的哨兵，在状态迁移里没有任何对应对象
//     （迁移必须指向确切版本，猜一个等于替调用方挑一档来上下架），所以这里挡下；
//   - state=0 = UNSPECIFIED 不是一个目标状态，服务同样拒；DRAFT/ACTIVE/RETIRED 之间
//     能否迁移、激活一个版本会不会顶掉另一个 ACTIVE，全部由服务判定，网关不复算状态机。
//
// reason 必填是 proto 的无条件要求（无理由不受理），网关提前挡住空串并点名字段。
// 幂等键原样进 request_id；重放回 reused=true 而不是第二次迁移。
func (l *SpmMetricDefinitionStateLogic) SpmMetricDefinitionState(req *types.ParamSpmMetricDefinitionState) (resp *types.SpmMetricDefinitionStateResponse, err error) {
	if l.svcCtx.Spm == nil {
		return nil, errSpmServiceNotConfigured
	}
	if req == nil {
		return nil, errSpmRequestMissing
	}
	operator, err := spmOperator(l.ctx, "spmMetricDefinitionState")
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("metric_key", req.MetricKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := spmPositive("metric_version", req.MetricVersion); err != nil {
		return nil, err
	}
	if err := spmPositive("state", req.State); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Spm.UpdateMetricDefinitionState(l.ctx, &spmrpc.UpdateMetricDefinitionStateReq{
		MetricKey:     req.MetricKey,
		MetricVersion: req.MetricVersion,
		State:         spmrpc.DefinitionState(req.State),
		Operator:      operator,
		Reason:        req.Reason,
		RequestId:     req.IdempotencyKey,
	})
	if err != nil {
		l.Errorf("gateway/admin/spmMetricDefinitionState: metric_key=%s metric_version=%d state=%d operator=%s trace_id=%s err=%v",
			req.MetricKey, req.MetricVersion, req.State, operator, req.TraceId, err)
		return nil, err
	}
	// reason 正文不进日志（§7 只留结论与主体）。
	l.Infof("gateway/admin/spmMetricDefinitionState: metric_key=%s metric_version=%d state=%d reused=%t operator=%s",
		req.MetricKey, req.MetricVersion, req.State, reply.GetReused(), operator)
	return &types.SpmMetricDefinitionStateResponse{
		Code:    0,
		Message: "ok",
		Data: types.SpmMetricDefinitionStateData{
			Definition: spmDefinitionToAPI(reply.GetDefinition()),
			Reused:     reply.GetReused(),
		},
		TTL: 0,
	}, nil
}
