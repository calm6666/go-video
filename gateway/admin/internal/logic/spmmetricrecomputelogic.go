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

type SpmMetricRecomputeLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 指标漂移修复重算（从事实表按指定口径版本重算，唯一正当的「改指标」路径）
func NewSpmMetricRecomputeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SpmMetricRecomputeLogic {
	return &SpmMetricRecomputeLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SpmMetricRecompute 转发 spm RecomputeMetrics（从行为事实表按指定口径版本重算窗口）。
//
// 与 SubmitAggregationJob 的关键差别：这里 metric_version 要**正数**，不是 0 哨兵。
// proto 把 4 位都标成必填（subject_type/subject_id/metric_key/metric_version/window_type），
// 其中 metric_version 注释写着「显式版本，避免悄悄按新版本改写历史」——0 在别处是
// 「当前 ACTIVE 版本」，在这条路径上会拿此刻的 ACTIVE 版本去覆盖历史窗口的解释，
// 隔几天再点一次就是另一个结论，所以这里挡下并点名字段。
//
// 同理 subject_id 必填 > 0：重算没有「全部主体」档（那是 SubmitAggregationJob 的批量回填语义），
// 传 0 只会指向一个不存在的主体。
//
// 作业是否受理、窗口区间多大、事实表还在不在 retention 期内，全部由服务判定；
// windows_planned 是服务给出的计划窗口数，原样回，网关不按区间自行估算。
//
// 契约缺口（已上报）：RecomputeMetricsReq 没有 reason 位（口径上下架与作业提交都有），
// 所以「为什么重算这段」在 spm 侧留不下落，只能靠网关访问日志与作业行本身；
// 网关因此**不**为它编造 reason，也不把 trace_id 塞进别的字段（request_id 是幂等键）。
func (l *SpmMetricRecomputeLogic) SpmMetricRecompute(req *types.ParamSpmMetricRecompute) (resp *types.SpmMetricRecomputeResponse, err error) {
	if l.svcCtx.Spm == nil {
		return nil, errSpmServiceNotConfigured
	}
	if req == nil {
		return nil, errSpmRequestMissing
	}
	operator, err := spmOperator(l.ctx, "spmMetricRecompute")
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("metric_key", req.MetricKey); err != nil {
		return nil, err
	}
	if err := spmPositive("subject_type", req.SubjectType); err != nil {
		return nil, err
	}
	if err := spmPositiveID("subject_id", req.SubjectId); err != nil {
		return nil, err
	}
	if err := spmPositive("metric_version", req.MetricVersion); err != nil {
		return nil, err
	}
	if err := spmPositive("window_type", req.WindowType); err != nil {
		return nil, err
	}
	if err := spmWindowRange(req.WindowStartFrom, req.WindowStartTo); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Spm.RecomputeMetrics(l.ctx, &spmrpc.RecomputeMetricsReq{
		SubjectType:     spmrpc.SubjectType(req.SubjectType),
		SubjectId:       req.SubjectId,
		MetricKey:       req.MetricKey,
		MetricVersion:   req.MetricVersion,
		WindowType:      spmrpc.WindowType(req.WindowType),
		WindowStartFrom: req.WindowStartFrom,
		WindowStartTo:   req.WindowStartTo,
		RequestId:       req.IdempotencyKey,
		Operator:        operator,
	})
	if err != nil {
		l.Errorf("gateway/admin/spmMetricRecompute: subject_type=%d subject_id=%d metric_key=%s metric_version=%d window_type=%d operator=%s trace_id=%s err=%v",
			req.SubjectType, req.SubjectId, req.MetricKey, req.MetricVersion, req.WindowType, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/spmMetricRecompute: job_id=%d reused=%t windows_planned=%d operator=%s",
		reply.GetJobId(), reply.GetReused(), reply.GetWindowsPlanned(), operator)
	return &types.SpmMetricRecomputeResponse{
		Code:    0,
		Message: "ok",
		Data: types.SpmMetricRecomputeData{
			JobId:          reply.GetJobId(),
			Reused:         reply.GetReused(),
			WindowsPlanned: reply.GetWindowsPlanned(),
		},
		TTL: 0,
	}, nil
}
