// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	collectorrpc "go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CollectorBatchGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 按 batch_id 精读接收批次（计数、状态、整批首要拒绝原因）
func NewCollectorBatchGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CollectorBatchGetLogic {
	return &CollectorBatchGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CollectorBatchGet 转发 event-collector GetIngestBatch。
// batch_id 是客户端/上报方生成的批次幂等键，网关只 TrimSpace 判空、不改写原值：
// 少一个字符就变成「查无此批」。
// found=false 是**结论**不是错误：批次可能已过 ec_ingest_batch 的保留期（retention_days，
// 由 cron 清理），也可能这个 batch_id 从未被接收。两者都由台账自身表达，网关不折叠成 404，
// 也不伪造一个空批次让后台以为「收到过但没内容」。
func (l *CollectorBatchGetLogic) CollectorBatchGet(req *types.ParamCollectorBatchGet) (resp *types.CollectorBatchResponse, err error) {
	if l.svcCtx.EventCollector == nil {
		return nil, errCollectorServiceNotConfigured
	}
	if req == nil {
		return nil, errCollectorRequestMissing
	}
	if _, ok := collectorTrim(req.BatchId); !ok {
		return nil, errCollectorSubjectRequired
	}
	reply, err := l.svcCtx.EventCollector.GetIngestBatch(l.ctx, &collectorrpc.GetIngestBatchReq{
		BatchId: req.BatchId,
	})
	if err != nil {
		l.Errorf("gateway/admin/collectorBatchGet: batch_id=%s err=%v", req.BatchId, err)
		return nil, err
	}
	return &types.CollectorBatchResponse{
		Code:    0,
		Message: "ok",
		Data: types.CollectorBatchData{
			Found: reply.GetFound(),
			Batch: collectorBatchToAPI(reply.GetBatch()),
		},
		TTL: 0,
	}, nil
}
