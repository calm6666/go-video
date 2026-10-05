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

type OpsSetRolloutStateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 启停灰度规则（软状态切换，保留放量证据）
func NewOpsSetRolloutStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpsSetRolloutStateLogic {
	return &OpsSetRolloutStateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OpsSetRolloutStateLogic) OpsSetRolloutState(req *types.ParamOpsSetRolloutState) (resp *types.OpsRolloutRuleResponse, err error) {
	if l.svcCtx.OpsConfig == nil {
		return nil, errOpsServiceNotConfigured
	}
	callCtx, err := opsCallContext(l.ctx, req.Ctx, true)
	if err != nil {
		return nil, err
	}
	if req.RuleId <= 0 {
		return nil, errors.New("gateway/admin: rule_id must be > 0")
	}
	// 只接受 1 生效 / 2 停用：其它值在 ops-config 里没有对应状态，
	// 透传过去只会得到一次无意义往返，而「启停放量」这种动作不适合靠下游报错来兜。
	if req.State != 1 && req.State != 2 {
		return nil, errors.New("gateway/admin: state must be 1(enabled) or 2(disabled)")
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.OpsConfig.SetRolloutRuleState(l.ctx, &opsconfigrpc.SetRolloutRuleStateReq{
		Ctx:    callCtx,
		RuleId: req.RuleId,
		State:  req.State,
		Reason: req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/opsSetRolloutState: operator=%d request_id=%s rule_id=%d state=%d err=%v",
			callCtx.GetOperatorId(), callCtx.GetRequestId(), req.RuleId, req.State, err)
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
