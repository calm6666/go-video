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

type LiveStreamHealthLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 流健康：当前判定 + 窗口聚合 + 最近采样点
func NewLiveStreamHealthLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveStreamHealthLogic {
	return &LiveStreamHealthLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveStreamHealth 聚合 live-ingest GetStreamHealth。
//
// 只有窗口与采样数形态门槛：window_seconds <= 0 由服务取配置默认、sample_limit 由服务夹取到
// MaxSamplePoints，网关不预先夹一遍（夹了会让后台与实际返回的条数对不上，反而更难解释）。
//
// 健康判定与阈值（何时算 DEGRADED/AT_RISK）是 live-ingest 的观测口径，网关按值投影不二次解释；
// fps_x100 与 packet_loss_ppm 的 ×100 / 百万分比口径同样由服务定义，此处不做单位换算。
func (l *LiveStreamHealthLogic) LiveStreamHealth(req *types.ParamLiveStreamHealth) (resp *types.LiveStreamHealthResponse, err error) {
	if l.svcCtx.LiveIngest == nil {
		return nil, errLiveIngestNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveRequiredText("stream_id", req.StreamId); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("window_seconds", req.WindowSeconds); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("sample_limit", req.SampleLimit); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveIngest.GetStreamHealth(l.ctx, &liveingestrpc.GetStreamHealthReq{
		StreamId:      req.StreamId,
		WindowSeconds: req.WindowSeconds,
		SampleLimit:   req.SampleLimit,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveStreamHealth: stream_id=%s window_seconds=%d err=%v", req.StreamId, req.WindowSeconds, err)
		return nil, err
	}
	return &types.LiveStreamHealthResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveStreamHealthData{
			StreamId:                reply.GetStreamId(),
			State:                   int32(reply.GetState()),
			HealthState:             int32(reply.GetHealthState()),
			HealthReportedAt:        reply.GetHealthReportedAt(),
			AvgVideoBitrateBps:      reply.GetAvgVideoBitrateBps(),
			MinVideoBitrateBps:      reply.GetMinVideoBitrateBps(),
			MaxPacketLossPpm:        reply.GetMaxPacketLossPpm(),
			SampleCount:             reply.GetSampleCount(),
			Samples:                 liveHealthSamplesToAPI(reply.GetSamples()),
			InterruptedTotalSeconds: reply.GetInterruptedTotalSeconds(),
			InterruptionCount:       reply.GetInterruptionCount(),
		},
		TTL: 0,
	}, nil
}
