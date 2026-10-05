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

type CollectorValidateSchemaLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单事件干跑校验：回缺失字段与归一化 event_type/topic，不落库不投递
func NewCollectorValidateSchemaLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CollectorValidateSchemaLogic {
	return &CollectorValidateSchemaLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CollectorValidateSchema 转发 event-collector ValidateEventSchema。
//
// 本路由刻意**不做数值门槛**（其它 collector 路由都挡负数）：干跑的价值就在于把
// 「客户端会怎么被判」复现出来，而 REJECT_INVALID_METRIC（position_ms 为负、
// result_index 越界）与 REJECT_TIME_IN_FUTURE / REJECT_EVENT_TOO_OLD（occurred_at 异常）
// 恰恰只有把越界值送进去才测得出来。网关在这里夹一次负数，等于替埋点方把要测的用例吃掉了。
// 同理不补 event_id / occurred_at / schema_version：网关补齐会让「漏字段」这条判定测不出来，
// 干跑通过而真跑被拒（服务侧 validateeventschemalogic 与采集路径共用 validateEvent）。
//
// source=0 是合法哨兵（服务按 SOURCE_CLIENT 判定），因此这里连非负门槛都不套 source；
// 越界的枚举值由 model.ValidSource 拒绝，网关不复算。
//
// 隐私（AGENTS.md §7）：ctx_device_id / ctx_ip 只作为本次请求的入参交给服务算哈希与 IP 段，
// 响应投影（CollectorSchemaValidateData）没有任何字段承载它们，网关也不回带
// 归一化后的 device_hash——那会把「给明文换摘要」变成这条免鉴权路由的副产品。
// 日志只打路由名与判定结论码，绝不打 payload、event_id 之外的主体标识。
func (l *CollectorValidateSchemaLogic) CollectorValidateSchema(req *types.ParamCollectorSchemaValidate) (resp *types.CollectorSchemaValidateResponse, err error) {
	if l.svcCtx.EventCollector == nil {
		return nil, errCollectorServiceNotConfigured
	}
	if req == nil {
		return nil, errCollectorRequestMissing
	}
	reply, err := l.svcCtx.EventCollector.ValidateEventSchema(l.ctx, &collectorrpc.ValidateEventSchemaReq{
		Source:  collectorrpc.Source(req.Source),
		Event:   collectorValidateEventForRPC(req),
		Context: collectorValidateContextForRPC(req),
	})
	if err != nil {
		l.Errorf("gateway/admin/collectorValidateSchema: event_id=%s event_type=%s source=%d err=%v",
			req.EventId, req.EventType, req.Source, err)
		return nil, err
	}
	valid := reply.GetValid()
	l.Infof("gateway/admin/collectorValidateSchema: valid=%t decision=%d reason=%d missing=%d",
		valid, reply.GetDecision(), reply.GetReason(), len(reply.GetMissingFields()))
	return &types.CollectorSchemaValidateResponse{
		Code:    0,
		Message: "ok",
		Data: types.CollectorSchemaValidateData{
			Valid:         valid,
			Decision:      int32(reply.GetDecision()),
			Reason:        int32(reply.GetReason()),
			MissingFields: collectorStrings(reply.GetMissingFields()),
			EventType:     reply.GetEventType(),
			Topic:         reply.GetTopic(),
			SchemaVersion: reply.GetSchemaVersion(),
			Note:          reply.GetNote(),
		},
		TTL: 0,
	}, nil
}
