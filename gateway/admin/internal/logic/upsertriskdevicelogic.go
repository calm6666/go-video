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

type UpsertRiskDeviceLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 写入/更新设备画像与设备-账号关联
func NewUpsertRiskDeviceLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertRiskDeviceLogic {
	return &UpsertRiskDeviceLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 设备画像写入：聚合 risk-control UpsertDeviceProfile RPC。
// source 必须由后台显式声明（人工改标签走 operation，服务端会二次校验 operator）；
// risk_score 不做默认值兜底：漏传会被服务端当成写 0，所以网关要求显式传 -1 表示不修改。
func (l *UpsertRiskDeviceLogic) UpsertRiskDevice(req *types.ParamUpsertRiskDevice) (resp *types.RiskUpsertDeviceResponse, err error) {
	if l.svcCtx.RiskControl == nil {
		return nil, errors.New("risk-control service not configured")
	}
	operatorID, err := adminOperatorID(l.ctx, "upsertRiskDevice", req.OperatorId)
	if err != nil {
		return nil, err
	}
	// 契约缺口：riskcontrol.v1.UpsertDeviceProfileReq.idempotency_key 在服务端只到
	// internal/logic 就被丢弃（未透传进 repository），实际不参与去重。
	// 网关仍要求非空：画像写入是运营人工动作，先把主体与批次留痕，等服务端补上落库再启用真幂等。
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("source", req.Source); err != nil {
		return nil, err
	}
	if req.DeviceId == "" && req.DeviceHash == "" {
		return nil, errors.New("gateway/admin: device_id or device_hash required")
	}
	if req.RiskScore < -1 {
		return nil, errors.New("gateway/admin: risk_score must be -1 (不修改) or 0..100")
	}
	reply, err := l.svcCtx.RiskControl.UpsertDeviceProfile(l.ctx, &riskcontrolrpc.UpsertDeviceProfileReq{
		DeviceId:       req.DeviceId,
		DeviceHash:     req.DeviceHash,
		Labels:         req.Labels,
		RiskScore:      req.RiskScore,
		Mid:            req.Mid,
		Source:         req.Source,
		Operator:       operatorID,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		// 只记脱敏维度：设备号原文与 labels 内容不进日志。
		l.Errorf("gateway/admin/upsertRiskDevice: has_device_id=%v mid=%d source=%s operator_id=%d err=%v",
			req.DeviceId != "", req.Mid, req.Source, operatorID, err)
		return nil, err
	}
	return &types.RiskUpsertDeviceResponse{
		Code:    0,
		Message: "ok",
		Data: types.RiskUpsertDeviceData{
			Profile:       riskDeviceProfileToAPI(reply.GetProfile()),
			Created:       reply.GetCreated(),
			RelationAdded: reply.GetRelationAdded(),
		},
		TTL: 0,
	}, nil
}
