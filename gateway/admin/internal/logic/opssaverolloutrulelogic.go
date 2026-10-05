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

type OpsSaveRolloutRuleLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新建/更新灰度规则（按 cfg_key+version+name upsert）
func NewOpsSaveRolloutRuleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpsSaveRolloutRuleLogic {
	return &OpsSaveRolloutRuleLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OpsSaveRolloutRuleLogic) OpsSaveRolloutRule(req *types.ParamOpsSaveRolloutRule) (resp *types.OpsRolloutRuleResponse, err error) {
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
	// rule.name 是 (config_id, version) 内的 upsert 幂等句柄，缺了就无从判断是新建还是改放量。
	if err := requireNonEmpty("rule.name", req.Rule.Name); err != nil {
		return nil, err
	}
	if req.Version <= 0 {
		return nil, errors.New("gateway/admin: version must be > 0")
	}
	if err := opsTimeWindow(req.Rule.StartAt, req.Rule.EndAt); err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.OpsConfig.SaveRolloutRule(l.ctx, &opsconfigrpc.SaveRolloutRuleReq{
		Ctx:     callCtx,
		CfgKey:  req.CfgKey,
		Scope:   req.Scope,
		Version: req.Version,
		Rule:    opsRolloutRuleSpecToRPC(req.Rule),
	})
	if err != nil {
		l.Errorf("gateway/admin/opsSaveRolloutRule: operator=%d request_id=%s cfg_key=%s version=%d rule=%s err=%v",
			callCtx.GetOperatorId(), callCtx.GetRequestId(), req.CfgKey, req.Version, req.Rule.Name, err)
		return nil, err
	}
	return &types.OpsRolloutRuleResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpsRolloutRuleData{
			Rule:         opsRolloutRuleToAPI(reply.GetRule()),
			AuditEntryId: reply.GetAuditEntryId(),
		},
		TTL: 0,
	}, nil
}
