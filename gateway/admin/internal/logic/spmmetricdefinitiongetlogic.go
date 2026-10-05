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

type SpmMetricDefinitionGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单口径读（metric_version=0 = 当前 ACTIVE 版本）
func NewSpmMetricDefinitionGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SpmMetricDefinitionGetLogic {
	return &SpmMetricDefinitionGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SpmMetricDefinitionGet 转发 spm GetMetricDefinition（读一条口径定义）。
//
// metric_version=0 是「给我当前 ACTIVE 版本」的合法哨兵，网关不先查一遍列表来替它挑一个版本号
// （那会把两次读之间的上下架吞进一次「看起来一致」的快照）。found=false 原样回：
// 「这个口径还没登记」与「登记了但没 ACTIVE 版本」都由服务说，网关不翻译成 404 也不造零值行。
func (l *SpmMetricDefinitionGetLogic) SpmMetricDefinitionGet(req *types.ParamSpmMetricDefinitionGet) (resp *types.SpmMetricDefinitionGetResponse, err error) {
	if l.svcCtx.Spm == nil {
		return nil, errSpmServiceNotConfigured
	}
	if req == nil {
		return nil, errSpmRequestMissing
	}
	if err := requireNonEmpty("metric_key", req.MetricKey); err != nil {
		return nil, err
	}
	if err := spmNonNeg("metric_version", int64(req.MetricVersion)); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Spm.GetMetricDefinition(l.ctx, &spmrpc.GetMetricDefinitionReq{
		MetricKey:     req.MetricKey,
		MetricVersion: req.MetricVersion,
	})
	if err != nil {
		l.Errorf("gateway/admin/spmMetricDefinitionGet: metric_key=%s metric_version=%d err=%v",
			req.MetricKey, req.MetricVersion, err)
		return nil, err
	}
	return &types.SpmMetricDefinitionGetResponse{
		Code:    0,
		Message: "ok",
		Data: types.SpmMetricDefinitionGetData{
			Found:      reply.GetFound(),
			Definition: spmDefinitionToAPI(reply.GetDefinition()),
		},
		TTL: 0,
	}, nil
}
