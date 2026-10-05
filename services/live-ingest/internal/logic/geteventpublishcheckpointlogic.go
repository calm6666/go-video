package logic

import (
	"context"
	"fmt"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetEventPublishCheckpointLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetEventPublishCheckpointLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetEventPublishCheckpointLogic {
	return &GetEventPublishCheckpointLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// Outbox 发布位点与滞后度（观测「事件必须可追踪」）
//
// 位点来自 model.Outbox.Checkpoint 的单条聚合查询（同一 state 索引一次扫完），
// 不做多次往返：多次往返之间的写入会让 pending/failed 计数互相矛盾，
// 观测面板就会显示「待发布 0 条但最老待发布时间是 3 分钟前」。
//
// pending_limit<=0 表示「只要位点不要样本」：这是一个高频观测调用，
// 每次都附带 20 条事件摘要会让监控自己成为负载来源。
func (l *GetEventPublishCheckpointLogic) GetEventPublishCheckpoint(in *rpc.GetEventPublishCheckpointReq) (*rpc.GetEventPublishCheckpointReply, error) {
	cfg := l.svcCtx.Config.LiveIngest
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}
	if in.PendingLimit < 0 {
		return nil, fmt.Errorf("%w: pending_limit 不能为负", model.ErrPageSizeInvalid)
	}
	withSamples := in.PendingLimit > 0
	sampleLimit := clampLimit(in.PendingLimit, cfg.MaxEventPageSize)

	cp, err := repo.Outbox.Checkpoint(l.ctx)
	if err != nil {
		return nil, err
	}
	if cp == nil {
		cp = &model.OutboxCheckpoint{}
	}
	serverTime := nowUnix()
	out := &rpc.EventPublishCheckpoint{
		LastPublishedId: cp.LastPublishedID,
		LastPublishedAt: cp.LastPublishedAt,
		PendingCount:    toInt32Total(cp.PendingCount),
		FailedCount:     toInt32Total(cp.FailedCount),
		OldestPendingId: cp.OldestPendingID,
		OldestPendingAt: cp.OldestPendingAt,
		ServerTime:      serverTime,
	}
	if cp.OldestPendingAt > 0 && serverTime > cp.OldestPendingAt {
		out.LagSeconds = serverTime - cp.OldestPendingAt
	}
	if !withSamples {
		return &rpc.GetEventPublishCheckpointReply{Checkpoint: out}, nil
	}
	pending, err := l.eventSamples(model.OutboxStatePending, sampleLimit)
	if err != nil {
		return nil, err
	}
	out.PendingSample = pending
	if in.IncludeFailed {
		failed, err := l.eventSamples(model.OutboxStateFailed, sampleLimit)
		if err != nil {
			return nil, err
		}
		out.FailedSample = failed
	}
	return &rpc.GetEventPublishCheckpointReply{Checkpoint: out}, nil
}

// eventSamples 把 outbox 行映射成事件摘要：按 event_id 回查 live_stream_event，
// 而不是解析 outbox.payload。两个视图必须同源，否则「位点里看到的 seq」
// 和 ListStreamEvents 给的 seq 会有机会对不上。
func (l *GetEventPublishCheckpointLogic) eventSamples(state, limit int32) ([]*rpc.StreamEventInfo, error) {
	repo := l.svcCtx.Repository
	rows, err := repo.Outbox.ListByState(l.ctx, state, limit)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if r != nil && r.EventID != "" {
			ids = append(ids, r.EventID)
		}
	}
	if len(ids) == 0 {
		return []*rpc.StreamEventInfo{}, nil
	}
	events, err := repo.StreamEvent.ListByEventIDs(l.ctx, ids)
	if err != nil {
		return nil, err
	}
	return eventInfos(events), nil
}
