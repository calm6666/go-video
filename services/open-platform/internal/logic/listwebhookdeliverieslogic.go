package logic

import (
	"context"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListWebhookDeliveriesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListWebhookDeliveriesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListWebhookDeliveriesLogic {
	return &ListWebhookDeliveriesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 投递记录分页（观测与排障）。
//
// 本方法是纯读：不 Claim、不重放、不改任何状态，也不读 worker 内存队列——
// 响应里的 state/next_retry_at 一律是 DB 真值（proto:604-618 的语义就是「台账」）。
//
// 最重要的一条：不回正文。model.WebhookDeliveryModel.ListByCursor 的 SELECT 列表里
// 根本没有 payload 列（helpers.projectWebhookDelivery 也只有 payload_digest 的位置），
// 因此「批量把投递正文读进内存」在本方法里做不到而不是不该做。
// 单条正文只在运维审计链路按 delivery_id 取，不通过本 RPC。
//
// 错误映射：ErrInvalidAppID/ErrAppNotFound/ErrWebhookNotFound/errInvalidDeliveryStateFilter/
// ErrInvalidCursor/ErrPsTooLarge/ErrInvalidPage→InvalidArgument；SQL 失败→Internal。
func (l *ListWebhookDeliveriesLogic) ListWebhookDeliveries(in *rpc.ListWebhookDeliveriesReq) (*rpc.ListWebhookDeliveriesReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 范围：投递台账永远是「某个应用」的，本方法不提供跨应用枚举。
	//    operator_mid==0 是 owner 自查（归属由网关校验），>0 是运营，负数是脏参数；
	//    两者读到的行集相同，区别只进日志。
	app, err := findApp(ctx, s, in.AppId)
	if err != nil {
		return nil, err
	}
	if in.OperatorMid < 0 {
		return nil, model.ErrOperatorRequired
	}

	// 2. 端点过滤器：>0 时必须属于本应用，否则等于用别人的 endpoint_id 交叉查询。
	//    这里刻意不排斥软删端点——端点删了，它的投递记录仍要列得出来（死信复盘要归属）。
	var endpointID int64
	if in.EndpointId != 0 {
		ep, err := webhookEndpointOfApp(ctx, s, app.AppID, in.EndpointId)
		if err != nil {
			return nil, err
		}
		endpointID = ep.EndpointID
	}

	// 3. 状态过滤器：UNSPECIFIED(0) 表示不过滤；非 0 必须是已定义状态，
	//    未知值绝不当成「不过滤」（那会让运营以为看到了全部，实际看到的是另一个集合）。
	state := int32(in.State)
	if state != 0 && !model.ValidDeliveryState(state) {
		return nil, errInvalidDeliveryStateFilter
	}

	// 4. 分页：ps 走 PageSize/MaxPageSize 归一（超限直接拒，不静默裁剪）；
	//    游标是 (ctime, delivery_id) 倒序位点，与 ListByCursor 的 ORDER BY 完全一致；
	//    多取一条判 has_more。
	ps, err := pageSize(s, in.Ps)
	if err != nil {
		return nil, err
	}
	cursorTime, cursorID, err := decodeCursor(in.Cursor)
	if err != nil {
		return nil, err
	}
	rows, err := s.WebhookDeliveries.ListByCursor(ctx, app.AppID, endpointID, state, false,
		cursorTime, cursorID, ps+1)
	if err != nil {
		return nil, err
	}

	page, next, hasMore := trimPage(rows, ps, func(d *model.WebhookDelivery) (int64, int64) {
		return d.Ctime, d.DeliveryID
	})
	list := make([]*rpc.WebhookDeliveryInfo, 0, len(page))
	for _, d := range page {
		if info := projectWebhookDelivery(d); info != nil {
			// last_error 入库前已截断到 200 字节并脱敏（model.maxDeliveryErrLen），原样透传即可。
			list = append(list, info)
		}
	}
	logx.WithContext(ctx).Infof("open-platform: 投递记录分页 app_id=%d endpoint_id=%d state=%d ps=%d "+
		"operator_mid=%d n=%d has_more=%t", app.AppID, endpointID, state, ps, in.OperatorMid, len(list), hasMore)

	return &rpc.ListWebhookDeliveriesReply{List: list, NextCursor: next, HasMore: hasMore}, nil
}
