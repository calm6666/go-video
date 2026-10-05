package logic

import (
	"context"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListStreamInterruptionsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListStreamInterruptionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListStreamInterruptionsLogic {
	return &ListStreamInterruptionsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 断流与重连查询
//
// total 走 CountHardLimit：命中上限时 model 返回 -1，本方法原样回带。
// 断流记录是一张只增不改的大表，为了一个「大概多少条」做全表 COUNT 不值得，
// 调用方（运营面板）按 -1 显示「超过阈值」即可。
func (l *ListStreamInterruptionsLogic) ListStreamInterruptions(in *rpc.ListStreamInterruptionsReq) (*rpc.ListStreamInterruptionsReply, error) {
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}
	if in.StreamId == "" && in.RoomId <= 0 {
		// 两个维度都不给就是无边界扫描，直接拒绝。
		return nil, model.ErrInvalidStreamId
	}

	filter := model.InterruptionFilter{
		StreamID: in.StreamId, RoomID: in.RoomId, OnlyOpen: in.OnlyOpen,
		StartTime: in.StartTime, EndTime: in.EndTime,
	}
	if filter.StreamID != "" {
		streamID, err := checkStreamID(in.StreamId)
		if err != nil {
			return nil, err
		}
		filter.StreamID = streamID
	}
	if filter.StartTime < 0 || filter.EndTime < 0 {
		return nil, model.ErrInvalidStreamId
	}
	if filter.StartTime > 0 && filter.EndTime > 0 && filter.StartTime > filter.EndTime {
		return nil, model.ErrInvalidStreamId
	}
	// MaxResults 多取一条：靠它判断「还有更多」，而 CountByFilter 的硬上限只用于 total。
	hardLimit := l.svcCtx.Config.LiveIngest.CountHardLimit
	if hardLimit <= 0 {
		hardLimit = fallbackCountHardLimit
	}
	filter.MaxResults = clampLimit(in.Limit, l.svcCtx.Config.LiveIngest.MaxListPageSize)

	rows, err := repo.StreamInterruption.ListByFilter(l.ctx, filter)
	if err != nil {
		return nil, err
	}
	total, err := repo.StreamInterruption.CountByFilter(l.ctx, filter, hardLimit)
	if err != nil {
		return nil, err
	}
	return &rpc.ListStreamInterruptionsReply{
		Interruptions: interruptionInfos(rows),
		Total:         toInt32Total(total),
	}, nil
}
