// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"

	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CollectEventsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCollectEventsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CollectEventsLogic {
	return &CollectEventsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 客户端 SDK 批量上报（batch_id 幂等，受条数/字节/限流约束）。
//
// 完整流水线（容量闸门 → 取盐脱敏 → 逐条校验 → 确定性采样 → 同事务落库 + Outbox）
// 在 ingest.go 的 runBatchIngest 里，与 IngestServerEvents 共用同一份规则：
// 两套规则必然漂移，最后变成「一个入口能收、另一个入口拒」。
// 本方法只负责入参归一化与响应投影。
func (l *CollectEventsLogic) CollectEvents(in *rpc.CollectEventsReq) (*rpc.CollectEventsReply, error) {
	if in == nil {
		return nil, model.ErrBatchIDRequired
	}
	source := in.GetSource()
	if source == rpc.Source_SOURCE_UNSPECIFIED {
		// 未指定按 SOURCE_CLIENT 处理（proto 注释）；显式给 SOURCE_SERVER 是走错通道。
		source = rpc.Source_SOURCE_CLIENT
	}
	mc := in.GetContext()
	if mc == nil {
		return nil, model.ErrEventsRequired
	}
	out, err := runBatchIngest(l.ctx, l.svcCtx, l.Logger, batchRequest{
		method:     "CollectEvents",
		batchID:    in.GetBatchId(),
		wantSource: model.SourceClient,
		source:     int32(source),
		// 客户端通道不接受调用方自报的幂等键与服务身份：那会让客户端把自己
		// 伪装成不采样的服务端埋点来源。
		policyHint:   in.GetPolicyVersion(),
		clientSeq:    in.GetClientSeq(),
		requestID:    in.GetRequestId(),
		batchTraceID: "",
		eventCtx:     mc,
		events:       in.GetEvents(),
		sampling:     true,
		bytes:        requestBytes(in),
	})
	if err != nil {
		return nil, err
	}
	return out.toCollectReply(), nil
}
