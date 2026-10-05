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

type OpenApplicationStateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 推进应用状态机（只有状态：资料改动在运营通道被服务直接拒）
func NewOpenApplicationStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpenApplicationStateLogic {
	return &OpenApplicationStateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OpenApplicationState 转发 open-platform UpdateApplication 的**运营通道**。
//
// 本方法刻意不把「改资料」和「推状态」合成一条通用更新路由：服务侧的通道分离是
// updateapplicationlogic 的核心门禁（运营通道见到 name/description/redirect_uris 任一非空
// 就回 ErrOwnerRequired，开发者通道见到 target_status!=0 同样拒），混在一个表单里等于
// 「运营顺手改了下线原因，把开发者的回调白名单也覆盖了」。因此这里三个资料字段一律不填，
// 也不提供清空语义（清空白名单会让用户再也走不完撤销流程，属需运营显式处置的动作）。
// 并发保护在服务侧：`WHERE app_id=? AND status=<刚读到的 from>` 的状态 CAS 加迁移表，
// 表单因此不带 expected_version——它在运营通道根本不参与判定，放上去是个假乐观锁位。
// is_operator 由网关置 true，operator_mid 是表单自报的处置主体（mid 空间缺口见 admin.api 段头）。
func (l *OpenApplicationStateLogic) OpenApplicationState(req *types.ParamOpenApplicationState) (resp *types.OpenApplicationStateResponse, err error) {
	if l.svcCtx.OpenPlatform == nil {
		return nil, errOpenPlatformNotConfigured
	}
	if req == nil {
		return nil, errOpenPlatformRequestMissing
	}
	if err := openIDGate("app_id", req.AppId); err != nil {
		return nil, err
	}
	if err := openPositive("target_status", req.TargetStatus); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := openOperatorGate(l.ctx, "openApplicationState", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.OpenPlatform.UpdateApplication(l.ctx, &openplatformrpc.UpdateApplicationReq{
		AppId:        req.AppId,
		TargetStatus: openplatformrpc.AppStatus(req.TargetStatus),
		OperatorMid:  req.OperatorMid,
		IsOperator:   true,
		Reason:       req.Reason,
		TraceId:      req.TraceId,
	})
	if err != nil {
		// reason 正文属处置依据，不进日志（可能含主体资料与工单内容）。
		l.Errorf("gateway/admin/openApplicationState: app_id=%d target_status=%d operator_mid=%d err=%v",
			req.AppId, req.TargetStatus, req.OperatorMid, err)
		return nil, err
	}
	return &types.OpenApplicationStateResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpenApplicationStateData{
			App:     openAppToAPI(reply.GetApp()),
			Changed: reply.GetChanged(),
		},
		TTL: 0,
	}, nil
}
