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

type OpsSaveSwitchLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新建/更新客户端开关（按 switch_key + platform upsert）
func NewOpsSaveSwitchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpsSaveSwitchLogic {
	return &OpsSaveSwitchLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OpsSaveSwitchLogic) OpsSaveSwitch(req *types.ParamOpsSaveSwitch) (resp *types.OpsSwitchResponse, err error) {
	if l.svcCtx.OpsConfig == nil {
		return nil, errOpsServiceNotConfigured
	}
	callCtx, err := opsCallContext(l.ctx, req.Ctx, true)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("switch_key", req.SwitchKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	// platform 必填：开关的 upsert 键是 (switch_key, platform)，0(UNSPECIFIED) 会让「哪个端的能力」失去意义。
	if req.Platform <= 0 {
		return nil, errors.New("gateway/admin: platform required")
	}
	if req.Enabled < 0 {
		return nil, errors.New("gateway/admin: enabled must be >= 0")
	}
	if req.ExpectVersion < 0 {
		return nil, errOpsExpectVersionInvalid
	}

	reply, err := l.svcCtx.OpsConfig.SaveClientSwitch(l.ctx, &opsconfigrpc.SaveClientSwitchReq{
		Ctx:           callCtx,
		SwitchId:      req.SwitchId,
		SwitchKey:     req.SwitchKey,
		Platform:      opsconfigrpc.ClientPlatform(req.Platform),
		MinVersion:    req.MinVersion,
		MaxVersion:    req.MaxVersion,
		Enabled:       req.Enabled,
		ConfigId:      req.ConfigId,
		Remark:        req.Remark,
		ExpectVersion: req.ExpectVersion,
		Reason:        req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/opsSaveSwitch: operator=%d request_id=%s switch_key=%s platform=%d enabled=%d err=%v",
			callCtx.GetOperatorId(), callCtx.GetRequestId(), req.SwitchKey, req.Platform, req.Enabled, err)
		return nil, err
	}
	return &types.OpsSwitchResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpsSwitchData{
			Switch:       opsClientSwitchToAPI(reply.GetSwitch()),
			AuditEntryId: reply.GetAuditEntryId(),
		},
		TTL: 0,
	}, nil
}
