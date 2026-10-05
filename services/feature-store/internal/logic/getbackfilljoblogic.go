package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/feature-store/internal/svc"
	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetBackfillJobLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetBackfillJobLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetBackfillJobLogic {
	return &GetBackfillJobLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询回填任务（按 job_id 或 request_id）
//
// 两个定位键二选一：job_id 是提交方拿到的返回值，request_id 是幂等键（走 uniq_request_id），
// 后者让「提交请求超时、不知道 job_id」的调用方也能查到自己那一次。
// 都查不到时 found=false 而不是报错：作业台账是运维视图，不存在就是不存在。
// 进度三列（entities_done / entities_failed / cursor_entity_id）由 worker 分段推进并续租，
// 这里是「读台账现值」，不做任何推算，也不补 0 假装进度。
func (l *GetBackfillJobLogic) GetBackfillJob(
	in *rpc.GetBackfillJobReq) (*rpc.GetBackfillJobReply, error) {
	jobID := in.GetJobId()
	requestID := strings.TrimSpace(in.GetRequestId())
	if jobID <= 0 && requestID == "" {
		return nil, fmt.Errorf("%w: job_id or request_id is required", model.ErrJobNotFound)
	}
	if jobID > 0 {
		job, err := l.svcCtx.Backfills.FindOne(l.ctx, jobID)
		if err != nil {
			if errors.Is(err, model.ErrJobNotFound) {
				return &rpc.GetBackfillJobReply{Found: false}, nil
			}
			return nil, err
		}
		return &rpc.GetBackfillJobReply{Job: backfillToProto(job), Found: true}, nil
	}
	if err := checkRequestID(requestID); err != nil {
		return nil, err
	}
	job, err := l.svcCtx.Backfills.FindByRequestID(l.ctx, requestID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return &rpc.GetBackfillJobReply{Found: false}, nil
	}
	return &rpc.GetBackfillJobReply{Job: backfillToProto(job), Found: true}, nil
}
