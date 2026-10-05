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

type SpmAggregationJobGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 聚合作业进度（job_id 或 request_id 二选一）
func NewSpmAggregationJobGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SpmAggregationJobGetLogic {
	return &SpmAggregationJobGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SpmAggregationJobGet 转发 spm GetAggregationJob（作业进度；job_id 与 request_id 二选一）。
//
// 两个定位位都为空在网关就挡下：服务会按空主键查出一条「不存在」，后台于是把参数缺失
// 读成「作业丢了」——这是最难排查的一类误读。给了 job_id 就按 job_id 查（不因为同时给了
// request_id 而挑一个「更准」的），两个都给时以哪个为准是服务的口径，网关不替它决定。
// request_id 原样透传，不 trim 不改写：它就是当初那次提交的指纹。
func (l *SpmAggregationJobGetLogic) SpmAggregationJobGet(req *types.ParamSpmAggregationJobGet) (resp *types.SpmAggregationJobGetResponse, err error) {
	if l.svcCtx.Spm == nil {
		return nil, errSpmServiceNotConfigured
	}
	if req == nil {
		return nil, errSpmRequestMissing
	}
	if err := spmJobSubject(req.JobId, req.RequestId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Spm.GetAggregationJob(l.ctx, &spmrpc.GetAggregationJobReq{
		JobId:     req.JobId,
		RequestId: req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/admin/spmAggregationJobGet: job_id=%d request_id=%s err=%v", req.JobId, req.RequestId, err)
		return nil, err
	}
	return &types.SpmAggregationJobGetResponse{
		Code:    0,
		Message: "ok",
		Data: types.SpmAggregationJobGetData{
			Found: reply.GetFound(),
			Job:   spmJobToAPI(reply.GetJob()),
		},
		TTL: 0,
	}, nil
}
