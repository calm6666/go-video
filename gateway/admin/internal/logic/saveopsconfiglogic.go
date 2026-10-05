// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	operationrpc "go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SaveOpsConfigLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 写入运营配置（expect_version 乐观锁，冲突需重新拉取）
func NewSaveOpsConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SaveOpsConfigLogic {
	return &SaveOpsConfigLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 写入运营配置：cfg_value 与 value_type 是否匹配、scope 合法性、版本冲突
// 都由 operation 判定（ErrConfigValueInvalid / ErrConfigVersionConflict）。
// 网关不做静默覆盖，也不在这里补默认值：expect_version=0 就是「新建」语义。
func (l *SaveOpsConfigLogic) SaveOpsConfig(req *types.ParamSaveOpsConfig) (resp *types.OperationSaveConfigResponse, err error) {
	if l.svcCtx.Operation == nil {
		return nil, errors.New("operation service not configured")
	}
	opCtx, err := operationOpContext(l.ctx, req.Op, true)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("cfg_key", req.CfgKey); err != nil {
		return nil, err
	}
	if req.ExpectVersion < 0 {
		return nil, errors.New("gateway/admin: expect_version must be >= 0")
	}

	reply, err := l.svcCtx.Operation.SaveOpsConfig(l.ctx, &operationrpc.SaveOpsConfigReq{
		Ctx:           opCtx,
		CfgKey:        req.CfgKey,
		CfgValue:      req.CfgValue,
		ValueType:     req.ValueType,
		Scope:         req.Scope,
		ExpectVersion: req.ExpectVersion,
		State:         req.State,
		Remark:        req.Remark,
	})
	if err != nil {
		// cfg_value 不落日志：运营配置可能承载密钥引用或名单，只记键与版本。
		l.Errorf("gateway/admin/saveOpsConfig: operator=%d cfg_key=%q scope=%q expect_version=%d err=%v",
			opCtx.GetOperatorId(), req.CfgKey, req.Scope, req.ExpectVersion, err)
		return nil, err
	}
	l.Infof("gateway/admin/saveOpsConfig: operator=%d cfg_key=%s version=%d request_id=%s",
		opCtx.GetOperatorId(), reply.GetConfig().GetCfgKey(), reply.GetConfig().GetVersion(), opCtx.GetRequestId())
	return &types.OperationSaveConfigResponse{
		Code:    0,
		Message: "ok",
		Data:    types.OperationSaveConfigData{Config: configToAPI(reply.GetConfig())},
		TTL:     0,
	}, nil
}
