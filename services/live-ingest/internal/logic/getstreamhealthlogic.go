package logic

import (
	"context"
	"errors"

	"go-video/services/live-ingest/internal/repository"
	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetStreamHealthLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetStreamHealthLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetStreamHealthLogic {
	return &GetStreamHealthLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 流健康检查：当前判定 + 窗口聚合 + 最近采样点
//
// 厂商探测（Gateway.ProbeStream）只是补偿路径：适配器未接入时降级为纯 DB 视图并记一条
// 告警日志，不整体失败——「读不到厂商侧」不等于「流不健康」，也不能反过来把无采样洗成正常。
func (l *GetStreamHealthLogic) GetStreamHealth(in *rpc.GetStreamHealthReq) (*rpc.GetStreamHealthReply, error) {
	cfg := l.svcCtx.Config.LiveIngest
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}
	streamID, err := checkStreamID(in.StreamId)
	if err != nil {
		return nil, err
	}
	s, err := repo.Stream.FindOne(l.ctx, streamID)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, model.ErrStreamNotFound
	}

	window := positiveOrDefault(int64(cfg.HealthSampleWindowSeconds), int64(in.WindowSeconds))
	if cfg.HealthSampleWindowSeconds > 0 && window > int64(cfg.HealthSampleWindowSeconds) {
		window = int64(cfg.HealthSampleWindowSeconds)
	}
	if window <= 0 {
		window = int64(defaultHealthWindowSeconds)
	}
	since := nowUnix() - window

	agg, err := repo.StreamHealthReport.Aggregate(l.ctx, streamID, since)
	if err != nil {
		return nil, err
	}
	limit := clampLimit(in.SampleLimit, cfg.MaxSamplePoints)
	recent, err := repo.StreamHealthReport.ListRecent(l.ctx, streamID, since, limit)
	if err != nil {
		return nil, err
	}

	health := s.HealthState
	if s.State != model.StreamStateStopped && cfg.HealthNoDataSeconds > 0 &&
		nowUnix()-s.HealthReportedAt > cfg.HealthNoDataSeconds {
		// 读路径不改库：NO_DATA 是「当前视图」，把它写进 health_state 会抹掉最后一次真实判定。
		health = model.HealthStateNoData
	}

	reply := &rpc.GetStreamHealthReply{
		StreamId: s.StreamID, State: rpcStreamState(s.State), HealthState: rpcHealthState(health),
		HealthReportedAt: s.HealthReportedAt, Samples: healthSamples(recent),
		InterruptedTotalSeconds: s.InterruptedTotalSecs, InterruptionCount: s.InterruptionCount,
	}
	if agg != nil {
		reply.AvgVideoBitrateBps = agg.AvgVideoBitrate
		reply.MinVideoBitrateBps = agg.MinVideoBitrate
		reply.MaxPacketLossPpm = agg.MaxPacketLossPpm
		reply.SampleCount = agg.SampleCount
	}
	if l.svcCtx.Gateway != nil {
		if _, err := l.svcCtx.Gateway.ProbeStream(l.ctx, s.NodeID, s.StreamID); err != nil {
			if errors.Is(err, repository.ErrCdnNotConfigured) {
				l.Logger.Infow("cdn probe unavailable, serving db-only health view",
					logx.Field("module", "live-ingest"), logx.Field("reason", "gateway_not_configured"))
			} else {
				l.Logger.Errorw("cdn probe failed, serving db-only health view",
					logx.Field("module", "live-ingest"), logx.Field("reason", "probe_failed"))
			}
		}
	}
	return reply, nil
}
