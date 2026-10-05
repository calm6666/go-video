// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	riskcontrolrpc "go-video/services/risk-control/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RiskReportLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 调试用行为上报（写滑窗计数，event_id 幂等）
func NewRiskReportLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RiskReportLogic {
	return &RiskReportLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 行为上报调试：聚合 risk-control ReportAction RPC。
// event_id 是契约里的幂等键，后台补数据也必须带，否则重放会重复累加滑窗计数；
// count<=0 由服务端按 1 处理，网关不改写调用方传值（AGENTS.md §7：这不是埋点通道）。
func (l *RiskReportLogic) RiskReport(req *types.ParamRiskReport) (resp *types.RiskReportResponse, err error) {
	if l.svcCtx.RiskControl == nil {
		return nil, errors.New("risk-control service not configured")
	}
	action, err := riskGuardedAction(req.Action, false)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("event_id", req.EventId); err != nil {
		return nil, err
	}
	// 契约缺口：riskcontrol.v1.ReportActionReq 没有 operator 字段，
	// 后台补报的行为无法与运营账号关联，只能记在网关日志里。
	operatorID, err := adminOperatorID(l.ctx, "riskReport", req.OperatorId)
	if err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.RiskControl.ReportAction(l.ctx, &riskcontrolrpc.ReportActionReq{
		Mid:        req.Mid,
		Action:     action,
		DeviceId:   req.DeviceId,
		IpHash:     req.IpHash,
		Platform:   req.Platform,
		Count:      req.Count,
		OccurredAt: req.OccurredAt,
		EventId:    req.EventId,
		TraceId:    req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/riskReport: mid=%d action=%d event_id=%s operator_id=%d err=%v",
			req.Mid, req.Action, req.EventId, operatorID, err)
		return nil, err
	}
	return &types.RiskReportResponse{
		Code:    0,
		Message: "ok",
		Data: types.RiskReportData{
			Deduplicated:  reply.GetDeduplicated(),
			WindowSeconds: reply.GetWindowSeconds(),
			MidCount:      reply.GetMidCount(),
			DeviceCount:   reply.GetDeviceCount(),
			IpCount:       reply.GetIpCount(),
		},
		TTL: 0,
	}, nil
}
