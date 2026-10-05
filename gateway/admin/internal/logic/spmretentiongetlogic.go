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

type SpmRetentionGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// cohort 留存曲线（注册日/首播日分桶）
func NewSpmRetentionGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SpmRetentionGetLogic {
	return &SpmRetentionGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SpmRetentionGet 转发 spm GetRetention（cohort 留存曲线）。
//
// cohort_date 按天规整、max_day 的上限（1..90）与「这一天的 cohort 是否已经算完」都是服务的结论：
// 超上限一律原样下传让它拒，网关不夹取——夹取会让后台以为曲线本来就只有 90 天。
// rate 是服务算好的 retained/cohort_size，网关不重算比值（两侧口径不同就会造出一条假曲线）。
// cohort_date 回显同样以服务回值为准（传入的秒级时间戳会被规整到当天边界）。
func (l *SpmRetentionGetLogic) SpmRetentionGet(req *types.ParamSpmRetentionGet) (resp *types.SpmRetentionGetResponse, err error) {
	if l.svcCtx.Spm == nil {
		return nil, errSpmServiceNotConfigured
	}
	if req == nil {
		return nil, errSpmRequestMissing
	}
	if err := spmPositive("cohort_type", req.CohortType); err != nil {
		return nil, err
	}
	if err := spmNonNeg("cohort_date", req.CohortDate); err != nil {
		return nil, err
	}
	if err := spmNonNeg("max_day", int64(req.MaxDay)); err != nil {
		return nil, err
	}
	if err := spmNonNeg("metric_version", int64(req.MetricVersion)); err != nil {
		return nil, err
	}
	if err := spmNonNeg("zone_id", req.ZoneId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Spm.GetRetention(l.ctx, &spmrpc.GetRetentionReq{
		CohortType:    spmrpc.GetRetentionReq_CohortType(req.CohortType),
		CohortDate:    req.CohortDate,
		MaxDay:        req.MaxDay,
		MetricVersion: req.MetricVersion,
		ZoneId:        req.ZoneId,
	})
	if err != nil {
		l.Errorf("gateway/admin/spmRetentionGet: cohort_type=%d cohort_date=%d max_day=%d metric_version=%d zone_id=%d err=%v",
			req.CohortType, req.CohortDate, req.MaxDay, req.MetricVersion, req.ZoneId, err)
		return nil, err
	}
	return &types.SpmRetentionGetResponse{
		Code:    0,
		Message: "ok",
		Data: types.SpmRetentionGetData{
			Points:        spmRetentionPointsToAPI(reply.GetPoints()),
			MetricVersion: reply.GetMetricVersion(),
			CohortDate:    reply.GetCohortDate(),
		},
		TTL: 0,
	}, nil
}
