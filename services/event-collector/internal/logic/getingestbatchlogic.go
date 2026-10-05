// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"strings"

	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetIngestBatchLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetIngestBatchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetIngestBatchLogic {
	return &GetIngestBatchLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询批次接收台账。
//
// 只读排障入口：不触发任何状态推进（幂等回放与整批重驱动都在 ingest.go 的采集路径里）。
// 投影只输出脱敏列（device_hash / ip_segment / salt_version）；计数列是 ec_event_record
// 的可重算投影，滞后属预期（README 已知缺口：批次计数重算由 cron 任务负责）。
func (l *GetIngestBatchLogic) GetIngestBatch(in *rpc.GetIngestBatchReq) (*rpc.GetIngestBatchReply, error) {
	batchID := strings.TrimSpace(in.GetBatchId())
	if batchID == "" || len(batchID) > maxBatchIDBytes {
		return nil, model.ErrBatchIDRequired
	}
	row, err := l.svcCtx.Batches.FindByBatchID(l.ctx, batchID)
	if err != nil {
		if model.IsNotFound(err) {
			// 「没有这个批次」是查询的正常答案，用 proto 的 found 表达；
			// DB 故障必须继续抛错 —— 把后者也回成 found=false，
			// 调用方就会断定「我的上报丢了」并重发整批。
			return &rpc.GetIngestBatchReply{Found: false}, nil
		}
		return nil, err
	}
	return &rpc.GetIngestBatchReply{Batch: batchToRPC(row), Found: true}, nil
}
