// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"

	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type IngestServerEventsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewIngestServerEventsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *IngestServerEventsLogic {
	return &IngestServerEventsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 服务端内部埋点上报（不采样、trace_id 必填、要求服务身份）。
//
// 与 CollectEvents 共用 runBatchIngest，只在这三处不同（proto 口径）：
//  1. 来源只能是 SOURCE_SERVER（未指定即归一化成 SERVER），且必须带 caller_service：
//     否则「不采样」这条特权就能被客户端通道借用（REJECT_SOURCE_NOT_ALLOWED）。
//  2. sampling=false：engagement/playback 的关键埋点一律全量投递，不做采样。
//  3. idempotency_key 必填，与 batch_id 一起构成跨进程重试的幂等落点；
//     批次级 trace_id 由事件未自带时继承（validateEvent 里判 REJECT_MISSING_TRACE_ID）。
//
// 限流维度换成 caller_service（见 ingest.go dimensionGate），落库与投递路径完全一致。
func (l *IngestServerEventsLogic) IngestServerEvents(in *rpc.IngestServerEventsReq) (*rpc.IngestServerEventsReply, error) {
	if in == nil {
		return nil, model.ErrBatchIDRequired
	}
	mc := in.GetContext()
	if mc == nil {
		// 服务端埋点也要有归属主体：现场造一个空上下文会把「无主体」伪装成合法请求，
		// 于是每条事件都变成 REJECT_MISSING_SUBJECT，反而看不出调用方漏传了上下文。
		return nil, model.ErrEventsRequired
	}
	out, err := runBatchIngest(l.ctx, l.svcCtx, l.Logger, batchRequest{
		method:         "IngestServerEvents",
		batchID:        in.GetBatchId(),
		wantSource:     model.SourceServer,
		source:         model.SourceServer,
		callerService:  in.GetCallerService(),
		idempotencyKey: in.GetIdempotencyKey(),
		// 服务端埋点没有「客户端缓存的策略版本」概念，永远按当前 ACTIVE 裁决。
		policyHint:   "",
		requestID:    "",
		batchTraceID: in.GetTraceId(),
		eventCtx:     mc,
		events:       in.GetEvents(),
		sampling:     false,
		bytes:        requestBytes(in),
	})
	if err != nil {
		return nil, err
	}
	return out.toServerReply(), nil
}
