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

type UpsertRiskListEntryLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新增/更新黑白名单条目
func NewUpsertRiskListEntryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertRiskListEntryLogic {
	return &UpsertRiskListEntryLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 名单写库：聚合 risk-control UpsertListEntry RPC。
// 黑白名单直接决定放行或拦截，target_value 的规范化（拒绝明文 IP、只收受控 hash）
// 由 risk-control 负责，网关只挡缺主体、缺幂等键与枚举越界（AGENTS.md §5 名单数据归风控）。
func (l *UpsertRiskListEntryLogic) UpsertRiskListEntry(req *types.ParamUpsertRiskListEntry) (resp *types.RiskUpsertListEntryResponse, err error) {
	if l.svcCtx.RiskControl == nil {
		return nil, errors.New("risk-control service not configured")
	}
	operatorID, err := adminOperatorID(l.ctx, "upsertRiskListEntry", req.OperatorId)
	if err != nil {
		return nil, err
	}
	// 契约缺口：riskcontrol.v1.UpsertListEntryReq.idempotency_key 服务端完全未使用
	// （upsertlistentrylogic 既不校验也不落库），重复提交会各自刷新 expire_at。
	// 网关仍要求非空：先把运营的重试批次留在入口日志里，等服务端补上落库再启用真幂等。
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("target_value", req.TargetValue); err != nil {
		return nil, err
	}
	listType, err := riskListType(req.ListType, false)
	if err != nil {
		return nil, err
	}
	targetType, err := riskTargetType(req.TargetType, false)
	if err != nil {
		return nil, err
	}
	if req.State != 0 && req.State != 1 {
		return nil, errors.New("gateway/admin: state must be 0(禁用)/1(启用)")
	}
	if req.DurationSeconds < 0 {
		return nil, errors.New("gateway/admin: duration_seconds must be >= 0 (0 = 永久)")
	}
	reply, err := l.svcCtx.RiskControl.UpsertListEntry(l.ctx, &riskcontrolrpc.UpsertListEntryReq{
		ListType:        listType,
		TargetType:      targetType,
		TargetValue:     req.TargetValue,
		Reason:          req.Reason,
		Operator:        operatorID,
		DurationSeconds: req.DurationSeconds,
		State:           req.State,
		IdempotencyKey:  req.IdempotencyKey,
	})
	if err != nil {
		// target_value 可能是设备/IP 的 hash 或他人账号，日志只记维度不记原文。
		l.Errorf("gateway/admin/upsertRiskListEntry: list_type=%d target_type=%d state=%d duration_seconds=%d operator_id=%d err=%v",
			req.ListType, req.TargetType, req.State, req.DurationSeconds, operatorID, err)
		return nil, err
	}
	return &types.RiskUpsertListEntryResponse{
		Code:    0,
		Message: "ok",
		Data: types.RiskUpsertListEntryData{
			Entry:   riskListEntryToAPI(reply.GetEntry()),
			Created: reply.GetCreated(),
		},
		TTL: 0,
	}, nil
}
