// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	opsconfigrpc "go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type OpsRollbackConfigLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 回滚到历史版本（生成新版本而不是删历史，change_type=rollback）
func NewOpsRollbackConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpsRollbackConfigLogic {
	return &OpsRollbackConfigLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OpsRollbackConfigLogic) OpsRollbackConfig(req *types.ParamOpsRollbackConfig) (resp *types.OpsRollbackResponse, err error) {
	if l.svcCtx.OpsConfig == nil {
		return nil, errOpsServiceNotConfigured
	}
	callCtx, err := opsCallContext(l.ctx, req.Ctx, true)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("cfg_key", req.CfgKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if req.ToVersion <= 0 {
		return nil, errors.New("gateway/admin: to_version must be > 0")
	}

	reply, err := l.svcCtx.OpsConfig.RollbackConfig(l.ctx, &opsconfigrpc.RollbackConfigReq{
		Ctx:       callCtx,
		CfgKey:    req.CfgKey,
		Scope:     req.Scope,
		ToVersion: req.ToVersion,
		Reason:    req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/opsRollbackConfig: operator=%d request_id=%s cfg_key=%s to_version=%d err=%v",
			callCtx.GetOperatorId(), callCtx.GetRequestId(), req.CfgKey, req.ToVersion, err)
		return nil, err
	}
	return &types.OpsRollbackResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpsRollbackData{
			Item:         opsConfigItemToAPI(reply.GetItem()),
			Version:      opsConfigVersionToAPI(reply.GetVersion()),
			AuditEntryId: reply.GetAuditEntryId(),
		},
		TTL: 0,
	}, nil
}
