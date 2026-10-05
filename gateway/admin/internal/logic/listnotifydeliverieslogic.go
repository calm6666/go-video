// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	notificationrpc "go-video/services/notification/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListNotifyDeliveriesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询投递记录（mid/channel/state/biz_key/时间窗过滤）
func NewListNotifyDeliveriesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListNotifyDeliveriesLogic {
	return &ListNotifyDeliveriesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 投递记录分页查询：聚合 notification ListDeliveries RPC。
// 过滤条件全部可选，0/空串表示不过滤（服务侧把 0 当作「不加该条件」）；
// 时间窗只做区间合法性这种会误导运营的检查（空区间=零结果，不写进业务规则），
// 状态是否终态、是否会继续重试由 notification 的状态机决定，网关不推算。
func (l *ListNotifyDeliveriesLogic) ListNotifyDeliveries(req *types.ParamListNotifyDeliveries) (resp *types.NotifyDeliveriesResponse, err error) {
	if l.svcCtx.Notification == nil {
		return nil, errors.New("notification service not configured")
	}
	if req.StartCtime < 0 || req.EndCtime < 0 {
		return nil, errors.New("gateway/admin: start_ctime/end_ctime must be >= 0")
	}
	if req.StartCtime > 0 && req.EndCtime > 0 && req.StartCtime >= req.EndCtime {
		return nil, errors.New("gateway/admin: start_ctime must be earlier than end_ctime")
	}
	pn, ps := normalizeNotificationPage(req.Pn, req.Ps)
	reply, err := l.svcCtx.Notification.ListDeliveries(l.ctx, &notificationrpc.ListDeliveriesReq{
		Mid:        req.Mid,
		Channel:    notificationrpc.Channel(req.Channel),
		State:      notificationrpc.DeliveryState(req.State),
		BizKey:     req.BizKey,
		StartCtime: req.StartCtime,
		EndCtime:   req.EndCtime,
		Pn:         pn,
		Ps:         ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listNotifyDeliveries: mid=%d channel=%d state=%d biz_key=%s start_ctime=%d end_ctime=%d pn=%d ps=%d err=%v",
			req.Mid, req.Channel, req.State, req.BizKey, req.StartCtime, req.EndCtime, pn, ps, err)
		return nil, err
	}
	return &types.NotifyDeliveriesResponse{
		Code:    0,
		Message: "ok",
		Data: types.NotifyDeliveriesData{
			List:  notifyDeliveriesToAPI(reply.GetDeliveries()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
