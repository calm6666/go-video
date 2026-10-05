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

type OpenWebhookListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 回调端点列表（地址属外部主体凭证面；软删行永远不回）
func NewOpenWebhookListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpenWebhookListLogic {
	return &OpenWebhookListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OpenWebhookList 转发 open-platform ListWebhooks。
//
// 挂权限点的理由是两条独立的：url 决定事件数据去向（属凭证/外泄面），且本方法要求
// operator_mid>0 才能把「运营在读」表达出来（mid==0 在服务侧是 owner 自查、归属由网关校验）。
// 台账只能按应用读——契约不提供跨应用枚举（列表里没有 owner/运营分支字段可注入归属），
// 因此 app_id 必填，网关不去凑一个「全部应用的端点」查询。
// 软删端点永远不回（它们只在投递台账里解释归属），所以本方法也不是「已删配置」的查询面。
func (l *OpenWebhookListLogic) OpenWebhookList(req *types.ParamOpenWebhookList) (resp *types.OpenWebhookListResponse, err error) {
	if l.svcCtx.OpenPlatform == nil {
		return nil, errOpenPlatformNotConfigured
	}
	if req == nil {
		return nil, errOpenPlatformRequestMissing
	}
	if err := openOperatorGate(l.ctx, "openWebhookList", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := openIDGate("app_id", req.AppId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.OpenPlatform.ListWebhooks(l.ctx, &openplatformrpc.ListWebhooksReq{
		AppId:           req.AppId,
		IncludeDisabled: req.IncludeDisabled,
		OperatorMid:     req.OperatorMid,
		TraceId:         req.TraceId,
	})
	if err != nil {
		// url 属外部主体凭证面：日志只有应用、过滤位与主体，不带任何端点地址或条数明细。
		l.Errorf("gateway/admin/openWebhookList: app_id=%d include_disabled=%t operator_mid=%d err=%v",
			req.AppId, req.IncludeDisabled, req.OperatorMid, err)
		return nil, err
	}
	return &types.OpenWebhookListResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpenWebhookListData{
			List: openWebhookEndpointsToAPI(reply.GetList()),
		},
		TTL: 0,
	}, nil
}
