// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	openplatformrpc "go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type OpenWebhookDeliveryListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 投递流水分页（只有 payload_digest 与脱敏错误；payload 正文不经本 RPC 外发）
func NewOpenWebhookDeliveryListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpenWebhookDeliveryListLogic {
	return &OpenWebhookDeliveryListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OpenWebhookDeliveryList 转发 open-platform ListWebhookDeliveries（排障：这条事件为什么没送到）。
//
// 台账只能按应用读，服务不提供跨应用枚举，所以 app_id 必填；endpoint_id=0 = 该应用全部端点，
// 非 0 时服务会核对它确实属于本应用（防用别人的端点号交叉查询），网关不代读列表去预校验。
// state=0 = 不按状态过滤，而未知取值服务直接拒（不当成「不过滤」，否则运营以为看到了全部）；
// 哪一态算合法由 model.ValidDeliveryState 判。响应里回 attempt/max_attempts/next_retry_at：
// 重放前要先看得懂「为什么这条被判死信」，一列都不裁。
func (l *OpenWebhookDeliveryListLogic) OpenWebhookDeliveryList(req *types.ParamOpenWebhookDeliveryList) (resp *types.OpenWebhookDeliveryListResponse, err error) {
	if l.svcCtx.OpenPlatform == nil {
		return nil, errOpenPlatformNotConfigured
	}
	if req == nil {
		return nil, errOpenPlatformRequestMissing
	}
	if err := openOperatorGate(l.ctx, "openWebhookDeliveryList", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := openIDGate("app_id", req.AppId); err != nil {
		return nil, err
	}
	if err := openNonNeg("endpoint_id", req.EndpointId); err != nil {
		return nil, err
	}
	if err := openNonNeg("state", int64(req.State)); err != nil {
		return nil, err
	}
	if err := openNonNeg("ps", int64(req.Ps)); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.OpenPlatform.ListWebhookDeliveries(l.ctx, &openplatformrpc.ListWebhookDeliveriesReq{
		AppId:       req.AppId,
		EndpointId:  req.EndpointId,
		State:       openplatformrpc.WebhookDeliveryState(req.State),
		Cursor:      req.Cursor,
		Ps:          req.Ps,
		OperatorMid: req.OperatorMid,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/openWebhookDeliveryList: app_id=%d endpoint_id=%d state=%d operator_mid=%d err=%v",
			req.AppId, req.EndpointId, req.State, req.OperatorMid, err)
		return nil, err
	}
	return &types.OpenWebhookDeliveryListResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpenWebhookDeliveryListData{
			List:       openWebhookDeliveriesToAPI(reply.GetList()),
			NextCursor: reply.GetNextCursor(),
			HasMore:    reply.GetHasMore(),
		},
		TTL: 0,
	}, nil
}
