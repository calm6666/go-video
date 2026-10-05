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

type SpmAggregationJobListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 聚合作业列表（按类型/状态/时间筛）
func NewSpmAggregationJobListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SpmAggregationJobListLogic {
	return &SpmAggregationJobListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SpmAggregationJobList 转发 spm ListAggregationJobs（作业列表）。
//
// job_type/state=0 与 since=0 都是「不限」的合法哨兵，网关不代填时间窗：回填作业经常要跨月看，
// 悄悄加一个「最近 N 天」会让运营以为那批作业没提交过。列表范围是否过大、要不要强制时间边界，
// 由服务判（§5 作业状态机与索引只属于 spm）；ps 上限同样由服务夹取。
func (l *SpmAggregationJobListLogic) SpmAggregationJobList(req *types.ParamSpmAggregationJobList) (resp *types.SpmAggregationJobListResponse, err error) {
	if l.svcCtx.Spm == nil {
		return nil, errSpmServiceNotConfigured
	}
	if req == nil {
		return nil, errSpmRequestMissing
	}
	if err := spmNonNeg("job_type", int64(req.JobType)); err != nil {
		return nil, err
	}
	if err := spmNonNeg("state", int64(req.State)); err != nil {
		return nil, err
	}
	if err := spmNonNeg("since", req.Since); err != nil {
		return nil, err
	}
	if err := spmPaging(req.Pn, req.Ps); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Spm.ListAggregationJobs(l.ctx, &spmrpc.ListAggregationJobsReq{
		JobType: spmrpc.JobType(req.JobType),
		State:   spmrpc.JobState(req.State),
		Since:   req.Since,
		Pn:      req.Pn,
		Ps:      req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/spmAggregationJobList: job_type=%d state=%d since=%d pn=%d ps=%d err=%v",
			req.JobType, req.State, req.Since, req.Pn, req.Ps, err)
		return nil, err
	}
	return &types.SpmAggregationJobListResponse{
		Code:    0,
		Message: "ok",
		Data: types.SpmAggregationJobListData{
			Jobs:  spmJobsToAPI(reply.GetJobs()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
