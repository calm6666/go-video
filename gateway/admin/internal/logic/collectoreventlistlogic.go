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

type CollectorEventListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 事件台账游标翻页（校验结论与投递状态是两个独立维度）
func NewCollectorEventListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CollectorEventListLogic {
	return &CollectorEventListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CollectorEventList 转发 event-collector ListEventRecords。
// decision / reason / delivery_state 三个维度原样透传、互不折算：
// 「事件被拒」（校验阶段）与「事件投递失败」（投递阶段）是两件不同的事，
// 网关替调用方把一个映射成另一个，后台就会拿一个错误的结论去改埋点或改 MQ 配置。
// 0 一律是「不过滤」（UNSPECIFIED），负数没有语义故先拒；枚举取值是否合法、
// page_size 上限、cursor 语法与无界扫描拒绝都由服务判定（reads.go）。
func (l *CollectorEventListLogic) CollectorEventList(req *types.ParamCollectorEventList) (resp *types.CollectorEventListResponse, err error) {
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
	for _, v := range []struct {
		field string
		num   int64
	}{
		{"category", int64(req.Category)},
		{"decision", int64(req.Decision)},
		{"reason", int64(req.Reason)},
		{"delivery_state", int64(req.DeliveryState)},
	} {
		if err := collectorNonNeg(v.field, v.num); err != nil {
			return nil, err
		}
	}
	reply, err := l.svcCtx.EventCollector.ListEventRecords(l.ctx, &collectorrpc.ListEventRecordsReq{
		BatchId:       req.BatchId,
		EventType:     req.EventType,
		Category:      collectorrpc.BehaviorCategory(req.Category),
		Decision:      collectorrpc.EventDecision(req.Decision),
		Reason:        collectorrpc.RejectReason(req.Reason),
		DeliveryState: collectorrpc.DeliveryState(req.DeliveryState),
		Topic:         req.Topic,
		Mid:           req.Mid,
		DeviceHash:    req.DeviceHash,
		CtimeFrom:     req.CtimeFrom,
		CtimeTo:       req.CtimeTo,
		Cursor:        req.Cursor,
		PageSize:      req.PageSize,
	})
	if err != nil {
		l.Errorf("gateway/admin/collectorEventList: batch_id=%s event_type=%s decision=%d delivery_state=%d cursor=%q err=%v",
			req.BatchId, req.EventType, req.Decision, req.DeliveryState, req.Cursor, err)
		return nil, err
	}
	return &types.CollectorEventListResponse{
		Code:    0,
		Message: "ok",
		Data: types.CollectorEventListData{
			List:       collectorRecordListToAPI(reply.GetList()),
			NextCursor: reply.GetNextCursor(),
			HasMore:    reply.GetHasMore(),
			Total:      reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
