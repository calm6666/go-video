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

type OpenWebhookDeleteLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除回调端点并抑制未投递任务（本域刻意没有新增/改地址入口）
func NewOpenWebhookDeleteLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpenWebhookDeleteLogic {
	return &OpenWebhookDeleteLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OpenWebhookDelete 转发 open-platform DeleteWebhook（软删 + 抑制未投递任务）。
//
// 本域刻意没有「注册端点」路由（段头口径 1：回调地址决定第三方数据去向，由运营代设有
// SSRF 与越权读取的应用侧后果），因此这里也不给「改地址」留口子——改地址等价于换数据去向，
// 只能由归属者重新注册。端点是否属于本应用由服务判（webhookEndpointOfApp），网关不预读列表
// 去核对归属，也不允许只给 endpoint_id（那样连台账都归不到应用上）。
// deleted=false 是合法结论（例如并发下已被别人删掉），服务照样回抑制条数，网关不伪装成失败。
func (l *OpenWebhookDeleteLogic) OpenWebhookDelete(req *types.ParamOpenWebhookDelete) (resp *types.OpenWebhookDeleteResponse, err error) {
	if l.svcCtx.OpenPlatform == nil {
		return nil, errOpenPlatformNotConfigured
	}
	if req == nil {
		return nil, errOpenPlatformRequestMissing
	}
	if err := openIDGate("app_id", req.AppId); err != nil {
		return nil, err
	}
	if err := openIDGate("endpoint_id", req.EndpointId); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := openOperatorGate(l.ctx, "openWebhookDelete", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.OpenPlatform.DeleteWebhook(l.ctx, &openplatformrpc.DeleteWebhookReq{
		AppId:       req.AppId,
		EndpointId:  req.EndpointId,
		OperatorMid: req.OperatorMid,
		IsOperator:  true,
		Reason:      req.Reason,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/openWebhookDelete: app_id=%d endpoint_id=%d operator_mid=%d err=%v",
			req.AppId, req.EndpointId, req.OperatorMid, err)
		return nil, err
	}
	return &types.OpenWebhookDeleteResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpenWebhookDeleteData{
			Deleted:              reply.GetDeleted(),
			DeliveriesSuppressed: reply.GetDeliveriesSuppressed(),
		},
		TTL: 0,
	}, nil
}
