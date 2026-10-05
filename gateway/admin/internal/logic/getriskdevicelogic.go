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

type GetRiskDeviceLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询设备画像（只回传 device_hash）
func NewGetRiskDeviceLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRiskDeviceLogic {
	return &GetRiskDeviceLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 设备画像查询：聚合 risk-control GetDeviceProfile RPC。
// device_id 原文只在一次请求内存在，响应只回 hash；画像不存在时 found=false，
// 网关不伪造空画像、也不把「没登记过」当错误（AGENTS.md §6）。
func (l *GetRiskDeviceLogic) GetRiskDevice(req *types.ParamRiskDevice) (resp *types.RiskDeviceProfileResponse, err error) {
	if l.svcCtx.RiskControl == nil {
		return nil, errors.New("risk-control service not configured")
	}
	if err := requireOperatorID(req.OperatorId); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("device_id", req.DeviceId); err != nil {
		return nil, err
	}
	// 契约缺口：riskcontrol.v1.GetDeviceProfileReq 没有 operator 字段，
	// 排障查询人只能记在网关日志里；日志不落 device_id 原文，只落 hash 是否存在。
	reply, err := l.svcCtx.RiskControl.GetDeviceProfile(l.ctx, &riskcontrolrpc.GetDeviceProfileReq{
		DeviceId:   req.DeviceId,
		DeviceHash: req.DeviceHash,
	})
	if err != nil {
		l.Errorf("gateway/admin/getRiskDevice: use_device_id=%s has_device_hash=%v operator_id=%d err=%v",
			req.DeviceId != "", req.DeviceHash != "", req.OperatorId, err)
		return nil, err
	}
	return &types.RiskDeviceProfileResponse{
		Code:    0,
		Message: "ok",
		Data: types.RiskDeviceProfileData{
			Profile: riskDeviceProfileToAPI(reply.GetProfile()),
			Found:   reply.GetFound(),
		},
		TTL: 0,
	}, nil
}
