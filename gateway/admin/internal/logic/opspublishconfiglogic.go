// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	opsconfigrpc "go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type OpsPublishConfigLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 发布配置新版本（expect_version 乐观锁，可一并挂灰度规则，写 audit）
func NewOpsPublishConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpsPublishConfigLogic {
	return &OpsPublishConfigLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OpsPublishConfigLogic) OpsPublishConfig(req *types.ParamOpsPublishConfig) (resp *types.OpsPublishResponse, err error) {
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
	// reason 必填：ops-config 把它连同 operator/request_id 一起写进 audit，
	// 少了它「为什么改了线上配置」就只能靠猜。
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if req.ExpectVersion < 0 {
		return nil, errOpsExpectVersionInvalid
	}

	reply, err := l.svcCtx.OpsConfig.PublishConfig(l.ctx, &opsconfigrpc.PublishConfigReq{
		Ctx:           callCtx,
		CfgKey:        req.CfgKey,
		Scope:         req.Scope,
		ValueType:     opsconfigrpc.ConfigValueType(req.ValueType),
		Value:         req.Value,
		ExpectVersion: req.ExpectVersion,
		Reason:        req.Reason,
		Rollout:       opsRolloutRuleSpecsToRPC(req.Rollout),
	})
	if err != nil {
		l.Errorf("gateway/admin/opsPublishConfig: operator=%d request_id=%s cfg_key=%s expect_version=%d rollout=%d err=%v",
			callCtx.GetOperatorId(), callCtx.GetRequestId(), req.CfgKey, req.ExpectVersion, len(req.Rollout), err)
		return nil, err
	}
	return &types.OpsPublishResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpsPublishData{
			Item:         opsConfigItemToAPI(reply.GetItem()),
			Version:      opsConfigVersionToAPI(reply.GetVersion()),
			Rules:        opsRolloutRulesToAPI(reply.GetRules()),
			AuditEntryId: reply.GetAuditEntryId(),
			Reused:       reply.GetReused(),
		},
		TTL: 0,
	}, nil
}
