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

type GetAggregationJobLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetAggregationJobLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetAggregationJobLogic {
	return &GetAggregationJobLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询作业（按 job_id 或 request_id）
func (l *GetAggregationJobLogic) GetAggregationJob(in *rpc.GetAggregationJobReq) (*rpc.GetAggregationJobReply, error) {
	// 逻辑轮规划：job_id 与 request_id 至少给一个：前者走主键、后者走 uniq_request_id；找不到时 found=false 而不报错，便于 cron 轮询终态。
	done, err := acquireReadToken(l.ctx, l.svcCtx, l.Logger, "GetAggregationJob")
	if err != nil {
		return nil, err
	}
	defer done()

	if in.GetJobId() <= 0 && in.GetRequestId() == "" {
		// 两个定位条件都不给，等于让服务端去猜「你问的是哪个作业」——这里没有可查的索引，
		// 只能扫表，所以按缺参拒绝。
		return nil, fmt.Errorf("%w: job_id 与 request_id 至少给一个", model.ErrRequestIdRequired)
	}

	var row *model.AggregationJob
	switch {
	case in.GetJobId() > 0:
		if row, err = l.svcCtx.Jobs.FindByID(l.ctx, in.GetJobId()); err != nil {
			return nil, err
		}
	default:
		// 两个都给了：以 job_id 为准（调用方多半是「提交后拿到的 id + 自己记的幂等键」）。
		// 两者不一致时不回 job_id 那一行，而是回 not found —— 静默挑一个会把
		// 「幂等键对应哪个作业」这条排查线索断掉。
		if row, err = l.svcCtx.Jobs.FindByRequestID(l.ctx, in.GetRequestId()); err != nil {
			return nil, err
		}
		if row != nil && row.ID != in.GetJobId() {
			l.Errorf("spm/GetAggregationJob: job_id=%d 与 request_id=%s 指向 job_id=%d，按不命中处理",
				in.GetJobId(), in.GetRequestId(), row.ID)
			return &rpc.GetAggregationJobReply{Found: false}, nil
		}
	}
	if row == nil {
		// found=false 而不是报错：cron 会按固定间隔轮询终态，作业不存在是它要处理的常态。
		return &rpc.GetAggregationJobReply{Found: false}, nil
	}
	return &rpc.GetAggregationJobReply{Found: true, Job: jobOf(row)}, nil
}
