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

type SpmMetricBatchGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 同主体多口径 × 连续窗口批量读（回值是稳定序的列表）
func NewSpmMetricBatchGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SpmMetricBatchGetLogic {
	return &SpmMetricBatchGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SpmMetricBatchGet 转发 spm BatchGetMetrics（一个主体、一组口径、连续若干窗口）。
//
// keys 原样下传：不去重、不排序、不裁剪。上限（50 个口径、window_count 1..30）由服务判——
// 悄悄丢掉一个键，后台看到的就是「这个口径没数据」，那是最坏的一类误读。
// 响应是 map（key 里带窗口边界），这里摊平成按 (metric_key, version, window_start) 升序的列表：
// map 遍历序随机，不排序会让同一页两次刷新顺序不同。缺数据的窗口服务本来就不回，
// 网关不补齐、也不写 0。
func (l *SpmMetricBatchGetLogic) SpmMetricBatchGet(req *types.ParamSpmMetricBatchGet) (resp *types.SpmMetricBatchGetResponse, err error) {
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
	if err := spmPositive("window_type", req.WindowType); err != nil {
		return nil, err
	}
	if err := spmNonNeg("window_start_from", req.WindowStartFrom); err != nil {
		return nil, err
	}
	if err := spmNonNeg("window_count", int64(req.WindowCount)); err != nil {
		return nil, err
	}
	keys := make([]*spmrpc.BatchGetMetricsReq_Key, 0, len(req.Keys))
	for _, k := range req.Keys {
		if err := requireNonEmpty("keys.metric_key", k.MetricKey); err != nil {
			return nil, err
		}
		if err := spmNonNeg("keys.metric_version", int64(k.MetricVersion)); err != nil {
			return nil, err
		}
		keys = append(keys, &spmrpc.BatchGetMetricsReq_Key{
			MetricKey:     k.MetricKey,
			MetricVersion: k.MetricVersion,
		})
	}
	reply, err := l.svcCtx.Spm.BatchGetMetrics(l.ctx, &spmrpc.BatchGetMetricsReq{
		SubjectType:     spmrpc.SubjectType(req.SubjectType),
		SubjectId:       req.SubjectId,
		Keys:            keys,
		WindowType:      spmrpc.WindowType(req.WindowType),
		WindowStartFrom: req.WindowStartFrom,
		WindowCount:     req.WindowCount,
	})
	if err != nil {
		l.Errorf("gateway/admin/spmMetricBatchGet: subject_type=%d subject_id=%d keys=%d window_type=%d window_start_from=%d window_count=%d err=%v",
			req.SubjectType, req.SubjectId, len(req.Keys), req.WindowType, req.WindowStartFrom, req.WindowCount, err)
		return nil, err
	}
	return &types.SpmMetricBatchGetResponse{
		Code:    0,
		Message: "ok",
		Data:    types.SpmMetricBatchGetData{Points: spmMetricPointsFromMap(reply.GetPoints())},
		TTL:     0,
	}, nil
}
