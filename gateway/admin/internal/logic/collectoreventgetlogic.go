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

type CollectorEventGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 按 event_id 精读单条事件（投递状态以 Outbox 真值覆盖）
func NewCollectorEventGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CollectorEventGetLogic {
	return &CollectorEventGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CollectorEventGet 转发 event-collector GetEventRecord。
// event_id 是客户端生成、服务端去重的那一个：网关不改写、不补前缀，只 TrimSpace 判空。
// found=false 是结论不是错误（未被接收、或已过台账保留期）。
//
// 本方法在单条读上刻意 ttl=0 且不缓存：服务会用 ec_pending_delivery 的 Outbox 真值覆盖
// delivery_*（applyPendingTruth），缓存一层就等于把「刚投递成功」重新说回「在途」，
// 而排障时问的正是「这条到底出去了没有」。payload 只有 sha256 摘要与字节数，原文不入库也不出本面。
func (l *CollectorEventGetLogic) CollectorEventGet(req *types.ParamCollectorEventGet) (resp *types.CollectorEventResponse, err error) {
	if l.svcCtx.EventCollector == nil {
		return nil, errCollectorServiceNotConfigured
	}
	if req == nil {
		return nil, errCollectorRequestMissing
	}
	if _, ok := collectorTrim(req.EventId); !ok {
		return nil, errCollectorSubjectRequired
	}
	reply, err := l.svcCtx.EventCollector.GetEventRecord(l.ctx, &collectorrpc.GetEventRecordReq{
		EventId: req.EventId,
	})
	if err != nil {
		l.Errorf("gateway/admin/collectorEventGet: event_id=%s err=%v", req.EventId, err)
		return nil, err
	}
	return &types.CollectorEventResponse{
		Code:    0,
		Message: "ok",
		Data: types.CollectorEventData{
			Found:  reply.GetFound(),
			Record: collectorRecordToAPI(reply.GetRecord()),
		},
		TTL: 0,
	}, nil
}
