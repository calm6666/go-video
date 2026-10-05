package logic

import (
	"context"
	"fmt"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type ReportStreamHealthLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReportStreamHealthLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportStreamHealthLogic {
	return &ReportStreamHealthLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 健康采样上报：更新最新健康字段并留采样点，越过危险阈值时触发 INTERRUPTED 迁移
//
// 「采样落库 + 投影回写 + 触发迁移（事件 + Outbox）」在同一事务：
// 只写采样不改投影，GetStreamHealth 会永远看到旧码率；只改投影不写采样，
// 聚合窗口就失去事实来源。两种半截状态都会让运营误判。
func (l *ReportStreamHealthLogic) ReportStreamHealth(in *rpc.ReportStreamHealthReq) (*rpc.ReportStreamHealthReply, error) {
	cfg := l.svcCtx.Config.LiveIngest
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}
	streamID, err := checkStreamID(in.StreamId)
	if err != nil {
		return nil, err
	}
	reportID, err := checkReportID(in.ReportId)
	if err != nil {
		return nil, err
	}
	nodeID := ""
	if in.NodeId != "" {
		if nodeID, err = checkNodeID(in.NodeId); err != nil {
			return nil, err
		}
	}
	if err := checkSampleMetrics(in); err != nil {
		return nil, err
	}

	// 幂等回放：采样行的 uniq_report_id 是判定依据，命中就不再新增采样点。
	dup, err := repo.StreamHealthReport.FindByReportID(l.ctx, reportID)
	if err != nil {
		return nil, err
	}
	if dup != nil {
		res := &rpc.ReportStreamHealthReply{
			HealthState: rpcHealthState(dup.HealthState), Replayed: true,
			Message: "相同 report_id 的采样已记录；本次未重复入库",
		}
		// 首次若触发了断流迁移，事件按同一 report_id 承载，回带 seq/event_id 才有意义。
		ev, err := repo.StreamEvent.FindByReportID(l.ctx, reportID)
		if err != nil {
			return nil, err
		}
		if ev != nil {
			res.Seq = ev.Seq
			res.EventId = ev.EventID
			res.TriggeredInterrupt = ev.ToState == model.StreamStateInterrupted
		}
		return res, nil
	}

	now := nowUnix()
	at, err := reportedAt(in.OccurredAt, now, cfg.CallbackSkewSeconds)
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
	if s.State == model.StreamStateStopped {
		// 终态流不再接受健康上报：节点侧应停止这条流的采样，而不是继续写无主数据。
		return nil, model.ErrTerminalStream
	}
	health := judgeHealth(healthThresholds{
		degradedMinVideoBitrate:  cfg.DegradedMinVideoBitrateBps,
		criticalMinVideoBitrate:  cfg.CriticalMinVideoBitrateBps,
		criticalMinFpsX100:       cfg.CriticalMinFpsX100,
		criticalMaxPacketLossPpm: cfg.CriticalMaxPacketLossPpm,
	}, in.VideoBitrateBps, in.FpsX100, in.PacketLossPpm)

	// 连续越界判定用「本次之前」的采样：刚插入的这条还没提交，事务内读不到自己。
	confirm := health == model.HealthStateCritical && s.State == model.StreamStatePublishing
	if confirm {
		need := int32(healthInterruptConfirmSamples - 1)
		recent, err := repo.StreamHealthReport.ListRecent(l.ctx, streamID, at-int64(cfg.HealthSampleWindowSeconds), need)
		if err != nil {
			return nil, err
		}
		if !lastSamplesCritical(recent, need) {
			confirm = false
		}
	}

	res := &transitionResult{}
	err = repo.Conn().TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		if _, err := repo.StreamHealthReport.Insert(ctx, tx, &model.StreamHealthReport{
			ReportID: reportID, StreamID: streamID, NodeID: nodeID,
			VideoBitrateBps: in.VideoBitrateBps, AudioBitrateBps: in.AudioBitrateBps,
			FpsX100: in.FpsX100, PacketLossPpm: in.PacketLossPpm, RttMs: in.RttMs,
			SampleWindowSeconds: sampleWindowOf(cfg.HealthSampleWindowSeconds, in.SampleWindowSeconds),
			HealthState:         health, OccurredAt: at, TraceID: sanitizeTraceID(in.TraceId),
		}); err != nil {
			return err
		}
		applied, err := repo.Stream.ApplyHealthTx(ctx, tx, streamID, model.StreamHealthPatch{
			HealthState: health, ReportedAt: at, VideoBitrateBps: in.VideoBitrateBps,
			AudioBitrateBps: in.AudioBitrateBps, FpsX100: in.FpsX100, PacketLossPpm: in.PacketLossPpm,
			TriggeredInterrupt: confirm,
		})
		if err != nil {
			return err
		}
		if !applied {
			// 非终态条件没命中：并发的停流先到了。整笔回滚，采样也不留，避免留下无主样本。
			return model.ErrTerminalStream
		}
		if !confirm {
			return nil
		}
		tr, err := applyStreamTransition(ctx, l.svcCtx, tx, transitionInput{
			streamID: streamID,
			to:       model.StreamStateInterrupted,
			at:       at,
			nodeID:   nodeID,
			reportID: reportID,
			source:   model.EventSourceHealth,
			reason:   "health metrics stayed critical",
			traceID:  sanitizeTraceID(in.TraceId),
		})
		if err != nil {
			return err
		}
		res = tr
		return nil
	})
	if err != nil {
		if model.IsDuplicate(err) {
			// 与并发上报撞 uniq_report_id：让调用方重试走回放分支，绝不产生第二个采样点。
			return nil, fmt.Errorf("%w: duplicate health report id", model.ErrConcurrentUpdate)
		}
		return nil, err
	}

	reply := &rpc.ReportStreamHealthReply{HealthState: rpcHealthState(health)}
	switch {
	case res.applied:
		reply.TriggeredInterrupt = true
		reply.Seq = res.seq
		reply.EventId = res.eventID
		reply.Message = "连续危险采样，已按状态机进入断流并产生事件"
	case health == model.HealthStateCritical && s.State == model.StreamStatePublishing:
		reply.Message = "本次采样危险，但未达连续次数，暂不断流"
	default:
		reply.Message = "采样已记录"
	}
	return reply, nil
}

// judgeHealth 按配置阈值判定单个采样点：CRITICAL 优先于 DEGRADED。
// 阈值 <=0 表示该维度未配置，不参与判定（而不是「任何值都算越界」）。
func judgeHealth(cfg healthThresholds, videoBitrate int64, fpsX100, packetLossPpm int32) int32 {
	if cfg.criticalMinVideoBitrate > 0 && videoBitrate < cfg.criticalMinVideoBitrate {
		return model.HealthStateCritical
	}
	if cfg.criticalMinFpsX100 > 0 && fpsX100 < cfg.criticalMinFpsX100 {
		return model.HealthStateCritical
	}
	if cfg.criticalMaxPacketLossPpm > 0 && packetLossPpm > cfg.criticalMaxPacketLossPpm {
		return model.HealthStateCritical
	}
	if cfg.degradedMinVideoBitrate > 0 && videoBitrate < cfg.degradedMinVideoBitrate {
		return model.HealthStateDegraded
	}
	return model.HealthStateHealthy
}

// healthThresholds 只取判定需要的四个阈值，便于单测不必构造整个配置。
type healthThresholds struct {
	degradedMinVideoBitrate  int64
	criticalMinVideoBitrate  int64
	criticalMinFpsX100       int32
	criticalMaxPacketLossPpm int32
}

func lastSamplesCritical(rows []*model.StreamHealthReport, need int32) bool {
	if need <= 0 {
		return true
	}
	if int32(len(rows)) < need {
		return false
	}
	for _, r := range rows[len(rows)-int(need):] {
		if r.HealthState != model.HealthStateCritical {
			return false
		}
	}
	return true
}

// sampleWindowOf 归一采样窗口并夹到物理上限，避免一个夸张的窗口污染聚合。
func sampleWindowOf(configured, requested int32) int32 {
	v := requested
	if v <= 0 {
		v = configured
	}
	if v <= 0 {
		return 1
	}
	return clampInt32(v, 1, maxSampleWindowSeconds)
}

// checkSampleMetrics 校验健康指标：非负且在物理上限内。
func checkSampleMetrics(in *rpc.ReportStreamHealthReq) error {
	switch {
	case in.VideoBitrateBps < 0 || in.VideoBitrateBps > maxVideoBitrateBps:
		return fmt.Errorf("%w: video bitrate out of range", model.ErrInvalidSampleMetrics)
	case in.AudioBitrateBps < 0 || in.AudioBitrateBps > maxAudioBitrateBps:
		return fmt.Errorf("%w: audio bitrate out of range", model.ErrInvalidSampleMetrics)
	case in.FpsX100 < 0 || in.FpsX100 > maxFpsX100:
		return fmt.Errorf("%w: fps out of range", model.ErrInvalidSampleMetrics)
	case in.PacketLossPpm < 0 || in.PacketLossPpm > maxPacketLossPpm:
		return fmt.Errorf("%w: packet loss out of range", model.ErrInvalidSampleMetrics)
	case in.RttMs < 0 || in.RttMs > maxRttMs:
		return fmt.Errorf("%w: rtt out of range", model.ErrInvalidSampleMetrics)
	case in.SampleWindowSeconds < 0 || in.SampleWindowSeconds > maxSampleWindowSeconds:
		return fmt.Errorf("%w: sample window out of range", model.ErrInvalidSampleMetrics)
	}
	return nil
}
