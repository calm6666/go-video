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

type CollectorBatchListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 批次台账游标翻页（来源/状态/mid/设备摘要/IP 段/时间窗）
func NewCollectorBatchListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CollectorBatchListLogic {
	return &CollectorBatchListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CollectorBatchList 转发 event-collector ListIngestBatches。
// 网关只挡「负数」与「from>to」两类形状问题：page_size 上限、cursor 语法、时间跨度上限
// （maxListWindowSeconds）与「零条件 + 零边界」的无界扫描拒绝全在 reads.go，
// 那里才知道这张表会长多大。
// device_hash / ip_segment 是**已经脱敏后的**过滤值，网关不接收明文设备号或 IP，
// 也不在这里替调用方算摘要——算摘要要用盐，而盐只在 event-collector 侧可取（AGENTS.md §7）。
func (l *CollectorBatchListLogic) CollectorBatchList(req *types.ParamCollectorBatchList) (resp *types.CollectorBatchListResponse, err error) {
	if l.svcCtx.EventCollector == nil {
		return nil, errCollectorServiceNotConfigured
	}
	if req == nil {
		return nil, errCollectorRequestMissing
	}
	if err := collectorTimeWindow(req.CtimeFrom, req.CtimeTo); err != nil {
		return nil, err
	}
	if err := collectorNonNeg32("page_size", req.PageSize); err != nil {
		return nil, err
	}
	if err := collectorNonNeg("mid", req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.EventCollector.ListIngestBatches(l.ctx, &collectorrpc.ListIngestBatchesReq{
		Source:     collectorrpc.Source(req.Source),
		State:      collectorrpc.BatchState(req.State),
		Mid:        req.Mid,
		DeviceHash: req.DeviceHash,
		IpSegment:  req.IpSegment,
		CtimeFrom:  req.CtimeFrom,
		CtimeTo:    req.CtimeTo,
		Cursor:     req.Cursor,
		PageSize:   req.PageSize,
	})
	if err != nil {
		l.Errorf("gateway/admin/collectorBatchList: source=%d state=%d mid=%d cursor=%q page_size=%d err=%v",
			req.Source, req.State, req.Mid, req.Cursor, req.PageSize, err)
		return nil, err
	}
	return &types.CollectorBatchListResponse{
		Code:    0,
		Message: "ok",
		Data: types.CollectorBatchListData{
			List:       collectorBatchListToAPI(reply.GetList()),
			NextCursor: reply.GetNextCursor(),
			HasMore:    reply.GetHasMore(),
			Total:      reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
