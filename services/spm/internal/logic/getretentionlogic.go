// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"fmt"

	"go-video/services/spm/internal/svc"
	"go-video/services/spm/model"
	"go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetRetentionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetRetentionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRetentionLogic {
	return &GetRetentionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 留存曲线
func (l *GetRetentionLogic) GetRetention(in *rpc.GetRetentionReq) (*rpc.GetRetentionReply, error) {
	// 逻辑轮规划：把 cohort_date 规整到天边界、max_day 收敛到 1..90 -> 读 spm_retention_cohort（uniq cohort_type+cohort_date+zone_id+day_offset+metric_version）-> rate 由 retained/cohort_size 现算，不返回历史落库的浮点值，避免回填后口径漂移。
	done, err := acquireReadToken(l.ctx, l.svcCtx, l.Logger, "GetRetention")
	if err != nil {
		return nil, err
	}
	defer done()

	cohortType := int32(in.GetCohortType())
	if !model.ValidCohortType(cohortType) {
		return nil, fmt.Errorf("%w: cohort_type=%d", model.ErrInvalidCohort, cohortType)
	}
	if in.GetCohortDate() <= 0 {
		return nil, fmt.Errorf("%w: cohort_date 必填", model.ErrInvalidCohort)
	}
	// 天边界由 model 与 logic 共用同一个函数规整：两侧各算一遍就会在不同时区的进程上
	// 落到不同的桶，响应回显的 cohort_date 也就不能原样传回来复查。
	cohortDate := model.DayStartUnix(in.GetCohortDate())
	if in.GetZoneId() < 0 {
		return nil, fmt.Errorf("%w: zone_id=%d", model.ErrInvalidCohort, in.GetZoneId())
	}
	cfg := l.svcCtx.Config.Spm
	maxDay := in.GetMaxDay()
	if maxDay <= 0 {
		maxDay = cfg.MaxRetentionDay
	}
	if maxDay > cfg.MaxRetentionDay {
		return nil, fmt.Errorf("%w: max_day=%d > %d", model.ErrMaxDayTooLarge, maxDay,
			cfg.MaxRetentionDay)
	}
	// 与 GetUserInterest 同一条线：spm_retention_cohort 没有 metric_key 列，
	// version=0 没有可解析的 ACTIVE 指针（README「契约缺口」），只能显式给定。
	version := in.GetMetricVersion()
	if version <= 0 {
		return nil, fmt.Errorf("%w: GetRetention 必须显式给定留存口径版本"+
			"（留存表无 metric_key，无法解析 ACTIVE 指针）", model.ErrMetricVersionRequired)
	}

	rows, err := l.svcCtx.Retention.ListCurve(l.ctx, cohortType, cohortDate, in.GetZoneId(),
		version, maxDay)
	if err != nil {
		return nil, err
	}
	points := make([]*rpc.GetRetentionReply_RetentionPoint, 0, len(rows))
	for _, r := range rows {
		// rate 现算而不是照抄落库值：比率列写于当时那次计算，cohort_size 之后被迟到事实
		// 修正过时，只有分子分母是新的，照抄旧比率就得到「人数变了、留存率没变」的曲线。
		rate := 0.0
		if r.CohortSize > 0 {
			rate = float64(r.Retained) / float64(r.CohortSize)
		}
		points = append(points, &rpc.GetRetentionReply_RetentionPoint{
			DayOffset:  r.DayOffset,
			CohortSize: r.CohortSize,
			Retained:   r.Retained,
			Rate:       rate,
		})
	}
	return &rpc.GetRetentionReply{
		Points:        points,
		MetricVersion: version,
		CohortDate:    cohortDate,
	}, nil
}
