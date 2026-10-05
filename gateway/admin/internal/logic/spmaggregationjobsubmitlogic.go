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

type SpmAggregationJobSubmitLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 提交聚合作业（实时/离线回填/重算；reason 说明回填范围或故障单号）
func NewSpmAggregationJobSubmitLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SpmAggregationJobSubmitLogic {
	return &SpmAggregationJobSubmitLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SpmAggregationJobSubmit 转发 spm SubmitAggregationJob（计划并排队一个聚合作业）。
//
// 网关只做形状门槛，四条边界：
//  1. 身份：operator 由会话渲染成 gateway/admin:<admin_id>，表单不自报；
//  2. 枚举位：job_type/window_type 为 0 即 UNSPECIFIED，服务同样拒，这里提前挡住并点名字段；
//  3. 0 值哨兵原样下传，不替调用方挑值：subject_type=0 是「全部主体」、subject_id=0 是「不限主体」、
//     metric_key="" 是「该作业类型下的全部指标」、metric_version=0 是「当前 ACTIVE 版本」、
//     window_start_to=0 是「当前时间」——把它们换成具体值就等于凭空圈定一个没人要的范围；
//  4. 区间：window_start_from>window_start_to 在服务侧圈不出任何窗口，回的是空作业加一次无谓扫描，
//     所以挡住；区间是否过大、该不该受理这类回填，全由服务判（网关不复制窗口上限）。
//
// reason 在契约里可选（口径上下架那条是必填，两条不同口径照实处理），
// 空串原样下传，不由网关替运营编一句「后台手工触发」——编出来的理由没有证据价值。
// reused 与作业全字段原样回：reused=true 是幂等重放的正常结论，既不折叠成错误，
// 也不假装新排了一个作业（那会让后台以为跑两遍）。
func (l *SpmAggregationJobSubmitLogic) SpmAggregationJobSubmit(req *types.ParamSpmAggregationJobSubmit) (resp *types.SpmAggregationJobSubmitResponse, err error) {
	if l.svcCtx.Spm == nil {
		return nil, errSpmServiceNotConfigured
	}
	if req == nil {
		return nil, errSpmRequestMissing
	}
	operator, err := spmOperator(l.ctx, "spmAggregationJobSubmit")
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := spmPositive("job_type", req.JobType); err != nil {
		return nil, err
	}
	if err := spmPositive("window_type", req.WindowType); err != nil {
		return nil, err
	}
	if err := spmNonNeg("subject_type", int64(req.SubjectType)); err != nil {
		return nil, err
	}
	if err := spmNonNeg("subject_id", req.SubjectId); err != nil {
		return nil, err
	}
	if err := spmNonNeg("metric_version", int64(req.MetricVersion)); err != nil {
		return nil, err
	}
	if err := spmWindowRange(req.WindowStartFrom, req.WindowStartTo); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Spm.SubmitAggregationJob(l.ctx, &spmrpc.SubmitAggregationJobReq{
		JobType:         spmrpc.JobType(req.JobType),
		SubjectType:     spmrpc.SubjectType(req.SubjectType),
		SubjectId:       req.SubjectId,
		MetricKey:       req.MetricKey,
		MetricVersion:   req.MetricVersion,
		WindowType:      spmrpc.WindowType(req.WindowType),
		WindowStartFrom: req.WindowStartFrom,
		WindowStartTo:   req.WindowStartTo,
		RequestId:       req.IdempotencyKey,
		Operator:        operator,
		Reason:          req.Reason,
	})
	if err != nil {
		// trace_id 只进日志（SubmitAggregationJobReq 没有该字段可下传）；reason 正文不落日志。
		l.Errorf("gateway/admin/spmAggregationJobSubmit: job_type=%d subject_type=%d subject_id=%d metric_key=%s window_type=%d operator=%s trace_id=%s err=%v",
			req.JobType, req.SubjectType, req.SubjectId, req.MetricKey, req.WindowType, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/spmAggregationJobSubmit: job_id=%d reused=%t windows_total=%d operator=%s",
		reply.GetJobId(), reply.GetReused(), reply.GetJob().GetWindowsTotal(), operator)
	return &types.SpmAggregationJobSubmitResponse{
		Code:    0,
		Message: "ok",
		Data: types.SpmAggregationJobSubmitData{
			JobId:  reply.GetJobId(),
			Reused: reply.GetReused(),
			Job:    spmJobToAPI(reply.GetJob()),
		},
		TTL: 0,
	}, nil
}
