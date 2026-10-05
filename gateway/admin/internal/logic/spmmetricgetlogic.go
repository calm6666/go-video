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

type SpmMetricGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单窗口指标读（found=false 表示该口径无此窗口，不伪造 0）
func NewSpmMetricGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SpmMetricGetLogic {
	return &SpmMetricGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SpmMetricGet 转发 spm GetMetric（读一个「主体 × 口径版本 × 窗口」的取值）。
//
// metric_version=0（当前 ACTIVE 版本）与 window_start=0（最近一个已闭合窗口）是契约里的
// 合法哨兵，网关不把它们换成具体值——换了就等于去读一个可能没人算过的窗口。
// 「这个口径在这个窗口到底有没有数」是服务的结论：found=false 原样回，不折叠成 value=0，
// 否则「没人看」和「还没算」在后台看起来是同一件事。
func (l *SpmMetricGetLogic) SpmMetricGet(req *types.ParamSpmMetricGet) (resp *types.SpmMetricGetResponse, err error) {
	if l.svcCtx.Spm == nil {
		return nil, errSpmServiceNotConfigured
	}
	if req == nil {
		return nil, errSpmRequestMissing
	}
	if err := spmPositive("subject_type", req.SubjectType); err != nil {
		return nil, err
	}
	if err := spmPositiveID("subject_id", req.SubjectId); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("metric_key", req.MetricKey); err != nil {
		return nil, err
	}
	if err := spmPositive("window_type", req.WindowType); err != nil {
		return nil, err
	}
	if err := spmNonNeg("metric_version", int64(req.MetricVersion)); err != nil {
		return nil, err
	}
	if err := spmNonNeg("window_start", req.WindowStart); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Spm.GetMetric(l.ctx, &spmrpc.GetMetricReq{
		SubjectType:   spmrpc.SubjectType(req.SubjectType),
		SubjectId:     req.SubjectId,
		MetricKey:     req.MetricKey,
		MetricVersion: req.MetricVersion,
		WindowType:    spmrpc.WindowType(req.WindowType),
		WindowStart:   req.WindowStart,
	})
	if err != nil {
		// 只记寻址位，不记值本身（值可能带业务敏感性，且日志不是取数通道）。
		l.Errorf("gateway/admin/spmMetricGet: subject_type=%d subject_id=%d metric_key=%s metric_version=%d window_type=%d window_start=%d err=%v",
			req.SubjectType, req.SubjectId, req.MetricKey, req.MetricVersion, req.WindowType, req.WindowStart, err)
		return nil, err
	}
	return &types.SpmMetricGetResponse{
		Code:    0,
		Message: "ok",
		Data: types.SpmMetricGetData{
			Found: reply.GetFound(),
			Point: spmMetricPointToAPI(reply.GetPoint()),
		},
		TTL: 0,
	}, nil
}
