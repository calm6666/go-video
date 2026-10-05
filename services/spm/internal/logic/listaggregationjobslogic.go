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

type ListAggregationJobsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListAggregationJobsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListAggregationJobsLogic {
	return &ListAggregationJobsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 作业列表（分页）
func (l *ListAggregationJobsLogic) ListAggregationJobs(in *rpc.ListAggregationJobsReq) (*rpc.ListAggregationJobsReply, error) {
	// 逻辑轮规划：校验 ps<=100 -> 按 job_type/state/since 过滤，ctime 倒序分页；(state, ctime) 与 (job_type, ctime) 索引已在迁移里就位。
	done, err := acquireReadToken(l.ctx, l.svcCtx, l.Logger, "ListAggregationJobs")
	if err != nil {
		return nil, err
	}
	defer done()

	size, err := pageSize(l.svcCtx, in.GetPs())
	if err != nil {
		return nil, err
	}
	filter := model.JobFilter{Limit: size, Offset: offsetTo32(pageOffset(in.GetPn(), size))}
	if jobType := int32(in.GetJobType()); jobType != model.JobTypeUnspecified {
		if !model.ValidJobType(jobType) {
			return nil, fmt.Errorf("%w: job_type=%d", model.ErrInvalidJobType, jobType)
		}
		filter.JobType = jobType
	}
	if state := int32(in.GetState()); state != model.JobStateUnspecified {
		if !validJobState(state) {
			return nil, fmt.Errorf("%w: state=%d", model.ErrInvalidJobState, state)
		}
		filter.State = state
	}
	if in.GetSince() < 0 {
		return nil, fmt.Errorf("%w: since=%d 不能为负", model.ErrInvalidJobState, in.GetSince())
	}
	filter.Since = in.GetSince()

	total, err := l.svcCtx.Jobs.Count(l.ctx, filter)
	if err != nil {
		return nil, err
	}
	reply := &rpc.ListAggregationJobsReply{Total: total}
	if !pageFits(int64(filter.Offset), total, size) {
		return reply, nil
	}
	rows, err := l.svcCtx.Jobs.List(l.ctx, filter)
	if err != nil {
		return nil, err
	}
	reply.Jobs = jobList(rows)
	return reply, nil
}

// validJobState 作业状态白名单：UNSPECIFIED 与越界值都拒绝（列表过滤条件写错时，
// 把它当「不限状态」返回会给出一张看起来正常、实则没按条件过滤的清单）。
func validJobState(s int32) bool { return s >= model.JobStatePending && s <= model.JobStateCancelled }
