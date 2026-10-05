// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/services/spm/internal/svc"
	"go-video/services/spm/model"
	"go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SubmitAggregationJobLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSubmitAggregationJobLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitAggregationJobLogic {
	return &SubmitAggregationJobLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 提交实时/离线聚合作业（request_id 幂等）
func (l *SubmitAggregationJobLogic) SubmitAggregationJob(in *rpc.SubmitAggregationJobReq) (*rpc.SubmitAggregationJobReply, error) {
	// 逻辑轮规划：规整窗口区间并计算 windows_total（上限 Spm.MaxWindowsPerJob，超限拒绝而不是拆半执行）-> 落 spm_aggregation_job（uniq_request_id 幂等，命中返回首次 job）-> REALTIME 作业由本服务 ticker 认领，OFFLINE_BACKFILL 由 services/cron 认领，认领方通过 model 的 ClaimPending/RenewLease/UpdateProgress/MarkFinished 推进状态（租约 JobLeaseSeconds，超过 JobMaxRetry 置 FAILED 终态）。
	done, err := acquireWriteToken(l.ctx, l.svcCtx, l.Logger, "SubmitAggregationJob")
	if err != nil {
		return nil, err
	}
	defer done()

	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	if err := checkOperator(in.GetOperator()); err != nil {
		return nil, err
	}
	// 作业是「改写一段历史窗口」的授权书，reason 是它唯一的说明文本（README「作业与租约语义」）。
	if err := checkReason(in.GetReason()); err != nil {
		return nil, err
	}
	jobType := int32(in.GetJobType())
	if !model.ValidJobType(jobType) {
		return nil, fmt.Errorf("%w: job_type=%d", model.ErrInvalidJobType, jobType)
	}
	if jobType == model.JobTypeRecompute {
		// 修复作业只由 RecomputeMetrics 派生：那条入口强制显式口径版本，
		// 而本接口的 metric_version=0 表示「当前 ACTIVE」，用它排队重算等价于
		// 悄悄用新口径改写历史窗口。
		return nil, fmt.Errorf("%w: 重算作业请走 RecomputeMetrics（必须显式给定口径版本）",
			model.ErrInvalidJobType)
	}
	subjectType, err := checkSubjectAllowUnscoped(in.GetSubjectType(), in.GetSubjectId())
	if err != nil {
		return nil, err
	}
	windowType, err := checkWindowType(in.GetWindowType(), nil)
	if err != nil {
		return nil, err
	}
	if jobType == model.JobTypeRealtime && windowType != model.WindowType5Min &&
		windowType != model.WindowTypeHour {
		// 实时作业只有 5 分钟/小时两档（config.RealtimeWindowType 的取值域同源）：
		// day/week/total 没有「闭合时机」，排队了也只会被 ticker 反复跳过。
		return nil, fmt.Errorf("%w: 实时作业只支持 5 分钟/小时窗口，收到 %d",
			model.ErrInvalidWindow, windowType)
	}

	metricKey, err := checkMetricKey(in.GetMetricKey())
	if errors.Is(err, model.ErrMetricKeyEmpty) && jobType == model.JobTypeRealtime {
		// REALTIME 允许不指定口径（= 本轮闭合窗口要推进的全部 ACTIVE 口径），
		// 空键在这里不是错误；超长等其它入参错误仍然照实返回。
		metricKey, err = "", nil
	}
	if err != nil {
		return nil, err
	}

	var version int32
	if metricKey != "" {
		def, err := resolveDefinition(l.ctx, l.svcCtx, metricKey, in.GetMetricVersion())
		if err != nil {
			return nil, err
		}
		if !definitionSupportsWindow(def, windowType) {
			return nil, fmt.Errorf("%w: 口径 %s@v%d 未登记粒度 %d（supported_windows=%s）",
				model.ErrInvalidWindow, def.MetricKey, def.MetricVersion, windowType,
				def.SupportedWindows)
		}
		if jobType != model.JobTypeRealtime && in.GetMetricVersion() <= 0 {
			// 离线回填会改写已闭合的历史窗口，必须钉住口径版本（与 RecomputeMetrics 同一条线）。
			return nil, fmt.Errorf("%w: %s 作业回填历史窗口，必须显式给定口径版本",
				model.ErrMetricVersionRequired, metricKey)
		}
		version = def.MetricVersion
		metricKey = def.MetricKey
	} else if in.GetMetricVersion() > 0 {
		return nil, fmt.Errorf("%w: 未指定 metric_key 时不能给定口径版本", model.ErrMetricKeyEmpty)
	} else if jobType != model.JobTypeRealtime {
		// 走到这里说明是 OFFLINE_BACKFILL 且没给 metric_key。
		return nil, fmt.Errorf("%w: 离线回填作业必须指定 metric_key（回填按口径逐个钉版本）",
			model.ErrMetricKeyEmpty)
	}

	from, to, total, err := checkJobWindowRange(l.svcCtx, in.GetWindowStartFrom(),
		in.GetWindowStartTo(), windowType, l.svcCtx.Config.Spm.MaxWindowsPerJob)
	if err != nil {
		return nil, err
	}

	job, reused, err := submitJob(l.ctx, l.svcCtx, l.Logger, &model.AggregationJob{
		JobType:         jobType,
		State:           model.JobStatePending,
		SubjectType:     subjectType,
		SubjectID:       in.GetSubjectId(),
		MetricKey:       metricKey,
		MetricVersion:   version,
		WindowType:      windowType,
		WindowStartFrom: from,
		WindowStartTo:   to,
		WindowsTotal:    total,
		RequestID:       in.GetRequestId(),
		Operator:        in.GetOperator(),
		Reason:          in.GetReason(),
	})
	if err != nil {
		return nil, err
	}
	return &rpc.SubmitAggregationJobReply{
		JobId:  job.ID,
		Reused: reused,
		Job:    jobOf(job),
	}, nil
}
