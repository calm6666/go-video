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

type RecomputeMetricsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRecomputeMetricsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RecomputeMetricsLogic {
	return &RecomputeMetricsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 从事实表重算指标（计数/指标漂移的修复入口，派生 JOB_TYPE_RECOMPUTE 作业）
func (l *RecomputeMetricsLogic) RecomputeMetrics(in *rpc.RecomputeMetricsReq) (*rpc.RecomputeMetricsReply, error) {
	// 逻辑轮规划：校验窗口区间并要求 metric_version 显式给定（禁止用 ACTIVE 版本悄悄改写历史）-> 派生 JOB_TYPE_RECOMPUTE 作业（uniq request_id 幂等）后立刻返回 job_id -> 作业执行器从 spm_behavior_event 事实表按该口径版本重算，再以 source=RECOMPUTE 走 WriteMetricWindow。本方法不落指标、不阻塞请求。
	done, err := acquireWriteToken(l.ctx, l.svcCtx, l.Logger, "RecomputeMetrics")
	if err != nil {
		return nil, err
	}
	defer done()

	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	// 重算会改写已闭合窗口的对外数值，operator 是这条链路唯一的追责入口。
	if err := checkOperator(in.GetOperator()); err != nil {
		return nil, err
	}
	subjectType, err := checkSubject(in.GetSubjectType(), in.GetSubjectId())
	if err != nil {
		return nil, err
	}
	metricKey, err := checkMetricKey(in.GetMetricKey())
	if err != nil {
		return nil, err
	}
	if in.GetMetricVersion() <= 0 {
		// 重算不接受 ACTIVE 指针：口径一旦换版，「按当前 ACTIVE 改写历史窗口」会让
		// 同一个窗口在两次重算后落到不同口径上，事后没人能解释这批数据是哪版算的。
		return nil, fmt.Errorf("%w: 重算必须显式给定口径版本", model.ErrMetricVersionRequired)
	}
	def, err := resolveDefinition(l.ctx, l.svcCtx, metricKey, in.GetMetricVersion())
	if err != nil {
		return nil, err
	}
	windowType, err := checkWindowType(in.GetWindowType(), def)
	if err != nil {
		return nil, err
	}
	from, to, total, err := checkJobWindowRange(l.svcCtx, in.GetWindowStartFrom(),
		in.GetWindowStartTo(), windowType, l.svcCtx.Config.Spm.MaxWindowsPerJob)
	if err != nil {
		return nil, err
	}

	job, reused, err := submitJob(l.ctx, l.svcCtx, l.Logger, &model.AggregationJob{
		JobType:         model.JobTypeRecompute,
		State:           model.JobStatePending,
		SubjectType:     subjectType,
		SubjectID:       in.GetSubjectId(),
		MetricKey:       def.MetricKey,
		MetricVersion:   def.MetricVersion,
		WindowType:      windowType,
		WindowStartFrom: from,
		WindowStartTo:   to,
		WindowsTotal:    total,
		RequestID:       in.GetRequestId(),
		Operator:        in.GetOperator(),
	})
	if err != nil {
		return nil, err
	}
	// 重算进度：新作业用本次算出的跨度，重放作业用库里已落的那份（两者一致，
	// 因为 submitJob 已经比对过区间，不一致的 request_id 复用会被拒）。
	planned := job.WindowsTotal
	if planned == 0 {
		planned = total
	}
	return &rpc.RecomputeMetricsReply{
		JobId:          job.ID,
		Reused:         reused,
		WindowsPlanned: planned,
	}, nil
}
