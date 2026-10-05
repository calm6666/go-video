// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	liveingestrpc "go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiveFailedEventRetryLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 重试失败的流状态事件（outbox 运营补偿；request_id 幂等）
func NewLiveFailedEventRetryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveFailedEventRetryLogic {
	return &LiveFailedEventRetryLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveFailedEventRetry 聚合 live-ingest RetryFailedEvents（把超过重试上限的事件重置为待发布）。
//
// 这是 outbox 的运营补偿口，不是事件写入口：事件内容与 seq 都在建档时由服务确定，
// 本路由只能让既有事件重发，不能造一条新事件（造事件是 ReportStreamState，那条不开面）。
//
// event_ids 为空时按 limit 批量重试，limit 上限（MaxEventRetryBatch）由服务夹取，网关不预先夹；
// retried / remaining_failed 原样回传——「重试了 0 条、仍有失败」是真实状态，不粉饰成成功。
// 网关也不代为轮询 GetEventPublishCheckpoint 来判断「是否真的发出去了」（观测与推进留在服务侧）。
func (l *LiveFailedEventRetryLogic) LiveFailedEventRetry(req *types.ParamLiveFailedEventRetry) (resp *types.LiveFailedEventRetryResponse, err error) {
	if l.svcCtx.LiveIngest == nil {
		return nil, errLiveIngestNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveOperatorGate(l.ctx, "liveFailedEventRetry", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := liveIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("limit", req.Limit); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveIngest.RetryFailedEvents(l.ctx, &liveingestrpc.RetryFailedEventsReq{
		EventIds:    req.EventIds,
		Limit:       req.Limit,
		RequestId:   req.RequestId,
		OperatorMid: req.OperatorMid,
		Reason:      req.Reason,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveFailedEventRetry: operator_mid=%d limit=%d request_id=%s err=%v",
			req.OperatorMid, req.Limit, req.RequestId, err)
		return nil, err
	}
	return &types.LiveFailedEventRetryResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveFailedEventRetryData{
			Retried:         reply.GetRetried(),
			RemainingFailed: reply.GetRemainingFailed(),
			Replayed:        reply.GetReplayed(),
			Message:         reply.GetMessage(),
		},
		TTL: 0,
	}, nil
}
